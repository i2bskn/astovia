package notify

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/i2bskn/astovia/internal/secretbox"
	"github.com/i2bskn/astovia/internal/store"
	"github.com/i2bskn/astovia/internal/testutil"
)

type fixture struct {
	t   *testing.T
	s   *store.Store
	now time.Time
	box *secretbox.Box
}

func newFixture(t *testing.T, driver string, users ...string) *fixture {
	t.Helper()
	s, _ := testutil.Database(t, driver)
	f := &fixture{t: t, s: s, now: time.Date(2026, 10, 24, 0, 30, 0, 0, time.UTC), box: secretbox.New([]byte(strings.Repeat("s", 32)))}
	for _, name := range users {
		u := store.User{ID: name + "-id", Email: name + "@example.com", PasswordHash: "x"}
		if err := s.CreateUser(context.Background(), u); err != nil {
			t.Fatal(err)
		}
		if err := s.Login(context.Background(), u, store.Session{ID: name + "-session", UserID: u.ID, ExpiresAt: f.now.Unix() + 86400*60, CreatedAt: f.now.Unix()}, store.Refresh{Hash: "r-" + name, SessionID: name + "-session", CreatedAt: f.now.Unix()}); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func (f *fixture) clock() time.Time { return f.now }

// subscribe adds a subscription with real P-256 keys, so that the library can
// encrypt for it.
func (f *fixture) subscribe(user, id, endpoint string) {
	f.t.Helper()
	key, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		f.t.Fatal(err)
	}
	secret := make([]byte, 16)
	if _, err = rand.Read(secret); err != nil {
		f.t.Fatal(err)
	}
	p := store.PushSubscription{ID: id, UserID: user + "-id", Endpoint: endpoint, P256dh: base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes()), Auth: base64.RawURLEncoding.EncodeToString(secret), Label: id}
	if _, _, err = f.s.SavePushSubscription(context.Background(), p, "", user+"-session", f.now.Unix()); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) item(user, id, status string, created int64) store.ActionItem {
	f.t.Helper()
	item := store.ActionItem{ID: id, UserID: user + "-id", Content: "content " + id, Status: status, Refs: "[]", Version: 1, CreatedAt: created, UpdatedAt: created}
	if err := f.s.CreateActionItem(context.Background(), item); err != nil {
		f.t.Fatal(err)
	}
	return item
}

func (f *fixture) pending() []store.Notification {
	f.t.Helper()
	list, err := f.s.DueNotifications(context.Background(), f.now.Unix()+86400*365, 100)
	if err != nil {
		f.t.Fatal(err)
	}
	return list
}

func drivers(t *testing.T, fn func(t *testing.T, driver string)) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) { fn(t, driver) })
	}
}

func TestEngineStale(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		f := newFixture(t, driver, "alice", "bob")
		e := &Engine{Store: f.s, Now: f.clock}
		f.subscribe("alice", "a1", "https://push.example/a1")
		day := int64(86400)
		now := f.now.Unix()
		f.item("alice", "fresh", "open", now-3*day+1)
		f.item("alice", "old", "open", now-3*day)
		f.item("alice", "older", "open", now-7*day)
		f.item("alice", "closed", "done", now-9*day)
		f.item("bob", "bobs", "open", now-9*day) // bob has no subscription
		e.RunOnce(context.Background())
		e.RunOnce(context.Background())
		got := map[string]store.Notification{}
		for _, n := range f.pending() {
			got[n.DedupeKey] = n
		}
		old, older := got["stale:old:1"], got["stale:older:2"]
		if len(got) != 2 || old.Title != "未対応が3日続いています" || old.Body != "content old" || old.ActionItemID.String != "old" || old.URL != "/" || older.Title != "未対応が7日続いています" || old.Kind != store.NotificationStale {
			t.Fatalf("stale notifications: %+v", got)
		}
		// With a shorter period, the same items reach a new period.
		if err := f.s.SaveNotificationSettings(context.Background(), store.NotificationSettings{UserID: "alice-id", Timezone: "UTC", DigestHour: 23, StaleDays: 1, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
		e.RunOnce(context.Background())
		got = map[string]store.Notification{}
		for _, n := range f.pending() {
			got[n.DedupeKey] = n
		}
		if _, ok := got["stale:old:3"]; !ok || len(got) != 5 {
			t.Fatalf("one-day period: %v", got)
		}
	})
}

