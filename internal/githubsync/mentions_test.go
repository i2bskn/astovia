package githubsync

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/i2bskn/astovia/internal/ai"
	"github.com/i2bskn/astovia/internal/github"
	"github.com/i2bskn/astovia/internal/store"
)

const aiKey = "sk-ant-test-key-0123456789"

// fakeAI is an Anthropic Messages API answering with replies in order (500
// when they run out). hook runs before the n-th reply (from 1).
type fakeAI struct {
	mu      sync.Mutex
	replies []func(w http.ResponseWriter)
	bodies  []string
	keys    []string
	hook    func(n int)
}

func (f *fakeAI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.bodies = append(f.bodies, string(body))
	f.keys = append(f.keys, r.Header.Get("X-Api-Key"))
	n := len(f.bodies)
	var reply func(http.ResponseWriter)
	if n <= len(f.replies) {
		reply = f.replies[n-1]
	}
	hook := f.hook
	f.mu.Unlock()
	if hook != nil {
		hook(n)
	}
	if reply == nil {
		reply = aiError(500)
	}
	reply(w)
}

func (f *fakeAI) set(fn func(f *fakeAI)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *fakeAI) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.bodies)
}

func (f *fakeAI) body(n int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bodies[n-1]
}

func aiOK(needsAction bool, task, summary string) func(http.ResponseWriter) {
	decision := ai.DecisionNone
	if needsAction {
		decision = ai.DecisionTask
	}
	return aiJudged(decision, task, summary, "", "", "")
}

// aiJudged answers a judgment with a decision and an event (empty strings
// for none).
func aiJudged(decision, task, summary, eventTitle, eventStart, eventEnd string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		out, _ := json.Marshal(map[string]any{"decision": decision, "task": task, "summary": summary, "event_title": eventTitle, "event_start": eventStart, "event_end": eventEnd})
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "msg", "type": "message", "role": "assistant", "model": "claude-haiku-5-5",
			"content": []map[string]any{{"type": "text", "text": string(out)}}, "stop_reason": "end_turn", "usage": map[string]int{"input_tokens": 1, "output_tokens": 1}})
	}
}

