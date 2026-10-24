// Package github is a small client of the GitHub REST API for the GitHub
// integration. It sends the user's token only to the configured API base URL:
// URLs found in API responses are parsed and checked, never followed as is.
package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// DefaultBaseURL is the GitHub REST API.
const DefaultBaseURL = "https://api.github.com"

const (
	timeout   = 10 * time.Second
	bodyLimit = 2 << 20
	// errorBodyLimit bounds the part of an error response that is searched
	// for the secondary rate limit message.
	errorBodyLimit = 64 << 10
	// MinPollInterval is the shortest notification polling interval in
	// seconds, also used when GitHub does not send X-Poll-Interval.
	MinPollInterval   = 60
	notificationPages = 3
	searchPages       = 2
	// recentPages is how many of the last pages of comments and reviews
	// RecentReviews, IssueComments and ReviewComments read.
	recentPages         = 2
	secondaryRetryDelay = 60 * time.Second
)

var (
	// ErrUnauthorized is a 401: the token is invalid, expired or revoked.
	ErrUnauthorized = errors.New("github: unauthorized")
	// ErrForbidden is a 403 that is not a rate limit (for example SAML SSO).
	ErrForbidden = errors.New("github: forbidden")
	// ErrNotFound is a 404.
	ErrNotFound = errors.New("github: not found")
	// ErrUnprocessable is a 422, returned by search for queries it rejects.
	ErrUnprocessable = errors.New("github: unprocessable request")
	// ErrRateLimited matches every *RateLimitError with errors.Is.
	ErrRateLimited = errors.New("github: rate limited")
)

// RateLimitError is a primary or secondary rate limit. RetryAt is when the
// request may be retried, or zero when GitHub did not say.
type RateLimitError struct {
	RetryAt time.Time
}

func (e *RateLimitError) Error() string { return "github: rate limited" }

// Is makes errors.Is(err, ErrRateLimited) true.
func (e *RateLimitError) Is(target error) bool { return target == ErrRateLimited }

// StatusError is an unexpected HTTP status, including 5xx and redirects.
type StatusError struct {
	Code int
}

func (e *StatusError) Error() string { return fmt.Sprintf("github: unexpected status %d", e.Code) }

// Client calls the GitHub REST API. BaseURL and HTTP are replaced by tests.
type Client struct {
	BaseURL string
	HTTP    *http.Client
	Now     func() time.Time
}

// New returns a client of the GitHub API that does not follow redirects.
func New(now func() time.Time) *Client {
	return &Client{BaseURL: DefaultBaseURL, Now: now, HTTP: &http.Client{
		Timeout: timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}}
}

// Thread is the part of a notification thread the integration uses.
type Thread struct {
	// ID is the thread ID and UpdatedAt the time of its latest update.
	ID          string
	UpdatedAt   time.Time
	Reason      string
	SubjectType string
	// SubjectURL is the API URL of the subject, or empty when it is null.
	SubjectURL string
}

// Pull is the part of a pull request the integration uses.
type Pull struct {
	State              string
	Merged             bool
	Title              string
	HTMLURL            string
	RequestedReviewers []string
	Assignees          []string
}

