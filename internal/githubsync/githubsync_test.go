package githubsync

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/i2bskn/astovia/internal/github"
	"github.com/i2bskn/astovia/internal/secretbox"
	"github.com/i2bskn/astovia/internal/store"
	"github.com/i2bskn/astovia/internal/testutil"
)

type pullState struct {
	State              string   `json:"state"`
	Merged             bool     `json:"merged"`
	Title              string   `json:"title"`
	HTMLURL            string   `json:"html_url"`
	RequestedReviewers []string `json:"-"`
	Assignees          []string `json:"-"`
}

type issueState struct {
	State     string
	Title     string
	HTMLURL   string
	Assignees []string
	IsPull    bool
	Merged    bool
	Body      string
	Login     string
	CreatedAt time.Time
	UpdatedAt time.Time
}

type reviewState struct {
	ID          int64
	Login       string
	Body        string
	SubmittedAt *time.Time
	HTMLURL     string
	State       string
}

// commentState is an issue comment or a review comment. An empty Login is
// a deleted user (null).
type commentState struct {
	ID        int64
	Login     string
	Body      string
	HTMLURL   string
	CreatedAt time.Time
	UpdatedAt time.Time
}

type searchItem struct {
	Repo   string // owner/repo
	Number int
	Title  string
	IsPull bool
}

// fakeGitHub is an in-memory GitHub API. forced maps a path to a status
// code (and optional headers) returned instead of the normal response.
type fakeGitHub struct {
	mu       sync.Mutex
	base     string
	threads  []map[string]any
	pulls    map[string]pullState
	issues   map[string]issueState
	reviews  map[string][]reviewState
	comments map[string][]commentState
	search   map[string][]searchItem
	forced   map[string]int
	headers  map[string]map[string]string
	requests []string
}

func users(logins []string) []map[string]string {
	out := []map[string]string{}
	for _, l := range logins {
		out = append(out, map[string]string{"login": l})
	}
	return out
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.URL.Path)
	if h, ok := f.headers[r.URL.Path]; ok {
		for k, v := range h {
			w.Header().Set(k, v)
		}
	}
	if code, ok := f.forced[r.URL.Path]; ok {
		w.WriteHeader(code)
		_, _ = w.Write([]byte(`{"message":"forced"}`))
		return
	}
	write := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	path := r.URL.Path
	switch {
	case path == "/notifications":
		write(f.threads)
	case path == "/search/issues":
		items := []map[string]any{}
		for _, it := range f.search[r.URL.Query().Get("q")] {
			v := map[string]any{"number": it.Number, "title": it.Title, "html_url": "https://github.com/" + it.Repo + "/x", "repository_url": f.base + "/repos/" + it.Repo}
			if it.IsPull {
				v["pull_request"] = map[string]any{}
			}
			items = append(items, v)
		}
		write(map[string]any{"items": items})
	case strings.HasSuffix(path, "/reviews"):
		key := strings.TrimSuffix(strings.TrimPrefix(path, "/repos/"), "/reviews")
		out := []map[string]any{}
		for _, rv := range f.reviews[key] {
			out = append(out, map[string]any{"id": rv.ID, "user": map[string]string{"login": rv.Login}, "body": rv.Body, "submitted_at": rv.SubmittedAt, "html_url": rv.HTMLURL, "state": rv.State})
		}
		write(out)
	case strings.HasSuffix(path, "/comments"):
		out := []map[string]any{}
		for _, c := range f.comments[strings.TrimPrefix(path, "/repos/")] {
			v := map[string]any{"id": c.ID, "user": nil, "body": c.Body, "html_url": c.HTMLURL, "created_at": c.CreatedAt, "updated_at": c.UpdatedAt}
			if c.Login != "" {
				v["user"] = map[string]string{"login": c.Login}
			}
			out = append(out, v)
		}
		write(out)
	case strings.Contains(path, "/pulls/"):
		p, ok := f.pulls[strings.TrimPrefix(path, "/repos/")]
		if !ok {
			w.WriteHeader(404)
			return
		}
		write(map[string]any{"state": p.State, "merged": p.Merged, "title": p.Title, "html_url": p.HTMLURL, "requested_reviewers": users(p.RequestedReviewers), "assignees": users(p.Assignees)})
	case strings.Contains(path, "/issues/"):
		i, ok := f.issues[strings.TrimPrefix(path, "/repos/")]
		if !ok {
			w.WriteHeader(404)
			return
		}
		v := map[string]any{"state": i.State, "title": i.Title, "html_url": i.HTMLURL, "assignees": users(i.Assignees), "body": i.Body, "user": map[string]string{"login": i.Login}, "created_at": i.CreatedAt, "updated_at": i.UpdatedAt}
		if i.IsPull {
			pr := map[string]any{"url": "x", "merged_at": nil}
			if i.Merged {
				pr["merged_at"] = i.UpdatedAt
			}
			v["pull_request"] = pr
		}
		write(v)
	default:
		w.WriteHeader(404)
	}
}

