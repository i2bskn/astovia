package httpserver

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/i2bskn/astovia/internal/auth"
	"github.com/i2bskn/astovia/internal/config"
	"github.com/i2bskn/astovia/internal/store"
	"github.com/i2bskn/astovia/internal/testutil"
)

const (
	aliceChatworkToken = "alice0123456789abcdef0123456789ab"
	bobChatworkToken   = "bob0123456789abcdef0123456789abcd"
)

type fakeChatworkRequest struct {
	path, query, token string
}

// fakeChatworkAPI answers GET /me, /rooms, /rooms/{id}/messages and
// /rooms/{id}/messages/{id}. status replaces the answer of a path; a 429
// carries reset as x-ratelimit-reset when it is set.
type fakeChatworkAPI struct {
	mu       sync.Mutex
	rooms    string
	messages map[string]string
	status   map[string]int
	reset    string
	requests []fakeChatworkRequest
}

// fakeRooms are rooms 1-12: room 1 is "my", room 2 "direct", room 3 is
// "Sales Team" and room 5 has no last update. Rooms 11 and 12 were updated
// at the same time. The order by last update is 12, 11, 10, 9, 8, 7, 6, 4,
// 3, 2, 1, 5.
func fakeRooms() string {
	var rooms []string
	for i := 1; i <= 12; i++ {
		typ, name, last, unread := "group", "Room "+strconv.Itoa(i), 1000+i, 0
		switch i {
		case 1:
			typ = "my"
		case 2:
			typ = "direct"
		case 3:
			name = "Sales Team"
		case 5:
			last = 0
		case 11, 12:
			last = 2000
		}
		if i == 12 {
			unread = 3
		}
		rooms = append(rooms, fmt.Sprintf(`{"room_id":%d,"name":%q,"type":%q,"role":"member","sticky":false,"unread_num":%d,"mention_num":%d,"mytask_num":0,"message_num":10,"file_num":0,"task_num":0,"icon_path":"https://example.com/i.png","last_update_time":%d}`, i, name, typ, unread, unread/3, last))
	}
	return "[" + strings.Join(rooms, ",") + "]"
}

// fakeMessages is the default answer for a room: Bob's greeting and Carol's
// invoice, sent at the same time (5000 + room).
func fakeMessages(room string) string {
	n, _ := strconv.Atoi(room)
	return fmt.Sprintf(`[{"message_id":"%d01","account":{"account_id":7,"name":"Bob","avatar_image_url":"x"},"body":"hello room %s","send_time":%d,"update_time":0},`+
		`{"message_id":"%d02","account":{"account_id":8,"name":"Carol","avatar_image_url":"x"},"body":"Invoice DEADLINE for %s","send_time":%d,"update_time":0}]`, n, room, 5000+n, n, room, 5000+n)
}

func (f *fakeChatworkAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, fakeChatworkRequest{path: r.URL.Path, query: r.URL.RawQuery, token: r.Header.Get("x-chatworktoken")})
	if code := f.status[r.URL.Path]; code != 0 {
		if code == 429 && f.reset != "" {
			w.Header().Set("x-ratelimit-reset", f.reset)
		}
		w.WriteHeader(code)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	var body string
	switch {
	case r.URL.Path == "/me":
		body = `{"account_id":101,"name":"me","organization_name":"Acme"}`
	case r.URL.Path == "/rooms":
		body = f.rooms
	case len(parts) == 3 && parts[0] == "rooms" && parts[2] == "messages":
		if m, ok := f.messages[parts[1]]; ok {
			body = m
		} else {
			body = fakeMessages(parts[1])
		}
	case len(parts) == 4 && parts[0] == "rooms" && parts[2] == "messages":
		body = `{"message_id":"` + parts[3] + `","account":{"account_id":7,"name":"Bob"},"body":"` + strings.Repeat("あ", 2100) + `","send_time":5001,"update_time":0}`
	default:
		w.WriteHeader(404)
		return
	}
	_, _ = w.Write([]byte(body))
}

func (f *fakeChatworkAPI) set(fn func(f *fakeChatworkAPI)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

// calls returns the requests other than GET /me, which only connecting
// sends, and forgets them.
func (f *fakeChatworkAPI) calls() []fakeChatworkRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakeChatworkRequest
	for _, r := range f.requests {
		if r.path != "/me" {
			out = append(out, r)
		}
	}
	f.requests = nil
	return out
}

