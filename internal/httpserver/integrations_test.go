package httpserver

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/i2bskn/astovia/internal/auth"
	"github.com/i2bskn/astovia/internal/config"
	"github.com/i2bskn/astovia/internal/githubsync"
	"github.com/i2bskn/astovia/internal/slacksync"
	"github.com/i2bskn/astovia/internal/store"
	"github.com/i2bskn/astovia/internal/testutil"
)

// fakeUser answers GitHub's GET /user with a configurable status and scopes.
type fakeUser struct {
	mu       sync.Mutex
	status   int
	scopes   *string
	header   map[string]string
	body     string
	login    string
	requests []*http.Request
}

func (f *fakeUser) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r)
	if f.scopes != nil {
		w.Header().Set("X-OAuth-Scopes", *f.scopes)
	}
	for k, v := range f.header {
		w.Header().Set(k, v)
	}
	if f.status != 0 && f.status != 200 {
		w.WriteHeader(f.status)
		_, _ = w.Write([]byte(f.body))
		return
	}
	_, _ = w.Write([]byte(`{"login":"` + f.login + `"}`))
}

func (f *fakeUser) set(status int, scopes *string, header map[string]string, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status, f.scopes, f.header, f.body = status, scopes, header, body
}

func (f *fakeUser) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func scopes(v string) *string { return &v }

type integrationAPI struct {
	t       *testing.T
	store   *store.Store
	server  *Server
	handler http.Handler
	now     *time.Time
	gh      *fakeUser
	sl      *fakeSlackAuth
	alice   string
	bob     string
}

func newIntegrationAPI(t *testing.T, driver string) *integrationAPI {
	t.Helper()
	s, _ := testutil.Database(t, driver)
	hash, err := auth.Password("correct-password-123")
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range []store.User{{ID: "alice-id", Email: "alice@example.com", PasswordHash: hash}, {ID: "bob-id", Email: "bob@example.com", PasswordHash: hash}} {
		if err = s.CreateUser(context.Background(), u); err != nil {
			t.Fatal(err)
		}
	}
	a, err := auth.New(s, make([]byte, 32), "https://astovia.test")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 24, 1, 2, 3, 0, time.UTC)
	a.Now = func() time.Time { return now }
	gh := &fakeUser{login: "octocat", scopes: scopes("notifications, repo")}
	srv := httptest.NewServer(gh)
	t.Cleanup(srv.Close)
	server := newServer(config.Config{PublicURL: "https://astovia.test", Secure: true, Secret: make([]byte, 32)}, a, fstest.MapFS{"sw.js": {Data: []byte("self")}, "manifest.webmanifest": {Data: []byte("{}")}, "icon-192.png": {Data: []byte("png")}, "index.html": {Data: []byte("<html>")}})
	server.github.BaseURL = srv.URL
	sl := &fakeSlackAuth{team: "T1", user: "U1", scopes: strings.Join(slacksync.RequiredScopes, ",")}
	slackSrv := httptest.NewServer(sl)
	t.Cleanup(slackSrv.Close)
	server.slack.BaseURL = slackSrv.URL
	api := &integrationAPI{t: t, store: s, server: server, handler: server.handler(), now: &now, gh: gh, sl: sl}
	tok := &tokenAPI{t: t, handler: api.handler}
	api.alice = tok.login("alice")
	api.bob = tok.login("bob")
	return api
}

func (a *integrationAPI) do(method, path, bearer, body string) *httptest.ResponseRecorder {
	a.t.Helper()
	return (&tokenAPI{t: a.t, handler: a.handler}).do(method, path, bearer, body)
}

func (a *integrationAPI) connect(jwt string) githubResponse {
	a.t.Helper()
	w := a.do("PUT", "/api/v1/integrations/github", jwt, `{"token":"ghp_0123456789abcdefWXYZ"}`)
	if w.Code != 200 {
		a.t.Fatalf("connect: %d %s", w.Code, w.Body.String())
	}
	var out githubResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		a.t.Fatal(err)
	}
	return out
}

func TestGitHubIntegrationAPI(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) { testGitHubIntegrationAPI(t, driver) })
	}
}

