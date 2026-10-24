package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	_ "github.com/jackc/pgx/v5/stdlib"
	"math"
	_ "modernc.org/sqlite"
	"net/url"
	"strings"
)

var ErrUnauthorized = errors.New("invalid credentials")
var ErrConflict = errors.New("email already exists")
var ErrNotFound = errors.New("user not found")
var ErrItemNotFound = errors.New("action item not found")
var ErrVersionConflict = errors.New("action item was updated by another request")
var ErrTokenNotFound = errors.New("API token not found")
var ErrTooManyTokens = errors.New("too many API tokens")

// MaxAPITokens is the number of API tokens a user can hold at once.
const MaxAPITokens = 50

// User, Session and Refresh are storage types. They are not serialized
// directly; HTTP handlers define their own response types.
type User struct {
	ID           string
	Email        string // Callers normalize it with auth.Email; case-insensitive uniqueness relies on that.
	PasswordHash string
	Disabled     int64
	CreatedAt    int64
}
type Session struct {
	ID, UserID string
	ExpiresAt  int64
	RevokedAt  sql.NullInt64
	CreatedAt  int64
}
type Refresh struct {
	Hash, SessionID string
	ConsumedAt      sql.NullInt64
	CreatedAt       int64
}
type ActionItem struct {
	ID, UserID, Content, Status string
	Note                        sql.NullString
	DueAt                       sql.NullInt64
	Refs                        string // JSON array; validated and normalized by the HTTP handlers.
	Version                     int64
	CreatedAt                   int64
	UpdatedAt                   int64
}

// APIToken is a long-lived bearer token. Only the SHA-256 hash of the secret
// is stored, and it is passed separately when the token is created so that it
// is never read back.
type APIToken struct {
	ID, UserID, Name string
	CreatedAt        int64
	LastUsedAt       sql.NullInt64
}

// ActionItemCursor is the position after which ListActionItems continues,
// in created_at DESC, id DESC order.
type ActionItemCursor struct {
	CreatedAt int64
	ID        string
}

// queries adapts the sqlc packages generated for each database. sqlc emits
// distinct types per package, so each driver has a small adapter that is
// checked against this interface at compile time.
type queries interface {
	userByEmail(ctx context.Context, email string) (User, error)
	userByID(ctx context.Context, id string) (User, error)
	lockUser(ctx context.Context, id string) (User, error)
	createUser(ctx context.Context, user User) error
	resetPassword(ctx context.Context, id, hash string) error
	disableUser(ctx context.Context, id string) error
	createSession(ctx context.Context, session Session) error
	sessionByID(ctx context.Context, id string) (Session, error)
	activeSession(ctx context.Context, id, userID string, now int64) (Session, error)
	revokeSession(ctx context.Context, id string, now int64) error
	revokeUserSessions(ctx context.Context, userID string, now int64) error
	createRefresh(ctx context.Context, refresh Refresh) error
	refreshByHash(ctx context.Context, hash string) (Refresh, error)
	consumeRefresh(ctx context.Context, hash string, now int64) (int64, error)
	createActionItem(ctx context.Context, item ActionItem) error
	actionItem(ctx context.Context, userID, id string) (ActionItem, error)
	lockActionItem(ctx context.Context, userID, id string) (ActionItem, error)
	listActionItems(ctx context.Context, userID, status string, after ActionItemCursor, limit int) ([]ActionItem, error)
	updateActionItem(ctx context.Context, item ActionItem, currentVersion int64) (int64, error)
	createAPIToken(ctx context.Context, token APIToken, hash string) error
	countAPITokens(ctx context.Context, userID string) (int64, error)
	listAPITokens(ctx context.Context, userID string) ([]APIToken, error)
	apiTokenByHash(ctx context.Context, hash string) (APIToken, error)
	deleteAPIToken(ctx context.Context, userID, id string) (int64, error)
	touchAPIToken(ctx context.Context, id string, now, before int64) error
	deleteUserAPITokens(ctx context.Context, userID string) error
	oauthClient(ctx context.Context, clientID string) (OAuthClient, error)
	createOAuthClient(ctx context.Context, c OAuthClient, secretHash sql.NullString) error
	upsertCIMDClient(ctx context.Context, c OAuthClient) error
	createOAuthGrant(ctx context.Context, g OAuthGrant) error
	oauthGrantByCode(ctx context.Context, codeHash string) (OAuthGrant, error)
	lockOAuthGrantByCode(ctx context.Context, codeHash, userID string) (OAuthGrant, error)
	oauthGrant(ctx context.Context, userID, id string) (OAuthGrant, error)
	markOAuthCodeUsed(ctx context.Context, userID, id string, now int64) (int64, error)
	extendOAuthGrant(ctx context.Context, userID, id string, expiresAt int64) error
	listOAuthGrants(ctx context.Context, userID string, now int64) ([]OAuthGrant, error)
	deleteOAuthGrant(ctx context.Context, userID, id string) (int64, error)
	touchOAuthGrant(ctx context.Context, userID, id string, now, before int64) error
	deleteExpiredOAuthGrants(ctx context.Context, userID string, now int64) error
	deleteUserOAuthGrants(ctx context.Context, userID string) error
	createOAuthToken(ctx context.Context, t OAuthToken) error
	oauthTokenByHash(ctx context.Context, hash string) (OAuthToken, error)
	lockOAuthTokenByHash(ctx context.Context, hash, userID string) (OAuthToken, error)
	deleteOAuthToken(ctx context.Context, userID, hash string) (int64, error)
	deleteGrantOAuthTokens(ctx context.Context, userID, grantID string) error
	deleteExpiredOAuthTokens(ctx context.Context, userID string, now int64) error
	deleteUserOAuthTokens(ctx context.Context, userID string) error
}
type Store struct {
	db     *sql.DB
	q      queries
	driver string
}

