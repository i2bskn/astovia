package gmailsync

import (
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/i2bskn/astovia/internal/ai"
	"github.com/i2bskn/astovia/internal/auth"
	"github.com/i2bskn/astovia/internal/google"
	"github.com/i2bskn/astovia/internal/store"
)

const (
	// aiCallsPerRun bounds the AI calls of one synchronization's detection;
	// the rest is postponed to the next one.
	aiCallsPerRun = 3
	// minRemaining is the time an AI call needs left before the account
	// deadline.
	minRemaining = 25 * time.Second
	// lookback is how far before the last window end messages are read
	// again; window, connectGrace bound how old a message may be: at most
	// 72 hours, and at most 24 hours before the account was connected or
	// the API key was registered.
	lookback     = 10 * time.Minute
	window       = 72 * time.Hour
	connectGrace = 24 * time.Hour
	// fallbackAge is the age after which a message that hit a temporary AI
	// failure becomes a confirmation task instead of waiting, and notifyAge
	// the age after which a detection is not notified at once.
	fallbackAge = 2 * time.Hour
	notifyAge   = 2 * time.Hour
	// retryLimit is how long the window may stay behind for postponed
	// messages; after that it moves on and they become confirmation tasks.
	retryLimit = 6 * time.Hour
	// runLimit is the largest number of messages judged together.
	runLimit     = 10
	contentLimit = 500
	excerptLimit = 80
	// conversationNameLimit bounds the conversation name kept in refs.
	conversationNameLimit = 100
	// relatedFetch is the number of items of a thread read to make at most
	// relatedLimit related tasks for the AI, each group with at most
	// subtaskLimit subtasks. groupWindow is how long a group whose
	// subtasks were all done still takes new subtasks.
	relatedFetch = 30
	relatedLimit = 10
	subtaskLimit = 5
	groupWindow  = 72 * time.Hour
	judgedLimit  = 500
	// listSlack is how far before the window start the list reads, against
	// the rounding of Gmail's after: search.
	listSlack = time.Hour
	// handledKeep is how long after its window a handled message is
	// remembered, beyond its internal date.
	handledKeep = window + 24*time.Hour

	// subjectLimit bounds the subject line of a post, and titleSubject the
	// subject in a notification title.
	subjectLimit = 200
	titleSubject = 40
	// Limits of the recipients and the body of a target post and of a
	// context post.
	targetRecipients    = 10
	contextRecipients   = 3
	targetRecipientLen  = 80
	contextRecipientLen = 60
	targetRawLen        = 300
	contextRawLen       = 120
	targetBodyLimit     = 2000
	contextBodyLimit    = 800
	noActionNotice      = "メール（対応不要）"
	listQuery           = "in:inbox -in:chats -category:promotions -category:social -category:forums -category:updates"
)

// outcome is the item to create for a judgment.
type outcome int

const (
	outcomeAction    outcome = iota // the AI found something to do
	outcomeCandidate                // the AI is not sure the user has something to do
	outcomeNoAction                 // the AI found nothing to do
	outcomeFallback                 // no judgment: ask the user to check
)

// unit is one judgment: a run of messages from others in a thread. posts is
// the context sent to the AI in thread order, targets the IDs judged and
// last the newest of them. related maps the symbols given to the AI to the
// related tasks.
type unit struct {
	key     sourceKey
	subject string
	title   string
	me      string
	posts   []google.Message
	targets map[string]bool
	last    google.Message
	related map[string]store.RelatedTask
}

type ref struct {
	Service          string `json:"service"`
	Account          string `json:"account,omitempty"`
	ConversationID   string `json:"conversation_id,omitempty"`
	ConversationName string `json:"conversation_name,omitempty"`
	MessageID        string `json:"message_id,omitempty"`
	URL              string `json:"url,omitempty"`
}

// detect lists the inbox in the window, judges its threads and moves the
// window to the start of the synchronization when nothing was left over.
func (r *run) detect() error {
	r.setWindow()
	if err := r.detectListed(); err != nil {
		return err
	}
	if r.pending && !r.overdue {
		return nil
	}
	if r.lost > 0 {
		r.warn("window moved on with messages left unread or unjudged", "count", r.lost)
	}
	r.state.LastSyncedAt = nullInt(r.to)
	return nil
}

