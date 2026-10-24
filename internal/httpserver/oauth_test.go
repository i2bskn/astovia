package httpserver

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"github.com/i2bskn/astovia/internal/auth"
	"github.com/i2bskn/astovia/internal/config"
	"github.com/i2bskn/astovia/internal/oauth"
	"github.com/i2bskn/astovia/internal/store"
	"github.com/i2bskn/astovia/internal/testutil"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

const (
	testOrigin   = "https://astovia.test"
	claudeClient = "https://claude.ai/oauth/claude-code-client-metadata"
	claudeWeb    = "https://claude.ai/api/mcp/auth_callback"
)

var testSecret = []byte(strings.Repeat("s", 32))

// oauthAPI is a server with alice and bob logged in, a fixed clock and a
// fake Client ID Metadata Document fetcher serving docs.
type oauthAPI struct {
	t       *testing.T
	store   *store.Store
	srv     *Server
	handler http.Handler
	now     *time.Time
	alice   string
	bob     string
	docs    map[string]oauth.Document
	fetches int
}

func newOAuthAPI(t *testing.T, driver string) *oauthAPI {
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
	now := time.Date(2026, 10, 23, 1, 2, 3, 0, time.UTC)
	api := &oauthAPI{t: t, store: s, now: &now, docs: map[string]oauth.Document{
		claudeClient: {Name: "Claude Code", RedirectURIs: []string{"http://localhost/callback", "http://127.0.0.1/callback"}, TTL: time.Hour},
	}}
	api.srv = api.newServer(s, testOrigin)
	// Most tests register many clients; TestOAuthRegister checks the limit.
	api.srv.registerLimiter.limit = 1000
	api.handler = api.srv.handler()
	api.alice = api.login("alice")
	api.bob = api.login("bob")
	return api
}

// newServer returns a server for origin that shares the store, clock and
// fetcher of a.
func (a *oauthAPI) newServer(s *store.Store, origin string) *Server {
	a.t.Helper()
	svc, err := auth.New(s, testSecret, origin)
	if err != nil {
		a.t.Fatal(err)
	}
	svc.Now = func() time.Time { return *a.now }
	srv := newServer(config.Config{PublicURL: origin, Secure: true, Secret: testSecret}, svc, fstest.MapFS{})
	srv.registerLimiter.now = func() time.Time { return *a.now }
	srv.cimdLimiter.now = func() time.Time { return *a.now }
	srv.fetchCIMD = func(_ context.Context, clientID string) (oauth.Document, error) {
		a.fetches++
		doc, ok := a.docs[clientID]
		if !ok {
			return oauth.Document{}, errors.New("fetch failed")
		}
		return doc, nil
	}
	return srv
}

func (a *oauthAPI) advance(d time.Duration) { *a.now = a.now.Add(d) }

func (a *oauthAPI) login(name string) string {
	a.t.Helper()
	r := httptest.NewRequest("POST", "/auth/login", strings.NewReader(`{"email":"`+name+`@example.com","password":"correct-password-123"}`))
	r.Header.Set("Origin", testOrigin)
	r.Header.Set("Content-Type", "application/json")
	w := serve(a.t, a.handler, r)
	var result authResponse
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil {
		a.t.Fatalf("login %s: %d %s", name, w.Code, w.Body.String())
	}
	return result.AccessToken
}

// do sends a request with a JSON content type when body is not empty.
func (a *oauthAPI) do(method, path, bearer, body string) *httptest.ResponseRecorder {
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

func (a *oauthAPI) register(body string) *httptest.ResponseRecorder {
	a.t.Helper()
	r := httptest.NewRequest("POST", "/oauth/register", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	return serve(a.t, a.handler, r)
}

// registerClient registers a DCR client and returns its ID and secret.
func (a *oauthAPI) registerClient(method string, uris ...string) (string, string) {
	a.t.Helper()
	body := map[string]any{"client_name": "Tool", "redirect_uris": uris}
	if method != "" {
		body["token_endpoint_auth_method"] = method
	}
	w := a.register(string(mustJSON(a.t, body)))
	var out registrationResponse
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
		a.t.Fatalf("register: %d %s", w.Code, w.Body.String())
	}
	return out.ClientID, out.ClientSecret
}

func (a *oauthAPI) authorize(q url.Values) *httptest.ResponseRecorder {
	a.t.Helper()
	return serve(a.t, a.handler, httptest.NewRequest("GET", "/oauth/authorize?"+q.Encode(), nil))
}

// pkce returns a new verifier and its S256 challenge.
func pkce(t *testing.T) (string, string) {
	t.Helper()
	v, err := auth.Random()
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(v))
	return v, base64.RawURLEncoding.EncodeToString(sum[:])
}

func authorizeQuery(clientID, redirectURI, challenge, scope string) url.Values {
	return url.Values{"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {redirectURI}, "code_challenge": {challenge}, "code_challenge_method": {"S256"}, "scope": {scope}, "state": {"st"}}
}

// consentRequest runs /oauth/authorize, which must send the browser to the
// consent page, and returns the signed request.
func (a *oauthAPI) consentRequest(q url.Values) string {
	a.t.Helper()
	w := a.authorize(q)
	loc, err := url.Parse(w.Header().Get("Location"))
	if w.Code != 302 || err != nil || loc.Path != "/admin/oauth/authorize" || loc.Host != "" || loc.Query().Get("request") == "" {
		a.t.Fatalf("authorize: %d %v %s", w.Code, w.Header(), w.Body.String())
	}
	return loc.Query().Get("request")
}

// decide posts the consent decision and returns the redirect URL.
func (a *oauthAPI) decide(jwt, request string, approve bool) *url.URL {
	a.t.Helper()
	w := a.do("POST", "/api/v1/oauth/consent", jwt, string(mustJSON(a.t, map[string]any{"request": request, "approve": approve})))
	var out consentResultResponse
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
		a.t.Fatalf("consent: %d %s", w.Code, w.Body.String())
	}
	u, err := url.Parse(out.RedirectURL)
	if err != nil {
		a.t.Fatal(err)
	}
	return u
}