func (f *fakeGitHub) set(fn func(f *fakeGitHub)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *fakeGitHub) requested(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, p := range f.requests {
		if p == path {
			n++
		}
	}
	return n
}

type syncTest struct {
	t      *testing.T
	s      *store.Store
	gh     *fakeGitHub
	syncer *Syncer
	now    time.Time
}

func newSyncTest(t *testing.T, driver string, immediate bool) *syncTest {
	t.Helper()
	ctx := context.Background()
	s, _ := testutil.Database(t, driver)
	st := &syncTest{t: t, s: s, now: time.Date(2026, 10, 24, 1, 0, 0, 0, time.UTC)}
	u := store.User{ID: "alice-id", Email: "alice@example.com", PasswordHash: "x"}
	if err := s.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	if err := s.Login(ctx, u, store.Session{ID: "sid", UserID: u.ID, ExpiresAt: st.now.Unix() + 86400*30, CreatedAt: st.now.Unix()}, store.Refresh{Hash: "r", SessionID: "sid", CreatedAt: st.now.Unix()}); err != nil {
		t.Fatal(err)
	}
	st.gh = &fakeGitHub{pulls: map[string]pullState{}, issues: map[string]issueState{}, reviews: map[string][]reviewState{}, comments: map[string][]commentState{}, search: map[string][]searchItem{}, forced: map[string]int{}, headers: map[string]map[string]string{}, threads: []map[string]any{}}
	srv := httptest.NewServer(st.gh)
	t.Cleanup(srv.Close)
	st.gh.base = srv.URL
	client := github.New(func() time.Time { return st.now })
	client.BaseURL = srv.URL
	box := secretbox.New([]byte(strings.Repeat("s", 32)))
	st.syncer = &Syncer{Store: s, GitHub: client, Box: box, Now: func() time.Time { return st.now }}
	st.connect("octocat", immediate)
	return st
}

func (st *syncTest) connect(login string, immediate bool) store.GitHubConnection {
	st.t.Helper()
	sealed, err := st.syncer.Box.Seal(TokenPurpose("alice-id"), "ghp_test")
	if err != nil {
		st.t.Fatal(err)
	}
	c, err := st.s.SaveGitHubConnection(context.Background(), store.GitHubConnection{UserID: "alice-id", TokenSealed: sealed, TokenLast4: "test", Login: login, Scopes: "repo"}, &immediate, "sid", st.now.Unix())
	if err != nil {
		st.t.Fatal(err)
	}
	return c
}

func (st *syncTest) conn() store.GitHubConnection {
	st.t.Helper()
	c, err := st.s.GitHubConnection(context.Background(), "alice-id")
	if err != nil {
		st.t.Fatal(err)
	}
	return c
}

// sync runs one synchronization of the current connection.
func (st *syncTest) sync() store.GitHubConnection {
	st.t.Helper()
	st.syncer.Sync(context.Background(), st.conn())
	return st.conn()
}

func (st *syncTest) advance(d time.Duration) { st.now = st.now.Add(d) }

func (st *syncTest) item(key string) (store.ActionItem, bool) {
	st.t.Helper()
	items, err := st.s.ListActionItems(context.Background(), "alice-id", "", nil, 100)
	if err != nil {
		st.t.Fatal(err)
	}
	for _, i := range items {
		if i.SourceKey.String == key {
			return i, true
		}
	}
	return store.ActionItem{}, false
}

