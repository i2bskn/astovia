package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/i2bskn/astovia/internal/store"
	"github.com/i2bskn/authlume/webauthn"
)

const (
	// webauthnBodyLimit is the body limit of the requests that carry the
	// response of an authenticator.
	webauthnBodyLimit = 32768
	// webauthnCookie holds the handle of the ceremony in progress between
	// the begin and the finish request. The ceremony itself is kept by
	// authlume in webauthn_ceremonies and can be taken once.
	webauthnCookie = "__Host-astovia_webauthn"
	// webauthnCookieMaxAge is the time allowed between the begin and the
	// finish request (5 minutes), as authlume's ceremony timeout.
	webauthnCookieMaxAge = 5 * 60
	// maxWebAuthnHandleLength bounds the handle read from the cookie.
	maxWebAuthnHandleLength = 128
	// maxCredentialNameLength is the length of the name of a credential in
	// characters.
	maxCredentialNameLength = 64
)

// webauthnBeginResponse starts a WebAuthn ceremony. Options is the argument
// of navigator.credentials.create or get, {"publicKey": options}, with the
// PublicKeyCredentialCreationOptions or PublicKeyCredentialRequestOptions in
// the JSON form of the specification (binary members as base64url without
// padding). The handle of the ceremony is in the webauthn cookie.
type webauthnBeginResponse struct {
	Options json.RawMessage `json:"options"`
}

// webauthnCreatedResponse is a credential registered from the account
// screen, with the recovery codes when it is the first factor of the user
// and null otherwise.
type webauthnCreatedResponse struct {
	Credential    mfaCredentialResponse `json:"credential"`
	RecoveryCodes []string              `json:"recovery_codes"`
}

// newWebAuthn returns the WebAuthn verifier of publicURL, or nil when
// passkeys and security keys cannot be used at it: authlume requires an
// https origin, whose host name is the relying party ID. A host it rejects
// is logged and leaves WebAuthn off; the server starts anyway. now is the
// clock of the stores, the server's.
func newWebAuthn(publicURL string, st *store.Store, now func() time.Time) *webauthn.Verifier {
	if !strings.HasPrefix(publicURL, "https://") {
		return nil
	}
	v, err := webauthn.New(publicURL, "Astovia", st.WebAuthnCredentials(now), st.WebAuthnCeremonies(now))
	if err != nil {
		log.Printf("auth: mfa: passkeys and security keys are off: %v", err)
		return nil
	}
	return v
}

// webauthnAvailable reports whether passkeys and security keys can be used
// on this server.
func (s *Server) webauthnAvailable() bool { return s.webauthn != nil }

// requireWebAuthn writes 400 webauthn_unavailable when passkeys and
// security keys cannot be used on this server.
func (s *Server) requireWebAuthn(w http.ResponseWriter) bool {
	if s.webauthnAvailable() {
		return true
	}
	fail(w, 400, "webauthn_unavailable", "Passkeys and security keys cannot be used at the address of this server.")
	return false
}

// newWebAuthnCookie returns the cookie of the ceremony handle: host-only,
// HTTPS-only, for the whole site, unreadable by scripts and sent only by
// this site. A negative maxAge deletes it.
func newWebAuthnCookie(value string, maxAge int) *http.Cookie {
	return &http.Cookie{Name: webauthnCookie, Value: value, Path: "/", MaxAge: maxAge, HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode}
}

// writeWebAuthnBegin answers the options of a ceremony that began and sets
// the cookie of its handle. The options are passed on as authlume encodes
// them: the argument of navigator.credentials.create or get in JSON form,
// {"publicKey": options}.
func writeWebAuthnBegin(w http.ResponseWriter, options json.RawMessage, handle string) error {
	if !json.Valid(options) {
		return errors.New("webauthn options are not JSON")
	}
	http.SetCookie(w, newWebAuthnCookie(handle, webauthnCookieMaxAge))
	writeJSON(w, 200, webauthnBeginResponse{Options: options})
	return nil
}

type webauthnHandleKey struct{}

