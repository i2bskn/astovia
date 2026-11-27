package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/i2bskn/astovia/internal/mfa"
)

var ErrMFAEnrolled = errors.New("a second factor is already enrolled")
var ErrMFACredentialExists = errors.New("WebAuthn credential is already registered")
var ErrMFALimit = errors.New("too many WebAuthn credentials")
var ErrMFARequired = errors.New("the last second factor cannot be removed")
var ErrMFANotEnrolled = errors.New("no second factor is enrolled")

// MaxWebAuthnCredentials is the number of WebAuthn credentials a user can
// hold at once.
const MaxWebAuthnCredentials = 10

// MFATOTP is the authenticator app of a user. ID is a random value given at
// each enrollment; a row without one was stored before authlume and cannot
// be used. SecretSealed is the secret sealed with the purpose "mfa-totp:" +
// UserID. LastStep is the last time step whose code was accepted; a code of
// that step or an earlier one is never accepted again.
type MFATOTP struct {
	ID           string
	UserID       string
	SecretSealed string
	LastStep     int64
	CreatedAt    int64
}

// WebAuthnCredential is a passkey or security key of a user. CredentialID is
// the ID the authenticator gave (base64url without padding), unique among
// all users. Credential is the JSON of the WebAuthn library's credential
// (public key, signature counter, flags and transports); it holds no
// secret.
type WebAuthnCredential struct {
	ID           string
	UserID       string
	CredentialID string
	Credential   string
	Name         string
	CreatedAt    int64
	LastUsedAt   sql.NullInt64
	// Version is the active revision of the credential, replaced at each
	// use; a row without one was stored before authlume and cannot be used.
	Version string
	Passkey int64
}

// RecoveryCode is a new recovery code to store: the row ID and the SHA-256
// hash of the normalized code. The code itself is never stored.
type RecoveryCode struct {
	ID   string
	Hash string
}

// MFAStatus is what a user has enrolled. TOTP is nil without an
// authenticator app, WebAuthn is never nil, and RecoveryCodes is the number
// of unused recovery codes.
type MFAStatus struct {
	TOTP          *MFATOTP
	WebAuthn      []WebAuthnCredential
	RecoveryCodes int64
}

// Enrolled reports whether the user has a second factor: an authenticator
// app or at least one WebAuthn credential. Recovery codes are not a factor.
// Rows stored before authlume count, so that such a user is still asked for
// a second factor (and can use a recovery code) rather than let in with the
// password alone.
func (m MFAStatus) Enrolled() bool { return m.TOTP != nil || len(m.WebAuthn) > 0 }

// UsableTOTP reports whether the authenticator app can be used: it has an
// ID, so it was stored by this version.
func (m MFAStatus) UsableTOTP() bool { return m.TOTP != nil && m.TOTP.ID != "" }

// UsableWebAuthn is the number of WebAuthn credentials that can be used:
// those with a version, stored by this version.
func (m MFAStatus) UsableWebAuthn() int {
	n := 0
	for _, c := range m.WebAuthn {
		if c.Version != "" {
			n++
		}
	}
	return n
}

// usable reports whether the user has a second factor that can be used.
func (m MFAStatus) usable() bool {
	return m.UsableTOTP() || m.UsableWebAuthn() > 0
}

// EnrollOptions are the conditions of an enrollment, checked under the lock
// of the user row.
type EnrollOptions struct {
	// OnlyIfNone refuses a user that has a usable second factor
	// (ErrMFAEnrolled); this is the enrollment before the first session,
	// which must never add a factor to an account that has one.
	OnlyIfNone bool
	// SessionID, when set, must be an active session of the user
	// (ErrUnauthorized). PasswordDigest, when set, must be auth.Hash of the
	// current password hash (ErrUnauthorized). One of them must be set.
	SessionID      string
	PasswordDigest string
	Now            int64
}

func checkEnrollment(ctx context.Context, q queries, u User, o EnrollOptions) error {
	if o.SessionID == "" && o.PasswordDigest == "" {
		return errNoGuard
	}
	return checkBinding(ctx, q, u, o.SessionID, o.PasswordDigest, o.Now)
}

