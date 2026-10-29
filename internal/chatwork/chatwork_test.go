package chatwork

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

var testNow = time.Date(2026, 10, 24, 10, 0, 0, 0, time.UTC)

const testToken = "chatwork-secret-token-0123"

type fakeChatwork struct {
	mu       sync.Mutex
	requests []*http.Request
	handler  http.HandlerFunc
}

func newFake(t *testing.T, h http.HandlerFunc) (*fakeChatwork, *Client) {
	f := &fakeChatwork{handler: h}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests = append(f.requests, r)
		f.mu.Unlock()
		f.handler(w, r)
	}))
	t.Cleanup(srv.Close)
	c := New(func() time.Time { return testNow })
	c.BaseURL = srv.URL + "/v2"
	return f, c
}

func (f *fakeChatwork) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func (f *fakeChatwork) last() *http.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[len(f.requests)-1]
}

func reply(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}
}

func status(code int) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) }
}

func TestRequests(t *testing.T) {
	f, c := newFake(t, reply(`{"account_id":123,"room_id":5,"name":"Alice","organization_name":"Acme","avatar_image_url":"https://example.com/a.png"}`))
	me, err := c.Me(context.Background(), testToken)
	if err != nil || me != (Account{ID: "123", Name: "Alice", OrganizationName: "Acme"}) {
		t.Fatalf("Me: %+v %v", me, err)
	}
	r := f.last()
	if r.Method != http.MethodGet || r.URL.Path != "/v2/me" || r.Header.Get("x-chatworktoken") != testToken || r.Header.Get("Authorization") != "" || r.URL.RawQuery != "" {
		t.Fatalf("request: %s %s %v", r.Method, r.URL, r.Header)
	}
	for _, body := range []string{`{"account_id":"0123","name":"x"}`, `{"name":"x"}`, `{"account_id":1.5}`, `[]`} {
		_, c = newFake(t, reply(body))
		if _, err = c.Me(context.Background(), testToken); !errors.Is(err, ErrUnexpected) {
			t.Fatalf("Me %s: %v", body, err)
		}
	}

	f, c = newFake(t, reply(`[{"room_id":1,"name":"My","type":"my","last_update_time":100},{"room_id":2,"name":"Team","type":"group","last_update_time":200},{"room_id":"x","name":"bad","type":"group"},{"room_id":0,"type":"direct"}]`))
	rooms, err := c.Rooms(context.Background(), testToken)
	if err != nil || !reflect.DeepEqual(rooms, []Room{{ID: "1", Name: "My", Type: "my", LastUpdate: 100}, {ID: "2", Name: "Team", Type: "group", LastUpdate: 200}}) {
		t.Fatalf("Rooms: %+v %v", rooms, err)
	}
	if f.last().URL.Path != "/v2/rooms" {
		t.Fatalf("rooms path: %s", f.last().URL)
	}

	f, c = newFake(t, reply(`[{"message_id":"1001","account":{"account_id":7,"name":"Bob"},"body":"[To:123] hi","send_time":300,"update_time":0},{"message_id":"01","account":{"account_id":7},"body":"bad"},{"message_id":1002,"account":{"account_id":"x","name":"?"},"body":"b","send_time":301}]`))
	page, err := c.Messages(context.Background(), testToken, "2")
	want := []Message{{ID: "1001", AccountID: "7", AccountName: "Bob", Body: "[To:123] hi", SendTime: 300}, {ID: "1002", AccountName: "?", Body: "b", SendTime: 301}}
	if err != nil || page.Raw != 3 || !reflect.DeepEqual(page.Messages, want) {
		t.Fatalf("Messages: %+v %v", page, err)
	}
	r = f.last()
	if r.URL.Path != "/v2/rooms/2/messages" || r.URL.Query().Get("force") != "1" || r.Header.Get("x-chatworktoken") != testToken {
		t.Fatalf("messages request: %s", r.URL)
	}
	// An invalid room is not requested.
	for _, room := range []string{"", "0", "01", "2/../me", "abc"} {
		if _, err = c.Messages(context.Background(), testToken, room); !errors.Is(err, ErrUnexpected) {
			t.Fatalf("room %q: %v", room, err)
		}
	}
	if n := f.count(); n != 1 {
		t.Fatalf("requests for invalid rooms: %d", n)
	}

	f, c = newFake(t, reply(`[{"task_id":11,"room":{"room_id":2,"name":"Team"},"assigned_by_account":{"account_id":7,"name":"Bob"},"message_id":"1001","body":"do it","limit_time":1700000000,"status":"open","limit_type":"date"},{"task_id":"bad","room":{"room_id":2},"message_id":"1"},{"task_id":12,"room":{"room_id":2},"message_id":"0"}]`))
	tasks, err := c.MyTasks(context.Background(), testToken)
	if err != nil || tasks.Raw != 3 || !reflect.DeepEqual(tasks.Tasks, []Task{{ID: "11", RoomID: "2", RoomName: "Team", AssignedByID: "7", AssignedByName: "Bob", MessageID: "1001", Body: "do it", LimitTime: 1700000000, LimitType: "date"}}) {
		t.Fatalf("MyTasks: %+v %v", tasks, err)
	}
	if r = f.last(); r.URL.Path != "/v2/my/tasks" || r.URL.Query().Get("status") != "open" {
		t.Fatalf("tasks request: %s", r.URL)
	}

	// 204 is an empty list.
	_, c = newFake(t, status(http.StatusNoContent))
	if page, err = c.Messages(context.Background(), testToken, "2"); err != nil || len(page.Messages) != 0 || page.Raw != 0 {
		t.Fatalf("204 messages: %+v %v", page, err)
	}
	if tasks, err = c.MyTasks(context.Background(), testToken); err != nil || len(tasks.Tasks) != 0 || tasks.Raw != 0 {
		t.Fatalf("204 tasks: %+v %v", tasks, err)
	}
	if rooms, err = c.Rooms(context.Background(), testToken); err != nil || len(rooms) != 0 {
		t.Fatalf("204 rooms: %+v %v", rooms, err)
	}
}