func aiError(code int) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"error","message":"forced"}}`))
	}
}

// newMentionTest is a synchronization test after the initial import, with
// the AI configured for alice.
func newMentionTest(t *testing.T, driver string) (*syncTest, *fakeAI) {
	t.Helper()
	st := newSyncTest(t, driver, true)
	st.gh.set(func(f *fakeGitHub) { f.headers["/notifications"] = map[string]string{"Last-Modified": "v1"} })
	st.sync()
	fa := &fakeAI{}
	srv := httptest.NewServer(fa)
	t.Cleanup(srv.Close)
	client := ai.New()
	client.BaseURL = srv.URL
	st.syncer.AI = client
	st.saveAI()
	return st, fa
}

func (st *syncTest) saveAI() store.AISettings {
	st.t.Helper()
	ctx := context.Background()
	var gen int64
	if cur, err := st.s.AISettings(ctx, "alice-id"); err == nil {
		gen = cur.Generation
	}
	sealed, err := st.syncer.Box.Seal(ai.KeyPurpose("alice-id"), aiKey)
	if err != nil {
		st.t.Fatal(err)
	}
	a, err := st.s.SaveAISettings(ctx, store.AISettings{UserID: "alice-id", Provider: ai.ProviderAnthropic, Model: "claude-haiku-5-5", APIKeySealed: sealed, KeyLast4: "6789"}, gen, true, "sid", st.now.Unix())
	if err != nil {
		st.t.Fatal(err)
	}
	return a
}

func (st *syncTest) aiSettings() store.AISettings {
	st.t.Helper()
	a, err := st.s.AISettings(context.Background(), "alice-id")
	if err != nil {
		st.t.Fatal(err)
	}
	return a
}

func (st *syncTest) mentionThread(id, typ, url string, updated time.Time) map[string]any {
	return map[string]any{"id": id, "updated_at": updated, "reason": "mention", "subject": map[string]any{"type": typ, "url": url}}
}

// issueWithComments sets up issue o/r#1 with the given comments, mentioned
// in thread "t1".
func (st *syncTest) issueWithComments(comments ...commentState) {
	st.gh.set(func(f *fakeGitHub) {
		f.issues["o/r/issues/1"] = issueState{State: "open", Title: "Bug", HTMLURL: "https://github.com/o/r/issues/1", Body: "desc", Login: "alice", CreatedAt: st.now.Add(-time.Hour), UpdatedAt: st.now}
		f.comments["o/r/issues/1/comments"] = comments
		f.threads = []map[string]any{st.mentionThread("t1", "Issue", f.base+"/repos/o/r/issues/1", st.now)}
	})
}

func comment(id int64, login, body string, at time.Time) commentState {
	return commentState{ID: id, Login: login, Body: body, HTMLURL: "https://github.com/o/r/issues/1#issuecomment-" + itoa(int(id)), CreatedAt: at, UpdatedAt: at}
}

func mentionKey(n int, post string) string { return "github:mention:o/r#" + itoa(n) + ":" + post }

func TestMentionDetection(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		st, fa := newMentionTest(t, driver)
		before := st.conn()
		recent := st.now.Add(-30 * time.Minute)
		st.issueWithComments(
			comment(10, "erin", "@octocat this is too old", st.now.Add(-25*time.Hour)),
			comment(11, "bob", "@octocat can you check this?", recent),
			comment(12, "octocat", "@octocat note to self", recent.Add(time.Minute)),
			comment(13, "carol", "```\n@octocat\n```\n> @octocat quoted\nno mention here", recent.Add(2*time.Minute)),
			comment(14, "carol", "thanks @OctoCat!", recent.Add(3*time.Minute)),
		)
		// The description mentions too, but it was written before the
		// cutoff; editing it later does not make it new.
		st.gh.set(func(f *fakeGitHub) {
			i := f.issues["o/r/issues/1"]
			i.Body, i.CreatedAt = "@octocat old description", st.now.Add(-30*time.Hour)
			f.issues["o/r/issues/1"] = i
			f.headers["/notifications"] = map[string]string{"Last-Modified": "v2"}
		})
		fa.set(func(f *fakeAI) {
			f.replies = []func(http.ResponseWriter){aiOK(true, "確認して回答する", "bob さんが確認を依頼"), aiOK(false, "", "carol さんがお礼")}
		})
		st.advance(time.Minute)
		c := st.sync()
		if fa.count() != 2 {
			t.Fatalf("AI calls: %d", fa.count())
		}
		if c.LastSyncedAt.Int64 != st.now.Unix() || c.LastModified.String != "v2" || c.LastSyncedAt == before.LastSyncedAt {
			t.Fatalf("connection: %+v", c)
		}
		open := st.mustItem(mentionKey(1, "c11"))
		if open.Status != "open" || open.Content != "確認して回答する" || open.Tracking != 0 || open.Source.String != "github" || open.AutoReason.Valid {
			t.Fatalf("open item: %+v", open)
		}
		var refs []map[string]string
		if err := json.Unmarshal([]byte(open.Refs), &refs); err != nil || len(refs) != 1 || refs[0]["url"] != "https://github.com/o/r/issues/1#issuecomment-11" || refs[0]["conversation_id"] != "o/r#1" || refs[0]["account"] != "octocat" {
			t.Fatalf("refs: %s", open.Refs)
		}
		done := st.mustItem(mentionKey(1, "c14"))
		if done.Status != "done" || done.Content != "carol さんがお礼" || done.AutoReason.String != ReasonMentionNoAction || done.AutoRef.String != "https://github.com/o/r/issues/1#issuecomment-14" || done.AutoAt.Int64 != st.now.Unix() {
			t.Fatalf("done item: %+v", done)
		}
		if st.count() != 2 {
			t.Fatalf("items: %d", st.count())
		}
		p := st.pending()
		if len(p) != 1 || p[0].Title != "メンション: o/r#1" || p[0].Body != "確認して回答する" || p[0].ActionItemID.String != open.ID {
			t.Fatalf("notifications: %+v", p)
		}
		// The request names the target and carries the user's key.
		var msg struct {
			Messages []struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal([]byte(fa.body(1)), &msg); err != nil {
			t.Fatal(err)
		}
		text := msg.Messages[0].Content[0].Text
		if !strings.Contains(text, `"id":"c11"`) || !strings.Contains(text, `"target":true`) || !strings.Contains(text, `"me":"octocat"`) || !strings.Contains(text, `"kind":"issue"`) || fa.keys[0] != aiKey {
			t.Fatalf("request: %s", text)
		}

		// An unchanged thread is not read again.
		reads := st.gh.requested("/repos/o/r/issues/1")
		st.advance(time.Minute)
		st.sync()
		if st.gh.requested("/repos/o/r/issues/1") != reads {
			t.Fatal("unchanged thread read again")
		}
		// An updated thread is read, but existing items are not judged
		// again.
		st.gh.set(func(f *fakeGitHub) {
			f.threads = []map[string]any{st.mentionThread("t1", "Issue", f.base+"/repos/o/r/issues/1", st.now)}
		})
		st.sync()
		if st.gh.requested("/repos/o/r/issues/1") != reads+1 || fa.count() != 2 || st.count() != 2 {
			t.Fatalf("updated thread: %d reads, %d AI calls, %d items", st.gh.requested("/repos/o/r/issues/1")-reads, fa.count(), st.count())
		}
	})
}

// Candidates are not notified; events become event items, already
// dismissed when they have started. The AI reads the times in the user's
// time zone.
func TestMentionCandidatesAndEvents(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		st, fa := newMentionTest(t, driver)
		if err := st.s.SaveNotificationSettings(context.Background(), store.NotificationSettings{UserID: "alice-id", Timezone: "Asia/Tokyo", DigestHour: store.DefaultDigestHour, StaleDays: store.DefaultStaleDays, UpdatedAt: st.now.Unix(), CandidateExpireDays: store.DefaultCandidateExpireDays}); err != nil {
			t.Fatal(err)
		}
		tokyo, err := time.LoadLocation("Asia/Tokyo")
		if err != nil {
			t.Fatal(err)
		}
		recent := st.now.Add(-30 * time.Minute)
		st.issueWithComments(
			comment(21, "bob", "@carol please fix this, cc @octocat", recent),
			comment(22, "bob", "@octocat meeting tomorrow 12:30-13:30", recent.Add(time.Minute)),
			comment(23, "bob", "@octocat meeting just now?", recent.Add(2*time.Minute)),
		)
		start := st.now.Add(24 * time.Hour).Truncate(time.Minute)
		passed := st.now.Add(-time.Hour).Truncate(time.Minute)
		local := func(v time.Time) string { return v.In(tokyo).Format("2006-01-02T15:04") }
		fa.set(func(f *fakeAI) {
			f.replies = []func(http.ResponseWriter){
				aiJudged(ai.DecisionCandidate, "修正を確認する", "s", "", "", ""),
				aiJudged(ai.DecisionTask, "打ち合わせを登録する", "s", "打ち合わせ", local(start), local(start.Add(time.Hour))),
				aiJudged(ai.DecisionCandidate, "打ち合わせを登録する", "s", "打ち合わせ", local(passed), ""),
			}
		})
		st.advance(time.Minute)
		st.sync()
		if fa.count() != 3 {
			t.Fatalf("AI calls: %d", fa.count())
		}
		candidate := st.mustItem(mentionKey(1, "c21"))
		if candidate.Status != "candidate" || candidate.Content != "修正を確認する" || candidate.Tracking != 0 || candidate.Kind != store.KindTask || candidate.EventTitle.Valid {
			t.Fatalf("candidate: %+v", candidate)
		}
		event := st.mustItem(mentionKey(1, "c22"))
		if event.Status != "open" || event.Kind != store.KindEvent || event.EventTitle.String != "打ち合わせ" || event.EventStartAt.Int64 != start.Unix() || event.EventEndAt.Int64 != start.Add(time.Hour).Unix() || event.DueAt.Int64 != start.Unix() || event.Tracking != 0 {
			t.Fatalf("event: %+v", event)
		}
		gone := st.mustItem(mentionKey(1, "c23"))
		if gone.Status != "dismissed" || gone.Kind != store.KindEvent || gone.EventEndAt.Valid || gone.AutoReason.String != store.ReasonEventPassed || gone.AutoRef.String != "https://github.com/o/r/issues/1#issuecomment-23" || gone.AutoAt.Int64 != st.now.Unix() {
			t.Fatalf("passed event: %+v", gone)
		}
		// Only the open task is notified.
		if p := st.pending(); len(p) != 1 || p[0].ActionItemID.String != event.ID {
			t.Fatalf("notifications: %+v", p)
		}
		if body := fa.body(1); !strings.Contains(body, `\"timezone\":\"Asia/Tokyo\"`) || !strings.Contains(body, `+09:00\",\"weekday\":`) {
			t.Fatalf("time zone: %s", body)
		}
	})
}

