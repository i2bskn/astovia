package httpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/i2bskn/astovia/internal/auth"
	"github.com/i2bskn/astovia/internal/config"
	"github.com/i2bskn/astovia/internal/slacksync"
	"github.com/i2bskn/astovia/internal/store"
	"github.com/i2bskn/astovia/internal/testutil"
)

const (
	aliceSlackToken = "xoxp-alice-0123456789"
	bobSlackToken   = "xoxp-bob-0123456789"
)

type fakeSlackRequest struct {
	method string
	query  url.Values
	token  string
}

// fakeSlackFailure replaces an answer: an HTTP status other than 200 (with
// retryAfter as Retry-After on a 429), or "ok": false with code.
type fakeSlackFailure struct {
	status     int
	code       string
	retryAfter string
}

// fakeSlackAPI answers the Slack Web API methods the MCP tools use.
// auth.test identifies alice's token as U1 and bob's as UB. users.info
// answers users, fails for failing and names other members "Name <id>".
// conversations.replies answers replies by ts. failures replace the answer
// of a method, or of "conversations.replies <ts>".
type fakeSlackAPI struct {
	mu        sync.Mutex
	answers   map[string]string
	replies   map[string]string
	users     map[string]string
	failing   map[string]bool
	failures  map[string]fakeSlackFailure
	requests  []fakeSlackRequest
	userCalls []string
}

func (f *fakeSlackAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	method := strings.TrimPrefix(r.URL.Path, "/")
	q := r.URL.Query()
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	f.requests = append(f.requests, fakeSlackRequest{method: method, query: q, token: token})
	w.Header().Set("Content-Type", "application/json")
	if method == "auth.test" {
		w.Header().Set("X-OAuth-Scopes", strings.Join(slacksync.RequiredScopes, ","))
		user, name := "U1", "alice"
		if token == bobSlackToken {
			user, name = "UB", "bob"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "url": "https://acme.slack.com/", "team": "Acme", "user": name, "team_id": "T1", "user_id": user})
		return
	}
	failure, ok := f.failures[method]
	if method == "conversations.replies" {
		if v, found := f.failures[method+" "+q.Get("ts")]; found {
			failure, ok = v, true
		}
	}
	if ok {
		if failure.status != 0 {
			if failure.retryAfter != "" {
				w.Header().Set("Retry-After", failure.retryAfter)
			}
			w.WriteHeader(failure.status)
			return
		}
		_, _ = w.Write([]byte(`{"ok":false,"error":"` + failure.code + `"}`))
		return
	}
	var body string
	switch method {
	case "users.info":
		id := q.Get("user")
		f.userCalls = append(f.userCalls, id)
		if f.failing[id] {
			body = `{"ok":false,"error":"user_not_found"}`
			break
		}
		profile := map[string]any{"display_name": "Name " + id}
		if v, found := f.users[id]; found {
			profile = map[string]any{"display_name": v}
		}
		b, _ := json.Marshal(map[string]any{"ok": true, "user": map[string]any{"id": id, "name": "login-" + id, "profile": profile}})
		body = string(b)
	case "conversations.replies":
		body = f.replies[q.Get("ts")]
		if body == "" {
			body = `{"ok":false,"error":"thread_not_found"}`
		}
	default:
		body = f.answers[method]
		if body == "" {
			body = `{"ok":false,"error":"unknown_method"}`
		}
	}
	_, _ = w.Write([]byte(body))
}

func (f *fakeSlackAPI) set(fn func(f *fakeSlackAPI)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

// calls returns the requests other than auth.test, which only connecting
// sends, and forgets them.
func (f *fakeSlackAPI) calls() []fakeSlackRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakeSlackRequest
	for _, r := range f.requests {
		if r.method != "auth.test" {
			out = append(out, r)
		}
	}
	f.requests, f.userCalls = nil, nil
	return out
}

// methodsOf returns the methods of calls.
func methodsOf(calls []fakeSlackRequest) string {
	var out []string
	for _, c := range calls {
		out = append(out, c.method)
	}
	return strings.Join(out, ",")
}

type mcpSlackAPI struct {
	t       *testing.T
	store   *store.Store
	server  *Server
	handler http.Handler
	sl      *fakeSlackAPI
	alice   string
	bob     string
}

func newMCPSlackAPI(t *testing.T, driver string) *mcpSlackAPI {
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
	now := time.Date(2026, 10, 24, 1, 2, 3, 0, time.UTC)
	a.Now = func() time.Time { return now }
	sl := &fakeSlackAPI{answers: map[string]string{}, replies: map[string]string{}, users: map[string]string{}, failing: map[string]bool{}, failures: map[string]fakeSlackFailure{}}
	srv := httptest.NewServer(sl)
	t.Cleanup(srv.Close)
	server := newServer(config.Config{PublicURL: "https://astovia.test", Secure: true, Secret: make([]byte, 32)}, a, fstest.MapFS{}, nil)
	server.slack.BaseURL = srv.URL
	api := &mcpSlackAPI{t: t, store: s, server: server, handler: server.handler(), sl: sl}
	tok := &tokenAPI{t: t, handler: api.handler}
	api.alice = tok.login("alice")
	api.bob = tok.login("bob")
	return api
}

