package httpserver

import (
	"encoding/json"
	"errors"
	"github.com/i2bskn/astovia/internal/auth"
	"github.com/i2bskn/astovia/internal/store"
	"log"
	"net/http"
)

// accountBodyLimit is the body limit of the public invitation and password
// reset endpoints, the same as login.
const accountBodyLimit = 4096

// adminUserResponse is a user in the user management API.
type adminUserResponse struct {
	ID        string `json:"id"`
	Email     string `json:"email"`
	Role      string `json:"role"`
	Disabled  bool   `json:"disabled"`
	CreatedAt string `json:"created_at"`
}

type adminUserListResponse struct {
	Users []adminUserResponse `json:"users"`
}

type invitationResponse struct {
	ID             string `json:"id"`
	Email          string `json:"email"`
	Role           string `json:"role"`
	CreatedAt      string `json:"created_at"`
	ExpiresAt      string `json:"expires_at"`
	InvitedByEmail string `json:"invited_by_email"`
}

type invitationListResponse struct {
	Invitations []invitationResponse `json:"invitations"`
}

// createdInvitationResponse carries the link, whose token the server returns
// only once.
type createdInvitationResponse struct {
	ID        string `json:"id"`
	Email     string `json:"email"`
	Role      string `json:"role"`
	CreatedAt string `json:"created_at"`
	ExpiresAt string `json:"expires_at"`
	URL       string `json:"url"`
}

type createdPasswordResetResponse struct {
	URL       string `json:"url"`
	ExpiresAt string `json:"expires_at"`
}

type inspectedInvitationResponse struct {
	Email     string `json:"email"`
	Role      string `json:"role"`
	ExpiresAt string `json:"expires_at"`
}

type inspectedPasswordResetResponse struct {
	Email string `json:"email"`
}

func toAdminUserResponse(u store.User) adminUserResponse {
	return adminUserResponse{ID: u.ID, Email: u.Email, Role: u.Role, Disabled: u.Disabled != 0, CreatedAt: formatTime(u.CreatedAt)}
}

// usersError writes the response for an unexpected store error of the user
// management API. The log never contains tokens or passwords.
func usersError(w http.ResponseWriter, op, userID string, err error) {
	log.Printf("users: %s user=%s: %v", op, userID, err)
	fail(w, 503, "unavailable", "The service is temporarily unavailable. Try again.")
}

// ownerError writes the response for the errors of the owner checks and
// rules, and reports whether err was one of them.
func ownerError(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, store.ErrForbidden):
		fail(w, 403, "forbidden", "Only owners can manage users.")
	case errors.Is(err, store.ErrNotFound):
		fail(w, 404, "not_found", "User not found.")
	case errors.Is(err, store.ErrLastOwner):
		fail(w, 409, "last_owner", "At least one enabled owner is required.")
	case errors.Is(err, store.ErrSelf):
		fail(w, 409, "self", "You cannot disable yourself.")
	case errors.Is(err, store.ErrUserDisabled):
		fail(w, 409, "user_disabled", "The user is disabled.")
	case errors.Is(err, store.ErrConflict):
		fail(w, 409, "email_taken", "A user with this email already exists.")
	case errors.Is(err, store.ErrInvitationNotFound):
		fail(w, 404, "not_found", "Invitation not found.")
	default:
		return false
	}
	return true
}

// requireOwner is requireUser that also requires the owner role. The user
// was read from the database for this request; the store checks the role
// again inside each write transaction.
func (s *Server) requireOwner(next http.Handler) http.Handler {
	return s.requireUser(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, ok := userFrom(r.Context())
		if !ok {
			s.authError(w, r, store.ErrUnauthorized)
			return
		}
		if u.Role != store.RoleOwner || u.Disabled != 0 {
			fail(w, 403, "forbidden", "Only owners can manage users.")
			return
		}
		next.ServeHTTP(w, r)
	}))
}

func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	u, _ := userFrom(r.Context())
	users, err := s.auth.Store.ListUsers(r.Context())
	if err != nil {
		usersError(w, "list", u.ID, err)
		return
	}
	out := adminUserListResponse{Users: make([]adminUserResponse, 0, len(users))}
	for _, v := range users {
		out.Users = append(out.Users, toAdminUserResponse(v))
	}
	writeJSON(w, 200, out)
}

