// Package slacksync finds Slack messages that may need something from the
// user (mentions, direct messages and group direct messages), lets the AI
// judge them with the user's own key and creates action items, and
// completes the items when the AI judges that the user's later reply dealt
// with them. Without active AI settings nothing is read from Slack. As in
// githubsync, requests to Slack and the AI are never made inside a database
// transaction.
package slacksync

import (
	"context"
	"database/sql"
	"errors"
	"html"
	"log"
	"log/slog"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/i2bskn/astovia/internal/ai"
	"github.com/i2bskn/astovia/internal/secretbox"
	"github.com/i2bskn/astovia/internal/slack"
	"github.com/i2bskn/astovia/internal/store"
)

const (
	// BatchSize is the number of due connections handled by one RunOnce.
	BatchSize = 20
	// connectionTimeout bounds the Slack requests of one connection's
	// synchronization; what is left is done in the next one.
	connectionTimeout = 60 * time.Second
	pollInterval      = 60
	trackInterval     = 5 * time.Minute
	rateLimitWait     = 15 * time.Minute
	maxBackoff        = 900
)

// Error codes stored in slack_connections.last_error and shown in the UI.
const (
	ErrorTokenInvalid       = "token_invalid"
	ErrorTokenUndecryptable = "token_undecryptable"
	ErrorScopesMissing      = "scopes_missing"
	ErrorRateLimited        = "rate_limited"
	ErrorUnavailable        = "unavailable"
)

// Automatic completion reasons stored in action_items.auto_reason.
const (
	ReasonNoAction      = "slack.no_action"
	ReasonReplyResolved = "slack.reply_resolved"
)

// Error codes stored in ai_settings.last_error by the synchronization. They
// are the same as githubsync's.
const (
	AIErrorKeyInvalid       = "key_invalid"
	AIErrorKeyUndecryptable = "key_undecryptable"
	AIErrorBilling          = "billing"
	AIErrorModelUnavailable = "model_unavailable"
	AIErrorRequestRejected  = "request_rejected"
)

// RequiredScopes are the user token scopes the integration needs.
var RequiredScopes = []string{"search:read", "users:read", "channels:read", "groups:read", "im:read", "mpim:read", "channels:history", "groups:history", "im:history", "mpim:history"}

// TokenPurpose is the secretbox purpose of a user's Slack token.
func TokenPurpose(userID string) string { return "slack-token:" + userID }

// Kinds of Slack API calls counted by the per-synchronization budget.
const (
	callSearch        = "search.messages"
	callConversations = "users.conversations"
	callHistory       = "conversations.history+replies"
	callUserInfo      = "users.info"
	callPermalink     = "chat.getPermalink"
)

// callLimits bound the Slack calls of one synchronization, so that a
// synchronization a minute stays within the methods' rate limits.
// trackHistoryLimit is the part of callHistory tracking may use.
var callLimits = map[string]int{callSearch: 4, callConversations: 1, callHistory: 40, callUserInfo: 20, callPermalink: 10}

const trackHistoryLimit = 20

// errBudget is returned instead of calling Slack when the call budget of the
// synchronization is used up. It never stops the synchronization.
var errBudget = errors.New("slacksync: call budget exhausted")

// Syncer synchronizes Slack connections. It assumes a single process. AI
// judges every message; without it nothing is detected or completed.
type Syncer struct {
	Store *store.Store
	Slack *slack.Client
	AI    *ai.Client
	Box   *secretbox.Box
	Now   func() time.Time
}

// RunOnce synchronizes up to BatchSize due connections, one after another.
func (s *Syncer) RunOnce(ctx context.Context) {
	conns, err := s.Store.DueSlackConnections(ctx, s.Now().Unix(), BatchSize)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("slack sync: list connections: %v", err)
		}
		return
	}
	for _, c := range conns {
		if ctx.Err() != nil {
			return
		}
		s.Sync(ctx, c)
	}
}