// UserByID returns the user with id, enabled or not, or sql.ErrNoRows.
func (s *Store) UserByID(ctx context.Context, id string) (User, error) {
	return s.q.userByID(ctx, id)
}

// MFAStatus returns what userID has enrolled.
func (s *Store) MFAStatus(ctx context.Context, userID string) (MFAStatus, error) {
	return mfaStatus(ctx, s.q, userID)
}

func mfaStatus(ctx context.Context, q queries, userID string) (MFAStatus, error) {
	var out MFAStatus
	t, err := q.mfaTOTP(ctx, userID)
	if err == nil {
		out.TOTP = &t
	} else if !errors.Is(err, sql.ErrNoRows) {
		return MFAStatus{}, err
	}
	if out.WebAuthn, err = q.listWebAuthnCredentials(ctx, userID); err != nil {
		return MFAStatus{}, err
	}
	if out.RecoveryCodes, err = q.countUnusedRecoveryCodes(ctx, userID); err != nil {
		return MFAStatus{}, err
	}
	return out, nil
}

// lockMFAUser locks the row of userID, which every write of the user's
// second factors does first, and returns the user with its factors as they
// are under the lock. A missing or disabled user fails with
// ErrUnauthorized.
func lockMFAUser(ctx context.Context, q queries, userID string) (User, MFAStatus, error) {
	u, err := lockEnabledUser(ctx, q, userID)
	if err != nil {
		return User{}, MFAStatus{}, err
	}
	status, err := mfaStatus(ctx, q, userID)
	if err != nil {
		return User{}, MFAStatus{}, err
	}
	return u, status, nil
}

func insertRecoveryCodes(ctx context.Context, q queries, userID string, codes []RecoveryCode, now int64) error {
	for _, c := range codes {
		if err := q.createRecoveryCode(ctx, userID, c, now); err != nil {
			return err
		}
	}
	return nil
}

// EnrollTOTP stores t as the authenticator app of t.UserID, replacing an
// existing one. t.ID must be a new random value and t.LastStep the time step
// of the code that confirmed the secret, so that the code cannot be used
// again. o is checked under the lock of the user row.
//
// When the user had no usable second factor, codes replace the recovery
// codes and issued is true; otherwise codes are ignored.
func (s *Store) EnrollTOTP(ctx context.Context, t MFATOTP, codes []RecoveryCode, o EnrollOptions) (issued bool, err error) {
	if t.ID == "" {
		return false, errors.New("authenticator app ID is required")
	}
	err = s.transaction(ctx, func(q queries) error {
		u, status, e := lockMFAUser(ctx, q, t.UserID)
		if e != nil {
			return e
		}
		if e = checkEnrollment(ctx, q, u, o); e != nil {
			return e
		}
		first := !status.usable()
		if o.OnlyIfNone && !first {
			return ErrMFAEnrolled
		}
		if status.TOTP == nil {
			if e = q.createMFATOTP(ctx, t); e != nil {
				return e
			}
		} else {
			n, e := q.replaceMFATOTP(ctx, t)
			if e != nil {
				return e
			}
			if n != 1 {
				return errors.New("authenticator app was not replaced")
			}
		}
		if !first {
			return nil
		}
		issued = true
		if e = q.deleteUserRecoveryCodes(ctx, t.UserID); e != nil {
			return e
		}
		return insertRecoveryCodes(ctx, q, t.UserID, codes, t.CreatedAt)
	})
	if err != nil {
		return false, err
	}
	return issued, nil
}

// removeFactor checks, for a user that is about to lose one second factor,
// that it is not the last factor of a user that policy (a policy of
// ASTOVIA_MFA_REQUIRED) requires one of (ErrMFARequired). The role is the
// one read under the lock of the user row.
func removeFactor(u User, status MFAStatus, policy string) (last bool, err error) {
	factors := len(status.WebAuthn)
	if status.TOTP != nil {
		factors++
	}
	last = factors <= 1
	if last && mfa.Required(policy, u.Role == RoleOwner) {
		return last, ErrMFARequired
	}
	return last, nil
}

