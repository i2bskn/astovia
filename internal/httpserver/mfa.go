package httpserver

import (
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/url"

	"github.com/i2bskn/astovia/internal/auth"
	"github.com/i2bskn/astovia/internal/mfa"
	"github.com/i2bskn/astovia/internal/store"
	"golang.org/x/crypto/bcrypt"
)

const (
	// mfaBodyLimit is the body limit of the MFA endpoints that carry no
	// WebAuthn response.
	mfaBodyLimit = 4096
	// maxMFATokenLength bounds the sealed MFA and setup tokens in bytes.
	maxMFATokenLength = 1024
	// mfaTokenPurpose is the purpose the MFA token is sealed with.
	mfaTokenPurpose = "mfa-login"
	// mfaTokenLifetime is the time allowed to give the second factor after
	// the password (10 minutes), and mfaEnrollTokenLifetime the time allowed
	// to enroll one first (30 minutes), in seconds.
	mfaTokenLifetime       = 10 * 60
	mfaEnrollTokenLifetime = 30 * 60
	// totpSetupPurpose is the prefix of the purpose the setup token of an
	// authenticator app is sealed with; the user ID follows. The same
	// purpose serves the enrollment before the first session and the
	// account screen: both begin after the password was checked.
	totpSetupPurpose = "mfa-totp-setup:"
	// totpSetupLifetime is the time allowed to confirm a new authenticator
	// app (30 minutes), in seconds.
	totpSetupLifetime = 30 * 60
	// totpSecretPurpose is the prefix of the purpose the stored secret of an
	// authenticator app is sealed with; the user ID follows.
	totpSecretPurpose = "mfa-totp:"
)

// mfaMethodsResponse tells which second factors a user can give now.
type mfaMethodsResponse struct {
	TOTP     bool `json:"totp"`
	WebAuthn bool `json:"webauthn"`
	Recovery bool `json:"recovery"`
}

// mfaChallengeResponse is returned instead of a session when the password
// was right but a second factor is needed. Token identifies the user at the
// /auth/mfa/* endpoints. Enrolled is false for a user that must enroll a
// factor before signing in; the methods are all false then.
type mfaChallengeResponse struct {
	Token             string             `json:"token"`
	Enrolled          bool               `json:"enrolled"`
	Methods           mfaMethodsResponse `json:"methods"`
	WebAuthnAvailable bool               `json:"webauthn_available"`
}

type mfaPendingResponse struct {
	MFA mfaChallengeResponse `json:"mfa"`
}

// mfaAuthResponse is the login response of an enrollment before the first
// session: the session and the recovery codes, which are shown only once.
type mfaAuthResponse struct {
	authResponse
	RecoveryCodes []string `json:"recovery_codes"`
}

// totpSetupResponse starts the enrollment of an authenticator app. Secret
// is shown for typing and OtpauthURL as a QR code; SetupToken carries the
// secret, sealed, back to the finish request.
type totpSetupResponse struct {
	Secret     string `json:"secret"`
	OtpauthURL string `json:"otpauth_url"`
	SetupToken string `json:"setup_token"`
}

type mfaTOTPResponse struct {
	CreatedAt string `json:"created_at"`
}

type mfaCredentialResponse struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	CreatedAt  string  `json:"created_at"`
	LastUsedAt *string `json:"last_used_at"`
}

// mfaStatusResponse is the two-step verification of the signed-in user.
type mfaStatusResponse struct {
	Required               bool                    `json:"required"`
	TOTP                   *mfaTOTPResponse        `json:"totp"`
	WebAuthn               []mfaCredentialResponse `json:"webauthn"`
	RecoveryCodesRemaining int64                   `json:"recovery_codes_remaining"`
	WebAuthnAvailable      bool                    `json:"webauthn_available"`
}

// recoveryCodesResponse carries new recovery codes, which the server
// returns only once. It is null when none were issued.
type recoveryCodesResponse struct {
	RecoveryCodes []string `json:"recovery_codes"`
}

// mfaToken is the content of the MFA token: the user, the SHA-256 of the
// password hash the password was checked against and the expiry (Unix
// seconds). A password change makes the token invalid.
type mfaToken struct {
	UserID       string `json:"u"`
	PasswordHash string `json:"h"`
	ExpiresAt    int64  `json:"e"`
}

// totpSetup is the content of the setup token of an authenticator app: the
// base32 secret and the expiry (Unix seconds).
type totpSetup struct {
	Secret    string `json:"s"`
	ExpiresAt int64  `json:"e"`
}