func TestMentionPullRequest(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		st, fa := newMentionTest(t, driver)
		at := st.now.Add(-10 * time.Minute)
		st.gh.set(func(f *fakeGitHub) {
			f.issues["o/r/issues/2"] = issueState{State: "closed", Title: "Feature", HTMLURL: "https://github.com/o/r/pull/2", IsPull: true, Merged: true, Body: "@octocat review please", Login: "alice", CreatedAt: at.Add(-time.Minute), UpdatedAt: at}
			f.reviews["o/r/pulls/2"] = []reviewState{
				{ID: 21, Login: "dave", Body: "LGTM? @octocat", SubmittedAt: &at, State: "COMMENTED", HTMLURL: "https://github.com/o/r/pull/2#pullrequestreview-21"},
				{ID: 22, Login: "dave", Body: "@octocat pending", State: "PENDING"},
			}
			f.comments["o/r/pulls/2/comments"] = []commentState{{ID: 31, Body: "@octocat nit", HTMLURL: "https://github.com/o/r/pull/2#discussion_r31", CreatedAt: at.Add(time.Minute), UpdatedAt: at.Add(time.Minute)}}
			f.threads = []map[string]any{st.mentionThread("t2", "PullRequest", f.base+"/repos/o/r/pulls/2", st.now)}
		})
		fa.set(func(f *fakeAI) {
			f.replies = []func(http.ResponseWriter){aiOK(true, "レビューする", "s"), aiOK(true, "確認する", "s"), aiOK(false, "", "")}
		})
		st.sync()
		if fa.count() != 3 {
			t.Fatalf("AI calls: %d", fa.count())
		}
		st.mustItem(mentionKey(2, "body"))
		st.mustItem(mentionKey(2, "r21"))
		if i := st.mustItem(mentionKey(2, "rc31")); i.Status != "done" || i.Content != "メンション: Feature" {
			t.Fatalf("empty summary: %+v", i)
		}
		if _, ok := st.item(mentionKey(2, "r22")); ok {
			t.Fatal("pending review detected")
		}
		third := fa.body(3)
		for _, want := range []string{`\"kind\":\"pull_request\"`, `\"state\":\"merged\"`, `\"author\":\"ghost\"`, `\"type\":\"review_comment\"`, `\"id\":\"r21\"`} {
			if !strings.Contains(third, want) {
				t.Fatalf("request lacks %s: %s", want, third)
			}
		}
	})
}

