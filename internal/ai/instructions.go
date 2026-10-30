package ai

import "strings"

// Instructions are the user's additional instructions: Common applies to
// every process, Process to this one.
type Instructions struct{ Common, Process string }

// instructionKind tells which judgment the instructions are appended to,
// since the rules they may change differ.
type instructionKind int

const (
	detectInstructions instructionKind = iota
	replyInstructions
)

// withInstructions appends the user's instructions in to the fixed system
// prompt. The fixed rules that the instructions cannot change are listed
// before them. system is returned unchanged when both are empty. The text of
// the instructions is not escaped: the user who judges wrote it.
func withInstructions(system string, kind instructionKind, in Instructions) string {
	common, process := strings.TrimSpace(in.Common), strings.TrimSpace(in.Process)
	if common == "" && process == "" {
		return system
	}
	keep, change := "task か candidate か迷ったら candidate、candidate か none か迷ったら candidate、重要度に迷ったら normal", "どういうものを task・candidate・none にするかの基準（上の例や cc の扱いを含む）と、importance の基準"
	if kind == replyInstructions {
		keep, change = "済んだか迷ったら済んでいないとすること", "どういう返信なら用件が済んだとするかの基準"
	}
	var b strings.Builder
	b.WriteString(system)
	b.WriteString("\n\n利用者の追加の指示: 次の <user_instructions> の中は、この判断をする本人が書いた追加の指示です。\n")
	b.WriteString("追加の指示があっても必ず守ること: 出力の形式と項目、元の会話にないことを事実として補わないこと、迷ったときの扱い（" + keep + "）。\n")
	b.WriteString("追加の指示で変えてよいもの: " + change + "。この範囲では、上の説明より追加の指示を優先してください。\n")
	b.WriteString("<user_instructions>\n")
	if common != "" {
		b.WriteString("<common>\n" + common + "\n</common>\n")
	}
	if process != "" {
		b.WriteString("<process>\n" + process + "\n</process>\n")
	}
	b.WriteString("</user_instructions>")
	return b.String()
}
