package notify

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"time"

	"github.com/i2bskn/astovia/internal/store"
)

const (
	dispatchBatch   = 100
	dispatchTimeout = 30 * time.Second
	// maxAttempts is the number of failed sends after which a notification
	// is given up.
	maxAttempts = 4
	retention   = 30 * 24 * time.Hour
	// rateLimitPause is how long a rate-limited scope is not sent to.
	rateLimitPause = 60
	// rateLimitGiveUp is the age in seconds after which a rate-limited
	// notification is given up instead of postponed again.
	rateLimitGiveUp = 86400
)

// retryDelays are the waits in seconds after the first, second and third
// failed sends.
var retryDelays = []int64{60, 300, 1800}

// Dispatcher sends the queued notifications whose time has come. Sending
// happens outside transactions; only the result is written afterwards.
type Dispatcher struct {
	Store *store.Store
	// Channels maps store.ChannelWebPush, store.ChannelSlack and
	// store.ChannelEmail to their channel. A missing channel (e-mail when
	// the server has no mail service) finishes its notifications with
	// no_subscription.
	Channels map[string]Channel
	Now      func() time.Time
	// paused maps a rate-limited scope ("email", or "slack:" + user ID) to
	// the Unix time until which it is not sent to. It lives in memory and
	// across runs. RunOnce is called from one goroutine at a time (the
	// worker loop), so it needs no lock.
	paused map[string]int64
}

// channelName returns the channel of a notification; a row without one
// (not expected, as the column has a default) is Web Push.
func channelName(n store.Notification) string {
	if n.Channel == "" {
		return store.ChannelWebPush
	}
	return n.Channel
}

// channelFor selects the channel of a notification by its channel column.
func (d *Dispatcher) channelFor(n store.Notification) (Channel, bool) {
	c, ok := d.Channels[channelName(n)]
	return c, ok && c != nil
}

// pauseScope returns the scope a rate limit of the channel applies to: the
// mail service as a whole, or one user's Slack webhook. Web Push has none.
func pauseScope(channel, userID string) string {
	switch channel {
	case store.ChannelEmail:
		return "email"
	case store.ChannelSlack:
		return "slack:" + userID
	}
	return ""
}

// tag returns the notification tag: notifications about the same item (or
// the digest, or tests) replace each other on the device.
func tag(n store.Notification) string {
	switch n.Kind {
	case store.NotificationDetected, store.NotificationStale:
		return n.Kind + "-" + n.ActionItemID.String
	case store.NotificationDueSoon, store.NotificationOverdue:
		return "due-" + n.ActionItemID.String
	}
	return n.Kind
}

// RunOnce sends up to 100 due notifications within 30 seconds.
func (d *Dispatcher) RunOnce(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, dispatchTimeout)
	defer cancel()
	list, err := d.Store.DueNotifications(ctx, d.Now().Unix(), dispatchBatch)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("notification dispatcher: list: %v", err)
		}
		return
	}
	for _, n := range list {
		if ctx.Err() != nil {
			return
		}
		d.deliver(ctx, parent, n)
	}
}

// deliver sends n with ctx and records the result with parent, so that a
// result is still written when the run's deadline passes after sending.
func (d *Dispatcher) deliver(ctx, parent context.Context, n store.Notification) {
	channel := channelName(n)
	scope := pauseScope(channel, n.UserID)
	// A paused scope is checked before anything is read, so that its rows
	// move out of the head of the queue without further queries.
	if until, ok := d.paused[scope]; ok && scope != "" {
		if until > d.Now().Unix() {
			d.postpone(parent, n, until)
			return
		}
		delete(d.paused, scope)
	}
	if n.ActionItemID.Valid {
		item, err := d.Store.ActionItem(ctx, n.UserID, n.ActionItemID.String)
		if err != nil && !errors.Is(err, store.ErrItemNotFound) {
			log.Printf("notification dispatcher: user=%s: read item: %v", n.UserID, err)
			return
		}
		if err != nil || item.Status != "open" {
			d.finish(parent, n, "skipped")
			return
		}
	}
	ch, ok := d.channelFor(n)
	if !ok {
		d.finish(parent, n, "no_subscription")
		return
	}
	_, err := ch.Send(ctx, n.UserID, Message{ID: n.ID, Kind: n.Kind, Title: n.Title, Body: n.Body, URL: n.URL, Tag: tag(n)})
	switch {
	case err == nil:
		d.finish(parent, n, "")
	case errors.Is(err, store.ErrNoSubscription):
		d.finish(parent, n, "no_subscription")
	case errors.Is(err, ErrDestinationInvalid):
		d.finish(parent, n, "destination_invalid")
	case ctx.Err() != nil:
		// Out of time or shutting down: sent again by a later run.
	case errors.Is(err, ErrRateLimited):
		logFailure(n.UserID, channel, "rate_limited", err)
		until := d.Now().Unix() + rateLimitPause
		if scope != "" {
			if d.paused == nil {
				d.paused = map[string]int64{}
			}
			d.paused[scope] = until
		}
		d.postpone(parent, n, until)
	default:
		logFailure(n.UserID, channel, "send_failed", err)
		n.Attempts++
		n.LastError = sql.NullString{String: "send_failed", Valid: true}
		if n.Attempts >= maxAttempts {
			n.SentAt = sql.NullInt64{Int64: d.Now().Unix(), Valid: true}
		} else {
			n.ScheduledAt = d.Now().Unix() + retryDelays[n.Attempts-1]
		}
		d.save(parent, n)
	}
}

// logFailure logs a failed send. The error of Web Push is not logged, as it
// can contain the push endpoint URL; the errors of Slack and e-mail carry
// only status codes and safe codes.
func logFailure(userID, channel, what string, err error) {
	if channel == store.ChannelWebPush {
		log.Printf("notification dispatcher: user=%s channel=%s: %s", userID, channel, what)
		return
	}
	log.Printf("notification dispatcher: user=%s channel=%s: %s: %v", userID, channel, what, err)
}

// postpone schedules n at until without counting an attempt, or gives it up
// with rate_limited once it is older than rateLimitGiveUp.
func (d *Dispatcher) postpone(ctx context.Context, n store.Notification, until int64) {
	if d.Now().Unix()-n.CreatedAt > rateLimitGiveUp {
		d.finish(ctx, n, "rate_limited")
		return
	}
	n.ScheduledAt = until
	n.LastError = sql.NullString{String: "rate_limited", Valid: true}
	d.save(ctx, n)
}

// finish marks n as done, with lastError (empty for a successful send).
func (d *Dispatcher) finish(ctx context.Context, n store.Notification, lastError string) {
	n.SentAt = sql.NullInt64{Int64: d.Now().Unix(), Valid: true}
	n.LastError = sql.NullString{String: lastError, Valid: lastError != ""}
	d.save(ctx, n)
}

func (d *Dispatcher) save(ctx context.Context, n store.Notification) {
	if err := d.Store.SaveNotificationResult(ctx, n); err != nil && ctx.Err() == nil {
		log.Printf("notification dispatcher: user=%s: save result: %v", n.UserID, err)
	}
}

// Cleanup deletes notifications sent more than 30 days ago.
func (d *Dispatcher) Cleanup(ctx context.Context) {
	if err := d.Store.DeleteSentNotifications(ctx, d.Now().Add(-retention).Unix()); err != nil && ctx.Err() == nil {
		log.Printf("notification cleanup: %v", err)
	}
}
