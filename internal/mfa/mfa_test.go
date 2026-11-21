package mfa

import (
	"encoding/base32"
	"net/url"
	"strings"
	"testing"
	"time"
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

func TestTOTPVectors(t *testing.T) {
	// RFC 6238 appendix B, SHA-1: the 8-digit values, of which the last six
	// digits are the 6-digit codes.
	for _, tc := range []struct {
		unix  int64
		eight string
	}{
		{59, "94287082"},
		{1111111109, "07081804"},
		{1111111111, "14050471"},
		{1234567890, "89005924"},
		{2000000000, "69279037"},
		{20000000000, "65353130"},
	} {
		step := TOTPStep(time.Unix(tc.unix, 0))
		if got := totpCode([]byte("12345678901234567890"), step, 8); got != tc.eight {
			t.Errorf("8 digits at %d: %s, want %s", tc.unix, got, tc.eight)
		}
		got, ok := TOTPCode(rfcSecret, step)
		if !ok || got != tc.eight[2:] {
			t.Errorf("6 digits at %d: %s %v, want %s", tc.unix, got, ok, tc.eight[2:])
		}
	}
	if _, ok := TOTPCode("not base32!", 1); ok {
		t.Error("an invalid secret produced a code")
	}
	if _, ok := TOTPCode(rfcSecret, -1); ok {
		t.Error("a negative step produced a code")
	}
}

func TestMatchTOTP(t *testing.T) {
	now := time.Unix(1111111111, 0)
	current := TOTPStep(now)
	code := func(step int64) string {
		t.Helper()
		c, ok := TOTPCode(rfcSecret, step)
		if !ok {
			t.Fatal("no code")
		}
		return c
	}
	for delta := int64(-1); delta <= 1; delta++ {
		step, ok := MatchTOTP(rfcSecret, code(current+delta), now, 0)
		if !ok || step != current+delta {
			t.Errorf("delta %d: %d %v", delta, step, ok)
		}
	}
	for _, delta := range []int64{-2, 2, 100} {
		if _, ok := MatchTOTP(rfcSecret, code(current+delta), now, 0); ok {
			t.Errorf("delta %d matched", delta)
		}
	}
	// A step that was accepted, and every step before it, no longer match.
	if _, ok := MatchTOTP(rfcSecret, code(current), now, current); ok {
		t.Error("the accepted step matched again")
	}
	if _, ok := MatchTOTP(rfcSecret, code(current-1), now, current); ok {
		t.Error("a step before the accepted one matched")
	}
	if step, ok := MatchTOTP(rfcSecret, code(current+1), now, current); !ok || step != current+1 {
		t.Errorf("the step after the accepted one: %d %v", step, ok)
	}
	if _, ok := MatchTOTP("not base32!", code(current), now, 0); ok {
		t.Error("an invalid secret matched")
	}
	if _, ok := MatchTOTP("", "", now, 0); ok {
		t.Error("an empty secret matched")
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
	if _, ok := TOTPCode(a, 1); !ok {
		t.Fatal("a new secret produced no code")
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
