package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// ErrConnectionNotFound is returned when the user has no connection of the
// integration (GitHub or Slack).
var ErrConnectionNotFound = errors.New("integration connection not found")

// ErrStaleConnection is returned by GitHubSync, SlackSync and ChatworkSync
// when the connection was replaced (a new generation), deleted, or its user
// disabled after the synchronization step started. The step's results must be
// discarded.
var ErrStaleConnection = errors.New("integration connection was changed or removed")

// SourceGitHub is action_items.source for items created by the GitHub
// integration.
const SourceGitHub = "github"

// GitHub connection status values.
const (
	GitHubActive = "active"
	GitHubError  = "error"
)

// GitHubConnection is a user's GitHub personal access token and the state of
// its synchronization. TokenSealed is the token sealed by secretbox; the
// plain token is never stored. Generation increases on every token
// registration so that a synchronization started with an older token cannot
// write its results.
type GitHubConnection struct {
	UserID               string
	Generation           int64
	TokenSealed          string
	TokenLast4           string
	Login                string
	Scopes               string // comma separated, sorted
	ImmediateNotify      int64
	Status               string
	LastError            sql.NullString
	FailureCount         int64
	LastModified         sql.NullString
	InitialImportPending int64
	NextPollAt           int64
	LastSyncedAt         sql.NullInt64
	LastReconciledAt     sql.NullInt64
	LastTrackedAt        sql.NullInt64
	CreatedAt            int64
	UpdatedAt            int64
}

// lockActiveUser locks the user row and checks that the user is enabled and
// that sessionID is still an active session of the user. It is the first
// step of every write made from a browser session; see CreateAPIToken.
func lockActiveUser(ctx context.Context, q queries, userID, sessionID string, now int64) error {
	u, e := q.lockUser(ctx, userID)
	if errors.Is(e, sql.ErrNoRows) {
		return ErrUnauthorized
	}
	if e != nil {
		return e
	}
	if u.Disabled != 0 {
		return ErrUnauthorized
	}
	if _, e = q.activeSession(ctx, sessionID, userID, now); e != nil {
		if errors.Is(e, sql.ErrNoRows) {
			return ErrUnauthorized
		}
		return e
	}
	return nil
}

// GitHubConnection returns the user's connection, or ErrConnectionNotFound.
func (s *Store) GitHubConnection(ctx context.Context, userID string) (GitHubConnection, error) {
	c, err := s.q.githubConnection(ctx, userID)
	if errors.Is(err, sql.ErrNoRows) {
		return GitHubConnection{}, ErrConnectionNotFound
	}
	return c, err
}

// SaveGitHubConnection registers a verified token for c.UserID. c carries
// TokenSealed, TokenLast4, Login and Scopes; every synchronization field is
// reset so that the next poll starts with the initial import. immediateNotify
// nil keeps the current setting (1 for a new connection).
//
// When the login differs from the previous connection's, the user's GitHub
// items stop being tracked and lose their source key: they belong to another
// account, and the new account's requests must be able to create new items.
func (s *Store) SaveGitHubConnection(ctx context.Context, c GitHubConnection, immediateNotify *bool, sessionID string, now int64) (GitHubConnection, error) {
	var saved GitHubConnection
	err := s.transaction(ctx, func(q queries) error {
		// Lock order: users, github_connections, action_items.
		if e := lockActiveUser(ctx, q, c.UserID, sessionID, now); e != nil {
			return e
		}
		existing, e := q.lockGitHubConnection(ctx, c.UserID)
		found := e == nil
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		next := GitHubConnection{UserID: c.UserID, Generation: 1, TokenSealed: c.TokenSealed, TokenLast4: c.TokenLast4, Login: c.Login, Scopes: c.Scopes, ImmediateNotify: 1, Status: GitHubActive, InitialImportPending: 1, NextPollAt: now, CreatedAt: now, UpdatedAt: now}
		if found {
			next.Generation, next.CreatedAt, next.ImmediateNotify = existing.Generation+1, existing.CreatedAt, existing.ImmediateNotify
			if !strings.EqualFold(existing.Login, c.Login) {
				if e = q.untrackSourceActionItems(ctx, c.UserID, SourceGitHub, now); e != nil {
					return e
				}
			}
		}
		if immediateNotify != nil {
			next.ImmediateNotify = boolInt(*immediateNotify)
		}
		if found {
			e = q.replaceGitHubConnection(ctx, next)
		} else {
			e = q.createGitHubConnection(ctx, next)
		}
		if e != nil {
			return e
		}
		saved = next
		return nil
	})
	if err != nil {
		return GitHubConnection{}, err
	}
	return saved, nil
}