func TestEngineDigest(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		f := newFixture(t, driver, "tokyo", "utc", "empty")
		e := &Engine{Store: f.s, Now: f.clock}
		ctx := context.Background()
		for _, u := range []string{"tokyo", "utc", "empty"} {
			f.subscribe(u, u+"-sub", "https://push.example/"+u)
		}
		// 00:30 UTC is 09:30 in Tokyo.
		if err := f.s.SaveNotificationSettings(ctx, store.NotificationSettings{UserID: "tokyo-id", Timezone: "Asia/Tokyo", DigestHour: 9, StaleDays: 30, UpdatedAt: 1}); err != nil {
			t.Fatal(err)
		}
		if err := f.s.SaveNotificationSettings(ctx, store.NotificationSettings{UserID: "empty-id", Timezone: "UTC", DigestHour: 0, StaleDays: 30, UpdatedAt: 1}); err != nil {
			t.Fatal(err)
		}
		now := f.now.Unix()
		for i, id := range []string{"t1", "t2", "t3", "t4"} {
			f.item("tokyo", id, "open", now-int64(100-i))
		}
		f.item("utc", "u1", "open", now-100) // UTC user with the default 08:00
		e.RunOnce(ctx)
		e.RunOnce(ctx)
		list := f.pending()
		if len(list) != 1 {
			t.Fatalf("digests: %+v", list)
		}
		d := list[0]
		if d.UserID != "tokyo-id" || d.DedupeKey != "digest:2026-10-24" || d.Title != "今日の未完了 4件" || d.Body != "content t4・content t3・content t2" || d.URL != "/" || d.ActionItemID.Valid {
			t.Fatalf("digest: %+v", d)
		}

		// The next day, automatically completed items are counted since the
		// previous digest and lead to the automatic list.
		f.now = f.now.Add(24 * time.Hour)
		autoAt := f.now.Unix() - 3600
		c, err := f.s.SaveGitHubConnection(ctx, store.GitHubConnection{UserID: "tokyo-id", TokenSealed: "x", TokenLast4: "x", Login: "x", Scopes: "repo"}, nil, "tokyo-session", f.now.Unix())
		if err != nil {
			t.Fatal(err)
		}
		err = f.s.GitHubSync(ctx, "tokyo-id", c.Generation, func(tx *store.GitHubTx) error {
			for _, id := range []string{"t1", "t2", "t3", "t4"} {
				cur, _, e := tx.LockItem(id)
				if e != nil {
					return e
				}
				next := cur
				next.Status, next.AutoReason, next.AutoAt = "done", sql.NullString{String: "github.pr_merged", Valid: true}, sql.NullInt64{Int64: autoAt, Valid: true}
				if _, _, e = tx.UpdateItem(cur, next); e != nil {
					return e
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		e.RunOnce(ctx)
		var next *store.Notification
		for _, n := range f.pending() {
			if n.DedupeKey == "digest:2026-10-25" {
				next = &n
			}
		}
		if next == nil || next.Title != "今日の未完了 0件" || next.Body != "自動で処理 4件／" || next.URL != "/auto" {
			t.Fatalf("second digest: %+v", next)
		}

		// Outside the hour nothing is created; the UTC user gets it at 08:00.
		f.now = time.Date(2026, 10, 25, 8, 59, 0, 0, time.UTC)
		e.RunOnce(ctx)
		found := false
		for _, n := range f.pending() {
			if n.UserID == "utc-id" && n.DedupeKey == "digest:2026-10-25" {
				found = true
			}
			if n.UserID == "empty-id" {
				t.Fatalf("digest without items: %+v", n)
			}
		}
		if !found {
			t.Fatal("UTC digest missing")
		}
	})
}

func TestDigestSkippedHour(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		f := newFixture(t, driver, "ny")
		e := &Engine{Store: f.s, Now: f.clock}
		f.subscribe("ny", "s", "https://push.example/ny")
		if err := f.s.SaveNotificationSettings(context.Background(), store.NotificationSettings{UserID: "ny-id", Timezone: "America/New_York", DigestHour: 2, StaleDays: 30, UpdatedAt: 1}); err != nil {
			t.Fatal(err)
		}
		f.item("ny", "i", "open", time.Date(2026, 3, 8, 0, 0, 0, 0, time.UTC).Unix())
		// 2026-03-08 02:00-03:00 does not exist in New York.
		for m := 0; m < 120; m += 10 {
			f.now = time.Date(2026, 3, 8, 6, 30, 0, 0, time.UTC).Add(time.Duration(m) * time.Minute)
			e.RunOnce(context.Background())
		}
		if list := f.pending(); len(list) != 0 {
			t.Fatalf("digest on a skipped hour: %+v", list)
		}
	})
}

