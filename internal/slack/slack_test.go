package slack

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

var testNow = time.Date(2026, 10, 24, 10, 0, 0, 0, time.UTC)

const testToken = "xoxp-secret-token-0123"

type fakeSlack struct {
	mu       sync.Mutex
	requests []*http.Request
	handler  http.HandlerFunc
}

func newFake(t *testing.T, h http.HandlerFunc) (*fakeSlack, *Client) {
	f := &fakeSlack{handler: h}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests = append(f.requests, r)
		f.mu.Unlock()
		f.handler(w, r)
	}))
	t.Cleanup(srv.Close)
	c := New(func() time.Time { return testNow })
	c.BaseURL = srv.URL + "/api"
	return f, c
}

func (f *fakeSlack) last() *http.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[len(f.requests)-1]
}

func reply(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}
}

func TestAuthTestAndRequests(t *testing.T) {
	f, c := newFake(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-OAuth-Scopes", "search:read, users:read,search:read, ,im:history")
		_, _ = w.Write([]byte(`{"ok":true,"url":"https://acme.slack.com/","team":"Acme","user":"alice","team_id":"T1","user_id":"U1"}`))
	})
	id, scopes, err := c.AuthTest(context.Background(), testToken)
	if err != nil || id != (Auth{UserID: "U1", User: "alice", TeamID: "T1", Team: "Acme", URL: "https://acme.slack.com/"}) || !reflect.DeepEqual(scopes, []string{"im:history", "search:read", "users:read"}) {
		t.Fatalf("AuthTest: %+v %v %v", id, scopes, err)
	}
	r := f.last()
	if r.Method != http.MethodGet || r.URL.Path != "/api/auth.test" || r.Header.Get("Authorization") != "Bearer "+testToken || r.URL.RawQuery != "" {
		t.Fatalf("request: %s %s %v", r.Method, r.URL, r.Header)
	}

	// Read methods send their arguments in the query, never the token.
	f, c = newFake(t, reply(`{"ok":true,"messages":[{"ts":"1.000002","user":"U2","text":"hi","thread_ts":"1.000001","latest_reply":"2.000000","subtype":"thread_broadcast"},{"ts":"1.000001","bot_id":"B1","text":"bot"}],"has_more":true,"response_metadata":{"next_cursor":"next"}}`))
	page, err := c.Replies(context.Background(), testToken, HistoryParams{Channel: "C1", TS: "1.000001", Oldest: "0.000000", Limit: 100, Cursor: "abc"})
	if err != nil || !page.HasMore || page.NextCursor != "next" || len(page.Messages) != 2 {
		t.Fatalf("Replies: %+v %v", page, err)
	}
	m := page.Messages[0]
	if m.TS != "1.000002" || m.User != "U2" || m.ThreadTS != "1.000001" || m.LatestReply != "2.000000" || m.Subtype != "thread_broadcast" || !m.HasSubtype || m.HasBotID {
		t.Fatalf("message: %+v", m)
	}
	if b := page.Messages[1]; !b.HasBotID || b.BotID != "B1" || b.HasSubtype {
		t.Fatalf("bot message: %+v", b)
	}
	r = f.last()
	q := r.URL.Query()
	if r.URL.Path != "/api/conversations.replies" || q.Get("channel") != "C1" || q.Get("ts") != "1.000001" || q.Get("oldest") != "0.000000" || q.Get("limit") != "100" || q.Get("cursor") != "abc" || q.Has("latest") || q.Has("inclusive") || strings.Contains(r.URL.RawQuery, "xoxp") {
		t.Fatalf("replies query: %s", r.URL)
	}
	if _, err = c.History(context.Background(), testToken, HistoryParams{Channel: "C1", TS: "ignored", Latest: "5.000000", Inclusive: true, Limit: 1}); err != nil {
		t.Fatal(err)
	}
	q = f.last().URL.Query()
	if f.last().URL.Path != "/api/conversations.history" || q.Has("ts") || q.Get("latest") != "5.000000" || q.Get("inclusive") != "true" {
		t.Fatalf("history query: %s", f.last().URL)
	}

	f, c = newFake(t, reply(`{"ok":true,"messages":{"matches":[{"ts":"3.000001","user":"U2","text":"<@U1> hi","permalink":"https://acme.slack.com/archives/C1/p3000001","channel":{"id":"C1","name":"general","is_im":false,"is_mpim":true}}],"paging":{"page":1,"pages":3}}}`))
	sp, err := c.SearchMessages(context.Background(), testToken, "<@U1> after:2026-10-20", 100, 1)
	if err != nil || sp.Pages != 3 || len(sp.Matches) != 1 || sp.Matches[0].Channel != (Channel{ID: "C1", Name: "general", IsMPIM: true}) || sp.Matches[0].Permalink == "" {
		t.Fatalf("SearchMessages: %+v %v", sp, err)
	}
	q = f.last().URL.Query()
	if q.Get("query") != "<@U1> after:2026-10-20" || q.Get("sort") != "timestamp" || q.Get("sort_dir") != "desc" || q.Get("count") != "100" || q.Get("page") != "1" {
		t.Fatalf("search query: %s", f.last().URL)
	}

	_, c = newFake(t, reply(`{"ok":true,"user":{"id":"U2","name":"bob","real_name":"Bob Old","profile":{"display_name":"bobby","real_name":"Bob B"}}}`))
	if u, err := c.UserInfo(context.Background(), testToken, "U2"); err != nil || u != (User{ID: "U2", Name: "bob", RealName: "Bob B", DisplayName: "bobby"}) {
		t.Fatalf("UserInfo: %+v %v", u, err)
	}
	_, c = newFake(t, reply(`{"ok":true,"channels":[{"id":"G1","name":"mpdm-a--b-1","is_mpim":true}]}`))
	if list, err := c.UserConversations(context.Background(), testToken, "mpim", 200); err != nil || len(list) != 1 || list[0].ID != "G1" {
		t.Fatalf("UserConversations: %+v %v", list, err)
	}
	_, c = newFake(t, reply(`{"ok":true,"channel":"C1","permalink":"https://acme.slack.com/archives/C1/p1"}`))
	if link, err := c.Permalink(context.Background(), testToken, "C1", "1.000001"); err != nil || link != "https://acme.slack.com/archives/C1/p1" {
		t.Fatalf("Permalink: %q %v", link, err)
	}
}