// Issue is the part of an issue (or of a pull request read through the
// issues API) the integration uses. Merged is set for a merged pull
// request. Login is empty when the author was deleted.
type Issue struct {
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

// Review is a pull request review. SubmittedAt is nil for pending reviews.
// Login is empty when the author was deleted.
type Review struct {
	ID          int64
	Login       string
	Body        string
	SubmittedAt *time.Time
	HTMLURL     string
	State       string
}

// Comment is an issue comment or a pull request review comment. Login is
// empty when the author was deleted.
type Comment struct {
	ID        int64
	Login     string
	Body      string
	HTMLURL   string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// SearchItem is an issue or pull request found by SearchIssues. Owner and
// Repo come from its repository_url, which must point at BaseURL.
type SearchItem struct {
	Owner, Repo string
	Number      int
	Title       string
	HTMLURL     string
	IsPull      bool
}

// Ref names an issue or pull request. Kind is "pulls" or "issues".
type Ref struct {
	Owner, Repo, Kind string
	Number            int
}

var namePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}$`)
var numberPattern = regexp.MustCompile(`^[1-9][0-9]{0,9}$`)

// ValidName reports whether v can be used as an owner or repository name in
// an API path.
func ValidName(v string) bool {
	return namePattern.MatchString(v) && v != "." && v != ".."
}

func parseNumber(v string) (int, bool) {
	if !numberPattern.MatchString(v) {
		return 0, false
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n > 2147483647 {
		return 0, false
	}
	return int(n), true
}

// ParseSubjectURL parses the API URL of a notification subject. Only
// <BaseURL>/repos/{owner}/{repo}/{pulls|issues}/{number} exactly is accepted,
// without a query or fragment.
func (c *Client) ParseSubjectURL(raw string) (Ref, bool) {
	rest, ok := strings.CutPrefix(raw, c.BaseURL+"/repos/")
	if !ok {
		return Ref{}, false
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 4 || !ValidName(parts[0]) || !ValidName(parts[1]) || (parts[2] != "pulls" && parts[2] != "issues") {
		return Ref{}, false
	}
	n, ok := parseNumber(parts[3])
	if !ok {
		return Ref{}, false
	}
	return Ref{Owner: parts[0], Repo: parts[1], Kind: parts[2], Number: n}, true
}

// parseRepositoryURL parses <BaseURL>/repos/{owner}/{repo}.
func (c *Client) parseRepositoryURL(raw string) (string, string, bool) {
	rest, ok := strings.CutPrefix(raw, c.BaseURL+"/repos/")
	if !ok {
		return "", "", false
	}
	owner, repo, ok := strings.Cut(rest, "/")
	if !ok || !ValidName(owner) || !ValidName(repo) {
		return "", "", false
	}
	return owner, repo, true
}

type response struct {
	status int
	header http.Header
	body   []byte
}

// get sends a GET request to rawURL, which callers build from BaseURL.
func (c *Client) get(ctx context.Context, token, rawURL string, header http.Header) (response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return response{}, err
	}
	for k, v := range header {
		req.Header[k] = v
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "astovia")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return response{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	limit := int64(bodyLimit)
	if resp.StatusCode >= 300 {
		limit = errorBodyLimit
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return response{}, err
	}
	if int64(len(body)) > limit {
		if resp.StatusCode < 300 {
			return response{}, errors.New("github: response body is too large")
		}
		body = body[:limit]
	}
	return response{status: resp.StatusCode, header: resp.Header, body: body}, nil
}

// check converts a non-2xx response to an error.
func (c *Client) check(r response) error {
	switch {
	case r.status >= 200 && r.status < 300:
		return nil
	case r.status == http.StatusUnauthorized:
		return ErrUnauthorized
	case r.status == http.StatusNotFound:
		return ErrNotFound
	case r.status == http.StatusUnprocessableEntity:
		return ErrUnprocessable
	case r.status == http.StatusForbidden || r.status == http.StatusTooManyRequests:
		if at, ok := c.retryAt(r); ok {
			return &RateLimitError{RetryAt: at}
		}
		if r.status == http.StatusTooManyRequests {
			return &RateLimitError{}
		}
		return ErrForbidden
	}
	return &StatusError{Code: r.status}
}

// retryAt finds a rate limit in a 403 or 429 response: Retry-After seconds,
// then X-RateLimit-Reset when X-RateLimit-Remaining is 0, then the secondary
// rate limit message in the body (retry after 60 seconds).
func (c *Client) retryAt(r response) (time.Time, bool) {
	now := c.Now()
	if v := strings.TrimSpace(r.header.Get("Retry-After")); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			return now.Add(time.Duration(n) * time.Second), true
		}
	}
	if strings.TrimSpace(r.header.Get("X-RateLimit-Remaining")) == "0" {
		if n, err := strconv.ParseInt(strings.TrimSpace(r.header.Get("X-RateLimit-Reset")), 10, 64); err == nil && n > 0 {
			return time.Unix(n, 0), true
		}
	}
	if strings.Contains(strings.ToLower(string(r.body)), "secondary rate limit") {
		return now.Add(secondaryRetryDelay), true
	}
	return time.Time{}, false
}

func (c *Client) getJSON(ctx context.Context, token, rawURL string, out any) (response, error) {
	r, err := c.get(ctx, token, rawURL, nil)
	if err != nil {
		return response{}, err
	}
	if err = c.check(r); err != nil {
		return response{}, err
	}
	if err = json.Unmarshal(r.body, out); err != nil {
		return response{}, fmt.Errorf("github: invalid JSON: %w", err)
	}
	return r, nil
}

// links parses a Link header into rel -> URL.
func links(h string) map[string]string {
	out := map[string]string{}
	for _, part := range strings.Split(h, ",") {
		part = strings.TrimSpace(part)
		end := strings.Index(part, ">")
		if !strings.HasPrefix(part, "<") || end < 0 {
			continue
		}
		target := part[1:end]
		for _, param := range strings.Split(part[end+1:], ";") {
			name, value, ok := strings.Cut(strings.TrimSpace(param), "=")
			if ok && strings.TrimSpace(name) == "rel" {
				for _, rel := range strings.Fields(strings.Trim(strings.TrimSpace(value), `"`)) {
					out[rel] = target
				}
			}
		}
	}
	return out
}