// connect runs authorize and approval for user jwt and returns the code and
// verifier.
func (a *oauthAPI) connect(jwt, clientID, redirectURI, scope string) (string, string) {
	a.t.Helper()
	verifier, challenge := pkce(a.t)
	loc := a.decide(jwt, a.consentRequest(authorizeQuery(clientID, redirectURI, challenge, scope)), true)
	q := loc.Query()
	if q.Get("state") != "st" || q.Get("iss") != testOrigin || !auth.IsOAuthCode(q.Get("code")) {
		a.t.Fatalf("approval redirect: %s", loc)
	}
	return q.Get("code"), verifier
}

// token posts form to /oauth/token, with HTTP Basic credentials when basic
// is not empty.
func (a *oauthAPI) token(form url.Values, basic string) *httptest.ResponseRecorder {
	a.t.Helper()
	r := httptest.NewRequest("POST", "/oauth/token", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if basic != "" {
		r.Header.Set("Authorization", "Basic "+basic)
	}
	return serve(a.t, a.handler, r)
}

func (a *oauthAPI) exchange(clientID, code, redirectURI, verifier string) *httptest.ResponseRecorder {
	a.t.Helper()
	return a.token(url.Values{"grant_type": {"authorization_code"}, "client_id": {clientID}, "code": {code}, "redirect_uri": {redirectURI}, "code_verifier": {verifier}}, "")
}

func (a *oauthAPI) refresh(clientID, refreshToken string) *httptest.ResponseRecorder {
	a.t.Helper()
	return a.token(url.Values{"grant_type": {"refresh_token"}, "client_id": {clientID}, "refresh_token": {refreshToken}}, "")
}

func tokensOf(t *testing.T, w *httptest.ResponseRecorder) tokenResponse {
	t.Helper()
	var out tokenResponse
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil || !auth.IsOAuthAccessToken(out.AccessToken) || out.TokenType != "Bearer" {
		t.Fatalf("token response: %d %s", w.Code, w.Body.String())
	}
	return out
}

// expectOAuthError checks an RFC 6749 error response that does not redirect.
func expectOAuthError(t *testing.T, w *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	var body oauthErrorResponse
	if w.Code != status || json.Unmarshal(w.Body.Bytes(), &body) != nil || body.Error != code || body.ErrorDescription == "" || w.Header().Get("Location") != "" || w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("got %d %s %v, want %d %q", w.Code, w.Body.String(), w.Header(), status, code)
	}
}

// expectRedirectError checks an authorization error sent to redirectURI.
func expectRedirectError(t *testing.T, w *httptest.ResponseRecorder, redirectURI, code, state string) {
	t.Helper()
	loc, err := url.Parse(w.Header().Get("Location"))
	if w.Code != 302 || err != nil {
		t.Fatalf("got %d %v", w.Code, w.Header())
	}
	q := loc.Query()
	loc.RawQuery = ""
	if loc.String() != redirectURI || q.Get("error") != code || q.Get("error_description") == "" || q.Get("iss") != testOrigin || q.Get("state") != state || q.Has("code") {
		t.Fatalf("redirect %s, want %s error=%s state=%q", w.Header().Get("Location"), redirectURI, code, state)
	}
}

func expectMCPUnauthorized(t *testing.T, h http.Handler, token string) {
	t.Helper()
	if w := mcpPost(t, h, token, "", ""); w.Code != 401 || w.Header().Get("WWW-Authenticate") != mcpInvalidToken {
		t.Fatalf("/mcp: %d %v", w.Code, w.Header())
	}
}

func expectMCPOK(t *testing.T, h http.Handler, token string) {
	t.Helper()
	if w := mcpPost(t, h, token, "", ""); w.Code != 200 || !strings.Contains(w.Body.String(), "action_items_create") {
		t.Fatalf("/mcp: %d %s", w.Code, w.Body.String())
	}
}

