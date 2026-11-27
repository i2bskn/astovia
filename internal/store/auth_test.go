package store_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/i2bskn/authlume/mcpoauth"
	"github.com/i2bskn/authlume/mcpoauth/storetest"
	"github.com/i2bskn/authlume/oidc"
	"github.com/i2bskn/authlume/session"
	"github.com/i2bskn/authlume/webauthn"

	"github.com/i2bskn/astovia/internal/store"
	"github.com/i2bskn/astovia/internal/testutil"
)

// digestOf returns a 32-byte key derived from v.
func digestOf(v string) [32]byte { return sha256.Sum256([]byte(v)) }

// access counts the ways into an account: browser sessions, API tokens, MCP
// codes and connections, and pending WebAuthn ceremonies.
type access struct {
	Sessions, Tokens, Codes, Connections, Ceremonies int64
}

// granted is what grantAccess adds to a user that has its fixture session.
var granted = access{Sessions: 2, Tokens: 1, Codes: 1, Connections: 1, Ceremonies: 1}

// grantAccess gives name a second session, an API token, an MCP code and
// connection, and a pending WebAuthn ceremony.
func grantAccess(t *testing.T, s *store.Store, name string) {
	t.Helper()
	ctx := context.Background()
	uid := name + "-id"
	sid := name + "-grant"
	if err := s.InsertWebSession(ctx, store.WebSession{ID: sid, UserID: uid, AuthenticatedAt: tokenTestNow, LastActivity: tokenTestNow, AbsoluteExpiry: tokenTestNow + 3600, IdleSeconds: 3600}); err != nil {
		t.Fatal(err)
	}
	createToken(t, s, uid, sid, name+"-token", tokenTestNow)
	now := time.Now().Truncate(time.Second)
	m := s.MCPOAuth(time.Now)
	code := mcpoauth.Code{ClientID: "client-" + name, RedirectURI: "https://client.example/callback", Subject: uid, Scopes: []string{"mcp"}, Challenge: "challenge", ExpiresAt: now.Add(time.Minute)}
	if err := m.CreateCode(store.WithApprovalSession(ctx, sid, time.Unix(tokenTestNow, 0)), digestOf("code-"+name), code); err != nil {
		t.Fatal(err)
	}
	conn := mcpoauth.Connection{
		ID: "connection-" + name, Selector: digestOf("selector-" + name), Subject: uid, ClientID: "client-" + name,
		Scopes: []string{"mcp"}, CreatedAt: now, RefreshedAt: now,
		Access: digestOf("access-" + name), AccessExpiry: now.Add(time.Hour), Refresh: digestOf("refresh-" + name), RefreshExpiry: now.Add(24 * time.Hour),
	}
	if err := m.Create(ctx, conn); err != nil {
		t.Fatal(err)
	}
	if err := s.WebAuthnCeremonies(time.Now).Create(ctx, digestOf("ceremony-"+name), uid, []byte("record"), now.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
}

func accessOf(t *testing.T, s *store.Store, userID string) access {
	t.Helper()
	count := func(table, column string) int64 {
		t.Helper()
		n, err := s.CountForTest(context.Background(), fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE %s = '%s'", table, column, userID))
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	return access{
		Sessions:    count("web_sessions", "user_id"),
		Tokens:      count("api_tokens", "user_id"),
		Codes:       count("mcp_oauth_codes", "subject"),
		Connections: count("mcp_oauth_connections", "subject"),
		Ceremonies:  count("webauthn_ceremonies", "subject"),
	}
}

// storetestNow is the start of the clock of storetest.
var storetestNow = time.Unix(1_800_000_000, 0).UTC()

func TestMCPStoreContract(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		t.Run("store", func(t *testing.T) { runMCPStore(t, driver) })
		t.Run("clients", func(t *testing.T) {
			storetest.RunClientStore(t, func(t *testing.T) mcpoauth.ClientStore {
				s, _ := testutil.Database(t, driver)
				return s.MCPClients()
			})
		})
	})
}

// runMCPStore runs storetest.Run with the users "user" and "other" that its
// codes and connections name.
func runMCPStore(t *testing.T, driver string) {
	storetest.Run(t, func(t *testing.T) mcpoauth.Store {
		s, _ := testutil.Database(t, driver)
		for _, id := range []string{"user", "other"} {
			if err := s.CreateUser(context.Background(), store.User{ID: id, Email: id + "@example.com", PasswordHash: "x", Role: store.RoleMember}); err != nil {
				t.Fatal(err)
			}
			at := storetestNow.Add(-time.Hour).Unix()
			if err := s.InsertWebSession(context.Background(), store.WebSession{ID: id + "-session", UserID: id, AuthenticatedAt: at, LastActivity: at, AbsoluteExpiry: at + 1<<30, IdleSeconds: 1 << 30}); err != nil {
				t.Fatal(err)
			}
		}
		return approvingStore{s.MCPOAuth(func() time.Time { return storetestNow })}
	})
}

// approvingStore issues each code as an approval by a browser session of
// its subject, as the consent screen does.
type approvingStore struct{ mcpoauth.Store }

func (a approvingStore) CreateCode(ctx context.Context, key mcpoauth.Key, code mcpoauth.Code) error {
	return a.Store.CreateCode(store.WithApprovalSession(ctx, code.Subject+"-session", storetestNow), key, code)
}

func TestSessionStore(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		ctx := context.Background()
		s := userFixture(t, driver)
		ws := s.Sessions()
		now := time.Unix(tokenTestNow, 0)
		state := func(key string, subject string) session.State {
			return session.State{Key: digestOf(key), Subject: subject, AuthenticatedAt: now, LastActivity: now, AbsoluteExpiry: now.Add(time.Hour), Idle: 10 * time.Minute}
		}
		idOf := func(key string) string {
			k := digestOf(key)
			return hex.EncodeToString(k[:])
		}

		guarded := store.WithIssueGuard(ctx, store.IssueGuard{PasswordDigest: passwordDigest("x")})
		first := state("one", "alice-id")
		// A write without its guard, or with a guard that checks nothing,
		// issues nothing.
		for _, c := range []context.Context{ctx, store.WithIssueGuard(ctx, store.IssueGuard{})} {
			if err := ws.Replace(c, nil, first); err == nil {
				t.Fatal("a session was issued without a guard")
			}
			if _, err := ws.Use(ctx, first.Key, now, false); !errors.Is(err, session.ErrInvalid) {
				t.Fatalf("issued without a guard: %v", err)
			}
		}
		if err := ws.Replace(guarded, nil, first); err != nil {
			t.Fatal(err)
		}
		got, err := ws.Use(ctx, first.Key, now.Add(time.Minute), false)
		if err != nil || got.Subject != "alice-id" || !got.LastActivity.Equal(now) || !got.AbsoluteExpiry.Equal(first.AbsoluteExpiry) || got.Idle != first.Idle || got.Key != first.Key {
			t.Fatalf("read: %+v %v", got, err)
		}
		if live, _ := s.SessionLive(ctx, idOf("one"), tokenTestNow); !live {
			t.Fatal("the session is not stored under the hex of its key")
		}
		// A touch moves the last activity, never back.
		if got, err = ws.Use(ctx, first.Key, now.Add(5*time.Minute), true); err != nil || !got.LastActivity.Equal(now.Add(5*time.Minute)) {
			t.Fatalf("touch: %+v %v", got, err)
		}
		if got, err = ws.Use(ctx, first.Key, now.Add(2*time.Minute), true); err != nil || !got.LastActivity.Equal(now.Add(5*time.Minute)) {
			t.Fatalf("older touch: %+v %v", got, err)
		}
		// The idle deadline is from the last activity; the absolute one
		// stays.
		if _, err = ws.Use(ctx, first.Key, now.Add(15*time.Minute), false); !errors.Is(err, session.ErrInvalid) {
			t.Fatalf("idle: %v", err)
		}
		for i := 1; i <= 6; i++ {
			if _, err = ws.Use(ctx, first.Key, now.Add(time.Duration(5+9*i)*time.Minute), true); err != nil {
				t.Fatalf("touch %d: %v", i, err)
			}
		}
		if _, err = ws.Use(ctx, first.Key, now.Add(time.Hour), true); !errors.Is(err, session.ErrInvalid) {
			t.Fatalf("absolute: %v", err)
		}
		if _, err = ws.Use(ctx, digestOf("missing"), now, false); !errors.Is(err, session.ErrInvalid) {
			t.Fatalf("missing: %v", err)
		}

		// A replacement deletes the previous session; a collision is
		// reported and keeps both.
		second := state("two", "alice-id")
		if err = ws.Replace(guarded, &first.Key, second); err != nil {
			t.Fatal(err)
		}
		if live, _ := s.SessionLive(ctx, idOf("one"), tokenTestNow); live {
			t.Fatal("the previous session was kept")
		}
		third := state("three", "bob-id")
		if err = ws.Replace(guarded, nil, third); err != nil {
			t.Fatal(err)
		}
		if err = ws.Replace(guarded, &third.Key, state("two", "bob-id")); !errors.Is(err, session.ErrCollision) {
			t.Fatalf("collision: %v", err)
		}
		if got, err = ws.Use(ctx, second.Key, now, false); err != nil || got.Subject != "alice-id" {
			t.Fatalf("collided session: %+v %v", got, err)
		}
		if _, err = ws.Use(ctx, third.Key, now, false); err != nil {
			t.Fatalf("session of the collided replacement: %v", err)
		}
		if err = ws.Delete(ctx, second.Key); err != nil {
			t.Fatal(err)
		}
		if _, err = ws.Use(ctx, second.Key, now, false); !errors.Is(err, session.ErrInvalid) {
			t.Fatalf("deleted: %v", err)
		}

		// The guard: a changed password, another or no OIDC identity, and a
		// disabled or missing user issue nothing.
		if err = ws.Replace(store.WithIssueGuard(ctx, store.IssueGuard{PasswordDigest: passwordDigest("old")}), nil, state("g1", "alice-id")); !errors.Is(err, store.ErrUnauthorized) {
			t.Fatalf("changed password: %v", err)
		}
		if err = ws.Replace(store.WithIssueGuard(ctx, store.IssueGuard{OIDCIssuer: "https://idp.example", OIDCSubject: "sub"}), nil, state("g2", "alice-id")); !errors.Is(err, store.ErrUnauthorized) {
			t.Fatalf("unlinked user: %v", err)
		}
		if err = s.ChangeUser(ctx, "carol@example.com", "", true, tokenTestNow); err != nil {
			t.Fatal(err)
		}
		if err = ws.Replace(guarded, nil, state("g3", "carol-id")); !errors.Is(err, store.ErrUnauthorized) {
			t.Fatalf("disabled user: %v", err)
		}
		if err = ws.Replace(guarded, nil, state("g4", "missing-id")); !errors.Is(err, store.ErrUnauthorized) {
			t.Fatalf("missing user: %v", err)
		}
		for _, key := range []string{"g1", "g2", "g3", "g4"} {
			if _, err = ws.Use(ctx, digestOf(key), now, false); !errors.Is(err, session.ErrInvalid) {
				t.Fatalf("%s was issued: %v", key, err)
			}
		}
		if err = s.ApproveOIDCLink(ctx, "bob@example.com", tokenTestNow+60); err != nil {
			t.Fatal(err)
		}
		user, _, err := s.LinkOIDCUser(ctx, "https://idp.example", "sub-dave", "bob@example.com", store.User{ID: "unused-id"}, tokenTestNow)
		if err != nil {
			t.Fatal(err)
		}
		if err = ws.Replace(store.WithIssueGuard(ctx, store.IssueGuard{OIDCIssuer: "https://idp.example", OIDCSubject: "sub-other"}), nil, state("g5", user.ID)); !errors.Is(err, store.ErrUnauthorized) {
			t.Fatalf("another identity: %v", err)
		}
		if err = ws.Replace(store.WithIssueGuard(ctx, store.IssueGuard{OIDCIssuer: "https://idp.example", OIDCSubject: "sub-dave"}), nil, state("g6", user.ID)); err != nil {
			t.Fatalf("linked identity: %v", err)
		}
	})
}

