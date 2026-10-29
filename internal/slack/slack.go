// Package slack is a small client of the Slack Web API for the Slack
// integration. It reads only, with the user's token sent in the
// Authorization header to BaseURL (https://slack.com/api) and nowhere else:
// URLs found in responses, such as the workspace URL, are never requested.
// Errors carry the method, the HTTP status or Slack's error code, never the
// request URL, the response body or the token.
package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// DefaultBaseURL is the Slack Web API.
const DefaultBaseURL = "https://slack.com/api"

const (
	timeout   = 10 * time.Second
	bodyLimit = 2 << 20
)

var (
	// ErrUnauthorized is an invalid, revoked or expired token, or a
	// deactivated account.
	ErrUnauthorized = errors.New("slack: unauthorized")
	// ErrMissingScope is a token without a scope the method needs.
	ErrMissingScope = errors.New("slack: missing scope")
	// ErrRateLimited matches every *RateLimitError with errors.Is.
	ErrRateLimited = errors.New("slack: rate limited")
)

// authCodes are the error codes of an unusable token.
var authCodes = map[string]bool{"invalid_auth": true, "not_authed": true, "token_revoked": true, "token_expired": true, "account_inactive": true}

// temporaryCodes are the error codes of a failure on Slack's side.
var temporaryCodes = map[string]bool{"internal_error": true, "fatal_error": true, "service_unavailable": true, "request_timeout": true}

// RateLimitError is an HTTP 429. RetryAt is when the request may be retried,
// or zero when Slack did not send Retry-After.
type RateLimitError struct {
	Method  string
	RetryAt time.Time
}

func (e *RateLimitError) Error() string { return "slack: " + e.Method + ": rate limited" }

// Is makes errors.Is(err, ErrRateLimited) true.
func (e *RateLimitError) Is(target error) bool { return target == ErrRateLimited }

// APIError is a response with "ok": false whose code is neither an
// authentication error nor missing_scope.
type APIError struct {
	Method string
	Code   string
}

func (e *APIError) Error() string { return "slack: " + e.Method + ": " + e.Code }

// Temporary reports whether the code is a failure on Slack's side
// (internal_error, fatal_error, service_unavailable, request_timeout).
func (e *APIError) Temporary() bool { return temporaryCodes[e.Code] }

// StatusError is an unexpected HTTP status, including 5xx and redirects.
type StatusError struct {
	Method string
	Code   int
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("slack: %s: unexpected status %d", e.Method, e.Code)
}

// Client calls the Slack Web API. BaseURL and HTTP are replaced by tests.
type Client struct {
	BaseURL string
	HTTP    *http.Client
	Now     func() time.Time
}

// New returns a client of the Slack Web API that does not follow redirects.
func New(now func() time.Time) *Client {
	return &Client{BaseURL: DefaultBaseURL, Now: now, HTTP: &http.Client{
		Timeout: timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}}
}

// call sends a GET request for method with the query and decodes the
// response into out, which must embed envelope. The response header is
// returned for auth.test.
func (c *Client) call(ctx context.Context, token, method string, query url.Values, out interface{ result() *envelope }) (http.Header, error) {
	rawURL := c.BaseURL + "/" + method
	if len(query) > 0 {
		rawURL += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("slack: %s: invalid request", method)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", "astovia")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		// *url.Error includes the URL with its query; only the cause is
		// kept.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, fmt.Errorf("slack: %s: %w", method, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		e := &RateLimitError{Method: method}
		if n, err := strconv.ParseInt(strings.TrimSpace(resp.Header.Get("Retry-After")), 10, 64); err == nil && n >= 0 {
			e.RetryAt = c.Now().Add(time.Duration(n) * time.Second)
		}
		return nil, e
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &StatusError{Method: method, Code: resp.StatusCode}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, bodyLimit+1))
	if err != nil {
		return nil, fmt.Errorf("slack: %s: %w", method, err)
	}
	if len(body) > bodyLimit {
		return nil, fmt.Errorf("slack: %s: response body is too large", method)
	}
	if err = json.Unmarshal(body, out); err != nil {
		return nil, fmt.Errorf("slack: %s: invalid JSON", method)
	}
	env := out.result()
	if !env.OK {
		switch {
		case authCodes[env.Error]:
			return nil, fmt.Errorf("%w (%s: %s)", ErrUnauthorized, method, env.Error)
		case env.Error == "missing_scope":
			return nil, fmt.Errorf("%w (%s)", ErrMissingScope, method)
		}
		code := env.Error
		if code == "" {
			code = "unknown_error"
		}
		return nil, &APIError{Method: method, Code: code}
	}
	return resp.Header, nil
}

