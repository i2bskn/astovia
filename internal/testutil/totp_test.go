package testutil

import (
	"encoding/base32"
	"testing"
	"time"
)

func TestTOTPVectors(t *testing.T) {
	// RFC 6238 appendix B, SHA-1, "12345678901234567890": the 8-digit
	// values, of which the last six digits are the 6-digit codes.
	secret := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString([]byte("12345678901234567890"))
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
		if got := hotp([]byte("12345678901234567890"), step, 8); got != tc.eight {
			t.Errorf("8 digits at %d: %s, want %s", tc.unix, got, tc.eight)
		}
		got, ok := TOTPCode(secret, step)
		if !ok || got != tc.eight[2:] {
			t.Errorf("6 digits at %d: %s %v, want %s", tc.unix, got, ok, tc.eight[2:])
		}
	}
	if _, ok := TOTPCode("not base32!", 1); ok {
		t.Error("an invalid secret produced a code")
	}
	if _, ok := TOTPCode(secret, -1); ok {
		t.Error("a negative step produced a code")
	}
}
