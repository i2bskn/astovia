package store

import (
	"context"
	"database/sql"
	"errors"
	"sort"
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

// Scopes of the AI instructions: InstructionCommon applies to every
// judgment, the others to one process of one integration.
const (
	InstructionCommon         = "common"
	InstructionGitHubDetect   = "github.detect"
	InstructionSlackDetect    = "slack.detect"
	InstructionChatworkDetect = "chatwork.detect"
	InstructionGmailDetect    = "gmail.detect"
	InstructionSlackReply     = "slack.reply"
	InstructionChatworkReply  = "chatwork.reply"
	InstructionGmailReply     = "gmail.reply"
)

// InstructionScopes lists every instruction scope in display order.
var InstructionScopes = []string{InstructionCommon, InstructionGitHubDetect, InstructionSlackDetect, InstructionChatworkDetect, InstructionGmailDetect, InstructionSlackReply, InstructionChatworkReply, InstructionGmailReply}

// MaxInstructionLength is the maximum length of one instruction in runes.
const MaxInstructionLength = 2000

// ValidInstructionScope reports whether scope is one of InstructionScopes.
func ValidInstructionScope(scope string) bool {
	for _, s := range InstructionScopes {
		if s == scope {
			return true
		}
	}
	return false
}

// AIInstruction is one of the user's additional instructions to the AI. An
// empty instruction has no row.
type AIInstruction struct {
	UserID    string
	Scope     string
	Body      string
	UpdatedAt int64
}

// AIInstructions returns the user's instructions by scope. Only the scopes
// with an instruction are present.
func (s *Store) AIInstructions(ctx context.Context, userID string) (map[string]string, error) {
	rows, err := s.q.listAIInstructions(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(rows))
	for _, row := range rows {
		out[row.Scope] = row.Body
	}
	return out, nil
}

// SaveAIInstructions replaces every instruction of the user with in, keyed
// by scope. An empty or missing scope removes the instruction. The caller
// validates the scopes and lengths; unknown scopes are ignored.
func (s *Store) SaveAIInstructions(ctx context.Context, userID, sessionID string, in map[string]string, now int64) error {
	return s.transaction(ctx, func(q queries) error {
		// Lock order: users, ai_instructions.
		if e := lockActiveUser(ctx, q, userID, sessionID, now); e != nil {
			return e
		}
		for _, scope := range InstructionScopes {
			var e error
			if body := in[scope]; body == "" {
				e = q.deleteAIInstruction(ctx, userID, scope)
			} else {
				e = q.upsertAIInstruction(ctx, AIInstruction{UserID: userID, Scope: scope, Body: body, UpdatedAt: now})
			}
			if e != nil {
				return e
			}
		}
		return nil
	})
}

// AIDetectSamples returns up to limit of the user's items of source made
// from AI judgments, newest detection first: a source key starting with one
// of prefixes, a detection time, and content not starting with fallback
// (the confirmation task made when the AI could not judge). It reads
// without locking.
func (s *Store) AIDetectSamples(ctx context.Context, userID, source string, prefixes []string, fallback string, limit int) ([]ActionItem, error) {
	var out []ActionItem
	for _, p := range prefixes {
		items, err := s.q.listAIDetectSamples(ctx, userID, source, p, fallback, limit)
		if err != nil {
			return nil, err
		}
		out = append(out, items...)
	}
	return newestSamples(out, func(item ActionItem) int64 { return item.DetectedAt.Int64 }, limit), nil
}

// AIReplySamples returns the user's items of source whose replies the AI
// judged or will judge, newest check first: up to each items completed for
// reason, and up to each open or candidate items tracked and checked at
// least once, with a source key starting with one of prefixes. It reads
// without locking.
func (s *Store) AIReplySamples(ctx context.Context, userID, source string, prefixes []string, reason string, each int) ([]ActionItem, error) {
	var resolved, tracked []ActionItem
	for _, p := range prefixes {
		items, err := s.q.listAIResolvedSamples(ctx, userID, source, p, reason, each)
		if err != nil {
			return nil, err
		}
		resolved = append(resolved, items...)
		if items, err = s.q.listAITrackedSamples(ctx, userID, source, p, each); err != nil {
			return nil, err
		}
		tracked = append(tracked, items...)
	}
	checked := func(item ActionItem) int64 {
		if item.TrackedAt.Valid {
			return item.TrackedAt.Int64
		}
		return item.DetectedAt.Int64
	}
	out := append(newestSamples(resolved, checked, each), newestSamples(tracked, checked, each)...)
	return newestSamples(out, checked, len(out)), nil
}

// newestSamples sorts items by at, newest first, then by ID descending, and
// keeps the first limit.
func newestSamples(items []ActionItem, at func(ActionItem) int64, limit int) []ActionItem {
	sort.SliceStable(items, func(i, j int) bool {
		if a, b := at(items[i]), at(items[j]); a != b {
			return a > b
		}
		return items[i].ID > items[j].ID
	})
	if len(items) > limit {
		items = items[:limit]
	}
	return items
}
