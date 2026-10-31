package notify

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/i2bskn/astovia/internal/mail"
	"github.com/i2bskn/astovia/internal/store"
)

// recorder is a channel that records what it is asked to send. errFor, when
// set, decides the result per user and message.
type recorder struct {
	mu     sync.Mutex
	sent   []sentMessage
	errFor func(userID string, m Message) error
}

type sentMessage struct {
	userID string
	m      Message
}

func (c *recorder) Name() string { return "recorder" }
func (c *recorder) Send(_ context.Context, userID string, m Message) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent = append(c.sent, sentMessage{userID: userID, m: m})
	if c.errFor != nil {
		if err := c.errFor(userID, m); err != nil {
			return 0, err
		}
	}
	return 1, nil
}

func (c *recorder) count(userID string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, s := range c.sent {
		if userID == "" || s.userID == userID {
			n++
		}
	}
	return n
}

// rowState is a notification row as stored.
type rowState struct {
	DedupeKey   string
	Channel     string
	Attempts    int64
	LastError   sql.NullString
	ScheduledAt int64
	SentAt      sql.NullInt64
}

// rows returns the user's notification rows by ID, read directly.
func (f *fixture) rows(userID string) map[string]rowState {
	f.t.Helper()
	name := f.driver
	if name == "postgres" {
		name = "pgx"
	}
	db, err := sql.Open(name, f.dsn)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	// userID is a fixed test value.
	list, err := db.QueryContext(context.Background(), "SELECT id, dedupe_key, channel, attempts, last_error, scheduled_at, sent_at FROM notifications WHERE user_id = '"+userID+"'")
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = list.Close() }()
	out := map[string]rowState{}
	for list.Next() {
		var id string
		var r rowState
		if err = list.Scan(&id, &r.DedupeKey, &r.Channel, &r.Attempts, &r.LastError, &r.ScheduledAt, &r.SentAt); err != nil {
			f.t.Fatal(err)
		}
		out[id] = r
	}
	if err = list.Err(); err != nil {
		f.t.Fatal(err)
	}
	return out
}

// byChannel returns the user's rows with the dedupe key by channel.
func (f *fixture) byChannel(userID, key string) map[string]rowState {
	f.t.Helper()
	out := map[string]rowState{}
	for _, r := range f.rows(userID) {
		if r.DedupeKey == key {
			out[r.Channel] = r
		}
	}
	return out
}

// exec runs a statement directly on the database.
func (f *fixture) exec(q string) {
	f.t.Helper()
	name := f.driver
	if name == "postgres" {
		name = "pgx"
	}
	db, err := sql.Open(name, f.dsn)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err = db.ExecContext(context.Background(), q); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) routes(user string, webpush, slack, email []string) {
	f.t.Helper()
	if err := f.s.SaveNotificationRoutes(context.Background(), user+"-id", user+"-session", webpush, slack, email, f.now.Unix()); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) webhook(user, sealed string) {
	f.t.Helper()
	if err := f.s.SetSlackWebhook(context.Background(), user+"-id", user+"-session", sealed, f.now.Unix()); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) enqueue(user string, list ...store.Notification) {
	f.t.Helper()
	if err := f.s.EnqueueNotifications(context.Background(), user+"-id", list); err != nil {
		f.t.Fatal(err)
	}
}

func staleNotification(id string, at int64) store.Notification {
	return store.Notification{ID: id, Kind: store.NotificationStale, DedupeKey: "stale:" + id + ":1", Title: "t " + id, Body: "b", URL: "/", ScheduledAt: at, CreatedAt: at}
}

