package githubsync

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/i2bskn/astovia/internal/ai"
	"github.com/i2bskn/astovia/internal/auth"
	"github.com/i2bskn/astovia/internal/github"
	"github.com/i2bskn/astovia/internal/store"
)

const (
	kindMention = "mention"
	// ReasonMentionNoAction completes a mention the AI judged to need
	// nothing from the user.
	ReasonMentionNoAction = "github.mention_no_action"
	// mentionAICallsPerRun bounds the AI calls of one synchronization; the
	// rest is postponed to the next one.
	mentionAICallsPerRun = 3
	// mentionMinRemaining is the time an AI call needs left before the
	// connection deadline.
	mentionMinRemaining = 25 * time.Second
	// mentionWindow and mentionConnectGrace bound how old a mention may be:
	// at most 72 hours, and at most 24 hours before the GitHub connection
	// or the API key was registered.
	mentionWindow       = 72 * time.Hour
	mentionConnectGrace = 24 * time.Hour
	// mentionFallbackAge is the age after which a mention that hit a
	// temporary AI failure becomes a confirmation task instead of waiting.
	mentionFallbackAge = 2 * time.Hour
	// mentionRetryLimit is how long LastSyncedAt may stay behind for
	// mentions that need a retry; after that it moves on and they become
	// confirmation tasks.
	mentionRetryLimit = 6 * time.Hour
	// mentionDoneTTL is how long a completely handled thread is remembered.
	mentionDoneTTL = time.Hour
)

// Error codes stored in ai_settings.last_error by the synchronization.
const (
	AIErrorKeyInvalid       = "key_invalid"
	AIErrorKeyUndecryptable = "key_undecryptable"
	AIErrorBilling          = "billing"
	AIErrorModelUnavailable = "model_unavailable"
	AIErrorRequestRejected  = "request_rejected"
)

// mentionThread is a notification thread with reason "mention", collected
// during notification polling and handled after tracking.
type mentionThread struct {
	thread github.Thread
	ref    github.Ref
}

// mentionRecord remembers a thread whose mentions were all handled at the
// thread's UpdatedAt.
type mentionRecord struct {
	updatedAt  time.Time
	recordedAt time.Time
}

func (s *Syncer) mentionKey(c store.GitHubConnection, t github.Thread) string {
	return c.UserID + "\x00" + strconv.FormatInt(c.Generation, 10) + "\x00" + t.ID
}

// mentionSeen reports whether the thread was completely handled at its
// current UpdatedAt.
func (s *Syncer) mentionSeen(c store.GitHubConnection, t github.Thread) bool {
	if t.ID == "" {
		return false
	}
	s.mentionMu.Lock()
	defer s.mentionMu.Unlock()
	rec, ok := s.mentionDone[s.mentionKey(c, t)]
	return ok && rec.updatedAt.Equal(t.UpdatedAt) && s.Now().Sub(rec.recordedAt) < mentionDoneTTL
}

// markMention records a completely handled thread and forgets records
// older than mentionDoneTTL.
func (s *Syncer) markMention(c store.GitHubConnection, t github.Thread) {
	if t.ID == "" {
		return
	}
	now := s.Now()
	s.mentionMu.Lock()
	defer s.mentionMu.Unlock()
	if s.mentionDone == nil {
		s.mentionDone = map[string]mentionRecord{}
	}
	for k, rec := range s.mentionDone {
		if now.Sub(rec.recordedAt) >= mentionDoneTTL {
			delete(s.mentionDone, k)
		}
	}
	s.mentionDone[s.mentionKey(c, t)] = mentionRecord{updatedAt: t.UpdatedAt, recordedAt: now}
}

