// Package aicred opens the saved AI credential of a user for a judgment and
// manages the access tokens of the ChatGPT plan (OpenAI): it refreshes them
// one at a time per user and saves the rotated refresh token. The lock is
// held in the process, so Astovia runs as a single process, and the HTTP
// server and the synchronizations share one Tokens. Tokens, client IDs and
// addresses are never logged or included in errors.
package aicred

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/i2bskn/astovia/internal/ai"
	"github.com/i2bskn/astovia/internal/openai"
	"github.com/i2bskn/astovia/internal/secretbox"
	"github.com/i2bskn/astovia/internal/store"
)

const (
	// refreshTimeout bounds a refresh and the save of its tokens.
	refreshTimeout = 10 * time.Second
	// freshFor is how long an access token must still be valid to be used;
	// retryFreshFor the same for a token another request just refreshed.
	freshFor      = 5 * time.Minute
	retryFreshFor = time.Minute
	// usageLimitWait is how long the AI is not called after the usage limit
	// of the ChatGPT plan was reached.
	usageLimitWait = 15 * time.Minute
	// UsageLimit is the last_error of a reached usage limit.
	UsageLimit = "usage_limit"
)

var (
	// ErrUndecryptable is a saved secret that cannot be opened or read.
	ErrUndecryptable = errors.New("aicred: undecryptable")
	// ErrStale is AI settings that changed or were removed while a token
	// was being obtained. It wraps ai.ErrTemporary.
	ErrStale = fmt.Errorf("aicred: stale: %w", ai.ErrTemporary)
)

// SecretPurpose is the secretbox purpose of a user's OpenAI secret. It
// differs from ai.KeyPurpose so that one cannot be taken for the other.
func SecretPurpose(userID string) string { return "ai-openai:" + userID }

// Secret is what the OpenAI settings keep sealed in api_key_sealed.
// ExpiresAt is the Unix time the access token expires at.
type Secret struct {
	V            int    `json:"v"`
	ClientID     string `json:"client_id"`
	Subject      string `json:"subject"`
	Email        string `json:"email"`
	RefreshToken string `json:"refresh_token"`
	AccessToken  string `json:"access_token"`
	ExpiresAt    int64  `json:"expires_at"`
}

// openSecret opens and checks the sealed secret of OpenAI settings.
func openSecret(box *secretbox.Box, a store.AISettings) (Secret, error) {
	if box == nil || a.Provider != ai.ProviderOpenAI {
		return Secret{}, ErrUndecryptable
	}
	raw, err := box.Open(SecretPurpose(a.UserID), a.APIKeySealed)
	if err != nil {
		return Secret{}, ErrUndecryptable
	}
	var s Secret
	if json.Unmarshal([]byte(raw), &s) != nil || s.V != 1 || s.ClientID == "" || s.Subject == "" || s.RefreshToken == "" {
		return Secret{}, ErrUndecryptable
	}
	return s, nil
}

// EndpointPurpose is the secretbox purpose of a user's OpenAI API key or
// OpenAI-compatible endpoint. It differs from ai.KeyPurpose and
// SecretPurpose so that one cannot be taken for another.
func EndpointPurpose(userID string) string { return "ai-endpoint:" + userID }

// Endpoint is what the settings of an OpenAI API key or an
// OpenAI-compatible endpoint keep sealed in api_key_sealed. BaseURL is
// empty for the OpenAI API; APIKey may be empty for an endpoint.
type Endpoint struct {
	V       int    `json:"v"`
	BaseURL string `json:"base_url"`
	APIKey  string `json:"api_key"`
}

// SealEndpoint seals e (with version 1) for the user's settings.
func SealEndpoint(box *secretbox.Box, userID string, e Endpoint) (string, error) {
	e.V = 1
	data, err := json.Marshal(e)
	if err != nil {
		return "", err
	}
	return box.Seal(EndpointPurpose(userID), string(data))
}

