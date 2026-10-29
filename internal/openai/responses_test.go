package openai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func sse(events ...string) string {
	var b strings.Builder
	for _, e := range events {
		b.WriteString(e)
		b.WriteString("\n\n")
	}
	return b.String()
}

const completed = `event: response.completed
data: {"type":"response.completed","response":{"output":[{"type":"reasoning","content":[]},{"type":"message","content":[{"type":"output_text","text":"{\"ok\":"},{"type":"output_text","text":"true}"}]}]}}`

func streamServer(t *testing.T, status int, body string, check func(map[string]any)) *Client {
	return testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/responses" || r.Header.Get("Authorization") != "Bearer at" {
			t.Errorf("request: %s %s", r.Method, r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		var got map[string]any
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Error(err)
		}
		if check != nil {
			check(got)
		}
		if status == 200 {
			w.Header().Set("Content-Type", "text/event-stream")
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
}

var testRequest = ResponseRequest{Model: "gpt-x", Instructions: "sys", Input: "hello", SchemaName: "check", Schema: map[string]any{"type": "object"}}

func TestRespond(t *testing.T) {
	c := streamServer(t, 200, sse(`event: response.created
data: {"type":"response.created"}`, `: comment`, `event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"x"}`, completed), func(got map[string]any) {
		want := map[string]any{
			"model":        "gpt-x",
			"instructions": "sys",
			"input":        []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": "hello"}}}},
			"text":         map[string]any{"format": map[string]any{"type": "json_schema", "name": "check", "schema": map[string]any{"type": "object"}, "strict": true}},
			"reasoning":    map[string]any{"effort": "low"},
			"store":        false,
			"stream":       true,
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("body: %v", got)
		}
		for _, k := range []string{"background", "conversation", "max_output_tokens", "max_tool_calls", "metadata", "moderation", "prompt", "prompt_cache_retention", "safety_identifier", "temperature", "top_logprobs", "top_p", "truncation", "user", "previous_response_id"} {
			if _, ok := got[k]; ok {
				t.Errorf("forbidden key %s", k)
			}
		}
	})
	text, err := c.Respond(context.Background(), "at", testRequest)
	if err != nil || text != `{"ok":true}` {
		t.Fatalf("respond: %q %v", text, err)
	}

	// Multi-line data and CRLF line endings.
	c = streamServer(t, 200, "event: response.completed\r\ndata: {\"type\":\"response.completed\",\r\ndata: \"response\":{\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"a\"}]}]}}\r\n\r\n", nil)
	if text, err = c.Respond(context.Background(), "at", testRequest); err != nil || text != "a" {
		t.Fatalf("multi-line: %q %v", text, err)
	}

	for _, tc := range []struct {
		name   string
		status int
		body   string
		check  func(error) bool
	}{
		{"failed", 200, sse(`data: {"type":"response.failed","response":{"error":{"code":"subscription_sharing_usage_limit_exceeded","message":"secret text"}}}`), func(err error) bool {
			var e *APIError
			return errors.As(err, &e) && e.Status == 0 && e.Code == "subscription_sharing_usage_limit_exceeded"
		}},
		{"failed unauthorized", 200, sse(`data: {"type":"response.failed","response":{"error":{"code":"subscription_sharing_invalid_user"}}}`), func(err error) bool { return errors.Is(err, ErrUnauthorized) }},
		{"error event", 200, sse(`event: error
data: {"type":"error","code":"model_not_found","param":"model"}`), func(err error) bool {
			var e *APIError
			return errors.As(err, &e) && e.Code == "model_not_found" && e.Param == "model"
		}},
		{"incomplete", 200, sse(`data: {"type":"response.incomplete","response":{}}`), func(err error) bool { return errors.Is(err, ErrRejected) }},
		{"refusal", 200, sse(`data: {"type":"response.completed","response":{"output":[{"type":"message","content":[{"type":"refusal","refusal":"no"}]}]}}`), func(err error) bool { return errors.Is(err, ErrRefused) }},
		{"cut", 200, sse(`data: {"type":"response.created"}`) + `data: {"type":"response.comp`, func(err error) bool { return errors.Is(err, ErrTemporary) }},
		{"event too large", 200, "data: " + strings.Repeat("a", eventLimit+10) + "\n\n", func(err error) bool { return errors.Is(err, ErrRejected) }},
		{"event too large in lines", 200, strings.Repeat("data: "+strings.Repeat("a", 1000)+"\n", 1100) + "\n", func(err error) bool { return errors.Is(err, ErrRejected) }},
		{"stream too large", 200, strings.Repeat(sse(`data: {"type":"response.output_text.delta","delta":"`+strings.Repeat("a", 1000)+`"}`), 5000), func(err error) bool { return errors.Is(err, ErrRejected) }},
		{"401", 401, `{"detail":"Unauthorized"}`, func(err error) bool { return errors.Is(err, ErrUnauthorized) }},
		{"403", 403, `{"detail":{"code":"subscription_sharing_user_not_eligible"}}`, func(err error) bool {
			var e *APIError
			return errors.As(err, &e) && e.Status == 403 && e.Code == "subscription_sharing_user_not_eligible" && !errors.Is(err, ErrUnauthorized)
		}},
		{"400", 400, `{"error":{"code":"unsupported_parameter","param":"text.format","message":"SECRET"}}`, func(err error) bool {
			var e *APIError
			return errors.As(err, &e) && e.Status == 400 && e.Code == "unsupported_parameter" && e.Param == "text.format"
		}},
		{"unsafe code", 400, `{"error":{"code":"a b <SECRET>"}}`, func(err error) bool {
			var e *APIError
			return errors.As(err, &e) && e.Code == ""
		}},
	} {
		c := streamServer(t, tc.status, tc.body, nil)
		_, err := c.Respond(context.Background(), "at", testRequest)
		if !tc.check(err) {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), "secret text") || strings.Contains(err.Error(), "Bearer") {
			t.Fatalf("%s: error includes the body: %v", tc.name, err)
		}
	}
}

