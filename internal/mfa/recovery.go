package mfa

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

const (
	// RecoveryCodeCount is the number of recovery codes issued at once.
	RecoveryCodeCount = 10
	// recoveryAlphabet is the 32 characters of a recovery code: lower-case
	// letters and digits without l, o, 0 and 1, which are easily confused.
	recoveryAlphabet = "abcdefghijkmnpqrstuvwxyz23456789"
	// recoveryCodeLength is the length of a code without its hyphens. Each
	// character carries 5 bits, so a code has 80 random bits.
	recoveryCodeLength = 16
)

// NewRecoveryCodes returns RecoveryCodeCount new random codes in the form
// shown to the user, xxxx-xxxx-xxxx-xxxx.
func NewRecoveryCodes() ([]string, error) {
	codes := make([]string, 0, RecoveryCodeCount)
	for len(codes) < RecoveryCodeCount {
		// 10 random bytes are the 80 bits of the 16 characters.
		b := make([]byte, recoveryCodeLength*5/8)
		if _, err := rand.Read(b); err != nil {
			return nil, err
		}
		var out strings.Builder
		bits, n := uint32(0), 0
		written := 0
		for _, c := range b {
			bits, n = bits<<8|uint32(c), n+8
			for n >= 5 {
				if written > 0 && written%4 == 0 {
					out.WriteByte('-')
				}
				out.WriteByte(recoveryAlphabet[bits>>(n-5)&0x1f])
				n -= 5
				written++
			}
		}
		codes = append(codes, out.String())
	}
	return codes, nil
}

// NormalizeRecoveryCode removes the spaces and hyphens of a code typed by
// the user and lower-cases it. It reports whether the result has the form
// of a recovery code; other values are rejected before anything is hashed.
func NormalizeRecoveryCode(v string) (string, bool) {
	if len(v) > 128 {
		return "", false
	}
	code := strings.ToLower(strings.ReplaceAll(strings.Join(strings.Fields(v), ""), "-", ""))
	if len(code) != recoveryCodeLength {
		return "", false
	}
	for i := 0; i < len(code); i++ {
		if strings.IndexByte(recoveryAlphabet, code[i]) < 0 {
			return "", false
		}
	}
	return code, true
}

// RecoveryCodeHash returns the value stored for a normalized code: its
// SHA-256 in hexadecimal, as auth.Hash.
func RecoveryCodeHash(normalized string) string {
	h := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(h[:])
}
