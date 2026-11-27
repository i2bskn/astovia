package store_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"github.com/i2bskn/astovia/internal/store"
	"github.com/i2bskn/astovia/internal/testutil"
)

const (
	oidcIssuer      = "https://idp.example.com"
	otherOIDCIssuer = "https://other.example.com"
)

// oidcFixture is a database with alice (owner) and carol (member), neither
// linked to an OIDC identity, and a raw connection to it.
type oidcFixture struct {
	t   *testing.T
	s   *store.Store
	raw *sql.DB
}

func newOIDCFixture(t *testing.T, driver string) *oidcFixture {
	t.Helper()
	s, dsn := testutil.Database(t, driver)
	name := driver
	if driver == "postgres" {
		name = "pgx"
	}
	raw, err := sql.Open(name, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	for _, u := range []store.User{
		{ID: "alice-id", Email: "alice@example.com", PasswordHash: "alice-hash", CreatedAt: 1, Role: store.RoleOwner},
		{ID: "carol-id", Email: "carol@example.com", PasswordHash: store.NoPasswordHash, CreatedAt: 2, Role: store.RoleMember},
	} {
		if err = s.CreateUser(context.Background(), u); err != nil {
			t.Fatal(err)
		}
	}
	return &oidcFixture{t: t, s: s, raw: raw}
}

// insertInvitation stores an invitation by alice without the checks of
// CreateInvitation, so that several active ones to one email, one to the
// email of a user and an invalid role can be made. The values are test
// constants.
func (f *oidcFixture) insertInvitation(id, email, role string, createdAt, expiresAt int64) {
	f.t.Helper()
	q := fmt.Sprintf("INSERT INTO invitations (id,token_hash,email,role,invited_by,created_at,expires_at) VALUES ('%s','inv-hash-%s','%s','%s','alice-id',%d,%d)", id, id, email, role, createdAt, expiresAt)
	if _, err := f.raw.ExecContext(context.Background(), q); err != nil {
		f.t.Fatal(err)
	}
}

// invitation returns whether the invitation id is accepted or revoked, and
// the user that accepted it.
func (f *oidcFixture) invitation(id string) (accepted, revoked bool, userID string) {
	f.t.Helper()
	q := fmt.Sprintf("SELECT accepted_at IS NOT NULL, revoked_at IS NOT NULL, COALESCE(accepted_user_id,'') FROM invitations WHERE id = '%s'", id)
	if err := f.raw.QueryRowContext(context.Background(), q).Scan(&accepted, &revoked, &userID); err != nil {
		f.t.Fatal(err)
	}
	return accepted, revoked, userID
}

func (f *oidcFixture) users() int {
	f.t.Helper()
	users, err := f.s.ListUsers(context.Background())
	if err != nil {
		f.t.Fatal(err)
	}
	return len(users)
}

// linked reports whether u is linked to (issuer, subject).
func linked(u store.User, issuer, subject string) bool {
	return u.OidcIssuer == (sql.NullString{String: issuer, Valid: true}) && u.OidcSubject == (sql.NullString{String: subject, Valid: true})
}

func TestLinkOIDCUser(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		ctx := context.Background()
		f := newOIDCFixture(t, driver)
		s := f.s
		now := int64(tokenTestNow)
		end := now + store.InvitationLifetime
		link := func(issuer, subject, email, id string, at int64) (store.User, bool, error) {
			return s.LinkOIDCUser(ctx, issuer, subject, email, store.User{ID: id, PasswordHash: store.NoPasswordHash, CreatedAt: at}, at)
		}

		// An empty issuer or subject is rejected.
		for _, pair := range [][2]string{{"", "sub-1"}, {oidcIssuer, ""}, {"", ""}} {
			if _, err := s.UserByOIDC(ctx, pair[0], pair[1]); !errors.Is(err, store.ErrInvalidOIDCIdentity) {
				t.Fatalf("UserByOIDC %q: %v", pair, err)
			}
			if _, _, err := link(pair[0], pair[1], "carol@example.com", "x-id", now); !errors.Is(err, store.ErrInvalidOIDCIdentity) {
				t.Fatalf("LinkOIDCUser %q: %v", pair, err)
			}
		}
		if u, err := s.UserByEmail(ctx, "carol@example.com"); err != nil || u.OidcIssuer.Valid || u.OidcSubject.Valid {
			t.Fatalf("carol was linked: %+v %v", u, err)
		}
		if _, err := s.UserByOIDC(ctx, oidcIssuer, "sub-dave"); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("unknown identity: %v", err)
		}

		// Without an invitation nobody is created.
		if _, _, err := link(oidcIssuer, "sub-dave", "dave@example.com", "dave-id", now); !errors.Is(err, store.ErrNotInvited) {
			t.Fatalf("not invited: %v", err)
		}
		// Invitations that are not active, or whose role is invalid, do not
		// count: expires_at == now is expired.
		f.insertInvitation("expired", "dave@example.com", store.RoleMember, now-10, now)
		f.insertInvitation("bad-role", "erin@example.com", "admin", now, end)
		invite(t, s, "alice-id", "revoked", "frank@example.com", store.RoleMember, now)
		if err := s.RevokeInvitation(ctx, "alice-id", "revoked", now); err != nil {
			t.Fatal(err)
		}
		for sub, email := range map[string]string{"sub-dave": "dave@example.com", "sub-erin": "erin@example.com", "sub-frank": "frank@example.com"} {
			if _, _, err := link(oidcIssuer, sub, email, sub+"-id", now); !errors.Is(err, store.ErrNotInvited) {
				t.Fatalf("%s: %v", email, err)
			}
			if _, err := s.UserByEmail(ctx, email); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("%s was created: %v", email, err)
			}
		}
		if n := f.users(); n != 2 {
			t.Fatalf("users: %d", n)
		}
		if accepted, revoked, _ := f.invitation("bad-role"); accepted || revoked {
			t.Fatal("the invitation with an invalid role changed")
		}

		// An invitation creates the user with its email and role, linked, and
		// is accepted.
		for _, tc := range []struct{ name, role string }{{"dave", store.RoleMember}, {"gina", store.RoleOwner}} {
			email, sub := tc.name+"@example.com", "sub-"+tc.name
			invite(t, s, "alice-id", "inv-"+tc.name, email, tc.role, now)
			u, created, err := link(oidcIssuer, sub, email, tc.name+"-id", now+1)
			if err != nil || !created || u.ID != tc.name+"-id" || u.Email != email || u.Role != tc.role || u.Disabled != 0 || u.PasswordHash != store.NoPasswordHash || u.CreatedAt != now+1 || !linked(u, oidcIssuer, sub) {
				t.Fatalf("%s: %+v %v %v", tc.name, u, created, err)
			}
			got, err := s.UserByOIDC(ctx, oidcIssuer, sub)
			if err != nil || got != u {
				t.Fatalf("%s by identity: %+v %v", tc.name, got, err)
			}
			if got, err = s.UserByEmail(ctx, email); err != nil || got != u {
				t.Fatalf("%s by email: %+v %v", tc.name, got, err)
			}
			if accepted, revoked, by := f.invitation("inv-" + tc.name); !accepted || revoked || by != u.ID {
				t.Fatalf("%s: invitation %v %v %q", tc.name, accepted, revoked, by)
			}
			// The accepted invitation is not used again.
			if _, _, err = link(oidcIssuer, sub+"-2", email, tc.name+"-2", now+2); !errors.Is(err, store.ErrOIDCLinked) {
				t.Fatalf("%s again: %v", tc.name, err)
			}
		}
		// The same sub at another issuer is another identity.
		if _, err := s.UserByOIDC(ctx, otherOIDCIssuer, "sub-dave"); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("other issuer: %v", err)
		}
		// The subject is compared as it is.
		if _, err := s.UserByOIDC(ctx, oidcIssuer, "SUB-DAVE"); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("other case: %v", err)
		}

		// Of several active invitations the newest decides (the larger ID on
		// a tie) and the others are revoked.
		f.insertInvitation("h1", "hana@example.com", store.RoleOwner, now+5, end)
		f.insertInvitation("h3", "hana@example.com", store.RoleMember, now+6, end)
		f.insertInvitation("h2", "hana@example.com", store.RoleOwner, now+6, end)
		u, created, err := link(oidcIssuer, "sub-hana", "hana@example.com", "hana-id", now+7)
		if err != nil || !created || u.Role != store.RoleMember {
			t.Fatalf("hana: %+v %v %v", u, created, err)
		}
		for id, want := range map[string][2]bool{"h1": {false, true}, "h2": {false, true}, "h3": {true, false}} {
			if accepted, revoked, _ := f.invitation(id); accepted != want[0] || revoked != want[1] {
				t.Fatalf("%s: accepted=%v revoked=%v", id, accepted, revoked)
			}
		}

		// The email alone never links a user that exists: the operator's
		// approval is needed, until its deadline (exclusive).
		f.insertInvitation("for-alice", "alice@example.com", store.RoleMember, now, end)
		if _, _, err = link(oidcIssuer, "sub-alice", "alice@example.com", "unused-id", now+8); !errors.Is(err, store.ErrOIDCNotApproved) {
			t.Fatalf("not approved: %v", err)
		}
		if err = s.ApproveOIDCLink(ctx, "alice@example.com", now+8); err != nil {
			t.Fatal(err)
		}
		if _, _, err = link(oidcIssuer, "sub-alice", "alice@example.com", "unused-id", now+8); !errors.Is(err, store.ErrOIDCNotApproved) {
			t.Fatalf("approval at its deadline: %v", err)
		}
		if err = s.ApproveOIDCLink(ctx, "missing@example.com", now+100); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("approval of a missing user: %v", err)
		}
		if err = s.ApproveOIDCLink(ctx, "alice@example.com", now+100); err != nil {
			t.Fatal(err)
		}
		if got, err := s.UserByEmail(ctx, "alice@example.com"); err != nil || got.OidcLinkApprovedUntil != (sql.NullInt64{Int64: now + 100, Valid: true}) {
			t.Fatalf("approval: %+v %v", got, err)
		}
		// Approved, the user is linked without an invitation; its role and
		// password hash stay, the approval is used up, and the active
		// invitations to its email are revoked.
		u, created, err = link(oidcIssuer, "sub-alice", "alice@example.com", "unused-id", now+8)
		if err != nil || created || u.ID != "alice-id" || u.Role != store.RoleOwner || u.PasswordHash != "alice-hash" || u.CreatedAt != 1 || !linked(u, oidcIssuer, "sub-alice") || u.OidcLinkApprovedUntil.Valid {
			t.Fatalf("alice: %+v %v %v", u, created, err)
		}
		// A linked user cannot be approved.
		if err = s.ApproveOIDCLink(ctx, "alice@example.com", now+100); !errors.Is(err, store.ErrOIDCLinked) {
			t.Fatalf("approval of a linked user: %v", err)
		}
		if got, err := s.UserByEmail(ctx, "alice@example.com"); err != nil || got != u {
			t.Fatalf("alice by email: %+v %v", got, err)
		}
		if accepted, revoked, _ := f.invitation("for-alice"); accepted || !revoked {
			t.Fatalf("invitation to a linked user: accepted=%v revoked=%v", accepted, revoked)
		}
		// Another identity cannot take a linked user, whatever the issuer.
		for _, pair := range [][2]string{{oidcIssuer, "sub-mallory"}, {otherOIDCIssuer, "sub-alice"}} {
			if _, _, err = link(pair[0], pair[1], "alice@example.com", "mallory-id", now+9); !errors.Is(err, store.ErrOIDCLinked) {
				t.Fatalf("%q: %v", pair, err)
			}
		}
		if got, err := s.UserByEmail(ctx, "alice@example.com"); err != nil || !linked(got, oidcIssuer, "sub-alice") {
			t.Fatalf("alice's link changed: %+v %v", got, err)
		}
		// The identity that is already linked is a conflict: the caller reads
		// it again.
		if _, _, err = link(oidcIssuer, "sub-alice", "alice@example.com", "unused-id", now+9); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("same identity again: %v", err)
		}

		// A disabled user is not linked.
		if _, err = s.UpdateUser(ctx, "alice-id", "carol-id", store.UserChange{Disabled: ptr(true)}, now); err != nil {
			t.Fatal(err)
		}
		if _, _, err = link(oidcIssuer, "sub-carol", "carol@example.com", "unused-id", now+9); !errors.Is(err, store.ErrUserDisabled) {
			t.Fatalf("disabled: %v", err)
		}
		if got, err := s.UserByEmail(ctx, "carol@example.com"); err != nil || got.OidcIssuer.Valid || got.OidcSubject.Valid || got.Disabled == 0 {
			t.Fatalf("carol: %+v %v", got, err)
		}
	})
}

