package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/i2bskn/authlume/oidc"
	"github.com/i2bskn/authlume/session"
	"github.com/i2bskn/authlume/webauthn"
)

// ErrOIDCNotApproved means that a user with the email of a new OIDC identity
// exists but the operator has not approved linking it (users approve-oidc).
var ErrOIDCNotApproved = errors.New("linking the user to an OIDC identity is not approved")

// OIDCLinkApproval is how long the operator's approval to link a user to
// its first OIDC identity lasts.
const OIDCLinkApproval = 7 * 24 * time.Hour

// WebSession is a row of web_sessions, the browser sessions of
// authlume/session. ID is the lowercase hex of the session key.
type WebSession struct {
	ID              string
	UserID          string
	AuthenticatedAt int64
	LastActivity    int64
	AbsoluteExpiry  int64
	IdleSeconds     int64
}

type oidcLogin struct {
	Key           string
	Nonce         string
	Verifier      string
	StartedAt     int64
	ExpiresAt     int64
	MaxAgeSeconds sql.NullInt64
	AppData       sql.NullString
}

type webauthnCeremony struct {
	Key       string
	Subject   string
	Record    string
	ExpiresAt int64
}

type mcpCode struct {
	Key         string
	ClientID    string
	RedirectUri string
	Subject     string
	Scopes      string
	Challenge   string
	ExpiresAt   int64
	Consumed    int64
	Reused      int64
}

type mcpConnection struct {
	ID             string
	Selector       string
	Subject        string
	ClientID       string
	Scopes         string
	CreatedAt      int64
	RefreshedAt    int64
	AbsoluteExpiry sql.NullInt64
	Access         string
	AccessExpiry   int64
	Refresh        string
	RefreshExpiry  int64
	Code           sql.NullString
	LastUsedAt     sql.NullInt64
}

type mcpConnectionListRow struct {
	ID             string
	ClientID       string
	Scopes         string
	CreatedAt      int64
	RefreshExpiry  int64
	AbsoluteExpiry sql.NullInt64
	LastUsedAt     sql.NullInt64
	ClientName     string
	ClientHost     string
}

type mcpRegistration struct {
	ID           string
	Name         string
	RedirectUris string
	CreatedAt    int64
	ExpiresAt    sql.NullInt64
}

