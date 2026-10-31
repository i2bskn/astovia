package store_test

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"sort"
	"testing"

	"github.com/i2bskn/astovia/internal/store"
	"github.com/i2bskn/astovia/internal/testutil"
)

func drivers(t *testing.T, fn func(t *testing.T, driver string)) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) { fn(t, driver) })
	}
}

func subscribe(t *testing.T, s *store.Store, user string) {
	t.Helper()
	if _, _, err := s.SavePushSubscription(context.Background(), store.PushSubscription{ID: user + "-sub", UserID: user + "-id", Endpoint: "https://push.example/" + user, P256dh: "k", Auth: "a"}, "", user+"-session", tokenTestNow); err != nil {
		t.Fatal(err)
	}
}

// channelsOf returns the channels of the user's pending rows with the dedupe
// key, sorted.
func channelsOf(t *testing.T, s *store.Store, userID, key string) []string {
	t.Helper()
	due, err := s.DueNotifications(context.Background(), tokenTestNow*2, 1000)
	if err != nil {
		t.Fatal(err)
	}
	out := []string{}
	ids := map[string]bool{}
	for _, n := range due {
		if n.UserID == userID && n.DedupeKey == key {
			out = append(out, n.Channel)
			if ids[n.ID] {
				t.Fatalf("duplicate ID %s", n.ID)
			}
			ids[n.ID] = true
		}
	}
	sort.Strings(out)
	return out
}

func TestNotificationChannelsDefaults(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		ctx := context.Background()
		s, _ := apiTokenFixture(t, driver)
		c, err := s.NotificationChannels(ctx, "alice-id")
		if err != nil || !reflect.DeepEqual(c.WebPush, store.RoutableKinds) || len(c.Slack) != 0 || len(c.Email) != 0 || c.SlackWebhookSealed.Valid || c.UserID != "alice-id" {
			t.Fatalf("defaults: %+v %v", c, err)
		}
		// Kinds are stored in the order of RoutableKinds.
		if err = s.SaveNotificationRoutes(ctx, "alice-id", "alice-session", []string{store.NotificationDigest, store.NotificationDetected}, nil, []string{store.NotificationOverdue}, tokenTestNow); err != nil {
			t.Fatal(err)
		}
		c, err = s.NotificationChannels(ctx, "alice-id")
		if err != nil || !reflect.DeepEqual(c.WebPush, []string{store.NotificationDetected, store.NotificationDigest}) || len(c.Slack) != 0 || !reflect.DeepEqual(c.Email, []string{store.NotificationOverdue}) || c.UpdatedAt != tokenTestNow {
			t.Fatalf("saved: %+v %v", c, err)
		}
		if err = s.SaveNotificationRoutes(ctx, "alice-id", "bob-session", nil, nil, nil, tokenTestNow); !errors.Is(err, store.ErrUnauthorized) {
			t.Fatalf("other session: %v", err)
		}
	})
}

