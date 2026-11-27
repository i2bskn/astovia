package httpserver

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/i2bskn/astovia/internal/auth"
	"github.com/i2bskn/astovia/internal/config"
	"github.com/i2bskn/astovia/internal/store"
)

const (
	oidcClientID = "astovia-client"
	oidcSecret   = "astovia-secret"
)

// fakeIdP is an OpenID provider served over TLS: discovery, the keys and
// the token endpoint, which answers an RS256 ID token with the claims
// granted to the authorization code. The authorization endpoint is never
// called: grant plays the user's consent.
type fakeIdP struct {
	*httptest.Server
	t   *testing.T
	key *rsa.PrivateKey

	mu          sync.Mutex
	grants      map[string]idpGrant
	discoveries int
	down        bool
}

// idpGrant is an authorization code: the nonce and the PKCE challenge of
// the request and the claims of the ID token.
type idpGrant struct {
	nonce, challenge string
	claims           map[string]any
	// forged signs the ID token with a key the provider does not publish.
	forged bool
}

func newFakeIdP(t *testing.T) *fakeIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	idp := &fakeIdP{t: t, key: key, grants: map[string]idpGrant{}}
	idp.Server = httptest.NewTLSServer(http.HandlerFunc(idp.serve))
	t.Cleanup(idp.Close)
	return idp
}

func (idp *fakeIdP) serve(w http.ResponseWriter, r *http.Request) {
	idp.mu.Lock()
	defer idp.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/.well-known/openid-configuration":
		idp.discoveries++
		if idp.down {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": idp.URL, "authorization_endpoint": idp.URL + "/authorize", "token_endpoint": idp.URL + "/token",
			"jwks_uri": idp.URL + "/keys", "response_types_supported": []string{"code"}, "subject_types_supported": []string{"public"},
			"id_token_signing_alg_values_supported": []string{"RS256"}, "code_challenge_methods_supported": []string{"S256"},
		})
	case "/keys":
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "kid": "key", "use": "sig", "alg": "RS256",
			"n": b64(idp.key.N.Bytes()), "e": b64(big.NewInt(int64(idp.key.E)).Bytes()),
		}}})
	case "/token":
		if err := r.ParseForm(); err != nil || r.Method != http.MethodPost {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		user, pass, ok := r.BasicAuth()
		if !ok {
			user, pass = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
		}
		code := r.PostForm.Get("code")
		g, found := idp.grants[code]
		delete(idp.grants, code)
		sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
		if user != oidcClientID || pass != oidcSecret || !found || b64(sum[:]) != g.challenge || r.PostForm.Get("grant_type") != "authorization_code" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "access", "token_type": "Bearer", "expires_in": 3600, "id_token": idp.idToken(g)})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// idToken signs the ID token of g.
func (idp *fakeIdP) idToken(g idpGrant) string {
	now := time.Now()
	claims := map[string]any{"iss": idp.URL, "aud": oidcClientID, "nonce": g.nonce, "iat": now.Unix(), "exp": now.Add(time.Hour).Unix()}
	for k, v := range g.claims {
		claims[k] = v
	}
	header := b64(mustJSON(idp.t, map[string]string{"alg": "RS256", "kid": "key", "typ": "JWT"}))
	signed := header + "." + b64(mustJSON(idp.t, claims))
	digest := sha256.Sum256([]byte(signed))
	key := idp.key
	if g.forged {
		var err error
		if key, err = rsa.GenerateKey(rand.Reader, 2048); err != nil {
			idp.t.Fatal(err)
		}
	}
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		idp.t.Fatal(err)
	}
	return signed + "." + b64(signature)
}

// grant answers the authorization request at location as the user of
// claims and returns the query of the redirect to the callback.
func (idp *fakeIdP) grant(location *url.URL, claims map[string]any) string {
	idp.t.Helper()
	q := location.Query()
	if location.Scheme+"://"+location.Host+location.Path != idp.URL+"/authorize" || q.Get("client_id") != oidcClientID || q.Get("redirect_uri") != usersOrigin+config.OIDCCallbackPath ||
		q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" || q.Get("state") == "" || q.Get("nonce") == "" || !strings.Contains(" "+q.Get("scope")+" ", " openid ") || !strings.Contains(" "+q.Get("scope")+" ", " email ") {
		idp.t.Fatalf("authorization request: %s", location)
	}
	code := b64([]byte(q.Get("state") + "-code"))
	idp.mu.Lock()
	idp.grants[code] = idpGrant{nonce: q.Get("nonce"), challenge: q.Get("code_challenge"), claims: claims}
	idp.mu.Unlock()
	return url.Values{"code": {code}, "state": {q.Get("state")}}.Encode()
}

// forge makes the ID token of the callback query signed with another key.
func (idp *fakeIdP) forge(query string) {
	idp.t.Helper()
	q, err := url.ParseQuery(query)
	if err != nil {
		idp.t.Fatal(err)
	}
	idp.mu.Lock()
	defer idp.mu.Unlock()
	g := idp.grants[q.Get("code")]
	g.forged = true
	idp.grants[q.Get("code")] = g
}