func TestMentionWithoutAI(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		st, fa := newMentionTest(t, driver)
		st.issueWithComments(comment(11, "bob", "@octocat hi", st.now))
		// Not configured: GitHub is not asked about the thread.
		if err := st.s.DeleteAISettings(context.Background(), "alice-id", "sid", st.now.Unix()); err != nil {
			t.Fatal(err)
		}
		c := st.sync()
		if st.gh.requested("/repos/o/r/issues/1") != 0 || fa.count() != 0 || st.count() != 0 || c.LastSyncedAt.Int64 != st.now.Unix() {
			t.Fatalf("unconfigured: %d reads, %d calls, %d items", st.gh.requested("/repos/o/r/issues/1"), fa.count(), st.count())
		}
		// Settings with an error: the same.
		a := st.saveAI()
		if ok, err := st.s.SetAIStatus(context.Background(), "alice-id", a.Generation, store.AIError, sql.NullString{String: AIErrorKeyInvalid, Valid: true}, st.now.Unix()); err != nil || !ok {
			t.Fatal(ok, err)
		}
		st.advance(time.Minute)
		st.sync()
		if st.gh.requested("/repos/o/r/issues/1") != 0 || fa.count() != 0 || st.count() != 0 {
			t.Fatal("mention handled with failing settings")
		}
	})
}