type mcpChatworkAPI struct {
	t       *testing.T
	store   *store.Store
	server  *Server
	handler http.Handler
	cw      *fakeChatworkAPI
	alice   string
	bob     string
}

func newMCPChatworkAPI(t *testing.T, driver string) *mcpChatworkAPI {
	t.Helper()
	s, _ := testutil.Database(t, driver)
	hash, err := auth.Password("correct-password-123")
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range []store.User{{ID: "alice-id", Email: "alice@example.com", PasswordHash: hash, Role: store.RoleOwner}, {ID: "bob-id", Email: "bob@example.com", PasswordHash: hash, Role: store.RoleOwner}} {
		if err = s.CreateUser(context.Background(), u); err != nil {
			t.Fatal(err)
		}
	}
	a, err := auth.New(s, make([]byte, 32), "https://astovia.test")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 24, 1, 2, 3, 0, time.UTC)
	a.Now = func() time.Time { return now }
	cw := &fakeChatworkAPI{rooms: fakeRooms(), messages: map[string]string{}, status: map[string]int{}}
	srv := httptest.NewServer(cw)
	t.Cleanup(srv.Close)
	server := newServer(config.Config{PublicURL: "https://astovia.test", Secure: true, Secret: make([]byte, 32)}, a, fstest.MapFS{}, nil)
	server.chatwork.BaseURL = srv.URL
	api := &mcpChatworkAPI{t: t, store: s, server: server, handler: server.handler(), cw: cw}
	tok := &tokenAPI{t: t, handler: api.handler}
	api.alice = tok.login("alice")
	api.bob = tok.login("bob")
	return api
}

// connect connects Chatwork with token through REST.
func (a *mcpChatworkAPI) connect(jwt, token string) {
	a.t.Helper()
	w := (&tokenAPI{t: a.t, handler: a.handler}).do("PUT", "/api/v1/integrations/chatwork", jwt, `{"token":"`+token+`"}`)
	if w.Code != 200 {
		a.t.Fatalf("connect: %d %s", w.Code, w.Body.String())
	}
	a.cw.calls()
}

// client connects an MCP client with a new API token of jwt's user.
func (a *mcpChatworkAPI) client(jwt string) *mcpClient {
	a.t.Helper()
	srv := httptest.NewServer(a.handler)
	a.t.Cleanup(srv.Close)
	return connectMCP(a.t, srv, (&tokenAPI{t: a.t, handler: a.handler}).issue(jwt, "mcp").Token, nil)
}

func roomIDsOf(rooms []chatworkRoomResponse) string {
	var ids []string
	for _, r := range rooms {
		ids = append(ids, r.RoomID)
	}
	return strings.Join(ids, ",")
}

func messageIDsOf(messages []chatworkMessageResponse) string {
	var ids []string
	for _, m := range messages {
		ids = append(ids, m.MessageID)
	}
	return strings.Join(ids, ",")
}

func TestMCPChatwork(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) { testMCPChatwork(t, driver) })
	}
}