// mfaRequired reports whether u must have a second factor
// (ASTOVIA_MFA_REQUIRED).
func (s *Server) mfaRequired(u store.User) bool {
	return mfa.Required(s.cfg.MFARequired, u.Role == store.RoleOwner)
}

// mfaError writes the response for an unexpected error of the MFA
// endpoints. The log never contains tokens, codes or secrets.
func mfaError(w http.ResponseWriter, op, userID string, err error) {
	log.Printf("auth: mfa: %s user=%s: %v", op, userID, err)
	fail(w, 503, "unavailable", "The service is temporarily unavailable. Try again.")
}

func mfaTokenInvalid(w http.ResponseWriter) {
	fail(w, 401, "mfa_token_invalid", "The sign-in has expired. Sign in again.")
}

// mfaCodeInvalid answers a wrong code and logs the failure with the user
// ID alone. status is 401 before the session and 400 in /api/v1, where a
// 401 would make the web UI refresh its session and send the code again.
func mfaCodeInvalid(w http.ResponseWriter, status int, method, userID string) {
	log.Printf("auth: mfa: %s failed user=%s", method, userID)
	fail(w, status, "mfa_code_invalid", "The code is not correct.")
}

func mfaStateInvalid(w http.ResponseWriter) {
	fail(w, 400, "mfa_state_invalid", "The enrollment has expired. Start again.")
}

// passwordSignIn is the gate every password sign-in passes after the user
// was verified: by the password (handleLogin) or by an invitation or
// password reset link (signInAfterLink). It starts a session only for a
// user that has no second factor and need not have one. Any other user
// gets an MFA token instead, and no refresh cookie; the session is started
// by completeMFASignIn once the second factor was given or enrolled.
// failed writes the response for an error.
func (s *Server) passwordSignIn(w http.ResponseWriter, r *http.Request, u store.User, failed func(error)) {
	status, err := s.auth.Store.MFAStatus(r.Context(), u.ID)
	if err != nil {
		failed(err)
		return
	}
	enrolled := status.Enrolled()
	if !enrolled && !s.mfaRequired(u) {
		result, err := s.auth.LoginUser(r.Context(), u)
		if err != nil {
			failed(err)
			return
		}
		s.writeAuthResult(w, r, result)
		return
	}
	out := mfaChallengeResponse{Enrolled: enrolled, WebAuthnAvailable: s.webauthnAvailable()}
	lifetime := int64(mfaEnrollTokenLifetime)
	if enrolled {
		lifetime = mfaTokenLifetime
		out.Methods = mfaMethodsResponse{TOTP: status.TOTP != nil, WebAuthn: s.webauthnAvailable() && len(status.WebAuthn) > 0, Recovery: status.RecoveryCodes > 0}
	}
	raw, err := json.Marshal(mfaToken{UserID: u.ID, PasswordHash: auth.Hash(u.PasswordHash), ExpiresAt: s.auth.Now().Unix() + lifetime})
	if err != nil {
		failed(err)
		return
	}
	if out.Token, err = s.box.Seal(mfaTokenPurpose, string(raw)); err != nil {
		failed(err)
		return
	}
	writeJSON(w, 200, mfaPendingResponse{MFA: out})
}

// completeMFASignIn starts the session of a password sign-in after the
// second factor of u was verified or enrolled. u is the row read by
// mfaUser. codes are the recovery codes of an enrollment, or nil. Nothing
// else on the password sign-in paths starts a session, apart from
// passwordSignIn.
//
// A failure is a 503 signin_unavailable whatever its cause: the code was
// spent or the enrollment is committed, so the user signs in again.
func (s *Server) completeMFASignIn(w http.ResponseWriter, r *http.Request, u store.User, codes []string) {
	result, err := s.auth.LoginUser(r.Context(), u)
	if err != nil {
		log.Printf("auth: mfa: sign in user=%s: %v", u.ID, err)
		fail(w, 503, "signin_unavailable", "The sign-in could not be completed. Sign in again.")
		return
	}
	if codes == nil {
		s.writeAuthResult(w, r, result)
		return
	}
	user := s.toUserResponse(r.Context(), result.User)
	s.setRefreshCookie(w, result)
	writeJSON(w, 200, mfaAuthResponse{authResponse: authResponse{AccessToken: result.AccessToken, ExpiresIn: result.ExpiresIn, User: user}, RecoveryCodes: codes})
}