// setWindow computes the detection window [from, to) and the list pages
// it may read.
func (r *run) setWindow() {
	r.to = r.start.Unix()
	r.from = r.acct.ConnectedAt - int64(connectGrace/time.Second)
	base := r.acct.ConnectedAt
	if r.state.LastSyncedAt.Valid {
		r.from = r.state.LastSyncedAt.Int64 - int64(lookback/time.Second)
		base = r.state.LastSyncedAt.Int64
	}
	for _, v := range []int64{r.ai.ActivatedAt - int64(connectGrace/time.Second), r.acct.ConnectedAt - int64(connectGrace/time.Second), r.to - int64(window/time.Second)} {
		if v > r.from {
			r.from = v
		}
	}
	r.overdue = base < r.to-int64(retryLimit/time.Second)
	r.listLimit = callLimits[callList]
	if r.to-r.from <= int64(shortWindow/time.Second) {
		r.listLimit = listShortLimit
	}
}

func (r *run) inWindow(m google.Message) bool {
	at := m.Internal.Unix()
	return at >= r.from && at < r.to
}

// carry leaves something to the next synchronization.
func (r *run) carry() {
	r.pending = true
	r.carried++
	r.lost++
}

// listed is a thread of the list with the IDs of its listed messages.
type listed struct {
	thread string
	ids    []string
}

// list reads the inbox from an hour before the window start, newest first,
// up to the list pages of the synchronization, and groups the messages by
// thread in the order the threads first appear. Older pages beyond the
// limit are not read; they count as lost.
func (r *run) list() ([]listed, error) {
	query := listQuery + " after:" + strconv.FormatInt(r.from-int64(listSlack/time.Second), 10)
	var threads []listed
	index := map[string]int{}
	page := ""
	for {
		if r.calls[callList] >= r.listLimit {
			// The older pages are not read.
			r.warn("list truncated", "pages", r.calls[callList])
			r.lost++
			return threads, nil
		}
		var list google.MessageList
		err := r.call(callList, func(token string) error {
			var e error
			list, e = r.s.Google.ListMessages(r.ctx, token, query, page)
			return e
		})
		if errors.Is(err, errBudget) {
			// No token could be refreshed within the budget.
			r.carry()
			return threads, nil
		}
		if err != nil {
			// The list has no error about one target: every error stops the
			// synchronization.
			return nil, err
		}
		for _, m := range list.Messages {
			i, ok := index[m.ThreadID]
			if !ok {
				i = len(threads)
				index[m.ThreadID] = i
				threads = append(threads, listed{thread: m.ThreadID})
			}
			threads[i].ids = append(threads[i].ids, m.ID)
		}
		if list.NextPageToken == "" {
			return threads, nil
		}
		page = list.NextPageToken
	}
}

// detectListed reads the listed threads that have messages not handled
// yet and judges them; what does not fit in the budget is carried over.
func (r *run) detectListed() error {
	threads, err := r.list()
	if err != nil {
		return err
	}
	for _, l := range threads {
		if r.s.allHandled(r.acct, l.ids) {
			continue
		}
		t, err := r.thread(l.thread)
		switch {
		case errors.Is(err, errBudget):
			r.carry()
			continue
		case err != nil && stops(err):
			return err
		case err != nil:
			r.warn("thread skipped", "error", apiCode(err))
			r.lost++
			until := map[string]int64{}
			for _, id := range l.ids {
				until[id] = r.to + int64(handledKeep/time.Second)
			}
			r.s.markHandled(r.acct, until)
			continue
		}
		carried := r.carried
		msgs := visible(t)
		if err = r.detectThread(t.ID, msgs); err != nil {
			return err
		}
		if r.carried != carried {
			continue
		}
		r.s.markHandled(r.acct, r.handledIDs(l, t))
	}
	return nil
}

