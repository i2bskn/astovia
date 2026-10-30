package slacksync

import (
	"errors"
	"time"

	"github.com/i2bskn/astovia/internal/ai"
	"github.com/i2bskn/astovia/internal/slack"
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
	// trackGrace allows for messages posted just before the last check.
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
// first: when the user posted in the conversation since the last check, the
// AI judges whether that completed the item. The items to check and their
// conversation come from the source key only.
func (r *run) track() error {
	items, err := r.s.Store.TrackedActionItems(r.parent, r.conn.UserID, store.SourceSlack, trackBatch)
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

// trackItem checks one item.
func (r *run) trackItem(item store.ActionItem, passStart int64) (passState, error) {
	key, ok := parseSourceKey(item.SourceKey.String)
	if !ok || key.Team != r.conn.TeamID {
		return passNext, r.untrack(item)
	}
	origin := key.Last
	checked := tsSeconds(origin)
	if item.TrackedAt.Valid {
		checked = item.TrackedAt.Int64
	}
	lower := origin
	if v := secondsTS(checked - trackGrace); compareTS(v, lower) > 0 {
		lower = v
	}
	// fail handles an error of a read about the item: the budget ends the
	// pass, a Slack error code about the conversation stops tracking the
	// item, and the rest stops the synchronization.
	fail := func(err error) (passState, error) {
		switch {
		case errors.Is(err, errBudget):
			return passEnd, nil
		case stops(err):
			return passAbort, err
		}
		return passNext, r.untrack(item)
	}
	var originMsg *slack.Message
	var later []slack.Message
	keep := func(list []slack.Message) {
		for _, m := range list {
			switch {
			case m.TS == origin:
				if originMsg == nil {
					originMsg = &m
				}
			case validTS(m.TS) && compareTS(m.TS, origin) > 0 && compareTS(m.TS, lower) >= 0:
				later = append(later, m)
			}
		}
	}
	if key.Thread != noThread {
		p := slack.HistoryParams{Channel: key.Channel, TS: key.Thread, Oldest: lower, Limit: historyLimit}
		for page := 0; page < historyPages; page++ {
			res, err := r.replies(p)
			if err != nil {
				return fail(err)
			}
			keep(res.Messages)
			if !res.HasMore || res.NextCursor == "" {
				break
			}
			p.Cursor = res.NextCursor
		}
	} else {
		res, err := r.history(slack.HistoryParams{Channel: key.Channel, Latest: origin, Oldest: origin, Inclusive: true, Limit: 1})
		if err != nil {
			return fail(err)
		}
		keep(res.Messages)
		if originMsg == nil {
			// The original message is gone or no longer visible.
			return passNext, r.untrack(item)
		}
		// Only the newest page is read: the user's latest reply is what
		// matters.
		if res, err = r.history(slack.HistoryParams{Channel: key.Channel, Oldest: lower, Limit: historyLimit}); err != nil {
			return fail(err)
		}
		keep(res.Messages)
		if validTS(originMsg.LatestReply) && compareTS(originMsg.LatestReply, secondsTS(checked)) > 0 {
			if res, err = r.replies(slack.HistoryParams{Channel: key.Channel, TS: origin, Oldest: lower, Limit: historyLimit}); err != nil {
				return fail(err)
			}
			keep(res.Messages)
		}
	}
	later = sortMessages(later)
	var mine *slack.Message
	for i := range later {
		if later[i].User == r.conn.SlackUserID && tsSeconds(later[i].TS) >= checked {
			mine = &later[i]
		}
	}
	if mine == nil {
		return passNext, r.touch(item, passStart)
	}
	return r.judgeReply(item, key, originMsg, later, *mine, passStart)
}

// judgeReply asks the AI whether the user's replies completed the item.
func (r *run) judgeReply(item store.ActionItem, key sourceKey, originMsg *slack.Message, later []slack.Message, mine slack.Message, passStart int64) (passState, error) {
	switch {
	case r.aiStopped || r.aiFallbackOnly:
		return passAbort, nil
	case r.aiStale:
		return passNext, nil
	case r.trackCalls >= trackAICalls || r.deadlineNear():
		return passEnd, nil
	}
	cur, cred, gate, err := r.reloadAI()
	switch {
	case err != nil:
		return passAbort, err
	case gate == gateStale:
		return passNext, nil
	case gate != gateOK:
		return passAbort, nil
	}
	loc, err := r.location()
	if err != nil {
		return passAbort, err
	}
	instructions := ai.Instructions{Common: r.instructions[store.InstructionCommon], Process: r.instructions[store.InstructionSlackReply]}
	in, err := replyInput(r.conn, loc, item.Content, originMsg, later, mine, instructions, r.userName)
	if err != nil {
		return passAbort, err
	}
	r.trackCalls++
	res, err := r.s.AI.JudgeReply(r.ctx, cred, cur.Model, in)
	class, err := r.classifyAI(cur, err)
	if err != nil {
		return passAbort, err
	}
	switch class {
	case aiOK:
		if !res.Resolved {
			return passNext, r.touch(item, passStart)
		}
		link, err := r.permalink(key.Channel, mine)
		if err != nil {
			return passAbort, err
		}
		return passNext, r.resolve(item, link)
	case aiUnusable:
		return passNext, r.touch(item, passStart)
	case aiFailed:
		return passAbort, nil
	case aiStaled:
		return passNext, nil
	}
	// A temporary failure: the item is checked again by the next pass.
	return passNext, nil
}

// replyInput builds the AI input of a reply judgment of the task: the
// original message (when known) and the newest replyPosts later messages,
// with the user's latest reply mine in place of the oldest when it is older
// than all of them. later is in time order.
func replyInput(conn store.SlackConnection, loc *time.Location, task string, originMsg *slack.Message, later []slack.Message, mine slack.Message, instructions ai.Instructions, name func(string) (string, error)) (ai.ReplyInput, error) {
	tail := later
	if len(tail) > replyPosts {
		tail = append([]slack.Message{}, tail[len(tail)-replyPosts:]...)
		if indexTS(tail, mine.TS) < 0 {
			tail[0] = mine
		}
	}
	var msgs []slack.Message
	if originMsg != nil {
		msgs = append(msgs, *originMsg)
	}
	msgs = append(msgs, tail...)
	in := ai.ReplyInput{Me: ai.SlackPerson{ID: conn.SlackUserID, Name: conn.SlackUserName}, Location: loc, Task: task, Instructions: instructions}
	for _, m := range msgs {
		p, err := toPost(m, false, conn.SlackUserID, name)
		if err != nil {
			return ai.ReplyInput{}, err
		}
		in.Posts = append(in.Posts, p)
	}
	return in, nil
}

// touch records that the item was checked at the start of the pass. Nothing
// is written when the item changed since it was read.
func (r *run) touch(item store.ActionItem, at int64) error {
	return r.s.Store.SlackSync(r.parent, r.conn.UserID, r.conn.Generation, func(tx *store.SlackTx) error {
		return tx.TouchItem(item, at)
	})
}

// change applies fn to the item after locking it, when it is still the
// open (or candidate), tracked Slack item of the version read.
func (r *run) change(item store.ActionItem, fn func(next *store.ActionItem), resolved bool) error {
	now := r.s.Now().Unix()
	return r.s.Store.SlackSync(r.parent, r.conn.UserID, r.conn.Generation, func(tx *store.SlackTx) error {
		cur, found, e := tx.LockItem(item.ID)
		if e != nil || !found {
			return e
		}
		if (cur.Status != "open" && cur.Status != "candidate") || cur.Tracking != 1 || cur.Source.String != store.SourceSlack || cur.Version != item.Version {
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

// resolve completes the item with the user's reply as evidence and drops
// its pending notifications.
func (r *run) resolve(item store.ActionItem, link string) error {
	now := r.s.Now().Unix()
	return r.change(item, func(next *store.ActionItem) {
		next.Status, next.AutoReason, next.AutoRef, next.AutoAt = "done", nullString(ReasonReplyResolved), slackURL(link), nullInt(now)
	}, true)
}

// untrack stops tracking the item without changing its status.
func (r *run) untrack(item store.ActionItem) error {
	return r.change(item, func(next *store.ActionItem) { next.Tracking = 0 }, false)
}
