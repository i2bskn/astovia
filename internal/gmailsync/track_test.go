package gmailsync

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/i2bskn/astovia/internal/store"
)

// detected makes a task of a new thread and returns the thread and the key.
func (st *syncTest) detected(subject string) (string, string) {
	st.t.Helper()
	th := st.recv("", subject, "お願いします", time.Minute)
	st.ai.set(aiAction(subject + "に返事する"))
	st.sync(me)
	key := runKey(me, th, th, th)
	st.mustItem(key)
	return th, key
}

func TestTrackResolve(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		st := newSyncTest(t, driver)
		th, key := st.detected("見積もり")
		if len(st.pending()) != 1 {
			t.Fatalf("pending: %+v", st.pending())
		}
		st.advance(2 * time.Minute)
		st.sent(th, "送りました", time.Minute)
		st.advance(5 * time.Minute)
		st.ai.set(aiResolved(true))
		st.sync(me)
		item := st.mustItem(key)
		if item.Status != "done" || item.AutoReason.String != ReasonReplyResolved || item.AutoRef.String != threadURL(me, th) || len(st.pending()) != 0 {
			t.Fatalf("resolved: %+v %+v", item, st.pending())
		}
		in := st.ai.input(t, 1)
		posts := st.ai.posts(t, 1)
		if in["_system"] == nil || in["task"] != "見積もりに返事する" || len(posts) != 2 || posts[0]["id"] != th || posts[1]["is_me"] != true {
			t.Fatalf("reply input: %v", in)
		}
		if m := in["me"].(map[string]any); m["id"] != me || m["name"] != "Alice Liddell" {
			t.Fatalf("me: %v", m)
		}
	})
}

func TestTrackTouch(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		st := newSyncTest(t, driver)
		th, key := st.detected("見積もり")
		// Without a message of the user, the item is only touched.
		st.advance(6 * time.Minute)
		a := st.sync(me)
		item := st.mustItem(key)
		if item.Status != "open" || item.TrackedAt.Int64 != st.now.Unix() || item.Version != 1 || st.ai.count() != 1 || a.LastTrackedAt.Int64 != st.now.Unix() {
			t.Fatalf("touched: %+v %d", item, st.ai.count())
		}
		// A draft is not a reply.
		st.g.add(gMsg{Thread: th, Labels: []string{"DRAFT"}, At: st.now.Add(time.Second), From: me, To: "bob@example.com", Subject: "Re", Body: "下書き"})
		st.advance(6 * time.Minute)
		st.sync(me)
		if item = st.mustItem(key); item.Status != "open" || st.ai.count() != 1 {
			t.Fatalf("draft: %+v %d", item, st.ai.count())
		}
		// A reply judged not to resolve touches the item.
		st.advance(6 * time.Minute)
		st.sent(th, "確認します", time.Minute)
		st.ai.set(aiResolved(false))
		st.sync(me)
		if item = st.mustItem(key); item.Status != "open" || item.TrackedAt.Int64 != st.now.Unix() || st.ai.count() != 1 {
			t.Fatalf("not resolved: %+v %d", item, st.ai.count())
		}
	})
}

func TestTrackUntrack(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		st := newSyncTest(t, driver)
		th, key := st.detected("消える")
		// A key of the account that is not a run is untracked.
		a := st.account(me)
		if err := st.s.GmailSync(context.Background(), "alice-id", a.ID, a.Generation, func(tx *store.GmailTx) error {
			_, e := tx.CreateItem(store.ActionItem{ID: "bad", Content: "x", Status: "open", Refs: "[]", Version: 1, CreatedAt: 1, UpdatedAt: 1, Source: sql.NullString{String: store.SourceGmail, Valid: true},
				SourceKey: sql.NullString{String: "gmail:" + me + ":r:zz", Valid: true}, Tracking: 1})
			return e
		}); err != nil {
			t.Fatal(err)
		}
		st.g.set(func(f *fakeGoogle) { f.statuses["threads:"+th] = 404 })
		st.advance(6 * time.Minute)
		st.sync(me)
		if item := st.mustItem(key); item.Tracking != 0 || item.Status != "open" {
			t.Fatalf("404: %+v", item)
		}
		if item := st.mustItem("gmail:" + me + ":r:zz"); item.Tracking != 0 {
			t.Fatalf("bad key: %+v", item)
		}
	})
}

// Another account's items, even many, do not keep this account's items
// from being tracked.
func TestTrackPerAccount(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		st := newSyncTest(t, driver)
		other := st.connect("second@example.com")
		if err := st.s.GmailSync(context.Background(), "alice-id", other.ID, other.Generation, func(tx *store.GmailTx) error {
			for i := 0; i < trackBatch+5; i++ {
				id := fmt.Sprintf("0-other-%03d", i)
				if _, e := tx.CreateItem(store.ActionItem{ID: id, Content: "x", Status: "open", Refs: "[]", Version: 1, CreatedAt: 1, UpdatedAt: 1, Source: sql.NullString{String: store.SourceGmail, Valid: true},
					SourceKey: sql.NullString{String: runKey("second@example.com", "18a0", fmt.Sprintf("%x", 0x100+i), fmt.Sprintf("%x", 0x100+i)), Valid: true}, Tracking: 1}); e != nil {
					return e
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		th, key := st.detected("見積もり")
		st.advance(6 * time.Minute)
		st.sent(th, "送りました", 30*time.Second)
		st.ai.set(aiResolved(true))
		st.sync(me)
		if item := st.mustItem(key); item.Status != "done" {
			t.Fatalf("not tracked: %+v", item)
		}
		// The other account's items were not touched by this account.
		item, err := st.s.ActionItem(context.Background(), "alice-id", "0-other-000")
		if err != nil || item.TrackedAt.Valid || item.Tracking != 1 {
			t.Fatalf("other account's item: %+v %v", item, err)
		}
	})
}