func TestSlackWebhookStore(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		ctx := context.Background()
		s, _ := apiTokenFixture(t, driver)
		// Clearing without a row succeeds and creates nothing.
		if err := s.ClearSlackWebhook(ctx, "alice-id", "alice-session", tokenTestNow); err != nil {
			t.Fatal(err)
		}
		// A webhook on a new row keeps the default kinds.
		if err := s.SetSlackWebhook(ctx, "alice-id", "alice-session", "sealed-1", tokenTestNow); err != nil {
			t.Fatal(err)
		}
		c, err := s.NotificationChannels(ctx, "alice-id")
		if err != nil || c.SlackWebhookSealed.String != "sealed-1" || !c.SlackReady() || !reflect.DeepEqual(c.WebPush, store.RoutableKinds) || len(c.Slack) != 0 {
			t.Fatalf("set: %+v %v", c, err)
		}
		// Saving the routes keeps the webhook; setting the webhook keeps the
		// routes.
		if err = s.SaveNotificationRoutes(ctx, "alice-id", "alice-session", nil, []string{store.NotificationStale}, nil, tokenTestNow+1); err != nil {
			t.Fatal(err)
		}
		if c, err = s.NotificationChannels(ctx, "alice-id"); err != nil || c.SlackWebhookSealed.String != "sealed-1" || len(c.WebPush) != 0 || !reflect.DeepEqual(c.Slack, []string{store.NotificationStale}) {
			t.Fatalf("routes kept the webhook: %+v %v", c, err)
		}
		// A different seal is not marked; the current one is.
		if ok, err := s.MarkSlackWebhookInvalid(ctx, "alice-id", "sealed-0", "webhook_invalid"); err != nil || ok {
			t.Fatalf("stale mark: %v %v", ok, err)
		}
		if c, _ = s.NotificationChannels(ctx, "alice-id"); c.SlackLastError.Valid {
			t.Fatalf("stale mark recorded: %+v", c)
		}
		if ok, err := s.MarkSlackWebhookInvalid(ctx, "alice-id", "sealed-1", "webhook_invalid"); err != nil || !ok {
			t.Fatalf("mark: %v %v", ok, err)
		}
		if c, _ = s.NotificationChannels(ctx, "alice-id"); c.SlackLastError.String != "webhook_invalid" || c.SlackReady() {
			t.Fatalf("marked: %+v", c)
		}
		// Registering again clears the error and keeps the routes.
		if err = s.SetSlackWebhook(ctx, "alice-id", "alice-session", "sealed-2", tokenTestNow+2); err != nil {
			t.Fatal(err)
		}
		if c, _ = s.NotificationChannels(ctx, "alice-id"); c.SlackLastError.Valid || c.SlackWebhookSealed.String != "sealed-2" || !reflect.DeepEqual(c.Slack, []string{store.NotificationStale}) {
			t.Fatalf("registered again: %+v", c)
		}
		if err = s.ClearSlackWebhook(ctx, "alice-id", "alice-session", tokenTestNow+3); err != nil {
			t.Fatal(err)
		}
		if c, _ = s.NotificationChannels(ctx, "alice-id"); c.SlackWebhookSealed.Valid || !reflect.DeepEqual(c.Slack, []string{store.NotificationStale}) {
			t.Fatalf("cleared: %+v", c)
		}
		if err = s.SetSlackWebhook(ctx, "alice-id", "bob-session", "x", tokenTestNow); !errors.Is(err, store.ErrUnauthorized) {
			t.Fatalf("other session: %v", err)
		}
	})
}

