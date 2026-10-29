package openai

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

var testNow = time.Unix(1_800_000_000, 0)

const testState = "oai_" + "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQ"

func testClient(t *testing.T, h http.Handler) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := New("urn:uuid:test")
	c.AuthBaseURL, c.APIBaseURL = srv.URL, srv.URL+"/v1"
	c.Now = func() time.Time { return testNow }
	return c
}

func TestHostID(t *testing.T) {
	a, b := HostID([]byte("secret-1")), HostID([]byte("secret-1"))
	if a != b {
		t.Fatalf("not deterministic: %s %s", a, b)
	}
	if HostID([]byte("secret-2")) == a {
		t.Fatal("same id for another secret")
	}
	if !regexp.MustCompile(`^urn:uuid:[0-9a-f]{8}-[0-9a-f]{4}-8[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(a) {
		t.Fatalf("format: %s", a)
	}
}

func TestAuthURL(t *testing.T) {
	c := New("urn:uuid:host")
	for _, tc := range []struct {
		clientID string
		hint     bool
	}{{DynamicClientID, true}, {"oaiapp_123", false}} {
		u, err := url.Parse(c.AuthURL(tc.clientID, "st", "no", "ch"))
		if err != nil {
			t.Fatal(err)
		}
		if u.Scheme+"://"+u.Host+u.Path != "https://auth.openai.com/api/accounts/authorize" {
			t.Fatalf("endpoint: %s", u)
		}
		q := u.Query()
		want := map[string]string{"client_id": tc.clientID, "ext_agent_host_id": "urn:uuid:host", "response_type": "code", "redirect_uri": RedirectURI, "scope": Scope, "resource": Resource, "state": "st", "nonce": "no", "code_challenge": "ch", "code_challenge_method": "S256"}
		if tc.hint {
			want["agent_name_hint"] = "Astovia"
		}
		if len(q) != len(want) {
			t.Fatalf("%s: keys %v", tc.clientID, q)
		}
		for k, v := range want {
			if q.Get(k) != v {
				t.Fatalf("%s: %s=%q", tc.clientID, k, q.Get(k))
			}
		}
	}
}

func TestParseCallback(t *testing.T) {
	base := "http://127.0.0.1:1455/auth/callback?state=" + testState
	cb, err := ParseCallback("  " + base + "&code=ac_1-2.3&scope=openid+chatgpt.tokens.use.direct+openid&client_id=oaiapp_1&unknown=x&unknown=y&error_description=z  ")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cb, Callback{Code: "ac_1-2.3", State: testState, ClientID: "oaiapp_1", Scopes: []string{"chatgpt.tokens.use.direct", "openid"}}) {
		t.Fatalf("callback: %+v", cb)
	}
	if cb, err = ParseCallback("https://127.0.0.1:1455/auth/callback?state=" + testState + "&code=c&iss=https%3A%2F%2Fauth.openai.com"); err != nil || cb.Code != "c" || cb.Scopes != nil {
		t.Fatalf("https: %+v %v", cb, err)
	}
	if cb, err = ParseCallback(base + "&error=access_denied"); err != nil || cb.Error != "access_denied" || cb.Code != "" {
		t.Fatalf("error: %+v %v", cb, err)
	}
	secret := "SECRETCODE"
	for name, raw := range map[string]string{
		"empty":          "",
		"too long":       base + "&code=c&x=" + strings.Repeat("a", 8192),
		"localhost":      "http://localhost:1455/auth/callback?state=" + testState + "&code=" + secret,
		"port":           "http://127.0.0.1:1456/auth/callback?state=" + testState + "&code=" + secret,
		"path":           "http://127.0.0.1:1455/callback?state=" + testState + "&code=" + secret,
		"scheme":         "ftp://127.0.0.1:1455/auth/callback?state=" + testState + "&code=" + secret,
		"fragment":       base + "&code=" + secret + "#x",
		"userinfo":       "http://u:p@127.0.0.1:1455/auth/callback?state=" + testState + "&code=" + secret,
		"duplicate code": base + "&code=" + secret + "&code=b",
		"duplicate iss":  base + "&code=" + secret + "&iss=https://auth.openai.com&iss=https://auth.openai.com",
		"iss":            base + "&code=" + secret + "&iss=https://evil.example",
		"both":           base + "&code=" + secret + "&error=access_denied",
		"neither":        base,
		"no state":       "http://127.0.0.1:1455/auth/callback?code=" + secret,
		"state":          "http://127.0.0.1:1455/auth/callback?state=oai_short&code=" + secret,
		"code":           base + "&code=" + strings.Repeat("a", 4097),
		"code space":     base + "&code=a%20" + secret,
		"error":          base + "&error=Access-Denied",
		"client_id":      base + "&code=" + secret + "&client_id=a/b",
		"bad query":      base + "&code=" + secret + "&%zz",
	} {
		_, err := ParseCallback(raw)
		if !errors.Is(err, ErrCallbackInvalid) {
			t.Fatalf("%s: %v", name, err)
		}
		if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "127.0.0.1") {
			t.Fatalf("%s: error includes the input: %v", name, err)
		}
	}
}

func tokenHandler(t *testing.T, status int, body string, check func(url.Values)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/accounts/oauth/token" || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
			t.Errorf("request: %s %s", r.Method, r.URL.Path)
		}
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		if check != nil {
			check(r.PostForm)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	})
}

func TestExchangeAndRefresh(t *testing.T) {
	ok := `{"access_token":"at","refresh_token":"rt","id_token":"idt","token_type":"bearer","expires_in":3600,"scope":"openid chatgpt.tokens.use.direct","earliest_refresh_at":1}`
	c := testClient(t, tokenHandler(t, 200, ok, func(f url.Values) {
		want := url.Values{"grant_type": {"authorization_code"}, "client_id": {"oaiapp_1"}, "code": {"code-1"}, "code_verifier": {"ver"}, "redirect_uri": {RedirectURI}, "resource": {Resource}}
		if !reflect.DeepEqual(f, want) {
			t.Errorf("exchange form: %v", f)
		}
	}))
	tok, err := c.Exchange(context.Background(), "oaiapp_1", "code-1", "ver")
	if err != nil || tok.AccessToken != "at" || tok.RefreshToken != "rt" || tok.IDToken != "idt" || !tok.Expiry.Equal(testNow.Add(time.Hour)) || !reflect.DeepEqual(tok.Scopes, []string{"chatgpt.tokens.use.direct", "openid"}) {
		t.Fatalf("exchange: %+v %v", tok, err)
	}
	c = testClient(t, tokenHandler(t, 200, `{"access_token":"at2","refresh_token":"rt2","token_type":"Bearer"}`, func(f url.Values) {
		want := url.Values{"grant_type": {"refresh_token"}, "client_id": {"oaiapp_1"}, "refresh_token": {"rt"}, "resource": {Resource}}
		if !reflect.DeepEqual(f, want) {
			t.Errorf("refresh form: %v", f)
		}
	}))
	tok, err = c.Refresh(context.Background(), "oaiapp_1", "rt")
	if err != nil || tok.AccessToken != "at2" || tok.RefreshToken != "rt2" || !tok.Expiry.Equal(testNow.Add(50*time.Minute)) {
		t.Fatalf("refresh: %+v %v", tok, err)
	}

	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"invalid_grant", 400, `{"error":"invalid_grant"}`, ErrInvalidGrant},
		{"reused", 400, `{"error":"refresh_token_reused"}`, ErrInvalidGrant},
		{"expired", 401, `{"error":"refresh_token_expired"}`, ErrInvalidGrant},
		{"invalidated", 400, `{"error":"refresh_token_invalidated"}`, ErrInvalidGrant},
		{"invalid_refresh_token", 400, `{"error":"invalid_refresh_token"}`, ErrInvalidGrant},
		{"token_expired", 400, `{"error":"token_expired"}`, ErrInvalidGrant},
		{"invalid_client", 401, `{"error":"invalid_client"}`, ErrInvalidGrant},
		{"other 400", 400, `{"error":"invalid_request"}`, ErrRejected},
		{"403", 403, `nope`, ErrRejected},
		{"no refresh token", 200, `{"access_token":"at","token_type":"Bearer"}`, ErrRejected},
		{"no access token", 200, `{"refresh_token":"rt","token_type":"Bearer"}`, ErrRejected},
		{"token type", 200, `{"access_token":"at","refresh_token":"rt","token_type":"mac"}`, ErrRejected},
		{"not json", 200, `<html>`, ErrRejected},
		{"too large", 200, `{"access_token":"` + strings.Repeat("a", 70000) + `"}`, ErrRejected},
		{"408", 408, ``, ErrTemporary},
		{"429", 429, `{"error":"invalid_grant"}`, ErrTemporary},
		{"500", 500, ``, ErrTemporary},
	} {
		c := testClient(t, tokenHandler(t, tc.status, tc.body, nil))
		_, err := c.Refresh(context.Background(), "oaiapp_1", "SECRET-RT")
		if !errors.Is(err, tc.want) {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if strings.Contains(err.Error(), "SECRET-RT") || strings.Contains(err.Error(), "oaiapp_1") {
			t.Fatalf("%s: error includes a secret: %v", tc.name, err)
		}
	}
	// A connection error is temporary and does not include the URL or the
	// form.
	c = New("")
	c.AuthBaseURL = "http://127.0.0.1:1"
	if _, err := c.Exchange(context.Background(), "oaiapp_1", "SECRET-CODE", "v"); !errors.Is(err, ErrTemporary) || strings.Contains(err.Error(), "/api/") || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("connection error: %v", err)
	}
}

func TestRevoke(t *testing.T) {
	revokeRetryWait = time.Millisecond
	t.Cleanup(func() { revokeRetryWait = time.Second })
	for _, tc := range []struct {
		name     string
		statuses []int
		want     error
		calls    int32
	}{
		{"ok", []int{200}, nil, 1},
		{"retry", []int{503, 200}, nil, 2},
		{"retry fails", []int{500, 502}, ErrTemporary, 2},
		{"rejected", []int{400}, ErrRejected, 1},
	} {
		var calls atomic.Int32
		c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n := calls.Add(1)
			_ = r.ParseForm()
			if r.URL.Path != "/api/accounts/oauth/revoke" || !reflect.DeepEqual(r.PostForm, url.Values{"token": {"rt"}, "token_type_hint": {"refresh_token"}, "client_id": {"oaiapp_1"}}) {
				t.Errorf("request: %s %v", r.URL.Path, r.PostForm)
			}
			w.WriteHeader(tc.statuses[n-1])
		}))
		err := c.Revoke(context.Background(), "oaiapp_1", "rt")
		if !errors.Is(err, tc.want) && !(tc.want == nil && err == nil) {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if calls.Load() != tc.calls {
			t.Fatalf("%s: calls %d", tc.name, calls.Load())
		}
	}
}

type jwksServer struct {
	key     *rsa.PrivateKey
	kid     atomic.Value
	fetches atomic.Int32
}

func (j *jwksServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/.well-known/jwks.json" {
		w.WriteHeader(404)
		return
	}
	j.fetches.Add(1)
	e := big.NewInt(int64(j.key.E)).Bytes()
	_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]any{
		{"kty": "EC", "kid": "ec"},
		{"kty": "RSA", "kid": "enc", "use": "enc", "n": base64.RawURLEncoding.EncodeToString(j.key.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(e)},
		{"kty": "RSA", "kid": j.kid.Load().(string), "use": "sig", "alg": "RS256", "n": base64.RawURLEncoding.EncodeToString(j.key.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(e)},
	}})
}

func signed(t *testing.T, key any, method jwt.SigningMethod, kid string, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(method, claims)
	tok.Header["kid"] = kid
	s, err := tok.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestVerifyIDToken(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	j := &jwksServer{key: key}
	j.kid.Store("k1")
	c := testClient(t, j)
	claims := func(mod func(jwt.MapClaims)) jwt.MapClaims {
		m := jwt.MapClaims{"iss": Issuer, "aud": []string{"oaiapp_1"}, "sub": "user-1", "email": "a@example.com", "nonce": "n1", "exp": testNow.Add(time.Hour).Unix(), "iat": testNow.Unix()}
		if mod != nil {
			mod(m)
		}
		return m
	}
	ctx := context.Background()
	id, err := c.VerifyIDToken(ctx, signed(t, key, jwt.SigningMethodRS256, "k1", claims(nil)), "oaiapp_1", "n1")
	if err != nil || id != (Identity{Subject: "user-1", Email: "a@example.com"}) {
		t.Fatalf("valid: %+v %v", id, err)
	}
	// Within the leeway.
	if _, err = c.VerifyIDToken(ctx, signed(t, key, jwt.SigningMethodRS256, "k1", claims(func(m jwt.MapClaims) { m["exp"] = testNow.Add(-30 * time.Second).Unix(); delete(m, "email") })), "oaiapp_1", "n1"); err != nil {
		t.Fatalf("leeway: %v", err)
	}
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	for name, tok := range map[string]string{
		"audience":  signed(t, key, jwt.SigningMethodRS256, "k1", claims(func(m jwt.MapClaims) { m["aud"] = "other" })),
		"nonce":     signed(t, key, jwt.SigningMethodRS256, "k1", claims(func(m jwt.MapClaims) { m["nonce"] = "n2" })),
		"expired":   signed(t, key, jwt.SigningMethodRS256, "k1", claims(func(m jwt.MapClaims) { m["exp"] = testNow.Add(-2 * time.Minute).Unix() })),
		"no exp":    signed(t, key, jwt.SigningMethodRS256, "k1", claims(func(m jwt.MapClaims) { delete(m, "exp") })),
		"issuer":    signed(t, key, jwt.SigningMethodRS256, "k1", claims(func(m jwt.MapClaims) { m["iss"] = "https://evil.example" })),
		"subject":   signed(t, key, jwt.SigningMethodRS256, "k1", claims(func(m jwt.MapClaims) { delete(m, "sub") })),
		"signature": signed(t, other, jwt.SigningMethodRS256, "k1", claims(nil)),
		"hs256":     signed(t, []byte("secret"), jwt.SigningMethodHS256, "k1", claims(nil)),
		"none":      signed(t, jwt.UnsafeAllowNoneSignatureType, jwt.SigningMethodNone, "k1", claims(nil)),
		"enc key":   signed(t, key, jwt.SigningMethodRS256, "enc", claims(nil)),
		"empty":     "",
	} {
		if _, err := c.VerifyIDToken(ctx, tok, "oaiapp_1", "n1"); !errors.Is(err, ErrIDToken) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	// An unknown kid fetches the keys once more, and only once.
	before := j.fetches.Load()
	if _, err := c.VerifyIDToken(ctx, signed(t, key, jwt.SigningMethodRS256, "unknown", claims(nil)), "oaiapp_1", "n1"); !errors.Is(err, ErrIDToken) {
		t.Fatalf("unknown kid: %v", err)
	}
	if got := j.fetches.Load() - before; got != 1 {
		t.Fatalf("fetches for an unknown kid: %d", got)
	}
	// A rotated key is found with that one fetch.
	j.kid.Store("k2")
	before = j.fetches.Load()
	if _, err := c.VerifyIDToken(ctx, signed(t, key, jwt.SigningMethodRS256, "k2", claims(nil)), "oaiapp_1", "n1"); err != nil {
		t.Fatalf("rotated key: %v", err)
	}
	if got := j.fetches.Load() - before; got != 1 {
		t.Fatalf("fetches for a rotated key: %d", got)
	}
	// A known kid uses the cache.
	before = j.fetches.Load()
	if _, err := c.VerifyIDToken(ctx, signed(t, key, jwt.SigningMethodRS256, "k2", claims(nil)), "oaiapp_1", "n1"); err != nil || j.fetches.Load() != before {
		t.Fatalf("cached: %v %d", err, j.fetches.Load()-before)
	}
}
