package httpserver

import (
	"context"
	"database/sql"
	"net/http/httptest"
	"net/url"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/i2bskn/astovia/internal/auth"
	"github.com/i2bskn/astovia/internal/config"
	"github.com/i2bskn/astovia/internal/mfa"
	"github.com/i2bskn/astovia/internal/store"
)

const mfaPassword = "correct-password-123"

// mfaAPI is usersAPI (alice the owner, carol the member) served with a
// policy of ASTOVIA_MFA_REQUIRED.
type mfaAPI struct {
	*usersAPI
}

func newMFAAPI(t *testing.T, driver, policy string) *mfaAPI {
	t.Helper()
	api := &mfaAPI{newUsersAPI(t, driver)}
	api.use(policy, false)
	return api
}

// use serves the same database and sessions with policy. The rate limits
// are lifted unless limits is set; they have their own test.
func (a *mfaAPI) use(policy string, limits bool) {
	srv := newServer(config.Config{PublicURL: usersOrigin, Secure: true, MFARequired: policy}, a.srv.auth, fstest.MapFS{}, nil)
	if !limits {
		for _, l := range []**loginLimiter{&srv.limiter, &srv.accountLimiter, &srv.mfaPeerLimiter, &srv.mfaLimiter, &srv.mfaPasswordLimiter} {
			*l = newKeyLimiter(1000000, time.Minute)
		}
	}
	a.srv, a.handler = srv, srv.handler()
}

func (a *mfaAPI) advance(d time.Duration) { *a.now = a.now.Add(d) }

// post posts the JSON object body to a public /auth endpoint.
func (a *mfaAPI) post(path string, body map[string]string) *httptest.ResponseRecorder {
	a.t.Helper()
	return a.public(path, string(mustJSON(a.t, body)), "")
}

// call sends the JSON object body to an /api/v1 endpoint with bearer.
func (a *mfaAPI) call(path, bearer string, body map[string]string) *httptest.ResponseRecorder {
	a.t.Helper()
	return a.do("POST", path, bearer, string(mustJSON(a.t, body)))
}

// challenge checks the response that asks for a second factor instead of
// starting a session, and returns it.
func (a *mfaAPI) challenge(w *httptest.ResponseRecorder) mfaChallengeResponse {
	a.t.Helper()
	expect(a.t, w, 200, "")
	body, keys := jsonKeys(a.t, w.Body.Bytes())
	if !reflect.DeepEqual(keys, []string{"mfa"}) {
		a.t.Fatalf("keys: %v", keys)
	}
	inner, keys := jsonKeys(a.t, body["mfa"])
	if !reflect.DeepEqual(keys, []string{"enrolled", "methods", "token", "webauthn_available"}) {
		a.t.Fatalf("mfa keys: %v", keys)
	}
	if _, keys = jsonKeys(a.t, inner["methods"]); !reflect.DeepEqual(keys, []string{"recovery", "totp", "webauthn"}) {
		a.t.Fatalf("methods keys: %v", keys)
	}
	if issuedCookie(w) {
		a.t.Fatalf("cookie set without a session: %v", w.Header().Values("Set-Cookie"))
	}
	out := decodeJSON[mfaPendingResponse](a.t, w).MFA
	if out.Token == "" || out.WebAuthnAvailable != a.srv.webauthnAvailable() || (out.Methods.WebAuthn && !out.WebAuthnAvailable) {
		a.t.Fatalf("challenge: enrolled=%v methods=%+v webauthn_available=%v", out.Enrolled, out.Methods, out.WebAuthnAvailable)
	}
	if !out.Enrolled && out.Methods != (mfaMethodsResponse{}) {
		a.t.Fatalf("methods without a factor: %+v", out.Methods)
	}
	// The token is not a credential of the API.
	expect(a.t, a.do("GET", "/api/v1/me", out.Token, ""), 401, "unauthorized")
	return out
}

// pending signs in with the password and returns the MFA challenge.
func (a *mfaAPI) pending(email string) mfaChallengeResponse {
	a.t.Helper()
	return a.challenge(a.public("/auth/login", string(mustJSON(a.t, map[string]string{"email": email, "password": mfaPassword})), ""))
}

// code returns the code of secret at the current time step plus offset.
func (a *mfaAPI) code(secret string, offset int64) string {
	a.t.Helper()
	code, ok := mfa.TOTPCode(secret, mfa.TOTPStep(*a.now)+offset)
	if !ok {
		a.t.Fatal("no code")
	}
	return code
}

// wrongCode returns six digits that are not a code of secret at the
// current time step or the ones next to it.
func (a *mfaAPI) wrongCode(secret string) string {
	a.t.Helper()
	for _, candidate := range []string{"000000", "111111", "222222", "333333"} {
		if a.code(secret, -1) != candidate && a.code(secret, 0) != candidate && a.code(secret, 1) != candidate {
			return candidate
		}
	}
	a.t.Fatal("no wrong code")
	return ""
}

// setup starts the enrollment of an authenticator app from the account
// screen and returns the secret and the setup token.
func (a *mfaAPI) setup(access string) totpSetupResponse {
	a.t.Helper()
	w := a.call("/api/v1/mfa/totp/begin", access, map[string]string{"password": mfaPassword})
	expect(a.t, w, 200, "")
	if _, keys := jsonKeys(a.t, w.Body.Bytes()); !reflect.DeepEqual(keys, []string{"otpauth_url", "secret", "setup_token"}) {
		a.t.Fatalf("setup keys: %v", keys)
	}
	out := decodeJSON[totpSetupResponse](a.t, w)
	if len(out.Secret) != 32 || out.SetupToken == "" || strings.Contains(out.SetupToken, out.Secret) {
		a.t.Fatal("setup response")
	}
	return out
}

// enroll enrolls an authenticator app for the session access from the
// account screen and returns its secret and the recovery codes (nil when
// none were issued). The clock moves to the next time step, whose code is
// the first one that signs in.
func (a *mfaAPI) enroll(access string) (string, []string) {
	a.t.Helper()
	setup := a.setup(access)
	w := a.call("/api/v1/mfa/totp/finish", access, map[string]string{"setup_token": setup.SetupToken, "code": a.code(setup.Secret, 0)})
	expect(a.t, w, 200, "")
	if _, keys := jsonKeys(a.t, w.Body.Bytes()); !reflect.DeepEqual(keys, []string{"recovery_codes"}) {
		a.t.Fatalf("finish keys: %v", keys)
	}
	a.advance(30 * time.Second)
	return setup.Secret, decodeJSON[recoveryCodesResponse](a.t, w).RecoveryCodes
}

// signIn signs in with the password and the code of secret and moves the
// clock to the next time step.
func (a *mfaAPI) signIn(email, secret string) (string, string) {
	a.t.Helper()
	ch := a.pending(email)
	access, cookie := a.session(a.post("/auth/mfa/totp", map[string]string{"mfa_token": ch.Token, "code": a.code(secret, 0)}))
	a.advance(30 * time.Second)
	return access, cookie
}

func (a *mfaAPI) status(access string) mfaStatusResponse {
	a.t.Helper()
	w := a.do("GET", "/api/v1/mfa", access, "")
	expect(a.t, w, 200, "")
	if _, keys := jsonKeys(a.t, w.Body.Bytes()); !reflect.DeepEqual(keys, []string{"recovery_codes_remaining", "required", "totp", "webauthn", "webauthn_available"}) {
		a.t.Fatalf("status keys: %v", keys)
	}
	if !strings.Contains(w.Body.String(), `"webauthn":[`) {
		a.t.Fatalf("webauthn is not an array: %s", w.Body.String())
	}
	return decodeJSON[mfaStatusResponse](a.t, w)
}

// issuedCookie tells whether w sets a session cookie with a value; a
// response may clear the cookie of the browser without starting a session.
func issuedCookie(w *httptest.ResponseRecorder) bool {
	for _, c := range w.Result().Cookies() {
		if c.Name == SessionCookieName && c.Value != "" {
			return true
		}
	}
	return false
}

// noSession checks that a failed response starts no session.
func noSession(t *testing.T, w *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	expect(t, w, status, code)
	if issuedCookie(w) || strings.Contains(w.Body.String(), `"user"`) {
		t.Fatalf("a failure started a session: %v %s", w.Header().Values("Set-Cookie"), w.Body.String())
	}
}

var recoveryCodePattern = regexp.MustCompile(`^[a-km-np-z2-9]{4}(-[a-km-np-z2-9]{4}){3}$`)

func checkRecoveryCodes(t *testing.T, codes []string) {
	t.Helper()
	if len(codes) != 10 {
		t.Fatalf("%d recovery codes", len(codes))
	}
	seen := map[string]bool{}
	for _, c := range codes {
		if !recoveryCodePattern.MatchString(c) || seen[c] {
			t.Fatal("recovery code form")
		}
		seen[c] = true
	}
}

func mfaDrivers(t *testing.T, fn func(t *testing.T, driver string)) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) { fn(t, driver) })
	}
}