func Open(ctx context.Context, driver, dsn string) (*Store, error) {
	name := driver
	if driver == "postgres" {
		name = "pgx"
	} else if driver == "sqlite" {
		sep := "?"
		if strings.Contains(dsn, "?") {
			sep = "&"
		}
		dsn += sep + "_pragma=" + url.QueryEscape("foreign_keys(1)") + "&_pragma=" + url.QueryEscape("busy_timeout(5000)") + "&_txlock=immediate"
	} else {
		return nil, errors.New("unsupported database")
	}
	db, err := sql.Open(name, dsn)
	if err != nil {
		return nil, fmt.Errorf("cannot open database: %w", err)
	}
	if driver == "sqlite" {
		db.SetMaxOpenConns(1)
	} else {
		db.SetMaxOpenConns(10)
	}
	s := &Store{db: db, driver: driver}
	s.q = s.bind(db)
	if err = db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("cannot connect to database: %w", err)
	}
	return s, nil
}
func (s *Store) Close() error { return s.db.Close() }
func (s *Store) CheckSchema(ctx context.Context) error {
	for _, q := range []string{"SELECT id,email,password_hash,disabled,created_at FROM users LIMIT 0", "SELECT id,user_id,expires_at,revoked_at,created_at FROM sessions LIMIT 0", "SELECT hash,session_id,consumed_at,created_at FROM refresh_tokens LIMIT 0", "SELECT id,user_id,content,status,note,due_at,refs,version,created_at,updated_at FROM action_items LIMIT 0", "SELECT id,user_id,name,token_hash,created_at,last_used_at FROM api_tokens LIMIT 0", "SELECT client_id,kind,name,redirect_uris,auth_method,secret_hash,metadata_expires_at,created_at,updated_at FROM oauth_clients LIMIT 0", "SELECT id,user_id,client_id,client_name,scope,resource,redirect_uri,code_hash,code_challenge,code_expires_at,code_used_at,created_at,expires_at,last_used_at FROM oauth_grants LIMIT 0", "SELECT token_hash,kind,grant_id,user_id,created_at,expires_at FROM oauth_tokens LIMIT 0"} {
		rows, e := s.db.QueryContext(ctx, q)
		if e != nil {
			return errors.New("database schema is not initialized or incompatible; apply the matching Atlas schema first")
		}
		if e = rows.Close(); e != nil {
			return e
		}
	}
	return nil
}
func (s *Store) UserByEmail(ctx context.Context, email string) (User, error) {
	return s.q.userByEmail(ctx, email)
}
func (s *Store) transaction(ctx context.Context, fn func(queries) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err = fn(s.bind(tx)); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) CreateUser(ctx context.Context, u User) error {
	err := s.transaction(ctx, func(q queries) error { return q.createUser(ctx, u) })
	if err != nil {
		existing, e := s.q.userByEmail(ctx, u.Email)
		if e == nil && existing.ID != "" {
			return ErrConflict
		}
	}
	return err
}
func (s *Store) ChangeUser(ctx context.Context, email, hash string, disable bool, now int64) error {
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
		var e error
		if disable {
			e = q.disableUser(ctx, u.ID)
		} else {
			e = q.resetPassword(ctx, u.ID, hash)
		}
		if e != nil {
			return e
		}
		if e = q.revokeUserSessions(ctx, u.ID, now); e != nil {
			return e
		}
		// API tokens have no expiry, so a token issued from a stolen access
		// token must not survive the password reset that recovers the account.
		// OAuth connections are granted from a session as well and go with it.
		// Tokens are deleted before grants because they reference them.
		if e = q.deleteUserAPITokens(ctx, u.ID); e != nil {
			return e
		}
		if e = q.deleteUserOAuthTokens(ctx, u.ID); e != nil {
			return e
		}
		return q.deleteUserOAuthGrants(ctx, u.ID)
	})
}
func (s *Store) Login(ctx context.Context, verified User, session Session, refresh Refresh) error {
	return s.transaction(ctx, func(q queries) error {
		u, e := q.lockUser(ctx, verified.ID)
		if e != nil {
			return e
		}
		if u.Disabled != 0 || u.PasswordHash != verified.PasswordHash {
			return ErrUnauthorized
		}
		if e = q.createSession(ctx, session); e != nil {
			return e
		}
		return q.createRefresh(ctx, refresh)
	})
}

