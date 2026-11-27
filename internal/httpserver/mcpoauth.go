package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/i2bskn/astovia/internal/config"
	"github.com/i2bskn/astovia/internal/store"
	"github.com/i2bskn/authlume/mcpoauth"
)

// Scopes of the MCP authorization server. mcp grants access to /mcp.
// offline_access is accepted for clients that ask for it; every connection
// gets a refresh token whether or not it is requested.
const (
	scopeMCP           = "mcp"
	scopeOfflineAccess = "offline_access"
)

// authorizePath is the authorization endpoint: the consent screen of the
// web UI, which sends its query to /api/v1/oauth/authorize.
const authorizePath = "/oauth/authorize"

// consentBodyLimit bounds the body of POST /api/v1/oauth/authorize.
const consentBodyLimit = 16 << 10

// maxClientNameRunes bounds the client name shown and stored as a label.
const maxClientNameRunes = 128

// mcpOAuthEnabled reports whether c enables the MCP authorization server:
// mcpoauth requires an https issuer, so a local http PublicURL serves /mcp
// with API tokens only.
func mcpOAuthEnabled(c config.Config) bool { return strings.HasPrefix(c.PublicURL, "https://") }

// newMCPOAuth returns the authorization server of PublicURL, or nil when
// mcpOAuthEnabled is false. Clients are resolved from Client ID Metadata
// Documents of any host and from dynamic client registration; both are
// public clients. now is read on every call.
func newMCPOAuth(c config.Config, st *store.Store, now func() time.Time) *mcpoauth.Server {
	if !mcpOAuthEnabled(c) {
		return nil
	}
	base := c.PublicURL
	srv, err := mcpoauth.New(mcpoauth.Config{
		Issuer:                base,
		Resource:              base + "/mcp",
		AuthorizationEndpoint: base + authorizePath,
		TokenEndpoint:         base + "/oauth/token",
		RevocationEndpoint:    base + "/oauth/revoke",
		Scopes:                []string{scopeMCP, scopeOfflineAccess},
		DefaultScopes:         []string{scopeMCP},
		AccessTTL:             c.MCP.AccessTTL,
		RefreshIdle:           c.MCP.RefreshIdle,
		ConnectionAbsolute:    c.MCP.ConnectionAbsolute,
		Now:                   now,
		MetadataDocuments:     &mcpoauth.MetadataDocumentConfig{},
		Registration:          &mcpoauth.RegistrationConfig{Endpoint: base + "/oauth/register", Store: st.MCPClients()},
		// The ceiling of the clients from metadata documents and
		// registrations. It is every scope today and keeps a future
		// scope from being granted to them by default.
		RestrictDynamicScopes: true,
		DynamicScopes:         []string{scopeMCP, scopeOfflineAccess},
	}, nil, st.MCPOAuth(now))
	if err != nil {
		// PublicURL is a canonical https origin and the lifetimes were
		// checked by config.Load, so the configuration is static.
		panic("httpserver: mcpoauth: " + err.Error())
	}
	return srv
}

// routeOAuth registers the metadata and the machine-facing OAuth endpoints.
// The authorization endpoint is the consent screen of the web UI. Without
// the authorization server none of them exists and they are JSON 404s.
func (s *Server) routeOAuth(mux *patternMux) {
	if s.mcp == nil {
		return
	}
	metadata := allow(http.MethodGet, http.HandlerFunc(s.mcp.Metadata))
	resource := allow(http.MethodGet, http.HandlerFunc(s.mcp.ResourceMetadata))
	mux.Handle(s.mcp.MetadataPath(), metadata)
	mux.Handle(s.mcp.ResourceMetadataPath(), resource)
	// Clients that look at the root find the same resource: /mcp is the
	// only one.
	mux.Handle("/.well-known/oauth-protected-resource", resource)
	mux.Handle("/oauth/register", allow(http.MethodPost, http.HandlerFunc(s.handleRegister)))
	mux.Handle("/oauth/token", allow(http.MethodPost, http.HandlerFunc(s.handleToken)))
	mux.Handle("/oauth/revoke", allow(http.MethodPost, http.HandlerFunc(s.handleRevoke)))
}

