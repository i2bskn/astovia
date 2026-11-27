package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"github.com/i2bskn/astovia/internal/store"
	"github.com/i2bskn/authlume/password"
	"log"
	"regexp"
	"strings"
	"time"
)

// Lengths of a password in UTF-8 bytes. The lower bound is checked only when
// a password is stored; the upper bound is also the limit of a login.
const (
	MinPasswordBytes = 12
	MaxPasswordBytes = 1024
)

// emailPattern is the WHATWG HTML "valid e-mail address" grammar restricted
// to lower-case ASCII. A dot in the domain is not required, so addresses such
// as alice@localhost are accepted.
var emailPattern = regexp.MustCompile("^[a-z0-9.!#$%&'*+/=?^_`{|}~-]+@[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)*$")

const emailFormatError = "email must be a valid ASCII address such as alice@example.com, up to 254 characters"

// Email trims surrounding ASCII whitespace, lower-cases and validates a login
// email address. Non-ASCII input is rejected before trimming and case folding
// so that Unicode case mappings (such as U+212A KELVIN SIGN to "k") and
// Unicode spaces cannot produce a different ASCII address.
func Email(v string) (string, error) {
	if len(v) > 256 {
		return "", errors.New("email is too long")
	}
	for i := 0; i < len(v); i++ {
		if v[i] >= 0x80 {
			return "", errors.New(emailFormatError)
		}
	}
	v = strings.ToLower(strings.Trim(v, " \t\n\r\v\f"))
	if len(v) > 254 || !emailPattern.MatchString(v) {
		return "", errors.New(emailFormatError)
	}
	if local := strings.IndexByte(v, '@'); local < 1 || local > 64 {
		return "", errors.New(emailFormatError)
	}
	return v, nil
}

// Password validates a new password and returns its stored record. A
// password must contain 12–1024 UTF-8 bytes; the record is an Argon2id hash
// from authlume's password package.
func Password(v string) (string, error) {
	if len(v) < MinPasswordBytes || len(v) > MaxPasswordBytes {
		return "", errors.New("password must contain 12–1024 UTF-8 bytes")
	}
	return password.Hash(v)
}

func Random() (string, error) {
	b := make([]byte, 32)
	if _, e := rand.Read(b); e != nil {
		return "", e
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// Hash is the lowercase hex SHA-256 of v: the stored form of the API, link
// and recovery tokens and the digest of a password hash. It is store.Digest,
// so that the store compares the digests it is given the same way.
func Hash(v string) string { return store.Digest(v) }
func validToken(v string) bool {
	if len(v) > 128 {
		return false
	}
	b, e := base64.RawURLEncoding.DecodeString(v)
	return e == nil && len(b) == 32
}

// apiTokenPrefix marks API tokens so that they can be told apart from other
// bearer values without a database lookup.
const apiTokenPrefix = "ast_"

// NewAPIToken returns a new API token and the hash stored for it.
func NewAPIToken() (raw, hash string, err error) {
	r, err := Random()
	if err != nil {
		return "", "", err
	}
	raw = apiTokenPrefix + r
	return raw, Hash(raw), nil
}

// IsAPIToken reports whether v has the form of a token from NewAPIToken.
func IsAPIToken(v string) bool {
	return len(v) == len(apiTokenPrefix)+43 && strings.HasPrefix(v, apiTokenPrefix) && validToken(v[len(apiTokenPrefix):])
}

// Prefixes of the invitation and password reset tokens. They differ from
// the "ast_" API tokens, so that a link token
// presented as a bearer token is never taken for one of them.
const (
	invitationTokenPrefix    = "inv_"
	passwordResetTokenPrefix = "pwr_"
)

// NewInvitationToken returns a new invitation token and the hash stored for
// it.
func NewInvitationToken() (raw, hash string, err error) {
	return newPrefixed(invitationTokenPrefix)
}

// NewPasswordResetToken returns a new password reset token and the hash
// stored for it.
func NewPasswordResetToken() (raw, hash string, err error) {
	return newPrefixed(passwordResetTokenPrefix)
}

// InvitationTokenHash returns the hash to look up an invitation token, or
// false when v does not have the form of a token from NewInvitationToken.
// Malformed values are rejected before hashing.
func InvitationTokenHash(v string) (string, bool) {
	if !isPrefixed(v, invitationTokenPrefix) {
		return "", false
	}
	return Hash(v), true
}

// PasswordResetTokenHash returns the hash to look up a password reset token,
// or false when v does not have the form of a token from
// NewPasswordResetToken.
func PasswordResetTokenHash(v string) (string, bool) {
	if !isPrefixed(v, passwordResetTokenPrefix) {
		return "", false
	}
	return Hash(v), true
}

// Service authenticates API tokens. Browser sessions, passwords and second
// factors are handled by authlume in the HTTP layer.
type Service struct {
	Store *store.Store
	Now   func() time.Time
}

// New returns a Service for s with the system clock.
func New(s *store.Store) *Service {
	return &Service{Store: s, Now: time.Now}
}

// AuthenticateAPIToken returns the user of an API token. A failure to record
// the time of use is logged and does not fail authentication.
func (s *Service) AuthenticateAPIToken(ctx context.Context, raw string) (store.User, error) {
	if !IsAPIToken(raw) {
		return store.User{}, store.ErrUnauthorized
	}
	u, touchErr, err := s.Store.AuthenticateAPIToken(ctx, Hash(raw), s.Now().Unix())
	if err != nil {
		return store.User{}, err
	}
	if touchErr != nil {
		log.Printf("api token: touch user=%s: %v", u.ID, touchErr)
	}
	return u, nil
}
