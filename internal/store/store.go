package store

import (
	"context"
	"database/sql"
	"errors"
	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"
	"net/url"
	"strings"
)

var ErrUnauthorized = errors.New("invalid credentials")
var ErrConflict = errors.New("username already exists")
var ErrNotFound = errors.New("user not found")

type User struct {
	ID           string `json:"id"`
	Username     string `json:"username"`
	PasswordHash string `json:"-"`
	Disabled     int64  `json:"-"`
	CreatedAt    int64  `json:"-"`
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
type queries interface {
	authenticated(context.Context, string, string, int64) (User, error)
	userName(context.Context, string) (User, error)
	userID(context.Context, string) (User, error)
	lockUser(context.Context, string) (User, error)
	createUser(context.Context, User) error
	reset(context.Context, string, string) error
	disable(context.Context, string) error
	createSession(context.Context, Session) error
	session(context.Context, string) (Session, error)
	revoke(context.Context, string, int64) error
	revokeUser(context.Context, string, int64) error
	createRefresh(context.Context, Refresh) error
	refresh(context.Context, string) (Refresh, error)
	consume(context.Context, string, int64) (int64, error)
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
		return nil, errors.New("cannot open database")
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
		return nil, errors.New("cannot connect to database")
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
	return s.q.userName(ctx, name)
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
		existing, e := s.q.userName(ctx, u.Username)
		if e == nil && existing.ID != "" {
			return ErrConflict
		}
	}
	return err
}
func (s *Store) ChangeUser(ctx context.Context, name, hash string, disable bool, now int64) error {
	u, err := s.q.userName(ctx, name)
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
			e = q.disable(ctx, u.ID)
		} else {
			e = q.reset(ctx, u.ID, hash)
		}
		if e != nil {
			return e
		}
		return q.revokeUser(ctx, u.ID, now)
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
func (s *Store) Rotate(ctx context.Context, hash, next string, now int64) (User, Session, error) {
	r, err := s.q.refresh(ctx, hash)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, Session{}, ErrUnauthorized
	}
	if err != nil {
		return User{}, Session{}, err
	}
	sess, err := s.q.session(ctx, r.SessionID)
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
		sess, e = q.session(ctx, r.SessionID)
		if e != nil {
			return e
		}
		r, e = q.refresh(ctx, hash)
		if e != nil {
			return e
		}
		if u.Disabled != 0 || sess.RevokedAt.Valid || sess.ExpiresAt <= now {
			invalid = true
			return nil
		}
		if r.ConsumedAt.Valid {
			invalid = true
			return q.revoke(ctx, sess.ID, now)
		}
		n, e := q.consume(ctx, hash, now)
		if e != nil {
			return e
		}
		if n != 1 {
			invalid = true
			return q.revoke(ctx, sess.ID, now)
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
func (s *Store) Logout(ctx context.Context, hash string, now int64) error {
	r, e := s.q.refresh(ctx, hash)
	if errors.Is(e, sql.ErrNoRows) {
		return nil
	}
	if e != nil {
		return e
	}
	sess, e := s.q.session(ctx, r.SessionID)
	if e != nil {
		return e
	}
	return s.transaction(ctx, func(q queries) error {
		if _, e := q.lockUser(ctx, sess.UserID); e != nil {
			return e
		}
		return q.revoke(ctx, sess.ID, now)
	})
}
func (s *Store) Authenticated(ctx context.Context, uid, sid string, now int64) (User, error) {
	u, err := s.q.authenticated(ctx, uid, sid, now)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrUnauthorized
	}
	return u, err
}
