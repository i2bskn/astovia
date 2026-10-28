package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/i2bskn/astovia/internal/oauth"
	"github.com/i2bskn/astovia/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"net/http"
	"strings"
)

// mcpInstructions is sent to MCP clients on initialize.
const mcpInstructions = "Astovia keeps the user's action items: small things to do that come from conversations (replies, schedules, promises). Use action_items_list to see open items, action_items_create to save a new one with a reference to the source conversation, and action_items_update to change status (open, candidate, done, dismissed) or details. candidate is an unconfirmed candidate found by the AI. Do not invent facts that are not in the source conversation."

// mcpBodyLimit bounds a single /mcp request body.
const mcpBodyLimit = 262144

// mcpHandler serves the stateless Streamable HTTP transport. It must be
// wrapped with mcpAuth.
func (s *Server) mcpHandler() http.Handler {
	h := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		// The SDK may call this more than once per request (protocol version
		// check and stateless handling), so it must not have side effects.
		// Stateless mode builds a new session for every request with the
		// server returned here, binding the tools to this request's user.
		u, ok := userFrom(r.Context())
		if !ok || u.ID == "" {
			return nil
		}
		return s.newMCPServer(u.ID)
	}, &mcp.StreamableHTTPOptions{
		Stateless:           true,
		JSONResponse:        true,
		MaxRequestBodyBytes: mcpBodyLimit,
		// /mcp requires a Bearer token and never reads cookies, so a page
		// reached through DNS rebinding has no credential to pass the check.
		// Requests with an Origin header (from browsers) are rejected by
		// mcpAuth unless it equals PublicURL. The SDK's own protection would
		// reject every request behind a same-host reverse proxy that keeps
		// the public Host header, so it is disabled.
		DisableLocalhostProtection: true,
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(&noStoreWriter{ResponseWriter: w}, r)
	})
}

// noStoreWriter restores the Cache-Control: no-store set by secureHeaders,
// which the SDK replaces with "no-cache, no-transform", so that responses
// carrying the user's data are never stored by caches.
type noStoreWriter struct {
	http.ResponseWriter
	wroteHeader bool
}

func (w *noStoreWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.wroteHeader = true
		w.Header().Set("Cache-Control", "no-store")
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *noStoreWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

// FlushError is used by http.ResponseController.
func (w *noStoreWriter) FlushError() error {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return http.NewResponseController(w.ResponseWriter).Flush()
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *noStoreWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// mcpAuth guards /mcp: it rejects foreign browser origins and requires an
// API token ("ast_") or an OAuth access token ("aso_"). Access JWTs are not
// accepted here.
func (s *Server) mcpAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// MCP clients outside a browser do not send Origin. When it is
		// present it must be this server's public origin.
		if origin, present := r.Header["Origin"]; present && (len(origin) != 1 || origin[0] != s.cfg.PublicURL) {
			fail(w, 403, "origin_rejected", "Request origin is not allowed.")
			return
		}
		token, ok := bearerToken(r)
		if !ok {
			s.mcpAuthError(w, r, store.ErrUnauthorized, false)
			return
		}
		var u store.User
		var err error
		switch {
		case strings.HasPrefix(token, "ast_"):
			u, err = s.auth.AuthenticateAPIToken(r.Context(), token)
		case strings.HasPrefix(token, "aso_"):
			u, err = s.auth.AuthenticateOAuthAccess(r.Context(), token)
		default:
			err = store.ErrUnauthorized
		}
		if err != nil {
			s.mcpAuthError(w, r, err, true)
			return
		}
		next.ServeHTTP(w, withSession(r, u, ""))
	})
}

// mcpAuthError is authError with the WWW-Authenticate challenge on 401. The
// challenge points to the protected resource metadata (RFC 9728) and the
// scope to request, which MCP clients use to start the OAuth flow. Following
// RFC 6750 section 3.1, the invalid_token error code is added only when a
// bearer token was presented.
func (s *Server) mcpAuthError(w http.ResponseWriter, r *http.Request, err error, presented bool) {
	if errors.Is(err, store.ErrUnauthorized) {
		challenge := `Bearer resource_metadata="` + s.cfg.PublicURL + protectedResourcePath + `", scope="` + oauth.ScopeMCP + `"`
		if presented {
			challenge = `Bearer error="invalid_token", resource_metadata="` + s.cfg.PublicURL + protectedResourcePath + `", scope="` + oauth.ScopeMCP + `"`
		}
		w.Header().Set("WWW-Authenticate", challenge)
	}
	s.authError(w, r, err)
}

