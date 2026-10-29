package access

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"log"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	testClientID = "0123456789abcdef0123456789abcdef"
	testNonce    = "nonce-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
)

// oidcPath is the path of the issuer of the test application.
const oidcPath = "/cdn-cgi/access/sso/oidc/" + testClientID

// testIssuer returns the issuer of the test application on teamDomain.
func testIssuer(teamDomain string) string {
	return teamDomain + oidcPath
}

// testKey is an RSA key with its key ID.
type testKey struct {
	kid  string
	priv *rsa.PrivateKey
}

// newKey returns a new RSA key of bits with the key ID kid.
func newKey(t *testing.T, kid string, bits int) testKey {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		t.Fatal(err)
	}
	return testKey{kid: kid, priv: priv}
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// jwk returns the JWK of the public key, as Cloudflare publishes it.
func (k testKey) jwk() map[string]any {
	return map[string]any{
		"kty": "RSA", "kid": k.kid, "alg": "RS256", "use": "sig",
		"n": b64(k.priv.N.Bytes()), "e": b64(big.NewInt(int64(k.priv.E)).Bytes()),
	}
}

// with returns a copy of m with the members of extra; a nil value removes
// the member.
func with(m map[string]any, extra map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range m {
		out[k] = v
	}
	for k, v := range extra {
		if v == nil {
			delete(out, k)
		} else {
			out[k] = v
		}
	}
	return out
}

// jwksServer serves the keys in its set and counts the requests.
type jwksServer struct {
	*httptest.Server
	mu       sync.Mutex
	set      []map[string]any
	status   int
	requests atomic.Int32
	// block, when set, is waited for before answering; started receives a
	// value when a request arrives.
	block   chan struct{}
	started chan struct{}
}

func newJWKSServer(t *testing.T, keys ...map[string]any) *jwksServer {
	t.Helper()
	s := &jwksServer{set: keys, status: http.StatusOK}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != oidcPath+"/jwks" || r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		s.requests.Add(1)
		s.mu.Lock()
		block, started, status := s.block, s.started, s.status
		body, err := json.Marshal(map[string]any{"keys": s.set})
		s.mu.Unlock()
		if started != nil {
			started <- struct{}{}
		}
		if block != nil {
			<-block
		}
		if err != nil {
			t.Error(err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *jwksServer) setKeys(keys ...map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.set = keys
}

func (s *jwksServer) setStatus(status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = status
}

// clock is a settable time source.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newVerifier(s *jwksServer, c *clock) *Verifier {
	return NewVerifier(testIssuer(s.URL), testClientID, s.Client(), c.now)
}

type tokenClaims struct {
	jwt.RegisteredClaims
	Email string `json:"email,omitempty"`
	Nonce string `json:"nonce,omitempty"`
}

// validClaims returns the claims of an ID token issued now by the test
// application of the team domain teamDomain.
func validClaims(teamDomain string, now time.Time) tokenClaims {
	return tokenClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    testIssuer(teamDomain),
			Audience:  jwt.ClaimStrings{testClientID},
			ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)),
			NotBefore: jwt.NewNumericDate(now),
			IssuedAt:  jwt.NewNumericDate(now),
			Subject:   "user-1",
		},
		Email: "alice@example.com",
		Nonce: testNonce,
	}
}

// signWith signs claims with RS256 and the header members of header.
func signWith(t *testing.T, k testKey, claims any, header map[string]any) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims.(jwt.Claims))
	if k.kid != "" {
		token.Header["kid"] = k.kid
	}
	for name, v := range header {
		token.Header[name] = v
	}
	s, err := token.SignedString(k.priv)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func sign(t *testing.T, k testKey, claims any) string {
	t.Helper()
	return signWith(t, k, claims, nil)
}

// rawToken returns an unsigned token with the header and claims as JSON,
// followed by signature.
func rawToken(t *testing.T, header string, claims any, signature string) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return b64([]byte(header)) + "." + b64(payload) + "." + signature
}

// captureLog sends the log output to a buffer for the rest of the test.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return &buf
}

