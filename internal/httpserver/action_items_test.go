package httpserver

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"github.com/i2bskn/astovia/internal/auth"
	"github.com/i2bskn/astovia/internal/config"
	"github.com/i2bskn/astovia/internal/store"
	"github.com/i2bskn/astovia/internal/testutil"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

type actionItemAPI struct {
	t       *testing.T
	handler http.Handler
	alice   string
	bob     string
}

// newActionItemAPI logs in alice and bob with the clock fixed at *now. Tests
// may advance *now only by a few minutes so that the access tokens stay valid.
func newActionItemAPI(t *testing.T, driver string, now *time.Time) *actionItemAPI {
	t.Helper()
	s, _ := testutil.Database(t, driver)
	hash, err := auth.Password("correct-password-123")
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range []store.User{{ID: "alice-id", Email: "alice@example.com", PasswordHash: hash}, {ID: "bob-id", Email: "bob@example.com", PasswordHash: hash}} {
		if err = s.CreateUser(context.Background(), u); err != nil {
			t.Fatal(err)
		}
	}
	a, err := auth.New(s, make([]byte, 32), "https://astovia.test")
	if err != nil {
		t.Fatal(err)
	}
	a.Now = func() time.Time { return *now }
	api := &actionItemAPI{t: t, handler: New(config.Config{PublicURL: "https://astovia.test", Secure: true}, a, fstest.MapFS{})}
	for _, u := range []struct {
		name  string
		token *string
	}{{"alice", &api.alice}, {"bob", &api.bob}} {
		r := httptest.NewRequest("POST", "/auth/login", strings.NewReader(`{"email":"`+u.name+`@example.com","password":"correct-password-123"}`))
		r.Header.Set("Origin", "https://astovia.test")
		r.Header.Set("Content-Type", "application/json")
		w := serve(t, api.handler, r)
		var result authResponse
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil {
			t.Fatalf("login %s: %d %s", u.name, w.Code, w.Body.String())
		}
		*u.token = result.AccessToken
	}
	return api
}

func (a *actionItemAPI) do(method, path, token, ct, body string) *httptest.ResponseRecorder {
	a.t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if ct != "" {
		r.Header.Set("Content-Type", ct)
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	return serve(a.t, a.handler, r)
}

// create posts body as alice and returns the created item.
func (a *actionItemAPI) create(token, body string) actionItemResponse {
	a.t.Helper()
	w := a.do("POST", "/api/v1/action-items", token, "application/json", body)
	if w.Code != 201 {
		a.t.Fatalf("create %s: %d %s", body, w.Code, w.Body.String())
	}
	var item actionItemResponse
	if err := json.Unmarshal(w.Body.Bytes(), &item); err != nil {
		a.t.Fatal(err)
	}
	return item
}

func expectStatus(t *testing.T, w *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if w.Code != status || errorCode(t, w) != code {
		t.Fatalf("got %d %q, want %d %q: %s", w.Code, errorCode(t, w), status, code, w.Body.String())
	}
}

func TestActionItemsAPI(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) { testActionItemsAPI(t, driver) })
	}
}

