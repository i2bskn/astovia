package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/i2bskn/astovia/internal/auth"
	"github.com/i2bskn/astovia/internal/store"
	"github.com/i2bskn/astovia/internal/testutil"
	"github.com/i2bskn/authlume/mcpoauth"
	"github.com/i2bskn/authlume/password"
	"github.com/i2bskn/authlume/session"
	"github.com/i2bskn/authlume/webauthn"
)

// verifyPassword checks password against the stored hash of the enabled user
// with email, as the sign-in does. A wrong password, a missing or disabled
// user and a user without a password are store.ErrUnauthorized.
func verifyPassword(ctx context.Context, s *store.Store, email, secret string) error {
	u, err := s.UserByEmail(ctx, email)
	if errors.Is(err, sql.ErrNoRows) {
		return store.ErrUnauthorized
	}
	if err != nil {
		return err
	}
	if u.Disabled != 0 {
		return store.ErrUnauthorized
	}
	a, err := password.New(func(context.Context, string) (password.Credential, error) {
		return password.Credential{Subject: u.ID, Hash: u.PasswordHash}, nil
	}, func(context.Context, string) (password.Decision, error) { return password.PasswordOnly, nil })
	if err != nil {
		return err
	}
	if _, err = a.Authenticate(ctx, email, secret); errors.Is(err, password.ErrCredentials) || errors.Is(err, password.ErrRecord) {
		return store.ErrUnauthorized
	}
	return err
}