func TestOAuthMetadata(t *testing.T) {
	h := New(config.Config{PublicURL: testOrigin}, nil, fstest.MapFS{})
	prm := `{"resource":"https://astovia.test/mcp","authorization_servers":["https://astovia.test"],"scopes_supported":["mcp"],"bearer_methods_supported":["header"],"resource_name":"Astovia"}`
	asm := `{"issuer":"https://astovia.test","authorization_endpoint":"https://astovia.test/oauth/authorize","token_endpoint":"https://astovia.test/oauth/token","registration_endpoint":"https://astovia.test/oauth/register","scopes_supported":["mcp","offline_access"],"response_types_supported":["code"],"response_modes_supported":["query"],"grant_types_supported":["authorization_code","refresh_token"],"code_challenge_methods_supported":["S256"],"token_endpoint_auth_methods_supported":["none","client_secret_basic","client_secret_post"],"client_id_metadata_document_supported":true,"authorization_response_iss_parameter_supported":true}`
	// No store is configured: metadata never touches the database.
	for path, want := range map[string]string{"/.well-known/oauth-protected-resource/mcp": prm, "/.well-known/oauth-protected-resource": prm, "/.well-known/oauth-authorization-server": asm} {
		w := serve(t, h, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 || strings.TrimSpace(w.Body.String()) != want || w.Header().Get("Content-Type") != "application/json" {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
		w = serve(t, h, httptest.NewRequest("POST", path, nil))
		expectStatus(t, w, 405, "method_not_allowed")
	}
	// OpenID Connect discovery is not provided.
	expectStatus(t, serve(t, h, httptest.NewRequest("GET", "/.well-known/openid-configuration", nil)), 404, "not_found")
	for path, method := range map[string]string{"/oauth/authorize": "POST", "/oauth/token": "GET", "/oauth/register": "GET"} {
		expectStatus(t, serve(t, h, httptest.NewRequest(method, path, nil)), 405, "method_not_allowed")
	}
}

func TestOAuthRegister(t *testing.T) {
	api := newOAuthAPI(t, "sqlite")
	// Confidential client by default: a secret that never expires.
	w := api.register(`{"redirect_uris":["https://tool.example/cb"],"client_name":"  My\u0000 Tool  ","logo_uri":"https://tool.example/logo.png","grant_types":["implicit"],"scope":"admin"}`)
	body, keys := jsonKeys(t, w.Body.Bytes())
	if w.Code != 201 || strings.Join(keys, ",") != "client_id,client_id_issued_at,client_name,client_secret,client_secret_expires_at,grant_types,redirect_uris,response_types,token_endpoint_auth_method" {
		t.Fatalf("register: %d %s", w.Code, w.Body.String())
	}
	var out registrationResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !auth.IsDCRClientID(out.ClientID) || !strings.HasPrefix(out.ClientSecret, "dcs_") || len(out.ClientSecret) != 47 || string(body["client_secret_expires_at"]) != "0" || out.ClientName != "My Tool" || out.TokenEndpointAuthMethod != "client_secret_basic" || out.ClientIDIssuedAt != api.now.Unix() {
		t.Fatalf("registration: %+v", out)
	}
	c, err := api.store.OAuthClient(context.Background(), out.ClientID)
	if err != nil || c.SecretHash.String != auth.Hash(out.ClientSecret) || c.Kind != "dcr" || strings.Contains(w.Body.String(), c.SecretHash.String) {
		t.Fatalf("stored client: %+v %v", c, err)
	}
	// Public client: no secret; the name falls back to the redirect host.
	w = api.register(`{"redirect_uris":["http://127.0.0.1:8000/cb"],"token_endpoint_auth_method":"none","client_name":" ​ "}`)
	if _, keys = jsonKeys(t, w.Body.Bytes()); w.Code != 201 || strings.Contains(strings.Join(keys, ","), "secret") || !strings.Contains(w.Body.String(), `"client_name":"127.0.0.1"`) {
		t.Fatalf("public client: %d %s", w.Code, w.Body.String())
	}
	nine := make([]string, 9)
	for i := range nine {
		nine[i] = "https://tool.example/cb"
	}
	for name, tc := range map[string]struct {
		ct, body string
		status   int
		code     string
	}{
		"http non-loopback": {"application/json", `{"redirect_uris":["http://tool.example/cb"]}`, 400, "invalid_redirect_uri"},
		"custom scheme":     {"application/json", `{"redirect_uris":["myapp://cb"]}`, 400, "invalid_redirect_uri"},
		"fragment":          {"application/json", `{"redirect_uris":["https://tool.example/cb#x"]}`, 400, "invalid_redirect_uri"},
		"nine":              {"application/json", string(mustJSON(t, map[string]any{"redirect_uris": nine})), 400, "invalid_redirect_uri"},
		"missing":           {"application/json", `{"client_name":"x"}`, 400, "invalid_redirect_uri"},
		"not strings":       {"application/json", `{"redirect_uris":[1]}`, 400, "invalid_redirect_uri"},
		"auth method":       {"application/json", `{"redirect_uris":["https://tool.example/cb"],"token_endpoint_auth_method":"private_key_jwt"}`, 400, "invalid_client_metadata"},
		"name type":         {"application/json", `{"redirect_uris":["https://tool.example/cb"],"client_name":1}`, 400, "invalid_client_metadata"},
		"array":             {"application/json", `[]`, 400, "invalid_client_metadata"},
		"two objects":       {"application/json", `{}{}`, 400, "invalid_client_metadata"},
		"content type":      {"application/x-www-form-urlencoded", `{"redirect_uris":["https://tool.example/cb"]}`, 415, "invalid_client_metadata"},
		"body over 64 KiB":  {"application/json", `{"redirect_uris":["https://tool.example/cb"],"x":"` + strings.Repeat("x", 64<<10) + `"}`, 413, "invalid_client_metadata"},
	} {
		r := httptest.NewRequest("POST", "/oauth/register", strings.NewReader(tc.body))
		r.Header.Set("Content-Type", tc.ct)
		w = serve(t, api.handler, r)
		t.Run(name, func(t *testing.T) { expectOAuthError(t, w, tc.status, tc.code) })
	}

	// Ten registrations a minute per peer, counting rejected requests.
	api.srv.registerLimiter = newLoginLimiter()
	api.srv.registerLimiter.now = func() time.Time { return *api.now }
	h := api.srv.handler()
	for i := 0; i < 11; i++ {
		r := httptest.NewRequest("POST", "/oauth/register", strings.NewReader(`{"redirect_uris":["https://tool.example/cb"]}`))
		r.Header.Set("Content-Type", "application/json")
		if i%2 == 1 {
			r.Header.Set("Content-Type", "text/plain")
		}
		w = serve(t, h, r)
	}
	expectOAuthError(t, w, 429, "invalid_request")
	if w.Header().Get("Retry-After") != "60" {
		t.Fatalf("Retry-After=%q", w.Header().Get("Retry-After"))
	}
	api.advance(time.Minute)
	r := httptest.NewRequest("POST", "/oauth/register", strings.NewReader(`{"redirect_uris":["https://tool.example/cb"]}`))
	r.Header.Set("Content-Type", "application/json")
	if w = serve(t, h, r); w.Code != 201 {
		t.Fatalf("after a minute: %d", w.Code)
	}
}

func TestOAuthAuthorize(t *testing.T) {
	api := newOAuthAPI(t, "sqlite")
	dcr, _ := api.registerClient("none", "https://tool.example/cb?keep=1", "http://localhost/callback", "https://localhost/cb")
	_, challenge := pkce(t)
	ok := authorizeQuery(dcr, "https://tool.example/cb?keep=1", challenge, "mcp")

	// Requests answered without redirecting.
	with := func(k, v string) url.Values {
		q := url.Values{}
		for key, vs := range ok {
			q[key] = append([]string(nil), vs...)
		}
		if v == "" {
			q.Del(k)
		} else {
			q.Set(k, v)
		}
		return q
	}
	dup := with("scope", "mcp")
	dup.Add("scope", "mcp")
	for name, tc := range map[string]struct {
		q    url.Values
		code string
	}{
		"repeated parameter":        {dup, "invalid_request"},
		"no client_id":              {with("client_id", ""), "invalid_request"},
		"long client_id":            {with("client_id", "https://tool.example/"+strings.Repeat("a", 500)), "invalid_request"},
		"no redirect_uri":           {with("redirect_uri", ""), "invalid_request"},
		"unknown DCR client":        {with("client_id", "dcr_"+strings.Repeat("A", 43)), "invalid_client"},
		"malformed client_id":       {with("client_id", "tool"), "invalid_client"},
		"CIMD fetch failure":        {with("client_id", "https://unknown.example/client"), "invalid_client"},
		"invalid CIMD URL":          {with("client_id", "https://unknown.example/"), "invalid_client"},
		"unregistered redirect_uri": {with("redirect_uri", "https://evil.example/cb"), "invalid_request"},
		"query differs":             {with("redirect_uri", "https://tool.example/cb"), "invalid_request"},
		"https port ignored":        {with("redirect_uri", "https://localhost:8443/cb"), "invalid_request"},
		"loopback host differs":     {with("redirect_uri", "http://127.0.0.1:5000/callback"), "invalid_request"},
	} {
		t.Run(name, func(t *testing.T) { expectOAuthError(t, api.authorize(tc.q), 400, tc.code) })
	}

	// Errors sent back to the redirect URI with state and iss.
	for name, tc := range map[string]struct {
		q    url.Values
		code string
	}{
		"no PKCE":             {with("code_challenge", ""), "invalid_request"},
		"no method":           {with("code_challenge_method", ""), "invalid_request"},
		"plain":               {with("code_challenge_method", "plain"), "invalid_request"},
		"short challenge":     {with("code_challenge", challenge[:42]), "invalid_request"},
		"response type":       {with("response_type", "token"), "unsupported_response_type"},
		"unknown scope":       {with("scope", "mcp admin"), "invalid_scope"},
		"offline_access only": {with("scope", "offline_access"), "invalid_scope"},
		"long scope":          {with("scope", "mcp"+strings.Repeat(" ", 300)), "invalid_scope"},
		"resource":            {with("resource", "https://other.test/mcp"), "invalid_target"},
		"resource path":       {with("resource", testOrigin+"/api"), "invalid_target"},
	} {
		t.Run(name, func(t *testing.T) {
			w := api.authorize(tc.q)
			expectRedirectError(t, w, "https://tool.example/cb", tc.code, "st")
			if loc, _ := url.Parse(w.Header().Get("Location")); loc.Query().Get("keep") != "1" {
				t.Fatalf("existing query lost: %s", loc)
			}
		})
	}
	w := api.authorize(with("state", strings.Repeat("s", 513)))
	expectRedirectError(t, w, "https://tool.example/cb", "invalid_request", "")

	// Accepted: loopback with another port, the resource in any accepted
	// form, an empty resource, and no scope (mcp).
	for _, q := range []url.Values{ok, with("redirect_uri", "http://localhost:53682/callback"), with("resource", testOrigin+"/mcp/"), with("resource", testOrigin), with("scope", "")} {
		api.consentRequest(q)
	}
	empty := with("resource", "")
	empty.Set("resource", "")
	api.consentRequest(empty)

	// A database failure is 503 without a redirect.
	if err := api.store.Close(); err != nil {
		t.Fatal(err)
	}
	expectOAuthError(t, api.authorize(ok), 503, "temporarily_unavailable")
}

func TestOAuthConsent(t *testing.T) {
	api := newOAuthAPI(t, "sqlite")
	_, challenge := pkce(t)
	request := api.consentRequest(authorizeQuery(claudeClient, "http://localhost:53682/callback", challenge, "offline_access mcp"))

	// The consent page data.
	w := api.do("GET", "/api/v1/oauth/consent?request="+url.QueryEscape(request), api.alice, "")
	want := `{"client_name":"Claude Code","client_kind":"cimd","client_host":"claude.ai","redirect_host":"localhost","redirect_loopback":true,"scopes":["mcp","offline_access"],"expires_at":"2026-10-23T01:12:03Z"}`
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != want {
		t.Fatalf("consent: %d %s", w.Code, w.Body.String())
	}
	dcr, _ := api.registerClient("none", claudeWeb)
	dcrRequest := api.consentRequest(authorizeQuery(dcr, claudeWeb, challenge, ""))
	w = api.do("GET", "/api/v1/oauth/consent?request="+url.QueryEscape(dcrRequest), api.alice, "")
	if want = `{"client_name":"Tool","client_kind":"dcr","client_host":null,"redirect_host":"claude.ai","redirect_loopback":false,"scopes":["mcp"],"expires_at":"2026-10-23T01:12:03Z"}`; w.Code != 200 || strings.TrimSpace(w.Body.String()) != want {
		t.Fatalf("DCR consent: %d %s", w.Code, w.Body.String())
	}

	// Only a browser session may see or answer it.
	apiToken := (&tokenAPI{t: t, handler: api.handler}).issue(api.alice, "t").Token
	code, verifier := api.connect(api.alice, claudeClient, "http://localhost:1/callback", "mcp")
	oauthToken := tokensOf(t, api.exchange(claudeClient, code, "http://localhost:1/callback", verifier)).AccessToken
	for name, bearer := range map[string]string{"none": "", "API token": apiToken, "OAuth token": oauthToken} {
		expectStatus(t, api.do("GET", "/api/v1/oauth/consent?request="+url.QueryEscape(request), bearer, ""), 401, "unauthorized")
		expectStatus(t, api.do("POST", "/api/v1/oauth/consent", bearer, `{"request":"`+request+`","approve":true}`), 401, "unauthorized")
		if w = api.do("GET", "/api/v1/oauth/connections", bearer, ""); w.Code != 401 {
			t.Fatalf("%s connections: %d", name, w.Code)
		}
	}

	// Tampered, malformed and expired requests.
	body, sig, _ := strings.Cut(request, ".")
	raw, _ := base64.RawURLEncoding.DecodeString(body)
	forged := base64.RawURLEncoding.EncodeToString([]byte(strings.Replace(string(raw), "localhost:53682", "evil.example", 1))) + "." + sig
	for _, v := range []string{"", "x", forged, request + "x"} {
		expectStatus(t, api.do("GET", "/api/v1/oauth/consent?request="+url.QueryEscape(v), api.alice, ""), 400, "invalid_request")
		expectStatus(t, api.do("POST", "/api/v1/oauth/consent", api.alice, string(mustJSON(t, map[string]any{"request": v, "approve": true}))), 400, "invalid_request")
	}
	expectStatus(t, api.do("POST", "/api/v1/oauth/consent", api.alice, `{"request":"`+request+`"}`), 400, "invalid_request")
	expectStatus(t, api.do("POST", "/api/v1/oauth/consent", api.alice, `{"request":"`+request+`","approve":true,"user_id":"bob-id"}`), 400, "invalid_request")
	expectStatus(t, api.do("POST", "/api/v1/oauth/consent", api.alice, `{"request":"`+strings.Repeat("x", 16<<10)+`","approve":true}`), 413, "body_too_large")
	r := httptest.NewRequest("POST", "/api/v1/oauth/consent", strings.NewReader(`{"request":"`+request+`","approve":true}`))
	r.Header.Set("Authorization", "Bearer "+api.alice)
	expectStatus(t, serve(t, api.handler, r), 415, "unsupported_media_type")

	// Denying writes nothing and returns access_denied.
	loc := api.decide(api.alice, request, false)
	if q := loc.Query(); loc.Host != "localhost:53682" || q.Get("error") != "access_denied" || q.Get("state") != "st" || q.Get("iss") != testOrigin || q.Has("code") {
		t.Fatalf("deny: %s", loc)
	}
	// The same request can be approved, more than once.
	for i := 0; i < 2; i++ {
		if loc = api.decide(api.alice, request, true); !auth.IsOAuthCode(loc.Query().Get("code")) {
			t.Fatalf("approve: %s", loc)
		}
	}

	api.advance(oauth.RequestLifetime)
	expectStatus(t, api.do("GET", "/api/v1/oauth/consent?request="+url.QueryEscape(request), api.login("alice"), ""), 400, "invalid_request")

	// After a password reset the old session cannot approve, and no grant is
	// left behind.
	jwt := api.login("alice")
	fresh := api.consentRequest(authorizeQuery(claudeClient, "http://localhost:53682/callback", challenge, "mcp"))
	if err := api.store.ChangeUser(context.Background(), "alice@example.com", "new-hash", false, api.now.Unix()); err != nil {
		t.Fatal(err)
	}
	expectStatus(t, api.do("POST", "/api/v1/oauth/consent", jwt, `{"request":"`+fresh+`","approve":true}`), 401, "unauthorized")
	if list, err := api.store.ListOAuthGrants(context.Background(), "alice-id", 0); err != nil || len(list) != 0 {
		t.Fatalf("grants after reset: %+v %v", list, err)
	}
}

func TestOAuthToken(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) { testOAuthToken(t, driver) })
	}
}

