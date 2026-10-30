package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/i2bskn/astovia/internal/access"
	"github.com/i2bskn/astovia/internal/ai"
	"github.com/i2bskn/astovia/internal/aicred"
	"github.com/i2bskn/astovia/internal/auth"
	"github.com/i2bskn/astovia/internal/chatwork"
	"github.com/i2bskn/astovia/internal/config"
	"github.com/i2bskn/astovia/internal/github"
	"github.com/i2bskn/astovia/internal/google"
	"github.com/i2bskn/astovia/internal/notify"
	"github.com/i2bskn/astovia/internal/oauth"
	"github.com/i2bskn/astovia/internal/openai"
	"github.com/i2bskn/astovia/internal/secretbox"
	"github.com/i2bskn/astovia/internal/slack"
	"github.com/i2bskn/astovia/internal/store"
	"io"
	"io/fs"
	"log"
	"mime"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const CookieName = "astovia_refresh"

type Server struct {
	cfg     config.Config
	auth    *auth.Service
	assets  fs.FS
	limiter *loginLimiter
	// accountLimiter limits the public invitation and password reset
	// endpoints together, per peer like limiter.
	accountLimiter *loginLimiter
	// registerLimiter limits OAuth client registrations and cimdLimiter
	// metadata document fetches, per peer like limiter.
	registerLimiter *loginLimiter
	cimdLimiter     *loginLimiter
	// fetchCIMD fetches a Client ID Metadata Document. Tests replace it.
	fetchCIMD func(ctx context.Context, clientID string) (oauth.Document, error)
	// github verifies GitHub tokens, slack Slack tokens, chatwork Chatwork
	// API tokens and ai API keys; google connects and revokes Google
	// accounts with the OAuth client of cfg.Google. Tests replace their
	// base URLs (and cfg.Google).
	github   *github.Client
	slack    *slack.Client
	chatwork *chatwork.Client
	google   *google.Client
	ai       *ai.Client
	// box seals stored credentials, and vapid manages the Web Push key
	// pair; both are the ones the background loops use as well.
	box   *secretbox.Box
	vapid *notify.VAPID
	// tokens manages the access tokens of the ChatGPT plan and holds the
	// OpenAI client (with the host ID) for its authorizations; it is the
	// one the synchronizations use, so that refreshes are serialized.
	tokens *aicred.Tokens
	// pushTestLimiter limits test notifications per user.
	pushTestLimiter *loginLimiter
	// aiTryLimiter limits the tries of AI instructions per user; the reads
	// of the integrations are limited by their own limiters as well.
	aiTryLimiter *loginLimiter
	// aiEndpointLimiter limits the checks of an OpenAI API key or an
	// OpenAI-compatible endpoint made when saving the AI settings, per user.
	aiEndpointLimiter *loginLimiter
	// chatworkMCPLimiter limits the Chatwork API calls made by the MCP
	// tools per user, leaving most of Chatwork's limit (shared with the
	// synchronization) to the synchronization.
	chatworkMCPLimiter *loginLimiter
	// The Slack limiters limit the Slack API calls made by the MCP tools
	// per user and method, so that twice their limits (at the edge of a
	// window) and the synchronization's budget stay within Slack's limits
	// for the methods. slackHistoryLimiter counts conversations.history and
	// conversations.replies together.
	slackSearchLimiter        *loginLimiter
	slackConversationsLimiter *loginLimiter
	slackHistoryLimiter       *loginLimiter
	slackUsersLimiter         *loginLimiter
	// gmailMCPLimiter limits the Gmail API quota units used by the MCP tools
	// per Google account, so that twice its limit (at the edge of a window)
	// and the synchronization's budget stay within Gmail's limit per
	// account. gmailTokens keeps the access tokens of the MCP tools apart
	// from the synchronization's.
	gmailMCPLimiter *loginLimiter
	gmailTokens     *gmailTokenCache
	// accessOIDC is the Cloudflare Access client of cfg.Access; it is set
	// exactly when cfg.Access is. Tests replace it.
	accessOIDC accessClient
}

