package store_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/i2bskn/astovia/internal/store"
)

func gmailItem(id, key string, created int64) store.ActionItem {
	return store.ActionItem{ID: id, Content: "メール: " + id, Status: "open", Refs: "[]", Version: 1, CreatedAt: created, UpdatedAt: created,
		Source: sql.NullString{String: store.SourceGmail, Valid: true}, SourceKey: sql.NullString{String: key, Valid: true}, Tracking: 1, DetectedAt: sql.NullInt64{Int64: created, Valid: true}}
}

// saveGmail connects email for the user; the sealed token names the ID it
// was sealed for.
func saveGmail(t *testing.T, s *store.Store, userID, email string, now int64) store.GmailAccount {
	t.Helper()
	a, err := s.SaveGmailAccount(context.Background(), userID, userID[:len(userID)-3]+"-session", email, func(id string) (string, error) { return "sealed-" + id, nil }, now)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestGoogleAuthStates(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) { testGoogleAuthStates(t, driver) })
	}
}

func testGoogleAuthStates(t *testing.T, driver string) {
	ctx := context.Background()
	s, _ := apiTokenFixture(t, driver)
	create := func(user, hash string, now int64) {
		t.Helper()
		if err := s.CreateAuthState(ctx, user+"-id", user+"-session", hash, "verifier-"+hash, now, 10*time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	consume := func(user, hash string, now int64) (string, error) {
		return s.ConsumeAuthState(ctx, user+"-id", user+"-session", hash, now)
	}

	// A state is used once.
	create("alice", "h1", tokenTestNow)
	if v, err := consume("alice", "h1", tokenTestNow+1); err != nil || v != "verifier-h1" {
		t.Fatalf("consume: %q %v", v, err)
	}
	if _, err := consume("alice", "h1", tokenTestNow+2); !errors.Is(err, store.ErrAuthStateNotFound) {
		t.Fatalf("second consume: %v", err)
	}

	// Another user cannot consume it, and it is kept for its owner.
	create("bob", "hb", tokenTestNow)
	if _, err := consume("alice", "hb", tokenTestNow+1); !errors.Is(err, store.ErrAuthStateNotFound) {
		t.Fatalf("other user: %v", err)
	}
	if v, err := consume("bob", "hb", tokenTestNow+1); err != nil || v != "verifier-hb" {
		t.Fatalf("owner after other user: %q %v", v, err)
	}

	// An expired state cannot be used; a failed consume of alice deletes
	// alice's expired states and not bob's.
	create("alice", "old", tokenTestNow)
	create("bob", "bob-old", tokenTestNow)
	if _, err := consume("alice", "old", tokenTestNow+600); !errors.Is(err, store.ErrAuthStateNotFound) {
		t.Fatalf("expired: %v", err)
	}
	if _, err := consume("alice", "missing", tokenTestNow+600); !errors.Is(err, store.ErrAuthStateNotFound) {
		t.Fatalf("missing: %v", err)
	}
	// bob's expired state is still there: it can be read before it expires.
	if v, err := consume("bob", "bob-old", tokenTestNow+599); err != nil || v != "verifier-bob-old" {
		t.Fatalf("bob's state: %q %v", v, err)
	}

	// At most five are kept: the sixth drops the oldest.
	for i := 1; i <= 6; i++ {
		create("alice", fmt.Sprintf("s%d", i), tokenTestNow+int64(i))
	}
	if _, err := consume("alice", "s1", tokenTestNow+10); !errors.Is(err, store.ErrAuthStateNotFound) {
		t.Fatalf("oldest kept: %v", err)
	}
	for i := 2; i <= 6; i++ {
		if _, err := consume("alice", fmt.Sprintf("s%d", i), tokenTestNow+10); err != nil {
			t.Fatalf("s%d: %v", i, err)
		}
	}

	// A password reset deletes the states.
	create("alice", "r1", tokenTestNow)
	if err := s.ChangeUser(ctx, "alice@example.com", "new", false, tokenTestNow); err != nil {
		t.Fatal(err)
	}
	u, _ := s.UserByEmail(ctx, "alice@example.com")
	if err := s.Login(ctx, u, store.Session{ID: "alice-session2", UserID: "alice-id", ExpiresAt: tokenTestNow + 3600, CreatedAt: tokenTestNow}, store.Refresh{Hash: "refresh-alice2", SessionID: "alice-session2", CreatedAt: tokenTestNow}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConsumeAuthState(ctx, "alice-id", "alice-session2", "r1", tokenTestNow+1); !errors.Is(err, store.ErrAuthStateNotFound) {
		t.Fatalf("after reset: %v", err)
	}
	// The revoked session cannot create or consume.
	if err := s.CreateAuthState(ctx, "alice-id", "alice-session", "x", "v", tokenTestNow, time.Minute); !errors.Is(err, store.ErrUnauthorized) {
		t.Fatalf("revoked session create: %v", err)
	}
	if _, err := consume("alice", "x", tokenTestNow); !errors.Is(err, store.ErrUnauthorized) {
		t.Fatalf("revoked session consume: %v", err)
	}
}

func TestGmailAccounts(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) { testGmailAccounts(t, driver) })
	}
}

