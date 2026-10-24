package notify

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"
	// Time zones are resolved without the host's zoneinfo files.
	_ "time/tzdata"

	"github.com/i2bskn/astovia/internal/auth"
	"github.com/i2bskn/astovia/internal/store"
)

// staleBatch bounds the items examined per user and run.
const staleBatch = 500

// Engine decides which notifications to queue: stale open items and the
// daily digest. Detection notifications are queued by the GitHub
// synchronization. Only enabled users with a push subscription are
// considered, so nothing piles up for users who cannot receive it.
type Engine struct {
	Store *store.Store
	Now   func() time.Time
}

// RunOnce queues the notifications due now.
func (e *Engine) RunOnce(ctx context.Context) {
	users, err := e.Store.PushUserIDs(ctx)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("notification engine: list users: %v", err)
		}
		return
	}
	for _, uid := range users {
		if ctx.Err() != nil {
			return
		}
		if err = e.user(ctx, uid); err != nil && ctx.Err() == nil {
			log.Printf("notification engine: user=%s: %v", uid, err)
		}
	}
}

func (e *Engine) user(ctx context.Context, userID string) error {
	settings, err := e.Store.NotificationSettings(ctx, userID)
	if err != nil {
		return err
	}
	now := e.Now()
	list, err := e.stale(ctx, userID, settings, now.Unix())
	if err != nil {
		return err
	}
	digest, ok, err := e.digest(ctx, userID, settings, now)
	if err != nil {
		return err
	}
	if ok {
		list = append(list, digest)
	}
	if len(list) == 0 {
		return nil
	}
	return e.Store.EnqueueNotifications(ctx, userID, list)
}

func newNotification(kind, dedupeKey, title, body, url string, now int64) (store.Notification, error) {
	id, err := auth.Random()
	if err != nil {
		return store.Notification{}, err
	}
	return store.Notification{ID: id, Kind: kind, DedupeKey: dedupeKey, Title: title, Body: body, URL: url, ScheduledAt: now, CreatedAt: now}, nil
}

// stale returns a notification for each open item that has waited a
// multiple of stale_days. The period number in the dedupe key makes each
// item notify once per period.
func (e *Engine) stale(ctx context.Context, userID string, settings store.NotificationSettings, now int64) ([]store.Notification, error) {
	period := settings.StaleDays * 86400
	if period <= 0 {
		return nil, nil
	}
	items, err := e.Store.StaleActionItems(ctx, userID, now-period, staleBatch)
	if err != nil {
		return nil, err
	}
	var list []store.Notification
	for _, item := range items {
		age := now - item.CreatedAt
		n, err := newNotification(store.NotificationStale, fmt.Sprintf("stale:%s:%d", item.ID, age/period), fmt.Sprintf("未対応が%d日続いています", age/86400), item.Content, "/admin/", now)
		if err != nil {
			return nil, err
		}
		n.ActionItemID.String, n.ActionItemID.Valid = item.ID, true
		list = append(list, n)
	}
	return list, nil
}

// digest returns the daily digest when the user's local time is in the
// digest hour and no digest exists for the local date. A day whose digest
// hour is skipped (a daylight saving change, or the server being stopped)
// gets no digest.
func (e *Engine) digest(ctx context.Context, userID string, settings store.NotificationSettings, now time.Time) (store.Notification, bool, error) {
	loc, err := time.LoadLocation(settings.Timezone)
	if err != nil {
		loc = time.UTC
	}
	local := now.In(loc)
	if int64(local.Hour()) != settings.DigestHour {
		return store.Notification{}, false, nil
	}
	key := "digest:" + local.Format("2006-01-02")
	if has, err := e.Store.HasNotification(ctx, userID, key); err != nil || has {
		return store.Notification{}, false, err
	}
	open, err := e.Store.CountActionItems(ctx, userID, "open")
	if err != nil {
		return store.Notification{}, false, err
	}
	since := now.Unix() - 86400
	if at, ok, err := e.Store.LatestNotificationAt(ctx, userID, store.NotificationDigest); err != nil {
		return store.Notification{}, false, err
	} else if ok {
		since = at
	}
	auto, err := e.Store.CountAutoActionItemsSince(ctx, userID, since)
	if err != nil {
		return store.Notification{}, false, err
	}
	if open == 0 && auto == 0 {
		return store.Notification{}, false, nil
	}
	items, err := e.Store.ListActionItems(ctx, userID, "open", nil, 3)
	if err != nil {
		return store.Notification{}, false, err
	}
	contents := make([]string, 0, len(items))
	for _, item := range items {
		contents = append(contents, item.Content)
	}
	body, url := strings.Join(contents, "・"), "/admin/"
	if auto > 0 {
		body, url = fmt.Sprintf("自動で処理 %d件／", auto)+body, "/admin/?view=auto"
	}
	n, err := newNotification(store.NotificationDigest, key, fmt.Sprintf("今日の未完了 %d件", open), body, url, now.Unix())
	return n, err == nil, err
}