// TestMFAGate checks who gets a session from the password alone.
func TestMFAGate(t *testing.T) {
	mfaDrivers(t, func(t *testing.T, driver string) {
		api := newMFAAPI(t, driver, config.MFAOff)

		// No factor and none required: the login response is unchanged.
		w := api.public("/auth/login", `{"email":"alice@example.com","password":"`+mfaPassword+`"}`, "")
		if _, keys := jsonKeys(t, w.Body.Bytes()); !reflect.DeepEqual(keys, []string{"user"}) {
			t.Fatalf("login keys: %v", keys)
		}
		alice, aliceCookie := api.session(w)
		expect(t, api.do("GET", "/api/v1/me", alice, ""), 200, "")

		// With a factor the password starts no session.
		secret, codes := api.enroll(alice)
		checkRecoveryCodes(t, codes)
		ch := api.pending("alice@example.com")
		if !ch.Enrolled || ch.Methods != (mfaMethodsResponse{TOTP: true, Recovery: true}) {
			t.Fatalf("challenge: %+v", ch.Methods)
		}
		// A wrong password still fails as before, and tells nothing.
		noSession(t, api.public("/auth/login", `{"email":"alice@example.com","password":"wrong-password-123"}`, ""), 401, "unauthorized")
		// The session that enabled the factor is kept.
		expect(t, api.do("GET", "/api/v1/me", alice, ""), 200, "")
		expect(t, api.do("GET", "/api/v1/me", aliceCookie, ""), 200, "")

		// The code starts the session.
		w = api.post("/auth/mfa/totp", map[string]string{"mfa_token": ch.Token, "code": api.code(secret, 0)})
		if _, keys := jsonKeys(t, w.Body.Bytes()); !reflect.DeepEqual(keys, []string{"user"}) {
			t.Fatalf("totp keys: %v", keys)
		}
		access, cookie := api.session(w)
		if got := decodeJSON[userResponse](t, api.do("GET", "/api/v1/me", access, "")); got.ID != "alice-id" {
			t.Fatalf("me: %+v", got)
		}
		expect(t, api.do("GET", "/api/v1/me", cookie, ""), 200, "")
		// The same code is not accepted again, with the same token or a new
		// sign-in; neither is the code of an earlier step.
		noSession(t, api.post("/auth/mfa/totp", map[string]string{"mfa_token": ch.Token, "code": api.code(secret, 0)}), 401, "mfa_code_invalid")
		again := api.pending("alice@example.com")
		noSession(t, api.post("/auth/mfa/totp", map[string]string{"mfa_token": again.Token, "code": api.code(secret, 0)}), 401, "mfa_code_invalid")
		noSession(t, api.post("/auth/mfa/totp", map[string]string{"mfa_token": again.Token, "code": api.code(secret, -1)}), 401, "mfa_code_invalid")
		// The code of the next step is not accepted before its period.
		noSession(t, api.post("/auth/mfa/totp", map[string]string{"mfa_token": again.Token, "code": api.code(secret, 1)}), 401, "mfa_code_invalid")
		// The code of the step before is accepted while it is unused, once.
		api.advance(time.Minute)
		api.session(api.post("/auth/mfa/totp", map[string]string{"mfa_token": again.Token, "code": api.code(secret, -1)}))
		noSession(t, api.post("/auth/mfa/totp", map[string]string{"mfa_token": again.Token, "code": api.code(secret, -1)}), 401, "mfa_code_invalid")
		api.advance(30 * time.Second)

		// A user without an authenticator app has no code that works.
		_, carolCookie := api.login("carol@example.com", mfaPassword)

		// The policies: off asks nobody without a factor, owners the owners
		// and all everybody. A user with a factor is always asked.
		for _, tc := range []struct {
			policy string
			carol  bool
		}{{config.MFAOff, false}, {"", false}, {config.MFAOwners, false}, {config.MFAAll, true}} {
			api.use(tc.policy, false)
			if ch := api.pending("alice@example.com"); !ch.Enrolled || !ch.Methods.TOTP {
				t.Fatalf("%s: alice: %+v", tc.policy, ch)
			}
			if tc.carol {
				ch := api.pending("carol@example.com")
				if ch.Enrolled {
					t.Fatalf("%s: carol: %+v", tc.policy, ch)
				}
				// No factor: no code, of any kind, signs in.
				noSession(t, api.post("/auth/mfa/totp", map[string]string{"mfa_token": ch.Token, "code": "000000"}), 401, "mfa_code_invalid")
				noSession(t, api.post("/auth/mfa/recovery", map[string]string{"mfa_token": ch.Token, "code": codes[0]}), 401, "mfa_code_invalid")
			} else {
				api.login("carol@example.com", mfaPassword)
			}
		}
		// An owner without a factor is asked to enroll under owners.
		if err := api.store.ResetMFA(context.Background(), "alice@example.com"); err != nil {
			t.Fatal(err)
		}
		api.use(config.MFAOwners, false)
		if ch := api.pending("alice@example.com"); ch.Enrolled {
			t.Fatalf("owners: alice without a factor: %+v", ch)
		}
		// The reset revoked alice's sessions.
		expect(t, api.do("GET", "/api/v1/me", aliceCookie, ""), 401, "unauthorized")
		api.use(config.MFAOff, false)
		_, aliceCookie = api.login("alice@example.com", mfaPassword)
		// Requiring a factor does not end the sessions that exist.
		api.use(config.MFAAll, false)
		expect(t, api.do("GET", "/api/v1/me", carolCookie, ""), 200, "")
		expect(t, api.do("GET", "/api/v1/me", aliceCookie, ""), 200, "")
	})
}

// TestMFAGateAfterLinks checks that accepting an invitation and completing
// a password reset pass the same gate as the login.
func TestMFAGateAfterLinks(t *testing.T) {
	mfaDrivers(t, func(t *testing.T, driver string) {
		api := newMFAAPI(t, driver, config.MFAOff)
		alice, _ := api.login("alice@example.com", mfaPassword)
		carol, carolCookie := api.login("carol@example.com", mfaPassword)
		secret, _ := api.enroll(carol)
		before := api.pending("carol@example.com")

		// An invitation accepted where everybody needs a factor: the account
		// exists, but there is no session until a factor is enrolled. The
		// session of the cookie the browser sent is revoked as before.
		w := api.do("POST", "/api/v1/invitations", alice, `{"email":"dave@example.com","role":"member"}`)
		expect(t, w, 201, "")
		invitation := fragmentToken(t, decodeJSON[createdInvitationResponse](t, w).URL, usersOrigin+"/invite#")
		api.use(config.MFAAll, false)
		ch := api.challenge(api.public("/auth/invitations/accept", string(mustJSON(t, map[string]string{"token": invitation, "password": "dave-password-123"})), carolCookie))
		if ch.Enrolled {
			t.Fatalf("invitation: %+v", ch)
		}
		if _, err := api.store.UserByEmail(context.Background(), "dave@example.com"); err != nil {
			t.Fatalf("dave was not created: %v", err)
		}
		expect(t, api.do("GET", "/api/v1/me", carolCookie, ""), 401, "unauthorized")
		// The token of the invitation enrolls dave.
		setup := decodeJSON[totpSetupResponse](t, api.post("/auth/mfa/enroll/totp/begin", map[string]string{"mfa_token": ch.Token}))
		w = api.post("/auth/mfa/enroll/totp/finish", map[string]string{"mfa_token": ch.Token, "setup_token": setup.SetupToken, "code": api.code(setup.Secret, 0)})
		dave, _ := api.session(w)
		if got := decodeJSON[userResponse](t, api.do("GET", "/api/v1/me", dave, "")); got.Email != "dave@example.com" {
			t.Fatalf("me: %+v", got)
		}
		api.use(config.MFAOff, false)

		// A password reset keeps the factor: the new password alone starts
		// no session, and the tokens of the old password are dead.
		w = api.do("POST", "/api/v1/users/carol-id/password-resets", alice, "")
		expect(t, w, 201, "")
		reset := fragmentToken(t, decodeJSON[createdPasswordResetResponse](t, w).URL, usersOrigin+"/reset-password#")
		ch = api.challenge(api.public("/auth/password-resets/complete", string(mustJSON(t, map[string]string{"token": reset, "password": "replacement-password-123"})), ""))
		if !ch.Enrolled || !ch.Methods.TOTP || !ch.Methods.Recovery {
			t.Fatalf("reset: %+v", ch)
		}
		noSession(t, api.post("/auth/mfa/totp", map[string]string{"mfa_token": before.Token, "code": api.code(secret, 0)}), 401, "mfa_token_invalid")
		expect(t, api.do("GET", "/api/v1/me", carol, ""), 401, "unauthorized")
		access, _ := api.session(api.post("/auth/mfa/totp", map[string]string{"mfa_token": ch.Token, "code": api.code(secret, 0)}))
		if got := decodeJSON[userResponse](t, api.do("GET", "/api/v1/me", access, "")); got.ID != "carol-id" || got.PasswordReset == nil {
			t.Fatalf("me after the reset: %+v", got)
		}
		api.advance(30 * time.Second)
		// The new password asks for the factor at the login as well.
		w = api.public("/auth/login", `{"email":"carol@example.com","password":"replacement-password-123"}`, "")
		if ch = api.challenge(w); !ch.Enrolled {
			t.Fatalf("login after the reset: %+v", ch)
		}
	})
}

