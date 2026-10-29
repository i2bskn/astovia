package httpserver

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/i2bskn/astovia/internal/store"
)

const parentError = "parent_id must be the id of your item that has no parent, and an item with subtasks cannot have a parent."

func decodeItem(t *testing.T, w *httptest.ResponseRecorder, status int) actionItemResponse {
	t.Helper()
	var item actionItemResponse
	if w.Code != status || json.Unmarshal(w.Body.Bytes(), &item) != nil {
		t.Fatalf("got %d, want %d: %s", w.Code, status, w.Body.String())
	}
	return item
}

func decodePage(t *testing.T, w *httptest.ResponseRecorder) actionItemListResponse {
	t.Helper()
	var page actionItemListResponse
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}
	return page
}

func findItem(page actionItemListResponse, id string) *actionItemResponse {
	for i := range page.Items {
		if page.Items[i].ID == id {
			return &page.Items[i]
		}
	}
	return nil
}

func TestActionItemParents(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) { testActionItemParents(t, driver) })
	}
}

func testActionItemParents(t *testing.T, driver string) {
	api := newTokenAPI(t, driver)
	create := func(body string) actionItemResponse {
		t.Helper()
		return decodeItem(t, api.do("POST", "/api/v1/action-items", api.alice, body), 201)
	}
	patch := func(id, body string) *httptest.ResponseRecorder {
		t.Helper()
		return api.do("PATCH", "/api/v1/action-items/"+id, api.alice, body)
	}
	parent := create(`{"content":"障害対応"}`)
	if parent.ParentID != nil || parent.Parent != nil || parent.Children != nil {
		t.Fatalf("plain item: %+v", parent)
	}

	// A subtask names its parent; the parent counts its subtasks.
	a := create(`{"content":"調査する","parent_id":"` + parent.ID + `"}`)
	if a.ParentID == nil || *a.ParentID != parent.ID || a.Parent == nil || *a.Parent != (actionItemParent{ID: parent.ID, Content: "障害対応", Status: "open"}) || a.Children != nil {
		t.Fatalf("subtask: %+v", a)
	}
	b := create(`{"content":"報告する","parent_id":"` + parent.ID + `","note":null}`)
	got := decodeItem(t, api.do("GET", "/api/v1/action-items/"+parent.ID, api.alice, ""), 200)
	if got.Children == nil || *got.Children != (actionItemChildren{Total: 2, Done: 0, Remaining: 2}) || got.Parent != nil {
		t.Fatalf("parent: %+v", got)
	}
	// null on create means no parent.
	if solo := create(`{"content":"別件","parent_id":null}`); solo.ParentID != nil {
		t.Fatalf("null parent: %+v", solo)
	}

	// The last subtask done completes the parent, and the update response
	// shows it.
	decodeItem(t, patch(a.ID, `{"status":"done"}`), 200)
	done := decodeItem(t, patch(b.ID, `{"status":"done"}`), 200)
	if done.Parent == nil || done.Parent.Status != "done" {
		t.Fatalf("update response: %+v", done)
	}
	got = decodeItem(t, api.do("GET", "/api/v1/action-items/"+parent.ID, api.alice, ""), 200)
	if got.Status != "done" || got.Auto == nil || got.Auto.Reason != store.ReasonChildrenDone || got.Auto.RefURL != nil || *got.Children != (actionItemChildren{Total: 2, Done: 2, Remaining: 0}) {
		t.Fatalf("completed parent: %+v", got)
	}
	// The automatic list shows it with its subtask counts.
	page := decodePage(t, api.do("GET", "/api/v1/action-items?auto=true", api.alice, ""))
	if p := findItem(page, parent.ID); p == nil || p.Children == nil || p.Children.Done != 2 {
		t.Fatalf("auto list: %s", api.do("GET", "/api/v1/action-items?auto=true", api.alice, "").Body.String())
	}
	// Reopened from /auto, it stays open; the stale version conflicts.
	if w := patch(parent.ID, `{"status":"open","version":1}`); w.Code != 409 {
		t.Fatalf("stale parent version: %d %s", w.Code, w.Body.String())
	}
	if got = decodeItem(t, patch(parent.ID, `{"status":"open","version":2}`), 200); got.Status != "open" || got.Auto != nil {
		t.Fatalf("reopened: %+v", got)
	}

	// Moving: out with null, under another parent, and the list shows both.
	other := create(`{"content":"別の親"}`)
	moved := decodeItem(t, patch(a.ID, `{"parent_id":"`+other.ID+`"}`), 200)
	if moved.ParentID == nil || *moved.ParentID != other.ID || moved.Parent.ID != other.ID {
		t.Fatalf("moved: %+v", moved)
	}
	out := decodeItem(t, patch(b.ID, `{"parent_id":null}`), 200)
	if out.ParentID != nil || out.Parent != nil {
		t.Fatalf("taken out: %+v", out)
	}
	page = decodePage(t, api.do("GET", "/api/v1/action-items", api.alice, ""))
	if p := findItem(page, parent.ID); p == nil || p.Children != nil {
		t.Fatalf("emptied parent: %+v", p)
	}
	if c := findItem(page, a.ID); c == nil || c.Parent == nil || c.Parent.Content != "別の親" {
		t.Fatalf("listed subtask: %+v", c)
	}
	if p := findItem(page, other.ID); p == nil || p.Children == nil || *p.Children != (actionItemChildren{Total: 1, Done: 1, Remaining: 0}) {
		t.Fatalf("listed parent: %+v", p)
	}

	// The subtasks of a parent, oldest first.
	*api.now = api.now.Add(time.Second)
	c2 := create(`{"content":"二つ目","parent_id":"` + other.ID + `"}`)
	page = decodePage(t, api.do("GET", "/api/v1/action-items?parent_id="+other.ID, api.alice, ""))
	if len(page.Items) != 2 || page.Items[0].ID != a.ID || page.Items[1].ID != c2.ID || page.NextCursor != nil || page.Items[0].Parent == nil {
		t.Fatalf("children: %+v", page)
	}
	// Another user's or a missing parent has none.
	for _, id := range []string{"missing", other.ID} {
		token := api.alice
		if id == other.ID {
			token = api.bob
		}
		if page = decodePage(t, api.do("GET", "/api/v1/action-items?parent_id="+id, token, "")); len(page.Items) != 0 {
			t.Fatalf("children of %s: %+v", id, page)
		}
	}
	for _, q := range []string{"parent_id=" + other.ID + "&status=open", "parent_id=" + other.ID + "&limit=10", "parent_id=" + other.ID + "&cursor=x", "parent_id=" + other.ID + "&auto=true", "parent_id=bad.id", "parent_id=", "parent_id=a&parent_id=b"} {
		expectStatus(t, api.do("GET", "/api/v1/action-items?"+q, api.alice, ""), 400, "invalid_request")
	}

	// Broken rules are 400 and change nothing.
	bobs := decodeItem(t, api.do("POST", "/api/v1/action-items", api.bob, `{"content":"bob"}`), 201)
	for name, tc := range map[string]struct{ method, path, body string }{
		"create missing":     {"POST", "/api/v1/action-items", `{"content":"x","parent_id":"missing"}`},
		"create other user":  {"POST", "/api/v1/action-items", `{"content":"x","parent_id":"` + bobs.ID + `"}`},
		"create grandchild":  {"POST", "/api/v1/action-items", `{"content":"x","parent_id":"` + a.ID + `"}`},
		"create empty":       {"POST", "/api/v1/action-items", `{"content":"x","parent_id":""}`},
		"create malformed":   {"POST", "/api/v1/action-items", `{"content":"x","parent_id":"bad.id"}`},
		"create number":      {"POST", "/api/v1/action-items", `{"content":"x","parent_id":1}`},
		"update missing":     {"PATCH", "/api/v1/action-items/" + b.ID, `{"parent_id":"missing"}`},
		"update other user":  {"PATCH", "/api/v1/action-items/" + b.ID, `{"parent_id":"` + bobs.ID + `"}`},
		"update grandchild":  {"PATCH", "/api/v1/action-items/" + b.ID, `{"parent_id":"` + a.ID + `"}`},
		"update has subtask": {"PATCH", "/api/v1/action-items/" + other.ID, `{"parent_id":"` + b.ID + `"}`},
		"update self":        {"PATCH", "/api/v1/action-items/" + b.ID, `{"parent_id":"` + b.ID + `"}`},
		"update empty":       {"PATCH", "/api/v1/action-items/" + b.ID, `{"parent_id":""}`},
	} {
		w := api.do(tc.method, tc.path, api.alice, tc.body)
		expectStatus(t, w, 400, "invalid_request")
		if !strings.Contains(w.Body.String(), parentError) {
			t.Fatalf("%s: %s", name, w.Body.String())
		}
	}
	if got = decodeItem(t, api.do("GET", "/api/v1/action-items/"+b.ID, api.alice, ""), 200); got.ParentID != nil {
		t.Fatalf("rejected update stored: %+v", got)
	}
	// Bob cannot put his item under alice's.
	expectStatus(t, api.do("PATCH", "/api/v1/action-items/"+bobs.ID, api.bob, `{"parent_id":"`+other.ID+`"}`), 400, "invalid_request")
}

