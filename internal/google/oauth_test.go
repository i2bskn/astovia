package google

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testCode     = "4/0AXcode-secret-value"
	testVerifier = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	testRefresh  = "1//refresh-token-secret"
	testAccess   = "ya29.access-token-secret"
	testSecret   = "GOCSPX-client-secret"
)

var testNow = time.Date(2026, 10, 24, 1, 0, 0, 0, time.UTC)

// recorded is a request seen by a fake endpoint.
type recorded struct {
	method, path, rawQuery, contentType, auth string
	form                                      url.Values
}

// fakeEndpoint answers every request with status and body, and records the
// requests.
type fakeEndpoint struct {
	mu       sync.Mutex
	status   int
	body     string
	header   map[string]string
	requests []recorded
}

func (f *fakeEndpoint) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	form, _ := url.ParseQuery(string(raw))
	f.mu.Lock()
	f.requests = append(f.requests, recorded{method: r.Method, path: r.URL.Path, rawQuery: r.URL.RawQuery, contentType: r.Header.Get("Content-Type"), auth: r.Header.Get("Authorization"), form: form})
	status, body, header := f.status, f.body, f.header
	f.mu.Unlock()
	for k, v := range header {
		w.Header().Set(k, v)
	}
	if status == 0 {
		status = 200
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

func (f *fakeEndpoint) set(status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status, f.body, f.header = status, body, nil
}

func (f *fakeEndpoint) last(t *testing.T) recorded {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		t.Fatal("no request")
	}
	return f.requests[len(f.requests)-1]
}

func (f *fakeEndpoint) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func newTestClient(t *testing.T, f http.Handler) *Client {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c := New(func() time.Time { return testNow })
	c.AuthEndpoint, c.TokenEndpoint, c.RevokeEndpoint, c.GmailBaseURL = srv.URL+"/auth", srv.URL+"/token", srv.URL+"/revoke", srv.URL+"/gmail/v1/users/me"
	return c
}

// noSecrets fails when an error message carries a secret or an address.
func noSecrets(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		return
	}
	for _, s := range []string{testCode, testVerifier, testRefresh, testAccess, testSecret, "alice@example.com", "http://", "/token", "/gmail"} {
		if strings.Contains(err.Error(), s) {
			t.Fatalf("error %q contains %q", err.Error(), s)
		}
	}
}

func TestAuthURL(t *testing.T) {
	c := New(time.Now)
	for _, hint := range []string{"", "alice@example.com"} {
		u, err := url.Parse(c.AuthURL("client-id", "https://astovia.test/integrations/google/callback", "gst_state", Challenge(testVerifier), hint))
		if err != nil || u.Scheme != "https" || u.Host != "accounts.google.com" || u.Path != "/o/oauth2/v2/auth" {
			t.Fatalf("url: %v %v", u, err)
		}
		q := u.Query()
		want := url.Values{"response_type": {"code"}, "client_id": {"client-id"}, "redirect_uri": {"https://astovia.test/integrations/google/callback"}, "scope": {ScopeGmailReadonly},
			"state": {"gst_state"}, "code_challenge": {"E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"}, "code_challenge_method": {"S256"}, "access_type": {"offline"},
			"prompt": {"consent select_account"}, "include_granted_scopes": {"false"}}
		if hint != "" {
			want.Set("login_hint", hint)
		}
		if !reflect.DeepEqual(q, want) {
			t.Fatalf("query:\n%v\nwant\n%v", q, want)
		}
	}
}

