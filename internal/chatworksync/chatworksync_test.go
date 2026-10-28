package chatworksync

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/i2bskn/astovia/internal/ai"
	"github.com/i2bskn/astovia/internal/chatwork"
	"github.com/i2bskn/astovia/internal/secretbox"
	"github.com/i2bskn/astovia/internal/store"
	"github.com/i2bskn/astovia/internal/testutil"
)

const (
	me    = "101"
	bob   = "102"
	carol = "103"
	cwTok = "chatwork-test-token-0001"
	aiKey = "sk-ant-test-key-0123456789"
)

// cwMsg is a message of the fake Chatwork.
type cwMsg struct {
	ID, Account, Body string
	Send              int64
}

type cwRoom struct {
	ID, Name, Type string
	LastUpdate     int64
}

// fakeChatwork is an in-memory Chatwork API. statuses forces an HTTP
// status for a request kind ("rooms", "my/tasks", "messages" or
// "messages:{room}"); status 1 answers with a body over the size limit.
// hook runs before a request of a kind is answered.
type fakeChatwork struct {
	mu       sync.Mutex
	rooms    []*cwRoom
	messages map[string][]cwMsg
	tasks    []map[string]any
	names    map[string]string
	statuses map[string]int
	reset    string
	pageSize int
	nextID   int
	hooks    map[string]func()
	requests []string
}

func newFakeChatwork() *fakeChatwork {
	return &fakeChatwork{messages: map[string][]cwMsg{}, names: map[string]string{me: "alice", bob: "bob", carol: "carol"}, statuses: map[string]int{}, pageSize: 100, nextID: 1000000, hooks: map[string]func(){}}
}

func (f *fakeChatwork) set(fn func(f *fakeChatwork)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

// count returns the number of requests of a kind ("" for all; "messages"
// for every room).
func (f *fakeChatwork) count(kind string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.requests {
		if kind == "" || r == kind || (kind == "messages" && strings.HasPrefix(r, "messages:")) {
			n++
		}
	}
	return n
}

func (f *fakeChatwork) reset0() { f.set(func(f *fakeChatwork) { f.requests = nil }) }

func (f *fakeChatwork) room(id, name, typ string) {
	f.set(func(f *fakeChatwork) { f.rooms = append(f.rooms, &cwRoom{ID: id, Name: name, Type: typ}) })
}

// say adds a message to a room at send, updating the room, and returns its
// ID.
func (f *fakeChatwork) say(room, account, body string, send int64) string {
	var id string
	f.set(func(f *fakeChatwork) {
		f.nextID++
		id = strconv.Itoa(f.nextID)
		f.messages[room] = append(f.messages[room], cwMsg{ID: id, Account: account, Body: body, Send: send})
		for _, r := range f.rooms {
			if r.ID == room && send > r.LastUpdate {
				r.LastUpdate = send
			}
		}
	})
	return id
}

func (f *fakeChatwork) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	path := strings.TrimPrefix(r.URL.Path, "/")
	kind := path
	if strings.HasPrefix(path, "rooms/") && strings.HasSuffix(path, "/messages") {
		kind = "messages:" + strings.TrimSuffix(strings.TrimPrefix(path, "rooms/"), "/messages")
	}
	f.requests = append(f.requests, kind)
	hook := f.hooks[kind]
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("x-chatworktoken") != cwTok {
		w.WriteHeader(401)
		return
	}
	code, ok := f.statuses[kind]
	if !ok && strings.HasPrefix(kind, "messages:") {
		code, ok = f.statuses["messages"]
	}
	if ok {
		if code == 1 {
			_, _ = w.Write([]byte(`[{"message_id":"1","body":"` + strings.Repeat("a", 8<<20) + `"}]`))
			return
		}
		if code == 429 && f.reset != "" {
			w.Header().Set("x-ratelimit-reset", f.reset)
		}
		w.WriteHeader(code)
		return
	}
	write := func(v []map[string]any) {
		if len(v) == 0 {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	switch {
	case kind == "rooms":
		var out []map[string]any
		for _, room := range f.rooms {
			out = append(out, map[string]any{"room_id": json.Number(room.ID), "name": room.Name, "type": room.Type, "last_update_time": room.LastUpdate, "unread_num": 0})
		}
		write(out)
	case strings.HasPrefix(kind, "messages:"):
		if r.URL.Query().Get("force") != "1" {
			w.WriteHeader(400)
			return
		}
		list := append([]cwMsg{}, f.messages[strings.TrimPrefix(kind, "messages:")]...)
		sort.Slice(list, func(i, j int) bool { return chatwork.CompareIDs(list[i].ID, list[j].ID) < 0 })
		if len(list) > f.pageSize {
			list = list[len(list)-f.pageSize:]
		}
		var out []map[string]any
		for _, m := range list {
			out = append(out, map[string]any{"message_id": m.ID, "account": map[string]any{"account_id": json.Number(m.Account), "name": f.names[m.Account]}, "body": m.Body, "send_time": m.Send, "update_time": 0})
		}
		write(out)
	case kind == "my/tasks":
		if r.URL.Query().Get("status") != "open" {
			w.WriteHeader(400)
			return
		}
		write(f.tasks)
	default:
		w.WriteHeader(404)
	}
}

// task is a task of GET /my/tasks.
func task(id, room, message, body string, limit int64, limitType string) map[string]any {
	return map[string]any{"task_id": json.Number(id), "room": map[string]any{"room_id": json.Number(room), "name": "Room " + room}, "assigned_by_account": map[string]any{"account_id": json.Number(bob), "name": "bob"},
		"message_id": message, "body": body, "limit_time": limit, "status": "open", "limit_type": limitType}
}

// fakeAI is an Anthropic Messages API answering with replies in order (500
// when they run out).
type fakeAI struct {
	mu      sync.Mutex
	replies []func(w http.ResponseWriter)
	bodies  []string
}

func (f *fakeAI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.bodies = append(f.bodies, string(body))
	n := len(f.bodies)
	var reply func(http.ResponseWriter)
	if n <= len(f.replies) {
		reply = f.replies[n-1]
	}
	f.mu.Unlock()
	if reply == nil {
		reply = aiError(500)
	}
	reply(w)
}

func (f *fakeAI) set(replies ...func(http.ResponseWriter)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replies, f.bodies = replies, nil
}

func (f *fakeAI) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.bodies)
}

