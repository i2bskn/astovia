package chatworksync

import (
	"context"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/i2bskn/astovia/internal/ai"
	"github.com/i2bskn/astovia/internal/store"
)

// posts returns the posts of an AI input as (id, target) pairs.
func posts(in map[string]any) [][2]string {
	var out [][2]string
	for _, p := range in["posts"].([]any) {
		m := p.(map[string]any)
		target := "-"
		if v, ok := m["target"].(bool); ok && v {
			target = "T"
		}
		out = append(out, [2]string{m["id"].(string), target})
	}
	return out
}

// targets returns the IDs of the target posts of an AI input.
func targets(in map[string]any) []string {
	var out []string
	for _, p := range posts(in) {
		if p[1] == "T" {
			out = append(out, p[0])
		}
	}
	return out
}

func related(in map[string]any) []string {
	out := []string{}
	for _, v := range in["related_tasks"].([]any) {
		m := v.(map[string]any)
		out = append(out, m["id"].(string)+":"+m["task"].(string))
	}
	return out
}

func TestWindow(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		st := newSyncTest(t, driver)

		// Nothing to read: the window moves to the start of the
		// synchronization.
		if c := st.sync(); c.LastSyncedAt.Int64 != st.now.Unix() || st.cw.count("rooms") != 1 {
			t.Fatalf("empty: %+v", c)
		}
		start := st.now.Unix()

		// A postponed message keeps the window.
		st.advance(time.Minute)
		st.cw.room("2", "Team", "group")
		first := st.say("2", bob, "[To:101]至急", 30*time.Second)
		st.ai.set(aiError(500))
		if c := st.sync(); c.LastSyncedAt.Int64 != start || len(st.items()) != 0 {
			t.Fatalf("postponed: %+v %d", c, len(st.items()))
		}

		// After retryLimit the window moves on and the postponed messages
		// become confirmation tasks.
		st.advance(7 * time.Hour)
		second := st.say("2", bob, "[To:101]こちらも", 30*time.Second)
		st.ai.set()
		if c := st.sync(); c.LastSyncedAt.Int64 != st.now.Unix() {
			t.Fatalf("overdue: %+v", c)
		}
		for id, want := range map[string]string{first: "Chatwork を確認: @alice(101) 至急", second: "Chatwork を確認: @alice(101) こちらも"} {
			if item := st.mustItem(mentionKey("2", id)); item.Content != want || item.Status != "open" || item.Tracking != 1 {
				t.Fatalf("confirmation task: %+v", item)
			}
		}

		// With detection off, nothing is read and the window moves.
		st.cw.reset0()
		st.settings(store.ChatworkSettings{DetectMentions: boolPtr(false), DetectDMs: boolPtr(false)})
		st.advance(time.Minute)
		if c := st.sync(); c.LastSyncedAt.Int64 != st.now.Unix() || st.cw.count("rooms") != 0 || st.cw.count("messages") != 0 {
			t.Fatalf("detection off: %+v", c)
		}
	})
}

func TestRoomSelection(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		st := newSyncTest(t, driver)
		st.cw.room("1", "マイチャット", "my")
		st.cw.room("2", "Team", "group")
		st.cw.room("3", "bob", "direct")
		st.cw.room("4", "Old", "group")
		st.say("1", me, "[To:101] memo", time.Minute)
		st.say("2", bob, "hello", time.Minute)
		st.say("3", me, "hi bob", time.Minute)
		st.say("4", bob, "[To:101] old", 30*time.Minute)
		st.sync()
		if st.cw.count("messages") != 2 || st.cw.count("messages:2") != 1 || st.cw.count("messages:3") != 1 || st.ai.count() != 0 {
			t.Fatalf("rooms read: %v", st.cw.requests)
		}

		// Rooms read to the end are not read again until they change.
		st.cw.reset0()
		st.advance(time.Minute)
		st.sync()
		if st.cw.count("messages") != 0 {
			t.Fatalf("read again: %v", st.cw.requests)
		}

		// Without mention detection, group chats are not read.
		st.settings(store.ChatworkSettings{DetectMentions: boolPtr(false)})
		st.say("2", bob, "again", 10*time.Second)
		st.say("3", me, "again", 10*time.Second)
		st.advance(time.Minute)
		st.sync()
		if st.cw.count("messages") != 1 || st.cw.count("messages:3") != 1 {
			t.Fatalf("direct only: %v", st.cw.requests)
		}
		// Without direct chat detection, direct chats are read for TO.
		st.cw.reset0()
		st.settings(store.ChatworkSettings{DetectMentions: boolPtr(true), DetectDMs: boolPtr(false)})
		st.say("3", me, "more", 10*time.Second)
		st.advance(time.Minute)
		st.sync()
		if st.cw.count("messages") != 2 || st.cw.count("messages:2") != 1 || st.cw.count("messages:3") != 1 {
			t.Fatalf("mentions only: %v", st.cw.requests)
		}
	})
}

