package store

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
)

// Channels a notification is delivered through, stored in
// notifications.channel.
const (
	ChannelWebPush = "webpush"
	ChannelSlack   = "slack"
	ChannelEmail   = "email"
)

// RoutableKinds lists the notification kinds a user routes to channels, in
// display order. Test notifications are not routed; they go to the channel
// they are created for.
var RoutableKinds = []string{NotificationDetected, NotificationStale, NotificationDueSoon, NotificationOverdue, NotificationDigest}

// NotificationChannels is a user's choice of channels per notification kind
// and the user's Slack Incoming Webhook. Each kind list holds kinds of
// RoutableKinds in that order.
type NotificationChannels struct {
	UserID             string
	WebPush, Slack     []string
	Email              []string
	SlackWebhookSealed sql.NullString
	SlackLastError     sql.NullString
	UpdatedAt          int64
}

// notificationChannelsRow is a notification_channels row; the kinds are
// comma-separated.
type notificationChannelsRow struct {
	UserID             string
	WebpushKinds       string
	SlackKinds         string
	EmailKinds         string
	SlackWebhookSealed sql.NullString
	SlackLastError     sql.NullString
	UpdatedAt          int64
}

// defaultNotificationChannels returns the channels of a user without a row:
// every kind goes to Web Push, nothing to Slack or e-mail.
func defaultNotificationChannels(userID string) NotificationChannels {
	return NotificationChannels{UserID: userID, WebPush: slices.Clone(RoutableKinds), Slack: []string{}, Email: []string{}}
}

// SortKinds returns the routable kinds of list in the order of
// RoutableKinds, without duplicates. Unknown kinds are dropped.
func SortKinds(list []string) []string {
	out := make([]string, 0, len(RoutableKinds))
	for _, k := range RoutableKinds {
		if slices.Contains(list, k) {
			out = append(out, k)
		}
	}
	return out
}

func splitKinds(v string) []string {
	return SortKinds(strings.Split(v, ","))
}

func joinKinds(list []string) string {
	return strings.Join(SortKinds(list), ",")
}

func (r notificationChannelsRow) channels() NotificationChannels {
	return NotificationChannels{UserID: r.UserID, WebPush: splitKinds(r.WebpushKinds), Slack: splitKinds(r.SlackKinds), Email: splitKinds(r.EmailKinds), SlackWebhookSealed: r.SlackWebhookSealed, SlackLastError: r.SlackLastError, UpdatedAt: r.UpdatedAt}
}

func readNotificationChannels(ctx context.Context, q queries, userID string) (NotificationChannels, error) {
	row, err := q.notificationChannels(ctx, userID)
	if errors.Is(err, sql.ErrNoRows) {
		return defaultNotificationChannels(userID), nil
	}
	if err != nil {
		return NotificationChannels{}, err
	}
	return row.channels(), nil
}

// SlackReady reports whether notifications can be sent to the Slack
// webhook: one is registered and it has not been found unusable.
func (c NotificationChannels) SlackReady() bool {
	return c.SlackWebhookSealed.Valid && !c.SlackLastError.Valid
}

// NotificationChannels returns the user's channels, or the defaults when the
// user has saved none. No row is created.
func (s *Store) NotificationChannels(ctx context.Context, userID string) (NotificationChannels, error) {
	return readNotificationChannels(ctx, s.q, userID)
}

// SaveNotificationRoutes stores the kinds the user of the browser session
// sends to each channel. The Slack webhook is not changed; a new row has
// none.
func (s *Store) SaveNotificationRoutes(ctx context.Context, userID, sessionID string, webpush, slack, email []string, now int64) error {
	return s.transaction(ctx, func(q queries) error {
		// Lock order: users, notification_channels.
		if e := lockActiveUser(ctx, q, userID, sessionID, now); e != nil {
			return e
		}
		return q.upsertNotificationRoutes(ctx, notificationChannelsRow{UserID: userID, WebpushKinds: joinKinds(webpush), SlackKinds: joinKinds(slack), EmailKinds: joinKinds(email), UpdatedAt: now})
	})
}

