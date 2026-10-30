package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/i2bskn/astovia/internal/ai"
	"github.com/i2bskn/astovia/internal/aicred"
	"github.com/i2bskn/astovia/internal/chatwork"
	"github.com/i2bskn/astovia/internal/chatworksync"
	"github.com/i2bskn/astovia/internal/github"
	"github.com/i2bskn/astovia/internal/githubsync"
	"github.com/i2bskn/astovia/internal/gmailsync"
	"github.com/i2bskn/astovia/internal/google"
	"github.com/i2bskn/astovia/internal/slack"
	"github.com/i2bskn/astovia/internal/slacksync"
	"github.com/i2bskn/astovia/internal/store"
)

const (
	// trySampleLimit is the number of items listed by the samples; a reply
	// scope lists up to half of them completed by a reply and half still
	// tracked.
	trySampleLimit = 10
	// tryTimeout bounds a try: the reads, the integration and the two AI
	// calls; tryReadTimeout bounds each call to the integration, as the
	// MCP tools do. The write deadline is extended as for the ChatGPT
	// plan handlers (openAIWriteDeadline).
	tryTimeout     = 55 * time.Second
	tryReadTimeout = 12 * time.Second
	// trySlackThreadCalls, trySlackChannelCalls and trySlackReplyCalls are
	// the most conversations.history and conversations.replies calls of a
	// Slack try, taken from slackHistoryLimiter at once.
	trySlackThreadCalls  = 2
	trySlackChannelCalls = 3
	trySlackReplyCalls   = 3
	// trySlackPages is the number of pages of a thread read, as the
	// synchronization does.
	trySlackPages = 2
	// tryRetryAfter is the Retry-After of a limit without a time of its
	// own, in seconds.
	tryRetryAfter = 60
)

// tryScope is a process of an integration the instructions of a scope
// apply to. reason is the automatic completion reason of a reply judgment.
type tryScope struct {
	source string
	reply  bool
	reason string
}

// tryScopes are the scopes that can be tried: every scope but the common
// one.
var tryScopes = map[string]tryScope{
	store.InstructionGitHubDetect:   {source: store.SourceGitHub},
	store.InstructionSlackDetect:    {source: store.SourceSlack},
	store.InstructionChatworkDetect: {source: store.SourceChatwork},
	store.InstructionGmailDetect:    {source: store.SourceGmail},
	store.InstructionSlackReply:     {source: store.SourceSlack, reply: true, reason: slacksync.ReasonReplyResolved},
	store.InstructionChatworkReply:  {source: store.SourceChatwork, reply: true, reason: chatworksync.ReasonReplyResolved},
	store.InstructionGmailReply:     {source: store.SourceGmail, reply: true, reason: gmailsync.ReasonReplyResolved},
}

// tryError is the failure of a try. retryAfter is the Retry-After of a 429
// in seconds.
type tryError struct {
	status        int
	code, message string
	retryAfter    int
}

var (
	errTryNotFound      = &tryError{status: 404, code: "not_found", message: "試せる判定が見つかりません。"}
	errTryNotConnected  = &tryError{status: 409, code: "not_connected", message: "連携が接続されていないか、使えなくなっています。"}
	errTryUnavailable   = &tryError{status: 409, code: "source_unavailable", message: "元の会話を読み直せませんでした（削除された、または古くて取得できない可能性があります）。"}
	errTryNoReply       = &tryError{status: 409, code: "no_reply", message: "自分の返信がまだないため、試せません。"}
	errTryUpstream      = &tryError{status: 502, code: "upstream_error", message: "連携先から読み直せませんでした。時間をおいてお試しください。"}
	errTryRateLimited   = &tryError{status: 429, code: "rate_limited", message: "試す回数が多すぎます。1分ほどおいてお試しください。", retryAfter: tryRetryAfter}
	errTryStore         = &tryError{status: 503, code: "unavailable", message: "The service is temporarily unavailable. Try again."}
	errTryTimeout       = &tryError{status: 504, code: "timeout", message: "時間内に判定できませんでした。"}
	errTryAINotReady    = &tryError{status: 409, code: "ai_not_ready", message: "AIの設定が有効ではありません。"}
	errTryAICoolingDown = &tryError{status: 409, code: "ai_cooling_down", message: "AIの利用上限に達したため、しばらく判定を止めています。時間をおいてお試しください。"}
)

// rateLimited is a 429 of an integration, with its Retry-After when it
// gave a time.
func rateLimited(at time.Time, now time.Time) *tryError {
	e := *errTryRateLimited
	e.message = "連携先の利用回数の上限に達しました。時間をおいてお試しください。"
	if !at.IsZero() {
		e.retryAfter = max(1, int(math.Ceil(at.Sub(now).Seconds())))
	}
	return &e
}