func (idp *fakeIdP) setDown(down bool) {
	idp.mu.Lock()
	defer idp.mu.Unlock()
	idp.down = down
}

func (idp *fakeIdP) discoveryCount() int {
	idp.mu.Lock()
	defer idp.mu.Unlock()
	return idp.discoveries
}

// claims are the claims of a verified address with the subject sub.
func claims(email, sub string) map[string]any {
	return map[string]any{"sub": sub, "email": email, "email_verified": true}
}

// oidcAPI is usersAPI (alice the owner, carol the member, both with
// passwords) served with OIDC through idp. The rate limit of the sign-in
// starts is lifted unless the test sets it.
type oidcAPI struct {
	*usersAPI
	idp *fakeIdP
}

func newOIDCAPI(t *testing.T, driver string, edit func(*config.OIDCClient)) *oidcAPI {
	t.Helper()
	o := &oidcAPI{usersAPI: newUsersAPI(t, driver), idp: newFakeIdP(t)}
	o.serve(config.MFAOff, edit)
	o.srv.oidcLimiter = newKeyLimiter(1000000, time.Minute)
	return o
}

// serve serves the same database and sessions with OIDC.
func (o *oidcAPI) serve(policy string, edit func(*config.OIDCClient)) {
	c := &config.OIDCClient{Issuer: o.idp.URL, ClientID: oidcClientID, Secret: oidcSecret, RedirectURI: usersOrigin + config.OIDCCallbackPath, FormActionOrigins: []string{o.idp.URL}}
	if edit != nil {
		edit(c)
	}
	srv := newServer(config.Config{PublicURL: usersOrigin, Secure: true, MFARequired: policy, OIDC: c}, o.srv.auth, fstest.MapFS{}, nil)
	srv.oidc.cfg.Transport = o.idp.Client().Transport
	o.srv, o.handler = srv, srv.handler()
}

