package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/i2bskn/astovia/internal/ai"
	"github.com/i2bskn/astovia/internal/aicred"
	"github.com/i2bskn/astovia/internal/secretbox"
	"github.com/i2bskn/astovia/internal/store"
)

// fakeChat is the Chat Completions API of the OpenAI API or an
// OpenAI-compatible endpoint. Every request is answered with status and
// body when status is set, with the answers in order otherwise (the last
// one repeats; {"ok":true} when there is none).
type fakeChat struct {
	mu       sync.Mutex
	status   int
	body     string
	answers  []string
	requests []fakeChatRequest
}

type fakeChatRequest struct {
	path, auth string
	body       map[string]any
}

func (f *fakeChat) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	f.mu.Lock()
	f.requests = append(f.requests, fakeChatRequest{path: r.URL.Path, auth: r.Header.Get("Authorization"), body: body})
	status, errBody, n := f.status, f.body, len(f.requests)
	answer := `{"ok":true}`
	if len(f.answers) > 0 {
		answer = f.answers[min(n, len(f.answers))-1]
	}
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if status != 0 {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, errBody)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": answer}, "finish_reason": "stop"}}})
}

func (f *fakeChat) set(status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status, f.body = status, body
}

func (f *fakeChat) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func (f *fakeChat) last() fakeChatRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[len(f.requests)-1]
}

// useEndpoint lets the server reach the fake chat API on the loopback
// address: as the OpenAI API, and as an endpoint with http allowed. It
// returns the URL of the fake.
func useEndpoint(t *testing.T, s *Server, f *fakeChat) string {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	s.cfg.AIAllowPrivateEndpoints = true
	s.ai.AllowPrivateEndpoints = true
	s.ai.Endpoints = ai.NewEndpointClient(true)
	s.ai.OpenAIAPIBaseURL = srv.URL + "/v1"
	// The limit is tested on its own.
	setLimit(s.aiEndpointLimiter, 1000)
	return srv.URL
}

func decodeAI(t *testing.T, w *httptest.ResponseRecorder) aiResponse {
	t.Helper()
	var got aiResponse
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	return got
}

func TestAIOpenAIAPIKey(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) { testAIOpenAIAPIKey(t, driver) })
	}
}