// handledIDs returns the listed messages of a thread judged to the end that
// need not be read again, with the time to forget each: those received
// before the window end, and those that can never be targets. Messages
// received after the window end are judged by the next synchronization, so
// they are not recorded and their thread is read again.
func (r *run) handledIDs(l listed, t google.Thread) map[string]int64 {
	byID := map[string]google.Message{}
	for _, m := range t.Messages {
		byID[m.ID] = m
	}
	until := map[string]int64{}
	for _, id := range l.ids {
		m, ok := byID[id]
		if !ok {
			// No longer in the thread; nothing to read.
			until[id] = r.to + int64(handledKeep/time.Second)
			continue
		}
		at := m.Internal.Unix()
		if at < r.to || excluded(m) {
			until[id] = at + int64(handledKeep/time.Second)
		}
	}
	return until
}

// excluded reports whether a message can never be a target: a draft, in
// the trash or spam, sent by the user, out of the inbox, or automatic.
func excluded(m google.Message) bool {
	return m.HasLabel(labelDraft) || m.HasLabel(labelTrash) || m.HasLabel(labelSpam) || m.HasLabel(labelSent) || !m.HasLabel(labelInbox) || automatic(m)
}

// eligible reports whether a message may be judged: in the window, not
// sent by the user, in the inbox and not automatic.
func (r *run) eligible(m google.Message) bool {
	return r.inWindow(m) && !excluded(m)
}

// exists reports whether the user has an item with the key.
func (r *run) exists(key string) (bool, error) {
	exists := false
	err := r.s.Store.GmailSync(r.parent, r.acct.UserID, r.acct.ID, r.acct.Generation, func(tx *store.GmailTx) error {
		_, found, e := tx.LockItemBySourceKey(key)
		exists = found
		return e
	})
	return exists, err
}

// detectThread splits a thread into runs of messages from others
// (separated by the user's messages and by messages judged already, at
// most runLimit each) and judges them oldest first. A run that is postponed
// postpones the rest of the thread. msgs are the visible messages in
// thread order.
func (r *run) detectThread(thread string, msgs []google.Message) error {
	email := r.acct.Email
	keys, err := r.s.Store.GmailJudgedKeys(r.parent, r.acct.UserID, threadPrefix(email, kindRun, thread), r.from, judgedLimit)
	if err != nil {
		return err
	}
	var judged [][2]int
	for _, k := range keys {
		sk, ok := parseSourceKey(k)
		if !ok || sk.Email != email || sk.Thread != thread {
			continue
		}
		i, j := indexID(msgs, sk.First), indexID(msgs, sk.Last)
		switch {
		case i < 0 && j < 0:
			continue
		case i < 0:
			i = j
		case j < 0:
			j = i
		}
		judged = append(judged, [2]int{i, j})
	}
	isJudged := func(pos int) bool {
		for _, j := range judged {
			if pos >= j[0] && pos <= j[1] {
				return true
			}
		}
		return false
	}
	var runs [][]google.Message
	var cur []google.Message
	flush := func() {
		for len(cur) > 0 {
			n := min(len(cur), runLimit)
			runs = append(runs, cur[:n])
			cur = cur[n:]
		}
		cur = nil
	}
	for i, m := range msgs {
		if !r.inWindow(m) {
			continue
		}
		switch {
		case m.HasLabel(labelSent), isJudged(i):
			flush()
		case r.eligible(m):
			cur = append(cur, m)
		}
	}
	flush()
	for _, run := range runs {
		last := run[len(run)-1]
		key := sourceKey{Email: email, Thread: thread, First: run[0].ID, Last: last.ID}
		found, err := r.exists(key.String())
		if err != nil {
			return err
		}
		if found {
			continue
		}
		i, j := indexID(msgs, run[0].ID), indexID(msgs, last.ID)
		u := &unit{key: key, subject: subjectOf(last, msgs), me: myName(msgs, email), posts: msgs[max(0, i-ai.PostsBefore):min(len(msgs), j+1+ai.PostsAfter)],
			targets: map[string]bool{}, last: last}
		u.title = "メール"
		if s := truncate(u.subject, titleSubject); s != "" {
			u.title = "メール: " + s
		}
		for _, m := range run {
			u.targets[m.ID] = true
		}
		handled, err := r.judge(u)
		if err != nil {
			return err
		}
		if !handled {
			break
		}
	}
	return nil
}

// subjectOf returns the subject of the message on one line, or the first
// subject of the thread when it has none.
func subjectOf(m google.Message, msgs []google.Message) string {
	if v := cleanText(oneLine(m.Subject)); v != "" {
		return v
	}
	for _, o := range msgs {
		if v := cleanText(oneLine(o.Subject)); v != "" {
			return v
		}
	}
	return ""
}