func TestExchangeAndRefresh(t *testing.T) {
	f := &fakeEndpoint{}
	c := newTestClient(t, f)
	ctx := context.Background()
	f.set(200, `{"access_token":"`+testAccess+`","expires_in":3599,"refresh_token":"`+testRefresh+`","scope":"https://www.googleapis.com/auth/gmail.readonly openid https://www.googleapis.com/auth/gmail.readonly","token_type":"Bearer"}`)
	tok, err := c.Exchange(ctx, "client-id", testSecret, "https://astovia.test/cb", testCode, testVerifier)
	if err != nil || tok.AccessToken != testAccess || tok.RefreshToken != testRefresh || !tok.Expiry.Equal(testNow.Add(3599*time.Second)) ||
		!reflect.DeepEqual(tok.Scopes, []string{ScopeGmailReadonly, "openid"}) || !tok.HasScope(ScopeGmailReadonly) || tok.HasScope("openid ") {
		t.Fatalf("exchange: %+v %v", tok, err)
	}
	r := f.last(t)
	if r.method != "POST" || r.path != "/token" || r.rawQuery != "" || r.contentType != "application/x-www-form-urlencoded" || r.auth != "" ||
		!reflect.DeepEqual(r.form, url.Values{"grant_type": {"authorization_code"}, "code": {testCode}, "code_verifier": {testVerifier}, "redirect_uri": {"https://astovia.test/cb"}, "client_id": {"client-id"}, "client_secret": {testSecret}}) {
		t.Fatalf("exchange request: %+v", r)
	}

	// No refresh token.
	f.set(200, `{"access_token":"`+testAccess+`","expires_in":3599,"scope":"`+ScopeGmailReadonly+`","token_type":"Bearer"}`)
	if _, err = c.Exchange(ctx, "client-id", testSecret, "https://astovia.test/cb", testCode, testVerifier); !errors.Is(err, ErrNoRefreshToken) {
		t.Fatalf("no refresh token: %v", err)
	}

	// A refresh keeps the refresh token unless Google replaces it.
	tok, err = c.Refresh(ctx, "client-id", testSecret, testRefresh)
	if err != nil || tok.RefreshToken != "" || tok.AccessToken != testAccess {
		t.Fatalf("refresh: %+v %v", tok, err)
	}
	r = f.last(t)
	if r.rawQuery != "" || !reflect.DeepEqual(r.form, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {testRefresh}, "client_id": {"client-id"}, "client_secret": {testSecret}}) {
		t.Fatalf("refresh request: %+v", r)
	}
	f.set(200, `{"access_token":"`+testAccess+`","expires_in":3599,"refresh_token":"1//rotated","token_type":"bearer"}`)
	if tok, err = c.Refresh(ctx, "client-id", testSecret, testRefresh); err != nil || tok.RefreshToken != "1//rotated" || len(tok.Scopes) != 0 {
		t.Fatalf("rotated: %+v %v", tok, err)
	}

	// expires_in missing or not positive is 3000 seconds.
	for _, v := range []string{``, `"expires_in":0,`, `"expires_in":-5,`, `"expires_in":"x",`} {
		f.set(200, `{"access_token":"a",`+v+`"token_type":"Bearer"}`)
		if tok, err = c.Refresh(ctx, "client-id", testSecret, testRefresh); err != nil || !tok.Expiry.Equal(testNow.Add(3000*time.Second)) {
			t.Fatalf("expires_in %q: %+v %v", v, tok, err)
		}
	}
	// The answer is validated.
	for _, body := range []string{`{"access_token":"","token_type":"Bearer","expires_in":1}`, `{"access_token":"a","token_type":"mac","expires_in":1}`, `{"access_token":"a","expires_in":1}`, `not json`, `[]`} {
		f.set(200, body)
		_, err = c.Refresh(ctx, "client-id", testSecret, testRefresh)
		if !errors.Is(err, ErrUnexpected) {
			t.Fatalf("%s: %v", body, err)
		}
		noSecrets(t, err)
	}

	// Token endpoint errors.
	for _, tc := range []struct {
		status int
		body   string
		want   error
	}{
		{400, `{"error":"invalid_grant","error_description":"Token has been expired or revoked."}`, ErrInvalidGrant},
		{401, `{"error":"invalid_client"}`, ErrInvalidClient},
		{400, `{"error":"unauthorized_client"}`, ErrInvalidClient},
		{400, `{"error":"admin_policy_enforced"}`, ErrPolicyBlocked},
	} {
		f.set(tc.status, tc.body)
		_, err = c.Refresh(ctx, "client-id", testSecret, testRefresh)
		if !errors.Is(err, tc.want) {
			t.Fatalf("%s: %v", tc.body, err)
		}
		noSecrets(t, err)
		_, err = c.Exchange(ctx, "client-id", testSecret, "https://astovia.test/cb", testCode, testVerifier)
		if !errors.Is(err, tc.want) {
			t.Fatalf("exchange %s: %v", tc.body, err)
		}
		noSecrets(t, err)
	}
	for _, tc := range []struct {
		status int
		body   string
	}{{400, `{"error":"invalid_request"}`}, {400, `x`}, {500, `{"error":"invalid_grant"}`}, {503, ``}, {429, ``}} {
		f.set(tc.status, tc.body)
		_, err = c.Exchange(ctx, "client-id", testSecret, "https://astovia.test/cb", testCode, testVerifier)
		var se *StatusError
		if !errors.As(err, &se) || se.Code != tc.status {
			t.Fatalf("%d %s: %v", tc.status, tc.body, err)
		}
		noSecrets(t, err)
	}
}

