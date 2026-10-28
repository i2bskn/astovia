// Package google is a small client of Google's OAuth 2.0 endpoints and the
// Gmail API for the mail integration. It reads only: the Gmail API is
// called with the gmail.readonly scope, and nothing is ever written to
// Gmail. Tokens are sent to the Google endpoints of the client (AuthURL,
// TokenURL, RevokeURL and GmailURL) and nowhere else; redirects are not
// followed and URLs found in responses are never requested. Errors carry
// the method and the HTTP status, never the request URL, the response body,
// a token, an authorization code or an address.
package google

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Default endpoints.
const (
	DefaultAuthURL   = "https://accounts.google.com/o/oauth2/v2/auth"
	DefaultTokenURL  = "https://oauth2.googleapis.com/token"
	DefaultRevokeURL = "https://oauth2.googleapis.com/revoke"
	DefaultGmailURL  = "https://gmail.googleapis.com/gmail/v1/users/me"
)

// ScopeGmailReadonly is the only scope the integration asks for.
const ScopeGmailReadonly = "https://www.googleapis.com/auth/gmail.readonly"

const (
	timeout = 10 * time.Second
	// tokenBodyLimit bounds the responses of the OAuth endpoints and
	// gmailBodyLimit those of the Gmail API.
	tokenBodyLimit = 1 << 20
	gmailBodyLimit = 10 << 20
	// defaultExpiry is the lifetime assumed for an access token whose
	// expires_in is missing or not positive.
	defaultExpiry = 3000 * time.Second
	// maxRetryWait bounds the Retry-After that is trusted.
	maxRetryWait = time.Hour
)

var (
	// ErrInvalidGrant is a refresh token or authorization code that is
	// invalid, expired or revoked (token endpoint error invalid_grant).
	ErrInvalidGrant = errors.New("google: invalid grant")
	// ErrInvalidClient is a client ID or secret that Google refuses
	// (invalid_client or unauthorized_client).
	ErrInvalidClient = errors.New("google: invalid client")
	// ErrPolicyBlocked is an access blocked by a Google Workspace
	// administrator (admin_policy_enforced, or the Gmail API reason
	// domainPolicy).
	ErrPolicyBlocked = errors.New("google: blocked by policy")
	// ErrNoRefreshToken is a code exchange that returned no refresh token.
	ErrNoRefreshToken = errors.New("google: no refresh token")
	// ErrUnexpected is a response whose shape is not the documented one.
	ErrUnexpected = errors.New("google: unexpected response")
	// ErrUnauthorized is an access token the Gmail API refuses (HTTP 401).
	ErrUnauthorized = errors.New("google: unauthorized")
	// ErrInsufficientScope is a token without the scope the method needs.
	ErrInsufficientScope = errors.New("google: insufficient scope")
	// ErrAPIDisabled is a Gmail API that is not enabled in the Google Cloud
	// project of the client.
	ErrAPIDisabled = errors.New("google: api disabled")
	// ErrRateLimited matches every *RateLimitError with errors.Is.
	ErrRateLimited = errors.New("google: rate limited")
)

// RateLimitError is an HTTP 429, or a 403 with a rate limit reason. RetryAt
// is when the request may be retried (Retry-After), or zero when the header
// was missing or not within an hour from now.
type RateLimitError struct {
	Method  string
	RetryAt time.Time
}

func (e *RateLimitError) Error() string { return "google: " + e.Method + ": rate limited" }

// Is makes errors.Is(err, ErrRateLimited) true.
func (e *RateLimitError) Is(target error) bool { return target == ErrRateLimited }

// APIError is a failure of one target (a thread that does not exist, a
// request Gmail refuses for that target, or a response too large to read):
// HTTP 404 or 400, or TooLarge with Status 200. The rest of a
// synchronization can go on.
type APIError struct {
	Method   string
	Status   int
	TooLarge bool
}

func (e *APIError) Error() string {
	if e.TooLarge {
		return "google: " + e.Method + ": response body is too large"
	}
	return fmt.Sprintf("google: %s: status %d", e.Method, e.Status)
}