func TestDispatcherChannels(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		f := newFixture(t, driver, "alice")
		ctx := context.Background()
		f.subscribe("alice", "s", "https://push.example/a")
		f.routes("alice", store.RoutableKinds, store.RoutableKinds, store.RoutableKinds)
		f.webhook("alice", "sealed")
		web, slack, email := &recorder{}, &recorder{}, &recorder{}
		d := &Dispatcher{Store: f.s, Channels: map[string]Channel{store.ChannelWebPush: web, store.ChannelSlack: slack, store.ChannelEmail: email}, Now: f.clock}
		now := f.now.Unix()

		// Each row goes to the channel of its column, with its own ID.
		f.enqueue("alice", staleNotification("n1", now))
		due := f.pending()
		if len(due) != 3 {
			t.Fatalf("rows: %+v", due)
		}
		ids := map[string]string{}
		for _, n := range due {
			ids[n.Channel] = n.ID
		}
		d.RunOnce(ctx)
		for name, c := range map[string]*recorder{store.ChannelWebPush: web, store.ChannelSlack: slack, store.ChannelEmail: email} {
			if len(c.sent) != 1 || c.sent[0].m.ID != ids[name] || c.sent[0].m.Title != "t n1" || c.sent[0].userID != "alice-id" {
				t.Fatalf("%s: %+v", name, c.sent)
			}
		}
		if ids[store.ChannelWebPush] != "n1" || ids[store.ChannelSlack] == "n1" || ids[store.ChannelEmail] == "n1" || ids[store.ChannelSlack] == ids[store.ChannelEmail] {
			t.Fatalf("ids: %v", ids)
		}
		if len(f.pending()) != 0 {
			t.Fatalf("pending: %+v", f.pending())
		}

		// A channel missing from the map ends with no_subscription.
		delete(d.Channels, store.ChannelEmail)
		f.enqueue("alice", staleNotification("n2", now))
		d.RunOnce(ctx)
		rows := f.byChannel("alice-id", "stale:n2:1")
		if r := rows[store.ChannelEmail]; !r.SentAt.Valid || r.LastError.String != "no_subscription" || r.Attempts != 0 {
			t.Fatalf("missing channel: %+v", r)
		}
		if email.count("") != 1 || web.count("") != 2 || slack.count("") != 2 {
			t.Fatalf("sends: %d %d %d", web.count(""), slack.count(""), email.count(""))
		}

		// An invalid destination ends without a retry; the other channels'
		// retries are independent.
		d.Channels[store.ChannelEmail] = email
		slack.errFor = func(string, Message) error { return fmt.Errorf("slack webhook status 404: %w", ErrDestinationInvalid) }
		web.errFor = func(string, Message) error { return errors.New("push service status 500") }
		f.enqueue("alice", staleNotification("n3", now))
		d.RunOnce(ctx)
		rows = f.byChannel("alice-id", "stale:n3:1")
		if r := rows[store.ChannelWebPush]; r.Attempts != 1 || r.LastError.String != "send_failed" || r.ScheduledAt != now+60 || r.SentAt.Valid {
			t.Fatalf("web push retry: %+v", r)
		}
		if r := rows[store.ChannelSlack]; r.Attempts != 0 || r.LastError.String != "destination_invalid" || !r.SentAt.Valid {
			t.Fatalf("slack invalid: %+v", r)
		}
		if r := rows[store.ChannelEmail]; r.Attempts != 0 || r.LastError.Valid || !r.SentAt.Valid {
			t.Fatalf("email: %+v", r)
		}
	})
}

// TestDispatcherLegacyRow checks that a row stored before the channel column
// existed is read as Web Push and sent.
func TestDispatcherLegacyRow(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		f := newFixture(t, driver, "alice")
		now := f.now.Unix()
		f.exec(fmt.Sprintf("INSERT INTO notifications (id,user_id,kind,dedupe_key,title,body,url,scheduled_at,attempts,created_at) VALUES ('old','alice-id','digest','digest:old','t','b','/',%d,0,%d)", now, now))
		due := f.pending()
		if len(due) != 1 || due[0].Channel != store.ChannelWebPush {
			t.Fatalf("legacy row: %+v", due)
		}
		web := &recorder{}
		d := &Dispatcher{Store: f.s, Channels: map[string]Channel{store.ChannelWebPush: web}, Now: f.clock}
		d.RunOnce(context.Background())
		if web.count("alice-id") != 1 || len(f.pending()) != 0 {
			t.Fatalf("legacy row not sent: %+v %+v", web.sent, f.pending())
		}
	})
}

