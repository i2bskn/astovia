package httpserver

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/i2bskn/astovia/internal/ai"
	"github.com/i2bskn/astovia/internal/aicred"
	"github.com/i2bskn/astovia/internal/openai"
	"github.com/i2bskn/astovia/internal/store"
)

const (
	planAccess  = "oai-access-SECRET"
	planRefresh = "oai-refresh-SECRET"
	planClient  = "oaiapp_CLIENT1"
	planEmail   = "alice@chatgpt.example"
	planScope   = "openid profile email offline_access resource.invoke chatgpt.tokens.use.direct"
)

// fakeOpenAIServer is auth.openai.com and api.openai.com for the ChatGPT
// plan.
type fakeOpenAIServer struct {
	mu  sync.Mutex
	key *rsa.PrivateKey
	// nonce is put in the ID tokens; scope in the token responses.
	nonce string
	scope string
	// exchangeStatus and exchangeBody replace the answer of a code
	// exchange when set.
	exchangeStatus int
	exchangeBody   string
	exchanges      []url.Values
	refreshes      int
	revokeStatus   int
	revoked        []url.Values
	models         []string
	// replies answer the Responses API by model ({"ok":true} otherwise);
	// hook runs before each answer.
	replies   map[string]func(http.ResponseWriter)
	hook      func()
	responses []string
}

func (f *fakeOpenAIServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	switch r.URL.Path {
	case "/.well-known/jwks.json":
		n, e := f.key.N.Bytes(), big.NewInt(int64(f.key.E)).Bytes()
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{"kty": "RSA", "kid": "k1", "use": "sig", "n": base64.RawURLEncoding.EncodeToString(n), "e": base64.RawURLEncoding.EncodeToString(e)}}})
	case "/api/accounts/oauth/token":
		form, _ := url.ParseQuery(string(raw))
		if form.Get("grant_type") == "refresh_token" {
			f.refreshes++
			n := f.refreshes
			f.mu.Unlock()
			_, _ = fmt.Fprintf(w, `{"access_token":"%s-r%d","refresh_token":"%s-r%d","token_type":"Bearer","expires_in":3600}`, planAccess, n, planRefresh, n)
			return
		}
		f.exchanges = append(f.exchanges, form)
		status, body, nonce, scope := f.exchangeStatus, f.exchangeBody, f.nonce, f.scope
		tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{"iss": openai.Issuer, "aud": form.Get("client_id"), "sub": "user-sub", "email": planEmail, "nonce": nonce, "exp": time.Date(2026, 10, 24, 3, 0, 0, 0, time.UTC).Unix()})
		tok.Header["kid"] = "k1"
		idToken, err := tok.SignedString(f.key)
		f.mu.Unlock()
		if err != nil {
			w.WriteHeader(500)
			return
		}
		if status != 0 {
			w.WriteHeader(status)
			_, _ = io.WriteString(w, body)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": planAccess, "refresh_token": planRefresh, "id_token": idToken, "token_type": "Bearer", "expires_in": 3600, "scope": scope})
	case "/api/accounts/oauth/revoke":
		form, _ := url.ParseQuery(string(raw))
		f.revoked = append(f.revoked, form)
		status := f.revokeStatus
		f.mu.Unlock()
		if status != 0 {
			w.WriteHeader(status)
		}
	case "/v1/models":
		list := []map[string]string{}
		for _, m := range f.models {
			list = append(list, map[string]string{"slug": m, "display_name": strings.ToUpper(m), "visibility": "list"})
		}
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"data": list})
	case "/v1/responses":
		var body struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(raw, &body)
		f.responses = append(f.responses, body.Model)
		reply, hook := f.replies[body.Model], f.hook
		f.mu.Unlock()
		if hook != nil {
			hook()
		}
		if reply == nil {
			reply = planCompleted(`{"ok":true}`)
		}
		reply(w)
	default:
		f.mu.Unlock()
		w.WriteHeader(404)
	}
}

func (f *fakeOpenAIServer) with(fn func(f *fakeOpenAIServer)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func planCompleted(text string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		data, _ := json.Marshal(map[string]any{"type": "response.completed", "response": map[string]any{"output": []any{map[string]any{"type": "message", "content": []any{map[string]any{"type": "output_text", "text": text}}}}}})
		_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
	}
}

