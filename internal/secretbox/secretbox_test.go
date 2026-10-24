package secretbox

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

func TestSecretbox(t *testing.T) {
	b := New([]byte(strings.Repeat("k", 32)))
	sealed, err := b.Seal("github-token:alice", "ghp_secret")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(sealed, "v1.") || strings.Contains(sealed, "ghp_secret") || strings.Contains(sealed, "=") {
		t.Fatalf("sealed: %q", sealed)
	}
	if again, _ := b.Seal("github-token:alice", "ghp_secret"); again == sealed {
		t.Fatal("nonce is not random")
	}
	if got, err := b.Open("github-token:alice", sealed); err != nil || got != "ghp_secret" {
		t.Fatalf("open: %q %v", got, err)
	}
	empty, err := b.Seal("p", "")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := b.Open("p", empty); err != nil || got != "" {
		t.Fatalf("empty: %q %v", got, err)
	}

	raw, _ := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(sealed, "v1."))
	tampered := append([]byte{}, raw...)
	tampered[len(tampered)-1] ^= 1
	for name, c := range map[string]struct {
		box            *Box
		purpose, value string
	}{
		"other purpose": {b, "github-token:bob", sealed},
		"other secret":  {New([]byte(strings.Repeat("x", 32))), "github-token:alice", sealed},
		"tampered":      {b, "github-token:alice", "v1." + base64.RawURLEncoding.EncodeToString(tampered)},
		"no prefix":     {b, "github-token:alice", strings.TrimPrefix(sealed, "v1.")},
		"other version": {b, "github-token:alice", "v2." + strings.TrimPrefix(sealed, "v1.")},
		"not base64":    {b, "github-token:alice", "v1.!!!"},
		"padded":        {b, "github-token:alice", sealed + "="},
		"too short":     {b, "github-token:alice", "v1.AAAA"},
		"empty":         {b, "github-token:alice", ""},
	} {
		if got, err := c.box.Open(c.purpose, c.value); !errors.Is(err, ErrUndecryptable) || got != "" {
			t.Fatalf("%s: %q %v", name, got, err)
		}
	}
}
