package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/i2bskn/astovia/internal/openai"
)

// fakeOpenAI answers the Responses API with the replies in order (the last
// one repeats) and records the bearer tokens.
type fakeOpenAI struct {
	mu      sync.Mutex
	replies []func(w http.ResponseWriter)
	tokens  []string
	bodies  []map[string]any
}

func (f *fakeOpenAI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	f.mu.Lock()
	f.tokens = append(f.tokens, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	f.bodies = append(f.bodies, body)
	reply := f.replies[min(len(f.tokens), len(f.replies))-1]
	f.mu.Unlock()
	reply(w)
}

func (f *fakeOpenAI) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.tokens...)
}

func completedWith(text string) func(w http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		data, _ := json.Marshal(map[string]any{"type": "response.completed", "response": map[string]any{"output": []any{map[string]any{"type": "message", "content": []any{map[string]any{"type": "output_text", "text": text}}}}}})
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "event: response.completed\ndata: %s\n\n", data)
	}
}

func statusWith(status int, body string) func(w http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

func failedWith(code string) func(w http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":%q}}}\n\n", code)
	}
}

func openAIClient(t *testing.T, replies ...func(w http.ResponseWriter)) (*Client, *fakeOpenAI) {
	t.Helper()
	f := &fakeOpenAI{replies: replies}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c := New()
	c.OpenAI.APIBaseURL = srv.URL
	return c, f
}

// tokenSource returns tokens "at-1", "at-2", ... and records the failed
// tokens it was given.
type tokenSource struct {
	mu     sync.Mutex
	n      int
	failed []string
}

func (s *tokenSource) cred() Credential {
	return Credential{Provider: ProviderOpenAI, Token: func(_ context.Context, failed string) (string, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.failed = append(s.failed, failed)
		s.n++
		return fmt.Sprintf("at-%d", s.n), nil
	}}
}

const judgmentJSON = `{"decision":"task","task":"返信する","summary":"s","event_title":"","event_start":"","event_end":"","topic":"","same_as":""}`

func TestJudgeOpenAI(t *testing.T) {
	c, f := openAIClient(t, completedWith(judgmentJSON))
	src := &tokenSource{}
	res, err := c.JudgeSlack(context.Background(), src.cred(), "gpt-x", slackInput())
	if err != nil || res.Decision != DecisionTask || res.Task != "返信する" {
		t.Fatalf("judge: %+v %v", res, err)
	}
	body := f.bodies[0]
	if body["model"] != "gpt-x" || body["instructions"] != SlackSystemPrompt || body["store"] != false || body["stream"] != true {
		t.Fatalf("body: %v", body)
	}
	if name := body["text"].(map[string]any)["format"].(map[string]any)["name"]; name != "judgment" {
		t.Fatalf("schema name: %v", name)
	}
	if got := f.seen(); len(got) != 1 || got[0] != "at-1" || len(src.failed) != 1 || src.failed[0] != "" {
		t.Fatalf("tokens: %v %v", got, src.failed)
	}

	c, f = openAIClient(t, completedWith(`{"resolved":true,"reason":"r"}`))
	if r, err := c.JudgeReply(context.Background(), src.cred(), "gpt-x", ReplyInput{Task: "x", Posts: slackInput().Posts}); err != nil || !r.Resolved {
		t.Fatalf("reply: %+v %v", r, err)
	}
	if name := f.bodies[0]["text"].(map[string]any)["format"].(map[string]any)["name"]; name != "reply" {
		t.Fatalf("reply schema name: %v", name)
	}

	// An unknown provider.
	if _, err := c.JudgeSlack(context.Background(), Credential{Provider: "x"}, "gpt-x", slackInput()); !errors.Is(err, ErrKeyInvalid) {
		t.Fatalf("unknown provider: %v", err)
	}
}

func TestOpenAIRetryAfter401(t *testing.T) {
	c, f := openAIClient(t, statusWith(401, `{"detail":"Unauthorized"}`), completedWith(judgmentJSON))
	src := &tokenSource{}
	if _, err := c.JudgeSlack(context.Background(), src.cred(), "gpt-x", slackInput()); err != nil {
		t.Fatal(err)
	}
	if got := f.seen(); len(got) != 2 || got[0] != "at-1" || got[1] != "at-2" {
		t.Fatalf("tokens: %v", got)
	}
	if len(src.failed) != 2 || src.failed[0] != "" || src.failed[1] != "at-1" {
		t.Fatalf("failed: %v", src.failed)
	}

	// Refused twice: the token is invalid, and no third request is made.
	c, f = openAIClient(t, statusWith(401, ``), failedWith("subscription_sharing_invalid_user"))
	src = &tokenSource{}
	if _, err := c.JudgeSlack(context.Background(), src.cred(), "gpt-x", slackInput()); !errors.Is(err, ErrKeyInvalid) {
		t.Fatalf("twice: %v", err)
	}
	if len(f.seen()) != 2 || len(src.failed) != 2 {
		t.Fatalf("calls: %v %v", f.seen(), src.failed)
	}
}

