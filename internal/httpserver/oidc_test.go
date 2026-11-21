package httpserver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
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

// oidcConfig is the configuration of usersOrigin with OIDC. email_verified
// is required.
func oidcConfig() config.Config {
	return config.Config{PublicURL: usersOrigin, Secure: true, Secret: make([]byte, 32), OIDC: &config.OIDCClient{
		Issuer:               testIssuer,
		ClientID:             testOIDCClientID,
		Secret:               "client-secret",
		RedirectURI:          usersOrigin + config.OIDCCallbackPath,
		RequireEmailVerified: true,
	}}
}

// fakeOIDC answers AuthorizationURL with the authorization endpoint of the
// test issuer or authErr, Exchange with idToken or exchangeErr and Verify
// with email, verified, the subject and hd or verifyErr, and records the
// arguments. The subject is subject when set and otherwise derived from the
// address, so that every address is another identity.
type fakeOIDC struct {
	mu          sync.Mutex
	authErr     error
	idToken     string
	exchangeErr error
	email       string
	verified    *bool
	subject     *string
	hd          string
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
	subject := defaultSubject(f.email)
	if f.subject != nil {
		subject = *f.subject
	}
	return oidc.Identity{Email: f.email, EmailVerified: f.verified, Subject: subject, HostedDomain: f.hd}, nil
}

// defaultSubject is the sub of the identity fakeOIDC gives to an address.
func defaultSubject(email string) string {
	return "sub:" + strings.ToLower(strings.TrimSpace(email))
}

func (f *fakeOIDC) set(email string, exchangeErr, verifyErr error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.email, f.exchangeErr, f.verifyErr = email, exchangeErr, verifyErr
}

// setSubject sets the sub claim whatever the address; nil derives it from
// the address again.
func (f *fakeOIDC) setSubject(subject *string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.subject = subject
}

