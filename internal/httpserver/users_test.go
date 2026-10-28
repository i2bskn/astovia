package httpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/i2bskn/astovia/internal/auth"
	"github.com/i2bskn/astovia/internal/config"
	"github.com/i2bskn/astovia/internal/store"
	"github.com/i2bskn/astovia/internal/testutil"
)

const usersOrigin = "https://astovia.test"

// usersAPI is a handler with alice (owner) and carol (member).
type usersAPI struct {
	t       *testing.T
	store   *store.Store
	dsn     string
	srv     *Server
	handler http.Handler
	now     *time.Time
}

func newUsersAPI(t *testing.T, driver string) *usersAPI {
	t.Helper()
	s, dsn := testutil.Database(t, driver)
	hash, err := auth.Password("correct-password-123")
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range []store.User{
		{ID: "alice-id", Email: "alice@example.com", PasswordHash: hash, CreatedAt: 1, Role: store.RoleOwner},
		{ID: "carol-id", Email: "carol@example.com", PasswordHash: hash, CreatedAt: 2, Role: store.RoleMember},
	} {
		if err = s.CreateUser(context.Background(), u); err != nil {
			t.Fatal(err)
		}
	}
	a, err := auth.New(s, make([]byte, 32), usersOrigin)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 23, 1, 2, 3, 0, time.UTC)
	a.Now = func() time.Time { return now }
	srv := newServer(config.Config{PublicURL: usersOrigin, Secure: true}, a, fstest.MapFS{})
	// The rate limit has its own test.
	srv.accountLimiter = newKeyLimiter(10000, time.Minute)
	return &usersAPI{t: t, store: s, dsn: dsn, srv: srv, handler: srv.handler(), now: &now}
}

// public posts body to a public /auth endpoint from the browser origin.
func (a *usersAPI) public(path, body, cookie string) *httptest.ResponseRecorder {
	a.t.Helper()
	r := httptest.NewRequest("POST", path, strings.NewReader(body))
	r.Header.Set("Origin", usersOrigin)
	r.Header.Set("Content-Type", "application/json")
	if cookie != "" {
		r.Header.Set("Cookie", cookie)
	}
	return serve(a.t, a.handler, r)
}

// session returns the access token and the refresh cookie of a login
// response.
func (a *usersAPI) session(w *httptest.ResponseRecorder) (string, string) {
	a.t.Helper()
	var result authResponse
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil {
		a.t.Fatalf("login response: %d %s", w.Code, w.Body.String())
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != CookieName || cookies[0].Value == "" || !cookies[0].HttpOnly || cookies[0].Path != "/auth" {
		a.t.Fatalf("cookie: %+v", cookies)
	}
	return result.AccessToken, cookies[0].Name + "=" + cookies[0].Value
}

func (a *usersAPI) login(email, password string) (string, string) {
	a.t.Helper()
	return a.session(a.public("/auth/login", `{"email":"`+email+`","password":"`+password+`"}`, ""))
}

func (a *usersAPI) do(method, path, bearer, body string) *httptest.ResponseRecorder {
	a.t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	return serve(a.t, a.handler, r)
}

// expect checks the status and, when code is not empty, the error code.
func expect(t *testing.T, w *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status %d, want %d: %s", w.Code, status, w.Body.String())
	}
	if code != "" && !strings.Contains(w.Body.String(), `"code":"`+code+`"`) {
		t.Fatalf("code, want %q: %s", code, w.Body.String())
	}
}

func decodeJSON[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("%v: %s", err, w.Body.String())
	}
	return out
}

// fragmentToken returns the token after # in a link.
func fragmentToken(t *testing.T, link, prefix string) string {
	t.Helper()
	token, ok := strings.CutPrefix(link, prefix)
	if !ok {
		t.Fatalf("link %q does not start with %q", link, prefix)
	}
	return token
}

func TestUsersAPI(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) { testUsersAPI(t, driver) })
	}
}