// userResponse is the public representation of the signed-in user.
// PasswordReset reports a password reset link the user completed recently,
// so that a reset the user did not ask for is noticed; it is null otherwise.
type userResponse struct {
	ID            string                 `json:"id"`
	Email         string                 `json:"email"`
	Role          string                 `json:"role"`
	PasswordReset *passwordResetResponse `json:"password_reset"`
}

type passwordResetResponse struct {
	At      string `json:"at"`
	ByEmail string `json:"by_email"`
}

// authResponse is returned by login and refresh.
type authResponse struct {
	AccessToken string       `json:"access_token"`
	ExpiresIn   int          `json:"expires_in"`
	User        userResponse `json:"user"`
}

// toUserResponse builds the user of /api/v1/me and the login responses. The
// password reset notice is read with a separate query; a failure is logged
// and reported as null so that a login or refresh, whose refresh token has
// already been rotated, still returns its new cookie.
func (s *Server) toUserResponse(ctx context.Context, u store.User) userResponse {
	out := userResponse{ID: u.ID, Email: u.Email, Role: u.Role}
	notice, err := s.auth.Store.LatestResetNotice(ctx, u.ID, s.auth.Now().Unix())
	if err != nil {
		log.Printf("auth: password reset notice user=%s: %v", u.ID, err)
		return out
	}
	if notice != nil {
		out.PasswordReset = &passwordResetResponse{At: formatTime(notice.At), ByEmail: notice.ByEmail}
	}
	return out
}

// New returns the application handler. tokens is shared with the
// synchronizations; nil makes one for the server alone.
func New(c config.Config, a *auth.Service, assets fs.FS, tokens *aicred.Tokens) http.Handler {
	return newServer(c, a, assets, tokens).handler()
}

// newServer builds a Server without wrapping it, so tests in this package can
// adjust the limiters and the CIMD fetcher before calling handler. A nil
// tokens is made from the server's store, box and clock.
func newServer(c config.Config, a *auth.Service, assets fs.FS, tokens *aicred.Tokens) *Server {
	cimd := oauth.NewCIMDClient()
	// The clock is read through a so that tests can replace a.Now later.
	now := time.Now
	var st *store.Store
	if a != nil {
		now = func() time.Time { return a.Now() }
		st = a.Store
	}
	box := secretbox.New(c.Secret)
	if tokens == nil {
		tokens = &aicred.Tokens{Store: st, Box: box, OpenAI: openai.New(openai.HostID(c.Secret)), Now: now}
	}
	var oidc accessClient
	if c.Access != nil {
		oidc = access.New(access.ClientConfig{Issuer: c.Access.Issuer, ClientID: c.Access.ClientID, ClientSecret: c.Access.Secret, RedirectURI: c.Access.RedirectURI})
	}
	aiClient := ai.New()
	if c.AIAllowPrivateEndpoints {
		aiClient.AllowPrivateEndpoints = true
		aiClient.Endpoints = ai.NewEndpointClient(true)
	}
	return &Server{cfg: c, auth: a, assets: assets, accessOIDC: oidc, limiter: newLoginLimiter(), accountLimiter: newLoginLimiter(), registerLimiter: newLoginLimiter(), cimdLimiter: newLoginLimiter(),
		fetchCIMD: func(ctx context.Context, clientID string) (oauth.Document, error) {
			return oauth.FetchCIMD(ctx, cimd, clientID)
		},
		github: github.New(now), slack: slack.New(now), chatwork: chatwork.New(now), google: google.New(now), ai: aiClient, box: box, vapid: &notify.VAPID{Store: st, Box: box, Now: now}, tokens: tokens, pushTestLimiter: newKeyLimiter(3, time.Minute), aiTryLimiter: newKeyLimiter(5, time.Minute), aiEndpointLimiter: newKeyLimiter(10, time.Minute),
		chatworkMCPLimiter: newKeyLimiter(100, 5*time.Minute), slackSearchLimiter: newKeyLimiter(5, time.Minute), slackConversationsLimiter: newKeyLimiter(10, time.Minute),
		slackHistoryLimiter: newKeyLimiter(4, time.Minute), slackUsersLimiter: newKeyLimiter(30, time.Minute),
		gmailMCPLimiter: newKeyLimiter(gmailMCPUnits, time.Minute), gmailTokens: &gmailTokenCache{}}
}

