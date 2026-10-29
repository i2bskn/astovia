package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/i2bskn/astovia/internal/auth"
	"github.com/i2bskn/astovia/internal/store"
	"github.com/i2bskn/astovia/internal/testutil"
	"golang.org/x/crypto/bcrypt"
)

func TestMain(m *testing.M) {
	auth.PasswordCost = bcrypt.MinCost
	os.Exit(m.Run())
}

func TestReadPasswordFromStdin(t *testing.T) {
	for _, tc := range []struct {
		in, want string
		ok       bool
	}{
		{"correct-password-123\n", "correct-password-123", true},
		{"correct-password-123\r\n", "correct-password-123", true},
		{"correct-password-123\r", "correct-password-123", true},
		{"correct-password-123", "correct-password-123", true},
		{"pw-123456789012\n\n", "", false},
		{"abc\ndef\n", "", false},
		{"abc\rdef", "", false},
		{strings.Repeat("x", 74) + "\n", strings.Repeat("x", 74), true},
		{strings.Repeat("x", 76), "", false},
		{strings.Repeat("x", 75) + "\n", "", false},
		{strings.Repeat("x", 200), "", false},
	} {
		got, err := readPassword(strings.NewReader(tc.in), true)
		if tc.ok && (err != nil || got != tc.want) {
			t.Fatalf("%q: got %q, %v", tc.in, got, err)
		}
		if !tc.ok && err == nil {
			t.Fatalf("%q: accepted as %q", tc.in, got)
		}
	}
}