func testAIOpenAIAPIKey(t *testing.T, driver string) {
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	api := newIntegrationAPI(t, driver)
	fc := &fakeChat{}
	useEndpoint(t, api.server, fc)
	const path = "/api/v1/ai"
	const key = "sk-proj-secret-value-ABCD"

	// Malformed input is rejected before the API is called.
	for _, body := range []string{
		`{"provider":"openai_api","model":"gpt-5-mini"}`,
		`{"provider":"openai_api","api_key":"","model":"gpt-5-mini"}`,
		`{"provider":"openai_api","api_key":"` + key + `"}`,
		`{"provider":"openai_api","api_key":"` + key + `","model":"gpt 5"}`,
		`{"provider":"openai_api","api_key":"` + key + `","model":"` + strings.Repeat("m", 257) + `"}`,
		`{"provider":"openai_api","api_key":"sk-short","model":"gpt-5-mini"}`,
		`{"provider":"openai_api","api_key":1,"model":"gpt-5-mini"}`,
		`{"provider":"openai_api","api_key":"` + key + `","model":"gpt-5-mini","base_url":"https://api.openai.com/v1"}`,
		`{"provider":"openai_api","api_key":"` + key + `","model":"gpt-5-mini","base_url":null}`,
		`{"api_key":"sk-ant-api03-secret-value-WXYZ","model":"claude-haiku-5-5","base_url":"https://example.com"}`,
		`{"provider":"openai","model":"gpt-5","base_url":"https://example.com"}`,
		`{"provider":"openai_apis","api_key":"` + key + `","model":"gpt-5-mini"}`,
	} {
		expectStatus(t, api.do("PUT", path, api.alice, body), 400, "invalid_request")
	}
	if fc.count() != 0 {
		t.Fatalf("API called %d times for invalid input", fc.count())
	}

	put := `{"provider":"openai_api","api_key":" ` + key + ` ","model":"gpt-5-mini"}`
	for _, tc := range []struct {
		status int
		body   string
		want   int
		code   string
	}{
		{401, `{"error":{"code":"invalid_api_key"}}`, 400, "ai_key_invalid"},
		{404, `{"error":{"code":"model_not_found"}}`, 400, "ai_model_unavailable"},
		{429, `{"error":{"code":"insufficient_quota","type":"insufficient_quota"}}`, 400, "ai_billing"},
		{400, `{"error":{"code":"invalid_value","param":"response_format"}}`, 400, "ai_unsupported"},
		{429, `{"error":{"code":"rate_limit_exceeded"}}`, 503, "ai_unavailable"},
		{500, `{}`, 503, "ai_unavailable"},
	} {
		fc.set(tc.status, tc.body)
		w := api.do("PUT", path, api.alice, put)
		expectStatus(t, w, tc.want, tc.code)
		if tc.code == "ai_model_unavailable" && !strings.Contains(w.Body.String(), "The API key cannot use this model.") {
			t.Fatalf("404 message: %s", w.Body.String())
		}
		if _, err := api.store.AISettings(context.Background(), "alice-id"); err == nil {
			t.Fatalf("%d: saved", tc.status)
		}
	}

	fc.set(0, "")
	w := api.do("PUT", path, api.alice, put)
	got := decodeAI(t, w)
	if got.Provider != ai.ProviderOpenAIAPI || got.Model != "gpt-5-mini" || got.KeyLast4 != "ABCD" || !got.APIKeySet || got.BaseURL != nil || got.Account != nil || got.Status != "active" || strings.Contains(w.Body.String(), "secret") {
		t.Fatalf("saved: %s", w.Body.String())
	}
	r := fc.last()
	if r.path != "/v1/chat/completions" || r.auth != "Bearer "+key || r.body["model"] != "gpt-5-mini" || r.body["reasoning_effort"] != "low" {
		t.Fatalf("request: %+v", r)
	}
	a, err := api.store.AISettings(context.Background(), "alice-id")
	if err != nil || strings.Contains(a.APIKeySealed, "secret") {
		t.Fatalf("stored: %+v %v", a, err)
	}
	if e, err := aicred.OpenEndpoint(api.server.box, a); err != nil || e.APIKey != key || e.BaseURL != "" {
		t.Fatalf("open: %+v %v", e, err)
	}
	if _, err := api.server.box.Open(ai.KeyPurpose("alice-id"), a.APIKeySealed); err == nil {
		t.Fatal("sealed as an Anthropic key")
	}

	// The model is changed with the saved key (omitted or empty).
	for _, body := range []string{`{"provider":"openai_api","model":"gpt-5"}`, `{"provider":"openai_api","api_key":"","model":"gpt-5.1"}`} {
		before := fc.count()
		got = decodeAI(t, api.do("PUT", path, api.alice, body))
		if got.KeyLast4 != "ABCD" || !got.APIKeySet || fc.count() != before+1 || fc.last().auth != "Bearer "+key {
			t.Fatalf("model change %s: %+v", body, got)
		}
	}
	if got = decodeAI(t, api.do("GET", path, api.alice, "")); got.Model != "gpt-5.1" || got.BaseURL != nil {
		t.Fatalf("GET: %+v", got)
	}

	// Another provider is refused until the settings are deleted.
	expectStatus(t, api.do("PUT", path, api.alice, `{"api_key":"sk-ant-api03-secret-value-WXYZ","model":"claude-haiku-5-5"}`), 409, "ai_provider_mismatch")
	expectStatus(t, api.do("PUT", path, api.alice, `{"provider":"openai_compatible","base_url":"https://example.com/v1","api_key":"","model":"m"}`), 409, "ai_provider_mismatch")

	// A saved key that cannot be opened must be registered again.
	box := api.server.box
	api.server.box = secretbox.New([]byte(strings.Repeat("o", 32)))
	before := fc.count()
	expectStatus(t, api.do("PUT", path, api.alice, `{"provider":"openai_api","model":"gpt-5"}`), 400, "ai_key_invalid")
	if fc.count() != before {
		t.Fatal("API called with an undecryptable key")
	}
	api.server.box = box

	if w = api.do("DELETE", path, api.alice, ""); w.Code != 204 {
		t.Fatalf("DELETE: %d %s", w.Code, w.Body.String())
	}
	if w = api.do("GET", path, api.alice, ""); strings.TrimSpace(w.Body.String()) != `{"configured":false}` {
		t.Fatalf("after DELETE: %s", w.Body.String())
	}
	if strings.Contains(logs.String(), "secret-value") {
		t.Fatalf("key logged: %s", logs.String())
	}
}

