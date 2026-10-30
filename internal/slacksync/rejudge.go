package slacksync

import (
	"strings"
	"time"

	"github.com/i2bskn/astovia/internal/ai"
	"github.com/i2bskn/astovia/internal/slack"
	"github.com/i2bskn/astovia/internal/store"
)

// The functions here build the AI input of an item made from a judgment
// again, from the messages of its conversation read again, to try the
// user's AI instructions. They use the same conversions as the
// synchronization; reading Slack is left to the caller.

// Key is the parsed source key of an item made from a judgment: a mention
// (KindMention) or a run of direct messages (KindDM).
type Key sourceKey

// Source key kinds of Key and the thread part of a key outside threads.
const (
	KindMention = kindMention
	KindDM      = kindDM
	NoThread    = noThread
)

// FallbackPrefix starts the content of the confirmation task made when the
// AI could not judge a message.
const FallbackPrefix = "Slack を確認: "

// ParseKey parses the source key of an item made from a judgment.
func ParseKey(v string) (Key, bool) {
	k, ok := parseSourceKey(v)
	return Key(k), ok
}

// JudgedPrefixes returns the starts of the source keys of the items made
// from judgments in the team: the mentions and the direct message runs.
func JudgedPrefixes(team string) []string {
	return []string{"slack:" + team + ":" + kindMention + ":", "slack:" + team + ":" + kindDM + ":"}
}

// Conversation returns the conversation type and the channel name of an
// item's conversation from its channel ID and the conversation name kept in
// its refs (see conversationName): a channel ID starting with D is a direct
// message, グループDM a group direct message, DM or DM（name） a direct
// message, and anything else a channel named after the name without its #
// (empty when it has none). The channel name of a direct message is empty.
func Conversation(channel, conversationName string) (string, string) {
	switch {
	case strings.HasPrefix(channel, "D"):
		return ai.SlackDM, ""
	case conversationName == "グループDM":
		return ai.SlackGroupDM, ""
	case conversationName == "DM" || strings.HasPrefix(conversationName, "DM（"):
		return ai.SlackDM, ""
	}
	if name, ok := strings.CutPrefix(conversationName, "#"); ok {
		return ai.SlackChannel, name
	}
	return ai.SlackChannel, ""
}

// RejudgeScope selects the related tasks of the item's conversation as the
// synchronization does, without the items of exclude.
func RejudgeScope(key Key, convType string, now time.Time, exclude []string) store.RelatedScope {
	thread := key.Thread
	if key.Kind == kindMention {
		thread = relatedThread(convType, key.Thread, key.Last)
	}
	scope := relatedScope(key.Team, key.Channel, thread, convType, now)
	scope.Exclude = exclude
	return scope
}

// RejudgeInput builds the AI input of the judgment of the item of key from
// messages, the messages of its conversation read again (in any order):
// for a mention the message of the key, and for a run the messages of the
// thread from the key's first to its last that may be detected. ok is false
// when none of them is among messages. name returns a member's display
// name.
func RejudgeInput(conn store.SlackConnection, loc *time.Location, key Key, convType, channelName string, messages []slack.Message, related []store.RelatedTask, name func(string) string) (in ai.SlackInput, ok bool, err error) {
	list := sortMessages(messages)
	targets := map[string]bool{}
	i, j := -1, -1
	for n, m := range list {
		target := m.TS == key.Last
		if key.Kind == kindDM {
			target = compareTS(m.TS, key.First) >= 0 && compareTS(m.TS, key.Last) <= 0 && threadOf(m) == key.Thread && eligible(m, conn.SlackUserID)
		}
		if !target {
			continue
		}
		targets[m.TS] = true
		if i < 0 {
			i = n
		}
		j = n
	}
	if i < 0 {
		return ai.SlackInput{}, false, nil
	}
	in, err = detectInput(conn, loc, convType, channelName, contextPosts(list, i, j), targets, related, ai.Instructions{}, nameFunc(name))
	return in, err == nil, err
}

// RejudgeReplyInput builds the AI input of the reply judgment of the item
// of key with the task from messages, the messages of its conversation read
// again (in any order): the original message (when among them) and the
// later ones, with the user's latest message after the original as the
// reply. ok is false when the user has not posted after the original.
func RejudgeReplyInput(conn store.SlackConnection, loc *time.Location, key Key, task string, messages []slack.Message, name func(string) string) (in ai.ReplyInput, ok bool, err error) {
	var origin *slack.Message
	var later []slack.Message
	for _, m := range sortMessages(messages) {
		switch {
		case m.TS == key.Last:
			if origin == nil {
				origin = &m
			}
		case compareTS(m.TS, key.Last) > 0:
			later = append(later, m)
		}
	}
	var mine *slack.Message
	for i := range later {
		if later[i].User == conn.SlackUserID {
			mine = &later[i]
		}
	}
	if mine == nil {
		return ai.ReplyInput{}, false, nil
	}
	in, err = replyInput(conn, loc, task, origin, later, *mine, ai.Instructions{}, nameFunc(name))
	return in, err == nil, err
}

// nameFunc adapts a name lookup that never fails.
func nameFunc(name func(string) string) func(string) (string, error) {
	return func(id string) (string, error) { return name(id), nil }
}
