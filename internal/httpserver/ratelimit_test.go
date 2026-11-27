package httpserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/i2bskn/astovia/internal/auth"
	"github.com/i2bskn/astovia/internal/config"
	"github.com/i2bskn/astovia/internal/store"
	"github.com/i2bskn/astovia/internal/testutil"
)

func TestLoginLimiterEvictsWhenFull(t *testing.T) {
	l := newLoginLimiter()
	l.capacity = 3
	base := time.Date(2026, time.October, 23, 0, 0, 0, 0, time.UTC)
	now := base
	l.now = func() time.Time { return now }

	// a is limited and oldest; b and c are under the limit.
	l.entries["192.0.2.1"] = bucket{start: base, count: l.limit}
	l.entries["192.0.2.2"] = bucket{start: base.Add(time.Second), count: 1}
	l.entries["192.0.2.3"] = bucket{start: base.Add(2 * time.Second), count: 1}
	now = base.Add(3 * time.Second)
	if !l.allow("192.0.2.4:1234") {
		t.Fatal("new peer rejected while the table was full")
	}
	if _, ok := l.entries["192.0.2.2"]; ok {
		t.Fatal("oldest entry under the limit was not evicted")
	}
	for _, key := range []string{"192.0.2.1", "192.0.2.3", "192.0.2.4"} {
		if _, ok := l.entries[key]; !ok {
			t.Fatalf("%s evicted, entries=%v", key, l.entries)
		}
	}
	if l.allow("192.0.2.1:1234") {
		t.Fatal("limited peer was reset")
	}

	// When every entry is limited, the oldest entry overall is evicted.
	l.entries = map[string]bucket{
		"192.0.2.1": {start: base.Add(2 * time.Second), count: l.limit},
		"192.0.2.2": {start: base, count: l.limit},
		"192.0.2.3": {start: base.Add(time.Second), count: l.limit},
	}
	if !l.allow("192.0.2.5:1234") {
		t.Fatal("new peer rejected while every entry was limited")
	}
	if _, ok := l.entries["192.0.2.2"]; ok {
		t.Fatal("oldest limited entry was not evicted")
	}
	if len(l.entries) != l.capacity {
		t.Fatalf("entries=%d", len(l.entries))
	}
}

func TestLoginLimiterGroupsIPv6By64(t *testing.T) {
	l := newLoginLimiter()
	for i, addr := range []string{"[2001:db8::1]:1", "[2001:db8::2]:2", "[2001:db8::ffff:1]:3", "[2001:db8:0:0:1::1]:4", "[2001:db8::abcd]:5", "[2001:db8::1]:6", "[2001:db8::3]:7", "[2001:db8::4]:8", "[2001:db8::5]:9", "[2001:db8::6]:10"} {
		if !l.allow(addr) {
			t.Fatalf("attempt %d rejected", i+1)
		}
	}
	if l.allow("[2001:db8::7]:11") {
		t.Fatal("11th attempt from the same /64 accepted")
	}
	if !l.allow("[2001:db8:0:1::1]:1") {
		t.Fatal("different /64 was limited")
	}
}

func TestLoginLimiterIPv4Mapped(t *testing.T) {
	if got := limiterKey("[::ffff:192.0.2.1]:1234"); got != "192.0.2.1" {
		t.Fatalf("mapped key: %q", got)
	}
	l := newLoginLimiter()
	for i := 0; i < l.limit; i++ {
		if !l.allow("[::ffff:192.0.2.1]:1234") {
			t.Fatalf("attempt %d rejected", i+1)
		}
	}
	if l.allow("192.0.2.1:1234") {
		t.Fatal("mapped and plain IPv4 were counted separately")
	}
	if !l.allow("[::ffff:192.0.2.2]:1234") {
		t.Fatal("different IPv4 addresses were grouped")
	}
	if !l.allow("192.0.2.3:1234") {
		t.Fatal("different IPv4 addresses were grouped")
	}
}

func TestLoginLimiterPerPeer(t *testing.T) {
	l := newLoginLimiter()
	now := time.Date(2026, time.October, 23, 0, 0, 0, 0, time.UTC)
	l.now = func() time.Time { return now }
	for i := 1; i <= 10; i++ {
		if !l.allow("192.0.2.1:1234") {
			t.Fatalf("attempt %d rejected", i)
		}
	}
	if l.allow("192.0.2.1:5678") {
		t.Fatal("11th attempt accepted")
	}
	if !l.allow("192.0.2.2:1234") {
		t.Fatal("another peer was limited")
	}
}

