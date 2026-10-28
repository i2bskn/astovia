package store

import (
	"context"
	"database/sql"
	"errors"
)

// SourceSlack is action_items.source for items created by the Slack
// integration.
const SourceSlack = "slack"

// Slack connection status values.
const (
	SlackActive = "active"
	SlackError  = "error"
)

// SlackConnection is a user's Slack user token (one workspace) and the state
// of its synchronization. TokenSealed is the token sealed by secretbox; the
// plain token is never stored. Generation increases on every token
// registration so that a synchronization started with an older token cannot
// write its results. ConnectedAt is when the current token was registered;
// CreatedAt stays at the first registration.
type SlackConnection struct {
	UserID          string
	Generation      int64
	TokenSealed     string
	TokenLast4      string
	TeamID          string
	TeamName        string
	TeamURL         string
	SlackUserID     string
	SlackUserName   string
	Scopes          string // comma separated, sorted
	ImmediateNotify int64
	DetectMentions  int64
	DetectDMs       int64
	DetectGroupDMs  int64
	Status          string
	LastError       sql.NullString
	FailureCount    int64
	ConnectedAt     int64
	NextPollAt      int64
	LastSyncedAt    sql.NullInt64
	LastTrackedAt   sql.NullInt64
	CreatedAt       int64
	UpdatedAt       int64
}

// SlackSettings are the user's choices of a Slack connection. A nil field
// keeps the current value (the default for a new connection).
type SlackSettings struct {
	ImmediateNotify *bool
	DetectMentions  *bool
	DetectDMs       *bool
	DetectGroupDMs  *bool
}

// apply sets the fields of s that are not nil on c.
func (s SlackSettings) apply(c *SlackConnection) {
	for _, f := range []struct {
		v   *bool
		dst *int64
	}{{s.ImmediateNotify, &c.ImmediateNotify}, {s.DetectMentions, &c.DetectMentions}, {s.DetectDMs, &c.DetectDMs}, {s.DetectGroupDMs, &c.DetectGroupDMs}} {
		if f.v != nil {
			*f.dst = boolInt(*f.v)
		}
	}
}

// SlackConnection returns the user's connection, or ErrConnectionNotFound.
func (s *Store) SlackConnection(ctx context.Context, userID string) (SlackConnection, error) {
	c, err := s.q.slackConnection(ctx, userID)
	if errors.Is(err, sql.ErrNoRows) {
		return SlackConnection{}, ErrConnectionNotFound
	}
	return c, err
}

// SaveSlackConnection registers a verified token for c.UserID. c carries
// TokenSealed, TokenLast4, the team and user of the token and Scopes; every
// synchronization field is reset and ConnectedAt is set to now, so that the
// next poll starts a new detection window. Settings left nil keep the
// current values (the defaults for a new connection).
//
// When the workspace or the Slack user differs from the previous
// connection's, the user's Slack items stop being tracked and lose their
// source key: they belong to another account.
func (s *Store) SaveSlackConnection(ctx context.Context, c SlackConnection, settings SlackSettings, sessionID string, now int64) (SlackConnection, error) {
	var saved SlackConnection
	err := s.transaction(ctx, func(q queries) error {
		// Lock order: users, slack_connections, action_items.
		if e := lockActiveUser(ctx, q, c.UserID, sessionID, now); e != nil {
			return e
		}
		existing, e := q.lockSlackConnection(ctx, c.UserID)
		found := e == nil
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		next := SlackConnection{UserID: c.UserID, Generation: 1, TokenSealed: c.TokenSealed, TokenLast4: c.TokenLast4, TeamID: c.TeamID, TeamName: c.TeamName, TeamURL: c.TeamURL,
			SlackUserID: c.SlackUserID, SlackUserName: c.SlackUserName, Scopes: c.Scopes, ImmediateNotify: 1, DetectMentions: 1, DetectDMs: 1, DetectGroupDMs: 0,
			Status: SlackActive, ConnectedAt: now, NextPollAt: now, CreatedAt: now, UpdatedAt: now}
		if found {
			next.Generation, next.CreatedAt = existing.Generation+1, existing.CreatedAt
			next.ImmediateNotify, next.DetectMentions, next.DetectDMs, next.DetectGroupDMs = existing.ImmediateNotify, existing.DetectMentions, existing.DetectDMs, existing.DetectGroupDMs
			if existing.TeamID != c.TeamID || existing.SlackUserID != c.SlackUserID {
				if e = q.untrackSourceActionItems(ctx, c.UserID, SourceSlack, now); e != nil {
					return e
				}
			}
		}
		settings.apply(&next)
		if found {
			e = q.replaceSlackConnection(ctx, next)
		} else {
			e = q.createSlackConnection(ctx, next)
		}
		if e != nil {
			return e
		}
		saved = next
		return nil
	})
	if err != nil {
		return SlackConnection{}, err
	}
	return saved, nil
}

