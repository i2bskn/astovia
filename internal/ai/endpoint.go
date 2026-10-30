package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/i2bskn/astovia/internal/oauth"
)

// OpenAIAPIBaseURL is the OpenAI API used with an OpenAI API key.
const OpenAIAPIBaseURL = "https://api.openai.com/v1"

const (
	// endpointTimeout bounds one judgment with an OpenAI API key or an
	// OpenAI-compatible endpoint, the retry without the reasoning effort
	// included. It also bounds the wait for the response headers.
	endpointTimeout = 20 * time.Second
	// endpointDialTimeout bounds a connection and endpointTLSTimeout its
	// TLS handshake.
	endpointDialTimeout = 10 * time.Second
	endpointTLSTimeout  = 10 * time.Second
	// endpointBodyLimit is the largest response body read.
	endpointBodyLimit = 1 << 20
	// endpointURLLimit is the longest endpoint URL accepted, in bytes.
	endpointURLLimit = 2048
	// endpointModelLimit is the longest model name accepted, in bytes.
	endpointModelLimit = 256
	// endpointEffort is the reasoning effort sent to the OpenAI API.
	endpointEffort = "low"
)

// ErrEndpointBlocked is an endpoint whose URL or address may not be used:
// a non-public address, or http, when they are not allowed. The errors of
// Client wrap it with ErrKeyInvalid, so that a synchronization records the
// settings as invalid instead of retrying.
var ErrEndpointBlocked = errors.New("ai: endpoint address not allowed")

var (
	// errBlockedAddress is returned by the dialer for an address that may
	// not be used.
	errBlockedAddress = errors.New("destination address is not allowed")
	// errEndpointURL is an endpoint URL that is not well formed.
	errEndpointURL = errors.New("ai: invalid endpoint URL")
)

// alwaysDeniedIPv4 and alwaysDeniedIPv6 are refused even when non-public
// addresses are allowed: the "this network" range and the known metadata
// services of cloud providers (AWS IPv6 and Alibaba Cloud; 169.254.169.254
// is link-local).
var (
	alwaysDeniedIPv4 = []netip.Prefix{
		netip.MustParsePrefix("0.0.0.0/8"),
		netip.MustParsePrefix("100.100.100.200/32"),
	}
	alwaysDeniedIPv6 = []netip.Prefix{
		netip.MustParsePrefix("fd00:ec2::254/128"),
	}
)

// endpointAddrAllowed reports whether an endpoint connection may go to a.
// Link-local, unspecified and multicast addresses and the known metadata
// services are always refused; the other addresses are allowed when
// allowPrivate is set, and otherwise only public ones
// (oauth.AllowedCIMDAddr). IPv4-mapped IPv6 addresses are checked as IPv4.
func endpointAddrAllowed(a netip.Addr, allowPrivate bool) bool {
	a = a.Unmap().WithZone("")
	if !a.IsValid() || a.IsLinkLocalUnicast() || a.IsUnspecified() || a.IsMulticast() {
		return false
	}
	denied := alwaysDeniedIPv6
	if a.Is4() {
		denied = alwaysDeniedIPv4
	}
	for _, p := range denied {
		if p.Contains(a) {
			return false
		}
	}
	if allowPrivate {
		return true
	}
	return oauth.AllowedCIMDAddr(a)
}

// NewEndpointClient returns the HTTP client of the OpenAI API and the
// OpenAI-compatible endpoints. Like the client of the metadata documents, it
// checks the address of every connection after name resolution, ignores the
// proxy environment variables and does not follow redirects. With
// allowPrivate, non-public addresses are reached as well (except the ones
// endpointAddrAllowed always refuses). Requests are bounded by their
// context; the client is meant to be shared.
func NewEndpointClient(allowPrivate bool) *http.Client {
	dialer := &net.Dialer{Timeout: endpointDialTimeout, Control: func(_, address string, _ syscall.RawConn) error {
		ap, err := netip.ParseAddrPort(address)
		if err != nil || !endpointAddrAllowed(ap.Addr(), allowPrivate) {
			return errBlockedAddress
		}
		return nil
	}}
	return &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Transport: &http.Transport{
			Proxy:                 nil,
			DialContext:           dialer.DialContext,
			TLSHandshakeTimeout:   endpointTLSTimeout,
			ResponseHeaderTimeout: endpointTimeout,
		},
	}
}

// ValidEndpointModel accepts a model name of 1-256 bytes of printable
// ASCII without spaces, such as meta-llama/Llama-3.1-8B-Instruct.
func ValidEndpointModel(model string) bool {
	if len(model) < 1 || len(model) > endpointModelLimit {
		return false
	}
	for i := 0; i < len(model); i++ {
		if model[i] < 0x21 || model[i] > 0x7e {
			return false
		}
	}
	return true
}