func testOAuthToken(t *testing.T, driver string) {
	api := newOAuthAPI(t, driver)
	const loopback = "http://localhost:53682/callback"
	other, _ := api.registerClient("none", loopback)

	// Wrong verifier, redirect URI (including another loopback port), client
	// or resource, and expired codes are invalid_grant and issue nothing;
	// the code stays usable for the right request.
	code, verifier := api.connect(api.alice, claudeClient, loopback, "mcp offline_access")
	wrongVerifier, _ := pkce(t)
	for name, w := range map[string]*httptest.ResponseRecorder{
		"verifier":        api.exchange(claudeClient, code, loopback, wrongVerifier),
		"redirect_uri":    api.exchange(claudeClient, code, "http://localhost/callback", verifier),
		"loopback port":   api.exchange(claudeClient, code, "http://localhost:53683/callback", verifier),
		"client":          api.exchange(other, code, loopback, verifier),
		"unknown code":    api.exchange(claudeClient, "asc_"+strings.Repeat("A", 43), loopback, verifier),
		"malformed code":  api.exchange(claudeClient, "code", loopback, verifier),
		"refresh as code": api.exchange(claudeClient, "asr_"+strings.Repeat("A", 43), loopback, verifier),
	} {
		t.Run(name, func(t *testing.T) { expectOAuthError(t, w, 400, "invalid_grant") })
	}
	w := api.token(url.Values{"grant_type": {"authorization_code"}, "client_id": {claudeClient}, "code": {code}, "redirect_uri": {loopback}, "code_verifier": {verifier}, "resource": {"https://other.test/mcp"}}, "")
	expectOAuthError(t, w, 400, "invalid_target")
	w = api.token(url.Values{"grant_type": {"authorization_code"}, "client_id": {claudeClient}, "code": {code}, "redirect_uri": {loopback}, "code_verifier": {verifier}, "resource": {testOrigin + "/mcp"}}, "")
	first := tokensOf(t, w)
	if first.ExpiresIn != 3600 || first.Scope != "mcp offline_access" || !auth.IsOAuthRefreshToken(first.RefreshToken) || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("first exchange: %+v", first)
	}
	expectMCPOK(t, api.handler, first.AccessToken)

	// Reusing the code with a wrong verifier changes nothing.
	expectOAuthError(t, api.exchange(claudeClient, code, loopback, wrongVerifier), 400, "invalid_grant")
	expectMCPOK(t, api.handler, first.AccessToken)
	// Reusing it with the right verifier revokes everything issued from it.
	expectOAuthError(t, api.exchange(claudeClient, code, loopback, verifier), 400, "invalid_grant")
	expectMCPUnauthorized(t, api.handler, first.AccessToken)
	expectOAuthError(t, api.refresh(claudeClient, first.RefreshToken), 400, "invalid_grant")

	// An expired code.
	code, verifier = api.connect(api.alice, claudeClient, loopback, "mcp")
	api.advance(oauth.CodeLifetime)
	expectOAuthError(t, api.exchange(claudeClient, code, loopback, verifier), 400, "invalid_grant")

	// Refresh rotation: the old refresh token is rejected and the new tokens
	// keep working.
	api.alice = api.login("alice")
	code, verifier = api.connect(api.alice, claudeClient, loopback, "mcp offline_access")
	issued := tokensOf(t, api.exchange(claudeClient, code, loopback, verifier))
	api.advance(time.Minute)
	for name, form := range map[string]url.Values{
		"scope not granted": {"grant_type": {"refresh_token"}, "client_id": {claudeClient}, "refresh_token": {issued.RefreshToken}, "scope": {"mcp admin"}},
		"offline only":      {"grant_type": {"refresh_token"}, "client_id": {claudeClient}, "refresh_token": {issued.RefreshToken}, "scope": {"offline_access"}},
	} {
		t.Run(name, func(t *testing.T) { expectOAuthError(t, api.token(form, ""), 400, "invalid_scope") })
	}
	expectOAuthError(t, api.token(url.Values{"grant_type": {"refresh_token"}, "client_id": {claudeClient}, "refresh_token": {issued.RefreshToken}, "resource": {"https://other.test"}}, ""), 400, "invalid_target")
	expectOAuthError(t, api.refresh(other, issued.RefreshToken), 400, "invalid_grant")
	expectOAuthError(t, api.refresh(claudeClient, issued.AccessToken), 400, "invalid_grant")
	rotated := tokensOf(t, api.token(url.Values{"grant_type": {"refresh_token"}, "client_id": {claudeClient}, "refresh_token": {issued.RefreshToken}, "scope": {"mcp"}, "resource": {testOrigin + "/mcp"}}, ""))
	if rotated.Scope != "mcp offline_access" || rotated.RefreshToken == "" || rotated.RefreshToken == issued.RefreshToken {
		t.Fatalf("rotated: %+v", rotated)
	}
	expectOAuthError(t, api.refresh(claudeClient, issued.RefreshToken), 400, "invalid_grant")
	expectMCPOK(t, api.handler, rotated.AccessToken)
	expectMCPOK(t, api.handler, issued.AccessToken)
	if again := tokensOf(t, api.refresh(claudeClient, rotated.RefreshToken)); again.RefreshToken == "" {
		t.Fatal("new refresh token rejected")
	}
	// A refresh token is not an access token.
	expectMCPUnauthorized(t, api.handler, rotated.RefreshToken)
	// Access tokens expire after an hour.
	api.advance(time.Hour)
	expectMCPUnauthorized(t, api.handler, issued.AccessToken)

	// Without offline_access: a seven-day access token and no refresh token.
	api.alice = api.login("alice")
	code, verifier = api.connect(api.alice, claudeClient, loopback, "mcp")
	long := tokensOf(t, api.exchange(claudeClient, code, loopback, verifier))
	if _, keys := jsonKeys(t, mustJSON(t, long)); long.ExpiresIn != 7*86400 || long.RefreshToken != "" || long.Scope != "mcp" || strings.Contains(strings.Join(keys, ","), "refresh_token") {
		t.Fatalf("no offline_access: %+v", long)
	}
	api.advance(7*24*time.Hour - time.Second)
	expectMCPOK(t, api.handler, long.AccessToken)
	api.advance(time.Second)
	expectMCPUnauthorized(t, api.handler, long.AccessToken)
}

