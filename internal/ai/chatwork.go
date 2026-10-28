package ai

import "context"

// ChatworkSystemPrompt is the fixed instruction of a Chatwork judgment. The
// rules are those of SlackSystemPrompt; only the service, the conversation
// types and the Chatwork notations (@all, 返信) differ.
var ChatworkSystemPrompt = judgmentPrompt("Chatwork", `"conversation_type" は "channel"（グループチャットでの自分宛ての発言）、"dm"（me との 1 対 1 のダイレクトチャット）のいずれかです。`,
	"本文の @all は全員宛て（TO ALL）で、me も宛先に含みます。全員への連絡や周知だけなら none、me も対応が必要な依頼なら candidate か task にしてください。\"返信 @名前(ID)\" で始まる本文は、その人の投稿への返信です。")

// ChatworkReplySystemPrompt is the fixed instruction of a Chatwork reply
// judgment.
var ChatworkReplySystemPrompt = replyPrompt("Chatwork")

// JudgeChatwork asks the AI whether the user has something to do about the
// target posts of a Chatwork room. The input is that of a Slack judgment:
// ChannelName is the room name, Workspace the organization name and
// ConversationType SlackChannel (group chat) or SlackDM (direct chat).
func (c *Client) JudgeChatwork(ctx context.Context, key, model string, in SlackInput) (MentionResult, error) {
	user, err := judgmentUserMessage(in, "次の JSON は Chatwork の会話です。あなたは JSON の me の代わりに判断します。\n")
	if err != nil {
		return MentionResult{}, err
	}
	return c.judgeConversation(ctx, key, model, ChatworkSystemPrompt, user, in)
}

// JudgeChatworkReply asks the AI whether the user's replies in a Chatwork
// room completed the task.
func (c *Client) JudgeChatworkReply(ctx context.Context, key, model string, in ReplyInput) (ReplyResult, error) {
	user, err := replyUserMessage(in, "次の JSON は Chatwork のタスクと、その後の会話です。あなたは JSON の me の代わりに判断します。\n")
	if err != nil {
		return ReplyResult{}, err
	}
	return c.judgeReply(ctx, key, model, ChatworkReplySystemPrompt, user)
}