// run is the state of one connection's synchronization. ctx carries the
// connection deadline and is used for Slack and AI requests; parent is used
// for database reads and writes, so that they still happen after the
// deadline.
type run struct {
	s      *Syncer
	ctx    context.Context
	parent context.Context
	conn   store.SlackConnection
	state  store.SlackConnection
	token  string
	start  time.Time

	// calls counts the Slack calls per kind; trackHistory the history calls
	// made by tracking. tracking is set while tracking runs.
	calls        map[string]int
	trackHistory int
	tracking     bool
	// names caches display names by member ID.
	names map[string]string
	// loc is the user's time zone for the AI, read once when needed.
	loc *time.Location

	// ai is the user's AI settings as last read. detectCalls and trackCalls
	// count the AI calls. aiFallbackOnly is set after the AI failed in a way
	// that stops it for the user; aiStale when the settings were replaced
	// during the synchronization; aiStopped when they were deleted or the
	// key cannot be opened.
	ai             *store.AISettings
	detectCalls    int
	trackCalls     int
	aiFallbackOnly bool
	aiStale        bool
	aiStopped      bool

	// The detection window [from, to) and its state: firstImport when the
	// window has never advanced; overdue when it has stayed behind for
	// longer than retryLimit; pending when something was carried over to
	// the next synchronization; lost counts what an overdue synchronization
	// gives up.
	from, to    int64
	firstImport bool
	overdue     bool
	pending     bool
	lost        int
}

// Sync synchronizes one connection and stores its new state. When the
// connection changed in the meantime (another token, deleted, user disabled)
// the results are discarded.
func (s *Syncer) Sync(parent context.Context, c store.SlackConnection) {
	r := &run{s: s, parent: parent, conn: c, state: c, start: s.Now(), calls: map[string]int{}, names: map[string]string{}}
	token, err := s.Box.Open(TokenPurpose(c.UserID), c.TokenSealed)
	if err != nil {
		r.state.Status, r.state.LastError = store.SlackError, nullString(ErrorTokenUndecryptable)
		log.Printf("slack sync: user=%s: %s", c.UserID, ErrorTokenUndecryptable)
		r.save()
		return
	}
	r.token = token
	ctx, cancel := context.WithTimeout(parent, connectionTimeout)
	defer cancel()
	r.ctx = ctx
	err = r.steps()
	now := s.Now().Unix()
	var rate *slack.RateLimitError
	switch {
	case errors.Is(err, store.ErrStaleConnection) || parent.Err() != nil:
		return
	case err == nil || ctx.Err() != nil:
		// Running out of time is not a failure; the rest waits for the
		// next poll.
		r.state.FailureCount, r.state.LastError, r.state.NextPollAt = 0, sql.NullString{}, now+pollInterval
	case errors.Is(err, slack.ErrUnauthorized):
		r.state.Status, r.state.LastError = store.SlackError, nullString(ErrorTokenInvalid)
		log.Printf("slack sync: user=%s: %s", c.UserID, ErrorTokenInvalid)
	case errors.Is(err, slack.ErrMissingScope):
		r.state.Status, r.state.LastError = store.SlackError, nullString(ErrorScopesMissing)
		log.Printf("slack sync: user=%s: %s", c.UserID, ErrorScopesMissing)
	case errors.As(err, &rate):
		r.state.LastError = nullString(ErrorRateLimited)
		r.state.NextPollAt = now + int64(rateLimitWait/time.Second)
		if !rate.RetryAt.IsZero() {
			r.state.NextPollAt = rate.RetryAt.Unix()
		}
		log.Printf("slack sync: user=%s: %s", c.UserID, ErrorRateLimited)
	default:
		r.state.FailureCount++
		r.state.LastError = nullString(ErrorUnavailable)
		r.state.NextPollAt = now + backoff(r.state.FailureCount)
		log.Printf("slack sync: user=%s: %s", c.UserID, ErrorUnavailable)
	}
	r.save()
}

// backoff returns min(60 * 2^(failures-1), 900) seconds.
func backoff(failures int64) int64 {
	wait := int64(60)
	for i := int64(1); i < failures && wait < maxBackoff; i++ {
		wait *= 2
	}
	if wait > maxBackoff {
		wait = maxBackoff
	}
	return wait
}

