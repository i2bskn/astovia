// Package openai is a small client of OpenAI's "Sign in with ChatGPT" for
// the ChatGPT plan usage of open source clients: the OAuth endpoints of
// auth.openai.com (authorization with PKCE, code exchange, refresh and
// revocation, ID token verification) and the Responses and Models APIs
// called with the user's access token. Tokens are sent to the endpoints of
// the client (AuthBaseURL and APIBaseURL) and nowhere else; redirects are
// not followed and URLs found in responses are never requested. Errors carry
// the method, the HTTP status and OpenAI's error code, never a request URL,
// a response body, a token, an authorization code, a client ID or an
// address.
package openai

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Fixed values of the ChatGPT plan usage flow.
const (
	// Issuer is the issuer of ID tokens and the default AuthBaseURL.
	Issuer = "https://auth.openai.com"
	// RedirectURI is the only redirect URI the flow allows. The browser
	// cannot reach it from a remote Astovia: the user pastes the address
	// of the page it shows instead.
	RedirectURI = "http://127.0.0.1:1455/auth/callback"
	// Resource is the API the tokens are for, and the default APIBaseURL.
	Resource = "https://api.openai.com/v1"
	// Scope is the scope requested.
	Scope = "openid profile email offline_access resource.invoke chatgpt.tokens.use.direct"
	// PlanScope is the scope that allows using the user's ChatGPT plan.
	PlanScope = "chatgpt.tokens.use.direct"
	// DynamicClientID registers a new client on the first authorization.
	DynamicClientID = "dynamic_agent_client"
	// AgentName is shown to the user on the first authorization.
	AgentName = "Astovia"
)

const (
	// bodyLimit bounds the responses of the OAuth endpoints and of errors.
	bodyLimit = 64 << 10
	// defaultExpiry is the lifetime assumed for an access token whose
	// expires_in is missing or not positive.
	defaultExpiry = 50 * time.Minute
	// jwksTTL is how long fetched signing keys are used.
	jwksTTL = time.Hour
	// leeway is the clock skew allowed for ID tokens.
	leeway = 60 * time.Second
	// callbackLimit bounds a pasted callback URL.
	callbackLimit = 8192
	// clientTimeout is a safety net; callers bound each call with a
	// context.
	clientTimeout = 60 * time.Second
)

// revokeRetryWait is the wait before the one retry of a revocation.
// Tests shorten it.
var revokeRetryWait = time.Second

var (
	// ErrInvalidGrant is a refresh token or an authorization code that can
	// no longer be used, or a client OpenAI refuses: the user has to
	// connect again.
	ErrInvalidGrant = errors.New("openai: invalid grant")
	// ErrUnauthorized is an access token the API refuses (HTTP 401 or
	// subscription_sharing_invalid_user). It matches *APIError with
	// errors.Is.
	ErrUnauthorized = errors.New("openai: unauthorized")
	// ErrTemporary is a 408, 429 or 5xx of the OAuth endpoints, a
	// connection error, a timeout or a stream that ended early.
	ErrTemporary = errors.New("openai: temporary")
	// ErrRejected is any other refusal or a response whose shape is not
	// the documented one.
	ErrRejected = errors.New("openai: rejected")
	// ErrIDToken is an ID token that cannot be verified.
	ErrIDToken = errors.New("openai: invalid id token")
	// ErrCallbackInvalid is a pasted callback URL that cannot be used.
	ErrCallbackInvalid = errors.New("openai: invalid callback")
	// ErrRefused is an answer the model refused to give.
	ErrRefused = errors.New("openai: refused")
)

// terminalGrantErrors are the OAuth errors after which the user has to
// connect again.
var terminalGrantErrors = map[string]bool{
	"invalid_grant":             true,
	"invalid_refresh_token":     true,
	"token_expired":             true,
	"refresh_token_expired":     true,
	"refresh_token_invalidated": true,
	"refresh_token_reused":      true,
	"invalid_client":            true,
}

