package store

import (
	"context"
	"database/sql"
	db "github.com/i2bskn/astovia/internal/store/postgresdb"
)

type postgresQueries struct{ q *db.Queries }

var _ queries = postgresQueries{}

func (q postgresQueries) userByName(ctx context.Context, name string) (User, error) {
	user, err := q.q.GetUserByName(ctx, name)
	return User(user), err
}
func (q postgresQueries) userByID(ctx context.Context, id string) (User, error) {
	user, err := q.q.GetUserByID(ctx, id)
	return User(user), err
}
func (q postgresQueries) lockUser(ctx context.Context, id string) (User, error) {
	user, err := q.q.LockUserByID(ctx, id)
	return User(user), err
}
func (q postgresQueries) createUser(ctx context.Context, user User) error {
	return q.q.CreateUser(ctx, db.CreateUserParams{ID: user.ID, Username: user.Username, PasswordHash: user.PasswordHash, CreatedAt: user.CreatedAt})
}
func (q postgresQueries) resetPassword(ctx context.Context, id, hash string) error {
	return q.q.ResetPassword(ctx, db.ResetPasswordParams{ID: id, PasswordHash: hash})
}
func (q postgresQueries) disableUser(ctx context.Context, id string) error {
	return q.q.DisableUser(ctx, id)
}
func (q postgresQueries) createSession(ctx context.Context, session Session) error {
	return q.q.CreateSession(ctx, db.CreateSessionParams{ID: session.ID, UserID: session.UserID, ExpiresAt: session.ExpiresAt, CreatedAt: session.CreatedAt})
}
func (q postgresQueries) sessionByID(ctx context.Context, id string) (Session, error) {
	session, err := q.q.GetSession(ctx, id)
	return Session(session), err
}
func (q postgresQueries) activeSession(ctx context.Context, id, userID string, now int64) (Session, error) {
	session, err := q.q.GetActiveSession(ctx, db.GetActiveSessionParams{ID: id, UserID: userID, Now: now})
	return Session(session), err
}
func (q postgresQueries) revokeSession(ctx context.Context, id string, now int64) error {
	return q.q.RevokeSession(ctx, db.RevokeSessionParams{ID: id, RevokedAt: sql.NullInt64{Int64: now, Valid: true}})
}
func (q postgresQueries) revokeUserSessions(ctx context.Context, userID string, now int64) error {
	return q.q.RevokeUserSessions(ctx, db.RevokeUserSessionsParams{UserID: userID, RevokedAt: sql.NullInt64{Int64: now, Valid: true}})
}
func (q postgresQueries) createRefresh(ctx context.Context, refresh Refresh) error {
	return q.q.CreateRefresh(ctx, db.CreateRefreshParams{Hash: refresh.Hash, SessionID: refresh.SessionID, CreatedAt: refresh.CreatedAt})
}
func (q postgresQueries) refreshByHash(ctx context.Context, hash string) (Refresh, error) {
	refresh, err := q.q.GetRefresh(ctx, hash)
	return Refresh(refresh), err
}
func (q postgresQueries) consumeRefresh(ctx context.Context, hash string, now int64) (int64, error) {
	return q.q.ConsumeRefresh(ctx, db.ConsumeRefreshParams{Hash: hash, ConsumedAt: sql.NullInt64{Int64: now, Valid: true}})
}
