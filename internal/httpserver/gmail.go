package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/i2bskn/astovia/internal/auth"
	"github.com/i2bskn/astovia/internal/config"
	"github.com/i2bskn/astovia/internal/gmailsync"
	"github.com/i2bskn/astovia/internal/google"
	"github.com/i2bskn/astovia/internal/store"
)

const (
	// googleTimeout bounds each request to Google while connecting an
	// account, and revokeTimeout the revocation when one is removed.
	googleTimeout = 10 * time.Second
	revokeTimeout = 5 * time.Second
	// authStateTTL is how long a Google authorization can be completed.
	authStateTTL = 10 * time.Minute
)

var (
	// gmailAccountIDPattern is the ID of a Google account row (auth.Random).
	gmailAccountIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)
	// googleStatePattern is the state of a Google authorization.
	googleStatePattern = regexp.MustCompile(`^gst_[A-Za-z0-9_-]{43}$`)
	// googleErrorPattern is an OAuth error code.
	googleErrorPattern = regexp.MustCompile(`^[a-z_]{1,100}$`)
	// gmailAddressPattern is the address of a Google account accepted as
	// it is: no colon, so that it can be part of a source key.
	gmailAddressPattern = regexp.MustCompile(`^[A-Za-z0-9._%+-]{1,64}@[A-Za-z0-9.-]{1,253}$`)
)

// validGoogleCode accepts an authorization code of 1-2048 printable ASCII
// characters without spaces.
func validGoogleCode(v string) bool {
	if len(v) < 1 || len(v) > 2048 {
		return false
	}
	for i := 0; i < len(v); i++ {
		if v[i] < 0x21 || v[i] > 0x7e {
			return false
		}
	}
	return true
}

// VerifierPurpose is the secretbox purpose of the PKCE code verifier of a
// user's Google authorization in progress.
func VerifierPurpose(userID string) string { return "google-auth:" + userID }

// gmailAccountResponse is a connected Google account. The token is never
// included.
type gmailAccountResponse struct {
	ID              string  `json:"id"`
	Email           string  `json:"email"`
	ImmediateNotify bool    `json:"immediate_notify"`
	Status          string  `json:"status"`
	LastError       *string `json:"last_error"`
	LastSyncedAt    *string `json:"last_synced_at"`
	CreatedAt       string  `json:"created_at"`
}

// gmailResponse is the state of the mail integration. Available is false
// when the operator set no OAuth client; the accounts are listed anyway so
// that they can be changed and removed.
type gmailResponse struct {
	Available   bool                   `json:"available"`
	RedirectURI string                 `json:"redirect_uri"`
	MaxAccounts int                    `json:"max_accounts"`
	Accounts    []gmailAccountResponse `json:"accounts"`
}

func toGmailAccountResponse(a store.GmailAccount) gmailAccountResponse {
	out := gmailAccountResponse{ID: a.ID, Email: a.Email, ImmediateNotify: a.ImmediateNotify == 1, Status: a.Status, CreatedAt: formatTime(a.CreatedAt)}
	if a.LastError.Valid {
		out.LastError = &a.LastError.String
	}
	if a.LastSyncedAt.Valid {
		v := formatTime(a.LastSyncedAt.Int64)
		out.LastSyncedAt = &v
	}
	return out
}

// googleClient returns the OAuth client of the instance, or nil.
func (s *Server) googleClient() *config.GoogleClient { return s.cfg.Google }

func (s *Server) handleGetGmail(w http.ResponseWriter, r *http.Request) {
	u, ok := userFrom(r.Context())
	if !ok {
		s.authError(w, r, store.ErrUnauthorized)
		return
	}
	list, err := s.auth.Store.GmailAccounts(r.Context(), u.ID)
	if err != nil {
		integrationError(w, "gmail get", u.ID, err)
		return
	}
	out := gmailResponse{Available: s.googleClient() != nil, RedirectURI: s.cfg.PublicURL + config.GoogleCallbackPath, MaxAccounts: store.MaxGmailAccounts, Accounts: []gmailAccountResponse{}}
	for _, a := range list {
		out.Accounts = append(out.Accounts, toGmailAccountResponse(a))
	}
	writeJSON(w, 200, out)
}

