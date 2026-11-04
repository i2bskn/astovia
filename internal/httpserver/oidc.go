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

	"github.com/i2bskn/astovia/internal/auth"
	"github.com/i2bskn/astovia/internal/config"
	"github.com/i2bskn/astovia/internal/oidc"
	"github.com/i2bskn/astovia/internal/store"
)

// relyingParty is the part of *oidc.Client used by the OIDC sign-in. Tests
// replace it.
type relyingParty interface {
	// AuthorizationURL returns the URL that starts the sign-in.
	// oidc.ErrProviderUnavailable means the provider metadata could not be
	// read.
	AuthorizationURL(state, nonce, challenge string) (string, error)
	// Exchange redeems the authorization code for an ID token.
	// oidc.ErrProviderUnavailable means the provider could not be reached.
	Exchange(ctx context.Context, code, verifier string) (string, error)
	// Verify verifies the ID token and returns its identity.
	// oidc.ErrKeysUnavailable means the signing keys could not be fetched.
	Verify(ctx context.Context, idToken, nonce string) (oidc.Identity, error)
}

const (
	// oidcCookie holds the state of a sign-in in progress, sealed.
	oidcCookie = "__Host-astovia_oidc"
	// oidcCookieMaxAge is the time allowed to finish a sign-in (10 minutes).
	oidcCookieMaxAge = 600
	// oidcSealPurpose is the purpose the cookie is sealed with.
	oidcSealPurpose = "oidc-login"
	// maxReturnTo bounds return_to in bytes after decoding, so that the
	// consent screen of an MCP client with a long request fits.
	maxReturnTo = 2700
	// maxOIDCCookieValue bounds the sealed cookie value, so that the name
	// and the value stay within the 4096 bytes browsers keep.
	maxOIDCCookieValue = 3800
	// maxCodeLength bounds the authorization code in bytes.
	maxCodeLength = 2048
	// oidcUserPassword is the password hash of the users created by an OIDC
	// sign-in. It is not a bcrypt hash, so no password matches it if
	// password sign-in is enabled again. The value is the one of the
	// Cloudflare Access sign-in of earlier releases, kept so that the
	// existing rows have the same value.
	oidcUserPassword = "!access"
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
	reasonNotAllowed    = "not_allowed"
	reasonNotVerified   = "email_not_verified"
)

// oidcState is the content of the sign-in cookie: the state, nonce and
// PKCE verifier of the sign-in and the path to return to.
type oidcState struct {
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
	ReturnTo string `json:"r"`
}

// authConfigResponse tells the web UI how to sign in.
type authConfigResponse struct {
	Mode string `json:"mode"`
}

// handleAuthConfig answers the sign-in mode: password or oidc.
func (s *Server) handleAuthConfig(w http.ResponseWriter, _ *http.Request) {
	if s.cfg.OIDC == nil {
		writeJSON(w, 200, authConfigResponse{Mode: config.AuthModePassword})
		return
	}
	writeJSON(w, 200, authConfigResponse{Mode: config.AuthModeOIDC})
}