// SetSlackWebhook stores the sealed Slack webhook of the user of the browser
// session and clears its last error. A new row gets the default kinds.
func (s *Store) SetSlackWebhook(ctx context.Context, userID, sessionID, sealed string, now int64) error {
	return s.transaction(ctx, func(q queries) error {
		// Lock order: users, notification_channels.
		if e := lockActiveUser(ctx, q, userID, sessionID, now); e != nil {
			return e
		}
		return q.upsertSlackWebhook(ctx, userID, sealed, now)
	})
}

// ClearSlackWebhook removes the Slack webhook of the user of the browser
// session. It succeeds when there is none.
func (s *Store) ClearSlackWebhook(ctx context.Context, userID, sessionID string, now int64) error {
	return s.transaction(ctx, func(q queries) error {
		// Lock order: users, notification_channels.
		if e := lockActiveUser(ctx, q, userID, sessionID, now); e != nil {
			return e
		}
		return q.clearSlackWebhook(ctx, userID, now)
	})
}

// MarkSlackWebhookInvalid records code as the last error of the user's Slack
// webhook while the stored webhook is still sealed, so that a webhook
// registered again in the meantime is not marked. The user's state is not
// checked, like SetAIStatus. The result reports whether a row was updated.
func (s *Store) MarkSlackWebhookInvalid(ctx context.Context, userID, sealed, code string) (bool, error) {
	n, err := s.q.markSlackWebhookInvalid(ctx, userID, sealed, code)
	return n == 1, err
}

// UserEmail returns the login address of an enabled user, or
// ErrNoSubscription when the user does not exist, is disabled or has no
// address.
func (s *Store) UserEmail(ctx context.Context, userID string) (string, error) {
	u, err := s.q.userByID(ctx, userID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNoSubscription
	}
	if err != nil {
		return "", err
	}
	if u.Disabled != 0 || u.Email == "" {
		return "", ErrNoSubscription
	}
	return u.Email, nil
}

// routing is what enqueueRouted needs to choose the channels of a user's
// notifications.
type routing struct {
	channels      NotificationChannels
	subscriptions int64
}

// readRouting reads the user's channels and push subscription count without
// locking them; the caller holds the lock of the user row.
func readRouting(ctx context.Context, q queries, userID string) (*routing, error) {
	c, e := readNotificationChannels(ctx, q, userID)
	if e != nil {
		return nil, e
	}
	n, e := q.countPushSubscriptions(ctx, userID)
	if e != nil {
		return nil, e
	}
	return &routing{channels: c, subscriptions: n}, nil
}

// targets returns the channels a notification of kind goes to. A kind that
// is not routable (such as a test) goes to Web Push only.
func (r *routing) targets(kind string) []string {
	if !slices.Contains(RoutableKinds, kind) {
		if r.subscriptions > 0 {
			return []string{ChannelWebPush}
		}
		return nil
	}
	var out []string
	if r.subscriptions > 0 && slices.Contains(r.channels.WebPush, kind) {
		out = append(out, ChannelWebPush)
	}
	if r.channels.SlackReady() && slices.Contains(r.channels.Slack, kind) {
		out = append(out, ChannelSlack)
	}
	// Whether the server can send e-mail is not checked here; the
	// dispatcher finishes the notification with no_subscription when it
	// cannot.
	if slices.Contains(r.channels.Email, kind) {
		out = append(out, ChannelEmail)
	}
	return out
}

// enqueueRouted stores one row of n per channel chosen by routes, with the
// same dedupe key. The Web Push row keeps n.ID and the other rows get new
// IDs, so that queueing the same n again does not collide on the primary
// key. A notification with no channel is not stored. Lock order: the caller
// holds the lock of the user row; notification_channels and
// web_push_subscriptions were read without locks (readRouting).
func enqueueRouted(ctx context.Context, q queries, userID string, routes *routing, n Notification) error {
	n.UserID = userID
	for _, ch := range routes.targets(n.Kind) {
		row := n
		row.Channel = ch
		if ch != ChannelWebPush {
			id, e := newID()
			if e != nil {
				return e
			}
			row.ID = id
		}
		if _, e := insertNotification(ctx, q, row); e != nil {
			return e
		}
	}
	return nil
}
