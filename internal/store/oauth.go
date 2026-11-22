package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

var ErrClientNotFound = errors.New("OAuth client not found")
var ErrGrantNotFound = errors.New("OAuth grant not found")

// ErrCodeReused is returned by RedeemOAuthCode when the code was already
// exchanged. The grant and its tokens have been deleted by then.
var ErrCodeReused = errors.New("authorization code was already used")

// ErrRefreshReused is returned by RotateOAuthRefresh when the refresh token
// was already exchanged. The grant and its tokens have been deleted by then.
var ErrRefreshReused = errors.New("refresh token was already used")

// OAuth token kinds stored in oauth_tokens.kind. OAuthRefreshUsed is a
// refresh token that was exchanged; it is kept until it expires so that a
// second use can be detected, and authenticates nothing.
const (
	OAuthAccess      = "access"
	OAuthRefresh     = "refresh"
	OAuthRefreshUsed = "refresh_used"
)

// OAuthGrantMaxLifetime is the longest a grant and its tokens can live,
// counted from the consent (OAuthGrant.CreatedAt). Refreshing does not
// extend it.
const OAuthGrantMaxLifetime = 180 * 24 * time.Hour

// OAuthGrantCap returns the time (Unix seconds) after which no token of g
// may be valid.
func OAuthGrantCap(g OAuthGrant) int64 {
	return g.CreatedAt + int64(OAuthGrantMaxLifetime/time.Second)
}

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
// so an expired grant never has a usable token. No token outlives
// OAuthGrantCap.
type OAuthGrant struct {
	ID, UserID, ClientID, ClientName string
	Scope, Resource, RedirectURI     string
	CodeHash, CodeChallenge          string
	CodeExpiresAt                    int64
	CodeUsedAt                       sql.NullInt64
	CreatedAt, ExpiresAt             int64
	LastUsedAt                       sql.NullInt64
}

// OAuthToken is an access token, a refresh token or an exchanged refresh
// token (Kind OAuthRefreshUsed). Only the hash is stored. Its ExpiresAt is
// never later than the ExpiresAt of its grant.
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
// RedeemOAuthCode. The exchanged token is kept as OAuthRefreshUsed until it
// expires. When such a token is presented again before it expires and issue
// accepts the request, the grant and all of its tokens are deleted and
// ErrRefreshReused is returned with the grant that was deleted.
func (s *Store) RotateOAuthRefresh(ctx context.Context, tokenHash string, now int64, issue func(OAuthGrant) ([]OAuthToken, error)) (OAuthGrant, error) {
	t, err := s.q.oauthTokenByHash(ctx, tokenHash)
	if errors.Is(err, sql.ErrNoRows) {
		return OAuthGrant{}, ErrUnauthorized
	}
	if err != nil {
		return OAuthGrant{}, err
	}
	if t.Kind != OAuthRefresh && t.Kind != OAuthRefreshUsed {
		return OAuthGrant{}, ErrUnauthorized
	}
	var g OAuthGrant
	var reused bool
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
		if (t.Kind != OAuthRefresh && t.Kind != OAuthRefreshUsed) || t.ExpiresAt <= now {
			return ErrUnauthorized
		}
		g, e = q.oauthGrant(ctx, u.ID, t.GrantID)
		if errors.Is(e, sql.ErrNoRows) {
			return ErrUnauthorized
		}
		if e != nil {
			return e
		}
		if t.Kind == OAuthRefreshUsed {
			// As in RedeemOAuthCode, issue must accept the request first, so
			// that another client presenting the token cannot revoke the
			// grant. The deletion must be committed, so the transaction
			// succeeds and the caller turns reused into ErrRefreshReused.
			if _, e = issue(g); e != nil {
				return e
			}
			reused = true
			if e = q.deleteGrantOAuthTokens(ctx, g.UserID, g.ID); e != nil {
				return e
			}
			_, e = q.deleteOAuthGrant(ctx, g.UserID, g.ID)
			return e
		}
		if !g.CodeUsedAt.Valid || u.Disabled != 0 {
			return ErrUnauthorized
		}
		tokens, e := issue(g)
		if e != nil {
			return e
		}
		// Mark the token used before storeOAuthTokens, which caps its expiry
		// with the other tokens of the grant.
		n, e := q.markOAuthRefreshUsed(ctx, u.ID, tokenHash)
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
	if reused {
		return g, ErrRefreshReused
	}
	return g, nil
}

// storeOAuthTokens inserts tokens for g and extends the grant to the latest
// token expiry, keeping the invariant that a grant outlives its tokens. No
// token and not the grant outlives OAuthGrantCap: the new tokens are capped,
// the existing tokens of the grant (including exchanged refresh tokens) are
// cut down to it, and so is the grant. It returns the new grant expiry.
func storeOAuthTokens(ctx context.Context, q queries, g OAuthGrant, tokens []OAuthToken) (int64, error) {
	limit := OAuthGrantCap(g)
	expires := g.ExpiresAt
	for _, t := range tokens {
		t.GrantID, t.UserID = g.ID, g.UserID
		t.ExpiresAt = min(t.ExpiresAt, limit)
		if err := q.createOAuthToken(ctx, t); err != nil {
			return 0, err
		}
		expires = max(expires, t.ExpiresAt)
	}
	if err := q.capGrantOAuthTokens(ctx, g.UserID, g.ID, limit); err != nil {
		return 0, err
	}
	expires = min(expires, limit)
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
