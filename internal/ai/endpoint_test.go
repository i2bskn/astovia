package ai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

const endpointKey = "ep-SECRET-key-0123456789"

// fakeEndpoint answers the Chat Completions API with the replies in order
// (the last one repeats) and records the requests.
type fakeEndpoint struct {
	mu       sync.Mutex
	replies  []func(w http.ResponseWriter, r *http.Request)
	requests []recorded
}

func (f *fakeEndpoint) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.requests = append(f.requests, recorded{method: r.Method, path: r.URL.Path, header: r.Header.Clone(), body: body})
	reply := f.replies[min(len(f.requests), len(f.replies))-1]
	f.mu.Unlock()
	reply(w, r)
}

func (f *fakeEndpoint) all() []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recorded(nil), f.requests...)
}

func (f *fakeEndpoint) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

// newEndpoint starts a fake endpoint and returns a client that may reach
// it (http on the loopback address).
func newEndpoint(t *testing.T, replies ...func(w http.ResponseWriter, r *http.Request)) (*fakeEndpoint, *Client, string) {
	t.Helper()
	f := &fakeEndpoint{replies: replies}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c := New()
	c.AllowPrivateEndpoints = true
	c.Endpoints = NewEndpointClient(true)
	c.OpenAIAPIBaseURL = srv.URL + "/v1"
	return f, c, srv.URL
}

func chatAnswer(content string) func(http.ResponseWriter, *http.Request) {
	return chatBody(map[string]any{"id": "c1", "object": "chat.completion", "choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": content, "refusal": nil}, "finish_reason": "stop"}}})
}

func chatBody(v any) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
}

