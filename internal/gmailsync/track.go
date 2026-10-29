package gmailsync

import (
	"errors"

	"github.com/i2bskn/astovia/internal/ai"
	"github.com/i2bskn/astovia/internal/google"
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

// track checks up to trackItems tracked open items of this account, least
// recently checked first: when the user sent a message in the thread since
// the last check, the AI judges whether that completed the item. The items
// to check and their thread come from the source key only.
func (r *run) track() error {
	items, err := r.s.Store.TrackedSourcePrefixItems(r.parent, r.acct.UserID, store.SourceGmail, accountPrefix(r.acct.Email), trackBatch)
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

// trackItem checks one item: the messages of its thread after the item's
// last message, from a little before the last check, are the later
// messages; drafts are not among them.
func (r *run) trackItem(item store.ActionItem, passStart int64) (passState, error) {
	key, ok := parseSourceKey(item.SourceKey.String)
	if !ok || key.Email != r.acct.Email {
		return passNext, r.untrack(item)
	}
	checked := item.CreatedAt
	switch {
	case item.TrackedAt.Valid:
		checked = item.TrackedAt.Int64
	case item.DetectedAt.Valid:
		checked = item.DetectedAt.Int64
	}
	t, err := r.thread(key.Thread)
	switch {
	case errors.Is(err, errBudget):
		return passEnd, nil
	case err != nil && stops(err):
		return passAbort, err
	case err != nil:
		return passNext, r.untrack(item)
	}
	msgs := visible(t)
	origin := indexID(msgs, key.Last)
	var originMsg *google.Message
	if origin >= 0 {
		originMsg = &msgs[origin]
	}
	var later []google.Message
	for i, m := range msgs {
		if i > origin && m.Internal.Unix() >= checked-trackGrace {
			later = append(later, m)
		}
	}
	var mine *google.Message
	for i := range later {
		if later[i].HasLabel(labelSent) && later[i].Internal.Unix() >= checked {
			mine = &later[i]
		}
	}
	if mine == nil {
		return passNext, r.touch(item, passStart)
	}
	return r.judgeReply(item, key, msgs, originMsg, later, *mine, passStart)
}

// judgeReply asks the AI whether the user's messages completed the item.
func (r *run) judgeReply(item store.ActionItem, key sourceKey, msgs []google.Message, originMsg *google.Message, later []google.Message, mine google.Message, passStart int64) (passState, error) {
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
	// The newest replyPosts later messages, with the user's latest message
	// in place of the oldest when it is older than all of them.
	tail := later
	if len(tail) > replyPosts {
		tail = append([]google.Message{}, tail[len(tail)-replyPosts:]...)
		if indexID(tail, mine.ID) < 0 {
			tail[0] = mine
		}
	}
	var posts []google.Message
	if originMsg != nil {
		posts = append(posts, *originMsg)
	}
	posts = append(posts, tail...)
	loc, err := r.location()
	if err != nil {
		return passAbort, err
	}
	in := ai.ReplyInput{Me: ai.SlackPerson{ID: r.acct.Email, Name: myName(msgs, r.acct.Email)}, Location: loc, Task: item.Content}
	for _, m := range posts {
		in.Posts = append(in.Posts, r.post(m, false))
	}
	r.trackCalls++
	res, err := r.s.AI.JudgeMailReply(r.ctx, cred, cur.Model, in)
	class, err := r.classifyAI(cur, err)
	if err != nil {
		return passAbort, err
	}
	switch class {
	case aiOK:
		if !res.Resolved {
			return passNext, r.touch(item, passStart)
		}
		return passNext, r.resolve(item, threadURL(r.acct.Email, key.Thread))
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

// touch records that the item was checked at the start of the pass. Nothing
// is written when the item changed since it was read.
func (r *run) touch(item store.ActionItem, at int64) error {
	return r.s.Store.GmailSync(r.parent, r.acct.UserID, r.acct.ID, r.acct.Generation, func(tx *store.GmailTx) error {
		return tx.TouchItem(item, at)
	})
}

// change applies fn to the item after locking it, when it is still the
// open (or candidate), tracked mail item of the version read.
func (r *run) change(item store.ActionItem, fn func(next *store.ActionItem), resolved bool) error {
	now := r.s.Now().Unix()
	return r.s.Store.GmailSync(r.parent, r.acct.UserID, r.acct.ID, r.acct.Generation, func(tx *store.GmailTx) error {
		cur, found, e := tx.LockItem(item.ID)
		if e != nil || !found {
			return e
		}
		if (cur.Status != "open" && cur.Status != "candidate") || cur.Tracking != 1 || cur.Source.String != store.SourceGmail || cur.Version != item.Version {
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

// resolve completes the item with the thread as evidence and drops its
// pending notifications.
func (r *run) resolve(item store.ActionItem, link string) error {
	now := r.s.Now().Unix()
	return r.change(item, func(next *store.ActionItem) {
		next.Status, next.AutoReason, next.AutoRef, next.AutoAt = "done", nullString(ReasonReplyResolved), gmailURL(link), nullInt(now)
	}, true)
}

// untrack stops tracking the item without changing its status.
func (r *run) untrack(item store.ActionItem) error {
	return r.change(item, func(next *store.ActionItem) { next.Tracking = 0 }, false)
}