// liveSession reports whether the browser session of cookie is still
// active.
func liveSession(t *testing.T, s *store.Store, cookie string) bool {
	t.Helper()
	m, err := session.New(s.Sessions(), session.Config{CookieName: testutil.SessionCookieName, Idle: 7 * 24 * time.Hour, Absolute: 30 * 24 * time.Hour, Now: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	r, _ := http.NewRequest(http.MethodGet, "https://astovia.example.com/api/v1/me", nil)
	r.AddCookie(&http.Cookie{Name: testutil.SessionCookieName, Value: cookie})
	_, err = m.Read(r)
	if err != nil && !errors.Is(err, session.ErrInvalid) {
		t.Fatal(err)
	}
	return err == nil
}

// grant gives userID a browser session and an API token, and returns the
// session cookie and the raw API token.
func grant(t *testing.T, s *store.Store, userID string) (cookie, token string) {
	t.Helper()
	ctx := context.Background()
	cookie, sid := testutil.Session(t, s, userID, time.Now())
	raw, hash, err := auth.NewAPIToken()
	if err != nil {
		t.Fatal(err)
	}
	if err = s.CreateAPIToken(ctx, store.APIToken{ID: userID + "-token", UserID: userID, Name: "cli", CreatedAt: time.Now().Unix()}, hash, sid, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	// An MCP authorization code, an MCP connection and a WebAuthn ceremony
	// in progress.
	grants++
	key := func(kind string) [32]byte { return sha256.Sum256(fmt.Appendf(nil, "%s-%s-%d", kind, userID, grants)) }
	now := time.Now().Truncate(time.Second)
	m := s.MCPOAuth(time.Now)
	code := mcpoauth.Code{ClientID: "client", RedirectURI: "https://client.example/callback", Subject: userID, Scopes: []string{"mcp"}, Challenge: "challenge", ExpiresAt: now.Add(time.Minute)}
	if err = m.CreateCode(store.WithApprovalSession(ctx, sid, now), key("code"), code); err != nil {
		t.Fatal(err)
	}
	conn := mcpoauth.Connection{
		ID: fmt.Sprintf("connection-%s-%d", userID, grants), Selector: key("selector"), Subject: userID, ClientID: "client",
		Scopes: []string{"mcp"}, CreatedAt: now, RefreshedAt: now,
		Access: key("access"), AccessExpiry: now.Add(time.Hour), Refresh: key("refresh"), RefreshExpiry: now.Add(24 * time.Hour),
	}
	if err = m.Create(ctx, conn); err != nil {
		t.Fatal(err)
	}
	if err = s.WebAuthnCeremonies(time.Now).Create(ctx, key("ceremony"), userID, []byte("record"), now.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	return cookie, raw
}

// grants numbers the calls of grant, so that each adds new rows.
var grants int

// mcpAccess returns how many MCP authorization codes, MCP connections and
// WebAuthn ceremonies userID has in the database of dsn; grant adds one of
// each.
func mcpAccess(t *testing.T, driver, dsn, userID string) int {
	t.Helper()
	db, err := sql.Open(map[string]string{"sqlite": "sqlite", "postgres": "pgx"}[driver], dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	total := 0
	for _, table := range []string{"mcp_oauth_codes", "mcp_oauth_connections", "webauthn_ceremonies"} {
		var n int
		if err := db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM "+table+" WHERE subject = $1", userID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		total += n
	}
	return total
}

// liveAPIToken reports whether the API token raw still authenticates.
func liveAPIToken(t *testing.T, s *store.Store, raw string) bool {
	t.Helper()
	_, err := auth.New(s).AuthenticateAPIToken(context.Background(), raw)
	if err != nil && !errors.Is(err, store.ErrUnauthorized) {
		t.Fatal(err)
	}
	return err == nil
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
		{strings.Repeat("x", 1024) + "\r\n", strings.Repeat("x", 1024), true},
		{strings.Repeat("x", 1025) + "\n", strings.Repeat("x", 1025), true},
		{strings.Repeat("x", 1027), "", false},
		{strings.Repeat("x", 1026) + "\n", "", false},
		{strings.Repeat("x", 4000), "", false},
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
			call := func(in string, args ...string) error {
				t.Helper()
				return account(ctx, s, args, strings.NewReader(in))
			}
			login := func(password string) error {
				t.Helper()
				return verifyPassword(ctx, s, "alice@example.com", password)
			}

			// The trailing \r\n is removed before hashing.
			var err error
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
				{"no subcommand", nil, "users command requires create, reset-password, disable, enable, role, unlink, approve-oidc, or reset-mfa"},
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
				{"short password", []string{"reset-password", "--email", "alice@example.com", "--password-stdin"}, "password must contain 12–1024 UTF-8 bytes"},
			} {
				in := "correct-password-123\n"
				if tc.name == "short password" {
					in = "short-pw\n"
				}
				if err = call(in, tc.args...); err == nil || err.Error() != tc.want {
					t.Fatalf("%s: got %v, want %q", tc.name, err, tc.want)
				}
			}
			if err = login("correct-password-123"); err != nil {
				t.Fatalf("rejected commands changed the user: %v", err)
			}
			// Passwords are stored with Argon2id.
			if u, err := s.UserByEmail(ctx, "alice@example.com"); err != nil || !strings.HasPrefix(u.PasswordHash, "v2$argon2id$") {
				t.Fatalf("%+v %v", u, err)
			}

			alice, err := s.UserByEmail(ctx, "alice@example.com")
			if err != nil {
				t.Fatal(err)
			}
			cookie, token := grant(t, s, alice.ID)
			if err = call("replacement-password-123\n", "reset-password", "--email", "alice@example.com", "--password-stdin"); err != nil {
				t.Fatalf("reset-password: %v", err)
			}
			if liveSession(t, s, cookie) || liveAPIToken(t, s, token) {
				t.Fatal("reset-password kept the session or the API token")
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
		if err == nil || err.Error() != "usage: astovia [serve | users create/reset-password/disable/enable/role/unlink/approve-oidc/reset-mfa --email ADDRESS [--password-stdin] [--no-password] [--role owner|member] [--all]]" {
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

			cookie, _ := grant(t, s, bob.ID)

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
			if !liveSession(t, s, cookie) {
				t.Fatal("session revoked by the demotion")
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
			s, dsn := testutil.Database(t, driver)
			var err error
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
			link := func(email, subject string) error {
				t.Helper()
				_, _, err := s.LinkOIDCUser(ctx, "https://idp.example.com", subject, email, store.User{}, time.Now().Unix())
				return err
			}
			approved := func(email string) bool {
				t.Helper()
				u := user(email)
				return u.OidcLinkApprovedUntil.Valid && u.OidcLinkApprovedUntil.Int64 > time.Now().Unix()
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
			// an owner unless --role says otherwise, not linked yet and
			// approved for linking for 7 days.
			if err = call("create", "--email", "Alice@Example.com", "--no-password"); err != nil {
				t.Fatalf("create: %v", err)
			}
			if err = call("create", "--email", "bob@example.com", "--no-password", "--role", "member"); err != nil {
				t.Fatalf("create a member: %v", err)
			}
			for email, role := range map[string]string{"alice@example.com": store.RoleOwner, "bob@example.com": store.RoleMember} {
				u := user(email)
				if u.PasswordHash != store.NoPasswordHash || u.Role != role || u.Disabled != 0 || linked(email) || !approved(email) {
					t.Fatalf("%s: %+v", email, u)
				}
				if until := u.OidcLinkApprovedUntil.Int64 - time.Now().Unix(); until > int64(store.OIDCLinkApproval/time.Second) || until < int64(store.OIDCLinkApproval/time.Second)-60 {
					t.Fatalf("%s: approved for %d seconds", email, until)
				}
			}
			for _, secret := range []string{store.NoPasswordHash, "correct-password-123", ""} {
				if err = verifyPassword(ctx, s, "alice@example.com", secret); !errors.Is(err, store.ErrUnauthorized) {
					t.Fatalf("password %q: %v", secret, err)
				}
			}
			if err = call("create", "--email", "alice@example.com", "--no-password"); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("duplicate create: %v", err)
			}

			for _, email := range []string{"alice@example.com", "bob@example.com"} {
				if err = link(email, "sub-"+email); err != nil {
					t.Fatalf("link %s: %v", email, err)
				}
				// Linking spends the approval.
				if approved(email) {
					t.Fatalf("%s: approval kept after linking", email)
				}
			}
			alice := user("alice@example.com")
			bob := user("bob@example.com")
			aliceCookie, aliceToken := grant(t, s, alice.ID)
			bobCookie, bobToken := grant(t, s, bob.ID)
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

			// approve-oidc refuses a linked user, a missing user and the
			// flags it does not take.
			for _, tc := range []struct {
				name string
				args []string
				want string
			}{
				{"linked", []string{"approve-oidc", "--email", "alice@example.com"}, store.ErrOIDCLinked.Error()},
				{"missing", []string{"approve-oidc", "--email", "missing@example.com"}, "user not found"},
				{"role", []string{"approve-oidc", "--email", "alice@example.com", "--role", "owner"}, "approve-oidc does not accept --role"},
				{"password-stdin", []string{"approve-oidc", "--email", "alice@example.com", "--password-stdin"}, "approve-oidc does not accept --password-stdin"},
				{"no-password", []string{"approve-oidc", "--email", "alice@example.com", "--no-password"}, "approve-oidc does not accept --no-password"},
				{"all", []string{"approve-oidc", "--all"}, "approve-oidc does not accept --all"},
			} {
				if err = call(tc.args...); err == nil || err.Error() != tc.want {
					t.Fatalf("%s: got %v, want %q", tc.name, err, tc.want)
				}
			}

			// unlink --email clears one user, twice without an error, keeps
			// the role and the password hash, revokes the sessions and API
			// tokens and gives no approval to link again.
			for range 2 {
				if err = call("unlink", "--email", "Alice@Example.com"); err != nil {
					t.Fatalf("unlink: %v", err)
				}
			}
			if linked("alice@example.com") || !linked("bob@example.com") {
				t.Fatal("unlink --email")
			}
			if u := user("alice@example.com"); u.Role != alice.Role || u.PasswordHash != alice.PasswordHash || u.Disabled != 0 || approved("alice@example.com") {
				t.Fatalf("%+v", u)
			}
			if liveSession(t, s, aliceCookie) || liveAPIToken(t, s, aliceToken) || mcpAccess(t, driver, dsn, alice.ID) != 0 {
				t.Fatal("unlink kept the session, the API token, the MCP access or the ceremony")
			}
			if !liveSession(t, s, bobCookie) || !liveAPIToken(t, s, bobToken) || mcpAccess(t, driver, dsn, bob.ID) != 3 {
				t.Fatal("unlink of alice revoked bob")
			}
			// Without an approval the same email address is not linked again.
			if err = link("alice@example.com", "sub-alice-2"); !errors.Is(err, store.ErrOIDCNotApproved) {
				t.Fatalf("link without an approval: %v", err)
			}
			// approve-oidc lets another identity link the user again.
			if err = call("approve-oidc", "--email", "Alice@Example.com"); err != nil || !approved("alice@example.com") {
				t.Fatalf("approve-oidc: %v", err)
			}
			if err = link("alice@example.com", "sub-alice-2"); err != nil {
				t.Fatalf("link after approve-oidc: %v", err)
			}
			aliceCookie, aliceToken = grant(t, s, alice.ID)

			// unlink --all clears everyone; --email is not needed.
			if err = call("unlink", "--all"); err != nil {
				t.Fatalf("unlink --all: %v", err)
			}
			if linked("alice@example.com") || linked("bob@example.com") || approved("alice@example.com") || approved("bob@example.com") {
				t.Fatal("unlink --all")
			}
			for _, c := range []string{aliceCookie, bobCookie} {
				if liveSession(t, s, c) {
					t.Fatal("unlink --all kept a session")
				}
			}
			for _, tok := range []string{aliceToken, bobToken} {
				if liveAPIToken(t, s, tok) {
					t.Fatal("unlink --all kept an API token")
				}
			}
			if mcpAccess(t, driver, dsn, alice.ID) != 0 || mcpAccess(t, driver, dsn, bob.ID) != 0 {
				t.Fatal("unlink --all kept the MCP access or a ceremony")
			}
			// A user that is not linked keeps its session.
			unlinkedCookie, _ := grant(t, s, user("alice@example.com").ID)
			if err = call("unlink", "--all"); err != nil {
				t.Fatalf("unlink --all again: %v", err)
			}
			if !liveSession(t, s, unlinkedCookie) || mcpAccess(t, driver, dsn, alice.ID) != 3 {
				t.Fatal("unlink --all revoked a user that was not linked")
			}
		})
	}
}

func TestUsersResetMFA(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			ctx := context.Background()
			s, dsn := testutil.Database(t, driver)
			call := func(args ...string) error {
				t.Helper()
				return account(ctx, s, args, strings.NewReader(""))
			}
			hash, err := auth.Password("correct-password-123")
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().Unix()
			for _, name := range []string{"alice", "bob"} {
				id := name + "-id"
				if err = s.CreateUser(ctx, store.User{ID: id, Email: name + "@example.com", PasswordHash: hash, CreatedAt: now, Role: store.RoleOwner}); err != nil {
					t.Fatal(err)
				}
				_, sid := testutil.Session(t, s, id, time.Now())
				codes := []store.RecoveryCode{{ID: name + "-code", Hash: name + "-code-hash"}}
				if _, err = s.EnrollTOTP(ctx, store.MFATOTP{ID: name + "-totp", UserID: id, SecretSealed: "sealed", LastStep: 1, CreatedAt: now}, codes, store.EnrollOptions{SessionID: sid, Now: now}); err != nil {
					t.Fatal(err)
				}
				enroll := store.WithWebAuthnEnrollment(ctx, store.WebAuthnEnrollment{UserID: id, Name: "Key", SessionID: sid, Now: now})
				if err = s.WebAuthnCredentials(time.Now).Insert(enroll, id, webauthn.Credential{ID: []byte(name + "-key"), PublicKey: []byte{1}}, false); err != nil {
					t.Fatal(err)
				}
			}
			alice, err := s.UserByEmail(ctx, "alice@example.com")
			if err != nil {
				t.Fatal(err)
			}
			cookie, token := grant(t, s, alice.ID)
			enrolled := func(id string) bool {
				t.Helper()
				status, err := s.MFAStatus(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				if status.Enrolled() != (status.RecoveryCodes > 0) {
					t.Fatalf("%s: %+v", id, status)
				}
				return status.Enrolled()
			}

			for _, tc := range []struct {
				name string
				args []string
				want string
			}{
				{"no email", []string{"reset-mfa"}, "email must be a valid ASCII address such as alice@example.com, up to 254 characters"},
				{"invalid email", []string{"reset-mfa", "--email", "alice"}, "email must be a valid ASCII address such as alice@example.com, up to 254 characters"},
				{"role", []string{"reset-mfa", "--email", "alice@example.com", "--role", "owner"}, "reset-mfa does not accept --role"},
				{"password-stdin", []string{"reset-mfa", "--email", "alice@example.com", "--password-stdin"}, "reset-mfa does not accept --password-stdin"},
				{"no-password", []string{"reset-mfa", "--email", "alice@example.com", "--no-password"}, "reset-mfa does not accept --no-password"},
				{"all", []string{"reset-mfa", "--email", "alice@example.com", "--all"}, "reset-mfa does not accept --all"},
				{"all without email", []string{"reset-mfa", "--all"}, "reset-mfa does not accept --all"},
				{"extra arguments", []string{"reset-mfa", "--email", "alice@example.com", "extra"}, "unexpected arguments"},
			} {
				if err = call(tc.args...); err == nil || err.Error() != tc.want {
					t.Fatalf("%s: got %v, want %q", tc.name, err, tc.want)
				}
			}
			if err = call("reset-mfa", "--email", "missing@example.com"); !errors.Is(err, store.ErrNotFound) || err.Error() != "user not found" {
				t.Fatalf("missing user: %v", err)
			}
			if !enrolled("alice-id") || !enrolled("bob-id") {
				t.Fatal("rejected commands reset a user")
			}

			// reset-mfa clears one user, twice without an error, keeps the
			// password and revokes the sessions and API tokens.
			for range 2 {
				if err = call("reset-mfa", "--email", "Alice@Example.com"); err != nil {
					t.Fatalf("reset-mfa: %v", err)
				}
			}
			if enrolled("alice-id") || !enrolled("bob-id") {
				t.Fatal("reset-mfa")
			}
			if liveSession(t, s, cookie) || liveAPIToken(t, s, token) || mcpAccess(t, driver, dsn, alice.ID) != 0 {
				t.Fatal("reset-mfa kept the session, the API token, the MCP access or the ceremony")
			}
			if err = verifyPassword(ctx, s, "alice@example.com", "correct-password-123"); err != nil {
				t.Fatalf("password after reset-mfa: %v", err)
			}
			// A disabled user can be reset as well.
			if err = s.ChangeUser(ctx, "bob@example.com", "", true, now); err != nil {
				t.Fatal(err)
			}
			if err = call("reset-mfa", "--email", "bob@example.com"); err != nil {
				t.Fatalf("reset-mfa of a disabled user: %v", err)
			}
			if enrolled("bob-id") {
				t.Fatal("reset-mfa of a disabled user")
			}
		})
	}
}