// input returns the JSON document of the n-th request's user message, with
// the system prompt as "_system".
func (f *fakeAI) input(t *testing.T, n int) map[string]any {
	t.Helper()
	f.mu.Lock()
	raw := f.bodies[n-1]
	f.mu.Unlock()
	var body struct {
		System []struct {
			Text string `json:"text"`
		} `json:"system"`
		Messages []struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatal(err)
	}
	_, doc, _ := strings.Cut(body.Messages[0].Content[0].Text, "\n")
	var out map[string]any
	if err := json.Unmarshal([]byte(doc), &out); err != nil {
		t.Fatal(err)
	}
	out["_system"] = body.System[0].Text
	return out
}

func aiText(v map[string]any) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		out, _ := json.Marshal(v)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "msg", "type": "message", "role": "assistant", "model": "claude-haiku-5-5",
			"content": []map[string]any{{"type": "text", "text": string(out)}}, "stop_reason": "end_turn", "usage": map[string]int{"input_tokens": 1, "output_tokens": 1}})
	}
}

func aiAction(task string) func(http.ResponseWriter) {
	return aiJudged(ai.DecisionTask, task, "s", "", "", "")
}

func aiCandidate(task string) func(http.ResponseWriter) {
	return aiJudged(ai.DecisionCandidate, task, "s", "", "", "")
}

func aiNoAction(summary string) func(http.ResponseWriter) {
	return aiJudged(ai.DecisionNone, "", summary, "", "", "")
}

func aiJudged(decision, task, summary, eventTitle, eventStart, eventEnd string) func(http.ResponseWriter) {
	return aiText(map[string]any{"decision": decision, "task": task, "summary": summary, "event_title": eventTitle, "event_start": eventStart, "event_end": eventEnd, "topic": "", "same_as": ""})
}

