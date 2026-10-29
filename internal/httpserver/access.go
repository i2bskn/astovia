package httpserver

import (
	"bytes"
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"

	"github.com/i2bskn/astovia/internal/access"
	"github.com/i2bskn/astovia/internal/auth"
	"github.com/i2bskn/astovia/internal/store"
)

// accessClient is the part of *access.Client used by the Cloudflare Access
// sign-in: the relying party of the SaaS application. Tests replace it.
type accessClient interface {
	// AuthorizationURL returns the URL that starts the sign-in.
	AuthorizationURL(state, nonce, challenge string) string
	// Exchange redeems the authorization code for an ID token.
	// access.ErrProviderUnavailable means the provider could not be reached.
	Exchange(ctx context.Context, code, verifier string) (string, error)
	// Verify verifies the ID token and returns its email claim.
	// access.ErrKeysUnavailable means the signing keys could not be fetched.
	Verify(ctx context.Context, idToken, nonce string) (string, error)
}

const (
	// accessCookie holds the state of a sign-in in progress, sealed.
	accessCookie = "__Host-astovia_access"
	// accessCookieMaxAge is the time allowed to finish a sign-in (10
	// minutes).
	accessCookieMaxAge = 600
	// accessSealPurpose is the purpose the cookie is sealed with.
	accessSealPurpose = "access-login"
	// maxReturnTo bounds return_to in bytes after decoding, so that the
	// consent screen of an MCP client with a long request fits.
	maxReturnTo = 2700
	// maxAccessCookieValue bounds the sealed cookie value, so that the name
	// and the value stay within the 4096 bytes browsers keep.
	maxAccessCookieValue = 3800
	// maxCodeLength bounds the authorization code in bytes.
	maxCodeLength = 2048
	// accessUserPassword is the password hash of the users created by an
	// Access sign-in. It is not a bcrypt hash, so no password matches it if
	// password sign-in is enabled again.
	accessUserPassword = "!access"
	// signinErrorPath is the screen that explains a failed sign-in.
	signinErrorPath = "/signin-error"
)

// Reasons of /signin-error. The screen shows a message for each.
const (
	reasonFailed        = "failed"
	reasonDenied        = "denied"
	reasonUnavailable   = "unavailable"
	reasonDisabled      = "disabled"
	reasonReturnTooLong = "return_too_long"
)

// accessState is the content of the sign-in cookie: the state, nonce and
// PKCE verifier of the sign-in and the path to return to.
type accessState struct {
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
	ReturnTo string `json:"r"`
}

// authConfigResponse tells the web UI how to sign in.
type authConfigResponse struct {
	Mode      string `json:"mode"`
	LogoutURL string `json:"logout_url,omitempty"`
}

// handleAuthConfig answers the sign-in mode: password or Cloudflare Access
// with the URL that ends the Access session.
func (s *Server) handleAuthConfig(w http.ResponseWriter, _ *http.Request) {
	if s.cfg.Access == nil {
		writeJSON(w, 200, authConfigResponse{Mode: "password"})
		return
	}
	writeJSON(w, 200, authConfigResponse{Mode: "cloudflare_access", LogoutURL: s.cfg.Access.TeamDomain + "/cdn-cgi/access/logout"})
}