// connect connects Slack with token through REST.
func (a *mcpSlackAPI) connect(jwt, token string) {
	a.t.Helper()
	w := (&tokenAPI{t: a.t, handler: a.handler}).do("PUT", "/api/v1/integrations/slack", jwt, `{"token":"`+token+`"}`)
	if w.Code != 200 {
		a.t.Fatalf("connect: %d %s", w.Code, w.Body.String())
	}
	a.sl.calls()
}

// client connects an MCP client with a new API token of jwt's user.
func (a *mcpSlackAPI) client(jwt string) *mcpClient {
	a.t.Helper()
	srv := httptest.NewServer(a.handler)
	a.t.Cleanup(srv.Close)
	return connectMCP(a.t, srv, (&tokenAPI{t: a.t, handler: a.handler}).issue(jwt, "mcp").Token, nil)
}

// setLimit changes the limit of l and forgets its counts. It holds l's
// lock, so it may be called while the server runs.
func setLimit(l *loginLimiter, limit int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.limit = limit
	l.entries = map[string]bucket{}
}

func tsOf(messages []slackMessageResponse) string {
	var out []string
	for _, m := range messages {
		out = append(out, m.TS)
	}
	return strings.Join(out, ",")
}

func slackJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// slackIMs is a users.conversations answer with n direct messages with U100,
// U101 and so on.
func slackIMs(t *testing.T, n int) string {
	var channels []any
	for i := 0; i < n; i++ {
		channels = append(channels, map[string]any{"id": fmt.Sprintf("D%d", 100+i), "is_im": true, "user": fmt.Sprintf("U%d", 100+i)})
	}
	return slackJSON(t, map[string]any{"ok": true, "channels": channels})
}

func TestMCPSlack(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) { testMCPSlack(t, driver) })
	}
}

