package store_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/i2bskn/authlume/mcpoauth"
	"github.com/i2bskn/authlume/webauthn"

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

// passwordDigest is auth.Hash of a stored password hash.
func passwordDigest(hash string) string {
	h := sha256.Sum256([]byte(hash))
	return hex.EncodeToString(h[:])
}

func totpOf(userID string, step int64) store.MFATOTP {
	return store.MFATOTP{ID: "totp-" + userID, UserID: userID, SecretSealed: "sealed-" + userID, LastStep: step, CreatedAt: tokenTestNow}
}

// first is the enrollment before the first session: the password digest of
// the fixtures' password hash "x" and no usable factor.
var first = store.EnrollOptions{OnlyIfNone: true, PasswordDigest: passwordDigest("x"), Now: tokenTestNow}

// signedIn is the enrollment of the settings screen of name.
func signedIn(name string) store.EnrollOptions {
	return store.EnrollOptions{SessionID: name + "-session", Now: tokenTestNow}
}

func keyOf(id string) webauthn.Credential {
	return webauthn.Credential{ID: []byte("key-" + id), PublicKey: []byte("public-" + id)}
}

// enrollKey inserts the credential id for userID through the credential
// store, as FinishRegistration does. Without a session or a password digest
// in e, it uses the session of the fixtures.
func enrollKey(s *store.Store, userID, id string, e store.WebAuthnEnrollment) error {
	e.UserID = userID
	if e.Name == "" {
		e.Name = "Key " + id
	}
	if e.Now == 0 {
		e.Now = tokenTestNow
	}
	if e.SessionID == "" && e.PasswordDigest == "" {
		e.SessionID = strings.TrimSuffix(userID, "-id") + "-session"
	}
	return s.WebAuthnCredentials(time.Now).Insert(store.WithWebAuthnEnrollment(context.Background(), e), userID, keyOf(id), false)
}

// legacyTOTP and legacyCredential store second factors as they were stored
// before authlume: no ID and no version.
func legacyTOTP(t *testing.T, s *store.Store, userID string) {
	t.Helper()
	if err := s.ExecForTest(context.Background(), fmt.Sprintf("INSERT INTO mfa_totp (user_id,secret_sealed,last_step,created_at) VALUES ('%s','legacy',5,%d)", userID, tokenTestNow)); err != nil {
		t.Fatal(err)
	}
}

