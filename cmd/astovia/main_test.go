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
				{"no subcommand", nil, "users command requires create, reset-password, disable, enable, role, or unlink"},
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
				{"no-password with password-stdin", []string{"create", "--email", "bob@example.com", "--no-password", "--password-stdin"}, "--no-password and --password-stdin cannot be used together"},
				{"no-password on reset-password", []string{"reset-password", "--email", "alice@example.com", "--password-stdin", "--no-password"}, "reset-password does not accept --no-password"},
				{"no-password on disable", []string{"disable", "--email", "alice@example.com", "--no-password"}, "disable does not accept --no-password"},
				{"no-password on enable", []string{"enable", "--email", "alice@example.com", "--no-password"}, "enable does not accept --no-password"},
				{"no-password on role", []string{"role", "--email", "alice@example.com", "--role", "member", "--no-password"}, "role does not accept --no-password"},
				{"all on create", []string{"create", "--email", "bob@example.com", "--password-stdin", "--all"}, "create does not accept --all"},
				{"all on disable", []string{"disable", "--email", "alice@example.com", "--all"}, "disable does not accept --all"},
				{"all on role", []string{"role", "--email", "alice@example.com", "--role", "member", "--all"}, "role does not accept --all"},
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
		if err == nil || err.Error() != "usage: astovia [serve | users create/reset-password/disable/enable/role/unlink --email ADDRESS [--password-stdin] [--no-password] [--role owner|member] [--all]]" {
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

func TestUsersNoPasswordAndUnlink(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			ctx := context.Background()
			s, _ := testutil.Database(t, driver)
			a, err := auth.New(s, make([]byte, 32), "http://localhost:8080")
			if err != nil {
				t.Fatal(err)
			}
			call := func(args ...string) error {
				t.Helper()
				// No password is read: the input is empty.
				return account(ctx, s, args, strings.NewReader(""))
			}
			user := func(email string) store.User {
				t.Helper()
				u, err := s.UserByEmail(ctx, email)
				if err != nil {
					t.Fatal(err)
				}
				return u
			}
			link := func(email, subject string) {
				t.Helper()
				if _, _, err := s.LinkOIDCUser(ctx, "https://idp.example.com", subject, email, store.User{}, time.Now().Unix()); err != nil {
					t.Fatalf("link %s: %v", email, err)
				}
			}
			linked := func(email string) bool {
				t.Helper()
				u := user(email)
				if u.OidcIssuer.Valid != u.OidcSubject.Valid {
					t.Fatalf("%s: half an identity: %+v", email, u)
				}
				return u.OidcSubject.Valid
			}

			// create --no-password makes a user that no password signs in,
			// an owner unless --role says otherwise, and not linked yet.
			if err = call("create", "--email", "Alice@Example.com", "--no-password"); err != nil {
				t.Fatalf("create: %v", err)
			}
			if err = call("create", "--email", "bob@example.com", "--no-password", "--role", "member"); err != nil {
				t.Fatalf("create a member: %v", err)
			}
			for email, role := range map[string]string{"alice@example.com": store.RoleOwner, "bob@example.com": store.RoleMember} {
				if u := user(email); u.PasswordHash != store.NoPasswordHash || u.Role != role || u.Disabled != 0 || linked(email) {
					t.Fatalf("%s: %+v", email, u)
				}
			}
			for _, password := range []string{store.NoPasswordHash, "correct-password-123", ""} {
				if _, err = a.Login(ctx, "alice@example.com", password); !errors.Is(err, store.ErrUnauthorized) {
					t.Fatalf("password %q: %v", password, err)
				}
			}
			if err = call("create", "--email", "alice@example.com", "--no-password"); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("duplicate create: %v", err)
			}

			link("alice@example.com", "sub-alice")
			link("bob@example.com", "sub-bob")
			alice := user("alice@example.com")
			session, err := a.LoginUser(ctx, alice)
			if err != nil {
				t.Fatal(err)
			}
			for _, tc := range []struct {
				name string
				args []string
				want string
			}{
				{"neither email nor all", []string{"unlink"}, "unlink requires exactly one of --email and --all"},
				{"email and all", []string{"unlink", "--email", "alice@example.com", "--all"}, "unlink requires exactly one of --email and --all"},
				{"role", []string{"unlink", "--email", "alice@example.com", "--role", "owner"}, "unlink does not accept --role"},
				{"password-stdin", []string{"unlink", "--email", "alice@example.com", "--password-stdin"}, "unlink does not accept --password-stdin"},
				{"no-password", []string{"unlink", "--email", "alice@example.com", "--no-password"}, "unlink does not accept --no-password"},
				{"all with role", []string{"unlink", "--all", "--role", "owner"}, "unlink does not accept --role"},
				{"invalid email", []string{"unlink", "--email", "alice"}, "email must be a valid ASCII address such as alice@example.com, up to 254 characters"},
				{"empty email", []string{"unlink", "--email", ""}, "email must be a valid ASCII address such as alice@example.com, up to 254 characters"},
				{"extra arguments", []string{"unlink", "--all", "extra"}, "unexpected arguments"},
			} {
				if err = call(tc.args...); err == nil || err.Error() != tc.want {
					t.Fatalf("%s: got %v, want %q", tc.name, err, tc.want)
				}
			}
			if err = call("unlink", "--email", "missing@example.com"); !errors.Is(err, store.ErrNotFound) || err.Error() != "user not found" {
				t.Fatalf("missing user: %v", err)
			}
			if !linked("alice@example.com") || !linked("bob@example.com") {
				t.Fatal("rejected commands unlinked a user")
			}

			// unlink --email clears one user, twice without an error, and
			// keeps the session, the role and the password hash.
			for range 2 {
				if err = call("unlink", "--email", "Alice@Example.com"); err != nil {
					t.Fatalf("unlink: %v", err)
				}
			}
			if linked("alice@example.com") || !linked("bob@example.com") {
				t.Fatal("unlink --email")
			}
			if u := user("alice@example.com"); u.Role != alice.Role || u.PasswordHash != alice.PasswordHash || u.Disabled != 0 {
				t.Fatalf("%+v", u)
			}
			if _, err = a.Refresh(ctx, session.Refresh); err != nil {
				t.Fatalf("refresh after unlink: %v", err)
			}
			// Another identity links the user again.
			link("alice@example.com", "sub-alice-2")

			// unlink --all clears everyone; --email is not needed.
			if err = call("unlink", "--all"); err != nil {
				t.Fatalf("unlink --all: %v", err)
			}
			if linked("alice@example.com") || linked("bob@example.com") {
				t.Fatal("unlink --all")
			}
			if err = call("unlink", "--all"); err != nil {
				t.Fatalf("unlink --all again: %v", err)
			}
		})
	}
}