func (s *Server) handlePatchUser(w http.ResponseWriter, r *http.Request) {
	u, _ := userFrom(r.Context())
	id := r.PathValue("id")
	var in struct {
		Role     json.RawMessage `json:"role"`
		Disabled json.RawMessage `json:"disabled"`
	}
	if !decode(w, r, &in, apiTokenBodyLimit) {
		return
	}
	if (in.Role == nil) == (in.Disabled == nil) {
		fail(w, 400, "invalid_request", "Give exactly one of role and disabled.")
		return
	}
	var change store.UserChange
	if in.Role != nil {
		role, ok := decodeString(in.Role)
		if !ok || !store.ValidRole(role) {
			fail(w, 400, "invalid_request", "role must be owner or member.")
			return
		}
		change.Role = role
	} else {
		var disabled bool
		if isNull(in.Disabled) || !decodeValue(in.Disabled, &disabled) {
			fail(w, 400, "invalid_request", "disabled must be true or false.")
			return
		}
		change.Disabled = &disabled
	}
	if !itemIDPattern.MatchString(id) {
		fail(w, 404, "not_found", "User not found.")
		return
	}
	updated, err := s.auth.Store.UpdateUser(r.Context(), u.ID, id, change, s.auth.Now().Unix())
	if err != nil {
		if !ownerError(w, err) {
			usersError(w, "update", u.ID, err)
		}
		return
	}
	writeJSON(w, 200, toAdminUserResponse(updated))
}

func (s *Server) handleCreatePasswordReset(w http.ResponseWriter, r *http.Request) {
	u, _ := userFrom(r.Context())
	id := r.PathValue("id")
	if !itemIDPattern.MatchString(id) {
		fail(w, 404, "not_found", "User not found.")
		return
	}
	raw, hash, err := auth.NewPasswordResetToken()
	if err != nil {
		usersError(w, "password reset", u.ID, err)
		return
	}
	rid, err := auth.Random()
	if err != nil {
		usersError(w, "password reset", u.ID, err)
		return
	}
	now := s.auth.Now().Unix()
	reset := store.PasswordReset{ID: rid, TokenHash: hash, UserID: id, CreatedAt: now, ExpiresAt: now + store.PasswordResetLifetime}
	if err = s.auth.Store.CreatePasswordReset(r.Context(), u.ID, reset); err != nil {
		if !ownerError(w, err) {
			usersError(w, "password reset", u.ID, err)
		}
		return
	}
	writeJSON(w, 201, createdPasswordResetResponse{URL: s.cfg.PublicURL + "/reset-password#" + raw, ExpiresAt: formatTime(reset.ExpiresAt)})
}

func (s *Server) handleListInvitations(w http.ResponseWriter, r *http.Request) {
	u, _ := userFrom(r.Context())
	invitations, err := s.auth.Store.ListInvitations(r.Context(), s.auth.Now().Unix())
	if err != nil {
		usersError(w, "list invitations", u.ID, err)
		return
	}
	// The inviters are read with one more query instead of a join.
	seen := map[string]bool{}
	ids := []string{}
	for _, inv := range invitations {
		if !seen[inv.InvitedBy] {
			seen[inv.InvitedBy] = true
			ids = append(ids, inv.InvitedBy)
		}
	}
	inviters, err := s.auth.Store.UsersByIDs(r.Context(), ids)
	if err != nil {
		usersError(w, "list invitations", u.ID, err)
		return
	}
	emails := make(map[string]string, len(inviters))
	for _, v := range inviters {
		emails[v.ID] = v.Email
	}
	out := invitationListResponse{Invitations: make([]invitationResponse, 0, len(invitations))}
	for _, inv := range invitations {
		out.Invitations = append(out.Invitations, invitationResponse{ID: inv.ID, Email: inv.Email, Role: inv.Role, CreatedAt: formatTime(inv.CreatedAt), ExpiresAt: formatTime(inv.ExpiresAt), InvitedByEmail: emails[inv.InvitedBy]})
	}
	writeJSON(w, 200, out)
}