func TestUsersCommand(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			ctx := context.Background()
			s, _ := testutil.Database(t, driver)
			a, err := auth.New(s, make([]byte, 32), "http://localhost:8080")
			if err != nil {
				t.Fatal(err)
			}
			call := func(in string, args ...string) error {
				t.Helper()
				return account(ctx, s, args, strings.NewReader(in))
			}
			login := func(password string) error {
				t.Helper()
				_, err := a.Login(ctx, "alice@example.com", password)
				return err
			}

			// The trailing \r\n is removed before hashing.
			if err = call("correct-password-123\r\n", "create", "--email", "alice@example.com", "--password-stdin"); err != nil {
				t.Fatalf("create: %v", err)
			}
			if err = login("correct-password-123"); err != nil {
				t.Fatalf("login after create: %v", err)
			}
			if err = call("correct-password-123\n", "create", "--email", "alice@example.com", "--password-stdin"); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("duplicate create: %v", err)
			}
			if err = call("correct-password-123\n", "create", "--email", "Alice@Example.com", "--password-stdin"); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("duplicate create differing only in case: %v", err)
			}
			if err = call("replacement-password-123\n", "reset-password", "--email", "missing@example.com", "--password-stdin"); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("reset of a missing user: %v", err)
			}

			for _, tc := range []struct {
				name string
				args []string
				want string
			}{
				// alice exists, so a missing check would disable her instead of failing.
				{"disable with password-stdin", []string{"disable", "--email", "alice@example.com", "--password-stdin"}, "disable does not accept --password-stdin"},
				{"extra arguments", []string{"create", "--email", "alice@example.com", "extra"}, "unexpected arguments"},
				{"unknown subcommand", []string{"rename", "--email", "alice@example.com"}, "unknown users command"},
				{"no subcommand", nil, "users command requires create, reset-password, disable, enable, or role"},
				{"username flag", []string{"create", "--username", "alice@example.com", "--password-stdin"}, "flag provided but not defined: -username"},
				{"invalid email on create", []string{"create", "--email", "alice", "--password-stdin"}, "email must be a valid ASCII address such as alice@example.com, up to 254 characters"},
				{"invalid email on reset-password", []string{"reset-password", "--email", "alice@", "--password-stdin"}, "email must be a valid ASCII address such as alice@example.com, up to 254 characters"},
				{"invalid email on disable", []string{"disable", "--email", "alice@@example.com"}, "email must be a valid ASCII address such as alice@example.com, up to 254 characters"},
				{"enable with password-stdin", []string{"enable", "--email", "alice@example.com", "--password-stdin"}, "enable does not accept --password-stdin"},
				{"invalid email on enable", []string{"enable", "--email", "alice"}, "email must be a valid ASCII address such as alice@example.com, up to 254 characters"},
				{"invalid role", []string{"create", "--email", "bob@example.com", "--password-stdin", "--role", "admin"}, "role must be owner or member"},
				{"empty role", []string{"create", "--email", "bob@example.com", "--password-stdin", "--role="}, "role must be owner or member"},
				{"role on reset-password", []string{"reset-password", "--email", "alice@example.com", "--password-stdin", "--role", "member"}, "reset-password does not accept --role"},
				{"role on disable", []string{"disable", "--email", "alice@example.com", "--role", "owner"}, "disable does not accept --role"},
				{"role on enable", []string{"enable", "--email", "alice@example.com", "--role", "owner"}, "enable does not accept --role"},
				{"role without --role", []string{"role", "--email", "alice@example.com"}, "role requires --role owner or member"},
				{"role with password-stdin", []string{"role", "--email", "alice@example.com", "--role", "member", "--password-stdin"}, "role does not accept --password-stdin"},
				{"invalid role on role", []string{"role", "--email", "alice@example.com", "--role", "admin"}, "role must be owner or member"},
				{"empty role on role", []string{"role", "--email", "alice@example.com", "--role="}, "role must be owner or member"},
				{"invalid email on role", []string{"role", "--email", "alice", "--role", "member"}, "email must be a valid ASCII address such as alice@example.com, up to 254 characters"},
			} {
				if err = call("correct-password-123\n", tc.args...); err == nil || err.Error() != tc.want {
					t.Fatalf("%s: got %v, want %q", tc.name, err, tc.want)
				}
			}
			if err = login("correct-password-123"); err != nil {
				t.Fatalf("rejected commands changed the user: %v", err)
			}

			if err = call("replacement-password-123\n", "reset-password", "--email", "alice@example.com", "--password-stdin"); err != nil {
				t.Fatalf("reset-password: %v", err)
			}
			if err = login("correct-password-123"); !errors.Is(err, store.ErrUnauthorized) {
				t.Fatalf("old password after reset: %v", err)
			}
			if err = login("replacement-password-123"); err != nil {
				t.Fatalf("new password after reset: %v", err)
			}

			if err = call("", "disable", "--email", "alice@example.com"); err != nil {
				t.Fatalf("disable: %v", err)
			}
			if err = login("replacement-password-123"); !errors.Is(err, store.ErrUnauthorized) {
				t.Fatalf("login after disable: %v", err)
			}
			// enable lets the user sign in again with the same password.
			if err = call("", "enable", "--email", "missing@example.com"); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("enable a missing user: %v", err)
			}
			for i := 0; i < 2; i++ {
				if err = call("", "enable", "--email", "Alice@example.com"); err != nil {
					t.Fatalf("enable: %v", err)
				}
			}
			if err = login("replacement-password-123"); err != nil {
				t.Fatalf("login after enable: %v", err)
			}

			// create makes owners unless --role says otherwise.
			for _, tc := range []struct {
				email, role string
				args        []string
			}{
				{"alice@example.com", store.RoleOwner, nil},
				{"bob@example.com", store.RoleMember, []string{"--role", "member"}},
				{"carol@example.com", store.RoleOwner, []string{"--role=owner"}},
			} {
				if tc.email != "alice@example.com" {
					if err = call("correct-password-123\n", append([]string{"create", "--email", tc.email, "--password-stdin"}, tc.args...)...); err != nil {
						t.Fatalf("create %s: %v", tc.email, err)
					}
				}
				u, err := s.UserByEmail(ctx, tc.email)
				if err != nil || u.Role != tc.role {
					t.Fatalf("%s: %+v %v", tc.email, u, err)
				}
			}
		})
	}
}

// Only arguments rejected before the configuration is loaded and the database
// is opened are tested through run; users commands would open ./data/astovia.db.
func TestRunUsage(t *testing.T) {
	for _, args := range [][]string{{"serve", "x"}, {"foo"}} {
		err := run(args)
		if err == nil || err.Error() != "usage: astovia [serve | users create/reset-password/disable/enable/role --email ADDRESS [--password-stdin] [--role owner|member]]" {
			t.Fatalf("%q: got %v", args, err)
		}
	}
}

