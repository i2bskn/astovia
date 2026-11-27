package httpserver

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// testOrigin is the public URL of most handlers in the tests.
const testOrigin = "https://astovia.test"

// sessionCookie returns the session cookie that w sets, as "name=value" for
// a Cookie header.
func sessionCookie(t testing.TB, w *httptest.ResponseRecorder) string {
	t.Helper()
	for _, c := range w.Result().Cookies() {
		if c.Name == SessionCookieName && c.Value != "" && c.MaxAge >= 0 {
			return c.Name + "=" + c.Value
		}
	}
	t.Fatalf("no session cookie: %d %v %s", w.Code, w.Header().Values("Set-Cookie"), w.Body.String())
	return ""
}

// authorize sets the credential token on r: an API token ("ast_") as a
// Bearer header, anything else but "" as the session cookie ("name=value").
// It sets the Origin of testOrigin unless r has an Origin already, so that
// the tests of the credentials are not answered by the Origin check.
func authorize(r *http.Request, token string) {
	switch {
	case token == "":
	case strings.HasPrefix(token, "ast_"):
		r.Header.Set("Authorization", "Bearer "+token)
	default:
		r.Header.Add("Cookie", token)
	}
	if _, ok := r.Header["Origin"]; !ok {
		r.Header.Set("Origin", testOrigin)
	}
}

// sessionID returns the ID of the session of a session cookie
// ("name=value"): the hex of the SHA-256 of the decoded value.
func sessionID(t testing.TB, cookie string) string {
	t.Helper()
	_, value, _ := strings.Cut(cookie, "=")
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		t.Fatalf("cookie %q: %v", cookie, err)
	}
	key := sha256.Sum256(raw)
	return hex.EncodeToString(key[:])
}

// signIn signs in with a password through h and returns the session
// cookie.
func signIn(t testing.TB, h http.Handler, email, password string) string {
	t.Helper()
	r := httptest.NewRequest("POST", "/auth/login", strings.NewReader(`{"email":"`+email+`","password":"`+password+`"}`))
	r.Header.Set("Origin", testOrigin)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("sign in %s: %d %s", email, w.Code, w.Body.String())
	}
	return sessionCookie(t, w)
}