// TestMFATokens checks the MFA token and the setup token.
func TestMFATokens(t *testing.T) {
	mfaDrivers(t, func(t *testing.T, driver string) {
		ctx := context.Background()
		api := newMFAAPI(t, driver, config.MFAOff)
		alice, _ := api.login("alice@example.com", mfaPassword)
		carol, _ := api.login("carol@example.com", mfaPassword)
		secret, _ := api.enroll(alice)
		totp := func(token, code string) *httptest.ResponseRecorder {
			return api.post("/auth/mfa/totp", map[string]string{"mfa_token": token, "code": code})
		}

		// Ten minutes for a user with a factor.
		ch := api.pending("alice@example.com")
		api.advance(10*time.Minute - time.Second)
		noSession(t, totp(ch.Token, api.wrongCode(secret)), 401, "mfa_code_invalid")
		api.advance(time.Second)
		noSession(t, totp(ch.Token, api.code(secret, 0)), 401, "mfa_token_invalid")
		noSession(t, api.post("/auth/mfa/recovery", map[string]string{"mfa_token": ch.Token, "code": "abcd-efgh-jkmn-pqrs"}), 401, "mfa_token_invalid")
		// The access token of before has expired by now.
		alice, _ = api.signIn("alice@example.com", secret)

		// Malformed, tampered and foreign tokens.
		ch = api.pending("alice@example.com")
		u, err := api.store.UserByEmail(ctx, "alice@example.com")
		if err != nil {
			t.Fatal(err)
		}
		content := string(mustJSON(t, mfaToken{UserID: "alice-id", PasswordHash: auth.Hash(u.PasswordHash), ExpiresAt: api.now.Unix() + 600}))
		seal := func(purpose, plain string) string {
			t.Helper()
			v, err := api.srv.box.Seal(purpose, plain)
			if err != nil {
				t.Fatal(err)
			}
			return v
		}
		// The content is the right one: sealed for the MFA token it works.
		noSession(t, totp(seal(mfaTokenPurpose, content), api.wrongCode(secret)), 401, "mfa_code_invalid")
		tampered := []byte(ch.Token)
		tampered[len(tampered)/2] ^= 1
		setup := api.setup(alice)
		for name, token := range map[string]string{
			"empty":           "",
			"garbage":         "v1.AAAA",
			"not sealed":      content,
			"tampered":        string(tampered),
			"other purpose":   seal("other-purpose", content),
			"setup purpose":   seal(totpSetupPurpose+"alice-id", content),
			"setup token":     setup.SetupToken,
			"access token":    alice,
			"too long":        ch.Token + strings.Repeat("A", maxMFATokenLength),
			"missing user":    seal(mfaTokenPurpose, string(mustJSON(t, mfaToken{UserID: "missing-id", PasswordHash: auth.Hash(u.PasswordHash), ExpiresAt: api.now.Unix() + 600}))),
			"no user":         seal(mfaTokenPurpose, string(mustJSON(t, mfaToken{PasswordHash: auth.Hash(u.PasswordHash), ExpiresAt: api.now.Unix() + 600}))),
			"other hash":      seal(mfaTokenPurpose, string(mustJSON(t, mfaToken{UserID: "alice-id", PasswordHash: auth.Hash("x"), ExpiresAt: api.now.Unix() + 600}))),
			"plain hash":      seal(mfaTokenPurpose, string(mustJSON(t, mfaToken{UserID: "alice-id", PasswordHash: u.PasswordHash, ExpiresAt: api.now.Unix() + 600}))),
			"expired content": seal(mfaTokenPurpose, string(mustJSON(t, mfaToken{UserID: "alice-id", PasswordHash: auth.Hash(u.PasswordHash), ExpiresAt: api.now.Unix()}))),
			"not an object":   seal(mfaTokenPurpose, `"alice-id"`),
		} {
			for _, path := range []string{"/auth/mfa/totp", "/auth/mfa/recovery", "/auth/mfa/enroll/totp/begin", "/auth/mfa/enroll/totp/finish"} {
				body := map[string]string{"mfa_token": token}
				switch path {
				case "/auth/mfa/totp":
					body["code"] = api.code(secret, 0)
				case "/auth/mfa/recovery":
					body["code"] = "abcd-efgh-jkmn-pqrs"
				case "/auth/mfa/enroll/totp/finish":
					body["setup_token"], body["code"] = setup.SetupToken, api.code(setup.Secret, 0)
				}
				w := api.post(path, body)
				if w.Code != 401 || errorCode(t, w) != "mfa_token_invalid" || len(w.Header().Values("Set-Cookie")) != 0 {
					t.Fatalf("%s at %s: %d %s", name, path, w.Code, w.Body.String())
				}
			}
		}
		// The token still works after all of that.
		api.session(totp(ch.Token, api.code(secret, 0)))
		api.advance(30 * time.Second)

		// A password change and disabling end the tokens issued before.
		ch = api.pending("alice@example.com")
		hash, err := auth.Password("replacement-password-123")
		if err != nil {
			t.Fatal(err)
		}
		if err = api.store.ChangeUser(ctx, "alice@example.com", hash, false, api.now.Unix()); err != nil {
			t.Fatal(err)
		}
		noSession(t, totp(ch.Token, api.code(secret, 0)), 401, "mfa_token_invalid")
		ch = api.challenge(api.public("/auth/login", `{"email":"alice@example.com","password":"replacement-password-123"}`, ""))
		if err = api.store.ChangeUser(ctx, "alice@example.com", "", true, api.now.Unix()); err != nil {
			t.Fatal(err)
		}
		noSession(t, totp(ch.Token, api.code(secret, 0)), 401, "mfa_token_invalid")
		// Enabled again, the token of before works: the factor was kept.
		if err = api.store.EnableUser(ctx, "alice@example.com"); err != nil {
			t.Fatal(err)
		}
		api.session(totp(ch.Token, api.code(secret, 0)))
		api.advance(30 * time.Second)

		// Thirty minutes for a user that has to enroll first, and for the
		// setup token.
		api.use(config.MFAAll, false)
		ch = api.pending("carol@example.com")
		begin := func(token string) *httptest.ResponseRecorder {
			return api.post("/auth/mfa/enroll/totp/begin", map[string]string{"mfa_token": token})
		}
		w := begin(ch.Token)
		expect(t, w, 200, "")
		if _, keys := jsonKeys(t, w.Body.Bytes()); !reflect.DeepEqual(keys, []string{"otpauth_url", "secret", "setup_token"}) {
			t.Fatalf("begin keys: %v", keys)
		}
		first := decodeJSON[totpSetupResponse](t, w)
		link, err := url.Parse(first.OtpauthURL)
		if err != nil || link.Scheme != "otpauth" || link.Host != "totp" || link.Path != "/Astovia (astovia.test):carol@example.com" || link.Query().Get("secret") != first.Secret || link.Query().Get("issuer") != "Astovia (astovia.test)" {
			t.Fatalf("otpauth URL: %v", err)
		}
		api.advance(30*time.Minute - time.Second)
		expect(t, begin(ch.Token), 200, "")
		// The first setup token is still good one second before its expiry,
		// for a wrong code as well.
		noSession(t, api.post("/auth/mfa/enroll/totp/finish", map[string]string{"mfa_token": ch.Token, "setup_token": first.SetupToken, "code": "12345"}), 401, "mfa_code_invalid")
		api.advance(time.Second)
		noSession(t, begin(ch.Token), 401, "mfa_token_invalid")
		// A new sign-in with the expired setup token.
		ch = api.pending("carol@example.com")
		noSession(t, api.post("/auth/mfa/enroll/totp/finish", map[string]string{"mfa_token": ch.Token, "setup_token": first.SetupToken, "code": api.code(first.Secret, 0)}), 400, "mfa_state_invalid")

		// Setup tokens: another user's, the MFA token and garbage do not open.
		api.use(config.MFAOff, false)
		carol, _ = api.login("carol@example.com", mfaPassword)
		carolSetup := api.setup(carol)
		api.use(config.MFAAll, false)
		dave := store.User{ID: "dave-id", Email: "dave@example.com", PasswordHash: u.PasswordHash, CreatedAt: 3, Role: store.RoleMember}
		if err = api.store.CreateUser(ctx, dave); err != nil {
			t.Fatal(err)
		}
		daveToken := api.pending("dave@example.com").Token
		for name, token := range map[string]string{"another user's": carolSetup.SetupToken, "the MFA token": daveToken, "empty": "", "garbage": "v1.AAAA", "too long": carolSetup.SetupToken + strings.Repeat("A", maxMFATokenLength)} {
			w := api.post("/auth/mfa/enroll/totp/finish", map[string]string{"mfa_token": daveToken, "setup_token": token, "code": api.code(carolSetup.Secret, 0)})
			if w.Code != 400 || errorCode(t, w) != "mfa_state_invalid" || len(w.Header().Values("Set-Cookie")) != 0 {
				t.Fatalf("%s setup token: %d %s", name, w.Code, w.Body.String())
			}
		}
		if status, err := api.store.MFAStatus(ctx, "dave-id"); err != nil || status.Enrolled() {
			t.Fatalf("dave: %+v %v", status, err)
		}
		// carol's own setup token, from the account screen, serves carol.
		api.session(api.post("/auth/mfa/enroll/totp/finish", map[string]string{"mfa_token": ch.Token, "setup_token": carolSetup.SetupToken, "code": api.code(carolSetup.Secret, 0)}))
	})
}

