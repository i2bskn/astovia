package httpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/i2bskn/astovia/internal/chatworksync"
	"github.com/i2bskn/astovia/internal/store"
)

// fakeChatworkMe answers Chatwork's GET /me. status other than 200 is
// returned as is.
type fakeChatworkMe struct {
	mu       sync.Mutex
	status   int
	account  string
	requests []*http.Request
}

func (f *fakeChatworkMe) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r)
	if f.status != 0 && f.status != 200 {
		w.WriteHeader(f.status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"account_id":` + f.account + `,"name":"alice","organization_name":"Acme","avatar_image_url":"https://example.com/a.png"}`))
}

func (f *fakeChatworkMe) set(fn func(f *fakeChatworkMe)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *fakeChatworkMe) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

const chatworkToken = "0123456789abcdef0123456789abWXYZ"

func TestChatworkIntegrationAPI(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) { testChatworkIntegrationAPI(t, driver) })
	}
}

func testChatworkIntegrationAPI(t *testing.T, driver string) {
	api := newIntegrationAPI(t, driver)
	ctx := context.Background()
	cw := &fakeChatworkMe{account: "101"}
	srv := httptest.NewServer(cw)
	t.Cleanup(srv.Close)
	api.server.chatwork.BaseURL = srv.URL
	const path = "/api/v1/integrations/chatwork"
	if w := api.do("GET", path, api.alice, ""); w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"connected":false}` {
		t.Fatalf("disconnected: %d %s", w.Code, w.Body.String())
	}

	// Malformed input is rejected before Chatwork is called.
	for _, body := range []string{`{}`, `{"token":""}`, `{"token":"a b"}`, `{"token":null}`, `{"token":1}`, `{"token":"` + strings.Repeat("x", 256) + `"}`, `{"token":"abc","detect_dms":"no"}`, `{"token":"abc","import_tasks":null}`, `{"token":"abc","other":1}`} {
		expectStatus(t, api.do("PUT", path, api.alice, body), 400, "invalid_request")
	}
	if cw.count() != 0 {
		t.Fatalf("Chatwork called %d times for invalid input", cw.count())
	}

	// Answers from Chatwork. Nothing is saved on failure.
	for _, tc := range []struct {
		name    string
		status  int
		account string
		want    int
		err     string
	}{
		{"401", 401, "101", 400, "chatwork_token_invalid"},
		{"403", 403, "101", 503, "chatwork_unavailable"},
		{"429", 429, "101", 503, "chatwork_unavailable"},
		{"500", 500, "101", 503, "chatwork_unavailable"},
		{"unexpected account ID", 200, `"x/../1"`, 503, "chatwork_unavailable"},
		{"zero account ID", 200, "0", 503, "chatwork_unavailable"},
	} {
		cw.set(func(f *fakeChatworkMe) { f.status, f.account = tc.status, tc.account })
		w := api.do("PUT", path, api.alice, `{"token":"`+chatworkToken+`"}`)
		expectStatus(t, w, tc.want, tc.err)
		if strings.Contains(w.Body.String(), chatworkToken) {
			t.Fatalf("%s: token in the response", tc.name)
		}
		if _, err := api.store.ChatworkConnection(ctx, "alice-id"); err == nil {
			t.Fatalf("%s: saved", tc.name)
		}
	}

	cw.set(func(f *fakeChatworkMe) { f.status, f.account = 200, "101" })
	w := api.do("PUT", path, api.alice, `{"token":"`+chatworkToken+`","detect_dms":false}`)
	if w.Code != 200 || strings.Contains(w.Body.String(), chatworkToken) {
		t.Fatalf("connect: %d %s", w.Code, w.Body.String())
	}
	_, keys := jsonKeys(t, w.Body.Bytes())
	if !reflect.DeepEqual(keys, []string{"account_name", "connected", "created_at", "detect_dms", "detect_mentions", "immediate_notify", "import_tasks", "last_error", "last_synced_at", "organization_name", "status", "token_last4"}) {
		t.Fatalf("keys: %v", keys)
	}
	var got chatworkResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Connected || got.AccountName != "alice" || got.OrganizationName != "Acme" || got.TokenLast4 != "WXYZ" || !got.ImmediateNotify || !got.DetectMentions || got.DetectDMs || !got.ImportTasks ||
		got.Status != "active" || got.LastError != nil || got.LastSyncedAt != nil || got.CreatedAt != "2026-10-24T01:02:03Z" {
		t.Fatalf("connected: %+v", got)
	}
	cw.mu.Lock()
	last := cw.requests[len(cw.requests)-1]
	cw.mu.Unlock()
	if last.URL.Path != "/me" || last.Header.Get("x-chatworktoken") != chatworkToken || last.Header.Get("Authorization") != "" || last.URL.RawQuery != "" {
		t.Fatalf("Chatwork request: %s %v", last.URL, last.Header)
	}
	c, err := api.store.ChatworkConnection(ctx, "alice-id")
	if err != nil || c.AccountID != "101" || strings.Contains(c.TokenSealed, chatworkToken) {
		t.Fatalf("stored: %+v %v", c, err)
	}
	if plain, err := api.server.box.Open(chatworksync.TokenPurpose("alice-id"), c.TokenSealed); err != nil || plain != chatworkToken {
		t.Fatalf("open: %q %v", plain, err)
	}
	if w = api.do("GET", path, api.alice, ""); w.Code != 200 || strings.Contains(w.Body.String(), chatworkToken) || !strings.Contains(w.Body.String(), `"token_last4":"WXYZ"`) {
		t.Fatalf("GET: %d %s", w.Code, w.Body.String())
	}
	// Bob has no connection.
	if w = api.do("GET", path, api.bob, ""); strings.TrimSpace(w.Body.String()) != `{"connected":false}` {
		t.Fatalf("bob: %s", w.Body.String())
	}

	// last_synced_at is the later of the window and the task import.
	err = api.store.ChatworkSync(ctx, "alice-id", c.Generation, func(tx *store.ChatworkTx) error {
		state := tx.Conn
		state.LastTasksAt = sql.NullInt64{Int64: c.CreatedAt + 60, Valid: true}
		return tx.SaveState(state)
	})
	if err != nil {
		t.Fatal(err)
	}
	if w = api.do("GET", path, api.alice, ""); !strings.Contains(w.Body.String(), `"last_synced_at":"2026-10-24T01:03:03Z"`) {
		t.Fatalf("tasks only: %s", w.Body.String())
	}
	err = api.store.ChatworkSync(ctx, "alice-id", c.Generation, func(tx *store.ChatworkTx) error {
		state := tx.Conn
		state.LastSyncedAt = sql.NullInt64{Int64: c.CreatedAt + 120, Valid: true}
		return tx.SaveState(state)
	})
	if err != nil {
		t.Fatal(err)
	}
	if w = api.do("GET", path, api.alice, ""); !strings.Contains(w.Body.String(), `"last_synced_at":"2026-10-24T01:04:03Z"`) {
		t.Fatalf("window: %s", w.Body.String())
	}

	// PATCH merges; PUT keeps the settings when they are left out.
	for _, body := range []string{`{}`, `{"import_tasks":null}`, `{"import_tasks":1}`, `{"detect_dms":false,"token":"x"}`, `{"detect_group_dms":true}`} {
		expectStatus(t, api.do("PATCH", path, api.alice, body), 400, "invalid_request")
	}
	if w = api.do("PATCH", path, api.alice, `{"import_tasks":false,"immediate_notify":false}`); w.Code != 200 || !strings.Contains(w.Body.String(), `"import_tasks":false`) ||
		!strings.Contains(w.Body.String(), `"immediate_notify":false`) || !strings.Contains(w.Body.String(), `"detect_dms":false`) || !strings.Contains(w.Body.String(), `"detect_mentions":true`) {
		t.Fatalf("PATCH: %d %s", w.Code, w.Body.String())
	}
	expectStatus(t, api.do("PATCH", path, api.bob, `{"detect_dms":true}`), 404, "not_found")
	if w = api.do("PUT", path, api.alice, `{"token":"`+chatworkToken+`"}`); w.Code != 200 || !strings.Contains(w.Body.String(), `"import_tasks":false`) || !strings.Contains(w.Body.String(), `"immediate_notify":false`) {
		t.Fatalf("PUT keeps settings: %d %s", w.Code, w.Body.String())
	}

	// Another account stops the tracking of the previous one's items and
	// keeps their keys.
	c, err = api.store.ChatworkConnection(ctx, "alice-id")
	if err != nil {
		t.Fatal(err)
	}
	err = api.store.ChatworkSync(ctx, "alice-id", c.Generation, func(tx *store.ChatworkTx) error {
		_, e := tx.CreateItem(store.ActionItem{ID: "cw-item", Content: "x", Status: "open", Refs: "[]", Version: 1, CreatedAt: 1, UpdatedAt: 1, Source: sql.NullString{String: "chatwork", Valid: true}, SourceKey: sql.NullString{String: "chatwork:101:m:2:1000", Valid: true}, Tracking: 1})
		return e
	})
	if err != nil {
		t.Fatal(err)
	}
	cw.set(func(f *fakeChatworkMe) { f.account = "102" })
	if w = api.do("PUT", path, api.alice, `{"token":"`+chatworkToken+`"}`); w.Code != 200 {
		t.Fatalf("PUT: %d %s", w.Code, w.Body.String())
	}
	if item, err := api.store.ActionItem(ctx, "alice-id", "cw-item"); err != nil || item.Tracking != 0 || item.SourceKey.String != "chatwork:101:m:2:1000" {
		t.Fatalf("item after account change: %+v %v", item, err)
	}

	// Bob's connection is not touched by Alice's requests.
	if w = api.do("PUT", path, api.bob, `{"token":"`+chatworkToken+`"}`); w.Code != 200 {
		t.Fatalf("bob PUT: %d %s", w.Code, w.Body.String())
	}
	for i := 0; i < 2; i++ {
		if w = api.do("DELETE", path, api.alice, ""); w.Code != 204 {
			t.Fatalf("DELETE: %d %s", w.Code, w.Body.String())
		}
	}
	if w = api.do("GET", path, api.alice, ""); strings.TrimSpace(w.Body.String()) != `{"connected":false}` {
		t.Fatalf("after DELETE: %s", w.Body.String())
	}
	if _, err = api.store.ChatworkConnection(ctx, "bob-id"); err != nil {
		t.Fatalf("bob's connection: %v", err)
	}

	// PUT and PATCH need a JSON body.
	r := httptest.NewRequest("PATCH", path, strings.NewReader(`{"detect_dms":true}`))
	r.Header.Set("Authorization", "Bearer "+api.bob)
	expectStatus(t, serve(t, api.handler, r), 415, "unsupported_media_type")
	r = httptest.NewRequest("PUT", path, strings.NewReader(`{"token":"x"}`))
	r.Header.Set("Authorization", "Bearer "+api.bob)
	expectStatus(t, serve(t, api.handler, r), 415, "unsupported_media_type")

	// API tokens cannot manage the integration.
	tok := (&tokenAPI{t: t, handler: api.handler}).issue(api.alice, "x").Token
	for _, m := range []string{"GET", "PUT", "PATCH", "DELETE"} {
		expectStatus(t, api.do(m, path, tok, `{}`), 401, "unauthorized")
	}
	// Without a session nothing is answered.
	expectStatus(t, api.do("GET", path, "", ""), 401, "unauthorized")
}
