// Package oidc signs users in with an OpenID Connect provider: it reads the
// provider metadata (discovery), builds the authorization URL, exchanges the
// authorization code for an ID token and verifies the ID token against the
// signing keys of the provider.
package oidc

import (
	"context"
	"crypto/rsa"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

var (
	// ErrInvalidToken is returned for every ID token that is not accepted.
	// The reason is only logged.
	ErrInvalidToken = errors.New("invalid ID token")
	// ErrKeysUnavailable is returned when no signing keys could be fetched.
	ErrKeysUnavailable = errors.New("OIDC signing keys are unavailable")
)

const (
	// maxTokenSize bounds the token before it is parsed (16 KiB).
	maxTokenSize = 16 << 10
	// maxKeysSize bounds the JWKS response (1 MiB).
	maxKeysSize = 1 << 20
	// keysTTL is how long fetched keys are used without fetching again.
	keysTTL = time.Hour
	// fetchInterval is the minimum time between two fetch attempts,
	// successful or not, so that tokens with unknown key IDs cannot make
	// many requests to the provider.
	fetchInterval = 30 * time.Second
	// fetchTimeout bounds one fetch of the JWKS.
	fetchTimeout = 5 * time.Second
	// leeway is the clock skew allowed for exp, nbf and iat.
	leeway = 30 * time.Second
	// minRSABits is the smallest accepted RSA modulus.
	minRSABits = 2048
	// maxRSAExponent is the largest accepted RSA public exponent (2^31-1).
	maxRSAExponent = 1<<31 - 1
)

// Verifier verifies the ID tokens that one issuer issues to one client
// against the signing keys of the issuer. Keys are fetched on the first
// request and cached; there is one cache, whatever the keys URL.
type Verifier struct {
	issuer   string
	audience string
	client   *http.Client
	now      func() time.Time

	// mu guards keys and fetchedAt. It is never held during a fetch.
	mu        sync.RWMutex
	keys      map[string]*rsa.PublicKey
	fetchedAt time.Time

	// fetchMu serializes fetches and guards attemptedAt.
	fetchMu     sync.Mutex
	attemptedAt time.Time
}

// NewVerifier returns a verifier for the ID tokens that issuer issues to
// clientID. The keys are read with client, which should not follow
// redirects, from the keys URL given to Verify (the jwks_uri of the
// provider metadata).
func NewVerifier(issuer, clientID string, client *http.Client, now func() time.Time) *Verifier {
	return &Verifier{issuer: issuer, audience: clientID, client: client, now: now}
}

// Identity is what a verified ID token says about the user.
type Identity struct {
	// Email is the email claim, unchanged; normalization is up to the
	// caller.
	Email string
	// EmailVerified is nil when the claim is absent, otherwise whether it is the JSON boolean true.
	EmailVerified *bool
}

// idClaims holds the claims read besides the registered ones. sub is not
// read: users are matched by their email address. email_verified is read
// raw, so that a value that is not a boolean is rejected by the caller
// instead of failing the whole parse.
type idClaims struct {
	jwt.RegisteredClaims
	Email           string          `json:"email"`
	Nonce           string          `json:"nonce"`
	AuthorizedParty *string         `json:"azp"`
	EmailVerified   json.RawMessage `json:"email_verified"`
}

// Verify checks the signature and the claims of the ID token, including that
// its nonce equals nonce, and returns its email claim unchanged with its
// email_verified claim. The signing keys are read from keysURL. It returns
// ErrKeysUnavailable when no signing keys could be fetched and
// ErrInvalidToken otherwise. Keys are fetched with their own timeout
// instead of the request context, so that a client going away does not
// cancel a fetch that other requests wait for.
func (v *Verifier) Verify(_ context.Context, token, nonce, keysURL string) (Identity, error) {
	if len(token) == 0 || len(token) > maxTokenSize {
		return Identity{}, reject("bad size")
	}
	var claims idClaims
	var keyErr error
	parsed, err := jwt.ParseWithClaims(token, &claims, func(t *jwt.Token) (any, error) {
		// WithValidMethods has already checked the alg header; the method is
		// checked again so that no other key type is ever returned.
		if t.Method == nil || t.Method.Alg() != "RS256" {
			keyErr = reject("not RS256")
			return nil, keyErr
		}
		if _, ok := t.Header["crit"]; ok {
			keyErr = reject("crit header")
			return nil, keyErr
		}
		kid, ok := t.Header["kid"].(string)
		if !ok || kid == "" {
			keyErr = reject("missing kid")
			return nil, keyErr
		}
		key, err := v.key(kid, keysURL)
		if err != nil {
			keyErr = err
			return nil, err
		}
		return key, nil
	}, jwt.WithValidMethods([]string{"RS256"}), jwt.WithIssuer(v.issuer), jwt.WithAudience(v.audience), jwt.WithExpirationRequired(), jwt.WithIssuedAt(), jwt.WithLeeway(leeway), jwt.WithTimeFunc(v.now))
	if keyErr != nil {
		if errors.Is(keyErr, ErrKeysUnavailable) {
			return Identity{}, ErrKeysUnavailable
		}
		return Identity{}, ErrInvalidToken
	}
	if err != nil || parsed == nil || !parsed.Valid {
		return Identity{}, reject(parseReason(err))
	}
	if len(claims.Audience) != 1 {
		return Identity{}, reject("several audiences")
	}
	// An authorized party, when present, must be this client.
	if claims.AuthorizedParty != nil && *claims.AuthorizedParty != v.audience {
		return Identity{}, reject("bad azp")
	}
	if nonce == "" || subtle.ConstantTimeCompare([]byte(claims.Nonce), []byte(nonce)) != 1 {
		return Identity{}, reject("bad nonce")
	}
	if claims.Email == "" {
		return Identity{}, reject("missing email")
	}
	return Identity{Email: claims.Email, EmailVerified: emailVerified(claims.EmailVerified)}, nil
}

// emailVerified reads the raw email_verified claim: nil when it is absent,
// a pointer to true when it is the JSON boolean true and a pointer to false
// for any other value, including false, null and the string "true".
func emailVerified(raw json.RawMessage) *bool {
	if raw == nil {
		return nil
	}
	verified := string(raw) == "true"
	return &verified
}

// parseReason names the failure of jwt.ParseWithClaims for the log. The
// error itself is not logged: it may quote the claims.
func parseReason(err error) string {
	switch {
	case errors.Is(err, jwt.ErrTokenMalformed):
		return "malformed"
	case errors.Is(err, jwt.ErrTokenSignatureInvalid):
		return "bad signature"
	case errors.Is(err, jwt.ErrTokenUnverifiable):
		return "unverifiable or not RS256"
	case errors.Is(err, jwt.ErrTokenInvalidIssuer):
		return "bad issuer"
	case errors.Is(err, jwt.ErrTokenInvalidAudience):
		return "bad audience"
	case errors.Is(err, jwt.ErrTokenRequiredClaimMissing):
		return "missing exp"
	case errors.Is(err, jwt.ErrTokenExpired):
		return "expired"
	case errors.Is(err, jwt.ErrTokenNotValidYet):
		return "not valid yet"
	case errors.Is(err, jwt.ErrTokenUsedBeforeIssued):
		return "issued in the future"
	default:
		return "invalid claims"
	}
}

// reject logs the reason, never the token or its claims, and returns
// ErrInvalidToken.
func reject(reason string) error {
	log.Printf("oidc: rejected token: %s", reason)
	return ErrInvalidToken
}

// key returns the public key kid, fetching the keys from keysURL when the
// cache has expired or does not know kid.
func (v *Verifier) key(kid, keysURL string) (*rsa.PublicKey, error) {
	k, known, fresh := v.lookup(kid)
	if known && fresh {
		return k, nil
	}
	if known {
		// The cached key still verifies; refresh only if no other request is
		// already fetching, and never wait for it.
		if v.fetchMu.TryLock() {
			v.refreshLocked("", keysURL)
			v.fetchMu.Unlock()
		}
	} else {
		// Wait for a fetch in progress: it may bring kid.
		v.fetchMu.Lock()
		v.refreshLocked(kid, keysURL)
		v.fetchMu.Unlock()
	}
	v.mu.RLock()
	defer v.mu.RUnlock()
	if k, ok := v.keys[kid]; ok {
		return k, nil
	}
	if len(v.keys) == 0 {
		return nil, ErrKeysUnavailable
	}
	return nil, reject("unknown kid")
}

// lookup returns the cached key kid, whether it is cached and whether the
// cache is within keysTTL.
func (v *Verifier) lookup(kid string) (*rsa.PublicKey, bool, bool) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	k, ok := v.keys[kid]
	return k, ok, !v.fetchedAt.IsZero() && v.now().Sub(v.fetchedAt) < keysTTL
}