// Rooms over roomLimit are carried over and read by the next
// synchronization, newest first.
func TestRoomLimit(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		st := newSyncTest(t, driver)
		start := st.conn().LastSyncedAt.Int64
		for i := 1; i <= 12; i++ {
			room := strconv.Itoa(10 + i)
			st.cw.room(room, "Room "+room, "group")
			st.say(room, bob, "hello", time.Duration(i)*time.Second)
		}
		if c := st.sync(); st.cw.count("messages") != 10 || c.LastSyncedAt.Int64 != start {
			t.Fatalf("first: %v %+v", st.cw.requests, c)
		}
		for _, room := range []string{"21", "22"} {
			if st.cw.count("messages:"+room) != 0 {
				t.Fatalf("oldest rooms read first: %v", st.cw.requests)
			}
		}
		st.cw.reset0()
		st.advance(time.Minute)
		if c := st.sync(); st.cw.count("messages") != 2 || st.cw.count("messages:21") != 1 || st.cw.count("messages:22") != 1 || c.LastSyncedAt.Int64 != st.now.Unix() {
			t.Fatalf("second: %v %+v", st.cw.requests, c)
		}
	})
}

func TestRoomReadAgain(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		st := newSyncTest(t, driver)
		st.cw.room("2", "Team", "group")
		m1 := st.say("2", bob, "[To:101]至急", 30*time.Second)

		// A room with a postponed message is read again.
		st.ai.set(aiError(500))
		st.sync()
		st.cw.reset0()
		st.advance(time.Minute)
		st.ai.set(aiAction("対応する"))
		st.sync()
		if st.cw.count("messages:2") != 1 || st.mustItem(mentionKey("2", m1)).Content != "対応する" {
			t.Fatalf("postponed room: %v", st.cw.requests)
		}

		// A new generation forgets the rooms read.
		st.cw.reset0()
		st.advance(time.Minute)
		st.sync()
		if st.cw.count("messages:2") != 0 {
			t.Fatalf("read room read again: %v", st.cw.requests)
		}
		st.connect(me, store.ChatworkSettings{})
		st.sync()
		if st.cw.count("messages:2") != 1 || st.ai.count() != 1 {
			t.Fatalf("new generation: %v AI %d", st.cw.requests, st.ai.count())
		}

		// A message sent at the window end is judged by the next
		// synchronization.
		st.cw.reset0()
		st.advance(time.Minute)
		m2 := st.say("2", bob, "[To:101]締め切り", 0)
		st.ai.set()
		st.sync()
		if _, ok := st.item(mentionKey("2", m2)); ok || st.cw.count("messages:2") != 1 || st.ai.count() != 0 {
			t.Fatalf("message at the window end judged")
		}
		st.advance(time.Minute)
		st.ai.set(aiAction("締め切りを確認する"))
		st.sync()
		if st.cw.count("messages:2") != 2 || st.mustItem(mentionKey("2", m2)).Content != "締め切りを確認する" {
			t.Fatalf("message after the window: %v", st.cw.requests)
		}

		// Rooms that cannot be read are skipped and count as read.
		st.cw.reset0()
		for room, code := range map[string]int{"5": 403, "6": 404, "7": 1} {
			st.cw.room(room, "Room "+room, "group")
			st.say(room, bob, "[To:101] x", 10*time.Second)
			st.cw.set(func(f *fakeChatwork) { f.statuses["messages:"+room] = code })
		}
		st.advance(time.Minute)
		if c := st.sync(); c.LastSyncedAt.Int64 != st.now.Unix() || c.LastError.Valid || st.cw.count("messages") != 3 {
			t.Fatalf("skipped rooms: %+v %v", c, st.cw.requests)
		}
		st.cw.reset0()
		st.advance(time.Minute)
		st.sync()
		if st.cw.count("messages") != 0 {
			t.Fatalf("skipped rooms read again: %v", st.cw.requests)
		}
	})
}

