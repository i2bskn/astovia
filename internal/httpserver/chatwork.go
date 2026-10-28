package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/i2bskn/astovia/internal/chatwork"
	"github.com/i2bskn/astovia/internal/chatworksync"
	"github.com/i2bskn/astovia/internal/store"
)

// chatworkTimeout bounds the token check against Chatwork.
const chatworkTimeout = 10 * time.Second

// chatworkResponse is a connected Chatwork integration. The token is never
// included; token_last4 identifies it.
type chatworkResponse struct {
	Connected        bool    `json:"connected"`
	AccountName      string  `json:"account_name"`
	OrganizationName string  `json:"organization_name"`
	TokenLast4       string  `json:"token_last4"`
	ImmediateNotify  bool    `json:"immediate_notify"`
	DetectMentions   bool    `json:"detect_mentions"`
	DetectDMs        bool    `json:"detect_dms"`
	ImportTasks      bool    `json:"import_tasks"`
	Status           string  `json:"status"`
	LastError        *string `json:"last_error"`
	LastSyncedAt     *string `json:"last_synced_at"`
	CreatedAt        string  `json:"created_at"`
}

// toChatworkResponse converts c. last_synced_at is the later of the
// detection window and the last task import, so that it moves when only
// the tasks are imported (without AI settings).
func toChatworkResponse(c store.ChatworkConnection) chatworkResponse {
	out := chatworkResponse{Connected: true, AccountName: c.AccountName, OrganizationName: c.OrganizationName, TokenLast4: c.TokenLast4,
		ImmediateNotify: c.ImmediateNotify == 1, DetectMentions: c.DetectMentions == 1, DetectDMs: c.DetectDMs == 1, ImportTasks: c.ImportTasks == 1, Status: c.Status, CreatedAt: formatTime(c.CreatedAt)}
	if c.LastError.Valid {
		out.LastError = &c.LastError.String
	}
	synced := c.LastSyncedAt
	if c.LastTasksAt.Valid && (!synced.Valid || c.LastTasksAt.Int64 > synced.Int64) {
		synced = c.LastTasksAt
	}
	if synced.Valid {
		v := formatTime(synced.Int64)
		out.LastSyncedAt = &v
	}
	return out
}

// chatworkSettingsInput is the part of a PUT or PATCH body with the
// settings.
type chatworkSettingsInput struct {
	ImmediateNotify json.RawMessage `json:"immediate_notify"`
	DetectMentions  json.RawMessage `json:"detect_mentions"`
	DetectDMs       json.RawMessage `json:"detect_dms"`
	ImportTasks     json.RawMessage `json:"import_tasks"`
}

// settings decodes the settings present in the body. It reports false and
// writes the response when one is not a boolean.
func (in chatworkSettingsInput) settings(w http.ResponseWriter) (store.ChatworkSettings, int, bool) {
	var out store.ChatworkSettings
	n := 0
	for _, f := range []struct {
		name string
		raw  json.RawMessage
		dst  **bool
	}{{"immediate_notify", in.ImmediateNotify, &out.ImmediateNotify}, {"detect_mentions", in.DetectMentions, &out.DetectMentions}, {"detect_dms", in.DetectDMs, &out.DetectDMs}, {"import_tasks", in.ImportTasks, &out.ImportTasks}} {
		if f.raw == nil {
			continue
		}
		v, ok := decodeBool(f.raw)
		if !ok {
			fail(w, 400, "invalid_request", f.name+" must be a boolean.")
			return store.ChatworkSettings{}, 0, false
		}
		*f.dst = &v
		n++
	}
	return out, n, true
}

func (s *Server) handleGetChatwork(w http.ResponseWriter, r *http.Request) {
	u, ok := userFrom(r.Context())
	if !ok {
		s.authError(w, r, store.ErrUnauthorized)
		return
	}
	c, err := s.auth.Store.ChatworkConnection(r.Context(), u.ID)
	if errors.Is(err, store.ErrConnectionNotFound) {
		writeJSON(w, 200, githubDisconnectedResponse{})
		return
	}
	if err != nil {
		integrationError(w, "chatwork get", u.ID, err)
		return
	}
	writeJSON(w, 200, toChatworkResponse(c))
}

