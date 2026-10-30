package slacksync

import (
	"testing"
	"time"

	"github.com/i2bskn/astovia/internal/ai"
	"github.com/i2bskn/astovia/internal/slack"
	"github.com/i2bskn/astovia/internal/store"
)

func TestConversation(t *testing.T) {
	for _, c := range []struct{ channel, name, convType, channelName string }{
		{"D1", "#general", ai.SlackDM, ""},
		{"C1", "グループDM", ai.SlackGroupDM, ""},
		{"G1", "DM", ai.SlackDM, ""},
		{"C1", "DM（Bob）", ai.SlackDM, ""},
		{"C1", "#general", ai.SlackChannel, "general"},
		{"C1", "", ai.SlackChannel, ""},
	} {
		if convType, name := Conversation(c.channel, c.name); convType != c.convType || name != c.channelName {
			t.Errorf("%s %q: %s %q", c.channel, c.name, convType, name)
		}
	}
}

// A run is judged again from the messages of its thread from its first to
// its last that may be detected; the user's own are context.
func TestRejudgeInputRun(t *testing.T) {
	conn := store.SlackConnection{SlackUserID: "U1", SlackUserName: "alice", TeamID: "T1", TeamName: "Acme"}
	key, ok := ParseKey("slack:T1:d:D1:1.000000:2.000000:4.000000")
	if !ok {
		t.Fatal("key")
	}
	msgs := []slack.Message{
		{TS: "5.000000", User: "U2", Text: "after", ThreadTS: "1.000000"},
		{TS: "4.000000", User: "U2", Text: "last", ThreadTS: "1.000000"},
		{TS: "3.000000", User: "U1", Text: "mine", ThreadTS: "1.000000"},
		{TS: "2.000000", User: "U2", Text: "first", ThreadTS: "1.000000"},
		{TS: "1.000000", User: "U2", Text: "parent", ThreadTS: "1.000000"},
	}
	name := func(id string) string { return "Name " + id }
	in, ok, err := RejudgeInput(conn, time.UTC, key, ai.SlackDM, "", msgs, nil, name)
	if err != nil || !ok || len(in.Posts) != 5 || in.ConversationType != ai.SlackDM || len(in.RelatedTasks) != 0 {
		t.Fatalf("input: %+v %v %v", in, ok, err)
	}
	var targets []string
	for _, p := range in.Posts {
		if p.Target {
			targets = append(targets, p.ID)
		}
	}
	if len(targets) != 2 || targets[0] != "2.000000" || targets[1] != "4.000000" || in.Posts[2].Author != "Name U1" || !in.Posts[2].IsMe {
		t.Fatalf("targets: %v %+v", targets, in.Posts)
	}
	if _, ok, _ = RejudgeInput(conn, time.UTC, key, ai.SlackDM, "", msgs[:1], nil, name); ok {
		t.Fatal("found without the run")
	}
	// The related tasks of a mention outside threads in a channel are those
	// of the thread of the message, without the excluded items.
	mention, _ := ParseKey("slack:T1:m:C1:-:7.000000")
	scope := RejudgeScope(mention, ai.SlackChannel, time.Unix(100000, 0), []string{"item"})
	if scope.Prefixes[0] != "slack:T1:m:C1:7.000000:" || len(scope.ExactKeys) != 1 || scope.ExactKeys[0] != "slack:T1:m:C1:-:7.000000" || len(scope.Exclude) != 1 {
		t.Fatalf("scope: %+v", scope)
	}
}
