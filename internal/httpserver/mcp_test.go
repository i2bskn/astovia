package httpserver

import (
	"context"
	"encoding/json"
	"github.com/i2bskn/astovia/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
)

const toolsListBody = `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`

// The WWW-Authenticate challenges of /mcp for PublicURL https://astovia.test.
const (
	mcpNoToken      = `Bearer resource_metadata="https://astovia.test/.well-known/oauth-protected-resource/mcp", scope="mcp"`
	mcpInvalidToken = `Bearer error="invalid_token", resource_metadata="https://astovia.test/.well-known/oauth-protected-resource/mcp", scope="mcp"`
)

// mcpPost sends a tools/list request to h without the SDK client.
func mcpPost(t *testing.T, h http.Handler, bearer, origin, host string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("POST", "/mcp", strings.NewReader(toolsListBody))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	if host != "" {
		r.Host = host
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// headerTransport adds headers to every request and records the methods sent.
type headerTransport struct {
	base    http.RoundTripper
	headers map[string]string
	mu      sync.Mutex
	methods []string
}

func (h *headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	for k, v := range h.headers {
		r.Header.Set(k, v)
	}
	h.mu.Lock()
	h.methods = append(h.methods, r.Method)
	h.mu.Unlock()
	return h.base.RoundTrip(r)
}

func (h *headerTransport) sent(method string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, m := range h.methods {
		if m == method {
			return true
		}
	}
	return false
}

type mcpClient struct {
	t         *testing.T
	session   *mcp.ClientSession
	transport *headerTransport
}

// connectMCP connects the SDK client to srv with an API token and extra
// headers.
func connectMCP(t *testing.T, srv *httptest.Server, token string, headers map[string]string) *mcpClient {
	t.Helper()
	all := map[string]string{"Authorization": "Bearer " + token}
	for k, v := range headers {
		all[k] = v
	}
	ht := &headerTransport{base: srv.Client().Transport, headers: all}
	client := mcp.NewClient(&mcp.Implementation{Name: "astovia-test", Version: "0"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: srv.URL + "/mcp", HTTPClient: &http.Client{Transport: ht}, MaxRetries: -1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	c := &mcpClient{t: t, session: session, transport: ht}
	t.Cleanup(func() { c.close() })
	return c
}

// close closes the session. The server is stateless and returns no
// Mcp-Session-Id, so the SDK's Close sends no DELETE (which /mcp would answer
// with 405) and returns nil.
func (c *mcpClient) close() {
	c.t.Helper()
	if err := c.session.Close(); err != nil {
		c.t.Errorf("close: %v", err)
	}
	if c.transport.sent("DELETE") {
		c.t.Error("Close sent DELETE to a stateless server")
	}
}

// call calls a tool and returns its text content and isError. Protocol errors
// fail the test: tool failures must be reported as results.
func (c *mcpClient) call(name string, args any) (string, bool) {
	c.t.Helper()
	res, err := c.session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		c.t.Fatalf("%s: protocol error: %v", name, err)
	}
	if len(res.Content) != 1 {
		c.t.Fatalf("%s: content: %+v", name, res.Content)
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		c.t.Fatalf("%s: content type %T", name, res.Content[0])
	}
	if !res.IsError {
		// StructuredContent carries the same object as the text.
		var structured, fromText any
		if err = json.Unmarshal(mustJSON(c.t, res.StructuredContent), &structured); err != nil {
			c.t.Fatal(err)
		}
		if err = json.Unmarshal([]byte(text.Text), &fromText); err != nil {
			c.t.Fatalf("%s: text is not JSON: %s", name, text.Text)
		}
		if string(mustJSON(c.t, structured)) != string(mustJSON(c.t, fromText)) {
			c.t.Fatalf("%s: structured content differs:\n%v\n%s", name, structured, text.Text)
		}
	}
	return text.Text, res.IsError
}

// ok calls a tool that must succeed and decodes its result into out.
func (c *mcpClient) ok(name string, args any, out any) {
	c.t.Helper()
	text, isError := c.call(name, args)
	if isError {
		c.t.Fatalf("%s %v: %s", name, args, text)
	}
	if err := json.Unmarshal([]byte(text), out); err != nil {
		c.t.Fatal(err)
	}
}

// fails calls a tool that must fail with the given text.
func (c *mcpClient) fails(name string, args any, want string) {
	c.t.Helper()
	text, isError := c.call(name, args)
	if !isError || text != want {
		c.t.Fatalf("%s %v: got isError=%v %q, want %q", name, args, isError, text, want)
	}
}

func TestMCPHTTP(t *testing.T) {
	api := newTokenAPI(t, "sqlite")
	tok := api.issue(api.alice, "mcp").Token
	srv := httptest.NewServer(api.handler)
	t.Cleanup(srv.Close)

	// Missing, malformed and non-API-token credentials get a Bearer challenge.
	deleted := api.issue(api.alice, "deleted")
	if w := api.do("DELETE", "/api/v1/api-tokens/"+deleted.ID, api.alice, ""); w.Code != 204 {
		t.Fatal(w.Code)
	}
	for name, bearer := range map[string]string{"none": "", "malformed": "ast_x", "unknown": "ast_" + strings.Repeat("A", 43), "access JWT": api.alice, "deleted": deleted.Token, "unknown OAuth": "aso_" + strings.Repeat("A", 43), "OAuth refresh": "asr_" + strings.Repeat("A", 43)} {
		challenge := mcpInvalidToken
		if bearer == "" {
			challenge = mcpNoToken
		}
		w := mcpPost(t, api.handler, bearer, "", "")
		if w.Code != 401 || w.Header().Get("WWW-Authenticate") != challenge || errorCode(t, w) != "unauthorized" || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("%s: %d %v %s", name, w.Code, w.Header(), w.Body.String())
		}
	}
	r := httptest.NewRequest("POST", "/mcp", strings.NewReader(toolsListBody))
	r.Header.Set("Authorization", "Basic YWxpY2U6cHc=")
	if w := serve(t, api.handler, r); w.Code != 401 || w.Header().Get("WWW-Authenticate") != mcpNoToken {
		t.Fatalf("Basic: %d", w.Code)
	}

	// Origin: absent or equal to PublicURL passes, anything else is rejected
	// before authentication.
	for _, origin := range []string{"https://evil.test", "https://astovia.test:8443", "null"} {
		for _, bearer := range []string{tok, ""} {
			w := mcpPost(t, api.handler, bearer, origin, "")
			if w.Code != 403 || errorCode(t, w) != "origin_rejected" {
				t.Fatalf("origin %q: %d %s", origin, w.Code, w.Body.String())
			}
		}
	}
	if w := mcpPost(t, api.handler, tok, "https://astovia.test", ""); w.Code != 200 {
		t.Fatalf("same origin: %d %s", w.Code, w.Body.String())
	}

	// Only POST.
	for _, method := range []string{"GET", "DELETE", "PUT"} {
		r := httptest.NewRequest(method, "/mcp", nil)
		r.Header.Set("Authorization", "Bearer "+tok)
		w := serve(t, api.handler, r)
		expectStatus(t, w, 405, "method_not_allowed")
		if w.Header().Get("Allow") != "POST" {
			t.Fatalf("%s Allow=%q", method, w.Header().Get("Allow"))
		}
	}

	// Over a real loopback connection, a non-loopback Host (as behind a
	// reverse proxy that keeps it) is accepted, with and without Origin.
	for _, origin := range []string{"", "https://astovia.test"} {
		req, err := http.NewRequest("POST", srv.URL+"/mcp", strings.NewReader(toolsListBody))
		if err != nil {
			t.Fatal(err)
		}
		req.Host = "astovia.example"
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Authorization", "Bearer "+tok)
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var body struct {
			Result struct {
				Tools []json.RawMessage `json:"tools"`
			} `json:"result"`
		}
		err = json.NewDecoder(resp.Body).Decode(&body)
		_ = resp.Body.Close()
		if resp.StatusCode != 200 || err != nil || len(body.Result.Tools) != 12 || resp.Header.Get("Cache-Control") != "no-store" {
			t.Fatalf("Host astovia.example origin %q: %d %v %d %v", origin, resp.StatusCode, err, len(body.Result.Tools), resp.Header)
		}
	}

	// A disabled user's token is rejected.
	bobTok := api.issue(api.bob, "bob").Token
	if w := mcpPost(t, api.handler, bobTok, "", ""); w.Code != 200 {
		t.Fatalf("bob: %d", w.Code)
	}
	if err := api.store.ChangeUser(context.Background(), "bob@example.com", "", true, api.now.Unix()); err != nil {
		t.Fatal(err)
	}
	if w := mcpPost(t, api.handler, bobTok, "", ""); w.Code != 401 || w.Header().Get("WWW-Authenticate") != mcpInvalidToken {
		t.Fatalf("disabled user: %d", w.Code)
	}

	// A database failure during authentication is 503 without a challenge.
	if err := api.store.Close(); err != nil {
		t.Fatal(err)
	}
	if w := mcpPost(t, api.handler, tok, "", ""); w.Code != 503 || errorCode(t, w) != "unavailable" || w.Header().Get("WWW-Authenticate") != "" {
		t.Fatalf("DB error: %d %v", w.Code, w.Header())
	}
}

func TestMCPTools(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) { testMCPTools(t, driver) })
	}
}