func legacyCredential(t *testing.T, s *store.Store, userID, id string) {
	t.Helper()
	if err := s.ExecForTest(context.Background(), fmt.Sprintf("INSERT INTO mfa_webauthn_credentials (id,user_id,credential_id,credential,name,created_at) VALUES ('%s','%s','legacy-%s','{}','Legacy %s',%d)", id, userID, id, id, tokenTestNow)); err != nil {
		t.Fatal(err)
	}
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

		// An authenticator app needs an ID, and the conditions are checked.
		noID := totpOf("alice-id", 100)
		noID.ID = ""
		if _, err := s.EnrollTOTP(ctx, noID, nil, first); err == nil {
			t.Fatal("authenticator app without an ID")
		}
		wrong := first
		wrong.PasswordDigest = passwordDigest("changed")
		if _, err := s.EnrollTOTP(ctx, totpOf("alice-id", 100), mfaCodes("alice", 10), wrong); !errors.Is(err, store.ErrUnauthorized) {
			t.Fatalf("changed password: %v", err)
		}
		if _, err := s.EnrollTOTP(ctx, totpOf("alice-id", 100), nil, signedIn("bob")); !errors.Is(err, store.ErrUnauthorized) {
			t.Fatalf("another user's session: %v", err)
		}
		expired := signedIn("alice")
		expired.Now = tokenTestNow + 3600
		if _, err := s.EnrollTOTP(ctx, totpOf("alice-id", 100), nil, expired); !errors.Is(err, store.ErrUnauthorized) {
			t.Fatalf("expired session: %v", err)
		}

		// The first factor issues the recovery codes.
		issued, err := s.EnrollTOTP(ctx, totpOf("alice-id", 100), mfaCodes("alice", 10), first)
		if err != nil || !issued {
			t.Fatalf("first factor: %v %v", issued, err)
		}
		status := mfaStatus(t, s, "alice-id")
		if !status.Enrolled() || !status.UsableTOTP() || status.TOTP == nil || *status.TOTP != totpOf("alice-id", 100) || status.RecoveryCodes != 10 {
			t.Fatalf("after the first factor: %+v", status)
		}
		// A user with a factor cannot enroll before the first session.
		if _, err = s.EnrollTOTP(ctx, totpOf("alice-id", 200), mfaCodes("again", 10), first); !errors.Is(err, store.ErrMFAEnrolled) {
			t.Fatalf("second enrollment before a session: %v", err)
		}
		err = enrollKey(s, "alice-id", "c0", store.WebAuthnEnrollment{Codes: mfaCodes("again", 10), OnlyIfNone: true, PasswordDigest: passwordDigest("x")})
		if !errors.Is(err, store.ErrMFAEnrolled) || !errors.Is(err, webauthn.ErrInvalid) {
			t.Fatalf("WebAuthn enrollment before a session: %v", err)
		}
		if got := mfaStatus(t, s, "alice-id"); !reflect.DeepEqual(got, status) {
			t.Fatalf("rejected enrollments changed the user: %+v", got)
		}

		// Signed in, the authenticator app is replaced and the codes are kept.
		replacement := store.MFATOTP{ID: "totp-2", UserID: "alice-id", SecretSealed: "sealed-2", LastStep: 300, CreatedAt: tokenTestNow + 5}
		if issued, err = s.EnrollTOTP(ctx, replacement, mfaCodes("replaced", 10), signedIn("alice")); err != nil || issued {
			t.Fatalf("replacement: %v %v", issued, err)
		}
		if got := mfaStatus(t, s, "alice-id"); *got.TOTP != replacement || got.RecoveryCodes != 10 {
			t.Fatalf("after the replacement: %+v", got)
		}
		if ok, err := s.UseRecoveryCode(ctx, "alice-id", "replaced-hash-0", tokenTestNow); err != nil || ok {
			t.Fatalf("a code of the replacement was stored: %v %v", ok, err)
		}

		// A second factor issues no codes.
		var stored store.WebAuthnCredential
		issued = false
		if err = enrollKey(s, "alice-id", "c1", store.WebAuthnEnrollment{Codes: mfaCodes("second", 10), Issued: &issued, Stored: &stored}); err != nil || issued {
			t.Fatalf("second factor: %v %v", issued, err)
		}
		got := mfaStatus(t, s, "alice-id")
		if len(got.WebAuthn) != 1 || got.WebAuthn[0] != stored || stored.Name != "Key c1" || stored.CredentialID != store.CredentialID([]byte("key-c1")) || stored.Version == "" || got.RecoveryCodes != 10 {
			t.Fatalf("after the second factor: %+v %+v", got, stored)
		}

		// WebAuthn as the first factor issues the codes as well.
		if err = enrollKey(s, "bob-id", "b1", store.WebAuthnEnrollment{Codes: mfaCodes("bob", 10), OnlyIfNone: true, PasswordDigest: passwordDigest("x"), Issued: &issued}); err != nil || !issued {
			t.Fatalf("WebAuthn first factor: %v %v", issued, err)
		}
		if got = mfaStatus(t, s, "bob-id"); !got.Enrolled() || got.TOTP != nil || got.UsableWebAuthn() != 1 || got.RecoveryCodes != 10 {
			t.Fatalf("bob: %+v", got)
		}
		// A credential ID registered by anyone is rejected.
		if err = enrollKey(s, "bob-id", "b1", store.WebAuthnEnrollment{}); !errors.Is(err, store.ErrMFACredentialExists) || !errors.Is(err, webauthn.ErrInvalid) {
			t.Fatalf("own duplicate: %v", err)
		}
		if err = enrollKey(s, "carol-id", "b1", store.WebAuthnEnrollment{Codes: mfaCodes("carol", 10), OnlyIfNone: true}); !errors.Is(err, store.ErrMFACredentialExists) {
			t.Fatalf("duplicate of another user: %v", err)
		}
		if got = mfaStatus(t, s, "carol-id"); got.Enrolled() || got.RecoveryCodes != 0 {
			t.Fatalf("a rejected credential left rows: %+v", got)
		}
		// The limit is 10 credentials.
		for i := 2; i <= store.MaxWebAuthnCredentials; i++ {
			if err = enrollKey(s, "bob-id", fmt.Sprintf("b%d", i), store.WebAuthnEnrollment{}); err != nil {
				t.Fatalf("credential %d: %v", i, err)
			}
		}
		if err = enrollKey(s, "bob-id", "b11", store.WebAuthnEnrollment{}); !errors.Is(err, store.ErrMFALimit) || !errors.Is(err, webauthn.ErrInvalid) {
			t.Fatalf("11th credential: %v", err)
		}
		if got = mfaStatus(t, s, "bob-id"); len(got.WebAuthn) != store.MaxWebAuthnCredentials || store.MaxWebAuthnCredentials != 10 {
			t.Fatalf("bob: %d credentials", len(got.WebAuthn))
		}

		// The credential store refuses an insert without the enrollment, for
		// another subject, or with neither a session nor a password.
		cs := s.WebAuthnCredentials(time.Now)
		if err = cs.Insert(ctx, "carol-id", keyOf("x0"), false); !errors.Is(err, webauthn.ErrInvalid) {
			t.Fatalf("without the enrollment: %v", err)
		}
		e := store.WebAuthnEnrollment{UserID: "alice-id", SessionID: "alice-session", Now: tokenTestNow}
		if err = cs.Insert(store.WithWebAuthnEnrollment(ctx, e), "carol-id", keyOf("x0"), false); !errors.Is(err, webauthn.ErrInvalid) {
			t.Fatalf("another subject: %v", err)
		}
		e = store.WebAuthnEnrollment{UserID: "carol-id", Now: tokenTestNow}
		if err = cs.Insert(store.WithWebAuthnEnrollment(ctx, e), "carol-id", keyOf("x0"), false); !errors.Is(err, webauthn.ErrInvalid) {
			t.Fatalf("no session nor password: %v", err)
		}
		if _, err = s.EnrollTOTP(ctx, totpOf("carol-id", 1), nil, store.EnrollOptions{Now: tokenTestNow}); err == nil {
			t.Fatal("TOTP enrolled with neither a session nor a password")
		}
		if err = enrollKey(s, "carol-id", "x0", store.WebAuthnEnrollment{SessionID: "alice-session"}); !errors.Is(err, store.ErrUnauthorized) {
			t.Fatalf("another user's session: %v", err)
		}
		if err = enrollKey(s, "carol-id", "x0", store.WebAuthnEnrollment{PasswordDigest: passwordDigest("y")}); !errors.Is(err, store.ErrUnauthorized) {
			t.Fatalf("changed password: %v", err)
		}
		if got = mfaStatus(t, s, "carol-id"); got.Enrolled() {
			t.Fatalf("carol: %+v", got)
		}

		// A disabled or missing user enrolls nothing.
		if err = s.ChangeUser(ctx, "carol@example.com", "", true, tokenTestNow); err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{"carol-id", "missing-id"} {
			if _, err = s.EnrollTOTP(ctx, totpOf(id, 1), nil, store.EnrollOptions{}); !errors.Is(err, store.ErrUnauthorized) {
				t.Fatalf("%s: TOTP: %v", id, err)
			}
			if err = enrollKey(s, id, "z-"+id, store.WebAuthnEnrollment{PasswordDigest: passwordDigest("x")}); !errors.Is(err, store.ErrUnauthorized) {
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
			if _, err := s.EnrollTOTP(ctx, totpOf(name+"-id", 100), mfaCodes(name, 3), first); err != nil {
				t.Fatal(err)
			}
		}
		if got, err := s.TOTPRecord(ctx, "alice-id"); err != nil || got != totpOf("alice-id", 100) {
			t.Fatalf("record: %+v %v", got, err)
		}
		if _, err := s.TOTPRecord(ctx, "carol-id"); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("record of carol: %v", err)
		}
		step := func(userID, id string, step int64, want bool) {
			t.Helper()
			ok, err := s.ConsumeTOTPStep(ctx, userID, id, step)
			if err != nil || ok != want {
				t.Fatalf("step %d of %s: %v %v, want %v", step, userID, ok, err, want)
			}
		}
		// The step of the enrollment and the ones before it are spent.
		step("alice-id", "totp-alice-id", 100, false)
		step("alice-id", "totp-alice-id", 99, false)
		step("alice-id", "totp-alice-id", 101, true)
		step("alice-id", "totp-alice-id", 101, false)
		// A later step spends the ones before it.
		step("alice-id", "totp-alice-id", 103, true)
		step("alice-id", "totp-alice-id", 102, false)
		// Another app's ID, no ID and a user without an app spend nothing.
		step("alice-id", "totp-bob-id", 104, false)
		step("alice-id", "", 104, false)
		step("carol-id", "totp-carol-id", 500, false)
		if got := mfaStatus(t, s, "alice-id").TOTP.LastStep; got != 103 {
			t.Fatalf("last step: %d", got)
		}
		if got := mfaStatus(t, s, "bob-id").TOTP.LastStep; got != 100 {
			t.Fatalf("bob's last step: %d", got)
		}
		// A replaced app's ID is not accepted any more.
		replacement := totpOf("alice-id", 200)
		replacement.ID = "totp-new"
		if _, err := s.EnrollTOTP(ctx, replacement, nil, signedIn("alice")); err != nil {
			t.Fatal(err)
		}
		step("alice-id", "totp-alice-id", 300, false)
		step("alice-id", "totp-new", 300, true)

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
		if _, err := s.EnrollTOTP(ctx, totpOf("alice-id", 1), mfaCodes("alice", 10), first); err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{"a1", "a2"} {
			if err := enrollKey(s, "alice-id", id, store.WebAuthnEnrollment{}); err != nil {
				t.Fatal(err)
			}
		}
		// carol (member): one credential. bob (owner): nothing.
		if err := enrollKey(s, "carol-id", "c1", store.WebAuthnEnrollment{Codes: mfaCodes("carol", 10), OnlyIfNone: true}); err != nil {
			t.Fatal(err)
		}
		alice := mfaStatus(t, s, "alice-id").WebAuthn
		carol := mfaStatus(t, s, "carol-id").WebAuthn[0].ID

		if err := s.DeleteTOTP(ctx, "bob-id", mfa.PolicyOff); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("no authenticator app: %v", err)
		}
		// Credentials of another user and unknown ones are not found.
		for _, id := range []string{carol, "missing"} {
			if err := s.DeleteWebAuthnCredential(ctx, "alice-id", id, mfa.PolicyOff); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("credential %s as alice: %v", id, err)
			}
		}
		if got := mfaStatus(t, s, "carol-id"); len(got.WebAuthn) != 1 {
			t.Fatalf("carol: %+v", got)
		}

		// While other factors are left, a required user may remove one, and
		// the recovery codes stay.
		if err := s.DeleteWebAuthnCredential(ctx, "alice-id", alice[0].ID, mfa.PolicyAll); err != nil {
			t.Fatal(err)
		}
		if err := s.DeleteTOTP(ctx, "alice-id", mfa.PolicyOwners); err != nil {
			t.Fatal(err)
		}
		if got := mfaStatus(t, s, "alice-id"); got.TOTP != nil || len(got.WebAuthn) != 1 || got.WebAuthn[0].ID != alice[1].ID || got.RecoveryCodes != 10 {
			t.Fatalf("alice with one factor: %+v", got)
		}
		// The last factor of a required user stays.
		for _, policy := range []string{mfa.PolicyOwners, mfa.PolicyAll} {
			if err := s.DeleteWebAuthnCredential(ctx, "alice-id", alice[1].ID, policy); !errors.Is(err, store.ErrMFARequired) {
				t.Fatalf("last factor of an owner under %s: %v", policy, err)
			}
		}
		if err := s.DeleteWebAuthnCredential(ctx, "carol-id", carol, mfa.PolicyAll); !errors.Is(err, store.ErrMFARequired) {
			t.Fatalf("last factor of a member under all: %v", err)
		}
		if a, c := mfaStatus(t, s, "alice-id"), mfaStatus(t, s, "carol-id"); len(a.WebAuthn) != 1 || a.RecoveryCodes != 10 || len(c.WebAuthn) != 1 || c.RecoveryCodes != 10 {
			t.Fatalf("rejected removals changed the users: %+v %+v", a, c)
		}
		// A member is not required under owners; the codes go with the last
		// factor.
		if err := s.DeleteWebAuthnCredential(ctx, "carol-id", carol, mfa.PolicyOwners); err != nil {
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
		if err := s.DeleteWebAuthnCredential(ctx, "alice-id", alice[1].ID, mfa.PolicyOwners); err != nil {
			t.Fatalf("demoted owner: %v", err)
		}
		if got := mfaStatus(t, s, "alice-id"); got.Enrolled() || got.RecoveryCodes != 0 {
			t.Fatalf("alice without factors: %+v", got)
		}

		// The last authenticator app follows the same rules.
		if _, err := s.EnrollTOTP(ctx, totpOf("bob-id", 1), mfaCodes("bob", 10), first); err != nil {
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

// TestWebAuthnCredentialStore checks Load and Update of the credential store
// of authlume/webauthn.
func TestWebAuthnCredentialStore(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		ctx := context.Background()
		s := userFixture(t, driver)
		cs := s.WebAuthnCredentials(time.Now)
		e := store.WebAuthnEnrollment{UserID: "alice-id", Name: "Passkey", SessionID: "alice-session", Now: tokenTestNow}
		if err := cs.Insert(store.WithWebAuthnEnrollment(ctx, e), "alice-id", keyOf("a1"), true); err != nil {
			t.Fatal(err)
		}
		if err := enrollKey(s, "bob-id", "b1", store.WebAuthnEnrollment{}); err != nil {
			t.Fatal(err)
		}
		legacyCredential(t, s, "alice-id", "old")

		load := func(subject string) []webauthn.Record {
			t.Helper()
			records, err := cs.Load(ctx, subject)
			if err != nil {
				t.Fatal(err)
			}
			return records
		}
		// The row from before authlume is left out.
		records := load("alice-id")
		if len(records) != 1 || string(records[0].Credential.ID) != "key-a1" || string(records[0].Credential.PublicKey) != "public-a1" || !records[0].Passkey || records[0].Version == "" {
			t.Fatalf("alice: %+v", records)
		}
		if got := load("missing-id"); got == nil || len(got) != 0 {
			t.Fatalf("missing user: %#v", got)
		}
		if got := load("ALICE-ID"); len(got) != 0 {
			t.Fatalf("subject folded: %+v", got)
		}

		// An update replaces the version; the old one is spent.
		version := records[0].Version
		updated := records[0].Credential
		updated.Authenticator.SignCount = 7
		if err := cs.Update(ctx, "alice-id", version, updated); err != nil {
			t.Fatal(err)
		}
		records = load("alice-id")
		if len(records) != 1 || records[0].Version == version || records[0].Version == "" || records[0].Credential.Authenticator.SignCount != 7 || !records[0].Passkey {
			t.Fatalf("after the update: %+v", records)
		}
		for _, c := range mfaStatus(t, s, "alice-id").WebAuthn {
			if c.LastUsedAt.Valid != (c.Version != "") {
				t.Fatalf("last use: %+v", c)
			}
		}
		if err := cs.Update(ctx, "alice-id", version, updated); !errors.Is(err, webauthn.ErrInvalid) {
			t.Fatalf("spent version: %v", err)
		}
		// Another subject, an unknown credential and no version fail.
		bob := load("bob-id")[0]
		if err := cs.Update(ctx, "alice-id", bob.Version, bob.Credential); !errors.Is(err, webauthn.ErrInvalid) {
			t.Fatalf("credential of another user: %v", err)
		}
		if err := cs.Update(ctx, "alice-id", records[0].Version, keyOf("missing")); !errors.Is(err, webauthn.ErrInvalid) {
			t.Fatalf("unknown credential: %v", err)
		}
		if err := cs.Update(ctx, "alice-id", "", webauthn.Credential{ID: []byte("legacy-old")}); !errors.Is(err, webauthn.ErrInvalid) {
			t.Fatalf("row from before authlume: %v", err)
		}
		if got := load("bob-id")[0]; got.Version != bob.Version {
			t.Fatalf("bob changed: %+v", got)
		}

		// Two uses of the same version at once: one wins.
		for i := 0; i < 10; i++ {
			current := load("alice-id")[0]
			errs := race(
				func() error { return cs.Update(ctx, "alice-id", current.Version, current.Credential) },
				func() error { return cs.Update(ctx, "alice-id", current.Version, current.Credential) },
			)
			ok, invalid := 0, 0
			for _, err := range errs {
				switch {
				case err == nil:
					ok++
				case errors.Is(err, webauthn.ErrInvalid):
					invalid++
				default:
					t.Fatalf("round %d: %v", i, err)
				}
			}
			if ok != 1 || invalid != 1 {
				t.Fatalf("round %d: %d succeeded, %d rejected", i, ok, invalid)
			}
		}
	})
}

