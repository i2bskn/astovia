package mfa

import (
	"crypto/rand"
	"encoding/base32"
	"net/url"
	"strings"
)

const (
	// totpSecretSize is the size of a new secret in bytes, the output size
	// of SHA-1.
	totpSecretSize = 20
	// totpDigits is the length of a code.
	totpDigits = 6
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

// TOTPSecretBytes decodes a secret in the base32 form of NewTOTPSecret.
func TOTPSecretBytes(secret string) ([]byte, error) { return secretEncoding.DecodeString(secret) }