// failTry writes e, with Retry-After on a 429.
func failTry(w http.ResponseWriter, e *tryError) {
	if e.status == 429 && e.retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(e.retryAfter))
	}
	fail(w, e.status, e.code, e.message)
}

// trySampleResponse is an item that can be tried.
type trySampleResponse struct {
	ID               string  `json:"id"`
	Content          string  `json:"content"`
	Status           string  `json:"status"`
	AutoReason       *string `json:"auto_reason"`
	ConversationName *string `json:"conversation_name"`
	DetectedAt       *string `json:"detected_at"`
}

type trySamplesResponse struct {
	Samples []trySampleResponse `json:"samples"`
}

// tryConnection is the user's current connection of a scope's integration:
// the starts of the source keys of its judged items and the start of the
// content of its confirmation tasks.
type tryConnection struct {
	prefixes []string
	fallback string
	github   store.GitHubConnection
	slack    store.SlackConnection
	chatwork store.ChatworkConnection
	gmail    []store.GmailAccount
}

// tryConnection reads the user's connection of the scope's integration.
// Without one it is errTryNotConnected; for Gmail, without any account.
func (s *Server) tryConnection(ctx context.Context, userID string, sc tryScope) (tryConnection, *tryError) {
	var out tryConnection
	var err error
	switch sc.source {
	case store.SourceGitHub:
		out.github, err = s.auth.Store.GitHubConnection(ctx, userID)
		out.prefixes, out.fallback = []string{githubsync.MentionPrefix}, githubsync.FallbackPrefix
	case store.SourceSlack:
		out.slack, err = s.auth.Store.SlackConnection(ctx, userID)
		out.prefixes, out.fallback = slacksync.JudgedPrefixes(out.slack.TeamID), slacksync.FallbackPrefix
	case store.SourceChatwork:
		out.chatwork, err = s.auth.Store.ChatworkConnection(ctx, userID)
		out.prefixes, out.fallback = chatworksync.JudgedPrefixes(out.chatwork.AccountID), chatworksync.FallbackPrefix
	case store.SourceGmail:
		out.gmail, err = s.auth.Store.GmailAccounts(ctx, userID)
		if err == nil && len(out.gmail) == 0 {
			err = store.ErrConnectionNotFound
		}
		for _, a := range out.gmail {
			out.prefixes = append(out.prefixes, gmailsync.JudgedPrefix(strings.ToLower(a.Email)))
		}
		out.fallback = gmailsync.FallbackPrefix
	}
	switch {
	case errors.Is(err, store.ErrConnectionNotFound):
		return tryConnection{}, errTryNotConnected
	case err != nil:
		log.Printf("integrations: ai try connection user=%s: %v", userID, err)
		return tryConnection{}, errTryStore
	}
	return out, nil
}

// trySample reports whether the user's item can be tried with the scope:
// an item of the integration made from a judgment of the current connection
// (see the samples queries), and for a reply scope one completed by a reply
// or tracked and checked at least once.
func trySample(sc tryScope, c tryConnection, item store.ActionItem) bool {
	key := item.SourceKey.String
	if item.Source.String != sc.source || !item.SourceKey.Valid {
		return false
	}
	matched := false
	for _, p := range c.prefixes {
		matched = matched || strings.HasPrefix(key, p)
	}
	if !matched {
		return false
	}
	var parsed bool
	switch sc.source {
	case store.SourceGitHub:
		_, _, parsed = githubsync.ParseMentionKey(key)
	case store.SourceSlack:
		_, parsed = slacksync.ParseKey(key)
	case store.SourceChatwork:
		_, parsed = chatworksync.ParseKey(key)
	case store.SourceGmail:
		_, parsed = gmailsync.ParseKey(key)
	}
	if !parsed {
		return false
	}
	if sc.reply {
		return item.AutoReason.String == sc.reason || ((item.Status == "open" || item.Status == "candidate") && item.Tracking == 1 && item.TrackedAt.Valid)
	}
	return item.DetectedAt.Valid && !strings.HasPrefix(item.Content, c.fallback)
}

// conversationName is the conversation name of the item's first ref, or
// "" when it has none.
func conversationName(item store.ActionItem) string {
	var refs []struct {
		ConversationName string `json:"conversation_name"`
	}
	if json.Unmarshal([]byte(item.Refs), &refs) != nil || len(refs) == 0 {
		return ""
	}
	return refs[0].ConversationName
}

// conversationID is the conversation ID of the item's first ref, or "".
func conversationID(item store.ActionItem) string {
	var refs []struct {
		ConversationID string `json:"conversation_id"`
	}
	if json.Unmarshal([]byte(item.Refs), &refs) != nil || len(refs) == 0 {
		return ""
	}
	return refs[0].ConversationID
}