// loginForm posts the sign-in form from origin ("" for none) with the
// remote address addr.
func (o *oidcAPI) loginForm(form url.Values, origin, addr string) *httptest.ResponseRecorder {
	o.t.Helper()
	r := httptest.NewRequest("POST", oidcLoginPath, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	if addr != "" {
		r.RemoteAddr = addr
	}
	return serve(o.t, o.handler, r)
}

// start posts the sign-in form with returnTo (none when empty) and returns
// the authorization request and the cookie that binds it to the browser.
func (o *oidcAPI) start(returnTo string) (*url.URL, string) {
	o.t.Helper()
	form := url.Values{}
	if returnTo != "" {
		form.Set("return_to", returnTo)
	}
	w := o.loginForm(form, usersOrigin, "")
	if w.Code != http.StatusSeeOther {
		o.t.Fatalf("login: %d %v %s", w.Code, w.Header(), w.Body.String())
	}
	loc, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		o.t.Fatal(err)
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || !strings.HasPrefix(cookies[0].Name, "__Host-") || !cookies[0].HttpOnly || !cookies[0].Secure || cookies[0].Path != "/" || cookies[0].Domain != "" || cookies[0].MaxAge <= 0 {
		o.t.Fatalf("binding cookie: %v", w.Header().Values("Set-Cookie"))
	}
	return loc, cookies[0].Name + "=" + cookies[0].Value
}

// callback sends the callback query with the binding cookie (none when
// empty).
func (o *oidcAPI) callback(query, cookie string) *httptest.ResponseRecorder {
	o.t.Helper()
	r := httptest.NewRequest("GET", config.OIDCCallbackPath+"?"+query, nil)
	if cookie != "" {
		r.Header.Set("Cookie", cookie)
	}
	return serve(o.t, o.handler, r)
}

// signIn runs a whole sign-in as the user of c, returning to returnTo.
func (o *oidcAPI) signIn(c map[string]any, returnTo string) *httptest.ResponseRecorder {
	o.t.Helper()
	loc, cookie := o.start(returnTo)
	return o.callback(o.idp.grant(loc, c), cookie)
}

// signedIn checks that w redirects to location with a session and returns
// the session cookie.
func (o *oidcAPI) signedIn(w *httptest.ResponseRecorder, location string) string {
	o.t.Helper()
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != location {
		o.t.Fatalf("callback: %d %v", w.Code, w.Header())
	}
	return sessionCookie(o.t, w)
}

// me returns the user of the session cookie.
func (o *oidcAPI) me(cookie string) userResponse {
	o.t.Helper()
	w := o.do("GET", "/api/v1/me", cookie, "")
	expect(o.t, w, 200, "")
	return decodeJSON[userResponse](o.t, w)
}

// expectSigninError checks a redirect to the sign-in error screen with
// reason that starts no session.
func expectSigninError(t *testing.T, w *httptest.ResponseRecorder, reason string) {
	t.Helper()
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != signinErrorPath+"?reason="+reason {
		t.Fatalf("got %d %v, want reason %s", w.Code, w.Header(), reason)
	}
	if issuedCookie(w) {
		t.Fatalf("session started: %v", w.Header().Values("Set-Cookie"))
	}
}

// linkedTo returns the identity alice's address is linked to.
func (o *oidcAPI) linkedTo(email string) (string, string) {
	o.t.Helper()
	u, err := o.store.UserByEmail(context.Background(), email)
	if err != nil {
		o.t.Fatal(err)
	}
	return u.OidcIssuer.String, u.OidcSubject.String
}

func TestAuthConfig(t *testing.T) {
	for _, tc := range []struct {
		cfg  config.Config
		want string
	}{
		{config.Config{PublicURL: usersOrigin}, `{"mode":"password"}`},
		{config.Config{PublicURL: usersOrigin, OIDC: &config.OIDCClient{Issuer: "https://idp.test", ClientID: "c", Secret: "s", RedirectURI: usersOrigin + config.OIDCCallbackPath}}, `{"mode":"oidc"}`},
	} {
		h := New(tc.cfg, nil, fstest.MapFS{}, nil)
		w := serve(t, h, httptest.NewRequest("GET", "/auth/config", nil))
		if w.Code != 200 || strings.TrimSpace(w.Body.String()) != tc.want || w.Header().Get("Content-Type") != "application/json" {
			t.Fatalf("%d %s", w.Code, w.Body.String())
		}
		r := httptest.NewRequest("POST", "/auth/config", strings.NewReader("{}"))
		r.Header.Set("Origin", usersOrigin)
		expect(t, serve(t, h, r), 405, "method_not_allowed")
	}
}

func TestOIDCRoutesWithoutOIDC(t *testing.T) {
	api := newUsersAPI(t, "sqlite")
	if api.srv.oidc != nil {
		t.Fatal("OIDC client without OIDC")
	}
	for _, method := range []string{"GET", "POST"} {
		for _, p := range []string{oidcLoginPath, config.OIDCCallbackPath + "?code=x&state=y"} {
			r := httptest.NewRequest(method, p, nil)
			r.Header.Set("Origin", usersOrigin)
			w := serve(t, api.handler, r)
			expect(t, w, 404, "not_found")
			if len(w.Result().Cookies()) != 0 {
				t.Fatalf("%s %s: cookie set", method, p)
			}
		}
	}
}

// TestOIDCLogin checks the start of a sign-in: a form POST of this origin,
// limited per peer, with the provider discovered once.
func TestOIDCLogin(t *testing.T) {
	o := newOIDCAPI(t, "sqlite", nil)

	// GET does not start a sign-in: a link or an image cannot.
	r := httptest.NewRequest("GET", oidcLoginPath+"?return_to=%2F", nil)
	expect(t, serve(t, o.handler, r), 405, "method_not_allowed")
	// The Origin must be this server's, exactly.
	for _, origin := range []string{"", "null", "https://evil.test", "http://astovia.test", "https://astovia.test:8443", "https://astovia.test/"} {
		w := o.loginForm(url.Values{"return_to": {"/settings"}}, origin, "")
		expect(t, w, 403, "origin_rejected")
		if len(w.Result().Cookies()) != 0 {
			t.Fatalf("origin %q: cookie set", origin)
		}
	}
	if n := o.idp.discoveryCount(); n != 0 {
		t.Fatalf("discoveries before a sign-in: %d", n)
	}

	// The provider is discovered on the first sign-in, and only then.
	loc, _ := o.start("")
	o.start("/settings")
	if n := o.idp.discoveryCount(); n != 1 {
		t.Fatalf("discoveries: %d", n)
	}
	if loc.Query().Get("prompt") != "" || loc.Query().Get("login_hint") != "" {
		t.Fatalf("authorization request: %s", loc)
	}
	// Each start has its own state and nonce.
	other, _ := o.start("")
	if other.Query().Get("state") == loc.Query().Get("state") || other.Query().Get("nonce") == loc.Query().Get("nonce") {
		t.Fatal("state or nonce reused")
	}

	// A malformed form is refused.
	r = httptest.NewRequest("POST", oidcLoginPath, strings.NewReader("return_to=%zz"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", usersOrigin)
	expect(t, serve(t, o.handler, r), 400, "invalid_request")
	r = httptest.NewRequest("POST", oidcLoginPath, strings.NewReader(strings.Repeat("a", oidcFormLimit+1)))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", usersOrigin)
	if w := serve(t, o.handler, r); w.Code != 400 && w.Code != 413 {
		t.Fatalf("too large: %d", w.Code)
	}
	// A return_to that cannot be kept is not dropped silently.
	expectSigninError(t, o.loginForm(url.Values{"return_to": {"/" + strings.Repeat("a", maxReturnTo)}}, usersOrigin, ""), reasonReturnTooLong)
	// Only the consent screen of an MCP client may return to a long
	// address; the screens of the SPA are short.
	expectSigninError(t, o.loginForm(url.Values{"return_to": {"/" + strings.Repeat("a", maxScreenReturnTo)}}, usersOrigin, ""), reasonReturnTooLong)
	expectSigninError(t, o.loginForm(url.Values{"return_to": {"/settings?x=/oauth/authorize" + strings.Repeat("a", maxScreenReturnTo)}}, usersOrigin, ""), reasonReturnTooLong)
	for _, returnTo := range []string{"/" + strings.Repeat("a", maxScreenReturnTo-1), authorizePath + "?client_id=" + strings.Repeat("a", 4000)} {
		if w := o.loginForm(url.Values{"return_to": {returnTo}}, usersOrigin, ""); w.Code != http.StatusSeeOther || strings.HasPrefix(w.Header().Get("Location"), signinErrorPath) {
			t.Fatalf("return_to of %d bytes: %d %v", len(returnTo), w.Code, w.Header())
		}
	}
}

// TestOIDCLoginsFull checks that a start over the bound of the sign-ins in
// progress evicts the oldest one instead of being refused: the new sign-in
// finishes, the evicted one fails at its callback, and a warning without
// anything of the sign-ins is logged.
func TestOIDCLoginsFull(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			o := newOIDCAPI(t, driver, nil)
			ctx := context.Background()
			if err := o.store.ApproveOIDCLink(ctx, "alice@example.com", o.now.Unix()+3600); err != nil {
				t.Fatal(err)
			}
			alice := claims("alice@example.com", "alice-sub")
			oldLoc, oldCookie := o.start("")
			// The others expire later, so the first one is the oldest.
			// authlume dates a sign-in by its own clock, the real time.
			later := time.Now().Add(time.Hour).Unix()
			rawExec(t, driver, o.dsn, fmt.Sprintf(`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < %d)
INSERT INTO oidc_logins (key, nonce, verifier, started_at, expires_at) SELECT 'k' || i, 'n', 'v', %d, %d FROM n`, store.MaxLiveOIDCLogins-1, later-600, later))
			logs := captureLog(t)
			loc, cookie := o.start("/settings")
			if !strings.Contains(logs.String(), "evicted the oldest 1") {
				t.Fatalf("log: %s", logs.String())
			}
			if strings.Contains(logs.String(), oldLoc.Query().Get("state")) || strings.Contains(logs.String(), loc.Query().Get("state")) {
				t.Fatalf("log has a state: %s", logs.String())
			}
			expectSigninError(t, o.callback(o.idp.grant(oldLoc, alice), oldCookie), reasonFailed)
			o.signedIn(o.callback(o.idp.grant(loc, alice), cookie), "/settings")
		})
	}
}