type envelope struct {
	OK               bool   `json:"ok"`
	Error            string `json:"error"`
	ResponseMetadata struct {
		NextCursor string `json:"next_cursor"`
	} `json:"response_metadata"`
}

func (e *envelope) result() *envelope { return e }

// Auth is the identity of a token from auth.test.
type Auth struct {
	UserID string
	User   string
	TeamID string
	Team   string
	URL    string
}

// AuthTest returns the token's user and workspace and its scopes from
// X-OAuth-Scopes, sorted without duplicates.
func (c *Client) AuthTest(ctx context.Context, token string) (Auth, []string, error) {
	var out struct {
		envelope
		URL    string `json:"url"`
		Team   string `json:"team"`
		User   string `json:"user"`
		TeamID string `json:"team_id"`
		UserID string `json:"user_id"`
	}
	h, err := c.call(ctx, token, "auth.test", nil, &out)
	if err != nil {
		return Auth{}, nil, err
	}
	return Auth{UserID: out.UserID, User: out.User, TeamID: out.TeamID, Team: out.Team, URL: out.URL}, ParseScopes(h.Get("X-OAuth-Scopes")), nil
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

// Message is the part of a Slack message the integration uses. ThreadTS is
// the parent's ts for a message in a thread (equal to TS for the parent
// itself). ReplyCount is the number of replies of a parent. Permalink and
// Channel are set only by SearchMessages.
type Message struct {
	TS          string
	User        string
	BotID       string
	Subtype     string
	Text        string
	ThreadTS    string
	LatestReply string
	ReplyCount  int
	Permalink   string
	Channel     Channel
	// HasBotID and HasSubtype tell whether the response included the
	// fields at all; search results may lack them.
	HasBotID   bool
	HasSubtype bool
}

// Channel is a conversation as described by search results and
// users.conversations. User is the other member of a direct message
// (users.conversations only).
type Channel struct {
	ID        string
	Name      string
	IsIM      bool
	IsMPIM    bool
	IsPrivate bool
	User      string
}

type messageJSON struct {
	TS          string           `json:"ts"`
	User        string           `json:"user"`
	BotID       *string          `json:"bot_id"`
	Subtype     *string          `json:"subtype"`
	Text        string           `json:"text"`
	ThreadTS    string           `json:"thread_ts"`
	LatestReply string           `json:"latest_reply"`
	ReplyCount  int              `json:"reply_count"`
	Permalink   string           `json:"permalink"`
	Channel     *json.RawMessage `json:"channel"`
}

type channelJSON struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	IsIM      bool   `json:"is_im"`
	IsMPIM    bool   `json:"is_mpim"`
	IsPrivate bool   `json:"is_private"`
	User      string `json:"user"`
}

func (m messageJSON) message() Message {
	out := Message{TS: m.TS, User: m.User, Text: m.Text, ThreadTS: m.ThreadTS, LatestReply: m.LatestReply, ReplyCount: m.ReplyCount, Permalink: m.Permalink}
	if m.BotID != nil {
		out.BotID, out.HasBotID = *m.BotID, true
	}
	if m.Subtype != nil {
		out.Subtype, out.HasSubtype = *m.Subtype, true
	}
	// In search results "channel" is an object; elsewhere it may be an ID.
	if m.Channel != nil {
		var ch channelJSON
		if json.Unmarshal(*m.Channel, &ch) == nil {
			out.Channel = Channel(ch)
		}
	}
	return out
}

