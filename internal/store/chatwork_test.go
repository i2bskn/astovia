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

func chatworkItem(id, key string, created int64) store.ActionItem {
	return store.ActionItem{ID: id, Content: "Chatwork: " + id, Status: "open", Refs: "[]", Version: 1, CreatedAt: created, UpdatedAt: created,
		Source: sql.NullString{String: store.SourceChatwork, Valid: true}, SourceKey: sql.NullString{String: key, Valid: true}, Tracking: 1, DetectedAt: sql.NullInt64{Int64: created, Valid: true}}
}

func saveChatwork(t *testing.T, s *store.Store, userID, account string, settings store.ChatworkSettings, now int64) store.ChatworkConnection {
	t.Helper()
	c, err := s.SaveChatworkConnection(context.Background(), store.ChatworkConnection{UserID: userID, TokenSealed: "sealed-" + account, TokenLast4: "abcd", AccountID: account, AccountName: "name-" + account, OrganizationName: "Org"},
		settings, userID[:len(userID)-3]+"-session", now)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestChatworkConnections(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) { testChatworkConnections(t, driver) })
	}
}

func testChatworkConnections(t *testing.T, driver string) {
	ctx := context.Background()
	s, _ := apiTokenFixture(t, driver)
	if _, err := s.ChatworkConnection(ctx, "alice-id"); !errors.Is(err, store.ErrConnectionNotFound) {
		t.Fatalf("missing: %v", err)
	}
	c := saveChatwork(t, s, "alice-id", "101", store.ChatworkSettings{}, tokenTestNow)
	if c.Generation != 1 || c.Status != store.ChatworkActive || c.ImmediateNotify != 1 || c.DetectMentions != 1 || c.DetectDMs != 1 || c.ImportTasks != 1 ||
		c.ConnectedAt != tokenTestNow || c.CreatedAt != tokenTestNow || c.NextPollAt != tokenTestNow || c.LastSyncedAt.Valid || c.LastTasksAt.Valid || c.OrganizationName != "Org" {
		t.Fatalf("new connection: %+v", c)
	}
	if got, err := s.ChatworkConnection(ctx, "alice-id"); err != nil || !reflect.DeepEqual(got, c) {
		t.Fatalf("round trip: %+v %v", got, err)
	}
	bob := saveChatwork(t, s, "bob-id", "201", store.ChatworkSettings{}, tokenTestNow)

	// SaveState writes only the synchronization state.
	err := s.ChatworkSync(ctx, "alice-id", c.Generation, func(tx *store.ChatworkTx) error {
		if ok, e := tx.CreateItem(chatworkItem("w1", "chatwork:101:m:1:1001", 100)); e != nil || !ok {
			return fmt.Errorf("create: %v %v", ok, e)
		}
		state := tx.Conn
		state.FailureCount, state.LastError, state.LastSyncedAt, state.LastTrackedAt, state.LastTasksAt, state.NextPollAt = 2, sql.NullString{String: "unavailable", Valid: true}, sql.NullInt64{Int64: 500, Valid: true}, sql.NullInt64{Int64: 400, Valid: true}, sql.NullInt64{Int64: 450, Valid: true}, 900
		state.ImmediateNotify, state.DetectMentions, state.ImportTasks, state.ConnectedAt, state.AccountID = 0, 0, 0, 1, "ignored"
		return tx.SaveState(state)
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.ChatworkConnection(ctx, "alice-id")
	if err != nil || got.FailureCount != 2 || got.LastError.String != "unavailable" || got.LastSyncedAt.Int64 != 500 || got.LastTrackedAt.Int64 != 400 || got.LastTasksAt.Int64 != 450 || got.NextPollAt != 900 ||
		got.ImmediateNotify != 1 || got.DetectMentions != 1 || got.ImportTasks != 1 || got.ConnectedAt != tokenTestNow || got.AccountID != "101" {
		t.Fatalf("after SaveState: %+v %v", got, err)
	}
	called := false
	if err = s.ChatworkSync(ctx, "alice-id", c.Generation+1, func(*store.ChatworkTx) error { called = true; return nil }); !errors.Is(err, store.ErrStaleConnection) || called {
		t.Fatalf("other generation: %v %v", err, called)
	}
	if err = s.ChatworkSync(ctx, "carol-id", 1, func(*store.ChatworkTx) error { called = true; return nil }); !errors.Is(err, store.ErrStaleConnection) || called {
		t.Fatalf("missing connection: %v %v", err, called)
	}

	// PATCH merges the given settings.
	if c, err = s.UpdateChatworkSettings(ctx, "alice-id", store.ChatworkSettings{DetectDMs: boolPtr(false), ImportTasks: boolPtr(false)}, "alice-session", tokenTestNow+5); err != nil ||
		c.DetectDMs != 0 || c.ImportTasks != 0 || c.DetectMentions != 1 || c.ImmediateNotify != 1 || c.UpdatedAt != tokenTestNow+5 || c.LastSyncedAt.Int64 != 500 {
		t.Fatalf("settings: %+v %v", c, err)
	}
	if got, err = s.ChatworkConnection(ctx, "alice-id"); err != nil || !reflect.DeepEqual(got, c) {
		t.Fatalf("settings stored: %+v %v", got, err)
	}

	// The same account: the settings, the creation time and the tracking
	// stay; the synchronization state and ConnectedAt are reset.
	c = saveChatwork(t, s, "alice-id", "101", store.ChatworkSettings{ImmediateNotify: boolPtr(false)}, tokenTestNow+10)
	if c.Generation != 2 || c.ImmediateNotify != 0 || c.DetectDMs != 0 || c.ImportTasks != 0 || c.CreatedAt != tokenTestNow || c.ConnectedAt != tokenTestNow+10 ||
		c.LastSyncedAt.Valid || c.LastTrackedAt.Valid || c.LastTasksAt.Valid || c.FailureCount != 0 || c.LastError.Valid || c.NextPollAt != tokenTestNow+10 {
		t.Fatalf("re-registered: %+v", c)
	}
	if item, err := s.ActionItem(ctx, "alice-id", "w1"); err != nil || item.Tracking != 1 || item.SourceKey.String != "chatwork:101:m:1:1001" || item.Version != 1 {
		t.Fatalf("same account item: %+v %v", item, err)
	}
	// Another account stops the tracking but keeps the source key.
	c = saveChatwork(t, s, "alice-id", "102", store.ChatworkSettings{}, tokenTestNow+20)
	if item, err := s.ActionItem(ctx, "alice-id", "w1"); err != nil || item.Tracking != 0 || item.SourceKey.String != "chatwork:101:m:1:1001" || item.Version != 2 {
		t.Fatalf("other account item: %+v %v", item, err)
	}
	if due, err := s.DueChatworkConnections(ctx, tokenTestNow+20, 10); err != nil || len(due) != 2 || due[0].UserID != "bob-id" || due[1].UserID != "alice-id" {
		t.Fatalf("due: %+v %v", due, err)
	}
	if due, err := s.DueChatworkConnections(ctx, tokenTestNow+19, 10); err != nil || len(due) != 1 || due[0].UserID != "bob-id" {
		t.Fatalf("not due: %+v %v", due, err)
	}
	// Connections in error are not due.
	if err = s.ChatworkSync(ctx, "bob-id", bob.Generation, func(tx *store.ChatworkTx) error {
		state := tx.Conn
		state.Status = store.ChatworkError
		return tx.SaveState(state)
	}); err != nil {
		t.Fatal(err)
	}
	if due, err := s.DueChatworkConnections(ctx, tokenTestNow+20, 10); err != nil || len(due) != 1 || due[0].UserID != "alice-id" {
		t.Fatalf("error not due: %+v %v", due, err)
	}

	// Delete stops the tracking, keeps the source keys, drops only
	// Chatwork's pending detections and is idempotent.
	err = s.ChatworkSync(ctx, "alice-id", c.Generation, func(tx *store.ChatworkTx) error {
		if _, e := tx.CreateItem(chatworkItem("w3", "chatwork:102:m:1:1003", 300)); e != nil {
			return e
		}
		return tx.Enqueue(notification("n-w3", store.NotificationDetected, "detected:w3:1", "w3", 300))
	})
	if err != nil {
		t.Fatal(err)
	}
	err = s.ChatworkSync(ctx, "bob-id", bob.Generation, func(tx *store.ChatworkTx) error {
		if _, e := tx.CreateItem(chatworkItem("b1", "chatwork:201:m:1:1001", 300)); e != nil {
			return e
		}
		return tx.Enqueue(notification("n-b1", store.NotificationDetected, "detected:b1:1", "b1", 300))
	})
	if err != nil {
		t.Fatal(err)
	}
	gh := saveConnection(t, s, "alice-id", "octocat", nil)
	err = s.GitHubSync(ctx, "alice-id", gh.Generation, func(tx *store.GitHubTx) error {
		if _, e := tx.CreateItem(githubItem("g1", "github:review:o/r#1", 300)); e != nil {
			return e
		}
		return tx.Enqueue(notification("n-g1", store.NotificationDetected, "detected:g1:1", "g1", 300))
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err = s.DeleteChatworkConnection(ctx, "alice-id", "alice-session", tokenTestNow); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = s.ChatworkConnection(ctx, "alice-id"); !errors.Is(err, store.ErrConnectionNotFound) {
		t.Fatalf("deleted: %v", err)
	}
	if item, err := s.ActionItem(ctx, "alice-id", "w3"); err != nil || item.Tracking != 0 || item.SourceKey.String != "chatwork:102:m:1:1003" {
		t.Fatalf("deleted connection item: %+v %v", item, err)
	}
	if has, err := s.HasNotification(ctx, "alice-id", "detected:w3:1"); err != nil || has {
		t.Fatalf("Chatwork detection kept: %v %v", has, err)
	}
	if has, err := s.HasNotification(ctx, "alice-id", "detected:g1:1"); err != nil || !has {
		t.Fatalf("GitHub detection dropped: %v %v", has, err)
	}
	// The other user's connection, items and notifications are untouched.
	if _, err = s.ChatworkConnection(ctx, "bob-id"); err != nil {
		t.Fatalf("other user's connection: %v", err)
	}
	if item, err := s.ActionItem(ctx, "bob-id", "b1"); err != nil || item.Tracking != 1 || item.Version != 1 {
		t.Fatalf("other user's item: %+v %v", item, err)
	}
	if has, err := s.HasNotification(ctx, "bob-id", "detected:b1:1"); err != nil || !has {
		t.Fatalf("other user's detection dropped: %v %v", has, err)
	}

	// A revoked session cannot write.
	if err = s.ChangeUser(ctx, "bob@example.com", "new", false, tokenTestNow); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SaveChatworkConnection(ctx, store.ChatworkConnection{UserID: "bob-id", AccountID: "201"}, store.ChatworkSettings{}, "bob-session", tokenTestNow); !errors.Is(err, store.ErrUnauthorized) {
		t.Fatalf("revoked session: %v", err)
	}
	if _, err = s.UpdateChatworkSettings(ctx, "bob-id", store.ChatworkSettings{DetectDMs: boolPtr(true)}, "bob-session", tokenTestNow); !errors.Is(err, store.ErrUnauthorized) {
		t.Fatalf("revoked session settings: %v", err)
	}
	if err = s.DeleteChatworkConnection(ctx, "bob-id", "bob-session", tokenTestNow); !errors.Is(err, store.ErrUnauthorized) {
		t.Fatalf("revoked session delete: %v", err)
	}
}

func TestChatworkKeyQueries(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) { testChatworkKeyQueries(t, driver) })
	}
}

func testChatworkKeyQueries(t *testing.T, driver string) {
	ctx := context.Background()
	s, _ := apiTokenFixture(t, driver)
	c := saveChatwork(t, s, "alice-id", "101", store.ChatworkSettings{}, tokenTestNow)
	items := []store.ActionItem{
		chatworkItem("t1", "chatwork:101:t:11", 100),
		chatworkItem("t2", "chatwork:101:t:12", 200),
		chatworkItem("old-d", "chatwork:101:d:5:1:2", 50),
		chatworkItem("d", "chatwork:101:d:5:3:4", 200),
		// Other kinds, accounts and LIKE wildcards do not match.
		chatworkItem("m", "chatwork:101:m:5:1", 300),
		chatworkItem("other", "chatwork:102:t:13", 300),
		chatworkItem("wildcard", "chatwork:101:tX14", 300),
	}
	done := chatworkItem("done", "chatwork:101:t:15", 400)
	done.Status, done.Tracking = "done", 0
	candidate := chatworkItem("candidate", "chatwork:101:t:16", 400)
	candidate.Status = "candidate"
	items = append(items, done, candidate)
	err := s.ChatworkSync(ctx, "alice-id", c.Generation, func(tx *store.ChatworkTx) error {
		for _, item := range items {
			if _, e := tx.CreateItem(item); e != nil {
				return e
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// A Slack item with a Chatwork-like key is not a Chatwork item.
	sc := saveSlack(t, s, "alice-id", "T1", "U1", store.SlackSettings{}, tokenTestNow)
	if err = s.SlackSync(ctx, "alice-id", sc.Generation, func(tx *store.SlackTx) error {
		_, e := tx.CreateItem(slackItem("slack", "chatwork:101:t:17", 600))
		return e
	}); err != nil {
		t.Fatal(err)
	}
	// Another user's task item is not listed.
	saveChatwork(t, s, "bob-id", "101", store.ChatworkSettings{}, tokenTestNow)
	bob, _ := s.ChatworkConnection(ctx, "bob-id")
	if err = s.ChatworkSync(ctx, "bob-id", bob.Generation, func(tx *store.ChatworkTx) error {
		_, e := tx.CreateItem(chatworkItem("bob-t", "chatwork:101:t:18", 50))
		return e
	}); err != nil {
		t.Fatal(err)
	}

	tasks, err := s.ChatworkTaskItems(ctx, "alice-id", "chatwork:101:t:", 200)
	var ids []string
	for _, item := range tasks {
		ids = append(ids, item.ID)
	}
	if err != nil || !reflect.DeepEqual(ids, []string{"t1", "t2"}) {
		t.Fatalf("task items: %v %v", ids, err)
	}
	if tasks, err = s.ChatworkTaskItems(ctx, "alice-id", "chatwork:101:t:", 1); err != nil || len(tasks) != 1 || tasks[0].ID != "t1" {
		t.Fatalf("limit: %+v %v", tasks, err)
	}
	if tasks, err = s.ChatworkTaskItems(ctx, "alice-id", "chatwork:101:t_", 200); err != nil || len(tasks) != 0 {
		t.Fatalf("wildcard prefix: %+v %v", tasks, err)
	}

	keys, err := s.ChatworkJudgedKeys(ctx, "alice-id", "chatwork:101:d:5:", 100, 500)
	if err != nil || !reflect.DeepEqual(keys, []string{"chatwork:101:d:5:3:4"}) {
		t.Fatalf("judged keys: %v %v", keys, err)
	}
	if keys, err = s.ChatworkJudgedKeys(ctx, "alice-id", "chatwork:101:d:%", 0, 500); err != nil || len(keys) != 0 {
		t.Fatalf("percent prefix: %v %v", keys, err)
	}
	if keys, err = s.ChatworkJudgedKeys(ctx, "bob-id", "chatwork:101:d:5:", 0, 500); err != nil || len(keys) != 0 {
		t.Fatalf("other user: %v %v", keys, err)
	}
}

// Disabling the user removes the Chatwork connection; a password reset
// keeps it.
func TestChangeUserChatwork(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			ctx := context.Background()
			s, _ := apiTokenFixture(t, driver)
			for _, u := range []string{"alice", "bob"} {
				saveChatwork(t, s, u+"-id", "1"+u[:1], store.ChatworkSettings{}, tokenTestNow)
			}
			if err := s.ChangeUser(ctx, "alice@example.com", "new", false, tokenTestNow); err != nil {
				t.Fatal(err)
			}
			if _, err := s.ChatworkConnection(ctx, "alice-id"); err != nil {
				t.Fatalf("after reset: %v", err)
			}
			if err := s.ChangeUser(ctx, "bob@example.com", "", true, tokenTestNow); err != nil {
				t.Fatal(err)
			}
			if _, err := s.ChatworkConnection(ctx, "bob-id"); !errors.Is(err, store.ErrConnectionNotFound) {
				t.Fatalf("after disable: %v", err)
			}
		})
	}
}
