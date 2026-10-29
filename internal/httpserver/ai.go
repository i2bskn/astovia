package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/i2bskn/astovia/internal/ai"
	"github.com/i2bskn/astovia/internal/aicred"
	"github.com/i2bskn/astovia/internal/auth"
	"github.com/i2bskn/astovia/internal/openai"
	"github.com/i2bskn/astovia/internal/store"
)

// Time budgets of the ChatGPT plan handlers. The server's WriteTimeout is
// shorter, so these handlers extend their write deadline first, and keep
// every call to OpenAI (and the wait for the token lock) within
// openAIHandlerTimeout.
const (
	openAIWriteDeadline  = 60 * time.Second
	openAIHandlerTimeout = 55 * time.Second
	// complete: the code exchange, the ID token (with the signing keys),
	// the model list and one or two checks.
	openAIExchangeTimeout = 8 * time.Second
	openAIIDTokenTimeout  = 5 * time.Second
	openAIListTimeout     = 8 * time.Second
	openAICheckTimeout    = 12 * time.Second
	// openAISecondCheckLeft is the time that must be left to try a second
	// model.
	openAISecondCheckLeft = 13 * time.Second
	// PUT: the model list and the check.
	openAIPutListTimeout  = 12 * time.Second
	openAIPutCheckTimeout = 15 * time.Second
	// GET models.
	openAIModelsTimeout = 15 * time.Second
	// DELETE: the lock and the delete, then the revocation.
	openAIDeleteTimeout = 15 * time.Second
	openAIRevokeTimeout = 10 * time.Second
)

// openAIAuthPurpose is the secretbox purpose of a user's ChatGPT
// authorization in progress. It differs from VerifierPurpose, so that a
// Google state cannot complete a ChatGPT authorization or the reverse.
func openAIAuthPurpose(userID string) string { return "openai-auth:" + userID }

// openAIAuthState is what is sealed with the state of an authorization.
type openAIAuthState struct {
	Verifier string `json:"verifier"`
	Nonce    string `json:"nonce"`
	ClientID string `json:"client_id"`
}

// aiResponse is the user's AI settings. The key and the tokens are never
// included; key_last4 identifies an Anthropic key and account is the
// ChatGPT account's address (null for Anthropic or when unknown).
type aiResponse struct {
	Configured bool    `json:"configured"`
	Provider   string  `json:"provider"`
	Model      string  `json:"model"`
	KeyLast4   string  `json:"key_last4"`
	Account    *string `json:"account"`
	Status     string  `json:"status"`
	LastError  *string `json:"last_error"`
	UpdatedAt  string  `json:"updated_at"`
}

type aiUnconfiguredResponse struct {
	Configured bool `json:"configured"`
}

func (s *Server) toAIResponse(a store.AISettings) aiResponse {
	out := aiResponse{Configured: true, Provider: a.Provider, Model: a.Model, KeyLast4: a.KeyLast4, Status: a.Status, UpdatedAt: formatTime(a.UpdatedAt)}
	if a.Provider == ai.ProviderOpenAI {
		out.KeyLast4 = ""
		if secret, err := s.tokens.OpenSecret(a); err == nil && secret.Email != "" {
			out.Account = &secret.Email
		}
	}
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

// extendWriteDeadline lets a handler that waits for OpenAI write after the
// server's WriteTimeout. A failure leaves the server's deadline.
func extendWriteDeadline(w http.ResponseWriter) {
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(openAIWriteDeadline))
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
	writeJSON(w, 200, s.toAIResponse(a))
}

