package store_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/i2bskn/astovia/internal/mfa"
	"github.com/i2bskn/astovia/internal/store"
	"github.com/i2bskn/astovia/internal/testutil"
)

// mfaCodes returns n recovery codes to store, named after prefix.
func mfaCodes(prefix string, n int) []store.RecoveryCode {
	out := make([]store.RecoveryCode, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, store.RecoveryCode{ID: fmt.Sprintf("%s-code-%d", prefix, i), Hash: fmt.Sprintf("%s-hash-%d", prefix, i)})
	}
	return out
}

func mfaStatus(t *testing.T, s *store.Store, userID string) store.MFAStatus {
	t.Helper()
	status, err := s.MFAStatus(context.Background(), userID)
	if err != nil {
		t.Fatal(err)
	}
	if status.WebAuthn == nil {
		t.Fatal("WebAuthn is nil")
	}
	return status
}

func totpOf(userID string, step int64) store.MFATOTP {
	return store.MFATOTP{UserID: userID, SecretSealed: "sealed-" + userID, LastStep: step, CreatedAt: tokenTestNow}
}

func credentialOf(userID, id string) store.WebAuthnCredential {
	return store.WebAuthnCredential{ID: id, UserID: userID, CredentialID: "key-" + id, Credential: `{"counter":0}`, Name: "Key " + id, CreatedAt: tokenTestNow}
}