func TestErrors(t *testing.T) {
	inRange := strconv.FormatInt(testNow.Add(10*time.Minute).Unix(), 10)
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
		check   func(error) bool
	}{
		{"401", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"errors":["Invalid API token"]}`))
		}, func(e error) bool { return errors.Is(e, ErrUnauthorized) }},
		{"403", status(403), func(e error) bool {
			var a *APIError
			return errors.As(e, &a) && a.Status == 403 && a.Method == "messages" && !a.TooLarge
		}},
		{"404", status(404), func(e error) bool { var a *APIError; return errors.As(e, &a) && a.Status == 404 }},
		{"429 with reset", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("x-ratelimit-reset", inRange)
			w.WriteHeader(429)
		}, func(e error) bool {
			var r *RateLimitError
			return errors.As(e, &r) && r.RetryAt.Equal(testNow.Add(10*time.Minute)) && errors.Is(e, ErrRateLimited)
		}},
		{"429 with a past reset", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("x-ratelimit-reset", strconv.FormatInt(testNow.Unix(), 10))
			w.WriteHeader(429)
		}, func(e error) bool { var r *RateLimitError; return errors.As(e, &r) && r.RetryAt.IsZero() }},
		{"429 with a far reset", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("x-ratelimit-reset", strconv.FormatInt(testNow.Add(2*time.Hour).Unix(), 10))
			w.WriteHeader(429)
		}, func(e error) bool { var r *RateLimitError; return errors.As(e, &r) && r.RetryAt.IsZero() }},
		{"429 without reset", status(429), func(e error) bool { var r *RateLimitError; return errors.As(e, &r) && r.RetryAt.IsZero() }},
		{"500", status(500), func(e error) bool { var s *StatusError; return errors.As(e, &s) && s.Code == 500 }},
		{"redirect is not followed", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Location", "https://evil.example/")
			w.WriteHeader(302)
		}, func(e error) bool { var s *StatusError; return errors.As(e, &s) && s.Code == 302 }},
		{"invalid JSON", reply(`[{"message_id":`), func(e error) bool { return errors.Is(e, ErrUnexpected) }},
		{"object instead of a list", reply(`{"errors":["x"]}`), func(e error) bool { return errors.Is(e, ErrUnexpected) }},
		{"too large", reply(`[{"message_id":"1","body":"` + strings.Repeat("a", bodyLimit) + `"}]`), func(e error) bool {
			var a *APIError
			return errors.As(e, &a) && a.TooLarge && a.Status == 200
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, c := newFake(t, tc.handler)
			_, err := c.Messages(context.Background(), testToken, "2")
			if err == nil || !tc.check(err) {
				t.Fatalf("error: %v", err)
			}
			// Errors never carry the token, the URL or the body.
			for _, s := range []string{testToken, "force", "Invalid API token", "evil.example", "aaaa", "/rooms/"} {
				if strings.Contains(err.Error(), s) {
					t.Fatalf("error %q contains %q", err, s)
				}
			}
			if n := f.count(); n != 1 {
				t.Fatalf("requests: %d", n)
			}
		})
	}

	// A connection error keeps only its cause, without the URL.
	c := New(func() time.Time { return testNow })
	c.BaseURL = "http://127.0.0.1:1/v2"
	_, err := c.Messages(context.Background(), testToken, "2")
	if err == nil || strings.Contains(err.Error(), "force") || strings.Contains(err.Error(), testToken) || !strings.HasPrefix(err.Error(), "chatwork: messages: ") {
		t.Fatalf("connection error: %v", err)
	}
	// The caller's context error stays visible.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = c.Messages(ctx, testToken, "2"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled: %v", err)
	}
}

// A redirect is not followed, so the token is not sent to the target.
func TestRedirectKeepsToken(t *testing.T) {
	var got []string
	var mu sync.Mutex
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, r.Header.Get("x-chatworktoken"))
		mu.Unlock()
		_, _ = w.Write([]byte(`{"account_id":1}`))
	}))
	t.Cleanup(target.Close)
	_, c := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/me", http.StatusFound)
	})
	var s *StatusError
	if _, err := c.Me(context.Background(), testToken); !errors.As(err, &s) || s.Code != http.StatusFound {
		t.Fatalf("redirect: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 0 {
		t.Fatalf("redirect target received %d requests", len(got))
	}
}

func TestIDs(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{{"9", "10", -1}, {"10", "9", 1}, {"123", "124", -1}, {"124", "124", 0}, {"1000000000000000000001", "999999999999999999999", 1}} {
		if got := CompareIDs(tc.a, tc.b); got != tc.want {
			t.Fatalf("CompareIDs(%s, %s) = %d", tc.a, tc.b, got)
		}
	}
	if !ValidID("12345678901234567890") || ValidID("123456789012345678901") || ValidID("0") || ValidID("01") || ValidID("-1") || ValidID("") {
		t.Fatal("ValidID")
	}
	if !ValidMessageID(strings.Repeat("9", 30)) || ValidMessageID(strings.Repeat("9", 31)) || ValidMessageID("0") {
		t.Fatal("ValidMessageID")
	}
}

func TestRoomCounts(t *testing.T) {
	_, c := newFake(t, reply(`[{"room_id":1,"name":"My","type":"my","last_update_time":100,"unread_num":3,"mention_num":1},`+
		`{"room_id":2,"name":"Team","type":"group","last_update_time":200,"unread_num":"3","mention_num":null},`+
		`{"room_id":3,"name":"Bob","type":"direct","last_update_time":300,"unread_num":1.5}]`))
	rooms, err := c.Rooms(context.Background(), testToken)
	want := []Room{
		{ID: "1", Name: "My", Type: "my", LastUpdate: 100, UnreadNum: 3, MentionNum: 1},
		{ID: "2", Name: "Team", Type: "group", LastUpdate: 200},
		{ID: "3", Name: "Bob", Type: "direct", LastUpdate: 300},
	}
	if err != nil || !reflect.DeepEqual(rooms, want) {
		t.Fatalf("Rooms: %+v %v", rooms, err)
	}
}

func TestMessage(t *testing.T) {
	f, c := newFake(t, reply(`{"message_id":"1001","account":{"account_id":7,"name":"Bob"},"body":"[To:123] hi","send_time":300,"update_time":0}`))
	m, err := c.Message(context.Background(), testToken, "2", "1001")
	if err != nil || m != (Message{ID: "1001", AccountID: "7", AccountName: "Bob", Body: "[To:123] hi", SendTime: 300}) {
		t.Fatalf("Message: %+v %v", m, err)
	}
	r := f.last()
	if r.URL.Path != "/v2/rooms/2/messages/1001" || r.URL.RawQuery != "" || r.Header.Get("x-chatworktoken") != testToken {
		t.Fatalf("message request: %s", r.URL)
	}
	// Invalid IDs are not requested.
	for _, ids := range [][2]string{{"0", "1001"}, {"2/../me", "1001"}, {"2", "01"}, {"2", ""}, {"2", "1/../../me"}} {
		if _, err = c.Message(context.Background(), testToken, ids[0], ids[1]); !errors.Is(err, ErrUnexpected) {
			t.Fatalf("ids %q: %v", ids, err)
		}
	}
	if n := f.count(); n != 1 {
		t.Fatalf("requests for invalid ids: %d", n)
	}

	_, c = newFake(t, reply(`{"message_id":"0","body":"x"}`))
	if _, err = c.Message(context.Background(), testToken, "2", "1001"); !errors.Is(err, ErrUnexpected) {
		t.Fatalf("invalid message_id: %v", err)
	}
	// 204 and 404 are a missing message.
	for _, code := range []int{http.StatusNoContent, http.StatusNotFound} {
		_, c = newFake(t, status(code))
		var a *APIError
		if _, err = c.Message(context.Background(), testToken, "2", "1001"); !errors.As(err, &a) || a.Status != 404 || a.Method != "message" {
			t.Fatalf("%d: %v", code, err)
		}
	}
}
