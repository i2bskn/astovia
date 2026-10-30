package gmailsync

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/i2bskn/astovia/internal/ai"
	"github.com/i2bskn/astovia/internal/config"
	"github.com/i2bskn/astovia/internal/google"
	"github.com/i2bskn/astovia/internal/secretbox"
	"github.com/i2bskn/astovia/internal/store"
	"github.com/i2bskn/astovia/internal/testutil"
)

const (
	me        = "alice@example.com"
	refreshTk = "1//refresh-secret-0001"
	aiKey     = "sk-ant-test-key-0123456789"
)

// gMsg is a message of the fake Gmail.
type gMsg struct {
	ID, Thread string
	Labels     []string
	At         time.Time
	From       string
	To, Cc     string
	Subject    string
	Body       string
	HTML       string
	Headers    map[string]string
}

// fakeGoogle is an in-memory Google: the token endpoint and the Gmail API.
// statuses forces a status for a request kind ("token", "list",
// "threads" or "threads:{id}"), with bodies[kind] as the body. unauth
// answers that many Gmail requests with 401. hooks run before a request of
// a kind is answered.
type fakeGoogle struct {
	mu        sync.Mutex
	msgs      []*gMsg
	refresh   string
	rotate    string
	access    string
	expiresIn int
	issued    int
	statuses  map[string]int
	bodies    map[string]string
	headers   map[string]string
	unauth    int
	pageSize  int
	nextID    int64
	hooks     map[string]func()
	requests  []string
	queries   []string
}

func newFakeGoogle() *fakeGoogle {
	return &fakeGoogle{refresh: refreshTk, expiresIn: 3600, statuses: map[string]int{}, bodies: map[string]string{}, headers: map[string]string{}, pageSize: 100, nextID: 0x18a00000, hooks: map[string]func(){}}
}

