package httpserver

import (
	"bytes"
	"context"
	"crypto/hmac"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"github.com/i2bskn/astovia/internal/auth"
	"github.com/i2bskn/astovia/internal/oauth"
	"github.com/i2bskn/astovia/internal/store"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const protectedResourcePath = "/.well-known/oauth-protected-resource/mcp"

// oauthBodyLimit bounds the bodies of /oauth/register and /oauth/token.
const oauthBodyLimit = 64 << 10

// consentBodyLimit bounds the body of POST /api/v1/oauth/consent.
const consentBodyLimit = 16 << 10

// authorizeParams are the /oauth/authorize parameters that must not repeat
// (RFC 6749 section 3.1).
var authorizeParams = []string{"response_type", "client_id", "redirect_uri", "code_challenge", "code_challenge_method", "scope", "state", "resource"}

var errClientRejected = errors.New("client rejected")
var errCIMDRateLimited = errors.New("metadata document fetch rate limited")

type protectedResourceMetadata struct {
	Resource               string   `json:"resource"`
	AuthorizationServers   []string `json:"authorization_servers"`
	ScopesSupported        []string `json:"scopes_supported"`
	BearerMethodsSupported []string `json:"bearer_methods_supported"`
	ResourceName           string   `json:"resource_name"`
}

type authorizationServerMetadata struct {
	Issuer                                     string   `json:"issuer"`
	AuthorizationEndpoint                      string   `json:"authorization_endpoint"`
	TokenEndpoint                              string   `json:"token_endpoint"`
	RegistrationEndpoint                       string   `json:"registration_endpoint"`
	ScopesSupported                            []string `json:"scopes_supported"`
	ResponseTypesSupported                     []string `json:"response_types_supported"`
	ResponseModesSupported                     []string `json:"response_modes_supported"`
	GrantTypesSupported                        []string `json:"grant_types_supported"`
	CodeChallengeMethodsSupported              []string `json:"code_challenge_methods_supported"`
	TokenEndpointAuthMethodsSupported          []string `json:"token_endpoint_auth_methods_supported"`
	ClientIDMetadataDocumentSupported          bool     `json:"client_id_metadata_document_supported"`
	AuthorizationResponseIssParameterSupported bool     `json:"authorization_response_iss_parameter_supported"`
}

type oauthErrorResponse struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

type registrationResponse struct {
	ClientID                string   `json:"client_id"`
	ClientSecret            string   `json:"client_secret,omitempty"`
	ClientIDIssuedAt        int64    `json:"client_id_issued_at"`
	ClientSecretExpiresAt   *int64   `json:"client_secret_expires_at,omitempty"`
	ClientName              string   `json:"client_name"`
	RedirectURIs            []string `json:"redirect_uris"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
	GrantTypes              []string `json:"grant_types"`
	ResponseTypes           []string `json:"response_types"`
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	RefreshToken string `json:"refresh_token,omitempty"`
	Scope        string `json:"scope"`
}

type consentResponse struct {
	ClientName       string   `json:"client_name"`
	ClientKind       string   `json:"client_kind"`
	ClientHost       *string  `json:"client_host"`
	RedirectHost     string   `json:"redirect_host"`
	RedirectLoopback bool     `json:"redirect_loopback"`
	Scopes           []string `json:"scopes"`
	ExpiresAt        string   `json:"expires_at"`
}

type consentResultResponse struct {
	RedirectURL string `json:"redirect_url"`
}

type connectionResponse struct {
	ID         string   `json:"id"`
	ClientName string   `json:"client_name"`
	ClientHost *string  `json:"client_host"`
	Scopes     []string `json:"scopes"`
	CreatedAt  string   `json:"created_at"`
	LastUsedAt *string  `json:"last_used_at"`
	ExpiresAt  string   `json:"expires_at"`
}

type connectionListResponse struct {
	Items []connectionResponse `json:"items"`
}

// oauthFail writes an RFC 6749 error response.
func oauthFail(w http.ResponseWriter, status int, code, description string) {
	writeJSON(w, status, oauthErrorResponse{Error: code, ErrorDescription: description})
}

// oauthUnavailable answers an unexpected error on /oauth/* without
// redirecting. The log never contains codes, tokens or secrets.
func oauthUnavailable(w http.ResponseWriter, r *http.Request, err error) {
	log.Printf("oauth: %s %s: %v", r.Method, r.URL.Path, err)
	oauthFail(w, 503, "temporarily_unavailable", "Try again later.")
}

// consentUnavailable is apiTokenError for the /api/v1/oauth/* handlers.
func consentUnavailable(w http.ResponseWriter, op, userID string, err error) {
	log.Printf("oauth consent: %s user=%s: %v", op, userID, err)
	fail(w, 503, "unavailable", "The service is temporarily unavailable. Try again.")
}

func sqlInt(v int64) sql.NullInt64 { return sql.NullInt64{Int64: v, Valid: true} }

func hasMediaType(r *http.Request, want string) bool {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return err == nil && mt == want
}

// clientHost returns the host of a metadata document client_id, or nil for
// other clients, whose names are self-asserted.
func clientHost(clientID string) *string {
	if oauth.ClientKind(clientID) != "cimd" {
		return nil
	}
	h := oauth.RedirectHost(clientID)
	return &h
}

func (s *Server) handleProtectedResourceMetadata(w http.ResponseWriter, _ *http.Request) {
	// The root URL returns the /mcp resource as well: it is the only
	// protected resource, and clients that fall back to the root should find
	// the same one.
	writeJSON(w, 200, protectedResourceMetadata{
		Resource:               auth.MCPResource(s.cfg.PublicURL),
		AuthorizationServers:   []string{s.cfg.PublicURL},
		ScopesSupported:        []string{oauth.ScopeMCP},
		BearerMethodsSupported: []string{"header"},
		ResourceName:           "Astovia",
	})
}

func (s *Server) handleAuthorizationServerMetadata(w http.ResponseWriter, _ *http.Request) {
	issuer := s.cfg.PublicURL
	writeJSON(w, 200, authorizationServerMetadata{
		Issuer:                                     issuer,
		AuthorizationEndpoint:                      issuer + "/oauth/authorize",
		TokenEndpoint:                              issuer + "/oauth/token",
		RegistrationEndpoint:                       issuer + "/oauth/register",
		ScopesSupported:                            []string{oauth.ScopeMCP, oauth.ScopeOfflineAccess},
		ResponseTypesSupported:                     []string{"code"},
		ResponseModesSupported:                     []string{"query"},
		GrantTypesSupported:                        []string{"authorization_code", "refresh_token"},
		CodeChallengeMethodsSupported:              []string{"S256"},
		TokenEndpointAuthMethodsSupported:          []string{"none", "client_secret_basic", "client_secret_post"},
		ClientIDMetadataDocumentSupported:          true,
		AuthorizationResponseIssParameterSupported: true,
	})
}

// handleRegister implements Dynamic Client Registration (RFC 7591).
func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	// Counted before any validation, so malformed requests count as well.
	if !s.registerLimiter.allow(r.RemoteAddr) {
		w.Header().Set("Retry-After", "60")
		oauthFail(w, 429, "invalid_request", "Too many registrations. Try again later.")
		return
	}
	if !hasMediaType(r, "application/json") {
		oauthFail(w, 415, "invalid_client_metadata", "Use application/json.")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, oauthBodyLimit))
	if err != nil {
		var large *http.MaxBytesError
		if errors.As(err, &large) {
			oauthFail(w, 413, "invalid_client_metadata", "Request body is too large.")
		} else {
			oauthFail(w, 400, "invalid_client_metadata", "Invalid request body.")
		}
		return
	}
	// Unknown fields such as logo_uri are ignored, so decode is not used.
	var fields map[string]json.RawMessage
	if body = bytes.TrimSpace(body); len(body) == 0 || body[0] != '{' || json.Unmarshal(body, &fields) != nil {
		oauthFail(w, 400, "invalid_client_metadata", "A JSON object is required.")
		return
	}
	var uris []string
	if raw, ok := fields["redirect_uris"]; !ok || json.Unmarshal(raw, &uris) != nil || oauth.ValidateRedirectURIs(uris) != nil {
		oauthFail(w, 400, "invalid_redirect_uri", "redirect_uris must be 1-8 https or loopback http URLs.")
		return
	}
	var name string
	if raw, ok := fields["client_name"]; ok && json.Unmarshal(raw, &name) != nil {
		oauthFail(w, 400, "invalid_client_metadata", "client_name must be a string.")
		return
	}
	method := "client_secret_basic"
	if raw, ok := fields["token_endpoint_auth_method"]; ok {
		var v string
		if json.Unmarshal(raw, &v) != nil {
			v = "invalid"
		}
		if v != "" {
			method = v
		}
	}
	switch method {
	case "none", "client_secret_basic", "client_secret_post":
	default:
		oauthFail(w, 400, "invalid_client_metadata", "token_endpoint_auth_method must be none, client_secret_basic or client_secret_post.")
		return
	}
	clientID, err := auth.NewDCRClientID()
	if err != nil {
		oauthUnavailable(w, r, err)
		return
	}
	var secret, secretHash string
	if method != "none" {
		if secret, secretHash, err = auth.NewDCRClientSecret(); err != nil {
			oauthUnavailable(w, r, err)
			return
		}
	}
	now := s.auth.Now().Unix()
	c := store.OAuthClient{ClientID: clientID, Kind: "dcr", Name: oauth.ClientName(name, oauth.RedirectHost(uris[0])), RedirectURIs: uris, AuthMethod: method, CreatedAt: now, UpdatedAt: now}
	if err = s.auth.Store.CreateOAuthClient(r.Context(), c, secretHash); err != nil {
		oauthUnavailable(w, r, err)
		return
	}
	out := registrationResponse{ClientID: clientID, ClientSecret: secret, ClientIDIssuedAt: now, ClientName: c.Name, RedirectURIs: uris, TokenEndpointAuthMethod: method, GrantTypes: []string{"authorization_code", "refresh_token"}, ResponseTypes: []string{"code"}}
	if secret != "" {
		// RFC 7591: required when a secret is issued; 0 means it never expires.
		never := int64(0)
		out.ClientSecretExpiresAt = &never
	}
	writeJSON(w, 201, out)
}

// resolveClient returns the client of an authorization request. Metadata
// document clients are fetched when the cached row is missing or expired.
// errClientRejected means the client cannot be used; errCIMDRateLimited
// that a fetch was needed but not allowed now. Other errors are unexpected.
func (s *Server) resolveClient(ctx context.Context, remoteAddr, clientID string) (store.OAuthClient, error) {
	switch {
	case strings.HasPrefix(clientID, "https://"):
	case auth.IsDCRClientID(clientID):
		c, err := s.auth.Store.OAuthClient(ctx, clientID)
		if errors.Is(err, store.ErrClientNotFound) || err == nil && c.Kind != "dcr" {
			return store.OAuthClient{}, errClientRejected
		}
		return c, err
	default:
		return store.OAuthClient{}, errClientRejected
	}
	if !oauth.ValidCIMDClientID(clientID) {
		return store.OAuthClient{}, errClientRejected
	}
	now := s.auth.Now()
	cached, err := s.auth.Store.OAuthClient(ctx, clientID)
	found := err == nil
	if err != nil && !errors.Is(err, store.ErrClientNotFound) {
		return store.OAuthClient{}, err
	}
	if found && cached.MetadataExpiresAt.Valid && cached.MetadataExpiresAt.Int64 > now.Unix() {
		return cached, nil
	}
	if !s.cimdLimiter.allow(remoteAddr) {
		// An expired cache is still better than refusing a known client.
		if found {
			return cached, nil
		}
		return store.OAuthClient{}, errCIMDRateLimited
	}
	doc, err := s.fetchCIMD(ctx, clientID)
	if err != nil {
		log.Printf("oauth: cimd fetch failed host=%s: %v", oauth.RedirectHost(clientID), err)
		return store.OAuthClient{}, errClientRejected
	}
	c := store.OAuthClient{ClientID: clientID, Kind: "cimd", Name: doc.Name, RedirectURIs: doc.RedirectURIs, AuthMethod: "none", MetadataExpiresAt: sqlInt(now.Add(doc.TTL).Unix()), CreatedAt: now.Unix(), UpdatedAt: now.Unix()}
	if err = s.auth.Store.UpsertCIMDClient(ctx, c); err != nil {
		return store.OAuthClient{}, err
	}
	return c, nil
}

// handleAuthorize validates an authorization request and sends the browser
// to the consent page. Errors about the client or redirect URI are answered
// here without redirecting (no open redirect); later errors go back to the
// redirect URI.
func (s *Server) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	p := map[string]string{}
	for _, k := range authorizeParams {
		if v := query[k]; len(v) > 1 {
			oauthFail(w, 400, "invalid_request", "Parameters must not be repeated.")
			return
		} else if len(v) == 1 {
			p[k] = v[0]
		}
	}
	clientID, redirectURI := p["client_id"], p["redirect_uri"]
	if clientID == "" || len(clientID) > oauth.MaxClientIDLength {
		oauthFail(w, 400, "invalid_request", "client_id is missing or too long.")
		return
	}
	if redirectURI == "" || len(redirectURI) > oauth.MaxRedirectURILength {
		oauthFail(w, 400, "invalid_request", "redirect_uri is missing or too long.")
		return
	}
	client, err := s.resolveClient(r.Context(), r.RemoteAddr, clientID)
	switch {
	case err == nil:
	case errors.Is(err, errClientRejected):
		oauthFail(w, 400, "invalid_client", "The client is unknown or its metadata is unusable.")
		return
	case errors.Is(err, errCIMDRateLimited):
		w.Header().Set("Retry-After", "60")
		oauthFail(w, 429, "temporarily_unavailable", "Too many requests. Try again later.")
		return
	default:
		oauthUnavailable(w, r, err)
		return
	}
	if !oauth.MatchRedirectURI(client.RedirectURIs, redirectURI) {
		oauthFail(w, 400, "invalid_request", "redirect_uri is not registered for the client.")
		return
	}
	state := p["state"]
	if len(state) > oauth.MaxStateLength {
		s.redirectError(w, r, redirectURI, "", "invalid_request", "state is too long.")
		return
	}
	if p["response_type"] != "code" {
		s.redirectError(w, r, redirectURI, state, "unsupported_response_type", "response_type must be code.")
		return
	}
	if p["code_challenge_method"] != "S256" {
		s.redirectError(w, r, redirectURI, state, "invalid_request", "code_challenge_method must be S256.")
		return
	}
	if !oauth.ValidCodeChallenge(p["code_challenge"]) {
		s.redirectError(w, r, redirectURI, state, "invalid_request", "code_challenge must be an S256 challenge.")
		return
	}
	scope, err := oauth.NormalizeScope(p["scope"])
	if len(p["scope"]) > oauth.MaxScopeLength || err != nil {
		s.redirectError(w, r, redirectURI, state, "invalid_scope", "scope must be mcp, optionally with offline_access.")
		return
	}
	if v := p["resource"]; v != "" && (len(v) > oauth.MaxResourceLength || !oauth.MatchResource(s.cfg.PublicURL, v)) {
		s.redirectError(w, r, redirectURI, state, "invalid_target", "resource is not this server's MCP endpoint.")
		return
	}
	signed, err := oauth.SignRequest(s.cfg.Secret, oauth.Request{ClientID: client.ClientID, ClientName: client.Name, RedirectURI: redirectURI, Scope: scope, State: state, CodeChallenge: p["code_challenge"], Exp: s.auth.Now().Add(oauth.RequestLifetime).Unix()})
	if err != nil {
		oauthUnavailable(w, r, err)
		return
	}
	http.Redirect(w, r, "/consent?request="+url.QueryEscape(signed), http.StatusFound)
}

// redirectURL returns redirectURI with params, state and iss (RFC 9207).
func (s *Server) redirectURL(redirectURI, state string, params map[string]string) (string, error) {
	params["state"] = state
	params["iss"] = s.cfg.PublicURL
	return oauth.BuildRedirectURL(redirectURI, params)
}

func (s *Server) redirectError(w http.ResponseWriter, r *http.Request, redirectURI, state, code, description string) {
	location, err := s.redirectURL(redirectURI, state, map[string]string{"error": code, "error_description": description})
	if err != nil {
		oauthFail(w, 400, "invalid_request", "redirect_uri is invalid.")
		return
	}
	http.Redirect(w, r, location, http.StatusFound)
}

// consentRequest reads the signed request. A failure has been answered.
func (s *Server) consentRequest(w http.ResponseWriter, v string) (oauth.Request, bool) {
	req, err := oauth.ParseRequest(s.cfg.Secret, v, s.auth.Now())
	if err != nil {
		fail(w, 400, "invalid_request", "The authorization request is invalid or expired.")
		return oauth.Request{}, false
	}
	return req, true
}

func (s *Server) handleGetConsent(w http.ResponseWriter, r *http.Request) {
	u, ok := userFrom(r.Context())
	if !ok {
		s.authError(w, r, store.ErrUnauthorized)
		return
	}
	req, ok := s.consentRequest(w, r.URL.Query().Get("request"))
	if !ok {
		return
	}
	if _, err := s.auth.Store.OAuthClient(r.Context(), req.ClientID); err != nil {
		if errors.Is(err, store.ErrClientNotFound) {
			fail(w, 400, "invalid_request", "The authorization request is invalid or expired.")
			return
		}
		consentUnavailable(w, "get", u.ID, err)
		return
	}
	writeJSON(w, 200, consentResponse{
		ClientName:       req.ClientName,
		ClientKind:       oauth.ClientKind(req.ClientID),
		ClientHost:       clientHost(req.ClientID),
		RedirectHost:     oauth.RedirectHost(req.RedirectURI),
		RedirectLoopback: oauth.IsLoopbackRedirect(req.RedirectURI),
		Scopes:           strings.Fields(req.Scope),
		ExpiresAt:        formatTime(req.Exp),
	})
}

func (s *Server) handlePostConsent(w http.ResponseWriter, r *http.Request) {
	u, ok := userFrom(r.Context())
	if !ok {
		s.authError(w, r, store.ErrUnauthorized)
		return
	}
	sid, ok := sessionFrom(r.Context())
	if !ok {
		s.authError(w, r, store.ErrUnauthorized)
		return
	}
	var in struct {
		Request string `json:"request"`
		Approve *bool  `json:"approve"`
	}
	if !decode(w, r, &in, consentBodyLimit) {
		return
	}
	if in.Approve == nil {
		fail(w, 400, "invalid_request", "approve is required.")
		return
	}
	req, ok := s.consentRequest(w, in.Request)
	if !ok {
		return
	}
	if !*in.Approve {
		location, err := s.redirectURL(req.RedirectURI, req.State, map[string]string{"error": "access_denied"})
		if err != nil {
			fail(w, 400, "invalid_request", "The authorization request is invalid or expired.")
			return
		}
		writeJSON(w, 200, consentResultResponse{RedirectURL: location})
		return
	}
	code, codeHash, err := auth.NewOAuthCode()
	if err != nil {
		consentUnavailable(w, "approve", u.ID, err)
		return
	}
	id, err := auth.Random()
	if err != nil {
		consentUnavailable(w, "approve", u.ID, err)
		return
	}
	now := s.auth.Now().Unix()
	codeExpires := now + int64(oauth.CodeLifetime/time.Second)
	g := store.OAuthGrant{ID: id, UserID: u.ID, ClientID: req.ClientID, ClientName: req.ClientName, Scope: req.Scope, Resource: auth.MCPResource(s.cfg.PublicURL), RedirectURI: req.RedirectURI, CodeHash: codeHash, CodeChallenge: req.CodeChallenge, CodeExpiresAt: codeExpires, CreatedAt: now, ExpiresAt: codeExpires}
	location, err := s.redirectURL(req.RedirectURI, req.State, map[string]string{"code": code})
	if err != nil {
		fail(w, 400, "invalid_request", "The authorization request is invalid or expired.")
		return
	}
	err = s.auth.Store.CreateOAuthGrant(r.Context(), g, sid, now)
	switch {
	case err == nil:
	case errors.Is(err, store.ErrUnauthorized):
		s.authError(w, r, err)
		return
	case errors.Is(err, store.ErrClientNotFound):
		fail(w, 400, "invalid_request", "The authorization request is invalid or expired.")
		return
	default:
		consentUnavailable(w, "approve", u.ID, err)
		return
	}
	writeJSON(w, 200, consentResultResponse{RedirectURL: location})
}

// clientCredentials returns the client_id and secret of a token request from
// HTTP Basic authentication (RFC 6749 section 2.3.1) or the form. viaBasic
// tells whether Authorization was used; ok is false for a malformed or
// conflicting header.
func clientCredentials(r *http.Request, form url.Values) (id, secret string, viaBasic, ok bool) {
	header := r.Header.Get("Authorization")
	if header == "" {
		return form.Get("client_id"), form.Get("client_secret"), false, true
	}
	encoded, found := strings.CutPrefix(header, "Basic ")
	if !found {
		return "", "", true, false
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", "", true, false
	}
	rawID, rawSecret, found := strings.Cut(string(decoded), ":")
	if !found {
		return "", "", true, false
	}
	if id, err = url.QueryUnescape(rawID); err != nil {
		return "", "", true, false
	}
	if secret, err = url.QueryUnescape(rawSecret); err != nil {
		return "", "", true, false
	}
	if v := form.Get("client_id"); v != "" && v != id {
		return "", "", true, false
	}
	return id, secret, true, true
}

// authenticateClient returns the client of a token request from the stored
// row only; metadata documents are not fetched again. Public clients are
// accepted by client_id alone, and a secret they send is not checked.
// errClientRejected means authentication failed.
func (s *Server) authenticateClient(ctx context.Context, id, secret string) (store.OAuthClient, error) {
	if id == "" || len(id) > oauth.MaxClientIDLength {
		return store.OAuthClient{}, errClientRejected
	}
	c, err := s.auth.Store.OAuthClient(ctx, id)
	if errors.Is(err, store.ErrClientNotFound) {
		return store.OAuthClient{}, errClientRejected
	}
	if err != nil {
		return store.OAuthClient{}, err
	}
	if c.AuthMethod == "none" {
		return c, nil
	}
	if secret == "" || !c.SecretHash.Valid || !hmac.Equal([]byte(auth.Hash(secret)), []byte(c.SecretHash.String)) {
		return store.OAuthClient{}, errClientRejected
	}
	return c, nil
}

// handleToken implements the token endpoint for authorization_code and
// refresh_token grants. Cache-Control: no-store is set by secureHeaders.
func (s *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	if !hasMediaType(r, "application/x-www-form-urlencoded") {
		oauthFail(w, 415, "invalid_request", "Use application/x-www-form-urlencoded.")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, oauthBodyLimit))
	if err != nil {
		var large *http.MaxBytesError
		if errors.As(err, &large) {
			oauthFail(w, 413, "invalid_request", "Request body is too large.")
		} else {
			oauthFail(w, 400, "invalid_request", "Invalid request body.")
		}
		return
	}
	// Only the body is read; parameters in the query string are ignored.
	form, err := url.ParseQuery(string(body))
	if err != nil {
		oauthFail(w, 400, "invalid_request", "Invalid request body.")
		return
	}
	for _, v := range form {
		if len(v) > 1 {
			oauthFail(w, 400, "invalid_request", "Parameters must not be repeated.")
			return
		}
	}
	id, secret, viaBasic, ok := clientCredentials(r, form)
	var client store.OAuthClient
	if ok {
		client, err = s.authenticateClient(r.Context(), id, secret)
		if err != nil && !errors.Is(err, errClientRejected) {
			oauthUnavailable(w, r, err)
			return
		}
		ok = err == nil
	}
	if !ok {
		status := 400
		if viaBasic {
			w.Header().Set("WWW-Authenticate", `Basic realm="astovia"`)
			status = 401
		}
		oauthFail(w, status, "invalid_client", "Client authentication failed.")
		return
	}
	switch form.Get("grant_type") {
	case "":
		oauthFail(w, 400, "invalid_request", "grant_type is required.")
	case "authorization_code":
		s.authorizationCodeGrant(w, r, form, client)
	case "refresh_token":
		s.refreshTokenGrant(w, r, form, client)
	default:
		oauthFail(w, 400, "unsupported_grant_type", "grant_type must be authorization_code or refresh_token.")
	}
}

// checkResource reports whether the optional resource parameter names this
// server's MCP resource, answering invalid_target when it does not.
func (s *Server) checkResource(w http.ResponseWriter, form url.Values) bool {
	v := form.Get("resource")
	if v != "" && (len(v) > oauth.MaxResourceLength || !oauth.MatchResource(s.cfg.PublicURL, v)) {
		oauthFail(w, 400, "invalid_target", "resource is not this server's MCP endpoint.")
		return false
	}
	return true
}

// issuedTokens holds new tokens created before the store transaction, so the
// issue callback only builds rows and never touches the database.
type issuedTokens struct {
	access, accessHash, refresh, refreshHash string
	now                                      int64
}

func newIssuedTokens(now time.Time) (issuedTokens, error) {
	t := issuedTokens{now: now.Unix()}
	var err error
	if t.access, t.accessHash, err = auth.NewOAuthAccessToken(); err != nil {
		return t, err
	}
	t.refresh, t.refreshHash, err = auth.NewOAuthRefreshToken()
	return t, err
}

// rows returns the token rows for scope: a refresh token only with
// offline_access, and a longer-lived access token without it.
func (t issuedTokens) rows(scope string) []store.OAuthToken {
	if !oauth.HasScope(scope, oauth.ScopeOfflineAccess) {
		return []store.OAuthToken{{Hash: t.accessHash, Kind: store.OAuthAccess, CreatedAt: t.now, ExpiresAt: t.now + int64(oauth.LongAccessLifetime/time.Second)}}
	}
	return []store.OAuthToken{
		{Hash: t.accessHash, Kind: store.OAuthAccess, CreatedAt: t.now, ExpiresAt: t.now + int64(oauth.AccessLifetime/time.Second)},
		{Hash: t.refreshHash, Kind: store.OAuthRefresh, CreatedAt: t.now, ExpiresAt: t.now + int64(oauth.RefreshLifetime/time.Second)},
	}
}

func (t issuedTokens) response(scope string) tokenResponse {
	out := tokenResponse{AccessToken: t.access, TokenType: "Bearer", ExpiresIn: int64(oauth.LongAccessLifetime / time.Second), Scope: scope}
	if oauth.HasScope(scope, oauth.ScopeOfflineAccess) {
		out.ExpiresIn = int64(oauth.AccessLifetime / time.Second)
		out.RefreshToken = t.refresh
	}
	return out
}

// grantError answers an error from RedeemOAuthCode or RotateOAuthRefresh.
func grantError(w http.ResponseWriter, r *http.Request, err error, description string) {
	switch {
	case errors.Is(err, oauth.ErrInvalidScope):
		oauthFail(w, 400, "invalid_scope", "scope exceeds the granted scope.")
	case errors.Is(err, oauth.ErrInvalidGrant), errors.Is(err, store.ErrUnauthorized), errors.Is(err, store.ErrCodeReused):
		oauthFail(w, 400, "invalid_grant", description)
	default:
		oauthUnavailable(w, r, err)
	}
}

func (s *Server) authorizationCodeGrant(w http.ResponseWriter, r *http.Request, form url.Values, client store.OAuthClient) {
	const invalid = "The authorization code is invalid."
	code, redirectURI, verifier := form.Get("code"), form.Get("redirect_uri"), form.Get("code_verifier")
	if code == "" || redirectURI == "" || verifier == "" {
		oauthFail(w, 400, "invalid_request", "code, redirect_uri and code_verifier are required.")
		return
	}
	if !s.checkResource(w, form) {
		return
	}
	if !auth.IsOAuthCode(code) {
		oauthFail(w, 400, "invalid_grant", invalid)
		return
	}
	now := s.auth.Now()
	tokens, err := newIssuedTokens(now)
	if err != nil {
		oauthUnavailable(w, r, err)
		return
	}
	resource := auth.MCPResource(s.cfg.PublicURL)
	g, err := s.auth.Store.RedeemOAuthCode(r.Context(), auth.Hash(code), now.Unix(), func(g store.OAuthGrant) ([]store.OAuthToken, error) {
		// The redirect URI must equal the one of the authorization request
		// exactly (RFC 6749 section 4.1.3); the loopback port is not ignored.
		if g.ClientID != client.ClientID || g.RedirectURI != redirectURI || g.Resource != resource || !oauth.VerifyPKCE(verifier, g.CodeChallenge) {
			return nil, oauth.ErrInvalidGrant
		}
		return tokens.rows(g.Scope), nil
	})
	if err != nil {
		grantError(w, r, err, invalid)
		return
	}
	writeJSON(w, 200, tokens.response(g.Scope))
}

func (s *Server) refreshTokenGrant(w http.ResponseWriter, r *http.Request, form url.Values, client store.OAuthClient) {
	const invalid = "The refresh token is invalid."
	presented := form.Get("refresh_token")
	if presented == "" {
		oauthFail(w, 400, "invalid_request", "refresh_token is required.")
		return
	}
	requested := ""
	if v := form.Get("scope"); v != "" {
		n, err := oauth.NormalizeScope(v)
		if len(v) > oauth.MaxScopeLength || err != nil {
			oauthFail(w, 400, "invalid_scope", "scope must be mcp, optionally with offline_access.")
			return
		}
		requested = n
	}
	if !s.checkResource(w, form) {
		return
	}
	if !auth.IsOAuthRefreshToken(presented) {
		oauthFail(w, 400, "invalid_grant", invalid)
		return
	}
	now := s.auth.Now()
	tokens, err := newIssuedTokens(now)
	if err != nil {
		oauthUnavailable(w, r, err)
		return
	}
	resource := auth.MCPResource(s.cfg.PublicURL)
	g, err := s.auth.Store.RotateOAuthRefresh(r.Context(), auth.Hash(presented), now.Unix(), func(g store.OAuthGrant) ([]store.OAuthToken, error) {
		// A grant for another public URL would only yield tokens that /mcp
		// rejects, so it is not refreshed.
		if g.ClientID != client.ClientID || g.Resource != resource {
			return nil, oauth.ErrInvalidGrant
		}
		// A narrower scope is accepted, but the tokens keep the grant's scope.
		if requested != "" && !oauth.ScopeSubset(requested, g.Scope) {
			return nil, oauth.ErrInvalidScope
		}
		return tokens.rows(g.Scope), nil
	})
	if err != nil {
		grantError(w, r, err, invalid)
		return
	}
	writeJSON(w, 200, tokens.response(g.Scope))
}

func toConnectionResponse(g store.OAuthGrant) connectionResponse {
	out := connectionResponse{ID: g.ID, ClientName: g.ClientName, ClientHost: clientHost(g.ClientID), Scopes: strings.Fields(g.Scope), CreatedAt: formatTime(g.CreatedAt), ExpiresAt: formatTime(g.ExpiresAt)}
	if g.LastUsedAt.Valid {
		v := formatTime(g.LastUsedAt.Int64)
		out.LastUsedAt = &v
	}
	return out
}

func (s *Server) handleListConnections(w http.ResponseWriter, r *http.Request) {
	u, ok := userFrom(r.Context())
	if !ok {
		s.authError(w, r, store.ErrUnauthorized)
		return
	}
	grants, err := s.auth.Store.ListOAuthGrants(r.Context(), u.ID, s.auth.Now().Unix())
	if err != nil {
		consentUnavailable(w, "list", u.ID, err)
		return
	}
	out := connectionListResponse{Items: make([]connectionResponse, 0, len(grants))}
	for _, g := range grants {
		out.Items = append(out.Items, toConnectionResponse(g))
	}
	writeJSON(w, 200, out)
}

func (s *Server) handleDeleteConnection(w http.ResponseWriter, r *http.Request) {
	u, ok := userFrom(r.Context())
	if !ok {
		s.authError(w, r, store.ErrUnauthorized)
		return
	}
	id := r.PathValue("id")
	if !itemIDPattern.MatchString(id) {
		fail(w, 404, "not_found", "Connection not found.")
		return
	}
	err := s.auth.Store.DeleteOAuthGrant(r.Context(), u.ID, id)
	if errors.Is(err, store.ErrGrantNotFound) {
		fail(w, 404, "not_found", "Connection not found.")
		return
	}
	if err != nil {
		consentUnavailable(w, "delete", u.ID, err)
		return
	}
	w.WriteHeader(204)
}
