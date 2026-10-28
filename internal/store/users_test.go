package store_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/i2bskn/astovia/internal/store"
	"github.com/i2bskn/astovia/internal/testutil"
)

// userFixture is apiTokenFixture (alice and bob, both owners, with sessions)
// plus carol, a member with the session "carol-session".
func userFixture(t *testing.T, driver string) *store.Store {
	t.Helper()
	s, _ := apiTokenFixture(t, driver)
	addUser(t, s, "carol", store.RoleMember)
	return s
}

// addUser creates name with role and an active session "<name>-session".
func addUser(t *testing.T, s *store.Store, name, role string) store.User {
	t.Helper()
	ctx := context.Background()
	u := store.User{ID: name + "-id", Email: name + "@example.com", PasswordHash: "x", Role: role}
	if err := s.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	sid := name + "-session"
	if err := s.Login(ctx, u, store.Session{ID: sid, UserID: u.ID, ExpiresAt: tokenTestNow + 3600, CreatedAt: tokenTestNow}, store.Refresh{Hash: "refresh-" + name, SessionID: sid, CreatedAt: tokenTestNow}); err != nil {
		t.Fatal(err)
	}
	return u
}

func invite(t *testing.T, s *store.Store, operator, id, email, role string, now int64) {
	t.Helper()
	inv := store.Invitation{ID: id, TokenHash: "inv-hash-" + id, Email: email, Role: role, CreatedAt: now, ExpiresAt: now + store.InvitationLifetime}
	if err := s.CreateInvitation(context.Background(), operator, inv); err != nil {
		t.Fatalf("invite %s: %v", id, err)
	}
}

func issueReset(t *testing.T, s *store.Store, operator, target, id string, now int64) {
	t.Helper()
	r := store.PasswordReset{ID: id, TokenHash: "pwr-hash-" + id, UserID: target, CreatedAt: now, ExpiresAt: now + store.PasswordResetLifetime}
	if err := s.CreatePasswordReset(context.Background(), operator, r); err != nil {
		t.Fatalf("reset %s: %v", id, err)
	}
}

func userRole(t *testing.T, s *store.Store, email string) (string, int64) {
	t.Helper()
	u, err := s.UserByEmail(context.Background(), email)
	if err != nil {
		t.Fatal(err)
	}
	return u.Role, u.Disabled
}

func enabledOwners(t *testing.T, s *store.Store) int {
	t.Helper()
	users, err := s.ListUsers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, u := range users {
		if u.Role == store.RoleOwner && u.Disabled == 0 {
			n++
		}
	}
	return n
}

func resetActive(s *store.Store, id string, now int64) bool {
	_, err := s.PasswordResetUser(context.Background(), "pwr-hash-"+id, now)
	return err == nil
}

func invitationActive(s *store.Store, id string, now int64) bool {
	_, err := s.InvitationByToken(context.Background(), "inv-hash-"+id, now)
	return err == nil
}

func ptr(v bool) *bool { return &v }

func TestUserRoles(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) { testUserRoles(t, driver) })
	}
}

func testUserRoles(t *testing.T, driver string) {
	ctx := context.Background()
	s, dsn := testutil.Database(t, driver)

	// Rows that existed before the role column, inserted without it, are
	// owners through the column default.
	name := "sqlite"
	if driver == "postgres" {
		name = "pgx"
	}
	raw, err := sql.Open(name, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	if _, err = raw.ExecContext(ctx, "INSERT INTO users (id,email,password_hash,disabled,created_at) VALUES ('old-id','old@example.com','x',0,1)"); err != nil {
		t.Fatal(err)
	}
	if role, _ := userRole(t, s, "old@example.com"); role != store.RoleOwner {
		t.Fatalf("existing row: %q", role)
	}

	// CreateUser requires a valid role and stores it.
	for _, role := range []string{"", "admin", "Owner"} {
		if err = s.CreateUser(ctx, store.User{ID: "bad-" + role, Email: "bad@example.com", PasswordHash: "x", Role: role}); !errors.Is(err, store.ErrInvalidRole) {
			t.Fatalf("role %q: %v", role, err)
		}
	}
	if err = s.CreateUser(ctx, store.User{ID: "member-id", Email: "member@example.com", PasswordHash: "x", CreatedAt: 2, Role: store.RoleMember}); err != nil {
		t.Fatal(err)
	}
	if role, _ := userRole(t, s, "member@example.com"); role != store.RoleMember {
		t.Fatalf("member: %q", role)
	}
	users, err := s.ListUsers(ctx)
	if err != nil || len(users) != 2 || users[0].ID != "old-id" || users[1].ID != "member-id" {
		t.Fatalf("list: %+v %v", users, err)
	}
	got, err := s.UsersByIDs(ctx, []string{"member-id", "missing"})
	if err != nil || len(got) != 1 || got[0].Email != "member@example.com" {
		t.Fatalf("by IDs: %+v %v", got, err)
	}
	if got, err = s.UsersByIDs(ctx, nil); err != nil || got == nil || len(got) != 0 {
		t.Fatalf("by no IDs: %+v %v", got, err)
	}
}

func TestOwnerRules(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) { testOwnerRules(t, driver) })
	}
}