// The identity columns are both null or both set, and one identity belongs
// to one user.
func TestOIDCIdentityColumns(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		ctx := context.Background()
		f := newOIDCFixture(t, driver)
		s := f.s
		set := func(v string) sql.NullString { return sql.NullString{String: v, Valid: true} }
		for name, u := range map[string]store.User{
			"issuer only":   {OidcIssuer: set(oidcIssuer)},
			"subject only":  {OidcSubject: set("sub-1")},
			"empty issuer":  {OidcIssuer: set(""), OidcSubject: set("sub-1")},
			"empty subject": {OidcIssuer: set(oidcIssuer), OidcSubject: set("")},
		} {
			u.ID, u.Email, u.PasswordHash, u.Role = "x-id", "x@example.com", "x", store.RoleMember
			if err := s.CreateUser(ctx, u); !errors.Is(err, store.ErrInvalidOIDCIdentity) {
				t.Fatalf("%s: %v", name, err)
			}
		}
		// alice and carol are both unlinked: null identities do not collide.
		if n := f.users(); n != 2 {
			t.Fatalf("users: %d", n)
		}
		dave := store.User{ID: "dave-id", Email: "dave@example.com", PasswordHash: "x", Role: store.RoleMember, OidcIssuer: set(oidcIssuer), OidcSubject: set("sub-1")}
		if err := s.CreateUser(ctx, dave); err != nil {
			t.Fatal(err)
		}
		if err := s.CreateUser(ctx, store.User{ID: "erin-id", Email: "erin@example.com", PasswordHash: "x", Role: store.RoleMember, OidcIssuer: set(oidcIssuer), OidcSubject: set("sub-1")}); err == nil {
			t.Fatal("one identity was given to two users")
		}
		// The same subject at another issuer, and another subject, are fine.
		for i, pair := range [][2]string{{otherOIDCIssuer, "sub-1"}, {oidcIssuer, "sub-2"}} {
			u := store.User{ID: fmt.Sprintf("u%d-id", i), Email: fmt.Sprintf("u%d@example.com", i), PasswordHash: "x", Role: store.RoleMember, OidcIssuer: set(pair[0]), OidcSubject: set(pair[1])}
			if err := s.CreateUser(ctx, u); err != nil {
				t.Fatalf("%q: %v", pair, err)
			}
		}
		if got, err := s.UserByOIDC(ctx, oidcIssuer, "sub-1"); err != nil || got.ID != "dave-id" {
			t.Fatalf("%+v %v", got, err)
		}
		// Linking an unlinked user to an identity that is taken fails and
		// leaves the user unlinked.
		if err := s.ApproveOIDCLink(ctx, "carol@example.com", tokenTestNow+60); err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.LinkOIDCUser(ctx, oidcIssuer, "sub-1", "carol@example.com", store.User{ID: "unused-id", PasswordHash: store.NoPasswordHash}, tokenTestNow); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("taken identity: %v", err)
		}
		if got, err := s.UserByEmail(ctx, "carol@example.com"); err != nil || got.OidcIssuer.Valid || got.OidcSubject.Valid {
			t.Fatalf("carol: %+v %v", got, err)
		}
	})
}