// OpenEndpoint opens and checks the sealed endpoint of settings a of an
// OpenAI API key or an OpenAI-compatible endpoint.
func OpenEndpoint(box *secretbox.Box, a store.AISettings) (Endpoint, error) {
	if box == nil || (a.Provider != ai.ProviderOpenAIAPI && a.Provider != ai.ProviderOpenAICompatible) {
		return Endpoint{}, ErrUndecryptable
	}
	raw, err := box.Open(EndpointPurpose(a.UserID), a.APIKeySealed)
	if err != nil {
		return Endpoint{}, ErrUndecryptable
	}
	var e Endpoint
	if json.Unmarshal([]byte(raw), &e) != nil || e.V != 1 {
		return Endpoint{}, ErrUndecryptable
	}
	if (a.Provider == ai.ProviderOpenAICompatible && e.BaseURL == "") || (a.Provider == ai.ProviderOpenAIAPI && e.APIKey == "") {
		return Endpoint{}, ErrUndecryptable
	}
	return e, nil
}

// Open returns the credential of the saved settings a. For OpenAI it only
// opens and checks the secret; the token source refreshes the access
// token when needed. A secret that cannot be opened, or an unknown
// provider, is ErrUndecryptable. Without tokens, the token source of OpenAI
// settings fails with ai.ErrTemporary.
func Open(box *secretbox.Box, tokens *Tokens, a store.AISettings) (ai.Credential, error) {
	switch a.Provider {
	case ai.ProviderAnthropic:
		if box == nil {
			return ai.Credential{}, ErrUndecryptable
		}
		key, err := box.Open(ai.KeyPurpose(a.UserID), a.APIKeySealed)
		if err != nil {
			return ai.Credential{}, ErrUndecryptable
		}
		return ai.Credential{Provider: ai.ProviderAnthropic, Key: key}, nil
	case ai.ProviderOpenAI:
		if _, err := openSecret(box, a); err != nil {
			return ai.Credential{}, err
		}
		cred := ai.Credential{Provider: ai.ProviderOpenAI}
		if tokens == nil {
			cred.Token = func(context.Context, string) (string, error) {
				return "", fmt.Errorf("%w (no token manager)", ai.ErrTemporary)
			}
		} else {
			cred.Token = tokens.source(a.UserID, a.Generation)
		}
		return cred, nil
	case ai.ProviderOpenAIAPI, ai.ProviderOpenAICompatible:
		e, err := OpenEndpoint(box, a)
		if err != nil {
			return ai.Credential{}, err
		}
		cred := ai.Credential{Provider: a.Provider, Key: e.APIKey}
		if a.Provider == ai.ProviderOpenAICompatible {
			cred.BaseURL = e.BaseURL
		}
		return cred, nil
	}
	return ai.Credential{}, ErrUndecryptable
}

// CoolingDown reports whether the AI is not to be called for OpenAI
// settings a: their usage limit was reached less than 15 minutes ago.
func CoolingDown(a store.AISettings, now time.Time) bool {
	return a.Provider == ai.ProviderOpenAI && a.LastError.Valid && a.LastError.String == UsageLimit && now.Unix()-a.UpdatedAt < int64(usageLimitWait/time.Second)
}

// Tokens manages the access tokens of the ChatGPT plan. The zero value of
// the lock map is ready to use.
type Tokens struct {
	Store  *store.Store
	Box    *secretbox.Box
	OpenAI *openai.Client
	Now    func() time.Time

	// mu guards locks: one channel of capacity 1 per user, so that waiting
	// for the lock can end with the context. Entries are never removed;
	// there is one per user who used the ChatGPT plan.
	mu    sync.Mutex
	locks map[string]chan struct{}
}

func (t *Tokens) now() time.Time {
	if t.Now == nil {
		return time.Now()
	}
	return t.Now()
}

func (t *Tokens) lock(userID string) chan struct{} {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.locks == nil {
		t.locks = map[string]chan struct{}{}
	}
	ch, ok := t.locks[userID]
	if !ok {
		ch = make(chan struct{}, 1)
		t.locks[userID] = ch
	}
	return ch
}

