// Package chatwork is a small client of the Chatwork API v2 for the Chatwork
// integration. It reads only, with the user's API token sent in the
// x-chatworktoken header to BaseURL (https://api.chatwork.com/v2) and
// nowhere else: URLs found in responses are never requested. Errors carry
// the method and the HTTP status, never the request URL, the response body
// or the token.
package chatwork

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// DefaultBaseURL is the Chatwork API v2.
const DefaultBaseURL = "https://api.chatwork.com/v2"

const (
	timeout   = 10 * time.Second
	bodyLimit = 8 << 20
	// maxRetryWait bounds the reset time of a rate limit that is trusted.
	maxRetryWait = time.Hour
)

var (
	// ErrUnauthorized is an invalid or revoked token (HTTP 401).
	ErrUnauthorized = errors.New("chatwork: unauthorized")
	// ErrRateLimited matches every *RateLimitError with errors.Is.
	ErrRateLimited = errors.New("chatwork: rate limited")
	// ErrUnexpected is a response whose shape is not the documented one.
	ErrUnexpected = errors.New("chatwork: unexpected response")
)

var (
	// idPattern is an account_id, room_id or task_id in decimal without a
	// leading zero.
	idPattern = regexp.MustCompile(`^[1-9][0-9]{0,19}$`)
	// messageIDPattern is a message_id in decimal without a leading zero.
	messageIDPattern = regexp.MustCompile(`^[1-9][0-9]{0,29}$`)
)

// ValidID reports whether v is an account_id, room_id or task_id.
func ValidID(v string) bool { return idPattern.MatchString(v) }

// ValidMessageID reports whether v is a message_id.
func ValidMessageID(v string) bool { return messageIDPattern.MatchString(v) }

// CompareIDs compares two decimal IDs without leading zeros as numbers: it
// returns -1, 0 or 1. A shorter ID is smaller; IDs of the same length
// compare as strings.
func CompareIDs(a, b string) int {
	switch {
	case len(a) != len(b):
		if len(a) < len(b) {
			return -1
		}
		return 1
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// RateLimitError is an HTTP 429. RetryAt is when the request may be retried
// (x-ratelimit-reset), or zero when the header was missing or not within an
// hour from now.
type RateLimitError struct {
	Method  string
	RetryAt time.Time
}

func (e *RateLimitError) Error() string { return "chatwork: " + e.Method + ": rate limited" }

// Is makes errors.Is(err, ErrRateLimited) true.
func (e *RateLimitError) Is(target error) bool { return target == ErrRateLimited }

// APIError is a failure of one target (a room the user cannot read, or a
// response too large to read): HTTP 403 or 404, or TooLarge with Status 200.
// The rest of a synchronization can go on.
type APIError struct {
	Method   string
	Status   int
	TooLarge bool
}

func (e *APIError) Error() string {
	if e.TooLarge {
		return "chatwork: " + e.Method + ": response body is too large"
	}
	return fmt.Sprintf("chatwork: %s: status %d", e.Method, e.Status)
}

// StatusError is an unexpected HTTP status, including 5xx and redirects.
type StatusError struct {
	Method string
	Code   int
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("chatwork: %s: unexpected status %d", e.Method, e.Code)
}

// Client calls the Chatwork API. BaseURL and HTTP are replaced by tests.
type Client struct {
	BaseURL string
	HTTP    *http.Client
	Now     func() time.Time
}

// New returns a client of the Chatwork API that does not follow redirects,
// so that the token is never sent to another host.
func New(now func() time.Time) *Client {
	return &Client{BaseURL: DefaultBaseURL, Now: now, HTTP: &http.Client{
		Timeout: timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}}
}

// call sends a GET request for path with the query and returns the body, or
// nil for 204 No Content. method names the call in errors.
func (c *Client) call(ctx context.Context, token, method, path string, query url.Values) ([]byte, error) {
	rawURL := c.BaseURL + path
	if len(query) > 0 {
		rawURL += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("chatwork: %s: invalid request", method)
	}
	req.Header.Set("x-chatworktoken", token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "astovia")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		// *url.Error includes the URL; only the cause is kept.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, fmt.Errorf("chatwork: %s: %w", method, err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNoContent:
		return nil, nil
	case http.StatusUnauthorized:
		return nil, fmt.Errorf("%w (%s)", ErrUnauthorized, method)
	case http.StatusTooManyRequests:
		e := &RateLimitError{Method: method}
		if n, err := strconv.ParseInt(strings.TrimSpace(resp.Header.Get("x-ratelimit-reset")), 10, 64); err == nil {
			at, now := time.Unix(n, 0), c.Now()
			if at.After(now) && !at.After(now.Add(maxRetryWait)) {
				e.RetryAt = at
			}
		}
		return nil, e
	case http.StatusForbidden, http.StatusNotFound:
		return nil, &APIError{Method: method, Status: resp.StatusCode}
	default:
		return nil, &StatusError{Method: method, Code: resp.StatusCode}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, bodyLimit+1))
	if err != nil {
		return nil, fmt.Errorf("chatwork: %s: %w", method, err)
	}
	if len(body) > bodyLimit {
		return nil, &APIError{Method: method, Status: http.StatusOK, TooLarge: true}
	}
	return body, nil
}