func testActionItemsAPI(t *testing.T, driver string) {
	now := time.Date(2026, 10, 23, 1, 2, 3, 0, time.UTC)
	api := newActionItemAPI(t, driver, &now)

	w := api.do("POST", "/api/v1/action-items", api.alice, "application/json", `{"content":" 日程を返答する ","note":"memo","due_at":"2026-10-24T09:30:00.9+09:00","refs":[{"service":"slack","account":"T123","conversation_id":"C123","conversation_name":"#general","message_id":"1700000000.000100","url":"https://example.com/x"},{"service":"line"}]}`)
	if w.Code != 201 {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	body, keys := jsonKeys(t, w.Body.Bytes())
	if !reflect.DeepEqual(keys, []string{"auto", "children", "content", "created_at", "due_at", "event", "id", "kind", "note", "parent", "parent_id", "refs", "source", "status", "updated_at", "version"}) {
		t.Fatalf("item keys: %v", keys)
	}
	if string(body["source"]) != "null" || string(body["auto"]) != "null" || string(body["kind"]) != `"task"` || string(body["event"]) != "null" {
		t.Fatalf("source and auto of a created item: %s %s", body["source"], body["auto"])
	}
	var refs []map[string]json.RawMessage
	if err := json.Unmarshal(body["refs"], &refs); err != nil || len(refs) != 2 {
		t.Fatalf("refs: %s", body["refs"])
	}
	if _, keys = jsonKeys(t, mustJSON(t, refs[0])); !reflect.DeepEqual(keys, []string{"account", "conversation_id", "conversation_name", "message_id", "service", "url"}) {
		t.Fatalf("ref keys: %v", keys)
	}
	if _, keys = jsonKeys(t, mustJSON(t, refs[1])); !reflect.DeepEqual(keys, []string{"service"}) {
		t.Fatalf("minimal ref keys: %v", keys)
	}
	var item actionItemResponse
	if err := json.Unmarshal(w.Body.Bytes(), &item); err != nil {
		t.Fatal(err)
	}
	if w.Header().Get("Location") != "/api/v1/action-items/"+item.ID || len(item.ID) != 43 {
		t.Fatalf("Location=%q id=%q", w.Header().Get("Location"), item.ID)
	}
	if item.Content != " 日程を返答する " || item.Status != "open" || item.Version != 1 || item.Note == nil || *item.Note != "memo" {
		t.Fatalf("created: %+v", item)
	}
	if item.CreatedAt != "2026-10-23T01:02:03Z" || item.UpdatedAt != "2026-10-23T01:02:03Z" || item.DueAt == nil || *item.DueAt != "2026-10-24T00:30:00Z" {
		t.Fatalf("times: %s %s %v", item.CreatedAt, item.UpdatedAt, item.DueAt)
	}

	w = api.do("GET", "/api/v1/action-items/"+item.ID, api.alice, "", "")
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("get: %d %s", w.Code, w.Body.String())
	}
	var got actionItemResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || !reflect.DeepEqual(got, item) {
		t.Fatalf("get: %+v %v", got, err)
	}
	minimal := api.create(api.alice, `{"content":"x","note":null,"due_at":null}`)
	if minimal.Note != nil || minimal.DueAt != nil || minimal.Refs == nil || len(minimal.Refs) != 0 {
		t.Fatalf("minimal: %+v", minimal)
	}
	w = api.do("GET", "/api/v1/action-items/"+minimal.ID, api.alice, "", "")
	if body, _ = jsonKeys(t, w.Body.Bytes()); string(body["note"]) != "null" || string(body["due_at"]) != "null" || string(body["refs"]) != "[]" {
		t.Fatalf("null fields: %s", w.Body.String())
	}

	// PATCH distinguishes a missing key from null and advances the version.
	now = now.Add(time.Minute)
	w = api.do("PATCH", "/api/v1/action-items/"+item.ID, api.alice, "application/json", `{"status":"done","version":1}`)
	if w.Code != 200 {
		t.Fatalf("patch: %d %s", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status != "done" || got.Version != 2 || got.Note == nil || got.DueAt == nil || got.Content != item.Content || got.CreatedAt != item.CreatedAt || got.UpdatedAt != "2026-10-23T01:03:03Z" {
		t.Fatalf("patched: %+v", got)
	}
	w = api.do("PATCH", "/api/v1/action-items/"+item.ID, api.alice, "application/json", `{"note":null,"due_at":null,"version":1}`)
	expectStatus(t, w, 409, "conflict")
	w = api.do("PATCH", "/api/v1/action-items/"+item.ID, api.alice, "application/json", `{"note":null,"due_at":null,"refs":[]}`)
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != 200 || got.Note != nil || got.DueAt != nil || len(got.Refs) != 0 || got.Version != 3 || got.Status != "done" {
		t.Fatalf("clear: %d %s", w.Code, w.Body.String())
	}
	w = api.do("PATCH", "/api/v1/action-items/"+item.ID, api.alice, "application/json", `{"note":"   "}`)
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != 200 || got.Note != nil {
		t.Fatalf("blank note: %d %s", w.Code, w.Body.String())
	}
	for _, body := range []string{`{}`, `{"version":4}`, `{"content":null}`, `{"status":null}`, `{"refs":null}`, `{"version":null,"status":"open"}`, `{"version":0,"status":"open"}`, `{"version":1.5,"status":"open"}`, `{"version":"4","status":"open"}`, `{"id":"x","status":"open"}`} {
		expectStatus(t, api.do("PATCH", "/api/v1/action-items/"+item.ID, api.alice, "application/json", body), 400, "invalid_request")
	}

	// Authorization: another user's item looks the same as a missing one.
	bobs := api.create(api.bob, `{"content":"bob's secret"}`)
	for _, path := range []string{"/api/v1/action-items/" + bobs.ID, "/api/v1/action-items/missing", "/api/v1/action-items/bad.id", "/api/v1/action-items/" + strings.Repeat("a", 65)} {
		expectStatus(t, api.do("GET", path, api.alice, "", ""), 404, "not_found")
		expectStatus(t, api.do("PATCH", path, api.alice, "application/json", `{"status":"done"}`), 404, "not_found")
	}
	w = api.do("GET", "/api/v1/action-items/"+bobs.ID, api.bob, "", "")
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || got.Status != "open" || got.Version != 1 {
		t.Fatalf("bob's item changed: %s", w.Body.String())
	}
	w = api.do("GET", "/api/v1/action-items", api.alice, "", "")
	if w.Code != 200 || strings.Contains(w.Body.String(), bobs.ID) || strings.Contains(w.Body.String(), "user_id") {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}
	if _, keys = jsonKeys(t, w.Body.Bytes()); !reflect.DeepEqual(keys, []string{"items", "next_cursor"}) {
		t.Fatalf("list keys: %v", keys)
	}
	for _, method := range []string{"GET", "POST"} {
		expectStatus(t, api.do(method, "/api/v1/action-items", "", "application/json", `{"content":"x"}`), 401, "unauthorized")
	}
	for _, method := range []string{"GET", "PATCH"} {
		expectStatus(t, api.do(method, "/api/v1/action-items/"+item.ID, "", "application/json", `{"status":"open"}`), 401, "unauthorized")
	}

	// Method, media type and size are checked in that order around requireUser.
	for path, allow := range map[string]string{"/api/v1/action-items": "GET, POST", "/api/v1/action-items/" + item.ID: "GET, PATCH"} {
		for _, method := range []string{"DELETE", "PUT"} {
			w = api.do(method, path, "", "", "")
			expectStatus(t, w, 405, "method_not_allowed")
			if w.Header().Get("Allow") != allow {
				t.Fatalf("%s %s: Allow=%q", method, path, w.Header().Get("Allow"))
			}
		}
	}
	expectStatus(t, api.do("PATCH", "/api/v1/action-items/"+item.ID, api.alice, "text/plain", `{"status":"open"}`), 415, "unsupported_media_type")
	expectStatus(t, api.do("POST", "/api/v1/action-items", api.alice, "", `{"content":"x"}`), 415, "unsupported_media_type")
	expectStatus(t, api.do("POST", "/api/v1/action-items", "", "text/plain", `{"content":"x"}`), 401, "unauthorized")
	expectStatus(t, api.do("POST", "/api/v1/action-items", api.alice, "application/json", `{"content":"x","note":"`+strings.Repeat("x", 65536)+`"}`), 413, "body_too_large")
	if w = api.do("POST", "/api/v1/action-items", api.alice, "application/json; charset=utf-8", `{"content":"x"}`); w.Code != 201 {
		t.Fatalf("charset: %d", w.Code)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestActionItemValidation(t *testing.T) {
	now := time.Date(2026, 10, 23, 1, 2, 3, 0, time.UTC)
	api := newActionItemAPI(t, "sqlite", &now)
	ref := `{"service":"slack"}`
	refs := func(n int) string { return "[" + strings.TrimSuffix(strings.Repeat(ref+",", n), ",") + "]" }
	for _, tc := range []struct {
		body string
		ok   bool
	}{
		{`{"content":"` + strings.Repeat("あ", 500) + `"}`, true},
		{`{"content":"` + strings.Repeat("あ", 501) + `"}`, false},
		{`{"content":" \t\n "}`, false},
		{`{"content":""}`, false},
		{`{}`, false},
		{`{"content":null}`, false},
		{`{"content":1}`, false},
		{`{"content":"a\u0000b"}`, false},
		{`{"content":"x","status":"done"}`, true},
		{`{"content":"x","status":"candidate"}`, true},
		{`{"content":"x","status":"closed"}`, false},
		{`{"content":"x","status":null}`, false},
		{`{"content":"x","note":"` + strings.Repeat("あ", 4000) + `"}`, true},
		{`{"content":"x","note":"` + strings.Repeat("あ", 4001) + `"}`, false},
		{`{"content":"x","note":"\u0000"}`, false},
		{`{"content":"x","due_at":"2026-10-23T10:00:00Z"}`, true},
		{`{"content":"x","due_at":"2026-10-23"}`, false},
		{`{"content":"x","due_at":"2026-10-23T10:00:00"}`, false},
		{`{"content":"x","due_at":"1969-12-31T23:59:59Z"}`, false},
		{`{"content":"x","due_at":"1970-01-01T08:59:59+09:00"}`, false},
		{`{"content":"x","due_at":"9999-12-31T23:59:59Z"}`, true},
		{`{"content":"x","due_at":1700000000}`, false},
		{`{"content":"x","refs":` + refs(10) + `}`, true},
		{`{"content":"x","refs":` + refs(11) + `}`, false},
		{`{"content":"x","refs":null}`, false},
		{`{"content":"x","refs":{}}`, false},
		{`{"content":"x","refs":["slack"]}`, false},
		{`{"content":"x","refs":[{}]}`, false},
		{`{"content":"x","refs":[{"service":"Slack"}]}`, false},
		{`{"content":"x","refs":[{"service":"` + strings.Repeat("a", 32) + `"}]}`, true},
		{`{"content":"x","refs":[{"service":"` + strings.Repeat("a", 33) + `"}]}`, false},
		{`{"content":"x","refs":[{"service":"slack","channel":"C1"}]}`, false},
		{`{"content":"x","refs":[{"service":"slack","account":""}]}`, false},
		{`{"content":"x","refs":[{"service":"slack","account":"` + strings.Repeat("あ", 256) + `"}]}`, true},
		{`{"content":"x","refs":[{"service":"slack","message_id":"` + strings.Repeat("a", 257) + `"}]}`, false},
		{`{"content":"x","refs":[{"service":"slack","conversation_id":"a\u0000"}]}`, false},
		{`{"content":"x","refs":[{"service":"slack","conversation_name":"` + strings.Repeat("あ", 256) + `"}]}`, true},
		{`{"content":"x","refs":[{"service":"slack","conversation_name":"` + strings.Repeat("あ", 257) + `"}]}`, false},
		{`{"content":"x","refs":[{"service":"slack","conversation_name":""}]}`, false},
		{`{"content":"x","refs":[{"service":"slack","url":"https://example.com/a"}]}`, true},
		{`{"content":"x","refs":[{"service":"slack","url":"HTTP://example.com/a"}]}`, true},
		{`{"content":"x","refs":[{"service":"slack","url":"javascript:alert(1)"}]}`, false},
		{`{"content":"x","refs":[{"service":"slack","url":"https:///path"}]}`, false},
		{`{"content":"x","refs":[{"service":"slack","url":"ftp://example.com/"}]}`, false},
		{`{"content":"x","refs":[{"service":"slack","url":"https://example.com/` + strings.Repeat("a", 2028) + `"}]}`, true},
		{`{"content":"x","refs":[{"service":"slack","url":"https://example.com/` + strings.Repeat("a", 2029) + `"}]}`, false},
		{`{"content":"x","version":1}`, false},
		{`{"content":"x","unknown":1}`, false},
		{`{"content":"x","kind":"event"}`, false},
		{`{"content":"x","event":null}`, false},
	} {
		w := api.do("POST", "/api/v1/action-items", api.alice, "application/json", tc.body)
		if tc.ok && w.Code != 201 || !tc.ok && (w.Code != 400 || errorCode(t, w) != "invalid_request") {
			t.Errorf("%.120s: got %d %s", tc.body, w.Code, w.Body.String())
		}
	}
	// Stored refs are the normalized form, not the raw input.
	item := api.create(api.alice, `{"content":"x","refs":[{"url":"https://example.com/","service":"slack"}]}`)
	if !reflect.DeepEqual(item.Refs, []actionItemRef{{Service: "slack", URL: "https://example.com/"}}) {
		t.Fatalf("refs: %+v", item.Refs)
	}
}

func TestActionItemList(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) { testActionItemList(t, driver) })
	}
}

func testActionItemList(t *testing.T, driver string) {
	now := time.Date(2026, 10, 23, 1, 2, 3, 0, time.UTC)
	api := newActionItemAPI(t, driver, &now)
	list := func(query string) (actionItemListResponse, *httptest.ResponseRecorder) {
		t.Helper()
		w := api.do("GET", "/api/v1/action-items"+query, api.alice, "", "")
		var out actionItemListResponse
		if w.Code == 200 {
			if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
				t.Fatal(err)
			}
		}
		return out, w
	}
	out, w := list("")
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"items":[],"next_cursor":null}` {
		t.Fatalf("empty list: %d %s", w.Code, w.Body.String())
	}
	all := map[string]bool{}
	done := map[string]bool{}
	for i := 0; i < 7; i++ {
		// Items created in the same second exercise the id tie-break.
		if i == 4 {
			now = now.Add(time.Second)
		}
		status := "open"
		if i%2 == 0 {
			status = "done"
		}
		item := api.create(api.alice, `{"content":"x","status":"`+status+`"}`)
		all[item.ID] = true
		if status == "done" {
			done[item.ID] = true
		}
	}
	for query, want := range map[string]map[string]bool{"": all, "status=done&": done} {
		seen := map[string]bool{}
		last := "9999"
		cursor := ""
		for page := 0; ; page++ {
			if page > 10 {
				t.Fatal("paging does not end")
			}
			q := "?" + query + "limit=2"
			if cursor != "" {
				q += "&cursor=" + cursor
			}
			out, w = list(q)
			if w.Code != 200 || len(out.Items) > 2 {
				t.Fatalf("%s: %d %s", q, w.Code, w.Body.String())
			}
			for _, item := range out.Items {
				if seen[item.ID] || item.CreatedAt > last {
					t.Fatalf("%s: duplicate or unordered %s", q, item.ID)
				}
				seen[item.ID] = true
				last = item.CreatedAt
			}
			if out.NextCursor == nil {
				break
			}
			if len(out.Items) != 2 {
				t.Fatalf("%s: next_cursor with a short page", q)
			}
			cursor = *out.NextCursor
		}
		if !reflect.DeepEqual(seen, want) {
			t.Fatalf("%q: listed %v, want %v", query, seen, want)
		}
	}
	if out, w = list("?limit=100"); w.Code != 200 || len(out.Items) != 7 || out.NextCursor != nil {
		t.Fatalf("limit=100: %d %d", w.Code, len(out.Items))
	}
	if out, w = list("?limit=7"); w.Code != 200 || len(out.Items) != 7 || out.NextCursor != nil {
		t.Fatalf("limit=7: %d %d %v", w.Code, len(out.Items), out.NextCursor)
	}
	if out, w = list("?status=open&limit=1"); w.Code != 200 || len(out.Items) != 1 || out.Items[0].Status != "open" || out.NextCursor == nil {
		t.Fatalf("status=open: %d %s", w.Code, w.Body.String())
	}
	b64 := base64.RawURLEncoding.EncodeToString
	for _, q := range []string{"?limit=0", "?limit=101", "?limit=abc", "?limit=05", "?limit=+5", "?limit=", "?limit=1&limit=2", "?status=", "?status=closed", "?status=open&status=done", "?sort=asc", "?cursor=", "?cursor=***", "?cursor=" + b64([]byte("abc")), "?cursor=" + b64([]byte("12:")), "?cursor=" + b64([]byte("-1:abc")), "?cursor=" + b64([]byte("+1:abc")), "?cursor=" + b64([]byte("1:a.b")), "?cursor=" + b64([]byte("99999999999999999999:abc")), "?cursor=" + b64([]byte("1:abc")) + "%3D", "?%zz"} {
		if _, w = list(q); w.Code != 400 || errorCode(t, w) != "invalid_request" {
			t.Errorf("%s: %d %s", q, w.Code, w.Body.String())
		}
	}
	if _, w = list("?cursor=" + b64([]byte("1:abc"))); w.Code != 200 {
		t.Fatalf("valid cursor: %d %s", w.Code, w.Body.String())
	}
}

func TestCandidateAndEventItems(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) { testCandidateAndEventItems(t, driver) })
	}
}

func testCandidateAndEventItems(t *testing.T, driver string) {
	api := newIntegrationAPI(t, driver)
	ctx := context.Background()
	now := api.now.Unix()
	null := func(v int64) sql.NullInt64 { return sql.NullInt64{Int64: v, Valid: true} }
	slackItem := func(id, status string) store.ActionItem {
		return store.ActionItem{ID: id, UserID: "alice-id", Content: id, Status: status, Refs: "[]", Version: 1, CreatedAt: now - 10, UpdatedAt: now - 10,
			Source: sql.NullString{String: store.SourceSlack, Valid: true}, SourceKey: sql.NullString{String: "slack:" + id, Valid: true}, Tracking: 1}
	}
	event := func(id, status string, start int64) store.ActionItem {
		e := slackItem(id, status)
		e.Tracking, e.Kind, e.EventTitle, e.EventStartAt, e.EventEndAt, e.DueAt = 0, store.KindEvent, sql.NullString{String: "MTG " + id, Valid: true}, null(start), null(start+3600), null(start)
		return e
	}
	future, passed := event("future", "open", now+3600), event("passed", "dismissed", now-3600)
	passed.AutoReason, passed.AutoAt = sql.NullString{String: store.ReasonEventPassed, Valid: true}, null(now-60)
	for _, item := range []store.ActionItem{slackItem("cand", "candidate"), slackItem("stale", "candidate"), future, passed} {
		if err := api.store.CreateActionItem(ctx, item); err != nil {
			t.Fatal(err)
		}
	}
	patch := func(id, body string) (actionItemResponse, *httptest.ResponseRecorder) {
		t.Helper()
		w := api.do("PATCH", "/api/v1/action-items/"+id, api.alice, body)
		var out actionItemResponse
		if w.Code == 200 {
			if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
				t.Fatal(err)
			}
		}
		return out, w
	}
	stored := func(id string) store.ActionItem {
		t.Helper()
		item, err := api.store.ActionItem(ctx, "alice-id", id)
		if err != nil {
			t.Fatal(err)
		}
		return item
	}

	// Candidates are listed by status.
	w := api.do("GET", "/api/v1/action-items?status=candidate", api.alice, "")
	var page actionItemListResponse
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil || len(page.Items) != 2 || page.Items[0].Status != "candidate" || page.Items[0].Kind != "task" || page.Items[0].Event != nil {
		t.Fatalf("candidates: %d %s", w.Code, w.Body.String())
	}
	// An event item returns its meeting.
	w = api.do("GET", "/api/v1/action-items/future", api.alice, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"kind":"event","event":{"title":"MTG future","start_at":"2026-10-24T02:02:03Z","end_at":"2026-10-24T03:02:03Z"}`) {
		t.Fatalf("event: %d %s", w.Code, w.Body.String())
	}

	// Sorting a candidate keeps the tracking; other changes stop it.
	for _, step := range []struct {
		status   string
		tracking int64
	}{{"open", 1}, {"candidate", 1}, {"dismissed", 0}, {"candidate", 0}} {
		item, w := patch("cand", `{"status":"`+step.status+`"}`)
		if w.Code != 200 || item.Status != step.status || stored("cand").Tracking != step.tracking {
			t.Fatalf("%s: %d %s tracking=%d", step.status, w.Code, w.Body.String(), stored("cand").Tracking)
		}
	}

	// A future event stays an event when reopened.
	if item, w := patch("future", `{"status":"done","version":1}`); w.Code != 200 || item.Kind != "event" {
		t.Fatalf("future done: %d %s", w.Code, w.Body.String())
	}
	if item, w := patch("future", `{"status":"open","version":2}`); w.Code != 200 || item.Kind != "event" || item.Event == nil || stored("future").Kind != store.KindEvent {
		t.Fatalf("future reopened: %d %s", w.Code, w.Body.String())
	}
	// A passed event reopened becomes a task and is not swept again.
	if item, w := patch("passed", `{"status":"open","version":1}`); w.Code != 200 || item.Kind != "task" || item.Event != nil || item.Auto != nil {
		t.Fatalf("passed reopened: %d %s", w.Code, w.Body.String())
	}
	if got := stored("passed"); got.Kind != store.KindTask || !got.EventStartAt.Valid {
		t.Fatalf("passed stored: %+v", got)
	}
	if n, err := api.store.ExpireEvents(ctx, "alice-id", now); err != nil || n != 0 || stored("passed").Status != "open" {
		t.Fatalf("passed swept: %d %v", n, err)
	}

	// The sweep changes the version, so an older PATCH conflicts.
	if n, err := api.store.ExpireCandidates(ctx, "alice-id", now-5, now); err != nil || n != 1 {
		t.Fatalf("expire: %d %v", n, err)
	}
	if _, w := patch("stale", `{"status":"open","version":1}`); w.Code != 409 {
		t.Fatalf("stale version: %d %s", w.Code, w.Body.String())
	}

	// Items created through the API are tasks.
	w = api.do("POST", "/api/v1/action-items", api.alice, `{"content":"x","status":"candidate"}`)
	var created actionItemResponse
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &created) != nil || created.Status != "candidate" || created.Kind != "task" || stored(created.ID).Kind != store.KindTask {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
}
