package store

import (
	"context"
	"database/sql"
	"errors"
	"math"
)

var ErrSubscriptionNotFound = errors.New("push subscription not found")
var ErrTooManySubscriptions = errors.New("too many push subscriptions")
var ErrNoSubscription = errors.New("no push subscription")
var ErrSecretNotFound = errors.New("instance secret not found")

// MaxPushSubscriptions is the number of push subscriptions a user can hold.
const MaxPushSubscriptions = 20

// Notification kinds stored in notifications.kind.
const (
	NotificationDetected = "detected"
	NotificationStale    = "stale"
	NotificationDueSoon  = "due_soon"
	NotificationOverdue  = "overdue"
	NotificationDigest   = "digest"
	NotificationTest     = "test"
)

// Default notification settings, used while a user has no settings row.
const (
	DefaultTimezone   = "UTC"
	DefaultDigestHour = 8
	DefaultStaleDays  = 3
	// DefaultCandidateExpireDays is how many days a task candidate is left
	// before it is dismissed automatically. Zero never dismisses them.
	DefaultCandidateExpireDays = 3
)

// PushSubscription is a browser's Web Push subscription. P256dh and Auth are
// the subscription keys in unpadded base64url.
type PushSubscription struct {
	ID, UserID, Endpoint string
	P256dh, Auth, Label  string
	CreatedAt            int64
	LastUsedAt           sql.NullInt64
}

// NotificationSettings is a user's notification timing.
type NotificationSettings struct {
	UserID     string
	Timezone   string
	DigestHour int64
	StaleDays  int64
	UpdatedAt  int64
	// CandidateExpireDays is how many days after its last change a task
	// candidate is dismissed automatically; 0 keeps candidates.
	CandidateExpireDays int64
}

// Notification is a message waiting to be sent, or sent (SentAt set). Its
// content is fixed when it is created.
type Notification struct {
	ID, UserID, Kind string
	ActionItemID     sql.NullString
	DedupeKey        string
	Title, Body, URL string
	ScheduledAt      int64
	SentAt           sql.NullInt64
	Attempts         int64
	LastError        sql.NullString
	CreatedAt        int64
}

// InstanceSecret is a value shared by the whole instance, such as the VAPID
// key pair.
type InstanceSecret struct {
	Name, Value          string
	CreatedAt, UpdatedAt int64
}

// SavePushSubscription registers p for p.UserID. An endpoint the user already
// has is updated with the new keys and label (created false). An endpoint of
// another user moves to this user as a new subscription, since the browser
// is now signed in as this user. When timezone is not empty and the user has
// no notification settings yet, settings with that timezone are created.
func (s *Store) SavePushSubscription(ctx context.Context, p PushSubscription, timezone, sessionID string, now int64) (PushSubscription, bool, error) {
	var saved PushSubscription
	var created bool
	err := s.transaction(ctx, func(q queries) error {
		// Lock order: users, notification_settings, web_push_subscriptions.
		if e := lockActiveUser(ctx, q, p.UserID, sessionID, now); e != nil {
			return e
		}
		if timezone != "" {
			if e := q.createNotificationSettingsIfMissing(ctx, NotificationSettings{UserID: p.UserID, Timezone: timezone, DigestHour: DefaultDigestHour, StaleDays: DefaultStaleDays, UpdatedAt: now, CandidateExpireDays: DefaultCandidateExpireDays}); e != nil {
				return e
			}
		}
		existing, e := q.lockPushSubscriptionByEndpoint(ctx, p.Endpoint)
		found := e == nil
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		if found && existing.UserID == p.UserID {
			existing.P256dh, existing.Auth, existing.Label = p.P256dh, p.Auth, p.Label
			saved = existing
			return q.updatePushSubscriptionKeys(ctx, existing)
		}
		n, e := q.countPushSubscriptions(ctx, p.UserID)
		if e != nil {
			return e
		}
		if n >= MaxPushSubscriptions {
			return ErrTooManySubscriptions
		}
		if found {
			if e = q.deletePushSubscriptionByID(ctx, existing.ID); e != nil {
				return e
			}
		}
		p.CreatedAt, p.LastUsedAt = now, sql.NullInt64{}
		if e = q.createPushSubscription(ctx, p); e != nil {
			return e
		}
		saved, created = p, true
		return nil
	})
	if err != nil {
		return PushSubscription{}, false, err
	}
	return saved, created, nil
}

// PushSubscriptions returns the user's subscriptions, newest first. It never
// returns nil.
func (s *Store) PushSubscriptions(ctx context.Context, userID string) ([]PushSubscription, error) {
	return s.q.listPushSubscriptions(ctx, userID)
}