// TestOIDCLoginRateLimit checks the limit of the starts per peer: ten a
// minute, counted before the Origin check.
func TestOIDCLoginRateLimit(t *testing.T) {
	o := newOIDCAPI(t, "sqlite", nil)
	o.srv.oidcLimiter = newLoginLimiter()
	now := time.Date(2026, time.October, 23, 0, 0, 0, 0, time.UTC)
	o.srv.oidcLimiter.now = func() time.Time { return now }
	for i := 0; i < 5; i++ {
		if w := o.loginForm(url.Values{}, usersOrigin, "192.0.2.1:1234"); w.Code != http.StatusSeeOther || strings.HasPrefix(w.Header().Get("Location"), signinErrorPath) {
			t.Fatalf("start %d: %d %v", i, w.Code, w.Header())
		}
		expect(t, o.loginForm(url.Values{}, "https://evil.test", "192.0.2.1:1234"), 403, "origin_rejected")
	}
	expectSigninError(t, o.loginForm(url.Values{}, usersOrigin, "192.0.2.1:1234"), reasonRateLimited)
	expectSigninError(t, o.loginForm(url.Values{}, "https://evil.test", "192.0.2.1:1234"), reasonRateLimited)
	// Another peer is not limited.
	if w := o.loginForm(url.Values{}, usersOrigin, "192.0.2.2:1234"); w.Code != http.StatusSeeOther || strings.HasPrefix(w.Header().Get("Location"), signinErrorPath) {
		t.Fatalf("other peer: %d %v", w.Code, w.Header())
	}
	now = now.Add(time.Minute)
	if w := o.loginForm(url.Values{}, usersOrigin, "192.0.2.1:1234"); strings.HasPrefix(w.Header().Get("Location"), signinErrorPath) {
		t.Fatalf("a minute later: %d %v", w.Code, w.Header())
	}
}

// TestOIDCLoginProviderUnavailable checks a provider that cannot be
// discovered: the sign-in does not start, and discovery is tried again only
// after thirty seconds.
func TestOIDCLoginProviderUnavailable(t *testing.T) {
	o := newOIDCAPI(t, "sqlite", nil)
	now := time.Date(2026, time.October, 23, 0, 0, 0, 0, time.UTC)
	o.srv.oidc.now = func() time.Time { return now }
	o.idp.setDown(true)
	logs := captureLog(t)
	for i := 0; i < 3; i++ {
		w := o.loginForm(url.Values{}, usersOrigin, "")
		expectSigninError(t, w, reasonUnavailable)
		if len(w.Result().Cookies()) != 0 {
			t.Fatalf("cookie set: %v", w.Header().Values("Set-Cookie"))
		}
	}
	if n := o.idp.discoveryCount(); n != 1 {
		t.Fatalf("discoveries: %d", n)
	}
	if !strings.Contains(logs.String(), "auth: oidc login: cannot start the sign-in: provider unavailable") {
		t.Fatalf("log: %s", logs.String())
	}
	o.idp.setDown(false)
	now = now.Add(oidcRetryInterval - time.Second)
	expectSigninError(t, o.loginForm(url.Values{}, usersOrigin, ""), reasonUnavailable)
	now = now.Add(time.Second)
	o.start("")
	if n := o.idp.discoveryCount(); n != 2 {
		t.Fatalf("discoveries: %d", n)
	}
}

