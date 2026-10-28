package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// Roles of a user. Every user is an owner or a member; only owners can use
// the user management API. Each user's data stays private to that user
// whatever the role.
const (
	RoleOwner  = "owner"
	RoleMember = "member"
)

// Lifetimes of the links issued from the user management API, and how long a
// completed password reset is reported to the user (seconds).
const (
	InvitationLifetime    = 7 * 24 * 60 * 60
	PasswordResetLifetime = 24 * 60 * 60
	ResetNoticePeriod     = 14 * 24 * 60 * 60
)

// Upper bounds of the user management lists. Instances are small, so the
// lists are not paged.
const (
	maxListedUsers       = 1000
	maxListedInvitations = 200
)

var ErrInvalidRole = errors.New("role must be owner or member")
var ErrForbidden = errors.New("the operator is not an enabled owner")
var ErrLastOwner = errors.New("at least one enabled owner is required")
var ErrSelf = errors.New("owners cannot disable themselves")
var ErrUserDisabled = errors.New("user is disabled")
var ErrInvitationInvalid = errors.New("invitation is invalid or expired")
var ErrInvitationNotFound = errors.New("invitation not found")
var ErrResetInvalid = errors.New("password reset link is invalid or expired")

// ValidRole reports whether role is RoleOwner or RoleMember.
func ValidRole(role string) bool { return role == RoleOwner || role == RoleMember }

// Invitation is a link that lets the holder create a user with Email and
// Role. Only the SHA-256 hash of the token is stored. It is active while it
// is neither accepted nor revoked and ExpiresAt is after now.
type Invitation struct {
	ID             string
	TokenHash      string
	Email          string
	Role           string
	InvitedBy      string
	CreatedAt      int64
	ExpiresAt      int64
	AcceptedAt     sql.NullInt64
	AcceptedUserID sql.NullString
	RevokedAt      sql.NullInt64
}

func (inv Invitation) active(now int64) bool {
	return !inv.AcceptedAt.Valid && !inv.RevokedAt.Valid && inv.ExpiresAt > now
}

// PasswordReset is a link that lets the holder set a new password of UserID.
// CreatedBy is the owner that issued it. Only the SHA-256 hash of the token
// is stored. It is active while it is neither used nor revoked and ExpiresAt
// is after now.
type PasswordReset struct {
	ID        string
	TokenHash string
	UserID    string
	CreatedBy string
	CreatedAt int64
	ExpiresAt int64
	UsedAt    sql.NullInt64
	RevokedAt sql.NullInt64
}

func (r PasswordReset) active(now int64) bool {
	return !r.UsedAt.Valid && !r.RevokedAt.Valid && r.ExpiresAt > now
}

// ResetNotice describes the latest password reset link the user completed:
// when, and the email of the owner that issued it.
type ResetNotice struct {
	At      int64
	ByEmail string
}

// UserChange is a change of the user management API. Exactly one of Role
// (non-empty) and Disabled (non-nil) is set.
type UserChange struct {
	Role     string
	Disabled *bool
}

// ownerLock is the result of lockOwnerChange.
type ownerLock struct {
	target User
	found  bool
	// owners is the number of enabled owners, read under the lock.
	owners int
}

// lockOwnerChange locks, in user ID order, the operator, the target and
// every enabled owner, and checks again that the operator is an enabled
// owner (ErrForbidden). The HTTP layer checked the role outside this
// transaction, so it may have changed in between. Every write of the user
// management API starts with it, which serializes them with each other and
// with the writers that lock a single user row (lockUser), and lets the
// last-owner rule count the owners that cannot change until the commit.
//
// On PostgreSQL the rows are locked with FOR UPDATE and re-read after any
// wait, so two owners that demote each other at the same time are
// serialized and the second sees the first's change. SQLite transactions
// are serialized as a whole.
func lockOwnerChange(ctx context.Context, q queries, operatorID, targetID string) (ownerLock, error) {
	rows, e := q.lockUsersForOwnerChange(ctx, operatorID, targetID)
	if e != nil {
		return ownerLock{}, e
	}
	var l ownerLock
	operator := false
	for _, u := range rows {
		if u.ID == operatorID {
			operator = u.Role == RoleOwner && u.Disabled == 0
		}
		if u.ID == targetID {
			l.target, l.found = u, true
		}
		if u.Role == RoleOwner && u.Disabled == 0 {
			l.owners++
		}
	}
	if !operator {
		return ownerLock{}, ErrForbidden
	}
	return l, nil
}

