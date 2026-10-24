// Package githubsync finds the user's GitHub review requests and assignments,
// creates action items for them, and completes the items when GitHub shows
// that they are done. Mentions of the user are judged by the AI with the
// user's own key (see mentions.go). Requests to GitHub and the AI are never
// made inside a database transaction: SQLite has a single connection, and a
// transaction held during a request would block the management screen.
package githubsync

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/i2bskn/astovia/internal/ai"
	"github.com/i2bskn/astovia/internal/auth"
	"github.com/i2bskn/astovia/internal/github"
	"github.com/i2bskn/astovia/internal/secretbox"
	"github.com/i2bskn/astovia/internal/store"
)

const (
	// BatchSize is the number of due connections handled by one RunOnce.
	BatchSize = 20
	// connectionTimeout bounds the GitHub requests of one connection's
	// synchronization; what is left is done in the next one.
	connectionTimeout = 60 * time.Second
	reconcileInterval = 15 * time.Minute
	trackInterval     = 5 * time.Minute
	trackBatch        = 100
	// lookback is how far before the last successful poll notifications are
	// read again, and initialLookback is used before the first one.
	lookback        = 10 * time.Minute
	initialLookback = 24 * time.Hour
	rateLimitWait   = 15 * time.Minute
	maxBackoff      = 900
	// reviewGrace allows for a review submitted just before the request was
	// detected.
	reviewGrace = 300
)

// Error codes stored in github_connections.last_error and shown in the UI.
const (
	ErrorTokenInvalid       = "token_invalid"
	ErrorTokenUndecryptable = "token_undecryptable"
	ErrorRateLimited        = "rate_limited"
	ErrorUnavailable        = "unavailable"
)

// Automatic completion reasons stored in action_items.auto_reason.
const (
	ReasonReviewSubmitted      = "github.review_submitted"
	ReasonReviewRequestRemoved = "github.review_request_removed"
	ReasonPRMerged             = "github.pr_merged"
	ReasonPRClosed             = "github.pr_closed"
	ReasonIssueClosed          = "github.issue_closed"
	ReasonUnassigned           = "github.unassigned"
)

const (
	kindReview = "review"
	kindAssign = "assign"
)

// searches are run for the initial import and for reconciliation.
var searches = []struct{ kind, query string }{
	{kindReview, "is:open is:pr user-review-requested:@me archived:false"},
	{kindAssign, "is:open is:issue assignee:@me archived:false"},
	{kindAssign, "is:open is:pr assignee:@me archived:false"},
}

// TokenPurpose is the secretbox purpose of a user's GitHub token.
func TokenPurpose(userID string) string { return "github-token:" + userID }

// Syncer synchronizes GitHub connections. It assumes a single process.
// AI judges mentions; without it mentions are ignored.
type Syncer struct {
	Store  *store.Store
	GitHub *github.Client
	AI     *ai.Client
	Box    *secretbox.Box
	Now    func() time.Time

	// mentionDone remembers threads whose mentions were all handled, so
	// that an unchanged thread is not read from GitHub again.
	mentionMu   sync.Mutex
	mentionDone map[string]mentionRecord
}

