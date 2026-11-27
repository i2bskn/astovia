package store

import (
	"context"
	"database/sql"

	db "github.com/i2bskn/astovia/internal/store/postgresdb"
)

func (q postgresQueries) setOIDCLinkApproval(ctx context.Context, id string, until sql.NullInt64) error {
	return q.q.SetOIDCLinkApproval(ctx, db.SetOIDCLinkApprovalParams{ApprovedUntil: until, ID: id})
}
func (q postgresQueries) insertWebSession(ctx context.Context, w WebSession) (int64, error) {
	return q.q.InsertWebSession(ctx, db.InsertWebSessionParams(w))
}
func (q postgresQueries) liveWebSession(ctx context.Context, id string, now int64) (WebSession, error) {
	row, err := q.q.GetLiveWebSession(ctx, db.GetLiveWebSessionParams{ID: id, Now: now})
	return WebSession(row), err
}
func (q postgresQueries) touchLiveWebSession(ctx context.Context, id string, now int64) (WebSession, error) {
	row, err := q.q.TouchLiveWebSession(ctx, db.TouchLiveWebSessionParams{ID: id, Now: now})
	return WebSession(row), err
}
func (q postgresQueries) deleteWebSession(ctx context.Context, id string) error {
	return q.q.DeleteWebSession(ctx, id)
}
func (q postgresQueries) deleteExpiredWebSessions(ctx context.Context, now int64) error {
	return q.q.DeleteExpiredWebSessions(ctx, now)
}
func (q postgresQueries) deleteExpiredOIDCLogin(ctx context.Context, key string, now int64) error {
	return q.q.DeleteExpiredOIDCLogin(ctx, db.DeleteExpiredOIDCLoginParams{Key: key, Now: now})
}
func (q postgresQueries) deleteExpiredOIDCLogins(ctx context.Context, now int64) error {
	return q.q.DeleteExpiredOIDCLogins(ctx, now)
}
func (q postgresQueries) insertOIDCLogin(ctx context.Context, l oidcLogin) (int64, error) {
	return q.q.InsertOIDCLogin(ctx, db.InsertOIDCLoginParams(l))
}
func (q postgresQueries) takeOIDCLogin(ctx context.Context, key string) (oidcLogin, error) {
	row, err := q.q.TakeOIDCLogin(ctx, key)
	return oidcLogin(row), err
}
func (q postgresQueries) deleteExpiredWebAuthnCeremony(ctx context.Context, key string, now int64) error {
	return q.q.DeleteExpiredWebAuthnCeremony(ctx, db.DeleteExpiredWebAuthnCeremonyParams{Key: key, Now: now})
}
func (q postgresQueries) deleteExpiredWebAuthnCeremonies(ctx context.Context, now int64) error {
	return q.q.DeleteExpiredWebAuthnCeremonies(ctx, now)
}
func (q postgresQueries) insertWebAuthnCeremony(ctx context.Context, c webauthnCeremony) (int64, error) {
	return q.q.InsertWebAuthnCeremony(ctx, db.InsertWebAuthnCeremonyParams(c))
}
func (q postgresQueries) takeWebAuthnCeremony(ctx context.Context, key string) (webauthnCeremony, error) {
	row, err := q.q.TakeWebAuthnCeremony(ctx, key)
	return webauthnCeremony(row), err
}
func (q postgresQueries) deleteSubjectWebAuthnCeremonies(ctx context.Context, subject string) error {
	return q.q.DeleteSubjectWebAuthnCeremonies(ctx, subject)
}
func (q postgresQueries) deleteExpiredMCPCodes(ctx context.Context, now int64) error {
	return q.q.DeleteExpiredMCPCodes(ctx, now)
}
func (q postgresQueries) insertMCPCode(ctx context.Context, c mcpCode) (int64, error) {
	return q.q.InsertMCPCode(ctx, db.InsertMCPCodeParams{Key: c.Key, ClientID: c.ClientID, RedirectUri: c.RedirectUri, Subject: c.Subject, Scopes: c.Scopes, Challenge: c.Challenge, ExpiresAt: c.ExpiresAt})
}
func (q postgresQueries) lockMCPCode(ctx context.Context, key string) (mcpCode, error) {
	row, err := q.q.LockMCPCode(ctx, key)
	return mcpCode(row), err
}
func (q postgresQueries) deleteMCPCode(ctx context.Context, key string) error {
	return q.q.DeleteMCPCode(ctx, key)
}
func (q postgresQueries) markMCPCodeConsumed(ctx context.Context, key string) error {
	return q.q.MarkMCPCodeConsumed(ctx, key)
}
func (q postgresQueries) markMCPCodeReused(ctx context.Context, key string) error {
	return q.q.MarkMCPCodeReused(ctx, key)
}
func (q postgresQueries) deleteMCPConnectionByCode(ctx context.Context, code string) ([]mcpConnection, error) {
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
func (q postgresQueries) deleteExpiredMCPConnections(ctx context.Context, now int64) error {
	return q.q.DeleteExpiredMCPConnections(ctx, now)
}
func (q postgresQueries) insertMCPConnection(ctx context.Context, c mcpConnection) (int64, error) {
	return q.q.InsertMCPConnection(ctx, db.InsertMCPConnectionParams{ID: c.ID, Selector: c.Selector, Subject: c.Subject, ClientID: c.ClientID, Scopes: c.Scopes, CreatedAt: c.CreatedAt, RefreshedAt: c.RefreshedAt, AbsoluteExpiry: c.AbsoluteExpiry, Access: c.Access, AccessExpiry: c.AccessExpiry, Refresh: c.Refresh, RefreshExpiry: c.RefreshExpiry, Code: c.Code})
}
func (q postgresQueries) lockMCPConnectionBySelector(ctx context.Context, selector string) (mcpConnection, error) {
	row, err := q.q.LockMCPConnectionBySelector(ctx, selector)
	return mcpConnection(row), err
}
func (q postgresQueries) rotateMCPConnection(ctx context.Context, id, access string, accessExpiry int64, refresh string, refreshExpiry, refreshedAt int64) (mcpConnection, error) {
	row, err := q.q.RotateMCPConnection(ctx, db.RotateMCPConnectionParams{Access: access, AccessExpiry: accessExpiry, Refresh: refresh, RefreshExpiry: refreshExpiry, RefreshedAt: refreshedAt, ID: id})
	return mcpConnection(row), err
}
func (q postgresQueries) liveMCPConnectionBySelector(ctx context.Context, selector string, now int64) (mcpConnection, error) {
	row, err := q.q.GetLiveMCPConnectionBySelector(ctx, db.GetLiveMCPConnectionBySelectorParams{Selector: selector, Now: now})
	return mcpConnection(row), err
}
func (q postgresQueries) liveMCPConnectionByAccess(ctx context.Context, access string, now int64) (mcpConnection, error) {
	row, err := q.q.GetLiveMCPConnectionByAccess(ctx, db.GetLiveMCPConnectionByAccessParams{Access: access, Now: now})
	return mcpConnection(row), err
}
func (q postgresQueries) deleteMCPConnection(ctx context.Context, id string) error {
	return q.q.DeleteMCPConnection(ctx, id)
}
func (q postgresQueries) listLiveMCPConnections(ctx context.Context, subject string, now int64) ([]mcpConnectionListRow, error) {
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
func (q postgresQueries) deleteSubjectMCPConnection(ctx context.Context, id, subject string) (int64, error) {
	return q.q.DeleteSubjectMCPConnection(ctx, db.DeleteSubjectMCPConnectionParams{ID: id, Subject: subject})
}
func (q postgresQueries) touchMCPConnection(ctx context.Context, id string, now, before int64) error {
	return q.q.TouchMCPConnection(ctx, db.TouchMCPConnectionParams{Now: now, ID: id, TouchedBefore: before})
}
func (q postgresQueries) deleteSubjectMCPCodes(ctx context.Context, subject string) error {
	return q.q.DeleteSubjectMCPCodes(ctx, subject)
}
func (q postgresQueries) deleteSubjectMCPConnections(ctx context.Context, subject string) error {
	return q.q.DeleteSubjectMCPConnections(ctx, subject)
}
func (q postgresQueries) deleteClientMCPCodes(ctx context.Context, clientID string) error {
	return q.q.DeleteClientMCPCodes(ctx, clientID)
}
func (q postgresQueries) deleteClientMCPConnections(ctx context.Context, clientID string) (int64, error) {
	return q.q.DeleteClientMCPConnections(ctx, clientID)
}
func (q postgresQueries) countClientMCPCodes(ctx context.Context, clientID string) (int64, error) {
	return q.q.CountClientMCPCodes(ctx, clientID)
}
func (q postgresQueries) countClientMCPConnections(ctx context.Context, clientID string) (int64, error) {
	return q.q.CountClientMCPConnections(ctx, clientID)
}
func (q postgresQueries) upsertMCPClientLabel(ctx context.Context, clientID, name, host string, now int64) error {
	return q.q.UpsertMCPClientLabel(ctx, db.UpsertMCPClientLabelParams{ClientID: clientID, Name: name, Host: host, UpdatedAt: now})
}
func (q postgresQueries) releaseMCPRegistrations(ctx context.Context, releaseAt, activatedBefore int64) error {
	return q.q.ReleaseMCPRegistrations(ctx, db.ReleaseMCPRegistrationsParams{ReleaseAt: sql.NullInt64{Int64: releaseAt, Valid: true}, ActivatedBefore: activatedBefore})
}
func (q postgresQueries) deleteExpiredMCPRegistrations(ctx context.Context, now int64) error {
	return q.q.DeleteExpiredMCPRegistrations(ctx, now)
}
func (q postgresQueries) insertMCPRegistration(ctx context.Context, r mcpRegistration) (int64, error) {
	return q.q.InsertMCPRegistration(ctx, db.InsertMCPRegistrationParams(r))
}
func (q postgresQueries) liveMCPRegistration(ctx context.Context, id string, now int64) (mcpRegistration, error) {
	row, err := q.q.GetLiveMCPRegistration(ctx, db.GetLiveMCPRegistrationParams{ID: id, Now: now})
	return mcpRegistration(row), err
}
func (q postgresQueries) activateMCPRegistration(ctx context.Context, id string, now int64) (int64, error) {
	return q.q.ActivateMCPRegistration(ctx, db.ActivateMCPRegistrationParams{ID: id, Now: now})
}
func (q postgresQueries) listOIDCLinkedUserIDs(ctx context.Context) ([]string, error) {
	return q.q.ListOIDCLinkedUserIDs(ctx)
}
