package store

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/i2bskn/authlume/mcpoauth"
	"github.com/jackc/pgx/v5/pgconn"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// MCPOAuth returns the mcpoauth.Store of the MCP authorization server on
// mcp_oauth_codes and mcp_oauth_connections. now is the clock of the sweeps
// of expired rows in CreateCode.
//
// Keys are stored as lowercase hex, scopes as a JSON array and times as Unix
// seconds; a zero AbsoluteExpiry and a zero Code are stored as NULL. Row
// locks are taken in the order users, codes, connections: Create and
// CreateCode lock the subject's user row and refuse a missing or disabled
// user, so that nothing is created after the transaction that disables the
// user and deletes its codes and connections.
func (s *Store) MCPOAuth(now func() time.Time) mcpoauth.Store { return mcpStore{s, now} }

// MCPClients returns the mcpoauth.ClientStore of dynamically registered
// clients on mcp_oauth_registrations. CreateClient sweeps the registrations:
// an activated registration whose connections and codes are all gone gets a
// deadline releasedRegistrationGrace away, and a registration whose deadline
// has passed with nothing left is deleted.
func (s *Store) MCPClients() mcpoauth.ClientStore { return mcpClientStore{s} }

const (
	// activatedRegistrationAge is how old a registration must be before the
	// sweep gives it a deadline again.
	activatedRegistrationAge = time.Hour
	// releasedRegistrationGrace is how long a registration without
	// connections is kept.
	releasedRegistrationGrace = 7 * 24 * time.Hour
)

type mcpStore struct {
	s   *Store
	now func() time.Time
}

type mcpClientStore struct{ s *Store }

func keyText(k mcpoauth.Key) string { return hex.EncodeToString(k[:]) }

func parseKey(v string) (mcpoauth.Key, error) {
	var k mcpoauth.Key
	b, err := hex.DecodeString(v)
	if err != nil || len(b) != len(k) {
		return k, errors.New("store: stored MCP key is malformed")
	}
	copy(k[:], b)
	return k, nil
}

func encodeList(v []string) (string, error) {
	if v == nil {
		v = []string{}
	}
	b, err := json.Marshal(v)
	return string(b), err
}

func decodeList(v string) ([]string, error) {
	out := []string{}
	if err := json.Unmarshal([]byte(v), &out); err != nil {
		return nil, errors.New("store: stored MCP list is malformed")
	}
	return out, nil
}

func nullableUnix(t time.Time) sql.NullInt64 {
	if t.IsZero() {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: t.Unix(), Valid: true}
}

func fromNullableUnix(v sql.NullInt64) time.Time {
	if !v.Valid {
		return time.Time{}
	}
	return time.Unix(v.Int64, 0)
}

// uniqueViolation reports whether err is a unique constraint violation of
// either database.
func uniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505"
	}
	var sqliteErr *sqlite.Error
	if errors.As(err, &sqliteErr) {
		return sqliteErr.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE || sqliteErr.Code() == sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY
	}
	return false
}

func toConnection(row mcpConnection) (mcpoauth.Connection, error) {
	selector, err := parseKey(row.Selector)
	if err != nil {
		return mcpoauth.Connection{}, err
	}
	access, err := parseKey(row.Access)
	if err != nil {
		return mcpoauth.Connection{}, err
	}
	refresh, err := parseKey(row.Refresh)
	if err != nil {
		return mcpoauth.Connection{}, err
	}
	var code mcpoauth.Key
	if row.Code.Valid {
		if code, err = parseKey(row.Code.String); err != nil {
			return mcpoauth.Connection{}, err
		}
	}
	scopes, err := decodeList(row.Scopes)
	if err != nil {
		return mcpoauth.Connection{}, err
	}
	return mcpoauth.Connection{
		ID:             row.ID,
		Selector:       selector,
		Subject:        row.Subject,
		ClientID:       row.ClientID,
		Scopes:         scopes,
		CreatedAt:      time.Unix(row.CreatedAt, 0),
		RefreshedAt:    time.Unix(row.RefreshedAt, 0),
		AbsoluteExpiry: fromNullableUnix(row.AbsoluteExpiry),
		Access:         access,
		AccessExpiry:   time.Unix(row.AccessExpiry, 0),
		Refresh:        refresh,
		RefreshExpiry:  time.Unix(row.RefreshExpiry, 0),
		Code:           code,
	}, nil
}