// NormalizeEndpointURL checks the URL of an OpenAI-compatible endpoint and
// returns it without surrounding spaces and trailing slashes. It must be an
// absolute https URL (http as well with allowPrivate) of at most 2048 bytes
// of printable ASCII, with a host and without credentials, query or
// fragment; the path is free. http without allowPrivate is an error
// wrapping ErrEndpointBlocked. The address of the host is checked when
// connecting, not here.
func NormalizeEndpointURL(raw string, allowPrivate bool) (string, error) {
	v := strings.TrimSpace(raw)
	if len(v) < 1 || len(v) > endpointURLLimit {
		return "", errEndpointURL
	}
	for i := 0; i < len(v); i++ {
		if v[i] < 0x21 || v[i] > 0x7e {
			return "", errEndpointURL
		}
	}
	u, err := url.Parse(v)
	if err != nil || !u.IsAbs() || u.Opaque != "" || u.Host == "" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(v, "#") {
		return "", errEndpointURL
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !allowPrivate {
			return "", fmt.Errorf("%w (http)", ErrEndpointBlocked)
		}
	default:
		return "", errEndpointURL
	}
	return strings.TrimRight(v, "/"), nil
}

// endpointClient returns the client of the endpoints, or a new one for a
// Client built without it.
func (c *Client) endpointClient() *http.Client {
	if c.Endpoints == nil {
		return NewEndpointClient(c.AllowPrivateEndpoints)
	}
	return c.Endpoints
}

// endpointBase returns the base URL of cred. The URL of an
// OpenAI-compatible endpoint is checked again, so that a URL saved while
// http or non-public addresses were allowed is not used after they no
// longer are.
func (c *Client) endpointBase(cred Credential) (string, error) {
	if cred.BaseURL == "" {
		if cred.Provider != ProviderOpenAIAPI {
			return "", fmt.Errorf("%w (%w)", ErrKeyInvalid, ErrEndpointBlocked)
		}
		if c.OpenAIAPIBaseURL != "" {
			return c.OpenAIAPIBaseURL, nil
		}
		return OpenAIAPIBaseURL, nil
	}
	base, err := NormalizeEndpointURL(cred.BaseURL, c.AllowPrivateEndpoints)
	if err != nil {
		return "", fmt.Errorf("%w (%w)", ErrKeyInvalid, ErrEndpointBlocked)
	}
	return base, nil
}

// structuredEndpoint is structured with an OpenAI API key or an
// OpenAI-compatible endpoint, through the Chat Completions API.
// EndpointTimeout bounds it.
func (c *Client) structuredEndpoint(ctx context.Context, cred Credential, model, system, user, schemaName string, schema map[string]any) (string, error) {
	timeout := c.EndpointTimeout
	if timeout <= 0 {
		timeout = endpointTimeout
	}
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return c.chat(ctx, rctx, cred, model, system, user, schemaName, schema)
}

// CheckEndpoint verifies with one small request that the endpoint of cred
// accepts its key, model and a JSON schema for the answer. ctx bounds it
// (EndpointTimeout does not apply). An answer other than {"ok": true} is
// ErrUnusable.
func (c *Client) CheckEndpoint(ctx context.Context, cred Credential, model string) error {
	text, err := c.chat(ctx, ctx, cred, model, `Reply with {"ok": true}.`, "ok?", checkSchemaName, checkSchema)
	if err != nil {
		return err
	}
	var out struct {
		OK *bool `json:"ok"`
	}
	d := json.NewDecoder(strings.NewReader(text))
	d.DisallowUnknownFields()
	if err := d.Decode(&out); err != nil || out.OK == nil || !*out.OK {
		return fmt.Errorf("%w (invalid output)", ErrUnusable)
	}
	return nil
}

