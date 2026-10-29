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
	return q.q.CreateUser(ctx, db.CreateUserParams{ID: user.ID, Email: user.Email, PasswordHash: user.PasswordHash, CreatedAt: user.CreatedAt, Role: user.Role})
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
func (q postgresQueries) listTopActionItems(ctx context.Context, userID, status string, after ActionItemCursor, limit int) ([]ActionItem, error) {
	if status == "" {
		return q.actionItems(q.q.ListTopActionItems(ctx, db.ListTopActionItemsParams{UserID: userID, BeforeCreatedAt: after.CreatedAt, TieCreatedAt: after.CreatedAt, BeforeID: after.ID, PageSize: int32(limit)}))
	}
	return q.actionItems(q.q.ListTopActionItemsByStatus(ctx, db.ListTopActionItemsByStatusParams{UserID: userID, Status: status, ParentStatus: status, BeforeCreatedAt: after.CreatedAt, TieCreatedAt: after.CreatedAt, BeforeID: after.ID, PageSize: int32(limit)}))
}
func (q postgresQueries) updateActionItem(ctx context.Context, item ActionItem, currentVersion int64) (int64, error) {
	return q.q.UpdateActionItem(ctx, db.UpdateActionItemParams{Content: item.Content, Status: item.Status, Note: item.Note, DueAt: item.DueAt, Refs: item.Refs, Source: item.Source, SourceKey: item.SourceKey, Tracking: item.Tracking, AutoReason: item.AutoReason, AutoRef: item.AutoRef, AutoAt: item.AutoAt, DetectedAt: item.DetectedAt, TrackedAt: item.TrackedAt, Kind: item.Kind, EventTitle: item.EventTitle, EventStartAt: item.EventStartAt, EventEndAt: item.EventEndAt, ParentID: item.ParentID, NewVersion: item.Version, UpdatedAt: item.UpdatedAt, ID: item.ID, UserID: item.UserID, CurrentVersion: currentVersion})
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
func (q postgresQueries) insertSourcedActionItem(ctx context.Context, item ActionItem) (int64, error) {
	return q.q.InsertSourcedActionItem(ctx, db.InsertSourcedActionItemParams(item))
}
func (q postgresQueries) lockActionItemBySourceKey(ctx context.Context, userID, sourceKey string) (ActionItem, error) {
	item, err := q.q.LockActionItemBySourceKey(ctx, db.LockActionItemBySourceKeyParams{UserID: userID, SourceKey: sql.NullString{String: sourceKey, Valid: true}})
	return ActionItem(item), err
}
func (q postgresQueries) listAutoActionItems(ctx context.Context, userID, status string, after AutoActionItemCursor, limit int) ([]ActionItem, error) {
	var rows []db.ActionItem
	var err error
	at := sql.NullInt64{Int64: after.AutoAt, Valid: true}
	if status == "" {
		rows, err = q.q.ListAutoActionItems(ctx, db.ListAutoActionItemsParams{UserID: userID, BeforeAutoAt: at, TieAutoAt: at, BeforeID: after.ID, PageSize: int32(limit)})
	} else {
		rows, err = q.q.ListAutoActionItemsByStatus(ctx, db.ListAutoActionItemsByStatusParams{UserID: userID, Status: status, BeforeAutoAt: at, TieAutoAt: at, BeforeID: after.ID, PageSize: int32(limit)})
	}
	return q.actionItems(rows, err)
}
func (q postgresQueries) listTrackedActionItems(ctx context.Context, userID, source string, limit int) ([]ActionItem, error) {
	return q.actionItems(q.q.ListTrackedActionItems(ctx, db.ListTrackedActionItemsParams{UserID: userID, Source: sql.NullString{String: source, Valid: true}, PageSize: int32(limit)}))
}
func (q postgresQueries) listStaleActionItems(ctx context.Context, userID string, createdBefore int64, limit int) ([]ActionItem, error) {
	return q.actionItems(q.q.ListStaleActionItems(ctx, db.ListStaleActionItemsParams{UserID: userID, CreatedBefore: createdBefore, PageSize: int32(limit)}))
}
func (q postgresQueries) countActionItemsByStatus(ctx context.Context, userID, status string) (int64, error) {
	return q.q.CountActionItemsByStatus(ctx, db.CountActionItemsByStatusParams{UserID: userID, Status: status})
}
func (q postgresQueries) countAutoActionItemsSince(ctx context.Context, userID string, since int64) (int64, error) {
	return q.q.CountAutoActionItemsSince(ctx, db.CountAutoActionItemsSinceParams{UserID: userID, Since: sql.NullInt64{Int64: since, Valid: true}})
}
func (q postgresQueries) touchActionItemTracked(ctx context.Context, userID, id string, version, now int64) (int64, error) {
	return q.q.TouchActionItemTracked(ctx, db.TouchActionItemTrackedParams{TrackedAt: sql.NullInt64{Int64: now, Valid: true}, ID: id, UserID: userID, Version: version})
}
func (q postgresQueries) untrackSourceActionItems(ctx context.Context, userID, source string, now int64) error {
	return q.q.UntrackSourceActionItems(ctx, db.UntrackSourceActionItemsParams{UpdatedAt: now, UserID: userID, Source: sql.NullString{String: source, Valid: true}})
}
func (q postgresQueries) stopTrackingSourceActionItems(ctx context.Context, userID, source string, now int64) error {
	return q.q.StopTrackingSourceActionItems(ctx, db.StopTrackingSourceActionItemsParams{UpdatedAt: now, UserID: userID, Source: sql.NullString{String: source, Valid: true}})
}
func (q postgresQueries) listCandidateSweepUserIDs(ctx context.Context, now int64) ([]string, error) {
	ids, err := q.q.ListCandidateSweepUserIDs(ctx, sql.NullInt64{Int64: now, Valid: true})
	if ids == nil {
		ids = []string{}
	}
	return ids, err
}
func (q postgresQueries) expireCandidates(ctx context.Context, userID string, updatedBefore, now int64) ([]sql.NullString, error) {
	return q.q.ExpireCandidates(ctx, db.ExpireCandidatesParams{Now: now, UserID: userID, UpdatedBefore: updatedBefore})
}
func (q postgresQueries) expireEvents(ctx context.Context, userID string, now int64) ([]sql.NullString, error) {
	return q.q.ExpireEvents(ctx, db.ExpireEventsParams{Now: now, UserID: userID})
}
func (q postgresQueries) listChildActionItems(ctx context.Context, userID, parentID string, limit int) ([]ActionItem, error) {
	return q.actionItems(q.q.ListChildActionItems(ctx, db.ListChildActionItemsParams{UserID: userID, ParentID: sql.NullString{String: parentID, Valid: true}, PageSize: int32(limit)}))
}
func (q postgresQueries) countChildActionItems(ctx context.Context, userID, parentID string) (int64, error) {
	return q.q.CountChildActionItems(ctx, db.CountChildActionItemsParams{UserID: userID, ParentID: sql.NullString{String: parentID, Valid: true}})
}
func (q postgresQueries) countChildrenByParents(ctx context.Context, userID, ids string) ([]ChildCount, error) {
	rows, err := q.q.CountChildrenByParents(ctx, db.CountChildrenByParentsParams{UserID: userID, Ids: ids})
	if err != nil {
		return nil, err
	}
	out := make([]ChildCount, 0, len(rows))
	for _, row := range rows {
		out = append(out, ChildCount{ParentID: row.ParentID.String, Children: row.Children, Total: row.Total, Done: row.Done, Remaining: row.Remaining, NextDueAt: nextDueAt(row.NextDueAt)})
	}
	return out, nil
}
func (q postgresQueries) actionItemSummaries(ctx context.Context, userID, ids string) ([]ItemSummary, error) {
	rows, err := q.q.GetActionItemsByIDs(ctx, db.GetActionItemsByIDsParams{UserID: userID, Ids: ids})
	if err != nil {
		return nil, err
	}
	out := make([]ItemSummary, 0, len(rows))
	for _, row := range rows {
		out = append(out, ItemSummary(row))
	}
	return out, nil
}
func (q postgresQueries) listConversationActionItems(ctx context.Context, userID, source, prefix string, createdSince, closedSince int64, limit int) ([]ActionItem, error) {
	return q.actionItems(q.q.ListConversationActionItems(ctx, db.ListConversationActionItemsParams{UserID: userID, Source: sql.NullString{String: source, Valid: true}, Prefix: prefix, CreatedSince: createdSince, ClosedSince: sql.NullInt64{Int64: closedSince, Valid: true}, PageSize: int32(limit)}))
}
func (q postgresQueries) actionItemBySourceKey(ctx context.Context, userID, sourceKey string) (ActionItem, error) {
	item, err := q.q.GetActionItemBySourceKey(ctx, db.GetActionItemBySourceKeyParams{UserID: userID, SourceKey: sql.NullString{String: sourceKey, Valid: true}})
	return ActionItem(item), err
}
func (q postgresQueries) githubConnection(ctx context.Context, userID string) (GitHubConnection, error) {
	row, err := q.q.GetGitHubConnection(ctx, userID)
	return GitHubConnection(row), err
}
func (q postgresQueries) lockGitHubConnection(ctx context.Context, userID string) (GitHubConnection, error) {
	row, err := q.q.LockGitHubConnection(ctx, userID)
	return GitHubConnection(row), err
}
func (q postgresQueries) createGitHubConnection(ctx context.Context, c GitHubConnection) error {
	return q.q.CreateGitHubConnection(ctx, db.CreateGitHubConnectionParams(c))
}
func (q postgresQueries) replaceGitHubConnection(ctx context.Context, c GitHubConnection) error {
	return q.q.ReplaceGitHubConnection(ctx, db.ReplaceGitHubConnectionParams{Generation: c.Generation, TokenSealed: c.TokenSealed, TokenLast4: c.TokenLast4, Login: c.Login, Scopes: c.Scopes, ImmediateNotify: c.ImmediateNotify, Status: c.Status, LastError: c.LastError, FailureCount: c.FailureCount, LastModified: c.LastModified, InitialImportPending: c.InitialImportPending, NextPollAt: c.NextPollAt, LastSyncedAt: c.LastSyncedAt, LastReconciledAt: c.LastReconciledAt, LastTrackedAt: c.LastTrackedAt, UpdatedAt: c.UpdatedAt, UserID: c.UserID})
}
func (q postgresQueries) updateGitHubSyncState(ctx context.Context, c GitHubConnection) (int64, error) {
	return q.q.UpdateGitHubSyncState(ctx, db.UpdateGitHubSyncStateParams{Status: c.Status, LastError: c.LastError, FailureCount: c.FailureCount, LastModified: c.LastModified, InitialImportPending: c.InitialImportPending, NextPollAt: c.NextPollAt, LastSyncedAt: c.LastSyncedAt, LastReconciledAt: c.LastReconciledAt, LastTrackedAt: c.LastTrackedAt, UpdatedAt: c.UpdatedAt, UserID: c.UserID, Generation: c.Generation})
}
func (q postgresQueries) updateGitHubImmediateNotify(ctx context.Context, userID string, immediateNotify, now int64) (int64, error) {
	return q.q.UpdateGitHubImmediateNotify(ctx, db.UpdateGitHubImmediateNotifyParams{ImmediateNotify: immediateNotify, UpdatedAt: now, UserID: userID})
}
func (q postgresQueries) deleteGitHubConnection(ctx context.Context, userID string) error {
	return q.q.DeleteGitHubConnection(ctx, userID)
}
func (q postgresQueries) listDueGitHubConnections(ctx context.Context, now int64, limit int) ([]GitHubConnection, error) {
	rows, err := q.q.ListDueGitHubConnections(ctx, db.ListDueGitHubConnectionsParams{Now: now, PageSize: int32(limit)})
	if err != nil {
		return nil, err
	}
	out := make([]GitHubConnection, 0, len(rows))
	for _, row := range rows {
		out = append(out, GitHubConnection(row))
	}
	return out, nil
}
func (q postgresQueries) slackConnection(ctx context.Context, userID string) (SlackConnection, error) {
	row, err := q.q.GetSlackConnection(ctx, userID)
	return fromPostgresSlack(row), err
}
func (q postgresQueries) lockSlackConnection(ctx context.Context, userID string) (SlackConnection, error) {
	row, err := q.q.LockSlackConnection(ctx, userID)
	return fromPostgresSlack(row), err
}
func (q postgresQueries) createSlackConnection(ctx context.Context, c SlackConnection) error {
	return q.q.CreateSlackConnection(ctx, db.CreateSlackConnectionParams{UserID: c.UserID, Generation: c.Generation, TokenSealed: c.TokenSealed, TokenLast4: c.TokenLast4, TeamID: c.TeamID, TeamName: c.TeamName, TeamUrl: c.TeamURL, SlackUserID: c.SlackUserID, SlackUserName: c.SlackUserName, Scopes: c.Scopes, ImmediateNotify: c.ImmediateNotify, DetectMentions: c.DetectMentions, DetectDms: c.DetectDMs, DetectGroupDms: c.DetectGroupDMs, Status: c.Status, LastError: c.LastError, FailureCount: c.FailureCount, ConnectedAt: c.ConnectedAt, NextPollAt: c.NextPollAt, LastSyncedAt: c.LastSyncedAt, LastTrackedAt: c.LastTrackedAt, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt})
}
func (q postgresQueries) replaceSlackConnection(ctx context.Context, c SlackConnection) error {
	return q.q.ReplaceSlackConnection(ctx, db.ReplaceSlackConnectionParams{Generation: c.Generation, TokenSealed: c.TokenSealed, TokenLast4: c.TokenLast4, TeamID: c.TeamID, TeamName: c.TeamName, TeamUrl: c.TeamURL, SlackUserID: c.SlackUserID, SlackUserName: c.SlackUserName, Scopes: c.Scopes, ImmediateNotify: c.ImmediateNotify, DetectMentions: c.DetectMentions, DetectDms: c.DetectDMs, DetectGroupDms: c.DetectGroupDMs, Status: c.Status, LastError: c.LastError, FailureCount: c.FailureCount, ConnectedAt: c.ConnectedAt, NextPollAt: c.NextPollAt, LastSyncedAt: c.LastSyncedAt, LastTrackedAt: c.LastTrackedAt, UpdatedAt: c.UpdatedAt, UserID: c.UserID})
}
func (q postgresQueries) updateSlackSyncState(ctx context.Context, c SlackConnection) (int64, error) {
	return q.q.UpdateSlackSyncState(ctx, db.UpdateSlackSyncStateParams{Status: c.Status, LastError: c.LastError, FailureCount: c.FailureCount, NextPollAt: c.NextPollAt, LastSyncedAt: c.LastSyncedAt, LastTrackedAt: c.LastTrackedAt, UpdatedAt: c.UpdatedAt, UserID: c.UserID, Generation: c.Generation})
}
func (q postgresQueries) updateSlackSettings(ctx context.Context, c SlackConnection) (int64, error) {
	return q.q.UpdateSlackSettings(ctx, db.UpdateSlackSettingsParams{ImmediateNotify: c.ImmediateNotify, DetectMentions: c.DetectMentions, DetectDms: c.DetectDMs, DetectGroupDms: c.DetectGroupDMs, UpdatedAt: c.UpdatedAt, UserID: c.UserID})
}
func (q postgresQueries) deleteSlackConnection(ctx context.Context, userID string) error {
	return q.q.DeleteSlackConnection(ctx, userID)
}
func (q postgresQueries) listDueSlackConnections(ctx context.Context, now int64, limit int) ([]SlackConnection, error) {
	rows, err := q.q.ListDueSlackConnections(ctx, db.ListDueSlackConnectionsParams{Now: now, PageSize: int32(limit)})
	if err != nil {
		return nil, err
	}
	out := make([]SlackConnection, 0, len(rows))
	for _, row := range rows {
		out = append(out, fromPostgresSlack(row))
	}
	return out, nil
}
func (q postgresQueries) listSlackJudgedKeys(ctx context.Context, userID, prefix string, createdSince int64, limit int) ([]string, error) {
	rows, err := q.q.ListSlackJudgedKeys(ctx, db.ListSlackJudgedKeysParams{UserID: userID, Prefix: prefix, CreatedSince: createdSince, PageSize: int32(limit)})
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.String)
	}
	return out, nil
}
func (q postgresQueries) chatworkConnection(ctx context.Context, userID string) (ChatworkConnection, error) {
	row, err := q.q.GetChatworkConnection(ctx, userID)
	return fromPostgresChatwork(row), err
}
func (q postgresQueries) lockChatworkConnection(ctx context.Context, userID string) (ChatworkConnection, error) {
	row, err := q.q.LockChatworkConnection(ctx, userID)
	return fromPostgresChatwork(row), err
}
func (q postgresQueries) createChatworkConnection(ctx context.Context, c ChatworkConnection) error {
	return q.q.CreateChatworkConnection(ctx, db.CreateChatworkConnectionParams{UserID: c.UserID, Generation: c.Generation, TokenSealed: c.TokenSealed, TokenLast4: c.TokenLast4, AccountID: c.AccountID, AccountName: c.AccountName, OrganizationName: c.OrganizationName, ImmediateNotify: c.ImmediateNotify, DetectMentions: c.DetectMentions, DetectDms: c.DetectDMs, ImportTasks: c.ImportTasks, Status: c.Status, LastError: c.LastError, FailureCount: c.FailureCount, ConnectedAt: c.ConnectedAt, NextPollAt: c.NextPollAt, LastSyncedAt: c.LastSyncedAt, LastTrackedAt: c.LastTrackedAt, LastTasksAt: c.LastTasksAt, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt})
}
func (q postgresQueries) replaceChatworkConnection(ctx context.Context, c ChatworkConnection) error {
	return q.q.ReplaceChatworkConnection(ctx, db.ReplaceChatworkConnectionParams{Generation: c.Generation, TokenSealed: c.TokenSealed, TokenLast4: c.TokenLast4, AccountID: c.AccountID, AccountName: c.AccountName, OrganizationName: c.OrganizationName, ImmediateNotify: c.ImmediateNotify, DetectMentions: c.DetectMentions, DetectDms: c.DetectDMs, ImportTasks: c.ImportTasks, Status: c.Status, LastError: c.LastError, FailureCount: c.FailureCount, ConnectedAt: c.ConnectedAt, NextPollAt: c.NextPollAt, LastSyncedAt: c.LastSyncedAt, LastTrackedAt: c.LastTrackedAt, LastTasksAt: c.LastTasksAt, UpdatedAt: c.UpdatedAt, UserID: c.UserID})
}
func (q postgresQueries) updateChatworkSyncState(ctx context.Context, c ChatworkConnection) (int64, error) {
	return q.q.UpdateChatworkSyncState(ctx, db.UpdateChatworkSyncStateParams{Status: c.Status, LastError: c.LastError, FailureCount: c.FailureCount, NextPollAt: c.NextPollAt, LastSyncedAt: c.LastSyncedAt, LastTrackedAt: c.LastTrackedAt, LastTasksAt: c.LastTasksAt, UpdatedAt: c.UpdatedAt, UserID: c.UserID, Generation: c.Generation})
}
func (q postgresQueries) updateChatworkSettings(ctx context.Context, c ChatworkConnection) (int64, error) {
	return q.q.UpdateChatworkSettings(ctx, db.UpdateChatworkSettingsParams{ImmediateNotify: c.ImmediateNotify, DetectMentions: c.DetectMentions, DetectDms: c.DetectDMs, ImportTasks: c.ImportTasks, UpdatedAt: c.UpdatedAt, UserID: c.UserID})
}
func (q postgresQueries) deleteChatworkConnection(ctx context.Context, userID string) error {
	return q.q.DeleteChatworkConnection(ctx, userID)
}
func (q postgresQueries) listDueChatworkConnections(ctx context.Context, now int64, limit int) ([]ChatworkConnection, error) {
	rows, err := q.q.ListDueChatworkConnections(ctx, db.ListDueChatworkConnectionsParams{Now: now, PageSize: int32(limit)})
	if err != nil {
		return nil, err
	}
	out := make([]ChatworkConnection, 0, len(rows))
	for _, row := range rows {
		out = append(out, fromPostgresChatwork(row))
	}
	return out, nil
}
func (q postgresQueries) listChatworkJudgedKeys(ctx context.Context, userID, prefix string, createdSince int64, limit int) ([]string, error) {
	rows, err := q.q.ListChatworkJudgedKeys(ctx, db.ListChatworkJudgedKeysParams{UserID: userID, Prefix: prefix, CreatedSince: createdSince, PageSize: int32(limit)})
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.String)
	}
	return out, nil
}
func (q postgresQueries) listChatworkTaskItems(ctx context.Context, userID, prefix string, limit int) ([]ActionItem, error) {
	return q.actionItems(q.q.ListChatworkTaskItems(ctx, db.ListChatworkTaskItemsParams{UserID: userID, Prefix: prefix, PageSize: int32(limit)}))
}
func (q postgresQueries) gmailAccounts(rows []db.GmailAccount, err error) ([]GmailAccount, error) {
	if err != nil {
		return nil, err
	}
	out := make([]GmailAccount, 0, len(rows))
	for _, row := range rows {
		out = append(out, GmailAccount(row))
	}
	return out, nil
}
func (q postgresQueries) listGmailAccounts(ctx context.Context, userID string) ([]GmailAccount, error) {
	return q.gmailAccounts(q.q.ListGmailAccounts(ctx, userID))
}
func (q postgresQueries) gmailAccount(ctx context.Context, userID, id string) (GmailAccount, error) {
	row, err := q.q.GetGmailAccount(ctx, db.GetGmailAccountParams{ID: id, UserID: userID})
	return GmailAccount(row), err
}
func (q postgresQueries) lockGmailAccount(ctx context.Context, userID, id string) (GmailAccount, error) {
	row, err := q.q.LockGmailAccount(ctx, db.LockGmailAccountParams{ID: id, UserID: userID})
	return GmailAccount(row), err
}
func (q postgresQueries) lockGmailAccountByEmail(ctx context.Context, email string) (GmailAccount, error) {
	row, err := q.q.LockGmailAccountByEmail(ctx, email)
	return GmailAccount(row), err
}
func (q postgresQueries) countGmailAccounts(ctx context.Context, userID string) (int64, error) {
	return q.q.CountGmailAccounts(ctx, userID)
}
func (q postgresQueries) createGmailAccount(ctx context.Context, a GmailAccount) (int64, error) {
	return q.q.CreateGmailAccount(ctx, db.CreateGmailAccountParams(a))
}
func (q postgresQueries) reconnectGmailAccount(ctx context.Context, a GmailAccount) (int64, error) {
	return q.q.ReconnectGmailAccount(ctx, db.ReconnectGmailAccountParams{Generation: a.Generation, TokenSealed: a.TokenSealed, Status: a.Status, LastError: a.LastError, FailureCount: a.FailureCount, ConnectedAt: a.ConnectedAt, NextPollAt: a.NextPollAt, UpdatedAt: a.UpdatedAt, ID: a.ID, UserID: a.UserID})
}
func (q postgresQueries) updateGmailTokenSealed(ctx context.Context, userID, id string, generation int64, tokenSealed string) (int64, error) {
	return q.q.UpdateGmailTokenSealed(ctx, db.UpdateGmailTokenSealedParams{TokenSealed: tokenSealed, ID: id, UserID: userID, Generation: generation})
}
func (q postgresQueries) updateGmailSyncState(ctx context.Context, a GmailAccount) (int64, error) {
	return q.q.UpdateGmailSyncState(ctx, db.UpdateGmailSyncStateParams{Status: a.Status, LastError: a.LastError, FailureCount: a.FailureCount, NextPollAt: a.NextPollAt, LastSyncedAt: a.LastSyncedAt, LastTrackedAt: a.LastTrackedAt, UpdatedAt: a.UpdatedAt, ID: a.ID, UserID: a.UserID, Generation: a.Generation})
}
func (q postgresQueries) updateGmailSettings(ctx context.Context, a GmailAccount) (int64, error) {
	return q.q.UpdateGmailSettings(ctx, db.UpdateGmailSettingsParams{ImmediateNotify: a.ImmediateNotify, UpdatedAt: a.UpdatedAt, ID: a.ID, UserID: a.UserID})
}
func (q postgresQueries) deleteGmailAccount(ctx context.Context, userID, id string) (int64, error) {
	return q.q.DeleteGmailAccount(ctx, db.DeleteGmailAccountParams{ID: id, UserID: userID})
}
func (q postgresQueries) deleteUserGmailAccounts(ctx context.Context, userID string) error {
	return q.q.DeleteUserGmailAccounts(ctx, userID)
}
func (q postgresQueries) listDueGmailAccounts(ctx context.Context, now int64, limit int) ([]GmailAccount, error) {
	return q.gmailAccounts(q.q.ListDueGmailAccounts(ctx, db.ListDueGmailAccountsParams{Now: now, PageSize: int32(limit)}))
}
func (q postgresQueries) listGmailJudgedKeys(ctx context.Context, userID, prefix string, createdSince int64, limit int) ([]string, error) {
	rows, err := q.q.ListGmailJudgedKeys(ctx, db.ListGmailJudgedKeysParams{UserID: userID, Prefix: prefix, CreatedSince: createdSince, PageSize: int32(limit)})
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.String)
	}
	return out, nil
}
func (q postgresQueries) createGoogleAuthState(ctx context.Context, st googleAuthState) error {
	return q.q.CreateGoogleAuthState(ctx, db.CreateGoogleAuthStateParams(st))
}
func (q postgresQueries) googleAuthState(ctx context.Context, userID, stateHash string) (googleAuthState, error) {
	row, err := q.q.GetGoogleAuthState(ctx, db.GetGoogleAuthStateParams{StateHash: stateHash, UserID: userID})
	return googleAuthState(row), err
}
func (q postgresQueries) listGoogleAuthStateHashes(ctx context.Context, userID string) ([]string, error) {
	hashes, err := q.q.ListGoogleAuthStateHashes(ctx, userID)
	if hashes == nil {
		hashes = []string{}
	}
	return hashes, err
}
func (q postgresQueries) deleteGoogleAuthState(ctx context.Context, userID, stateHash string) (int64, error) {
	return q.q.DeleteGoogleAuthState(ctx, db.DeleteGoogleAuthStateParams{StateHash: stateHash, UserID: userID})
}
func (q postgresQueries) deleteExpiredGoogleAuthStates(ctx context.Context, userID string, now int64) error {
	return q.q.DeleteExpiredGoogleAuthStates(ctx, db.DeleteExpiredGoogleAuthStatesParams{UserID: userID, Now: now})
}
func (q postgresQueries) deleteUserGoogleAuthStates(ctx context.Context, userID string) error {
	return q.q.DeleteUserGoogleAuthStates(ctx, userID)
}
func (q postgresQueries) stopTrackingSourcePrefixItems(ctx context.Context, userID, source, prefix string, now int64) error {
	return q.q.StopTrackingSourcePrefixItems(ctx, db.StopTrackingSourcePrefixItemsParams{UpdatedAt: now, UserID: userID, Source: sql.NullString{String: source, Valid: true}, Prefix: prefix})
}
func (q postgresQueries) listTrackedSourcePrefixItems(ctx context.Context, userID, source, prefix string, limit int) ([]ActionItem, error) {
	return q.actionItems(q.q.ListTrackedSourcePrefixItems(ctx, db.ListTrackedSourcePrefixItemsParams{UserID: userID, Source: sql.NullString{String: source, Valid: true}, Prefix: prefix, PageSize: int32(limit)}))
}
func (q postgresQueries) deleteUserPendingNotificationsBySourcePrefix(ctx context.Context, userID, kind, source, prefix string) error {
	return q.q.DeleteUserPendingNotificationsByKindAndSourcePrefix(ctx, db.DeleteUserPendingNotificationsByKindAndSourcePrefixParams{UserID: userID, Kind: kind, ItemUserID: userID, Source: sql.NullString{String: source, Valid: true}, Prefix: prefix})
}
func (q postgresQueries) aiSettings(ctx context.Context, userID string) (AISettings, error) {
	row, err := q.q.GetAISettings(ctx, userID)
	return fromPostgresAISetting(row), err
}
func (q postgresQueries) lockAISettings(ctx context.Context, userID string) (AISettings, error) {
	row, err := q.q.LockAISettings(ctx, userID)
	return fromPostgresAISetting(row), err
}
func (q postgresQueries) createAISettings(ctx context.Context, a AISettings) (int64, error) {
	return q.q.CreateAISettings(ctx, db.CreateAISettingsParams{UserID: a.UserID, Generation: a.Generation, Provider: a.Provider, Model: a.Model, ApiKeySealed: a.APIKeySealed, KeyLast4: a.KeyLast4, Status: a.Status, LastError: a.LastError, ActivatedAt: a.ActivatedAt, CreatedAt: a.CreatedAt, UpdatedAt: a.UpdatedAt})
}
func (q postgresQueries) replaceAISettings(ctx context.Context, a AISettings, currentGeneration int64) (int64, error) {
	return q.q.ReplaceAISettings(ctx, db.ReplaceAISettingsParams{Generation: a.Generation, Provider: a.Provider, Model: a.Model, ApiKeySealed: a.APIKeySealed, KeyLast4: a.KeyLast4, Status: a.Status, LastError: a.LastError, ActivatedAt: a.ActivatedAt, UpdatedAt: a.UpdatedAt, UserID: a.UserID, CurrentGeneration: currentGeneration})
}
func (q postgresQueries) updateAISettingsStatus(ctx context.Context, userID string, generation int64, status string, lastError sql.NullString, now int64) (int64, error) {
	return q.q.UpdateAISettingsStatus(ctx, db.UpdateAISettingsStatusParams{Status: status, LastError: lastError, UpdatedAt: now, UserID: userID, Generation: generation})
}
func (q postgresQueries) updateAISecret(ctx context.Context, userID string, generation int64, oldSealed, newSealed string) (int64, error) {
	return q.q.UpdateAISecret(ctx, db.UpdateAISecretParams{NewSealed: newSealed, UserID: userID, Generation: generation, OldSealed: oldSealed})
}
func (q postgresQueries) deleteAISettings(ctx context.Context, userID string) error {
	return q.q.DeleteAISettings(ctx, userID)
}
func (q postgresQueries) createPushSubscription(ctx context.Context, p PushSubscription) error {
	return q.q.CreatePushSubscription(ctx, db.CreatePushSubscriptionParams{ID: p.ID, UserID: p.UserID, Endpoint: p.Endpoint, P256dh: p.P256dh, Auth: p.Auth, Label: p.Label, CreatedAt: p.CreatedAt})
}
func (q postgresQueries) lockPushSubscriptionByEndpoint(ctx context.Context, endpoint string) (PushSubscription, error) {
	row, err := q.q.LockPushSubscriptionByEndpoint(ctx, endpoint)
	return PushSubscription(row), err
}
func (q postgresQueries) updatePushSubscriptionKeys(ctx context.Context, p PushSubscription) error {
	return q.q.UpdatePushSubscriptionKeys(ctx, db.UpdatePushSubscriptionKeysParams{P256dh: p.P256dh, Auth: p.Auth, Label: p.Label, ID: p.ID, UserID: p.UserID})
}
func (q postgresQueries) countPushSubscriptions(ctx context.Context, userID string) (int64, error) {
	return q.q.CountPushSubscriptions(ctx, userID)
}
func (q postgresQueries) listPushSubscriptions(ctx context.Context, userID string) ([]PushSubscription, error) {
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
func (q postgresQueries) listPushUserIDs(ctx context.Context) ([]string, error) {
	ids, err := q.q.ListPushUserIDs(ctx)
	if ids == nil {
		ids = []string{}
	}
	return ids, err
}
func (q postgresQueries) touchPushSubscription(ctx context.Context, id string, now int64) error {
	return q.q.TouchPushSubscription(ctx, db.TouchPushSubscriptionParams{LastUsedAt: sql.NullInt64{Int64: now, Valid: true}, ID: id})
}
func (q postgresQueries) deletePushSubscription(ctx context.Context, userID, id string) (int64, error) {
	return q.q.DeletePushSubscription(ctx, db.DeletePushSubscriptionParams{ID: id, UserID: userID})
}
func (q postgresQueries) deletePushSubscriptionByID(ctx context.Context, id string) error {
	return q.q.DeletePushSubscriptionByID(ctx, id)
}
func (q postgresQueries) deleteUserPushSubscriptions(ctx context.Context, userID string) error {
	return q.q.DeleteUserPushSubscriptions(ctx, userID)
}
func (q postgresQueries) deleteAllPushSubscriptions(ctx context.Context) error {
	return q.q.DeleteAllPushSubscriptions(ctx)
}
func (q postgresQueries) notificationSettings(ctx context.Context, userID string) (NotificationSettings, error) {
	row, err := q.q.GetNotificationSettings(ctx, userID)
	return NotificationSettings(row), err
}
func (q postgresQueries) upsertNotificationSettings(ctx context.Context, n NotificationSettings) error {
	return q.q.UpsertNotificationSettings(ctx, db.UpsertNotificationSettingsParams(n))
}
func (q postgresQueries) createNotificationSettingsIfMissing(ctx context.Context, n NotificationSettings) error {
	return q.q.CreateNotificationSettingsIfMissing(ctx, db.CreateNotificationSettingsIfMissingParams(n))
}
func (q postgresQueries) enqueueNotification(ctx context.Context, n Notification) (int64, error) {
	return q.q.EnqueueNotification(ctx, db.EnqueueNotificationParams{ID: n.ID, UserID: n.UserID, Kind: n.Kind, ActionItemID: n.ActionItemID, DedupeKey: n.DedupeKey, Title: n.Title, Body: n.Body, Url: n.URL, ScheduledAt: n.ScheduledAt, SentAt: n.SentAt, Attempts: n.Attempts, LastError: n.LastError, CreatedAt: n.CreatedAt})
}
func (q postgresQueries) countNotificationsByDedupeKey(ctx context.Context, userID, dedupeKey string) (int64, error) {
	return q.q.CountNotificationsByDedupeKey(ctx, db.CountNotificationsByDedupeKeyParams{UserID: userID, DedupeKey: dedupeKey})
}
func (q postgresQueries) latestNotificationCreatedAt(ctx context.Context, userID, kind string) (int64, error) {
	return q.q.LatestNotificationCreatedAt(ctx, db.LatestNotificationCreatedAtParams{UserID: userID, Kind: kind})
}
func (q postgresQueries) listDueNotifications(ctx context.Context, now int64, limit int) ([]Notification, error) {
	rows, err := q.q.ListDueNotifications(ctx, db.ListDueNotificationsParams{Now: now, PageSize: int32(limit)})
	if err != nil {
		return nil, err
	}
	out := make([]Notification, 0, len(rows))
	for _, row := range rows {
		out = append(out, Notification{ID: row.ID, UserID: row.UserID, Kind: row.Kind, ActionItemID: row.ActionItemID, DedupeKey: row.DedupeKey, Title: row.Title, Body: row.Body, URL: row.Url, ScheduledAt: row.ScheduledAt, SentAt: row.SentAt, Attempts: row.Attempts, LastError: row.LastError, CreatedAt: row.CreatedAt})
	}
	return out, nil
}
func (q postgresQueries) updateNotificationResult(ctx context.Context, n Notification) error {
	return q.q.UpdateNotificationResult(ctx, db.UpdateNotificationResultParams{Attempts: n.Attempts, LastError: n.LastError, ScheduledAt: n.ScheduledAt, SentAt: n.SentAt, ID: n.ID})
}
func (q postgresQueries) deleteItemPendingNotifications(ctx context.Context, userID, itemID string) error {
	return q.q.DeleteItemPendingNotifications(ctx, db.DeleteItemPendingNotificationsParams{UserID: userID, ActionItemID: sql.NullString{String: itemID, Valid: true}})
}
func (q postgresQueries) deleteUserPendingNotifications(ctx context.Context, userID string) error {
	return q.q.DeleteUserPendingNotifications(ctx, userID)
}
func (q postgresQueries) deleteUserPendingNotificationsBySource(ctx context.Context, userID, kind, source string) error {
	return q.q.DeleteUserPendingNotificationsByKindAndSource(ctx, db.DeleteUserPendingNotificationsByKindAndSourceParams{UserID: userID, Kind: kind, ItemUserID: userID, Source: sql.NullString{String: source, Valid: true}})
}
func (q postgresQueries) deleteSentNotificationsBefore(ctx context.Context, before int64) error {
	return q.q.DeleteSentNotificationsBefore(ctx, sql.NullInt64{Int64: before, Valid: true})
}
func (q postgresQueries) instanceSecret(ctx context.Context, name string) (InstanceSecret, error) {
	row, err := q.q.GetInstanceSecret(ctx, name)
	return InstanceSecret(row), err
}
func (q postgresQueries) createInstanceSecret(ctx context.Context, sec InstanceSecret) (int64, error) {
	return q.q.CreateInstanceSecret(ctx, db.CreateInstanceSecretParams(sec))
}
func (q postgresQueries) replaceInstanceSecret(ctx context.Context, name, oldValue, newValue string, now int64) (int64, error) {
	return q.q.ReplaceInstanceSecret(ctx, db.ReplaceInstanceSecretParams{NewValue: newValue, UpdatedAt: now, Name: name, OldValue: oldValue})
}

// actionItems converts the rows of a list query.
func (postgresQueries) actionItems(rows []db.ActionItem, err error) ([]ActionItem, error) {
	if err != nil {
		return nil, err
	}
	items := make([]ActionItem, 0, len(rows))
	for _, row := range rows {
		items = append(items, ActionItem(row))
	}
	return items, nil
}

func fromPostgresAISetting(row db.AiSetting) AISettings {
	return AISettings{UserID: row.UserID, Generation: row.Generation, Provider: row.Provider, Model: row.Model, APIKeySealed: row.ApiKeySealed, KeyLast4: row.KeyLast4, Status: row.Status, LastError: row.LastError, ActivatedAt: row.ActivatedAt, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}
}

func fromPostgresSlack(row db.SlackConnection) SlackConnection {
	return SlackConnection{UserID: row.UserID, Generation: row.Generation, TokenSealed: row.TokenSealed, TokenLast4: row.TokenLast4, TeamID: row.TeamID, TeamName: row.TeamName, TeamURL: row.TeamUrl, SlackUserID: row.SlackUserID, SlackUserName: row.SlackUserName, Scopes: row.Scopes, ImmediateNotify: row.ImmediateNotify, DetectMentions: row.DetectMentions, DetectDMs: row.DetectDms, DetectGroupDMs: row.DetectGroupDms, Status: row.Status, LastError: row.LastError, FailureCount: row.FailureCount, ConnectedAt: row.ConnectedAt, NextPollAt: row.NextPollAt, LastSyncedAt: row.LastSyncedAt, LastTrackedAt: row.LastTrackedAt, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}
}
func fromPostgresChatwork(row db.ChatworkConnection) ChatworkConnection {
	return ChatworkConnection{UserID: row.UserID, Generation: row.Generation, TokenSealed: row.TokenSealed, TokenLast4: row.TokenLast4, AccountID: row.AccountID, AccountName: row.AccountName, OrganizationName: row.OrganizationName, ImmediateNotify: row.ImmediateNotify, DetectMentions: row.DetectMentions, DetectDMs: row.DetectDms, ImportTasks: row.ImportTasks, Status: row.Status, LastError: row.LastError, FailureCount: row.FailureCount, ConnectedAt: row.ConnectedAt, NextPollAt: row.NextPollAt, LastSyncedAt: row.LastSyncedAt, LastTrackedAt: row.LastTrackedAt, LastTasksAt: row.LastTasksAt, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}
}

func (q postgresQueries) enableUser(ctx context.Context, id string) error {
	return q.q.EnableUser(ctx, id)
}
func (q postgresQueries) setUserRole(ctx context.Context, id, role string) error {
	return q.q.SetUserRole(ctx, db.SetUserRoleParams{Role: role, ID: id})
}
func (q postgresQueries) listUsers(ctx context.Context, limit int) ([]User, error) {
	return q.users(q.q.ListUsers(ctx, int32(limit)))
}
func (q postgresQueries) usersByIDs(ctx context.Context, ids string) ([]User, error) {
	return q.users(q.q.GetUsersByIDs(ctx, ids))
}
func (q postgresQueries) lockUsersForOwnerChange(ctx context.Context, operatorID, targetID string) ([]User, error) {
	return q.users(q.q.LockUsersForOwnerChange(ctx, db.LockUsersForOwnerChangeParams{OperatorID: operatorID, TargetID: targetID}))
}
func (q postgresQueries) createInvitation(ctx context.Context, inv Invitation) error {
	return q.q.CreateInvitation(ctx, db.CreateInvitationParams{ID: inv.ID, TokenHash: inv.TokenHash, Email: inv.Email, Role: inv.Role, InvitedBy: inv.InvitedBy, CreatedAt: inv.CreatedAt, ExpiresAt: inv.ExpiresAt})
}
func (q postgresQueries) invitationByTokenHash(ctx context.Context, hash string) (Invitation, error) {
	row, err := q.q.GetInvitationByTokenHash(ctx, hash)
	return Invitation(row), err
}
func (q postgresQueries) lockInvitationByTokenHash(ctx context.Context, hash string) (Invitation, error) {
	row, err := q.q.LockInvitationByTokenHash(ctx, hash)
	return Invitation(row), err
}
func (q postgresQueries) acceptInvitation(ctx context.Context, id, userID string, now int64) (int64, error) {
	return q.q.AcceptInvitation(ctx, db.AcceptInvitationParams{AcceptedAt: sql.NullInt64{Int64: now, Valid: true}, AcceptedUserID: sql.NullString{String: userID, Valid: true}, ID: id, Now: now})
}
func (q postgresQueries) revokeInvitation(ctx context.Context, id string, now int64) (int64, error) {
	return q.q.RevokeInvitation(ctx, db.RevokeInvitationParams{RevokedAt: sql.NullInt64{Int64: now, Valid: true}, ID: id, Now: now})
}
func (q postgresQueries) revokeEmailInvitations(ctx context.Context, email string, now int64) error {
	return q.q.RevokeEmailInvitations(ctx, db.RevokeEmailInvitationsParams{RevokedAt: sql.NullInt64{Int64: now, Valid: true}, Email: email, Now: now})
}
func (q postgresQueries) revokeIssuedInvitations(ctx context.Context, invitedBy string, now int64) error {
	return q.q.RevokeIssuedInvitations(ctx, db.RevokeIssuedInvitationsParams{RevokedAt: sql.NullInt64{Int64: now, Valid: true}, InvitedBy: invitedBy, Now: now})
}
func (q postgresQueries) listActiveInvitations(ctx context.Context, now int64, limit int) ([]Invitation, error) {
	rows, err := q.q.ListActiveInvitations(ctx, db.ListActiveInvitationsParams{Now: now, PageSize: int32(limit)})
	if err != nil {
		return nil, err
	}
	out := make([]Invitation, 0, len(rows))
	for _, row := range rows {
		out = append(out, Invitation(row))
	}
	return out, nil
}
func (q postgresQueries) createPasswordReset(ctx context.Context, r PasswordReset) error {
	return q.q.CreatePasswordReset(ctx, db.CreatePasswordResetParams{ID: r.ID, TokenHash: r.TokenHash, UserID: r.UserID, CreatedBy: r.CreatedBy, CreatedAt: r.CreatedAt, ExpiresAt: r.ExpiresAt})
}
func (q postgresQueries) passwordResetByTokenHash(ctx context.Context, hash string) (PasswordReset, error) {
	row, err := q.q.GetPasswordResetByTokenHash(ctx, hash)
	return PasswordReset(row), err
}
func (q postgresQueries) lockActivePasswordResets(ctx context.Context, userID string, now int64) ([]PasswordReset, error) {
	rows, err := q.q.LockActivePasswordResets(ctx, db.LockActivePasswordResetsParams{UserID: userID, CreatedBy: userID, Now: now})
	if err != nil {
		return nil, err
	}
	out := make([]PasswordReset, 0, len(rows))
	for _, row := range rows {
		out = append(out, PasswordReset(row))
	}
	return out, nil
}
func (q postgresQueries) revokePasswordReset(ctx context.Context, id string, now int64) error {
	return q.q.RevokePasswordReset(ctx, db.RevokePasswordResetParams{RevokedAt: sql.NullInt64{Int64: now, Valid: true}, ID: id})
}
func (q postgresQueries) usePasswordReset(ctx context.Context, id string, now int64) (int64, error) {
	return q.q.UsePasswordReset(ctx, db.UsePasswordResetParams{UsedAt: sql.NullInt64{Int64: now, Valid: true}, ID: id})
}
func (q postgresQueries) latestUsedPasswordReset(ctx context.Context, userID string, since int64) (PasswordReset, error) {
	row, err := q.q.LatestUsedPasswordReset(ctx, db.LatestUsedPasswordResetParams{UserID: userID, Since: sql.NullInt64{Int64: since, Valid: true}})
	return PasswordReset(row), err
}

// users converts the rows of a user list query.
func (postgresQueries) users(rows []db.User, err error) ([]User, error) {
	if err != nil {
		return nil, err
	}
	out := make([]User, 0, len(rows))
	for _, row := range rows {
		out = append(out, User(row))
	}
	return out, nil
}