// takeWebAuthnHandle wraps the routes that finish a ceremony: before the
// checks of the route, it clears the cookie of the ceremony handle and
// hands its value to next (see webauthnHandle). Once it runs, a ceremony is
// finished once, whatever the outcome. A request refused before it, by the
// Origin check (403) or the method check (405), leaves the cookie as it
// is; the ceremony then stays until its 5 minutes run out, and can still be
// taken only once.
func takeWebAuthnHandle(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, newWebAuthnCookie("", -1))
		handle := ""
		if c, err := r.Cookie(webauthnCookie); err == nil && len(c.Value) <= maxWebAuthnHandleLength {
			handle = c.Value
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), webauthnHandleKey{}, handle)))
	})
}

// webauthnHandle returns the ceremony handle takeWebAuthnHandle took, ""
// when there is none.
func webauthnHandle(r *http.Request) string {
	v, _ := r.Context().Value(webauthnHandleKey{}).(string)
	return v
}

// credentialName returns the name a user gave a credential, trimmed: 1-64
// characters without control characters.
func credentialName(v string) (string, bool) {
	name := strings.TrimSpace(v)
	if !utf8.ValidString(name) {
		return "", false
	}
	if n := utf8.RuneCountInString(name); n < 1 || n > maxCredentialNameLength {
		return "", false
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return "", false
		}
	}
	return name, true
}

// beginWebAuthnRegistration answers the options to create a credential for
// u, which exclude the credentials u has, and sets the cookie of the
// ceremony handle.
func (s *Server) beginWebAuthnRegistration(w http.ResponseWriter, r *http.Request, u store.User) {
	options, handle, err := s.webauthn.BeginRegistration(r.Context(), u.ID, u.Email)
	if err == nil {
		err = writeWebAuthnBegin(w, options, handle)
	}
	if err != nil {
		mfaError(w, "webauthn begin", u.ID, err)
	}
}

// finishWebAuthnRegistration verifies the response of the authenticator
// against the ceremony of the cookie and stores the credential for u with
// the conditions of e, in one transaction that locks the user row. It
// returns the stored credential and whether the recovery codes of e were
// stored. Otherwise it writes the response: codeStatus mfa_code_invalid for
// a missing or spent ceremony and a response authlume rejects, 409 for the
// enrollment conditions, and unauthorized when the user, the session or the
// password changed after the ceremony began.
func (s *Server) finishWebAuthnRegistration(w http.ResponseWriter, r *http.Request, u store.User, response json.RawMessage, e store.WebAuthnEnrollment, codeStatus int, unauthorized func()) (store.WebAuthnCredential, bool, bool) {
	handle := webauthnHandle(r)
	if handle == "" {
		mfaStateInvalid(w)
		return store.WebAuthnCredential{}, false, false
	}
	var stored store.WebAuthnCredential
	var issued bool
	e.UserID, e.Now, e.Stored, e.Issued = u.ID, s.auth.Now().Unix(), &stored, &issued
	subject, err := s.webauthn.FinishRegistration(store.WithWebAuthnEnrollment(r.Context(), e), handle, response)
	switch {
	case err == nil && subject == u.ID:
		return stored, issued, true
	case err == nil:
		// Not reachable: the enrollment is bound to u.ID.
		mfaError(w, "webauthn finish", u.ID, errors.New("ceremony of another user"))
	case webauthnEnrollError(w, err):
	case errors.Is(err, store.ErrUnauthorized):
		unauthorized()
	case errors.Is(err, webauthn.ErrInvalid):
		mfaCodeInvalid(w, codeStatus, "webauthn", u.ID)
	default:
		mfaError(w, "webauthn finish", u.ID, err)
	}
	return store.WebAuthnCredential{}, false, false
}

// webauthnEnrollError writes the response for the errors of storing a
// credential that both enrollments share, and reports whether err was one
// of them.
func webauthnEnrollError(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, store.ErrMFAEnrolled):
		fail(w, 409, "mfa_already_enrolled", "Two-step verification is already set up. Sign in with it.")
	case errors.Is(err, store.ErrMFALimit):
		fail(w, 409, "mfa_limit", "No more passkeys or security keys can be registered.")
	case errors.Is(err, store.ErrMFACredentialExists):
		fail(w, 409, "mfa_credential_exists", "This passkey or security key is already registered.")
	default:
		return false
	}
	return true
}

