package store

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"strings"
)

// ErrInvalidParent is returned when a parent would break the nesting rules:
// the parent must be another item of the same user that has no parent, and
// an item with subtasks cannot get a parent.
var ErrInvalidParent = errors.New("invalid parent action item")

// ReasonChildrenDone is the automatic reason of a parent completed because
// its subtasks were all done.
const ReasonChildrenDone = "children.done"

// ChildCount counts the subtasks of a parent. Children counts every
// subtask, Total those that are not dismissed, Done the done ones and
// Remaining the open and candidate ones. NextDueAt is the earliest due date
// of the open and candidate ones, or null when none of them has one.
type ChildCount struct {
	ParentID  string
	Children  int64
	Total     int64
	Done      int64
	Remaining int64
	NextDueAt sql.NullInt64
}

// nextDueAt converts the next_due_at column of CountChildrenByParents, where
// -1 stands for no due date, to a nullable value.
func nextDueAt(v int64) sql.NullInt64 {
	if v == -1 {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: v, Valid: true}
}

// ItemSummary is the part of an item shown with its subtasks.
type ItemSummary struct {
	ID, Content, Status string
}

// joinIDs returns the IDs as the comma-separated list taken by the queries
// that filter by several IDs. Item IDs never contain a comma; one that does
// could not match and is left out.
func joinIDs(ids []string) string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id != "" && !strings.Contains(id, ",") {
			out = append(out, id)
		}
	}
	return strings.Join(out, ",")
}

// ChildCounts returns the subtask counts of the given parents that have at
// least one subtask, by parent ID. No query is made for an empty list.
func (s *Store) ChildCounts(ctx context.Context, userID string, parentIDs []string) (map[string]ChildCount, error) {
	out := map[string]ChildCount{}
	ids := joinIDs(parentIDs)
	if ids == "" {
		return out, nil
	}
	rows, err := s.q.countChildrenByParents(ctx, userID, ids)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		out[row.ParentID] = row
	}
	return out, nil
}

// ItemSummaries returns the user's items with the given IDs by ID. Missing
// items and other users' items are left out. No query is made for an empty
// list.
func (s *Store) ItemSummaries(ctx context.Context, userID string, ids []string) (map[string]ItemSummary, error) {
	out := map[string]ItemSummary{}
	list := joinIDs(ids)
	if list == "" {
		return out, nil
	}
	rows, err := s.q.actionItemSummaries(ctx, userID, list)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		out[row.ID] = row
	}
	return out, nil
}

// ChildActionItems returns up to limit subtasks of the user's parent, oldest
// first. It reads without locking.
func (s *Store) ChildActionItems(ctx context.Context, userID, parentID string, limit int) ([]ActionItem, error) {
	return s.q.listChildActionItems(ctx, userID, parentID, limit)
}

// ConversationItems returns the user's items of source in one conversation,
// newest first, at most fetchLimit: the open items whose source key starts
// with one of prefixes or equals one of exactKeys, created at or after
// dmSince, and the items completed with ReasonChildrenDone at or after
// closedSince. It reads without locking.
func (s *Store) ConversationItems(ctx context.Context, userID, source string, prefixes, exactKeys []string, dmSince, closedSince int64, fetchLimit int) ([]ActionItem, error) {
	seen := map[string]bool{}
	var out []ActionItem
	add := func(item ActionItem) {
		if !seen[item.ID] {
			seen[item.ID] = true
			out = append(out, item)
		}
	}
	for _, prefix := range prefixes {
		items, err := s.q.listConversationActionItems(ctx, userID, source, prefix, dmSince, closedSince, fetchLimit)
		if err != nil {
			return nil, err
		}
		for _, item := range items {
			add(item)
		}
	}
	for _, key := range exactKeys {
		item, err := s.q.actionItemBySourceKey(ctx, userID, key)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if item.Source.String == source && item.CreatedAt >= dmSince && (item.Status == "open" || (item.Status == "done" && item.AutoReason.String == ReasonChildrenDone && item.AutoAt.Valid && item.AutoAt.Int64 >= closedSince)) {
			add(item)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].CreatedAt != out[j].CreatedAt {
			return out[i].CreatedAt > out[j].CreatedAt
		}
		return out[i].ID > out[j].ID
	})
	if len(out) > fetchLimit {
		out = out[:fetchLimit]
	}
	return out, nil
}