func (st *syncTest) mustItem(key string) store.ActionItem {
	st.t.Helper()
	i, ok := st.item(key)
	if !ok {
		st.t.Fatalf("no item %s", key)
	}
	return i
}

func (st *syncTest) count() int {
	st.t.Helper()
	items, err := st.s.ListActionItems(context.Background(), "alice-id", "", nil, 100)
	if err != nil {
		st.t.Fatal(err)
	}
	return len(items)
}

// pending returns the unsent notifications of alice.
func (st *syncTest) pending() []store.Notification {
	st.t.Helper()
	list, err := st.s.DueNotifications(context.Background(), st.now.Unix()+86400*365, 100)
	if err != nil {
		st.t.Fatal(err)
	}
	return list
}

func (st *syncTest) thread(reason, typ, url string) map[string]any {
	return map[string]any{"reason": reason, "subject": map[string]any{"type": typ, "url": url}}
}

func drivers(t *testing.T, fn func(t *testing.T, driver string)) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) { fn(t, driver) })
	}
}

func TestInitialImport(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		st := newSyncTest(t, driver, true)
		st.gh.set(func(f *fakeGitHub) {
			f.search[searches[0].query] = []searchItem{{Repo: "Octo/Repo", Number: 1, Title: " Fix\tbug\n", IsPull: true}, {Repo: "o/r", Number: 9, Title: "not a PR"}}
			f.search[searches[1].query] = []searchItem{{Repo: "o/r", Number: 2, Title: ""}}
			f.search[searches[2].query] = []searchItem{{Repo: "o/r", Number: 3, Title: "PR", IsPull: true}}
			// Tracking runs in the same synchronization; GitHub compares
			// owner and repository names without case.
			f.pulls["octo/repo/pulls/1"] = pullState{State: "open", RequestedReviewers: []string{"octocat"}}
			f.issues["o/r/issues/2"] = issueState{State: "open", Assignees: []string{"octocat"}}
			f.issues["o/r/issues/3"] = issueState{State: "open", Assignees: []string{"octocat"}, IsPull: true}
			f.issues["o/r/issues/4"] = issueState{State: "open", Assignees: []string{"octocat"}}
		})
		c := st.sync()
		if c.InitialImportPending != 0 || c.LastError.Valid || c.Status != store.GitHubActive || c.NextPollAt != st.now.Unix()+60 || !c.LastSyncedAt.Valid || !c.LastReconciledAt.Valid {
			t.Fatalf("connection: %+v", c)
		}
		review := st.mustItem("github:review:octo/repo#1")
		if review.Content != "レビュー: Fixbug" || review.Status != "open" || review.Tracking != 1 || review.Source.String != "github" || review.DetectedAt.Int64 != st.now.Unix() || review.Kind != store.KindTask {
			t.Fatalf("review item: %+v", review)
		}
		var refs []map[string]string
		if err := json.Unmarshal([]byte(review.Refs), &refs); err != nil || len(refs) != 1 || refs[0]["service"] != "github" || refs[0]["account"] != "octocat" || refs[0]["conversation_id"] != "Octo/Repo#1" || refs[0]["url"] != "https://github.com/Octo/Repo/x" {
			t.Fatalf("refs: %s", review.Refs)
		}
		if i := st.mustItem("github:assign:o/r#2"); i.Content != "対応: o/r#2" {
			t.Fatalf("empty title: %+v", i)
		}
		st.mustItem("github:assign:o/r#3")
		if _, ok := st.item("github:review:o/r#9"); ok || st.count() != 3 {
			t.Fatalf("items: %d", st.count())
		}
		// The initial import does not notify.
		if p := st.pending(); len(p) != 0 {
			t.Fatalf("notifications: %+v", p)
		}
		// Reconciliation 15 minutes later notifies what it newly finds.
		st.gh.set(func(f *fakeGitHub) {
			f.search[searches[1].query] = append(f.search[searches[1].query], searchItem{Repo: "o/r", Number: 4, Title: "new"})
		})
		st.advance(16 * time.Minute)
		st.sync()
		st.mustItem("github:assign:o/r#4")
		if p := st.pending(); len(p) != 1 || p[0].Title != "割り当て: o/r#4" || p[0].Body != "new" || p[0].Kind != "detected" {
			t.Fatalf("reconcile notifications: %+v", p)
		}
	})
}