func (s *Server) handleCreateInvitation(w http.ResponseWriter, r *http.Request) {
	u, _ := userFrom(r.Context())
	var in struct {
		Email json.RawMessage `json:"email"`
		Role  json.RawMessage `json:"role"`
	}
	if !decode(w, r, &in, apiTokenBodyLimit) {
		return
	}
	raw, ok := decodeString(in.Email)
	if in.Email == nil || !ok {
		fail(w, 400, "invalid_request", "email is required.")
		return
	}
	email, err := auth.Email(raw)
	if err != nil {
		fail(w, 400, "invalid_request", err.Error())
		return
	}
	role, ok := decodeString(in.Role)
	if in.Role == nil || !ok || !store.ValidRole(role) {
		fail(w, 400, "invalid_request", "role must be owner or member.")
		return
	}
	token, hash, err := auth.NewInvitationToken()
	if err != nil {
		usersError(w, "invite", u.ID, err)
		return
	}
	id, err := auth.Random()
	if err != nil {
		usersError(w, "invite", u.ID, err)
		return
	}
	now := s.auth.Now().Unix()
	inv := store.Invitation{ID: id, TokenHash: hash, Email: email, Role: role, CreatedAt: now, ExpiresAt: now + store.InvitationLifetime}
	if err = s.auth.Store.CreateInvitation(r.Context(), u.ID, inv); err != nil {
		if !ownerError(w, err) {
			usersError(w, "invite", u.ID, err)
		}
		return
	}
	writeJSON(w, 201, createdInvitationResponse{ID: id, Email: email, Role: role, CreatedAt: formatTime(now), ExpiresAt: formatTime(inv.ExpiresAt), URL: s.cfg.PublicURL + "/invite#" + token})
}

func (s *Server) handleDeleteInvitation(w http.ResponseWriter, r *http.Request) {
	u, _ := userFrom(r.Context())
	id := r.PathValue("id")
	if !itemIDPattern.MatchString(id) {
		fail(w, 404, "not_found", "Invitation not found.")
		return
	}
	if err := s.auth.Store.RevokeInvitation(r.Context(), u.ID, id, s.auth.Now().Unix()); err != nil {
		if !ownerError(w, err) {
			usersError(w, "revoke invitation", u.ID, err)
		}
		return
	}
	w.WriteHeader(204)
}

// allowAccountAttempt counts an attempt at the public link endpoints and
// writes 429 when the peer is over the limit.
func (s *Server) allowAccountAttempt(w http.ResponseWriter, r *http.Request) bool {
	if s.accountLimiter.allow(r.RemoteAddr) {
		return true
	}
	w.Header().Set("Retry-After", "60")
	fail(w, 429, "rate_limited", "Too many attempts. Try again later.")
	return false
}

func invitationInvalid(w http.ResponseWriter) {
	fail(w, 404, "invitation_invalid", "The invitation link is invalid or has expired.")
}

func resetInvalid(w http.ResponseWriter) {
	fail(w, 404, "reset_invalid", "The password reset link is invalid or has expired.")
}

// linkError writes the response for an unexpected store error of the public
// link endpoints. Tokens and passwords are never logged.
func linkError(w http.ResponseWriter, op string, err error) {
	log.Printf("auth: %s: %v", op, err)
	fail(w, 503, "unavailable", "The service is temporarily unavailable. Try again.")
}

// linkPassword checks the length of a new password and hashes it, before any
// transaction is started. It writes 400 with the same message as the CLI.
func linkPassword(w http.ResponseWriter, password string) (string, bool) {
	hash, err := auth.Password(password)
	if err != nil {
		if len(password) < 12 || len(password) > 72 {
			fail(w, 400, "invalid_request", err.Error())
		} else {
			log.Printf("auth: hash password: %v", err)
			fail(w, 503, "unavailable", "The service is temporarily unavailable. Try again.")
		}
		return "", false
	}
	return hash, true
}

// signInAfterLink revokes the session of the refresh cookie the browser
// sent, if any, and signs u in. It is called only after the invitation or
// reset was committed. A failure is a 503 whatever its cause: the account
// change stays committed and the screen sends the user to the login form.
func (s *Server) signInAfterLink(w http.ResponseWriter, r *http.Request, op string, u store.User) {
	// A browser signed in to another account must not keep that session
	// next to the new one.
	if err := s.auth.Logout(r.Context(), refreshCookie(r)); err != nil {
		linkError(w, op+": revoke previous session", err)
		return
	}
	result, err := s.auth.LoginUser(r.Context(), u)
	if err != nil {
		linkError(w, op+": sign in", err)
		return
	}
	s.writeAuthResult(w, r, result)
}

