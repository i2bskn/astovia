package slacksync

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/i2bskn/astovia/internal/ai"
	"github.com/i2bskn/astovia/internal/secretbox"
	"github.com/i2bskn/astovia/internal/slack"
	"github.com/i2bskn/astovia/internal/store"
	"github.com/i2bskn/astovia/internal/testutil"
)

const (
	me        = "U1"
	slackTok  = "xoxp-test-token-0001"
	aiKey     = "sk-ant-test-key-0123456789"
	permaBase = "https://acme.slack.com/archives/"
)

// msg is a message of the fake workspace. Channels starting with D are
// direct messages and G group direct messages.
type msg struct {
	TS, User, Text, ThreadTS, Subtype, BotID string
}

// fakeSlack is an in-memory Slack Web API. forced maps a method to an error
// code ("ok": false) and statuses to an HTTP status; forcedChannel limits a
// forced code to one channel.
type fakeSlack struct {
	mu            sync.Mutex
	channels      map[string][]msg
	names         map[string]string
	mpims         []string
	forced        map[string]string
	forcedChannel map[string]string
	statuses      map[string]int
	retryAfter    string
	pageSize      int
	requests      []request
}

type request struct {
	method string
	query  url.Values
	auth   string
}

func newFakeSlack() *fakeSlack {
	return &fakeSlack{channels: map[string][]msg{}, names: map[string]string{me: "alice", "U2": "bob", "U3": "carol"}, forced: map[string]string{}, forcedChannel: map[string]string{}, statuses: map[string]int{}, pageSize: 100}
}

func (f *fakeSlack) set(fn func(f *fakeSlack)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *fakeSlack) post(channel string, m msg) {
	f.set(func(f *fakeSlack) { f.channels[channel] = append(f.channels[channel], m) })
}

// count returns the number of requests of a method ("" for all).
func (f *fakeSlack) count(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.requests {
		if method == "" || r.method == method {
			n++
		}
	}
	return n
}

func (f *fakeSlack) reset() { f.set(func(f *fakeSlack) { f.requests = nil }) }

func less(a, b string) bool { return compareTS(a, b) < 0 }

func link(channel, ts, thread string) string {
	v := permaBase + channel + "/p" + strings.ReplaceAll(ts, ".", "")
	if thread != "" && thread != ts {
		v += "?thread_ts=" + thread + "&cid=" + channel
	}
	return v
}

func (f *fakeSlack) json(m msg, channel string) map[string]any {
	v := map[string]any{"ts": m.TS, "user": m.User, "text": m.Text, "type": "message"}
	if m.ThreadTS != "" {
		v["thread_ts"] = m.ThreadTS
	}
	if m.Subtype != "" {
		v["subtype"] = m.Subtype
	}
	if m.BotID != "" {
		v["bot_id"] = m.BotID
	}
	if m.ThreadTS == m.TS {
		latest := ""
		for _, r := range f.channels[channel] {
			if r.ThreadTS == m.TS && r.TS != m.TS && (latest == "" || less(latest, r.TS)) {
				latest = r.TS
			}
		}
		if latest != "" {
			v["latest_reply"] = latest
		}
	}
	return v
}

