// Package gmailsync finds mail in the inbox of the user's Google accounts
// (Gmail and Google Workspace) that may need something from the user, lets
// the AI judge each thread with the user's own key and creates action
// items, and completes the items when the AI judges that the user's later
// message in the thread dealt with them. Without active AI settings nothing
// is read. As in slacksync, requests to Google and the AI are never made
// inside a database transaction, and nothing is ever written to Gmail.
package gmailsync

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"log/slog"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/i2bskn/astovia/internal/ai"
	"github.com/i2bskn/astovia/internal/aicred"
	"github.com/i2bskn/astovia/internal/config"
	"github.com/i2bskn/astovia/internal/google"
	"github.com/i2bskn/astovia/internal/secretbox"
	"github.com/i2bskn/astovia/internal/store"
)

const (
	// BatchSize is the number of due accounts handled by one RunOnce.
	BatchSize = 20
	// accountTimeout bounds the Google requests of one account's
	// synchronization; what is left is done in the next one.
	accountTimeout = 60 * time.Second
	pollInterval   = 60
	trackInterval  = 5 * time.Minute
	rateLimitWait  = 15 * time.Minute
	maxBackoff     = 900
	// tableIdle is how long the access token of an account that is no
	// longer synchronized is kept in memory.
	tableIdle = 24 * time.Hour
	// tokenMargin is how long before its expiry an access token is
	// refreshed.
	tokenMargin = 2 * time.Minute
)

// Error codes stored in gmail_accounts.last_error and shown in the UI.
const (
	ErrorTokenInvalid       = "token_invalid"
	ErrorTokenUndecryptable = "token_undecryptable"
	ErrorScopesMissing      = "scopes_missing"
	ErrorPolicyBlocked      = "policy_blocked"
	ErrorAPIDisabled        = "api_disabled"
	ErrorClientInvalid      = "client_invalid"
	ErrorRateLimited        = "rate_limited"
	ErrorUnavailable        = "unavailable"
)

// Automatic completion reasons stored in action_items.auto_reason.
const (
	ReasonNoAction      = "gmail.no_action"
	ReasonReplyResolved = "gmail.reply_resolved"
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

// TokenPurpose is the secretbox purpose of the refresh token of a user's
// Google account.
func TokenPurpose(userID, accountID string) string { return "gmail-token:" + userID + ":" + accountID }

// Kinds of Google calls counted by the per-synchronization budget.
const (
	callList    = "list"
	callThreads = "threads"
	callToken   = "token"
)

// callLimits bound the Google calls of one synchronization. The list pages
// are fewer when the window is short (listShortLimit); detectThreadsLimit
// and trackThreadsLimit split callThreads between detection and tracking.
// A retry after a 401 counts as the same call; its refresh uses callToken.
var callLimits = map[string]int{callList: 10, callThreads: 20, callToken: 2}

const (
	listShortLimit     = 3
	shortWindow        = 2 * time.Hour
	detectThreadsLimit = 12
	trackThreadsLimit  = 8
)

// errBudget is returned instead of calling Google when the call budget of
// the synchronization is used up. It never stops the synchronization.
var errBudget = errors.New("gmailsync: call budget exhausted")

// errTokenInvalid is a request refused with 401 again after the access
// token was refreshed.
var errTokenInvalid = errors.New("gmailsync: token refused after refresh")

// Syncer synchronizes the connected Google accounts. It assumes a single
// process: access tokens and the messages handled are remembered in memory.
type Syncer struct {
	Store  *store.Store
	Google *google.Client
	// Client is the OAuth client of the instance; nil disables the
	// synchronization.
	Client *config.GoogleClient
	AI     *ai.Client
	Box    *secretbox.Box
	// Tokens manages the access tokens of the ChatGPT plan; it is shared
	// with the HTTP server.
	Tokens *aicred.Tokens
	Now    func() time.Time

	mu sync.Mutex
	// tokens maps an account ID to its access token. It is lost on
	// restart, which only makes the token refreshed again.
	tokens map[string]*accessToken
	// handled maps an account ID to the listed messages that need not be
	// read again.
	handled map[string]*handledTable
}

// accessToken is the access token of one account generation.
type accessToken struct {
	generation int64
	token      string
	expiry     time.Time
	used       time.Time
}

// handledTable is the listed messages handled for one account generation:
// a message ID and the Unix time after which the entry is dropped.
type handledTable struct {
	generation int64
	ids        map[string]int64
}

// RunOnce synchronizes up to BatchSize due accounts, one after another.
// Without an OAuth client nothing is done.
func (s *Syncer) RunOnce(ctx context.Context) {
	if s.Client == nil || s.Google == nil {
		return
	}
	s.prune(s.Now())
	accounts, err := s.Store.DueGmailAccounts(ctx, s.Now().Unix(), BatchSize)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("gmail sync: list accounts: %v", err)
		}
		return
	}
	for _, a := range accounts {
		if ctx.Err() != nil {
			return
		}
		s.Sync(ctx, a)
	}
}