// handleMFAWebAuthnBegin starts the sign-in with a passkey or security key:
// the options allow the usable credentials of the user of the MFA token
// alone.
func (s *Server) handleMFAWebAuthnBegin(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Token string `json:"mfa_token"`
	}
	if !decode(w, r, &in, mfaBodyLimit) {
		return
	}
	u, _, ok := s.mfaUser(w, r, in.Token)
	if !ok || !s.requireWebAuthn(w) {
		return
	}
	options, handle, err := s.webauthn.BeginVerification(r.Context(), u.ID)
	switch {
	case errors.Is(err, webauthn.ErrInvalid):
		// No usable credential.
		mfaCodeInvalid(w, 401, "webauthn", u.ID)
		return
	case err == nil:
		err = writeWebAuthnBegin(w, options, handle)
	}
	if err != nil {
		mfaError(w, "webauthn begin", u.ID, err)
	}
}

// handleMFAWebAuthnFinish signs in with the response of a passkey or
// security key. The ceremony is taken once and the credential is updated
// only at its current version, so the same response signs in once.
func (s *Server) handleMFAWebAuthnFinish(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Token      string          `json:"mfa_token"`
		Credential json.RawMessage `json:"credential"`
	}
	if !decode(w, r, &in, webauthnBodyLimit) {
		return
	}
	if len(in.Credential) == 0 {
		fail(w, 400, "invalid_request", "credential is required.")
		return
	}
	u, digest, ok := s.mfaUser(w, r, in.Token)
	if !ok || !s.requireWebAuthn(w) || !s.allowMFAAttempt(w, u.ID) {
		return
	}
	handle := webauthnHandle(r)
	if handle == "" {
		mfaStateInvalid(w)
		return
	}
	// authlume checks the challenge, the origin, the relying party ID, the
	// signature and the counter, and that the credential is one of the
	// subject of the ceremony.
	subject, err := s.webauthn.FinishVerification(r.Context(), handle, in.Credential)
	switch {
	case err == nil && subject == u.ID:
	case err == nil, errors.Is(err, webauthn.ErrInvalid), errors.Is(err, webauthn.ErrRecord):
		// Another user's ceremony, a spent or expired one, a response
		// that does not verify, or a credential removed meanwhile.
		mfaCodeInvalid(w, 401, "webauthn", u.ID)
		return
	default:
		mfaError(w, "webauthn", u.ID, err)
		return
	}
	s.completeMFASignIn(w, r, u, digest, nil)
}

// handleMFAEnrollWebAuthnBegin starts the registration of a passkey or
// security key before the first session.
func (s *Server) handleMFAEnrollWebAuthnBegin(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Token string `json:"mfa_token"`
	}
	if !decode(w, r, &in, mfaBodyLimit) {
		return
	}
	u, _, ok := s.mfaUser(w, r, in.Token)
	if !ok || !s.mfaEnrollable(w, r, u) || !s.requireWebAuthn(w) {
		return
	}
	s.beginWebAuthnRegistration(w, r, u)
}

// handleMFAEnrollWebAuthnFinish stores the passkey or security key of a
// user that has no second factor yet and must have one, issues the recovery
// codes and signs the user in. The attempts are not limited per user: the
// response holds no secret that could be guessed.
func (s *Server) handleMFAEnrollWebAuthnFinish(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Token      string          `json:"mfa_token"`
		Credential json.RawMessage `json:"credential"`
		Name       string          `json:"name"`
	}
	if !decode(w, r, &in, webauthnBodyLimit) {
		return
	}
	name, ok := credentialName(in.Name)
	if !ok || len(in.Credential) == 0 {
		fail(w, 400, "invalid_request", "credential is required and name must contain 1-64 characters.")
		return
	}
	u, digest, ok := s.mfaUser(w, r, in.Token)
	if !ok || !s.mfaEnrollable(w, r, u) || !s.requireWebAuthn(w) {
		return
	}
	codes, rows, err := newRecoveryCodes()
	if err != nil {
		mfaError(w, "webauthn enroll", u.ID, err)
		return
	}
	// The store checks again, under the lock of the user row, that the user
	// has no factor (of two concurrent enrollments, one wins) and still has
	// the password the MFA token was issued for.
	_, _, ok = s.finishWebAuthnRegistration(w, r, u, in.Credential, store.WebAuthnEnrollment{Name: name, Codes: rows, OnlyIfNone: true, PasswordDigest: digest}, 401, func() { mfaTokenInvalid(w) })
	if !ok {
		return
	}
	log.Printf("auth: mfa: webauthn enrolled user=%s", u.ID)
	s.completeMFASignIn(w, r, u, digest, codes)
}

