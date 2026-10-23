package auth

import (
	"context"
	"encoding/base64"
	"errors"
	"github.com/i2bskn/astovia/internal/store"
	"github.com/i2bskn/astovia/internal/testutil"
	"strings"
	"testing"
	"time"
)

func TestNewAPIToken(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 10; i++ {
		raw, hash, err := NewAPIToken()
		if err != nil {
			t.Fatal(err)
		}
		if len(raw) != 47 || !strings.HasPrefix(raw, "ast_") || !IsAPIToken(raw) {
			t.Fatalf("token format: %q", raw)
		}
		if hash != Hash(raw) {
			t.Fatalf("hash %q is not Hash(raw)", hash)
		}
		if seen[raw] {
			t.Fatal("duplicate token")
		}
		seen[raw] = true
	}
}

func TestIsAPIToken(t *testing.T) {
	body := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	if !IsAPIToken("ast_" + body) {
		t.Fatal("valid token rejected")
	}
	jwt := "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiJhbGljZS1pZCJ9.c2lnbmF0dXJl"
	for _, v := range []string{
		"",
		"ast_",
		"asx_" + body,
		"AST_" + body,
		"ast-" + body,
		body + "abcd",
		"ast_" + body[:42],       // 46 characters
		"ast_" + body + "A",      // 48 characters
		"ast_" + body[:42] + "*", // not base64url
		"ast_" + body[:42] + "=", // padding
		"ast_" + strings.Repeat("A", 43) + "\n",
		jwt,
	} {
		if IsAPIToken(v) {
			t.Errorf("accepted %q", v)
		}
	}
}

func TestAuthenticateAPIToken(t *testing.T) {
	ctx := context.Background()
	s, _ := testutil.Database(t, "sqlite")
	a, err := New(s, contractSecret, "https://astovia.test")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0)
	a.Now = func() time.Time { return now }
	u := store.User{ID: "alice-id", Email: "alice@example.com", PasswordHash: "x"}
	if err = s.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	if err = s.Login(ctx, u, store.Session{ID: "sid", UserID: u.ID, ExpiresAt: now.Unix() + 60, CreatedAt: now.Unix()}, store.Refresh{Hash: "r", SessionID: "sid", CreatedAt: now.Unix()}); err != nil {
		t.Fatal(err)
	}
	raw, hash, err := NewAPIToken()
	if err != nil {
		t.Fatal(err)
	}
	if err = s.CreateAPIToken(ctx, store.APIToken{ID: "tok", UserID: u.ID, Name: "x", CreatedAt: now.Unix()}, hash, "sid", now.Unix()); err != nil {
		t.Fatal(err)
	}
	got, err := a.AuthenticateAPIToken(ctx, raw)
	if err != nil || got.ID != u.ID {
		t.Fatalf("authenticate: %+v %v", got, err)
	}
	other, _, err := NewAPIToken()
	if err != nil {
		t.Fatal(err)
	}
	// The hash itself, a malformed value and an unknown token are rejected.
	for _, v := range []string{other, hash, raw + "x", strings.TrimPrefix(raw, "ast_")} {
		if _, err = a.AuthenticateAPIToken(ctx, v); !errors.Is(err, store.ErrUnauthorized) {
			t.Fatalf("%q: %v", v, err)
		}
	}
}