func testUsersAPI(t *testing.T, driver string) {
	api := newUsersAPI(t, driver)
	alice, aliceCookie := api.login("alice@example.com", "correct-password-123")
	carol, carolCookie := api.login("carol@example.com", "correct-password-123")

	// /api/v1/me reports the role.
	if me := decodeJSON[userResponse](t, api.do("GET", "/api/v1/me", carol, "")); me.Role != store.RoleMember || me.PasswordReset != nil {
		t.Fatalf("carol: %+v", me)
	}
	if me := decodeJSON[userResponse](t, api.do("GET", "/api/v1/me", alice, "")); me.Role != store.RoleOwner {
		t.Fatalf("alice: %+v", me)
	}

	// The owner API: 401 without an access JWT or with an API token, 403
	// for a member.
	token := decodeJSON[createdAPITokenResponse](t, api.do("POST", "/api/v1/api-tokens", alice, `{"name":"t"}`)).Token
	ownerRoutes := []struct{ method, path, body string }{
		{"GET", "/api/v1/users", ""},
		{"PATCH", "/api/v1/users/carol-id", `{"role":"owner"}`},
		{"POST", "/api/v1/users/carol-id/password-resets", ""},
		{"GET", "/api/v1/invitations", ""},
		{"POST", "/api/v1/invitations", `{"email":"x@example.com","role":"owner"}`},
		{"DELETE", "/api/v1/invitations/some-id", ""},
	}
	for _, rt := range ownerRoutes {
		expect(t, api.do(rt.method, rt.path, "", rt.body), 401, "unauthorized")
		expect(t, api.do(rt.method, rt.path, token, rt.body), 401, "unauthorized")
		expect(t, api.do(rt.method, rt.path, carol, rt.body), 403, "forbidden")
	}
	if role, _ := userState(t, api.store, "carol@example.com"); role != store.RoleMember {
		t.Fatal("a member changed its role")
	}

	// The user list.
	w := api.do("GET", "/api/v1/users", alice, "")
	expect(t, w, 200, "")
	users := decodeJSON[adminUserListResponse](t, w).Users
	if len(users) != 2 || users[0] != (adminUserResponse{ID: "alice-id", Email: "alice@example.com", Role: "owner", CreatedAt: "1970-01-01T00:00:01Z"}) || users[1].ID != "carol-id" || users[1].Disabled {
		t.Fatalf("users: %+v", users)
	}
	if strings.Contains(w.Body.String(), "password") {
		t.Fatalf("list leaks the hash: %s", w.Body.String())
	}
	var raw map[string][]map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if keys := sortedKeys(raw["users"][0]); !reflect.DeepEqual(keys, []string{"created_at", "disabled", "email", "id", "role"}) || string(raw["users"][0]["disabled"]) != "false" {
		t.Fatalf("user keys: %v", keys)
	}

	// Invitations: validation.
	for _, body := range []string{`{}`, `{"email":"dave@example.com"}`, `{"role":"member"}`, `{"email":"dave","role":"member"}`, `{"email":"dave@example.com","role":"admin"}`, `{"email":"dave@example.com","role":null}`, `{"email":"dave@example.com","role":"member","extra":1}`} {
		expect(t, api.do("POST", "/api/v1/invitations", alice, body), 400, "invalid_request")
	}
	expect(t, api.do("POST", "/api/v1/invitations", alice, `{"email":" Carol@Example.com ","role":"member"}`), 409, "email_taken")
	w = api.do("POST", "/api/v1/invitations", alice, `{"email":" Dave@Example.com ","role":"member"}`)
	expect(t, w, 201, "")
	if _, keys := jsonKeys(t, w.Body.Bytes()); !reflect.DeepEqual(keys, []string{"created_at", "email", "expires_at", "id", "role", "url"}) {
		t.Fatalf("invitation keys: %v", keys)
	}
	created := decodeJSON[createdInvitationResponse](t, w)
	if created.Email != "dave@example.com" || created.Role != "member" || created.CreatedAt != "2026-10-23T01:02:03Z" || created.ExpiresAt != "2026-10-30T01:02:03Z" {
		t.Fatalf("invitation: %+v", created)
	}
	daveToken := fragmentToken(t, created.URL, usersOrigin+"/invite#")
	if _, ok := auth.InvitationTokenHash(daveToken); !ok {
		t.Fatalf("invitation token %q", daveToken)
	}
	w = api.do("GET", "/api/v1/invitations", alice, "")
	expect(t, w, 200, "")
	list := decodeJSON[invitationListResponse](t, w).Invitations
	if len(list) != 1 || list[0] != (invitationResponse{ID: created.ID, Email: "dave@example.com", Role: "member", CreatedAt: created.CreatedAt, ExpiresAt: created.ExpiresAt, InvitedByEmail: "alice@example.com"}) {
		t.Fatalf("invitations: %+v", list)
	}
	if strings.Contains(w.Body.String(), daveToken) || strings.Contains(w.Body.String(), auth.Hash(daveToken)) {
		t.Fatal("list leaks the token")
	}

	// Inspecting the invitation, before signing in.
	w = api.public("/auth/invitations/inspect", `{"token":"`+daveToken+`"}`, "")
	expect(t, w, 200, "")
	if got := decodeJSON[inspectedInvitationResponse](t, w); got != (inspectedInvitationResponse{Email: "dave@example.com", Role: "member", ExpiresAt: created.ExpiresAt}) {
		t.Fatalf("inspect: %+v", got)
	}

	// A too short password is refused before anything is written.
	expect(t, api.public("/auth/invitations/accept", `{"token":"`+daveToken+`","password":"short"}`, ""), 400, "invalid_request")
	expect(t, api.public("/auth/invitations/inspect", `{"token":"`+daveToken+`"}`, ""), 200, "")

	// Accepting from a browser signed in as carol signs in as dave and
	// revokes carol's session of that browser.
	w = api.public("/auth/invitations/accept", `{"token":"`+daveToken+`","password":"dave-password-123"}`, carolCookie)
	expect(t, w, 200, "")
	body, keys := jsonKeys(t, w.Body.Bytes())
	if !reflect.DeepEqual(keys, []string{"access_token", "expires_in", "user"}) {
		t.Fatalf("accept keys: %v", keys)
	}
	var daveUser userResponse
	if err := json.Unmarshal(body["user"], &daveUser); err != nil || daveUser.Email != "dave@example.com" || daveUser.Role != "member" || daveUser.PasswordReset != nil {
		t.Fatalf("accept user: %+v %v", daveUser, err)
	}
	dave, daveCookie := api.session(w)
	if me := decodeJSON[userResponse](t, api.do("GET", "/api/v1/me", dave, "")); me.Email != "dave@example.com" {
		t.Fatalf("dave: %+v", me)
	}
	expect(t, api.do("GET", "/api/v1/me", carol, ""), 401, "unauthorized")
	expect(t, api.public("/auth/refresh", `{}`, carolCookie), 401, "unauthorized")
	expect(t, api.public("/auth/refresh", `{}`, daveCookie), 200, "")
	api.login("dave@example.com", "dave-password-123")
	carol, _ = api.login("carol@example.com", "correct-password-123")

	// The link works once.
	expect(t, api.public("/auth/invitations/accept", `{"token":"`+daveToken+`","password":"dave-password-123"}`, ""), 404, "invitation_invalid")

	// A user created after the invitation makes the acceptance fail and
	// keeps the invitation.
	erin := decodeJSON[createdInvitationResponse](t, api.do("POST", "/api/v1/invitations", alice, `{"email":"erin@example.com","role":"owner"}`))
	erinToken := fragmentToken(t, erin.URL, usersOrigin+"/invite#")
	if err := api.store.CreateUser(context.Background(), store.User{ID: "erin-id", Email: "erin@example.com", PasswordHash: "x", Role: store.RoleMember}); err != nil {
		t.Fatal(err)
	}
	expect(t, api.public("/auth/invitations/accept", `{"token":"`+erinToken+`","password":"erin-password-123"}`, ""), 409, "email_taken")
	expect(t, api.public("/auth/invitations/inspect", `{"token":"`+erinToken+`"}`, ""), 200, "")

	// Revoking.
	expect(t, api.do("DELETE", "/api/v1/invitations/"+erin.ID, alice, ""), 204, "")
	expect(t, api.do("DELETE", "/api/v1/invitations/"+erin.ID, alice, ""), 404, "not_found")
	expect(t, api.do("DELETE", "/api/v1/invitations/bad.id", alice, ""), 404, "not_found")
	expect(t, api.do("DELETE", "/api/v1/invitations/"+created.ID, alice, ""), 404, "not_found")

	// Invalid, unknown, accepted, revoked and expired invitations all get
	// the same answer.
	frank := decodeJSON[createdInvitationResponse](t, api.do("POST", "/api/v1/invitations", alice, `{"email":"frank@example.com","role":"member"}`))
	frankToken := fragmentToken(t, frank.URL, usersOrigin+"/invite#")
	unknown, _, err := auth.NewInvitationToken()
	if err != nil {
		t.Fatal(err)
	}
	reset, _, err := auth.NewPasswordResetToken()
	if err != nil {
		t.Fatal(err)
	}
	var invalid string
	*api.now = api.now.Add(7 * 24 * time.Hour)
	for _, v := range []string{"", "x", unknown, reset, daveToken, erinToken, frankToken, strings.TrimPrefix(frankToken, "inv_")} {
		for _, path := range []string{"/auth/invitations/inspect", "/auth/invitations/accept"} {
			w = api.public(path, linkBody(path, v), "")
			expect(t, w, 404, "invitation_invalid")
			if invalid == "" {
				invalid = w.Body.String()
			}
			if w.Body.String() != invalid {
				t.Fatalf("%s %q: %s", path, v, w.Body.String())
			}
		}
	}
	*api.now = api.now.Add(-7 * 24 * time.Hour)
	if _, err = api.store.UserByEmail(context.Background(), "frank@example.com"); err == nil {
		t.Fatal("an expired invitation created a user")
	}

	// Changing users: validation.
	for _, b := range []string{`{}`, `{"role":"owner","disabled":true}`, `{"role":null}`, `{"disabled":null}`, `{"role":"admin"}`, `{"disabled":"true"}`, `{"role":1}`, `{"name":"x"}`} {
		expect(t, api.do("PATCH", "/api/v1/users/carol-id", alice, b), 400, "invalid_request")
	}
	expect(t, api.do("PATCH", "/api/v1/users/missing", alice, `{"role":"owner"}`), 404, "not_found")
	expect(t, api.do("PATCH", "/api/v1/users/bad.id", alice, `{"role":"owner"}`), 404, "not_found")
	w = api.do("PATCH", "/api/v1/users/carol-id", alice, `{"role":"owner"}`)
	r := httptest.NewRequest("PATCH", "/api/v1/users/carol-id", strings.NewReader(`{"role":"owner"}`))
	r.Header.Set("Authorization", "Bearer "+alice)
	expect(t, serve(t, api.handler, r), 415, "unsupported_media_type")
	expect(t, w, 200, "")
	if got := decodeJSON[adminUserResponse](t, w); got.Role != "owner" || got.ID != "carol-id" {
		t.Fatalf("promote: %+v", got)
	}
	// The rules.
	expect(t, api.do("PATCH", "/api/v1/users/alice-id", alice, `{"disabled":true}`), 409, "self")
	expect(t, api.do("PATCH", "/api/v1/users/carol-id", alice, `{"role":"member"}`), 200, "")
	expect(t, api.do("PATCH", "/api/v1/users/alice-id", alice, `{"role":"member"}`), 409, "last_owner")
	// A no-op change succeeds.
	if got := decodeJSON[adminUserResponse](t, api.do("PATCH", "/api/v1/users/alice-id", alice, `{"role":"owner"}`)); got.Role != "owner" {
		t.Fatalf("no-op: %+v", got)
	}
	// Disabling signs the user out; links cannot be issued for it.
	if got := decodeJSON[adminUserResponse](t, api.do("PATCH", "/api/v1/users/carol-id", alice, `{"disabled":true}`)); !got.Disabled {
		t.Fatalf("disable: %+v", got)
	}
	expect(t, api.do("GET", "/api/v1/me", carol, ""), 401, "unauthorized")
	expect(t, api.do("POST", "/api/v1/users/carol-id/password-resets", alice, ""), 409, "user_disabled")
	if got := decodeJSON[adminUserResponse](t, api.do("PATCH", "/api/v1/users/carol-id", alice, `{"disabled":false}`)); got.Disabled {
		t.Fatalf("enable: %+v", got)
	}
	expect(t, api.do("POST", "/api/v1/users/missing/password-resets", alice, ""), 404, "not_found")
	expect(t, api.do("POST", "/api/v1/users/bad.id/password-resets", alice, ""), 404, "not_found")

	// A password reset link; a body is ignored.
	r = httptest.NewRequest("POST", "/api/v1/users/carol-id/password-resets", strings.NewReader("ignored"))
	r.Header.Set("Authorization", "Bearer "+alice)
	r.Header.Set("Content-Type", "text/plain")
	w = serve(t, api.handler, r)
	expect(t, w, 201, "")
	if _, keys = jsonKeys(t, w.Body.Bytes()); !reflect.DeepEqual(keys, []string{"expires_at", "url"}) {
		t.Fatalf("reset keys: %v", keys)
	}
	link := decodeJSON[createdPasswordResetResponse](t, w)
	if link.ExpiresAt != "2026-10-24T01:02:03Z" {
		t.Fatalf("reset: %+v", link)
	}
	carolToken := fragmentToken(t, link.URL, usersOrigin+"/reset-password#")
	if _, ok := auth.PasswordResetTokenHash(carolToken); !ok {
		t.Fatalf("reset token %q", carolToken)
	}
	w = api.public("/auth/password-resets/inspect", `{"token":"`+carolToken+`"}`, "")
	expect(t, w, 200, "")
	if got := decodeJSON[inspectedPasswordResetResponse](t, w); got.Email != "carol@example.com" {
		t.Fatalf("inspect reset: %+v", got)
	}
	if _, keys = jsonKeys(t, w.Body.Bytes()); !reflect.DeepEqual(keys, []string{"email"}) {
		t.Fatalf("inspect reset keys: %v", keys)
	}
	expect(t, api.public("/auth/password-resets/complete", `{"token":"`+carolToken+`","password":"short"}`, ""), 400, "invalid_request")

	// Completing from alice's browser signs in as carol, with the notice,
	// and revokes alice's session of that browser.
	*api.now = api.now.Add(time.Minute)
	w = api.public("/auth/password-resets/complete", `{"token":"`+carolToken+`","password":"carol-new-password"}`, aliceCookie)
	expect(t, w, 200, "")
	notice := &passwordResetResponse{At: "2026-10-23T01:03:03Z", ByEmail: "alice@example.com"}
	if got := decodeJSON[authResponse](t, w).User; got.Email != "carol@example.com" || got.Role != "member" || !reflect.DeepEqual(got.PasswordReset, notice) {
		t.Fatalf("complete user: %+v", got)
	}
	carol, _ = api.session(w)
	expect(t, api.public("/auth/refresh", `{}`, aliceCookie), 401, "unauthorized")
	expect(t, api.do("GET", "/api/v1/me", alice, ""), 401, "unauthorized")
	if me := decodeJSON[userResponse](t, api.do("GET", "/api/v1/me", carol, "")); !reflect.DeepEqual(me.PasswordReset, notice) {
		t.Fatalf("me after reset: %+v", me)
	}
	expect(t, api.public("/auth/login", `{"email":"carol@example.com","password":"correct-password-123"}`, ""), 401, "unauthorized")
	_, cookie := api.login("carol@example.com", "carol-new-password")
	if got := decodeJSON[authResponse](t, api.public("/auth/refresh", `{}`, cookie)).User; !reflect.DeepEqual(got.PasswordReset, notice) {
		t.Fatalf("refresh after reset: %+v", got)
	}
	for _, path := range []string{"/auth/password-resets/inspect", "/auth/password-resets/complete"} {
		expect(t, api.public(path, linkBody(path, carolToken), ""), 404, "reset_invalid")
	}
	// The notice is shown for 14 days.
	*api.now = api.now.Add(14*24*time.Hour - time.Second)
	carol, _ = api.login("carol@example.com", "carol-new-password")
	if me := decodeJSON[userResponse](t, api.do("GET", "/api/v1/me", carol, "")); me.PasswordReset == nil {
		t.Fatal("notice ended early")
	}
	*api.now = api.now.Add(time.Second)
	if me := decodeJSON[userResponse](t, api.do("GET", "/api/v1/me", carol, "")); me.PasswordReset != nil {
		t.Fatalf("notice after 14 days: %+v", me)
	}

	// A demoted owner keeps its session but loses the owner API at once.
	alice, _ = api.login("alice@example.com", "correct-password-123")
	expect(t, api.do("PATCH", "/api/v1/users/carol-id", alice, `{"role":"owner"}`), 200, "")
	expect(t, api.do("PATCH", "/api/v1/users/alice-id", carol, `{"role":"member"}`), 200, "")
	if me := decodeJSON[userResponse](t, api.do("GET", "/api/v1/me", alice, "")); me.Role != "member" {
		t.Fatalf("demoted alice: %+v", me)
	}
	expect(t, api.do("GET", "/api/v1/users", alice, ""), 403, "forbidden")
}

