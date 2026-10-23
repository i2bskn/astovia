package auth

import (
	"context"
	"encoding/base64"
	"errors"
	"github.com/golang-jwt/jwt/v5"
	"github.com/i2bskn/astovia/internal/store"
	"github.com/i2bskn/astovia/internal/testutil"
	"golang.org/x/crypto/bcrypt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	PasswordCost = bcrypt.MinCost
	os.Exit(m.Run())
}

func TestDefaultPasswordCost(t *testing.T) {
	if DefaultPasswordCost != 12 {
		t.Fatalf("DefaultPasswordCost = %d", DefaultPasswordCost)
	}
}

// Refresh must use one clock reading for the store check and the issued
// token, so a session that is valid at that reading never yields expires_in 0.
func TestRefreshNearSessionExpiry(t *testing.T) {
	ctx := context.Background()
	s, _ := testutil.Database(t, "sqlite")
	a, err := New(s, []byte("01234567890123456789012345678901"), "http://localhost:8080")
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, time.October, 23, 0, 0, 0, 0, time.UTC)
	hash, err := Password("correct-password-123")
	if err != nil {
		t.Fatal(err)
	}
	u := store.User{ID: "alice-id", Username: "alice", PasswordHash: hash, CreatedAt: base.Unix()}
	if err = s.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	raw, err := Random()
	if err != nil {
		t.Fatal(err)
	}
	sess := store.Session{ID: "near-expiry", UserID: u.ID, CreatedAt: base.Unix(), ExpiresAt: base.Unix() + 1}
	if err = s.Login(ctx, u, sess, store.Refresh{Hash: Hash(raw), SessionID: sess.ID, CreatedAt: base.Unix()}); err != nil {
		t.Fatal(err)
	}
	// Every call advances the clock by one second.
	now := base
	a.Now = func() time.Time { current := now; now = now.Add(time.Second); return current }
	result, err := a.Refresh(ctx, raw)
	if err != nil {
		if !errors.Is(err, store.ErrUnauthorized) {
			t.Fatal(err)
		}
		return
	}
	if result.ExpiresIn <= 0 || result.MaxAge < 1 {
		t.Fatalf("expires_in=%d max_age=%d", result.ExpiresIn, result.MaxAge)
	}
}

// contractSecret signs the access tokens issued by contract fixtures.
var contractSecret = []byte("01234567890123456789012345678901")

// contractFixture is an isolated database with users whose password is
// "correct-password-123". Every Service built from it reads the same fixed
// clock, now, which a subtest may move forward but never mixes with the real
// time.
type contractFixture struct {
	t      *testing.T
	ctx    context.Context
	driver string
	dsn    string
	store  *store.Store
	auth   *Service
	hash   string
	users  map[string]store.User
	now    time.Time
}

func newContractFixture(t *testing.T, driver string, names ...string) *contractFixture {
	t.Helper()
	ctx := context.Background()
	s, dsn := testutil.Database(t, driver)
	f := &contractFixture{t: t, ctx: ctx, driver: driver, dsn: dsn, store: s, users: make(map[string]store.User, len(names)), now: time.Date(2026, time.October, 23, 0, 0, 0, 0, time.UTC)}
	f.auth = f.service(s)
	hash, err := Password("correct-password-123")
	if err != nil {
		t.Fatal(err)
	}
	f.hash = hash
	for _, name := range names {
		u := store.User{ID: name + "-id", Username: name, PasswordHash: hash, CreatedAt: f.now.Unix()}
		if err = s.CreateUser(ctx, u); err != nil {
			t.Fatal(err)
		}
		f.users[name] = u
	}
	return f
}

// service returns a Service on s that reads the fixture clock.
func (f *contractFixture) service(s *store.Store) *Service {
	f.t.Helper()
	a, err := New(s, contractSecret, "http://localhost:8080")
	if err != nil {
		f.t.Fatal(err)
	}
	a.Now = func() time.Time { return f.now }
	return a
}

// reopen opens a second, independent Store on the same database, so that
// concurrent operations share database locks rather than application mutexes.
func (f *contractFixture) reopen() (*store.Store, *Service) {
	f.t.Helper()
	other, err := store.Open(f.ctx, f.driver, f.dsn)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { _ = other.Close() })
	return other, f.service(other)
}

func (f *contractFixture) login(name, password string) Result {
	f.t.Helper()
	r, err := f.auth.Login(f.ctx, name, password)
	if err != nil {
		f.t.Fatalf("login %s: %v", name, err)
	}
	return r
}

func (f *contractFixture) assertActive(token, wantID string) {
	f.t.Helper()
	if u, err := f.auth.Authenticate(f.ctx, token); err != nil || u.ID != wantID {
		f.t.Fatalf("access token must be active: user=%+v err=%v", u, err)
	}
}