// TestMFALegacyRows checks the rows stored before authlume (§6.7): they keep
// the second factor required, cannot be used, are replaced by an
// enrollment and are not counted by the limit.
func TestMFALegacyRows(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		ctx := context.Background()
		s := userFixture(t, driver)
		legacyTOTP(t, s, "alice-id")
		legacyCredential(t, s, "alice-id", "old-1")
		if err := s.ReplaceRecoveryCodes(ctx, "alice-id", mfaCodes("old", 10), tokenTestNow); err != nil {
			t.Fatal(err)
		}
		status := mfaStatus(t, s, "alice-id")
		if !status.Enrolled() || status.UsableTOTP() || status.UsableWebAuthn() != 0 || status.RecoveryCodes != 10 {
			t.Fatalf("legacy rows: %+v", status)
		}
		if enabled, err := s.MFAEnabledUserIDs(ctx); err != nil || !enabled["alice-id"] {
			t.Fatalf("enabled: %v %v", enabled, err)
		}
		// The rows cannot be used; the recovery codes can.
		if ok, err := s.ConsumeTOTPStep(ctx, "alice-id", "", 100); err != nil || ok {
			t.Fatalf("legacy authenticator app: %v %v", ok, err)
		}
		if records, err := s.WebAuthnCredentials(time.Now).Load(ctx, "alice-id"); err != nil || len(records) != 0 {
			t.Fatalf("legacy credentials: %+v %v", records, err)
		}
		if ok, err := s.UseRecoveryCode(ctx, "alice-id", "old-hash-0", tokenTestNow); err != nil || !ok {
			t.Fatalf("recovery code: %v %v", ok, err)
		}

		// The enrollment before the first session treats them as no factor:
		// the authenticator app replaces the legacy one, and the recovery
		// codes are replaced.
		issued, err := s.EnrollTOTP(ctx, totpOf("alice-id", 100), mfaCodes("new", 10), first)
		if err != nil || !issued {
			t.Fatalf("enrollment over the legacy rows: %v %v", issued, err)
		}
		status = mfaStatus(t, s, "alice-id")
		if !status.UsableTOTP() || *status.TOTP != totpOf("alice-id", 100) || len(status.WebAuthn) != 1 || status.RecoveryCodes != 10 {
			t.Fatalf("after the enrollment: %+v", status)
		}
		if ok, _ := s.UseRecoveryCode(ctx, "alice-id", "old-hash-1", tokenTestNow); ok {
			t.Fatal("an old recovery code was kept")
		}
		// The legacy credential can be removed like any other.
		if err = s.DeleteWebAuthnCredential(ctx, "alice-id", "old-1", mfa.PolicyAll); err != nil {
			t.Fatal(err)
		}

		// A WebAuthn credential over a legacy credential is a first factor
		// as well.
		legacyCredential(t, s, "bob-id", "old-2")
		if err = s.ReplaceRecoveryCodes(ctx, "bob-id", mfaCodes("bob-old", 10), tokenTestNow); err != nil {
			t.Fatal(err)
		}
		issued = false
		if err = enrollKey(s, "bob-id", "b0", store.WebAuthnEnrollment{Codes: mfaCodes("bob", 10), OnlyIfNone: true, PasswordDigest: passwordDigest("x"), Issued: &issued}); err != nil || !issued {
			t.Fatalf("WebAuthn over a legacy credential: %v %v", issued, err)
		}
		if ok, _ := s.UseRecoveryCode(ctx, "bob-id", "bob-old-hash-0", tokenTestNow); ok {
			t.Fatal("bob's old recovery code was kept")
		}

		// The limit counts the usable credentials alone.
		for i := 0; i < store.MaxWebAuthnCredentials; i++ {
			legacyCredential(t, s, "carol-id", fmt.Sprintf("old-c%d", i))
		}
		for i := 0; i < store.MaxWebAuthnCredentials; i++ {
			if err = enrollKey(s, "carol-id", fmt.Sprintf("c%d", i), store.WebAuthnEnrollment{}); err != nil {
				t.Fatalf("credential %d over the legacy ones: %v", i, err)
			}
		}
		if err = enrollKey(s, "carol-id", "c-extra", store.WebAuthnEnrollment{}); !errors.Is(err, store.ErrMFALimit) {
			t.Fatalf("over the limit: %v", err)
		}
		if got := mfaStatus(t, s, "carol-id"); got.UsableWebAuthn() != store.MaxWebAuthnCredentials || len(got.WebAuthn) != 2*store.MaxWebAuthnCredentials {
			t.Fatalf("carol: %d usable of %d", got.UsableWebAuthn(), len(got.WebAuthn))
		}

		// ResetMFA removes them.
		if err = s.ResetMFA(ctx, "carol@example.com"); err != nil {
			t.Fatal(err)
		}
		if got := mfaStatus(t, s, "carol-id"); got.Enrolled() {
			t.Fatalf("carol after ResetMFA: %+v", got)
		}
	})
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
					_, err := s.EnrollTOTP(ctx, totpOf("alice-id", 1), mfaCodes(fmt.Sprintf("t%d", i), 10), first)
					return err
				},
				func() error {
					return enrollKey(s, "alice-id", fmt.Sprintf("w%d", i), store.WebAuthnEnrollment{Codes: mfaCodes(fmt.Sprintf("w%d", i), 10), OnlyIfNone: true, PasswordDigest: passwordDigest("x")})
				},
				func() error {
					_, err := s.EnrollTOTP(ctx, totpOf("alice-id", 2), mfaCodes(fmt.Sprintf("u%d", i), 10), first)
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
		// ResetMFA revoked the session; a new one for the settings screen.
		if err := s.InsertWebSession(ctx, store.WebSession{ID: "alice-session-2", UserID: "alice-id", AuthenticatedAt: tokenTestNow, LastActivity: tokenTestNow, AbsoluteExpiry: tokenTestNow + 3600, IdleSeconds: 3600}); err != nil {
			t.Fatal(err)
		}

		// The same step and the same recovery code at once: one wins.
		if _, err := s.EnrollTOTP(ctx, totpOf("alice-id", 1), mfaCodes("alice", 10), first); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 10; i++ {
			results := make([]bool, 4)
			errs := race(
				func() (err error) {
					results[0], err = s.ConsumeTOTPStep(ctx, "alice-id", "totp-alice-id", int64(10+i))
					return
				},
				func() (err error) {
					results[1], err = s.ConsumeTOTPStep(ctx, "alice-id", "totp-alice-id", int64(10+i))
					return
				},
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
			var stored store.WebAuthnCredential
			if err := enrollKey(s, "alice-id", fmt.Sprintf("r%d", i), store.WebAuthnEnrollment{SessionID: "alice-session-2", Stored: &stored}); err != nil {
				t.Fatal(err)
			}
			errs := race(
				func() error { return s.DeleteTOTP(ctx, "alice-id", mfa.PolicyAll) },
				func() error { return s.DeleteWebAuthnCredential(ctx, "alice-id", stored.ID, mfa.PolicyAll) },
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
				if _, err := s.EnrollTOTP(ctx, totpOf("alice-id", 1), nil, store.EnrollOptions{SessionID: "alice-session-2", Now: tokenTestNow}); err != nil {
					t.Fatal(err)
				}
				if err := s.DeleteWebAuthnCredential(ctx, "alice-id", stored.ID, mfa.PolicyAll); err != nil {
					t.Fatal(err)
				}
			}
		}
	})
}