// prune drops the access tokens not used for tableIdle and the handled
// messages whose time has passed.
func (s *Syncer) prune(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, t := range s.tokens {
		if now.Sub(t.used) >= tableIdle {
			delete(s.tokens, id)
		}
	}
	for id, t := range s.handled {
		for msg, until := range t.ids {
			if until <= now.Unix() {
				delete(t.ids, msg)
			}
		}
		if len(t.ids) == 0 {
			delete(s.handled, id)
		}
	}
}

// cachedToken returns the access token of the account generation.
func (s *Syncer) cachedToken(a store.GmailAccount, now time.Time) (accessToken, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tokens[a.ID]
	if !ok || t.generation != a.Generation {
		return accessToken{}, false
	}
	t.used = now
	return *t, true
}

func (s *Syncer) storeToken(a store.GmailAccount, token string, expiry, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tokens == nil {
		s.tokens = map[string]*accessToken{}
	}
	s.tokens[a.ID] = &accessToken{generation: a.Generation, token: token, expiry: expiry, used: now}
}

func (s *Syncer) dropToken(a store.GmailAccount) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.tokens, a.ID)
}

// table returns the handled messages of the account generation, replacing
// the table of another generation. The caller holds s.mu.
func (s *Syncer) table(a store.GmailAccount) *handledTable {
	if s.handled == nil {
		s.handled = map[string]*handledTable{}
	}
	t, ok := s.handled[a.ID]
	if !ok || t.generation != a.Generation {
		t = &handledTable{generation: a.Generation, ids: map[string]int64{}}
		s.handled[a.ID] = t
	}
	return t
}

// allHandled reports whether every listed message ID is in the table.
func (s *Syncer) allHandled(a store.GmailAccount, ids []string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.table(a)
	for _, id := range ids {
		if _, ok := t.ids[id]; !ok {
			return false
		}
	}
	return true
}

// markHandled records listed messages as handled until the given times.
func (s *Syncer) markHandled(a store.GmailAccount, until map[string]int64) {
	if len(until) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.table(a)
	for id, v := range until {
		t.ids[id] = v
	}
}

