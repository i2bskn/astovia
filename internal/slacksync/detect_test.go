package slacksync

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/i2bskn/astovia/internal/ai"
	"github.com/i2bskn/astovia/internal/store"
)

func TestMentionDetection(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		st := newSyncTest(t, driver)
		st.advance(time.Minute)
		c1, m1, self, m3, bot, join := st.ts(10*time.Minute, 1), st.ts(5*time.Minute, 1), st.ts(4*time.Minute, 1), st.ts(3*time.Minute, 1), st.ts(2*time.Minute, 1), st.ts(time.Minute, 1)
		st.sl.post("C1", msg{TS: c1, User: "U2", Text: "hello"})
		st.sl.post("C1", msg{TS: m1, User: "U2", Text: "<@U1> can you check <#C2|dev>?"})
		st.sl.post("C1", msg{TS: self, User: me, Text: "<@U1> note to self"})
		st.sl.post("C1", msg{TS: m3, User: "U3", Text: "thanks <@U1>"})
		st.sl.post("C1", msg{TS: bot, User: "U2", BotID: "B1", Text: "<@U1> deploy done"})
		st.sl.post("C1", msg{TS: join, User: "U3", Subtype: "channel_join", Text: "<@U1> joined"})
		// The AI reads the times in the time zone of the notification settings.
		if err := st.s.SaveNotificationSettings(context.Background(), store.NotificationSettings{UserID: "alice-id", Timezone: "Asia/Tokyo", DigestHour: store.DefaultDigestHour, StaleDays: store.DefaultStaleDays, UpdatedAt: st.now.Unix()}); err != nil {
			t.Fatal(err)
		}
		// Newest first: the bot and the join are dropped by their context.
		st.ai.set(aiNoAction("carol さんがお礼"), aiAction("確認して回答する"))
		c := st.sync()
		if st.ai.count() != 2 || c.LastSyncedAt.Int64 != st.now.Unix() || c.LastError.Valid || c.NextPollAt != st.now.Unix()+60 {
			t.Fatalf("AI calls %d, connection %+v", st.ai.count(), c)
		}
		open := st.mustItem(mentionKey("C1", "-", m1))
		if open.Status != "open" || open.Content != "確認して回答する" || open.Tracking != 1 || open.Source.String != "slack" || open.AutoReason.Valid {
			t.Fatalf("open item: %+v", open)
		}
		var refs []map[string]string
		if err := json.Unmarshal([]byte(open.Refs), &refs); err != nil || len(refs) != 1 || refs[0]["service"] != "slack" || refs[0]["account"] != "Acme" || refs[0]["conversation_id"] != "C1" || refs[0]["conversation_name"] != "#name-C1" || refs[0]["message_id"] != m1 || refs[0]["url"] != link("C1", m1, "") {
			t.Fatalf("refs: %s", open.Refs)
		}
		done := st.mustItem(mentionKey("C1", "-", m3))
		if done.Status != "done" || done.Content != "carol さんがお礼" || done.AutoReason.String != ReasonNoAction || done.Tracking != 0 || done.AutoRef.String != link("C1", m3, "") {
			t.Fatalf("done item: %+v", done)
		}
		if len(st.items()) != 2 {
			t.Fatalf("items: %d", len(st.items()))
		}
		p := st.pending()
		if len(p) != 1 || p[0].Title != "Slack: メンション" || p[0].Body != "確認して回答する" || p[0].ActionItemID.String != open.ID {
			t.Fatalf("notifications: %+v", p)
		}
		in := st.ai.input(t, 2)
		me := in["me"].(map[string]any)
		if in["_system"] != ai.SlackSystemPrompt || me["id"] != "U1" || me["name"] != "alice" || in["workspace"] != "Acme" || in["conversation_type"] != "channel" || in["channel_name"] != "name-C1" || len(in["open_tasks"].([]any)) != 0 {
			t.Fatalf("input: %+v", in)
		}
		got := posts(in)
		if len(got) < 2 || got[0] != [2]string{c1, "-"} || got[1] != [2]string{m1, "T"} {
			t.Fatalf("posts: %v", got)
		}
		body := in["posts"].([]any)[1].(map[string]any)
		if in["timezone"] != "Asia/Tokyo" || !strings.HasSuffix(body["at"].(string), "+09:00") {
			t.Fatalf("time zone: %v %v", in["timezone"], body["at"])
		}
		if body["body"] != "@alice(U1) can you check #dev?" || body["author"] != "bob" || body["author_id"] != "U2" || body["is_me"] != false {
			t.Fatalf("target post: %+v", body)
		}
		for _, p := range in["posts"].([]any) {
			if pm := p.(map[string]any); pm["id"] == self && pm["is_me"] != true {
				t.Fatalf("own post: %+v", pm)
			}
		}

		// Known messages are not read or judged again.
		st.sl.reset()
		st.advance(time.Minute)
		st.ai.set()
		st.sync()
		if st.ai.count() != 0 || st.sl.count("conversations.history") != 4 {
			t.Fatalf("second sync: AI %d, history %d", st.ai.count(), st.sl.count("conversations.history"))
		}
	})
}