func TestDispatcherRateLimit(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		f := newFixture(t, driver, "alice", "bob")
		ctx := context.Background()
		now := f.now.Unix()
		f.routes("alice", nil, nil, []string{store.NotificationStale})
		f.subscribe("bob", "s", "https://push.example/b")
		var list []store.Notification
		for i := range 150 {
			list = append(list, staleNotification(fmt.Sprintf("a%03d", i), now-100))
		}
		f.enqueue("alice", list...)
		list = nil
		for i := range 5 {
			list = append(list, staleNotification(fmt.Sprintf("b%d", i), now-50))
		}
		f.enqueue("bob", list...)
		web, email := &recorder{}, &recorder{}
		limited := true
		email.errFor = func(string, Message) error {
			if limited {
				return fmt.Errorf("%w: resend status 429 rate_limit_exceeded", ErrRateLimited)
			}
			return nil
		}
		d := &Dispatcher{Store: f.s, Channels: map[string]Channel{store.ChannelWebPush: web, store.ChannelEmail: email}, Now: f.clock}

		// The first rate limit pauses e-mail: the rest of the batch is
		// postponed without sending.
		d.RunOnce(ctx)
		if email.count("") != 1 {
			t.Fatalf("e-mail sends: %d", email.count(""))
		}
		postponed, untouched := 0, 0
		for id, r := range f.rows("alice-id") {
			switch {
			case r.Attempts != 0 || r.SentAt.Valid:
				t.Fatalf("%s counted: %+v", id, r)
			case r.ScheduledAt == now+60 && r.LastError.String == "rate_limited":
				postponed++
			case r.ScheduledAt == now-100 && !r.LastError.Valid:
				untouched++
			default:
				t.Fatalf("%s: %+v", id, r)
			}
		}
		if postponed != 100 || untouched != 50 {
			t.Fatalf("postponed %d, untouched %d", postponed, untouched)
		}
		// The next run moves the remaining e-mail rows out of the way and
		// sends the Web Push rows behind them.
		d.RunOnce(ctx)
		if email.count("") != 1 || web.count("bob-id") != 5 {
			t.Fatalf("second run: e-mail %d, web push %d", email.count(""), web.count("bob-id"))
		}
		// After the pause everything is sent.
		limited = false
		f.now = f.now.Add(61 * time.Second)
		for range 3 {
			d.RunOnce(ctx)
		}
		if email.count("") != 151 || len(f.pending()) != 0 {
			t.Fatalf("after the pause: %d sends, %d pending", email.count(""), len(f.pending()))
		}
	})
}

func TestDispatcherRateLimitSlackPerUser(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		f := newFixture(t, driver, "dave", "erin")
		now := f.now.Unix()
		for _, u := range []string{"dave", "erin"} {
			f.routes(u, nil, []string{store.NotificationStale}, nil)
			f.webhook(u, "sealed-"+u)
		}
		f.enqueue("dave", staleNotification("d1", now), staleNotification("d2", now), staleNotification("d3", now))
		f.enqueue("erin", staleNotification("e1", now), staleNotification("e2", now))
		slack := &recorder{errFor: func(userID string, _ Message) error {
			if userID == "dave-id" {
				return fmt.Errorf("slack webhook status 429: %w", ErrRateLimited)
			}
			return nil
		}}
		d := &Dispatcher{Store: f.s, Channels: map[string]Channel{store.ChannelSlack: slack}, Now: f.clock}
		d.RunOnce(context.Background())
		if slack.count("dave-id") != 1 || slack.count("erin-id") != 2 {
			t.Fatalf("sends: dave %d, erin %d", slack.count("dave-id"), slack.count("erin-id"))
		}
		for id, r := range f.rows("dave-id") {
			if r.Attempts != 0 || r.SentAt.Valid || r.ScheduledAt != now+60 || r.LastError.String != "rate_limited" {
				t.Fatalf("%s: %+v", id, r)
			}
		}
	})
}

func TestDispatcherRateLimitGivesUp(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		f := newFixture(t, driver, "alice")
		now := f.now.Unix()
		f.routes("alice", nil, nil, []string{store.NotificationStale})
		old1, old2 := staleNotification("o1", now-86401), staleNotification("o2", now-86401)
		young := staleNotification("y1", now-10)
		young.ScheduledAt = now
		f.enqueue("alice", old1, old2, young)
		email := &recorder{errFor: func(string, Message) error { return ErrRateLimited }}
		d := &Dispatcher{Store: f.s, Channels: map[string]Channel{store.ChannelEmail: email}, Now: f.clock}
		d.RunOnce(context.Background())
		if email.count("") != 1 {
			t.Fatalf("sends: %d", email.count(""))
		}
		finished, postponed := 0, 0
		for id, r := range f.rows("alice-id") {
			switch {
			case r.SentAt.Valid && r.LastError.String == "rate_limited" && r.Attempts == 0:
				finished++
			case !r.SentAt.Valid && r.ScheduledAt == now+60 && r.LastError.String == "rate_limited" && r.Attempts == 0:
				postponed++
			default:
				t.Fatalf("%s: %+v", id, r)
			}
		}
		if finished != 2 || postponed != 1 {
			t.Fatalf("finished %d, postponed %d", finished, postponed)
		}
	})
}