// handleAIInstructionSamples lists the user's recent items of a scope's
// integration that can be tried.
func (s *Server) handleAIInstructionSamples(w http.ResponseWriter, r *http.Request) {
	u, ok := userFrom(r.Context())
	if !ok {
		s.authError(w, r, store.ErrUnauthorized)
		return
	}
	sc, ok := tryScopes[r.URL.Query().Get("scope")]
	if !ok {
		fail(w, 400, "invalid_request", "scope must be one of the scopes other than common.")
		return
	}
	c, e := s.tryConnection(r.Context(), u.ID, sc)
	if e != nil {
		failTry(w, e)
		return
	}
	var items []store.ActionItem
	var err error
	if sc.reply {
		items, err = s.auth.Store.AIReplySamples(r.Context(), u.ID, sc.source, c.prefixes, sc.reason, trySampleLimit/2)
	} else {
		items, err = s.auth.Store.AIDetectSamples(r.Context(), u.ID, sc.source, c.prefixes, c.fallback, trySampleLimit)
	}
	if err != nil {
		integrationError(w, "ai instruction samples", u.ID, err)
		return
	}
	out := trySamplesResponse{Samples: []trySampleResponse{}}
	for _, item := range items {
		v := trySampleResponse{ID: item.ID, Content: item.Content, Status: item.Status}
		if item.AutoReason.Valid {
			v.AutoReason = &item.AutoReason.String
		}
		if name := conversationName(item); name != "" {
			v.ConversationName = &name
		}
		if item.DetectedAt.Valid {
			at := formatTime(item.DetectedAt.Int64)
			v.DetectedAt = &at
		}
		out.Samples = append(out.Samples, v)
	}
	writeJSON(w, 200, out)
}

// tryEventResponse is the event of a detection: start and end as the AI
// wrote them (local time of the user's time zone, YYYY-MM-DDTHH:MM).
type tryEventResponse struct {
	Title string  `json:"title"`
	Start string  `json:"start"`
	End   *string `json:"end"`
}

type tryDetectResponse struct {
	Decision   string            `json:"decision"`
	Task       string            `json:"task"`
	Importance string            `json:"importance"`
	Summary    string            `json:"summary"`
	Event      *tryEventResponse `json:"event"`
}

type tryReplyResponse struct {
	Done bool `json:"done"`
}

type tryResponse struct {
	Current any `json:"current"`
	Draft   any `json:"draft"`
}

// eventLayout is the layout of the event times of a judgment.
const eventLayout = "2006-01-02T15:04"

func toTryDetect(res ai.MentionResult, loc *time.Location) tryDetectResponse {
	out := tryDetectResponse{Decision: res.Decision, Task: res.Task, Importance: res.Importance, Summary: res.Summary}
	if res.Event != nil {
		out.Event = &tryEventResponse{Title: res.Event.Title, Start: res.Event.Start.In(loc).Format(eventLayout)}
		if !res.Event.End.IsZero() {
			end := res.Event.End.In(loc).Format(eventLayout)
			out.Event.End = &end
		}
	}
	return out
}

// tryJudge judges the input built for a try with instructions.
type tryJudge func(ctx context.Context, cred ai.Credential, model string, in ai.Instructions) (any, error)

// detectJudge judges a conversation input with judge.
func detectJudge(judge func(context.Context, ai.Credential, string, ai.SlackInput) (ai.MentionResult, error), in ai.SlackInput, loc *time.Location) tryJudge {
	return func(ctx context.Context, cred ai.Credential, model string, instructions ai.Instructions) (any, error) {
		in.Instructions = instructions
		res, err := judge(ctx, cred, model, in)
		return toTryDetect(res, loc), err
	}
}

// replyJudge judges a reply input with judge.
func replyJudge(judge func(context.Context, ai.Credential, string, ai.ReplyInput) (ai.ReplyResult, error), in ai.ReplyInput) tryJudge {
	return func(ctx context.Context, cred ai.Credential, model string, instructions ai.Instructions) (any, error) {
		in.Instructions = instructions
		res, err := judge(ctx, cred, model, in)
		return tryReplyResponse{Done: res.Resolved}, err
	}
}

// tryText returns a required instruction of a try body, trimmed, or false
// when it is missing, not a string or too long.
func tryText(raw json.RawMessage) (string, bool) {
	if raw == nil {
		return "", false
	}
	v, ok := decodeString(raw)
	v = strings.TrimSpace(v)
	return v, ok && utf8.RuneCountInString(v) <= store.MaxInstructionLength
}