func TestEnqueueNotificationsRouting(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		ctx := context.Background()
		s, _ := apiTokenFixture(t, driver)
		subscribe(t, s, "alice")
		// The defaults with a subscription: Web Push only.
		if err := s.EnqueueNotifications(ctx, "alice-id", []store.Notification{notification("a1", store.NotificationStale, "stale:a:1", "", 10)}); err != nil {
			t.Fatal(err)
		}
		if got := channelsOf(t, s, "alice-id", "stale:a:1"); !reflect.DeepEqual(got, []string{store.ChannelWebPush}) {
			t.Fatalf("defaults: %v", got)
		}
		// Slack without a webhook is skipped; e-mail is queued.
		if err := s.SaveNotificationRoutes(ctx, "alice-id", "alice-session", store.RoutableKinds, store.RoutableKinds, store.RoutableKinds, tokenTestNow); err != nil {
			t.Fatal(err)
		}
		if err := s.EnqueueNotifications(ctx, "alice-id", []store.Notification{notification("a2", store.NotificationStale, "stale:a:2", "", 10)}); err != nil {
			t.Fatal(err)
		}
		if got := channelsOf(t, s, "alice-id", "stale:a:2"); !reflect.DeepEqual(got, []string{store.ChannelEmail, store.ChannelWebPush}) {
			t.Fatalf("no webhook: %v", got)
		}
		// With a webhook every channel gets a row with the same dedupe key,
		// and queueing again adds only the missing channel.
		if err := s.SetSlackWebhook(ctx, "alice-id", "alice-session", "sealed", tokenTestNow); err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{"a2b", "a2c"} {
			if err := s.EnqueueNotifications(ctx, "alice-id", []store.Notification{notification(id, store.NotificationStale, "stale:a:2", "", 10)}); err != nil {
				t.Fatal(err)
			}
		}
		if got := channelsOf(t, s, "alice-id", "stale:a:2"); !reflect.DeepEqual(got, []string{store.ChannelEmail, store.ChannelSlack, store.ChannelWebPush}) {
			t.Fatalf("all channels: %v", got)
		}
		if has, err := s.HasNotification(ctx, "alice-id", "stale:a:2"); err != nil || !has {
			t.Fatalf("has: %v %v", has, err)
		}
		// A webhook with an error gets no row.
		if _, err := s.MarkSlackWebhookInvalid(ctx, "alice-id", "sealed", "webhook_invalid"); err != nil {
			t.Fatal(err)
		}
		if err := s.EnqueueNotifications(ctx, "alice-id", []store.Notification{notification("a3", store.NotificationDigest, "digest:x", "", 10)}); err != nil {
			t.Fatal(err)
		}
		if got := channelsOf(t, s, "alice-id", "digest:x"); !reflect.DeepEqual(got, []string{store.ChannelEmail, store.ChannelWebPush}) {
			t.Fatalf("invalid webhook: %v", got)
		}
		// A kind outside the table goes to Web Push only.
		if err := s.EnqueueNotifications(ctx, "alice-id", []store.Notification{notification("a4", store.NotificationTest, "test:x", "", 10)}); err != nil {
			t.Fatal(err)
		}
		if got := channelsOf(t, s, "alice-id", "test:x"); !reflect.DeepEqual(got, []string{store.ChannelWebPush}) {
			t.Fatalf("test kind: %v", got)
		}

		// Without a subscription, e-mail alone is queued; nothing chosen
		// stores nothing.
		if err := s.SaveNotificationRoutes(ctx, "bob-id", "bob-session", store.RoutableKinds, nil, []string{store.NotificationOverdue}, tokenTestNow); err != nil {
			t.Fatal(err)
		}
		list := []store.Notification{notification("b1", store.NotificationOverdue, "overdue:b:1", "", 10), notification("b2", store.NotificationStale, "stale:b:1", "", 10)}
		if err := s.EnqueueNotifications(ctx, "bob-id", list); err != nil {
			t.Fatal(err)
		}
		if got := channelsOf(t, s, "bob-id", "overdue:b:1"); !reflect.DeepEqual(got, []string{store.ChannelEmail}) {
			t.Fatalf("e-mail only: %v", got)
		}
		if has, err := s.HasNotification(ctx, "bob-id", "stale:b:1"); err != nil || has {
			t.Fatalf("stored without a channel: %v %v", has, err)
		}
		// A disabled user gets nothing.
		if err := s.ChangeUser(ctx, "bob@example.com", "", true, tokenTestNow); err != nil {
			t.Fatal(err)
		}
		if err := s.EnqueueNotifications(ctx, "bob-id", []store.Notification{notification("b3", store.NotificationOverdue, "overdue:b:2", "", 10)}); err != nil {
			t.Fatal(err)
		}
		if has, err := s.HasNotification(ctx, "bob-id", "overdue:b:2"); err != nil || has {
			t.Fatalf("disabled user: %v %v", has, err)
		}
	})
}