// handlePutAI registers an API key and model, or changes the model with the
// saved key or ChatGPT connection. The provider of the body must be the one
// of the saved settings: switching providers means deleting the settings
// first. The credential is checked with the provider before it is saved,
// outside the token lock; the save, under the lock, fails with ai_conflict
// when the settings changed since they were read here.
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
		APIKey   json.RawMessage `json:"api_key"`
		Model    json.RawMessage `json:"model"`
		Provider json.RawMessage `json:"provider"`
	}
	if !decode(w, r, &in, integrationBodyLimit) {
		return
	}
	provider := ai.ProviderAnthropic
	if in.Provider != nil {
		v, ok := decodeString(in.Provider)
		if !ok || (v != ai.ProviderAnthropic && v != ai.ProviderOpenAI) {
			fail(w, 400, "invalid_request", "provider is invalid.")
			return
		}
		provider = v
	}
	model, ok := decodeString(in.Model)
	valid := ai.ValidModel
	if provider == ai.ProviderOpenAI {
		valid = ai.ValidOpenAIModel
	}
	if in.Model == nil || !ok || !valid(model) {
		fail(w, 400, "invalid_request", "model is invalid.")
		return
	}
	var key string
	if in.APIKey != nil {
		if provider == ai.ProviderOpenAI {
			fail(w, 400, "invalid_request", "api_key is not used with the ChatGPT plan.")
			return
		}
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
	if found && current.Provider != provider {
		fail(w, 409, "ai_provider_mismatch", "Delete the current AI settings before using another provider.")
		return
	}
	if provider == ai.ProviderOpenAI {
		s.putOpenAI(w, r, u.ID, sid, model, current, found)
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
	var saved store.AISettings
	err = s.tokens.WithLock(r.Context(), u.ID, func() error {
		var e error
		saved, e = s.auth.Store.SaveAISettings(r.Context(), next, current.Generation, keyChanged, sid, s.auth.Now().Unix())
		return e
	})
	if !s.aiSaveError(w, r, u.ID, err) {
		return
	}
	writeJSON(w, 200, s.toAIResponse(saved))
}

// putOpenAI changes the model of the ChatGPT plan settings current: the
// model must be in the plan's list and answer a check request.
func (s *Server) putOpenAI(w http.ResponseWriter, r *http.Request, userID, sid, model string, current store.AISettings, found bool) {
	if !found {
		fail(w, 400, "invalid_request", "Connect ChatGPT first.")
		return
	}
	extendWriteDeadline(w)
	ctx, cancel := context.WithTimeout(r.Context(), openAIHandlerTimeout)
	defer cancel()
	// OpenAI is called outside the token lock: the token source takes it.
	cred, err := aicred.Open(s.box, s.tokens, current)
	if err != nil {
		s.openAIError(w, r, userID, "ai verify", err)
		return
	}
	lctx, lcancel := context.WithTimeout(ctx, openAIPutListTimeout)
	models, err := s.ai.OpenAIModels(lctx, cred)
	lcancel()
	if err != nil {
		s.openAIError(w, r, userID, "ai models", err)
		return
	}
	if !hasModel(models, model) {
		fail(w, 400, "ai_model_unavailable", "Your ChatGPT plan cannot use this model.")
		return
	}
	cctx, ccancel := context.WithTimeout(ctx, openAIPutCheckTimeout)
	err = s.ai.CheckOpenAI(cctx, cred, model)
	ccancel()
	if err != nil {
		s.openAIError(w, r, userID, "ai verify", err)
		return
	}
	var saved store.AISettings
	err = s.tokens.WithLock(ctx, userID, func() error {
		// The secret is read again: the check may have refreshed the
		// tokens.
		latest, e := s.auth.Store.AISettings(ctx, userID)
		if errors.Is(e, store.ErrAISettingsNotFound) {
			return store.ErrAIConflict
		}
		if e != nil {
			return e
		}
		if latest.Generation != current.Generation || latest.Provider != ai.ProviderOpenAI {
			return store.ErrAIConflict
		}
		next := store.AISettings{UserID: userID, Provider: ai.ProviderOpenAI, Model: model, APIKeySealed: latest.APIKeySealed}
		saved, e = s.auth.Store.SaveAISettings(ctx, next, current.Generation, false, sid, s.auth.Now().Unix())
		return e
	})
	if !s.aiSaveError(w, r, userID, err) {
		return
	}
	writeJSON(w, 200, s.toAIResponse(saved))
}

// hasModel reports whether slug is in models.
func hasModel(models []openai.Model, slug string) bool {
	for _, m := range models {
		if m.Slug == slug {
			return true
		}
	}
	return false
}

// aiSaveError writes the response of a failed save and reports whether err
// is nil.
func (s *Server) aiSaveError(w http.ResponseWriter, r *http.Request, userID string, err error) bool {
	switch {
	case err == nil:
		return true
	case errors.Is(err, store.ErrUnauthorized):
		s.authError(w, r, err)
	case errors.Is(err, store.ErrAIConflict):
		fail(w, 409, "ai_conflict", "The AI settings were changed by another request. Reload them.")
	case errors.Is(err, errProviderMismatch):
		fail(w, 409, "ai_provider_mismatch", "Delete the current AI settings before using another provider.")
	default:
		integrationError(w, "ai save", userID, err)
	}
	return false
}

// errProviderMismatch is settings of another provider found when saving.
var errProviderMismatch = errors.New("ai provider mismatch")

// openAIError writes the response of a failed call to the ChatGPT plan.
// The caller's own end is checked first; the errors are not logged with
// tokens, client IDs or addresses (the ai and aicred errors carry none).
func (s *Server) openAIError(w http.ResponseWriter, r *http.Request, userID, op string, err error) {
	switch {
	case r.Context().Err() != nil:
		fail(w, 503, "ai_unavailable", "The AI provider could not be reached. Try again later.")
	case errors.Is(err, aicred.ErrStale):
		fail(w, 409, "ai_conflict", "The AI settings were changed by another request. Reload them.")
	case errors.Is(err, aicred.ErrUndecryptable):
		fail(w, 400, "ai_key_invalid", "The saved ChatGPT connection cannot be used. Connect again.")
	case errors.Is(err, ai.ErrKeyInvalid):
		fail(w, 400, "ai_key_invalid", "The ChatGPT connection is no longer valid. Connect again.")
	case errors.Is(err, ai.ErrModelUnavailable):
		fail(w, 400, "ai_model_unavailable", "Your ChatGPT plan cannot use this model.")
	case errors.Is(err, ai.ErrBilling):
		fail(w, 400, "ai_billing", "Your ChatGPT plan cannot be used for this.")
	case errors.Is(err, ai.ErrUsageLimit):
		fail(w, 400, "ai_usage_limit", "The usage limit of your ChatGPT plan has been reached.")
	case errors.Is(err, ai.ErrUnusable):
		log.Printf("integrations: %s user=%s: %v", op, userID, err)
		fail(w, 400, "ai_unsupported", "The ChatGPT plan did not accept the request Astovia needs.")
	default:
		log.Printf("integrations: %s user=%s: %v", op, userID, err)
		fail(w, 503, "ai_unavailable", "The AI provider could not be reached. Try again later.")
	}
}

// handleAuthorizeOpenAI starts a ChatGPT authorization: it stores the state
// (bound to the user, with the Google states) with the PKCE verifier, the
// nonce and the client ID, and returns the URL of OpenAI's sign-in. A user
// connected again signs in with the client registered before; a new
// connection registers a client.
func (s *Server) handleAuthorizeOpenAI(w http.ResponseWriter, r *http.Request) {
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
	var in struct{}
	if !decode(w, r, &in, integrationBodyLimit) {
		return
	}
	current, err := s.auth.Store.AISettings(r.Context(), u.ID)
	found := err == nil
	if err != nil && !errors.Is(err, store.ErrAISettingsNotFound) {
		integrationError(w, "openai authorize", u.ID, err)
		return
	}
	if found && current.Provider != ai.ProviderOpenAI {
		fail(w, 409, "ai_provider_mismatch", "Delete the current AI settings before using another provider.")
		return
	}
	clientID := openai.DynamicClientID
	if found {
		if secret, err := s.tokens.OpenSecret(current); err == nil {
			clientID = secret.ClientID
		}
	}
	var random [3]string
	for i := range random {
		if random[i], err = auth.Random(); err != nil {
			integrationError(w, "openai authorize", u.ID, err)
			return
		}
	}
	state := "oai_" + random[0]
	pending := openAIAuthState{Verifier: random[1], Nonce: random[2], ClientID: clientID}
	data, err := json.Marshal(pending)
	if err != nil {
		integrationError(w, "openai authorize", u.ID, err)
		return
	}
	sealed, err := s.box.Seal(openAIAuthPurpose(u.ID), string(data))
	if err != nil {
		integrationError(w, "openai seal", u.ID, err)
		return
	}
	err = s.auth.Store.CreateAuthState(r.Context(), u.ID, sid, auth.Hash(state), sealed, s.auth.Now().Unix(), authStateTTL)
	if errors.Is(err, store.ErrUnauthorized) {
		s.authError(w, r, err)
		return
	}
	if err != nil {
		integrationError(w, "openai authorize", u.ID, err)
		return
	}
	writeJSON(w, 200, map[string]string{"url": s.tokens.OpenAI.AuthURL(clientID, state, pending.Nonce, openai.Challenge(pending.Verifier)), "redirect_uri": openai.RedirectURI})
}

// handleCompleteOpenAI completes a ChatGPT authorization with the address
// the browser was sent to, pasted by the user. The address is checked
// before the state is consumed; once consumed, any failure means starting
// again. The tokens received are never revoked on failure (it is not known
// how much a revocation removes).
func (s *Server) handleCompleteOpenAI(w http.ResponseWriter, r *http.Request) {
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
		CallbackURL json.RawMessage `json:"callback_url"`
	}
	if !decode(w, r, &in, integrationBodyLimit) {
		return
	}
	raw, ok := decodeString(in.CallbackURL)
	if in.CallbackURL == nil || !ok {
		fail(w, 400, "invalid_request", "callback_url must be a string.")
		return
	}
	cb, err := openai.ParseCallback(raw)
	if err != nil {
		fail(w, 400, "openai_callback_invalid", "The pasted address cannot be read. Paste the whole address that starts with http://127.0.0.1:1455/.")
		return
	}
	extendWriteDeadline(w)
	ctx, cancel := context.WithTimeout(r.Context(), openAIHandlerTimeout)
	defer cancel()
	sealed, err := s.auth.Store.ConsumeAuthState(r.Context(), u.ID, sid, auth.Hash(cb.State), s.auth.Now().Unix())
	switch {
	case err == nil:
	case errors.Is(err, store.ErrAuthStateNotFound):
		fail(w, 400, "openai_state_invalid", "The connection request expired or was already used. Start again.")
		return
	case errors.Is(err, store.ErrUnauthorized):
		s.authError(w, r, err)
		return
	default:
		integrationError(w, "openai complete", u.ID, err)
		return
	}
	var pending openAIAuthState
	opened, err := s.box.Open(openAIAuthPurpose(u.ID), sealed)
	if err != nil || json.Unmarshal([]byte(opened), &pending) != nil || pending.Verifier == "" || pending.Nonce == "" || pending.ClientID == "" {
		fail(w, 400, "openai_state_invalid", "The connection request expired or was already used. Start again.")
		return
	}
	if cb.Error != "" {
		fail(w, 400, "openai_denied", "ChatGPT did not allow the connection.")
		return
	}
	clientID := pending.ClientID
	if clientID == openai.DynamicClientID {
		if cb.ClientID == "" || cb.ClientID == openai.DynamicClientID {
			fail(w, 400, "openai_callback_invalid", "The pasted address cannot be read. Paste the whole address that starts with http://127.0.0.1:1455/.")
			return
		}
		clientID = cb.ClientID
	} else if cb.ClientID != "" && cb.ClientID != clientID {
		fail(w, 400, "openai_callback_invalid", "The pasted address cannot be read. Paste the whole address that starts with http://127.0.0.1:1455/.")
		return
	}
	if cb.Scopes != nil && !openai.HasScope(cb.Scopes, openai.PlanScope) {
		fail(w, 400, "openai_scope_missing", "Allow using your ChatGPT plan to connect.")
		return
	}
	current, err := s.auth.Store.AISettings(r.Context(), u.ID)
	found := err == nil
	if err != nil && !errors.Is(err, store.ErrAISettingsNotFound) {
		integrationError(w, "openai complete", u.ID, err)
		return
	}
	if found && current.Provider != ai.ProviderOpenAI {
		fail(w, 409, "ai_provider_mismatch", "Delete the current AI settings before using another provider.")
		return
	}
	ectx, ecancel := context.WithTimeout(ctx, openAIExchangeTimeout)
	tok, err := s.tokens.OpenAI.Exchange(ectx, clientID, cb.Code, pending.Verifier)
	ecancel()
	switch {
	case err == nil:
	case errors.Is(err, openai.ErrInvalidGrant):
		fail(w, 400, "openai_state_invalid", "The connection request expired or was already used. Start again.")
		return
	default:
		log.Printf("integrations: openai exchange user=%s: %v", u.ID, err)
		fail(w, 502, "openai_unavailable", "ChatGPT could not complete the connection. Try again.")
		return
	}
	if !openai.HasScope(tok.Scopes, openai.PlanScope) {
		fail(w, 400, "openai_scope_missing", "Allow using your ChatGPT plan to connect.")
		return
	}
	ictx, icancel := context.WithTimeout(ctx, openAIIDTokenTimeout)
	identity, err := s.tokens.OpenAI.VerifyIDToken(ictx, tok.IDToken, clientID, pending.Nonce)
	icancel()
	if err != nil {
		log.Printf("integrations: openai id token invalid user=%s", u.ID)
		fail(w, 502, "openai_unavailable", "ChatGPT could not complete the connection. Try again.")
		return
	}
	// The checks use the new access token as it is: nothing is refreshed
	// before the tokens are saved.
	cred := ai.Credential{Provider: ai.ProviderOpenAI, Token: func(_ context.Context, failed string) (string, error) {
		if failed != "" {
			return "", ai.ErrKeyInvalid
		}
		return tok.AccessToken, nil
	}}
	lctx, lcancel := context.WithTimeout(ctx, openAIListTimeout)
	models, err := s.ai.OpenAIModels(lctx, cred)
	lcancel()
	if err != nil {
		s.openAIError(w, r, u.ID, "openai models", err)
		return
	}
	if len(models) == 0 {
		fail(w, 400, "ai_model_unavailable", "Your ChatGPT plan has no model Astovia can use.")
		return
	}
	// The saved model is kept when the plan still has it; otherwise the
	// first light model (Luna, like Haiku for Anthropic) or else the first
	// of the list. When the model cannot be used, the next one of the list
	// is tried once.
	first := 0
	for i, m := range models {
		if strings.Contains(strings.ToLower(m.Slug), "luna") {
			first = i
			break
		}
	}
	if found {
		for i, m := range models {
			if m.Slug == current.Model {
				first = i
				break
			}
		}
	}
	model := models[first].Slug
	cctx, ccancel := context.WithTimeout(ctx, openAICheckTimeout)
	err = s.ai.CheckOpenAI(cctx, cred, model)
	ccancel()
	if err != nil && (errors.Is(err, ai.ErrModelUnavailable) || errors.Is(err, ai.ErrUnusable)) && first+1 < len(models) && r.Context().Err() == nil {
		if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) >= openAISecondCheckLeft {
			log.Printf("integrations: openai check user=%s: %v", u.ID, err)
			model = models[first+1].Slug
			cctx, ccancel = context.WithTimeout(ctx, openAICheckTimeout)
			err = s.ai.CheckOpenAI(cctx, cred, model)
			ccancel()
		}
	}
	if err != nil {
		s.openAIError(w, r, u.ID, "openai check", err)
		return
	}
	secret, err := s.tokens.Seal(u.ID, aicred.Secret{ClientID: clientID, Subject: identity.Subject, Email: identity.Email, RefreshToken: tok.RefreshToken, AccessToken: tok.AccessToken, ExpiresAt: tok.Expiry.Unix()})
	if err != nil {
		integrationError(w, "openai seal", u.ID, err)
		return
	}
	var saved store.AISettings
	err = s.tokens.WithLock(ctx, u.ID, func() error {
		latest, e := s.auth.Store.AISettings(ctx, u.ID)
		stillFound := e == nil
		if e != nil && !errors.Is(e, store.ErrAISettingsNotFound) {
			return e
		}
		if stillFound && latest.Provider != ai.ProviderOpenAI {
			return errProviderMismatch
		}
		if stillFound != found || (found && latest.Generation != current.Generation) {
			return store.ErrAIConflict
		}
		next := store.AISettings{UserID: u.ID, Provider: ai.ProviderOpenAI, Model: model, APIKeySealed: secret}
		saved, e = s.auth.Store.SaveAISettings(ctx, next, current.Generation, true, sid, s.auth.Now().Unix())
		return e
	})
	if !s.aiSaveError(w, r, u.ID, err) {
		return
	}
	writeJSON(w, 200, s.toAIResponse(saved))
}

