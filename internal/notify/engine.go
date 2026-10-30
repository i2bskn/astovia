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

const (
	// staleBatch bounds the items examined per user and run.
	staleBatch = 500
	// dueBatch bounds the items with a deadline examined per user and run.
	dueBatch = 500
	// dueSoonLead is how long before an item's deadline the due-soon
	// notification is queued.
	dueSoonLead = 86400
	// overdueInterval is the interval of the reminders after a deadline.
	// TODO: make both user settings when the reminder interval is decided.
	overdueInterval = 86400
)

// Engine decides which notifications to queue: stale open items, tasks
// near or past their deadline and the daily digest. Detection
// notifications are queued by the GitHub synchronization. Only enabled
// users with a push subscription are considered, so nothing piles up for
// users who cannot receive it.
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
	due, err := e.due(ctx, userID, settings, now)
	if err != nil {
		return err
	}
	list = append(list, due...)
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

// location returns the named time zone, or UTC when it cannot be loaded.
func location(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		return time.UTC
	}
	return loc
}

// stale returns a notification for each open item that has waited a
// multiple of stale_days. The period number in the dedupe key makes each
// item notify once per period. Tasks within dueSoonLead of their deadline
// or past it are left to due, so that one item gets one kind of reminder.
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
		if item.Kind == store.KindTask && item.DueAt.Valid && item.DueAt.Int64 <= now+dueSoonLead {
			continue
		}
		age := now - item.CreatedAt
		n, err := newNotification(store.NotificationStale, fmt.Sprintf("stale:%s:%d", item.ID, age/period), fmt.Sprintf("未対応が%d日続いています", age/86400), item.Content, "/tasks/"+item.ID, now)
		if err != nil {
			return nil, err
		}
		n.ActionItemID.String, n.ActionItemID.Valid = item.ID, true
		list = append(list, n)
	}
	return list, nil
}

// due returns a due-soon notification for each open task whose deadline is
// within dueSoonLead, and an overdue notification per overdueInterval after
// the deadline until the task is closed. The deadline in the dedupe key
// makes a changed deadline notify again. Items created within dueSoonLead
// of their deadline get no due-soon notification, and items created after
// their deadline are first reminded overdueInterval after creation, as the
// user already knows of them through the detection or their own action.
// Only the current period is queued; periods missed while the server was
// stopped are not.
func (e *Engine) due(ctx context.Context, userID string, settings store.NotificationSettings, now time.Time) ([]store.Notification, error) {
	unix := now.Unix()
	items, err := e.Store.DueActionItems(ctx, userID, unix+dueSoonLead, dueBatch)
	if err != nil {
		return nil, err
	}
	loc := location(settings.Timezone)
	var list []store.Notification
	for _, item := range items {
		due := item.DueAt.Int64
		at := time.Unix(due, 0).In(loc).Format("1/2 15:04")
		var kind, key, title string
		if unix < due {
			if item.CreatedAt > due-dueSoonLead {
				continue
			}
			kind, key, title = store.NotificationDueSoon, fmt.Sprintf("due_soon:%s:%d", item.ID, due), fmt.Sprintf("期限が近づいています（%s）", at)
		} else {
			k := (unix - max(due, item.CreatedAt)) / overdueInterval
			if item.CreatedAt >= due && k == 0 {
				continue
			}
			kind, key = store.NotificationOverdue, fmt.Sprintf("overdue:%s:%d:%d", item.ID, due, k)
			if days := (unix - due) / 86400; days == 0 {
				title = fmt.Sprintf("期限を過ぎました（%s）", at)
			} else {
				title = fmt.Sprintf("期限を%d日過ぎています", days)
			}
		}
		n, err := newNotification(kind, key, title, item.Content, "/tasks/"+item.ID, unix)
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
	local := now.In(location(settings.Timezone))
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
	candidates, err := e.Store.CountActionItems(ctx, userID, "candidate")
	if err != nil {
		return store.Notification{}, false, err
	}
	if open == 0 && auto == 0 && candidates == 0 {
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
	body, url := strings.Join(contents, "・"), "/"
	if candidates > 0 {
		body = fmt.Sprintf("タスク候補 %d件／", candidates) + body
	}
	if auto > 0 {
		body, url = fmt.Sprintf("自動で処理 %d件／", auto)+body, "/auto"
	}
	n, err := newNotification(store.NotificationDigest, key, fmt.Sprintf("今日の未完了 %d件", open), body, url, now.Unix())
	return n, err == nil, err
}