func (f *contractFixture) assertRejected(token, what string) {
	f.t.Helper()
	if _, err := f.auth.Authenticate(f.ctx, token); !errors.Is(err, store.ErrUnauthorized) {
		f.t.Fatalf("%s accepted: %v", what, err)
	}
}

// accessClaims returns the unverified claims of an access token.
func accessClaims(t *testing.T, token string) *claims {
	t.Helper()
	c := new(claims)
	if _, _, err := jwt.NewParser().ParseUnverified(token, c); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestContracts(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			t.Run("duplicate username conflicts", func(t *testing.T) {
				f := newContractFixture(t, driver, "alice")
				if err := f.store.CreateUser(f.ctx, store.User{ID: "other", Username: "alice", PasswordHash: f.hash}); !errors.Is(err, store.ErrConflict) {
					t.Fatalf("duplicate: %v", err)
				}
			})

			t.Run("invalid login is unauthorized", func(t *testing.T) {
				f := newContractFixture(t, driver, "alice")
				f.login("alice", "correct-password-123")
				for _, name := range []string{"alice", "missing"} {
					if _, err := f.auth.Login(f.ctx, name, "wrong-password"); !errors.Is(err, store.ErrUnauthorized) {
						t.Fatalf("invalid login %s: %v", name, err)
					}
				}
			})

			t.Run("login normalizes the username", func(t *testing.T) {
				f := newContractFixture(t, driver, "alice")
				first := f.login(" ALICE ", "correct-password-123")
				f.assertActive(first.AccessToken, "alice-id")
			})

			t.Run("rotation and replay", func(t *testing.T) {
				f := newContractFixture(t, driver, "alice", "bob")
				first := f.login("alice", "correct-password-123")
				bob := f.login("bob", "correct-password-123")
				// Move the clock (still inside the access lifetime) so that a
				// rotation which extended the session would change ExpiresAt.
				f.now = f.now.Add(time.Minute)
				f.assertActive(first.AccessToken, "alice-id")
				f.assertActive(bob.AccessToken, "bob-id")
				rotated, err := f.auth.Refresh(f.ctx, first.Refresh)
				if err != nil || rotated.Refresh == first.Refresh || rotated.ExpiresAt != first.ExpiresAt {
					t.Fatalf("rotation: %+v %v", rotated, err)
				}
				f.assertActive(rotated.AccessToken, "alice-id")
				if _, err = f.auth.Refresh(f.ctx, first.Refresh); !errors.Is(err, store.ErrUnauthorized) {
					t.Fatalf("replay: %v", err)
				}
				f.assertRejected(first.AccessToken, "access after replay")
				f.assertRejected(rotated.AccessToken, "rotated access after replay")
				// Replay revokes only the replayed session.
				f.assertActive(bob.AccessToken, "bob-id")
			})

			t.Run("logout with a rotated token revokes the session", func(t *testing.T) {
				f := newContractFixture(t, driver, "alice")
				r := f.login("alice", "correct-password-123")
				rr, err := f.auth.Refresh(f.ctx, r.Refresh)
				if err != nil {
					t.Fatal(err)
				}
				f.assertActive(rr.AccessToken, "alice-id")
				if err = f.auth.Logout(f.ctx, r.Refresh); err != nil {
					t.Fatal(err)
				}
				f.assertRejected(rr.AccessToken, "rotated session after logout")
				// Logout is idempotent.
				if err = f.auth.Logout(f.ctx, r.Refresh); err != nil {
					t.Fatal(err)
				}
			})

			t.Run("password reset revokes sessions and stale logins", func(t *testing.T) {
				f := newContractFixture(t, driver, "alice")
				r := f.login("alice", "correct-password-123")
				f.assertActive(r.AccessToken, "alice-id")
				newHash, err := Password("replacement-password-123")
				if err != nil {
					t.Fatal(err)
				}
				if err = f.store.ChangeUser(f.ctx, "alice", newHash, false, f.now.Unix()); err != nil {
					t.Fatal(err)
				}
				f.assertRejected(r.AccessToken, "access after reset")
				// A login that verified the old hash before reset may not create a new session.
				if err = f.store.Login(f.ctx, f.users["alice"], store.Session{ID: "stale-session", UserID: "alice-id", ExpiresAt: f.now.Add(time.Hour).Unix()}, store.Refresh{Hash: "stale-hash", SessionID: "stale-session"}); !errors.Is(err, store.ErrUnauthorized) {
					t.Fatalf("stale verification: %v", err)
				}
				f.login("alice", "replacement-password-123")
			})

			t.Run("disable revokes sessions and reset does not enable", func(t *testing.T) {
				f := newContractFixture(t, driver, "bob")
				bob := f.login("bob", "correct-password-123")
				f.assertActive(bob.AccessToken, "bob-id")
				if err := f.store.ChangeUser(f.ctx, "bob", "", true, f.now.Unix()); err != nil {
					t.Fatal(err)
				}
				f.assertRejected(bob.AccessToken, "access after disable")
				if _, err := f.auth.Login(f.ctx, "bob", "correct-password-123"); !errors.Is(err, store.ErrUnauthorized) {
					t.Fatalf("disabled login: %v", err)
				}
				if err := f.store.ChangeUser(f.ctx, "bob", f.hash, false, f.now.Unix()); err != nil {
					t.Fatal(err)
				}
				if _, err := f.auth.Login(f.ctx, "bob", "correct-password-123"); !errors.Is(err, store.ErrUnauthorized) {
					t.Fatalf("reset enabled disabled user: %v", err)
				}
			})

			t.Run("concurrent refresh across stores", func(t *testing.T) {
				f := newContractFixture(t, driver, "alice")
				_, a2 := f.reopen()
				r := f.login("alice", "correct-password-123")
				f.assertActive(r.AccessToken, "alice-id")
				results := make(chan error, 2)
				var wg sync.WaitGroup
				start := make(chan struct{})
				for _, svc := range []*Service{f.auth, a2} {
					wg.Add(1)
					go func(svc *Service) { defer wg.Done(); <-start; _, e := svc.Refresh(f.ctx, r.Refresh); results <- e }(svc)
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
				f.assertRejected(r.AccessToken, "concurrent replay")
			})

			t.Run("failed rotation keeps the token", func(t *testing.T) {
				f := newContractFixture(t, driver, "alice")
				r := f.login("alice", "correct-password-123")
				// A successor insertion failure rolls back consumption of the original token.
				if _, _, err := f.store.Rotate(f.ctx, Hash(r.Refresh), Hash(r.Refresh), f.now.Unix()); err == nil {
					t.Fatal("duplicate successor hash did not fail")
				}
				if _, err := f.auth.Refresh(f.ctx, r.Refresh); err != nil {
					t.Fatalf("failed rotation consumed old token: %v", err)
				}
			})

			// Refresh racing with logout or reset cannot leave a live successor session.
			for _, operation := range []string{"logout", "reset"} {
				t.Run("refresh racing "+operation, func(t *testing.T) {
					f := newContractFixture(t, driver, "alice")
					_, a2 := f.reopen()
					newHash, err := Password("replacement-password-123")
					if err != nil {
						t.Fatal(err)
					}
					r := f.login("alice", "correct-password-123")
					f.assertActive(r.AccessToken, "alice-id")
					gate := make(chan struct{})
					type refreshResult struct {
						result Result
						err    error
					}
					refreshed := make(chan refreshResult, 1)
					changed := make(chan error, 1)
					go func(raw string) {
						<-gate
						result, err := a2.Refresh(f.ctx, raw)
						refreshed <- refreshResult{result, err}
					}(r.Refresh)
					go func(raw string) {
						<-gate
						if operation == "logout" {
							changed <- f.auth.Logout(f.ctx, raw)
						} else {
							changed <- f.store.ChangeUser(f.ctx, "alice", newHash, false, f.now.Unix())
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
					f.assertRejected(r.AccessToken, operation+" original access")
					if rr.err == nil {
						f.assertRejected(rr.result.AccessToken, operation+" successor access")
						if _, err := f.auth.Refresh(f.ctx, rr.result.Refresh); !errors.Is(err, store.ErrUnauthorized) {
							t.Fatalf("%s left successor refresh active: %v", operation, err)
						}
					}
				})
			}

			t.Run("expired session", func(t *testing.T) {
				f := newContractFixture(t, driver, "alice")
				r := f.login("alice", "correct-password-123")
				f.assertActive(r.AccessToken, "alice-id")
				f.now = f.now.Add(8 * 24 * time.Hour)
				if _, err := f.auth.Refresh(f.ctx, r.Refresh); !errors.Is(err, store.ErrUnauthorized) {
					t.Fatalf("expired refresh: %v", err)
				}
				f.assertRejected(r.AccessToken, "expired access")
			})

			t.Run("invalid claims", func(t *testing.T) {
				f := newContractFixture(t, driver, "alice")
				r := f.login("alice", "correct-password-123")
				sign := func(method jwt.SigningMethod, c *claims) string {
					t.Helper()
					raw, err := jwt.NewWithClaims(method, c).SignedString(contractSecret)
					if err != nil {
						t.Fatal(err)
					}
					return raw
				}
				// Positive control: the original and an unmodified re-signed token
				// are accepted at this clock, so rejections below come from the
				// mutation rather than from expiry.
				f.assertActive(r.AccessToken, "alice-id")
				f.assertActive(sign(jwt.SigningMethodHS256, accessClaims(t, r.AccessToken)), "alice-id")
				for name, mutate := range map[string]func(*claims){
					"audience":   func(c *claims) { c.Audience = jwt.ClaimStrings{"wrong"} },
					"issuer":     func(c *claims) { c.Issuer = "https://wrong.example" },
					"use":        func(c *claims) { c.Use = "oauth" },
					"issued at":  func(c *claims) { c.IssuedAt = nil },
					"session id": func(c *claims) { c.SessionID = "wrong" },
					"expiry":     func(c *claims) { c.ExpiresAt = nil },
				} {
					c := accessClaims(t, r.AccessToken)
					mutate(c)
					f.assertRejected(sign(jwt.SigningMethodHS256, c), "invalid "+name)
				}
				f.assertRejected(sign(jwt.SigningMethodHS384, accessClaims(t, r.AccessToken)), "wrong algorithm")
				f.assertRejected(r.AccessToken+"forged", "forged JWT")
			})

			t.Run("reopened store retains users", func(t *testing.T) {
				f := newContractFixture(t, driver, "alice")
				other, _ := f.reopen()
				if user, err := other.UserByName(f.ctx, "alice"); err != nil || user.ID != "alice-id" {
					t.Fatalf("persistence: %+v %v", user, err)
				}
			})
		})
	}
}

func TestPasswordAndUsernameBoundaries(t *testing.T) {
	for _, tc := range []struct {
		length int
		ok     bool
	}{{11, false}, {12, true}, {72, true}, {73, false}} {
		if _, err := Password(strings.Repeat("p", tc.length)); (err == nil) != tc.ok {
			t.Fatalf("Password with %d bytes: err=%v", tc.length, err)
		}
	}
	for _, tc := range []struct {
		length int
		ok     bool
	}{{2, false}, {3, true}, {64, true}, {65, false}} {
		if _, err := Username(strings.Repeat("u", tc.length)); (err == nil) != tc.ok {
			t.Fatalf("Username with %d characters: err=%v", tc.length, err)
		}
	}
}

func TestInputBoundaries(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			f := newContractFixture(t, driver, "alice")
			r := f.login("alice", "correct-password-123")
			for name, password := range map[string]string{"empty": "", "73 bytes": strings.Repeat("p", 73)} {
				if _, err := f.auth.Login(f.ctx, "alice", password); !errors.Is(err, store.ErrUnauthorized) {
					t.Fatalf("Login with %s password: %v", name, err)
				}
			}
			for name, raw := range map[string]string{
				"empty": "",
				// Also rejected by base64 decoding; this checks the behavior, not
				// the length guard on its own.
				"129 characters": strings.Repeat("A", 129),
				"non-base64":     strings.Repeat("!", 43),
				"31 bytes":       base64.RawURLEncoding.EncodeToString(make([]byte, 31)),
				"33 bytes":       base64.RawURLEncoding.EncodeToString(make([]byte, 33)),
			} {
				if _, err := f.auth.Refresh(f.ctx, raw); !errors.Is(err, store.ErrUnauthorized) {
					t.Fatalf("Refresh with %s token: %v", name, err)
				}
			}
			if err := f.auth.Logout(f.ctx, "not a token"); err != nil {
				t.Fatalf("Logout with an invalid token: %v", err)
			}
			// Also rejected by JWT parsing; this checks the behavior, not the
			// length guard on its own.
			f.assertRejected(strings.Repeat("a", 4097), "4097-character access token")
			// The valid session is unaffected by the rejected inputs.
			f.assertActive(r.AccessToken, "alice-id")
			if _, err := f.auth.Refresh(f.ctx, r.Refresh); err != nil {
				t.Fatalf("valid refresh after rejected inputs: %v", err)
			}
		})
	}
}

// The session expiry is checked by the store; JWT expiry would hide it.
func TestSessionExpiryBoundary(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			f := newContractFixture(t, driver, "alice")
			r := f.login("alice", "correct-password-123")
			c := accessClaims(t, r.AccessToken)
			if _, err := f.store.Authenticated(f.ctx, c.Subject, c.SessionID, r.ExpiresAt-1); err != nil {
				t.Fatalf("session rejected one second before expiry: %v", err)
			}
			if _, err := f.store.Authenticated(f.ctx, c.Subject, c.SessionID, r.ExpiresAt); !errors.Is(err, store.ErrUnauthorized) {
				t.Fatalf("session accepted at expiry: %v", err)
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