func TestOpenAIErrors(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reply func(w http.ResponseWriter)
		want  error
	}{
		{"not eligible", statusWith(403, `{"detail":{"code":"subscription_sharing_user_not_eligible"}}`), ErrBilling},
		{"usage limit", statusWith(429, `{"error":{"code":"subscription_sharing_usage_limit_exceeded"}}`), ErrUsageLimit},
		{"usage limit in stream", failedWith("subscription_sharing_usage_limit_exceeded"), ErrUsageLimit},
		{"usage unavailable", statusWith(503, `{"error":{"code":"subscription_sharing_usage_unavailable"}}`), ErrTemporary},
		{"user unavailable", failedWith("subscription_sharing_user_unavailable"), ErrTemporary},
		{"other 429", statusWith(429, ``), ErrTemporary},
		{"500", statusWith(500, ``), ErrTemporary},
		{"408", statusWith(408, ``), ErrTemporary},
		{"unsupported", statusWith(400, `{"error":{"code":"subscription_sharing_unsupported_capability","param":"text.format"}}`), ErrUnusable},
		{"route", statusWith(400, `{"error":{"code":"subscription_sharing_route_not_supported"}}`), ErrUnusable},
		{"scope", statusWith(403, `{"error":{"code":"chatpass_v2_scope_not_authorized"}}`), ErrKeyInvalid},
		{"context", statusWith(400, `{"error":{"code":"chatpass_v2_invalid_authorization_context"}}`), ErrKeyInvalid},
		{"403", statusWith(403, ``), ErrKeyInvalid},
		{"404", statusWith(404, ``), ErrModelUnavailable},
		{"model_not_found", failedWith("model_not_found"), ErrModelUnavailable},
		{"other 400", statusWith(400, `{"error":{"code":"invalid_value"}}`), ErrUnusable},
		{"incomplete", func(w http.ResponseWriter) {
			_, _ = io.WriteString(w, "data: {\"type\":\"response.incomplete\",\"response\":{}}\n\n")
		}, ErrUnusable},
		{"refusal", func(w http.ResponseWriter) {
			_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"refusal\"}]}]}}\n\n")
		}, ErrUnusable},
		{"cut", func(w http.ResponseWriter) { _, _ = io.WriteString(w, "data: {\"type\":\"response.created\"}\n\n") }, ErrTemporary},
		{"not json", completedWith("not json"), ErrUnusable},
	} {
		c, _ := openAIClient(t, tc.reply)
		src := &tokenSource{}
		_, err := c.JudgeSlack(context.Background(), src.cred(), "gpt-x", slackInput())
		if !errors.Is(err, tc.want) {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if strings.Contains(err.Error(), "at-1") {
			t.Fatalf("%s: error includes the token: %v", tc.name, err)
		}
	}
}

// stale is like aicred.ErrStale: it wraps ErrTemporary.
var stale = fmt.Errorf("aicred: stale: %w", ErrTemporary)

func TestOpenAITokenErrors(t *testing.T) {
	c, f := openAIClient(t, completedWith(judgmentJSON))
	for _, tc := range []struct {
		name string
		err  error
		want error
	}{
		{"stale", stale, stale},
		{"key invalid", fmt.Errorf("%w (refresh)", ErrKeyInvalid), ErrKeyInvalid},
		{"unclassified", errors.New("boom"), ErrTemporary},
		{"context", context.DeadlineExceeded, ErrTemporary},
	} {
		cred := Credential{Provider: ProviderOpenAI, Token: func(context.Context, string) (string, error) { return "", tc.err }}
		_, err := c.JudgeSlack(context.Background(), cred, "gpt-x", slackInput())
		if !errors.Is(err, tc.want) || (tc.name == "context" && errors.Is(err, context.DeadlineExceeded)) {
			t.Fatalf("%s: %v", tc.name, err)
		}
	}
	if len(f.seen()) != 0 {
		t.Fatalf("requests without a token: %v", f.seen())
	}
	// No token source at all.
	if _, err := c.JudgeSlack(context.Background(), Credential{Provider: ProviderOpenAI}, "gpt-x", slackInput()); !errors.Is(err, ErrKeyInvalid) {
		t.Fatalf("no source: %v", err)
	}
}