// Errors that stop the AI for the user mark the settings and turn this and
// the remaining mentions into confirmation tasks.
func TestMentionAIErrors(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		for code, want := range map[int]string{401: AIErrorKeyInvalid, 403: AIErrorKeyInvalid, 402: AIErrorBilling, 404: AIErrorModelUnavailable} {
			st, fa := newMentionTest(t, driver)
			st.issueWithComments(comment(11, "bob", "@octocat one", st.now), comment(12, "bob", "@octocat two", st.now.Add(time.Second)))
			fa.set(func(f *fakeAI) { f.replies = []func(http.ResponseWriter){aiError(code)} })
			c := st.sync()
			a := st.aiSettings()
			if fa.count() != 1 || a.Status != store.AIError || a.LastError.String != want {
				t.Fatalf("%d: %d calls, settings %+v", code, fa.count(), a)
			}
			for _, post := range []string{"c11", "c12"} {
				if i := st.mustItem(mentionKey(1, post)); i.Status != "open" || i.Content != "メンションを確認: Bug" {
					t.Fatalf("%d: fallback %+v", code, i)
				}
			}
			if len(st.pending()) != 2 || c.LastSyncedAt.Int64 != st.now.Unix() {
				t.Fatalf("%d: %d notifications, %+v", code, len(st.pending()), c)
			}
		}
	})
}

// A rejected request makes only that mention a confirmation task; the
// warning is cleared by the next successful call.
func TestMentionRequestRejected(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		st, fa := newMentionTest(t, driver)
		st.issueWithComments(comment(11, "bob", "@octocat one", st.now), comment(12, "bob", "@octocat two", st.now.Add(time.Second)))
		var mu sync.Mutex
		var between store.AISettings
		fa.set(func(f *fakeAI) {
			f.replies = []func(http.ResponseWriter){aiError(400), aiOK(true, "やる", "s")}
			f.hook = func(n int) {
				if n == 2 {
					a, err := st.s.AISettings(context.Background(), "alice-id")
					if err != nil {
						t.Error(err)
					}
					mu.Lock()
					between = a
					mu.Unlock()
				}
			}
		})
		st.sync()
		mu.Lock()
		defer mu.Unlock()
		if between.Status != store.AIActive || between.LastError.String != AIErrorRequestRejected {
			t.Fatalf("after 400: %+v", between)
		}
		if a := st.aiSettings(); a.Status != store.AIActive || a.LastError.Valid {
			t.Fatalf("after success: %+v", a)
		}
		if st.mustItem(mentionKey(1, "c11")).Content != "メンションを確認: Bug" || st.mustItem(mentionKey(1, "c12")).Content != "やる" {
			t.Fatal("items")
		}
	})
}

