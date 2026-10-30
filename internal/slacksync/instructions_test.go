package slacksync

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/i2bskn/astovia/internal/ai"
	"github.com/i2bskn/astovia/internal/store"
)

// The user's instructions of the process follow the fixed system prompt of
// the detection and of the reply judgment.
func TestInstructionsInSystem(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		st := newSyncTest(t, driver)
		in := map[string]string{store.InstructionCommon: "共通の指示", store.InstructionSlackDetect: "Slack の検出の指示", store.InstructionSlackReply: "Slack の返信の指示", store.InstructionGitHubDetect: "GitHub の指示"}
		if err := st.s.SaveAIInstructions(context.Background(), "alice-id", "sid", in, st.now.Unix()); err != nil {
			t.Fatal(err)
		}
		key, origin := st.trackedMention()
		system := st.ai.input(t, 1)["_system"].(string)
		if !strings.HasPrefix(system, ai.SlackSystemPrompt) || !strings.HasSuffix(system, "<user_instructions>\n<common>\n共通の指示\n</common>\n<process>\nSlack の検出の指示\n</process>\n</user_instructions>") {
			t.Fatalf("detection system: %q", system)
		}

		st.advance(5 * time.Minute)
		st.reply("C1", origin, "送りました", time.Minute)
		st.ai.set(aiResolved(true))
		st.sync()
		if st.mustItem(key).Status != "done" {
			t.Fatal("not resolved")
		}
		system = st.ai.input(t, 1)["_system"].(string)
		if !strings.HasPrefix(system, ai.ReplySystemPrompt) || !strings.HasSuffix(system, "<user_instructions>\n<common>\n共通の指示\n</common>\n<process>\nSlack の返信の指示\n</process>\n</user_instructions>") {
			t.Fatalf("reply system: %q", system)
		}
	})
}