func testGitHubIntegrationAPI(t *testing.T, driver string) {
	api := newIntegrationAPI(t, driver)
	const path = "/api/v1/integrations/github"
	w := api.do("GET", path, api.alice, "")
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"connected":false}` {
		t.Fatalf("disconnected: %d %s", w.Code, w.Body.String())
	}

	// Malformed tokens are rejected before GitHub is called.
	for _, body := range []string{`{}`, `{"token":""}`, `{"token":"ghp abc"}`, `{"token":"ghp_é"}`, `{"token":null}`, `{"token":1}`, `{"token":"` + strings.Repeat("x", 256) + `"}`, `{"token":"x","immediate_notify":null}`, `{"token":"x","immediate_notify":"yes"}`, `{"token":"x","other":1}`} {
		expectStatus(t, api.do("PUT", path, api.alice, body), 400, "invalid_request")
	}
	if api.gh.count() != 0 {
		t.Fatalf("GitHub called %d times for invalid input", api.gh.count())
	}

	// Answers from GitHub. Nothing is saved on failure.
	for _, tc := range []struct {
		name   string
		status int
		scopes *string
		header map[string]string
		body   string
		want   int
		code   string
	}{
		{"401", 401, nil, nil, "", 400, "github_token_invalid"},
		{"plain 403", 403, nil, nil, `{"message":"SSO"}`, 400, "github_token_invalid"},
		{"rate limited", 403, nil, map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": "1793000000"}, "", 503, "github_unavailable"},
		{"secondary limit", 403, nil, nil, `{"message":"secondary rate limit"}`, 503, "github_unavailable"},
		{"500", 500, nil, nil, "", 503, "github_unavailable"},
		{"no scopes header (fine-grained)", 200, nil, nil, "", 400, "github_scopes_missing"},
		{"notifications only", 200, scopes("notifications"), nil, "", 400, "github_scopes_missing"},
	} {
		api.gh.set(tc.status, tc.scopes, tc.header, tc.body)
		expectStatus(t, api.do("PUT", path, api.alice, `{"token":"ghp_0123456789abcdefWXYZ"}`), tc.want, tc.code)
		if _, err := api.store.GitHubConnection(context.Background(), "alice-id"); err == nil {
			t.Fatalf("%s: saved", tc.name)
		}
	}

	// repo alone is enough.
	api.gh.set(200, scopes("repo"), nil, "")
	w = api.do("PUT", path, api.alice, `{"token":"ghp_0123456789abcdefWXYZ"}`)
	if w.Code != 200 || strings.Contains(w.Body.String(), "ghp_") {
		t.Fatalf("connect: %d %s", w.Code, w.Body.String())
	}
	body, keys := jsonKeys(t, w.Body.Bytes())
	if !reflect.DeepEqual(keys, []string{"connected", "created_at", "immediate_notify", "last_error", "last_synced_at", "login", "scopes", "status", "token_last4"}) {
		t.Fatalf("keys: %v", keys)
	}
	var got githubResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Connected || got.Login != "octocat" || got.TokenLast4 != "WXYZ" || !reflect.DeepEqual(got.Scopes, []string{"repo"}) || !got.ImmediateNotify || got.Status != "active" || got.LastError != nil || got.LastSyncedAt != nil || got.CreatedAt != "2026-10-24T01:02:03Z" || string(body["last_error"]) != "null" {
		t.Fatalf("connected: %+v", got)
	}
	api.gh.mu.Lock()
	last := api.gh.requests[len(api.gh.requests)-1]
	api.gh.mu.Unlock()
	if last.URL.Path != "/user" || last.Header.Get("Authorization") != "Bearer ghp_0123456789abcdefWXYZ" {
		t.Fatalf("GitHub request: %s %v", last.URL, last.Header)
	}
	// The token is stored sealed for alice only.
	c, err := api.store.GitHubConnection(context.Background(), "alice-id")
	if err != nil || strings.Contains(c.TokenSealed, "ghp_") {
		t.Fatalf("stored: %+v %v", c, err)
	}
	if plain, err := api.server.box.Open(githubsync.TokenPurpose("alice-id"), c.TokenSealed); err != nil || plain != "ghp_0123456789abcdefWXYZ" {
		t.Fatalf("open: %q %v", plain, err)
	}
	if _, err = api.server.box.Open(githubsync.TokenPurpose("bob-id"), c.TokenSealed); err == nil {
		t.Fatal("token opens for another user")
	}
	if w = api.do("GET", path, api.alice, ""); w.Code != 200 || strings.Contains(w.Body.String(), "ghp_") || !strings.Contains(w.Body.String(), `"token_last4":"WXYZ"`) {
		t.Fatalf("GET: %d %s", w.Code, w.Body.String())
	}

	// PATCH changes only immediate_notify; PUT keeps it when omitted.
	for _, body := range []string{`{}`, `{"immediate_notify":null}`, `{"immediate_notify":1}`, `{"immediate_notify":false,"token":"x"}`} {
		expectStatus(t, api.do("PATCH", path, api.alice, body), 400, "invalid_request")
	}
	if w = api.do("PATCH", path, api.alice, `{"immediate_notify":false}`); w.Code != 200 || !strings.Contains(w.Body.String(), `"immediate_notify":false`) {
		t.Fatalf("PATCH: %d %s", w.Code, w.Body.String())
	}
	if got = api.connect(api.alice); got.ImmediateNotify {
		t.Fatal("PUT reset immediate_notify")
	}
	if w = api.do("PUT", path, api.alice, `{"token":"ghp_0123456789abcdefWXYZ","immediate_notify":true}`); w.Code != 200 || !strings.Contains(w.Body.String(), `"immediate_notify":true`) {
		t.Fatalf("PUT immediate_notify: %d %s", w.Code, w.Body.String())
	}
	expectStatus(t, api.do("PATCH", path, api.bob, `{"immediate_notify":true}`), 404, "not_found")

	// A new login untracks the items of the previous one.
	c, err = api.store.GitHubConnection(context.Background(), "alice-id")
	if err != nil {
		t.Fatal(err)
	}
	err = api.store.GitHubSync(context.Background(), "alice-id", c.Generation, func(tx *store.GitHubTx) error {
		_, e := tx.CreateItem(store.ActionItem{ID: "gh-item", Content: "レビュー: x", Status: "open", Refs: "[]", Version: 1, CreatedAt: 1, UpdatedAt: 1, Source: sql.NullString{String: "github", Valid: true}, SourceKey: sql.NullString{String: "github:review:o/r#1", Valid: true}, Tracking: 1})
		return e
	})
	if err != nil {
		t.Fatal(err)
	}
	api.gh.mu.Lock()
	api.gh.login = "someone"
	api.gh.mu.Unlock()
	api.connect(api.alice)
	if item, err := api.store.ActionItem(context.Background(), "alice-id", "gh-item"); err != nil || item.Tracking != 0 || item.SourceKey.Valid {
		t.Fatalf("item after login change: %+v %v", item, err)
	}

	// DELETE is idempotent.
	for i := 0; i < 2; i++ {
		if w = api.do("DELETE", path, api.alice, ""); w.Code != 204 {
			t.Fatalf("DELETE: %d %s", w.Code, w.Body.String())
		}
	}
	if w = api.do("GET", path, api.alice, ""); strings.TrimSpace(w.Body.String()) != `{"connected":false}` {
		t.Fatalf("after DELETE: %s", w.Body.String())
	}

	// API tokens cannot manage integrations or notifications.
	tok := (&tokenAPI{t: t, handler: api.handler}).issue(api.alice, "x").Token
	for _, r := range [][2]string{{"GET", path}, {"PUT", path}, {"PATCH", path}, {"DELETE", path}, {"GET", "/api/v1/push/public-key"}, {"GET", "/api/v1/push/subscriptions"}, {"POST", "/api/v1/push/subscriptions"}, {"DELETE", "/api/v1/push/subscriptions/abc"}, {"POST", "/api/v1/push/test"}, {"GET", "/api/v1/notification-settings"}, {"PUT", "/api/v1/notification-settings"}} {
		expectStatus(t, api.do(r[0], r[1], tok, `{}`), 401, "unauthorized")
	}
}

// fakeSlackAuth answers Slack's auth.test. status other than 200 is
// returned as is; code makes the answer "ok": false.
type fakeSlackAuth struct {
	mu       sync.Mutex
	status   int
	code     string
	scopes   string
	team     string
	user     string
	url      string
	requests []*http.Request
}

func (f *fakeSlackAuth) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r)
	if f.status != 0 && f.status != 200 {
		w.WriteHeader(f.status)
		return
	}
	if f.code != "" {
		_, _ = w.Write([]byte(`{"ok":false,"error":"` + f.code + `"}`))
		return
	}
	w.Header().Set("X-OAuth-Scopes", f.scopes)
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "url": f.url, "team": "Acme", "user": "alice", "team_id": f.team, "user_id": f.user})
}

func (f *fakeSlackAuth) set(fn func(f *fakeSlackAuth)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *fakeSlackAuth) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

const slackToken = "xoxp-0123456789-abcdefWXYZ"

func TestSlackIntegrationAPI(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) { testSlackIntegrationAPI(t, driver) })
	}
}

func testSlackIntegrationAPI(t *testing.T, driver string) {
	api := newIntegrationAPI(t, driver)
	ctx := context.Background()
	const path = "/api/v1/integrations/slack"
	if w := api.do("GET", path, api.alice, ""); w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"connected":false}` {
		t.Fatalf("disconnected: %d %s", w.Code, w.Body.String())
	}

	// Malformed input is rejected before Slack is called.
	for _, body := range []string{`{}`, `{"token":""}`, `{"token":"xoxp- a"}`, `{"token":null}`, `{"token":"` + strings.Repeat("x", 256) + `"}`, `{"token":"xoxp-1","detect_dms":"no"}`, `{"token":"xoxp-1","detect_group_dms":null}`, `{"token":"xoxp-1","other":1}`} {
		expectStatus(t, api.do("PUT", path, api.alice, body), 400, "invalid_request")
	}
	for _, token := range []string{"xoxb-bot-token-1234", "xoxe.xoxp-1-refresh", "ghp_0123456789"} {
		expectStatus(t, api.do("PUT", path, api.alice, `{"token":"`+token+`"}`), 400, "slack_token_invalid")
	}
	if api.sl.count() != 0 {
		t.Fatalf("Slack called %d times for invalid input", api.sl.count())
	}

	// Answers from Slack. Nothing is saved on failure.
	all := strings.Join(slacksync.RequiredScopes, ",")
	for _, tc := range []struct {
		name   string
		status int
		code   string
		scopes string
		team   string
		want   int
		err    string
	}{
		{"invalid_auth", 200, "invalid_auth", all, "T1", 400, "slack_token_invalid"},
		{"token_revoked", 200, "token_revoked", all, "T1", 400, "slack_token_invalid"},
		{"other error", 200, "internal_error", all, "T1", 503, "slack_unavailable"},
		{"429", 429, "", all, "T1", 503, "slack_unavailable"},
		{"500", 500, "", all, "T1", 503, "slack_unavailable"},
		{"unexpected team ID", 200, "", all, "t1/../x", 503, "slack_unavailable"},
		{"scope missing", 200, "", strings.Replace(all, "mpim:history", "", 1), "T1", 400, "slack_scopes_missing"},
		{"no scopes", 200, "", "", "T1", 400, "slack_scopes_missing"},
	} {
		api.sl.set(func(f *fakeSlackAuth) { f.status, f.code, f.scopes, f.team = tc.status, tc.code, tc.scopes, tc.team })
		w := api.do("PUT", path, api.alice, `{"token":"`+slackToken+`"}`)
		expectStatus(t, w, tc.want, tc.err)
		if tc.err == "slack_scopes_missing" && !strings.Contains(w.Body.String(), "mpim:history") {
			t.Fatalf("%s: missing scopes not named: %s", tc.name, w.Body.String())
		}
		if _, err := api.store.SlackConnection(ctx, "alice-id"); err == nil {
			t.Fatalf("%s: saved", tc.name)
		}
	}

	api.sl.set(func(f *fakeSlackAuth) {
		f.status, f.code, f.scopes, f.team, f.url = 200, "", all+",chat:write", "T1", "https://acme.slack.com/"
	})
	w := api.do("PUT", path, api.alice, `{"token":"`+slackToken+`","detect_group_dms":true}`)
	if w.Code != 200 || strings.Contains(w.Body.String(), "xoxp-") {
		t.Fatalf("connect: %d %s", w.Code, w.Body.String())
	}
	_, keys := jsonKeys(t, w.Body.Bytes())
	if !reflect.DeepEqual(keys, []string{"connected", "created_at", "detect_dms", "detect_group_dms", "detect_mentions", "immediate_notify", "last_error", "last_synced_at", "scopes", "status", "team_name", "team_url", "token_last4", "user_name"}) {
		t.Fatalf("keys: %v", keys)
	}
	var got slackResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Connected || got.TeamName != "Acme" || got.TeamURL != "https://acme.slack.com/" || got.UserName != "alice" || got.TokenLast4 != "WXYZ" || len(got.Scopes) != 11 ||
		!got.ImmediateNotify || !got.DetectMentions || !got.DetectDMs || !got.DetectGroupDMs || got.Status != "active" || got.LastError != nil || got.CreatedAt != "2026-10-24T01:02:03Z" {
		t.Fatalf("connected: %+v", got)
	}
	api.sl.mu.Lock()
	last := api.sl.requests[len(api.sl.requests)-1]
	api.sl.mu.Unlock()
	if last.URL.Path != "/auth.test" || last.Header.Get("Authorization") != "Bearer "+slackToken || last.URL.RawQuery != "" {
		t.Fatalf("Slack request: %s %v", last.URL, last.Header)
	}
	c, err := api.store.SlackConnection(ctx, "alice-id")
	if err != nil || c.TeamID != "T1" || c.SlackUserID != "U1" || strings.Contains(c.TokenSealed, "xoxp-") {
		t.Fatalf("stored: %+v %v", c, err)
	}
	if plain, err := api.server.box.Open(slacksync.TokenPurpose("alice-id"), c.TokenSealed); err != nil || plain != slackToken {
		t.Fatalf("open: %q %v", plain, err)
	}

	// PATCH merges; PUT keeps the settings when they are left out.
	for _, body := range []string{`{}`, `{"detect_dms":null}`, `{"detect_dms":1}`, `{"detect_dms":false,"token":"x"}`} {
		expectStatus(t, api.do("PATCH", path, api.alice, body), 400, "invalid_request")
	}
	if w = api.do("PATCH", path, api.alice, `{"detect_dms":false,"immediate_notify":false}`); w.Code != 200 || !strings.Contains(w.Body.String(), `"detect_dms":false`) ||
		!strings.Contains(w.Body.String(), `"immediate_notify":false`) || !strings.Contains(w.Body.String(), `"detect_group_dms":true`) {
		t.Fatalf("PATCH: %d %s", w.Code, w.Body.String())
	}
	expectStatus(t, api.do("PATCH", path, api.bob, `{"detect_dms":true}`), 404, "not_found")
	// A non-Slack workspace URL is not kept.
	api.sl.set(func(f *fakeSlackAuth) { f.url = "https://evil.example/" })
	if w = api.do("PUT", path, api.alice, `{"token":"`+slackToken+`"}`); w.Code != 200 || !strings.Contains(w.Body.String(), `"detect_dms":false`) || !strings.Contains(w.Body.String(), `"immediate_notify":false`) || !strings.Contains(w.Body.String(), `"team_url":""`) {
		t.Fatalf("PUT keeps settings: %d %s", w.Code, w.Body.String())
	}

	// Another Slack user untracks the items of the previous one.
	c, err = api.store.SlackConnection(ctx, "alice-id")
	if err != nil {
		t.Fatal(err)
	}
	err = api.store.SlackSync(ctx, "alice-id", c.Generation, func(tx *store.SlackTx) error {
		_, e := tx.CreateItem(store.ActionItem{ID: "sl-item", Content: "x", Status: "open", Refs: "[]", Version: 1, CreatedAt: 1, UpdatedAt: 1, Source: sql.NullString{String: "slack", Valid: true}, SourceKey: sql.NullString{String: "slack:T1:m:C1:-:1.000001", Valid: true}, Tracking: 1})
		return e
	})
	if err != nil {
		t.Fatal(err)
	}
	api.sl.set(func(f *fakeSlackAuth) { f.user = "U2" })
	if w = api.do("PUT", path, api.alice, `{"token":"`+slackToken+`"}`); w.Code != 200 {
		t.Fatalf("PUT: %d %s", w.Code, w.Body.String())
	}
	if item, err := api.store.ActionItem(ctx, "alice-id", "sl-item"); err != nil || item.Tracking != 0 || item.SourceKey.Valid {
		t.Fatalf("item after user change: %+v %v", item, err)
	}

	for i := 0; i < 2; i++ {
		if w = api.do("DELETE", path, api.alice, ""); w.Code != 204 {
			t.Fatalf("DELETE: %d %s", w.Code, w.Body.String())
		}
	}
	if w = api.do("GET", path, api.alice, ""); strings.TrimSpace(w.Body.String()) != `{"connected":false}` {
		t.Fatalf("after DELETE: %s", w.Body.String())
	}

	// API tokens cannot manage the integration.
	tok := (&tokenAPI{t: t, handler: api.handler}).issue(api.alice, "x").Token
	for _, m := range []string{"GET", "PUT", "PATCH", "DELETE"} {
		expectStatus(t, api.do(m, path, tok, `{}`), 401, "unauthorized")
	}
}