// UpdateSlackSettings changes the settings that are not nil and keeps the
// others. It returns ErrConnectionNotFound without a connection.
func (s *Store) UpdateSlackSettings(ctx context.Context, userID string, settings SlackSettings, sessionID string, now int64) (SlackConnection, error) {
	var saved SlackConnection
	err := s.transaction(ctx, func(q queries) error {
		if e := lockActiveUser(ctx, q, userID, sessionID, now); e != nil {
			return e
		}
		c, e := q.lockSlackConnection(ctx, userID)
		if errors.Is(e, sql.ErrNoRows) {
			return ErrConnectionNotFound
		}
		if e != nil {
			return e
		}
		settings.apply(&c)
		c.UpdatedAt = now
		n, e := q.updateSlackSettings(ctx, c)
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
		return SlackConnection{}, err
	}
	return saved, nil
}

// DeleteSlackConnection removes the user's connection. The user's Slack
// items are kept, but stop being tracked and lose their source key, and
// detection notifications of Slack items that were not sent yet are
// dropped. Deleting a missing connection succeeds.
func (s *Store) DeleteSlackConnection(ctx context.Context, userID, sessionID string, now int64) error {
	return s.transaction(ctx, func(q queries) error {
		// Lock order: users, slack_connections, action_items, notifications.
		if e := lockActiveUser(ctx, q, userID, sessionID, now); e != nil {
			return e
		}
		if e := q.deleteSlackConnection(ctx, userID); e != nil {
			return e
		}
		if e := q.untrackSourceActionItems(ctx, userID, SourceSlack, now); e != nil {
			return e
		}
		return q.deleteUserPendingNotificationsBySource(ctx, userID, NotificationDetected, SourceSlack)
	})
}

// DueSlackConnections returns up to limit active connections of enabled
// users whose next poll time has come, earliest first.
func (s *Store) DueSlackConnections(ctx context.Context, now int64, limit int) ([]SlackConnection, error) {
	return s.q.listDueSlackConnections(ctx, now, limit)
}

// SlackJudgedKeys returns the source keys of up to limit of the user's Slack
// items whose key starts with prefix (compared exactly, case-sensitively)
// and that were created at or after createdSince, newest first. It reads
// without locking.
func (s *Store) SlackJudgedKeys(ctx context.Context, userID, prefix string, createdSince int64, limit int) ([]string, error) {
	return s.q.listSlackJudgedKeys(ctx, userID, prefix, createdSince, limit)
}

// SlackOpenTasks returns the content of up to limit of the user's open and
// tracked Slack items whose key starts with prefix, newest first. It reads
// without locking.
func (s *Store) SlackOpenTasks(ctx context.Context, userID, prefix string, limit int) ([]string, error) {
	return s.q.listSlackOpenTasks(ctx, userID, prefix, limit)
}

// SlackTx is one write of a Slack synchronization step. Conn is the
// connection as locked at the start of the transaction.
//
// The tables must be written in the lock order users, slack_connections,
// action_items, notifications: SlackSync has locked users and
// slack_connections, so callers write items before notifications.
type SlackTx struct {
	sourceTx
	Conn SlackConnection
}

// SlackSync runs fn in a transaction that first locks the user, checks that
// the user is enabled, and locks the connection of the given generation. If
// any check fails, fn is not called and ErrStaleConnection is returned.
// External requests must never be made inside fn.
func (s *Store) SlackSync(ctx context.Context, userID string, generation int64, fn func(*SlackTx) error) error {
	return s.transaction(ctx, func(q queries) error {
		if e := lockSyncUser(ctx, q, userID); e != nil {
			return e
		}
		c, e := q.lockSlackConnection(ctx, userID)
		if errors.Is(e, sql.ErrNoRows) {
			return ErrStaleConnection
		}
		if e != nil {
			return e
		}
		if c.Generation != generation {
			return ErrStaleConnection
		}
		return fn(&SlackTx{sourceTx: sourceTx{ctx: ctx, q: q, userID: userID}, Conn: c})
	})
}

// SaveState stores the synchronization fields of c (status, errors and poll
// times). The token, workspace, settings and ConnectedAt are not written.
func (t *SlackTx) SaveState(c SlackConnection) error {
	c.UserID, c.Generation = t.Conn.UserID, t.Conn.Generation
	n, e := t.q.updateSlackSyncState(t.ctx, c)
	if e != nil {
		return e
	}
	if n != 1 {
		return ErrStaleConnection
	}
	return nil
}
