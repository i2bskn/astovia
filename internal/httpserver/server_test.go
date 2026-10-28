package httpserver

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"github.com/i2bskn/astovia/internal/auth"
	"github.com/i2bskn/astovia/internal/config"
	"github.com/i2bskn/astovia/internal/store"
	"github.com/i2bskn/astovia/internal/testutil"
	"golang.org/x/crypto/bcrypt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

func TestMain(m *testing.M) {
	auth.PasswordCost = bcrypt.MinCost
	os.Exit(m.Run())
}

func jsonKeys(t *testing.T, raw []byte) (map[string]json.RawMessage, []string) {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return m, keys
}

func TestHTTP(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) { testHTTP(t, driver) })
	}
}

func testHTTP(t *testing.T, driver string) {
	s, _ := testutil.Database(t, driver)
	hash, e := auth.Password("correct-password-123")
	if e != nil {
		t.Fatal(e)
	}
	if e = s.CreateUser(context.Background(), store.User{ID: "alice-id", Email: "alice@example.com", PasswordHash: hash}); e != nil {
		t.Fatal(e)
	}
	a, e := auth.New(s, make([]byte, 32), "https://astovia.test")
	if e != nil {
		t.Fatal(e)
	}
	handler := New(config.Config{PublicURL: "https://astovia.test", Secure: true}, a, fstest.MapFS{"index.html": {Data: []byte("<html>Astovia</html>")}, "assets/app.js": {Data: []byte("void 0")}})
	request := func(method, path, body, origin, ct, cookie, bearer string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Origin", origin)
		r.Header.Set("Content-Type", ct)
		if cookie != "" {
			r.Header.Set("Cookie", cookie)
		}
		if bearer != "" {
			r.Header.Set("Authorization", "Bearer "+bearer)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	for _, tc := range []struct {
		origin, ct, body string
		status           int
	}{{"", "application/json", "{}", 403}, {"https://evil.test", "application/json", "{}", 403}, {"https://astovia.test", "text/plain", "{}", 415}, {"https://astovia.test", "application/json", "null", 400}, {"https://astovia.test", "application/json", "{\"user_id\":\"bob\"}", 400}, {"https://astovia.test", "application/json", "{} {}", 400}, {"https://astovia.test", "application/json", strings.Repeat("x", 4097), 413}} {
		w := request("POST", "/auth/refresh", tc.body, tc.origin, tc.ct, "", "")
		if w.Code != tc.status {
			t.Fatalf("%+v got %d %s", tc, w.Code, w.Body.String())
		}
		if len(w.Result().Cookies()) != 0 {
			t.Fatal("failure modified cookie")
		}
	}
	w := request("POST", "/auth/login", `{"email":"alice@example.com","password":"correct-password-123"}`, "https://astovia.test", "application/json", "", "")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var result authResponse
	if e = json.Unmarshal(w.Body.Bytes(), &result); e != nil {
		t.Fatal(e)
	}
	// The response keys are part of the browser contract (web/src/auth.ts).
	body, keys := jsonKeys(t, w.Body.Bytes())
	if !reflect.DeepEqual(keys, []string{"access_token", "expires_in", "user"}) {
		t.Fatalf("login keys: %v", keys)
	}
	if _, keys = jsonKeys(t, body["user"]); !reflect.DeepEqual(keys, []string{"email", "id"}) {
		t.Fatalf("login user keys: %v", keys)
	}
	if strings.Contains(w.Body.String(), hash) || strings.Contains(w.Body.String(), "password_hash") {
		t.Fatal("private hash serialized")
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatal(cookies)
	}
	c := cookies[0]
	if c.Path != "/auth" || !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteStrictMode || c.MaxAge <= 0 {
		t.Fatalf("cookie: %+v", c)
	}
	cookie := c.Name + "=" + c.Value
	w = request("GET", "/api/v1/me", "", "", "", "", result.AccessToken)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "alice-id") || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal(w.Code, w.Body.String())
	}
	if _, keys = jsonKeys(t, w.Body.Bytes()); !reflect.DeepEqual(keys, []string{"email", "id"}) {
		t.Fatalf("me keys: %v", keys)
	}
	w = request("POST", "/auth/refresh", "{}", "https://astovia.test", "application/json", cookie, "")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if e = json.Unmarshal(w.Body.Bytes(), &result); e != nil {
		t.Fatal(e)
	}
	w = request("POST", "/auth/logout", "{}", "https://astovia.test", "application/json", cookie, "")
	if w.Code != 204 || w.Result().Cookies()[0].MaxAge != -1 {
		t.Fatal(w.Code, w.Body.String())
	}
	w = request("GET", "/api/v1/me", "", "", "", "", result.AccessToken)
	if w.Code != 401 {
		t.Fatal("logout did not revoke access")
	}
	for _, p := range []string{"/api/nope", "/api/v1/unknown", "/integrations/nope", "/assets/missing.js", "/missing.css", "/index.html", "/foo", "/settings/foo/bar"} {
		w = request("GET", p, "", "", "", "", "")
		if w.Code != 404 || strings.Contains(w.Body.String(), "<html>") {
			t.Fatalf("fallback escaped: %s %d", p, w.Code)
		}
	}
	for _, p := range []string{"/", "/candidates", "/auto", "/settings/foo", "/consent"} {
		w = request("GET", p, "", "", "", "", "")
		if w.Code != 200 || !strings.Contains(w.Body.String(), "<html>") {
			t.Fatal(p, w.Code)
		}
	}
	w = request("GET", "/auth/login", "", "", "", "", "")
	if w.Code != 405 || w.Header().Get("Allow") != "POST" {
		t.Fatal("method handling")
	}
	w = request("GET", "/healthz", "", "", "", "", "")
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	// Rate limiting applies to the direct peer regardless of forwarded headers.
	for i := 0; i < 12; i++ {
		w = request("POST", "/auth/login", `{"email":"alice@example.com","password":""}`, "https://astovia.test", "application/json", "", "")
	}
	if w.Code != 429 || w.Header().Get("Retry-After") == "" {
		t.Fatal("rate limit absent")
	}
	// Database errors do not delete the browser's refresh cookie or claim success.
	if e = s.Close(); e != nil {
		t.Fatal(e)
	}
	w = request("POST", "/auth/logout", "{}", "https://astovia.test", "application/json", cookie, "")
	if w.Code != 503 || len(w.Result().Cookies()) != 0 {
		t.Fatalf("DB error %d", w.Code)
	}
}
func TestBucketBound(t *testing.T) {
	l := newLoginLimiter()
	now := time.Now()
	l.now = func() time.Time { return now }
	for i := 0; i < l.capacity; i++ {
		l.entries[string(rune(i))] = bucket{start: now, count: l.limit}
	}
	// A full table of limited peers evicts the oldest entry rather than
	// rejecting every new peer.
	if !l.allow("new:1") {
		t.Fatal("full table rejected a new peer")
	}
	if len(l.entries) != l.capacity {
		t.Fatalf("table grew past capacity: %d", len(l.entries))
	}
	now = now.Add(2 * time.Minute)
	if !l.allow("new:2") {
		t.Fatal("expired entries not reclaimed")
	}
	if len(l.entries) != 1 {
		t.Fatalf("expired entries kept: %d", len(l.entries))
	}
}

