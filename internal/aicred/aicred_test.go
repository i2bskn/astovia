package aicred_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/i2bskn/astovia/internal/ai"
	"github.com/i2bskn/astovia/internal/aicred"
	"github.com/i2bskn/astovia/internal/openai"
	"github.com/i2bskn/astovia/internal/secretbox"
	"github.com/i2bskn/astovia/internal/store"
	"github.com/i2bskn/astovia/internal/testutil"
)

var testNow = time.Unix(1_800_000_000, 0)

// fakeToken is the token endpoint: every refresh answers with a new
// access and refresh token unless status is set.
type fakeToken struct {
	calls   atomic.Int32
	status  atomic.Int32
	body    atomic.Value
	delay   time.Duration
	started chan struct{}
}

func (f *fakeToken) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	n := f.calls.Add(1)
	if f.started != nil {
		f.started <- struct{}{}
	}
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	if s := f.status.Load(); s != 0 {
		w.WriteHeader(int(s))
		if b, ok := f.body.Load().(string); ok {
			_, _ = w.Write([]byte(b))
		}
		return
	}
	_ = r.ParseForm()
	_, _ = fmt.Fprintf(w, `{"access_token":"at-%d","refresh_token":"rt-%d","token_type":"Bearer","expires_in":3600}`, n+1, n+1)
}

type fixture struct {
	store  *store.Store
	tokens *aicred.Tokens
	fake   *fakeToken
	now    atomic.Int64
	a      store.AISettings
}

func newFixture(t *testing.T, expiresIn time.Duration) *fixture {
	t.Helper()
	ctx := context.Background()
	s, _ := testutil.Database(t, "sqlite")
	u := store.User{ID: "alice-id", Email: "alice@example.com", PasswordHash: "x", Role: store.RoleOwner}
	if err := s.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	if err := s.Login(ctx, u, store.Session{ID: "alice-session", UserID: u.ID, ExpiresAt: testNow.Unix() + 3600, CreatedAt: testNow.Unix()}, store.Refresh{Hash: "refresh-alice", SessionID: "alice-session", CreatedAt: testNow.Unix()}); err != nil {
		t.Fatal(err)
	}
	f := &fixture{store: s, fake: &fakeToken{}}
	f.now.Store(testNow.Unix())
	srv := httptest.NewServer(f.fake)
	t.Cleanup(srv.Close)
	oc := openai.New("urn:uuid:test")
	oc.AuthBaseURL = srv.URL
	oc.Now = func() time.Time { return time.Unix(f.now.Load(), 0) }
	f.tokens = &aicred.Tokens{Store: s, Box: secretbox.New([]byte(strings.Repeat("s", 32))), OpenAI: oc, Now: oc.Now}
	sealed, err := f.tokens.Seal(u.ID, aicred.Secret{ClientID: "oaiapp_1", Subject: "sub", Email: "alice@example.com", RefreshToken: "rt-1", AccessToken: "at-1", ExpiresAt: testNow.Add(expiresIn).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	if f.a, err = s.SaveAISettings(ctx, store.AISettings{UserID: u.ID, Provider: ai.ProviderOpenAI, Model: "gpt-x", APIKeySealed: sealed}, 0, true, "alice-session", testNow.Unix()); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) cred(t *testing.T) ai.Credential {
	t.Helper()
	cred, err := aicred.Open(f.tokens.Box, f.tokens, f.a)
	if err != nil || cred.Provider != ai.ProviderOpenAI || cred.Token == nil {
		t.Fatalf("open: %+v %v", cred, err)
	}
	return cred
}

func (f *fixture) saved(t *testing.T) (store.AISettings, aicred.Secret) {
	t.Helper()
	a, err := f.store.AISettings(context.Background(), "alice-id")
	if err != nil {
		t.Fatal(err)
	}
	s, err := f.tokens.OpenSecret(a)
	if err != nil {
		t.Fatal(err)
	}
	return a, s
}

func TestTokenFreshAndRefresh(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, time.Hour)
	cred := f.cred(t)
	// Valid for long enough: no refresh.
	if tok, err := cred.Token(ctx, ""); err != nil || tok != "at-1" || f.fake.calls.Load() != 0 {
		t.Fatalf("fresh: %q %v %d", tok, err, f.fake.calls.Load())
	}
	// Close to the expiry: refreshed and saved, without a new generation.
	f.now.Store(testNow.Add(56 * time.Minute).Unix())
	tok, err := cred.Token(ctx, "")
	if err != nil || tok != "at-2" || f.fake.calls.Load() != 1 {
		t.Fatalf("refresh: %q %v", tok, err)
	}
	a, s := f.saved(t)
	if a.Generation != f.a.Generation || a.UpdatedAt != f.a.UpdatedAt || s.RefreshToken != "rt-2" || s.AccessToken != "at-2" || s.ExpiresAt != testNow.Add(116*time.Minute).Unix() || s.ClientID != "oaiapp_1" || s.Subject != "sub" || s.Email != "alice@example.com" {
		t.Fatalf("saved: %+v %+v", a, s)
	}
	// A refused token is refreshed even when it is valid.
	if tok, err = cred.Token(ctx, "at-2"); err != nil || tok != "at-3" || f.fake.calls.Load() != 2 {
		t.Fatalf("failed: %q %v", tok, err)
	}
	// A refused token that another request already replaced is not.
	if tok, err = cred.Token(ctx, "at-2"); err != nil || tok != "at-3" || f.fake.calls.Load() != 2 {
		t.Fatalf("replaced: %q %v", tok, err)
	}
}

func TestTokenConcurrentRefresh(t *testing.T) {
	f := newFixture(t, time.Minute)
	cred := f.cred(t)
	var wg sync.WaitGroup
	tokens := make([]string, 10)
	errs := make([]error, 10)
	for i := range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tokens[i], errs[i] = cred.Token(context.Background(), "")
		}()
	}
	wg.Wait()
	for i := range 10 {
		if errs[i] != nil || tokens[i] != "at-2" {
			t.Fatalf("%d: %q %v", i, tokens[i], errs[i])
		}
	}
	if n := f.fake.calls.Load(); n != 1 {
		t.Fatalf("refreshes: %d", n)
	}
}