func TestOAuthTokenRequests(t *testing.T) {
	api := newOAuthAPI(t, "sqlite")
	const cb = "https://tool.example/cb"
	basicID, basicSecret := api.registerClient("", cb)
	postID, postSecret := api.registerClient("client_secret_post", cb)
	basic := func(id, secret string) string {
		return base64.StdEncoding.EncodeToString([]byte(url.QueryEscape(id) + ":" + url.QueryEscape(secret)))
	}
	grant := func(clientID string) (url.Values, string) {
		code, verifier := api.connect(api.alice, clientID, cb, "mcp")
		return url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {cb}, "code_verifier": {verifier}}, code
	}

	// Client authentication failures: Basic is 401 with a challenge, the
	// form is 400.
	form, _ := grant(basicID)
	for name, tc := range map[string]struct {
		basic  string
		form   url.Values
		status int
	}{
		"wrong Basic secret":  {basic(basicID, "dcs_wrong"), nil, 401},
		"malformed Basic":     {"***", nil, 401},
		"Basic without colon": {base64.StdEncoding.EncodeToString([]byte(basicID)), nil, 401},
		"Basic id mismatch":   {basic(basicID, basicSecret), url.Values{"client_id": {postID}}, 401},
		"wrong form secret":   {"", url.Values{"client_id": {basicID}, "client_secret": {"dcs_wrong"}}, 400},
		"missing secret":      {"", url.Values{"client_id": {basicID}}, 400},
		"unknown client":      {"", url.Values{"client_id": {"dcr_unknown"}}, 400},
		"no client":           {"", url.Values{}, 400},
	} {
		t.Run(name, func(t *testing.T) {
			f := url.Values{}
			for k, v := range form {
				f[k] = v
			}
			for k, v := range tc.form {
				f[k] = v
			}
			w := api.token(f, tc.basic)
			expectOAuthError(t, w, tc.status, "invalid_client")
			if got := w.Header().Get("WWW-Authenticate"); (tc.status == 401) != (got == `Basic realm="astovia"`) {
				t.Fatalf("WWW-Authenticate=%q", got)
			}
		})
	}
	r := httptest.NewRequest("POST", "/oauth/token", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Authorization", "Bearer "+api.alice)
	w := serve(t, api.handler, r)
	expectOAuthError(t, w, 401, "invalid_client")

	// Either location works for either registered method.
	tokensOf(t, api.token(form, basic(basicID, basicSecret)))
	form, _ = grant(postID)
	form.Set("client_id", postID)
	form.Set("client_secret", postSecret)
	tokensOf(t, api.token(form, ""))
	form, _ = grant(postID)
	tokensOf(t, api.token(form, basic(postID, postSecret)))

	// Request format.
	form, _ = grant(basicID)
	form.Set("client_id", basicID)
	form.Set("client_secret", basicSecret)
	for name, tc := range map[string]struct {
		ct, body string
		status   int
		code     string
	}{
		"JSON":             {"application/json", `{"grant_type":"authorization_code"}`, 415, "invalid_request"},
		"repeated":         {"application/x-www-form-urlencoded", form.Encode() + "&code=x", 400, "invalid_request"},
		"no grant_type":    {"application/x-www-form-urlencoded", "client_id=" + url.QueryEscape(basicID) + "&client_secret=" + basicSecret, 400, "invalid_request"},
		"password grant":   {"application/x-www-form-urlencoded", "grant_type=password&client_id=" + url.QueryEscape(basicID) + "&client_secret=" + basicSecret, 400, "unsupported_grant_type"},
		"missing verifier": {"application/x-www-form-urlencoded", strings.Replace(form.Encode(), "code_verifier=", "x=", 1), 400, "invalid_request"},
		"no refresh_token": {"application/x-www-form-urlencoded", "grant_type=refresh_token&client_id=" + url.QueryEscape(basicID) + "&client_secret=" + basicSecret, 400, "invalid_request"},
		"body over 64 KiB": {"application/x-www-form-urlencoded", form.Encode() + "&x=" + strings.Repeat("x", 64<<10), 413, "invalid_request"},
		"bad encoding":     {"application/x-www-form-urlencoded", form.Encode() + "&x=%zz", 400, "invalid_request"},
	} {
		t.Run(name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/oauth/token", strings.NewReader(tc.body))
			r.Header.Set("Content-Type", tc.ct)
			expectOAuthError(t, serve(t, api.handler, r), tc.status, tc.code)
		})
	}
	// Parameters in the query string are ignored.
	r = httptest.NewRequest("POST", "/oauth/token?grant_type=authorization_code", strings.NewReader("client_id="+url.QueryEscape(basicID)+"&client_secret="+basicSecret))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	expectOAuthError(t, serve(t, api.handler, r), 400, "invalid_request")
	// The code was not consumed by the failures above.
	tokensOf(t, api.token(form, ""))

	// A public client may send a secret; it is not checked.
	pub, _ := api.registerClient("none", cb)
	form, _ = grant(pub)
	form.Set("client_id", pub)
	form.Set("client_secret", "anything")
	tokensOf(t, api.token(form, ""))

	// A database failure is 503.
	form, _ = grant(pub)
	form.Set("client_id", pub)
	if err := api.store.Close(); err != nil {
		t.Fatal(err)
	}
	expectOAuthError(t, api.token(form, ""), 503, "temporarily_unavailable")
}