func TestDispatcherLog(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		f := newFixture(t, driver, "alice")
		f.subscribe("alice", "s", "https://push.example/a")
		f.routes("alice", store.RoutableKinds, store.RoutableKinds, nil)
		f.webhook("alice", "sealed")
		f.enqueue("alice", staleNotification("n1", f.now.Unix()))
		web := &recorder{errFor: func(string, Message) error {
			return errors.New(`Post "https://push.example/secret-endpoint": connection refused`)
		}}
		slack := &recorder{errFor: func(string, Message) error { return errors.New("slack webhook status 500") }}
		d := &Dispatcher{Store: f.s, Channels: map[string]Channel{store.ChannelWebPush: web, store.ChannelSlack: slack}, Now: f.clock}
		var buf bytes.Buffer
		log.SetOutput(&buf)
		defer log.SetOutput(os.Stderr)
		d.RunOnce(context.Background())
		out := buf.String()
		if strings.Contains(out, "secret-endpoint") || !strings.Contains(out, "user=alice-id channel=webpush: send_failed\n") || !strings.Contains(out, "user=alice-id channel=slack: send_failed: slack webhook status 500") {
			t.Fatalf("log: %s", out)
		}
	})
}

// slackServer answers webhook requests with the status of the path's last
// element and records the paths and bodies.
type slackServer struct {
	mu     sync.Mutex
	paths  []string
	bodies [][]byte
}

func (s *slackServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	s.paths = append(s.paths, r.URL.Path)
	s.bodies = append(s.bodies, body)
	s.mu.Unlock()
	switch {
	case strings.HasSuffix(r.URL.Path, "/redirect"):
		http.Redirect(w, r, "/target", http.StatusFound)
	case strings.HasSuffix(r.URL.Path, "/ok"), r.URL.Path == "/target":
		_, _ = io.WriteString(w, "ok")
	default:
		var code int
		_, _ = fmt.Sscanf(r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:], "%d", &code)
		w.WriteHeader(code)
		_, _ = io.WriteString(w, "no_service")
	}
}

func (s *slackServer) requested(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, p := range s.paths {
		if p == path {
			n++
		}
	}
	return n
}