// SetGitHubImmediateNotify changes whether detections are notified at once.
func (s *Store) SetGitHubImmediateNotify(ctx context.Context, userID string, immediateNotify bool, sessionID string, now int64) (GitHubConnection, error) {
	var saved GitHubConnection
	err := s.transaction(ctx, func(q queries) error {
		if e := lockActiveUser(ctx, q, userID, sessionID, now); e != nil {
			return e
		}
		n, e := q.updateGitHubImmediateNotify(ctx, userID, boolInt(immediateNotify), now)
		if e != nil {
			return e
		}
		if n == 0 {
			return ErrConnectionNotFound
		}
		saved, e = q.githubConnection(ctx, userID)
		return e
	})
	if err != nil {
		return GitHubConnection{}, err
	}
	return saved, nil
}

// DeleteGitHubConnection removes the user's connection. The user's GitHub
// items are kept, but stop being tracked and lose their source key, and
// detection notifications of GitHub items that were not sent yet are dropped.
// Deleting a missing connection succeeds.
func (s *Store) DeleteGitHubConnection(ctx context.Context, userID, sessionID string, now int64) error {
	return s.transaction(ctx, func(q queries) error {
		// Lock order: users, github_connections, action_items, notifications.
		if e := lockActiveUser(ctx, q, userID, sessionID, now); e != nil {
			return e
		}
		if e := q.deleteGitHubConnection(ctx, userID); e != nil {
			return e
		}
		if e := q.untrackSourceActionItems(ctx, userID, SourceGitHub, now); e != nil {
			return e
		}
		return q.deleteUserPendingNotificationsBySource(ctx, userID, NotificationDetected, SourceGitHub)
	})
}

// DueGitHubConnections returns up to limit active connections of enabled
// users whose next poll time has come, earliest first.
func (s *Store) DueGitHubConnections(ctx context.Context, now int64, limit int) ([]GitHubConnection, error) {
	return s.q.listDueGitHubConnections(ctx, now, limit)
}

// TrackedActionItems returns up to limit open or candidate items of source
// that are tracked, least recently checked first. It reads without locking; writes go
// through GitHubSync.
func (s *Store) TrackedActionItems(ctx context.Context, userID, source string, limit int) ([]ActionItem, error) {
	return s.q.listTrackedActionItems(ctx, userID, source, limit)
}

// GitHubTx is one write of a GitHub synchronization step. Conn is the
// connection as locked at the start of the transaction.
//
// The tables must be written in the lock order users, github_connections,
// action_items, notifications: GitHubSync has locked users and
// github_connections, so callers write items before notifications.
type GitHubTx struct {
	sourceTx
	Conn GitHubConnection
}

// GitHubSync runs fn in a transaction that first locks the user, checks
// that the user is enabled, and locks the connection of the given
// generation. If any check fails, fn is not called and ErrStaleConnection is
// returned. External requests must never be made inside fn.
func (s *Store) GitHubSync(ctx context.Context, userID string, generation int64, fn func(*GitHubTx) error) error {
	return s.transaction(ctx, func(q queries) error {
		if e := lockSyncUser(ctx, q, userID); e != nil {
			return e
		}
		c, e := q.lockGitHubConnection(ctx, userID)
		if errors.Is(e, sql.ErrNoRows) {
			return ErrStaleConnection
		}
		if e != nil {
			return e
		}
		if c.Generation != generation {
			return ErrStaleConnection
		}
		return fn(&GitHubTx{sourceTx: sourceTx{ctx: ctx, q: q, userID: userID}, Conn: c})
	})
}

// lockSyncUser locks the user row of a synchronization step and checks that
// the user exists and is enabled.
func lockSyncUser(ctx context.Context, q queries, userID string) error {
	u, e := q.lockUser(ctx, userID)
	if errors.Is(e, sql.ErrNoRows) {
		return ErrStaleConnection
	}
	if e != nil {
		return e
	}
	if u.Disabled != 0 {
		return ErrStaleConnection
	}
	return nil
}