func testOwnerRules(t *testing.T, driver string) {
	ctx := context.Background()
	s := userFixture(t, driver)
	now := int64(tokenTestNow)
	update := func(operator, target string, change store.UserChange) (store.User, error) {
		t.Helper()
		return s.UpdateUser(ctx, operator+"-id", target+"-id", change, now)
	}

	// A member is refused, whoever the target is.
	if _, err := update("carol", "carol", store.UserChange{Role: store.RoleOwner}); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("member promotes itself: %v", err)
	}
	if _, err := update("missing", "carol", store.UserChange{Role: store.RoleOwner}); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("missing operator: %v", err)
	}
	if _, err := update("alice", "missing", store.UserChange{Role: store.RoleOwner}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing target: %v", err)
	}
	if _, err := update("alice", "bob", store.UserChange{Role: "admin"}); !errors.Is(err, store.ErrInvalidRole) {
		t.Fatalf("invalid role: %v", err)
	}
	if _, err := update("alice", "bob", store.UserChange{}); err == nil {
		t.Fatal("empty change accepted")
	}
	if _, err := update("alice", "bob", store.UserChange{Role: store.RoleMember, Disabled: ptr(true)}); err == nil {
		t.Fatal("two changes accepted")
	}

	// An owner cannot disable itself, even with other owners.
	if _, err := update("alice", "alice", store.UserChange{Disabled: ptr(true)}); !errors.Is(err, store.ErrSelf) {
		t.Fatalf("disable self: %v", err)
	}
	// Changes to the current value do nothing and succeed.
	if u, err := update("alice", "alice", store.UserChange{Disabled: ptr(false)}); err != nil || u.Disabled != 0 {
		t.Fatalf("enable an enabled user: %+v %v", u, err)
	}
	if u, err := update("alice", "carol", store.UserChange{Role: store.RoleMember}); err != nil || u.Role != store.RoleMember {
		t.Fatalf("same role: %+v %v", u, err)
	}

	// With two owners, one may demote itself; it then loses the owner API.
	if u, err := update("bob", "bob", store.UserChange{Role: store.RoleMember}); err != nil || u.Role != store.RoleMember || u.Email != "bob@example.com" {
		t.Fatalf("demote self: %+v %v", u, err)
	}
	if _, err := update("bob", "carol", store.UserChange{Role: store.RoleOwner}); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("demoted operator: %v", err)
	}
	// The last enabled owner can neither be demoted nor disable itself.
	if _, err := update("alice", "alice", store.UserChange{Role: store.RoleMember}); !errors.Is(err, store.ErrLastOwner) {
		t.Fatalf("demote the last owner: %v", err)
	}
	if _, err := update("alice", "alice", store.UserChange{Disabled: ptr(true)}); !errors.Is(err, store.ErrSelf) {
		t.Fatalf("disable the last owner: %v", err)
	}
	// A disabled owner does not count.
	if _, err := update("alice", "bob", store.UserChange{Role: store.RoleOwner}); err != nil {
		t.Fatal(err)
	}
	if u, err := update("alice", "bob", store.UserChange{Disabled: ptr(true)}); err != nil || u.Disabled != 1 || u.Role != store.RoleOwner {
		t.Fatalf("disable bob: %+v %v", u, err)
	}
	if _, err := update("alice", "alice", store.UserChange{Role: store.RoleMember}); !errors.Is(err, store.ErrLastOwner) {
		t.Fatalf("demote with only a disabled owner left: %v", err)
	}
	// Disabled users can change role, and keep it when enabled.
	if u, err := update("alice", "bob", store.UserChange{Role: store.RoleMember}); err != nil || u.Role != store.RoleMember || u.Disabled != 1 {
		t.Fatalf("demote a disabled user: %+v %v", u, err)
	}
	if u, err := update("alice", "bob", store.UserChange{Disabled: ptr(false)}); err != nil || u.Disabled != 0 || u.Role != store.RoleMember {
		t.Fatalf("enable bob: %+v %v", u, err)
	}
	// Disabling revoked bob's sessions; enabling does not bring them back.
	if _, err := s.Authenticated(ctx, "bob-id", "bob-session", now); !errors.Is(err, store.ErrUnauthorized) {
		t.Fatalf("bob's session after disable and enable: %v", err)
	}
	// Demoting keeps the sessions.
	if _, err := update("alice", "carol", store.UserChange{Role: store.RoleOwner}); err != nil {
		t.Fatal(err)
	}
	if _, err := update("alice", "carol", store.UserChange{Role: store.RoleMember}); err != nil {
		t.Fatal(err)
	}
	if u, err := s.Authenticated(ctx, "carol-id", "carol-session", now); err != nil || u.Role != store.RoleMember {
		t.Fatalf("carol's session after demotion: %+v %v", u, err)
	}
	if n := enabledOwners(t, s); n != 1 {
		t.Fatalf("owners: %d", n)
	}
}

