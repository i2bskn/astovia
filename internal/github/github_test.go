package github

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

var testNow = time.Date(2026, 10, 24, 10, 0, 0, 0, time.UTC)

type fakeGitHub struct {
	t        *testing.T
	mu       sync.Mutex
	requests []*http.Request
	handler  http.HandlerFunc
}

func newFake(t *testing.T, h http.HandlerFunc) (*fakeGitHub, *Client) {
	f := &fakeGitHub{t: t, handler: h}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests = append(f.requests, r)
		f.mu.Unlock()
		f.handler(w, r)
	}))
	t.Cleanup(srv.Close)
	c := New(func() time.Time { return testNow })
	c.BaseURL = srv.URL
	return f, c
}

func (f *fakeGitHub) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func TestUserAndHeaders(t *testing.T) {
	f, c := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-OAuth-Scopes", " repo, notifications,repo , ,gist")
		_, _ = w.Write([]byte(`{"login":"octocat"}`))
	})
	login, scopes, err := c.User(context.Background(), "ghp_token")
	if err != nil || login != "octocat" || !reflect.DeepEqual(scopes, []string{"gist", "notifications", "repo"}) {
		t.Fatalf("User: %q %v %v", login, scopes, err)
	}
	r := f.requests[0]
	if r.URL.Path != "/user" || r.Header.Get("Authorization") != "Bearer ghp_token" || r.Header.Get("Accept") != "application/vnd.github+json" || r.Header.Get("X-GitHub-Api-Version") != "2022-11-28" || r.Header.Get("User-Agent") != "astovia" {
		t.Fatalf("request: %s %v", r.URL, r.Header)
	}
	// Fine-grained tokens send no X-OAuth-Scopes.
	_, c = newFake(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"login":"octocat"}`)) })
	if _, scopes, err = c.User(context.Background(), "t"); err != nil || len(scopes) != 0 {
		t.Fatalf("no scopes header: %v %v", scopes, err)
	}
}

func TestErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		header map[string]string
		body   string
		check  func(error) bool
	}{
		{"401", 401, nil, "", func(e error) bool { return errors.Is(e, ErrUnauthorized) }},
		{"404", 404, nil, "", func(e error) bool { return errors.Is(e, ErrNotFound) }},
		{"422", 422, nil, "", func(e error) bool { return errors.Is(e, ErrUnprocessable) }},
		{"500", 500, nil, "", func(e error) bool { var s *StatusError; return errors.As(e, &s) && s.Code == 500 }},
		{"redirect is not followed", 302, map[string]string{"Location": "https://evil.example/"}, "", func(e error) bool { var s *StatusError; return errors.As(e, &s) && s.Code == 302 }},
		{"plain 403", 403, nil, `{"message":"Resource protected by organization SAML enforcement"}`, func(e error) bool { return errors.Is(e, ErrForbidden) && !errors.Is(e, ErrRateLimited) }},
		{"Retry-After", 403, map[string]string{"Retry-After": "120", "X-RateLimit-Remaining": "0", "X-RateLimit-Reset": "1"}, "", rateAt(testNow.Add(120 * time.Second))},
		{"primary limit", 403, map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": "1793000000"}, "", rateAt(time.Unix(1793000000, 0))},
		{"remaining not 0", 403, map[string]string{"X-RateLimit-Remaining": "10", "X-RateLimit-Reset": "1793000000"}, "", func(e error) bool { return errors.Is(e, ErrForbidden) }},
		{"secondary limit", 403, nil, `{"message":"You have exceeded a Secondary Rate Limit."}`, rateAt(testNow.Add(60 * time.Second))},
		{"429 without hints", 429, nil, "", rateAt(time.Time{})},
		{"429 Retry-After", 429, map[string]string{"Retry-After": "5"}, "", rateAt(testNow.Add(5 * time.Second))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, c := newFake(t, func(w http.ResponseWriter, r *http.Request) {
				for k, v := range tc.header {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			_, err := c.Pull(context.Background(), "t", "o", "r", 1)
			if err == nil || !tc.check(err) {
				t.Fatalf("got %v", err)
			}
		})
	}
	// Transport failures and invalid JSON are general errors.
	_, c := newFake(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{`)) })
	if _, err := c.Issue(context.Background(), "t", "o", "r", 1); err == nil || errors.Is(err, ErrRateLimited) {
		t.Fatalf("invalid JSON: %v", err)
	}
	c.BaseURL = "http://127.0.0.1:1"
	if _, err := c.Issue(context.Background(), "t", "o", "r", 1); err == nil {
		t.Fatal("connection refused")
	}
}

func rateAt(at time.Time) func(error) bool {
	return func(e error) bool {
		var r *RateLimitError
		return errors.Is(e, ErrRateLimited) && errors.As(e, &r) && r.RetryAt.Equal(at)
	}
}

