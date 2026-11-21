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
var ErrMFAChallengeUsed = errors.New("WebAuthn challenge was already used")

// MaxWebAuthnCredentials is the number of WebAuthn credentials a user can
// hold at once.
const MaxWebAuthnCredentials = 10

// MFATOTP is the authenticator app of a user. SecretSealed is the secret
// sealed with the purpose "mfa-totp:" + UserID. LastStep is the last time
// step whose code was accepted; a code of that step or an earlier one is
// never accepted again.
type MFATOTP struct {
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
func (m MFAStatus) Enrolled() bool { return m.TOTP != nil || len(m.WebAuthn) > 0 }

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
	u, err := q.lockUser(ctx, userID)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, MFAStatus{}, ErrUnauthorized
	}
	if err != nil {
		return User{}, MFAStatus{}, err
	}
	if u.Disabled != 0 {
		return User{}, MFAStatus{}, ErrUnauthorized
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

// EnrollTOTP stores t as the authenticator app of t.UserID. t.LastStep is
// the time step of the code that confirmed the secret, so that the code
// cannot be used again.
//
// With onlyIfNone the user must have no second factor yet
// (ErrMFAEnrolled); this is the enrollment before the first session, which
// must never add a factor to an account that has one. Without it, an
// existing authenticator app is replaced.
//
// When the user had no second factor, codes are stored as the recovery
// codes and issued is true; otherwise codes are ignored.
func (s *Store) EnrollTOTP(ctx context.Context, t MFATOTP, codes []RecoveryCode, onlyIfNone bool) (issued bool, err error) {
	err = s.transaction(ctx, func(q queries) error {
		_, status, e := lockMFAUser(ctx, q, t.UserID)
		if e != nil {
			return e
		}
		first := !status.Enrolled()
		if onlyIfNone && !first {
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
		return insertRecoveryCodes(ctx, q, t.UserID, codes, t.CreatedAt)
	})
	if err != nil {
		return false, err
	}
	return issued, nil
}

// EnrollWebAuthn stores c as a WebAuthn credential of c.UserID. A user can
// hold MaxWebAuthnCredentials (ErrMFALimit), and a credential ID registered
// by any user is rejected (ErrMFACredentialExists). onlyIfNone, codes and
// issued are as in EnrollTOTP.
func (s *Store) EnrollWebAuthn(ctx context.Context, c WebAuthnCredential, codes []RecoveryCode, onlyIfNone bool) (issued bool, err error) {
	err = s.transaction(ctx, func(q queries) error {
		_, status, e := lockMFAUser(ctx, q, c.UserID)
		if e != nil {
			return e
		}
		first := !status.Enrolled()
		if onlyIfNone && !first {
			return ErrMFAEnrolled
		}
		if len(status.WebAuthn) >= MaxWebAuthnCredentials {
			return ErrMFALimit
		}
		n, e := q.createWebAuthnCredential(ctx, c)
		if e != nil {
			return e
		}
		if n != 1 {
			return ErrMFACredentialExists
		}
		if !first {
			return nil
		}
		issued = true
		return insertRecoveryCodes(ctx, q, c.UserID, codes, c.CreatedAt)
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

// UseTOTPStep records step as the last accepted time step of userID's
// authenticator app. It reports false when the app is missing or step is
// not after the last accepted one, that is, when the code was used before.
// Of two concurrent calls with the same step, one reports true.
func (s *Store) UseTOTPStep(ctx context.Context, userID string, step int64) (bool, error) {
	n, err := s.q.useMFATOTPStep(ctx, userID, step)
	return n == 1, err
}

// UseRecoveryCode marks the unused recovery code of userID whose normalized
// form hashes to hash as used. It reports false when there is no such code:
// unknown, used before or another user's. Of two concurrent calls with the
// same code, one reports true.
func (s *Store) UseRecoveryCode(ctx context.Context, userID, hash string, now int64) (bool, error) {
	n, err := s.q.useRecoveryCode(ctx, userID, hash, now)
	return n == 1, err
}

// UpdateWebAuthnCredential stores the credential after a verified sign-in
// with it (the JSON with the new signature counter) and its time of use,
// and spends the challenge of that sign-in, in one transaction:
//
//  1. The user row is locked; a missing or disabled user fails with
//     ErrUnauthorized.
//  2. The credential credentialID of userID is updated, or ErrNotFound is
//     returned when it does not exist (it was removed meanwhile).
//  3. The spent challenges that expired (expires_at <= now) are removed.
//  4. challengeHash is inserted with expiresAt, the expiry of the sign-in
//     state. A challenge spent before fails with ErrMFAChallengeUsed.
//
// Any error rolls everything back, so a replayed sign-in response changes
// nothing.
func (s *Store) UpdateWebAuthnCredential(ctx context.Context, userID, credentialID, credential, challengeHash string, expiresAt, now int64) error {
	return s.transaction(ctx, func(q queries) error {
		if _, _, e := lockMFAUser(ctx, q, userID); e != nil {
			return e
		}
		n, e := q.updateWebAuthnCredential(ctx, userID, credentialID, credential, now)
		if e != nil {
			return e
		}
		if n != 1 {
			return ErrNotFound
		}
		if e = q.deleteExpiredUsedChallenges(ctx, now); e != nil {
			return e
		}
		if n, e = q.createUsedChallenge(ctx, challengeHash, expiresAt); e != nil {
			return e
		}
		if n != 1 {
			return ErrMFAChallengeUsed
		}
		return nil
	})
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

// ResetMFA removes the authenticator app, the WebAuthn credentials and the
// recovery codes of the user with email, enabled or disabled, for the
// operator's CLI. Sessions and tokens are kept.
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
		return q.deleteUserRecoveryCodes(ctx, u.ID)
	})
}