func testGmailAccounts(t *testing.T, driver string) {
	ctx := context.Background()
	s, _ := apiTokenFixture(t, driver)
	a := saveGmail(t, s, "alice-id", "a@example.com", tokenTestNow)
	if len(a.ID) != 43 || a.Generation != 1 || a.Status != store.GmailActive || a.ImmediateNotify != 1 || a.TokenSealed != "sealed-"+a.ID || a.ConnectedAt != tokenTestNow || a.NextPollAt != tokenTestNow ||
		a.CreatedAt != tokenTestNow || a.LastSyncedAt.Valid || a.LastTrackedAt.Valid {
		t.Fatalf("new account: %+v", a)
	}
	if got, err := s.GmailAccount(ctx, "alice-id", a.ID); err != nil || !reflect.DeepEqual(got, a) {
		t.Fatalf("round trip: %+v %v", got, err)
	}
	if _, err := s.GmailAccount(ctx, "bob-id", a.ID); !errors.Is(err, store.ErrConnectionNotFound) {
		t.Fatalf("other user's account: %v", err)
	}

	// SaveState writes only the synchronization state; the window stays
	// across a reconnection of the same address.
	err := s.GmailSync(ctx, "alice-id", a.ID, a.Generation, func(tx *store.GmailTx) error {
		state := tx.Account
		state.Status, state.FailureCount, state.LastError = store.GmailError, 3, sql.NullString{String: "token_invalid", Valid: true}
		state.LastSyncedAt, state.LastTrackedAt, state.NextPollAt = sql.NullInt64{Int64: 500, Valid: true}, sql.NullInt64{Int64: 400, Valid: true}, 900
		state.ImmediateNotify, state.Email, state.ConnectedAt, state.TokenSealed = 0, "x@example.com", 1, "ignored"
		return tx.SaveState(state)
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.GmailAccount(ctx, "alice-id", a.ID)
	if err != nil || got.Status != store.GmailError || got.FailureCount != 3 || got.LastSyncedAt.Int64 != 500 || got.LastTrackedAt.Int64 != 400 || got.NextPollAt != 900 ||
		got.ImmediateNotify != 1 || got.Email != "a@example.com" || got.ConnectedAt != tokenTestNow || got.TokenSealed != a.TokenSealed {
		t.Fatalf("after SaveState: %+v %v", got, err)
	}
	// A token replaced by Google keeps the generation.
	if err = s.GmailSync(ctx, "alice-id", a.ID, a.Generation, func(tx *store.GmailTx) error { return tx.UpdateGmailToken("rotated") }); err != nil {
		t.Fatal(err)
	}
	if got, err = s.GmailAccount(ctx, "alice-id", a.ID); err != nil || got.TokenSealed != "rotated" || got.Generation != 1 {
		t.Fatalf("rotated: %+v %v", got, err)
	}
	if _, err = s.UpdateGmailSettings(ctx, "alice-id", "alice-session", a.ID, false, tokenTestNow+5); err != nil {
		t.Fatal(err)
	}
	re := saveGmail(t, s, "alice-id", "a@example.com", tokenTestNow+10)
	if re.ID != a.ID || re.Generation != 2 || re.Status != store.GmailActive || re.LastError.Valid || re.FailureCount != 0 || re.ConnectedAt != tokenTestNow+10 || re.NextPollAt != tokenTestNow+10 ||
		re.LastSyncedAt.Int64 != 500 || re.LastTrackedAt.Int64 != 400 || re.CreatedAt != tokenTestNow || re.ImmediateNotify != 0 || re.TokenSealed != "sealed-"+a.ID {
		t.Fatalf("reconnected: %+v", re)
	}
	if got, err = s.GmailAccount(ctx, "alice-id", a.ID); err != nil || !reflect.DeepEqual(got, re) {
		t.Fatalf("reconnected stored: %+v %v", got, err)
	}
	// A synchronization of the older generation is stale.
	called := false
	if err = s.GmailSync(ctx, "alice-id", a.ID, 1, func(*store.GmailTx) error { called = true; return nil }); !errors.Is(err, store.ErrStaleConnection) || called {
		t.Fatalf("older generation: %v %v", err, called)
	}
	if err = s.GmailSync(ctx, "bob-id", a.ID, 2, func(*store.GmailTx) error { called = true; return nil }); !errors.Is(err, store.ErrStaleConnection) || called {
		t.Fatalf("other user: %v %v", err, called)
	}

	// An address connected by another user is refused.
	if _, err = s.SaveGmailAccount(ctx, "bob-id", "bob-session", "a@example.com", func(string) (string, error) { return "x", nil }, tokenTestNow); !errors.Is(err, store.ErrAccountInUse) {
		t.Fatalf("in use: %v", err)
	}
	// The seal function's error is returned and nothing is saved.
	if _, err = s.SaveGmailAccount(ctx, "bob-id", "bob-session", "b@example.com", func(string) (string, error) { return "", errors.New("seal") }, tokenTestNow); err == nil {
		t.Fatal("seal error ignored")
	}

	// Ten accounts at most; reconnecting one of them is not a new one.
	for i := 2; i <= store.MaxGmailAccounts; i++ {
		saveGmail(t, s, "alice-id", fmt.Sprintf("a%02d@example.com", i), tokenTestNow+int64(i))
	}
	if _, err = s.SaveGmailAccount(ctx, "alice-id", "alice-session", "eleven@example.com", func(string) (string, error) { return "x", nil }, tokenTestNow); !errors.Is(err, store.ErrTooManyAccounts) {
		t.Fatalf("eleventh: %v", err)
	}
	saveGmail(t, s, "alice-id", "a02@example.com", tokenTestNow+20)
	list, err := s.GmailAccounts(ctx, "alice-id")
	if err != nil || len(list) != store.MaxGmailAccounts || list[0].Email != "a02@example.com" || list[len(list)-1].Email != "a@example.com" {
		t.Fatalf("list: %d %v", len(list), err)
	}
	if list, err = s.GmailAccounts(ctx, "bob-id"); err != nil || list == nil || len(list) != 0 {
		t.Fatalf("bob's list: %v %v", list, err)
	}

	// Due: active accounts of enabled users whose time has come.
	bob := saveGmail(t, s, "bob-id", "b@example.com", tokenTestNow+30)
	due, err := s.DueGmailAccounts(ctx, tokenTestNow+30, 100)
	if err != nil || len(due) != store.MaxGmailAccounts+1 || due[len(due)-1].ID != bob.ID {
		t.Fatalf("due: %d %v", len(due), err)
	}
	if due, err = s.DueGmailAccounts(ctx, tokenTestNow+9, 100); err != nil || len(due) != 7 {
		t.Fatalf("due before: %d %v", len(due), err)
	}
	if due, err = s.DueGmailAccounts(ctx, tokenTestNow+30, 2); err != nil || len(due) != 2 {
		t.Fatalf("due limit: %d %v", len(due), err)
	}
	if err = s.GmailSync(ctx, "bob-id", bob.ID, bob.Generation, func(tx *store.GmailTx) error {
		state := tx.Account
		state.Status = store.GmailError
		return tx.SaveState(state)
	}); err != nil {
		t.Fatal(err)
	}
	for _, d := range mustDue(t, s, tokenTestNow+30) {
		if d.ID == bob.ID {
			t.Fatal("account in error is due")
		}
	}

	// Another user's account cannot be changed or deleted.
	if _, err = s.UpdateGmailSettings(ctx, "bob-id", "bob-session", a.ID, true, tokenTestNow); !errors.Is(err, store.ErrConnectionNotFound) {
		t.Fatalf("other user's settings: %v", err)
	}
	if _, err = s.DeleteGmailAccount(ctx, "bob-id", "bob-session", a.ID, tokenTestNow); !errors.Is(err, store.ErrConnectionNotFound) {
		t.Fatalf("other user's delete: %v", err)
	}
	if _, err = s.GmailAccount(ctx, "alice-id", a.ID); err != nil {
		t.Fatalf("after other user's delete: %v", err)
	}

	// Disabling the user removes the accounts; a password reset keeps them.
	if err = s.ChangeUser(ctx, "alice@example.com", "new", false, tokenTestNow); err != nil {
		t.Fatal(err)
	}
	if list, err = s.GmailAccounts(ctx, "alice-id"); err != nil || len(list) != store.MaxGmailAccounts {
		t.Fatalf("after reset: %d %v", len(list), err)
	}
	if err = s.ChangeUser(ctx, "alice@example.com", "", true, tokenTestNow); err != nil {
		t.Fatal(err)
	}
	if list, err = s.GmailAccounts(ctx, "alice-id"); err != nil || len(list) != 0 {
		t.Fatalf("after disable: %d %v", len(list), err)
	}
	if _, err = s.GmailAccount(ctx, "bob-id", bob.ID); err != nil {
		t.Fatalf("bob's account after alice was disabled: %v", err)
	}
}

func mustDue(t *testing.T, s *store.Store, now int64) []store.GmailAccount {
	t.Helper()
	due, err := s.DueGmailAccounts(context.Background(), now, 100)
	if err != nil {
		t.Fatal(err)
	}
	return due
}

func TestGmailDelete(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) { testGmailDelete(t, driver) })
	}
}

