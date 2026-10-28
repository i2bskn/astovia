package store_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/i2bskn/astovia/internal/store"
	"github.com/i2bskn/astovia/internal/testutil"
	"reflect"
	"testing"
)

const tokenTestNow = 1_800_000_000

// apiTokenFixture creates alice and bob, each with an active session whose ID
// is "<name>-session".
func apiTokenFixture(t *testing.T, driver string) (*store.Store, map[string]store.User) {
	t.Helper()
	ctx := context.Background()
	s, _ := testutil.Database(t, driver)
	users := map[string]store.User{}
	for _, name := range []string{"alice", "bob"} {
		u := store.User{ID: name + "-id", Email: name + "@example.com", PasswordHash: "x", Role: store.RoleOwner}
		if err := s.CreateUser(ctx, u); err != nil {
			t.Fatal(err)
		}
		sid := name + "-session"
		if err := s.Login(ctx, u, store.Session{ID: sid, UserID: u.ID, ExpiresAt: tokenTestNow + 3600, CreatedAt: tokenTestNow}, store.Refresh{Hash: "refresh-" + name, SessionID: sid, CreatedAt: tokenTestNow}); err != nil {
			t.Fatal(err)
		}
		users[name] = u
	}
	return s, users
}

func createToken(t *testing.T, s *store.Store, userID, sid, id string, created int64) store.APIToken {
	t.Helper()
	tok := store.APIToken{ID: id, UserID: userID, Name: "name " + id, CreatedAt: created}
	if err := s.CreateAPIToken(context.Background(), tok, "hash-"+id, sid, tokenTestNow); err != nil {
		t.Fatalf("create %s: %v", id, err)
	}
	return tok
}

func TestAPITokens(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) { testAPITokens(t, driver) })
	}
}

func testAPITokens(t *testing.T, driver string) {
	ctx := context.Background()
	s, _ := apiTokenFixture(t, driver)

	empty, err := s.ListAPITokens(ctx, "alice-id")
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("empty list: %#v %v", empty, err)
	}

	// Newest first, ties broken by id descending; another user's tokens are
	// not listed.
	a1 := createToken(t, s, "alice-id", "alice-session", "a1", 100)
	a2 := createToken(t, s, "alice-id", "alice-session", "a2", 200)
	a3 := createToken(t, s, "alice-id", "alice-session", "a3", 200)
	b1 := createToken(t, s, "bob-id", "bob-session", "b1", 300)
	list, err := s.ListAPITokens(ctx, "alice-id")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(list, []store.APIToken{a3, a2, a1}) {
		t.Fatalf("list: %+v", list)
	}

	// Deleting another user's token fails and keeps it.
	if err = s.DeleteAPIToken(ctx, "alice-id", "b1"); !errors.Is(err, store.ErrTokenNotFound) {
		t.Fatalf("delete other's token: %v", err)
	}
	if list, err = s.ListAPITokens(ctx, "bob-id"); err != nil || !reflect.DeepEqual(list, []store.APIToken{b1}) {
		t.Fatalf("bob's tokens: %+v %v", list, err)
	}
	if err = s.DeleteAPIToken(ctx, "alice-id", "missing"); !errors.Is(err, store.ErrTokenNotFound) {
		t.Fatalf("delete missing token: %v", err)
	}
	if err = s.DeleteAPIToken(ctx, "alice-id", "a1"); err != nil {
		t.Fatal(err)
	}
	if list, err = s.ListAPITokens(ctx, "alice-id"); err != nil || !reflect.DeepEqual(list, []store.APIToken{a3, a2}) {
		t.Fatalf("after delete: %+v %v", list, err)
	}

	// Authentication by hash.
	u, touchErr, err := s.AuthenticateAPIToken(ctx, "hash-a2", 1000)
	if err != nil || touchErr != nil || u.ID != "alice-id" {
		t.Fatalf("authenticate: %+v %v %v", u, touchErr, err)
	}
	for _, hash := range []string{"hash-a1", "hash-missing", ""} {
		if _, _, err = s.AuthenticateAPIToken(ctx, hash, 1000); !errors.Is(err, store.ErrUnauthorized) {
			t.Fatalf("hash %q: %v", hash, err)
		}
	}

	// last_used_at is recorded, but not again within 60 seconds.
	lastUsed := func() sql.NullInt64 {
		t.Helper()
		list, err := s.ListAPITokens(ctx, "alice-id")
		if err != nil {
			t.Fatal(err)
		}
		for _, tok := range list {
			if tok.ID == "a2" {
				return tok.LastUsedAt
			}
		}
		t.Fatal("a2 missing")
		return sql.NullInt64{}
	}
	if got := lastUsed(); got != (sql.NullInt64{Int64: 1000, Valid: true}) {
		t.Fatalf("last_used_at after first use: %+v", got)
	}
	for _, step := range []struct{ now, want int64 }{{1030, 1000}, {1060, 1000}, {1061, 1061}} {
		if _, _, err = s.AuthenticateAPIToken(ctx, "hash-a2", step.now); err != nil {
			t.Fatal(err)
		}
		if got := lastUsed(); got != (sql.NullInt64{Int64: step.want, Valid: true}) {
			t.Fatalf("last_used_at at %d: %+v, want %d", step.now, got, step.want)
		}
	}
	if list, err = s.ListAPITokens(ctx, "alice-id"); err != nil || len(list) != 2 || list[0].ID != "a3" || list[0].LastUsedAt.Valid {
		t.Fatalf("unused token: %+v %v", list, err)
	}

	// A disabled user's tokens are deleted and stop authenticating.
	if err = s.ChangeUser(ctx, "alice@example.com", "", true, tokenTestNow); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.AuthenticateAPIToken(ctx, "hash-a2", 2000); !errors.Is(err, store.ErrUnauthorized) {
		t.Fatalf("disabled user's token: %v", err)
	}
	if list, err = s.ListAPITokens(ctx, "alice-id"); err != nil || len(list) != 0 {
		t.Fatalf("disabled user's tokens remain: %+v %v", list, err)
	}
	if u, _, err = s.AuthenticateAPIToken(ctx, "hash-b1", 2000); err != nil || u.ID != "bob-id" {
		t.Fatalf("bob's token: %+v %v", u, err)
	}
}