// myName is the user's name as the sender of the newest message the user
// sent in the thread, or the address of the account.
func myName(msgs []google.Message, email string) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].HasLabel(labelSent) {
			if v := cleanText(oneLine(msgs[i].FromName)); v != "" {
				return v
			}
		}
	}
	return email
}

// judge asks the AI about a unit and creates its item, and reports whether
// it was handled. The AI settings are handled like slacksync's.
func (r *run) judge(u *unit) (bool, error) {
	switch {
	case r.aiStopped:
		r.carry()
		return false, nil
	case r.aiFallbackOnly:
		return r.create(u, outcomeFallback, ai.MentionResult{})
	case r.aiStale || r.detectCalls >= aiCallsPerRun || r.deadlineNear():
		return r.postpone(u)
	}
	cur, apiKey, gate, err := r.reloadAI()
	switch {
	case err != nil:
		return false, err
	case gate == gateStopped:
		r.carry()
		return false, nil
	case gate == gateStale:
		return r.postpone(u)
	case gate == gateInactive:
		return r.create(u, outcomeFallback, ai.MentionResult{})
	}
	in, err := r.input(u)
	if err != nil {
		return false, err
	}
	r.detectCalls++
	res, err := r.s.AI.JudgeMail(r.ctx, apiKey, cur.Model, in)
	class, err := r.classifyAI(cur, err)
	if err != nil {
		return false, err
	}
	switch class {
	case aiOK:
		switch res.Decision {
		case ai.DecisionTask:
			return r.create(u, outcomeAction, res)
		case ai.DecisionCandidate:
			return r.create(u, outcomeCandidate, res)
		}
		return r.create(u, outcomeNoAction, res)
	case aiFailed, aiUnusable:
		return r.create(u, outcomeFallback, ai.MentionResult{})
	}
	// A temporary failure: an old message becomes a confirmation task, a
	// recent one waits for the next synchronization.
	if u.last.Internal.Before(r.s.Now().Add(-fallbackAge)) {
		return r.create(u, outcomeFallback, ai.MentionResult{})
	}
	return r.postpone(u)
}

// postpone leaves a unit to the next synchronization, or makes it a
// confirmation task when the window has stayed behind for too long.
func (r *run) postpone(u *unit) (bool, error) {
	if r.overdue {
		return r.create(u, outcomeFallback, ai.MentionResult{})
	}
	r.carry()
	return false, nil
}

// addressPattern finds the characters an address cannot touch, for the
// search of the account address in a header that cannot be parsed.
var addressPattern = regexp.MustCompile(`[A-Za-z0-9._%+-]`)

// normalizeAddress returns the form addresses are compared in: lower case,
// without the +suffix of the local part, and for gmail.com (and
// googlemail.com, read as gmail.com) without the dots of the local part.
func normalizeAddress(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	local, domain, ok := strings.Cut(v, "@")
	if !ok {
		return v
	}
	if i := strings.IndexByte(local, '+'); i >= 0 {
		local = local[:i]
	}
	if domain == "googlemail.com" {
		domain = "gmail.com"
	}
	if domain == "gmail.com" {
		local = strings.ReplaceAll(local, ".", "")
	}
	return local + "@" + domain
}

// containsAddress reports whether the raw value of a header that cannot be
// parsed contains the address on its own: not next to a character that can
// be part of an address.
func containsAddress(raw, email string) bool {
	raw, email = strings.ToLower(raw), strings.ToLower(email)
	for start := 0; ; {
		i := strings.Index(raw[start:], email)
		if i < 0 {
			return false
		}
		i += start
		end := i + len(email)
		before := i == 0 || !addressPattern.MatchString(raw[i-1:i])
		after := end == len(raw) || !addressPattern.MatchString(raw[end:end+1])
		if before && after {
			return true
		}
		start = i + 1
	}
}

// hasAddress reports whether the parsed list or, when the header could not
// be parsed, its raw value has the account address.
func hasAddress(list []google.Address, parsed bool, raw, email string) bool {
	if !parsed {
		return containsAddress(raw, email)
	}
	want := normalizeAddress(email)
	for _, a := range list {
		if normalizeAddress(a.Email) == want {
			return true
		}
	}
	return false
}

