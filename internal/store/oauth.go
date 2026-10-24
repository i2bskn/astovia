package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

var ErrClientNotFound = errors.New("OAuth client not found")
var ErrGrantNotFound = errors.New("OAuth grant not found")

// ErrCodeReused is returned by RedeemOAuthCode when the code was already
// exchanged. The grant and its tokens have been deleted by then.
var ErrCodeReused = errors.New("authorization code was already used")

// OAuth token kinds stored in oauth_tokens.kind.
const (
	OAuthAccess  = "access"
	OAuthRefresh = "refresh"
)

// OAuthClient is a client registered by DCR or cached from its Client ID
// Metadata Document. RedirectURIs is stored as a JSON array.
type OAuthClient struct {
	ClientID, Kind, Name string
	RedirectURIs         []string
	AuthMethod           string
	SecretHash           sql.NullString
	MetadataExpiresAt    sql.NullInt64
	CreatedAt, UpdatedAt int64
}

// OAuthGrant is one consent: it holds the authorization code and, once the
// code is exchanged, groups the tokens issued from it. ExpiresAt is the code
// expiry until the exchange and afterwards the latest expiry of its tokens,
// so an expired grant never has a usable token.
type OAuthGrant struct {
	ID, UserID, ClientID, ClientName string
	Scope, Resource, RedirectURI     string
	CodeHash, CodeChallenge          string
	CodeExpiresAt                    int64
	CodeUsedAt                       sql.NullInt64
	CreatedAt, ExpiresAt             int64
	LastUsedAt                       sql.NullInt64
}

// OAuthToken is an access or refresh token. Only the hash is stored.
type OAuthToken struct {
	Hash, Kind, GrantID, UserID string
	CreatedAt, ExpiresAt        int64
}

func encodeRedirectURIs(uris []string) (string, error) {
	if uris == nil {
		uris = []string{}
	}
	b, err := json.Marshal(uris)
	return string(b), err
}

func decodeRedirectURIs(raw string) ([]string, error) {
	var uris []string
	if err := json.Unmarshal([]byte(raw), &uris); err != nil {
		return nil, err
	}
	return uris, nil
}

// OAuthClient returns a registered client, or ErrClientNotFound.
func (s *Store) OAuthClient(ctx context.Context, clientID string) (OAuthClient, error) {
	c, err := s.q.oauthClient(ctx, clientID)
	if errors.Is(err, sql.ErrNoRows) {
		return OAuthClient{}, ErrClientNotFound
	}
	return c, err
}

// CreateOAuthClient stores a DCR client. secretHash is empty for public
// clients (auth method none) and is stored as NULL; c.SecretHash is ignored.
func (s *Store) CreateOAuthClient(ctx context.Context, c OAuthClient, secretHash string) error {
	return s.q.createOAuthClient(ctx, c, sql.NullString{String: secretHash, Valid: secretHash != ""})
}

// UpsertCIMDClient stores or refreshes the cached metadata of a CIMD client.
// The creation time of an existing row is kept.
func (s *Store) UpsertCIMDClient(ctx context.Context, c OAuthClient) error {
	return s.q.upsertCIMDClient(ctx, c)
}

// CreateOAuthGrant stores a consent with its authorization code. As in
// CreateAPIToken, the session of the access token that approved it is
// checked again after locking the user row, so a concurrent password reset or
// disable cannot leave a grant behind. Expired tokens and grants of the user
// are removed in the same transaction.
func (s *Store) CreateOAuthGrant(ctx context.Context, g OAuthGrant, sessionID string, now int64) error {
	return s.transaction(ctx, func(q queries) error {
		// Lock the user row first; see CreateAPIToken. Do not change this order.
		u, e := q.lockUser(ctx, g.UserID)
		if errors.Is(e, sql.ErrNoRows) {
			return ErrUnauthorized
		}
		if e != nil {
			return e
		}
		if u.Disabled != 0 {
			return ErrUnauthorized
		}
		if _, e = q.activeSession(ctx, sessionID, g.UserID, now); e != nil {
			if errors.Is(e, sql.ErrNoRows) {
				return ErrUnauthorized
			}
			return e
		}
		if _, e = q.oauthClient(ctx, g.ClientID); e != nil {
			if errors.Is(e, sql.ErrNoRows) {
				return ErrClientNotFound
			}
			return e
		}
		// Tokens first: they reference the grants.
		if e = q.deleteExpiredOAuthTokens(ctx, g.UserID, now); e != nil {
			return e
		}
		if e = q.deleteExpiredOAuthGrants(ctx, g.UserID, now); e != nil {
			return e
		}
		return q.createOAuthGrant(ctx, g)
	})
}

