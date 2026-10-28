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

func slackItem(id, key string, created int64) store.ActionItem {
	return store.ActionItem{ID: id, Content: "Slack: " + id, Status: "open", Refs: "[]", Version: 1, CreatedAt: created, UpdatedAt: created,
		Source: sql.NullString{String: store.SourceSlack, Valid: true}, SourceKey: sql.NullString{String: key, Valid: true}, Tracking: 1, DetectedAt: sql.NullInt64{Int64: created, Valid: true}}
}

func saveSlack(t *testing.T, s *store.Store, userID, team, slackUser string, settings store.SlackSettings, now int64) store.SlackConnection {
	t.Helper()
	c, err := s.SaveSlackConnection(context.Background(), store.SlackConnection{UserID: userID, TokenSealed: "sealed-" + team, TokenLast4: "abcd", TeamID: team, TeamName: "Team " + team, TeamURL: "https://x.slack.com/",
		SlackUserID: slackUser, SlackUserName: "name-" + slackUser, Scopes: "im:history,search:read"}, settings, userID[:len(userID)-3]+"-session", now)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func boolPtr(v bool) *bool { return &v }

func TestSlackConnections(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) { testSlackConnections(t, driver) })
	}
}

func testSlackConnections(t *testing.T, driver string) {
	ctx := context.Background()
	s, _ := apiTokenFixture(t, driver)
	if _, err := s.SlackConnection(ctx, "alice-id"); !errors.Is(err, store.ErrConnectionNotFound) {
		t.Fatalf("missing: %v", err)
	}
	c := saveSlack(t, s, "alice-id", "T1", "U1", store.SlackSettings{}, tokenTestNow)
	if c.Generation != 1 || c.Status != store.SlackActive || c.ImmediateNotify != 1 || c.DetectMentions != 1 || c.DetectDMs != 1 || c.DetectGroupDMs != 0 ||
		c.ConnectedAt != tokenTestNow || c.CreatedAt != tokenTestNow || c.NextPollAt != tokenTestNow || c.LastSyncedAt.Valid || c.TeamURL != "https://x.slack.com/" {
		t.Fatalf("new connection: %+v", c)
	}
	if got, err := s.SlackConnection(ctx, "alice-id"); err != nil || !reflect.DeepEqual(got, c) {
		t.Fatalf("round trip: %+v %v", got, err)
	}

	// SaveState writes only the synchronization state.
	err := s.SlackSync(ctx, "alice-id", c.Generation, func(tx *store.SlackTx) error {
		if ok, e := tx.CreateItem(slackItem("s1", "slack:T1:m:C1:-:1.000001", 100)); e != nil || !ok {
			return fmt.Errorf("create: %v %v", ok, e)
		}
		state := tx.Conn
		state.FailureCount, state.LastError, state.LastSyncedAt, state.LastTrackedAt, state.NextPollAt = 2, sql.NullString{String: "unavailable", Valid: true}, sql.NullInt64{Int64: 500, Valid: true}, sql.NullInt64{Int64: 400, Valid: true}, 900
		state.ImmediateNotify, state.DetectMentions, state.DetectGroupDMs, state.ConnectedAt, state.TeamID = 0, 0, 1, 1, "ignored"
		return tx.SaveState(state)
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.SlackConnection(ctx, "alice-id")
	if err != nil || got.FailureCount != 2 || got.LastError.String != "unavailable" || got.LastSyncedAt.Int64 != 500 || got.LastTrackedAt.Int64 != 400 || got.NextPollAt != 900 ||
		got.ImmediateNotify != 1 || got.DetectMentions != 1 || got.DetectGroupDMs != 0 || got.ConnectedAt != tokenTestNow || got.TeamID != "T1" {
		t.Fatalf("after SaveState: %+v %v", got, err)
	}
	called := false
	if err = s.SlackSync(ctx, "alice-id", c.Generation+1, func(*store.SlackTx) error { called = true; return nil }); !errors.Is(err, store.ErrStaleConnection) || called {
		t.Fatalf("other generation: %v %v", err, called)
	}
	if err = s.SlackSync(ctx, "bob-id", 1, func(*store.SlackTx) error { called = true; return nil }); !errors.Is(err, store.ErrStaleConnection) || called {
		t.Fatalf("missing connection: %v %v", err, called)
	}

	// PATCH merges the given settings.
	if c, err = s.UpdateSlackSettings(ctx, "alice-id", store.SlackSettings{DetectDMs: boolPtr(false), DetectGroupDMs: boolPtr(true)}, "alice-session", tokenTestNow+5); err != nil ||
		c.DetectDMs != 0 || c.DetectGroupDMs != 1 || c.DetectMentions != 1 || c.ImmediateNotify != 1 || c.UpdatedAt != tokenTestNow+5 || c.LastSyncedAt.Int64 != 500 {
		t.Fatalf("settings: %+v %v", c, err)
	}
	if got, err = s.SlackConnection(ctx, "alice-id"); err != nil || !reflect.DeepEqual(got, c) {
		t.Fatalf("settings stored: %+v %v", got, err)
	}
	if _, err = s.UpdateSlackSettings(ctx, "bob-id", store.SlackSettings{DetectDMs: boolPtr(false)}, "bob-session", tokenTestNow); !errors.Is(err, store.ErrConnectionNotFound) {
		t.Fatalf("settings without connection: %v", err)
	}

	// The same workspace and user: the settings, the creation time and the
	// tracking stay; the synchronization state and ConnectedAt are reset.
	c = saveSlack(t, s, "alice-id", "T1", "U1", store.SlackSettings{ImmediateNotify: boolPtr(false)}, tokenTestNow+10)
	if c.Generation != 2 || c.ImmediateNotify != 0 || c.DetectDMs != 0 || c.DetectGroupDMs != 1 || c.CreatedAt != tokenTestNow || c.ConnectedAt != tokenTestNow+10 ||
		c.LastSyncedAt.Valid || c.LastTrackedAt.Valid || c.FailureCount != 0 || c.LastError.Valid || c.NextPollAt != tokenTestNow+10 {
		t.Fatalf("re-registered: %+v", c)
	}
	if item, err := s.ActionItem(ctx, "alice-id", "s1"); err != nil || item.Tracking != 1 || !item.SourceKey.Valid {
		t.Fatalf("same account item: %+v %v", item, err)
	}
	// Another Slack user in the same workspace untracks the items.
	c = saveSlack(t, s, "alice-id", "T1", "U2", store.SlackSettings{}, tokenTestNow+20)
	if item, err := s.ActionItem(ctx, "alice-id", "s1"); err != nil || item.Tracking != 0 || item.SourceKey.Valid || item.Version != 2 {
		t.Fatalf("other user item: %+v %v", item, err)
	}
	// So does another workspace.
	err = s.SlackSync(ctx, "alice-id", c.Generation, func(tx *store.SlackTx) error {
		_, e := tx.CreateItem(slackItem("s2", "slack:T1:m:C1:-:2.000001", 200))
		return e
	})
	if err != nil {
		t.Fatal(err)
	}
	c = saveSlack(t, s, "alice-id", "T2", "U2", store.SlackSettings{}, tokenTestNow+30)
	if item, err := s.ActionItem(ctx, "alice-id", "s2"); err != nil || item.Tracking != 0 || item.SourceKey.Valid {
		t.Fatalf("other workspace item: %+v %v", item, err)
	}
	if due, err := s.DueSlackConnections(ctx, tokenTestNow+30, 10); err != nil || len(due) != 1 || due[0].UserID != "alice-id" {
		t.Fatalf("due: %+v %v", due, err)
	}
	if due, err := s.DueSlackConnections(ctx, tokenTestNow+29, 10); err != nil || len(due) != 0 {
		t.Fatalf("not due: %+v %v", due, err)
	}

	// Delete untracks, drops only Slack's pending detections and is
	// idempotent.
	err = s.SlackSync(ctx, "alice-id", c.Generation, func(tx *store.SlackTx) error {
		if _, e := tx.CreateItem(slackItem("s3", "slack:T2:m:C1:-:3.000001", 300)); e != nil {
			return e
		}
		return tx.Enqueue(notification("n-s3", store.NotificationDetected, "detected:s3:1", "s3", 300))
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
		if err = s.DeleteSlackConnection(ctx, "alice-id", "alice-session", tokenTestNow); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = s.SlackConnection(ctx, "alice-id"); !errors.Is(err, store.ErrConnectionNotFound) {
		t.Fatalf("deleted: %v", err)
	}
	if item, err := s.ActionItem(ctx, "alice-id", "s3"); err != nil || item.Tracking != 0 || item.SourceKey.Valid {
		t.Fatalf("deleted connection item: %+v %v", item, err)
	}
	if has, err := s.HasNotification(ctx, "alice-id", "detected:s3:1"); err != nil || has {
		t.Fatalf("Slack detection kept: %v %v", has, err)
	}
	if has, err := s.HasNotification(ctx, "alice-id", "detected:g1:1"); err != nil || !has {
		t.Fatalf("GitHub detection dropped: %v %v", has, err)
	}

	// A revoked session cannot write.
	if err = s.ChangeUser(ctx, "bob@example.com", "new", false, tokenTestNow); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SaveSlackConnection(ctx, store.SlackConnection{UserID: "bob-id", TeamID: "T1", SlackUserID: "U1"}, store.SlackSettings{}, "bob-session", tokenTestNow); !errors.Is(err, store.ErrUnauthorized) {
		t.Fatalf("revoked session: %v", err)
	}
	if _, err = s.UpdateSlackSettings(ctx, "bob-id", store.SlackSettings{DetectDMs: boolPtr(true)}, "bob-session", tokenTestNow); !errors.Is(err, store.ErrUnauthorized) {
		t.Fatalf("revoked session settings: %v", err)
	}
	if err = s.DeleteSlackConnection(ctx, "bob-id", "bob-session", tokenTestNow); !errors.Is(err, store.ErrUnauthorized) {
		t.Fatalf("revoked session delete: %v", err)
	}
}

func TestSlackKeyQueries(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) { testSlackKeyQueries(t, driver) })
	}
}

func testSlackKeyQueries(t *testing.T, driver string) {
	ctx := context.Background()
	s, _ := apiTokenFixture(t, driver)
	c := saveSlack(t, s, "alice-id", "T1", "U1", store.SlackSettings{}, tokenTestNow)
	items := []store.ActionItem{
		slackItem("a", "slack:T1:d:C1:-:1.000001:1.000002", 100),
		slackItem("b", "slack:T1:d:C1:-:2.000001:2.000003", 200),
		slackItem("c", "slack:T1:d:C1:-:3.000001:3.000001", 200),
		slackItem("old", "slack:T1:d:C1:-:0.000001:0.000001", 50),
		// Other conversations, other cases and LIKE wildcards do not match.
		slackItem("other-channel", "slack:T1:d:C10:-:1.000001:1.000001", 300),
		slackItem("wildcard", "slack:T1:d:CX:-:1.000001:1.000001", 300),
		slackItem("lower", "slack:t1:d:c1:-:1.000001:1.000001", 300),
		slackItem("thread", "slack:T1:d:C1:1.000000:1.000001:1.000001", 300),
	}
	done := slackItem("done", "slack:T1:d:C1:-:4.000001:4.000001", 400)
	done.Status, done.Tracking = "done", 0
	untracked := slackItem("untracked", "slack:T1:d:C1:-:5.000001:5.000001", 500)
	// Untracked open items (events) are listed; candidates are not.
	untracked.Tracking = 0
	candidate := slackItem("candidate", "slack:T1:d:C1:-:7.000001:7.000001", 550)
	candidate.Status = "candidate"
	items = append(items, done, untracked, candidate)
	err := s.SlackSync(ctx, "alice-id", c.Generation, func(tx *store.SlackTx) error {
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
	// A GitHub item with a Slack-like key is not a Slack item.
	gh := saveConnection(t, s, "alice-id", "octocat", nil)
	if err = s.GitHubSync(ctx, "alice-id", gh.Generation, func(tx *store.GitHubTx) error {
		_, e := tx.CreateItem(githubItem("gh", "slack:T1:d:C1:-:6.000001:6.000001", 600))
		return e
	}); err != nil {
		t.Fatal(err)
	}
	keys, err := s.SlackJudgedKeys(ctx, "alice-id", "slack:T1:d:C1:-:", 100, 500)
	want := []string{"slack:T1:d:C1:-:7.000001:7.000001", "slack:T1:d:C1:-:5.000001:5.000001", "slack:T1:d:C1:-:4.000001:4.000001", "slack:T1:d:C1:-:3.000001:3.000001", "slack:T1:d:C1:-:2.000001:2.000003", "slack:T1:d:C1:-:1.000001:1.000002"}
	if err != nil || !reflect.DeepEqual(keys, want) {
		t.Fatalf("judged keys: %v %v", keys, err)
	}
	if keys, err = s.SlackJudgedKeys(ctx, "alice-id", "slack:T1:d:C_:-:", 0, 500); err != nil || len(keys) != 0 {
		t.Fatalf("wildcard prefix: %v %v", keys, err)
	}
	if keys, err = s.SlackJudgedKeys(ctx, "alice-id", "slack:T1:d:C%", 0, 500); err != nil || len(keys) != 0 {
		t.Fatalf("percent prefix: %v %v", keys, err)
	}
	if keys, err = s.SlackJudgedKeys(ctx, "alice-id", "slack:T1:d:C1:-:", 0, 2); err != nil || len(keys) != 2 {
		t.Fatalf("limit: %v %v", keys, err)
	}
	if keys, err = s.SlackJudgedKeys(ctx, "bob-id", "slack:T1:d:C1:-:", 0, 500); err != nil || len(keys) != 0 {
		t.Fatalf("other user: %v %v", keys, err)
	}
	tasks, err := s.SlackOpenTasks(ctx, "alice-id", "slack:T1:d:C1:-:", 4)
	if err != nil || !reflect.DeepEqual(tasks, []string{"Slack: untracked", "Slack: c", "Slack: b", "Slack: a"}) {
		t.Fatalf("open tasks: %v %v", tasks, err)
	}
	if tasks, err = s.SlackOpenTasks(ctx, "alice-id", "slack:T9:", 3); err != nil || tasks == nil || len(tasks) != 0 {
		t.Fatalf("no open tasks: %#v %v", tasks, err)
	}
	// Tracked candidates are followed like open items.
	tracked, err := s.TrackedActionItems(ctx, "alice-id", store.SourceSlack, 100)
	found := false
	for _, item := range tracked {
		found = found || item.ID == "candidate"
	}
	if err != nil || !found {
		t.Fatalf("tracked candidates: %+v %v", tracked, err)
	}
}

// Disabling the user removes the Slack connection; a password reset keeps it.
func TestChangeUserSlack(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			ctx := context.Background()
			s, _ := apiTokenFixture(t, driver)
			for _, u := range []string{"alice", "bob"} {
				saveSlack(t, s, u+"-id", "T1", "U-"+u, store.SlackSettings{}, tokenTestNow)
			}
			if err := s.ChangeUser(ctx, "alice@example.com", "new", false, tokenTestNow); err != nil {
				t.Fatal(err)
			}
			if _, err := s.SlackConnection(ctx, "alice-id"); err != nil {
				t.Fatalf("after reset: %v", err)
			}
			if err := s.ChangeUser(ctx, "bob@example.com", "", true, tokenTestNow); err != nil {
				t.Fatal(err)
			}
			if _, err := s.SlackConnection(ctx, "bob-id"); !errors.Is(err, store.ErrConnectionNotFound) {
				t.Fatalf("after disable: %v", err)
			}
		})
	}
}
