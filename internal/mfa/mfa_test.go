package mfa

import (
	"encoding/base32"
	"net/url"
	"strings"
	"testing"
)

// rfcSecret is the SHA-1 secret of RFC 6238 appendix B, "12345678901234567890".
var rfcSecret = base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString([]byte("12345678901234567890"))

func TestRequired(t *testing.T) {
	for _, tc := range []struct {
		policy string
		owner  bool
		want   bool
	}{
		{"", true, false}, {"", false, false},
		{PolicyOff, true, false}, {PolicyOff, false, false},
		{PolicyOwners, true, true}, {PolicyOwners, false, false},
		{PolicyAll, true, true}, {PolicyAll, false, true},
		{"OWNERS", true, false},
	} {
		if got := Required(tc.policy, tc.owner); got != tc.want {
			t.Errorf("Required(%q, %v) = %v", tc.policy, tc.owner, got)
		}
	}
}

func TestNormalizeTOTPCode(t *testing.T) {
	for in, want := range map[string]string{"123456": "123456", " 123 456 ": "123456", "1 2 3 4 5 6": "123456", "\t000000\n": "000000"} {
		if got, ok := NormalizeTOTPCode(in); !ok || got != want {
			t.Errorf("%q: %q %v", in, got, ok)
		}
	}
	for _, in := range []string{"", "12345", "1234567", "12345a", "123-456", "１２３４５６", "-12345", "+12345", strings.Repeat(" ", 100) + "123456"} {
		if got, ok := NormalizeTOTPCode(in); ok {
			t.Errorf("%q was accepted as %q", in, got)
		}
	}
}

func TestNewTOTPSecret(t *testing.T) {
	a, err := NewTOTPSecret()
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewTOTPSecret()
	if err != nil {
		t.Fatal(err)
	}
	if a == b || len(a) != 32 || strings.ContainsAny(a, "=") || strings.ToUpper(a) != a {
		t.Fatalf("secrets: %d characters, equal=%v", len(a), a == b)
	}
	if key, err := TOTPSecretBytes(a); err != nil || len(key) != totpSecretSize {
		t.Fatalf("a new secret does not decode: %d bytes, %v", len(key), err)
	}
}

func TestTOTPURL(t *testing.T) {
	raw := TOTPURL(rfcSecret, "astovia.example.com", "alice+test@example.com")
	if !strings.HasPrefix(raw, "otpauth://totp/Astovia%20%28astovia.example.com%29:alice+test@example.com?") {
		t.Fatalf("label: %s", raw)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme != "otpauth" || u.Host != "totp" || u.Path != "/Astovia (astovia.example.com):alice+test@example.com" {
		t.Fatalf("url: %s %s %s", u.Scheme, u.Host, u.Path)
	}
	q := u.Query()
	for k, want := range map[string]string{"secret": rfcSecret, "issuer": "Astovia (astovia.example.com)", "algorithm": "SHA1", "digits": "6", "period": "30"} {
		if q.Get(k) != want {
			t.Errorf("%s: %q", k, q.Get(k))
		}
	}
	if len(q) != 5 || strings.Contains(u.RawQuery, "+") {
		t.Fatalf("query: %s", u.RawQuery)
	}
}

func TestRecoveryCodes(t *testing.T) {
	codes, err := NewRecoveryCodes()
	if err != nil {
		t.Fatal(err)
	}
	if len(codes) != RecoveryCodeCount || RecoveryCodeCount != 10 {
		t.Fatalf("%d codes", len(codes))
	}
	if len(recoveryAlphabet) != 32 || strings.ContainsAny(recoveryAlphabet, "lo01") {
		t.Fatalf("alphabet: %q", recoveryAlphabet)
	}
	seen := map[string]bool{}
	for _, code := range codes {
		parts := strings.Split(code, "-")
		if len(code) != 19 || len(parts) != 4 {
			t.Fatalf("form: %d characters, %d parts", len(code), len(parts))
		}
		for _, p := range parts {
			if len(p) != 4 || strings.Trim(p, recoveryAlphabet) != "" {
				t.Fatalf("part of %d characters", len(p))
			}
		}
		normalized, ok := NormalizeRecoveryCode(code)
		if !ok || normalized != strings.ReplaceAll(code, "-", "") {
			t.Fatal("a new code is not normalized to itself")
		}
		if seen[normalized] {
			t.Fatal("duplicate code")
		}
		seen[normalized] = true
	}
}

func TestNormalizeRecoveryCode(t *testing.T) {
	const want = "abcdefghjkmnpqrs"
	for _, in := range []string{"abcd-efgh-jkmn-pqrs", "ABCD-EFGH-JKMN-PQRS", " abcd efgh\tjkmn pqrs\n", "abcdefghjkmnpqrs", "a-b-c-d-efghjkmnpqrs"} {
		if got, ok := NormalizeRecoveryCode(in); !ok || got != want {
			t.Errorf("%q: %q %v", in, got, ok)
		}
	}
	for _, in := range []string{"", "abcd-efgh-jkmn-pqr", "abcd-efgh-jkmn-pqrst", "abcd-efgh-jkmn-pqr0", "abcd-efgh-jkmn-pqrl", "abcd-efgh-jkmn-pqro", "abcd-efgh-jkmn-pqr1", "abcd_efgh_jkmn_pqrs", "ａbcd-efgh-jkmn-pqrs", strings.Repeat("-", 200) + want} {
		if got, ok := NormalizeRecoveryCode(in); ok {
			t.Errorf("%q was accepted as %q", in, got)
		}
	}
	// The hash is the SHA-256 of the normalized code in hexadecimal.
	if got := RecoveryCodeHash("abc"); got != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Fatalf("hash: %s", got)
	}
}