// handleOpenAIModels lists the models of the user's ChatGPT plan.
func (s *Server) handleOpenAIModels(w http.ResponseWriter, r *http.Request) {
	u, ok := userFrom(r.Context())
	if !ok {
		s.authError(w, r, store.ErrUnauthorized)
		return
	}
	current, err := s.auth.Store.AISettings(r.Context(), u.ID)
	if errors.Is(err, store.ErrAISettingsNotFound) || (err == nil && current.Provider != ai.ProviderOpenAI) {
		fail(w, 404, "not_found", "The ChatGPT connection was not found.")
		return
	}
	if err != nil {
		integrationError(w, "openai models", u.ID, err)
		return
	}
	extendWriteDeadline(w)
	cred, err := aicred.Open(s.box, s.tokens, current)
	if err != nil {
		s.openAIError(w, r, u.ID, "openai models", err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), openAIModelsTimeout)
	models, err := s.ai.OpenAIModels(ctx, cred)
	cancel()
	if err != nil {
		s.openAIError(w, r, u.ID, "openai models", err)
		return
	}
	type modelResponse struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	out := struct {
		Models []modelResponse `json:"models"`
	}{Models: []modelResponse{}}
	for _, m := range models {
		out.Models = append(out.Models, modelResponse{ID: m.Slug, Name: m.Name})
	}
	writeJSON(w, 200, out)
}

