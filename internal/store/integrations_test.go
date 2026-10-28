package store_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/i2bskn/astovia/internal/store"
)

func githubItem(id, key string, created int64) store.ActionItem {
	return store.ActionItem{ID: id, Content: "レビュー: " + id, Status: "open", Refs: "[]", Version: 1, CreatedAt: created, UpdatedAt: created,
		Source: sql.NullString{String: store.SourceGitHub, Valid: true}, SourceKey: sql.NullString{String: key, Valid: true}, Tracking: 1, DetectedAt: sql.NullInt64{Int64: created, Valid: true}}
}

func notification(id, kind, key string, item string, scheduled int64) store.Notification {
	n := store.Notification{ID: id, Kind: kind, DedupeKey: key, Title: "t", Body: "b", URL: "/", ScheduledAt: scheduled, CreatedAt: scheduled}
	if item != "" {
		n.ActionItemID = sql.NullString{String: item, Valid: true}
	}
	return n
}

func saveConnection(t *testing.T, s *store.Store, userID, login string, notify *bool) store.GitHubConnection {
	t.Helper()
	c, err := s.SaveGitHubConnection(context.Background(), store.GitHubConnection{UserID: userID, TokenSealed: "sealed-" + login, TokenLast4: "abcd", Login: login, Scopes: "repo"}, notify, userID[:len(userID)-3]+"-session", tokenTestNow)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestGitHubConnections(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) { testGitHubConnections(t, driver) })
	}
}

