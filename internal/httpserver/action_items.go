package httpserver

import (
	"bytes"
	"context"
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
	// ConversationName is a readable name of the conversation for display,
	// such as #general.
	ConversationName string `json:"conversation_name,omitempty"`
	MessageID        string `json:"message_id,omitempty"`
	URL              string `json:"url,omitempty"`
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
	// Source is the integration that created the item, or null. Auto
	// describes an automatic completion, or is null. The source key and the
	// tracking flag are internal and not returned.
	Source *string             `json:"source"`
	Auto   *actionItemAutoInfo `json:"auto"`
	// Kind is "task" or "event". Event describes the meeting of an event
	// item, and is null for other kinds.
	Kind  string           `json:"kind"`
	Event *actionItemEvent `json:"event"`
	// ParentID is the item this one is a subtask of, or null; Parent shows
	// that item. Children counts the subtasks, or is null without any.
	ParentID *string             `json:"parent_id"`
	Parent   *actionItemParent   `json:"parent"`
	Children *actionItemChildren `json:"children"`
}

// actionItemParent is the parent of a subtask.
type actionItemParent struct {
	ID      string `json:"id"`
	Content string `json:"content"`
	Status  string `json:"status"`
}

// actionItemChildren counts the subtasks of a parent. Total leaves out the
// dismissed ones; Remaining counts the open and candidate ones. NextDueAt is
// the earliest due date of the open and candidate ones, or null.
type actionItemChildren struct {
	Total     int64   `json:"total"`
	Done      int64   `json:"done"`
	Remaining int64   `json:"remaining"`
	NextDueAt *string `json:"next_due_at"`
}

// actionItemEvent is the meeting of an event item. EndAt is null when no end
// was given.
type actionItemEvent struct {
	Title   string  `json:"title"`
	StartAt string  `json:"start_at"`
	EndAt   *string `json:"end_at"`
}