// authQueries are the queries of the authlume stores.
type authQueries interface {
	setOIDCLinkApproval(ctx context.Context, id string, until sql.NullInt64) error
	listOIDCLinkedUserIDs(ctx context.Context) ([]string, error)
	insertWebSession(ctx context.Context, w WebSession) (int64, error)
	liveWebSession(ctx context.Context, id string, now int64) (WebSession, error)
	touchLiveWebSession(ctx context.Context, id string, now int64) (WebSession, error)
	deleteWebSession(ctx context.Context, id string) error
	deleteExpiredWebSessions(ctx context.Context, now int64) error
	deleteExpiredOIDCLogin(ctx context.Context, key string, now int64) error
	deleteExpiredOIDCLogins(ctx context.Context, now int64) error
	insertOIDCLogin(ctx context.Context, l oidcLogin) (int64, error)
	takeOIDCLogin(ctx context.Context, key string) (oidcLogin, error)
	deleteExpiredWebAuthnCeremony(ctx context.Context, key string, now int64) error
	deleteExpiredWebAuthnCeremonies(ctx context.Context, now int64) error
	insertWebAuthnCeremony(ctx context.Context, c webauthnCeremony) (int64, error)
	takeWebAuthnCeremony(ctx context.Context, key string) (webauthnCeremony, error)
	deleteSubjectWebAuthnCeremonies(ctx context.Context, subject string) error
	deleteExpiredMCPCodes(ctx context.Context, now int64) error
	insertMCPCode(ctx context.Context, c mcpCode) (int64, error)
	lockMCPCode(ctx context.Context, key string) (mcpCode, error)
	deleteMCPCode(ctx context.Context, key string) error
	markMCPCodeConsumed(ctx context.Context, key string) error
	markMCPCodeReused(ctx context.Context, key string) error
	deleteMCPConnectionByCode(ctx context.Context, code string) ([]mcpConnection, error)
	deleteExpiredMCPConnections(ctx context.Context, now int64) error
	insertMCPConnection(ctx context.Context, c mcpConnection) (int64, error)
	lockMCPConnectionBySelector(ctx context.Context, selector string) (mcpConnection, error)
	rotateMCPConnection(ctx context.Context, id, access string, accessExpiry int64, refresh string, refreshExpiry, refreshedAt int64) (mcpConnection, error)
	liveMCPConnectionBySelector(ctx context.Context, selector string, now int64) (mcpConnection, error)
	liveMCPConnectionByAccess(ctx context.Context, access string, now int64) (mcpConnection, error)
	deleteMCPConnection(ctx context.Context, id string) error
	listLiveMCPConnections(ctx context.Context, subject string, now int64) ([]mcpConnectionListRow, error)
	deleteSubjectMCPConnection(ctx context.Context, id, subject string) (int64, error)
	touchMCPConnection(ctx context.Context, id string, now, before int64) error
	deleteSubjectMCPCodes(ctx context.Context, subject string) error
	deleteSubjectMCPConnections(ctx context.Context, subject string) error
	deleteClientMCPCodes(ctx context.Context, clientID string) error
	deleteClientMCPConnections(ctx context.Context, clientID string) (int64, error)
	countClientMCPCodes(ctx context.Context, clientID string) (int64, error)
	countClientMCPConnections(ctx context.Context, clientID string) (int64, error)
	upsertMCPClientLabel(ctx context.Context, clientID, name, host string, now int64) error
	releaseMCPRegistrations(ctx context.Context, releaseAt, activatedBefore int64) error
	deleteExpiredMCPRegistrations(ctx context.Context, now int64) error
	insertMCPRegistration(ctx context.Context, r mcpRegistration) (int64, error)
	liveMCPRegistration(ctx context.Context, id string, now int64) (mcpRegistration, error)
	activateMCPRegistration(ctx context.Context, id string, now int64) (int64, error)
}

// digest is the lowercase hex SHA-256 of v, the same as auth.Hash.
func digest(v string) string {
	h := sha256.Sum256([]byte(v))
	return hex.EncodeToString(h[:])
}

// newRandomID returns 32 random bytes in unpadded base64url, the form of
// auth.Random.
func newRandomID() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// revokeAccess removes every way into the account that a session could have
// left behind: the browser sessions, the API tokens, the MCP codes and
// connections and the pending WebAuthn ceremonies. The caller holds the lock
// of the user row.
func revokeAccess(ctx context.Context, q queries, userID string) error {
	if err := q.revokeUserSessions(ctx, userID); err != nil {
		return err
	}
	// API tokens have no expiry, so a token issued from a stolen session
	// must not survive the recovery of the account. MCP connections are
	// granted from a session as well and go with it.
	if err := q.deleteUserAPITokens(ctx, userID); err != nil {
		return err
	}
	if err := revokeMCPSubject(ctx, q, userID); err != nil {
		return err
	}
	return q.deleteSubjectWebAuthnCeremonies(ctx, userID)
}

// IssueGuard is checked by the session store when it issues a session, in
// the transaction that locks the user row. A zero field is not checked.
type IssueGuard struct {
	// PasswordDigest is auth.Hash of the password hash that was verified
	// (or stored) before the session is issued. A different current hash
	// means that the password was changed meanwhile.
	PasswordDigest string
	// OIDCIssuer and OIDCSubject are the identity the user signed in with.
	// The user must still be linked to it.
	OIDCIssuer, OIDCSubject string
}

type issueGuardKey struct{}

// WithIssueGuard returns ctx carrying g for the session store's Replace.
func WithIssueGuard(ctx context.Context, g IssueGuard) context.Context {
	return context.WithValue(ctx, issueGuardKey{}, g)
}