// setHD sets the hd claim: empty is absent.
func (f *fakeOIDC) setHD(hd string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hd = hd
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

	// alice was linked to the identity of the first sign-in; her role and
	// her password hash stayed.
	if u, err := api.store.UserByOIDC(ctx, testIssuer, defaultSubject("alice@example.com")); err != nil || u.ID != "alice-id" || u.Role != store.RoleOwner || u.PasswordHash == store.NoPasswordHash {
		t.Fatalf("%+v %v", u, err)
	}

	// The first sign-in of an address that was not invited creates nobody.
	rp.set(" Dave@Example.COM ", nil, nil)
	st, value = valid()
	expectSigninError(t, serve(t, h, callbackRequest(query(st, nil), value)), reasonNotAllowed)
	if n := countUsers(); n != 2 {
		t.Fatalf("users: %d", n)
	}
	// Once invited, it creates a member that no password matches; the next
	// sign-in finds it.
	expect(t, api.do("POST", "/api/v1/invitations", ownerToken(t, api), `{"email":"dave@example.com","role":"member"}`), 201, "")
	st, value = valid()
	w = serve(t, h, callbackRequest(query(st, nil), value))
	if w.Code != 303 || w.Header().Get("Location") != "/" {
		t.Fatalf("%d %v", w.Code, w.Header())
	}
	daveCookie := expectRefreshCookie(t, w)
	dave, err := api.store.UserByEmail(ctx, "dave@example.com")
	if err != nil || dave.Role != store.RoleMember || dave.Disabled != 0 || dave.PasswordHash != "!access" || dave.CreatedAt != api.now.Unix() ||
		dave.OidcIssuer != (sql.NullString{String: testIssuer, Valid: true}) || dave.OidcSubject != (sql.NullString{String: defaultSubject("dave@example.com"), Valid: true}) {
		t.Fatalf("%+v %v", dave, err)
	}
	if got := refresh(daveCookie).User; got.ID != dave.ID || got.Role != store.RoleMember {
		t.Fatalf("%+v", got)
	}
	if list, err := api.store.ListInvitations(ctx, api.now.Unix()); err != nil || len(list) != 0 {
		t.Fatalf("the invitation is still active: %+v %v", list, err)
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

	// Concurrent first sign-ins of one identity create one user, and all of
	// them sign in.
	expect(t, api.do("POST", "/api/v1/invitations", ownerToken(t, api), `{"email":"erin@example.com","role":"member"}`), 201, "")
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
	for _, secret := range []string{"code-value-1", "id-token-value", "alice@example", "Alice@Example", "Dave@Example", "dave@example", "carol@example", "erin@example", "not an address", "sub:", st.State, st.Nonce, st.Verifier, cookie.Value, session} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("log contains %q: %s", secret, logs.String())
		}
	}
	for _, reason := range []string{"auth: oidc sign-in failed: state mismatch", "auth: oidc sign-in failed: malformed email claim", "auth: oidc sign-in failed: user is disabled", "auth: oidc sign-in failed: not invited", "auth: linked user alice-id to an oidc identity", "auth: created user " + dave.ID + " from an invitation by an oidc sign-in"} {
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
	// disabled flag changed. Creating them has its own test.
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

// oidcSignIn is an OIDC server over a users API with helpers for the first
// sign-in tests.
type oidcSignIn struct {
	t   *testing.T
	api *usersAPI
	rp  *fakeOIDC
	raw *sql.DB
}

func newOIDCSignIn(t *testing.T, driver string) *oidcSignIn {
	t.Helper()
	api, rp := newOIDCAPI(t, driver)
	name := driver
	if driver == "postgres" {
		name = "pgx"
	}
	raw, err := sql.Open(name, api.dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	return &oidcSignIn{t: t, api: api, rp: rp, raw: raw}
}

// callback finishes a new sign-in as email with the sub subject; an empty
// subject is the one derived from the address.
func (o *oidcSignIn) callback(email, subject string) *httptest.ResponseRecorder {
	o.t.Helper()
	o.rp.set(email, nil, nil)
	if subject == "" {
		o.rp.setSubject(nil)
	} else {
		o.rp.setSubject(&subject)
	}
	st, value := newOIDCState(o.t, o.api.srv, "/")
	return serve(o.t, o.api.handler, callbackRequest(url.Values{"code": {"c"}, "state": {st.State}}.Encode(), value))
}

// signedIn checks that w is a successful sign-in and returns its user.
func (o *oidcSignIn) signedIn(w *httptest.ResponseRecorder) userResponse {
	o.t.Helper()
	if w.Code != 303 || w.Header().Get("Location") != "/" {
		o.t.Fatalf("%d %v", w.Code, w.Header())
	}
	r := o.api.public("/auth/refresh", `{}`, expectRefreshCookie(o.t, w))
	expect(o.t, r, 200, "")
	return decodeJSON[authResponse](o.t, r).User
}

// invite registers an invitation as alice and returns its ID.
func (o *oidcSignIn) invite(email, role string) string {
	o.t.Helper()
	w := o.api.do("POST", "/api/v1/invitations", ownerToken(o.t, o.api), `{"email":"`+email+`","role":"`+role+`"}`)
	expect(o.t, w, 201, "")
	return decodeJSON[createdInvitationResponse](o.t, w).ID
}

// exec runs a statement on the database, for states the API cannot make.
func (o *oidcSignIn) exec(query string) {
	o.t.Helper()
	if _, err := o.raw.ExecContext(context.Background(), query); err != nil {
		o.t.Fatal(err)
	}
}

func (o *oidcSignIn) user(email string) store.User {
	o.t.Helper()
	u, err := o.api.store.UserByEmail(context.Background(), email)
	if err != nil {
		o.t.Fatalf("%s: %v", email, err)
	}
	return u
}

func (o *oidcSignIn) noUser(email string) {
	o.t.Helper()
	if _, err := o.api.store.UserByEmail(context.Background(), email); !errors.Is(err, sql.ErrNoRows) {
		o.t.Fatalf("%s exists: %v", email, err)
	}
}

// activeInvitations returns the IDs of the active invitations, newest
// first.
func (o *oidcSignIn) activeInvitations() []string {
	o.t.Helper()
	list, err := o.api.store.ListInvitations(context.Background(), o.api.now.Unix())
	if err != nil {
		o.t.Fatal(err)
	}
	ids := make([]string, 0, len(list))
	for _, inv := range list {
		ids = append(ids, inv.ID)
	}
	return ids
}

// identity returns the identity columns of the user of email.
func (o *oidcSignIn) identity(email string) (string, string) {
	o.t.Helper()
	u := o.user(email)
	if u.OidcIssuer.Valid != u.OidcSubject.Valid {
		o.t.Fatalf("%s: half an identity: %+v", email, u)
	}
	return u.OidcIssuer.String, u.OidcSubject.String
}

func oidcDrivers(t *testing.T, fn func(t *testing.T, o *oidcSignIn)) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) { fn(t, newOIDCSignIn(t, driver)) })
	}
}

