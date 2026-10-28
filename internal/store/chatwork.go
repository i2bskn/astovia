package store

import (
	"context"
	"database/sql"
	"errors"
)

// SourceChatwork is action_items.source for items created by the Chatwork
// integration.
const SourceChatwork = "chatwork"

// Chatwork connection status values.
const (
	ChatworkActive = "active"
	ChatworkError  = "error"
)

// ChatworkConnection is a user's Chatwork API token and the state of its
// synchronization. TokenSealed is the token sealed by secretbox; the plain
// token is never stored. Generation increases on every token registration
// so that a synchronization started with an older token cannot write its
// results. ConnectedAt is when the current token was registered; CreatedAt
// stays at the first registration. AccountID is the Chatwork account_id in
// decimal.
type ChatworkConnection struct {
	UserID           string
	Generation       int64
	TokenSealed      string
	TokenLast4       string
	AccountID        string
	AccountName      string
	OrganizationName string
	ImmediateNotify  int64
	DetectMentions   int64
	DetectDMs        int64
	ImportTasks      int64
	Status           string
	LastError        sql.NullString
	FailureCount     int64
	ConnectedAt      int64
	NextPollAt       int64
	LastSyncedAt     sql.NullInt64
	LastTrackedAt    sql.NullInt64
	LastTasksAt      sql.NullInt64
	CreatedAt        int64
	UpdatedAt        int64
}

// ChatworkSettings are the user's choices of a Chatwork connection. A nil
// field keeps the current value (the default for a new connection).
type ChatworkSettings struct {
	ImmediateNotify *bool
	DetectMentions  *bool
	DetectDMs       *bool
	ImportTasks     *bool
}

// apply sets the fields of s that are not nil on c.
func (s ChatworkSettings) apply(c *ChatworkConnection) {
	for _, f := range []struct {
		v   *bool
		dst *int64
	}{{s.ImmediateNotify, &c.ImmediateNotify}, {s.DetectMentions, &c.DetectMentions}, {s.DetectDMs, &c.DetectDMs}, {s.ImportTasks, &c.ImportTasks}} {
		if f.v != nil {
			*f.dst = boolInt(*f.v)
		}
	}
}

// ChatworkConnection returns the user's connection, or ErrConnectionNotFound.
func (s *Store) ChatworkConnection(ctx context.Context, userID string) (ChatworkConnection, error) {
	c, err := s.q.chatworkConnection(ctx, userID)
	if errors.Is(err, sql.ErrNoRows) {
		return ChatworkConnection{}, ErrConnectionNotFound
	}
	return c, err
}

// SaveChatworkConnection registers a verified token for c.UserID. c carries
// TokenSealed, TokenLast4 and the account of the token; every
// synchronization field is reset and ConnectedAt is set to now, so that the
// next poll starts a new detection window. Settings left nil keep the
// current values (the defaults for a new connection).
//
// When the account differs from the previous connection's, the user's
// Chatwork items stop being tracked. Their source keys are kept: the keys
// contain the account ID, so they cannot collide with the new account's,
// and keeping them prevents duplicates when the first account comes back.
func (s *Store) SaveChatworkConnection(ctx context.Context, c ChatworkConnection, settings ChatworkSettings, sessionID string, now int64) (ChatworkConnection, error) {
	var saved ChatworkConnection
	err := s.transaction(ctx, func(q queries) error {
		// Lock order: users, chatwork_connections, action_items.
		if e := lockActiveUser(ctx, q, c.UserID, sessionID, now); e != nil {
			return e
		}
		existing, e := q.lockChatworkConnection(ctx, c.UserID)
		found := e == nil
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		next := ChatworkConnection{UserID: c.UserID, Generation: 1, TokenSealed: c.TokenSealed, TokenLast4: c.TokenLast4, AccountID: c.AccountID, AccountName: c.AccountName, OrganizationName: c.OrganizationName,
			ImmediateNotify: 1, DetectMentions: 1, DetectDMs: 1, ImportTasks: 1, Status: ChatworkActive, ConnectedAt: now, NextPollAt: now, CreatedAt: now, UpdatedAt: now}
		if found {
			next.Generation, next.CreatedAt = existing.Generation+1, existing.CreatedAt
			next.ImmediateNotify, next.DetectMentions, next.DetectDMs, next.ImportTasks = existing.ImmediateNotify, existing.DetectMentions, existing.DetectDMs, existing.ImportTasks
			if existing.AccountID != c.AccountID {
				if e = q.stopTrackingSourceActionItems(ctx, c.UserID, SourceChatwork, now); e != nil {
					return e
				}
			}
		}
		settings.apply(&next)
		if found {
			e = q.replaceChatworkConnection(ctx, next)
		} else {
			e = q.createChatworkConnection(ctx, next)
		}
		if e != nil {
			return e
		}
		saved = next
		return nil
	})
	if err != nil {
		return ChatworkConnection{}, err
	}
	return saved, nil
}

