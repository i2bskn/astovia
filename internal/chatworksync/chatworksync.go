// Package chatworksync finds Chatwork messages that may need something from
// the user (TO, replies and TO ALL in group chats, and direct chats), lets
// the AI judge them with the user's own key and creates action items, and
// completes the items when the AI judges that the user's later message dealt
// with them. It also imports the open Chatwork tasks assigned to the user,
// without the AI, and completes them when they leave the list. Without
// active AI settings only the tasks are imported. As in slacksync, requests
// to Chatwork and the AI are never made inside a database transaction, and
// nothing is ever written to Chatwork.
package chatworksync

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"log/slog"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/i2bskn/astovia/internal/ai"
	"github.com/i2bskn/astovia/internal/aicred"
	"github.com/i2bskn/astovia/internal/chatwork"
	"github.com/i2bskn/astovia/internal/secretbox"
	"github.com/i2bskn/astovia/internal/store"
)

const (
	// BatchSize is the number of due connections handled by one RunOnce.
	BatchSize = 20
	// connectionTimeout bounds the Chatwork requests of one connection's
	// synchronization; what is left is done in the next one.
	connectionTimeout = 60 * time.Second
	pollInterval      = 60
	trackInterval     = 5 * time.Minute
	taskInterval      = 5 * time.Minute
	rateLimitWait     = 15 * time.Minute
	maxBackoff        = 900
	// readTableIdle is how long the table of rooms read is kept for a
	// connection that is no longer synchronized.
	readTableIdle = 24 * time.Hour
)

// Error codes stored in chatwork_connections.last_error and shown in the UI.
const (
	ErrorTokenInvalid       = "token_invalid"
	ErrorTokenUndecryptable = "token_undecryptable"
	ErrorRateLimited        = "rate_limited"
	ErrorUnavailable        = "unavailable"
)

// Automatic completion reasons stored in action_items.auto_reason.
const (
	ReasonNoAction      = "chatwork.no_action"
	ReasonReplyResolved = "chatwork.reply_resolved"
	ReasonTaskClosed    = "chatwork.task_closed"
)

// Error codes stored in ai_settings.last_error by the synchronization. They
// are the same as slacksync's.
const (
	AIErrorKeyInvalid       = "key_invalid"
	AIErrorKeyUndecryptable = "key_undecryptable"
	AIErrorBilling          = "billing"
	AIErrorModelUnavailable = "model_unavailable"
	AIErrorRequestRejected  = "request_rejected"
	// AIErrorUsageLimit is the usage limit of the ChatGPT plan; the status
	// stays active.
	AIErrorUsageLimit = aicred.UsageLimit
)

// TokenPurpose is the secretbox purpose of a user's Chatwork API token.
func TokenPurpose(userID string) string { return "chatwork-token:" + userID }

// Kinds of Chatwork API calls counted by the per-synchronization budget.
const (
	callRooms    = "rooms"
	callMessages = "messages"
	callTasks    = "my/tasks"
)

// callLimits bound the Chatwork calls of one synchronization: at most 22 a
// minute, 110 in five minutes, well below the documented 300.
// detectMessagesLimit and trackMessagesLimit split callMessages between
// detection and tracking.
var callLimits = map[string]int{callRooms: 1, callMessages: 20, callTasks: 1}

const (
	detectMessagesLimit = 10
	trackMessagesLimit  = 10
)

// errBudget is returned instead of calling Chatwork when the call budget of
// the synchronization is used up. It never stops the synchronization.
var errBudget = errors.New("chatworksync: call budget exhausted")

// Syncer synchronizes Chatwork connections. It assumes a single process:
// the rooms read are remembered in memory.
type Syncer struct {
	Store    *store.Store
	Chatwork *chatwork.Client
	AI       *ai.Client
	Box      *secretbox.Box
	// Tokens manages the access tokens of the ChatGPT plan; it is shared
	// with the HTTP server.
	Tokens *aicred.Tokens
	Now    func() time.Time

	mu sync.Mutex
	// read maps a user to the rooms read to the end, with the
	// last_update_time they had then. It is lost on restart, which only
	// makes the rooms read again.
	read map[string]*readTable
}

