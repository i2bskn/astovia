package httpserver

import (
	"context"
	"encoding/json"
	"github.com/i2bskn/astovia/internal/auth"
	"github.com/i2bskn/astovia/internal/config"
	"github.com/i2bskn/astovia/internal/store"
	"github.com/i2bskn/astovia/internal/testutil"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

// tokenAPI is a handler with alice and bob logged in. Unlike actionItemAPI it
// keeps the store so that tests can reset passwords and disable users.
type tokenAPI struct {
	t       *testing.T
	store   *store.Store
	handler http.Handler
	now     *time.Time
	alice   string
	bob     string
}

func newTokenAPI(t *testing.T, driver string) *tokenAPI {
	t.Helper()
	s, _ := testutil.Database(t, driver)
	hash, err := auth.Password("correct-password-123")
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range []store.User{{ID: "alice-id", Email: "alice@example.com", PasswordHash: hash}, {ID: "bob-id", Email: "bob@example.com", PasswordHash: hash}} {
		if err = s.CreateUser(context.Background(), u); err != nil {
			t.Fatal(err)
		}
	}
	a, err := auth.New(s, make([]byte, 32), "https://astovia.test")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 23, 1, 2, 3, 0, time.UTC)
	a.Now = func() time.Time { return now }
	api := &tokenAPI{t: t, store: s, now: &now, handler: New(config.Config{PublicURL: "https://astovia.test", Secure: true}, a, fstest.MapFS{})}
	api.alice = api.login("alice")
	api.bob = api.login("bob")
	return api
}

func (a *tokenAPI) login(name string) string {
	a.t.Helper()
	r := httptest.NewRequest("POST", "/auth/login", strings.NewReader(`{"email":"`+name+`@example.com","password":"correct-password-123"}`))
	r.Header.Set("Origin", "https://astovia.test")
	r.Header.Set("Content-Type", "application/json")
	w := serve(a.t, a.handler, r)
	var result authResponse
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil {
		a.t.Fatalf("login %s: %d %s", name, w.Code, w.Body.String())
	}
	return result.AccessToken
}

// do sends a request with a JSON content type when body is not empty.
func (a *tokenAPI) do(method, path, bearer, body string) *httptest.ResponseRecorder {
	a.t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	return serve(a.t, a.handler, r)
}

// issue creates an API token with the access JWT jwt.
func (a *tokenAPI) issue(jwt, name string) createdAPITokenResponse {
	a.t.Helper()
	w := a.do("POST", "/api/v1/api-tokens", jwt, `{"name":`+string(mustJSON(a.t, name))+`}`)
	if w.Code != 201 {
		a.t.Fatalf("issue %q: %d %s", name, w.Code, w.Body.String())
	}
	var out createdAPITokenResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		a.t.Fatal(err)
	}
	return out
}

func (a *tokenAPI) list(jwt string) []apiTokenResponse {
	a.t.Helper()
	w := a.do("GET", "/api/v1/api-tokens", jwt, "")
	var out apiTokenListResponse
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
		a.t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}
	return out.Items
}

func TestAPITokensAPI(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) { testAPITokensAPI(t, driver) })
	}
}

