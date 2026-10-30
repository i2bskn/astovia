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
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/i2bskn/astovia/internal/ai"
	"github.com/i2bskn/astovia/internal/auth"
	"github.com/i2bskn/astovia/internal/config"
	"github.com/i2bskn/astovia/internal/store"
	"github.com/i2bskn/astovia/internal/testutil"
)

// fakeTryAI is Anthropic: GET /v1/models/{model} accepts every model, and
// POST /v1/messages answers with replies in order (500 when they run out),
// keeping the bodies.
type fakeTryAI struct {
	mu      sync.Mutex
	replies []func(w http.ResponseWriter)
	bodies  []string
}

func (f *fakeTryAI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if strings.HasPrefix(r.URL.Path, "/v1/models/") {
		_, _ = w.Write([]byte(`{"id":"x","type":"model","display_name":"x","created_at":"2026-01-01T00:00:00Z"}`))
		return
	}
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
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"api_error","message":"x"}}`))
		return
	}
	reply(w)
}

func (f *fakeTryAI) set(replies ...func(http.ResponseWriter)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replies, f.bodies = replies, nil
}

// requests returns the system prompts and the JSON documents of the user
// messages sent since set.
func (f *fakeTryAI) requests(t *testing.T) ([]string, []map[string]any) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	var systems []string
	var inputs []map[string]any
	for _, raw := range f.bodies {
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
		var in map[string]any
		if err := json.Unmarshal([]byte(doc), &in); err != nil {
			t.Fatal(err)
		}
		systems, inputs = append(systems, body.System[0].Text), append(inputs, in)
	}
	return systems, inputs
}

func tryAIAnswer(v map[string]any) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		out, _ := json.Marshal(v)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "msg", "type": "message", "role": "assistant", "model": "claude-haiku-5-5",
			"content": []map[string]any{{"type": "text", "text": string(out)}}, "stop_reason": "end_turn", "usage": map[string]int{"input_tokens": 1, "output_tokens": 1}})
	}
}

func tryJudged(decision, task, importance, eventStart string) func(http.ResponseWriter) {
	title := ""
	if eventStart != "" {
		title = "打ち合わせ"
	}
	return tryAIAnswer(map[string]any{"decision": decision, "task": task, "summary": "要約 " + decision, "event_title": title, "event_start": eventStart, "event_end": "",
		"topic": "", "same_as": "", "importance": importance})
}

func tryResolved(done bool) func(http.ResponseWriter) {
	return tryAIAnswer(map[string]any{"resolved": done, "reason": "r"})
}

// fakeTryGitHub answers GET /user (octocat) and the issue 7 of acme/web
// with its comment 11, comparing paths without case.
type fakeTryGitHub struct {
	mu    sync.Mutex
	paths []string
}

func (f *fakeTryGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.paths = append(f.paths, r.URL.Path)
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch strings.ToLower(r.URL.Path) {
	case "/user":
		w.Header().Set("X-OAuth-Scopes", "notifications, repo")
		_, _ = w.Write([]byte(`{"login":"octocat"}`))
	case "/repos/acme/web/issues/7":
		_, _ = w.Write([]byte(`{"state":"open","title":"Fix login","html_url":"https://github.com/Acme/Web/issues/7","body":"説明","user":{"login":"bob"},"created_at":"2026-10-23T00:00:00Z","updated_at":"2026-10-23T01:00:00Z"}`))
	case "/repos/acme/web/issues/7/comments":
		_, _ = w.Write([]byte(`[{"id":11,"user":{"login":"bob"},"body":"@octocat 見てください","html_url":"https://github.com/Acme/Web/issues/7#issuecomment-11","created_at":"2026-10-23T02:00:00Z","updated_at":"2026-10-23T02:00:00Z"}]`))
	default:
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"message":"Not Found"}`))
	}
}

type tryAPI struct {
	t       *testing.T
	store   *store.Store
	server  *Server
	handler http.Handler
	now     time.Time
	ai      *fakeTryAI
	gh      *fakeTryGitHub
	sl      *fakeSlackAPI
	cw      *fakeChatworkAPI
	g       *fakeGmail
	alice   string
	bob     string
}