func testMCPChatwork(t *testing.T, driver string) {
	api := newMCPChatworkAPI(t, driver)
	// The limit is tested in TestMCPChatworkLimit.
	api.server.chatworkMCPLimiter = newKeyLimiter(1000, time.Minute)
	api.connect(api.bob, bobChatworkToken)
	c := api.client(api.alice)

	// Without alice's own connection nothing is sent, even though bob is
	// connected.
	for _, call := range []struct {
		tool string
		args map[string]any
	}{
		{"chatwork_rooms_list", nil},
		{"chatwork_messages_list", map[string]any{"room_id": "2"}},
		{"chatwork_message_get", map[string]any{"room_id": "2", "message_id": "201"}},
		{"chatwork_messages_search", map[string]any{"query": "invoice"}},
	} {
		c.fails(call.tool, call.args, "chatwork_not_connected: Chatwork is not connected. Connect it in Astovia's settings.")
	}
	if calls := api.cw.calls(); len(calls) != 0 {
		t.Fatalf("calls without a connection: %+v", calls)
	}
	api.connect(api.alice, aliceChatworkToken)

	// rooms_list: newest first, ties by room ID, with alice's token.
	var rooms chatworkRoomListResponse
	c.ok("chatwork_rooms_list", nil, &rooms)
	if roomIDsOf(rooms.Rooms) != "12,11,10,9,8,7,6,4,3,2,1,5" || rooms.Total != 12 {
		t.Fatalf("rooms: %s %d", roomIDsOf(rooms.Rooms), rooms.Total)
	}
	if r := rooms.Rooms[0]; r.Name != "Room 12" || r.Type != "group" || r.UnreadNum != 3 || r.MentionNum != 1 || r.LastUpdateAt == nil || *r.LastUpdateAt != formatTime(2000) || r.URL != "https://www.chatwork.com/#!rid12" {
		t.Fatalf("room 12: %+v", r)
	}
	if r := rooms.Rooms[11]; r.RoomID != "5" || r.LastUpdateAt != nil {
		t.Fatalf("room without an update: %+v", r)
	}
	if calls := api.cw.calls(); len(calls) != 1 || calls[0].path != "/rooms" || calls[0].token != aliceChatworkToken {
		t.Fatalf("rooms calls: %+v", calls)
	}
	c.ok("chatwork_rooms_list", map[string]any{"type": "group", "limit": 3, "query": nil}, &rooms)
	if roomIDsOf(rooms.Rooms) != "12,11,10" || rooms.Total != 10 {
		t.Fatalf("group rooms: %s %d", roomIDsOf(rooms.Rooms), rooms.Total)
	}
	c.ok("chatwork_rooms_list", map[string]any{"query": " team  SALES "}, &rooms)
	if roomIDsOf(rooms.Rooms) != "3" || rooms.Total != 1 {
		t.Fatalf("rooms by name: %s %d", roomIDsOf(rooms.Rooms), rooms.Total)
	}
	c.ok("chatwork_rooms_list", map[string]any{"type": "direct", "query": "sales"}, &rooms)
	if len(rooms.Rooms) != 0 || rooms.Total != 0 || rooms.Rooms == nil {
		t.Fatalf("no rooms: %+v", rooms)
	}
	api.cw.calls()

	// messages_list: newest first, AND matching in the body and the sender
	// ignoring case, long bodies cut.
	long := strings.Repeat("い", 2001)
	api.cw.set(func(f *fakeChatworkAPI) {
		f.messages["2"] = `[{"message_id":"10","account":{"account_id":7,"name":"Bob"},"body":"[To:101] Invoice please","send_time":100},` +
			`{"message_id":"bad","account":{"account_id":7,"name":"Bob"},"body":"invoice","send_time":150},` +
			`{"message_id":"11","account":{"account_id":"x","name":"Carol"},"body":"` + long + ` invoice","send_time":200},` +
			`{"message_id":"13","account":{"account_id":8,"name":"Carol"},"body":"lunch","send_time":300},` +
			`{"message_id":"12","account":{"account_id":8,"name":"Carol"},"body":"INVOICE sent","send_time":300}]`
	})
	var list chatworkMessageListResponse
	c.ok("chatwork_messages_list", map[string]any{"room_id": "2"}, &list)
	if messageIDsOf(list.Messages) != "13,12,11,10" || list.Matched != 4 || list.Fetched != 4 || list.RoomID != "2" || list.RoomURL != "https://www.chatwork.com/#!rid2" {
		t.Fatalf("messages: %s %+v", messageIDsOf(list.Messages), list)
	}
	calls := api.cw.calls()
	if len(calls) != 1 || calls[0].path != "/rooms/2/messages" || calls[0].query != "force=1" || calls[0].token != aliceChatworkToken {
		t.Fatalf("messages calls: %+v", calls)
	}
	m := list.Messages[3]
	if m.RoomID != "2" || m.AccountID != "7" || m.AccountName != "Bob" || m.Body != "[To:101] Invoice please" || m.BodyTruncated || m.SentAt != formatTime(100) || m.URL != "https://www.chatwork.com/#!rid2-10" || m.RoomName != nil {
		t.Fatalf("message: %+v", m)
	}
	if m = list.Messages[2]; m.AccountID != "" || !m.BodyTruncated || m.Body != strings.Repeat("い", 2000) {
		t.Fatalf("truncated: %+v", m.AccountID)
	}
	c.ok("chatwork_messages_list", map[string]any{"room_id": "2", "query": "invoice carol", "limit": 1}, &list)
	if messageIDsOf(list.Messages) != "12" || list.Matched != 2 || list.Fetched != 4 {
		t.Fatalf("messages by query: %s %+v", messageIDsOf(list.Messages), list)
	}
	// The text of a truncated body still matches.
	c.ok("chatwork_messages_list", map[string]any{"room_id": "2", "query": "い invoice", "limit": nil}, &list)
	if messageIDsOf(list.Messages) != "11" {
		t.Fatalf("messages by the end of a long body: %s", messageIDsOf(list.Messages))
	}
	text, isError := c.call("chatwork_messages_list", map[string]any{"room_id": "2", "query": "lunch"})
	if isError || strings.Contains(text, "body_truncated") {
		t.Fatalf("body_truncated without truncation: %s", text)
	}
	api.cw.calls()

	// message_get returns the whole body.
	var got chatworkMessageResponse
	c.ok("chatwork_message_get", map[string]any{"room_id": "2", "message_id": "201"}, &got)
	if got.MessageID != "201" || got.RoomID != "2" || got.Body != strings.Repeat("あ", 2100) || got.BodyTruncated || got.URL != "https://www.chatwork.com/#!rid2-201" || got.AccountName != "Bob" {
		t.Fatalf("message_get: %+v", got.MessageID)
	}
	if calls = api.cw.calls(); len(calls) != 1 || calls[0].path != "/rooms/2/messages/201" || calls[0].query != "" || calls[0].token != aliceChatworkToken {
		t.Fatalf("message calls: %+v", calls)
	}

	// Chatwork's errors.
	reset := time.Date(2026, 10, 24, 1, 12, 3, 0, time.UTC)
	for _, tc := range []struct {
		status int
		reset  string
		want   string
	}{
		{401, "", "chatwork_token_invalid: The Chatwork API token cannot be used. Reconnect Chatwork in Astovia's settings."},
		{429, strconv.FormatInt(reset.Unix(), 10), "rate_limited: Chatwork rate limit reached. Try again after 2026-10-24T01:12:03Z."},
		{429, "", "rate_limited: Chatwork rate limit reached. Try again in a few minutes."},
		{403, "", "not_found: The room or message was not found, or you cannot access it."},
		{404, "", "not_found: The room or message was not found, or you cannot access it."},
		{500, "", "chatwork_unavailable: Chatwork could not be reached. Try again later."},
	} {
		api.cw.set(func(f *fakeChatworkAPI) {
			f.status = map[string]int{"/rooms": tc.status, "/rooms/2/messages": tc.status, "/rooms/2/messages/201": tc.status}
			f.reset = tc.reset
		})
		c.fails("chatwork_rooms_list", nil, tc.want)
		c.fails("chatwork_messages_list", map[string]any{"room_id": "2"}, tc.want)
		c.fails("chatwork_message_get", map[string]any{"room_id": "2", "message_id": "201"}, tc.want)
		c.fails("chatwork_messages_search", map[string]any{"query": "x"}, tc.want)
	}
	// A missing message (204) is not found.
	api.cw.set(func(f *fakeChatworkAPI) { f.status = map[string]int{"/rooms/2/messages/201": 204}; f.reset = "" })
	c.fails("chatwork_message_get", map[string]any{"room_id": "2", "message_id": "201"}, "not_found: The room or message was not found, or you cannot access it.")
	// The token check does not change the connection.
	if conn, err := api.store.ChatworkConnection(context.Background(), "alice-id"); err != nil || conn.Status == "error" {
		t.Fatalf("connection after 401: %+v %v", conn.Status, err)
	}
	api.cw.set(func(f *fakeChatworkAPI) { f.status, f.messages = map[string]int{}, map[string]string{} })
	api.cw.calls()

	// messages_search: the 10 most recently updated rooms by default.
	var found chatworkSearchResponse
	c.ok("chatwork_messages_search", map[string]any{"query": "deadline INVOICE", "limit": 3}, &found)
	var searched []string
	for _, r := range found.SearchedRooms {
		searched = append(searched, r.RoomID)
	}
	if strings.Join(searched, ",") != "12,11,10,9,8,7,6,4,3,2" || found.Matched != 10 || messageIDsOf(found.Messages) != "1202,1102,1002" || len(found.Skipped) != 0 || found.Skipped == nil || found.MissingRoomIDs == nil ||
		found.Query != "deadline INVOICE" || found.Note != "Only the latest 100 messages of each room are searched." {
		t.Fatalf("search: %s %+v", searched, found)
	}
	if m := found.Messages[0]; m.RoomName == nil || *m.RoomName != "Room 12" || m.RoomID != "12" || m.AccountName != "Carol" || m.URL != "https://www.chatwork.com/#!rid12-1202" {
		t.Fatalf("search message: %+v", m)
	}
	if found.SearchedRooms[8] != (chatworkSearchedRoom{RoomID: "3", RoomName: "Sales Team"}) {
		t.Fatalf("searched room: %+v", found.SearchedRooms[8])
	}
	calls = api.cw.calls()
	if len(calls) != 11 || calls[0].path != "/rooms" {
		t.Fatalf("search calls: %+v", calls)
	}
	for _, call := range calls[1:] {
		if call.query != "force=1" || call.token != aliceChatworkToken {
			t.Fatalf("search call: %+v", call)
		}
	}
	// The sender's name matches; type and rooms choose the rooms.
	c.ok("chatwork_messages_search", map[string]any{"query": "bob hello", "type": "group", "rooms": 2}, &found)
	if messageIDsOf(found.Messages) != "1201,1101" || found.Matched != 2 || len(found.SearchedRooms) != 2 {
		t.Fatalf("search by type: %+v", found)
	}
	api.cw.calls()

	// room_ids: duplicates are read once, unknown rooms are reported, and a
	// room that cannot be read is skipped.
	api.cw.set(func(f *fakeChatworkAPI) {
		f.status = map[string]int{"/rooms/3/messages": 404, "/rooms/4/messages": 500}
	})
	c.ok("chatwork_messages_search", map[string]any{"query": "invoice", "room_ids": []any{"3", "1", "99", "1", "4", "2"}}, &found)
	want := chatworkSearchResponse{Query: "invoice", Matched: 2, Note: chatworkSearchNote,
		SearchedRooms:  []chatworkSearchedRoom{{RoomID: "1", RoomName: "Room 1"}, {RoomID: "2", RoomName: "Room 2"}},
		Skipped:        []chatworkSkippedRoom{{RoomID: "3", RoomName: "Sales Team", Reason: "not_found"}, {RoomID: "4", RoomName: "Room 4", Reason: "unavailable"}},
		MissingRoomIDs: []string{"99"}}
	if messageIDsOf(found.Messages) != "202,102" || !reflect.DeepEqual(found.SearchedRooms, want.SearchedRooms) || !reflect.DeepEqual(found.Skipped, want.Skipped) ||
		!reflect.DeepEqual(found.MissingRoomIDs, want.MissingRoomIDs) || found.Matched != want.Matched {
		t.Fatalf("search by room_ids: %+v", found)
	}
	if calls = api.cw.calls(); len(calls) != 5 {
		t.Fatalf("room_ids calls: %+v", calls)
	}
	// A 429 skips the room.
	api.cw.set(func(f *fakeChatworkAPI) { f.status = map[string]int{"/rooms/3/messages": 429} })
	c.ok("chatwork_messages_search", map[string]any{"query": "invoice", "room_ids": []any{"3"}}, &found)
	if len(found.SearchedRooms) != 0 || !reflect.DeepEqual(found.Skipped, []chatworkSkippedRoom{{RoomID: "3", RoomName: "Sales Team", Reason: "rate_limited"}}) || len(found.Messages) != 0 || found.Messages == nil {
		t.Fatalf("search with 429: %+v", found)
	}
	// A rejected token fails the whole search.
	api.cw.set(func(f *fakeChatworkAPI) { f.status = map[string]int{"/rooms/7/messages": 401} })
	c.fails("chatwork_messages_search", map[string]any{"query": "invoice"}, "chatwork_token_invalid: The Chatwork API token cannot be used. Reconnect Chatwork in Astovia's settings.")
	api.cw.set(func(f *fakeChatworkAPI) { f.status = map[string]int{} })
	api.cw.calls()

	// Invalid arguments are rejected before Chatwork is called.
	const notFound = "not_found: The room or message was not found, or you cannot access it."
	const badQuery = "invalid_request: query must be 1-100 characters with at least one word."
	const badType = "invalid_request: type must be my, direct or group."
	const badRoomIDs = "invalid_request: room_ids must be an array of 1-20 room IDs."
	const combined = "invalid_request: room_ids cannot be combined with type or rooms."
	var many []any
	for i := 1; i <= 21; i++ {
		many = append(many, strconv.Itoa(i))
	}
	for _, tc := range []struct {
		tool string
		args any
		want string
	}{
		{"chatwork_rooms_list", map[string]any{"limit": 0}, "invalid_request: limit must be an integer from 1 to 200."},
		{"chatwork_rooms_list", map[string]any{"limit": 201}, "invalid_request: limit must be an integer from 1 to 200."},
		{"chatwork_rooms_list", map[string]any{"limit": 50.5}, "invalid_request: limit must be an integer from 1 to 200."},
		{"chatwork_rooms_list", map[string]any{"limit": "50"}, "invalid_request: limit must be an integer from 1 to 200."},
		{"chatwork_rooms_list", map[string]any{"type": "public"}, badType},
		{"chatwork_rooms_list", map[string]any{"type": ""}, badType},
		{"chatwork_rooms_list", map[string]any{"query": " 　 "}, badQuery},
		{"chatwork_rooms_list", map[string]any{"query": strings.Repeat("a", 101)}, badQuery},
		{"chatwork_rooms_list", map[string]any{"query": 1}, badQuery},
		{"chatwork_rooms_list", map[string]any{"user_id": "bob-id"}, "invalid_request: Invalid arguments."},
		{"chatwork_messages_list", map[string]any{}, "invalid_request: room_id is required."},
		{"chatwork_messages_list", nil, "invalid_request: room_id is required."},
		{"chatwork_messages_list", map[string]any{"room_id": nil}, notFound},
		{"chatwork_messages_list", map[string]any{"room_id": 2}, notFound},
		{"chatwork_messages_list", map[string]any{"room_id": "0"}, notFound},
		{"chatwork_messages_list", map[string]any{"room_id": "2/../me"}, notFound},
		{"chatwork_messages_list", map[string]any{"room_id": "2", "limit": 101}, "invalid_request: limit must be an integer from 1 to 100."},
		{"chatwork_messages_list", map[string]any{"room_id": "2", "query": ""}, badQuery},
		{"chatwork_message_get", map[string]any{"room_id": "2"}, "invalid_request: message_id is required."},
		{"chatwork_message_get", map[string]any{"message_id": "201"}, "invalid_request: room_id is required."},
		{"chatwork_message_get", map[string]any{"room_id": "2", "message_id": "01"}, notFound},
		{"chatwork_message_get", map[string]any{"room_id": "2", "message_id": nil}, notFound},
		{"chatwork_message_get", map[string]any{"room_id": "x", "message_id": "201"}, notFound},
		{"chatwork_messages_search", map[string]any{}, "invalid_request: query is required."},
		{"chatwork_messages_search", map[string]any{"query": nil}, "invalid_request: query is required."},
		{"chatwork_messages_search", map[string]any{"query": "   "}, badQuery},
		{"chatwork_messages_search", map[string]any{"query": strings.Repeat("あ", 101)}, badQuery},
		{"chatwork_messages_search", map[string]any{"query": "x", "room_ids": many}, badRoomIDs},
		{"chatwork_messages_search", map[string]any{"query": "x", "room_ids": []any{}}, badRoomIDs},
		{"chatwork_messages_search", map[string]any{"query": "x", "room_ids": []any{"1", "x"}}, badRoomIDs},
		{"chatwork_messages_search", map[string]any{"query": "x", "room_ids": []any{nil}}, badRoomIDs},
		{"chatwork_messages_search", map[string]any{"query": "x", "room_ids": []any{1}}, badRoomIDs},
		{"chatwork_messages_search", map[string]any{"query": "x", "room_ids": "1"}, badRoomIDs},
		{"chatwork_messages_search", map[string]any{"query": "x", "room_ids": []any{"1"}, "type": "group"}, combined},
		{"chatwork_messages_search", map[string]any{"query": "x", "room_ids": []any{"1"}, "rooms": 2}, combined},
		{"chatwork_messages_search", map[string]any{"query": "x", "rooms": 21}, "invalid_request: rooms must be an integer from 1 to 20."},
		{"chatwork_messages_search", map[string]any{"query": "x", "limit": 0}, "invalid_request: limit must be an integer from 1 to 100."},
		{"chatwork_messages_search", map[string]any{"query": "x", "type": "all"}, badType},
	} {
		c.fails(tc.tool, tc.args, tc.want)
	}
	if calls = api.cw.calls(); len(calls) != 0 {
		t.Fatalf("calls for invalid arguments: %+v", calls)
	}
	// Null optional arguments are omitted ones.
	c.ok("chatwork_messages_search", map[string]any{"query": "invoice", "room_ids": nil, "type": nil, "rooms": nil, "limit": nil}, &found)
	if len(found.SearchedRooms) != 10 {
		t.Fatalf("search with nulls: %+v", found)
	}
	api.cw.calls()

	// Bob's calls use bob's token.
	bob := api.client(api.bob)
	bob.ok("chatwork_rooms_list", nil, &rooms)
	if calls = api.cw.calls(); len(calls) != 1 || calls[0].token != bobChatworkToken {
		t.Fatalf("bob's calls: %+v", calls)
	}
}