// TO, replies and TO ALL in a group chat are judged one by one, newest
// first; other messages are not.
func TestMentionUnits(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		st := newSyncTest(t, driver)
		st.cw.room("2", "開発チーム", "group")
		before := st.say("2", carol, "おはようございます", 9*time.Minute)
		to := st.say("2", bob, "[To:101]資料ください", 8*time.Minute)
		rp := st.say("2", carol, "[rp aid=101 to=2-1000001]了解です", 7*time.Minute)
		all := st.say("2", bob, "[toall]明日は休みです", 6*time.Minute)
		st.say("2", bob, "[To:103]carolさんお願い", 5*time.Minute)
		st.say("2", carol, "[qt][qtmeta aid=102][To:101]資料ください[/qt]そうですね", 4*time.Minute)
		st.say("2", me, "[To:101]メモ", 3*time.Minute)
		st.say("2", bob, "[info][title][dtext:task_added][/title]x[/info]", 2*time.Minute)
		withTask := st.say("2", bob, "[To:101][task aid=101 st=open lt=0]作業[/task]", time.Minute)
		st.ai.set(aiNoAction("bob さんが周知"), aiCandidate("返事する"), aiAction("資料を送る"))
		st.sync()
		if st.ai.count() != 3 {
			t.Fatalf("AI calls: %d", st.ai.count())
		}
		open := st.mustItem(mentionKey("2", to))
		if open.Status != "open" || open.Content != "資料を送る" || open.Tracking != 1 || open.Kind != store.KindTask {
			t.Fatalf("task: %+v", open)
		}
		refs := refsOf(t, open)
		if len(refs) != 1 || refs[0]["service"] != "chatwork" || refs[0]["account"] != "Acme" || refs[0]["conversation_id"] != "2" || refs[0]["conversation_name"] != "開発チーム" || refs[0]["message_id"] != to || refs[0]["url"] != "https://www.chatwork.com/#!rid2-"+to {
			t.Fatalf("refs: %s", open.Refs)
		}
		if c := st.mustItem(mentionKey("2", rp)); c.Status != "candidate" || c.Content != "返事する" || c.Tracking != 1 {
			t.Fatalf("candidate: %+v", c)
		}
		if d := st.mustItem(mentionKey("2", all)); d.Status != "done" || d.AutoReason.String != ReasonNoAction || d.Tracking != 0 || d.AutoRef.String != "https://www.chatwork.com/#!rid2-"+all {
			t.Fatalf("no action: %+v", d)
		}
		if n := len(st.items()); n != 3 {
			t.Fatalf("items: %d", n)
		}
		p := st.pending()
		if len(p) != 1 || p[0].Title != "Chatwork: TO" || p[0].Body != "資料を送る" || p[0].ActionItemID.String != open.ID {
			t.Fatalf("notifications: %+v", p)
		}
		in := st.ai.input(t, 3)
		meJSON := in["me"].(map[string]any)
		if in["_system"] != ai.ChatworkSystemPrompt || meJSON["id"] != me || meJSON["name"] != "alice" || in["workspace"] != "Acme" || in["conversation_type"] != "channel" || in["channel_name"] != "開発チーム" {
			t.Fatalf("input: %+v", in)
		}
		if got := posts(in); got[0] != [2]string{before, "-"} || !reflect.DeepEqual(targets(in), []string{to}) {
			t.Fatalf("posts: %v", got)
		}
		target := in["posts"].([]any)[1].(map[string]any)
		if target["body"] != "@alice(101) 資料ください" || target["author"] != "bob" || target["author_id"] != bob || target["is_me"] != false {
			t.Fatalf("target post: %+v", target)
		}

		// With task import off, a message adding a task is judged too.
		st.settings(store.ChatworkSettings{ImportTasks: boolPtr(false)})
		st.advance(time.Minute)
		st.say("2", carol, "ok", 10*time.Second)
		st.ai.set(aiAction("作業する"))
		st.sync()
		if item := st.mustItem(mentionKey("2", withTask)); item.Content != "作業する" || st.ai.count() != 1 {
			t.Fatalf("task message: %+v", item)
		}

		// Without direct chat detection, a TO in a direct chat is a
		// mention of a direct chat.
		st.settings(store.ChatworkSettings{DetectDMs: boolPtr(false)})
		st.cw.room("3", "bob", "direct")
		st.advance(time.Minute)
		st.say("3", bob, "おつかれさまです", 20*time.Second)
		dm := st.say("3", bob, "[To:101]見てください", 10*time.Second)
		st.ai.set(aiAction("見る"))
		st.sync()
		item := st.mustItem(mentionKey("3", dm))
		if st.ai.count() != 1 || st.ai.input(t, 1)["conversation_type"] != "dm" || refsOf(t, item)[0]["conversation_name"] != "DM（bob）" {
			t.Fatalf("direct mention: %+v", item)
		}
	})
}