// TestMFAEnrollBeforeSession checks the enrollment of a user that must
// have a factor, and that nobody else can enroll with the password alone.
func TestMFAEnrollBeforeSession(t *testing.T) {
	mfaDrivers(t, func(t *testing.T, driver string) {
		ctx := context.Background()
		api := newMFAAPI(t, driver, config.MFAOwners)
		begin := func(token string) *httptest.ResponseRecorder {
			return api.post("/auth/mfa/enroll/totp/begin", map[string]string{"mfa_token": token})
		}
		finish := func(token, setup, code string) *httptest.ResponseRecorder {
			return api.post("/auth/mfa/enroll/totp/finish", map[string]string{"mfa_token": token, "setup_token": setup, "code": code})
		}

		ch := api.pending("alice@example.com")
		if ch.Enrolled {
			t.Fatalf("alice: %+v", ch)
		}
		setup := decodeJSON[totpSetupResponse](t, begin(ch.Token))
		// Nothing is stored before the code confirms the secret.
		if status, err := api.store.MFAStatus(ctx, "alice-id"); err != nil || status.Enrolled() {
			t.Fatalf("after begin: %+v %v", status, err)
		}
		for _, code := range []string{"", "12345", "1234567", "abcdef", api.wrongCode(setup.Secret)} {
			noSession(t, finish(ch.Token, setup.SetupToken, code), 401, "mfa_code_invalid")
		}
		noSession(t, finish(ch.Token, "v1.AAAA", api.code(setup.Secret, 0)), 400, "mfa_state_invalid")
		if status, err := api.store.MFAStatus(ctx, "alice-id"); err != nil || status.Enrolled() {
			t.Fatalf("after wrong codes: %+v %v", status, err)
		}

		// The code, typed with spaces, enrolls and signs in.
		code := api.code(setup.Secret, 0)
		w := finish(ch.Token, setup.SetupToken, " "+code[:3]+" "+code[3:]+" ")
		if _, keys := jsonKeys(t, w.Body.Bytes()); !reflect.DeepEqual(keys, []string{"recovery_codes", "user"}) {
			t.Fatalf("finish keys: %v", keys)
		}
		alice, _ := api.session(w)
		codes := decodeJSON[mfaAuthResponse](t, w).RecoveryCodes
		checkRecoveryCodes(t, codes)
		if got := api.status(alice); !got.Required || got.TOTP == nil || got.TOTP.CreatedAt != formatTime(api.now.Unix()) || len(got.WebAuthn) != 0 || got.RecoveryCodesRemaining != 10 || !got.WebAuthnAvailable {
			t.Fatalf("status: %+v", got)
		}

		// The same token enrolls nothing more: the account has a factor.
		noSession(t, begin(ch.Token), 409, "mfa_already_enrolled")
		other := api.setup(alice)
		noSession(t, finish(ch.Token, other.SetupToken, api.code(other.Secret, 0)), 409, "mfa_already_enrolled")
		noSession(t, finish(ch.Token, setup.SetupToken, api.code(setup.Secret, 1)), 409, "mfa_already_enrolled")

		// The code that confirmed the secret does not sign in; the next does.
		enrolled := api.pending("alice@example.com")
		if !enrolled.Enrolled || enrolled.Methods != (mfaMethodsResponse{TOTP: true, Recovery: true}) {
			t.Fatalf("after the enrollment: %+v", enrolled)
		}
		noSession(t, api.post("/auth/mfa/totp", map[string]string{"mfa_token": enrolled.Token, "code": code}), 401, "mfa_code_invalid")
		// The token of a user with a factor, issued by the password alone,
		// cannot replace or add a factor.
		noSession(t, begin(enrolled.Token), 409, "mfa_already_enrolled")
		noSession(t, finish(enrolled.Token, other.SetupToken, api.code(other.Secret, 0)), 409, "mfa_already_enrolled")
		noSession(t, finish(enrolled.Token, other.SetupToken, api.wrongCode(other.Secret)), 409, "mfa_already_enrolled")
		api.advance(30 * time.Second)
		api.session(api.post("/auth/mfa/totp", map[string]string{"mfa_token": enrolled.Token, "code": api.code(setup.Secret, 0)}))
		api.advance(30 * time.Second)
		// The secret is the first one, as the sign-in showed: the rejected
		// enrollments changed nothing.
		if got := api.status(alice); got.RecoveryCodesRemaining != 10 {
			t.Fatalf("codes: %d", got.RecoveryCodesRemaining)
		}

		// A user that need not have a factor cannot enroll before a session:
		// carol's token was issued while everybody had to.
		api.use(config.MFAAll, false)
		carol := api.pending("carol@example.com")
		carolSetup := decodeJSON[totpSetupResponse](t, begin(carol.Token))
		api.use(config.MFAOwners, false)
		noSession(t, begin(carol.Token), 403, "mfa_not_required")
		noSession(t, finish(carol.Token, carolSetup.SetupToken, api.code(carolSetup.Secret, 0)), 403, "mfa_not_required")
		if status, err := api.store.MFAStatus(ctx, "carol-id"); err != nil || status.Enrolled() || status.RecoveryCodes != 0 {
			t.Fatalf("carol: %+v %v", status, err)
		}

		// Two enrollments of the same user at once: one factor, one set of
		// recovery codes and one session.
		api.use(config.MFAAll, false)
		for round := 0; round < 5; round++ {
			carol = api.pending("carol@example.com")
			a := decodeJSON[totpSetupResponse](t, begin(carol.Token))
			b := decodeJSON[totpSetupResponse](t, begin(carol.Token))
			results := api.parallel("/auth/mfa/enroll/totp/finish",
				map[string]string{"mfa_token": carol.Token, "setup_token": a.SetupToken, "code": api.code(a.Secret, 0)},
				map[string]string{"mfa_token": carol.Token, "setup_token": b.SetupToken, "code": api.code(b.Secret, 0)})
			sessions := 0
			for _, w := range results {
				switch {
				case w.Code == 200 && len(w.Header().Values("Set-Cookie")) == 1:
					sessions++
				case w.Code == 409 && errorCode(t, w) == "mfa_already_enrolled" && len(w.Header().Values("Set-Cookie")) == 0:
				default:
					t.Fatalf("round %d: %d %s", round, w.Code, w.Body.String())
				}
			}
			status, err := api.store.MFAStatus(ctx, "carol-id")
			if err != nil || sessions != 1 || status.TOTP == nil || status.RecoveryCodes != 10 {
				t.Fatalf("round %d: %d sessions, %+v %v", round, sessions, status, err)
			}
			if err = api.store.ResetMFA(ctx, "carol@example.com"); err != nil {
				t.Fatal(err)
			}
		}
	})
}

// parallel posts the bodies to path at the same time.
func (a *mfaAPI) parallel(path string, bodies ...map[string]string) []*httptest.ResponseRecorder {
	a.t.Helper()
	raw := make([]string, len(bodies))
	for i, body := range bodies {
		raw[i] = string(mustJSON(a.t, body))
	}
	return a.parallelRaw(path, raw...)
}

// parallelRaw posts the JSON bodies to path at the same time.
func (a *mfaAPI) parallelRaw(path string, bodies ...string) []*httptest.ResponseRecorder {
	a.t.Helper()
	out := make([]*httptest.ResponseRecorder, len(bodies))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, raw := range bodies {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := httptest.NewRequest("POST", path, strings.NewReader(raw))
			r.Header.Set("Origin", usersOrigin)
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			<-start
			a.handler.ServeHTTP(w, r)
			out[i] = w
		}()
	}
	close(start)
	wg.Wait()
	return out
}

// TestMFARecoveryCodes checks signing in with the recovery codes.
func TestMFARecoveryCodes(t *testing.T) {
	mfaDrivers(t, func(t *testing.T, driver string) {
		ctx := context.Background()
		api := newMFAAPI(t, driver, config.MFAOff)
		alice, _ := api.login("alice@example.com", mfaPassword)
		carol, _ := api.login("carol@example.com", mfaPassword)
		secret, aliceCodes := api.enroll(alice)
		_, carolCodes := api.enroll(carol)
		checkRecoveryCodes(t, aliceCodes)
		checkRecoveryCodes(t, carolCodes)
		recovery := func(token, code string) *httptest.ResponseRecorder {
			return api.post("/auth/mfa/recovery", map[string]string{"mfa_token": token, "code": code})
		}

		ch := api.pending("alice@example.com")
		// Another user's code, an unknown one and malformed ones.
		for _, code := range []string{carolCodes[0], "abcd-efgh-jkmn-pqrs", "", "abcd", aliceCodes[0] + "a", strings.ReplaceAll(aliceCodes[0], "-", "_"), "000000", api.code(secret, 0)} {
			noSession(t, recovery(ch.Token, code), 401, "mfa_code_invalid")
		}
		// A code signs in once.
		w := recovery(ch.Token, aliceCodes[0])
		if _, keys := jsonKeys(t, w.Body.Bytes()); !reflect.DeepEqual(keys, []string{"user"}) {
			t.Fatalf("recovery keys: %v", keys)
		}
		access, _ := api.session(w)
		if got := api.status(access); got.RecoveryCodesRemaining != 9 || got.TOTP == nil {
			t.Fatalf("status: %+v", got)
		}
		noSession(t, recovery(ch.Token, aliceCodes[0]), 401, "mfa_code_invalid")
		noSession(t, recovery(api.pending("alice@example.com").Token, aliceCodes[0]), 401, "mfa_code_invalid")
		// Upper case, spaces and missing hyphens are the same code.
		api.session(recovery(ch.Token, strings.ToUpper(aliceCodes[1])))
		api.session(recovery(ch.Token, "  "+strings.ReplaceAll(aliceCodes[2], "-", " ")+"\n"))
		api.session(recovery(ch.Token, strings.ReplaceAll(aliceCodes[3], "-", "")))
		if got := api.status(access); got.RecoveryCodesRemaining != 6 {
			t.Fatalf("remaining: %d", got.RecoveryCodesRemaining)
		}
		// carol's codes are untouched and sign carol in.
		api.session(recovery(api.pending("carol@example.com").Token, carolCodes[0]))

		// The same code twice at once: one session.
		for round := 4; round < 9; round++ {
			body := map[string]string{"mfa_token": ch.Token, "code": aliceCodes[round]}
			sessions := 0
			for _, w := range api.parallel("/auth/mfa/recovery", body, body) {
				switch {
				case w.Code == 200 && len(w.Header().Values("Set-Cookie")) == 1:
					sessions++
				case w.Code == 401 && errorCode(t, w) == "mfa_code_invalid" && len(w.Header().Values("Set-Cookie")) == 0:
				default:
					t.Fatalf("code %d: %d %s", round, w.Code, w.Body.String())
				}
			}
			if sessions != 1 {
				t.Fatalf("code %d: %d sessions", round, sessions)
			}
		}
		// The same authenticator code twice at once: one session.
		for round := 0; round < 5; round++ {
			body := map[string]string{"mfa_token": ch.Token, "code": api.code(secret, 0)}
			sessions := 0
			for _, w := range api.parallel("/auth/mfa/totp", body, body) {
				switch {
				case w.Code == 200 && len(w.Header().Values("Set-Cookie")) == 1:
					sessions++
				case w.Code == 401 && errorCode(t, w) == "mfa_code_invalid" && len(w.Header().Values("Set-Cookie")) == 0:
				default:
					t.Fatalf("round %d: %d %s", round, w.Code, w.Body.String())
				}
			}
			if sessions != 1 {
				t.Fatalf("round %d: %d sessions", round, sessions)
			}
			api.advance(30 * time.Second)
		}
		// With the last code used, the challenge offers no recovery code.
		api.session(recovery(ch.Token, aliceCodes[9]))
		if got := api.pending("alice@example.com"); got.Methods != (mfaMethodsResponse{TOTP: true}) {
			t.Fatalf("methods without codes: %+v", got.Methods)
		}

		// The database holds neither the secret nor the codes.
		name := "sqlite"
		if driver == "postgres" {
			name = "pgx"
		}
		raw, err := sql.Open(name, api.dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = raw.Close() }()
		var sealed string
		if err = raw.QueryRowContext(ctx, "SELECT secret_sealed FROM mfa_totp WHERE user_id = 'alice-id'").Scan(&sealed); err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(sealed, "v1.") || strings.Contains(sealed, secret) {
			t.Fatal("the secret is not sealed")
		}
		if opened, err := api.srv.box.Open(totpSecretPurpose+"alice-id", sealed); err != nil || opened != secret {
			t.Fatalf("the sealed secret does not open: %v", err)
		}
		if _, err = api.srv.box.Open(totpSecretPurpose+"carol-id", sealed); err == nil {
			t.Fatal("the secret opens for another user")
		}
		rows, err := raw.QueryContext(ctx, "SELECT code_hash FROM mfa_recovery_codes")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rows.Close() }()
		hashes := map[string]bool{}
		for rows.Next() {
			var hash string
			if err = rows.Scan(&hash); err != nil {
				t.Fatal(err)
			}
			if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(hash) {
				t.Fatal("a stored code is not a SHA-256")
			}
			hashes[hash] = true
		}
		if err = rows.Err(); err != nil {
			t.Fatal(err)
		}
		if len(hashes) != 20 || !hashes[auth.Hash(strings.ReplaceAll(carolCodes[1], "-", ""))] {
			t.Fatalf("%d stored codes", len(hashes))
		}
	})
}