func TestOIDCLoginStore(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		ctx := context.Background()
		s, _ := testutil.Database(t, driver)
		o := s.OIDCLogins(time.Now)
		now := time.Now().Truncate(time.Second)
		zero := int64(0)
		withData := oidc.Transaction{Nonce: "n1", Verifier: "v1", StartedAt: now, ExpiresAt: now.Add(10 * time.Minute), MaxAgeSeconds: &zero, AppData: []byte("/oauth/authorize?\x00x")}
		plain := oidc.Transaction{Nonce: "n2", Verifier: "v2", StartedAt: now, ExpiresAt: now.Add(10 * time.Minute)}
		if err := o.Create(ctx, digestOf("one"), withData); err != nil {
			t.Fatal(err)
		}
		if err := o.Create(ctx, digestOf("two"), plain); err != nil {
			t.Fatal(err)
		}
		if err := o.Create(ctx, digestOf("one"), plain); err == nil {
			t.Fatal("collision accepted")
		}
		got, err := o.Take(ctx, digestOf("one"), now)
		if err != nil || got.Nonce != "n1" || got.Verifier != "v1" || !got.StartedAt.Equal(now) || !got.ExpiresAt.Equal(withData.ExpiresAt) || got.MaxAgeSeconds == nil || *got.MaxAgeSeconds != 0 || string(got.AppData) != string(withData.AppData) {
			t.Fatalf("take: %+v %v", got, err)
		}
		if _, err = o.Take(ctx, digestOf("one"), now); !errors.Is(err, oidc.ErrInvalid) {
			t.Fatalf("second take: %v", err)
		}
		// An expired login is consumed as well.
		if _, err = o.Take(ctx, digestOf("two"), now.Add(10*time.Minute)); !errors.Is(err, oidc.ErrInvalid) {
			t.Fatalf("expired: %v", err)
		}
		if _, err = o.Take(ctx, digestOf("two"), now); !errors.Is(err, oidc.ErrInvalid) {
			t.Fatalf("after the expired take: %v", err)
		}
		if err = o.Create(ctx, digestOf("three"), plain); err != nil {
			t.Fatal(err)
		}
		if got, err = o.Take(ctx, digestOf("three"), now); err != nil || got.MaxAgeSeconds != nil || len(got.AppData) != 0 {
			t.Fatalf("plain: %+v %v", got, err)
		}
		// An expired row of the same key is replaced.
		old := plain
		old.ExpiresAt = now.Add(-time.Second)
		if err = o.Create(ctx, digestOf("four"), old); err != nil {
			t.Fatal(err)
		}
		if err = o.Create(ctx, digestOf("four"), plain); err != nil {
			t.Fatalf("over an expired row: %v", err)
		}
	})
}

