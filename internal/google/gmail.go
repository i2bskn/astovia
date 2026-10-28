package google

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// idPattern is a Gmail message or thread ID.
var idPattern = regexp.MustCompile(`^[0-9a-f]{1,32}$`)

// ValidID reports whether v is a Gmail message or thread ID: up to 32
// lower-case hexadecimal digits.
func ValidID(v string) bool { return idPattern.MatchString(v) }

// rateReasons are the reasons of a 403 that is a rate limit.
var rateReasons = map[string]bool{"rateLimitExceeded": true, "userRateLimitExceeded": true, "RATE_LIMIT_EXCEEDED": true}

// get sends a GET request for path with the query and the access token,
// and returns the body of a 200 response. Other statuses are mapped to the
// errors of the package.
func (c *Client) get(ctx context.Context, accessToken, method, path string, query url.Values) ([]byte, error) {
	rawURL := c.GmailBaseURL + path
	if len(query) > 0 {
		rawURL += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("google: %s: invalid request", method)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "astovia")
	status, body, header, err := c.send(method, req, gmailBodyLimit)
	if err != nil {
		return nil, err
	}
	switch status {
	case http.StatusOK:
		return body, nil
	case http.StatusUnauthorized:
		return nil, fmt.Errorf("%w (%s)", ErrUnauthorized, method)
	case http.StatusTooManyRequests:
		return nil, &RateLimitError{Method: method, RetryAt: c.retryAt(header)}
	case http.StatusForbidden:
		reasons := errorReasons(body)
		switch {
		case reasons["insufficientPermissions"] || reasons["ACCESS_TOKEN_SCOPE_INSUFFICIENT"]:
			return nil, fmt.Errorf("%w (%s)", ErrInsufficientScope, method)
		case reasons["accessNotConfigured"] || reasons["SERVICE_DISABLED"]:
			return nil, fmt.Errorf("%w (%s)", ErrAPIDisabled, method)
		case reasons["domainPolicy"]:
			return nil, fmt.Errorf("%w (%s)", ErrPolicyBlocked, method)
		}
		for r := range reasons {
			if rateReasons[r] {
				return nil, &RateLimitError{Method: method, RetryAt: c.retryAt(header)}
			}
		}
		return nil, &StatusError{Method: method, Code: status}
	case http.StatusNotFound, http.StatusBadRequest:
		return nil, &APIError{Method: method, Status: status}
	}
	return nil, &StatusError{Method: method, Code: status}
}

// errorReasons returns the reasons of a Google API error body: the reasons
// of error.errors[], the reasons of error.details[] and error.status.
func errorReasons(body []byte) map[string]bool {
	var out struct {
		Error struct {
			Status string `json:"status"`
			Errors []struct {
				Reason string `json:"reason"`
			} `json:"errors"`
			Details []struct {
				Reason string `json:"reason"`
			} `json:"details"`
		} `json:"error"`
	}
	reasons := map[string]bool{}
	if json.Unmarshal(body, &out) != nil {
		return reasons
	}
	if out.Error.Status != "" {
		reasons[out.Error.Status] = true
	}
	for _, e := range out.Error.Errors {
		reasons[e.Reason] = true
	}
	for _, d := range out.Error.Details {
		reasons[d.Reason] = true
	}
	return reasons
}

// Profile returns the address of the Google account of the access token.
func (c *Client) Profile(ctx context.Context, accessToken string) (string, error) {
	const method = "profile"
	body, err := c.get(ctx, accessToken, method, "/profile", nil)
	if err != nil {
		return "", err
	}
	var out struct {
		EmailAddress string `json:"emailAddress"`
	}
	if err = decode(method, body, &out); err != nil {
		return "", err
	}
	if out.EmailAddress == "" {
		return "", fmt.Errorf("%w (%s)", ErrUnexpected, method)
	}
	return out.EmailAddress, nil
}

// MessageRef is a message of a list: its ID and its thread's.
type MessageRef struct {
	ID       string
	ThreadID string
}

// MessageList is one page of messages.list, newest first. Messages with an
// invalid ID are left out. NextPageToken is empty on the last page.
type MessageList struct {
	Messages      []MessageRef
	NextPageToken string
}

// ListMessages returns one page (up to 100) of the messages matching the
// Gmail search query.
func (c *Client) ListMessages(ctx context.Context, accessToken, query, pageToken string) (MessageList, error) {
	const method = "messages.list"
	q := url.Values{"q": {query}, "maxResults": {"100"}}
	if pageToken != "" {
		q.Set("pageToken", pageToken)
	}
	body, err := c.get(ctx, accessToken, method, "/messages", q)
	if err != nil {
		return MessageList{}, err
	}
	var out struct {
		Messages []struct {
			ID       string `json:"id"`
			ThreadID string `json:"threadId"`
		} `json:"messages"`
		NextPageToken string `json:"nextPageToken"`
	}
	if err = decode(method, body, &out); err != nil {
		return MessageList{}, err
	}
	list := MessageList{Messages: make([]MessageRef, 0, len(out.Messages)), NextPageToken: out.NextPageToken}
	for _, m := range out.Messages {
		if ValidID(m.ID) && ValidID(m.ThreadID) {
			list.Messages = append(list.Messages, MessageRef{ID: m.ID, ThreadID: m.ThreadID})
		}
	}
	return list, nil
}