type fakeChannel struct {
	mu       sync.Mutex
	messages []Message
	err      error
}

func (c *fakeChannel) Name() string { return "fake" }
func (c *fakeChannel) Send(_ context.Context, _ string, m Message) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.messages = append(c.messages, m)
	if c.err != nil {
		return 0, c.err
	}
	return 1, nil
}

func TestDispatcher(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		f := newFixture(t, driver, "alice")
		ctx := context.Background()
		f.subscribe("alice", "s", "https://push.example/a")
		ch := &fakeChannel{}
		d := &Dispatcher{Store: f.s, Channel: ch, Now: f.clock}
		now := f.now.Unix()
		f.item("alice", "open", "open", now)
		f.item("alice", "done", "done", now)
		list := []store.Notification{
			{ID: "n1", Kind: store.NotificationDetected, ActionItemID: sql.NullString{String: "open", Valid: true}, DedupeKey: "detected:open:1", Title: "レビュー依頼: o/r#1", Body: "b", URL: "/", ScheduledAt: now, CreatedAt: now},
			{ID: "n2", Kind: store.NotificationStale, ActionItemID: sql.NullString{String: "done", Valid: true}, DedupeKey: "stale:done:1", Title: "t", Body: "b", URL: "/", ScheduledAt: now, CreatedAt: now},
			{ID: "n3", Kind: store.NotificationDigest, DedupeKey: "digest:x", Title: "t", Body: "b", URL: "/auto", ScheduledAt: now, CreatedAt: now},
			{ID: "n4", Kind: store.NotificationStale, ActionItemID: sql.NullString{String: "open", Valid: true}, DedupeKey: "stale:open:1", Title: "t", Body: "b", URL: "/", ScheduledAt: now + 60, CreatedAt: now},
		}
		if err := f.s.EnqueueNotifications(ctx, "alice-id", list); err != nil {
			t.Fatal(err)
		}
		d.RunOnce(ctx)
		tags := map[string]bool{}
		for _, m := range ch.messages {
			tags[m.Tag] = true
		}
		// The completed item's notification is skipped; n4 is not due yet.
		if len(ch.messages) != 2 || !tags["detected-open"] || !tags["digest"] || ch.messages[0].Kind == "" {
			t.Fatalf("sent: %+v", ch.messages)
		}
		if p := f.pending(); len(p) != 1 || p[0].ID != "n4" {
			t.Fatalf("pending: %+v", p)
		}

		// Failures are retried after 60, 300 and 1800 seconds, then given up.
		ch.err = errors.New("push service status 500")
		f.now = f.now.Add(time.Minute)
		for i, wait := range []int64{60, 300, 1800} {
			d.RunOnce(ctx)
			p := f.pending()
			if len(p) != 1 || p[0].Attempts != int64(i+1) || p[0].LastError.String != "send_failed" || p[0].ScheduledAt != f.now.Unix()+wait {
				t.Fatalf("attempt %d: %+v", i+1, p)
			}
			d.RunOnce(ctx) // not due yet
			f.now = time.Unix(p[0].ScheduledAt, 0)
		}
		d.RunOnce(ctx)
		if p := f.pending(); len(p) != 0 {
			t.Fatalf("not given up: %+v", p)
		}
		if len(ch.messages) != 6 || ch.messages[2].Tag != "stale-open" {
			t.Fatalf("sends: %d %+v", len(ch.messages), ch.messages)
		}

		// No subscription ends the notification.
		ch.err = store.ErrNoSubscription
		test := store.Notification{ID: "n5", Kind: store.NotificationTest, DedupeKey: "test:1", Title: "t", Body: "b", URL: "/settings/notifications", ScheduledAt: f.now.Unix(), CreatedAt: f.now.Unix()}
		if err := f.s.EnqueueNotifications(ctx, "alice-id", []store.Notification{test}); err != nil {
			t.Fatal(err)
		}
		d.RunOnce(ctx)
		if p := f.pending(); len(p) != 0 || ch.messages[len(ch.messages)-1].Tag != "test" {
			t.Fatalf("no subscription: %+v", p)
		}

		// Sent notifications older than 30 days are cleaned up.
		f.now = f.now.Add(31 * 24 * time.Hour)
		d.Cleanup(ctx)
		if has, err := f.s.HasNotification(ctx, "alice-id", "test:1"); err != nil || has {
			t.Fatalf("cleanup: %v %v", has, err)
		}
	})
}