// The MCP tools call Chatwork at most 100 times per user in 5 minutes.
func TestMCPChatworkLimit(t *testing.T) {
	api := newMCPChatworkAPI(t, "sqlite")
	api.connect(api.alice, aliceChatworkToken)
	api.connect(api.bob, bobChatworkToken)
	c := api.client(api.alice)
	var list chatworkMessageListResponse
	for i := 0; i < 100; i++ {
		c.ok("chatwork_messages_list", map[string]any{"room_id": "2"}, &list)
	}
	if n := len(api.cw.calls()); n != 100 {
		t.Fatalf("calls: %d", n)
	}
	const limited = "rate_limited: Too many Chatwork requests from MCP. Try again in a few minutes."
	c.fails("chatwork_messages_list", map[string]any{"room_id": "2"}, limited)
	c.fails("chatwork_rooms_list", nil, limited)
	c.fails("chatwork_message_get", map[string]any{"room_id": "2", "message_id": "201"}, limited)
	c.fails("chatwork_messages_search", map[string]any{"query": "x"}, limited)
	if n := len(api.cw.calls()); n != 0 {
		t.Fatalf("calls over the limit: %d", n)
	}
	// Bob has his own count.
	var rooms chatworkRoomListResponse
	api.client(api.bob).ok("chatwork_rooms_list", nil, &rooms)

	// A search stops reading rooms when the limit runs out: GET /rooms and
	// two rooms fit in 3. The limiter is replaced before any server
	// goroutine starts.
	api = newMCPChatworkAPI(t, "sqlite")
	api.server.chatworkMCPLimiter = newKeyLimiter(3, time.Minute)
	api.connect(api.alice, aliceChatworkToken)
	c = api.client(api.alice)
	var found chatworkSearchResponse
	c.ok("chatwork_messages_search", map[string]any{"query": "invoice", "rooms": 4}, &found)
	wantSkipped := []chatworkSkippedRoom{{RoomID: "10", RoomName: "Room 10", Reason: "rate_limited"}, {RoomID: "9", RoomName: "Room 9", Reason: "rate_limited"}}
	if messageIDsOf(found.Messages) != "1202,1102" || found.Matched != 2 || len(found.SearchedRooms) != 2 || !reflect.DeepEqual(found.Skipped, wantSkipped) {
		t.Fatalf("search over the limit: %+v", found)
	}
	if n := len(api.cw.calls()); n != 3 {
		t.Fatalf("search calls: %d", n)
	}
}