func TestMFAEnroll(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		ctx := context.Background()
		s := userFixture(t, driver)

		if status := mfaStatus(t, s, "alice-id"); status.Enrolled() || status.TOTP != nil || len(status.WebAuthn) != 0 || status.RecoveryCodes != 0 {
			t.Fatalf("new user: %+v", status)
		}
		if status := mfaStatus(t, s, "missing-id"); status.Enrolled() {
			t.Fatalf("missing user: %+v", status)
		}

		// The first factor issues the recovery codes.
		issued, err := s.EnrollTOTP(ctx, totpOf("alice-id", 100), mfaCodes("alice", 10), true)
		if err != nil || !issued {
			t.Fatalf("first factor: %v %v", issued, err)
		}
		status := mfaStatus(t, s, "alice-id")
		if !status.Enrolled() || status.TOTP == nil || *status.TOTP != totpOf("alice-id", 100) || status.RecoveryCodes != 10 {
			t.Fatalf("after the first factor: %+v", status)
		}
		// A user with a factor cannot enroll before the first session.
		if _, err = s.EnrollTOTP(ctx, totpOf("alice-id", 200), mfaCodes("again", 10), true); !errors.Is(err, store.ErrMFAEnrolled) {
			t.Fatalf("second enrollment before a session: %v", err)
		}
		if _, err = s.EnrollWebAuthn(ctx, credentialOf("alice-id", "c0"), mfaCodes("again", 10), true); !errors.Is(err, store.ErrMFAEnrolled) {
			t.Fatalf("WebAuthn enrollment before a session: %v", err)
		}
		if got := mfaStatus(t, s, "alice-id"); !reflect.DeepEqual(got, status) {
			t.Fatalf("rejected enrollments changed the user: %+v", got)
		}

		// Signed in, the authenticator app is replaced and the codes are kept.
		replacement := store.MFATOTP{UserID: "alice-id", SecretSealed: "sealed-2", LastStep: 300, CreatedAt: tokenTestNow + 5}
		if issued, err = s.EnrollTOTP(ctx, replacement, mfaCodes("replaced", 10), false); err != nil || issued {
			t.Fatalf("replacement: %v %v", issued, err)
		}
		if got := mfaStatus(t, s, "alice-id"); *got.TOTP != replacement || got.RecoveryCodes != 10 {
			t.Fatalf("after the replacement: %+v", got)
		}
		if ok, err := s.UseRecoveryCode(ctx, "alice-id", "replaced-hash-0", tokenTestNow); err != nil || ok {
			t.Fatalf("a code of the replacement was stored: %v %v", ok, err)
		}

		// A second factor issues no codes.
		if issued, err = s.EnrollWebAuthn(ctx, credentialOf("alice-id", "c1"), mfaCodes("second", 10), false); err != nil || issued {
			t.Fatalf("second factor: %v %v", issued, err)
		}
		got := mfaStatus(t, s, "alice-id")
		if len(got.WebAuthn) != 1 || got.WebAuthn[0] != credentialOf("alice-id", "c1") || got.RecoveryCodes != 10 {
			t.Fatalf("after the second factor: %+v", got)
		}

		// WebAuthn as the first factor issues the codes as well.
		if issued, err = s.EnrollWebAuthn(ctx, credentialOf("bob-id", "b1"), mfaCodes("bob", 10), true); err != nil || !issued {
			t.Fatalf("WebAuthn first factor: %v %v", issued, err)
		}
		if got = mfaStatus(t, s, "bob-id"); !got.Enrolled() || got.TOTP != nil || len(got.WebAuthn) != 1 || got.RecoveryCodes != 10 {
			t.Fatalf("bob: %+v", got)
		}
		// A credential ID registered by anyone is rejected.
		dup := credentialOf("bob-id", "b2")
		dup.CredentialID = "key-b1"
		if _, err = s.EnrollWebAuthn(ctx, dup, nil, false); !errors.Is(err, store.ErrMFACredentialExists) {
			t.Fatalf("own duplicate: %v", err)
		}
		dup = credentialOf("carol-id", "x1")
		dup.CredentialID = "key-b1"
		if _, err = s.EnrollWebAuthn(ctx, dup, mfaCodes("carol", 10), true); !errors.Is(err, store.ErrMFACredentialExists) {
			t.Fatalf("duplicate of another user: %v", err)
		}
		if got = mfaStatus(t, s, "carol-id"); got.Enrolled() || got.RecoveryCodes != 0 {
			t.Fatalf("a rejected credential left rows: %+v", got)
		}
		// The limit is 10 credentials.
		for i := 2; i <= store.MaxWebAuthnCredentials; i++ {
			if _, err = s.EnrollWebAuthn(ctx, credentialOf("bob-id", fmt.Sprintf("b%d", i)), nil, false); err != nil {
				t.Fatalf("credential %d: %v", i, err)
			}
		}
		if _, err = s.EnrollWebAuthn(ctx, credentialOf("bob-id", "b11"), nil, false); !errors.Is(err, store.ErrMFALimit) {
			t.Fatalf("11th credential: %v", err)
		}
		if got = mfaStatus(t, s, "bob-id"); len(got.WebAuthn) != store.MaxWebAuthnCredentials || store.MaxWebAuthnCredentials != 10 {
			t.Fatalf("bob: %d credentials", len(got.WebAuthn))
		}

		// A disabled or missing user enrolls nothing.
		if err = s.ChangeUser(ctx, "carol@example.com", "", true, tokenTestNow); err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{"carol-id", "missing-id"} {
			if _, err = s.EnrollTOTP(ctx, totpOf(id, 1), nil, false); !errors.Is(err, store.ErrUnauthorized) {
				t.Fatalf("%s: TOTP: %v", id, err)
			}
			if _, err = s.EnrollWebAuthn(ctx, credentialOf(id, "z-"+id), nil, false); !errors.Is(err, store.ErrUnauthorized) {
				t.Fatalf("%s: WebAuthn: %v", id, err)
			}
		}

		enabled, err := s.MFAEnabledUserIDs(ctx)
		if err != nil || !reflect.DeepEqual(enabled, map[string]bool{"alice-id": true, "bob-id": true}) {
			t.Fatalf("enabled: %v %v", enabled, err)
		}
	})
}