func testMCPSlack(t *testing.T, driver string) {
	api := newMCPSlackAPI(t, driver)
	// The limits are tested in TestMCPSlackLimit.
	for _, l := range []*loginLimiter{api.server.slackSearchLimiter, api.server.slackConversationsLimiter, api.server.slackHistoryLimiter, api.server.slackUsersLimiter} {
		setLimit(l, 1000)
	}
	api.connect(api.bob, bobSlackToken)
	c := api.client(api.alice)

	// Without alice's own connection nothing is sent, even though bob is
	// connected.
	for _, call := range []struct {
		tool string
		args map[string]any
	}{
		{"slack_messages_search", map[string]any{"query": "hi"}},
		{"slack_conversations_list", nil},
		{"slack_messages_list", map[string]any{"channel": "C1"}},
		{"slack_thread_get", map[string]any{"channel": "C1", "ts": "1730000000.000100"}},
	} {
		c.fails(call.tool, call.args, "slack_not_connected: Slack is not connected. Connect it in Astovia's settings.")
	}
	if calls := api.sl.calls(); len(calls) != 0 {
		t.Fatalf("calls without a connection: %+v", calls)
	}
	api.connect(api.alice, aliceSlackToken)

	// messages_search: the query, count and page are passed to Slack; names
	// are resolved (alice's own without Slack) and the text is formatted.
	long := strings.Repeat("あ", 2001)
	api.sl.set(func(f *fakeSlackAPI) {
		f.users["U2"] = "Bob​"
		f.answers["search.messages"] = slackJSON(t, map[string]any{"ok": true, "messages": map[string]any{"matches": []any{
			map[string]any{"ts": "1730000000.000100", "user": "U2", "text": "<@U1> hi <#C9|random> &amp; <https://x.example|x>\nbye", "permalink": "https://acme.slack.com/archives/C1/p1730000000000100", "channel": map[string]any{"id": "C1", "name": "general​"}},
			map[string]any{"ts": "1730000000.000200", "user": "U1", "text": long, "permalink": "https://evil.example/archives/D1/p1730000000000200", "channel": map[string]any{"id": "D1", "is_im": true}},
			map[string]any{"ts": "1730000000.000300", "user": "U3", "bot_id": "B1", "subtype": "bot_message", "text": "<@U2>", "permalink": "https://acme.slack.com/archives/G1/p1730000000000300?thread_ts=1730000000.000100&cid=G1", "channel": map[string]any{"id": "G1", "name": "mpdm-a--b-1", "is_mpim": true}},
			map[string]any{"ts": "1730000000.000400", "text": "", "permalink": "http://acme.slack.com/x", "channel": map[string]any{"id": "bad id", "name": ""}},
		}, "paging": map[string]any{"page": 2, "pages": 5, "total": 93}}})
	})
	var found slackSearchResponse
	c.ok("slack_messages_search", map[string]any{"query": "  in:#general from:<@U2> ", "count": 50, "page": 2}, &found)
	if found.Query != "in:#general from:<@U2>" || found.Page != 2 || found.Pages != 5 || found.Total != 93 || tsOf(found.Messages) != "1730000000.000100,1730000000.000200,1730000000.000300,1730000000.000400" {
		t.Fatalf("search: %+v", found)
	}
	calls := api.sl.calls()
	if methodsOf(calls) != "search.messages,users.info,users.info" || calls[1].query.Get("user") != "U2" || calls[2].query.Get("user") != "U3" {
		t.Fatalf("search calls: %+v", calls)
	}
	q := calls[0].query
	if calls[0].token != aliceSlackToken || q.Get("query") != "in:#general from:<@U2>" || q.Get("count") != "50" || q.Get("page") != "2" || q.Get("sort") != "timestamp" || q.Get("sort_dir") != "desc" {
		t.Fatalf("search query: %+v", calls[0])
	}
	m := found.Messages[0]
	sent := formatTime(1730000000)
	if m.ChannelID != "C1" || m.ChannelName == nil || *m.ChannelName != "#general" || m.UserID != "U2" || m.UserName != "Bob" || m.Text != "@alice(U1) hi #random & x (https://x.example)\nbye" || m.TextTruncated ||
		m.SentAt == nil || *m.SentAt != sent || m.ThreadTS != "" || m.ReplyCount != nil || m.Subtype != "" || m.Bot || m.URL != "https://acme.slack.com/archives/C1/p1730000000000100" {
		t.Fatalf("search message: %+v", m)
	}
	// An invalid permalink is replaced by the URL built from the workspace.
	if m = found.Messages[1]; *m.ChannelName != "DM" || m.UserName != "alice" || !m.TextTruncated || m.Text != strings.Repeat("あ", 2000) || m.URL != "https://acme.slack.com/archives/D1/p1730000000000200" {
		t.Fatalf("search DM: %+v", m.URL)
	}
	// thread_ts comes from the permalink when Slack leaves it out.
	if m = found.Messages[2]; *m.ChannelName != "グループDM" || m.UserName != "Name U3" || m.Text != "@Bob(U2)" || m.ThreadTS != "1730000000.000100" || !m.Bot || m.Subtype != "bot_message" ||
		m.URL != "https://acme.slack.com/archives/G1/p1730000000000300?thread_ts=1730000000.000100&cid=G1" {
		t.Fatalf("search reply: %+v", m)
	}
	if m = found.Messages[3]; m.ChannelID != "" || *m.ChannelName != "" || m.URL != "" || m.UserID != "" || m.UserName != "" || m.Text != "" {
		t.Fatalf("search without a channel: %+v", m)
	}
	// Search results have no reply_count; text_truncated only when cut.
	text, isError := c.call("slack_messages_search", map[string]any{"query": "x"})
	if isError || strings.Contains(text, "reply_count") || strings.Count(text, "text_truncated") != 1 || !strings.Contains(text, `"channel_name":""`) {
		t.Fatalf("search JSON: %s", text)
	}
	api.sl.calls()

	// conversations_list: all types by default, types and names as shown.
	api.sl.set(func(f *fakeSlackAPI) {
		f.failing["U404"] = true
		f.answers["users.conversations"] = slackJSON(t, map[string]any{"ok": true, "channels": []any{
			map[string]any{"id": "C1", "name": "general"},
			map[string]any{"id": "G2", "name": "secret", "is_private": true},
			map[string]any{"id": "D1", "is_im": true, "user": "U2", "is_private": true},
			map[string]any{"id": "D2", "is_im": true, "user": ""},
			map[string]any{"id": "D3", "is_im": true, "user": "U404"},
			map[string]any{"id": "D4", "is_im": true, "user": "U2"},
			map[string]any{"id": "D5", "is_im": true, "user": "U1"},
			map[string]any{"id": "G3", "name": "mpdm-a--b-1", "is_mpim": true, "is_private": true},
			map[string]any{"id": "C4", "name": ""},
			map[string]any{"id": "bad", "name": "x"},
		}, "response_metadata": map[string]any{"next_cursor": "next1"}})
	})
	var convs slackConversationListResponse
	c.ok("slack_conversations_list", nil, &convs)
	want := []slackConversationResponse{
		{ID: "C1", Name: "#general", Type: "public_channel", URL: "https://acme.slack.com/archives/C1"},
		{ID: "G2", Name: "#secret", Type: "private_channel", URL: "https://acme.slack.com/archives/G2"},
		{ID: "D1", Name: "DM（Bob）", Type: "im", URL: "https://acme.slack.com/archives/D1"},
		{ID: "D2", Name: "DM", Type: "im", URL: "https://acme.slack.com/archives/D2"},
		{ID: "D3", Name: "DM", Type: "im", URL: "https://acme.slack.com/archives/D3"},
		{ID: "D4", Name: "DM（Bob）", Type: "im", URL: "https://acme.slack.com/archives/D4"},
		{ID: "D5", Name: "DM（alice）", Type: "im", URL: "https://acme.slack.com/archives/D5"},
		{ID: "G3", Name: "グループDM", Type: "mpim", URL: "https://acme.slack.com/archives/G3"},
		{ID: "C4", Name: "", Type: "public_channel", URL: "https://acme.slack.com/archives/C4"},
		{ID: "bad", Name: "#x", Type: "public_channel", URL: ""},
	}
	if !reflect.DeepEqual(convs.Conversations, want) || convs.NextCursor != "next1" {
		t.Fatalf("conversations: %+v", convs)
	}
	// U2 is asked once; a failed lookup leaves the ID.
	calls = api.sl.calls()
	if methodsOf(calls) != "users.conversations,users.info,users.info" || calls[1].query.Get("user") != "U2" || calls[2].query.Get("user") != "U404" {
		t.Fatalf("conversations calls: %+v", calls)
	}
	q = calls[0].query
	if calls[0].token != aliceSlackToken || q.Get("types") != "public_channel,private_channel,im,mpim" || q.Get("limit") != "100" || q.Has("cursor") || q.Get("exclude_archived") != "true" {
		t.Fatalf("conversations query: %+v", q)
	}
	c.ok("slack_conversations_list", map[string]any{"types": []any{"mpim", "im"}, "limit": 5, "cursor": "abc"}, &convs)
	q = api.sl.calls()[0].query
	if q.Get("types") != "mpim,im" || q.Get("limit") != "5" || q.Get("cursor") != "abc" {
		t.Fatalf("conversations query with arguments: %+v", q)
	}
	// Only the first 20 members are asked from Slack.
	api.sl.set(func(f *fakeSlackAPI) { f.answers["users.conversations"] = slackIMs(t, 25) })
	c.ok("slack_conversations_list", map[string]any{"types": []any{"im"}}, &convs)
	if len(convs.Conversations) != 25 || convs.Conversations[19].Name != "DM（Name U119）" || convs.Conversations[20].Name != "DM" || convs.Conversations[24].Name != "DM" || convs.NextCursor != "" {
		t.Fatalf("25 DMs: %+v", convs)
	}
	if calls = api.sl.calls(); len(calls) != 21 || calls[20].query.Get("user") != "U119" {
		t.Fatalf("25 DMs calls: %d", len(calls))
	}

	// messages_list: Slack's order (newest first), oldest and latest in
	// seconds, reply URLs within their thread.
	api.sl.set(func(f *fakeSlackAPI) {
		f.answers["conversations.history"] = slackJSON(t, map[string]any{"ok": true, "messages": []any{
			map[string]any{"ts": "1730000000.000300", "user": "U2", "text": "<@U3> ok"},
			map[string]any{"ts": "1730000000.000200", "user": "U3", "text": "also", "thread_ts": "1730000000.000100", "subtype": "thread_broadcast"},
			map[string]any{"ts": "1730000000.000100", "bot_id": "B1", "subtype": "bot_message", "text": long, "thread_ts": "1730000000.000100", "reply_count": 3},
			map[string]any{"ts": "bad", "user": "U2", "text": "x"},
		}, "has_more": true, "response_metadata": map[string]any{"next_cursor": "c2"}})
	})
	var list slackMessageListResponse
	c.ok("slack_messages_list", map[string]any{"channel": "C1", "oldest": "2024-10-27T12:00:00+09:00", "latest": "2024-10-28T00:00:00.5Z", "limit": 10, "cursor": "abc"}, &list)
	if list.ChannelID != "C1" || !list.HasMore || list.NextCursor != "c2" || tsOf(list.Messages) != "1730000000.000300,1730000000.000200,1730000000.000100,bad" {
		t.Fatalf("messages: %+v", list)
	}
	calls = api.sl.calls()
	if methodsOf(calls) != "conversations.history,users.info,users.info" {
		t.Fatalf("messages calls: %+v", calls)
	}
	q = calls[0].query
	oldest := time.Date(2024, 10, 27, 3, 0, 0, 0, time.UTC).Unix()
	latest := time.Date(2024, 10, 28, 0, 0, 0, 0, time.UTC).Unix()
	if calls[0].token != aliceSlackToken || q.Get("channel") != "C1" || q.Get("oldest") != strconv.FormatInt(oldest, 10)+".000000" || q.Get("latest") != strconv.FormatInt(latest, 10)+".000000" || q.Get("limit") != "10" || q.Get("cursor") != "abc" {
		t.Fatalf("history query: %+v", q)
	}
	m = list.Messages[0]
	if m.UserName != "Bob" || m.Text != "@Name U3(U3) ok" || m.ReplyCount == nil || *m.ReplyCount != 0 || m.ThreadTS != "" || m.Bot || m.URL != "https://acme.slack.com/archives/C1/p1730000000000300" || m.ChannelName != nil {
		t.Fatalf("message: %+v", m)
	}
	if m = list.Messages[1]; m.Subtype != "thread_broadcast" || m.ThreadTS != "1730000000.000100" || m.URL != "https://acme.slack.com/archives/C1/p1730000000000200?thread_ts=1730000000.000100&cid=C1" {
		t.Fatalf("reply: %+v", m)
	}
	if m = list.Messages[2]; !m.Bot || m.Subtype != "bot_message" || m.UserID != "" || m.UserName != "" || *m.ReplyCount != 3 || !m.TextTruncated || m.Text != strings.Repeat("あ", 2000) || m.URL != "https://acme.slack.com/archives/C1/p1730000000000100" {
		t.Fatalf("parent: %+v", m.URL)
	}
	if m = list.Messages[3]; m.SentAt != nil || m.URL != "" {
		t.Fatalf("invalid ts: %+v", m)
	}
	text, _ = c.call("slack_messages_list", map[string]any{"channel": "C1", "oldest": nil, "latest": nil, "cursor": nil, "limit": nil})
	if !strings.Contains(text, `"sent_at":null`) || !strings.Contains(text, `"reply_count":0`) || strings.Count(text, "text_truncated") != 1 {
		t.Fatalf("messages JSON: %s", text)
	}
	if q = api.sl.calls()[0].query; q.Has("oldest") || q.Has("latest") || q.Has("cursor") || q.Get("limit") != "30" {
		t.Fatalf("history query without arguments: %+v", q)
	}
	// latest may equal oldest.
	c.ok("slack_messages_list", map[string]any{"channel": "C1", "oldest": "2024-10-27T12:00:00+09:00", "latest": "2024-10-27T03:00:00Z"}, &list)
	api.sl.calls()

	// thread_get: the whole thread, oldest first, texts cut at 8000.
	longer := strings.Repeat("い", 8001)
	thread := slackJSON(t, map[string]any{"ok": true, "messages": []any{
		map[string]any{"ts": "1730000000.000100", "user": "U2", "text": longer, "thread_ts": "1730000000.000100", "reply_count": 2},
		map[string]any{"ts": "1730000000.000500", "user": "U1", "text": "<@U2> sure", "thread_ts": "1730000000.000100"},
		map[string]any{"ts": "1730000000.000600", "user": "U3", "text": "ok", "thread_ts": "1730000000.000100"},
	}, "has_more": true, "response_metadata": map[string]any{"next_cursor": "t2"}})
	api.sl.set(func(f *fakeSlackAPI) {
		f.replies["1730000000.000100"] = thread
		// conversations.replies for a reply returns the reply alone.
		f.replies["1730000000.000500"] = slackJSON(t, map[string]any{"ok": true, "messages": []any{map[string]any{"ts": "1730000000.000500", "user": "U1", "text": "sure", "thread_ts": "1730000000.000100"}}})
		f.replies["1730000000.000900"] = slackJSON(t, map[string]any{"ok": true, "messages": []any{map[string]any{"ts": "1730000000.000900", "user": "U2", "text": "alone"}}})
		f.replies["1730000000.000999"] = `{"ok":true,"messages":[]}`
	})
	var got slackThreadResponse
	c.ok("slack_thread_get", map[string]any{"channel": "C1", "ts": "1730000000.000100"}, &got)
	if got.ChannelID != "C1" || got.ThreadTS != "1730000000.000100" || got.Partial || !got.HasMore || got.NextCursor != "t2" || tsOf(got.Messages) != "1730000000.000100,1730000000.000500,1730000000.000600" {
		t.Fatalf("thread: %+v", got)
	}
	if m = got.Messages[0]; !m.TextTruncated || m.Text != strings.Repeat("い", 8000) || *m.ReplyCount != 2 || m.URL != "https://acme.slack.com/archives/C1/p1730000000000100" {
		t.Fatalf("thread parent: %+v", m.URL)
	}
	if m = got.Messages[1]; m.UserName != "alice" || m.Text != "@Bob(U2) sure" || m.URL != "https://acme.slack.com/archives/C1/p1730000000000500?thread_ts=1730000000.000100&cid=C1" {
		t.Fatalf("thread reply: %+v", m)
	}
	calls = api.sl.calls()
	if methodsOf(calls) != "conversations.replies,users.info,users.info" || calls[0].query.Get("ts") != "1730000000.000100" || calls[0].query.Get("limit") != "50" || calls[0].query.Get("channel") != "C1" || calls[0].token != aliceSlackToken {
		t.Fatalf("thread calls: %+v", calls)
	}
	// The ts of a reply reads the thread again from its parent; the cursor
	// is used only by the first call.
	c.ok("slack_thread_get", map[string]any{"channel": "C1", "ts": "1730000000.000500", "limit": 3, "cursor": "abc"}, &got)
	if got.ThreadTS != "1730000000.000100" || got.Partial || tsOf(got.Messages) != "1730000000.000100,1730000000.000500,1730000000.000600" {
		t.Fatalf("thread from a reply: %+v", got)
	}
	calls = api.sl.calls()
	if len(calls) < 2 || methodsOf(calls[:2]) != "conversations.replies,conversations.replies" || calls[0].query.Get("ts") != "1730000000.000500" || calls[0].query.Get("cursor") != "abc" ||
		calls[1].query.Get("ts") != "1730000000.000100" || calls[1].query.Has("cursor") || calls[1].query.Get("limit") != "3" {
		t.Fatalf("thread from a reply calls: %+v", calls)
	}
	// A failed second call returns the first page as partial, except for
	// an unusable token or a missing scope.
	for _, failure := range []fakeSlackFailure{{code: "thread_not_found"}, {status: 429}, {code: "internal_error"}, {status: 500}} {
		api.sl.set(func(f *fakeSlackAPI) {
			f.failures = map[string]fakeSlackFailure{"conversations.replies 1730000000.000100": failure}
		})
		c.ok("slack_thread_get", map[string]any{"channel": "C1", "ts": "1730000000.000500"}, &got)
		if !got.Partial || got.ThreadTS != "1730000000.000100" || tsOf(got.Messages) != "1730000000.000500" || got.Messages[0].Text != "sure" {
			t.Fatalf("partial thread %+v: %+v", failure, got)
		}
	}
	for code, want := range map[string]string{
		"invalid_auth":  "slack_token_invalid: The Slack token cannot be used. Reconnect Slack in Astovia's settings.",
		"missing_scope": "slack_scopes_missing: The Slack token lacks a required scope. Reinstall the Slack app from the manifest and reconnect.",
	} {
		api.sl.set(func(f *fakeSlackAPI) {
			f.failures = map[string]fakeSlackFailure{"conversations.replies 1730000000.000100": {code: code}}
		})
		c.fails("slack_thread_get", map[string]any{"channel": "C1", "ts": "1730000000.000500"}, want)
	}
	api.sl.set(func(f *fakeSlackAPI) { f.failures = map[string]fakeSlackFailure{} })
	// A message outside threads is returned alone; no message is not found.
	c.ok("slack_thread_get", map[string]any{"channel": "C1", "ts": "1730000000.000900"}, &got)
	if got.ThreadTS != "1730000000.000900" || got.Partial || tsOf(got.Messages) != "1730000000.000900" || got.Messages[0].ThreadTS != "" || got.Messages[0].URL != "https://acme.slack.com/archives/C1/p1730000000000900" {
		t.Fatalf("message outside threads: %+v", got)
	}
	text, _ = c.call("slack_thread_get", map[string]any{"channel": "C1", "ts": "1730000000.000900"})
	if !strings.Contains(text, `"partial":false`) {
		t.Fatalf("thread JSON: %s", text)
	}
	const notFound = "not_found: The conversation or message was not found, or you cannot access it."
	c.fails("slack_thread_get", map[string]any{"channel": "C1", "ts": "1730000000.000999"}, notFound)
	api.sl.calls()

	// Slack's errors.
	const unavailable = "slack_unavailable: Slack could not be reached. Try again later."
	const badCursor = "invalid_request: cursor is invalid."
	for _, tc := range []struct {
		failure fakeSlackFailure
		want    string
	}{
		{fakeSlackFailure{code: "invalid_auth"}, "slack_token_invalid: The Slack token cannot be used. Reconnect Slack in Astovia's settings."},
		{fakeSlackFailure{code: "token_revoked"}, "slack_token_invalid: The Slack token cannot be used. Reconnect Slack in Astovia's settings."},
		{fakeSlackFailure{code: "missing_scope"}, "slack_scopes_missing: The Slack token lacks a required scope. Reinstall the Slack app from the manifest and reconnect."},
		{fakeSlackFailure{status: 429, retryAfter: "30"}, "rate_limited: Slack rate limit reached. Try again after 2026-10-24T01:02:33Z."},
		{fakeSlackFailure{status: 429}, "rate_limited: Slack rate limit reached. Try again in a minute."},
		{fakeSlackFailure{code: "channel_not_found"}, notFound},
		{fakeSlackFailure{code: "not_in_channel"}, notFound},
		{fakeSlackFailure{code: "invalid_cursor"}, badCursor},
		{fakeSlackFailure{code: "internal_error"}, unavailable},
		{fakeSlackFailure{code: "something_else"}, unavailable},
		{fakeSlackFailure{status: 500}, unavailable},
	} {
		api.sl.set(func(f *fakeSlackAPI) {
			f.failures = map[string]fakeSlackFailure{"search.messages": tc.failure, "users.conversations": tc.failure, "conversations.history": tc.failure, "conversations.replies": tc.failure}
		})
		c.fails("slack_messages_search", map[string]any{"query": "x"}, tc.want)
		c.fails("slack_conversations_list", nil, tc.want)
		c.fails("slack_messages_list", map[string]any{"channel": "C1"}, tc.want)
		c.fails("slack_thread_get", map[string]any{"channel": "C1", "ts": "1730000000.000100"}, tc.want)
		if calls = api.sl.calls(); len(calls) != 4 {
			t.Fatalf("calls for %+v: %+v", tc.failure, calls)
		}
	}
	api.sl.set(func(f *fakeSlackAPI) {
		f.failures = map[string]fakeSlackFailure{"conversations.history": {code: "invalid_ts_oldest"}}
	})
	c.fails("slack_messages_list", map[string]any{"channel": "C1"}, "invalid_request: oldest and latest must be RFC 3339 date-times with an offset, from 1970 to 9999.")
	// The token check does not change the connection.
	if conn, err := api.store.SlackConnection(context.Background(), "alice-id"); err != nil || conn.Status == store.SlackError {
		t.Fatalf("connection after invalid_auth: %+v %v", conn.Status, err)
	}
	// A failed name lookup never fails the tool.
	api.sl.set(func(f *fakeSlackAPI) {
		f.failures = map[string]fakeSlackFailure{"users.info": {code: "invalid_auth"}}
	})
	c.ok("slack_messages_list", map[string]any{"channel": "C1"}, &list)
	if list.Messages[0].UserName != "U2" || list.Messages[0].Text != "@U3(U3) ok" {
		t.Fatalf("names after a failed lookup: %+v", list.Messages[0])
	}
	api.sl.set(func(f *fakeSlackAPI) { f.failures = map[string]fakeSlackFailure{} })
	api.sl.calls()

	// Invalid arguments are rejected before Slack is called.
	const badQuery = "invalid_request: query must be 1-500 characters and not blank."
	const badTypes = "invalid_request: types must be 1-4 distinct values of public_channel, private_channel, im, mpim."
	const badTime = "invalid_request: oldest and latest must be RFC 3339 date-times with an offset, from 1970 to 9999."
	for _, tc := range []struct {
		tool string
		args any
		want string
	}{
		{"slack_messages_search", map[string]any{}, "invalid_request: query is required."},
		{"slack_messages_search", nil, "invalid_request: query is required."},
		{"slack_messages_search", map[string]any{"query": nil}, "invalid_request: query is required."},
		{"slack_messages_search", map[string]any{"query": " \n "}, badQuery},
		{"slack_messages_search", map[string]any{"query": strings.Repeat("あ", 501)}, badQuery},
		{"slack_messages_search", map[string]any{"query": 1}, badQuery},
		{"slack_messages_search", map[string]any{"query": "x", "count": 0}, "invalid_request: count must be an integer from 1 to 100."},
		{"slack_messages_search", map[string]any{"query": "x", "count": 101}, "invalid_request: count must be an integer from 1 to 100."},
		{"slack_messages_search", map[string]any{"query": "x", "page": 101}, "invalid_request: page must be an integer from 1 to 100."},
		{"slack_messages_search", map[string]any{"query": "x", "page": "1"}, "invalid_request: page must be an integer from 1 to 100."},
		{"slack_messages_search", map[string]any{"query": "x", "user_id": "bob-id"}, "invalid_request: Invalid arguments."},
		{"slack_conversations_list", map[string]any{"types": []any{}}, badTypes},
		{"slack_conversations_list", map[string]any{"types": []any{"im", "im"}}, badTypes},
		{"slack_conversations_list", map[string]any{"types": []any{"channel"}}, badTypes},
		{"slack_conversations_list", map[string]any{"types": []any{nil}}, badTypes},
		{"slack_conversations_list", map[string]any{"types": "im"}, badTypes},
		{"slack_conversations_list", map[string]any{"types": []any{"im", "mpim", "public_channel", "private_channel", "im"}}, badTypes},
		{"slack_conversations_list", map[string]any{"limit": 201}, "invalid_request: limit must be an integer from 1 to 200."},
		{"slack_conversations_list", map[string]any{"limit": 1.5}, "invalid_request: limit must be an integer from 1 to 200."},
		{"slack_conversations_list", map[string]any{"cursor": ""}, badCursor},
		{"slack_conversations_list", map[string]any{"cursor": 1}, badCursor},
		{"slack_conversations_list", map[string]any{"cursor": strings.Repeat("a", 501)}, badCursor},
		{"slack_messages_list", map[string]any{}, "invalid_request: channel is required."},
		{"slack_messages_list", map[string]any{"channel": nil}, notFound},
		{"slack_messages_list", map[string]any{"channel": 1}, notFound},
		{"slack_messages_list", map[string]any{"channel": "c1"}, notFound},
		{"slack_messages_list", map[string]any{"channel": "C1/../x"}, notFound},
		{"slack_messages_list", map[string]any{"channel": "C1", "limit": 101}, "invalid_request: limit must be an integer from 1 to 100."},
		{"slack_messages_list", map[string]any{"channel": "C1", "oldest": "2024-10-27"}, badTime},
		{"slack_messages_list", map[string]any{"channel": "C1", "oldest": "2024-10-27T12:00:00"}, badTime},
		{"slack_messages_list", map[string]any{"channel": "C1", "latest": "1969-12-31T23:59:59Z"}, badTime},
		{"slack_messages_list", map[string]any{"channel": "C1", "latest": 1730000000}, badTime},
		{"slack_messages_list", map[string]any{"channel": "C1", "oldest": "2024-10-27T12:00:00+09:00", "latest": "2024-10-27T02:59:59Z"}, "invalid_request: latest must not be before oldest."},
		{"slack_messages_list", map[string]any{"channel": "C1", "cursor": ""}, badCursor},
		{"slack_thread_get", map[string]any{"channel": "C1"}, "invalid_request: ts is required."},
		{"slack_thread_get", map[string]any{"ts": "1730000000.000100"}, "invalid_request: channel is required."},
		{"slack_thread_get", map[string]any{"channel": "C1", "ts": nil}, notFound},
		{"slack_thread_get", map[string]any{"channel": "C1", "ts": "1730000000"}, notFound},
		{"slack_thread_get", map[string]any{"channel": "C1", "ts": 1730000000.0001}, notFound},
		{"slack_thread_get", map[string]any{"channel": "bad", "ts": "1730000000.000100"}, notFound},
		{"slack_thread_get", map[string]any{"channel": "C1", "ts": "1730000000.000100", "limit": 101}, "invalid_request: limit must be an integer from 1 to 100."},
		{"slack_thread_get", map[string]any{"channel": "C1", "ts": "1730000000.000100", "cursor": ""}, badCursor},
	} {
		c.fails(tc.tool, tc.args, tc.want)
	}
	if calls = api.sl.calls(); len(calls) != 0 {
		t.Fatalf("calls for invalid arguments: %+v", calls)
	}

	// Bob's calls use bob's token.
	api.client(api.bob).ok("slack_conversations_list", map[string]any{"types": []any{"public_channel"}}, &convs)
	if calls = api.sl.calls(); len(calls) == 0 || calls[0].token != bobSlackToken {
		t.Fatalf("bob's calls: %+v", calls)
	}
}