func chatStatus(status int, body string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

const noneAnswer = `{"decision":"none","task":"","summary":"x","event_title":"","event_start":"","event_end":"","topic":"","same_as":"","importance":"normal"}`

func bodyKeys(t *testing.T, raw []byte) (map[string]any, []string) {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(body))
	for k := range body {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return body, keys
}

func TestJudgeEndpoint(t *testing.T) {
	f, c, url := newEndpoint(t, chatAnswer(`{"decision":"task","task":"回答する","summary":"質問","event_title":"","event_start":"","event_end":"","topic":"","same_as":"","importance":"high"}`))
	cred := Credential{Provider: ProviderOpenAICompatible, Key: endpointKey, BaseURL: url + "/v1"}
	got, err := c.JudgeMention(context.Background(), cred, "meta-llama/Llama-3.1-8B-Instruct", testInput())
	if err != nil || got.Decision != DecisionTask || got.Task != "回答する" || got.Importance != ImportanceHigh {
		t.Fatalf("%+v %v", got, err)
	}
	r := f.all()[0]
	if r.method != "POST" || r.path != "/v1/chat/completions" {
		t.Fatalf("request: %s %s", r.method, r.path)
	}
	if r.header.Get("Authorization") != "Bearer "+endpointKey || r.header.Get("Content-Type") != "application/json" || r.header.Get("Accept") != "application/json" || r.header.Get("User-Agent") != "astovia" {
		t.Fatalf("headers: %v", r.header)
	}
	body, keys := bodyKeys(t, r.body)
	// Nothing else is sent: no temperature, max_tokens, reasoning_effort,
	// store or stream.
	if !reflect.DeepEqual(keys, []string{"messages", "model", "response_format"}) || body["model"] != "meta-llama/Llama-3.1-8B-Instruct" {
		t.Fatalf("body: %s", r.body)
	}
	messages := body["messages"].([]any)
	if len(messages) != 2 || messages[0].(map[string]any)["role"] != "system" || messages[0].(map[string]any)["content"] == "" || messages[1].(map[string]any)["role"] != "user" || !strings.Contains(messages[1].(map[string]any)["content"].(string), "octocat") {
		t.Fatalf("messages: %v", messages)
	}
	format := body["response_format"].(map[string]any)
	schema := format["json_schema"].(map[string]any)
	if format["type"] != "json_schema" || schema["name"] != judgmentSchemaName || schema["strict"] != true || schema["schema"] == nil {
		t.Fatalf("response_format: %v", format)
	}

	// Without a key, no Authorization header is sent.
	cred.Key = ""
	if _, err = c.JudgeMention(context.Background(), cred, "llama3", testInput()); err != nil {
		t.Fatal(err)
	}
	if r = f.all()[1]; r.header.Get("Authorization") != "" {
		t.Fatalf("authorization without a key: %v", r.header)
	}
}

func TestJudgeOpenAIAPI(t *testing.T) {
	f, c, _ := newEndpoint(t, chatAnswer(noneAnswer))
	cred := Credential{Provider: ProviderOpenAIAPI, Key: endpointKey}
	if _, err := c.JudgeMention(context.Background(), cred, "gpt-5-mini", testInput()); err != nil {
		t.Fatal(err)
	}
	r := f.all()[0]
	body, keys := bodyKeys(t, r.body)
	if r.path != "/v1/chat/completions" || r.header.Get("Authorization") != "Bearer "+endpointKey || !reflect.DeepEqual(keys, []string{"messages", "model", "reasoning_effort", "response_format"}) || body["reasoning_effort"] != "low" {
		t.Fatalf("request: %s %s", r.path, r.body)
	}
}

func TestOpenAIAPIReasoningEffortRetry(t *testing.T) {
	for _, refusal := range []string{
		`{"error":{"message":"Unsupported parameter: 'reasoning_effort'","type":"invalid_request_error","param":"reasoning_effort","code":"unsupported_parameter"}}`,
		`{"error":{"message":"x","type":"invalid_request_error","param":"reasoning_effort","code":"unsupported_value"}}`,
		`{"error":{"message":"x","type":"invalid_request_error","param":"reasoning_effort","code":null}}`,
	} {
		f, c, _ := newEndpoint(t, chatStatus(400, refusal), chatAnswer(noneAnswer))
		if _, err := c.JudgeMention(context.Background(), Credential{Provider: ProviderOpenAIAPI, Key: endpointKey}, "gpt-4.1-mini", testInput()); err != nil {
			t.Fatalf("%s: %v", refusal, err)
		}
		requests := f.all()
		if len(requests) != 2 {
			t.Fatalf("%d requests", len(requests))
		}
		first, _ := bodyKeys(t, requests[0].body)
		second, keys := bodyKeys(t, requests[1].body)
		if first["reasoning_effort"] != "low" || second["reasoning_effort"] != nil || !reflect.DeepEqual(keys, []string{"messages", "model", "response_format"}) {
			t.Fatalf("bodies: %s / %s", requests[0].body, requests[1].body)
		}
	}

	// The retry is made once, and only for a 400 about the reasoning
	// effort.
	f, c, _ := newEndpoint(t, chatStatus(400, `{"error":{"param":"reasoning_effort","code":"unsupported_parameter"}}`))
	if _, err := c.JudgeMention(context.Background(), Credential{Provider: ProviderOpenAIAPI, Key: endpointKey}, "gpt-4.1-mini", testInput()); !errors.Is(err, ErrUnusable) || f.count() != 2 {
		t.Fatalf("twice refused: %v, %d requests", err, f.count())
	}
	for _, reply := range []func(http.ResponseWriter, *http.Request){
		chatStatus(400, `{"error":{"param":"messages","code":"invalid_value"}}`),
		chatStatus(422, `{"error":{"param":"reasoning_effort","code":"unsupported_parameter"}}`),
		chatStatus(401, `{"error":{"param":"reasoning_effort"}}`),
	} {
		f, c, _ = newEndpoint(t, reply, chatAnswer(noneAnswer))
		if _, err := c.JudgeMention(context.Background(), Credential{Provider: ProviderOpenAIAPI, Key: endpointKey}, "gpt-5-mini", testInput()); err == nil || f.count() != 1 {
			t.Fatalf("retried: %v, %d requests", err, f.count())
		}
	}

	// An OpenAI-compatible endpoint never gets a reasoning effort.
	f, c, url := newEndpoint(t, chatStatus(400, `{"error":{"param":"reasoning_effort","code":"unsupported_parameter"}}`), chatAnswer(noneAnswer))
	if _, err := c.JudgeMention(context.Background(), Credential{Provider: ProviderOpenAICompatible, BaseURL: url}, "m", testInput()); !errors.Is(err, ErrUnusable) || f.count() != 1 {
		t.Fatalf("compatible: %v, %d requests", err, f.count())
	}
	if body, _ := bodyKeys(t, f.all()[0].body); body["reasoning_effort"] != nil {
		t.Fatalf("compatible body: %v", body)
	}
}

func TestEndpointAnswers(t *testing.T) {
	choice := func(message map[string]any, finish any) func(http.ResponseWriter, *http.Request) {
		return chatBody(map[string]any{"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}}})
	}
	for name, reply := range map[string]func(http.ResponseWriter, *http.Request){
		"refusal":        choice(map[string]any{"role": "assistant", "content": nil, "refusal": "I can't help with that."}, "stop"),
		"refusal text":   choice(map[string]any{"role": "assistant", "content": noneAnswer, "refusal": "no"}, "stop"),
		"length":         choice(map[string]any{"role": "assistant", "content": noneAnswer}, "length"),
		"content filter": choice(map[string]any{"role": "assistant", "content": noneAnswer}, "content_filter"),
		"null content":   choice(map[string]any{"role": "assistant", "content": nil}, "stop"),
		"empty content":  choice(map[string]any{"role": "assistant", "content": ""}, "stop"),
		"no choices":     chatBody(map[string]any{"choices": []any{}}),
		"no choices key": chatBody(map[string]any{"id": "x"}),
		"not json":       chatStatus(200, "<html>ok</html>"),
		"too large": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"`+strings.Repeat("a", endpointBodyLimit)+`"}}]}`)
		},
		"not the schema": chatAnswer(`{"decision":"maybe"}`),
	} {
		_, c, url := newEndpoint(t, reply)
		if _, err := c.JudgeMention(context.Background(), Credential{Provider: ProviderOpenAICompatible, BaseURL: url}, "m", testInput()); !errors.Is(err, ErrUnusable) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	// A null finish reason and a null refusal are fine.
	_, c, url := newEndpoint(t, choice(map[string]any{"role": "assistant", "content": noneAnswer, "refusal": nil}, nil))
	if _, err := c.JudgeMention(context.Background(), Credential{Provider: ProviderOpenAICompatible, BaseURL: url}, "m", testInput()); err != nil {
		t.Fatal(err)
	}
}

func TestEndpointErrorClasses(t *testing.T) {
	const secret = "BODY-SECRET"
	for _, tc := range []struct {
		status int
		body   string
		want   error
	}{
		{401, `{"error":{"message":"` + secret + `","type":"invalid_request_error","code":"invalid_api_key"}}`, ErrKeyInvalid},
		{403, `{"error":{"message":"` + secret + `"}}`, ErrKeyInvalid},
		{402, `{"error":{"message":"` + secret + `"}}`, ErrBilling},
		{429, `{"error":{"message":"` + secret + `","type":"insufficient_quota","code":"insufficient_quota"}}`, ErrBilling},
		{429, `{"error":{"message":"` + secret + `","type":"insufficient_quota"}}`, ErrBilling},
		{429, `{"error":{"message":"` + secret + `","code":"insufficient_quota"}}`, ErrBilling},
		{429, `{"error":{"message":"` + secret + `","type":"requests","code":"rate_limit_exceeded"}}`, ErrTemporary},
		{404, `{"error":{"message":"` + secret + `","code":"model_not_found"}}`, ErrModelUnavailable},
		{404, `404 page not found ` + secret, ErrModelUnavailable},
		{400, `{"error":{"message":"` + secret + `","code":"model_not_found"}}`, ErrModelUnavailable},
		{400, `{"error":{"message":"` + secret + `","type":"invalid_request_error"}}`, ErrUnusable},
		{422, `{"detail":"` + secret + `"}`, ErrUnusable},
		{408, ``, ErrTemporary},
		{500, `{"error":{"message":"` + secret + `"}}`, ErrTemporary},
		{503, `<html>` + secret + `</html>`, ErrTemporary},
		// Bodies of other shapes are classified by the status alone.
		{401, `{"error":"` + secret + `"}`, ErrKeyInvalid},
		{401, `{"error":{"code":401,"message":"` + secret + `"}}`, ErrKeyInvalid},
		{401, `<html>` + secret + `</html>`, ErrKeyInvalid},
		{401, ``, ErrKeyInvalid},
		{429, `{"error":"insufficient_quota ` + secret + `"}`, ErrTemporary},
		{429, `{"error":{"code":429,"message":"` + secret + `"}}`, ErrTemporary},
		{429, `<html>` + secret + `</html>`, ErrTemporary},
		{429, `{"error":{"code":"insufficient quota <b>` + secret + `</b>"}}`, ErrTemporary},
	} {
		f, c, url := newEndpoint(t, chatStatus(tc.status, tc.body))
		_, err := c.JudgeMention(context.Background(), Credential{Provider: ProviderOpenAICompatible, Key: endpointKey, BaseURL: url + "/v1"}, "m", testInput())
		if !errors.Is(err, tc.want) || f.count() != 1 {
			t.Fatalf("%d %s: %v, %d requests", tc.status, tc.body, err, f.count())
		}
		if s := err.Error(); strings.Contains(s, secret) || strings.Contains(s, url) || strings.Contains(s, "127.0.0.1") || strings.Contains(s, endpointKey) {
			t.Fatalf("%d: error carries the body, URL or key: %q", tc.status, s)
		}
	}
}

func TestEndpointRedirectIsNotFollowed(t *testing.T) {
	other := &fakeEndpoint{replies: []func(http.ResponseWriter, *http.Request){chatAnswer(noneAnswer)}}
	otherSrv := httptest.NewServer(other)
	t.Cleanup(otherSrv.Close)
	for _, status := range []int{http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		_, c, url := newEndpoint(t, func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, otherSrv.URL+r.URL.Path, status)
		})
		_, err := c.JudgeMention(context.Background(), Credential{Provider: ProviderOpenAICompatible, Key: endpointKey, BaseURL: url}, "m", testInput())
		if !errors.Is(err, ErrUnusable) || strings.Contains(err.Error(), otherSrv.URL) {
			t.Fatalf("%d: %v", status, err)
		}
	}
	if other.count() != 0 {
		t.Fatalf("redirect followed: %d requests", other.count())
	}
}

func TestEndpointProxyIsIgnored(t *testing.T) {
	proxy := &fakeEndpoint{replies: []func(http.ResponseWriter, *http.Request){chatAnswer(noneAnswer)}}
	proxySrv := httptest.NewServer(proxy)
	t.Cleanup(proxySrv.Close)
	for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
		t.Setenv(k, proxySrv.URL)
	}
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")
	f, c, url := newEndpoint(t, chatAnswer(noneAnswer))
	if _, err := c.JudgeMention(context.Background(), Credential{Provider: ProviderOpenAICompatible, BaseURL: url}, "m", testInput()); err != nil {
		t.Fatal(err)
	}
	if f.count() != 1 || proxy.count() != 0 {
		t.Fatalf("endpoint %d, proxy %d", f.count(), proxy.count())
	}
	// Loopback addresses are never proxied by the environment, so the
	// transport is checked as well.
	for _, allow := range []bool{false, true} {
		if tr := NewEndpointClient(allow).Transport.(*http.Transport); tr.Proxy != nil {
			t.Fatalf("allowPrivate=%v: proxy is set", allow)
		}
	}
}

func TestEndpointAddressBlocked(t *testing.T) {
	// The OpenAI API URL is not checked again, so the dialer refuses the
	// loopback address of the fake.
	f, c, url := newEndpoint(t, chatAnswer(noneAnswer))
	c.Endpoints = NewEndpointClient(false)
	_, err := c.JudgeMention(context.Background(), Credential{Provider: ProviderOpenAIAPI, Key: endpointKey}, "m", testInput())
	if !errors.Is(err, ErrEndpointBlocked) || !errors.Is(err, ErrKeyInvalid) || errors.Is(err, ErrTemporary) || f.count() != 0 {
		t.Fatalf("blocked: %v, %d requests", err, f.count())
	}
	if strings.Contains(err.Error(), url) || strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("error carries the URL: %v", err)
	}
	// An endpoint whose http URL is allowed is still refused by a client
	// limited to public addresses.
	_, err = c.JudgeMention(context.Background(), Credential{Provider: ProviderOpenAICompatible, BaseURL: url}, "m", testInput())
	if !errors.Is(err, ErrEndpointBlocked) || !errors.Is(err, ErrKeyInvalid) || f.count() != 0 {
		t.Fatalf("blocked endpoint: %v, %d requests", err, f.count())
	}
	if err = c.CheckEndpoint(context.Background(), Credential{Provider: ProviderOpenAICompatible, BaseURL: url}, "m"); !errors.Is(err, ErrEndpointBlocked) || f.count() != 0 {
		t.Fatalf("blocked check: %v", err)
	}
	c.Endpoints = NewEndpointClient(true)
	if _, err = c.JudgeMention(context.Background(), Credential{Provider: ProviderOpenAIAPI, Key: endpointKey}, "m", testInput()); err != nil || f.count() != 1 {
		t.Fatalf("allowed: %v, %d requests", err, f.count())
	}
	// Without Endpoints, a client is made from AllowPrivateEndpoints.
	c.Endpoints = nil
	if _, err = c.JudgeMention(context.Background(), Credential{Provider: ProviderOpenAICompatible, BaseURL: url}, "m", testInput()); err != nil || f.count() != 2 {
		t.Fatalf("default client: %v, %d requests", err, f.count())
	}
}

// A URL saved while http was allowed is not used after it no longer is: the
// URL is checked again before every request.
func TestEndpointURLCheckedAgain(t *testing.T) {
	f, c, url := newEndpoint(t, chatAnswer(noneAnswer))
	c.AllowPrivateEndpoints = false
	for name, base := range map[string]string{"http": url, "invalid": "https://user:pass@example.com", "query": "https://example.com/v1?x=1"} {
		_, err := c.JudgeMention(context.Background(), Credential{Provider: ProviderOpenAICompatible, Key: endpointKey, BaseURL: base}, "m", testInput())
		if !errors.Is(err, ErrKeyInvalid) || !errors.Is(err, ErrEndpointBlocked) {
			t.Fatalf("%s: %v", name, err)
		}
		if err = c.CheckEndpoint(context.Background(), Credential{Provider: ProviderOpenAICompatible, Key: endpointKey, BaseURL: base}, "m"); !errors.Is(err, ErrKeyInvalid) || !errors.Is(err, ErrEndpointBlocked) {
			t.Fatalf("%s check: %v", name, err)
		}
	}
	// An endpoint without a URL is never sent to the OpenAI API.
	if _, err := c.JudgeMention(context.Background(), Credential{Provider: ProviderOpenAICompatible, Key: endpointKey}, "m", testInput()); !errors.Is(err, ErrKeyInvalid) {
		t.Fatalf("no URL: %v", err)
	}
	if f.count() != 0 {
		t.Fatalf("%d requests sent", f.count())
	}
}

func TestEndpointAddrAllowed(t *testing.T) {
	for _, tc := range []struct {
		addr                 string
		public, allowPrivate bool
	}{
		{"8.8.8.8", true, true},
		{"2606:4700:4700::1111", true, true},
		{"127.0.0.1", false, true},
		{"::1", false, true},
		{"10.0.0.5", false, true},
		{"192.168.1.10", false, true},
		{"172.16.0.1", false, true},
		{"fd12:3456::1", false, true},
		{"100.64.0.1", false, true},
		// Always refused.
		{"169.254.169.254", false, false},
		{"169.254.0.1", false, false},
		{"::ffff:169.254.169.254", false, false},
		{"fe80::1", false, false},
		{"fd00:ec2::254", false, false},
		{"100.100.100.200", false, false},
		{"0.0.0.0", false, false},
		{"0.1.2.3", false, false},
		{"::", false, false},
		{"224.0.0.1", false, false},
		{"239.255.255.250", false, false},
		{"ff02::1", false, false},
	} {
		a := netip.MustParseAddr(tc.addr)
		if got := endpointAddrAllowed(a, false); got != tc.public {
			t.Fatalf("%s public: %v", tc.addr, got)
		}
		if got := endpointAddrAllowed(a, true); got != tc.allowPrivate {
			t.Fatalf("%s allowPrivate: %v", tc.addr, got)
		}
	}
}

func TestNormalizeEndpointURL(t *testing.T) {
	for _, tc := range []struct {
		raw, want string
		private   bool
	}{
		{"https://openrouter.ai/api/v1", "https://openrouter.ai/api/v1", false},
		{" https://example.com/v1/ ", "https://example.com/v1", false},
		{"https://example.com///", "https://example.com", false},
		{"https://example.com:8443/v1", "https://example.com:8443/v1", false},
		{"http://localhost:11434/v1", "http://localhost:11434/v1", true},
		{"http://192.168.1.10:8000/v1/", "http://192.168.1.10:8000/v1", true},
		{"https://[2001:db8::1]/v1", "https://[2001:db8::1]/v1", false},
	} {
		got, err := NormalizeEndpointURL(tc.raw, tc.private)
		if err != nil || got != tc.want {
			t.Fatalf("%q: %q %v", tc.raw, got, err)
		}
	}
	// http needs allowPrivate.
	if _, err := NormalizeEndpointURL("http://localhost:11434/v1", false); !errors.Is(err, ErrEndpointBlocked) {
		t.Fatalf("http: %v", err)
	}
	for _, raw := range []string{
		"", "   ", "https://user:pass@example.com/v1", "https://user@example.com", "https://example.com/v1?x=1", "https://example.com/v1?",
		"https://example.com/v1#x", "https://example.com/v1#", "/v1", "example.com/v1", "ftp://example.com", "file:///etc/passwd",
		"https://", "https:///v1", "https://:443/v1", "https:example.com", "https://exa mple.com", "https://example.com/v 1",
		"https://example.com/" + strings.Repeat("a", 2049-len("https://example.com/")),
	} {
		for _, private := range []bool{false, true} {
			if _, err := NormalizeEndpointURL(raw, private); err == nil || errors.Is(err, ErrEndpointBlocked) {
				t.Fatalf("%q (%v): %v", raw, private, err)
			}
		}
	}
	long := "https://example.com/" + strings.Repeat("a", 2048-len("https://example.com/"))
	if got, err := NormalizeEndpointURL(long, false); err != nil || got != long {
		t.Fatalf("2048 bytes: %v", err)
	}
}

func TestValidEndpointModel(t *testing.T) {
	for _, m := range []string{"gpt-5-mini", "meta-llama/Llama-3.1-8B-Instruct", "llama3.1:8b", "a", strings.Repeat("m", 256)} {
		if !ValidEndpointModel(m) {
			t.Fatalf("rejected %q", m)
		}
	}
	for _, m := range []string{"", "gpt 5", " gpt", "gpt\t5", "モデル", strings.Repeat("m", 257)} {
		if ValidEndpointModel(m) {
			t.Fatalf("accepted %q", m)
		}
	}
}

func TestCheckEndpoint(t *testing.T) {
	f, c, url := newEndpoint(t, chatAnswer(`{"ok":true}`))
	cred := Credential{Provider: ProviderOpenAICompatible, Key: endpointKey, BaseURL: url}
	if err := c.CheckEndpoint(context.Background(), cred, "m"); err != nil {
		t.Fatal(err)
	}
	body, _ := bodyKeys(t, f.all()[0].body)
	if schema := body["response_format"].(map[string]any)["json_schema"].(map[string]any); schema["name"] != checkSchemaName || schema["strict"] != true {
		t.Fatalf("check body: %v", body)
	}
	for _, answer := range []string{`{"ok":false}`, `{}`, `ok`, `{"ok":true,"x":1}`} {
		_, c, url = newEndpoint(t, chatAnswer(answer))
		if err := c.CheckEndpoint(context.Background(), Credential{Provider: ProviderOpenAICompatible, BaseURL: url}, "m"); !errors.Is(err, ErrUnusable) {
			t.Fatalf("%s: %v", answer, err)
		}
	}
}

func TestEndpointTimeout(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	slow := func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}
	_, c, url := newEndpoint(t, slow)
	c.EndpointTimeout = 50 * time.Millisecond
	cred := Credential{Provider: ProviderOpenAICompatible, BaseURL: url}
	if _, err := c.JudgeMention(context.Background(), cred, "m", testInput()); !errors.Is(err, ErrTemporary) {
		t.Fatalf("timeout: %v", err)
	}
	// The caller's own end is returned as is.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	c.EndpointTimeout = time.Minute
	if _, err := c.JudgeMention(ctx, cred, "m", testInput()); !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrTemporary) {
		t.Fatalf("caller: %v", err)
	}
	// A refused connection is temporary.
	srv := httptest.NewServer(http.NotFoundHandler())
	closed := srv.URL
	srv.Close()
	if _, err := c.JudgeMention(context.Background(), Credential{Provider: ProviderOpenAICompatible, BaseURL: closed}, "m", testInput()); !errors.Is(err, ErrTemporary) || strings.Contains(err.Error(), closed) {
		t.Fatalf("refused: %v", err)
	}
}