// tokenBodyLimit bounds the body of the token and revocation endpoints. It
// is the limit of mcpoauth, which reads the body again.
const tokenBodyLimit = 16 << 10

// oauthLog logs a token or revocation outcome worth keeping. Detected reuse
// is logged with the connection it revoked; a client metadata document
// failure only with the host of clientID and a fixed class, since its error
// quotes the URL and the network error; other backend failures with their
// error. Codes and tokens never appear in Event or the errors.
func oauthLog(endpoint, clientID string, ev mcpoauth.Event, err error) {
	var metaErr *mcpoauth.MetadataError
	switch {
	case err == nil:
	case errors.Is(err, mcpoauth.ErrReused) && ev.ConnectionID == "":
		// The connection was already gone, for example revoked by an
		// earlier reuse.
		log.Printf("oauth: %s: reuse detected client=%q", endpoint, ev.ClientID)
	case errors.Is(err, mcpoauth.ErrReused):
		log.Printf("oauth: %s: reuse detected, connection revoked client=%q connection=%q user=%s", endpoint, ev.ClientID, ev.ConnectionID, ev.Subject)
	case errors.As(err, &metaErr):
		log.Printf("oauth: %s: client metadata: %s host=%s", endpoint, metadataClass(metaErr), metadataHost(metaErr.ClientID))
	case metadataUnavailable(err):
		log.Printf("oauth: %s: client metadata: unavailable host=%s", endpoint, metadataHost(clientID))
	case errors.Is(err, mcpoauth.ErrInvalid):
	default:
		log.Printf("oauth: %s: %v", endpoint, err)
	}
}

// metadataClass returns the fixed class of a metadata document failure that
// is logged in place of its reason. mcpoauth exports the reason of a blocked
// address only as text; TestMCPOAuthMetadataErrors makes authlume return it
// and fails when the text changes.
func metadataClass(err *mcpoauth.MetadataError) string {
	switch {
	case err.Mismatch:
		return "mismatch"
	case err.Reason == "blocked address":
		return "blocked"
	default:
		return "invalid"
	}
}

// metadataUnavailable reports whether err is a temporary failure to fetch a
// client metadata document. mcpoauth does not export this error, so its
// message is matched; TestMCPOAuthMetadataErrors makes authlume return it
// and fails when the message changes.
func metadataUnavailable(err error) bool {
	return strings.Contains(err.Error(), "client metadata document unavailable")
}

// revokeMismatchedClient revokes every connection of the client whose
// registration no longer matches: a metadata document that names another
// client_id, or a registration that is gone. It reports whether err was such
// a mismatch. The revocation depends only on what is served at that URL, so
// an unauthenticated request cannot use it against another client.
func (s *Server) revokeMismatchedClient(ctx context.Context, err error) bool {
	var metaErr *mcpoauth.MetadataError
	if !errors.As(err, &metaErr) || !metaErr.Mismatch {
		return false
	}
	host := metadataHost(metaErr.ClientID)
	n, rerr := s.auth.Store.RevokeMCPClient(ctx, metaErr.ClientID)
	if rerr != nil {
		log.Printf("oauth: client metadata: mismatch host=%s: revoke: %v", host, rerr)
	} else {
		log.Printf("oauth: client metadata: mismatch host=%s revoked=%d", host, n)
	}
	return true
}

// handleRegister serves dynamic client registration behind a per-peer limit.
func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	// Counted before any validation, so malformed requests count as well.
	if !s.registerLimiter.allow(s.clientAddr(r)) {
		w.Header().Set("Retry-After", "60")
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "invalid_request", "error_description": "Too many registrations. Try again later."})
		return
	}
	_, err := s.mcp.Register(w, r)
	oauthLog("register", "", mcpoauth.Event{}, err)
}