// DeletePushSubscription deletes the user's subscription, or returns
// ErrSubscriptionNotFound when it does not exist or belongs to another user.
func (s *Store) DeletePushSubscription(ctx context.Context, userID, id string) error {
	n, err := s.q.deletePushSubscription(ctx, userID, id)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrSubscriptionNotFound
	}
	return nil
}

// RemovePushSubscription deletes a subscription that the push service
// reported as gone.
func (s *Store) RemovePushSubscription(ctx context.Context, id string) error {
	return s.q.deletePushSubscriptionByID(ctx, id)
}

// TouchPushSubscription records a successful delivery.
func (s *Store) TouchPushSubscription(ctx context.Context, id string, now int64) error {
	return s.q.touchPushSubscription(ctx, id, now)
}

// PushUserIDs returns the enabled users that have at least one subscription.
func (s *Store) PushUserIDs(ctx context.Context) ([]string, error) {
	return s.q.listPushUserIDs(ctx)
}

// NotificationSettings returns the user's settings, or the defaults when the
// user has saved none. No row is created.
func (s *Store) NotificationSettings(ctx context.Context, userID string) (NotificationSettings, error) {
	n, err := s.q.notificationSettings(ctx, userID)
	if errors.Is(err, sql.ErrNoRows) {
		return NotificationSettings{UserID: userID, Timezone: DefaultTimezone, DigestHour: DefaultDigestHour, StaleDays: DefaultStaleDays, CandidateExpireDays: DefaultCandidateExpireDays}, nil
	}
	return n, err
}

// SaveNotificationSettings stores the user's settings.
func (s *Store) SaveNotificationSettings(ctx context.Context, n NotificationSettings) error {
	return s.q.upsertNotificationSettings(ctx, n)
}

// CreateTestNotification queues n for the user of the browser session, or
// returns ErrNoSubscription when the user has no subscription.
func (s *Store) CreateTestNotification(ctx context.Context, n Notification, sessionID string, now int64) error {
	return s.transaction(ctx, func(q queries) error {
		if e := lockActiveUser(ctx, q, n.UserID, sessionID, now); e != nil {
			return e
		}
		count, e := q.countPushSubscriptions(ctx, n.UserID)
		if e != nil {
			return e
		}
		if count == 0 {
			return ErrNoSubscription
		}
		_, e = q.enqueueNotification(ctx, n)
		return e
	})
}

// EnqueueNotifications queues the notifications of an enabled user that has
// a subscription; otherwise nothing is stored. Notifications whose dedupe key
// the user already has are skipped.
func (s *Store) EnqueueNotifications(ctx context.Context, userID string, list []Notification) error {
	return s.transaction(ctx, func(q queries) error {
		u, e := q.lockUser(ctx, userID)
		if errors.Is(e, sql.ErrNoRows) {
			return nil
		}
		if e != nil {
			return e
		}
		if u.Disabled != 0 {
			return nil
		}
		count, e := q.countPushSubscriptions(ctx, userID)
		if e != nil || count == 0 {
			return e
		}
		for _, n := range list {
			n.UserID = userID
			if _, e = q.enqueueNotification(ctx, n); e != nil {
				return e
			}
		}
		return nil
	})
}

// HasNotification reports whether the user has a notification with the
// dedupe key, sent or not.
func (s *Store) HasNotification(ctx context.Context, userID, dedupeKey string) (bool, error) {
	n, err := s.q.countNotificationsByDedupeKey(ctx, userID, dedupeKey)
	return n > 0, err
}

// LatestNotificationAt returns when the user's latest notification of kind
// was created, and false when there is none.
func (s *Store) LatestNotificationAt(ctx context.Context, userID, kind string) (int64, bool, error) {
	at, err := s.q.latestNotificationCreatedAt(ctx, userID, kind)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return at, err == nil, err
}

// DueNotifications returns up to limit unsent notifications of enabled users
// scheduled at or before now, oldest first.
func (s *Store) DueNotifications(ctx context.Context, now int64, limit int) ([]Notification, error) {
	return s.q.listDueNotifications(ctx, now, limit)
}

// SaveNotificationResult stores the attempts, last error, schedule and sent
// time of n, unless it was sent or deleted in the meantime.
func (s *Store) SaveNotificationResult(ctx context.Context, n Notification) error {
	return s.q.updateNotificationResult(ctx, n)
}

// DeleteSentNotifications deletes notifications sent before the time.
func (s *Store) DeleteSentNotifications(ctx context.Context, before int64) error {
	return s.q.deleteSentNotificationsBefore(ctx, before)
}

// StaleActionItems returns up to limit of the user's open items created at
// or before createdBefore, oldest first.
func (s *Store) StaleActionItems(ctx context.Context, userID string, createdBefore int64, limit int) ([]ActionItem, error) {
	return s.q.listStaleActionItems(ctx, userID, createdBefore, limit)
}