func testMCPTools(t *testing.T, driver string) {
	api := newTokenAPI(t, driver)
	srv := httptest.NewServer(api.handler)
	t.Cleanup(srv.Close)
	c := connectMCP(t, srv, api.issue(api.alice, "alice").Token, nil)

	if got := c.session.InitializeResult(); got == nil || got.ServerInfo == nil || got.ServerInfo.Name != "astovia" || !strings.HasPrefix(got.Instructions, "Astovia keeps the user's action items") {
		t.Fatalf("initialize result: %+v", got)
	}

	tools, err := c.session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
		schema, ok := tool.InputSchema.(map[string]any)
		if !ok || schema["type"] != "object" || schema["additionalProperties"] != false || schema["properties"] == nil {
			t.Fatalf("%s input schema: %#v", tool.Name, tool.InputSchema)
		}
		if tool.Description == "" || tool.Annotations == nil || tool.Annotations.OpenWorldHint == nil {
			t.Fatalf("%s: description or annotations: %+v", tool.Name, tool.Annotations)
		}
		// The Chatwork and Slack tools read an external service and nothing
		// else.
		if strings.HasPrefix(tool.Name, "chatwork_") || strings.HasPrefix(tool.Name, "slack_") {
			if !tool.Annotations.ReadOnlyHint || !*tool.Annotations.OpenWorldHint {
				t.Fatalf("%s annotations: %+v", tool.Name, tool.Annotations)
			}
			continue
		}
		if *tool.Annotations.OpenWorldHint {
			t.Fatalf("%s: open world: %+v", tool.Name, tool.Annotations)
		}
		// Every status argument accepts candidate.
		if status, ok := schema["properties"].(map[string]any)["status"].(map[string]any); ok {
			if enum, _ := json.Marshal(status["enum"]); !strings.Contains(string(enum), `"candidate"`) {
				t.Fatalf("%s status enum: %s", tool.Name, enum)
			}
		} else if tool.Name != "action_items_get" {
			t.Fatalf("%s: no status property", tool.Name)
		}
		readOnly := tool.Name == "action_items_list" || tool.Name == "action_items_get"
		if tool.Annotations.ReadOnlyHint != readOnly || !readOnly && (tool.Annotations.DestructiveHint == nil || *tool.Annotations.DestructiveHint) {
			t.Fatalf("%s annotations: %+v", tool.Name, tool.Annotations)
		}
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "action_items_create,action_items_get,action_items_list,action_items_update,chatwork_message_get,chatwork_messages_list,chatwork_messages_search,chatwork_rooms_list,slack_conversations_list,slack_messages_list,slack_messages_search,slack_thread_get" {
		t.Fatalf("tools: %v", names)
	}

	// create -> get -> list -> update.
	var item actionItemResponse
	c.ok("action_items_create", map[string]any{"content": "日程を返答する", "note": "memo", "due_at": "2026-10-24T13:00:00+09:00", "refs": []any{map[string]any{"service": "slack", "url": "https://example.com/x"}}}, &item)
	if item.Content != "日程を返答する" || item.Status != "open" || item.Note == nil || *item.Note != "memo" || item.DueAt == nil || *item.DueAt != "2026-10-24T04:00:00Z" || len(item.Refs) != 1 || item.Version != 1 || item.Kind != "task" || item.Event != nil {
		t.Fatalf("created: %+v", item)
	}
	if stored, err := api.store.ActionItem(context.Background(), "alice-id", item.ID); err != nil || stored.Kind != store.KindTask {
		t.Fatalf("stored kind: %+v %v", stored, err)
	}
	var got actionItemResponse
	c.ok("action_items_get", map[string]any{"id": item.ID}, &got)
	if got.ID != item.ID || got.Content != item.Content {
		t.Fatalf("get: %+v", got)
	}
	// The item is the same one REST returns.
	if w := api.do("GET", "/api/v1/action-items/"+item.ID, api.alice, ""); w.Code != 200 || !strings.Contains(w.Body.String(), item.ID) {
		t.Fatalf("REST get: %d", w.Code)
	}
	var page actionItemListResponse
	c.ok("action_items_list", map[string]any{"status": "open"}, &page)
	if len(page.Items) != 1 || page.Items[0].ID != item.ID || page.NextCursor != nil {
		t.Fatalf("list: %+v", page)
	}
	c.ok("action_items_update", map[string]any{"id": item.ID, "status": "done", "note": nil, "version": 1}, &got)
	if got.Status != "done" || got.Note != nil || got.Version != 2 || got.Content != item.Content || got.DueAt == nil {
		t.Fatalf("update: %+v", got)
	}
	c.ok("action_items_list", map[string]any{"status": "open"}, &page)
	if len(page.Items) != 0 {
		t.Fatalf("open after done: %+v", page)
	}
	c.fails("action_items_update", map[string]any{"id": item.ID, "status": "open", "version": 1}, "conflict: The action item was updated by another request.")

	// Arguments may be omitted, null or empty; null list arguments mean the
	// defaults.
	c.ok("action_items_list", nil, &page)
	if len(page.Items) != 1 {
		t.Fatalf("list without arguments: %+v", page)
	}
	c.ok("action_items_list", map[string]any{"status": nil, "limit": nil, "cursor": nil}, &page)
	if len(page.Items) != 1 || page.NextCursor != nil {
		t.Fatalf("list with nulls: %+v", page)
	}

	// Validation failures are tool results with REST's code and message.
	for _, tc := range []struct {
		tool string
		args any
		want string
	}{
		{"action_items_create", map[string]any{"content": ""}, "invalid_request: content must be 1-500 characters."},
		{"action_items_create", map[string]any{"content": nil}, "invalid_request: content must be 1-500 characters."},
		{"action_items_create", map[string]any{}, "invalid_request: content is required."},
		{"action_items_create", nil, "invalid_request: content is required."},
		{"action_items_create", map[string]any{"content": "x", "user_id": "bob-id"}, "invalid_request: Invalid arguments."},
		{"action_items_create", map[string]any{"content": "x", "status": "closed"}, "invalid_request: status must be open, candidate, done or dismissed."},
		{"action_items_create", map[string]any{"content": "x", "refs": nil}, "invalid_request: refs must be an array of at most 10 objects with a valid service and optional account, conversation_id, conversation_name, message_id and http(s) url."},
		{"action_items_create", []any{"x"}, "invalid_request: Invalid arguments."},
		{"action_items_list", map[string]any{"limit": "50"}, "invalid_request: limit must be an integer from 1 to 100."},
		{"action_items_list", map[string]any{"limit": 50.5}, "invalid_request: limit must be an integer from 1 to 100."},
		{"action_items_list", map[string]any{"limit": 0}, "invalid_request: limit must be an integer from 1 to 100."},
		{"action_items_list", map[string]any{"limit": 101}, "invalid_request: limit must be an integer from 1 to 100."},
		{"action_items_list", map[string]any{"status": "closed"}, "invalid_request: status must be open, candidate, done or dismissed."},
		{"action_items_list", map[string]any{"status": ""}, "invalid_request: status must be open, candidate, done or dismissed."},
		{"action_items_list", map[string]any{"cursor": "***"}, "invalid_request: cursor is invalid."},
		{"action_items_list", map[string]any{"cursor": ""}, "invalid_request: cursor is invalid."},
		{"action_items_list", map[string]any{"sort": "asc"}, "invalid_request: Invalid arguments."},
		{"action_items_get", map[string]any{}, "invalid_request: id is required."},
		{"action_items_get", nil, "invalid_request: id is required."},
		{"action_items_get", map[string]any{"id": nil}, "not_found: Action item not found."},
		{"action_items_get", map[string]any{"id": 123}, "not_found: Action item not found."},
		{"action_items_get", map[string]any{"id": "bad.id"}, "not_found: Action item not found."},
		{"action_items_get", map[string]any{"id": "missing"}, "not_found: Action item not found."},
		{"action_items_get", map[string]any{"id": item.ID, "extra": 1}, "invalid_request: Invalid arguments."},
		{"action_items_update", map[string]any{"status": "done"}, "invalid_request: id is required."},
		{"action_items_update", map[string]any{"id": item.ID}, "invalid_request: At least one of content, status, note, due_at, refs or parent_id is required."},
		{"action_items_update", map[string]any{"id": item.ID, "version": 0, "status": "open"}, "invalid_request: version must be a positive integer."},
		{"action_items_update", map[string]any{"id": item.ID, "content": ""}, "invalid_request: content must be 1-500 characters."},
		{"action_items_update", map[string]any{"id": item.ID, "unknown": 1}, "invalid_request: Invalid arguments."},
	} {
		c.fails(tc.tool, tc.args, tc.want)
	}

	// The list default is 50 items.
	for i := 0; i < 55; i++ {
		if err := api.store.CreateActionItem(context.Background(), store.ActionItem{ID: "bulk-" + strings.Repeat("x", i+1), UserID: "alice-id", Content: "c", Status: "open", Refs: "[]", Version: 1, CreatedAt: int64(i), UpdatedAt: int64(i)}); err != nil {
			t.Fatal(err)
		}
	}
	c.ok("action_items_list", map[string]any{"status": nil, "limit": nil, "cursor": nil}, &page)
	if len(page.Items) != 50 || page.NextCursor == nil {
		t.Fatalf("default limit: %d %v", len(page.Items), page.NextCursor)
	}
	c.ok("action_items_list", map[string]any{"cursor": *page.NextCursor, "limit": 100}, &page)
	if len(page.Items) != 6 || page.NextCursor != nil {
		t.Fatalf("second page: %d", len(page.Items))
	}
}