// collectMention keeps a mention thread for the mention stage when the
// user has active AI settings. Without them nothing is requested from
// GitHub.
func (r *run) collectMention(t github.Thread, ref github.Ref) error {
	if r.s.AI == nil {
		return nil
	}
	if !r.aiChecked {
		a, err := r.s.Store.AISettings(r.parent, r.conn.UserID)
		switch {
		case errors.Is(err, store.ErrAISettingsNotFound):
		case err != nil:
			return err
		case a.Status == store.AIActive:
			r.ai = &a
		}
		r.aiChecked = true
	}
	if r.ai != nil {
		r.mentionThreads = append(r.mentionThreads, mentionThread{thread: t, ref: ref})
	}
	return nil
}

// mentionsPending reports whether collected mentions were not all handled,
// so that LastSyncedAt and LastModified must stay where they were.
func (r *run) mentionsPending() bool {
	return len(r.mentionThreads) > 0 && (!r.mentionsDone || r.retry)
}

// mentions is the mention stage: the collected threads, least recently
// updated first.
func (r *run) mentions() error {
	sort.SliceStable(r.mentionThreads, func(i, j int) bool {
		return r.mentionThreads[i].thread.UpdatedAt.Before(r.mentionThreads[j].thread.UpdatedAt)
	})
	for _, mt := range r.mentionThreads {
		if r.aiStopped {
			break
		}
		if err := r.ctx.Err(); err != nil {
			return err
		}
		if r.s.mentionSeen(r.conn, mt.thread) {
			continue
		}
		complete, err := r.mentionThread(mt)
		if err != nil {
			return err
		}
		if complete {
			r.s.markMention(r.conn, mt.thread)
		}
	}
	r.mentionsDone = true
	return nil
}

// mentionGitHubError handles a GitHub error of the mention stage: a missing
// or forbidden (for example SSO) subject counts as handled, errors that stop
// the synchronization are returned, and others need a retry.
func (r *run) mentionGitHubError(err error) (bool, error) {
	switch {
	case errors.Is(err, github.ErrNotFound), errors.Is(err, github.ErrForbidden):
		return true, nil
	case stops(err):
		return false, err
	}
	log.Printf("github sync: user=%s: mention %s", r.conn.UserID, ErrorUnavailable)
	r.retry = true
	return false, nil
}

// mentionPost is a post with the URL of its page and the time compared
// with the cutoff (the creation of a description, the last update of a
// comment, the submission of a review).
type mentionPost struct {
	ai.Post
	url   string
	since time.Time
}

// mentionThread handles the mentions of the user in one thread and reports
// whether all of them were handled.
func (r *run) mentionThread(mt mentionThread) (bool, error) {
	gh, ctx, token, ref := r.s.GitHub, r.ctx, r.token, mt.ref
	issue, err := gh.Issue(ctx, token, ref.Owner, ref.Repo, ref.Number)
	if err != nil {
		return r.mentionGitHubError(err)
	}
	comments, err := gh.IssueComments(ctx, token, ref.Owner, ref.Repo, ref.Number)
	if err != nil {
		return r.mentionGitHubError(err)
	}
	var reviews []github.Review
	var reviewComments []github.Comment
	if issue.IsPull {
		if reviews, err = gh.RecentReviews(ctx, token, ref.Owner, ref.Repo, ref.Number); err != nil {
			return r.mentionGitHubError(err)
		}
		if reviewComments, err = gh.ReviewComments(ctx, token, ref.Owner, ref.Repo, ref.Number); err != nil {
			return r.mentionGitHubError(err)
		}
	}
	posts := mentionPosts(issue, comments, reviews, reviewComments)

	now := r.s.Now()
	cutoff := now.Add(-mentionWindow)
	for _, t := range []time.Time{time.Unix(r.conn.CreatedAt, 0).Add(-mentionConnectGrace), time.Unix(r.ai.ActivatedAt, 0).Add(-mentionConnectGrace)} {
		if t.After(cutoff) {
			cutoff = t
		}
	}
	complete := true
	for i, p := range posts {
		if strings.EqualFold(p.Author, r.conn.Login) || p.since.Before(cutoff) || !mentions(p.Body, r.conn.Login) {
			continue
		}
		handled, err := r.mention(mt.ref, issue, posts, i)
		if err != nil {
			return false, err
		}
		complete = complete && handled
	}
	return complete, nil
}

