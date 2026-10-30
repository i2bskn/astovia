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
	// Role is RoleOwner or RoleMember. CreateUser requires one of them.
	Role string
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
	// Source is the integration that created the item ("github"), or null
	// for items created by the user, the REST API or MCP. SourceKey
	// identifies the source object and is unique per user.
	Source    sql.NullString
	SourceKey sql.NullString
	// Tracking is 1 while the integration follows the source object to
	// complete or reopen the item.
	Tracking int64
	// AutoReason, AutoRef and AutoAt describe an automatic completion.
	AutoReason sql.NullString
	AutoRef    sql.NullString
	AutoAt     sql.NullInt64
	// DetectedAt is when the item was last detected or reopened, and
	// TrackedAt when its source object was last checked.
	DetectedAt sql.NullInt64
	TrackedAt  sql.NullInt64
	// Kind is KindTask or KindEvent. An event item asks the user to add a
	// meeting to the calendar; EventTitle, EventStartAt and EventEndAt
	// describe the meeting (EventEndAt is null when no end was given).
	Kind         string
	EventTitle   sql.NullString
	EventStartAt sql.NullInt64
	EventEndAt   sql.NullInt64
	// ParentID is the item this one is a subtask of, or null. Items nest at
	// most two levels: a parent has no parent, and an item with subtasks has
	// no parent. The store keeps these rules; see lockParent.
	ParentID sql.NullString
	// Importance is ImportanceHigh, ImportanceNormal or ImportanceLow. A
	// high item is notified on detection even when immediate notifications
	// are off.
	Importance string
}

// Kinds of action items. Items are created as KindTask unless the caller
// sets KindEvent.
const (
	KindTask  = "task"
	KindEvent = "event"
)

// Automatic reasons of the candidate sweep. An item created for an event
// that has already started is dismissed with ReasonEventPassed as well.
const (
	ReasonCandidateExpired = "candidate.expired"
	ReasonEventPassed      = "event.passed"
)

// Importance levels of action items. Items are created as ImportanceNormal
// unless the caller sets another level.
const (
	ImportanceHigh   = "high"
	ImportanceNormal = "normal"
	ImportanceLow    = "low"
)

// ValidImportance reports whether v is one of the importance levels.
func ValidImportance(v string) bool {
	return v == ImportanceHigh || v == ImportanceNormal || v == ImportanceLow
}

