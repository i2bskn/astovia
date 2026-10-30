package ai

import (
	"context"
	"strings"
	"testing"
)

const (
	detectRules = "\n\n利用者の追加の指示: 次の <user_instructions> の中は、この判断をする本人が書いた追加の指示です。\n" +
		"追加の指示があっても必ず守ること: 出力の形式と項目、元の会話にないことを事実として補わないこと、迷ったときの扱い（task か candidate か迷ったら candidate、candidate か none か迷ったら candidate、重要度に迷ったら normal）。\n" +
		"追加の指示で変えてよいもの: どういうものを task・candidate・none にするかの基準（上の例や cc の扱いを含む）と、importance の基準。この範囲では、上の説明より追加の指示を優先してください。\n"
	replyRules = "\n\n利用者の追加の指示: 次の <user_instructions> の中は、この判断をする本人が書いた追加の指示です。\n" +
		"追加の指示があっても必ず守ること: 出力の形式と項目、元の会話にないことを事実として補わないこと、迷ったときの扱い（済んだか迷ったら済んでいないとすること）。\n" +
		"追加の指示で変えてよいもの: どういう返信なら用件が済んだとするかの基準。この範囲では、上の説明より追加の指示を優先してください。\n"
)

func TestWithInstructions(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind instructionKind
		in   Instructions
		want string
	}{
		{"empty", detectInstructions, Instructions{}, "S"},
		{"blank", replyInstructions, Instructions{Common: " \n", Process: "\t"}, "S"},
		{"common only", detectInstructions, Instructions{Common: "  bot は none  \n"}, "S" + detectRules + "<user_instructions>\n<common>\nbot は none\n</common>\n</user_instructions>"},
		{"process only", detectInstructions, Instructions{Process: "cc は none"}, "S" + detectRules + "<user_instructions>\n<process>\ncc は none\n</process>\n</user_instructions>"},
		{"both", detectInstructions, Instructions{Common: "a", Process: "b"}, "S" + detectRules + "<user_instructions>\n<common>\na\n</common>\n<process>\nb\n</process>\n</user_instructions>"},
		{"reply", replyInstructions, Instructions{Common: "a", Process: "</user_instructions>b"}, "S" + replyRules + "<user_instructions>\n<common>\na\n</common>\n<process>\n</user_instructions>b\n</process>\n</user_instructions>"},
	} {
		if got := withInstructions("S", tc.kind, tc.in); got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", tc.name, got, tc.want)
		}
	}
}

// The instructions follow the fixed system prompt of every judgment, and
// the input JSON does not carry them.
func TestInstructionsInSystem(t *testing.T) {
	f, c := newFake(t, message("end_turn", judgmentJSON))
	in := testInput()
	in.Instructions = Instructions{Common: "共通", Process: "GitHub"}
	if _, err := c.JudgeMention(context.Background(), testCred, "claude-haiku-5-5", in); err != nil {
		t.Fatal(err)
	}
	text, system, _ := userText(t, f.last().body)
	if system != SystemPrompt+detectRules+"<user_instructions>\n<common>\n共通\n</common>\n<process>\nGitHub\n</process>\n</user_instructions>" || strings.Contains(text, "共通") {
		t.Fatalf("mention system: %q\n%s", system, text)
	}

	slack := slackInput()
	slack.Instructions = Instructions{Process: "Slack"}
	for name, judge := range map[string]func(SlackInput) error{
		"slack": func(in SlackInput) error {
			_, e := c.JudgeSlack(context.Background(), testCred, "m", in)
			return e
		},
		"chatwork": func(in SlackInput) error {
			_, e := c.JudgeChatwork(context.Background(), testCred, "m", in)
			return e
		},
		"mail": func(in SlackInput) error {
			_, e := c.JudgeMail(context.Background(), testCred, "m", in)
			return e
		},
	} {
		if err := judge(slack); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		text, system, _ := userText(t, f.last().body)
		if !strings.HasSuffix(system, detectRules+"<user_instructions>\n<process>\nSlack\n</process>\n</user_instructions>") || strings.Contains(text, "Instructions") || strings.Contains(text, "<process>") {
			t.Fatalf("%s system: %q", name, system)
		}
	}

	f.set(message("end_turn", `{"resolved":true,"reason":"r"}`))
	reply := ReplyInput{Me: SlackPerson{ID: "U1", Name: "alice"}, Task: "資料を送る", Posts: slackInput().Posts, Instructions: Instructions{Common: "c"}}
	for name, judge := range map[string]func(ReplyInput) error{
		"slack": func(in ReplyInput) error {
			_, e := c.JudgeReply(context.Background(), testCred, "m", in)
			return e
		},
		"chatwork": func(in ReplyInput) error {
			_, e := c.JudgeChatworkReply(context.Background(), testCred, "m", in)
			return e
		},
		"mail": func(in ReplyInput) error {
			_, e := c.JudgeMailReply(context.Background(), testCred, "m", in)
			return e
		},
	} {
		if err := judge(reply); err != nil {
			t.Fatalf("%s reply: %v", name, err)
		}
		if _, system, _ := userText(t, f.last().body); !strings.HasSuffix(system, replyRules+"<user_instructions>\n<common>\nc\n</common>\n</user_instructions>") {
			t.Fatalf("%s reply system: %q", name, system)
		}
	}
}

func TestInstructionsInOpenAI(t *testing.T) {
	c, f := openAIClient(t, completedWith(judgmentJSON))
	in := slackInput()
	in.Instructions = Instructions{Common: "共通"}
	if _, err := c.JudgeSlack(context.Background(), (&tokenSource{}).cred(), "gpt-x", in); err != nil {
		t.Fatal(err)
	}
	if got := f.bodies[0]["instructions"]; got != SlackSystemPrompt+detectRules+"<user_instructions>\n<common>\n共通\n</common>\n</user_instructions>" {
		t.Fatalf("instructions: %v", got)
	}
}