// handleDeleteAI removes the settings. For the ChatGPT plan the refresh
// token is then revoked at OpenAI; the row is removed under the token lock
// so that a refresh token rotated just before is not missed. A failed
// revocation does not fail the removal: it answers 200 with revoke_failed
// so that the user can remove the connection on ChatGPT.
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
	extendWriteDeadline(w)
	ctx, cancel := context.WithTimeout(r.Context(), openAIHandlerTimeout)
	defer cancel()
	dctx, dcancel := context.WithTimeout(ctx, openAIDeleteTimeout)
	var removed store.AISettings
	var found bool
	err := s.tokens.WithLock(dctx, u.ID, func() error {
		var e error
		removed, found, e = s.auth.Store.DeleteAISettingsReturning(dctx, u.ID, sid, s.auth.Now().Unix())
		return e
	})
	dcancel()
	if errors.Is(err, store.ErrUnauthorized) {
		s.authError(w, r, err)
		return
	}
	if err != nil {
		integrationError(w, "ai delete", u.ID, err)
		return
	}
	if !found || removed.Provider != ai.ProviderOpenAI {
		w.WriteHeader(204)
		return
	}
	secret, err := s.tokens.OpenSecret(removed)
	if err != nil {
		log.Printf("integrations: openai revoke user=%s: undecryptable", u.ID)
		writeJSON(w, 200, map[string]bool{"revoke_failed": true})
		return
	}
	rctx, rcancel := context.WithTimeout(context.WithoutCancel(r.Context()), openAIRevokeTimeout)
	err = s.tokens.OpenAI.Revoke(rctx, secret.ClientID, secret.RefreshToken)
	rcancel()
	if err != nil {
		log.Printf("integrations: openai revoke user=%s: %v", u.ID, err)
		writeJSON(w, 200, map[string]bool{"revoke_failed": true})
		return
	}
	w.WriteHeader(204)
}