// Sessions returns the session.Store of authlume/session on web_sessions.
// Replace locks the user row of the new session and refuses a missing or
// disabled user (ErrUnauthorized), and checks the IssueGuard of the context
// in the same transaction.
func (s *Store) Sessions() session.Store { return sessionStore{s} }

type sessionStore struct{ s *Store }

func sessionID(k session.Key) string { return hex.EncodeToString(k[:]) }

func (w sessionStore) Replace(ctx context.Context, previous *session.Key, next session.State) error {
	guard, _ := ctx.Value(issueGuardKey{}).(IssueGuard)
	return w.s.transaction(ctx, func(q queries) error {
		u, err := q.lockUser(ctx, next.Subject)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrUnauthorized
		}
		if err != nil {
			return err
		}
		if u.Disabled != 0 {
			return ErrUnauthorized
		}
		if guard.PasswordDigest != "" && subtle.ConstantTimeCompare([]byte(digest(u.PasswordHash)), []byte(guard.PasswordDigest)) != 1 {
			return ErrUnauthorized
		}
		if guard.OIDCIssuer != "" || guard.OIDCSubject != "" {
			if u.OidcIssuer.String != guard.OIDCIssuer || u.OidcSubject.String != guard.OIDCSubject || !u.OidcIssuer.Valid || !u.OidcSubject.Valid {
				return ErrUnauthorized
			}
		}
		n, err := q.insertWebSession(ctx, WebSession{
			ID:              sessionID(next.Key),
			UserID:          next.Subject,
			AuthenticatedAt: next.AuthenticatedAt.Unix(),
			LastActivity:    next.LastActivity.Unix(),
			AbsoluteExpiry:  next.AbsoluteExpiry.Unix(),
			IdleSeconds:     int64(next.Idle / time.Second),
		})
		if err != nil {
			return err
		}
		if n == 0 {
			return session.ErrCollision
		}
		if previous != nil {
			return q.deleteWebSession(ctx, sessionID(*previous))
		}
		return nil
	})
}

func (w sessionStore) Use(ctx context.Context, key session.Key, now time.Time, touch bool) (session.State, error) {
	var row WebSession
	var err error
	if touch {
		row, err = w.s.q.touchLiveWebSession(ctx, sessionID(key), now.Unix())
	} else {
		row, err = w.s.q.liveWebSession(ctx, sessionID(key), now.Unix())
	}
	if errors.Is(err, sql.ErrNoRows) {
		return session.State{}, session.ErrInvalid
	}
	if err != nil {
		return session.State{}, err
	}
	return session.State{
		Key:             key,
		Subject:         row.UserID,
		AuthenticatedAt: time.Unix(row.AuthenticatedAt, 0),
		LastActivity:    time.Unix(row.LastActivity, 0),
		AbsoluteExpiry:  time.Unix(row.AbsoluteExpiry, 0),
		Idle:            time.Duration(row.IdleSeconds) * time.Second,
	}, nil
}

func (w sessionStore) Delete(ctx context.Context, key session.Key) error {
	return w.s.q.deleteWebSession(ctx, sessionID(key))
}

// OIDCLogins returns the oidc.Store of authlume/oidc on oidc_logins.
func (s *Store) OIDCLogins() oidc.Store { return oidcLoginStore{s} }

type oidcLoginStore struct{ s *Store }

func (o oidcLoginStore) Create(ctx context.Context, key oidc.Key, tx oidc.Transaction) error {
	k := hex.EncodeToString(key[:])
	row := oidcLogin{Key: k, Nonce: tx.Nonce, Verifier: tx.Verifier, StartedAt: tx.StartedAt.Unix(), ExpiresAt: tx.ExpiresAt.Unix()}
	if tx.MaxAgeSeconds != nil {
		row.MaxAgeSeconds = sql.NullInt64{Int64: *tx.MaxAgeSeconds, Valid: true}
	}
	if len(tx.AppData) > 0 {
		row.AppData = sql.NullString{String: base64.StdEncoding.EncodeToString(tx.AppData), Valid: true}
	}
	return o.s.transaction(ctx, func(q queries) error {
		if err := q.deleteExpiredOIDCLogin(ctx, k, time.Now().Unix()); err != nil {
			return err
		}
		n, err := q.insertOIDCLogin(ctx, row)
		if err != nil {
			return err
		}
		if n == 0 {
			return errors.New("store: OIDC login key collision")
		}
		return nil
	})
}

