package store_test

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"sync"
	"testing"

	"github.com/i2bskn/astovia/internal/store"
	"github.com/i2bskn/astovia/internal/testutil"
)

func nestingItem(id string, created int64) store.ActionItem {
	return store.ActionItem{ID: id, UserID: "alice-id", Content: "task " + id, Status: "open", Refs: "[]", Version: 1, CreatedAt: created, UpdatedAt: created}
}

func child(id, parent string, created int64) store.ActionItem {
	item := nestingItem(id, created)
	item.ParentID = sql.NullString{String: parent, Valid: true}
	return item
}

// nestingFixture creates alice and bob.
func nestingFixture(t *testing.T, driver string) (*store.Store, string) {
	t.Helper()
	s, dsn := testutil.Database(t, driver)
	for _, u := range []store.User{{ID: "alice-id", Email: "alice@example.com", PasswordHash: "x"}, {ID: "bob-id", Email: "bob@example.com", PasswordHash: "x"}} {
		if err := s.CreateUser(context.Background(), u); err != nil {
			t.Fatal(err)
		}
	}
	return s, dsn
}

func mustCreate(t *testing.T, s *store.Store, items ...store.ActionItem) {
	t.Helper()
	for _, item := range items {
		if err := s.CreateActionItem(context.Background(), item); err != nil {
			t.Fatalf("create %s: %v", item.ID, err)
		}
	}
}

func stored(t *testing.T, s *store.Store, id string) store.ActionItem {
	t.Helper()
	item, err := s.ActionItem(context.Background(), "alice-id", id)
	if err != nil {
		t.Fatalf("get %s: %v", id, err)
	}
	return item
}

// setStatus changes the status as the REST handlers do: a status chosen by
// the user clears the automatic completion.
func setStatus(t *testing.T, s *store.Store, id, status string, now int64) {
	t.Helper()
	if _, err := s.UpdateActionItem(context.Background(), "alice-id", id, func(item *store.ActionItem) error {
		item.Status, item.UpdatedAt = status, now
		item.AutoReason, item.AutoRef, item.AutoAt = sql.NullString{}, sql.NullString{}, sql.NullInt64{}
		return nil
	}); err != nil {
		t.Fatalf("status of %s: %v", id, err)
	}
}

func setParent(s *store.Store, id, parent string, now int64) error {
	_, err := s.UpdateActionItem(context.Background(), "alice-id", id, func(item *store.ActionItem) error {
		item.ParentID, item.UpdatedAt = sql.NullString{String: parent, Valid: parent != ""}, now
		return nil
	})
	return err
}

func TestParentRules(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			ctx := context.Background()
			s, _ := nestingFixture(t, driver)
			bobs := nestingItem("bobs", 1)
			bobs.UserID = "bob-id"
			mustCreate(t, s, nestingItem("p", 1), child("c", "p", 2), nestingItem("solo", 3), bobs)

			// Creating: a missing or another user's parent, a grandchild.
			for name, item := range map[string]store.ActionItem{"missing": child("x1", "nope", 4), "other user": child("x2", "bobs", 4), "grandchild": child("x3", "c", 4), "self": child("x4", "x4", 4)} {
				if err := s.CreateActionItem(ctx, item); !errors.Is(err, store.ErrInvalidParent) {
					t.Fatalf("create %s: %v", name, err)
				}
				if _, err := s.ActionItem(ctx, "alice-id", item.ID); !errors.Is(err, store.ErrItemNotFound) {
					t.Fatalf("create %s stored the item: %v", name, err)
				}
			}
			// Updating: the same, an item with subtasks and itself.
			for name, tc := range map[string]struct{ id, parent string }{
				"missing": {"solo", "nope"}, "other user": {"solo", "bobs"}, "grandchild": {"solo", "c"}, "has subtasks": {"p", "solo"}, "self": {"solo", "solo"},
			} {
				if err := setParent(s, tc.id, tc.parent, 5); !errors.Is(err, store.ErrInvalidParent) {
					t.Fatalf("update %s: %v", name, err)
				}
			}
			if got := stored(t, s, "solo"); got.ParentID.Valid || got.Version != 1 {
				t.Fatalf("rejected update stored: %+v", got)
			}
			// Bob cannot use alice's item as a parent either.
			other := child("bobs-child", "p", 6)
			other.UserID = "bob-id"
			if err := s.CreateActionItem(ctx, other); !errors.Is(err, store.ErrInvalidParent) {
				t.Fatalf("bob's child of alice's item: %v", err)
			}
			// A done parent takes subtasks; one set by hand stays done.
			setStatus(t, s, "solo", "done", 7)
			mustCreate(t, s, child("late", "solo", 8))
			if got := stored(t, s, "solo"); got.Status != "done" || got.AutoReason.Valid {
				t.Fatalf("parent done by hand: %+v", got)
			}
		})
	}
}