func messages(list []messageJSON) []Message {
	out := make([]Message, 0, len(list))
	for _, m := range list {
		out = append(out, m.message())
	}
	return out
}

// SearchPage is one page of search.messages. Pages is the number of pages
// and Total the number of matches (0 when Slack did not tell).
type SearchPage struct {
	Matches []Message
	Page    int
	Pages   int
	Total   int
}

// SearchMessages runs search.messages for one page (from 1) of up to count
// matches, newest first.
func (c *Client) SearchMessages(ctx context.Context, token, query string, count, page int) (SearchPage, error) {
	var out struct {
		envelope
		Messages struct {
			Matches []messageJSON `json:"matches"`
			Paging  struct {
				Page  int `json:"page"`
				Pages int `json:"pages"`
				Total int `json:"total"`
			} `json:"paging"`
		} `json:"messages"`
	}
	q := url.Values{"query": {query}, "sort": {"timestamp"}, "sort_dir": {"desc"}, "count": {strconv.Itoa(count)}, "page": {strconv.Itoa(page)}}
	if _, err := c.call(ctx, token, "search.messages", q, &out); err != nil {
		return SearchPage{}, err
	}
	return SearchPage{Matches: messages(out.Messages.Matches), Page: out.Messages.Paging.Page, Pages: out.Messages.Paging.Pages, Total: out.Messages.Paging.Total}, nil
}

// UserConversations returns one page of the conversations of the given
// types (for example "mpim") the user is a member of, archived ones
// excluded.
func (c *Client) UserConversations(ctx context.Context, token, types string, limit int) ([]Channel, error) {
	list, _, err := c.UserConversationsPage(ctx, token, types, limit, "")
	return list, err
}

// UserConversationsPage is UserConversations from cursor (the first page
// when empty). It also returns the cursor of the next page, empty on the
// last page.
func (c *Client) UserConversationsPage(ctx context.Context, token, types string, limit int, cursor string) ([]Channel, string, error) {
	var out struct {
		envelope
		Channels []channelJSON `json:"channels"`
	}
	q := url.Values{"types": {types}, "exclude_archived": {"true"}, "limit": {strconv.Itoa(limit)}}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	if _, err := c.call(ctx, token, "users.conversations", q, &out); err != nil {
		return nil, "", err
	}
	list := make([]Channel, 0, len(out.Channels))
	for _, ch := range out.Channels {
		list = append(list, Channel(ch))
	}
	return list, out.ResponseMetadata.NextCursor, nil
}

// HistoryParams are the arguments of conversations.history and
// conversations.replies. Empty values are not sent. TS is the thread
// parent (replies only).
type HistoryParams struct {
	Channel   string
	TS        string
	Oldest    string
	Latest    string
	Inclusive bool
	Limit     int
	Cursor    string
}

func (p HistoryParams) query() url.Values {
	q := url.Values{"channel": {p.Channel}}
	for k, v := range map[string]string{"ts": p.TS, "oldest": p.Oldest, "latest": p.Latest, "cursor": p.Cursor} {
		if v != "" {
			q.Set(k, v)
		}
	}
	if p.Inclusive {
		q.Set("inclusive", "true")
	}
	if p.Limit > 0 {
		q.Set("limit", strconv.Itoa(p.Limit))
	}
	return q
}

// HistoryPage is one page of conversations.history or
// conversations.replies.
type HistoryPage struct {
	Messages   []Message
	HasMore    bool
	NextCursor string
}

// History runs conversations.history (newest first).
func (c *Client) History(ctx context.Context, token string, p HistoryParams) (HistoryPage, error) {
	p.TS = ""
	return c.history(ctx, token, "conversations.history", p)
}

// Replies runs conversations.replies for the thread whose parent is p.TS.
func (c *Client) Replies(ctx context.Context, token string, p HistoryParams) (HistoryPage, error) {
	return c.history(ctx, token, "conversations.replies", p)
}