// pushKeys returns a valid p256dh key and auth secret in unpadded base64url.
func pushKeys(t *testing.T) (string, string) {
	t.Helper()
	k, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	secret := make([]byte, 16)
	if _, err = rand.Read(secret); err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(k.PublicKey().Bytes()), base64.RawURLEncoding.EncodeToString(secret)
}

func subscriptionBody(endpoint, p256dh, authKey string, extra string) string {
	return fmt.Sprintf(`{"endpoint":%q,"keys":{"p256dh":%q,"auth":%q}%s}`, endpoint, p256dh, authKey, extra)
}

func TestPushAPI(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) { testPushAPI(t, driver) })
	}
}

func testPushAPI(t *testing.T, driver string) {
	api := newIntegrationAPI(t, driver)
	const path = "/api/v1/push/subscriptions"
	p256dh, authKey := pushKeys(t)

	w := api.do("GET", "/api/v1/push/public-key", api.alice, "")
	var key struct {
		PublicKey string `json:"public_key"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &key) != nil {
		t.Fatalf("public key: %d %s", w.Code, w.Body.String())
	}
	if raw, err := base64.RawURLEncoding.DecodeString(key.PublicKey); err != nil || len(raw) != 65 {
		t.Fatalf("public key: %q", key.PublicKey)
	}
	if w = api.do("GET", "/api/v1/push/public-key", api.bob, ""); !strings.Contains(w.Body.String(), key.PublicKey) {
		t.Fatal("public key differs between requests")
	}

	padded := func(v string) string { return v + strings.Repeat("=", (4-len(v)%4)%4) }
	short, _ := base64.RawURLEncoding.DecodeString(p256dh)
	compressed := base64.RawURLEncoding.EncodeToString(append([]byte{2}, short[1:]...))
	for name, body := range map[string]string{
		"http":             subscriptionBody("http://push.example/x", p256dh, authKey, ""),
		"IPv4":             subscriptionBody("https://192.0.2.1/x", p256dh, authKey, ""),
		"IPv6":             subscriptionBody("https://[2001:db8::1]/x", p256dh, authKey, ""),
		"numeric host":     subscriptionBody("https://0x7f000001/x", p256dh, authKey, ""),
		"port":             subscriptionBody("https://push.example:8443/x", p256dh, authKey, ""),
		"empty port":       subscriptionBody("https://push.example:/x", p256dh, authKey, ""),
		"userinfo":         subscriptionBody("https://user@push.example/x", p256dh, authKey, ""),
		"fragment":         subscriptionBody("https://push.example/x#y", p256dh, authKey, ""),
		"upper-case host":  subscriptionBody("https://Push.Example/x", p256dh, authKey, ""),
		"percent host":     subscriptionBody("https://push%2Eexample/x", p256dh, authKey, ""),
		"space":            subscriptionBody("https://push.example/x y", p256dh, authKey, ""),
		"non-ASCII":        subscriptionBody("https://push.example/é", p256dh, authKey, ""),
		"too long":         subscriptionBody("https://push.example/"+strings.Repeat("x", 2030), p256dh, authKey, ""),
		"missing endpoint": `{"keys":{"p256dh":"` + p256dh + `","auth":"` + authKey + `"}}`,
		"short p256dh":     subscriptionBody("https://push.example/x", p256dh[:20], authKey, ""),
		"compressed key":   subscriptionBody("https://push.example/x", compressed, authKey, ""),
		"long auth":        subscriptionBody("https://push.example/x", p256dh, authKey+"AAAA", ""),
		"standard base64":  subscriptionBody("https://push.example/x", strings.ReplaceAll(strings.ReplaceAll(p256dh, "-", "+"), "_", "/")+"+/", authKey, ""),
		"unknown key":      subscriptionBody("https://push.example/x", p256dh, authKey, `,"expirationTime":null`),
		"label type":       subscriptionBody("https://push.example/x", p256dh, authKey, `,"label":1`),
	} {
		if w = api.do("POST", path, api.alice, body); w.Code != 400 || errorCode(t, w) != "invalid_request" {
			t.Fatalf("%s: %d %s", name, w.Code, w.Body.String())
		}
	}

	// Padded keys, a cleaned label and a timezone that creates settings.
	label := "  iPhone​ Safari\n" + strings.Repeat("あ", 70)
	w = api.do("POST", path, api.alice, subscriptionBody("https://web.push.apple.com:443/abc", padded(p256dh), padded(authKey), `,"label":`+string(mustJSON(t, label))+`,"timezone":"Asia/Tokyo"`))
	if w.Code != 201 {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	_, keys := jsonKeys(t, w.Body.Bytes())
	if !reflect.DeepEqual(keys, []string{"created_at", "id", "label", "last_used_at"}) {
		t.Fatalf("keys: %v", keys)
	}
	var sub pushSubscriptionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &sub); err != nil {
		t.Fatal(err)
	}
	if want := string([]rune("iPhone Safari" + strings.Repeat("あ", 70))[:64]); sub.Label != want || sub.LastUsedAt != nil {
		t.Fatalf("label: %q", sub.Label)
	}
	if w = api.do("GET", "/api/v1/notification-settings", api.alice, ""); strings.TrimSpace(w.Body.String()) != `{"timezone":"Asia/Tokyo","digest_hour":8,"stale_days":3,"candidate_expire_days":3}` {
		t.Fatalf("settings from subscription: %s", w.Body.String())
	}
	stored, err := api.store.PushSubscriptions(context.Background(), "alice-id")
	if err != nil || len(stored) != 1 || stored[0].P256dh != p256dh || stored[0].Auth != authKey {
		t.Fatalf("stored keys: %+v %v", stored, err)
	}
	// An invalid timezone is ignored.
	for _, tz := range []string{"Mars/Base", "Local", ""} {
		w = api.do("POST", path, api.bob, subscriptionBody("https://push.example/bob-"+tz, p256dh, authKey, `,"timezone":`+string(mustJSON(t, tz))))
		if w.Code != 201 {
			t.Fatalf("timezone %q: %d %s", tz, w.Code, w.Body.String())
		}
	}
	if w = api.do("GET", "/api/v1/notification-settings", api.bob, ""); !strings.Contains(w.Body.String(), `"timezone":"UTC"`) {
		t.Fatalf("bob settings: %s", w.Body.String())
	}

	// Twenty devices at most; re-registering an own endpoint still works.
	for i := 1; i < store.MaxPushSubscriptions; i++ {
		if w = api.do("POST", path, api.alice, subscriptionBody(fmt.Sprintf("https://push.example/a%d", i), p256dh, authKey, "")); w.Code != 201 {
			t.Fatalf("subscription %d: %d %s", i, w.Code, w.Body.String())
		}
	}
	expectStatus(t, api.do("POST", path, api.alice, subscriptionBody("https://push.example/a21", p256dh, authKey, "")), 409, "too_many_subscriptions")
	if w = api.do("POST", path, api.alice, subscriptionBody("https://push.example/a3", p256dh, authKey, `,"label":"renamed"`)); w.Code != 200 || !strings.Contains(w.Body.String(), `"label":"renamed"`) {
		t.Fatalf("re-register: %d %s", w.Code, w.Body.String())
	}

	// Bob signs in on one of alice's browsers.
	if w = api.do("POST", path, api.bob, subscriptionBody("https://push.example/a5", p256dh, authKey, "")); w.Code != 201 {
		t.Fatalf("move: %d %s", w.Code, w.Body.String())
	}
	var list pushSubscriptionListResponse
	w = api.do("GET", path, api.alice, "")
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &list) != nil || len(list.Subscriptions) != store.MaxPushSubscriptions-1 || strings.Contains(w.Body.String(), "push.example") || strings.Contains(w.Body.String(), p256dh) {
		t.Fatalf("alice list: %d %s", w.Code, w.Body.String())
	}

	// Deleting needs the owner.
	expectStatus(t, api.do("DELETE", path+"/"+list.Subscriptions[0].ID, api.bob, ""), 404, "not_found")
	expectStatus(t, api.do("DELETE", path+"/not!valid", api.alice, ""), 404, "not_found")
	if w = api.do("DELETE", path+"/"+list.Subscriptions[0].ID, api.alice, ""); w.Code != 204 {
		t.Fatalf("delete: %d %s", w.Code, w.Body.String())
	}
	expectStatus(t, api.do("DELETE", path+"/"+list.Subscriptions[0].ID, api.alice, ""), 404, "not_found")
}

func TestPushTestAPI(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) { testPushTestAPI(t, driver) })
	}
}

func testPushTestAPI(t *testing.T, driver string) {
	api := newIntegrationAPI(t, driver)
	expectStatus(t, api.do("POST", "/api/v1/push/test", api.alice, ""), 409, "no_subscription")
	p256dh, authKey := pushKeys(t)
	if w := api.do("POST", "/api/v1/push/subscriptions", api.alice, subscriptionBody("https://push.example/x", p256dh, authKey, "")); w.Code != 201 {
		t.Fatalf("subscribe: %d %s", w.Code, w.Body.String())
	}
	for i := 0; i < 2; i++ {
		if w := api.do("POST", "/api/v1/push/test", api.alice, ""); w.Code != 202 {
			t.Fatalf("test %d: %d %s", i, w.Code, w.Body.String())
		}
	}
	w := api.do("POST", "/api/v1/push/test", api.alice, "")
	if w.Code != 429 || errorCode(t, w) != "rate_limited" || w.Header().Get("Retry-After") != "60" {
		t.Fatalf("rate limit: %d %s", w.Code, w.Body.String())
	}
	// The limit is per user.
	expectStatus(t, api.do("POST", "/api/v1/push/test", api.bob, ""), 409, "no_subscription")
	due, err := api.store.DueNotifications(context.Background(), api.now.Unix(), 10)
	if err != nil || len(due) != 2 {
		t.Fatalf("queued: %+v %v", due, err)
	}
	n := due[0]
	if n.Kind != store.NotificationTest || n.Title != "Astovia のテスト通知" || n.Body != "この端末で通知を受け取れます。" || n.URL != "/settings/notifications" || n.ActionItemID.Valid || !strings.HasPrefix(n.DedupeKey, "test:") {
		t.Fatalf("test notification: %+v", n)
	}
}

func TestNotificationSettingsAPI(t *testing.T) {
	api := newIntegrationAPI(t, "sqlite")
	const path = "/api/v1/notification-settings"
	if w := api.do("GET", path, api.alice, ""); w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"timezone":"UTC","digest_hour":8,"stale_days":3,"candidate_expire_days":3}` {
		t.Fatalf("defaults: %d %s", w.Code, w.Body.String())
	}
	for _, body := range []string{
		`{}`,
		`{"timezone":"Asia/Tokyo","digest_hour":8}`,
		`{"timezone":"Mars/Base","digest_hour":8,"stale_days":3}`,
		`{"timezone":"Local","digest_hour":8,"stale_days":3}`,
		`{"timezone":"","digest_hour":8,"stale_days":3}`,
		`{"timezone":null,"digest_hour":8,"stale_days":3}`,
		`{"timezone":"Asia/Tokyo","digest_hour":24,"stale_days":3}`,
		`{"timezone":"Asia/Tokyo","digest_hour":-1,"stale_days":3}`,
		`{"timezone":"Asia/Tokyo","digest_hour":8.5,"stale_days":3}`,
		`{"timezone":"Asia/Tokyo","digest_hour":"8","stale_days":3}`,
		`{"timezone":"Asia/Tokyo","digest_hour":null,"stale_days":3}`,
		`{"timezone":"Asia/Tokyo","digest_hour":8,"stale_days":0}`,
		`{"timezone":"Asia/Tokyo","digest_hour":8,"stale_days":31}`,
		`{"timezone":"Asia/Tokyo","digest_hour":8,"stale_days":3,"candidate_expire_days":3,"x":1}`,
		// candidate_expire_days is required and from 0 to 30.
		`{"timezone":"Asia/Tokyo","digest_hour":8,"stale_days":3}`,
		`{"timezone":"Asia/Tokyo","digest_hour":8,"stale_days":3,"candidate_expire_days":null}`,
		`{"timezone":"Asia/Tokyo","digest_hour":8,"stale_days":3,"candidate_expire_days":-1}`,
		`{"timezone":"Asia/Tokyo","digest_hour":8,"stale_days":3,"candidate_expire_days":31}`,
		`{"timezone":"Asia/Tokyo","digest_hour":8,"stale_days":3,"candidate_expire_days":1.5}`,
		`{"timezone":"Asia/Tokyo","digest_hour":8,"stale_days":3,"candidate_expire_days":"3"}`,
	} {
		expectStatus(t, api.do("PUT", path, api.alice, body), 400, "invalid_request")
	}
	want := `{"timezone":"America/New_York","digest_hour":0,"stale_days":30,"candidate_expire_days":0}`
	if w := api.do("PUT", path, api.alice, want); w.Code != 200 || strings.TrimSpace(w.Body.String()) != want {
		t.Fatalf("PUT: %d %s", w.Code, w.Body.String())
	}
	if w := api.do("GET", path, api.alice, ""); strings.TrimSpace(w.Body.String()) != want {
		t.Fatalf("GET: %s", w.Body.String())
	}
	if w := api.do("GET", path, api.bob, ""); !strings.Contains(w.Body.String(), `"UTC"`) {
		t.Fatalf("bob: %s", w.Body.String())
	}
	want = `{"timezone":"America/New_York","digest_hour":0,"stale_days":30,"candidate_expire_days":30}`
	if w := api.do("PUT", path, api.alice, want); w.Code != 200 || strings.TrimSpace(w.Body.String()) != want {
		t.Fatalf("PUT 30: %d %s", w.Code, w.Body.String())
	}
}

func TestAutoActionItems(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) { testAutoActionItems(t, driver) })
	}
}