// Candidates are tracked but not notified, and a reply resolves them.
// Events are not tracked; an event that has started is dismissed at once.
func TestCandidatesAndEvents(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		st := newSyncTest(t, driver)
		st.advance(time.Minute)
		m1, m2, m3 := st.ts(5*time.Minute, 1), st.ts(4*time.Minute, 1), st.ts(3*time.Minute, 1)
		st.sl.post("C1", msg{TS: m1, User: "U2", Text: "<@U3> please fix this, cc <@U1>"})
		st.sl.post("C1", msg{TS: m2, User: "U2", Text: "<@U1> meeting tomorrow 12:30"})
		st.sl.post("C1", msg{TS: m3, User: "U2", Text: "<@U1> meeting an hour ago"})
		start := st.now.Add(24 * time.Hour).Truncate(time.Minute)
		passed := st.now.Add(-time.Hour).Truncate(time.Minute)
		local := func(v time.Time) string { return v.UTC().Format("2006-01-02T15:04") }
		// Newest first.
		st.ai.set(
			aiJudged(ai.DecisionTask, "打ち合わせを登録する", "s", "打ち合わせ", local(passed), ""),
			aiJudged(ai.DecisionTask, "打ち合わせを登録する", "s", "打ち合わせ", local(start), local(start.Add(30*time.Minute))),
			aiCandidate("修正を確認する"),
		)
		st.sync()
		if st.ai.count() != 3 {
			t.Fatalf("AI calls: %d", st.ai.count())
		}
		candidate := st.mustItem(mentionKey("C1", "-", m1))
		if candidate.Status != "candidate" || candidate.Content != "修正を確認する" || candidate.Tracking != 1 || candidate.Kind != store.KindTask {
			t.Fatalf("candidate: %+v", candidate)
		}
		event := st.mustItem(mentionKey("C1", "-", m2))
		if event.Status != "open" || event.Tracking != 0 || event.Kind != store.KindEvent || event.EventTitle.String != "打ち合わせ" || event.EventStartAt.Int64 != start.Unix() || event.EventEndAt.Int64 != start.Add(30*time.Minute).Unix() || event.DueAt.Int64 != start.Unix() {
			t.Fatalf("event: %+v", event)
		}
		gone := st.mustItem(mentionKey("C1", "-", m3))
		if gone.Status != "dismissed" || gone.Tracking != 0 || gone.Kind != store.KindEvent || gone.AutoReason.String != store.ReasonEventPassed || gone.AutoRef.String != link("C1", m3, "") || gone.AutoAt.Int64 != st.now.Unix() {
			t.Fatalf("passed event: %+v", gone)
		}
		if p := st.pending(); len(p) != 1 || p[0].ActionItemID.String != event.ID {
			t.Fatalf("notifications: %+v", p)
		}

		// The user's reply resolves the candidate; the event is not tracked.
		st.sl.reset()
		st.advance(5 * time.Minute)
		r := st.reply("C1", m1, "直しました", time.Minute)
		st.ai.set(aiResolved(true))
		st.sync()
		done := st.mustItem(mentionKey("C1", "-", m1))
		if done.Status != "done" || done.AutoReason.String != ReasonReplyResolved || done.AutoRef.String != link("C1", r, "") {
			t.Fatalf("resolved candidate: %+v", done)
		}
		if st.ai.count() != 1 {
			t.Fatalf("reply judgments: %d", st.ai.count())
		}
		if got := st.mustItem(mentionKey("C1", "-", m2)); got.Status != "open" || got.Version != 1 {
			t.Fatalf("event changed: %+v", got)
		}
	})
}