func (f *fakeGoogle) set(fn func(f *fakeGoogle)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

// count returns the number of requests of a kind ("" for all; "threads"
// for every thread).
func (f *fakeGoogle) count(kind string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.requests {
		if kind == "" || r == kind || (kind == "threads" && strings.HasPrefix(r, "threads:")) {
			n++
		}
	}
	return n
}

func (f *fakeGoogle) reset0() { f.set(func(f *fakeGoogle) { f.requests, f.queries = nil, nil }) }

// add stores a message and returns its ID. A message without a thread
// starts its own; one without labels is in the inbox.
func (f *fakeGoogle) add(m gMsg) string {
	var id string
	f.set(func(f *fakeGoogle) {
		f.nextID++
		id = strconv.FormatInt(f.nextID, 16)
		m.ID = id
		if m.Thread == "" {
			m.Thread = id
		}
		if m.Labels == nil {
			m.Labels = []string{"INBOX", "UNREAD"}
		}
		f.msgs = append(f.msgs, &m)
	})
	return id
}

func (f *fakeGoogle) setLabels(id string, labels ...string) {
	f.set(func(f *fakeGoogle) {
		for _, m := range f.msgs {
			if m.ID == id {
				m.Labels = labels
			}
		}
	})
}

func has(labels []string, l string) bool {
	for _, v := range labels {
		if v == l {
			return true
		}
	}
	return false
}

var afterPattern = regexp.MustCompile(`after:([0-9]+)`)

func (f *fakeGoogle) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	form, _ := url.ParseQuery(string(raw))
	kind := ""
	switch {
	case r.URL.Path == "/token":
		kind = "token"
	case r.URL.Path == "/gmail/messages":
		kind = "list"
	case strings.HasPrefix(r.URL.Path, "/gmail/threads/"):
		kind = "threads:" + strings.TrimPrefix(r.URL.Path, "/gmail/threads/")
	}
	f.mu.Lock()
	f.requests = append(f.requests, kind)
	if kind == "list" {
		f.queries = append(f.queries, r.URL.Query().Get("q"))
	}
	hook := f.hooks[kind]
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	write := func(status int, v any) {
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	forced := func(kinds ...string) bool {
		for _, k := range kinds {
			if code, ok := f.statuses[k]; ok {
				for hk, hv := range f.headers {
					w.Header().Set(hk, hv)
				}
				w.WriteHeader(code)
				_, _ = w.Write([]byte(f.bodies[k]))
				return true
			}
		}
		return false
	}
	if kind == "token" {
		if forced("token") {
			return
		}
		if form.Get("grant_type") != "refresh_token" || form.Get("client_id") != "client-id" || form.Get("client_secret") != "client-secret" {
			write(401, map[string]string{"error": "invalid_client"})
			return
		}
		if form.Get("refresh_token") != f.refresh {
			write(400, map[string]string{"error": "invalid_grant"})
			return
		}
		f.issued++
		f.access = fmt.Sprintf("ya29.access-%d", f.issued)
		out := map[string]any{"access_token": f.access, "expires_in": f.expiresIn, "scope": google.ScopeGmailReadonly, "token_type": "Bearer"}
		if f.rotate != "" {
			out["refresh_token"] = f.rotate
			f.refresh, f.rotate = f.rotate, ""
		}
		write(200, out)
		return
	}
	if f.unauth > 0 || r.Header.Get("Authorization") != "Bearer "+f.access || f.access == "" {
		if f.unauth > 0 {
			f.unauth--
		}
		write(401, map[string]any{"error": map[string]any{"code": 401, "status": "UNAUTHENTICATED"}})
		return
	}
	switch {
	case kind == "list":
		if forced("list") {
			return
		}
		q := r.URL.Query()
		after := int64(0)
		if m := afterPattern.FindStringSubmatch(q.Get("q")); m != nil {
			after, _ = strconv.ParseInt(m[1], 10, 64)
		}
		var list []*gMsg
		for _, m := range f.msgs {
			if has(m.Labels, "INBOX") && !has(m.Labels, "DRAFT") && m.At.Unix() > after {
				list = append(list, m)
			}
		}
		sort.SliceStable(list, func(i, j int) bool { return list[i].At.After(list[j].At) })
		offset, _ := strconv.Atoi(q.Get("pageToken"))
		end := min(len(list), offset+f.pageSize)
		out := map[string]any{"resultSizeEstimate": len(list)}
		var refs []map[string]string
		for _, m := range list[min(offset, len(list)):end] {
			refs = append(refs, map[string]string{"id": m.ID, "threadId": m.Thread})
		}
		if refs != nil {
			out["messages"] = refs
		}
		if end < len(list) {
			out["nextPageToken"] = strconv.Itoa(end)
		}
		write(200, out)
	case strings.HasPrefix(kind, "threads:"):
		if forced(kind, "threads") {
			return
		}
		id := strings.TrimPrefix(kind, "threads:")
		var msgs []map[string]any
		for _, m := range f.msgs {
			if m.Thread == id {
				msgs = append(msgs, m.json())
			}
		}
		if msgs == nil {
			write(404, map[string]any{"error": map[string]any{"code": 404}})
			return
		}
		write(200, map[string]any{"id": id, "messages": msgs})
	default:
		write(404, map[string]any{})
	}
}

func (m *gMsg) json() map[string]any {
	headers := []map[string]string{{"name": "From", "value": m.From}, {"name": "To", "value": m.To}, {"name": "Subject", "value": m.Subject}}
	if m.Cc != "" {
		headers = append(headers, map[string]string{"name": "Cc", "value": m.Cc})
	}
	for k, v := range m.Headers {
		headers = append(headers, map[string]string{"name": k, "value": v})
	}
	mime, data := "text/plain", m.Body
	if m.HTML != "" {
		mime, data = "text/html", m.HTML
	}
	return map[string]any{"id": m.ID, "threadId": m.Thread, "labelIds": m.Labels, "internalDate": strconv.FormatInt(m.At.UnixMilli(), 10),
		"payload": map[string]any{"mimeType": mime, "headers": headers, "body": map[string]any{"data": base64.URLEncoding.EncodeToString([]byte(data)), "size": len(data)}}}
}

// fakeAI is an Anthropic Messages API answering with replies in order (500
// when they run out).
type fakeAI struct {
	mu      sync.Mutex
	replies []func(w http.ResponseWriter)
	bodies  []string
}

func (f *fakeAI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.bodies = append(f.bodies, string(body))
	n := len(f.bodies)
	var reply func(http.ResponseWriter)
	if n <= len(f.replies) {
		reply = f.replies[n-1]
	}
	f.mu.Unlock()
	if reply == nil {
		reply = aiError(500)
	}
	reply(w)
}

func (f *fakeAI) set(replies ...func(http.ResponseWriter)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replies, f.bodies = replies, nil
}

func (f *fakeAI) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.bodies)
}