// refreshLocked fetches the keys from keysURL unless the last attempt was less than
// fetchInterval ago or, when kid is not empty, kid has meanwhile become
// known. On success the cached keys are replaced as a whole; on failure they
// are kept. fetchMu must be held.
func (v *Verifier) refreshLocked(kid, keysURL string) {
	now := v.now()
	if !v.attemptedAt.IsZero() && now.Sub(v.attemptedAt) < fetchInterval {
		return
	}
	if kid != "" {
		if _, ok, _ := v.lookup(kid); ok {
			return
		}
	}
	v.attemptedAt = now
	keys, err := v.fetch(keysURL)
	if err != nil {
		log.Printf("oidc: fetch signing keys: %v", err)
		return
	}
	v.mu.Lock()
	v.keys = keys
	v.fetchedAt = now
	v.mu.Unlock()
}

// jwk is the part of a JSON Web Key that is read.
type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	N   string `json:"n"`
	E   string `json:"e"`
}

// fetch reads the JWKS at keysURL and returns its RS256 signing keys
// of at least minRSABits by key ID. Keys that cannot be used are skipped; a
// key ID that appears more than once is ambiguous and skipped as a whole.
func (v *Verifier) fetch(keysURL string) (map[string]*rsa.PublicKey, error) {
	ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, keysURL, nil)
	if err != nil {
		return nil, errors.New("cannot build the request")
	}
	req.Header.Set("Accept", "application/json")
	res, err := v.client.Do(req)
	if err != nil {
		// The error names the URL, which is public provider metadata.
		return nil, err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d", res.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, maxKeysSize+1))
	if err != nil {
		return nil, errors.New("cannot read the response")
	}
	if len(body) > maxKeysSize {
		return nil, errors.New("response is larger than 1 MiB")
	}
	// Each key is parsed on its own so that one unsupported key does not
	// discard the others.
	var set struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err = json.Unmarshal(body, &set); err != nil {
		return nil, errors.New("response is not a JWKS")
	}
	seen := map[string]int{}
	parsed := map[string]*rsa.PublicKey{}
	for _, raw := range set.Keys {
		var k jwk
		if json.Unmarshal(raw, &k) != nil || k.Kid == "" {
			continue
		}
		seen[k.Kid]++
		if pub, ok := rsaKey(k); ok {
			parsed[k.Kid] = pub
		}
	}
	keys := map[string]*rsa.PublicKey{}
	for kid, pub := range parsed {
		if seen[kid] == 1 {
			keys[kid] = pub
		}
	}
	// An empty set would reject every token; keep the cached keys instead.
	if len(keys) == 0 {
		return nil, errors.New("no usable RS256 keys")
	}
	return keys, nil
}

