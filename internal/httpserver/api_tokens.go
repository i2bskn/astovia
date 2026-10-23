package httpserver

import (
	"encoding/json"
	"errors"
	"github.com/i2bskn/astovia/internal/auth"
	"github.com/i2bskn/astovia/internal/store"
	"log"
	"net/http"
	"strings"
	"unicode/utf8"
)

const apiTokenBodyLimit = 4096

// apiTokenResponse is the public representation of an API token. The secret
// is never included; it is returned once, by createdAPITokenResponse.
type apiTokenResponse struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	CreatedAt  string  `json:"created_at"`
	LastUsedAt *string `json:"last_used_at"`
}

type apiTokenListResponse struct {
	Items []apiTokenResponse `json:"items"`
}

type createdAPITokenResponse struct {
	apiTokenResponse
	Token string `json:"token"`
}

func toAPITokenResponse(t store.APIToken) apiTokenResponse {
	out := apiTokenResponse{ID: t.ID, Name: t.Name, CreatedAt: formatTime(t.CreatedAt)}
	if t.LastUsedAt.Valid {
		v := formatTime(t.LastUsedAt.Int64)
		out.LastUsedAt = &v
	}
	return out
}

// apiTokenError writes the response for an unexpected store error. The log
// never contains token values.
func apiTokenError(w http.ResponseWriter, op, userID string, err error) {
	log.Printf("api tokens: %s user=%s: %v", op, userID, err)
	fail(w, 503, "unavailable", "The service is temporarily unavailable. Try again.")
}

func (s *Server) handleListAPITokens(w http.ResponseWriter, r *http.Request) {
	u, ok := userFrom(r.Context())
	if !ok {
		s.authError(w, r, store.ErrUnauthorized)
		return
	}
	tokens, err := s.auth.Store.ListAPITokens(r.Context(), u.ID)
	if err != nil {
		apiTokenError(w, "list", u.ID, err)
		return
	}
	out := apiTokenListResponse{Items: make([]apiTokenResponse, 0, len(tokens))}
	for _, t := range tokens {
		out.Items = append(out.Items, toAPITokenResponse(t))
	}
	writeJSON(w, 200, out)
}

func (s *Server) handleCreateAPIToken(w http.ResponseWriter, r *http.Request) {
	u, ok := userFrom(r.Context())
	if !ok {
		s.authError(w, r, store.ErrUnauthorized)
		return
	}
	// Tokens can only be issued from a browser session; the session is
	// checked again inside the store transaction.
	sid, ok := sessionFrom(r.Context())
	if !ok {
		s.authError(w, r, store.ErrUnauthorized)
		return
	}
	var in struct {
		Name json.RawMessage `json:"name"`
	}
	if !decode(w, r, &in, apiTokenBodyLimit) {
		return
	}
	var name string
	if in.Name != nil {
		name, ok = decodeString(in.Name)
		name = strings.TrimSpace(name)
	}
	if in.Name == nil || !ok || name == "" || utf8.RuneCountInString(name) > 100 {
		fail(w, 400, "invalid_request", "name must be 1-100 characters.")
		return
	}
	raw, hash, err := auth.NewAPIToken()
	if err != nil {
		apiTokenError(w, "create", u.ID, err)
		return
	}
	id, err := auth.Random()
	if err != nil {
		apiTokenError(w, "create", u.ID, err)
		return
	}
	t := store.APIToken{ID: id, UserID: u.ID, Name: name, CreatedAt: s.auth.Now().Unix()}
	err = s.auth.Store.CreateAPIToken(r.Context(), t, hash, sid, t.CreatedAt)
	switch {
	case err == nil:
	case errors.Is(err, store.ErrUnauthorized):
		s.authError(w, r, err)
		return
	case errors.Is(err, store.ErrTooManyTokens):
		fail(w, 409, "limit_reached", "You can have at most 50 API tokens. Delete one first.")
		return
	default:
		apiTokenError(w, "create", u.ID, err)
		return
	}
	writeJSON(w, 201, createdAPITokenResponse{apiTokenResponse: toAPITokenResponse(t), Token: raw})
}

func (s *Server) handleDeleteAPIToken(w http.ResponseWriter, r *http.Request) {
	u, ok := userFrom(r.Context())
	if !ok {
		s.authError(w, r, store.ErrUnauthorized)
		return
	}
	id := r.PathValue("id")
	if !itemIDPattern.MatchString(id) {
		fail(w, 404, "not_found", "API token not found.")
		return
	}
	err := s.auth.Store.DeleteAPIToken(r.Context(), u.ID, id)
	if errors.Is(err, store.ErrTokenNotFound) {
		fail(w, 404, "not_found", "API token not found.")
		return
	}
	if err != nil {
		apiTokenError(w, "delete", u.ID, err)
		return
	}
	w.WriteHeader(204)
}
