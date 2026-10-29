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

	"github.com/i2bskn/astovia/internal/access"
	"github.com/i2bskn/astovia/internal/auth"
	"github.com/i2bskn/astovia/internal/config"
	"github.com/i2bskn/astovia/internal/secretbox"
	"github.com/i2bskn/astovia/internal/store"
	"golang.org/x/crypto/bcrypt"
)

const (
	testTeamDomain     = "https://example.cloudflareaccess.com"
	testAccessClientID = "client-id"
)

// accessConfig is the configuration of usersOrigin with Cloudflare Access.
func accessConfig() config.Config {
	return config.Config{PublicURL: usersOrigin, Secure: true, Secret: make([]byte, 32), Access: &config.AccessClient{
		TeamDomain:  testTeamDomain,
		ClientID:    testAccessClientID,
		Secret:      "client-secret",
		Issuer:      testTeamDomain + "/cdn-cgi/access/sso/oidc/" + testAccessClientID,
		RedirectURI: usersOrigin + config.AccessCallbackPath,
	}}
}

// fakeOIDC answers Exchange with idToken or exchangeErr and Verify with email
// or verifyErr, and records the arguments. The authorization URL is the one
// of the real client.
type fakeOIDC struct {
	mu          sync.Mutex
	idToken     string
	exchangeErr error
	email       string
	verifyErr   error

	code, verifier string
	token, nonce   string
	exchanges      int
	verifies       int
}

func newFakeOIDC() *fakeOIDC {
	return &fakeOIDC{idToken: "id-token-value", email: "alice@example.com"}
}

func (f *fakeOIDC) AuthorizationURL(state, nonce, challenge string) string {
	a := accessConfig().Access
	return access.New(access.ClientConfig{Issuer: a.Issuer, ClientID: a.ClientID, ClientSecret: a.Secret, RedirectURI: a.RedirectURI}).AuthorizationURL(state, nonce, challenge)
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

func (f *fakeOIDC) Verify(_ context.Context, idToken, nonce string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.verifies++
	f.token, f.nonce = idToken, nonce
	if f.verifyErr != nil {
		return "", f.verifyErr
	}
	return f.email, nil
}

func (f *fakeOIDC) set(email string, exchangeErr, verifyErr error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.email, f.exchangeErr, f.verifyErr = email, exchangeErr, verifyErr
}

func (f *fakeOIDC) exchangeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.exchanges
}

