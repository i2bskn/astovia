package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"github.com/golang-jwt/jwt/v5"
	"github.com/i2bskn/astovia/internal/store"
	"golang.org/x/crypto/bcrypt"
	"log"
	"regexp"
	"strings"
	"time"
)

const AccessLifetime = 10 * time.Minute
const SessionLifetime = 7 * 24 * time.Hour

// DefaultPasswordCost is the bcrypt cost used in production.
const DefaultPasswordCost = 12

// PasswordCost is the bcrypt cost used for new hashes. Only a test package's
// TestMain may lower it; production code must leave it at DefaultPasswordCost.
var PasswordCost = DefaultPasswordCost

const audience = "astovia-browser"

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
func Password(v string) (string, error) {
	if len(v) < 12 || len(v) > 72 {
		return "", errors.New("password must contain 12–72 UTF-8 bytes")
	}
	h, e := bcrypt.GenerateFromPassword([]byte(v), PasswordCost)
	return string(h), e
}
func Random() (string, error) {
	b := make([]byte, 32)
	if _, e := rand.Read(b); e != nil {
		return "", e
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
func Hash(v string) string { h := sha256.Sum256([]byte(v)); return hex.EncodeToString(h[:]) }
func validToken(v string) bool {
	if len(v) > 128 {
		return false
	}
	b, e := base64.RawURLEncoding.DecodeString(v)
	return e == nil && len(b) == 32
}

// apiTokenPrefix marks API tokens so that they can be told apart from access
// JWTs without parsing.
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
// the "ast_" API tokens and the "aso_" OAuth tokens, so that a link token
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

type claims struct {
	Use       string `json:"use"`
	SessionID string `json:"sid"`
	jwt.RegisteredClaims
}
type Service struct {
	Store  *store.Store
	secret []byte
	issuer string
	dummy  []byte
	Now    func() time.Time
}

// Result is the outcome of a login or refresh. It is an internal type; HTTP
// handlers copy the public fields into their own response type.
type Result struct {
	AccessToken string
	ExpiresIn   int
	User        store.User
	Refresh     string
	// ExpiresAt is the session expiry (Unix seconds).
	ExpiresAt int64
	// MaxAge is the refresh cookie lifetime in seconds, computed from the
	// same clock reading as ExpiresIn.
	MaxAge int
}

func New(s *store.Store, secret []byte, issuer string) (*Service, error) {
	d, e := bcrypt.GenerateFromPassword([]byte("astovia-dummy-password"), PasswordCost)
	if e != nil {
		return nil, e
	}
	return &Service{Store: s, secret: secret, issuer: issuer, dummy: d, Now: time.Now}, nil
}
func (s *Service) Login(ctx context.Context, email, password string) (Result, error) {
	normalized, e := Email(email)
	if e != nil || len(password) < 1 || len(password) > 72 {
		return Result{}, store.ErrUnauthorized
	}
	u, e := s.Store.UserByEmail(ctx, normalized)
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return Result{}, e
	}
	hash := s.dummy
	if e == nil {
		hash = []byte(u.PasswordHash)
	}
	check := bcrypt.CompareHashAndPassword(hash, []byte(password))
	if e != nil || check != nil || u.Disabled != 0 {
		return Result{}, store.ErrUnauthorized
	}
	return s.LoginUser(ctx, u)
}

// LoginUser starts a session for u without a password, after the caller
// has verified the user another way: accepting an invitation or completing
// a password reset. u must be the stored user, including its current
// password hash; the store rejects a disabled user or a changed hash.
func (s *Service) LoginUser(ctx context.Context, u store.User) (Result, error) {
	sid, e := Random()
	if e != nil {
		return Result{}, e
	}
	raw, e := Random()
	if e != nil {
		return Result{}, e
	}
	now := s.Now()
	unix := now.Unix()
	sess := store.Session{ID: sid, UserID: u.ID, CreatedAt: unix, ExpiresAt: unix + int64(SessionLifetime/time.Second)}
	if e = s.Store.Login(ctx, u, sess, store.Refresh{Hash: Hash(raw), SessionID: sid, CreatedAt: unix}); e != nil {
		return Result{}, e
	}
	return s.result(u, sess, raw, now)
}
func (s *Service) Refresh(ctx context.Context, raw string) (Result, error) {
	if !validToken(raw) {
		return Result{}, store.ErrUnauthorized
	}
	next, e := Random()
	if e != nil {
		return Result{}, e
	}
	now := s.Now()
	u, sess, e := s.Store.Rotate(ctx, Hash(raw), Hash(next), now.Unix())
	if e != nil {
		return Result{}, e
	}
	return s.result(u, sess, next, now)
}
func (s *Service) Logout(ctx context.Context, raw string) error {
	if !validToken(raw) {
		return nil
	}
	return s.Store.Logout(ctx, Hash(raw), s.Now().Unix())
}

// result issues an access token. now must be the same clock reading that was
// passed to the store, so that the store's ExpiresAt > now check also holds
// here.
func (s *Service) result(u store.User, sess store.Session, raw string, now time.Time) (Result, error) {
	exp := now.Add(AccessLifetime)
	if exp.Unix() > sess.ExpiresAt {
		exp = time.Unix(sess.ExpiresAt, 0)
	}
	// Defensive: with the same now, Login and Rotate guarantee
	// sess.ExpiresAt > now, so this branch is not normally reached.
	if exp.Unix() <= now.Unix() {
		return Result{}, store.ErrUnauthorized
	}
	id, e := Random()
	if e != nil {
		return Result{}, e
	}
	c := claims{Use: "access", SessionID: sess.ID, RegisteredClaims: jwt.RegisteredClaims{Issuer: s.issuer, Subject: u.ID, Audience: jwt.ClaimStrings{audience}, ExpiresAt: jwt.NewNumericDate(exp), IssuedAt: jwt.NewNumericDate(now), ID: id}}
	token, e := jwt.NewWithClaims(jwt.SigningMethodHS256, c).SignedString(s.secret)
	if e != nil {
		return Result{}, e
	}
	return Result{AccessToken: token, ExpiresIn: int(exp.Unix() - now.Unix()), User: u, Refresh: raw, ExpiresAt: sess.ExpiresAt, MaxAge: int(sess.ExpiresAt - now.Unix())}, nil
}
func (s *Service) Authenticate(ctx context.Context, raw string) (store.User, error) {
	u, _, err := s.AuthenticateSession(ctx, raw)
	return u, err
}

// AuthenticateSession validates an access JWT and returns its user together
// with the verified session ID (the sid claim).
func (s *Service) AuthenticateSession(ctx context.Context, raw string) (store.User, string, error) {
	if len(raw) > 4096 {
		return store.User{}, "", store.ErrUnauthorized
	}
	c := new(claims)
	t, e := jwt.ParseWithClaims(raw, c, func(_ *jwt.Token) (any, error) { return s.secret, nil }, jwt.WithValidMethods([]string{"HS256"}), jwt.WithIssuer(s.issuer), jwt.WithAudience(audience), jwt.WithExpirationRequired(), jwt.WithIssuedAt(), jwt.WithTimeFunc(s.Now))
	if e != nil || !t.Valid || c.Use != "access" || c.Subject == "" || c.SessionID == "" || c.ID == "" || c.IssuedAt == nil {
		return store.User{}, "", store.ErrUnauthorized
	}
	u, e := s.Store.Authenticated(ctx, c.Subject, c.SessionID, s.Now().Unix())
	if e != nil {
		return store.User{}, "", e
	}
	return u, c.SessionID, nil
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