func TestMFAUseCodes(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		ctx := context.Background()
		s := userFixture(t, driver)
		for _, name := range []string{"alice", "bob"} {
			if _, err := s.EnrollTOTP(ctx, totpOf(name+"-id", 100), mfaCodes(name, 3), true); err != nil {
				t.Fatal(err)
			}
		}
		step := func(userID string, step int64, want bool) {
			t.Helper()
			ok, err := s.UseTOTPStep(ctx, userID, step)
			if err != nil || ok != want {
				t.Fatalf("step %d of %s: %v %v, want %v", step, userID, ok, err, want)
			}
		}
		// The step of the enrollment and the ones before it are spent.
		step("alice-id", 100, false)
		step("alice-id", 99, false)
		step("alice-id", 101, true)
		step("alice-id", 101, false)
		// A later step spends the ones before it.
		step("alice-id", 103, true)
		step("alice-id", 102, false)
		step("carol-id", 500, false)
		if got := mfaStatus(t, s, "alice-id").TOTP.LastStep; got != 103 {
			t.Fatalf("last step: %d", got)
		}
		if got := mfaStatus(t, s, "bob-id").TOTP.LastStep; got != 100 {
			t.Fatalf("bob's last step: %d", got)
		}

		code := func(userID, hash string, want bool) {
			t.Helper()
			ok, err := s.UseRecoveryCode(ctx, userID, hash, tokenTestNow)
			if err != nil || ok != want {
				t.Fatalf("code %s of %s: %v %v, want %v", hash, userID, ok, err, want)
			}
		}
		code("alice-id", "alice-hash-0", true)
		code("alice-id", "alice-hash-0", false)
		code("alice-id", "bob-hash-0", false)
		code("alice-id", "unknown", false)
		code("bob-id", "alice-hash-1", false)
		if a, b := mfaStatus(t, s, "alice-id").RecoveryCodes, mfaStatus(t, s, "bob-id").RecoveryCodes; a != 2 || b != 3 {
			t.Fatalf("remaining: %d %d", a, b)
		}

		// New codes replace the old ones, used or not.
		if err := s.ReplaceRecoveryCodes(ctx, "alice-id", mfaCodes("new", 10), tokenTestNow+1); err != nil {
			t.Fatal(err)
		}
		code("alice-id", "alice-hash-1", false)
		if got := mfaStatus(t, s, "alice-id").RecoveryCodes; got != 10 {
			t.Fatalf("after the replacement: %d", got)
		}
		code("alice-id", "new-hash-0", true)
		if got := mfaStatus(t, s, "bob-id").RecoveryCodes; got != 3 {
			t.Fatalf("bob after alice's replacement: %d", got)
		}
		if err := s.ReplaceRecoveryCodes(ctx, "carol-id", mfaCodes("carol", 10), tokenTestNow); !errors.Is(err, store.ErrMFANotEnrolled) {
			t.Fatalf("codes without a factor: %v", err)
		}
		if got := mfaStatus(t, s, "carol-id").RecoveryCodes; got != 0 {
			t.Fatalf("carol: %d", got)
		}
		if err := s.ReplaceRecoveryCodes(ctx, "missing-id", nil, tokenTestNow); !errors.Is(err, store.ErrUnauthorized) {
			t.Fatalf("codes of a missing user: %v", err)
		}
	})
}