// pageLink returns the rel link of r when it points at the same endpoint as
// path under BaseURL (only the query may differ), so that the token is never
// sent elsewhere.
func (c *Client) pageLink(r response, rel, path string) (string, bool) {
	v, ok := links(r.header.Get("Link"))[rel]
	if !ok || !strings.HasPrefix(v, c.BaseURL+path+"?") || strings.Contains(v, "#") {
		return "", false
	}
	return v, true
}

type userJSON struct {
	Login string `json:"login"`
}

func logins(users []*userJSON) []string {
	out := make([]string, 0, len(users))
	for _, u := range users {
		if u != nil && u.Login != "" {
			out = append(out, u.Login)
		}
	}
	return out
}

// User returns the login of the token's user and the token's classic scopes
// from X-OAuth-Scopes (empty for fine-grained tokens), sorted without
// duplicates.
func (c *Client) User(ctx context.Context, token string) (string, []string, error) {
	var u userJSON
	r, err := c.getJSON(ctx, token, c.BaseURL+"/user", &u)
	if err != nil {
		return "", nil, err
	}
	if u.Login == "" {
		return "", nil, errors.New("github: user has no login")
	}
	return u.Login, ParseScopes(r.header.Get("X-OAuth-Scopes")), nil
}

// ParseScopes splits an X-OAuth-Scopes value.
func ParseScopes(v string) []string {
	seen := map[string]bool{}
	scopes := []string{}
	for _, s := range strings.Split(v, ",") {
		s = strings.TrimSpace(s)
		if s != "" && !seen[s] {
			seen[s] = true
			scopes = append(scopes, s)
		}
	}
	sort.Strings(scopes)
	return scopes
}

// pollInterval reads X-Poll-Interval, at least MinPollInterval seconds.
func pollInterval(h http.Header) int {
	n, err := strconv.Atoi(strings.TrimSpace(h.Get("X-Poll-Interval")))
	if err != nil || n < MinPollInterval {
		return MinPollInterval
	}
	return n
}

// Notifications lists notification threads updated since the time, following
// up to three pages. lastModified (when not empty) is sent as
// If-Modified-Since; a 304 response returns notModified with it unchanged.
func (c *Client) Notifications(ctx context.Context, token, lastModified string, since time.Time) (threads []Thread, newLastModified string, interval int, notModified bool, err error) {
	q := url.Values{"all": {"true"}, "since": {since.UTC().Format(time.RFC3339)}, "per_page": {"50"}}
	next := c.BaseURL + "/notifications?" + q.Encode()
	threads = []Thread{}
	for page := 0; page < notificationPages && next != ""; page++ {
		header := http.Header{}
		if page == 0 && lastModified != "" {
			header.Set("If-Modified-Since", lastModified)
		}
		r, e := c.get(ctx, token, next, header)
		if e != nil {
			return nil, "", 0, false, e
		}
		if page == 0 {
			interval = pollInterval(r.header)
			if r.status == http.StatusNotModified {
				return []Thread{}, lastModified, interval, true, nil
			}
			newLastModified = r.header.Get("Last-Modified")
		}
		if e = c.check(r); e != nil {
			return nil, "", 0, false, e
		}
		var list []struct {
			ID        string    `json:"id"`
			UpdatedAt time.Time `json:"updated_at"`
			Reason    string    `json:"reason"`
			Subject   struct {
				Type string  `json:"type"`
				URL  *string `json:"url"`
			} `json:"subject"`
		}
		if e = json.Unmarshal(r.body, &list); e != nil {
			return nil, "", 0, false, fmt.Errorf("github: invalid JSON: %w", e)
		}
		for _, t := range list {
			th := Thread{ID: t.ID, UpdatedAt: t.UpdatedAt, Reason: t.Reason, SubjectType: t.Subject.Type}
			if t.Subject.URL != nil {
				th.SubjectURL = *t.Subject.URL
			}
			threads = append(threads, th)
		}
		next, _ = c.pageLink(r, "next", "/notifications")
	}
	return threads, newLastModified, interval, false, nil
}