func TestInvitations(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) { testInvitations(t, driver) })
	}
}

func testInvitations(t *testing.T, driver string) {
	ctx := context.Background()
	s := userFixture(t, driver)
	now := int64(tokenTestNow)
	create := func(operator, id, email, role string, at int64) error {
		return s.CreateInvitation(ctx, operator+"-id", store.Invitation{ID: id, TokenHash: "inv-hash-" + id, Email: email, Role: role, CreatedAt: at, ExpiresAt: at + store.InvitationLifetime})
	}

	if err := create("carol", "i0", "dave@example.com", store.RoleMember, now); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("member invites: %v", err)
	}
	if err := create("alice", "i0", "dave@example.com", "admin", now); !errors.Is(err, store.ErrInvalidRole) {
		t.Fatalf("invalid role: %v", err)
	}
	// Existing users, enabled or not, cannot be invited.
	if err := create("alice", "i0", "carol@example.com", store.RoleMember, now); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("existing user: %v", err)
	}
	if _, err := s.UpdateUser(ctx, "alice-id", "carol-id", store.UserChange{Disabled: ptr(true)}, now); err != nil {
		t.Fatal(err)
	}
	if err := create("alice", "i0", "carol@example.com", store.RoleMember, now); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("disabled user: %v", err)
	}

	invite(t, s, "alice-id", "i1", "dave@example.com", store.RoleMember, now)
	inv, err := s.InvitationByToken(ctx, "inv-hash-i1", now)
	if err != nil || inv.Email != "dave@example.com" || inv.Role != store.RoleMember || inv.InvitedBy != "alice-id" || inv.ExpiresAt != now+7*24*3600 {
		t.Fatalf("inspect: %+v %v", inv, err)
	}
	// A new invitation to the same email replaces the active one.
	invite(t, s, "bob-id", "i2", "dave@example.com", store.RoleOwner, now+1)
	if invitationActive(s, "i1", now+1) || !invitationActive(s, "i2", now+1) {
		t.Fatal("same email was not replaced")
	}
	invite(t, s, "alice-id", "i3", "erin@example.com", store.RoleMember, now+2)
	list, err := s.ListInvitations(ctx, now+2)
	if err != nil || len(list) != 2 || list[0].ID != "i3" || list[1].ID != "i2" || list[1].InvitedBy != "bob-id" {
		t.Fatalf("list: %+v %v", list, err)
	}

	// Expiry: expires_at == now is expired.
	end := now + 1 + store.InvitationLifetime
	if !invitationActive(s, "i2", end-1) || invitationActive(s, "i2", end) {
		t.Fatal("expiry boundary")
	}
	if _, err = s.AcceptInvitation(ctx, "inv-hash-i2", store.User{ID: "dave-id", PasswordHash: "h"}, end); !errors.Is(err, store.ErrInvitationInvalid) {
		t.Fatalf("accept at expiry: %v", err)
	}
	if list, err = s.ListInvitations(ctx, end); err != nil || len(list) != 1 || list[0].ID != "i3" {
		t.Fatalf("list after expiry: %+v %v", list, err)
	}

	// Accepting creates the user with the invitation's email and role.
	u, err := s.AcceptInvitation(ctx, "inv-hash-i2", store.User{ID: "dave-id", PasswordHash: "h", CreatedAt: now + 5}, now+5)
	if err != nil || u.Email != "dave@example.com" || u.Role != store.RoleOwner || u.Disabled != 0 || u.PasswordHash != "h" {
		t.Fatalf("accept: %+v %v", u, err)
	}
	if role, _ := userRole(t, s, "dave@example.com"); role != store.RoleOwner {
		t.Fatalf("dave: %q", role)
	}
	for _, id := range []string{"i1", "i2", "missing"} {
		if _, err = s.AcceptInvitation(ctx, "inv-hash-"+id, store.User{ID: "x-" + id, PasswordHash: "h"}, now+6); !errors.Is(err, store.ErrInvitationInvalid) {
			t.Fatalf("accept %s again: %v", id, err)
		}
	}

	// Revoking.
	if err = s.RevokeInvitation(ctx, "carol-id", "i3", now+6); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("revoke by a disabled member: %v", err)
	}
	if err = s.RevokeInvitation(ctx, "alice-id", "i3", now+6); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"i3", "i2", "missing"} {
		if err = s.RevokeInvitation(ctx, "alice-id", id, now+7); !errors.Is(err, store.ErrInvitationNotFound) {
			t.Fatalf("revoke %s again: %v", id, err)
		}
	}
	if invitationActive(s, "i3", now+7) {
		t.Fatal("revoked invitation is active")
	}

	// An invitation for an email that got a user meanwhile fails with
	// ErrConflict and stays active.
	invite(t, s, "alice-id", "i4", "frank@example.com", store.RoleMember, now+8)
	if err = s.CreateUser(ctx, store.User{ID: "frank-id", Email: "frank@example.com", PasswordHash: "x", Role: store.RoleMember}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AcceptInvitation(ctx, "inv-hash-i4", store.User{ID: "frank-2", PasswordHash: "h"}, now+9); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("accept for an existing email: %v", err)
	}
	if !invitationActive(s, "i4", now+9) {
		t.Fatal("failed acceptance used the invitation")
	}
}

