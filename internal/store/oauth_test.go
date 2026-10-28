package store_test

import (
	"context"
	"database/sql"
	"errors"
	"github.com/i2bskn/astovia/internal/store"
	"github.com/i2bskn/astovia/internal/testutil"
	"reflect"
	"testing"
)

const oauthNow = tokenTestNow

var errRejected = errors.New("rejected by issue")

type oauthFixture struct {
	t     *testing.T
	s     *store.Store
	raw   *sql.DB
	users map[string]store.User
}

// newOAuthFixture creates alice and bob with sessions "<name>-session", a
// public CIMD client and a confidential DCR client.
func newOAuthFixture(t *testing.T, driver string) *oauthFixture {
	t.Helper()
	ctx := context.Background()
	s, dsn := testutil.Database(t, driver)
	users := map[string]store.User{}
	for _, name := range []string{"alice", "bob"} {
		u := store.User{ID: name + "-id", Email: name + "@example.com", PasswordHash: "x", Role: store.RoleOwner}
		if err := s.CreateUser(ctx, u); err != nil {
			t.Fatal(err)
		}
		sid := name + "-session"
		if err := s.Login(ctx, u, store.Session{ID: sid, UserID: u.ID, ExpiresAt: oauthNow + 7*86400, CreatedAt: oauthNow}, store.Refresh{Hash: "refresh-" + name, SessionID: sid, CreatedAt: oauthNow}); err != nil {
			t.Fatal(err)
		}
		users[name] = u
	}
	if err := s.UpsertCIMDClient(ctx, store.OAuthClient{ClientID: "https://claude.ai/client", Name: "Claude", RedirectURIs: []string{"https://claude.ai/cb"}, MetadataExpiresAt: sql.NullInt64{Int64: oauthNow + 3600, Valid: true}, CreatedAt: oauthNow, UpdatedAt: oauthNow}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateOAuthClient(ctx, store.OAuthClient{ClientID: "dcr_client", Kind: "dcr", Name: "Tool", RedirectURIs: []string{"http://localhost/cb"}, AuthMethod: "client_secret_basic", CreatedAt: oauthNow, UpdatedAt: oauthNow}, "secret-hash"); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open(map[string]string{"sqlite": "sqlite", "postgres": "pgx"}[driver], dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	return &oauthFixture{t: t, s: s, raw: raw, users: users}
}

// count returns the number of rows of table for userID.
func (f *oauthFixture) count(table, userID string) int {
	f.t.Helper()
	var n int
	if err := f.raw.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM "+table+" WHERE user_id = '"+userID+"'").Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

// grant stores a consent of user with the code hash "code-<id>".
func (f *oauthFixture) grant(user, id, scope string, created int64) store.OAuthGrant {
	f.t.Helper()
	g := store.OAuthGrant{ID: id, UserID: user + "-id", ClientID: "https://claude.ai/client", ClientName: "Claude", Scope: scope, Resource: "https://astovia.test/mcp", RedirectURI: "https://claude.ai/cb", CodeHash: "code-" + id, CodeChallenge: "challenge", CodeExpiresAt: created + 60, CreatedAt: created, ExpiresAt: created + 60}
	if err := f.s.CreateOAuthGrant(context.Background(), g, user+"-session", created); err != nil {
		f.t.Fatalf("grant %s: %v", id, err)
	}
	return g
}

// tokens returns an issue callback that yields an access token "access-<tag>"
// and, when refresh is true, a refresh token "refresh-<tag>".
func tokens(tag string, now int64, refresh bool) func(store.OAuthGrant) ([]store.OAuthToken, error) {
	return func(store.OAuthGrant) ([]store.OAuthToken, error) {
		out := []store.OAuthToken{{Hash: "access-" + tag, Kind: store.OAuthAccess, CreatedAt: now, ExpiresAt: now + 3600}}
		if refresh {
			out = append(out, store.OAuthToken{Hash: "refresh-" + tag, Kind: store.OAuthRefresh, CreatedAt: now, ExpiresAt: now + 90*86400})
		}
		return out, nil
	}
}

func reject(store.OAuthGrant) ([]store.OAuthToken, error) { return nil, errRejected }

// redeem exchanges the code of grant id and fails the test on error.
func (f *oauthFixture) redeem(id, tag string, now int64) store.OAuthGrant {
	f.t.Helper()
	g, err := f.s.RedeemOAuthCode(context.Background(), "code-"+id, now, tokens(tag, now, true))
	if err != nil {
		f.t.Fatalf("redeem %s: %v", id, err)
	}
	return g
}

func (f *oauthFixture) authenticate(hash string, now int64) error {
	f.t.Helper()
	_, _, _, err := f.s.AuthenticateOAuthAccess(context.Background(), hash, now)
	return err
}

func TestOAuthClients(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			ctx := context.Background()
			f := newOAuthFixture(t, driver)
			if _, err := f.s.OAuthClient(ctx, "missing"); !errors.Is(err, store.ErrClientNotFound) {
				t.Fatalf("missing client: %v", err)
			}
			c, err := f.s.OAuthClient(ctx, "dcr_client")
			want := store.OAuthClient{ClientID: "dcr_client", Kind: "dcr", Name: "Tool", RedirectURIs: []string{"http://localhost/cb"}, AuthMethod: "client_secret_basic", SecretHash: sql.NullString{String: "secret-hash", Valid: true}, CreatedAt: oauthNow, UpdatedAt: oauthNow}
			if err != nil || !reflect.DeepEqual(c, want) {
				t.Fatalf("DCR client: %+v %v", c, err)
			}
			if err = f.s.CreateOAuthClient(ctx, store.OAuthClient{ClientID: "dcr_public", Kind: "dcr", Name: "P", RedirectURIs: []string{"https://a.example/cb", "https://b.example/cb"}, AuthMethod: "none"}, ""); err != nil {
				t.Fatal(err)
			}
			if c, err = f.s.OAuthClient(ctx, "dcr_public"); err != nil || c.SecretHash.Valid || len(c.RedirectURIs) != 2 {
				t.Fatalf("public client: %+v %v", c, err)
			}
			// The upsert refreshes the cached metadata and keeps created_at.
			if err = f.s.UpsertCIMDClient(ctx, store.OAuthClient{ClientID: "https://claude.ai/client", Name: "Claude 2", RedirectURIs: []string{"https://claude.ai/cb2"}, MetadataExpiresAt: sql.NullInt64{Int64: oauthNow + 7200, Valid: true}, CreatedAt: oauthNow + 10, UpdatedAt: oauthNow + 10}); err != nil {
				t.Fatal(err)
			}
			c, err = f.s.OAuthClient(ctx, "https://claude.ai/client")
			want = store.OAuthClient{ClientID: "https://claude.ai/client", Kind: "cimd", Name: "Claude 2", RedirectURIs: []string{"https://claude.ai/cb2"}, AuthMethod: "none", MetadataExpiresAt: sql.NullInt64{Int64: oauthNow + 7200, Valid: true}, CreatedAt: oauthNow, UpdatedAt: oauthNow + 10}
			if err != nil || !reflect.DeepEqual(c, want) {
				t.Fatalf("CIMD client: %+v %v", c, err)
			}
		})
	}
}