// RunOnce synchronizes up to BatchSize due connections, one after another.
func (s *Syncer) RunOnce(ctx context.Context) {
	conns, err := s.Store.DueGitHubConnections(ctx, s.Now().Unix(), BatchSize)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("github sync: list connections: %v", err)
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
// connection deadline and is used for GitHub requests; parent is used for
// database writes, so that they still happen after the deadline.
type run struct {
	s      *Syncer
	ctx    context.Context
	parent context.Context
	conn   store.GitHubConnection
	state  store.GitHubConnection
	token  string
	start  time.Time

	// The mention stage. ai is the user's active AI settings, read at the
	// first mention thread (aiChecked). retry is set when a mention must be
	// read again by the next synchronization; overdue when LastSyncedAt has
	// stayed behind for longer than mentionRetryLimit, so that it moves on
	// and waiting mentions become confirmation tasks instead.
	ai             *store.AISettings
	aiChecked      bool
	mentionThreads []mentionThread
	mentionsDone   bool
	retry          bool
	overdue        bool
	aiCalls        int
	// aiFallbackOnly is set after the AI failed in a way that stops it for
	// the user; aiStale when the settings were replaced during the
	// synchronization; aiStopped when they were deleted or the key cannot
	// be opened.
	aiFallbackOnly bool
	aiStale        bool
	aiStopped      bool
}

// Sync synchronizes one connection and stores its new state. When the
// connection changed in the meantime (another token, deleted, user disabled)
// the results are discarded.
func (s *Syncer) Sync(parent context.Context, c store.GitHubConnection) {
	r := &run{s: s, parent: parent, conn: c, state: c, start: s.Now()}
	token, err := s.Box.Open(TokenPurpose(c.UserID), c.TokenSealed)
	if err != nil {
		r.state.Status, r.state.LastError = store.GitHubError, nullString(ErrorTokenUndecryptable)
		log.Printf("github sync: user=%s: %s", c.UserID, ErrorTokenUndecryptable)
		r.save()
		return
	}
	r.token = token
	ctx, cancel := context.WithTimeout(parent, connectionTimeout)
	defer cancel()
	r.ctx = ctx
	interval, err := r.steps()
	now := s.Now().Unix()
	var rate *github.RateLimitError
	switch {
	case errors.Is(err, store.ErrStaleConnection) || parent.Err() != nil:
		return
	case err == nil || ctx.Err() != nil:
		// Running out of time is not a failure; the rest waits for the
		// next poll.
		r.state.FailureCount, r.state.LastError, r.state.NextPollAt = 0, sql.NullString{}, now+int64(interval)
	case errors.Is(err, github.ErrUnauthorized):
		r.state.Status, r.state.LastError = store.GitHubError, nullString(ErrorTokenInvalid)
		log.Printf("github sync: user=%s: %s", c.UserID, ErrorTokenInvalid)
	case errors.As(err, &rate):
		r.state.LastError = nullString(ErrorRateLimited)
		r.state.NextPollAt = now + int64(rateLimitWait/time.Second)
		if !rate.RetryAt.IsZero() {
			r.state.NextPollAt = rate.RetryAt.Unix()
		}
		log.Printf("github sync: user=%s: %s", c.UserID, ErrorRateLimited)
	default:
		r.state.FailureCount++
		r.state.LastError = nullString(ErrorUnavailable)
		r.state.NextPollAt = now + backoff(r.state.FailureCount)
		log.Printf("github sync: user=%s: %s", c.UserID, ErrorUnavailable)
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
	err := r.s.Store.GitHubSync(r.parent, r.conn.UserID, r.conn.Generation, func(tx *store.GitHubTx) error {
		return tx.SaveState(r.state)
	})
	if err != nil && !errors.Is(err, store.ErrStaleConnection) && r.parent.Err() == nil {
		log.Printf("github sync: user=%s: save state: %v", r.conn.UserID, err)
	}
}

// steps runs the initial import, notification polling, reconciliation,
// tracking and the mention stage, recording progress in r.state. It returns
// the poll interval. When collected mentions were not all handled,
// LastSyncedAt and LastModified keep their previous values so that the
// next synchronization reads the same notifications again (items are not
// duplicated: their source keys are unique).
func (r *run) steps() (interval int, err error) {
	interval = github.MinPollInterval
	start := r.start.Unix()
	synced, modified := r.state.LastSyncedAt, r.state.LastModified
	r.overdue = synced.Valid && start-synced.Int64 > int64(mentionRetryLimit/time.Second)
	defer func() {
		if r.mentionsPending() && !r.overdue {
			r.state.LastSyncedAt, r.state.LastModified = synced, modified
		}
	}()
	imported := false
	if r.state.InitialImportPending == 1 {
		imported = true
		complete, err := r.search(false)
		if err != nil {
			return interval, err
		}
		if complete {
			r.state.InitialImportPending = 0
			r.state.LastReconciledAt = nullInt(start)
		}
	}

	since := r.start.Add(-initialLookback)
	if r.state.LastSyncedAt.Valid {
		since = time.Unix(r.state.LastSyncedAt.Int64, 0).Add(-lookback)
	}
	threads, lastModified, interval, notModified, err := r.s.GitHub.Notifications(r.ctx, r.token, r.state.LastModified.String, since)
	if err != nil {
		return github.MinPollInterval, err
	}
	if !notModified {
		for _, t := range threads {
			if err = r.thread(t); err != nil {
				return interval, err
			}
		}
		r.state.LastModified = sql.NullString{String: lastModified, Valid: lastModified != ""}
	}
	r.state.LastSyncedAt = nullInt(start)

	if !imported && due(r.state.LastReconciledAt, start, reconcileInterval) {
		if _, err = r.search(true); err != nil {
			return interval, err
		}
		r.state.LastReconciledAt = nullInt(start)
	}
	if due(r.state.LastTrackedAt, start, trackInterval) {
		if err = r.track(); err != nil {
			return interval, err
		}
		r.state.LastTrackedAt = nullInt(start)
	}
	if err = r.mentions(); err != nil {
		return interval, err
	}
	return interval, nil
}

func due(last sql.NullInt64, now int64, every time.Duration) bool {
	return !last.Valid || now-last.Int64 >= int64(every/time.Second)
}

// stops reports whether err ends the whole synchronization: an invalid
// token, a rate limit, the deadline, shutdown or a lost connection row.
// Other errors only skip the object being looked at.
func stops(err error) bool {
	return errors.Is(err, github.ErrUnauthorized) || errors.Is(err, github.ErrRateLimited) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) || errors.Is(err, store.ErrStaleConnection)
}

// search runs the searches and creates items for what it finds (never
// reopening). notify queues detection notifications. It reports whether
// every search completed; a rate limited search skips the rest without
// failing the synchronization, and a search GitHub rejects (422) is logged
// and counted as done.
func (r *run) search(notify bool) (bool, error) {
	complete := true
	for _, q := range searches {
		items, err := r.s.GitHub.SearchIssues(r.ctx, r.token, q.query)
		switch {
		case err == nil:
		case errors.Is(err, github.ErrRateLimited):
			log.Printf("github sync: user=%s: search %s", r.conn.UserID, ErrorRateLimited)
			return false, nil
		case errors.Is(err, github.ErrUnprocessable):
			log.Printf("github sync: user=%s: search rejected", r.conn.UserID)
			continue
		case stops(err):
			return false, err
		default:
			log.Printf("github sync: user=%s: search %s", r.conn.UserID, ErrorUnavailable)
			complete = false
			continue
		}
		for _, it := range items {
			if q.kind == kindReview && !it.IsPull {
				continue
			}
			ref := github.Ref{Owner: it.Owner, Repo: it.Repo, Number: it.Number}
			if err = r.detect(q.kind, ref, it.Title, it.HTMLURL, notify, false); err != nil {
				return false, err
			}
		}
	}
	return complete, nil
}

// thread handles one notification thread. Only review requests to the user,
// assignments and mentions are used; the subject must be an API URL under the base
// URL, otherwise the thread is ignored and the token is not sent anywhere.
func (r *run) thread(t github.Thread) error {
	ref, ok := r.s.GitHub.ParseSubjectURL(t.SubjectURL)
	if !ok {
		return nil
	}
	switch {
	case t.Reason == "review_requested" && t.SubjectType == "PullRequest" && ref.Kind == "pulls":
		p, err := r.s.GitHub.Pull(r.ctx, r.token, ref.Owner, ref.Repo, ref.Number)
		if err != nil {
			return r.skip(err)
		}
		if p.State == "open" && hasLogin(p.RequestedReviewers, r.conn.Login) {
			return r.detect(kindReview, ref, p.Title, p.HTMLURL, true, true)
		}
	case t.Reason == "assign" && ((t.SubjectType == "Issue" && ref.Kind == "issues") || (t.SubjectType == "PullRequest" && ref.Kind == "pulls")):
		// A pull request has the same number in the issues API.
		i, err := r.s.GitHub.Issue(r.ctx, r.token, ref.Owner, ref.Repo, ref.Number)
		if err != nil {
			return r.skip(err)
		}
		if i.State == "open" && hasLogin(i.Assignees, r.conn.Login) {
			return r.detect(kindAssign, ref, i.Title, i.HTMLURL, true, true)
		}
	case t.Reason == "mention" && ((t.SubjectType == "Issue" && ref.Kind == "issues") || (t.SubjectType == "PullRequest" && ref.Kind == "pulls")):
		// Handled after tracking, so that a slow AI does not hold up the
		// automatic completions.
		return r.collectMention(t, ref)
	}
	return nil
}

// skip returns err when it stops the synchronization, and nil otherwise so
// that only the current object is skipped.
func (r *run) skip(err error) error {
	if stops(err) {
		return err
	}
	return nil
}

func hasLogin(logins []string, login string) bool {
	for _, l := range logins {
		if strings.EqualFold(l, login) {
			return true
		}
	}
	return false
}

// SourceKey returns the action item key of a review request or assignment.
// Owner and repository are lower-cased because GitHub compares them without
// case.
func SourceKey(kind string, ref github.Ref) string {
	return fmt.Sprintf("github:%s:%s/%s#%d", kind, strings.ToLower(ref.Owner), strings.ToLower(ref.Repo), ref.Number)
}

// parseSourceKey is the inverse of SourceKey.
func parseSourceKey(key string) (string, github.Ref, bool) {
	rest, ok := strings.CutPrefix(key, "github:")
	if !ok {
		return "", github.Ref{}, false
	}
	kind, rest, ok := strings.Cut(rest, ":")
	if !ok || (kind != kindReview && kind != kindAssign) {
		return "", github.Ref{}, false
	}
	repo, number, ok := strings.Cut(rest, "#")
	owner, name, ok2 := strings.Cut(repo, "/")
	n, err := strconv.Atoi(number)
	if !ok || !ok2 || err != nil || n < 1 || !github.ValidName(owner) || !github.ValidName(name) {
		return "", github.Ref{}, false
	}
	return kind, github.Ref{Owner: owner, Repo: name, Number: n}, true
}

// cleanTitle removes control and format characters and surrounding spaces.
func cleanTitle(v string) string {
	v = strings.Map(func(r rune) rune {
		if unicode.In(r, unicode.Cc, unicode.Cf, unicode.Zl, unicode.Zp) {
			return -1
		}
		return r
	}, v)
	return strings.TrimFunc(v, unicode.IsSpace)
}

func truncate(v string, n int) string {
	if utf8.RuneCountInString(v) <= n {
		return v
	}
	return string([]rune(v)[:n])
}

// githubURL returns v when it is a github.com web URL, for refs and
// automatic completion evidence.
func githubURL(v string) sql.NullString {
	if strings.HasPrefix(v, "https://github.com/") && len(v) <= 2048 && !strings.ContainsAny(v, "\x00") {
		return sql.NullString{String: v, Valid: true}
	}
	return sql.NullString{}
}

type ref struct {
	Service        string `json:"service"`
	Account        string `json:"account,omitempty"`
	ConversationID string `json:"conversation_id,omitempty"`
	URL            string `json:"url,omitempty"`
}

// detect creates the item for a review request or assignment, or reopens it
// when reopen is set and the item was completed automatically and is still
// tracked. Items the user completed or dismissed are left alone.
func (r *run) detect(kind string, gref github.Ref, title, htmlURL string, notify, reopen bool) error {
	key := SourceKey(kind, gref)
	name := fmt.Sprintf("%s/%s#%d", gref.Owner, gref.Repo, gref.Number)
	title = cleanTitle(title)
	if title == "" {
		title = name
	}
	content := "対応: " + title
	if kind == kindReview {
		content = "レビュー: " + title
	}
	content = truncate(content, 500)
	refs, err := json.Marshal([]ref{{Service: store.SourceGitHub, Account: r.conn.Login, ConversationID: name, URL: githubURL(htmlURL).String}})
	if err != nil {
		return err
	}
	id, err := auth.Random()
	if err != nil {
		return err
	}
	now := r.s.Now().Unix()
	return r.s.Store.GitHubSync(r.parent, r.conn.UserID, r.conn.Generation, func(tx *store.GitHubTx) error {
		item, found, e := tx.LockItemBySourceKey(key)
		if e != nil {
			return e
		}
		if !found {
			item = store.ActionItem{ID: id, Content: content, Status: "open", Refs: string(refs), Version: 1, CreatedAt: now, UpdatedAt: now,
				Source: nullString(store.SourceGitHub), SourceKey: nullString(key), Tracking: 1, DetectedAt: nullInt(now)}
			created, e := tx.CreateItem(item)
			if e != nil || !created {
				// Created concurrently: treated as existing.
				return e
			}
			if notify && tx.Conn.ImmediateNotify == 1 {
				return tx.Enqueue(detectedNotification(kind, name, title, item, false, now))
			}
			return nil
		}
		if !reopen || item.Status != "done" || !item.AutoReason.Valid || item.Tracking != 1 {
			return nil
		}
		next := item
		next.Status, next.AutoReason, next.AutoRef, next.AutoAt = "open", sql.NullString{}, sql.NullString{}, sql.NullInt64{}
		next.Tracking, next.DetectedAt, next.UpdatedAt = 1, nullInt(now), now
		updated, ok, e := tx.UpdateItem(item, next)
		if e != nil || !ok {
			return e
		}
		if notify && tx.Conn.ImmediateNotify == 1 {
			return tx.Enqueue(detectedNotification(kind, name, title, updated, true, now))
		}
		return nil
	})
}

func detectedNotification(kind, name, title string, item store.ActionItem, again bool, now int64) store.Notification {
	label := map[bool]string{false: "割り当て", true: "再割り当て"}[again]
	if kind == kindReview {
		label = map[bool]string{false: "レビュー依頼", true: "再レビュー依頼"}[again]
	}
	id, err := auth.Random()
	if err != nil {
		// The ID only needs to be unique; the dedupe key keeps the
		// notification single either way.
		id = item.ID + "-" + strconv.FormatInt(item.Version, 10)
	}
	return store.Notification{ID: id, Kind: store.NotificationDetected, ActionItemID: nullString(item.ID),
		DedupeKey: "detected:" + item.ID + ":" + strconv.FormatInt(item.Version, 10),
		Title:     label + ": " + name, Body: title, URL: "/", ScheduledAt: now, CreatedAt: now}
}

// decision is the outcome of checking one tracked item: an automatic
// completion (reason set), stop tracking (untrack) or nothing.
type decision struct {
	reason, ref string
	untrack     bool
}

// track checks the tracked open items, least recently checked first.
func (r *run) track() error {
	items, err := r.s.Store.TrackedActionItems(r.parent, r.conn.UserID, store.SourceGitHub, trackBatch)
	if err != nil {
		return err
	}
	for _, item := range items {
		if err = r.ctx.Err(); err != nil {
			return err
		}
		kind, gref, ok := parseSourceKey(item.SourceKey.String)
		var d decision
		if ok {
			d, err = r.decide(kind, gref, item)
			switch {
			case err == nil:
			case errors.Is(err, github.ErrNotFound):
				d = decision{untrack: true}
			case stops(err):
				return err
			default:
				// Only this item is skipped (for example 301 or 403 by SSO).
				d = decision{}
			}
		}
		if err = r.apply(item.ID, d); err != nil {
			return err
		}
	}
	return nil
}

func (r *run) decide(kind string, gref github.Ref, item store.ActionItem) (decision, error) {
	gh, ctx, token := r.s.GitHub, r.ctx, r.token
	if kind == kindReview {
		p, err := gh.Pull(ctx, token, gref.Owner, gref.Repo, gref.Number)
		if err != nil {
			return decision{}, err
		}
		switch {
		case p.Merged:
			return decision{reason: ReasonPRMerged, ref: p.HTMLURL}, nil
		case p.State == "closed":
			return decision{reason: ReasonPRClosed, ref: p.HTMLURL}, nil
		case hasLogin(p.RequestedReviewers, r.conn.Login):
			return decision{}, nil
		}
		reviews, err := gh.Reviews(ctx, token, gref.Owner, gref.Repo, gref.Number)
		if err != nil {
			return decision{}, err
		}
		since := item.CreatedAt
		if item.DetectedAt.Valid {
			since = item.DetectedAt.Int64
		}
		since -= reviewGrace
		var latest *github.Review
		for i := range reviews {
			rv := &reviews[i]
			if !strings.EqualFold(rv.Login, r.conn.Login) || rv.State == "PENDING" || rv.SubmittedAt == nil || rv.SubmittedAt.Unix() < since {
				continue
			}
			if latest == nil || rv.SubmittedAt.After(*latest.SubmittedAt) {
				latest = rv
			}
		}
		if latest != nil {
			return decision{reason: ReasonReviewSubmitted, ref: latest.HTMLURL}, nil
		}
		return decision{reason: ReasonReviewRequestRemoved, ref: p.HTMLURL}, nil
	}
	i, err := gh.Issue(ctx, token, gref.Owner, gref.Repo, gref.Number)
	if err != nil {
		return decision{}, err
	}
	if i.State == "closed" {
		if !i.IsPull {
			return decision{reason: ReasonIssueClosed, ref: i.HTMLURL}, nil
		}
		p, err := gh.Pull(ctx, token, gref.Owner, gref.Repo, gref.Number)
		if err != nil {
			return decision{}, err
		}
		if p.Merged {
			return decision{reason: ReasonPRMerged, ref: p.HTMLURL}, nil
		}
		return decision{reason: ReasonPRClosed, ref: p.HTMLURL}, nil
	}
	if !hasLogin(i.Assignees, r.conn.Login) {
		return decision{reason: ReasonUnassigned, ref: i.HTMLURL}, nil
	}
	return decision{}, nil
}

// apply stores a decision after locking and re-reading the item. An
// automatic completion requires the item to be still open, tracked and from
// GitHub, and drops its pending notifications. Without a change only
// tracked_at is written.
func (r *run) apply(id string, d decision) error {
	now := r.s.Now().Unix()
	return r.s.Store.GitHubSync(r.parent, r.conn.UserID, r.conn.Generation, func(tx *store.GitHubTx) error {
		cur, found, e := tx.LockItem(id)
		if e != nil || !found {
			return e
		}
		if cur.Status != "open" || cur.Tracking != 1 || cur.Source.String != store.SourceGitHub {
			return nil
		}
		next := cur
		switch {
		case d.reason != "":
			next.Status, next.AutoReason, next.AutoRef, next.AutoAt = "done", nullString(d.reason), githubURL(d.ref), nullInt(now)
		case d.untrack:
			next.Tracking = 0
		default:
			return tx.TouchItem(cur, now)
		}
		next.TrackedAt, next.UpdatedAt = nullInt(now), now
		if _, ok, e := tx.UpdateItem(cur, next); e != nil || !ok {
			return e
		}
		if d.reason != "" {
			return tx.DropPendingNotifications(cur.ID)
		}
		return nil
	})
}

func nullString(v string) sql.NullString { return sql.NullString{String: v, Valid: true} }
func nullInt(v int64) sql.NullInt64      { return sql.NullInt64{Int64: v, Valid: true} }
