package store_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/i2bskn/astovia/internal/store"
	"github.com/i2bskn/astovia/internal/testutil"
)

func TestActionItems(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) { testActionItems(t, driver) })
	}
}

func testActionItems(t *testing.T, driver string) {
	ctx := context.Background()
	s, _ := testutil.Database(t, driver)
	for _, u := range []store.User{{ID: "alice-id", Email: "alice@example.com", PasswordHash: "x"}, {ID: "bob-id", Email: "bob@example.com", PasswordHash: "x"}} {
		if err := s.CreateUser(ctx, u); err != nil {
			t.Fatal(err)
		}
	}

	// Round trip with and without the nullable columns.
	plain := store.ActionItem{ID: "plain", UserID: "alice-id", Content: "reply", Status: "open", Refs: "[]", Version: 1, CreatedAt: 100, UpdatedAt: 100}
	full := store.ActionItem{ID: "full", UserID: "alice-id", Content: "call", Status: "done", Note: sql.NullString{String: "note", Valid: true}, DueAt: sql.NullInt64{Int64: 1700000000, Valid: true}, Refs: `[{"service":"slack"}]`, Version: 1, CreatedAt: 101, UpdatedAt: 101}
	for _, item := range []store.ActionItem{plain, full} {
		if err := s.CreateActionItem(ctx, item); err != nil {
			t.Fatal(err)
		}
		got, err := s.ActionItem(ctx, "alice-id", item.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, item) {
			t.Fatalf("round trip:\ngot  %+v\nwant %+v", got, item)
		}
	}
	list, err := s.ListActionItems(ctx, "alice-id", "", nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(list, []store.ActionItem{full, plain}) {
		t.Fatalf("list: %+v", list)
	}

	// Another user's items are invisible and cannot be updated.
	bobs := store.ActionItem{ID: "bobs", UserID: "bob-id", Content: "secret", Status: "open", Refs: "[]", Version: 1, CreatedAt: 102, UpdatedAt: 102}
	if err = s.CreateActionItem(ctx, bobs); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ActionItem(ctx, "alice-id", "bobs"); !errors.Is(err, store.ErrItemNotFound) {
		t.Fatalf("other user's item: %v", err)
	}
	called := false
	if _, err = s.UpdateActionItem(ctx, "alice-id", "bobs", func(*store.ActionItem) error { called = true; return nil }); !errors.Is(err, store.ErrItemNotFound) || called {
		t.Fatalf("update of other user's item: %v called=%v", err, called)
	}
	if list, err = s.ListActionItems(ctx, "alice-id", "", nil, 10); err != nil || len(list) != 2 {
		t.Fatalf("alice sees %d items: %v", len(list), err)
	}
	if list, err = s.ListActionItems(ctx, "bob-id", "", nil, 10); err != nil || len(list) != 1 || list[0].ID != "bobs" {
		t.Fatalf("bob's list: %+v %v", list, err)
	}

	// UpdateActionItem increments the version and keeps the identity columns.
	updated, err := s.UpdateActionItem(ctx, "alice-id", "plain", func(item *store.ActionItem) error {
		item.Content, item.Status, item.UpdatedAt = "replied", "done", 200
		item.ID, item.UserID, item.CreatedAt, item.Version = "other", "bob-id", 1, 99
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := plain
	want.Content, want.Status, want.UpdatedAt, want.Version = "replied", "done", 200, 2
	if !reflect.DeepEqual(updated, want) {
		t.Fatalf("update:\ngot  %+v\nwant %+v", updated, want)
	}
	if got, err := s.ActionItem(ctx, "alice-id", "plain"); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("stored update: %+v %v", got, err)
	}
	// An error from fn is returned and nothing is stored.
	stop := errors.New("stop")
	if _, err = s.UpdateActionItem(ctx, "alice-id", "plain", func(item *store.ActionItem) error { item.Content = "changed"; return stop }); !errors.Is(err, stop) {
		t.Fatalf("fn error: %v", err)
	}
	if got, err := s.ActionItem(ctx, "alice-id", "plain"); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("rolled back update: %+v %v", got, err)
	}
	if _, err = s.UpdateActionItem(ctx, "alice-id", "missing", func(*store.ActionItem) error { return nil }); !errors.Is(err, store.ErrItemNotFound) {
		t.Fatalf("missing item: %v", err)
	}
}

func TestActionItemPaging(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) { testActionItemPaging(t, driver) })
	}
}

func testActionItemPaging(t *testing.T, driver string) {
	ctx := context.Background()
	s, _ := testutil.Database(t, driver)
	if err := s.CreateUser(ctx, store.User{ID: "alice-id", Email: "alice@example.com", PasswordHash: "x"}); err != nil {
		t.Fatal(err)
	}
	created := map[string]map[string]bool{"": {}, "open": {}, "done": {}}
	for i := 0; i < 13; i++ {
		status := "open"
		if i%3 == 0 {
			status = "done"
		}
		// Several items share each created_at so that ties span pages.
		item := store.ActionItem{ID: fmt.Sprintf("item-%02d", i), UserID: "alice-id", Content: "c", Status: status, Refs: "[]", Version: 1, CreatedAt: int64(1000 + i/4), UpdatedAt: 1}
		if err := s.CreateActionItem(ctx, item); err != nil {
			t.Fatal(err)
		}
		created[""][item.ID] = true
		created[status][item.ID] = true
	}
	for status, want := range created {
		seen := map[string]bool{}
		var after *store.ActionItemCursor
		last := int64(1 << 62)
		for page := 0; ; page++ {
			if page > 20 {
				t.Fatalf("status %q: paging does not end", status)
			}
			items, err := s.ListActionItems(ctx, "alice-id", status, after, 3)
			if err != nil {
				t.Fatal(err)
			}
			for _, item := range items {
				if seen[item.ID] {
					t.Fatalf("status %q: %s listed twice", status, item.ID)
				}
				if item.CreatedAt > last {
					t.Fatalf("status %q: created_at increased at %s", status, item.ID)
				}
				if status != "" && item.Status != status {
					t.Fatalf("status %q: got %s", status, item.Status)
				}
				seen[item.ID] = true
				last = item.CreatedAt
			}
			if len(items) < 3 {
				break
			}
			end := items[len(items)-1]
			after = &store.ActionItemCursor{CreatedAt: end.CreatedAt, ID: end.ID}
		}
		if !reflect.DeepEqual(seen, want) {
			t.Fatalf("status %q: listed %v, want %v", status, seen, want)
		}
	}
}