func TestVerify(t *testing.T) {
	logs := captureLog(t)
	k1 := newKey(t, "k1", 2048)
	s := newJWKSServer(t, k1.jwk())
	c := &clock{t: time.Unix(1_800_000_000, 0)}
	v := newVerifier(s, c)
	ctx := context.Background()

	email, err := v.Verify(ctx, sign(t, k1, validClaims(s.URL, c.now())), testNonce)
	if err != nil || email != "alice@example.com" {
		t.Fatalf("%q %v", email, err)
	}
	// The email is returned as it is; normalization is up to the caller.
	claims := validClaims(s.URL, c.now())
	claims.Email = " Alice@Example.com"
	if email, err = v.Verify(ctx, sign(t, k1, claims), testNonce); err != nil || email != " Alice@Example.com" {
		t.Fatalf("%q %v", email, err)
	}
	// A single audience may also be a string.
	singleToken := rawToken(t, `{"alg":"RS256","typ":"JWT","kid":"k1"}`, map[string]any{"iss": testIssuer(s.URL), "aud": testClientID, "exp": c.now().Add(time.Hour).Unix(), "email": "alice@example.com", "nonce": testNonce}, "")
	singleToken = strings.TrimSuffix(singleToken, ".")
	sig, err := jwt.SigningMethodRS256.Sign(singleToken, k1.priv)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = v.Verify(ctx, singleToken+"."+b64(sig), testNonce); err != nil {
		t.Fatalf("string audience: %v", err)
	}

	// An HS256 token whose HMAC key is the public key of k1 (the key
	// confusion attack), in DER and in PEM.
	der, err := x509.MarshalPKIXPublicKey(&k1.priv.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	hsToken := func(key []byte) string {
		token := jwt.NewWithClaims(jwt.SigningMethodHS256, validClaims(s.URL, c.now()))
		token.Header["kid"] = "k1"
		out, err := token.SignedString(key)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	none := rawToken(t, `{"alg":"none","kid":"k1"}`, validClaims(s.URL, c.now()), "")
	noneUpper := rawToken(t, `{"alg":"None","kid":"k1"}`, validClaims(s.URL, c.now()), "")
	// A valid RS256 signature with the alg changed afterwards.
	parts := strings.Split(sign(t, k1, validClaims(s.URL, c.now())), ".")
	ps256 := b64([]byte(`{"alg":"PS256","typ":"JWT","kid":"k1"}`)) + "." + parts[1] + "." + parts[2]

	other := newKey(t, "k1", 2048)
	mutate := func(f func(*tokenClaims)) string {
		cl := validClaims(s.URL, c.now())
		f(&cl)
		return sign(t, k1, cl)
	}
	for name, token := range map[string]string{
		"empty":               "",
		"too large":           strings.Repeat("a", 16<<10+1),
		"garbage":             "not.a.jwt",
		"two parts":           parts[0] + "." + parts[1],
		"bad signature":       parts[0] + "." + parts[1] + "." + b64([]byte("x")),
		"other key":           sign(t, other, validClaims(s.URL, c.now())),
		"HS256 DER":           hsToken(der),
		"HS256 PEM":           hsToken(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})),
		"none":                none,
		"None":                noneUpper,
		"PS256 header":        ps256,
		"no kid":              sign(t, testKey{priv: k1.priv}, validClaims(s.URL, c.now())),
		"empty kid":           signWith(t, testKey{priv: k1.priv}, validClaims(s.URL, c.now()), map[string]any{"kid": ""}),
		"number kid":          signWith(t, testKey{priv: k1.priv}, validClaims(s.URL, c.now()), map[string]any{"kid": 1}),
		"crit":                signWith(t, k1, validClaims(s.URL, c.now()), map[string]any{"crit": []string{"exp"}}),
		"empty crit":          signWith(t, k1, validClaims(s.URL, c.now()), map[string]any{"crit": []string{}}),
		"issuer":              mutate(func(cl *tokenClaims) { cl.Issuer = "https://other.cloudflareaccess.com" }),
		"team domain issuer":  mutate(func(cl *tokenClaims) { cl.Issuer = s.URL }),
		"other app issuer":    mutate(func(cl *tokenClaims) { cl.Issuer = s.URL + "/cdn-cgi/access/sso/oidc/other" }),
		"issuer with slash":   mutate(func(cl *tokenClaims) { cl.Issuer = testIssuer(s.URL) + "/" }),
		"no issuer":           mutate(func(cl *tokenClaims) { cl.Issuer = "" }),
		"audience":            mutate(func(cl *tokenClaims) { cl.Audience = jwt.ClaimStrings{"ffff"} }),
		"several aud":         mutate(func(cl *tokenClaims) { cl.Audience = jwt.ClaimStrings{"ffff", testClientID} }),
		"same aud twice":      mutate(func(cl *tokenClaims) { cl.Audience = jwt.ClaimStrings{testClientID, testClientID} }),
		"no audience":         mutate(func(cl *tokenClaims) { cl.Audience = nil }),
		"nonce mismatch":      mutate(func(cl *tokenClaims) { cl.Nonce = testNonce + "x" }),
		"no nonce":            mutate(func(cl *tokenClaims) { cl.Nonce = "" }),
		"expired":             mutate(func(cl *tokenClaims) { cl.ExpiresAt = jwt.NewNumericDate(c.now().Add(-31 * time.Second)) }),
		"no exp":              mutate(func(cl *tokenClaims) { cl.ExpiresAt = nil }),
		"nbf in the future":   mutate(func(cl *tokenClaims) { cl.NotBefore = jwt.NewNumericDate(c.now().Add(31 * time.Second)) }),
		"iat in the future":   mutate(func(cl *tokenClaims) { cl.IssuedAt = jwt.NewNumericDate(c.now().Add(31 * time.Second)) }),
		"no email":            mutate(func(cl *tokenClaims) { cl.Email = "" }),
		"exp in the past day": mutate(func(cl *tokenClaims) { cl.ExpiresAt = jwt.NewNumericDate(c.now().Add(-24 * time.Hour)) }),
	} {
		if _, err := v.Verify(ctx, token, testNonce); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A non-string email claim.
	numberEmail := rawToken(t, `{"alg":"RS256","typ":"JWT","kid":"k1"}`, map[string]any{"iss": testIssuer(s.URL), "aud": testClientID, "exp": c.now().Add(time.Hour).Unix(), "email": 1, "nonce": testNonce}, "")
	numberEmail = strings.TrimSuffix(numberEmail, ".")
	if sig, err = jwt.SigningMethodRS256.Sign(numberEmail, k1.priv); err != nil {
		t.Fatal(err)
	}
	if _, err = v.Verify(ctx, numberEmail+"."+b64(sig), testNonce); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("number email: %v", err)
	}

	// Within the leeway of 30 seconds.
	for name, token := range map[string]string{
		"just expired":  mutate(func(cl *tokenClaims) { cl.ExpiresAt = jwt.NewNumericDate(c.now().Add(-29 * time.Second)) }),
		"nbf soon":      mutate(func(cl *tokenClaims) { cl.NotBefore = jwt.NewNumericDate(c.now().Add(29 * time.Second)) }),
		"iat soon":      mutate(func(cl *tokenClaims) { cl.IssuedAt = jwt.NewNumericDate(c.now().Add(29 * time.Second)) }),
		"no nbf or iat": mutate(func(cl *tokenClaims) { cl.NotBefore, cl.IssuedAt = nil, nil }),
		"no sub":        mutate(func(cl *tokenClaims) { cl.Subject = "" }),
	} {
		if _, err := v.Verify(ctx, token, testNonce); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}

	// An empty expected nonce matches nothing, not even an empty claim.
	if _, err = v.Verify(ctx, mutate(func(cl *tokenClaims) { cl.Nonce = "" }), ""); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("empty nonce: %v", err)
	}
	if _, err = v.Verify(ctx, sign(t, k1, validClaims(s.URL, c.now())), ""); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("empty expected nonce: %v", err)
	}

	// The rejected algorithms and headers never fetched the keys again.
	if n := s.requests.Load(); n != 1 {
		t.Fatalf("requests: %d", n)
	}

	// The injected clock decides expiry.
	token := sign(t, k1, validClaims(s.URL, c.now()))
	c.add(time.Hour + 31*time.Second)
	if _, err = v.Verify(ctx, token, testNonce); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("expired by the clock: %v", err)
	}
	// Reasons are logged, never the tokens or the claims.
	for _, secret := range []string{"alice@example", "Alice@Example", testNonce, parts[1], parts[2]} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("log contains %q: %s", secret, logs.String())
		}
	}
	for _, reason := range []string{"access: rejected token: bad nonce", "access: rejected token: crit header", "access: rejected token: missing kid", "access: rejected token: several audiences", "access: rejected token: bad issuer", "access: rejected token: expired", "access: rejected token: missing email"} {
		if !strings.Contains(logs.String(), reason) {
			t.Fatalf("log lacks %q: %s", reason, logs.String())
		}
	}
}