func sortedKeys(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// linkBody is the body of a public link endpoint: the token, and a valid
// password for the endpoints that take one.
func linkBody(path, token string) string {
	if strings.HasSuffix(path, "/inspect") {
		return `{"token":"` + token + `"}`
	}
	return `{"token":"` + token + `","password":"long-enough-password"}`
}

func userState(t *testing.T, s *store.Store, email string) (string, int64) {
	t.Helper()
	u, err := s.UserByEmail(context.Background(), email)
	if err != nil {
		t.Fatal(err)
	}
	return u.Role, u.Disabled
}

// Link tokens presented as bearer tokens are not credentials anywhere.
func TestLinkTokensAsBearer(t *testing.T) {
	api := newUsersAPI(t, "sqlite")
	alice, _ := api.login("alice@example.com", "correct-password-123")
	inv := decodeJSON[createdInvitationResponse](t, api.do("POST", "/api/v1/invitations", alice, `{"email":"dave@example.com","role":"owner"}`))
	pwr := decodeJSON[createdPasswordResetResponse](t, api.do("POST", "/api/v1/users/carol-id/password-resets", alice, ""))
	for _, token := range []string{fragmentToken(t, inv.URL, usersOrigin+"/invite#"), fragmentToken(t, pwr.URL, usersOrigin+"/reset-password#")} {
		for _, path := range []string{"/api/v1/action-items", "/api/v1/me", "/api/v1/users"} {
			expect(t, api.do("GET", path, token, ""), 401, "unauthorized")
		}
		if w := mcpPost(t, api.handler, token, "", ""); w.Code != 401 {
			t.Fatalf("/mcp with %q: %d %s", token[:4], w.Code, w.Body.String())
		}
	}
}

// The public link endpoints are browser JSON endpoints and share a rate
// limit per peer.
func TestAccountLinkEndpoints(t *testing.T) {
	api := newUsersAPI(t, "sqlite")
	paths := []string{"/auth/invitations/inspect", "/auth/invitations/accept", "/auth/password-resets/inspect", "/auth/password-resets/complete"}
	for _, path := range paths {
		for _, tc := range []struct {
			origin, ct, body string
			status           int
		}{
			{"", "application/json", `{"token":"x"}`, 403},
			{"https://evil.test", "application/json", `{"token":"x"}`, 403},
			{usersOrigin, "text/plain", `{"token":"x"}`, 415},
			{usersOrigin, "application/json", `{"token":"x","extra":1}`, 400},
			{usersOrigin, "application/json", `null`, 400},
			{usersOrigin, "application/json", `{"token":"` + strings.Repeat("x", 4096) + `"}`, 413},
		} {
			r := httptest.NewRequest("POST", path, strings.NewReader(tc.body))
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
			}
			r.Header.Set("Content-Type", tc.ct)
			if w := serve(t, api.handler, r); w.Code != tc.status || len(w.Result().Cookies()) != 0 {
				t.Fatalf("%s %+v: %d %s", path, tc, w.Code, w.Body.String())
			}
		}
		r := httptest.NewRequest("GET", path, nil)
		if w := serve(t, api.handler, r); w.Code != 405 || w.Header().Get("Allow") != "POST" {
			t.Fatalf("GET %s: %d", path, w.Code)
		}
	}

	// Ten attempts a minute over the four endpoints together; the eleventh
	// is refused, and other peers are not affected.
	api.srv.accountLimiter = newLoginLimiter()
	for i := 0; i < 10; i++ {
		w := api.public(paths[i%len(paths)], linkBody(paths[i%len(paths)], "x"), "")
		expect(t, w, 404, "")
	}
	for _, path := range paths {
		w := api.public(path, `{"token":"x"}`, "")
		expect(t, w, 429, "rate_limited")
		if w.Header().Get("Retry-After") != "60" {
			t.Fatalf("Retry-After: %q", w.Header().Get("Retry-After"))
		}
	}
	r := httptest.NewRequest("POST", paths[0], strings.NewReader(`{"token":"x"}`))
	r.Header.Set("Origin", usersOrigin)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Forwarded-For", "198.51.100.7")
	expect(t, serve(t, api.handler, r), 429, "rate_limited")
	r = httptest.NewRequest("POST", paths[0], strings.NewReader(`{"token":"x"}`))
	r.RemoteAddr = "198.51.100.7:1234"
	r.Header.Set("Origin", usersOrigin)
	r.Header.Set("Content-Type", "application/json")
	expect(t, serve(t, api.handler, r), 404, "invitation_invalid")
	// Login keeps its own limiter.
	api.login("alice@example.com", "correct-password-123")
}

