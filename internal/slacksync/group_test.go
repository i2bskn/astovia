package slacksync

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/i2bskn/astovia/internal/ai"
	"github.com/i2bskn/astovia/internal/store"
)

// groups returns the group parents made by the AI.
func (st *syncTest) groups() []store.ActionItem {
	st.t.Helper()
	var out []store.ActionItem
	for _, i := range st.items() {
		if strings.HasPrefix(i.SourceKey.String, "slack:T1:g:") {
			out = append(out, i)
		}
	}
	return out
}

// threadMention posts a reply mentioning alice in the thread of root, d ago.
func (st *syncTest) threadMention(root string, d time.Duration, n int, text string) string {
	ts := st.ts(d, n)
	st.sl.post("C1", msg{TS: ts, User: "U2", Text: "<@U1> " + text, ThreadTS: root})
	return ts
}

// before runs fn when the AI is called, before it answers, as a change
// made while the AI judges.
func before(fn func(), reply func(http.ResponseWriter)) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		fn()
		reply(w)
	}
}

func (st *syncTest) setStatus(id, status string) {
	st.t.Helper()
	if _, err := st.s.UpdateActionItem(context.Background(), "alice-id", id, func(i *store.ActionItem) error {
		i.Status, i.UpdatedAt = status, st.now.Unix()
		return nil
	}); err != nil {
		st.t.Fatal(err)
	}
}

// threadFixture starts a thread in C1 whose reply mentions alice and makes
// a task of it.
func (st *syncTest) threadFixture() (root string, first store.ActionItem) {
	st.t.Helper()
	st.advance(time.Minute)
	root = st.ts(20*time.Minute, 1)
	st.sl.post("C1", msg{TS: root, User: "U2", Text: "決済で障害が出ています", ThreadTS: root})
	r1 := st.threadMention(root, 5*time.Minute, 1, "調査お願いします")
	st.ai.set(aiAction("障害を調査する"))
	st.sync()
	return root, st.mustItem(mentionKey("C1", root, r1))
}

