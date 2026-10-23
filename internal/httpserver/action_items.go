package httpserver

import (
	"bytes"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/i2bskn/astovia/internal/auth"
	"github.com/i2bskn/astovia/internal/store"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const actionItemBodyLimit = 65536

var itemIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
var servicePattern = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)
var limitPattern = regexp.MustCompile(`^[1-9][0-9]{0,2}$`)
var cursorTimePattern = regexp.MustCompile(`^[0-9]+$`)

// actionItemRef is a reference to the conversation an item came from. It is
// both the response shape and the normalized form stored in refs.
type actionItemRef struct {
	Service        string `json:"service"`
	Account        string `json:"account,omitempty"`
	ConversationID string `json:"conversation_id,omitempty"`
	MessageID      string `json:"message_id,omitempty"`
	URL            string `json:"url,omitempty"`
}

// actionItemResponse is the public representation of an action item.
type actionItemResponse struct {
	ID        string          `json:"id"`
	Content   string          `json:"content"`
	Status    string          `json:"status"`
	Note      *string         `json:"note"`
	DueAt     *string         `json:"due_at"`
	Refs      []actionItemRef `json:"refs"`
	Version   int64           `json:"version"`
	CreatedAt string          `json:"created_at"`
	UpdatedAt string          `json:"updated_at"`
}

type actionItemListResponse struct {
	Items      []actionItemResponse `json:"items"`
	NextCursor *string              `json:"next_cursor"`
}

func formatTime(unix int64) string { return time.Unix(unix, 0).UTC().Format(time.RFC3339) }

func toActionItemResponse(item store.ActionItem) (actionItemResponse, error) {
	refs := []actionItemRef{}
	if err := json.Unmarshal([]byte(item.Refs), &refs); err != nil {
		return actionItemResponse{}, fmt.Errorf("stored refs of %s: %w", item.ID, err)
	}
	if refs == nil {
		refs = []actionItemRef{}
	}
	out := actionItemResponse{ID: item.ID, Content: item.Content, Status: item.Status, Refs: refs, Version: item.Version, CreatedAt: formatTime(item.CreatedAt), UpdatedAt: formatTime(item.UpdatedAt)}
	if item.Note.Valid {
		out.Note = &item.Note.String
	}
	if item.DueAt.Valid {
		v := formatTime(item.DueAt.Int64)
		out.DueAt = &v
	}
	return out, nil
}

// actionItemFields holds the writable fields of a request body. Each field is
// kept raw so that a missing key (nil) can be told apart from null.
type actionItemFields struct {
	Content json.RawMessage `json:"content"`
	Status  json.RawMessage `json:"status"`
	Note    json.RawMessage `json:"note"`
	DueAt   json.RawMessage `json:"due_at"`
	Refs    json.RawMessage `json:"refs"`
}

type actionItemPatch struct {
	actionItemFields
	Version json.RawMessage `json:"version"`
}

// actionItemRefInput uses pointers so that a missing key can be told apart
// from an empty string.
type actionItemRefInput struct {
	Service        *string `json:"service"`
	Account        *string `json:"account"`
	ConversationID *string `json:"conversation_id"`
	MessageID      *string `json:"message_id"`
	URL            *string `json:"url"`
}

func isNull(raw json.RawMessage) bool { return bytes.Equal(raw, []byte("null")) }

// decodeValue decodes one JSON value that decode has already checked as
// syntactically valid.
func decodeValue(raw json.RawMessage, out any) bool {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	return d.Decode(out) == nil
}

// decodeString decodes a JSON string without NUL characters, which
// PostgreSQL text cannot store.
func decodeString(raw json.RawMessage) (string, bool) {
	var v string
	if isNull(raw) || !decodeValue(raw, &v) || strings.ContainsRune(v, 0) {
		return "", false
	}
	return v, true
}

func validStatus(v string) bool { return v == "open" || v == "done" || v == "dismissed" }