// RelatedTask is an item of a conversation shown to the AI as a related
// task. A group parent (Group) carries Subtasks, the content of its open
// subtasks, or of its latest subtasks when it is Closed (completed because
// its subtasks were all done).
type RelatedTask struct {
	ID       string
	Content  string
	Subtasks []string
	Group    bool
	Closed   bool
}

// RelatedScope selects the related tasks of a conversation of the
// integration Source. A key starting with one of GroupPrefixes is a group
// parent created by the integration; only those are folded with their
// subtasks, and only those are kept when they are closed. The items of
// Exclude (by ID) are left out, on their own and among the subtasks; they
// are left out after the items are read, so the related tasks may be fewer
// than FetchLimit would otherwise give.
type RelatedScope struct {
	Source        string
	Prefixes      []string
	ExactKeys     []string
	GroupPrefixes []string
	DMSince       int64
	ClosedSince   int64
	FetchLimit    int
	Limit         int
	SubtaskLimit  int
	Exclude       []string
}

// subtaskReadLimit bounds the subtasks read for one group parent.
const subtaskReadLimit = 100

// RelatedTasks returns up to scope.Limit related tasks of a conversation,
// newest first. Group parents come once with their subtasks, whose items
// are not listed on their own; every other open item is listed on its own.
// It reads without locking.
func (s *Store) RelatedTasks(ctx context.Context, userID string, scope RelatedScope) ([]RelatedTask, error) {
	items, err := s.ConversationItems(ctx, userID, scope.Source, scope.Prefixes, scope.ExactKeys, scope.DMSince, scope.ClosedSince, scope.FetchLimit)
	if err != nil {
		return nil, err
	}
	isGroup := func(item ActionItem) bool {
		for _, p := range scope.GroupPrefixes {
			if item.SourceKey.Valid && strings.HasPrefix(item.SourceKey.String, p) {
				return true
			}
		}
		return false
	}
	folded := map[string]bool{}
	var list []RelatedTask
	var order []ActionItem
	for _, item := range items {
		group := isGroup(item)
		if contains(scope.Exclude, item.ID) || (item.Status != "open" && !group) {
			continue
		}
		if !group {
			order = append(order, item)
			continue
		}
		children, err := s.q.listChildActionItems(ctx, userID, item.ID, subtaskReadLimit)
		if err != nil {
			return nil, err
		}
		closed := item.Status != "open"
		var subtasks []string
		for _, c := range children {
			folded[c.ID] = true
			if !contains(scope.Exclude, c.ID) && (closed || c.Status == "open") {
				subtasks = append(subtasks, c.Content)
			}
		}
		// The latest subtasks are kept.
		if len(subtasks) > scope.SubtaskLimit {
			subtasks = subtasks[len(subtasks)-scope.SubtaskLimit:]
		}
		list = append(list, RelatedTask{ID: item.ID, Content: item.Content, Subtasks: subtasks, Group: true, Closed: closed})
		order = append(order, item)
	}
	byID := map[string]RelatedTask{}
	for _, t := range list {
		byID[t.ID] = t
	}
	out := []RelatedTask{}
	for _, item := range order {
		if folded[item.ID] {
			continue
		}
		t, ok := byID[item.ID]
		if !ok {
			t = RelatedTask{ID: item.ID, Content: item.Content}
		}
		out = append(out, t)
		if len(out) >= scope.Limit {
			break
		}
	}
	return out, nil
}

// lockParent locks the item to become the parent of childID and checks that
// it is another item of the user without a parent.
func lockParent(ctx context.Context, q queries, userID, parentID, childID string) (ActionItem, error) {
	if parentID == childID {
		return ActionItem{}, ErrInvalidParent
	}
	p, e := q.lockActionItem(ctx, userID, parentID)
	if errors.Is(e, sql.ErrNoRows) {
		return ActionItem{}, ErrInvalidParent
	}
	if e != nil {
		return ActionItem{}, e
	}
	if p.ParentID.Valid {
		return ActionItem{}, ErrInvalidParent
	}
	return p, nil
}