// revokeIssued revokes the active password reset links and invitations that
// userID issued. It is used when an owner becomes a member, which cannot
// issue them. The caller holds the lock of the user row.
func revokeIssued(ctx context.Context, q queries, userID string, now int64) error {
	return revokeLinks(ctx, q, userID, false, now)
}

// revokeLinks revokes the active password reset links and invitations that
// userID issued and, with received, the reset links issued for userID. The
// reset rows are locked in ID order before they are updated, so that
// concurrent revocations and completions lock them in the same order.
func revokeLinks(ctx context.Context, q queries, userID string, received bool, now int64) error {
	resets, e := q.lockActivePasswordResets(ctx, userID, now)
	if e != nil {
		return e
	}
	for _, r := range resets {
		if r.CreatedBy == userID || (received && r.UserID == userID) {
			if e = q.revokePasswordReset(ctx, r.ID, now); e != nil {
				return e
			}
		}
	}
	return q.revokeIssuedInvitations(ctx, userID, now)
}

// ListUsers returns every user, oldest first (at most 1000).
func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	return s.q.listUsers(ctx, maxListedUsers)
}

// UsersByIDs returns the users with the given IDs, in no particular order.
// Missing users are left out.
func (s *Store) UsersByIDs(ctx context.Context, ids []string) ([]User, error) {
	if len(ids) == 0 {
		return []User{}, nil
	}
	return s.q.usersByIDs(ctx, strings.Join(ids, ","))
}

// UpdateUser applies change to targetID on behalf of operatorID and returns
// the user after the change. The checks run in this order: the operator is
// an enabled owner (ErrForbidden), the target exists (ErrNotFound), a change
// to the current value does nothing, an owner cannot disable itself
// (ErrSelf), and the last enabled owner can be neither demoted nor disabled
// (ErrLastOwner).
//
// Disabling removes what revokeAccount removes, keeping the user's items
// and notification settings. Enabling only clears the flag. Demoting
// revokes the links the user issued; it keeps the sessions, which lose the
// owner API at their next request.
func (s *Store) UpdateUser(ctx context.Context, operatorID, targetID string, change UserChange, now int64) (User, error) {
	if (change.Role == "") == (change.Disabled == nil) {
		return User{}, errors.New("exactly one change is required")
	}
	if change.Role != "" && !ValidRole(change.Role) {
		return User{}, ErrInvalidRole
	}
	var out User
	err := s.transaction(ctx, func(q queries) error {
		l, e := lockOwnerChange(ctx, q, operatorID, targetID)
		if e != nil {
			return e
		}
		if !l.found {
			return ErrNotFound
		}
		t := l.target
		owner := t.Role == RoleOwner && t.Disabled == 0
		if change.Disabled != nil {
			disabled := int64(0)
			if *change.Disabled {
				disabled = 1
			}
			if t.Disabled == disabled {
				out = t
				return nil
			}
			if disabled == 1 {
				if t.ID == operatorID {
					return ErrSelf
				}
				if owner && l.owners <= 1 {
					return ErrLastOwner
				}
				if e = revokeAccount(ctx, q, t.ID, "", true, now); e != nil {
					return e
				}
			} else if e = q.enableUser(ctx, t.ID); e != nil {
				return e
			}
			t.Disabled = disabled
			out = t
			return nil
		}
		if t.Role == change.Role {
			out = t
			return nil
		}
		if change.Role == RoleMember && owner && l.owners <= 1 {
			return ErrLastOwner
		}
		if e = q.setUserRole(ctx, t.ID, change.Role); e != nil {
			return e
		}
		if change.Role == RoleMember {
			if e = revokeIssued(ctx, q, t.ID, now); e != nil {
				return e
			}
		}
		t.Role = change.Role
		out = t
		return nil
	})
	if err != nil {
		return User{}, err
	}
	return out, nil
}

// EnableUser clears the disabled flag of the user with email, for the
// operator's CLI. Nothing removed by disabling comes back.
func (s *Store) EnableUser(ctx context.Context, email string) error {
	u, err := s.q.userByEmail(ctx, email)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	return s.transaction(ctx, func(q queries) error {
		if _, e := q.lockUser(ctx, u.ID); e != nil {
			return e
		}
		return q.enableUser(ctx, u.ID)
	})
}

