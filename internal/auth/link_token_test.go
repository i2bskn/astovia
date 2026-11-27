package auth

import (
	"encoding/base64"
	"strings"
	"testing"
)

// The invitation and password reset tokens have their own prefixes and are
// never taken for API tokens, nor the other way round.
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
			if IsAPIToken(raw) || strings.HasPrefix(raw, "ast_") {
				t.Fatalf("%q is taken for another kind of token", raw)
			}
		}
	}

	body := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	api, _, err := NewAPIToken()
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
		reset,
	} {
		if h, ok := InvitationTokenHash(v); ok || h != "" {
			t.Errorf("invitation accepted %q", v)
		}
	}
	for _, v := range []string{"", "pwr_", "pwr_" + body[:42], "pwr_" + body + "A", "PWR_" + body, api, invitation} {
		if h, ok := PasswordResetTokenHash(v); ok || h != "" {
			t.Errorf("password reset accepted %q", v)
		}
	}
	if _, ok := InvitationTokenHash("inv_" + body); !ok {
		t.Fatal("well-formed invitation token rejected")
	}
}
