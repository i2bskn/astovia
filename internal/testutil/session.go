package testutil

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"testing"
	"time"

	"github.com/i2bskn/astovia/internal/store"
	"github.com/i2bskn/authlume/session"
)

// SessionCookieName is the name of the browser session cookie.
const SessionCookieName = "__Host-astovia_session"

// Session stores a browser session for userID that starts at now and lasts
// for the product's idle (7 days) and absolute (30 days) lifetimes. It
// returns the cookie value and the session ID (the hex of the stored key).
func Session(t testing.TB, s *store.Store, userID string, now time.Time) (cookie, id string) {
	t.Helper()
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		t.Fatal(err)
	}
	key := session.Key(sha256.Sum256(raw[:]))
	state := session.State{Key: key, Subject: userID, AuthenticatedAt: now, LastActivity: now, AbsoluteExpiry: now.Add(30 * 24 * time.Hour), Idle: 7 * 24 * time.Hour}
	u, err := s.UserByID(context.Background(), userID)
	if err != nil {
		t.Fatal(err)
	}
	ctx := store.WithIssueGuard(context.Background(), store.IssueGuard{PasswordDigest: store.Digest(u.PasswordHash)})
	if err := s.Sessions().Replace(ctx, nil, state); err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), hex.EncodeToString(key[:])
}
