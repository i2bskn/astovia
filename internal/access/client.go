package access

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
	// ErrProviderUnavailable is returned when the token endpoint cannot be
	// reached, times out or answers with a 5xx status.
	ErrProviderUnavailable = errors.New("access token endpoint is unavailable")
	// ErrExchangeFailed is returned when the token endpoint refuses the code
	// or answers without an ID token.
	ErrExchangeFailed = errors.New("access code exchange failed")
)

const (
	// exchangeTimeout bounds one request to the token endpoint. With the
	// JWKS fetch and the database it stays within the server WriteTimeout.
	exchangeTimeout = 5 * time.Second
	// maxTokenResponseSize bounds the token endpoint response (1 MiB).
	maxTokenResponseSize = 1 << 20
)

// ClientConfig identifies the SaaS application and the redirect URI of this
// server.
type ClientConfig struct {
	// Issuer is the OIDC issuer of the application,
	// https://<team>.cloudflareaccess.com/cdn-cgi/access/sso/oidc/<client ID>.
	// The authorization, token and JWKS endpoints are under it.
	Issuer string
	// ClientID is the OIDC client ID of the application.
	ClientID string
	// ClientSecret is the OIDC client secret. It is never logged.
	ClientSecret string
	// RedirectURI is the callback registered in the application.
	RedirectURI string
}

// Client is the OIDC relying party of the SaaS application: it builds the
// authorization URL, exchanges codes and verifies the ID tokens.
type Client struct {
	cfg      ClientConfig
	http     *http.Client
	verifier *Verifier
}

// New returns the client of the application c with HTTP clients that do not
// follow redirects; each request has its own timeout.
func New(c ClientConfig) *Client {
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return NewClient(c, client, client, time.Now)
}

// NewClient returns the client of the application c. keys fetches the JWKS
// and exchange calls the token endpoint; neither should follow redirects.
// now is the clock of the token checks and the key cache.
func NewClient(c ClientConfig, keys, exchange *http.Client, now func() time.Time) *Client {
	return &Client{cfg: c, http: exchange, verifier: NewVerifier(c.Issuer, c.ClientID, keys, now)}
}

// Challenge returns the S256 PKCE challenge of verifier.
func Challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// AuthorizationURL returns the URL that starts the sign-in with state, nonce
// and the S256 PKCE challenge.
func (c *Client) AuthorizationURL(state, nonce, challenge string) string {
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {c.cfg.ClientID},
		"redirect_uri":          {c.cfg.RedirectURI},
		"scope":                 {"openid email"},
		"state":                 {state},
		"nonce":                 {nonce},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}
	return c.cfg.Issuer + "/authorization?" + q.Encode()
}

// Exchange redeems the authorization code with its PKCE verifier at the
// token endpoint and returns the ID token, unverified. The client
// authenticates with client_secret_basic. The access token is not used.
// Only the status code is logged, never the code, the secret or the
// response.
func (c *Client) Exchange(ctx context.Context, code, verifier string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, exchangeTimeout)
	defer cancel()
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {c.cfg.RedirectURI},
		"code_verifier": {verifier},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.Issuer+"/token", strings.NewReader(form.Encode()))
	if err != nil {
		log.Printf("access: token request: cannot build the request")
		return "", ErrExchangeFailed
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	// RFC 6749 section 2.3.1: both are form-encoded before Basic.
	req.SetBasicAuth(url.QueryEscape(c.cfg.ClientID), url.QueryEscape(c.cfg.ClientSecret))
	res, err := c.http.Do(req)
	if err != nil {
		// The error names the URL, which carries only the client ID.
		log.Printf("access: token request: %v", err)
		return "", ErrProviderUnavailable
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode >= 500 {
		log.Printf("access: token request: status %d", res.StatusCode)
		return "", ErrProviderUnavailable
	}
	if res.StatusCode != http.StatusOK {
		log.Printf("access: token request: status %d", res.StatusCode)
		return "", ErrExchangeFailed
	}
	if mt, _, err := mime.ParseMediaType(res.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		log.Printf("access: token response: not JSON")
		return "", ErrExchangeFailed
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, maxTokenResponseSize+1))
	if err != nil {
		log.Printf("access: token response: read failed")
		return "", ErrProviderUnavailable
	}
	if len(body) > maxTokenResponseSize {
		log.Printf("access: token response: larger than 1 MiB")
		return "", ErrExchangeFailed
	}
	var out struct {
		IDToken any `json:"id_token"`
	}
	if err = json.Unmarshal(body, &out); err != nil {
		log.Printf("access: token response: invalid JSON")
		return "", ErrExchangeFailed
	}
	idToken, ok := out.IDToken.(string)
	if !ok || idToken == "" {
		log.Printf("access: token response: no id_token")
		return "", ErrExchangeFailed
	}
	return idToken, nil
}

// Verify verifies the ID token with the nonce of the sign-in and returns its
// email claim; see Verifier.Verify.
func (c *Client) Verify(ctx context.Context, idToken, nonce string) (string, error) {
	return c.verifier.Verify(ctx, idToken, nonce)
}