func TestNotificationDetection(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		st := newSyncTest(t, driver, true)
		st.sync() // initial import with no results
		b := st.gh.base
		st.gh.set(func(f *fakeGitHub) {
			f.pulls["o/r/pulls/1"] = pullState{State: "open", Title: "Add feature", HTMLURL: "https://github.com/o/r/pull/1", RequestedReviewers: []string{"OctoCat"}}
			f.pulls["o/r/pulls/2"] = pullState{State: "open", Title: "Someone else", RequestedReviewers: []string{"other"}}
			f.pulls["o/r/pulls/3"] = pullState{State: "closed", Title: "Closed", RequestedReviewers: []string{"octocat"}}
			f.issues["o/r/issues/4"] = issueState{State: "open", Title: "Bug", HTMLURL: "https://github.com/o/r/issues/4", Assignees: []string{"octocat"}}
			f.issues["o/r/issues/5"] = issueState{State: "open", Title: "PR assigned", HTMLURL: "https://github.com/o/r/pull/5", Assignees: []string{"octocat"}, IsPull: true}
			f.pulls["o/r/pulls/6"] = pullState{State: "open", Title: "Evil", RequestedReviewers: []string{"octocat"}}
			f.threads = []map[string]any{
				st.thread("review_requested", "PullRequest", b+"/repos/o/r/pulls/1"),
				st.thread("review_requested", "PullRequest", b+"/repos/o/r/pulls/1"),
				st.thread("review_requested", "PullRequest", b+"/repos/o/r/pulls/2"),
				st.thread("review_requested", "PullRequest", b+"/repos/o/r/pulls/3"),
				st.thread("assign", "Issue", b+"/repos/o/r/issues/4"),
				st.thread("assign", "PullRequest", b+"/repos/o/r/pulls/5"),
				st.thread("mention", "PullRequest", b+"/repos/o/r/pulls/1"),
				st.thread("review_requested", "PullRequest", "https://evil.example/repos/o/r/pulls/6"),
				st.thread("review_requested", "PullRequest", b+"/repos/o/r/pulls/6?x=1"),
				{"reason": "review_requested", "subject": map[string]any{"type": "PullRequest", "url": nil}},
			}
		})
		st.advance(time.Minute)
		st.sync()
		st.sync() // the same threads again create nothing new
		st.mustItem("github:review:o/r#1")
		st.mustItem("github:assign:o/r#4")
		st.mustItem("github:assign:o/r#5")
		if st.count() != 3 {
			t.Fatalf("items: %d", st.count())
		}
		// The subject URL outside the base URL was never followed.
		if n := st.gh.requested("/repos/o/r/pulls/6"); n != 0 {
			t.Fatalf("pulls/6 requested %d times", n)
		}
		titles := map[string]bool{}
		for _, n := range st.pending() {
			titles[n.Title+"|"+n.Body] = true
			if n.Kind != "detected" || n.URL != "/tasks/"+n.ActionItemID.String || !n.ActionItemID.Valid || !strings.HasPrefix(n.DedupeKey, "detected:"+n.ActionItemID.String+":") {
				t.Fatalf("notification: %+v", n)
			}
		}
		if len(titles) != 3 || !titles["レビュー依頼: o/r#1|Add feature"] || !titles["割り当て: o/r#4|Bug"] || !titles["割り当て: o/r#5|PR assigned"] {
			t.Fatalf("notifications: %v", titles)
		}
	})
}

