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
	"regexp"
	"strings"
	"time"
)

const AccessLifetime = 10 * time.Minute
const SessionLifetime = 7 * 24 * time.Hour
const PasswordCost = 12
const audience = "astovia-browser"

var usernamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{2,63}$`)

func Username(v string) (string, error) {
	if len(v) > 256 {
		return "", errors.New("username is too long")
	}
	v = strings.ToLower(strings.TrimSpace(v))
	if !usernamePattern.MatchString(v) {
		return "", errors.New("username must be 3–64 ASCII letters, numbers, dots, underscores or hyphens, starting with a letter or number")
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
type Result struct {
	AccessToken string     `json:"access_token"`
	ExpiresIn   int        `json:"expires_in"`
	User        store.User `json:"user"`
	Refresh     string     `json:"-"`
	ExpiresAt   int64      `json:"-"`
}

func New(s *store.Store, secret []byte, issuer string) (*Service, error) {
	d, e := bcrypt.GenerateFromPassword([]byte("astovia-dummy-password"), PasswordCost)
	if e != nil {
		return nil, e
	}
	return &Service{Store: s, secret: secret, issuer: issuer, dummy: d, Now: time.Now}, nil
}
func (s *Service) Login(ctx context.Context, name, password string) (Result, error) {
	normalized, e := Username(name)
	if e != nil || len(password) < 1 || len(password) > 72 {
		return Result{}, store.ErrUnauthorized
	}
	u, e := s.Store.UserByName(ctx, normalized)
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
	sid, e := Random()
	if e != nil {
		return Result{}, e
	}
	raw, e := Random()
	if e != nil {
		return Result{}, e
	}
	now := s.Now().Unix()
	sess := store.Session{ID: sid, UserID: u.ID, CreatedAt: now, ExpiresAt: now + int64(SessionLifetime/time.Second)}
	if e = s.Store.Login(ctx, u, sess, store.Refresh{Hash: Hash(raw), SessionID: sid, CreatedAt: now}); e != nil {
		return Result{}, e
	}
	return s.result(u, sess, raw)
}
func (s *Service) Refresh(ctx context.Context, raw string) (Result, error) {
	if !validToken(raw) {
		return Result{}, store.ErrUnauthorized
	}
	next, e := Random()
	if e != nil {
		return Result{}, e
	}
	u, sess, e := s.Store.Rotate(ctx, Hash(raw), Hash(next), s.Now().Unix())
	if e != nil {
		return Result{}, e
	}
	return s.result(u, sess, next)
}
func (s *Service) Logout(ctx context.Context, raw string) error {
	if !validToken(raw) {
		return nil
	}
	return s.Store.Logout(ctx, Hash(raw), s.Now().Unix())
}
func (s *Service) result(u store.User, sess store.Session, raw string) (Result, error) {
	now := s.Now()
	id, e := Random()
	if e != nil {
		return Result{}, e
	}
	exp := now.Add(AccessLifetime)
	if exp.Unix() > sess.ExpiresAt {
		exp = time.Unix(sess.ExpiresAt, 0)
	}
	c := claims{Use: "access", SessionID: sess.ID, RegisteredClaims: jwt.RegisteredClaims{Issuer: s.issuer, Subject: u.ID, Audience: jwt.ClaimStrings{audience}, ExpiresAt: jwt.NewNumericDate(exp), IssuedAt: jwt.NewNumericDate(now), ID: id}}
	token, e := jwt.NewWithClaims(jwt.SigningMethodHS256, c).SignedString(s.secret)
	if e != nil {
		return Result{}, e
	}
	return Result{AccessToken: token, ExpiresIn: int(exp.Unix() - now.Unix()), User: u, Refresh: raw, ExpiresAt: sess.ExpiresAt}, nil
}
func (s *Service) Authenticate(ctx context.Context, raw string) (store.User, error) {
	if len(raw) > 4096 {
		return store.User{}, store.ErrUnauthorized
	}
	c := new(claims)
	t, e := jwt.ParseWithClaims(raw, c, func(_ *jwt.Token) (any, error) { return s.secret, nil }, jwt.WithValidMethods([]string{"HS256"}), jwt.WithIssuer(s.issuer), jwt.WithAudience(audience), jwt.WithExpirationRequired(), jwt.WithIssuedAt(), jwt.WithTimeFunc(s.Now))
	if e != nil || !t.Valid || c.Use != "access" || c.Subject == "" || c.SessionID == "" || c.ID == "" || c.IssuedAt == nil {
		return store.User{}, store.ErrUnauthorized
	}
	return s.Store.Authenticated(ctx, c.Subject, c.SessionID, s.Now().Unix())
}