// newMCPServer returns an MCP server whose tools act only on userID's data.
func (s *Server) newMCPServer(userID string) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "astovia", Version: "0.1.0"}, &mcp.ServerOptions{Instructions: mcpInstructions})
	f := false
	server.AddTool(&mcp.Tool{
		Name:        "action_items_create",
		Description: "Save a new action item for the user. Include refs to the source conversation when known. Give parent_id to make it a subtask; items nest at most two levels.",
		InputSchema: objectSchema([]string{"content"}, map[string]any{
			"content":   stringProp("Short description of what needs to be done (1-500 characters)."),
			"status":    statusProp(),
			"note":      nullableStringProp("Optional free-form note (up to 4000 characters)."),
			"due_at":    dueAtProp(),
			"refs":      refsProp(),
			"parent_id": nullableStringProp("Optional ID of the parent item, which must have no parent itself (two levels at most). null means no parent."),
		}),
		Annotations: &mcp.ToolAnnotations{DestructiveHint: &f, OpenWorldHint: &f},
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var in actionItemFields
		if !decodeArguments(req, &in) {
			return toolError(invalidArguments), nil
		}
		// null parent_id means no parent, as when it is omitted.
		if isNull(in.ParentID) {
			in.ParentID = nil
		}
		out, e := s.createActionItem(ctx, userID, in)
		return toolResult(out, e), nil
	})
	server.AddTool(&mcp.Tool{
		Name:        "action_items_list",
		Description: "List the user's action items, newest first. Use next_cursor to get the next page. With parent_id, list the subtasks of that item instead, oldest first (items nest at most two levels).",
		InputSchema: objectSchema(nil, map[string]any{
			"status":    map[string]any{"type": []string{"string", "null"}, "enum": []any{"open", "candidate", "done", "dismissed", nil}, "description": "open, candidate (an unconfirmed candidate found by the AI), done or dismissed."},
			"limit":     map[string]any{"type": []string{"integer", "null"}, "minimum": 1, "maximum": 100, "description": "Maximum number of items (1-100, default 50)."},
			"cursor":    map[string]any{"type": []string{"string", "null"}, "description": "next_cursor from a previous call."},
			"parent_id": map[string]any{"type": []string{"string", "null"}, "description": "List the subtasks of this item (up to 100, no paging). Cannot be combined with status, limit or cursor."},
		}),
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: &f},
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var in struct {
			Status   json.RawMessage `json:"status"`
			Limit    json.RawMessage `json:"limit"`
			Cursor   json.RawMessage `json:"cursor"`
			ParentID json.RawMessage `json:"parent_id"`
		}
		if !decodeArguments(req, &in) {
			return toolError(invalidArguments), nil
		}
		// null is treated as omitted, as for the other arguments below.
		if in.ParentID != nil && !isNull(in.ParentID) {
			for _, raw := range []json.RawMessage{in.Status, in.Limit, in.Cursor} {
				if raw != nil && !isNull(raw) {
					return toolError(invalidRequest("parent_id cannot be combined with status, limit or cursor.")), nil
				}
			}
			v, ok := decodeString(in.ParentID)
			if !ok {
				return toolError(invalidRequest(parentMessage)), nil
			}
			out, e := s.listChildActionItems(ctx, userID, v)
			return toolResult(out, e), nil
		}
		// null is treated as omitted: clients often send null for optional
		// arguments.
		status := ""
		if in.Status != nil && !isNull(in.Status) {
			v, ok := decodeString(in.Status)
			if !ok || !validStatus(v) {
				return toolError(invalidRequest(statusMessage)), nil
			}
			status = v
		}
		limit := 50
		if in.Limit != nil && !isNull(in.Limit) {
			// Only JSON integers: 50.5, 5e1 and "50" are rejected.
			var n int
			if !decodeValue(in.Limit, &n) {
				return toolError(invalidRequest(limitMessage)), nil
			}
			limit = n
		}
		cursor := ""
		if in.Cursor != nil && !isNull(in.Cursor) {
			v, ok := decodeString(in.Cursor)
			if !ok || v == "" {
				return toolError(invalidRequest(cursorMessage)), nil
			}
			cursor = v
		}
		out, e := s.listActionItems(ctx, userID, status, limit, cursor)
		return toolResult(out, e), nil
	})
	server.AddTool(&mcp.Tool{
		Name:        "action_items_get",
		Description: "Get one of the user's action items by ID.",
		InputSchema: objectSchema([]string{"id"}, map[string]any{
			"id": stringProp("Action item ID."),
		}),
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: &f},
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var in struct {
			ID json.RawMessage `json:"id"`
		}
		if !decodeArguments(req, &in) {
			return toolError(invalidArguments), nil
		}
		id, e := toolItemID(in.ID)
		if e != nil {
			return toolError(e), nil
		}
		out, e := s.getActionItem(ctx, userID, id)
		return toolResult(out, e), nil
	})
	server.AddTool(&mcp.Tool{
		Name:        "action_items_update",
		Description: "Update one of the user's action items: change its status (open, candidate, done, dismissed; candidate is an unconfirmed candidate found by the AI), details or parent. Only the given fields change. Items nest at most two levels.",
		InputSchema: objectSchema([]string{"id"}, map[string]any{
			"id":        stringProp("Action item ID."),
			"content":   stringProp("Short description of what needs to be done (1-500 characters)."),
			"status":    statusProp(),
			"note":      nullableStringProp("Optional free-form note (up to 4000 characters). null clears it."),
			"due_at":    dueAtProp(),
			"refs":      refsProp(),
			"parent_id": nullableStringProp("ID of the parent item, which must have no parent itself; an item with subtasks cannot get a parent. null removes the parent."),
			"version":   map[string]any{"type": "integer", "minimum": 1, "description": "If given, the update fails when the item's current version differs."},
		}),
		Annotations: &mcp.ToolAnnotations{DestructiveHint: &f, IdempotentHint: false, OpenWorldHint: &f},
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var in struct {
			ID json.RawMessage `json:"id"`
			actionItemPatch
		}
		if !decodeArguments(req, &in) {
			return toolError(invalidArguments), nil
		}
		id, e := toolItemID(in.ID)
		if e != nil {
			return toolError(e), nil
		}
		out, e := s.updateActionItem(ctx, userID, id, in.actionItemPatch)
		return toolResult(out, e), nil
	})
	return server
}

