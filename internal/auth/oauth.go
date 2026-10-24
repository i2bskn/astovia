package auth

import (
	"context"
	"github.com/i2bskn/astovia/internal/store"
	"log"
	"strings"
)

// Prefixes of the OAuth credentials. Like apiTokenPrefix they make each kind
// recognizable without a database lookup.
const (
	oauthAccessPrefix  = "aso_"
	oauthRefreshPrefix = "asr_"
	oauthCodePrefix    = "asc_"
	dcrClientIDPrefix  = "dcr_"
	dcrSecretPrefix    = "dcs_"
)

// MCPResource returns the protected resource (audience) of the MCP endpoint
// for the public origin issuer. It is the only place that builds it.
func MCPResource(issuer string) string { return issuer + "/mcp" }

// newPrefixed returns prefix followed by Random() and the hash of the whole
// value.
func newPrefixed(prefix string) (raw, hash string, err error) {
	r, err := Random()
	if err != nil {
		return "", "", err
	}
	raw = prefix + r
	return raw, Hash(raw), nil
}

// isPrefixed reports whether v is prefix followed by 43 base64url characters
// that decode to 32 bytes, the form produced by newPrefixed.
func isPrefixed(v, prefix string) bool {
	return len(v) == len(prefix)+43 && strings.HasPrefix(v, prefix) && validToken(v[len(prefix):])
}

// NewOAuthAccessToken returns a new OAuth access token and its hash.
func NewOAuthAccessToken() (raw, hash string, err error) { return newPrefixed(oauthAccessPrefix) }

// NewOAuthRefreshToken returns a new OAuth refresh token and its hash.
func NewOAuthRefreshToken() (raw, hash string, err error) { return newPrefixed(oauthRefreshPrefix) }

// NewOAuthCode returns a new authorization code and its hash.
func NewOAuthCode() (raw, hash string, err error) { return newPrefixed(oauthCodePrefix) }

// NewDCRClientID returns a new client_id for Dynamic Client Registration.
// The client_id is public; the hash is not used.
func NewDCRClientID() (string, error) {
	raw, _, err := newPrefixed(dcrClientIDPrefix)
	return raw, err
}

// NewDCRClientSecret returns a new client secret and its hash.
func NewDCRClientSecret() (raw, hash string, err error) { return newPrefixed(dcrSecretPrefix) }

// IsOAuthAccessToken reports whether v has the form of an OAuth access token.
func IsOAuthAccessToken(v string) bool { return isPrefixed(v, oauthAccessPrefix) }

// IsOAuthRefreshToken reports whether v has the form of an OAuth refresh token.
func IsOAuthRefreshToken(v string) bool { return isPrefixed(v, oauthRefreshPrefix) }

// IsOAuthCode reports whether v has the form of an authorization code.
func IsOAuthCode(v string) bool { return isPrefixed(v, oauthCodePrefix) }

// IsDCRClientID reports whether v has the form of a DCR client_id.
func IsDCRClientID(v string) bool { return isPrefixed(v, dcrClientIDPrefix) }

// AuthenticateOAuthAccess returns the user of an OAuth access token. The
// token must have been issued for this server's MCP resource: changing the
// public URL invalidates existing tokens. A failure to record the time of use
// is logged and does not fail authentication.
func (s *Service) AuthenticateOAuthAccess(ctx context.Context, raw string) (store.User, error) {
	if !IsOAuthAccessToken(raw) {
		return store.User{}, store.ErrUnauthorized
	}
	u, g, touchErr, err := s.Store.AuthenticateOAuthAccess(ctx, Hash(raw), s.Now().Unix())
	if err != nil {
		return store.User{}, err
	}
	if g.Resource != MCPResource(s.issuer) {
		return store.User{}, store.ErrUnauthorized
	}
	if touchErr != nil {
		log.Printf("oauth token: touch user=%s: %v", u.ID, touchErr)
	}
	return u, nil
}