func TestKeyRotation(t *testing.T) {
	k1 := newKey(t, "k1", 2048)
	k2 := newKey(t, "k2", 2048)
	s := newJWKSServer(t, k1.jwk())
	c := &clock{t: time.Unix(1_800_000_000, 0)}
	v := newVerifier(s, c)
	ctx := context.Background()

	if _, err := v.Verify(ctx, sign(t, k1, validClaims(s.URL, c.now())), testNonce); err != nil {
		t.Fatal(err)
	}
	if n := s.requests.Load(); n != 1 {
		t.Fatalf("requests: %d", n)
	}
	// Cached keys are used without fetching again.
	if _, err := v.Verify(ctx, sign(t, k1, validClaims(s.URL, c.now())), testNonce); err != nil || s.requests.Load() != 1 {
		t.Fatalf("%v, requests %d", err, s.requests.Load())
	}

	// Cloudflare rotates to k2 right after the fetch: within 30 seconds the
	// unknown kid does not fetch again.
	s.setKeys(k1.jwk(), k2.jwk())
	c.add(10 * time.Second)
	for range 3 {
		if _, err := v.Verify(ctx, sign(t, k2, validClaims(s.URL, c.now())), testNonce); !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("unknown kid within 30s: %v", err)
		}
	}
	if n := s.requests.Load(); n != 1 {
		t.Fatalf("requests within 30s: %d", n)
	}
	// After 30 seconds the unknown kid fetches the new set.
	c.add(20 * time.Second)
	if _, err := v.Verify(ctx, sign(t, k2, validClaims(s.URL, c.now())), testNonce); err != nil {
		t.Fatal(err)
	}
	if n := s.requests.Load(); n != 2 {
		t.Fatalf("requests after rotation: %d", n)
	}
	// A kid that is still unknown after fetching is rejected, and a second
	// unknown kid within 30 seconds does not fetch.
	k3 := newKey(t, "k3", 2048)
	c.add(31 * time.Second)
	if _, err := v.Verify(ctx, sign(t, k3, validClaims(s.URL, c.now())), testNonce); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("unknown kid: %v", err)
	}
	if _, err := v.Verify(ctx, sign(t, testKey{kid: "k4", priv: k3.priv}, validClaims(s.URL, c.now())), testNonce); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("second unknown kid: %v", err)
	}
	if n := s.requests.Load(); n != 3 {
		t.Fatalf("requests for unknown kids: %d", n)
	}

	// A successful fetch replaces the whole set: k1 removed by Cloudflare is
	// no longer accepted.
	s.setKeys(k2.jwk())
	c.add(2 * time.Hour)
	if _, err := v.Verify(ctx, sign(t, k2, validClaims(s.URL, c.now())), testNonce); err != nil {
		t.Fatal(err)
	}
	if n := s.requests.Load(); n != 4 {
		t.Fatalf("requests after expiry: %d", n)
	}
	if _, err := v.Verify(ctx, sign(t, k1, validClaims(s.URL, c.now())), testNonce); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("removed key: %v", err)
	}
}

