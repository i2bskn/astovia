package oidc

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var (
	// ErrProviderUnavailable is returned when the provider metadata cannot
	// be read, or the token endpoint cannot be reached, times out or answers
	// with a 5xx status.
	ErrProviderUnavailable = errors.New("OIDC provider is unavailable")
	// ErrExchangeFailed is returned when the token endpoint refuses the code
	// or answers without an ID token.
	ErrExchangeFailed = errors.New("OIDC code exchange failed")
)

const (
	// exchangeTimeout bounds one request to the token endpoint. With the
	// discovery (3 seconds), the JWKS fetch (5 seconds) and the database it
	// stays within the server WriteTimeout (15 seconds).
	exchangeTimeout = 5 * time.Second
	// maxTokenResponseSize bounds the token endpoint response (1 MiB).
	maxTokenResponseSize = 1 << 20
)

// ClientConfig identifies the provider, the client registered at it and the
// redirect URI of this server.
type ClientConfig struct {
	// Issuer is the OIDC issuer of the provider, compared exactly. The
	// endpoints are read from its provider metadata.
	Issuer string
	// ClientID is the OIDC client ID.
	ClientID string
	// ClientSecret is the OIDC client secret. It is never logged.
	ClientSecret string
	// RedirectURI is the callback registered at the provider.
	RedirectURI string
}

// Client is the OIDC relying party: it reads the provider metadata on first
// use, builds the authorization URL, exchanges codes and verifies the ID
// tokens.
type Client struct {
	cfg       ClientConfig
	http      *http.Client
	discovery *discovery
	verifier  *Verifier
}

// New returns the client of c with an HTTP client that does not follow
// redirects; each request has its own timeout.
func New(c ClientConfig) *Client {
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return NewClient(c, client, time.Now)
}

// NewClient returns the client of c. client reads the provider metadata,
// fetches the JWKS and calls the token endpoint; it should not follow
// redirects. now is the clock of the token checks, the key cache and the
// discovery retries.
func NewClient(c ClientConfig, client *http.Client, now func() time.Time) *Client {
	return &Client{
		cfg:       c,
		http:      client,
		discovery: &discovery{issuer: c.Issuer, client: client, now: now},
		verifier:  NewVerifier(c.Issuer, c.ClientID, client, now),
	}
}

// Challenge returns the S256 PKCE challenge of verifier.
func Challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// AuthorizationURL returns the URL that starts the sign-in with state, nonce
// and the S256 PKCE challenge: the authorization endpoint of the provider
// metadata with its own query kept and the sign-in parameters set over it
// (RFC 6749 section 3.1). It returns ErrProviderUnavailable when the
// provider metadata cannot be read.
func (c *Client) AuthorizationURL(state, nonce, challenge string) (string, error) {
	p, err := c.discovery.get()
	if err != nil {
		return "", ErrProviderUnavailable
	}
	u := *p.authorization
	q := u.Query()
	q.Set("response_type", "code")
	q.Set("client_id", c.cfg.ClientID)
	q.Set("redirect_uri", c.cfg.RedirectURI)
	q.Set("scope", "openid email")
	q.Set("state", state)
	q.Set("nonce", nonce)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// Exchange redeems the authorization code with its PKCE verifier at the
// token endpoint and returns the ID token, unverified. The client
// authenticates with client_secret_basic, or client_secret_post when the
// provider supports only that. The access token is not used. Only the
// status code is logged, never the code, the secret or the response.
func (c *Client) Exchange(ctx context.Context, code, verifier string) (string, error) {
	p, err := c.discovery.get()
	if err != nil {
		return "", ErrProviderUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, exchangeTimeout)
	defer cancel()
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {c.cfg.RedirectURI},
		"code_verifier": {verifier},
	}
	if !p.basic {
		form.Set("client_id", c.cfg.ClientID)
		form.Set("client_secret", c.cfg.ClientSecret)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.token, strings.NewReader(form.Encode()))
	if err != nil {
		log.Printf("oidc: token request: cannot build the request")
		return "", ErrExchangeFailed
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if p.basic {
		// RFC 6749 section 2.3.1: both are form-encoded before Basic.
		req.SetBasicAuth(url.QueryEscape(c.cfg.ClientID), url.QueryEscape(c.cfg.ClientSecret))
	}
	res, err := c.http.Do(req)
	if err != nil {
		// The error names the URL, which is public provider metadata.
		log.Printf("oidc: token request: %v", err)
		return "", ErrProviderUnavailable
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode >= 500 {
		log.Printf("oidc: token request: status %d", res.StatusCode)
		return "", ErrProviderUnavailable
	}
	if res.StatusCode != http.StatusOK {
		log.Printf("oidc: token request: status %d", res.StatusCode)
		return "", ErrExchangeFailed
	}
	if mt, _, err := mime.ParseMediaType(res.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		log.Printf("oidc: token response: not JSON")
		return "", ErrExchangeFailed
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, maxTokenResponseSize+1))
	if err != nil {
		log.Printf("oidc: token response: read failed")
		return "", ErrProviderUnavailable
	}
	if len(body) > maxTokenResponseSize {
		log.Printf("oidc: token response: larger than 1 MiB")
		return "", ErrExchangeFailed
	}
	var out struct {
		IDToken any `json:"id_token"`
	}
	if err = json.Unmarshal(body, &out); err != nil {
		log.Printf("oidc: token response: invalid JSON")
		return "", ErrExchangeFailed
	}
	idToken, ok := out.IDToken.(string)
	if !ok || idToken == "" {
		log.Printf("oidc: token response: no id_token")
		return "", ErrExchangeFailed
	}
	return idToken, nil
}

// Verify verifies the ID token with the nonce of the sign-in against the
// keys of the jwks_uri of the provider metadata and returns its identity;
// see Verifier.Verify. It returns ErrKeysUnavailable when the provider
// metadata cannot be read.
func (c *Client) Verify(ctx context.Context, idToken, nonce string) (Identity, error) {
	p, err := c.discovery.get()
	if err != nil {
		return Identity{}, ErrKeysUnavailable
	}
	return c.verifier.Verify(ctx, idToken, nonce, p.keys)
}