func TestUnlinkOIDC(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		ctx := context.Background()
		f := newOIDCFixture(t, driver)
		s := f.s
		now := int64(tokenTestNow)
		link := func(subject, email string) error {
			_, _, err := s.LinkOIDCUser(ctx, oidcIssuer, subject, email, store.User{ID: "unused-id", PasswordHash: store.NoPasswordHash}, now)
			return err
		}
		approveAndLink := func(subject, email string) {
			t.Helper()
			if err := s.ApproveOIDCLink(ctx, email, now+60); err != nil {
				t.Fatal(err)
			}
			if err := link(subject, email); err != nil {
				t.Fatal(err)
			}
		}
		user := func(email string) store.User {
			t.Helper()
			u, err := s.UserByEmail(ctx, email)
			if err != nil {
				t.Fatal(err)
			}
			return u
		}
		unlinked := func(email string) bool {
			t.Helper()
			u := user(email)
			return !u.OidcIssuer.Valid && !u.OidcSubject.Valid
		}
		if err := s.UnlinkOIDC(ctx, "missing@example.com"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("missing user: %v", err)
		}
		for _, name := range []string{"alice", "carol"} {
			approveAndLink("sub-"+name, name+"@example.com")
			grantAccess(t, s, name)
		}
		// Unlinking one user leaves the others and revokes its access; a
		// user that is not linked is left as it is.
		for range 2 {
			if err := s.UnlinkOIDC(ctx, "alice@example.com"); err != nil {
				t.Fatal(err)
			}
		}
		if !unlinked("alice@example.com") || unlinked("carol@example.com") {
			t.Fatal("unlink of alice")
		}
		if got := accessOf(t, s, "alice-id"); got != (access{}) {
			t.Fatalf("alice's access after the unlink: %+v", got)
		}
		if got, want := accessOf(t, s, "carol-id"), (access{Sessions: 1, Tokens: 1, Codes: 1, Connections: 1, Ceremonies: 1}); got != want {
			t.Fatalf("carol's access after alice's unlink: %+v", got)
		}
		if _, err := s.UserByOIDC(ctx, oidcIssuer, "sub-alice"); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("old identity: %v", err)
		}
		// The role, the password hash and the disabled flag stay, and no
		// approval is given.
		if u := user("alice@example.com"); u.Role != store.RoleOwner || u.PasswordHash != "alice-hash" || u.Disabled != 0 || u.OidcLinkApprovedUntil.Valid {
			t.Fatalf("%+v", u)
		}
		if err := link("sub-alice-2", "alice@example.com"); !errors.Is(err, store.ErrOIDCNotApproved) {
			t.Fatalf("link again without an approval: %v", err)
		}
		// Approved, another identity links the user again.
		approveAndLink("sub-alice-2", "alice@example.com")
		if u, err := s.UserByOIDC(ctx, oidcIssuer, "sub-alice-2"); err != nil || u.ID != "alice-id" {
			t.Fatalf("%+v %v", u, err)
		}

		// Disabling clears an approval that was not used.
		if err := s.CreateUser(ctx, store.User{ID: "dave-id", Email: "dave@example.com", PasswordHash: "x", Role: store.RoleMember}); err != nil {
			t.Fatal(err)
		}
		if err := s.ApproveOIDCLink(ctx, "dave@example.com", now+60); err != nil {
			t.Fatal(err)
		}
		if _, err := s.UpdateUser(ctx, "alice-id", "dave-id", store.UserChange{Disabled: ptr(true)}, now); err != nil {
			t.Fatal(err)
		}
		if _, err := s.UpdateUser(ctx, "alice-id", "dave-id", store.UserChange{Disabled: ptr(false)}, now); err != nil {
			t.Fatal(err)
		}
		if u := user("dave@example.com"); u.OidcLinkApprovedUntil.Valid {
			t.Fatalf("dave's approval after disabling: %+v", u)
		}
		if err := link("sub-dave", "dave@example.com"); !errors.Is(err, store.ErrOIDCNotApproved) {
			t.Fatalf("dave: %v", err)
		}
		if err := s.ApproveOIDCLink(ctx, "dave@example.com", now+60); err != nil {
			t.Fatal(err)
		}
		if err := s.ChangeUser(ctx, "dave@example.com", "", true, now); err != nil {
			t.Fatal(err)
		}
		if u := user("dave@example.com"); u.OidcLinkApprovedUntil.Valid {
			t.Fatalf("dave's approval after the CLI disabled the user: %+v", u)
		}

		for i, want := range []int{2, 0} {
			n, err := s.UnlinkAllOIDC(ctx)
			if err != nil || n != want {
				t.Fatalf("unlink all %d: %d %v", i, n, err)
			}
		}
		if !unlinked("alice@example.com") || !unlinked("carol@example.com") || f.users() != 3 {
			t.Fatal("unlink of everyone")
		}
		for _, email := range []string{"alice@example.com", "carol@example.com"} {
			if u := user(email); u.OidcLinkApprovedUntil.Valid {
				t.Fatalf("%s got an approval: %+v", email, u)
			}
		}
		if got := accessOf(t, s, "carol-id"); got != (access{}) {
			t.Fatalf("carol's access after the unlink of everyone: %+v", got)
		}
	})
}