// handleTryAIInstructions judges an item of a scope again with the saved
// instructions and with the draft ones of the body, and returns both
// results. Nothing is written: no item, notification, AI status or tracking
// time changes.
func (s *Server) handleTryAIInstructions(w http.ResponseWriter, r *http.Request) {
	u, ok := userFrom(r.Context())
	if !ok {
		s.authError(w, r, store.ErrUnauthorized)
		return
	}
	var in struct {
		Scope       json.RawMessage `json:"scope"`
		ItemID      json.RawMessage `json:"item_id"`
		Common      json.RawMessage `json:"common"`
		Instruction json.RawMessage `json:"instruction"`
	}
	if !decode(w, r, &in, instructionsBodyLimit) {
		return
	}
	var scope, itemID string
	if in.Scope != nil {
		scope, ok = decodeString(in.Scope)
	}
	if in.Scope == nil || !ok {
		fail(w, 400, "invalid_request", "scope must be a string.")
		return
	}
	if in.ItemID != nil {
		itemID, ok = decodeString(in.ItemID)
	}
	if in.ItemID == nil || !ok {
		fail(w, 400, "invalid_request", "item_id must be a string.")
		return
	}
	sc, found := tryScopes[scope]
	if !found {
		fail(w, 400, "invalid_request", "scope must be one of the scopes other than common.")
		return
	}
	common, ok := tryText(in.Common)
	if !ok {
		fail(w, 400, "invalid_request", fmt.Sprintf("common must be a string of at most %d characters.", store.MaxInstructionLength))
		return
	}
	instruction, ok := tryText(in.Instruction)
	if !ok {
		fail(w, 400, "invalid_request", fmt.Sprintf("instruction must be a string of at most %d characters.", store.MaxInstructionLength))
		return
	}
	if !s.aiTryLimiter.allowKey(u.ID) {
		failTry(w, errTryRateLimited)
		return
	}
	if !itemIDPattern.MatchString(itemID) {
		failTry(w, errTryNotFound)
		return
	}
	extendWriteDeadline(w)
	ctx, cancel := context.WithTimeout(r.Context(), tryTimeout)
	defer cancel()
	out, e := s.tryAIInstructions(ctx, u.ID, scope, sc, itemID, ai.Instructions{Common: common, Process: instruction})
	switch {
	case e == nil:
		writeJSON(w, 200, out)
	case r.Context().Err() != nil:
		fail(w, 503, "unavailable", "The service is temporarily unavailable. Try again.")
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		failTry(w, errTryTimeout)
	default:
		failTry(w, e)
	}
}

// tryAIInstructions checks the connection, the item and the AI settings in
// this order, builds the input again from the integration and judges it
// with the saved and the draft instructions, one after the other.
func (s *Server) tryAIInstructions(ctx context.Context, userID, scope string, sc tryScope, itemID string, draft ai.Instructions) (tryResponse, *tryError) {
	c, e := s.tryConnection(ctx, userID, sc)
	if e != nil {
		return tryResponse{}, e
	}
	item, err := s.auth.Store.ActionItem(ctx, userID, itemID)
	switch {
	case errors.Is(err, store.ErrItemNotFound):
		return tryResponse{}, errTryNotFound
	case err != nil:
		log.Printf("integrations: ai try item user=%s: %v", userID, err)
		return tryResponse{}, errTryStore
	case !trySample(sc, c, item):
		return tryResponse{}, errTryNotFound
	}
	a, err := s.auth.Store.AISettings(ctx, userID)
	switch {
	case errors.Is(err, store.ErrAISettingsNotFound):
		return tryResponse{}, errTryAINotReady
	case err != nil:
		log.Printf("integrations: ai try settings user=%s: %v", userID, err)
		return tryResponse{}, errTryStore
	case a.Status != store.AIActive:
		return tryResponse{}, errTryAINotReady
	case aicred.CoolingDown(a, s.auth.Now()):
		return tryResponse{}, errTryAICoolingDown
	}
	saved, err := s.auth.Store.AIInstructions(ctx, userID)
	if err != nil {
		log.Printf("integrations: ai try instructions user=%s: %v", userID, err)
		return tryResponse{}, errTryStore
	}
	settings, err := s.auth.Store.NotificationSettings(ctx, userID)
	if err != nil {
		log.Printf("integrations: ai try settings user=%s: %v", userID, err)
		return tryResponse{}, errTryStore
	}
	loc, err := time.LoadLocation(settings.Timezone)
	if err != nil {
		loc = time.UTC
	}
	var judge tryJudge
	switch sc.source {
	case store.SourceGitHub:
		judge, e = s.tryGitHub(ctx, userID, c.github, item, loc)
	case store.SourceSlack:
		judge, e = s.trySlack(ctx, userID, c.slack, item, sc, loc)
	case store.SourceChatwork:
		judge, e = s.tryChatwork(ctx, userID, c.chatwork, item, sc, loc)
	case store.SourceGmail:
		judge, e = s.tryGmail(ctx, userID, item, sc, loc)
	}
	if e != nil {
		return tryResponse{}, e
	}
	cred, err := aicred.Open(s.box, s.tokens, a)
	if err != nil {
		return tryResponse{}, tryAIError(ctx, userID, err)
	}
	current, err := judge(ctx, cred, a.Model, ai.Instructions{Common: saved[store.InstructionCommon], Process: saved[scope]})
	if err != nil {
		return tryResponse{}, tryAIError(ctx, userID, err)
	}
	next, err := judge(ctx, cred, a.Model, draft)
	if err != nil {
		return tryResponse{}, tryAIError(ctx, userID, err)
	}
	return tryResponse{Current: current, Draft: next}, nil
}