func TestParseScopes(t *testing.T) {
	if got := ParseScopes(" b  a\tb\n"); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("scopes: %v", got)
	}
	if got := ParseScopes(""); got == nil || len(got) != 0 {
		t.Fatalf("empty: %v", got)
	}
}

func TestRevoke(t *testing.T) {
	f := &fakeEndpoint{}
	c := newTestClient(t, f)
	if err := c.Revoke(context.Background(), testRefresh); err != nil {
		t.Fatal(err)
	}
	r := f.last(t)
	if r.method != "POST" || r.path != "/revoke" || r.rawQuery != "" || strings.Contains(r.rawQuery, "token") || r.contentType != "application/x-www-form-urlencoded" ||
		!reflect.DeepEqual(r.form, url.Values{"token": {testRefresh}}) {
		t.Fatalf("revoke request: %+v", r)
	}
	f.set(400, `{"error":"invalid_token"}`)
	err := c.Revoke(context.Background(), testRefresh)
	var se *StatusError
	if !errors.As(err, &se) || se.Code != 400 {
		t.Fatalf("revoke failure: %v", err)
	}
	noSecrets(t, err)
}

// Redirects are not followed, so that a token never reaches another host.
func TestNoRedirects(t *testing.T) {
	other := &fakeEndpoint{}
	otherSrv := httptest.NewServer(other)
	t.Cleanup(otherSrv.Close)
	redirect := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, otherSrv.URL+r.URL.Path, http.StatusTemporaryRedirect)
	})
	c := newTestClient(t, redirect)
	ctx := context.Background()
	var se *StatusError
	if _, err := c.Refresh(ctx, "client-id", testSecret, testRefresh); !errors.As(err, &se) || se.Code != 307 {
		t.Fatalf("token: %v", err)
	}
	if err := c.Revoke(ctx, testRefresh); !errors.As(err, &se) || se.Code != 307 {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := c.Profile(ctx, testAccess); !errors.As(err, &se) || se.Code != 307 {
		t.Fatalf("profile: %v", err)
	}
	if other.count() != 0 {
		t.Fatalf("redirect followed %d times", other.count())
	}
}

// A connection failure does not carry the URL.
func TestTransportErrorHidesURL(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	base := srv.URL
	srv.Close()
	c := New(func() time.Time { return testNow })
	c.TokenEndpoint, c.GmailBaseURL = base+"/token?x="+testCode, base+"/gmail"
	_, err := c.Refresh(context.Background(), "client-id", testSecret, testRefresh)
	if err == nil {
		t.Fatal("no error")
	}
	noSecrets(t, err)
	_, err = c.ListMessages(context.Background(), testAccess, "from:alice@example.com", "")
	if err == nil {
		t.Fatal("no error")
	}
	noSecrets(t, err)
}