func aiGrouped(task, topic, sameAs string) func(http.ResponseWriter) {
	return aiText(map[string]any{"decision": ai.DecisionTask, "task": task, "summary": "s", "event_title": "", "event_start": "", "event_end": "", "topic": topic, "same_as": sameAs})
}

func aiResolved(v bool) func(http.ResponseWriter) {
	return aiText(map[string]any{"resolved": v, "reason": "secret reason"})
}

func aiError(code int) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"error","message":"forced"}}`))
	}
}

type syncTest struct {
	t      *testing.T
	s      *store.Store
	cw     *fakeChatwork
	ai     *fakeAI
	syncer *Syncer
	now    time.Time
}

func drivers(t *testing.T, fn func(t *testing.T, driver string)) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) { fn(t, driver) })
	}
}

// newSyncTest connects alice (account 101) with the AI configured and runs
// the first synchronization on an empty account, so that the window has
// started; then it moves 10 minutes on.
func newSyncTest(t *testing.T, driver string) *syncTest {
	t.Helper()
	st := newBareSyncTest(t, driver)
	st.saveAI()
	st.sync()
	st.cw.reset0()
	st.advance(10 * time.Minute)
	return st
}

// newBareSyncTest connects alice without AI settings and without syncing.
func newBareSyncTest(t *testing.T, driver string) *syncTest {
	t.Helper()
	ctx := context.Background()
	s, _ := testutil.Database(t, driver)
	st := &syncTest{t: t, s: s, now: time.Date(2026, 10, 24, 1, 0, 0, 0, time.UTC)}
	u := store.User{ID: "alice-id", Email: "alice@example.com", PasswordHash: "x", Role: store.RoleOwner}
	if err := s.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	if err := s.Login(ctx, u, store.Session{ID: "sid", UserID: u.ID, ExpiresAt: st.now.Unix() + 86400*30, CreatedAt: st.now.Unix()}, store.Refresh{Hash: "r", SessionID: "sid", CreatedAt: st.now.Unix()}); err != nil {
		t.Fatal(err)
	}
	st.cw = newFakeChatwork()
	srv := httptest.NewServer(st.cw)
	t.Cleanup(srv.Close)
	client := chatwork.New(func() time.Time { return st.now })
	client.BaseURL = srv.URL
	st.ai = &fakeAI{}
	aiSrv := httptest.NewServer(st.ai)
	t.Cleanup(aiSrv.Close)
	aiClient := ai.New()
	aiClient.BaseURL = aiSrv.URL
	st.syncer = &Syncer{Store: s, Chatwork: client, AI: aiClient, Box: secretbox.New([]byte(strings.Repeat("s", 32))), Now: func() time.Time { return st.now }}
	st.connect(me, store.ChatworkSettings{})
	return st
}

func (st *syncTest) connect(account string, settings store.ChatworkSettings) store.ChatworkConnection {
	st.t.Helper()
	sealed, err := st.syncer.Box.Seal(TokenPurpose("alice-id"), cwTok)
	if err != nil {
		st.t.Fatal(err)
	}
	c, err := st.s.SaveChatworkConnection(context.Background(), store.ChatworkConnection{UserID: "alice-id", TokenSealed: sealed, TokenLast4: "0001", AccountID: account, AccountName: "alice", OrganizationName: "Acme"}, settings, "sid", st.now.Unix())
	if err != nil {
		st.t.Fatal(err)
	}
	return c
}

func (st *syncTest) settings(v store.ChatworkSettings) {
	st.t.Helper()
	if _, err := st.s.UpdateChatworkSettings(context.Background(), "alice-id", v, "sid", st.now.Unix()); err != nil {
		st.t.Fatal(err)
	}
}

func (st *syncTest) saveAI() store.AISettings {
	st.t.Helper()
	ctx := context.Background()
	var gen int64
	if cur, err := st.s.AISettings(ctx, "alice-id"); err == nil {
		gen = cur.Generation
	}
	sealed, err := st.syncer.Box.Seal(ai.KeyPurpose("alice-id"), aiKey)
	if err != nil {
		st.t.Fatal(err)
	}
	a, err := st.s.SaveAISettings(ctx, store.AISettings{UserID: "alice-id", Provider: ai.ProviderAnthropic, Model: "claude-haiku-5-5", APIKeySealed: sealed, KeyLast4: "6789"}, gen, true, "sid", st.now.Unix())
	if err != nil {
		st.t.Fatal(err)
	}
	return a
}

func (st *syncTest) conn() store.ChatworkConnection {
	st.t.Helper()
	c, err := st.s.ChatworkConnection(context.Background(), "alice-id")
	if err != nil {
		st.t.Fatal(err)
	}
	return c
}

func (st *syncTest) sync() store.ChatworkConnection {
	st.t.Helper()
	st.syncer.Sync(context.Background(), st.conn())
	return st.conn()
}

func (st *syncTest) advance(d time.Duration) { st.now = st.now.Add(d) }

// say posts a message d before now.
func (st *syncTest) say(room, account, body string, d time.Duration) string {
	return st.cw.say(room, account, body, st.now.Add(-d).Unix())
}

func (st *syncTest) items() []store.ActionItem {
	st.t.Helper()
	items, err := st.s.ListActionItems(context.Background(), "alice-id", "", nil, 300)
	if err != nil {
		st.t.Fatal(err)
	}
	return items
}

func (st *syncTest) item(key string) (store.ActionItem, bool) {
	st.t.Helper()
	for _, i := range st.items() {
		if i.SourceKey.String == key {
			return i, true
		}
	}
	return store.ActionItem{}, false
}

func (st *syncTest) mustItem(key string) store.ActionItem {
	st.t.Helper()
	i, ok := st.item(key)
	if !ok {
		keys := []string{}
		for _, i := range st.items() {
			keys = append(keys, i.SourceKey.String)
		}
		st.t.Fatalf("no item %s; have %v", key, keys)
	}
	return i
}

func (st *syncTest) pending() []store.Notification {
	st.t.Helper()
	list, err := st.s.DueNotifications(context.Background(), st.now.Unix()+86400*365, 100)
	if err != nil {
		st.t.Fatal(err)
	}
	return list
}

// create stores an item for alice's connection.
func (st *syncTest) create(item store.ActionItem) {
	st.t.Helper()
	c := st.conn()
	if err := st.s.ChatworkSync(context.Background(), "alice-id", c.Generation, func(tx *store.ChatworkTx) error {
		_, e := tx.CreateItem(item)
		return e
	}); err != nil {
		st.t.Fatal(err)
	}
}

func mentionKey(room, message string) string { return "chatwork:" + me + ":m:" + room + ":" + message }

func dmKey(room, first, last string) string {
	return "chatwork:" + me + ":d:" + room + ":" + first + ":" + last
}

func taskKey(id string) string { return "chatwork:" + me + ":t:" + id }

func refsOf(t *testing.T, item store.ActionItem) []map[string]string {
	t.Helper()
	var refs []map[string]string
	if err := json.Unmarshal([]byte(item.Refs), &refs); err != nil {
		t.Fatal(err)
	}
	return refs
}

func TestSourceKeys(t *testing.T) {
	for _, k := range []sourceKey{
		{Account: me, Kind: kindMention, Room: "2", First: "1000001", Last: "1000001"},
		{Account: me, Kind: kindDM, Room: "3", First: "999", Last: "1000"},
		{Account: me, Kind: kindDM, Room: "3", First: "5", Last: "5"},
	} {
		got, ok := parseSourceKey(k.String())
		if !ok || got != k {
			t.Fatalf("%s: %+v %v", k, got, ok)
		}
	}
	for _, v := range []string{"", "slack:T1:m:C1:-:1.000001", "chatwork:0101:m:2:1", "chatwork:101:x:2:1", "chatwork:101:m:2:1:2", "chatwork:101:d:2:1000:999",
		"chatwork:101:m:02:1", "chatwork:101:m:2:01", "chatwork:101:m:2:", "chatwork:abc:m:2:1", "chatwork:101:d:2:1",
		// Group parents and imported tasks are not messages.
		"chatwork:101:g:2:item-1", "chatwork:101:t:11", "chatwork:101:t:2:11"} {
		if _, ok := parseSourceKey(v); ok {
			t.Fatalf("%q parsed", v)
		}
	}
}

func TestHelpers(t *testing.T) {
	for v, want := range map[string]bool{"https://www.chatwork.com/#!rid2-1000": true, "https://www.chatwork.com/#!rid2": true, "https://chatwork.com/#!rid2": false, "http://www.chatwork.com/": false,
		"https://www.chatwork.com.evil.example/": false, "https://www.chatwork.com:8443/": false, "https://user@www.chatwork.com/": false, "javascript:alert(1)": false, "https://WWW.CHATWORK.COM/": false} {
		if chatworkURL(v).Valid != want {
			t.Fatalf("chatworkURL(%q)", v)
		}
	}
	for v, want := range map[string]string{"a[qt]b[/qt]c[qt]d[/qt]e": "ace", "a[qt]b": "a", "[qt][qt]x[/qt]y[/qt]z": "y[/qt]z", "plain": "plain"} {
		if got := stripQuotes(v); got != want {
			t.Fatalf("stripQuotes(%q) = %q", v, got)
		}
	}
	for v, want := range map[string]bool{"[dtext:chatroom_member_is]": true, " [info][title][dtext:task_added][/title]x[/info]": true, "[info][title]お知らせ[/title]x[/info]": false, "hi [dtext:x]": false} {
		if system(v) != want {
			t.Fatalf("system(%q)", v)
		}
	}
	for v, want := range map[string]bool{"[To:101] hi": true, "[rp aid=101 to=2-1000] ok": true, "[rp aid=101] ok": true, "[toall] 全員へ": true, "[To:1010] hi": false, "[rp aid=1011 to=2-1]": false,
		"[qt][qtmeta aid=102][To:101] old[/qt] see above": false, "[To:102] hi": false} {
		if addressed(v, me) != want {
			t.Fatalf("addressed(%q)", v)
		}
	}
	if got := excerpt(" a\nb\r\n\tc ", 80); got != "a b c" {
		t.Fatalf("excerpt: %q", got)
	}
}

func TestFormatText(t *testing.T) {
	names := map[string]string{me: "alice", bob: "bob"}
	for in, want := range map[string]string{
		"[To:101]確認お願いします":                               "@alice(101) 確認お願いします",
		"[To:999] x":                                     "@999(999)  x",
		"[rp aid=102 to=2-1000]了解":                       "返信 @bob(102) 了解",
		"[toall]周知です":                                    "@all 周知です",
		"[pname:102]さん [piconname:101] [picon:102]":      "bobさん alice ",
		"[qt][qtmeta aid=102 time=1700000000]前の話[/qt]返事": "引用（bob）: 前の話返事",
		"[qt][qtmeta aid=999]x[/qt]":                     "引用: x",
		"[qt]原文[/qt]":                                    "引用: 原文",
		"[info][title]件名[/title]本文[/info]":               "\n件名\n本文\n",
		"a[hr]b":                         "a----b",
		"[code]x := 1[/code]":            "x := 1\n",
		"[url]https://example.com[/url]": "https://example.com",
		"[task aid=101 st=open lt=1700000000]作業[/task]": "タスク: 作業",
		"[dtext:file_uploaded][preview id=1 ht=10]":     "",
		"[download:55]資料.pdf[/download]":                "ファイル: 資料.pdf",
		"[unknown] [To:abc]":                            "[unknown] [To:abc]",
		"tab\there\r\nnext\u200b\x00":                   "tab\there\nnext",
	} {
		if got := formatText(in, names); got != want {
			t.Fatalf("formatText(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSyncErrors(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		st := newSyncTest(t, driver)
		st.cw.room("2", "Team", "group")
		st.say("2", bob, "[To:101] hi", time.Minute)

		// 401 stops the connection.
		st.cw.set(func(f *fakeChatwork) { f.statuses["rooms"] = 401 })
		c := st.sync()
		if c.Status != store.ChatworkError || c.LastError.String != ErrorTokenInvalid {
			t.Fatalf("401: %+v", c)
		}
		if due, err := st.s.DueChatworkConnections(context.Background(), st.now.Unix()+86400, 10); err != nil || len(due) != 0 {
			t.Fatalf("due after 401: %+v %v", due, err)
		}

		// 429 waits until the reset time, or 15 minutes.
		st.connect(me, store.ChatworkSettings{})
		reset := st.now.Add(20 * time.Minute).Unix()
		st.cw.set(func(f *fakeChatwork) {
			f.statuses = map[string]int{"my/tasks": 429}
			f.reset = strconv.FormatInt(reset, 10)
		})
		if c = st.sync(); c.Status != store.ChatworkActive || c.LastError.String != ErrorRateLimited || c.NextPollAt != reset || c.FailureCount != 0 {
			t.Fatalf("429: %+v", c)
		}
		st.cw.set(func(f *fakeChatwork) { f.reset = "" })
		if c = st.sync(); c.LastError.String != ErrorRateLimited || c.NextPollAt != st.now.Unix()+900 {
			t.Fatalf("429 without reset: %+v", c)
		}

		// 5xx and other errors of the list calls back off.
		st.cw.set(func(f *fakeChatwork) { f.statuses = map[string]int{"rooms": 503} })
		if c = st.sync(); c.Status != store.ChatworkActive || c.LastError.String != ErrorUnavailable || c.FailureCount != 1 || c.NextPollAt != st.now.Unix()+60 {
			t.Fatalf("503: %+v", c)
		}
		st.cw.set(func(f *fakeChatwork) { f.statuses = map[string]int{"rooms": 403} })
		if c = st.sync(); c.LastError.String != ErrorUnavailable || c.FailureCount != 2 || c.NextPollAt != st.now.Unix()+120 {
			t.Fatalf("403 rooms: %+v", c)
		}
		// The tasks are read every 5 minutes; a failure keeps the last read.
		tasksAt := c.LastTasksAt
		st.advance(5 * time.Minute)
		st.cw.set(func(f *fakeChatwork) { f.statuses = map[string]int{"my/tasks": 404} })
		if c = st.sync(); c.LastError.String != ErrorUnavailable || c.FailureCount != 3 || c.NextPollAt != st.now.Unix()+240 || c.LastTasksAt != tasksAt || !tasksAt.Valid {
			t.Fatalf("404 tasks: %+v", c)
		}

		// A success clears the error.
		st.cw.set(func(f *fakeChatwork) { f.statuses = map[string]int{} })
		st.ai.set(aiAction("返事する"))
		if c = st.sync(); c.LastError.Valid || c.FailureCount != 0 || c.NextPollAt != st.now.Unix()+60 || !c.LastSyncedAt.Valid {
			t.Fatalf("success: %+v", c)
		}

		// A token that cannot be opened stops the connection.
		bad := st.conn()
		bad.TokenSealed = "broken"
		st.syncer.Sync(context.Background(), bad)
		if c = st.conn(); c.Status != store.ChatworkError || c.LastError.String != ErrorTokenUndecryptable {
			t.Fatalf("undecryptable: %+v", c)
		}
	})
}

// A connection replaced during the synchronization keeps its new state.
func TestStaleConnection(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		st := newSyncTest(t, driver)
		st.cw.room("2", "Team", "group")
		st.say("2", bob, "[To:101] hi", time.Minute)
		st.cw.set(func(f *fakeChatwork) {
			f.hooks["rooms"] = func() { st.connect(me, store.ChatworkSettings{}) }
		})
		old := st.conn()
		st.ai.set(aiAction("返事する"))
		st.syncer.Sync(context.Background(), old)
		c := st.conn()
		if c.Generation != old.Generation+1 || c.LastSyncedAt.Valid || c.LastTasksAt.Valid || c.NextPollAt != st.now.Unix() {
			t.Fatalf("replaced connection: %+v", c)
		}
		if len(st.items()) != 0 {
			t.Fatalf("items of a stale synchronization: %d", len(st.items()))
		}
	})
}

func TestImportTasks(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		// Without the AI, tasks are imported and nothing else is read.
		st := newBareSyncTest(t, driver)
		if err := st.s.SaveNotificationSettings(context.Background(), store.NotificationSettings{UserID: "alice-id", Timezone: "Asia/Tokyo", DigestHour: store.DefaultDigestHour, StaleDays: store.DefaultStaleDays, UpdatedAt: st.now.Unix()}); err != nil {
			t.Fatal(err)
		}
		at := time.Date(2026, 10, 30, 3, 0, 0, 0, time.UTC).Unix()
		st.cw.set(func(f *fakeChatwork) {
			f.tasks = []map[string]any{
				task("11", "2", "1001", "[To:101]資料を\n送ってください", at, "time"),
				task("12", "2", "1002", "[pname:102]さんの件", at, "date"),
				task("13", "3", "1003", "[picon:102]", 0, "none"),
			}
		})
		c := st.sync()
		if !c.LastTasksAt.Valid || c.LastTasksAt.Int64 != st.now.Unix() || c.LastSyncedAt.Valid || st.cw.count("rooms") != 0 || st.ai.count() != 0 {
			t.Fatalf("first import: %+v rooms=%d", c, st.cw.count("rooms"))
		}
		t11 := st.mustItem(taskKey("11"))
		if t11.Status != "open" || t11.Content != "@alice(101) 資料を 送ってください" || t11.Tracking != 0 || t11.Kind != store.KindTask || t11.DueAt.Int64 != at || t11.Source.String != "chatwork" || !t11.DetectedAt.Valid {
			t.Fatalf("time task: %+v", t11)
		}
		refs := refsOf(t, t11)
		if len(refs) != 1 || refs[0]["service"] != "chatwork" || refs[0]["account"] != "Acme" || refs[0]["conversation_id"] != "2" || refs[0]["conversation_name"] != "Room 2" || refs[0]["message_id"] != "1001" || refs[0]["url"] != "https://www.chatwork.com/#!rid2-1001" {
			t.Fatalf("refs: %s", t11.Refs)
		}
		// A date limit is due at 23:59 of its day in the user's time zone
		// (2026-10-30 12:00 in Tokyo).
		tokyo, _ := time.LoadLocation("Asia/Tokyo")
		if t12 := st.mustItem(taskKey("12")); t12.Content != "bobさんの件" || t12.DueAt.Int64 != time.Date(2026, 10, 30, 23, 59, 0, 0, tokyo).Unix() {
			t.Fatalf("date task: %+v", t12)
		}
		if t13 := st.mustItem(taskKey("13")); t13.Content != "Chatwork のタスク" || t13.DueAt.Valid {
			t.Fatalf("empty task: %+v", t13)
		}
		// The first import is not notified.
		if p := st.pending(); len(p) != 0 {
			t.Fatalf("first import notified: %+v", p)
		}

		// Within taskInterval nothing is requested.
		st.cw.reset0()
		st.advance(time.Minute)
		st.sync()
		if st.cw.count("") != 0 {
			t.Fatalf("requests within the interval: %d", st.cw.count(""))
		}

		// A new task is notified; a task that left the list is completed.
		st.advance(5 * time.Minute)
		st.cw.set(func(f *fakeChatwork) {
			f.tasks = append(f.tasks[1:], task("14", "3", "1004", "レビュー", 0, "none"))
		})
		st.sync()
		t14 := st.mustItem(taskKey("14"))
		p := st.pending()
		if len(p) != 1 || p[0].Title != "Chatwork: タスク" || p[0].Body != "レビュー" || p[0].ActionItemID.String != t14.ID || p[0].URL != "/tasks/"+t14.ID {
			t.Fatalf("notifications: %+v", p)
		}
		if t11 = st.mustItem(taskKey("11")); t11.Status != "done" || t11.AutoReason.String != ReasonTaskClosed || t11.AutoRef.String != "https://www.chatwork.com/#!rid2-1001" || t11.AutoAt.Int64 != st.now.Unix() {
			t.Fatalf("closed task: %+v", t11)
		}
		if n := len(st.items()); n != 4 {
			t.Fatalf("items: %d", n)
		}

		// A list with an invalid task or 100 tasks may be incomplete:
		// nothing is completed.
		st.advance(5 * time.Minute)
		st.cw.set(func(f *fakeChatwork) {
			f.tasks = []map[string]any{task("12", "2", "1002", "x", 0, "none"), {"task_id": "bad", "room": map[string]any{"room_id": 2}, "message_id": "1"}}
		})
		st.sync()
		if t14 = st.mustItem(taskKey("14")); t14.Status != "open" {
			t.Fatalf("completed with an invalid task: %+v", t14)
		}
		st.advance(5 * time.Minute)
		st.cw.set(func(f *fakeChatwork) {
			f.tasks = nil
			for i := 0; i < 100; i++ {
				f.tasks = append(f.tasks, task(strconv.Itoa(100+i), "2", strconv.Itoa(5000+i), "t", 0, "none"))
			}
		})
		st.sync()
		if t14 = st.mustItem(taskKey("14")); t14.Status != "open" {
			t.Fatalf("completed with 100 tasks: %+v", t14)
		}

		// The task leaves the list: completed, and its pending notification
		// dropped.
		st.advance(5 * time.Minute)
		st.cw.set(func(f *fakeChatwork) { f.tasks = nil })
		st.sync()
		if t14 = st.mustItem(taskKey("14")); t14.Status != "done" || t14.AutoReason.String != ReasonTaskClosed {
			t.Fatalf("closed new task: %+v", t14)
		}
		for _, n := range st.pending() {
			if n.ActionItemID.String == t14.ID {
				t.Fatalf("notification of a closed task kept: %+v", n)
			}
		}

		// import_tasks=0 does not request the tasks.
		st.cw.reset0()
		st.settings(store.ChatworkSettings{ImportTasks: boolPtr(false)})
		st.advance(5 * time.Minute)
		st.sync()
		if st.cw.count("my/tasks") != 0 {
			t.Fatalf("tasks requested while disabled")
		}
	})
}

// closeTask writes nothing when the item changed since it was read.
func TestCloseTaskChecks(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		st := newBareSyncTest(t, driver)
		now := st.now.Unix()
		for _, id := range []string{"a", "b"} {
			st.create(store.ActionItem{ID: id, Content: id, Status: "open", Refs: "[]", Version: 1, CreatedAt: now, UpdatedAt: now,
				Source: nullString(store.SourceChatwork), SourceKey: nullString(taskKey(id + "1")), DetectedAt: nullInt(now)})
		}
		r := &run{s: st.syncer, ctx: context.Background(), parent: context.Background(), conn: st.conn(), state: st.conn(), start: st.now}
		stale := st.mustItem(taskKey("a1"))
		if _, err := st.s.UpdateActionItem(context.Background(), "alice-id", "a", func(i *store.ActionItem) error { i.Note = nullString("n"); return nil }); err != nil {
			t.Fatal(err)
		}
		dismissed := st.mustItem(taskKey("b1"))
		if _, err := st.s.UpdateActionItem(context.Background(), "alice-id", "b", func(i *store.ActionItem) error { i.Status = "dismissed"; return nil }); err != nil {
			t.Fatal(err)
		}
		dismissed.Version++
		for _, item := range []store.ActionItem{stale, dismissed} {
			if err := r.closeTask(item); err != nil {
				t.Fatal(err)
			}
		}
		if a := st.mustItem(taskKey("a1")); a.Status != "open" || a.AutoReason.Valid {
			t.Fatalf("changed item completed: %+v", a)
		}
		if b := st.mustItem(taskKey("b1")); b.Status != "dismissed" || b.AutoReason.Valid {
			t.Fatalf("dismissed item completed: %+v", b)
		}
	})
}

func boolPtr(v bool) *bool { return &v }
