package slacksync

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/i2bskn/astovia/internal/ai"
	"github.com/i2bskn/astovia/internal/aicred"
	"github.com/i2bskn/astovia/internal/openai"
	"github.com/i2bskn/astovia/internal/store"
)

// fakePlan is OpenAI for the ChatGPT plan: the Responses API answers with
// replies in order (the usage limit when they run out), and the token
// endpoint runs onRefresh before answering with new tokens.
type fakePlan struct {
	mu        sync.Mutex
	replies   []func(http.ResponseWriter)
	calls     int
	refreshes int
	onRefresh func()
}

func (f *fakePlan) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_, _ = io.ReadAll(r.Body)
	f.mu.Lock()
	switch r.URL.Path {
	case "/v1/responses":
		f.calls++
		reply := planLimit
		if f.calls <= len(f.replies) {
			reply = f.replies[f.calls-1]
		}
		f.mu.Unlock()
		reply(w)
	case "/api/accounts/oauth/token":
		f.refreshes++
		n, hook := f.refreshes, f.onRefresh
		f.onRefresh = nil
		f.mu.Unlock()
		if hook != nil {
			hook()
		}
		_, _ = fmt.Fprintf(w, `{"access_token":"at-%d","refresh_token":"rt-%d","token_type":"Bearer","expires_in":3600}`, n+1, n+1)
	default:
		f.mu.Unlock()
		w.WriteHeader(404)
	}
}

func (f *fakePlan) set(replies ...func(http.ResponseWriter)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replies, f.calls = replies, 0
}

func (f *fakePlan) hook(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onRefresh = fn
}

func (f *fakePlan) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls, f.refreshes
}

func planLimit(w http.ResponseWriter) {
	w.WriteHeader(429)
	_, _ = io.WriteString(w, `{"error":{"code":"subscription_sharing_usage_limit_exceeded"}}`)
}

func planText(v map[string]any) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		out, _ := json.Marshal(v)
		data, _ := json.Marshal(map[string]any{"type": "response.completed", "response": map[string]any{"output": []any{map[string]any{"type": "message", "content": []any{map[string]any{"type": "output_text", "text": string(out)}}}}}})
		_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
	}
}

func planAction(task string) func(http.ResponseWriter) {
	return planText(map[string]any{"decision": ai.DecisionTask, "task": task, "summary": "s", "event_title": "", "event_start": "", "event_end": "", "topic": "", "same_as": "", "importance": "normal"})
}

// usePlan replaces alice's AI settings with the ChatGPT plan, with an
// access token that expires after expires.
func (st *syncTest) usePlan(expires time.Duration) *fakePlan {
	st.t.Helper()
	f := &fakePlan{}
	srv := httptest.NewServer(f)
	st.t.Cleanup(srv.Close)
	now := st.now
	oc := openai.New("urn:uuid:test")
	oc.AuthBaseURL = srv.URL
	oc.Now = func() time.Time { return now }
	st.syncer.AI.OpenAI.APIBaseURL = srv.URL + "/v1"
	st.syncer.Tokens = &aicred.Tokens{Store: st.s, Box: st.syncer.Box, OpenAI: oc, Now: func() time.Time { return now }}
	sealed, err := st.syncer.Tokens.Seal("alice-id", aicred.Secret{ClientID: "oaiapp_1", Subject: "sub", RefreshToken: "rt-1", AccessToken: "at-1", ExpiresAt: now.Add(expires).Unix()})
	if err != nil {
		st.t.Fatal(err)
	}
	st.savePlan(sealed)
	return f
}

// savePlan saves the ChatGPT plan settings with the sealed secret.
func (st *syncTest) savePlan(sealed string) store.AISettings {
	st.t.Helper()
	ctx := context.Background()
	var gen int64
	if cur, err := st.s.AISettings(ctx, "alice-id"); err == nil {
		gen = cur.Generation
	}
	a, err := st.s.SaveAISettings(ctx, store.AISettings{UserID: "alice-id", Provider: ai.ProviderOpenAI, Model: "gpt-x", APIKeySealed: sealed}, gen, true, "sid", st.now.Unix())
	if err != nil {
		st.t.Fatal(err)
	}
	return a
}