func TestWebAuthnCeremonyStore(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		ctx := context.Background()
		s, _ := testutil.Database(t, driver)
		c := s.WebAuthnCeremonies(time.Now)
		now := time.Now().Truncate(time.Second)
		if err := c.Create(ctx, digestOf("one"), "alice-id", []byte{0, 1, 2, 255}, now.Add(5*time.Minute)); err != nil {
			t.Fatal(err)
		}
		if err := c.Create(ctx, digestOf("one"), "bob-id", []byte("other"), now.Add(5*time.Minute)); err == nil {
			t.Fatal("collision accepted")
		}
		got, err := c.Take(ctx, digestOf("one"), now)
		if err != nil || !reflect.DeepEqual(got, []byte{0, 1, 2, 255}) {
			t.Fatalf("take: %v %v", got, err)
		}
		if _, err = c.Take(ctx, digestOf("one"), now); !errors.Is(err, webauthn.ErrInvalid) {
			t.Fatalf("second take: %v", err)
		}
		if err = c.Create(ctx, digestOf("two"), "alice-id", []byte("r"), now.Add(5*time.Minute)); err != nil {
			t.Fatal(err)
		}
		if _, err = c.Take(ctx, digestOf("two"), now.Add(5*time.Minute)); !errors.Is(err, webauthn.ErrInvalid) {
			t.Fatalf("expired: %v", err)
		}
		// The ceremonies of a subject are deleted together; a passkey login
		// has no subject.
		for _, k := range []string{"a1", "a2"} {
			if err = c.Create(ctx, digestOf(k), "alice-id", []byte("r"), now.Add(5*time.Minute)); err != nil {
				t.Fatal(err)
			}
		}
		if err = c.Create(ctx, digestOf("b1"), "bob-id", []byte("r"), now.Add(5*time.Minute)); err != nil {
			t.Fatal(err)
		}
		if err = c.Create(ctx, digestOf("p1"), "", []byte("r"), now.Add(5*time.Minute)); err != nil {
			t.Fatal(err)
		}
		if err = s.DeleteWebAuthnCeremonies(ctx, "alice-id"); err != nil {
			t.Fatal(err)
		}
		for k, want := range map[string]bool{"a1": false, "a2": false, "b1": true, "p1": true} {
			if _, err = c.Take(ctx, digestOf(k), now); (err == nil) != want {
				t.Fatalf("%s: %v", k, err)
			}
		}
	})
}