// tryAIError converts the error of an AI call of a try. The AI status is
// not recorded: a try never changes the settings.
func tryAIError(ctx context.Context, userID string, err error) *tryError {
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return errTryTimeout
	case errors.Is(err, aicred.ErrStale):
		return &tryError{status: 409, code: "ai_not_ready", message: "AIの設定が変わりました。もう一度お試しください。"}
	case errors.Is(err, ai.ErrKeyInvalid), errors.Is(err, aicred.ErrUndecryptable):
		return &tryError{status: 409, code: "ai_key_invalid", message: "AIのキーまたは接続を確認してください。"}
	case errors.Is(err, ai.ErrModelUnavailable):
		return &tryError{status: 409, code: "ai_model_unavailable", message: "選んだモデルを使えません。"}
	case errors.Is(err, ai.ErrBilling):
		return &tryError{status: 409, code: "ai_billing", message: "AIの利用料金の設定を確認してください。"}
	case errors.Is(err, ai.ErrUsageLimit):
		return &tryError{status: 429, code: "ai_usage_limit", message: "AIの利用上限に達しました。時間をおいてお試しください。"}
	case errors.Is(err, ai.ErrUnusable):
		log.Printf("integrations: ai try user=%s: %v", userID, err)
		return &tryError{status: 502, code: "ai_error", message: "AIの応答を読み取れませんでした。"}
	}
	log.Printf("integrations: ai try user=%s: %v", userID, err)
	return &tryError{status: 502, code: "ai_error", message: "AIで判定できませんでした。"}
}

// tryRead makes one call to an integration within tryReadTimeout.
func tryRead(ctx context.Context, fn func(ctx context.Context) error) error {
	cctx, cancel := context.WithTimeout(ctx, tryReadTimeout)
	defer cancel()
	return fn(cctx)
}

// logUpstream logs an unexpected error of an integration; a call cut by
// its own timeout or the try's is not logged.
func logUpstream(op, userID string, err error) {
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		log.Printf("integrations: ai try %s user=%s: %v", op, userID, err)
	}
}

// tryGitHub reads the issue or pull request of a mention item again.
func (s *Server) tryGitHub(ctx context.Context, userID string, c store.GitHubConnection, item store.ActionItem, loc *time.Location) (tryJudge, *tryError) {
	gref, postID, _ := githubsync.ParseMentionKey(item.SourceKey.String)
	// The key is lower-cased; the ref keeps the names as GitHub gave them.
	if name := conversationID(item); strings.EqualFold(name, fmt.Sprintf("%s/%s#%d", gref.Owner, gref.Repo, gref.Number)) {
		if owner, rest, ok := strings.Cut(name, "/"); ok {
			if repo, _, ok := strings.Cut(rest, "#"); ok {
				gref.Owner, gref.Repo = owner, repo
			}
		}
	}
	token, err := s.box.Open(githubsync.TokenPurpose(userID), c.TokenSealed)
	if err != nil {
		log.Printf("integrations: ai try github user=%s: open token: %v", userID, err)
		return nil, errTryNotConnected
	}
	gh := s.github
	var issue github.Issue
	var comments, reviewComments []github.Comment
	var reviews []github.Review
	err = tryRead(ctx, func(ctx context.Context) error {
		var e error
		issue, e = gh.Issue(ctx, token, gref.Owner, gref.Repo, gref.Number)
		return e
	})
	if err == nil {
		err = tryRead(ctx, func(ctx context.Context) error {
			var e error
			comments, e = gh.IssueComments(ctx, token, gref.Owner, gref.Repo, gref.Number)
			return e
		})
	}
	if err == nil && issue.IsPull {
		err = tryRead(ctx, func(ctx context.Context) error {
			var e error
			reviews, e = gh.RecentReviews(ctx, token, gref.Owner, gref.Repo, gref.Number)
			return e
		})
		if err == nil {
			err = tryRead(ctx, func(ctx context.Context) error {
				var e error
				reviewComments, e = gh.ReviewComments(ctx, token, gref.Owner, gref.Repo, gref.Number)
				return e
			})
		}
	}
	if err != nil {
		return nil, s.githubTryError(userID, err)
	}
	related, err := s.auth.Store.RelatedTasks(ctx, userID, githubsync.RejudgeScope(gref, s.auth.Now(), []string{item.ID}))
	if err != nil {
		log.Printf("integrations: ai try related user=%s: %v", userID, err)
		return nil, errTryStore
	}
	in, ok := githubsync.RejudgeInput(c.Login, loc, gref, issue, comments, reviews, reviewComments, postID, related)
	if !ok {
		return nil, errTryUnavailable
	}
	return func(ctx context.Context, cred ai.Credential, model string, instructions ai.Instructions) (any, error) {
		in.Instructions = instructions
		res, err := s.ai.JudgeMention(ctx, cred, model, in)
		return toTryDetect(res, loc), err
	}, nil
}