func testAutoActionItems(t *testing.T, driver string) {
	api := newIntegrationAPI(t, driver)
	ctx := context.Background()
	c := api.connect(api.alice)
	conn, err := api.store.GitHubConnection(ctx, "alice-id")
	if err != nil || !c.Connected {
		t.Fatal(err)
	}
	type spec struct {
		id, status string
		autoAt     int64
		ref        string
	}
	specs := []spec{{"a1", "done", 300, "https://github.com/o/r/pull/1"}, {"a2", "done", 500, ""}, {"a3", "done", 500, "https://github.com/o/r/pull/3"}, {"a4", "dismissed", 100, ""}, {"open", "open", 0, ""}}
	err = api.store.GitHubSync(ctx, "alice-id", conn.Generation, func(tx *store.GitHubTx) error {
		for i, s := range specs {
			item := store.ActionItem{ID: s.id, Content: "item " + s.id, Status: s.status, Refs: "[]", Version: 1, CreatedAt: int64(10 + i), UpdatedAt: int64(10 + i), Source: sql.NullString{String: "github", Valid: true}, SourceKey: sql.NullString{String: "github:review:o/r#" + s.id, Valid: true}, Tracking: 1}
			if s.autoAt != 0 {
				item.AutoReason, item.AutoAt = sql.NullString{String: "github.pr_merged", Valid: true}, sql.NullInt64{Int64: s.autoAt, Valid: true}
				item.AutoRef = sql.NullString{String: s.ref, Valid: s.ref != ""}
			}
			if _, e := tx.CreateItem(item); e != nil {
				return e
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	list := func(q string) (actionItemListResponse, *httptest.ResponseRecorder) {
		t.Helper()
		w := api.do("GET", "/api/v1/action-items"+q, api.alice, "")
		var out actionItemListResponse
		if w.Code == 200 {
			if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
				t.Fatal(err)
			}
		}
		return out, w
	}
	ids := func(out actionItemListResponse) []string {
		var v []string
		for _, i := range out.Items {
			v = append(v, i.ID)
		}
		return v
	}
	out, w := list("?auto=true")
	if w.Code != 200 || !reflect.DeepEqual(ids(out), []string{"a3", "a2", "a1", "a4"}) {
		t.Fatalf("auto list: %d %v", w.Code, ids(out))
	}
	a3 := out.Items[0]
	if a3.Source == nil || *a3.Source != "github" || a3.Auto == nil || a3.Auto.Reason != "github.pr_merged" || a3.Auto.RefURL == nil || *a3.Auto.RefURL != "https://github.com/o/r/pull/3" || a3.Auto.At != formatTime(500) {
		t.Fatalf("a3: %+v", a3)
	}
	if out.Items[1].Auto == nil || out.Items[1].Auto.RefURL != nil {
		t.Fatalf("a2 ref_url: %+v", out.Items[1].Auto)
	}
	if !strings.Contains(w.Body.String(), `"ref_url":null`) {
		t.Fatalf("ref_url null missing: %s", w.Body.String())
	}
	// Paging with the auto cursor, combined with status.
	out, w = list("?auto=true&status=done&limit=2")
	if w.Code != 200 || !reflect.DeepEqual(ids(out), []string{"a3", "a2"}) || out.NextCursor == nil || !strings.HasPrefix(*out.NextCursor, "a.") {
		t.Fatalf("page 1: %d %v %v", w.Code, ids(out), out.NextCursor)
	}
	next := *out.NextCursor
	if out, w = list("?auto=true&status=done&limit=2&cursor=" + next); w.Code != 200 || !reflect.DeepEqual(ids(out), []string{"a1"}) || out.NextCursor != nil {
		t.Fatalf("page 2: %d %v", w.Code, ids(out))
	}
	// Cursors of the two lists cannot be mixed.
	plain, _ := list("?limit=1")
	for _, q := range []string{"?cursor=" + next, "?auto=true&cursor=" + *plain.NextCursor, "?auto=false", "?auto=1", "?auto=", "?auto=true&auto=true", "?auto=true&cursor=a."} {
		if _, w = list(q); w.Code != 400 || errorCode(t, w) != "invalid_request" {
			t.Fatalf("%s: %d %s", q, w.Code, w.Body.String())
		}
	}
	if out, _ = list("?status=open"); len(out.Items) != 1 || out.Items[0].Auto != nil {
		t.Fatalf("open: %+v", out.Items)
	}

	// Changing the status ends tracking and clears the automatic completion;
	// other changes keep them.
	w = api.do("PATCH", "/api/v1/action-items/open", api.alice, `{"note":"memo"}`)
	if w.Code != 200 {
		t.Fatalf("note: %d %s", w.Code, w.Body.String())
	}
	if item, _ := api.store.ActionItem(ctx, "alice-id", "open"); item.Tracking != 1 {
		t.Fatalf("note change untracked: %+v", item)
	}
	w = api.do("PATCH", "/api/v1/action-items/a3", api.alice, `{"status":"done"}`)
	if item, _ := api.store.ActionItem(ctx, "alice-id", "a3"); w.Code != 200 || item.Tracking != 1 || !item.AutoReason.Valid {
		t.Fatalf("same status: %+v", item)
	}
	w = api.do("PATCH", "/api/v1/action-items/a3", api.alice, `{"status":"open","version":2}`)
	var reopened actionItemResponse
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &reopened) != nil || reopened.Auto != nil || reopened.Status != "open" || reopened.Source == nil {
		t.Fatalf("reopen: %d %s", w.Code, w.Body.String())
	}
	item, err := api.store.ActionItem(ctx, "alice-id", "a3")
	if err != nil || item.Tracking != 0 || item.AutoReason.Valid || item.AutoRef.Valid || item.AutoAt.Valid || item.SourceKey.String != "github:review:o/r#a3" {
		t.Fatalf("after reopen: %+v %v", item, err)
	}

	// MCP updates follow the same rule.
	srv := httptest.NewServer(api.handler)
	t.Cleanup(srv.Close)
	mc := connectMCP(t, srv, (&tokenAPI{t: t, handler: api.handler}).issue(api.alice, "mcp").Token, nil)
	var updated actionItemResponse
	mc.ok("action_items_update", map[string]any{"id": "a1", "status": "open"}, &updated)
	if updated.Auto != nil || updated.Source == nil || *updated.Source != "github" {
		t.Fatalf("MCP update: %+v", updated)
	}
	if item, _ = api.store.ActionItem(ctx, "alice-id", "a1"); item.Tracking != 0 || item.AutoReason.Valid {
		t.Fatalf("MCP update kept tracking: %+v", item)
	}
	var got actionItemResponse
	mc.ok("action_items_get", map[string]any{"id": "a2"}, &got)
	if got.Auto == nil || got.Auto.Reason != "github.pr_merged" {
		t.Fatalf("MCP get: %+v", got)
	}
}

func TestServiceWorkerAndManifest(t *testing.T) {
	api := newIntegrationAPI(t, "sqlite")
	for path, want := range map[string][2]string{
		"/sw.js":                {"javascript", "no-cache"},
		"/admin/sw.js":          {"javascript", "no-cache"},
		"/manifest.webmanifest": {"application/manifest+json", "no-cache"},
		"/icon-192.png":         {"image/png", "public, max-age=31536000, immutable"},
	} {
		w := api.do("GET", path, "", "")
		if w.Code != 200 || !strings.Contains(w.Header().Get("Content-Type"), want[0]) || w.Header().Get("Cache-Control") != want[1] {
			t.Fatalf("%s: %d %v", path, w.Code, w.Header())
		}
	}
}