func TestKeysUnavailable(t *testing.T) {
	k1 := newKey(t, "k1", 2048)
	s := newJWKSServer(t, k1.jwk())
	s.setStatus(http.StatusInternalServerError)
	c := &clock{t: time.Unix(1_800_000_000, 0)}
	v := newVerifier(s, c)
	ctx := context.Background()

	if _, err := v.Verify(ctx, sign(t, k1, validClaims(s.URL, c.now())), testNonce); !errors.Is(err, ErrKeysUnavailable) {
		t.Fatalf("empty cache: %v", err)
	}
	// The failed attempt counts for the 30 seconds as well.
	if _, err := v.Verify(ctx, sign(t, k1, validClaims(s.URL, c.now())), testNonce); !errors.Is(err, ErrKeysUnavailable) || s.requests.Load() != 1 {
		t.Fatalf("retry within 30s: %v, requests %d", err, s.requests.Load())
	}

	s.setStatus(http.StatusOK)
	c.add(31 * time.Second)
	if _, err := v.Verify(ctx, sign(t, k1, validClaims(s.URL, c.now())), testNonce); err != nil {
		t.Fatal(err)
	}

	// The keys expire and Cloudflare fails: the stale cache keeps working.
	s.setStatus(http.StatusInternalServerError)
	c.add(2 * time.Hour)
	if _, err := v.Verify(ctx, sign(t, k1, validClaims(s.URL, c.now())), testNonce); err != nil {
		t.Fatalf("stale cache: %v", err)
	}
	if n := s.requests.Load(); n != 3 {
		t.Fatalf("requests: %d", n)
	}
	// An unknown kid with a non-empty cache is an invalid token.
	c.add(time.Minute)
	k2 := newKey(t, "k2", 2048)
	if _, err := v.Verify(ctx, sign(t, k2, validClaims(s.URL, c.now())), testNonce); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("unknown kid with stale cache: %v", err)
	}

	// A JWKS that cannot be reached, is not JSON or redirects.
	for name, handler := range map[string]http.HandlerFunc{
		"not JSON": func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("<html>")) },
		"redirect": func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, oidcPath+"/jwks", http.StatusFound) },
		"too large": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"keys":[],"x":"` + strings.Repeat("a", 1<<20) + `"}`))
		},
	} {
		srv := httptest.NewServer(handler)
		noRedirect := *srv.Client()
		noRedirect.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		v := NewVerifier(testIssuer(srv.URL), testClientID, &noRedirect, c.now)
		if _, err := v.Verify(ctx, sign(t, k1, validClaims(srv.URL, c.now())), testNonce); !errors.Is(err, ErrKeysUnavailable) {
			t.Errorf("%s: %v", name, err)
		}
		srv.Close()
	}
	closed := httptest.NewServer(http.NotFoundHandler())
	closedVerifier := NewVerifier(testIssuer(closed.URL), testClientID, closed.Client(), c.now)
	closed.Close()
	if _, err := closedVerifier.Verify(ctx, sign(t, k1, validClaims(closed.URL, c.now())), testNonce); !errors.Is(err, ErrKeysUnavailable) {
		t.Fatalf("closed: %v", err)
	}
}

func TestUnusableKeys(t *testing.T) {
	// The keys that are rejected for their JWK share one private key; their
	// key IDs tell them apart.
	small := newKey(t, "small", 1024)
	shared := newKey(t, "", 2048)
	alias := func(kid string) testKey { return testKey{kid: kid, priv: shared.priv} }
	enc, ps, ec, noKty, even, one, long, huge, badN := alias("enc"), alias("ps"), alias("ec"), alias("nokty"), alias("even"), alias("one"), alias("long"), alias("huge"), alias("badn")
	dupA, dupB := alias("dup"), newKey(t, "dup", 2048)
	good := newKey(t, "good", 2048)
	padded, noUse, leadingZero := testKey{kid: "padded", priv: good.priv}, testKey{kid: "nouse", priv: good.priv}, testKey{kid: "zero", priv: good.priv}
	// 256 bytes of modulus need padding in base64.
	paddedJWK := with(padded.jwk(), map[string]any{"n": base64.URLEncoding.EncodeToString(padded.priv.N.Bytes())})
	if !strings.HasSuffix(paddedJWK["n"].(string), "=") {
		t.Fatal("the modulus is not padded")
	}
	s := newJWKSServer(t,
		small.jwk(),
		with(enc.jwk(), map[string]any{"use": "enc"}),
		with(ps.jwk(), map[string]any{"alg": "PS256"}),
		with(ec.jwk(), map[string]any{"kty": "EC"}),
		with(noKty.jwk(), map[string]any{"kty": nil}),
		with(even.jwk(), map[string]any{"e": b64(big.NewInt(65536).Bytes())}),
		with(one.jwk(), map[string]any{"e": b64([]byte{1})}),
		with(long.jwk(), map[string]any{"e": b64([]byte{0, 1, 0, 0, 1})}),
		with(huge.jwk(), map[string]any{"e": b64([]byte{0x80, 0, 0, 1})}),
		with(badN.jwk(), map[string]any{"n": "not base64url!"}),
		with(good.jwk(), map[string]any{"kid": ""}),
		with(good.jwk(), map[string]any{"kid": 1}),
		dupA.jwk(), dupB.jwk(),
		paddedJWK,
		with(noUse.jwk(), map[string]any{"use": nil, "alg": nil}),
		with(leadingZero.jwk(), map[string]any{"e": b64([]byte{0, 1, 0, 1})}),
		good.jwk(),
	)
	c := &clock{t: time.Unix(1_800_000_000, 0)}
	v := newVerifier(s, c)
	ctx := context.Background()

	for _, k := range []testKey{good, padded, noUse, leadingZero} {
		if _, err := v.Verify(ctx, sign(t, k, validClaims(s.URL, c.now())), testNonce); err != nil {
			t.Fatalf("%s: %v", k.kid, err)
		}
	}
	for _, k := range []testKey{small, enc, ps, ec, noKty, even, one, long, huge, badN, dupA, dupB} {
		if _, err := v.Verify(ctx, sign(t, k, validClaims(s.URL, c.now())), testNonce); !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("%s: %v", k.kid, err)
		}
	}

	// A set with no usable key is a failed fetch.
	s2 := newJWKSServer(t, small.jwk(), dupA.jwk(), dupB.jwk())
	v2 := newVerifier(s2, c)
	if _, err := v2.Verify(ctx, sign(t, small, validClaims(s2.URL, c.now())), testNonce); !errors.Is(err, ErrKeysUnavailable) {
		t.Fatalf("only unusable keys: %v", err)
	}
}

func TestConcurrentFirstFetch(t *testing.T) {
	k1 := newKey(t, "k1", 2048)
	s := newJWKSServer(t, k1.jwk())
	s.block = make(chan struct{})
	s.started = make(chan struct{}, 4)
	c := &clock{t: time.Unix(1_800_000_000, 0)}
	v := newVerifier(s, c)
	token := sign(t, k1, validClaims(s.URL, c.now()))

	errs := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := v.Verify(context.Background(), token, testNonce)
			errs <- err
		}()
	}
	<-s.started
	// Give the second request time to reach the fetch lock.
	time.Sleep(50 * time.Millisecond)
	close(s.block)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if n := s.requests.Load(); n != 1 {
		t.Fatalf("requests: %d", n)
	}
}

func TestStaleCacheDoesNotWait(t *testing.T) {
	k1 := newKey(t, "k1", 2048)
	k2 := newKey(t, "k2", 2048)
	s := newJWKSServer(t, k1.jwk())
	c := &clock{t: time.Unix(1_800_000_000, 0)}
	v := newVerifier(s, c)
	ctx := context.Background()
	if _, err := v.Verify(ctx, sign(t, k1, validClaims(s.URL, c.now())), testNonce); err != nil {
		t.Fatal(err)
	}

	// The cache expires; a request with an unknown kid starts a fetch that
	// hangs.
	c.add(2 * time.Hour)
	s.mu.Lock()
	s.block = make(chan struct{})
	s.started = make(chan struct{}, 4)
	s.mu.Unlock()
	unknown := sign(t, k2, validClaims(s.URL, c.now()))
	known := sign(t, k1, validClaims(s.URL, c.now()))
	errs := make(chan error, 1)
	go func() {
		_, err := v.Verify(ctx, unknown, testNonce)
		errs <- err
	}()
	<-s.started

	// A known kid in the stale cache is verified without waiting.
	done := make(chan error, 1)
	go func() {
		_, err := v.Verify(ctx, known, testNonce)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the known kid waited for the fetch")
	}
	s.mu.Lock()
	close(s.block)
	s.mu.Unlock()
	if err := <-errs; !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("unknown kid: %v", err)
	}
	if n := s.requests.Load(); n != 2 {
		t.Fatalf("requests: %d", n)
	}
}

// A fetch that was canceled by a caller would leave every waiting request
// without keys; the fetch uses its own context.
func TestFetchIgnoresCallerContext(t *testing.T) {
	k1 := newKey(t, "k1", 2048)
	s := newJWKSServer(t, k1.jwk())
	c := &clock{t: time.Unix(1_800_000_000, 0)}
	v := newVerifier(s, c)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := v.Verify(ctx, sign(t, k1, validClaims(s.URL, c.now())), testNonce); err != nil {
		t.Fatal(err)
	}
}