// parse validates the fields present in the body. It returns a function that
// copies them to an item and the number of fields present, or a message for a
// 400 response.
func (in actionItemFields) parse() (func(*store.ActionItem), int, string) {
	var sets []func(*store.ActionItem)
	if in.Content != nil {
		v, ok := decodeString(in.Content)
		if !ok || strings.TrimSpace(v) == "" || utf8.RuneCountInString(v) > 500 {
			return nil, 0, "content must be 1-500 characters."
		}
		sets = append(sets, func(item *store.ActionItem) { item.Content = v })
	}
	if in.Status != nil {
		v, ok := decodeString(in.Status)
		if !ok || !validStatus(v) {
			return nil, 0, "status must be open, done or dismissed."
		}
		sets = append(sets, func(item *store.ActionItem) { item.Status = v })
	}
	if in.Note != nil {
		var note sql.NullString
		if !isNull(in.Note) {
			v, ok := decodeString(in.Note)
			if !ok || utf8.RuneCountInString(v) > 4000 {
				return nil, 0, "note must be null or at most 4000 characters."
			}
			note = sql.NullString{String: v, Valid: strings.TrimSpace(v) != ""}
		}
		sets = append(sets, func(item *store.ActionItem) { item.Note = note })
	}
	if in.DueAt != nil {
		var due sql.NullInt64
		if !isNull(in.DueAt) {
			v, ok := decodeString(in.DueAt)
			t, err := time.Parse(time.RFC3339, v)
			if !ok || err != nil || t.UTC().Year() < 1970 || t.UTC().Year() > 9999 {
				return nil, 0, "due_at must be null or an RFC 3339 date-time between 1970 and 9999."
			}
			due = sql.NullInt64{Int64: t.Unix(), Valid: true}
		}
		sets = append(sets, func(item *store.ActionItem) { item.DueAt = due })
	}
	if in.Refs != nil {
		refs, msg := parseRefs(in.Refs)
		if msg != "" {
			return nil, 0, msg
		}
		sets = append(sets, func(item *store.ActionItem) { item.Refs = refs })
	}
	return func(item *store.ActionItem) {
		for _, set := range sets {
			set(item)
		}
	}, len(sets), ""
}

// parseRefs validates refs and returns its normalized JSON.
func parseRefs(raw json.RawMessage) (string, string) {
	const invalid = "refs must be an array of at most 10 objects with a valid service and optional account, conversation_id, message_id and http(s) url."
	var in []actionItemRefInput
	if isNull(raw) || !decodeValue(raw, &in) || len(in) > 10 {
		return "", invalid
	}
	refs := make([]actionItemRef, 0, len(in))
	for _, r := range in {
		if r.Service == nil || !servicePattern.MatchString(*r.Service) {
			return "", invalid
		}
		ref := actionItemRef{Service: *r.Service}
		for _, f := range []struct {
			in  *string
			out *string
		}{{r.Account, &ref.Account}, {r.ConversationID, &ref.ConversationID}, {r.MessageID, &ref.MessageID}} {
			if f.in == nil {
				continue
			}
			if n := utf8.RuneCountInString(*f.in); n < 1 || n > 256 || strings.ContainsRune(*f.in, 0) {
				return "", invalid
			}
			*f.out = *f.in
		}
		if r.URL != nil {
			u, err := url.Parse(*r.URL)
			if len(*r.URL) > 2048 || strings.ContainsRune(*r.URL, 0) || err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				return "", invalid
			}
			ref.URL = *r.URL
		}
		refs = append(refs, ref)
	}
	b, err := json.Marshal(refs)
	if err != nil {
		return "", invalid
	}
	return string(b), ""
}

// apiError writes the response for a store error. Unexpected errors are
// logged without request bodies or tokens.
func (s *Server) apiError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrItemNotFound):
		fail(w, 404, "not_found", "Action item not found.")
	case errors.Is(err, store.ErrVersionConflict):
		fail(w, 409, "conflict", "The action item was updated by another request.")
	default:
		log.Printf("api: %s %s: %v", r.Method, r.URL.Path, err)
		fail(w, 503, "unavailable", "The service is temporarily unavailable. Try again.")
	}
}

func (s *Server) writeActionItem(w http.ResponseWriter, r *http.Request, status int, item store.ActionItem) {
	out, err := toActionItemResponse(item)
	if err != nil {
		s.apiError(w, r, err)
		return
	}
	if status == 201 {
		w.Header().Set("Location", "/api/v1/action-items/"+item.ID)
	}
	writeJSON(w, status, out)
}

func encodeCursor(item store.ActionItem) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(item.CreatedAt, 10) + ":" + item.ID))
}
func decodeCursor(v string) (*store.ActionItemCursor, bool) {
	b, err := base64.RawURLEncoding.DecodeString(v)
	if err != nil {
		return nil, false
	}
	created, id, ok := strings.Cut(string(b), ":")
	if !ok || !cursorTimePattern.MatchString(created) || !itemIDPattern.MatchString(id) {
		return nil, false
	}
	n, err := strconv.ParseInt(created, 10, 64)
	if err != nil {
		return nil, false
	}
	return &store.ActionItemCursor{CreatedAt: n, ID: id}, true
}