// position is the user's position on a message, computed by Astovia.
func position(m google.Message, email string) string {
	switch {
	case m.HasLabel(labelSent):
		return "送信者（自分）"
	case hasAddress(m.ToList, m.ToParsed, m.To, email):
		return "To"
	case hasAddress(m.CcList, m.CcParsed, m.Cc, email):
		return "CC"
	}
	return "なし"
}

// recipients formats a recipient header for the AI: the parsed addresses
// as "name <address>" (or the address alone), the account's first, at
// most count of them cut at length characters each and the rest counted;
// a header that cannot be parsed is its raw value cut at rawLength.
func recipients(list []google.Address, parsed bool, raw, email string, count, length, rawLength int) string {
	if !parsed {
		return truncate(strings.TrimSpace(oneLine(raw)), rawLength)
	}
	want := normalizeAddress(email)
	var mine, others []string
	for _, a := range list {
		v := oneLine(a.Email)
		if name := strings.TrimSpace(oneLine(a.Name)); name != "" {
			v = name + " <" + v + ">"
		}
		v = truncate(v, length)
		if normalizeAddress(a.Email) == want {
			mine = append(mine, v)
		} else {
			others = append(others, v)
		}
	}
	all := append(mine, others...)
	if len(all) <= count {
		return strings.Join(all, ", ")
	}
	return strings.Join(all[:count], ", ") + ", ほか " + strconv.Itoa(len(all)-count) + " 件"
}

// postBody builds the body of a post for the AI: four lines written by
// Astovia (subject, the user's position, To and Cc), an empty line and the
// text.
func postBody(m google.Message, email string, target bool) string {
	count, length, rawLength, bodyLimit := contextRecipients, contextRecipientLen, contextRawLen, contextBodyLimit
	if target {
		count, length, rawLength, bodyLimit = targetRecipients, targetRecipientLen, targetRawLen, targetBodyLimit
	}
	var b strings.Builder
	b.WriteString("件名: " + truncate(strings.TrimSpace(oneLine(m.Subject)), subjectLimit) + "\n")
	b.WriteString("自分の位置: " + position(m, email) + "\n")
	b.WriteString("宛先: " + recipients(m.ToList, m.ToParsed, m.To, email, count, length, rawLength) + "\n")
	b.WriteString("CC: " + recipients(m.CcList, m.CcParsed, m.Cc, email, count, length, rawLength) + "\n\n")
	b.WriteString(truncate(bodyText(m.Text), bodyLimit))
	return b.String()
}

// post converts a message for the AI.
func (r *run) post(m google.Message, target bool) ai.SlackPost {
	author := cleanText(oneLine(m.FromName))
	if author == "" {
		author = m.FromEmail
	}
	if author == "" {
		author = "unknown"
	}
	return ai.SlackPost{ID: m.ID, Author: author, AuthorID: strings.ToLower(m.FromEmail), IsMe: m.HasLabel(labelSent), At: m.Internal, Target: target, Body: postBody(m, r.acct.Email, target)}
}

// input builds the AI input of a unit.
func (r *run) input(u *unit) (ai.SlackInput, error) {
	loc, err := r.location()
	if err != nil {
		return ai.SlackInput{}, err
	}
	in := ai.SlackInput{Me: ai.SlackPerson{ID: r.acct.Email, Name: u.me}, Location: loc, Workspace: r.acct.Email, ConversationType: ai.MailThread,
		ChannelName: truncate(u.subject, subjectLimit), RelatedTasks: []ai.RelatedTask{}}
	for _, m := range u.posts {
		in.Posts = append(in.Posts, r.post(m, u.targets[m.ID]))
	}
	tasks, err := r.s.Store.RelatedTasks(r.parent, r.acct.UserID, r.relatedScope(u))
	if err != nil {
		return ai.SlackInput{}, err
	}
	u.related = map[string]store.RelatedTask{}
	for i, t := range tasks {
		id := "t" + strconv.Itoa(i+1)
		u.related[id] = t
		in.RelatedTasks = append(in.RelatedTasks, ai.RelatedTask{ID: id, Task: t.Content, Subtasks: t.Subtasks, Closed: t.Closed})
	}
	return in, nil
}