// TestMFARequests checks the request checks of the /auth/mfa/* endpoints.
func TestMFARequests(t *testing.T) {
	api := newMFAAPI(t, "sqlite", config.MFAAll)
	token := api.pending("alice@example.com").Token
	// The requests that carry the response of an authenticator may be
	// larger.
	for path, limit := range map[string]int{
		"/auth/mfa/totp": mfaBodyLimit, "/auth/mfa/recovery": mfaBodyLimit, "/auth/mfa/enroll/totp/begin": mfaBodyLimit, "/auth/mfa/enroll/totp/finish": mfaBodyLimit,
		"/auth/mfa/webauthn/begin": mfaBodyLimit, "/auth/mfa/webauthn/finish": webauthnBodyLimit, "/auth/mfa/enroll/webauthn/begin": mfaBodyLimit, "/auth/mfa/enroll/webauthn/finish": webauthnBodyLimit,
	} {
		body := string(mustJSON(t, map[string]string{"mfa_token": token}))
		for _, tc := range []struct {
			name, method, origin, ct, body string
			status                         int
			code                           string
		}{
			{"no origin", "POST", "", "application/json", body, 403, "origin_rejected"},
			{"other origin", "POST", "https://evil.test", "application/json", body, 403, "origin_rejected"},
			{"form", "POST", usersOrigin, "application/x-www-form-urlencoded", body, 415, "unsupported_media_type"},
			{"no content type", "POST", usersOrigin, "", body, 415, "unsupported_media_type"},
			{"large", "POST", usersOrigin, "application/json", `{"mfa_token":"` + strings.Repeat("x", limit+1) + `"}`, 413, "body_too_large"},
			{"not an object", "POST", usersOrigin, "application/json", `["x"]`, 400, "invalid_request"},
			{"unknown field", "POST", usersOrigin, "application/json", `{"mfa_token":"x","user_id":"alice-id"}`, 400, "invalid_request"},
			{"wrong type", "POST", usersOrigin, "application/json", `{"mfa_token":1}`, 400, "invalid_request"},
			{"get", "GET", usersOrigin, "application/json", "", 405, "method_not_allowed"},
			{"delete", "DELETE", usersOrigin, "application/json", body, 405, "method_not_allowed"},
		} {
			r := httptest.NewRequest(tc.method, path, strings.NewReader(tc.body))
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
			}
			if tc.ct != "" {
				r.Header.Set("Content-Type", tc.ct)
			}
			w := serve(t, api.handler, r)
			if w.Code != tc.status || errorCode(t, w) != tc.code || len(w.Header().Values("Set-Cookie")) != 0 {
				t.Fatalf("%s %s: %d %s", path, tc.name, w.Code, w.Body.String())
			}
		}
	}
	// A body over the smaller limit reaches the handlers of the larger one.
	large := strings.Repeat("x", mfaBodyLimit+1)
	noSession(t, api.public("/auth/mfa/webauthn/finish", `{"mfa_token":"`+large+`","credential":{}}`, ""), 401, "mfa_token_invalid")
	noSession(t, api.public("/auth/mfa/enroll/webauthn/finish", `{"mfa_token":"`+large+`","credential":{},"name":"Key"}`, ""), 401, "mfa_token_invalid")
	for _, path := range []string{"/auth/mfa", "/auth/mfa/webauthn", "/auth/mfa/enroll", "/auth/mfa/enroll/webauthn"} {
		expect(t, api.post(path, map[string]string{"mfa_token": token}), 404, "not_found")
	}
}

// TestMFAManagement checks the two-step verification of the account screen.
func TestMFAManagement(t *testing.T) {
	mfaDrivers(t, func(t *testing.T, driver string) {
		ctx := context.Background()
		api := newMFAAPI(t, driver, config.MFAOff)
		alice, aliceCookie := api.login("alice@example.com", mfaPassword)
		carol, _ := api.login("carol@example.com", mfaPassword)
		w := api.do("POST", "/api/v1/api-tokens", alice, `{"name":"cli"}`)
		expect(t, w, 201, "")
		apiToken := decodeJSON[createdAPITokenResponse](t, w).Token

		if got := api.status(alice); got.Required || got.TOTP != nil || len(got.WebAuthn) != 0 || got.RecoveryCodesRemaining != 0 || !got.WebAuthnAvailable {
			t.Fatalf("status: %+v", got)
		}
		unchanged := func(userID string, want store.MFAStatus) {
			t.Helper()
			got, err := api.store.MFAStatus(ctx, userID)
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("%s changed: %+v, want %+v (%v)", userID, got, want, err)
			}
		}
		state := func(userID string) store.MFAStatus {
			t.Helper()
			got, err := api.store.MFAStatus(ctx, userID)
			if err != nil {
				t.Fatal(err)
			}
			return got
		}
		paths := []string{"/api/v1/mfa/totp/begin", "/api/v1/mfa/totp/delete", "/api/v1/mfa/recovery-codes"}

		// The password is checked, and its failures are 400s: a 401 would
		// make the web UI refresh the session and send the request again.
		empty := state("alice-id")
		for _, path := range paths {
			expect(t, api.call(path, alice, map[string]string{"password": "wrong-password-123"}), 400, "password_invalid")
			expect(t, api.call(path, alice, map[string]string{"password": strings.Repeat("p", 1025)}), 400, "invalid_request")
			expect(t, api.call(path, alice, map[string]string{"password": ""}), 400, "invalid_request")
			expect(t, api.call(path, alice, map[string]string{}), 400, "invalid_request")
			expect(t, api.do("POST", path, alice, `{"password":"`+mfaPassword+`","extra":1}`), 400, "invalid_request")
			expect(t, api.do("POST", path, alice, `{"password":"`+strings.Repeat("p", 4097)+`"}`), 413, "body_too_large")
		}
		unchanged("alice-id", empty)
		// Without a factor there is nothing to remove or reissue.
		expect(t, api.call("/api/v1/mfa/totp/delete", alice, map[string]string{"password": mfaPassword}), 404, "not_found")
		expect(t, api.call("/api/v1/mfa/recovery-codes", alice, map[string]string{"password": mfaPassword}), 409, "mfa_not_enrolled")
		unchanged("alice-id", empty)

		// The setup token and the code, with 400s as well.
		setup := api.setup(alice)
		carolSetup := api.setup(carol)
		finish := func(access, token, code string) *httptest.ResponseRecorder {
			return api.call("/api/v1/mfa/totp/finish", access, map[string]string{"setup_token": token, "code": code})
		}
		for _, code := range []string{"", "12345", "abcdef", api.wrongCode(setup.Secret)} {
			expect(t, finish(alice, setup.SetupToken, code), 400, "mfa_code_invalid")
		}
		mfaToken := func() string {
			api.use(config.MFAAll, false)
			defer api.use(config.MFAOff, false)
			return api.pending("alice@example.com").Token
		}()
		for _, token := range []string{"", "v1.AAAA", carolSetup.SetupToken, mfaToken, alice} {
			expect(t, finish(alice, token, api.code(setup.Secret, 0)), 400, "mfa_state_invalid")
			expect(t, finish(alice, token, api.code(carolSetup.Secret, 0)), 400, "mfa_state_invalid")
		}
		expect(t, api.do("POST", "/api/v1/mfa/totp/finish", alice, `{"setup_token":"x","code":"000000","password":"x"}`), 400, "invalid_request")
		unchanged("alice-id", empty)

		// The first factor returns the recovery codes.
		w = finish(alice, setup.SetupToken, api.code(setup.Secret, 0))
		expect(t, w, 200, "")
		codes := decodeJSON[recoveryCodesResponse](t, w).RecoveryCodes
		checkRecoveryCodes(t, codes)
		enrolledAt := api.now.Unix()
		if got := api.status(alice); got.TOTP == nil || got.TOTP.CreatedAt != formatTime(enrolledAt) || got.RecoveryCodesRemaining != 10 || got.Required {
			t.Fatalf("status: %+v", got)
		}
		unchanged("carol-id", store.MFAStatus{WebAuthn: []store.WebAuthnCredential{}})
		api.advance(30 * time.Second)
		// The session, its refresh and the API token are kept.
		expect(t, api.do("GET", "/api/v1/me", alice, ""), 200, "")
		expect(t, api.do("GET", "/api/v1/me", aliceCookie, ""), 200, "")
		expect(t, api.do("GET", "/api/v1/action-items", apiToken, ""), 200, "")
		api.signIn("alice@example.com", setup.Secret)

		// Replacing the authenticator app: the old secret stops working, the
		// recovery codes stay and none are returned.
		before := state("alice-id")
		expect(t, api.call("/api/v1/mfa/totp/begin", alice, map[string]string{"password": "wrong-password-123"}), 400, "password_invalid")
		replacement := api.setup(alice)
		expect(t, finish(alice, replacement.SetupToken, api.wrongCode(replacement.Secret)), 400, "mfa_code_invalid")
		unchanged("alice-id", before)
		w = finish(alice, replacement.SetupToken, api.code(replacement.Secret, 0))
		expect(t, w, 200, "")
		if got := strings.TrimSpace(w.Body.String()); got != `{"recovery_codes":null}` {
			t.Fatalf("replacement: %s", got)
		}
		api.advance(30 * time.Second)
		if got := api.status(alice); got.TOTP == nil || got.TOTP.CreatedAt == formatTime(enrolledAt) || got.RecoveryCodesRemaining != 10 {
			t.Fatalf("status after the replacement: %+v", got)
		}
		ch := api.pending("alice@example.com")
		// The code of the old secret, unless it happens to be one of the new.
		if old := api.code(setup.Secret, 0); old != api.code(replacement.Secret, -1) && old != api.code(replacement.Secret, 0) && old != api.code(replacement.Secret, 1) {
			noSession(t, api.post("/auth/mfa/totp", map[string]string{"mfa_token": ch.Token, "code": old}), 401, "mfa_code_invalid")
		}
		api.session(api.post("/auth/mfa/totp", map[string]string{"mfa_token": ch.Token, "code": api.code(replacement.Secret, 0)}))
		api.advance(30 * time.Second)
		// A setup token is good for the user as long as it has not expired,
		// here in a new session: the access token of the old one has expired.
		late := api.setup(alice)
		api.advance(30*time.Minute - time.Second)
		alice, aliceCookie = api.signIn("alice@example.com", replacement.Secret)
		api.advance(-30 * time.Second)
		expect(t, finish(alice, late.SetupToken, api.wrongCode(late.Secret)), 400, "mfa_code_invalid")
		api.advance(time.Second)
		expect(t, finish(alice, late.SetupToken, api.code(late.Secret, 0)), 400, "mfa_state_invalid")
		api.advance(30 * time.Second)

		// Reissuing the recovery codes ends the old ones.
		w = api.call("/api/v1/mfa/recovery-codes", alice, map[string]string{"password": mfaPassword})
		expect(t, w, 200, "")
		if _, keys := jsonKeys(t, w.Body.Bytes()); !reflect.DeepEqual(keys, []string{"recovery_codes"}) {
			t.Fatalf("reissue keys: %v", keys)
		}
		reissued := decodeJSON[recoveryCodesResponse](t, w).RecoveryCodes
		checkRecoveryCodes(t, reissued)
		ch = api.pending("alice@example.com")
		noSession(t, api.post("/auth/mfa/recovery", map[string]string{"mfa_token": ch.Token, "code": codes[0]}), 401, "mfa_code_invalid")
		api.session(api.post("/auth/mfa/recovery", map[string]string{"mfa_token": ch.Token, "code": reissued[0]}))
		if got := api.status(alice); got.RecoveryCodesRemaining != 9 {
			t.Fatalf("remaining: %d", got.RecoveryCodesRemaining)
		}

		// Access tokens only: an API token, the MFA token and no token.
		for _, bearer := range []string{apiToken, ch.Token, ""} {
			expect(t, api.do("GET", "/api/v1/mfa", bearer, ""), 401, "unauthorized")
			for _, path := range append([]string{"/api/v1/mfa/totp/finish"}, paths...) {
				expect(t, api.call(path, bearer, map[string]string{"password": mfaPassword}), 401, "unauthorized")
			}
		}
		// JSON only, and the methods.
		for _, path := range append([]string{"/api/v1/mfa/totp/finish"}, paths...) {
			r := httptest.NewRequest("POST", path, strings.NewReader(`{"password":"`+mfaPassword+`"}`))
			authorize(r, alice)
			expect(t, serve(t, api.handler, r), 415, "unsupported_media_type")
			expect(t, api.do("GET", path, alice, ""), 405, "method_not_allowed")
			expect(t, api.do("DELETE", path, alice, ""), 405, "method_not_allowed")
		}
		expect(t, api.call("/api/v1/mfa", alice, map[string]string{}), 405, "method_not_allowed")
		expect(t, api.do("DELETE", "/api/v1/mfa/totp", alice, ""), 404, "not_found")

		// A user that must have a factor cannot remove the last one.
		before = state("alice-id")
		carol, _ = api.login("carol@example.com", mfaPassword)
		for _, policy := range []string{config.MFAOwners, config.MFAAll} {
			api.use(policy, false)
			if got := api.status(alice); !got.Required {
				t.Fatalf("%s: alice is not required", policy)
			}
			if got := api.status(carol); got.Required != (policy == config.MFAAll) {
				t.Fatalf("%s: carol: %+v", policy, got)
			}
			expect(t, api.call("/api/v1/mfa/totp/delete", alice, map[string]string{"password": mfaPassword}), 409, "mfa_required")
			unchanged("alice-id", before)
		}
		// A wrong password removes nothing either.
		api.use(config.MFAOff, false)
		expect(t, api.call("/api/v1/mfa/totp/delete", alice, map[string]string{"password": "wrong-password-123"}), 400, "password_invalid")
		unchanged("alice-id", before)
		// Not required, the last factor goes with the recovery codes, and
		// the password alone signs in again.
		w = api.call("/api/v1/mfa/totp/delete", alice, map[string]string{"password": mfaPassword})
		if w.Code != 204 || w.Body.Len() != 0 {
			t.Fatalf("delete: %d %s", w.Code, w.Body.String())
		}
		unchanged("alice-id", empty)
		if got := api.status(alice); got.TOTP != nil || got.RecoveryCodesRemaining != 0 {
			t.Fatalf("status after the removal: %+v", got)
		}
		expect(t, api.call("/api/v1/mfa/totp/delete", alice, map[string]string{"password": mfaPassword}), 404, "not_found")
		expect(t, api.do("GET", "/api/v1/me", alice, ""), 200, "")
		expect(t, api.do("GET", "/api/v1/me", aliceCookie, ""), 200, "")
		expect(t, api.do("GET", "/api/v1/action-items", apiToken, ""), 200, "")
		api.login("alice@example.com", mfaPassword)
		// The codes of before the removal do not come back with a new factor.
		alice, _ = api.login("alice@example.com", mfaPassword)
		_, fresh := api.enroll(alice)
		checkRecoveryCodes(t, fresh)
		ch = api.pending("alice@example.com")
		noSession(t, api.post("/auth/mfa/recovery", map[string]string{"mfa_token": ch.Token, "code": reissued[1]}), 401, "mfa_code_invalid")
		api.session(api.post("/auth/mfa/recovery", map[string]string{"mfa_token": ch.Token, "code": fresh[0]}))
	})
}

