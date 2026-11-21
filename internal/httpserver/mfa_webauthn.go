package httpserver

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/i2bskn/astovia/internal/auth"
	"github.com/i2bskn/astovia/internal/store"
)

const (
	// webauthnBodyLimit is the body limit of the requests that carry the
	// response of an authenticator.
	webauthnBodyLimit = 32768
	// maxWebAuthnStateLength bounds the sealed state in bytes.
	maxWebAuthnStateLength = 16384
	// webauthnRegisterPurpose and webauthnLoginPurpose are the prefixes of
	// the purposes the state of a registration and of a sign-in are sealed
	// with; the user ID follows. The two differ, so that neither state
	// serves the other ceremony. The registration purpose serves the
	// enrollment before the first session and the account screen alike:
	// both begin after the password was checked.
	webauthnRegisterPurpose = "mfa-webauthn-register:"
	webauthnLoginPurpose    = "mfa-webauthn-login:"
	// webauthnStateLifetime is the time allowed between the begin and the
	// finish request (5 minutes), in seconds.
	webauthnStateLifetime = 5 * 60
	// maxCredentialNameLength is the length of the name of a credential in
	// characters.
	maxCredentialNameLength = 64
)

// webauthnBeginResponse starts a WebAuthn ceremony. Options is the
// PublicKeyCredentialCreationOptions or PublicKeyCredentialRequestOptions
// in the JSON form of the specification (binary members as base64url
// without padding); State carries the challenge, sealed, back to the finish
// request.
type webauthnBeginResponse struct {
	Options any    `json:"options"`
	State   string `json:"state"`
}

// webauthnCreatedResponse is a credential registered from the account
// screen, with the recovery codes when it is the first factor of the user
// and null otherwise.
type webauthnCreatedResponse struct {
	Credential    mfaCredentialResponse `json:"credential"`
	RecoveryCodes []string              `json:"recovery_codes"`
}

// webauthnState is the content of the state of a ceremony: the session
// data of the WebAuthn library, which holds the challenge, and the expiry
// (Unix seconds). It is never stored.
type webauthnState struct {
	Session   webauthn.SessionData `json:"s"`
	ExpiresAt int64                `json:"e"`
}

// webauthnUser is a user as the WebAuthn library sees it: the user handle
// is the user ID and both names are the email address.
type webauthnUser struct {
	user        store.User
	credentials []webauthn.Credential
}

func (u webauthnUser) WebAuthnID() []byte                         { return []byte(u.user.ID) }
func (u webauthnUser) WebAuthnName() string                       { return u.user.Email }
func (u webauthnUser) WebAuthnDisplayName() string                { return u.user.Email }
func (u webauthnUser) WebAuthnCredentials() []webauthn.Credential { return u.credentials }