func TestThreadMentionAndOpenTasks(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		st := newSyncTest(t, driver)
		st.advance(time.Minute)
		parent, r1, r2 := st.ts(20*time.Minute, 1), st.ts(5*time.Minute, 1), st.ts(4*time.Minute, 1)
		top := st.ts(3*time.Minute, 1)
		st.sl.post("C1", msg{TS: parent, User: "U2", Text: "topic", ThreadTS: parent})
		st.sl.post("C1", msg{TS: r1, User: "U2", Text: "<@U1> review this", ThreadTS: parent})
		st.sl.post("C1", msg{TS: r2, User: "U3", Text: "<@U1> also review this", ThreadTS: parent})
		st.sl.post("C1", msg{TS: top, User: "U3", Text: "<@U1> other topic"})
		// Newest first: top, r2, r1, so r1 sees the task made from r2.
		st.ai.set(aiAction("別件に答える"), aiAction("レビューする"), aiNoAction("同じ依頼"))
		st.sync()
		if st.ai.count() != 3 {
			t.Fatalf("AI calls: %d", st.ai.count())
		}
		st.mustItem(mentionKey("C1", parent, r2))
		if i := st.mustItem(mentionKey("C1", parent, r1)); i.Status != "done" {
			t.Fatalf("r1: %+v", i)
		}
		// A top-level mention gets no open tasks; a thread mention gets the
		// thread's.
		if tasks := st.ai.input(t, 1)["open_tasks"].([]any); len(tasks) != 0 {
			t.Fatalf("top-level open tasks: %v", tasks)
		}
		if tasks := st.ai.input(t, 2)["open_tasks"].([]any); len(tasks) != 0 {
			t.Fatalf("first thread open tasks: %v", tasks)
		}
		if tasks := st.ai.input(t, 3)["open_tasks"].([]any); !reflect.DeepEqual(tasks, []any{"レビューする"}) {
			t.Fatalf("second thread open tasks: %v", tasks)
		}
		got := posts(st.ai.input(t, 3))
		if got[0] != [2]string{parent, "-"} || got[1] != [2]string{r1, "T"} || got[2] != [2]string{r2, "-"} {
			t.Fatalf("thread posts: %v", got)
		}
	})
}