type approvalSessionKey struct{}

type approvalSession struct {
	id  string
	now int64
}

// WithApprovalSession returns ctx carrying the browser session that
// approves an MCP authorization. CreateCode requires that session to be an
// active session of the subject, in the transaction that locks the user row
// (ErrUnauthorized otherwise), and refuses a context without one.
func WithApprovalSession(ctx context.Context, sessionID string, now time.Time) context.Context {
	return context.WithValue(ctx, approvalSessionKey{}, approvalSession{id: sessionID, now: now.Unix()})
}

// CreateCode inserts the code after deleting the expired ones. A missing or
// disabled subject is ErrUnauthorized, and so is an approval session (see
// WithApprovalSession) that is no longer active. A code is only issued by an
// approval, so a context without an approval session is refused.
func (m mcpStore) CreateCode(ctx context.Context, key mcpoauth.Key, code mcpoauth.Code) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	approval, ok := ctx.Value(approvalSessionKey{}).(approvalSession)
	if !ok || approval.id == "" {
		return errNoGuard
	}
	scopes, err := encodeList(code.Scopes)
	if err != nil {
		return err
	}
	if err := m.s.q.deleteExpiredMCPCodes(ctx, m.now().Unix()); err != nil {
		return err
	}
	return m.s.transaction(ctx, func(q queries) error {
		u, err := lockEnabledUser(ctx, q, code.Subject)
		if err != nil {
			return err
		}
		if err := checkBinding(ctx, q, u, approval.id, "", approval.now); err != nil {
			return err
		}
		n, err := q.insertMCPCode(ctx, mcpCode{
			Key: keyText(key), ClientID: code.ClientID, RedirectUri: code.RedirectURI, Subject: code.Subject,
			Scopes: scopes, Challenge: code.Challenge, ExpiresAt: code.ExpiresAt.Unix(),
		})
		if err != nil {
			return err
		}
		if n == 0 {
			return mcpoauth.ErrCollision
		}
		return nil
	})
}

// TakeCode follows the steps of mcpoauth.Store. The deletion of an expired
// code and the reuse mark are committed before the error is returned: the
// closure returns nil and carries the result out.
func (m mcpStore) TakeCode(ctx context.Context, key mcpoauth.Key, now time.Time) (mcpoauth.Code, mcpoauth.Connection, error) {
	if err := ctx.Err(); err != nil {
		return mcpoauth.Code{}, mcpoauth.Connection{}, err
	}
	k := keyText(key)
	var code mcpoauth.Code
	var conn mcpoauth.Connection
	var result error
	err := m.s.transaction(ctx, func(q queries) error {
		row, err := q.lockMCPCode(ctx, k)
		if errors.Is(err, sql.ErrNoRows) {
			result = mcpoauth.ErrInvalid
			return nil
		}
		if err != nil {
			return err
		}
		if now.Unix() >= row.ExpiresAt {
			result = mcpoauth.ErrInvalid
			return q.deleteMCPCode(ctx, k)
		}
		if row.Consumed != 0 {
			result = mcpoauth.ErrReused
			if err = q.markMCPCodeReused(ctx, k); err != nil {
				return err
			}
			deleted, err := q.deleteMCPConnectionByCode(ctx, k)
			if err != nil {
				return err
			}
			if len(deleted) > 0 {
				conn, err = toConnection(deleted[0])
				return err
			}
			return nil
		}
		if err = q.markMCPCodeConsumed(ctx, k); err != nil {
			return err
		}
		scopes, err := decodeList(row.Scopes)
		if err != nil {
			return err
		}
		code = mcpoauth.Code{
			ClientID: row.ClientID, RedirectURI: row.RedirectUri, Subject: row.Subject,
			Scopes: scopes, Challenge: row.Challenge, ExpiresAt: time.Unix(row.ExpiresAt, 0),
		}
		return nil
	})
	if err != nil {
		return mcpoauth.Code{}, mcpoauth.Connection{}, err
	}
	if result != nil {
		return mcpoauth.Code{}, conn, result
	}
	return code, mcpoauth.Connection{}, nil
}