// TestMFAKeptAndReset checks that a password reset and disabling keep the
// second factors, and that ResetMFA removes those of one user alone and
// revokes every way into the account (§9).
func TestMFAKeptAndReset(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		ctx := context.Background()
		s := userFixture(t, driver)
		for _, name := range []string{"alice", "bob"} {
			if _, err := s.EnrollTOTP(ctx, totpOf(name+"-id", 1), mfaCodes(name, 10), first); err != nil {
				t.Fatal(err)
			}
			if err := enrollKey(s, name+"-id", name+"-1", store.WebAuthnEnrollment{}); err != nil {
				t.Fatal(err)
			}
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
		grantAccess(t, s, "alice")
		grantAccess(t, s, "bob")
		bob := mfaStatus(t, s, "bob-id")
		for range 2 {
			if err := s.ResetMFA(ctx, "alice@example.com"); err != nil {
				t.Fatal(err)
			}
		}
		if got := mfaStatus(t, s, "alice-id"); got.Enrolled() || got.RecoveryCodes != 0 {
			t.Fatalf("alice after ResetMFA: %+v", got)
		}
		if got := accessOf(t, s, "alice-id"); got != (access{}) {
			t.Fatalf("alice's access after ResetMFA: %+v", got)
		}
		if got := mfaStatus(t, s, "bob-id"); !reflect.DeepEqual(got, bob) || !got.Enrolled() || got.RecoveryCodes != 10 {
			t.Fatalf("bob after alice's ResetMFA: %+v", got)
		}
		if got := accessOf(t, s, "bob-id"); got != granted {
			t.Fatalf("bob's access after alice's ResetMFA: %+v", got)
		}
		enabled, err := s.MFAEnabledUserIDs(ctx)
		if err != nil || !reflect.DeepEqual(enabled, map[string]bool{"bob-id": true}) {
			t.Fatalf("enabled: %v %v", enabled, err)
		}
	})
}