// handleAuthorizeGmail starts a Google authorization: it stores the state
// (bound to the user, not to the session) and the PKCE verifier and
// returns the URL of Google's consent screen. account_id connects that
// account again, with its address as the login hint.
func (s *Server) handleAuthorizeGmail(w http.ResponseWriter, r *http.Request) {
	u, ok := userFrom(r.Context())
	if !ok {
		s.authError(w, r, store.ErrUnauthorized)
		return
	}
	sid, ok := sessionFrom(r.Context())
	if !ok {
		s.authError(w, r, store.ErrUnauthorized)
		return
	}
	var in struct {
		AccountID json.RawMessage `json:"account_id"`
	}
	if !decode(w, r, &in, integrationBodyLimit) {
		return
	}
	client := s.googleClient()
	if client == nil {
		fail(w, 409, "gmail_unavailable", "The mail integration is not configured by the operator.")
		return
	}
	loginHint := ""
	if in.AccountID != nil {
		id, ok := decodeString(in.AccountID)
		if !ok {
			fail(w, 400, "invalid_request", "account_id must be a string.")
			return
		}
		if !gmailAccountIDPattern.MatchString(id) {
			fail(w, 404, "not_found", "The account was not found.")
			return
		}
		a, err := s.auth.Store.GmailAccount(r.Context(), u.ID, id)
		if errors.Is(err, store.ErrConnectionNotFound) {
			fail(w, 404, "not_found", "The account was not found.")
			return
		}
		if err != nil {
			integrationError(w, "gmail authorize", u.ID, err)
			return
		}
		loginHint = a.Email
	} else {
		list, err := s.auth.Store.GmailAccounts(r.Context(), u.ID)
		if err != nil {
			integrationError(w, "gmail authorize", u.ID, err)
			return
		}
		if len(list) >= store.MaxGmailAccounts {
			fail(w, 409, "gmail_too_many", "You can connect at most 10 Google accounts.")
			return
		}
	}
	random, err := auth.Random()
	if err != nil {
		integrationError(w, "gmail authorize", u.ID, err)
		return
	}
	state := "gst_" + random
	verifier, err := auth.Random()
	if err != nil {
		integrationError(w, "gmail authorize", u.ID, err)
		return
	}
	sealed, err := s.box.Seal(VerifierPurpose(u.ID), verifier)
	if err != nil {
		integrationError(w, "gmail seal", u.ID, err)
		return
	}
	err = s.auth.Store.CreateAuthState(r.Context(), u.ID, sid, auth.Hash(state), sealed, s.auth.Now().Unix(), authStateTTL)
	if errors.Is(err, store.ErrUnauthorized) {
		s.authError(w, r, err)
		return
	}
	if err != nil {
		integrationError(w, "gmail authorize", u.ID, err)
		return
	}
	writeJSON(w, 200, map[string]string{"url": s.google.AuthURL(client.ID, client.RedirectURI, state, google.Challenge(verifier), loginHint)})
}