// StatusError is an unexpected HTTP status, including 5xx and redirects.
type StatusError struct {
	Method string
	Code   int
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("google: %s: unexpected status %d", e.Method, e.Code)
}

// Client calls Google. The URLs and HTTP are replaced by tests.
type Client struct {
	AuthEndpoint   string
	TokenEndpoint  string
	RevokeEndpoint string
	GmailBaseURL   string
	HTTP           *http.Client
	Now            func() time.Time
}

// New returns a client of Google that does not follow redirects, so that a
// token is never sent to another host.
func New(now func() time.Time) *Client {
	return &Client{AuthEndpoint: DefaultAuthURL, TokenEndpoint: DefaultTokenURL, RevokeEndpoint: DefaultRevokeURL, GmailBaseURL: DefaultGmailURL, Now: now, HTTP: &http.Client{
		Timeout: timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}}
}

// Challenge returns the S256 PKCE code challenge of a code verifier.
func Challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// AuthURL returns the URL of Google's consent screen for the client: an
// authorization code with PKCE (S256) for gmail.readonly only, with a
// refresh token (access_type=offline, prompt=consent) and the account
// chooser. loginHint, the address of an account connected again, is left
// out when empty.
func (c *Client) AuthURL(clientID, redirectURI, state, challenge, loginHint string) string {
	q := url.Values{
		"response_type":          {"code"},
		"client_id":              {clientID},
		"redirect_uri":           {redirectURI},
		"scope":                  {ScopeGmailReadonly},
		"state":                  {state},
		"code_challenge":         {challenge},
		"code_challenge_method":  {"S256"},
		"access_type":            {"offline"},
		"prompt":                 {"consent select_account"},
		"include_granted_scopes": {"false"},
	}
	if loginHint != "" {
		q.Set("login_hint", loginHint)
	}
	return c.AuthEndpoint + "?" + q.Encode()
}

// Token is the answer of the token endpoint. RefreshToken is empty when
// Google did not send one (a refresh that keeps the current token). Scopes
// are the granted scopes, sorted without duplicates.
type Token struct {
	AccessToken  string
	RefreshToken string
	Expiry       time.Time
	Scopes       []string
}

// HasScope reports whether the token was granted scope.
func (t Token) HasScope(scope string) bool {
	for _, s := range t.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

// ParseScopes splits the space-separated scope of a token response.
func ParseScopes(v string) []string {
	seen := map[string]bool{}
	scopes := []string{}
	for _, s := range strings.Fields(v) {
		if !seen[s] {
			seen[s] = true
			scopes = append(scopes, s)
		}
	}
	sort.Strings(scopes)
	return scopes
}

// Exchange trades an authorization code and its PKCE verifier for tokens.
// A response without a refresh token is ErrNoRefreshToken.
func (c *Client) Exchange(ctx context.Context, clientID, secret, redirectURI, code, verifier string) (Token, error) {
	t, err := c.token(ctx, url.Values{"grant_type": {"authorization_code"}, "code": {code}, "code_verifier": {verifier}, "redirect_uri": {redirectURI}, "client_id": {clientID}, "client_secret": {secret}})
	if err != nil {
		return Token{}, err
	}
	if t.RefreshToken == "" {
		return Token{}, ErrNoRefreshToken
	}
	return t, nil
}

// Refresh gets a new access token with a refresh token. When Google
// replaced the refresh token, the new one is in RefreshToken.
func (c *Client) Refresh(ctx context.Context, clientID, secret, refreshToken string) (Token, error) {
	return c.token(ctx, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refreshToken}, "client_id": {clientID}, "client_secret": {secret}})
}

