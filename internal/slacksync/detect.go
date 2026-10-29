package slacksync

import (
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/i2bskn/astovia/internal/ai"
	"github.com/i2bskn/astovia/internal/auth"
	"github.com/i2bskn/astovia/internal/slack"
	"github.com/i2bskn/astovia/internal/store"
)

const (
	// aiCallsPerRun bounds the AI calls of one synchronization's detection;
	// the rest is postponed to the next one.
	aiCallsPerRun = 3
	// minRemaining is the time an AI call needs left before the connection
	// deadline.
	minRemaining = 25 * time.Second
	// lookback is how far before the last window end messages are read
	// again; window, connectGrace bound how old a message may be: at most
	// 72 hours, and at most 24 hours before the Slack connection or the API
	// key was registered.
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

	searchCount  = 100
	searchPages  = 2
	historyLimit = 100
	historyPages = 2
	groupDMLimit = 20
	groupDMList  = 200
	judgedLimit  = 500
	// relatedFetch is the number of items of a conversation read to make
	// at most relatedLimit related tasks for the AI, each group with at
	// most subtaskLimit subtasks. groupWindow is how long a group whose
	// subtasks were all done still takes new subtasks, and how old the
	// items of a direct message conversation outside threads may be.
	relatedFetch = 30
	relatedLimit = 10
	subtaskLimit = 5
	groupWindow  = 72 * time.Hour
	// runLimit is the largest number of messages judged together.
	runLimit     = 10
	contentLimit = 500
	excerptLimit = 80
	// conversationNameLimit bounds the conversation name kept in refs.
	conversationNameLimit = 100
	noActionNotice        = "Slack の発言（対応不要）"
)

// outcome is the item to create for a judgment.
type outcome int

const (
	outcomeAction    outcome = iota // the AI found something to do
	outcomeCandidate                // the AI is not sure the user has something to do
	outcomeNoAction                 // the AI found nothing to do
	outcomeFallback                 // no judgment: ask the user to check
)

// conversation is a direct message channel, a group direct message channel
// or one of their threads, whose messages are judged in runs.
type conversation struct {
	kind     string // ai.SlackDM or ai.SlackGroupDM
	channel  slack.Channel
	thread   string
	latest   string
	messages []slack.Message // time order
}

// unit is one judgment: a mention, or a run of messages from others in a
// conversation. posts is the context sent to the AI in time order, targets
// the timestamps judged and last the newest of them.
//
// thread is the thread of the conversation the unit belongs to, for the
// related tasks and the group parent: the key's thread, or for a message
// outside a thread in a channel the message itself (so that a mention in a
// thread and one in its first message share a conversation), or noThread
// for direct messages outside threads. related maps the symbols given to
// the AI to the related tasks.
type unit struct {
	key         sourceKey
	convType    string
	channelName string
	title       string
	posts       []slack.Message
	targets     map[string]bool
	last        slack.Message
	thread      string
	related     map[string]store.RelatedTask
}

type ref struct {
	Service          string `json:"service"`
	Account          string `json:"account,omitempty"`
	ConversationID   string `json:"conversation_id,omitempty"`
	ConversationName string `json:"conversation_name,omitempty"`
	MessageID        string `json:"message_id,omitempty"`
	URL              string `json:"url,omitempty"`
}

