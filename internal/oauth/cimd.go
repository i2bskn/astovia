package oauth

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Limits of Client ID Metadata Document fetching. /oauth/authorize is
// unauthenticated, so it makes the server send GET requests to URLs chosen by
// anyone; the client below only reaches public addresses, never follows
// redirects and reads a bounded body within a bounded time.
const (
	CIMDBodyLimit       = 64 << 10
	cimdTimeout         = 5 * time.Second
	cimdDefaultCacheTTL = time.Hour
	cimdMaxCacheTTL     = 24 * time.Hour
)

// errBlockedAddress is returned when the resolved address is not public.
var errBlockedAddress = errors.New("destination address is not allowed")

// Document is the validated part of a Client ID Metadata Document. TTL is how
// long it may be cached.
type Document struct {
	Name         string
	RedirectURIs []string
	TTL          time.Duration
}

var deniedIPv4 = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("255.255.255.255/32"),
}

var globalIPv6 = netip.MustParsePrefix("2000::/3")

var deniedIPv6 = []netip.Prefix{
	netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("3fff::/20"),
}

// AllowedCIMDAddr reports whether a metadata document may be fetched from a.
// It is an allow list: global unicast IPv4 outside private and special-use
// ranges, and IPv6 in 2000::/3 outside special-use ranges. Everything else
// (loopback, private, link-local, CGNAT, multicast, NAT64, ...) is refused.
// IPv4-mapped IPv6 addresses are checked as IPv4.
func AllowedCIMDAddr(a netip.Addr) bool {
	a = a.Unmap().WithZone("")
	if a.Is4() {
		if !a.IsGlobalUnicast() || a.IsPrivate() {
			return false
		}
		for _, p := range deniedIPv4 {
			if p.Contains(a) {
				return false
			}
		}
		return true
	}
	if !a.Is6() || !globalIPv6.Contains(a) {
		return false
	}
	for _, p := range deniedIPv6 {
		if p.Contains(a) {
			return false
		}
	}
	return true
}

// NewCIMDClient returns the HTTP client used to fetch metadata documents.
func NewCIMDClient() *http.Client {
	return newCIMDClient(AllowedCIMDAddr, nil)
}

// newCIMDClient checks the address of every connection after name
// resolution, so a public name that resolves to a private address is refused
// as well. Proxy environment variables are ignored because a proxy would
// bypass the check. Tests pass their own allow function and TLS config.
func newCIMDClient(allow func(netip.Addr) bool, tlsConfig *tls.Config) *http.Client {
	dialer := &net.Dialer{Timeout: cimdTimeout, Control: func(_, address string, _ syscall.RawConn) error {
		ap, err := netip.ParseAddrPort(address)
		if err != nil || !allow(ap.Addr()) {
			return errBlockedAddress
		}
		return nil
	}}
	return &http.Client{
		Timeout: cimdTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Transport: &http.Transport{
			Proxy:                 nil,
			DialContext:           dialer.DialContext,
			TLSClientConfig:       tlsConfig,
			TLSHandshakeTimeout:   cimdTimeout,
			ResponseHeaderTimeout: cimdTimeout,
			DisableKeepAlives:     true,
		},
	}
}

// FetchCIMD fetches and validates the metadata document of clientID. Only a
// 200 response with a JSON object of at most CIMDBodyLimit bytes is accepted;
// redirects are failures.
func FetchCIMD(ctx context.Context, client *http.Client, clientID string) (Document, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, clientID, nil)
	if err != nil {
		return Document{}, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return Document{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return Document{}, fmt.Errorf("status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, CIMDBodyLimit+1))
	if err != nil {
		return Document{}, err
	}
	if len(body) > CIMDBodyLimit {
		return Document{}, errors.New("document is too large")
	}
	doc, err := parseCIMD(clientID, body)
	if err != nil {
		return Document{}, err
	}
	doc.TTL = cacheTTL(resp.Header.Get("Cache-Control"))
	return doc, nil
}

// parseCIMD validates a document. Its token_endpoint_auth_method is not read:
// metadata document clients are always public clients (none).
func parseCIMD(clientID string, body []byte) (Document, error) {
	var in struct {
		ClientID     string   `json:"client_id"`
		ClientName   string   `json:"client_name"`
		RedirectURIs []string `json:"redirect_uris"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		return Document{}, err
	}
	if in.ClientID != clientID {
		return Document{}, errors.New("client_id does not match the document URL")
	}
	name := ClientName(in.ClientName, "")
	if name == "" {
		return Document{}, errors.New("client_name is missing")
	}
	if err := ValidateRedirectURIs(in.RedirectURIs); err != nil {
		return Document{}, err
	}
	return Document{Name: name, RedirectURIs: in.RedirectURIs}, nil
}

// cacheTTL returns the positive max-age of a Cache-Control header, capped at
// 24 hours, or one hour when there is none.
func cacheTTL(cacheControl string) time.Duration {
	for _, d := range strings.Split(cacheControl, ",") {
		name, value, ok := strings.Cut(strings.TrimSpace(d), "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(name), "max-age") {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err != nil || n <= 0 {
			return cimdDefaultCacheTTL
		}
		if n >= int64(cimdMaxCacheTTL/time.Second) {
			return cimdMaxCacheTTL
		}
		return time.Duration(n) * time.Second
	}
	return cimdDefaultCacheTTL
}
