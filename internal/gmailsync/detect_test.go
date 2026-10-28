package gmailsync

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/i2bskn/astovia/internal/ai"
	"github.com/i2bskn/astovia/internal/store"
)

// bodyLines splits a post body into its four header lines and the text.
func bodyLines(t *testing.T, p map[string]any) ([]string, string) {
	t.Helper()
	body := p["body"].(string)
	head, text, ok := strings.Cut(body, "\n\n")
	lines := strings.Split(head, "\n")
	if !ok || len(lines) != 4 {
		t.Fatalf("body: %q", body)
	}
	return lines, text
}

func TestDetectTask(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		st := newSyncTest(t, driver)
		th := st.recv("", "見積もりの件", "見積もりを送ってください。", time.Minute)
		st.ai.set(aiAction("見積もりを送る"))
		a := st.sync(me)
		item := st.mustItem(runKey(me, th, th, th))
		if item.Content != "見積もりを送る" || item.Status != "open" || item.Tracking != 1 || item.Source.String != store.SourceGmail || a.LastSyncedAt.Int64 != st.now.Unix() {
			t.Fatalf("item: %+v %+v", item, a)
		}
		refs := refsOf(t, item)
		want := map[string]string{"service": "gmail", "account": me, "conversation_id": th, "conversation_name": "見積もりの件", "message_id": th, "url": "https://mail.google.com/mail/?authuser=alice%40example.com#all/" + th}
		if len(refs) != 1 || !reflect.DeepEqual(refs[0], want) {
			t.Fatalf("refs: %v", refs)
		}
		p := st.pending()
		if len(p) != 1 || p[0].Title != "メール: 見積もりの件" || p[0].Body != "見積もりを送る" || p[0].URL != "/tasks/"+item.ID {
			t.Fatalf("notification: %+v", p)
		}
		in := st.ai.input(t, 1)
		if in["_system"] != ai.MailSystemPrompt || in["conversation_type"] != "email" || in["channel_name"] != "見積もりの件" || in["workspace"] != me {
			t.Fatalf("input: %v", in)
		}
		if m := in["me"].(map[string]any); m["id"] != me || m["name"] != me {
			t.Fatalf("me: %v", m)
		}
		posts := st.ai.posts(t, 1)
		lines, text := bodyLines(t, posts[0])
		if len(posts) != 1 || posts[0]["author"] != "Bob" || posts[0]["author_id"] != "bob@example.com" || posts[0]["is_me"] != false || posts[0]["target"] != true ||
			!reflect.DeepEqual(lines, []string{"件名: 見積もりの件", "自分の位置: To", "宛先: Alice <alice@example.com>", "CC: "}) || text != "見積もりを送ってください。" {
			t.Fatalf("posts: %v", posts)
		}
		// The query reads the inbox from an hour before the window.
		if q := st.g.queries[0]; q != fmt.Sprintf("in:inbox -in:chats -category:promotions -category:social -category:forums -category:updates after:%d", st.now.Add(-20*time.Minute-time.Hour).Unix()) {
			t.Fatalf("query: %s", q)
		}
		// Judged once.
		st.advance(time.Minute)
		st.sync(me)
		if st.ai.count() != 1 || len(st.items()) != 1 {
			t.Fatalf("judged again: %d %d", st.ai.count(), len(st.items()))
		}
	})
}