// A temporary failure keeps a recent mention for the next synchronization
// (LastSyncedAt and LastModified stay), while other threads are handled.
func TestMentionTemporaryFailure(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		st, fa := newMentionTest(t, driver)
		before := st.conn()
		st.issueWithComments(comment(11, "bob", "@octocat recent", st.now.Add(-time.Hour)), comment(12, "bob", "@octocat old", st.now.Add(-3*time.Hour)))
		st.gh.set(func(f *fakeGitHub) {
			f.pulls["o/r/pulls/5"] = pullState{State: "open", Title: "PR", RequestedReviewers: []string{"octocat"}}
			f.threads = append(f.threads, st.thread("review_requested", "PullRequest", f.base+"/repos/o/r/pulls/5"))
			f.headers["/notifications"] = map[string]string{"Last-Modified": "v2"}
		})
		fa.set(func(f *fakeAI) { f.replies = []func(http.ResponseWriter){aiError(529), aiError(429)} })
		st.advance(time.Minute)
		c := st.sync()
		if c.LastSyncedAt != before.LastSyncedAt || c.LastModified != before.LastModified || c.LastError.Valid || c.FailureCount != 0 || c.NextPollAt != st.now.Unix()+60 {
			t.Fatalf("connection: %+v (before %+v)", c, before)
		}
		st.mustItem("github:review:o/r#5")
		if _, ok := st.item(mentionKey(1, "c11")); ok {
			t.Fatal("recent mention created after a temporary failure")
		}
		// The mention older than two hours became a confirmation task.
		if i := st.mustItem(mentionKey(1, "c12")); i.Content != "メンションを確認: Bug" {
			t.Fatalf("old mention: %+v", i)
		}
		if a := st.aiSettings(); a.Status != store.AIActive || a.LastError.Valid {
			t.Fatalf("settings changed: %+v", a)
		}
		// The next synchronization reads the same notifications again.
		fa.set(func(f *fakeAI) { f.replies = append(f.replies, aiOK(true, "返信する", "s")) })
		st.advance(time.Minute)
		c = st.sync()
		if i := st.mustItem(mentionKey(1, "c11")); i.Content != "返信する" || c.LastSyncedAt.Int64 != st.now.Unix() || c.LastModified.String != "v2" {
			t.Fatalf("retry: %+v %+v", i, c)
		}
	})
}

func TestMentionCallLimit(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		st, fa := newMentionTest(t, driver)
		before := st.conn()
		var list []commentState
		for n := 1; n <= 4; n++ {
			list = append(list, comment(int64(10+n), "bob", "@octocat "+itoa(n), st.now.Add(time.Duration(n)*time.Second-time.Hour)))
		}
		st.issueWithComments(list...)
		fa.set(func(f *fakeAI) {
			for n := 0; n < 4; n++ {
				f.replies = append(f.replies, aiOK(true, "task "+itoa(n+1), "s"))
			}
		})
		st.advance(time.Minute)
		c := st.sync()
		if fa.count() != mentionAICallsPerRun || st.count() != 3 || c.LastSyncedAt != before.LastSyncedAt {
			t.Fatalf("%d calls, %d items, %+v", fa.count(), st.count(), c)
		}
		if _, ok := st.item(mentionKey(1, "c14")); ok {
			t.Fatal("postponed mention created")
		}
		// Postponed mentions are not fallbacks, and the thread is read
		// again next time.
		st.advance(time.Minute)
		c = st.sync()
		if i := st.mustItem(mentionKey(1, "c14")); i.Content != "task 4" || fa.count() != 4 || c.LastSyncedAt.Int64 != st.now.Unix() {
			t.Fatalf("next: %+v %d %+v", i, fa.count(), c)
		}
	})
}

// replaceAI registers the key again from a fake AI hook, outside the test
// goroutine.
func replaceAI(t *testing.T, st *syncTest, now int64) {
	ctx := context.Background()
	cur, err := st.s.AISettings(ctx, "alice-id")
	if err != nil {
		t.Error(err)
		return
	}
	sealed, err := st.syncer.Box.Seal(ai.KeyPurpose("alice-id"), aiKey)
	if err != nil {
		t.Error(err)
		return
	}
	if _, err = st.s.SaveAISettings(ctx, store.AISettings{UserID: "alice-id", Provider: ai.ProviderAnthropic, Model: "claude-haiku-5-5", APIKeySealed: sealed, KeyLast4: "6789"}, cur.Generation, true, "sid", now); err != nil {
		t.Error(err)
	}
}

