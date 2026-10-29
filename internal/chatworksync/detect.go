package chatworksync

import (
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"time"

	"github.com/i2bskn/astovia/internal/ai"
	"github.com/i2bskn/astovia/internal/auth"
	"github.com/i2bskn/astovia/internal/chatwork"
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
	// 72 hours, and at most 24 hours before the Chatwork connection or the
	// API key was registered.
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

	// roomLimit is the number of rooms read by one synchronization's
	// detection (the detection's budget of message reads).
	roomLimit = 10
	// messagePage is the most messages GET /rooms/{id}/messages returns.
	messagePage = 100
	judgedLimit = 500
	// relatedFetch is the number of items of a room read to make at most
	// relatedLimit related tasks for the AI, each group with at most
	// subtaskLimit subtasks. groupWindow is how long a group whose
	// subtasks were all done still takes new subtasks, and how old the
	// items of a room may be.
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
	noActionNotice        = "Chatwork の発言（対応不要）"
)

// Chatwork room types.
const (
	roomGroup  = "group"
	roomDirect = "direct"
)

// outcome is the item to create for a judgment.
type outcome int

const (
	outcomeAction    outcome = iota // the AI found something to do
	outcomeCandidate                // the AI is not sure the user has something to do
	outcomeNoAction                 // the AI found nothing to do
	outcomeFallback                 // no judgment: ask the user to check
)

// unit is one judgment: a message addressed to the user, or a run of
// messages from others in a direct chat. posts is the context sent to the
// AI in message order, targets the IDs judged and last the newest of them.
// names are the names known in the room. related maps the symbols given to
// the AI to the related tasks.
type unit struct {
	key      sourceKey
	convType string
	room     chatwork.Room
	title    string
	posts    []chatwork.Message
	targets  map[string]bool
	last     chatwork.Message
	names    map[string]string
	related  map[string]store.RelatedTask
}

type ref struct {
	Service          string `json:"service"`
	Account          string `json:"account,omitempty"`
	ConversationID   string `json:"conversation_id,omitempty"`
	ConversationName string `json:"conversation_name,omitempty"`
	MessageID        string `json:"message_id,omitempty"`
	URL              string `json:"url,omitempty"`
}