func TestLoginLimiterWindowBoundary(t *testing.T) {
	l := newLoginLimiter()
	base := time.Date(2026, time.October, 23, 0, 0, 0, 0, time.UTC)
	now := base
	l.now = func() time.Time { return now }
	for i := 0; i < l.limit; i++ {
		if !l.allow("192.0.2.1:1234") {
			t.Fatalf("attempt %d rejected", i+1)
		}
	}
	now = base.Add(59 * time.Second)
	if l.allow("192.0.2.1:1234") {
		t.Fatal("accepted 59 seconds after the first attempt")
	}
	now = base.Add(60 * time.Second)
	if !l.allow("192.0.2.1:1234") {
		t.Fatal("rejected 60 seconds after the first attempt")
	}
}

func TestLoginRateLimitIgnoresForwardedFor(t *testing.T) {
	s, _ := testutil.Database(t, "sqlite")
	hash, err := auth.Password("correct-password-123")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.CreateUser(context.Background(), store.User{ID: "alice-id", Email: "alice@example.com", PasswordHash: hash, Role: store.RoleOwner}); err != nil {
		t.Fatal(err)
	}
	a := auth.New(s)
	srv := newServer(config.Config{PublicURL: "https://astovia.test"}, a, fstest.MapFS{}, nil)
	now := time.Date(2026, time.October, 23, 0, 0, 0, 0, time.UTC)
	srv.limiter.now = func() time.Time { return now }
	h := srv.handler()
	for i := 1; i <= 11; i++ {
		r := httptest.NewRequest("POST", "/auth/login", strings.NewReader(`{"email":"alice@example.com","password":"wrong-password-1"}`))
		r.RemoteAddr = "192.0.2.1:1234"
		r.Header.Set("X-Forwarded-For", "198.51.100."+strconv.Itoa(i))
		r.Header.Set("Origin", "https://astovia.test")
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		want := http.StatusUnauthorized
		if i == 11 {
			want = http.StatusTooManyRequests
		}
		if w.Code != want {
			t.Fatalf("attempt %d: got %d, want %d", i, w.Code, want)
		}
	}
}

// With a trusted proxy, two clients behind it are counted apart by their
// X-Forwarded-For addresses.
func TestLoginRateLimitTrustedProxy(t *testing.T) {
	s, _ := testutil.Database(t, "sqlite")
	hash, err := auth.Password("correct-password-123")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.CreateUser(context.Background(), store.User{ID: "alice-id", Email: "alice@example.com", PasswordHash: hash, Role: store.RoleOwner}); err != nil {
		t.Fatal(err)
	}
	a := auth.New(s)
	cfg := config.Config{PublicURL: "https://astovia.test", TrustedProxies: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}}
	srv := newServer(cfg, a, fstest.MapFS{}, nil)
	now := time.Date(2026, time.October, 23, 0, 0, 0, 0, time.UTC)
	srv.limiter.now = func() time.Time { return now }
	h := srv.handler()
	// Each client signs in to its own address, so that only the limit per
	// client is under test, not the one per email address.
	login := func(forwardedFor, email string) int {
		r := httptest.NewRequest("POST", "/auth/login", strings.NewReader(`{"email":"`+email+`","password":"wrong-password-1"}`))
		r.RemoteAddr = "10.0.0.2:1234"
		r.Header.Set("X-Forwarded-For", forwardedFor)
		r.Header.Set("Origin", "https://astovia.test")
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	for i := 1; i <= 11; i++ {
		want := http.StatusUnauthorized
		if i == 11 {
			want = http.StatusTooManyRequests
		}
		if got := login("198.51.100.1", "alice@example.com"); got != want {
			t.Fatalf("first client attempt %d: got %d, want %d", i, got, want)
		}
	}
	if got := login("198.51.100.2", "bob@example.com"); got != http.StatusUnauthorized {
		t.Fatalf("second client: got %d, want 401", got)
	}
	// The leftmost entry is written by the client and is not the key.
	if got := login("203.0.113.9, 198.51.100.1", "carol@example.com"); got != http.StatusTooManyRequests {
		t.Fatalf("forged leftmost entry: got %d, want 429", got)
	}
}

