package chatworksync

import (
	"time"

	"github.com/i2bskn/astovia/internal/ai"
	"github.com/i2bskn/astovia/internal/chatwork"
	"github.com/i2bskn/astovia/internal/store"
)

// The functions here build the AI input of an item made from a judgment
// again, from the latest messages of its room read again, to try the user's
// AI instructions. They use the same conversions as the synchronization;
// reading Chatwork is left to the caller.

// Key is the parsed source key of an item made from a judgment: a mention
// (KindMention) or a run of direct chat messages (KindDM).
type Key sourceKey

// Source key kinds of Key.
const (
	KindMention = kindMention
	KindDM      = kindDM
)

// FallbackPrefix starts the content of the confirmation task made when the
// AI could not judge a message.
const FallbackPrefix = "Chatwork を確認: "

// ParseKey parses the source key of an item made from a judgment.
func ParseKey(v string) (Key, bool) {
	k, ok := parseSourceKey(v)
	return Key(k), ok
}

// JudgedPrefixes returns the starts of the source keys of the items made
// from judgments for the account: the mentions and the direct chat runs.
func JudgedPrefixes(account string) []string {
	return []string{"chatwork:" + account + ":" + kindMention + ":", "chatwork:" + account + ":" + kindDM + ":"}
}

// RejudgeScope selects the related tasks of the item's room as the
// synchronization does, without the items of exclude.
func RejudgeScope(key Key, now time.Time, exclude []string) store.RelatedScope {
	scope := relatedScope(key.Account, key.Room, now)
	scope.Exclude = exclude
	return scope
}

// RejudgeInput builds the AI input of the judgment of the item of key from
// messages, the latest messages of its room read again: for a mention the
// message of the key, and for a run the messages from the key's first to
// its last that may be detected. ok is false when none of them is among
// messages.
func RejudgeInput(conn store.ChatworkConnection, loc *time.Location, key Key, room chatwork.Room, messages []chatwork.Message, related []store.RelatedTask) (ai.SlackInput, bool) {
	list := sortMessages(messages)
	convType := mentionConversation(room.Type)
	if key.Kind == kindDM {
		convType = ai.SlackDM
	}
	targets := map[string]bool{}
	i, j := -1, -1
	for n, m := range list {
		target := m.ID == key.Last
		if key.Kind == kindDM {
			target = chatwork.CompareIDs(m.ID, key.First) >= 0 && chatwork.CompareIDs(m.ID, key.Last) <= 0 && detectable(m, conn)
		}
		if !target {
			continue
		}
		targets[m.ID] = true
		if i < 0 {
			i = n
		}
		j = n
	}
	if i < 0 {
		return ai.SlackInput{}, false
	}
	names := roomNames(senderNames(list), conn)
	return detectInput(conn, loc, convType, room, contextPosts(list, i, j), targets, names, related, ai.Instructions{}), true
}

// RejudgeReplyInput builds the AI input of the reply judgment of the item
// of key with the task from messages, the latest messages of its room read
// again: the original message (when among them) and the later ones, with
// the user's latest message after the original as the reply. ok is false
// when the user has not written after the original.
func RejudgeReplyInput(conn store.ChatworkConnection, loc *time.Location, key Key, task string, messages []chatwork.Message) (ai.ReplyInput, bool) {
	list := sortMessages(messages)
	var origin *chatwork.Message
	var later []chatwork.Message
	for i, m := range list {
		switch {
		case m.ID == key.Last:
			origin = &list[i]
		case chatwork.CompareIDs(m.ID, key.Last) > 0:
			later = append(later, m)
		}
	}
	var mine *chatwork.Message
	for i := range later {
		if later[i].AccountID == conn.AccountID {
			mine = &later[i]
		}
	}
	if mine == nil {
		return ai.ReplyInput{}, false
	}
	return replyInput(conn, loc, task, origin, later, *mine, roomNames(senderNames(list), conn), ai.Instructions{}), true
}