// mfaPeerLimit limits the /auth/mfa/* endpoints together per peer, like
// the login. It runs after passwordAuth, so that nothing is counted with
// OIDC sign-in.
func (s *Server) mfaPeerLimit(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.mfaPeerLimiter.allow(r.RemoteAddr) {
			w.Header().Set("Retry-After", "60")
			fail(w, 429, "rate_limited", "Too many attempts. Try again later.")
			return
		}
		next(w, r)
	})
}

// allowMFAAttempt counts an attempt to give a second factor code of
// userID, successful or not, and writes 429 over the limit. The limit is
// what keeps a holder of the password from guessing the codes.
func (s *Server) allowMFAAttempt(w http.ResponseWriter, userID string) bool {
	if s.mfaLimiter.allowKey("mfa:" + userID) {
		return true
	}
	w.Header().Set("Retry-After", "900")
	fail(w, 429, "rate_limited", "Too many attempts. Try again later.")
	return false
}

// mfaUser returns the user of an MFA token, read again from the database.
// It writes 401 mfa_token_invalid unless the token opens, has not expired,
// and its user exists, is enabled and still has the password hash the
// password was checked against.
func (s *Server) mfaUser(w http.ResponseWriter, r *http.Request, token string) (store.User, bool) {
	var t mfaToken
	if len(token) == 0 || len(token) > maxMFATokenLength {
		mfaTokenInvalid(w)
		return store.User{}, false
	}
	raw, err := s.box.Open(mfaTokenPurpose, token)
	if err != nil || json.Unmarshal([]byte(raw), &t) != nil || t.UserID == "" || s.auth.Now().Unix() >= t.ExpiresAt {
		mfaTokenInvalid(w)
		return store.User{}, false
	}
	u, err := s.auth.Store.UserByID(r.Context(), t.UserID)
	if errors.Is(err, sql.ErrNoRows) {
		mfaTokenInvalid(w)
		return store.User{}, false
	}
	if err != nil {
		mfaError(w, "token", t.UserID, err)
		return store.User{}, false
	}
	if u.Disabled != 0 || subtle.ConstantTimeCompare([]byte(auth.Hash(u.PasswordHash)), []byte(t.PasswordHash)) != 1 {
		mfaTokenInvalid(w)
		return store.User{}, false
	}
	return u, true
}

// mfaEnrollable checks that u may enroll a second factor before its first
// session: it has none yet and must have one. Otherwise the holder of the
// password alone could add a factor of their own to an account that is
// protected by one (409), or to one that asked for none (403).
func (s *Server) mfaEnrollable(w http.ResponseWriter, r *http.Request, u store.User) bool {
	status, err := s.auth.Store.MFAStatus(r.Context(), u.ID)
	if err != nil {
		mfaError(w, "enroll", u.ID, err)
		return false
	}
	if status.Enrolled() {
		fail(w, 409, "mfa_already_enrolled", "Two-step verification is already set up. Sign in with it.")
		return false
	}
	if !s.mfaRequired(u) {
		fail(w, 403, "mfa_not_required", "Set up two-step verification from the account screen after signing in.")
		return false
	}
	return true
}

// beginTOTP answers a new secret for u with its otpauth URL and the setup
// token that carries it to the finish request. Nothing is stored.
func (s *Server) beginTOTP(w http.ResponseWriter, u store.User) {
	secret, err := mfa.NewTOTPSecret()
	if err != nil {
		mfaError(w, "totp begin", u.ID, err)
		return
	}
	raw, err := json.Marshal(totpSetup{Secret: secret, ExpiresAt: s.auth.Now().Unix() + totpSetupLifetime})
	if err != nil {
		mfaError(w, "totp begin", u.ID, err)
		return
	}
	token, err := s.box.Seal(totpSetupPurpose+u.ID, string(raw))
	if err != nil {
		mfaError(w, "totp begin", u.ID, err)
		return
	}
	host := ""
	if public, err := url.Parse(s.cfg.PublicURL); err == nil {
		host = public.Hostname()
	}
	writeJSON(w, 200, totpSetupResponse{Secret: secret, OtpauthURL: mfa.TOTPURL(secret, host, u.Email), SetupToken: token})
}