// handleBeginWebAuthn starts the registration of a passkey or security key
// from the account screen.
func (s *Server) handleBeginWebAuthn(w http.ResponseWriter, r *http.Request) {
	u, ok := userFrom(r.Context())
	if !ok {
		s.authError(w, r, store.ErrUnauthorized)
		return
	}
	var in struct {
		Password string `json:"password"`
	}
	if !decode(w, r, &in, mfaBodyLimit) || !s.requireWebAuthn(w) || !s.confirmPassword(w, r, u, in.Password) {
		return
	}
	status, err := s.auth.Store.MFAStatus(r.Context(), u.ID)
	if err != nil {
		mfaError(w, "webauthn begin", u.ID, err)
		return
	}
	// Rows stored before this version are not counted; the store checks
	// the limit again when it inserts.
	if status.UsableWebAuthn() >= store.MaxWebAuthnCredentials {
		webauthnEnrollError(w, store.ErrMFALimit)
		return
	}
	s.beginWebAuthnRegistration(w, r, u)
}

// handleFinishWebAuthn stores the passkey or security key the authenticator
// created. The ceremony stands for the password check of the begin request;
// the store refuses it once the session that began it has ended. The first
// factor of the user issues the recovery codes.
func (s *Server) handleFinishWebAuthn(w http.ResponseWriter, r *http.Request) {
	u, ok := userFrom(r.Context())
	sid, ok2 := sessionFrom(r.Context())
	if !ok || !ok2 {
		s.authError(w, r, store.ErrUnauthorized)
		return
	}
	var in struct {
		Credential json.RawMessage `json:"credential"`
		Name       string          `json:"name"`
	}
	if !decode(w, r, &in, webauthnBodyLimit) {
		return
	}
	name, ok := credentialName(in.Name)
	if !ok || len(in.Credential) == 0 {
		fail(w, 400, "invalid_request", "credential is required and name must contain 1-64 characters.")
		return
	}
	if !s.requireWebAuthn(w) {
		return
	}
	codes, rows, err := newRecoveryCodes()
	if err != nil {
		mfaError(w, "webauthn enroll", u.ID, err)
		return
	}
	credential, issued, ok := s.finishWebAuthnRegistration(w, r, u, in.Credential, store.WebAuthnEnrollment{Name: name, Codes: rows, SessionID: sid}, 400, func() { s.authError(w, r, store.ErrUnauthorized) })
	if !ok {
		return
	}
	log.Printf("auth: mfa: webauthn enrolled user=%s", u.ID)
	if !issued {
		codes = nil
	}
	writeJSON(w, 201, webauthnCreatedResponse{Credential: mfaCredentialResponse{ID: credential.ID, Name: credential.Name, CreatedAt: formatTime(credential.CreatedAt)}, RecoveryCodes: codes})
}

// handleDeleteWebAuthn removes a passkey or security key of the signed-in
// user. It works where passkeys cannot be used as well, so that a key
// registered at another address can be removed.
func (s *Server) handleDeleteWebAuthn(w http.ResponseWriter, r *http.Request) {
	u, ok := userFrom(r.Context())
	if !ok {
		s.authError(w, r, store.ErrUnauthorized)
		return
	}
	var in struct {
		Password string `json:"password"`
	}
	if !decode(w, r, &in, mfaBodyLimit) || !s.confirmPassword(w, r, u, in.Password) {
		return
	}
	if err := s.auth.Store.DeleteWebAuthnCredential(r.Context(), u.ID, r.PathValue("id"), s.cfg.MFARequired); err != nil {
		if !s.factorError(w, r, err) {
			mfaError(w, "webauthn delete", u.ID, err)
		}
		return
	}
	log.Printf("auth: mfa: webauthn deleted user=%s", u.ID)
	w.WriteHeader(204)
}