func TestSyncParent(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			s, _ := nestingFixture(t, driver)
			mustCreate(t, s, nestingItem("p", 1), child("a", "p", 2), child("b", "p", 3))

			// One subtask done: the parent stays open.
			setStatus(t, s, "a", "done", 10)
			if got := stored(t, s, "p"); got.Status != "open" || got.Version != 1 {
				t.Fatalf("one of two done: %+v", got)
			}
			// All done: the parent is completed automatically.
			setStatus(t, s, "b", "done", 11)
			p := stored(t, s, "p")
			if p.Status != "done" || p.AutoReason.String != store.ReasonChildrenDone || p.AutoRef.Valid || p.AutoAt.Int64 != 11 || p.Tracking != 0 || p.Version != 2 || p.UpdatedAt != 11 {
				t.Fatalf("completed: %+v", p)
			}
			// A subtask open again reopens it, clearing the automatic fields.
			setStatus(t, s, "b", "open", 12)
			p = stored(t, s, "p")
			if p.Status != "open" || p.AutoReason.Valid || p.AutoAt.Valid || p.Version != 3 {
				t.Fatalf("reopened: %+v", p)
			}
			// A new open subtask of an automatically completed parent reopens it.
			setStatus(t, s, "b", "done", 13)
			mustCreate(t, s, child("c", "p", 14))
			if p = stored(t, s, "p"); p.Status != "open" {
				t.Fatalf("new subtask: %+v", p)
			}
			setStatus(t, s, "c", "dismissed", 15)
			if p = stored(t, s, "p"); p.Status != "done" || p.AutoReason.String != store.ReasonChildrenDone {
				t.Fatalf("dismissed counts as settled: %+v", p)
			}

			// Reopened by hand (from /auto): it is not completed again at once.
			setStatus(t, s, "p", "open", 16)
			if p = stored(t, s, "p"); p.Status != "open" || p.AutoReason.Valid {
				t.Fatalf("reopened by hand: %+v", p)
			}
			// Completed by hand: a subtask reopened does not reopen it.
			setStatus(t, s, "p", "done", 17)
			setStatus(t, s, "a", "open", 18)
			if p = stored(t, s, "p"); p.Status != "done" || p.AutoReason.Valid {
				t.Fatalf("done by hand: %+v", p)
			}

			// Every subtask dismissed: the parent is not completed.
			mustCreate(t, s, nestingItem("q", 20), child("q1", "q", 21))
			setStatus(t, s, "q1", "dismissed", 22)
			if got := stored(t, s, "q"); got.Status != "open" {
				t.Fatalf("all dismissed: %+v", got)
			}
			// A candidate or dismissed parent is left alone.
			for _, status := range []string{"candidate", "dismissed"} {
				id := "r-" + status
				mustCreate(t, s, nestingItem(id, 30), child(id+"1", id, 31))
				setStatus(t, s, id, status, 32)
				setStatus(t, s, id+"1", "done", 33)
				if got := stored(t, s, id); got.Status != status {
					t.Fatalf("%s parent: %+v", status, got)
				}
			}
		})
	}
}