// TestSourceEnqueueRouting checks that the detection notifications of the
// synchronizations are routed like the engine's.
func TestSourceEnqueueRouting(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		ctx := context.Background()
		s, _ := apiTokenFixture(t, driver)
		subscribe(t, s, "alice")
		subscribe(t, s, "bob")
		if err := s.SaveNotificationRoutes(ctx, "bob-id", "bob-session", store.RoutableKinds, []string{store.NotificationDetected}, []string{store.NotificationDetected}, tokenTestNow); err != nil {
			t.Fatal(err)
		}
		if err := s.SetSlackWebhook(ctx, "bob-id", "bob-session", "sealed", tokenTestNow); err != nil {
			t.Fatal(err)
		}
		for _, u := range []string{"alice", "bob"} {
			c := saveConnection(t, s, u+"-id", u, nil)
			err := s.GitHubSync(ctx, u+"-id", c.Generation, func(tx *store.GitHubTx) error {
				if _, e := tx.CreateItem(githubItem(u+"-item", "github:review:o/r#1", 100)); e != nil {
					return e
				}
				if e := tx.Enqueue(notification(u+"-n", store.NotificationDetected, "detected:1", u+"-item", 100)); e != nil {
					return e
				}
				return tx.Enqueue(notification(u+"-n2", store.NotificationDetected, "detected:2", u+"-item", 100))
			})
			if err != nil {
				t.Fatal(err)
			}
		}
		if got := channelsOf(t, s, "alice-id", "detected:1"); !reflect.DeepEqual(got, []string{store.ChannelWebPush}) {
			t.Fatalf("defaults: %v", got)
		}
		for _, key := range []string{"detected:1", "detected:2"} {
			if got := channelsOf(t, s, "bob-id", key); !reflect.DeepEqual(got, []string{store.ChannelEmail, store.ChannelSlack, store.ChannelWebPush}) {
				t.Fatalf("routed %s: %v", key, got)
			}
		}
	})
}

func TestNotificationUserIDs(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		ctx := context.Background()
		s, _ := testutil.Database(t, driver)
		for _, name := range []string{"push", "slack", "email", "invalid", "empty", "nothing", "disabled"} {
			u := store.User{ID: name + "-id", Email: name + "@example.com", PasswordHash: "x", Role: store.RoleOwner}
			if err := s.CreateUser(ctx, u); err != nil {
				t.Fatal(err)
			}
			if err := s.Login(ctx, u, store.Session{ID: name + "-session", UserID: u.ID, ExpiresAt: tokenTestNow + 3600, CreatedAt: tokenTestNow}, store.Refresh{Hash: "refresh-" + name, SessionID: name + "-session", CreatedAt: tokenTestNow}); err != nil {
				t.Fatal(err)
			}
		}
		subscribe(t, s, "push")
		routes := func(user string, slack, email []string) {
			t.Helper()
			if err := s.SaveNotificationRoutes(ctx, user+"-id", user+"-session", nil, slack, email, tokenTestNow); err != nil {
				t.Fatal(err)
			}
		}
		webhook := func(user string) {
			t.Helper()
			if err := s.SetSlackWebhook(ctx, user+"-id", user+"-session", "sealed-"+user, tokenTestNow); err != nil {
				t.Fatal(err)
			}
		}
		routes("slack", []string{store.NotificationStale}, nil)
		webhook("slack")
		routes("email", nil, []string{store.NotificationDigest})
		routes("invalid", []string{store.NotificationStale}, nil)
		webhook("invalid")
		if _, err := s.MarkSlackWebhookInvalid(ctx, "invalid-id", "sealed-invalid", "webhook_invalid"); err != nil {
			t.Fatal(err)
		}
		routes("empty", nil, nil)
		webhook("empty")
		routes("disabled", nil, []string{store.NotificationDigest})
		if err := s.ChangeUser(ctx, "disabled@example.com", "", true, tokenTestNow); err != nil {
			t.Fatal(err)
		}
		ids, err := s.NotificationUserIDs(ctx)
		if err != nil || !reflect.DeepEqual(ids, []string{"email-id", "push-id", "slack-id"}) {
			t.Fatalf("users: %v %v", ids, err)
		}
	})
}