// APIError is a refusal of the Responses or Models API: the HTTP status
// (0 when it was reported in the stream) and OpenAI's error code and
// parameter, both reduced to safe characters.
type APIError struct {
	Status int
	Code   string
	Param  string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("openai: status %d code %q param %q", e.Status, e.Code, e.Param)
}

// Is makes errors.Is(err, ErrUnauthorized) true for a refused token.
func (e *APIError) Is(target error) bool {
	return target == ErrUnauthorized && (e.Status == http.StatusUnauthorized || e.Code == "subscription_sharing_invalid_user")
}

// Client calls OpenAI. The URLs, HTTP and Now are replaced by tests.
type Client struct {
	AuthBaseURL string
	APIBaseURL  string
	HTTP        *http.Client
	// HostID is the ext_agent_host_id of the authorizations.
	HostID string
	Now    func() time.Time

	// mu guards the signing keys fetched from the JWKS endpoint.
	mu     sync.Mutex
	keys   map[string]*rsa.PublicKey
	keysAt time.Time
}

// New returns a client of OpenAI that does not follow redirects, so that a
// token is never sent to another host. hostID may be empty when no
// authorization is started with the client.
func New(hostID string) *Client {
	return &Client{AuthBaseURL: Issuer, APIBaseURL: Resource, HostID: hostID, Now: time.Now, HTTP: &http.Client{
		Timeout: clientTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}}
}

func (c *Client) now() time.Time {
	if c.Now == nil {
		return time.Now()
	}
	return c.Now()
}

// HostID derives the ext_agent_host_id of the instance from its secret: the
// first 16 bytes of an HMAC-SHA256 as a version 8 UUID URN. The same secret
// always gives the same value, and nothing about a user is included.
func HostID(secret []byte) string {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte("astovia openai ext_agent_host_id"))
	b := m.Sum(nil)[:16]
	b[6] = (b[6] & 0x0f) | 0x80
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b)
	return "urn:uuid:" + h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// Challenge returns the S256 PKCE code challenge of a code verifier.
func Challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// AuthURL returns the URL of OpenAI's sign-in for the client. The agent
// name is sent only when a new client is registered (DynamicClientID).
func (c *Client) AuthURL(clientID, state, nonce, challenge string) string {
	q := url.Values{
		"client_id":             {clientID},
		"ext_agent_host_id":     {c.HostID},
		"response_type":         {"code"},
		"redirect_uri":          {RedirectURI},
		"scope":                 {Scope},
		"resource":              {Resource},
		"state":                 {state},
		"nonce":                 {nonce},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}
	if clientID == DynamicClientID {
		q.Set("agent_name_hint", AgentName)
	}
	return c.AuthBaseURL + "/api/accounts/authorize?" + q.Encode()
}

// Callback is what a pasted callback URL carries: either Code or Error,
// with State. ClientID is empty when OpenAI left it out. Scopes are sorted
// without duplicates, or nil when the URL had no scope.
type Callback struct {
	Code     string
	State    string
	ClientID string
	Error    string
	Scopes   []string
}

