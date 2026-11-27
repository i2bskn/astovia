package httpserver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/i2bskn/authlume/webauthn"

	"github.com/i2bskn/astovia/internal/auth"
	"github.com/i2bskn/astovia/internal/store"
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

// storeKey stores a WebAuthn credential id for userID through the
// credential store, as an enrollment before the first session does.
func storeKey(t testing.TB, s *store.Store, userID, id string) {
	t.Helper()
	ctx := context.Background()
	u, err := s.UserByID(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	e := store.WebAuthnEnrollment{UserID: userID, Name: "Key " + id, PasswordDigest: auth.Hash(u.PasswordHash), Now: time.Now().Unix()}
	cred := webauthn.Credential{ID: []byte("credential-" + id), PublicKey: []byte("public-" + id)}
	if err := s.WebAuthnCredentials().Insert(store.WithWebAuthnEnrollment(ctx, e), userID, cred, false); err != nil {
		t.Fatal(err)
	}
}

// captureLog sends the log to a buffer for the rest of the test.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return &buf
}
