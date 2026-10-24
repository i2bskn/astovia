package httpserver

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/i2bskn/astovia/internal/ai"
	"github.com/i2bskn/astovia/internal/store"
)

// aiResponse is the user's AI settings. The key is never included;
// key_last4 identifies it.
type aiResponse struct {
	Configured bool    `json:"configured"`
	Provider   string  `json:"provider"`
	Model      string  `json:"model"`
	KeyLast4   string  `json:"key_last4"`
	Status     string  `json:"status"`
	LastError  *string `json:"last_error"`
	UpdatedAt  string  `json:"updated_at"`
}

type aiUnconfiguredResponse struct {
	Configured bool `json:"configured"`
}

func toAIResponse(a store.AISettings) aiResponse {
	out := aiResponse{Configured: true, Provider: a.Provider, Model: a.Model, KeyLast4: a.KeyLast4, Status: a.Status, UpdatedAt: formatTime(a.UpdatedAt)}
	if a.LastError.Valid {
		out.LastError = &a.LastError.String
	}
	return out
}

// validAIKey accepts 20-512 bytes of printable ASCII without spaces.
func validAIKey(v string) bool {
	if len(v) < 20 || len(v) > 512 {
		return false
	}
	for i := 0; i < len(v); i++ {
		if v[i] < 0x21 || v[i] > 0x7e {
			return false
		}
	}
	return true
}

func (s *Server) handleGetAI(w http.ResponseWriter, r *http.Request) {
	u, ok := userFrom(r.Context())
	if !ok {
		s.authError(w, r, store.ErrUnauthorized)
		return
	}
	a, err := s.auth.Store.AISettings(r.Context(), u.ID)
	if errors.Is(err, store.ErrAISettingsNotFound) {
		writeJSON(w, 200, aiUnconfiguredResponse{})
		return
	}
	if err != nil {
		integrationError(w, "ai get", u.ID, err)
		return
	}
	writeJSON(w, 200, toAIResponse(a))
}

// handlePutAI registers an API key and model, or changes the model with the
// saved key. The key is checked with the provider before it is saved; the
// save fails with ai_conflict when the settings changed since they were
// read here.
func (s *Server) handlePutAI(w http.ResponseWriter, r *http.Request) {
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
		APIKey json.RawMessage `json:"api_key"`
		Model  json.RawMessage `json:"model"`
	}
	if !decode(w, r, &in, integrationBodyLimit) {
		return
	}
	model, ok := decodeString(in.Model)
	if in.Model == nil || !ok || !ai.ValidModel(model) {
		fail(w, 400, "invalid_request", "model is invalid.")
		return
	}
	var key string
	if in.APIKey != nil {
		v, ok := decodeString(in.APIKey)
		key = strings.TrimSpace(v)
		if !ok || (key != "" && !validAIKey(key)) {
			fail(w, 400, "invalid_request", "api_key is invalid.")
			return
		}
	}
	current, err := s.auth.Store.AISettings(r.Context(), u.ID)
	found := err == nil
	if err != nil && !errors.Is(err, store.ErrAISettingsNotFound) {
		integrationError(w, "ai read", u.ID, err)
		return
	}
	keyChanged := key != ""
	if !keyChanged {
		if !found {
			fail(w, 400, "invalid_request", "api_key is required.")
			return
		}
		if key, err = s.box.Open(ai.KeyPurpose(u.ID), current.APIKeySealed); err != nil {
			fail(w, 400, "ai_key_invalid", "The saved API key cannot be used. Register the key again.")
			return
		}
	}
	// The provider is called before, and outside, the store transaction.
	switch err = s.ai.CheckModel(r.Context(), key, model); {
	case err == nil:
	case errors.Is(err, ai.ErrKeyInvalid):
		fail(w, 400, "ai_key_invalid", "The API key is invalid.")
		return
	case errors.Is(err, ai.ErrModelUnavailable):
		fail(w, 400, "ai_model_unavailable", "The API key cannot use this model.")
		return
	case errors.Is(err, ai.ErrBilling):
		fail(w, 400, "ai_billing", "Check the credit balance or billing settings of the provider.")
		return
	default:
		if r.Context().Err() == nil {
			log.Printf("integrations: ai verify user=%s: %v", u.ID, err)
		}
		fail(w, 503, "ai_unavailable", "The AI provider could not be reached. Try again later.")
		return
	}
	next := store.AISettings{UserID: u.ID, Provider: ai.ProviderAnthropic, Model: model, APIKeySealed: current.APIKeySealed, KeyLast4: current.KeyLast4}
	if keyChanged {
		if next.APIKeySealed, err = s.box.Seal(ai.KeyPurpose(u.ID), key); err != nil {
			integrationError(w, "ai seal", u.ID, err)
			return
		}
		next.KeyLast4 = tokenLast4(key)
	}
	saved, err := s.auth.Store.SaveAISettings(r.Context(), next, current.Generation, keyChanged, sid, s.auth.Now().Unix())
	switch {
	case err == nil:
	case errors.Is(err, store.ErrUnauthorized):
		s.authError(w, r, err)
		return
	case errors.Is(err, store.ErrAIConflict):
		fail(w, 409, "ai_conflict", "The AI settings were changed by another request. Reload them.")
		return
	default:
		integrationError(w, "ai save", u.ID, err)
		return
	}
	writeJSON(w, 200, toAIResponse(saved))
}

func (s *Server) handleDeleteAI(w http.ResponseWriter, r *http.Request) {
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
	err := s.auth.Store.DeleteAISettings(r.Context(), u.ID, sid, s.auth.Now().Unix())
	if errors.Is(err, store.ErrUnauthorized) {
		s.authError(w, r, err)
		return
	}
	if err != nil {
		integrationError(w, "ai delete", u.ID, err)
		return
	}
	w.WriteHeader(204)
}