// A token whose user is disabled without going through ChangeUser (so the
// row is still there) is rejected as well.
func TestAPITokenDisabledUser(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			ctx := context.Background()
			s, dsn := testutil.Database(t, driver)
			if err := s.CreateUser(ctx, store.User{ID: "alice-id", Email: "alice@example.com", PasswordHash: "x", Role: store.RoleOwner}); err != nil {
				t.Fatal(err)
			}
			if err := s.Login(ctx, store.User{ID: "alice-id", PasswordHash: "x"}, store.Session{ID: "sid", UserID: "alice-id", ExpiresAt: tokenTestNow + 60, CreatedAt: tokenTestNow}, store.Refresh{Hash: "r", SessionID: "sid", CreatedAt: tokenTestNow}); err != nil {
				t.Fatal(err)
			}
			createToken(t, s, "alice-id", "sid", "a1", 1)
			raw, err := sql.Open(map[string]string{"sqlite": "sqlite", "postgres": "pgx"}[driver], dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = raw.Close() }()
			if _, err = raw.ExecContext(ctx, "UPDATE users SET disabled = 1"); err != nil {
				t.Fatal(err)
			}
			if _, _, err = s.AuthenticateAPIToken(ctx, "hash-a1", tokenTestNow); !errors.Is(err, store.ErrUnauthorized) {
				t.Fatalf("disabled user's token: %v", err)
			}
		})
	}
}