func TestModels(t *testing.T) {
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer at" {
			t.Errorf("request: %s %s", r.Method, r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"data":[
			{"slug":"gpt-a","display_name":"GPT A","visibility":"list"},
			{"slug":"gpt-hidden","display_name":"Hidden","visibility":"hide"},
			{"id":"gpt-b","visibility":"list"},
			{"slug":"gpt-a","display_name":"Again"},
			{"slug":"gpt-c","display_name":"GPT C"},
			{"display_name":"no slug"}
		]}`)
	}))
	models, err := c.Models(context.Background(), "at")
	want := []Model{{Slug: "gpt-a", Name: "GPT A"}, {Slug: "gpt-b", Name: "gpt-b"}, {Slug: "gpt-c", Name: "GPT C"}}
	if err != nil || !reflect.DeepEqual(models, want) {
		t.Fatalf("models: %+v %v", models, err)
	}
	// The plan route lists the models under "models".
	c = testClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"models":[{"slug":"gpt-m","display_name":"GPT M","visibility":"list"},{"slug":"gpt-x","visibility":"hide"}]}`)
	}))
	if models, err = c.Models(context.Background(), "at"); err != nil || !reflect.DeepEqual(models, []Model{{Slug: "gpt-m", Name: "GPT M"}}) {
		t.Fatalf("models key: %+v %v", models, err)
	}
	for name, h := range map[string]http.HandlerFunc{
		"401":       func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(401) },
		"no data":   func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{"items":[]}`) },
		"no slug":   func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{"models":[{"name":"x"}]}`) },
		"not array": func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `[]`) },
	} {
		c := testClient(t, h)
		_, err := c.Models(context.Background(), "at")
		switch name {
		case "401":
			if !errors.Is(err, ErrUnauthorized) {
				t.Fatalf("%s: %v", name, err)
			}
		default:
			if !errors.Is(err, ErrTemporary) {
				t.Fatalf("%s: %v", name, err)
			}
		}
	}
}