func TestNoImmediateNotify(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		st := newSyncTest(t, driver, false)
		st.gh.set(func(f *fakeGitHub) {
			f.pulls["o/r/pulls/1"] = pullState{State: "open", Title: "x", RequestedReviewers: []string{"octocat"}}
			f.threads = []map[string]any{st.thread("review_requested", "PullRequest", f.base+"/repos/o/r/pulls/1")}
		})
		st.sync()
		st.mustItem("github:review:o/r#1")
		if p := st.pending(); len(p) != 0 {
			t.Fatalf("notifications: %+v", p)
		}
	})
}

func TestTracking(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		st := newSyncTest(t, driver, true)
		reviewKeys := map[int]string{}
		st.gh.set(func(f *fakeGitHub) {
			for n := 1; n <= 5; n++ {
				f.pulls["o/r/pulls/"+itoa(n)] = pullState{State: "open", Title: "PR", HTMLURL: "https://github.com/o/r/pull/" + itoa(n), RequestedReviewers: []string{"octocat"}}
				f.threads = append(f.threads, st.thread("review_requested", "PullRequest", f.base+"/repos/o/r/pulls/"+itoa(n)))
				reviewKeys[n] = "github:review:o/r#" + itoa(n)
			}
			f.issues["o/r/issues/10"] = issueState{State: "open", Title: "Issue", HTMLURL: "https://github.com/o/r/issues/10", Assignees: []string{"octocat"}}
			f.issues["o/r/issues/11"] = issueState{State: "open", Title: "PR", HTMLURL: "https://github.com/o/r/pull/11", Assignees: []string{"octocat"}, IsPull: true}
			f.issues["o/r/issues/12"] = issueState{State: "open", Title: "PR", HTMLURL: "https://github.com/o/r/pull/12", Assignees: []string{"octocat"}, IsPull: true}
			f.issues["o/r/issues/13"] = issueState{State: "open", Title: "Issue", HTMLURL: "https://github.com/o/r/issues/13", Assignees: []string{"octocat"}}
			f.issues["o/r/issues/14"] = issueState{State: "open", Title: "Issue", Assignees: []string{"octocat"}}
			f.issues["o/r/issues/15"] = issueState{State: "open", Title: "Issue", Assignees: []string{"octocat"}}
			for _, n := range []int{10, 13, 14, 15} {
				f.threads = append(f.threads, st.thread("assign", "Issue", f.base+"/repos/o/r/issues/"+itoa(n)))
			}
			for _, n := range []int{11, 12} {
				f.threads = append(f.threads, st.thread("assign", "PullRequest", f.base+"/repos/o/r/pulls/"+itoa(n)))
			}
		})
		st.sync()
		if st.count() != 11 || len(st.pending()) != 11 {
			t.Fatalf("detected %d items, %d notifications", st.count(), len(st.pending()))
		}
		detectedAt := st.now
		submitted := detectedAt.Add(-time.Minute)
		tooOld := detectedAt.Add(-10 * time.Minute)
		st.gh.set(func(f *fakeGitHub) {
			f.threads = []map[string]any{}
			f.pulls["o/r/pulls/1"] = pullState{State: "closed", Merged: true, HTMLURL: "https://github.com/o/r/pull/1"}
			f.pulls["o/r/pulls/2"] = pullState{State: "closed", HTMLURL: "https://github.com/o/r/pull/2"}
			f.pulls["o/r/pulls/3"] = pullState{State: "open", HTMLURL: "https://github.com/o/r/pull/3"}
			f.reviews["o/r/pulls/3"] = []reviewState{{Login: "octocat", SubmittedAt: &tooOld, HTMLURL: "old", State: "APPROVED"}, {Login: "OCTOCAT", SubmittedAt: &submitted, HTMLURL: "https://github.com/o/r/pull/3#review-1", State: "COMMENTED"}, {Login: "octocat", HTMLURL: "pending", State: "PENDING"}}
			f.pulls["o/r/pulls/4"] = pullState{State: "open", HTMLURL: "https://github.com/o/r/pull/4"}
			f.reviews["o/r/pulls/4"] = []reviewState{{Login: "octocat", SubmittedAt: &tooOld, HTMLURL: "old", State: "APPROVED"}, {Login: "someone", SubmittedAt: &submitted, HTMLURL: "x", State: "APPROVED"}}
			// pulls/5 is unchanged: still requested.
			f.issues["o/r/issues/10"] = issueState{State: "closed", HTMLURL: "https://github.com/o/r/issues/10", Assignees: []string{"octocat"}}
			f.issues["o/r/issues/11"] = issueState{State: "closed", IsPull: true, Assignees: []string{"octocat"}}
			f.pulls["o/r/pulls/11"] = pullState{State: "closed", Merged: true, HTMLURL: "https://github.com/o/r/pull/11"}
			f.issues["o/r/issues/12"] = issueState{State: "closed", IsPull: true, Assignees: []string{"octocat"}}
			f.pulls["o/r/pulls/12"] = pullState{State: "closed", HTMLURL: "https://github.com/o/r/pull/12"}
			f.issues["o/r/issues/13"] = issueState{State: "open", HTMLURL: "https://github.com/o/r/issues/13", Assignees: []string{"someone"}}
			delete(f.issues, "o/r/issues/14")
			f.forced["/repos/o/r/issues/15"] = 403
		})
		st.advance(6 * time.Minute)
		c := st.sync()
		if c.LastError.Valid || c.LastTrackedAt.Int64 != st.now.Unix() {
			t.Fatalf("connection: %+v", c)
		}
		for key, want := range map[string][2]string{
			reviewKeys[1]:          {ReasonPRMerged, "https://github.com/o/r/pull/1"},
			reviewKeys[2]:          {ReasonPRClosed, "https://github.com/o/r/pull/2"},
			reviewKeys[3]:          {ReasonReviewSubmitted, "https://github.com/o/r/pull/3#review-1"},
			reviewKeys[4]:          {ReasonReviewRequestRemoved, "https://github.com/o/r/pull/4"},
			"github:assign:o/r#10": {ReasonIssueClosed, "https://github.com/o/r/issues/10"},
			"github:assign:o/r#11": {ReasonPRMerged, "https://github.com/o/r/pull/11"},
			"github:assign:o/r#12": {ReasonPRClosed, "https://github.com/o/r/pull/12"},
			"github:assign:o/r#13": {ReasonUnassigned, "https://github.com/o/r/issues/13"},
		} {
			i := st.mustItem(key)
			if i.Status != "done" || i.AutoReason.String != want[0] || i.AutoRef.String != want[1] || i.AutoAt.Int64 != st.now.Unix() || i.Tracking != 1 || i.Version != 2 || i.TrackedAt.Int64 != st.now.Unix() {
				t.Fatalf("%s: %+v", key, i)
			}
		}
		if i := st.mustItem(reviewKeys[5]); i.Status != "open" || i.AutoReason.Valid || i.Version != 1 || i.TrackedAt.Int64 != st.now.Unix() {
			t.Fatalf("unchanged item: %+v", i)
		}
		if i := st.mustItem("github:assign:o/r#14"); i.Status != "open" || i.Tracking != 0 || i.AutoReason.Valid {
			t.Fatalf("404 item: %+v", i)
		}
		if i := st.mustItem("github:assign:o/r#15"); i.Status != "open" || i.Tracking != 1 || i.Version != 1 || i.TrackedAt.Int64 != st.now.Unix() {
			t.Fatalf("403 item: %+v", i)
		}
		// Completed items have no pending notifications left.
		pending := map[string]bool{}
		for _, n := range st.pending() {
			pending[n.ActionItemID.String] = true
		}
		if len(pending) != 3 || !pending[st.mustItem(reviewKeys[5]).ID] || !pending[st.mustItem("github:assign:o/r#14").ID] || !pending[st.mustItem("github:assign:o/r#15").ID] {
			t.Fatalf("pending notifications: %v", pending)
		}
		// Tracking runs only every five minutes.
		before := st.gh.requested("/repos/o/r/pulls/5")
		st.advance(time.Minute)
		st.sync()
		if st.gh.requested("/repos/o/r/pulls/5") != before {
			t.Fatal("tracked again within five minutes")
		}
	})
}

