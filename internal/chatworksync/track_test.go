package chatworksync

import (
	"strconv"
	"testing"
	"time"

	"github.com/i2bskn/astovia/internal/ai"
	"github.com/i2bskn/astovia/internal/store"
)

// mentionFixture makes a task of a TO in room 2 and returns its message.
func (st *syncTest) mentionFixture() string {
	st.t.Helper()
	st.cw.room("2", "Team", "group")
	m := st.say("2", bob, "[To:101]資料ください", time.Minute)
	st.ai.set(aiAction("資料を送る"))
	st.sync()
	st.mustItem(mentionKey("2", m))
	return m
}

func TestTrackReply(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		st := newSyncTest(t, driver)
		origin := st.mentionFixture()

		// Without a message of the user, the item is only touched.
		st.advance(5 * time.Minute)
		st.say("2", bob, "よろしく", time.Minute)
		st.ai.set()
		st.sync()
		item := st.mustItem(mentionKey("2", origin))
		if st.ai.count() != 0 || item.Status != "open" || item.TrackedAt.Int64 != st.now.Unix() || item.Version != 1 {
			t.Fatalf("touched: %+v AI %d", item, st.ai.count())
		}

		// The user's message is judged and completes the item.
		st.advance(5 * time.Minute)
		mine := st.say("2", me, "[rp aid=102 to=2-"+origin+"]送りました", time.Minute)
		st.ai.set(aiResolved(true))
		st.sync()
		item = st.mustItem(mentionKey("2", origin))
		if item.Status != "done" || item.AutoReason.String != ReasonReplyResolved || item.AutoRef.String != "https://www.chatwork.com/#!rid2-"+mine || item.Tracking != 1 {
			t.Fatalf("resolved: %+v", item)
		}
		in := st.ai.input(t, 1)
		got := posts(in)
		if in["_system"] != ai.ChatworkReplySystemPrompt || in["task"] != "資料を送る" || got[0][0] != origin || got[len(got)-1][0] != mine {
			t.Fatalf("reply input: %+v", in)
		}
		last := in["posts"].([]any)[len(got)-1].(map[string]any)
		if last["is_me"] != true || last["body"] != "返信 @bob(102) 送りました" {
			t.Fatalf("reply post: %+v", last)
		}
	})
}

// The original message may have scrolled out of the latest messages; the
// later messages are judged without it.
func TestTrackWithoutOrigin(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		st := newSyncTest(t, driver)
		origin := st.mentionFixture()
		st.advance(5 * time.Minute)
		st.cw.set(func(f *fakeChatwork) { f.pageSize = 3 })
		for i := 0; i < 3; i++ {
			st.say("2", carol, "雑談 "+strconv.Itoa(i), time.Duration(4-i)*time.Minute)
		}
		mine := st.say("2", me, "対応しました", 30*time.Second)
		st.ai.set(aiResolved(false))
		st.sync()
		in := st.ai.input(t, 1)
		got := posts(in)
		if st.ai.count() != 1 || got[0][0] == origin || got[len(got)-1][0] != mine {
			t.Fatalf("posts: %v", got)
		}
		if item := st.mustItem(mentionKey("2", origin)); item.Status != "open" || item.Tracking != 1 || item.TrackedAt.Int64 != st.now.Unix() {
			t.Fatalf("not resolved: %+v", item)
		}
	})
}

func TestTrackUntrack(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		st := newSyncTest(t, driver)
		origin := st.mentionFixture()
		now := st.now.Unix()
		// An item of another account is not followed.
		st.create(store.ActionItem{ID: "other", Content: "other", Status: "open", Refs: "[]", Version: 1, CreatedAt: now, UpdatedAt: now,
			Source: nullString(store.SourceChatwork), SourceKey: nullString("chatwork:999:m:2:1000"), Tracking: 1, DetectedAt: nullInt(now)})
		// A room that cannot be read any more stops the tracking.
		st.advance(5 * time.Minute)
		st.cw.set(func(f *fakeChatwork) { f.statuses["messages:2"] = 403 })
		st.sync()
		for _, key := range []string{mentionKey("2", origin), "chatwork:999:m:2:1000"} {
			if item := st.mustItem(key); item.Tracking != 0 || item.Status != "open" {
				t.Fatalf("untracked: %+v", item)
			}
		}
		if c := st.conn(); c.LastError.Valid || !c.LastTrackedAt.Valid || c.LastTrackedAt.Int64 != st.now.Unix() {
			t.Fatalf("connection: %+v", c)
		}
	})
}

// Tracking reads at most trackMessagesLimit rooms; the pass ends there.
func TestTrackBudget(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		st := newSyncTest(t, driver)
		now := st.now.Unix()
		for i := 1; i <= 11; i++ {
			id := strconv.Itoa(10 + i)
			st.create(store.ActionItem{ID: "i" + id, Content: id, Status: "open", Refs: "[]", Version: 1, CreatedAt: now, UpdatedAt: now,
				Source: nullString(store.SourceChatwork), SourceKey: nullString(mentionKey(id, "1000")), Tracking: 1, DetectedAt: nullInt(now)})
		}
		st.advance(5 * time.Minute)
		c := st.sync()
		if st.cw.count("messages") != trackMessagesLimit || c.LastTrackedAt.Int64 != st.now.Unix() {
			t.Fatalf("messages read: %d %+v", st.cw.count("messages"), c)
		}
		for i := 1; i <= 11; i++ {
			id := strconv.Itoa(10 + i)
			if item := st.mustItem(mentionKey(id, "1000")); item.TrackedAt.Valid != (i <= 10) {
				t.Fatalf("item %s: %+v", id, item)
			}
		}
	})
}
