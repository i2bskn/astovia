package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"time"
)

// SourceGmail is action_items.source for items created by the mail
// integration (Gmail and Google Workspace).
const SourceGmail = "gmail"

// Gmail account status values.
const (
	GmailActive = "active"
	GmailError  = "error"
)

// MaxGmailAccounts is the number of Google accounts a user can connect.
const MaxGmailAccounts = 10

// maxAuthStates is the number of Google authorizations a user can have in
// progress at once; the oldest are dropped first.
const maxAuthStates = 4

var (
	// ErrAccountInUse is returned when the Google account is connected by
	// another user. A Google account can be connected by one user only:
	// revoking a token revokes every grant of the account to the client.
	ErrAccountInUse = errors.New("google account is connected by another user")
	// ErrTooManyAccounts is returned when the user already has
	// MaxGmailAccounts accounts.
	ErrTooManyAccounts = errors.New("too many google accounts")
	// ErrAuthStateNotFound is returned for a Google authorization state that
	// does not exist, expired, was used, or belongs to another user.
	ErrAuthStateNotFound = errors.New("google authorization state not found")
)

// GmailAccount is a Google account connected by a user, with its refresh
// token and the state of its synchronization. Email is the lower-case
// address, unique in the instance. TokenSealed is the refresh token sealed
// by secretbox with the purpose of the user and the ID; the plain token is
// never stored. Generation increases on every token registration so that a
// synchronization started with an older token cannot write its results.
// ConnectedAt is when the current token was registered; CreatedAt stays at
// the first registration.
type GmailAccount struct {
	ID              string
	UserID          string
	Email           string
	Generation      int64
	TokenSealed     string
	ImmediateNotify int64
	Status          string
	LastError       sql.NullString
	FailureCount    int64
	ConnectedAt     int64
	NextPollAt      int64
	LastSyncedAt    sql.NullInt64
	LastTrackedAt   sql.NullInt64
	CreatedAt       int64
	UpdatedAt       int64
}

// googleAuthState is a Google authorization in progress.
type googleAuthState struct {
	StateHash      string
	UserID         string
	VerifierSealed string
	ExpiresAt      int64
	CreatedAt      int64
}

// CreateAuthState stores a Google authorization in progress of the user:
// the SHA-256 hash of its state and the sealed PKCE verifier, valid for
// ttl. The user's expired states are deleted first, and the oldest ones
// too when the user has more than four, so that at most five are kept.
func (s *Store) CreateAuthState(ctx context.Context, userID, sessionID, stateHash, verifierSealed string, now int64, ttl time.Duration) error {
	return s.transaction(ctx, func(q queries) error {
		// Lock order: users, google_auth_states.
		if e := lockActiveUser(ctx, q, userID, sessionID, now); e != nil {
			return e
		}
		if e := q.deleteExpiredGoogleAuthStates(ctx, userID, now); e != nil {
			return e
		}
		hashes, e := q.listGoogleAuthStateHashes(ctx, userID)
		if e != nil {
			return e
		}
		// The list is newest first; the ones after the newest four go.
		for i := maxAuthStates; i < len(hashes); i++ {
			if _, e = q.deleteGoogleAuthState(ctx, userID, hashes[i]); e != nil {
				return e
			}
		}
		return q.createGoogleAuthState(ctx, googleAuthState{StateHash: stateHash, UserID: userID, VerifierSealed: verifierSealed, ExpiresAt: now + int64(ttl/time.Second), CreatedAt: now})
	})
}

// ConsumeAuthState deletes the user's authorization state with the hash and
// returns its sealed verifier, so that a state can be used once. A state
// that does not exist, expired or belongs to another user returns
// ErrAuthStateNotFound and is not deleted; the user's expired states are
// deleted then (another user's are not touched).
func (s *Store) ConsumeAuthState(ctx context.Context, userID, sessionID, stateHash string, now int64) (string, error) {
	var sealed string
	var missing bool
	err := s.transaction(ctx, func(q queries) error {
		if e := lockActiveUser(ctx, q, userID, sessionID, now); e != nil {
			return e
		}
		st, e := q.googleAuthState(ctx, userID, stateHash)
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		if e != nil || st.ExpiresAt <= now {
			missing = true
			return q.deleteExpiredGoogleAuthStates(ctx, userID, now)
		}
		n, e := q.deleteGoogleAuthState(ctx, userID, stateHash)
		if e != nil {
			return e
		}
		if n != 1 {
			missing = true
			return nil
		}
		sealed = st.VerifierSealed
		return nil
	})
	if err != nil {
		return "", err
	}
	if missing {
		return "", ErrAuthStateNotFound
	}
	return sealed, nil
}

