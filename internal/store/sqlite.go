package store

import (
	"context"
	"database/sql"
	db "github.com/i2bskn/astovia/internal/store/sqlitedb"
)

type sqliteQueries struct{ q *db.Queries }

func (a sqliteQueries) userName(c context.Context, n string) (User, error) {
	u, e := a.q.GetUserByName(c, n)
	return User(u), e
}
func (a sqliteQueries) userID(c context.Context, n string) (User, error) {
	u, e := a.q.GetUserByID(c, n)
	return User(u), e
}
func (a sqliteQueries) lockUser(c context.Context, n string) (User, error) {
	u, e := a.q.LockUserByID(c, n)
	return User(u), e
}
func (a sqliteQueries) authenticated(c context.Context, u, s string, now int64) (User, error) {
	r, e := a.q.GetAuthenticated(c, db.GetAuthenticatedParams{ID: u, ID_2: s, ExpiresAt: now})
	return User(r), e
}
func (a sqliteQueries) createUser(c context.Context, u User) error {
	return a.q.CreateUser(c, db.CreateUserParams{ID: u.ID, Username: u.Username, PasswordHash: u.PasswordHash, CreatedAt: u.CreatedAt})
}
func (a sqliteQueries) reset(c context.Context, id, h string) error {
	return a.q.ResetPassword(c, db.ResetPasswordParams{ID: id, PasswordHash: h})
}
func (a sqliteQueries) disable(c context.Context, id string) error { return a.q.DisableUser(c, id) }
func (a sqliteQueries) createSession(c context.Context, s Session) error {
	return a.q.CreateSession(c, db.CreateSessionParams{ID: s.ID, UserID: s.UserID, ExpiresAt: s.ExpiresAt, CreatedAt: s.CreatedAt})
}
func (a sqliteQueries) session(c context.Context, id string) (Session, error) {
	s, e := a.q.GetSession(c, id)
	return Session(s), e
}
func (a sqliteQueries) revoke(c context.Context, id string, n int64) error {
	return a.q.RevokeSession(c, db.RevokeSessionParams{ID: id, RevokedAt: sql.NullInt64{Int64: n, Valid: true}})
}
func (a sqliteQueries) revokeUser(c context.Context, id string, n int64) error {
	return a.q.RevokeUserSessions(c, db.RevokeUserSessionsParams{UserID: id, RevokedAt: sql.NullInt64{Int64: n, Valid: true}})
}
func (a sqliteQueries) createRefresh(c context.Context, r Refresh) error {
	return a.q.CreateRefresh(c, db.CreateRefreshParams{Hash: r.Hash, SessionID: r.SessionID, CreatedAt: r.CreatedAt})
}
func (a sqliteQueries) refresh(c context.Context, h string) (Refresh, error) {
	r, e := a.q.GetRefresh(c, h)
	return Refresh(r), e
}
func (a sqliteQueries) consume(c context.Context, h string, n int64) (int64, error) {
	return a.q.ConsumeRefresh(c, db.ConsumeRefreshParams{Hash: h, ConsumedAt: sql.NullInt64{Int64: n, Valid: true}})
}
