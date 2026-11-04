package httpserver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/i2bskn/astovia/internal/auth"
	"github.com/i2bskn/astovia/internal/config"
	"github.com/i2bskn/astovia/internal/oidc"
	"github.com/i2bskn/astovia/internal/secretbox"
	"github.com/i2bskn/astovia/internal/store"
	"golang.org/x/crypto/bcrypt"
)

const (
	testIssuer       = "https://idp.example.com/realms/astovia"
	testOIDCClientID = "client-id"
)

// oidcConfig is the configuration of usersOrigin with OIDC. Every address
// is allowed and email_verified is required.
func oidcConfig() config.Config {
	return config.Config{PublicURL: usersOrigin, Secure: true, Secret: make([]byte, 32), OIDC: &config.OIDCClient{
		Issuer:               testIssuer,
		ClientID:             testOIDCClientID,
		Secret:               "client-secret",
		RedirectURI:          usersOrigin + config.OIDCCallbackPath,
		AllowedEmails:        []string{"*"},
		RequireEmailVerified: true,
	}}
}

// fakeOIDC answers AuthorizationURL with the authorization endpoint of the
// test issuer or authErr, Exchange with idToken or exchangeErr and Verify
// with email and verified or verifyErr, and records the arguments.
type fakeOIDC struct {
	mu          sync.Mutex
	authErr     error
	idToken     string
	exchangeErr error
	email       string
	verified    *bool
	verifyErr   error

	code, verifier string
	token, nonce   string
	exchanges      int
	verifies       int
}

func newFakeOIDC() *fakeOIDC {
	verified := true
	return &fakeOIDC{idToken: "id-token-value", email: "alice@example.com", verified: &verified}
}

func (f *fakeOIDC) AuthorizationURL(state, nonce, challenge string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.authErr != nil {
		return "", f.authErr
	}
	c := oidcConfig().OIDC
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {c.ClientID},
		"redirect_uri":          {c.RedirectURI},
		"scope":                 {"openid email"},
		"state":                 {state},
		"nonce":                 {nonce},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}
	return testIssuer + "/authorization?" + q.Encode(), nil
}

func (f *fakeOIDC) Exchange(_ context.Context, code, verifier string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.exchanges++
	f.code, f.verifier = code, verifier
	if f.exchangeErr != nil {
		return "", f.exchangeErr
	}
	return f.idToken, nil
}

func (f *fakeOIDC) Verify(_ context.Context, idToken, nonce string) (oidc.Identity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.verifies++
	f.token, f.nonce = idToken, nonce
	if f.verifyErr != nil {
		return oidc.Identity{}, f.verifyErr
	}
	return oidc.Identity{Email: f.email, EmailVerified: f.verified}, nil
}

func (f *fakeOIDC) set(email string, exchangeErr, verifyErr error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.email, f.exchangeErr, f.verifyErr = email, exchangeErr, verifyErr
}

// setVerified sets the email_verified claim: nil is absent.
func (f *fakeOIDC) setVerified(verified *bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.verified = verified
}

func (f *fakeOIDC) setAuthErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.authErr = err
}

func (f *fakeOIDC) exchangeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.exchanges
}

// newOIDCAPI is newUsersAPI (alice the owner, carol the member) served
// with OIDC and the fake OIDC client. The rate limits are the real ones.
func newOIDCAPI(t *testing.T, driver string) (*usersAPI, *fakeOIDC) {
	t.Helper()
	api := newUsersAPI(t, driver)
	srv := newServer(oidcConfig(), api.srv.auth, fstest.MapFS{}, nil)
	if _, ok := srv.rp.(*oidc.Client); !ok {
		t.Fatalf("no OIDC client: %T", srv.rp)
	}
	rp := newFakeOIDC()
	srv.rp = rp
	api.srv, api.handler = srv, srv.handler()
	return api, rp
}

// responseCookie returns the cookie name set by w and its Set-Cookie line.
func responseCookie(w *httptest.ResponseRecorder, name string) (*http.Cookie, string) {
	for _, line := range w.Header().Values("Set-Cookie") {
		c, err := http.ParseSetCookie(line)
		if err == nil && c.Name == name {
			return c, line
		}
	}
	return nil, ""
}

// expectOIDCCookie checks the attributes of the sign-in cookie set by w
// and returns it.
func expectOIDCCookie(t *testing.T, w *httptest.ResponseRecorder, maxAge int) *http.Cookie {
	t.Helper()
	c, line := responseCookie(w, oidcCookie)
	if c == nil {
		t.Fatalf("no %s cookie: %v", oidcCookie, w.Header().Values("Set-Cookie"))
	}
	if c.Path != "/" || c.Domain != "" || !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode || c.MaxAge != maxAge {
		t.Fatalf("cookie attributes: %s", line)
	}
	if maxAge < 0 && c.Value != "" {
		t.Fatalf("cleared cookie has a value: %s", line)
	}
	return c
}

// captureLog sends the log output to a buffer for the rest of the test.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return &buf
}

// oidcLogin starts a sign-in with the raw query.
func oidcLogin(t *testing.T, h http.Handler, query string) *httptest.ResponseRecorder {
	t.Helper()
	return serve(t, h, httptest.NewRequest("GET", "/auth/oidc/login"+query, nil))
}