// rsaKey returns the public key of k when it is an RSA signing key for
// RS256 with a modulus of at least minRSABits and an odd exponent from 3 to
// maxRSAExponent.
func rsaKey(k jwk) (*rsa.PublicKey, bool) {
	if k.Kty != "RSA" || (k.Use != "" && k.Use != "sig") || (k.Alg != "" && k.Alg != "RS256") {
		return nil, false
	}
	n, err := decodeKeyPart(k.N)
	if err != nil || len(n) == 0 {
		return nil, false
	}
	e, err := decodeKeyPart(k.E)
	if err != nil || len(e) == 0 || len(e) > 4 {
		return nil, false
	}
	exponent := new(big.Int).SetBytes(e).Int64()
	if exponent < 3 || exponent%2 == 0 || exponent > maxRSAExponent {
		return nil, false
	}
	pub := &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(exponent)}
	if pub.N.BitLen() < minRSABits {
		return nil, false
	}
	return pub, true
}

// decodeKeyPart decodes a base64url member of a JWK. Padding is tolerated
// and removed first, as other JOSE implementations do.
func decodeKeyPart(v string) ([]byte, error) {
	v = strings.TrimRight(v, "=")
	if v == "" {
		return nil, errors.New("empty")
	}
	return base64.RawURLEncoding.Strict().DecodeString(v)
}