// newWebAuthn returns the relying party of publicURL, or nil when passkeys
// and security keys cannot be used at it: the relying party ID is the host
// name, which must not be an IP address, and browsers offer WebAuthn only
// over https or at localhost. A host the library rejects is logged and
// leaves WebAuthn off; the server starts anyway.
func newWebAuthn(publicURL string) *webauthn.WebAuthn {
	public, err := url.Parse(publicURL)
	if err != nil {
		return nil
	}
	host := public.Hostname()
	if host == "" || net.ParseIP(host) != nil || (public.Scheme != "https" && host != "localhost") {
		return nil
	}
	rp, err := webauthn.New(&webauthn.Config{
		RPID:                  host,
		RPDisplayName:         "Astovia",
		RPOrigins:             []string{publicURL},
		AttestationPreference: protocol.PreferNoAttestation,
		AuthenticatorSelection: protocol.AuthenticatorSelection{
			RequireResidentKey: protocol.ResidentKeyNotRequired(),
			ResidentKey:        protocol.ResidentKeyRequirementDiscouraged,
			UserVerification:   protocol.VerificationPreferred,
		},
		// No extension is requested and no output is read. Browsers and
		// password manager extensions return some anyway (credProps); the
		// default would reject those responses.
		ExtensionsUnsolicitedOutputPolicy: protocol.UnsolicitedOutputPolicyIgnore,
	})
	if err != nil {
		log.Printf("auth: mfa: passkeys and security keys are off: %v", err)
		return nil
	}
	return rp
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

// webauthnUserOf returns u with the credentials of status as the library
// reads them.
func webauthnUserOf(u store.User, status store.MFAStatus) (webauthnUser, error) {
	out := webauthnUser{user: u, credentials: make([]webauthn.Credential, 0, len(status.WebAuthn))}
	for _, row := range status.WebAuthn {
		var c webauthn.Credential
		if err := json.Unmarshal([]byte(row.Credential), &c); err != nil {
			return webauthnUser{}, fmt.Errorf("stored credential %s: %w", row.ID, err)
		}
		out.credentials = append(out.credentials, c)
	}
	return out, nil
}

// sealWebAuthnState seals the session data of a ceremony of userID that
// begins now.
func (s *Server) sealWebAuthnState(purpose, userID string, session *webauthn.SessionData) (string, error) {
	raw, err := json.Marshal(webauthnState{Session: *session, ExpiresAt: s.auth.Now().Unix() + webauthnStateLifetime})
	if err != nil {
		return "", err
	}
	return s.box.Seal(purpose+userID, string(raw))
}

// openWebAuthnState opens a state sealed for the ceremony purpose of
// userID. It writes 400 mfa_state_invalid for a state that does not open
// for this user and ceremony or has expired; the second of the expiry is
// expired already.
func (s *Server) openWebAuthnState(w http.ResponseWriter, purpose, userID, state string) (webauthnState, bool) {
	var out webauthnState
	if len(state) == 0 || len(state) > maxWebAuthnStateLength {
		mfaStateInvalid(w)
		return webauthnState{}, false
	}
	raw, err := s.box.Open(purpose+userID, state)
	if err != nil || json.Unmarshal([]byte(raw), &out) != nil || out.Session.Challenge == "" || s.auth.Now().Unix() >= out.ExpiresAt {
		mfaStateInvalid(w)
		return webauthnState{}, false
	}
	return out, true
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
// u, which exclude the credentials u has, and the state that carries the
// challenge to the finish request. Nothing is stored.
func (s *Server) beginWebAuthnRegistration(w http.ResponseWriter, u store.User, status store.MFAStatus) {
	user, err := webauthnUserOf(u, status)
	if err != nil {
		mfaError(w, "webauthn begin", u.ID, err)
		return
	}
	exclusions := make([]protocol.CredentialDescriptor, 0, len(user.credentials))
	for _, c := range user.credentials {
		exclusions = append(exclusions, c.Descriptor())
	}
	creation, session, err := s.webauthn.BeginRegistration(user, webauthn.WithExclusions(exclusions))
	if err != nil {
		mfaError(w, "webauthn begin", u.ID, err)
		return
	}
	state, err := s.sealWebAuthnState(webauthnRegisterPurpose, u.ID, session)
	if err != nil {
		mfaError(w, "webauthn begin", u.ID, err)
		return
	}
	writeJSON(w, 200, webauthnBeginResponse{Options: creation.Response, State: state})
}

// confirmWebAuthnRegistration opens the registration state of u and
// verifies the response of the authenticator against it. It returns the
// credential to store. Otherwise it writes 400 mfa_state_invalid for a
// state that does not open or has expired, and codeStatus mfa_code_invalid
// for a response the library rejects.
func (s *Server) confirmWebAuthnRegistration(w http.ResponseWriter, u store.User, state string, response json.RawMessage, name string, codeStatus int) (store.WebAuthnCredential, bool) {
	opened, ok := s.openWebAuthnState(w, webauthnRegisterPurpose, u.ID, state)
	if !ok {
		return store.WebAuthnCredential{}, false
	}
	parsed, err := protocol.ParseCredentialCreationResponseBytes(response)
	if err != nil {
		mfaCodeInvalid(w, codeStatus, "webauthn", u.ID)
		return store.WebAuthnCredential{}, false
	}
	// The credentials of the user are not needed to create one: the
	// database rejects a credential ID that is registered already.
	credential, err := s.webauthn.CreateCredential(webauthnUser{user: u}, opened.Session, parsed)
	if err != nil {
		mfaCodeInvalid(w, codeStatus, "webauthn", u.ID)
		return store.WebAuthnCredential{}, false
	}
	raw, err := json.Marshal(credential)
	if err != nil {
		mfaError(w, "webauthn finish", u.ID, err)
		return store.WebAuthnCredential{}, false
	}
	id, err := auth.Random()
	if err != nil {
		mfaError(w, "webauthn finish", u.ID, err)
		return store.WebAuthnCredential{}, false
	}
	return store.WebAuthnCredential{ID: id, UserID: u.ID, CredentialID: base64.RawURLEncoding.EncodeToString(credential.ID), Credential: string(raw), Name: name, CreatedAt: s.auth.Now().Unix()}, true
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
// the options allow the credentials of the user of the MFA token alone.
func (s *Server) handleMFAWebAuthnBegin(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Token string `json:"mfa_token"`
	}
	if !decode(w, r, &in, mfaBodyLimit) {
		return
	}
	u, ok := s.mfaUser(w, r, in.Token)
	if !ok || !s.requireWebAuthn(w) {
		return
	}
	status, err := s.auth.Store.MFAStatus(r.Context(), u.ID)
	if err != nil {
		mfaError(w, "webauthn begin", u.ID, err)
		return
	}
	if len(status.WebAuthn) == 0 {
		mfaCodeInvalid(w, 401, "webauthn", u.ID)
		return
	}
	user, err := webauthnUserOf(u, status)
	if err != nil {
		mfaError(w, "webauthn begin", u.ID, err)
		return
	}
	assertion, session, err := s.webauthn.BeginLogin(user)
	if err != nil {
		mfaError(w, "webauthn begin", u.ID, err)
		return
	}
	state, err := s.sealWebAuthnState(webauthnLoginPurpose, u.ID, session)
	if err != nil {
		mfaError(w, "webauthn begin", u.ID, err)
		return
	}
	writeJSON(w, 200, webauthnBeginResponse{Options: assertion.Response, State: state})
}

// handleMFAWebAuthnFinish signs in with the response of a passkey or
// security key. The challenge is spent with the update of the credential,
// so the same response signs in once.
func (s *Server) handleMFAWebAuthnFinish(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Token      string          `json:"mfa_token"`
		State      string          `json:"state"`
		Credential json.RawMessage `json:"credential"`
	}
	if !decode(w, r, &in, webauthnBodyLimit) {
		return
	}
	if len(in.Credential) == 0 {
		fail(w, 400, "invalid_request", "credential is required.")
		return
	}
	u, ok := s.mfaUser(w, r, in.Token)
	if !ok || !s.requireWebAuthn(w) || !s.allowMFAAttempt(w, u.ID) {
		return
	}
	opened, ok := s.openWebAuthnState(w, webauthnLoginPurpose, u.ID, in.State)
	if !ok {
		return
	}
	status, err := s.auth.Store.MFAStatus(r.Context(), u.ID)
	if err != nil {
		mfaError(w, "webauthn", u.ID, err)
		return
	}
	user, err := webauthnUserOf(u, status)
	if err != nil {
		mfaError(w, "webauthn", u.ID, err)
		return
	}
	parsed, err := protocol.ParseCredentialRequestResponseBytes(in.Credential)
	if err != nil {
		mfaCodeInvalid(w, 401, "webauthn", u.ID)
		return
	}
	// The library checks the challenge, the origin, the relying party ID
	// and the signature, and that the credential is one of this user.
	credential, err := s.webauthn.ValidateLogin(user, opened.Session, parsed)
	if err != nil {
		mfaCodeInvalid(w, 401, "webauthn", u.ID)
		return
	}
	// A signature counter that did not advance: the credential may have
	// been cloned. The counter is not stored.
	if credential.Authenticator.CloneWarning {
		mfaCodeInvalid(w, 401, "webauthn", u.ID)
		return
	}
	raw, err := json.Marshal(credential)
	if err != nil {
		mfaError(w, "webauthn", u.ID, err)
		return
	}
	err = s.auth.Store.UpdateWebAuthnCredential(r.Context(), u.ID, base64.RawURLEncoding.EncodeToString(credential.ID), string(raw), auth.Hash(opened.Session.Challenge), opened.ExpiresAt, s.auth.Now().Unix())
	switch {
	case err == nil:
	case errors.Is(err, store.ErrMFAChallengeUsed), errors.Is(err, store.ErrNotFound):
		// The response was sent before, or the credential was removed
		// meanwhile.
		mfaCodeInvalid(w, 401, "webauthn", u.ID)
		return
	case errors.Is(err, store.ErrUnauthorized):
		mfaTokenInvalid(w)
		return
	default:
		mfaError(w, "webauthn", u.ID, err)
		return
	}
	s.completeMFASignIn(w, r, u, nil)
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
	u, ok := s.mfaUser(w, r, in.Token)
	if !ok || !s.mfaEnrollable(w, r, u) || !s.requireWebAuthn(w) {
		return
	}
	// mfaEnrollable saw no factor, so there is nothing to exclude.
	s.beginWebAuthnRegistration(w, u, store.MFAStatus{})
}

// handleMFAEnrollWebAuthnFinish stores the passkey or security key of a
// user that has no second factor yet and must have one, issues the recovery
// codes and signs the user in. The attempts are not limited per user: the
// response holds no secret that could be guessed.
func (s *Server) handleMFAEnrollWebAuthnFinish(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Token      string          `json:"mfa_token"`
		State      string          `json:"state"`
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
	u, ok := s.mfaUser(w, r, in.Token)
	if !ok || !s.mfaEnrollable(w, r, u) || !s.requireWebAuthn(w) {
		return
	}
	credential, ok := s.confirmWebAuthnRegistration(w, u, in.State, in.Credential, name, 401)
	if !ok {
		return
	}
	codes, rows, err := newRecoveryCodes()
	if err != nil {
		mfaError(w, "webauthn enroll", u.ID, err)
		return
	}
	// The store checks again, under the lock of the user row, that the user
	// has no factor: of two concurrent enrollments, one wins.
	_, err = s.auth.Store.EnrollWebAuthn(r.Context(), credential, rows, true)
	switch {
	case err == nil:
	case webauthnEnrollError(w, err):
		return
	case errors.Is(err, store.ErrUnauthorized):
		mfaTokenInvalid(w)
		return
	default:
		mfaError(w, "webauthn enroll", u.ID, err)
		return
	}
	log.Printf("auth: mfa: webauthn enrolled user=%s", u.ID)
	s.completeMFASignIn(w, r, u, codes)
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
	if len(status.WebAuthn) >= store.MaxWebAuthnCredentials {
		webauthnEnrollError(w, store.ErrMFALimit)
		return
	}
	s.beginWebAuthnRegistration(w, u, status)
}

// handleFinishWebAuthn stores the passkey or security key the authenticator
// created. The state stands for the password check of the begin request.
// The first factor of the user issues the recovery codes.
func (s *Server) handleFinishWebAuthn(w http.ResponseWriter, r *http.Request) {
	u, ok := userFrom(r.Context())
	if !ok {
		s.authError(w, r, store.ErrUnauthorized)
		return
	}
	var in struct {
		State      string          `json:"state"`
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
	credential, ok := s.confirmWebAuthnRegistration(w, u, in.State, in.Credential, name, 400)
	if !ok {
		return
	}
	codes, rows, err := newRecoveryCodes()
	if err != nil {
		mfaError(w, "webauthn enroll", u.ID, err)
		return
	}
	issued, err := s.auth.Store.EnrollWebAuthn(r.Context(), credential, rows, false)
	switch {
	case err == nil:
	case webauthnEnrollError(w, err):
		return
	case errors.Is(err, store.ErrUnauthorized):
		s.authError(w, r, err)
		return
	default:
		mfaError(w, "webauthn enroll", u.ID, err)
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