func TestMoveSubtask(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			ctx := context.Background()
			s, _ := nestingFixture(t, driver)
			mustCreate(t, s, nestingItem("old", 1), nestingItem("new", 2), child("done1", "old", 3), child("moving", "old", 4), child("done2", "new", 5), child("open2", "new", 6))
			setStatus(t, s, "done1", "done", 7)
			setStatus(t, s, "done2", "done", 8)
			// Moving the open subtask completes the old parent; the new one
			// stays open.
			if err := setParent(s, "moving", "new", 9); err != nil {
				t.Fatal(err)
			}
			if got := stored(t, s, "old"); got.Status != "done" || got.AutoReason.String != store.ReasonChildrenDone {
				t.Fatalf("old parent: %+v", got)
			}
			if got := stored(t, s, "moving"); got.ParentID.String != "new" || got.Version != 2 {
				t.Fatalf("moved: %+v", got)
			}
			// Taking the open subtasks out of the new parent completes it, and
			// moving one back reopens the old one.
			if err := setParent(s, "open2", "", 10); err != nil {
				t.Fatal(err)
			}
			if err := setParent(s, "moving", "old", 11); err != nil {
				t.Fatal(err)
			}
			if got := stored(t, s, "new"); got.Status != "done" {
				t.Fatalf("new parent: %+v", got)
			}
			if got := stored(t, s, "old"); got.Status != "open" || got.AutoReason.Valid {
				t.Fatalf("old parent reopened: %+v", got)
			}

			counts, err := s.ChildCounts(ctx, "alice-id", []string{"old", "new", "open2", "missing"})
			want := map[string]store.ChildCount{
				"old": {ParentID: "old", Children: 2, Total: 2, Done: 1, Remaining: 1},
				"new": {ParentID: "new", Children: 1, Total: 1, Done: 1, Remaining: 0},
			}
			if err != nil || !reflect.DeepEqual(counts, want) {
				t.Fatalf("counts: %+v %v", counts, err)
			}
			setStatus(t, s, "moving", "dismissed", 12)
			if counts, err = s.ChildCounts(ctx, "alice-id", []string{"old"}); err != nil || counts["old"] != (store.ChildCount{ParentID: "old", Children: 2, Total: 1, Done: 1, Remaining: 0}) {
				t.Fatalf("dismissed left out of total: %+v %v", counts, err)
			}
			if counts, err = s.ChildCounts(ctx, "bob-id", []string{"old"}); err != nil || len(counts) != 0 {
				t.Fatalf("other user's counts: %+v %v", counts, err)
			}
			if counts, err = s.ChildCounts(ctx, "alice-id", nil); err != nil || len(counts) != 0 {
				t.Fatalf("no IDs: %+v %v", counts, err)
			}
			sums, err := s.ItemSummaries(ctx, "alice-id", []string{"old", "new", "missing", "old"})
			if err != nil || !reflect.DeepEqual(sums, map[string]store.ItemSummary{"old": {ID: "old", Content: "task old", Status: "done"}, "new": {ID: "new", Content: "task new", Status: "done"}}) {
				t.Fatalf("summaries: %+v %v", sums, err)
			}
			if sums, err = s.ItemSummaries(ctx, "bob-id", []string{"old"}); err != nil || len(sums) != 0 {
				t.Fatalf("other user's summaries: %+v %v", sums, err)
			}
			children, err := s.ChildActionItems(ctx, "alice-id", "old", 100)
			if err != nil || len(children) != 2 || children[0].ID != "done1" || children[1].ID != "moving" {
				t.Fatalf("children: %+v %v", children, err)
			}
		})
	}
}

