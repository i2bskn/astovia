package auth

import (
	"context"
	"strings"
	"testing"

	"github.com/i2bskn/authlume/password"
)

func TestPasswordAndEmailBoundaries(t *testing.T) {
	for _, tc := range []struct {
		length int
		ok     bool
	}{{11, false}, {12, true}, {1024, true}, {1025, false}} {
		if _, err := Password(strings.Repeat("p", tc.length)); (err == nil) != tc.ok {
			t.Fatalf("Password with %d bytes: err=%v", tc.length, err)
		}
	}
	local64 := strings.Repeat("a", 64)
	domain := strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + "."
	valid254 := local64 + "@" + domain + strings.Repeat("d", 61)
	invalid255 := local64 + "@" + domain + strings.Repeat("d", 62)
	if len(valid254) != 254 || len(invalid255) != 255 {
		t.Fatalf("fixture lengths: %d %d", len(valid254), len(invalid255))
	}
	padded257 := "alice@example.com" + strings.Repeat(" ", 257-len("alice@example.com"))
	for _, tc := range []struct {
		name  string
		input string
		ok    bool
	}{
		{"local part of 64 bytes", local64 + "@example.com", true},
		{"254 bytes", valid254, true},
		{"domain label of 64 characters", "alice@" + strings.Repeat("b", 64) + ".com", false},
		{"local part of 65 bytes", strings.Repeat("a", 65) + "@example.com", false},
		{"255 bytes", invalid255, false},
		{"257 bytes before trimming", padded257, false},
	} {
		if _, err := Email(tc.input); (err == nil) != tc.ok {
			t.Fatalf("Email with %s: err=%v", tc.name, err)
		}
	}
	if _, err := Email(padded257); err == nil || err.Error() != "email is too long" {
		t.Fatalf("Email with 257 bytes before trimming: err=%v", err)
	}
}

func TestPasswordAndEmail(t *testing.T) {
	for _, s := range []string{
		"", "   ", "alice", "@example.com", "alice@", "alice@@example.com", "a@b@example.com",
		"alice@-example.com", "alice@example-.com", "alice@.example.com", "alice@example.com.", "alice@ex..com",
		"alice @example.com", "アリス@example.com", "alice@例え.jp",
		"\u212alice@example.com", "\u3000alice@example.com\u3000",
	} {
		if _, e := Email(s); e == nil {
			t.Fatalf("accepted %q", s)
		}
	}
	for input, want := range map[string]string{
		"alice@example.com":     "alice@example.com",
		" Alice@Example.COM ":   "alice@example.com",
		"\talice@example.com\n": "alice@example.com",
		"alice+tag@example.com": "alice+tag@example.com",
		"a.b@sub.example.co.jp": "a.b@sub.example.co.jp",
		"alice@localhost":       "alice@localhost",
		"alice@xn--r8jz45g.jp":  "alice@xn--r8jz45g.jp",
	} {
		if v, e := Email(input); e != nil || v != want {
			t.Fatalf("Email(%q) = %q, %v; want %q", input, v, e, want)
		}
	}
	if _, e := Password("short"); e == nil {
		t.Fatal("short password accepted")
	}
}

// A stored password is an Argon2id record that authlume's password package
// accepts.
func TestPasswordRecord(t *testing.T) {
	record, err := Password("correct-password-123")
	if err != nil || !strings.HasPrefix(record, "v2$argon2id$") {
		t.Fatalf("record %q %v", record, err)
	}
	a, err := password.New(func(_ context.Context, id string) (password.Credential, error) {
		return password.Credential{Subject: id, Hash: record}, nil
	}, func(context.Context, string) (password.Decision, error) { return password.PasswordOnly, nil })
	if err != nil {
		t.Fatal(err)
	}
	if r, err := a.Authenticate(context.Background(), "alice", "correct-password-123"); err != nil || r.Subject != "alice" {
		t.Fatalf("authenticate: %+v %v", r, err)
	}
	if _, err := a.Authenticate(context.Background(), "alice", "wrong-password-123"); err != password.ErrCredentials {
		t.Fatalf("wrong password: %v", err)
	}
}
