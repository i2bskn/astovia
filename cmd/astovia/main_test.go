package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

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
				_, err := a.Login(ctx, "alice", password)
				return err
			}

			// The trailing \r\n is removed before hashing.
			if err = call("correct-password-123\r\n", "create", "--username", "alice", "--password-stdin"); err != nil {
				t.Fatalf("create: %v", err)
			}
			if err = login("correct-password-123"); err != nil {
				t.Fatalf("login after create: %v", err)
			}
			if err = call("correct-password-123\n", "create", "--username", "alice", "--password-stdin"); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("duplicate create: %v", err)
			}
			if err = call("replacement-password-123\n", "reset-password", "--username", "missing", "--password-stdin"); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("reset of a missing user: %v", err)
			}

			for _, tc := range []struct {
				name string
				args []string
				want string
			}{
				// alice exists, so a missing check would disable her instead of failing.
				{"disable with password-stdin", []string{"disable", "--username", "alice", "--password-stdin"}, "disable does not accept --password-stdin"},
				{"extra arguments", []string{"create", "--username", "alice", "extra"}, "unexpected arguments"},
				{"unknown subcommand", []string{"rename", "--username", "alice"}, "unknown users command"},
				{"no subcommand", nil, "users command requires create, reset-password, or disable"},
			} {
				if err = call("correct-password-123\n", tc.args...); err == nil || err.Error() != tc.want {
					t.Fatalf("%s: got %v, want %q", tc.name, err, tc.want)
				}
			}
			if err = login("correct-password-123"); err != nil {
				t.Fatalf("rejected commands changed the user: %v", err)
			}

			if err = call("replacement-password-123\n", "reset-password", "--username", "alice", "--password-stdin"); err != nil {
				t.Fatalf("reset-password: %v", err)
			}
			if err = login("correct-password-123"); !errors.Is(err, store.ErrUnauthorized) {
				t.Fatalf("old password after reset: %v", err)
			}
			if err = login("replacement-password-123"); err != nil {
				t.Fatalf("new password after reset: %v", err)
			}

			if err = call("", "disable", "--username", "alice"); err != nil {
				t.Fatalf("disable: %v", err)
			}
			if err = login("replacement-password-123"); !errors.Is(err, store.ErrUnauthorized) {
				t.Fatalf("login after disable: %v", err)
			}
		})
	}
}

// Only arguments rejected before the configuration is loaded and the database
// is opened are tested through run; users commands would open ./data/astovia.db.
func TestRunUsage(t *testing.T) {
	for _, args := range [][]string{{"serve", "x"}, {"foo"}} {
		err := run(args)
		if err == nil || !strings.HasPrefix(err.Error(), "usage: astovia") {
			t.Fatalf("%q: got %v", args, err)
		}
	}
}