// DeleteTOTP removes the authenticator app of userID, or returns
// ErrNotFound when there is none. A user that policy requires a second
// factor of cannot remove the last one (ErrMFARequired). When no factor is
// left, the recovery codes are removed as well. Sessions and tokens are
// kept.
func (s *Store) DeleteTOTP(ctx context.Context, userID, policy string) error {
	return s.transaction(ctx, func(q queries) error {
		u, status, e := lockMFAUser(ctx, q, userID)
		if e != nil {
			return e
		}
		if status.TOTP == nil {
			return ErrNotFound
		}
		last, e := removeFactor(u, status, policy)
		if e != nil {
			return e
		}
		n, e := q.deleteMFATOTP(ctx, userID)
		if e != nil {
			return e
		}
		if n != 1 {
			return ErrNotFound
		}
		if last {
			return q.deleteUserRecoveryCodes(ctx, userID)
		}
		return nil
	})
}

// DeleteWebAuthnCredential removes the WebAuthn credential id of userID, or
// returns ErrNotFound when it does not exist or belongs to another user.
// The rules are those of DeleteTOTP.
func (s *Store) DeleteWebAuthnCredential(ctx context.Context, userID, id, policy string) error {
	return s.transaction(ctx, func(q queries) error {
		u, status, e := lockMFAUser(ctx, q, userID)
		if e != nil {
			return e
		}
		found := false
		for _, c := range status.WebAuthn {
			found = found || c.ID == id
		}
		if !found {
			return ErrNotFound
		}
		last, e := removeFactor(u, status, policy)
		if e != nil {
			return e
		}
		n, e := q.deleteWebAuthnCredential(ctx, userID, id)
		if e != nil {
			return e
		}
		if n != 1 {
			return ErrNotFound
		}
		if last {
			return q.deleteUserRecoveryCodes(ctx, userID)
		}
		return nil
	})
}

// ReplaceRecoveryCodes removes every recovery code of userID, used or not,
// and stores codes instead. A user without a second factor has no recovery
// codes (ErrMFANotEnrolled).
func (s *Store) ReplaceRecoveryCodes(ctx context.Context, userID string, codes []RecoveryCode, now int64) error {
	return s.transaction(ctx, func(q queries) error {
		_, status, e := lockMFAUser(ctx, q, userID)
		if e != nil {
			return e
		}
		if !status.Enrolled() {
			return ErrMFANotEnrolled
		}
		if e = q.deleteUserRecoveryCodes(ctx, userID); e != nil {
			return e
		}
		return insertRecoveryCodes(ctx, q, userID, codes, now)
	})
}

// UseRecoveryCode marks the unused recovery code of userID whose normalized
// form hashes to hash as used. It reports false when there is no such code:
// unknown, used before or another user's. Of two concurrent calls with the
// same code, one reports true.
func (s *Store) UseRecoveryCode(ctx context.Context, userID, hash string, now int64) (bool, error) {
	n, err := s.q.useRecoveryCode(ctx, userID, hash, now)
	return n == 1, err
}

// MFAEnabledUserIDs returns the IDs of the users that have a second
// factor, for the user list. The two tables are read with one query each.
func (s *Store) MFAEnabledUserIDs(ctx context.Context) (map[string]bool, error) {
	totp, err := s.q.listMFATOTPUserIDs(ctx)
	if err != nil {
		return nil, err
	}
	webauthn, err := s.q.listWebAuthnUserIDs(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(totp)+len(webauthn))
	for _, id := range totp {
		out[id] = true
	}
	for _, id := range webauthn {
		out[id] = true
	}
	return out, nil
}

// ResetMFA removes the authenticator app, the WebAuthn credentials, the
// recovery codes and the pending WebAuthn ceremonies of the user with email,
// enabled or disabled, for the operator's CLI. The sessions, API tokens and
// MCP connections are revoked as well, in the same transaction.
func (s *Store) ResetMFA(ctx context.Context, email string) error {
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
		if _, e := q.deleteMFATOTP(ctx, u.ID); e != nil {
			return e
		}
		if e := q.deleteUserWebAuthnCredentials(ctx, u.ID); e != nil {
			return e
		}
		if e := q.deleteUserRecoveryCodes(ctx, u.ID); e != nil {
			return e
		}
		return revokeAccess(ctx, q, u.ID)
	})
}
