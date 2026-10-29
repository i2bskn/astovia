package store

import (
	"context"
	"database/sql"
	"errors"
)

// ErrAISettingsNotFound is returned when the user has no AI settings.
var ErrAISettingsNotFound = errors.New("AI settings not found")

// ErrAIConflict is returned by SaveAISettings when the settings changed
// after the caller read them (another save, or a delete).
var ErrAIConflict = errors.New("AI settings were changed by another request")

// AI settings status values.
const (
	AIActive = "active"
	AIError  = "error"
)

// AISettings is a user's AI provider, model and API key. APIKeySealed is the
// key sealed by secretbox; the plain key is never stored. Generation
// increases on every save so that a synchronization that read older
// settings cannot overwrite the status of newer ones. ActivatedAt is when
// the current key was registered.
type AISettings struct {
	UserID       string
	Generation   int64
	Provider     string
	Model        string
	APIKeySealed string
	KeyLast4     string
	Status       string
	LastError    sql.NullString
	ActivatedAt  int64
	CreatedAt    int64
	UpdatedAt    int64
}

// AISettings returns the user's settings, or ErrAISettingsNotFound.
func (s *Store) AISettings(ctx context.Context, userID string) (AISettings, error) {
	a, err := s.q.aiSettings(ctx, userID)
	if errors.Is(err, sql.ErrNoRows) {
		return AISettings{}, ErrAISettingsNotFound
	}
	return a, err
}

// SaveAISettings stores verified settings for a.UserID. a carries Provider,
// Model, APIKeySealed and KeyLast4. generation is the generation the caller
// read, or 0 when it found no settings; when the stored settings differ,
// ErrAIConflict is returned and nothing is written. keyChanged sets
// ActivatedAt to now (a model change keeps it). The status is reset to
// active without an error.
func (s *Store) SaveAISettings(ctx context.Context, a AISettings, generation int64, keyChanged bool, sessionID string, now int64) (AISettings, error) {
	var saved AISettings
	err := s.transaction(ctx, func(q queries) error {
		// Lock order: users, github_connections, ai_settings.
		if e := lockActiveUser(ctx, q, a.UserID, sessionID, now); e != nil {
			return e
		}
		existing, e := q.lockAISettings(ctx, a.UserID)
		found := e == nil
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		if found != (generation != 0) || (found && existing.Generation != generation) {
			return ErrAIConflict
		}
		next := AISettings{UserID: a.UserID, Generation: 1, Provider: a.Provider, Model: a.Model, APIKeySealed: a.APIKeySealed, KeyLast4: a.KeyLast4, Status: AIActive, ActivatedAt: now, CreatedAt: now, UpdatedAt: now}
		var n int64
		if found {
			next.Generation, next.CreatedAt = existing.Generation+1, existing.CreatedAt
			if !keyChanged {
				next.ActivatedAt = existing.ActivatedAt
			}
			n, e = q.replaceAISettings(ctx, next, existing.Generation)
		} else {
			n, e = q.createAISettings(ctx, next)
		}
		if e != nil {
			return e
		}
		if n != 1 {
			return ErrAIConflict
		}
		saved = next
		return nil
	})
	if err != nil {
		return AISettings{}, err
	}
	return saved, nil
}

// DeleteAISettings removes the user's settings. Deleting missing settings
// succeeds.
func (s *Store) DeleteAISettings(ctx context.Context, userID, sessionID string, now int64) error {
	return s.transaction(ctx, func(q queries) error {
		if e := lockActiveUser(ctx, q, userID, sessionID, now); e != nil {
			return e
		}
		return q.deleteAISettings(ctx, userID)
	})
}

// DeleteAISettingsReturning removes the user's settings like
// DeleteAISettings and returns the row that was removed, read in the same
// transaction. found is false when there were no settings.
func (s *Store) DeleteAISettingsReturning(ctx context.Context, userID, sessionID string, now int64) (AISettings, bool, error) {
	var removed AISettings
	found := false
	err := s.transaction(ctx, func(q queries) error {
		if e := lockActiveUser(ctx, q, userID, sessionID, now); e != nil {
			return e
		}
		existing, e := q.lockAISettings(ctx, userID)
		if errors.Is(e, sql.ErrNoRows) {
			return nil
		}
		if e != nil {
			return e
		}
		if e = q.deleteAISettings(ctx, userID); e != nil {
			return e
		}
		removed, found = existing, true
		return nil
	})
	if err != nil {
		return AISettings{}, false, err
	}
	return removed, found, nil
}

// UpdateAISecret replaces the sealed secret of the user's OpenAI settings
// of the given generation, when it is still oldSealed. The generation,
// updated_at, the status and the activation time are kept, so that a
// synchronization does not take a refreshed token for new settings. The
// user's state is not checked, like SetAIStatus. The result reports whether
// a row was updated.
func (s *Store) UpdateAISecret(ctx context.Context, userID string, generation int64, oldSealed, newSealed string) (bool, error) {
	n, err := s.q.updateAISecret(ctx, userID, generation, oldSealed, newSealed)
	return n == 1, err
}

// SetAIStatus records the outcome of using the settings of the given
// generation. Nothing is written when the settings were saved again or
// deleted in the meantime; the result reports whether a row was updated.
func (s *Store) SetAIStatus(ctx context.Context, userID string, generation int64, status string, lastError sql.NullString, now int64) (bool, error) {
	n, err := s.q.updateAISettingsStatus(ctx, userID, generation, status, lastError, now)
	return n == 1, err
}
