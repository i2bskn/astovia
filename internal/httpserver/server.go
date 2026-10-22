package httpserver

import (
	"bytes"
	"encoding/json"
	"errors"
	"github.com/i2bskn/astovia/internal/auth"
	"github.com/i2bskn/astovia/internal/config"
	"github.com/i2bskn/astovia/internal/store"
	"io"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
)

const CookieName = "astovia_refresh"

type Server struct {
	cfg      config.Config
	auth     *auth.Service
	assets   fs.FS
	mu       sync.Mutex
	attempts map[string]bucket
}
type bucket struct {
	start time.Time
	count int
}

func New(c config.Config, a *auth.Service, assets fs.FS) http.Handler {
	s := &Server{cfg: c, auth: a, assets: assets, attempts: make(map[string]bucket)}
	return s
}
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "same-origin")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'self'; form-action 'self'")
	if strings.HasPrefix(r.URL.Path, "/auth/") || strings.HasPrefix(r.URL.Path, "/api/") {
		w.Header().Set("Cache-Control", "no-store")
	}
	switch r.URL.Path {
	case "/":
		if !method(w, r, "GET") {
			return
		}
		http.Redirect(w, r, "/admin/", http.StatusFound)
	case "/healthz":
		if !method(w, r, "GET") {
			return
		}
		writeJSON(w, 200, map[string]string{"status": "ok"})
	case "/auth/login", "/auth/refresh", "/auth/logout":
		if !method(w, r, "POST") {
			return
		}
		if r.Header.Get("Origin") != s.cfg.PublicURL {
			fail(w, 403, "origin_rejected", "Request origin is not allowed.")
			return
		}
		mt, _, e := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if e != nil || mt != "application/json" {
			fail(w, 415, "unsupported_media_type", "Use application/json.")
			return
		}
		s.authentication(w, r)
	case "/api/v1/me":
		if !method(w, r, "GET") {
			return
		}
		v := r.Header.Get("Authorization")
		if !strings.HasPrefix(v, "Bearer ") {
			s.authError(w, store.ErrUnauthorized)
			return
		}
		u, e := s.auth.Authenticate(r.Context(), strings.TrimPrefix(v, "Bearer "))
		if e != nil {
			s.authError(w, e)
			return
		}
		writeJSON(w, 200, u)
	case "/admin":
		if !method(w, r, "GET") {
			return
		}
		http.Redirect(w, r, "/admin/", http.StatusMovedPermanently)
	default:
		if strings.HasPrefix(r.URL.Path, "/admin/") {
			if !method(w, r, "GET") {
				return
			}
			s.static(w, r)
			return
		}
		fail(w, 404, "not_found", "Not found.")
	}
}
func method(w http.ResponseWriter, r *http.Request, want string) bool {
	if r.Method == want {
		return true
	}
	w.Header().Set("Allow", want)
	fail(w, 405, "method_not_allowed", "Method not allowed.")
	return false
}
func decode(w http.ResponseWriter, r *http.Request, out any) bool {
	body, e := io.ReadAll(http.MaxBytesReader(w, r.Body, 4096))
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
func (s *Server) authentication(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/auth/login" {
		if !s.allowed(r.RemoteAddr) {
			w.Header().Set("Retry-After", "60")
			fail(w, 429, "rate_limited", "Too many attempts. Try again later.")
			return
		}
		var in struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		if !decode(w, r, &in) {
			return
		}
		if len(in.Username) > 256 || len(in.Password) > 72 {
			fail(w, 400, "invalid_request", "Credentials exceed the supported length.")
			return
		}
		result, e := s.auth.Login(r.Context(), in.Username, in.Password)
		if e != nil {
			s.authError(w, e)
			return
		}
		s.cookie(w, result.Refresh, result.ExpiresAt, false)
		writeJSON(w, 200, result)
		return
	}
	var empty struct{}
	if !decode(w, r, &empty) {
		return
	}
	raw := ""
	if c, e := r.Cookie(CookieName); e == nil {
		raw = c.Value
	}
	if r.URL.Path == "/auth/logout" {
		if e := s.auth.Logout(r.Context(), raw); e != nil {
			s.authError(w, e)
			return
		}
		s.cookie(w, "", 0, true)
		w.WriteHeader(204)
		return
	}
	result, e := s.auth.Refresh(r.Context(), raw)
	if e != nil {
		s.authError(w, e)
		return
	}
	s.cookie(w, result.Refresh, result.ExpiresAt, false)
	writeJSON(w, 200, result)
}
func (s *Server) cookie(w http.ResponseWriter, v string, expires int64, clear bool) {
	c := &http.Cookie{Name: CookieName, Value: v, Path: "/auth", HttpOnly: true, Secure: s.cfg.Secure, SameSite: http.SameSiteStrictMode, Expires: time.Unix(expires, 0)}
	if clear {
		c.MaxAge = -1
	} else {
		c.MaxAge = int(expires - s.auth.Now().Unix())
	}
	http.SetCookie(w, c)
}
func (s *Server) authError(w http.ResponseWriter, e error) {
	if errors.Is(e, store.ErrUnauthorized) {
		fail(w, 401, "unauthorized", "Authentication is required or credentials are invalid.")
	} else {
		fail(w, 503, "unavailable", "Authentication is temporarily unavailable. Try again.")
	}
}
func (s *Server) allowed(remote string) bool {
	host, _, e := net.SplitHostPort(remote)
	if e != nil {
		host = remote
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, b := range s.attempts {
		if now.Sub(b.start) >= time.Minute {
			delete(s.attempts, k)
		}
	}
	b, ok := s.attempts[host]
	if !ok {
		if len(s.attempts) >= 4096 {
			return false
		}
		b = bucket{start: now}
	}
	if b.count >= 10 {
		return false
	}
	b.count++
	s.attempts[host] = b
	return true
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