// newOIDCState returns a sign-in state returning to returnTo and its
// cookie value, sealed by srv.
func newOIDCState(t *testing.T, srv *Server, returnTo string) (oidcState, string) {
	t.Helper()
	st := oidcState{ReturnTo: returnTo}
	for _, p := range []*string{&st.State, &st.Nonce, &st.Verifier} {
		v, err := auth.Random()
		if err != nil {
			t.Fatal(err)
		}
		*p = v
	}
	value, err := srv.sealOIDCState(st)
	if err != nil {
		t.Fatal(err)
	}
	return st, value
}

// callbackRequest returns the callback request with query and the sign-in
// cookie value, if any.
func callbackRequest(query, cookie string) *http.Request {
	r := httptest.NewRequest("GET", "/auth/oidc/callback?"+query, nil)
	if cookie != "" {
		r.AddCookie(&http.Cookie{Name: oidcCookie, Value: cookie})
	}
	return r
}

// expectSigninError checks a failed callback: a redirect without a body to
// the error screen with reason, the sign-in cookie cleared and no refresh
// cookie.
func expectSigninError(t *testing.T, w *httptest.ResponseRecorder, reason string) {
	t.Helper()
	if w.Code != 303 || w.Header().Get("Location") != "/signin-error?reason="+reason || w.Body.Len() != 0 {
		t.Fatalf("%d %v %q, want reason %s", w.Code, w.Header(), w.Body.String(), reason)
	}
	expectOIDCCookie(t, w, -1)
	if c, _ := responseCookie(w, CookieName); c != nil {
		t.Fatal("refresh cookie set")
	}
}

// expectRefreshCookie checks the refresh cookie of a successful sign-in and
// returns it as a Cookie header.
func expectRefreshCookie(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	c, line := responseCookie(w, CookieName)
	if c == nil || c.Value == "" || c.Path != "/auth" || !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteStrictMode || c.MaxAge != int(auth.SessionLifetime/time.Second) {
		t.Fatalf("refresh cookie: %s", line)
	}
	return c.Name + "=" + c.Value
}

func TestAuthConfig(t *testing.T) {
	for _, tc := range []struct {
		cfg  config.Config
		want string
	}{
		{config.Config{PublicURL: usersOrigin}, `{"mode":"password"}`},
		{oidcConfig(), `{"mode":"oidc"}`},
	} {
		h := New(tc.cfg, nil, fstest.MapFS{}, nil)
		w := serve(t, h, httptest.NewRequest("GET", "/auth/config", nil))
		if w.Code != 200 || strings.TrimSpace(w.Body.String()) != tc.want || w.Header().Get("Content-Type") != "application/json" {
			t.Fatalf("%d %s", w.Code, w.Body.String())
		}
		w = serve(t, h, httptest.NewRequest("POST", "/auth/config", strings.NewReader("{}")))
		expect(t, w, 405, "method_not_allowed")
	}
}

func TestOIDCRoutesWithoutOIDC(t *testing.T) {
	api := newUsersAPI(t, "sqlite")
	if api.srv.rp != nil {
		t.Fatal("OIDC client without OIDC")
	}
	for _, p := range []string{"/auth/oidc/login", "/auth/oidc/login?return_to=%2F", "/auth/oidc/callback?code=x&state=y"} {
		w := serve(t, api.handler, httptest.NewRequest("GET", p, nil))
		expect(t, w, 404, "not_found")
		if c, _ := responseCookie(w, oidcCookie); c != nil {
			t.Fatalf("%s: cookie set", p)
		}
	}
}