func TestMFADelete(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		ctx := context.Background()
		s := userFixture(t, driver)
		// alice (owner): an authenticator app and two credentials.
		if _, err := s.EnrollTOTP(ctx, totpOf("alice-id", 1), mfaCodes("alice", 10), true); err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{"a1", "a2"} {
			if _, err := s.EnrollWebAuthn(ctx, credentialOf("alice-id", id), nil, false); err != nil {
				t.Fatal(err)
			}
		}
		// carol (member): one credential. bob (owner): nothing.
		if _, err := s.EnrollWebAuthn(ctx, credentialOf("carol-id", "c1"), mfaCodes("carol", 10), true); err != nil {
			t.Fatal(err)
		}

		if err := s.DeleteTOTP(ctx, "bob-id", mfa.PolicyOff); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("no authenticator app: %v", err)
		}
		// Credentials of another user and unknown ones are not found.
		for _, id := range []string{"c1", "missing"} {
			if err := s.DeleteWebAuthnCredential(ctx, "alice-id", id, mfa.PolicyOff); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("credential %s as alice: %v", id, err)
			}
		}
		if got := mfaStatus(t, s, "carol-id"); len(got.WebAuthn) != 1 {
			t.Fatalf("carol: %+v", got)
		}

		// While other factors are left, a required user may remove one, and
		// the recovery codes stay.
		if err := s.DeleteWebAuthnCredential(ctx, "alice-id", "a1", mfa.PolicyAll); err != nil {
			t.Fatal(err)
		}
		if err := s.DeleteTOTP(ctx, "alice-id", mfa.PolicyOwners); err != nil {
			t.Fatal(err)
		}
		if got := mfaStatus(t, s, "alice-id"); got.TOTP != nil || len(got.WebAuthn) != 1 || got.WebAuthn[0].ID != "a2" || got.RecoveryCodes != 10 {
			t.Fatalf("alice with one factor: %+v", got)
		}
		// The last factor of a required user stays.
		for _, policy := range []string{mfa.PolicyOwners, mfa.PolicyAll} {
			if err := s.DeleteWebAuthnCredential(ctx, "alice-id", "a2", policy); !errors.Is(err, store.ErrMFARequired) {
				t.Fatalf("last factor of an owner under %s: %v", policy, err)
			}
		}
		if err := s.DeleteWebAuthnCredential(ctx, "carol-id", "c1", mfa.PolicyAll); !errors.Is(err, store.ErrMFARequired) {
			t.Fatalf("last factor of a member under all: %v", err)
		}
		if a, c := mfaStatus(t, s, "alice-id"), mfaStatus(t, s, "carol-id"); len(a.WebAuthn) != 1 || a.RecoveryCodes != 10 || len(c.WebAuthn) != 1 || c.RecoveryCodes != 10 {
			t.Fatalf("rejected removals changed the users: %+v %+v", a, c)
		}
		// A member is not required under owners; the codes go with the last
		// factor.
		if err := s.DeleteWebAuthnCredential(ctx, "carol-id", "c1", mfa.PolicyOwners); err != nil {
			t.Fatal(err)
		}
		if got := mfaStatus(t, s, "carol-id"); got.Enrolled() || got.RecoveryCodes != 0 {
			t.Fatalf("carol without factors: %+v", got)
		}
		// The role is the one at the time of the removal: a demoted owner
		// may remove the last factor.
		if _, err := s.UpdateUser(ctx, "bob-id", "alice-id", store.UserChange{Role: store.RoleMember}, tokenTestNow); err != nil {
			t.Fatal(err)
		}
		if err := s.DeleteWebAuthnCredential(ctx, "alice-id", "a2", mfa.PolicyOwners); err != nil {
			t.Fatalf("demoted owner: %v", err)
		}
		if got := mfaStatus(t, s, "alice-id"); got.Enrolled() || got.RecoveryCodes != 0 {
			t.Fatalf("alice without factors: %+v", got)
		}

		// The last authenticator app follows the same rules.
		if _, err := s.EnrollTOTP(ctx, totpOf("bob-id", 1), mfaCodes("bob", 10), true); err != nil {
			t.Fatal(err)
		}
		if err := s.DeleteTOTP(ctx, "bob-id", mfa.PolicyOwners); !errors.Is(err, store.ErrMFARequired) {
			t.Fatalf("last authenticator app of an owner: %v", err)
		}
		if got := mfaStatus(t, s, "bob-id"); got.TOTP == nil || got.RecoveryCodes != 10 {
			t.Fatalf("bob after the rejected removal: %+v", got)
		}
		if err := s.DeleteTOTP(ctx, "bob-id", ""); err != nil {
			t.Fatal(err)
		}
		if got := mfaStatus(t, s, "bob-id"); got.Enrolled() || got.RecoveryCodes != 0 {
			t.Fatalf("bob without factors: %+v", got)
		}
		if err := s.DeleteTOTP(ctx, "missing-id", ""); !errors.Is(err, store.ErrUnauthorized) {
			t.Fatalf("missing user: %v", err)
		}
	})
}

