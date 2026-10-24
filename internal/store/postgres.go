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
	return q.q.UpdateActionItem(ctx, db.UpdateActionItemParams{Content: item.Content, Status: item.Status, Note: item.Note, DueAt: item.DueAt, Refs: item.Refs, Source: item.Source, SourceKey: item.SourceKey, Tracking: item.Tracking, AutoReason: item.AutoReason, AutoRef: item.AutoRef, AutoAt: item.AutoAt, DetectedAt: item.DetectedAt, TrackedAt: item.TrackedAt, NewVersion: item.Version, UpdatedAt: item.UpdatedAt, ID: item.ID, UserID: item.UserID, CurrentVersion: currentVersion})
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
func (q postgresQueries) deleteUserPendingNotificationsByKind(ctx context.Context, userID, kind string) error {
	return q.q.DeleteUserPendingNotificationsByKind(ctx, db.DeleteUserPendingNotificationsByKindParams{UserID: userID, Kind: kind})
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