// passwordAuth rejects with 409 the endpoints of password sign-in, the
// invitations and the password resets when Cloudflare Access signs users
// in. It runs before the rate limits, so that nothing is counted.
func (s *Server) passwordAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.Access != nil {
			fail(w, 409, "password_auth_disabled", "Passwords, invitations and password resets are disabled; this server signs in with Cloudflare Access.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// handleAccessLogin starts the sign-in: it seals a new state, nonce and PKCE
// verifier with return_to in the sign-in cookie and redirects to the
// authorization endpoint of the SaaS application.
func (s *Server) handleAccessLogin(w http.ResponseWriter, r *http.Request) {
	if s.accessOIDC == nil {
		log.Printf("auth: access login: no OIDC client")
		redirectSigninError(w, http.StatusFound, reasonUnavailable)
		return
	}
	returnTo := "/"
	// Several return_to values are ignored.
	if values := r.URL.Query()["return_to"]; len(values) == 1 {
		// A return_to that cannot be kept is not replaced silently: the
		// consent screen of an MCP client would be lost.
		if len(values[0]) > maxReturnTo {
			redirectSigninError(w, http.StatusFound, reasonReturnTooLong)
			return
		}
		if safeReturnTo(values[0]) {
			returnTo = values[0]
		}
	}
	st := accessState{ReturnTo: returnTo}
	for _, p := range []*string{&st.State, &st.Nonce, &st.Verifier} {
		v, err := auth.Random()
		if err != nil {
			log.Printf("auth: access login: %v", err)
			redirectSigninError(w, http.StatusFound, reasonUnavailable)
			return
		}
		*p = v
	}
	value, err := s.sealAccessState(st)
	if err != nil {
		log.Printf("auth: access login: seal state: %v", err)
		redirectSigninError(w, http.StatusFound, reasonUnavailable)
		return
	}
	if len(value) > maxAccessCookieValue {
		redirectSigninError(w, http.StatusFound, reasonReturnTooLong)
		return
	}
	http.SetCookie(w, newAccessCookie(value, accessCookieMaxAge))
	redirect(w, http.StatusFound, s.accessOIDC.AuthorizationURL(st.State, st.Nonce, access.Challenge(st.Verifier)))
}

// handleAccessCallback finishes the sign-in: it checks the state, exchanges
// the code, verifies the ID token, finds or creates the user and starts a
// session with the refresh cookie. Every failure redirects to /signin-error
// with a reason. Only the reasons are logged, never the code, the state,
// the tokens, the cookies or the email address.
func (s *Server) handleAccessCallback(w http.ResponseWriter, r *http.Request) {
	// The sign-in cookie is single use: it is cleared whatever the outcome.
	http.SetCookie(w, newAccessCookie("", -1))
	failed := func(reason, detail string) {
		log.Printf("auth: access sign-in failed: %s", detail)
		redirectSigninError(w, http.StatusSeeOther, reason)
	}
	c, err := r.Cookie(accessCookie)
	if err != nil {
		failed(reasonFailed, "no sign-in in progress")
		return
	}
	st, ok := s.openAccessState(c.Value)
	if !ok {
		failed(reasonFailed, "malformed sign-in state")
		return
	}
	q := r.URL.Query()
	// The state is checked before the error, so that another site cannot
	// show an error for a sign-in it did not start.
	if states := q["state"]; len(states) != 1 || subtle.ConstantTimeCompare([]byte(states[0]), []byte(st.State)) != 1 {
		failed(reasonFailed, "state mismatch")
		return
	}
	if _, ok := q["error"]; ok {
		failed(reasonDenied, "denied by the provider")
		return
	}
	codes := q["code"]
	if len(codes) != 1 || len(codes[0]) == 0 || len(codes[0]) > maxCodeLength {
		failed(reasonFailed, "missing or malformed code")
		return
	}
	if s.accessOIDC == nil {
		failed(reasonUnavailable, "no OIDC client")
		return
	}
	idToken, err := s.accessOIDC.Exchange(r.Context(), codes[0], st.Verifier)
	if errors.Is(err, access.ErrProviderUnavailable) {
		failed(reasonUnavailable, "token endpoint unavailable")
		return
	}
	if err != nil {
		failed(reasonFailed, "code exchange failed")
		return
	}
	claim, err := s.accessOIDC.Verify(r.Context(), idToken, st.Nonce)
	if errors.Is(err, access.ErrKeysUnavailable) {
		failed(reasonUnavailable, "signing keys unavailable")
		return
	}
	if err != nil {
		failed(reasonFailed, "invalid ID token")
		return
	}
	email, err := auth.Email(claim)
	if err != nil {
		failed(reasonFailed, "malformed email claim")
		return
	}
	u, reason, detail := s.accessUser(r.Context(), email)
	if reason != "" {
		failed(reason, detail)
		return
	}
	// The refresh cookie of a session the browser may already hold is not
	// sent here: it is SameSite=Strict and this is a cross-site navigation
	// from Cloudflare. It is replaced by the new cookie, and its session
	// stays on the server until it expires.
	result, err := s.auth.LoginUser(r.Context(), u)
	if errors.Is(err, store.ErrUnauthorized) {
		// Disabled, or the password hash changed, since the lookup.
		failed(reasonFailed, "user "+u.ID+" changed during the sign-in")
		return
	}
	if err != nil {
		log.Printf("auth: access sign-in: sign in user=%s: %v", u.ID, err)
		failed(reasonUnavailable, "cannot start the session")
		return
	}
	s.setRefreshCookie(w, result)
	redirect(w, http.StatusSeeOther, st.ReturnTo)
}

// accessUser returns the enabled user of the verified, normalized email,
// creating it as a member on the first sign-in. Otherwise it returns the
// reason for /signin-error and the detail to log. A disabled user is never
// enabled again here.
func (s *Server) accessUser(ctx context.Context, email string) (store.User, string, string) {
	u, err := s.auth.Store.UserByEmail(ctx, email)
	if errors.Is(err, sql.ErrNoRows) {
		// Who can sign in is decided by the Access policy.
		id, idErr := auth.Random()
		if idErr != nil {
			return store.User{}, reasonUnavailable, "cannot create a user ID"
		}
		u = store.User{ID: id, Email: email, PasswordHash: accessUserPassword, CreatedAt: s.auth.Now().Unix(), Role: store.RoleMember}
		err = s.auth.Store.CreateUser(ctx, u)
		switch {
		case err == nil:
			log.Printf("auth: created user %s by an access sign-in", u.ID)
		case errors.Is(err, store.ErrConflict):
			// Created by a concurrent sign-in with the same address. Read it
			// once.
			u, err = s.auth.Store.UserByEmail(ctx, email)
		default:
			// The error of a failed insert is not logged: it may name the
			// address.
			return store.User{}, reasonUnavailable, "cannot create the user"
		}
	}
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return store.User{}, reasonUnavailable, "cannot find the created user"
	case err != nil:
		log.Printf("auth: access sign-in: find user: %v", err)
		return store.User{}, reasonUnavailable, "database error while finding the user"
	case u.Disabled != 0:
		return store.User{}, reasonDisabled, "user " + u.ID + " is disabled"
	}
	return u, "", ""
}

// safeReturnTo reports whether v is a path and query of a screen on this
// origin to return to after the sign-in. Anything else, including a path to
// another site, is rejected. Only printable ASCII is accepted: the browser
// sends the path and the query percent-encoded.
func safeReturnTo(v string) bool {
	if len(v) > maxReturnTo || !strings.HasPrefix(v, "/") || strings.HasPrefix(v, "//") || strings.HasPrefix(v, "/\\") || strings.Contains(v, "#") {
		return false
	}
	for i := 0; i < len(v); i++ {
		if v[i] <= 0x20 || v[i] >= 0x7f {
			return false
		}
	}
	u, err := url.Parse(v)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil || u.Opaque != "" || u.Fragment != "" {
		return false
	}
	return screenPath(u.Path) && strings.TrimSuffix(u.Path, "/") != signinErrorPath
}

// validRandomValue reports whether v has the form produced by auth.Random:
// 32 bytes in unpadded base64url (43 characters).
func validRandomValue(v string) bool {
	if len(v) != 43 {
		return false
	}
	b, err := base64.RawURLEncoding.Strict().DecodeString(v)
	return err == nil && len(b) == 32
}

// sealAccessState returns the sign-in cookie value: the JSON of st sealed
// for accessSealPurpose. HTML characters in the return path are not
// escaped, so that the size check counts the real length.
func (s *Server) sealAccessState(st accessState) (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(st); err != nil {
		return "", err
	}
	return s.box.Seal(accessSealPurpose, string(bytes.TrimSuffix(buf.Bytes(), []byte("\n"))))
}

// openAccessState reads the sign-in cookie value. A value that was not
// sealed by this server for the sign-in, unknown keys, trailing data and
// values not in the form written by handleAccessLogin are rejected.
func (s *Server) openAccessState(v string) (accessState, bool) {
	if len(v) > maxAccessCookieValue {
		return accessState{}, false
	}
	plain, err := s.box.Open(accessSealPurpose, v)
	if err != nil {
		return accessState{}, false
	}
	d := json.NewDecoder(strings.NewReader(plain))
	d.DisallowUnknownFields()
	var st accessState
	if err = d.Decode(&st); err != nil {
		return accessState{}, false
	}
	var extra any
	if err = d.Decode(&extra); err != io.EOF {
		return accessState{}, false
	}
	if !validRandomValue(st.State) || !validRandomValue(st.Nonce) || !validRandomValue(st.Verifier) || !safeReturnTo(st.ReturnTo) {
		return accessState{}, false
	}
	return st, true
}

// newAccessCookie returns the sign-in cookie: host-only, HTTPS-only, for the
// whole site and unreadable by scripts. It is Lax, not Strict, because the
// callback is a cross-site navigation from Cloudflare. A negative maxAge
// deletes it.
func newAccessCookie(value string, maxAge int) *http.Cookie {
	return &http.Cookie{Name: accessCookie, Value: value, Path: "/", MaxAge: maxAge, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode}
}

// redirect redirects to location as it is, without a body. http.Redirect is
// not used because it cleans relative paths and writes HTML.
func redirect(w http.ResponseWriter, status int, location string) {
	w.Header().Set("Location", location)
	w.WriteHeader(status)
}

// redirectSigninError redirects to the sign-in error screen with reason.
func redirectSigninError(w http.ResponseWriter, status int, reason string) {
	redirect(w, status, signinErrorPath+"?reason="+url.QueryEscape(reason))
}