func TestBodyLimit(t *testing.T) {
	_, c := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"title":"` + strings.Repeat("x", bodyLimit) + `"}`))
	})
	if _, err := c.Issue(context.Background(), "t", "o", "r", 1); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("large body: %v", err)
	}
}

func TestNotifications(t *testing.T) {
	since := time.Date(2026, 10, 23, 9, 50, 0, 0, time.FixedZone("JST", 9*3600))
	var base string
	f, c := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		w.Header().Set("X-Poll-Interval", "30")
		if page == "" {
			w.Header().Set("Last-Modified", "Sat, 24 Oct 2026 10:00:00 GMT")
		}
		next := map[string]string{"": "2", "2": "3", "3": "4"}[page]
		if next != "" {
			w.Header().Set("Link", `<`+base+`/notifications?all=true&page=`+next+`>; rel="next", <`+base+`/notifications?page=9>; rel="last"`)
		}
		_, _ = w.Write([]byte(`[{"reason":"review_requested","subject":{"type":"PullRequest","url":"` + base + `/repos/o/r/pulls/` + page + `1"}},{"reason":"mention","subject":{"type":"Issue","url":null}}]`))
	})
	base = c.BaseURL
	threads, lm, interval, notModified, err := c.Notifications(context.Background(), "t", "", since)
	if err != nil || notModified || interval != 60 || lm != "Sat, 24 Oct 2026 10:00:00 GMT" {
		t.Fatalf("notifications: %v %v %d %q", err, notModified, interval, lm)
	}
	// Three pages at most.
	if len(threads) != 6 || f.count() != 3 || threads[0] != (Thread{Reason: "review_requested", SubjectType: "PullRequest", SubjectURL: base + "/repos/o/r/pulls/1"}) || threads[1].SubjectURL != "" {
		t.Fatalf("threads: %+v requests=%d", threads, f.count())
	}
	q := f.requests[0].URL.Query()
	if f.requests[0].URL.Path != "/notifications" || q.Get("all") != "true" || q.Get("since") != "2026-10-23T00:50:00Z" || q.Get("per_page") != "50" || f.requests[0].Header.Get("If-Modified-Since") != "" {
		t.Fatalf("first request: %s %v", f.requests[0].URL, f.requests[0].Header)
	}

	// 304 with If-Modified-Since; the interval is at least 60 seconds.
	f, c = newFake(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Poll-Interval", "120")
		w.WriteHeader(304)
	})
	threads, lm, interval, notModified, err = c.Notifications(context.Background(), "t", "Fri, 23 Oct 2026 00:00:00 GMT", since)
	if err != nil || !notModified || len(threads) != 0 || interval != 120 || lm != "Fri, 23 Oct 2026 00:00:00 GMT" || f.requests[0].Header.Get("If-Modified-Since") != "Fri, 23 Oct 2026 00:00:00 GMT" {
		t.Fatalf("304: %v %v %d %q", err, notModified, interval, lm)
	}

	// A next link outside the base URL or the endpoint is not followed.
	for _, link := range []string{"https://evil.example/notifications?page=2", "BASE/user?page=2", "BASE/notifications/x?page=2", "BASE/notificationsx?page=2"} {
		f, c = newFake(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Link", `<`+strings.Replace(link, "BASE", base, 1)+`>; rel="next"`)
			_, _ = w.Write([]byte(`[]`))
		})
		base = c.BaseURL
		if _, _, _, _, err = c.Notifications(context.Background(), "t", "", since); err != nil || f.count() != 1 {
			t.Fatalf("%s: followed (%d requests) %v", link, f.count(), err)
		}
	}
}

func TestPullIssueReviewsSearch(t *testing.T) {
	var base string
	f, c := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/o/r/pulls/7":
			_, _ = w.Write([]byte(`{"state":"open","merged":false,"title":"Fix","html_url":"https://github.com/o/r/pull/7","requested_reviewers":[{"login":"Octocat"},null],"assignees":[]}`))
		case r.URL.Path == "/repos/o/r/issues/7":
			_, _ = w.Write([]byte(`{"state":"closed","title":"Fix","html_url":"https://github.com/o/r/pull/7","assignees":[{"login":"a"}],"pull_request":{"url":"x"}}`))
		case r.URL.Path == "/repos/o/r/issues/8":
			_, _ = w.Write([]byte(`{"state":"open","title":"Bug","html_url":"https://github.com/o/r/issues/8","assignees":[],"pull_request":null}`))
		case r.URL.Path == "/repos/o/r/pulls/7/reviews" && r.URL.Query().Get("page") == "":
			w.Header().Set("Link", `<`+base+`/repos/o/r/pulls/7/reviews?per_page=100&page=2>; rel="next", <`+base+`/repos/o/r/pulls/7/reviews?per_page=100&page=3>; rel="last"`)
			_, _ = w.Write([]byte(`[{"user":{"login":"old"},"submitted_at":"2026-01-01T00:00:00Z","html_url":"h1","state":"APPROVED"}]`))
		case r.URL.Path == "/repos/o/r/pulls/7/reviews" && r.URL.Query().Get("page") == "3":
			_, _ = w.Write([]byte(`[{"user":{"login":"octocat"},"submitted_at":"2026-10-24T09:00:00Z","html_url":"h3","state":"COMMENTED"},{"user":null,"submitted_at":null,"html_url":"h4","state":"PENDING"}]`))
		case r.URL.Path == "/search/issues":
			if r.URL.Query().Get("page") == "" {
				w.Header().Set("Link", `<`+base+`/search/issues?q=x&page=2>; rel="next"`)
				_, _ = w.Write([]byte(`{"items":[{"number":1,"title":"A","html_url":"https://github.com/o/r/pull/1","repository_url":"` + base + `/repos/o/r","pull_request":{}},{"number":2,"title":"B","html_url":"x","repository_url":"https://evil.example/repos/o/r"}]}`))
				return
			}
			w.Header().Set("Link", `<`+base+`/search/issues?q=x&page=3>; rel="next"`)
			_, _ = w.Write([]byte(`{"items":[{"number":3,"title":"C","html_url":"https://github.com/o/r/issues/3","repository_url":"` + base + `/repos/o/r"}]}`))
		default:
			w.WriteHeader(404)
		}
	})
	base = c.BaseURL
	ctx := context.Background()
	p, err := c.Pull(ctx, "t", "o", "r", 7)
	if err != nil || p.State != "open" || p.Merged || p.HTMLURL != "https://github.com/o/r/pull/7" || !reflect.DeepEqual(p.RequestedReviewers, []string{"Octocat"}) || len(p.Assignees) != 0 {
		t.Fatalf("pull: %+v %v", p, err)
	}
	i, err := c.Issue(ctx, "t", "o", "r", 7)
	if err != nil || !i.IsPull || i.State != "closed" || !reflect.DeepEqual(i.Assignees, []string{"a"}) {
		t.Fatalf("issue of PR: %+v %v", i, err)
	}
	if i, err = c.Issue(ctx, "t", "o", "r", 8); err != nil || i.IsPull {
		t.Fatalf("issue: %+v %v", i, err)
	}
	reviews, err := c.Reviews(ctx, "t", "o", "r", 7)
	if err != nil || len(reviews) != 2 || reviews[0].Login != "octocat" || reviews[0].HTMLURL != "h3" || reviews[1].SubmittedAt != nil || reviews[1].Login != "" {
		t.Fatalf("reviews (last page only): %+v %v", reviews, err)
	}
	items, err := c.SearchIssues(ctx, "t", "is:open is:pr user-review-requested:@me archived:false")
	if err != nil || len(items) != 2 || items[0] != (SearchItem{Owner: "o", Repo: "r", Number: 1, Title: "A", HTMLURL: "https://github.com/o/r/pull/1", IsPull: true}) || items[1].Number != 3 || items[1].IsPull {
		t.Fatalf("search: %+v %v", items, err)
	}
	var searches int
	for _, r := range f.requests {
		if r.URL.Path == "/search/issues" {
			searches++
		}
	}
	if searches != 2 || f.requests[len(f.requests)-2].URL.Query().Get("q") != "is:open is:pr user-review-requested:@me archived:false" || f.requests[len(f.requests)-2].URL.Query().Get("per_page") != "50" {
		t.Fatalf("search requests: %d", searches)
	}
	before := f.count()
	for _, name := range [][2]string{{"o/x", "r"}, {"o", ".."}, {".", "r"}, {"", "r"}, {"o", strings.Repeat("r", 101)}} {
		if _, err = c.Pull(ctx, "t", name[0], name[1], 1); err == nil {
			t.Fatalf("%v accepted", name)
		}
	}
	if f.count() != before {
		t.Fatal("invalid names were requested")
	}
}

func TestParseSubjectURL(t *testing.T) {
	c := New(time.Now)
	ok := map[string]Ref{
		"https://api.github.com/repos/Octo-Org/my.repo_1/pulls/12": {Owner: "Octo-Org", Repo: "my.repo_1", Kind: "pulls", Number: 12},
		"https://api.github.com/repos/o/r/issues/2147483647":       {Owner: "o", Repo: "r", Kind: "issues", Number: 2147483647},
	}
	for raw, want := range ok {
		if got, valid := c.ParseSubjectURL(raw); !valid || got != want {
			t.Fatalf("%s: %+v %v", raw, got, valid)
		}
	}
	for _, raw := range []string{
		"",
		"https://evil.example/repos/o/r/pulls/1",
		"https://api.github.com.evil.example/repos/o/r/pulls/1",
		"http://api.github.com/repos/o/r/pulls/1",
		"https://api.github.com/repos/o/r/pulls/1?x=1",
		"https://api.github.com/repos/o/r/pulls/1#x",
		"https://api.github.com/repos/o/r/pulls/1/",
		"https://api.github.com/repos/o/r/commits/1",
		"https://api.github.com/repos/o/r/pulls/01",
		"https://api.github.com/repos/o/r/pulls/0",
		"https://api.github.com/repos/o/r/pulls/2147483648",
		"https://api.github.com/repos/o/../pulls/1",
		"https://api.github.com/repos/./r/pulls/1",
		"https://api.github.com/repos/o%2Fx/r/pulls/1",
		"https://api.github.com/repos/o/r/x/pulls/1",
	} {
		if got, valid := c.ParseSubjectURL(raw); valid {
			t.Fatalf("%s accepted: %+v", raw, got)
		}
	}
}