func TestDMRuns(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		st := newSyncTest(t, driver)
		st.advance(time.Minute)
		a1, a2, own, a3 := st.ts(50*time.Second, 1), st.ts(49*time.Second, 1), st.ts(48*time.Second, 1), st.ts(47*time.Second, 1)
		st.sl.post("D1", msg{TS: a1, User: "U2", Text: "hi"})
		st.sl.post("D1", msg{TS: a2, User: "U2", Text: "<@U1> question?"})
		st.sl.post("D1", msg{TS: own, User: me, Text: "yes?"})
		st.sl.post("D1", msg{TS: a3, User: "U2", Text: "please send the doc"})
		st.ai.set(aiNoAction("挨拶"), aiAction("資料を送る"))
		st.sync()
		if st.ai.count() != 2 {
			t.Fatalf("AI calls: %d", st.ai.count())
		}
		// The mention inside the DM is judged as part of the DM.
		if _, ok := st.item(mentionKey("D1", "-", a2)); ok {
			t.Fatal("DM mention judged as a mention")
		}
		st.mustItem(dmKey("D1", "-", a1, a2))
		item := st.mustItem(dmKey("D1", "-", a3, a3))
		if item.Content != "資料を送る" || item.Status != "open" {
			t.Fatalf("run item: %+v", item)
		}
		if p := st.pending(); len(p) != 1 || p[0].Title != "Slack: DM" {
			t.Fatalf("notifications: %+v", p)
		}
		in := st.ai.input(t, 1)
		if in["conversation_type"] != "dm" || !reflect.DeepEqual(posts(in), [][2]string{{a1, "T"}, {a2, "T"}, {own, "-"}, {a3, "-"}}) {
			t.Fatalf("first run: %v %v", in["conversation_type"], posts(in))
		}
		in = st.ai.input(t, 2)
		if !reflect.DeepEqual(posts(in), [][2]string{{a1, "-"}, {a2, "-"}, {own, "-"}, {a3, "T"}}) || len(in["open_tasks"].([]any)) != 0 {
			t.Fatalf("second run: %v %v", posts(in), in["open_tasks"])
		}

		// A message after a judged run starts a new run; judged messages
		// stay as context only.
		st.advance(time.Minute)
		a4 := st.ts(10*time.Second, 1)
		st.sl.post("D1", msg{TS: a4, User: "U2", Text: "also the slides"})
		st.ai.set(aiAction("スライドを送る"))
		st.sync()
		if st.ai.count() != 1 {
			t.Fatalf("AI calls: %d", st.ai.count())
		}
		st.mustItem(dmKey("D1", "-", a4, a4))
		in = st.ai.input(t, 1)
		if got := posts(in); got[len(got)-1] != [2]string{a4, "T"} || got[len(got)-2] != [2]string{a3, "-"} {
			t.Fatalf("new run: %v", got)
		}
		if !reflect.DeepEqual(in["open_tasks"], []any{"資料を送る"}) {
			t.Fatalf("open tasks: %v", in["open_tasks"])
		}

		// A reply in a thread is judged in its own conversation.
		st.advance(time.Minute)
		r1 := st.ts(5*time.Second, 1)
		st.sl.post("D1", msg{TS: r1, User: "U2", Text: "<@U1> in thread", ThreadTS: a3})
		st.ai.set(aiAction("スレッドに返す"))
		st.sync()
		st.mustItem(dmKey("D1", a3, r1, r1))
		if n := len(st.items()); n != 4 {
			t.Fatalf("items: %d", n)
		}
	})
}

func TestRunLimit(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		st := newSyncTest(t, driver)
		st.advance(time.Minute)
		var ts []string
		for i := 0; i < 12; i++ {
			ts = append(ts, st.ts(time.Duration(30-i)*time.Second, i))
			st.sl.post("D1", msg{TS: ts[i], User: "U2", Text: fmt.Sprintf("line %d", i)})
		}
		st.ai.set(aiAction("一つ目"), aiAction("二つ目"))
		st.sync()
		// The conversation is named after the other member.
		if name := refName(t, st.mustItem(dmKey("D1", "-", ts[0], ts[9]))); name != "DM（bob）" {
			t.Fatalf("DM name: %q", name)
		}
		st.mustItem(dmKey("D1", "-", ts[10], ts[11]))
		targets := 0
		for _, p := range posts(st.ai.input(t, 1)) {
			if p[1] == "T" {
				targets++
			}
		}
		if targets != 10 {
			t.Fatalf("targets: %d", targets)
		}
	})
}