// Settings deleted during the synchronization stop it from sending more,
// without a retry; replaced settings postpone the rest to the next one.
func TestMentionSettingsChanged(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		st, fa := newMentionTest(t, driver)
		st.issueWithComments(comment(11, "bob", "@octocat one", st.now), comment(12, "bob", "@octocat two", st.now.Add(time.Second)))
		now := st.now.Unix()
		fa.set(func(f *fakeAI) {
			f.replies = []func(http.ResponseWriter){aiOK(true, "one", "s"), aiOK(true, "two", "s")}
			f.hook = func(n int) {
				if n == 1 {
					if err := st.s.DeleteAISettings(context.Background(), "alice-id", "sid", now); err != nil {
						t.Error(err)
					}
				}
			}
		})
		c := st.sync()
		st.mustItem(mentionKey(1, "c11"))
		if _, ok := st.item(mentionKey(1, "c12")); ok || fa.count() != 1 || c.LastSyncedAt.Int64 != st.now.Unix() {
			t.Fatalf("deleted: %v %d calls %+v", ok, fa.count(), c)
		}

		st, fa = newMentionTest(t, driver)
		before := st.conn()
		st.issueWithComments(comment(11, "bob", "@octocat one", st.now), comment(12, "bob", "@octocat two", st.now.Add(time.Second)))
		now = st.now.Unix()
		fa.set(func(f *fakeAI) {
			f.replies = []func(http.ResponseWriter){aiOK(true, "one", "s"), aiOK(true, "two", "s")}
			f.hook = func(n int) {
				if n == 1 {
					replaceAI(t, st, now)
				}
			}
		})
		st.advance(time.Minute)
		c = st.sync()
		if _, ok := st.item(mentionKey(1, "c12")); ok || fa.count() != 1 || c.LastSyncedAt != before.LastSyncedAt {
			t.Fatalf("replaced: %v %d calls %+v", ok, fa.count(), c)
		}
		st.advance(time.Minute)
		st.sync()
		if i := st.mustItem(mentionKey(1, "c12")); i.Content != "two" || fa.count() != 2 {
			t.Fatalf("after replace: %+v", i)
		}

		// A failure reported for replaced settings does not mark the new
		// ones; the rest of the synchronization still uses fallbacks.
		st, fa = newMentionTest(t, driver)
		st.issueWithComments(comment(11, "bob", "@octocat one", st.now), comment(12, "bob", "@octocat two", st.now.Add(time.Second)))
		now = st.now.Unix()
		fa.set(func(f *fakeAI) {
			f.replies = []func(http.ResponseWriter){aiError(401)}
			f.hook = func(n int) {
				if n == 1 {
					replaceAI(t, st, now)
				}
			}
		})
		st.sync()
		if a := st.aiSettings(); a.Generation != 2 || a.Status != store.AIActive || a.LastError.Valid {
			t.Fatalf("new settings overwritten: %+v", a)
		}
		if st.mustItem(mentionKey(1, "c12")).Content != "メンションを確認: Bug" || fa.count() != 1 {
			t.Fatal("rest after 401")
		}
	})
}

func TestMentionUndecryptableKey(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		st, fa := newMentionTest(t, driver)
		st.issueWithComments(comment(11, "bob", "@octocat one", st.now))
		sealed, err := st.syncer.Box.Seal(ai.KeyPurpose("bob-id"), aiKey)
		if err != nil {
			t.Fatal(err)
		}
		a := st.aiSettings()
		if _, err = st.s.SaveAISettings(context.Background(), store.AISettings{UserID: "alice-id", Provider: "anthropic", Model: a.Model, APIKeySealed: sealed, KeyLast4: "x"}, a.Generation, true, "sid", st.now.Unix()); err != nil {
			t.Fatal(err)
		}
		st.sync()
		if a = st.aiSettings(); a.Status != store.AIError || a.LastError.String != AIErrorKeyUndecryptable || fa.count() != 0 || st.count() != 0 {
			t.Fatalf("undecryptable: %+v %d %d", a, fa.count(), st.count())
		}
	})
}

// After six hours behind, LastSyncedAt moves on and mentions waiting for a
// retry become confirmation tasks.
func TestMentionRetryLimit(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		st, fa := newMentionTest(t, driver)
		c := st.conn()
		err := st.s.GitHubSync(context.Background(), "alice-id", c.Generation, func(tx *store.GitHubTx) error {
			state := tx.Conn
			state.LastSyncedAt = nullInt(st.now.Add(-7 * time.Hour).Unix())
			return tx.SaveState(state)
		})
		if err != nil {
			t.Fatal(err)
		}
		st.issueWithComments(comment(11, "bob", "@octocat recent", st.now))
		fa.set(func(f *fakeAI) { f.replies = []func(http.ResponseWriter){aiError(529)} })
		c = st.sync()
		if i := st.mustItem(mentionKey(1, "c11")); i.Content != "メンションを確認: Bug" || c.LastSyncedAt.Int64 != st.now.Unix() {
			t.Fatalf("overdue: %+v %+v", i, c)
		}
	})
}