// readTable is the rooms read of one connection generation.
type readTable struct {
	generation int64
	used       time.Time
	rooms      map[string]int64
}

// RunOnce synchronizes up to BatchSize due connections, one after another.
func (s *Syncer) RunOnce(ctx context.Context) {
	s.pruneRead(s.Now())
	conns, err := s.Store.DueChatworkConnections(ctx, s.Now().Unix(), BatchSize)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("chatwork sync: list connections: %v", err)
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

// pruneRead drops the tables of connections not synchronized for
// readTableIdle (deleted connections and disabled users).
func (s *Syncer) pruneRead(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for user, t := range s.read {
		if now.Sub(t.used) >= readTableIdle {
			delete(s.read, user)
		}
	}
}

// table returns the rooms read of the connection, replacing the table of
// another generation. The caller holds s.mu.
func (s *Syncer) table(c store.ChatworkConnection, now time.Time) *readTable {
	if s.read == nil {
		s.read = map[string]*readTable{}
	}
	t, ok := s.read[c.UserID]
	if !ok || t.generation != c.Generation {
		t = &readTable{generation: c.Generation, rooms: map[string]int64{}}
		s.read[c.UserID] = t
	}
	t.used = now
	return t
}

// roomRead returns the last_update_time the room had when it was last read
// to the end.
func (s *Syncer) roomRead(c store.ChatworkConnection, room string, now time.Time) (int64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.table(c, now).rooms[room]
	return v, ok
}

// markRead records that the room was read to the end at lastUpdate.
func (s *Syncer) markRead(c store.ChatworkConnection, room string, lastUpdate int64, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.table(c, now).rooms[room] = lastUpdate
}

// run is the state of one connection's synchronization. ctx carries the
// connection deadline and is used for Chatwork and AI requests; parent is
// used for database reads and writes, so that they still happen after the
// deadline.
type run struct {
	s      *Syncer
	ctx    context.Context
	parent context.Context
	conn   store.ChatworkConnection
	state  store.ChatworkConnection
	token  string
	start  time.Time

	// calls counts the Chatwork calls per kind; detectMessages and
	// trackMessages the message reads of detection and tracking. tracking
	// is set while tracking runs.
	calls          map[string]int
	detectMessages int
	trackMessages  int
	tracking       bool
	// rooms caches the messages of the rooms read, shared by tracking and
	// detection.
	rooms map[string]*roomCache
	// loc is the user's time zone, read once when needed.
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

	// The detection window [from, to) and its state: overdue when it has
	// stayed behind for longer than retryLimit; pending when something was
	// carried over to the next synchronization (carried counts them); lost
	// counts what an overdue synchronization gives up.
	from, to int64
	overdue  bool
	pending  bool
	carried  int
	lost     int
}

// roomCache is the latest messages of a room in message ID order, the
// names of their senders by account ID, and Raw as returned by Messages.
// err is the error of a room that cannot be read.
type roomCache struct {
	messages []chatwork.Message
	names    map[string]string
	raw      int
	err      error
}

// Sync synchronizes one connection and stores its new state. When the
// connection changed in the meantime (another token, deleted, user disabled)
// the results are discarded.
func (s *Syncer) Sync(parent context.Context, c store.ChatworkConnection) {
	r := &run{s: s, parent: parent, conn: c, state: c, start: s.Now(), calls: map[string]int{}, rooms: map[string]*roomCache{}}
	token, err := s.Box.Open(TokenPurpose(c.UserID), c.TokenSealed)
	if err != nil {
		r.state.Status, r.state.LastError = store.ChatworkError, nullString(ErrorTokenUndecryptable)
		log.Printf("chatwork sync: user=%s: %s", c.UserID, ErrorTokenUndecryptable)
		r.save()
		return
	}
	r.token = token
	ctx, cancel := context.WithTimeout(parent, connectionTimeout)
	defer cancel()
	r.ctx = ctx
	err = r.steps()
	now := s.Now().Unix()
	var rate *chatwork.RateLimitError
	switch {
	case errors.Is(err, store.ErrStaleConnection) || parent.Err() != nil:
		return
	case err == nil || ctx.Err() != nil:
		// Running out of time is not a failure; the rest waits for the
		// next poll.
		r.state.FailureCount, r.state.LastError, r.state.NextPollAt = 0, sql.NullString{}, now+pollInterval
	case errors.Is(err, chatwork.ErrUnauthorized):
		r.state.Status, r.state.LastError = store.ChatworkError, nullString(ErrorTokenInvalid)
		log.Printf("chatwork sync: user=%s: %s", c.UserID, ErrorTokenInvalid)
	case errors.As(err, &rate):
		r.state.LastError = nullString(ErrorRateLimited)
		r.state.NextPollAt = now + int64(rateLimitWait/time.Second)
		if !rate.RetryAt.IsZero() {
			r.state.NextPollAt = rate.RetryAt.Unix()
		}
		log.Printf("chatwork sync: user=%s: %s", c.UserID, ErrorRateLimited)
	default:
		r.state.FailureCount++
		r.state.LastError = nullString(ErrorUnavailable)
		r.state.NextPollAt = now + backoff(r.state.FailureCount)
		log.Printf("chatwork sync: user=%s: %s", c.UserID, ErrorUnavailable)
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
	err := r.s.Store.ChatworkSync(r.parent, r.conn.UserID, r.conn.Generation, func(tx *store.ChatworkTx) error {
		return tx.SaveState(r.state)
	})
	if err != nil && !errors.Is(err, store.ErrStaleConnection) && r.parent.Err() == nil {
		log.Printf("chatwork sync: user=%s: save state: %v", r.conn.UserID, err)
	}
}

// steps imports the tasks (every taskInterval, with or without the AI),
// then runs tracking (every trackInterval) and detection, recording
// progress in r.state. Without active AI settings nothing is detected, and
// the detection window does not move.
func (r *run) steps() error {
	if r.conn.ImportTasks == 1 && due(r.state.LastTasksAt, r.start.Unix(), taskInterval) {
		if err := r.importTasks(); err != nil {
			return err
		}
	}
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
// about the object asked for (a *chatwork.APIError: 403, 404 or a response
// too large) and the call budget leave it running.
func stops(err error) bool {
	var api *chatwork.APIError
	return !errors.As(err, &api) && !errors.Is(err, errBudget)
}

// apiCode describes a non-stopping error for logs: the method and the
// status, never the room.
func apiCode(err error) string {
	var api *chatwork.APIError
	switch {
	case errors.As(err, &api) && api.TooLarge:
		return api.Method + " too_large"
	case errors.As(err, &api):
		return api.Method + " " + strconv.Itoa(api.Status)
	}
	return "budget"
}

// take counts one call of the kind, or reports false when the budget of the
// synchronization (or of detection or tracking) is used up.
func (r *run) take(kind string) bool {
	if r.calls[kind] >= callLimits[kind] {
		return false
	}
	if kind == callMessages {
		if r.tracking {
			if r.trackMessages >= trackMessagesLimit {
				return false
			}
			r.trackMessages++
		} else {
			if r.detectMessages >= detectMessagesLimit {
				return false
			}
			r.detectMessages++
		}
	}
	r.calls[kind]++
	return true
}

// roomMessages returns the latest messages of a room, read once per
// synchronization. A room that cannot be read returns its
// *chatwork.APIError every time; errBudget is returned without reading
// when the budget is used up.
func (r *run) roomMessages(room string) (*roomCache, error) {
	if c, ok := r.rooms[room]; ok {
		return c, c.err
	}
	if !r.take(callMessages) {
		return nil, errBudget
	}
	page, err := r.s.Chatwork.Messages(r.ctx, r.token, room)
	if err != nil {
		if !stops(err) {
			r.rooms[room] = &roomCache{err: err}
		}
		return nil, err
	}
	c := &roomCache{messages: sortMessages(page.Messages), names: map[string]string{}, raw: page.Raw}
	for _, m := range c.messages {
		if name := cleanText(m.AccountName); m.AccountID != "" && name != "" {
			c.names[m.AccountID] = name
		}
	}
	r.rooms[room] = c
	return c, nil
}

// sortMessages returns the messages in message ID order, keeping the first
// of each ID.
func sortMessages(list []chatwork.Message) []chatwork.Message {
	seen := map[string]bool{}
	out := make([]chatwork.Message, 0, len(list))
	for _, m := range list {
		if !chatwork.ValidMessageID(m.ID) || seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		out = append(out, m)
	}
	sort.SliceStable(out, func(i, j int) bool { return chatwork.CompareIDs(out[i].ID, out[j].ID) < 0 })
	return out
}

func indexID(list []chatwork.Message, id string) int {
	for i, m := range list {
		if m.ID == id {
			return i
		}
	}
	return -1
}

// names returns the names known in a room: its senders and the user.
func (r *run) names(c *roomCache) map[string]string {
	out := map[string]string{}
	if c != nil {
		for k, v := range c.names {
			out[k] = v
		}
	}
	if name := cleanText(r.conn.AccountName); name != "" {
		out[r.conn.AccountID] = name
	}
	return out
}

// location returns the time zone of the user's notification settings, in
// which the AI reads post times and task dates are read. It falls back to
// UTC.
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
// nothing is sent after they were deleted or replaced, and opens the
// credential. While the usage limit of the ChatGPT plan is cooling down the
// settings are handled as replaced: the AI is not called.
func (r *run) reloadAI() (store.AISettings, ai.Credential, aiGate, error) {
	cur, err := r.s.Store.AISettings(r.parent, r.conn.UserID)
	switch {
	case errors.Is(err, store.ErrAISettingsNotFound):
		r.aiStopped = true
		return store.AISettings{}, ai.Credential{}, gateStopped, nil
	case err != nil:
		return store.AISettings{}, ai.Credential{}, gateStopped, err
	case cur.Generation != r.ai.Generation:
		r.aiStale = true
		return store.AISettings{}, ai.Credential{}, gateStale, nil
	case cur.Status != store.AIActive:
		r.aiFallbackOnly = true
		return store.AISettings{}, ai.Credential{}, gateInactive, nil
	case aicred.CoolingDown(cur, r.s.Now()):
		r.aiStale = true
		return store.AISettings{}, ai.Credential{}, gateStale, nil
	}
	r.ai = &cur
	cred, err := aicred.Open(r.s.Box, r.s.Tokens, cur)
	if err != nil {
		log.Printf("chatwork sync: user=%s: ai %s", r.conn.UserID, AIErrorKeyUndecryptable)
		r.setAIStatus(store.AIError, AIErrorKeyUndecryptable)
		r.aiStopped = true
		return store.AISettings{}, ai.Credential{}, gateStopped, nil
	}
	return cur, cred, gateOK, nil
}

// aiClass is the class of an AI call's result.
type aiClass int

const (
	aiOK        aiClass = iota
	aiFailed            // key, billing or model: the AI stops for the user
	aiUnusable          // the request or the answer was rejected
	aiTemporary         // try again later
	aiStaled            // the settings changed while getting a token
)

// classifyAI records the outcome of an AI call on the settings and returns
// its class. Context errors are returned as they are.
func (r *run) classifyAI(cur store.AISettings, err error) (aiClass, error) {
	if err != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		return aiTemporary, err
	}
	// Settings replaced or removed while the token was obtained are
	// handled like settings replaced before the call.
	if errors.Is(err, aicred.ErrStale) {
		r.aiStale = true
		return aiStaled, nil
	}
	if err != nil {
		log.Printf("chatwork sync: user=%s: ai %v", r.conn.UserID, err)
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
	case errors.Is(err, ai.ErrUsageLimit):
		// Recorded so that the next calls wait (aicred.CoolingDown); this
		// one is handled like a temporary failure.
		r.setAIStatus(store.AIActive, AIErrorUsageLimit)
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
		log.Printf("chatwork sync: user=%s: save ai status: %v", r.conn.UserID, err)
		return
	}
	r.ai.Status, r.ai.LastError = status, v
}

// Source key kinds. A group parent made by the AI for several tasks of one
// matter has the key chatwork:{account}:g:{room}:{item}, where item is the
// ID of the task it was made for, and an imported Chatwork task has
// chatwork:{account}:t:{task}; parseSourceKey rejects both, so that
// detection and tracking never take them for messages.
const (
	kindMention = "m"
	kindDM      = "d"
	kindGroup   = "g"
	kindTask    = "t"
)

// sourceKey is the parsed form of a Chatwork item's source key:
// chatwork:{account}:m:{room}:{message} for a TO, a reply or a TO ALL, and
// chatwork:{account}:d:{room}:{first}:{last} for a run of direct chat
// messages. Account is the user's own account ID. First and Last are equal
// for a mention.
type sourceKey struct {
	Account, Kind, Room, First, Last string
}

func (k sourceKey) String() string {
	if k.Kind == kindMention {
		return roomPrefix(k.Account, k.Kind, k.Room) + k.Last
	}
	return roomPrefix(k.Account, k.Kind, k.Room) + k.First + ":" + k.Last
}

// roomPrefix is the start of the keys of one kind in a room.
func roomPrefix(account, kind, room string) string {
	return "chatwork:" + account + ":" + kind + ":" + room + ":"
}

// taskPrefix is the start of the keys of the imported tasks.
func taskPrefix(account string) string {
	return "chatwork:" + account + ":" + kindTask + ":"
}

// parseSourceKey is the inverse of sourceKey.String.
func parseSourceKey(v string) (sourceKey, bool) {
	parts := strings.Split(v, ":")
	if len(parts) < 5 || parts[0] != "chatwork" || !chatwork.ValidID(parts[1]) || !chatwork.ValidID(parts[3]) {
		return sourceKey{}, false
	}
	k := sourceKey{Account: parts[1], Kind: parts[2], Room: parts[3]}
	switch {
	case k.Kind == kindMention && len(parts) == 5 && chatwork.ValidMessageID(parts[4]):
		k.First, k.Last = parts[4], parts[4]
	case k.Kind == kindDM && len(parts) == 6 && chatwork.ValidMessageID(parts[4]) && chatwork.ValidMessageID(parts[5]) && chatwork.CompareIDs(parts[4], parts[5]) <= 0:
		k.First, k.Last = parts[4], parts[5]
	default:
		return sourceKey{}, false
	}
	return k, true
}

// messageURL links to a message, and roomURL to a room.
func messageURL(room, message string) string { return chatwork.MessageURL(room, message) }

func roomURL(room string) string { return chatwork.RoomURL(room) }

// chatworkURL returns v when it is an https URL on www.chatwork.com, for
// refs and automatic completion evidence.
func chatworkURL(v string) sql.NullString {
	u, err := url.Parse(v)
	if err != nil || u.Scheme != "https" || u.Host != "www.chatwork.com" || len(v) > 2048 || strings.ContainsRune(v, 0) || u.User != nil {
		return sql.NullString{}
	}
	return sql.NullString{String: v, Valid: true}
}

// system reports whether a body is a message Chatwork wrote (joins, room
// changes and the like).
func system(body string) bool {
	v := strings.TrimSpace(body)
	return strings.HasPrefix(v, "[dtext:") || strings.HasPrefix(v, "[info][title][dtext:")
}

// stripQuotes removes the quotes of a body: from each [qt] to the first
// [/qt] after it, or to the end when it is not closed. Nesting is not
// considered.
func stripQuotes(v string) string {
	var b strings.Builder
	for {
		i := strings.Index(v, "[qt]")
		if i < 0 {
			b.WriteString(v)
			return b.String()
		}
		b.WriteString(v[:i])
		j := strings.Index(v[i:], "[/qt]")
		if j < 0 {
			return b.String()
		}
		v = v[i+j+len("[/qt]"):]
	}
}

// addressed reports whether a body outside its quotes is addressed to the
// account: a TO, a reply or a TO ALL.
func addressed(body, account string) bool {
	v := stripQuotes(body)
	return strings.Contains(v, "[To:"+account+"]") || strings.Contains(v, "[rp aid="+account+" ") || strings.Contains(v, "[rp aid="+account+"]") || strings.Contains(v, "[toall]")
}

// hasTask reports whether a body outside its quotes adds a Chatwork task.
func hasTask(body string) bool { return strings.Contains(stripQuotes(body), "[task ") }

var (
	quotePattern    = regexp.MustCompile(`(?s)\[qt\](?:\[qtmeta aid=([0-9]+)(?: time=[0-9]+)?\])?(.*?)\[/qt\]`)
	taskPattern     = regexp.MustCompile(`(?s)\[task(?: [^\]]*)?\](.*?)\[/task\]`)
	downloadPattern = regexp.MustCompile(`(?s)\[download:[0-9]+\](.*?)\[/download\]`)
	tagPattern      = regexp.MustCompile(`\[(To:[0-9]+|rp aid=[0-9]+(?: to=[0-9]+-[0-9]+)?|toall|pname:[0-9]+|piconname:[0-9]+|picon:[0-9]+|info|/info|title|/title|hr|code|/code|url|/url|dtext:[^\]]*|preview [^\]]*)\]`)
	idDigits        = regexp.MustCompile(`[0-9]+`)
)

// formatText turns Chatwork notation into plain text for the AI and the
// confirmation tasks: [To:id] becomes "@name(id) ", a reply "返信
// @name(id) ", [toall] "@all ", quotes "引用（name）: ..." ("引用: ..." when
// the author is unknown), tasks "タスク:
// ..." and files "ファイル: ...". names maps account IDs to names; an
// unknown ID stands for its name. Decoration tags are dropped and other
// brackets are kept. Control characters other than newlines and tabs are
// removed.
func formatText(body string, names map[string]string) string {
	name := func(id string) string {
		if v := names[id]; v != "" {
			return v
		}
		return id
	}
	out := quotePattern.ReplaceAllStringFunc(body, func(m string) string {
		g := quotePattern.FindStringSubmatch(m)
		// A quote of an unknown author is not labeled with its ID.
		if v := names[g[1]]; g[1] != "" && v != "" {
			return "引用（" + v + "）: " + g[2]
		}
		return "引用: " + g[2]
	})
	out = taskPattern.ReplaceAllString(out, "タスク: $1")
	out = downloadPattern.ReplaceAllString(out, "ファイル: $1")
	out = tagPattern.ReplaceAllStringFunc(out, func(m string) string {
		tag := m[1 : len(m)-1]
		id := idDigits.FindString(tag)
		switch {
		case strings.HasPrefix(tag, "To:"):
			return "@" + name(id) + "(" + id + ") "
		case strings.HasPrefix(tag, "rp "):
			return "返信 @" + name(id) + "(" + id + ") "
		case tag == "toall":
			return "@all "
		case strings.HasPrefix(tag, "pname:"), strings.HasPrefix(tag, "piconname:"):
			return name(id)
		case tag == "hr":
			return "----"
		case tag == "info", tag == "/info", tag == "/title", tag == "/code":
			return "\n"
		}
		// [picon:], [title], [code], [url], [/url], [dtext:] and
		// [preview] leave nothing.
		return ""
	})
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if unicode.In(r, unicode.Cc, unicode.Cf, unicode.Zl, unicode.Zp) {
			return -1
		}
		return r
	}, out)
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
// statuses and counts, never message contents, room names or people.
func (r *run) warn(msg string, attrs ...any) {
	slog.Warn("chatwork sync: "+msg, append([]any{"user", r.conn.UserID}, attrs...)...)
}