func TestTokenStale(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, time.Minute)
	cred := f.cred(t)
	// Saved again (a model change): the old generation is stale.
	if _, err := f.store.SaveAISettings(ctx, store.AISettings{UserID: "alice-id", Provider: ai.ProviderOpenAI, Model: "gpt-y", APIKeySealed: f.a.APIKeySealed}, f.a.Generation, false, "alice-session", testNow.Unix()); err != nil {
		t.Fatal(err)
	}
	if _, err := cred.Token(ctx, ""); !errors.Is(err, aicred.ErrStale) || !errors.Is(err, ai.ErrTemporary) || f.fake.calls.Load() != 0 {
		t.Fatalf("stale: %v", err)
	}
	// Deleted.
	if err := f.store.DeleteAISettings(ctx, "alice-id", "alice-session", testNow.Unix()); err != nil {
		t.Fatal(err)
	}
	if _, err := cred.Token(ctx, ""); !errors.Is(err, aicred.ErrStale) {
		t.Fatalf("deleted: %v", err)
	}
}

// The settings are removed while the refresh runs: the new tokens are not
// saved anywhere.
func TestTokenRemovedDuringRefresh(t *testing.T) {
	f := newFixture(t, time.Minute)
	f.fake.started = make(chan struct{}, 1)
	f.fake.delay = 100 * time.Millisecond
	cred := f.cred(t)
	done := make(chan error, 1)
	go func() {
		_, err := cred.Token(context.Background(), "")
		done <- err
	}()
	<-f.fake.started
	if err := f.store.ChangeUser(context.Background(), "alice@example.com", "", true, testNow.Unix()); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, aicred.ErrStale) {
		t.Fatalf("removed: %v", err)
	}
}

func TestTokenRefreshErrors(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		want   error
	}{
		{400, `{"error":"refresh_token_reused"}`, ai.ErrKeyInvalid},
		{400, `{"error":"invalid_request"}`, ai.ErrKeyInvalid},
		{503, ``, ai.ErrTemporary},
	} {
		f := newFixture(t, time.Minute)
		f.fake.status.Store(int32(tc.status))
		f.fake.body.Store(tc.body)
		_, err := f.cred(t).Token(context.Background(), "")
		if !errors.Is(err, tc.want) || errors.Is(err, aicred.ErrStale) {
			t.Fatalf("%d %s: %v", tc.status, tc.body, err)
		}
		if strings.Contains(err.Error(), "rt-1") || strings.Contains(err.Error(), "oaiapp_1") {
			t.Fatalf("error includes a secret: %v", err)
		}
		if _, s := f.saved(t); s.RefreshToken != "rt-1" {
			t.Fatalf("saved after a failure: %+v", s)
		}
	}
}

// A caller that gives up during the refresh gets its context error, and
// the new tokens are saved anyway.
func TestTokenCallerCanceledDuringRefresh(t *testing.T) {
	f := newFixture(t, time.Minute)
	f.fake.started = make(chan struct{}, 1)
	f.fake.delay = 100 * time.Millisecond
	cred := f.cred(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := cred.Token(ctx, "")
		done <- err
	}()
	<-f.fake.started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled: %v", err)
	}
	if _, s := f.saved(t); s.RefreshToken != "rt-2" || s.AccessToken != "at-2" {
		t.Fatalf("not saved: %+v", s)
	}
}