// mentionPosts returns the description (when it has a body), the comments,
// the submitted reviews with a body and the review comments: the
// description first, then the rest in time order.
func mentionPosts(issue github.Issue, comments []github.Comment, reviews []github.Review, reviewComments []github.Comment) []mentionPost {
	author := func(login string) string {
		if login == "" {
			return "ghost"
		}
		return login
	}
	var out []mentionPost
	if strings.TrimSpace(issue.Body) != "" {
		out = append(out, mentionPost{Post: ai.Post{ID: "body", Author: author(issue.Login), At: issue.CreatedAt, Type: ai.PostDescription, Body: issue.Body}, url: issue.HTMLURL, since: issue.CreatedAt})
	}
	head := len(out)
	for _, c := range comments {
		out = append(out, mentionPost{Post: ai.Post{ID: "c" + strconv.FormatInt(c.ID, 10), Author: author(c.Login), At: c.CreatedAt, Type: ai.PostComment, Body: c.Body}, url: c.HTMLURL, since: c.UpdatedAt})
	}
	for _, rv := range reviews {
		if rv.State == "PENDING" || rv.SubmittedAt == nil || strings.TrimSpace(rv.Body) == "" {
			continue
		}
		out = append(out, mentionPost{Post: ai.Post{ID: "r" + strconv.FormatInt(rv.ID, 10), Author: author(rv.Login), At: *rv.SubmittedAt, Type: ai.PostReview, Body: rv.Body}, url: rv.HTMLURL, since: *rv.SubmittedAt})
	}
	for _, c := range reviewComments {
		out = append(out, mentionPost{Post: ai.Post{ID: "rc" + strconv.FormatInt(c.ID, 10), Author: author(c.Login), At: c.CreatedAt, Type: ai.PostReviewComment, Body: c.Body}, url: c.HTMLURL, since: c.UpdatedAt})
	}
	rest := out[head:]
	sort.SliceStable(rest, func(i, j int) bool { return rest[i].At.Before(rest[j].At) })
	return out
}

// mentionOutcome is the item to create for a mention.
type mentionOutcome int

const (
	mentionAction    mentionOutcome = iota // the AI found something to do
	mentionCandidate                       // the AI is not sure the user has something to do
	mentionNoAction                        // the AI found nothing to do
	mentionFallback                        // no judgment: ask the user to check
)

