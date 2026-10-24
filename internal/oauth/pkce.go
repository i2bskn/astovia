package oauth

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
)

// ValidCodeChallenge reports whether c is an S256 code challenge: 43
// base64url characters encoding 32 bytes.
func ValidCodeChallenge(c string) bool {
	if len(c) != 43 {
		return false
	}
	b, err := base64.RawURLEncoding.DecodeString(c)
	return err == nil && len(b) == 32
}

// validCodeVerifier checks the characters and length of RFC 7636 section 4.1.
func validCodeVerifier(v string) bool {
	if len(v) < 43 || len(v) > 128 {
		return false
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '.' || c == '_' || c == '~') {
			return false
		}
	}
	return true
}

// VerifyPKCE reports whether base64url(SHA-256(verifier)) equals challenge.
// Only S256 is supported.
func VerifyPKCE(verifier, challenge string) bool {
	if !validCodeVerifier(verifier) {
		return false
	}
	sum := sha256.Sum256([]byte(verifier))
	computed := base64.RawURLEncoding.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(computed), []byte(challenge)) == 1
}