func TestSweepCompletesParent(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			ctx := context.Background()
			s, dsn := nestingFixture(t, driver)
			candidate := child("cand", "p", 2)
			candidate.Status = "candidate"
			event := child("event", "q", 4)
			event.Kind, event.EventTitle, event.EventStartAt = store.KindEvent, sql.NullString{String: "MTG", Valid: true}, sql.NullInt64{Int64: 500, Valid: true}
			mustCreate(t, s, nestingItem("p", 1), candidate, child("pdone", "p", 3), nestingItem("q", 1), event, child("qdone", "q", 5))
			setStatus(t, s, "pdone", "done", 6)
			setStatus(t, s, "qdone", "done", 6)

			// A failure while synchronizing a parent rolls back the sweep.
			raw, err := sql.Open(map[string]string{"sqlite": "sqlite", "postgres": "pgx"}[driver], dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = raw.Close() }()
			create, drop := []string{`CREATE TRIGGER fail_parent BEFORE UPDATE ON action_items WHEN NEW.auto_reason = 'children.done' BEGIN SELECT RAISE(ABORT, 'boom'); END`}, []string{`DROP TRIGGER fail_parent`}
			if driver == "postgres" {
				create = []string{`CREATE FUNCTION fail_parent() RETURNS trigger AS $$ BEGIN RAISE EXCEPTION 'boom'; END $$ LANGUAGE plpgsql`,
					`CREATE TRIGGER fail_parent BEFORE UPDATE ON action_items FOR EACH ROW WHEN (NEW.auto_reason = 'children.done') EXECUTE FUNCTION fail_parent()`}
				drop = []string{`DROP TRIGGER fail_parent ON action_items`, `DROP FUNCTION fail_parent()`}
			}
			for _, q := range create {
				if _, err = raw.ExecContext(ctx, q); err != nil {
					t.Fatal(err)
				}
			}
			if _, err = s.ExpireCandidates(ctx, "alice-id", 100, 1000); err == nil {
				t.Fatal("sweep did not fail")
			}
			if got := stored(t, s, "cand"); got.Status != "candidate" {
				t.Fatalf("failed sweep kept the dismissal: %+v", got)
			}
			for _, q := range drop {
				if _, err = raw.ExecContext(ctx, q); err != nil {
					t.Fatal(err)
				}
			}

			if n, err := s.ExpireCandidates(ctx, "alice-id", 100, 1000); err != nil || n != 1 {
				t.Fatalf("expire candidates: %d %v", n, err)
			}
			if got := stored(t, s, "p"); got.Status != "done" || got.AutoReason.String != store.ReasonChildrenDone || got.AutoAt.Int64 != 1000 {
				t.Fatalf("parent after candidate sweep: %+v", got)
			}
			if n, err := s.ExpireEvents(ctx, "alice-id", 1000); err != nil || n != 1 {
				t.Fatalf("expire events: %d %v", n, err)
			}
			if got := stored(t, s, "q"); got.Status != "done" || got.AutoReason.String != store.ReasonChildrenDone {
				t.Fatalf("parent after event sweep: %+v", got)
			}
		})
	}
}