func TestPasswordResets(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) { testPasswordResets(t, driver) })
	}
}

func testPasswordResets(t *testing.T, driver string) {
	ctx := context.Background()
	s := userFixture(t, driver)
	now := int64(tokenTestNow)
	create := func(operator, target, id string) error {
		return s.CreatePasswordReset(ctx, operator+"-id", store.PasswordReset{ID: id, TokenHash: "pwr-hash-" + id, UserID: target + "-id", CreatedAt: now, ExpiresAt: now + store.PasswordResetLifetime})
	}
	if err := create("carol", "bob", "r0"); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("member issues: %v", err)
	}
	if err := create("alice", "missing", "r0"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing user: %v", err)
	}

	// Carol has a session, an API token, AI settings, a push subscription
	// and a GitHub connection.
	if err := s.CreateAPIToken(ctx, store.APIToken{ID: "carol-token", UserID: "carol-id", Name: "t", CreatedAt: now}, "carol-token-hash", "carol-session", now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveAISettings(ctx, store.AISettings{UserID: "carol-id", Provider: "anthropic", Model: "m", APIKeySealed: "s"}, 0, true, "carol-session", now); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.SavePushSubscription(ctx, store.PushSubscription{ID: "carol-sub", UserID: "carol-id", Endpoint: "https://push.example/carol", P256dh: "k", Auth: "a"}, "", "carol-session", now); err != nil {
		t.Fatal(err)
	}
	saveConnection(t, s, "carol-id", "carol", nil)

	issueReset(t, s, "alice-id", "carol-id", "r1", now)
	if u, err := s.PasswordResetUser(ctx, "pwr-hash-r1", now); err != nil || u.ID != "carol-id" {
		t.Fatalf("inspect: %+v %v", u, err)
	}
	// Issuing again revokes the previous link for the same user only.
	issueReset(t, s, "alice-id", "bob-id", "rb", now)
	issueReset(t, s, "bob-id", "carol-id", "r2", now)
	if resetActive(s, "r1", now) || !resetActive(s, "r2", now) || !resetActive(s, "rb", now) {
		t.Fatal("re-issue")
	}
	// Carol, while a member, cannot issue; give her links she issued as an
	// owner to check that completing her reset revokes them.
	if _, err := s.UpdateUser(ctx, "alice-id", "carol-id", store.UserChange{Role: store.RoleOwner}, now); err != nil {
		t.Fatal(err)
	}
	issueReset(t, s, "carol-id", "bob-id", "by-carol", now)
	invite(t, s, "carol-id", "inv-carol", "gina@example.com", store.RoleMember, now)
	if resetActive(s, "rb", now) {
		t.Fatal("carol's link did not replace alice's link for bob")
	}

	// Expiry boundary.
	end := now + store.PasswordResetLifetime
	if !resetActive(s, "r2", end-1) || resetActive(s, "r2", end) {
		t.Fatal("expiry boundary")
	}
	if _, err := s.CompletePasswordReset(ctx, "pwr-hash-r2", "new-hash", end); !errors.Is(err, store.ErrResetInvalid) {
		t.Fatalf("complete at expiry: %v", err)
	}
	if _, err := s.CompletePasswordReset(ctx, "pwr-hash-missing", "new-hash", now); !errors.Is(err, store.ErrResetInvalid) {
		t.Fatalf("complete missing: %v", err)
	}

	u, err := s.CompletePasswordReset(ctx, "pwr-hash-r2", "new-hash", now+10)
	if err != nil || u.ID != "carol-id" || u.PasswordHash != "new-hash" {
		t.Fatalf("complete: %+v %v", u, err)
	}
	stored, err := s.UserByEmail(ctx, "carol@example.com")
	if err != nil || stored.PasswordHash != "new-hash" || stored.Disabled != 0 {
		t.Fatalf("stored: %+v %v", stored, err)
	}
	// The same things as a CLI password reset are gone; the GitHub
	// connection stays.
	if _, err = s.Authenticated(ctx, "carol-id", "carol-session", now+10); !errors.Is(err, store.ErrUnauthorized) {
		t.Fatalf("session: %v", err)
	}
	if _, _, err = s.AuthenticateAPIToken(ctx, "carol-token-hash", now+10); !errors.Is(err, store.ErrUnauthorized) {
		t.Fatalf("API token: %v", err)
	}
	if _, err = s.AISettings(ctx, "carol-id"); !errors.Is(err, store.ErrAISettingsNotFound) {
		t.Fatalf("AI settings: %v", err)
	}
	if subs, err := s.PushSubscriptions(ctx, "carol-id"); err != nil || len(subs) != 0 {
		t.Fatalf("subscriptions: %+v %v", subs, err)
	}
	if _, err = s.GitHubConnection(ctx, "carol-id"); err != nil {
		t.Fatalf("GitHub connection: %v", err)
	}
	// The link is used, and the links carol issued are revoked.
	if resetActive(s, "r2", now+10) || resetActive(s, "by-carol", now+10) || invitationActive(s, "inv-carol", now+10) {
		t.Fatal("links survived the reset")
	}
	if _, err = s.CompletePasswordReset(ctx, "pwr-hash-r2", "other", now+11); !errors.Is(err, store.ErrResetInvalid) {
		t.Fatalf("complete twice: %v", err)
	}

	// The notice: the latest used link within 14 days, with the issuer.
	notice, err := s.LatestResetNotice(ctx, "carol-id", now+10)
	if err != nil || notice == nil || notice.At != now+10 || notice.ByEmail != "bob@example.com" {
		t.Fatalf("notice: %+v %v", notice, err)
	}
	if notice, err = s.LatestResetNotice(ctx, "carol-id", now+10+store.ResetNoticePeriod-1); err != nil || notice == nil {
		t.Fatalf("notice before the end: %+v %v", notice, err)
	}
	if notice, err = s.LatestResetNotice(ctx, "carol-id", now+10+store.ResetNoticePeriod); err != nil || notice != nil {
		t.Fatalf("notice at the end: %+v %v", notice, err)
	}
	if notice, err = s.LatestResetNotice(ctx, "bob-id", now+10); err != nil || notice != nil {
		t.Fatalf("no reset: %+v %v", notice, err)
	}
	issueReset(t, s, "alice-id", "carol-id", "r3", now+20)
	if _, err = s.CompletePasswordReset(ctx, "pwr-hash-r3", "newer", now+30); err != nil {
		t.Fatal(err)
	}
	if notice, err = s.LatestResetNotice(ctx, "carol-id", now+30); err != nil || notice == nil || notice.At != now+30 || notice.ByEmail != "alice@example.com" {
		t.Fatalf("latest notice: %+v %v", notice, err)
	}

	// Disabled users get no links, and their links do not work.
	issueReset(t, s, "alice-id", "carol-id", "r4", now+40)
	if _, err = s.UpdateUser(ctx, "alice-id", "carol-id", store.UserChange{Disabled: ptr(true)}, now+41); err != nil {
		t.Fatal(err)
	}
	if err = s.CreatePasswordReset(ctx, "alice-id", store.PasswordReset{ID: "r5", TokenHash: "pwr-hash-r5", UserID: "carol-id", CreatedAt: now + 42, ExpiresAt: now + 42 + store.PasswordResetLifetime}); !errors.Is(err, store.ErrUserDisabled) {
		t.Fatalf("issue for a disabled user: %v", err)
	}
	// Enabling again does not bring back the link revoked by disabling.
	if _, err = s.UpdateUser(ctx, "alice-id", "carol-id", store.UserChange{Disabled: ptr(false)}, now+43); err != nil {
		t.Fatal(err)
	}
	if resetActive(s, "r4", now+43) {
		t.Fatal("link survived disable and enable")
	}
	if _, err = s.CompletePasswordReset(ctx, "pwr-hash-r4", "x", now+43); !errors.Is(err, store.ErrResetInvalid) {
		t.Fatalf("complete after disable and enable: %v", err)
	}

	// An owner may issue a link for itself.
	issueReset(t, s, "alice-id", "alice-id", "self", now+50)
	if u, err = s.PasswordResetUser(ctx, "pwr-hash-self", now+50); err != nil || u.ID != "alice-id" {
		t.Fatalf("self link: %+v %v", u, err)
	}
}