func testGmailDelete(t *testing.T, driver string) {
	ctx := context.Background()
	s, _ := apiTokenFixture(t, driver)
	// Detection notifications are stored only for users who can receive
	// them.
	subscribe(t, s, "alice")
	subscribe(t, s, "bob")
	// The addresses contain the LIKE wildcards; the other accounts' keys
	// would match them as patterns.
	target := saveGmail(t, s, "alice-id", "a_b%c@example.com", tokenTestNow)
	other := saveGmail(t, s, "alice-id", "axbyc@example.com", tokenTestNow)
	other2 := saveGmail(t, s, "alice-id", "a_b%c@example.co", tokenTestNow)
	items := map[string]store.ActionItem{
		"t1":  gmailItem("t1", "gmail:a_b%c@example.com:r:1a:1b:1c", 100),
		"t2":  gmailItem("t2", "gmail:a_b%c@example.com:r:2a:2b:2c", 200),
		"o1":  gmailItem("o1", "gmail:axbyc@example.com:r:1a:1b:1c", 100),
		"o2":  gmailItem("o2", "gmail:a_b%c@example.co:r:1a:1b:1c", 100),
		"o3":  gmailItem("o3", "gmail:a_b%c@example.com.evil:r:1a:1b:1c", 100),
		"cnd": gmailItem("cnd", "gmail:a_b%c@example.com:r:3a:3b:3c", 300),
	}
	cnd := items["cnd"]
	cnd.Status = "candidate"
	items["cnd"] = cnd
	accounts := map[string]store.GmailAccount{"t1": target, "t2": target, "cnd": target, "o1": other, "o2": other2, "o3": other}
	for id, item := range items {
		acc := accounts[id]
		if err := s.GmailSync(ctx, "alice-id", acc.ID, acc.Generation, func(tx *store.GmailTx) error {
			if _, e := tx.CreateItem(item); e != nil {
				return e
			}
			return tx.Enqueue(notification("n-"+id, store.NotificationDetected, "detected:"+id+":1", id, 300))
		}); err != nil {
			t.Fatal(err)
		}
	}
	// A Slack item with a Gmail-like key is not a mail item.
	sc := saveSlack(t, s, "alice-id", "T1", "U1", store.SlackSettings{}, tokenTestNow)
	if err := s.SlackSync(ctx, "alice-id", sc.Generation, func(tx *store.SlackTx) error {
		if _, e := tx.CreateItem(slackItem("slack", "gmail:a_b%c@example.com:r:9a:9b:9c", 300)); e != nil {
			return e
		}
		return tx.Enqueue(notification("n-slack", store.NotificationDetected, "detected:slack:1", "slack", 300))
	}); err != nil {
		t.Fatal(err)
	}

	tracked, err := s.TrackedSourcePrefixItems(ctx, "alice-id", store.SourceGmail, "gmail:a_b%c@example.com:", 100)
	var ids []string
	for _, item := range tracked {
		ids = append(ids, item.ID)
	}
	if err != nil || !reflect.DeepEqual(ids, []string{"cnd", "t1", "t2"}) {
		t.Fatalf("tracked: %v %v", ids, err)
	}
	if tracked, err = s.TrackedSourcePrefixItems(ctx, "alice-id", store.SourceGmail, "gmail:a_b%c@example.com:", 1); err != nil || len(tracked) != 1 {
		t.Fatalf("tracked limit: %d %v", len(tracked), err)
	}
	if tracked, err = s.TrackedSourcePrefixItems(ctx, "bob-id", store.SourceGmail, "gmail:a_b%c@example.com:", 100); err != nil || len(tracked) != 0 {
		t.Fatalf("other user's tracked: %d %v", len(tracked), err)
	}
	keys, err := s.GmailJudgedKeys(ctx, "alice-id", "gmail:a_b%c@example.com:r:1a:", 0, 500)
	if err != nil || !reflect.DeepEqual(keys, []string{"gmail:a_b%c@example.com:r:1a:1b:1c"}) {
		t.Fatalf("judged keys: %v %v", keys, err)
	}
	if keys, err = s.GmailJudgedKeys(ctx, "alice-id", "gmail:a_b%c@example.com:r:", 150, 500); err != nil || len(keys) != 2 {
		t.Fatalf("judged keys since: %v %v", keys, err)
	}

	deleted, err := s.DeleteGmailAccount(ctx, "alice-id", "alice-session", target.ID, tokenTestNow+5)
	if err != nil || deleted.ID != target.ID || deleted.TokenSealed != target.TokenSealed {
		t.Fatalf("delete: %+v %v", deleted, err)
	}
	if _, err = s.DeleteGmailAccount(ctx, "alice-id", "alice-session", target.ID, tokenTestNow+5); !errors.Is(err, store.ErrConnectionNotFound) {
		t.Fatalf("second delete: %v", err)
	}
	for id, item := range items {
		got, err := s.ActionItem(ctx, "alice-id", id)
		if err != nil || got.SourceKey.String != item.SourceKey.String {
			t.Fatalf("%s: %+v %v", id, got, err)
		}
		stopped := strings.HasPrefix(item.SourceKey.String, "gmail:a_b%c@example.com:")
		if stopped != (got.Tracking == 0) || stopped != (got.Version == 2) {
			t.Fatalf("%s tracking: %+v", id, got)
		}
		has, err := s.HasNotification(ctx, "alice-id", "detected:"+id+":1")
		if err != nil || has == stopped {
			t.Fatalf("%s notification: %v %v", id, has, err)
		}
	}
	if has, err := s.HasNotification(ctx, "alice-id", "detected:slack:1"); err != nil || !has {
		t.Fatalf("Slack notification: %v %v", has, err)
	}
	if item, err := s.ActionItem(ctx, "alice-id", "slack"); err != nil || item.Tracking != 1 {
		t.Fatalf("Slack item: %+v %v", item, err)
	}
	list, err := s.GmailAccounts(ctx, "alice-id")
	if err != nil || len(list) != 2 {
		t.Fatalf("remaining: %d %v", len(list), err)
	}

	// A revoked session cannot write.
	if err = s.ChangeUser(ctx, "alice@example.com", "new", false, tokenTestNow); err != nil {
		t.Fatal(err)
	}
	if _, err = s.DeleteGmailAccount(ctx, "alice-id", "alice-session", other.ID, tokenTestNow); !errors.Is(err, store.ErrUnauthorized) {
		t.Fatalf("revoked delete: %v", err)
	}
	if _, err = s.UpdateGmailSettings(ctx, "alice-id", "alice-session", other.ID, true, tokenTestNow); !errors.Is(err, store.ErrUnauthorized) {
		t.Fatalf("revoked settings: %v", err)
	}
	if _, err = s.SaveGmailAccount(ctx, "alice-id", "alice-session", "z@example.com", func(string) (string, error) { return "x", nil }, tokenTestNow); !errors.Is(err, store.ErrUnauthorized) {
		t.Fatalf("revoked save: %v", err)
	}
}
