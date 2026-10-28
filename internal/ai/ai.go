// Package ai calls the AI provider (Anthropic) with the user's own API key.
// Clients are built without any environment default: neither the API key
// nor the base URL can come from ANTHROPIC_* variables or profile files, so
// a request only ever carries the key it was given and goes to BaseURL.
package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// DefaultBaseURL is the Anthropic API.
const DefaultBaseURL = "https://api.anthropic.com/"

// ProviderAnthropic is the only provider for now.
const ProviderAnthropic = "anthropic"

const (
	// requestTimeout bounds one judgment and modelTimeout one model check.
	requestTimeout = 15 * time.Second
	modelTimeout   = 10 * time.Second
	maxTokens      = 4096
	// resultLimit is the longest task or summary kept, in characters.
	resultLimit = 200
)

// Models are the models a user can choose, the default first. The web UI
// has the same list.
var Models = []string{"claude-haiku-5-5", "claude-sonnet-5-5", "claude-opus-5-5"}

// ValidModel reports whether model is one of Models.
func ValidModel(model string) bool {
	for _, m := range Models {
		if m == model {
			return true
		}
	}
	return false
}

// KeyPurpose is the secretbox purpose of a user's API key.
func KeyPurpose(userID string) string { return "ai-key:" + userID }

// Error classes. Errors returned by Client wrap exactly one of them, except
// that the caller's own context error is returned as is.
var (
	// ErrKeyInvalid is a 401 or 403: the key is invalid or revoked.
	ErrKeyInvalid = errors.New("ai: key invalid")
	// ErrModelUnavailable is a 404: the key cannot use the model.
	ErrModelUnavailable = errors.New("ai: model unavailable")
	// ErrBilling is a 402, or a 400 about the credit balance.
	ErrBilling = errors.New("ai: billing")
	// ErrUnusable is a request the API rejected (other 4xx) or an answer
	// that cannot be used (refusal, max_tokens, invalid JSON, empty task).
	ErrUnusable = errors.New("ai: unusable")
	// ErrTemporary is a 408, 429, 5xx (529 included), a connection error or
	// this package's own timeout.
	ErrTemporary = errors.New("ai: temporary")
)

// Client calls the Anthropic API. BaseURL, HTTP and the timeouts are
// replaced by tests.
type Client struct {
	BaseURL        string
	HTTP           *http.Client
	RequestTimeout time.Duration
	ModelTimeout   time.Duration
}

// New returns a client of the Anthropic API that does not follow redirects.
func New() *Client {
	return &Client{BaseURL: DefaultBaseURL, RequestTimeout: requestTimeout, ModelTimeout: modelTimeout, HTTP: &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}}
}

// sdk builds an SDK client for one request. WithoutEnvironmentDefaults
// keeps anthropic.NewClient from reading ANTHROPIC_API_KEY,
// ANTHROPIC_AUTH_TOKEN, ANTHROPIC_BASE_URL, profiles and federation
// settings; only the options given here apply. The SDK does not retry.
func (c *Client) sdk(key string) anthropic.Client {
	return anthropic.NewClient(
		option.WithoutEnvironmentDefaults(),
		option.WithBaseURL(c.BaseURL),
		option.WithHTTPClient(c.HTTP),
		option.WithAPIKey(key),
		option.WithHeaderDel("Authorization"),
		option.WithMaxRetries(0),
	)
}

// classify maps an SDK error to one of the error classes. parent is the
// caller's context: when it is done, its error is returned so that the
// caller can stop. The result never includes response bodies or headers.
func classify(parent context.Context, err error) error {
	if e := parent.Err(); e != nil {
		return e
	}
	var api *anthropic.Error
	if !errors.As(err, &api) {
		// Connection errors, timeouts of this package and invalid
		// responses.
		return ErrTemporary
	}
	code := api.StatusCode
	var class error
	switch {
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		class = ErrKeyInvalid
	case code == http.StatusPaymentRequired:
		class = ErrBilling
	case code == http.StatusBadRequest && strings.Contains(strings.ToLower(errorMessage(api.RawJSON())), "credit balance"):
		class = ErrBilling
	case code == http.StatusNotFound:
		class = ErrModelUnavailable
	case code == http.StatusRequestTimeout || code == http.StatusTooManyRequests || code >= 500:
		class = ErrTemporary
	case code >= 400 && code < 500:
		class = ErrUnusable
	default:
		class = ErrTemporary
	}
	return fmt.Errorf("%w (status %d)", class, code)
}

// errorMessage returns error.message of an API error body.
func errorMessage(raw string) string {
	var body struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(raw), &body) != nil {
		return ""
	}
	return body.Error.Message
}

// CheckModel verifies that key can use model with the Models API, which
// does not use tokens.
func (c *Client) CheckModel(ctx context.Context, key, model string) error {
	rctx, cancel := context.WithTimeout(ctx, c.ModelTimeout)
	defer cancel()
	client := c.sdk(key)
	if _, err := client.Models.Get(rctx, model, anthropic.ModelGetParams{}); err != nil {
		return classify(ctx, err)
	}
	return nil
}

// Decisions of a judgment.
const (
	// DecisionTask is something the user clearly has to do.
	DecisionTask = "task"
	// DecisionCandidate is something the user may have to do: the AI is
	// not sure that it is addressed to the user.
	DecisionCandidate = "candidate"
	// DecisionNone is nothing to do.
	DecisionNone = "none"
)

// eventLayout is the format of event_start and event_end: a local time in
// the time zone of the input.
const eventLayout = "2006-01-02T15:04"