func (r *run) save() {
	r.state.UpdatedAt = r.s.Now().Unix()
	err := r.s.Store.SlackSync(r.parent, r.conn.UserID, r.conn.Generation, func(tx *store.SlackTx) error {
		return tx.SaveState(r.state)
	})
	if err != nil && !errors.Is(err, store.ErrStaleConnection) && r.parent.Err() == nil {
		log.Printf("slack sync: user=%s: save state: %v", r.conn.UserID, err)
	}
}

// steps runs tracking (every trackInterval) and detection, recording
// progress in r.state. Without active AI settings nothing is done, and the
// detection window does not move.
func (r *run) steps() error {
	if r.s.AI == nil {
		return nil
	}
	a, err := r.s.Store.AISettings(r.parent, r.conn.UserID)
	switch {
	case errors.Is(err, store.ErrAISettingsNotFound):
		return nil
	case err != nil:
		return err
	case a.Status != store.AIActive:
		return nil
	}
	r.ai = &a
	if due(r.state.LastTrackedAt, r.start.Unix(), trackInterval) {
		if err = r.track(); err != nil {
			return err
		}
	}
	return r.detect()
}

func due(last sql.NullInt64, now int64, every time.Duration) bool {
	return !last.Valid || now-last.Int64 >= int64(every/time.Second)
}

// stops reports whether err ends the whole synchronization. Only an error
// code about the object asked for (a non-temporary *slack.APIError) and the
// call budget leave it running.
func stops(err error) bool {
	var api *slack.APIError
	if errors.As(err, &api) {
		return api.Temporary()
	}
	return !errors.Is(err, errBudget)
}

// apiCode returns the Slack error code of a non-stopping error, for logs.
func apiCode(err error) string {
	var api *slack.APIError
	if errors.As(err, &api) {
		return api.Method + " " + api.Code
	}
	return "budget"
}

// take counts one call of the kind, or reports false when the budget of the
// synchronization (or of tracking) is used up.
func (r *run) take(kind string) bool {
	if r.calls[kind] >= callLimits[kind] {
		return false
	}
	if kind == callHistory && r.tracking {
		if r.trackHistory >= trackHistoryLimit {
			return false
		}
		r.trackHistory++
	}
	r.calls[kind]++
	return true
}

func (r *run) history(p slack.HistoryParams) (slack.HistoryPage, error) {
	if !r.take(callHistory) {
		return slack.HistoryPage{}, errBudget
	}
	return r.s.Slack.History(r.ctx, r.token, p)
}

func (r *run) replies(p slack.HistoryParams) (slack.HistoryPage, error) {
	if !r.take(callHistory) {
		return slack.HistoryPage{}, errBudget
	}
	return r.s.Slack.Replies(r.ctx, r.token, p)
}

// userName returns the display name of a member: the profile's display
// name, the real name or the user name, or the ID when Slack does not tell
// (or the budget is used up). Names are cached for the synchronization.
func (r *run) userName(id string) (string, error) {
	if id == "" {
		return "", nil
	}
	if id == r.conn.SlackUserID && r.conn.SlackUserName != "" {
		return r.conn.SlackUserName, nil
	}
	if v, ok := r.names[id]; ok {
		return v, nil
	}
	if !idPattern.MatchString(id) || !r.take(callUserInfo) {
		return id, nil
	}
	name := id
	u, err := r.s.Slack.UserInfo(r.ctx, r.token, id)
	switch {
	case err == nil:
		for _, v := range []string{u.DisplayName, u.RealName, u.Name} {
			if v = cleanText(v); v != "" {
				name = v
				break
			}
		}
	case stops(err):
		return "", err
	}
	r.names[id] = name
	return name, nil
}

// permalink returns the message's permalink from the search result, or
// asks chat.getPermalink. It is empty when neither gives a Slack URL.
func (r *run) permalink(channel string, m slack.Message) (string, error) {
	if v := slackURL(m.Permalink); v.Valid {
		return v.String, nil
	}
	if !r.take(callPermalink) {
		return "", nil
	}
	v, err := r.s.Slack.Permalink(r.ctx, r.token, channel, m.TS)
	if err != nil {
		if stops(err) {
			return "", err
		}
		return "", nil
	}
	return slackURL(v).String, nil
}