func TestOIDCLogin(t *testing.T) {
	api, rp := newOIDCAPI(t, "sqlite")
	h := api.handler

	returnTo := "/consent?request=abc_DEF-123"
	w := oidcLogin(t, h, "?return_to="+url.QueryEscape(returnTo))
	if w.Code != 302 || w.Header().Get("Cache-Control") != "no-store" || w.Body.Len() != 0 {
		t.Fatalf("%d %v %q", w.Code, w.Header(), w.Body.String())
	}
	c := expectOIDCCookie(t, w, oidcCookieMaxAge)
	// The cookie is sealed: the state is not readable from it.
	if !strings.HasPrefix(c.Value, "v1.") || strings.Contains(c.Value, "consent") {
		t.Fatalf("cookie %q", c.Value)
	}
	st, ok := api.srv.openOIDCState(c.Value)
	if !ok || st.ReturnTo != returnTo {
		t.Fatalf("state %+v %v", st, ok)
	}
	loc, err := url.Parse(w.Header().Get("Location"))
	if err != nil || loc.Scheme+"://"+loc.Host+loc.Path != testIssuer+"/authorization" {
		t.Fatalf("location %q", w.Header().Get("Location"))
	}
	q := loc.Query()
	if q.Get("state") != st.State || q.Get("nonce") != st.Nonce || q.Get("code_challenge_method") != "S256" ||
		q.Get("response_type") != "code" || q.Get("scope") != "openid email" || q.Get("client_id") != testOIDCClientID || q.Get("redirect_uri") != usersOrigin+"/auth/oidc/callback" {
		t.Fatalf("query %v", q)
	}
	// S256 of the verifier, as in RFC 7636; the verifier itself never
	// leaves the cookie.
	sum := sha256.Sum256([]byte(st.Verifier))
	if q.Get("code_challenge") != base64.RawURLEncoding.EncodeToString(sum[:]) || strings.Contains(w.Header().Get("Location"), st.Verifier) {
		t.Fatal("code_challenge")
	}
	// Every sign-in has new values.
	other, _ := api.srv.openOIDCState(expectOIDCCookie(t, oidcLogin(t, h, ""), oidcCookieMaxAge).Value)
	if other.State == st.State || other.Nonce == st.Nonce || other.Verifier == st.Verifier || other.ReturnTo != "/" {
		t.Fatalf("%+v", other)
	}

	// Kept return paths, including HTML characters, which are not escaped.
	for _, v := range []string{"/", "/candidates", "/auto/", "/settings", "/settings/notifications", "/account", "/account/users", "/tasks/abc_DEF-1", "/consent", "/integrations/google/callback", "/?x=1", "/candidates?q=<a&b>\"'", "/consent?request=" + strings.Repeat("x", 2500)} {
		w := oidcLogin(t, h, "?return_to="+url.QueryEscape(v))
		st, ok := api.srv.openOIDCState(expectOIDCCookie(t, w, oidcCookieMaxAge).Value)
		if w.Code != 302 || !ok || st.ReturnTo != v {
			t.Fatalf("%q: %d %+v", v, w.Code, st)
		}
	}
	// Other sites and other paths fall back to /.
	for _, v := range []string{"//evil.test", "/\\evil.test", "https://evil.test/", "http:/candidates", "/unknown", "candidates", "", "/candidates\x01", "/candidates\t", "/candidates x", "/candidates\x7f", "/candidates?q=é",
		"/%2F%2Fevil.test", "/tasks/..%2F..%2Fx", "/tasks/../settings", "/signin-error", "/signin-error/", "/signin-error?reason=failed", "/candidates#x", "/candidates#", "/api/v1/me", "/auth/oidc/login", "/auth/refresh", "/mcp", "/tasks/a.b"} {
		w := oidcLogin(t, h, "?return_to="+url.QueryEscape(v))
		st, ok := api.srv.openOIDCState(expectOIDCCookie(t, w, oidcCookieMaxAge).Value)
		if w.Code != 302 || !ok || st.ReturnTo != "/" {
			t.Fatalf("%q: %d %+v", v, w.Code, st)
		}
	}
	for _, query := range []string{"", "?return_to=", "?return_to=%2Fsettings&return_to=%2Faccount", "?return_to=%2Fsettings&return_to=" + strings.Repeat("x", 3000)} {
		st, _ := api.srv.openOIDCState(expectOIDCCookie(t, oidcLogin(t, h, query), oidcCookieMaxAge).Value)
		if st.ReturnTo != "/" {
			t.Fatalf("%q: %+v", query, st)
		}
	}

	if w := serve(t, h, httptest.NewRequest("POST", "/auth/oidc/login", nil)); w.Code != 405 || errorCode(t, w) != "method_not_allowed" || w.Header().Get("Allow") != "GET" {
		t.Fatalf("POST: %d %v", w.Code, w.Header())
	}
	if rp.exchangeCount() != 0 {
		t.Fatal("login called the provider")
	}
}

// The longest return path that fits starts the sign-in with a cookie of less
// than 4096 bytes; a longer one does not start it.
func TestOIDCLoginReturnToLength(t *testing.T) {
	api, _ := newOIDCAPI(t, "sqlite")
	h := api.handler
	tooLong := func(w *httptest.ResponseRecorder) bool {
		t.Helper()
		if w.Code != 302 || w.Header().Get("Location") != "/signin-error?reason=return_too_long" {
			return false
		}
		if c, _ := responseCookie(w, oidcCookie); c != nil {
			t.Fatal("cookie set")
		}
		return true
	}

	// Characters that grow in JSON or would be escaped as HTML, and
	// percent-encoded non-ASCII.
	const unit = `&"\<%E3%81%82>'`
	prefix := "/consent?request="
	filler := strings.Repeat(unit, maxReturnTo/len(unit)+1)
	longest := ""
	for n := maxReturnTo; n > len(prefix); n-- {
		v := (prefix + filler)[:n]
		w := oidcLogin(t, h, "?return_to="+url.QueryEscape(v))
		if tooLong(w) {
			continue
		}
		c, line := responseCookie(w, oidcCookie)
		st, ok := api.srv.openOIDCState(c.Value)
		if w.Code != 302 || !ok || st.ReturnTo != v {
			t.Fatalf("%d bytes: %d %+v", n, w.Code, st)
		}
		pair, _, _ := strings.Cut(line, ";")
		if len(c.Value) > maxOIDCCookieValue || len(pair) >= 4096 {
			t.Fatalf("%d bytes: cookie of %d bytes", n, len(pair))
		}
		longest = v
		break
	}
	if len(longest) < 1000 {
		t.Fatalf("longest return_to: %d bytes", len(longest))
	}
	// The same characters, one byte more, do not fit.
	if !tooLong(oidcLogin(t, h, "?return_to="+url.QueryEscape((prefix + filler)[:len(longest)+1]))) {
		t.Fatalf("%d bytes fit", len(longest)+1)
	}

	// 2700 bytes of ASCII pass the length check but not the cookie size;
	// longer ones fail the length check.
	for _, n := range []int{2700, 2701, 10000} {
		v := prefix + strings.Repeat("x", n-len(prefix))
		if !tooLong(oidcLogin(t, h, "?return_to="+url.QueryEscape(v))) {
			t.Fatalf("%d bytes fit", n)
		}
	}
}