// Create inserts the connection after deleting the expired ones at
// conn.CreatedAt. A missing or disabled subject is ErrInvalid.
func (m mcpStore) Create(ctx context.Context, conn mcpoauth.Connection) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	scopes, err := encodeList(conn.Scopes)
	if err != nil {
		return err
	}
	if err := m.s.q.deleteExpiredMCPConnections(ctx, conn.CreatedAt.Unix()); err != nil {
		return err
	}
	var code sql.NullString
	if conn.Code != (mcpoauth.Key{}) {
		code = sql.NullString{String: keyText(conn.Code), Valid: true}
	}
	return m.s.transaction(ctx, func(q queries) error {
		if _, err := lockEnabledUser(ctx, q, conn.Subject); errors.Is(err, ErrUnauthorized) {
			return mcpoauth.ErrInvalid
		} else if err != nil {
			return err
		}
		if code.Valid {
			row, err := q.lockMCPCode(ctx, code.String)
			if errors.Is(err, sql.ErrNoRows) {
				return mcpoauth.ErrInvalid
			}
			if err != nil {
				return err
			}
			if row.Consumed == 0 {
				return mcpoauth.ErrInvalid
			}
			if row.Reused != 0 {
				return mcpoauth.ErrReused
			}
		}
		n, err := q.insertMCPConnection(ctx, mcpConnection{
			ID: conn.ID, Selector: keyText(conn.Selector), Subject: conn.Subject, ClientID: conn.ClientID,
			Scopes: scopes, CreatedAt: conn.CreatedAt.Unix(), RefreshedAt: conn.RefreshedAt.Unix(),
			AbsoluteExpiry: nullableUnix(conn.AbsoluteExpiry), Access: keyText(conn.Access),
			AccessExpiry: conn.AccessExpiry.Unix(), Refresh: keyText(conn.Refresh),
			RefreshExpiry: conn.RefreshExpiry.Unix(), Code: code,
		})
		if err != nil {
			return err
		}
		if n == 0 {
			return mcpoauth.ErrCollision
		}
		return nil
	})
}

// Rotate follows the steps of mcpoauth.Store. The deletions of an expired or
// reused connection are committed before the error is returned.
func (m mcpStore) Rotate(ctx context.Context, selector, refresh mcpoauth.Key, clientID string, now time.Time, next mcpoauth.Rotation) (mcpoauth.Connection, error) {
	if err := ctx.Err(); err != nil {
		return mcpoauth.Connection{}, err
	}
	var out mcpoauth.Connection
	var result error
	err := m.s.transaction(ctx, func(q queries) error {
		row, err := q.lockMCPConnectionBySelector(ctx, keyText(selector))
		if errors.Is(err, sql.ErrNoRows) {
			result = mcpoauth.ErrInvalid
			return nil
		}
		if err != nil {
			return err
		}
		conn, err := toConnection(row)
		if err != nil {
			return err
		}
		if conn.ClientID != clientID {
			result = mcpoauth.ErrInvalid
			return nil
		}
		if conn.RefreshExpired(now) {
			result = mcpoauth.ErrInvalid
			return q.deleteMCPConnection(ctx, conn.ID)
		}
		if subtle.ConstantTimeCompare(conn.Refresh[:], refresh[:]) != 1 {
			result = mcpoauth.ErrReused
			out = conn
			return q.deleteMCPConnection(ctx, conn.ID)
		}
		updated, err := q.rotateMCPConnection(ctx, conn.ID, keyText(next.Access), next.AccessExpiry.Unix(), keyText(next.Refresh), next.RefreshExpiry.Unix(), now.Unix())
		if uniqueViolation(err) {
			// Only the access token digest is unique among the changed
			// columns. Nothing has changed once the transaction rolls back.
			return mcpoauth.ErrCollision
		}
		if err != nil {
			return err
		}
		out, err = toConnection(updated)
		return err
	})
	if err != nil {
		return mcpoauth.Connection{}, err
	}
	if result != nil {
		return out, result
	}
	return out, nil
}