// tokenClient reads the body of a token or revocation request, puts it back
// for mcpoauth and returns the client_id the request names, from HTTP Basic
// or the body, or "". It reports false after writing a response.
//
// A client_id that is a metadata document URL is resolved, and so possibly
// fetched, before anything else of the request is checked, and these
// endpoints need no signed-in user. Every real client has a code record by
// the time it exchanges a code and a connection when it refreshes or
// revokes, so a URL that no record names is answered here without a fetch:
// otherwise anyone could make the server fetch any public URL. The token
// endpoint refuses it as invalid_client. The revocation endpoint answers 200
// as RFC 7009 does for a token that is no longer valid, since such a client
// has none, for example after the user deleted its connection.
func (s *Server) tokenClient(w http.ResponseWriter, r *http.Request, revocation bool) (string, bool) {
	w.Header().Set("Cache-Control", "no-store")
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, tokenBodyLimit))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
		return "", false
	}
	r.Body = io.NopCloser(bytes.NewReader(raw))
	clientID := ""
	if user, _, ok := r.BasicAuth(); ok {
		clientID, _ = url.QueryUnescape(user)
	} else if values, err := url.ParseQuery(string(raw)); err == nil {
		clientID = values.Get("client_id")
	}
	if !strings.HasPrefix(clientID, "https://") {
		return clientID, true
	}
	inUse, err := s.auth.Store.MCPClientInUse(r.Context(), clientID)
	if err != nil {
		log.Printf("oauth: client in use: %v", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "temporarily_unavailable"})
		return "", false
	}
	if !inUse && revocation {
		w.WriteHeader(http.StatusOK)
		return "", false
	}
	if !inUse {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid_client"})
		return "", false
	}
	return clientID, true
}

// handleToken serves the token endpoint. A client from a metadata document
// may make the server fetch its document here when its cached copy has
// expired; the library bounds the concurrent fetches.
func (s *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	clientID, ok := s.tokenClient(w, r, false)
	if !ok {
		return
	}
	ev, err := s.mcp.Token(w, r)
	s.revokeMismatchedClient(r.Context(), err)
	oauthLog("token", clientID, ev, err)
}

// handleRevoke serves RFC 7009 token revocation.
func (s *Server) handleRevoke(w http.ResponseWriter, r *http.Request) {
	clientID, ok := s.tokenClient(w, r, true)
	if !ok {
		return
	}
	ev, err := s.mcp.Revocation(w, r)
	s.revokeMismatchedClient(r.Context(), err)
	oauthLog("revoke", clientID, ev, err)
}

// authorizeResponse describes a valid authorization request on the consent
// screen. client_name is asserted by the client, at registration or in its
// metadata document. client_host is the verified host of a metadata
// document's URL, and null for registered clients, whose names are their own
// claim. user_id is the signed-in user, which the screen sends back with the
// decision.
type authorizeResponse struct {
	Status           string   `json:"status"`
	ClientName       string   `json:"client_name"`
	ClientID         string   `json:"client_id"`
	ClientHost       *string  `json:"client_host"`
	RedirectHost     string   `json:"redirect_host"`
	RedirectLoopback bool     `json:"redirect_loopback"`
	Scopes           []string `json:"scopes"`
	UserID           string   `json:"user_id"`
}

// authorizeErrorResponse describes an error that is returned to the client.
// The screen shows it and navigates to redirect_url only when the user asks.
type authorizeErrorResponse struct {
	Status       string `json:"status"`
	RedirectURL  string `json:"redirect_url"`
	Error        string `json:"error"`
	RedirectHost string `json:"redirect_host"`
}

type redirectResponse struct {
	RedirectURL string `json:"redirect_url"`
}

// redirectHost returns the lower-case host name of a redirect URI.
func redirectHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// isLoopbackRedirect reports whether raw goes to localhost or a loopback IP
// address, where any local program can receive the response.
func isLoopbackRedirect(raw string) bool {
	host := redirectHost(raw)
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	addr, err := netip.ParseAddr(host)
	return err == nil && addr.IsLoopback()
}

// clientHost returns the host shown for a client: the verified host of its
// metadata document, or else the host of its redirect URI.
func clientHost(req mcpoauth.Request) string {
	if req.ClientHost != "" {
		return req.ClientHost
	}
	return redirectHost(req.RedirectURI)
}

// optional returns nil for "" and &v otherwise.
func optional(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}

