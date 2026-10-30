package githubsync

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/i2bskn/astovia/internal/ai"
	"github.com/i2bskn/astovia/internal/aicred"
	"github.com/i2bskn/astovia/internal/store"
)

// fakeEndpoint is the Chat Completions API of an OpenAI-compatible
// endpoint: it answers with the judgments in order (the last one repeats)
// and records the bearer tokens.
type fakeEndpoint struct {
	mu      sync.Mutex
	answers []string
	auth    []string
}

func (f *fakeEndpoint) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_, _ = io.ReadAll(r.Body)
	f.mu.Lock()
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	answer := f.answers[min(len(f.auth), len(f.answers))-1]
	f.mu.Unlock()
	if r.URL.Path != "/v1/chat/completions" {
		w.WriteHeader(404)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": answer}, "finish_reason": "stop"}}})
}

func (f *fakeEndpoint) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.auth...)
}

func endpointTask(task string) string {
	out, _ := json.Marshal(map[string]any{"decision": ai.DecisionTask, "task": task, "summary": "s", "event_title": "", "event_start": "", "event_end": "", "topic": "", "same_as": "", "importance": "normal"})
	return string(out)
}

// useEndpoint replaces alice's AI settings with an OpenAI-compatible
// endpoint on the loopback address, which the synchronization may reach.
func (st *syncTest) useEndpoint(answers ...string) *fakeEndpoint {
	st.t.Helper()
	f := &fakeEndpoint{answers: answers}
	srv := httptest.NewServer(f)
	st.t.Cleanup(srv.Close)
	st.syncer.AI.AllowPrivateEndpoints = true
	st.syncer.AI.Endpoints = ai.NewEndpointClient(true)
	sealed, err := aicred.SealEndpoint(st.syncer.Box, "alice-id", aicred.Endpoint{BaseURL: srv.URL + "/v1", APIKey: "ep-key-0123"})
	if err != nil {
		st.t.Fatal(err)
	}
	ctx := context.Background()
	cur, err := st.s.AISettings(ctx, "alice-id")
	if err != nil {
		st.t.Fatal(err)
	}
	if _, err = st.s.SaveAISettings(ctx, store.AISettings{UserID: "alice-id", Provider: ai.ProviderOpenAICompatible, Model: "llama3", APIKeySealed: sealed}, cur.Generation, true, "sid", st.now.Unix()); err != nil {
		st.t.Fatal(err)
	}
	return f
}

// Mentions are judged through an OpenAI-compatible endpoint.
func TestMentionOpenAICompatible(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		st, fa := newMentionTest(t, driver)
		f := st.useEndpoint(endpointTask("一つ目に答える"), endpointTask("二つ目に答える"))
		st.mentionsOfBob()
		st.advance(time.Minute)
		st.sync()
		if auth := f.seen(); len(auth) != 2 || auth[0] != "Bearer ep-key-0123" || fa.count() != 0 {
			t.Fatalf("endpoint calls %v, Anthropic calls %d", auth, fa.count())
		}
		if a, b := st.mustItem(mentionKey(1, "c11")), st.mustItem(mentionKey(1, "c12")); a.Content != "一つ目に答える" || b.Content != "二つ目に答える" {
			t.Fatalf("items: %+v %+v", a, b)
		}
		if a := st.aiSettings(); a.Status != store.AIActive || a.LastError.Valid {
			t.Fatalf("settings: %+v", a)
		}
	})
}

// An endpoint that may no longer be reached stops the AI with key_invalid
// instead of being retried.
func TestMentionEndpointBlocked(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		st, _ := newMentionTest(t, driver)
		f := st.useEndpoint(endpointTask("x"))
		st.syncer.AI.AllowPrivateEndpoints = false
		st.mentionsOfBob()
		st.advance(time.Minute)
		st.sync()
		if a := st.aiSettings(); len(f.seen()) != 0 || a.Status != store.AIError || a.LastError.String != AIErrorKeyInvalid {
			t.Fatalf("%d calls, settings %+v", len(f.seen()), a)
		}
	})
}