// detect reads the rooms updated in the window, judges their messages and
// moves the window to the start of the synchronization when nothing was
// left over.
func (r *run) detect() error {
	r.setWindow()
	if r.conn.DetectMentions == 1 || r.conn.DetectDMs == 1 {
		if err := r.detectRooms(); err != nil {
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
	r.overdue = base < r.to-int64(retryLimit/time.Second)
}

func (r *run) inWindow(m chatwork.Message) bool {
	return m.SendTime >= r.from && m.SendTime < r.to
}

// carry leaves something to the next synchronization.
func (r *run) carry() {
	r.pending = true
	r.carried++
	r.lost++
}

// detectRooms lists the rooms and reads those updated in the window that
// were not read to the end since, newest first, up to roomLimit; the others
// are carried over.
func (r *run) detectRooms() error {
	if !r.take(callRooms) {
		r.carry()
		return nil
	}
	rooms, err := r.s.Chatwork.Rooms(r.ctx, r.token)
	if err != nil {
		// GET /rooms has no error about one target: every error stops the
		// synchronization.
		return err
	}
	var targets []chatwork.Room
	for _, room := range rooms {
		switch {
		case room.Type == roomGroup && r.conn.DetectMentions == 1:
		case room.Type == roomDirect && (r.conn.DetectDMs == 1 || r.conn.DetectMentions == 1):
		default:
			continue
		}
		if room.LastUpdate < r.from {
			continue
		}
		if v, ok := r.s.roomRead(r.conn, room.ID, r.start); ok && room.LastUpdate <= v {
			continue
		}
		targets = append(targets, room)
	}
	sort.SliceStable(targets, func(i, j int) bool {
		if targets[i].LastUpdate != targets[j].LastUpdate {
			return targets[i].LastUpdate > targets[j].LastUpdate
		}
		return chatwork.CompareIDs(targets[i].ID, targets[j].ID) < 0
	})
	if len(targets) > roomLimit {
		for range targets[roomLimit:] {
			r.carry()
		}
		targets = targets[:roomLimit]
	}
	for _, room := range targets {
		done, err := r.detectRoom(room)
		if err != nil {
			return err
		}
		if done {
			// A message sent at or after the window end is judged by the
			// next synchronization, so such a room is read again.
			r.s.markRead(r.conn, room.ID, min(room.LastUpdate, r.to-1), r.start)
		}
	}
	return nil
}

// detectRoom reads a room and judges its messages, and reports whether the
// room was read to the end: nothing in it was postponed or carried over. A
// room that cannot be read is skipped and counts as read.
func (r *run) detectRoom(room chatwork.Room) (bool, error) {
	c, err := r.roomMessages(room.ID)
	switch {
	case errors.Is(err, errBudget):
		r.carry()
		return false, nil
	case err != nil && stops(err):
		return false, err
	case err != nil:
		r.warn("room skipped", "error", apiCode(err))
		r.lost++
		return true, nil
	}
	if c.raw >= messagePage && len(c.messages) > 0 && c.messages[0].SendTime > r.from {
		r.warn("room truncated", "messages", c.raw)
	}
	carried := r.carried
	if room.Type == roomDirect && r.conn.DetectDMs == 1 {
		err = r.directRuns(room, c)
	} else {
		err = r.mentions(room, c)
	}
	if err != nil {
		return false, err
	}
	return r.carried == carried, nil
}

// eligible reports whether a message may be detected: in the window, not by
// the user, not written by Chatwork, and not a task left to the task import.
func (r *run) eligible(m chatwork.Message) bool {
	return r.inWindow(m) && m.AccountID != r.conn.AccountID && !system(m.Body) && !(r.conn.ImportTasks == 1 && hasTask(m.Body))
}

// exists reports whether the user has an item with the key.
func (r *run) exists(key string) (bool, error) {
	exists := false
	err := r.s.Store.ChatworkSync(r.parent, r.conn.UserID, r.conn.Generation, func(tx *store.ChatworkTx) error {
		_, found, e := tx.LockItemBySourceKey(key)
		exists = found
		return e
	})
	return exists, err
}

// mentions judges the messages of a room addressed to the user, newest
// first, each with the messages around it.
func (r *run) mentions(room chatwork.Room, c *roomCache) error {
	convType := ai.SlackChannel
	if room.Type == roomDirect {
		convType = ai.SlackDM
	}
	names := r.names(c)
	for i := len(c.messages) - 1; i >= 0; i-- {
		m := c.messages[i]
		if !r.eligible(m) || !addressed(m.Body, r.conn.AccountID) {
			continue
		}
		key := sourceKey{Account: r.conn.AccountID, Kind: kindMention, Room: room.ID, First: m.ID, Last: m.ID}
		found, err := r.exists(key.String())
		if err != nil {
			return err
		}
		if found {
			continue
		}
		u := &unit{key: key, convType: convType, room: room, title: "Chatwork: TO", posts: c.messages[max(0, i-ai.PostsBefore):min(len(c.messages), i+1+ai.PostsAfter)],
			targets: map[string]bool{m.ID: true}, last: m, names: names}
		if _, err = r.judge(u); err != nil {
			return err
		}
	}
	return nil
}

// directRuns splits a direct chat into runs of messages from the other
// member (separated by the user's messages and by messages judged already,
// at most runLimit each) and judges them oldest first. A run that is
// postponed postpones the rest of the room.
func (r *run) directRuns(room chatwork.Room, c *roomCache) error {
	prefix := roomPrefix(r.conn.AccountID, kindDM, room.ID)
	keys, err := r.s.Store.ChatworkJudgedKeys(r.parent, r.conn.UserID, prefix, r.from, judgedLimit)
	if err != nil {
		return err
	}
	var judged [][2]string
	for _, k := range keys {
		if sk, ok := parseSourceKey(k); ok && sk.Kind == kindDM && sk.Account == r.conn.AccountID && sk.Room == room.ID {
			judged = append(judged, [2]string{sk.First, sk.Last})
		}
	}
	isJudged := func(id string) bool {
		for _, j := range judged {
			if chatwork.CompareIDs(id, j[0]) >= 0 && chatwork.CompareIDs(id, j[1]) <= 0 {
				return true
			}
		}
		return false
	}
	var runs [][]chatwork.Message
	var cur []chatwork.Message
	flush := func() {
		for len(cur) > 0 {
			n := min(len(cur), runLimit)
			runs = append(runs, cur[:n])
			cur = cur[n:]
		}
		cur = nil
	}
	for _, m := range c.messages {
		if !r.inWindow(m) {
			continue
		}
		switch {
		case m.AccountID == r.conn.AccountID, isJudged(m.ID):
			flush()
		case r.eligible(m):
			cur = append(cur, m)
		}
	}
	flush()
	names := r.names(c)
	for _, run := range runs {
		last := run[len(run)-1]
		key := sourceKey{Account: r.conn.AccountID, Kind: kindDM, Room: room.ID, First: run[0].ID, Last: last.ID}
		found, err := r.exists(key.String())
		if err != nil {
			return err
		}
		if found {
			continue
		}
		i, j := indexID(c.messages, run[0].ID), indexID(c.messages, last.ID)
		u := &unit{key: key, convType: ai.SlackDM, room: room, title: "Chatwork: ダイレクトチャット", posts: c.messages[max(0, i-ai.PostsBefore):min(len(c.messages), j+1+ai.PostsAfter)],
			targets: map[string]bool{}, last: last, names: names}
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
	in, err := r.input(u)
	if err != nil {
		return false, err
	}
	r.detectCalls++
	res, err := r.s.AI.JudgeChatwork(r.ctx, cred, cur.Model, in)
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
	if time.Unix(u.last.SendTime, 0).Before(r.s.Now().Add(-fallbackAge)) {
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
func (r *run) post(m chatwork.Message, target bool, names map[string]string) ai.SlackPost {
	name := cleanText(m.AccountName)
	if name == "" {
		name = "unknown"
	}
	return ai.SlackPost{ID: m.ID, Author: name, AuthorID: m.AccountID, IsMe: m.AccountID == r.conn.AccountID, At: time.Unix(m.SendTime, 0), Target: target, Body: formatText(m.Body, names)}
}

// input builds the AI input of a unit.
func (r *run) input(u *unit) (ai.SlackInput, error) {
	loc, err := r.location()
	if err != nil {
		return ai.SlackInput{}, err
	}
	in := ai.SlackInput{Me: ai.SlackPerson{ID: r.conn.AccountID, Name: r.conn.AccountName}, Location: loc, Workspace: r.conn.OrganizationName, ConversationType: u.convType,
		ChannelName: cleanText(u.room.Name), RelatedTasks: []ai.RelatedTask{}}
	for _, m := range u.posts {
		in.Posts = append(in.Posts, r.post(m, u.targets[m.ID], u.names))
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

// relatedScope selects the related tasks of a unit's room: the mentions and
// direct chat runs of the room and the groups made for it, from the last
// groupWindow (rooms have no threads to narrow them).
func (r *run) relatedScope(u *unit) store.RelatedScope {
	now := r.s.Now()
	account, room := r.conn.AccountID, u.key.Room
	group := roomPrefix(account, kindGroup, room)
	return store.RelatedScope{
		Source:        store.SourceChatwork,
		Prefixes:      []string{roomPrefix(account, kindMention, room), roomPrefix(account, kindDM, room), group},
		GroupPrefixes: []string{group},
		DMSince:       now.Add(-groupWindow).Unix(),
		ClosedSince:   now.Add(-groupWindow).Unix(),
		FetchLimit:    relatedFetch,
		Limit:         relatedLimit,
		SubtaskLimit:  subtaskLimit,
	}
}

// create creates the item of a unit unless it exists, and queues its
// detection notification when the message is recent enough and was sent
// after the connection and the AI settings were made. Candidates are
// tracked like tasks but not notified; event items are not tracked, since a
// reply does not add the event to the calendar.
func (r *run) create(u *unit, o outcome, res ai.MentionResult) (bool, error) {
	link := chatworkURL(messageURL(u.key.Room, u.last.ID))
	now := r.s.Now().Unix()
	id, err := auth.Random()
	if err != nil {
		return false, err
	}
	place := r.conversationName(u)
	refs, err := json.Marshal([]ref{{Service: store.SourceChatwork, Account: r.conn.OrganizationName, ConversationID: u.key.Room, ConversationName: place, MessageID: u.last.ID, URL: link.String}})
	if err != nil {
		return false, err
	}
	key := u.key.String()
	item := store.ActionItem{ID: id, Status: "open", Refs: string(refs), Version: 1, CreatedAt: now, UpdatedAt: now,
		Source: nullString(store.SourceChatwork), SourceKey: nullString(key), Tracking: 1, DetectedAt: nullInt(now)}
	// The group parent shares the room's reference, with a link to the room
	// itself.
	groupRefs, err := json.Marshal([]ref{{Service: store.SourceChatwork, Account: r.conn.OrganizationName, ConversationID: u.key.Room, ConversationName: place, URL: chatworkURL(roomURL(u.key.Room)).String}})
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
		item.Content = "Chatwork を確認: " + excerpt(formatText(u.last.Body, u.names), excerptLimit)
	}
	item.Content = truncate(item.Content, contentLimit)
	sent := u.last.SendTime
	notify := sent >= r.conn.ConnectedAt && sent >= r.to-int64(notifyAge/time.Second) && sent >= r.ai.ActivatedAt
	err = r.s.Store.ChatworkSync(r.parent, r.conn.UserID, r.conn.Generation, func(tx *store.ChatworkTx) error {
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
func (r *run) groupParent(tx *store.ChatworkTx, u *unit, res ai.MentionResult, itemID, refs string, now int64) (sql.NullString, bool, error) {
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
		Source: nullString(store.SourceChatwork), SourceKey: nullString(roomPrefix(r.conn.AccountID, kindGroup, u.key.Room) + itemID), DetectedAt: nullInt(now), Kind: store.KindTask}
	created, e := tx.CreateGroup(group)
	if e != nil {
		return sql.NullString{}, false, e
	}
	if !created {
		return sql.NullString{}, false, errors.New("chatworksync: group parent key already exists")
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

// conversationName names the room of a unit for display: the room name of
// a group chat, or DM（name） for a direct chat, whose room name is the other
// member's name. It is DM, or empty for a group chat, without a room name.
func (r *run) conversationName(u *unit) string {
	name := cleanText(u.room.Name)
	if u.room.Type == roomDirect {
		if name == "" {
			return "DM"
		}
		return truncate("DM（"+name+"）", conversationNameLimit)
	}
	return truncate(name, conversationNameLimit)
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
