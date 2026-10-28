package chatworksync

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/i2bskn/astovia/internal/auth"
	"github.com/i2bskn/astovia/internal/chatwork"
	"github.com/i2bskn/astovia/internal/store"
)

const (
	// taskItemsLimit bounds the open imported items checked against the
	// task list.
	taskItemsLimit = 200
	// taskListLimit is the most tasks GET /my/tasks returns; a list this
	// long may be cut.
	taskListLimit = 100
	taskNotice    = "Chatwork のタスク"
)

// importTasks creates an item for each open Chatwork task assigned to the
// user, and completes the imported items whose task left the list, when the
// list is known to be complete. The items are not tracked by the AI.
func (r *run) importTasks() error {
	if !r.take(callTasks) {
		return nil
	}
	page, err := r.s.Chatwork.MyTasks(r.ctx, r.token)
	if err != nil {
		// GET /my/tasks has no error about one target: every error stops
		// the synchronization.
		return err
	}
	loc, err := r.location()
	if err != nil {
		return err
	}
	first := !r.state.LastTasksAt.Valid
	open := map[string]bool{}
	for _, t := range page.Tasks {
		open[t.ID] = true
		if err = r.importTask(t, loc, first); err != nil {
			return err
		}
	}
	if dropped := page.Raw - len(page.Tasks); page.Raw >= taskListLimit || dropped > 0 {
		r.warn("closed tasks not checked", "count", page.Raw, "dropped", dropped)
	} else if err = r.closeTasks(open); err != nil {
		return err
	}
	r.state.LastTasksAt = nullInt(r.start.Unix())
	return nil
}

// importTask creates the item of a task unless it exists. A detection
// notification is queued except on the first import.
func (r *run) importTask(t chatwork.Task, loc *time.Location, first bool) error {
	now := r.s.Now().Unix()
	id, err := auth.Random()
	if err != nil {
		return err
	}
	names := r.names(nil)
	if name := cleanText(t.AssignedByName); t.AssignedByID != "" && name != "" {
		names[t.AssignedByID] = name
	}
	content := excerpt(formatText(t.Body, names), contentLimit)
	if content == "" {
		content = taskNotice
	}
	refs, err := json.Marshal([]ref{{Service: store.SourceChatwork, Account: r.conn.OrganizationName, ConversationID: t.RoomID,
		ConversationName: truncate(cleanText(t.RoomName), conversationNameLimit), MessageID: t.MessageID, URL: chatworkURL(messageURL(t.RoomID, t.MessageID)).String}})
	if err != nil {
		return err
	}
	key := taskPrefix(r.conn.AccountID) + t.ID
	item := store.ActionItem{ID: id, Content: content, Status: "open", Refs: string(refs), Version: 1, CreatedAt: now, UpdatedAt: now,
		Source: nullString(store.SourceChatwork), SourceKey: nullString(key), Tracking: 0, DetectedAt: nullInt(now), Kind: store.KindTask}
	switch {
	case t.LimitTime <= 0:
	case t.LimitType == "time":
		item.DueAt = nullInt(t.LimitTime)
	case t.LimitType == "date":
		// A date limit is due at the end of the day, in the user's time
		// zone.
		d := time.Unix(t.LimitTime, 0).In(loc)
		item.DueAt = nullInt(time.Date(d.Year(), d.Month(), d.Day(), 23, 59, 0, 0, loc).Unix())
	}
	return r.s.Store.ChatworkSync(r.parent, r.conn.UserID, r.conn.Generation, func(tx *store.ChatworkTx) error {
		if _, found, e := tx.LockItemBySourceKey(key); e != nil || found {
			return e
		}
		created, e := tx.CreateItem(item)
		if e != nil || !created || first || tx.Conn.ImmediateNotify != 1 {
			return e
		}
		return tx.Enqueue(detectedNotification("Chatwork: タスク", item, now))
	})
}

// closeTasks completes the open imported items whose task is not in open.
func (r *run) closeTasks(open map[string]bool) error {
	prefix := taskPrefix(r.conn.AccountID)
	items, err := r.s.Store.ChatworkTaskItems(r.parent, r.conn.UserID, prefix, taskItemsLimit)
	if err != nil {
		return err
	}
	for _, item := range items {
		task := strings.TrimPrefix(item.SourceKey.String, prefix)
		if !chatwork.ValidID(task) || open[task] {
			continue
		}
		if err = r.closeTask(item); err != nil {
			return err
		}
	}
	return nil
}

// closeTask completes an imported item whose task left the list, when it
// is still the open Chatwork item of the key and version read, and drops
// its pending notifications.
func (r *run) closeTask(item store.ActionItem) error {
	now := r.s.Now().Unix()
	return r.s.Store.ChatworkSync(r.parent, r.conn.UserID, r.conn.Generation, func(tx *store.ChatworkTx) error {
		cur, found, e := tx.LockItem(item.ID)
		if e != nil || !found {
			return e
		}
		if cur.Status != "open" || cur.Source.String != store.SourceChatwork || cur.SourceKey != item.SourceKey || cur.Version != item.Version {
			return nil
		}
		next := cur
		next.Status, next.AutoReason, next.AutoRef, next.AutoAt, next.UpdatedAt = "done", nullString(ReasonTaskClosed), chatworkURL(firstRefURL(cur.Refs)), nullInt(now), now
		_, ok, e := tx.UpdateItem(cur, next)
		if e != nil || !ok {
			return e
		}
		return tx.DropPendingNotifications(cur.ID)
	})
}

// firstRefURL returns the URL of the first reference of an item.
func firstRefURL(refs string) string {
	var list []ref
	if err := json.Unmarshal([]byte(refs), &list); err != nil || len(list) == 0 {
		return ""
	}
	return list[0].URL
}