// A linked user signs in by iss and sub alone: the email address,
// email_verified, hd and the invitations are not looked at.
func TestOIDCLinkedSignIn(t *testing.T) {
	oidcDrivers(t, func(t *testing.T, o *oidcSignIn) {
		logs := captureLog(t)
		yes, no := true, false
		// The first sign-in links alice, who was created with a password.
		before := o.user("alice@example.com")
		if got := o.signedIn(o.callback("alice@example.com", "subject-alice-1")); got.ID != "alice-id" || got.Role != store.RoleOwner {
			t.Fatalf("%+v", got)
		}
		if issuer, sub := o.identity("alice@example.com"); issuer != testIssuer || sub != "subject-alice-1" {
			t.Fatalf("identity %q %q", issuer, sub)
		}
		if after := o.user("alice@example.com"); after.PasswordHash != before.PasswordHash || after.Role != before.Role {
			t.Fatalf("%+v", after)
		}
		inv := o.invite("dave@example.com", store.RoleOwner)

		// Another address, the address of another user, an invited address
		// and a malformed address all sign alice in.
		for _, email := range []string{"alice@example.com", "renamed@example.org", "carol@example.com", "dave@example.com", "not an address", ""} {
			for name, verified := range map[string]*bool{"true": &yes, "false": &no, "absent": nil} {
				o.rp.setVerified(verified)
				if got := o.signedIn(o.callback(email, "subject-alice-1")); got.ID != "alice-id" || got.Email != "alice@example.com" {
					t.Fatalf("%q email_verified %s: %+v", email, name, got)
				}
			}
		}
		o.rp.setVerified(&yes)
		// Nothing else changed: no user was created or linked, and the
		// invitation is still active.
		o.noUser("renamed@example.org")
		o.noUser("dave@example.com")
		if issuer, sub := o.identity("carol@example.com"); issuer != "" || sub != "" {
			t.Fatalf("carol was linked: %q %q", issuer, sub)
		}
		if ids := o.activeInvitations(); len(ids) != 1 || ids[0] != inv {
			t.Fatalf("invitations: %v", ids)
		}
		if users, err := o.api.store.ListUsers(context.Background()); err != nil || len(users) != 2 {
			t.Fatalf("users: %d %v", len(users), err)
		}

		// The same sub at another issuer is not alice: her address belongs to
		// a user linked to another identity.
		o.api.srv.cfg.OIDC.Issuer = "https://other-idp.example.com"
		expectSigninError(t, o.callback("alice@example.com", "subject-alice-1"), reasonNotAllowed)
		o.api.srv.cfg.OIDC.Issuer = testIssuer
		// Another sub with alice's address is not alice either.
		expectSigninError(t, o.callback("alice@example.com", "subject-mallory"), reasonNotAllowed)
		if issuer, sub := o.identity("alice@example.com"); issuer != testIssuer || sub != "subject-alice-1" {
			t.Fatalf("identity %q %q", issuer, sub)
		}
		// The sub is compared as it is.
		expectSigninError(t, o.callback("alice@example.com", "SUBJECT-ALICE-1"), reasonNotAllowed)
		// An empty sub, which the verifier never returns, fails.
		o.rp.setSubject(new(string))
		st, value := newOIDCState(t, o.api.srv, "/")
		expectSigninError(t, serve(t, o.api.handler, callbackRequest(url.Values{"code": {"c"}, "state": {st.State}}.Encode(), value)), reasonFailed)

		// A linked user that is disabled gets no session.
		expect(t, o.api.do("PATCH", "/api/v1/users/carol-id", ownerToken(t, o.api), `{"role":"owner"}`), 200, "")
		o.signedIn(o.callback("carol@example.com", "subject-carol"))
		carol, err := o.api.srv.auth.LoginUser(context.Background(), o.user("carol@example.com"))
		if err != nil {
			t.Fatal(err)
		}
		expect(t, o.api.do("PATCH", "/api/v1/users/alice-id", carol.AccessToken, `{"disabled":true}`), 200, "")
		expectSigninError(t, o.callback("alice@example.com", "subject-alice-1"), reasonDisabled)
		if u := o.user("alice@example.com"); u.Disabled == 0 {
			t.Fatal("alice was enabled")
		}

		for _, secret := range []string{"subject-alice", "subject-mallory", "subject-carol", "SUBJECT-ALICE", "@example"} {
			if strings.Contains(logs.String(), secret) {
				t.Fatalf("log contains %q: %s", secret, logs.String())
			}
		}
		for _, line := range []string{"auth: linked user alice-id to an oidc identity", "auth: oidc sign-in failed: email belongs to a user linked to another identity", "auth: oidc sign-in failed: empty issuer or sub", "auth: oidc sign-in failed: user alice-id is disabled"} {
			if !strings.Contains(logs.String(), line) {
				t.Fatalf("log lacks %q: %s", line, logs.String())
			}
		}
	})
}