func (f *fakeSlack) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	method := strings.TrimPrefix(r.URL.Path, "/")
	q := r.URL.Query()
	f.requests = append(f.requests, request{method: method, query: q, auth: r.Header.Get("Authorization")})
	if code, ok := f.statuses[method]; ok {
		if code == 429 && f.retryAfter != "" {
			w.Header().Set("Retry-After", f.retryAfter)
		}
		w.WriteHeader(code)
		return
	}
	write := func(v map[string]any) {
		v["ok"] = true
		_ = json.NewEncoder(w).Encode(v)
	}
	if code, ok := f.forced[method]; ok && (f.forcedChannel[method] == "" || f.forcedChannel[method] == q.Get("channel")) {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": code})
		return
	}
	switch method {
	case "search.messages":
		query := q.Get("query")
		var hits []map[string]any
		var all []struct {
			ch string
			m  msg
		}
		for ch, list := range f.channels {
			for _, m := range list {
				all = append(all, struct {
					ch string
					m  msg
				}{ch, m})
			}
		}
		sort.Slice(all, func(i, j int) bool { return less(all[j].m.TS, all[i].m.TS) })
		for _, e := range all {
			im, mpim := strings.HasPrefix(e.ch, "D"), strings.HasPrefix(e.ch, "G")
			switch {
			case strings.HasPrefix(query, "to:me"):
				if !im || e.m.User == me {
					continue
				}
			case strings.HasPrefix(query, "<@"+me+">"):
				if !strings.Contains(e.m.Text, "<@"+me+">") {
					continue
				}
			default:
				continue
			}
			hits = append(hits, map[string]any{"ts": e.m.TS, "user": e.m.User, "text": e.m.Text, "permalink": link(e.ch, e.m.TS, e.m.ThreadTS),
				"channel": map[string]any{"id": e.ch, "name": "name-" + e.ch, "is_im": im, "is_mpim": mpim}})
		}
		page, _ := strconv.Atoi(q.Get("page"))
		size := f.pageSize
		pages := (len(hits) + size - 1) / size
		start, end := min(len(hits), (page-1)*size), min(len(hits), page*size)
		write(map[string]any{"messages": map[string]any{"matches": hits[start:end], "paging": map[string]any{"page": page, "pages": max(pages, 1)}}})
	case "conversations.history", "conversations.replies":
		ch := q.Get("channel")
		var list []msg
		for _, m := range f.channels[ch] {
			if method == "conversations.history" && m.ThreadTS != "" && m.ThreadTS != m.TS && m.Subtype != "thread_broadcast" {
				continue
			}
			if method == "conversations.replies" && m.TS != q.Get("ts") && m.ThreadTS != q.Get("ts") {
				continue
			}
			list = append(list, m)
		}
		if method == "conversations.replies" && len(list) == 0 {
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "thread_not_found"})
			return
		}
		inclusive := q.Get("inclusive") == "true"
		var out []msg
		for _, m := range list {
			isParent := method == "conversations.replies" && m.TS == q.Get("ts")
			if o := q.Get("oldest"); o != "" && !isParent && (less(m.TS, o) || (!inclusive && m.TS == o)) {
				continue
			}
			if l := q.Get("latest"); l != "" && !isParent && (less(l, m.TS) || (!inclusive && m.TS == l)) {
				continue
			}
			out = append(out, m)
		}
		if method == "conversations.history" {
			sort.Slice(out, func(i, j int) bool { return less(out[j].TS, out[i].TS) })
		} else {
			sort.Slice(out, func(i, j int) bool { return less(out[i].TS, out[j].TS) })
		}
		offset, _ := strconv.Atoi(q.Get("cursor"))
		limit, _ := strconv.Atoi(q.Get("limit"))
		if limit == 0 {
			limit = 100
		}
		page := out[min(offset, len(out)):min(offset+limit, len(out))]
		items := []map[string]any{}
		for _, m := range page {
			items = append(items, f.json(m, ch))
		}
		res := map[string]any{"messages": items, "has_more": offset+limit < len(out)}
		if offset+limit < len(out) {
			res["response_metadata"] = map[string]any{"next_cursor": strconv.Itoa(offset + limit)}
		}
		write(res)
	case "users.info":
		name, ok := f.names[q.Get("user")]
		if !ok {
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "user_not_found"})
			return
		}
		write(map[string]any{"user": map[string]any{"id": q.Get("user"), "name": name, "profile": map[string]any{"display_name": name}}})
	case "users.conversations":
		list := []map[string]any{}
		for _, id := range f.mpims {
			list = append(list, map[string]any{"id": id, "name": "mpdm-" + id, "is_mpim": true})
		}
		write(map[string]any{"channels": list})
	case "chat.getPermalink":
		write(map[string]any{"permalink": link(q.Get("channel"), q.Get("message_ts"), "")})
	default:
		w.WriteHeader(404)
	}
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

// input returns the JSON document of the n-th request's user message.
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

// aiJudged answers a judgment with a decision and an event (empty strings
// for none).
func aiJudged(decision, task, summary, eventTitle, eventStart, eventEnd string) func(http.ResponseWriter) {
	return aiText(map[string]any{"decision": decision, "task": task, "summary": summary, "event_title": eventTitle, "event_start": eventStart, "event_end": eventEnd})
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
	t      *testing.T
	s      *store.Store
	sl     *fakeSlack
	ai     *fakeAI
	syncer *Syncer
	now    time.Time
}