func TestAPITokenLimit(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			ctx := context.Background()
			s, _ := apiTokenFixture(t, driver)
			for i := 0; i < store.MaxAPITokens; i++ {
				createToken(t, s, "alice-id", "alice-session", fmt.Sprintf("t%02d", i), int64(i))
			}
			err := s.CreateAPIToken(ctx, store.APIToken{ID: "extra", UserID: "alice-id", Name: "x", CreatedAt: 1}, "hash-extra", "alice-session", tokenTestNow)
			if !errors.Is(err, store.ErrTooManyTokens) {
				t.Fatalf("51st token: %v", err)
			}
			if list, err := s.ListAPITokens(ctx, "alice-id"); err != nil || len(list) != store.MaxAPITokens {
				t.Fatalf("tokens: %d %v", len(list), err)
			}
			// The limit is per user.
			createToken(t, s, "bob-id", "bob-session", "b1", 1)
		})
	}
}

// CreateAPIToken re-checks the session inside its transaction, so a session
// revoked by a password reset or disable, or another user's session, cannot
// issue a token.
func TestAPITokenSessionCheck(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			ctx := context.Background()
			s, _ := apiTokenFixture(t, driver)
			count := func(userID string) int {
				t.Helper()
				list, err := s.ListAPITokens(ctx, userID)
				if err != nil {
					t.Fatal(err)
				}
				return len(list)
			}
			create := func(userID, sid string, now int64) error {
				return s.CreateAPIToken(ctx, store.APIToken{ID: "new-" + sid, UserID: userID, Name: "x", CreatedAt: now}, "hash-new-"+sid, sid, now)
			}
			// Another user's valid session.
			if err := create("alice-id", "bob-session", tokenTestNow); !errors.Is(err, store.ErrUnauthorized) {
				t.Fatalf("other user's session: %v", err)
			}
			// Unknown and expired sessions.
			if err := create("alice-id", "missing", tokenTestNow); !errors.Is(err, store.ErrUnauthorized) {
				t.Fatalf("unknown session: %v", err)
			}
			if err := create("alice-id", "alice-session", tokenTestNow+3600); !errors.Is(err, store.ErrUnauthorized) {
				t.Fatalf("expired session: %v", err)
			}
			// Unknown user.
			if err := s.CreateAPIToken(ctx, store.APIToken{ID: "ghost", UserID: "ghost-id", Name: "x"}, "hash-ghost", "alice-session", tokenTestNow); !errors.Is(err, store.ErrUnauthorized) {
				t.Fatalf("unknown user: %v", err)
			}
			if count("alice-id") != 0 || count("bob-id") != 0 {
				t.Fatal("rejected requests stored tokens")
			}
			// A valid session succeeds.
			if err := create("alice-id", "alice-session", tokenTestNow); err != nil {
				t.Fatal(err)
			}
			if count("alice-id") != 1 {
				t.Fatal("token not stored")
			}
			// After a password reset the old session cannot issue tokens, and
			// the existing token is gone.
			if err := s.ChangeUser(ctx, "alice@example.com", "new-hash", false, tokenTestNow); err != nil {
				t.Fatal(err)
			}
			if count("alice-id") != 0 {
				t.Fatal("password reset kept tokens")
			}
			if err := s.CreateAPIToken(ctx, store.APIToken{ID: "after-reset", UserID: "alice-id", Name: "x", CreatedAt: tokenTestNow}, "hash-after-reset", "alice-session", tokenTestNow); !errors.Is(err, store.ErrUnauthorized) {
				t.Fatalf("revoked session after reset: %v", err)
			}
			if count("alice-id") != 0 {
				t.Fatal("revoked session stored a token")
			}
			// Disable behaves the same for bob.
			if err := create("bob-id", "bob-session", tokenTestNow); err != nil {
				t.Fatal(err)
			}
			if err := s.ChangeUser(ctx, "bob@example.com", "", true, tokenTestNow); err != nil {
				t.Fatal(err)
			}
			if err := s.CreateAPIToken(ctx, store.APIToken{ID: "after-disable", UserID: "bob-id", Name: "x", CreatedAt: tokenTestNow}, "hash-after-disable", "bob-session", tokenTestNow); !errors.Is(err, store.ErrUnauthorized) {
				t.Fatalf("disabled user: %v", err)
			}
			if count("bob-id") != 0 {
				t.Fatal("disabled user kept or stored tokens")
			}
		})
	}
}