// The MCP tools call each Slack method at most a few times per user in a
// minute; users.info over its limit leaves IDs.
func TestMCPSlackLimit(t *testing.T) {
	api := newMCPSlackAPI(t, "sqlite")
	api.connect(api.alice, aliceSlackToken)
	api.connect(api.bob, bobSlackToken)
	api.sl.set(func(f *fakeSlackAPI) {
		f.answers["search.messages"] = `{"ok":true,"messages":{"matches":[],"paging":{"page":1,"pages":0,"total":0}}}`
		f.answers["users.conversations"] = `{"ok":true,"channels":[]}`
		f.answers["conversations.history"] = `{"ok":true,"messages":[]}`
		f.replies["1730000000.000100"] = `{"ok":true,"messages":[{"ts":"1730000000.000100","text":"x"}]}`
	})
	c := api.client(api.alice)
	const limited = "rate_limited: Too many Slack requests from MCP. Try again in a minute."
	var found slackSearchResponse
	for i := 0; i < 5; i++ {
		c.ok("slack_messages_search", map[string]any{"query": "x"}, &found)
	}
	c.fails("slack_messages_search", map[string]any{"query": "x"}, limited)
	var convs slackConversationListResponse
	for i := 0; i < 10; i++ {
		c.ok("slack_conversations_list", nil, &convs)
	}
	c.fails("slack_conversations_list", nil, limited)
	// conversations.history and conversations.replies share 4.
	var list slackMessageListResponse
	var got slackThreadResponse
	for i := 0; i < 2; i++ {
		c.ok("slack_messages_list", map[string]any{"channel": "C1"}, &list)
		c.ok("slack_thread_get", map[string]any{"channel": "C1", "ts": "1730000000.000100"}, &got)
	}
	c.fails("slack_messages_list", map[string]any{"channel": "C1"}, limited)
	c.fails("slack_thread_get", map[string]any{"channel": "C1", "ts": "1730000000.000100"}, limited)
	if calls := api.sl.calls(); len(calls) != 19 {
		t.Fatalf("calls: %d", len(calls))
	}
	// Bob has his own counts.
	bob := api.client(api.bob)
	bob.ok("slack_messages_search", map[string]any{"query": "x"}, &found)
	bob.ok("slack_messages_list", map[string]any{"channel": "C1"}, &list)
	api.sl.calls()

	// users.info: 30 a minute; over the limit the names stay IDs. Bob's 20
	// lookups do not count for alice.
	api.sl.set(func(f *fakeSlackAPI) { f.answers["users.conversations"] = slackIMs(t, 20) })
	setLimit(api.server.slackConversationsLimiter, 100)
	bob.ok("slack_conversations_list", nil, &convs)
	c.ok("slack_conversations_list", nil, &convs)
	if convs.Conversations[19].Name != "DM（Name U119）" {
		t.Fatalf("first 20 names: %+v", convs.Conversations[19])
	}
	c.ok("slack_conversations_list", nil, &convs)
	if convs.Conversations[9].Name != "DM（Name U109）" || convs.Conversations[10].Name != "DM" || convs.Conversations[19].Name != "DM" {
		t.Fatalf("names over the limit: %+v", convs.Conversations)
	}
	if calls := api.sl.calls(); len(calls) != 3+20+20+10 {
		t.Fatalf("users.info calls: %d", len(calls))
	}

	// The second call of slack_thread_get over the limit returns the first
	// page as partial.
	setLimit(api.server.slackHistoryLimiter, 1)
	api.sl.set(func(f *fakeSlackAPI) {
		f.replies["1730000000.000500"] = `{"ok":true,"messages":[{"ts":"1730000000.000500","text":"reply","thread_ts":"1730000000.000100"}]}`
	})
	c.ok("slack_thread_get", map[string]any{"channel": "C1", "ts": "1730000000.000500"}, &got)
	if !got.Partial || got.ThreadTS != "1730000000.000100" || tsOf(got.Messages) != "1730000000.000500" {
		t.Fatalf("partial over the limit: %+v", got)
	}
	if calls := api.sl.calls(); methodsOf(calls) != "conversations.replies" {
		t.Fatalf("partial calls: %+v", calls)
	}
}
