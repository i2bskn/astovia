package httpserver

import (
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestActionItemsTopView(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) { testActionItemsTopView(t, driver) })
	}
}

func testActionItemsTopView(t *testing.T, driver string) {
	api := newTokenAPI(t, driver)
	create := func(body string) actionItemResponse {
		t.Helper()
		*api.now = api.now.Add(time.Second)
		return decodeItem(t, api.do("POST", "/api/v1/action-items", api.alice, body), 201)
	}
	under := func(parent actionItemResponse, body string) actionItemResponse {
		t.Helper()
		return create(`{"parent_id":"` + parent.ID + `",` + body + `}`)
	}
	patch := func(id, body string) actionItemResponse {
		t.Helper()
		return decodeItem(t, api.do("PATCH", "/api/v1/action-items/"+id, api.alice, body), 200)
	}
	ids := func(query string) []string {
		t.Helper()
		page := decodePage(t, api.do("GET", "/api/v1/action-items?"+query, api.alice, ""))
		out := []string{}
		for _, item := range page.Items {
			out = append(out, item.ID)
		}
		return out
	}

	// p1 has open subtasks with due dates, and a done and a dismissed one
	// with earlier dates that are left out of next_due_at.
	p1 := create(`{"content":"親1"}`)
	c1 := under(p1, `"content":"子1","due_at":"2026-11-03T00:00:00Z"`)
	c2 := under(p1, `"content":"子2","due_at":"2026-11-02T09:00:00+09:00"`)
	c3 := under(p1, `"content":"子3","due_at":"2026-10-30T00:00:00Z"`)
	c4 := under(p1, `"content":"子4","due_at":"2026-10-29T00:00:00Z"`)
	patch(c3.ID, `{"status":"done"}`)
	patch(c4.ID, `{"status":"dismissed"}`)
	// p2's only subtask is a candidate; its due date counts.
	p2 := create(`{"content":"親2"}`)
	c5 := under(p2, `"content":"候補の子","status":"candidate","due_at":"2026-11-05T00:00:00Z"`)
	// p3 is dismissed by hand while its subtask stays open.
	p3 := create(`{"content":"親3"}`)
	c6 := under(p3, `"content":"残った子"`)
	patch(p3.ID, `{"status":"dismissed"}`)
	// p4 is completed with its only subtask.
	p4 := create(`{"content":"親4"}`)
	c7 := under(p4, `"content":"終わった子"`)
	patch(c7.ID, `{"status":"done"}`)
	solo := create(`{"content":"単独"}`)
	decodeItem(t, api.do("POST", "/api/v1/action-items", api.bob, `{"content":"bob"}`), 201)

	for query, want := range map[string][]string{
		// Newest first: items without a parent, and subtasks whose parent
		// has another status.
		"view=top&status=open":      {solo.ID, c6.ID, p2.ID, p1.ID},
		"view=top&status=candidate": {c5.ID},
		"view=top&status=done":      {p4.ID, c3.ID},
		"view=top&status=dismissed": {p3.ID, c4.ID},
		// Every status: only the items without a parent.
		"view=top": {solo.ID, p4.ID, p3.ID, p2.ID, p1.ID},
	} {
		if got := ids(query); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: got %v, want %v", query, got, want)
		}
	}
	// c3 and c4 are listed because p1 is open; once p1 is done by hand, c3
	// (done) shares its status and leaves the done list.
	patch(p1.ID, `{"status":"done"}`)
	if got, want := ids("view=top&status=done"), []string{p4.ID, p1.ID}; !reflect.DeepEqual(got, want) {
		t.Fatalf("done after parent done: got %v, want %v", got, want)
	}
	if got, want := ids("view=top&status=open"), []string{solo.ID, c6.ID, p2.ID, c2.ID, c1.ID}; !reflect.DeepEqual(got, want) {
		t.Fatalf("open after parent done: got %v, want %v", got, want)
	}
	patch(p1.ID, `{"status":"open"}`)

	// Paging keeps the same cursors and filter.
	var paged []string
	cursor := ""
	for i := 0; i < 10; i++ {
		q := "view=top&status=open&limit=1"
		if cursor != "" {
			q += "&cursor=" + url.QueryEscape(cursor)
		}
		page := decodePage(t, api.do("GET", "/api/v1/action-items?"+q, api.alice, ""))
		for _, item := range page.Items {
			paged = append(paged, item.ID)
		}
		if page.NextCursor == nil {
			break
		}
		cursor = *page.NextCursor
	}
	if want := []string{solo.ID, c6.ID, p2.ID, p1.ID}; !reflect.DeepEqual(paged, want) {
		t.Fatalf("paged: got %v, want %v", paged, want)
	}

	// The subtask listed on its own shows its parent; the parents show the
	// earliest due date of their open and candidate subtasks.
	page := decodePage(t, api.do("GET", "/api/v1/action-items?view=top&status=open", api.alice, ""))
	if c := findItem(page, c6.ID); c == nil || c.Parent == nil || c.Parent.ID != p3.ID || c.Parent.Status != "dismissed" {
		t.Fatalf("listed subtask: %+v", c)
	}
	if p := findItem(page, p1.ID); p == nil || p.Children == nil || p.Children.NextDueAt == nil || *p.Children.NextDueAt != "2026-11-02T00:00:00Z" {
		t.Fatalf("p1 next_due_at: %+v", p)
	}
	if p := findItem(page, p2.ID); p == nil || p.Children == nil || p.Children.NextDueAt == nil || *p.Children.NextDueAt != "2026-11-05T00:00:00Z" {
		t.Fatalf("p2 next_due_at: %+v", p)
	}
	// Without a due date, or with only done subtasks, it is null.
	got := api.do("GET", "/api/v1/action-items/"+p3.ID, api.alice, "")
	if !strings.Contains(got.Body.String(), `"next_due_at":null`) {
		t.Fatalf("p3 next_due_at: %s", got.Body.String())
	}
	patch(c1.ID, `{"status":"done"}`)
	patch(c2.ID, `{"status":"done"}`)
	if p := decodeItem(t, api.do("GET", "/api/v1/action-items/"+p1.ID, api.alice, ""), 200); p.Children == nil || p.Children.NextDueAt != nil || p.Children.Remaining != 0 {
		t.Fatalf("p1 with done subtasks: %+v", p.Children)
	}

	// view is validated and cannot be combined with auto or parent_id.
	for query, msg := range map[string]string{
		"view=all":                    "view must be top.",
		"view=":                       "view must be top.",
		"view=top&view=top":           "Only status, limit, cursor, auto, parent_id and view are allowed, each at most once.",
		"view=top&auto=true":          "view cannot be combined with auto or parent_id.",
		"view=top&parent_id=" + p1.ID: "parent_id cannot be combined with status, limit, cursor, auto or view.",
	} {
		w := api.do("GET", "/api/v1/action-items?"+query, api.alice, "")
		expectStatus(t, w, 400, "invalid_request")
		if !strings.Contains(w.Body.String(), msg) {
			t.Fatalf("%s: %s", query, w.Body.String())
		}
	}
}