func planFailure(status int, code string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.WriteHeader(status)
		_, _ = fmt.Fprintf(w, `{"error":{"code":%q,"param":"text.format"}}`, code)
	}
}

type openAIAPI struct {
	*integrationAPI
	fake *fakeOpenAIServer
}

func newOpenAIAPI(t *testing.T, driver string) *openAIAPI {
	t.Helper()
	api := newIntegrationAPI(t, driver)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeOpenAIServer{key: key, scope: planScope, models: []string{"gpt-a", "gpt-b"}, replies: map[string]func(http.ResponseWriter){}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	api.server.tokens.OpenAI.AuthBaseURL = srv.URL
	api.server.tokens.OpenAI.Now = func() time.Time { return *api.now }
	api.server.ai.OpenAI.APIBaseURL = srv.URL + "/v1"
	return &openAIAPI{integrationAPI: api, fake: f}
}

// authorize starts an authorization and returns its URL's query; the
// nonce is given to the fake.
func (a *openAIAPI) authorize(t *testing.T) url.Values {
	t.Helper()
	w := a.do("POST", "/api/v1/ai/openai/authorize", a.alice, `{}`)
	if w.Code != 200 {
		t.Fatalf("authorize: %d %s", w.Code, w.Body.String())
	}
	var out map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || len(out) != 2 || out["redirect_uri"] != openai.RedirectURI {
		t.Fatalf("authorize: %s %v", w.Body.String(), err)
	}
	u, err := url.Parse(out["url"])
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	a.fake.with(func(f *fakeOpenAIServer) { f.nonce = q.Get("nonce") })
	return q
}

func callback(state, extra string) string {
	return "http://127.0.0.1:1455/auth/callback?code=ac_SECRETCODE&state=" + state + extra
}

func (a *openAIAPI) complete(t *testing.T, cb string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"callback_url": cb})
	return a.do("POST", "/api/v1/ai/openai/complete", a.alice, string(body))
}

func (a *openAIAPI) settings(t *testing.T) (store.AISettings, aicred.Secret) {
	t.Helper()
	s, err := a.store.AISettings(context.Background(), "alice-id")
	if err != nil {
		t.Fatal(err)
	}
	secret, err := a.server.tokens.OpenSecret(s)
	if err != nil {
		t.Fatal(err)
	}
	return s, secret
}

func TestOpenAIAPI(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) { testOpenAIAPI(t, driver) })
	}
}