// handler returns the routes wrapped with the common response headers.
//
// Patterns carry no method so that method mismatches go through allow and
// produce the JSON 405 response, and the "/" catch-all produces a JSON 404
// instead of the ServeMux text responses.
func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/healthz", allow(http.MethodGet, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, map[string]string{"status": "ok"})
	})))
	// With Cloudflare Access the password endpoints answer 409 (passwordAuth)
	// and the Access sign-in routes exist; without it they are JSON 404s.
	mux.Handle("/auth/config", allow(http.MethodGet, http.HandlerFunc(s.handleAuthConfig)))
	mux.Handle("/auth/login", allow(http.MethodPost, s.passwordAuth(s.browserJSON(http.HandlerFunc(s.handleLogin)))))
	if s.cfg.Access != nil {
		// No rate limit: behind a proxy every user shares its address, and
		// the sign-in itself happens at Cloudflare.
		mux.Handle("/auth/access/login", allow(http.MethodGet, http.HandlerFunc(s.handleAccessLogin)))
		mux.Handle("/auth/access/callback", allow(http.MethodGet, http.HandlerFunc(s.handleAccessCallback)))
	}
	mux.Handle("/auth/refresh", allow(http.MethodPost, s.browserJSON(http.HandlerFunc(s.handleRefresh))))
	mux.Handle("/auth/logout", allow(http.MethodPost, s.browserJSON(http.HandlerFunc(s.handleLogout))))
	// The invitation and password reset links are used before signing in.
	// They share accountLimiter.
	mux.Handle("/auth/invitations/inspect", allow(http.MethodPost, s.passwordAuth(s.browserJSON(http.HandlerFunc(s.handleInspectInvitation)))))
	mux.Handle("/auth/invitations/accept", allow(http.MethodPost, s.passwordAuth(s.browserJSON(http.HandlerFunc(s.handleAcceptInvitation)))))
	mux.Handle("/auth/password-resets/inspect", allow(http.MethodPost, s.passwordAuth(s.browserJSON(http.HandlerFunc(s.handleInspectPasswordReset)))))
	mux.Handle("/auth/password-resets/complete", allow(http.MethodPost, s.passwordAuth(s.browserJSON(http.HandlerFunc(s.handleCompletePasswordReset)))))
	// Every /api/v1/* route must be wrapped with either requireUser (access
	// JWT only) or requireUserOrAPIToken (access JWT or API token). Routes that
	// manage credentials or the account use requireUser. The user management
	// routes use requireOwner, which is requireUser plus the owner role.
	mux.Handle("/api/v1/me", allow(http.MethodGet, s.requireUser(http.HandlerFunc(s.handleMe))))
	mux.Handle("/api/v1/users", allow(http.MethodGet, s.requireOwner(http.HandlerFunc(s.handleListUsers))))
	mux.Handle("/api/v1/users/{id}", allow(http.MethodPatch, s.requireOwner(requireJSON(http.HandlerFunc(s.handlePatchUser)))))
	// The password reset link takes no body, so no content type is required.
	// Issuing links is password sign-in only; listing and revoking the
	// invitations is not, so that old ones can be cleaned up.
	mux.Handle("/api/v1/users/{id}/password-resets", allow(http.MethodPost, s.requireOwner(s.passwordAuth(http.HandlerFunc(s.handleCreatePasswordReset)))))
	mux.Handle("/api/v1/invitations", allowMethods(map[string]http.Handler{
		http.MethodGet:  s.requireOwner(http.HandlerFunc(s.handleListInvitations)),
		http.MethodPost: s.requireOwner(s.passwordAuth(requireJSON(http.HandlerFunc(s.handleCreateInvitation)))),
	}))
	mux.Handle("/api/v1/invitations/{id}", allow(http.MethodDelete, s.requireOwner(http.HandlerFunc(s.handleDeleteInvitation))))
	mux.Handle("/api/v1/action-items", allowMethods(map[string]http.Handler{
		http.MethodGet:  s.requireUserOrAPIToken(http.HandlerFunc(s.handleListActionItems)),
		http.MethodPost: s.requireUserOrAPIToken(requireJSON(http.HandlerFunc(s.handleCreateActionItem))),
	}))
	mux.Handle("/api/v1/action-items/{id}", allowMethods(map[string]http.Handler{
		http.MethodGet:   s.requireUserOrAPIToken(http.HandlerFunc(s.handleGetActionItem)),
		http.MethodPatch: s.requireUserOrAPIToken(requireJSON(http.HandlerFunc(s.handleUpdateActionItem))),
	}))
	mux.Handle("/api/v1/api-tokens", allowMethods(map[string]http.Handler{
		http.MethodGet:  s.requireUser(http.HandlerFunc(s.handleListAPITokens)),
		http.MethodPost: s.requireUser(requireJSON(http.HandlerFunc(s.handleCreateAPIToken))),
	}))
	mux.Handle("/api/v1/api-tokens/{id}", allow(http.MethodDelete, s.requireUser(http.HandlerFunc(s.handleDeleteAPIToken))))
	mux.Handle("/api/v1/oauth/consent", allowMethods(map[string]http.Handler{
		http.MethodGet:  s.requireUser(http.HandlerFunc(s.handleGetConsent)),
		http.MethodPost: s.requireUser(requireJSON(http.HandlerFunc(s.handlePostConsent))),
	}))
	mux.Handle("/api/v1/integrations/github", allowMethods(map[string]http.Handler{
		http.MethodGet:    s.requireUser(http.HandlerFunc(s.handleGetGitHub)),
		http.MethodPut:    s.requireUser(requireJSON(http.HandlerFunc(s.handlePutGitHub))),
		http.MethodPatch:  s.requireUser(requireJSON(http.HandlerFunc(s.handlePatchGitHub))),
		http.MethodDelete: s.requireUser(http.HandlerFunc(s.handleDeleteGitHub)),
	}))
	mux.Handle("/api/v1/integrations/slack", allowMethods(map[string]http.Handler{
		http.MethodGet:    s.requireUser(http.HandlerFunc(s.handleGetSlack)),
		http.MethodPut:    s.requireUser(requireJSON(http.HandlerFunc(s.handlePutSlack))),
		http.MethodPatch:  s.requireUser(requireJSON(http.HandlerFunc(s.handlePatchSlack))),
		http.MethodDelete: s.requireUser(http.HandlerFunc(s.handleDeleteSlack)),
	}))
	mux.Handle("/api/v1/integrations/chatwork", allowMethods(map[string]http.Handler{
		http.MethodGet:    s.requireUser(http.HandlerFunc(s.handleGetChatwork)),
		http.MethodPut:    s.requireUser(requireJSON(http.HandlerFunc(s.handlePutChatwork))),
		http.MethodPatch:  s.requireUser(requireJSON(http.HandlerFunc(s.handlePatchChatwork))),
		http.MethodDelete: s.requireUser(http.HandlerFunc(s.handleDeleteChatwork)),
	}))
	mux.Handle("/api/v1/integrations/gmail", allow(http.MethodGet, s.requireUser(http.HandlerFunc(s.handleGetGmail))))
	mux.Handle("/api/v1/integrations/gmail/authorize", allow(http.MethodPost, s.requireUser(requireJSON(http.HandlerFunc(s.handleAuthorizeGmail)))))
	mux.Handle("/api/v1/integrations/gmail/complete", allow(http.MethodPost, s.requireUser(requireJSON(http.HandlerFunc(s.handleCompleteGmail)))))
	mux.Handle("/api/v1/integrations/gmail/accounts/{id}", allowMethods(map[string]http.Handler{
		http.MethodPatch:  s.requireUser(requireJSON(http.HandlerFunc(s.handlePatchGmailAccount))),
		http.MethodDelete: s.requireUser(http.HandlerFunc(s.handleDeleteGmailAccount)),
	}))
	mux.Handle("/api/v1/ai", allowMethods(map[string]http.Handler{
		http.MethodGet:    s.requireUser(http.HandlerFunc(s.handleGetAI)),
		http.MethodPut:    s.requireUser(requireJSON(http.HandlerFunc(s.handlePutAI))),
		http.MethodDelete: s.requireUser(http.HandlerFunc(s.handleDeleteAI)),
	}))
	mux.Handle("/api/v1/ai/instructions", allowMethods(map[string]http.Handler{
		http.MethodGet: s.requireUser(http.HandlerFunc(s.handleGetAIInstructions)),
		http.MethodPut: s.requireUser(requireJSON(http.HandlerFunc(s.handlePutAIInstructions))),
	}))
	mux.Handle("/api/v1/ai/instructions/samples", allow(http.MethodGet, s.requireUser(http.HandlerFunc(s.handleAIInstructionSamples))))
	mux.Handle("/api/v1/ai/instructions/try", allow(http.MethodPost, s.requireUser(requireJSON(http.HandlerFunc(s.handleTryAIInstructions)))))
	mux.Handle("/api/v1/ai/openai/authorize", allow(http.MethodPost, s.requireUser(requireJSON(http.HandlerFunc(s.handleAuthorizeOpenAI)))))
	mux.Handle("/api/v1/ai/openai/complete", allow(http.MethodPost, s.requireUser(requireJSON(http.HandlerFunc(s.handleCompleteOpenAI)))))
	mux.Handle("/api/v1/ai/openai/models", allow(http.MethodGet, s.requireUser(http.HandlerFunc(s.handleOpenAIModels))))
	mux.Handle("/api/v1/push/public-key", allow(http.MethodGet, s.requireUser(http.HandlerFunc(s.handlePushPublicKey))))
	mux.Handle("/api/v1/push/subscriptions", allowMethods(map[string]http.Handler{
		http.MethodGet:  s.requireUser(http.HandlerFunc(s.handleListPushSubscriptions)),
		http.MethodPost: s.requireUser(requireJSON(http.HandlerFunc(s.handleCreatePushSubscription))),
	}))
	mux.Handle("/api/v1/push/subscriptions/{id}", allow(http.MethodDelete, s.requireUser(http.HandlerFunc(s.handleDeletePushSubscription))))
	mux.Handle("/api/v1/push/test", allow(http.MethodPost, s.requireUser(http.HandlerFunc(s.handlePushTest))))
	mux.Handle("/api/v1/notification-settings", allowMethods(map[string]http.Handler{
		http.MethodGet: s.requireUser(http.HandlerFunc(s.handleGetNotificationSettings)),
		http.MethodPut: s.requireUser(requireJSON(http.HandlerFunc(s.handlePutNotificationSettings))),
	}))
	mux.Handle("/api/v1/oauth/connections", allow(http.MethodGet, s.requireUser(http.HandlerFunc(s.handleListConnections))))
	mux.Handle("/api/v1/oauth/connections/{id}", allow(http.MethodDelete, s.requireUser(http.HandlerFunc(s.handleDeleteConnection))))
	// OAuth authorization server and protected resource metadata. Errors
	// raised inside the /oauth/* handlers use the RFC 6749 format.
	mux.Handle("/.well-known/oauth-protected-resource", allow(http.MethodGet, http.HandlerFunc(s.handleProtectedResourceMetadata)))
	mux.Handle("/.well-known/oauth-protected-resource/mcp", allow(http.MethodGet, http.HandlerFunc(s.handleProtectedResourceMetadata)))
	mux.Handle("/.well-known/oauth-authorization-server", allow(http.MethodGet, http.HandlerFunc(s.handleAuthorizationServerMetadata)))
	mux.Handle("/oauth/register", allow(http.MethodPost, http.HandlerFunc(s.handleRegister)))
	mux.Handle("/oauth/authorize", allow(http.MethodGet, http.HandlerFunc(s.handleAuthorize)))
	mux.Handle("/oauth/token", allow(http.MethodPost, http.HandlerFunc(s.handleToken)))
	// /mcp authenticates with API tokens and OAuth access tokens; see mcpAuth.
	mux.Handle("/mcp", allow(http.MethodPost, s.mcpAuth(s.mcpHandler())))
	// The screens, the static files and the legacy /admin URLs are routed
	// inside the catch-all so that unknown paths stay JSON 404s.
	mux.HandleFunc("/", s.web)
	return secureHeaders(mux)
}

func secureHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'self'; form-action 'self'")
		if strings.HasPrefix(r.URL.Path, "/auth/") || strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/oauth/") || r.URL.Path == "/mcp" {
			w.Header().Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

// allow rejects requests whose method is not method with a JSON 405.
func allow(method string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != method {
			w.Header().Set("Allow", method)
			fail(w, 405, "method_not_allowed", "Method not allowed.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// allowMethods dispatches by method and rejects other methods with a JSON 405
// listing the allowed methods.
func allowMethods(handlers map[string]http.Handler) http.Handler {
	methods := make([]string, 0, len(handlers))
	for m := range handlers {
		methods = append(methods, m)
	}
	sort.Strings(methods)
	allowed := strings.Join(methods, ", ")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next, ok := handlers[r.Method]
		if !ok {
			w.Header().Set("Allow", allowed)
			fail(w, 405, "method_not_allowed", "Method not allowed.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// browserJSON requires the configured Origin and a JSON content type.
func (s *Server) browserJSON(next http.Handler) http.Handler {
	jsonOnly := requireJSON(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") != s.cfg.PublicURL {
			fail(w, 403, "origin_rejected", "Request origin is not allowed.")
			return
		}
		jsonOnly.ServeHTTP(w, r)
	})
}

// requireJSON requires a JSON content type.
func requireJSON(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || mt != "application/json" {
			fail(w, 415, "unsupported_media_type", "Use application/json.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

type userKey struct{}
type sessionKey struct{}

// bearerToken returns the token of an "Authorization: Bearer" header.
func bearerToken(r *http.Request) (string, bool) {
	v := r.Header.Get("Authorization")
	token := strings.TrimPrefix(v, "Bearer ")
	if !strings.HasPrefix(v, "Bearer ") || token == "" {
		return "", false
	}
	return token, true
}

// withSession stores the authenticated user and, for access JWTs, the
// verified session ID in the request context.
func withSession(r *http.Request, u store.User, sid string) *http.Request {
	ctx := context.WithValue(r.Context(), userKey{}, u)
	if sid != "" {
		ctx = context.WithValue(ctx, sessionKey{}, sid)
	}
	return r.WithContext(ctx)
}

// requireUser authenticates the Bearer access token and stores the user and
// session ID in the request context for userFrom and sessionFrom. API tokens
// are rejected.
func (s *Server) requireUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearerToken(r)
		if !ok {
			s.authError(w, r, store.ErrUnauthorized)
			return
		}
		u, sid, err := s.auth.AuthenticateSession(r.Context(), token)
		if err != nil {
			s.authError(w, r, err)
			return
		}
		next.ServeHTTP(w, withSession(r, u, sid))
	})
}

// requireUserOrAPIToken accepts an API token (a Bearer value starting with
// "ast_") as well as an access JWT. Only the access JWT path stores a session
// ID.
func (s *Server) requireUserOrAPIToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearerToken(r)
		if !ok {
			s.authError(w, r, store.ErrUnauthorized)
			return
		}
		var u store.User
		var sid string
		var err error
		if strings.HasPrefix(token, "ast_") {
			u, err = s.auth.AuthenticateAPIToken(r.Context(), token)
		} else {
			u, sid, err = s.auth.AuthenticateSession(r.Context(), token)
		}
		if err != nil {
			s.authError(w, r, err)
			return
		}
		next.ServeHTTP(w, withSession(r, u, sid))
	})
}

func userFrom(ctx context.Context) (store.User, bool) {
	u, ok := ctx.Value(userKey{}).(store.User)
	return u, ok
}

// sessionFrom returns the session ID of the access JWT that authenticated the
// request. It is absent for API tokens.
func sessionFrom(ctx context.Context) (string, bool) {
	sid, ok := ctx.Value(sessionKey{}).(string)
	return sid, ok && sid != ""
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	u, ok := userFrom(r.Context())
	if !ok {
		s.authError(w, r, store.ErrUnauthorized)
		return
	}
	writeJSON(w, 200, s.toUserResponse(r.Context(), u))
}

// decode reads a single JSON object of at most limit bytes into out.
func decode(w http.ResponseWriter, r *http.Request, out any, limit int64) bool {
	body, e := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if e != nil {
		var large *http.MaxBytesError
		if errors.As(e, &large) {
			fail(w, 413, "body_too_large", "Request body is too large.")
		} else {
			fail(w, 400, "invalid_request", "Invalid JSON body.")
		}
		return false
	}
	body = bytes.TrimSpace(body)
	if len(body) == 0 || body[0] != '{' {
		fail(w, 400, "invalid_request", "A JSON object is required.")
		return false
	}
	d := json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	if e = d.Decode(out); e != nil {
		fail(w, 400, "invalid_request", "Invalid JSON body.")
		return false
	}
	var extra any
	if e = d.Decode(&extra); e != io.EOF {
		fail(w, 400, "invalid_request", "Only one JSON object is allowed.")
		return false
	}
	return true
}
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !s.limiter.allow(r.RemoteAddr) {
		w.Header().Set("Retry-After", "60")
		fail(w, 429, "rate_limited", "Too many attempts. Try again later.")
		return
	}
	var in struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if !decode(w, r, &in, 4096) {
		return
	}
	if len(in.Email) > 256 || len(in.Password) > 72 {
		fail(w, 400, "invalid_request", "Credentials exceed the supported length.")
		return
	}
	result, err := s.auth.Login(r.Context(), in.Email, in.Password)
	if err != nil {
		s.authError(w, r, err)
		return
	}
	s.writeAuthResult(w, r, result)
}
func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	var empty struct{}
	if !decode(w, r, &empty, 4096) {
		return
	}
	result, err := s.auth.Refresh(r.Context(), refreshCookie(r))
	if err != nil {
		s.authError(w, r, err)
		return
	}
	s.writeAuthResult(w, r, result)
}
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	var empty struct{}
	if !decode(w, r, &empty, 4096) {
		return
	}
	if err := s.auth.Logout(r.Context(), refreshCookie(r)); err != nil {
		s.authError(w, r, err)
		return
	}
	s.clearCookie(w)
	w.WriteHeader(204)
}
func refreshCookie(r *http.Request) string {
	if c, err := r.Cookie(CookieName); err == nil {
		return c.Value
	}
	return ""
}
func (s *Server) writeAuthResult(w http.ResponseWriter, r *http.Request, result auth.Result) {
	user := s.toUserResponse(r.Context(), result.User)
	s.setRefreshCookie(w, result)
	writeJSON(w, 200, authResponse{AccessToken: result.AccessToken, ExpiresIn: result.ExpiresIn, User: user})
}