// handleCompleteGmail completes a Google authorization with the state and
// the code (or the error) Google sent to the callback. The state is
// consumed only when it is the signed-in user's, so that an authorization
// started by someone else cannot connect their mail to this user. The
// tokens received are never revoked on failure: a revocation removes every
// grant of the Google account, including the connections that work.
func (s *Server) handleCompleteGmail(w http.ResponseWriter, r *http.Request) {
	u, ok := userFrom(r.Context())
	if !ok {
		s.authError(w, r, store.ErrUnauthorized)
		return
	}
	sid, ok := sessionFrom(r.Context())
	if !ok {
		s.authError(w, r, store.ErrUnauthorized)
		return
	}
	var in struct {
		State json.RawMessage `json:"state"`
		Code  json.RawMessage `json:"code"`
		Error json.RawMessage `json:"error"`
	}
	if !decode(w, r, &in, integrationBodyLimit) {
		return
	}
	// The input is checked before the state is consumed.
	state, okState := decodeString(in.State)
	var code, denial string
	okCode, okError := false, false
	if in.Code != nil {
		code, okCode = decodeString(in.Code)
		okCode = okCode && validGoogleCode(code)
	}
	if in.Error != nil {
		denial, okError = decodeString(in.Error)
		okError = okError && googleErrorPattern.MatchString(denial)
	}
	if in.State == nil || !okState || !googleStatePattern.MatchString(state) || (in.Code != nil) == (in.Error != nil) || (in.Code != nil && !okCode) || (in.Error != nil && !okError) {
		fail(w, 400, "invalid_request", "state and either code or error are required.")
		return
	}
	client := s.googleClient()
	if client == nil {
		fail(w, 409, "gmail_unavailable", "The mail integration is not configured by the operator.")
		return
	}
	sealed, err := s.auth.Store.ConsumeAuthState(r.Context(), u.ID, sid, auth.Hash(state), s.auth.Now().Unix())
	switch {
	case err == nil:
	case errors.Is(err, store.ErrAuthStateNotFound):
		fail(w, 400, "gmail_state_invalid", "The connection request expired or was already used. Try again.")
		return
	case errors.Is(err, store.ErrUnauthorized):
		s.authError(w, r, err)
		return
	default:
		integrationError(w, "gmail complete", u.ID, err)
		return
	}
	if in.Error != nil {
		fail(w, 400, "gmail_denied", "Google did not allow the connection.")
		return
	}
	verifier, err := s.box.Open(VerifierPurpose(u.ID), sealed)
	if err != nil {
		integrationError(w, "gmail complete", u.ID, errors.New("verifier_undecryptable"))
		return
	}
	// Google is called before, and outside, the store transaction.
	ctx, cancel := context.WithTimeout(r.Context(), googleTimeout)
	tok, err := s.google.Exchange(ctx, client.ID, client.Secret, client.RedirectURI, code, verifier)
	cancel()
	switch {
	case err == nil:
	case errors.Is(err, google.ErrPolicyBlocked):
		fail(w, 403, "gmail_policy_blocked", "The Google Workspace administrator has not allowed this app.")
		return
	default:
		log.Printf("integrations: gmail exchange user=%s: %v", u.ID, err)
		fail(w, 502, "gmail_exchange_failed", "Google could not complete the connection. Try again.")
		return
	}
	if !tok.HasScope(google.ScopeGmailReadonly) {
		fail(w, 400, "gmail_scope_missing", "Allow reading your mail to connect.")
		return
	}
	ctx, cancel = context.WithTimeout(r.Context(), googleTimeout)
	email, err := s.google.Profile(ctx, tok.AccessToken)
	cancel()
	switch {
	case err == nil:
	case errors.Is(err, google.ErrAPIDisabled):
		fail(w, 502, "gmail_api_disabled", "The Gmail API is not enabled for the OAuth client.")
		return
	case errors.Is(err, google.ErrPolicyBlocked):
		fail(w, 403, "gmail_policy_blocked", "The Google Workspace administrator has not allowed this app.")
		return
	default:
		log.Printf("integrations: gmail profile user=%s: %v", u.ID, err)
		fail(w, 502, "gmail_exchange_failed", "Google could not complete the connection. Try again.")
		return
	}
	if !gmailAddressPattern.MatchString(email) {
		log.Printf("integrations: gmail profile user=%s: unexpected address", u.ID)
		fail(w, 502, "gmail_exchange_failed", "Google could not complete the connection. Try again.")
		return
	}
	email = strings.ToLower(email)
	a, err := s.auth.Store.SaveGmailAccount(r.Context(), u.ID, sid, email, func(id string) (string, error) {
		return s.box.Seal(gmailsync.TokenPurpose(u.ID, id), tok.RefreshToken)
	}, s.auth.Now().Unix())
	switch {
	case err == nil:
	case errors.Is(err, store.ErrAccountInUse):
		fail(w, 409, "gmail_in_use", "This Google account is connected by another user.")
		return
	case errors.Is(err, store.ErrTooManyAccounts):
		fail(w, 409, "gmail_too_many", "You can connect at most 10 Google accounts.")
		return
	case errors.Is(err, store.ErrUnauthorized):
		s.authError(w, r, err)
		return
	default:
		log.Printf("integrations: gmail save user=%s: %v", u.ID, err)
		fail(w, 500, "internal_error", "The account could not be saved. Try again.")
		return
	}
	writeJSON(w, 200, toGmailAccountResponse(a))
}