// WithLock runs fn while holding the user's token lock. It returns ctx's
// error when ctx ends before the lock is taken. fn must not call a token
// source of the same user (it would wait for itself).
func (t *Tokens) WithLock(ctx context.Context, userID string, fn func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	ch := t.lock(userID)
	select {
	case ch <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-ch }()
	return fn()
}

// Seal seals s (with version 1) for the user's OpenAI settings.
func (t *Tokens) Seal(userID string, s Secret) (string, error) {
	s.V = 1
	data, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	return t.Box.Seal(SecretPurpose(userID), string(data))
}

// OpenSecret opens the secret of OpenAI settings a, or returns
// ErrUndecryptable.
func (t *Tokens) OpenSecret(a store.AISettings) (Secret, error) {
	return openSecret(t.Box, a)
}

// source returns the token source of the user's OpenAI settings of the
// given generation.
func (t *Tokens) source(userID string, generation int64) func(ctx context.Context, failed string) (string, error) {
	return func(ctx context.Context, failed string) (string, error) {
		var token string
		err := t.WithLock(ctx, userID, func() error {
			var e error
			token, e = t.token(ctx, userID, generation, failed)
			return e
		})
		if err != nil {
			return "", err
		}
		return token, nil
	}
}

// token returns a usable access token, refreshing it when it expires soon
// or failed was refused. It runs under the user's lock.
func (t *Tokens) token(ctx context.Context, userID string, generation int64, failed string) (string, error) {
	a, err := t.Store.AISettings(ctx, userID)
	switch {
	case errors.Is(err, store.ErrAISettingsNotFound):
		return "", ErrStale
	case err != nil:
		if e := ctx.Err(); e != nil {
			return "", e
		}
		return "", fmt.Errorf("%w (read settings)", ai.ErrTemporary)
	case a.Generation != generation || a.Provider != ai.ProviderOpenAI:
		return "", ErrStale
	}
	s, err := t.OpenSecret(a)
	if err != nil {
		return "", fmt.Errorf("%w (undecryptable)", ai.ErrKeyInvalid)
	}
	left := time.Unix(s.ExpiresAt, 0).Sub(t.now())
	if s.AccessToken != "" {
		if failed == "" && left > freshFor {
			return s.AccessToken, nil
		}
		// Another request refreshed the token after failed was read.
		if failed != "" && s.AccessToken != failed && left > retryFreshFor {
			return s.AccessToken, nil
		}
	}
	// The refresh and the save run to the end even when the caller gives
	// up: OpenAI has already replaced the refresh token once it answers,
	// and the new one would be lost.
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), refreshTimeout)
	defer cancel()
	tok, err := t.OpenAI.Refresh(rctx, s.ClientID, s.RefreshToken)
	switch {
	case err == nil:
	case errors.Is(err, openai.ErrInvalidGrant), errors.Is(err, openai.ErrRejected):
		return "", fmt.Errorf("%w (openai refresh: %v)", ai.ErrKeyInvalid, err)
	default:
		return "", fmt.Errorf("%w (openai refresh: %v)", ai.ErrTemporary, err)
	}
	s.RefreshToken, s.AccessToken, s.ExpiresAt = tok.RefreshToken, tok.AccessToken, tok.Expiry.Unix()
	sealed, err := t.Seal(userID, s)
	if err != nil {
		log.Printf("aicred: user=%s: openai token refresh not saved", userID)
		return "", fmt.Errorf("%w (seal)", ai.ErrTemporary)
	}
	ok, err := t.Store.UpdateAISecret(rctx, userID, generation, a.APIKeySealed, sealed)
	if err != nil {
		log.Printf("aicred: user=%s: openai token refresh not saved", userID)
		return "", fmt.Errorf("%w (save)", ai.ErrTemporary)
	}
	if !ok {
		return "", ErrStale
	}
	if e := ctx.Err(); e != nil {
		return "", e
	}
	return tok.AccessToken, nil
}