// TestCheckSchemaOIDC checks that a database without the identity columns
// of users is reported.
func TestCheckSchemaOIDC(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		for _, column := range []string{"oidc_issuer", "oidc_subject"} {
			f := newOIDCFixture(t, driver)
			for _, q := range []string{"DROP INDEX users_oidc_identity_key", "ALTER TABLE users DROP COLUMN " + column} {
				if _, err := f.raw.ExecContext(context.Background(), q); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.s.CheckSchema(context.Background()); err == nil {
				t.Fatalf("missing %s accepted", column)
			}
		}
	})
}

func TestLinkOIDCUserConcurrency(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		ctx := context.Background()
		f := newOIDCFixture(t, driver)
		s := f.s
		now := int64(tokenTestNow)
		const rounds, concurrent = 10, 6
		// count returns how many errors are nil and how many are want; any
		// other error fails the test.
		count := func(round int, errs []error, want error) (int, int) {
			t.Helper()
			ok, other := 0, 0
			for _, err := range errs {
				switch {
				case err == nil:
					ok++
				case errors.Is(err, want):
					other++
				default:
					t.Fatalf("round %d: %v", round, err)
				}
			}
			return ok, other
		}
		links := func(email string, subject func(i int) string) []error {
			fns := make([]func() error, concurrent)
			for i := range fns {
				fns[i] = func() error {
					_, _, err := s.LinkOIDCUser(ctx, oidcIssuer, subject(i), email, store.User{ID: fmt.Sprintf("%s-%d", email, i), PasswordHash: store.NoPasswordHash, CreatedAt: now}, now)
					return err
				}
			}
			return race(fns...)
		}
		users := f.users()
		for i := range rounds {
			// One identity, one invitation: one sign-in creates the user and
			// the others find it by the identity afterwards.
			email, sub := fmt.Sprintf("same%d@example.com", i), fmt.Sprintf("sub-same-%d", i)
			invite(t, s, "alice-id", fmt.Sprintf("inv-same-%d", i), email, store.RoleMember, now)
			if ok, conflicts := count(i, links(email, func(int) string { return sub }), store.ErrConflict); ok != 1 || conflicts != concurrent-1 {
				t.Fatalf("round %d: same identity: %d created, %d conflicts", i, ok, conflicts)
			}
			if u, err := s.UserByOIDC(ctx, oidcIssuer, sub); err != nil || u.Email != email {
				t.Fatalf("round %d: %+v %v", i, u, err)
			}

			// Several identities, one invitation: one gets the email address
			// and the others find no user of their identity.
			email = fmt.Sprintf("other%d@example.com", i)
			subject := func(n int) string { return fmt.Sprintf("sub-other-%d-%d", i, n) }
			invite(t, s, "alice-id", fmt.Sprintf("inv-other-%d", i), email, store.RoleMember, now)
			errs := links(email, subject)
			ok, rejected := 0, 0
			for n, err := range errs {
				_, found := s.UserByOIDC(ctx, oidcIssuer, subject(n))
				switch {
				case err == nil && found == nil:
					ok++
				case (errors.Is(err, store.ErrConflict) || errors.Is(err, store.ErrOIDCLinked)) && errors.Is(found, sql.ErrNoRows):
					rejected++
				default:
					t.Fatalf("round %d: %v, lookup %v", i, err, found)
				}
			}
			if ok != 1 || rejected != concurrent-1 {
				t.Fatalf("round %d: other identities: %d created, %d rejected", i, ok, rejected)
			}

			// Several identities, one approved user: one is linked.
			email = fmt.Sprintf("user%d@example.com", i)
			if err := s.CreateUser(ctx, store.User{ID: fmt.Sprintf("user%d-id", i), Email: email, PasswordHash: "x", Role: store.RoleMember}); err != nil {
				t.Fatal(err)
			}
			if err := s.ApproveOIDCLink(ctx, email, now+60); err != nil {
				t.Fatal(err)
			}
			if ok, taken := count(i, links(email, func(n int) string { return fmt.Sprintf("sub-user-%d-%d", i, n) }), store.ErrOIDCLinked); ok != 1 || taken != concurrent-1 {
				t.Fatalf("round %d: unlinked user: %d linked, %d rejected", i, ok, taken)
			}
			users += 3
			if n := f.users(); n != users {
				t.Fatalf("round %d: users: %d, want %d", i, n, users)
			}
		}
	})
}