// run is the state of one account's synchronization. ctx carries the
// account deadline and is used for Google and AI requests; parent is used
// for database reads and writes, so that they still happen after the
// deadline.
type run struct {
	s       *Syncer
	ctx     context.Context
	parent  context.Context
	acct    store.GmailAccount
	state   store.GmailAccount
	refresh string
	start   time.Time

	// calls counts the Google calls per kind; detectThreads and
	// trackThreads the thread reads of detection and tracking. tracking is
	// set while tracking runs. listLimit is the list pages of this
	// synchronization.
	calls         map[string]int
	detectThreads int
	trackThreads  int
	tracking      bool
	listLimit     int
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

// Sync synchronizes one account and stores its new state. When the account
// changed in the meantime (another token, deleted, user disabled) the
// results are discarded. Without an OAuth client nothing is done.
func (s *Syncer) Sync(parent context.Context, a store.GmailAccount) {
	if s.Client == nil || s.Google == nil {
		return
	}
	r := &run{s: s, parent: parent, acct: a, state: a, start: s.Now(), calls: map[string]int{}}
	refresh, err := s.Box.Open(TokenPurpose(a.UserID, a.ID), a.TokenSealed)
	if err != nil {
		r.state.Status, r.state.LastError = store.GmailError, nullString(ErrorTokenUndecryptable)
		r.logCode(ErrorTokenUndecryptable)
		r.save()
		return
	}
	r.refresh = refresh
	ctx, cancel := context.WithTimeout(parent, accountTimeout)
	defer cancel()
	r.ctx = ctx
	err = r.steps()
	now := s.Now().Unix()
	var rate *google.RateLimitError
	switch {
	case errors.Is(err, store.ErrStaleConnection) || parent.Err() != nil:
		return
	case err == nil || ctx.Err() != nil:
		// Running out of time is not a failure; the rest waits for the
		// next poll.
		r.state.FailureCount, r.state.LastError, r.state.NextPollAt = 0, sql.NullString{}, now+pollInterval
	case errors.Is(err, google.ErrInvalidGrant), errors.Is(err, errTokenInvalid), errors.Is(err, google.ErrUnauthorized):
		s.dropToken(a)
		r.stop(ErrorTokenInvalid)
	case errors.Is(err, google.ErrInsufficientScope):
		r.stop(ErrorScopesMissing)
	case errors.Is(err, google.ErrPolicyBlocked):
		s.dropToken(a)
		r.stop(ErrorPolicyBlocked)
	case errors.Is(err, google.ErrAPIDisabled):
		r.backoff(ErrorAPIDisabled, now)
	case errors.Is(err, google.ErrInvalidClient):
		s.dropToken(a)
		r.backoff(ErrorClientInvalid, now)
	case errors.As(err, &rate):
		r.state.LastError = nullString(ErrorRateLimited)
		r.state.NextPollAt = now + int64(rateLimitWait/time.Second)
		if !rate.RetryAt.IsZero() {
			r.state.NextPollAt = rate.RetryAt.Unix()
		}
		r.logCode(ErrorRateLimited)
	default:
		r.backoff(ErrorUnavailable, now)
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			log.Printf("gmail sync: user=%s account=%s: %v", a.UserID, a.ID, err)
		}
	}
	r.save()
}

// stop marks the account as failing until it is connected again.
func (r *run) stop(code string) {
	r.state.Status, r.state.LastError = store.GmailError, nullString(code)
	r.logCode(code)
}

// backoff keeps the account active and waits longer after each failure.
func (r *run) backoff(code string, now int64) {
	r.state.FailureCount++
	r.state.LastError = nullString(code)
	r.state.NextPollAt = now + backoff(r.state.FailureCount)
	r.logCode(code)
}

func (r *run) logCode(code string) {
	log.Printf("gmail sync: user=%s account=%s: %s", r.acct.UserID, r.acct.ID, code)
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
	err := r.s.Store.GmailSync(r.parent, r.acct.UserID, r.acct.ID, r.acct.Generation, func(tx *store.GmailTx) error {
		return tx.SaveState(r.state)
	})
	if err != nil && !errors.Is(err, store.ErrStaleConnection) && r.parent.Err() == nil {
		log.Printf("gmail sync: user=%s account=%s: save state: %v", r.acct.UserID, r.acct.ID, err)
	}
}