// mention handles the mention in posts[i] and reports whether it was
// handled (an item exists or was created). Nothing is sent to the AI when
// the item exists already.
func (r *run) mention(gref github.Ref, issue github.Issue, posts []mentionPost, i int) (bool, error) {
	key := SourceKey(kindMention, gref) + ":" + posts[i].ID
	exists := false
	err := r.s.Store.GitHubSync(r.parent, r.conn.UserID, r.conn.Generation, func(tx *store.GitHubTx) error {
		_, found, e := tx.LockItemBySourceKey(key)
		exists = found
		return e
	})
	if err != nil || exists {
		return exists, err
	}
	create := func(o mentionOutcome, res ai.MentionResult) (bool, error) {
		return true, r.createMention(key, gref, issue, posts[i], o, res)
	}
	switch {
	case r.aiStopped:
		return false, nil
	case r.aiFallbackOnly:
		return create(mentionFallback, ai.MentionResult{})
	case r.aiStale || r.aiCalls >= mentionAICallsPerRun || r.deadlineNear():
		return r.postpone(create)
	}

	// The settings are read again right before the call, so that nothing
	// is sent after they were deleted or replaced.
	cur, err := r.s.Store.AISettings(r.parent, r.conn.UserID)
	switch {
	case errors.Is(err, store.ErrAISettingsNotFound):
		r.aiStopped = true
		return false, nil
	case err != nil:
		return false, err
	case cur.Generation != r.ai.Generation:
		r.aiStale = true
		return r.postpone(create)
	case cur.Status != store.AIActive:
		r.aiFallbackOnly = true
		return create(mentionFallback, ai.MentionResult{})
	}
	r.ai = &cur
	apiKey, err := r.s.Box.Open(ai.KeyPurpose(r.conn.UserID), cur.APIKeySealed)
	if err != nil {
		log.Printf("github sync: user=%s: mention ai %s", r.conn.UserID, AIErrorKeyUndecryptable)
		r.setAIStatus(store.AIError, AIErrorKeyUndecryptable)
		r.aiStopped = true
		return false, nil
	}
	loc, err := r.location()
	if err != nil {
		return false, err
	}
	in := ai.MentionInput{Me: r.conn.Login, Location: loc, Repository: gref.Owner + "/" + gref.Repo, Number: gref.Number, Kind: "issue", State: issue.State, Title: issue.Title, Target: i}
	if issue.IsPull {
		in.Kind = "pull_request"
	}
	if issue.Merged {
		in.State = "merged"
	}
	for _, p := range posts {
		in.Posts = append(in.Posts, p.Post)
	}
	r.aiCalls++
	res, err := r.s.AI.JudgeMention(r.ctx, apiKey, cur.Model, in)
	if err != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		return false, err
	}
	if err != nil {
		log.Printf("github sync: user=%s: mention %v", r.conn.UserID, err)
	}
	switch {
	case err == nil:
		if cur.LastError.Valid {
			r.setAIStatus(store.AIActive, "")
		}
		switch res.Decision {
		case ai.DecisionTask:
			return create(mentionAction, res)
		case ai.DecisionCandidate:
			return create(mentionCandidate, res)
		}
		return create(mentionNoAction, res)
	case errors.Is(err, ai.ErrKeyInvalid):
		return r.aiFailed(AIErrorKeyInvalid, create)
	case errors.Is(err, ai.ErrBilling):
		return r.aiFailed(AIErrorBilling, create)
	case errors.Is(err, ai.ErrModelUnavailable):
		return r.aiFailed(AIErrorModelUnavailable, create)
	case errors.Is(err, ai.ErrUnusable):
		r.setAIStatus(store.AIActive, AIErrorRequestRejected)
		return create(mentionFallback, ai.MentionResult{})
	}
	// A temporary failure: an old mention becomes a confirmation task,
	// a recent one waits for the next synchronization.
	if posts[i].since.Before(r.s.Now().Add(-mentionFallbackAge)) {
		return create(mentionFallback, ai.MentionResult{})
	}
	return r.postpone(create)
}

// aiFailed stops using the AI for the user: the settings are marked as
// failing, and this and the remaining mentions of the synchronization
// become confirmation tasks.
func (r *run) aiFailed(code string, create func(mentionOutcome, ai.MentionResult) (bool, error)) (bool, error) {
	r.setAIStatus(store.AIError, code)
	r.aiFallbackOnly = true
	return create(mentionFallback, ai.MentionResult{})
}

// postpone leaves a mention to the next synchronization, or makes it a
// confirmation task when LastSyncedAt has stayed behind for too long.
func (r *run) postpone(create func(mentionOutcome, ai.MentionResult) (bool, error)) (bool, error) {
	if r.overdue {
		return create(mentionFallback, ai.MentionResult{})
	}
	r.retry = true
	return false, nil
}

// deadlineNear reports whether too little time is left for an AI call.
func (r *run) deadlineNear() bool {
	deadline, ok := r.ctx.Deadline()
	return ok && time.Until(deadline) < mentionMinRemaining
}