func testGitHubConnections(t *testing.T, driver string) {
	ctx := context.Background()
	s, _ := apiTokenFixture(t, driver)

	if _, err := s.GitHubConnection(ctx, "alice-id"); !errors.Is(err, store.ErrConnectionNotFound) {
		t.Fatalf("missing connection: %v", err)
	}
	off := false
	c := saveConnection(t, s, "alice-id", "Octocat", &off)
	if c.Generation != 1 || c.Status != store.GitHubActive || c.InitialImportPending != 1 || c.NextPollAt != tokenTestNow || c.ImmediateNotify != 0 || c.LastError.Valid {
		t.Fatalf("new connection: %+v", c)
	}
	got, err := s.GitHubConnection(ctx, "alice-id")
	if err != nil || !reflect.DeepEqual(got, c) {
		t.Fatalf("round trip: %+v %v", got, err)
	}

	// The synchronization writes only while the generation matches.
	err = s.GitHubSync(ctx, "alice-id", c.Generation, func(tx *store.GitHubTx) error {
		if ok, e := tx.CreateItem(githubItem("pr1", "github:review:o/r#1", 100)); e != nil || !ok {
			return fmt.Errorf("create: %v %v", ok, e)
		}
		if ok, e := tx.CreateItem(githubItem("pr1-dup", "github:review:o/r#1", 100)); e != nil || ok {
			return fmt.Errorf("duplicate source key: %v %v", ok, e)
		}
		if e := tx.Enqueue(notification("n1", store.NotificationDetected, "detected:pr1:1", "pr1", 100)); e != nil {
			return e
		}
		state := tx.Conn
		state.InitialImportPending, state.FailureCount, state.LastError = 0, 2, sql.NullString{String: "unavailable", Valid: true}
		state.ImmediateNotify, state.Login = 1, "ignored"
		return tx.SaveState(state)
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err = s.GitHubConnection(ctx, "alice-id")
	if err != nil || got.InitialImportPending != 0 || got.FailureCount != 2 || got.LastError.String != "unavailable" || got.ImmediateNotify != 0 || got.Login != "Octocat" {
		t.Fatalf("after SaveState: %+v %v", got, err)
	}
	called := false
	if err = s.GitHubSync(ctx, "alice-id", c.Generation+1, func(*store.GitHubTx) error { called = true; return nil }); !errors.Is(err, store.ErrStaleConnection) || called {
		t.Fatalf("other generation: %v called=%v", err, called)
	}
	if err = s.GitHubSync(ctx, "bob-id", 1, func(*store.GitHubTx) error { called = true; return nil }); !errors.Is(err, store.ErrStaleConnection) || called {
		t.Fatalf("missing connection: %v called=%v", err, called)
	}

	// Re-registering the same login (any case) keeps the items tracked and
	// the notification setting; synchronization state is reset.
	c = saveConnection(t, s, "alice-id", "octocat", nil)
	if c.Generation != 2 || c.ImmediateNotify != 0 || c.InitialImportPending != 1 || c.FailureCount != 0 || c.LastError.Valid {
		t.Fatalf("re-registered: %+v", c)
	}
	item, err := s.ActionItem(ctx, "alice-id", "pr1")
	if err != nil || item.Tracking != 1 || item.SourceKey.String != "github:review:o/r#1" || item.Version != 1 {
		t.Fatalf("same login item: %+v %v", item, err)
	}
	if c, err = s.SetGitHubImmediateNotify(ctx, "alice-id", true, "alice-session", tokenTestNow); err != nil || c.ImmediateNotify != 1 {
		t.Fatalf("PATCH: %+v %v", c, err)
	}
	if _, err = s.SetGitHubImmediateNotify(ctx, "bob-id", true, "bob-session", tokenTestNow); !errors.Is(err, store.ErrConnectionNotFound) {
		t.Fatalf("PATCH without connection: %v", err)
	}

	// Another login: items stop being tracked and lose their key.
	c = saveConnection(t, s, "alice-id", "someone-else", nil)
	if c.Generation != 3 {
		t.Fatalf("generation: %d", c.Generation)
	}
	item, err = s.ActionItem(ctx, "alice-id", "pr1")
	if err != nil || item.Tracking != 0 || item.SourceKey.Valid || item.Version != 2 || item.UpdatedAt != tokenTestNow || item.Source.String != store.SourceGitHub {
		t.Fatalf("other login item: %+v %v", item, err)
	}

	// Delete untracks, drops pending detections and is idempotent.
	err = s.GitHubSync(ctx, "alice-id", c.Generation, func(tx *store.GitHubTx) error {
		if _, e := tx.CreateItem(githubItem("pr2", "github:review:o/r#2", 200)); e != nil {
			return e
		}
		return tx.Enqueue(notification("n2", store.NotificationDetected, "detected:pr2:1", "pr2", 200))
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.DeleteGitHubConnection(ctx, "alice-id", "alice-session", tokenTestNow); err != nil {
		t.Fatal(err)
	}
	if err = s.DeleteGitHubConnection(ctx, "alice-id", "alice-session", tokenTestNow); err != nil {
		t.Fatal(err)
	}
	if _, err = s.GitHubConnection(ctx, "alice-id"); !errors.Is(err, store.ErrConnectionNotFound) {
		t.Fatalf("deleted: %v", err)
	}
	item, err = s.ActionItem(ctx, "alice-id", "pr2")
	if err != nil || item.Tracking != 0 || item.SourceKey.Valid || item.Version != 2 {
		t.Fatalf("deleted connection item: %+v %v", item, err)
	}
	if has, err := s.HasNotification(ctx, "alice-id", "detected:pr2:1"); err != nil || has {
		t.Fatalf("pending detection kept: %v %v", has, err)
	}

	// A revoked session cannot write.
	if err = s.ChangeUser(ctx, "bob@example.com", "new", false, tokenTestNow); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SaveGitHubConnection(ctx, store.GitHubConnection{UserID: "bob-id", TokenSealed: "x", TokenLast4: "x", Login: "bob", Scopes: "repo"}, nil, "bob-session", tokenTestNow); !errors.Is(err, store.ErrUnauthorized) {
		t.Fatalf("revoked session: %v", err)
	}
	if err = s.DeleteGitHubConnection(ctx, "bob-id", "bob-session", tokenTestNow); !errors.Is(err, store.ErrUnauthorized) {
		t.Fatalf("revoked session delete: %v", err)
	}
}

// Disconnecting one integration drops only its own pending detections.
func TestDeleteConnectionKeepsOtherSources(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			ctx := context.Background()
			s, _ := apiTokenFixture(t, driver)
			c := saveConnection(t, s, "alice-id", "octocat", nil)
			err := s.GitHubSync(ctx, "alice-id", c.Generation, func(tx *store.GitHubTx) error {
				other := githubItem("slack-item", "slack:T1:m:C1:-:1.000001", 100)
				other.Source = sql.NullString{String: store.SourceSlack, Valid: true}
				for _, item := range []store.ActionItem{githubItem("gh-item", "github:review:o/r#1", 100), other} {
					if _, e := tx.CreateItem(item); e != nil {
						return e
					}
					if e := tx.Enqueue(notification("n-"+item.ID, store.NotificationDetected, "detected:"+item.ID+":1", item.ID, 100)); e != nil {
						return e
					}
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if err = s.DeleteGitHubConnection(ctx, "alice-id", "alice-session", tokenTestNow); err != nil {
				t.Fatal(err)
			}
			if has, err := s.HasNotification(ctx, "alice-id", "detected:gh-item:1"); err != nil || has {
				t.Fatalf("GitHub detection kept: %v %v", has, err)
			}
			if has, err := s.HasNotification(ctx, "alice-id", "detected:slack-item:1"); err != nil || !has {
				t.Fatalf("Slack detection dropped by the GitHub disconnect: %v %v", has, err)
			}
		})
	}
}

func TestSourcedActionItems(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) { testSourcedActionItems(t, driver) })
	}
}

func testSourcedActionItems(t *testing.T, driver string) {
	ctx := context.Background()
	s, _ := apiTokenFixture(t, driver)
	c := saveConnection(t, s, "alice-id", "octocat", nil)
	err := s.GitHubSync(ctx, "alice-id", c.Generation, func(tx *store.GitHubTx) error {
		for i, id := range []string{"a", "b", "c"} {
			item := githubItem(id, "github:assign:o/r#"+id, int64(100+i))
			if id == "b" {
				item.TrackedAt = sql.NullInt64{Int64: 50, Valid: true}
			}
			if _, e := tx.CreateItem(item); e != nil {
				return e
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Never checked items come first, then by tracked_at and id.
	tracked, err := s.TrackedActionItems(ctx, "alice-id", store.SourceGitHub, 10)
	if err != nil || len(tracked) != 3 || tracked[0].ID != "a" || tracked[1].ID != "c" || tracked[2].ID != "b" {
		t.Fatalf("tracked order: %+v %v", tracked, err)
	}

	// UpdateItem and TouchItem keep the optimistic lock.
	err = s.GitHubSync(ctx, "alice-id", c.Generation, func(tx *store.GitHubTx) error {
		current, ok, e := tx.LockItemBySourceKey("github:assign:o/r#a")
		if e != nil || !ok {
			return fmt.Errorf("lock by key: %v %v", ok, e)
		}
		next := current
		next.Status, next.AutoReason, next.AutoAt, next.UpdatedAt = "done", sql.NullString{String: "github.unassigned", Valid: true}, sql.NullInt64{Int64: 300, Valid: true}, 300
		updated, ok, e := tx.UpdateItem(current, next)
		if e != nil || !ok || updated.Version != 2 {
			return fmt.Errorf("update: %+v %v %v", updated, ok, e)
		}
		if _, ok, e = tx.UpdateItem(current, next); e != nil || ok {
			return fmt.Errorf("stale update: %v %v", ok, e)
		}
		if e = tx.TouchItem(current, 999); e != nil {
			return e
		}
		other, _, e := tx.LockItem("c")
		if e != nil {
			return e
		}
		return tx.TouchItem(other, 400)
	})
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.ActionItem(ctx, "alice-id", "a")
	if err != nil || a.TrackedAt.Valid || a.Status != "done" || a.Version != 2 {
		t.Fatalf("stale touch wrote: %+v %v", a, err)
	}
	cItem, err := s.ActionItem(ctx, "alice-id", "c")
	if err != nil || cItem.TrackedAt.Int64 != 400 || cItem.Version != 1 {
		t.Fatalf("touch: %+v %v", cItem, err)
	}

	// Automatically completed items are listed newest auto_at first.
	err = s.GitHubSync(ctx, "alice-id", c.Generation, func(tx *store.GitHubTx) error {
		for _, v := range []struct {
			id string
			at int64
		}{{"b", 300}, {"c", 500}} {
			current, _, e := tx.LockItem(v.id)
			if e != nil {
				return e
			}
			next := current
			next.Status, next.AutoReason, next.AutoAt = "done", sql.NullString{String: "github.issue_closed", Valid: true}, sql.NullInt64{Int64: v.at, Valid: true}
			if _, _, e = tx.UpdateItem(current, next); e != nil {
				return e
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	ids := func(items []store.ActionItem) []string {
		out := []string{}
		for _, i := range items {
			out = append(out, i.ID)
		}
		return out
	}
	list, err := s.ListAutoActionItems(ctx, "alice-id", "", nil, 10)
	if err != nil || !reflect.DeepEqual(ids(list), []string{"c", "b", "a"}) {
		t.Fatalf("auto list: %v %v", ids(list), err)
	}
	list, err = s.ListAutoActionItems(ctx, "alice-id", "done", &store.AutoActionItemCursor{AutoAt: 300, ID: "b"}, 10)
	if err != nil || !reflect.DeepEqual(ids(list), []string{"a"}) {
		t.Fatalf("auto list after cursor: %v %v", ids(list), err)
	}
	if list, err = s.ListAutoActionItems(ctx, "alice-id", "open", nil, 10); err != nil || len(list) != 0 {
		t.Fatalf("auto open: %v %v", ids(list), err)
	}
	if n, err := s.CountAutoActionItemsSince(ctx, "alice-id", 300); err != nil || n != 1 {
		t.Fatalf("auto since: %d %v", n, err)
	}
	if n, err := s.CountActionItems(ctx, "alice-id", "done"); err != nil || n != 3 {
		t.Fatalf("count done: %d %v", n, err)
	}
}

func TestPushSubscriptions(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) { testPushSubscriptions(t, driver) })
	}
}

func testPushSubscriptions(t *testing.T, driver string) {
	ctx := context.Background()
	s, _ := apiTokenFixture(t, driver)
	sub := func(id, endpoint, user string) store.PushSubscription {
		return store.PushSubscription{ID: id, UserID: user, Endpoint: endpoint, P256dh: "key-" + id, Auth: "auth-" + id, Label: "label " + id}
	}
	saved, created, err := s.SavePushSubscription(ctx, sub("s0", "https://push.example/0", "alice-id"), "Asia/Tokyo", "alice-session", tokenTestNow)
	if err != nil || !created || saved.CreatedAt != tokenTestNow {
		t.Fatalf("create: %+v %v %v", saved, created, err)
	}
	settings, err := s.NotificationSettings(ctx, "alice-id")
	if err != nil || settings.Timezone != "Asia/Tokyo" || settings.DigestHour != store.DefaultDigestHour || settings.StaleDays != store.DefaultStaleDays {
		t.Fatalf("settings from timezone: %+v %v", settings, err)
	}
	// An existing settings row is not changed by a later subscription.
	if _, _, err = s.SavePushSubscription(ctx, sub("s1", "https://push.example/1", "alice-id"), "Europe/Paris", "alice-session", tokenTestNow); err != nil {
		t.Fatal(err)
	}
	if settings, err = s.NotificationSettings(ctx, "alice-id"); err != nil || settings.Timezone != "Asia/Tokyo" {
		t.Fatalf("settings changed: %+v %v", settings, err)
	}
	if settings, err = s.NotificationSettings(ctx, "bob-id"); err != nil || settings.Timezone != store.DefaultTimezone || settings.UserID != "bob-id" {
		t.Fatalf("default settings: %+v %v", settings, err)
	}

	// Re-registering an endpoint updates it in place.
	again := sub("ignored", "https://push.example/0", "alice-id")
	again.P256dh, again.Label = "new-key", "new label"
	saved, created, err = s.SavePushSubscription(ctx, again, "", "alice-session", tokenTestNow+5)
	if err != nil || created || saved.ID != "s0" || saved.P256dh != "new-key" || saved.Label != "new label" || saved.CreatedAt != tokenTestNow {
		t.Fatalf("re-register: %+v %v %v", saved, created, err)
	}

	for i := 2; i < store.MaxPushSubscriptions; i++ {
		if _, _, err = s.SavePushSubscription(ctx, sub(fmt.Sprintf("s%d", i), fmt.Sprintf("https://push.example/%d", i), "alice-id"), "", "alice-session", tokenTestNow); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err = s.SavePushSubscription(ctx, sub("s20", "https://push.example/20", "alice-id"), "", "alice-session", tokenTestNow); !errors.Is(err, store.ErrTooManySubscriptions) {
		t.Fatalf("21st subscription: %v", err)
	}
	if _, created, err = s.SavePushSubscription(ctx, sub("x", "https://push.example/3", "alice-id"), "", "alice-session", tokenTestNow); err != nil || created {
		t.Fatalf("re-register at the limit: %v %v", created, err)
	}

	// Bob signs in on alice's browser: the endpoint moves to bob.
	saved, created, err = s.SavePushSubscription(ctx, sub("b0", "https://push.example/0", "bob-id"), "", "bob-session", tokenTestNow)
	if err != nil || !created || saved.ID != "b0" {
		t.Fatalf("move: %+v %v %v", saved, created, err)
	}
	alice, err := s.PushSubscriptions(ctx, "alice-id")
	if err != nil || len(alice) != store.MaxPushSubscriptions-1 {
		t.Fatalf("alice after move: %d %v", len(alice), err)
	}
	if err = s.DeletePushSubscription(ctx, "alice-id", "b0"); !errors.Is(err, store.ErrSubscriptionNotFound) {
		t.Fatalf("delete other user's: %v", err)
	}
	if err = s.DeletePushSubscription(ctx, "bob-id", "b0"); err != nil {
		t.Fatal(err)
	}
	if err = s.TouchPushSubscription(ctx, "s1", tokenTestNow+9); err != nil {
		t.Fatal(err)
	}
	if ids, err := s.PushUserIDs(ctx); err != nil || !reflect.DeepEqual(ids, []string{"alice-id"}) {
		t.Fatalf("push users: %v %v", ids, err)
	}

	// A VAPID key change removes every subscription, only when it wins.
	if ok, err := s.CreateInstanceSecret(ctx, "vapid", "v1", tokenTestNow); err != nil || !ok {
		t.Fatalf("create secret: %v %v", ok, err)
	}
	if ok, err := s.CreateInstanceSecret(ctx, "vapid", "v2", tokenTestNow); err != nil || ok {
		t.Fatalf("second create: %v %v", ok, err)
	}
	if ok, err := s.ReplaceInstanceSecret(ctx, "vapid", "stale", "v3", tokenTestNow, true); err != nil || ok {
		t.Fatalf("stale replace: %v %v", ok, err)
	}
	if alice, err = s.PushSubscriptions(ctx, "alice-id"); err != nil || len(alice) == 0 {
		t.Fatalf("subscriptions removed by a lost replace: %d %v", len(alice), err)
	}
	if ok, err := s.ReplaceInstanceSecret(ctx, "vapid", "v1", "v3", tokenTestNow, true); err != nil || !ok {
		t.Fatalf("replace: %v %v", ok, err)
	}
	if sec, err := s.InstanceSecret(ctx, "vapid"); err != nil || sec.Value != "v3" {
		t.Fatalf("secret: %+v %v", sec, err)
	}
	if alice, err = s.PushSubscriptions(ctx, "alice-id"); err != nil || len(alice) != 0 {
		t.Fatalf("subscriptions after replace: %d %v", len(alice), err)
	}
	if _, err = s.InstanceSecret(ctx, "missing"); !errors.Is(err, store.ErrSecretNotFound) {
		t.Fatalf("missing secret: %v", err)
	}
}

func TestNotificationQueue(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) { testNotificationQueue(t, driver) })
	}
}

func testNotificationQueue(t *testing.T, driver string) {
	ctx := context.Background()
	s, _ := apiTokenFixture(t, driver)
	// Without a subscription nothing is queued.
	if err := s.EnqueueNotifications(ctx, "alice-id", []store.Notification{notification("n0", store.NotificationDigest, "digest:x", "", 10)}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateTestNotification(ctx, store.Notification{ID: "t0", UserID: "alice-id", Kind: store.NotificationTest, DedupeKey: "test:0", URL: "/", ScheduledAt: 10, CreatedAt: 10}, "alice-session", tokenTestNow); !errors.Is(err, store.ErrNoSubscription) {
		t.Fatalf("test without subscription: %v", err)
	}
	for _, u := range []string{"alice", "bob"} {
		if _, _, err := s.SavePushSubscription(ctx, store.PushSubscription{ID: u + "-sub", UserID: u + "-id", Endpoint: "https://push.example/" + u, P256dh: "k", Auth: "a"}, "", u+"-session", tokenTestNow); err != nil {
			t.Fatal(err)
		}
	}
	list := []store.Notification{notification("n1", store.NotificationDigest, "digest:2026-10-24", "", 30), notification("n2", store.NotificationDigest, "digest:2026-10-24", "", 5), notification("n3", store.NotificationStale, "stale:x:1", "", 20)}
	if err := s.EnqueueNotifications(ctx, "alice-id", list); err != nil {
		t.Fatal(err)
	}
	if err := s.EnqueueNotifications(ctx, "bob-id", []store.Notification{notification("n4", store.NotificationDigest, "digest:2026-10-24", "", 1)}); err != nil {
		t.Fatal(err)
	}
	if has, err := s.HasNotification(ctx, "alice-id", "digest:2026-10-24"); err != nil || !has {
		t.Fatalf("has: %v %v", has, err)
	}
	if at, ok, err := s.LatestNotificationAt(ctx, "alice-id", store.NotificationDigest); err != nil || !ok || at != 30 {
		t.Fatalf("latest: %d %v %v", at, ok, err)
	}
	if _, ok, err := s.LatestNotificationAt(ctx, "alice-id", store.NotificationTest); err != nil || ok {
		t.Fatalf("latest missing: %v %v", ok, err)
	}
	due, err := s.DueNotifications(ctx, 25, 10)
	if err != nil || len(due) != 2 || due[0].ID != "n4" || due[1].ID != "n3" {
		t.Fatalf("due: %+v %v", due, err)
	}
	// Disabled users are skipped; their pending notifications are deleted.
	if err = s.ChangeUser(ctx, "bob@example.com", "", true, tokenTestNow); err != nil {
		t.Fatal(err)
	}
	if due, err = s.DueNotifications(ctx, 25, 10); err != nil || len(due) != 1 || due[0].ID != "n3" {
		t.Fatalf("due after disable: %+v %v", due, err)
	}
	n := due[0]
	n.Attempts, n.LastError, n.ScheduledAt = 1, sql.NullString{String: "send_failed", Valid: true}, 100
	if err = s.SaveNotificationResult(ctx, n); err != nil {
		t.Fatal(err)
	}
	if due, err = s.DueNotifications(ctx, 99, 10); err != nil || len(due) != 1 || due[0].ID != "n1" {
		t.Fatalf("rescheduled: %+v %v", due, err)
	}
	n.SentAt = sql.NullInt64{Int64: 1000, Valid: true}
	if err = s.SaveNotificationResult(ctx, n); err != nil {
		t.Fatal(err)
	}
	if due, err = s.DueNotifications(ctx, 1000, 10); err != nil || len(due) != 1 || due[0].ID != "n1" {
		t.Fatalf("after sent: %+v %v", due, err)
	}
	if err = s.DeleteSentNotifications(ctx, 1001); err != nil {
		t.Fatal(err)
	}
	if has, err := s.HasNotification(ctx, "alice-id", "stale:x:1"); err != nil || has {
		t.Fatalf("cleanup: %v %v", has, err)
	}
	if has, err := s.HasNotification(ctx, "alice-id", "digest:2026-10-24"); err != nil || !has {
		t.Fatalf("cleanup removed an unsent one: %v %v", has, err)
	}
}

func TestChangeUserIntegrations(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) { testChangeUserIntegrations(t, driver) })
	}
}

func testChangeUserIntegrations(t *testing.T, driver string) {
	ctx := context.Background()
	s, _ := apiTokenFixture(t, driver)
	for _, u := range []string{"alice", "bob"} {
		c := saveConnection(t, s, u+"-id", u, nil)
		if _, _, err := s.SavePushSubscription(ctx, store.PushSubscription{ID: u + "-sub", UserID: u + "-id", Endpoint: "https://push.example/" + u, P256dh: "k", Auth: "a"}, "", u+"-session", tokenTestNow); err != nil {
			t.Fatal(err)
		}
		err := s.GitHubSync(ctx, u+"-id", c.Generation, func(tx *store.GitHubTx) error {
			if _, e := tx.CreateItem(githubItem(u+"-item", "github:review:o/r#1", 100)); e != nil {
				return e
			}
			if e := tx.Enqueue(notification(u+"-pending", store.NotificationDetected, "detected:1", u+"-item", 100)); e != nil {
				return e
			}
			return tx.Enqueue(notification(u+"-sent", store.NotificationTest, "test:1", "", 100))
		})
		if err != nil {
			t.Fatal(err)
		}
		due, err := s.DueNotifications(ctx, 100, 10)
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range due {
			if n.ID == u+"-sent" {
				n.SentAt = sql.NullInt64{Int64: 100, Valid: true}
				if err = s.SaveNotificationResult(ctx, n); err != nil {
					t.Fatal(err)
				}
			}
		}
	}

	check := func(user string, connection bool, subs int, pending, sent bool) {
		t.Helper()
		_, err := s.GitHubConnection(ctx, user+"-id")
		if connection != (err == nil) || (err != nil && !errors.Is(err, store.ErrConnectionNotFound)) {
			t.Fatalf("%s connection: %v", user, err)
		}
		list, err := s.PushSubscriptions(ctx, user+"-id")
		if err != nil || len(list) != subs {
			t.Fatalf("%s subscriptions: %d %v", user, len(list), err)
		}
		if has, err := s.HasNotification(ctx, user+"-id", "detected:1"); err != nil || has != pending {
			t.Fatalf("%s pending notification: %v %v", user, has, err)
		}
		if has, err := s.HasNotification(ctx, user+"-id", "test:1"); err != nil || has != sent {
			t.Fatalf("%s sent notification: %v %v", user, has, err)
		}
		if item, err := s.ActionItem(ctx, user+"-id", user+"-item"); err != nil || item.Tracking != 1 {
			t.Fatalf("%s item: %+v %v", user, item, err)
		}
	}
	// A password reset removes subscriptions and pending notifications but
	// keeps the GitHub connection.
	if err := s.ChangeUser(ctx, "alice@example.com", "new", false, tokenTestNow); err != nil {
		t.Fatal(err)
	}
	check("alice", true, 0, false, true)
	check("bob", true, 1, true, true)
	// Disabling removes the connection as well.
	if err := s.ChangeUser(ctx, "bob@example.com", "", true, tokenTestNow); err != nil {
		t.Fatal(err)
	}
	check("bob", false, 0, false, true)
	if due, err := s.DueGitHubConnections(ctx, tokenTestNow, 10); err != nil || len(due) != 1 || due[0].UserID != "alice-id" {
		t.Fatalf("due connections: %+v %v", due, err)
	}
}

func TestAISettings(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) { testAISettings(t, driver) })
	}
}