func TestOAuthGrantSessionCheck(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			ctx := context.Background()
			f := newOAuthFixture(t, driver)
			g := store.OAuthGrant{ID: "g1", UserID: "alice-id", ClientID: "https://claude.ai/client", ClientName: "Claude", Scope: "mcp", Resource: "r", RedirectURI: "https://claude.ai/cb", CodeHash: "code-g1", CodeChallenge: "c", CodeExpiresAt: oauthNow + 60, CreatedAt: oauthNow, ExpiresAt: oauthNow + 60}
			for name, tc := range map[string]struct {
				sid  string
				now  int64
				user string
			}{"other user's session": {"bob-session", oauthNow, "alice-id"}, "unknown session": {"missing", oauthNow, "alice-id"}, "expired session": {"alice-session", oauthNow + 7*86400, "alice-id"}, "unknown user": {"alice-session", oauthNow, "ghost-id"}} {
				bad := g
				bad.UserID = tc.user
				if err := f.s.CreateOAuthGrant(ctx, bad, tc.sid, tc.now); !errors.Is(err, store.ErrUnauthorized) {
					t.Fatalf("%s: %v", name, err)
				}
			}
			bad := g
			bad.ClientID = "dcr_missing"
			if err := f.s.CreateOAuthGrant(ctx, bad, "alice-session", oauthNow); !errors.Is(err, store.ErrClientNotFound) {
				t.Fatalf("unknown client: %v", err)
			}
			if f.count("oauth_grants", "alice-id") != 0 {
				t.Fatal("rejected grants stored")
			}
			// After a password reset the session is revoked.
			if err := f.s.ChangeUser(ctx, "alice@example.com", "new", false, oauthNow); err != nil {
				t.Fatal(err)
			}
			if err := f.s.CreateOAuthGrant(ctx, g, "alice-session", oauthNow); !errors.Is(err, store.ErrUnauthorized) {
				t.Fatalf("revoked session: %v", err)
			}
			if f.count("oauth_grants", "alice-id") != 0 {
				t.Fatal("revoked session stored a grant")
			}
		})
	}
}