// TestOIDCReturnTo checks that return_to goes through the provider on the
// server and comes back only as a screen of this origin.
func TestOIDCReturnTo(t *testing.T) {
	o := newOIDCAPI(t, "sqlite", nil)
	if err := o.store.ApproveOIDCLink(context.Background(), "alice@example.com", o.now.Unix()+3600); err != nil {
		t.Fatal(err)
	}
	alice := claims("alice@example.com", "alice-sub")
	o.signedIn(o.signIn(alice, ""), "/")
	consent := "/oauth/authorize?" + url.Values{"client_id": {"https://client.example/cimd.json"}, "redirect_uri": {"http://127.0.0.1:3000/cb"}, "state": {strings.Repeat("s", 4000)}}.Encode()
	for returnTo, want := range map[string]string{
		"/settings":                  "/settings",
		"/tasks/abc?x=1":             "/tasks/abc?x=1",
		consent:                      consent,
		"https://evil.test/settings": "/",
		"//evil.test/settings":       "/",
		"/\\evil.test":               "/",
		"/settings#x":                "/",
		"/signin-error?reason=x":     "/",
		"/api/v1/me":                 "/",
		"/auth/logout":               "/",
		"/settings\n":                "/",
	} {
		cookie := o.signedIn(o.signIn(alice, returnTo), want)
		if me := o.me(cookie); me.ID != "alice-id" {
			t.Fatalf("%q: %+v", returnTo, me)
		}
	}
	// Several values are ignored.
	loc, cookie := func() (*url.URL, string) {
		w := o.loginForm(url.Values{"return_to": {"/settings", "/auto"}}, usersOrigin, "")
		loc, err := url.Parse(w.Header().Get("Location"))
		if err != nil {
			t.Fatal(err)
		}
		c := w.Result().Cookies()[0]
		return loc, c.Name + "=" + c.Value
	}()
	o.signedIn(o.callback(o.idp.grant(loc, alice), cookie), "/")
}

// TestOIDCCallback checks the failures of the callback: the binding to the
// browser, a callback used twice, an error of the provider and a disabled
// user.
func TestOIDCCallback(t *testing.T) {
	o := newOIDCAPI(t, "sqlite", nil)
	ctx := context.Background()
	if err := o.store.ApproveOIDCLink(ctx, "alice@example.com", o.now.Unix()+3600); err != nil {
		t.Fatal(err)
	}
	alice := claims("alice@example.com", "alice-sub")
	logs := captureLog(t)

	// Without the cookie of the browser that started it, or with another
	// one, the callback fails, and the sign-in is not spent.
	loc, cookie := o.start("/settings")
	query := o.idp.grant(loc, alice)
	expectSigninError(t, o.callback(query, ""), reasonFailed)
	_, other := o.start("")
	expectSigninError(t, o.callback(query, other), reasonFailed)
	// A state that was not issued.
	expectSigninError(t, o.callback(url.Values{"code": {"x"}, "state": {"forged"}}.Encode(), cookie), reasonFailed)
	o.signedIn(o.callback(query, cookie), "/settings")
	// The same callback again.
	expectSigninError(t, o.callback(query, cookie), reasonFailed)
	// The provider answers an error.
	loc, cookie = o.start("")
	expectSigninError(t, o.callback(url.Values{"error": {"access_denied"}, "state": {loc.Query().Get("state")}}.Encode(), cookie), reasonFailed)
	// The provider refuses the code.
	loc, cookie = o.start("")
	expectSigninError(t, o.callback(url.Values{"code": {"unknown"}, "state": {loc.Query().Get("state")}}.Encode(), cookie), reasonFailed)

	// A disabled user is not signed in, and not enabled.
	if err := o.store.ChangeUser(ctx, "alice@example.com", "", true, o.now.Unix()); err != nil {
		t.Fatal(err)
	}
	expectSigninError(t, o.signIn(alice, ""), reasonDisabled)
	for _, secret := range []string{"alice@example.com", "alice-sub", loc.Query().Get("state"), "unknown"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("log has %q: %s", secret, logs.String())
		}
	}
}