// The first sign-in of an identity needs an active invitation to its
// verified address, or a user of that address that is not linked yet.
func TestOIDCFirstSignIn(t *testing.T) {
	oidcDrivers(t, func(t *testing.T, o *oidcSignIn) {
		logs := captureLog(t)
		ctx := context.Background()
		users := func() int {
			t.Helper()
			list, err := o.api.store.ListUsers(ctx)
			if err != nil {
				t.Fatal(err)
			}
			return len(list)
		}

		// An invitation creates the user with its role.
		for _, tc := range []struct{ name, role string }{{"dave", store.RoleMember}, {"erin", store.RoleOwner}} {
			email := tc.name + "@example.com"
			o.invite(email, tc.role)
			got := o.signedIn(o.callback(" "+strings.ToUpper(email)+" ", "subject-"+tc.name))
			u := o.user(email)
			if got.ID != u.ID || got.Email != email || u.Role != tc.role || u.Disabled != 0 || u.PasswordHash != store.NoPasswordHash || u.CreatedAt != o.api.now.Unix() {
				t.Fatalf("%s: %+v %+v", tc.name, got, u)
			}
			if issuer, sub := o.identity(email); issuer != testIssuer || sub != "subject-"+tc.name {
				t.Fatalf("%s: identity %q %q", tc.name, issuer, sub)
			}
			if ids := o.activeInvitations(); len(ids) != 0 {
				t.Fatalf("%s: the invitation is still active: %v", tc.name, ids)
			}
			// The next sign-in finds the user without an invitation.
			if again := o.signedIn(o.callback(email, "subject-"+tc.name)); again.ID != u.ID {
				t.Fatalf("%s again: %+v", tc.name, again)
			}
		}
		if n := users(); n != 4 {
			t.Fatalf("users: %d", n)
		}

		// Without an invitation, or with one that is revoked, expired or
		// already used, nobody is created.
		expectSigninError(t, o.callback("frank@example.com", ""), reasonNotAllowed)
		revoked := o.invite("frank@example.com", store.RoleMember)
		expect(t, o.api.do("DELETE", "/api/v1/invitations/"+revoked, ownerToken(t, o.api), ""), 204, "")
		expectSigninError(t, o.callback("frank@example.com", ""), reasonNotAllowed)
		used := o.invite("frank@example.com", store.RoleMember)
		o.exec("UPDATE invitations SET accepted_at = 1 WHERE id = '" + used + "'")
		expectSigninError(t, o.callback("frank@example.com", ""), reasonNotAllowed)
		// An invitation whose role is not a role is not used either.
		o.exec("UPDATE invitations SET accepted_at = NULL, role = 'admin' WHERE id = '" + used + "'")
		expectSigninError(t, o.callback("frank@example.com", ""), reasonNotAllowed)
		// expires_at == now is expired.
		o.invite("frank@example.com", store.RoleMember)
		start := *o.api.now
		*o.api.now = start.Add(store.InvitationLifetime * time.Second)
		expectSigninError(t, o.callback("frank@example.com", ""), reasonNotAllowed)
		o.noUser("frank@example.com")
		if n := users(); n != 4 {
			t.Fatalf("users: %d", n)
		}
		// One second earlier it is still active.
		*o.api.now = start.Add((store.InvitationLifetime - 1) * time.Second)
		if got := o.signedIn(o.callback("frank@example.com", "")); got.Email != "frank@example.com" {
			t.Fatalf("%+v", got)
		}
		*o.api.now = start

		// Of two active invitations to one address the newest decides and the
		// other is revoked. The API never leaves two, so they are inserted.
		now := o.api.now.Unix()
		for _, inv := range []struct {
			id, role string
			at       int64
		}{{"gina-old", store.RoleOwner, now - 10}, {"gina-new", store.RoleMember, now - 5}} {
			o.exec(fmt.Sprintf("INSERT INTO invitations (id,token_hash,email,role,invited_by,created_at,expires_at) VALUES ('%s','hash-%s','gina@example.com','%s','alice-id',%d,%d)", inv.id, inv.id, inv.role, inv.at, now+1000))
		}
		o.signedIn(o.callback("gina@example.com", ""))
		if u := o.user("gina@example.com"); u.Role != store.RoleMember {
			t.Fatalf("gina: %+v", u)
		}
		var oldRevoked, newAccepted bool
		if err := o.raw.QueryRowContext(ctx, "SELECT (SELECT revoked_at IS NOT NULL AND accepted_at IS NULL FROM invitations WHERE id = 'gina-old'), (SELECT accepted_at IS NOT NULL AND revoked_at IS NULL FROM invitations WHERE id = 'gina-new')").Scan(&oldRevoked, &newAccepted); err != nil || !oldRevoked || !newAccepted {
			t.Fatalf("gina's invitations: %v %v %v", oldRevoked, newAccepted, err)
		}

		// Users that are not linked yet are linked without an invitation,
		// with or without a password; the role and the hash stay.
		if err := o.api.store.CreateUser(ctx, store.User{ID: "hana-id", Email: "hana@example.com", PasswordHash: store.NoPasswordHash, CreatedAt: 3, Role: store.RoleOwner}); err != nil {
			t.Fatal(err)
		}
		for _, email := range []string{"carol@example.com", "hana@example.com"} {
			before := o.user(email)
			if got := o.signedIn(o.callback(email, "")); got.ID != before.ID {
				t.Fatalf("%s: %+v", email, got)
			}
			after := o.user(email)
			if after.Role != before.Role || after.PasswordHash != before.PasswordHash || after.CreatedAt != before.CreatedAt {
				t.Fatalf("%s: %+v", email, after)
			}
			if issuer, sub := o.identity(email); issuer != testIssuer || sub != defaultSubject(email) {
				t.Fatalf("%s: identity %q %q", email, issuer, sub)
			}
		}

		// An unlinked user that is disabled is not linked.
		expect(t, o.api.do("PATCH", "/api/v1/users/"+o.user("carol@example.com").ID, ownerToken(t, o.api), `{"disabled":true}`), 200, "")
		if err := o.api.store.UnlinkOIDC(ctx, "carol@example.com"); err != nil {
			t.Fatal(err)
		}
		expectSigninError(t, o.callback("carol@example.com", ""), reasonDisabled)
		if issuer, sub := o.identity("carol@example.com"); issuer != "" || sub != "" {
			t.Fatalf("carol was linked: %q %q", issuer, sub)
		}

		if strings.Contains(strings.ToLower(logs.String()), "@example") || strings.Contains(logs.String(), "subject-") || strings.Contains(logs.String(), "sub:") {
			t.Fatalf("log contains an address or a sub: %s", logs.String())
		}
		for _, line := range []string{"auth: oidc sign-in failed: not invited", "auth: oidc sign-in failed: user is disabled", "auth: created user " + o.user("dave@example.com").ID + " from an invitation by an oidc sign-in", "auth: linked user hana-id to an oidc identity"} {
			if !strings.Contains(logs.String(), line) {
				t.Fatalf("log lacks %q: %s", line, logs.String())
			}
		}
	})
}