func TestDetectOutcomes(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		st := newSyncTest(t, driver)
		cand := st.recv("", "候補", "できれば見てください", time.Minute)
		none := st.recv("", "お知らせ", "共有です", 2*time.Minute)
		empty := st.recv("", "空", "x", 3*time.Minute)
		st.ai.set(aiCandidate("確認する"), aiNoAction("共有のみ"), aiNoAction(""))
		st.sync(me)
		if i := st.mustItem(runKey(me, cand, cand, cand)); i.Status != "candidate" || i.Tracking != 1 {
			t.Fatalf("candidate: %+v", i)
		}
		i := st.mustItem(runKey(me, none, none, none))
		if i.Status != "done" || i.Content != "共有のみ" || i.AutoReason.String != ReasonNoAction || i.Tracking != 0 || i.AutoRef.String != threadURL(me, none) {
			t.Fatalf("none: %+v", i)
		}
		if i = st.mustItem(runKey(me, empty, empty, empty)); i.Content != "メール（対応不要）" {
			t.Fatalf("empty summary: %+v", i)
		}
		if len(st.pending()) != 0 {
			t.Fatalf("notified: %+v", st.pending())
		}

		// A rejected request makes a confirmation task: the subject, or the
		// body without one.
		st.advance(time.Minute)
		subj := st.recv("", "請求書の件", "本文", time.Minute)
		bodyOnly := st.recv("", "", "件名のない本文です", 2*time.Minute)
		st.ai.set(aiError(400), aiError(400))
		st.sync(me)
		if i = st.mustItem(runKey(me, subj, subj, subj)); i.Content != "メールを確認: 請求書の件" || i.Status != "open" {
			t.Fatalf("fallback: %+v", i)
		}
		if i = st.mustItem(runKey(me, bodyOnly, bodyOnly, bodyOnly)); i.Content != "メールを確認: 件名のない本文です" {
			t.Fatalf("fallback body: %+v", i)
		}
		if p := st.pending(); len(p) != 2 || p[0].Title != "メール: 請求書の件" && p[1].Title != "メール: 請求書の件" {
			t.Fatalf("fallback notifications: %+v", p)
		}
		for _, n := range st.pending() {
			if n.Title != "メール: 請求書の件" && n.Title != "メール" {
				t.Fatalf("title: %q", n.Title)
			}
		}

		// An event becomes an untracked event item.
		st.advance(time.Minute)
		ev := st.recv("", "打ち合わせ", "10/30 10:00 から打ち合わせです", time.Minute)
		st.ai.set(aiJudged(ai.DecisionTask, "打ち合わせを登録する", "s", "定例", "2026-10-30T10:00", ""))
		st.sync(me)
		if i = st.mustItem(runKey(me, ev, ev, ev)); i.Kind != store.KindEvent || i.EventTitle.String != "定例" || i.Tracking != 0 || i.Status != "open" {
			t.Fatalf("event: %+v", i)
		}
	})
}

// Detections are notified only for messages received after the account
// was connected and the AI settings were made, within notifyAge, and while
// the account notifies at once.
func TestNotifications(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		ctx := context.Background()
		// Before the connection.
		st := newBareSyncTest(t, driver)
		st.saveAI()
		early := st.recv("", "接続前", "接続前", time.Minute)
		st.ai.set(aiAction("接続前の対応"))
		st.sync(me)
		if st.mustItem(runKey(me, early, early, early)).Status != "open" || len(st.pending()) != 0 {
			t.Fatalf("message before the connection notified: %+v", st.pending())
		}

		// Before the AI settings.
		st = newBareSyncTest(t, driver)
		st.sync(me)
		st.advance(30 * time.Minute)
		beforeAI := st.recv("", "AI 前", "AI 前", time.Minute)
		st.saveAI()
		st.ai.set(aiAction("AI 前の対応"))
		st.sync(me)
		if st.mustItem(runKey(me, beforeAI, beforeAI, beforeAI)).Status != "open" || len(st.pending()) != 0 {
			t.Fatalf("message before the AI notified: %+v", st.pending())
		}

		// Older than notifyAge.
		st = newSyncTest(t, driver)
		old := st.recv("", "古い", "古い", 0)
		st.advance(3 * time.Hour)
		recent := st.recv("", "新しい", "新しい", time.Minute)
		st.ai.set(aiAction("新しい対応"), aiAction("古い対応"))
		st.sync(me)
		p := st.pending()
		if st.mustItem(runKey(me, old, old, old)).Content != "古い対応" || len(p) != 1 || p[0].ActionItemID.String != st.mustItem(runKey(me, recent, recent, recent)).ID {
			t.Fatalf("notifications: %+v", p)
		}

		// Not at once.
		st = newSyncTest(t, driver)
		if _, err := st.s.UpdateGmailSettings(ctx, "alice-id", "sid", st.accounts[me], false, st.now.Unix()); err != nil {
			t.Fatal(err)
		}
		quiet := st.recv("", "静か", "静か", time.Minute)
		st.ai.set(aiAction("静かな対応"))
		st.sync(me)
		if st.mustItem(runKey(me, quiet, quiet, quiet)).Status != "open" || len(st.pending()) != 0 {
			t.Fatalf("immediate_notify off: %+v", st.pending())
		}
	})
}

