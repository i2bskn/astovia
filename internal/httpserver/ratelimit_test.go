package httpserver

import (
	"context"
	"net/http"
	"net/http/httptest"
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
	if err = s.CreateUser(context.Background(), store.User{ID: "alice-id", Username: "alice", PasswordHash: hash}); err != nil {
		t.Fatal(err)
	}
	a, err := auth.New(s, make([]byte, 32), "https://astovia.test")
	if err != nil {
		t.Fatal(err)
	}
	srv := newServer(config.Config{PublicURL: "https://astovia.test"}, a, fstest.MapFS{})
	now := time.Date(2026, time.October, 23, 0, 0, 0, 0, time.UTC)
	srv.limiter.now = func() time.Time { return now }
	h := srv.handler()
	for i := 1; i <= 11; i++ {
		r := httptest.NewRequest("POST", "/auth/login", strings.NewReader(`{"username":"alice","password":"wrong-password-1"}`))
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

// IPv6 /64 grouping is covered by TestLoginLimiterGroupsIPv6By64 and eviction
// of the oldest entry by TestLoginLimiterEvictsWhenFull.