// A failure to read the password reset notice does not fail login,
// refresh or /api/v1/me; the notice is null.
func TestResetNoticeFailure(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			api := newUsersAPI(t, driver)
			name := "sqlite"
			if driver == "postgres" {
				name = "pgx"
			}
			raw, err := sql.Open(name, api.dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = raw.Close() }()
			if _, err = raw.ExecContext(context.Background(), "DROP TABLE password_resets"); err != nil {
				t.Fatal(err)
			}
			w := api.public("/auth/login", `{"email":"alice@example.com","password":"correct-password-123"}`, "")
			access, cookie := api.session(w)
			if !strings.Contains(w.Body.String(), `"password_reset":null`) {
				t.Fatalf("login: %s", w.Body.String())
			}
			w = api.public("/auth/refresh", `{}`, cookie)
			if _, next := api.session(w); next == cookie || !strings.Contains(w.Body.String(), `"password_reset":null`) {
				t.Fatalf("refresh: %s", w.Body.String())
			}
			w = api.do("GET", "/api/v1/me", access, "")
			expect(t, w, 200, "")
			if !strings.Contains(w.Body.String(), `"password_reset":null`) || !strings.Contains(w.Body.String(), `"role":"owner"`) {
				t.Fatalf("me: %s", w.Body.String())
			}
		})
	}
}