func (s *Server) handlePatchGmailAccount(w http.ResponseWriter, r *http.Request) {
	u, ok := userFrom(r.Context())
	if !ok {
		s.authError(w, r, store.ErrUnauthorized)
		return
	}
	sid, ok := sessionFrom(r.Context())
	if !ok {
		s.authError(w, r, store.ErrUnauthorized)
		return
	}
	var in struct {
		ImmediateNotify json.RawMessage `json:"immediate_notify"`
	}
	if !decode(w, r, &in, integrationBodyLimit) {
		return
	}
	notify, ok := decodeBool(in.ImmediateNotify)
	if in.ImmediateNotify == nil || !ok {
		fail(w, 400, "invalid_request", "immediate_notify must be a boolean.")
		return
	}
	id := r.PathValue("id")
	if !gmailAccountIDPattern.MatchString(id) {
		fail(w, 404, "not_found", "The account was not found.")
		return
	}
	a, err := s.auth.Store.UpdateGmailSettings(r.Context(), u.ID, sid, id, notify, s.auth.Now().Unix())
	switch {
	case err == nil:
	case errors.Is(err, store.ErrUnauthorized):
		s.authError(w, r, err)
		return
	case errors.Is(err, store.ErrConnectionNotFound):
		fail(w, 404, "not_found", "The account was not found.")
		return
	default:
		integrationError(w, "gmail patch", u.ID, err)
		return
	}
	writeJSON(w, 200, toGmailAccountResponse(a))
}

// handleDeleteGmailAccount removes the account and then revokes its token
// at Google. A failed revocation is logged and does not fail the removal.
func (s *Server) handleDeleteGmailAccount(w http.ResponseWriter, r *http.Request) {
	u, ok := userFrom(r.Context())
	if !ok {
		s.authError(w, r, store.ErrUnauthorized)
		return
	}
	sid, ok := sessionFrom(r.Context())
	if !ok {
		s.authError(w, r, store.ErrUnauthorized)
		return
	}
	id := r.PathValue("id")
	if !gmailAccountIDPattern.MatchString(id) {
		fail(w, 404, "not_found", "The account was not found.")
		return
	}
	a, err := s.auth.Store.DeleteGmailAccount(r.Context(), u.ID, sid, id, s.auth.Now().Unix())
	switch {
	case err == nil:
	case errors.Is(err, store.ErrUnauthorized):
		s.authError(w, r, err)
		return
	case errors.Is(err, store.ErrConnectionNotFound):
		fail(w, 404, "not_found", "The account was not found.")
		return
	default:
		integrationError(w, "gmail delete", u.ID, err)
		return
	}
	// The revocation needs no OAuth client, so it is made without one too.
	token, err := s.box.Open(gmailsync.TokenPurpose(u.ID, a.ID), a.TokenSealed)
	if err != nil {
		log.Printf("integrations: gmail revoke user=%s account=%s: token_undecryptable", u.ID, a.ID)
	} else {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), revokeTimeout)
		if err = s.google.Revoke(ctx, token); err != nil {
			log.Printf("integrations: gmail revoke user=%s: %v", u.ID, err)
		}
		cancel()
	}
	w.WriteHeader(204)
}
