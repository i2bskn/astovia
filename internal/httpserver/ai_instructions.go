package httpserver

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/i2bskn/astovia/internal/store"
)

// instructionsBodyLimit is the body limit of the instruction requests: eight
// instructions of MaxInstructionLength runes of up to four bytes each, and
// room for the JSON around them.
const instructionsBodyLimit = 128 << 10

// aiInstructionsResponse is the user's AI instructions with every scope of
// store.InstructionScopes; a scope without an instruction is "".
type aiInstructionsResponse struct {
	Instructions map[string]string `json:"instructions"`
}

func toAIInstructionsResponse(m map[string]string) aiInstructionsResponse {
	out := make(map[string]string, len(store.InstructionScopes))
	for _, scope := range store.InstructionScopes {
		out[scope] = m[scope]
	}
	return aiInstructionsResponse{Instructions: out}
}

func (s *Server) handleGetAIInstructions(w http.ResponseWriter, r *http.Request) {
	u, ok := userFrom(r.Context())
	if !ok {
		s.authError(w, r, store.ErrUnauthorized)
		return
	}
	m, err := s.auth.Store.AIInstructions(r.Context(), u.ID)
	if err != nil {
		integrationError(w, "ai instructions get", u.ID, err)
		return
	}
	writeJSON(w, 200, toAIInstructionsResponse(m))
}

// handlePutAIInstructions replaces every instruction of the user. The body
// must carry every scope as a string; an empty one (after trimming the
// surrounding white space) removes the instruction.
func (s *Server) handlePutAIInstructions(w http.ResponseWriter, r *http.Request) {
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
		Instructions json.RawMessage `json:"instructions"`
	}
	if !decode(w, r, &in, instructionsBodyLimit) {
		return
	}
	var raw map[string]json.RawMessage
	if in.Instructions == nil || isNull(in.Instructions) || !decodeValue(in.Instructions, &raw) || raw == nil {
		fail(w, 400, "invalid_request", "instructions must be an object.")
		return
	}
	for key := range raw {
		if !store.ValidInstructionScope(key) {
			fail(w, 400, "invalid_request", "instructions has an unknown scope.")
			return
		}
	}
	values := make(map[string]string, len(store.InstructionScopes))
	for _, scope := range store.InstructionScopes {
		v, found := raw[scope]
		var text string
		if found {
			text, ok = decodeString(v)
		}
		if !found || !ok {
			fail(w, 400, "invalid_request", fmt.Sprintf("instruction for %s must be a string.", scope))
			return
		}
		text = strings.TrimSpace(text)
		if utf8.RuneCountInString(text) > store.MaxInstructionLength {
			fail(w, 400, "invalid_request", fmt.Sprintf("instruction for %s must be at most %d characters.", scope, store.MaxInstructionLength))
			return
		}
		values[scope] = text
	}
	err := s.auth.Store.SaveAIInstructions(r.Context(), u.ID, sid, values, s.auth.Now().Unix())
	switch {
	case err == nil:
	case errors.Is(err, store.ErrUnauthorized):
		s.authError(w, r, err)
		return
	default:
		integrationError(w, "ai instructions put", u.ID, err)
		return
	}
	writeJSON(w, 200, toAIInstructionsResponse(values))
}
