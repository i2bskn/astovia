package httpserver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/i2bskn/authlume/password"

	"github.com/i2bskn/astovia/internal/auth"
	"github.com/i2bskn/astovia/internal/config"
	"github.com/i2bskn/astovia/internal/store"
)

// rawExec runs query on the database of dsn outside the store.
func rawExec(t *testing.T, driver, dsn, query string) {
	t.Helper()
	db, err := sql.Open(map[string]string{"sqlite": "sqlite", "postgres": "pgx"}[driver], dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.ExecContext(context.Background(), query); err != nil {
		t.Fatal(err)
	}
}

// loginFrom posts a password sign-in from the peer addr.
func loginFrom(h http.Handler, addr, email, secret string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/auth/login", strings.NewReader(`{"email":"`+email+`","password":"`+secret+`"}`))
	r.RemoteAddr = addr
	r.Header.Set("Origin", usersOrigin)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// peer returns the address of the i-th peer, each counted on its own by
// the limit per peer.
func peer(i int) string { return fmt.Sprintf("198.51.%d.%d:1234", i/250, i%250+1) }

// expectEmailLimited checks the answer of an address over its limit.
func expectEmailLimited(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	expect(t, w, 429, "rate_limited")
	if got := w.Header().Get("Retry-After"); got != "900" {
		t.Fatalf("Retry-After: %q", got)
	}
	if issuedCookie(w) {
		t.Fatal("a limited sign-in set a cookie")
	}
}

// TestLoginEmailLimit checks the limit of failed password sign-ins per
// email address (spec §5): 10 in 15 minutes, counted across peers on the
// normalized address, unknown addresses alike, and reset by a success.
func TestLoginEmailLimit(t *testing.T) {
	api := newUsersAPI(t, "sqlite")
	now := *api.now
	api.srv.emailLimiter.now = func() time.Time { return now }
	h := api.handler
	n := 0
	next := func() string { n++; return peer(n) }

	for i := 1; i <= 10; i++ {
		expect(t, loginFrom(h, next(), "alice@example.com", "wrong-password-1"), 401, "")
	}
	// The 11th attempt is refused from any peer, in any spelling of the
	// address, and with the right password too.
	for _, email := range []string{"alice@example.com", " ALICE@Example.COM ", "Alice@example.com"} {
		expectEmailLimited(t, loginFrom(h, next(), email, "wrong-password-1"))
		expectEmailLimited(t, loginFrom(h, next(), email, "correct-password-123"))
	}
	// Another address is not limited.
	if w := loginFrom(h, next(), "carol@example.com", "correct-password-123"); w.Code != 200 || !issuedCookie(w) {
		t.Fatalf("carol: %d %s", w.Code, w.Body.String())
	}
	// The window ends 15 minutes after its first attempt.
	now = now.Add(15*time.Minute - time.Second)
	expectEmailLimited(t, loginFrom(h, next(), "alice@example.com", "correct-password-123"))
	now = now.Add(time.Second)
	if w := loginFrom(h, next(), "alice@example.com", "correct-password-123"); w.Code != 200 || !issuedCookie(w) {
		t.Fatalf("after the window: %d %s", w.Code, w.Body.String())
	}

	// A success forgets the failures before it.
	for i := 1; i <= 9; i++ {
		expect(t, loginFrom(h, next(), "alice@example.com", "wrong-password-1"), 401, "")
	}
	if w := loginFrom(h, next(), "alice@example.com", "correct-password-123"); w.Code != 200 {
		t.Fatalf("9 failures: %d %s", w.Code, w.Body.String())
	}
	for i := 1; i <= 10; i++ {
		expect(t, loginFrom(h, next(), "alice@example.com", "wrong-password-1"), 401, "")
	}
	expectEmailLimited(t, loginFrom(h, next(), "alice@example.com", "correct-password-123"))

	// An unknown address is counted the same way, so the answer does not
	// tell whether it exists.
	for i := 1; i <= 10; i++ {
		expect(t, loginFrom(h, next(), "nobody@example.com", "wrong-password-1"), 401, "")
	}
	expectEmailLimited(t, loginFrom(h, next(), "Nobody@Example.com", "wrong-password-1"))
}

// TestLoginEmailLimitConcurrent checks that attempts at once cannot pass
// the limit while their passwords are derived (spec §4.1).
func TestLoginEmailLimitConcurrent(t *testing.T) {
	api := newUsersAPI(t, "sqlite")
	const attempts = 30
	out := make([]*httptest.ResponseRecorder, attempts)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range attempts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			out[i] = loginFrom(api.handler, peer(i), "alice@example.com", "wrong-password-1")
		}()
	}
	close(start)
	wg.Wait()
	counts := map[int]int{}
	for _, w := range out {
		counts[w.Code]++
		if w.Code == 429 {
			expectEmailLimited(t, w)
		}
	}
	if counts[401] != emailLimit || counts[429] != attempts-emailLimit {
		t.Fatalf("answers: %v", counts)
	}
	expectEmailLimited(t, loginFrom(api.handler, peer(attempts), "alice@example.com", "correct-password-123"))
}

