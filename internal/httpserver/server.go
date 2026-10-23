package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/i2bskn/astovia/internal/auth"
	"github.com/i2bskn/astovia/internal/config"
	"github.com/i2bskn/astovia/internal/store"
	"io"
	"io/fs"
	"log"
	"mime"
	"net/http"
	"path"
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
}

// userResponse is the public representation of a user.
type userResponse struct {
	ID    string `json:"id"`
	Email string `json:"email"`
}

// authResponse is returned by login and refresh.
type authResponse struct {
	AccessToken string       `json:"access_token"`
	ExpiresIn   int          `json:"expires_in"`
	User        userResponse `json:"user"`
}

func toUserResponse(u store.User) userResponse {
	return userResponse{ID: u.ID, Email: u.Email}
}

// New returns the application handler.
func New(c config.Config, a *auth.Service, assets fs.FS) http.Handler {
	return newServer(c, a, assets).handler()
}

// newServer builds a Server without wrapping it, so tests in this package can
// adjust the limiter before calling handler.
func newServer(c config.Config, a *auth.Service, assets fs.FS) *Server {
	return &Server{cfg: c, auth: a, assets: assets, limiter: newLoginLimiter()}
}

// handler returns the routes wrapped with the common response headers.
//
// Patterns carry no method so that method mismatches go through allow and
// produce the JSON 405 response, and the "/" catch-all produces a JSON 404
// instead of the ServeMux text responses.
func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/{$}", allow(http.MethodGet, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin/", http.StatusFound)
	})))
	mux.Handle("/healthz", allow(http.MethodGet, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, map[string]string{"status": "ok"})
	})))
	mux.Handle("/auth/login", allow(http.MethodPost, s.browserJSON(http.HandlerFunc(s.handleLogin))))
	mux.Handle("/auth/refresh", allow(http.MethodPost, s.browserJSON(http.HandlerFunc(s.handleRefresh))))
	mux.Handle("/auth/logout", allow(http.MethodPost, s.browserJSON(http.HandlerFunc(s.handleLogout))))
	// Every /api/v1/* route must be wrapped with requireUser.
	mux.Handle("/api/v1/me", allow(http.MethodGet, s.requireUser(http.HandlerFunc(s.handleMe))))
	mux.Handle("/api/v1/action-items", allowMethods(map[string]http.Handler{
		http.MethodGet:  s.requireUser(http.HandlerFunc(s.handleListActionItems)),
		http.MethodPost: s.requireUser(requireJSON(http.HandlerFunc(s.handleCreateActionItem))),
	}))
	mux.Handle("/api/v1/action-items/{id}", allowMethods(map[string]http.Handler{
		http.MethodGet:   s.requireUser(http.HandlerFunc(s.handleGetActionItem)),
		http.MethodPatch: s.requireUser(requireJSON(http.HandlerFunc(s.handleUpdateActionItem))),
	}))
	mux.Handle("/admin", allow(http.MethodGet, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin/", http.StatusMovedPermanently)
	})))
	mux.Handle("/admin/", allow(http.MethodGet, http.HandlerFunc(s.static)))
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		fail(w, 404, "not_found", "Not found.")
	})
	return secureHeaders(mux)
}

func secureHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'self'; form-action 'self'")
		if strings.HasPrefix(r.URL.Path, "/auth/") || strings.HasPrefix(r.URL.Path, "/api/") {
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

// requireUser authenticates the Bearer access token and stores the user in
// the request context for userFrom.
func (s *Server) requireUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v := r.Header.Get("Authorization")
		token := strings.TrimPrefix(v, "Bearer ")
		if !strings.HasPrefix(v, "Bearer ") || token == "" {
			s.authError(w, r, store.ErrUnauthorized)
			return
		}
		u, err := s.auth.Authenticate(r.Context(), token)
		if err != nil {
			s.authError(w, r, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userKey{}, u)))
	})
}

func userFrom(ctx context.Context) (store.User, bool) {
	u, ok := ctx.Value(userKey{}).(store.User)
	return u, ok
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	u, ok := userFrom(r.Context())
	if !ok {
		s.authError(w, r, store.ErrUnauthorized)
		return
	}
	writeJSON(w, 200, toUserResponse(u))
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
	s.writeAuthResult(w, result)
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
	s.writeAuthResult(w, result)
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
func (s *Server) writeAuthResult(w http.ResponseWriter, result auth.Result) {
	http.SetCookie(w, &http.Cookie{Name: CookieName, Value: result.Refresh, Path: "/auth", HttpOnly: true, Secure: s.cfg.Secure, SameSite: http.SameSiteStrictMode, Expires: time.Unix(result.ExpiresAt, 0), MaxAge: result.MaxAge})
	writeJSON(w, 200, authResponse{AccessToken: result.AccessToken, ExpiresIn: result.ExpiresIn, User: toUserResponse(result.User)})
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
func (s *Server) static(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/admin/")
	if name == "" {
		name = "index.html"
	}
	if !fs.ValidPath(name) {
		fail(w, 404, "not_found", "Not found.")
		return
	}
	data, e := fs.ReadFile(s.assets, name)
	if e != nil {
		if strings.HasPrefix(name, "assets/") || path.Ext(name) != "" {
			fail(w, 404, "not_found", "Not found.")
			return
		}
		data, e = fs.ReadFile(s.assets, "index.html")
		name = "index.html"
	}
	if e != nil {
		fail(w, 404, "not_found", "Not found.")
		return
	}
	mt := mime.TypeByExtension(path.Ext(name))
	if mt == "" {
		mt = "application/octet-stream"
	}
	w.Header().Set("Content-Type", mt)
	if name == "index.html" {
		w.Header().Set("Cache-Control", "no-store")
	} else {
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