// confirmTOTPSetup opens the setup token of u and checks code against its
// secret. It returns the authenticator app to store, with the secret
// sealed and the time step of code as the last accepted one. Otherwise it
// writes 400 mfa_state_invalid for a token that does not open for this
// user or has expired, and codeStatus mfa_code_invalid for a wrong code.
func (s *Server) confirmTOTPSetup(w http.ResponseWriter, u store.User, setupToken, code string, codeStatus int) (store.MFATOTP, bool) {
	var setup totpSetup
	now := s.auth.Now()
	if len(setupToken) == 0 || len(setupToken) > maxMFATokenLength {
		mfaStateInvalid(w)
		return store.MFATOTP{}, false
	}
	raw, err := s.box.Open(totpSetupPurpose+u.ID, setupToken)
	if err != nil || json.Unmarshal([]byte(raw), &setup) != nil || setup.Secret == "" || now.Unix() >= setup.ExpiresAt {
		mfaStateInvalid(w)
		return store.MFATOTP{}, false
	}
	normalized, ok := mfa.NormalizeTOTPCode(code)
	if !ok {
		mfaCodeInvalid(w, codeStatus, "totp", u.ID)
		return store.MFATOTP{}, false
	}
	step, ok := mfa.MatchTOTP(setup.Secret, normalized, now, 0)
	if !ok {
		mfaCodeInvalid(w, codeStatus, "totp", u.ID)
		return store.MFATOTP{}, false
	}
	sealed, err := s.box.Seal(totpSecretPurpose+u.ID, setup.Secret)
	if err != nil {
		mfaError(w, "totp finish", u.ID, err)
		return store.MFATOTP{}, false
	}
	return store.MFATOTP{UserID: u.ID, SecretSealed: sealed, LastStep: step, CreatedAt: now.Unix()}, true
}

// newRecoveryCodes returns new recovery codes as shown to the user and as
// stored.
func newRecoveryCodes() ([]string, []store.RecoveryCode, error) {
	codes, err := mfa.NewRecoveryCodes()
	if err != nil {
		return nil, nil, err
	}
	rows := make([]store.RecoveryCode, 0, len(codes))
	for _, code := range codes {
		normalized, ok := mfa.NormalizeRecoveryCode(code)
		if !ok {
			return nil, nil, errors.New("generated recovery code is malformed")
		}
		id, err := auth.Random()
		if err != nil {
			return nil, nil, err
		}
		rows = append(rows, store.RecoveryCode{ID: id, Hash: mfa.RecoveryCodeHash(normalized)})
	}
	return codes, rows, nil
}

// handleMFATOTP signs in with the code of the authenticator app.
func (s *Server) handleMFATOTP(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Token string `json:"mfa_token"`
		Code  string `json:"code"`
	}
	if !decode(w, r, &in, mfaBodyLimit) {
		return
	}
	u, ok := s.mfaUser(w, r, in.Token)
	if !ok || !s.allowMFAAttempt(w, u.ID) {
		return
	}
	status, err := s.auth.Store.MFAStatus(r.Context(), u.ID)
	if err != nil {
		mfaError(w, "totp", u.ID, err)
		return
	}
	code, ok := mfa.NormalizeTOTPCode(in.Code)
	if status.TOTP == nil || !ok {
		mfaCodeInvalid(w, 401, "totp", u.ID)
		return
	}
	secret, err := s.box.Open(totpSecretPurpose+u.ID, status.TOTP.SecretSealed)
	if err != nil {
		// ASTOVIA_JWT_SECRET changed: the answer is that of a wrong code.
		log.Printf("auth: mfa: totp secret of user %s cannot be decrypted", u.ID)
		mfaCodeInvalid(w, 401, "totp", u.ID)
		return
	}
	step, ok := mfa.MatchTOTP(secret, code, s.auth.Now(), status.TOTP.LastStep)
	if !ok {
		mfaCodeInvalid(w, 401, "totp", u.ID)
		return
	}
	// The step is spent here; of two requests with the same code, one wins.
	used, err := s.auth.Store.UseTOTPStep(r.Context(), u.ID, step)
	if err != nil {
		mfaError(w, "totp", u.ID, err)
		return
	}
	if !used {
		mfaCodeInvalid(w, 401, "totp", u.ID)
		return
	}
	s.completeMFASignIn(w, r, u, nil)
}

// handleMFARecovery signs in with a recovery code, which is spent. A code
// spent before a failure to start the session is lost.
func (s *Server) handleMFARecovery(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Token string `json:"mfa_token"`
		Code  string `json:"code"`
	}
	if !decode(w, r, &in, mfaBodyLimit) {
		return
	}
	u, ok := s.mfaUser(w, r, in.Token)
	if !ok || !s.allowMFAAttempt(w, u.ID) {
		return
	}
	code, ok := mfa.NormalizeRecoveryCode(in.Code)
	if !ok {
		mfaCodeInvalid(w, 401, "recovery", u.ID)
		return
	}
	used, err := s.auth.Store.UseRecoveryCode(r.Context(), u.ID, mfa.RecoveryCodeHash(code), s.auth.Now().Unix())
	if err != nil {
		mfaError(w, "recovery", u.ID, err)
		return
	}
	if !used {
		mfaCodeInvalid(w, 401, "recovery", u.ID)
		return
	}
	s.completeMFASignIn(w, r, u, nil)
}

