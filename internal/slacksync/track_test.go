package slacksync

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/i2bskn/astovia/internal/ai"
	"github.com/i2bskn/astovia/internal/store"
)

// trackedMention creates the open item of a mention of alice in C1 by a
// synchronization and returns its key.
func (st *syncTest) trackedMention() (string, string) {
	st.t.Helper()
	st.advance(time.Minute)
	ts := st.ts(30*time.Second, 1)
	st.sl.post("C1", msg{TS: ts, User: "U2", Text: "<@U1> send me the report"})
	st.ai.set(aiAction("レポートを送る"))
	st.sync()
	key := mentionKey("C1", "-", ts)
	st.mustItem(key)
	st.sl.reset()
	return key, ts
}

// reply posts alice's reply in the thread of ts and marks ts as its parent.
func (st *syncTest) reply(channel, parent, text string, ago time.Duration) string {
	st.t.Helper()
	ts := st.ts(ago, 7)
	st.sl.set(func(f *fakeSlack) {
		for i, m := range f.channels[channel] {
			if m.TS == parent {
				f.channels[channel][i].ThreadTS = parent
			}
		}
		f.channels[channel] = append(f.channels[channel], msg{TS: ts, User: me, Text: text, ThreadTS: parent})
	})
	return ts
}

func TestTrackResolved(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		st := newSyncTest(t, driver)
		key, origin := st.trackedMention()
		if len(st.pending()) != 1 {
			t.Fatal("no detection notification")
		}
		// The user's reference to the source is not what tracking follows.
		item := st.mustItem(key)
		if _, err := st.s.UpdateActionItem(context.Background(), "alice-id", item.ID, func(i *store.ActionItem) error {
			i.Refs = `[{"service":"slack","conversation_id":"C9","message_id":"9.000001"}]`
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		st.advance(5 * time.Minute)
		r := st.reply("C1", origin, "送りました", time.Minute)
		st.sl.post("C1", msg{TS: st.ts(30*time.Second, 8), User: "U2", Text: "ありがとう", ThreadTS: origin})
		st.ai.set(aiResolved(true))
		c := st.sync()
		done := st.mustItem(key)
		if done.Status != "done" || done.AutoReason.String != ReasonReplyResolved || done.AutoRef.String != link("C1", r, "") || done.AutoAt.Int64 != st.now.Unix() || done.Tracking != 1 {
			t.Fatalf("resolved: %+v", done)
		}
		if len(st.pending()) != 0 {
			t.Fatal("pending notification kept")
		}
		if c.LastTrackedAt.Int64 != st.now.Unix() {
			t.Fatalf("last tracked: %+v", c.LastTrackedAt)
		}
		in := st.ai.input(t, 1)
		if in["_system"] != ai.ReplySystemPrompt || in["task"] != "レポートを送る" || in["me"].(map[string]any)["id"] != me {
			t.Fatalf("input: %+v", in)
		}
		got := posts(in)
		if len(got) != 3 || got[0][0] != origin || got[1][0] != r {
			t.Fatalf("posts: %v", got)
		}
		for _, p := range in["posts"].([]any) {
			pm := p.(map[string]any)
			if _, ok := pm["target"]; ok || (pm["id"] == r) != (pm["is_me"] == true) {
				t.Fatalf("post: %+v", pm)
			}
		}
		// The three calls: the original message, later messages and the
		// thread.
		if n := st.sl.count("conversations.history") + st.sl.count("conversations.replies"); n < 3 {
			t.Fatalf("calls: %d", n)
		}
		// The reason is never stored.
		if strings.Contains(done.Content+done.Note.String+done.AutoRef.String, "secret reason") {
			t.Fatal("reason stored")
		}
	})
}

func TestTrackOutcomes(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		for _, tc := range []struct {
			name      string
			reply     bool
			ai        func(st *syncTest)
			touched   bool
			calls     int
			aiStatus  string
			lastError string
			tracked   bool
		}{
			{"not resolved", true, func(st *syncTest) { st.ai.set(aiResolved(false)) }, true, 1, store.AIActive, "", true},
			{"rejected", true, func(st *syncTest) { st.ai.set(aiError(400)) }, true, 1, store.AIActive, AIErrorRequestRejected, true},
			{"key error", true, func(st *syncTest) { st.ai.set(aiError(401)) }, false, 1, store.AIError, AIErrorKeyInvalid, false},
			{"temporary", true, func(st *syncTest) { st.ai.set(aiError(529)) }, false, 1, store.AIActive, "", true},
			{"no reply", false, func(st *syncTest) { st.ai.set() }, true, 0, store.AIActive, "", true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				st := newSyncTest(t, driver)
				key, origin := st.trackedMention()
				st.advance(5 * time.Minute)
				if tc.reply {
					st.reply("C1", origin, "見ます", time.Minute)
				}
				tc.ai(st)
				c := st.sync()
				item := st.mustItem(key)
				if item.Status != "open" || item.Tracking != 1 || item.TrackedAt.Valid != tc.touched || (tc.touched && item.TrackedAt.Int64 != st.now.Unix()) {
					t.Fatalf("item: %+v", item)
				}
				if st.ai.count() != tc.calls {
					t.Fatalf("AI calls: %d", st.ai.count())
				}
				if a := st.aiSettings(); a.Status != tc.aiStatus || a.LastError.String != tc.lastError {
					t.Fatalf("AI settings: %+v", a)
				}
				if (c.LastTrackedAt.Int64 == st.now.Unix()) != tc.tracked {
					t.Fatalf("last tracked: %+v", c.LastTrackedAt)
				}
			})
		}
	})
}

