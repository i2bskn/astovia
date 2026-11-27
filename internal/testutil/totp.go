package testutil

import (
	"crypto/hmac"
	"crypto/sha1" // Authenticator apps use HMAC-SHA1 (RFC 6238).
	"encoding/base32"
	"encoding/binary"
	"time"
)

// TOTPStep returns the 30-second time step of t.
func TOTPStep(t time.Time) int64 { return t.Unix() / 30 }

// TOTPCode returns the 6-digit code of the base32 secret (upper case, no
// padding) at step, as an authenticator app shows it, or false when the
// secret is not valid base32. The server verifies codes with authlume's
// totp package; this computes them independently for the tests.
func TOTPCode(secret string, step int64) (string, bool) {
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secret)
	if err != nil || len(key) == 0 || step < 0 {
		return "", false
	}
	return hotp(key, step, 6), true
}

// hotp is the HOTP value (RFC 4226) of key at counter step with the given
// number of digits.
func hotp(key []byte, step int64, digits int) string {
	var counter [8]byte
	binary.BigEndian.PutUint64(counter[:], uint64(step))
	mac := hmac.New(sha1.New, key)
	mac.Write(counter[:])
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 0x0f
	value := binary.BigEndian.Uint32(sum[offset:offset+4]) & 0x7fffffff
	out := make([]byte, digits)
	for i := digits - 1; i >= 0; i-- {
		out[i] = byte('0' + value%10)
		value /= 10
	}
	return string(out)
}