// clientName cleans a client-supplied display name: control and format
// characters and line or paragraph separators are removed, surrounding spaces
// trimmed, fallback used when nothing is left, and the result cut to
// maxClientNameRunes.
func clientName(raw, fallback string) string {
	name := strings.Map(func(r rune) rune {
		if r == utf8.RuneError || unicode.In(r, unicode.Cc, unicode.Cf, unicode.Zl, unicode.Zp) {
			return -1
		}
		return r
	}, raw)
	name = strings.TrimFunc(name, unicode.IsSpace)
	if name == "" {
		name = fallback
	}
	if utf8.RuneCountInString(name) > maxClientNameRunes {
		name = string([]rune(name)[:maxClientNameRunes])
	}
	return name
}

// metadataHost returns the lower-case authority of a client_id URL, or "".
func metadataHost(clientID string) string {
	u, err := url.Parse(clientID)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Host)
}

// limitClientLookup counts an authorization request whose client_id is a
// metadata document URL, which can make the server fetch it, against the
// signed-in user's limit. It reports false after writing 429.
func (s *Server) limitClientLookup(w http.ResponseWriter, userID, query string) bool {
	values, err := url.ParseQuery(query)
	if err != nil || !strings.HasPrefix(values.Get("client_id"), "https://") {
		return true
	}
	if s.clientLimiter.allowKey("client-lookup:" + userID) {
		return true
	}
	w.Header().Set("Retry-After", "60")
	fail(w, http.StatusTooManyRequests, "rate_limited", "Too many client lookups. Try again later.")
	return false
}

// unknownClientMessage tells the user what to do about a client that is not
// registered or whose registration has expired.
const unknownClientMessage = "The client is unknown. Remove this server in the client and add it again."

// authorizeFailed answers an error of Authorize, Approve or Deny that is not
// a *mcpoauth.RejectError: none of them is reported to the client. Only the
// host and a fixed class of a metadata document failure are logged.
func (s *Server) authorizeFailed(w http.ResponseWriter, r *http.Request, op string, err error) {
	var metaErr *mcpoauth.MetadataError
	switch {
	case s.revokeMismatchedClient(r.Context(), err):
		fail(w, http.StatusBadRequest, "invalid_client", "The client metadata is invalid.")
	case errors.As(err, &metaErr) && metaErr.Reason == "blocked address":
		// The reason is pinned by TestMCPOAuthMetadataErrors. Answered
		// like a failed fetch, so that the response does not tell whether
		// a name exists on an internal network.
		log.Printf("oauth: client metadata: blocked host=%s", metadataHost(metaErr.ClientID))
		fail(w, http.StatusServiceUnavailable, "client_metadata_unavailable", "The client metadata could not be fetched. Try again later.")
	case errors.As(err, &metaErr):
		log.Printf("oauth: client metadata: invalid host=%s", metadataHost(metaErr.ClientID))
		fail(w, http.StatusBadRequest, "invalid_client", "The client metadata is invalid.")
	case errors.Is(err, mcpoauth.ErrUnknownClient):
		fail(w, http.StatusBadRequest, "unknown_client", unknownClientMessage)
	case errors.Is(err, mcpoauth.ErrInvalid):
		fail(w, http.StatusBadRequest, "invalid_request", "The authorization request is invalid.")
	case errors.Is(err, store.ErrUnauthorized):
		// The user was disabled or the session ended after requireUser.
		s.authError(w, r, err)
	case metadataUnavailable(err):
		// A temporary fetch failure; its message may quote the network
		// error, so only the class is logged.
		values, _ := url.ParseQuery(r.URL.RawQuery)
		log.Printf("oauth: client metadata: unavailable op=%s host=%s", op, metadataHost(values.Get("client_id")))
		fail(w, http.StatusServiceUnavailable, "client_metadata_unavailable", "The client metadata could not be fetched. Try again later.")
	default:
		log.Printf("oauth: %s: %v", op, err)
		fail(w, 503, "unavailable", "The service is temporarily unavailable. Try again.")
	}
}

// withError returns location with its error parameter replaced by code.
func withError(location, code string) (string, bool) {
	u, err := url.Parse(location)
	if err != nil {
		return "", false
	}
	q := u.Query()
	q.Set("error", code)
	u.RawQuery = q.Encode()
	return u.String(), true
}

