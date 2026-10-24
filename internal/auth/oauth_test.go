package auth

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"github.com/i2bskn/astovia/internal/store"
	"github.com/i2bskn/astovia/internal/testutil"
	"strings"
	"testing"
	"time"
)

func TestOAuthCredentialFormats(t *testing.T) {
	for prefix, gen := range map[string]func() (string, string, error){"aso_": NewOAuthAccessToken, "asr_": NewOAuthRefreshToken, "asc_": NewOAuthCode, "dcs_": NewDCRClientSecret} {
		raw, hash, err := gen()
		if err != nil || len(raw) != 47 || !strings.HasPrefix(raw, prefix) || hash != Hash(raw) {
			t.Fatalf("%s: %q %q %v", prefix, raw, hash, err)
		}
	}
	id, err := NewDCRClientID()
	if err != nil || !IsDCRClientID(id) {
		t.Fatalf("client_id %q %v", id, err)
	}
	body := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	checks := map[string]func(string) bool{"aso_": IsOAuthAccessToken, "asr_": IsOAuthRefreshToken, "asc_": IsOAuthCode, "dcr_": IsDCRClientID}
	for prefix, check := range checks {
		if !check(prefix + body) {
			t.Fatalf("%s rejected", prefix)
		}
		for _, v := range []string{"", prefix, prefix + body[:42], prefix + body + "A", prefix + body[:42] + "*", strings.ToUpper(prefix) + body, "ast_" + body} {
			if check(v) {
				t.Fatalf("%s accepted %q", prefix, v)
			}
		}
		// Each kind rejects the others.
		for other := range checks {
			if other != prefix && check(other+body) {
				t.Fatalf("%s accepted %s", prefix, other)
			}
		}
	}
	if MCPResource("https://astovia.test") != "https://astovia.test/mcp" {
		t.Fatal("MCPResource")
	}
}

func TestAuthenticateOAuthAccess(t *testing.T) {
	ctx := context.Background()
	s, _ := testutil.Database(t, "sqlite")
	now := time.Unix(1_800_000_000, 0)
	service := func(issuer string) *Service {
		t.Helper()
		a, err := New(s, contractSecret, issuer)
		if err != nil {
			t.Fatal(err)
		}
		a.Now = func() time.Time { return now }
		return a
	}
	a := service("https://astovia.test")
	u := store.User{ID: "alice-id", Email: "alice@example.com", PasswordHash: "x"}
	if err := s.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	if err := s.Login(ctx, u, store.Session{ID: "sid", UserID: u.ID, ExpiresAt: now.Unix() + 60, CreatedAt: now.Unix()}, store.Refresh{Hash: "r", SessionID: "sid", CreatedAt: now.Unix()}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertCIMDClient(ctx, store.OAuthClient{ClientID: "https://claude.ai/c", Name: "Claude", RedirectURIs: []string{"https://claude.ai/cb"}, MetadataExpiresAt: sql.NullInt64{Int64: now.Unix() + 60, Valid: true}}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateOAuthGrant(ctx, store.OAuthGrant{ID: "g", UserID: u.ID, ClientID: "https://claude.ai/c", ClientName: "Claude", Scope: "mcp", Resource: MCPResource("https://astovia.test"), RedirectURI: "https://claude.ai/cb", CodeHash: "code", CodeChallenge: "c", CodeExpiresAt: now.Unix() + 60, CreatedAt: now.Unix(), ExpiresAt: now.Unix() + 60}, "sid", now.Unix()); err != nil {
		t.Fatal(err)
	}
	raw, hash, err := NewOAuthAccessToken()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.RedeemOAuthCode(ctx, "code", now.Unix(), func(store.OAuthGrant) ([]store.OAuthToken, error) {
		return []store.OAuthToken{{Hash: hash, Kind: store.OAuthAccess, CreatedAt: now.Unix(), ExpiresAt: now.Unix() + 3600}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if got, err := a.AuthenticateOAuthAccess(ctx, raw); err != nil || got.ID != u.ID {
		t.Fatalf("authenticate: %+v %v", got, err)
	}
	// The hash, an API token form and a malformed value are rejected.
	for _, v := range []string{hash, "ast_" + strings.TrimPrefix(raw, "aso_"), raw + "x"} {
		if _, err = a.AuthenticateOAuthAccess(ctx, v); !errors.Is(err, store.ErrUnauthorized) {
			t.Fatalf("%q: %v", v, err)
		}
	}
	// The token is only for the MCP resource of the issuing public URL.
	if _, err = service("https://other.test").AuthenticateOAuthAccess(ctx, raw); !errors.Is(err, store.ErrUnauthorized) {
		t.Fatalf("other resource: %v", err)
	}
}