// Lookup returns the connection of selector until its refresh deadline.
func (m mcpStore) Lookup(ctx context.Context, selector mcpoauth.Key, now time.Time) (mcpoauth.Connection, error) {
	if err := ctx.Err(); err != nil {
		return mcpoauth.Connection{}, err
	}
	row, err := m.s.q.liveMCPConnectionBySelector(ctx, keyText(selector), now.Unix())
	if errors.Is(err, sql.ErrNoRows) {
		return mcpoauth.Connection{}, mcpoauth.ErrInvalid
	}
	if err != nil {
		return mcpoauth.Connection{}, err
	}
	return toConnection(row)
}

// Use returns the connection of the access token digest until its deadline.
func (m mcpStore) Use(ctx context.Context, access mcpoauth.Key, now time.Time) (mcpoauth.Connection, error) {
	if err := ctx.Err(); err != nil {
		return mcpoauth.Connection{}, err
	}
	row, err := m.s.q.liveMCPConnectionByAccess(ctx, keyText(access), now.Unix())
	if errors.Is(err, sql.ErrNoRows) {
		return mcpoauth.Connection{}, mcpoauth.ErrInvalid
	}
	if err != nil {
		return mcpoauth.Connection{}, err
	}
	return toConnection(row)
}

// Revoke deletes the connection. A missing connection is not an error.
func (m mcpStore) Revoke(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return m.s.q.deleteMCPConnection(ctx, id)
}

func toRegistration(row mcpRegistration) (mcpoauth.Registration, error) {
	uris, err := decodeList(row.RedirectUris)
	if err != nil {
		return mcpoauth.Registration{}, err
	}
	return mcpoauth.Registration{
		ID: row.ID, Name: row.Name, RedirectURIs: uris,
		CreatedAt: time.Unix(row.CreatedAt, 0), ExpiresAt: fromNullableUnix(row.ExpiresAt),
	}, nil
}

// sweepMCPRegistrations deletes the registrations whose deadline passed with
// nothing left and gives a deadline to the activated ones that lost their
// last connection.
func sweepMCPRegistrations(ctx context.Context, q queries, now int64) error {
	if err := q.deleteExpiredMCPRegistrations(ctx, now); err != nil {
		return err
	}
	return q.releaseMCPRegistrations(ctx, now+int64(releasedRegistrationGrace/time.Second), now-int64(activatedRegistrationAge/time.Second))
}

// CreateClient inserts the registration after sweeping the stale ones at
// reg.CreatedAt.
func (c mcpClientStore) CreateClient(ctx context.Context, reg mcpoauth.Registration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	uris, err := encodeList(reg.RedirectURIs)
	if err != nil {
		return err
	}
	now := reg.CreatedAt.Unix()
	if err := sweepMCPRegistrations(ctx, c.s.q, now); err != nil {
		return err
	}
	n, err := c.s.q.insertMCPRegistration(ctx, mcpRegistration{ID: reg.ID, Name: reg.Name, RedirectUris: uris, CreatedAt: now, ExpiresAt: nullableUnix(reg.ExpiresAt)})
	if err != nil {
		return err
	}
	if n == 0 {
		return mcpoauth.ErrCollision
	}
	return nil
}

// Client returns the registration until its deadline.
func (c mcpClientStore) Client(ctx context.Context, id string, now time.Time) (mcpoauth.Registration, error) {
	if err := ctx.Err(); err != nil {
		return mcpoauth.Registration{}, err
	}
	row, err := c.s.q.liveMCPRegistration(ctx, id, now.Unix())
	if errors.Is(err, sql.ErrNoRows) {
		return mcpoauth.Registration{}, mcpoauth.ErrNotFound
	}
	if err != nil {
		return mcpoauth.Registration{}, err
	}
	return toRegistration(row)
}

// Activate clears the deadline of a registration that has not expired.
func (c mcpClientStore) Activate(ctx context.Context, id string, now time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	n, err := c.s.q.activateMCPRegistration(ctx, id, now.Unix())
	if err != nil {
		return err
	}
	if n == 0 {
		return mcpoauth.ErrNotFound
	}
	return nil
}