// input returns the JSON document of the n-th request's user message, with
// the system prompt as "_system".
func (f *fakeAI) input(t *testing.T, n int) map[string]any {
	t.Helper()
	f.mu.Lock()
	raw := f.bodies[n-1]
	f.mu.Unlock()
	var body struct {
		System []struct {
			Text string `json:"text"`
		} `json:"system"`
		Messages []struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatal(err)
	}
	_, doc, _ := strings.Cut(body.Messages[0].Content[0].Text, "\n")
	var out map[string]any
	if err := json.Unmarshal([]byte(doc), &out); err != nil {
		t.Fatal(err)
	}
	out["_system"] = body.System[0].Text
	return out
}

// posts returns the posts of the n-th request.
func (f *fakeAI) posts(t *testing.T, n int) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, p := range f.input(t, n)["posts"].([]any) {
		out = append(out, p.(map[string]any))
	}
	return out
}

func aiText(v map[string]any) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		out, _ := json.Marshal(v)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "msg", "type": "message", "role": "assistant", "model": "claude-haiku-5-5",
			"content": []map[string]any{{"type": "text", "text": string(out)}}, "stop_reason": "end_turn", "usage": map[string]int{"input_tokens": 1, "output_tokens": 1}})
	}
}

func aiAction(task string) func(http.ResponseWriter) {
	return aiJudged(ai.DecisionTask, task, "s", "", "", "")
}

func aiCandidate(task string) func(http.ResponseWriter) {
	return aiJudged(ai.DecisionCandidate, task, "s", "", "", "")
}

func aiNoAction(summary string) func(http.ResponseWriter) {
	return aiJudged(ai.DecisionNone, "", summary, "", "", "")
}

func aiJudged(decision, task, summary, eventTitle, eventStart, eventEnd string) func(http.ResponseWriter) {
	return aiJudgedImportance(ai.ImportanceNormal, decision, task, summary, eventTitle, eventStart, eventEnd)
}

// aiJudgedImportance is aiJudged with the given importance.
func aiJudgedImportance(importance, decision, task, summary, eventTitle, eventStart, eventEnd string) func(http.ResponseWriter) {
	return aiText(map[string]any{"decision": decision, "task": task, "summary": summary, "event_title": eventTitle, "event_start": eventStart, "event_end": eventEnd, "topic": "", "same_as": "", "importance": importance})
}

func aiGrouped(task, topic, sameAs string) func(http.ResponseWriter) {
	return aiText(map[string]any{"decision": ai.DecisionTask, "task": task, "summary": "s", "event_title": "", "event_start": "", "event_end": "", "topic": topic, "same_as": sameAs, "importance": "normal"})
}

func aiResolved(v bool) func(http.ResponseWriter) {
	return aiText(map[string]any{"resolved": v, "reason": "secret reason"})
}