// SaveState stores the synchronization fields of c (status, errors, poll
// times and the Last-Modified value). The token, login and notification
// setting are not written.
func (t *GitHubTx) SaveState(c GitHubConnection) error {
	c.UserID, c.Generation = t.Conn.UserID, t.Conn.Generation
	n, e := t.q.updateGitHubSyncState(t.ctx, c)
	if e != nil {
		return e
	}
	if n != 1 {
		return ErrStaleConnection
	}
	return nil
}

// sourceTx holds the item and notification writes shared by the
// synchronization steps of every integration (GitHubTx, SlackTx). It only
// touches the rows of userID.
type sourceTx struct {
	ctx    context.Context
	q      queries
	userID string
}

// LockItemBySourceKey locks the user's item with the source key.
func (t *sourceTx) LockItemBySourceKey(key string) (ActionItem, bool, error) {
	item, e := t.q.lockActionItemBySourceKey(t.ctx, t.userID, key)
	if errors.Is(e, sql.ErrNoRows) {
		return ActionItem{}, false, nil
	}
	return item, e == nil, e
}

// LockItem locks the user's item.
func (t *sourceTx) LockItem(id string) (ActionItem, bool, error) {
	item, e := t.q.lockActionItem(t.ctx, t.userID, id)
	if errors.Is(e, sql.ErrNoRows) {
		return ActionItem{}, false, nil
	}
	return item, e == nil, e
}

// CreateItem inserts item unless the user already has an item with its
// source key, and reports whether it was inserted. An item with ParentID is
// checked against the nesting rules (ErrInvalidParent) after locking the
// parent, and the parent is synchronized after the insert.
func (t *sourceTx) CreateItem(item ActionItem) (bool, error) {
	item.UserID = t.userID
	if item.ParentID.Valid {
		if _, e := lockParent(t.ctx, t.q, t.userID, item.ParentID.String, item.ID); e != nil {
			return false, e
		}
	}
	n, e := t.q.insertSourcedActionItem(t.ctx, withKind(item))
	if e != nil || n != 1 || !item.ParentID.Valid {
		return n == 1, e
	}
	return true, syncParent(t.ctx, t.q, t.userID, item.ParentID.String, item.UpdatedAt)
}

// UpdateItem stores next, which must have been read with LockItem or
// LockItemBySourceKey, with its version incremented and reports whether the
// stored version still matched. ParentID cannot be changed here (see
// SetParent); the item's parent is synchronized after the update.
func (t *sourceTx) UpdateItem(current, next ActionItem) (ActionItem, bool, error) {
	next.ID, next.UserID, next.CreatedAt, next.Version, next.ParentID = current.ID, current.UserID, current.CreatedAt, current.Version+1, current.ParentID
	n, e := t.q.updateActionItem(t.ctx, next, current.Version)
	if e != nil || n != 1 {
		return ActionItem{}, false, e
	}
	if next.ParentID.Valid {
		if e = syncParent(t.ctx, t.q, t.userID, next.ParentID.String, next.UpdatedAt); e != nil {
			return ActionItem{}, false, e
		}
	}
	return next, true, nil
}

// TouchItem records that item was checked, without changing its version.
// Nothing is written when the item changed since it was locked.
func (t *sourceTx) TouchItem(item ActionItem, now int64) error {
	_, e := t.q.touchActionItemTracked(t.ctx, t.userID, item.ID, item.Version, now)
	return e
}

// Enqueue adds n to the notifications to send unless the user already has
// one with its dedupe key.
func (t *sourceTx) Enqueue(n Notification) error {
	n.UserID = t.userID
	_, e := t.q.enqueueNotification(t.ctx, n)
	return e
}

// DropPendingNotifications deletes the notifications of the item that were
// not sent yet.
func (t *sourceTx) DropPendingNotifications(itemID string) error {
	return t.q.deleteItemPendingNotifications(t.ctx, t.userID, itemID)
}

func boolInt(v bool) int64 {
	if v {
		return 1
	}
	return 0
}