// pushServer records Web Push requests and answers with status.
type pushServer struct {
	mu       sync.Mutex
	requests []*http.Request
	bodies   [][]byte
	status   map[string]int
}

func (p *pushServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	p.mu.Lock()
	p.requests = append(p.requests, r)
	p.bodies = append(p.bodies, body)
	code := p.status[r.URL.Path]
	p.mu.Unlock()
	if code == 0 {
		code = 201
	}
	w.WriteHeader(code)
}

func TestWebPushSend(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		f := newFixture(t, driver, "alice", "bob")
		ctx := context.Background()
		ps := &pushServer{status: map[string]int{"/gone": 410, "/missing": 404, "/forbidden": 403, "/error": 500}}
		srv := httptest.NewTLSServer(ps)
		t.Cleanup(srv.Close)
		vapid := &VAPID{Store: f.s, Box: f.box, Now: f.clock}
		w := NewWebPush(f.s, vapid, "https://astovia.test", f.clock)
		w.Client = newPushClient(func(netip.Addr) bool { return true }, srv.Client().Transport.(*http.Transport).TLSClientConfig)

		for _, path := range []string{"/ok", "/gone", "/missing", "/forbidden", "/error"} {
			f.subscribe("alice", "s"+strings.TrimPrefix(path, "/"), srv.URL+path)
		}
		sent, err := w.Send(ctx, "alice-id", Message{Kind: store.NotificationDetected, Title: strings.Repeat("あ", 100), Body: strings.Repeat("い", 300), URL: "https://evil.example/", Tag: "detected-x"})
		if err != nil || sent != 1 {
			t.Fatalf("send: %d %v", sent, err)
		}
		keys, err := vapid.Keys(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var ok *http.Request
		for _, r := range ps.requests {
			if r.URL.Path == "/ok" {
				ok = r
			}
		}
		if ok == nil || !strings.HasPrefix(ok.Header.Get("Authorization"), "vapid t=") || !strings.HasSuffix(ok.Header.Get("Authorization"), ", k="+keys.Public) || ok.Header.Get("Content-Encoding") != "aes128gcm" || ok.Header.Get("TTL") != "3600" || ok.Header.Get("Urgency") != "high" {
			t.Fatalf("push request: %+v", ok)
		}
		for _, b := range ps.bodies {
			// The payload is encrypted: the title is not readable.
			if strings.Contains(string(b), "あ") || strings.Contains(string(b), "title") {
				t.Fatal("payload sent in clear")
			}
		}
		// 404, 410 and 403 remove the subscription; 500 keeps it.
		subs, err := f.s.PushSubscriptions(ctx, "alice-id")
		if err != nil || len(subs) != 2 {
			t.Fatalf("subscriptions: %+v %v", subs, err)
		}
		for _, s := range subs {
			if s.ID == "sok" && s.LastUsedAt.Int64 != f.now.Unix() {
				t.Fatalf("last used: %+v", s)
			}
		}

		// Only failures: an error; everything gone: no subscription.
		f.subscribe("bob", "berror", srv.URL+"/error")
		if _, err = w.Send(ctx, "bob-id", Message{Kind: store.NotificationStale, Title: "t"}); err == nil || errors.Is(err, store.ErrNoSubscription) {
			t.Fatalf("failure: %v", err)
		}
		if last := ps.requests[len(ps.requests)-1]; last.Header.Get("TTL") != "21600" || last.Header.Get("Urgency") != "normal" {
			t.Fatalf("stale headers: %v", last.Header)
		}
		ps.mu.Lock()
		ps.status["/error"] = 410
		ps.mu.Unlock()
		if _, err = w.Send(ctx, "bob-id", Message{Kind: store.NotificationDigest, Title: "t"}); !errors.Is(err, store.ErrNoSubscription) {
			t.Fatalf("all gone: %v", err)
		}
		if _, err = w.Send(ctx, "bob-id", Message{Kind: store.NotificationDigest, Title: "t"}); !errors.Is(err, store.ErrNoSubscription) {
			t.Fatalf("no subscriptions: %v", err)
		}
	})
}

