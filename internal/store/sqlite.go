package store

import (
	"context"
	"database/sql"
	db "github.com/i2bskn/astovia/internal/store/sqlitedb"
)

type sqliteQueries struct{ q *db.Queries }

var _ queries = sqliteQueries{}

func (q sqliteQueries) userByEmail(ctx context.Context, email string) (User, error) {
	user, err := q.q.GetUserByEmail(ctx, email)
	return User(user), err
}
func (q sqliteQueries) userByID(ctx context.Context, id string) (User, error) {
	user, err := q.q.GetUserByID(ctx, id)
	return User(user), err
}
func (q sqliteQueries) lockUser(ctx context.Context, id string) (User, error) {
	user, err := q.q.LockUserByID(ctx, id)
	return User(user), err
}
func (q sqliteQueries) createUser(ctx context.Context, user User) error {
	return q.q.CreateUser(ctx, db.CreateUserParams{ID: user.ID, Email: user.Email, PasswordHash: user.PasswordHash, CreatedAt: user.CreatedAt})
}
func (q sqliteQueries) resetPassword(ctx context.Context, id, hash string) error {
	return q.q.ResetPassword(ctx, db.ResetPasswordParams{ID: id, PasswordHash: hash})
}
func (q sqliteQueries) disableUser(ctx context.Context, id string) error {
	return q.q.DisableUser(ctx, id)
}
func (q sqliteQueries) createSession(ctx context.Context, session Session) error {
	return q.q.CreateSession(ctx, db.CreateSessionParams{ID: session.ID, UserID: session.UserID, ExpiresAt: session.ExpiresAt, CreatedAt: session.CreatedAt})
}
func (q sqliteQueries) sessionByID(ctx context.Context, id string) (Session, error) {
	session, err := q.q.GetSession(ctx, id)
	return Session(session), err
}
func (q sqliteQueries) activeSession(ctx context.Context, id, userID string, now int64) (Session, error) {
	session, err := q.q.GetActiveSession(ctx, db.GetActiveSessionParams{ID: id, UserID: userID, Now: now})
	return Session(session), err
}
func (q sqliteQueries) revokeSession(ctx context.Context, id string, now int64) error {
	return q.q.RevokeSession(ctx, db.RevokeSessionParams{ID: id, RevokedAt: sql.NullInt64{Int64: now, Valid: true}})
}
func (q sqliteQueries) revokeUserSessions(ctx context.Context, userID string, now int64) error {
	return q.q.RevokeUserSessions(ctx, db.RevokeUserSessionsParams{UserID: userID, RevokedAt: sql.NullInt64{Int64: now, Valid: true}})
}
func (q sqliteQueries) createRefresh(ctx context.Context, refresh Refresh) error {
	return q.q.CreateRefresh(ctx, db.CreateRefreshParams{Hash: refresh.Hash, SessionID: refresh.SessionID, CreatedAt: refresh.CreatedAt})
}
func (q sqliteQueries) refreshByHash(ctx context.Context, hash string) (Refresh, error) {
	refresh, err := q.q.GetRefresh(ctx, hash)
	return Refresh(refresh), err
}
func (q sqliteQueries) consumeRefresh(ctx context.Context, hash string, now int64) (int64, error) {
	return q.q.ConsumeRefresh(ctx, db.ConsumeRefreshParams{Hash: hash, ConsumedAt: sql.NullInt64{Int64: now, Valid: true}})
}
func (q sqliteQueries) createActionItem(ctx context.Context, item ActionItem) error {
	return q.q.CreateActionItem(ctx, db.CreateActionItemParams(item))
}
func (q sqliteQueries) actionItem(ctx context.Context, userID, id string) (ActionItem, error) {
	item, err := q.q.GetActionItem(ctx, db.GetActionItemParams{ID: id, UserID: userID})
	return ActionItem(item), err
}
func (q sqliteQueries) lockActionItem(ctx context.Context, userID, id string) (ActionItem, error) {
	item, err := q.q.LockActionItem(ctx, db.LockActionItemParams{ID: id, UserID: userID})
	return ActionItem(item), err
}
func (q sqliteQueries) listActionItems(ctx context.Context, userID, status string, after ActionItemCursor, limit int) ([]ActionItem, error) {
	var rows []db.ActionItem
	var err error
	if status == "" {
		rows, err = q.q.ListActionItems(ctx, db.ListActionItemsParams{UserID: userID, BeforeCreatedAt: after.CreatedAt, TieCreatedAt: after.CreatedAt, BeforeID: after.ID, PageSize: int64(limit)})
	} else {
		rows, err = q.q.ListActionItemsByStatus(ctx, db.ListActionItemsByStatusParams{UserID: userID, Status: status, BeforeCreatedAt: after.CreatedAt, TieCreatedAt: after.CreatedAt, BeforeID: after.ID, PageSize: int64(limit)})
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
func (q sqliteQueries) updateActionItem(ctx context.Context, item ActionItem, currentVersion int64) (int64, error) {
	return q.q.UpdateActionItem(ctx, db.UpdateActionItemParams{Content: item.Content, Status: item.Status, Note: item.Note, DueAt: item.DueAt, Refs: item.Refs, Source: item.Source, SourceKey: item.SourceKey, Tracking: item.Tracking, AutoReason: item.AutoReason, AutoRef: item.AutoRef, AutoAt: item.AutoAt, DetectedAt: item.DetectedAt, TrackedAt: item.TrackedAt, NewVersion: item.Version, UpdatedAt: item.UpdatedAt, ID: item.ID, UserID: item.UserID, CurrentVersion: currentVersion})
}
func (q sqliteQueries) createAPIToken(ctx context.Context, token APIToken, hash string) error {
	return q.q.CreateAPIToken(ctx, db.CreateAPITokenParams{ID: token.ID, UserID: token.UserID, Name: token.Name, TokenHash: hash, CreatedAt: token.CreatedAt})
}
func (q sqliteQueries) countAPITokens(ctx context.Context, userID string) (int64, error) {
	return q.q.CountAPITokens(ctx, userID)
}
func (q sqliteQueries) listAPITokens(ctx context.Context, userID string) ([]APIToken, error) {
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
func (q sqliteQueries) apiTokenByHash(ctx context.Context, hash string) (APIToken, error) {
	row, err := q.q.GetAPITokenByHash(ctx, hash)
	return APIToken{ID: row.ID, UserID: row.UserID, LastUsedAt: row.LastUsedAt}, err
}
func (q sqliteQueries) deleteAPIToken(ctx context.Context, userID, id string) (int64, error) {
	return q.q.DeleteAPIToken(ctx, db.DeleteAPITokenParams{ID: id, UserID: userID})
}
func (q sqliteQueries) touchAPIToken(ctx context.Context, id string, now, before int64) error {
	return q.q.TouchAPIToken(ctx, db.TouchAPITokenParams{LastUsedAt: sql.NullInt64{Int64: now, Valid: true}, ID: id, TouchedBefore: sql.NullInt64{Int64: before, Valid: true}})
}
func (q sqliteQueries) deleteUserAPITokens(ctx context.Context, userID string) error {
	return q.q.DeleteUserAPITokens(ctx, userID)
}
func (q sqliteQueries) oauthClient(ctx context.Context, clientID string) (OAuthClient, error) {
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
func (q sqliteQueries) createOAuthClient(ctx context.Context, c OAuthClient, secretHash sql.NullString) error {
	uris, err := encodeRedirectURIs(c.RedirectURIs)
	if err != nil {
		return err
	}
	return q.q.CreateOAuthClient(ctx, db.CreateOAuthClientParams{ClientID: c.ClientID, Kind: c.Kind, Name: c.Name, RedirectUris: uris, AuthMethod: c.AuthMethod, SecretHash: secretHash, MetadataExpiresAt: c.MetadataExpiresAt, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt})
}
func (q sqliteQueries) upsertCIMDClient(ctx context.Context, c OAuthClient) error {
	uris, err := encodeRedirectURIs(c.RedirectURIs)
	if err != nil {
		return err
	}
	return q.q.UpsertCIMDClient(ctx, db.UpsertCIMDClientParams{ClientID: c.ClientID, Name: c.Name, RedirectUris: uris, MetadataExpiresAt: c.MetadataExpiresAt, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt})
}
func (q sqliteQueries) createOAuthGrant(ctx context.Context, g OAuthGrant) error {
	return q.q.CreateOAuthGrant(ctx, db.CreateOAuthGrantParams{ID: g.ID, UserID: g.UserID, ClientID: g.ClientID, ClientName: g.ClientName, Scope: g.Scope, Resource: g.Resource, RedirectUri: g.RedirectURI, CodeHash: g.CodeHash, CodeChallenge: g.CodeChallenge, CodeExpiresAt: g.CodeExpiresAt, CreatedAt: g.CreatedAt, ExpiresAt: g.ExpiresAt})
}
func (q sqliteQueries) oauthGrantByCode(ctx context.Context, codeHash string) (OAuthGrant, error) {
	row, err := q.q.GetOAuthGrantByCode(ctx, codeHash)
	return q.grant(row), err
}
func (q sqliteQueries) lockOAuthGrantByCode(ctx context.Context, codeHash, userID string) (OAuthGrant, error) {
	row, err := q.q.LockOAuthGrantByCode(ctx, db.LockOAuthGrantByCodeParams{CodeHash: codeHash, UserID: userID})
	return q.grant(row), err
}
func (q sqliteQueries) oauthGrant(ctx context.Context, userID, id string) (OAuthGrant, error) {
	row, err := q.q.GetOAuthGrant(ctx, db.GetOAuthGrantParams{ID: id, UserID: userID})
	return q.grant(row), err
}
func (q sqliteQueries) markOAuthCodeUsed(ctx context.Context, userID, id string, now int64) (int64, error) {
	return q.q.MarkOAuthCodeUsed(ctx, db.MarkOAuthCodeUsedParams{CodeUsedAt: sql.NullInt64{Int64: now, Valid: true}, ID: id, UserID: userID})
}
func (q sqliteQueries) extendOAuthGrant(ctx context.Context, userID, id string, expiresAt int64) error {
	return q.q.ExtendOAuthGrant(ctx, db.ExtendOAuthGrantParams{ExpiresAt: expiresAt, ID: id, UserID: userID})
}
func (q sqliteQueries) listOAuthGrants(ctx context.Context, userID string, now int64) ([]OAuthGrant, error) {
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
func (q sqliteQueries) deleteOAuthGrant(ctx context.Context, userID, id string) (int64, error) {
	return q.q.DeleteOAuthGrant(ctx, db.DeleteOAuthGrantParams{ID: id, UserID: userID})
}
func (q sqliteQueries) touchOAuthGrant(ctx context.Context, userID, id string, now, before int64) error {
	return q.q.TouchOAuthGrant(ctx, db.TouchOAuthGrantParams{LastUsedAt: sql.NullInt64{Int64: now, Valid: true}, ID: id, UserID: userID, TouchedBefore: sql.NullInt64{Int64: before, Valid: true}})
}
func (q sqliteQueries) deleteExpiredOAuthGrants(ctx context.Context, userID string, now int64) error {
	return q.q.DeleteExpiredOAuthGrants(ctx, db.DeleteExpiredOAuthGrantsParams{UserID: userID, Now: now})
}
func (q sqliteQueries) deleteUserOAuthGrants(ctx context.Context, userID string) error {
	return q.q.DeleteUserOAuthGrants(ctx, userID)
}
func (q sqliteQueries) createOAuthToken(ctx context.Context, t OAuthToken) error {
	return q.q.CreateOAuthToken(ctx, db.CreateOAuthTokenParams{TokenHash: t.Hash, Kind: t.Kind, GrantID: t.GrantID, UserID: t.UserID, CreatedAt: t.CreatedAt, ExpiresAt: t.ExpiresAt})
}
func (q sqliteQueries) oauthTokenByHash(ctx context.Context, hash string) (OAuthToken, error) {
	row, err := q.q.GetOAuthTokenByHash(ctx, hash)
	return q.token(row), err
}
func (q sqliteQueries) lockOAuthTokenByHash(ctx context.Context, hash, userID string) (OAuthToken, error) {
	row, err := q.q.LockOAuthTokenByHash(ctx, db.LockOAuthTokenByHashParams{TokenHash: hash, UserID: userID})
	return q.token(row), err
}
func (q sqliteQueries) deleteOAuthToken(ctx context.Context, userID, hash string) (int64, error) {
	return q.q.DeleteOAuthToken(ctx, db.DeleteOAuthTokenParams{TokenHash: hash, UserID: userID})
}
func (q sqliteQueries) deleteGrantOAuthTokens(ctx context.Context, userID, grantID string) error {
	return q.q.DeleteGrantOAuthTokens(ctx, db.DeleteGrantOAuthTokensParams{GrantID: grantID, UserID: userID})
}
func (q sqliteQueries) deleteExpiredOAuthTokens(ctx context.Context, userID string, now int64) error {
	return q.q.DeleteExpiredOAuthTokens(ctx, db.DeleteExpiredOAuthTokensParams{UserID: userID, Now: now})
}
func (q sqliteQueries) deleteUserOAuthTokens(ctx context.Context, userID string) error {
	return q.q.DeleteUserOAuthTokens(ctx, userID)
}
func (sqliteQueries) grant(row db.OauthGrant) OAuthGrant {
	return OAuthGrant{ID: row.ID, UserID: row.UserID, ClientID: row.ClientID, ClientName: row.ClientName, Scope: row.Scope, Resource: row.Resource, RedirectURI: row.RedirectUri, CodeHash: row.CodeHash, CodeChallenge: row.CodeChallenge, CodeExpiresAt: row.CodeExpiresAt, CodeUsedAt: row.CodeUsedAt, CreatedAt: row.CreatedAt, ExpiresAt: row.ExpiresAt, LastUsedAt: row.LastUsedAt}
}
func (sqliteQueries) token(row db.OauthToken) OAuthToken {
	return OAuthToken{Hash: row.TokenHash, Kind: row.Kind, GrantID: row.GrantID, UserID: row.UserID, CreatedAt: row.CreatedAt, ExpiresAt: row.ExpiresAt}
}
func (q sqliteQueries) insertSourcedActionItem(ctx context.Context, item ActionItem) (int64, error) {
	return q.q.InsertSourcedActionItem(ctx, db.InsertSourcedActionItemParams(item))
}
func (q sqliteQueries) lockActionItemBySourceKey(ctx context.Context, userID, sourceKey string) (ActionItem, error) {
	item, err := q.q.LockActionItemBySourceKey(ctx, db.LockActionItemBySourceKeyParams{UserID: userID, SourceKey: sql.NullString{String: sourceKey, Valid: true}})
	return ActionItem(item), err
}
func (q sqliteQueries) listAutoActionItems(ctx context.Context, userID, status string, after AutoActionItemCursor, limit int) ([]ActionItem, error) {
	var rows []db.ActionItem
	var err error
	at := sql.NullInt64{Int64: after.AutoAt, Valid: true}
	if status == "" {
		rows, err = q.q.ListAutoActionItems(ctx, db.ListAutoActionItemsParams{UserID: userID, BeforeAutoAt: at, TieAutoAt: at, BeforeID: after.ID, PageSize: int64(limit)})
	} else {
		rows, err = q.q.ListAutoActionItemsByStatus(ctx, db.ListAutoActionItemsByStatusParams{UserID: userID, Status: status, BeforeAutoAt: at, TieAutoAt: at, BeforeID: after.ID, PageSize: int64(limit)})
	}
	return q.actionItems(rows, err)
}
func (q sqliteQueries) listTrackedActionItems(ctx context.Context, userID, source string, limit int) ([]ActionItem, error) {
	return q.actionItems(q.q.ListTrackedActionItems(ctx, db.ListTrackedActionItemsParams{UserID: userID, Source: sql.NullString{String: source, Valid: true}, PageSize: int64(limit)}))
}
func (q sqliteQueries) listStaleActionItems(ctx context.Context, userID string, createdBefore int64, limit int) ([]ActionItem, error) {
	return q.actionItems(q.q.ListStaleActionItems(ctx, db.ListStaleActionItemsParams{UserID: userID, CreatedBefore: createdBefore, PageSize: int64(limit)}))
}
func (q sqliteQueries) countActionItemsByStatus(ctx context.Context, userID, status string) (int64, error) {
	return q.q.CountActionItemsByStatus(ctx, db.CountActionItemsByStatusParams{UserID: userID, Status: status})
}
func (q sqliteQueries) countAutoActionItemsSince(ctx context.Context, userID string, since int64) (int64, error) {
	return q.q.CountAutoActionItemsSince(ctx, db.CountAutoActionItemsSinceParams{UserID: userID, Since: sql.NullInt64{Int64: since, Valid: true}})
}
func (q sqliteQueries) touchActionItemTracked(ctx context.Context, userID, id string, version, now int64) (int64, error) {
	return q.q.TouchActionItemTracked(ctx, db.TouchActionItemTrackedParams{TrackedAt: sql.NullInt64{Int64: now, Valid: true}, ID: id, UserID: userID, Version: version})
}
func (q sqliteQueries) untrackSourceActionItems(ctx context.Context, userID, source string, now int64) error {
	return q.q.UntrackSourceActionItems(ctx, db.UntrackSourceActionItemsParams{UpdatedAt: now, UserID: userID, Source: sql.NullString{String: source, Valid: true}})
}
func (q sqliteQueries) githubConnection(ctx context.Context, userID string) (GitHubConnection, error) {
	row, err := q.q.GetGitHubConnection(ctx, userID)
	return GitHubConnection(row), err
}
func (q sqliteQueries) lockGitHubConnection(ctx context.Context, userID string) (GitHubConnection, error) {
	row, err := q.q.LockGitHubConnection(ctx, userID)
	return GitHubConnection(row), err
}
func (q sqliteQueries) createGitHubConnection(ctx context.Context, c GitHubConnection) error {
	return q.q.CreateGitHubConnection(ctx, db.CreateGitHubConnectionParams(c))
}
func (q sqliteQueries) replaceGitHubConnection(ctx context.Context, c GitHubConnection) error {
	return q.q.ReplaceGitHubConnection(ctx, db.ReplaceGitHubConnectionParams{Generation: c.Generation, TokenSealed: c.TokenSealed, TokenLast4: c.TokenLast4, Login: c.Login, Scopes: c.Scopes, ImmediateNotify: c.ImmediateNotify, Status: c.Status, LastError: c.LastError, FailureCount: c.FailureCount, LastModified: c.LastModified, InitialImportPending: c.InitialImportPending, NextPollAt: c.NextPollAt, LastSyncedAt: c.LastSyncedAt, LastReconciledAt: c.LastReconciledAt, LastTrackedAt: c.LastTrackedAt, UpdatedAt: c.UpdatedAt, UserID: c.UserID})
}
func (q sqliteQueries) updateGitHubSyncState(ctx context.Context, c GitHubConnection) (int64, error) {
	return q.q.UpdateGitHubSyncState(ctx, db.UpdateGitHubSyncStateParams{Status: c.Status, LastError: c.LastError, FailureCount: c.FailureCount, LastModified: c.LastModified, InitialImportPending: c.InitialImportPending, NextPollAt: c.NextPollAt, LastSyncedAt: c.LastSyncedAt, LastReconciledAt: c.LastReconciledAt, LastTrackedAt: c.LastTrackedAt, UpdatedAt: c.UpdatedAt, UserID: c.UserID, Generation: c.Generation})
}
func (q sqliteQueries) updateGitHubImmediateNotify(ctx context.Context, userID string, immediateNotify, now int64) (int64, error) {
	return q.q.UpdateGitHubImmediateNotify(ctx, db.UpdateGitHubImmediateNotifyParams{ImmediateNotify: immediateNotify, UpdatedAt: now, UserID: userID})
}
func (q sqliteQueries) deleteGitHubConnection(ctx context.Context, userID string) error {
	return q.q.DeleteGitHubConnection(ctx, userID)
}
func (q sqliteQueries) listDueGitHubConnections(ctx context.Context, now int64, limit int) ([]GitHubConnection, error) {
	rows, err := q.q.ListDueGitHubConnections(ctx, db.ListDueGitHubConnectionsParams{Now: now, PageSize: int64(limit)})
	if err != nil {
		return nil, err
	}
	out := make([]GitHubConnection, 0, len(rows))
	for _, row := range rows {
		out = append(out, GitHubConnection(row))
	}
	return out, nil
}
func (q sqliteQueries) slackConnection(ctx context.Context, userID string) (SlackConnection, error) {
	row, err := q.q.GetSlackConnection(ctx, userID)
	return fromSQLiteSlack(row), err
}
func (q sqliteQueries) lockSlackConnection(ctx context.Context, userID string) (SlackConnection, error) {
	row, err := q.q.LockSlackConnection(ctx, userID)
	return fromSQLiteSlack(row), err
}
func (q sqliteQueries) createSlackConnection(ctx context.Context, c SlackConnection) error {
	return q.q.CreateSlackConnection(ctx, db.CreateSlackConnectionParams{UserID: c.UserID, Generation: c.Generation, TokenSealed: c.TokenSealed, TokenLast4: c.TokenLast4, TeamID: c.TeamID, TeamName: c.TeamName, TeamUrl: c.TeamURL, SlackUserID: c.SlackUserID, SlackUserName: c.SlackUserName, Scopes: c.Scopes, ImmediateNotify: c.ImmediateNotify, DetectMentions: c.DetectMentions, DetectDms: c.DetectDMs, DetectGroupDms: c.DetectGroupDMs, Status: c.Status, LastError: c.LastError, FailureCount: c.FailureCount, ConnectedAt: c.ConnectedAt, NextPollAt: c.NextPollAt, LastSyncedAt: c.LastSyncedAt, LastTrackedAt: c.LastTrackedAt, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt})
}
func (q sqliteQueries) replaceSlackConnection(ctx context.Context, c SlackConnection) error {
	return q.q.ReplaceSlackConnection(ctx, db.ReplaceSlackConnectionParams{Generation: c.Generation, TokenSealed: c.TokenSealed, TokenLast4: c.TokenLast4, TeamID: c.TeamID, TeamName: c.TeamName, TeamUrl: c.TeamURL, SlackUserID: c.SlackUserID, SlackUserName: c.SlackUserName, Scopes: c.Scopes, ImmediateNotify: c.ImmediateNotify, DetectMentions: c.DetectMentions, DetectDms: c.DetectDMs, DetectGroupDms: c.DetectGroupDMs, Status: c.Status, LastError: c.LastError, FailureCount: c.FailureCount, ConnectedAt: c.ConnectedAt, NextPollAt: c.NextPollAt, LastSyncedAt: c.LastSyncedAt, LastTrackedAt: c.LastTrackedAt, UpdatedAt: c.UpdatedAt, UserID: c.UserID})
}
func (q sqliteQueries) updateSlackSyncState(ctx context.Context, c SlackConnection) (int64, error) {
	return q.q.UpdateSlackSyncState(ctx, db.UpdateSlackSyncStateParams{Status: c.Status, LastError: c.LastError, FailureCount: c.FailureCount, NextPollAt: c.NextPollAt, LastSyncedAt: c.LastSyncedAt, LastTrackedAt: c.LastTrackedAt, UpdatedAt: c.UpdatedAt, UserID: c.UserID, Generation: c.Generation})
}
func (q sqliteQueries) updateSlackSettings(ctx context.Context, c SlackConnection) (int64, error) {
	return q.q.UpdateSlackSettings(ctx, db.UpdateSlackSettingsParams{ImmediateNotify: c.ImmediateNotify, DetectMentions: c.DetectMentions, DetectDms: c.DetectDMs, DetectGroupDms: c.DetectGroupDMs, UpdatedAt: c.UpdatedAt, UserID: c.UserID})
}
func (q sqliteQueries) deleteSlackConnection(ctx context.Context, userID string) error {
	return q.q.DeleteSlackConnection(ctx, userID)
}
func (q sqliteQueries) listDueSlackConnections(ctx context.Context, now int64, limit int) ([]SlackConnection, error) {
	rows, err := q.q.ListDueSlackConnections(ctx, db.ListDueSlackConnectionsParams{Now: now, PageSize: int64(limit)})
	if err != nil {
		return nil, err
	}
	out := make([]SlackConnection, 0, len(rows))
	for _, row := range rows {
		out = append(out, fromSQLiteSlack(row))
	}
	return out, nil
}
func (q sqliteQueries) listSlackJudgedKeys(ctx context.Context, userID, prefix string, createdSince int64, limit int) ([]string, error) {
	rows, err := q.q.ListSlackJudgedKeys(ctx, db.ListSlackJudgedKeysParams{UserID: userID, Prefix: prefix, CreatedSince: createdSince, PageSize: int64(limit)})
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.String)
	}
	return out, nil
}
func (q sqliteQueries) listSlackOpenTasks(ctx context.Context, userID, prefix string, limit int) ([]string, error) {
	rows, err := q.q.ListSlackOpenTasks(ctx, db.ListSlackOpenTasksParams{UserID: userID, Prefix: prefix, PageSize: int64(limit)})
	if rows == nil {
		rows = []string{}
	}
	return rows, err
}
func (q sqliteQueries) aiSettings(ctx context.Context, userID string) (AISettings, error) {
	row, err := q.q.GetAISettings(ctx, userID)
	return fromSQLiteAISetting(row), err
}
func (q sqliteQueries) lockAISettings(ctx context.Context, userID string) (AISettings, error) {
	row, err := q.q.LockAISettings(ctx, userID)
	return fromSQLiteAISetting(row), err
}
func (q sqliteQueries) createAISettings(ctx context.Context, a AISettings) (int64, error) {
	return q.q.CreateAISettings(ctx, db.CreateAISettingsParams{UserID: a.UserID, Generation: a.Generation, Provider: a.Provider, Model: a.Model, ApiKeySealed: a.APIKeySealed, KeyLast4: a.KeyLast4, Status: a.Status, LastError: a.LastError, ActivatedAt: a.ActivatedAt, CreatedAt: a.CreatedAt, UpdatedAt: a.UpdatedAt})
}
func (q sqliteQueries) replaceAISettings(ctx context.Context, a AISettings, currentGeneration int64) (int64, error) {
	return q.q.ReplaceAISettings(ctx, db.ReplaceAISettingsParams{Generation: a.Generation, Provider: a.Provider, Model: a.Model, ApiKeySealed: a.APIKeySealed, KeyLast4: a.KeyLast4, Status: a.Status, LastError: a.LastError, ActivatedAt: a.ActivatedAt, UpdatedAt: a.UpdatedAt, UserID: a.UserID, CurrentGeneration: currentGeneration})
}
func (q sqliteQueries) updateAISettingsStatus(ctx context.Context, userID string, generation int64, status string, lastError sql.NullString, now int64) (int64, error) {
	return q.q.UpdateAISettingsStatus(ctx, db.UpdateAISettingsStatusParams{Status: status, LastError: lastError, UpdatedAt: now, UserID: userID, Generation: generation})
}
func (q sqliteQueries) deleteAISettings(ctx context.Context, userID string) error {
	return q.q.DeleteAISettings(ctx, userID)
}
func (q sqliteQueries) createPushSubscription(ctx context.Context, p PushSubscription) error {
	return q.q.CreatePushSubscription(ctx, db.CreatePushSubscriptionParams{ID: p.ID, UserID: p.UserID, Endpoint: p.Endpoint, P256dh: p.P256dh, Auth: p.Auth, Label: p.Label, CreatedAt: p.CreatedAt})
}
func (q sqliteQueries) lockPushSubscriptionByEndpoint(ctx context.Context, endpoint string) (PushSubscription, error) {
	row, err := q.q.LockPushSubscriptionByEndpoint(ctx, endpoint)
	return PushSubscription(row), err
}
func (q sqliteQueries) updatePushSubscriptionKeys(ctx context.Context, p PushSubscription) error {
	return q.q.UpdatePushSubscriptionKeys(ctx, db.UpdatePushSubscriptionKeysParams{P256dh: p.P256dh, Auth: p.Auth, Label: p.Label, ID: p.ID, UserID: p.UserID})
}
func (q sqliteQueries) countPushSubscriptions(ctx context.Context, userID string) (int64, error) {
	return q.q.CountPushSubscriptions(ctx, userID)
}
func (q sqliteQueries) listPushSubscriptions(ctx context.Context, userID string) ([]PushSubscription, error) {
	rows, err := q.q.ListPushSubscriptions(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]PushSubscription, 0, len(rows))
	for _, row := range rows {
		out = append(out, PushSubscription(row))
	}
	return out, nil
}
func (q sqliteQueries) listPushUserIDs(ctx context.Context) ([]string, error) {
	ids, err := q.q.ListPushUserIDs(ctx)
	if ids == nil {
		ids = []string{}
	}
	return ids, err
}
func (q sqliteQueries) touchPushSubscription(ctx context.Context, id string, now int64) error {
	return q.q.TouchPushSubscription(ctx, db.TouchPushSubscriptionParams{LastUsedAt: sql.NullInt64{Int64: now, Valid: true}, ID: id})
}
func (q sqliteQueries) deletePushSubscription(ctx context.Context, userID, id string) (int64, error) {
	return q.q.DeletePushSubscription(ctx, db.DeletePushSubscriptionParams{ID: id, UserID: userID})
}
func (q sqliteQueries) deletePushSubscriptionByID(ctx context.Context, id string) error {
	return q.q.DeletePushSubscriptionByID(ctx, id)
}
func (q sqliteQueries) deleteUserPushSubscriptions(ctx context.Context, userID string) error {
	return q.q.DeleteUserPushSubscriptions(ctx, userID)
}
func (q sqliteQueries) deleteAllPushSubscriptions(ctx context.Context) error {
	return q.q.DeleteAllPushSubscriptions(ctx)
}
func (q sqliteQueries) notificationSettings(ctx context.Context, userID string) (NotificationSettings, error) {
	row, err := q.q.GetNotificationSettings(ctx, userID)
	return NotificationSettings(row), err
}
func (q sqliteQueries) upsertNotificationSettings(ctx context.Context, n NotificationSettings) error {
	return q.q.UpsertNotificationSettings(ctx, db.UpsertNotificationSettingsParams(n))
}
func (q sqliteQueries) createNotificationSettingsIfMissing(ctx context.Context, n NotificationSettings) error {
	return q.q.CreateNotificationSettingsIfMissing(ctx, db.CreateNotificationSettingsIfMissingParams(n))
}
func (q sqliteQueries) enqueueNotification(ctx context.Context, n Notification) (int64, error) {
	return q.q.EnqueueNotification(ctx, db.EnqueueNotificationParams{ID: n.ID, UserID: n.UserID, Kind: n.Kind, ActionItemID: n.ActionItemID, DedupeKey: n.DedupeKey, Title: n.Title, Body: n.Body, Url: n.URL, ScheduledAt: n.ScheduledAt, SentAt: n.SentAt, Attempts: n.Attempts, LastError: n.LastError, CreatedAt: n.CreatedAt})
}
func (q sqliteQueries) countNotificationsByDedupeKey(ctx context.Context, userID, dedupeKey string) (int64, error) {
	return q.q.CountNotificationsByDedupeKey(ctx, db.CountNotificationsByDedupeKeyParams{UserID: userID, DedupeKey: dedupeKey})
}
func (q sqliteQueries) latestNotificationCreatedAt(ctx context.Context, userID, kind string) (int64, error) {
	return q.q.LatestNotificationCreatedAt(ctx, db.LatestNotificationCreatedAtParams{UserID: userID, Kind: kind})
}
func (q sqliteQueries) listDueNotifications(ctx context.Context, now int64, limit int) ([]Notification, error) {
	rows, err := q.q.ListDueNotifications(ctx, db.ListDueNotificationsParams{Now: now, PageSize: int64(limit)})
	if err != nil {
		return nil, err
	}
	out := make([]Notification, 0, len(rows))
	for _, row := range rows {
		out = append(out, Notification{ID: row.ID, UserID: row.UserID, Kind: row.Kind, ActionItemID: row.ActionItemID, DedupeKey: row.DedupeKey, Title: row.Title, Body: row.Body, URL: row.Url, ScheduledAt: row.ScheduledAt, SentAt: row.SentAt, Attempts: row.Attempts, LastError: row.LastError, CreatedAt: row.CreatedAt})
	}
	return out, nil
}
func (q sqliteQueries) updateNotificationResult(ctx context.Context, n Notification) error {
	return q.q.UpdateNotificationResult(ctx, db.UpdateNotificationResultParams{Attempts: n.Attempts, LastError: n.LastError, ScheduledAt: n.ScheduledAt, SentAt: n.SentAt, ID: n.ID})
}
func (q sqliteQueries) deleteItemPendingNotifications(ctx context.Context, userID, itemID string) error {
	return q.q.DeleteItemPendingNotifications(ctx, db.DeleteItemPendingNotificationsParams{UserID: userID, ActionItemID: sql.NullString{String: itemID, Valid: true}})
}
func (q sqliteQueries) deleteUserPendingNotifications(ctx context.Context, userID string) error {
	return q.q.DeleteUserPendingNotifications(ctx, userID)
}
func (q sqliteQueries) deleteUserPendingNotificationsBySource(ctx context.Context, userID, kind, source string) error {
	return q.q.DeleteUserPendingNotificationsByKindAndSource(ctx, db.DeleteUserPendingNotificationsByKindAndSourceParams{UserID: userID, Kind: kind, ItemUserID: userID, Source: sql.NullString{String: source, Valid: true}})
}
func (q sqliteQueries) deleteSentNotificationsBefore(ctx context.Context, before int64) error {
	return q.q.DeleteSentNotificationsBefore(ctx, sql.NullInt64{Int64: before, Valid: true})
}
func (q sqliteQueries) instanceSecret(ctx context.Context, name string) (InstanceSecret, error) {
	row, err := q.q.GetInstanceSecret(ctx, name)
	return InstanceSecret(row), err
}
func (q sqliteQueries) createInstanceSecret(ctx context.Context, sec InstanceSecret) (int64, error) {
	return q.q.CreateInstanceSecret(ctx, db.CreateInstanceSecretParams(sec))
}
func (q sqliteQueries) replaceInstanceSecret(ctx context.Context, name, oldValue, newValue string, now int64) (int64, error) {
	return q.q.ReplaceInstanceSecret(ctx, db.ReplaceInstanceSecretParams{NewValue: newValue, UpdatedAt: now, Name: name, OldValue: oldValue})
}

// actionItems converts the rows of a list query.
func (sqliteQueries) actionItems(rows []db.ActionItem, err error) ([]ActionItem, error) {
	if err != nil {
		return nil, err
	}
	items := make([]ActionItem, 0, len(rows))
	for _, row := range rows {
		items = append(items, ActionItem(row))
	}
	return items, nil
}

func fromSQLiteAISetting(row db.AiSetting) AISettings {
	return AISettings{UserID: row.UserID, Generation: row.Generation, Provider: row.Provider, Model: row.Model, APIKeySealed: row.ApiKeySealed, KeyLast4: row.KeyLast4, Status: row.Status, LastError: row.LastError, ActivatedAt: row.ActivatedAt, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}
}

func fromSQLiteSlack(row db.SlackConnection) SlackConnection {
	return SlackConnection{UserID: row.UserID, Generation: row.Generation, TokenSealed: row.TokenSealed, TokenLast4: row.TokenLast4, TeamID: row.TeamID, TeamName: row.TeamName, TeamURL: row.TeamUrl, SlackUserID: row.SlackUserID, SlackUserName: row.SlackUserName, Scopes: row.Scopes, ImmediateNotify: row.ImmediateNotify, DetectMentions: row.DetectMentions, DetectDMs: row.DetectDms, DetectGroupDMs: row.DetectGroupDms, Status: row.Status, LastError: row.LastError, FailureCount: row.FailureCount, ConnectedAt: row.ConnectedAt, NextPollAt: row.NextPollAt, LastSyncedAt: row.LastSyncedAt, LastTrackedAt: row.LastTrackedAt, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}
}