func TestOIDCCallback(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) { testOIDCCallback(t, driver) })
	}
}

func testOIDCCallback(t *testing.T, driver string) {
	logs := captureLog(t)
	api, rp := newOIDCAPI(t, driver)
	h := api.handler
	ctx := context.Background()
	countUsers := func() int {
		t.Helper()
		users, err := api.store.ListUsers(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return len(users)
	}
	refresh := func(cookie string) authResponse {
		t.Helper()
		w := api.public("/auth/refresh", `{}`, cookie)
		expect(t, w, 200, "")
		return decodeJSON[authResponse](t, w)
	}

	// The full flow: login, then the callback from the provider. The address
	// matches alice regardless of case, and alice stays an owner.
	returnTo := "/consent?request=abc"
	w := oidcLogin(t, h, "?return_to="+url.QueryEscape(returnTo))
	cookie := expectOIDCCookie(t, w, oidcCookieMaxAge)
	loc, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	st, _ := api.srv.openOIDCState(cookie.Value)
	rp.set("Alice@Example.com", nil, nil)
	w = serve(t, h, callbackRequest(url.Values{"code": {"code-value-1"}, "state": {loc.Query().Get("state")}}.Encode(), cookie.Value))
	if w.Code != 303 || w.Header().Get("Location") != returnTo || w.Header().Get("Cache-Control") != "no-store" || w.Body.Len() != 0 {
		t.Fatalf("%d %v %q", w.Code, w.Header(), w.Body.String())
	}
	expectOIDCCookie(t, w, -1)
	session := expectRefreshCookie(t, w)
	// The verifier and the nonce of the cookie reach the provider.
	if rp.code != "code-value-1" || rp.verifier != st.Verifier || rp.token != "id-token-value" || rp.nonce != st.Nonce {
		t.Fatalf("%+v", rp)
	}
	// The SPA restores the session with the refresh cookie.
	if got := refresh(session).User; got.ID != "alice-id" || got.Email != "alice@example.com" || got.Role != store.RoleOwner {
		t.Fatalf("%+v", got)
	}
	if n := countUsers(); n != 2 {
		t.Fatalf("users: %d", n)
	}
	// The cookie is single use: the same callback again fails.
	expectSigninError(t, serve(t, h, callbackRequest(url.Values{"code": {"code-value-1"}, "state": {st.State}}.Encode(), "")), reasonFailed)

	valid := func() (oidcState, string) { return newOIDCState(t, api.srv, "/") }
	query := func(st oidcState, extra url.Values) string {
		q := url.Values{"code": {"code-value-1"}, "state": {st.State}}
		for k, v := range extra {
			q[k] = v
		}
		return q.Encode()
	}
	seal := func(box *secretbox.Box, purpose, plain string) string {
		t.Helper()
		v, err := box.Seal(purpose, plain)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	box := api.srv.box
	st, value := valid()
	// One character of the sealed nonce changed.
	tampered := []byte(value)
	if tampered[10] == 'A' {
		tampered[10] = 'B'
	} else {
		tampered[10] = 'A'
	}
	plain := func(r string, extra string) string {
		return fmt.Sprintf(`{"s":%q,"n":%q,"v":%q,"r":%q%s}`, st.State, st.Nonce, st.Verifier, r, extra)
	}
	for name, tc := range map[string]struct {
		query, cookie string
		reason        string
	}{
		"no cookie":          {query(st, nil), "", reasonFailed},
		"garbage cookie":     {query(st, nil), "not-sealed", reasonFailed},
		"tampered cookie":    {query(st, nil), string(tampered), reasonFailed},
		"unsealed JSON":      {query(st, nil), base64.RawURLEncoding.EncodeToString([]byte(plain("/", ""))), reasonFailed},
		"other purpose":      {query(st, nil), seal(box, "google-auth", plain("/", "")), reasonFailed},
		"other secret":       {query(st, nil), seal(secretbox.New([]byte(strings.Repeat("x", 32))), oidcSealPurpose, plain("/", "")), reasonFailed},
		"unknown key":        {query(st, nil), seal(box, oidcSealPurpose, plain("/", `,"x":1`)), reasonFailed},
		"trailing data":      {query(st, nil), seal(box, oidcSealPurpose, plain("/", "")+"{}"), reasonFailed},
		"short state":        {query(st, nil), seal(box, oidcSealPurpose, fmt.Sprintf(`{"s":"abc","n":%q,"v":%q,"r":"/"}`, st.Nonce, st.Verifier)), reasonFailed},
		"padded nonce":       {query(st, nil), seal(box, oidcSealPurpose, fmt.Sprintf(`{"s":%q,"n":%q,"v":%q,"r":"/"}`, st.State, st.Nonce[:42]+"=", st.Verifier)), reasonFailed},
		"no verifier":        {query(st, nil), seal(box, oidcSealPurpose, fmt.Sprintf(`{"s":%q,"n":%q,"r":"/"}`, st.State, st.Nonce)), reasonFailed},
		"other site return":  {query(st, nil), seal(box, oidcSealPurpose, plain("//evil.test", "")), reasonFailed},
		"signin-error":       {query(st, nil), seal(box, oidcSealPurpose, plain("/signin-error", "")), reasonFailed},
		"state mismatch":     {query(oidcState{State: st.State + "x"}, nil), value, reasonFailed},
		"other state":        {query(oidcState{State: strings.Repeat("A", 43)}, nil), value, reasonFailed},
		"no state":           {"code=code-value-1", value, reasonFailed},
		"two states":         {query(st, url.Values{"state": {st.State, st.State}}), value, reasonFailed},
		"denied":             {query(st, url.Values{"error": {"access_denied"}}), value, reasonDenied},
		"denied empty error": {query(st, url.Values{"error": {""}}), value, reasonDenied},
		"denied bad state":   {query(oidcState{State: "x"}, url.Values{"error": {"access_denied"}}), value, reasonFailed},
		"denied no state":    {"error=access_denied", value, reasonFailed},
		"no code":            {url.Values{"state": {st.State}}.Encode(), value, reasonFailed},
		"empty code":         {query(st, url.Values{"code": {""}}), value, reasonFailed},
		"two codes":          {query(st, url.Values{"code": {"a", "b"}}), value, reasonFailed},
		"long code":          {query(st, url.Values{"code": {strings.Repeat("c", 2049)}}), value, reasonFailed},
	} {
		t.Run(name, func(t *testing.T) {
			before := rp.exchangeCount()
			expectSigninError(t, serve(t, h, callbackRequest(tc.query, tc.cookie)), tc.reason)
			if rp.exchangeCount() != before {
				t.Fatal("exchanged")
			}
		})
	}
	// A code of 2048 bytes is accepted.
	rp.set("alice@example.com", nil, nil)
	w = serve(t, h, callbackRequest(query(st, url.Values{"code": {strings.Repeat("c", 2048)}}), value))
	if w.Code != 303 || w.Header().Get("Location") != "/" {
		t.Fatalf("2048 bytes: %d %v", w.Code, w.Header())
	}
	expectRefreshCookie(t, w)

	// Provider errors.
	for name, tc := range map[string]struct {
		exchangeErr, verifyErr error
		reason                 string
	}{
		"provider unavailable": {oidc.ErrProviderUnavailable, nil, reasonUnavailable},
		"exchange failed":      {oidc.ErrExchangeFailed, nil, reasonFailed},
		"exchange other error": {errors.New("other"), nil, reasonFailed},
		"keys unavailable":     {nil, oidc.ErrKeysUnavailable, reasonUnavailable},
		"invalid ID token":     {nil, oidc.ErrInvalidToken, reasonFailed},
		"verify other error":   {nil, errors.New("other"), reasonFailed},
	} {
		t.Run(name, func(t *testing.T) {
			rp.set("alice@example.com", tc.exchangeErr, tc.verifyErr)
			defer rp.set("alice@example.com", nil, nil)
			st, value := valid()
			expectSigninError(t, serve(t, h, callbackRequest(query(st, nil), value)), tc.reason)
		})
	}

	// The first sign-in of an address creates a member that no password
	// matches; the next sign-in finds it.
	rp.set(" Dave@Example.COM ", nil, nil)
	st, value = valid()
	w = serve(t, h, callbackRequest(query(st, nil), value))
	if w.Code != 303 || w.Header().Get("Location") != "/" {
		t.Fatalf("%d %v", w.Code, w.Header())
	}
	daveCookie := expectRefreshCookie(t, w)
	dave, err := api.store.UserByEmail(ctx, "dave@example.com")
	if err != nil || dave.Role != store.RoleMember || dave.Disabled != 0 || dave.PasswordHash != "!access" || dave.CreatedAt != api.now.Unix() {
		t.Fatalf("%+v %v", dave, err)
	}
	if got := refresh(daveCookie).User; got.ID != dave.ID || got.Role != store.RoleMember {
		t.Fatalf("%+v", got)
	}
	st, value = valid()
	expectRefreshCookie(t, serve(t, h, callbackRequest(query(st, nil), value)))
	if n := countUsers(); n != 3 {
		t.Fatalf("users: %d", n)
	}
	// Back on passwords, no password signs dave in.
	if err = bcrypt.CompareHashAndPassword([]byte(dave.PasswordHash), []byte("!access")); err == nil {
		t.Fatal("the hash matched")
	}
	for _, password := range []string{"!access", "correct-password-123", "x"} {
		if _, err = api.srv.auth.Login(ctx, "dave@example.com", password); !errors.Is(err, store.ErrUnauthorized) {
			t.Fatalf("password %q: %v", password, err)
		}
	}

	// A disabled user gets no session and stays disabled.
	expect(t, api.do("PATCH", "/api/v1/users/carol-id", ownerToken(t, api), `{"disabled":true}`), 200, "")
	rp.set("carol@example.com", nil, nil)
	st, value = valid()
	expectSigninError(t, serve(t, h, callbackRequest(query(st, nil), value)), reasonDisabled)
	if u, err := api.store.UserByEmail(ctx, "carol@example.com"); err != nil || u.Disabled == 0 {
		t.Fatalf("%+v %v", u, err)
	}

	// A malformed email claim creates nobody.
	for _, email := range []string{"not an address", "bob@", "bøb@example.com", "", strings.Repeat("a", 250) + "@example.com"} {
		rp.set(email, nil, nil)
		st, value = valid()
		expectSigninError(t, serve(t, h, callbackRequest(query(st, nil), value)), reasonFailed)
	}
	if n := countUsers(); n != 3 {
		t.Fatalf("users: %d", n)
	}

	// Concurrent first sign-ins of one address create one user.
	rp.set("erin@example.com", nil, nil)
	const concurrent = 8
	codes := make(chan string, concurrent)
	var wg sync.WaitGroup
	for range concurrent {
		st, value := valid()
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := httptest.NewRecorder()
			h.ServeHTTP(w, callbackRequest(query(st, nil), value))
			c, _ := responseCookie(w, CookieName)
			codes <- fmt.Sprintf("%d %s %v", w.Code, w.Header().Get("Location"), c != nil)
		}()
	}
	wg.Wait()
	close(codes)
	for got := range codes {
		if got != "303 / true" {
			t.Fatalf("concurrent sign-in: %s", got)
		}
	}
	if n := countUsers(); n != 4 {
		t.Fatalf("users: %d", n)
	}

	// Nothing secret was logged.
	for _, secret := range []string{"code-value-1", "id-token-value", "alice@example", "Alice@Example", "Dave@Example", "dave@example", "carol@example", "erin@example", "not an address", st.State, st.Nonce, st.Verifier, cookie.Value, session} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("log contains %q: %s", secret, logs.String())
		}
	}
	for _, reason := range []string{"auth: oidc sign-in failed: state mismatch", "auth: oidc sign-in failed: malformed email claim", "auth: oidc sign-in failed: user carol-id is disabled", "auth: created user " + dave.ID + " by an oidc sign-in"} {
		if !strings.Contains(logs.String(), reason) {
			t.Fatalf("log lacks %q: %s", reason, logs.String())
		}
	}
}