// CreateInvitation stores inv, issued by operatorID, after revoking the
// active invitations to the same email. It fails with ErrConflict when a
// user with the email exists, enabled or not. The operator's row lock
// serializes invitations from the same owner; two owners inviting the same
// email at once may leave two active invitations, of which only one can be
// accepted.
func (s *Store) CreateInvitation(ctx context.Context, operatorID string, inv Invitation) error {
	if !ValidRole(inv.Role) {
		return ErrInvalidRole
	}
	inv.InvitedBy = operatorID
	return s.transaction(ctx, func(q queries) error {
		if _, e := lockOwnerChange(ctx, q, operatorID, operatorID); e != nil {
			return e
		}
		if _, e := q.userByEmail(ctx, inv.Email); e == nil {
			return ErrConflict
		} else if !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		if e := q.revokeEmailInvitations(ctx, inv.Email, inv.CreatedAt); e != nil {
			return e
		}
		return q.createInvitation(ctx, inv)
	})
}

// RevokeInvitation revokes the active invitation id on behalf of
// operatorID, or returns ErrInvitationNotFound when there is none.
func (s *Store) RevokeInvitation(ctx context.Context, operatorID, id string, now int64) error {
	return s.transaction(ctx, func(q queries) error {
		if _, e := lockOwnerChange(ctx, q, operatorID, operatorID); e != nil {
			return e
		}
		n, e := q.revokeInvitation(ctx, id, now)
		if e != nil {
			return e
		}
		if n == 0 {
			return ErrInvitationNotFound
		}
		return nil
	})
}

// ListInvitations returns the active invitations, newest first (at most
// 200).
func (s *Store) ListInvitations(ctx context.Context, now int64) ([]Invitation, error) {
	return s.q.listActiveInvitations(ctx, now, maxListedInvitations)
}

// InvitationByToken returns the active invitation whose token hashes to
// hash, or ErrInvitationInvalid. Missing, expired, accepted and revoked
// invitations are not told apart.
func (s *Store) InvitationByToken(ctx context.Context, hash string, now int64) (Invitation, error) {
	inv, err := s.q.invitationByTokenHash(ctx, hash)
	if errors.Is(err, sql.ErrNoRows) {
		return Invitation{}, ErrInvitationInvalid
	}
	if err != nil {
		return Invitation{}, err
	}
	if !inv.active(now) {
		return Invitation{}, ErrInvitationInvalid
	}
	return inv, nil
}

// AcceptInvitation creates the user of the active invitation whose token
// hashes to hash, with the invitation's email and role, and marks the
// invitation accepted. u gives the ID, password hash and creation time.
// The invitation row is locked first, so that a second acceptance of the
// same invitation waits and then fails with ErrInvitationInvalid. A user
// with the same email, existing or created concurrently, fails it with
// ErrConflict and leaves the invitation active.
//
// The state of the owner that issued the invitation is not checked here:
// disabling, demoting or recovering that owner revokes its active
// invitations (revokeLinks) under the owner's row lock, so an active
// invitation is always one its issuer may still give.
func (s *Store) AcceptInvitation(ctx context.Context, hash string, u User, now int64) (User, error) {
	var email string
	err := s.transaction(ctx, func(q queries) error {
		inv, e := q.lockInvitationByTokenHash(ctx, hash)
		if errors.Is(e, sql.ErrNoRows) {
			return ErrInvitationInvalid
		}
		if e != nil {
			return e
		}
		if !inv.active(now) || !ValidRole(inv.Role) {
			return ErrInvitationInvalid
		}
		email = inv.Email
		if _, e = q.userByEmail(ctx, inv.Email); e == nil {
			return ErrConflict
		} else if !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		u.Email, u.Role, u.Disabled = inv.Email, inv.Role, 0
		if e = q.createUser(ctx, u); e != nil {
			return e
		}
		n, e := q.acceptInvitation(ctx, inv.ID, u.ID, now)
		if e != nil {
			return e
		}
		if n != 1 {
			return ErrInvitationInvalid
		}
		return nil
	})
	if err != nil {
		// A unique violation of users.email by a concurrent creation is
		// reported as ErrConflict, as in CreateUser.
		if email != "" && !errors.Is(err, ErrInvitationInvalid) && !errors.Is(err, ErrConflict) {
			if existing, e := s.q.userByEmail(ctx, email); e == nil && existing.ID != "" {
				return User{}, ErrConflict
			}
		}
		return User{}, err
	}
	return u, nil
}