func (s *Server) handlePutChatwork(w http.ResponseWriter, r *http.Request) {
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
		Token json.RawMessage `json:"token"`
		chatworkSettingsInput
	}
	if !decode(w, r, &in, integrationBodyLimit) {
		return
	}
	var token string
	if in.Token != nil {
		token, ok = decodeString(in.Token)
	}
	// The same format check as a GitHub token: printable ASCII.
	if in.Token == nil || !ok || !validGitHubToken(token) {
		fail(w, 400, "invalid_request", "token is invalid.")
		return
	}
	settings, _, ok := in.settings(w)
	if !ok {
		return
	}
	// Chatwork is called before, and outside, the store transaction.
	ctx, cancel := context.WithTimeout(r.Context(), chatworkTimeout)
	account, err := s.chatwork.Me(ctx, token)
	cancel()
	switch {
	case err == nil:
	case errors.Is(err, chatwork.ErrUnauthorized):
		fail(w, 400, "chatwork_token_invalid", "The Chatwork API token is invalid.")
		return
	default:
		log.Printf("integrations: chatwork verify user=%s: %v", u.ID, err)
		fail(w, 503, "chatwork_unavailable", "Chatwork could not be reached. Try again later.")
		return
	}
	if !chatwork.ValidID(account.ID) {
		log.Printf("integrations: chatwork verify user=%s: unexpected account", u.ID)
		fail(w, 503, "chatwork_unavailable", "Chatwork could not be reached. Try again later.")
		return
	}
	sealed, err := s.box.Seal(chatworksync.TokenPurpose(u.ID), token)
	if err != nil {
		integrationError(w, "chatwork seal", u.ID, err)
		return
	}
	c, err := s.auth.Store.SaveChatworkConnection(r.Context(), store.ChatworkConnection{UserID: u.ID, TokenSealed: sealed, TokenLast4: tokenLast4(token), AccountID: account.ID,
		AccountName: account.Name, OrganizationName: account.OrganizationName}, settings, sid, s.auth.Now().Unix())
	if errors.Is(err, store.ErrUnauthorized) {
		s.authError(w, r, err)
		return
	}
	if err != nil {
		integrationError(w, "chatwork save", u.ID, err)
		return
	}
	writeJSON(w, 200, toChatworkResponse(c))
}

func (s *Server) handlePatchChatwork(w http.ResponseWriter, r *http.Request) {
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
	var in chatworkSettingsInput
	if !decode(w, r, &in, integrationBodyLimit) {
		return
	}
	settings, n, ok := in.settings(w)
	if !ok {
		return
	}
	if n == 0 {
		fail(w, 400, "invalid_request", "At least one of immediate_notify, detect_mentions, detect_dms and import_tasks is required.")
		return
	}
	c, err := s.auth.Store.UpdateChatworkSettings(r.Context(), u.ID, settings, sid, s.auth.Now().Unix())
	switch {
	case err == nil:
	case errors.Is(err, store.ErrUnauthorized):
		s.authError(w, r, err)
		return
	case errors.Is(err, store.ErrConnectionNotFound):
		fail(w, 404, "not_found", "Chatwork is not connected.")
		return
	default:
		integrationError(w, "chatwork patch", u.ID, err)
		return
	}
	writeJSON(w, 200, toChatworkResponse(c))
}

func (s *Server) handleDeleteChatwork(w http.ResponseWriter, r *http.Request) {
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
	err := s.auth.Store.DeleteChatworkConnection(r.Context(), u.ID, sid, s.auth.Now().Unix())
	if errors.Is(err, store.ErrUnauthorized) {
		s.authError(w, r, err)
		return
	}
	if err != nil {
		integrationError(w, "chatwork delete", u.ID, err)
		return
	}
	w.WriteHeader(204)
}
