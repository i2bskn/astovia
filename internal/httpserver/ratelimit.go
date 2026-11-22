package httpserver

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// loginLimiter limits login attempts per peer in a bounded in-process table.
// The peer is the one clientAddr returns: RemoteAddr, or the client address
// in X-Forwarded-For when RemoteAddr is a trusted proxy
// (ASTOVIA_TRUSTED_PROXIES).
type loginLimiter struct {
	mu       sync.Mutex
	entries  map[string]bucket
	now      func() time.Time
	limit    int
	window   time.Duration
	capacity int
}

type bucket struct {
	start time.Time
	count int
}

func newLoginLimiter() *loginLimiter {
	return newKeyLimiter(10, time.Minute)
}

// newKeyLimiter returns a limiter that allows limit attempts per key in each
// window. Keys are given to allowKey; allow derives them from the peer.
func newKeyLimiter(limit int, window time.Duration) *loginLimiter {
	return &loginLimiter{entries: make(map[string]bucket), now: time.Now, limit: limit, window: window, capacity: 4096}
}

// maxForwardedEntries is how many X-Forwarded-For entries clientAddr reads,
// from the right.
const maxForwardedEntries = 32

// clientAddr returns the client address of r for the per-peer limiters.
// When RemoteAddr is not one of the trusted proxies, it is returned as-is
// and X-Forwarded-For is not read. Otherwise the X-Forwarded-For entries of
// every header line are read from the right, skipping trusted proxies, and
// the first untrusted address is returned without a port. RemoteAddr is
// returned when there is no header, when an entry cannot be parsed (it may
// be forged), and when every entry read is a trusted proxy: the leftmost
// entry is written by the client and is never used as the key. Only the
// rightmost maxForwardedEntries entries are read. Forwarded and X-Real-IP
// are not read.
func (s *Server) clientAddr(r *http.Request) string {
	trusted := s.cfg.TrustedProxies
	if len(trusted) == 0 {
		return r.RemoteAddr
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer, err := netip.ParseAddr(host)
	if err != nil || !trustedAddr(trusted, peer.Unmap()) {
		return r.RemoteAddr
	}
	var entries []string
	for _, v := range r.Header.Values("X-Forwarded-For") {
		entries = append(entries, strings.Split(v, ",")...)
	}
	if len(entries) > maxForwardedEntries {
		entries = entries[len(entries)-maxForwardedEntries:]
	}
	for i := len(entries) - 1; i >= 0; i-- {
		addr, ok := forwardedAddr(entries[i])
		if !ok {
			return r.RemoteAddr
		}
		if !trustedAddr(trusted, addr) {
			return addr.String()
		}
	}
	return r.RemoteAddr
}

// forwardedAddr parses an X-Forwarded-For entry: an IP address, or an
// address with a port (1.2.3.4:5678 or [2001:db8::1]:443). Zones are
// rejected, and IPv4-mapped IPv6 addresses are read as IPv4.
func forwardedAddr(entry string) (netip.Addr, bool) {
	entry = strings.TrimSpace(entry)
	addr, err := netip.ParseAddr(entry)
	if err != nil {
		ap, err := netip.ParseAddrPort(entry)
		if err != nil {
			return netip.Addr{}, false
		}
		addr = ap.Addr()
	}
	if addr.Zone() != "" {
		return netip.Addr{}, false
	}
	return addr.Unmap(), true
}

// trustedAddr reports whether addr is in one of the trusted prefixes.
func trustedAddr(trusted []netip.Prefix, addr netip.Addr) bool {
	for _, p := range trusted {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// limiterKey returns the IPv4 address itself, or the /64 prefix for IPv6.
// IPv4-mapped IPv6 addresses are treated as IPv4. Unparseable hosts are used
// as-is.
func limiterKey(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return host
	}
	addr = addr.Unmap()
	if addr.Is4() {
		return addr.String()
	}
	prefix, err := addr.Prefix(64)
	if err != nil {
		return host
	}
	return prefix.Masked().String()
}

// allow records an attempt from remoteAddr and reports whether it is within
// the limit. When the table is full, an entry is evicted instead of rejecting
// new peers, so that filling the table cannot lock everyone out.
func (l *loginLimiter) allow(remoteAddr string) bool {
	return l.allowKey(limiterKey(remoteAddr))
}

// allowKey records an attempt for key, such as a user ID, and reports
// whether it is within the limit.
func (l *loginLimiter) allowKey(key string) bool { return l.allowKeyN(key, 1) }

// allowKeyN records n units for key and reports whether they are within the
// limit. Units over the limit are not recorded; n <= 0 records nothing and
// is allowed.
func (l *loginLimiter) allowKeyN(key string, n int) bool {
	if n <= 0 {
		return true
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, b := range l.entries {
		if now.Sub(b.start) >= l.window {
			delete(l.entries, k)
		}
	}
	b, ok := l.entries[key]
	if !ok {
		if len(l.entries) >= l.capacity {
			l.evict()
		}
		b = bucket{start: now}
	}
	if b.count+n > l.limit {
		return false
	}
	b.count += n
	l.entries[key] = b
	return true
}

// evict removes the oldest entry that is still under the limit. If every
// entry is limited, it removes the oldest entry overall. The caller holds mu.
func (l *loginLimiter) evict() {
	var under, oldest string
	var underStart, oldestStart time.Time
	underFound, oldestFound := false, false
	for k, b := range l.entries {
		if b.count < l.limit && (!underFound || b.start.Before(underStart)) {
			under, underStart, underFound = k, b.start, true
		}
		if !oldestFound || b.start.Before(oldestStart) {
			oldest, oldestStart, oldestFound = k, b.start, true
		}
	}
	if underFound {
		delete(l.entries, under)
	} else if oldestFound {
		delete(l.entries, oldest)
	}
}