func TestDeleteExpiredAuth(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		ctx := context.Background()
		s := userFixture(t, driver)
		grantAccess(t, s, "alice")
		now := time.Now().Truncate(time.Second)
		// Expired rows of each kind.
		if err := s.InsertWebSession(ctx, store.WebSession{ID: "idle", UserID: "alice-id", AuthenticatedAt: now.Unix() - 100, LastActivity: now.Unix() - 100, AbsoluteExpiry: now.Unix() + 3600, IdleSeconds: 50}); err != nil {
			t.Fatal(err)
		}
		if err := s.InsertWebSession(ctx, store.WebSession{ID: "old", UserID: "alice-id", AuthenticatedAt: now.Unix() - 100, LastActivity: now.Unix(), AbsoluteExpiry: now.Unix(), IdleSeconds: 3600}); err != nil {
			t.Fatal(err)
		}
		if err := s.InsertWebSession(ctx, store.WebSession{ID: "live", UserID: "alice-id", AuthenticatedAt: now.Unix(), LastActivity: now.Unix(), AbsoluteExpiry: now.Unix() + 3600, IdleSeconds: 3600}); err != nil {
			t.Fatal(err)
		}
		if err := s.OIDCLogins(time.Now).Create(ctx, digestOf("login-old"), oidc.Transaction{Nonce: "n", Verifier: "v", StartedAt: now.Add(-time.Hour), ExpiresAt: now}); err != nil {
			t.Fatal(err)
		}
		if err := s.OIDCLogins(time.Now).Create(ctx, digestOf("login-live"), oidc.Transaction{Nonce: "n", Verifier: "v", StartedAt: now, ExpiresAt: now.Add(time.Minute)}); err != nil {
			t.Fatal(err)
		}
		if err := s.WebAuthnCeremonies(time.Now).Create(ctx, digestOf("ceremony-old"), "alice-id", []byte("r"), now); err != nil {
			t.Fatal(err)
		}
		m := s.MCPOAuth(func() time.Time { return now.Add(-time.Hour) })
		if err := m.CreateCode(store.WithApprovalSession(ctx, "alice-grant", time.Unix(tokenTestNow, 0)), digestOf("code-old"), mcpoauth.Code{ClientID: "c", RedirectURI: "https://c.example/cb", Subject: "alice-id", Scopes: []string{"mcp"}, Challenge: "x", ExpiresAt: now}); err != nil {
			t.Fatal(err)
		}
		old := now.Add(-48 * time.Hour)
		if err := m.Create(ctx, mcpoauth.Connection{ID: "conn-old", Selector: digestOf("sel-old"), Subject: "alice-id", ClientID: "c", Scopes: []string{"mcp"}, CreatedAt: old, RefreshedAt: old, Access: digestOf("acc-old"), AccessExpiry: old.Add(time.Hour), Refresh: digestOf("ref-old"), RefreshExpiry: now}); err != nil {
			t.Fatal(err)
		}
		before := accessOf(t, s, "alice-id")
		if before.Sessions != 5 || before.Codes != 2 || before.Connections != 2 || before.Ceremonies != 2 {
			t.Fatalf("before: %+v", before)
		}
		// The fixture sessions end at tokenTestNow+3600, which may be before
		// now; count them out.
		fixture := int64(0)
		for _, id := range []string{"alice-session", "alice-grant"} {
			if live, _ := s.SessionLive(ctx, id, now.Unix()); live {
				fixture++
			}
		}
		if err := s.DeleteExpiredAuth(ctx, now); err != nil {
			t.Fatal(err)
		}
		after := accessOf(t, s, "alice-id")
		if want := (access{Sessions: 1 + fixture, Tokens: 1, Codes: 1, Connections: 1, Ceremonies: 1}); after != want {
			t.Fatalf("after: %+v, want %+v", after, want)
		}
		if n, err := s.CountForTest(ctx, "SELECT COUNT(*) FROM oidc_logins"); err != nil || n != 1 {
			t.Fatalf("OIDC logins: %d %v", n, err)
		}
	})
}