func (s *Server) githubTryError(userID string, err error) *tryError {
	var rate *github.RateLimitError
	switch {
	case errors.As(err, &rate):
		return rateLimited(rate.RetryAt, s.auth.Now())
	case errors.Is(err, github.ErrRateLimited):
		return rateLimited(time.Time{}, s.auth.Now())
	case errors.Is(err, github.ErrUnauthorized):
		return errTryNotConnected
	case errors.Is(err, github.ErrForbidden), errors.Is(err, github.ErrNotFound):
		return errTryUnavailable
	}
	logUpstream("github", userID, err)
	return errTryUpstream
}

// trySlack reads the conversation of a Slack item again: the thread, or
// outside threads the item's messages and the messages around them (or
// after them, for a reply). The calls are taken from slackHistoryLimiter
// at once, shared with the MCP tools.
func (s *Server) trySlack(ctx context.Context, userID string, c store.SlackConnection, item store.ActionItem, sc tryScope, loc *time.Location) (tryJudge, *tryError) {
	key, _ := slacksync.ParseKey(item.SourceKey.String)
	token, err := s.box.Open(slacksync.TokenPurpose(userID), c.TokenSealed)
	if err != nil {
		log.Printf("integrations: ai try slack user=%s: open token: %v", userID, err)
		return nil, errTryNotConnected
	}
	calls := trySlackChannelCalls
	switch {
	case sc.reply:
		calls = trySlackReplyCalls
	case key.Thread != slacksync.NoThread:
		calls = trySlackThreadCalls
	}
	if !s.slackHistoryLimiter.allowKeyN(userID, calls) {
		e := *errTryRateLimited
		e.message = "Slackを読み直せる回数の上限に達しました。1分ほどおいてお試しください。"
		return nil, &e
	}
	read := func(replies bool, p slack.HistoryParams) (slack.HistoryPage, *tryError) {
		var page slack.HistoryPage
		err := tryRead(ctx, func(ctx context.Context) error {
			var e error
			if replies {
				page, e = s.slack.Replies(ctx, token, p)
			} else {
				page, e = s.slack.History(ctx, token, p)
			}
			return e
		})
		if err != nil {
			return page, s.slackTryError(userID, err)
		}
		return page, nil
	}
	thread := func(p slack.HistoryParams) ([]slack.Message, *tryError) {
		var out []slack.Message
		for range trySlackPages {
			page, e := read(true, p)
			if e != nil {
				return nil, e
			}
			out = append(out, page.Messages...)
			if !page.HasMore || page.NextCursor == "" {
				break
			}
			p.Cursor = page.NextCursor
		}
		return out, nil
	}
	var msgs []slack.Message
	switch {
	case key.Thread != slacksync.NoThread:
		p := slack.HistoryParams{Channel: key.Channel, TS: key.Thread, Limit: historyLimit}
		if sc.reply {
			p.Oldest = key.Last
		}
		list, e := thread(p)
		if e != nil {
			return nil, e
		}
		msgs = list
	case sc.reply:
		page, e := read(false, slack.HistoryParams{Channel: key.Channel, Latest: key.Last, Oldest: key.Last, Inclusive: true, Limit: 1})
		if e != nil {
			return nil, e
		}
		var origin *slack.Message
		for i := range page.Messages {
			if page.Messages[i].TS == key.Last {
				origin = &page.Messages[i]
			}
		}
		if origin == nil {
			return nil, errTryUnavailable
		}
		msgs = append(msgs, *origin)
		if page, e = read(false, slack.HistoryParams{Channel: key.Channel, Oldest: key.Last, Limit: historyLimit}); e != nil {
			return nil, e
		}
		msgs = append(msgs, page.Messages...)
		if slackTSPattern.MatchString(origin.LatestReply) && origin.LatestReply != origin.TS {
			if page, e = read(true, slack.HistoryParams{Channel: key.Channel, TS: key.Last, Oldest: key.Last, Limit: historyLimit}); e != nil {
				return nil, e
			}
			msgs = append(msgs, page.Messages...)
		}
	default:
		for _, p := range []slack.HistoryParams{
			{Channel: key.Channel, Oldest: key.First, Latest: key.Last, Inclusive: true, Limit: historyLimit},
			{Channel: key.Channel, Latest: key.First, Limit: ai.PostsBefore},
			{Channel: key.Channel, Oldest: key.Last, Limit: ai.PostsAfter},
		} {
			page, e := read(false, p)
			if e != nil {
				return nil, e
			}
			msgs = append(msgs, page.Messages...)
		}
	}
	nctx, ncancel := context.WithTimeout(ctx, tryReadTimeout)
	defer ncancel()
	names := s.newSlackNames(nctx, c, token)
	if sc.reply {
		in, ok, err := slacksync.RejudgeReplyInput(c, loc, key, item.Content, msgs, names.name)
		switch {
		case err != nil:
			return nil, s.slackTryError(userID, err)
		case !ok:
			return nil, errTryNoReply
		}
		return replyJudge(s.ai.JudgeReply, in), nil
	}
	convType, channelName := slacksync.Conversation(key.Channel, conversationName(item))
	related, err := s.auth.Store.RelatedTasks(ctx, userID, slacksync.RejudgeScope(key, convType, s.auth.Now(), []string{item.ID}))
	if err != nil {
		log.Printf("integrations: ai try related user=%s: %v", userID, err)
		return nil, errTryStore
	}
	in, ok, err := slacksync.RejudgeInput(c, loc, key, convType, channelName, msgs, related, names.name)
	switch {
	case err != nil:
		return nil, s.slackTryError(userID, err)
	case !ok:
		return nil, errTryUnavailable
	}
	return detectJudge(s.ai.JudgeSlack, in, loc), nil
}