// handleMFAEnrollTOTPBegin starts the enrollment of an authenticator app
// before the first session.
func (s *Server) handleMFAEnrollTOTPBegin(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Token string `json:"mfa_token"`
	}
	if !decode(w, r, &in, mfaBodyLimit) {
		return
	}
	u, ok := s.mfaUser(w, r, in.Token)
	if !ok || !s.mfaEnrollable(w, r, u) {
		return
	}
	s.beginTOTP(w, u)
}

// handleMFAEnrollTOTPFinish stores the authenticator app of a user that has
// no second factor yet and must have one, issues the recovery codes and
// signs the user in.
func (s *Server) handleMFAEnrollTOTPFinish(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Token      string `json:"mfa_token"`
		SetupToken string `json:"setup_token"`
		Code       string `json:"code"`
	}
	if !decode(w, r, &in, mfaBodyLimit) {
		return
	}
	u, ok := s.mfaUser(w, r, in.Token)
	if !ok || !s.allowMFAAttempt(w, u.ID) || !s.mfaEnrollable(w, r, u) {
		return
	}
	totp, ok := s.confirmTOTPSetup(w, u, in.SetupToken, in.Code, 401)
	if !ok {
		return
	}
	codes, rows, err := newRecoveryCodes()
	if err != nil {
		mfaError(w, "totp enroll", u.ID, err)
		return
	}
	// The store checks again, under the lock of the user row, that the user
	// has no factor: of two concurrent enrollments, one wins.
	_, err = s.auth.Store.EnrollTOTP(r.Context(), totp, rows, true)
	switch {
	case err == nil:
	case errors.Is(err, store.ErrMFAEnrolled):
		fail(w, 409, "mfa_already_enrolled", "Two-step verification is already set up. Sign in with it.")
		return
	case errors.Is(err, store.ErrUnauthorized):
		mfaTokenInvalid(w)
		return
	default:
		mfaError(w, "totp enroll", u.ID, err)
		return
	}
	log.Printf("auth: mfa: totp enrolled user=%s", u.ID)
	s.completeMFASignIn(w, r, u, codes)
}

// confirmPassword checks the current password of the signed-in user, which
// the account screen asks for before it changes the second factors. The
// user row is read again. It writes 400 password_invalid for a wrong
// password; attempts, successful or not, are limited per user before any
// bcrypt work.
func (s *Server) confirmPassword(w http.ResponseWriter, r *http.Request, u store.User, password string) bool {
	if len(password) < 1 || len(password) > 72 {
		fail(w, 400, "invalid_request", "password must contain 1-72 bytes.")
		return false
	}
	if !s.mfaPasswordLimiter.allowKey("mfa-password:" + u.ID) {
		w.Header().Set("Retry-After", "300")
		fail(w, 429, "rate_limited", "Too many attempts. Try again later.")
		return false
	}
	current, err := s.auth.Store.UserByID(r.Context(), u.ID)
	if errors.Is(err, sql.ErrNoRows) {
		s.authError(w, r, store.ErrUnauthorized)
		return false
	}
	if err != nil {
		mfaError(w, "confirm password", u.ID, err)
		return false
	}
	if current.Disabled != 0 {
		s.authError(w, r, store.ErrUnauthorized)
		return false
	}
	if bcrypt.CompareHashAndPassword([]byte(current.PasswordHash), []byte(password)) != nil {
		fail(w, 400, "password_invalid", "The password is not correct.")
		return false
	}
	return true
}

func toMFAStatusResponse(status store.MFAStatus) mfaStatusResponse {
	out := mfaStatusResponse{WebAuthn: make([]mfaCredentialResponse, 0, len(status.WebAuthn)), RecoveryCodesRemaining: status.RecoveryCodes}
	if status.TOTP != nil {
		out.TOTP = &mfaTOTPResponse{CreatedAt: formatTime(status.TOTP.CreatedAt)}
	}
	for _, c := range status.WebAuthn {
		item := mfaCredentialResponse{ID: c.ID, Name: c.Name, CreatedAt: formatTime(c.CreatedAt)}
		if c.LastUsedAt.Valid {
			v := formatTime(c.LastUsedAt.Int64)
			item.LastUsedAt = &v
		}
		out.WebAuthn = append(out.WebAuthn, item)
	}
	return out
}