// MentionResult is the AI's judgment of a mention. Decision is one of the
// Decision constants; Task is empty when it is DecisionNone. Event is set
// when the posts fixed a meeting that the user attends.
type MentionResult struct {
	Decision string
	Task     string
	Summary  string
	Event    *Event
}

// Event is a meeting found by a judgment. End is zero when no end was given.
type Event struct {
	Title string
	Start time.Time
	End   time.Time
}

// outputSchema is the structured output of a judgment.
var outputSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"decision":    map[string]any{"type": "string", "enum": []string{DecisionTask, DecisionCandidate, DecisionNone}},
		"task":        map[string]any{"type": "string"},
		"summary":     map[string]any{"type": "string"},
		"event_title": map[string]any{"type": "string"},
		"event_start": map[string]any{"type": "string", "description": "YYYY-MM-DDTHH:MM in the local time of the input timezone, or an empty string"},
		"event_end":   map[string]any{"type": "string", "description": "YYYY-MM-DDTHH:MM in the local time of the input timezone, or an empty string"},
	},
	"required":             []string{"decision", "task", "summary", "event_title", "event_start", "event_end"},
	"additionalProperties": false,
}

// JudgeMention asks the AI whether the mentioned user has something to do.
func (c *Client) JudgeMention(ctx context.Context, key, model string, in MentionInput) (MentionResult, error) {
	user, err := UserMessage(in)
	if err != nil {
		return MentionResult{}, err
	}
	text, err := c.structured(ctx, key, model, SystemPrompt, user, outputSchema)
	if err != nil {
		return MentionResult{}, err
	}
	return parseResult(text, location(in.Location))
}

// structured sends one judgment with a fixed system prompt and a JSON
// schema for the answer, and returns the answer's text. Errors are
// classified like those of JudgeMention.
func (c *Client) structured(ctx context.Context, key, model, system, user string, schema map[string]any) (string, error) {
	rctx, cancel := context.WithTimeout(ctx, c.RequestTimeout)
	defer cancel()
	client := c.sdk(key)
	// TODO: use prompt caching once the system prompt is long enough to
	// reach the minimum cacheable length.
	msg, err := client.Messages.New(rctx, anthropic.MessageNewParams{
		Model:     anthropic.Model(model),
		MaxTokens: maxTokens,
		System:    []anthropic.TextBlockParam{{Text: system}},
		Messages:  []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(user))},
		OutputConfig: anthropic.OutputConfigParam{
			Effort: anthropic.OutputConfigEffortLow,
			Format: anthropic.JSONOutputFormatParam{Schema: schema},
		},
	})
	if err != nil {
		return "", classify(ctx, err)
	}
	if msg.StopReason == anthropic.StopReasonRefusal || msg.StopReason == anthropic.StopReasonMaxTokens {
		return "", fmt.Errorf("%w (stop reason %s)", ErrUnusable, msg.StopReason)
	}
	var text strings.Builder
	for _, b := range msg.Content {
		if b.Type == "text" {
			text.WriteString(b.Text)
		}
	}
	return text.String(), nil
}

// parseResult decodes and checks the structured output. Event times are
// read in loc. An event that cannot be used is dropped and the task kept.
func parseResult(text string, loc *time.Location) (MentionResult, error) {
	var out struct {
		Decision   *string `json:"decision"`
		Task       *string `json:"task"`
		Summary    *string `json:"summary"`
		EventTitle *string `json:"event_title"`
		EventStart *string `json:"event_start"`
		EventEnd   *string `json:"event_end"`
	}
	d := json.NewDecoder(strings.NewReader(text))
	d.DisallowUnknownFields()
	if err := d.Decode(&out); err != nil || out.Decision == nil || out.Task == nil || out.Summary == nil || out.EventTitle == nil || out.EventStart == nil || out.EventEnd == nil {
		return MentionResult{}, fmt.Errorf("%w (invalid output)", ErrUnusable)
	}
	r := MentionResult{Decision: *out.Decision, Task: truncate(clean(*out.Task), resultLimit), Summary: truncate(clean(*out.Summary), resultLimit)}
	switch r.Decision {
	case DecisionNone:
		r.Task = ""
		return r, nil
	case DecisionTask, DecisionCandidate:
		if r.Task == "" {
			return MentionResult{}, fmt.Errorf("%w (empty task)", ErrUnusable)
		}
	default:
		return MentionResult{}, fmt.Errorf("%w (invalid decision)", ErrUnusable)
	}
	r.Event = parseEvent(*out.EventTitle, *out.EventStart, *out.EventEnd, loc)
	return r, nil
}

// parseEvent returns the event of a judgment, or nil when there is none or
// its title or start cannot be used. An end that cannot be read or is not
// after the start is left zero.
func parseEvent(title, start, end string, loc *time.Location) *Event {
	title = truncate(clean(title), resultLimit)
	start, end = strings.TrimSpace(start), strings.TrimSpace(end)
	if title == "" || start == "" {
		return nil
	}
	at, err := time.ParseInLocation(eventLayout, start, loc)
	if err != nil {
		return nil
	}
	e := &Event{Title: title, Start: at}
	if end != "" {
		if to, err := time.ParseInLocation(eventLayout, end, loc); err == nil && to.After(at) {
			e.End = to
		}
	}
	return e
}

// clean removes control and format characters and surrounding spaces, like
// the titles of GitHub items.
func clean(v string) string {
	v = strings.Map(func(r rune) rune {
		if unicode.In(r, unicode.Cc, unicode.Cf, unicode.Zl, unicode.Zp) {
			return -1
		}
		return r
	}, v)
	return strings.TrimFunc(v, unicode.IsSpace)
}

func truncate(v string, n int) string {
	if utf8.RuneCountInString(v) <= n {
		return v
	}
	return string([]rune(v)[:n])
}