// TestOIDCFirstSignIn checks the first sign-in of an identity: an
// invitation creates the user; an existing user is linked only with the
// approval of an administrator, which expires.
func TestOIDCFirstSignIn(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			ctx := context.Background()
			o := newOIDCAPI(t, driver, nil)
			alice := claims("alice@example.com", "alice-sub")

			// Not invited, and an existing user without the approval.
			expectSigninError(t, o.signIn(claims("dave@example.com", "dave-sub"), ""), reasonNotAllowed)
			expectSigninError(t, o.signIn(alice, ""), reasonNotAllowed)
			if iss, _ := o.linkedTo("alice@example.com"); iss != "" {
				t.Fatalf("alice linked to %s", iss)
			}

			// The approval is good until its deadline, excluded.
			if err := o.store.ApproveOIDCLink(ctx, "alice@example.com", o.now.Unix()+60); err != nil {
				t.Fatal(err)
			}
			*o.now = o.now.Add(time.Minute)
			expectSigninError(t, o.signIn(alice, ""), reasonNotAllowed)
			if err := o.store.ApproveOIDCLink(ctx, "alice@example.com", o.now.Unix()+60); err != nil {
				t.Fatal(err)
			}
			*o.now = o.now.Add(time.Minute - time.Second)
			// An unverified address does not use it.
			expectSigninError(t, o.signIn(map[string]any{"sub": "alice-sub", "email": "alice@example.com", "email_verified": false}, ""), reasonNotVerified)
			cookie := o.signedIn(o.signIn(alice, ""), "/")
			if me := o.me(cookie); me.ID != "alice-id" || me.Email != "alice@example.com" {
				t.Fatalf("me: %+v", me)
			}
			if iss, sub := o.linkedTo("alice@example.com"); iss != o.idp.URL || sub != "alice-sub" {
				t.Fatalf("alice linked to %s %s", iss, sub)
			}
			// The approval is used up by the link: another identity with
			// the address is not linked, even before the deadline.
			expectSigninError(t, o.signIn(claims("alice@example.com", "other-sub"), ""), reasonNotAllowed)
			if err := o.store.ApproveOIDCLink(ctx, "alice@example.com", o.now.Unix()+60); err == nil {
				t.Fatal("approved a linked user")
			}
			// The linked identity signs in whatever its address says now.
			o.signedIn(o.signIn(map[string]any{"sub": "alice-sub", "email": "new@example.com", "email_verified": false}, ""), "/")

			// Unlinking revokes the sessions and the API tokens and needs
			// a new approval.
			w := o.do("POST", "/api/v1/api-tokens", cookie, `{"name":"cli"}`)
			expect(t, w, 201, "")
			apiToken := decodeJSON[createdAPITokenResponse](t, w).Token
			if err := o.store.UnlinkOIDC(ctx, "alice@example.com"); err != nil {
				t.Fatal(err)
			}
			expect(t, o.do("GET", "/api/v1/me", cookie, ""), 401, "unauthorized")
			expect(t, o.do("GET", "/api/v1/action-items", apiToken, ""), 401, "unauthorized")
			expectSigninError(t, o.signIn(alice, ""), reasonNotAllowed)

			// Disabling clears the approval.
			if err := o.store.ApproveOIDCLink(ctx, "carol@example.com", o.now.Unix()+3600); err != nil {
				t.Fatal(err)
			}
			if err := o.store.ChangeUser(ctx, "carol@example.com", "", true, o.now.Unix()); err != nil {
				t.Fatal(err)
			}
			expectSigninError(t, o.signIn(claims("carol@example.com", "carol-sub"), ""), reasonDisabled)
			if err := o.store.EnableUser(ctx, "carol@example.com"); err != nil {
				t.Fatal(err)
			}
			expectSigninError(t, o.signIn(claims("carol@example.com", "carol-sub"), ""), reasonNotAllowed)

			// An invitation creates the user, without a password, and is
			// used up.
			w = o.do("POST", "/api/v1/invitations", o.ownerSession(), `{"email":"dave@example.com","role":"member"}`)
			expect(t, w, 201, "")
			if strings.Contains(w.Body.String(), "/invite") {
				t.Fatalf("invitation link with OIDC: %s", w.Body.String())
			}
			cookie = o.signedIn(o.signIn(claims("Dave@Example.com", "dave-sub"), "/settings"), "/settings")
			dave := o.me(cookie)
			if dave.Email != "dave@example.com" || dave.Role != store.RoleMember {
				t.Fatalf("dave: %+v", dave)
			}
			if u, err := o.store.UserByEmail(ctx, "dave@example.com"); err != nil || u.PasswordHash != store.NoPasswordHash {
				t.Fatalf("dave: %+v %v", u, err)
			}
			if list, err := o.store.ListInvitations(ctx, o.now.Unix()); err != nil || len(list) != 0 {
				t.Fatalf("invitations: %+v %v", list, err)
			}
			// Another identity with dave's address is not linked.
			expectSigninError(t, o.signIn(claims("dave@example.com", "dave-other"), ""), reasonNotAllowed)
		})
	}
}