func (s *Server) handleGetMFA(w http.ResponseWriter, r *http.Request) {
	u, ok := userFrom(r.Context())
	if !ok {
		s.authError(w, r, store.ErrUnauthorized)
		return
	}
	status, err := s.auth.Store.MFAStatus(r.Context(), u.ID)
	if err != nil {
		mfaError(w, "get", u.ID, err)
		return
	}
	out := toMFAStatusResponse(status)
	out.Required, out.WebAuthnAvailable = s.mfaRequired(u), s.webauthnAvailable()
	writeJSON(w, 200, out)
}

// handleBeginTOTP starts the enrollment of an authenticator app from the
// account screen, or its replacement when the user has one.
func (s *Server) handleBeginTOTP(w http.ResponseWriter, r *http.Request) {
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
	s.beginTOTP(w, u)
}

// handleFinishTOTP stores the authenticator app confirmed with a code. The
// setup token stands for the password check of the begin request. The
// first factor of the user issues the recovery codes; a replacement keeps
// them.
func (s *Server) handleFinishTOTP(w http.ResponseWriter, r *http.Request) {
	u, ok := userFrom(r.Context())
	if !ok {
		s.authError(w, r, store.ErrUnauthorized)
		return
	}
	var in struct {
		SetupToken string `json:"setup_token"`
		Code       string `json:"code"`
	}
	if !decode(w, r, &in, mfaBodyLimit) || !s.allowMFAAttempt(w, u.ID) {
		return
	}
	totp, ok := s.confirmTOTPSetup(w, u, in.SetupToken, in.Code, 400)
	if !ok {
		return
	}
	codes, rows, err := newRecoveryCodes()
	if err != nil {
		mfaError(w, "totp enroll", u.ID, err)
		return
	}
	issued, err := s.auth.Store.EnrollTOTP(r.Context(), totp, rows, false)
	if errors.Is(err, store.ErrUnauthorized) {
		s.authError(w, r, err)
		return
	}
	if err != nil {
		mfaError(w, "totp enroll", u.ID, err)
		return
	}
	log.Printf("auth: mfa: totp enrolled user=%s", u.ID)
	if !issued {
		codes = nil
	}
	writeJSON(w, 200, recoveryCodesResponse{RecoveryCodes: codes})
}

// factorError writes the response for the errors of removing a second
// factor and reissuing the recovery codes, and reports whether err was one
// of them.
func (s *Server) factorError(w http.ResponseWriter, r *http.Request, err error) bool {
	switch {
	case errors.Is(err, store.ErrUnauthorized):
		s.authError(w, r, err)
	case errors.Is(err, store.ErrNotFound):
		fail(w, 404, "not_found", "Not found.")
	case errors.Is(err, store.ErrMFARequired):
		fail(w, 409, "mfa_required", "Two-step verification is required on this server; the last method cannot be removed.")
	case errors.Is(err, store.ErrMFANotEnrolled):
		fail(w, 409, "mfa_not_enrolled", "Two-step verification is not set up.")
	default:
		return false
	}
	return true
}

func (s *Server) handleDeleteTOTP(w http.ResponseWriter, r *http.Request) {
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
	if err := s.auth.Store.DeleteTOTP(r.Context(), u.ID, s.cfg.MFARequired); err != nil {
		if !s.factorError(w, r, err) {
			mfaError(w, "totp delete", u.ID, err)
		}
		return
	}
	log.Printf("auth: mfa: totp deleted user=%s", u.ID)
	w.WriteHeader(204)
}

// handleRecoveryCodes replaces the recovery codes of the signed-in user.
func (s *Server) handleRecoveryCodes(w http.ResponseWriter, r *http.Request) {
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
	codes, rows, err := newRecoveryCodes()
	if err != nil {
		mfaError(w, "recovery codes", u.ID, err)
		return
	}
	if err = s.auth.Store.ReplaceRecoveryCodes(r.Context(), u.ID, rows, s.auth.Now().Unix()); err != nil {
		if !s.factorError(w, r, err) {
			mfaError(w, "recovery codes", u.ID, err)
		}
		return
	}
	log.Printf("auth: mfa: recovery codes reissued user=%s", u.ID)
	writeJSON(w, 200, recoveryCodesResponse{RecoveryCodes: codes})
}