var (
	callbackStatePattern = regexp.MustCompile(`^oai_[A-Za-z0-9_-]{43}$`)
	clientIDPattern      = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,256}$`)
	oauthErrorPattern    = regexp.MustCompile(`^[a-z_]{1,64}$`)
	// safePattern keeps an error code or parameter name of the API.
	safePattern = regexp.MustCompile(`^[A-Za-z0-9_.\[\]-]{1,100}$`)
)

// callbackKeys are the query keys read from a callback; each may appear at
// most once.
var callbackKeys = []string{"code", "state", "scope", "client_id", "error", "error_description", "iss"}

func invalidCallback(reason string) error {
	return fmt.Errorf("%w (%s)", ErrCallbackInvalid, reason)
}

// ParseCallback reads the address of the page the browser was sent to
// after the sign-in. https is accepted for the HTTPS-Only mode of browsers;
// the code is exchanged with RedirectURI anyway. Errors never include the
// URL or its values.
func ParseCallback(raw string) (Callback, error) {
	raw = strings.TrimSpace(raw)
	if len(raw) < 1 || len(raw) > callbackLimit {
		return Callback{}, invalidCallback("length")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return Callback{}, invalidCallback("url")
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Opaque != "" || u.User != nil || u.Host != "127.0.0.1:1455" || u.Path != "/auth/callback" || u.Fragment != "" || strings.Contains(raw, "#") {
		return Callback{}, invalidCallback("address")
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return Callback{}, invalidCallback("query")
	}
	for _, k := range callbackKeys {
		if len(q[k]) > 1 {
			return Callback{}, invalidCallback("duplicate key")
		}
	}
	if q.Has("iss") && q.Get("iss") != Issuer {
		return Callback{}, invalidCallback("issuer")
	}
	cb := Callback{Code: q.Get("code"), State: q.Get("state"), ClientID: q.Get("client_id"), Error: q.Get("error")}
	if !callbackStatePattern.MatchString(cb.State) {
		return Callback{}, invalidCallback("state")
	}
	if q.Has("code") == q.Has("error") {
		return Callback{}, invalidCallback("code or error")
	}
	if q.Has("code") && !validCode(cb.Code) {
		return Callback{}, invalidCallback("code")
	}
	if q.Has("error") && !oauthErrorPattern.MatchString(cb.Error) {
		return Callback{}, invalidCallback("error")
	}
	if q.Has("client_id") && !clientIDPattern.MatchString(cb.ClientID) {
		return Callback{}, invalidCallback("client_id")
	}
	if q.Has("scope") {
		cb.Scopes = ParseScopes(q.Get("scope"))
	}
	return cb, nil
}

// validCode accepts an authorization code of 1-4096 printable ASCII
// characters.
func validCode(v string) bool {
	if len(v) < 1 || len(v) > 4096 {
		return false
	}
	for i := 0; i < len(v); i++ {
		if v[i] < 0x21 || v[i] > 0x7e {
			return false
		}
	}
	return true
}

// ParseScopes splits a space-separated scope, sorted without duplicates.
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

// HasScope reports whether scopes include scope.
func HasScope(scopes []string, scope string) bool {
	for _, s := range scopes {
		if s == scope {
			return true
		}
	}
	return false
}

// Token is the answer of the token endpoint. IDToken is empty when the
// endpoint did not send one (a refresh may not). Scopes are sorted without
// duplicates.
type Token struct {
	AccessToken  string
	RefreshToken string
	Expiry       time.Time
	Scopes       []string
	IDToken      string
}

// Exchange trades an authorization code and its PKCE verifier for tokens.
func (c *Client) Exchange(ctx context.Context, clientID, code, verifier string) (Token, error) {
	return c.token(ctx, "exchange", url.Values{"grant_type": {"authorization_code"}, "client_id": {clientID}, "code": {code}, "code_verifier": {verifier}, "redirect_uri": {RedirectURI}, "resource": {Resource}})
}

// Refresh gets new tokens with a refresh token. The refresh token is
// replaced on every refresh; the old one cannot be used again, so refreshes
// of one session must not run concurrently.
func (c *Client) Refresh(ctx context.Context, clientID, refreshToken string) (Token, error) {
	return c.token(ctx, "refresh", url.Values{"grant_type": {"refresh_token"}, "client_id": {clientID}, "refresh_token": {refreshToken}, "resource": {Resource}})
}

// token posts form to the token endpoint and validates the answer. Both a
// refresh token and an access token are required.
func (c *Client) token(ctx context.Context, method string, form url.Values) (Token, error) {
	status, body, err := c.post(ctx, method, c.AuthBaseURL+"/api/accounts/oauth/token", form)
	if err != nil {
		return Token{}, err
	}
	if status != http.StatusOK {
		return Token{}, oauthError(method, status, body)
	}
	var out struct {
		AccessToken  string          `json:"access_token"`
		RefreshToken string          `json:"refresh_token"`
		IDToken      string          `json:"id_token"`
		TokenType    string          `json:"token_type"`
		ExpiresIn    json.RawMessage `json:"expires_in"`
		Scope        string          `json:"scope"`
	}
	if err = json.Unmarshal(body, &out); err != nil || out.AccessToken == "" || out.RefreshToken == "" || !strings.EqualFold(out.TokenType, "Bearer") {
		return Token{}, fmt.Errorf("%w (%s: unexpected response)", ErrRejected, method)
	}
	expiry := defaultExpiry
	var seconds float64
	if json.Unmarshal(out.ExpiresIn, &seconds) == nil && seconds > 0 && seconds < 1e9 {
		expiry = time.Duration(seconds) * time.Second
	}
	return Token{AccessToken: out.AccessToken, RefreshToken: out.RefreshToken, IDToken: out.IDToken, Expiry: c.now().Add(expiry), Scopes: ParseScopes(out.Scope)}, nil
}

// oauthError classifies a refusal of an OAuth endpoint.
func oauthError(method string, status int, body []byte) error {
	if temporaryStatus(status) {
		return fmt.Errorf("%w (%s: status %d)", ErrTemporary, method, status)
	}
	var out struct {
		Error json.RawMessage `json:"error"`
	}
	var code string
	if json.Unmarshal(body, &out) == nil && out.Error != nil {
		if json.Unmarshal(out.Error, &code) != nil {
			var obj struct {
				Code string `json:"code"`
			}
			if json.Unmarshal(out.Error, &obj) == nil {
				code = obj.Code
			}
		}
	}
	if status >= 400 && status < 500 && terminalGrantErrors[code] {
		return fmt.Errorf("%w (%s: %s)", ErrInvalidGrant, method, code)
	}
	return fmt.Errorf("%w (%s: status %d)", ErrRejected, method, status)
}

// temporaryStatus reports whether status is worth trying again later.
func temporaryStatus(status int) bool {
	return status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500
}

// Revoke revokes a refresh token. It is tried once more after a 5xx or a
// connection error.
func (c *Client) Revoke(ctx context.Context, clientID, refreshToken string) error {
	const method = "revoke"
	form := url.Values{"token": {refreshToken}, "token_type_hint": {"refresh_token"}, "client_id": {clientID}}
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			t := time.NewTimer(revokeRetryWait)
			select {
			case <-ctx.Done():
				t.Stop()
				return fmt.Errorf("%w (%s): %w", ErrTemporary, method, ctx.Err())
			case <-t.C:
			}
		}
		var status int
		status, _, err = c.post(ctx, method, c.AuthBaseURL+"/api/accounts/oauth/revoke", form)
		if err == nil {
			if status == http.StatusOK {
				return nil
			}
			if status < 500 {
				return fmt.Errorf("%w (%s: status %d)", ErrRejected, method, status)
			}
			err = fmt.Errorf("%w (%s: status %d)", ErrTemporary, method, status)
		}
	}
	return err
}

// post sends a form to endpoint and returns the status and the body.
func (c *Client) post(ctx context.Context, method, endpoint string, form url.Values) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, nil, fmt.Errorf("%w (%s: invalid request)", ErrRejected, method)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "astovia")
	return c.send(method, req)
}

// send runs req and reads up to bodyLimit bytes of the body. A larger body
// is ErrRejected.
func (c *Client) send(method string, req *http.Request) (int, []byte, error) {
	resp, err := c.do(method, req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, bodyLimit+1))
	if err != nil {
		return 0, nil, fmt.Errorf("%w (%s): %w", ErrTemporary, method, err)
	}
	if len(body) > bodyLimit {
		return 0, nil, fmt.Errorf("%w (%s: response body is too large)", ErrRejected, method)
	}
	return resp.StatusCode, body, nil
}

// do runs req. A failure is ErrTemporary with the cause only: *url.Error
// includes the URL with its query.
func (c *Client) do(method string, req *http.Request) (*http.Response, error) {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, fmt.Errorf("%w (%s): %w", ErrTemporary, method, err)
	}
	return resp, nil
}

// Identity is the account of a verified ID token. Email is empty when the
// token had none.
type Identity struct {
	Subject string
	Email   string
}

type idClaims struct {
	jwt.RegisteredClaims
	Nonce string `json:"nonce"`
	Email string `json:"email"`
}

// VerifyIDToken verifies an ID token: an RS256 signature by a key of the
// JWKS endpoint, the issuer, clientID in the audience, an expiry in the
// future (60 seconds of clock skew allowed), the nonce of the authorization
// and a subject.
func (c *Client) VerifyIDToken(ctx context.Context, idToken, clientID, nonce string) (Identity, error) {
	if idToken == "" {
		return Identity{}, fmt.Errorf("%w (missing)", ErrIDToken)
	}
	refetched := false
	var claims idClaims
	_, err := jwt.ParseWithClaims(idToken, &claims, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		key, err := c.signingKey(ctx, kid, &refetched)
		if err != nil {
			return nil, err
		}
		return key, nil
	}, jwt.WithValidMethods([]string{"RS256"}), jwt.WithIssuer(Issuer), jwt.WithAudience(clientID), jwt.WithExpirationRequired(), jwt.WithLeeway(leeway), jwt.WithTimeFunc(c.now))
	if err != nil {
		return Identity{}, fmt.Errorf("%w: %w", ErrIDToken, err)
	}
	if nonce == "" || subtle.ConstantTimeCompare([]byte(claims.Nonce), []byte(nonce)) != 1 {
		return Identity{}, fmt.Errorf("%w (nonce)", ErrIDToken)
	}
	if claims.Subject == "" {
		return Identity{}, fmt.Errorf("%w (subject)", ErrIDToken)
	}
	return Identity{Subject: claims.Subject, Email: claims.Email}, nil
}

// signingKey returns the key of kid. The keys are fetched when the cache is
// empty or old, and once more per verification for an unknown kid.
func (c *Client) signingKey(ctx context.Context, kid string, refetched *bool) (*rsa.PublicKey, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fresh := c.keys != nil && c.now().Sub(c.keysAt) < jwksTTL
	if fresh {
		if k, ok := c.keys[kid]; ok {
			return k, nil
		}
		if *refetched {
			return nil, errors.New("unknown key")
		}
	}
	keys, err := c.fetchKeys(ctx)
	*refetched = true
	if err != nil {
		return nil, err
	}
	c.keys, c.keysAt = keys, c.now()
	if k, ok := keys[kid]; ok {
		return k, nil
	}
	return nil, errors.New("unknown key")
}

// fetchKeys reads the RSA signing keys of the JWKS endpoint. Keys of
// another type or use are ignored.
func (c *Client) fetchKeys(ctx context.Context) (map[string]*rsa.PublicKey, error) {
	const method = "jwks"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.AuthBaseURL+"/.well-known/jwks.json", nil)
	if err != nil {
		return nil, fmt.Errorf("%w (%s: invalid request)", ErrRejected, method)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "astovia")
	status, body, err := c.send(method, req)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("%w (%s: status %d)", ErrRejected, method, status)
	}
	var out struct {
		Keys []struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			Use string `json:"use"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err = json.NewDecoder(bytes.NewReader(body)).Decode(&out); err != nil {
		return nil, fmt.Errorf("%w (%s: unexpected response)", ErrRejected, method)
	}
	keys := map[string]*rsa.PublicKey{}
	for _, k := range out.Keys {
		if k.Kty != "RSA" || (k.Use != "" && k.Use != "sig") {
			continue
		}
		n, errN := base64.RawURLEncoding.DecodeString(strings.TrimRight(k.N, "="))
		e, errE := base64.RawURLEncoding.DecodeString(strings.TrimRight(k.E, "="))
		if errN != nil || errE != nil || len(e) == 0 || len(e) > 4 {
			continue
		}
		key := &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
		if key.N.BitLen() < 2048 || key.E < 3 {
			continue
		}
		keys[k.Kid] = key
	}
	return keys, nil
}
