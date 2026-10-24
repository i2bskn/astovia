// Package oauth holds the protocol rules of the built-in OAuth authorization
// server for /mcp: scopes, redirect URI rules, PKCE, the signed consent
// request and Client ID Metadata Document fetching. It has no storage or HTTP
// handlers; internal/httpserver uses it with internal/store.
package oauth

import (
	"errors"
	"github.com/i2bskn/astovia/internal/auth"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Scopes. mcp grants access to /mcp; offline_access adds a refresh token.
const (
	ScopeMCP           = "mcp"
	ScopeOfflineAccess = "offline_access"
)

// Lifetimes.
const (
	RequestLifetime = 10 * time.Minute
	CodeLifetime    = 60 * time.Second
	// AccessLifetime applies when a refresh token is issued as well.
	AccessLifetime = time.Hour
	// LongAccessLifetime applies without offline_access. ChatGPT does not ask
	// for offline_access, and an hour would make it reconnect every hour.
	LongAccessLifetime = 7 * 24 * time.Hour
	// RefreshLifetime counts from each rotation, so a connection in use is
	// extended.
	RefreshLifetime = 90 * 24 * time.Hour
)

// Length limits of request parameters. The consent request carries them in a
// URL, so they are bounded.
const (
	MaxClientIDLength    = 512
	MaxRedirectURILength = 512
	MaxStateLength       = 512
	MaxScopeLength       = 256
	MaxResourceLength    = 512
	MaxClientNameLength  = 128
	MaxRedirectURIs      = 8
)

// ErrInvalidGrant and ErrInvalidScope are returned by the token issuing
// callbacks when a grant does not match the token request.
var (
	ErrInvalidGrant = errors.New("invalid grant")
	ErrInvalidScope = errors.New("invalid scope")
)

// NormalizeScope validates a space-separated scope. An empty scope means mcp.
// Only mcp and offline_access are known, and mcp is required because a token
// without it cannot be used anywhere. The result has no duplicates and lists
// mcp before offline_access.
func NormalizeScope(raw string) (string, error) {
	fields := strings.Fields(raw)
	if len(fields) == 0 {
		return ScopeMCP, nil
	}
	var mcp, offline bool
	for _, f := range fields {
		switch f {
		case ScopeMCP:
			mcp = true
		case ScopeOfflineAccess:
			offline = true
		default:
			return "", ErrInvalidScope
		}
	}
	if !mcp {
		return "", ErrInvalidScope
	}
	if offline {
		return ScopeMCP + " " + ScopeOfflineAccess, nil
	}
	return ScopeMCP, nil
}

// HasScope reports whether the space-separated scope contains v.
func HasScope(scope, v string) bool {
	for _, f := range strings.Fields(scope) {
		if f == v {
			return true
		}
	}
	return false
}

// ScopeSubset reports whether every scope in requested is in granted.
func ScopeSubset(requested, granted string) bool {
	for _, f := range strings.Fields(requested) {
		if !HasScope(granted, f) {
			return false
		}
	}
	return true
}

// MatchResource reports whether v names this server's MCP resource. The
// resource itself and the bare origin are accepted, each with or without one
// trailing slash; /mcp is the only protected resource, so both mean it.
func MatchResource(issuer, v string) bool {
	v = strings.TrimSuffix(v, "/")
	return v == auth.MCPResource(issuer) || v == issuer
}

// ClientKind returns "cimd" for a metadata document URL and "dcr" otherwise.
func ClientKind(clientID string) string {
	if strings.HasPrefix(clientID, "https://") {
		return "cimd"
	}
	return "dcr"
}

// ClientName cleans a client-supplied display name: format and control
// characters and line or paragraph separators are removed, surrounding spaces
// are trimmed, fallback is used when nothing is left, and the result is cut
// to MaxClientNameLength runes.
func ClientName(raw, fallback string) string {
	name := strings.Map(func(r rune) rune {
		if unicode.In(r, unicode.Cc, unicode.Cf, unicode.Zl, unicode.Zp) {
			return -1
		}
		return r
	}, raw)
	name = strings.TrimFunc(name, unicode.IsSpace)
	if name == "" {
		name = fallback
	}
	if utf8.RuneCountInString(name) > MaxClientNameLength {
		name = string([]rune(name)[:MaxClientNameLength])
	}
	return name
}

// BuildRedirectURL adds the non-empty params to the query of redirectURI,
// keeping its existing query parameters.
func BuildRedirectURL(redirectURI string, params map[string]string) (string, error) {
	u, err := url.Parse(redirectURI)
	if err != nil {
		return "", err
	}
	q := u.Query()
	for k, v := range params {
		if v != "" {
			q.Set(k, v)
		}
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}