func TestOAuthCodeExchange(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			ctx := context.Background()
			f := newOAuthFixture(t, driver)
			f.grant("alice", "g1", "mcp offline_access", oauthNow)

			// Unknown codes and a rejecting callback write nothing.
			if _, err := f.s.RedeemOAuthCode(ctx, "code-missing", oauthNow, tokens("x", oauthNow, true)); !errors.Is(err, store.ErrUnauthorized) {
				t.Fatalf("unknown code: %v", err)
			}
			var seen store.OAuthGrant
			_, err := f.s.RedeemOAuthCode(ctx, "code-g1", oauthNow+1, func(g store.OAuthGrant) ([]store.OAuthToken, error) {
				seen = g
				return nil, errRejected
			})
			if !errors.Is(err, errRejected) || seen.ID != "g1" || seen.RedirectURI != "https://claude.ai/cb" || seen.CodeChallenge != "challenge" {
				t.Fatalf("rejecting issue: %v %+v", err, seen)
			}
			if f.count("oauth_tokens", "alice-id") != 0 {
				t.Fatal("rejected exchange stored tokens")
			}

			// The exchange marks the code used, stores the tokens with the
			// grant's IDs and extends the grant to the latest token expiry.
			g := f.redeem("g1", "1", oauthNow+1)
			if !g.CodeUsedAt.Valid || g.CodeUsedAt.Int64 != oauthNow+1 || g.ExpiresAt != oauthNow+1+90*86400 {
				t.Fatalf("redeemed grant: %+v", g)
			}
			if err = f.authenticate("access-1", oauthNow+2); err != nil {
				t.Fatalf("access token: %v", err)
			}
			list, err := f.s.ListOAuthGrants(ctx, "alice-id", oauthNow+2)
			if err != nil || len(list) != 1 || list[0].ExpiresAt != g.ExpiresAt {
				t.Fatalf("list: %+v %v", list, err)
			}

			// A second exchange that the callback rejects (for example a
			// wrong verifier) changes nothing.
			if _, err = f.s.RedeemOAuthCode(ctx, "code-g1", oauthNow+3, reject); !errors.Is(err, errRejected) {
				t.Fatalf("reuse with rejecting issue: %v", err)
			}
			if err = f.authenticate("access-1", oauthNow+3); err != nil {
				t.Fatalf("token after rejected reuse: %v", err)
			}
			// A second exchange that passes the callback deletes the grant
			// and every token issued from it.
			if _, err = f.s.RedeemOAuthCode(ctx, "code-g1", oauthNow+3, tokens("2", oauthNow+3, true)); !errors.Is(err, store.ErrCodeReused) {
				t.Fatalf("reuse: %v", err)
			}
			if f.count("oauth_tokens", "alice-id") != 0 || f.count("oauth_grants", "alice-id") != 0 {
				t.Fatal("reuse kept the grant or tokens")
			}
			if err = f.authenticate("access-1", oauthNow+3); !errors.Is(err, store.ErrUnauthorized) {
				t.Fatalf("token after reuse: %v", err)
			}
			if _, err = f.s.RotateOAuthRefresh(ctx, "refresh-1", oauthNow+3, tokens("3", oauthNow+3, true)); !errors.Is(err, store.ErrUnauthorized) {
				t.Fatalf("refresh after reuse: %v", err)
			}

			// An expired code is rejected.
			f.grant("alice", "g2", "mcp", oauthNow)
			if _, err = f.s.RedeemOAuthCode(ctx, "code-g2", oauthNow+60, tokens("4", oauthNow+60, false)); !errors.Is(err, store.ErrUnauthorized) {
				t.Fatalf("expired code: %v", err)
			}
			// A code of a disabled user (not through ChangeUser) is rejected.
			f.grant("bob", "g3", "mcp", oauthNow)
			if _, err = f.raw.ExecContext(ctx, "UPDATE users SET disabled = 1 WHERE id = 'bob-id'"); err != nil {
				t.Fatal(err)
			}
			if _, err = f.s.RedeemOAuthCode(ctx, "code-g3", oauthNow+1, tokens("5", oauthNow+1, false)); !errors.Is(err, store.ErrUnauthorized) {
				t.Fatalf("disabled user: %v", err)
			}
		})
	}
}