// lockParents locks the existing items among ids in ascending ID order, the
// order every writer that locks several parents follows.
func lockParents(ctx context.Context, q queries, userID string, ids ...string) error {
	list := []string{}
	for _, id := range ids {
		if id != "" && !contains(list, id) {
			list = append(list, id)
		}
	}
	sort.Strings(list)
	for _, id := range list {
		if _, e := q.lockActionItem(ctx, userID, id); e != nil && !errors.Is(e, sql.ErrNoRows) {
			return e
		}
	}
	return nil
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// syncParents calls syncParent for each parent in ascending ID order.
func syncParents(ctx context.Context, q queries, userID string, now int64, ids ...string) error {
	list := []string{}
	for _, id := range ids {
		if id != "" && !contains(list, id) {
			list = append(list, id)
		}
	}
	sort.Strings(list)
	for _, id := range list {
		if e := syncParent(ctx, q, userID, id, now); e != nil {
			return e
		}
	}
	return nil
}

// syncParent completes an open parent whose subtasks are all done or
// dismissed (with at least one done), and reopens a parent completed that
// way when one of its subtasks is open or a candidate again. Other parents
// (completed or dismissed by hand, candidates) are left alone. It locks the
// parent before counting the subtasks, so that concurrent changes of two
// subtasks complete the parent once.
func syncParent(ctx context.Context, q queries, userID, parentID string, now int64) error {
	p, e := q.lockActionItem(ctx, userID, parentID)
	if errors.Is(e, sql.ErrNoRows) {
		return nil
	}
	if e != nil {
		return e
	}
	counts, e := q.countChildrenByParents(ctx, userID, joinIDs([]string{parentID}))
	if e != nil {
		return e
	}
	var c ChildCount
	if len(counts) > 0 {
		c = counts[0]
	}
	next := p
	switch {
	case p.Status == "open" && c.Remaining == 0 && c.Done > 0:
		next.Status, next.Tracking = "done", 0
		next.AutoReason, next.AutoRef, next.AutoAt = sql.NullString{String: ReasonChildrenDone, Valid: true}, sql.NullString{}, sql.NullInt64{Int64: now, Valid: true}
	case p.Status == "done" && p.AutoReason.Valid && p.AutoReason.String == ReasonChildrenDone && c.Remaining > 0:
		next.Status = "open"
		next.AutoReason, next.AutoRef, next.AutoAt = sql.NullString{}, sql.NullString{}, sql.NullInt64{}
	default:
		return nil
	}
	next.Version, next.UpdatedAt = p.Version+1, now
	n, e := q.updateActionItem(ctx, next, p.Version)
	if e != nil {
		return e
	}
	if n != 1 {
		return ErrVersionConflict
	}
	return nil
}

// ChildCount returns the number of subtasks of the user's item, whatever
// their status.
func (t *sourceTx) ChildCount(id string) (int64, error) {
	return t.q.countChildActionItems(t.ctx, t.userID, id)
}

// CreateGroup inserts a group parent made by an integration unless the user
// already has an item with its source key, and reports whether it was
// inserted. The group has no parent.
func (t *sourceTx) CreateGroup(item ActionItem) (bool, error) {
	item.ParentID = sql.NullString{}
	return t.CreateItem(item)
}

// SetParent makes the user's item itemID a subtask of parentID. Both are
// locked (the item first, then its old and new parents in ID order) and the
// nesting rules are checked; a violation is ErrInvalidParent. The item's
// version is incremented, and the old and new parents are synchronized.
func (t *sourceTx) SetParent(itemID, parentID string, now int64) error {
	item, e := t.q.lockActionItem(t.ctx, t.userID, itemID)
	if errors.Is(e, sql.ErrNoRows) {
		return ErrItemNotFound
	}
	if e != nil {
		return e
	}
	if item.ParentID.Valid && item.ParentID.String == parentID {
		return nil
	}
	if e = lockParents(t.ctx, t.q, t.userID, item.ParentID.String, parentID); e != nil {
		return e
	}
	if _, e = lockParent(t.ctx, t.q, t.userID, parentID, itemID); e != nil {
		return e
	}
	n, e := t.q.countChildActionItems(t.ctx, t.userID, itemID)
	if e != nil {
		return e
	}
	if n > 0 {
		return ErrInvalidParent
	}
	next := item
	next.ParentID = sql.NullString{String: parentID, Valid: true}
	next.Version, next.UpdatedAt = item.Version+1, now
	updated, e := t.q.updateActionItem(t.ctx, next, item.Version)
	if e != nil {
		return e
	}
	if updated != 1 {
		return ErrVersionConflict
	}
	return syncParents(t.ctx, t.q, t.userID, now, item.ParentID.String, parentID)
}

// SyncParent completes or reopens the user's parent after its subtasks
// changed; see syncParent.
func (t *sourceTx) SyncParent(parentID string, now int64) error {
	return syncParent(t.ctx, t.q, t.userID, parentID, now)
}