func TestDetectionToggles(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		st := newSyncTest(t, driver)
		f := false
		st.settings(store.SlackSettings{DetectDMs: &f})
		st.advance(time.Minute)
		d, g := st.ts(time.Minute, 1), st.ts(time.Minute, 2)
		st.sl.post("D1", msg{TS: d, User: "U2", Text: "<@U1> dm"})
		st.sl.set(func(f *fakeSlack) { f.mpims = []string{"G1"} })
		st.sl.post("G1", msg{TS: g, User: "U3", Text: "<@U1> group"})
		// DMs and group DMs are off: both mentions are mentions.
		st.ai.set(aiAction("グループに答える"), aiAction("DM に答える"))
		st.sync()
		if st.sl.count("users.conversations") != 0 || st.sl.count("search.messages") != 1 {
			t.Fatalf("requests: %+v", st.sl.requests)
		}
		if i := st.mustItem(mentionKey("D1", "-", d)); i.Content != "DM に答える" {
			t.Fatalf("DM mention: %+v", i)
		}
		st.mustItem(mentionKey("G1", "-", g))
		if in := st.ai.input(t, 2); in["conversation_type"] != "dm" {
			t.Fatalf("conversation type: %v", in["conversation_type"])
		}
		for _, n := range st.pending() {
			if n.Title != "Slack: メンション" {
				t.Fatalf("title: %q", n.Title)
			}
		}

		// Group DMs on: a mention in a group DM that was read is part of it.
		on := true
		st.settings(store.SlackSettings{DetectGroupDMs: &on, DetectMentions: &f})
		st.advance(time.Minute)
		g2 := st.ts(10*time.Second, 1)
		st.sl.post("G1", msg{TS: g2, User: "U3", Text: "<@U1> again"})
		st.sl.reset()
		st.ai.set(aiAction("グループで答える"))
		st.sync()
		if st.sl.count("users.conversations") != 1 {
			t.Fatal("group DMs not listed")
		}
		for _, r := range st.sl.requests {
			if r.method == "search.messages" && strings.HasPrefix(r.query.Get("query"), "<@") {
				t.Fatal("mention search while mentions are off")
			}
		}
		// The group DM's messages not judged as a group DM yet form one run.
		if name := refName(t, st.mustItem(dmKey("G1", "-", g, g2))); name != "グループDM" {
			t.Fatalf("group DM name: %q", name)
		}
		if _, ok := st.item(mentionKey("G1", "-", g2)); ok {
			t.Fatal("group DM mention judged as a mention")
		}
		if in := st.ai.input(t, 1); in["conversation_type"] != "group_dm" {
			t.Fatalf("conversation type: %v", in["conversation_type"])
		}
	})
}

func TestGroupDMLimitAndMentions(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		st := newSyncTest(t, driver)
		on := true
		st.settings(store.SlackSettings{DetectGroupDMs: &on})
		st.advance(time.Minute)
		var ids []string
		for i := 0; i < 21; i++ {
			ids = append(ids, fmt.Sprintf("G%02d", i))
		}
		st.sl.set(func(f *fakeSlack) { f.mpims = ids })
		// The 21st group DM is not read: its mention is a mention.
		m := st.ts(30*time.Second, 1)
		st.sl.post("G20", msg{TS: m, User: "U2", Text: "<@U1> beyond the limit"})
		st.ai.set(aiAction("答える"))
		st.sync()
		reads := map[string]bool{}
		for _, r := range st.sl.requests {
			if r.method == "conversations.history" {
				reads[r.query.Get("channel")] = true
			}
		}
		for _, id := range ids[:20] {
			if !reads[id] {
				t.Fatalf("%s not read: %v", id, reads)
			}
		}
		st.mustItem(mentionKey("G20", "-", m))
	})
}