// RedeemOAuthCode exchanges the authorization code whose hash is codeHash.
// issue validates the grant against the token request (client, redirect URI,
// PKCE) and returns the tokens to store; its error is returned unchanged and
// nothing is written. issue runs inside the transaction and must not use the
// database. When the code was already exchanged and issue accepts the
// request, the grant and all of its tokens are deleted and ErrCodeReused is
// returned. GrantID and UserID of the tokens are set from the grant.
func (s *Store) RedeemOAuthCode(ctx context.Context, codeHash string, now int64, issue func(OAuthGrant) ([]OAuthToken, error)) (OAuthGrant, error) {
	// Read outside the transaction only to learn user_id, so that the user row
	// can be locked first as in Rotate.
	g, err := s.q.oauthGrantByCode(ctx, codeHash)
	if errors.Is(err, sql.ErrNoRows) {
		return OAuthGrant{}, ErrUnauthorized
	}
	if err != nil {
		return OAuthGrant{}, err
	}
	var reused bool
	err = s.transaction(ctx, func(q queries) error {
		u, e := q.lockUser(ctx, g.UserID)
		if errors.Is(e, sql.ErrNoRows) {
			return ErrUnauthorized
		}
		if e != nil {
			return e
		}
		g, e = q.lockOAuthGrantByCode(ctx, codeHash, u.ID)
		if errors.Is(e, sql.ErrNoRows) {
			return ErrUnauthorized
		}
		if e != nil {
			return e
		}
		tokens, e := issue(g)
		if e != nil {
			return e
		}
		if g.CodeUsedAt.Valid {
			// The deletion must be committed, so the transaction succeeds and
			// the caller turns reused into ErrCodeReused.
			reused = true
			if e = q.deleteGrantOAuthTokens(ctx, g.UserID, g.ID); e != nil {
				return e
			}
			_, e = q.deleteOAuthGrant(ctx, g.UserID, g.ID)
			return e
		}
		if g.CodeExpiresAt <= now || u.Disabled != 0 {
			return ErrUnauthorized
		}
		if e = q.deleteExpiredOAuthTokens(ctx, g.UserID, now); e != nil {
			return e
		}
		n, e := q.markOAuthCodeUsed(ctx, g.UserID, g.ID, now)
		if e != nil {
			return e
		}
		if n != 1 {
			return ErrUnauthorized
		}
		g.CodeUsedAt = sql.NullInt64{Int64: now, Valid: true}
		g.ExpiresAt, e = storeOAuthTokens(ctx, q, g, tokens)
		return e
	})
	if err != nil {
		return OAuthGrant{}, err
	}
	if reused {
		return OAuthGrant{}, ErrCodeReused
	}
	return g, nil
}

// RotateOAuthRefresh replaces the refresh token whose hash is tokenHash with
// the tokens returned by issue. issue has the same contract as in
// RedeemOAuthCode. A refresh token that was already rotated no longer exists
// and is rejected with ErrUnauthorized; the grant is kept.
func (s *Store) RotateOAuthRefresh(ctx context.Context, tokenHash string, now int64, issue func(OAuthGrant) ([]OAuthToken, error)) (OAuthGrant, error) {
	t, err := s.q.oauthTokenByHash(ctx, tokenHash)
	if errors.Is(err, sql.ErrNoRows) {
		return OAuthGrant{}, ErrUnauthorized
	}
	if err != nil {
		return OAuthGrant{}, err
	}
	if t.Kind != OAuthRefresh {
		return OAuthGrant{}, ErrUnauthorized
	}
	var g OAuthGrant
	err = s.transaction(ctx, func(q queries) error {
		u, e := q.lockUser(ctx, t.UserID)
		if errors.Is(e, sql.ErrNoRows) {
			return ErrUnauthorized
		}
		if e != nil {
			return e
		}
		t, e = q.lockOAuthTokenByHash(ctx, tokenHash, u.ID)
		if errors.Is(e, sql.ErrNoRows) {
			return ErrUnauthorized
		}
		if e != nil {
			return e
		}
		if t.Kind != OAuthRefresh || t.ExpiresAt <= now {
			return ErrUnauthorized
		}
		g, e = q.oauthGrant(ctx, u.ID, t.GrantID)
		if errors.Is(e, sql.ErrNoRows) {
			return ErrUnauthorized
		}
		if e != nil {
			return e
		}
		if !g.CodeUsedAt.Valid || u.Disabled != 0 {
			return ErrUnauthorized
		}
		tokens, e := issue(g)
		if e != nil {
			return e
		}
		n, e := q.deleteOAuthToken(ctx, u.ID, tokenHash)
		if e != nil {
			return e
		}
		if n != 1 {
			return ErrUnauthorized
		}
		if e = q.deleteExpiredOAuthTokens(ctx, u.ID, now); e != nil {
			return e
		}
		g.ExpiresAt, e = storeOAuthTokens(ctx, q, g, tokens)
		return e
	})
	if err != nil {
		return OAuthGrant{}, err
	}
	return g, nil
}