func TestSlackSend(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		f := newFixture(t, driver, "alice")
		ctx := context.Background()
		ss := &slackServer{}
		srv := httptest.NewServer(ss)
		t.Cleanup(srv.Close)
		ch := &Slack{Store: f.s, Box: f.box, PublicURL: "https://astovia.test", Client: newPushClient(func(netip.Addr) bool { return true }, nil)}
		register := func(path string) string {
			t.Helper()
			webhook := srv.URL + "/services/T1/B1/" + path
			sealed, err := f.box.Seal(SlackWebhookPurpose("alice-id"), webhook)
			if err != nil {
				t.Fatal(err)
			}
			f.webhook("alice", sealed)
			return webhook
		}
		lastError := func() sql.NullString {
			t.Helper()
			c, err := f.s.NotificationChannels(ctx, "alice-id")
			if err != nil {
				t.Fatal(err)
			}
			return c.SlackLastError
		}
		m := Message{ID: "n1", Kind: store.NotificationStale, Title: "a<b>&c", Body: "x<y", URL: "/tasks/1"}

		if _, err := ch.Send(ctx, "alice-id", m); !errors.Is(err, store.ErrNoSubscription) {
			t.Fatalf("no webhook: %v", err)
		}
		register("ok")
		if sent, err := ch.Send(ctx, "alice-id", m); err != nil || sent != 1 {
			t.Fatalf("ok: %d %v", sent, err)
		}
		var body map[string]string
		if err := json.Unmarshal(ss.bodies[len(ss.bodies)-1], &body); err != nil || body["text"] != "*a&lt;b&gt;&amp;c*\nx&lt;y\n<https://astovia.test/tasks/1|Astovia で開く>" {
			t.Fatalf("body: %v %v", body, err)
		}

		// 403, 404 and 410 mark the webhook invalid; Send then reports no
		// destination until it is registered again.
		for _, code := range []string{"403", "404", "410"} {
			webhook := register(code)
			_, err := ch.Send(ctx, "alice-id", m)
			if !errors.Is(err, ErrDestinationInvalid) || strings.Contains(err.Error(), webhook) || strings.Contains(err.Error(), "B1") {
				t.Fatalf("%s: %v", code, err)
			}
			if got := lastError(); got.String != SlackWebhookInvalid {
				t.Fatalf("%s last error: %+v", code, got)
			}
			if _, err = ch.Send(ctx, "alice-id", m); !errors.Is(err, store.ErrNoSubscription) {
				t.Fatalf("%s after invalid: %v", code, err)
			}
		}
		// 429 is a rate limit, 500 and 400 are retried; none is recorded.
		for _, code := range []string{"429", "500", "400"} {
			webhook := register(code)
			_, err := ch.Send(ctx, "alice-id", m)
			if err == nil || errors.Is(err, ErrDestinationInvalid) || errors.Is(err, ErrRateLimited) != (code == "429") || strings.Contains(err.Error(), webhook) {
				t.Fatalf("%s: %v", code, err)
			}
			if got := lastError(); got.Valid {
				t.Fatalf("%s recorded: %+v", code, got)
			}
		}
		// A redirect is not followed.
		register("redirect")
		if _, err := ch.Send(ctx, "alice-id", m); err == nil || errors.Is(err, ErrDestinationInvalid) || ss.requested("/target") != 0 {
			t.Fatalf("redirect: %v %d", err, ss.requested("/target"))
		}
		// A connection error does not contain the URL.
		closed := httptest.NewServer(ss)
		closedURL := closed.URL + "/services/T1/B1/ok"
		closed.Close()
		sealed, err := f.box.Seal(SlackWebhookPurpose("alice-id"), closedURL)
		if err != nil {
			t.Fatal(err)
		}
		f.webhook("alice", sealed)
		if _, err = ch.Send(ctx, "alice-id", m); err == nil || strings.Contains(err.Error(), closedURL) || strings.Contains(err.Error(), "services") {
			t.Fatalf("connection error: %v", err)
		}
		// A webhook sealed for another user cannot be opened: it is marked
		// and not retried.
		sealed, err = f.box.Seal(SlackWebhookPurpose("bob-id"), srv.URL+"/services/T1/B1/ok")
		if err != nil {
			t.Fatal(err)
		}
		f.webhook("alice", sealed)
		before := len(ss.paths)
		if _, err = ch.Send(ctx, "alice-id", m); !errors.Is(err, ErrDestinationInvalid) || len(ss.paths) != before {
			t.Fatalf("undecryptable: %v", err)
		}
		if got := lastError(); got.String != SlackWebhookUndecryptable {
			t.Fatalf("undecryptable last error: %+v", got)
		}
	})
}

// TestSlackUndecryptableNotRetried checks the dispatcher's handling of a
// webhook that cannot be decrypted.
func TestSlackUndecryptableNotRetried(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		f := newFixture(t, driver, "alice")
		f.routes("alice", nil, store.RoutableKinds, nil)
		f.webhook("alice", "not-sealed")
		f.enqueue("alice", staleNotification("n1", f.now.Unix()))
		ch := &Slack{Store: f.s, Box: f.box, PublicURL: "https://astovia.test", Client: newPushClient(func(netip.Addr) bool { return false }, nil)}
		d := &Dispatcher{Store: f.s, Channels: map[string]Channel{store.ChannelSlack: ch}, Now: f.clock}
		d.RunOnce(context.Background())
		rows := f.rows("alice-id")
		if len(rows) != 1 {
			t.Fatalf("rows: %+v", rows)
		}
		for _, r := range rows {
			if r.Attempts != 0 || !r.SentAt.Valid || r.LastError.String != "destination_invalid" {
				t.Fatalf("row: %+v", r)
			}
		}
		c, err := f.s.NotificationChannels(context.Background(), "alice-id")
		if err != nil || c.SlackLastError.String != SlackWebhookUndecryptable {
			t.Fatalf("channels: %+v %v", c, err)
		}
	})
}