// TestCheckSchemaMFA checks that a database without one of the MFA or
// authlume tables is reported.
func TestCheckSchemaMFA(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		for _, table := range []string{"mfa_totp", "mfa_webauthn_credentials", "mfa_recovery_codes", "web_sessions", "oidc_logins", "webauthn_ceremonies", "mcp_oauth_codes", "mcp_oauth_connections", "mcp_oauth_registrations", "mcp_oauth_client_labels"} {
			s, _ := testutil.Database(t, driver)
			if err := s.ExecForTest(context.Background(), "DROP TABLE "+table); err != nil {
				t.Fatal(err)
			}
			if err := s.CheckSchema(context.Background()); err == nil {
				t.Fatalf("missing %s accepted", table)
			}
		}
	})
}

// TestSessionBoundWrites checks that the writes bound to a browser session
// recheck it: a session past its idle limit and one past its absolute limit
// each refuse the write, while a live session allows it.
func TestSessionBoundWrites(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		ctx := context.Background()
		s := userFixture(t, driver)
		for _, ws := range []store.WebSession{
			// Idle: the last activity is one idle period ago, the absolute
			// limit is far.
			{ID: "idle-session", UserID: "alice-id", AuthenticatedAt: tokenTestNow - 3600, LastActivity: tokenTestNow - 600, AbsoluteExpiry: tokenTestNow + 36000, IdleSeconds: 600},
			// Absolute: the last activity is now, the absolute limit is now.
			{ID: "absolute-session", UserID: "alice-id", AuthenticatedAt: tokenTestNow - 3600, LastActivity: tokenTestNow, AbsoluteExpiry: tokenTestNow, IdleSeconds: 600},
		} {
			if err := s.InsertWebSession(ctx, ws); err != nil {
				t.Fatal(err)
			}
		}
		code := func(sid, key string) error {
			c := mcpoauth.Code{ClientID: "client", RedirectURI: "https://client.example/callback", Subject: "alice-id", Scopes: []string{"mcp"}, Challenge: "challenge", ExpiresAt: time.Now().Add(time.Minute)}
			return s.MCPOAuth(time.Now).CreateCode(store.WithApprovalSession(ctx, sid, time.Unix(tokenTestNow, 0)), sha256.Sum256([]byte(key)), c)
		}
		for _, sid := range []string{"idle-session", "absolute-session"} {
			if _, err := s.EnrollTOTP(ctx, totpOf("alice-id", 100), nil, store.EnrollOptions{SessionID: sid, Now: tokenTestNow}); !errors.Is(err, store.ErrUnauthorized) {
				t.Fatalf("%s: authenticator app: %v", sid, err)
			}
			if err := enrollKey(s, "alice-id", sid, store.WebAuthnEnrollment{SessionID: sid}); !errors.Is(err, store.ErrUnauthorized) {
				t.Fatalf("%s: security key: %v", sid, err)
			}
			if err := code(sid, "code-"+sid); !errors.Is(err, store.ErrUnauthorized) {
				t.Fatalf("%s: authorization code: %v", sid, err)
			}
		}
		if status := mfaStatus(t, s, "alice-id"); status.TOTP != nil || len(status.WebAuthn) != 0 {
			t.Fatalf("written through an expired session: %+v", status)
		}
		// The live session of the fixtures allows each write.
		if _, err := s.EnrollTOTP(ctx, totpOf("alice-id", 100), nil, signedIn("alice")); err != nil {
			t.Fatal(err)
		}
		if err := enrollKey(s, "alice-id", "live", store.WebAuthnEnrollment{}); err != nil {
			t.Fatal(err)
		}
		if err := code("alice-session", "code-live"); err != nil {
			t.Fatal(err)
		}
	})
}