var invalidArguments = invalidRequest("Invalid arguments.")

// decodeArguments decodes the tool arguments, a JSON object, into out.
// Missing, empty or null arguments are treated as {}. Unknown keys and values
// that do not fit out are rejected.
func decodeArguments(req *mcp.CallToolRequest, out any) bool {
	var raw json.RawMessage
	if req.Params != nil {
		raw = bytes.TrimSpace(req.Params.Arguments)
	}
	if len(raw) == 0 || isNull(raw) {
		raw = json.RawMessage("{}")
	}
	if raw[0] != '{' {
		return false
	}
	return decodeValue(raw, out)
}

// toolItemID returns the id argument. A missing key is an invalid request;
// any other unusable value is reported like a missing item, as REST does for
// a malformed path.
func toolItemID(raw json.RawMessage) (string, *opError) {
	if raw == nil {
		return "", invalidRequest("id is required.")
	}
	var id string
	if isNull(raw) || !decodeValue(raw, &id) || !itemIDPattern.MatchString(id) {
		return "", errActionItemNotFound
	}
	return id, nil
}

// toolResult returns out as structured and text content, or e as an error
// result.
func toolResult(out any, e *opError) *mcp.CallToolResult {
	if e != nil {
		return toolError(e)
	}
	b, err := json.Marshal(out)
	if err != nil {
		return toolError(storeError("encode", "", err))
	}
	return &mcp.CallToolResult{StructuredContent: out, Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}
}

// toolError reports e to the model as a tool result rather than a protocol
// error, so that it can correct the call.
func toolError(e *opError) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: e.code + ": " + e.message}}}
}

func objectSchema(required []string, properties map[string]any) map[string]any {
	schema := map[string]any{"type": "object", "additionalProperties": false, "properties": properties}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

func stringProp(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

func nullableStringProp(description string) map[string]any {
	return map[string]any{"type": []string{"string", "null"}, "description": description}
}

func statusProp() map[string]any {
	return map[string]any{"type": "string", "enum": []string{"open", "candidate", "done", "dismissed"}, "description": "open, candidate (an unconfirmed candidate found by the AI), done or dismissed."}
}

func dueAtProp() map[string]any {
	return map[string]any{"type": []string{"string", "null"}, "format": "date-time", "description": "Optional due date-time in RFC 3339, e.g. 2026-10-24T13:00:00+09:00. null clears it."}
}

func refsProp() map[string]any {
	return map[string]any{
		"type":        "array",
		"maxItems":    10,
		"description": "References to the source conversation (up to 10). Each needs service (e.g. gmail, slack) and may have account, conversation_id, conversation_name (a readable name such as #general, shown instead of the account), message_id and url (http or https).",
		"items": objectSchema([]string{"service"}, map[string]any{
			"service":           map[string]any{"type": "string"},
			"account":           map[string]any{"type": "string"},
			"conversation_id":   map[string]any{"type": "string"},
			"conversation_name": map[string]any{"type": "string"},
			"message_id":        map[string]any{"type": "string"},
			"url":               map[string]any{"type": "string"},
		}),
	}
}
