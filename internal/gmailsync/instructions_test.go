package gmailsync

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
		in := map[string]string{store.InstructionCommon: "共通の指示", store.InstructionGmailDetect: "メールの検出の指示", store.InstructionGmailReply: "メールの返信の指示"}
		if err := st.s.SaveAIInstructions(context.Background(), "alice-id", "sid", in, st.now.Unix()); err != nil {
			t.Fatal(err)
		}
		st.recv("", "見積もりの件", "見積もりを送ってください。", time.Minute)
		st.ai.set(aiAction("見積もりを送る"))
		st.sync(me)
		if st.ai.count() != 1 {
			t.Fatalf("AI calls: %d", st.ai.count())
		}
		system := st.ai.input(t, 1)["_system"].(string)
		if !strings.HasPrefix(system, ai.MailSystemPrompt) || !strings.HasSuffix(system, "<user_instructions>\n<common>\n共通の指示\n</common>\n<process>\nメールの検出の指示\n</process>\n</user_instructions>") {
			t.Fatalf("system: %q", system)
		}
	})
}
