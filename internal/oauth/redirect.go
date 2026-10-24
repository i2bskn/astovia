package oauth

import (
	"errors"
	"github.com/i2bskn/astovia/internal/config"
	"net/netip"
	"net/url"
	"strings"
)

// ErrInvalidRedirectURI is returned for redirect_uris that break the
// registration rules.
var ErrInvalidRedirectURI = errors.New("invalid redirect_uri")

// parseURL parses an absolute URL used as a client identifier or redirect
// URI. Only printable ASCII is accepted, and the host must be a lower-case
// ASCII DNS name, an IPv4 address or a bracketed IPv6 address, so that the
// host shown on the consent page is exactly the one the browser goes to.
// Userinfo, fragments, percent-encoded hosts and IPv6 zones are rejected.
func parseURL(raw string) (*url.URL, bool) {
	for i := 0; i < len(raw); i++ {
		if c := raw[i]; c <= 0x20 || c >= 0x7f {
			return nil, false
		}
	}
	if strings.Contains(raw, "#") {
		return nil, false
	}
	// url.Parse decodes percent-encoded hosts, so look at the raw authority.
	if _, rest, ok := strings.Cut(raw, "://"); ok {
		if i := strings.IndexAny(rest, "/?"); i >= 0 {
			rest = rest[:i]
		}
		if strings.Contains(rest, "%") {
			return nil, false
		}
	}
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" || u.User != nil || u.Host == "" {
		return nil, false
	}
	return u, validHost(u)
}

// validHost applies the host rules of config's ASTOVIA_PUBLIC_URL check
// (canonicalOrigin): a DNS name that config.ValidDNSHost accepts, an IPv4
// address, or a bracketed IPv6 address without zone that is not IPv4-mapped.
func validHost(u *url.URL) bool {
	host := u.Hostname()
	if strings.HasPrefix(u.Host, "[") {
		ip, err := netip.ParseAddr(host)
		return err == nil && ip.Is6() && !ip.Is4In6() && ip.Zone() == ""
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		return ip.Is4()
	}
	return config.ValidDNSHost(host)
}

// isLoopbackHost reports whether host is localhost or a loopback IP address.
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.IsLoopback()
}

// loopbackHTTP returns the parsed URL when raw is a valid http URL on a
// loopback host.
func loopbackHTTP(raw string) (*url.URL, bool) {
	u, ok := parseURL(raw)
	if !ok || u.Scheme != "http" || !isLoopbackHost(u.Hostname()) {
		return nil, false
	}
	return u, true
}

// ValidRedirectURI reports whether raw may be registered as a redirect URI:
// an https URL, or an http URL on a loopback host. Custom schemes are not
// accepted; the consent page navigates to the URI with window.location.
func ValidRedirectURI(raw string) bool {
	if raw == "" || len(raw) > MaxRedirectURILength {
		return false
	}
	u, ok := parseURL(raw)
	if !ok {
		return false
	}
	return u.Scheme == "https" || u.Scheme == "http" && isLoopbackHost(u.Hostname())
}

// ValidateRedirectURIs checks the redirect_uris of a registration: 1 to
// MaxRedirectURIs values that each pass ValidRedirectURI.
func ValidateRedirectURIs(uris []string) error {
	if len(uris) == 0 || len(uris) > MaxRedirectURIs {
		return ErrInvalidRedirectURI
	}
	for _, u := range uris {
		if !ValidRedirectURI(u) {
			return ErrInvalidRedirectURI
		}
	}
	return nil
}

// RedirectURIMatches reports whether the requested redirect URI matches a
// registered one. They must be equal, except that for http loopback URIs the
// port is ignored (RFC 8252 section 7.3): native clients such as Claude Code
// listen on a random port and register the URI without one. The host name
// must still be the same, so localhost and 127.0.0.1 differ. https URIs never
// ignore the port.
func RedirectURIMatches(registered, requested string) bool {
	if registered == requested {
		return true
	}
	reg, ok := loopbackHTTP(registered)
	if !ok {
		return false
	}
	req, ok := loopbackHTTP(requested)
	if !ok {
		return false
	}
	return reg.Scheme == req.Scheme && reg.Hostname() == req.Hostname() && reg.EscapedPath() == req.EscapedPath() && reg.RawQuery == req.RawQuery && reg.ForceQuery == req.ForceQuery
}

// MatchRedirectURI reports whether requested matches any registered URI.
func MatchRedirectURI(registered []string, requested string) bool {
	for _, r := range registered {
		if RedirectURIMatches(r, requested) {
			return true
		}
	}
	return false
}

// RedirectHost returns the host name of a redirect URI or client_id URL.
func RedirectHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// IsLoopbackRedirect reports whether raw goes to a loopback host.
func IsLoopbackRedirect(raw string) bool {
	return isLoopbackHost(RedirectHost(raw))
}

// ValidCIMDClientID reports whether clientID can identify a Client ID
// Metadata Document: an https URL with a path other than "/", no query and
// the host rules of parseURL.
func ValidCIMDClientID(clientID string) bool {
	if len(clientID) > MaxClientIDLength || !strings.HasPrefix(clientID, "https://") || strings.Contains(clientID, "?") {
		return false
	}
	u, ok := parseURL(clientID)
	if !ok || u.Scheme != "https" {
		return false
	}
	p := u.EscapedPath()
	return p != "" && p != "/"
}