// invalidScopeURL returns the client's redirect URL with error=invalid_scope,
// for a request without the mcp scope, which grants nothing on this server.
func (s *Server) invalidScopeURL(ctx context.Context, query string) (string, error) {
	location, err := s.mcp.Deny(ctx, query)
	if err != nil {
		return "", err
	}
	location, ok := withError(location, "invalid_scope")
	if !ok {
		return "", mcpoauth.ErrInvalid
	}
	return location, nil
}

// handleGetAuthorize returns what the consent screen shows for the
// authorization request in the query string.
func (s *Server) handleGetAuthorize(w http.ResponseWriter, r *http.Request) {
	if s.mcp == nil {
		fail(w, 404, "not_found", "Not found.")
		return
	}
	u, ok := userFrom(r.Context())
	if !ok {
		s.authError(w, r, store.ErrUnauthorized)
		return
	}
	query := r.URL.RawQuery
	if !s.limitClientLookup(w, u.ID, query) {
		return
	}
	op := "authorize user=" + u.ID
	req, err := s.mcp.Authorize(r.Context(), query)
	var reject *mcpoauth.RejectError
	switch {
	case errors.As(err, &reject):
		writeJSON(w, http.StatusOK, authorizeErrorResponse{Status: "error", RedirectURL: reject.RedirectURL, Error: reject.Code, RedirectHost: redirectHost(reject.RedirectURL)})
		return
	case err != nil:
		s.authorizeFailed(w, r, op, err)
		return
	}
	if !slices.Contains(req.Scopes, scopeMCP) {
		location, err := s.invalidScopeURL(r.Context(), query)
		if err != nil {
			s.authorizeFailed(w, r, op, err)
			return
		}
		writeJSON(w, http.StatusOK, authorizeErrorResponse{Status: "error", RedirectURL: location, Error: "invalid_scope", RedirectHost: redirectHost(location)})
		return
	}
	writeJSON(w, http.StatusOK, authorizeResponse{
		Status:           "ok",
		ClientName:       clientName(req.ClientName, clientHost(req)),
		ClientID:         req.ClientID,
		ClientHost:       optional(req.ClientHost),
		RedirectHost:     redirectHost(req.RedirectURI),
		RedirectLoopback: isLoopbackRedirect(req.RedirectURI),
		Scopes:           req.Scopes,
		UserID:           u.ID,
	})
}

// handlePostAuthorize answers the consent screen: it validates the
// authorization request again and returns where to send the browser. The
// request is not stored between the screen and the decision, so the screen
// sends back the user and the client it showed: a decision made for another
// user after a sign-in switch (409 user_changed), or for a client whose name
// or verified host changed since the screen was shown (409 client_changed),
// is not applied. An approval issues the code only while the session that
// approved it is active, which the store checks under the lock of the user
// row.
func (s *Server) handlePostAuthorize(w http.ResponseWriter, r *http.Request) {
	if s.mcp == nil {
		fail(w, 404, "not_found", "Not found.")
		return
	}
	u, ok := userFrom(r.Context())
	sid, ok2 := sessionFrom(r.Context())
	if !ok || !ok2 {
		s.authError(w, r, store.ErrUnauthorized)
		return
	}
	var in struct {
		Query      *string         `json:"query"`
		Decision   *string         `json:"decision"`
		UserID     *string         `json:"user_id"`
		ClientName *string         `json:"client_name"`
		ClientHost json.RawMessage `json:"client_host"`
	}
	if !decode(w, r, &in, consentBodyLimit) {
		return
	}
	var shownHost *string
	if in.Query == nil || in.Decision == nil || in.UserID == nil || in.ClientName == nil || in.ClientHost == nil ||
		(*in.Decision != "approve" && *in.Decision != "deny") || json.Unmarshal(in.ClientHost, &shownHost) != nil {
		fail(w, http.StatusBadRequest, "invalid_request", "query, decision (approve or deny), user_id, client_name and client_host are required.")
		return
	}
	// Checked before the request is resolved, so that a user switch is
	// always reported as such.
	if *in.UserID != u.ID {
		fail(w, http.StatusConflict, "user_changed", "The signed-in user has changed. Reload the page.")
		return
	}
	query := *in.Query
	if !s.limitClientLookup(w, u.ID, query) {
		return
	}
	ctx := r.Context()
	op := "approve user=" + u.ID
	req, err := s.mcp.Authorize(ctx, query)
	var reject *mcpoauth.RejectError
	switch {
	case errors.As(err, &reject):
		writeJSON(w, http.StatusOK, redirectResponse{RedirectURL: reject.RedirectURL})
		return
	case err != nil:
		s.authorizeFailed(w, r, op, err)
		return
	}
	name := clientName(req.ClientName, clientHost(req))
	if *in.ClientName != name || (shownHost == nil) != (req.ClientHost == "") || (shownHost != nil && *shownHost != req.ClientHost) {
		fail(w, http.StatusConflict, "client_changed", "The client has changed. Reload the page.")
		return
	}
	var location string
	approved := false
	switch {
	case *in.Decision == "deny":
		location, err = s.mcp.Deny(ctx, query)
	case !slices.Contains(req.Scopes, scopeMCP):
		location, err = s.invalidScopeURL(ctx, query)
	default:
		_, location, err = s.mcp.Approve(store.WithApprovalSession(ctx, sid, s.auth.Now()), query, u.ID)
		approved = err == nil
	}
	switch {
	case errors.As(err, &reject):
		writeJSON(w, http.StatusOK, redirectResponse{RedirectURL: reject.RedirectURL})
	case err != nil:
		s.authorizeFailed(w, r, op, err)
	default:
		if approved {
			// The label is what the connection list shows; it is stored
			// only once the approval issued a code, and failing to store it
			// does not stop the connection. Only the verified host of a
			// metadata document is stored; a registered client's redirect
			// host is its own claim.
			if lerr := s.auth.Store.SetMCPClientLabel(ctx, req.ClientID, name, req.ClientHost, s.auth.Now()); lerr != nil {
				log.Printf("oauth: client label: %v", lerr)
			}
		}
		writeJSON(w, http.StatusOK, redirectResponse{RedirectURL: location})
	}
}