// UpdateChatworkSettings changes the settings that are not nil and keeps the
// others. It returns ErrConnectionNotFound without a connection.
func (s *Store) UpdateChatworkSettings(ctx context.Context, userID string, settings ChatworkSettings, sessionID string, now int64) (ChatworkConnection, error) {
	var saved ChatworkConnection
	err := s.transaction(ctx, func(q queries) error {
		if e := lockActiveUser(ctx, q, userID, sessionID, now); e != nil {
			return e
		}
		c, e := q.lockChatworkConnection(ctx, userID)
		if errors.Is(e, sql.ErrNoRows) {
			return ErrConnectionNotFound
		}
		if e != nil {
			return e
		}
		settings.apply(&c)
		c.UpdatedAt = now
		n, e := q.updateChatworkSettings(ctx, c)
		if e != nil {
			return e
		}
		if n == 0 {
			return ErrConnectionNotFound
		}
		saved = c
		return nil
	})
	if err != nil {
		return ChatworkConnection{}, err
	}
	return saved, nil
}

// DeleteChatworkConnection removes the user's connection. The user's
// Chatwork items are kept and stop being tracked, and detection
// notifications of Chatwork items that were not sent yet are dropped. The
// source keys are kept so that connecting the same account again does not
// create the items again. Deleting a missing connection succeeds.
func (s *Store) DeleteChatworkConnection(ctx context.Context, userID, sessionID string, now int64) error {
	return s.transaction(ctx, func(q queries) error {
		// Lock order: users, chatwork_connections, action_items, notifications.
		if e := lockActiveUser(ctx, q, userID, sessionID, now); e != nil {
			return e
		}
		if e := q.deleteChatworkConnection(ctx, userID); e != nil {
			return e
		}
		if e := q.stopTrackingSourceActionItems(ctx, userID, SourceChatwork, now); e != nil {
			return e
		}
		return q.deleteUserPendingNotificationsBySource(ctx, userID, NotificationDetected, SourceChatwork)
	})
}

// DueChatworkConnections returns up to limit active connections of enabled
// users whose next poll time has come, earliest first.
func (s *Store) DueChatworkConnections(ctx context.Context, now int64, limit int) ([]ChatworkConnection, error) {
	return s.q.listDueChatworkConnections(ctx, now, limit)
}

// ChatworkJudgedKeys returns the source keys of up to limit of the user's
// Chatwork items whose key starts with prefix (compared exactly,
// case-sensitively) and that were created at or after createdSince, newest
// first. It reads without locking.
func (s *Store) ChatworkJudgedKeys(ctx context.Context, userID, prefix string, createdSince int64, limit int) ([]string, error) {
	return s.q.listChatworkJudgedKeys(ctx, userID, prefix, createdSince, limit)
}

// ChatworkTaskItems returns up to limit of the user's open Chatwork items
// whose key starts with prefix (compared exactly, case-sensitively), oldest
// first. It reads without locking; writes go through ChatworkSync.
func (s *Store) ChatworkTaskItems(ctx context.Context, userID, prefix string, limit int) ([]ActionItem, error) {
	return s.q.listChatworkTaskItems(ctx, userID, prefix, limit)
}

// ChatworkTx is one write of a Chatwork synchronization step. Conn is the
// connection as locked at the start of the transaction.
//
// The tables must be written in the lock order users, chatwork_connections,
// action_items, notifications: ChatworkSync has locked users and
// chatwork_connections, so callers write items before notifications.
type ChatworkTx struct {
	sourceTx
	Conn ChatworkConnection
}

// ChatworkSync runs fn in a transaction that first locks the user, checks
// that the user is enabled, and locks the connection of the given
// generation. If any check fails, fn is not called and ErrStaleConnection
// is returned. External requests must never be made inside fn.
func (s *Store) ChatworkSync(ctx context.Context, userID string, generation int64, fn func(*ChatworkTx) error) error {
	return s.transaction(ctx, func(q queries) error {
		if e := lockSyncUser(ctx, q, userID); e != nil {
			return e
		}
		c, e := q.lockChatworkConnection(ctx, userID)
		if errors.Is(e, sql.ErrNoRows) {
			return ErrStaleConnection
		}
		if e != nil {
			return e
		}
		if c.Generation != generation {
			return ErrStaleConnection
		}
		return fn(&ChatworkTx{sourceTx: sourceTx{ctx: ctx, q: q, userID: userID}, Conn: c})
	})
}

// SaveState stores the synchronization fields of c (status, errors and poll
// times). The token, account, settings and ConnectedAt are not written.
func (t *ChatworkTx) SaveState(c ChatworkConnection) error {
	c.UserID, c.Generation = t.Conn.UserID, t.Conn.Generation
	n, e := t.q.updateChatworkSyncState(t.ctx, c)
	if e != nil {
		return e
	}
	if n != 1 {
		return ErrStaleConnection
	}
	return nil
}