// setRefreshCookie sets the refresh cookie of a login or refresh result.
func (s *Server) setRefreshCookie(w http.ResponseWriter, result auth.Result) {
	http.SetCookie(w, &http.Cookie{Name: CookieName, Value: result.Refresh, Path: "/auth", HttpOnly: true, Secure: s.cfg.Secure, SameSite: http.SameSiteStrictMode, Expires: time.Unix(result.ExpiresAt, 0), MaxAge: result.MaxAge})
}
func (s *Server) clearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: CookieName, Value: "", Path: "/auth", HttpOnly: true, Secure: s.cfg.Secure, SameSite: http.SameSiteStrictMode, Expires: time.Unix(0, 0), MaxAge: -1})
}

// authError writes the authentication failure response. Errors other than
// ErrUnauthorized are logged without tokens, passwords or cookie values.
func (s *Server) authError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, store.ErrUnauthorized) {
		fail(w, 401, "unauthorized", "Authentication is required or credentials are invalid.")
		return
	}
	log.Printf("auth: %s %s: %v", r.Method, r.URL.Path, err)
	fail(w, 503, "unavailable", "Authentication is temporarily unavailable. Try again.")
}

// pageName matches a settings page name such as "notifications".
var pageName = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)

// web serves the paths not matched by an API route: the screens rendered by
// index.html, the embedded static files and the legacy /admin URLs. Anything
// else is a JSON 404.
func (s *Server) web(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	if screenPath(p) {
		if readOnly(w, r) {
			s.serveAsset(w, "index.html")
		}
		return
	}
	if name, ok := s.staticName(p); ok {
		if readOnly(w, r) {
			s.serveAsset(w, name)
		}
		return
	}
	if p == "/admin" || strings.HasPrefix(p, "/admin/") {
		if !readOnly(w, r) {
			return
		}
		if p == "/admin/sw.js" {
			// Service workers registered with the /admin/ scope update from
			// this URL, so it serves the current worker instead of redirecting.
			s.serveAsset(w, "sw.js")
			return
		}
		http.Redirect(w, r, s.legacyLocation(r), http.StatusPermanentRedirect)
		return
	}
	fail(w, 404, "not_found", "Not found.")
}