// email_verified is checked at the first sign-in of an identity, before a
// user is created or linked and before an invitation is used.
func TestOIDCFirstSignInEmailVerified(t *testing.T) {
	oidcDrivers(t, func(t *testing.T, o *oidcSignIn) {
		logs := captureLog(t)
		yes, no := true, false
		inv := o.invite("dave@example.com", store.RoleMember)
		unchanged := func() {
			t.Helper()
			o.noUser("dave@example.com")
			if ids := o.activeInvitations(); len(ids) != 1 || ids[0] != inv {
				t.Fatalf("invitations: %v", ids)
			}
			if issuer, sub := o.identity("carol@example.com"); issuer != "" || sub != "" {
				t.Fatalf("carol was linked: %q %q", issuer, sub)
			}
		}
		// Required: false and absent are rejected, for an invited address
		// and for an unlinked user.
		for _, verified := range []*bool{&no, nil} {
			o.rp.setVerified(verified)
			for _, email := range []string{"dave@example.com", "carol@example.com"} {
				expectSigninError(t, o.callback(email, ""), reasonNotVerified)
			}
			unchanged()
		}
		// Not required: absent passes, false does not.
		o.api.srv.cfg.OIDC.RequireEmailVerified = false
		o.rp.setVerified(&no)
		for _, email := range []string{"dave@example.com", "carol@example.com"} {
			expectSigninError(t, o.callback(email, ""), reasonNotVerified)
		}
		unchanged()
		o.rp.setVerified(nil)
		if got := o.signedIn(o.callback("dave@example.com", "")); got.Email != "dave@example.com" {
			t.Fatalf("%+v", got)
		}
		if got := o.signedIn(o.callback("carol@example.com", "")); got.ID != "carol-id" {
			t.Fatalf("%+v", got)
		}
		// Required again: true passes.
		o.api.srv.cfg.OIDC.RequireEmailVerified = true
		o.rp.setVerified(&yes)
		o.invite("erin@example.com", store.RoleMember)
		o.signedIn(o.callback("erin@example.com", ""))

		if strings.Contains(strings.ToLower(logs.String()), "@example.com") {
			t.Fatalf("log contains an address: %s", logs.String())
		}
		if !strings.Contains(logs.String(), "auth: oidc sign-in failed: email not verified") {
			t.Fatalf("log: %s", logs.String())
		}
	})
}