func TestMCPParents(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) { testMCPParents(t, driver) })
	}
}

func testMCPParents(t *testing.T, driver string) {
	api := newTokenAPI(t, driver)
	srv := httptest.NewServer(api.handler)
	t.Cleanup(srv.Close)
	c := connectMCP(t, srv, api.issue(api.alice, "alice").Token, nil)

	tools, err := c.session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools.Tools {
		// The Chatwork, Slack and Gmail tools have no action items.
		if strings.HasPrefix(tool.Name, "chatwork_") || strings.HasPrefix(tool.Name, "slack_") || strings.HasPrefix(tool.Name, "gmail_") {
			continue
		}
		props := tool.InputSchema.(map[string]any)["properties"].(map[string]any)
		p, ok := props["parent_id"].(map[string]any)
		if tool.Name == "action_items_get" {
			if ok {
				t.Fatalf("%s has parent_id", tool.Name)
			}
			continue
		}
		if types, _ := json.Marshal(p["type"]); !ok || string(types) != `["string","null"]` {
			t.Fatalf("%s parent_id: %v", tool.Name, p)
		}
		if tool.Name != "action_items_list" && !strings.Contains(tool.Description, "two levels") {
			t.Fatalf("%s description: %s", tool.Name, tool.Description)
		}
	}

	var parent, child, got actionItemResponse
	c.ok("action_items_create", map[string]any{"content": "障害対応", "parent_id": nil}, &parent)
	if parent.ParentID != nil {
		t.Fatalf("null parent on create: %+v", parent)
	}
	c.ok("action_items_create", map[string]any{"content": "調査する", "parent_id": parent.ID}, &child)
	if child.ParentID == nil || *child.ParentID != parent.ID || child.Parent == nil {
		t.Fatalf("child: %+v", child)
	}
	var page actionItemListResponse
	c.ok("action_items_list", map[string]any{"parent_id": parent.ID, "status": nil}, &page)
	if len(page.Items) != 1 || page.Items[0].ID != child.ID {
		t.Fatalf("children: %+v", page)
	}
	c.ok("action_items_get", map[string]any{"id": parent.ID}, &got)
	if got.Children == nil || got.Children.Remaining != 1 {
		t.Fatalf("parent: %+v", got)
	}
	// null on update takes the item out of its parent.
	c.ok("action_items_update", map[string]any{"id": child.ID, "parent_id": nil}, &got)
	if got.ParentID != nil || got.Parent != nil {
		t.Fatalf("taken out: %+v", got)
	}
	c.fails("action_items_update", map[string]any{"id": parent.ID, "parent_id": parent.ID}, "invalid_request: "+parentError)
	c.fails("action_items_create", map[string]any{"content": "x", "parent_id": "missing"}, "invalid_request: "+parentError)
	c.fails("action_items_list", map[string]any{"parent_id": parent.ID, "limit": 10}, "invalid_request: parent_id cannot be combined with status, limit or cursor.")
	c.fails("action_items_list", map[string]any{"parent_id": 1}, "invalid_request: "+parentError)
	c.fails("action_items_list", map[string]any{"parent_id": "bad.id"}, "invalid_request: "+parentError)
}