func (o oidcLoginStore) Take(ctx context.Context, key oidc.Key, now time.Time) (oidc.Transaction, error) {
	row, err := o.s.q.takeOIDCLogin(ctx, hex.EncodeToString(key[:]))
	if errors.Is(err, sql.ErrNoRows) {
		return oidc.Transaction{}, oidc.ErrInvalid
	}
	if err != nil {
		return oidc.Transaction{}, err
	}
	if now.Unix() >= row.ExpiresAt {
		return oidc.Transaction{}, oidc.ErrInvalid
	}
	tx := oidc.Transaction{Nonce: row.Nonce, Verifier: row.Verifier, StartedAt: time.Unix(row.StartedAt, 0), ExpiresAt: time.Unix(row.ExpiresAt, 0)}
	if row.MaxAgeSeconds.Valid {
		v := row.MaxAgeSeconds.Int64
		tx.MaxAgeSeconds = &v
	}
	if row.AppData.Valid {
		if tx.AppData, err = base64.StdEncoding.DecodeString(row.AppData.String); err != nil {
			return oidc.Transaction{}, fmt.Errorf("store: stored OIDC app data: %w", err)
		}
	}
	return tx, nil
}

// WebAuthnCeremonies returns the webauthn.CeremonyStore of authlume/webauthn
// on webauthn_ceremonies.
func (s *Store) WebAuthnCeremonies() webauthn.CeremonyStore { return ceremonyStore{s} }

type ceremonyStore struct{ s *Store }

func (c ceremonyStore) Create(ctx context.Context, key webauthn.Key, subject string, record []byte, expiresAt time.Time) error {
	k := hex.EncodeToString(key[:])
	return c.s.transaction(ctx, func(q queries) error {
		if err := q.deleteExpiredWebAuthnCeremony(ctx, k, time.Now().Unix()); err != nil {
			return err
		}
		n, err := q.insertWebAuthnCeremony(ctx, webauthnCeremony{Key: k, Subject: subject, Record: base64.StdEncoding.EncodeToString(record), ExpiresAt: expiresAt.Unix()})
		if err != nil {
			return err
		}
		if n == 0 {
			return errors.New("store: WebAuthn ceremony key collision")
		}
		return nil
	})
}

func (c ceremonyStore) Take(ctx context.Context, key webauthn.Key, now time.Time) ([]byte, error) {
	row, err := c.s.q.takeWebAuthnCeremony(ctx, hex.EncodeToString(key[:]))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, webauthn.ErrInvalid
	}
	if err != nil {
		return nil, err
	}
	if now.Unix() >= row.ExpiresAt {
		return nil, webauthn.ErrInvalid
	}
	record, err := base64.StdEncoding.DecodeString(row.Record)
	if err != nil {
		return nil, fmt.Errorf("store: stored WebAuthn ceremony: %w", err)
	}
	return record, nil
}

// DeleteWebAuthnCeremonies removes the pending WebAuthn ceremonies of
// subject.
func (s *Store) DeleteWebAuthnCeremonies(ctx context.Context, subject string) error {
	return s.q.deleteSubjectWebAuthnCeremonies(ctx, subject)
}

// TOTPRecord returns the authenticator app of userID, or sql.ErrNoRows.
func (s *Store) TOTPRecord(ctx context.Context, userID string) (MFATOTP, error) {
	return s.q.mfaTOTP(ctx, userID)
}