// With Google as the issuer, the first sign-in of an address outside
// gmail.com needs the hd claim of a Google Workspace account.
func TestOIDCFirstSignInGoogle(t *testing.T) {
	oidcDrivers(t, func(t *testing.T, o *oidcSignIn) {
		logs := captureLog(t)
		o.api.srv.cfg.OIDC.Issuer = "https://accounts.google.com"
		for _, email := range []string{"dave@example.com", "erin@gmail.com", "frank@googlemail.com", "gina@example.org", "hana@sub.gmail.com"} {
			o.invite(email, store.RoleMember)
		}
		invited := len(o.activeInvitations())
		// Without hd: only gmail.com passes. An unlinked user is held to the
		// same rule as an invited address.
		for _, email := range []string{"dave@example.com", "frank@googlemail.com", "hana@sub.gmail.com", "carol@example.com"} {
			expectSigninError(t, o.callback(email, ""), reasonUnsupported)
		}
		o.noUser("dave@example.com")
		o.noUser("frank@googlemail.com")
		if issuer, sub := o.identity("carol@example.com"); issuer != "" || sub != "" {
			t.Fatalf("carol was linked: %q %q", issuer, sub)
		}
		if n := len(o.activeInvitations()); n != invited {
			t.Fatalf("invitations: %d, want %d", n, invited)
		}
		if got := o.signedIn(o.callback("Erin@Gmail.com", "")); got.Email != "erin@gmail.com" {
			t.Fatalf("%+v", got)
		}
		// With hd they pass; its value is not compared with the address.
		o.rp.setHD("example.com")
		for _, email := range []string{"dave@example.com", "gina@example.org", "carol@example.com"} {
			if got := o.signedIn(o.callback(email, "")); got.Email != email {
				t.Fatalf("%s: %+v", email, got)
			}
		}
		if issuer, sub := o.identity("dave@example.com"); issuer != "https://accounts.google.com" || sub != defaultSubject("dave@example.com") {
			t.Fatalf("identity %q %q", issuer, sub)
		}
		// A linked user signs in without hd.
		o.rp.setHD("")
		o.signedIn(o.callback("dave@example.com", ""))

		// Any other issuer needs no hd, whatever it looks like.
		for i, issuer := range []string{testIssuer, "https://accounts.google.com/", "https://accounts.google.com.example.com"} {
			o.api.srv.cfg.OIDC.Issuer = issuer
			email := fmt.Sprintf("other%d@example.com", i)
			o.invite(email, store.RoleMember)
			if got := o.signedIn(o.callback(email, "")); got.Email != email {
				t.Fatalf("%s: %+v", issuer, got)
			}
		}
		if !strings.Contains(logs.String(), "auth: oidc sign-in failed: google account without hd") || strings.Contains(strings.ToLower(logs.String()), "gmail") || strings.Contains(strings.ToLower(logs.String()), "@example") {
			t.Fatalf("log: %s", logs.String())
		}
	})
}