func (s *Server) handleListActionItems(w http.ResponseWriter, r *http.Request) {
	u, ok := userFrom(r.Context())
	if !ok {
		s.authError(w, r, store.ErrUnauthorized)
		return
	}
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		fail(w, 400, "invalid_request", "Invalid query string.")
		return
	}
	for k, v := range q {
		if (k != "status" && k != "limit" && k != "cursor") || len(v) != 1 {
			fail(w, 400, "invalid_request", "Only status, limit and cursor are allowed, each at most once.")
			return
		}
	}
	status := ""
	if v, ok := q["status"]; ok {
		if !validStatus(v[0]) {
			fail(w, 400, "invalid_request", "status must be open, done or dismissed.")
			return
		}
		status = v[0]
	}
	limit := 50
	if v, ok := q["limit"]; ok {
		n, err := strconv.Atoi(v[0])
		if !limitPattern.MatchString(v[0]) || err != nil || n > 100 {
			fail(w, 400, "invalid_request", "limit must be an integer from 1 to 100.")
			return
		}
		limit = n
	}
	var after *store.ActionItemCursor
	if v, ok := q["cursor"]; ok {
		if after, ok = decodeCursor(v[0]); !ok {
			fail(w, 400, "invalid_request", "cursor is invalid.")
			return
		}
	}
	items, err := s.auth.Store.ListActionItems(r.Context(), u.ID, status, after, limit+1)
	if err != nil {
		s.apiError(w, r, err)
		return
	}
	out := actionItemListResponse{Items: []actionItemResponse{}}
	if len(items) > limit {
		items = items[:limit]
		next := encodeCursor(items[limit-1])
		out.NextCursor = &next
	}
	for _, item := range items {
		v, err := toActionItemResponse(item)
		if err != nil {
			s.apiError(w, r, err)
			return
		}
		out.Items = append(out.Items, v)
	}
	writeJSON(w, 200, out)
}

func (s *Server) handleCreateActionItem(w http.ResponseWriter, r *http.Request) {
	u, ok := userFrom(r.Context())
	if !ok {
		s.authError(w, r, store.ErrUnauthorized)
		return
	}
	var in actionItemFields
	if !decode(w, r, &in, actionItemBodyLimit) {
		return
	}
	if in.Content == nil {
		fail(w, 400, "invalid_request", "content is required.")
		return
	}
	apply, _, msg := in.parse()
	if msg != "" {
		fail(w, 400, "invalid_request", msg)
		return
	}
	id, err := auth.Random()
	if err != nil {
		s.apiError(w, r, err)
		return
	}
	now := s.auth.Now().Unix()
	item := store.ActionItem{ID: id, UserID: u.ID, Status: "open", Refs: "[]", Version: 1, CreatedAt: now, UpdatedAt: now}
	apply(&item)
	if err = s.auth.Store.CreateActionItem(r.Context(), item); err != nil {
		s.apiError(w, r, err)
		return
	}
	s.writeActionItem(w, r, 201, item)
}

func (s *Server) handleGetActionItem(w http.ResponseWriter, r *http.Request) {
	u, ok := userFrom(r.Context())
	if !ok {
		s.authError(w, r, store.ErrUnauthorized)
		return
	}
	id := r.PathValue("id")
	if !itemIDPattern.MatchString(id) {
		s.apiError(w, r, store.ErrItemNotFound)
		return
	}
	item, err := s.auth.Store.ActionItem(r.Context(), u.ID, id)
	if err != nil {
		s.apiError(w, r, err)
		return
	}
	s.writeActionItem(w, r, 200, item)
}

func (s *Server) handleUpdateActionItem(w http.ResponseWriter, r *http.Request) {
	u, ok := userFrom(r.Context())
	if !ok {
		s.authError(w, r, store.ErrUnauthorized)
		return
	}
	id := r.PathValue("id")
	if !itemIDPattern.MatchString(id) {
		s.apiError(w, r, store.ErrItemNotFound)
		return
	}
	var in actionItemPatch
	if !decode(w, r, &in, actionItemBodyLimit) {
		return
	}
	var version int64
	if in.Version != nil {
		if isNull(in.Version) || !decodeValue(in.Version, &version) || version < 1 {
			fail(w, 400, "invalid_request", "version must be a positive integer.")
			return
		}
	}
	apply, n, msg := in.parse()
	if msg != "" {
		fail(w, 400, "invalid_request", msg)
		return
	}
	if n == 0 {
		fail(w, 400, "invalid_request", "At least one of content, status, note, due_at or refs is required.")
		return
	}
	now := s.auth.Now().Unix()
	item, err := s.auth.Store.UpdateActionItem(r.Context(), u.ID, id, func(item *store.ActionItem) error {
		if in.Version != nil && item.Version != version {
			return store.ErrVersionConflict
		}
		apply(item)
		item.UpdatedAt = now
		return nil
	})
	if err != nil {
		s.apiError(w, r, err)
		return
	}
	s.writeActionItem(w, r, 200, item)
}