func TestWebPushPayload(t *testing.T) {
	// The payload is checked before encryption through the library's
	// plaintext: decode what the channel marshals.
	b, err := json.Marshal(pushPayload{Title: truncate(strings.Repeat("あ", 100), 80), Body: truncate(strings.Repeat("い", 300), 200), URL: "/", Tag: "t"})
	if err != nil {
		t.Fatal(err)
	}
	var p pushPayload
	if err = json.Unmarshal(b, &p); err != nil || len([]rune(p.Title)) != 80 || len([]rune(p.Body)) != 200 {
		t.Fatalf("payload: %+v %v", p, err)
	}
	for kind, want := range map[string]int{store.NotificationDetected: 3600, store.NotificationTest: 3600, store.NotificationStale: 21600, store.NotificationDigest: 43200} {
		if ttl(kind) != want {
			t.Fatalf("%s TTL %d", kind, ttl(kind))
		}
	}
}

// Notification URLs saved before the web UI moved from /admin to / are
// rewritten, and anything that is not a same-origin path opens /.
func TestClickURL(t *testing.T) {
	for in, want := range map[string]string{
		"/":                             "/",
		"/auto":                         "/auto",
		"/settings/notifications":       "/settings/notifications",
		"/admin":                        "/",
		"/admin/":                       "/",
		"/admin/auto":                   "/auto",
		"/admin/settings/notifications": "/settings/notifications",
		"/a%09b":                        "/a%09b",
		"/admin//x":                     "/",
		"/admin/\\x":                    "/",
		"//x":                           "/",
		"/\\x":                          "/",
		"https://x":                     "/",
		"":                              "/",
		"auto":                          "/",
		"/a\tb":                         "/",
		"/a\nb":                         "/",
		"/a\x7fb":                       "/",
		"/admin/a\x00b":                 "/",
	} {
		if got := clickURL(in); got != want {
			t.Errorf("clickURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// The default client refuses endpoints on private addresses: the allow
// function is not replaced here.
func TestWebPushRejectsPrivateAddress(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		f := newFixture(t, driver, "alice")
		ctx := context.Background()
		ps := &pushServer{status: map[string]int{}}
		srv := httptest.NewTLSServer(ps)
		t.Cleanup(srv.Close)
		w := NewWebPush(f.s, &VAPID{Store: f.s, Box: f.box, Now: f.clock}, "http://localhost:8080", f.clock)
		w.Client.Transport.(*http.Transport).TLSClientConfig = srv.Client().Transport.(*http.Transport).TLSClientConfig
		f.subscribe("alice", "s", srv.URL+"/x")
		if _, err := w.Send(ctx, "alice-id", Message{Kind: store.NotificationTest, Title: "t"}); err == nil || errors.Is(err, store.ErrNoSubscription) {
			t.Fatalf("private address: %v", err)
		}
		if len(ps.requests) != 0 {
			t.Fatal("request reached a loopback endpoint")
		}
		if w.Subscriber != "webpush@localhost" {
			t.Fatalf("subscriber: %q", w.Subscriber)
		}
	})
}

func TestVAPIDKeys(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		f := newFixture(t, driver, "alice")
		ctx := context.Background()
		v := &VAPID{Store: f.s, Box: f.box, Now: f.clock}
		keys, err := v.Keys(ctx)
		if err != nil {
			t.Fatal(err)
		}
		pub, err := base64.RawURLEncoding.DecodeString(keys.Public)
		if err != nil || len(pub) != 65 || pub[0] != 4 {
			t.Fatalf("public key: %q %v", keys.Public, err)
		}
		sec, err := f.s.InstanceSecret(ctx, "vapid")
		if err != nil || strings.Contains(sec.Value, keys.Private) {
			t.Fatalf("private key stored in clear: %v", err)
		}
		if again, err := v.Keys(ctx); err != nil || again != keys {
			t.Fatalf("keys changed: %v", err)
		}
		// Another server secret cannot decrypt the key: a new pair replaces
		// it and every subscription is removed.
		f.subscribe("alice", "s", "https://push.example/a")
		other := &VAPID{Store: f.s, Box: secretbox.New([]byte(strings.Repeat("o", 32))), Now: f.clock}
		renewed, err := other.Keys(ctx)
		if err != nil || renewed.Public == keys.Public {
			t.Fatalf("renewed: %v", err)
		}
		if subs, err := f.s.PushSubscriptions(ctx, "alice-id"); err != nil || len(subs) != 0 {
			t.Fatalf("subscriptions after renewal: %d %v", len(subs), err)
		}
		if again, err := other.Keys(ctx); err != nil || again != renewed {
			t.Fatalf("renewed keys changed: %v", err)
		}
	})
}