func TestSlackText(t *testing.T) {
	got := slackText(Message{Title: strings.Repeat("あ", 200), Body: strings.Repeat("い", 1200) + "<", URL: "https://evil.example/"}, "https://astovia.test/")
	want := "*" + strings.Repeat("あ", 150) + "*\n" + strings.Repeat("い", 1000) + "\n<https://astovia.test/|Astovia で開く>"
	if got != want {
		t.Fatalf("text: %q", got)
	}
	if got = slackText(Message{Title: "t", Body: "b", URL: "/x|y>z&"}, "https://astovia.test"); got != "*t*\nb\n<https://astovia.test/x%7Cy&gt;z&amp;|Astovia で開く>" {
		t.Fatalf("link: %q", got)
	}
}

type fakeSender struct {
	messages []mail.Message
	err      error
}

func (s *fakeSender) Provider() string { return "fake" }
func (s *fakeSender) Send(_ context.Context, m mail.Message) error {
	s.messages = append(s.messages, m)
	return s.err
}

func TestEmailSend(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		f := newFixture(t, driver, "alice", "bob")
		ctx := context.Background()
		m := Message{ID: "n-1", Kind: store.NotificationStale, Title: "a\x00b\nc", Body: "line1\r\nline2\x07", URL: "/tasks/x"}
		if _, err := (&Email{Store: f.s, PublicURL: "https://astovia.test"}).Send(ctx, "alice-id", m); !errors.Is(err, store.ErrNoSubscription) {
			t.Fatalf("no sender: %v", err)
		}
		sender := &fakeSender{}
		ch := &Email{Store: f.s, Sender: sender, PublicURL: "https://astovia.test/"}
		if sent, err := ch.Send(ctx, "alice-id", m); err != nil || sent != 1 {
			t.Fatalf("send: %d %v", sent, err)
		}
		want := mail.Message{To: "alice@example.com", Subject: "abc", Text: "line1\nline2\n\nhttps://astovia.test/tasks/x\n\n--\nこの通知は Astovia から送っています。送り先は https://astovia.test/settings/notifications で変えられます。\n", IdempotencyKey: "n-1"}
		if len(sender.messages) != 1 || sender.messages[0] != want {
			t.Fatalf("message: %+v", sender.messages)
		}
		sender.err = fmt.Errorf("%w: resend status 429 daily_quota_exceeded", mail.ErrRateLimited)
		if _, err := ch.Send(ctx, "alice-id", m); !errors.Is(err, ErrRateLimited) || !errors.Is(err, mail.ErrRateLimited) {
			t.Fatalf("rate limited: %v", err)
		}
		sender.err = fmt.Errorf("%w: resend status 403", mail.ErrRejected)
		if _, err := ch.Send(ctx, "alice-id", m); err == nil || errors.Is(err, ErrRateLimited) {
			t.Fatalf("rejected: %v", err)
		}
		if err := f.s.ChangeUser(ctx, "bob@example.com", "", true, f.now.Unix()); err != nil {
			t.Fatal(err)
		}
		if _, err := ch.Send(ctx, "bob-id", m); !errors.Is(err, store.ErrNoSubscription) {
			t.Fatalf("disabled user: %v", err)
		}
	})
}

func TestMailSubject(t *testing.T) {
	for _, tc := range []struct{ title, want string }{
		{"件名", "件名"},
		{"a\tb\u0085c", "abc"},
		{strings.Repeat("あ", 200), strings.Repeat("あ", 150)},
		{"", "Astovia の通知"},
		{" \n ", "Astovia の通知"},
	} {
		if got := mailSubject(Message{Title: tc.title}); got != tc.want {
			t.Fatalf("%q: %q", tc.title, got)
		}
	}
}

// TestEngineSlackOnly checks that a user without a push subscription who
// sends deadlines to Slack gets the deadline notifications.
func TestEngineSlackOnly(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		f := newFixture(t, driver, "alice")
		f.routes("alice", store.RoutableKinds, []string{store.NotificationDueSoon, store.NotificationOverdue}, nil)
		f.webhook("alice", "sealed")
		t0 := f.now.Unix()
		f.dueItem("alice", "late", "open", store.KindTask, t0-7*86400, sql.NullInt64{Int64: t0 - 2*86400, Valid: true})
		(&Engine{Store: f.s, Now: f.clock}).RunOnce(context.Background())
		list := f.pending()
		if len(list) != 1 || list[0].Kind != store.NotificationOverdue || list[0].Channel != store.ChannelSlack {
			t.Fatalf("pending: %+v", list)
		}
	})
}
