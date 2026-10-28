package chatworksync

import (
	"errors"

	"github.com/i2bskn/astovia/internal/ai"
	"github.com/i2bskn/astovia/internal/chatwork"
	"github.com/i2bskn/astovia/internal/store"
)

const (
	trackBatch = 100
	// trackItems is the number of items checked by one tracking pass and
	// trackAICalls the AI calls it may make.
	trackItems   = 20
	trackAICalls = 3
	// replyPosts is the number of later messages sent with a reply
	// judgment.
	replyPosts = 10
	// trackGrace allows for messages sent just before the last check.
	trackGrace = 60
)

// passState tells the tracking pass how to go on after an item.
type passState int

const (
	passNext  passState = iota // check the next item
	passEnd                    // stop here; the pass counts as done
	passAbort                  // stop without recording the pass
)

// track checks up to trackItems tracked open items, least recently checked
// first: when the user wrote in the room since the last check, the AI
// judges whether that completed the item. The items to check and their room
// come from the source key only.
func (r *run) track() error {
	items, err := r.s.Store.TrackedActionItems(r.parent, r.conn.UserID, store.SourceChatwork, trackBatch)
	if err != nil {
		return err
	}
	r.tracking = true
	defer func() { r.tracking = false }()
	passStart := r.start.Unix()
	for i, item := range items {
		if i >= trackItems {
			break
		}
		if err = r.ctx.Err(); err != nil {
			return err
		}
		state, err := r.trackItem(item, passStart)
		if err != nil {
			return err
		}
		if state == passAbort {
			return nil
		}
		if state == passEnd {
			break
		}
	}
	r.state.LastTrackedAt = nullInt(passStart)
	return nil
}

// trackItem checks one item. The latest messages of its room are read; the
// original message may have scrolled out of them in a busy room, and then
// the later messages are judged without it.
func (r *run) trackItem(item store.ActionItem, passStart int64) (passState, error) {
	key, ok := parseSourceKey(item.SourceKey.String)
	if !ok || key.Account != r.conn.AccountID {
		return passNext, r.untrack(item)
	}
	checked := item.CreatedAt
	switch {
	case item.TrackedAt.Valid:
		checked = item.TrackedAt.Int64
	case item.DetectedAt.Valid:
		checked = item.DetectedAt.Int64
	}
	c, err := r.roomMessages(key.Room)
	switch {
	case errors.Is(err, errBudget):
		return passEnd, nil
	case err != nil && stops(err):
		return passAbort, err
	case err != nil:
		return passNext, r.untrack(item)
	}
	var originMsg *chatwork.Message
	var later []chatwork.Message
	for i, m := range c.messages {
		switch {
		case m.ID == key.Last:
			originMsg = &c.messages[i]
		case chatwork.CompareIDs(m.ID, key.Last) > 0 && m.SendTime >= checked-trackGrace:
			later = append(later, m)
		}
	}
	var mine *chatwork.Message
	for i := range later {
		if later[i].AccountID == r.conn.AccountID && later[i].SendTime >= checked {
			mine = &later[i]
		}
	}
	if mine == nil {
		return passNext, r.touch(item, passStart)
	}
	return r.judgeReply(item, key, c, originMsg, later, *mine, passStart)
}

// judgeReply asks the AI whether the user's messages completed the item.
func (r *run) judgeReply(item store.ActionItem, key sourceKey, c *roomCache, originMsg *chatwork.Message, later []chatwork.Message, mine chatwork.Message, passStart int64) (passState, error) {
	switch {
	case r.aiStopped || r.aiFallbackOnly:
		return passAbort, nil
	case r.aiStale:
		return passNext, nil
	case r.trackCalls >= trackAICalls || r.deadlineNear():
		return passEnd, nil
	}
	cur, apiKey, gate, err := r.reloadAI()
	switch {
	case err != nil:
		return passAbort, err
	case gate == gateStale:
		return passNext, nil
	case gate != gateOK:
		return passAbort, nil
	}
	// The newest replyPosts later messages, with the user's latest message
	// in place of the oldest when it is older than all of them.
	tail := later
	if len(tail) > replyPosts {
		tail = append([]chatwork.Message{}, tail[len(tail)-replyPosts:]...)
		if indexID(tail, mine.ID) < 0 {
			tail[0] = mine
		}
	}
	var msgs []chatwork.Message
	if originMsg != nil {
		msgs = append(msgs, *originMsg)
	}
	msgs = append(msgs, tail...)
	loc, err := r.location()
	if err != nil {
		return passAbort, err
	}
	names := r.names(c)
	in := ai.ReplyInput{Me: ai.SlackPerson{ID: r.conn.AccountID, Name: r.conn.AccountName}, Location: loc, Task: item.Content}
	for _, m := range msgs {
		in.Posts = append(in.Posts, r.post(m, false, names))
	}
	r.trackCalls++
	res, err := r.s.AI.JudgeChatworkReply(r.ctx, apiKey, cur.Model, in)
	class, err := r.classifyAI(cur, err)
	if err != nil {
		return passAbort, err
	}
	switch class {
	case aiOK:
		if !res.Resolved {
			return passNext, r.touch(item, passStart)
		}
		return passNext, r.resolve(item, messageURL(key.Room, mine.ID))
	case aiUnusable:
		return passNext, r.touch(item, passStart)
	case aiFailed:
		return passAbort, nil
	}
	// A temporary failure: the item is checked again by the next pass.
	return passNext, nil
}

// touch records that the item was checked at the start of the pass. Nothing
// is written when the item changed since it was read.
func (r *run) touch(item store.ActionItem, at int64) error {
	return r.s.Store.ChatworkSync(r.parent, r.conn.UserID, r.conn.Generation, func(tx *store.ChatworkTx) error {
		return tx.TouchItem(item, at)
	})
}

// change applies fn to the item after locking it, when it is still the
// open (or candidate), tracked Chatwork item of the version read.
func (r *run) change(item store.ActionItem, fn func(next *store.ActionItem), resolved bool) error {
	now := r.s.Now().Unix()
	return r.s.Store.ChatworkSync(r.parent, r.conn.UserID, r.conn.Generation, func(tx *store.ChatworkTx) error {
		cur, found, e := tx.LockItem(item.ID)
		if e != nil || !found {
			return e
		}
		if (cur.Status != "open" && cur.Status != "candidate") || cur.Tracking != 1 || cur.Source.String != store.SourceChatwork || cur.Version != item.Version {
			return nil
		}
		next := cur
		fn(&next)
		next.TrackedAt, next.UpdatedAt = nullInt(now), now
		if _, ok, e := tx.UpdateItem(cur, next); e != nil || !ok {
			return e
		}
		if resolved {
			return tx.DropPendingNotifications(cur.ID)
		}
		return nil
	})
}

// resolve completes the item with the user's message as evidence and drops
// its pending notifications.
func (r *run) resolve(item store.ActionItem, link string) error {
	now := r.s.Now().Unix()
	return r.change(item, func(next *store.ActionItem) {
		next.Status, next.AutoReason, next.AutoRef, next.AutoAt = "done", nullString(ReasonReplyResolved), chatworkURL(link), nullInt(now)
	}, true)
}

// untrack stops tracking the item without changing its status.
func (r *run) untrack(item store.ActionItem) error {
	return r.change(item, func(next *store.ActionItem) { next.Tracking = 0 }, false)
}