func TestGroupSameMatter(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		st := newSyncTest(t, driver)
		root, first := st.threadFixture()

		// A task of the same matter: a group parent is made for both.
		st.advance(time.Minute)
		r2 := st.threadMention(root, 30*time.Second, 2, "報告書もお願いします")
		st.ai.set(aiGrouped(ai.DecisionTask, "報告書を書く", "決済障害の対応", "t1"))
		st.sync()
		if got := related(st.ai.input(t, 1)); !reflect.DeepEqual(got, []string{"t1:障害を調査する"}) {
			t.Fatalf("related tasks: %v", got)
		}
		groups := st.groups()
		if len(groups) != 1 {
			t.Fatalf("groups: %+v", groups)
		}
		g := groups[0]
		second := st.mustItem(mentionKey("C1", root, r2))
		if g.Content != "決済障害の対応" || g.Status != "open" || g.Tracking != 0 || g.Source.String != "slack" || g.Kind != store.KindTask || g.ParentID.Valid ||
			g.SourceKey.String != "slack:T1:g:C1:"+root+":"+second.ID || g.DetectedAt.Int64 != st.now.Unix() {
			t.Fatalf("group: %+v", g)
		}
		var refs []map[string]string
		if err := json.Unmarshal([]byte(g.Refs), &refs); err != nil || len(refs) != 1 || refs[0]["url"] != permaBase+"C1/p"+strings.ReplaceAll(root, ".", "") || refs[0]["conversation_id"] != "C1" || refs[0]["conversation_name"] != "#name-C1" || refs[0]["account"] != "Acme" || refs[0]["message_id"] != "" {
			t.Fatalf("group refs: %s", g.Refs)
		}
		if got := st.mustItem(first.SourceKey.String); got.ParentID.String != g.ID || got.Version != first.Version+1 || got.Status != "open" {
			t.Fatalf("first task: %+v", got)
		}
		if second.ParentID.String != g.ID || second.Status != "open" {
			t.Fatalf("second task: %+v", second)
		}
		// The new task is notified; the group parent is not.
		for _, n := range st.pending() {
			if n.ActionItemID.String == g.ID {
				t.Fatalf("group notified: %+v", n)
			}
		}
		if len(st.pending()) != 2 {
			t.Fatalf("notifications: %+v", st.pending())
		}

		// The group is shown once with its open subtasks; a task of the
		// same matter joins it.
		st.advance(time.Minute)
		r3 := st.threadMention(root, 30*time.Second, 3, "再発防止の打ち合わせを")
		st.ai.set(aiGrouped(ai.DecisionTask, "再発防止を検討する", "別の名前", "t1"))
		st.sync()
		if got := related(st.ai.input(t, 1)); !reflect.DeepEqual(got, []string{"t1:決済障害の対応[障害を調査する,報告書を書く]"}) {
			t.Fatalf("related with group: %v", got)
		}
		if got := st.mustItem(mentionKey("C1", root, r3)); got.ParentID.String != g.ID {
			t.Fatalf("third task: %+v", got)
		}
		if n := len(st.groups()); n != 1 {
			t.Fatalf("groups after joining: %d", n)
		}

		// Another matter in the same thread stays on its own, then gets a
		// group of its own.
		st.advance(time.Minute)
		r4 := st.threadMention(root, 30*time.Second, 4, "ランチどうします?")
		st.ai.set(aiGrouped(ai.DecisionTask, "ランチの店を決める", "ランチ", ""))
		st.sync()
		lunch := st.mustItem(mentionKey("C1", root, r4))
		if lunch.ParentID.Valid {
			t.Fatalf("other matter grouped: %+v", lunch)
		}
		st.advance(time.Minute)
		r5 := st.threadMention(root, 30*time.Second, 5, "予約もお願い")
		st.ai.set(aiGrouped(ai.DecisionTask, "店を予約する", "ランチの調整", "t1"))
		st.sync()
		if got := related(st.ai.input(t, 1)); !reflect.DeepEqual(got, []string{"t1:ランチの店を決める", "t2:決済障害の対応[障害を調査する,報告書を書く,再発防止を検討する]"}) {
			t.Fatalf("related with two matters: %v", got)
		}
		if n := len(st.groups()); n != 2 {
			t.Fatalf("groups: %d", n)
		}
		booking := st.mustItem(mentionKey("C1", root, r5))
		if lunch = st.mustItem(lunch.SourceKey.String); !lunch.ParentID.Valid || lunch.ParentID != booking.ParentID || lunch.ParentID.String == g.ID {
			t.Fatalf("second group: %+v %+v", lunch, booking)
		}
	})
}

// Only open tasks are grouped, with a topic and a known related task.
func TestGroupSkipped(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		st := newSyncTest(t, driver)
		root, first := st.threadFixture()
		for name, reply := range map[string]func(http.ResponseWriter){
			"candidate":      aiGrouped(ai.DecisionCandidate, "確認する", "決済障害の対応", "t1"),
			"no topic":       aiGrouped(ai.DecisionTask, "確認する", "", "t1"),
			"unknown symbol": aiGrouped(ai.DecisionTask, "確認する", "決済障害の対応", "t9"),
			"database id":    aiGrouped(ai.DecisionTask, "確認する", "決済障害の対応", first.ID),
		} {
			st.advance(time.Minute)
			ts := st.threadMention(root, 30*time.Second, 2, name)
			st.ai.set(reply)
			st.sync()
			if got := st.mustItem(mentionKey("C1", root, ts)); got.ParentID.Valid {
				t.Fatalf("%s grouped: %+v", name, got)
			}
			if len(st.groups()) != 0 {
				t.Fatalf("%s made a group", name)
			}
		}
		if got := st.mustItem(first.SourceKey.String); got.ParentID.Valid || got.Version != first.Version {
			t.Fatalf("first task changed: %+v", got)
		}
	})
}