func TestOAuthAccessScope(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) { testOAuthAccessScope(t, driver) })
	}
}

func testOAuthAccessScope(t *testing.T, driver string) {
	api := newOAuthAPI(t, driver)
	const loopback = "http://localhost:53682/callback"
	issue := func(jwt string) tokenResponse {
		t.Helper()
		code, verifier := api.connect(jwt, claudeClient, loopback, "mcp offline_access")
		return tokensOf(t, api.exchange(claudeClient, code, loopback, verifier))
	}
	alice := issue(api.alice)
	bob := issue(api.bob)

	// OAuth tokens are rejected everywhere under /api/v1.
	for _, token := range []string{alice.AccessToken, alice.RefreshToken} {
		for _, path := range []string{"/api/v1/me", "/api/v1/action-items", "/api/v1/api-tokens", "/api/v1/oauth/connections"} {
			expectStatus(t, api.do("GET", path, token, ""), 401, "unauthorized")
		}
		expectStatus(t, api.do("POST", "/api/v1/action-items", token, `{"content":"x"}`), 401, "unauthorized")
	}

	// /mcp acts on the token owner's data only.
	if w := api.do("POST", "/api/v1/action-items", api.bob, `{"content":"bob's secret"}`); w.Code != 201 {
		t.Fatalf("bob's item: %d", w.Code)
	}
	srv := httptest.NewServer(api.handler)
	t.Cleanup(srv.Close)
	c := connectMCP(t, srv, alice.AccessToken, nil)
	var item actionItemResponse
	c.ok("action_items_create", map[string]any{"content": "alice's"}, &item)
	var page actionItemListResponse
	c.ok("action_items_list", nil, &page)
	if len(page.Items) != 1 || page.Items[0].ID != item.ID {
		t.Fatalf("alice's list: %+v", page)
	}

	// A grant for another public URL is rejected by the resource check, and
	// its refresh token is not refreshed there.
	otherSrv := api.newServer(api.store, "https://other.test")
	other := otherSrv.handler()
	if w := mcpPost(t, other, alice.AccessToken, "", ""); w.Code != 401 || !strings.Contains(w.Header().Get("WWW-Authenticate"), `error="invalid_token", resource_metadata="https://other.test/.well-known/oauth-protected-resource/mcp"`) {
		t.Fatalf("other resource: %d %v", w.Code, w.Header())
	}
	r := httptest.NewRequest("POST", "/oauth/token", strings.NewReader(url.Values{"grant_type": {"refresh_token"}, "client_id": {claudeClient}, "refresh_token": {alice.RefreshToken}}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	expectOAuthError(t, serve(t, other, r), 400, "invalid_grant")
	expectMCPOK(t, api.handler, alice.AccessToken)

	// Disabling bob and resetting alice's password delete their grants and
	// tokens.
	if err := api.store.ChangeUser(context.Background(), "bob@example.com", "", true, api.now.Unix()); err != nil {
		t.Fatal(err)
	}
	expectMCPUnauthorized(t, api.handler, bob.AccessToken)
	expectMCPOK(t, api.handler, alice.AccessToken)
	if err := api.store.ChangeUser(context.Background(), "alice@example.com", "new-hash", false, api.now.Unix()); err != nil {
		t.Fatal(err)
	}
	expectMCPUnauthorized(t, api.handler, alice.AccessToken)
	expectOAuthError(t, api.refresh(claudeClient, alice.RefreshToken), 400, "invalid_grant")
	for _, id := range []string{"alice-id", "bob-id"} {
		if list, err := api.store.ListOAuthGrants(context.Background(), id, 0); err != nil || len(list) != 0 {
			t.Fatalf("%s grants: %+v %v", id, list, err)
		}
	}
}

// TestOAuthFlow follows a DCR client from registration to disconnection.
func TestOAuthFlow(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) { testOAuthFlow(t, driver) })
	}
}