// Rotate consumes a refresh token and stores its successor. The token and
// session are first read outside the transaction only to learn user_id, so
// that the transaction can lock the user row before re-reading the session and
// token. Every writer locks user before session, keeping the lock order fixed.
func (s *Store) Rotate(ctx context.Context, hash, next string, now int64) (User, Session, error) {
	r, err := s.q.refreshByHash(ctx, hash)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, Session{}, ErrUnauthorized
	}
	if err != nil {
		return User{}, Session{}, err
	}
	sess, err := s.q.sessionByID(ctx, r.SessionID)
	if err != nil {
		return User{}, Session{}, err
	}
	var u User
	var invalid bool
	err = s.transaction(ctx, func(q queries) error {
		var e error
		u, e = q.lockUser(ctx, sess.UserID)
		if e != nil {
			return e
		}
		sess, e = q.sessionByID(ctx, r.SessionID)
		if e != nil {
			return e
		}
		r, e = q.refreshByHash(ctx, hash)
		if e != nil {
			return e
		}
		if u.Disabled != 0 || sess.RevokedAt.Valid || sess.ExpiresAt <= now {
			invalid = true
			return nil
		}
		if r.ConsumedAt.Valid {
			invalid = true
			return q.revokeSession(ctx, sess.ID, now)
		}
		n, e := q.consumeRefresh(ctx, hash, now)
		if e != nil {
			return e
		}
		if n != 1 {
			invalid = true
			return q.revokeSession(ctx, sess.ID, now)
		}
		return q.createRefresh(ctx, Refresh{Hash: next, SessionID: sess.ID, CreatedAt: now})
	})
	if err != nil {
		return User{}, Session{}, err
	}
	if invalid {
		return User{}, Session{}, ErrUnauthorized
	}
	return u, sess, nil
}

// Logout revokes the session of a refresh token. As in Rotate, user_id is read
// outside the transaction so the user row can be locked before the session.
func (s *Store) Logout(ctx context.Context, hash string, now int64) error {
	r, e := s.q.refreshByHash(ctx, hash)
	if errors.Is(e, sql.ErrNoRows) {
		return nil
	}
	if e != nil {
		return e
	}
	sess, e := s.q.sessionByID(ctx, r.SessionID)
	if e != nil {
		return e
	}
	return s.transaction(ctx, func(q queries) error {
		if _, e := q.lockUser(ctx, sess.UserID); e != nil {
			return e
		}
		return q.revokeSession(ctx, sess.ID, now)
	})
}

// Authenticated returns the user for an access token only while the session
// belongs to that user, is not revoked or expired, and the user is enabled.
func (s *Store) Authenticated(ctx context.Context, uid, sid string, now int64) (User, error) {
	if _, err := s.q.activeSession(ctx, sid, uid, now); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return User{}, ErrUnauthorized
		}
		return User{}, err
	}
	u, err := s.q.userByID(ctx, uid)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrUnauthorized
	}
	if err != nil {
		return User{}, err
	}
	if u.Disabled != 0 {
		return User{}, ErrUnauthorized
	}
	return u, nil
}

func (s *Store) CreateActionItem(ctx context.Context, item ActionItem) error {
	return s.q.createActionItem(ctx, item)
}

// ActionItem returns the user's item, or ErrItemNotFound when the item does
// not exist or belongs to another user.
func (s *Store) ActionItem(ctx context.Context, userID, id string) (ActionItem, error) {
	item, err := s.q.actionItem(ctx, userID, id)
	if errors.Is(err, sql.ErrNoRows) {
		return ActionItem{}, ErrItemNotFound
	}
	return item, err
}