func testOpenAIAPI(t *testing.T, driver string) {
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	api := newOpenAIAPI(t, driver)
	f := api.fake

	// The first authorization registers a client.
	q := api.authorize(t)
	if q.Get("client_id") != openai.DynamicClientID || q.Get("agent_name_hint") != "Astovia" || q.Get("ext_agent_host_id") != openai.HostID(make([]byte, 32)) || q.Get("redirect_uri") != openai.RedirectURI || q.Get("code_challenge_method") != "S256" || !strings.HasPrefix(q.Get("state"), "oai_") {
		t.Fatalf("authorize URL: %v", q)
	}
	state := q.Get("state")
	// Addresses that cannot be read do not use the state up.
	for _, cb := range []string{"http://localhost:1455/auth/callback?code=c&state=" + state, "not a url", callback(state, "#frag")} {
		expectStatus(t, api.complete(t, cb), 400, "openai_callback_invalid")
	}
	expectStatus(t, api.do("POST", "/api/v1/ai/openai/complete", api.alice, `{"callback_url":1}`), 400, "invalid_request")
	// Without the client ID of the new registration.
	expectStatus(t, api.complete(t, callback(state, "")), 400, "openai_callback_invalid")
	// The state was used.
	expectStatus(t, api.complete(t, callback(state, "&client_id="+planClient)), 400, "openai_state_invalid")
	// Bob cannot use alice's state.
	state = api.authorize(t).Get("state")
	body, _ := json.Marshal(map[string]string{"callback_url": callback(state, "&client_id="+planClient)})
	expectStatus(t, api.do("POST", "/api/v1/ai/openai/complete", api.bob, string(body)), 400, "openai_state_invalid")
	expectStatus(t, api.complete(t, "http://127.0.0.1:1455/auth/callback?error=access_denied&state="+state), 400, "openai_denied")
	// The plan scope missing from the callback or from the token response.
	state = api.authorize(t).Get("state")
	expectStatus(t, api.complete(t, callback(state, "&client_id="+planClient+"&scope=openid+email")), 400, "openai_scope_missing")
	state = api.authorize(t).Get("state")
	f.with(func(f *fakeOpenAIServer) { f.scope = "openid email offline_access" })
	expectStatus(t, api.complete(t, callback(state, "&client_id="+planClient)), 400, "openai_scope_missing")
	f.with(func(f *fakeOpenAIServer) { f.scope = planScope })
	// A code OpenAI refuses.
	state = api.authorize(t).Get("state")
	f.with(func(f *fakeOpenAIServer) { f.exchangeStatus, f.exchangeBody = 400, `{"error":"invalid_grant"}` })
	expectStatus(t, api.complete(t, callback(state, "&client_id="+planClient)), 400, "openai_state_invalid")
	state = api.authorize(t).Get("state")
	f.with(func(f *fakeOpenAIServer) { f.exchangeStatus, f.exchangeBody = 500, `` })
	expectStatus(t, api.complete(t, callback(state, "&client_id="+planClient)), 502, "openai_unavailable")
	f.with(func(f *fakeOpenAIServer) { f.exchangeStatus = 0 })
	// An ID token with another nonce.
	state = api.authorize(t).Get("state")
	f.with(func(f *fakeOpenAIServer) { f.nonce = "other" })
	expectStatus(t, api.complete(t, callback(state, "&client_id="+planClient)), 502, "openai_unavailable")
	// A plan that is not eligible.
	state = api.authorize(t).Get("state")
	f.with(func(f *fakeOpenAIServer) {
		f.replies["gpt-a"] = planFailure(403, "subscription_sharing_user_not_eligible")
	})
	expectStatus(t, api.complete(t, callback(state, "&client_id="+planClient)), 400, "ai_billing")
	if _, err := api.store.AISettings(context.Background(), "alice-id"); err == nil {
		t.Fatal("saved after a failure")
	}
	// A model that does not accept the request: the next one is tried.
	state = api.authorize(t).Get("state")
	f.with(func(f *fakeOpenAIServer) {
		f.replies["gpt-a"] = planFailure(400, "subscription_sharing_unsupported_capability")
		f.responses = nil
	})
	w := api.complete(t, callback(state, "&client_id="+planClient+"&scope=openid+chatgpt.tokens.use.direct&iss=https%3A%2F%2Fauth.openai.com"))
	if w.Code != 200 {
		t.Fatalf("complete: %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "SECRET") || strings.Contains(w.Body.String(), planClient) {
		t.Fatalf("tokens in the response: %s", w.Body.String())
	}
	var got aiResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Configured || got.Provider != ai.ProviderOpenAI || got.Model != "gpt-b" || got.KeyLast4 != "" || got.Account == nil || *got.Account != planEmail || got.Status != store.AIActive {
		t.Fatalf("saved: %+v", got)
	}
	f.with(func(f *fakeOpenAIServer) {
		if strings.Join(f.responses, ",") != "gpt-a,gpt-b" {
			t.Errorf("checks: %v", f.responses)
		}
		last := f.exchanges[len(f.exchanges)-1]
		if last.Get("client_id") != planClient || last.Get("redirect_uri") != openai.RedirectURI || last.Get("code") != "ac_SECRETCODE" || last.Get("code_verifier") == "" || last.Get("resource") != openai.Resource {
			t.Errorf("exchange: %v", last)
		}
	})
	a, secret := api.settings(t)
	if a.ActivatedAt != api.now.Unix() || a.KeyLast4 != "" || strings.Contains(a.APIKeySealed, "SECRET") || secret.ClientID != planClient || secret.Subject != "user-sub" || secret.Email != planEmail || secret.RefreshToken != planRefresh || secret.AccessToken != planAccess || secret.ExpiresAt != api.now.Add(time.Hour).Unix() {
		t.Fatalf("stored: %+v %+v", a, secret)
	}
	// The same address again.
	expectStatus(t, api.complete(t, callback(state, "&client_id="+planClient)), 400, "openai_state_invalid")

	// GET and the model list.
	if w = api.do("GET", "/api/v1/ai", api.alice, ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"account":"`+planEmail+`"`) || !strings.Contains(w.Body.String(), `"provider":"openai"`) {
		t.Fatalf("GET: %d %s", w.Code, w.Body.String())
	}
	if w = api.do("GET", "/api/v1/ai/openai/models", api.alice, ""); w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"models":[{"id":"gpt-a","name":"GPT-A"},{"id":"gpt-b","name":"GPT-B"}]}` {
		t.Fatalf("models: %d %s", w.Code, w.Body.String())
	}
	expectStatus(t, api.do("GET", "/api/v1/ai/openai/models", api.bob, ""), 404, "not_found")

	// PUT: the provider must stay the same, and the model is checked.
	expectStatus(t, api.do("PUT", "/api/v1/ai", api.alice, `{"model":"claude-haiku-5-5"}`), 409, "ai_provider_mismatch")
	expectStatus(t, api.do("PUT", "/api/v1/ai", api.alice, `{"model":"gpt-a","provider":"openai","api_key":"sk-ant-0123456789012345678"}`), 400, "invalid_request")
	expectStatus(t, api.do("PUT", "/api/v1/ai", api.alice, `{"model":"bad model","provider":"openai"}`), 400, "invalid_request")
	expectStatus(t, api.do("PUT", "/api/v1/ai", api.alice, `{"model":"gpt-z","provider":"openai"}`), 400, "ai_model_unavailable")
	expectStatus(t, api.do("PUT", "/api/v1/ai", api.alice, `{"model":"gpt-a","provider":"openai"}`), 400, "ai_unsupported")
	f.with(func(f *fakeOpenAIServer) { delete(f.replies, "gpt-a") })
	*api.now = api.now.Add(time.Minute)
	w = api.do("PUT", "/api/v1/ai", api.alice, `{"model":"gpt-a","provider":"openai"}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"model":"gpt-a"`) {
		t.Fatalf("model change: %d %s", w.Code, w.Body.String())
	}
	b, _ := api.settings(t)
	if b.Generation != a.Generation+1 || b.ActivatedAt != a.ActivatedAt || b.APIKeySealed != a.APIKeySealed {
		t.Fatalf("after the model change: %+v", b)
	}
	// Settings saved by another request during the check.
	f.with(func(f *fakeOpenAIServer) {
		f.hook = func() {
			f.with(func(f *fakeOpenAIServer) { f.hook = nil })
			if w := api.do("PUT", "/api/v1/ai", api.alice, `{"model":"gpt-a","provider":"openai"}`); w.Code != 200 {
				t.Errorf("concurrent PUT: %d %s", w.Code, w.Body.String())
			}
		}
	})
	expectStatus(t, api.do("PUT", "/api/v1/ai", api.alice, `{"model":"gpt-b","provider":"openai"}`), 409, "ai_conflict")

	// Connecting again signs in with the registered client; a saved model
	// that the plan no longer has is replaced by the light one (Luna).
	q = api.authorize(t)
	if q.Get("client_id") != planClient || q.Has("agent_name_hint") {
		t.Fatalf("reconnect URL: %v", q)
	}
	expectStatus(t, api.complete(t, callback(q.Get("state"), "&client_id=oaiapp_OTHER")), 400, "openai_callback_invalid")
	q = api.authorize(t)
	f.with(func(f *fakeOpenAIServer) { f.models = []string{"gpt-c", "gpt-luna", "gpt-b"} })
	*api.now = api.now.Add(time.Minute)
	if w = api.complete(t, callback(q.Get("state"), "")); w.Code != 200 || !strings.Contains(w.Body.String(), `"model":"gpt-luna"`) {
		t.Fatalf("reconnect: %d %s", w.Code, w.Body.String())
	}
	if c, _ := api.settings(t); c.ActivatedAt != api.now.Unix() {
		t.Fatalf("reconnect activation: %+v", c)
	}

	// DELETE revokes the refresh token.
	if w = api.do("DELETE", "/api/v1/ai", api.alice, ""); w.Code != 204 {
		t.Fatalf("DELETE: %d %s", w.Code, w.Body.String())
	}
	f.with(func(f *fakeOpenAIServer) {
		if len(f.revoked) != 1 || f.revoked[0].Get("token") != planRefresh || f.revoked[0].Get("token_type_hint") != "refresh_token" || f.revoked[0].Get("client_id") != planClient {
			t.Errorf("revoked: %v", f.revoked)
		}
	})
	// A failed revocation still removes the settings.
	q = api.authorize(t)
	if q.Get("client_id") != openai.DynamicClientID {
		t.Fatalf("after delete: %v", q)
	}
	if w = api.complete(t, callback(q.Get("state"), "&client_id="+planClient)); w.Code != 200 {
		t.Fatalf("connect: %d %s", w.Code, w.Body.String())
	}
	f.with(func(f *fakeOpenAIServer) { f.revokeStatus = 400 })
	if w = api.do("DELETE", "/api/v1/ai", api.alice, ""); w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"revoke_failed":true}` {
		t.Fatalf("DELETE with a failed revocation: %d %s", w.Code, w.Body.String())
	}
	if w = api.do("GET", "/api/v1/ai", api.alice, ""); strings.TrimSpace(w.Body.String()) != `{"configured":false}` {
		t.Fatalf("after DELETE: %s", w.Body.String())
	}

	// With an Anthropic key, ChatGPT cannot be connected.
	q = api.authorize(t)
	fm := &fakeModels{}
	srv := httptest.NewServer(fm)
	t.Cleanup(srv.Close)
	api.server.ai.BaseURL = srv.URL
	api.connectAI(t, "sk-ant-api03-secret-value-WXYZ")
	expectStatus(t, api.do("POST", "/api/v1/ai/openai/authorize", api.alice, `{}`), 409, "ai_provider_mismatch")
	expectStatus(t, api.complete(t, callback(q.Get("state"), "&client_id="+planClient)), 409, "ai_provider_mismatch")
	expectStatus(t, api.do("PUT", "/api/v1/ai", api.alice, `{"model":"gpt-a","provider":"openai"}`), 409, "ai_provider_mismatch")
	expectStatus(t, api.do("GET", "/api/v1/ai/openai/models", api.alice, ""), 404, "not_found")
	if w = api.do("DELETE", "/api/v1/ai", api.alice, ""); w.Code != 204 {
		t.Fatalf("DELETE anthropic: %d", w.Code)
	}

	// API tokens cannot use the routes.
	tok := (&tokenAPI{t: t, handler: api.handler}).issue(api.alice, "x").Token
	expectStatus(t, api.do("POST", "/api/v1/ai/openai/authorize", tok, `{}`), 401, "unauthorized")
	expectStatus(t, api.do("POST", "/api/v1/ai/openai/complete", tok, `{}`), 401, "unauthorized")
	expectStatus(t, api.do("GET", "/api/v1/ai/openai/models", tok, ""), 401, "unauthorized")

	for _, secret := range []string{"SECRET", planClient, planEmail} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("%s logged: %s", secret, logs.String())
		}
	}
}

// A token refreshed while a model change is checked is kept: the save uses
// the secret read again under the lock.
func TestOpenAIPutKeepsRefreshedToken(t *testing.T) {
	api := newOpenAIAPI(t, "sqlite")
	q := api.authorize(t)
	if w := api.complete(t, callback(q.Get("state"), "&client_id="+planClient)); w.Code != 200 {
		t.Fatalf("connect: %d %s", w.Code, w.Body.String())
	}
	// The access token expires soon, so the list or the check refreshes it.
	a, secret := api.settings(t)
	secret.ExpiresAt = api.now.Add(2 * time.Minute).Unix()
	sealed, err := api.server.tokens.Seal("alice-id", secret)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := api.store.UpdateAISecret(context.Background(), "alice-id", a.Generation, a.APIKeySealed, sealed); err != nil || !ok {
		t.Fatalf("expire: %v %v", ok, err)
	}
	if w := api.do("PUT", "/api/v1/ai", api.alice, `{"model":"gpt-b","provider":"openai"}`); w.Code != 200 {
		t.Fatalf("PUT: %d %s", w.Code, w.Body.String())
	}
	_, secret = api.settings(t)
	if secret.RefreshToken != planRefresh+"-r1" || secret.AccessToken != planAccess+"-r1" {
		t.Fatalf("refreshed token lost: %+v", secret)
	}
}