func newTryAPI(t *testing.T, driver string) *tryAPI {
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
	api := &tryAPI{t: t, store: s, now: now, ai: &fakeTryAI{}, gh: &fakeTryGitHub{},
		sl: &fakeSlackAPI{answers: map[string]string{}, replies: map[string]string{}, users: map[string]string{}, failing: map[string]bool{}, failures: map[string]fakeSlackFailure{}},
		cw: &fakeChatworkAPI{rooms: fakeRooms(), messages: map[string]string{}, status: map[string]int{}},
		g:  &fakeGmail{refreshOK: map[string]bool{}, accessOK: map[string]bool{}, threads: map[string]string{}, failures: map[string]fakeGmailFailure{}, tooLarge: map[string]bool{}}}
	server := newServer(config.Config{PublicURL: "https://astovia.test", Secure: true, Secret: make([]byte, 32)}, a, fstest.MapFS{}, nil)
	for _, h := range []struct {
		handler http.Handler
		set     func(base string)
	}{
		{api.ai, func(base string) { server.ai.BaseURL = base }},
		{api.gh, func(base string) { server.github.BaseURL = base }},
		{api.sl, func(base string) { server.slack.BaseURL = base }},
		{api.cw, func(base string) { server.chatwork.BaseURL = base }},
		{api.g, func(base string) {
			server.google.TokenEndpoint, server.google.RevokeEndpoint, server.google.GmailBaseURL = base+"/token", base+"/revoke", base+"/gmail"
		}},
	} {
		srv := httptest.NewServer(h.handler)
		t.Cleanup(srv.Close)
		h.set(srv.URL)
	}
	server.cfg.Google = &config.GoogleClient{ID: "client-id", Secret: "client-secret", RedirectURI: "https://astovia.test/integrations/google/callback"}
	// The limits are tested on their own.
	for _, l := range []*loginLimiter{server.aiTryLimiter, server.slackHistoryLimiter, server.slackUsersLimiter, server.chatworkMCPLimiter} {
		setLimit(l, 1000)
	}
	api.server, api.handler = server, server.handler()
	tok := &tokenAPI{t: t, handler: api.handler}
	api.alice = tok.login("alice")
	api.bob = tok.login("bob")
	return api
}

func (a *tryAPI) do(method, path, bearer, body string) *httptest.ResponseRecorder {
	a.t.Helper()
	return (&tokenAPI{t: a.t, handler: a.handler}).do(method, path, bearer, body)
}

func (a *tryAPI) put(path, jwt, body string) {
	a.t.Helper()
	if w := a.do("PUT", path, jwt, body); w.Code != 200 {
		a.t.Fatalf("PUT %s: %d %s", path, w.Code, w.Body.String())
	}
}

func (a *tryAPI) connectAI(jwt string) {
	a.t.Helper()
	a.put("/api/v1/ai", jwt, `{"api_key":"sk-ant-api03-0123456789abcdef","model":"claude-haiku-5-5"}`)
}

// connectGmail connects the Google account email for jwt's user, as
// mcpGmailAPI.connect does.
func (a *tryAPI) connectGmail(jwt, email string) string {
	a.t.Helper()
	a.g.set(func(f *fakeGmail) { f.email = email })
	w := a.do("POST", "/api/v1/integrations/gmail/authorize", jwt, `{}`)
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
	w = a.do("POST", "/api/v1/integrations/gmail/complete", jwt, string(body))
	var out gmailAccountResponse
	if err = json.Unmarshal(w.Body.Bytes(), &out); w.Code != 200 || err != nil {
		a.t.Fatalf("complete: %d %s", w.Code, w.Body.String())
	}
	a.g.calls()
	return out.ID
}

// tryItem is alice's open, tracked item of source with key, detected
// detectedAgo seconds before now.
func (a *tryAPI) tryItem(id, source, key string, detectedAgo int64, refs string) store.ActionItem {
	at := a.now.Unix() - detectedAgo
	if refs == "" {
		refs = "[]"
	}
	return store.ActionItem{ID: id, UserID: "alice-id", Content: "task " + id, Status: "open", Refs: refs, Version: 1, CreatedAt: at, UpdatedAt: at,
		Source: sql.NullString{String: source, Valid: true}, SourceKey: sql.NullString{String: key, Valid: true}, Tracking: 1, DetectedAt: sql.NullInt64{Int64: at, Valid: true},
		Kind: store.KindTask, Importance: store.ImportanceNormal}
}

func (a *tryAPI) create(items ...store.ActionItem) {
	a.t.Helper()
	for _, item := range items {
		if err := a.store.CreateActionItem(context.Background(), item); err != nil {
			a.t.Fatalf("create %s: %v", item.ID, err)
		}
	}
}

func (a *tryAPI) samples(scope string) []trySampleResponse {
	a.t.Helper()
	w := a.do("GET", "/api/v1/ai/instructions/samples?scope="+url.QueryEscape(scope), a.alice, "")
	var out trySamplesResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); w.Code != 200 || err != nil {
		a.t.Fatalf("samples %s: %d %s", scope, w.Code, w.Body.String())
	}
	return out.Samples
}

func sampleIDs(list []trySampleResponse) string {
	var ids []string
	for _, s := range list {
		ids = append(ids, s.ID)
	}
	return strings.Join(ids, ",")
}

func tryBody(scope, itemID, common, instruction string) string {
	b, _ := json.Marshal(map[string]string{"scope": scope, "item_id": itemID, "common": common, "instruction": instruction})
	return string(b)
}

func (a *tryAPI) try(scope, itemID, common, instruction string) *httptest.ResponseRecorder {
	a.t.Helper()
	return a.do("POST", "/api/v1/ai/instructions/try", a.alice, tryBody(scope, itemID, common, instruction))
}