// DueActionItems returns up to limit of the user's open tasks whose due
// time is at or before dueBefore, earliest due first.
func (s *Store) DueActionItems(ctx context.Context, userID string, dueBefore int64, limit int) ([]ActionItem, error) {
	return s.q.listDueActionItems(ctx, userID, dueBefore, limit)
}

// CountActionItems returns the number of the user's items with the status.
func (s *Store) CountActionItems(ctx context.Context, userID, status string) (int64, error) {
	return s.q.countActionItemsByStatus(ctx, userID, status)
}

// CandidateSweepUserIDs returns the enabled users that have a task
// candidate, or an open or candidate event whose start is at or before now.
func (s *Store) CandidateSweepUserIDs(ctx context.Context, now int64) ([]string, error) {
	return s.q.listCandidateSweepUserIDs(ctx, now)
}

// ExpireCandidates dismisses the user's task candidates last changed at or
// before updatedBefore, and returns how many it dismissed. The parents of
// the dismissed items are synchronized in the same transaction.
func (s *Store) ExpireCandidates(ctx context.Context, userID string, updatedBefore, now int64) (int64, error) {
	return s.expire(ctx, userID, now, func(q queries) ([]sql.NullString, error) {
		return q.expireCandidates(ctx, userID, updatedBefore, now)
	})
}

// ExpireEvents dismisses the user's open or candidate events that started at
// or before now, and returns how many it dismissed. The parents of the
// dismissed items are synchronized in the same transaction.
func (s *Store) ExpireEvents(ctx context.Context, userID string, now int64) (int64, error) {
	return s.expire(ctx, userID, now, func(q queries) ([]sql.NullString, error) {
		return q.expireEvents(ctx, userID, now)
	})
}

// expire runs a sweep that returns the parent IDs of the items it changed,
// then synchronizes those parents; a failure rolls back the whole sweep.
func (s *Store) expire(ctx context.Context, userID string, now int64, sweep func(queries) ([]sql.NullString, error)) (int64, error) {
	var n int64
	err := s.transaction(ctx, func(q queries) error {
		parents, e := sweep(q)
		if e != nil {
			return e
		}
		n = int64(len(parents))
		ids := make([]string, 0, len(parents))
		for _, p := range parents {
			if p.Valid {
				ids = append(ids, p.String)
			}
		}
		return syncParents(ctx, q, userID, now, ids...)
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}

// CountAutoActionItemsSince returns the number of the user's items completed
// automatically after the time. Items dismissed automatically are not
// counted.
func (s *Store) CountAutoActionItemsSince(ctx context.Context, userID string, since int64) (int64, error) {
	return s.q.countAutoActionItemsSince(ctx, userID, since)
}

// ListAutoActionItems returns up to limit of the user's automatically
// completed items after the cursor in auto_at DESC, id DESC order. An empty
// status lists every status.
func (s *Store) ListAutoActionItems(ctx context.Context, userID, status string, after *AutoActionItemCursor, limit int) ([]ActionItem, error) {
	c := AutoActionItemCursor{AutoAt: math.MaxInt64}
	if after != nil {
		c = *after
	}
	return s.q.listAutoActionItems(ctx, userID, status, c, limit)
}

// InstanceSecret returns the named secret, or ErrSecretNotFound.
func (s *Store) InstanceSecret(ctx context.Context, name string) (InstanceSecret, error) {
	sec, err := s.q.instanceSecret(ctx, name)
	if errors.Is(err, sql.ErrNoRows) {
		return InstanceSecret{}, ErrSecretNotFound
	}
	return sec, err
}

// CreateInstanceSecret stores the named secret unless it exists, and reports
// whether it was stored. Concurrent creators therefore agree on one value.
func (s *Store) CreateInstanceSecret(ctx context.Context, name, value string, now int64) (bool, error) {
	n, err := s.q.createInstanceSecret(ctx, InstanceSecret{Name: name, Value: value, CreatedAt: now, UpdatedAt: now})
	return n == 1, err
}

// ReplaceInstanceSecret replaces the named secret only while it still holds
// oldValue, and reports whether it did. When clearPushSubscriptions is set,
// a successful replacement also deletes every push subscription in the same
// transaction (used when the VAPID key pair changes). As the one exception
// to the lock order, this transaction does not lock users, updates
// instance_secrets first and touches no table other than instance_secrets and
// web_push_subscriptions.
func (s *Store) ReplaceInstanceSecret(ctx context.Context, name, oldValue, newValue string, now int64, clearPushSubscriptions bool) (bool, error) {
	var replaced bool
	err := s.transaction(ctx, func(q queries) error {
		n, e := q.replaceInstanceSecret(ctx, name, oldValue, newValue, now)
		if e != nil || n != 1 {
			return e
		}
		replaced = true
		if clearPushSubscriptions {
			return q.deleteAllPushSubscriptions(ctx)
		}
		return nil
	})
	return replaced, err
}