// TestMFARolesAndReset checks the policy against role changes, disabling
// and the reset of the CLI.
func TestMFARolesAndReset(t *testing.T) {
	mfaDrivers(t, func(t *testing.T, driver string) {
		ctx := context.Background()
		api := newMFAAPI(t, driver, config.MFAOff)
		alice, aliceCookie := api.login("alice@example.com", mfaPassword)
		carol, _ := api.login("carol@example.com", mfaPassword)
		aliceSecret, _ := api.enroll(alice)
		w := api.do("POST", "/api/v1/api-tokens", alice, `{"name":"cli"}`)
		expect(t, w, 201, "")
		apiToken := decodeJSON[createdAPITokenResponse](t, w).Token
		api.use(config.MFAOwners, false)

		// The user list tells who has a factor: an authenticator app, a
		// WebAuthn credential or nothing.
		if err := api.store.CreateUser(ctx, store.User{ID: "dave-id", Email: "dave@example.com", PasswordHash: "x", CreatedAt: 3, Role: store.RoleMember}); err != nil {
			t.Fatal(err)
		}
		storeKey(t, api.store, "dave-id", "dave-key")
		enabled := func() map[string]bool {
			t.Helper()
			w := api.do("GET", "/api/v1/users", alice, "")
			expect(t, w, 200, "")
			out := map[string]bool{}
			for _, u := range decodeJSON[adminUserListResponse](t, w).Users {
				out[u.ID] = u.MFAEnabled
			}
			return out
		}
		if got := enabled(); !reflect.DeepEqual(got, map[string]bool{"alice-id": true, "carol-id": false, "dave-id": true}) {
			t.Fatalf("mfa_enabled: %v", got)
		}

		// A member promoted to owner is asked to enroll at the next login;
		// the response of the change carries mfa_enabled.
		api.login("carol@example.com", mfaPassword)
		w = api.do("PATCH", "/api/v1/users/carol-id", alice, `{"role":"owner"}`)
		expect(t, w, 200, "")
		if _, keys := jsonKeys(t, w.Body.Bytes()); !reflect.DeepEqual(keys, []string{"created_at", "disabled", "email", "id", "mfa_enabled", "role"}) {
			t.Fatalf("patch keys: %v", keys)
		}
		if got := decodeJSON[adminUserResponse](t, w); got.MFAEnabled || got.Role != store.RoleOwner {
			t.Fatalf("patch: %+v", got)
		}
		if got := decodeJSON[adminUserResponse](t, api.do("PATCH", "/api/v1/users/dave-id", alice, `{"disabled":true}`)); !got.MFAEnabled || !got.Disabled {
			t.Fatalf("patch of dave: %+v", got)
		}
		ch := api.pending("carol@example.com")
		if ch.Enrolled {
			t.Fatalf("promoted member: %+v", ch)
		}
		// The session from before the promotion is kept, and needs no factor.
		expect(t, api.do("GET", "/api/v1/me", carol, ""), 200, "")
		if got := api.status(carol); !got.Required || got.TOTP != nil {
			t.Fatalf("carol: %+v", got)
		}

		// A demoted owner may remove the last factor.
		expect(t, api.call("/api/v1/mfa/totp/delete", alice, map[string]string{"password": mfaPassword}), 409, "mfa_required")
		expect(t, api.do("PATCH", "/api/v1/users/alice-id", carol, `{"role":"member"}`), 200, "")
		if got := api.status(alice); got.Required || got.TOTP == nil {
			t.Fatalf("demoted alice: %+v", got)
		}
		w = api.call("/api/v1/mfa/totp/delete", alice, map[string]string{"password": mfaPassword})
		expect(t, w, 204, "")
		api.login("alice@example.com", mfaPassword)
		if got := decodeJSON[adminUserListResponse](t, api.do("GET", "/api/v1/users", carol, "")).Users; len(got) != 3 || got[0].MFAEnabled || got[1].MFAEnabled || !got[2].MFAEnabled {
			t.Fatalf("users: %+v", got)
		}
		expect(t, api.do("PATCH", "/api/v1/users/alice-id", carol, `{"role":"owner"}`), 200, "")

		// A user disabled and enabled again keeps the factor and is asked
		// for it.
		api.use(config.MFAOff, false)
		alice, aliceCookie = api.login("alice@example.com", mfaPassword)
		aliceSecret, _ = api.enroll(alice)
		w = api.do("POST", "/api/v1/api-tokens", alice, `{"name":"cli"}`)
		expect(t, w, 201, "")
		apiToken = decodeJSON[createdAPITokenResponse](t, w).Token
		expect(t, api.do("PATCH", "/api/v1/users/alice-id", carol, `{"disabled":true}`), 200, "")
		noSession(t, api.public("/auth/login", `{"email":"alice@example.com","password":"`+mfaPassword+`"}`, ""), 401, "unauthorized")
		expect(t, api.do("PATCH", "/api/v1/users/alice-id", carol, `{"disabled":false}`), 200, "")
		if ch = api.pending("alice@example.com"); !ch.Enrolled || !ch.Methods.TOTP || !ch.Methods.Recovery {
			t.Fatalf("enabled again: %+v", ch)
		}
		alice, aliceCookie = api.signIn("alice@example.com", aliceSecret)
		w = api.do("POST", "/api/v1/api-tokens", alice, `{"name":"cli"}`)
		expect(t, w, 201, "")
		apiToken = decodeJSON[createdAPITokenResponse](t, w).Token

		// The reset of the CLI: a required user enrolls again, another
		// signs in with the password; sessions and API tokens are revoked.
		api.use(config.MFAOwners, false)
		if err := api.store.ResetMFA(ctx, "alice@example.com"); err != nil {
			t.Fatal(err)
		}
		expect(t, api.do("GET", "/api/v1/me", aliceCookie, ""), 401, "unauthorized")
		expect(t, api.do("GET", "/api/v1/action-items", apiToken, ""), 401, "unauthorized")
		if ch = api.pending("alice@example.com"); ch.Enrolled {
			t.Fatalf("required user after the reset: %+v", ch)
		}
		noSession(t, api.post("/auth/mfa/totp", map[string]string{"mfa_token": ch.Token, "code": api.code(aliceSecret, 0)}), 401, "mfa_code_invalid")
		api.use(config.MFAOff, false)
		api.login("alice@example.com", mfaPassword)
	})
}