// SaveGmailAccount registers a refresh token of the Google account email
// (lower case) for the user. tokenSealedFn seals the token for the ID of
// the account, which is known only here. The same address connected by the
// user again keeps its row: the generation increases, the token is
// replaced, the error state is cleared and the detection window is kept
// (LastSyncedAt and LastTrackedAt stay). A new address is refused with
// ErrTooManyAccounts when the user already has MaxGmailAccounts, and an
// address connected by another user with ErrAccountInUse.
func (s *Store) SaveGmailAccount(ctx context.Context, userID, sessionID, email string, tokenSealedFn func(id string) (string, error), now int64) (GmailAccount, error) {
	var saved GmailAccount
	err := s.transaction(ctx, func(q queries) error {
		// Lock order: users, gmail_accounts.
		if e := lockActiveUser(ctx, q, userID, sessionID, now); e != nil {
			return e
		}
		existing, e := q.lockGmailAccountByEmail(ctx, email)
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		if e == nil {
			if existing.UserID != userID {
				return ErrAccountInUse
			}
			sealed, e := tokenSealedFn(existing.ID)
			if e != nil {
				return e
			}
			next := existing
			next.Generation, next.TokenSealed, next.Status, next.LastError, next.FailureCount = existing.Generation+1, sealed, GmailActive, sql.NullString{}, 0
			next.ConnectedAt, next.NextPollAt, next.UpdatedAt = now, now, now
			n, e := q.reconnectGmailAccount(ctx, next)
			if e != nil {
				return e
			}
			if n != 1 {
				return ErrAccountInUse
			}
			saved = next
			return nil
		}
		count, e := q.countGmailAccounts(ctx, userID)
		if e != nil {
			return e
		}
		if count >= MaxGmailAccounts {
			return ErrTooManyAccounts
		}
		id, e := newID()
		if e != nil {
			return e
		}
		sealed, e := tokenSealedFn(id)
		if e != nil {
			return e
		}
		next := GmailAccount{ID: id, UserID: userID, Email: email, Generation: 1, TokenSealed: sealed, ImmediateNotify: 1, Status: GmailActive, ConnectedAt: now, NextPollAt: now, CreatedAt: now, UpdatedAt: now}
		// Another user may have created the address in the meantime; the
		// insert then does nothing instead of failing on the unique index.
		n, e := q.createGmailAccount(ctx, next)
		if e != nil {
			return e
		}
		if n != 1 {
			return ErrAccountInUse
		}
		saved = next
		return nil
	})
	if err != nil {
		return GmailAccount{}, err
	}
	return saved, nil
}

// GmailAccounts returns the user's accounts in address order. It never
// returns nil.
func (s *Store) GmailAccounts(ctx context.Context, userID string) ([]GmailAccount, error) {
	return s.q.listGmailAccounts(ctx, userID)
}

// GmailAccount returns the user's account, or ErrConnectionNotFound when it
// does not exist or belongs to another user.
func (s *Store) GmailAccount(ctx context.Context, userID, accountID string) (GmailAccount, error) {
	a, err := s.q.gmailAccount(ctx, userID, accountID)
	if errors.Is(err, sql.ErrNoRows) {
		return GmailAccount{}, ErrConnectionNotFound
	}
	return a, err
}

// UpdateGmailSettings changes whether detections of the account are
// notified at once. It returns ErrConnectionNotFound when the account does
// not exist or belongs to another user.
func (s *Store) UpdateGmailSettings(ctx context.Context, userID, sessionID, accountID string, immediateNotify bool, now int64) (GmailAccount, error) {
	var saved GmailAccount
	err := s.transaction(ctx, func(q queries) error {
		if e := lockActiveUser(ctx, q, userID, sessionID, now); e != nil {
			return e
		}
		a, e := q.lockGmailAccount(ctx, userID, accountID)
		if errors.Is(e, sql.ErrNoRows) {
			return ErrConnectionNotFound
		}
		if e != nil {
			return e
		}
		a.ImmediateNotify, a.UpdatedAt = boolInt(immediateNotify), now
		n, e := q.updateGmailSettings(ctx, a)
		if e != nil {
			return e
		}
		if n == 0 {
			return ErrConnectionNotFound
		}
		saved = a
		return nil
	})
	if err != nil {
		return GmailAccount{}, err
	}
	return saved, nil
}