// Non-clean paths are redirected to the clean path by ServeMux (307 with the
// pinned Go toolchain) and escaped slashes never match a route.
func TestServeMuxPathHandling(t *testing.T) {
	handler := New(config.Config{PublicURL: "https://astovia.test"}, nil, fstest.MapFS{"index.html": {Data: []byte("<html>Astovia</html>")}})
	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{"POST", "/auth//login", http.StatusTemporaryRedirect},
		{"GET", "/admin/../api/v1/me", http.StatusTemporaryRedirect},
		{"GET", "/api%2Fv1%2Fme", http.StatusNotFound},
		{"POST", "/auth%2Flogin", http.StatusNotFound},
	} {
		r := httptest.NewRequest(tc.method, tc.path, strings.NewReader("{}"))
		r.Header.Set("Origin", "https://astovia.test")
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatalf("%s %s: got %d %s", tc.method, tc.path, w.Code, w.Body.String())
		}
		if tc.status == http.StatusNotFound {
			var body struct {
				Error struct{ Code string } `json:"error"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.Error.Code != "not_found" {
				t.Fatalf("%s %s: not a JSON 404: %s", tc.method, tc.path, w.Body.String())
			}
		}
	}
}

func TestRejectedLoginDoesNotConsumeRateLimit(t *testing.T) {
	s, _ := testutil.Database(t, "sqlite")
	a, err := auth.New(s, make([]byte, 32), "https://astovia.test")
	if err != nil {
		t.Fatal(err)
	}
	handler := New(config.Config{PublicURL: "https://astovia.test"}, a, fstest.MapFS{})
	request := func(origin, ct string) int {
		r := httptest.NewRequest("POST", "/auth/login", strings.NewReader(`{"email":"alice@example.com","password":"wrong-password-1"}`))
		r.Header.Set("Origin", origin)
		r.Header.Set("Content-Type", ct)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w.Code
	}
	for i := 0; i < 20; i++ {
		if code := request("https://evil.test", "application/json"); code != http.StatusForbidden {
			t.Fatalf("foreign origin: %d", code)
		}
		if code := request("https://astovia.test", "text/plain"); code != http.StatusUnsupportedMediaType {
			t.Fatalf("wrong content type: %d", code)
		}
	}
	if code := request("https://astovia.test", "application/json"); code != http.StatusUnauthorized {
		t.Fatalf("rejected requests consumed attempts: %d", code)
	}
}

func TestCanonicalPublicURLAuthentication(t *testing.T) {
	t.Setenv("ASTOVIA_DB_DRIVER", "sqlite")
	t.Setenv("ASTOVIA_DB_DSN", "")
	t.Setenv("ASTOVIA_PUBLIC_URL", "https://EXAMPLE.com:443")
	t.Setenv("ASTOVIA_JWT_SECRET", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	cfg, err := config.Load(true)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PublicURL != "https://example.com" {
		t.Fatalf("noncanonical config: %q", cfg.PublicURL)
	}
	s, _ := testutil.Database(t, "sqlite")
	hash, err := auth.Password("correct-password-123")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.CreateUser(context.Background(), store.User{ID: "alice-id", Email: "alice@example.com", PasswordHash: hash}); err != nil {
		t.Fatal(err)
	}
	a, err := auth.New(s, cfg.Secret, cfg.PublicURL)
	if err != nil {
		t.Fatal(err)
	}
	handler := New(cfg, a, fstest.MapFS{"index.html": {Data: []byte("<html>Astovia</html>")}})
	request := func(method, path, body, origin string, cookie *http.Cookie, token string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", origin)
		if cookie != nil {
			r.AddCookie(cookie)
		}
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	w := request("POST", "/auth/login", `{"email":"alice@example.com","password":"correct-password-123"}`, "https://example.com", nil, "")
	if w.Code != http.StatusOK {
		t.Fatalf("canonical login: %d %s", w.Code, w.Body.String())
	}
	var result authResponse
	if err = json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("expected refresh cookie, got %d", len(cookies))
	}
	w = request("GET", "/api/v1/me", "", "", nil, result.AccessToken)
	if w.Code != http.StatusOK {
		t.Fatalf("canonical issuer JWT: %d %s", w.Code, w.Body.String())
	}
	w = request("POST", "/auth/refresh", "{}", "https://example.com", cookies[0], "")
	if w.Code != http.StatusOK {
		t.Fatalf("canonical refresh: %d %s", w.Code, w.Body.String())
	}
	cookies = w.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("expected rotated cookie, got %d", len(cookies))
	}
	for _, origin := range []string{"", "https://foreign.example", "https://example.com:8443"} {
		w = request("POST", "/auth/logout", "{}", origin, cookies[0], "")
		if w.Code != http.StatusForbidden {
			t.Fatalf("foreign/missing Origin %q accepted: %d", origin, w.Code)
		}
	}
	w = request("POST", "/auth/logout", "{}", "https://example.com", cookies[0], "")
	if w.Code != http.StatusNoContent {
		t.Fatalf("canonical logout: %d %s", w.Code, w.Body.String())
	}
}

// errorCode returns error.code from a JSON error response, or "" when the
// body is not one.
func errorCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		return ""
	}
	return body.Error.Code
}

// serve sends r to h and checks the headers every response must carry.
func serve(t *testing.T, h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if !strings.Contains(w.Header().Get("Content-Security-Policy"), "default-src 'self'") || w.Header().Get("X-Content-Type-Options") != "nosniff" || w.Header().Get("Referrer-Policy") != "same-origin" {
		t.Fatalf("%s %s: security headers missing: %v", r.Method, r.URL.Path, w.Header())
	}
	if (strings.HasPrefix(r.URL.Path, "/auth/") || strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/oauth/")) && w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("%s %s: Cache-Control=%q", r.Method, r.URL.Path, w.Header().Get("Cache-Control"))
	}
	return w
}

func TestAuthEndpointInputs(t *testing.T) {
	s, _ := testutil.Database(t, "sqlite")
	hash, err := auth.Password("correct-password-123")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.CreateUser(context.Background(), store.User{ID: "alice-id", Email: "alice@example.com", PasswordHash: hash}); err != nil {
		t.Fatal(err)
	}
	a, err := auth.New(s, make([]byte, 32), "https://astovia.test")
	if err != nil {
		t.Fatal(err)
	}
	const origin = "https://astovia.test"
	type tc struct {
		name, method, origin, ct, body string
		status                         int
		code                           string
	}
	common := func(okBody string) []tc {
		return []tc{
			{"origin mismatch", "POST", "https://evil.test", "application/json", okBody, 403, "origin_rejected"},
			{"content type mismatch", "POST", origin, "text/plain", okBody, 415, "unsupported_media_type"},
			{"body over 4096 bytes", "POST", origin, "application/json", `{"x":"` + strings.Repeat("x", 4096) + `"}`, 413, "body_too_large"},
			{"two objects", "POST", origin, "application/json", "{}{}", 400, "invalid_request"},
			{"unknown field", "POST", origin, "application/json", `{"unknown":1}`, 400, "invalid_request"},
			{"non-object", "POST", origin, "application/json", `["x"]`, 400, "invalid_request"},
			{"GET", "GET", origin, "application/json", "", 405, "method_not_allowed"},
		}
	}
	cases := map[string][]tc{
		"/auth/login": append(common(`{"email":"alice@example.com","password":"wrong-password-1"}`),
			tc{"charset parameter", "POST", origin, "application/json; charset=utf-8", `{"email":"alice@example.com","password":"wrong-password-1"}`, 401, "unauthorized"},
			tc{"username key", "POST", origin, "application/json", `{"username":"alice@example.com","password":"correct-password-123"}`, 400, "invalid_request"},
			tc{"email of 256 bytes", "POST", origin, "application/json", `{"email":"` + strings.Repeat("a", 256) + `","password":"wrong-password-1"}`, 401, "unauthorized"},
			tc{"email over 256 bytes", "POST", origin, "application/json", `{"email":"` + strings.Repeat("a", 257) + `","password":"wrong-password-1"}`, 400, "invalid_request"},
			tc{"password of 72 bytes", "POST", origin, "application/json", `{"email":"alice@example.com","password":"` + strings.Repeat("p", 72) + `"}`, 401, "unauthorized"},
			tc{"password over 72 bytes", "POST", origin, "application/json", `{"email":"alice@example.com","password":"` + strings.Repeat("p", 73) + `"}`, 400, "invalid_request"},
		),
		"/auth/refresh": append(common("{}"),
			tc{"charset parameter without cookie", "POST", origin, "application/json; charset=utf-8", "{}", 401, "unauthorized"},
		),
		"/auth/logout": append(common("{}"),
			tc{"charset parameter without cookie", "POST", origin, "application/json; charset=utf-8", "{}", 204, ""},
		),
	}
	for path, list := range cases {
		for _, c := range list {
			t.Run(path+"/"+c.name, func(t *testing.T) {
				// A new handler per case keeps rate limiting state separate.
				h := New(config.Config{PublicURL: origin, Secure: true}, a, fstest.MapFS{})
				r := httptest.NewRequest(c.method, path, strings.NewReader(c.body))
				r.Header.Set("Origin", c.origin)
				r.Header.Set("Content-Type", c.ct)
				w := serve(t, h, r)
				if w.Code != c.status || errorCode(t, w) != c.code {
					t.Fatalf("got %d %q, want %d %q: %s", w.Code, errorCode(t, w), c.status, c.code, w.Body.String())
				}
				if c.status == 405 && w.Header().Get("Allow") != "POST" {
					t.Fatalf("Allow=%q", w.Header().Get("Allow"))
				}
				if c.status != 204 && len(w.Result().Cookies()) != 0 {
					t.Fatal("failure modified cookie")
				}
			})
		}
	}
}

func TestRedirectsAndBearer(t *testing.T) {
	s, _ := testutil.Database(t, "sqlite")
	a, err := auth.New(s, make([]byte, 32), "https://astovia.test")
	if err != nil {
		t.Fatal(err)
	}
	h := New(config.Config{PublicURL: "https://astovia.test"}, a, fstest.MapFS{"index.html": {Data: []byte("<html>Astovia</html>")}})
	if w := serve(t, h, httptest.NewRequest("GET", "/admin", nil)); w.Code != http.StatusPermanentRedirect || w.Header().Get("Location") != "/" {
		t.Fatalf("/admin: %d Location=%q", w.Code, w.Header().Get("Location"))
	}
	for _, v := range []string{"", "Token abc", "bearer abc", "Bearer", "Bearer ", "Basic YWxpY2U6cHc="} {
		r := httptest.NewRequest("GET", "/api/v1/me", nil)
		if v != "" {
			r.Header.Set("Authorization", v)
		}
		w := serve(t, h, r)
		if w.Code != 401 || errorCode(t, w) != "unauthorized" {
			t.Fatalf("Authorization %q: %d %s", v, w.Code, w.Body.String())
		}
	}
}

func TestStaticAssets(t *testing.T) {
	assets := fstest.MapFS{"index.html": {Data: []byte("<html>Astovia</html>")}, "assets/app.js": {Data: []byte("void 0")}, "favicon.svg": {Data: []byte("<svg/>")}, ".env.js": {Data: []byte("secret")}, "LICENSE": {Data: []byte("text")}}
	h := New(config.Config{PublicURL: "https://astovia.test"}, nil, assets)
	w := serve(t, h, httptest.NewRequest("GET", "/assets/app.js", nil))
	if w.Code != 200 || !strings.Contains(w.Header().Get("Content-Type"), "javascript") || !strings.Contains(w.Header().Get("Cache-Control"), "immutable") || w.Body.String() != "void 0" {
		t.Fatalf("app.js: %d %v", w.Code, w.Header())
	}
	w = serve(t, h, httptest.NewRequest("HEAD", "/favicon.svg", nil))
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "image/svg+xml") || !strings.Contains(w.Header().Get("Cache-Control"), "immutable") {
		t.Fatalf("favicon.svg: %d %v", w.Code, w.Header())
	}
	for _, path := range []string{"/", "/candidates", "/candidates/", "/auto", "/auto/", "/settings", "/settings/", "/settings/notifications", "/settings/ai-clients/", "/settings/foo", "/consent", "/consent/"} {
		for _, method := range []string{"GET", "HEAD"} {
			w = serve(t, h, httptest.NewRequest(method, path, nil))
			if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") {
				t.Fatalf("%s %s: %d %v", method, path, w.Code, w.Header())
			}
			if method == "GET" && w.Body.String() != "<html>Astovia</html>" {
				t.Fatalf("%s %s: %s", method, path, w.Body.String())
			}
		}
	}
	for _, path := range []string{"/missing.js", "/index.html", "/.env.js", "/LICENSE", "/foo", "/assets", "/assets/", "/settings/foo/bar", "/settings/Foo", "/auto/x", "/consent/x", "/api/v1/unknown", "/some/route"} {
		w = serve(t, h, httptest.NewRequest("GET", path, nil))
		if w.Code != 404 || errorCode(t, w) != "not_found" {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
	}
	for _, path := range []string{"/", "/auto", "/settings/notifications", "/assets/app.js", "/favicon.svg", "/admin", "/admin/auto"} {
		w = serve(t, h, httptest.NewRequest("POST", path, nil))
		if w.Code != 405 || w.Header().Get("Allow") != "GET, HEAD" || errorCode(t, w) != "method_not_allowed" {
			t.Fatalf("POST %s: %d %v %s", path, w.Code, w.Header(), w.Body.String())
		}
	}
	h = New(config.Config{PublicURL: "https://astovia.test"}, nil, fstest.MapFS{"assets/app.js": {Data: []byte("void 0")}})
	w = serve(t, h, httptest.NewRequest("GET", "/", nil))
	if w.Code != 404 || errorCode(t, w) != "not_found" {
		t.Fatalf("missing index.html: %d %s", w.Code, w.Body.String())
	}
}

// The screens moved from /admin to /. Legacy URLs are redirected with 308
// only to fixed or validated same-origin paths; anything else goes to /.
func TestLegacyAdminRedirects(t *testing.T) {
	assets := fstest.MapFS{"index.html": {Data: []byte("<html>Astovia</html>")}, "assets/app.js": {Data: []byte("void 0")}, "favicon.svg": {Data: []byte("<svg/>")}, "sw.js": {Data: []byte("self")}}
	h := New(config.Config{PublicURL: "https://astovia.test"}, nil, assets)
	for path, want := range map[string]string{
		"/admin":                        "/",
		"/admin/":                       "/",
		"/admin/auto":                   "/auto",
		"/admin/auto/":                  "/auto",
		"/admin/settings":               "/settings",
		"/admin/settings/":              "/settings",
		"/admin/settings/notifications": "/settings/notifications",
		"/admin/settings/ai-clients/":   "/settings/ai-clients",
		"/admin/settings/Foo":           "/",
		"/admin/settings/a/b":           "/",
		"/admin/settings/" + strings.Repeat("a", 33):       "/",
		"/admin/oauth/authorize?request=a+b%2Fc&x=1":       "/consent?request=" + url.QueryEscape("a b/c"),
		"/admin/oauth/authorize/?request=r1&request=r2":    "/consent?request=r1",
		"/admin/oauth/authorize":                           "/consent",
		"/admin/oauth/authorize?request=x&redirect=//evil": "/consent?request=x",
		"/admin/assets/app.js":                             "/assets/app.js",
		"/admin/favicon.svg":                               "/favicon.svg",
		"/admin/missing.css":                               "/",
		"/admin/assets/missing.js":                         "/",
		"/admin/index.html":                                "/",
		"/admin/connections":                               "/",
		"/admin/auto?x=1":                                  "/auto",
		"/admin/%5Cevil.example":                           "/",
		"/admin/%09/evil.example":                          "/",
	} {
		for _, method := range []string{"GET", "HEAD"} {
			w := serve(t, h, httptest.NewRequest(method, path, nil))
			if w.Code != http.StatusPermanentRedirect || w.Header().Get("Location") != want {
				t.Fatalf("%s %s: %d Location=%q, want %q", method, path, w.Code, w.Header().Get("Location"), want)
			}
		}
	}
	// ServeMux redirects non-clean paths first; whichever handler answers,
	// the location must stay a same-origin path.
	for _, path := range []string{"/admin//evil.example", "/admin/https://evil.example", "/admin/settings/%2F%2Fevil", "/admin/%5C%5Cevil.example", "/admin/..%2F..%2Fevil"} {
		for hops := 0; ; hops++ {
			w := serve(t, h, httptest.NewRequest("GET", path, nil))
			if w.Code < 300 || w.Code > 399 {
				break
			}
			loc := w.Header().Get("Location")
			u, err := url.Parse(loc)
			if err != nil || u.Scheme != "" || u.Host != "" || !strings.HasPrefix(loc, "/") || strings.HasPrefix(loc, "//") || strings.HasPrefix(loc, "/\\") || strings.ContainsAny(loc, "\\\t\n\r") {
				t.Fatalf("%s: unsafe Location %q", path, loc)
			}
			if hops > 3 {
				t.Fatalf("%s: redirect loop at %q", path, loc)
			}
			path = loc
		}
	}
	w := serve(t, h, httptest.NewRequest("GET", "/admin/sw.js", nil))
	if w.Code != 200 || w.Body.String() != "self" || w.Header().Get("Cache-Control") != "no-cache" || !strings.Contains(w.Header().Get("Content-Type"), "javascript") {
		t.Fatalf("/admin/sw.js: %d %v", w.Code, w.Header())
	}
}