// TestLinksRevoked checks that recovering, disabling and demoting a user
// revoke the links it issued, and that recovering and disabling also revoke
// the reset links issued for it.
func TestLinksRevoked(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) { testLinksRevoked(t, driver) })
	}
}

func testLinksRevoked(t *testing.T, driver string) {
	ctx := context.Background()
	now := int64(tokenTestNow)
	for _, tc := range []struct {
		name     string
		received bool // whether the reset links issued for bob are revoked
		run      func(s *store.Store) error
	}{
		{"CLI reset-password", true, func(s *store.Store) error {
			return s.ChangeUser(ctx, "bob@example.com", "new", false, now)
		}},
		{"CLI disable", true, func(s *store.Store) error { return s.ChangeUser(ctx, "bob@example.com", "", true, now) }},
		{"disable", true, func(s *store.Store) error {
			_, err := s.UpdateUser(ctx, "alice-id", "bob-id", store.UserChange{Disabled: ptr(true)}, now)
			return err
		}},
		{"complete", true, func(s *store.Store) error {
			issueReset(t, s, "alice-id", "bob-id", "used", now)
			_, err := s.CompletePasswordReset(ctx, "pwr-hash-used", "new", now)
			return err
		}},
		{"demote", false, func(s *store.Store) error {
			_, err := s.UpdateUser(ctx, "alice-id", "bob-id", store.UserChange{Role: store.RoleMember}, now)
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := userFixture(t, driver)
			// for-bob is a link for bob; by-bob and inv-bob were issued by
			// bob; for-carol and inv-alice are unrelated to bob.
			issueReset(t, s, "bob-id", "carol-id", "by-bob", now)
			invite(t, s, "bob-id", "inv-bob", "dave@example.com", store.RoleMember, now)
			invite(t, s, "alice-id", "inv-alice", "erin@example.com", store.RoleMember, now)
			addUser(t, s, "frank", store.RoleMember)
			issueReset(t, s, "alice-id", "frank-id", "for-frank", now)
			if tc.name != "complete" {
				issueReset(t, s, "alice-id", "bob-id", "for-bob", now)
			}
			if err := tc.run(s); err != nil {
				t.Fatal(err)
			}
			if resetActive(s, "by-bob", now) || invitationActive(s, "inv-bob", now) {
				t.Fatal("links issued by bob survived")
			}
			if tc.name != "complete" && resetActive(s, "for-bob", now) == tc.received {
				t.Fatalf("link for bob active: %v", resetActive(s, "for-bob", now))
			}
			if !resetActive(s, "for-frank", now) || !invitationActive(s, "inv-alice", now) {
				t.Fatal("unrelated links were revoked")
			}
		})
	}
}