func TestOAuthRefreshRotation(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			ctx := context.Background()
			f := newOAuthFixture(t, driver)
			f.grant("alice", "g1", "mcp offline_access", oauthNow)
			f.redeem("g1", "1", oauthNow)

			// Access tokens and unknown hashes are not refresh tokens.
			for _, hash := range []string{"access-1", "missing"} {
				if _, err := f.s.RotateOAuthRefresh(ctx, hash, oauthNow, tokens("x", oauthNow, true)); !errors.Is(err, store.ErrUnauthorized) {
					t.Fatalf("%s: %v", hash, err)
				}
			}
			// A rejecting callback keeps the refresh token.
			if _, err := f.s.RotateOAuthRefresh(ctx, "refresh-1", oauthNow+10, reject); !errors.Is(err, errRejected) {
				t.Fatalf("rejecting issue: %v", err)
			}
			g, err := f.s.RotateOAuthRefresh(ctx, "refresh-1", oauthNow+10, tokens("2", oauthNow+10, true))
			if err != nil || g.ID != "g1" || g.ExpiresAt != oauthNow+10+90*86400 {
				t.Fatalf("rotate: %+v %v", g, err)
			}
			// The old refresh token is gone; the new one and both access
			// tokens work.
			if _, err = f.s.RotateOAuthRefresh(ctx, "refresh-1", oauthNow+20, tokens("3", oauthNow+20, true)); !errors.Is(err, store.ErrUnauthorized) {
				t.Fatalf("old refresh token: %v", err)
			}
			for _, hash := range []string{"access-1", "access-2"} {
				if err = f.authenticate(hash, oauthNow+20); err != nil {
					t.Fatalf("%s: %v", hash, err)
				}
			}
			// Rotation removes the user's expired tokens: access-1 and
			// access-2 expire one hour after issue.
			if _, err = f.s.RotateOAuthRefresh(ctx, "refresh-2", oauthNow+7200, tokens("4", oauthNow+7200, true)); err != nil {
				t.Fatal(err)
			}
			if n := f.count("oauth_tokens", "alice-id"); n != 2 {
				t.Fatalf("tokens after cleanup: %d", n)
			}
			// An expired refresh token is rejected.
			if _, err = f.s.RotateOAuthRefresh(ctx, "refresh-4", oauthNow+7200+90*86400, tokens("5", oauthNow, true)); !errors.Is(err, store.ErrUnauthorized) {
				t.Fatalf("expired refresh token: %v", err)
			}
			// A disabled user cannot refresh.
			if _, err = f.raw.ExecContext(ctx, "UPDATE users SET disabled = 1 WHERE id = 'alice-id'"); err != nil {
				t.Fatal(err)
			}
			if _, err = f.s.RotateOAuthRefresh(ctx, "refresh-4", oauthNow+7300, tokens("6", oauthNow+7300, true)); !errors.Is(err, store.ErrUnauthorized) {
				t.Fatalf("disabled user: %v", err)
			}
			if err = f.authenticate("access-4", oauthNow+7300); !errors.Is(err, store.ErrUnauthorized) {
				t.Fatalf("disabled user's access token: %v", err)
			}
		})
	}
}