func TestAICallLimitAndRetryLimit(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		st := newSyncTest(t, driver)
		before := st.conn().LastSyncedAt
		st.advance(time.Minute)
		var ts []string
		for i := 0; i < 5; i++ {
			ts = append(ts, st.ts(time.Duration(50-i)*time.Second, i))
			st.sl.post("C1", msg{TS: ts[i], User: "U2", Text: fmt.Sprintf("<@U1> task %d", i)})
		}
		st.ai.set(aiAction("a"), aiAction("b"), aiAction("c"))
		c := st.sync()
		if st.ai.count() != 3 || len(st.items()) != 3 || c.LastSyncedAt != before {
			t.Fatalf("AI %d, items %d, window %+v", st.ai.count(), len(st.items()), c.LastSyncedAt)
		}
		// The newest were judged first.
		st.mustItem(mentionKey("C1", "-", ts[4]))
		if _, ok := st.item(mentionKey("C1", "-", ts[0])); ok {
			t.Fatal("oldest judged first")
		}

		// A temporary AI failure postpones the rest.
		st.advance(time.Minute)
		st.ai.set(aiError(529))
		if c = st.sync(); len(st.items()) != 3 || c.LastSyncedAt != before {
			t.Fatalf("after failure: items %d, window %+v", len(st.items()), c.LastSyncedAt)
		}

		// After six hours the window moves on and what is left becomes a
		// confirmation task (old messages after a temporary failure too).
		st.advance(6 * time.Hour)
		st.ai.set(aiError(529))
		c = st.sync()
		if c.LastSyncedAt.Int64 != st.now.Unix() || len(st.items()) != 5 {
			t.Fatalf("overdue: items %d, window %+v", len(st.items()), c.LastSyncedAt)
		}
		fb := st.mustItem(mentionKey("C1", "-", ts[0]))
		if fb.Status != "open" || fb.Tracking != 1 || fb.Kind != store.KindTask || fb.Content != "Slack を確認: @alice(U1) task 0" {
			t.Fatalf("fallback: %+v", fb)
		}
		// Messages older than two hours are not notified.
		for _, n := range st.pending() {
			if n.ActionItemID.String == fb.ID {
				t.Fatal("old message notified")
			}
		}
	})
}

func TestNotificationAge(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		st := newBareSyncTest(t, driver)
		st.saveAI()
		// The first synchronization imports without notifying.
		m0 := st.ts(time.Hour, 1)
		st.sl.post("C1", msg{TS: m0, User: "U2", Text: "<@U1> before"})
		st.ai.set(aiAction("最初"))
		st.sync()
		st.mustItem(mentionKey("C1", "-", m0))
		if len(st.pending()) != 0 {
			t.Fatal("initial import notified")
		}
		// A message postponed for more than two hours is created without a
		// notification; a recent one is notified.
		st.advance(time.Minute)
		m1 := st.ts(10*time.Second, 1)
		st.sl.post("C1", msg{TS: m1, User: "U2", Text: "<@U1> later"})
		st.ai.set(aiError(500))
		st.sync()
		st.advance(3 * time.Hour)
		m2 := st.ts(10*time.Second, 2)
		st.sl.post("C1", msg{TS: m2, User: "U2", Text: "<@U1> now"})
		st.ai.set(aiAction("今"), aiAction("前"))
		st.sync()
		i1, i2 := st.mustItem(mentionKey("C1", "-", m1)), st.mustItem(mentionKey("C1", "-", m2))
		p := st.pending()
		if len(p) != 1 || p[0].ActionItemID.String != i2.ID || i1.Content != "前" {
			t.Fatalf("notifications: %+v %+v", p, i1)
		}
	})
}

func TestWindow(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	day := int64(86400)
	for _, tc := range []struct {
		name                      string
		synced, connected, active int64
		from                      int64
		overdue, first            bool
	}{
		{"first", 0, now.Unix() - 60, now.Unix() - 3600, now.Unix() - 60 - day, false, true},
		{"after a sync", now.Unix() - 120, now.Unix() - 10*day, now.Unix() - 10*day, now.Unix() - 720, false, false},
		{"72 hours", now.Unix() - 5*day, now.Unix() - 10*day, now.Unix() - 10*day, now.Unix() - 3*day, true, false},
		{"AI added later", now.Unix() - 2*day, now.Unix() - 10*day, now.Unix() - 3600, now.Unix() - 3600 - day, true, false},
		{"reconnected", 0, now.Unix() - 7*3600, now.Unix() - 10*day, now.Unix() - 7*3600 - day, true, true},
	} {
		c := store.SlackConnection{ConnectedAt: tc.connected}
		if tc.synced != 0 {
			c.LastSyncedAt.Int64, c.LastSyncedAt.Valid = tc.synced, true
		}
		r := &run{conn: c, state: c, start: now, ai: &store.AISettings{ActivatedAt: tc.active}}
		r.setWindow()
		if r.from != tc.from || r.to != now.Unix() || r.overdue != tc.overdue || r.firstImport != tc.first {
			t.Fatalf("%s: from %d (want %d) overdue %v first %v", tc.name, r.from, tc.from, r.overdue, r.firstImport)
		}
	}
}

