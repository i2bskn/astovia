package httpserver

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/i2bskn/astovia/internal/ai"
	"github.com/i2bskn/astovia/internal/secretbox"
	"github.com/i2bskn/astovia/internal/store"
)

// fakeModels answers Anthropic's GET /v1/models/{model} with a configurable
// status. hook runs before the answer.
type fakeModels struct {
	mu       sync.Mutex
	status   int
	message  string
	hook     func()
	requests []*http.Request
}

func (f *fakeModels) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, r)
	status, message, hook := f.status, f.message, f.hook
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	w.Header().Set("Content-Type", "application/json")
	if status != 0 && status != 200 {
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{"type": "error", "error": map[string]string{"type": "error", "message": message}})
		return
	}
	_, _ = w.Write([]byte(`{"id":"x","type":"model","display_name":"x","created_at":"2026-01-01T00:00:00Z"}`))
}

func (f *fakeModels) set(status int, message string, hook func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status, f.message, f.hook = status, message, hook
}

func (f *fakeModels) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func (f *fakeModels) last() *http.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[len(f.requests)-1]
}

func TestAIAPI(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) { testAIAPI(t, driver) })
	}
}

func testAIAPI(t *testing.T, driver string) {
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	api := newIntegrationAPI(t, driver)
	fm := &fakeModels{}
	srv := httptest.NewServer(fm)
	t.Cleanup(srv.Close)
	api.server.ai.BaseURL = srv.URL
	const path = "/api/v1/ai"
	const key = "sk-ant-api03-secret-value-WXYZ"

	w := api.do("GET", path, api.alice, "")
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"configured":false}` {
		t.Fatalf("unconfigured: %d %s", w.Code, w.Body.String())
	}

	// Malformed input is rejected before the provider is called.
	for _, body := range []string{`{}`, `{"api_key":"` + key + `"}`, `{"api_key":"` + key + `","model":"claude-haiku-4-5"}`, `{"api_key":"` + key + `","model":null}`, `{"api_key":"` + key + `","model":1}`,
		`{"model":"claude-haiku-5-5"}`, `{"api_key":"","model":"claude-haiku-5-5"}`, `{"api_key":"   ","model":"claude-haiku-5-5"}`, `{"api_key":null,"model":"claude-haiku-5-5"}`, `{"api_key":1,"model":"claude-haiku-5-5"}`,
		`{"api_key":"sk-short","model":"claude-haiku-5-5"}`, `{"api_key":"sk-ant-api03 with space","model":"claude-haiku-5-5"}`, `{"api_key":"sk-ant-api03-ü-0123456789","model":"claude-haiku-5-5"}`,
		`{"api_key":"` + strings.Repeat("k", 513) + `","model":"claude-haiku-5-5"}`, `{"api_key":"` + key + `","model":"claude-haiku-5-5","provider":"openai"}`} {
		expectStatus(t, api.do("PUT", path, api.alice, body), 400, "invalid_request")
	}
	if fm.count() != 0 {
		t.Fatalf("provider called %d times for invalid input", fm.count())
	}

	put := `{"api_key":"  ` + key + `\t","model":"claude-haiku-5-5"}`
	for _, tc := range []struct {
		status  int
		message string
		want    int
		code    string
	}{
		{401, "invalid x-api-key", 400, "ai_key_invalid"},
		{403, "forbidden", 400, "ai_key_invalid"},
		{404, "model not found", 400, "ai_model_unavailable"},
		{402, "billing", 400, "ai_billing"},
		{400, "Your credit balance is too low", 400, "ai_billing"},
		{400, "bad request", 503, "ai_unavailable"},
		{429, "slow down", 503, "ai_unavailable"},
		{529, "overloaded", 503, "ai_unavailable"},
	} {
		fm.set(tc.status, tc.message, nil)
		expectStatus(t, api.do("PUT", path, api.alice, put), tc.want, tc.code)
		if _, err := api.store.AISettings(context.Background(), "alice-id"); err == nil {
			t.Fatalf("%d: saved", tc.status)
		}
	}

	fm.set(200, "", nil)
	w = api.do("PUT", path, api.alice, put)
	if w.Code != 200 || strings.Contains(w.Body.String(), "secret") {
		t.Fatalf("save: %d %s", w.Code, w.Body.String())
	}
	body, keys := jsonKeys(t, w.Body.Bytes())
	if !reflect.DeepEqual(keys, []string{"configured", "key_last4", "last_error", "model", "provider", "status", "updated_at"}) || string(body["last_error"]) != "null" {
		t.Fatalf("keys: %v %s", keys, w.Body.String())
	}
	var got aiResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Configured || got.Provider != "anthropic" || got.Model != "claude-haiku-5-5" || got.KeyLast4 != "WXYZ" || got.Status != "active" || got.UpdatedAt != "2026-10-24T01:02:03Z" {
		t.Fatalf("saved: %+v", got)
	}
	r := fm.last()
	if r.URL.Path != "/v1/models/claude-haiku-5-5" || r.Header.Get("X-Api-Key") != key || r.Header.Get("Authorization") != "" {
		t.Fatalf("provider request: %s %v", r.URL, r.Header)
	}
	a, err := api.store.AISettings(context.Background(), "alice-id")
	if err != nil || strings.Contains(a.APIKeySealed, "secret") {
		t.Fatalf("stored: %+v %v", a, err)
	}
	if plain, err := api.server.box.Open(ai.KeyPurpose("alice-id"), a.APIKeySealed); err != nil || plain != key {
		t.Fatalf("open: %v", err)
	}
	if _, err = api.server.box.Open(ai.KeyPurpose("bob-id"), a.APIKeySealed); err == nil {
		t.Fatal("key opens for another user")
	}
	if w = api.do("GET", path, api.alice, ""); w.Code != 200 || strings.Contains(w.Body.String(), "secret") || !strings.Contains(w.Body.String(), `"key_last4":"WXYZ"`) {
		t.Fatalf("GET: %d %s", w.Code, w.Body.String())
	}
	if w = api.do("GET", path, api.bob, ""); strings.TrimSpace(w.Body.String()) != `{"configured":false}` {
		t.Fatalf("bob sees alice's settings: %s", w.Body.String())
	}

	// A model change checks the saved key again and clears an error.
	if _, err = api.store.SetAIStatus(context.Background(), "alice-id", a.Generation, store.AIError, sql.NullString{String: "billing", Valid: true}, 1); err != nil {
		t.Fatal(err)
	}
	fm.set(404, "x", nil)
	expectStatus(t, api.do("PUT", path, api.alice, `{"model":"claude-opus-5-5"}`), 400, "ai_model_unavailable")
	fm.set(200, "", nil)
	before := fm.count()
	w = api.do("PUT", path, api.alice, `{"model":"claude-opus-5-5"}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"model":"claude-opus-5-5"`) || !strings.Contains(w.Body.String(), `"status":"active"`) || !strings.Contains(w.Body.String(), `"last_error":null`) || fm.count() != before+1 || fm.last().Header.Get("X-Api-Key") != key {
		t.Fatalf("model change: %d %s", w.Code, w.Body.String())
	}

	// Settings deleted while the key was being checked.
	fm.set(200, "", func() {
		if w := api.do("DELETE", path, api.alice, ""); w.Code != 204 {
			t.Errorf("concurrent DELETE: %d", w.Code)
		}
	})
	expectStatus(t, api.do("PUT", path, api.alice, `{"api_key":"`+key+`","model":"claude-haiku-5-5"}`), 409, "ai_conflict")
	fm.set(200, "", nil)

	// A saved key that cannot be opened (another server secret) must be
	// registered again.
	api.connectAI(t, key)
	api.server.box = secretbox.New([]byte(strings.Repeat("o", 32)))
	before = fm.count()
	expectStatus(t, api.do("PUT", path, api.alice, `{"model":"claude-haiku-5-5"}`), 400, "ai_key_invalid")
	if fm.count() != before {
		t.Fatal("provider called with an undecryptable key")
	}
	api.connectAI(t, key)

	// DELETE is idempotent.
	for i := 0; i < 2; i++ {
		if w = api.do("DELETE", path, api.alice, ""); w.Code != 204 {
			t.Fatalf("DELETE: %d %s", w.Code, w.Body.String())
		}
	}
	if w = api.do("GET", path, api.alice, ""); strings.TrimSpace(w.Body.String()) != `{"configured":false}` {
		t.Fatalf("after DELETE: %s", w.Body.String())
	}

	// API tokens cannot manage the AI settings.
	tok := (&tokenAPI{t: t, handler: api.handler}).issue(api.alice, "x").Token
	for _, m := range []string{"GET", "PUT", "DELETE"} {
		expectStatus(t, api.do(m, path, tok, `{}`), 401, "unauthorized")
	}
	if strings.Contains(logs.String(), "secret-value") {
		t.Fatalf("key logged: %s", logs.String())
	}
}

func (a *integrationAPI) connectAI(t *testing.T, key string) {
	t.Helper()
	if w := a.do("PUT", "/api/v1/ai", a.alice, `{"api_key":"`+key+`","model":"claude-haiku-5-5"}`); w.Code != 200 {
		t.Fatalf("connect AI: %d %s", w.Code, w.Body.String())
	}
}