func TestMFAUpdateWebAuthnCredential(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		ctx := context.Background()
		s, dsn := mfaFixture(t, driver)
		for _, c := range []store.WebAuthnCredential{credentialOf("alice-id", "a1"), credentialOf("bob-id", "b1")} {
			if _, err := s.EnrollWebAuthn(ctx, c, nil, false); err != nil {
				t.Fatal(err)
			}
		}
		now := int64(tokenTestNow)
		credential := func(userID string) store.WebAuthnCredential {
			t.Helper()
			return mfaStatus(t, s, userID).WebAuthn[0]
		}

		if err := s.UpdateWebAuthnCredential(ctx, "alice-id", "key-a1", `{"counter":1}`, "challenge-1", now+300, now); err != nil {
			t.Fatal(err)
		}
		if got := credential("alice-id"); got.Credential != `{"counter":1}` || got.LastUsedAt != (sql.NullInt64{Int64: now, Valid: true}) {
			t.Fatalf("after the sign-in: %+v", got)
		}
		// The same challenge is spent, for any user and credential, and
		// nothing is updated.
		if err := s.UpdateWebAuthnCredential(ctx, "alice-id", "key-a1", `{"counter":2}`, "challenge-1", now+300, now+10); !errors.Is(err, store.ErrMFAChallengeUsed) {
			t.Fatalf("replay: %v", err)
		}
		if err := s.UpdateWebAuthnCredential(ctx, "bob-id", "key-b1", `{"counter":2}`, "challenge-1", now+300, now+10); !errors.Is(err, store.ErrMFAChallengeUsed) {
			t.Fatalf("replay as bob: %v", err)
		}
		if a, b := credential("alice-id"), credential("bob-id"); a.Credential != `{"counter":1}` || a.LastUsedAt.Int64 != now || b.Credential != `{"counter":0}` || b.LastUsedAt.Valid {
			t.Fatalf("a replay changed the credentials: %+v %+v", a, b)
		}
		// Another user's credential, an unknown one and a missing or
		// disabled user spend no challenge.
		if err := s.UpdateWebAuthnCredential(ctx, "alice-id", "key-b1", `{}`, "challenge-2", now+300, now); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("credential of another user: %v", err)
		}
		if err := s.UpdateWebAuthnCredential(ctx, "alice-id", "missing", `{}`, "challenge-2", now+300, now); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("unknown credential: %v", err)
		}
		if err := s.UpdateWebAuthnCredential(ctx, "missing-id", "key-a1", `{}`, "challenge-2", now+300, now); !errors.Is(err, store.ErrUnauthorized) {
			t.Fatalf("missing user: %v", err)
		}
		if err := s.ChangeUser(ctx, "bob@example.com", "", true, now); err != nil {
			t.Fatal(err)
		}
		if err := s.UpdateWebAuthnCredential(ctx, "bob-id", "key-b1", `{}`, "challenge-2", now+300, now); !errors.Is(err, store.ErrUnauthorized) {
			t.Fatalf("disabled user: %v", err)
		}
		if got := credential("bob-id"); got.Credential != `{"counter":0}` || got.LastUsedAt.Valid {
			t.Fatalf("bob: %+v", got)
		}
		if err := s.UpdateWebAuthnCredential(ctx, "alice-id", "key-a1", `{"counter":2}`, "challenge-2", now+300, now+20); err != nil {
			t.Fatalf("the challenge of the rejected attempts was spent: %v", err)
		}

		// A spent challenge is kept until its expiry, and removed at it: the
		// state of the sign-in is rejected from the same second on.
		if err := s.UpdateWebAuthnCredential(ctx, "alice-id", "key-a1", `{"counter":3}`, "challenge-1", now+300, now+299); !errors.Is(err, store.ErrMFAChallengeUsed) {
			t.Fatalf("replay before the expiry: %v", err)
		}
		if got := usedChallenges(t, driver, dsn); got != 2 {
			t.Fatalf("spent challenges: %d", got)
		}
		if err := s.UpdateWebAuthnCredential(ctx, "alice-id", "key-a1", `{"counter":3}`, "challenge-3", now+600, now+300); err != nil {
			t.Fatal(err)
		}
		if got := usedChallenges(t, driver, dsn); got != 1 {
			t.Fatalf("spent challenges after the expiry: %d", got)
		}
		if got := credential("alice-id"); got.Credential != `{"counter":3}` || got.LastUsedAt.Int64 != now+300 {
			t.Fatalf("alice: %+v", got)
		}

		// Two sign-ins with the same challenge at once: one wins.
		for i := 0; i < 10; i++ {
			hash := fmt.Sprintf("race-%d", i)
			errs := race(
				func() error {
					return s.UpdateWebAuthnCredential(ctx, "alice-id", "key-a1", `{"counter":9}`, hash, now+900, now+400)
				},
				func() error {
					return s.UpdateWebAuthnCredential(ctx, "alice-id", "key-a1", `{"counter":9}`, hash, now+900, now+400)
				},
			)
			ok, used := 0, 0
			for _, err := range errs {
				switch {
				case err == nil:
					ok++
				case errors.Is(err, store.ErrMFAChallengeUsed):
					used++
				default:
					t.Fatalf("round %d: %v", i, err)
				}
			}
			if ok != 1 || used != 1 {
				t.Fatalf("round %d: %d succeeded, %d rejected", i, ok, used)
			}
		}
	})
}

// mfaFixture is userFixture (alice and bob the owners, carol the member)
// with the DSN of its database.
func mfaFixture(t *testing.T, driver string) (*store.Store, string) {
	t.Helper()
	s, dsn := testutil.Database(t, driver)
	addUser(t, s, "alice", store.RoleOwner)
	addUser(t, s, "bob", store.RoleOwner)
	addUser(t, s, "carol", store.RoleMember)
	return s, dsn
}