// ownerSession signs alice in through the store, as a session of the
// password mode would.
func (o *oidcAPI) ownerSession() string {
	o.t.Helper()
	ctx := context.Background()
	if iss, _ := o.linkedTo("alice@example.com"); iss == "" {
		if err := o.store.ApproveOIDCLink(ctx, "alice@example.com", o.now.Unix()+3600); err != nil {
			o.t.Fatal(err)
		}
		return o.signedIn(o.signIn(claims("alice@example.com", "alice-sub"), ""), "/")
	}
	iss, sub := o.linkedTo("alice@example.com")
	if iss != o.idp.URL {
		o.t.Fatalf("alice is linked to %s", iss)
	}
	return o.signedIn(o.signIn(claims("alice@example.com", sub), ""), "/")
}

// TestOIDCFirstSignInClaims checks email_verified and the hd claim at the
// first sign-in.
func TestOIDCFirstSignInClaims(t *testing.T) {
	invite := func(o *oidcAPI, email string) {
		t.Helper()
		id, err := auth.Random()
		if err != nil {
			t.Fatal(err)
		}
		if err = o.store.CreateInvitation(context.Background(), "alice-id", store.Invitation{ID: id, TokenHash: auth.Hash(id), Email: email, Role: store.RoleMember, CreatedAt: o.now.Unix(), ExpiresAt: o.now.Unix() + store.InvitationLifetime}); err != nil {
			t.Fatal(err)
		}
	}
	// email_verified may be absent unless it is required.
	o := newOIDCAPI(t, "sqlite", nil)
	invite(o, "dave@example.com")
	expectSigninError(t, o.signIn(map[string]any{"sub": "dave", "email": "dave@example.com", "email_verified": false}, ""), reasonNotVerified)
	o.signedIn(o.signIn(map[string]any{"sub": "dave", "email": "dave@example.com"}, ""), "/")
	o = newOIDCAPI(t, "sqlite", func(c *config.OIDCClient) { c.RequireEmailVerified = true })
	invite(o, "dave@example.com")
	expectSigninError(t, o.signIn(map[string]any{"sub": "dave", "email": "dave@example.com"}, ""), reasonNotVerified)
	o.signedIn(o.signIn(claims("dave@example.com", "dave"), ""), "/")
	// A malformed address.
	expectSigninError(t, o.signIn(claims("not an address", "erin"), ""), reasonFailed)

	// Google accounts: an address outside gmail.com needs hd.
	o = newOIDCAPI(t, "sqlite", func(c *config.OIDCClient) { c.RequireGoogleHD = true })
	invite(o, "dave@example.com")
	invite(o, "erin@gmail.com")
	expectSigninError(t, o.signIn(claims("dave@example.com", "dave"), ""), reasonUnsupported)
	o.signedIn(o.signIn(claims("erin@gmail.com", "erin"), ""), "/")
	c := claims("dave@example.com", "dave")
	c["hd"] = "example.com"
	o.signedIn(o.signIn(c, ""), "/")
}

// TestOIDCUnlinkAll checks the CLI's unlink of every user: the sessions
// end and no approval is left, so nobody is linked again without one.
func TestOIDCUnlinkAll(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			ctx := context.Background()
			o := newOIDCAPI(t, driver, nil)
			alice := o.ownerSession()
			n, err := o.store.UnlinkAllOIDC(ctx)
			if err != nil || n != 1 {
				t.Fatalf("unlink all: %d %v", n, err)
			}
			expect(t, o.do("GET", "/api/v1/me", alice, ""), 401, "unauthorized")
			expectSigninError(t, o.signIn(claims("alice@example.com", "alice-sub"), ""), reasonNotAllowed)
		})
	}
}

