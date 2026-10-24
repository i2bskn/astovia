package store

import (
	"context"
	"database/sql"
	db "github.com/i2bskn/astovia/internal/store/postgresdb"
)

type postgresQueries struct{ q *db.Queries }

var _ queries = postgresQueries{}

func (q postgresQueries) userByEmail(ctx context.Context, email string) (User, error) {
	user, err := q.q.GetUserByEmail(ctx, email)
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
	return q.q.CreateUser(ctx, db.CreateUserParams{ID: user.ID, Email: user.Email, PasswordHash: user.PasswordHash, CreatedAt: user.CreatedAt})
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
func (q postgresQueries) createActionItem(ctx context.Context, item ActionItem) error {
	return q.q.CreateActionItem(ctx, db.CreateActionItemParams(item))
}
func (q postgresQueries) actionItem(ctx context.Context, userID, id string) (ActionItem, error) {
	item, err := q.q.GetActionItem(ctx, db.GetActionItemParams{ID: id, UserID: userID})
	return ActionItem(item), err
}
func (q postgresQueries) lockActionItem(ctx context.Context, userID, id string) (ActionItem, error) {
	item, err := q.q.LockActionItem(ctx, db.LockActionItemParams{ID: id, UserID: userID})
	return ActionItem(item), err
}

// limit is validated by the HTTP handler (at most 101), so int32 cannot overflow.
func (q postgresQueries) listActionItems(ctx context.Context, userID, status string, after ActionItemCursor, limit int) ([]ActionItem, error) {
	var rows []db.ActionItem
	var err error
	if status == "" {
		rows, err = q.q.ListActionItems(ctx, db.ListActionItemsParams{UserID: userID, BeforeCreatedAt: after.CreatedAt, TieCreatedAt: after.CreatedAt, BeforeID: after.ID, PageSize: int32(limit)})
	} else {
		rows, err = q.q.ListActionItemsByStatus(ctx, db.ListActionItemsByStatusParams{UserID: userID, Status: status, BeforeCreatedAt: after.CreatedAt, TieCreatedAt: after.CreatedAt, BeforeID: after.ID, PageSize: int32(limit)})
	}
	if err != nil {
		return nil, err
	}
	items := make([]ActionItem, 0, len(rows))
	for _, row := range rows {
		items = append(items, ActionItem(row))
	}
	return items, nil
}
func (q postgresQueries) updateActionItem(ctx context.Context, item ActionItem, currentVersion int64) (int64, error) {
	return q.q.UpdateActionItem(ctx, db.UpdateActionItemParams{Content: item.Content, Status: item.Status, Note: item.Note, DueAt: item.DueAt, Refs: item.Refs, NewVersion: item.Version, UpdatedAt: item.UpdatedAt, ID: item.ID, UserID: item.UserID, CurrentVersion: currentVersion})
}
func (q postgresQueries) createAPIToken(ctx context.Context, token APIToken, hash string) error {
	return q.q.CreateAPIToken(ctx, db.CreateAPITokenParams{ID: token.ID, UserID: token.UserID, Name: token.Name, TokenHash: hash, CreatedAt: token.CreatedAt})
}
func (q postgresQueries) countAPITokens(ctx context.Context, userID string) (int64, error) {
	return q.q.CountAPITokens(ctx, userID)
}
func (q postgresQueries) listAPITokens(ctx context.Context, userID string) ([]APIToken, error) {
	rows, err := q.q.ListAPITokens(ctx, userID)
	if err != nil {
		return nil, err
	}
	tokens := make([]APIToken, 0, len(rows))
	for _, row := range rows {
		tokens = append(tokens, APIToken{ID: row.ID, UserID: row.UserID, Name: row.Name, CreatedAt: row.CreatedAt, LastUsedAt: row.LastUsedAt})
	}
	return tokens, nil
}
func (q postgresQueries) apiTokenByHash(ctx context.Context, hash string) (APIToken, error) {
	row, err := q.q.GetAPITokenByHash(ctx, hash)
	return APIToken{ID: row.ID, UserID: row.UserID, LastUsedAt: row.LastUsedAt}, err
}
func (q postgresQueries) deleteAPIToken(ctx context.Context, userID, id string) (int64, error) {
	return q.q.DeleteAPIToken(ctx, db.DeleteAPITokenParams{ID: id, UserID: userID})
}
func (q postgresQueries) touchAPIToken(ctx context.Context, id string, now, before int64) error {
	return q.q.TouchAPIToken(ctx, db.TouchAPITokenParams{LastUsedAt: sql.NullInt64{Int64: now, Valid: true}, ID: id, TouchedBefore: sql.NullInt64{Int64: before, Valid: true}})
}
func (q postgresQueries) deleteUserAPITokens(ctx context.Context, userID string) error {
	return q.q.DeleteUserAPITokens(ctx, userID)
}
func (q postgresQueries) oauthClient(ctx context.Context, clientID string) (OAuthClient, error) {
	row, err := q.q.GetOAuthClient(ctx, clientID)
	if err != nil {
		return OAuthClient{}, err
	}
	uris, err := decodeRedirectURIs(row.RedirectUris)
	if err != nil {
		return OAuthClient{}, err
	}
	return OAuthClient{ClientID: row.ClientID, Kind: row.Kind, Name: row.Name, RedirectURIs: uris, AuthMethod: row.AuthMethod, SecretHash: row.SecretHash, MetadataExpiresAt: row.MetadataExpiresAt, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}, nil
}
func (q postgresQueries) createOAuthClient(ctx context.Context, c OAuthClient, secretHash sql.NullString) error {
	uris, err := encodeRedirectURIs(c.RedirectURIs)
	if err != nil {
		return err
	}
	return q.q.CreateOAuthClient(ctx, db.CreateOAuthClientParams{ClientID: c.ClientID, Kind: c.Kind, Name: c.Name, RedirectUris: uris, AuthMethod: c.AuthMethod, SecretHash: secretHash, MetadataExpiresAt: c.MetadataExpiresAt, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt})
}
func (q postgresQueries) upsertCIMDClient(ctx context.Context, c OAuthClient) error {
	uris, err := encodeRedirectURIs(c.RedirectURIs)
	if err != nil {
		return err
	}
	return q.q.UpsertCIMDClient(ctx, db.UpsertCIMDClientParams{ClientID: c.ClientID, Name: c.Name, RedirectUris: uris, MetadataExpiresAt: c.MetadataExpiresAt, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt})
}
func (q postgresQueries) createOAuthGrant(ctx context.Context, g OAuthGrant) error {
	return q.q.CreateOAuthGrant(ctx, db.CreateOAuthGrantParams{ID: g.ID, UserID: g.UserID, ClientID: g.ClientID, ClientName: g.ClientName, Scope: g.Scope, Resource: g.Resource, RedirectUri: g.RedirectURI, CodeHash: g.CodeHash, CodeChallenge: g.CodeChallenge, CodeExpiresAt: g.CodeExpiresAt, CreatedAt: g.CreatedAt, ExpiresAt: g.ExpiresAt})
}
func (q postgresQueries) oauthGrantByCode(ctx context.Context, codeHash string) (OAuthGrant, error) {
	row, err := q.q.GetOAuthGrantByCode(ctx, codeHash)
	return q.grant(row), err
}
func (q postgresQueries) lockOAuthGrantByCode(ctx context.Context, codeHash, userID string) (OAuthGrant, error) {
	row, err := q.q.LockOAuthGrantByCode(ctx, db.LockOAuthGrantByCodeParams{CodeHash: codeHash, UserID: userID})
	return q.grant(row), err
}
func (q postgresQueries) oauthGrant(ctx context.Context, userID, id string) (OAuthGrant, error) {
	row, err := q.q.GetOAuthGrant(ctx, db.GetOAuthGrantParams{ID: id, UserID: userID})
	return q.grant(row), err
}
func (q postgresQueries) markOAuthCodeUsed(ctx context.Context, userID, id string, now int64) (int64, error) {
	return q.q.MarkOAuthCodeUsed(ctx, db.MarkOAuthCodeUsedParams{CodeUsedAt: sql.NullInt64{Int64: now, Valid: true}, ID: id, UserID: userID})
}
func (q postgresQueries) extendOAuthGrant(ctx context.Context, userID, id string, expiresAt int64) error {
	return q.q.ExtendOAuthGrant(ctx, db.ExtendOAuthGrantParams{ExpiresAt: expiresAt, ID: id, UserID: userID})
}
func (q postgresQueries) listOAuthGrants(ctx context.Context, userID string, now int64) ([]OAuthGrant, error) {
	rows, err := q.q.ListOAuthGrants(ctx, db.ListOAuthGrantsParams{UserID: userID, Now: now})
	if err != nil {
		return nil, err
	}
	grants := make([]OAuthGrant, 0, len(rows))
	for _, row := range rows {
		grants = append(grants, q.grant(row))
	}
	return grants, nil
}
func (q postgresQueries) deleteOAuthGrant(ctx context.Context, userID, id string) (int64, error) {
	return q.q.DeleteOAuthGrant(ctx, db.DeleteOAuthGrantParams{ID: id, UserID: userID})
}
func (q postgresQueries) touchOAuthGrant(ctx context.Context, userID, id string, now, before int64) error {
	return q.q.TouchOAuthGrant(ctx, db.TouchOAuthGrantParams{LastUsedAt: sql.NullInt64{Int64: now, Valid: true}, ID: id, UserID: userID, TouchedBefore: sql.NullInt64{Int64: before, Valid: true}})
}
func (q postgresQueries) deleteExpiredOAuthGrants(ctx context.Context, userID string, now int64) error {
	return q.q.DeleteExpiredOAuthGrants(ctx, db.DeleteExpiredOAuthGrantsParams{UserID: userID, Now: now})
}
func (q postgresQueries) deleteUserOAuthGrants(ctx context.Context, userID string) error {
	return q.q.DeleteUserOAuthGrants(ctx, userID)
}
func (q postgresQueries) createOAuthToken(ctx context.Context, t OAuthToken) error {
	return q.q.CreateOAuthToken(ctx, db.CreateOAuthTokenParams{TokenHash: t.Hash, Kind: t.Kind, GrantID: t.GrantID, UserID: t.UserID, CreatedAt: t.CreatedAt, ExpiresAt: t.ExpiresAt})
}
func (q postgresQueries) oauthTokenByHash(ctx context.Context, hash string) (OAuthToken, error) {
	row, err := q.q.GetOAuthTokenByHash(ctx, hash)
	return q.token(row), err
}
func (q postgresQueries) lockOAuthTokenByHash(ctx context.Context, hash, userID string) (OAuthToken, error) {
	row, err := q.q.LockOAuthTokenByHash(ctx, db.LockOAuthTokenByHashParams{TokenHash: hash, UserID: userID})
	return q.token(row), err
}
func (q postgresQueries) deleteOAuthToken(ctx context.Context, userID, hash string) (int64, error) {
	return q.q.DeleteOAuthToken(ctx, db.DeleteOAuthTokenParams{TokenHash: hash, UserID: userID})
}
func (q postgresQueries) deleteGrantOAuthTokens(ctx context.Context, userID, grantID string) error {
	return q.q.DeleteGrantOAuthTokens(ctx, db.DeleteGrantOAuthTokensParams{GrantID: grantID, UserID: userID})
}
func (q postgresQueries) deleteExpiredOAuthTokens(ctx context.Context, userID string, now int64) error {
	return q.q.DeleteExpiredOAuthTokens(ctx, db.DeleteExpiredOAuthTokensParams{UserID: userID, Now: now})
}
func (q postgresQueries) deleteUserOAuthTokens(ctx context.Context, userID string) error {
	return q.q.DeleteUserOAuthTokens(ctx, userID)
}
func (postgresQueries) grant(row db.OauthGrant) OAuthGrant {
	return OAuthGrant{ID: row.ID, UserID: row.UserID, ClientID: row.ClientID, ClientName: row.ClientName, Scope: row.Scope, Resource: row.Resource, RedirectURI: row.RedirectUri, CodeHash: row.CodeHash, CodeChallenge: row.CodeChallenge, CodeExpiresAt: row.CodeExpiresAt, CodeUsedAt: row.CodeUsedAt, CreatedAt: row.CreatedAt, ExpiresAt: row.ExpiresAt, LastUsedAt: row.LastUsedAt}
}
func (postgresQueries) token(row db.OauthToken) OAuthToken {
	return OAuthToken{Hash: row.TokenHash, Kind: row.Kind, GrantID: row.GrantID, UserID: row.UserID, CreatedAt: row.CreatedAt, ExpiresAt: row.ExpiresAt}
}