// A direct chat is judged in runs of the other member's messages, split by
// the user's messages, the runs judged already and runLimit.
func TestDirectRuns(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		st := newSyncTest(t, driver)
		st.cw.room("3", "bob", "direct")
		b1 := st.say("3", bob, "相談です", 8*time.Minute)
		b2 := st.say("3", bob, "来週の件", 7*time.Minute)
		st.say("3", me, "はい", 6*time.Minute)
		b3 := st.say("3", bob, "ありがとう", 5*time.Minute)
		st.say("3", bob, "[dtext:chatroom_member_is]", 4*time.Minute)
		st.ai.set(aiAction("相談に乗る"), aiNoAction("お礼"))
		st.sync()
		if st.ai.count() != 2 || !reflect.DeepEqual(targets(st.ai.input(t, 1)), []string{b1, b2}) || !reflect.DeepEqual(targets(st.ai.input(t, 2)), []string{b3}) {
			t.Fatalf("runs: AI %d", st.ai.count())
		}
		in := st.ai.input(t, 1)
		if in["conversation_type"] != "dm" || in["channel_name"] != "bob" {
			t.Fatalf("input: %+v", in)
		}
		first := st.mustItem(dmKey("3", b1, b2))
		if first.Content != "相談に乗る" || refsOf(t, first)[0]["conversation_name"] != "DM（bob）" || refsOf(t, first)[0]["message_id"] != b2 {
			t.Fatalf("first run: %+v", first)
		}
		if st.mustItem(dmKey("3", b3, b3)).Status != "done" {
			t.Fatal("second run")
		}
		if p := st.pending(); len(p) != 1 || p[0].Title != "Chatwork: ダイレクトチャット" {
			t.Fatalf("notifications: %+v", p)
		}

		// Judged runs are not judged again; long runs are split.
		st.advance(time.Minute)
		b4 := st.say("3", bob, "追加です", 50*time.Second)
		st.say("3", me, "了解", 45*time.Second)
		var long []string
		for i := 0; i < 12; i++ {
			long = append(long, st.say("3", bob, "連投 "+strconv.Itoa(i), time.Duration(40-i)*time.Second))
		}
		st.ai.set(aiNoAction("a"), aiNoAction("b"), aiNoAction("c"))
		st.sync()
		if st.ai.count() != 3 || !reflect.DeepEqual(targets(st.ai.input(t, 1)), []string{b4}) || !reflect.DeepEqual(targets(st.ai.input(t, 2)), long[:10]) || !reflect.DeepEqual(targets(st.ai.input(t, 3)), long[10:]) {
			t.Fatalf("second sync: AI %d", st.ai.count())
		}
		st.mustItem(dmKey("3", long[0], long[9]))
		st.mustItem(dmKey("3", long[10], long[11]))
	})
}

func TestJudgmentOutcomes(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		st := newSyncTest(t, driver)
		st.cw.room("2", "Team", "group")
		meeting := st.say("2", bob, "[To:101]明日 10:00 から打ち合わせです", time.Minute)
		start := st.now.Add(24 * time.Hour).Truncate(time.Minute)
		local := func(v time.Time) string { return v.UTC().Format("2006-01-02T15:04") }
		st.ai.set(aiJudged(ai.DecisionTask, "打ち合わせを登録する", "s", "打ち合わせ", local(start), ""))
		st.sync()
		event := st.mustItem(mentionKey("2", meeting))
		if event.Kind != store.KindEvent || event.Tracking != 0 || event.EventStartAt.Int64 != start.Unix() || event.DueAt.Int64 != start.Unix() || event.EventTitle.String != "打ち合わせ" {
			t.Fatalf("event: %+v", event)
		}

		// An invalid key makes confirmation tasks.
		st.advance(time.Minute)
		urgent := st.say("2", bob, "[To:101]至急\n確認を", 10*time.Second)
		st.ai.set(aiError(401))
		st.sync()
		if item := st.mustItem(mentionKey("2", urgent)); item.Content != "Chatwork を確認: @alice(101) 至急 確認を" || item.Status != "open" {
			t.Fatalf("confirmation task: %+v", item)
		}
		if a, err := st.s.AISettings(context.Background(), "alice-id"); err != nil || a.Status != store.AIError || a.LastError.String != AIErrorKeyInvalid {
			t.Fatalf("AI settings: %+v %v", a, err)
		}
	})
}