func (c *Client) repoPath(owner, repo string) (string, error) {
	if !ValidName(owner) || !ValidName(repo) {
		return "", errors.New("github: invalid repository name")
	}
	return "/repos/" + owner + "/" + repo, nil
}

// Pull returns a pull request.
func (c *Client) Pull(ctx context.Context, token, owner, repo string, number int) (Pull, error) {
	path, err := c.repoPath(owner, repo)
	if err != nil {
		return Pull{}, err
	}
	var p struct {
		State              string      `json:"state"`
		Merged             bool        `json:"merged"`
		Title              string      `json:"title"`
		HTMLURL            string      `json:"html_url"`
		RequestedReviewers []*userJSON `json:"requested_reviewers"`
		Assignees          []*userJSON `json:"assignees"`
	}
	if _, err = c.getJSON(ctx, token, c.BaseURL+path+"/pulls/"+strconv.Itoa(number), &p); err != nil {
		return Pull{}, err
	}
	return Pull{State: p.State, Merged: p.Merged, Title: p.Title, HTMLURL: p.HTMLURL, RequestedReviewers: logins(p.RequestedReviewers), Assignees: logins(p.Assignees)}, nil
}

// Issue returns an issue, or a pull request through the issues API.
func (c *Client) Issue(ctx context.Context, token, owner, repo string, number int) (Issue, error) {
	path, err := c.repoPath(owner, repo)
	if err != nil {
		return Issue{}, err
	}
	var i struct {
		State       string      `json:"state"`
		Title       string      `json:"title"`
		HTMLURL     string      `json:"html_url"`
		Assignees   []*userJSON `json:"assignees"`
		PullRequest *struct {
			MergedAt *time.Time `json:"merged_at"`
		} `json:"pull_request"`
		Body      *string   `json:"body"`
		User      *userJSON `json:"user"`
		CreatedAt time.Time `json:"created_at"`
		UpdatedAt time.Time `json:"updated_at"`
	}
	if _, err = c.getJSON(ctx, token, c.BaseURL+path+"/issues/"+strconv.Itoa(number), &i); err != nil {
		return Issue{}, err
	}
	out := Issue{State: i.State, Title: i.Title, HTMLURL: i.HTMLURL, Assignees: logins(i.Assignees), IsPull: i.PullRequest != nil, CreatedAt: i.CreatedAt, UpdatedAt: i.UpdatedAt}
	out.Merged = i.PullRequest != nil && i.PullRequest.MergedAt != nil
	if i.Body != nil {
		out.Body = *i.Body
	}
	if i.User != nil {
		out.Login = i.User.Login
	}
	return out, nil
}

// Reviews returns the reviews of a pull request on the last page of 100
// (the newest ones), or on the first page when there is only one.
func (c *Client) Reviews(ctx context.Context, token, owner, repo string, number int) ([]Review, error) {
	return c.reviews(ctx, token, owner, repo, number, 1)
}

// RecentReviews returns the reviews of a pull request on the last two pages
// of 100, oldest first.
func (c *Client) RecentReviews(ctx context.Context, token, owner, repo string, number int) ([]Review, error) {
	return c.reviews(ctx, token, owner, repo, number, recentPages)
}

func (c *Client) reviews(ctx context.Context, token, owner, repo string, number, pages int) ([]Review, error) {
	path, err := c.repoPath(owner, repo)
	if err != nil {
		return nil, err
	}
	path += "/pulls/" + strconv.Itoa(number) + "/reviews"
	type reviewJSON struct {
		ID          int64      `json:"id"`
		User        *userJSON  `json:"user"`
		Body        *string    `json:"body"`
		SubmittedAt *time.Time `json:"submitted_at"`
		HTMLURL     string     `json:"html_url"`
		State       string     `json:"state"`
	}
	var list []reviewJSON
	if err = lastPages(ctx, c, token, path, pages, &list); err != nil {
		return nil, err
	}
	out := make([]Review, 0, len(list))
	for _, v := range list {
		rv := Review{ID: v.ID, SubmittedAt: v.SubmittedAt, HTMLURL: v.HTMLURL, State: v.State}
		if v.User != nil {
			rv.Login = v.User.Login
		}
		if v.Body != nil {
			rv.Body = *v.Body
		}
		out = append(out, rv)
	}
	return out, nil
}