// ConsumeTOTPStep records step as the last accepted time step of the
// authenticator app id of userID. It reports false when the app is missing,
// was replaced (another id) or step is not after the last accepted one. Of
// two concurrent calls with the same step, one reports true.
func (s *Store) ConsumeTOTPStep(ctx context.Context, userID, id string, step int64) (bool, error) {
	if id == "" {
		return false, nil
	}
	n, err := s.q.consumeMFATOTPStep(ctx, userID, id, step)
	return n == 1, err
}

// WebAuthnEnrollment is what the WebAuthn credential store's Insert needs
// besides the credential. The caller puts it in the context of
// FinishRegistration with WithWebAuthnEnrollment.
type WebAuthnEnrollment struct {
	// UserID must equal the subject of the ceremony.
	UserID string
	// Name is the name of the new credential.
	Name string
	// Codes are stored as the recovery codes when the user had no usable
	// second factor.
	Codes []RecoveryCode
	// OnlyIfNone refuses a user that has a usable second factor
	// (ErrMFAEnrolled).
	OnlyIfNone bool
	// SessionID, when set, must be an active session of the user (the
	// settings screen). PasswordDigest, when set, must be auth.Hash of the
	// current password hash (the enrollment before the first session).
	SessionID      string
	PasswordDigest string
	Now            int64
	// Issued is set to true when Codes were stored.
	Issued *bool
	// Stored, when not nil, receives the stored credential.
	Stored *WebAuthnCredential
}

type webauthnEnrollmentKey struct{}

// WithWebAuthnEnrollment returns ctx carrying e for the credential store's
// Insert.
func WithWebAuthnEnrollment(ctx context.Context, e WebAuthnEnrollment) context.Context {
	return context.WithValue(ctx, webauthnEnrollmentKey{}, e)
}

// WebAuthnCredentials returns the webauthn.Store of authlume/webauthn on
// mfa_webauthn_credentials. Rows without a version, stored before authlume,
// are left out: they cannot be used.
func (s *Store) WebAuthnCredentials() webauthn.Store { return credentialStore{s} }

type credentialStore struct{ s *Store }

// CredentialID is the stored form of a WebAuthn credential ID.
func CredentialID(id []byte) string { return base64.RawURLEncoding.EncodeToString(id) }

func (c credentialStore) Load(ctx context.Context, subject string) ([]webauthn.Record, error) {
	rows, err := c.s.q.listWebAuthnCredentials(ctx, subject)
	if err != nil {
		return nil, err
	}
	out := make([]webauthn.Record, 0, len(rows))
	for _, row := range rows {
		if row.Version == "" {
			continue
		}
		var cred webauthn.Credential
		if err := json.Unmarshal([]byte(row.Credential), &cred); err != nil {
			return nil, fmt.Errorf("store: stored WebAuthn credential: %w", err)
		}
		out = append(out, webauthn.Record{Credential: cred, Version: row.Version, Passkey: row.Passkey != 0})
	}
	return out, nil
}

// invalidEnrollment wraps reason so that both webauthn.ErrInvalid and
// reason match with errors.Is.
func invalidEnrollment(reason error) error { return fmt.Errorf("%w: %w", webauthn.ErrInvalid, reason) }