func TestTrackRepliesSinceLastCheck(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		st := newSyncTest(t, driver)
		key, origin := st.trackedMention()
		// A reply already judged at the last check is not judged again.
		st.advance(5 * time.Minute)
		st.reply("C1", origin, "見ます", time.Minute)
		st.ai.set(aiResolved(false))
		st.sync()
		st.advance(5 * time.Minute)
		st.ai.set()
		st.sync()
		if st.ai.count() != 0 {
			t.Fatalf("AI calls: %d", st.ai.count())
		}
		if item := st.mustItem(key); item.TrackedAt.Int64 != st.now.Unix() {
			t.Fatalf("not touched: %+v", item)
		}
		// A top-level reply after the original message counts as well.
		st.advance(5 * time.Minute)
		st.sl.post("C1", msg{TS: st.ts(time.Minute, 9), User: me, Text: "送りました"})
		st.ai.set(aiResolved(true))
		st.sync()
		if item := st.mustItem(key); item.Status != "done" {
			t.Fatalf("top-level reply: %+v", item)
		}
	})
}

func TestTrackUntrack(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		st := newSyncTest(t, driver)
		key, _ := st.trackedMention()
		// An item of another workspace and one whose key cannot be parsed
		// stop being tracked.
		c := st.conn()
		err := st.s.SlackSync(context.Background(), "alice-id", c.Generation, func(tx *store.SlackTx) error {
			for _, k := range []string{"slack:T9:m:C1:-:1.000001", "slack:T1:m:C1:-:bad"} {
				item := store.ActionItem{ID: "id-" + k[6:8], Content: "x", Status: "open", Refs: "[]", Version: 1, CreatedAt: 1, UpdatedAt: 1,
					Source: sql.NullString{String: "slack", Valid: true}, SourceKey: sql.NullString{String: k, Valid: true}, Tracking: 1}
				if _, e := tx.CreateItem(item); e != nil {
					return e
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		// The original message was deleted.
		st.sl.set(func(f *fakeSlack) { f.channels["C1"] = nil })
		st.advance(5 * time.Minute)
		st.sync()
		for _, k := range []string{key, "slack:T9:m:C1:-:1.000001", "slack:T1:m:C1:-:bad"} {
			if item := st.mustItem(k); item.Tracking != 0 || item.Status != "open" {
				t.Fatalf("%s: %+v", k, item)
			}
		}
		// A conversation Slack refuses stops tracking too.
		st2 := newSyncTest(t, driver)
		key2, _ := st2.trackedMention()
		st2.sl.set(func(f *fakeSlack) { f.forced["conversations.history"] = "channel_not_found" })
		st2.advance(5 * time.Minute)
		st2.sync()
		if item := st2.mustItem(key2); item.Tracking != 0 {
			t.Fatalf("refused: %+v", item)
		}
	})
}

func TestTrackLimits(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		st := newSyncTest(t, driver)
		c := st.conn()
		parent := st.ts(time.Hour, 0)
		st.sl.post("C1", msg{TS: parent, User: "U2", Text: "topic", ThreadTS: parent})
		var keys []string
		for i := 0; i < 25; i++ {
			ts := st.ts(time.Duration(50-i)*time.Second, i)
			st.sl.post("C1", msg{TS: ts, User: "U2", Text: "x", ThreadTS: parent})
			keys = append(keys, mentionKey("C1", parent, ts))
		}
		err := st.s.SlackSync(context.Background(), "alice-id", c.Generation, func(tx *store.SlackTx) error {
			for i, key := range keys {
				item := store.ActionItem{ID: fmt.Sprintf("item-%02d", i), Content: "x", Status: "open", Refs: "[]", Version: 1, CreatedAt: int64(i), UpdatedAt: 1,
					Source: sql.NullString{String: "slack", Valid: true}, SourceKey: sql.NullString{String: key, Valid: true}, Tracking: 1}
				if _, e := tx.CreateItem(item); e != nil {
					return e
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		// Without replies, 20 items are checked per pass.
		st.advance(5 * time.Minute)
		st.sync()
		touched := 0
		for _, k := range keys {
			if st.mustItem(k).TrackedAt.Valid {
				touched++
			}
		}
		if touched != 20 || st.sl.count("conversations.replies") != 20 {
			t.Fatalf("touched %d, replies %d", touched, st.sl.count("conversations.replies"))
		}
		// With replies, the pass stops after three AI calls and still counts.
		st.advance(5 * time.Minute)
		st.sl.post("C1", msg{TS: st.ts(time.Second, 99), User: me, Text: "done", ThreadTS: parent})
		st.ai.set(aiResolved(false), aiResolved(false), aiResolved(false), aiResolved(false))
		c = st.sync()
		if st.ai.count() != 3 || c.LastTrackedAt.Int64 != st.now.Unix() {
			t.Fatalf("AI calls %d, last tracked %+v", st.ai.count(), c.LastTrackedAt)
		}
	})
}