// newAccessAPI is newUsersAPI (alice the owner, carol the member) served
// with Cloudflare Access and the fake OIDC client. The rate limits are the
// real ones.
func newAccessAPI(t *testing.T, driver string) (*usersAPI, *fakeOIDC) {
	t.Helper()
	api := newUsersAPI(t, driver)
	srv := newServer(accessConfig(), api.srv.auth, fstest.MapFS{}, nil)
	if _, ok := srv.accessOIDC.(*access.Client); !ok {
		t.Fatalf("no Access client: %T", srv.accessOIDC)
	}
	oidc := newFakeOIDC()
	srv.accessOIDC = oidc
	api.srv, api.handler = srv, srv.handler()
	return api, oidc
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

// expectAccessCookie checks the attributes of the sign-in cookie set by w
// and returns it.
func expectAccessCookie(t *testing.T, w *httptest.ResponseRecorder, maxAge int) *http.Cookie {
	t.Helper()
	c, line := responseCookie(w, accessCookie)
	if c == nil {
		t.Fatalf("no %s cookie: %v", accessCookie, w.Header().Values("Set-Cookie"))
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

// accessLogin starts a sign-in with the raw query.
func accessLogin(t *testing.T, h http.Handler, query string) *httptest.ResponseRecorder {
	t.Helper()
	return serve(t, h, httptest.NewRequest("GET", "/auth/access/login"+query, nil))
}

// newAccessState returns a sign-in state returning to returnTo and its
// cookie value, sealed by srv.
func newAccessState(t *testing.T, srv *Server, returnTo string) (accessState, string) {
	t.Helper()
	st := accessState{ReturnTo: returnTo}
	for _, p := range []*string{&st.State, &st.Nonce, &st.Verifier} {
		v, err := auth.Random()
		if err != nil {
			t.Fatal(err)
		}
		*p = v
	}
	value, err := srv.sealAccessState(st)
	if err != nil {
		t.Fatal(err)
	}
	return st, value
}

// callbackRequest returns the callback request with query and the sign-in
// cookie value, if any.
func callbackRequest(query, cookie string) *http.Request {
	r := httptest.NewRequest("GET", "/auth/access/callback?"+query, nil)
	if cookie != "" {
		r.AddCookie(&http.Cookie{Name: accessCookie, Value: cookie})
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
	expectAccessCookie(t, w, -1)
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
		{accessConfig(), `{"mode":"cloudflare_access","logout_url":"https://example.cloudflareaccess.com/cdn-cgi/access/logout"}`},
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

func TestAccessRoutesWithoutAccess(t *testing.T) {
	api := newUsersAPI(t, "sqlite")
	if api.srv.accessOIDC != nil {
		t.Fatal("OIDC client without Access")
	}
	for _, p := range []string{"/auth/access/login", "/auth/access/login?return_to=%2F", "/auth/access/callback?code=x&state=y"} {
		w := serve(t, api.handler, httptest.NewRequest("GET", p, nil))
		expect(t, w, 404, "not_found")
		if c, _ := responseCookie(w, accessCookie); c != nil {
			t.Fatalf("%s: cookie set", p)
		}
	}
}

func TestAccessLogin(t *testing.T) {
	api, oidc := newAccessAPI(t, "sqlite")
	h := api.handler

	returnTo := "/consent?request=abc_DEF-123"
	w := accessLogin(t, h, "?return_to="+url.QueryEscape(returnTo))
	if w.Code != 302 || w.Header().Get("Cache-Control") != "no-store" || w.Body.Len() != 0 {
		t.Fatalf("%d %v %q", w.Code, w.Header(), w.Body.String())
	}
	c := expectAccessCookie(t, w, accessCookieMaxAge)
	// The cookie is sealed: the state is not readable from it.
	if !strings.HasPrefix(c.Value, "v1.") || strings.Contains(c.Value, "consent") {
		t.Fatalf("cookie %q", c.Value)
	}
	st, ok := api.srv.openAccessState(c.Value)
	if !ok || st.ReturnTo != returnTo {
		t.Fatalf("state %+v %v", st, ok)
	}
	loc, err := url.Parse(w.Header().Get("Location"))
	if err != nil || loc.Scheme+"://"+loc.Host+loc.Path != testTeamDomain+"/cdn-cgi/access/sso/oidc/"+testAccessClientID+"/authorization" {
		t.Fatalf("location %q", w.Header().Get("Location"))
	}
	q := loc.Query()
	if q.Get("state") != st.State || q.Get("nonce") != st.Nonce || q.Get("code_challenge_method") != "S256" ||
		q.Get("response_type") != "code" || q.Get("scope") != "openid email" || q.Get("client_id") != testAccessClientID || q.Get("redirect_uri") != usersOrigin+"/auth/access/callback" {
		t.Fatalf("query %v", q)
	}
	// S256 of the verifier, as in RFC 7636; the verifier itself never
	// leaves the cookie.
	sum := sha256.Sum256([]byte(st.Verifier))
	if q.Get("code_challenge") != base64.RawURLEncoding.EncodeToString(sum[:]) || strings.Contains(w.Header().Get("Location"), st.Verifier) {
		t.Fatal("code_challenge")
	}
	// Every sign-in has new values.
	other, _ := api.srv.openAccessState(expectAccessCookie(t, accessLogin(t, h, ""), accessCookieMaxAge).Value)
	if other.State == st.State || other.Nonce == st.Nonce || other.Verifier == st.Verifier || other.ReturnTo != "/" {
		t.Fatalf("%+v", other)
	}

	// Kept return paths, including HTML characters, which are not escaped.
	for _, v := range []string{"/", "/candidates", "/auto/", "/settings", "/settings/notifications", "/account", "/account/users", "/tasks/abc_DEF-1", "/consent", "/integrations/google/callback", "/?x=1", "/candidates?q=<a&b>\"'", "/consent?request=" + strings.Repeat("x", 2500)} {
		w := accessLogin(t, h, "?return_to="+url.QueryEscape(v))
		st, ok := api.srv.openAccessState(expectAccessCookie(t, w, accessCookieMaxAge).Value)
		if w.Code != 302 || !ok || st.ReturnTo != v {
			t.Fatalf("%q: %d %+v", v, w.Code, st)
		}
	}
	// Other sites and other paths fall back to /.
	for _, v := range []string{"//evil.test", "/\\evil.test", "https://evil.test/", "http:/candidates", "/unknown", "candidates", "", "/candidates\x01", "/candidates\t", "/candidates x", "/candidates\x7f", "/candidates?q=é",
		"/%2F%2Fevil.test", "/tasks/..%2F..%2Fx", "/tasks/../settings", "/signin-error", "/signin-error/", "/signin-error?reason=failed", "/candidates#x", "/candidates#", "/api/v1/me", "/auth/access/login", "/auth/refresh", "/mcp", "/tasks/a.b"} {
		w := accessLogin(t, h, "?return_to="+url.QueryEscape(v))
		st, ok := api.srv.openAccessState(expectAccessCookie(t, w, accessCookieMaxAge).Value)
		if w.Code != 302 || !ok || st.ReturnTo != "/" {
			t.Fatalf("%q: %d %+v", v, w.Code, st)
		}
	}
	for _, query := range []string{"", "?return_to=", "?return_to=%2Fsettings&return_to=%2Faccount", "?return_to=%2Fsettings&return_to=" + strings.Repeat("x", 3000)} {
		st, _ := api.srv.openAccessState(expectAccessCookie(t, accessLogin(t, h, query), accessCookieMaxAge).Value)
		if st.ReturnTo != "/" {
			t.Fatalf("%q: %+v", query, st)
		}
	}

	if w := serve(t, h, httptest.NewRequest("POST", "/auth/access/login", nil)); w.Code != 405 || errorCode(t, w) != "method_not_allowed" || w.Header().Get("Allow") != "GET" {
		t.Fatalf("POST: %d %v", w.Code, w.Header())
	}
	if oidc.exchangeCount() != 0 {
		t.Fatal("login called the provider")
	}
}

// The longest return path that fits starts the sign-in with a cookie of less
// than 4096 bytes; a longer one does not start it.
func TestAccessLoginReturnToLength(t *testing.T) {
	api, _ := newAccessAPI(t, "sqlite")
	h := api.handler
	tooLong := func(w *httptest.ResponseRecorder) bool {
		t.Helper()
		if w.Code != 302 || w.Header().Get("Location") != "/signin-error?reason=return_too_long" {
			return false
		}
		if c, _ := responseCookie(w, accessCookie); c != nil {
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
		w := accessLogin(t, h, "?return_to="+url.QueryEscape(v))
		if tooLong(w) {
			continue
		}
		c, line := responseCookie(w, accessCookie)
		st, ok := api.srv.openAccessState(c.Value)
		if w.Code != 302 || !ok || st.ReturnTo != v {
			t.Fatalf("%d bytes: %d %+v", n, w.Code, st)
		}
		pair, _, _ := strings.Cut(line, ";")
		if len(c.Value) > maxAccessCookieValue || len(pair) >= 4096 {
			t.Fatalf("%d bytes: cookie of %d bytes", n, len(pair))
		}
		longest = v
		break
	}
	if len(longest) < 1000 {
		t.Fatalf("longest return_to: %d bytes", len(longest))
	}
	// The same characters, one byte more, do not fit.
	if !tooLong(accessLogin(t, h, "?return_to="+url.QueryEscape((prefix + filler)[:len(longest)+1]))) {
		t.Fatalf("%d bytes fit", len(longest)+1)
	}

	// 2700 bytes of ASCII pass the length check but not the cookie size;
	// longer ones fail the length check.
	for _, n := range []int{2700, 2701, 10000} {
		v := prefix + strings.Repeat("x", n-len(prefix))
		if !tooLong(accessLogin(t, h, "?return_to="+url.QueryEscape(v))) {
			t.Fatalf("%d bytes fit", n)
		}
	}
}

func TestAccessCallback(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) { testAccessCallback(t, driver) })
	}
}

func testAccessCallback(t *testing.T, driver string) {
	logs := captureLog(t)
	api, oidc := newAccessAPI(t, driver)
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

	// The full flow: login, then the callback from Cloudflare. The address
	// matches alice regardless of case, and alice stays an owner.
	returnTo := "/consent?request=abc"
	w := accessLogin(t, h, "?return_to="+url.QueryEscape(returnTo))
	cookie := expectAccessCookie(t, w, accessCookieMaxAge)
	loc, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	st, _ := api.srv.openAccessState(cookie.Value)
	oidc.set("Alice@Example.com", nil, nil)
	w = serve(t, h, callbackRequest(url.Values{"code": {"code-value-1"}, "state": {loc.Query().Get("state")}}.Encode(), cookie.Value))
	if w.Code != 303 || w.Header().Get("Location") != returnTo || w.Header().Get("Cache-Control") != "no-store" || w.Body.Len() != 0 {
		t.Fatalf("%d %v %q", w.Code, w.Header(), w.Body.String())
	}
	expectAccessCookie(t, w, -1)
	session := expectRefreshCookie(t, w)
	// The verifier and the nonce of the cookie reach the provider.
	if oidc.code != "code-value-1" || oidc.verifier != st.Verifier || oidc.token != "id-token-value" || oidc.nonce != st.Nonce {
		t.Fatalf("%+v", oidc)
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

	valid := func() (accessState, string) { return newAccessState(t, api.srv, "/") }
	query := func(st accessState, extra url.Values) string {
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
		"other secret":       {query(st, nil), seal(secretbox.New([]byte(strings.Repeat("x", 32))), accessSealPurpose, plain("/", "")), reasonFailed},
		"unknown key":        {query(st, nil), seal(box, accessSealPurpose, plain("/", `,"x":1`)), reasonFailed},
		"trailing data":      {query(st, nil), seal(box, accessSealPurpose, plain("/", "")+"{}"), reasonFailed},
		"short state":        {query(st, nil), seal(box, accessSealPurpose, fmt.Sprintf(`{"s":"abc","n":%q,"v":%q,"r":"/"}`, st.Nonce, st.Verifier)), reasonFailed},
		"padded nonce":       {query(st, nil), seal(box, accessSealPurpose, fmt.Sprintf(`{"s":%q,"n":%q,"v":%q,"r":"/"}`, st.State, st.Nonce[:42]+"=", st.Verifier)), reasonFailed},
		"no verifier":        {query(st, nil), seal(box, accessSealPurpose, fmt.Sprintf(`{"s":%q,"n":%q,"r":"/"}`, st.State, st.Nonce)), reasonFailed},
		"other site return":  {query(st, nil), seal(box, accessSealPurpose, plain("//evil.test", "")), reasonFailed},
		"signin-error":       {query(st, nil), seal(box, accessSealPurpose, plain("/signin-error", "")), reasonFailed},
		"state mismatch":     {query(accessState{State: st.State + "x"}, nil), value, reasonFailed},
		"other state":        {query(accessState{State: strings.Repeat("A", 43)}, nil), value, reasonFailed},
		"no state":           {"code=code-value-1", value, reasonFailed},
		"two states":         {query(st, url.Values{"state": {st.State, st.State}}), value, reasonFailed},
		"denied":             {query(st, url.Values{"error": {"access_denied"}}), value, reasonDenied},
		"denied empty error": {query(st, url.Values{"error": {""}}), value, reasonDenied},
		"denied bad state":   {query(accessState{State: "x"}, url.Values{"error": {"access_denied"}}), value, reasonFailed},
		"denied no state":    {"error=access_denied", value, reasonFailed},
		"no code":            {url.Values{"state": {st.State}}.Encode(), value, reasonFailed},
		"empty code":         {query(st, url.Values{"code": {""}}), value, reasonFailed},
		"two codes":          {query(st, url.Values{"code": {"a", "b"}}), value, reasonFailed},
		"long code":          {query(st, url.Values{"code": {strings.Repeat("c", 2049)}}), value, reasonFailed},
	} {
		t.Run(name, func(t *testing.T) {
			before := oidc.exchangeCount()
			expectSigninError(t, serve(t, h, callbackRequest(tc.query, tc.cookie)), tc.reason)
			if oidc.exchangeCount() != before {
				t.Fatal("exchanged")
			}
		})
	}
	// A code of 2048 bytes is accepted.
	oidc.set("alice@example.com", nil, nil)
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
		"provider unavailable": {access.ErrProviderUnavailable, nil, reasonUnavailable},
		"exchange failed":      {access.ErrExchangeFailed, nil, reasonFailed},
		"exchange other error": {errors.New("other"), nil, reasonFailed},
		"keys unavailable":     {nil, access.ErrKeysUnavailable, reasonUnavailable},
		"invalid ID token":     {nil, access.ErrInvalidToken, reasonFailed},
		"verify other error":   {nil, errors.New("other"), reasonFailed},
	} {
		t.Run(name, func(t *testing.T) {
			oidc.set("alice@example.com", tc.exchangeErr, tc.verifyErr)
			defer oidc.set("alice@example.com", nil, nil)
			st, value := valid()
			expectSigninError(t, serve(t, h, callbackRequest(query(st, nil), value)), tc.reason)
		})
	}

	// The first sign-in of an address creates a member that no password
	// matches; the next sign-in finds it.
	oidc.set(" Dave@Example.COM ", nil, nil)
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
	oidc.set("carol@example.com", nil, nil)
	st, value = valid()
	expectSigninError(t, serve(t, h, callbackRequest(query(st, nil), value)), reasonDisabled)
	if u, err := api.store.UserByEmail(ctx, "carol@example.com"); err != nil || u.Disabled == 0 {
		t.Fatalf("%+v %v", u, err)
	}

	// A malformed email claim creates nobody.
	for _, email := range []string{"not an address", "bob@", "bøb@example.com", "", strings.Repeat("a", 250) + "@example.com"} {
		oidc.set(email, nil, nil)
		st, value = valid()
		expectSigninError(t, serve(t, h, callbackRequest(query(st, nil), value)), reasonFailed)
	}
	if n := countUsers(); n != 3 {
		t.Fatalf("users: %d", n)
	}

	// Concurrent first sign-ins of one address create one user.
	oidc.set("erin@example.com", nil, nil)
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
	for _, reason := range []string{"auth: access sign-in failed: state mismatch", "auth: access sign-in failed: malformed email claim", "auth: access sign-in failed: user carol-id is disabled", "auth: created user " + dave.ID + " by an access sign-in"} {
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

func TestAccessCallbackDatabaseError(t *testing.T) {
	logs := captureLog(t)
	api, oidc := newAccessAPI(t, "sqlite")
	if err := api.store.Close(); err != nil {
		t.Fatal(err)
	}
	oidc.set("alice@example.com", nil, nil)
	st, value := newAccessState(t, api.srv, "/")
	expectSigninError(t, serve(t, api.handler, callbackRequest(url.Values{"code": {"c"}, "state": {st.State}}.Encode(), value)), reasonUnavailable)
	oidc.set("new@example.com", nil, nil)
	st, value = newAccessState(t, api.srv, "/")
	expectSigninError(t, serve(t, api.handler, callbackRequest(url.Values{"code": {"c"}, "state": {st.State}}.Encode(), value)), reasonUnavailable)
	if strings.Contains(logs.String(), "@example.com") {
		t.Fatalf("log contains the address: %s", logs.String())
	}
}

// Without an OIDC client the callback is unavailable instead of panicking.
func TestAccessCallbackWithoutClient(t *testing.T) {
	api, _ := newAccessAPI(t, "sqlite")
	api.srv.accessOIDC = nil
	st, value := newAccessState(t, api.srv, "/")
	expectSigninError(t, serve(t, api.handler, callbackRequest(url.Values{"code": {"c"}, "state": {st.State}}.Encode(), value)), reasonUnavailable)
	w := accessLogin(t, api.handler, "")
	if w.Code != 302 || w.Header().Get("Location") != "/signin-error?reason=unavailable" {
		t.Fatalf("%d %v", w.Code, w.Header())
	}
}

func TestPasswordEndpointsWithAccess(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) { testPasswordEndpointsWithAccess(t, driver) })
	}
}

func testPasswordEndpointsWithAccess(t *testing.T, driver string) {
	// Links issued while passwords were in use.
	password := newUsersAPI(t, driver)
	alice, _ := password.login("alice@example.com", "correct-password-123")
	inv := decodeJSON[createdInvitationResponse](t, password.do("POST", "/api/v1/invitations", alice, `{"email":"dave@example.com","role":"member"}`))
	pwr := decodeJSON[createdPasswordResetResponse](t, password.do("POST", "/api/v1/users/carol-id/password-resets", alice, ""))
	invToken := fragmentToken(t, inv.URL, usersOrigin+"/invite#")
	pwrToken := fragmentToken(t, pwr.URL, usersOrigin+"/reset-password#")

	// The same database served with Access.
	srv := newServer(accessConfig(), password.srv.auth, fstest.MapFS{}, nil)
	srv.accessOIDC = newFakeOIDC()
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
	srv.cfg.Access = nil
	expect(t, api.public("/auth/login", `{"email":"carol@example.com","password":"wrong-password-123"}`, ""), 401, "unauthorized")
	expect(t, api.public("/auth/invitations/inspect", linkBody("/auth/invitations/inspect", invToken), ""), 404, "invitation_invalid")
}
