package githubsync

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/i2bskn/astovia/internal/ai"
	"github.com/i2bskn/astovia/internal/store"
)

// system returns the system prompt of the n-th request.
func (f *fakeAI) system(t *testing.T, n int) string {
	t.Helper()
	var body struct {
		System []struct {
			Text string `json:"text"`
		} `json:"system"`
	}
	if err := json.Unmarshal([]byte(f.body(n)), &body); err != nil || len(body.System) != 1 {
		t.Fatalf("system: %v %s", err, f.body(n))
	}
	return body.System[0].Text
}

// The user's instructions of the mention judgment follow the fixed system
// prompt.
func TestMentionInstructions(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		st, fa := newMentionTest(t, driver)
		in := map[string]string{store.InstructionCommon: "共通の指示", store.InstructionGitHubDetect: "GitHub の検出の指示", store.InstructionSlackDetect: "Slack の指示"}
		if err := st.s.SaveAIInstructions(context.Background(), "alice-id", "sid", in, st.now.Unix()); err != nil {
			t.Fatal(err)
		}
		st.issueWithComments(comment(11, "bob", "@octocat can you check this?", st.now.Add(-30*time.Minute)))
		fa.set(func(f *fakeAI) {
			f.replies = []func(http.ResponseWriter){aiOK(true, "確認して回答する", "s")}
		})
		st.advance(time.Minute)
		st.sync()
		if fa.count() != 1 {
			t.Fatalf("AI calls: %d", fa.count())
		}
		system := fa.system(t, 1)
		if !strings.HasPrefix(system, ai.SystemPrompt) || !strings.HasSuffix(system, "<user_instructions>\n<common>\n共通の指示\n</common>\n<process>\nGitHub の検出の指示\n</process>\n</user_instructions>") {
			t.Fatalf("system: %q", system)
		}
	})
}