// storeOAuthTokens inserts tokens for g and extends the grant to the latest
// token expiry, keeping the invariant that a grant outlives its tokens. It
// returns the new grant expiry.
func storeOAuthTokens(ctx context.Context, q queries, g OAuthGrant, tokens []OAuthToken) (int64, error) {
	expires := g.ExpiresAt
	for _, t := range tokens {
		t.GrantID, t.UserID = g.ID, g.UserID
		if err := q.createOAuthToken(ctx, t); err != nil {
			return 0, err
		}
		expires = max(expires, t.ExpiresAt)
	}
	return expires, q.extendOAuthGrant(ctx, g.UserID, g.ID, expires)
}

// AuthenticateOAuthAccess returns the enabled user and the grant of the
// access token whose hash is hash, or ErrUnauthorized. Like
// AuthenticateAPIToken it records the time of use on the grant at most once a
// minute and returns a failure to do so as touchErr.
func (s *Store) AuthenticateOAuthAccess(ctx context.Context, hash string, now int64) (u User, g OAuthGrant, touchErr error, err error) {
	t, err := s.q.oauthTokenByHash(ctx, hash)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, OAuthGrant{}, nil, ErrUnauthorized
	}
	if err != nil {
		return User{}, OAuthGrant{}, nil, err
	}
	if t.Kind != OAuthAccess || t.ExpiresAt <= now {
		return User{}, OAuthGrant{}, nil, ErrUnauthorized
	}
	g, err = s.q.oauthGrant(ctx, t.UserID, t.GrantID)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, OAuthGrant{}, nil, ErrUnauthorized
	}
	if err != nil {
		return User{}, OAuthGrant{}, nil, err
	}
	if !g.CodeUsedAt.Valid || g.ExpiresAt <= now {
		return User{}, OAuthGrant{}, nil, ErrUnauthorized
	}
	u, err = s.q.userByID(ctx, t.UserID)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, OAuthGrant{}, nil, ErrUnauthorized
	}
	if err != nil {
		return User{}, OAuthGrant{}, nil, err
	}
	if u.Disabled != 0 {
		return User{}, OAuthGrant{}, nil, ErrUnauthorized
	}
	return u, g, s.q.touchOAuthGrant(ctx, u.ID, g.ID, now, now-60), nil
}

// ListOAuthGrants returns the user's exchanged, unexpired grants, newest
// first. It never returns nil.
func (s *Store) ListOAuthGrants(ctx context.Context, userID string, now int64) ([]OAuthGrant, error) {
	return s.q.listOAuthGrants(ctx, userID, now)
}

// DeleteOAuthGrant deletes the user's grant and its tokens, or returns
// ErrGrantNotFound when it does not exist or belongs to another user.
func (s *Store) DeleteOAuthGrant(ctx context.Context, userID, id string) error {
	return s.transaction(ctx, func(q queries) error {
		if _, e := q.lockUser(ctx, userID); e != nil {
			if errors.Is(e, sql.ErrNoRows) {
				return ErrGrantNotFound
			}
			return e
		}
		if e := q.deleteGrantOAuthTokens(ctx, userID, id); e != nil {
			return e
		}
		n, e := q.deleteOAuthGrant(ctx, userID, id)
		if e != nil {
			return e
		}
		if n == 0 {
			return ErrGrantNotFound
		}
		return nil
	})
}