func TestReopen(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		st := newSyncTest(t, driver, true)
		st.gh.set(func(f *fakeGitHub) {
			for _, n := range []string{"1", "2"} {
				f.pulls["o/r/pulls/"+n] = pullState{State: "open", Title: "PR " + n, RequestedReviewers: []string{"octocat"}}
				f.threads = append(f.threads, st.thread("review_requested", "PullRequest", f.base+"/repos/o/r/pulls/"+n))
			}
		})
		st.sync()
		// PR 1 is reviewed (completed automatically); PR 2 is completed by
		// the user.
		manual := st.mustItem("github:review:o/r#2")
		if _, err := st.s.UpdateActionItem(context.Background(), "alice-id", manual.ID, func(i *store.ActionItem) error {
			i.Status, i.Tracking = "done", 0
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		st.gh.set(func(f *fakeGitHub) {
			f.threads = []map[string]any{}
			f.pulls["o/r/pulls/1"] = pullState{State: "open", Title: "PR 1"}
		})
		st.advance(6 * time.Minute)
		st.sync()
		if i := st.mustItem("github:review:o/r#1"); i.Status != "done" || i.AutoReason.String != ReasonReviewRequestRemoved {
			t.Fatalf("not completed: %+v", i)
		}
		// Requested again: the automatic completion is undone and notified.
		st.gh.set(func(f *fakeGitHub) {
			for _, n := range []string{"1", "2"} {
				f.pulls["o/r/pulls/"+n] = pullState{State: "open", Title: "PR " + n, RequestedReviewers: []string{"octocat"}}
				f.threads = append(f.threads, st.thread("review_requested", "PullRequest", f.base+"/repos/o/r/pulls/"+n))
			}
		})
		st.advance(time.Minute)
		st.sync()
		reopened := st.mustItem("github:review:o/r#1")
		if reopened.Status != "open" || reopened.AutoReason.Valid || reopened.AutoAt.Valid || reopened.AutoRef.Valid || reopened.Tracking != 1 || reopened.Version != 3 || reopened.DetectedAt.Int64 != st.now.Unix() {
			t.Fatalf("reopened: %+v", reopened)
		}
		var again []store.Notification
		for _, n := range st.pending() {
			if n.ActionItemID.String == reopened.ID {
				again = append(again, n)
			}
		}
		if len(again) != 1 || again[0].Title != "再レビュー依頼: o/r#1" || again[0].DedupeKey != "detected:"+reopened.ID+":3" {
			t.Fatalf("reopen notification: %+v", again)
		}
		if i := st.mustItem("github:review:o/r#2"); i.Status != "done" || i.Version != 2 {
			t.Fatalf("manually completed item changed: %+v", i)
		}
		if st.count() != 2 {
			t.Fatalf("items: %d", st.count())
		}
	})
}