// TestMFARateLimits checks the limits with the real limiters.
func TestMFARateLimits(t *testing.T) {
	api := newMFAAPI(t, "sqlite", config.MFAOff)
	alice, _ := api.login("alice@example.com", mfaPassword)
	carol, _ := api.login("carol@example.com", mfaPassword)
	aliceSecret, aliceCodes := api.enroll(alice)
	carolSecret, _ := api.enroll(carol)
	aliceToken, carolToken := api.pending("alice@example.com").Token, api.pending("carol@example.com").Token
	limited := func(w *httptest.ResponseRecorder, retry string) {
		t.Helper()
		noSession(t, w, 429, "rate_limited")
		if got := w.Header().Get("Retry-After"); got != retry {
			t.Fatalf("Retry-After %q, want %q", got, retry)
		}
	}
	from := func(addr, path string, body map[string]string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest("POST", path, strings.NewReader(string(mustJSON(t, body))))
		r.RemoteAddr = addr
		r.Header.Set("Origin", usersOrigin)
		r.Header.Set("Content-Type", "application/json")
		return serve(t, api.handler, r)
	}

	// Five attempts per user in 15 minutes, on every way to give a code
	// together; the sixth is rejected even with the right code.
	api.use(config.MFAOff, true)
	api.srv.limiter = newKeyLimiter(1000000, time.Minute)
	api.handler = api.srv.handler()
	for i := 0; i < 3; i++ {
		noSession(t, api.post("/auth/mfa/totp", map[string]string{"mfa_token": aliceToken, "code": api.wrongCode(aliceSecret)}), 401, "mfa_code_invalid")
	}
	noSession(t, api.post("/auth/mfa/recovery", map[string]string{"mfa_token": aliceToken, "code": "abcd-efgh-jkmn-pqrs"}), 401, "mfa_code_invalid")
	setup := api.setup(alice)
	expect(t, api.call("/api/v1/mfa/totp/finish", alice, map[string]string{"setup_token": setup.SetupToken, "code": api.wrongCode(setup.Secret)}), 400, "mfa_code_invalid")
	limited(api.post("/auth/mfa/totp", map[string]string{"mfa_token": aliceToken, "code": api.code(aliceSecret, 0)}), "900")
	limited(api.post("/auth/mfa/recovery", map[string]string{"mfa_token": aliceToken, "code": aliceCodes[0]}), "900")
	limited(api.call("/api/v1/mfa/totp/finish", alice, map[string]string{"setup_token": setup.SetupToken, "code": api.code(setup.Secret, 0)}), "900")
	// Nothing was spent or stored by the rejected attempts.
	if status, err := api.store.MFAStatus(context.Background(), "alice-id"); err != nil || status.RecoveryCodes != 10 {
		t.Fatalf("alice: %+v %v", status, err)
	}
	// Another user is not affected, and a successful attempt counts too.
	api.session(api.post("/auth/mfa/totp", map[string]string{"mfa_token": carolToken, "code": api.code(carolSecret, 0)}))
	// The password checks have their own limit: alice still has them.
	api.setup(alice)

	// The enrollment before the session counts in the same limit.
	api.use(config.MFAAll, true)
	api.srv.limiter = newKeyLimiter(1000000, time.Minute)
	api.handler = api.srv.handler()
	if err := api.store.CreateUser(context.Background(), store.User{ID: "dave-id", Email: "dave@example.com", PasswordHash: mustHash(t), CreatedAt: 3, Role: store.RoleMember}); err != nil {
		t.Fatal(err)
	}
	dave := api.pending("dave@example.com").Token
	daveSetup := decodeJSON[totpSetupResponse](t, api.post("/auth/mfa/enroll/totp/begin", map[string]string{"mfa_token": dave}))
	for i := 0; i < 5; i++ {
		noSession(t, from("198.51.100.1:1", "/auth/mfa/enroll/totp/finish", map[string]string{"mfa_token": dave, "setup_token": daveSetup.SetupToken, "code": api.wrongCode(daveSetup.Secret)}), 401, "mfa_code_invalid")
	}
	limited(from("198.51.100.1:1", "/auth/mfa/enroll/totp/finish", map[string]string{"mfa_token": dave, "setup_token": daveSetup.SetupToken, "code": api.code(daveSetup.Secret, 0)}), "900")
	// Beginning again is not an attempt at a code.
	expect(t, from("198.51.100.1:1", "/auth/mfa/enroll/totp/begin", map[string]string{"mfa_token": dave}), 200, "")

	// Ten requests per peer in a minute on /auth/mfa/* together; forwarded
	// headers are not trusted.
	api.use(config.MFAOff, true)
	paths := []string{"/auth/mfa/totp", "/auth/mfa/recovery", "/auth/mfa/enroll/totp/begin", "/auth/mfa/enroll/totp/finish"}
	for i := 0; i < 10; i++ {
		noSession(t, from("203.0.113.9:1000", paths[i%len(paths)], map[string]string{"mfa_token": "x"}), 401, "mfa_token_invalid")
	}
	for _, path := range paths {
		limited(from("203.0.113.9:2000", path, map[string]string{"mfa_token": "x"}), "60")
	}
	r := httptest.NewRequest("POST", paths[0], strings.NewReader(`{"mfa_token":"x"}`))
	r.RemoteAddr = "203.0.113.9:1000"
	r.Header.Set("Origin", usersOrigin)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Forwarded-For", "198.51.100.7")
	limited(serve(t, api.handler, r), "60")
	noSession(t, from("198.51.100.7:1000", paths[0], map[string]string{"mfa_token": "x"}), 401, "mfa_token_invalid")
	// Rejected requests are not counted: the Origin check comes first.
	for i := 0; i < 20; i++ {
		r = httptest.NewRequest("POST", paths[0], strings.NewReader(`{"mfa_token":"x"}`))
		r.RemoteAddr = "198.51.100.8:1000"
		r.Header.Set("Content-Type", "application/json")
		expect(t, serve(t, api.handler, r), 403, "origin_rejected")
	}
	noSession(t, from("198.51.100.8:1000", paths[0], map[string]string{"mfa_token": "x"}), 401, "mfa_token_invalid")
	// The login keeps its own limiter.
	api.pending("alice@example.com")

	// Ten password checks per user in five minutes, successful or not,
	// apart from the limit of the codes.
	api.use(config.MFAOff, true)
	for i := 0; i < 9; i++ {
		expect(t, api.call("/api/v1/mfa/totp/begin", carol, map[string]string{"password": "wrong-password-123"}), 400, "password_invalid")
	}
	api.setup(carol)
	for _, path := range []string{"/api/v1/mfa/totp/begin", "/api/v1/mfa/totp/delete", "/api/v1/mfa/recovery-codes"} {
		w := api.call(path, carol, map[string]string{"password": mfaPassword})
		expect(t, w, 429, "rate_limited")
		if w.Header().Get("Retry-After") != "300" {
			t.Fatalf("Retry-After %q", w.Header().Get("Retry-After"))
		}
	}
	// A password that is not checked is not counted, and alice is not
	// affected.
	for i := 0; i < 20; i++ {
		expect(t, api.call("/api/v1/mfa/totp/begin", alice, map[string]string{"password": strings.Repeat("p", 1025)}), 400, "invalid_request")
	}
	api.setup(alice)
	// carol's codes are not limited by it.
	api.advance(30 * time.Second)
	carolToken = api.pending("carol@example.com").Token
	api.session(api.post("/auth/mfa/totp", map[string]string{"mfa_token": carolToken, "code": api.code(carolSecret, 0)}))
	if status, err := api.store.MFAStatus(context.Background(), "carol-id"); err != nil || status.TOTP == nil || status.RecoveryCodes != 10 {
		t.Fatalf("carol: %+v %v", status, err)
	}
}

func mustHash(t *testing.T) string {
	t.Helper()
	hash, err := auth.Password(mfaPassword)
	if err != nil {
		t.Fatal(err)
	}
	return hash
}