// historyLimit is the page size of the conversations read again, as the
// synchronization's.
const historyLimit = 100

func (s *Server) slackTryError(userID string, err error) *tryError {
	var rate *slack.RateLimitError
	var api *slack.APIError
	switch {
	case errors.Is(err, slack.ErrUnauthorized), errors.Is(err, slack.ErrMissingScope):
		return errTryNotConnected
	case errors.As(err, &rate):
		return rateLimited(rate.RetryAt, s.auth.Now())
	case errors.As(err, &api) && slackNotFoundCodes[api.Code]:
		return errTryUnavailable
	}
	logUpstream("slack", userID, err)
	return errTryUpstream
}

// tryChatwork reads the latest messages of a Chatwork item's room again,
// with the room list for its type and name.
func (s *Server) tryChatwork(ctx context.Context, userID string, c store.ChatworkConnection, item store.ActionItem, sc tryScope, loc *time.Location) (tryJudge, *tryError) {
	key, _ := chatworksync.ParseKey(item.SourceKey.String)
	token, err := s.box.Open(chatworksync.TokenPurpose(userID), c.TokenSealed)
	if err != nil {
		log.Printf("integrations: ai try chatwork user=%s: open token: %v", userID, err)
		return nil, errTryNotConnected
	}
	calls := 1
	if !sc.reply {
		calls = 2
	}
	if !s.chatworkMCPLimiter.allowKeyN(userID, calls) {
		e := *errTryRateLimited
		e.message = "Chatworkを読み直せる回数の上限に達しました。数分おいてお試しください。"
		return nil, &e
	}
	var room chatwork.Room
	if !sc.reply {
		var rooms []chatwork.Room
		err = tryRead(ctx, func(ctx context.Context) error {
			var e error
			rooms, e = s.chatwork.Rooms(ctx, token)
			return e
		})
		if err != nil {
			return nil, s.chatworkTryError(userID, err)
		}
		found := false
		for _, v := range rooms {
			if v.ID == key.Room {
				room, found = v, true
			}
		}
		if !found {
			return nil, errTryUnavailable
		}
	}
	var page chatwork.MessagePage
	err = tryRead(ctx, func(ctx context.Context) error {
		var e error
		page, e = s.chatwork.Messages(ctx, token, key.Room)
		return e
	})
	if err != nil {
		return nil, s.chatworkTryError(userID, err)
	}
	if sc.reply {
		in, ok := chatworksync.RejudgeReplyInput(c, loc, key, item.Content, page.Messages)
		if !ok {
			return nil, errTryNoReply
		}
		return replyJudge(s.ai.JudgeChatworkReply, in), nil
	}
	related, err := s.auth.Store.RelatedTasks(ctx, userID, chatworksync.RejudgeScope(key, s.auth.Now(), []string{item.ID}))
	if err != nil {
		log.Printf("integrations: ai try related user=%s: %v", userID, err)
		return nil, errTryStore
	}
	in, ok := chatworksync.RejudgeInput(c, loc, key, room, page.Messages, related)
	if !ok {
		return nil, errTryUnavailable
	}
	return detectJudge(s.ai.JudgeChatwork, in, loc), nil
}