// steps runs tracking (every trackInterval) and detection, recording
// progress in r.state. Without active AI settings nothing is read, and the
// detection window does not move.
func (r *run) steps() error {
	if r.s.AI == nil {
		return nil
	}
	a, err := r.s.Store.AISettings(r.parent, r.acct.UserID)
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
// about the object asked for (a *google.APIError: 404, 400 or a response
// too large) and the call budget leave it running.
func stops(err error) bool {
	var api *google.APIError
	return !errors.As(err, &api) && !errors.Is(err, errBudget)
}

// apiCode describes a non-stopping error for logs: the method and the
// status, never the thread.
func apiCode(err error) string {
	var api *google.APIError
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
	limit := callLimits[kind]
	if kind == callList && r.listLimit > 0 {
		limit = r.listLimit
	}
	if r.calls[kind] >= limit {
		return false
	}
	if kind == callThreads {
		if r.tracking {
			if r.trackThreads >= trackThreadsLimit {
				return false
			}
			r.trackThreads++
		} else {
			if r.detectThreads >= detectThreadsLimit {
				return false
			}
			r.detectThreads++
		}
	}
	r.calls[kind]++
	return true
}

// accessToken returns an access token of the account: the one in memory
// until tokenMargin before it expires, otherwise a refreshed one.
func (r *run) accessToken() (string, error) {
	now := r.s.Now()
	if t, ok := r.s.cachedToken(r.acct, now); ok && now.Before(t.expiry.Add(-tokenMargin)) {
		return t.token, nil
	}
	return r.refreshToken()
}

// refreshToken gets a new access token, within the token budget. A refresh
// token replaced by Google is stored at once.
func (r *run) refreshToken() (string, error) {
	if !r.take(callToken) {
		return "", errBudget
	}
	tok, err := r.s.Google.Refresh(r.ctx, r.s.Client.ID, r.s.Client.Secret, r.refresh)
	if err != nil {
		return "", err
	}
	if tok.RefreshToken != "" && tok.RefreshToken != r.refresh {
		sealed, err := r.s.Box.Seal(TokenPurpose(r.acct.UserID, r.acct.ID), tok.RefreshToken)
		if err != nil {
			return "", err
		}
		if err = r.s.Store.GmailSync(r.parent, r.acct.UserID, r.acct.ID, r.acct.Generation, func(tx *store.GmailTx) error {
			return tx.UpdateGmailToken(sealed)
		}); err != nil {
			return "", err
		}
		r.refresh = tok.RefreshToken
	}
	r.s.storeToken(r.acct, tok.AccessToken, tok.Expiry, r.s.Now())
	return tok.AccessToken, nil
}

// call makes one Gmail request of the kind with an access token. A 401 is
// retried once with a refreshed token (counted as the same call); a second
// 401 is errTokenInvalid.
func (r *run) call(kind string, fn func(token string) error) error {
	if !r.take(kind) {
		return errBudget
	}
	token, err := r.accessToken()
	if err != nil {
		return err
	}
	err = fn(token)
	if !errors.Is(err, google.ErrUnauthorized) {
		return err
	}
	r.s.dropToken(r.acct)
	if token, err = r.refreshToken(); err != nil {
		return err
	}
	if err = fn(token); errors.Is(err, google.ErrUnauthorized) {
		return errTokenInvalid
	}
	return err
}

// thread reads a thread within the budget.
func (r *run) thread(id string) (google.Thread, error) {
	var t google.Thread
	err := r.call(callThreads, func(token string) error {
		var e error
		t, e = r.s.Google.Thread(r.ctx, token, id)
		return e
	})
	return t, err
}

// location returns the time zone of the user's notification settings, in
// which the AI reads post times. It falls back to UTC.
func (r *run) location() (*time.Location, error) {
	if r.loc != nil {
		return r.loc, nil
	}
	settings, err := r.s.Store.NotificationSettings(r.parent, r.acct.UserID)
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
	cur, err := r.s.Store.AISettings(r.parent, r.acct.UserID)
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
		log.Printf("gmail sync: user=%s account=%s: ai %s", r.acct.UserID, r.acct.ID, AIErrorKeyUndecryptable)
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
		log.Printf("gmail sync: user=%s account=%s: ai %v", r.acct.UserID, r.acct.ID, err)
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
	if _, err := r.s.Store.SetAIStatus(r.parent, r.acct.UserID, r.ai.Generation, status, v, r.s.Now().Unix()); err != nil && r.parent.Err() == nil {
		log.Printf("gmail sync: user=%s account=%s: save ai status: %v", r.acct.UserID, r.acct.ID, err)
		return
	}
	r.ai.Status, r.ai.LastError = status, v
}

// Source key kinds. A run of messages of a thread has the key
// gmail:{email}:r:{thread}:{first}:{last}; a group parent made by the AI
// for several tasks of one matter has gmail:{email}:g:{thread}:{item},
// where item is the ID of the task it was made for. parseSourceKey rejects
// the group key, so that detection and tracking never take it for
// messages.
const (
	kindRun   = "r"
	kindGroup = "g"
)

// sourceKey is the parsed form of a mail item's source key. Email is the
// lower-case address of the account, which never contains a colon.
type sourceKey struct {
	Email, Thread, First, Last string
}

func (k sourceKey) String() string {
	return threadPrefix(k.Email, kindRun, k.Thread) + k.First + ":" + k.Last
}

// accountPrefix is the start of the keys of an account's items.
func accountPrefix(email string) string { return "gmail:" + email + ":" }

// threadPrefix is the start of the keys of one kind in a thread.
func threadPrefix(email, kind, thread string) string {
	return accountPrefix(email) + kind + ":" + thread + ":"
}

// parseSourceKey is the inverse of sourceKey.String.
func parseSourceKey(v string) (sourceKey, bool) {
	parts := strings.Split(v, ":")
	if len(parts) != 6 || parts[0] != "gmail" || !strings.Contains(parts[1], "@") || parts[2] != kindRun ||
		!google.ValidID(parts[3]) || !google.ValidID(parts[4]) || !google.ValidID(parts[5]) {
		return sourceKey{}, false
	}
	return sourceKey{Email: parts[1], Thread: parts[3], First: parts[4], Last: parts[5]}, true
}

// threadURL links to a thread in Gmail, in the account's mailbox.
func threadURL(email, thread string) string { return google.ThreadURL(email, thread) }

// gmailURL returns v when it is an https URL on mail.google.com, for refs
// and automatic completion evidence.
func gmailURL(v string) sql.NullString {
	u, err := url.Parse(v)
	if err != nil || u.Scheme != "https" || u.Host != "mail.google.com" || len(v) > 2048 || strings.ContainsRune(v, 0) || u.User != nil {
		return sql.NullString{}
	}
	return sql.NullString{String: v, Valid: true}
}

// Labels of messages left out entirely, and the labels a target needs or
// must not have.
const (
	labelDraft = "DRAFT"
	labelTrash = "TRASH"
	labelSpam  = "SPAM"
	labelSent  = "SENT"
	labelInbox = "INBOX"
)

// visible returns the messages of a thread that are not drafts, in the
// trash or spam, in (internal date, ID) order.
func visible(t google.Thread) []google.Message {
	out := make([]google.Message, 0, len(t.Messages))
	for _, m := range t.Messages {
		if m.HasLabel(labelDraft) || m.HasLabel(labelTrash) || m.HasLabel(labelSpam) {
			continue
		}
		out = append(out, m)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].Internal.Equal(out[j].Internal) {
			return out[i].Internal.Before(out[j].Internal)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func indexID(list []google.Message, id string) int {
	for i, m := range list {
		if m.ID == id {
			return i
		}
	}
	return -1
}

// noReplyLocals are the local parts of senders that never expect a reply.
var noReplyLocals = map[string]bool{"noreply": true, "no-reply": true, "donotreply": true, "do-not-reply": true, "mailer-daemon": true}

// automatic reports whether a message was sent by a program: an
// Auto-Submitted header other than "no", Precedence bulk or junk, or a
// no-reply sender. A sender that cannot be parsed is not judged.
func automatic(m google.Message) bool {
	if v := strings.ToLower(strings.TrimSpace(m.AutoSubmitted)); m.HasAutoSubmitted && v != "" && v != "no" {
		return true
	}
	if v := strings.ToLower(strings.TrimSpace(m.Precedence)); v == "bulk" || v == "junk" {
		return true
	}
	if local, _, ok := strings.Cut(m.FromEmail, "@"); ok && noReplyLocals[strings.ToLower(local)] {
		return true
	}
	return false
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

// oneLine replaces control characters (including CR, LF and U+0085) and the
// line and paragraph separators with spaces, so that a header value cannot
// make a line of its own.
func oneLine(v string) string {
	return strings.Map(func(r rune) rune {
		if unicode.In(r, unicode.Cc, unicode.Zl, unicode.Zp) {
			return ' '
		}
		return r
	}, v)
}

// bodyText removes control characters other than newlines and tabs.
func bodyText(v string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if unicode.In(r, unicode.Cc, unicode.Cf, unicode.Zl, unicode.Zp) {
			return -1
		}
		return r
	}, v)
}

// excerpt returns the first n characters of a text on one line.
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
// statuses and counts, never mail contents, subjects, addresses or names.
func (r *run) warn(msg string, attrs ...any) {
	slog.Warn("gmail sync: "+msg, append([]any{"user", r.acct.UserID, "account", r.acct.ID}, attrs...)...)
}
