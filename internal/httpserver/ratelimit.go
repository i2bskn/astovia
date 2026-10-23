package httpserver

import (
	"net"
	"net/netip"
	"sync"
	"time"
)

// loginLimiter limits login attempts per peer in a bounded in-process table.
// Forwarded headers are never trusted; the key is derived from RemoteAddr.
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
	return &loginLimiter{entries: make(map[string]bucket), now: time.Now, limit: 10, window: time.Minute, capacity: 4096}
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
	key := limiterKey(remoteAddr)
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
	if b.count >= l.limit {
		return false
	}
	b.count++
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
