// Package mail sends notification mail through the service the operator
// configured: Cloudflare Email Service or Resend. Errors carry only the
// status code and a safe code from the service; never the API key, the
// recipient, the content or the response body.
package mail

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/i2bskn/astovia/internal/config"
)

const sendTimeout = 10 * time.Second

// Kinds of send errors. The dispatcher retries all of them; they are told
// apart for the log and for rate limiting.
var (
	// ErrRejected is an authentication, permission, unverified domain or
	// other 4xx error.
	ErrRejected = errors.New("mail: rejected")
	// ErrRateLimited is a 429, including the daily and monthly quotas of
	// Resend.
	ErrRateLimited = errors.New("mail: rate limited")
	// ErrTemporary is a 5xx, a connection error or a concurrent request
	// with the same idempotency key (Resend).
	ErrTemporary = errors.New("mail: temporary failure")
)

// Message is one notification mail to one recipient. IdempotencyKey is unique per notification delivery.
type Message struct{ To, Subject, Text, IdempotencyKey string }

// Sender sends notification mail.
type Sender interface {
	Provider() string
	Send(ctx context.Context, m Message) error
}

// New returns the sender of c, or nil when c is nil.
func New(c *config.MailConfig) Sender {
	if c == nil {
		return nil
	}
	if c.Provider == config.MailResend {
		return &resendSender{BaseURL: "https://api.resend.com", HTTP: newClient(), APIKey: c.APIKey, From: c.From, FromName: c.FromName}
	}
	return &cloudflareSender{BaseURL: "https://api.cloudflare.com", HTTP: newClient(), APIKey: c.APIKey, AccountID: c.AccountID, From: c.From}
}

// newClient returns a client that does not follow redirects.
func newClient() *http.Client {
	return &http.Client{
		Timeout: sendTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

type payload struct {
	From    string   `json:"from"`
	To      []string `json:"to"`
	Subject string   `json:"subject"`
	Text    string   `json:"text"`
}

// safeCode matches the error codes of Resend that may be put in an error.
var safeCode = regexp.MustCompile(`^[A-Za-z0-9_]{1,64}$`)

// post sends body as JSON and returns the status and up to 64 KiB of the
// response. The error of a failed request is not wrapped, since it can
// contain the URL.
func post(ctx context.Context, client *http.Client, url, key string, header map[string]string, body any) (int, []byte, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return 0, nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return 0, nil, fmt.Errorf("%w: invalid request", ErrTemporary)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("%w: request failed", ErrTemporary)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return resp.StatusCode, data, nil
}

// classify returns the kind of a failed status.
func classify(status int) error {
	switch {
	case status == http.StatusTooManyRequests:
		return ErrRateLimited
	case status >= 500:
		return ErrTemporary
	}
	return ErrRejected
}

// cloudflareSender sends through the REST API of Cloudflare Email Service.
// It has no idempotency key, so a retried send can deliver twice. The
// sender's display name is not sent.
type cloudflareSender struct {
	BaseURL   string
	HTTP      *http.Client
	APIKey    string
	AccountID string
	From      string
}

func (c *cloudflareSender) Provider() string { return config.MailCloudflare }

func (c *cloudflareSender) Send(ctx context.Context, m Message) error {
	status, data, err := post(ctx, c.HTTP, c.BaseURL+"/client/v4/accounts/"+c.AccountID+"/email/sending/send", c.APIKey, nil, payload{From: c.From, To: []string{m.To}, Subject: m.Subject, Text: m.Text})
	if err != nil {
		return err
	}
	var res struct {
		Success bool `json:"success"`
		Errors  []struct {
			Code json.Number `json:"code"`
		} `json:"errors"`
	}
	decodeErr := json.Unmarshal(data, &res)
	if status == http.StatusOK && decodeErr == nil && res.Success {
		return nil
	}
	code := ""
	if decodeErr == nil && len(res.Errors) > 0 {
		if n, e := strconv.ParseInt(res.Errors[0].Code.String(), 10, 64); e == nil {
			code = " code " + strconv.FormatInt(n, 10)
		}
	}
	kind := ErrRejected
	if status != http.StatusOK {
		kind = classify(status)
	}
	return fmt.Errorf("%w: cloudflare status %d%s", kind, status, code)
}

// resendSender sends through the Resend API.
type resendSender struct {
	BaseURL  string
	HTTP     *http.Client
	APIKey   string
	From     string
	FromName string
}

func (r *resendSender) Provider() string { return config.MailResend }

func (r *resendSender) Send(ctx context.Context, m Message) error {
	from := r.From
	if r.FromName != "" {
		from = r.FromName + " <" + r.From + ">"
	}
	header := map[string]string{"User-Agent": "astovia"}
	if m.IdempotencyKey != "" {
		header["Idempotency-Key"] = m.IdempotencyKey
	}
	status, data, err := post(ctx, r.HTTP, r.BaseURL+"/emails", r.APIKey, header, payload{From: from, To: []string{m.To}, Subject: m.Subject, Text: m.Text})
	if err != nil {
		return err
	}
	if status >= 200 && status < 300 {
		return nil
	}
	var res struct {
		Name string `json:"name"`
	}
	code := ""
	if json.Unmarshal(data, &res) == nil && safeCode.MatchString(res.Name) {
		code = " " + res.Name
	}
	kind := classify(status)
	if status == http.StatusConflict && res.Name == "concurrent_idempotent_requests" {
		kind = ErrTemporary
	}
	return fmt.Errorf("%w: resend status %d%s", kind, status, code)
}
