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
)

// retryDelays are the waits in seconds after the first, second and third
// failed sends.
var retryDelays = []int64{60, 300, 1800}

// Dispatcher sends the queued notifications whose time has come. Sending
// happens outside transactions; only the result is written afterwards.
type Dispatcher struct {
	Store   *store.Store
	Channel Channel
	Now     func() time.Time
}

// channelFor selects the channel of a notification. Every notification goes
// to Web Push for now; this is the one place to change when users can choose
// other channels.
func (d *Dispatcher) channelFor(store.Notification) Channel {
	return d.Channel
}

// tag returns the notification tag: notifications about the same item (or
// the digest, or tests) replace each other on the device.
func tag(n store.Notification) string {
	switch n.Kind {
	case store.NotificationDetected, store.NotificationStale:
		return n.Kind + "-" + n.ActionItemID.String
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
	_, err := d.channelFor(n).Send(ctx, n.UserID, Message{Kind: n.Kind, Title: n.Title, Body: n.Body, URL: n.URL, Tag: tag(n)})
	switch {
	case err == nil:
		d.finish(parent, n, "")
	case errors.Is(err, store.ErrNoSubscription):
		d.finish(parent, n, "no_subscription")
	case ctx.Err() != nil:
		// Out of time or shutting down: sent again by a later run.
	default:
		log.Printf("notification dispatcher: user=%s: send_failed", n.UserID)
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
