package auth

import "strings"

// newPrefixed returns prefix followed by Random() and the hash of the whole
// value.
func newPrefixed(prefix string) (raw, hash string, err error) {
	r, err := Random()
	if err != nil {
		return "", "", err
	}
	raw = prefix + r
	return raw, Hash(raw), nil
}

// isPrefixed reports whether v is prefix followed by 43 base64url characters
// that decode to 32 bytes, the form produced by newPrefixed.
func isPrefixed(v, prefix string) bool {
	return len(v) == len(prefix)+43 && strings.HasPrefix(v, prefix) && validToken(v[len(prefix):])
}