// Address is a parsed address of a header. Name is the display name with
// its RFC 2047 encoding decoded; Email is lower case.
type Address struct {
	Name  string
	Email string
}

// Message is a message of a thread as the integration uses it. From, To and
// Cc are the raw header values. FromName and FromEmail are the parsed
// sender: when From cannot be parsed, FromName is the raw value (without
// control characters, at most 200 characters) and FromEmail is empty.
// ToList and CcList are the parsed recipients; ToParsed and CcParsed are
// false when the header could not be parsed. Subject is decoded. Text is
// the body read for the AI (see bodyText).
type Message struct {
	ID              string
	ThreadID        string
	Labels          []string
	Internal        time.Time
	From            string
	To              string
	Cc              string
	Subject         string
	Date            string
	ListUnsubscribe string
	Precedence      string
	AutoSubmitted   string
	// HasAutoSubmitted tells whether the message had an Auto-Submitted
	// header at all.
	HasAutoSubmitted bool
	Text             string
	FromName         string
	FromEmail        string
	ToList           []Address
	CcList           []Address
	ToParsed         bool
	CcParsed         bool
}

// HasLabel reports whether the message has the label.
func (m Message) HasLabel(label string) bool {
	for _, l := range m.Labels {
		if l == label {
			return true
		}
	}
	return false
}

// Thread is a thread with its messages in the order Gmail returned them.
// Messages with an invalid ID or date are left out.
type Thread struct {
	ID       string
	Messages []Message
}

// part is a MessagePart of the Gmail API.
type part struct {
	MimeType string `json:"mimeType"`
	Filename string `json:"filename"`
	Headers  []struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	} `json:"headers"`
	Body struct {
		AttachmentID string `json:"attachmentId"`
		Data         string `json:"data"`
		Size         int64  `json:"size"`
	} `json:"body"`
	Parts []part `json:"parts"`
}

// header returns the first value of the header name (case-insensitive) and
// whether it was present.
func (p part) header(name string) (string, bool) {
	for _, h := range p.Headers {
		if strings.EqualFold(h.Name, name) {
			return h.Value, true
		}
	}
	return "", false
}

// Thread returns a thread with the full messages. An invalid thread ID is
// not requested.
func (c *Client) Thread(ctx context.Context, accessToken, threadID string) (Thread, error) {
	const method = "threads.get"
	if !ValidID(threadID) {
		return Thread{}, &APIError{Method: method, Status: http.StatusBadRequest}
	}
	body, err := c.get(ctx, accessToken, method, "/threads/"+threadID, url.Values{"format": {"full"}})
	if err != nil {
		return Thread{}, err
	}
	var out struct {
		ID       string `json:"id"`
		Messages []struct {
			ID           string   `json:"id"`
			ThreadID     string   `json:"threadId"`
			LabelIDs     []string `json:"labelIds"`
			InternalDate string   `json:"internalDate"`
			Payload      part     `json:"payload"`
		} `json:"messages"`
	}
	if err = decode(method, body, &out); err != nil {
		return Thread{}, err
	}
	t := Thread{ID: threadID, Messages: make([]Message, 0, len(out.Messages))}
	for _, m := range out.Messages {
		ms, err := strconv.ParseInt(m.InternalDate, 10, 64)
		if !ValidID(m.ID) || err != nil || ms < 0 {
			continue
		}
		msg := Message{ID: m.ID, ThreadID: threadID, Labels: m.LabelIDs, Internal: time.UnixMilli(ms)}
		p := m.Payload
		msg.From, _ = p.header("From")
		msg.To, _ = p.header("To")
		msg.Cc, _ = p.header("Cc")
		subject, _ := p.header("Subject")
		msg.Subject = decodeHeader(subject)
		msg.Date, _ = p.header("Date")
		msg.ListUnsubscribe, _ = p.header("List-Unsubscribe")
		msg.Precedence, _ = p.header("Precedence")
		msg.AutoSubmitted, msg.HasAutoSubmitted = p.header("Auto-Submitted")
		msg.FromName, msg.FromEmail = parseFrom(msg.From)
		msg.ToList, msg.ToParsed = parseAddresses(msg.To)
		msg.CcList, msg.CcParsed = parseAddresses(msg.Cc)
		msg.Text = bodyText(p)
		t.Messages = append(t.Messages, msg)
	}
	return t, nil
}