// Concurrent first sign-ins of several identities with one address: one
// gets the address and the others are not allowed.
func TestOIDCFirstSignInConcurrency(t *testing.T) {
	oidcDrivers(t, func(t *testing.T, o *oidcSignIn) {
		ctx := context.Background()
		const concurrent = 8
		// race finishes one sign-in per subject at the same time, all with
		// email, and returns how many signed in and how many were not
		// allowed. The fake provider answers one identity at a time, so each
		// request has its own.
		race := func(email string, subjects []string) (int, int) {
			t.Helper()
			var wg sync.WaitGroup
			results := make(chan string, len(subjects))
			for _, subject := range subjects {
				srv := newServer(oidcConfig(), o.api.srv.auth, fstest.MapFS{}, nil)
				rp := newFakeOIDC()
				rp.set(email, nil, nil)
				rp.setSubject(&subject)
				srv.rp = rp
				st, value := newOIDCState(t, srv, "/")
				h := srv.handler()
				wg.Add(1)
				go func() {
					defer wg.Done()
					w := httptest.NewRecorder()
					h.ServeHTTP(w, callbackRequest(url.Values{"code": {"c"}, "state": {st.State}}.Encode(), value))
					c, _ := responseCookie(w, CookieName)
					results <- fmt.Sprintf("%d %s %v", w.Code, w.Header().Get("Location"), c != nil)
				}()
			}
			wg.Wait()
			close(results)
			ok, notAllowed := 0, 0
			for got := range results {
				switch got {
				case "303 / true":
					ok++
				case "303 /signin-error?reason=not_allowed false":
					notAllowed++
				default:
					t.Fatalf("%s: %s", email, got)
				}
			}
			return ok, notAllowed
		}
		subjects := func(prefix string, same bool) []string {
			out := make([]string, concurrent)
			for i := range out {
				out[i] = prefix
				if !same {
					out[i] = fmt.Sprintf("%s-%d", prefix, i)
				}
			}
			return out
		}
		linkedSubjects := func(email string, candidates []string) int {
			t.Helper()
			_, sub := o.identity(email)
			n := 0
			for _, s := range candidates {
				if s == sub {
					n++
				}
			}
			return n
		}

		// One identity, one invitation: everyone signs in as the one user.
		o.invite("dave@example.com", store.RoleMember)
		if ok, notAllowed := race("dave@example.com", subjects("subject-dave", true)); ok != concurrent || notAllowed != 0 {
			t.Fatalf("same identity: %d signed in, %d not allowed", ok, notAllowed)
		}
		// Several identities, one invitation.
		o.invite("erin@example.com", store.RoleMember)
		erin := subjects("subject-erin", false)
		if ok, notAllowed := race("erin@example.com", erin); ok != 1 || notAllowed != concurrent-1 {
			t.Fatalf("invitation: %d signed in, %d not allowed", ok, notAllowed)
		}
		if linkedSubjects("erin@example.com", erin) != 1 {
			t.Fatal("erin is linked to none of the identities")
		}
		// Several identities, one unlinked user.
		carol := subjects("subject-carol", false)
		if ok, notAllowed := race("carol@example.com", carol); ok != 1 || notAllowed != concurrent-1 {
			t.Fatalf("unlinked user: %d signed in, %d not allowed", ok, notAllowed)
		}
		if linkedSubjects("carol@example.com", carol) != 1 {
			t.Fatal("carol is linked to none of the identities")
		}
		if users, err := o.api.store.ListUsers(ctx); err != nil || len(users) != 4 {
			t.Fatalf("users: %d %v", len(users), err)
		}
	})
}

// After store.ErrConflict the identity is read once: its user signs in, a
// disabled one does not, and without a user another identity took the
// address.
func TestOIDCUserAfterConflict(t *testing.T) {
	oidcDrivers(t, func(t *testing.T, o *oidcSignIn) {
		logs := captureLog(t)
		ctx := context.Background()
		o.signedIn(o.callback("alice@example.com", "subject-alice"))
		o.signedIn(o.callback("carol@example.com", "subject-carol"))
		expect(t, o.api.do("PATCH", "/api/v1/users/carol-id", ownerToken(t, o.api), `{"disabled":true}`), 200, "")

		if u, reason, _ := o.api.srv.oidcUserAfterConflict(ctx, testIssuer, "subject-alice"); reason != "" || u.ID != "alice-id" {
			t.Fatalf("enabled: %+v %q", u, reason)
		}
		if u, reason, detail := o.api.srv.oidcUserAfterConflict(ctx, testIssuer, "subject-carol"); reason != reasonDisabled || u.ID != "" || detail != "user carol-id is disabled" {
			t.Fatalf("disabled: %+v %q %q", u, reason, detail)
		}
		if u, reason, _ := o.api.srv.oidcUserAfterConflict(ctx, testIssuer, "subject-nobody"); reason != reasonNotAllowed || u.ID != "" {
			t.Fatalf("no user: %+v %q", u, reason)
		}
		if err := o.api.store.Close(); err != nil {
			t.Fatal(err)
		}
		if u, reason, detail := o.api.srv.oidcUserAfterConflict(ctx, testIssuer, "subject-alice"); reason != reasonUnavailable || u.ID != "" || strings.Contains(detail, "subject") {
			t.Fatalf("database error: %+v %q %q", u, reason, detail)
		}
		if strings.Contains(logs.String(), "subject-") {
			t.Fatalf("log contains a sub: %s", logs.String())
		}
	})
}