func TestWithLock(t *testing.T) {
	tokens := &aicred.Tokens{}
	held := make(chan struct{})
	release := make(chan struct{})
	go func() {
		_ = tokens.WithLock(context.Background(), "u", func() error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	ran := false
	if err := tokens.WithLock(ctx, "u", func() error { ran = true; return nil }); !errors.Is(err, context.DeadlineExceeded) || ran {
		t.Fatalf("wait: %v %v", err, ran)
	}
	// Another user is not blocked.
	if err := tokens.WithLock(context.Background(), "v", func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	close(release)
	want := errors.New("fn")
	if err := tokens.WithLock(context.Background(), "u", func() error { return want }); !errors.Is(err, want) {
		t.Fatalf("after release: %v", err)
	}
}

// A store that fails to read the settings, or to save the refreshed
// tokens, makes the token source fail temporarily.
func TestTokenStoreFails(t *testing.T) {
	f := newFixture(t, time.Minute)
	f.fake.started = make(chan struct{}, 1)
	f.fake.delay = 100 * time.Millisecond
	cred := f.cred(t)
	done := make(chan error, 1)
	go func() {
		_, err := cred.Token(context.Background(), "")
		done <- err
	}()
	<-f.fake.started
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, ai.ErrTemporary) || errors.Is(err, aicred.ErrStale) || f.fake.calls.Load() != 1 {
		t.Fatalf("save fails: %v", err)
	}
	if _, err := cred.Token(context.Background(), ""); !errors.Is(err, ai.ErrTemporary) || errors.Is(err, aicred.ErrStale) || f.fake.calls.Load() != 1 {
		t.Fatalf("read fails: %v", err)
	}
}

func TestOpen(t *testing.T) {
	box := secretbox.New([]byte(strings.Repeat("s", 32)))
	key, err := box.Seal(ai.KeyPurpose("u"), "sk-key")
	if err != nil {
		t.Fatal(err)
	}
	cred, err := aicred.Open(box, nil, store.AISettings{UserID: "u", Provider: ai.ProviderAnthropic, APIKeySealed: key})
	if err != nil || cred.Provider != ai.ProviderAnthropic || cred.Key != "sk-key" {
		t.Fatalf("anthropic: %+v %v", cred, err)
	}
	tokens := &aicred.Tokens{Box: box}
	good, err := tokens.Seal("u", aicred.Secret{ClientID: "c", Subject: "s", RefreshToken: "r"})
	if err != nil {
		t.Fatal(err)
	}
	missing, err := tokens.Seal("u", aicred.Secret{ClientID: "c", Subject: "s"})
	if err != nil {
		t.Fatal(err)
	}
	badVersion, err := box.Seal(aicred.SecretPurpose("u"), `{"v":2,"client_id":"c","subject":"s","refresh_token":"r"}`)
	if err != nil {
		t.Fatal(err)
	}
	for name, a := range map[string]store.AISettings{
		"anthropic key as openai": {UserID: "u", Provider: ai.ProviderOpenAI, APIKeySealed: key},
		"openai as anthropic":     {UserID: "u", Provider: ai.ProviderAnthropic, APIKeySealed: good},
		"other user":              {UserID: "v", Provider: ai.ProviderOpenAI, APIKeySealed: good},
		"no refresh token":        {UserID: "u", Provider: ai.ProviderOpenAI, APIKeySealed: missing},
		"version":                 {UserID: "u", Provider: ai.ProviderOpenAI, APIKeySealed: badVersion},
		"provider":                {UserID: "u", Provider: "x", APIKeySealed: good},
	} {
		if _, err := aicred.Open(box, tokens, a); !errors.Is(err, aicred.ErrUndecryptable) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	// Without a token manager the source fails temporarily.
	cred, err = aicred.Open(box, nil, store.AISettings{UserID: "u", Provider: ai.ProviderOpenAI, APIKeySealed: good})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = cred.Token(context.Background(), ""); !errors.Is(err, ai.ErrTemporary) {
		t.Fatalf("no tokens: %v", err)
	}
}

func TestEndpoint(t *testing.T) {
	box := secretbox.New([]byte(strings.Repeat("s", 32)))
	compatible, err := aicred.SealEndpoint(box, "u", aicred.Endpoint{BaseURL: "https://example.com/v1", APIKey: "ep-key"})
	if err != nil {
		t.Fatal(err)
	}
	e, err := aicred.OpenEndpoint(box, store.AISettings{UserID: "u", Provider: ai.ProviderOpenAICompatible, APIKeySealed: compatible})
	if err != nil || e != (aicred.Endpoint{V: 1, BaseURL: "https://example.com/v1", APIKey: "ep-key"}) {
		t.Fatalf("round trip: %+v %v", e, err)
	}
	cred, err := aicred.Open(box, nil, store.AISettings{UserID: "u", Provider: ai.ProviderOpenAICompatible, APIKeySealed: compatible})
	if err != nil || cred.Provider != ai.ProviderOpenAICompatible || cred.Key != "ep-key" || cred.BaseURL != "https://example.com/v1" || cred.Token != nil {
		t.Fatalf("compatible: %+v %v", cred, err)
	}
	noKey, err := aicred.SealEndpoint(box, "u", aicred.Endpoint{BaseURL: "http://ollama:11434/v1"})
	if err != nil {
		t.Fatal(err)
	}
	cred, err = aicred.Open(box, nil, store.AISettings{UserID: "u", Provider: ai.ProviderOpenAICompatible, APIKeySealed: noKey})
	if err != nil || cred.Provider != ai.ProviderOpenAICompatible || cred.Key != "" || cred.BaseURL != "http://ollama:11434/v1" || cred.Token != nil {
		t.Fatalf("compatible without key: %+v %v", cred, err)
	}
	apiKey, err := aicred.SealEndpoint(box, "u", aicred.Endpoint{APIKey: "sk-openai-key"})
	if err != nil {
		t.Fatal(err)
	}
	cred, err = aicred.Open(box, nil, store.AISettings{UserID: "u", Provider: ai.ProviderOpenAIAPI, APIKeySealed: apiKey})
	if err != nil || cred.Provider != ai.ProviderOpenAIAPI || cred.Key != "sk-openai-key" || cred.BaseURL != "" || cred.Token != nil {
		t.Fatalf("openai api: %+v %v", cred, err)
	}
	anthropic, err := box.Seal(ai.KeyPurpose("u"), "sk-key")
	if err != nil {
		t.Fatal(err)
	}
	badVersion, err := box.Seal(aicred.EndpointPurpose("u"), `{"v":2,"base_url":"https://example.com","api_key":"k"}`)
	if err != nil {
		t.Fatal(err)
	}
	for name, a := range map[string]store.AISettings{
		"anthropic key as endpoint":   {UserID: "u", Provider: ai.ProviderOpenAICompatible, APIKeySealed: anthropic},
		"anthropic key as openai api": {UserID: "u", Provider: ai.ProviderOpenAIAPI, APIKeySealed: anthropic},
		"endpoint as anthropic":       {UserID: "u", Provider: ai.ProviderAnthropic, APIKeySealed: compatible},
		"endpoint as chatgpt":         {UserID: "u", Provider: ai.ProviderOpenAI, APIKeySealed: compatible},
		"other user":                  {UserID: "v", Provider: ai.ProviderOpenAICompatible, APIKeySealed: compatible},
		"version":                     {UserID: "u", Provider: ai.ProviderOpenAICompatible, APIKeySealed: badVersion},
		"compatible without url":      {UserID: "u", Provider: ai.ProviderOpenAICompatible, APIKeySealed: apiKey},
		"openai api without key":      {UserID: "u", Provider: ai.ProviderOpenAIAPI, APIKeySealed: noKey},
	} {
		if _, err := aicred.Open(box, nil, a); !errors.Is(err, aicred.ErrUndecryptable) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if _, err := aicred.OpenEndpoint(box, store.AISettings{UserID: "u", Provider: ai.ProviderAnthropic, APIKeySealed: compatible}); !errors.Is(err, aicred.ErrUndecryptable) {
		t.Fatalf("anthropic provider: %v", err)
	}
}

func TestCoolingDown(t *testing.T) {
	limit := sql.NullString{String: aicred.UsageLimit, Valid: true}
	at := testNow.Unix()
	for _, tc := range []struct {
		a    store.AISettings
		now  time.Time
		want bool
	}{
		{store.AISettings{Provider: ai.ProviderOpenAI, LastError: limit, UpdatedAt: at}, testNow, true},
		{store.AISettings{Provider: ai.ProviderOpenAI, LastError: limit, UpdatedAt: at}, testNow.Add(15*time.Minute - time.Second), true},
		{store.AISettings{Provider: ai.ProviderOpenAI, LastError: limit, UpdatedAt: at}, testNow.Add(15 * time.Minute), false},
		{store.AISettings{Provider: ai.ProviderAnthropic, LastError: limit, UpdatedAt: at}, testNow, false},
		{store.AISettings{Provider: ai.ProviderOpenAI, LastError: sql.NullString{String: "billing", Valid: true}, UpdatedAt: at}, testNow, false},
		{store.AISettings{Provider: ai.ProviderOpenAI, UpdatedAt: at}, testNow, false},
	} {
		if got := aicred.CoolingDown(tc.a, tc.now); got != tc.want {
			t.Fatalf("%+v at %v: %v", tc.a, tc.now, got)
		}
	}
}
