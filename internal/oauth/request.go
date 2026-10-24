package oauth

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// ErrInvalidRequest is returned for a consent request that is malformed,
// tampered with or expired. The cause is not distinguished.
var ErrInvalidRequest = errors.New("invalid or expired authorization request")

// Request is a validated authorization request carried from /oauth/authorize
// to the consent page. It is signed instead of stored, so it can be approved
// more than once within its lifetime; every code it yields is still bound to
// PKCE and the redirect URI and can be exchanged once within 60 seconds.
type Request struct {
	ClientID      string `json:"client_id"`
	ClientName    string `json:"client_name"`
	RedirectURI   string `json:"redirect_uri"`
	Scope         string `json:"scope"`
	State         string `json:"state,omitempty"`
	CodeChallenge string `json:"code_challenge"`
	Exp           int64  `json:"exp"`
}

const requestPurpose = "astovia-oauth-request.v1."

// requestSignature signs body with a key derived from the server secret, so
// that the JWT signing key is not used directly.
func requestSignature(secret []byte, body string) []byte {
	k := hmac.New(sha256.New, secret)
	k.Write([]byte("astovia oauth request key v1"))
	mac := hmac.New(sha256.New, k.Sum(nil))
	mac.Write([]byte(requestPurpose + body))
	return mac.Sum(nil)
}

// SignRequest returns base64url(JSON) "." base64url(signature).
func SignRequest(secret []byte, r Request) (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(r); err != nil {
		return "", err
	}
	body := base64.RawURLEncoding.EncodeToString(bytes.TrimSuffix(buf.Bytes(), []byte("\n")))
	return body + "." + base64.RawURLEncoding.EncodeToString(requestSignature(secret, body)), nil
}

// ParseRequest verifies the signature and expiry of v and returns the request.
func ParseRequest(secret []byte, v string, now time.Time) (Request, error) {
	body, sig, ok := strings.Cut(v, ".")
	if !ok || body == "" || sig == "" {
		return Request{}, ErrInvalidRequest
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !hmac.Equal(got, requestSignature(secret, body)) {
		return Request{}, ErrInvalidRequest
	}
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return Request{}, ErrInvalidRequest
	}
	var r Request
	if err = json.Unmarshal(raw, &r); err != nil {
		return Request{}, ErrInvalidRequest
	}
	if r.ClientID == "" || r.ClientName == "" || r.RedirectURI == "" || r.Scope == "" || r.CodeChallenge == "" || now.Unix() >= r.Exp {
		return Request{}, ErrInvalidRequest
	}
	return r, nil
}