// ownerToken returns an access token of alice, signed in without a
// password.
func ownerToken(t *testing.T, api *usersAPI) string {
	t.Helper()
	alice, err := api.store.UserByEmail(context.Background(), "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	result, err := api.srv.auth.LoginUser(context.Background(), alice)
	if err != nil {
		t.Fatal(err)
	}
	return result.AccessToken
}

func TestOIDCCallbackDatabaseError(t *testing.T) {
	logs := captureLog(t)
	api, rp := newOIDCAPI(t, "sqlite")
	if err := api.store.Close(); err != nil {
		t.Fatal(err)
	}
	rp.set("alice@example.com", nil, nil)
	st, value := newOIDCState(t, api.srv, "/")
	expectSigninError(t, serve(t, api.handler, callbackRequest(url.Values{"code": {"c"}, "state": {st.State}}.Encode(), value)), reasonUnavailable)
	rp.set("new@example.com", nil, nil)
	st, value = newOIDCState(t, api.srv, "/")
	expectSigninError(t, serve(t, api.handler, callbackRequest(url.Values{"code": {"c"}, "state": {st.State}}.Encode(), value)), reasonUnavailable)
	if strings.Contains(logs.String(), "@example.com") {
		t.Fatalf("log contains the address: %s", logs.String())
	}
}

// Without an OIDC client the callback is unavailable instead of panicking.
func TestOIDCCallbackWithoutClient(t *testing.T) {
	api, _ := newOIDCAPI(t, "sqlite")
	api.srv.rp = nil
	st, value := newOIDCState(t, api.srv, "/")
	expectSigninError(t, serve(t, api.handler, callbackRequest(url.Values{"code": {"c"}, "state": {st.State}}.Encode(), value)), reasonUnavailable)
	w := oidcLogin(t, api.handler, "")
	if w.Code != 302 || w.Header().Get("Location") != "/signin-error?reason=unavailable" {
		t.Fatalf("%d %v", w.Code, w.Header())
	}
}

func TestPasswordEndpointsWithOIDC(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) { testPasswordEndpointsWithOIDC(t, driver) })
	}
}

