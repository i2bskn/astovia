package httpserver

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/i2bskn/astovia/internal/config"
	"github.com/i2bskn/astovia/internal/gmailsync"
	"github.com/i2bskn/astovia/internal/google"
	"github.com/i2bskn/astovia/internal/store"
)

const (
	gmailCode    = "4/0AXcode-secret"
	gmailRefresh = "1//refresh-secret"
	gmailAccess  = "ya29.access-secret"
)

// fakeGoogleOAuth answers the token, profile and revoke endpoints.
type fakeGoogleOAuth struct {
	mu            sync.Mutex
	email         string
	scope         string
	noRefresh     bool
	tokenStatus   int
	tokenBody     string
	profileStatus int
	profileBody   string
	revokeStatus  int
	tokenHook     func()
	tokenForms    []url.Values
	profiles      int
	revoked       []string
	revokeQueries []string
}

func (f *fakeGoogleOAuth) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	form, _ := url.ParseQuery(string(raw))
	f.mu.Lock()
	hook := f.tokenHook
	f.mu.Unlock()
	if r.URL.Path == "/token" && hook != nil {
		hook()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/token":
		f.tokenForms = append(f.tokenForms, form)
		if f.tokenStatus != 0 {
			w.WriteHeader(f.tokenStatus)
			_, _ = w.Write([]byte(f.tokenBody))
			return
		}
		out := map[string]any{"access_token": gmailAccess, "expires_in": 3599, "scope": f.scope, "token_type": "Bearer"}
		if !f.noRefresh {
			out["refresh_token"] = gmailRefresh
		}
		_ = json.NewEncoder(w).Encode(out)
	case "/gmail/profile":
		f.profiles++
		if r.Header.Get("Authorization") != "Bearer "+gmailAccess {
			w.WriteHeader(401)
			return
		}
		if f.profileStatus != 0 {
			w.WriteHeader(f.profileStatus)
			_, _ = w.Write([]byte(f.profileBody))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"emailAddress": f.email})
	case "/revoke":
		f.revoked = append(f.revoked, form.Get("token"))
		f.revokeQueries = append(f.revokeQueries, r.URL.RawQuery)
		if f.revokeStatus != 0 {
			w.WriteHeader(f.revokeStatus)
		}
	default:
		w.WriteHeader(404)
	}
}

func (f *fakeGoogleOAuth) set(fn func(f *fakeGoogleOAuth)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *fakeGoogleOAuth) revokes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.revoked...)
}

func (f *fakeGoogleOAuth) exchanges() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.tokenForms)
}

type gmailAPI struct {
	*integrationAPI
	g *fakeGoogleOAuth
}

func newGmailAPI(t *testing.T, driver string) *gmailAPI {
	t.Helper()
	api := newIntegrationAPI(t, driver)
	g := &fakeGoogleOAuth{email: "Alice@Example.com", scope: google.ScopeGmailReadonly + " openid"}
	srv := httptest.NewServer(g)
	t.Cleanup(srv.Close)
	api.server.google.TokenEndpoint, api.server.google.RevokeEndpoint, api.server.google.GmailBaseURL = srv.URL+"/token", srv.URL+"/revoke", srv.URL+"/gmail"
	api.server.cfg.Google = &config.GoogleClient{ID: "client-id", Secret: "client-secret", RedirectURI: "https://astovia.test/integrations/google/callback"}
	return &gmailAPI{integrationAPI: api, g: g}
}

// authorize starts an authorization and returns its URL's query.
func (a *gmailAPI) authorize(jwt, body string) url.Values {
	a.t.Helper()
	w := a.do("POST", "/api/v1/integrations/gmail/authorize", jwt, body)
	if w.Code != 200 {
		a.t.Fatalf("authorize: %d %s", w.Code, w.Body.String())
	}
	var out struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		a.t.Fatal(err)
	}
	u, err := url.Parse(out.URL)
	if err != nil || u.Scheme != "https" || u.Host != "accounts.google.com" {
		a.t.Fatalf("url: %s %v", out.URL, err)
	}
	return u.Query()
}