func TestOpenAITimeout(t *testing.T) {
	c, _ := openAIClient(t, completedWith(judgmentJSON))
	c.OpenAITimeout = 50 * time.Millisecond
	// The timeout covers getting the token.
	cred := Credential{Provider: ProviderOpenAI, Token: func(ctx context.Context, _ string) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}}
	start := time.Now()
	_, err := c.JudgeSlack(context.Background(), cred, "gpt-x", slackInput())
	if !errors.Is(err, ErrTemporary) || errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 5*time.Second {
		t.Fatalf("token timeout: %v", err)
	}
	// And the request, with the retry after a 401.
	slow := func(w http.ResponseWriter) { time.Sleep(200 * time.Millisecond); statusWith(401, ``)(w) }
	c, f := openAIClient(t, slow)
	c.OpenAITimeout = 300 * time.Millisecond
	src := &tokenSource{}
	if _, err = c.JudgeSlack(context.Background(), src.cred(), "gpt-x", slackInput()); !errors.Is(err, ErrTemporary) {
		t.Fatalf("request timeout: %v", err)
	}
	if n := len(f.seen()); n != 2 {
		t.Fatalf("requests: %d", n)
	}
	// The caller's context error is returned as it is.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = c.JudgeSlack(ctx, src.cred(), "gpt-x", slackInput()); !errors.Is(err, context.Canceled) || errors.Is(err, ErrTemporary) {
		t.Fatalf("caller canceled: %v", err)
	}
}

func TestCheckOpenAI(t *testing.T) {
	c, f := openAIClient(t, completedWith(`{"ok":true}`))
	src := &tokenSource{}
	if err := c.CheckOpenAI(context.Background(), src.cred(), "gpt-x"); err != nil {
		t.Fatal(err)
	}
	body := f.bodies[0]
	if body["instructions"] != `Reply with {"ok": true}.` || body["text"].(map[string]any)["format"].(map[string]any)["name"] != "check" {
		t.Fatalf("body: %v", body)
	}
	for _, text := range []string{`{"ok":false}`, `{}`, `{"ok":true,"x":1}`, `nope`} {
		c, _ := openAIClient(t, completedWith(text))
		if err := c.CheckOpenAI(context.Background(), src.cred(), "gpt-x"); !errors.Is(err, ErrUnusable) {
			t.Fatalf("%s: %v", text, err)
		}
	}
	c, _ = openAIClient(t, statusWith(403, `{"detail":{"code":"subscription_sharing_user_not_eligible"}}`))
	if err := c.CheckOpenAI(context.Background(), src.cred(), "gpt-x"); !errors.Is(err, ErrBilling) {
		t.Fatalf("billing: %v", err)
	}
}

func TestOpenAIModels(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(401)
			return
		}
		_, _ = io.WriteString(w, `{"data":[{"slug":"gpt-a","display_name":"A"},{"slug":"bad slug"},{"slug":"`+strings.Repeat("a", 129)+`"},{"id":"gpt-b"}]}`)
	}))
	t.Cleanup(srv.Close)
	c := New()
	c.OpenAI.APIBaseURL = srv.URL
	src := &tokenSource{}
	models, err := c.OpenAIModels(context.Background(), src.cred())
	if err != nil || len(models) != 2 || models[0] != (openai.Model{Slug: "gpt-a", Name: "A"}) || models[1].Slug != "gpt-b" {
		t.Fatalf("models: %+v %v", models, err)
	}
	if len(src.failed) != 2 || src.failed[1] != "at-1" {
		t.Fatalf("failed: %v", src.failed)
	}
}

func TestValidOpenAIModel(t *testing.T) {
	for v, want := range map[string]bool{"gpt-5.5": true, "o4-mini:2025_01": true, "": false, "a b": false, "a/b": false, strings.Repeat("a", 128): true, strings.Repeat("a", 129): false} {
		if ValidOpenAIModel(v) != want {
			t.Fatalf("%q", v)
		}
	}
}

// The schemas meet the requirements of strict structured outputs: every
// object lists all of its properties as required and allows no others.
func TestSchemasAreStrict(t *testing.T) {
	var check func(path string, s map[string]any)
	check = func(path string, s map[string]any) {
		if s["type"] != "object" {
			return
		}
		props, _ := s["properties"].(map[string]any)
		required, _ := s["required"].([]string)
		keys := make([]string, 0, len(props))
		for k, v := range props {
			keys = append(keys, k)
			if sub, ok := v.(map[string]any); ok {
				check(path+"."+k, sub)
			}
		}
		sort.Strings(keys)
		got := append([]string(nil), required...)
		sort.Strings(got)
		if strings.Join(keys, ",") != strings.Join(got, ",") {
			t.Fatalf("%s: required %v, properties %v", path, got, keys)
		}
		if s["additionalProperties"] != false {
			t.Fatalf("%s: additionalProperties", path)
		}
	}
	for name, s := range map[string]map[string]any{"output": outputSchema, "reply": replySchema, "check": checkSchema} {
		check(name, s)
	}
}