// chatRequest is the body of a Chat Completions request. Nothing else is
// sent: parameters such as temperature or max_tokens are refused by some
// models and endpoints.
type chatRequest struct {
	Model           string             `json:"model"`
	Messages        []chatMessage      `json:"messages"`
	ResponseFormat  chatResponseFormat `json:"response_format"`
	ReasoningEffort string             `json:"reasoning_effort,omitempty"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatResponseFormat struct {
	Type       string         `json:"type"`
	JSONSchema chatJSONSchema `json:"json_schema"`
}

type chatJSONSchema struct {
	Name   string         `json:"name"`
	Schema map[string]any `json:"schema"`
	Strict bool           `json:"strict"`
}

// chat sends one Chat Completions request and returns the answer's text.
// The OpenAI API gets a low reasoning effort; when the model does not take
// one, the request is sent once more without it. parent is the caller's
// context and ctx the one bounding the requests. Errors are classified:
// when parent is done its error is returned.
func (c *Client) chat(parent, ctx context.Context, cred Credential, model, system, user, schemaName string, schema map[string]any) (string, error) {
	base, err := c.endpointBase(cred)
	if err != nil {
		return "", err
	}
	req := chatRequest{
		Model:          model,
		Messages:       []chatMessage{{Role: "system", Content: system}, {Role: "user", Content: user}},
		ResponseFormat: chatResponseFormat{Type: "json_schema", JSONSchema: chatJSONSchema{Name: schemaName, Schema: schema, Strict: true}},
	}
	if cred.Provider == ProviderOpenAIAPI {
		req.ReasoningEffort = endpointEffort
	}
	text, err := c.chatOnce(parent, ctx, base, cred.Key, req)
	var e *endpointError
	if req.ReasoningEffort != "" && errors.As(err, &e) && e.status == http.StatusBadRequest && e.param == "reasoning_effort" {
		req.ReasoningEffort = ""
		text, err = c.chatOnce(parent, ctx, base, cred.Key, req)
	}
	return text, err
}

// endpointError is a refusal of an endpoint: the status and the error code,
// type and parameter, reduced to safe characters. It wraps the error class.
type endpointError struct {
	class                error
	status               int
	code, errType, param string
}

func (e *endpointError) Error() string {
	return fmt.Sprintf("%v (status %d, code %q, type %q, param %q)", e.class, e.status, e.code, e.errType, e.param)
}

func (e *endpointError) Unwrap() error { return e.class }

// chatOnce sends one request. The errors never include the URL, the key,
// headers or the response body.
func (c *Client) chatOnce(parent, ctx context.Context, base, key string, body chatRequest) (string, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("%w (request)", ErrUnusable)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/chat/completions", bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("%w (%w)", ErrKeyInvalid, ErrEndpointBlocked)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "astovia")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := c.endpointClient().Do(req)
	if err != nil {
		if e := parent.Err(); e != nil {
			return "", e
		}
		// A *url.Error includes the URL, so only the class is kept.
		if errors.Is(err, errBlockedAddress) {
			return "", fmt.Errorf("%w (%w)", ErrKeyInvalid, ErrEndpointBlocked)
		}
		return "", fmt.Errorf("%w (connection)", ErrTemporary)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, endpointBodyLimit+1))
	if err != nil {
		if e := parent.Err(); e != nil {
			return "", e
		}
		return "", fmt.Errorf("%w (read)", ErrTemporary)
	}
	if resp.StatusCode != http.StatusOK {
		return "", readEndpointError(resp.StatusCode, raw)
	}
	if len(raw) > endpointBodyLimit {
		return "", fmt.Errorf("%w (response too large)", ErrUnusable)
	}
	return readChatAnswer(raw)
}

// readChatAnswer returns the content of the first choice of a 200 response.
func readChatAnswer(raw []byte) (string, error) {
	var out struct {
		Choices []struct {
			FinishReason *string `json:"finish_reason"`
			Message      struct {
				Content *string `json:"content"`
				Refusal *string `json:"refusal"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(raw, &out) != nil {
		return "", fmt.Errorf("%w (invalid response)", ErrUnusable)
	}
	if len(out.Choices) == 0 {
		return "", fmt.Errorf("%w (no choice)", ErrUnusable)
	}
	choice := out.Choices[0]
	if choice.Message.Refusal != nil && *choice.Message.Refusal != "" {
		return "", fmt.Errorf("%w (refusal)", ErrUnusable)
	}
	if choice.FinishReason != nil && (*choice.FinishReason == "length" || *choice.FinishReason == "content_filter") {
		return "", fmt.Errorf("%w (finish reason %s)", ErrUnusable, *choice.FinishReason)
	}
	if choice.Message.Content == nil || *choice.Message.Content == "" {
		return "", fmt.Errorf("%w (empty content)", ErrUnusable)
	}
	return *choice.Message.Content, nil
}

// readEndpointError classifies a response that is not 200. Only the string
// error.code, error.type and error.param are read, and only when they are
// plain names; a body of another shape is classified by the status alone.
func readEndpointError(status int, raw []byte) error {
	e := &endpointError{status: status}
	var out struct {
		Error json.RawMessage `json:"error"`
	}
	if len(raw) <= endpointBodyLimit && json.Unmarshal(raw, &out) == nil && out.Error != nil {
		var obj map[string]json.RawMessage
		if json.Unmarshal(out.Error, &obj) == nil {
			e.code, e.errType, e.param = safeName(obj["code"]), safeName(obj["type"]), safeName(obj["param"])
		}
	}
	e.class = endpointErrorClass(e.status, e.code, e.errType)
	return e
}

// endpointErrorClass is the class of a response that is not 200.
func endpointErrorClass(status int, code, errType string) error {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return ErrKeyInvalid
	case status == http.StatusPaymentRequired:
		return ErrBilling
	case status == http.StatusTooManyRequests && (code == "insufficient_quota" || errType == "insufficient_quota"):
		return ErrBilling
	case status == http.StatusTooManyRequests:
		return ErrTemporary
	case status == http.StatusNotFound || code == "model_not_found":
		return ErrModelUnavailable
	case status == http.StatusRequestTimeout || status >= 500:
		return ErrTemporary
	case status >= 300 && status < 500:
		// Redirects are not followed.
		return ErrUnusable
	}
	return ErrTemporary
}

// safeNamePattern keeps an error code, type or parameter name.
var safeNamePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}$`)

// safeName returns raw when it is a JSON string of a plain name, or "".
func safeName(raw json.RawMessage) string {
	var v string
	if raw == nil || json.Unmarshal(raw, &v) != nil || !safeNamePattern.MatchString(v) {
		return ""
	}
	return v
}