func TestMFAUndecryptableSecret(t *testing.T) {
	api := newMFAAPI(t, "sqlite", config.MFAOff)
	alice, _ := api.login("alice@example.com", mfaPassword)
	secret, codes := api.enroll(alice)
	other := auth.New(api.store)
	other.Now = api.srv.auth.Now
	srv := newServer(config.Config{PublicURL: usersOrigin, Secure: true, Secret: []byte(strings.Repeat("k", 32))}, other, fstest.MapFS{}, nil)
	api.srv, api.handler = srv, srv.handler()
	logs := captureLog(t)
	ch := api.pending("alice@example.com")
	if ch.Methods != (mfaMethodsResponse{TOTP: true, Recovery: true}) {
		t.Fatalf("methods: %+v", ch.Methods)
	}
	noSession(t, api.post("/auth/mfa/totp", map[string]string{"mfa_token": ch.Token, "code": api.code(secret, 0)}), 401, "mfa_code_invalid")
	if got := logs.String(); !strings.Contains(got, "auth: mfa: totp secret of user alice-id cannot be decrypted") || !strings.Contains(got, "auth: mfa: totp failed user=alice-id") {
		t.Fatalf("log: %s", got)
	}
	api.session(api.post("/auth/mfa/recovery", map[string]string{"mfa_token": ch.Token, "code": codes[0]}))
}

// TestMFASessionFailure checks the response when the factor was accepted
// or enrolled but the session could not be started.
func TestMFASessionFailure(t *testing.T) {
	ctx := context.Background()
	api := newMFAAPI(t, "sqlite", config.MFAOff)
	alice, _ := api.login("alice@example.com", mfaPassword)
	secret, codes := api.enroll(alice)
	raw, err := sql.Open("sqlite", api.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	exec := func(q string) {
		t.Helper()
		if _, err := raw.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	logs := captureLog(t)
	ch := api.pending("alice@example.com")
	api.use(config.MFAAll, false)
	carol := api.pending("carol@example.com")
	setup := decodeJSON[totpSetupResponse](t, api.post("/auth/mfa/enroll/totp/begin", map[string]string{"mfa_token": carol.Token}))
	// No session can be stored.
	exec("ALTER TABLE web_sessions RENAME TO web_sessions_away")

	noSession(t, api.post("/auth/mfa/totp", map[string]string{"mfa_token": ch.Token, "code": api.code(secret, 0)}), 503, "signin_unavailable")
	noSession(t, api.post("/auth/mfa/recovery", map[string]string{"mfa_token": ch.Token, "code": codes[0]}), 503, "signin_unavailable")
	w := api.post("/auth/mfa/enroll/totp/finish", map[string]string{"mfa_token": carol.Token, "setup_token": setup.SetupToken, "code": api.code(setup.Secret, 0)})
	noSession(t, w, 503, "signin_unavailable")
	if strings.Contains(w.Body.String(), "recovery_codes") {
		t.Fatalf("recovery codes without a session: %s", w.Body.String())
	}
	// The login itself reports the failure as before.
	api.use(config.MFAOff, false)
	if err = api.store.CreateUser(ctx, store.User{ID: "dave-id", Email: "dave@example.com", PasswordHash: mustHash(t), CreatedAt: 3, Role: store.RoleMember}); err != nil {
		t.Fatal(err)
	}
	noSession(t, api.public("/auth/login", `{"email":"dave@example.com","password":"`+mfaPassword+`"}`, ""), 503, "unavailable")

	exec("ALTER TABLE web_sessions_away RENAME TO web_sessions")
	// The code and the recovery code were spent, and the enrollment stays.
	noSession(t, api.post("/auth/mfa/totp", map[string]string{"mfa_token": ch.Token, "code": api.code(secret, 0)}), 401, "mfa_code_invalid")
	noSession(t, api.post("/auth/mfa/recovery", map[string]string{"mfa_token": ch.Token, "code": codes[0]}), 401, "mfa_code_invalid")
	api.session(api.post("/auth/mfa/recovery", map[string]string{"mfa_token": ch.Token, "code": codes[1]}))
	if status, err := api.store.MFAStatus(ctx, "carol-id"); err != nil || status.TOTP == nil || status.RecoveryCodes != 10 {
		t.Fatalf("carol: %+v %v", status, err)
	}
	api.advance(30 * time.Second)
	api.signIn("carol@example.com", setup.Secret)
	if got := logs.String(); strings.Count(got, "auth: mfa: sign in user=alice-id") != 2 || strings.Count(got, "auth: mfa: sign in user=carol-id") != 1 {
		t.Fatalf("log: %s", got)
	}

	// A failure of the enrollment itself stores nothing and is a plain 503.
	api.use(config.MFAAll, false)
	token := api.pending("dave@example.com").Token
	daveSetup := decodeJSON[totpSetupResponse](t, api.post("/auth/mfa/enroll/totp/begin", map[string]string{"mfa_token": token}))
	exec("CREATE TRIGGER mfa_test_fail BEFORE INSERT ON mfa_recovery_codes BEGIN SELECT RAISE(ABORT, 'test'); END")
	noSession(t, api.post("/auth/mfa/enroll/totp/finish", map[string]string{"mfa_token": token, "setup_token": daveSetup.SetupToken, "code": api.code(daveSetup.Secret, 0)}), 503, "unavailable")
	exec("DROP TRIGGER mfa_test_fail")
	if status, err := api.store.MFAStatus(ctx, "dave-id"); err != nil || status.Enrolled() {
		t.Fatalf("dave: %+v %v", status, err)
	}
	api.session(api.post("/auth/mfa/enroll/totp/finish", map[string]string{"mfa_token": token, "setup_token": daveSetup.SetupToken, "code": api.code(daveSetup.Secret, 0)}))
}

// TestMFALogs checks the lines logged for the failures and the changes, and
// that no secret reaches the log.
func TestMFALogs(t *testing.T) {
	api := newMFAAPI(t, "sqlite", config.MFAOff)
	alice, _ := api.login("alice@example.com", mfaPassword)
	logs := captureLog(t)
	secrets := []string{mfaPassword, "alice@example.com"}

	setup := api.setup(alice)
	wrong := api.wrongCode(setup.Secret)
	expect(t, api.call("/api/v1/mfa/totp/finish", alice, map[string]string{"setup_token": setup.SetupToken, "code": wrong}), 400, "mfa_code_invalid")
	w := api.call("/api/v1/mfa/totp/finish", alice, map[string]string{"setup_token": setup.SetupToken, "code": api.code(setup.Secret, 0)})
	expect(t, w, 200, "")
	codes := decodeJSON[recoveryCodesResponse](t, w).RecoveryCodes
	secrets = append(secrets, setup.Secret, setup.SetupToken, api.code(setup.Secret, 0), api.code(setup.Secret, 1), wrong, alice)
	api.advance(30 * time.Second)

	ch := api.pending("alice@example.com")
	noSession(t, api.post("/auth/mfa/totp", map[string]string{"mfa_token": ch.Token, "code": api.wrongCode(setup.Secret)}), 401, "mfa_code_invalid")
	noSession(t, api.post("/auth/mfa/recovery", map[string]string{"mfa_token": ch.Token, "code": "abcd-efgh-jkmn-pqrs"}), 401, "mfa_code_invalid")
	noSession(t, api.post("/auth/mfa/totp", map[string]string{"mfa_token": ch.Token + "x", "code": api.code(setup.Secret, 0)}), 401, "mfa_token_invalid")
	access, _ := api.session(api.post("/auth/mfa/recovery", map[string]string{"mfa_token": ch.Token, "code": codes[0]}))
	api.session(api.post("/auth/mfa/totp", map[string]string{"mfa_token": ch.Token, "code": api.code(setup.Secret, 0)}))
	expect(t, api.call("/api/v1/mfa/recovery-codes", access, map[string]string{"password": "wrong-password-123"}), 400, "password_invalid")
	w = api.call("/api/v1/mfa/recovery-codes", access, map[string]string{"password": mfaPassword})
	expect(t, w, 200, "")
	reissued := decodeJSON[recoveryCodesResponse](t, w).RecoveryCodes
	expect(t, api.call("/api/v1/mfa/totp/delete", access, map[string]string{"password": mfaPassword}), 204, "")
	secrets = append(secrets, ch.Token, access, api.code(setup.Secret, 0), "abcd-efgh-jkmn-pqrs", "wrong-password-123")
	for _, c := range append(codes, reissued...) {
		secrets = append(secrets, c, strings.ReplaceAll(c, "-", ""), auth.Hash(strings.ReplaceAll(c, "-", "")))
	}

	got := logs.String()
	for line, n := range map[string]int{
		"auth: mfa: totp failed user=alice-id\n":             2,
		"auth: mfa: recovery failed user=alice-id\n":         1,
		"auth: mfa: totp enrolled user=alice-id\n":           1,
		"auth: mfa: recovery codes reissued user=alice-id\n": 1,
		"auth: mfa: totp deleted user=alice-id\n":            1,
	} {
		if strings.Count(got, line) != n {
			t.Errorf("%q: %d times, want %d", strings.TrimSpace(line), strings.Count(got, line), n)
		}
	}
	for _, secret := range secrets {
		if secret != "" && strings.Contains(got, secret) {
			t.Errorf("the log contains a secret of %d bytes", len(secret))
		}
	}
	if t.Failed() {
		t.Logf("%d lines logged", strings.Count(got, "\n"))
	}
}

// TestMFAStoreFailure checks that a failure to read the factors is not
// taken for a user without factors.
func TestMFAStoreFailure(t *testing.T) {
	api := newMFAAPI(t, "sqlite", config.MFAOff)
	alice, _ := api.login("alice@example.com", mfaPassword)
	api.enroll(alice)
	raw, err := sql.Open("sqlite", api.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	if _, err = raw.ExecContext(context.Background(), "ALTER TABLE mfa_totp RENAME TO mfa_totp_away"); err != nil {
		t.Fatal(err)
	}
	captureLog(t)
	noSession(t, api.public("/auth/login", `{"email":"alice@example.com","password":"`+mfaPassword+`"}`, ""), 503, "unavailable")
	expect(t, api.do("GET", "/api/v1/mfa", alice, ""), 503, "unavailable")
	expect(t, api.do("GET", "/api/v1/users", alice, ""), 503, "unavailable")
	expect(t, api.do("PATCH", "/api/v1/users/carol-id", alice, `{"role":"owner"}`), 503, "unavailable")
	if role, _ := userState(t, api.store, "carol@example.com"); role != store.RoleMember {
		t.Fatal("the role changed")
	}
}