// location returns the time zone of the user's notification settings, in
// which the AI reads post times and writes dates. It falls back to UTC.
func (r *run) location() (*time.Location, error) {
	if r.loc != nil {
		return r.loc, nil
	}
	settings, err := r.s.Store.NotificationSettings(r.parent, r.conn.UserID)
	if err != nil {
		return nil, err
	}
	loc, err := time.LoadLocation(settings.Timezone)
	if err != nil {
		loc = time.UTC
	}
	r.loc = loc
	return loc, nil
}

// deadlineNear reports whether too little time is left for an AI call.
func (r *run) deadlineNear() bool {
	deadline, ok := r.ctx.Deadline()
	return ok && time.Until(deadline) < minRemaining
}

// aiGate is the state of the AI settings read again before a call.
type aiGate int

const (
	gateOK       aiGate = iota
	gateStopped         // deleted, or the key cannot be opened
	gateStale           // replaced during the synchronization
	gateInactive        // not active
)

// reloadAI reads the settings again right before an AI call, so that
// nothing is sent after they were deleted or replaced, and opens the key.
func (r *run) reloadAI() (store.AISettings, string, aiGate, error) {
	cur, err := r.s.Store.AISettings(r.parent, r.conn.UserID)
	switch {
	case errors.Is(err, store.ErrAISettingsNotFound):
		r.aiStopped = true
		return store.AISettings{}, "", gateStopped, nil
	case err != nil:
		return store.AISettings{}, "", gateStopped, err
	case cur.Generation != r.ai.Generation:
		r.aiStale = true
		return store.AISettings{}, "", gateStale, nil
	case cur.Status != store.AIActive:
		r.aiFallbackOnly = true
		return store.AISettings{}, "", gateInactive, nil
	}
	r.ai = &cur
	apiKey, err := r.s.Box.Open(ai.KeyPurpose(r.conn.UserID), cur.APIKeySealed)
	if err != nil {
		log.Printf("slack sync: user=%s: ai %s", r.conn.UserID, AIErrorKeyUndecryptable)
		r.setAIStatus(store.AIError, AIErrorKeyUndecryptable)
		r.aiStopped = true
		return store.AISettings{}, "", gateStopped, nil
	}
	return cur, apiKey, gateOK, nil
}

// aiClass is the class of an AI call's result.
type aiClass int

const (
	aiOK        aiClass = iota
	aiFailed            // key, billing or model: the AI stops for the user
	aiUnusable          // the request or the answer was rejected
	aiTemporary         // try again later
)

// classifyAI records the outcome of an AI call on the settings and returns
// its class. Context errors are returned as they are.
func (r *run) classifyAI(cur store.AISettings, err error) (aiClass, error) {
	if err != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		return aiTemporary, err
	}
	if err != nil {
		log.Printf("slack sync: user=%s: ai %v", r.conn.UserID, err)
	}
	switch {
	case err == nil:
		if cur.LastError.Valid {
			r.setAIStatus(store.AIActive, "")
		}
		return aiOK, nil
	case errors.Is(err, ai.ErrKeyInvalid):
		r.aiStopsWith(AIErrorKeyInvalid)
		return aiFailed, nil
	case errors.Is(err, ai.ErrBilling):
		r.aiStopsWith(AIErrorBilling)
		return aiFailed, nil
	case errors.Is(err, ai.ErrModelUnavailable):
		r.aiStopsWith(AIErrorModelUnavailable)
		return aiFailed, nil
	case errors.Is(err, ai.ErrUnusable):
		r.setAIStatus(store.AIActive, AIErrorRequestRejected)
		return aiUnusable, nil
	}
	return aiTemporary, nil
}

// aiStopsWith marks the settings as failing; the rest of the
// synchronization creates confirmation tasks instead.
func (r *run) aiStopsWith(code string) {
	r.setAIStatus(store.AIError, code)
	r.aiFallbackOnly = true
}