func (c *Client) history(ctx context.Context, token, method string, p HistoryParams) (HistoryPage, error) {
	var out struct {
		envelope
		Messages []messageJSON `json:"messages"`
		HasMore  bool          `json:"has_more"`
	}
	if _, err := c.call(ctx, token, method, p.query(), &out); err != nil {
		return HistoryPage{}, err
	}
	return HistoryPage{Messages: messages(out.Messages), HasMore: out.HasMore, NextCursor: out.ResponseMetadata.NextCursor}, nil
}

// User is the part of a Slack user the integration uses.
type User struct {
	ID          string
	Name        string
	RealName    string
	DisplayName string
}

// UserInfo runs users.info.
func (c *Client) UserInfo(ctx context.Context, token, userID string) (User, error) {
	var out struct {
		envelope
		User struct {
			ID       string `json:"id"`
			Name     string `json:"name"`
			RealName string `json:"real_name"`
			Profile  struct {
				DisplayName string `json:"display_name"`
				RealName    string `json:"real_name"`
			} `json:"profile"`
		} `json:"user"`
	}
	if _, err := c.call(ctx, token, "users.info", url.Values{"user": {userID}}, &out); err != nil {
		return User{}, err
	}
	u := User{ID: out.User.ID, Name: out.User.Name, RealName: out.User.Profile.RealName, DisplayName: out.User.Profile.DisplayName}
	if u.RealName == "" {
		u.RealName = out.User.RealName
	}
	return u, nil
}

// Permalink runs chat.getPermalink.
func (c *Client) Permalink(ctx context.Context, token, channel, ts string) (string, error) {
	var out struct {
		envelope
		Permalink string `json:"permalink"`
	}
	if _, err := c.call(ctx, token, "chat.getPermalink", url.Values{"channel": {channel}, "message_ts": {ts}}, &out); err != nil {
		return "", err
	}
	return out.Permalink, nil
}

// ValidURL reports whether v is an https URL on a slack.com host, without
// user information, NUL or more than 2048 bytes.
func ValidURL(v string) bool {
	u, err := url.Parse(v)
	if err != nil || u.Scheme != "https" || len(v) > 2048 || strings.ContainsRune(v, 0) || u.User != nil {
		return false
	}
	return strings.HasSuffix(strings.ToLower(u.Hostname()), ".slack.com")
}

var markupPattern = regexp.MustCompile(`<([^<>]*)>`)

// FormatText turns Slack markup into plain text: <@U1> and <@U1|name>
// become @name(U1) with the name given by name, <#C1|name> becomes #name,
// <url|text> becomes "text (url)" and <url> the URL. HTML entities are
// decoded. An error of name is returned as is.
func FormatText(text string, name func(id string) (string, error)) (string, error) {
	var err error
	out := markupPattern.ReplaceAllStringFunc(text, func(m string) string {
		inner := m[1 : len(m)-1]
		target, label, hasLabel := strings.Cut(inner, "|")
		switch {
		case strings.HasPrefix(target, "@"):
			id := target[1:]
			v, e := name(id)
			if e != nil {
				err = e
				return m
			}
			return "@" + v + "(" + id + ")"
		case strings.HasPrefix(target, "#"):
			if hasLabel && label != "" {
				return "#" + label
			}
			return target
		case strings.HasPrefix(target, "!"):
			if hasLabel && label != "" {
				return label
			}
			return "@" + strings.TrimPrefix(target, "!")
		case hasLabel && label != "":
			return label + " (" + target + ")"
		}
		return target
	})
	if err != nil {
		return "", err
	}
	return html.UnescapeString(out), nil
}

// CleanText removes control and format characters and surrounding spaces.
func CleanText(v string) string {
	v = strings.Map(func(r rune) rune {
		if unicode.In(r, unicode.Cc, unicode.Cf, unicode.Zl, unicode.Zp) {
			return -1
		}
		return r
	}, v)
	return strings.TrimFunc(v, unicode.IsSpace)
}
