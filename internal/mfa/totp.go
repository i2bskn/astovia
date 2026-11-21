package mfa

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" // Authenticator apps use HMAC-SHA1 (RFC 6238).
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"net/url"
	"strings"
	"time"
)

const (
	// totpSecretSize is the size of a new secret in bytes, the output size
	// of SHA-1.
	totpSecretSize = 20
	// totpPeriod is the length of a time step in seconds.
	totpPeriod = 30
	// totpDigits is the length of a code.
	totpDigits = 6
	// totpSkew is the number of time steps accepted before and after the
	// current one.
	totpSkew = 1
)

// secretEncoding is how a secret is shown to the user and put in the
// otpauth URL: upper-case base32 without padding.
var secretEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewTOTPSecret returns a new random secret in its base32 form.
func NewTOTPSecret() (string, error) {
	b := make([]byte, totpSecretSize)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return secretEncoding.EncodeToString(b), nil
}

// TOTPStep returns the time step of t.
func TOTPStep(t time.Time) int64 { return t.Unix() / totpPeriod }

// TOTPCode returns the code of the base32 secret at step, or false when the
// secret is not valid base32.
func TOTPCode(secret string, step int64) (string, bool) {
	key, err := secretEncoding.DecodeString(secret)
	if err != nil || len(key) == 0 || step < 0 {
		return "", false
	}
	return totpCode(key, step, totpDigits), true
}

// totpCode is the HOTP value (RFC 4226) of key at counter step with the
// given number of digits.
func totpCode(key []byte, step int64, digits int) string {
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

// NormalizeTOTPCode removes the spaces of a code typed by the user and
// reports whether six digits remain.
func NormalizeTOTPCode(v string) (string, bool) {
	if len(v) > 64 {
		return "", false
	}
	code := strings.Join(strings.Fields(v), "")
	if len(code) != totpDigits {
		return "", false
	}
	for i := 0; i < len(code); i++ {
		if code[i] < '0' || code[i] > '9' {
			return "", false
		}
	}
	return code, true
}

// MatchTOTP returns the time step whose code is code: the step of now or
// one step before or after it. Steps up to after, the last step accepted
// for the secret, never match, so that a code cannot be used again. code
// must come from NormalizeTOTPCode. Every candidate step is compared, in
// constant time.
func MatchTOTP(secret, code string, now time.Time, after int64) (int64, bool) {
	key, err := secretEncoding.DecodeString(secret)
	if err != nil || len(key) == 0 {
		return 0, false
	}
	current := TOTPStep(now)
	matched, found := int64(0), 0
	for step := current - totpSkew; step <= current+totpSkew; step++ {
		if step < 0 {
			continue
		}
		equal := subtle.ConstantTimeCompare([]byte(totpCode(key, step, totpDigits)), []byte(code))
		if equal == 1 && step > after {
			matched, found = step, 1
		}
	}
	return matched, found == 1
}

// TOTPURL returns the otpauth URL an authenticator app reads from a QR
// code. The issuer carries host, so that the accounts of several Astovia
// servers can be told apart in the app.
func TOTPURL(secret, host, email string) string {
	issuer := "Astovia (" + host + ")"
	q := url.Values{}
	q.Set("secret", secret)
	q.Set("issuer", issuer)
	q.Set("algorithm", "SHA1")
	q.Set("digits", "6")
	q.Set("period", "30")
	// url.Values encodes a space as "+", which some apps show as it is.
	return "otpauth://totp/" + url.PathEscape(issuer+":"+email) + "?" + strings.ReplaceAll(q.Encode(), "+", "%20")
}