// relatedScope selects the related tasks of a unit's thread: its runs and
// the groups made for it. The thread narrows them enough, so they are not
// limited by age.
func (r *run) relatedScope(u *unit) store.RelatedScope {
	now := r.s.Now()
	group := threadPrefix(r.acct.Email, kindGroup, u.key.Thread)
	return store.RelatedScope{
		Source:        store.SourceGmail,
		Prefixes:      []string{threadPrefix(r.acct.Email, kindRun, u.key.Thread), group},
		GroupPrefixes: []string{group},
		ClosedSince:   now.Add(-groupWindow).Unix(),
		FetchLimit:    relatedFetch,
		Limit:         relatedLimit,
		SubtaskLimit:  subtaskLimit,
	}
}

// create creates the item of a unit unless it exists, and queues its
// detection notification when the message is recent enough and arrived
// after the account was connected and the AI settings were made.
// Candidates are tracked like tasks but not notified; event items are not
// tracked, since a reply does not add the event to the calendar.
func (r *run) create(u *unit, o outcome, res ai.MentionResult) (bool, error) {
	link := gmailURL(threadURL(r.acct.Email, u.key.Thread))
	now := r.s.Now().Unix()
	id, err := auth.Random()
	if err != nil {
		return false, err
	}
	place := truncate(u.subject, conversationNameLimit)
	refs, err := json.Marshal([]ref{{Service: store.SourceGmail, Account: r.acct.Email, ConversationID: u.key.Thread, ConversationName: place, MessageID: u.last.ID, URL: link.String}})
	if err != nil {
		return false, err
	}
	key := u.key.String()
	item := store.ActionItem{ID: id, Status: "open", Refs: string(refs), Version: 1, CreatedAt: now, UpdatedAt: now,
		Source: nullString(store.SourceGmail), SourceKey: nullString(key), Tracking: 1, DetectedAt: nullInt(now)}
	// The group parent shares the thread's reference, without a message.
	groupRefs, err := json.Marshal([]ref{{Service: store.SourceGmail, Account: r.acct.Email, ConversationID: u.key.Thread, ConversationName: place, URL: link.String}})
	if err != nil {
		return false, err
	}
	switch o {
	case outcomeAction, outcomeCandidate:
		item.Content = res.Task
		if o == outcomeCandidate {
			item.Status = "candidate"
		}
		setEvent(&item, res.Event, link, now)
	case outcomeNoAction:
		item.Content = res.Summary
		if item.Content == "" {
			item.Content = noActionNotice
		}
		item.Status, item.Tracking, item.AutoReason, item.AutoRef, item.AutoAt = "done", 0, nullString(ReasonNoAction), link, nullInt(now)
	default:
		what := excerpt(u.subject, excerptLimit)
		if what == "" {
			what = excerpt(bodyText(u.last.Text), excerptLimit)
		}
		item.Content = "メールを確認: " + what
	}
	item.Content = truncate(item.Content, contentLimit)
	sent := u.last.Internal.Unix()
	notify := sent >= r.acct.ConnectedAt && sent >= r.to-int64(notifyAge/time.Second) && sent >= r.ai.ActivatedAt
	err = r.s.Store.GmailSync(r.parent, r.acct.UserID, r.acct.ID, r.acct.Generation, func(tx *store.GmailTx) error {
		if _, found, e := tx.LockItemBySourceKey(key); e != nil || found {
			return e
		}
		madeGroup := false
		if o == outcomeAction && item.Status == "open" {
			parent, made, e := r.groupParent(tx, u, res, item.ID, string(groupRefs), now)
			if e != nil {
				return e
			}
			item.ParentID, madeGroup = parent, made
		}
		created, e := tx.CreateItem(item)
		if errors.Is(e, store.ErrInvalidParent) && !madeGroup {
			// The group changed; the item is created on its own.
			r.warn("grouping skipped", "reason", "invalid_parent")
			item.ParentID = sql.NullString{}
			created, e = tx.CreateItem(item)
		}
		if e != nil || !created || item.Status != "open" || !notify || tx.Account.ImmediateNotify != 1 {
			return e
		}
		return tx.Enqueue(detectedNotification(u.title, item, now))
	})
	return err == nil, err
}