// usedChallenges counts the rows of mfa_used_challenges.
func usedChallenges(t *testing.T, driver, dsn string) int {
	t.Helper()
	name := driver
	if driver == "postgres" {
		name = "pgx"
	}
	raw, err := sql.Open(name, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	var n int
	if err = raw.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM mfa_used_challenges").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestMFAConcurrentEnrollment(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		ctx := context.Background()
		s := userFixture(t, driver)
		// Two enrollments before the first session at once: one factor and
		// one set of codes.
		for i := 0; i < 10; i++ {
			errs := race(
				func() error {
					_, err := s.EnrollTOTP(ctx, totpOf("alice-id", 1), mfaCodes(fmt.Sprintf("t%d", i), 10), true)
					return err
				},
				func() error {
					_, err := s.EnrollWebAuthn(ctx, credentialOf("alice-id", fmt.Sprintf("w%d", i)), mfaCodes(fmt.Sprintf("w%d", i), 10), true)
					return err
				},
				func() error {
					_, err := s.EnrollTOTP(ctx, totpOf("alice-id", 2), mfaCodes(fmt.Sprintf("u%d", i), 10), true)
					return err
				},
			)
			ok := 0
			for _, err := range errs {
				switch {
				case err == nil:
					ok++
				case errors.Is(err, store.ErrMFAEnrolled):
				default:
					t.Fatalf("round %d: %v", i, err)
				}
			}
			status := mfaStatus(t, s, "alice-id")
			factors := len(status.WebAuthn)
			if status.TOTP != nil {
				factors++
			}
			if ok != 1 || factors != 1 || status.RecoveryCodes != 10 {
				t.Fatalf("round %d: %d succeeded, %d factors, %d codes", i, ok, factors, status.RecoveryCodes)
			}
			if err := s.ResetMFA(ctx, "alice@example.com"); err != nil {
				t.Fatal(err)
			}
		}

		// The same step and the same recovery code at once: one wins.
		if _, err := s.EnrollTOTP(ctx, totpOf("alice-id", 1), mfaCodes("alice", 10), true); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 10; i++ {
			results := make([]bool, 4)
			errs := race(
				func() (err error) { results[0], err = s.UseTOTPStep(ctx, "alice-id", int64(10+i)); return },
				func() (err error) { results[1], err = s.UseTOTPStep(ctx, "alice-id", int64(10+i)); return },
				func() (err error) {
					results[2], err = s.UseRecoveryCode(ctx, "alice-id", fmt.Sprintf("alice-hash-%d", i), tokenTestNow)
					return
				},
				func() (err error) {
					results[3], err = s.UseRecoveryCode(ctx, "alice-id", fmt.Sprintf("alice-hash-%d", i), tokenTestNow)
					return
				},
			)
			for _, err := range errs {
				if err != nil {
					t.Fatalf("round %d: %v", i, err)
				}
			}
			if results[0] == results[1] || results[2] == results[3] {
				t.Fatalf("round %d: %v", i, results)
			}
		}

		if err := s.ReplaceRecoveryCodes(ctx, "alice-id", mfaCodes("fresh", 10), tokenTestNow); err != nil {
			t.Fatal(err)
		}
		// Two removals of the two factors of a required user at once: one
		// factor stays.
		for i := 0; i < 10; i++ {
			id := fmt.Sprintf("r%d", i)
			if _, err := s.EnrollWebAuthn(ctx, credentialOf("alice-id", id), nil, false); err != nil {
				t.Fatal(err)
			}
			errs := race(
				func() error { return s.DeleteTOTP(ctx, "alice-id", mfa.PolicyAll) },
				func() error { return s.DeleteWebAuthnCredential(ctx, "alice-id", id, mfa.PolicyAll) },
			)
			ok, required := 0, 0
			for _, err := range errs {
				switch {
				case err == nil:
					ok++
				case errors.Is(err, store.ErrMFARequired):
					required++
				default:
					t.Fatalf("round %d: %v", i, err)
				}
			}
			status := mfaStatus(t, s, "alice-id")
			if ok != 1 || required != 1 || !status.Enrolled() || status.RecoveryCodes == 0 {
				t.Fatalf("round %d: %d removed, %d rejected, %+v", i, ok, required, status)
			}
			// Back to an authenticator app alone for the next round.
			if status.TOTP == nil {
				if _, err := s.EnrollTOTP(ctx, totpOf("alice-id", 1), nil, false); err != nil {
					t.Fatal(err)
				}
				if err := s.DeleteWebAuthnCredential(ctx, "alice-id", id, mfa.PolicyAll); err != nil {
					t.Fatal(err)
				}
			}
		}
	})
}