// connectionResponse is an MCP connection on the account screen. expires_at
// is when the connection ends unless it is used: each refresh extends it, up
// to the absolute limit when one is set.
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

// consentUnavailable answers a database failure of the /api/v1/oauth/*
// handlers.
func consentUnavailable(w http.ResponseWriter, op, userID string, err error) {
	log.Printf("oauth consent: %s user=%s: %v", op, userID, err)
	fail(w, 503, "unavailable", "The service is temporarily unavailable. Try again.")
}

func (s *Server) handleListConnections(w http.ResponseWriter, r *http.Request) {
	u, ok := userFrom(r.Context())
	if !ok {
		s.authError(w, r, store.ErrUnauthorized)
		return
	}
	conns, err := s.auth.Store.ListMCPConnections(r.Context(), u.ID, s.auth.Now())
	if err != nil {
		consentUnavailable(w, "list", u.ID, err)
		return
	}
	out := connectionListResponse{Items: make([]connectionResponse, 0, len(conns))}
	for _, c := range conns {
		name := c.ClientName
		if name == "" {
			name = clientName(metadataHost(c.ClientID), "MCP client")
		}
		item := connectionResponse{ID: c.ID, ClientName: name, ClientHost: optional(c.ClientHost), Scopes: c.Scopes, CreatedAt: formatTime(c.CreatedAt), ExpiresAt: formatTime(c.ExpiresAt)}
		if item.Scopes == nil {
			item.Scopes = []string{}
		}
		if c.LastUsedAt.Valid {
			v := formatTime(c.LastUsedAt.Int64)
			item.LastUsedAt = &v
		}
		out.Items = append(out.Items, item)
	}
	writeJSON(w, 200, out)
}

// handleDeleteConnection deletes one of the signed-in user's connections,
// which invalidates its access and refresh tokens together.
func (s *Server) handleDeleteConnection(w http.ResponseWriter, r *http.Request) {
	u, ok := userFrom(r.Context())
	if !ok {
		s.authError(w, r, store.ErrUnauthorized)
		return
	}
	err := s.auth.Store.DeleteMCPConnection(r.Context(), u.ID, r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		fail(w, 404, "not_found", "Connection not found.")
		return
	}
	if err != nil {
		consentUnavailable(w, "delete", u.ID, err)
		return
	}
	w.WriteHeader(204)
}
