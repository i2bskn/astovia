package store_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/i2bskn/astovia/internal/store"
)

// TestSchemaUpgrade is the Go part of scripts/check-schema-upgrade.sh, which
// applies the current schema with Atlas to databases made from the schema
// before authlume. It runs only from that script: ASTOVIA_TEST_UPGRADE_STAGE
// is "setup" to make the databases from db/testdata/upgrade, and "verify"
// to check them after the apply. ASTOVIA_TEST_UPGRADE_SQLITE is the path of
// the SQLite file and ASTOVIA_TEST_UPGRADE_POSTGRES the URL of an empty
// PostgreSQL database.
func TestSchemaUpgrade(t *testing.T) {
	stage := os.Getenv("ASTOVIA_TEST_UPGRADE_STAGE")
	if stage == "" {
		t.Skip("run by scripts/check-schema-upgrade.sh")
	}
	targets := map[string]string{"sqlite": os.Getenv("ASTOVIA_TEST_UPGRADE_SQLITE"), "postgres": os.Getenv("ASTOVIA_TEST_UPGRADE_POSTGRES")}
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			target := targets[driver]
			if target == "" {
				t.Fatal("no database given")
			}
			dsn := target
			if driver == "sqlite" {
				dsn = "file:" + target
			}
			switch stage {
			case "setup":
				setupUpgrade(t, driver, target, dsn)
			case "verify":
				verifyUpgrade(t, driver, dsn)
			default:
				t.Fatalf("stage %q", stage)
			}
		})
	}
}

// setupUpgrade makes the database of the schema before authlume, with rows.
func setupUpgrade(t *testing.T, driver, target, dsn string) {
	ctx := context.Background()
	schema, err := os.ReadFile(filepath.Join("..", "..", "db", "testdata", "upgrade", driver+".sql"))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := os.ReadFile(filepath.Join("..", "..", "db", "testdata", "upgrade", "rows.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if driver == "sqlite" {
		if err = os.Remove(target); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	db, err := sql.Open(map[string]string{"sqlite": "sqlite", "postgres": "pgx"}[driver], dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, script := range [][]byte{schema, rows} {
		if _, err = db.ExecContext(ctx, string(script)); err != nil {
			t.Fatal(err)
		}
	}
}

// verifyUpgrade checks the database after the apply: the store accepts the
// schema, the replaced tables are gone, the rows of the kept tables are
// unchanged, and the second factors of the earlier version are kept as
// unusable ones (spec §6.7).
func verifyUpgrade(t *testing.T, driver, dsn string) {
	ctx := context.Background()
	s, err := store.Open(ctx, driver, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.CheckSchema(ctx); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open(map[string]string{"sqlite": "sqlite", "postgres": "pgx"}[driver], dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	count := func(query string) (int, error) {
		var n int
		err := db.QueryRowContext(ctx, query).Scan(&n)
		return n, err
	}
	for _, table := range []string{"sessions", "refresh_tokens", "oauth_clients", "oauth_grants", "oauth_tokens", "mfa_used_challenges"} {
		if _, err = count("SELECT COUNT(*) FROM " + table); err == nil {
			t.Errorf("%s was not dropped", table)
		}
	}
	for query, want := range map[string]int{
		"SELECT COUNT(*) FROM users":                                                       3,
		"SELECT COUNT(*) FROM users WHERE oidc_link_approved_until IS NULL":                3,
		"SELECT COUNT(*) FROM api_tokens":                                                  1,
		"SELECT COUNT(*) FROM mfa_recovery_codes":                                          2,
		"SELECT COUNT(*) FROM mfa_totp WHERE id = ''":                                      1,
		"SELECT COUNT(*) FROM mfa_webauthn_credentials WHERE version = '' AND passkey = 0": 1,
		"SELECT COUNT(*) FROM web_sessions":                                                0,
		"SELECT COUNT(*) FROM mcp_oauth_connections":                                       0,
	} {
		if got, err := count(query); err != nil || got != want {
			t.Errorf("%s: %d %v, want %d", query, got, err, want)
		}
	}

	alice, err := s.UserByEmail(ctx, "alice@example.com")
	if err != nil || alice.ID != "alice-id" || alice.Role != store.RoleOwner || alice.Disabled != 0 {
		t.Fatalf("alice: %+v %v", alice, err)
	}
	if bob, err := s.UserByOIDC(ctx, "https://idp.example", "bob-sub"); err != nil || bob.ID != "bob-id" {
		t.Fatalf("bob: %+v %v", bob, err)
	}
	if carol, err := s.UserByID(ctx, "carol-id"); err != nil || carol.Disabled == 0 {
		t.Fatalf("carol: %+v %v", carol, err)
	}
	status, err := s.MFAStatus(ctx, "alice-id")
	if err != nil || !status.Enrolled() || status.UsableTOTP() || status.UsableWebAuthn() != 0 || status.RecoveryCodes != 1 {
		t.Fatalf("alice's second factors: %+v %v", status, err)
	}
	if ok, err := s.UseRecoveryCode(ctx, "alice-id", "code-hash-1", 100); err != nil || !ok {
		t.Fatalf("recovery code: %v %v", ok, err)
	}
	if inv, err := s.InvitationByToken(ctx, "invitation-hash-1", 100); err != nil || inv.Email != "dave@example.com" {
		t.Fatalf("invitation: %+v %v", inv, err)
	}
	if u, err := s.PasswordResetUser(ctx, "reset-hash-1", 100); err != nil || u.ID != "alice-id" {
		t.Fatalf("password reset: %+v %v", u, err)
	}
}
