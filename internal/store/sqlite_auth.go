package store

import (
	"context"
	"database/sql"

	db "github.com/i2bskn/astovia/internal/store/sqlitedb"
)

func (q sqliteQueries) setOIDCLinkApproval(ctx context.Context, id string, until sql.NullInt64) error {
	return q.q.SetOIDCLinkApproval(ctx, db.SetOIDCLinkApprovalParams{ApprovedUntil: until, ID: id})
}
func (q sqliteQueries) insertWebSession(ctx context.Context, w WebSession) (int64, error) {
	return q.q.InsertWebSession(ctx, db.InsertWebSessionParams(w))
}
func (q sqliteQueries) liveWebSession(ctx context.Context, id string, now int64) (WebSession, error) {
	row, err := q.q.GetLiveWebSession(ctx, db.GetLiveWebSessionParams{ID: id, Now: now})
	return WebSession(row), err
}
func (q sqliteQueries) touchLiveWebSession(ctx context.Context, id string, now int64) (WebSession, error) {
	row, err := q.q.TouchLiveWebSession(ctx, db.TouchLiveWebSessionParams{ID: id, Now: now})
	return WebSession(row), err
}
func (q sqliteQueries) deleteWebSession(ctx context.Context, id string) error {
	return q.q.DeleteWebSession(ctx, id)
}
func (q sqliteQueries) deleteExpiredWebSessions(ctx context.Context, now int64) error {
	return q.q.DeleteExpiredWebSessions(ctx, now)
}
func (q sqliteQueries) deleteExpiredOIDCLogin(ctx context.Context, key string, now int64) error {
	return q.q.DeleteExpiredOIDCLogin(ctx, db.DeleteExpiredOIDCLoginParams{Key: key, Now: now})
}
func (q sqliteQueries) deleteExpiredOIDCLogins(ctx context.Context, now int64) error {
	return q.q.DeleteExpiredOIDCLogins(ctx, now)
}
func (q sqliteQueries) insertOIDCLogin(ctx context.Context, l oidcLogin) (int64, error) {
	return q.q.InsertOIDCLogin(ctx, db.InsertOIDCLoginParams(l))
}
func (q sqliteQueries) takeOIDCLogin(ctx context.Context, key string) (oidcLogin, error) {
	row, err := q.q.TakeOIDCLogin(ctx, key)
	return oidcLogin(row), err
}
func (q sqliteQueries) deleteExpiredWebAuthnCeremony(ctx context.Context, key string, now int64) error {
	return q.q.DeleteExpiredWebAuthnCeremony(ctx, db.DeleteExpiredWebAuthnCeremonyParams{Key: key, Now: now})
}
func (q sqliteQueries) deleteExpiredWebAuthnCeremonies(ctx context.Context, now int64) error {
	return q.q.DeleteExpiredWebAuthnCeremonies(ctx, now)
}
func (q sqliteQueries) insertWebAuthnCeremony(ctx context.Context, c webauthnCeremony) (int64, error) {
	return q.q.InsertWebAuthnCeremony(ctx, db.InsertWebAuthnCeremonyParams(c))
}
func (q sqliteQueries) takeWebAuthnCeremony(ctx context.Context, key string) (webauthnCeremony, error) {
	row, err := q.q.TakeWebAuthnCeremony(ctx, key)
	return webauthnCeremony(row), err
}
func (q sqliteQueries) deleteSubjectWebAuthnCeremonies(ctx context.Context, subject string) error {
	return q.q.DeleteSubjectWebAuthnCeremonies(ctx, subject)
}
func (q sqliteQueries) deleteExpiredMCPCodes(ctx context.Context, now int64) error {
	return q.q.DeleteExpiredMCPCodes(ctx, now)
}
func (q sqliteQueries) insertMCPCode(ctx context.Context, c mcpCode) (int64, error) {
	return q.q.InsertMCPCode(ctx, db.InsertMCPCodeParams{Key: c.Key, ClientID: c.ClientID, RedirectUri: c.RedirectUri, Subject: c.Subject, Scopes: c.Scopes, Challenge: c.Challenge, ExpiresAt: c.ExpiresAt})
}
func (q sqliteQueries) lockMCPCode(ctx context.Context, key string) (mcpCode, error) {
	row, err := q.q.LockMCPCode(ctx, key)
	return mcpCode(row), err
}
func (q sqliteQueries) deleteMCPCode(ctx context.Context, key string) error {
	return q.q.DeleteMCPCode(ctx, key)
}
func (q sqliteQueries) markMCPCodeConsumed(ctx context.Context, key string) error {
	return q.q.MarkMCPCodeConsumed(ctx, key)
}
func (q sqliteQueries) markMCPCodeReused(ctx context.Context, key string) error {
	return q.q.MarkMCPCodeReused(ctx, key)
}
func (q sqliteQueries) deleteMCPConnectionByCode(ctx context.Context, code string) ([]mcpConnection, error) {
	rows, err := q.q.DeleteMCPConnectionByCode(ctx, sql.NullString{String: code, Valid: true})
	if err != nil {
		return nil, err
	}
	out := make([]mcpConnection, 0, len(rows))
	for _, row := range rows {
		out = append(out, mcpConnection(row))
	}
	return out, nil
}
func (q sqliteQueries) deleteExpiredMCPConnections(ctx context.Context, now int64) error {
	return q.q.DeleteExpiredMCPConnections(ctx, now)
}
func (q sqliteQueries) insertMCPConnection(ctx context.Context, c mcpConnection) (int64, error) {
	return q.q.InsertMCPConnection(ctx, db.InsertMCPConnectionParams{ID: c.ID, Selector: c.Selector, Subject: c.Subject, ClientID: c.ClientID, Scopes: c.Scopes, CreatedAt: c.CreatedAt, RefreshedAt: c.RefreshedAt, AbsoluteExpiry: c.AbsoluteExpiry, Access: c.Access, AccessExpiry: c.AccessExpiry, Refresh: c.Refresh, RefreshExpiry: c.RefreshExpiry, Code: c.Code})
}
func (q sqliteQueries) lockMCPConnectionBySelector(ctx context.Context, selector string) (mcpConnection, error) {
	row, err := q.q.LockMCPConnectionBySelector(ctx, selector)
	return mcpConnection(row), err
}
func (q sqliteQueries) rotateMCPConnection(ctx context.Context, id, access string, accessExpiry int64, refresh string, refreshExpiry, refreshedAt int64) (mcpConnection, error) {
	row, err := q.q.RotateMCPConnection(ctx, db.RotateMCPConnectionParams{Access: access, AccessExpiry: accessExpiry, Refresh: refresh, RefreshExpiry: refreshExpiry, RefreshedAt: refreshedAt, ID: id})
	return mcpConnection(row), err
}
func (q sqliteQueries) liveMCPConnectionBySelector(ctx context.Context, selector string, now int64) (mcpConnection, error) {
	row, err := q.q.GetLiveMCPConnectionBySelector(ctx, db.GetLiveMCPConnectionBySelectorParams{Selector: selector, Now: now})
	return mcpConnection(row), err
}
func (q sqliteQueries) liveMCPConnectionByAccess(ctx context.Context, access string, now int64) (mcpConnection, error) {
	row, err := q.q.GetLiveMCPConnectionByAccess(ctx, db.GetLiveMCPConnectionByAccessParams{Access: access, Now: now})
	return mcpConnection(row), err
}
func (q sqliteQueries) deleteMCPConnection(ctx context.Context, id string) error {
	return q.q.DeleteMCPConnection(ctx, id)
}
func (q sqliteQueries) listLiveMCPConnections(ctx context.Context, subject string, now int64) ([]mcpConnectionListRow, error) {
	rows, err := q.q.ListLiveMCPConnections(ctx, db.ListLiveMCPConnectionsParams{Subject: subject, Now: now})
	if err != nil {
		return nil, err
	}
	out := make([]mcpConnectionListRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, mcpConnectionListRow(row))
	}
	return out, nil
}
func (q sqliteQueries) deleteSubjectMCPConnection(ctx context.Context, id, subject string) (int64, error) {
	return q.q.DeleteSubjectMCPConnection(ctx, db.DeleteSubjectMCPConnectionParams{ID: id, Subject: subject})
}
func (q sqliteQueries) touchMCPConnection(ctx context.Context, id string, now, before int64) error {
	return q.q.TouchMCPConnection(ctx, db.TouchMCPConnectionParams{Now: now, ID: id, TouchedBefore: before})
}
func (q sqliteQueries) deleteSubjectMCPCodes(ctx context.Context, subject string) error {
	return q.q.DeleteSubjectMCPCodes(ctx, subject)
}
func (q sqliteQueries) deleteSubjectMCPConnections(ctx context.Context, subject string) error {
	return q.q.DeleteSubjectMCPConnections(ctx, subject)
}
func (q sqliteQueries) deleteClientMCPCodes(ctx context.Context, clientID string) error {
	return q.q.DeleteClientMCPCodes(ctx, clientID)
}
func (q sqliteQueries) deleteClientMCPConnections(ctx context.Context, clientID string) (int64, error) {
	return q.q.DeleteClientMCPConnections(ctx, clientID)
}
func (q sqliteQueries) countClientMCPCodes(ctx context.Context, clientID string) (int64, error) {
	return q.q.CountClientMCPCodes(ctx, clientID)
}
func (q sqliteQueries) countClientMCPConnections(ctx context.Context, clientID string) (int64, error) {
	return q.q.CountClientMCPConnections(ctx, clientID)
}
func (q sqliteQueries) upsertMCPClientLabel(ctx context.Context, clientID, name, host string, now int64) error {
	return q.q.UpsertMCPClientLabel(ctx, db.UpsertMCPClientLabelParams{ClientID: clientID, Name: name, Host: host, UpdatedAt: now})
}
func (q sqliteQueries) releaseMCPRegistrations(ctx context.Context, releaseAt, activatedBefore int64) error {
	return q.q.ReleaseMCPRegistrations(ctx, db.ReleaseMCPRegistrationsParams{ReleaseAt: sql.NullInt64{Int64: releaseAt, Valid: true}, ActivatedBefore: activatedBefore})
}
func (q sqliteQueries) deleteExpiredMCPRegistrations(ctx context.Context, now int64) error {
	return q.q.DeleteExpiredMCPRegistrations(ctx, now)
}
func (q sqliteQueries) insertMCPRegistration(ctx context.Context, r mcpRegistration) (int64, error) {
	return q.q.InsertMCPRegistration(ctx, db.InsertMCPRegistrationParams(r))
}
func (q sqliteQueries) liveMCPRegistration(ctx context.Context, id string, now int64) (mcpRegistration, error) {
	row, err := q.q.GetLiveMCPRegistration(ctx, db.GetLiveMCPRegistrationParams{ID: id, Now: now})
	return mcpRegistration(row), err
}
func (q sqliteQueries) activateMCPRegistration(ctx context.Context, id string, now int64) (int64, error) {
	return q.q.ActivateMCPRegistration(ctx, db.ActivateMCPRegistrationParams{ID: id, Now: now})
}
func (q sqliteQueries) listOIDCLinkedUserIDs(ctx context.Context) ([]string, error) {
	return q.q.ListOIDCLinkedUserIDs(ctx)
}
