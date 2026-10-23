package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"
	"net/url"
	"strings"
)

var ErrUnauthorized = errors.New("invalid credentials")
var ErrConflict = errors.New("username already exists")
var ErrNotFound = errors.New("user not found")

// User, Session and Refresh are storage types. They are not serialized
// directly; HTTP handlers define their own response types.
type User struct {
	ID           string
	Username     string
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

// queries adapts the sqlc packages generated for each database. sqlc emits
// distinct types per package, so each driver has a small adapter that is
// checked against this interface at compile time.
type queries interface {
	userByName(ctx context.Context, name string) (User, error)
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
	for _, q := range []string{"SELECT id,username,password_hash,disabled,created_at FROM users LIMIT 0", "SELECT id,user_id,expires_at,revoked_at,created_at FROM sessions LIMIT 0", "SELECT hash,session_id,consumed_at,created_at FROM refresh_tokens LIMIT 0"} {
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
func (s *Store) UserByName(ctx context.Context, name string) (User, error) {
	return s.q.userByName(ctx, name)
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
		existing, e := s.q.userByName(ctx, u.Username)
		if e == nil && existing.ID != "" {
			return ErrConflict
		}
	}
	return err
}
func (s *Store) ChangeUser(ctx context.Context, name, hash string, disable bool, now int64) error {
	u, err := s.q.userByName(ctx, name)
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
		return q.revokeUserSessions(ctx, u.ID, now)
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