// token posts form to the token endpoint and validates the answer.
func (c *Client) token(ctx context.Context, form url.Values) (Token, error) {
	const method = "token"
	status, body, _, err := c.post(ctx, method, c.TokenEndpoint, form)
	if err != nil {
		return Token{}, err
	}
	if status != http.StatusOK {
		var out struct {
			Error string `json:"error"`
		}
		if status == http.StatusBadRequest || status == http.StatusUnauthorized || status == http.StatusForbidden {
			if json.Unmarshal(body, &out) == nil {
				switch out.Error {
				case "invalid_grant":
					return Token{}, fmt.Errorf("%w (%s)", ErrInvalidGrant, method)
				case "invalid_client", "unauthorized_client":
					return Token{}, fmt.Errorf("%w (%s)", ErrInvalidClient, method)
				case "admin_policy_enforced":
					return Token{}, fmt.Errorf("%w (%s)", ErrPolicyBlocked, method)
				}
			}
		}
		return Token{}, &StatusError{Method: method, Code: status}
	}
	var out struct {
		AccessToken  string          `json:"access_token"`
		ExpiresIn    json.RawMessage `json:"expires_in"`
		RefreshToken string          `json:"refresh_token"`
		Scope        string          `json:"scope"`
		TokenType    string          `json:"token_type"`
	}
	if err = json.Unmarshal(body, &out); err != nil || out.AccessToken == "" || !strings.EqualFold(out.TokenType, "Bearer") {
		return Token{}, fmt.Errorf("%w (%s)", ErrUnexpected, method)
	}
	expiry := defaultExpiry
	var seconds float64
	if json.Unmarshal(out.ExpiresIn, &seconds) == nil && seconds > 0 && seconds < 1e9 {
		expiry = time.Duration(seconds) * time.Second
	}
	return Token{AccessToken: out.AccessToken, RefreshToken: out.RefreshToken, Expiry: c.Now().Add(expiry), Scopes: ParseScopes(out.Scope)}, nil
}

// Revoke revokes a token, and with it every grant of the Google account to
// the client. The token is sent in the form body, never in the URL.
func (c *Client) Revoke(ctx context.Context, token string) error {
	const method = "revoke"
	status, _, _, err := c.post(ctx, method, c.RevokeEndpoint, url.Values{"token": {token}})
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return &StatusError{Method: method, Code: status}
	}
	return nil
}

// post sends a form to endpoint and returns the status and the body.
func (c *Client) post(ctx context.Context, method, endpoint string, form url.Values) (int, []byte, http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, nil, nil, fmt.Errorf("google: %s: invalid request", method)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "astovia")
	return c.send(method, req, tokenBodyLimit)
}

// send runs req and reads up to limit bytes of the body. A larger body is
// an *APIError with TooLarge.
func (c *Client) send(method string, req *http.Request, limit int) (int, []byte, http.Header, error) {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		// *url.Error includes the URL with its query; only the cause is
		// kept.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return 0, nil, nil, fmt.Errorf("google: %s: %w", method, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(limit)+1))
	if err != nil {
		return 0, nil, nil, fmt.Errorf("google: %s: %w", method, err)
	}
	if len(body) > limit {
		return 0, nil, nil, &APIError{Method: method, Status: resp.StatusCode, TooLarge: true}
	}
	return resp.StatusCode, body, resp.Header, nil
}

// retryAt reads Retry-After (seconds or an HTTP date) when it is within
// maxRetryWait from now.
func (c *Client) retryAt(h http.Header) time.Time {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return time.Time{}
	}
	now := c.Now()
	var at time.Time
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		if n < 0 || n > int64(maxRetryWait/time.Second) {
			return time.Time{}
		}
		at = now.Add(time.Duration(n) * time.Second)
	} else if t, err := http.ParseTime(v); err == nil {
		at = t
	} else {
		return time.Time{}
	}
	if at.Before(now) || at.After(now.Add(maxRetryWait)) {
		return time.Time{}
	}
	return at
}

// decode parses body into out; a body that does not fit is ErrUnexpected.
func decode(method string, body []byte, out interface{}) error {
	if err := json.NewDecoder(bytes.NewReader(body)).Decode(out); err != nil {
		return fmt.Errorf("%w (%s)", ErrUnexpected, method)
	}
	return nil
}