func (a *gmailAPI) complete(jwt, state, code string) *httptest.ResponseRecorder {
	a.t.Helper()
	body, _ := json.Marshal(map[string]string{"state": state, "code": code})
	return a.do("POST", "/api/v1/integrations/gmail/complete", jwt, string(body))
}

// connect connects the fake's address for the user and returns it.
func (a *gmailAPI) connect(jwt string) gmailAccountResponse {
	a.t.Helper()
	w := a.complete(jwt, a.authorize(jwt, `{}`).Get("state"), gmailCode)
	if w.Code != 200 {
		a.t.Fatalf("complete: %d %s", w.Code, w.Body.String())
	}
	var out gmailAccountResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		a.t.Fatal(err)
	}
	return out
}

func (a *gmailAPI) state(jwt string) gmailResponse {
	a.t.Helper()
	w := a.do("GET", "/api/v1/integrations/gmail", jwt, "")
	if w.Code != 200 {
		a.t.Fatalf("GET: %d %s", w.Code, w.Body.String())
	}
	var out gmailResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		a.t.Fatal(err)
	}
	return out
}

func TestGmailIntegrationAPI(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) { testGmailIntegrationAPI(t, driver) })
	}
}

func testGmailIntegrationAPI(t *testing.T, driver string) {
	api := newGmailAPI(t, driver)
	ctx := context.Background()

	// Without accounts.
	w := api.do("GET", "/api/v1/integrations/gmail", api.alice, "")
	_, keys := jsonKeys(t, w.Body.Bytes())
	if w.Code != 200 || !reflect.DeepEqual(keys, []string{"accounts", "available", "max_accounts", "redirect_uri"}) ||
		!strings.Contains(w.Body.String(), `"available":true`) || !strings.Contains(w.Body.String(), `"redirect_uri":"https://astovia.test/integrations/google/callback"`) ||
		!strings.Contains(w.Body.String(), `"max_accounts":10`) || !strings.Contains(w.Body.String(), `"accounts":[]`) {
		t.Fatalf("empty: %d %s", w.Code, w.Body.String())
	}

	// The consent URL.
	q := api.authorize(api.alice, `{}`)
	state := q.Get("state")
	if !googleStatePattern.MatchString(state) || q.Get("client_id") != "client-id" || q.Get("redirect_uri") != "https://astovia.test/integrations/google/callback" ||
		q.Get("scope") != google.ScopeGmailReadonly || q.Get("code_challenge_method") != "S256" || len(q.Get("code_challenge")) != 43 || q.Get("access_type") != "offline" ||
		q.Get("prompt") != "consent select_account" || q.Get("include_granted_scopes") != "false" || q.Get("response_type") != "code" || q.Has("login_hint") {
		t.Fatalf("query: %v", q)
	}

	// Malformed input is refused before the state is consumed.
	for _, body := range []string{`{}`, `{"state":"` + state + `"}`, `{"state":"` + state + `","code":"c","error":"access_denied"}`, `{"state":"gst_short","code":"c"}`,
		`{"state":"` + state + `","code":""}`, `{"state":"` + state + `","code":"a b"}`, `{"state":"` + state + `","code":"` + strings.Repeat("c", 2049) + `"}`,
		`{"state":"` + state + `","error":"Access-Denied"}`, `{"state":"` + state + `","code":null}`, `{"state":1,"code":"c"}`, `{"state":"` + state + `","code":"c","x":1}`} {
		expectStatus(t, api.do("POST", "/api/v1/integrations/gmail/complete", api.alice, body), 400, "invalid_request")
	}
	// Another user cannot use the state, and it stays usable.
	expectStatus(t, api.complete(api.bob, state, gmailCode), 400, "gmail_state_invalid")
	if api.g.exchanges() != 0 {
		t.Fatalf("exchanged: %d", api.g.exchanges())
	}
	w = api.complete(api.alice, state, gmailCode)
	if w.Code != 200 {
		t.Fatalf("complete: %d %s", w.Code, w.Body.String())
	}
	_, keys = jsonKeys(t, w.Body.Bytes())
	var acct gmailAccountResponse
	if err := json.Unmarshal(w.Body.Bytes(), &acct); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(keys, []string{"created_at", "email", "id", "immediate_notify", "last_error", "last_synced_at", "status"}) || acct.Email != "alice@example.com" ||
		!acct.ImmediateNotify || acct.Status != "active" || acct.LastError != nil || acct.LastSyncedAt != nil || acct.CreatedAt != "2026-10-24T01:02:03Z" || !gmailAccountIDPattern.MatchString(acct.ID) {
		t.Fatalf("account: %s", w.Body.String())
	}
	api.g.mu.Lock()
	form := api.g.tokenForms[0]
	api.g.mu.Unlock()
	if form.Get("code") != gmailCode || form.Get("grant_type") != "authorization_code" || form.Get("redirect_uri") != "https://astovia.test/integrations/google/callback" ||
		form.Get("client_secret") != "client-secret" || google.Challenge(form.Get("code_verifier")) != q.Get("code_challenge") {
		t.Fatalf("token request: %v", form)
	}
	stored, err := api.store.GmailAccount(ctx, "alice-id", acct.ID)
	if err != nil || strings.Contains(stored.TokenSealed, gmailRefresh) {
		t.Fatalf("stored: %+v %v", stored, err)
	}
	if plain, err := api.server.box.Open(gmailsync.TokenPurpose("alice-id", acct.ID), stored.TokenSealed); err != nil || plain != gmailRefresh {
		t.Fatalf("open: %q %v", plain, err)
	}
	for _, s := range []string{gmailRefresh, gmailAccess, gmailCode} {
		if strings.Contains(w.Body.String(), s) {
			t.Fatalf("secret in the response: %s", s)
		}
	}
	// The second use of the state is refused.
	expectStatus(t, api.complete(api.alice, state, gmailCode), 400, "gmail_state_invalid")
	if got := api.state(api.alice); len(got.Accounts) != 1 || got.Accounts[0] != acct {
		t.Fatalf("GET: %+v", got)
	}
	if got := api.state(api.bob); len(got.Accounts) != 0 {
		t.Fatalf("bob: %+v", got)
	}

	// An expired state.
	state = api.authorize(api.alice, `{}`).Get("state")
	*api.now = api.now.Add(10 * time.Minute)
	// The access token expires with the state; a new sign-in is needed.
	fresh := (&tokenAPI{t: t, handler: api.handler}).login("alice")
	expectStatus(t, api.complete(fresh, state, gmailCode), 400, "gmail_state_invalid")
	*api.now = api.now.Add(-10 * time.Minute)

	// A denial consumes the state.
	state = api.authorize(api.alice, `{}`).Get("state")
	expectStatus(t, api.do("POST", "/api/v1/integrations/gmail/complete", api.alice, `{"state":"`+state+`","error":"access_denied"}`), 400, "gmail_denied")
	expectStatus(t, api.complete(api.alice, state, gmailCode), 400, "gmail_state_invalid")

	// Failures of Google; none revokes the token received.
	n := api.g.exchanges()
	for _, tc := range []struct {
		name string
		fn   func(f *fakeGoogleOAuth)
		want int
		code string
	}{
		{"scope", func(f *fakeGoogleOAuth) { f.scope = "openid email" }, 400, "gmail_scope_missing"},
		{"no refresh token", func(f *fakeGoogleOAuth) { f.noRefresh = true }, 502, "gmail_exchange_failed"},
		{"invalid_grant", func(f *fakeGoogleOAuth) { f.tokenStatus, f.tokenBody = 400, `{"error":"invalid_grant"}` }, 502, "gmail_exchange_failed"},
		{"policy", func(f *fakeGoogleOAuth) { f.tokenStatus, f.tokenBody = 400, `{"error":"admin_policy_enforced"}` }, 403, "gmail_policy_blocked"},
		{"api disabled", func(f *fakeGoogleOAuth) {
			f.profileStatus, f.profileBody = 403, `{"error":{"errors":[{"reason":"accessNotConfigured"}]}}`
		}, 502, "gmail_api_disabled"},
		{"profile policy", func(f *fakeGoogleOAuth) {
			f.profileStatus, f.profileBody = 403, `{"error":{"errors":[{"reason":"domainPolicy"}]}}`
		}, 403, "gmail_policy_blocked"},
		{"profile 500", func(f *fakeGoogleOAuth) { f.profileStatus = 500 }, 502, "gmail_exchange_failed"},
		{"bad address", func(f *fakeGoogleOAuth) { f.email = "a:b@example.com" }, 502, "gmail_exchange_failed"},
	} {
		api.g.set(func(f *fakeGoogleOAuth) {
			f.email, f.scope, f.noRefresh, f.tokenStatus, f.tokenBody, f.profileStatus, f.profileBody = "Alice@Example.com", google.ScopeGmailReadonly, false, 0, "", 0, ""
			tc.fn(f)
		})
		state = api.authorize(api.alice, `{}`).Get("state")
		w = api.complete(api.alice, state, gmailCode)
		expectStatus(t, w, tc.want, tc.code)
		if strings.Contains(w.Body.String(), gmailCode) || strings.Contains(w.Body.String(), "example.com") {
			t.Fatalf("%s: secret in the response: %s", tc.name, w.Body.String())
		}
	}
	if api.g.exchanges() != n+8 || len(api.g.revokes()) != 0 {
		t.Fatalf("exchanges %d revokes %v", api.g.exchanges()-n, api.g.revokes())
	}
	api.g.set(func(f *fakeGoogleOAuth) {
		f.email, f.scope, f.noRefresh, f.tokenStatus, f.tokenBody, f.profileStatus, f.profileBody = "Alice@Example.com", google.ScopeGmailReadonly, false, 0, "", 0, ""
	})

	// The address is connected by alice: bob is refused, without a revoke.
	expectStatus(t, api.complete(api.bob, api.authorize(api.bob, `{}`).Get("state"), gmailCode), 409, "gmail_in_use")
	// Alice connecting it again keeps the row.
	if again := api.connect(api.alice); again.ID != acct.ID {
		t.Fatalf("reconnected: %+v", again)
	}
	if s2, _ := api.store.GmailAccount(ctx, "alice-id", acct.ID); s2.Generation != 2 {
		t.Fatalf("generation: %+v", s2)
	}
	if len(api.g.revokes()) != 0 {
		t.Fatalf("revoked: %v", api.g.revokes())
	}

	// Reconnecting an account passes its address as the login hint, even
	// at the limit; another user's account is not found.
	q = api.authorize(api.alice, `{"account_id":"`+acct.ID+`"}`)
	if q.Get("login_hint") != "alice@example.com" {
		t.Fatalf("login_hint: %v", q)
	}
	expectStatus(t, api.do("POST", "/api/v1/integrations/gmail/authorize", api.bob, `{"account_id":"`+acct.ID+`"}`), 404, "not_found")
	expectStatus(t, api.do("POST", "/api/v1/integrations/gmail/authorize", api.alice, `{"account_id":"x/../y"}`), 404, "not_found")
	expectStatus(t, api.do("POST", "/api/v1/integrations/gmail/authorize", api.alice, `{"account_id":1}`), 400, "invalid_request")
	for i := 2; i <= store.MaxGmailAccounts; i++ {
		api.g.set(func(f *fakeGoogleOAuth) { f.email = "a" + strings.Repeat("x", i) + "@example.com" })
		api.connect(api.alice)
	}
	expectStatus(t, api.do("POST", "/api/v1/integrations/gmail/authorize", api.alice, `{}`), 409, "gmail_too_many")
	if q = api.authorize(api.alice, `{"account_id":"`+acct.ID+`"}`); q.Get("login_hint") != "alice@example.com" {
		t.Fatalf("reconnect at the limit: %v", q)
	}
	// A reconnection that Google answers with another address is a new
	// account, refused at the limit.
	api.g.set(func(f *fakeGoogleOAuth) { f.email = "eleven@example.com" })
	expectStatus(t, api.complete(api.alice, q.Get("state"), gmailCode), 409, "gmail_too_many")
	// Answered with the account's own address, it reconnects the row.
	api.g.set(func(f *fakeGoogleOAuth) { f.email = "alice@example.com" })
	if again := api.connect2(api.alice, acct.ID); again.ID != acct.ID {
		t.Fatalf("reconnect: %+v", again)
	}
	if len(api.g.revokes()) != 0 {
		t.Fatalf("revoked: %v", api.g.revokes())
	}

	// PATCH and DELETE, and another user's account.
	path := "/api/v1/integrations/gmail/accounts/" + acct.ID
	for _, body := range []string{`{}`, `{"immediate_notify":null}`, `{"immediate_notify":1}`, `{"immediate_notify":false,"x":1}`} {
		expectStatus(t, api.do("PATCH", path, api.alice, body), 400, "invalid_request")
	}
	if w = api.do("PATCH", path, api.alice, `{"immediate_notify":false}`); w.Code != 200 || !strings.Contains(w.Body.String(), `"immediate_notify":false`) {
		t.Fatalf("PATCH: %d %s", w.Code, w.Body.String())
	}
	expectStatus(t, api.do("PATCH", path, api.bob, `{"immediate_notify":true}`), 404, "not_found")
	expectStatus(t, api.do("PATCH", "/api/v1/integrations/gmail/accounts/short", api.alice, `{"immediate_notify":true}`), 404, "not_found")
	expectStatus(t, api.do("DELETE", path, api.bob, ""), 404, "not_found")
	expectStatus(t, api.do("DELETE", "/api/v1/integrations/gmail/accounts/short", api.alice, ""), 404, "not_found")
	if w = api.do("DELETE", path, api.alice, ""); w.Code != 204 {
		t.Fatalf("DELETE: %d %s", w.Code, w.Body.String())
	}
	if got := api.g.revokes(); !reflect.DeepEqual(got, []string{gmailRefresh}) || api.g.revokeQueries[0] != "" {
		t.Fatalf("revoke: %v %v", got, api.g.revokeQueries)
	}
	expectStatus(t, api.do("DELETE", path, api.alice, ""), 404, "not_found")
	// A failed revocation does not fail the removal.
	second := api.state(api.alice).Accounts[0]
	api.g.set(func(f *fakeGoogleOAuth) { f.revokeStatus = 500 })
	if w = api.do("DELETE", "/api/v1/integrations/gmail/accounts/"+second.ID, api.alice, ""); w.Code != 204 {
		t.Fatalf("DELETE with failed revoke: %d %s", w.Code, w.Body.String())
	}
	if len(api.g.revokes()) != 2 {
		t.Fatalf("revokes: %v", api.g.revokes())
	}

	// A JSON body is required, and API tokens and anonymous requests are
	// refused.
	for _, p := range []string{"/api/v1/integrations/gmail/authorize", "/api/v1/integrations/gmail/complete"} {
		r := httptest.NewRequest("POST", p, strings.NewReader(`{}`))
		r.Header.Set("Authorization", "Bearer "+api.alice)
		expectStatus(t, serve(t, api.handler, r), 415, "unsupported_media_type")
	}
	r := httptest.NewRequest("PATCH", path, strings.NewReader(`{"immediate_notify":true}`))
	r.Header.Set("Authorization", "Bearer "+api.alice)
	expectStatus(t, serve(t, api.handler, r), 415, "unsupported_media_type")
	tok := (&tokenAPI{t: t, handler: api.handler}).issue(api.alice, "x").Token
	for _, c := range []struct{ method, path string }{{"GET", "/api/v1/integrations/gmail"}, {"POST", "/api/v1/integrations/gmail/authorize"}, {"POST", "/api/v1/integrations/gmail/complete"}, {"PATCH", path}, {"DELETE", path}} {
		expectStatus(t, api.do(c.method, c.path, tok, `{}`), 401, "unauthorized")
		expectStatus(t, api.do(c.method, c.path, "", `{}`), 401, "unauthorized")
	}
}