// TestPasswordLookup checks that an unknown, disabled or password-less
// user, and an input that is no address, all go through authlume's
// password check, which spends the derivation on each (spec §14), and all
// get the same answer.
func TestPasswordLookup(t *testing.T) {
	api := newUsersAPI(t, "sqlite")
	ctx := context.Background()
	for _, u := range []store.User{
		{ID: "dave-id", Email: "dave@example.com", PasswordHash: store.NoPasswordHash, CreatedAt: 3, Role: store.RoleMember},
		{ID: "erin-id", Email: "erin@example.com", PasswordHash: api.mustUser("alice-id").PasswordHash, CreatedAt: 4, Role: store.RoleMember},
	} {
		if err := api.store.CreateUser(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	disabled := true
	if _, err := api.store.UpdateUser(ctx, "alice-id", "erin-id", store.UserChange{Disabled: &disabled}, 5); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		identifier string
		found      bool
		hash       string
	}{
		{"nobody@example.com", false, ""},
		{"not an address", false, ""},
		{"erin@example.com", false, ""},
		{"dave@example.com", true, store.NoPasswordHash},
		{" Carol@Example.com", true, api.mustUser("carol-id").PasswordHash},
	} {
		var found store.User
		cred, err := api.srv.passwordLookup(&found)(ctx, tc.identifier)
		switch {
		case !tc.found && !errors.Is(err, password.ErrNotFound):
			t.Fatalf("%s: %v", tc.identifier, err)
		case tc.found && (err != nil || cred.Hash != tc.hash || cred.Subject != found.ID || found.ID == ""):
			t.Fatalf("%s: %+v %v", tc.identifier, cred, err)
		}
		if tc.found {
			continue
		}
		// Each of them is the same failure as a wrong password.
		if _, err = api.srv.verifyPassword(ctx, tc.identifier, "correct-password-123"); !errors.Is(err, store.ErrUnauthorized) {
			t.Fatalf("%s: verify: %v", tc.identifier, err)
		}
	}
	if _, err := api.srv.verifyPassword(ctx, "dave@example.com", store.NoPasswordHash); !errors.Is(err, store.ErrUnauthorized) {
		t.Fatalf("password-less user: %v", err)
	}
	want := loginFrom(api.handler, peer(1), "carol@example.com", "wrong-password-1")
	expect(t, want, 401, "")
	for i, email := range []string{"nobody@example.com", "erin@example.com", "dave@example.com"} {
		w := loginFrom(api.handler, peer(i+2), email, "correct-password-123")
		if w.Code != want.Code || w.Body.String() != want.Body.String() || issuedCookie(w) {
			t.Fatalf("%s: %d %s", email, w.Code, w.Body.String())
		}
	}
}

func (a *usersAPI) mustUser(id string) store.User {
	a.t.Helper()
	u, err := a.store.UserByID(context.Background(), id)
	if err != nil {
		a.t.Fatal(err)
	}
	return u
}

// TestSessionIssueRechecks checks that a session is not issued when the
// password changes or the user is disabled after the password was verified
// (spec §4): the store checks both again in the transaction that issues it.
func TestSessionIssueRechecks(t *testing.T) {
	mfaDrivers(t, func(t *testing.T, driver string) {
		api := newUsersAPI(t, driver)
		ctx := context.Background()
		signInAfter := func(email string, between func()) *httptest.ResponseRecorder {
			t.Helper()
			v, err := api.srv.verifyPassword(ctx, email, "correct-password-123")
			if err != nil || v.requireMFA {
				t.Fatalf("verify %s: %+v %v", email, v, err)
			}
			between()
			r := httptest.NewRequest("POST", "/auth/login", nil)
			r.Header.Set("Origin", usersOrigin)
			w := httptest.NewRecorder()
			api.srv.passwordSignIn(w, r, v, func(err error) { api.srv.authError(w, r, err) })
			return w
		}
		sessions := func() int {
			t.Helper()
			n, err := countRows(driver, api.dsn, "SELECT COUNT(*) FROM web_sessions")
			if err != nil {
				t.Fatal(err)
			}
			return n
		}

		if w := signInAfter("alice@example.com", func() {}); w.Code != 200 || !issuedCookie(w) {
			t.Fatalf("unchanged: %d %s", w.Code, w.Body.String())
		}
		before := sessions()
		hash, err := auth.Password("another-password-456")
		if err != nil {
			t.Fatal(err)
		}
		w := signInAfter("alice@example.com", func() {
			rawExec(t, driver, api.dsn, "UPDATE users SET password_hash = '"+hash+"' WHERE id = 'alice-id'")
		})
		noSession(t, w, 401, "")
		disabled := true
		w = signInAfter("carol@example.com", func() {
			if _, err := api.store.UpdateUser(ctx, "alice-id", "carol-id", store.UserChange{Disabled: &disabled}, 10); err != nil {
				t.Fatal(err)
			}
		})
		noSession(t, w, 401, "")
		if got := sessions(); got != before {
			t.Fatalf("sessions: %d, want %d", got, before)
		}
	})
}

// countRows returns the result of a SELECT COUNT(*) statement.
func countRows(driver, dsn, query string) (int, error) {
	db, err := sql.Open(map[string]string{"sqlite": "sqlite", "postgres": "pgx"}[driver], dsn)
	if err != nil {
		return 0, err
	}
	defer db.Close()
	var n int
	err = db.QueryRowContext(context.Background(), query).Scan(&n)
	return n, err
}

// originRoutes are the state-changing routes under /api/ and /auth/, which
// the browser session cookie authenticates.
var originRoutes = []struct{ method, path string }{
	{"POST", "/auth/login"},
	{"POST", "/auth/logout"},
	{"POST", "/auth/mfa/totp"},
	{"POST", "/auth/mfa/recovery"},
	{"POST", "/auth/mfa/enroll/totp/begin"},
	{"POST", "/auth/mfa/enroll/totp/finish"},
	{"POST", "/auth/mfa/webauthn/begin"},
	{"POST", "/auth/mfa/webauthn/finish"},
	{"POST", "/auth/mfa/enroll/webauthn/begin"},
	{"POST", "/auth/mfa/enroll/webauthn/finish"},
	{"POST", "/auth/invitations/inspect"},
	{"POST", "/auth/invitations/accept"},
	{"POST", "/auth/password-resets/inspect"},
	{"POST", "/auth/password-resets/complete"},
	{"POST", "/api/v1/mfa/totp/begin"},
	{"POST", "/api/v1/mfa/totp/finish"},
	{"POST", "/api/v1/mfa/totp/delete"},
	{"POST", "/api/v1/mfa/webauthn/begin"},
	{"POST", "/api/v1/mfa/webauthn/finish"},
	{"POST", "/api/v1/mfa/webauthn/x/delete"},
	{"POST", "/api/v1/mfa/recovery-codes"},
	{"PATCH", "/api/v1/users/x"},
	{"POST", "/api/v1/users/x/password-resets"},
	{"POST", "/api/v1/invitations"},
	{"DELETE", "/api/v1/invitations/x"},
	{"POST", "/api/v1/action-items"},
	{"PATCH", "/api/v1/action-items/x"},
	{"POST", "/api/v1/api-tokens"},
	{"DELETE", "/api/v1/api-tokens/x"},
	{"POST", "/api/v1/oauth/authorize"},
	{"PUT", "/api/v1/integrations/github"},
	{"PATCH", "/api/v1/integrations/github"},
	{"DELETE", "/api/v1/integrations/github"},
	{"PUT", "/api/v1/integrations/slack"},
	{"PATCH", "/api/v1/integrations/slack"},
	{"DELETE", "/api/v1/integrations/slack"},
	{"PUT", "/api/v1/integrations/chatwork"},
	{"PATCH", "/api/v1/integrations/chatwork"},
	{"DELETE", "/api/v1/integrations/chatwork"},
	{"POST", "/api/v1/integrations/gmail/authorize"},
	{"POST", "/api/v1/integrations/gmail/complete"},
	{"PATCH", "/api/v1/integrations/gmail/accounts/x"},
	{"DELETE", "/api/v1/integrations/gmail/accounts/x"},
	{"PUT", "/api/v1/ai"},
	{"DELETE", "/api/v1/ai"},
	{"PUT", "/api/v1/ai/instructions"},
	{"POST", "/api/v1/ai/instructions/try"},
	{"POST", "/api/v1/ai/openai/authorize"},
	{"POST", "/api/v1/ai/openai/complete"},
	{"POST", "/api/v1/push/subscriptions"},
	{"DELETE", "/api/v1/push/subscriptions/x"},
	{"POST", "/api/v1/push/test"},
	{"PUT", "/api/v1/notification-channels"},
	{"PUT", "/api/v1/notification-channels/slack-webhook"},
	{"DELETE", "/api/v1/notification-channels/slack-webhook"},
	{"POST", "/api/v1/notification-channels/test"},
	{"PUT", "/api/v1/notification-settings"},
	{"DELETE", "/api/v1/oauth/connections/x"},
}

// TestOriginRoutesComplete checks that originRoutes lists every
// state-changing method of every route under /api/ and /auth/ that the
// server registers, with and without OIDC; the OIDC sign-in form is posted
// across origins by design and has its own test.
func TestOriginRoutesComplete(t *testing.T) {
	listed := map[string]bool{}
	for _, r := range originRoutes {
		listed[r.method+" "+r.path] = true
	}
	registered := map[string]bool{}
	o := newOIDCAPI(t, "sqlite", nil)
	for _, srv := range []*Server{newUsersAPI(t, "sqlite").srv, o.srv} {
		h := srv.handler()
		if len(srv.patterns) == 0 {
			t.Fatal("no patterns recorded")
		}
		for _, p := range srv.patterns {
			if (!strings.HasPrefix(p, "/api/") && !strings.HasPrefix(p, "/auth/")) || p == oidcLoginPath {
				continue
			}
			path := strings.ReplaceAll(p, "{id}", "x")
			if strings.Contains(path, "{") {
				t.Fatalf("pattern %s: unknown wildcard", p)
			}
			for _, method := range []string{"POST", "PUT", "PATCH", "DELETE"} {
				r := httptest.NewRequest(method, path, strings.NewReader("{}"))
				r.Header.Set("Content-Type", "application/json")
				r.Header.Set("Origin", usersOrigin)
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				if w.Code != 405 {
					registered[method+" "+path] = true
				}
			}
		}
	}
	for route := range registered {
		if !listed[route] {
			t.Errorf("%s is not in originRoutes", route)
		}
	}
	for route := range listed {
		if !registered[route] {
			t.Errorf("%s is listed but not registered", route)
		}
	}
}

// TestOriginRequired checks the Origin check of every state-changing route
// (spec §5.4): with the session cookie, a missing or foreign Origin is
// refused before the handler runs; a request to the API with an API token
// is not checked.
func TestOriginRequired(t *testing.T) {
	api := newUsersAPI(t, "sqlite")
	api.srv.limiter = newKeyLimiter(1000000, time.Minute)
	api.handler = api.srv.handler()
	session, _ := api.login("alice@example.com", "correct-password-123")
	w := api.do("POST", "/api/v1/api-tokens", session, `{"name":"origin"}`)
	expect(t, w, 201, "")
	token := decodeJSON[createdAPITokenResponse](t, w).Token
	send := func(method, path string, origins []string, credential string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader("{}"))
		r.Header.Set("Content-Type", "application/json")
		for _, o := range origins {
			r.Header.Add("Origin", o)
		}
		switch {
		case strings.HasPrefix(credential, "ast_"):
			r.Header.Set("Authorization", "Bearer "+credential)
		case credential != "":
			r.Header.Set("Cookie", credential)
		}
		r.RemoteAddr = "192.0.2.10:1234"
		w := httptest.NewRecorder()
		api.handler.ServeHTTP(w, r)
		return w
	}
	foreign := [][]string{
		nil,
		{"https://evil.test"},
		{"null"},
		{""},
		{"http://astovia.test"},
		{"https://astovia.test/"},
		{"https://astovia.test:443"},
		{"https://ASTOVIA.test"},
		{"https://astovia.test.evil.test"},
		{usersOrigin, usersOrigin},
		{usersOrigin, "https://evil.test"},
	}
	for _, route := range originRoutes {
		for _, origins := range foreign {
			for _, credential := range []string{session, ""} {
				w := send(route.method, route.path, origins, credential)
				expect(t, w, 403, "origin_rejected")
				if len(w.Header().Values("Set-Cookie")) != 0 {
					t.Fatalf("%s %s %q: cookie set", route.method, route.path, origins)
				}
			}
		}
		// The table holds routes that exist: with the right Origin and no
		// credential, the route answers for itself.
		w := send(route.method, route.path, []string{usersOrigin}, "")
		if w.Code == 403 || w.Code == 405 || (w.Code == 404 && errorCode(t, w) == "not_found") {
			t.Fatalf("%s %s with the Origin: %d %s", route.method, route.path, w.Code, w.Body.String())
		}
		// An API token carries no cookie credential and is not checked on
		// the API; the /auth endpoints serve the browser only.
		for _, origins := range [][]string{nil, {"https://evil.test"}} {
			w := send(route.method, route.path, origins, token)
			rejected := w.Code == 403 && errorCode(t, w) == "origin_rejected"
			if rejected != strings.HasPrefix(route.path, "/auth/") {
				t.Fatalf("%s %s with an API token: %d %s", route.method, route.path, w.Code, w.Body.String())
			}
		}
		// Something that only looks like an API token is no reason to skip
		// the check, on the API or under /auth, with or without the cookie.
		for _, fake := range []string{"ast_x", "ast_", token + "x", token[:len(token)-1] + "!"} {
			for _, cookie := range []string{"", session} {
				r := httptest.NewRequest(route.method, route.path, strings.NewReader("{}"))
				r.Header.Set("Content-Type", "application/json")
				r.Header.Set("Origin", "https://evil.test")
				r.Header.Set("Authorization", "Bearer "+fake)
				if cookie != "" {
					r.Header.Set("Cookie", cookie)
				}
				w := httptest.NewRecorder()
				api.handler.ServeHTTP(w, r)
				expect(t, w, 403, "origin_rejected")
			}
		}
		// A well-formed token that is not one is taken as a token on the
		// API, where it gets a 401 and the cookie is not used.
		if strings.HasPrefix(route.path, "/api/") {
			r := httptest.NewRequest(route.method, route.path, strings.NewReader("{}"))
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Origin", "https://evil.test")
			r.Header.Set("Authorization", "Bearer ast_"+strings.Repeat("A", 43))
			r.Header.Set("Cookie", session)
			w := httptest.NewRecorder()
			api.handler.ServeHTTP(w, r)
			expect(t, w, 401, "unauthorized")
		}
		// Neither is a second Authorization header beside a valid token.
		r := httptest.NewRequest(route.method, route.path, strings.NewReader("{}"))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", "https://evil.test")
		r.Header.Add("Authorization", "Bearer "+token)
		r.Header.Add("Authorization", "Bearer "+token)
		r.Header.Set("Cookie", session)
		w = httptest.NewRecorder()
		api.handler.ServeHTTP(w, r)
		expect(t, w, 403, "origin_rejected")
	}
	// The session refused above still works, and an API token without an
	// Origin changes state where it is accepted.
	expect(t, api.do("GET", "/api/v1/me", session, ""), 200, "")
	expect(t, send("POST", "/api/v1/action-items", nil, token), 400, "")
	r := httptest.NewRequest("POST", "/api/v1/action-items", strings.NewReader(`{"content":"origin"}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+token)
	expect(t, serve(t, api.handler, r), 201, "")
	// Reads are not checked.
	r = httptest.NewRequest("GET", "/api/v1/me", nil)
	r.Header.Set("Cookie", session)
	r.Header.Set("Origin", "https://evil.test")
	expect(t, serve(t, api.handler, r), 200, "")
}

// TestLegacyMFALogin checks the sign-in of a user whose second factors
// were stored before this version and cannot be used (spec §6): the second
// factor is still asked for, and a recovery code signs in.
func TestLegacyMFALogin(t *testing.T) {
	mfaDrivers(t, func(t *testing.T, driver string) {
		api := newMFAAPI(t, driver, config.MFAOff)
		alice, _ := api.login("alice@example.com", mfaPassword)
		secret, codes := api.enroll(alice)
		rawExec(t, driver, api.dsn, "DELETE FROM mfa_totp WHERE user_id = 'alice-id'")
		rawExec(t, driver, api.dsn, "INSERT INTO mfa_totp (user_id,secret_sealed,last_step,created_at) VALUES ('alice-id','legacy',5,1)")
		rawExec(t, driver, api.dsn, "INSERT INTO mfa_webauthn_credentials (id,user_id,credential_id,credential,name,created_at) VALUES ('old','alice-id','legacy-old','{}','Legacy',1)")

		ch := api.pending("alice@example.com")
		if !ch.Enrolled || ch.Methods != (mfaMethodsResponse{Recovery: true}) {
			t.Fatalf("challenge: %+v", ch)
		}
		noSession(t, api.post("/auth/mfa/totp", map[string]string{"mfa_token": ch.Token, "code": api.code(secret, 0)}), 401, "mfa_code_invalid")
		w := api.post("/auth/mfa/recovery", map[string]string{"mfa_token": ch.Token, "code": codes[0]})
		session, _ := api.session(w)
		expect(t, api.do("GET", "/api/v1/me", session, ""), 200, "")
		if got := api.status(session); got.RecoveryCodesRemaining != 9 {
			t.Fatalf("status: %+v", got)
		}

		// A user with legacy factors only and no recovery code is still
		// asked for a second factor; the session is not started.
		rawExec(t, driver, api.dsn, "INSERT INTO mfa_totp (user_id,secret_sealed,last_step,created_at) VALUES ('carol-id','legacy',5,1)")
		ch = api.pending("carol@example.com")
		if !ch.Enrolled || ch.Methods != (mfaMethodsResponse{}) {
			t.Fatalf("carol: %+v", ch)
		}
	})
}

// TestSessionLifetime checks the lifetime of the browser session (spec §4):
// each authenticated request moves the idle limit of 7 days, and the
// session ends 30 days after the sign-in whatever its activity.
func TestSessionLifetime(t *testing.T) {
	mfaDrivers(t, func(t *testing.T, driver string) {
		api := newUsersAPI(t, driver)
		me := func(cookie string) int {
			t.Helper()
			return api.do("GET", "/api/v1/me", cookie, "").Code
		}
		day := 24 * time.Hour

		// Idle: used every 6 days, the session lives on; 7 days unused end it.
		idle, _ := api.login("alice@example.com", "correct-password-123")
		for range 3 {
			*api.now = api.now.Add(6 * day)
			if got := me(idle); got != 200 {
				t.Fatalf("used within the idle limit: %d", got)
			}
		}
		*api.now = api.now.Add(7*day - time.Second)
		if got := me(idle); got != 200 {
			t.Fatalf("just before the idle limit: %d", got)
		}
		*api.now = api.now.Add(7 * day)
		if got := me(idle); got != 401 {
			t.Fatalf("after the idle limit: %d", got)
		}

		// Absolute: the session ends 30 days after the sign-in even when it
		// is used every day.
		absolute, _ := api.login("carol@example.com", "correct-password-123")
		for i := 1; i < 30; i++ {
			*api.now = api.now.Add(day)
			if got := me(absolute); got != 200 {
				t.Fatalf("day %d: %d", i, got)
			}
		}
		*api.now = api.now.Add(day)
		if got := me(absolute); got != 401 {
			t.Fatalf("after 30 days: %d", got)
		}
	})
}

// TestSignInCookies checks the cookies a sign-in meets: a malformed session
// cookie does not stop it, the session cookie it presents is replaced, and
// the refresh cookie of earlier releases is expired on any /auth/ request.
func TestSignInCookies(t *testing.T) {
	mfaDrivers(t, func(t *testing.T, driver string) {
		api := newUsersAPI(t, driver)
		body := `{"email":"alice@example.com","password":"correct-password-123"}`
		for _, bad := range []string{SessionCookieName + "=not-base64!", SessionCookieName + "=", SessionCookieName + "=" + strings.Repeat("A", 4096)} {
			w := api.public("/auth/login", body, bad)
			expect(t, w, 200, "")
			expect(t, api.do("GET", "/api/v1/me", sessionCookie(t, w), ""), 200, "")
		}

		first := sessionCookie(t, api.public("/auth/login", body, ""))
		w := api.public("/auth/login", body, first)
		expect(t, w, 200, "")
		second := sessionCookie(t, w)
		if second == first {
			t.Fatal("the sign-in kept the session it was given")
		}
		expect(t, api.do("GET", "/api/v1/me", first, ""), 401, "unauthorized")
		expect(t, api.do("GET", "/api/v1/me", second, ""), 200, "")

		legacy := func(w *httptest.ResponseRecorder) bool {
			for _, c := range w.Result().Cookies() {
				if c.Name == legacyRefreshCookie {
					return c.Value == "" && c.MaxAge < 0 && c.Path == "/auth"
				}
			}
			return false
		}
		for _, tc := range []struct {
			path, body string
			status     int
		}{
			{"/auth/login", body, 200},
			{"/auth/login", `{"email":"alice@example.com","password":"wrong-password-123"}`, 401},
			{"/auth/logout", "{}", 204},
		} {
			w := api.public(tc.path, tc.body, legacyRefreshCookie+"=old-refresh-token")
			expect(t, w, tc.status, "")
			if !legacy(w) {
				t.Fatalf("%s: the refresh cookie is not expired: %v", tc.path, w.Header().Values("Set-Cookie"))
			}
		}
		if w := api.public("/auth/logout", "{}", ""); legacy(w) {
			t.Fatal("a refresh cookie was expired for a request without one")
		}
		// Outside /auth/ the browser does not send it.
		if w := api.do("GET", "/api/v1/me", second+"; "+legacyRefreshCookie+"=old-refresh-token", ""); legacy(w) {
			t.Fatal("the refresh cookie was expired outside /auth/")
		}
	})
}