// When the related task changed while the AI judged, nothing is grouped,
// no group parent is left behind, and the synchronization goes on.
func TestGroupTargetChanged(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		st := newSyncTest(t, driver)
		root, first := st.threadFixture()
		st.advance(time.Minute)
		r2 := st.threadMention(root, 30*time.Second, 2, "報告書も")
		st.ai.set(before(func() { st.setStatus(first.ID, "done") }, aiGrouped(ai.DecisionTask, "報告書を書く", "決済障害の対応", "t1")))
		c := st.sync()
		if got := st.mustItem(mentionKey("C1", root, r2)); got.ParentID.Valid || got.Status != "open" {
			t.Fatalf("grouped with a done task: %+v", got)
		}
		if len(st.groups()) != 0 || c.LastSyncedAt.Int64 != st.now.Unix() || c.LastError.Valid {
			t.Fatalf("groups %d, connection %+v", len(st.groups()), c)
		}

		// A group dismissed while the AI judged does not take the task.
		st.setStatus(first.ID, "open")
		st.advance(time.Minute)
		r3 := st.threadMention(root, 30*time.Second, 3, "まとめて")
		st.ai.set(aiGrouped(ai.DecisionTask, "まとめる", "決済障害の対応", "t2"))
		st.sync()
		g := st.groups()
		if len(g) != 1 {
			t.Fatalf("groups: %d", len(g))
		}
		r4 := st.threadMention(root, 20*time.Second, 4, "これも")
		st.advance(time.Minute)
		st.ai.set(before(func() { st.setStatus(g[0].ID, "dismissed") }, aiGrouped(ai.DecisionTask, "これもやる", "決済障害の対応", "t1")))
		st.sync()
		if got := st.mustItem(mentionKey("C1", root, r4)); got.ParentID.Valid {
			t.Fatalf("joined a dismissed group: %+v", got)
		}
		st.mustItem(mentionKey("C1", root, r3))
	})
}

// A mention in a thread and one in its first message are one conversation,
// whichever comes first.
func TestGroupThreadRoot(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		st := newSyncTest(t, driver)
		st.advance(time.Minute)
		root := st.ts(10*time.Minute, 1)
		st.sl.post("C1", msg{TS: root, User: "U2", Text: "<@U1> 障害の確認をお願いします"})
		st.ai.set(aiAction("障害を確認する"))
		st.sync()
		rootItem := st.mustItem(mentionKey("C1", "-", root))

		// A reply mention later gets the first message's task.
		st.advance(time.Minute)
		st.sl.set(func(f *fakeSlack) { f.channels["C1"][0].ThreadTS = root })
		r1 := st.threadMention(root, 30*time.Second, 2, "報告もお願いします")
		st.ai.set(aiGrouped(ai.DecisionTask, "報告する", "障害対応", "t1"))
		st.sync()
		if got := related(st.ai.input(t, 1)); !reflect.DeepEqual(got, []string{"t1:障害を確認する"}) {
			t.Fatalf("reply related: %v", got)
		}
		g := st.groups()
		if len(g) != 1 || g[0].SourceKey.String != "slack:T1:g:C1:"+root+":"+st.mustItem(mentionKey("C1", root, r1)).ID || st.mustItem(rootItem.SourceKey.String).ParentID.String != g[0].ID {
			t.Fatalf("groups: %+v", g)
		}

		// The first message judged after a reply gets the reply's task.
		st.advance(time.Minute)
		other := st.ts(5*time.Minute, 3)
		st.sl.post("C1", msg{TS: other, User: "U3", Text: "<@U1> 別スレッドの依頼です", ThreadTS: other})
		reply := st.threadMention(other, 30*time.Second, 4, "こちらも")
		st.ai.set(aiAction("返信する"), aiAction("依頼を受ける"))
		st.sync()
		if got := related(st.ai.input(t, 2)); !reflect.DeepEqual(got, []string{"t1:返信する"}) {
			t.Fatalf("root related: %v", got)
		}
		st.mustItem(mentionKey("C1", other, reply))
	})
}