// TestPasswordEndpointsWithOIDC checks that the password sign-in, the
// links and the second factors of Astovia are off with OIDC, without
// counting attempts, and that OIDC signs in a user with second factors.
func TestPasswordEndpointsWithOIDC(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			ctx := context.Background()
			o := newOIDCAPI(t, driver, nil)
			o.serve(config.MFAAll, nil)
			if _, err := o.store.EnrollTOTP(ctx, store.MFATOTP{ID: "totp-alice", UserID: "alice-id", SecretSealed: "sealed", LastStep: 1, CreatedAt: 1}, []store.RecoveryCode{{ID: "code", Hash: "hash"}}, store.EnrollOptions{PasswordDigest: o.passwordDigest("alice-id"), Now: o.now.Unix()}); err != nil {
				t.Fatal(err)
			}
			disabled := func(w *httptest.ResponseRecorder) {
				t.Helper()
				expect(t, w, 409, "password_auth_disabled")
				if len(w.Result().Cookies()) != 0 {
					t.Fatal("cookie set")
				}
			}
			for range 15 {
				for _, path := range []string{"/auth/login", "/auth/invitations/inspect", "/auth/invitations/accept", "/auth/password-resets/inspect", "/auth/password-resets/complete",
					"/auth/mfa/totp", "/auth/mfa/recovery", "/auth/mfa/enroll/totp/begin", "/auth/mfa/enroll/totp/finish", "/auth/mfa/webauthn/begin", "/auth/mfa/webauthn/finish", "/auth/mfa/enroll/webauthn/begin", "/auth/mfa/enroll/webauthn/finish"} {
					disabled(o.public(path, `{"email":"carol@example.com","password":"correct-password-123","token":"x","mfa_token":"x"}`, ""))
				}
			}
			// Even without the content type.
			r := httptest.NewRequest("POST", "/auth/login", strings.NewReader("x"))
			r.Header.Set("Origin", usersOrigin)
			disabled(serve(t, o.handler, r))

			// The callback signs alice in without a second factor.
			alice := o.ownerSession()
			if me := o.me(alice); me.ID != "alice-id" {
				t.Fatalf("me: %+v", me)
			}
			for range 12 {
				expect(t, o.do("GET", "/api/v1/mfa", alice, ""), 409, "password_auth_disabled")
				for _, path := range []string{"/api/v1/mfa/totp/begin", "/api/v1/mfa/totp/finish", "/api/v1/mfa/totp/delete", "/api/v1/mfa/recovery-codes", "/api/v1/mfa/webauthn/begin", "/api/v1/mfa/webauthn/finish", "/api/v1/mfa/webauthn/x/delete"} {
					expect(t, o.do("POST", path, alice, `{"password":"`+mfaPassword+`"}`), 409, "password_auth_disabled")
				}
			}
			disabled(o.do("POST", "/api/v1/users/carol-id/password-resets", alice, ""))
			expect(t, o.do("GET", "/api/v1/mfa", "", ""), 401, "unauthorized")
			if status, err := o.store.MFAStatus(ctx, "alice-id"); err != nil || status.TOTP == nil || status.RecoveryCodes != 1 {
				t.Fatalf("alice: %+v %v", status, err)
			}
			// The 409s consumed no attempts: with passwords again, the login
			// is not limited.
			o.srv.cfg.OIDC = nil
			expect(t, o.public("/auth/login", `{"email":"carol@example.com","password":"wrong-password-123"}`, ""), 401, "unauthorized")
			// carol's password is the same.
			expect(t, o.public("/auth/login", `{"email":"carol@example.com","password":"correct-password-123"}`, ""), 200, "")
		})
	}
}

// TestOIDCIDTokenRejected checks that an ID token that fails verification
// starts no session: a nonce of another sign-in, another audience, an
// expired token and a signature of another key.
func TestOIDCIDTokenRejected(t *testing.T) {
	o := newOIDCAPI(t, "sqlite", nil)
	if err := o.store.ApproveOIDCLink(context.Background(), "alice@example.com", o.now.Unix()+3600); err != nil {
		t.Fatal(err)
	}
	with := func(extra map[string]any) map[string]any {
		c := claims("alice@example.com", "alice-sub")
		for k, v := range extra {
			c[k] = v
		}
		return c
	}
	past := time.Now().Add(-time.Hour)
	for _, tc := range []struct {
		name   string
		claims map[string]any
		forged bool
	}{
		{"nonce", with(map[string]any{"nonce": "another-nonce"}), false},
		{"audience", with(map[string]any{"aud": "another-client"}), false},
		{"expired", with(map[string]any{"iat": past.Add(-time.Hour).Unix(), "exp": past.Unix()}), false},
		{"signature", with(nil), true},
	} {
		loc, cookie := o.start("")
		query := o.idp.grant(loc, tc.claims)
		if tc.forged {
			o.idp.forge(query)
		}
		w := o.callback(query, cookie)
		if w.Code != http.StatusSeeOther || w.Header().Get("Location") != signinErrorPath+"?reason="+reasonFailed || issuedCookie(w) {
			t.Fatalf("%s: %d %v", tc.name, w.Code, w.Header())
		}
		if issuer, subject := o.linkedTo("alice@example.com"); issuer != "" || subject != "" {
			t.Fatalf("%s: linked to %s %s", tc.name, issuer, subject)
		}
	}
	// The same sign-in with a valid token goes through.
	o.signedIn(o.signIn(claims("alice@example.com", "alice-sub"), "/settings"), "/settings")
}

// TestOIDCLoginExpired checks that a callback after its sign-in expired
// fails and starts no session.
func TestOIDCLoginExpired(t *testing.T) {
	mfaDrivers(t, func(t *testing.T, driver string) {
		o := newOIDCAPI(t, driver, nil)
		if err := o.store.ApproveOIDCLink(context.Background(), "alice@example.com", o.now.Unix()+3600); err != nil {
			t.Fatal(err)
		}
		loc, cookie := o.start("/settings")
		query := o.idp.grant(loc, claims("alice@example.com", "alice-sub"))
		// authlume keeps the sign-in until its expiry by the wall clock;
		// move the expiry into the past instead of waiting for it.
		rawExec(t, driver, o.dsn, fmt.Sprintf("UPDATE oidc_logins SET expires_at = %d", time.Now().Add(-time.Second).Unix()))
		expectSigninError(t, o.callback(query, cookie), reasonFailed)
		if issuer, _ := o.linkedTo("alice@example.com"); issuer != "" {
			t.Fatal("linked after an expired sign-in")
		}
	})
}