// detect finds and judges the messages of the window and moves the window
// to the start of the synchronization when nothing was left over.
func (r *run) detect() error {
	r.setWindow()
	date := time.Unix(r.from, 0).UTC().AddDate(0, 0, -2).Format("2006-01-02")
	var mentionHits, dmHits []slack.Message
	var err error
	if r.conn.DetectMentions == 1 {
		if mentionHits, err = r.search("<@" + r.conn.SlackUserID + "> after:" + date); err != nil {
			return err
		}
	}
	if r.conn.DetectDMs == 1 {
		if dmHits, err = r.search("to:me after:" + date); err != nil {
			return err
		}
	}
	var convs []*conversation
	scanned := map[string]bool{}
	if r.conn.DetectDMs == 1 {
		list, err := r.dmConversations(dmHits)
		if err != nil {
			return err
		}
		convs = append(convs, list...)
	}
	if r.conn.DetectGroupDMs == 1 {
		list, err := r.groupDMConversations(scanned)
		if err != nil {
			return err
		}
		convs = append(convs, list...)
	}
	for _, m := range r.mentionHits(mentionHits, scanned) {
		if err = r.mention(m); err != nil {
			return err
		}
	}
	for _, c := range convs {
		if err = r.conversation(c); err != nil {
			return err
		}
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

// setWindow computes the detection window [from, to).
func (r *run) setWindow() {
	r.to = r.start.Unix()
	r.from = r.conn.ConnectedAt - int64(connectGrace/time.Second)
	base := r.conn.ConnectedAt
	if r.state.LastSyncedAt.Valid {
		r.from = r.state.LastSyncedAt.Int64 - int64(lookback/time.Second)
		base = r.state.LastSyncedAt.Int64
	}
	for _, v := range []int64{r.ai.ActivatedAt - int64(connectGrace/time.Second), r.conn.ConnectedAt - int64(connectGrace/time.Second), r.to - int64(window/time.Second)} {
		if v > r.from {
			r.from = v
		}
	}
	r.firstImport = !r.state.LastSyncedAt.Valid
	r.overdue = base < r.to-int64(retryLimit/time.Second)
}

func (r *run) inWindow(ts string) bool {
	return validTS(ts) && compareTS(ts, secondsTS(r.from)) >= 0 && compareTS(ts, secondsTS(r.to)) < 0
}

// carry leaves something to the next synchronization.
func (r *run) carry() {
	r.pending = true
	r.lost++
}

// search runs a search for up to searchPages pages, until the results reach
// the start of the window. A failed search is carried over.
func (r *run) search(query string) ([]slack.Message, error) {
	var out []slack.Message
	from := secondsTS(r.from)
	for page := 1; page <= searchPages; page++ {
		if !r.take(callSearch) {
			r.carry()
			return out, nil
		}
		res, err := r.s.Slack.SearchMessages(r.ctx, r.token, query, searchCount, page)
		if err != nil {
			if stops(err) {
				return nil, err
			}
			r.warn("search failed", "error", apiCode(err))
			r.carry()
			return out, nil
		}
		out = append(out, res.Matches...)
		if page >= res.Pages || len(res.Matches) == 0 {
			return out, nil
		}
		if last := res.Matches[len(res.Matches)-1].TS; validTS(last) && compareTS(last, from) < 0 {
			return out, nil
		}
	}
	r.warn("search truncated", "pages", searchPages)
	return out, nil
}

// fetch reads the messages of a conversation since the start of the window,
// up to historyPages pages, in time order. ok is false when it was skipped
// (Slack refused it) or carried over (the budget is used up).
func (r *run) fetch(channel, thread string) ([]slack.Message, bool, error) {
	p := slack.HistoryParams{Channel: channel, Oldest: secondsTS(r.from), Limit: historyLimit}
	call := r.history
	if thread != noThread {
		p.TS, call = thread, r.replies
	} else {
		p.Latest = secondsTS(r.to)
	}
	var out []slack.Message
	for page := 0; page < historyPages; page++ {
		res, err := call(p)
		switch {
		case errors.Is(err, errBudget):
			r.carry()
			return nil, false, nil
		case err != nil && stops(err):
			return nil, false, err
		case err != nil:
			r.warn("conversation skipped", "error", apiCode(err))
			return nil, false, nil
		}
		out = append(out, res.Messages...)
		if !res.HasMore || res.NextCursor == "" {
			return sortMessages(out), true, nil
		}
		p.Cursor = res.NextCursor
	}
	r.warn("conversation truncated", "pages", historyPages)
	return sortMessages(out), true, nil
}

// sortMessages returns the messages with a valid timestamp in time order,
// keeping the first of each timestamp.
func sortMessages(list []slack.Message) []slack.Message {
	seen := map[string]bool{}
	out := make([]slack.Message, 0, len(list))
	for _, m := range list {
		if !validTS(m.TS) || seen[m.TS] {
			continue
		}
		seen[m.TS] = true
		out = append(out, m)
	}
	sort.SliceStable(out, func(i, j int) bool { return compareTS(out[i].TS, out[j].TS) < 0 })
	return out
}

// newest returns the newest timestamp of the messages in the window.
func (r *run) newest(list []slack.Message) string {
	latest := ""
	for _, m := range list {
		if r.inWindow(m.TS) && (latest == "" || compareTS(m.TS, latest) > 0) {
			latest = m.TS
		}
	}
	return latest
}

// sortConversations orders conversations by their newest message, newest
// first, so that the budget runs out on the older ones.
func sortConversations(list []*conversation) {
	sort.SliceStable(list, func(i, j int) bool {
		a, b := list[i].latest, list[j].latest
		if a == "" || b == "" {
			return b == "" && a != ""
		}
		return compareTS(a, b) > 0
	})
}

// dmConversations reads the direct message conversations (channels and
// threads) where the "to:me" search found messages in the window.
func (r *run) dmConversations(hits []slack.Message) ([]*conversation, error) {
	byKey := map[string]*conversation{}
	var list []*conversation
	for _, m := range hits {
		if !m.Channel.IsIM || !idPattern.MatchString(m.Channel.ID) || !r.inWindow(m.TS) {
			continue
		}
		thread := searchThread(m)
		k := m.Channel.ID + ":" + thread
		c, ok := byKey[k]
		if !ok {
			c = &conversation{kind: ai.SlackDM, channel: m.Channel, thread: thread}
			byKey[k] = c
			list = append(list, c)
		}
		if c.latest == "" || compareTS(m.TS, c.latest) > 0 {
			c.latest = m.TS
		}
	}
	sortConversations(list)
	out := make([]*conversation, 0, len(list))
	for _, c := range list {
		msgs, ok, err := r.fetch(c.channel.ID, c.thread)
		if err != nil {
			return nil, err
		}
		if ok {
			c.messages = msgs
			out = append(out, c)
		}
	}
	return out, nil
}

// groupDMConversations reads the first groupDMLimit group direct message
// channels (by ID) and their threads with replies in the window. scanned
// receives the channels whose messages were read.
func (r *run) groupDMConversations(scanned map[string]bool) ([]*conversation, error) {
	if !r.take(callConversations) {
		r.carry()
		return nil, nil
	}
	channels, err := r.s.Slack.UserConversations(r.ctx, r.token, "mpim", groupDMList)
	if err != nil {
		if stops(err) {
			return nil, err
		}
		r.warn("group DM list failed", "error", apiCode(err))
		r.carry()
		return nil, nil
	}
	valid := channels[:0]
	for _, ch := range channels {
		if idPattern.MatchString(ch.ID) {
			valid = append(valid, ch)
		}
	}
	sort.Slice(valid, func(i, j int) bool { return valid[i].ID < valid[j].ID })
	if len(valid) > groupDMLimit {
		r.warn("group DMs over the limit are not read", "count", len(valid)-groupDMLimit)
		valid = valid[:groupDMLimit]
	}
	from := secondsTS(r.from)
	var out []*conversation
	for _, ch := range valid {
		msgs, ok, err := r.fetch(ch.ID, noThread)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		scanned[ch.ID] = true
		out = append(out, &conversation{kind: ai.SlackGroupDM, channel: ch, thread: noThread, messages: msgs, latest: r.newest(msgs)})
		for _, m := range msgs {
			if threadOf(m) != noThread || !validTS(m.LatestReply) || compareTS(m.LatestReply, from) < 0 {
				continue
			}
			replies, ok, err := r.fetch(ch.ID, m.TS)
			if err != nil {
				return nil, err
			}
			if ok {
				out = append(out, &conversation{kind: ai.SlackGroupDM, channel: ch, thread: m.TS, messages: replies, latest: r.newest(replies)})
			}
		}
	}
	sortConversations(out)
	return out, nil
}

// mentionHits returns the mentions of the user in the window, newest
// first. Mentions in direct messages are left to DM detection when it is
// on, and those in group direct messages when their channel was read.
func (r *run) mentionHits(hits []slack.Message, scanned map[string]bool) []slack.Message {
	needle := "<@" + r.conn.SlackUserID + ">"
	seen := map[string]bool{}
	var out []slack.Message
	for _, m := range hits {
		if !idPattern.MatchString(m.Channel.ID) || !r.inWindow(m.TS) || !strings.Contains(m.Text, needle) || !r.eligible(m) {
			continue
		}
		if (m.Channel.IsIM && r.conn.DetectDMs == 1) || (m.Channel.IsMPIM && r.conn.DetectGroupDMs == 1 && scanned[m.Channel.ID]) {
			continue
		}
		if k := m.Channel.ID + ":" + m.TS; !seen[k] {
			seen[k] = true
			out = append(out, m)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return compareTS(out[i].TS, out[j].TS) > 0 })
	return out
}

// exists reports whether the user has an item with the key.
func (r *run) exists(key string) (bool, error) {
	exists := false
	err := r.s.Store.SlackSync(r.parent, r.conn.UserID, r.conn.Generation, func(tx *store.SlackTx) error {
		_, found, e := tx.LockItemBySourceKey(key)
		exists = found
		return e
	})
	return exists, err
}

// mention judges one mention. Nothing is requested when its item exists.
// The context is the thread, or the 10 messages before and 3 after.
func (r *run) mention(m slack.Message) error {
	thread := searchThread(m)
	key := sourceKey{Team: r.conn.TeamID, Kind: kindMention, Channel: m.Channel.ID, Thread: thread, First: m.TS, Last: m.TS}
	if found, err := r.exists(key.String()); err != nil || found {
		return err
	}
	var around []slack.Message
	var err error
	if thread != noThread {
		var page slack.HistoryPage
		page, err = r.replies(slack.HistoryParams{Channel: m.Channel.ID, TS: thread, Limit: historyLimit})
		around = page.Messages
	} else {
		var before, after slack.HistoryPage
		before, err = r.history(slack.HistoryParams{Channel: m.Channel.ID, Latest: m.TS, Inclusive: true, Limit: ai.PostsBefore + 1})
		if err == nil {
			after, err = r.history(slack.HistoryParams{Channel: m.Channel.ID, Oldest: m.TS, Limit: ai.PostsAfter})
		}
		around = append(before.Messages, after.Messages...)
	}
	postponed := false
	switch {
	case errors.Is(err, errBudget):
		postponed, around = true, nil
	case err != nil && stops(err):
		return err
	case err != nil:
		// Judged without context.
		r.warn("mention context skipped", "error", apiCode(err))
		around = nil
	}
	list := sortMessages(append(around, m))
	i := indexTS(list, m.TS)
	target := list[i]
	if target.User == "" {
		target.User = m.User
	}
	if !r.eligible(target) {
		return nil
	}
	u := &unit{key: key, convType: ai.SlackChannel, channelName: m.Channel.Name, title: "Slack: メンション",
		posts: list[max(0, i-ai.PostsBefore):min(len(list), i+1+ai.PostsAfter)], targets: map[string]bool{m.TS: true}, last: target}
	u.last.Permalink = m.Permalink
	switch {
	case m.Channel.IsIM:
		u.convType = ai.SlackDM
	case m.Channel.IsMPIM:
		u.convType = ai.SlackGroupDM
	}
	u.thread = thread
	if thread == noThread && u.convType == ai.SlackChannel {
		u.thread = m.TS
	}
	if postponed {
		_, err = r.postpone(u)
		return err
	}
	_, err = r.judge(u)
	return err
}

func indexTS(list []slack.Message, ts string) int {
	for i, m := range list {
		if m.TS == ts {
			return i
		}
	}
	return -1
}

// conversation splits a conversation into runs of messages from others
// (separated by the user's messages and by messages judged already, at most
// runLimit each) and judges them oldest first. A run that is postponed
// postpones the rest of the conversation.
func (r *run) conversation(c *conversation) error {
	prefix := conversationPrefix(r.conn.TeamID, kindDM, c.channel.ID, c.thread)
	keys, err := r.s.Store.SlackJudgedKeys(r.parent, r.conn.UserID, prefix, r.from, judgedLimit)
	if err != nil {
		return err
	}
	var judged [][2]string
	for _, k := range keys {
		if sk, ok := parseSourceKey(k); ok && sk.Kind == kindDM && sk.Channel == c.channel.ID && sk.Thread == c.thread {
			judged = append(judged, [2]string{sk.First, sk.Last})
		}
	}
	isJudged := func(ts string) bool {
		for _, j := range judged {
			if compareTS(ts, j[0]) >= 0 && compareTS(ts, j[1]) <= 0 {
				return true
			}
		}
		return false
	}
	var runs [][]slack.Message
	var cur []slack.Message
	flush := func() {
		for len(cur) > 0 {
			n := min(len(cur), runLimit)
			runs = append(runs, cur[:n])
			cur = cur[n:]
		}
		cur = nil
	}
	for _, m := range c.messages {
		if threadOf(m) != c.thread || !r.inWindow(m.TS) {
			continue
		}
		switch {
		case m.User == r.conn.SlackUserID, isJudged(m.TS):
			flush()
		case r.eligible(m):
			cur = append(cur, m)
		}
	}
	flush()
	title := "Slack: DM"
	if c.kind == ai.SlackGroupDM {
		title = "Slack: グループDM"
	}
	for _, run := range runs {
		last := run[len(run)-1]
		key := sourceKey{Team: r.conn.TeamID, Kind: kindDM, Channel: c.channel.ID, Thread: c.thread, First: run[0].TS, Last: last.TS}
		found, err := r.exists(key.String())
		if err != nil {
			return err
		}
		if found {
			continue
		}
		i, j := indexTS(c.messages, run[0].TS), indexTS(c.messages, last.TS)
		u := &unit{key: key, convType: c.kind, channelName: c.channel.Name, title: title, posts: c.messages[max(0, i-ai.PostsBefore):min(len(c.messages), j+1+ai.PostsAfter)],
			targets: map[string]bool{}, last: last, thread: c.thread}
		for _, m := range run {
			u.targets[m.TS] = true
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

// judge asks the AI about a unit and creates its item, and reports whether
// it was handled. The AI settings are handled like githubsync's mentions.
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
	cur, cred, gate, err := r.reloadAI()
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
	in, err := r.slackInput(u)
	if err != nil {
		return false, err
	}
	r.detectCalls++
	res, err := r.s.AI.JudgeSlack(r.ctx, cred, cur.Model, in)
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
	case aiStaled:
		return r.postpone(u)
	}
	// A temporary failure: an old message becomes a confirmation task, a
	// recent one waits for the next synchronization.
	if tsTime(u.last.TS).Before(r.s.Now().Add(-fallbackAge)) {
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

// post converts a message for the AI.
func (r *run) post(m slack.Message, target bool) (ai.SlackPost, error) {
	name, err := r.userName(m.User)
	if err != nil {
		return ai.SlackPost{}, err
	}
	if name == "" {
		name = "unknown"
	}
	body, err := r.formatText(m.Text)
	if err != nil {
		return ai.SlackPost{}, err
	}
	return ai.SlackPost{ID: m.TS, Author: name, AuthorID: m.User, IsMe: m.User == r.conn.SlackUserID, At: tsTime(m.TS), Target: target, Body: body}, nil
}

// slackInput builds the AI input of a unit.
func (r *run) slackInput(u *unit) (ai.SlackInput, error) {
	loc, err := r.location()
	if err != nil {
		return ai.SlackInput{}, err
	}
	in := ai.SlackInput{Me: ai.SlackPerson{ID: r.conn.SlackUserID, Name: r.conn.SlackUserName}, Location: loc, Workspace: r.conn.TeamName, ConversationType: u.convType, ChannelName: u.channelName, RelatedTasks: []ai.RelatedTask{}}
	for _, m := range u.posts {
		p, err := r.post(m, u.targets[m.TS])
		if err != nil {
			return ai.SlackInput{}, err
		}
		in.Posts = append(in.Posts, p)
	}
	tasks, err := r.s.Store.RelatedTasks(r.parent, r.conn.UserID, r.relatedScope(u))
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

// relatedScope selects the related tasks of a unit's conversation: the
// mentions and direct message runs of its channel and thread, whatever their
// kind, the groups made for it, and in a channel the mention of the
// thread's first message. Outside threads, the items of a direct message
// conversation are limited to the last groupWindow.
func (r *run) relatedScope(u *unit) store.RelatedScope {
	now := r.s.Now()
	team, channel := r.conn.TeamID, u.key.Channel
	group := conversationPrefix(team, kindGroup, channel, u.thread)
	scope := store.RelatedScope{
		Source:        store.SourceSlack,
		Prefixes:      []string{conversationPrefix(team, kindMention, channel, u.thread), conversationPrefix(team, kindDM, channel, u.thread), group},
		GroupPrefixes: []string{group},
		ClosedSince:   now.Add(-groupWindow).Unix(),
		FetchLimit:    relatedFetch,
		Limit:         relatedLimit,
		SubtaskLimit:  subtaskLimit,
	}
	if u.convType == ai.SlackChannel && u.thread != noThread {
		scope.ExactKeys = []string{conversationPrefix(team, kindMention, channel, noThread) + u.thread}
	}
	if u.convType != ai.SlackChannel && u.thread == noThread {
		scope.DMSince = now.Add(-groupWindow).Unix()
	}
	return scope
}

// create creates the item of a unit unless it exists, and queues its
// detection notification when the message is recent enough. Candidates are
// tracked like tasks but not notified; event items are not tracked, since
// a reply does not add the event to the calendar.
func (r *run) create(u *unit, o outcome, res ai.MentionResult) (bool, error) {
	link, err := r.permalink(u.key.Channel, u.last)
	if err != nil {
		return false, err
	}
	now := r.s.Now().Unix()
	id, err := auth.Random()
	if err != nil {
		return false, err
	}
	place, err := r.conversationName(u)
	if err != nil {
		return false, err
	}
	refs, err := json.Marshal([]ref{{Service: store.SourceSlack, Account: r.conn.TeamName, ConversationID: u.key.Channel, ConversationName: place, MessageID: u.key.Last, URL: link}})
	if err != nil {
		return false, err
	}
	key := u.key.String()
	item := store.ActionItem{ID: id, Status: "open", Refs: string(refs), Version: 1, CreatedAt: now, UpdatedAt: now,
		Source: nullString(store.SourceSlack), SourceKey: nullString(key), Tracking: 1, DetectedAt: nullInt(now)}
	// The group parent shares the conversation's reference, with a link to
	// the conversation itself.
	groupRefs, err := json.Marshal([]ref{{Service: store.SourceSlack, Account: r.conn.TeamName, ConversationID: u.key.Channel, ConversationName: place, URL: r.conversationURL(u.key.Channel, u.thread)}})
	if err != nil {
		return false, err
	}
	switch o {
	case outcomeAction, outcomeCandidate:
		item.Content = res.Task
		if o == outcomeCandidate {
			item.Status = "candidate"
		}
		setEvent(&item, res.Event, slackURL(link), now)
	case outcomeNoAction:
		item.Content = res.Summary
		if item.Content == "" {
			item.Content = noActionNotice
		}
		item.Status, item.Tracking, item.AutoReason, item.AutoRef, item.AutoAt = "done", 0, nullString(ReasonNoAction), slackURL(link), nullInt(now)
	default:
		body, err := r.formatText(u.last.Text)
		if err != nil {
			return false, err
		}
		item.Content = "Slack を確認: " + excerpt(body, excerptLimit)
	}
	item.Content = truncate(item.Content, contentLimit)
	notify := !r.firstImport && tsSeconds(u.last.TS) >= r.to-int64(notifyAge/time.Second)
	err = r.s.Store.SlackSync(r.parent, r.conn.UserID, r.conn.Generation, func(tx *store.SlackTx) error {
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
		if e != nil || !created || item.Status != "open" || !notify || tx.Conn.ImmediateNotify != 1 {
			return e
		}
		return tx.Enqueue(detectedNotification(u.title, item, now))
	})
	return err == nil, err
}

// groupParent returns the parent for a new open task that the AI judged to
// belong to the same matter as a related task, and whether a group parent
// was made for it. A related group takes the task while it is open, or
// while it was completed by its subtasks within groupWindow. A related plain
// task still open, without a parent and without subtasks, is put under a new
// group parent named after the topic, and the new task joins it. Otherwise
// the task stays on its own. Nothing is written before the checks pass, and
// any error after the group parent was made rolls back the transaction.
func (r *run) groupParent(tx *store.SlackTx, u *unit, res ai.MentionResult, itemID, refs string, now int64) (sql.NullString, bool, error) {
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
		Source: nullString(store.SourceSlack), SourceKey: nullString(conversationPrefix(r.conn.TeamID, kindGroup, u.key.Channel, u.thread) + itemID), DetectedAt: nullInt(now), Kind: store.KindTask}
	created, e := tx.CreateGroup(group)
	if e != nil {
		return sql.NullString{}, false, e
	}
	if !created {
		return sql.NullString{}, false, errors.New("slacksync: group parent key already exists")
	}
	if e = tx.SetParent(t.ID, group.ID, now); e != nil {
		return sql.NullString{}, false, e
	}
	return nullString(group.ID), true, nil
}

// conversationURL links to a conversation: the thread's first message, or
// the channel outside threads. It is empty when the workspace URL is not a
// Slack URL.
func (r *run) conversationURL(channel, thread string) string {
	base := strings.TrimSuffix(r.conn.TeamURL, "/") + "/archives/" + channel
	if thread != noThread {
		base += "/p" + strings.ReplaceAll(thread, ".", "")
	}
	return slackURL(base).String
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

// conversationName names the conversation of a unit for display: #channel,
// DM（name） with the other member's display name, or グループDM. It is empty
// when Slack did not give a channel name.
func (r *run) conversationName(u *unit) (string, error) {
	switch u.convType {
	case ai.SlackDM:
		name, err := r.userName(u.last.User)
		if err != nil {
			return "", err
		}
		if name == "" || name == u.last.User {
			return "DM", nil
		}
		return truncate("DM（"+name+"）", conversationNameLimit), nil
	case ai.SlackGroupDM:
		return "グループDM", nil
	}
	if name := cleanText(u.channelName); name != "" {
		return truncate("#"+name, conversationNameLimit), nil
	}
	return "", nil
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
