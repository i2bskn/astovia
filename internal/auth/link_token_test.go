package auth

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/i2bskn/astovia/internal/store"
	"github.com/i2bskn/astovia/internal/testutil"
)

// The invitation and password reset tokens have their own prefixes and are
// never taken for API or OAuth tokens, nor the other way round.
func TestLinkTokens(t *testing.T) {
	for _, tc := range []struct {
		prefix string
		create func() (string, string, error)
		hash   func(string) (string, bool)
	}{
		{"inv_", NewInvitationToken, InvitationTokenHash},
		{"pwr_", NewPasswordResetToken, PasswordResetTokenHash},
	} {
		seen := map[string]bool{}
		for i := 0; i < 10; i++ {
			raw, hash, err := tc.create()
			if err != nil {
				t.Fatal(err)
			}
			if len(raw) != 47 || !strings.HasPrefix(raw, tc.prefix) || hash != Hash(raw) || seen[raw] {
				t.Fatalf("token %q hash %q", raw, hash)
			}
			seen[raw] = true
			if got, ok := tc.hash(raw); !ok || got != hash {
				t.Fatalf("%q: lookup hash %q %v", raw, got, ok)
			}
			if IsAPIToken(raw) || IsOAuthAccessToken(raw) || IsOAuthRefreshToken(raw) || IsOAuthCode(raw) || strings.HasPrefix(raw, "ast_") || strings.HasPrefix(raw, "aso_") {
				t.Fatalf("%q is taken for another kind of token", raw)
			}
		}
	}

	body := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	api, _, err := NewAPIToken()
	if err != nil {
		t.Fatal(err)
	}
	access, _, err := NewOAuthAccessToken()
	if err != nil {
		t.Fatal(err)
	}
	reset, _, err := NewPasswordResetToken()
	if err != nil {
		t.Fatal(err)
	}
	invitation, _, err := NewInvitationToken()
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{
		"",
		"inv_",
		body,
		"INV_" + body,
		"inv-" + body,
		"inv_" + body[:42],
		"inv_" + body + "A",
		"inv_" + body[:42] + "*",
		"inv_" + body[:42] + "=",
		"inv_" + strings.Repeat("A", 43) + "\n",
		" inv_" + body,
		api,
		access,
		reset,
	} {
		if h, ok := InvitationTokenHash(v); ok || h != "" {
			t.Errorf("invitation accepted %q", v)
		}
	}
	for _, v := range []string{"", "pwr_", "pwr_" + body[:42], "pwr_" + body + "A", "PWR_" + body, api, access, invitation} {
		if h, ok := PasswordResetTokenHash(v); ok || h != "" {
			t.Errorf("password reset accepted %q", v)
		}
	}
	if _, ok := InvitationTokenHash("inv_" + body); !ok {
		t.Fatal("well-formed invitation token rejected")
	}
}

// LoginUser starts a session without a password, but only for the stored
// user as it is: a changed password hash or a disabled user is refused.
func TestLoginUser(t *testing.T) {
	ctx := context.Background()
	s, _ := testutil.Database(t, "sqlite")
	a, err := New(s, contractSecret, "https://astovia.test")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0)
	a.Now = func() time.Time { return now }
	u := store.User{ID: "alice-id", Email: "alice@example.com", PasswordHash: "stored", CreatedAt: now.Unix(), Role: store.RoleMember}
	if err = s.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	result, err := a.LoginUser(ctx, u)
	if err != nil || result.User.ID != u.ID || result.User.Role != store.RoleMember || result.Refresh == "" || result.ExpiresIn != int(AccessLifetime/time.Second) {
		t.Fatalf("login: %+v %v", result, err)
	}
	if got, err := a.Authenticate(ctx, result.AccessToken); err != nil || got.ID != u.ID {
		t.Fatalf("authenticate: %+v %v", got, err)
	}
	stale := u
	stale.PasswordHash = "other"
	if _, err = a.LoginUser(ctx, stale); !errors.Is(err, store.ErrUnauthorized) {
		t.Fatalf("stale hash: %v", err)
	}
	if err = s.ChangeUser(ctx, u.Email, "", true, now.Unix()); err != nil {
		t.Fatal(err)
	}
	if _, err = a.LoginUser(ctx, u); !errors.Is(err, store.ErrUnauthorized) {
		t.Fatalf("disabled: %v", err)
	}
}