// MCPConnection is a live MCP connection as listed to its user.
type MCPConnection struct {
	ID       string
	ClientID string
	// ClientName and ClientHost are the label shown on the latest consent
	// screen of the client, empty when there is none.
	ClientName string
	ClientHost string
	Scopes     []string
	CreatedAt  int64
	LastUsedAt sql.NullInt64
	// ExpiresAt is the refresh deadline, or the absolute one when earlier.
	ExpiresAt int64
}

// ListMCPConnections returns the live MCP connections of the user, newest
// first.
func (s *Store) ListMCPConnections(ctx context.Context, userID string, now time.Time) ([]MCPConnection, error) {
	rows, err := s.q.listLiveMCPConnections(ctx, userID, now.Unix())
	if err != nil {
		return nil, err
	}
	out := make([]MCPConnection, 0, len(rows))
	for _, r := range rows {
		scopes, err := decodeList(r.Scopes)
		if err != nil {
			return nil, err
		}
		expires := r.RefreshExpiry
		if r.AbsoluteExpiry.Valid && r.AbsoluteExpiry.Int64 < expires {
			expires = r.AbsoluteExpiry.Int64
		}
		out = append(out, MCPConnection{
			ID: r.ID, ClientID: r.ClientID, ClientName: r.ClientName, ClientHost: r.ClientHost,
			Scopes: scopes, CreatedAt: r.CreatedAt, LastUsedAt: r.LastUsedAt, ExpiresAt: expires,
		})
	}
	return out, nil
}

func validText(v string) bool { return utf8.ValidString(v) && !strings.ContainsRune(v, 0) }

// DeleteMCPConnection deletes the user's connection id, which invalidates
// its access and refresh tokens. Another user's or a missing connection is
// ErrNotFound.
func (s *Store) DeleteMCPConnection(ctx context.Context, userID, id string) error {
	if !validText(id) {
		return ErrNotFound
	}
	n, err := s.q.deleteSubjectMCPConnection(ctx, id, userID)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// mcpTouchInterval is how often last_used_at of a connection is updated.
const mcpTouchInterval = time.Minute

// TouchMCPConnection records that the connection was used at now. It writes
// at most once per minute per connection.
func (s *Store) TouchMCPConnection(ctx context.Context, id string, now time.Time) error {
	return s.q.touchMCPConnection(ctx, id, now.Unix(), now.Add(-mcpTouchInterval).Unix())
}

// SetMCPClientLabel records the name and host shown for clientID on a
// consent screen, for the connection list.
func (s *Store) SetMCPClientLabel(ctx context.Context, clientID, name, host string, now time.Time) error {
	if !validText(clientID) || !validText(name) || !validText(host) {
		return errors.New("client label must be valid UTF-8 without NUL characters")
	}
	return s.q.upsertMCPClientLabel(ctx, clientID, name, host, now.Unix())
}

// RevokeMCPClient deletes every code and connection of clientID, for a
// client whose registration no longer matches what was approved. It
// returns the number of connections deleted.
func (s *Store) RevokeMCPClient(ctx context.Context, clientID string) (int64, error) {
	var n int64
	err := s.transaction(ctx, func(q queries) error {
		if err := q.deleteClientMCPCodes(ctx, clientID); err != nil {
			return err
		}
		var err error
		n, err = q.deleteClientMCPConnections(ctx, clientID)
		return err
	})
	return n, err
}

// revokeMCPSubject deletes every code, consumed ones included, and every
// connection of the user, in the caller's transaction after its user row is
// locked.
func revokeMCPSubject(ctx context.Context, q queries, userID string) error {
	if err := q.deleteSubjectMCPCodes(ctx, userID); err != nil {
		return err
	}
	return q.deleteSubjectMCPConnections(ctx, userID)
}

// MCPClientInUse reports whether a code record or a connection names
// clientID. The token and revocation endpoints use it to refuse a client_id
// that no authorization was ever made for without resolving it.
func (s *Store) MCPClientInUse(ctx context.Context, clientID string) (bool, error) {
	if !validText(clientID) {
		return false, nil
	}
	n, err := s.q.countClientMCPCodes(ctx, clientID)
	if err != nil || n > 0 {
		return n > 0, err
	}
	n, err = s.q.countClientMCPConnections(ctx, clientID)
	return n > 0, err
}
