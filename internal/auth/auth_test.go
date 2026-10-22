package auth

import (
	"context"
	"errors"
	"github.com/golang-jwt/jwt/v5"
	"github.com/i2bskn/astovia/internal/store"
	"github.com/i2bskn/astovia/internal/testutil"
	"sync"
	"testing"
	"time"
)

func TestContracts(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			ctx := context.Background()
			s, dsn := testutil.Database(t, driver)
			secret := []byte("01234567890123456789012345678901")
			a, e := New(s, secret, "http://localhost:8080")
			if e != nil {
				t.Fatal(e)
			}
			hash, e := Password("correct-password-123")
			if e != nil {
				t.Fatal(e)
			}
			u := store.User{ID: "alice-id", Username: "alice", PasswordHash: hash, CreatedAt: time.Now().Unix()}
			v := store.User{ID: "bob-id", Username: "bob", PasswordHash: hash, CreatedAt: u.CreatedAt}
			for _, user := range []store.User{u, v} {
				if e = s.CreateUser(ctx, user); e != nil {
					t.Fatal(e)
				}
			}
			if e = s.CreateUser(ctx, store.User{ID: "other", Username: "alice", PasswordHash: hash}); !errors.Is(e, store.ErrConflict) {
				t.Fatalf("duplicate: %v", e)
			}
			login := func(name string) Result {
				t.Helper()
				r, e := a.Login(ctx, name, "correct-password-123")
				if e != nil {
					t.Fatal(e)
				}
				return r
			}
			for _, name := range []string{"alice", "missing"} {
				if _, e = a.Login(ctx, name, "wrong-password"); !errors.Is(e, store.ErrUnauthorized) {
					t.Fatalf("invalid login: %v", e)
				}
			}
			first := login(" ALICE ")
			bob := login("bob")
			if got, e := a.Authenticate(ctx, first.AccessToken); e != nil || got.ID != u.ID {
				t.Fatalf("identity: %+v %v", got, e)
			}
			rotated, e := a.Refresh(ctx, first.Refresh)
			if e != nil || rotated.Refresh == first.Refresh || rotated.ExpiresAt != first.ExpiresAt {
				t.Fatalf("rotation: %+v %v", rotated, e)
			}
			if _, e = a.Refresh(ctx, first.Refresh); !errors.Is(e, store.ErrUnauthorized) {
				t.Fatalf("replay: %v", e)
			}
			for _, token := range []string{first.AccessToken, rotated.AccessToken} {
				if _, e = a.Authenticate(ctx, token); !errors.Is(e, store.ErrUnauthorized) {
					t.Fatalf("replay must revoke access: %v", e)
				}
			}
			if _, e = a.Authenticate(ctx, bob.AccessToken); e != nil {
				t.Fatalf("other user affected: %v", e)
			}
			r := login("alice")
			rr, e := a.Refresh(ctx, r.Refresh)
			if e != nil {
				t.Fatal(e)
			}
			if e = a.Logout(ctx, r.Refresh); e != nil {
				t.Fatal(e)
			}
			if _, e = a.Authenticate(ctx, rr.AccessToken); !errors.Is(e, store.ErrUnauthorized) {
				t.Fatalf("logout rotated session: %v", e)
			}
			if e = a.Logout(ctx, r.Refresh); e != nil {
				t.Fatal(e)
			}
			r = login("alice")
			newHash, e := Password("replacement-password-123")
			if e != nil {
				t.Fatal(e)
			}
			if e = s.ChangeUser(ctx, "alice", newHash, false, time.Now().Unix()); e != nil {
				t.Fatal(e)
			}
			if _, e = a.Authenticate(ctx, r.AccessToken); !errors.Is(e, store.ErrUnauthorized) {
				t.Fatalf("reset access: %v", e)
			}
			// A login that verified the old hash before reset may not create a new session.
			if e = s.Login(ctx, u, store.Session{ID: "stale-session", UserID: u.ID, ExpiresAt: time.Now().Add(time.Hour).Unix()}, store.Refresh{Hash: "stale-hash", SessionID: "stale-session"}); !errors.Is(e, store.ErrUnauthorized) {
				t.Fatalf("stale verification: %v", e)
			}
			if e = s.ChangeUser(ctx, "bob", "", true, time.Now().Unix()); e != nil {
				t.Fatal(e)
			}
			if _, e = a.Authenticate(ctx, bob.AccessToken); !errors.Is(e, store.ErrUnauthorized) {
				t.Fatalf("disable access: %v", e)
			}
			if _, e = a.Login(ctx, "bob", "correct-password-123"); !errors.Is(e, store.ErrUnauthorized) {
				t.Fatalf("disabled login: %v", e)
			}
			if e = s.ChangeUser(ctx, "bob", hash, false, time.Now().Unix()); e != nil {
				t.Fatal(e)
			}
			if _, e = a.Login(ctx, "bob", "correct-password-123"); !errors.Is(e, store.ErrUnauthorized) {
				t.Fatalf("reset enabled disabled user: %v", e)
			}
			// Two independently opened stores share DB locks, not application mutexes.
			other, e := store.Open(ctx, driver, dsn)
			if e != nil {
				t.Fatal(e)
			}
			defer func() { _ = other.Close() }()
			a2, e := New(other, secret, "http://localhost:8080")
			if e != nil {
				t.Fatal(e)
			}
			r, e = a.Login(ctx, "alice", "replacement-password-123")
			if e != nil {
				t.Fatal(e)
			}
			results := make(chan error, 2)
			var wg sync.WaitGroup
			start := make(chan struct{})
			for _, svc := range []*Service{a, a2} {
				wg.Add(1)
				go func(svc *Service) { defer wg.Done(); <-start; _, e := svc.Refresh(ctx, r.Refresh); results <- e }(svc)
			}
			close(start)
			wg.Wait()
			close(results)
			success := 0
			for e := range results {
				if e == nil {
					success++
				} else if !errors.Is(e, store.ErrUnauthorized) {
					t.Fatalf("concurrent refresh: %v", e)
				}
			}
			if success != 1 {
				t.Fatalf("successes=%d", success)
			}
			if _, e = a.Authenticate(ctx, r.AccessToken); !errors.Is(e, store.ErrUnauthorized) {
				t.Fatalf("concurrent replay not revoked: %v", e)
			}
			// A successor insertion failure rolls back consumption of the original token.
			r, e = a.Login(ctx, "alice", "replacement-password-123")
			if e != nil {
				t.Fatal(e)
			}
			if _, _, e = s.Rotate(ctx, Hash(r.Refresh), Hash(r.Refresh), time.Now().Unix()); e == nil {
				t.Fatal("duplicate successor hash did not fail")
			}
			if _, e = a.Refresh(ctx, r.Refresh); e != nil {
				t.Fatalf("failed rotation consumed old token: %v", e)
			}
			// Refresh racing with logout or reset cannot leave a live successor session.
			for _, operation := range []string{"logout", "reset"} {
				r, e = a.Login(ctx, "alice", "replacement-password-123")
				if e != nil {
					t.Fatal(e)
				}
				gate := make(chan struct{})
				type refreshResult struct {
					result Result
					err    error
				}
				refreshed := make(chan refreshResult, 1)
				changed := make(chan error, 1)
				go func(raw string) { <-gate; result, err := a2.Refresh(ctx, raw); refreshed <- refreshResult{result, err} }(r.Refresh)
				go func(raw string) {
					<-gate
					if operation == "logout" {
						changed <- a.Logout(ctx, raw)
					} else {
						changed <- s.ChangeUser(ctx, "alice", newHash, false, time.Now().Unix())
					}
				}(r.Refresh)
				close(gate)
				rr := <-refreshed
				if err := <-changed; err != nil {
					t.Fatalf("%s race: %v", operation, err)
				}
				if rr.err != nil && !errors.Is(rr.err, store.ErrUnauthorized) {
					t.Fatalf("refresh/%s: %v", operation, rr.err)
				}
				if _, err := a.Authenticate(ctx, r.AccessToken); !errors.Is(err, store.ErrUnauthorized) {
					t.Fatalf("%s left original access active: %v", operation, err)
				}
				if rr.err == nil {
					if _, err := a.Authenticate(ctx, rr.result.AccessToken); !errors.Is(err, store.ErrUnauthorized) {
						t.Fatalf("%s left successor access active: %v", operation, err)
					}
					if _, err := a.Refresh(ctx, rr.result.Refresh); !errors.Is(err, store.ErrUnauthorized) {
						t.Fatalf("%s left successor refresh active: %v", operation, err)
					}
				}
			}
			r, e = a.Login(ctx, "alice", "replacement-password-123")
			if e != nil {
				t.Fatal(e)
			}
			a.Now = func() time.Time { return time.Now().Add(8 * 24 * time.Hour) }
			if _, e = a.Refresh(ctx, r.Refresh); !errors.Is(e, store.ErrUnauthorized) {
				t.Fatalf("expired refresh: %v", e)
			}
			if _, e = a.Authenticate(ctx, r.AccessToken); !errors.Is(e, store.ErrUnauthorized) {
				t.Fatalf("expired access: %v", e)
			}
			a.Now = time.Now
			for _, mutate := range []func(*claims){func(c *claims) { c.Audience = jwt.ClaimStrings{"wrong"} },
				func(c *claims) { c.Issuer = "https://wrong.example" }, func(c *claims) { c.Use = "oauth" }, func(c *claims) { c.IssuedAt = nil }, func(c *claims) { c.SessionID = "wrong" }, func(c *claims) { c.ExpiresAt = nil }} {
				c := new(claims)
				_, _, e = jwt.NewParser().ParseUnverified(r.AccessToken, c)
				if e != nil {
					t.Fatal(e)
				}
				mutate(c)
				raw, e := jwt.NewWithClaims(jwt.SigningMethodHS256, c).SignedString(secret)
				if e != nil {
					t.Fatal(e)
				}
				if _, e = a.Authenticate(ctx, raw); !errors.Is(e, store.ErrUnauthorized) {
					t.Fatalf("invalid claims accepted: %v", e)
				}
			}
			wrongAlgorithm := new(claims)
			if _, _, e = jwt.NewParser().ParseUnverified(r.AccessToken, wrongAlgorithm); e != nil {
				t.Fatal(e)
			}
			wrongRaw, e := jwt.NewWithClaims(jwt.SigningMethodHS384, wrongAlgorithm).SignedString(secret)
			if e != nil {
				t.Fatal(e)
			}
			if _, e = a.Authenticate(ctx, wrongRaw); !errors.Is(e, store.ErrUnauthorized) {
				t.Fatal("wrong algorithm accepted")
			}
			if _, e = a.Authenticate(ctx, r.AccessToken+"forged"); !errors.Is(e, store.ErrUnauthorized) {
				t.Fatal("forged JWT accepted")
			}
			// Reopening retains users.
			if user, e := other.UserByName(ctx, "alice"); e != nil || user.ID != u.ID {
				t.Fatalf("persistence: %v", e)
			}
		})
	}
}
func TestPasswordAndUsername(t *testing.T) {
	for _, s := range []string{"ab", "bad name", "日本語", "_abc"} {
		if _, e := Username(s); e == nil {
			t.Fatalf("accepted %q", s)
		}
	}
	if v, e := Username(" Alice "); e != nil || v != "alice" {
		t.Fatal(v, e)
	}
	if _, e := Password("short"); e == nil {
		t.Fatal("short password accepted")
	}
}