// TestUsersConcurrency checks the rules under concurrent requests. SQLite
// serializes the transactions; PostgreSQL relies on the row locks.
func TestUsersConcurrency(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) { testUsersConcurrency(t, driver) })
	}
}

// race runs the functions at the same time and returns their errors.
func race(fns ...func() error) []error {
	errs := make([]error, len(fns))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, fn := range fns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs[i] = fn()
		}()
	}
	close(start)
	wg.Wait()
	return errs
}

func testUsersConcurrency(t *testing.T, driver string) {
	ctx := context.Background()
	s := userFixture(t, driver)
	now := int64(tokenTestNow)
	const rounds = 15

	// Two owners demoting each other never leave no owner: the second
	// finds that it is no longer an owner.
	for i := 0; i < rounds; i++ {
		errs := race(
			func() error {
				_, err := s.UpdateUser(ctx, "alice-id", "bob-id", store.UserChange{Role: store.RoleMember}, now)
				return err
			},
			func() error {
				_, err := s.UpdateUser(ctx, "bob-id", "alice-id", store.UserChange{Role: store.RoleMember}, now)
				return err
			},
		)
		ok, forbidden := 0, 0
		for _, err := range errs {
			switch {
			case err == nil:
				ok++
			case errors.Is(err, store.ErrForbidden):
				forbidden++
			default:
				t.Fatalf("round %d: %v", i, err)
			}
		}
		if ok != 1 || forbidden != 1 || enabledOwners(t, s) != 1 {
			t.Fatalf("round %d: %v owners=%d", i, errs, enabledOwners(t, s))
		}
		winner := "alice-id"
		if errs[0] != nil {
			winner = "bob-id"
		}
		for _, id := range []string{"alice-id", "bob-id"} {
			if _, err := s.UpdateUser(ctx, winner, id, store.UserChange{Role: store.RoleOwner}, now); err != nil {
				t.Fatal(err)
			}
		}
	}

	// Two owners disabling each other: one wins and the other was disabled
	// before it could act.
	for i := 0; i < rounds; i++ {
		a := addUser(t, s, fmt.Sprintf("a%d", i), store.RoleOwner)
		b := addUser(t, s, fmt.Sprintf("b%d", i), store.RoleOwner)
		errs := race(
			func() error {
				_, err := s.UpdateUser(ctx, a.ID, b.ID, store.UserChange{Disabled: ptr(true)}, now)
				return err
			},
			func() error {
				_, err := s.UpdateUser(ctx, b.ID, a.ID, store.UserChange{Disabled: ptr(true)}, now)
				return err
			},
		)
		if (errs[0] == nil) == (errs[1] == nil) || !errors.Is(errors.Join(errs...), store.ErrForbidden) {
			t.Fatalf("round %d: %v", i, errs)
		}
	}

	// An operator demoted while it invites: either the invitation came
	// first and the demotion revoked it, or the invitation is refused.
	for i := 0; i < rounds; i++ {
		id := fmt.Sprintf("race-inv-%d", i)
		errs := race(
			func() error {
				_, err := s.UpdateUser(ctx, "alice-id", "bob-id", store.UserChange{Role: store.RoleMember}, now)
				return err
			},
			func() error {
				return s.CreateInvitation(ctx, "bob-id", store.Invitation{ID: id, TokenHash: "inv-hash-" + id, Email: id + "@example.com", Role: store.RoleMember, CreatedAt: now, ExpiresAt: now + store.InvitationLifetime})
			},
		)
		if errs[0] != nil || (errs[1] != nil && !errors.Is(errs[1], store.ErrForbidden)) {
			t.Fatalf("round %d: %v", i, errs)
		}
		if invitationActive(s, id, now) {
			t.Fatalf("round %d: an invitation of a member is active", i)
		}
		if _, err := s.UpdateUser(ctx, "alice-id", "bob-id", store.UserChange{Role: store.RoleOwner}, now); err != nil {
			t.Fatal(err)
		}
	}

	// The same invitation accepted twice at once: one user is created.
	for i := 0; i < rounds; i++ {
		id := fmt.Sprintf("twice-%d", i)
		email := id + "@example.com"
		invite(t, s, "alice-id", id, email, store.RoleMember, now)
		errs := race(
			func() error {
				_, err := s.AcceptInvitation(ctx, "inv-hash-"+id, store.User{ID: id + "-1", PasswordHash: "h"}, now)
				return err
			},
			func() error {
				_, err := s.AcceptInvitation(ctx, "inv-hash-"+id, store.User{ID: id + "-2", PasswordHash: "h"}, now)
				return err
			},
		)
		if (errs[0] == nil) == (errs[1] == nil) || !errors.Is(errors.Join(errs...), store.ErrInvitationInvalid) {
			t.Fatalf("round %d: %v", i, errs)
		}
	}

	// An acceptance racing the creation of a user with the same email: one
	// of them fails with ErrConflict, and a failed acceptance keeps the
	// invitation.
	for i := 0; i < rounds; i++ {
		id := fmt.Sprintf("taken-%d", i)
		email := id + "@example.com"
		invite(t, s, "alice-id", id, email, store.RoleMember, now)
		errs := race(
			func() error {
				_, err := s.AcceptInvitation(ctx, "inv-hash-"+id, store.User{ID: id + "-invited", PasswordHash: "h"}, now)
				return err
			},
			func() error {
				return s.CreateUser(ctx, store.User{ID: id + "-cli", Email: email, PasswordHash: "h", Role: store.RoleOwner})
			},
		)
		if (errs[0] == nil) == (errs[1] == nil) || !errors.Is(errors.Join(errs...), store.ErrConflict) {
			t.Fatalf("round %d: %v", i, errs)
		}
		if errs[0] != nil && !invitationActive(s, id, now) {
			t.Fatalf("round %d: the failed acceptance used the invitation", i)
		}
	}

	// A reset completed while an owner disables the user: either the
	// disable came first and the link is invalid, or the reset completed
	// and the user was then disabled. Neither fails otherwise.
	for i := 0; i < rounds; i++ {
		u := addUser(t, s, fmt.Sprintf("r%d", i), store.RoleMember)
		id := fmt.Sprintf("complete-%d", i)
		issueReset(t, s, "alice-id", u.ID, id, now)
		errs := race(
			func() error {
				_, err := s.CompletePasswordReset(ctx, "pwr-hash-"+id, "new", now)
				return err
			},
			func() error {
				_, err := s.UpdateUser(ctx, "alice-id", u.ID, store.UserChange{Disabled: ptr(true)}, now)
				return err
			},
		)
		if (errs[0] != nil && !errors.Is(errs[0], store.ErrResetInvalid)) || errs[1] != nil {
			t.Fatalf("round %d: %v", i, errs)
		}
		if _, disabled := userRole(t, s, u.Email); disabled != 1 {
			t.Fatalf("round %d: not disabled", i)
		}
		if resetActive(s, id, now) {
			t.Fatalf("round %d: link active", i)
		}
	}
}
