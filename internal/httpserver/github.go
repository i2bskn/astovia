package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/i2bskn/astovia/internal/github"
	"github.com/i2bskn/astovia/internal/githubsync"
	"github.com/i2bskn/astovia/internal/store"
)

// integrationBodyLimit bounds the bodies of the integration and
// notification endpoints.
const integrationBodyLimit = 16384

// githubTimeout bounds the token check against GitHub.
const githubTimeout = 10 * time.Second

// githubResponse is a connected GitHub integration. The token is never
// included; token_last4 identifies it.
type githubResponse struct {
	Connected       bool     `json:"connected"`
	Login           string   `json:"login"`
	TokenLast4      string   `json:"token_last4"`
	Scopes          []string `json:"scopes"`
	ImmediateNotify bool     `json:"immediate_notify"`
	Status          string   `json:"status"`
	LastError       *string  `json:"last_error"`
	LastSyncedAt    *string  `json:"last_synced_at"`
	CreatedAt       string   `json:"created_at"`
}

type githubDisconnectedResponse struct {
	Connected bool `json:"connected"`
}

func toGitHubResponse(c store.GitHubConnection) githubResponse {
	out := githubResponse{Connected: true, Login: c.Login, TokenLast4: c.TokenLast4, Scopes: github.ParseScopes(c.Scopes), ImmediateNotify: c.ImmediateNotify == 1, Status: c.Status, CreatedAt: formatTime(c.CreatedAt)}
	if c.LastError.Valid {
		out.LastError = &c.LastError.String
	}
	if c.LastSyncedAt.Valid {
		v := formatTime(c.LastSyncedAt.Int64)
		out.LastSyncedAt = &v
	}
	return out
}

// integrationError writes the response for an unexpected error. The log
// never contains tokens, request bodies or push endpoints.
func integrationError(w http.ResponseWriter, op, userID string, err error) {
	log.Printf("integrations: %s user=%s: %v", op, userID, err)
	fail(w, 503, "unavailable", "The service is temporarily unavailable. Try again.")
}

// validGitHubToken accepts 1-255 bytes of printable ASCII without spaces.
func validGitHubToken(v string) bool {
	if len(v) < 1 || len(v) > 255 {
		return false
	}
	for i := 0; i < len(v); i++ {
		if v[i] < 0x21 || v[i] > 0x7e {
			return false
		}
	}
	return true
}

// tokenLast4 returns the last four characters of a token, or nothing for a
// token so short that they would reveal most of it.
func tokenLast4(v string) string {
	if len(v) <= 8 {
		return ""
	}
	return v[len(v)-4:]
}

// decodeBool decodes a JSON boolean (null is rejected).
func decodeBool(raw json.RawMessage) (bool, bool) {
	var v bool
	if isNull(raw) || !decodeValue(raw, &v) {
		return false, false
	}
	return v, true
}

func (s *Server) handleGetGitHub(w http.ResponseWriter, r *http.Request) {
	u, ok := userFrom(r.Context())
	if !ok {
		s.authError(w, r, store.ErrUnauthorized)
		return
	}
	c, err := s.auth.Store.GitHubConnection(r.Context(), u.ID)
	if errors.Is(err, store.ErrConnectionNotFound) {
		writeJSON(w, 200, githubDisconnectedResponse{})
		return
	}
	if err != nil {
		integrationError(w, "github get", u.ID, err)
		return
	}
	writeJSON(w, 200, toGitHubResponse(c))
}

func (s *Server) handlePutGitHub(w http.ResponseWriter, r *http.Request) {
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
		Token           json.RawMessage `json:"token"`
		ImmediateNotify json.RawMessage `json:"immediate_notify"`
	}
	if !decode(w, r, &in, integrationBodyLimit) {
		return
	}
	var token string
	if in.Token != nil {
		token, ok = decodeString(in.Token)
	}
	if in.Token == nil || !ok || !validGitHubToken(token) {
		fail(w, 400, "invalid_request", "token is invalid.")
		return
	}
	var immediate *bool
	if in.ImmediateNotify != nil {
		v, ok := decodeBool(in.ImmediateNotify)
		if !ok {
			fail(w, 400, "invalid_request", "immediate_notify must be a boolean.")
			return
		}
		immediate = &v
	}
	// GitHub is called before, and outside, the store transaction.
	ctx, cancel := context.WithTimeout(r.Context(), githubTimeout)
	login, scopes, err := s.github.User(ctx, token)
	cancel()
	switch {
	case err == nil:
	case errors.Is(err, github.ErrUnauthorized), errors.Is(err, github.ErrForbidden):
		fail(w, 400, "github_token_invalid", "The GitHub token is invalid or expired.")
		return
	default:
		log.Printf("integrations: github verify user=%s: %v", u.ID, err)
		fail(w, 503, "github_unavailable", "GitHub could not be reached. Try again later.")
		return
	}
	if !github.ValidName(login) {
		log.Printf("integrations: github verify user=%s: unexpected login", u.ID)
		fail(w, 503, "github_unavailable", "GitHub could not be reached. Try again later.")
		return
	}
	hasRepo := false
	for _, sc := range scopes {
		hasRepo = hasRepo || sc == "repo"
	}
	if !hasRepo {
		fail(w, 400, "github_scopes_missing", "The token is missing required scopes: repo.")
		return
	}
	sealed, err := s.box.Seal(githubsync.TokenPurpose(u.ID), token)
	if err != nil {
		integrationError(w, "github seal", u.ID, err)
		return
	}
	c, err := s.auth.Store.SaveGitHubConnection(r.Context(), store.GitHubConnection{UserID: u.ID, TokenSealed: sealed, TokenLast4: tokenLast4(token), Login: login, Scopes: strings.Join(scopes, ",")}, immediate, sid, s.auth.Now().Unix())
	if errors.Is(err, store.ErrUnauthorized) {
		s.authError(w, r, err)
		return
	}
	if err != nil {
		integrationError(w, "github save", u.ID, err)
		return
	}
	writeJSON(w, 200, toGitHubResponse(c))
}

func (s *Server) handlePatchGitHub(w http.ResponseWriter, r *http.Request) {
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
		ImmediateNotify json.RawMessage `json:"immediate_notify"`
	}
	if !decode(w, r, &in, integrationBodyLimit) {
		return
	}
	immediate, ok := decodeBool(in.ImmediateNotify)
	if in.ImmediateNotify == nil || !ok {
		fail(w, 400, "invalid_request", "immediate_notify must be a boolean.")
		return
	}
	c, err := s.auth.Store.SetGitHubImmediateNotify(r.Context(), u.ID, immediate, sid, s.auth.Now().Unix())
	switch {
	case err == nil:
	case errors.Is(err, store.ErrUnauthorized):
		s.authError(w, r, err)
		return
	case errors.Is(err, store.ErrConnectionNotFound):
		fail(w, 404, "not_found", "GitHub is not connected.")
		return
	default:
		integrationError(w, "github patch", u.ID, err)
		return
	}
	writeJSON(w, 200, toGitHubResponse(c))
}

func (s *Server) handleDeleteGitHub(w http.ResponseWriter, r *http.Request) {
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
	err := s.auth.Store.DeleteGitHubConnection(r.Context(), u.ID, sid, s.auth.Now().Unix())
	if errors.Is(err, store.ErrUnauthorized) {
		s.authError(w, r, err)
		return
	}
	if err != nil {
		integrationError(w, "github delete", u.ID, err)
		return
	}
	w.WriteHeader(204)
}