// The usage limit of the ChatGPT plan is recorded with the status kept
// active; the message waits, and the AI is not called for 15 minutes.
func TestDetectionUsageLimit(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		st := newSyncTest(t, driver)
		plan := st.usePlan(time.Hour)
		before := st.aiSettings()
		st.advance(time.Minute)
		m1, m2 := st.ts(20*time.Second, 1), st.ts(10*time.Second, 1)
		st.sl.post("C1", msg{TS: m1, User: "U2", Text: "<@U1> one"})
		st.sl.post("C1", msg{TS: m2, User: "U2", Text: "<@U1> two"})
		st.sync()
		if calls, _ := plan.counts(); calls != 1 || len(st.items()) != 0 {
			t.Fatalf("calls %d, items %d", calls, len(st.items()))
		}
		a := st.aiSettings()
		if a.Status != store.AIActive || a.LastError.String != AIErrorUsageLimit || a.Generation != before.Generation || a.UpdatedAt != st.now.Unix() {
			t.Fatalf("settings: %+v", a)
		}
		// Cooling down: nothing is sent.
		st.advance(14 * time.Minute)
		st.sync()
		if calls, _ := plan.counts(); calls != 1 || len(st.items()) != 0 {
			t.Fatalf("cooling down: calls %d, items %d", calls, len(st.items()))
		}
		// Afterwards the messages are judged and the error is cleared.
		st.advance(time.Minute)
		plan.set(planAction("a"), planAction("b"))
		st.sync()
		if calls, _ := plan.counts(); calls != 2 || len(st.items()) != 2 {
			t.Fatalf("after cooling down: calls %d, items %d", calls, len(st.items()))
		}
		if a = st.aiSettings(); a.Status != store.AIActive || a.LastError.Valid {
			t.Fatalf("settings after: %+v", a)
		}
	})
}

// Settings replaced while the token is refreshed are handled like settings
// replaced before the call: the message waits and no status is recorded.
func TestDetectionStaleToken(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		st := newSyncTest(t, driver)
		plan := st.usePlan(time.Minute)
		st.advance(time.Minute)
		st.sl.post("C1", msg{TS: st.ts(10*time.Second, 1), User: "U2", Text: "<@U1> one"})
		before := st.aiSettings()
		now := st.now.Unix()
		plan.hook(func() {
			if _, err := st.s.SaveAISettings(context.Background(), store.AISettings{UserID: "alice-id", Provider: ai.ProviderOpenAI, Model: "gpt-y", APIKeySealed: before.APIKeySealed}, before.Generation, false, "sid", now); err != nil {
				t.Error(err)
			}
		})
		st.sync()
		if calls, refreshes := plan.counts(); calls != 0 || refreshes != 1 || len(st.items()) != 0 {
			t.Fatalf("calls %d, refreshes %d, items %d", calls, refreshes, len(st.items()))
		}
		if a := st.aiSettings(); a.Generation != before.Generation+1 || a.Status != store.AIActive || a.LastError.Valid {
			t.Fatalf("settings: %+v", a)
		}
	})
}

// A usage limit while tracking leaves the item to the next pass.
func TestTrackUsageLimit(t *testing.T) {
	drivers(t, func(t *testing.T, driver string) {
		st := newSyncTest(t, driver)
		key, origin := st.trackedMention()
		plan := st.usePlan(time.Hour)
		before := st.mustItem(key)
		st.advance(5 * time.Minute)
		st.reply("C1", origin, "送りました", time.Minute)
		st.sync()
		if calls, _ := plan.counts(); calls != 1 {
			t.Fatalf("calls %d", calls)
		}
		if item := st.mustItem(key); item.Status != "open" || item.TrackedAt != before.TrackedAt {
			t.Fatalf("item: %+v", item)
		}
		if a := st.aiSettings(); a.Status != store.AIActive || a.LastError.String != AIErrorUsageLimit {
			t.Fatalf("settings: %+v", a)
		}
	})
}