func testPasswordEndpointsWithOIDC(t *testing.T, driver string) {
	// Links issued while passwords were in use.
	password := newUsersAPI(t, driver)
	alice, _ := password.login("alice@example.com", "correct-password-123")
	inv := decodeJSON[createdInvitationResponse](t, password.do("POST", "/api/v1/invitations", alice, `{"email":"dave@example.com","role":"member"}`))
	pwr := decodeJSON[createdPasswordResetResponse](t, password.do("POST", "/api/v1/users/carol-id/password-resets", alice, ""))
	invToken := fragmentToken(t, inv.URL, usersOrigin+"/invite#")
	pwrToken := fragmentToken(t, pwr.URL, usersOrigin+"/reset-password#")

	// The same database served with OIDC.
	srv := newServer(oidcConfig(), password.srv.auth, fstest.MapFS{}, nil)
	srv.rp = newFakeOIDC()
	api := &usersAPI{t: t, store: password.store, srv: srv, handler: srv.handler(), now: password.now}
	ctx := context.Background()
	disabled := func(w *httptest.ResponseRecorder) {
		t.Helper()
		expect(t, w, 409, "password_auth_disabled")
		if body := decodeJSON[map[string]map[string]string](t, w); body["error"]["message"] == "" {
			t.Fatalf("%s", w.Body.String())
		}
		if len(w.Result().Cookies()) != 0 {
			t.Fatal("cookie set")
		}
	}
	// More attempts than the rate limits allow are all 409.
	for range 15 {
		disabled(api.public("/auth/login", `{"email":"carol@example.com","password":"correct-password-123"}`, ""))
		disabled(api.public("/auth/login", `{"email":"carol@example.com","password":"wrong-password-123"}`, ""))
		disabled(api.public("/auth/invitations/inspect", linkBody("/auth/invitations/inspect", invToken), ""))
		disabled(api.public("/auth/invitations/accept", `{"token":"`+invToken+`","password":"dave-password-123"}`, ""))
		disabled(api.public("/auth/password-resets/inspect", linkBody("/auth/password-resets/inspect", pwrToken), ""))
		disabled(api.public("/auth/password-resets/complete", `{"token":"`+pwrToken+`","password":"carol-new-password"}`, ""))
	}
	// Even without the Origin or the content type.
	r := httptest.NewRequest("POST", "/auth/login", strings.NewReader("x"))
	disabled(serve(t, api.handler, r))

	token := ownerToken(t, api)
	disabled(api.do("POST", "/api/v1/invitations", token, `{"email":"erin@example.com","role":"member"}`))
	disabled(api.do("POST", "/api/v1/users/carol-id/password-resets", token, ""))
	// Signing in comes first.
	expect(t, api.do("POST", "/api/v1/invitations", "", `{"email":"erin@example.com","role":"member"}`), 401, "unauthorized")
	expect(t, api.do("POST", "/api/v1/users/carol-id/password-resets", "", ""), 401, "unauthorized")

	// Nothing changed: the links are still active and carol's password is
	// the same.
	if _, err := api.store.InvitationByToken(ctx, auth.Hash(invToken), api.now.Unix()); err != nil {
		t.Fatalf("invitation: %v", err)
	}
	if _, err := api.store.PasswordResetUser(ctx, auth.Hash(pwrToken), api.now.Unix()); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if _, err := api.store.UserByEmail(ctx, "dave@example.com"); err == nil {
		t.Fatal("dave was created")
	}
	if _, err := api.srv.auth.Login(ctx, "carol@example.com", "correct-password-123"); err != nil {
		t.Fatalf("carol's password changed: %v", err)
	}

	// The invitations can still be listed and revoked, and the roles and the
	// disabled flag changed.
	list := decodeJSON[invitationListResponse](t, api.do("GET", "/api/v1/invitations", token, ""))
	if len(list.Invitations) != 1 || list.Invitations[0].ID != inv.ID {
		t.Fatalf("%+v", list)
	}
	expect(t, api.do("PATCH", "/api/v1/users/carol-id", token, `{"role":"owner"}`), 200, "")
	expect(t, api.do("PATCH", "/api/v1/users/carol-id", token, `{"disabled":true}`), 200, "")
	expect(t, api.do("PATCH", "/api/v1/users/carol-id", token, `{"disabled":false}`), 200, "")
	expect(t, api.do("DELETE", "/api/v1/invitations/"+inv.ID, token, ""), 204, "")

	// The 409s consumed no attempts: with passwords again, the login and the
	// links are not rate limited.
	srv.cfg.OIDC = nil
	expect(t, api.public("/auth/login", `{"email":"carol@example.com","password":"wrong-password-123"}`, ""), 401, "unauthorized")
	expect(t, api.public("/auth/invitations/inspect", linkBody("/auth/invitations/inspect", invToken), ""), 404, "invitation_invalid")
}

