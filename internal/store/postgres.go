package store

import (
	"context"
	"database/sql"
	db "github.com/i2bskn/astovia/internal/store/postgresdb"
)

type postgresQueries struct{ q *db.Queries }

func (a postgresQueries) userName(c context.Context, n string) (User, error) {
	u, e := a.q.GetUserByName(c, n)
	return User(u), e
}
func (a postgresQueries) userID(c context.Context, n string) (User, error) {
	u, e := a.q.GetUserByID(c, n)
	return User(u), e
}
func (a postgresQueries) lockUser(c context.Context, n string) (User, error) {
	u, e := a.q.LockUserByID(c, n)
	return User(u), e
}
func (a postgresQueries) authenticated(c context.Context, u, s string, now int64) (User, error) {
	r, e := a.q.GetAuthenticated(c, db.GetAuthenticatedParams{ID: u, ID_2: s, ExpiresAt: now})
	return User(r), e
}
func (a postgresQueries) createUser(c context.Context, u User) error {
	return a.q.CreateUser(c, db.CreateUserParams{ID: u.ID, Username: u.Username, PasswordHash: u.PasswordHash, CreatedAt: u.CreatedAt})
}
func (a postgresQueries) reset(c context.Context, id, h string) error {
	return a.q.ResetPassword(c, db.ResetPasswordParams{ID: id, PasswordHash: h})
}
func (a postgresQueries) disable(c context.Context, id string) error { return a.q.DisableUser(c, id) }
func (a postgresQueries) createSession(c context.Context, s Session) error {
	return a.q.CreateSession(c, db.CreateSessionParams{ID: s.ID, UserID: s.UserID, ExpiresAt: s.ExpiresAt, CreatedAt: s.CreatedAt})
}
func (a postgresQueries) session(c context.Context, id string) (Session, error) {
	s, e := a.q.GetSession(c, id)
	return Session(s), e
}
func (a postgresQueries) revoke(c context.Context, id string, n int64) error {
	return a.q.RevokeSession(c, db.RevokeSessionParams{ID: id, RevokedAt: sql.NullInt64{Int64: n, Valid: true}})
}
func (a postgresQueries) revokeUser(c context.Context, id string, n int64) error {
	return a.q.RevokeUserSessions(c, db.RevokeUserSessionsParams{UserID: id, RevokedAt: sql.NullInt64{Int64: n, Valid: true}})
}
func (a postgresQueries) createRefresh(c context.Context, r Refresh) error {
	return a.q.CreateRefresh(c, db.CreateRefreshParams{Hash: r.Hash, SessionID: r.SessionID, CreatedAt: r.CreatedAt})
}
func (a postgresQueries) refresh(c context.Context, h string) (Refresh, error) {
	r, e := a.q.GetRefresh(c, h)
	return Refresh(r), e
}
func (a postgresQueries) consume(c context.Context, h string, n int64) (int64, error) {
	return a.q.ConsumeRefresh(c, db.ConsumeRefreshParams{Hash: h, ConsumedAt: sql.NullInt64{Int64: n, Valid: true}})
}