func aiError(code int) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"error","message":"forced"}}`))
	}
}

type syncTest struct {
	t        *testing.T
	s        *store.Store
	g        *fakeGoogle
	ai       *fakeAI
	syncer   *Syncer
	now      time.Time
	accounts map[string]string
}

func drivers(t *testing.T, fn func(t *testing.T, driver string)) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) { fn(t, driver) })
	}
}

// newSyncTest connects alice@example.com with the AI configured and runs
// the first synchronization on an empty inbox, so that the window has
// started; then it moves 10 minutes on.
func newSyncTest(t *testing.T, driver string) *syncTest {
	t.Helper()
	st := newBareSyncTest(t, driver)
	st.saveAI()
	st.sync(me)
	st.g.reset0()
	st.advance(10 * time.Minute)
	return st
}

// newBareSyncTest connects alice@example.com without AI settings and
// without syncing.
func newBareSyncTest(t *testing.T, driver string) *syncTest {
	t.Helper()
	ctx := context.Background()
	s, _ := testutil.Database(t, driver)
	st := &syncTest{t: t, s: s, now: time.Date(2026, 10, 24, 1, 0, 0, 0, time.UTC), accounts: map[string]string{}}
	u := store.User{ID: "alice-id", Email: "alice@astovia.test", PasswordHash: "x", Role: store.RoleOwner}
	if err := s.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	if err := s.Login(ctx, u, store.Session{ID: "sid", UserID: u.ID, ExpiresAt: st.now.Unix() + 86400*30, CreatedAt: st.now.Unix()}, store.Refresh{Hash: "r", SessionID: "sid", CreatedAt: st.now.Unix()}); err != nil {
		t.Fatal(err)
	}
	st.g = newFakeGoogle()
	srv := httptest.NewServer(st.g)
	t.Cleanup(srv.Close)
	client := google.New(func() time.Time { return st.now })
	client.TokenEndpoint, client.RevokeEndpoint, client.GmailBaseURL = srv.URL+"/token", srv.URL+"/revoke", srv.URL+"/gmail"
	st.ai = &fakeAI{}
	aiSrv := httptest.NewServer(st.ai)
	t.Cleanup(aiSrv.Close)
	aiClient := ai.New()
	aiClient.BaseURL = aiSrv.URL
	st.syncer = &Syncer{Store: s, Google: client, Client: &config.GoogleClient{ID: "client-id", Secret: "client-secret", RedirectURI: "https://astovia.test/integrations/google/callback"},
		AI: aiClient, Box: secretbox.New([]byte(strings.Repeat("s", 32))), Now: func() time.Time { return st.now }}
	st.connect(me)
	return st
}

// connect connects (or reconnects) an address with the refresh token of
// the fake.
func (st *syncTest) connect(email string) store.GmailAccount {
	st.t.Helper()
	a, err := st.s.SaveGmailAccount(context.Background(), "alice-id", "sid", email, func(id string) (string, error) {
		return st.syncer.Box.Seal(TokenPurpose("alice-id", id), refreshTk)
	}, st.now.Unix())
	if err != nil {
		st.t.Fatal(err)
	}
	st.accounts[email] = a.ID
	return a
}

func (st *syncTest) saveAI() store.AISettings {
	st.t.Helper()
	ctx := context.Background()
	var gen int64
	if cur, err := st.s.AISettings(ctx, "alice-id"); err == nil {
		gen = cur.Generation
	}
	sealed, err := st.syncer.Box.Seal(ai.KeyPurpose("alice-id"), aiKey)
	if err != nil {
		st.t.Fatal(err)
	}
	a, err := st.s.SaveAISettings(ctx, store.AISettings{UserID: "alice-id", Provider: ai.ProviderAnthropic, Model: "claude-haiku-5-5", APIKeySealed: sealed, KeyLast4: "6789"}, gen, true, "sid", st.now.Unix())
	if err != nil {
		st.t.Fatal(err)
	}
	return a
}

func (st *syncTest) account(email string) store.GmailAccount {
	st.t.Helper()
	a, err := st.s.GmailAccount(context.Background(), "alice-id", st.accounts[email])
	if err != nil {
		st.t.Fatal(err)
	}
	return a
}

func (st *syncTest) sync(email string) store.GmailAccount {
	st.t.Helper()
	st.syncer.Sync(context.Background(), st.account(email))
	return st.account(email)
}

func (st *syncTest) advance(d time.Duration) { st.now = st.now.Add(d) }

// recv adds a message from bob to alice in the thread ("" for a new
// thread), received d before now, and returns its ID.
func (st *syncTest) recv(thread, subject, body string, d time.Duration) string {
	return st.g.add(gMsg{Thread: thread, At: st.now.Add(-d), From: "Bob <bob@example.com>", To: "Alice <alice@example.com>", Subject: subject, Body: body})
}

// sent adds a message alice sent in the thread d before now.
func (st *syncTest) sent(thread, body string, d time.Duration) string {
	return st.g.add(gMsg{Thread: thread, Labels: []string{"SENT"}, At: st.now.Add(-d), From: "Alice Liddell <alice@example.com>", To: "Bob <bob@example.com>", Subject: "Re: 見積もり", Body: body})
}

func (st *syncTest) items() []store.ActionItem {
	st.t.Helper()
	items, err := st.s.ListActionItems(context.Background(), "alice-id", "", nil, 300)
	if err != nil {
		st.t.Fatal(err)
	}
	return items
}

func (st *syncTest) item(key string) (store.ActionItem, bool) {
	st.t.Helper()
	for _, i := range st.items() {
		if i.SourceKey.String == key {
			return i, true
		}
	}
	return store.ActionItem{}, false
}

func (st *syncTest) mustItem(key string) store.ActionItem {
	st.t.Helper()
	i, ok := st.item(key)
	if !ok {
		keys := []string{}
		for _, i := range st.items() {
			keys = append(keys, i.SourceKey.String)
		}
		st.t.Fatalf("no item %s; have %v", key, keys)
	}
	return i
}

func (st *syncTest) pending() []store.Notification {
	st.t.Helper()
	list, err := st.s.DueNotifications(context.Background(), st.now.Unix()+86400*365, 100)
	if err != nil {
		st.t.Fatal(err)
	}
	return list
}

func runKey(email, thread, first, last string) string {
	return "gmail:" + email + ":r:" + thread + ":" + first + ":" + last
}

func refsOf(t *testing.T, item store.ActionItem) []map[string]string {
	t.Helper()
	var refs []map[string]string
	if err := json.Unmarshal([]byte(item.Refs), &refs); err != nil {
		t.Fatal(err)
	}
	return refs
}

func TestSourceKeys(t *testing.T) {
	k := sourceKey{Email: me, Thread: "18a0", First: "18a1", Last: "18a9"}
	if got, ok := parseSourceKey(k.String()); !ok || got != k || k.String() != "gmail:alice@example.com:r:18a0:18a1:18a9" {
		t.Fatalf("%s: %+v %v", k, got, ok)
	}
	for _, v := range []string{"", "gmail:alice@example.com:g:18a0:item", "gmail:alice@example.com:r:18a0:18a1", "gmail:alice@example.com:r:18A0:1:2", "gmail:alice:r:1:2:3",
		"slack:alice@example.com:r:1:2:3", "gmail:alice@example.com:x:1:2:3", "gmail:alice@example.com:r:1:2:3:4", "gmail:a:b@example.com:r:1:2:3"} {
		if _, ok := parseSourceKey(v); ok {
			t.Fatalf("%q parsed", v)
		}
	}
}

func TestHelpers(t *testing.T) {
	for v, want := range map[string]bool{threadURL("a+b@example.com", "18a0"): true, "https://mail.google.com/mail/#all/1": true, "http://mail.google.com/": false,
		"https://mail.google.com.evil.example/": false, "https://mail.google.com:8443/": false, "https://user@mail.google.com/": false, "javascript:alert(1)": false, "https://gmail.com/": false} {
		if gmailURL(v).Valid != want {
			t.Fatalf("gmailURL(%q)", v)
		}
	}
	if got := threadURL("a+b@example.com", "18a0"); got != "https://mail.google.com/mail/?authuser=a%2Bb%40example.com#all/18a0" {
		t.Fatalf("threadURL: %s", got)
	}
	for in, want := range map[string]string{"A.B+x@Gmail.com": "ab@gmail.com", "a.b@googlemail.com": "ab@gmail.com", "A.B+x@Example.com": "a.b@example.com", "x": "x"} {
		if got := normalizeAddress(in); got != want {
			t.Fatalf("normalizeAddress(%q) = %q", in, got)
		}
	}
	for raw, want := range map[string]bool{"alice@example.com": true, "<ALICE@example.com>, x": true, "ba@example.com": false, "alice@example.com.au": false, "x.alice@example.com": false, "alice@example.com_": false, "": false} {
		if containsAddress(raw, "alice@example.com") != want {
			t.Fatalf("containsAddress(%q)", raw)
		}
	}
	for _, tc := range []struct {
		m    google.Message
		want bool
	}{
		{google.Message{}, false},
		{google.Message{HasAutoSubmitted: true, AutoSubmitted: " No "}, false},
		{google.Message{HasAutoSubmitted: true, AutoSubmitted: ""}, false},
		{google.Message{HasAutoSubmitted: true, AutoSubmitted: "auto-generated"}, true},
		{google.Message{Precedence: "Bulk"}, true},
		{google.Message{Precedence: "junk"}, true},
		{google.Message{Precedence: "list"}, false},
		{google.Message{FromEmail: "No-Reply@example.com"}, true},
		{google.Message{FromEmail: "mailer-daemon@example.com"}, true},
		{google.Message{FromEmail: "noreplyx@example.com"}, false},
		{google.Message{FromName: "noreply"}, false},
	} {
		if automatic(tc.m) != tc.want {
			t.Fatalf("automatic(%+v)", tc.m)
		}
	}
}

// captureLogs collects the log and slog output of the test.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	var mu sync.Mutex
	w := writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return buf.Write(p)
	})
	prevOut, prevFlags := log.Writer(), log.Flags()
	prevSlog := slog.Default()
	log.SetOutput(w)
	slog.SetDefault(slog.New(slog.NewTextHandler(w, nil)))
	t.Cleanup(func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
		slog.SetDefault(prevSlog)
	})
	return &buf
}

type writerFunc func(p []byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

func TestAccessTokens(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		st := newSyncTest(t, driver)
		// The first synchronization refreshed once; the token is reused
		// while it is valid.
		if st.g.issued != 1 {
			t.Fatalf("issued: %d", st.g.issued)
		}
		st.sync(me)
		if st.g.count("token") != 0 || st.g.count("list") != 1 {
			t.Fatalf("reused: %v", st.g.requests)
		}
		// Two minutes before the expiry the token is refreshed.
		st.advance(time.Hour - 10*time.Minute - 2*time.Minute)
		st.g.reset0()
		st.sync(me)
		if st.g.count("token") != 1 || st.g.issued != 2 {
			t.Fatalf("refresh before expiry: %v", st.g.requests)
		}

		// A 401 refreshes once and retries the same request.
		st.advance(time.Minute)
		st.g.reset0()
		st.g.set(func(f *fakeGoogle) { f.unauth = 1 })
		a := st.sync(me)
		if st.g.count("token") != 1 || st.g.count("list") != 2 || a.Status != store.GmailActive || a.LastError.Valid {
			t.Fatalf("401 retry: %v %+v", st.g.requests, a)
		}

		// A refresh token replaced by Google is stored sealed.
		st.advance(time.Minute)
		st.g.set(func(f *fakeGoogle) { f.unauth, f.rotate = 1, "1//rotated-0002" })
		a = st.sync(me)
		if plain, err := st.syncer.Box.Open(TokenPurpose("alice-id", a.ID), a.TokenSealed); err != nil || plain != "1//rotated-0002" || a.Generation != 1 || a.Status != store.GmailActive {
			t.Fatalf("rotated: %q %v %+v", plain, err, a)
		}
		// The new refresh token is used by the next refresh.
		st.advance(time.Minute)
		st.g.set(func(f *fakeGoogle) { f.unauth = 1 })
		if a = st.sync(me); a.Status != store.GmailActive || a.LastError.Valid {
			t.Fatalf("rotated token used: %+v", a)
		}

		// A 401 again after the refresh is token_invalid.
		st.advance(time.Minute)
		st.g.reset0()
		st.g.set(func(f *fakeGoogle) { f.unauth = 2 })
		a = st.sync(me)
		if a.Status != store.GmailError || a.LastError.String != ErrorTokenInvalid || st.g.count("token") != 1 || st.g.count("list") != 2 {
			t.Fatalf("second 401: %v %+v", st.g.requests, a)
		}
	})
}

func TestInvalidGrant(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		st := newBareSyncTest(t, driver)
		st.saveAI()
		st.g.set(func(f *fakeGoogle) { f.refresh = "revoked" })
		a := st.sync(me)
		if a.Status != store.GmailError || a.LastError.String != ErrorTokenInvalid || a.LastSyncedAt.Valid {
			t.Fatalf("invalid_grant: %+v", a)
		}
		// Accounts in error are not due.
		due, err := st.s.DueGmailAccounts(context.Background(), st.now.Unix()+3600, 10)
		if err != nil || len(due) != 0 {
			t.Fatalf("due: %v %v", due, err)
		}
		// Connecting again clears the error and keeps nothing stale.
		st.g.set(func(f *fakeGoogle) { f.refresh = refreshTk })
		st.connect(me)
		if a = st.sync(me); a.Status != store.GmailActive || a.LastError.Valid || a.Generation != 2 || !a.LastSyncedAt.Valid {
			t.Fatalf("reconnected: %+v", a)
		}
	})
}

func TestSyncStates(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		st := newSyncTest(t, driver)
		ctx := context.Background()
		type tc struct {
			name     string
			kind     string
			status   int
			body     string
			header   map[string]string
			code     string
			state    string
			failures int64
			next     int64
		}
		reason := func(r string) string {
			return `{"error":{"code":403,"errors":[{"reason":"` + r + `"}],"status":"PERMISSION_DENIED"}}`
		}
		windowEnd := st.now.Add(-10 * time.Minute).Unix()
		for _, c := range []tc{
			{"429 with Retry-After", "list", 429, `{}`, map[string]string{"Retry-After": "120"}, ErrorRateLimited, store.GmailActive, 0, 120},
			{"429", "list", 429, `{}`, nil, ErrorRateLimited, store.GmailActive, 0, 900},
			{"rate 403", "list", 403, reason("userRateLimitExceeded"), nil, ErrorRateLimited, store.GmailActive, 0, 900},
			{"500", "list", 500, `{}`, nil, ErrorUnavailable, store.GmailActive, 1, 60},
			{"503 again", "list", 503, `{}`, nil, ErrorUnavailable, store.GmailActive, 2, 120},
			{"api disabled", "list", 403, reason("accessNotConfigured"), nil, ErrorAPIDisabled, store.GmailActive, 3, 240},
			{"client invalid", "token", 401, `{"error":"invalid_client"}`, nil, ErrorClientInvalid, store.GmailActive, 4, 480},
			{"scopes", "list", 403, reason("insufficientPermissions"), nil, ErrorScopesMissing, store.GmailError, 4, -1},
		} {
			st.g.set(func(f *fakeGoogle) {
				f.statuses, f.bodies, f.headers = map[string]int{c.kind: c.status}, map[string]string{c.kind: c.body}, c.header
				f.access = "" // a refresh is needed for the token cases
			})
			st.syncer.dropToken(st.account(me))
			a := st.sync(me)
			if a.LastError.String != c.code || a.Status != c.state || a.FailureCount != c.failures || (c.next >= 0 && a.NextPollAt != st.now.Unix()+c.next) || a.LastSyncedAt.Int64 != windowEnd {
				t.Fatalf("%s: %+v", c.name, a)
			}
			st.advance(time.Minute)
		}
		// policy_blocked: a Workspace administrator blocks the client.
		for _, c := range []tc{{"domainPolicy", "list", 403, reason("domainPolicy"), nil, ErrorPolicyBlocked, store.GmailError, 0, -1}, {"admin_policy_enforced", "token", 400, `{"error":"admin_policy_enforced"}`, nil, ErrorPolicyBlocked, store.GmailError, 0, -1}} {
			st.connect(me)
			st.g.set(func(f *fakeGoogle) {
				f.statuses, f.bodies, f.headers = map[string]int{c.kind: c.status}, map[string]string{c.kind: c.body}, nil
				f.access = ""
			})
			if a := st.sync(me); a.LastError.String != c.code || a.Status != c.state {
				t.Fatalf("%s: %+v", c.name, a)
			}
		}
		// A success clears the error and the failures.
		st.g.set(func(f *fakeGoogle) { f.statuses = map[string]int{} })
		st.connect(me)
		if a := st.sync(me); a.LastError.Valid || a.FailureCount != 0 || a.NextPollAt != st.now.Unix()+60 {
			t.Fatalf("recovered: %+v", a)
		}

		// An account replaced during the synchronization keeps the new
		// state: the results of the old generation are discarded.
		old := st.account(me)
		st.g.set(func(f *fakeGoogle) {
			f.statuses = map[string]int{"list": 500}
			f.hooks["list"] = func() { st.connect(me) }
		})
		st.syncer.Sync(ctx, old)
		if a := st.account(me); a.Generation != old.Generation+1 || a.FailureCount != 0 || a.LastError.Valid || a.NextPollAt != st.now.Unix() {
			t.Fatalf("stale: %+v", a)
		}
		st.g.set(func(f *fakeGoogle) { f.hooks = map[string]func(){}; f.statuses = map[string]int{} })

		// A token that cannot be opened stops the account.
		a := st.account(me)
		if err := st.s.GmailSync(ctx, "alice-id", a.ID, a.Generation, func(tx *store.GmailTx) error { return tx.UpdateGmailToken("garbage") }); err != nil {
			t.Fatal(err)
		}
		if a = st.sync(me); a.Status != store.GmailError || a.LastError.String != ErrorTokenUndecryptable {
			t.Fatalf("undecryptable: %+v", a)
		}
	})
}

// Without the OAuth client nothing is done, and nothing changes.
func TestNoClient(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		st := newBareSyncTest(t, driver)
		st.saveAI()
		st.syncer.Client = nil
		before := st.account(me)
		st.syncer.RunOnce(context.Background())
		st.syncer.Sync(context.Background(), before)
		if after := st.account(me); after != before || st.g.count("") != 0 {
			t.Fatalf("changed: %+v %v", after, st.g.requests)
		}
	})
}

// Without AI settings nothing is read: the token is not refreshed and the
// window does not move.
func TestNoAI(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		st := newBareSyncTest(t, driver)
		st.g.set(func(f *fakeGoogle) { f.refresh = "revoked" })
		st.syncer.RunOnce(context.Background())
		a := st.account(me)
		if st.g.count("") != 0 || a.Status != store.GmailActive || a.LastSyncedAt.Valid || a.NextPollAt != st.now.Unix()+60 {
			t.Fatalf("without AI: %v %+v", st.g.requests, a)
		}
	})
}

// RunOnce synchronizes each due account on its own and forgets the access
// tokens unused for a day.
func TestRunOnce(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		st := newBareSyncTest(t, driver)
		st.saveAI()
		st.connect("second@example.com")
		st.syncer.RunOnce(context.Background())
		if st.g.count("list") != 2 || len(st.syncer.tokens) != 2 {
			t.Fatalf("both accounts: %v %d", st.g.requests, len(st.syncer.tokens))
		}
		st.g.reset0()
		st.advance(30 * time.Second)
		st.syncer.RunOnce(context.Background())
		if st.g.count("") != 0 {
			t.Fatalf("not due: %v", st.g.requests)
		}
		st.advance(25 * time.Hour)
		st.syncer.prune(st.now)
		if len(st.syncer.tokens) != 0 {
			t.Fatalf("tokens kept: %d", len(st.syncer.tokens))
		}
	})
}

// The logs carry no address, subject, body or token.
func TestLogs(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		logs := captureLogs(t)
		st := newSyncTest(t, driver)
		th := st.recv("", "極秘の件名", "極秘の本文です", time.Minute)
		st.g.set(func(f *fakeGoogle) { f.statuses["threads:"+th] = 404 })
		st.recv("", "別の件名", "別の本文", 2*time.Minute)
		st.ai.set(aiError(400))
		st.g.set(func(f *fakeGoogle) { f.pageSize = 1 })
		st.sync(me)
		st.advance(time.Minute)
		st.g.set(func(f *fakeGoogle) {
			f.statuses = map[string]int{"list": 403}
			f.bodies = map[string]string{"list": `{"error":{"errors":[{"reason":"insufficientPermissions"}],"message":"alice@example.com"}}`}
		})
		st.sync(me)
		out := logs.String()
		if !strings.Contains(out, "gmail sync") {
			t.Fatalf("nothing logged: %s", out)
		}
		for _, s := range []string{me, "bob@example.com", "極秘", "別の", refreshTk, "ya29", aiKey, "client-secret"} {
			if strings.Contains(out, s) {
				t.Fatalf("log contains %q:\n%s", s, out)
			}
		}
	})
}