// ListActionItems returns up to limit of the user's items after the cursor in
// created_at DESC, id DESC order. An empty status lists every status.
func (s *Store) ListActionItems(ctx context.Context, userID, status string, after *ActionItemCursor, limit int) ([]ActionItem, error) {
	// The first page uses a cursor that sorts after every row.
	c := ActionItemCursor{CreatedAt: math.MaxInt64}
	if after != nil {
		c = *after
	}
	return s.q.listActionItems(ctx, userID, status, c, limit)
}

// UpdateActionItem locks the user's item, lets fn modify it and stores the
// result with the version incremented. ID, UserID and CreatedAt cannot be
// changed by fn. An error from fn is returned as is and nothing is stored.
// The transaction explicitly locks only the target action_items row.
func (s *Store) UpdateActionItem(ctx context.Context, userID, id string, fn func(*ActionItem) error) (ActionItem, error) {
	var updated ActionItem
	err := s.transaction(ctx, func(q queries) error {
		item, e := q.lockActionItem(ctx, userID, id)
		if errors.Is(e, sql.ErrNoRows) {
			return ErrItemNotFound
		}
		if e != nil {
			return e
		}
		next := item
		if e = fn(&next); e != nil {
			return e
		}
		next.ID, next.UserID, next.CreatedAt, next.Version = item.ID, item.UserID, item.CreatedAt, item.Version+1
		n, e := q.updateActionItem(ctx, next, item.Version)
		if e != nil {
			return e
		}
		if n != 1 {
			return ErrVersionConflict
		}
		updated = next
		return nil
	})
	if err != nil {
		return ActionItem{}, err
	}
	return updated, nil
}

// CreateAPIToken stores a new token for t.UserID whose secret hashes to hash.
// The access token that requested it must belong to sessionID, which is
// checked again here: the HTTP layer verified the session outside this
// transaction, so a password reset may have revoked it in between.
func (s *Store) CreateAPIToken(ctx context.Context, t APIToken, hash, sessionID string, now int64) error {
	return s.transaction(ctx, func(q queries) error {
		// Lock the user row before re-reading the session. This serializes
		// with ChangeUser, which revokes sessions and deletes tokens on a
		// password reset or disable; do not change this order.
		u, e := q.lockUser(ctx, t.UserID)
		if errors.Is(e, sql.ErrNoRows) {
			return ErrUnauthorized
		}
		if e != nil {
			return e
		}
		if u.Disabled != 0 {
			return ErrUnauthorized
		}
		if _, e = q.activeSession(ctx, sessionID, t.UserID, now); e != nil {
			if errors.Is(e, sql.ErrNoRows) {
				return ErrUnauthorized
			}
			return e
		}
		n, e := q.countAPITokens(ctx, t.UserID)
		if e != nil {
			return e
		}
		if n >= MaxAPITokens {
			return ErrTooManyTokens
		}
		return q.createAPIToken(ctx, t, hash)
	})
}

// ListAPITokens returns the user's tokens, newest first. It never returns nil.
func (s *Store) ListAPITokens(ctx context.Context, userID string) ([]APIToken, error) {
	return s.q.listAPITokens(ctx, userID)
}

// DeleteAPIToken deletes the user's token, or returns ErrTokenNotFound when
// it does not exist or belongs to another user.
func (s *Store) DeleteAPIToken(ctx context.Context, userID, id string) error {
	n, err := s.q.deleteAPIToken(ctx, userID, id)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrTokenNotFound
	}
	return nil
}

// AuthenticateAPIToken returns the enabled user owning the token whose secret
// hashes to hash, or ErrUnauthorized. It also records the time of use, at
// most once a minute. A failure to record it does not fail authentication;
// it is returned as touchErr for the caller to log. Database errors are
// returned as err unchanged.
func (s *Store) AuthenticateAPIToken(ctx context.Context, hash string, now int64) (u User, touchErr error, err error) {
	t, err := s.q.apiTokenByHash(ctx, hash)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, nil, ErrUnauthorized
	}
	if err != nil {
		return User{}, nil, err
	}
	u, err = s.q.userByID(ctx, t.UserID)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, nil, ErrUnauthorized
	}
	if err != nil {
		return User{}, nil, err
	}
	if u.Disabled != 0 {
		return User{}, nil, ErrUnauthorized
	}
	return u, s.q.touchAPIToken(ctx, t.ID, now, now-60), nil
}
