package access

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// testSecret has characters that change under form encoding.
const testSecret = "s3cr3t:with+special/chars%~"

const testRedirectURI = "https://astovia.test/auth/access/callback"

// tokenServer is a token endpoint that answers with handler and records the
// last request.
type tokenServer struct {
	*httptest.Server
	mu      sync.Mutex
	handler http.HandlerFunc
	form    url.Values
	user    string
	pass    string
	basic   bool
	ctype   string
}

func newTokenServer(t *testing.T, handler http.HandlerFunc) *tokenServer {
	t.Helper()
	s := &tokenServer{handler: handler}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != oidcPath+"/token" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		s.mu.Lock()
		s.form = r.PostForm
		s.user, s.pass, s.basic = r.BasicAuth()
		s.ctype = r.Header.Get("Content-Type")
		h := s.handler
		s.mu.Unlock()
		h(w, r)
	}))
	t.Cleanup(s.Close)
	return s
}

// answer returns a handler writing body with the status and content type.
func answer(status int, contentType, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

func newTestClient(teamDomain string, client *http.Client) *Client {
	c := &clock{t: time.Unix(1_800_000_000, 0)}
	exchange := *client
	exchange.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return NewClient(ClientConfig{Issuer: testIssuer(teamDomain), ClientID: testClientID, ClientSecret: testSecret, RedirectURI: testRedirectURI}, client, &exchange, c.now)
}

func TestAuthorizationURL(t *testing.T) {
	c := newTestClient("https://example.cloudflareaccess.com", http.DefaultClient)
	u, err := url.Parse(c.AuthorizationURL("state-1", "nonce-1", "challenge-1"))
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme != "https" || u.Host != "example.cloudflareaccess.com" || u.Path != oidcPath+"/authorization" {
		t.Fatalf("%s", u)
	}
	want := url.Values{
		"response_type":         {"code"},
		"client_id":             {testClientID},
		"redirect_uri":          {testRedirectURI},
		"scope":                 {"openid email"},
		"state":                 {"state-1"},
		"nonce":                 {"nonce-1"},
		"code_challenge":        {"challenge-1"},
		"code_challenge_method": {"S256"},
	}
	if got := u.Query(); got.Encode() != want.Encode() {
		t.Fatalf("%v", got)
	}
	// The default client does not follow redirects.
	if New(ClientConfig{}).http.CheckRedirect == nil {
		t.Fatal("redirects are followed")
	}
}

func TestChallenge(t *testing.T) {
	// RFC 7636 appendix B.
	if got := Challenge("dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"); got != "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM" {
		t.Fatal(got)
	}
	sum := sha256.Sum256([]byte("v"))
	if Challenge("v") != base64.RawURLEncoding.EncodeToString(sum[:]) {
		t.Fatal("challenge")
	}
}

func TestExchange(t *testing.T) {
	s := newTokenServer(t, answer(http.StatusOK, "application/json; charset=utf-8", `{"access_token":"x","id_token":"id.token.value","token_type":"bearer"}`))
	c := newTestClient(s.URL, s.Client())
	ctx := context.Background()

	idToken, err := c.Exchange(ctx, "code-1", "verifier-1")
	if err != nil || idToken != "id.token.value" {
		t.Fatalf("%q %v", idToken, err)
	}
	s.mu.Lock()
	want := url.Values{"grant_type": {"authorization_code"}, "code": {"code-1"}, "redirect_uri": {testRedirectURI}, "code_verifier": {"verifier-1"}}
	if s.form.Encode() != want.Encode() || s.ctype != "application/x-www-form-urlencoded" {
		t.Fatalf("form %v, content type %q", s.form, s.ctype)
	}
	// client_secret_basic with the form-encoded ID and secret.
	if !s.basic || s.user != testClientID || s.pass != url.QueryEscape(testSecret) || s.pass == testSecret {
		t.Fatalf("basic auth %v %q", s.basic, s.user)
	}
	s.mu.Unlock()

	for name, tc := range map[string]struct {
		handler http.HandlerFunc
		want    error
	}{
		"400":              {answer(http.StatusBadRequest, "application/json", `{"error":"invalid_grant"}`), ErrExchangeFailed},
		"401":              {answer(http.StatusUnauthorized, "application/json", `{"error":"invalid_client"}`), ErrExchangeFailed},
		"302":              {func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/elsewhere", http.StatusFound) }, ErrExchangeFailed},
		"500":              {answer(http.StatusInternalServerError, "text/plain", "oops"), ErrProviderUnavailable},
		"503":              {answer(http.StatusServiceUnavailable, "application/json", `{}`), ErrProviderUnavailable},
		"no id_token":      {answer(http.StatusOK, "application/json", `{"access_token":"x"}`), ErrExchangeFailed},
		"empty id_token":   {answer(http.StatusOK, "application/json", `{"id_token":""}`), ErrExchangeFailed},
		"id_token number":  {answer(http.StatusOK, "application/json", `{"id_token":1}`), ErrExchangeFailed},
		"id_token null":    {answer(http.StatusOK, "application/json", `{"id_token":null}`), ErrExchangeFailed},
		"not JSON":         {answer(http.StatusOK, "application/json", `id_token=x`), ErrExchangeFailed},
		"not JSON type":    {answer(http.StatusOK, "text/html", `{"id_token":"x"}`), ErrExchangeFailed},
		"no content type":  {answer(http.StatusOK, "", `{"id_token":"x"}`), ErrExchangeFailed},
		"larger than 1MiB": {answer(http.StatusOK, "application/json", `{"id_token":"`+strings.Repeat("a", 1<<20)+`"}`), ErrExchangeFailed},
	} {
		s.mu.Lock()
		s.handler = tc.handler
		s.mu.Unlock()
		if _, err := c.Exchange(ctx, "code-1", "verifier-1"); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v", name, err)
		}
	}

	// A token endpoint that cannot be reached.
	closed := newTokenServer(t, answer(http.StatusOK, "application/json", `{"id_token":"x"}`))
	closedClient := newTestClient(closed.URL, closed.Client())
	closed.Close()
	if _, err := closedClient.Exchange(ctx, "code-1", "verifier-1"); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("closed: %v", err)
	}

	// A token endpoint that does not answer in time.
	slow := newTokenServer(t, func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	slowClient := newTestClient(slow.URL, slow.Client())
	timeout, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if _, err := slowClient.Exchange(timeout, "code-1", "verifier-1"); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("timeout: %v", err)
	}
}

func TestClientVerify(t *testing.T) {
	k1 := newKey(t, "k1", 2048)
	s := newJWKSServer(t, k1.jwk())
	c := newTestClient(s.URL, s.Client())
	now := time.Unix(1_800_000_000, 0)
	email, err := c.Verify(context.Background(), sign(t, k1, validClaims(s.URL, now)), testNonce)
	if err != nil || email != "alice@example.com" {
		t.Fatalf("%q %v", email, err)
	}
	if _, err = c.Verify(context.Background(), sign(t, k1, validClaims(s.URL, now)), "other"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("nonce: %v", err)
	}
}