func (s *Server) chatworkTryError(userID string, err error) *tryError {
	var rate *chatwork.RateLimitError
	var api *chatwork.APIError
	switch {
	case errors.Is(err, chatwork.ErrUnauthorized):
		return errTryNotConnected
	case errors.As(err, &rate):
		return rateLimited(rate.RetryAt, s.auth.Now())
	case errors.As(err, &api) && !api.TooLarge && (api.Status == http.StatusForbidden || api.Status == http.StatusNotFound):
		return errTryUnavailable
	}
	logUpstream("chatwork", userID, err)
	return errTryUpstream
}

// tryGmail reads the thread of a mail item again, in the account of its
// key, with the MCP tools' tokens and quota.
func (s *Server) tryGmail(ctx context.Context, userID string, item store.ActionItem, sc tryScope, loc *time.Location) (tryJudge, *tryError) {
	key, _ := gmailsync.ParseKey(item.SourceKey.String)
	a, oe := s.gmailAccount(ctx, "ai try", userID, key.Email)
	if oe != nil {
		return nil, gmailTryOpError(oe)
	}
	var t google.Thread
	err := tryRead(ctx, func(ctx context.Context) error {
		return s.gmailCall(ctx, "ai try", a, gmailFullUnits, func(token string) error {
			var e error
			t, e = s.google.Thread(ctx, token, key.Thread)
			return e
		})
	})
	if err != nil {
		return nil, s.gmailTryError(userID, a, err)
	}
	if sc.reply {
		in, ok := gmailsync.RejudgeReplyInput(key.Email, loc, key, item.Content, t)
		if !ok {
			return nil, errTryNoReply
		}
		return replyJudge(s.ai.JudgeMailReply, in), nil
	}
	related, err := s.auth.Store.RelatedTasks(ctx, userID, gmailsync.RejudgeScope(key, s.auth.Now(), []string{item.ID}))
	if err != nil {
		log.Printf("integrations: ai try related user=%s: %v", userID, err)
		return nil, errTryStore
	}
	in, ok := gmailsync.RejudgeInput(key.Email, loc, key, t, related)
	if !ok {
		return nil, errTryUnavailable
	}
	return detectJudge(s.ai.JudgeMail, in, loc), nil
}

// gmailTryOpError converts a failure of the MCP tools' Gmail helpers.
func gmailTryOpError(e *opError) *tryError {
	switch e {
	case errGmailAccountNotFound:
		return errTryNotFound
	case errGmailNotConnected, errGmailTokenInvalid, errGmailScopesMissing, errGmailPolicyBlocked:
		return errTryNotConnected
	case errGmailMCPLimited:
		return errTryRateLimited
	case errGmailStore:
		return errTryStore
	}
	return errTryUpstream
}

func (s *Server) gmailTryError(userID string, a store.GmailAccount, err error) *tryError {
	var tokenErr *gmailTokenError
	var rate *google.RateLimitError
	var api *google.APIError
	switch {
	case errors.As(err, &tokenErr):
		return gmailTryOpError(tokenErr.e)
	case errors.Is(err, errGmailLimited):
		e := *errTryRateLimited
		e.message = "Gmailを読み直せる回数の上限に達しました。1分ほどおいてお試しください。"
		return &e
	case errors.Is(err, google.ErrUnauthorized), errors.Is(err, google.ErrInvalidGrant), errors.Is(err, google.ErrInsufficientScope), errors.Is(err, google.ErrPolicyBlocked):
		return errTryNotConnected
	case errors.As(err, &rate):
		return rateLimited(rate.RetryAt, s.auth.Now())
	case errors.As(err, &api) && !api.TooLarge && (api.Status == http.StatusNotFound || api.Status == http.StatusBadRequest):
		return errTryUnavailable
	}
	logUpstream("gmail account="+a.ID, userID, err)
	return errTryUpstream
}