// passwordAuth rejects with 409 the endpoints of password sign-in, the
// invitations and the password resets when OpenID Connect signs users in.
// It runs before the rate limits, so that nothing is counted.
func (s *Server) passwordAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.OIDC != nil {
			fail(w, 409, "password_auth_disabled", "Passwords, invitations and password resets are disabled; this server signs in with OpenID Connect.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// handleOIDCLogin starts the sign-in: it seals a new state, nonce and PKCE
// verifier with return_to in the sign-in cookie and redirects to the
// authorization endpoint of the provider. The cookie is set only once the
// authorization URL is known, so that a provider that cannot be reached
// leaves no cookie.
func (s *Server) handleOIDCLogin(w http.ResponseWriter, r *http.Request) {
	if s.rp == nil {
		log.Printf("auth: oidc login: no OIDC client")
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
	st := oidcState{ReturnTo: returnTo}
	for _, p := range []*string{&st.State, &st.Nonce, &st.Verifier} {
		v, err := auth.Random()
		if err != nil {
			log.Printf("auth: oidc login: %v", err)
			redirectSigninError(w, http.StatusFound, reasonUnavailable)
			return
		}
		*p = v
	}
	value, err := s.sealOIDCState(st)
	if err != nil {
		log.Printf("auth: oidc login: seal state: %v", err)
		redirectSigninError(w, http.StatusFound, reasonUnavailable)
		return
	}
	if len(value) > maxOIDCCookieValue {
		redirectSigninError(w, http.StatusFound, reasonReturnTooLong)
		return
	}
	location, err := s.rp.AuthorizationURL(st.State, st.Nonce, oidc.Challenge(st.Verifier))
	if err != nil {
		log.Printf("auth: oidc login: provider unavailable")
		redirectSigninError(w, http.StatusFound, reasonUnavailable)
		return
	}
	http.SetCookie(w, newOIDCCookie(value, oidcCookieMaxAge))
	redirect(w, http.StatusFound, location)
}

// handleOIDCCallback finishes the sign-in: it checks the state, exchanges
// the code, verifies the ID token, checks email_verified and the allowed
// emails, finds or creates the user and starts a session with the refresh
// cookie. Every failure redirects to /signin-error with a reason. Only the
// reasons are logged, never the code, the state, the tokens, the cookies or
// the email address.
func (s *Server) handleOIDCCallback(w http.ResponseWriter, r *http.Request) {
	// The sign-in cookie is single use: it is cleared whatever the outcome.
	http.SetCookie(w, newOIDCCookie("", -1))
	failed := func(reason, detail string) {
		log.Printf("auth: oidc sign-in failed: %s", detail)
		redirectSigninError(w, http.StatusSeeOther, reason)
	}
	c, err := r.Cookie(oidcCookie)
	if err != nil {
		failed(reasonFailed, "no sign-in in progress")
		return
	}
	st, ok := s.openOIDCState(c.Value)
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
	if s.rp == nil || s.cfg.OIDC == nil {
		failed(reasonUnavailable, "no OIDC client")
		return
	}
	idToken, err := s.rp.Exchange(r.Context(), codes[0], st.Verifier)
	if errors.Is(err, oidc.ErrProviderUnavailable) {
		failed(reasonUnavailable, "token endpoint unavailable")
		return
	}
	if err != nil {
		failed(reasonFailed, "code exchange failed")
		return
	}
	identity, err := s.rp.Verify(r.Context(), idToken, st.Nonce)
	if errors.Is(err, oidc.ErrKeysUnavailable) {
		failed(reasonUnavailable, "signing keys unavailable")
		return
	}
	if err != nil {
		failed(reasonFailed, "invalid ID token")
		return
	}
	email, err := auth.Email(identity.Email)
	if err != nil {
		failed(reasonFailed, "malformed email claim")
		return
	}
	if !emailVerified(identity.EmailVerified, s.cfg.OIDC.RequireEmailVerified) {
		failed(reasonNotVerified, "email not verified")
		return
	}
	// Checked on every sign-in, so that removing an address from the list
	// stops existing users as well.
	if !auth.EmailAllowed(email, s.cfg.OIDC.AllowedEmails) {
		failed(reasonNotAllowed, "email not allowed")
		return
	}
	u, reason, detail := s.oidcUser(r.Context(), email)
	if reason != "" {
		failed(reason, detail)
		return
	}
	// The refresh cookie of a session the browser may already hold is not
	// sent here: it is SameSite=Strict and this is a cross-site navigation
	// from the provider. It is replaced by the new cookie, and its session
	// stays on the server until it expires.
	result, err := s.auth.LoginUser(r.Context(), u)
	if errors.Is(err, store.ErrUnauthorized) {
		// Disabled, or the password hash changed, since the lookup.
		failed(reasonFailed, "user "+u.ID+" changed during the sign-in")
		return
	}
	if err != nil {
		log.Printf("auth: oidc sign-in: sign in user=%s: %v", u.ID, err)
		failed(reasonUnavailable, "cannot start the session")
		return
	}
	s.setRefreshCookie(w, result)
	redirect(w, http.StatusSeeOther, st.ReturnTo)
}

// emailVerified reports whether the email_verified claim (nil when absent)
// passes: it must be true, or, when require is false, may be absent.
func emailVerified(claim *bool, require bool) bool {
	if claim == nil {
		return !require
	}
	return *claim
}

// oidcUser returns the enabled user of the verified, normalized email,
// creating it as a member on the first sign-in. Otherwise it returns the
// reason for /signin-error and the detail to log. A disabled user is never
// enabled again here.
func (s *Server) oidcUser(ctx context.Context, email string) (store.User, string, string) {
	u, err := s.auth.Store.UserByEmail(ctx, email)
	if errors.Is(err, sql.ErrNoRows) {
		// Who can sign in is decided by the provider and the allowed emails.
		id, idErr := auth.Random()
		if idErr != nil {
			return store.User{}, reasonUnavailable, "cannot create a user ID"
		}
		u = store.User{ID: id, Email: email, PasswordHash: oidcUserPassword, CreatedAt: s.auth.Now().Unix(), Role: store.RoleMember}
		err = s.auth.Store.CreateUser(ctx, u)
		switch {
		case err == nil:
			log.Printf("auth: created user %s by an oidc sign-in", u.ID)
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
		log.Printf("auth: oidc sign-in: find user: %v", err)
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

// sealOIDCState returns the sign-in cookie value: the JSON of st sealed
// for oidcSealPurpose. HTML characters in the return path are not
// escaped, so that the size check counts the real length.
func (s *Server) sealOIDCState(st oidcState) (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(st); err != nil {
		return "", err
	}
	return s.box.Seal(oidcSealPurpose, string(bytes.TrimSuffix(buf.Bytes(), []byte("\n"))))
}

// openOIDCState reads the sign-in cookie value. A value that was not
// sealed by this server for the sign-in, unknown keys, trailing data and
// values not in the form written by handleOIDCLogin are rejected.
func (s *Server) openOIDCState(v string) (oidcState, bool) {
	if len(v) > maxOIDCCookieValue {
		return oidcState{}, false
	}
	plain, err := s.box.Open(oidcSealPurpose, v)
	if err != nil {
		return oidcState{}, false
	}
	d := json.NewDecoder(strings.NewReader(plain))
	d.DisallowUnknownFields()
	var st oidcState
	if err = d.Decode(&st); err != nil {
		return oidcState{}, false
	}
	var extra any
	if err = d.Decode(&extra); err != io.EOF {
		return oidcState{}, false
	}
	if !validRandomValue(st.State) || !validRandomValue(st.Nonce) || !validRandomValue(st.Verifier) || !safeReturnTo(st.ReturnTo) {
		return oidcState{}, false
	}
	return st, true
}

// newOIDCCookie returns the sign-in cookie: host-only, HTTPS-only, for the
// whole site and unreadable by scripts. It is Lax, not Strict, because the
// callback is a cross-site navigation from the provider. A negative maxAge
// deletes it.
func newOIDCCookie(value string, maxAge int) *http.Cookie {
	return &http.Cookie{Name: oidcCookie, Value: value, Path: "/", MaxAge: maxAge, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode}
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