// connect2 reconnects an account through authorize with its ID.
func (a *gmailAPI) connect2(jwt, accountID string) gmailAccountResponse {
	a.t.Helper()
	w := a.complete(jwt, a.authorize(jwt, `{"account_id":"`+accountID+`"}`).Get("state"), gmailCode)
	if w.Code != 200 {
		a.t.Fatalf("complete: %d %s", w.Code, w.Body.String())
	}
	var out gmailAccountResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		a.t.Fatal(err)
	}
	return out
}

// A session revoked while Google is called cannot save the account.
func TestGmailCompleteRevokedSession(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			api := newGmailAPI(t, driver)
			state := api.authorize(api.alice, `{}`).Get("state")
			api.g.set(func(f *fakeGoogleOAuth) {
				f.tokenHook = func() {
					if err := api.store.ChangeUser(context.Background(), "alice@example.com", "new", false, api.now.Unix()); err != nil {
						t.Error(err)
					}
				}
			})
			expectStatus(t, api.complete(api.alice, state, gmailCode), 401, "unauthorized")
			if list, err := api.store.GmailAccounts(context.Background(), "alice-id"); err != nil || len(list) != 0 {
				t.Fatalf("saved: %v %v", list, err)
			}
			if len(api.g.revokes()) != 0 {
				t.Fatalf("revoked: %v", api.g.revokes())
			}
		})
	}
}