// TestMFAKeptAndReset checks that a password reset and disabling keep the
// second factors, and that ResetMFA removes those of one user alone.
func TestMFAKeptAndReset(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		ctx := context.Background()
		s, dsn := mfaFixture(t, driver)
		for _, name := range []string{"alice", "bob"} {
			if _, err := s.EnrollTOTP(ctx, totpOf(name+"-id", 1), mfaCodes(name, 10), true); err != nil {
				t.Fatal(err)
			}
			if _, err := s.EnrollWebAuthn(ctx, credentialOf(name+"-id", name+"-1"), nil, false); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.UpdateWebAuthnCredential(ctx, "alice-id", "key-alice-1", `{"counter":1}`, "challenge", tokenTestNow+300, tokenTestNow); err != nil {
			t.Fatal(err)
		}
		before := mfaStatus(t, s, "alice-id")

		// The password reset of the CLI, the one of a reset link and
		// disabling and enabling again keep everything.
		if err := s.ChangeUser(ctx, "alice@example.com", "new-hash", false, tokenTestNow); err != nil {
			t.Fatal(err)
		}
		issueReset(t, s, "bob-id", "alice-id", "pwr-1", tokenTestNow)
		if _, err := s.CompletePasswordReset(ctx, "pwr-hash-pwr-1", "newer-hash", tokenTestNow+1); err != nil {
			t.Fatal(err)
		}
		if _, err := s.UpdateUser(ctx, "bob-id", "alice-id", store.UserChange{Disabled: ptr(true)}, tokenTestNow+2); err != nil {
			t.Fatal(err)
		}
		if got := mfaStatus(t, s, "alice-id"); !reflect.DeepEqual(got, before) {
			t.Fatalf("disabled: %+v, want %+v", got, before)
		}
		if _, err := s.UpdateUser(ctx, "bob-id", "alice-id", store.UserChange{Disabled: ptr(false)}, tokenTestNow+3); err != nil {
			t.Fatal(err)
		}
		if got := mfaStatus(t, s, "alice-id"); !reflect.DeepEqual(got, before) {
			t.Fatalf("after the resets: %+v, want %+v", got, before)
		}

		if err := s.ResetMFA(ctx, "missing@example.com"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("missing user: %v", err)
		}
		bob := mfaStatus(t, s, "bob-id")
		for range 2 {
			if err := s.ResetMFA(ctx, "alice@example.com"); err != nil {
				t.Fatal(err)
			}
		}
		if got := mfaStatus(t, s, "alice-id"); got.Enrolled() || got.RecoveryCodes != 0 {
			t.Fatalf("alice after ResetMFA: %+v", got)
		}
		if got := mfaStatus(t, s, "bob-id"); !reflect.DeepEqual(got, bob) || !got.Enrolled() || got.RecoveryCodes != 10 {
			t.Fatalf("bob after alice's ResetMFA: %+v", got)
		}
		// The spent challenges are not the user's and stay.
		if got := usedChallenges(t, driver, dsn); got != 1 {
			t.Fatalf("spent challenges: %d", got)
		}
		// Sessions are kept: bob's still authenticates.
		if _, err := s.Authenticated(ctx, "bob-id", "bob-session", tokenTestNow); err != nil {
			t.Fatalf("bob's session: %v", err)
		}
		enabled, err := s.MFAEnabledUserIDs(ctx)
		if err != nil || !reflect.DeepEqual(enabled, map[string]bool{"bob-id": true}) {
			t.Fatalf("enabled: %v %v", enabled, err)
		}
	})
}

// TestCheckSchemaMFA checks that a database without one of the MFA tables
// is reported.
func TestCheckSchemaMFA(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		for _, table := range []string{"mfa_totp", "mfa_webauthn_credentials", "mfa_recovery_codes", "mfa_used_challenges"} {
			s, dsn := testutil.Database(t, driver)
			name := driver
			if driver == "postgres" {
				name = "pgx"
			}
			raw, err := sql.Open(name, dsn)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = raw.ExecContext(context.Background(), "DROP TABLE "+table); err != nil {
				t.Fatal(err)
			}
			if err = raw.Close(); err != nil {
				t.Fatal(err)
			}
			if err = s.CheckSchema(context.Background()); err == nil {
				t.Fatalf("missing %s accepted", table)
			}
		}
	})
}