func (s *Server) handleInspectInvitation(w http.ResponseWriter, r *http.Request) {
	if !s.allowAccountAttempt(w, r) {
		return
	}
	var in struct {
		Token string `json:"token"`
	}
	if !decode(w, r, &in, accountBodyLimit) {
		return
	}
	hash, ok := auth.InvitationTokenHash(in.Token)
	if !ok {
		invitationInvalid(w)
		return
	}
	inv, err := s.auth.Store.InvitationByToken(r.Context(), hash, s.auth.Now().Unix())
	if errors.Is(err, store.ErrInvitationInvalid) {
		invitationInvalid(w)
		return
	}
	if err != nil {
		linkError(w, "inspect invitation", err)
		return
	}
	writeJSON(w, 200, inspectedInvitationResponse{Email: inv.Email, Role: inv.Role, ExpiresAt: formatTime(inv.ExpiresAt)})
}

func (s *Server) handleAcceptInvitation(w http.ResponseWriter, r *http.Request) {
	if !s.allowAccountAttempt(w, r) {
		return
	}
	var in struct {
		Token    string `json:"token"`
		Password string `json:"password"`
	}
	if !decode(w, r, &in, accountBodyLimit) {
		return
	}
	hash, ok := auth.InvitationTokenHash(in.Token)
	if !ok {
		invitationInvalid(w)
		return
	}
	// The link is checked before the password is hashed so that an invalid
	// link costs no bcrypt work; the transaction checks it again.
	if _, err := s.auth.Store.InvitationByToken(r.Context(), hash, s.auth.Now().Unix()); err != nil {
		if errors.Is(err, store.ErrInvitationInvalid) {
			invitationInvalid(w)
		} else {
			linkError(w, "accept invitation", err)
		}
		return
	}
	passwordHash, ok := linkPassword(w, in.Password)
	if !ok {
		return
	}
	id, err := auth.Random()
	if err != nil {
		linkError(w, "accept invitation", err)
		return
	}
	now := s.auth.Now().Unix()
	u, err := s.auth.Store.AcceptInvitation(r.Context(), hash, store.User{ID: id, PasswordHash: passwordHash, CreatedAt: now}, now)
	switch {
	case err == nil:
	case errors.Is(err, store.ErrInvitationInvalid):
		invitationInvalid(w)
		return
	case errors.Is(err, store.ErrConflict):
		fail(w, 409, "email_taken", "An account with this email already exists.")
		return
	default:
		linkError(w, "accept invitation", err)
		return
	}
	s.signInAfterLink(w, r, "accept invitation", u)
}

func (s *Server) handleInspectPasswordReset(w http.ResponseWriter, r *http.Request) {
	if !s.allowAccountAttempt(w, r) {
		return
	}
	var in struct {
		Token string `json:"token"`
	}
	if !decode(w, r, &in, accountBodyLimit) {
		return
	}
	hash, ok := auth.PasswordResetTokenHash(in.Token)
	if !ok {
		resetInvalid(w)
		return
	}
	u, err := s.auth.Store.PasswordResetUser(r.Context(), hash, s.auth.Now().Unix())
	if errors.Is(err, store.ErrResetInvalid) {
		resetInvalid(w)
		return
	}
	if err != nil {
		linkError(w, "inspect password reset", err)
		return
	}
	writeJSON(w, 200, inspectedPasswordResetResponse{Email: u.Email})
}

func (s *Server) handleCompletePasswordReset(w http.ResponseWriter, r *http.Request) {
	if !s.allowAccountAttempt(w, r) {
		return
	}
	var in struct {
		Token    string `json:"token"`
		Password string `json:"password"`
	}
	if !decode(w, r, &in, accountBodyLimit) {
		return
	}
	hash, ok := auth.PasswordResetTokenHash(in.Token)
	if !ok {
		resetInvalid(w)
		return
	}
	if _, err := s.auth.Store.PasswordResetUser(r.Context(), hash, s.auth.Now().Unix()); err != nil {
		if errors.Is(err, store.ErrResetInvalid) {
			resetInvalid(w)
		} else {
			linkError(w, "complete password reset", err)
		}
		return
	}
	passwordHash, ok := linkPassword(w, in.Password)
	if !ok {
		return
	}
	u, err := s.auth.Store.CompletePasswordReset(r.Context(), hash, passwordHash, s.auth.Now().Unix())
	if errors.Is(err, store.ErrResetInvalid) {
		resetInvalid(w)
		return
	}
	if err != nil {
		linkError(w, "complete password reset", err)
		return
	}
	s.signInAfterLink(w, r, "complete password reset", u)
}