// Only messages from others in the inbox are targets; drafts, the trash
// and spam are not even context.
func TestTargets(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		st := newSyncTest(t, driver)
		th := st.recv("", "件", "1 通目", 9*time.Minute)
		auto := st.g.add(gMsg{Thread: th, At: st.now.Add(-8 * time.Minute), From: "Bot <bot@example.com>", To: me, Subject: "件", Body: "自動", Headers: map[string]string{"Auto-Submitted": "auto-generated"}})
		no := st.g.add(gMsg{Thread: th, At: st.now.Add(-8 * time.Minute).Add(time.Second), From: "Bob <bob@example.com>", To: me, Subject: "件", Body: "no は対象", Headers: map[string]string{"Auto-Submitted": " No "}})
		bulk := st.g.add(gMsg{Thread: th, At: st.now.Add(-7 * time.Minute), From: "List <list@example.com>", To: me, Subject: "件", Body: "一斉", Headers: map[string]string{"Precedence": "bulk"}})
		noreply := st.g.add(gMsg{Thread: th, At: st.now.Add(-7 * time.Minute).Add(time.Second), From: "Service <no-reply@example.com>", To: me, Subject: "件", Body: "通知"})
		archived := st.g.add(gMsg{Thread: th, Labels: []string{"UNREAD"}, At: st.now.Add(-6 * time.Minute), From: "Carol <carol@example.com>", To: me, Subject: "件", Body: "アーカイブ済み"})
		draft := st.g.add(gMsg{Thread: th, Labels: []string{"DRAFT"}, At: st.now.Add(-5 * time.Minute), From: me, To: "bob@example.com", Subject: "件", Body: "下書きの秘密"})
		trash := st.g.add(gMsg{Thread: th, Labels: []string{"TRASH"}, At: st.now.Add(-5 * time.Minute).Add(time.Second), From: "Bob <bob@example.com>", To: me, Subject: "件", Body: "ゴミ箱"})
		spam := st.g.add(gMsg{Thread: th, Labels: []string{"SPAM"}, At: st.now.Add(-5 * time.Minute).Add(2 * time.Second), From: "Bob <bob@example.com>", To: me, Subject: "件", Body: "迷惑"})
		third := st.recv(th, "件", "3 通目", 4*time.Minute)
		mine := st.sent(th, "返信しました", 3*time.Minute)
		after := st.recv(th, "件", "返信の後", 2*time.Minute)
		st.ai.set(aiAction("一つ目"), aiAction("二つ目"))
		st.sync(me)
		// The automatic and archived messages neither join nor split the
		// first run; the user's message splits it from the last.
		first := st.mustItem(runKey(me, th, th, third))
		second := st.mustItem(runKey(me, th, after, after))
		if first.Content != "一つ目" || second.Content != "二つ目" || len(st.items()) != 2 {
			t.Fatalf("items: %+v %+v", first, second)
		}
		posts := st.ai.posts(t, 1)
		var targets, ids []string
		for _, p := range posts {
			ids = append(ids, p["id"].(string))
			if p["target"] == true {
				targets = append(targets, p["id"].(string))
			}
		}
		if !reflect.DeepEqual(targets, []string{th, no, third}) || !reflect.DeepEqual(ids, []string{th, auto, no, bulk, noreply, archived, third, mine, after}) {
			t.Fatalf("posts: targets %v ids %v", targets, ids)
		}
		for _, id := range []string{draft, trash, spam} {
			for _, p := range posts {
				if p["id"] == id {
					t.Fatalf("%s sent to the AI", id)
				}
			}
		}
		// The user's message is is_me with the position of the sender, and
		// the user's name is its sender name.
		for _, p := range posts {
			if p["id"] == mine {
				lines, _ := bodyLines(t, p)
				if p["is_me"] != true || lines[1] != "自分の位置: 送信者（自分）" {
					t.Fatalf("mine: %v", p)
				}
			}
		}
		if m := st.ai.input(t, 1)["me"].(map[string]any); m["name"] != "Alice Liddell" {
			t.Fatalf("me: %v", m)
		}
	})
}

