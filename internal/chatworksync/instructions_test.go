package chatworksync

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/i2bskn/astovia/internal/ai"
	"github.com/i2bskn/astovia/internal/store"
)

// The user's instructions of the detection follow the fixed system prompt.
func TestInstructionsInSystem(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		st := newSyncTest(t, driver)
		in := map[string]string{store.InstructionCommon: "共通の指示", store.InstructionChatworkDetect: "Chatwork の検出の指示", store.InstructionSlackDetect: "Slack の指示"}
		if err := st.s.SaveAIInstructions(context.Background(), "alice-id", "sid", in, st.now.Unix()); err != nil {
			t.Fatal(err)
		}
		st.cw.room("2", "開発チーム", "group")
		st.say("2", bob, "[To:101]資料ください", time.Minute)
		st.ai.set(aiAction("資料を送る"))
		st.sync()
		if st.ai.count() != 1 {
			t.Fatalf("AI calls: %d", st.ai.count())
		}
		system := st.ai.input(t, 1)["_system"].(string)
		if !strings.HasPrefix(system, ai.ChatworkSystemPrompt) || !strings.HasSuffix(system, "<user_instructions>\n<common>\n共通の指示\n</common>\n<process>\nChatwork の検出の指示\n</process>\n</user_instructions>") {
			t.Fatalf("system: %q", system)
		}
	})
}