func (c credentialStore) Insert(ctx context.Context, subject string, credential webauthn.Credential, passkey bool) error {
	e, ok := ctx.Value(webauthnEnrollmentKey{}).(WebAuthnEnrollment)
	if !ok || e.UserID == "" || e.UserID != subject {
		return webauthn.ErrInvalid
	}
	encoded, err := json.Marshal(credential)
	if err != nil {
		return err
	}
	id, err := newRandomID()
	if err != nil {
		return err
	}
	version, err := newRandomID()
	if err != nil {
		return err
	}
	pk := int64(0)
	if passkey {
		pk = 1
	}
	return c.s.transaction(ctx, func(q queries) error {
		u, status, err := lockMFAUser(ctx, q, subject)
		if errors.Is(err, ErrUnauthorized) {
			return invalidEnrollment(ErrUnauthorized)
		}
		if err != nil {
			return err
		}
		if e.SessionID != "" {
			if _, err := q.activeSession(ctx, e.SessionID, subject, e.Now); errors.Is(err, sql.ErrNoRows) {
				return invalidEnrollment(ErrUnauthorized)
			} else if err != nil {
				return err
			}
		}
		if e.PasswordDigest != "" && subtle.ConstantTimeCompare([]byte(digest(u.PasswordHash)), []byte(e.PasswordDigest)) != 1 {
			return invalidEnrollment(ErrUnauthorized)
		}
		if e.SessionID == "" && e.PasswordDigest == "" {
			return webauthn.ErrInvalid
		}
		first := !status.usable()
		if e.OnlyIfNone && !first {
			return invalidEnrollment(ErrMFAEnrolled)
		}
		if status.UsableWebAuthn() >= MaxWebAuthnCredentials {
			return invalidEnrollment(ErrMFALimit)
		}
		row := WebAuthnCredential{ID: id, UserID: subject, CredentialID: CredentialID(credential.ID), Credential: string(encoded), Name: e.Name, CreatedAt: e.Now, Version: version, Passkey: pk}
		n, err := q.createWebAuthnCredential(ctx, row)
		if err != nil {
			return err
		}
		if n != 1 {
			return invalidEnrollment(ErrMFACredentialExists)
		}
		if e.Stored != nil {
			*e.Stored = row
		}
		if !first {
			return nil
		}
		if err := q.deleteUserRecoveryCodes(ctx, subject); err != nil {
			return err
		}
		if err := insertRecoveryCodes(ctx, q, subject, e.Codes, e.Now); err != nil {
			return err
		}
		if e.Issued != nil {
			*e.Issued = true
		}
		return nil
	})
}

func (c credentialStore) Update(ctx context.Context, subject, version string, credential webauthn.Credential) error {
	if version == "" {
		return webauthn.ErrInvalid
	}
	encoded, err := json.Marshal(credential)
	if err != nil {
		return err
	}
	next, err := newRandomID()
	if err != nil {
		return err
	}
	n, err := c.s.q.updateWebAuthnCredential(ctx, subject, CredentialID(credential.ID), string(encoded), version, next, time.Now().Unix())
	if err != nil {
		return err
	}
	if n != 1 {
		return webauthn.ErrInvalid
	}
	return nil
}

// ApproveOIDCLink approves linking the user with email to the first OIDC
// identity that signs in with that email until until (Unix seconds), for the
// operator's CLI. A linked user cannot be approved (ErrOIDCLinked).
func (s *Store) ApproveOIDCLink(ctx context.Context, email string, until int64) error {
	u, err := s.q.userByEmail(ctx, email)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	return s.transaction(ctx, func(q queries) error {
		u, err := q.lockUser(ctx, u.ID)
		if err != nil {
			return err
		}
		if u.OidcIssuer.Valid || u.OidcSubject.Valid {
			return ErrOIDCLinked
		}
		return q.setOIDCLinkApproval(ctx, u.ID, sql.NullInt64{Int64: until, Valid: true})
	})
}

// DeleteExpiredAuth removes the expired browser sessions, OIDC logins,
// WebAuthn ceremonies, MCP codes and connections, and the dynamic MCP client
// registrations whose deadline passed, for the hourly cleanup.
func (s *Store) DeleteExpiredAuth(ctx context.Context, now time.Time) error {
	n := now.Unix()
	for _, f := range []func(context.Context, int64) error{
		s.q.deleteExpiredWebSessions,
		s.q.deleteExpiredOIDCLogins,
		s.q.deleteExpiredWebAuthnCeremonies,
		s.q.deleteExpiredMCPCodes,
		s.q.deleteExpiredMCPConnections,
	} {
		if err := f(ctx, n); err != nil {
			return err
		}
	}
	return sweepMCPRegistrations(ctx, s.q, n)
}