func drivers(t *testing.T, fn func(t *testing.T, driver string)) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) { fn(t, driver) })
	}
}

// newSyncTest connects alice (U1 in T1) with the AI configured, and runs
// the first synchronization on an empty workspace so that the window has
// started.
func newSyncTest(t *testing.T, driver string) *syncTest {
	t.Helper()
	st := newBareSyncTest(t, driver)
	st.saveAI()
	st.sync()
	st.sl.reset()
	return st
}

// newBareSyncTest connects alice without AI settings and without syncing.
func newBareSyncTest(t *testing.T, driver string) *syncTest {
	t.Helper()
	ctx := context.Background()
	s, _ := testutil.Database(t, driver)
	st := &syncTest{t: t, s: s, now: time.Date(2026, 10, 24, 1, 0, 0, 0, time.UTC)}
	u := store.User{ID: "alice-id", Email: "alice@example.com", PasswordHash: "x"}
	if err := s.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	if err := s.Login(ctx, u, store.Session{ID: "sid", UserID: u.ID, ExpiresAt: st.now.Unix() + 86400*30, CreatedAt: st.now.Unix()}, store.Refresh{Hash: "r", SessionID: "sid", CreatedAt: st.now.Unix()}); err != nil {
		t.Fatal(err)
	}
	st.sl = newFakeSlack()
	srv := httptest.NewServer(st.sl)
	t.Cleanup(srv.Close)
	client := slack.New(func() time.Time { return st.now })
	client.BaseURL = srv.URL
	st.ai = &fakeAI{}
	aiSrv := httptest.NewServer(st.ai)
	t.Cleanup(aiSrv.Close)
	aiClient := ai.New()
	aiClient.BaseURL = aiSrv.URL
	st.syncer = &Syncer{Store: s, Slack: client, AI: aiClient, Box: secretbox.New([]byte(strings.Repeat("s", 32))), Now: func() time.Time { return st.now }}
	st.connect("T1", me, store.SlackSettings{})
	return st
}

func (st *syncTest) connect(team, user string, settings store.SlackSettings) store.SlackConnection {
	st.t.Helper()
	sealed, err := st.syncer.Box.Seal(TokenPurpose("alice-id"), slackTok)
	if err != nil {
		st.t.Fatal(err)
	}
	c, err := st.s.SaveSlackConnection(context.Background(), store.SlackConnection{UserID: "alice-id", TokenSealed: sealed, TokenLast4: "0001", TeamID: team, TeamName: "Acme", SlackUserID: user, SlackUserName: "alice", Scopes: strings.Join(RequiredScopes, ",")}, settings, "sid", st.now.Unix())
	if err != nil {
		st.t.Fatal(err)
	}
	return c
}