// A judged range is not judged again; the messages after it make a new
// run, and runs hold at most runLimit messages.
func TestRuns(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		st := newSyncTest(t, driver)
		th := st.recv("", "件", "1", 9*time.Minute)
		second := st.recv(th, "件", "2", 8*time.Minute)
		st.ai.set(aiAction("最初"))
		st.sync(me)
		st.mustItem(runKey(me, th, th, second))
		st.advance(time.Minute)
		var ids []string
		for i := 0; i < 12; i++ {
			ids = append(ids, st.recv(th, "件", fmt.Sprint("追加 ", i), time.Duration(50-i)*time.Second))
		}
		st.ai.set(aiAction("次"), aiAction("最後"))
		st.sync(me)
		st.mustItem(runKey(me, th, ids[0], ids[9]))
		st.mustItem(runKey(me, th, ids[10], ids[11]))
		if len(st.items()) != 3 || st.ai.count() != 2 {
			t.Fatalf("items: %d calls: %d", len(st.items()), st.ai.count())
		}
		var targets int
		for _, p := range st.ai.posts(t, 1) {
			if p["target"] == true {
				targets++
			}
		}
		if targets != runLimit {
			t.Fatalf("targets: %d", targets)
		}
	})
}

// The four header lines of a post are computed by Astovia.
func TestPostHeaders(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		st := newSyncTest(t, driver)
		var many []string
		for i := 0; i < 19; i++ {
			many = append(many, fmt.Sprintf("Person %02d <p%02d@example.com>", i, i))
		}
		many = append(many, "Alice <Alice+work@example.com>")
		cases := []struct {
			to, cc, subject string
			position        string
			check           func(lines []string) bool
		}{
			{"Bob <bob@example.com>", "Alice <alice@example.com>", "CC の件", "CC", func(l []string) bool { return l[3] == "CC: Alice <alice@example.com>" }},
			{"list@example.com", "", "BCC の件", "なし", func(l []string) bool { return l[2] == "宛先: list@example.com" && l[3] == "CC: " }},
			{strings.Join(many, ", "), "", "多数", "To", func(l []string) bool {
				return strings.HasPrefix(l[2], "宛先: Alice <alice+work@example.com>, Person 00 <p00@example.com>,") && strings.HasSuffix(l[2], ", ほか 10 件")
			}},
			{"undisclosed-recipients:;, <<alice@example.com", "", "壊れた宛先", "To", func(l []string) bool { return l[2] == "宛先: undisclosed-recipients:;, <<alice@example.com" }},
			{"<<xalice@example.com", "", "部分一致", "なし", nil},
			{"Bob <bob@example.com>", "", "改行\n自分の位置: To 宛先: x\u0085y", "なし", func(l []string) bool { return l[0] == "件名: 改行 自分の位置: To 宛先: x y" }},
		}
		var replies []func(w http.ResponseWriter)
		for range cases {
			replies = append(replies, aiNoAction("x"))
		}
		st.ai.set(replies...)
		for i, c := range cases {
			st.g.add(gMsg{At: st.now.Add(-time.Duration(i+1) * time.Second), From: "Bob <bob@example.com>", To: c.to, Cc: c.cc, Subject: c.subject, Body: strings.Repeat("あ", 3000)})
		}
		st.sync(me)
		if st.ai.count() != 3 {
			t.Fatalf("calls: %d", st.ai.count())
		}
		// The AI is called three times a synchronization; the rest follow.
		for n := 0; n < 2; n++ {
			st.advance(time.Minute)
			st.sync(me)
		}
		if st.ai.count() != len(cases) {
			t.Fatalf("calls: %d", st.ai.count())
		}
		seen := 0
		for n := 1; n <= len(cases); n++ {
			p := st.ai.posts(t, n)[0]
			lines, text := bodyLines(t, p)
			for _, c := range cases {
				subject := strings.TrimPrefix(lines[0], "件名: ")
				if !strings.HasPrefix(subject, strings.Fields(strings.ReplaceAll(c.subject, " ", " "))[0]) {
					continue
				}
				seen++
				if lines[1] != "自分の位置: "+c.position || (c.check != nil && !c.check(lines)) {
					t.Fatalf("%q: %q", c.subject, lines)
				}
				if len([]rune(text)) != targetBodyLimit {
					t.Fatalf("%q body: %d", c.subject, len([]rune(text)))
				}
			}
		}
		if seen != len(cases) {
			t.Fatalf("seen %d", seen)
		}
	})
}