// groupParent returns the parent for a new open task that the AI judged to
// belong to the same matter as a related task, and whether a group parent
// was made for it. It works like chatworksync's: a related group takes the
// task while it is open, or while it was completed by its subtasks within
// groupWindow; a related plain task still open, without a parent and
// without subtasks, is put under a new group parent named after the topic.
func (r *run) groupParent(tx *store.GmailTx, u *unit, res ai.MentionResult, itemID, refs string, now int64) (sql.NullString, bool, error) {
	target, ok := u.related[res.SameAs]
	if res.SameAs == "" || !ok {
		return sql.NullString{}, false, nil
	}
	if target.Group {
		p, found, e := tx.LockItem(target.ID)
		if e != nil {
			return sql.NullString{}, false, e
		}
		closedRecently := p.Status == "done" && p.AutoReason.String == store.ReasonChildrenDone && p.AutoAt.Valid && p.AutoAt.Int64 >= now-int64(groupWindow/time.Second)
		if !found || p.ParentID.Valid || (p.Status != "open" && !closedRecently) {
			r.warn("grouping skipped", "reason", "group_changed")
			return sql.NullString{}, false, nil
		}
		return nullString(p.ID), false, nil
	}
	if res.Topic == "" {
		return sql.NullString{}, false, nil
	}
	t, found, e := tx.LockItem(target.ID)
	if e != nil {
		return sql.NullString{}, false, e
	}
	if !found || t.Status != "open" || t.ParentID.Valid {
		r.warn("grouping skipped", "reason", "task_changed")
		return sql.NullString{}, false, nil
	}
	n, e := tx.ChildCount(t.ID)
	if e != nil {
		return sql.NullString{}, false, e
	}
	if n > 0 {
		r.warn("grouping skipped", "reason", "task_changed")
		return sql.NullString{}, false, nil
	}
	id, e := auth.Random()
	if e != nil {
		return sql.NullString{}, false, e
	}
	group := store.ActionItem{ID: id, Content: truncate(res.Topic, contentLimit), Status: "open", Refs: refs, Version: 1, CreatedAt: now, UpdatedAt: now,
		Source: nullString(store.SourceGmail), SourceKey: nullString(threadPrefix(r.acct.Email, kindGroup, u.key.Thread) + itemID), DetectedAt: nullInt(now), Kind: store.KindTask}
	created, e := tx.CreateGroup(group)
	if e != nil {
		return sql.NullString{}, false, e
	}
	if !created {
		return sql.NullString{}, false, errors.New("gmailsync: group parent key already exists")
	}
	if e = tx.SetParent(t.ID, group.ID, now); e != nil {
		return sql.NullString{}, false, e
	}
	return nullString(group.ID), true, nil
}

// setEvent makes item an untracked event item when the AI found an event.
// An event that has already started is dismissed at once; ref is the
// evidence of that dismissal.
func setEvent(item *store.ActionItem, e *ai.Event, ref sql.NullString, now int64) {
	if e == nil {
		return
	}
	start := e.Start.Unix()
	item.Kind, item.EventTitle, item.EventStartAt, item.DueAt, item.Tracking = store.KindEvent, nullString(e.Title), nullInt(start), nullInt(start), 0
	if !e.End.IsZero() {
		item.EventEndAt = nullInt(e.End.Unix())
	}
	if start <= now {
		item.Status, item.AutoReason, item.AutoRef, item.AutoAt = "dismissed", nullString(store.ReasonEventPassed), ref, nullInt(now)
	}
}

func detectedNotification(title string, item store.ActionItem, now int64) store.Notification {
	id, err := auth.Random()
	if err != nil {
		// The ID only needs to be unique; the dedupe key keeps the
		// notification single either way.
		id = item.ID + "-" + strconv.FormatInt(item.Version, 10)
	}
	return store.Notification{ID: id, Kind: store.NotificationDetected, ActionItemID: nullString(item.ID),
		DedupeKey: "detected:" + item.ID + ":" + strconv.FormatInt(item.Version, 10),
		Title:     title, Body: item.Content, URL: "/tasks/" + item.ID, ScheduledAt: now, CreatedAt: now}
}