// readOnly rejects methods other than GET and HEAD with a JSON 405.
func readOnly(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return true
	}
	w.Header().Set("Allow", "GET, HEAD")
	fail(w, 405, "method_not_allowed", "Method not allowed.")
	return false
}

// screenPath reports whether p is a screen of the web UI: /, /candidates,
// /auto, /settings, /settings/<page>, /tasks/<id>, /consent, /invite,
// /reset-password, /signin-error or the Google callback, optionally with a trailing
// slash. The tokens of /invite and /reset-password are in the URL fragment,
// which never reaches here. The query of the Google callback (the code and
// the state) does reach here; it is never logged, and the page removes it
// from the address bar at once.
func screenPath(p string) bool {
	if p == "/" {
		return true
	}
	p = strings.TrimSuffix(p, "/")
	switch p {
	case "/candidates", "/auto", "/settings", "/account", "/consent", "/invite", "/reset-password", signinErrorPath, config.GoogleCallbackPath:
		return true
	}
	if id, ok := strings.CutPrefix(p, "/tasks/"); ok {
		return itemIDPattern.MatchString(id)
	}
	if page, ok := strings.CutPrefix(p, "/account/"); ok {
		return pageName.MatchString(page)
	}
	page, ok := strings.CutPrefix(p, "/settings/")
	return ok && pageName.MatchString(page)
}