func testAISettings(t *testing.T, driver string) {
	ctx := context.Background()
	s, _ := apiTokenFixture(t, driver)
	in := store.AISettings{UserID: "alice-id", Provider: "anthropic", Model: "claude-haiku-5-5", APIKeySealed: "sealed-1", KeyLast4: "abcd"}

	if _, err := s.AISettings(ctx, "alice-id"); !errors.Is(err, store.ErrAISettingsNotFound) {
		t.Fatalf("missing: %v", err)
	}
	a, err := s.SaveAISettings(ctx, in, 0, true, "alice-session", tokenTestNow)
	if err != nil || a.Generation != 1 || a.Status != store.AIActive || a.LastError.Valid || a.ActivatedAt != tokenTestNow || a.CreatedAt != tokenTestNow {
		t.Fatalf("create: %+v %v", a, err)
	}
	if got, err := s.AISettings(ctx, "alice-id"); err != nil || !reflect.DeepEqual(got, a) {
		t.Fatalf("round trip: %+v %v", got, err)
	}
	if _, err = s.AISettings(ctx, "bob-id"); !errors.Is(err, store.ErrAISettingsNotFound) {
		t.Fatalf("bob: %v", err)
	}
	// A save based on a stale read is rejected.
	if _, err = s.SaveAISettings(ctx, in, 0, true, "alice-session", tokenTestNow); !errors.Is(err, store.ErrAIConflict) {
		t.Fatalf("create twice: %v", err)
	}
	if _, err = s.SaveAISettings(ctx, in, 5, true, "alice-session", tokenTestNow); !errors.Is(err, store.ErrAIConflict) {
		t.Fatalf("wrong generation: %v", err)
	}
	if _, err = s.SaveAISettings(ctx, store.AISettings{UserID: "bob-id", Provider: "anthropic", Model: "m", APIKeySealed: "x", KeyLast4: ""}, 0, true, "alice-session", tokenTestNow); !errors.Is(err, store.ErrUnauthorized) {
		t.Fatalf("another user's session: %v", err)
	}

	// Status updates apply only to the generation that was read.
	if ok, err := s.SetAIStatus(ctx, "alice-id", 1, store.AIError, sql.NullString{String: "key_invalid", Valid: true}, tokenTestNow+5); err != nil || !ok {
		t.Fatalf("status: %v %v", ok, err)
	}
	// A model change keeps the key's activation time and resets the status.
	in.Model = "claude-opus-5-5"
	a, err = s.SaveAISettings(ctx, in, 1, false, "alice-session", tokenTestNow+10)
	if err != nil || a.Generation != 2 || a.Model != "claude-opus-5-5" || a.Status != store.AIActive || a.LastError.Valid || a.ActivatedAt != tokenTestNow || a.CreatedAt != tokenTestNow || a.UpdatedAt != tokenTestNow+10 {
		t.Fatalf("model change: %+v %v", a, err)
	}
	if ok, err := s.SetAIStatus(ctx, "alice-id", 1, store.AIError, sql.NullString{String: "billing", Valid: true}, tokenTestNow+20); err != nil || ok {
		t.Fatalf("stale status: %v %v", ok, err)
	}
	if got, err := s.AISettings(ctx, "alice-id"); err != nil || got.Status != store.AIActive || got.LastError.Valid {
		t.Fatalf("stale status written: %+v %v", got, err)
	}
	in.APIKeySealed, in.KeyLast4 = "sealed-2", "wxyz"
	if a, err = s.SaveAISettings(ctx, in, 2, true, "alice-session", tokenTestNow+30); err != nil || a.Generation != 3 || a.ActivatedAt != tokenTestNow+30 || a.APIKeySealed != "sealed-2" {
		t.Fatalf("new key: %+v %v", a, err)
	}

	// Deleting is idempotent and needs an active session.
	if err = s.DeleteAISettings(ctx, "alice-id", "bob-session", tokenTestNow); !errors.Is(err, store.ErrUnauthorized) {
		t.Fatalf("delete with another session: %v", err)
	}
	for i := 0; i < 2; i++ {
		if err = s.DeleteAISettings(ctx, "alice-id", "alice-session", tokenTestNow); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = s.AISettings(ctx, "alice-id"); !errors.Is(err, store.ErrAISettingsNotFound) {
		t.Fatalf("after delete: %v", err)
	}
	if ok, err := s.SetAIStatus(ctx, "alice-id", 3, store.AIError, sql.NullString{}, tokenTestNow); err != nil || ok {
		t.Fatalf("status of deleted settings: %v %v", ok, err)
	}
}

// Both a password reset and disabling the user remove the AI settings.
func TestChangeUserAISettings(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			ctx := context.Background()
			s, _ := apiTokenFixture(t, driver)
			for _, u := range []string{"alice", "bob"} {
				if _, err := s.SaveAISettings(ctx, store.AISettings{UserID: u + "-id", Provider: "anthropic", Model: "claude-haiku-5-5", APIKeySealed: "s", KeyLast4: ""}, 0, true, u+"-session", tokenTestNow); err != nil {
					t.Fatal(err)
				}
				saveConnection(t, s, u+"-id", u, nil)
			}
			if err := s.ChangeUser(ctx, "alice@example.com", "new", false, tokenTestNow); err != nil {
				t.Fatal(err)
			}
			if _, err := s.AISettings(ctx, "alice-id"); !errors.Is(err, store.ErrAISettingsNotFound) {
				t.Fatalf("after reset: %v", err)
			}
			if _, err := s.AISettings(ctx, "bob-id"); err != nil {
				t.Fatalf("bob after alice's reset: %v", err)
			}
			if err := s.ChangeUser(ctx, "bob@example.com", "", true, tokenTestNow); err != nil {
				t.Fatal(err)
			}
			if _, err := s.AISettings(ctx, "bob-id"); !errors.Is(err, store.ErrAISettingsNotFound) {
				t.Fatalf("after disable: %v", err)
			}
		})
	}
}