// setAIStatus records the AI status of the settings the synchronization
// read; settings saved again or deleted in the meantime are left alone.
// lastError "" clears the error.
func (r *run) setAIStatus(status, lastError string) {
	v := sql.NullString{String: lastError, Valid: lastError != ""}
	if _, err := r.s.Store.SetAIStatus(r.parent, r.conn.UserID, r.ai.Generation, status, v, r.s.Now().Unix()); err != nil && r.parent.Err() == nil {
		log.Printf("github sync: user=%s: save ai status: %v", r.conn.UserID, err)
		return
	}
	r.ai.Status, r.ai.LastError = status, v
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

// createMention creates the item of a mention unless it exists. Mention
// items are not tracked. A candidate is not notified.
func (r *run) createMention(key string, gref github.Ref, issue github.Issue, p mentionPost, o mentionOutcome, res ai.MentionResult) error {
	name := fmt.Sprintf("%s/%s#%d", gref.Owner, gref.Repo, gref.Number)
	title := cleanTitle(issue.Title)
	if title == "" {
		title = name
	}
	now := r.s.Now().Unix()
	id, err := auth.Random()
	if err != nil {
		return err
	}
	url := githubURL(p.url)
	if !url.Valid {
		url = githubURL(issue.HTMLURL)
	}
	refs, err := json.Marshal([]ref{{Service: store.SourceGitHub, Account: r.conn.Login, ConversationID: name, URL: url.String}})
	if err != nil {
		return err
	}
	item := store.ActionItem{ID: id, Status: "open", Refs: string(refs), Version: 1, CreatedAt: now, UpdatedAt: now,
		Source: nullString(store.SourceGitHub), SourceKey: nullString(key), Tracking: 0, DetectedAt: nullInt(now)}
	switch o {
	case mentionAction, mentionCandidate:
		item.Content = res.Task
		if o == mentionCandidate {
			item.Status = "candidate"
		}
		setEvent(&item, res.Event, githubURL(p.url), now)
	case mentionNoAction:
		item.Content = res.Summary
		if item.Content == "" {
			item.Content = "メンション: " + title
		}
		item.Status, item.AutoReason, item.AutoRef, item.AutoAt = "done", nullString(ReasonMentionNoAction), githubURL(p.url), nullInt(now)
	default:
		item.Content = "メンションを確認: " + title
	}
	item.Content = truncate(item.Content, 500)
	return r.s.Store.GitHubSync(r.parent, r.conn.UserID, r.conn.Generation, func(tx *store.GitHubTx) error {
		if _, found, e := tx.LockItemBySourceKey(key); e != nil || found {
			return e
		}
		created, e := tx.CreateItem(item)
		if e != nil || !created || item.Status != "open" || tx.Conn.ImmediateNotify != 1 {
			return e
		}
		return tx.Enqueue(mentionNotification(name, item, now))
	})
}

// setEvent makes item an event item when the AI found an event. An event
// that has already started is dismissed at once; ref is the evidence of
// that dismissal.
func setEvent(item *store.ActionItem, e *ai.Event, ref sql.NullString, now int64) {
	if e == nil {
		return
	}
	start := e.Start.Unix()
	item.Kind, item.EventTitle, item.EventStartAt, item.DueAt = store.KindEvent, nullString(e.Title), nullInt(start), nullInt(start)
	if !e.End.IsZero() {
		item.EventEndAt = nullInt(e.End.Unix())
	}
	if start <= now {
		item.Status, item.Tracking, item.AutoReason, item.AutoRef, item.AutoAt = "dismissed", 0, nullString(store.ReasonEventPassed), ref, nullInt(now)
	}
}

func mentionNotification(name string, item store.ActionItem, now int64) store.Notification {
	id, err := auth.Random()
	if err != nil {
		// The ID only needs to be unique; the dedupe key keeps the
		// notification single either way.
		id = item.ID + "-" + strconv.FormatInt(item.Version, 10)
	}
	return store.Notification{ID: id, Kind: store.NotificationDetected, ActionItemID: nullString(item.ID),
		DedupeKey: "detected:" + item.ID + ":" + strconv.FormatInt(item.Version, 10),
		Title:     "メンション: " + name, Body: item.Content, URL: "/", ScheduledAt: now, CreatedAt: now}
}

// mentions reports whether body mentions @login (ASCII case-insensitive).
// The login must not follow a letter, digit, "_", "/" or "-", nor be
// followed by a letter, digit, "-" or "/" (which would make it a team).
// Fenced code blocks (``` or ~~~, unterminated ones to the end), inline
// code, HTML comments and quoted lines are ignored. Indented code blocks
// are read as text.
func mentions(body, login string) bool {
	if login == "" {
		return false
	}
	text := stripInlineCode(stripHTMLComments(stripBlocks(body)))
	at := "@" + login
	for i := 0; i+len(at) <= len(text); i++ {
		if text[i] != '@' || !asciiEqualFold(text[i+1:i+len(at)], login) {
			continue
		}
		if i > 0 && (isWordByte(text[i-1]) || text[i-1] == '_' || text[i-1] == '/') {
			continue
		}
		if end := i + len(at); end < len(text) && (isWordByte(text[end]) || text[end] == '/') {
			continue
		}
		return true
	}
	return false
}

// isWordByte reports whether b is an ASCII letter, digit or "-".
func isWordByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '-'
}

func asciiEqualFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		x, y := a[i], b[i]
		if 'A' <= x && x <= 'Z' {
			x += 'a' - 'A'
		}
		if 'A' <= y && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}

// indent0to3 returns line without up to three leading spaces, or false
// when it is indented further.
func indent0to3(line string) (string, bool) {
	n := len(line) - len(strings.TrimLeft(line, " "))
	if n > 3 {
		return line, false
	}
	return line[n:], true
}

// fence returns the fence character and length that line opens, or 0.
func fence(line string) (byte, int) {
	rest, ok := indent0to3(line)
	if !ok || len(rest) < 3 || (rest[0] != '`' && rest[0] != '~') {
		return 0, 0
	}
	n := len(rest) - len(strings.TrimLeft(rest, rest[:1]))
	if n < 3 {
		return 0, 0
	}
	return rest[0], n
}

// stripBlocks drops fenced code blocks and quoted lines.
func stripBlocks(body string) string {
	lines := strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n")
	out := make([]string, 0, len(lines))
	var open byte
	openLen := 0
	for _, line := range lines {
		if open != 0 {
			if c, n := fence(line); c == open && n >= openLen && strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), string(c))) == "" {
				open = 0
			}
			continue
		}
		if c, n := fence(line); c != 0 {
			open, openLen = c, n
			continue
		}
		if rest, ok := indent0to3(line); ok && strings.HasPrefix(rest, ">") {
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// stripHTMLComments drops <!-- --> comments, an unterminated one to the end.
func stripHTMLComments(text string) string {
	var b strings.Builder
	for {
		start := strings.Index(text, "<!--")
		if start < 0 {
			b.WriteString(text)
			return b.String()
		}
		b.WriteString(text[:start])
		end := strings.Index(text[start+4:], "-->")
		if end < 0 {
			return b.String()
		}
		b.WriteString(" ")
		text = text[start+4+end+3:]
	}
}

// stripInlineCode drops code spans: a run of backticks up to the next run
// of the same length. A run without a match is kept as text.
func stripInlineCode(text string) string {
	var b strings.Builder
	i := 0
	for i < len(text) {
		if text[i] != '`' {
			b.WriteByte(text[i])
			i++
			continue
		}
		n := 0
		for i+n < len(text) && text[i+n] == '`' {
			n++
		}
		closing := -1
		for j := i + n; j < len(text); {
			if text[j] != '`' {
				j++
				continue
			}
			m := 0
			for j+m < len(text) && text[j+m] == '`' {
				m++
			}
			if m == n {
				closing = j
				break
			}
			j += m
		}
		if closing < 0 {
			b.WriteString(text[i : i+n])
			i += n
			continue
		}
		b.WriteString(" ")
		i = closing + n
	}
	return b.String()
}