// tryOK tries and decodes the response.
func (a *tryAPI) tryOK(scope, itemID, common, instruction string, out any) {
	a.t.Helper()
	w := a.try(scope, itemID, common, instruction)
	if w.Code != 200 {
		a.t.Fatalf("try %s %s: %d %s", scope, itemID, w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
		a.t.Fatal(err)
	}
}

// instructionsTail is the end of a system prompt with the instructions.
func instructionsTail(common, process string) string {
	return "<user_instructions>\n<common>\n" + common + "\n</common>\n<process>\n" + process + "\n</process>\n</user_instructions>"
}

// unchanged checks that a try changed nothing: the items, the AI settings
// and the notifications.
func (a *tryAPI) unchanged(before []store.ActionItem, settings store.AISettings) {
	a.t.Helper()
	ctx := context.Background()
	for _, item := range before {
		got, err := a.store.ActionItem(ctx, item.UserID, item.ID)
		if err != nil || !reflect.DeepEqual(got, item) {
			a.t.Fatalf("item %s changed:\n%+v\n%+v %v", item.ID, got, item, err)
		}
	}
	cur, err := a.store.AISettings(ctx, "alice-id")
	if err != nil || !reflect.DeepEqual(cur, settings) {
		a.t.Fatalf("AI settings changed: %+v %v", cur, err)
	}
	if list, err := a.store.DueNotifications(ctx, a.now.Unix()+1<<30, 100); err != nil || len(list) != 0 {
		a.t.Fatalf("notifications: %+v %v", list, err)
	}
}

func (a *tryAPI) stored(ids ...string) []store.ActionItem {
	a.t.Helper()
	var out []store.ActionItem
	for _, id := range ids {
		item, err := a.store.ActionItem(context.Background(), "alice-id", id)
		if err != nil {
			a.t.Fatal(err)
		}
		out = append(out, item)
	}
	return out
}

func (a *tryAPI) aiSettings() store.AISettings {
	a.t.Helper()
	s, err := a.store.AISettings(context.Background(), "alice-id")
	if err != nil {
		a.t.Fatal(err)
	}
	return s
}

const (
	tryTS         = "1729700000.000100"
	tryMine       = "1729700100.000100"
	tryMentionKey = "slack:T1:m:C1:-:" + tryTS
)

// slackHistory is a conversations.history answer, newest first.
func slackHistory(t *testing.T, messages ...map[string]any) string {
	return slackJSON(t, map[string]any{"ok": true, "messages": messages, "has_more": false})
}

type tryDetect struct {
	Current tryDetectResponse `json:"current"`
	Draft   tryDetectResponse `json:"draft"`
}

type tryReply struct {
	Current tryReplyResponse `json:"current"`
	Draft   tryReplyResponse `json:"draft"`
}

func TestAIInstructionTry(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) { testAIInstructionTry(t, driver) })
	}
}