// decode parses body into out; a body that does not fit is ErrUnexpected.
func decode(method string, body []byte, out interface{}) error {
	d := json.NewDecoder(bytes.NewReader(body))
	d.UseNumber()
	if err := d.Decode(out); err != nil {
		return fmt.Errorf("%w (%s)", ErrUnexpected, method)
	}
	return nil
}

// id returns the decimal ID in raw, a JSON number or string, when it
// matches pattern.
func id(raw json.RawMessage, pattern *regexp.Regexp) (string, bool) {
	var v interface{}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if len(raw) == 0 || d.Decode(&v) != nil {
		return "", false
	}
	var s string
	switch t := v.(type) {
	case json.Number:
		s = t.String()
	case string:
		s = t
	default:
		return "", false
	}
	if !pattern.MatchString(s) {
		return "", false
	}
	return s, true
}

// Account is the owner of a token from GET /me.
type Account struct {
	ID               string
	Name             string
	OrganizationName string
}

// Me returns the account of the token.
func (c *Client) Me(ctx context.Context, token string) (Account, error) {
	const method = "me"
	body, err := c.call(ctx, token, method, "/me", nil)
	if err != nil {
		return Account{}, err
	}
	var out struct {
		AccountID        json.RawMessage `json:"account_id"`
		Name             string          `json:"name"`
		OrganizationName string          `json:"organization_name"`
	}
	if err = decode(method, body, &out); err != nil {
		return Account{}, err
	}
	accountID, ok := id(out.AccountID, idPattern)
	if !ok {
		return Account{}, fmt.Errorf("%w (%s)", ErrUnexpected, method)
	}
	return Account{ID: accountID, Name: out.Name, OrganizationName: out.OrganizationName}, nil
}

// Room is a chat the user is a member of. Type is "my", "direct" or
// "group"; LastUpdate is the Unix time of its last update.
type Room struct {
	ID         string
	Name       string
	Type       string
	LastUpdate int64
}

// Rooms returns the user's rooms. Rooms with an invalid room_id are left
// out.
func (c *Client) Rooms(ctx context.Context, token string) ([]Room, error) {
	const method = "rooms"
	body, err := c.call(ctx, token, method, "/rooms", nil)
	if err != nil || body == nil {
		return nil, err
	}
	var out []struct {
		RoomID         json.RawMessage `json:"room_id"`
		Name           string          `json:"name"`
		Type           string          `json:"type"`
		LastUpdateTime int64           `json:"last_update_time"`
	}
	if err = decode(method, body, &out); err != nil {
		return nil, err
	}
	rooms := make([]Room, 0, len(out))
	for _, r := range out {
		roomID, ok := id(r.RoomID, idPattern)
		if !ok {
			continue
		}
		rooms = append(rooms, Room{ID: roomID, Name: r.Name, Type: r.Type, LastUpdate: r.LastUpdateTime})
	}
	return rooms, nil
}