func TestUsersRoleCommand(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			ctx := context.Background()
			s, _ := testutil.Database(t, driver)
			call := func(args ...string) error {
				t.Helper()
				return account(ctx, s, args, strings.NewReader(""))
			}
			role := func(email string) string {
				t.Helper()
				u, err := s.UserByEmail(ctx, email)
				if err != nil {
					t.Fatal(err)
				}
				return u.Role
			}
			for _, email := range []string{"alice@example.com", "bob@example.com"} {
				if err := account(ctx, s, []string{"create", "--email", email, "--password-stdin"}, strings.NewReader("correct-password-123\n")); err != nil {
					t.Fatal(err)
				}
			}
			// bob, an owner, issues an invitation and a reset link for alice.
			alice, err := s.UserByEmail(ctx, "alice@example.com")
			if err != nil {
				t.Fatal(err)
			}
			bob, err := s.UserByEmail(ctx, "bob@example.com")
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().Unix()
			if err = s.CreateInvitation(ctx, bob.ID, store.Invitation{ID: "inv-1", TokenHash: "inv-hash", Email: "dave@example.com", Role: store.RoleMember, CreatedAt: now, ExpiresAt: now + store.InvitationLifetime}); err != nil {
				t.Fatal(err)
			}
			if err = s.CreatePasswordReset(ctx, bob.ID, store.PasswordReset{ID: "pwr-1", TokenHash: "pwr-hash", UserID: alice.ID, CreatedAt: now, ExpiresAt: now + store.PasswordResetLifetime}); err != nil {
				t.Fatal(err)
			}

			a, err := auth.New(s, make([]byte, 32), "http://localhost:8080")
			if err != nil {
				t.Fatal(err)
			}
			session, err := a.Login(ctx, "bob@example.com", "correct-password-123")
			if err != nil {
				t.Fatal(err)
			}

			if err = call("role", "--email", "missing@example.com", "--role", "owner"); !errors.Is(err, store.ErrNotFound) || err.Error() != "user not found" {
				t.Fatalf("missing user: %v", err)
			}
			// Promoting an owner changes nothing.
			if err = call("role", "--email", "Bob@Example.com", "--role", "owner"); err != nil || role("bob@example.com") != store.RoleOwner {
				t.Fatalf("owner to owner: %v", err)
			}
			if _, err = s.InvitationByToken(ctx, "inv-hash", now); err != nil {
				t.Fatalf("invitation revoked without a demotion: %v", err)
			}
			// Demoting revokes the links bob issued; no last-owner rule.
			if err = call("role", "--email", "bob@example.com", "--role", "member"); err != nil || role("bob@example.com") != store.RoleMember {
				t.Fatalf("demote: %v", err)
			}
			if _, err = s.InvitationByToken(ctx, "inv-hash", now); !errors.Is(err, store.ErrInvitationInvalid) {
				t.Fatalf("invitation after the demotion: %v", err)
			}
			if _, err = s.PasswordResetUser(ctx, "pwr-hash", now); !errors.Is(err, store.ErrResetInvalid) {
				t.Fatalf("reset link after the demotion: %v", err)
			}
			for _, email := range []string{"alice@example.com", "bob@example.com"} {
				if err = call("role", "--email", email, "--role", "member"); err != nil || role(email) != store.RoleMember {
					t.Fatalf("demote %s: %v", email, err)
				}
			}
			// The session of bob survives the demotion.
			if _, err = a.Refresh(ctx, session.Refresh); err != nil {
				t.Fatalf("refresh after the demotion: %v", err)
			}
			// A disabled user can be promoted, and stays disabled.
			if err = call("disable", "--email", "alice@example.com"); err != nil {
				t.Fatal(err)
			}
			if err = call("role", "--email", "alice@example.com", "--role", "owner"); err != nil || role("alice@example.com") != store.RoleOwner {
				t.Fatalf("promote a disabled user: %v", err)
			}
			if u, err := s.UserByEmail(ctx, "alice@example.com"); err != nil || u.Disabled == 0 {
				t.Fatalf("%+v %v", u, err)
			}
		})
	}
}