// PasswordResetUser returns the enabled user of the active password reset
// link whose token hashes to hash, or ErrResetInvalid. Missing, expired,
// used and revoked links and disabled users are not told apart.
func (s *Store) PasswordResetUser(ctx context.Context, hash string, now int64) (User, error) {
	r, err := s.q.passwordResetByTokenHash(ctx, hash)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrResetInvalid
	}
	if err != nil {
		return User{}, err
	}
	if !r.active(now) {
		return User{}, ErrResetInvalid
	}
	u, err := s.q.userByID(ctx, r.UserID)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrResetInvalid
	}
	if err != nil {
		return User{}, err
	}
	if u.Disabled != 0 {
		return User{}, ErrResetInvalid
	}
	return u, nil
}

// CreatePasswordReset stores r, a reset link for r.UserID issued by
// operatorID, after revoking the active links for the same user. It fails
// with ErrNotFound for a missing user and ErrUserDisabled for a disabled
// one. An owner may issue a link for itself.
func (s *Store) CreatePasswordReset(ctx context.Context, operatorID string, r PasswordReset) error {
	r.CreatedBy = operatorID
	return s.transaction(ctx, func(q queries) error {
		l, e := lockOwnerChange(ctx, q, operatorID, r.UserID)
		if e != nil {
			return e
		}
		if !l.found {
			return ErrNotFound
		}
		if l.target.Disabled != 0 {
			return ErrUserDisabled
		}
		resets, e := q.lockActivePasswordResets(ctx, r.UserID, r.CreatedAt)
		if e != nil {
			return e
		}
		for _, old := range resets {
			if old.UserID == r.UserID {
				if e = q.revokePasswordReset(ctx, old.ID, r.CreatedAt); e != nil {
					return e
				}
			}
		}
		return q.createPasswordReset(ctx, r)
	})
}

// CompletePasswordReset sets the password of the user of the active reset
// link whose token hashes to hash to passwordHash, marks the link used and
// removes what revokeAccount removes for a password reset, including the
// user's other links. It returns the user with the new hash, or
// ErrResetInvalid when the link is not active or the user is disabled.
//
// As in Rotate, the link is first read outside the transaction only to
// learn user_id, so that the transaction locks the user row before the link
// rows. The link rows of the user are then locked in ID order (with the
// ones revokeAccount revokes) and the link is checked again among them.
func (s *Store) CompletePasswordReset(ctx context.Context, hash, passwordHash string, now int64) (User, error) {
	r, err := s.q.passwordResetByTokenHash(ctx, hash)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrResetInvalid
	}
	if err != nil {
		return User{}, err
	}
	var out User
	err = s.transaction(ctx, func(q queries) error {
		u, e := q.lockUser(ctx, r.UserID)
		if errors.Is(e, sql.ErrNoRows) {
			return ErrResetInvalid
		}
		if e != nil {
			return e
		}
		if u.Disabled != 0 {
			return ErrResetInvalid
		}
		resets, e := q.lockActivePasswordResets(ctx, u.ID, now)
		if e != nil {
			return e
		}
		found := false
		for _, x := range resets {
			if x.ID == r.ID && x.UserID == u.ID && x.TokenHash == hash {
				found = true
			}
		}
		if !found {
			return ErrResetInvalid
		}
		n, e := q.usePasswordReset(ctx, r.ID, now)
		if e != nil {
			return e
		}
		if n != 1 {
			return ErrResetInvalid
		}
		if e = revokeAccount(ctx, q, u.ID, passwordHash, false, now); e != nil {
			return e
		}
		u.PasswordHash = passwordHash
		out = u
		return nil
	})
	if err != nil {
		return User{}, err
	}
	return out, nil
}

// LatestResetNotice returns the latest password reset link for userID that
// was used after now-ResetNoticePeriod, or nil when there is none.
func (s *Store) LatestResetNotice(ctx context.Context, userID string, now int64) (*ResetNotice, error) {
	r, err := s.q.latestUsedPasswordReset(ctx, userID, now-ResetNoticePeriod)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	by, err := s.q.userByID(ctx, r.CreatedBy)
	if err != nil {
		return nil, err
	}
	return &ResetNotice{At: r.UsedAt.Int64, ByEmail: by.Email}, nil
}