// setAIStatus records the AI status of the settings the synchronization
// read; settings saved again or deleted in the meantime are left alone.
// lastError "" clears the error.
func (r *run) setAIStatus(status, lastError string) {
	v := sql.NullString{String: lastError, Valid: lastError != ""}
	if _, err := r.s.Store.SetAIStatus(r.parent, r.conn.UserID, r.ai.Generation, status, v, r.s.Now().Unix()); err != nil && r.parent.Err() == nil {
		log.Printf("slack sync: user=%s: save ai status: %v", r.conn.UserID, err)
		return
	}
	r.ai.Status, r.ai.LastError = status, v
}

var (
	tsPattern = regexp.MustCompile(`^[0-9]{1,12}\.[0-9]{1,8}$`)
	idPattern = regexp.MustCompile(`^[A-Z0-9]{1,32}$`)
)

// validTS reports whether v is a message timestamp ("1712345678.123456").
func validTS(v string) bool { return tsPattern.MatchString(v) }

// tsSeconds returns the whole seconds of a timestamp.
func tsSeconds(ts string) int64 {
	sec, _, _ := strings.Cut(ts, ".")
	n, _ := strconv.ParseInt(sec, 10, 64)
	return n
}

// secondsTS returns the timestamp of a whole second.
func secondsTS(sec int64) string { return strconv.FormatInt(sec, 10) + ".000000" }

// compareTS compares two valid timestamps numerically.
func compareTS(a, b string) int {
	if x, y := tsSeconds(a), tsSeconds(b); x != y {
		if x < y {
			return -1
		}
		return 1
	}
	_, fa, _ := strings.Cut(a, ".")
	_, fb, _ := strings.Cut(b, ".")
	fa, fb = fa+strings.Repeat("0", 8-len(fa)), fb+strings.Repeat("0", 8-len(fb))
	return strings.Compare(fa, fb)
}

// tsTime returns the time of a timestamp, to the second.
func tsTime(ts string) time.Time { return time.Unix(tsSeconds(ts), 0) }

// Source key kinds.
const (
	kindMention = "m"
	kindDM      = "d"
)

// noThread is the thread part of a key outside threads.
const noThread = "-"

// sourceKey is the parsed form of a Slack item's source key:
// slack:{team}:m:{channel}:{thread}:{ts} for a mention, and
// slack:{team}:d:{channel}:{thread}:{first}:{last} for a run of direct
// messages. First and Last are equal for a mention.
type sourceKey struct {
	Team, Kind, Channel, Thread, First, Last string
}

func (k sourceKey) String() string {
	if k.Kind == kindMention {
		return conversationPrefix(k.Team, k.Kind, k.Channel, k.Thread) + k.Last
	}
	return conversationPrefix(k.Team, k.Kind, k.Channel, k.Thread) + k.First + ":" + k.Last
}

// conversationPrefix is the start of the keys of one conversation (a
// channel or a thread).
func conversationPrefix(team, kind, channel, thread string) string {
	return "slack:" + team + ":" + kind + ":" + channel + ":" + thread + ":"
}

// parseSourceKey is the inverse of sourceKey.String.
func parseSourceKey(v string) (sourceKey, bool) {
	parts := strings.Split(v, ":")
	if len(parts) < 6 || parts[0] != "slack" || !idPattern.MatchString(parts[1]) || !idPattern.MatchString(parts[3]) || (parts[4] != noThread && !validTS(parts[4])) {
		return sourceKey{}, false
	}
	k := sourceKey{Team: parts[1], Kind: parts[2], Channel: parts[3], Thread: parts[4]}
	switch {
	case k.Kind == kindMention && len(parts) == 6 && validTS(parts[5]):
		k.First, k.Last = parts[5], parts[5]
	case k.Kind == kindDM && len(parts) == 7 && validTS(parts[5]) && validTS(parts[6]) && compareTS(parts[5], parts[6]) <= 0:
		k.First, k.Last = parts[5], parts[6]
	default:
		return sourceKey{}, false
	}
	return k, true
}

// threadOf returns the thread of a message from conversations.history or
// conversations.replies: the parent's ts for a reply, noThread otherwise
// (the parent itself included).
func threadOf(m slack.Message) string {
	if m.ThreadTS != "" && m.ThreadTS != m.TS && validTS(m.ThreadTS) {
		return m.ThreadTS
	}
	return noThread
}