// Without the OAuth client, the accounts are listed and can be changed and
// removed (the revocation needs no client), but none can be connected.
func TestGmailUnavailable(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			api := newGmailAPI(t, driver)
			acct := api.connect(api.alice)
			state := api.authorize(api.alice, `{}`).Get("state")
			api.server.cfg.Google = nil
			got := api.state(api.alice)
			if got.Available || got.RedirectURI != "https://astovia.test/integrations/google/callback" || len(got.Accounts) != 1 || got.Accounts[0].ID != acct.ID {
				t.Fatalf("GET: %+v", got)
			}
			expectStatus(t, api.do("POST", "/api/v1/integrations/gmail/authorize", api.alice, `{}`), 409, "gmail_unavailable")
			expectStatus(t, api.complete(api.alice, state, gmailCode), 409, "gmail_unavailable")
			path := "/api/v1/integrations/gmail/accounts/" + acct.ID
			if w := api.do("PATCH", path, api.alice, `{"immediate_notify":false}`); w.Code != 200 {
				t.Fatalf("PATCH: %d %s", w.Code, w.Body.String())
			}
			if w := api.do("DELETE", path, api.alice, ""); w.Code != 204 || len(api.g.revokes()) != 1 {
				t.Fatalf("DELETE: %d %v", w.Code, api.g.revokes())
			}
			if got = api.state(api.alice); len(got.Accounts) != 0 {
				t.Fatalf("after DELETE: %+v", got)
			}
		})
	}
}

// The Google callback is a screen of the web UI.
func TestGoogleCallbackScreen(t *testing.T) {
	api := newIntegrationAPI(t, "sqlite")
	for _, p := range []string{"/integrations/google/callback?state=gst_x&code=c", "/integrations/google/callback/"} {
		w := serve(t, api.handler, httptest.NewRequest("GET", p, nil))
		if w.Code != 200 || w.Body.String() != "<html>" {
			t.Fatalf("%s: %d %s", p, w.Code, w.Body.String())
		}
	}
	if w := serve(t, api.handler, httptest.NewRequest("GET", "/integrations/google", nil)); w.Code != 404 {
		t.Fatalf("other path: %d", w.Code)
	}
}