func TestClientAddr(t *testing.T) {
	srv := &Server{cfg: config.Config{TrustedProxies: []netip.Prefix{
		netip.MustParsePrefix("10.0.0.0/8"),
		netip.MustParsePrefix("192.168.1.1/32"),
		netip.MustParsePrefix("fd00::/16"),
	}}}
	many := make([]string, 40)
	for i := range many {
		many[i] = "10.0.0." + strconv.Itoa(i)
	}
	for _, tc := range []struct {
		name, remote string
		header       []string
		want         string
	}{
		{"untrusted peer ignores the header", "203.0.113.1:1234", []string{"198.51.100.1"}, "203.0.113.1:1234"},
		{"unparseable peer", "pipe", []string{"198.51.100.1"}, "pipe"},
		{"no header", "10.0.0.2:1234", nil, "10.0.0.2:1234"},
		{"empty header", "10.0.0.2:1234", []string{""}, "10.0.0.2:1234"},
		{"one entry", "10.0.0.2:1234", []string{"198.51.100.1"}, "198.51.100.1"},
		{"trusted peer as IPv4-mapped IPv6", "[::ffff:10.0.0.2]:1234", []string{"198.51.100.1"}, "198.51.100.1"},
		{"trusted IPv6 peer", "[fd00::1]:1234", []string{"2001:db8::1"}, "2001:db8::1"},
		{"skips trusted entries on the right", "10.0.0.2:1234", []string{"203.0.113.5, 198.51.100.1, 10.1.2.3, 192.168.1.1"}, "198.51.100.1"},
		{"several header lines", "10.0.0.2:1234", []string{"203.0.113.5, 198.51.100.1", "10.1.2.3"}, "198.51.100.1"},
		{"last header line wins", "10.0.0.2:1234", []string{"198.51.100.1", "198.51.100.2"}, "198.51.100.2"},
		{"spaces", "10.0.0.2:1234", []string{"  198.51.100.1  ,  10.0.0.3 "}, "198.51.100.1"},
		{"unparseable entry", "10.0.0.2:1234", []string{"198.51.100.1, unknown, 10.0.0.3"}, "10.0.0.2:1234"},
		{"empty entry", "10.0.0.2:1234", []string{"198.51.100.1,,10.0.0.3"}, "10.0.0.2:1234"},
		{"zone", "10.0.0.2:1234", []string{"fe80::1%eth0"}, "10.0.0.2:1234"},
		{"unparseable entry left of the client is not read", "10.0.0.2:1234", []string{"garbage, 198.51.100.1"}, "198.51.100.1"},
		{"all entries trusted", "10.0.0.2:1234", []string{"10.0.0.5, 10.0.0.3"}, "10.0.0.2:1234"},
		{"IPv4 with a port", "10.0.0.2:1234", []string{"1.2.3.4:5678"}, "1.2.3.4"},
		{"IPv6 with a port", "10.0.0.2:1234", []string{"[2001:db8::1]:443"}, "2001:db8::1"},
		{"IPv4-mapped entry", "10.0.0.2:1234", []string{"::ffff:198.51.100.1"}, "198.51.100.1"},
		{"only the rightmost 32 entries are read", "10.0.0.2:1234", []string{"198.51.100.1, " + strings.Join(many[:32], ", ")}, "10.0.0.2:1234"},
		{"within the rightmost 32 entries", "10.0.0.2:1234", []string{"198.51.100.1, " + strings.Join(many[:31], ", ")}, "198.51.100.1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			r.RemoteAddr = tc.remote
			for _, v := range tc.header {
				r.Header.Add("X-Forwarded-For", v)
			}
			if got := srv.clientAddr(r); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
	// Without trusted proxies the header is never read.
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.2:1234"
	r.Header.Set("X-Forwarded-For", "198.51.100.1")
	if got := (&Server{}).clientAddr(r); got != "10.0.0.2:1234" {
		t.Fatalf("no trusted proxies: %q", got)
	}
}

// IPv6 /64 grouping is covered by TestLoginLimiterGroupsIPv6By64 and eviction
// of the oldest entry by TestLoginLimiterEvictsWhenFull.

// allowKeyN counts units: exactly the limit passes, units over it are
// refused without being counted, and n <= 0 is allowed without counting.
func TestKeyLimiterUnits(t *testing.T) {
	l := newKeyLimiter(100, time.Minute)
	now := time.Date(2026, time.October, 23, 0, 0, 0, 0, time.UTC)
	l.now = func() time.Time { return now }
	if !l.allowKeyN("a", 0) || !l.allowKeyN("a", -5) || len(l.entries) != 0 {
		t.Fatalf("n <= 0: %+v", l.entries)
	}
	if !l.allowKeyN("a", 60) || !l.allowKeyN("a", 40) || l.entries["a"].count != 100 {
		t.Fatalf("up to the limit: %+v", l.entries)
	}
	if l.allowKeyN("a", 1) || l.allowKey("a") || l.entries["a"].count != 100 {
		t.Fatalf("over the limit: %+v", l.entries)
	}
	if !l.allowKeyN("b", 90) || l.allowKeyN("b", 11) || l.entries["b"].count != 90 || !l.allowKeyN("b", 10) {
		t.Fatalf("over the limit is not counted: %+v", l.entries)
	}
	if l.allowKeyN("c", 101) || len(l.entries) != 2 {
		t.Fatalf("more than the limit at once: %+v", l.entries)
	}
	now = now.Add(time.Minute)
	if !l.allowKeyN("a", 100) {
		t.Fatal("rejected in the next window")
	}
}