// IssueComments returns the comments of an issue or pull request on the
// last two pages of 100, oldest first.
func (c *Client) IssueComments(ctx context.Context, token, owner, repo string, number int) ([]Comment, error) {
	return c.comments(ctx, token, owner, repo, "/issues/"+strconv.Itoa(number)+"/comments")
}

// ReviewComments returns the review comments of a pull request on the last
// two pages of 100, oldest first.
func (c *Client) ReviewComments(ctx context.Context, token, owner, repo string, number int) ([]Comment, error) {
	return c.comments(ctx, token, owner, repo, "/pulls/"+strconv.Itoa(number)+"/comments")
}

func (c *Client) comments(ctx context.Context, token, owner, repo, suffix string) ([]Comment, error) {
	path, err := c.repoPath(owner, repo)
	if err != nil {
		return nil, err
	}
	var list []struct {
		ID        int64     `json:"id"`
		User      *userJSON `json:"user"`
		Body      *string   `json:"body"`
		HTMLURL   string    `json:"html_url"`
		CreatedAt time.Time `json:"created_at"`
		UpdatedAt time.Time `json:"updated_at"`
	}
	if err = lastPages(ctx, c, token, path+suffix, recentPages, &list); err != nil {
		return nil, err
	}
	out := make([]Comment, 0, len(list))
	for _, v := range list {
		cm := Comment{ID: v.ID, HTMLURL: v.HTMLURL, CreatedAt: v.CreatedAt, UpdatedAt: v.UpdatedAt}
		if v.User != nil {
			cm.Login = v.User.Login
		}
		if v.Body != nil {
			cm.Body = *v.Body
		}
		out = append(out, cm)
	}
	return out, nil
}

// lastPages reads the last page of 100 of a list endpoint into out (a
// pointer to a slice), or the last two pages oldest first when pages is 2.
// The first page is read to learn the last one from its Link header; the
// page before the last comes from the last page's prev link, or is the
// first page itself. Every URL must pass pageLink, so the token is never
// sent elsewhere.
func lastPages[T any](ctx context.Context, c *Client, token, path string, pages int, out *[]T) error {
	var first []T
	r, err := c.getJSON(ctx, token, c.BaseURL+path+"?per_page=100", &first)
	if err != nil {
		return err
	}
	last, ok := c.pageLink(r, "last", path)
	if !ok {
		*out = first
		return nil
	}
	var tail []T
	lr, err := c.getJSON(ctx, token, last, &tail)
	if err != nil {
		return err
	}
	prev, ok := c.pageLink(lr, "prev", path)
	if pages < 2 || !ok {
		*out = tail
		return nil
	}
	if u, e := url.Parse(prev); e == nil && u.Query().Get("page") == "1" {
		*out = append(first, tail...)
		return nil
	}
	var before []T
	if _, err = c.getJSON(ctx, token, prev, &before); err != nil {
		return err
	}
	*out = append(before, tail...)
	return nil
}

// SearchIssues searches issues and pull requests, following up to two pages
// of 50. Items whose repository_url does not point at BaseURL are skipped.
func (c *Client) SearchIssues(ctx context.Context, token, query string) ([]SearchItem, error) {
	next := c.BaseURL + "/search/issues?" + url.Values{"q": {query}, "per_page": {"50"}}.Encode()
	out := []SearchItem{}
	for page := 0; page < searchPages && next != ""; page++ {
		var result struct {
			Items []struct {
				Number        int             `json:"number"`
				Title         string          `json:"title"`
				HTMLURL       string          `json:"html_url"`
				RepositoryURL string          `json:"repository_url"`
				PullRequest   json.RawMessage `json:"pull_request"`
			} `json:"items"`
		}
		r, err := c.getJSON(ctx, token, next, &result)
		if err != nil {
			return nil, err
		}
		for _, it := range result.Items {
			owner, repo, ok := c.parseRepositoryURL(it.RepositoryURL)
			if !ok || it.Number < 1 {
				continue
			}
			out = append(out, SearchItem{Owner: owner, Repo: repo, Number: it.Number, Title: it.Title, HTMLURL: it.HTMLURL, IsPull: len(it.PullRequest) > 0 && string(it.PullRequest) != "null"})
		}
		next, _ = c.pageLink(r, "next", "/search/issues")
	}
	return out, nil
}