func TestErrors(t *testing.T) {
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
		check   func(error) bool
	}{
		{"invalid_auth", reply(`{"ok":false,"error":"invalid_auth"}`), func(e error) bool { return errors.Is(e, ErrUnauthorized) }},
		{"token_revoked", reply(`{"ok":false,"error":"token_revoked"}`), func(e error) bool { return errors.Is(e, ErrUnauthorized) }},
		{"account_inactive", reply(`{"ok":false,"error":"account_inactive"}`), func(e error) bool { return errors.Is(e, ErrUnauthorized) }},
		{"missing_scope", reply(`{"ok":false,"error":"missing_scope","needed":"im:history"}`), func(e error) bool { return errors.Is(e, ErrMissingScope) }},
		{"channel_not_found", reply(`{"ok":false,"error":"channel_not_found"}`), func(e error) bool {
			var a *APIError
			return errors.As(e, &a) && a.Code == "channel_not_found" && a.Method == "conversations.history" && !a.Temporary()
		}},
		{"internal_error", reply(`{"ok":false,"error":"internal_error"}`), func(e error) bool { var a *APIError; return errors.As(e, &a) && a.Temporary() }},
		{"429 with Retry-After", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(429)
		}, func(e error) bool {
			var r *RateLimitError
			return errors.As(e, &r) && r.RetryAt.Equal(testNow.Add(30*time.Second)) && errors.Is(e, ErrRateLimited)
		}},
		{"429 without Retry-After", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(429) }, func(e error) bool { var r *RateLimitError; return errors.As(e, &r) && r.RetryAt.IsZero() }},
		{"500", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) }, func(e error) bool { var s *StatusError; return errors.As(e, &s) && s.Code == 500 }},
		{"redirect is not followed", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Location", "https://evil.example/")
			w.WriteHeader(302)
		}, func(e error) bool { var s *StatusError; return errors.As(e, &s) && s.Code == 302 }},
		{"invalid JSON", reply(`{"ok":`), func(e error) bool { return e != nil && strings.Contains(e.Error(), "invalid JSON") }},
		{"too large", reply(`{"ok":true,"x":"` + strings.Repeat("a", bodyLimit) + `"}`), func(e error) bool { return e != nil && strings.Contains(e.Error(), "too large") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, c := newFake(t, tc.handler)
			_, err := c.History(context.Background(), testToken, HistoryParams{Channel: "C1", Oldest: "secret-query-value"})
			if err == nil || !tc.check(err) {
				t.Fatalf("error: %v", err)
			}
			// Errors never carry the token, the query or the body.
			for _, s := range []string{testToken, "secret-query-value", "needed", "evil.example", "aaaa"} {
				if strings.Contains(err.Error(), s) {
					t.Fatalf("error %q contains %q", err, s)
				}
			}
			if n := len(f.requests); n != 1 {
				t.Fatalf("requests: %d", n)
			}
		})
	}

	// A connection error keeps only its cause, without the URL.
	c := New(func() time.Time { return testNow })
	c.BaseURL = "http://127.0.0.1:1/api"
	_, err := c.History(context.Background(), testToken, HistoryParams{Channel: "C1", Oldest: "secret-query-value"})
	if err == nil || strings.Contains(err.Error(), "secret-query-value") || strings.Contains(err.Error(), testToken) || !strings.HasPrefix(err.Error(), "slack: conversations.history: ") {
		t.Fatalf("connection error: %v", err)
	}
	// The caller's context error stays visible.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = c.History(ctx, testToken, HistoryParams{Channel: "C1"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled: %v", err)
	}
}