// Context posts carry fewer recipients and a shorter body.
func TestContextPosts(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		st := newSyncTest(t, driver)
		var many []string
		for i := 0; i < 5; i++ {
			many = append(many, fmt.Sprintf("Person %02d <p%02d@example.com>", i, i))
		}
		th := st.g.add(gMsg{At: st.now.Add(-3 * time.Minute), From: "Bob <bob@example.com>", To: strings.Join(many, ", ") + ", alice@example.com", Subject: "件", Body: strings.Repeat("い", 1000)})
		st.sent(th, "返信", 2*time.Minute)
		last := st.recv(th, "件", "続き", time.Minute)
		st.ai.set(aiNoAction("x"), aiNoAction("y"))
		st.sync(me)
		// The second judgment is of the last message, with the first as
		// context.
		posts := st.ai.posts(t, 2)
		if len(posts) != 3 || posts[2]["id"] != last || posts[0]["id"] != th || posts[0]["target"] == true {
			t.Fatalf("posts: %v", posts)
		}
		lines, text := bodyLines(t, posts[0])
		if lines[2] != "宛先: alice@example.com, Person 00 <p00@example.com>, Person 01 <p01@example.com>, ほか 3 件" || len([]rune(text)) != contextBodyLimit {
			t.Fatalf("context: %q %d", lines, len([]rune(text)))
		}
	})
}

// Tasks of the same matter in a thread are put under a group parent.
func TestGroupSameMatter(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		st := newSyncTest(t, driver)
		th := st.recv("", "障害", "調査お願いします", time.Minute)
		st.ai.set(aiAction("障害を調査する"))
		st.sync(me)
		first := st.mustItem(runKey(me, th, th, th))

		st.advance(time.Minute)
		m2 := st.recv(th, "障害", "報告書もお願いします", 10*time.Second)
		st.ai.set(aiGrouped("報告書を書く", "決済障害の対応", "t1"))
		st.sync(me)
		related := st.ai.input(t, 1)["related_tasks"].([]any)
		if len(related) != 1 || related[0].(map[string]any)["task"] != "障害を調査する" {
			t.Fatalf("related: %v", related)
		}
		second := st.mustItem(runKey(me, th, m2, m2))
		var groups []store.ActionItem
		for _, i := range st.items() {
			if strings.HasPrefix(i.SourceKey.String, "gmail:"+me+":g:"+th+":") {
				groups = append(groups, i)
			}
		}
		if len(groups) != 1 || groups[0].Content != "決済障害の対応" || groups[0].SourceKey.String != "gmail:"+me+":g:"+th+":"+second.ID {
			t.Fatalf("groups: %+v", groups)
		}
		refs := refsOf(t, groups[0])
		if refs[0]["url"] != threadURL(me, th) || refs[0]["message_id"] != "" || refs[0]["conversation_name"] != "障害" {
			t.Fatalf("group refs: %s", groups[0].Refs)
		}
		if first = st.mustItem(runKey(me, th, th, th)); first.ParentID.String != groups[0].ID || second.ParentID.String != groups[0].ID {
			t.Fatalf("parents: %+v %+v", first, second)
		}
	})
}