func TestSearchTruncation(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		st := newSyncTest(t, driver)
		st.advance(time.Minute)
		st.sl.set(func(f *fakeSlack) { f.pageSize = 1 })
		var ts []string
		for i := 0; i < 3; i++ {
			ts = append(ts, st.ts(time.Duration(50-i)*time.Second, i))
			st.sl.post("C1", msg{TS: ts[i], User: "U2", Text: "<@U1> x"})
		}
		st.ai.set(aiAction("a"), aiAction("b"))
		c := st.sync()
		if c.LastSyncedAt.Int64 != st.now.Unix() || st.ai.count() != 2 || len(st.items()) != 2 {
			t.Fatalf("window %+v, AI %d, items %d", c.LastSyncedAt, st.ai.count(), len(st.items()))
		}
		if _, ok := st.item(mentionKey("C1", "-", ts[0])); ok {
			t.Fatal("third page read")
		}
	})
}

func TestSkippedConversationsAndBudget(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		st := newSyncTest(t, driver)
		st.advance(time.Minute)
		// A DM Slack refuses is skipped and the window still moves.
		d := st.ts(30*time.Second, 1)
		st.sl.post("D1", msg{TS: d, User: "U2", Text: "hi"})
		st.sl.set(func(f *fakeSlack) {
			f.forced["conversations.history"], f.forcedChannel["conversations.history"] = "channel_not_found", "D1"
		})
		c := st.sync()
		if c.LastSyncedAt.Int64 != st.now.Unix() || len(st.items()) != 0 || st.ai.count() != 0 || c.LastError.Valid {
			t.Fatalf("skipped: %+v items %d", c, len(st.items()))
		}
		// A mention whose context cannot be read is judged without it.
		st.advance(time.Minute)
		st.sl.set(func(f *fakeSlack) { f.forcedChannel["conversations.history"] = "C1" })
		m := st.ts(10*time.Second, 1)
		st.sl.post("C1", msg{TS: m, User: "U2", Text: "<@U1> hi"})
		st.ai.set(aiAction("返す"))
		st.sync()
		st.mustItem(mentionKey("C1", "-", m))
		if got := posts(st.ai.input(t, 1)); len(got) != 1 {
			t.Fatalf("posts without context: %v", got)
		}

		// The history budget carries the rest over.
		st.sl.set(func(f *fakeSlack) { delete(f.forced, "conversations.history") })
		st.advance(time.Minute)
		before := st.conn().LastSyncedAt
		for i := 0; i < 45; i++ {
			st.sl.post(fmt.Sprintf("D%02d", i+10), msg{TS: st.ts(time.Duration(50-i)*time.Second, i), User: "U2", Text: "hello"})
		}
		st.sl.reset()
		st.ai.set()
		c = st.sync()
		if n := st.sl.count("conversations.history"); n != 40 || c.LastSyncedAt != before {
			t.Fatalf("history calls %d, window %+v", n, c.LastSyncedAt)
		}
	})
}

