package mail

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/i2bskn/astovia/internal/config"
)

// apiServer records requests and answers with status and body.
type apiServer struct {
	mu       sync.Mutex
	requests []*http.Request
	bodies   []map[string]any
	status   int
	body     string
}

func (a *apiServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	data, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(data, &body)
	a.mu.Lock()
	a.requests = append(a.requests, r)
	a.bodies = append(a.bodies, body)
	status, resp := a.status, a.body
	a.mu.Unlock()
	if r.URL.Path == "/elsewhere" {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"success":true,"id":"x"}`)
		return
	}
	if status == http.StatusFound {
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
		return
	}
	w.WriteHeader(status)
	_, _ = io.WriteString(w, resp)
}

func (a *apiServer) answer(status int, body string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.status, a.body = status, body
}

func (a *apiServer) last() (*http.Request, map[string]any) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.requests[len(a.requests)-1], a.bodies[len(a.bodies)-1]
}

func (a *apiServer) requested(path string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for _, r := range a.requests {
		if r.URL.Path == path {
			n++
		}
	}
	return n
}

const (
	testKey = "re_secret_key_value"
	testTo  = "someone@example.org"
)

var testMessage = Message{To: testTo, Subject: "件名", Text: "本文", IdempotencyKey: "notification-1"}

// checkSafe fails when the error contains the key, the recipient or the
// response body marker.
func checkSafe(t *testing.T, err error) {
	t.Helper()
	for _, v := range []string{testKey, testTo, "LEAK", "本文"} {
		if strings.Contains(err.Error(), v) {
			t.Fatalf("error contains %q: %v", v, err)
		}
	}
}

func TestNew(t *testing.T) {
	if New(nil) != nil {
		t.Fatal("nil config made a sender")
	}
	if s := New(&config.MailConfig{Provider: config.MailResend, APIKey: "k", From: "a@example.com"}); s.Provider() != "resend" {
		t.Fatalf("resend: %v", s.Provider())
	}
	if s := New(&config.MailConfig{Provider: config.MailCloudflare, APIKey: "k", AccountID: strings.Repeat("a", 32), From: "a@example.com"}); s.Provider() != "cloudflare" {
		t.Fatalf("cloudflare: %v", s.Provider())
	}
}

func TestCloudflareSend(t *testing.T) {
	api := &apiServer{status: 200, body: `{"success":true,"errors":[],"result":{}}`}
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	s := New(&config.MailConfig{Provider: config.MailCloudflare, APIKey: testKey, AccountID: "0123456789abcdef0123456789abcdef", From: "notify@example.com", FromName: "Astovia"}).(*cloudflareSender)
	s.BaseURL = srv.URL
	ctx := context.Background()
	if err := s.Send(ctx, testMessage); err != nil {
		t.Fatal(err)
	}
	r, body := api.last()
	if r.Method != http.MethodPost || r.URL.Path != "/client/v4/accounts/0123456789abcdef0123456789abcdef/email/sending/send" || r.Header.Get("Authorization") != "Bearer "+testKey || r.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("request: %s %s %v", r.Method, r.URL.Path, r.Header)
	}
	to, _ := body["to"].([]any)
	if body["from"] != "notify@example.com" || len(to) != 1 || to[0] != testTo || body["subject"] != "件名" || body["text"] != "本文" || len(body) != 4 {
		t.Fatalf("body: %v", body)
	}
	for _, tc := range []struct {
		status int
		body   string
		kind   error
		code   string
	}{
		{200, `{"success":false,"errors":[{"code":10203,"message":"LEAK"}]}`, ErrRejected, "code 10203"},
		{200, `LEAK`, ErrRejected, "status 200"},
		{401, `{"success":false,"errors":[{"code":10101,"message":"LEAK"}]}`, ErrRejected, "code 10101"},
		{403, `{"success":false,"errors":[{"code":10102,"message":"LEAK"}]}`, ErrRejected, "status 403"},
		{429, `{"success":false,"errors":[{"code":10004,"message":"LEAK"}]}`, ErrRateLimited, "code 10004"},
		{500, `{"success":false,"errors":[{"code":10002,"message":"LEAK"}]}`, ErrTemporary, "status 500"},
		{503, `LEAK`, ErrTemporary, "status 503"},
	} {
		api.answer(tc.status, tc.body)
		err := s.Send(ctx, testMessage)
		if !errors.Is(err, tc.kind) || !strings.Contains(err.Error(), tc.code) {
			t.Fatalf("%d %s: %v", tc.status, tc.body, err)
		}
		checkSafe(t, err)
	}
}

func TestResendSend(t *testing.T) {
	api := &apiServer{status: 200, body: `{"id":"x"}`}
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	s := New(&config.MailConfig{Provider: config.MailResend, APIKey: testKey, From: "notify@example.com", FromName: "Astovia 通知"}).(*resendSender)
	s.BaseURL = srv.URL
	ctx := context.Background()
	if err := s.Send(ctx, testMessage); err != nil {
		t.Fatal(err)
	}
	r, body := api.last()
	if r.Method != http.MethodPost || r.URL.Path != "/emails" || r.Header.Get("Authorization") != "Bearer "+testKey || r.Header.Get("User-Agent") != "astovia" || r.Header.Get("Idempotency-Key") != "notification-1" || r.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("request: %s %s %v", r.Method, r.URL.Path, r.Header)
	}
	to, _ := body["to"].([]any)
	if body["from"] != "Astovia 通知 <notify@example.com>" || len(to) != 1 || to[0] != testTo || body["subject"] != "件名" || body["text"] != "本文" {
		t.Fatalf("body: %v", body)
	}
	s.FromName = ""
	if err := s.Send(ctx, testMessage); err != nil {
		t.Fatal(err)
	}
	if _, body = api.last(); body["from"] != "notify@example.com" {
		t.Fatalf("bare from: %v", body)
	}
	for _, tc := range []struct {
		status int
		body   string
		kind   error
		code   string
	}{
		{401, `{"statusCode":401,"name":"missing_api_key","message":"LEAK"}`, ErrRejected, "missing_api_key"},
		{403, `{"statusCode":403,"name":"invalid_api_key","message":"LEAK"}`, ErrRejected, "invalid_api_key"},
		{422, `{"statusCode":422,"name":"missing_required_field","message":"LEAK"}`, ErrRejected, "missing_required_field"},
		{429, `{"statusCode":429,"name":"rate_limit_exceeded","message":"LEAK"}`, ErrRateLimited, "rate_limit_exceeded"},
		{429, `{"statusCode":429,"name":"daily_quota_exceeded","message":"LEAK"}`, ErrRateLimited, "daily_quota_exceeded"},
		{409, `{"statusCode":409,"name":"concurrent_idempotent_requests","message":"LEAK"}`, ErrTemporary, "concurrent_idempotent_requests"},
		{409, `{"statusCode":409,"name":"invalid_idempotent_request","message":"LEAK"}`, ErrRejected, "invalid_idempotent_request"},
		{500, `{"statusCode":500,"name":"LEAK LEAK","message":"LEAK"}`, ErrTemporary, "status 500"},
		{503, `LEAK`, ErrTemporary, "status 503"},
	} {
		api.answer(tc.status, tc.body)
		err := s.Send(ctx, testMessage)
		if !errors.Is(err, tc.kind) || !strings.Contains(err.Error(), tc.code) {
			t.Fatalf("%d %s: %v", tc.status, tc.body, err)
		}
		checkSafe(t, err)
	}
}

func TestNoRedirect(t *testing.T) {
	api := &apiServer{status: http.StatusFound}
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	r := New(&config.MailConfig{Provider: config.MailResend, APIKey: testKey, From: "notify@example.com"}).(*resendSender)
	r.BaseURL = srv.URL
	c := New(&config.MailConfig{Provider: config.MailCloudflare, APIKey: testKey, AccountID: strings.Repeat("a", 32), From: "notify@example.com"}).(*cloudflareSender)
	c.BaseURL = srv.URL
	for _, s := range []Sender{r, c} {
		err := s.Send(context.Background(), testMessage)
		if !errors.Is(err, ErrRejected) || api.requested("/elsewhere") != 0 {
			t.Fatalf("%s redirect: %v %d", s.Provider(), err, api.requested("/elsewhere"))
		}
	}
}

func TestConnectionError(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	base := srv.URL
	srv.Close()
	r := New(&config.MailConfig{Provider: config.MailResend, APIKey: testKey, From: "notify@example.com"}).(*resendSender)
	r.BaseURL = base
	err := r.Send(context.Background(), testMessage)
	if !errors.Is(err, ErrTemporary) || strings.Contains(err.Error(), base) {
		t.Fatalf("connection error: %v", err)
	}
	checkSafe(t, err)
}
