package gmailsync

import (
	"time"

	"github.com/i2bskn/astovia/internal/ai"
	"github.com/i2bskn/astovia/internal/google"
	"github.com/i2bskn/astovia/internal/store"
)

// The functions here build the AI input of an item made from a judgment
// again, from its thread read again, to try the user's AI instructions.
// They use the same conversions as the synchronization; reading Gmail is
// left to the caller.

// Key is the parsed source key of an item made from a judgment of a run of
// messages of a thread.
type Key sourceKey

// FallbackPrefix starts the content of the confirmation task made when the
// AI could not judge a run.
const FallbackPrefix = "メールを確認: "

// ParseKey parses the source key of an item made from a judgment.
func ParseKey(v string) (Key, bool) {
	k, ok := parseSourceKey(v)
	return Key(k), ok
}

// JudgedPrefix returns the start of the source keys of the items made from
// judgments in the account of the lower-case address email.
func JudgedPrefix(email string) string { return accountPrefix(email) + kindRun + ":" }

// RejudgeScope selects the related tasks of the item's thread as the
// synchronization does, without the items of exclude.
func RejudgeScope(key Key, now time.Time, exclude []string) store.RelatedScope {
	scope := relatedScope(key.Email, key.Thread, now)
	scope.Exclude = exclude
	return scope
}

// RejudgeInput builds the AI input of the judgment of the item of key from
// its thread read again: the visible messages from the key's first to its
// last that may be judged. ok is false when none of them is in the thread.
func RejudgeInput(email string, loc *time.Location, key Key, t google.Thread, related []store.RelatedTask) (ai.SlackInput, bool) {
	msgs := visible(t)
	first, last := indexID(msgs, key.First), indexID(msgs, key.Last)
	if first < 0 || last < 0 || first > last {
		return ai.SlackInput{}, false
	}
	targets := map[string]bool{}
	i, j := -1, -1
	for n := first; n <= last; n++ {
		if excluded(msgs[n]) {
			continue
		}
		targets[msgs[n].ID] = true
		if i < 0 {
			i = n
		}
		j = n
	}
	if i < 0 {
		return ai.SlackInput{}, false
	}
	subject := subjectOf(msgs[j], msgs)
	return detectInput(email, loc, myName(msgs, email), subject, contextPosts(msgs, i, j), targets, related, ai.Instructions{}), true
}

// RejudgeReplyInput builds the AI input of the reply judgment of the item
// of key with the task from its thread read again: the original message
// (when in the thread) and the later visible messages, with the user's
// latest message after the original as the reply. ok is false when the user
// has not sent anything after the original.
func RejudgeReplyInput(email string, loc *time.Location, key Key, task string, t google.Thread) (ai.ReplyInput, bool) {
	msgs := visible(t)
	origin := indexID(msgs, key.Last)
	var originMsg *google.Message
	if origin >= 0 {
		originMsg = &msgs[origin]
	}
	later := msgs[origin+1:]
	var mine *google.Message
	for i := range later {
		if later[i].HasLabel(labelSent) {
			mine = &later[i]
		}
	}
	if mine == nil {
		return ai.ReplyInput{}, false
	}
	return replyInput(email, loc, task, msgs, originMsg, later, *mine, ai.Instructions{}), true
}
