package oidc

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// testSecret has characters that change under form encoding.
const testSecret = "s3cr3t:with+special/chars%~"

const testRedirectURI = "https://astovia.test/auth/oidc/callback"

// providerServer is a TLS OIDC provider: it serves the provider metadata,
// the token endpoint and the JWKS, and records the last token request.
type providerServer struct {
	*httptest.Server
	mu sync.Mutex
	// edit changes the default metadata before it is served; a nil value
	// removes a member.
	edit func(m map[string]any)
	// metaStatus and metaType are the status and content type of the
	// metadata response; metaBody, when set, replaces the metadata.
	metaStatus int
	metaType   string
	metaBody   string
	// block, when set, is waited for before the metadata is served;
	// started receives a value when a metadata request arrives.
	block   chan struct{}
	started chan struct{}
	// token answers the token endpoint.
	token http.HandlerFunc
	keys  []map[string]any

	discoveries atomic.Int32
	keyRequests atomic.Int32
	// The last token request.
	form  url.Values
	user  string
	pass  string
	basic bool
	ctype string
	// keysQuery is the query of the last JWKS request.
	keysQuery string
}

func newProviderServer(t *testing.T) *providerServer {
	t.Helper()
	s := &providerServer{metaStatus: http.StatusOK, metaType: "application/json", token: answer(http.StatusOK, "application/json", `{"id_token":"id.token.value"}`)}
	s.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == oidcPath+discoveryPath && r.Method == http.MethodGet:
			s.serveMetadata(t, w)
		case r.URL.Path == oidcPath+"/token" && r.Method == http.MethodPost:
			if err := r.ParseForm(); err != nil {
				t.Error(err)
			}
			s.mu.Lock()
			s.form = r.PostForm
			s.user, s.pass, s.basic = r.BasicAuth()
			s.ctype = r.Header.Get("Content-Type")
			h := s.token
			s.mu.Unlock()
			h(w, r)
		case r.URL.Path == oidcPath+"/jwks" && r.Method == http.MethodGet:
			s.keyRequests.Add(1)
			s.mu.Lock()
			s.keysQuery = r.URL.RawQuery
			body, err := json.Marshal(map[string]any{"keys": s.keys})
			s.mu.Unlock()
			if err != nil {
				t.Error(err)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(body)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

// metadata returns the default provider metadata of s.
func (s *providerServer) metadata() map[string]any {
	base := s.URL + oidcPath
	return map[string]any{
		"issuer":                                base,
		"authorization_endpoint":                base + "/auth",
		"token_endpoint":                        base + "/token",
		"jwks_uri":                              base + "/jwks",
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"response_types_supported":              []string{"code"},
	}
}

func (s *providerServer) serveMetadata(t *testing.T, w http.ResponseWriter) {
	s.discoveries.Add(1)
	s.mu.Lock()
	m := s.metadata()
	if s.edit != nil {
		s.edit(m)
	}
	for k, v := range m {
		if v == nil {
			delete(m, k)
		}
	}
	status, ctype, raw, block, started := s.metaStatus, s.metaType, s.metaBody, s.block, s.started
	s.mu.Unlock()
	if started != nil {
		started <- struct{}{}
	}
	if block != nil {
		<-block
	}
	body := []byte(raw)
	if raw == "" {
		var err error
		if body, err = json.Marshal(m); err != nil {
			t.Error(err)
			return
		}
	}
	if ctype != "" {
		w.Header().Set("Content-Type", ctype)
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// set changes the server under its lock.
func (s *providerServer) set(f func(s *providerServer)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(s)
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

// newTestClient returns the client of s with the issuer of s, an HTTP
// client that trusts s and does not follow redirects, and a settable clock.
func newTestClient(s *providerServer) (*Client, *clock) {
	return newTestClientWithIssuer(s, testIssuer(s.URL))
}

func newTestClientWithIssuer(s *providerServer, issuer string) (*Client, *clock) {
	c := &clock{t: time.Unix(1_800_000_000, 0)}
	client := *s.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return NewClient(ClientConfig{Issuer: issuer, ClientID: testClientID, ClientSecret: testSecret, RedirectURI: testRedirectURI}, &client, c.now), c
}

func TestAuthorizationURL(t *testing.T) {
	s := newProviderServer(t)
	c, _ := newTestClient(s)
	raw, err := c.AuthorizationURL("state-1", "nonce-1", "challenge-1")
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme+"://"+u.Host+u.Path != s.URL+oidcPath+"/auth" {
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

// The query of the authorization endpoint is kept, and the sign-in
// parameters replace the ones it already has.
func TestAuthorizationURLKeepsQuery(t *testing.T) {
	s := newProviderServer(t)
	s.set(func(s *providerServer) {
		s.edit = func(m map[string]any) {
			m["authorization_endpoint"] = s.URL + oidcPath + "/auth?tenant=a%20b&client_id=evil&scope=profile&state=x"
		}
	})
	c, _ := newTestClient(s)
	raw, err := c.AuthorizationURL("state-1", "nonce-1", "challenge-1")
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if u.Path != oidcPath+"/auth" || q.Get("tenant") != "a b" {
		t.Fatalf("%s", raw)
	}
	for k, v := range map[string]string{"client_id": testClientID, "scope": "openid email", "state": "state-1", "response_type": "code"} {
		if len(q[k]) != 1 || q[k][0] != v {
			t.Fatalf("%s: %v", k, q[k])
		}
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
	s := newProviderServer(t)
	s.set(func(s *providerServer) {
		s.token = answer(http.StatusOK, "application/json; charset=utf-8", `{"access_token":"x","id_token":"id.token.value","token_type":"bearer"}`)
	})
	c, _ := newTestClient(s)
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
		s.set(func(s *providerServer) { s.token = tc.handler })
		if _, err := c.Exchange(ctx, "code-1", "verifier-1"); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v", name, err)
		}
	}

	// A token endpoint that cannot be reached once the metadata is known.
	closed := newProviderServer(t)
	closedClient, _ := newTestClient(closed)
	if _, err := closedClient.AuthorizationURL("s", "n", "c"); err != nil {
		t.Fatal(err)
	}
	closed.Close()
	if _, err := closedClient.Exchange(ctx, "code-1", "verifier-1"); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("closed: %v", err)
	}

	// A token endpoint that does not answer in time.
	slow := newProviderServer(t)
	slow.set(func(s *providerServer) {
		s.token = func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }
	})
	slowClient, _ := newTestClient(slow)
	timeout, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	if _, err := slowClient.Exchange(timeout, "code-1", "verifier-1"); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("timeout: %v", err)
	}
}

// With client_secret_post only, the ID and the secret are sent in the form
// and no Authorization header is sent; with both, Basic is used.
func TestExchangeAuthMethods(t *testing.T) {
	for name, tc := range map[string]struct {
		methods any
		basic   bool
	}{
		"absent":     {nil, true},
		"basic":      {[]string{"client_secret_basic"}, true},
		"post":       {[]string{"client_secret_post"}, false},
		"both":       {[]string{"client_secret_post", "client_secret_basic"}, true},
		"post first": {[]string{"private_key_jwt", "client_secret_post"}, false},
	} {
		t.Run(name, func(t *testing.T) {
			s := newProviderServer(t)
			s.set(func(s *providerServer) {
				s.edit = func(m map[string]any) { m["token_endpoint_auth_methods_supported"] = tc.methods }
			})
			c, _ := newTestClient(s)
			if _, err := c.Exchange(context.Background(), "code-1", "verifier-1"); err != nil {
				t.Fatal(err)
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			if tc.basic {
				if !s.basic || s.form.Has("client_id") || s.form.Has("client_secret") {
					t.Fatalf("basic %v, form %v", s.basic, s.form)
				}
				return
			}
			if s.basic || s.form.Get("client_id") != testClientID || s.form.Get("client_secret") != testSecret {
				t.Fatalf("basic %v, form %v", s.basic, s.form)
			}
			if s.form.Get("code") != "code-1" || s.form.Get("code_verifier") != "verifier-1" || s.form.Get("grant_type") != "authorization_code" {
				t.Fatalf("form %v", s.form)
			}
		})
	}
}

func TestClientVerify(t *testing.T) {
	k1 := newKey(t, "k1", 2048)
	s := newProviderServer(t)
	s.set(func(s *providerServer) {
		s.keys = []map[string]any{k1.jwk()}
		// The jwks_uri is used as it is, with its query.
		s.edit = func(m map[string]any) { m["jwks_uri"] = s.URL + oidcPath + "/jwks?v=2" }
	})
	c, _ := newTestClient(s)
	now := time.Unix(1_800_000_000, 0)
	cl := validClaims(s.URL, now)
	cl.EmailVerified = true
	id, err := c.Verify(context.Background(), sign(t, k1, cl), testNonce)
	if err != nil || id.Email != "alice@example.com" || id.EmailVerified == nil || !*id.EmailVerified {
		t.Fatalf("%+v %v", id, err)
	}
	s.mu.Lock()
	query := s.keysQuery
	s.mu.Unlock()
	if query != "v=2" || s.keyRequests.Load() != 1 {
		t.Fatalf("query %q, requests %d", query, s.keyRequests.Load())
	}
	if _, err = c.Verify(context.Background(), sign(t, k1, validClaims(s.URL, now)), "other"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("nonce: %v", err)
	}
}

// Without the provider metadata, nothing reaches the provider and every
// method reports the provider as unavailable.
func TestClientWithoutMetadata(t *testing.T) {
	s := newProviderServer(t)
	s.set(func(s *providerServer) { s.metaStatus = http.StatusInternalServerError })
	c, _ := newTestClient(s)
	ctx := context.Background()
	if _, err := c.AuthorizationURL("s", "n", "c"); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("authorization: %v", err)
	}
	if _, err := c.Exchange(ctx, "code-1", "verifier-1"); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("exchange: %v", err)
	}
	if _, err := c.Verify(ctx, "a.b.c", testNonce); !errors.Is(err, ErrKeysUnavailable) {
		t.Fatalf("verify: %v", err)
	}
	s.mu.Lock()
	form := s.form
	s.mu.Unlock()
	if form != nil || s.keyRequests.Load() != 0 {
		t.Fatal("the provider was called")
	}
}