// newID returns a random ID in the form of auth.Random (32 random bytes in
// unpadded base64url, 43 characters), which this package cannot import.
func newID() (string, error) {
	b := make([]byte, 32)
	if _, e := rand.Read(b); e != nil {
		return "", e
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// gmailKeyPrefix is the start of the source keys of the items of the
// account email.
func gmailKeyPrefix(email string) string { return "gmail:" + email + ":" }

// DeleteGmailAccount removes the user's account and returns it, so that the
// caller can revoke its token. The account's items are kept and stop being
// tracked (their source keys stay, so that connecting the address again
// does not create them again), and their detection notifications that were
// not sent yet are dropped. The user's other accounts are not touched. It
// returns ErrConnectionNotFound when the account does not exist or belongs
// to another user.
func (s *Store) DeleteGmailAccount(ctx context.Context, userID, sessionID, accountID string, now int64) (GmailAccount, error) {
	var deleted GmailAccount
	err := s.transaction(ctx, func(q queries) error {
		// Lock order: users, gmail_accounts, action_items, notifications.
		if e := lockActiveUser(ctx, q, userID, sessionID, now); e != nil {
			return e
		}
		a, e := q.lockGmailAccount(ctx, userID, accountID)
		if errors.Is(e, sql.ErrNoRows) {
			return ErrConnectionNotFound
		}
		if e != nil {
			return e
		}
		n, e := q.deleteGmailAccount(ctx, userID, accountID)
		if e != nil {
			return e
		}
		if n != 1 {
			return ErrConnectionNotFound
		}
		prefix := gmailKeyPrefix(a.Email)
		if e = q.stopTrackingSourcePrefixItems(ctx, userID, SourceGmail, prefix, now); e != nil {
			return e
		}
		if e = q.deleteUserPendingNotificationsBySourcePrefix(ctx, userID, NotificationDetected, SourceGmail, prefix); e != nil {
			return e
		}
		deleted = a
		return nil
	})
	if err != nil {
		return GmailAccount{}, err
	}
	return deleted, nil
}

// DueGmailAccounts returns up to limit active accounts of enabled users
// whose next poll time has come, earliest first.
func (s *Store) DueGmailAccounts(ctx context.Context, now int64, limit int) ([]GmailAccount, error) {
	return s.q.listDueGmailAccounts(ctx, now, limit)
}

// GmailJudgedKeys returns the source keys of up to limit of the user's mail
// items whose key starts with prefix (compared exactly, case-sensitively)
// and that were created at or after createdSince, newest first. It reads
// without locking.
func (s *Store) GmailJudgedKeys(ctx context.Context, userID, prefix string, createdSince int64, limit int) ([]string, error) {
	return s.q.listGmailJudgedKeys(ctx, userID, prefix, createdSince, limit)
}

// TrackedSourcePrefixItems returns up to limit open or candidate items of
// source that are tracked and whose source key starts with prefix (compared
// exactly, case-sensitively), least recently checked first. It reads
// without locking.
func (s *Store) TrackedSourcePrefixItems(ctx context.Context, userID, source, prefix string, limit int) ([]ActionItem, error) {
	return s.q.listTrackedSourcePrefixItems(ctx, userID, source, prefix, limit)
}

// GmailTx is one write of a mail synchronization step. Account is the
// account as locked at the start of the transaction.
//
// The tables must be written in the lock order users, gmail_accounts,
// action_items, notifications: GmailSync has locked users and the account,
// so callers write items before notifications.
type GmailTx struct {
	sourceTx
	Account GmailAccount
}

// GmailSync runs fn in a transaction that first locks the user, checks that
// the user is enabled, and locks the user's account of the given
// generation. If any check fails, fn is not called and ErrStaleConnection
// is returned. External requests must never be made inside fn.
func (s *Store) GmailSync(ctx context.Context, userID, accountID string, generation int64, fn func(*GmailTx) error) error {
	return s.transaction(ctx, func(q queries) error {
		if e := lockSyncUser(ctx, q, userID); e != nil {
			return e
		}
		a, e := q.lockGmailAccount(ctx, userID, accountID)
		if errors.Is(e, sql.ErrNoRows) {
			return ErrStaleConnection
		}
		if e != nil {
			return e
		}
		if a.Generation != generation {
			return ErrStaleConnection
		}
		return fn(&GmailTx{sourceTx: sourceTx{ctx: ctx, q: q, userID: userID}, Account: a})
	})
}

// SaveState stores the synchronization fields of a (status, errors, poll
// times and the windows). The token, the address, the setting and
// ConnectedAt are not written.
func (t *GmailTx) SaveState(a GmailAccount) error {
	a.ID, a.UserID, a.Generation = t.Account.ID, t.Account.UserID, t.Account.Generation
	n, e := t.q.updateGmailSyncState(t.ctx, a)
	if e != nil {
		return e
	}
	if n != 1 {
		return ErrStaleConnection
	}
	return nil
}

// UpdateGmailToken stores a refresh token that Google replaced during the
// synchronization, sealed by the caller, without changing the generation.
func (t *GmailTx) UpdateGmailToken(tokenSealed string) error {
	n, e := t.q.updateGmailTokenSealed(t.ctx, t.Account.UserID, t.Account.ID, t.Account.Generation, tokenSealed)
	if e != nil {
		return e
	}
	if n != 1 {
		return ErrStaleConnection
	}
	t.Account.TokenSealed = tokenSealed
	return nil
}