func TestConnectionErrors(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		st := newSyncTest(t, driver, true)
		st.sync()

		// Server errors back off exponentially up to 15 minutes.
		st.gh.set(func(f *fakeGitHub) { f.forced["/notifications"] = 502 })
		for i, wait := range []int64{60, 120, 240, 480, 900, 900} {
			st.advance(time.Minute)
			c := st.sync()
			if c.LastError.String != ErrorUnavailable || c.FailureCount != int64(i+1) || c.NextPollAt != st.now.Unix()+wait || c.Status != store.GitHubActive {
				t.Fatalf("failure %d: %+v", i+1, c)
			}
		}
		st.gh.set(func(f *fakeGitHub) { delete(f.forced, "/notifications") })
		if c := st.sync(); c.LastError.Valid || c.FailureCount != 0 {
			t.Fatalf("recovered: %+v", c)
		}

		// A rate limit waits until the reset time and keeps the connection active.
		reset := st.now.Add(30 * time.Minute).Unix()
		st.gh.set(func(f *fakeGitHub) {
			f.forced["/notifications"] = 403
			f.headers["/notifications"] = map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": itoa(int(reset))}
		})
		if c := st.sync(); c.LastError.String != ErrorRateLimited || c.NextPollAt != reset || c.Status != store.GitHubActive {
			t.Fatalf("rate limited: %+v", c)
		}

		// 401 stops polling.
		st.gh.set(func(f *fakeGitHub) {
			f.forced["/notifications"] = 401
			delete(f.headers, "/notifications")
		})
		c := st.sync()
		if c.Status != store.GitHubError || c.LastError.String != ErrorTokenInvalid {
			t.Fatalf("unauthorized: %+v", c)
		}
		if due, err := st.s.DueGitHubConnections(context.Background(), st.now.Unix()+86400, 10); err != nil || len(due) != 0 {
			t.Fatalf("stopped connection is due: %+v %v", due, err)
		}
	})
}