func testAIInstructionTry(t *testing.T, driver string) {
	api := newTryAPI(t, driver)
	const try = "/api/v1/ai/instructions/try"
	refs := `[{"service":"slack","account":"Acme","conversation_id":"C1","conversation_name":"#general","message_id":"` + tryTS + `"}]`
	mention := api.tryItem("mention", store.SourceSlack, tryMentionKey, 100, refs)
	// A task of the mention's thread is a related task.
	sibling := api.tryItem("sibling", store.SourceSlack, "slack:T1:m:C1:"+tryTS+":1729700050.000100", 50, "")
	tracked := api.tryItem("tracked", store.SourceSlack, "slack:T1:m:C1:-:1729700001.000100", 200, refs)
	tracked.TrackedAt = sql.NullInt64{Int64: api.now.Unix() - 60, Valid: true}
	fallback := api.tryItem("fallback", store.SourceSlack, "slack:T1:m:C1:-:1729700002.000100", 10, "")
	fallback.Content = "Slack を確認: hello"
	group := api.tryItem("group", store.SourceSlack, "slack:T1:g:C1:1729700003.000100:mention", 10, "")
	otherTeam := api.tryItem("otherteam", store.SourceSlack, "slack:T9:m:C1:-:"+tryTS, 10, "")
	bobs := api.tryItem("bobs", store.SourceSlack, tryMentionKey+"x", 10, "")
	bobs.UserID, bobs.SourceKey.String = "bob-id", tryMentionKey
	api.create(mention, sibling, tracked, fallback, group, otherTeam, bobs)

	// The body is checked first; common is not a scope that can be tried.
	for _, body := range []string{
		`{}`,
		`{"scope":"slack.detect","item_id":"mention","common":""}`,
		`{"scope":"slack.detect","item_id":"mention","common":"","instruction":null}`,
		`{"scope":"slack.detect","item_id":"mention","common":1,"instruction":""}`,
		`{"scope":null,"item_id":"mention","common":"","instruction":""}`,
		`{"scope":"slack.detect","item_id":"mention","common":"","instruction":"","other":""}`,
		tryBody("common", "mention", "", ""),
		tryBody("slack", "mention", "", ""),
		tryBody("slack.detect", "mention", strings.Repeat("あ", store.MaxInstructionLength+1), ""),
		tryBody("slack.detect", "mention", "", strings.Repeat("𠮷", store.MaxInstructionLength+1)),
	} {
		expectStatus(t, api.do("POST", try, api.alice, body), 400, "invalid_request")
	}
	for _, scope := range []string{"", "common", "x"} {
		expectStatus(t, api.do("GET", "/api/v1/ai/instructions/samples?scope="+scope, api.alice, ""), 400, "invalid_request")
	}

	// Without the connection, whatever the item.
	for _, scope := range []string{store.InstructionGitHubDetect, store.InstructionSlackDetect, store.InstructionChatworkDetect, store.InstructionGmailReply} {
		expectStatus(t, api.try(scope, "mention", "", ""), 409, "not_connected")
		expectStatus(t, api.do("GET", "/api/v1/ai/instructions/samples?scope="+scope, api.alice, ""), 409, "not_connected")
	}
	api.put("/api/v1/integrations/slack", api.alice, `{"token":"`+aliceSlackToken+`"}`)

	// Items that cannot be tried are not found: missing, another user's, a
	// confirmation task, a group parent, another workspace, not a reply.
	for _, id := range []string{"missing", "bobs", "fallback", "group", "otherteam", "bad id!"} {
		expectStatus(t, api.try(store.InstructionSlackDetect, id, "", ""), 404, "not_found")
	}
	expectStatus(t, api.try(store.InstructionSlackReply, "mention", "", ""), 404, "not_found")

	// Without active AI settings.
	expectStatus(t, api.try(store.InstructionSlackDetect, "mention", "", ""), 409, "ai_not_ready")
	api.connectAI(api.alice)
	api.put("/api/v1/ai/instructions", api.alice, instructionsBody(t, map[string]string{store.InstructionCommon: "保存した共通", store.InstructionSlackDetect: "保存したSlack", store.InstructionSlackReply: "保存した返信"}))
	settings := api.aiSettings()

	// Slack detection outside a thread: the message and the ones around
	// it, judged with the saved and then the draft instructions. The
	// related tasks leave out the item itself.
	api.sl.set(func(f *fakeSlackAPI) {
		f.answers["conversations.history"] = slackHistory(t,
			map[string]any{"type": "message", "user": "U3", "text": "後の発言", "ts": "1729700010.000100"},
			map[string]any{"type": "message", "user": "U2", "text": "<@U1> 資料を見てください", "ts": tryTS},
			map[string]any{"type": "message", "user": "U3", "text": "前の発言", "ts": "1729699990.000100"})
	})
	api.sl.calls()
	before := api.stored("mention", "sibling", "tracked")
	api.ai.set(tryJudged(ai.DecisionTask, "資料を見る", ai.ImportanceNormal, ""), tryJudged(ai.DecisionCandidate, "資料を確認する", ai.ImportanceHigh, "2026-10-29T12:30"))
	draft := strings.Repeat("𠮷", store.MaxInstructionLength)
	var detect tryDetect
	api.tryOK(store.InstructionSlackDetect, "mention", " 書きかけの共通 ", draft, &detect)
	want := tryDetect{
		Current: tryDetectResponse{Decision: ai.DecisionTask, Task: "資料を見る", Importance: ai.ImportanceNormal, Summary: "要約 task"},
		Draft:   tryDetectResponse{Decision: ai.DecisionCandidate, Task: "資料を確認する", Importance: ai.ImportanceHigh, Summary: "要約 candidate", Event: &tryEventResponse{Title: "打ち合わせ", Start: "2026-10-29T12:30"}},
	}
	if !reflect.DeepEqual(detect, want) {
		t.Fatalf("slack detect:\n%+v\n%+v", detect, want)
	}
	systems, inputs := api.ai.requests(t)
	if len(systems) != 2 || !strings.HasPrefix(systems[0], ai.SlackSystemPrompt) || !strings.HasSuffix(systems[0], instructionsTail("保存した共通", "保存したSlack")) ||
		!strings.HasSuffix(systems[1], instructionsTail("書きかけの共通", draft)) {
		t.Fatalf("systems: %q", systems)
	}
	if !reflect.DeepEqual(inputs[0], inputs[1]) {
		t.Fatalf("inputs differ:\n%v\n%v", inputs[0], inputs[1])
	}
	in := inputs[0]
	related, _ := in["related_tasks"].([]any)
	if in["conversation_type"] != "channel" || in["channel_name"] != "general" || len(related) != 1 || related[0].(map[string]any)["task"] != "task sibling" || len(in["posts"].([]any)) != 3 {
		t.Fatalf("slack input: %v", in)
	}
	var targets []string
	for _, p := range in["posts"].([]any) {
		if post := p.(map[string]any); post["target"] == true {
			targets = append(targets, post["id"].(string))
		}
	}
	if len(targets) != 1 || targets[0] != tryTS {
		t.Fatalf("targets: %v", targets)
	}
	if got := methodsOf(api.sl.calls()); strings.Count(got, "conversations.history") != 3 {
		t.Fatalf("slack calls: %s", got)
	}
	api.unchanged(before, settings)

	// The target message is gone.
	api.sl.set(func(f *fakeSlackAPI) {
		f.answers["conversations.history"] = slackHistory(t, map[string]any{"type": "message", "user": "U3", "text": "後の発言", "ts": "1729700010.000100"})
	})
	api.ai.set()
	expectStatus(t, api.try(store.InstructionSlackDetect, "mention", "", ""), 409, "source_unavailable")
	// Slack fails, refuses the conversation or limits the calls.
	api.sl.set(func(f *fakeSlackAPI) { f.failures["conversations.history"] = fakeSlackFailure{status: 500} })
	expectStatus(t, api.try(store.InstructionSlackDetect, "mention", "", ""), 502, "upstream_error")
	api.sl.set(func(f *fakeSlackAPI) {
		f.failures["conversations.history"] = fakeSlackFailure{code: "channel_not_found"}
	})
	expectStatus(t, api.try(store.InstructionSlackDetect, "mention", "", ""), 409, "source_unavailable")
	api.sl.set(func(f *fakeSlackAPI) {
		f.failures["conversations.history"] = fakeSlackFailure{status: 429, retryAfter: "30"}
	})
	w := api.try(store.InstructionSlackDetect, "mention", "", "")
	expectStatus(t, w, 429, "rate_limited")
	if w.Header().Get("Retry-After") != "30" {
		t.Fatalf("Retry-After: %q", w.Header().Get("Retry-After"))
	}
	api.sl.set(func(f *fakeSlackAPI) { f.failures["conversations.history"] = fakeSlackFailure{code: "invalid_auth"} })
	expectStatus(t, api.try(store.InstructionSlackDetect, "mention", "", ""), 409, "not_connected")
	api.sl.set(func(f *fakeSlackAPI) { delete(f.failures, "conversations.history") })
	// The calls of a try are taken at once from the MCP tools' limit.
	setLimit(api.server.slackHistoryLimiter, 2)
	api.sl.calls()
	w = api.try(store.InstructionSlackDetect, "mention", "", "")
	expectStatus(t, w, 429, "rate_limited")
	if w.Header().Get("Retry-After") != "60" || len(api.sl.calls()) != 0 {
		t.Fatalf("Slack limit: %q", w.Header().Get("Retry-After"))
	}
	setLimit(api.server.slackHistoryLimiter, 1000)

	// Slack reply: the user's later message is the reply.
	api.sl.set(func(f *fakeSlackAPI) {
		f.answers["conversations.history"] = slackHistory(t,
			map[string]any{"type": "message", "user": "U1", "text": "送りました", "ts": tryMine},
			map[string]any{"type": "message", "user": "U2", "text": "<@U1> 資料を送ってください", "ts": "1729700001.000100"})
	})
	api.ai.set(tryResolved(true), tryResolved(false))
	before = api.stored("tracked")
	var reply tryReply
	api.tryOK(store.InstructionSlackReply, "tracked", "", "返信の書きかけ", &reply)
	if !reply.Current.Done || reply.Draft.Done {
		t.Fatalf("slack reply: %+v", reply)
	}
	systems, inputs = api.ai.requests(t)
	if len(systems) != 2 || !strings.HasPrefix(systems[0], ai.ReplySystemPrompt) || !strings.HasSuffix(systems[0], instructionsTail("保存した共通", "保存した返信")) ||
		!strings.HasSuffix(systems[1], "<user_instructions>\n<process>\n返信の書きかけ\n</process>\n</user_instructions>") || inputs[0]["task"] != "task tracked" || len(inputs[0]["posts"].([]any)) != 2 {
		t.Fatalf("reply: %q %v", systems, inputs)
	}
	api.unchanged(before, settings)
	// Without a reply of the user.
	api.sl.set(func(f *fakeSlackAPI) {
		f.answers["conversations.history"] = slackHistory(t, map[string]any{"type": "message", "user": "U2", "text": "<@U1> 資料を送ってください", "ts": "1729700001.000100"})
	})
	api.ai.set()
	expectStatus(t, api.try(store.InstructionSlackReply, "tracked", "", ""), 409, "no_reply")

	// GitHub: the issue is read again with the names of the ref.
	refsGH := `[{"service":"github","account":"octocat","conversation_id":"Acme/Web#7","url":"https://github.com/Acme/Web/issues/7"}]`
	gh := api.tryItem("gh", store.SourceGitHub, "github:mention:acme/web#7:c11", 30, refsGH)
	gh.Tracking = 0
	ghGone := api.tryItem("ghgone", store.SourceGitHub, "github:mention:acme/web#8:c12", 30, "")
	ghGone.Tracking = 0
	review := api.tryItem("review", store.SourceGitHub, "github:review:acme/web#7", 30, "")
	api.create(gh, ghGone, review)
	api.put("/api/v1/integrations/github", api.alice, `{"token":"ghp_0123456789abcdefWXYZ"}`)
	api.ai.set(tryJudged(ai.DecisionNone, "", ai.ImportanceNormal, ""), tryJudged(ai.DecisionTask, "ログインを直す", ai.ImportanceLow, ""))
	before = api.stored("gh")
	api.tryOK(store.InstructionGitHubDetect, "gh", "", "", &detect)
	if detect.Current.Decision != ai.DecisionNone || detect.Current.Summary != "要約 none" || detect.Draft.Task != "ログインを直す" || detect.Draft.Importance != ai.ImportanceLow {
		t.Fatalf("github: %+v", detect)
	}
	systems, inputs = api.ai.requests(t)
	if len(systems) != 2 || !strings.HasSuffix(systems[0], "<common>\n保存した共通\n</common>\n</user_instructions>") || systems[1] != ai.SystemPrompt ||
		inputs[0]["repository"] != "Acme/Web" || inputs[0]["number"] != float64(7) {
		t.Fatalf("github AI: %q %v", systems, inputs)
	}
	api.gh.mu.Lock()
	paths := strings.Join(api.gh.paths, ",")
	api.gh.mu.Unlock()
	if !strings.Contains(paths, "/repos/Acme/Web/issues/7/comments") {
		t.Fatalf("github paths: %s", paths)
	}
	api.unchanged(before, settings)
	expectStatus(t, api.try(store.InstructionGitHubDetect, "ghgone", "", ""), 409, "source_unavailable")
	expectStatus(t, api.try(store.InstructionGitHubDetect, "review", "", ""), 404, "not_found")

	// Chatwork: the room's latest messages.
	api.put("/api/v1/integrations/chatwork", api.alice, `{"token":"`+aliceChatworkToken+`"}`)
	api.cw.set(func(f *fakeChatworkAPI) {
		f.messages["3"] = `[{"message_id":"301","account":{"account_id":7,"name":"Bob"},"body":"[To:101] 見積もりをお願いします","send_time":5001,"update_time":0},` +
			`{"message_id":"302","account":{"account_id":8,"name":"Carol"},"body":"了解です","send_time":5002,"update_time":0}]`
	})
	cw := api.tryItem("cw", store.SourceChatwork, "chatwork:101:m:3:301", 40, "")
	cwGone := api.tryItem("cwgone", store.SourceChatwork, "chatwork:101:m:99:301", 40, "")
	task := api.tryItem("cwtask", store.SourceChatwork, "chatwork:101:t:5", 40, "")
	api.create(cw, cwGone, task)
	api.ai.set(tryJudged(ai.DecisionTask, "見積もりを送る", ai.ImportanceNormal, ""), tryJudged(ai.DecisionTask, "見積もりを送る", ai.ImportanceHigh, ""))
	before = api.stored("cw")
	api.tryOK(store.InstructionChatworkDetect, "cw", "", "Chatworkの書きかけ", &detect)
	if detect.Current.Importance != ai.ImportanceNormal || detect.Draft.Importance != ai.ImportanceHigh {
		t.Fatalf("chatwork: %+v", detect)
	}
	_, inputs = api.ai.requests(t)
	if inputs[0]["channel_name"] != "Sales Team" || inputs[0]["conversation_type"] != "channel" || len(inputs[0]["posts"].([]any)) != 2 {
		t.Fatalf("chatwork input: %v", inputs[0])
	}
	api.unchanged(before, settings)
	api.ai.set()
	expectStatus(t, api.try(store.InstructionChatworkDetect, "cwgone", "", ""), 409, "source_unavailable")
	expectStatus(t, api.try(store.InstructionChatworkDetect, "cwtask", "", ""), 404, "not_found")
	api.cw.set(func(f *fakeChatworkAPI) { f.status["/rooms/3/messages"] = 500 })
	expectStatus(t, api.try(store.InstructionChatworkDetect, "cw", "", ""), 502, "upstream_error")
	api.cw.set(func(f *fakeChatworkAPI) { f.status["/rooms/3/messages"] = 401 })
	expectStatus(t, api.try(store.InstructionChatworkDetect, "cw", "", ""), 409, "not_connected")
	api.cw.set(func(f *fakeChatworkAPI) { delete(f.status, "/rooms/3/messages") })

	// Gmail: the thread in the account of the key.
	api.connectGmail(api.alice, "alice@example.com")
	api.g.set(func(f *fakeGmail) {
		f.threads["a1"] = gmailThreadJSON(t, "a1", gmailTestMessage{id: "b1", sec: 1729700000, headers: map[string]string{"From": "Bob <bob@example.net>", "To": "alice@example.com", "Subject": "請求書"}, text: "確認をお願いします"})
	})
	mail := api.tryItem("mail", store.SourceGmail, "gmail:alice@example.com:r:a1:b1:b1", 20, "")
	mailGone := api.tryItem("mailgone", store.SourceGmail, "gmail:alice@example.com:r:a2:b2:b2", 20, "")
	api.create(mail, mailGone)
	api.ai.set(tryJudged(ai.DecisionTask, "請求書を確認する", ai.ImportanceNormal, ""), tryJudged(ai.DecisionNone, "", ai.ImportanceNormal, ""))
	before = api.stored("mail")
	api.tryOK(store.InstructionGmailDetect, "mail", "", "", &detect)
	if detect.Current.Task != "請求書を確認する" || detect.Draft.Decision != ai.DecisionNone {
		t.Fatalf("gmail: %+v", detect)
	}
	if _, inputs = api.ai.requests(t); inputs[0]["channel_name"] != "請求書" || inputs[0]["conversation_type"] != ai.MailThread {
		t.Fatalf("gmail input: %v", inputs[0])
	}
	api.unchanged(before, settings)
	api.ai.set()
	expectStatus(t, api.try(store.InstructionGmailDetect, "mailgone", "", ""), 409, "source_unavailable")

	// The AI fails: nothing is recorded on the settings.
	api.ai.set(tryJudged(ai.DecisionTask, "x", ai.ImportanceNormal, ""))
	expectStatus(t, api.try(store.InstructionGmailDetect, "mail", "", ""), 502, "ai_error")
	api.ai.set(func(w http.ResponseWriter) {
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"x"}}`))
	})
	expectStatus(t, api.try(store.InstructionGmailDetect, "mail", "", ""), 409, "ai_key_invalid")
	api.unchanged(before, settings)

	// Other users see nothing of alice's.
	w = api.do("POST", try, api.bob, tryBody(store.InstructionSlackDetect, "mention", "", ""))
	expectStatus(t, w, 409, "not_connected")

	// At most 5 tries a minute per user.
	setLimit(api.server.aiTryLimiter, 5)
	for i := 0; i < 5; i++ {
		expectStatus(t, api.try(store.InstructionSlackDetect, "missing", "", ""), 404, "not_found")
	}
	w = api.try(store.InstructionSlackDetect, "missing", "", "")
	expectStatus(t, w, 429, "rate_limited")
	if w.Header().Get("Retry-After") != "60" {
		t.Fatalf("Retry-After: %q", w.Header().Get("Retry-After"))
	}

	// Unauthenticated requests and API tokens are rejected.
	expectStatus(t, api.do("POST", try, "", tryBody(store.InstructionSlackDetect, "mention", "", "")), 401, "unauthorized")
	expectStatus(t, api.do("GET", "/api/v1/ai/instructions/samples?scope=slack.detect", "", ""), 401, "unauthorized")
	tok := (&tokenAPI{t: t, handler: api.handler}).issue(api.alice, "x").Token
	expectStatus(t, api.do("POST", try, tok, tryBody(store.InstructionSlackDetect, "mention", "", "")), 401, "unauthorized")
}

// sessionID returns the session of an access token, from its sid claim.
func sessionID(t *testing.T, jwt string) string {
	t.Helper()
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("token: %s", jwt)
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims struct {
		SID string `json:"sid"`
	}
	if err = json.Unmarshal(raw, &claims); err != nil || claims.SID == "" {
		t.Fatalf("claims: %s %v", raw, err)
	}
	return claims.SID
}

func TestAIInstructionTryCoolingDown(t *testing.T) {
	api := newTryAPI(t, "sqlite")
	api.put("/api/v1/integrations/slack", api.alice, `{"token":"`+aliceSlackToken+`"}`)
	api.create(api.tryItem("mention", store.SourceSlack, tryMentionKey, 100, ""))
	ctx := context.Background()
	a, err := api.store.SaveAISettings(ctx, store.AISettings{UserID: "alice-id", Provider: ai.ProviderOpenAI, Model: "gpt-5", APIKeySealed: "sealed"}, 0, true, sessionID(t, api.alice), api.now.Unix())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = api.store.SetAIStatus(ctx, "alice-id", a.Generation, store.AIActive, sql.NullString{String: "usage_limit", Valid: true}, api.now.Unix()); err != nil {
		t.Fatal(err)
	}
	expectStatus(t, api.try(store.InstructionSlackDetect, "mention", "", ""), 409, "ai_cooling_down")
	if _, err = api.store.SetAIStatus(ctx, "alice-id", a.Generation, store.AIError, sql.NullString{String: "key_invalid", Valid: true}, api.now.Unix()); err != nil {
		t.Fatal(err)
	}
	expectStatus(t, api.try(store.InstructionSlackDetect, "mention", "", ""), 409, "ai_not_ready")
}

func TestAIInstructionSamples(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) { testAIInstructionSamples(t, driver) })
	}
}

func testAIInstructionSamples(t *testing.T, driver string) {
	api := newTryAPI(t, driver)
	api.put("/api/v1/integrations/slack", api.alice, `{"token":"`+aliceSlackToken+`"}`)
	api.put("/api/v1/integrations/github", api.alice, `{"token":"ghp_0123456789abcdefWXYZ"}`)
	api.put("/api/v1/integrations/chatwork", api.alice, `{"token":"`+aliceChatworkToken+`"}`)

	// Slack detection: twelve judged items, newest first, of which ten are
	// listed; the other kinds are left out.
	var items []store.ActionItem
	for i := 0; i < 12; i++ {
		ts := fmt.Sprintf("17297000%02d.000100", i)
		key := "slack:T1:m:C1:-:" + ts
		if i%2 == 1 {
			key = "slack:T1:d:D1:-:" + ts + ":" + ts
		}
		items = append(items, api.tryItem("s"+string(rune('a'+i)), store.SourceSlack, key, int64(100+i*10), `[{"service":"slack","conversation_name":"#general"}]`))
	}
	fallback := api.tryItem("fallback", store.SourceSlack, "slack:T1:m:C1:-:1729700099.000100", 1, "")
	fallback.Content = "Slack を確認: x"
	group := api.tryItem("group", store.SourceSlack, "slack:T1:g:C1:-:x", 1, "")
	other := api.tryItem("otherteam", store.SourceSlack, "slack:T9:m:C1:-:1729700099.000100", 1, "")
	bobs := api.tryItem("bobs", store.SourceSlack, "slack:T1:m:C1:-:1729700098.000100", 1, "")
	bobs.UserID = "bob-id"
	cwItem := api.tryItem("cwitem", store.SourceChatwork, "chatwork:101:m:3:301", 1, "")
	undetected := api.tryItem("undetected", store.SourceSlack, "slack:T1:m:C1:-:1729700097.000100", 1, "")
	undetected.DetectedAt = sql.NullInt64{}
	items = append(items, fallback, group, other, bobs, cwItem, undetected)
	api.create(items...)
	list := api.samples(store.InstructionSlackDetect)
	if got := sampleIDs(list); got != "sa,sb,sc,sd,se,sf,sg,sh,si,sj" {
		t.Fatalf("slack detect: %s", got)
	}
	if s := list[0]; s.ConversationName == nil || *s.ConversationName != "#general" || s.DetectedAt == nil || *s.DetectedAt != formatTime(api.now.Unix()-100) || s.Status != "open" || s.AutoReason != nil {
		t.Fatalf("sample: %+v", s)
	}

	// Slack replies: up to five completed by a reply and five tracked
	// and checked, newest check first.
	var replies []store.ActionItem
	for i := 0; i < 6; i++ {
		r := api.tryItem("r"+string(rune('a'+i)), store.SourceSlack, "slack:T1:m:C2:-:172970010"+string(rune('0'+i))+".000100", 500, "")
		r.Status, r.Tracking, r.AutoReason = "done", 0, sql.NullString{String: "slack.reply_resolved", Valid: true}
		r.TrackedAt = sql.NullInt64{Int64: api.now.Unix() - int64(100+i*20), Valid: true}
		k := api.tryItem("k"+string(rune('a'+i)), store.SourceSlack, "slack:T1:m:C2:-:172970020"+string(rune('0'+i))+".000100", 500, "")
		k.TrackedAt = sql.NullInt64{Int64: api.now.Unix() - int64(110+i*20), Valid: true}
		replies = append(replies, r, k)
	}
	unchecked := api.tryItem("unchecked", store.SourceSlack, "slack:T1:m:C2:-:1729700300.000100", 1, "")
	done := api.tryItem("donebyhand", store.SourceSlack, "slack:T1:m:C2:-:1729700301.000100", 1, "")
	done.Status, done.TrackedAt = "done", sql.NullInt64{Int64: api.now.Unix(), Valid: true}
	replies = append(replies, unchecked, done)
	api.create(replies...)
	if got := sampleIDs(api.samples(store.InstructionSlackReply)); got != "ra,ka,rb,kb,rc,kc,rd,kd,re,ke" {
		t.Fatalf("slack reply: %s", got)
	}

	// GitHub: the mentions only.
	api.create(api.tryItem("gm", store.SourceGitHub, "github:mention:acme/web#7:c11", 5, ""), api.tryItem("gr", store.SourceGitHub, "github:review:acme/web#7", 1, ""),
		api.tryItem("gg", store.SourceGitHub, "github:group:acme/web#7:gm", 1, ""))
	if got := sampleIDs(api.samples(store.InstructionGitHubDetect)); got != "gm" {
		t.Fatalf("github: %s", got)
	}
	// Chatwork: not the imported tasks.
	api.create(api.tryItem("ct", store.SourceChatwork, "chatwork:101:t:5", 0, ""), api.tryItem("cd", store.SourceChatwork, "chatwork:101:d:2:201:202", 2, ""))
	if got := sampleIDs(api.samples(store.InstructionChatworkDetect)); got != "cwitem,cd" {
		t.Fatalf("chatwork: %s", got)
	}

	// Gmail: the items of every connected account, not those of an
	// account no longer connected.
	api.connectGmail(api.alice, "alice@example.com")
	api.connectGmail(api.alice, "work@example.com")
	api.create(api.tryItem("ma", store.SourceGmail, "gmail:alice@example.com:r:a1:b1:b1", 3, ""), api.tryItem("mw", store.SourceGmail, "gmail:work@example.com:r:a2:b2:b2", 2, ""),
		api.tryItem("mo", store.SourceGmail, "gmail:old@example.com:r:a3:b3:b3", 1, ""))
	if got := sampleIDs(api.samples(store.InstructionGmailDetect)); got != "mw,ma" {
		t.Fatalf("gmail: %s", got)
	}
	api.connectAI(api.alice)
	expectStatus(t, api.try(store.InstructionGmailDetect, "mo", "", ""), 404, "not_found")
}