// actionItemAutoInfo is why and when an item was completed automatically.
// RefURL is the evidence on the source service, when known.
type actionItemAutoInfo struct {
	Reason string  `json:"reason"`
	RefURL *string `json:"ref_url"`
	At     string  `json:"at"`
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
	if item.Source.Valid {
		out.Source = &item.Source.String
	}
	if item.AutoReason.Valid {
		auto := &actionItemAutoInfo{Reason: item.AutoReason.String, At: formatTime(item.UpdatedAt)}
		if item.AutoAt.Valid {
			auto.At = formatTime(item.AutoAt.Int64)
		}
		if item.AutoRef.Valid {
			auto.RefURL = &item.AutoRef.String
		}
		out.Auto = auto
	}
	out.Kind = store.KindTask
	if item.Kind == store.KindEvent && item.EventTitle.Valid && item.EventStartAt.Valid {
		out.Kind = store.KindEvent
		event := &actionItemEvent{Title: item.EventTitle.String, StartAt: formatTime(item.EventStartAt.Int64)}
		if item.EventEndAt.Valid {
			v := formatTime(item.EventEndAt.Int64)
			event.EndAt = &v
		}
		out.Event = event
	}
	if item.ParentID.Valid {
		out.ParentID = &item.ParentID.String
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
	// ParentID is the parent's ID, or null for no parent.
	ParentID json.RawMessage `json:"parent_id"`
}

type actionItemPatch struct {
	actionItemFields
	Version json.RawMessage `json:"version"`
}

// actionItemRefInput uses pointers so that a missing key can be told apart
// from an empty string.
type actionItemRefInput struct {
	Service          *string `json:"service"`
	Account          *string `json:"account"`
	ConversationID   *string `json:"conversation_id"`
	ConversationName *string `json:"conversation_name"`
	MessageID        *string `json:"message_id"`
	URL              *string `json:"url"`
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

func validStatus(v string) bool {
	return v == "open" || v == "candidate" || v == "done" || v == "dismissed"
}

// activeStatus reports whether the status is one still to be handled: open,
// or a candidate the user has not sorted yet.
func activeStatus(v string) bool { return v == "open" || v == "candidate" }

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
			return nil, 0, statusMessage
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
	if in.ParentID != nil {
		var parent sql.NullString
		if !isNull(in.ParentID) {
			v, ok := decodeString(in.ParentID)
			if !ok || !itemIDPattern.MatchString(v) {
				return nil, 0, parentMessage
			}
			parent = sql.NullString{String: v, Valid: true}
		}
		sets = append(sets, func(item *store.ActionItem) { item.ParentID = parent })
	}
	return func(item *store.ActionItem) {
		for _, set := range sets {
			set(item)
		}
	}, len(sets), ""
}

// parseRefs validates refs and returns its normalized JSON.
func parseRefs(raw json.RawMessage) (string, string) {
	const invalid = "refs must be an array of at most 10 objects with a valid service and optional account, conversation_id, conversation_name, message_id and http(s) url."
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
		}{{r.Account, &ref.Account}, {r.ConversationID, &ref.ConversationID}, {r.ConversationName, &ref.ConversationName}, {r.MessageID, &ref.MessageID}} {
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

// opError is the failure of an action item operation. REST writes it as a
// JSON error response and MCP as a tool result with isError set.
type opError struct {
	status        int
	code, message string
}

func invalidRequest(message string) *opError {
	return &opError{status: 400, code: "invalid_request", message: message}
}

var errActionItemNotFound = &opError{status: 404, code: "not_found", message: "Action item not found."}

// failOp writes e as a JSON error response.
func failOp(w http.ResponseWriter, e *opError) {
	fail(w, e.status, e.code, e.message)
}

// storeError converts an error from the store (or from building a response)
// to an opError. Unexpected errors are logged here, and only here, without
// request bodies or tokens.
func storeError(op, userID string, err error) *opError {
	switch {
	case errors.Is(err, store.ErrItemNotFound):
		return errActionItemNotFound
	case errors.Is(err, store.ErrVersionConflict):
		return &opError{status: 409, code: "conflict", message: "The action item was updated by another request."}
	case errors.Is(err, store.ErrInvalidParent):
		return invalidRequest(parentMessage)
	default:
		log.Printf("action items: %s user=%s: %v", op, userID, err)
		return &opError{status: 503, code: "unavailable", message: "The service is temporarily unavailable. Try again."}
	}
}

// itemResponse converts a stored item, reporting a failure as storeError does.
func (s *Server) itemResponse(ctx context.Context, op, userID string, item store.ActionItem) (actionItemResponse, *opError) {
	out, e := s.itemResponses(ctx, op, userID, []store.ActionItem{item})
	if e != nil {
		return actionItemResponse{}, e
	}
	return out[0], nil
}

// itemResponses converts stored items with their parents and subtask
// counts, read with one query each for all the items.
func (s *Server) itemResponses(ctx context.Context, op, userID string, items []store.ActionItem) ([]actionItemResponse, *opError) {
	ids := make([]string, 0, len(items))
	var parentIDs []string
	for _, item := range items {
		ids = append(ids, item.ID)
		if item.ParentID.Valid {
			parentIDs = append(parentIDs, item.ParentID.String)
		}
	}
	counts, err := s.auth.Store.ChildCounts(ctx, userID, ids)
	if err != nil {
		return nil, storeError(op, userID, err)
	}
	parents, err := s.auth.Store.ItemSummaries(ctx, userID, parentIDs)
	if err != nil {
		return nil, storeError(op, userID, err)
	}
	out := make([]actionItemResponse, 0, len(items))
	for _, item := range items {
		v, err := toActionItemResponse(item)
		if err != nil {
			return nil, storeError(op, userID, err)
		}
		if p, ok := parents[item.ParentID.String]; ok && item.ParentID.Valid {
			v.Parent = &actionItemParent{ID: p.ID, Content: p.Content, Status: p.Status}
		}
		if c, ok := counts[item.ID]; ok && c.Children > 0 {
			v.Children = &actionItemChildren{Total: c.Total, Done: c.Done, Remaining: c.Remaining}
			if c.NextDueAt.Valid {
				next := formatTime(c.NextDueAt.Int64)
				v.Children.NextDueAt = &next
			}
		}
		out = append(out, v)
	}
	return out, nil
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

// autoCursorPrefix marks cursors of the auto=true list, so that the two kinds
// of cursor cannot be mixed.
const autoCursorPrefix = "a."

func encodeAutoCursor(item store.ActionItem) string {
	return autoCursorPrefix + base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(item.AutoAt.Int64, 10)+":"+item.ID))
}
func decodeAutoCursor(v string) (*store.AutoActionItemCursor, bool) {
	raw, ok := strings.CutPrefix(v, autoCursorPrefix)
	if !ok {
		return nil, false
	}
	c, ok := decodeCursor(raw)
	if !ok {
		return nil, false
	}
	return &store.AutoActionItemCursor{AutoAt: c.CreatedAt, ID: c.ID}, true
}

const (
	parentMessage = "parent_id must be the id of your item that has no parent, and an item with subtasks cannot have a parent."
	statusMessage = "status must be open, candidate, done or dismissed."
	limitMessage  = "limit must be an integer from 1 to 100."
	cursorMessage = "cursor is invalid."
)

// listActionItems lists the user's items. An empty status lists every status
// and an empty cursor starts from the newest item; callers reject empty values
// given explicitly before calling.
func (s *Server) listActionItems(ctx context.Context, userID, status string, limit int, cursor string) (actionItemListResponse, *opError) {
	return s.listCreatedItems(ctx, userID, status, limit, cursor, s.auth.Store.ListActionItems)
}

// listTopActionItems is listActionItems for view=top: the items without a
// parent, and the subtasks whose parent has another status than the one
// listed. The cursors are the same as those of listActionItems.
func (s *Server) listTopActionItems(ctx context.Context, userID, status string, limit int, cursor string) (actionItemListResponse, *opError) {
	return s.listCreatedItems(ctx, userID, status, limit, cursor, s.auth.Store.ListTopActionItems)
}

// listCreatedItems lists the items read by fetch, newest created first.
func (s *Server) listCreatedItems(ctx context.Context, userID, status string, limit int, cursor string, fetch func(context.Context, string, string, *store.ActionItemCursor, int) ([]store.ActionItem, error)) (actionItemListResponse, *opError) {
	if status != "" && !validStatus(status) {
		return actionItemListResponse{}, invalidRequest(statusMessage)
	}
	if limit < 1 || limit > 100 {
		return actionItemListResponse{}, invalidRequest(limitMessage)
	}
	var after *store.ActionItemCursor
	if cursor != "" {
		var ok bool
		if after, ok = decodeCursor(cursor); !ok {
			return actionItemListResponse{}, invalidRequest(cursorMessage)
		}
	}
	items, err := fetch(ctx, userID, status, after, limit+1)
	if err != nil {
		return actionItemListResponse{}, storeError("list", userID, err)
	}
	out := actionItemListResponse{Items: []actionItemResponse{}}
	if len(items) > limit {
		items = items[:limit]
		next := encodeCursor(items[limit-1])
		out.NextCursor = &next
	}
	list, e := s.itemResponses(ctx, "list", userID, items)
	if e != nil {
		return actionItemListResponse{}, e
	}
	out.Items = append(out.Items, list...)
	return out, nil
}

// listAutoActionItems lists the user's automatically completed items, newest
// completion first. Its cursors start with autoCursorPrefix.
func (s *Server) listAutoActionItems(ctx context.Context, userID, status string, limit int, cursor string) (actionItemListResponse, *opError) {
	if status != "" && !validStatus(status) {
		return actionItemListResponse{}, invalidRequest(statusMessage)
	}
	if limit < 1 || limit > 100 {
		return actionItemListResponse{}, invalidRequest(limitMessage)
	}
	var after *store.AutoActionItemCursor
	if cursor != "" {
		var ok bool
		if after, ok = decodeAutoCursor(cursor); !ok {
			return actionItemListResponse{}, invalidRequest(cursorMessage)
		}
	}
	items, err := s.auth.Store.ListAutoActionItems(ctx, userID, status, after, limit+1)
	if err != nil {
		return actionItemListResponse{}, storeError("list", userID, err)
	}
	out := actionItemListResponse{Items: []actionItemResponse{}}
	if len(items) > limit {
		items = items[:limit]
		next := encodeAutoCursor(items[limit-1])
		out.NextCursor = &next
	}
	list, e := s.itemResponses(ctx, "list", userID, items)
	if e != nil {
		return actionItemListResponse{}, e
	}
	out.Items = append(out.Items, list...)
	return out, nil
}

// childListLimit is the most subtasks listed for one parent.
const childListLimit = 100

// listChildActionItems lists the subtasks of the user's parent, oldest
// first, without paging. Another user's or a missing parent has none.
func (s *Server) listChildActionItems(ctx context.Context, userID, parentID string) (actionItemListResponse, *opError) {
	if !itemIDPattern.MatchString(parentID) {
		return actionItemListResponse{}, invalidRequest(parentMessage)
	}
	items, err := s.auth.Store.ChildActionItems(ctx, userID, parentID, childListLimit)
	if err != nil {
		return actionItemListResponse{}, storeError("list", userID, err)
	}
	out := actionItemListResponse{Items: []actionItemResponse{}}
	list, e := s.itemResponses(ctx, "list", userID, items)
	if e != nil {
		return actionItemListResponse{}, e
	}
	out.Items = append(out.Items, list...)
	return out, nil
}

// createActionItem validates body and stores a new item for the user.
func (s *Server) createActionItem(ctx context.Context, userID string, body actionItemFields) (actionItemResponse, *opError) {
	if body.Content == nil {
		return actionItemResponse{}, invalidRequest("content is required.")
	}
	apply, _, msg := body.parse()
	if msg != "" {
		return actionItemResponse{}, invalidRequest(msg)
	}
	id, err := auth.Random()
	if err != nil {
		return actionItemResponse{}, storeError("create", userID, err)
	}
	now := s.auth.Now().Unix()
	item := store.ActionItem{ID: id, UserID: userID, Status: "open", Refs: "[]", Version: 1, CreatedAt: now, UpdatedAt: now, Kind: store.KindTask}
	apply(&item)
	if err = s.auth.Store.CreateActionItem(ctx, item); err != nil {
		return actionItemResponse{}, storeError("create", userID, err)
	}
	return s.itemResponse(ctx, "create", userID, item)
}

// getActionItem returns the user's item. A malformed ID is reported as not
// found, the same as another user's item.
func (s *Server) getActionItem(ctx context.Context, userID, id string) (actionItemResponse, *opError) {
	if !itemIDPattern.MatchString(id) {
		return actionItemResponse{}, errActionItemNotFound
	}
	item, err := s.auth.Store.ActionItem(ctx, userID, id)
	if err != nil {
		return actionItemResponse{}, storeError("get", userID, err)
	}
	return s.itemResponse(ctx, "get", userID, item)
}

// updateActionItem validates body and applies it to the user's item.
func (s *Server) updateActionItem(ctx context.Context, userID, id string, body actionItemPatch) (actionItemResponse, *opError) {
	if !itemIDPattern.MatchString(id) {
		return actionItemResponse{}, errActionItemNotFound
	}
	var version int64
	if body.Version != nil {
		if isNull(body.Version) || !decodeValue(body.Version, &version) || version < 1 {
			return actionItemResponse{}, invalidRequest("version must be a positive integer.")
		}
	}
	apply, n, msg := body.parse()
	if msg != "" {
		return actionItemResponse{}, invalidRequest(msg)
	}
	if n == 0 {
		return actionItemResponse{}, invalidRequest("At least one of content, status, note, due_at, refs or parent_id is required.")
	}
	now := s.auth.Now().Unix()
	item, err := s.auth.Store.UpdateActionItem(ctx, userID, id, func(item *store.ActionItem) error {
		if body.Version != nil && item.Version != version {
			return store.ErrVersionConflict
		}
		status := item.Status
		apply(item)
		// A status chosen by the user ends the integration's say over the
		// item: it is no longer completed or reopened automatically. Sorting
		// a candidate (candidate to open, or back) keeps the tracking, so
		// that a reply still completes it.
		if item.Status != status {
			if !activeStatus(status) || !activeStatus(item.Status) {
				item.Tracking = 0
			}
			item.AutoReason, item.AutoRef, item.AutoAt = sql.NullString{}, sql.NullString{}, sql.NullInt64{}
			// An event reopened after its start becomes a plain task, so
			// that the sweep does not dismiss it again.
			if item.Kind == store.KindEvent && !activeStatus(status) && activeStatus(item.Status) && item.EventStartAt.Valid && item.EventStartAt.Int64 <= now {
				item.Kind = store.KindTask
			}
		}
		item.UpdatedAt = now
		return nil
	})
	if err != nil {
		return actionItemResponse{}, storeError("update", userID, err)
	}
	return s.itemResponse(ctx, "update", userID, item)
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
		if (k != "status" && k != "limit" && k != "cursor" && k != "auto" && k != "parent_id" && k != "view") || len(v) != 1 {
			fail(w, 400, "invalid_request", "Only status, limit, cursor, auto, parent_id and view are allowed, each at most once.")
			return
		}
	}
	if v, ok := q["parent_id"]; ok {
		if len(q) != 1 {
			fail(w, 400, "invalid_request", "parent_id cannot be combined with status, limit, cursor, auto or view.")
			return
		}
		out, e := s.listChildActionItems(r.Context(), u.ID, v[0])
		if e != nil {
			failOp(w, e)
			return
		}
		writeJSON(w, 200, out)
		return
	}
	auto := false
	if v, ok := q["auto"]; ok {
		if v[0] != "true" {
			fail(w, 400, "invalid_request", "auto must be true.")
			return
		}
		auto = true
	}
	top := false
	if v, ok := q["view"]; ok {
		if v[0] != "top" {
			fail(w, 400, "invalid_request", "view must be top.")
			return
		}
		if auto {
			fail(w, 400, "invalid_request", "view cannot be combined with auto or parent_id.")
			return
		}
		top = true
	}
	status := ""
	if v, ok := q["status"]; ok {
		if !validStatus(v[0]) {
			fail(w, 400, "invalid_request", statusMessage)
			return
		}
		status = v[0]
	}
	limit := 50
	if v, ok := q["limit"]; ok {
		n, err := strconv.Atoi(v[0])
		if !limitPattern.MatchString(v[0]) || err != nil || n > 100 {
			fail(w, 400, "invalid_request", limitMessage)
			return
		}
		limit = n
	}
	cursor := ""
	if v, ok := q["cursor"]; ok {
		if v[0] == "" {
			fail(w, 400, "invalid_request", cursorMessage)
			return
		}
		cursor = v[0]
	}
	list := s.listActionItems
	switch {
	case auto:
		list = s.listAutoActionItems
	case top:
		list = s.listTopActionItems
	}
	out, e := list(r.Context(), u.ID, status, limit, cursor)
	if e != nil {
		failOp(w, e)
		return
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
	out, e := s.createActionItem(r.Context(), u.ID, in)
	if e != nil {
		failOp(w, e)
		return
	}
	w.Header().Set("Location", "/api/v1/action-items/"+out.ID)
	writeJSON(w, 201, out)
}

func (s *Server) handleGetActionItem(w http.ResponseWriter, r *http.Request) {
	u, ok := userFrom(r.Context())
	if !ok {
		s.authError(w, r, store.ErrUnauthorized)
		return
	}
	out, e := s.getActionItem(r.Context(), u.ID, r.PathValue("id"))
	if e != nil {
		failOp(w, e)
		return
	}
	writeJSON(w, 200, out)
}

func (s *Server) handleUpdateActionItem(w http.ResponseWriter, r *http.Request) {
	u, ok := userFrom(r.Context())
	if !ok {
		s.authError(w, r, store.ErrUnauthorized)
		return
	}
	id := r.PathValue("id")
	// A malformed ID is answered before the body is read, as before the
	// operation was shared with MCP.
	if !itemIDPattern.MatchString(id) {
		failOp(w, errActionItemNotFound)
		return
	}
	var in actionItemPatch
	if !decode(w, r, &in, actionItemBodyLimit) {
		return
	}
	out, e := s.updateActionItem(r.Context(), u.ID, id, in)
	if e != nil {
		failOp(w, e)
		return
	}
	writeJSON(w, 200, out)
}