func TestSearchRateLimitKeepsImportPending(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		st := newSyncTest(t, driver, true)
		st.gh.set(func(f *fakeGitHub) {
			f.forced["/search/issues"] = 429
			f.pulls["o/r/pulls/1"] = pullState{State: "open", RequestedReviewers: []string{"octocat"}}
			f.threads = []map[string]any{st.thread("review_requested", "PullRequest", f.base+"/repos/o/r/pulls/1")}
		})
		c := st.sync()
		// Polling continued; only the import waits for the next poll.
		if c.InitialImportPending != 1 || c.LastError.Valid || !c.LastSyncedAt.Valid {
			t.Fatalf("connection: %+v", c)
		}
		st.mustItem("github:review:o/r#1")
		st.gh.set(func(f *fakeGitHub) { delete(f.forced, "/search/issues") })
		if c = st.sync(); c.InitialImportPending != 0 {
			t.Fatalf("import not retried: %+v", c)
		}
	})
}

func TestStaleAndUndecryptable(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		st := newSyncTest(t, driver, true)
		old := st.conn()
		st.gh.set(func(f *fakeGitHub) {
			f.pulls["o/r/pulls/1"] = pullState{State: "open", RequestedReviewers: []string{"octocat"}}
			f.threads = []map[string]any{st.thread("review_requested", "PullRequest", f.base+"/repos/o/r/pulls/1")}
		})
		// A new token was registered after the synchronization read the
		// connection: its results are discarded.
		st.connect("octocat", true)
		st.syncer.Sync(context.Background(), old)
		if st.count() != 0 {
			t.Fatal("stale synchronization wrote items")
		}
		if c := st.conn(); c.Generation != 2 || c.InitialImportPending != 1 || c.LastSyncedAt.Valid {
			t.Fatalf("stale synchronization wrote state: %+v", c)
		}

		// A token sealed with another secret stops the connection.
		before := st.gh.requested("/notifications")
		st.syncer.Box = secretbox.New([]byte(strings.Repeat("o", 32)))
		c := st.sync()
		if c.Status != store.GitHubError || c.LastError.String != ErrorTokenUndecryptable {
			t.Fatalf("undecryptable: %+v", c)
		}
		if n := st.gh.requested("/notifications") - before; n != 0 {
			t.Fatalf("requests with an undecryptable token: %d", n)
		}
	})
}

func TestSourceKey(t *testing.T) {
	key := SourceKey(kindReview, github.Ref{Owner: "Octo", Repo: "Repo.js", Number: 12})
	if key != "github:review:octo/repo.js#12" {
		t.Fatal(key)
	}
	kind, ref, ok := parseSourceKey(key)
	if !ok || kind != kindReview || ref != (github.Ref{Owner: "octo", Repo: "repo.js", Number: 12}) {
		t.Fatalf("%v %+v", ok, ref)
	}
	for _, bad := range []string{"", "github:x:o/r#1", "github:review:o#1", "github:review:o/r#0", "github:review:o/r", "slack:review:o/r#1", "github:review:../r#1"} {
		if _, _, ok := parseSourceKey(bad); ok {
			t.Fatalf("%q accepted", bad)
		}
	}
	if got := backoff(1); got != 60 {
		t.Fatal(got)
	}
	if got := backoff(100); got != 900 {
		t.Fatal(got)
	}
	if v := githubURL("https://evil.example/x"); v.Valid {
		t.Fatal("non-GitHub URL kept")
	}
	if v := githubURL("https://github.com/o/r/pull/1"); !v.Valid {
		t.Fatal("GitHub URL dropped")
	}
}

func itoa(n int) string { return strconv.Itoa(n) }