func TestSourceTxNesting(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			ctx := context.Background()
			s, _ := apiTokenFixture(t, driver)
			c := saveSlack(t, s, "alice-id", "T1", "U1", store.SlackSettings{}, tokenTestNow)
			run := func(fn func(tx *store.SlackTx) error) error {
				return s.SlackSync(ctx, "alice-id", c.Generation, fn)
			}
			task := slackItem("task", "slack:T1:m:C1:1.000001:1.000002", 100)
			if err := run(func(tx *store.SlackTx) error { _, e := tx.CreateItem(task); return e }); err != nil {
				t.Fatal(err)
			}
			group := slackItem("group", "slack:T1:g:C1:1.000001:new", 200)
			group.Tracking = 0
			grouped := slackItem("new", "slack:T1:m:C1:1.000001:1.000003", 200)
			grouped.ParentID = sql.NullString{String: "group", Valid: true}
			err := run(func(tx *store.SlackTx) error {
				if created, e := tx.CreateGroup(group); e != nil || !created {
					return errors.Join(e, errors.New("group not created"))
				}
				if e := tx.SetParent("task", "group", 200); e != nil {
					return e
				}
				if n, e := tx.ChildCount("group"); e != nil || n != 1 {
					return errors.Join(e, errors.New("child count"))
				}
				created, e := tx.CreateItem(grouped)
				if e != nil || !created {
					return errors.Join(e, errors.New("grouped item not created"))
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if got := stored(t, s, "task"); got.ParentID.String != "group" || got.Version != 2 || got.UpdatedAt != 200 {
				t.Fatalf("task moved under the group: %+v", got)
			}
			// SetParent checks the rules: no grandchild, no parent for a group.
			err = run(func(tx *store.SlackTx) error { return tx.SetParent("group", "task", 300) })
			if !errors.Is(err, store.ErrInvalidParent) {
				t.Fatalf("group under its child: %v", err)
			}
			// UpdateItem keeps the parent and synchronizes it: the group is
			// completed when both subtasks are done.
			for _, id := range []string{"task", "new"} {
				err = run(func(tx *store.SlackTx) error {
					cur, _, e := tx.LockItem(id)
					if e != nil {
						return e
					}
					next := cur
					next.Status, next.UpdatedAt, next.ParentID = "done", 400, sql.NullString{}
					_, _, e = tx.UpdateItem(cur, next)
					return e
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			if got := stored(t, s, "new"); got.ParentID.String != "group" {
				t.Fatalf("UpdateItem changed the parent: %+v", got)
			}
			if got := stored(t, s, "group"); got.Status != "done" || got.AutoReason.String != store.ReasonChildrenDone {
				t.Fatalf("group: %+v", got)
			}
			// A subtask created by the integration reopens the group.
			late := slackItem("late", "slack:T1:m:C1:1.000001:1.000009", 500)
			late.ParentID = sql.NullString{String: "group", Valid: true}
			if err = run(func(tx *store.SlackTx) error { _, e := tx.CreateItem(late); return e }); err != nil {
				t.Fatal(err)
			}
			if got := stored(t, s, "group"); got.Status != "open" || got.AutoReason.Valid {
				t.Fatalf("group reopened: %+v", got)
			}
		})
	}
}

func TestRelatedTasks(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			ctx := context.Background()
			s, _ := apiTokenFixture(t, driver)
			c := saveSlack(t, s, "alice-id", "T1", "U1", store.SlackSettings{}, tokenTestNow)
			items := []store.ActionItem{
				slackItem("plain", "slack:T1:m:C1:9.000001:9.000002", 100),
				slackItem("group", "slack:T1:g:C1:9.000001:x", 110),
				slackItem("sub1", "slack:T1:m:C1:9.000001:9.000003", 120),
				slackItem("sub2", "slack:T1:m:C1:9.000001:9.000004", 130),
				slackItem("subdone", "slack:T1:m:C1:9.000001:9.000005", 135),
				slackItem("root", "slack:T1:m:C1:-:9.000001", 90),
				slackItem("dm", "slack:T1:d:C1:9.000001:9.000006:9.000007", 140),
				slackItem("other", "slack:T1:m:C2:9.000001:9.000008", 150),
				slackItem("manualdone", "slack:T1:g:C1:9.000001:y", 160),
			}
			for _, i := range []int{2, 3, 4} {
				items[i].ParentID = sql.NullString{String: "group", Valid: true}
			}
			items[4].Status = "done"
			items[8].Status = "done"
			err := s.SlackSync(ctx, "alice-id", c.Generation, func(tx *store.SlackTx) error {
				for _, item := range items {
					if item.ParentID.Valid {
						continue
					}
					if _, e := tx.CreateItem(item); e != nil {
						return e
					}
				}
				for _, item := range items {
					if !item.ParentID.Valid {
						continue
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
			scope := store.RelatedScope{
				Source:        store.SourceSlack,
				Prefixes:      []string{"slack:T1:m:C1:9.000001:", "slack:T1:d:C1:9.000001:", "slack:T1:g:C1:9.000001:"},
				ExactKeys:     []string{"slack:T1:m:C1:-:9.000001"},
				GroupPrefixes: []string{"slack:T1:g:C1:9.000001:"},
				FetchLimit:    30, Limit: 10, SubtaskLimit: 1,
			}
			got, err := s.RelatedTasks(ctx, "alice-id", scope)
			want := []store.RelatedTask{
				{ID: "dm", Content: "Slack: dm"},
				{ID: "group", Content: "Slack: group", Subtasks: []string{"Slack: sub2"}, Group: true},
				{ID: "plain", Content: "Slack: plain"},
				{ID: "root", Content: "Slack: root"},
			}
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("related:\ngot  %+v\nwant %+v %v", got, want, err)
			}
			scope.Limit = 2
			if got, err = s.RelatedTasks(ctx, "alice-id", scope); err != nil || len(got) != 2 || got[1].ID != "group" {
				t.Fatalf("limit: %+v %v", got, err)
			}
			// Items of a direct message conversation outside threads are
			// limited by age; a subtask whose group is left out is listed on
			// its own.
			scope.Limit, scope.DMSince = 10, 125
			if got, err = s.RelatedTasks(ctx, "alice-id", scope); err != nil || !reflect.DeepEqual(got, []store.RelatedTask{{ID: "dm", Content: "Slack: dm"}, {ID: "sub2", Content: "Slack: sub2"}}) {
				t.Fatalf("since: %+v %v", got, err)
			}

			// A group completed by its subtasks is kept, closed, with its
			// latest subtasks; one completed by hand is not.
			scope.DMSince = 0
			for _, id := range []string{"sub1", "sub2"} {
				setStatus(t, s, id, "done", tokenTestNow)
			}
			if g := stored(t, s, "group"); g.Status != "done" || g.AutoReason.String != store.ReasonChildrenDone {
				t.Fatalf("group: %+v", g)
			}
			scope.SubtaskLimit, scope.ClosedSince = 5, tokenTestNow
			got, err = s.RelatedTasks(ctx, "alice-id", scope)
			if err != nil || len(got) != 4 || !got[1].Closed || !reflect.DeepEqual(got[1].Subtasks, []string{"Slack: sub1", "Slack: sub2", "Slack: subdone"}) {
				t.Fatalf("closed group: %+v %v", got, err)
			}
			scope.ClosedSince = tokenTestNow + 1
			if got, err = s.RelatedTasks(ctx, "alice-id", scope); err != nil || len(got) != 3 {
				t.Fatalf("old closed group: %+v %v", got, err)
			}
		})
	}
}

// Concurrent changes cannot nest three levels, and two subtasks done at the
// same time complete their parent once. The barrier holds both transactions
// after they locked their own item, so that each reaches the parent while
// the other is still open.
func TestNestingConcurrency(t *testing.T) {
	ctx := context.Background()
	s, _ := nestingFixture(t, "postgres")
	barrier := func(n int) func() {
		var wg sync.WaitGroup
		wg.Add(n)
		return func() { wg.Done(); wg.Wait() }
	}
	update := func(id string, wait func(), fn func(*store.ActionItem)) <-chan error {
		done := make(chan error, 1)
		go func() {
			_, err := s.UpdateActionItem(ctx, "alice-id", id, func(item *store.ActionItem) error {
				wait()
				fn(item)
				return nil
			})
			done <- err
		}()
		return done
	}
	for round := 0; round < 5; round++ {
		p, x, y := "p"+string(rune('a'+round)), "x"+string(rune('a'+round)), "y"+string(rune('a'+round))
		mustCreate(t, s, nestingItem(p, 1), nestingItem(x, 2), nestingItem(y, 3))
		wait := barrier(2)
		a := update(x, wait, func(item *store.ActionItem) { item.ParentID = sql.NullString{String: p, Valid: true} })
		b := update(y, wait, func(item *store.ActionItem) { item.ParentID = sql.NullString{String: x, Valid: true} })
		ea, eb := <-a, <-b
		if (ea == nil) == (eb == nil) || (ea != nil && !errors.Is(ea, store.ErrInvalidParent)) || (eb != nil && !errors.Is(eb, store.ErrInvalidParent)) {
			t.Fatalf("round %d: %v %v", round, ea, eb)
		}
		if px, py := stored(t, s, x), stored(t, s, y); px.ParentID.Valid && py.ParentID.Valid {
			t.Fatalf("round %d: three levels", round)
		}
	}
	for round := 0; round < 5; round++ {
		p := "q" + string(rune('a'+round))
		mustCreate(t, s, nestingItem(p, 1), child(p+"1", p, 2), child(p+"2", p, 3))
		wait := barrier(2)
		done := func(item *store.ActionItem) { item.Status = "done" }
		a, b := update(p+"1", wait, done), update(p+"2", wait, done)
		if ea, eb := <-a, <-b; ea != nil || eb != nil {
			t.Fatalf("round %d: %v %v", round, ea, eb)
		}
		if got := stored(t, s, p); got.Status != "done" || got.Version != 2 {
			t.Fatalf("round %d: parent %+v", round, got)
		}
	}
}