// withDefaults returns item with an empty Kind set to KindTask and an empty
// Importance set to ImportanceNormal. The INSERT statements list every
// column, so the column defaults do not apply.
func withDefaults(item ActionItem) ActionItem {
	if item.Kind == "" {
		item.Kind = KindTask
	}
	if item.Importance == "" {
		item.Importance = ImportanceNormal
	}
	return item
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

// AutoActionItemCursor is the position after which ListAutoActionItems
// continues, in auto_at DESC, id DESC order.
type AutoActionItemCursor struct {
	AutoAt int64
	ID     string
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
	listTopActionItems(ctx context.Context, userID, status string, after ActionItemCursor, limit int) ([]ActionItem, error)
	updateActionItem(ctx context.Context, item ActionItem, currentVersion int64) (int64, error)
	insertSourcedActionItem(ctx context.Context, item ActionItem) (int64, error)
	lockActionItemBySourceKey(ctx context.Context, userID, sourceKey string) (ActionItem, error)
	listAutoActionItems(ctx context.Context, userID, status string, after AutoActionItemCursor, limit int) ([]ActionItem, error)
	listTrackedActionItems(ctx context.Context, userID, source string, limit int) ([]ActionItem, error)
	listStaleActionItems(ctx context.Context, userID string, createdBefore int64, limit int) ([]ActionItem, error)
	listDueActionItems(ctx context.Context, userID string, dueBefore int64, limit int) ([]ActionItem, error)
	countActionItemsByStatus(ctx context.Context, userID, status string) (int64, error)
	countAutoActionItemsSince(ctx context.Context, userID string, since int64) (int64, error)
	touchActionItemTracked(ctx context.Context, userID, id string, version, now int64) (int64, error)
	untrackSourceActionItems(ctx context.Context, userID, source string, now int64) error
	stopTrackingSourceActionItems(ctx context.Context, userID, source string, now int64) error
	listCandidateSweepUserIDs(ctx context.Context, now int64) ([]string, error)
	expireCandidates(ctx context.Context, userID string, updatedBefore, now int64) ([]sql.NullString, error)
	expireEvents(ctx context.Context, userID string, now int64) ([]sql.NullString, error)
	listChildActionItems(ctx context.Context, userID, parentID string, limit int) ([]ActionItem, error)
	countChildActionItems(ctx context.Context, userID, parentID string) (int64, error)
	countChildrenByParents(ctx context.Context, userID, ids string) ([]ChildCount, error)
	actionItemSummaries(ctx context.Context, userID, ids string) ([]ItemSummary, error)
	listConversationActionItems(ctx context.Context, userID, source, prefix string, createdSince, closedSince int64, limit int) ([]ActionItem, error)
	actionItemBySourceKey(ctx context.Context, userID, sourceKey string) (ActionItem, error)
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
	githubConnection(ctx context.Context, userID string) (GitHubConnection, error)
	lockGitHubConnection(ctx context.Context, userID string) (GitHubConnection, error)
	createGitHubConnection(ctx context.Context, c GitHubConnection) error
	replaceGitHubConnection(ctx context.Context, c GitHubConnection) error
	updateGitHubSyncState(ctx context.Context, c GitHubConnection) (int64, error)
	updateGitHubImmediateNotify(ctx context.Context, userID string, immediateNotify, now int64) (int64, error)
	deleteGitHubConnection(ctx context.Context, userID string) error
	listDueGitHubConnections(ctx context.Context, now int64, limit int) ([]GitHubConnection, error)
	slackConnection(ctx context.Context, userID string) (SlackConnection, error)
	lockSlackConnection(ctx context.Context, userID string) (SlackConnection, error)
	createSlackConnection(ctx context.Context, c SlackConnection) error
	replaceSlackConnection(ctx context.Context, c SlackConnection) error
	updateSlackSyncState(ctx context.Context, c SlackConnection) (int64, error)
	updateSlackSettings(ctx context.Context, c SlackConnection) (int64, error)
	deleteSlackConnection(ctx context.Context, userID string) error
	listDueSlackConnections(ctx context.Context, now int64, limit int) ([]SlackConnection, error)
	listSlackJudgedKeys(ctx context.Context, userID, prefix string, createdSince int64, limit int) ([]string, error)
	chatworkConnection(ctx context.Context, userID string) (ChatworkConnection, error)
	lockChatworkConnection(ctx context.Context, userID string) (ChatworkConnection, error)
	createChatworkConnection(ctx context.Context, c ChatworkConnection) error
	replaceChatworkConnection(ctx context.Context, c ChatworkConnection) error
	updateChatworkSyncState(ctx context.Context, c ChatworkConnection) (int64, error)
	updateChatworkSettings(ctx context.Context, c ChatworkConnection) (int64, error)
	deleteChatworkConnection(ctx context.Context, userID string) error
	listDueChatworkConnections(ctx context.Context, now int64, limit int) ([]ChatworkConnection, error)
	listChatworkJudgedKeys(ctx context.Context, userID, prefix string, createdSince int64, limit int) ([]string, error)
	listChatworkTaskItems(ctx context.Context, userID, prefix string, limit int) ([]ActionItem, error)
	listGmailAccounts(ctx context.Context, userID string) ([]GmailAccount, error)
	gmailAccount(ctx context.Context, userID, id string) (GmailAccount, error)
	lockGmailAccount(ctx context.Context, userID, id string) (GmailAccount, error)
	lockGmailAccountByEmail(ctx context.Context, email string) (GmailAccount, error)
	countGmailAccounts(ctx context.Context, userID string) (int64, error)
	createGmailAccount(ctx context.Context, a GmailAccount) (int64, error)
	reconnectGmailAccount(ctx context.Context, a GmailAccount) (int64, error)
	updateGmailTokenSealed(ctx context.Context, userID, id string, generation int64, tokenSealed string) (int64, error)
	updateGmailSyncState(ctx context.Context, a GmailAccount) (int64, error)
	updateGmailSettings(ctx context.Context, a GmailAccount) (int64, error)
	deleteGmailAccount(ctx context.Context, userID, id string) (int64, error)
	deleteUserGmailAccounts(ctx context.Context, userID string) error
	listDueGmailAccounts(ctx context.Context, now int64, limit int) ([]GmailAccount, error)
	listGmailJudgedKeys(ctx context.Context, userID, prefix string, createdSince int64, limit int) ([]string, error)
	createGoogleAuthState(ctx context.Context, st googleAuthState) error
	googleAuthState(ctx context.Context, userID, stateHash string) (googleAuthState, error)
	listGoogleAuthStateHashes(ctx context.Context, userID string) ([]string, error)
	deleteGoogleAuthState(ctx context.Context, userID, stateHash string) (int64, error)
	deleteExpiredGoogleAuthStates(ctx context.Context, userID string, now int64) error
	deleteUserGoogleAuthStates(ctx context.Context, userID string) error
	stopTrackingSourcePrefixItems(ctx context.Context, userID, source, prefix string, now int64) error
	listTrackedSourcePrefixItems(ctx context.Context, userID, source, prefix string, limit int) ([]ActionItem, error)
	deleteUserPendingNotificationsBySourcePrefix(ctx context.Context, userID, kind, source, prefix string) error
	aiSettings(ctx context.Context, userID string) (AISettings, error)
	lockAISettings(ctx context.Context, userID string) (AISettings, error)
	createAISettings(ctx context.Context, a AISettings) (int64, error)
	replaceAISettings(ctx context.Context, a AISettings, currentGeneration int64) (int64, error)
	updateAISettingsStatus(ctx context.Context, userID string, generation int64, status string, lastError sql.NullString, now int64) (int64, error)
	updateAISecret(ctx context.Context, userID string, generation int64, oldSealed, newSealed string) (int64, error)
	deleteAISettings(ctx context.Context, userID string) error
	createPushSubscription(ctx context.Context, p PushSubscription) error
	lockPushSubscriptionByEndpoint(ctx context.Context, endpoint string) (PushSubscription, error)
	updatePushSubscriptionKeys(ctx context.Context, p PushSubscription) error
	countPushSubscriptions(ctx context.Context, userID string) (int64, error)
	listPushSubscriptions(ctx context.Context, userID string) ([]PushSubscription, error)
	listPushUserIDs(ctx context.Context) ([]string, error)
	touchPushSubscription(ctx context.Context, id string, now int64) error
	deletePushSubscription(ctx context.Context, userID, id string) (int64, error)
	deletePushSubscriptionByID(ctx context.Context, id string) error
	deleteUserPushSubscriptions(ctx context.Context, userID string) error
	deleteAllPushSubscriptions(ctx context.Context) error
	notificationSettings(ctx context.Context, userID string) (NotificationSettings, error)
	upsertNotificationSettings(ctx context.Context, n NotificationSettings) error
	createNotificationSettingsIfMissing(ctx context.Context, n NotificationSettings) error
	enqueueNotification(ctx context.Context, n Notification) (int64, error)
	countNotificationsByDedupeKey(ctx context.Context, userID, dedupeKey string) (int64, error)
	latestNotificationCreatedAt(ctx context.Context, userID, kind string) (int64, error)
	listDueNotifications(ctx context.Context, now int64, limit int) ([]Notification, error)
	updateNotificationResult(ctx context.Context, n Notification) error
	deleteItemPendingNotifications(ctx context.Context, userID, itemID string) error
	deleteUserPendingNotifications(ctx context.Context, userID string) error
	deleteUserPendingNotificationsBySource(ctx context.Context, userID, kind, source string) error
	deleteSentNotificationsBefore(ctx context.Context, before int64) error
	enableUser(ctx context.Context, id string) error
	setUserRole(ctx context.Context, id, role string) error
	listUsers(ctx context.Context, limit int) ([]User, error)
	usersByIDs(ctx context.Context, ids string) ([]User, error)
	lockUsersForOwnerChange(ctx context.Context, operatorID, targetID string) ([]User, error)
	createInvitation(ctx context.Context, inv Invitation) error
	invitationByTokenHash(ctx context.Context, hash string) (Invitation, error)
	lockInvitationByTokenHash(ctx context.Context, hash string) (Invitation, error)
	acceptInvitation(ctx context.Context, id, userID string, now int64) (int64, error)
	revokeInvitation(ctx context.Context, id string, now int64) (int64, error)
	revokeEmailInvitations(ctx context.Context, email string, now int64) error
	revokeIssuedInvitations(ctx context.Context, invitedBy string, now int64) error
	listActiveInvitations(ctx context.Context, now int64, limit int) ([]Invitation, error)
	createPasswordReset(ctx context.Context, r PasswordReset) error
	passwordResetByTokenHash(ctx context.Context, hash string) (PasswordReset, error)
	lockActivePasswordResets(ctx context.Context, userID string, now int64) ([]PasswordReset, error)
	revokePasswordReset(ctx context.Context, id string, now int64) error
	usePasswordReset(ctx context.Context, id string, now int64) (int64, error)
	latestUsedPasswordReset(ctx context.Context, userID string, since int64) (PasswordReset, error)
	instanceSecret(ctx context.Context, name string) (InstanceSecret, error)
	createInstanceSecret(ctx context.Context, sec InstanceSecret) (int64, error)
	replaceInstanceSecret(ctx context.Context, name, oldValue, newValue string, now int64) (int64, error)
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
	for _, q := range []string{"SELECT id,email,password_hash,disabled,created_at,role FROM users LIMIT 0", "SELECT id,user_id,expires_at,revoked_at,created_at FROM sessions LIMIT 0", "SELECT hash,session_id,consumed_at,created_at FROM refresh_tokens LIMIT 0", "SELECT id,user_id,content,status,note,due_at,refs,version,created_at,updated_at,source,source_key,tracking,auto_reason,auto_ref,auto_at,detected_at,tracked_at,kind,event_title,event_start_at,event_end_at,parent_id,importance FROM action_items LIMIT 0", "SELECT id,user_id,name,token_hash,created_at,last_used_at FROM api_tokens LIMIT 0", "SELECT client_id,kind,name,redirect_uris,auth_method,secret_hash,metadata_expires_at,created_at,updated_at FROM oauth_clients LIMIT 0", "SELECT id,user_id,client_id,client_name,scope,resource,redirect_uri,code_hash,code_challenge,code_expires_at,code_used_at,created_at,expires_at,last_used_at FROM oauth_grants LIMIT 0", "SELECT token_hash,kind,grant_id,user_id,created_at,expires_at FROM oauth_tokens LIMIT 0", "SELECT user_id,generation,token_sealed,token_last4,login,scopes,immediate_notify,status,last_error,failure_count,last_modified,initial_import_pending,next_poll_at,last_synced_at,last_reconciled_at,last_tracked_at,created_at,updated_at FROM github_connections LIMIT 0", "SELECT user_id,generation,token_sealed,token_last4,team_id,team_name,team_url,slack_user_id,slack_user_name,scopes,immediate_notify,detect_mentions,detect_dms,detect_group_dms,status,last_error,failure_count,connected_at,next_poll_at,last_synced_at,last_tracked_at,created_at,updated_at FROM slack_connections LIMIT 0", "SELECT user_id,generation,token_sealed,token_last4,account_id,account_name,organization_name,immediate_notify,detect_mentions,detect_dms,import_tasks,status,last_error,failure_count,connected_at,next_poll_at,last_synced_at,last_tracked_at,last_tasks_at,created_at,updated_at FROM chatwork_connections LIMIT 0", "SELECT id,user_id,email,generation,token_sealed,immediate_notify,status,last_error,failure_count,connected_at,next_poll_at,last_synced_at,last_tracked_at,created_at,updated_at FROM gmail_accounts LIMIT 0", "SELECT state_hash,user_id,verifier_sealed,expires_at,created_at FROM google_auth_states LIMIT 0", "SELECT user_id,generation,provider,model,api_key_sealed,key_last4,status,last_error,activated_at,created_at,updated_at FROM ai_settings LIMIT 0", "SELECT id,user_id,endpoint,p256dh,auth,label,created_at,last_used_at FROM web_push_subscriptions LIMIT 0", "SELECT user_id,timezone,digest_hour,stale_days,updated_at,candidate_expire_days FROM notification_settings LIMIT 0", "SELECT id,user_id,kind,action_item_id,dedupe_key,title,body,url,scheduled_at,sent_at,attempts,last_error,created_at FROM notifications LIMIT 0", "SELECT name,value,created_at,updated_at FROM instance_secrets LIMIT 0", "SELECT id,token_hash,email,role,invited_by,created_at,expires_at,accepted_at,accepted_user_id,revoked_at FROM invitations LIMIT 0", "SELECT id,token_hash,user_id,created_by,created_at,expires_at,used_at,revoked_at FROM password_resets LIMIT 0"} {
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

// CreateUser stores a new enabled user. u.Role must be RoleOwner or
// RoleMember; the column default is not relied on.
func (s *Store) CreateUser(ctx context.Context, u User) error {
	if !ValidRole(u.Role) {
		return ErrInvalidRole
	}
	err := s.transaction(ctx, func(q queries) error { return q.createUser(ctx, u) })
	if err != nil {
		existing, e := s.q.userByEmail(ctx, u.Email)
		if e == nil && existing.ID != "" {
			return ErrConflict
		}
	}
	return err
}

// ChangeUser resets the password of (hash) or disables the user with email,
// for the operator's CLI. It does not apply the last-owner rule of the user
// management API: the CLI is the operator's last resort.
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
		return revokeAccount(ctx, q, u.ID, hash, disable, now)
	})
}