// Each request is served by a server bound to that request's token, so one
// user's MCP calls never reach another user's data, even with a session ID
// taken from the other user's requests.
func TestMCPUserIsolation(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			api := newTokenAPI(t, driver)
			srv := httptest.NewServer(api.handler)
			t.Cleanup(srv.Close)
			alice := connectMCP(t, srv, api.issue(api.alice, "alice").Token, nil)

			// Stateless: no Mcp-Session-Id is issued, so B sends an arbitrary one.
			w := mcpPost(t, api.handler, api.issue(api.alice, "probe").Token, "", "")
			sessionID := w.Header().Get("Mcp-Session-Id")
			if sessionID == "" {
				sessionID = "session-of-alice"
			}
			bob := connectMCP(t, srv, api.issue(api.bob, "bob").Token, map[string]string{"Mcp-Session-Id": sessionID})

			var item actionItemResponse
			alice.ok("action_items_create", map[string]any{"content": "alice's secret"}, &item)
			var bobs actionItemResponse
			bob.ok("action_items_create", map[string]any{"content": "bob's"}, &bobs)

			bob.fails("action_items_get", map[string]any{"id": item.ID}, "not_found: Action item not found.")
			bob.fails("action_items_update", map[string]any{"id": item.ID, "status": "done", "content": "taken"}, "not_found: Action item not found.")
			var page actionItemListResponse
			bob.ok("action_items_list", nil, &page)
			if len(page.Items) != 1 || page.Items[0].ID != bobs.ID {
				t.Fatalf("bob's list: %+v", page)
			}
			alice.ok("action_items_list", nil, &page)
			if len(page.Items) != 1 || page.Items[0].ID != item.ID {
				t.Fatalf("alice's list: %+v", page)
			}
			// Bob's failed update changed nothing.
			var got actionItemResponse
			alice.ok("action_items_get", map[string]any{"id": item.ID}, &got)
			if got.Content != "alice's secret" || got.Status != "open" || got.Version != 1 {
				t.Fatalf("alice's item changed: %+v", got)
			}
			alice.fails("action_items_get", map[string]any{"id": bobs.ID}, "not_found: Action item not found.")
		})
	}
}