func testAPITokensAPI(t *testing.T, driver string) {
	api := newTokenAPI(t, driver)

	w := api.do("GET", "/api/v1/api-tokens", api.alice, "")
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"items":[]}` {
		t.Fatalf("empty list: %d %s", w.Code, w.Body.String())
	}

	w = api.do("POST", "/api/v1/api-tokens", api.alice, `{"name":"  Claude Code  "}`)
	if w.Code != 201 || w.Header().Get("Location") != "" || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("create: %d %v %s", w.Code, w.Header(), w.Body.String())
	}
	body, keys := jsonKeys(t, w.Body.Bytes())
	if !reflect.DeepEqual(keys, []string{"created_at", "id", "last_used_at", "name", "token"}) || string(body["last_used_at"]) != "null" {
		t.Fatalf("create keys: %v %s", keys, w.Body.String())
	}
	var created createdAPITokenResponse
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Name != "Claude Code" || created.CreatedAt != "2026-10-23T01:02:03Z" || !auth.IsAPIToken(created.Token) || !itemIDPattern.MatchString(created.ID) {
		t.Fatalf("created: %+v", created)
	}

	// The list never contains the secret or its hash.
	*api.now = api.now.Add(time.Second)
	second := api.issue(api.alice, "second")
	w = api.do("GET", "/api/v1/api-tokens", api.alice, "")
	if strings.Contains(w.Body.String(), created.Token) || strings.Contains(w.Body.String(), auth.Hash(created.Token)) || strings.Contains(w.Body.String(), "token") {
		t.Fatalf("list leaks the secret: %s", w.Body.String())
	}
	var list apiTokenListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 2 || list.Items[0].ID != second.ID || list.Items[1].ID != created.ID {
		t.Fatalf("list order: %+v", list.Items)
	}
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(mustJSON(t, list.Items), &items); err != nil {
		t.Fatal(err)
	}
	if _, keys = jsonKeys(t, mustJSON(t, items[0])); !reflect.DeepEqual(keys, []string{"created_at", "id", "last_used_at", "name"}) {
		t.Fatalf("list item keys: %v", keys)
	}

	// Using the token records last_used_at.
	if w = api.do("GET", "/api/v1/action-items", created.Token, ""); w.Code != 200 {
		t.Fatalf("token use: %d %s", w.Code, w.Body.String())
	}
	if got := api.list(api.alice); got[1].LastUsedAt == nil || *got[1].LastUsedAt != "2026-10-23T01:02:04Z" || got[0].LastUsedAt != nil {
		t.Fatalf("last_used_at: %+v", got)
	}

	// name boundaries.
	for _, tc := range []struct {
		body string
		ok   bool
	}{
		{`{"name":"` + strings.Repeat("あ", 100) + `"}`, true},
		{`{"name":" ` + strings.Repeat("a", 100) + ` "}`, true},
		{`{"name":"` + strings.Repeat("あ", 101) + `"}`, false},
		{`{"name":""}`, false},
		{`{"name":" \t\n "}`, false},
		{`{"name":"a\u0000b"}`, false},
		{`{"name":null}`, false},
		{`{"name":1}`, false},
		{`{}`, false},
	} {
		w = api.do("POST", "/api/v1/api-tokens", api.bob, tc.body)
		if tc.ok && w.Code != 201 || !tc.ok && (w.Code != 400 || errorCode(t, w) != "invalid_request" || !strings.Contains(w.Body.String(), "name must be 1-100 characters.")) {
			t.Errorf("%.60s: %d %s", tc.body, w.Code, w.Body.String())
		}
	}
	expectStatus(t, api.do("POST", "/api/v1/api-tokens", api.alice, `{"name":"x","scope":"all"}`), 400, "invalid_request")
	expectStatus(t, api.do("POST", "/api/v1/api-tokens", api.alice, `{"name":"`+strings.Repeat("x", 4096)+`"}`), 413, "body_too_large")
	r := httptest.NewRequest("POST", "/api/v1/api-tokens", strings.NewReader(`{"name":"x"}`))
	r.Header.Set("Authorization", "Bearer "+api.alice)
	expectStatus(t, serve(t, api.handler, r), 415, "unsupported_media_type")

	// API tokens cannot manage tokens or read the account.
	expectStatus(t, api.do("GET", "/api/v1/api-tokens", created.Token, ""), 401, "unauthorized")
	expectStatus(t, api.do("POST", "/api/v1/api-tokens", created.Token, `{"name":"x"}`), 401, "unauthorized")
	expectStatus(t, api.do("DELETE", "/api/v1/api-tokens/"+second.ID, created.Token, ""), 401, "unauthorized")
	expectStatus(t, api.do("GET", "/api/v1/me", created.Token, ""), 401, "unauthorized")
	if len(api.list(api.alice)) != 2 {
		t.Fatal("token deleted by an API token")
	}

	// Another user's, missing and malformed IDs are all 404.
	bobs := api.issue(api.bob, "bob's")
	for _, id := range []string{bobs.ID, "missing", "bad.id", strings.Repeat("a", 65)} {
		w = api.do("DELETE", "/api/v1/api-tokens/"+id, api.alice, "")
		expectStatus(t, w, 404, "not_found")
		if !strings.Contains(w.Body.String(), "API token not found.") {
			t.Fatalf("message: %s", w.Body.String())
		}
	}
	if w = api.do("GET", "/api/v1/action-items", bobs.Token, ""); w.Code != 200 {
		t.Fatalf("bob's token after failed delete: %d", w.Code)
	}

	// Deleting revokes the token immediately.
	w = api.do("DELETE", "/api/v1/api-tokens/"+created.ID, api.alice, "")
	if w.Code != 204 || w.Body.Len() != 0 {
		t.Fatalf("delete: %d %q", w.Code, w.Body.String())
	}
	expectStatus(t, api.do("GET", "/api/v1/action-items", created.Token, ""), 401, "unauthorized")
	expectStatus(t, api.do("DELETE", "/api/v1/api-tokens/"+created.ID, api.alice, ""), 404, "not_found")

	// Methods.
	for _, tc := range []struct{ method, path, allow string }{
		{"PUT", "/api/v1/api-tokens", "GET, POST"},
		{"DELETE", "/api/v1/api-tokens", "GET, POST"},
		{"GET", "/api/v1/api-tokens/" + second.ID, "DELETE"},
		{"PATCH", "/api/v1/api-tokens/" + second.ID, "DELETE"},
	} {
		w = api.do(tc.method, tc.path, api.alice, "")
		expectStatus(t, w, 405, "method_not_allowed")
		if w.Header().Get("Allow") != tc.allow {
			t.Fatalf("%s %s: Allow=%q", tc.method, tc.path, w.Header().Get("Allow"))
		}
	}
	// Without credentials.
	expectStatus(t, api.do("GET", "/api/v1/api-tokens", "", ""), 401, "unauthorized")
	expectStatus(t, api.do("POST", "/api/v1/api-tokens", "", `{"name":"x"}`), 401, "unauthorized")
	expectStatus(t, api.do("DELETE", "/api/v1/api-tokens/"+second.ID, "", ""), 401, "unauthorized")
}

func TestAPITokenLimitAPI(t *testing.T) {
	api := newTokenAPI(t, "sqlite")
	for i := 0; i < store.MaxAPITokens; i++ {
		api.issue(api.alice, "t")
	}
	w := api.do("POST", "/api/v1/api-tokens", api.alice, `{"name":"one too many"}`)
	expectStatus(t, w, 409, "limit_reached")
	if !strings.Contains(w.Body.String(), "You can have at most 50 API tokens. Delete one first.") {
		t.Fatalf("message: %s", w.Body.String())
	}
	if len(api.list(api.alice)) != store.MaxAPITokens {
		t.Fatal("limit exceeded")
	}
	api.issue(api.bob, "bob's first")
}

// After a password reset, the old access JWT cannot issue tokens and the
// user's existing tokens stop working everywhere. Other users keep theirs.
func TestAPITokensAfterPasswordReset(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			api := newTokenAPI(t, driver)
			alices := api.issue(api.alice, "alice")
			bobs := api.issue(api.bob, "bob")
			if err := api.store.ChangeUser(context.Background(), "alice@example.com", "new-hash", false, api.now.Unix()); err != nil {
				t.Fatal(err)
			}
			expectStatus(t, api.do("POST", "/api/v1/api-tokens", api.alice, `{"name":"after reset"}`), 401, "unauthorized")
			expectStatus(t, api.do("GET", "/api/v1/action-items", alices.Token, ""), 401, "unauthorized")
			w := mcpPost(t, api.handler, alices.Token, "", "")
			if w.Code != 401 || w.Header().Get("WWW-Authenticate") != mcpInvalidToken {
				t.Fatalf("/mcp after reset: %d %v", w.Code, w.Header())
			}
			if list, err := api.store.ListAPITokens(context.Background(), "alice-id"); err != nil || len(list) != 0 {
				t.Fatalf("alice's tokens remain: %+v %v", list, err)
			}
			if w = api.do("GET", "/api/v1/action-items", bobs.Token, ""); w.Code != 200 {
				t.Fatalf("bob's token: %d", w.Code)
			}
			if w = mcpPost(t, api.handler, bobs.Token, "", ""); w.Code != 200 {
				t.Fatalf("bob's token on /mcp: %d %s", w.Code, w.Body.String())
			}
			// Disabling bob removes his tokens too.
			if err := api.store.ChangeUser(context.Background(), "bob@example.com", "", true, api.now.Unix()); err != nil {
				t.Fatal(err)
			}
			expectStatus(t, api.do("GET", "/api/v1/action-items", bobs.Token, ""), 401, "unauthorized")
			if w = mcpPost(t, api.handler, bobs.Token, "", ""); w.Code != 401 {
				t.Fatalf("disabled user's token on /mcp: %d", w.Code)
			}
			if list, err := api.store.ListAPITokens(context.Background(), "bob-id"); err != nil || len(list) != 0 {
				t.Fatalf("bob's tokens remain: %+v %v", list, err)
			}
		})
	}
}

// API tokens work on every /api/v1/action-items route and see only their
// user's items.
func TestActionItemsWithAPIToken(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			api := newTokenAPI(t, driver)
			tok := api.issue(api.alice, "alice").Token
			w := api.do("POST", "/api/v1/action-items", tok, `{"content":"reply"}`)
			if w.Code != 201 {
				t.Fatalf("create: %d %s", w.Code, w.Body.String())
			}
			var item actionItemResponse
			if err := json.Unmarshal(w.Body.Bytes(), &item); err != nil {
				t.Fatal(err)
			}
			if w = api.do("GET", "/api/v1/action-items/"+item.ID, tok, ""); w.Code != 200 {
				t.Fatalf("get: %d %s", w.Code, w.Body.String())
			}
			if w = api.do("PATCH", "/api/v1/action-items/"+item.ID, tok, `{"status":"done","version":1}`); w.Code != 200 || !strings.Contains(w.Body.String(), `"status":"done"`) {
				t.Fatalf("update: %d %s", w.Code, w.Body.String())
			}
			if w = api.do("GET", "/api/v1/action-items", tok, ""); w.Code != 200 || !strings.Contains(w.Body.String(), item.ID) {
				t.Fatalf("list: %d %s", w.Code, w.Body.String())
			}
			// The same item is visible with alice's access JWT.
			if w = api.do("GET", "/api/v1/action-items/"+item.ID, api.alice, ""); w.Code != 200 {
				t.Fatalf("get with JWT: %d", w.Code)
			}

			// Bob's item is not visible with alice's token.
			w = api.do("POST", "/api/v1/action-items", api.bob, `{"content":"bob's"}`)
			var bobs actionItemResponse
			if err := json.Unmarshal(w.Body.Bytes(), &bobs); err != nil {
				t.Fatal(err)
			}
			expectStatus(t, api.do("GET", "/api/v1/action-items/"+bobs.ID, tok, ""), 404, "not_found")
			expectStatus(t, api.do("PATCH", "/api/v1/action-items/"+bobs.ID, tok, `{"status":"done"}`), 404, "not_found")
			if w = api.do("GET", "/api/v1/action-items", tok, ""); strings.Contains(w.Body.String(), bobs.ID) {
				t.Fatal("bob's item listed")
			}

			// Malformed tokens with the prefix are rejected without a lookup.
			for _, v := range []string{"ast_", "ast_x", tok + "x", strings.Replace(tok, "ast_", "ast-", 1)} {
				expectStatus(t, api.do("GET", "/api/v1/action-items", v, ""), 401, "unauthorized")
			}

			// A deleted token stops working.
			list := api.list(api.alice)
			if w = api.do("DELETE", "/api/v1/api-tokens/"+list[0].ID, api.alice, ""); w.Code != 204 {
				t.Fatalf("delete: %d", w.Code)
			}
			expectStatus(t, api.do("GET", "/api/v1/action-items", tok, ""), 401, "unauthorized")

			// A disabled user's token stops working.
			bobTok := api.issue(api.bob, "bob").Token
			if err := api.store.ChangeUser(context.Background(), "bob@example.com", "", true, api.now.Unix()); err != nil {
				t.Fatal(err)
			}
			expectStatus(t, api.do("GET", "/api/v1/action-items", bobTok, ""), 401, "unauthorized")
		})
	}
}