// Message is a message of a room. AccountID is empty when the sender's
// account_id is invalid.
type Message struct {
	ID          string
	AccountID   string
	AccountName string
	Body        string
	SendTime    int64
}

// MessagePage is the latest messages of a room, oldest first as returned.
// Raw is the number of messages in the response before invalid ones were
// left out.
type MessagePage struct {
	Messages []Message
	Raw      int
}

// Messages returns the latest (up to 100) messages of the room. It always
// sends force=1, so that the result does not depend on what was read before
// with the same token.
func (c *Client) Messages(ctx context.Context, token, roomID string) (MessagePage, error) {
	const method = "messages"
	if !ValidID(roomID) {
		return MessagePage{}, fmt.Errorf("%w (%s: invalid room)", ErrUnexpected, method)
	}
	body, err := c.call(ctx, token, method, "/rooms/"+roomID+"/messages", url.Values{"force": {"1"}})
	if err != nil || body == nil {
		return MessagePage{}, err
	}
	var out []struct {
		MessageID json.RawMessage `json:"message_id"`
		Account   struct {
			AccountID json.RawMessage `json:"account_id"`
			Name      string          `json:"name"`
		} `json:"account"`
		Body     string `json:"body"`
		SendTime int64  `json:"send_time"`
	}
	if err = decode(method, body, &out); err != nil {
		return MessagePage{}, err
	}
	page := MessagePage{Messages: make([]Message, 0, len(out)), Raw: len(out)}
	for _, m := range out {
		messageID, ok := id(m.MessageID, messageIDPattern)
		if !ok {
			continue
		}
		accountID, _ := id(m.Account.AccountID, idPattern)
		page.Messages = append(page.Messages, Message{ID: messageID, AccountID: accountID, AccountName: m.Account.Name, Body: m.Body, SendTime: m.SendTime})
	}
	return page, nil
}

// Task is an open task assigned to the user. LimitType is "none", "date"
// or "time"; AssignedByID is empty when the account_id is invalid.
type Task struct {
	ID             string
	RoomID         string
	RoomName       string
	AssignedByID   string
	AssignedByName string
	MessageID      string
	Body           string
	LimitTime      int64
	LimitType      string
}

// TaskPage is the user's open tasks. Raw is the number of tasks in the
// response before invalid ones were left out; the response has at most
// 100.
type TaskPage struct {
	Tasks []Task
	Raw   int
}

// MyTasks returns the open tasks assigned to the user.
func (c *Client) MyTasks(ctx context.Context, token string) (TaskPage, error) {
	const method = "my/tasks"
	body, err := c.call(ctx, token, method, "/my/tasks", url.Values{"status": {"open"}})
	if err != nil || body == nil {
		return TaskPage{}, err
	}
	var out []struct {
		TaskID json.RawMessage `json:"task_id"`
		Room   struct {
			RoomID json.RawMessage `json:"room_id"`
			Name   string          `json:"name"`
		} `json:"room"`
		AssignedBy struct {
			AccountID json.RawMessage `json:"account_id"`
			Name      string          `json:"name"`
		} `json:"assigned_by_account"`
		MessageID json.RawMessage `json:"message_id"`
		Body      string          `json:"body"`
		LimitTime int64           `json:"limit_time"`
		LimitType string          `json:"limit_type"`
	}
	if err = decode(method, body, &out); err != nil {
		return TaskPage{}, err
	}
	page := TaskPage{Tasks: make([]Task, 0, len(out)), Raw: len(out)}
	for _, t := range out {
		taskID, ok1 := id(t.TaskID, idPattern)
		roomID, ok2 := id(t.Room.RoomID, idPattern)
		messageID, ok3 := id(t.MessageID, messageIDPattern)
		if !ok1 || !ok2 || !ok3 {
			continue
		}
		assignedBy, _ := id(t.AssignedBy.AccountID, idPattern)
		page.Tasks = append(page.Tasks, Task{ID: taskID, RoomID: roomID, RoomName: t.Room.Name, AssignedByID: assignedBy, AssignedByName: t.AssignedBy.Name,
			MessageID: messageID, Body: t.Body, LimitTime: t.LimitTime, LimitType: t.LimitType})
	}
	return page, nil
}