// An invitation issued while passwords were in use admits its address at
// the first OIDC sign-in.
func TestOIDCPasswordModeInvitation(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			password := newUsersAPI(t, driver)
			alice, _ := password.login("alice@example.com", "correct-password-123")
			w := password.do("POST", "/api/v1/invitations", alice, `{"email":"dave@example.com","role":"owner"}`)
			expect(t, w, 201, "")
			inv := decodeJSON[createdInvitationResponse](t, w)
			token := fragmentToken(t, inv.URL, usersOrigin+"/invite#")

			srv := newServer(oidcConfig(), password.srv.auth, fstest.MapFS{}, nil)
			rp := newFakeOIDC()
			srv.rp = rp
			o := &oidcSignIn{t: t, api: &usersAPI{t: t, store: password.store, srv: srv, handler: srv.handler(), now: password.now}, rp: rp}
			if got := o.signedIn(o.callback("dave@example.com", "")); got.Email != "dave@example.com" || got.Role != store.RoleOwner {
				t.Fatalf("%+v", got)
			}
			if u := o.user("dave@example.com"); u.PasswordHash != store.NoPasswordHash {
				t.Fatalf("%+v", u)
			}
			// The link is used up: back on passwords it creates nobody.
			expect(t, password.public("/auth/invitations/accept", `{"token":"`+token+`","password":"dave-password-123"}`, ""), 404, "invitation_invalid")
		})
	}
}

// With OIDC an invitation is registered without a link.
func TestOIDCInvitationsAPI(t *testing.T) {
	oidcDrivers(t, func(t *testing.T, o *oidcSignIn) {
		api := o.api
		owner := ownerToken(t, api)
		carol, err := api.srv.auth.LoginUser(context.Background(), o.user("carol@example.com"))
		if err != nil {
			t.Fatal(err)
		}
		body := `{"email":" Dave@Example.com ","role":"member"}`
		expect(t, api.do("POST", "/api/v1/invitations", "", body), 401, "unauthorized")
		expect(t, api.do("POST", "/api/v1/invitations", carol.AccessToken, body), 403, "forbidden")
		expect(t, api.do("POST", "/api/v1/invitations", owner, `{"email":"dave","role":"member"}`), 400, "invalid_request")
		expect(t, api.do("POST", "/api/v1/invitations", owner, `{"email":"carol@example.com","role":"member"}`), 409, "email_taken")
		if ids := o.activeInvitations(); len(ids) != 0 {
			t.Fatalf("invitations: %v", ids)
		}

		w := api.do("POST", "/api/v1/invitations", owner, body)
		expect(t, w, 201, "")
		raw, keys := jsonKeys(t, w.Body.Bytes())
		if strings.Join(keys, ",") != "created_at,email,expires_at,id,role" || string(raw["email"]) != `"dave@example.com"` || string(raw["role"]) != `"member"` {
			t.Fatalf("%s", w.Body.String())
		}
		if strings.Contains(w.Body.String(), "/invite") {
			t.Fatalf("the response has a link: %s", w.Body.String())
		}
		first := decodeJSON[createdInvitationResponse](t, w)
		if first.ExpiresAt != formatTime(api.now.Unix()+store.InvitationLifetime) {
			t.Fatalf("%+v", first)
		}
		// Inviting the address again replaces the invitation.
		second := o.invite("dave@example.com", store.RoleOwner)
		if ids := o.activeInvitations(); len(ids) != 1 || ids[0] != second || second == first.ID {
			t.Fatalf("invitations: %v", ids)
		}
		// The list and the revocation work as with passwords.
		list := decodeJSON[invitationListResponse](t, api.do("GET", "/api/v1/invitations", owner, ""))
		if len(list.Invitations) != 1 || list.Invitations[0].ID != second || list.Invitations[0].Email != "dave@example.com" || list.Invitations[0].Role != store.RoleOwner {
			t.Fatalf("%+v", list)
		}
		expect(t, api.do("DELETE", "/api/v1/invitations/"+second, owner, ""), 204, "")
		expectSigninError(t, o.callback("dave@example.com", ""), reasonNotAllowed)
		// The invitation links stay off.
		expect(t, api.public("/auth/invitations/accept", `{"token":"`+strings.Repeat("a", 43)+`","password":"dave-password-123"}`, ""), 409, "password_auth_disabled")
		expect(t, api.public("/auth/invitations/inspect", `{"token":"`+strings.Repeat("a", 43)+`"}`, ""), 409, "password_auth_disabled")
	})
}