func activeAuthFixture(t *testing.T, driver string, names ...string) (*Service, map[string]Result) {
	t.Helper()
	ctx := context.Background()
	s, _ := testutil.Database(t, driver)
	a, err := New(s, []byte("01234567890123456789012345678901"), "http://localhost:8080")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.October, 23, 0, 0, 0, 0, time.UTC)
	a.Now = func() time.Time { return now }
	hash, err := Password("correct-password-123")
	if err != nil {
		t.Fatal(err)
	}
	results := make(map[string]Result, len(names))
	for _, name := range names {
		if err = s.CreateUser(ctx, store.User{ID: name + "-id", Username: name, PasswordHash: hash, CreatedAt: now.Unix()}); err != nil {
			t.Fatal(err)
		}
		result, err := a.Login(ctx, name, "correct-password-123")
		if err != nil {
			t.Fatal(err)
		}
		results[name] = result
	}
	return a, results
}

func TestAccessJWTExpiresBeforeSession(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			ctx := context.Background()
			a, results := activeAuthFixture(t, driver, "alice")
			original := results["alice"]
			if user, err := a.Authenticate(ctx, original.AccessToken); err != nil || user.ID != "alice-id" {
				t.Fatalf("initial access rejected: user=%+v err=%v", user, err)
			}
			c := new(claims)
			if _, _, err := jwt.NewParser().ParseUnverified(original.AccessToken, c); err != nil {
				t.Fatal(err)
			}
			if c.ExpiresAt == nil || c.IssuedAt == nil || c.ExpiresAt.Unix()-c.IssuedAt.Unix() != 600 || original.ExpiresIn != 600 {
				t.Fatalf("access lifetime must be 600 seconds: claims=%+v expires_in=%d", c, original.ExpiresIn)
			}
			later := a.Now().Add(11 * time.Minute)
			a.Now = func() time.Time { return later }
			if _, err := a.Store.Authenticated(ctx, c.Subject, c.SessionID, later.Unix()); err != nil {
				t.Fatalf("session must still be active after 11 minutes: %v", err)
			}
			if _, err := a.Authenticate(ctx, original.AccessToken); !errors.Is(err, store.ErrUnauthorized) {
				t.Fatalf("expired access accepted while session remained active: %v", err)
			}
			refreshed, err := a.Refresh(ctx, original.Refresh)
			if err != nil {
				t.Fatalf("refresh must remain valid after access expiry: %v", err)
			}
			if refreshed.ExpiresIn != 600 || refreshed.ExpiresAt != original.ExpiresAt {
				t.Fatalf("refresh changed lifetimes: expires_in=%d session_exp=%d", refreshed.ExpiresIn, refreshed.ExpiresAt)
			}
			if user, err := a.Authenticate(ctx, refreshed.AccessToken); err != nil || user.ID != "alice-id" {
				t.Fatalf("renewed access rejected at the same clock: user=%+v err=%v", user, err)
			}
		})
	}
}

func TestAccessJWTRequiresSessionOwner(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			ctx := context.Background()
			a, results := activeAuthFixture(t, driver, "alice", "bob")
			assertOriginalsActive := func() {
				t.Helper()
				for name, result := range results {
					if user, err := a.Authenticate(ctx, result.AccessToken); err != nil || user.ID != name+"-id" {
						t.Fatalf("%s original session must remain active: user=%+v err=%v", name, user, err)
					}
				}
			}
			assertOriginalsActive()
			for owner, wrongSubject := range map[string]string{"alice": "bob-id", "bob": "alice-id"} {
				c := new(claims)
				if _, _, err := jwt.NewParser().ParseUnverified(results[owner].AccessToken, c); err != nil {
					t.Fatal(err)
				}
				c.Subject = wrongSubject
				raw, err := jwt.NewWithClaims(jwt.SigningMethodHS256, c).SignedString(a.secret)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = a.Authenticate(ctx, raw); !errors.Is(err, store.ErrUnauthorized) {
					t.Fatalf("%s session accepted subject %s: %v", owner, wrongSubject, err)
				}
			}
			assertOriginalsActive()
		})
	}
}