// With OIDC the routes of the Cloudflare Access sign-in of earlier releases
// do not exist.
func TestOIDCOldRoutes(t *testing.T) {
	api, _ := newOIDCAPI(t, "sqlite")
	for _, p := range []string{"/auth/access/login", "/auth/access/login?return_to=%2F", "/auth/access/callback?code=x&state=y"} {
		w := serve(t, api.handler, httptest.NewRequest("GET", p, nil))
		expect(t, w, 404, "not_found")
		if c, _ := responseCookie(w, oidcCookie); c != nil {
			t.Fatalf("%s: cookie set", p)
		}
	}
}

// A provider whose metadata cannot be read does not start the sign-in and
// leaves no sign-in cookie.
func TestOIDCLoginProviderUnavailable(t *testing.T) {
	api, rp := newOIDCAPI(t, "sqlite")
	rp.setAuthErr(oidc.ErrProviderUnavailable)
	w := oidcLogin(t, api.handler, "?return_to=%2Fsettings")
	if w.Code != 302 || w.Header().Get("Location") != "/signin-error?reason=unavailable" || w.Body.Len() != 0 {
		t.Fatalf("%d %v %q", w.Code, w.Header(), w.Body.String())
	}
	if c, _ := responseCookie(w, oidcCookie); c != nil {
		t.Fatal("cookie set")
	}
	rp.setAuthErr(nil)
	expectOIDCCookie(t, oidcLogin(t, api.handler, ""), oidcCookieMaxAge)
}

