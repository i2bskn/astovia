package httpserver

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/i2bskn/astovia/internal/auth"
	"github.com/i2bskn/astovia/internal/config"
)

const toolRedirect = "http://127.0.0.1:3000/callback"

// oauthTokens is the response of the token endpoint.
type oauthTokens struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
}

// mcpOAuthAPI is usersAPI (alice the owner, carol the member) with the
// MCP authorization server; the registrations are not limited unless the
// test sets it.
type mcpOAuthAPI struct {
	*usersAPI
}

func newMCPOAuthAPI(t *testing.T, driver string) *mcpOAuthAPI {
	t.Helper()
	a := &mcpOAuthAPI{newUsersAPI(t, driver)}
	if a.srv.mcp == nil {
		t.Fatal("no authorization server at an https address")
	}
	a.srv.registerLimiter = newKeyLimiter(1000000, time.Minute)
	return a
}

// form posts the form to a machine-facing endpoint, as a client does: no
// Origin, no cookie.
func (a *mcpOAuthAPI) form(path string, form url.Values) *httptest.ResponseRecorder {
	a.t.Helper()
	r := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return serve(a.t, a.handler, r)
}

// register registers a public client named name with the redirect URI.
func (a *mcpOAuthAPI) register(name string, body map[string]any) *httptest.ResponseRecorder {
	a.t.Helper()
	if body == nil {
		body = map[string]any{"client_name": name, "redirect_uris": []string{toolRedirect}}
	}
	r := httptest.NewRequest("POST", "/oauth/register", strings.NewReader(string(mustJSON(a.t, body))))
	r.Header.Set("Content-Type", "application/json")
	return serve(a.t, a.handler, r)
}

