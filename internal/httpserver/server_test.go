package httpserver

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"github.com/i2bskn/astovia/internal/auth"
	"github.com/i2bskn/astovia/internal/config"
	"github.com/i2bskn/astovia/internal/store"
	"github.com/i2bskn/astovia/internal/testutil"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

func TestHTTP(t *testing.T) {
	s, _ := testutil.Database(t, "sqlite")
	hash, e := auth.Password("correct-password-123")
	if e != nil {
		t.Fatal(e)
	}
	if e = s.CreateUser(context.Background(), store.User{ID: "alice-id", Username: "alice", PasswordHash: hash}); e != nil {
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
	w := request("POST", "/auth/login", `{"username":"alice","password":"correct-password-123"}`, "https://astovia.test", "application/json", "", "")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var result auth.Result
	if e = json.Unmarshal(w.Body.Bytes(), &result); e != nil {
		t.Fatal(e)
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
	for _, p := range []string{"/api/nope", "/mcp", "/integrations/nope", "/admin/assets/missing.js", "/admin/missing.css"} {
		w = request("GET", p, "", "", "", "", "")
		if w.Code != 404 || strings.Contains(w.Body.String(), "<html>") {
			t.Fatalf("fallback escaped: %s %d", p, w.Code)
		}
	}
	for _, p := range []string{"/admin/", "/admin/connections"} {
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
		w = request("POST", "/auth/login", `{"username":"alice","password":""}`, "https://astovia.test", "application/json", "", "")
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
	s := &Server{attempts: make(map[string]bucket)}
	for i := 0; i < 4096; i++ {
		s.attempts[string(rune(i))] = bucket{start: time.Now()}
	}
	if s.allowed("new:1") {
		t.Fatal("unbounded peers")
	}
	for k, v := range s.attempts {
		v.start = time.Now().Add(-2 * time.Minute)
		s.attempts[k] = v
	}
	if !s.allowed("new:1") {
		t.Fatal("expired entries not reclaimed")
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
	if err = s.CreateUser(context.Background(), store.User{ID: "alice-id", Username: "alice", PasswordHash: hash}); err != nil {
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
	w := request("POST", "/auth/login", `{"username":"alice","password":"correct-password-123"}`, "https://example.com", nil, "")
	if w.Code != http.StatusOK {
		t.Fatalf("canonical login: %d %s", w.Code, w.Body.String())
	}
	var result auth.Result
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