// With too little time left before the deadline, the AI is not called and
// the mention waits.
func TestMentionDeadline(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		st, fa := newMentionTest(t, driver)
		before := st.conn()
		st.issueWithComments(comment(11, "bob", "@octocat recent", st.now))
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		st.advance(time.Minute)
		st.syncer.Sync(ctx, st.conn())
		if c := st.conn(); fa.count() != 0 || st.count() != 0 || c.LastSyncedAt != before.LastSyncedAt || c.LastModified != before.LastModified {
			t.Fatalf("deadline: %d calls %d items %+v", fa.count(), st.count(), c)
		}
	})
}

func TestMentionGitHubErrors(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		st, fa := newMentionTest(t, driver)
		before := st.conn()
		st.issueWithComments(comment(11, "bob", "@octocat hi", st.now))
		st.gh.set(func(f *fakeGitHub) { f.forced["/repos/o/r/issues/1/comments"] = 500 })
		st.advance(time.Minute)
		c := st.sync()
		if c.LastSyncedAt != before.LastSyncedAt || c.LastError.Valid || fa.count() != 0 {
			t.Fatalf("500: %+v", c)
		}
		// A missing subject counts as handled.
		st.gh.set(func(f *fakeGitHub) {
			delete(f.forced, "/repos/o/r/issues/1/comments")
			f.forced["/repos/o/r/issues/1"] = 404
		})
		st.advance(time.Minute)
		c = st.sync()
		if c.LastSyncedAt.Int64 != st.now.Unix() || st.count() != 0 {
			t.Fatalf("404: %+v", c)
		}
		reads := st.gh.requested("/repos/o/r/issues/1")
		st.advance(time.Minute)
		st.sync()
		if st.gh.requested("/repos/o/r/issues/1") != reads {
			t.Fatal("missing subject read again")
		}
	})
}

func TestMentions(t *testing.T) {
	for body, want := range map[string]bool{
		"@octocat":                      true,
		"hi @OctoCat, please":           true,
		"(@octocat)":                    true,
		"@octocat.":                     true,
		"@octocat_x":                    true,
		"cc @octocat-bot":               false,
		"@octocats":                     false,
		"mail@octocat":                  false,
		"x_@octocat":                    false,
		"a/@octocat":                    false,
		"-@octocat":                     false,
		"@octocat/team":                 false,
		"`@octocat`":                    false,
		"``a ` @octocat``":              false,
		"`unclosed @octocat":            true,
		"```\n@octocat\n```":            false,
		"~~~ go\n@octocat\n~~~\n":       false,
		"```\n@octocat":                 false,
		"````\n```\n@octocat\n````\nok": false,
		"   ```\n@octocat\n   ```":      false,
		"    ```\n@octocat":             true,
		"> @octocat":                    false,
		"   > @octocat":                 false,
		"text\n> quoted\n@octocat":      true,
		"<!-- @octocat -->":             false,
		"<!-- open @octocat":            false,
		"<!-- x -->@octocat":            true,
		"    @octocat indented code":    true,
		"@Koctocat":                     false,
		"":                              false,
	} {
		if got := mentions(body, "octocat"); got != want {
			t.Errorf("mentions(%q) = %v, want %v", body, got, want)
		}
	}
	if mentions("@x", "") {
		t.Error("empty login matched")
	}
	// Mention keys are not tracked items.
	for _, key := range []string{mentionKey(1, "c11"), "github:mention:o/r#1"} {
		if _, _, ok := parseSourceKey(key); ok {
			t.Errorf("%s parsed", key)
		}
	}
	if k := SourceKey(kindMention, github.Ref{Owner: "Octo", Repo: "Repo", Number: 12}) + ":c345"; k != "github:mention:octo/repo#12:c345" {
		t.Error(k)
	}
}
