package httpserver

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/i2bskn/astovia/internal/auth"
	"github.com/i2bskn/astovia/internal/config"
	"github.com/i2bskn/astovia/internal/gmailsync"
	"github.com/i2bskn/astovia/internal/google"
	"github.com/i2bskn/astovia/internal/store"
	"github.com/i2bskn/astovia/internal/testutil"
)

type fakeGmailRequest struct {
	path  string
	query url.Values
	token string
	form  url.Values
}

// fakeGmailFailure replaces an answer with status and body, with
// retryAfter as Retry-After.
type fakeGmailFailure struct {
	status     int
	body       string
	retryAfter string
}

// fakeGmail answers the token, revoke, profile and threads endpoints of
// Google. An authorization code gets the refresh token 1//refresh-<email>
// and every token answer a new access token; Gmail accepts only the access
// tokens issued, and none with reject. With rotate, a refresh replaces the
// refresh token.
// threads answers threads.get by ID (404 otherwise); failures replace the
// answer of "threads.list" or "threads.get <id>", and tooLarge answers a
// body over the client's limit. tokenHook runs before a refresh is
// answered, outside the lock.
type fakeGmail struct {
	mu          sync.Mutex
	email       string
	refreshOK   map[string]bool
	accessOK    map[string]bool
	issued      int
	rotate      bool
	reject      bool
	tokenStatus int
	tokenBody   string
	tokenHook   func(refresh string)
	list        string
	threads     map[string]string
	failures    map[string]fakeGmailFailure
	tooLarge    map[string]bool
	requests    []fakeGmailRequest
}