func TestCreateTestNotificationChannels(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		ctx := context.Background()
		s, _ := apiTokenFixture(t, driver)
		test := func(id, channel string) store.Notification {
			return store.Notification{ID: id, UserID: "alice-id", Kind: store.NotificationTest, DedupeKey: "test:" + id, URL: "/", ScheduledAt: 10, CreatedAt: 10, Channel: channel}
		}
		// A notification without a channel is never stored.
		if err := s.CreateTestNotification(ctx, test("t0", ""), "alice-session", tokenTestNow); err == nil || errors.Is(err, store.ErrNoSubscription) {
			t.Fatalf("empty channel: %v", err)
		}
		if err := s.CreateTestNotification(ctx, test("t1", store.ChannelSlack), "alice-session", tokenTestNow); !errors.Is(err, store.ErrNoSubscription) {
			t.Fatalf("slack without a webhook: %v", err)
		}
		if err := s.SetSlackWebhook(ctx, "alice-id", "alice-session", "sealed", tokenTestNow); err != nil {
			t.Fatal(err)
		}
		if err := s.CreateTestNotification(ctx, test("t2", store.ChannelSlack), "alice-session", tokenTestNow); err != nil {
			t.Fatalf("slack: %v", err)
		}
		if _, err := s.MarkSlackWebhookInvalid(ctx, "alice-id", "sealed", "webhook_invalid"); err != nil {
			t.Fatal(err)
		}
		if err := s.CreateTestNotification(ctx, test("t3", store.ChannelSlack), "alice-session", tokenTestNow); !errors.Is(err, store.ErrNoSubscription) {
			t.Fatalf("slack with an error: %v", err)
		}
		if err := s.CreateTestNotification(ctx, test("t4", store.ChannelEmail), "alice-session", tokenTestNow); err != nil {
			t.Fatalf("email: %v", err)
		}
		if err := s.CreateTestNotification(ctx, test("t5", store.ChannelWebPush), "alice-session", tokenTestNow); !errors.Is(err, store.ErrNoSubscription) {
			t.Fatalf("webpush without a subscription: %v", err)
		}
		for key, want := range map[string][]string{"test:t0": {}, "test:t2": {store.ChannelSlack}, "test:t4": {store.ChannelEmail}, "test:t5": {}} {
			if got := channelsOf(t, s, "alice-id", key); !reflect.DeepEqual(got, want) {
				t.Fatalf("%s: %v", key, got)
			}
		}
	})
}

func TestUserEmail(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		ctx := context.Background()
		s, _ := apiTokenFixture(t, driver)
		if v, err := s.UserEmail(ctx, "alice-id"); err != nil || v != "alice@example.com" {
			t.Fatalf("email: %q %v", v, err)
		}
		if _, err := s.UserEmail(ctx, "missing-id"); !errors.Is(err, store.ErrNoSubscription) {
			t.Fatalf("missing: %v", err)
		}
		if err := s.ChangeUser(ctx, "bob@example.com", "", true, tokenTestNow); err != nil {
			t.Fatal(err)
		}
		if _, err := s.UserEmail(ctx, "bob-id"); !errors.Is(err, store.ErrNoSubscription) {
			t.Fatalf("disabled: %v", err)
		}
	})
}

// TestCheckSchemaChannels checks that a database without the channel tables
// and columns is reported.
func TestCheckSchemaChannels(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		for _, stmts := range [][]string{
			{"DROP TABLE notification_channels"},
			{"DROP INDEX notifications_user_dedupe_key", "ALTER TABLE notifications DROP COLUMN channel"},
		} {
			s, dsn := testutil.Database(t, driver)
			name := driver
			if driver == "postgres" {
				name = "pgx"
			}
			raw, err := sql.Open(name, dsn)
			if err != nil {
				t.Fatal(err)
			}
			for _, q := range stmts {
				if _, err = raw.ExecContext(context.Background(), q); err != nil {
					t.Fatal(err)
				}
			}
			if err = raw.Close(); err != nil {
				t.Fatal(err)
			}
			if err = s.CheckSchema(context.Background()); err == nil {
				t.Fatalf("missing %v accepted", stmts)
			}
		}
	})
}