func (a *mcpOAuthAPI) client(name string) string {
	a.t.Helper()
	w := a.register(name, nil)
	if w.Code != 201 {
		a.t.Fatalf("register: %d %s", w.Code, w.Body.String())
	}
	var out struct {
		ClientID                string `json:"client_id"`
		TokenEndpointAuthMethod string `json:"token_endpoint_auth_method"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || out.ClientID == "" || out.TokenEndpointAuthMethod != "none" || strings.Contains(w.Body.String(), "client_secret") {
		a.t.Fatalf("register: %s", w.Body.String())
	}
	return out.ClientID
}

// authorizeQuery is an authorization request of clientID with a new PKCE
// verifier, returned with it.
func authorizeQuery(t *testing.T, clientID, scope string) (string, string) {
	t.Helper()
	verifier, err := auth.Random()
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(verifier))
	q := url.Values{"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {toolRedirect}, "code_challenge": {b64(sum[:])}, "code_challenge_method": {"S256"}, "state": {"st"}, "resource": {usersOrigin + "/mcp"}}
	if scope != "" {
		q.Set("scope", scope)
	}
	return q.Encode(), verifier
}

// consent reads the consent screen of query for session.
func (a *mcpOAuthAPI) consent(session, query string) *httptest.ResponseRecorder {
	a.t.Helper()
	return a.do("GET", "/api/v1/oauth/authorize?"+query, session, "")
}

// decide posts the decision of the consent screen with the client the
// screen of query shows to session (an empty name and no host when it shows
// none).
func (a *mcpOAuthAPI) decide(session, query, decision, userID string) *httptest.ResponseRecorder {
	a.t.Helper()
	var shown authorizeResponse
	if w := a.consent(session, query); w.Code == 200 {
		_ = json.Unmarshal(w.Body.Bytes(), &shown)
	}
	return a.decideFor(session, query, decision, userID, shown.ClientName, shown.ClientHost)
}

// decideFor posts the decision of the consent screen with the client name
// and host given as the ones the screen showed.
func (a *mcpOAuthAPI) decideFor(session, query, decision, userID, clientName string, clientHost *string) *httptest.ResponseRecorder {
	a.t.Helper()
	body := map[string]any{"query": query, "decision": decision, "user_id": userID, "client_name": clientName, "client_host": clientHost}
	return a.do("POST", "/api/v1/oauth/authorize", session, string(mustJSON(a.t, body)))
}

// labels counts the stored client labels.
func (a *mcpOAuthAPI) labels(driver string) int {
	a.t.Helper()
	n, err := countRows(driver, a.dsn, "SELECT COUNT(*) FROM mcp_oauth_client_labels")
	if err != nil {
		a.t.Fatal(err)
	}
	return n
}

// redirected returns the redirect URL of a decision.
func redirected(t *testing.T, w *httptest.ResponseRecorder) *url.URL {
	t.Helper()
	expect(t, w, 200, "")
	var out redirectResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(out.RedirectURL)
	if err != nil || u.Scheme+"://"+u.Host+u.Path != toolRedirect || u.Query().Get("state") != "st" || u.Query().Get("iss") != usersOrigin {
		t.Fatalf("redirect: %s", out.RedirectURL)
	}
	return u
}

// connect approves clientID for session and exchanges the code.
func (a *mcpOAuthAPI) connect(session, userID, clientID string) oauthTokens {
	a.t.Helper()
	query, verifier := authorizeQuery(a.t, clientID, "mcp")
	code := redirected(a.t, a.decide(session, query, "approve", userID)).Query().Get("code")
	return a.tokens(a.exchange(clientID, code, verifier))
}

func (a *mcpOAuthAPI) exchange(clientID, code, verifier string) *httptest.ResponseRecorder {
	a.t.Helper()
	return a.form("/oauth/token", url.Values{"grant_type": {"authorization_code"}, "client_id": {clientID}, "code": {code}, "redirect_uri": {toolRedirect}, "code_verifier": {verifier}, "resource": {usersOrigin + "/mcp"}})
}

func (a *mcpOAuthAPI) refresh(clientID, token string) *httptest.ResponseRecorder {
	a.t.Helper()
	return a.form("/oauth/token", url.Values{"grant_type": {"refresh_token"}, "client_id": {clientID}, "refresh_token": {token}})
}

func (a *mcpOAuthAPI) tokens(w *httptest.ResponseRecorder) oauthTokens {
	a.t.Helper()
	var out oauthTokens
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil || out.AccessToken == "" || out.RefreshToken == "" || out.TokenType != "Bearer" || out.ExpiresIn <= 0 || w.Header().Get("Cache-Control") != "no-store" {
		a.t.Fatalf("token response: %d %v %s", w.Code, w.Header(), w.Body.String())
	}
	return out
}

// connections lists the connections of session.
func (a *mcpOAuthAPI) connections(session string) []connectionResponse {
	a.t.Helper()
	w := a.do("GET", "/api/v1/oauth/connections", session, "")
	expect(a.t, w, 200, "")
	return decodeJSON[connectionListResponse](a.t, w).Items
}

func expectMCP(t *testing.T, h http.Handler, token string, status int) {
	t.Helper()
	w := mcpPost(t, h, token, "", "")
	if w.Code != status {
		t.Fatalf("/mcp: %d %v %s", w.Code, w.Header(), w.Body.String())
	}
	if status == 401 && w.Header().Get("WWW-Authenticate") != mcpInvalidToken {
		t.Fatalf("/mcp challenge: %v", w.Header())
	}
}

func oauthError(t *testing.T, w *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	var body struct {
		Error string `json:"error"`
	}
	if w.Code != status || json.Unmarshal(w.Body.Bytes(), &body) != nil || body.Error != code {
		t.Fatalf("got %d %s, want %d %s", w.Code, w.Body.String(), status, code)
	}
}

func TestMCPOAuthMetadata(t *testing.T) {
	h := New(config.Config{PublicURL: usersOrigin}, nil, fstest.MapFS{}, nil)
	var as map[string]any
	w := serve(t, h, httptest.NewRequest("GET", "/.well-known/oauth-authorization-server", nil))
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &as) != nil {
		t.Fatalf("authorization server: %d %s", w.Code, w.Body.String())
	}
	for k, v := range map[string]string{"issuer": usersOrigin, "authorization_endpoint": usersOrigin + "/oauth/authorize", "token_endpoint": usersOrigin + "/oauth/token",
		"registration_endpoint": usersOrigin + "/oauth/register", "revocation_endpoint": usersOrigin + "/oauth/revoke"} {
		if as[k] != v {
			t.Fatalf("%s: %v", k, as[k])
		}
	}
	if !reflect.DeepEqual(as["scopes_supported"], []any{"mcp", "offline_access"}) || as["client_id_metadata_document_supported"] != true {
		t.Fatalf("authorization server: %s", w.Body.String())
	}
	var resource string
	for _, path := range []string{"/.well-known/oauth-protected-resource/mcp", "/.well-known/oauth-protected-resource"} {
		w = serve(t, h, httptest.NewRequest("GET", path, nil))
		var prm map[string]any
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &prm) != nil || prm["resource"] != usersOrigin+"/mcp" || !reflect.DeepEqual(prm["authorization_servers"], []any{usersOrigin}) {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
		if resource != "" && w.Body.String() != resource {
			t.Fatalf("%s differs: %s", path, w.Body.String())
		}
		resource = w.Body.String()
	}
	r := httptest.NewRequest("POST", "/.well-known/oauth-authorization-server", nil)
	r.Header.Set("Origin", usersOrigin)
	expect(t, serve(t, h, r), 405, "method_not_allowed")
	expect(t, serve(t, h, httptest.NewRequest("GET", "/.well-known/openid-configuration", nil)), 404, "not_found")

	// At an http address there is no authorization server: /mcp takes API
	// tokens alone.
	h = New(config.Config{PublicURL: "http://localhost:8080"}, nil, fstest.MapFS{}, nil)
	for _, path := range []string{"/.well-known/oauth-authorization-server", "/.well-known/oauth-protected-resource/mcp", "/.well-known/oauth-protected-resource"} {
		expect(t, serve(t, h, httptest.NewRequest("GET", path, nil)), 404, "not_found")
	}
	for _, path := range []string{"/oauth/register", "/oauth/token", "/oauth/revoke"} {
		expect(t, serve(t, h, httptest.NewRequest("POST", path, nil)), 404, "not_found")
	}
	w = serve(t, h, httptest.NewRequest("GET", "/oauth/authorize", nil))
	expect(t, w, 404, "not_found")
	w = mcpPost(t, h, "", "", "")
	if w.Code != 401 || w.Header().Get("WWW-Authenticate") != "Bearer" {
		t.Fatalf("/mcp without the authorization server: %d %v", w.Code, w.Header())
	}
}

// TestMCPOAuthRegister checks dynamic client registration: every client is
// public, and the registrations of a peer are limited.
func TestMCPOAuthRegister(t *testing.T) {
	a := newMCPOAuthAPI(t, "sqlite")
	a.client("Tool")
	// A confidential client asked for is registered as a public one.
	w := a.register("", map[string]any{"client_name": "Tool", "redirect_uris": []string{toolRedirect}, "token_endpoint_auth_method": "client_secret_basic"})
	if w.Code != 201 || !strings.Contains(w.Body.String(), `"token_endpoint_auth_method":"none"`) || strings.Contains(w.Body.String(), "client_secret") {
		t.Fatalf("confidential: %d %s", w.Code, w.Body.String())
	}
	oauthError(t, a.register("", map[string]any{"client_name": "Tool"}), 400, "invalid_redirect_uri")

	a.srv.registerLimiter = newLoginLimiter()
	for i := 0; i < 10; i++ {
		if w := a.register("", map[string]any{"redirect_uris": "malformed"}); w.Code == 429 {
			t.Fatalf("registration %d limited", i)
		}
	}
	w = a.register("Tool", nil)
	oauthError(t, w, 429, "invalid_request")
	if w.Header().Get("Retry-After") != "60" {
		t.Fatalf("Retry-After: %v", w.Header())
	}
}

// TestMCPOAuthFlow connects a client: the consent screen, the code, the
// tokens, /mcp, the connection list and the revocation.
func TestMCPOAuthFlow(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			a := newMCPOAuthAPI(t, driver)
			alice, _ := a.login("alice@example.com", "correct-password-123")
			carol, _ := a.login("carol@example.com", "correct-password-123")
			clientID := a.client("  Tool​ ")
			query, verifier := authorizeQuery(t, clientID, "mcp offline_access")

			// The consent screen.
			expect(t, a.consent("", query), 401, "unauthorized")
			w := a.consent(alice, query)
			expect(t, w, 200, "")
			if _, keys := jsonKeys(t, w.Body.Bytes()); !reflect.DeepEqual(keys, []string{"client_host", "client_id", "client_name", "redirect_host", "redirect_loopback", "scopes", "status", "user_id"}) {
				t.Fatalf("consent keys: %v", keys)
			}
			screen := decodeJSON[authorizeResponse](t, w)
			if !reflect.DeepEqual(screen, authorizeResponse{Status: "ok", ClientName: "Tool", ClientID: clientID, RedirectHost: "127.0.0.1", RedirectLoopback: true, Scopes: []string{"mcp", "offline_access"}, UserID: "alice-id"}) {
				t.Fatalf("consent: %+v", screen)
			}
			// An unknown client and a malformed request are not sent back.
			unknown, _ := authorizeQuery(t, "unregistered", "mcp")
			expect(t, a.consent(alice, unknown), 400, "unknown_client")
			expect(t, a.consent(alice, "client_id="+url.QueryEscape(clientID)+"&client_id=x"), 400, "invalid_request")
			// An error the client may know of is shown with its redirect.
			bad := strings.Replace(query, "code_challenge_method=S256", "code_challenge_method=plain", 1)
			w = a.consent(alice, bad)
			if got := decodeJSON[authorizeErrorResponse](t, w); got.Status != "error" || got.Error != "invalid_request" || got.RedirectHost != "127.0.0.1" || !strings.Contains(got.RedirectURL, "error=invalid_request") {
				t.Fatalf("rejected: %+v", got)
			}

			// The decision: the same user as the screen, from this origin.
			expect(t, a.decide(alice, query, "approve", "carol-id"), 409, "user_changed")
			expect(t, a.decide(carol, query, "approve", "alice-id"), 409, "user_changed")
			// A user switch is reported as such, even for a request that is
			// no longer valid.
			expect(t, a.decideFor(carol, unknown, "approve", "alice-id", "Tool", nil), 409, "user_changed")
			expect(t, a.decide(alice, query, "maybe", "alice-id"), 400, "invalid_request")
			expect(t, a.decide("", query, "approve", "alice-id"), 401, "unauthorized")
			// The client the screen showed must still be the one of the
			// request: its cleaned name and its verified host (none for a
			// registered client).
			host := "tool.example"
			expect(t, a.decideFor(alice, query, "approve", "alice-id", "Other tool", nil), 409, "client_changed")
			expect(t, a.decideFor(alice, query, "approve", "alice-id", "  Tool\u200b ", nil), 409, "client_changed")
			expect(t, a.decideFor(alice, query, "approve", "alice-id", "Tool", &host), 409, "client_changed")
			expect(t, a.decideFor(alice, query, "deny", "alice-id", "Other tool", nil), 409, "client_changed")
			expect(t, a.do("POST", "/api/v1/oauth/authorize", alice, string(mustJSON(t, map[string]string{"query": query, "decision": "approve", "user_id": "alice-id"}))), 400, "invalid_request")
			expect(t, a.do("POST", "/api/v1/oauth/authorize", alice, string(mustJSON(t, map[string]any{"query": query, "decision": "approve", "user_id": "alice-id", "client_name": "Tool", "client_host": 1}))), 400, "invalid_request")
			r := httptest.NewRequest("POST", "/api/v1/oauth/authorize", strings.NewReader(string(mustJSON(t, map[string]any{"query": query, "decision": "approve", "user_id": "alice-id", "client_name": "Tool", "client_host": nil}))))
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Origin", "https://evil.test")
			authorize(r, alice)
			expect(t, serve(t, a.handler, r), 403, "origin_rejected")
			// The API tokens are not a session that approves.
			w = a.do("POST", "/api/v1/api-tokens", alice, `{"name":"cli"}`)
			expect(t, w, 201, "")
			apiToken := decodeJSON[createdAPITokenResponse](t, w).Token
			expect(t, a.decide(apiToken, query, "approve", "alice-id"), 401, "unauthorized")
			if got := a.connections(alice); len(got) != 0 {
				t.Fatalf("connections before the approval: %+v", got)
			}
			// Deny.
			if got := redirected(t, a.decide(alice, query, "deny", "alice-id")); got.Query().Get("error") != "access_denied" || got.Query().Get("code") != "" {
				t.Fatalf("deny: %s", got)
			}
			// A request without mcp grants nothing.
			noMCP, _ := authorizeQuery(t, clientID, "offline_access")
			if got := decodeJSON[authorizeErrorResponse](t, a.consent(alice, noMCP)); got.Error != "invalid_scope" {
				t.Fatalf("no mcp: %+v", got)
			}
			if got := redirected(t, a.decideFor(alice, noMCP, "approve", "alice-id", "Tool", nil)); got.Query().Get("error") != "invalid_scope" || got.Query().Get("code") != "" {
				t.Fatalf("no mcp: %s", got)
			}
			// No decision so far issued a code, so no label is stored: the
			// labels are shared by every user of the client.
			if n := a.labels(driver); n != 0 {
				t.Fatalf("labels before the approval: %d", n)
			}

			// Approve, exchange.
			code := redirected(t, a.decide(alice, query, "approve", "alice-id")).Query().Get("code")
			if n := a.labels(driver); n != 1 {
				t.Fatalf("labels after the approval: %d", n)
			}
			oauthError(t, a.exchange(clientID, code, "wrong-verifier-"+strings.Repeat("v", 40)), 400, "invalid_grant")
			code = redirected(t, a.decide(alice, query, "approve", "alice-id")).Query().Get("code")
			tokens := a.tokens(a.exchange(clientID, code, verifier))
			if tokens.Scope != "mcp offline_access" {
				t.Fatalf("scope: %q", tokens.Scope)
			}
			expectMCP(t, a.handler, tokens.AccessToken, 200)
			// A code used twice revokes the connection it made.
			oauthError(t, a.exchange(clientID, code, verifier), 400, "invalid_grant")
			expectMCP(t, a.handler, tokens.AccessToken, 401)
			code = redirected(t, a.decide(alice, query, "approve", "alice-id")).Query().Get("code")
			tokens = a.tokens(a.exchange(clientID, code, verifier))
			expectMCP(t, a.handler, tokens.AccessToken, 200)
			// The tokens are not credentials of the API.
			expect(t, a.do("GET", "/api/v1/me", tokens.AccessToken, ""), 401, "unauthorized")

			// The connection list shows the client by its cleaned name.
			conns := a.connections(alice)
			if len(conns) != 1 || conns[0].ClientName != "Tool" || conns[0].ClientHost != nil || !reflect.DeepEqual(conns[0].Scopes, []string{"mcp", "offline_access"}) || conns[0].LastUsedAt == nil || conns[0].CreatedAt != formatTime(a.now.Unix()) {
				t.Fatalf("connections: %+v", conns)
			}
			if got := a.connections(carol); len(got) != 0 {
				t.Fatalf("carol's connections: %+v", got)
			}

			// Refresh: the new tokens work, the old refresh token does not.
			*a.now = a.now.Add(time.Minute)
			next := a.tokens(a.refresh(clientID, tokens.RefreshToken))
			expectMCP(t, a.handler, next.AccessToken, 200)
			oauthError(t, a.refresh(clientID, "not-a-token"), 400, "invalid_grant")

			// Revocation (RFC 7009) ends the connection.
			w = a.form("/oauth/revoke", url.Values{"token": {next.RefreshToken}, "client_id": {clientID}})
			if w.Code != 200 {
				t.Fatalf("revoke: %d %s", w.Code, w.Body.String())
			}
			expectMCP(t, a.handler, next.AccessToken, 401)
			if got := a.connections(alice); len(got) != 0 {
				t.Fatalf("connections after the revocation: %+v", got)
			}
			// An unknown token is answered as revoked.
			if w = a.form("/oauth/revoke", url.Values{"token": {"unknown"}, "client_id": {clientID}}); w.Code != 200 {
				t.Fatalf("revoke unknown: %d %s", w.Code, w.Body.String())
			}

			// The account screen deletes a connection; only its owner can.
			tokens = a.connect(alice, "alice-id", clientID)
			id := a.connections(alice)[0].ID
			expect(t, a.do("DELETE", "/api/v1/oauth/connections/"+id, carol, ""), 404, "not_found")
			expectMCP(t, a.handler, tokens.AccessToken, 200)
			expect(t, a.do("DELETE", "/api/v1/oauth/connections/"+id, alice, ""), 204, "")
			expect(t, a.do("DELETE", "/api/v1/oauth/connections/"+id, alice, ""), 404, "not_found")
			expectMCP(t, a.handler, tokens.AccessToken, 401)
			oauthError(t, a.refresh(clientID, tokens.RefreshToken), 400, "invalid_grant")
		})
	}
}

// TestMCPOAuthRevocation checks that what ends a user's access ends the
// MCP connections: signing out does not; disabling, a password reset and
// reset-mfa do. Unlinking is tested with OIDC and in the store.
func TestMCPOAuthRevocation(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			ctx := context.Background()
			a := newMCPOAuthAPI(t, driver)
			clientID := a.client("Tool")
			for _, tc := range []struct {
				name    string
				revoke  func(session string) error
				revokes bool
			}{
				{"logout", func(session string) error {
					expect(t, a.public("/auth/logout", "{}", session), 204, "")
					return nil
				}, false},
				{"disable", func(string) error {
					if err := a.store.ChangeUser(ctx, "carol@example.com", "", true, a.now.Unix()); err != nil {
						return err
					}
					return a.store.EnableUser(ctx, "carol@example.com")
				}, true},
				{"password reset", func(string) error {
					hash, err := auth.Password("correct-password-123")
					if err != nil {
						return err
					}
					return a.store.ChangeUser(ctx, "carol@example.com", hash, false, a.now.Unix())
				}, true},
				{"reset-mfa", func(string) error { return a.store.ResetMFA(ctx, "carol@example.com") }, true},
			} {
				carol, _ := a.login("carol@example.com", "correct-password-123")
				tokens := a.connect(carol, "carol-id", clientID)
				expectMCP(t, a.handler, tokens.AccessToken, 200)
				if err := tc.revoke(carol); err != nil {
					t.Fatalf("%s: %v", tc.name, err)
				}
				if !tc.revokes {
					expectMCP(t, a.handler, tokens.AccessToken, 200)
					continue
				}
				expectMCP(t, a.handler, tokens.AccessToken, 401)
				oauthError(t, a.refresh(clientID, tokens.RefreshToken), 400, "invalid_grant")
				expect(t, a.do("GET", "/api/v1/me", carol, ""), 401, "unauthorized")
			}
		})
	}
}

// TestMCPOAuthMetadataDocumentClients checks that a client_id URL that no
// code or connection names is refused at the token and revocation endpoints
// without a fetch.
func TestMCPOAuthMetadataDocumentClients(t *testing.T) {
	a := newMCPOAuthAPI(t, "sqlite")
	// A host that cannot be resolved: a fetch would fail as unavailable.
	clientID := "https://client.invalid/metadata.json"
	oauthError(t, a.form("/oauth/token", url.Values{"grant_type": {"refresh_token"}, "client_id": {clientID}, "refresh_token": {"x"}}), 401, "invalid_client")
	r := httptest.NewRequest("POST", "/oauth/token", strings.NewReader(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {"x"}}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.SetBasicAuth(url.QueryEscape(clientID), "")
	oauthError(t, serve(t, a.handler, r), 401, "invalid_client")
	if w := a.form("/oauth/revoke", url.Values{"token": {"x"}, "client_id": {clientID}}); w.Code != 200 {
		t.Fatalf("revoke: %d %s", w.Code, w.Body.String())
	}
}

// TestMCPOAuthMetadataErrors makes authlume fail to fetch a client
// metadata document in the two ways that Astovia classifies by what
// mcpoauth exports least of: a blocked address (MetadataError.Reason) and
// a temporary failure (the message of an unexported error). The handlers
// of both, metadataClass and metadataUnavailable, depend on this test.
func TestMCPOAuthMetadataErrors(t *testing.T) {
	// Names are resolved by a fake DNS server: blocked.example is a
	// loopback address and missing.example does not exist.
	fakeDNS(t, map[string]netip.Addr{"blocked.example.": netip.MustParseAddr("127.0.0.1")})
	a := newMCPOAuthAPI(t, "sqlite")
	session, _ := a.login("alice@example.com", "correct-password-123")
	for _, tc := range []struct{ clientID, log, host string }{
		// A loopback address is never fetched.
		{"https://blocked.example/oauth/client.json", "oauth: client metadata: blocked ", "host=blocked.example"},
		// A name that does not resolve fails a fetch that may succeed
		// later.
		{"https://missing.example/oauth/client.json", "oauth: client metadata: unavailable ", "host=missing.example"},
	} {
		logs := captureLog(t)
		query, _ := authorizeQuery(t, tc.clientID, "mcp")
		w := a.consent(session, query)
		expect(t, w, 503, "client_metadata_unavailable")
		if !strings.Contains(logs.String(), tc.log) || !strings.Contains(logs.String(), tc.host) {
			t.Fatalf("%s: log %q", tc.clientID, logs.String())
		}
	}
}

// fakeDNS makes the resolver of the net package answer from records, over
// an in-memory connection, until the end of the test: A queries of a name
// in records get its address, other names do not exist, and other types
// have no answer.
func fakeDNS(t *testing.T, records map[string]netip.Addr) {
	t.Helper()
	previous := net.DefaultResolver
	net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: func(context.Context, string, string) (net.Conn, error) {
		client, server := net.Pipe()
		go serveFakeDNS(server, records)
		return client, nil
	}}
	t.Cleanup(func() { net.DefaultResolver = previous })
}

// serveFakeDNS answers the queries of one connection, framed as over TCP.
func serveFakeDNS(conn net.Conn, records map[string]netip.Addr) {
	defer conn.Close()
	for {
		var size [2]byte
		if _, err := io.ReadFull(conn, size[:]); err != nil {
			return
		}
		query := make([]byte, binary.BigEndian.Uint16(size[:]))
		if _, err := io.ReadFull(conn, query); err != nil {
			return
		}
		var msg dnsmessage.Message
		if err := msg.Unpack(query); err != nil || len(msg.Questions) != 1 {
			return
		}
		q := msg.Questions[0]
		reply := dnsmessage.Message{Header: dnsmessage.Header{ID: msg.ID, Response: true, Authoritative: true, RecursionAvailable: true}, Questions: msg.Questions}
		addr, ok := records[q.Name.String()]
		switch {
		case !ok:
			reply.RCode = dnsmessage.RCodeNameError
		case q.Type == dnsmessage.TypeA:
			reply.Answers = []dnsmessage.Resource{{Header: dnsmessage.ResourceHeader{Name: q.Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 60}, Body: &dnsmessage.AResource{A: addr.As4()}}}
		}
		out, err := reply.Pack()
		if err != nil {
			return
		}
		binary.BigEndian.PutUint16(size[:], uint16(len(out)))
		if _, err := conn.Write(append(size[:], out...)); err != nil {
			return
		}
	}
}