// revokeAccount resets the password of userID to passwordHash, or disables
// the user, and removes what a stolen session could have left behind. The
// caller holds the lock of the user row (lockUser or
// lockUsersForOwnerChange). It is shared by the CLI (ChangeUser), the
// disabling from the user management API and the completion of a password
// reset link.
func revokeAccount(ctx context.Context, q queries, userID, passwordHash string, disable bool, now int64) error {
	var e error
	if disable {
		e = q.disableUser(ctx, userID)
	} else {
		e = q.resetPassword(ctx, userID, passwordHash)
	}
	if e != nil {
		return e
	}
	if e = q.revokeUserSessions(ctx, userID, now); e != nil {
		return e
	}
	// API tokens have no expiry, so a token issued from a stolen access
	// token must not survive the password reset that recovers the account.
	// OAuth connections are granted from a session as well and go with it.
	// Tokens are deleted before grants because they reference them.
	if e = q.deleteUserAPITokens(ctx, userID); e != nil {
		return e
	}
	if e = q.deleteUserOAuthTokens(ctx, userID); e != nil {
		return e
	}
	if e = q.deleteUserOAuthGrants(ctx, userID); e != nil {
		return e
	}
	// Integrations and notifications follow, in the lock order of their
	// tables (github_connections, slack_connections, chatwork_connections,
	// gmail_accounts, google_auth_states, ai_settings,
	// web_push_subscriptions, notifications). A push subscription
	// registered during a takeover would keep receiving item titles, so it
	// goes with a password reset as well. The GitHub, Slack and Chatwork
	// connections and the Google accounts are removed only when the user is
	// disabled; the Google grants of the removed accounts are not revoked,
	// since Google is never called outside a synchronization. The Google
	// authorizations in progress go with a password reset too: one started
	// during a takeover must not be completed afterwards. The AI settings go
	// with a password reset too: a key registered during a takeover would
	// keep sending conversation text to the attacker's organization.
	if disable {
		if e = q.deleteGitHubConnection(ctx, userID); e != nil {
			return e
		}
		if e = q.deleteSlackConnection(ctx, userID); e != nil {
			return e
		}
		if e = q.deleteChatworkConnection(ctx, userID); e != nil {
			return e
		}
		if e = q.deleteUserGmailAccounts(ctx, userID); e != nil {
			return e
		}
	}
	if e = q.deleteUserGoogleAuthStates(ctx, userID); e != nil {
		return e
	}
	if e = q.deleteAISettings(ctx, userID); e != nil {
		return e
	}
	if e = q.deleteUserPushSubscriptions(ctx, userID); e != nil {
		return e
	}
	if e = q.deleteUserPendingNotifications(ctx, userID); e != nil {
		return e
	}
	// Links are revoked last: the reset links sent to the user, and the
	// reset links and invitations the user issued. A link issued during a
	// takeover must not outlive the recovery of the account.
	return revokeLinks(ctx, q, userID, true, now)
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

// CreateActionItem stores a new item. An item with ParentID is created in a
// transaction that first locks the parent and checks the nesting rules
// (ErrInvalidParent), and synchronizes the parent after the insert.
func (s *Store) CreateActionItem(ctx context.Context, item ActionItem) error {
	item = withDefaults(item)
	if !item.ParentID.Valid {
		return s.q.createActionItem(ctx, item)
	}
	return s.transaction(ctx, func(q queries) error {
		if _, e := lockParent(ctx, q, item.UserID, item.ParentID.String, item.ID); e != nil {
			return e
		}
		if e := q.createActionItem(ctx, item); e != nil {
			return e
		}
		return syncParent(ctx, q, item.UserID, item.ParentID.String, item.UpdatedAt)
	})
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

// ListTopActionItems is ListActionItems for the task list: it returns only
// the items without a parent, and, for a status, also the subtasks whose
// parent has another status (so that the parent's list does not show them).
func (s *Store) ListTopActionItems(ctx context.Context, userID, status string, after *ActionItemCursor, limit int) ([]ActionItem, error) {
	c := ActionItemCursor{CreatedAt: math.MaxInt64}
	if after != nil {
		c = *after
	}
	return s.q.listTopActionItems(ctx, userID, status, c, limit)
}

// UpdateActionItem locks the user's item, lets fn modify it and stores the
// result with the version incremented. ID, UserID and CreatedAt cannot be
// changed by fn. An error from fn is returned as is and nothing is stored.
//
// The transaction locks the target row first. When fn changes ParentID, the
// old and new parents are then locked in ID order and the nesting rules are
// checked (ErrInvalidParent): the new parent has no parent and the item has
// no subtasks. When the parent or the status changed, the old and new
// parents are synchronized (completed or reopened); the item itself is not,
// so that a parent reopened by hand stays open.
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
		moved := next.ParentID != item.ParentID
		if moved {
			if e = lockParents(ctx, q, userID, item.ParentID.String, next.ParentID.String); e != nil {
				return e
			}
			if next.ParentID.Valid {
				if _, e = lockParent(ctx, q, userID, next.ParentID.String, item.ID); e != nil {
					return e
				}
				children, e := q.countChildActionItems(ctx, userID, item.ID)
				if e != nil {
					return e
				}
				if children > 0 {
					return ErrInvalidParent
				}
			}
		}
		n, e := q.updateActionItem(ctx, next, item.Version)
		if e != nil {
			return e
		}
		if n != 1 {
			return ErrVersionConflict
		}
		if moved || next.Status != item.Status {
			if e = syncParents(ctx, q, userID, next.UpdatedAt, item.ParentID.String, next.ParentID.String); e != nil {
				return e
			}
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