func testOAuthFlow(t *testing.T, driver string) {
	api := newOAuthAPI(t, driver)

	// The 401 challenge leads to the metadata.
	w := mcpPost(t, api.handler, "", "", "")
	if w.Code != 401 || w.Header().Get("WWW-Authenticate") != mcpNoToken {
		t.Fatalf("challenge: %d %v", w.Code, w.Header())
	}

	// Registration with client_secret_post, as ChatGPT may.
	clientID, secret := api.registerClient("client_secret_post", "https://chatgpt.com/connector_platform_oauth_redirect")
	const cb = "https://chatgpt.com/connector_platform_oauth_redirect"
	verifier, challenge := pkce(t)
	q := authorizeQuery(clientID, cb, challenge, "mcp offline_access")
	q.Set("resource", testOrigin+"/mcp")
	request := api.consentRequest(q)
	w = api.do("GET", "/api/v1/oauth/consent?request="+url.QueryEscape(request), api.alice, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"client_name":"Tool"`) {
		t.Fatalf("consent: %d %s", w.Code, w.Body.String())
	}
	loc := api.decide(api.alice, request, true)
	if loc.Scheme+"://"+loc.Host+loc.Path != cb || loc.Query().Get("state") != "st" || loc.Query().Get("iss") != testOrigin {
		t.Fatalf("redirect: %s", loc)
	}
	form := url.Values{"grant_type": {"authorization_code"}, "client_id": {clientID}, "client_secret": {secret}, "code": {loc.Query().Get("code")}, "redirect_uri": {cb}, "code_verifier": {verifier}, "resource": {testOrigin + "/mcp"}}
	issued := tokensOf(t, api.token(form, ""))

	// /mcp with the access token.
	srv := httptest.NewServer(api.handler)
	t.Cleanup(srv.Close)
	c := connectMCP(t, srv, issued.AccessToken, nil)
	tools, err := c.session.ListTools(context.Background(), nil)
	if err != nil || len(tools.Tools) != 4 {
		t.Fatalf("tools/list: %v %v", tools, err)
	}
	var item actionItemResponse
	c.ok("action_items_create", map[string]any{"content": "from OAuth"}, &item)
	if w = api.do("GET", "/api/v1/action-items/"+item.ID, api.alice, ""); w.Code != 200 {
		t.Fatalf("item via REST: %d", w.Code)
	}

	// Refresh and reject the old refresh token.
	api.advance(time.Minute)
	rotated := tokensOf(t, api.token(url.Values{"grant_type": {"refresh_token"}, "client_id": {clientID}, "client_secret": {secret}, "refresh_token": {issued.RefreshToken}}, ""))
	expectOAuthError(t, api.token(url.Values{"grant_type": {"refresh_token"}, "client_id": {clientID}, "client_secret": {secret}, "refresh_token": {issued.RefreshToken}}, ""), 400, "invalid_grant")
	expectMCPOK(t, api.handler, rotated.AccessToken)

	// The connection is listed with its last use.
	w = api.do("GET", "/api/v1/oauth/connections", api.alice, "")
	var list connectionListResponse
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &list) != nil || len(list.Items) != 1 {
		t.Fatalf("connections: %d %s", w.Code, w.Body.String())
	}
	conn := list.Items[0]
	if _, keys := jsonKeys(t, mustJSON(t, conn)); strings.Join(keys, ",") != "client_host,client_name,created_at,expires_at,id,last_used_at,scopes" {
		t.Fatalf("connection keys: %v", keys)
	}
	if conn.ClientName != "Tool" || conn.ClientHost != nil || strings.Join(conn.Scopes, " ") != "mcp offline_access" || conn.CreatedAt != "2026-10-23T01:02:03Z" || conn.LastUsedAt == nil || *conn.LastUsedAt != "2026-10-23T01:02:03Z" || conn.ExpiresAt != "2027-01-21T01:03:03Z" {
		t.Fatalf("connection: %+v", conn)
	}
	if w = api.do("GET", "/api/v1/oauth/connections", api.bob, ""); strings.TrimSpace(w.Body.String()) != `{"items":[]}` {
		t.Fatalf("bob's connections: %s", w.Body.String())
	}

	// Deleting: another user's or unknown IDs are 404; the owner's removes
	// the tokens at once.
	expectStatus(t, api.do("DELETE", "/api/v1/oauth/connections/"+conn.ID, api.bob, ""), 404, "not_found")
	expectStatus(t, api.do("DELETE", "/api/v1/oauth/connections/missing", api.alice, ""), 404, "not_found")
	expectStatus(t, api.do("DELETE", "/api/v1/oauth/connections/bad.id", api.alice, ""), 404, "not_found")
	expectMCPOK(t, api.handler, rotated.AccessToken)
	if w = api.do("DELETE", "/api/v1/oauth/connections/"+conn.ID, api.alice, ""); w.Code != 204 {
		t.Fatalf("delete: %d", w.Code)
	}
	expectMCPUnauthorized(t, api.handler, rotated.AccessToken)
	expectOAuthError(t, api.token(url.Values{"grant_type": {"refresh_token"}, "client_id": {clientID}, "client_secret": {secret}, "refresh_token": {rotated.RefreshToken}}, ""), 400, "invalid_grant")
	if w = api.do("GET", "/api/v1/oauth/connections", api.alice, ""); strings.TrimSpace(w.Body.String()) != `{"items":[]}` {
		t.Fatalf("after delete: %s", w.Body.String())
	}
}

// TestOAuthCIMDFlow connects Claude Code through its metadata document with
// a random loopback port.
func TestOAuthCIMDFlow(t *testing.T) {
	api := newOAuthAPI(t, "sqlite")
	const cb = "http://127.0.0.1:41234/callback"
	code, verifier := api.connect(api.alice, claudeClient, cb, "mcp offline_access")
	if api.fetches != 1 {
		t.Fatalf("fetches: %d", api.fetches)
	}
	issued := tokensOf(t, api.exchange(claudeClient, code, cb, verifier))
	expectMCPOK(t, api.handler, issued.AccessToken)
	w := api.do("GET", "/api/v1/oauth/connections", api.alice, "")
	if !strings.Contains(w.Body.String(), `"client_name":"Claude Code","client_host":"claude.ai"`) {
		t.Fatalf("connections: %s", w.Body.String())
	}

	// The cached document is used within its TTL, and refreshed after it.
	_, challenge := pkce(t)
	api.consentRequest(authorizeQuery(claudeClient, cb, challenge, "mcp"))
	if api.fetches != 1 {
		t.Fatalf("cache not used: %d", api.fetches)
	}
	api.advance(time.Hour)
	api.docs[claudeClient] = oauth.Document{Name: "Claude Code 2", RedirectURIs: []string{"http://localhost/callback"}, TTL: time.Hour}
	expectOAuthError(t, api.authorize(authorizeQuery(claudeClient, cb, challenge, "mcp")), 400, "invalid_request")
	if api.fetches != 2 {
		t.Fatalf("expired cache not refreshed: %d", api.fetches)
	}
	// Token requests use the stored row and never fetch: the refresh works
	// while the document is unavailable.
	delete(api.docs, claudeClient)
	rotated := tokensOf(t, api.refresh(claudeClient, issued.RefreshToken))
	if api.fetches != 2 {
		t.Fatalf("token endpoint fetched: %d", api.fetches)
	}
	expectMCPOK(t, api.handler, rotated.AccessToken)

	// Fetching is limited per peer: after ten fetches a client without a row
	// gets 429, and one with an expired row continues with it.
	api.advance(time.Hour)
	for i := 0; i < 10; i++ {
		expectOAuthError(t, api.authorize(authorizeQuery("https://unknown.example/c"+string(rune('a'+i)), cb, challenge, "mcp")), 400, "invalid_client")
	}
	w = api.authorize(authorizeQuery("https://unknown.example/z", cb, challenge, "mcp"))
	expectOAuthError(t, w, 429, "temporarily_unavailable")
	if w.Header().Get("Retry-After") != "60" {
		t.Fatalf("Retry-After=%q", w.Header().Get("Retry-After"))
	}
	fetches := api.fetches
	api.consentRequest(authorizeQuery(claudeClient, "http://localhost:5/callback", challenge, "mcp"))
	if api.fetches != fetches {
		t.Fatal("rate-limited request fetched")
	}
}