// Tasks of the same matter in a room are put under a group parent.
func TestGroupSameMatter(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		st := newSyncTest(t, driver)
		st.cw.room("2", "Team", "group")
		m1 := st.say("2", bob, "[To:101]障害の調査お願いします", time.Minute)
		st.ai.set(aiAction("障害を調査する"))
		st.sync()
		first := st.mustItem(mentionKey("2", m1))

		st.advance(time.Minute)
		m2 := st.say("2", bob, "[To:101]報告書もお願いします", 10*time.Second)
		st.ai.set(aiGrouped("報告書を書く", "決済障害の対応", "t1"))
		st.sync()
		if got := related(st.ai.input(t, 1)); !reflect.DeepEqual(got, []string{"t1:障害を調査する"}) {
			t.Fatalf("related tasks: %v", got)
		}
		var groups []store.ActionItem
		for _, i := range st.items() {
			if strings.HasPrefix(i.SourceKey.String, "chatwork:101:g:2:") {
				groups = append(groups, i)
			}
		}
		second := st.mustItem(mentionKey("2", m2))
		if len(groups) != 1 || groups[0].Content != "決済障害の対応" || groups[0].SourceKey.String != "chatwork:101:g:2:"+second.ID {
			t.Fatalf("groups: %+v", groups)
		}
		g := groups[0]
		refs := refsOf(t, g)
		if refs[0]["url"] != "https://www.chatwork.com/#!rid2" || refs[0]["message_id"] != "" || refs[0]["conversation_name"] != "Team" {
			t.Fatalf("group refs: %s", g.Refs)
		}
		if first = st.mustItem(mentionKey("2", m1)); first.ParentID.String != g.ID || second.ParentID.String != g.ID {
			t.Fatalf("parents: %+v %+v", first, second)
		}
	})
}

// Detections are notified only for messages sent after the connection and
// the AI settings were made, within notifyAge.
func TestNotifications(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		// Before the connection.
		st := newBareSyncTest(t, driver)
		st.saveAI()
		st.cw.room("2", "Team", "group")
		early := st.say("2", bob, "[To:101]接続前", time.Minute)
		st.ai.set(aiAction("接続前の対応"))
		st.sync()
		if st.mustItem(mentionKey("2", early)).Status != "open" || len(st.pending()) != 0 {
			t.Fatalf("message before the connection notified: %+v", st.pending())
		}

		// Before the AI settings.
		st = newBareSyncTest(t, driver)
		st.sync()
		st.advance(30 * time.Minute)
		st.cw.room("2", "Team", "group")
		beforeAI := st.say("2", bob, "[To:101]AI 前", time.Minute)
		st.saveAI()
		st.ai.set(aiAction("AI 前の対応"))
		st.sync()
		if st.mustItem(mentionKey("2", beforeAI)).Status != "open" || len(st.pending()) != 0 {
			t.Fatalf("message before the AI notified: %+v", st.pending())
		}

		// Older than notifyAge.
		st = newSyncTest(t, driver)
		st.cw.room("2", "Team", "group")
		old := st.say("2", bob, "[To:101]古い", 0)
		st.advance(3 * time.Hour)
		recent := st.say("2", bob, "[To:101]新しい", time.Minute)
		st.ai.set(aiAction("新しい対応"), aiAction("古い対応"))
		st.sync()
		p := st.pending()
		if st.mustItem(mentionKey("2", old)).Content != "古い対応" || len(p) != 1 || p[0].ActionItemID.String != st.mustItem(mentionKey("2", recent)).ID {
			t.Fatalf("notifications: %+v", p)
		}
	})
}