func TestAIOpenAICompatible(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) { testAIOpenAICompatible(t, driver) })
	}
}

func testAIOpenAICompatible(t *testing.T, driver string) {
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	api := newIntegrationAPI(t, driver)
	fc := &fakeChat{}
	base := useEndpoint(t, api.server, fc) + "/v1"
	const path = "/api/v1/ai"
	const key = "ep-secret-value-7890"
	const model = "meta-llama/Llama-3.1-8B-Instruct"

	// Malformed input is rejected before the endpoint is called.
	for _, body := range []string{
		`{"provider":"openai_compatible","api_key":"","model":"m"}`,
		`{"provider":"openai_compatible","base_url":"` + base + `","model":"m"}`,
		`{"provider":"openai_compatible","base_url":null,"api_key":"","model":"m"}`,
		`{"provider":"openai_compatible","base_url":1,"api_key":"","model":"m"}`,
		`{"provider":"openai_compatible","base_url":"ftp://example.com","api_key":"","model":"m"}`,
		`{"provider":"openai_compatible","base_url":"https://example.com/v1?x=1","api_key":"","model":"m"}`,
		`{"provider":"openai_compatible","base_url":"https://user:pass@example.com/v1","api_key":"","model":"m"}`,
		`{"provider":"openai_compatible","base_url":"example.com/v1","api_key":"","model":"m"}`,
		`{"provider":"openai_compatible","base_url":"` + base + `","api_key":"with space","model":"m"}`,
		`{"provider":"openai_compatible","base_url":"` + base + `","api_key":"","model":"m m"}`,
		`{"provider":"openai_compatible","base_url":"` + base + `","api_key":""}`,
	} {
		expectStatus(t, api.do("PUT", path, api.alice, body), 400, "invalid_request")
	}
	if fc.count() != 0 {
		t.Fatalf("endpoint called %d times for invalid input", fc.count())
	}

	// Saved without a key; the trailing slash is removed.
	w := api.do("PUT", path, api.alice, `{"provider":"openai_compatible","base_url":" `+base+`/ ","api_key":"","model":"`+model+`"}`)
	got := decodeAI(t, w)
	if got.Provider != ai.ProviderOpenAICompatible || got.Model != model || got.BaseURL == nil || *got.BaseURL != base || got.APIKeySet || got.KeyLast4 != "" || got.Account != nil {
		t.Fatalf("saved: %s", w.Body.String())
	}
	if r := fc.last(); r.path != "/v1/chat/completions" || r.auth != "" || r.body["reasoning_effort"] != nil || r.body["model"] != model {
		t.Fatalf("request: %+v", r)
	}

	// A key is added with the saved URL.
	got = decodeAI(t, api.do("PUT", path, api.alice, `{"provider":"openai_compatible","api_key":"`+key+`","model":"`+model+`"}`))
	if !got.APIKeySet || got.KeyLast4 != "7890" || *got.BaseURL != base || fc.last().auth != "Bearer "+key {
		t.Fatalf("key added: %+v", got)
	}

	// The same URL without api_key keeps the saved key.
	got = decodeAI(t, api.do("PUT", path, api.alice, `{"provider":"openai_compatible","base_url":"`+base+`/","model":"llama3"}`))
	if got.Model != "llama3" || !got.APIKeySet || fc.last().auth != "Bearer "+key {
		t.Fatalf("same URL: %+v %+v", got, fc.last())
	}
	got = decodeAI(t, api.do("PUT", path, api.alice, `{"provider":"openai_compatible","model":"llama3.1"}`))
	if got.Model != "llama3.1" || fc.last().auth != "Bearer "+key {
		t.Fatalf("model only: %+v", got)
	}

	// The saved key is never sent to another URL.
	before := fc.count()
	expectStatus(t, api.do("PUT", path, api.alice, `{"provider":"openai_compatible","base_url":"`+base+`2","model":"llama3"}`), 400, "invalid_request")
	if fc.count() != before {
		t.Fatal("endpoint called for a changed URL without a key")
	}
	got = decodeAI(t, api.do("PUT", path, api.alice, `{"provider":"openai_compatible","base_url":"`+base+`2","api_key":"`+key+`","model":"llama3"}`))
	if *got.BaseURL != base+"2" || fc.last().path != "/v12/chat/completions" || fc.last().auth != "Bearer "+key {
		t.Fatalf("URL changed: %+v %+v", got, fc.last())
	}

	// An empty key removes the key.
	got = decodeAI(t, api.do("PUT", path, api.alice, `{"provider":"openai_compatible","api_key":"","model":"llama3"}`))
	if got.APIKeySet || got.KeyLast4 != "" || fc.last().auth != "" {
		t.Fatalf("key removed: %+v", got)
	}
	if got = decodeAI(t, api.do("GET", path, api.alice, "")); *got.BaseURL != base+"2" || got.APIKeySet {
		t.Fatalf("GET: %+v", got)
	}

	// Errors of the check.
	for _, tc := range []struct {
		status int
		body   string
		want   int
		code   string
		text   string
	}{
		{401, `{"error":"bad key"}`, 400, "ai_key_invalid", "The API key is invalid."},
		{404, `404 page not found`, 400, "ai_model_unavailable", "The endpoint or the model was not found."},
		{400, `{"error":{"message":"response_format is not supported"}}`, 400, "ai_unsupported", "structured output with a JSON schema"},
		{502, `bad gateway`, 503, "ai_unavailable", ""},
	} {
		fc.set(tc.status, tc.body)
		w = api.do("PUT", path, api.alice, `{"provider":"openai_compatible","model":"llama3"}`)
		expectStatus(t, w, tc.want, tc.code)
		if !strings.Contains(w.Body.String(), tc.text) {
			t.Fatalf("%d message: %s", tc.status, w.Body.String())
		}
	}
	fc.set(0, "")

	// A client limited to public addresses refuses the loopback address.
	api.server.ai.Endpoints = ai.NewEndpointClient(false)
	before = fc.count()
	w = api.do("PUT", path, api.alice, `{"provider":"openai_compatible","model":"llama3"}`)
	expectStatus(t, w, 400, "ai_endpoint_blocked")
	if fc.count() != before || strings.Contains(w.Body.String(), "127.0.0.1") {
		t.Fatalf("blocked: %d %s", fc.count(), w.Body.String())
	}
	api.server.ai.Endpoints = ai.NewEndpointClient(true)

	// http needs ASTOVIA_AI_ALLOW_PRIVATE_ENDPOINTS.
	api.server.cfg.AIAllowPrivateEndpoints = false
	expectStatus(t, api.do("PUT", path, api.alice, `{"provider":"openai_compatible","base_url":"`+base+`","api_key":"","model":"llama3"}`), 400, "ai_endpoint_blocked")
	if fc.count() != before {
		t.Fatal("endpoint called for an http URL")
	}
	api.server.cfg.AIAllowPrivateEndpoints = true

	// A saved endpoint that cannot be opened must be registered again when
	// its URL or key is kept.
	box := api.server.box
	api.server.box = secretbox.New([]byte(strings.Repeat("o", 32)))
	for _, body := range []string{
		`{"provider":"openai_compatible","model":"llama3"}`,
		`{"provider":"openai_compatible","api_key":"","model":"llama3"}`,
		`{"provider":"openai_compatible","base_url":"` + base + `","model":"llama3"}`,
	} {
		expectStatus(t, api.do("PUT", path, api.alice, body), 400, "ai_key_invalid")
	}
	if fc.count() != before {
		t.Fatal("endpoint called with undecryptable settings")
	}
	// The GET of such settings has no URL.
	if got = decodeAI(t, api.do("GET", path, api.alice, "")); got.BaseURL != nil || got.APIKeySet {
		t.Fatalf("undecryptable GET: %+v", got)
	}
	api.server.box = box

	expectStatus(t, api.do("PUT", path, api.alice, `{"provider":"openai_api","api_key":"sk-proj-0123456789abcdef","model":"gpt-5-mini"}`), 409, "ai_provider_mismatch")
	if w = api.do("DELETE", path, api.alice, ""); w.Code != 204 {
		t.Fatalf("DELETE: %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(logs.String(), "secret-value") || strings.Contains(logs.String(), "127.0.0.1") {
		t.Fatalf("key or URL logged: %s", logs.String())
	}
}

func TestAIEndpointRateLimit(t *testing.T) {
	api := newIntegrationAPI(t, "sqlite")
	fc := &fakeChat{}
	base := useEndpoint(t, api.server, fc) + "/v1"
	setLimit(api.server.aiEndpointLimiter, 10)
	// Invalid input is not counted.
	for i := 0; i < 5; i++ {
		expectStatus(t, api.do("PUT", "/api/v1/ai", api.alice, `{"provider":"openai_compatible","base_url":"ftp://x","api_key":"","model":"m"}`), 400, "invalid_request")
	}
	body := `{"provider":"openai_compatible","base_url":"` + base + `","api_key":"","model":"m"}`
	for i := 0; i < 10; i++ {
		if w := api.do("PUT", "/api/v1/ai", api.alice, body); w.Code != 200 {
			t.Fatalf("%d: %d %s", i, w.Code, w.Body.String())
		}
	}
	expectStatus(t, api.do("PUT", "/api/v1/ai", api.alice, body), 429, "rate_limited")
	if fc.count() != 10 {
		t.Fatalf("%d requests", fc.count())
	}
	// The limit is per user.
	if w := api.do("PUT", "/api/v1/ai", api.bob, body); w.Code != 200 {
		t.Fatalf("bob: %d %s", w.Code, w.Body.String())
	}
	// The server's limit is 10 a minute.
	if l := newServer(api.server.cfg, api.server.auth, nil, nil).aiEndpointLimiter; l == nil || l.limit != 10 || l.window != time.Minute {
		t.Fatalf("server limiter: %+v", l)
	}
}

// A try with an OpenAI-compatible endpoint judges twice through it.
func TestAIInstructionTryOpenAICompatible(t *testing.T) {
	api := newTryAPI(t, "sqlite")
	fc := &fakeChat{answers: []string{
		`{"ok":true}`,
		`{"decision":"task","task":"資料を見る","summary":"要約 task","event_title":"","event_start":"","event_end":"","topic":"","same_as":"","importance":"normal"}`,
		`{"decision":"none","task":"","summary":"要約 none","event_title":"","event_start":"","event_end":"","topic":"","same_as":"","importance":"normal"}`,
	}}
	base := useEndpoint(t, api.server, fc) + "/v1"
	refs := `[{"service":"slack","account":"Acme","conversation_id":"C1","conversation_name":"#general","message_id":"` + tryTS + `"}]`
	api.create(api.tryItem("mention", store.SourceSlack, tryMentionKey, 100, refs))
	api.put("/api/v1/integrations/slack", api.alice, `{"token":"`+aliceSlackToken+`"}`)
	api.put("/api/v1/ai", api.alice, `{"provider":"openai_compatible","base_url":"`+base+`","api_key":"ep-key-0123","model":"llama3"}`)
	api.sl.set(func(f *fakeSlackAPI) {
		f.answers["conversations.history"] = slackHistory(t, map[string]any{"type": "message", "user": "U2", "text": "<@U1> 資料を見てください", "ts": tryTS})
	})
	var detect tryDetect
	api.tryOK(store.InstructionSlackDetect, "mention", "", "書きかけ", &detect)
	want := tryDetect{
		Current: tryDetectResponse{Decision: ai.DecisionTask, Task: "資料を見る", Importance: ai.ImportanceNormal, Summary: "要約 task"},
		Draft:   tryDetectResponse{Decision: ai.DecisionNone, Importance: ai.ImportanceNormal, Summary: "要約 none"},
	}
	if detect != want {
		t.Fatalf("try:\n%+v\n%+v", detect, want)
	}
	if fc.count() != 3 {
		t.Fatalf("%d requests", fc.count())
	}
	r := fc.last()
	system := r.body["messages"].([]any)[0].(map[string]any)["content"].(string)
	if r.path != "/v1/chat/completions" || r.auth != "Bearer ep-key-0123" || r.body["model"] != "llama3" || !strings.Contains(system, "書きかけ") {
		t.Fatalf("request: %+v", r)
	}
}