// The window stays when something is carried over, and moves when an
// overdue window gives up.
func TestWindowCarry(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		st := newSyncTest(t, driver)
		start := st.account(me).LastSyncedAt
		for i := 0; i < 4; i++ {
			st.recv("", fmt.Sprint("件 ", i), "本文", time.Duration(i+1)*time.Minute)
		}
		st.ai.set(aiAction("a"), aiAction("b"), aiAction("c"), aiAction("d"))
		a := st.sync(me)
		if st.ai.count() != 3 || a.LastSyncedAt != start || len(st.items()) != 3 {
			t.Fatalf("carried: %d %+v %d", st.ai.count(), a, len(st.items()))
		}
		st.advance(time.Minute)
		if a = st.sync(me); st.ai.count() != 4 || a.LastSyncedAt.Int64 != st.now.Unix() || len(st.items()) != 4 {
			t.Fatalf("caught up: %d %+v", st.ai.count(), a)
		}

		// An overdue window makes confirmation tasks of what it cannot
		// judge, and moves on.
		st.advance(7 * time.Hour)
		for i := 0; i < 4; i++ {
			st.recv("", fmt.Sprint("遅れ ", i), "本文", time.Duration(i+1)*time.Minute)
		}
		st.ai.set(aiAction("a"), aiAction("b"), aiAction("c"))
		if a = st.sync(me); a.LastSyncedAt.Int64 != st.now.Unix() || len(st.items()) != 8 {
			t.Fatalf("overdue: %+v %d", a, len(st.items()))
		}
		fallbacks := 0
		for _, i := range st.items() {
			if strings.HasPrefix(i.Content, "メールを確認: 遅れ") {
				fallbacks++
			}
		}
		if fallbacks != 1 {
			t.Fatalf("fallbacks: %d", fallbacks)
		}
	})
}

// Threads whose listed messages were all handled are not read again.
func TestHandledMessages(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		st := newSyncTest(t, driver)
		judged := st.recv("", "判定済み", "本文", 5*time.Minute)
		replied := st.recv("", "返信済み", "本文", 4*time.Minute)
		st.sent(replied, "返しました", 3*time.Minute)
		future := st.recv("", "未来", "本文", -30*time.Second)
		st.ai.set(aiAction("a"), aiAction("b"), aiAction("c"))
		st.sync(me)
		if st.ai.count() != 2 || st.g.count("threads:"+future) != 1 {
			t.Fatalf("first: %d %v", st.ai.count(), st.g.requests)
		}
		// The judged thread and the one whose newest message is the user's
		// are not read again; the message after the window end is.
		st.g.reset0()
		st.advance(time.Minute)
		st.sync(me)
		if st.g.count("threads:"+judged) != 0 || st.g.count("threads:"+replied) != 0 || st.g.count("threads:"+future) != 1 || st.ai.count() != 3 {
			t.Fatalf("second: %d %v", st.ai.count(), st.g.requests)
		}
		st.mustItem(runKey(me, future, future, future))
		st.g.reset0()
		st.advance(time.Minute)
		st.sync(me)
		if st.g.count("threads") != 0 {
			t.Fatalf("third: %v", st.g.requests)
		}

		// A new message in a handled thread makes it read again.
		late := st.recv(judged, "判定済み", "追加", 10*time.Second)
		st.ai.set(aiAction("d"))
		st.sync(me)
		if st.g.count("threads:"+judged) != 1 {
			t.Fatalf("new message: %v", st.g.requests)
		}
		st.mustItem(runKey(me, judged, late, late))

		// Another generation forgets the table.
		st.connect(me)
		st.g.reset0()
		st.advance(time.Minute)
		st.sync(me)
		if st.g.count("threads:"+judged) != 1 || st.g.count("threads:"+replied) != 1 || len(st.items()) != 4 {
			t.Fatalf("new generation: %v %d", st.g.requests, len(st.items()))
		}

		// Entries are dropped after the internal date plus the window and a
		// day.
		if len(st.syncer.handled[st.accounts[me]].ids) == 0 {
			t.Fatal("empty table")
		}
		st.syncer.prune(st.now.Add(window + 24*time.Hour))
		if _, ok := st.syncer.handled[st.accounts[me]]; ok {
			t.Fatalf("table kept: %v", st.syncer.handled)
		}
	})
}