func TestMCPConnections(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		ctx := context.Background()
		s := userFixture(t, driver)
		grantAccess(t, s, "alice")
		grantAccess(t, s, "bob")
		now := time.Now().Truncate(time.Second)

		list := func(userID string) []store.MCPConnection {
			t.Helper()
			out, err := s.ListMCPConnections(ctx, userID, now)
			if err != nil {
				t.Fatal(err)
			}
			return out
		}
		got := list("alice-id")
		if len(got) != 1 || got[0].ID != "connection-alice" || got[0].ClientID != "client-alice" || got[0].ClientName != "" || got[0].ClientHost != "" || !reflect.DeepEqual(got[0].Scopes, []string{"mcp"}) || got[0].LastUsedAt.Valid || got[0].ExpiresAt != now.Add(24*time.Hour).Unix() {
			t.Fatalf("alice: %+v", got)
		}
		if got := list("carol-id"); got == nil || len(got) != 0 {
			t.Fatalf("carol: %#v", got)
		}

		// The label of the latest consent screen is shown.
		if err := s.SetMCPClientLabel(ctx, "client-alice", "Client", "client.example", now); err != nil {
			t.Fatal(err)
		}
		if err := s.SetMCPClientLabel(ctx, "client-alice", "Renamed", "", now); err != nil {
			t.Fatal(err)
		}
		if err := s.SetMCPClientLabel(ctx, "client-alice", "bad\x00", "", now); err == nil {
			t.Fatal("label with NUL accepted")
		}
		if got = list("alice-id"); got[0].ClientName != "Renamed" || got[0].ClientHost != "" {
			t.Fatalf("label: %+v", got)
		}

		// The use is written at most once a minute.
		if err := s.TouchMCPConnection(ctx, "connection-alice", now); err != nil {
			t.Fatal(err)
		}
		if err := s.TouchMCPConnection(ctx, "connection-alice", now.Add(30*time.Second)); err != nil {
			t.Fatal(err)
		}
		if got = list("alice-id"); got[0].LastUsedAt.Int64 != now.Unix() {
			t.Fatalf("touch: %+v", got)
		}
		if err := s.TouchMCPConnection(ctx, "connection-alice", now.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		if got = list("alice-id"); got[0].LastUsedAt.Int64 != now.Add(time.Minute).Unix() {
			t.Fatalf("touch after a minute: %+v", got)
		}

		for _, id := range []string{"client-alice", "client-bob"} {
			if used, err := s.MCPClientInUse(ctx, id); err != nil || !used {
				t.Fatalf("%s in use: %v %v", id, used, err)
			}
		}
		if used, err := s.MCPClientInUse(ctx, "client-unknown"); err != nil || used {
			t.Fatalf("unknown client: %v %v", used, err)
		}

		// Another user's or a missing connection is not found.
		if err := s.DeleteMCPConnection(ctx, "alice-id", "connection-bob"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("bob's connection as alice: %v", err)
		}
		if err := s.DeleteMCPConnection(ctx, "alice-id", "missing\x00"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("missing: %v", err)
		}
		if err := s.DeleteMCPConnection(ctx, "alice-id", "connection-alice"); err != nil {
			t.Fatal(err)
		}
		if got = list("alice-id"); len(got) != 0 {
			t.Fatalf("after the removal: %+v", got)
		}
		if _, err := s.MCPOAuth(time.Now).Use(ctx, digestOf("access-alice"), now); !errors.Is(err, mcpoauth.ErrInvalid) {
			t.Fatalf("access token of the removed connection: %v", err)
		}
		// Alice's code still names her client.
		if used, _ := s.MCPClientInUse(ctx, "client-alice"); !used {
			t.Fatal("client-alice with its code")
		}

		// A client whose registration changed loses its codes and
		// connections.
		if n, err := s.RevokeMCPClient(ctx, "client-bob"); err != nil || n != 1 {
			t.Fatalf("revoke client: %d %v", n, err)
		}
		if used, _ := s.MCPClientInUse(ctx, "client-bob"); used {
			t.Fatal("client-bob after its revocation")
		}
		if got := accessOf(t, s, "bob-id"); got.Codes != 0 || got.Connections != 0 || got.Sessions != 2 {
			t.Fatalf("bob: %+v", got)
		}

		// The approval session must be live, and the user enabled.
		m := s.MCPOAuth(time.Now)
		code := mcpoauth.Code{ClientID: "c", RedirectURI: "https://c.example/cb", Subject: "alice-id", Scopes: []string{"mcp"}, Challenge: "x", ExpiresAt: now.Add(time.Minute)}
		if err := m.CreateCode(ctx, digestOf("c1"), code); err == nil {
			t.Fatal("a code was issued without an approval session")
		}
		if err := m.CreateCode(store.WithApprovalSession(ctx, "bob-session", time.Unix(tokenTestNow, 0)), digestOf("c1"), code); !errors.Is(err, store.ErrUnauthorized) {
			t.Fatalf("another user's session: %v", err)
		}
		if err := m.CreateCode(store.WithApprovalSession(ctx, "alice-session", time.Unix(tokenTestNow+3600, 0)), digestOf("c1"), code); !errors.Is(err, store.ErrUnauthorized) {
			t.Fatalf("expired session: %v", err)
		}
		if err := m.CreateCode(store.WithApprovalSession(ctx, "alice-session", time.Unix(tokenTestNow, 0)), digestOf("c1"), code); err != nil {
			t.Fatalf("live session: %v", err)
		}
		if err := s.ChangeUser(ctx, "alice@example.com", "", true, tokenTestNow); err != nil {
			t.Fatal(err)
		}
		if err := m.CreateCode(store.WithApprovalSession(ctx, "alice-session", time.Unix(tokenTestNow, 0)), digestOf("c2"), code); !errors.Is(err, store.ErrUnauthorized) {
			t.Fatalf("disabled user: %v", err)
		}
		conn := mcpoauth.Connection{ID: "x", Selector: digestOf("sx"), Subject: "alice-id", ClientID: "c", Scopes: []string{"mcp"}, CreatedAt: now, RefreshedAt: now, Access: digestOf("ax"), AccessExpiry: now.Add(time.Hour), Refresh: digestOf("rx"), RefreshExpiry: now.Add(time.Hour)}
		if err := m.Create(ctx, conn); !errors.Is(err, mcpoauth.ErrInvalid) {
			t.Fatalf("connection of a disabled user: %v", err)
		}
	})
}