// searchThread returns the thread of a search result: its thread_ts when
// present, otherwise the thread_ts query of its permalink.
func searchThread(m slack.Message) string {
	if m.ThreadTS != "" {
		return threadOf(m)
	}
	u, err := url.Parse(m.Permalink)
	if err != nil {
		return noThread
	}
	m.ThreadTS = u.Query().Get("thread_ts")
	return threadOf(m)
}

// allowedSubtypes are the subtypes of messages people write; other
// subtypes (joins, topics, bots) are never detected.
var allowedSubtypes = map[string]bool{"": true, "thread_broadcast": true, "file_share": true}

// eligible reports whether m may be detected: not by the user, not by a
// bot and not a system message. Missing fields do not exclude it.
func (r *run) eligible(m slack.Message) bool {
	return m.User != r.conn.SlackUserID && !(m.HasBotID && m.BotID != "") && !(m.HasSubtype && !allowedSubtypes[m.Subtype])
}

// slackURL returns v when it is an https URL on a slack.com host, for refs
// and automatic completion evidence.
func slackURL(v string) sql.NullString {
	u, err := url.Parse(v)
	if err != nil || u.Scheme != "https" || len(v) > 2048 || strings.ContainsRune(v, 0) || u.User != nil {
		return sql.NullString{}
	}
	if host := strings.ToLower(u.Hostname()); !strings.HasSuffix(host, ".slack.com") {
		return sql.NullString{}
	}
	return sql.NullString{String: v, Valid: true}
}

var markupPattern = regexp.MustCompile(`<([^<>]*)>`)

// formatText turns Slack markup into plain text: <@U1> and <@U1|name>
// become @name(U1) with the member's display name, <#C1|name> becomes
// #name, <url|text> becomes "text (url)" and <url> the URL. HTML entities
// are decoded.
func (r *run) formatText(text string) (string, error) {
	var err error
	out := markupPattern.ReplaceAllStringFunc(text, func(m string) string {
		inner := m[1 : len(m)-1]
		target, label, hasLabel := strings.Cut(inner, "|")
		switch {
		case strings.HasPrefix(target, "@"):
			id := target[1:]
			name, e := r.userName(id)
			if e != nil {
				err = e
				return m
			}
			return "@" + name + "(" + id + ")"
		case strings.HasPrefix(target, "#"):
			if hasLabel && label != "" {
				return "#" + label
			}
			return target
		case strings.HasPrefix(target, "!"):
			if hasLabel && label != "" {
				return label
			}
			return "@" + strings.TrimPrefix(target, "!")
		case hasLabel && label != "":
			return label + " (" + target + ")"
		}
		return target
	})
	if err != nil {
		return "", err
	}
	return html.UnescapeString(out), nil
}

// cleanText removes control and format characters and surrounding spaces.
func cleanText(v string) string {
	v = strings.Map(func(r rune) rune {
		if unicode.In(r, unicode.Cc, unicode.Cf, unicode.Zl, unicode.Zp) {
			return -1
		}
		return r
	}, v)
	return strings.TrimFunc(v, unicode.IsSpace)
}

// excerpt returns the first n characters of a body on one line.
func excerpt(v string, n int) string {
	v = strings.Join(strings.FieldsFunc(v, func(r rune) bool { return r == '\n' || r == '\r' }), " ")
	return truncate(cleanText(v), n)
}

func truncate(v string, n int) string {
	if utf8.RuneCountInString(v) <= n {
		return v
	}
	return string([]rune(v)[:n])
}

func nullString(v string) sql.NullString { return sql.NullString{String: v, Valid: true} }
func nullInt(v int64) sql.NullInt64      { return sql.NullInt64{Int64: v, Valid: true} }

// warn logs a skipped, truncated or lost read. attrs carry only methods,
// error codes and counts, never message contents.
func (r *run) warn(msg string, attrs ...any) {
	slog.Warn("slack sync: "+msg, append([]any{"user", r.conn.UserID}, attrs...)...)
}