func (st *syncTest) settings(v store.SlackSettings) {
	st.t.Helper()
	if _, err := st.s.UpdateSlackSettings(context.Background(), "alice-id", v, "sid", st.now.Unix()); err != nil {
		st.t.Fatal(err)
	}
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

func (st *syncTest) aiSettings() store.AISettings {
	st.t.Helper()
	a, err := st.s.AISettings(context.Background(), "alice-id")
	if err != nil {
		st.t.Fatal(err)
	}
	return a
}

func (st *syncTest) conn() store.SlackConnection {
	st.t.Helper()
	c, err := st.s.SlackConnection(context.Background(), "alice-id")
	if err != nil {
		st.t.Fatal(err)
	}
	return c
}

func (st *syncTest) sync() store.SlackConnection {
	st.t.Helper()
	st.syncer.Sync(context.Background(), st.conn())
	return st.conn()
}

func (st *syncTest) advance(d time.Duration) { st.now = st.now.Add(d) }

// ts returns a timestamp d before now, with the micro part n.
func (st *syncTest) ts(d time.Duration, n int) string {
	return strconv.FormatInt(st.now.Add(-d).Unix(), 10) + "." + strconv.Itoa(100000+n)
}

func (st *syncTest) items() []store.ActionItem {
	st.t.Helper()
	items, err := st.s.ListActionItems(context.Background(), "alice-id", "", nil, 100)
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

func mentionKey(channel, thread, ts string) string {
	return "slack:T1:m:" + channel + ":" + thread + ":" + ts
}

func dmKey(channel, thread, first, last string) string {
	return "slack:T1:d:" + channel + ":" + thread + ":" + first + ":" + last
}

// posts returns the posts of an AI input as (id, target) pairs.
func posts(in map[string]any) [][2]string {
	var out [][2]string
	for _, p := range in["posts"].([]any) {
		m := p.(map[string]any)
		target := "-"
		if v, ok := m["target"].(bool); ok && v {
			target = "T"
		}
		out = append(out, [2]string{m["id"].(string), target})
	}
	return out
}

func TestSourceKeys(t *testing.T) {
	for _, k := range []sourceKey{
		{Team: "T1", Kind: kindMention, Channel: "C1", Thread: noThread, First: "1.000001", Last: "1.000001"},
		{Team: "T1", Kind: kindMention, Channel: "C1", Thread: "1.000000", First: "1.000001", Last: "1.000001"},
		{Team: "T1", Kind: kindDM, Channel: "D1", Thread: noThread, First: "1.000001", Last: "2.5"},
	} {
		got, ok := parseSourceKey(k.String())
		if !ok || got != k {
			t.Fatalf("%s: %+v %v", k, got, ok)
		}
	}
	for _, v := range []string{"", "github:review:o/r#1", "slack:t1:m:C1:-:1.000001", "slack:T1:x:C1:-:1.000001", "slack:T1:m:C1:-:1.000001:2.000001", "slack:T1:d:C1:-:2.000001:1.000001",
		"slack:T1:m:C1:x:1.000001", "slack:T1:m:C1:-:1", "slack:T1:m:C1:-:1.123456789", "slack:T1:m:C/1:-:1.000001", "slack:T1:d:C1:-:1.000001"} {
		if _, ok := parseSourceKey(v); ok {
			t.Fatalf("%q parsed", v)
		}
	}
	if compareTS("10.000001", "9.999999") != 1 || compareTS("1.1", "1.000002") != 1 || compareTS("1.000001", "1.0000010") != 0 || compareTS("1.5", "2.0") != -1 {
		t.Fatal("compareTS")
	}
}

func TestHelpers(t *testing.T) {
	for v, want := range map[string]bool{"https://acme.slack.com/archives/C1/p1": true, "https://slack.com/x": false, "http://acme.slack.com/": false, "https://acme.slack.com.evil.example/": false, "https://user@acme.slack.com/": false, "javascript:alert(1)": false} {
		if slackURL(v).Valid != want {
			t.Fatalf("slackURL(%q)", v)
		}
	}
	if got := excerpt(" a\nb\r\n\tc ", 80); got != "a b c" {
		t.Fatalf("excerpt: %q", got)
	}
	if got := searchThread(slack.Message{TS: "2.000001", Permalink: link("C1", "2.000001", "1.000001")}); got != "1.000001" {
		t.Fatalf("thread from permalink: %q", got)
	}
	if got := searchThread(slack.Message{TS: "1.000001", Permalink: link("C1", "1.000001", "1.000001")}); got != noThread {
		t.Fatalf("parent: %q", got)
	}
}

func TestFormatText(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		st := newSyncTest(t, driver)
		r := &run{s: st.syncer, ctx: context.Background(), parent: context.Background(), conn: st.conn(), token: slackTok, calls: map[string]int{}, names: map[string]string{}}
		got, err := r.formatText("<@U2> <@U9|old> see <#C1|general> <https://example.com|docs> <https://x.example> <!here> &lt;b&gt; &amp;")
		if err != nil || got != "@bob(U2) @U9(U9) see #general docs (https://example.com) https://x.example @here <b> &" {
			t.Fatalf("formatText: %q %v", got, err)
		}
		// Names are read once per synchronization.
		if _, err = r.formatText("<@U2> <@U2>"); err != nil || st.sl.count("users.info") != 2 {
			t.Fatalf("users.info calls: %d %v", st.sl.count("users.info"), err)
		}
	})
}