// staticName returns the embedded file served at p: a file under assets/ or
// a file with an extension at the root, other than index.html and dot files.
func (s *Server) staticName(p string) (string, bool) {
	name, ok := strings.CutPrefix(p, "/")
	if !ok || name == "index.html" || !fs.ValidPath(name) {
		return "", false
	}
	for _, c := range name {
		if c < 0x20 || c == 0x7f || c == '\\' {
			return "", false
		}
	}
	for _, elem := range strings.Split(name, "/") {
		if strings.HasPrefix(elem, ".") {
			return "", false
		}
	}
	if !strings.HasPrefix(name, "assets/") && (strings.Contains(name, "/") || path.Ext(name) == "") {
		return "", false
	}
	info, err := fs.Stat(s.assets, name)
	if err != nil || info.IsDir() {
		return "", false
	}
	return name, true
}

// legacyLocation returns where a legacy /admin URL moved. The location is
// built only from fixed paths and validated values so that it is always a
// same-origin path; anything unknown goes to /.
func (s *Server) legacyLocation(r *http.Request) string {
	rest := strings.TrimPrefix(r.URL.Path, "/admin")
	switch strings.TrimSuffix(rest, "/") {
	case "":
		return "/"
	case "/auto":
		return "/auto"
	case "/settings":
		return "/settings"
	case "/oauth/authorize":
		if v := r.URL.Query().Get("request"); v != "" {
			return "/consent?request=" + url.QueryEscape(v)
		}
		return "/consent"
	}
	if page, ok := strings.CutPrefix(strings.TrimSuffix(rest, "/"), "/settings/"); ok && pageName.MatchString(page) {
		return "/settings/" + page
	}
	if name, ok := s.staticName(rest); ok {
		return "/" + name
	}
	return "/"
}

// serveAsset writes the embedded file name, or a JSON 404 when it is missing.
func (s *Server) serveAsset(w http.ResponseWriter, name string) {
	data, e := fs.ReadFile(s.assets, name)
	if e != nil {
		fail(w, 404, "not_found", "Not found.")
		return
	}
	mt := mime.TypeByExtension(path.Ext(name))
	if name == "manifest.webmanifest" {
		mt = "application/manifest+json"
	}
	if mt == "" {
		mt = "application/octet-stream"
	}
	w.Header().Set("Content-Type", mt)
	switch name {
	case "index.html":
		w.Header().Set("Cache-Control", "no-store")
	case "sw.js", "manifest.webmanifest":
		// The service worker and the manifest keep their names across
		// releases, so they must be revalidated.
		w.Header().Set("Cache-Control", "no-cache")
	default:
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(200)
	_, _ = w.Write(data)
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}