// TestOIDCLoginLimit checks that the sign-ins in progress are bounded by
// evicting the oldest ones, and that expired ones do not count.
func TestOIDCLoginLimit(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		ctx := context.Background()
		s, _ := testutil.Database(t, driver)
		now := time.Unix(tokenTestNow, 0)
		o := s.OIDCLogins(func() time.Time { return now })
		fill := func(prefix string, expiresAt int64, n int) {
			t.Helper()
			err := s.ExecForTest(ctx, fmt.Sprintf(`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < %d)
INSERT INTO oidc_logins (key, nonce, verifier, started_at, expires_at) SELECT '%s' || i, 'n', 'v', %d, %d FROM n`, n, prefix, tokenTestNow, expiresAt))
			if err != nil {
				t.Fatal(err)
			}
		}
		start := func(key string, life time.Duration) error {
			return o.Create(ctx, digestOf(key), oidc.Transaction{Nonce: "n", Verifier: "v", StartedAt: now, ExpiresAt: now.Add(life)})
		}
		count := func(query string) int64 {
			t.Helper()
			n, err := s.CountForTest(ctx, query)
			if err != nil {
				t.Fatal(err)
			}
			return n
		}
		live := func() int64 {
			t.Helper()
			return count(fmt.Sprintf("SELECT COUNT(*) FROM oidc_logins WHERE expires_at > %d", now.Unix()))
		}
		taken := func(key string) bool {
			t.Helper()
			_, err := o.Take(ctx, digestOf(key), now)
			if err != nil && !errors.Is(err, oidc.ErrInvalid) {
				t.Fatal(err)
			}
			return err == nil
		}
		// The oldest sign-in expires first.
		if err := start("oldest", time.Minute); err != nil {
			t.Fatal(err)
		}
		fill("live-", tokenTestNow+600, store.MaxLiveOIDCLogins-2)
		fill("expired-", tokenTestNow, 10)
		if err := start("last", 10*time.Minute); err != nil {
			t.Fatalf("the last start under the limit: %v", err)
		}
		if n := live(); n != store.MaxLiveOIDCLogins {
			t.Fatalf("live: %d", n)
		}
		// Over the limit, the oldest one is evicted and the new one starts.
		if err := start("over", 10*time.Minute); err != nil {
			t.Fatalf("over the limit: %v", err)
		}
		if n := live(); n != store.MaxLiveOIDCLogins {
			t.Fatalf("live after the eviction: %d", n)
		}
		if taken("oldest") {
			t.Fatal("the oldest sign-in was not evicted")
		}
		if !taken("over") || !taken("last") {
			t.Fatal("a new sign-in was evicted")
		}
		// Expired ones are neither counted nor evicted.
		if n := count("SELECT COUNT(*) FROM oidc_logins WHERE key LIKE 'expired-%'"); n != 10 {
			t.Fatalf("expired: %d", n)
		}
	})
}