func TestConnectionErrors(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		for _, tc := range []struct {
			name    string
			set     func(f *fakeSlack)
			status  string
			code    string
			next    int64
			failure int64
		}{
			{"invalid_auth", func(f *fakeSlack) { f.forced["search.messages"] = "invalid_auth" }, store.SlackError, ErrorTokenInvalid, -1, 0},
			{"missing_scope", func(f *fakeSlack) { f.forced["search.messages"] = "missing_scope" }, store.SlackError, ErrorScopesMissing, -1, 0},
			{"429", func(f *fakeSlack) { f.statuses["search.messages"], f.retryAfter = 429, "30" }, store.SlackActive, ErrorRateLimited, 30, 0},
			{"429 without Retry-After", func(f *fakeSlack) { f.statuses["search.messages"] = 429 }, store.SlackActive, ErrorRateLimited, 900, 0},
			{"500", func(f *fakeSlack) { f.statuses["search.messages"] = 500 }, store.SlackActive, ErrorUnavailable, 60, 1},
			{"internal_error", func(f *fakeSlack) { f.forced["search.messages"] = "internal_error" }, store.SlackActive, ErrorUnavailable, 60, 1},
		} {
			t.Run(tc.name, func(t *testing.T) {
				st := newSyncTest(t, driver)
				before := st.conn()
				st.advance(time.Minute)
				st.sl.set(tc.set)
				c := st.sync()
				if c.Status != tc.status || c.LastError.String != tc.code || c.FailureCount != tc.failure || c.LastSyncedAt != before.LastSyncedAt {
					t.Fatalf("connection: %+v", c)
				}
				if tc.next >= 0 && c.NextPollAt != st.now.Unix()+tc.next {
					t.Fatalf("next poll: %d", c.NextPollAt-st.now.Unix())
				}
				if tc.failure > 0 {
					st.sync()
					if c = st.conn(); c.FailureCount != 2 || c.NextPollAt != st.now.Unix()+120 {
						t.Fatalf("backoff: %+v", c)
					}
				}
			})
		}
	})
}

func TestWithoutAI(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		st := newBareSyncTest(t, driver)
		st.sl.post("C1", msg{TS: st.ts(time.Minute, 1), User: "U2", Text: "<@U1> hi"})
		c := st.sync()
		if st.sl.count("") != 0 || c.LastSyncedAt.Valid || c.LastTrackedAt.Valid || c.NextPollAt != st.now.Unix()+60 || len(st.items()) != 0 {
			t.Fatalf("without AI: %d requests, %+v", st.sl.count(""), c)
		}
		// Settings in error are the same as none.
		a := st.saveAI()
		if _, err := st.s.SetAIStatus(context.Background(), "alice-id", a.Generation, store.AIError, nullString("key_invalid"), st.now.Unix()); err != nil {
			t.Fatal(err)
		}
		if c = st.sync(); st.sl.count("") != 0 || c.LastSyncedAt.Valid {
			t.Fatalf("AI in error: %d requests", st.sl.count(""))
		}
		// An undecryptable token stops the connection.
		bad := st.conn()
		bad.TokenSealed = "broken"
		st.syncer.Sync(context.Background(), bad)
		if c = st.conn(); c.Status != store.SlackError || c.LastError.String != ErrorTokenUndecryptable {
			t.Fatalf("undecryptable: %+v", c)
		}
	})
}

func TestDetectionAIErrors(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		st := newSyncTest(t, driver)
		st.advance(time.Minute)
		m1, m2 := st.ts(20*time.Second, 1), st.ts(10*time.Second, 1)
		st.sl.post("C1", msg{TS: m1, User: "U2", Text: "<@U1> one"})
		st.sl.post("C1", msg{TS: m2, User: "U2", Text: "<@U1>\ntwo"})
		// A rejected request is a confirmation task; a key error stops the
		// AI and the rest become confirmation tasks.
		st.ai.set(func(w http.ResponseWriter) { aiError(400)(w) }, aiError(401))
		c := st.sync()
		if c.LastSyncedAt.Int64 != st.now.Unix() {
			t.Fatalf("window: %+v", c)
		}
		if i := st.mustItem(mentionKey("C1", "-", m2)); i.Content != "Slack を確認: @alice(U1) two" || i.Status != "open" {
			t.Fatalf("rejected: %+v", i)
		}
		st.mustItem(mentionKey("C1", "-", m1))
		if a := st.aiSettings(); a.Status != store.AIError || a.LastError.String != AIErrorKeyInvalid {
			t.Fatalf("AI settings: %+v", a)
		}
	})
}

// refName returns the conversation name of an item's only ref.
func refName(t *testing.T, item store.ActionItem) string {
	t.Helper()
	var refs []map[string]string
	if err := json.Unmarshal([]byte(item.Refs), &refs); err != nil || len(refs) != 1 {
		t.Fatalf("refs: %s", item.Refs)
	}
	return refs[0]["conversation_name"]
}