func (f *fakeGmail) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	form, _ := url.ParseQuery(string(raw))
	f.mu.Lock()
	hook := f.tokenHook
	f.mu.Unlock()
	if r.URL.Path == "/token" && form.Get("grant_type") == "refresh_token" && hook != nil {
		hook(form.Get("refresh_token"))
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	f.requests = append(f.requests, fakeGmailRequest{path: r.URL.Path, query: r.URL.Query(), token: token, form: form})
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.URL.Path == "/token":
		if f.tokenStatus != 0 {
			w.WriteHeader(f.tokenStatus)
			_, _ = w.Write([]byte(f.tokenBody))
			return
		}
		out := map[string]any{"access_token": f.access(), "expires_in": 3599, "scope": google.ScopeGmailReadonly, "token_type": "Bearer"}
		switch form.Get("grant_type") {
		case "authorization_code":
			refresh := "1//refresh-" + f.email
			f.refreshOK[refresh] = true
			out["refresh_token"] = refresh
		default:
			refresh := form.Get("refresh_token")
			if !f.refreshOK[refresh] {
				w.WriteHeader(400)
				_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
				return
			}
			if f.rotate {
				f.issued++
				next := fmt.Sprintf("1//rotated-%d", f.issued)
				delete(f.refreshOK, refresh)
				f.refreshOK[next] = true
				out["refresh_token"] = next
			}
		}
		_ = json.NewEncoder(w).Encode(out)
	case r.URL.Path == "/revoke":
	case f.reject || !f.accessOK[token]:
		w.WriteHeader(401)
	case r.URL.Path == "/gmail/profile":
		_ = json.NewEncoder(w).Encode(map[string]any{"emailAddress": f.email})
	default:
		method := "threads.list"
		if id, ok := strings.CutPrefix(r.URL.Path, "/gmail/threads/"); ok {
			method = "threads.get " + id
		}
		if failure, ok := f.failures[method]; ok {
			if failure.retryAfter != "" {
				w.Header().Set("Retry-After", failure.retryAfter)
			}
			w.WriteHeader(failure.status)
			_, _ = w.Write([]byte(failure.body))
			return
		}
		if f.tooLarge[method] {
			_, _ = w.Write([]byte(strings.Repeat(" ", 10<<20+1)))
			return
		}
		body := f.list
		if method != "threads.list" {
			body = f.threads[strings.TrimPrefix(method, "threads.get ")]
		}
		if body == "" {
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"error":{"code":404}}`))
			return
		}
		_, _ = w.Write([]byte(body))
	}
}

// access issues an access token. The caller holds mu.
func (f *fakeGmail) access() string {
	f.issued++
	token := fmt.Sprintf("ya29.access-%d", f.issued)
	f.accessOK[token] = true
	return token
}

func (f *fakeGmail) set(fn func(f *fakeGmail)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

// calls returns the requests other than those of connecting and forgets
// them.
func (f *fakeGmail) calls() []fakeGmailRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakeGmailRequest
	for _, r := range f.requests {
		if r.path != "/gmail/profile" && r.form.Get("grant_type") != "authorization_code" {
			out = append(out, r)
		}
	}
	f.requests = nil
	return out
}

// gmailMethods names the requests: token (with the refresh token),
// threads.list or threads.get <id>.
func gmailMethods(calls []fakeGmailRequest) string {
	var out []string
	for _, c := range calls {
		switch {
		case c.path == "/token":
			out = append(out, "token "+c.form.Get("refresh_token"))
		case c.path == "/gmail/threads":
			out = append(out, "threads.list")
		default:
			out = append(out, "threads.get "+strings.TrimPrefix(c.path, "/gmail/threads/"))
		}
	}
	return strings.Join(out, ",")
}

type mcpGmailAPI struct {
	t       *testing.T
	store   *store.Store
	server  *Server
	handler http.Handler
	g       *fakeGmail
	clock   *atomic.Int64
	alice   string
	bob     string
}

func newMCPGmailAPI(t *testing.T, driver string) *mcpGmailAPI {
	t.Helper()
	s, _ := testutil.Database(t, driver)
	hash, err := auth.Password("correct-password-123")
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range []store.User{{ID: "alice-id", Email: "alice@example.com", PasswordHash: hash, Role: store.RoleOwner}, {ID: "bob-id", Email: "bob@example.com", PasswordHash: hash, Role: store.RoleOwner}} {
		if err = s.CreateUser(context.Background(), u); err != nil {
			t.Fatal(err)
		}
	}
	a, err := auth.New(s, make([]byte, 32), "https://astovia.test")
	if err != nil {
		t.Fatal(err)
	}
	// The clock is read by the server's goroutines while tests move it.
	clock := &atomic.Int64{}
	clock.Store(time.Date(2026, 10, 24, 1, 2, 3, 0, time.UTC).Unix())
	a.Now = func() time.Time { return time.Unix(clock.Load(), 0).UTC() }
	g := &fakeGmail{refreshOK: map[string]bool{}, accessOK: map[string]bool{}, threads: map[string]string{}, failures: map[string]fakeGmailFailure{}, tooLarge: map[string]bool{}}
	srv := httptest.NewServer(g)
	t.Cleanup(srv.Close)
	server := newServer(config.Config{PublicURL: "https://astovia.test", Secure: true, Secret: make([]byte, 32)}, a, fstest.MapFS{}, nil)
	server.google.TokenEndpoint, server.google.RevokeEndpoint, server.google.GmailBaseURL = srv.URL+"/token", srv.URL+"/revoke", srv.URL+"/gmail"
	server.cfg.Google = &config.GoogleClient{ID: "client-id", Secret: "client-secret", RedirectURI: "https://astovia.test/integrations/google/callback"}
	api := &mcpGmailAPI{t: t, store: s, server: server, handler: server.handler(), g: g, clock: clock}
	tok := &tokenAPI{t: t, handler: api.handler}
	api.alice = tok.login("alice")
	api.bob = tok.login("bob")
	return api
}

// connect connects the Google account email for jwt's user through REST
// and returns its ID.
func (a *mcpGmailAPI) connect(jwt, email string) string {
	a.t.Helper()
	a.g.set(func(f *fakeGmail) { f.email = email })
	tok := &tokenAPI{t: a.t, handler: a.handler}
	w := tok.do("POST", "/api/v1/integrations/gmail/authorize", jwt, `{}`)
	var started struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &started); w.Code != 200 || err != nil {
		a.t.Fatalf("authorize: %d %s", w.Code, w.Body.String())
	}
	u, err := url.Parse(started.URL)
	if err != nil {
		a.t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]string{"state": u.Query().Get("state"), "code": "4/0AXcode"})
	w = tok.do("POST", "/api/v1/integrations/gmail/complete", jwt, string(body))
	var out gmailAccountResponse
	if err = json.Unmarshal(w.Body.Bytes(), &out); w.Code != 200 || err != nil {
		a.t.Fatalf("complete: %d %s", w.Code, w.Body.String())
	}
	a.g.calls()
	return out.ID
}

func (a *mcpGmailAPI) client(jwt string) *mcpClient {
	a.t.Helper()
	srv := httptest.NewServer(a.handler)
	a.t.Cleanup(srv.Close)
	return connectMCP(a.t, srv, (&tokenAPI{t: a.t, handler: a.handler}).issue(jwt, "mcp").Token, nil)
}

func (a *mcpGmailAPI) account(userID, id string) store.GmailAccount {
	a.t.Helper()
	acct, err := a.store.GmailAccount(context.Background(), userID, id)
	if err != nil {
		a.t.Fatal(err)
	}
	return acct
}

// setState stores the status and last error of an account, as the
// synchronization does.
func (a *mcpGmailAPI) setState(userID, id, status, lastError string) {
	a.t.Helper()
	acct := a.account(userID, id)
	err := a.store.GmailSync(context.Background(), userID, id, acct.Generation, func(tx *store.GmailTx) error {
		acct.Status, acct.LastError = status, sql.NullString{String: lastError, Valid: lastError != ""}
		return tx.SaveState(acct)
	})
	if err != nil {
		a.t.Fatal(err)
	}
}

// storeRefresh replaces the stored refresh token of an account, as the
// synchronization does when Google replaces it.
func (a *mcpGmailAPI) storeRefresh(userID, id, refresh string) {
	a.t.Helper()
	sealed, err := a.server.box.Seal(gmailsync.TokenPurpose(userID, id), refresh)
	if err != nil {
		a.t.Fatal(err)
	}
	acct := a.account(userID, id)
	if err = a.store.GmailSync(context.Background(), userID, id, acct.Generation, func(tx *store.GmailTx) error { return tx.UpdateGmailToken(sealed) }); err != nil {
		a.t.Fatal(err)
	}
}

// storedRefresh returns the stored refresh token of an account.
func (a *mcpGmailAPI) storedRefresh(userID, id string) string {
	a.t.Helper()
	v, err := a.server.box.Open(gmailsync.TokenPurpose(userID, id), a.account(userID, id).TokenSealed)
	if err != nil {
		a.t.Fatal(err)
	}
	return v
}

// gmailUnits returns the units counted for an account by gmailMCPLimiter.
func (a *mcpGmailAPI) gmailUnits(id string) int {
	l := a.server.gmailMCPLimiter
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.entries[id].count
}

// gmailTestMessage is a message of a thread answered by the fake.
type gmailTestMessage struct {
	id      string
	sec     int64
	labels  []string
	headers map[string]string
	text    string
}

func gmailThreadJSON(t *testing.T, id string, messages ...gmailTestMessage) string {
	t.Helper()
	var out []any
	for _, m := range messages {
		var headers []map[string]string
		keys := make([]string, 0, len(m.headers))
		for k := range m.headers {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			headers = append(headers, map[string]string{"name": k, "value": m.headers[k]})
		}
		labels := m.labels
		if labels == nil {
			labels = []string{"INBOX"}
		}
		out = append(out, map[string]any{"id": m.id, "threadId": id, "labelIds": labels, "internalDate": fmt.Sprint(m.sec * 1000),
			"payload": map[string]any{"mimeType": "text/plain", "headers": headers, "body": map[string]any{"data": base64.URLEncoding.EncodeToString([]byte(m.text))}}})
	}
	b, err := json.Marshal(map[string]any{"id": id, "messages": out})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// threadIDsOf returns the IDs of the threads of a search.
func threadIDsOf(threads []gmailThreadSummary) string {
	var out []string
	for _, th := range threads {
		out = append(out, th.ThreadID)
	}
	return strings.Join(out, ",")
}

// jsonThreadKeys returns the keys of each thread of a search's JSON.
func jsonThreadKeys(t *testing.T, text string) []string {
	t.Helper()
	var out struct {
		Threads []map[string]any `json:"threads"`
	}
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, th := range out.Threads {
		var k []string
		for key := range th {
			k = append(k, key)
		}
		sort.Strings(k)
		keys = append(keys, strings.Join(k, " "))
	}
	return keys
}

const (
	gmailBase      = int64(1792800000)
	gmailAliceAddr = "alice@example.com"
)

// searchThreads answers threads.list with 18a1-18a4: 18a1 has two messages
// and a draft, 18a2 and 18a4 have one message and 18a3 has only a draft.
func searchThreads(t *testing.T, f *fakeGmail) {
	f.list = `{"threads":[{"id":"18a1","snippet":"a &amp; b"},{"id":"18a2","snippet":"two"},{"id":"xyz"},{"id":"18a3","snippet":"three"},{"id":"18a4","snippet":"four"}],"nextPageToken":"0042","resultSizeEstimate":42}`
	f.threads["18a1"] = gmailThreadJSON(t, "18a1",
		gmailTestMessage{id: "18b3", sec: gmailBase + 3, labels: []string{"INBOX", "UNREAD", "IMPORTANT"}, headers: map[string]string{"From": "Carol <Carol@Example.com>", "Subject": "Re: 見積もり"}},
		gmailTestMessage{id: "18b1", sec: gmailBase + 1, labels: []string{"INBOX", "Label_1", "CATEGORY_UPDATES"}, headers: map[string]string{"From": "Bob <bob@example.com>", "Subject": "見積もり\u0007の件"}},
		gmailTestMessage{id: "18b5", sec: gmailBase + 5, labels: []string{"DRAFT"}, headers: map[string]string{"From": "alice@example.com", "Subject": "draft"}},
	)
	f.threads["18a2"] = gmailThreadJSON(t, "18a2", gmailTestMessage{id: "18c1", sec: gmailBase + 10, headers: map[string]string{"From": "dan@example.com"}})
	f.threads["18a3"] = gmailThreadJSON(t, "18a3", gmailTestMessage{id: "18d1", sec: gmailBase + 20, labels: []string{"DRAFT"}})
	f.threads["18a4"] = gmailThreadJSON(t, "18a4", gmailTestMessage{id: "18e1", sec: gmailBase + 30, labels: []string{"SPAM"}, headers: map[string]string{"From": "x <<"}})
	f.failures = map[string]fakeGmailFailure{}
	f.tooLarge = map[string]bool{}
}

func TestMCPGmail(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) { testMCPGmail(t, driver) })
	}
}

func testMCPGmail(t *testing.T, driver string) {
	api := newMCPGmailAPI(t, driver)
	// The limit is tested in TestMCPGmailLimit.
	setLimit(api.server.gmailMCPLimiter, 1000000)
	api.connect(api.bob, "bob@example.com")
	c := api.client(api.alice)

	// Without alice's own account nothing is sent, even though bob is
	// connected.
	var accounts gmailAccountListResponse
	c.ok("gmail_accounts_list", nil, &accounts)
	if text, _ := c.call("gmail_accounts_list", map[string]any{}); text != `{"accounts":[]}` {
		t.Fatalf("no accounts: %s", text)
	}
	const notConnected = "gmail_not_connected: No Google account is connected. Connect one in Astovia's settings."
	const noAccount = "not_found: No connected Google account has this address; use gmail_accounts_list."
	c.fails("gmail_threads_search", map[string]any{"query": "x"}, notConnected)
	c.fails("gmail_thread_get", map[string]any{"thread_id": "18a1"}, notConnected)
	c.fails("gmail_thread_get", map[string]any{"thread_id": "18a1", "account": "bob@example.com"}, noAccount)
	if calls := api.g.calls(); len(calls) != 0 {
		t.Fatalf("calls without an account: %+v", calls)
	}
	aliceID := api.connect(api.alice, gmailAliceAddr)

	// accounts_list: no IDs, tokens or settings.
	text, _ := c.call("gmail_accounts_list", nil)
	connected := formatTime(api.clock.Load())
	if text != `{"accounts":[{"email":"alice@example.com","status":"active","last_error":null,"connected_at":"`+connected+`","last_synced_at":null}]}` {
		t.Fatalf("accounts: %s", text)
	}

	// threads_search: the query, limit and page token are passed to Gmail;
	// each thread is read with its headers. A thread that is gone is left
	// out, one that fails is returned without them.
	api.g.set(func(f *fakeGmail) {
		searchThreads(t, f)
		f.failures["threads.get 18a2"] = fakeGmailFailure{status: 404, body: `{}`}
		f.failures["threads.get 18a4"] = fakeGmailFailure{status: 500, body: `x`}
	})
	var found gmailSearchResponse
	found = gmailSearchResponse{}
	c.ok("gmail_threads_search", map[string]any{"query": "  from:bob is:unread ", "limit": 5, "page_token": "0041"}, &found)
	if found.Account != gmailAliceAddr || found.NextPageToken != "0042" || found.ResultSizeEstimate != 42 || !found.Partial || threadIDsOf(found.Threads) != "18a1,18a3,18a4" {
		t.Fatalf("search: %+v", found)
	}
	calls := api.g.calls()
	if gmailMethods(calls) != "token 1//refresh-alice@example.com,threads.list,threads.get 18a1,threads.get 18a2,threads.get 18a3,threads.get 18a4" {
		t.Fatalf("search calls: %s", gmailMethods(calls))
	}
	if q := calls[1].query; q.Get("q") != "from:bob is:unread" || q.Get("maxResults") != "5" || q.Get("pageToken") != "0041" || q.Has("includeSpamTrash") || calls[1].token != calls[2].token {
		t.Fatalf("list query: %+v", calls[1])
	}
	if q := calls[2].query; q.Get("format") != "metadata" || !reflect.DeepEqual(q["metadataHeaders"], []string{"From", "To", "Cc", "Subject"}) {
		t.Fatalf("metadata query: %+v", q)
	}
	th := found.Threads[0]
	if th.Subject == nil || *th.Subject != "見積もりの件" || th.From == nil || *th.From != (gmailAddress{Name: "Carol", Email: "carol@example.com"}) || th.Date == nil || *th.Date != formatTime(gmailBase+3) ||
		th.Snippet != "a & b" || th.MessageCount == nil || *th.MessageCount != 2 || th.Unread == nil || !*th.Unread || th.Labels == nil ||
		!reflect.DeepEqual(*th.Labels, []string{"CATEGORY_UPDATES", "IMPORTANT", "INBOX", "UNREAD"}) || th.URL != "https://mail.google.com/mail/?authuser=alice%40example.com#all/18a1" {
		t.Fatalf("thread: %+v", th)
	}
	if th = found.Threads[1]; th.Subject != nil || th.From != nil || th.MessageCount != nil || th.Snippet != "three" || th.URL != "https://mail.google.com/mail/?authuser=alice%40example.com#all/18a3" {
		t.Fatalf("thread of a draft: %+v", th)
	}
	text, _ = c.call("gmail_threads_search", map[string]any{"query": "x"})
	full := "date from labels message_count snippet subject thread_id unread url"
	if keys := jsonThreadKeys(t, text); !reflect.DeepEqual(keys, []string{full, "snippet thread_id url", "snippet thread_id url"}) || !strings.Contains(text, `"partial":true`) {
		t.Fatalf("search JSON: %v %s", keys, text)
	}
	// The access token is reused; the defaults are sent.
	calls = api.g.calls()
	if gmailMethods(calls) != "threads.list,threads.get 18a1,threads.get 18a2,threads.get 18a3,threads.get 18a4" || calls[0].query.Get("maxResults") != "10" || calls[0].query.Has("pageToken") {
		t.Fatalf("second search calls: %s %+v", gmailMethods(calls), calls[0].query)
	}

	// A thread without labels to show, read, and the last page.
	api.g.set(func(f *fakeGmail) {
		searchThreads(t, f)
		f.list = `{"threads":[{"id":"18a4","snippet":"four"}],"resultSizeEstimate":1}`
	})
	text, _ = c.call("gmail_threads_search", map[string]any{"query": "x"})
	if !strings.Contains(text, `"labels":["SPAM"]`) || !strings.Contains(text, `"unread":false`) || !strings.Contains(text, `"from":{"name":"x \u003c\u003c","email":""}`) || strings.Contains(text, "next_page_token") || !strings.Contains(text, `"partial":false`) {
		t.Fatalf("search of one thread: %s", text)
	}
	api.g.set(func(f *fakeGmail) { f.list = `{"resultSizeEstimate":0}` })
	if text, _ = c.call("gmail_threads_search", map[string]any{"query": "x"}); text != `{"account":"alice@example.com","threads":[],"result_size_estimate":0,"partial":false}` {
		t.Fatalf("empty search: %s", text)
	}
	api.g.calls()

	// A rate limit or the limiter stops the rest; 400 and a response too
	// large skip only the thread.
	for _, tc := range []struct {
		name    string
		setup   func(f *fakeGmail)
		threads string
		calls   string
		full    []bool
	}{
		{"429", func(f *fakeGmail) { f.failures["threads.get 18a2"] = fakeGmailFailure{status: 429} }, "18a1,18a2,18a3,18a4", "threads.list,threads.get 18a1,threads.get 18a2", []bool{true, false, false, false}},
		{"rateLimitExceeded", func(f *fakeGmail) {
			f.failures["threads.get 18a1"] = fakeGmailFailure{status: 403, body: `{"error":{"errors":[{"reason":"rateLimitExceeded"}]}}`}
		}, "18a1,18a2,18a3,18a4", "threads.list,threads.get 18a1", []bool{false, false, false, false}},
		{"400", func(f *fakeGmail) { f.failures["threads.get 18a2"] = fakeGmailFailure{status: 400, body: `{}`} }, "18a1,18a2,18a3,18a4", "threads.list,threads.get 18a1,threads.get 18a2,threads.get 18a3,threads.get 18a4", []bool{true, false, false, true}},
		{"too large", func(f *fakeGmail) { f.tooLarge["threads.get 18a1"] = true }, "18a1,18a2,18a3,18a4", "threads.list,threads.get 18a1,threads.get 18a2,threads.get 18a3,threads.get 18a4", []bool{false, true, false, true}},
		{"api disabled", func(f *fakeGmail) {
			f.failures["threads.get 18a4"] = fakeGmailFailure{status: 403, body: `{"error":{"errors":[{"reason":"accessNotConfigured"}]}}`}
		}, "18a1,18a2,18a3,18a4", "threads.list,threads.get 18a1,threads.get 18a2,threads.get 18a3,threads.get 18a4", []bool{true, true, false, false}},
	} {
		api.g.set(func(f *fakeGmail) {
			searchThreads(t, f)
			tc.setup(f)
		})
		found = gmailSearchResponse{}
		c.ok("gmail_threads_search", map[string]any{"query": "x"}, &found)
		if !found.Partial || threadIDsOf(found.Threads) != tc.threads {
			t.Fatalf("%s: %+v", tc.name, found)
		}
		for i, want := range tc.full {
			if (found.Threads[i].MessageCount != nil) != want {
				t.Fatalf("%s: thread %d: %+v", tc.name, i, found.Threads[i])
			}
		}
		if calls = api.g.calls(); gmailMethods(calls) != tc.calls {
			t.Fatalf("%s calls: %s", tc.name, gmailMethods(calls))
		}
	}
	// The token, the scope and the policy fail the whole search.
	for body, want := range map[string]string{
		`{"error":{"errors":[{"reason":"insufficientPermissions"}]}}`: "gmail_scopes_missing: Reading mail is not allowed for the Google account. Reconnect it in Astovia's settings and allow it.",
		`{"error":{"errors":[{"reason":"domainPolicy"}]}}`:            "gmail_policy_blocked: The Google Workspace administrator has not allowed this app.",
	} {
		api.g.set(func(f *fakeGmail) {
			searchThreads(t, f)
			f.failures["threads.get 18a2"] = fakeGmailFailure{status: 403, body: body}
		})
		c.fails("gmail_threads_search", map[string]any{"query": "x"}, want)
	}
	api.g.calls()

	// Errors of threads.list.
	const unavailable = "gmail_unavailable: Gmail could not be reached. Try again later."
	for _, tc := range []struct {
		failure fakeGmailFailure
		want    string
	}{
		{fakeGmailFailure{status: 400, body: `{}`}, "invalid_request: Invalid page_token or query."},
		{fakeGmailFailure{status: 429, retryAfter: "30"}, "rate_limited: Gmail rate limit reached. Try again after 2026-10-24T01:02:33Z."},
		{fakeGmailFailure{status: 429}, "rate_limited: Gmail rate limit reached. Try again in a minute."},
		{fakeGmailFailure{status: 403, body: `{"error":{"errors":[{"reason":"insufficientPermissions"}]}}`}, "gmail_scopes_missing: Reading mail is not allowed for the Google account. Reconnect it in Astovia's settings and allow it."},
		{fakeGmailFailure{status: 403, body: `{"error":{"errors":[{"reason":"accessNotConfigured"}]}}`}, unavailable},
		{fakeGmailFailure{status: 500, body: `x`}, unavailable},
	} {
		api.g.set(func(f *fakeGmail) { f.failures = map[string]fakeGmailFailure{"threads.list": tc.failure} })
		c.fails("gmail_threads_search", map[string]any{"query": "x", "page_token": "abc"}, tc.want)
		if calls = api.g.calls(); gmailMethods(calls) != "threads.list" {
			t.Fatalf("calls for %+v: %s", tc.failure, gmailMethods(calls))
		}
	}
	api.g.set(func(f *fakeGmail) { f.failures = map[string]fakeGmailFailure{} })

	// thread_get: drafts are left out, the messages are oldest first (by
	// date, then ID) and only the newest 30 are returned.
	var messages []gmailTestMessage
	for i := 31; i >= 0; i-- {
		m := gmailTestMessage{id: fmt.Sprintf("19%03x", i), sec: gmailBase + int64(i), headers: map[string]string{"From": fmt.Sprintf("User %d <user%d@example.com>", i, i), "To": "alice@example.com", "Subject": "Re: 件名"}, text: fmt.Sprintf("body %d", i)}
		switch i {
		case 0:
			m.headers["Subject"] = "最初の\u0007件名"
		case 30:
			m.headers = map[string]string{"From": "Bob <bob@example.com>"}
		case 31:
			// The same date as 30; the ID orders them.
			m.sec = gmailBase + 30
			m.labels = []string{"INBOX", "UNREAD", "Label_9", "CATEGORY_PERSONAL", "TRASH"}
			m.headers = map[string]string{"From": "Carol <carol@example.com>", "To": "Alice <alice@example.com>, bob@example.com", "Cc": "<<\u0007broken", "Subject": "Re: 件名 "}
			m.text = strings.Repeat("あ", 5000)
		}
		messages = append(messages, m)
	}
	messages = append(messages, gmailTestMessage{id: "19fff", sec: gmailBase + 99, labels: []string{"DRAFT"}, headers: map[string]string{"From": "alice@example.com"}, text: "draft"})
	api.g.set(func(f *fakeGmail) { f.threads["19a0"] = gmailThreadJSON(t, "19a0", messages...) })
	var got gmailThreadResponse
	c.ok("gmail_thread_get", map[string]any{"thread_id": "19a0", "account": nil}, &got)
	if got.Account != gmailAliceAddr || got.ThreadID != "19a0" || got.Subject != "最初の件名" || got.URL != "https://mail.google.com/mail/?authuser=alice%40example.com#all/19a0" || got.OmittedMessages != 2 || len(got.Messages) != 30 {
		t.Fatalf("thread: %+v", got)
	}
	if got.Messages[0].MessageID != "19002" || got.Messages[28].MessageID != "1901e" || got.Messages[29].MessageID != "1901f" {
		t.Fatalf("order: %s %s %s", got.Messages[0].MessageID, got.Messages[28].MessageID, got.Messages[29].MessageID)
	}
	if m := got.Messages[0]; !reflect.DeepEqual(m.From, gmailAddress{Name: "User 2", Email: "user2@example.com"}) || !reflect.DeepEqual(m.To, []gmailAddress{{Email: "alice@example.com"}}) ||
		len(m.Cc) != 0 || m.Date != formatTime(gmailBase+2) || m.Subject != "Re: 件名" || !reflect.DeepEqual(m.Labels, []string{"INBOX"}) || m.Text != "body 2" || m.TextTruncated {
		t.Fatalf("message: %+v", m)
	}
	if m := got.Messages[29]; m.From.Name != "Carol" || !reflect.DeepEqual(m.To, []gmailAddress{{Name: "Alice", Email: "alice@example.com"}, {Email: "bob@example.com"}}) ||
		!reflect.DeepEqual(m.Cc, []gmailAddress{{Name: "<<broken"}}) || m.Subject != "Re: 件名" || !reflect.DeepEqual(m.Labels, []string{"CATEGORY_PERSONAL", "INBOX", "TRASH", "UNREAD"}) ||
		m.Text != strings.Repeat("あ", google.TextLimit) || !m.TextTruncated || m.Date != formatTime(gmailBase+30) {
		t.Fatalf("last message: %+v", m)
	}
	calls = api.g.calls()
	if gmailMethods(calls) != "threads.get 19a0" || calls[0].query.Get("format") != "full" {
		t.Fatalf("thread calls: %s %+v", gmailMethods(calls), calls)
	}
	text, _ = c.call("gmail_thread_get", map[string]any{"thread_id": "19a0"})
	if !strings.Contains(text, `"message_id":"1901e","from":{"name":"Bob","email":"bob@example.com"},"to":[],"cc":[]`) || !strings.Contains(text, `"text_truncated":false`) || !strings.Contains(text, `"omitted_messages":2`) {
		t.Fatalf("thread JSON: %s", text)
	}
	api.g.calls()

	// Errors of thread_get.
	const notFound = "not_found: The thread was not found, or you cannot access it."
	api.g.set(func(f *fakeGmail) {
		f.threads["19b0"] = gmailThreadJSON(t, "19b0", gmailTestMessage{id: "19b1", sec: gmailBase, labels: []string{"DRAFT"}})
		f.threads["19c0"] = `{"id":"19c0"}`
	})
	c.fails("gmail_thread_get", map[string]any{"thread_id": "19b0"}, notFound)
	c.fails("gmail_thread_get", map[string]any{"thread_id": "19c0"}, notFound)
	c.fails("gmail_thread_get", map[string]any{"thread_id": "19d0"}, notFound)
	for _, tc := range []struct {
		failure fakeGmailFailure
		want    string
	}{
		{fakeGmailFailure{status: 400, body: `{}`}, notFound},
		{fakeGmailFailure{status: 429, retryAfter: "30"}, "rate_limited: Gmail rate limit reached. Try again after 2026-10-24T01:02:33Z."},
		{fakeGmailFailure{status: 403, body: `{"error":{"errors":[{"reason":"domainPolicy"}]}}`}, "gmail_policy_blocked: The Google Workspace administrator has not allowed this app."},
		{fakeGmailFailure{status: 500, body: `x`}, unavailable},
		{fakeGmailFailure{status: 302}, unavailable},
	} {
		api.g.set(func(f *fakeGmail) { f.failures = map[string]fakeGmailFailure{"threads.get 19a0": tc.failure} })
		c.fails("gmail_thread_get", map[string]any{"thread_id": "19a0"}, tc.want)
	}
	api.g.set(func(f *fakeGmail) {
		f.failures = map[string]fakeGmailFailure{}
		f.tooLarge["threads.get 19a0"] = true
	})
	c.fails("gmail_thread_get", map[string]any{"thread_id": "19a0"}, "gmail_unavailable: The thread is too large to read.")
	api.g.set(func(f *fakeGmail) { f.tooLarge = map[string]bool{} })
	if calls = api.g.calls(); len(calls) != 9 {
		t.Fatalf("thread error calls: %s", gmailMethods(calls))
	}
	// The errors do not change the account.
	if a := api.account("alice-id", aliceID); a.Status != store.GmailActive || a.LastError.Valid {
		t.Fatalf("account after errors: %+v", a)
	}

	// Invalid arguments are rejected before Google is called.
	const badAccount = "invalid_request: account must be the address of a connected Google account (1-320 characters)."
	const badQuery = "invalid_request: query must be 1-1000 characters and not blank."
	const badPage = "invalid_request: page_token is invalid."
	for _, tc := range []struct {
		tool string
		args any
		want string
	}{
		{"gmail_accounts_list", map[string]any{"account": "x"}, "invalid_request: Invalid arguments."},
		{"gmail_threads_search", nil, "invalid_request: query is required."},
		{"gmail_threads_search", map[string]any{"query": nil}, "invalid_request: query is required."},
		{"gmail_threads_search", map[string]any{"query": " \n "}, badQuery},
		{"gmail_threads_search", map[string]any{"query": strings.Repeat("あ", 1001)}, badQuery},
		{"gmail_threads_search", map[string]any{"query": "a\x00b"}, badQuery},
		{"gmail_threads_search", map[string]any{"query": 1}, badQuery},
		{"gmail_threads_search", map[string]any{"query": "x", "limit": 0}, "invalid_request: limit must be an integer from 1 to 20."},
		{"gmail_threads_search", map[string]any{"query": "x", "limit": 21}, "invalid_request: limit must be an integer from 1 to 20."},
		{"gmail_threads_search", map[string]any{"query": "x", "page_token": ""}, badPage},
		{"gmail_threads_search", map[string]any{"query": "x", "page_token": "a/b"}, badPage},
		{"gmail_threads_search", map[string]any{"query": "x", "page_token": strings.Repeat("a", 513)}, badPage},
		{"gmail_threads_search", map[string]any{"query": "x", "page_token": 1}, badPage},
		{"gmail_threads_search", map[string]any{"query": "x", "account": ""}, badAccount},
		{"gmail_threads_search", map[string]any{"query": "x", "account": " "}, badAccount},
		{"gmail_threads_search", map[string]any{"query": "x", "account": 1}, badAccount},
		{"gmail_threads_search", map[string]any{"query": "x", "account": strings.Repeat("あ", 321)}, badAccount},
		{"gmail_threads_search", map[string]any{"query": "x", "user_id": "bob-id"}, "invalid_request: Invalid arguments."},
		{"gmail_thread_get", map[string]any{}, "invalid_request: thread_id is required."},
		{"gmail_thread_get", map[string]any{"thread_id": nil}, notFound},
		{"gmail_thread_get", map[string]any{"thread_id": 1}, notFound},
		{"gmail_thread_get", map[string]any{"thread_id": "18A0"}, notFound},
		{"gmail_thread_get", map[string]any{"thread_id": "../profile"}, notFound},
		{"gmail_thread_get", map[string]any{"thread_id": "18a0", "account": []any{}}, badAccount},
		{"gmail_thread_get", map[string]any{"thread_id": "18a0", "account": "bob@example.com"}, noAccount},
	} {
		c.fails(tc.tool, tc.args, tc.want)
	}
	if calls = api.g.calls(); len(calls) != 0 {
		t.Fatalf("calls for invalid arguments: %s", gmailMethods(calls))
	}

	// With two accounts, account is required; it is compared ignoring case
	// and surrounding spaces.
	api.connect(api.alice, "alice2@example.com")
	const required = "invalid_request: account is required when more than one Google account is connected; use gmail_accounts_list."
	c.fails("gmail_threads_search", map[string]any{"query": "x"}, required)
	c.fails("gmail_thread_get", map[string]any{"thread_id": "19a0", "account": nil}, required)
	c.fails("gmail_thread_get", map[string]any{"thread_id": "19a0", "account": "nobody@example.com"}, noAccount)
	c.ok("gmail_thread_get", map[string]any{"thread_id": "19a0", "account": " ALICE2@Example.com "}, &got)
	if got.Account != "alice2@example.com" || got.URL != "https://mail.google.com/mail/?authuser=alice2%40example.com#all/19a0" {
		t.Fatalf("second account: %+v", got)
	}
	if calls = api.g.calls(); gmailMethods(calls) != "token 1//refresh-alice2@example.com,threads.get 19a0" {
		t.Fatalf("second account calls: %s", gmailMethods(calls))
	}
	c.ok("gmail_accounts_list", nil, &accounts)
	if len(accounts.Accounts) != 2 || accounts.Accounts[0].Email != "alice2@example.com" || accounts.Accounts[1].Email != gmailAliceAddr {
		t.Fatalf("two accounts: %+v", accounts)
	}

	// An account in the error state is not tried.
	for lastError, want := range map[string]string{
		gmailsync.ErrorTokenInvalid:       "gmail_token_invalid: The connection to the Google account cannot be used. Reconnect it in Astovia's settings.",
		gmailsync.ErrorTokenUndecryptable: "gmail_token_invalid: The connection to the Google account cannot be used. Reconnect it in Astovia's settings.",
		gmailsync.ErrorScopesMissing:      "gmail_scopes_missing: Reading mail is not allowed for the Google account. Reconnect it in Astovia's settings and allow it.",
		gmailsync.ErrorPolicyBlocked:      "gmail_policy_blocked: The Google Workspace administrator has not allowed this app.",
		gmailsync.ErrorUnavailable:        unavailable,
	} {
		api.setState("alice-id", aliceID, store.GmailError, lastError)
		c.fails("gmail_threads_search", map[string]any{"query": "x", "account": gmailAliceAddr}, want)
		c.fails("gmail_thread_get", map[string]any{"thread_id": "19a0", "account": gmailAliceAddr}, want)
	}
	if calls = api.g.calls(); len(calls) != 0 {
		t.Fatalf("calls for the error state: %s", gmailMethods(calls))
	}
	api.setState("alice-id", aliceID, store.GmailError, gmailsync.ErrorUnavailable)
	c.ok("gmail_accounts_list", nil, &accounts)
	if a := accounts.Accounts[1]; a.Status != "error" || a.LastError == nil || *a.LastError != gmailsync.ErrorUnavailable {
		t.Fatalf("account in error: %+v", a)
	}
	// An active account with an error is tried.
	api.setState("alice-id", aliceID, store.GmailActive, gmailsync.ErrorRateLimited)
	c.ok("gmail_thread_get", map[string]any{"thread_id": "19a0", "account": gmailAliceAddr}, &got)
	if calls = api.g.calls(); gmailMethods(calls) != "threads.get 19a0" {
		t.Fatalf("calls for an active account with an error: %s", gmailMethods(calls))
	}

	// Without the OAuth client of the instance, Google is not called. A new
	// client starts its server after the change.
	api.server.cfg.Google = nil
	c = api.client(api.alice)
	c.fails("gmail_thread_get", map[string]any{"thread_id": "19a0", "account": gmailAliceAddr}, "gmail_unavailable: The Google OAuth client is not configured on this server.")
	c.ok("gmail_accounts_list", nil, &accounts)
	if calls = api.g.calls(); len(calls) != 0 {
		t.Fatalf("calls without a client: %s", gmailMethods(calls))
	}
}

func TestMCPGmailToken(t *testing.T) {
	api := newMCPGmailAPI(t, "sqlite")
	setLimit(api.server.gmailMCPLimiter, 1000000)
	id := api.connect(api.alice, gmailAliceAddr)
	c := api.client(api.alice)
	api.g.set(func(f *fakeGmail) { searchThreads(t, f) })
	var got gmailThreadResponse
	get := func() {
		t.Helper()
		c.ok("gmail_thread_get", map[string]any{"thread_id": "18a1"}, &got)
	}
	get()
	if calls := api.g.calls(); gmailMethods(calls) != "token 1//refresh-alice@example.com,threads.get 18a1" || calls[0].form.Get("client_id") != "client-id" {
		t.Fatalf("first calls: %s", gmailMethods(calls))
	}

	// A 401 refreshes the token once and retries.
	api.g.set(func(f *fakeGmail) { f.accessOK = map[string]bool{} })
	get()
	calls := api.g.calls()
	if gmailMethods(calls) != "threads.get 18a1,token 1//refresh-alice@example.com,threads.get 18a1" || calls[0].token == calls[2].token {
		t.Fatalf("401 calls: %s", gmailMethods(calls))
	}
	// A second 401 is an unusable token, and does not change the account.
	const tokenInvalid = "gmail_token_invalid: The connection to the Google account cannot be used. Reconnect it in Astovia's settings."
	api.g.set(func(f *fakeGmail) { f.reject = true })
	c.fails("gmail_thread_get", map[string]any{"thread_id": "18a1"}, tokenInvalid)
	if calls = api.g.calls(); gmailMethods(calls) != "threads.get 18a1,token 1//refresh-alice@example.com,threads.get 18a1" {
		t.Fatalf("second 401 calls: %s", gmailMethods(calls))
	}
	if a := api.account("alice-id", id); a.Status != store.GmailActive {
		t.Fatalf("account after 401: %+v", a)
	}
	api.g.set(func(f *fakeGmail) { f.reject = false })

	// The token is refreshed 2 minutes before it expires.
	get()
	api.g.calls()
	api.clock.Add(3599 - 121)
	get()
	if calls = api.g.calls(); gmailMethods(calls) != "threads.get 18a1" {
		t.Fatalf("before the margin: %s", gmailMethods(calls))
	}
	api.clock.Add(1)
	get()
	if calls = api.g.calls(); gmailMethods(calls) != "token 1//refresh-alice@example.com,threads.get 18a1" {
		t.Fatalf("at the margin: %s", gmailMethods(calls))
	}

	// A refresh token replaced by Google is stored and used next.
	api.g.set(func(f *fakeGmail) {
		f.rotate = true
		f.accessOK = map[string]bool{}
	})
	get()
	rotated := api.storedRefresh("alice-id", id)
	if !strings.HasPrefix(rotated, "1//rotated-") || api.account("alice-id", id).Generation != 1 {
		t.Fatalf("rotated token: %s", rotated)
	}
	api.g.set(func(f *fakeGmail) { f.accessOK = map[string]bool{} })
	get()
	if calls = api.g.calls(); !strings.Contains(gmailMethods(calls), "token "+rotated+",") {
		t.Fatalf("rotated calls: %s", gmailMethods(calls))
	}
	api.g.set(func(f *fakeGmail) { f.rotate = false })

	// Another generation does not use the token in memory. The clock has
	// moved past the session's access token, so alice logs in again.
	api.alice = (&tokenAPI{t: t, handler: api.handler}).login("alice")
	api.connect(api.alice, gmailAliceAddr)
	if api.account("alice-id", id).Generation != 2 {
		t.Fatal("not connected again")
	}
	get()
	if calls = api.g.calls(); gmailMethods(calls) != "token 1//refresh-alice@example.com,threads.get 18a1" {
		t.Fatalf("calls after connecting again: %s", gmailMethods(calls))
	}

	// invalid_grant: when the stored token changed in the meantime (the
	// synchronization replaced it), it is tried once.
	api.clock.Add(3600)
	api.g.set(func(f *fakeGmail) {
		f.tokenHook = func(refresh string) {
			f.mu.Lock()
			f.tokenHook = nil
			delete(f.refreshOK, refresh)
			f.refreshOK["1//from-sync"] = true
			f.mu.Unlock()
			api.storeRefresh("alice-id", id, "1//from-sync")
		}
	})
	get()
	if calls = api.g.calls(); gmailMethods(calls) != "token 1//refresh-alice@example.com,token 1//from-sync,threads.get 18a1" {
		t.Fatalf("invalid_grant calls: %s", gmailMethods(calls))
	}
	// Without a new token, invalid_grant is an unusable token.
	api.clock.Add(3600)
	api.g.set(func(f *fakeGmail) { f.refreshOK = map[string]bool{} })
	c.fails("gmail_thread_get", map[string]any{"thread_id": "18a1"}, tokenInvalid)
	if calls = api.g.calls(); gmailMethods(calls) != "token 1//from-sync" {
		t.Fatalf("invalid_grant without a new token: %s", gmailMethods(calls))
	}
	// In a search, a failed refresh fails the whole search.
	c.fails("gmail_threads_search", map[string]any{"query": "x"}, tokenInvalid)
	// Other answers of the token endpoint.
	for _, tc := range []struct {
		status int
		body   string
		want   string
	}{
		{400, `{"error":"invalid_client"}`, "gmail_unavailable: Gmail could not be reached. Try again later."},
		{400, `{"error":"admin_policy_enforced"}`, "gmail_policy_blocked: The Google Workspace administrator has not allowed this app."},
		{500, `x`, "gmail_unavailable: Gmail could not be reached. Try again later."},
	} {
		api.g.set(func(f *fakeGmail) { f.tokenStatus, f.tokenBody = tc.status, tc.body })
		c.fails("gmail_thread_get", map[string]any{"thread_id": "18a1"}, tc.want)
	}
	api.g.set(func(f *fakeGmail) { f.tokenStatus = 0 })
	// A refresh token that cannot be opened is an unusable token.
	acct := api.account("alice-id", id)
	if err := api.store.GmailSync(context.Background(), "alice-id", id, acct.Generation, func(tx *store.GmailTx) error { return tx.UpdateGmailToken("broken") }); err != nil {
		t.Fatal(err)
	}
	c.fails("gmail_thread_get", map[string]any{"thread_id": "18a1"}, tokenInvalid)
	api.g.calls()

	// A replaced refresh token that cannot be stored because the account
	// was connected again fails the call.
	api.storeRefresh("alice-id", id, "1//refresh-alice@example.com")
	api.g.set(func(f *fakeGmail) {
		f.refreshOK["1//refresh-alice@example.com"] = true
		f.rotate = true
		f.tokenHook = func(string) {
			f.mu.Lock()
			f.tokenHook = nil
			f.mu.Unlock()
			api.connect((&tokenAPI{t: t, handler: api.handler}).login("alice"), gmailAliceAddr)
		}
	})
	c.fails("gmail_thread_get", map[string]any{"thread_id": "18a1"}, "gmail_unavailable: The Google account was connected again or removed. Try again.")
}

// Calls at the same time refresh the token once, even when Google replaces
// the refresh token.
func TestMCPGmailConcurrent(t *testing.T) {
	api := newMCPGmailAPI(t, "sqlite")
	setLimit(api.server.gmailMCPLimiter, 1000000)
	id := api.connect(api.alice, gmailAliceAddr)
	api.g.set(func(f *fakeGmail) {
		searchThreads(t, f)
		f.rotate = true
	})
	c1, c2 := api.client(api.alice), api.client(api.alice)
	var wg sync.WaitGroup
	results := make([]gmailThreadResponse, 2)
	for i, c := range []*mcpClient{c1, c2} {
		wg.Add(1)
		go func(i int, c *mcpClient) {
			defer wg.Done()
			text, isError := c.call("gmail_thread_get", map[string]any{"thread_id": "18a1"})
			if isError {
				t.Errorf("call %d: %s", i, text)
				return
			}
			if err := json.Unmarshal([]byte(text), &results[i]); err != nil {
				t.Error(err)
			}
		}(i, c)
	}
	wg.Wait()
	if t.Failed() {
		return
	}
	if results[0].ThreadID != "18a1" || results[1].ThreadID != "18a1" {
		t.Fatalf("results: %+v", results)
	}
	if calls := api.g.calls(); gmailMethods(calls) != "token 1//refresh-alice@example.com,threads.get 18a1,threads.get 18a1" {
		t.Fatalf("concurrent calls: %s", gmailMethods(calls))
	}
	if v := api.storedRefresh("alice-id", id); !strings.HasPrefix(v, "1//rotated-") {
		t.Fatalf("stored token: %s", v)
	}
}

// The MCP tools use at most gmailMCPUnits quota units per Google account in
// a minute: 10 for threads.list, 40 for each thread of a search and 120 for
// gmail_thread_get.
func TestMCPGmailLimit(t *testing.T) {
	api := newMCPGmailAPI(t, "sqlite")
	id := api.connect(api.alice, gmailAliceAddr)
	bobID := api.connect(api.bob, "bob@example.com")
	api.g.set(func(f *fakeGmail) { searchThreads(t, f) })
	c := api.client(api.alice)
	var found gmailSearchResponse
	found = gmailSearchResponse{}
	c.ok("gmail_threads_search", map[string]any{"query": "x"}, &found)
	if n := api.gmailUnits(id); n != 10+4*40 {
		t.Fatalf("search units: %d", n)
	}
	var got gmailThreadResponse
	c.ok("gmail_thread_get", map[string]any{"thread_id": "18a1"}, &got)
	if n := api.gmailUnits(id); n != 10+4*40+120 {
		t.Fatalf("thread units: %d", n)
	}
	// accounts_list does not call Google.
	var accounts gmailAccountListResponse
	c.ok("gmail_accounts_list", nil, &accounts)
	if n := api.gmailUnits(id); n != 290 {
		t.Fatalf("accounts units: %d", n)
	}
	// Bob's account has its own count.
	api.client(api.bob).ok("gmail_thread_get", map[string]any{"thread_id": "18a1"}, &got)
	if n := api.gmailUnits(bobID); n != 120 {
		t.Fatalf("bob's units: %d", n)
	}
	api.g.calls()

	// Over the limit: the search stops reading threads and is partial; the
	// list itself and gmail_thread_get are refused.
	const limited = "rate_limited: Too many Gmail requests from MCP. Try again in a minute."
	setLimit(api.server.gmailMCPLimiter, 10+40+39)
	found = gmailSearchResponse{}
	c.ok("gmail_threads_search", map[string]any{"query": "x"}, &found)
	if !found.Partial || threadIDsOf(found.Threads) != "18a1,18a2,18a3,18a4" || found.Threads[0].MessageCount == nil || found.Threads[1].MessageCount != nil {
		t.Fatalf("search over the limit: %+v", found)
	}
	if calls := api.g.calls(); gmailMethods(calls) != "threads.list,threads.get 18a1" {
		t.Fatalf("search over the limit calls: %s", gmailMethods(calls))
	}
	c.fails("gmail_thread_get", map[string]any{"thread_id": "18a1"}, limited)
	setLimit(api.server.gmailMCPLimiter, gmailListUnits-1)
	c.fails("gmail_threads_search", map[string]any{"query": "x"}, limited)
	if calls := api.g.calls(); len(calls) != 0 {
		t.Fatalf("calls over the limit: %s", gmailMethods(calls))
	}
	// A retry after a 401 takes the units again.
	setLimit(api.server.gmailMCPLimiter, 2*120-1)
	api.g.set(func(f *fakeGmail) { f.accessOK = map[string]bool{} })
	c.fails("gmail_thread_get", map[string]any{"thread_id": "18a1"}, limited)
	if calls := api.g.calls(); gmailMethods(calls) != "threads.get 18a1" {
		t.Fatalf("retry over the limit calls: %s", gmailMethods(calls))
	}
	setLimit(api.server.gmailMCPLimiter, 2*120)
	c.ok("gmail_thread_get", map[string]any{"thread_id": "18a1"}, &got)
	if n := api.gmailUnits(id); n != 240 {
		t.Fatalf("retry units: %d", n)
	}
}