// Group parents in a direct message link to the conversation; their keys
// do not disturb the judged runs or tracking.
func TestGroupInDirectMessages(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		st := newSyncTest(t, driver)
		st.advance(time.Minute)
		a1, own, a2 := st.ts(50*time.Second, 1), st.ts(40*time.Second, 1), st.ts(30*time.Second, 1)
		st.sl.post("D1", msg{TS: a1, User: "U2", Text: "資料ください"})
		st.sl.post("D1", msg{TS: own, User: me, Text: "はい"})
		st.sl.post("D1", msg{TS: a2, User: "U2", Text: "あとスライドも"})
		st.ai.set(aiAction("資料を送る"), aiGrouped(ai.DecisionTask, "スライドを送る", "勉強会の準備", "t1"))
		st.sync()
		g := st.groups()
		if len(g) != 1 || !strings.HasPrefix(g[0].SourceKey.String, "slack:T1:g:D1:-:") {
			t.Fatalf("groups: %+v", g)
		}
		var refs []map[string]string
		if err := json.Unmarshal([]byte(g[0].Refs), &refs); err != nil || refs[0]["url"] != permaBase+"D1" || refs[0]["conversation_name"] != "DM（bob）" {
			t.Fatalf("refs: %s", g[0].Refs)
		}
		for _, key := range []string{dmKey("D1", "-", a1, a1), dmKey("D1", "-", a2, a2)} {
			if got := st.mustItem(key); got.ParentID.String != g[0].ID {
				t.Fatalf("%s: %+v", key, got)
			}
		}
		if _, ok := parseSourceKey(g[0].SourceKey.String); ok {
			t.Fatal("group key parsed as a message key")
		}

		// The next run is judged once; the group is neither judged nor
		// tracked, and it is completed when its subtasks are resolved by
		// replies.
		st.advance(5 * time.Minute)
		st.sl.post("D1", msg{TS: st.ts(20*time.Second, 2), User: me, Text: "送りました、スライドも"})
		st.ai.set(aiResolved(true), aiResolved(true))
		st.sync()
		if st.ai.count() != 2 {
			t.Fatalf("AI calls: %d", st.ai.count())
		}
		for _, key := range []string{dmKey("D1", "-", a1, a1), dmKey("D1", "-", a2, a2)} {
			if got := st.mustItem(key); got.Status != "done" || got.AutoReason.String != ReasonReplyResolved {
				t.Fatalf("%s: %+v", key, got)
			}
		}
		done := st.mustItem(g[0].SourceKey.String)
		if done.Status != "done" || done.AutoReason.String != store.ReasonChildrenDone || done.TrackedAt.Valid {
			t.Fatalf("group: %+v", done)
		}

		// A closed group takes a new task of its matter and opens again.
		st.advance(time.Minute)
		a3 := st.ts(10*time.Second, 3)
		st.sl.post("D1", msg{TS: a3, User: "U2", Text: "当日の司会も"})
		st.ai.set(aiGrouped(ai.DecisionTask, "司会をする", "勉強会の準備", "t1"))
		st.sync()
		// Both subtasks were made in one synchronization, so their order
		// follows their random IDs.
		if got := related(st.ai.input(t, 1)); !reflect.DeepEqual(got, []string{"t1:勉強会の準備[資料を送る,スライドを送る]!"}) && !reflect.DeepEqual(got, []string{"t1:勉強会の準備[スライドを送る,資料を送る]!"}) {
			t.Fatalf("closed group: %v", got)
		}
		if got := st.mustItem(dmKey("D1", "-", a3, a3)); got.ParentID.String != g[0].ID {
			t.Fatalf("new subtask: %+v", got)
		}
		if got := st.mustItem(g[0].SourceKey.String); got.Status != "open" || got.AutoReason.Valid {
			t.Fatalf("group reopened: %+v", got)
		}
	})
}