func TestOAuthAccessAndConnections(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			ctx := context.Background()
			f := newOAuthFixture(t, driver)
			empty, err := f.s.ListOAuthGrants(ctx, "alice-id", oauthNow)
			if err != nil || empty == nil || len(empty) != 0 {
				t.Fatalf("empty list: %#v %v", empty, err)
			}
			// Each exchange extends its grant, so a later consent does not
			// clean it up as expired.
			redeem := func(id string, now int64) {
				t.Helper()
				if _, err := f.s.RedeemOAuthCode(ctx, "code-"+id, now, tokens(id, now, id == "a2")); err != nil {
					t.Fatalf("redeem %s: %v", id, err)
				}
			}
			f.grant("alice", "a1", "mcp", oauthNow+100)
			redeem("a1", oauthNow+101)
			f.grant("alice", "a2", "mcp offline_access", oauthNow+200)
			f.grant("alice", "a3", "mcp", oauthNow+200)
			redeem("a2", oauthNow+201)
			redeem("a3", oauthNow+201)
			f.grant("alice", "pending", "mcp", oauthNow+300)
			f.grant("bob", "b1", "mcp", oauthNow+100)
			redeem("b1", oauthNow+101)
			// Exchanged grants only, newest first with ties by id descending.
			list, err := f.s.ListOAuthGrants(ctx, "alice-id", oauthNow+400)
			if err != nil || len(list) != 3 || list[0].ID != "a3" || list[1].ID != "a2" || list[2].ID != "a1" {
				t.Fatalf("list: %+v %v", list, err)
			}
			if list[1].Scope != "mcp offline_access" || list[1].ClientName != "Claude" || list[0].LastUsedAt.Valid {
				t.Fatalf("listed grant: %+v", list[1])
			}

			// Authentication: access tokens only, unexpired, and last_used_at
			// is recorded at most once a minute.
			u, g, touchErr, err := f.s.AuthenticateOAuthAccess(ctx, "access-a3", oauthNow+401)
			if err != nil || touchErr != nil || u.ID != "alice-id" || g.ID != "a3" || g.Resource != "https://astovia.test/mcp" {
				t.Fatalf("authenticate: %+v %+v %v %v", u, g, touchErr, err)
			}
			for _, hash := range []string{"refresh-a2", "missing"} {
				if err = f.authenticate(hash, oauthNow+401); !errors.Is(err, store.ErrUnauthorized) {
					t.Fatalf("%s: %v", hash, err)
				}
			}
			if err = f.authenticate("access-a3", oauthNow+201+3600); !errors.Is(err, store.ErrUnauthorized) {
				t.Fatalf("expired access token: %v", err)
			}
			lastUsed := func() sql.NullInt64 {
				t.Helper()
				list, err := f.s.ListOAuthGrants(ctx, "alice-id", oauthNow+500)
				if err != nil {
					t.Fatal(err)
				}
				return list[0].LastUsedAt
			}
			for _, step := range []struct{ now, want int64 }{{oauthNow + 430, oauthNow + 401}, {oauthNow + 461, oauthNow + 401}, {oauthNow + 462, oauthNow + 462}} {
				if err = f.authenticate("access-a3", step.now); err != nil {
					t.Fatal(err)
				}
				if got := lastUsed(); got != (sql.NullInt64{Int64: step.want, Valid: true}) {
					t.Fatalf("last_used_at at %d: %+v", step.now, got)
				}
			}

			// Deleting another user's grant fails and keeps it.
			if err = f.s.DeleteOAuthGrant(ctx, "alice-id", "b1"); !errors.Is(err, store.ErrGrantNotFound) {
				t.Fatalf("delete other's grant: %v", err)
			}
			if err = f.s.DeleteOAuthGrant(ctx, "alice-id", "missing"); !errors.Is(err, store.ErrGrantNotFound) {
				t.Fatalf("delete missing grant: %v", err)
			}
			if err = f.authenticate("access-b1", oauthNow+500); err != nil {
				t.Fatalf("bob's token after alice's delete: %v", err)
			}
			if err = f.s.DeleteOAuthGrant(ctx, "alice-id", "a2"); err != nil {
				t.Fatal(err)
			}
			if err = f.authenticate("access-a2", oauthNow+500); !errors.Is(err, store.ErrUnauthorized) {
				t.Fatalf("deleted grant's token: %v", err)
			}
			if _, err = f.s.RotateOAuthRefresh(ctx, "refresh-a2", oauthNow+500, tokens("x", oauthNow+500, true)); !errors.Is(err, store.ErrUnauthorized) {
				t.Fatalf("deleted grant's refresh token: %v", err)
			}

			// A new consent removes the user's expired tokens and grants
			// (including the unexchanged one), but not another user's.
			later := int64(oauthNow + 7*86400)
			if err = f.s.Login(ctx, f.users["alice"], store.Session{ID: "alice-later", UserID: "alice-id", ExpiresAt: later + 60, CreatedAt: later}, store.Refresh{Hash: "refresh-later", SessionID: "alice-later", CreatedAt: later}); err != nil {
				t.Fatal(err)
			}
			if err = f.s.CreateOAuthGrant(ctx, store.OAuthGrant{ID: "new", UserID: "alice-id", ClientID: "dcr_client", ClientName: "Tool", Scope: "mcp", Resource: "r", RedirectURI: "http://localhost/cb", CodeHash: "code-new", CodeChallenge: "c", CodeExpiresAt: later + 60, CreatedAt: later, ExpiresAt: later + 60}, "alice-later", later); err != nil {
				t.Fatal(err)
			}
			if f.count("oauth_tokens", "alice-id") != 0 || f.count("oauth_grants", "alice-id") != 1 {
				t.Fatalf("cleanup: tokens %d grants %d", f.count("oauth_tokens", "alice-id"), f.count("oauth_grants", "alice-id"))
			}
			if f.count("oauth_tokens", "bob-id") != 1 || f.count("oauth_grants", "bob-id") != 1 {
				t.Fatal("cleanup removed another user's rows")
			}

			// Password reset and disable delete every grant and token.
			if err = f.s.ChangeUser(ctx, "bob@example.com", "", true, oauthNow); err != nil {
				t.Fatal(err)
			}
			if f.count("oauth_tokens", "bob-id") != 0 || f.count("oauth_grants", "bob-id") != 0 {
				t.Fatal("disable kept OAuth rows")
			}
			if err = f.s.ChangeUser(ctx, "alice@example.com", "new", false, later); err != nil {
				t.Fatal(err)
			}
			if f.count("oauth_grants", "alice-id") != 0 {
				t.Fatal("password reset kept OAuth rows")
			}
			// Client rows are kept.
			if _, err = f.s.OAuthClient(ctx, "dcr_client"); err != nil {
				t.Fatal(err)
			}
		})
	}
}