// A thread postponed is read again; one that cannot be read is skipped and
// counted as lost.
func TestHandledPostponedAndFailed(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		logs := captureLogs(t)
		st := newSyncTest(t, driver)
		var threads []string
		for i := 0; i < 4; i++ {
			threads = append(threads, st.recv("", fmt.Sprint("件 ", i), "本文", time.Duration(i+1)*time.Minute))
		}
		gone := st.recv("", "消えた", "本文", 30*time.Second)
		st.g.set(func(f *fakeGoogle) { f.statuses["threads:"+gone] = 404 })
		st.ai.set(aiAction("a"), aiAction("b"), aiAction("c"), aiAction("d"))
		st.sync(me)
		if !strings.Contains(logs.String(), "thread skipped") {
			t.Fatalf("no warning: %s", logs.String())
		}
		st.g.reset0()
		st.g.set(func(f *fakeGoogle) { f.statuses = map[string]int{} })
		st.advance(time.Minute)
		st.sync(me)
		if st.g.count("threads") != 1 || st.g.count("threads:"+threads[3]) != 1 || st.g.count("threads:"+gone) != 0 {
			t.Fatalf("second: %v", st.g.requests)
		}
		if _, ok := st.item(runKey(me, gone, gone, gone)); ok {
			t.Fatal("skipped thread judged")
		}
	})
}

// The list reads at most 3 pages for a short window (and warns), and 10
// for a long one.
func TestListPages(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		logs := captureLogs(t)
		st := newSyncTest(t, driver)
		st.g.set(func(f *fakeGoogle) { f.pageSize = 1 })
		for i := 0; i < 4; i++ {
			st.recv("", fmt.Sprint("件 ", i), "本文", time.Duration(i+1)*time.Minute)
		}
		st.ai.set(aiNoAction("a"), aiNoAction("b"), aiNoAction("c"))
		a := st.sync(me)
		if st.g.count("list") != 3 || a.LastSyncedAt.Int64 != st.now.Unix() || !strings.Contains(logs.String(), "list truncated") {
			t.Fatalf("short window: %v %+v", st.g.requests, a)
		}

		st = newBareSyncTest(t, driver)
		st.saveAI()
		st.g.set(func(f *fakeGoogle) { f.pageSize = 1 })
		for i := 0; i < 12; i++ {
			st.recv("", fmt.Sprint("件 ", i), "本文", time.Duration(i+1)*time.Hour)
		}
		st.sync(me)
		if st.g.count("list") != 10 {
			t.Fatalf("long window: %v", st.g.requests)
		}
	})
}

// A refresh beyond the token budget carries the rest over.
func TestTokenBudget(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		st := newSyncTest(t, driver)
		start := st.account(me).LastSyncedAt
		a1 := st.recv("", "一", "本文", 2*time.Minute)
		a2 := st.recv("", "二", "本文", 3*time.Minute)
		var once1, once2 sync.Once
		st.g.set(func(f *fakeGoogle) {
			f.unauth = 1
			f.hooks["threads:"+a1] = func() { once1.Do(func() { st.g.set(func(f *fakeGoogle) { f.unauth = 1 }) }) }
			f.hooks["threads:"+a2] = func() { once2.Do(func() { st.g.set(func(f *fakeGoogle) { f.unauth = 1 }) }) }
		})
		st.ai.set(aiAction("一"), aiAction("二"))
		a := st.sync(me)
		if st.g.count("token") != 2 || a.LastSyncedAt != start || a.LastError.Valid || a.Status != store.GmailActive || len(st.items()) != 1 {
			t.Fatalf("budget: %v %+v %d", st.g.requests, a, len(st.items()))
		}
		st.mustItem(runKey(me, a1, a1, a1))
		st.g.set(func(f *fakeGoogle) { f.hooks = map[string]func(){} })
		st.advance(time.Minute)
		if a = st.sync(me); a.LastSyncedAt.Int64 != st.now.Unix() || len(st.items()) != 2 {
			t.Fatalf("next: %+v %d", a, len(st.items()))
		}
		st.mustItem(runKey(me, a2, a2, a2))
	})
}

// The same thread in two accounts makes two items.
func TestAccountsApart(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		st := newSyncTest(t, driver)
		st.connect("second@example.com")
		th := st.recv("", "共通", "本文", time.Minute)
		st.ai.set(aiAction("一"), aiAction("二"))
		st.sync(me)
		st.sync("second@example.com")
		st.mustItem(runKey(me, th, th, th))
		item := st.mustItem(runKey("second@example.com", th, th, th))
		lines, _ := bodyLines(t, st.ai.posts(t, 2)[0])
		if lines[1] != "自分の位置: なし" || refsOf(t, item)[0]["account"] != "second@example.com" {
			t.Fatalf("second account: %q %s", lines, item.Refs)
		}
	})
}