// email_verified and the allowed emails are checked after the ID token, on
// every sign-in, before any user is found or created.
func TestOIDCCallbackEmailChecks(t *testing.T) {
	logs := captureLog(t)
	api, rp := newOIDCAPI(t, "sqlite")
	h := api.handler
	ctx := context.Background()
	callback := func() *httptest.ResponseRecorder {
		t.Helper()
		st, value := newOIDCState(t, api.srv, "/")
		return serve(t, h, callbackRequest(url.Values{"code": {"c"}, "state": {st.State}}.Encode(), value))
	}
	countUsers := func() int {
		t.Helper()
		users, err := api.store.ListUsers(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return len(users)
	}
	yes, no := true, false
	cfg := api.srv.cfg.OIDC
	before := countUsers()

	// email_verified required.
	for name, tc := range map[string]struct {
		verified *bool
		ok       bool
	}{"true": {&yes, true}, "false": {&no, false}, "absent": {nil, false}} {
		rp.setVerified(tc.verified)
		for _, email := range []string{"alice@example.com", "new-" + name + "@example.com"} {
			rp.set(email, nil, nil)
			w := callback()
			if tc.ok {
				if w.Code != 303 || w.Header().Get("Location") != "/" {
					t.Fatalf("%s %s: %d %v", name, email, w.Code, w.Header())
				}
				expectRefreshCookie(t, w)
				continue
			}
			expectSigninError(t, w, reasonNotVerified)
		}
	}
	if n := countUsers(); n != before+1 {
		t.Fatalf("users: %d", n)
	}

	// Not required: an absent claim passes, false does not.
	cfg.RequireEmailVerified = false
	rp.set("alice@example.com", nil, nil)
	rp.setVerified(nil)
	expectRefreshCookie(t, callback())
	rp.setVerified(&no)
	expectSigninError(t, callback(), reasonNotVerified)
	rp.setVerified(&yes)
	expectRefreshCookie(t, callback())

	// The allowed emails apply to existing users as well, after
	// email_verified.
	for _, tc := range []struct {
		allowed []string
		email   string
		reason  string
	}{
		{[]string{}, "alice@example.com", reasonNotAllowed},
		{nil, "alice@example.com", reasonNotAllowed},
		{[]string{"example.org"}, "alice@example.com", reasonNotAllowed},
		{[]string{"sub.example.com"}, "alice@example.com", reasonNotAllowed},
		{[]string{"bob@example.com"}, "Alice@Example.com", reasonNotAllowed},
		{[]string{"example.org"}, "frank@example.com", reasonNotAllowed},
		{[]string{"alice@example.com"}, " Alice@Example.COM ", ""},
		{[]string{"example.org", "example.com"}, "alice@example.com", ""},
		{[]string{"*"}, "alice@example.com", ""},
	} {
		cfg.AllowedEmails = tc.allowed
		rp.set(tc.email, nil, nil)
		w := callback()
		if tc.reason != "" {
			expectSigninError(t, w, tc.reason)
			continue
		}
		if w.Code != 303 || w.Header().Get("Location") != "/" {
			t.Fatalf("%+v: %d %v", tc, w.Code, w.Header())
		}
		expectRefreshCookie(t, w)
	}
	if _, err := api.store.UserByEmail(ctx, "frank@example.com"); err == nil {
		t.Fatal("frank was created")
	}
	// email_verified is checked first.
	cfg.AllowedEmails = []string{}
	rp.set("alice@example.com", nil, nil)
	rp.setVerified(&no)
	expectSigninError(t, callback(), reasonNotVerified)
	if n := countUsers(); n != before+1 {
		t.Fatalf("users: %d", n)
	}

	if strings.Contains(strings.ToLower(logs.String()), "@example.com") {
		t.Fatalf("log contains an address: %s", logs.String())
	}
	for _, reason := range []string{"auth: oidc sign-in failed: email not verified", "auth: oidc sign-in failed: email not allowed"} {
		if !strings.Contains(logs.String(), reason) {
			t.Fatalf("log lacks %q: %s", reason, logs.String())
		}
	}
}

// An empty ASTOVIA_ALLOWED_EMAILS is logged once when the server is built.
func TestOIDCAllowedEmailsEmptyLog(t *testing.T) {
	logs := captureLog(t)
	const warning = "oidc: ASTOVIA_ALLOWED_EMAILS is empty; nobody can sign in"
	newServer(oidcConfig(), nil, fstest.MapFS{}, nil)
	newServer(config.Config{PublicURL: usersOrigin}, nil, fstest.MapFS{}, nil)
	if strings.Contains(logs.String(), warning) {
		t.Fatalf("logged: %s", logs.String())
	}
	c := oidcConfig()
	c.OIDC.AllowedEmails = []string{}
	newServer(c, nil, fstest.MapFS{}, nil)
	if strings.Count(logs.String(), warning) != 1 {
		t.Fatalf("log: %s", logs.String())
	}
}
