package httpserver

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/i2bskn/astovia/internal/config"
	"github.com/i2bskn/astovia/internal/store"
)

// softKey is a software authenticator: it holds an ES256 key and builds the
// responses a browser returns from navigator.credentials.create (with the
// attestation format "none") and navigator.credentials.get, so that the
// tests pass the real verification of the WebAuthn library.
type softKey struct {
	t   *testing.T
	key *ecdsa.PrivateKey
	id  []byte
	// counter is the signature counter of the last response. A counterless
	// key always reports zero, as most passkeys do.
	counter     uint32
	counterless bool
	// origin is the origin the browser reports. rpID replaces the relying
	// party ID of the options when it is set.
	origin string
	rpID   string
	// handle is returned as the user handle of an assertion when it is set.
	handle []byte
	// extensions are returned as the client extension results when set.
	extensions map[string]any
}

// extensionResults returns the client extension results of a response.
func (k *softKey) extensionResults() map[string]any {
	if k.extensions != nil {
		return k.extensions
	}
	return map[string]any{}
}

func newSoftKey(t *testing.T) *softKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	id := make([]byte, 16)
	if _, err = rand.Read(id); err != nil {
		t.Fatal(err)
	}
	return &softKey{t: t, key: key, id: id, origin: usersOrigin}
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func cborBytes(b []byte) []byte {
	switch n := len(b); {
	case n < 24:
		return append([]byte{0x40 | byte(n)}, b...)
	case n < 256:
		return append([]byte{0x58, byte(n)}, b...)
	default:
		return append([]byte{0x59, byte(n >> 8), byte(n)}, b...)
	}
}

// cborText encodes a text string shorter than 24 bytes.
func cborText(s string) []byte { return append([]byte{0x60 | byte(len(s))}, s...) }

// coseKey returns the public key as a COSE EC2 key for ES256:
// {1: 2, 3: -7, -1: 1, -2: x, -3: y}.
func (k *softKey) coseKey() []byte {
	k.t.Helper()
	public, err := k.key.PublicKey.ECDH()
	if err != nil {
		k.t.Fatal(err)
	}
	point := public.Bytes() // 0x04 || X || Y
	out := []byte{0xa5, 0x01, 0x02, 0x03, 0x26, 0x20, 0x01, 0x21}
	out = append(out, cborBytes(point[1:33])...)
	out = append(out, 0x22)
	return append(out, cborBytes(point[33:65])...)
}

// softOptions are the members of the creation and request options the key
// reads. The challenge goes into the client data as it was received.
type softOptions struct {
	Challenge string `json:"challenge"`
	RP        struct {
		ID string `json:"id"`
	} `json:"rp"`
	RPID string `json:"rpId"`
}

func (k *softKey) options(raw json.RawMessage) softOptions {
	k.t.Helper()
	var o softOptions
	if err := json.Unmarshal(raw, &o); err != nil {
		k.t.Fatal(err)
	}
	if o.RPID == "" {
		o.RPID = o.RP.ID
	}
	if k.rpID != "" {
		o.RPID = k.rpID
	}
	if o.Challenge == "" || o.RPID == "" {
		k.t.Fatalf("options: %s", raw)
	}
	return o
}

func (k *softKey) clientData(ceremony, challenge string) []byte {
	return mustJSON(k.t, map[string]any{"type": ceremony, "challenge": challenge, "origin": k.origin, "crossOrigin": false})
}

// authData returns the authenticator data: the hash of the relying party
// ID, the flags (user present and verified), the counter and, for a new
// credential, the attested credential data.
func (k *softKey) authData(rpID string, attested bool) []byte {
	hash := sha256.Sum256([]byte(rpID))
	out := append([]byte{}, hash[:]...)
	flags := byte(0x01 | 0x04)
	if attested {
		flags |= 0x40
	}
	out = append(out, flags)
	out = binary.BigEndian.AppendUint32(out, k.counter)
	if attested {
		out = append(out, make([]byte, 16)...)
		out = binary.BigEndian.AppendUint16(out, uint16(len(k.id)))
		out = append(out, k.id...)
		out = append(out, k.coseKey()...)
	}
	return out
}

// create returns the registration response to the creation options.
func (k *softKey) create(options json.RawMessage) json.RawMessage {
	k.t.Helper()
	o := k.options(options)
	// {"fmt": "none", "attStmt": {}, "authData": bytes}
	attestation := append([]byte{0xa3}, cborText("fmt")...)
	attestation = append(attestation, cborText("none")...)
	attestation = append(attestation, cborText("attStmt")...)
	attestation = append(attestation, 0xa0)
	attestation = append(attestation, cborText("authData")...)
	attestation = append(attestation, cborBytes(k.authData(o.RPID, true))...)
	return mustJSON(k.t, map[string]any{
		"id": b64(k.id), "rawId": b64(k.id), "type": "public-key",
		"authenticatorAttachment": "cross-platform",
		"clientExtensionResults":  k.extensionResults(),
		"response": map[string]any{
			"clientDataJSON":    b64(k.clientData("webauthn.create", o.Challenge)),
			"attestationObject": b64(attestation),
			"transports":        []string{"usb"},
		},
	})
}

// get returns the authentication response to the request options, signed
// with the key over the authenticator data and the hash of the client data.
func (k *softKey) get(options json.RawMessage) json.RawMessage {
	k.t.Helper()
	o := k.options(options)
	if !k.counterless {
		k.counter++
	}
	authData, clientData := k.authData(o.RPID, false), k.clientData("webauthn.get", o.Challenge)
	sum := sha256.Sum256(clientData)
	digest := sha256.Sum256(append(append([]byte{}, authData...), sum[:]...))
	signature, err := ecdsa.SignASN1(rand.Reader, k.key, digest[:])
	if err != nil {
		k.t.Fatal(err)
	}
	response := map[string]any{"clientDataJSON": b64(clientData), "authenticatorData": b64(authData), "signature": b64(signature)}
	if k.handle != nil {
		response["userHandle"] = b64(k.handle)
	}
	return mustJSON(k.t, map[string]any{
		"id": b64(k.id), "rawId": b64(k.id), "type": "public-key",
		"clientExtensionResults": k.extensionResults(),
		"response":               response,
	})
}

// webauthnBegin is the response of the begin requests and the handle
// cookie it set, as "name=value" for a Cookie header.
type webauthnBegin struct {
	Options json.RawMessage `json:"options"`
	Handle  string          `json:"-"`
}

// useURL serves the same database and sessions at publicURL, without the
// rate limits.
func (a *mfaAPI) useURL(policy, publicURL string) {
	srv := newServer(config.Config{PublicURL: publicURL, Secure: true, MFARequired: policy}, a.srv.auth, fstest.MapFS{}, nil)
	for _, l := range []**loginLimiter{&srv.limiter, &srv.accountLimiter, &srv.mfaPeerLimiter, &srv.mfaLimiter, &srv.mfaPasswordLimiter} {
		*l = newKeyLimiter(1000000, time.Minute)
	}
	a.srv, a.handler = srv, srv.handler()
}

// send posts the JSON object body with the session cookie (none when
// session is empty) and the handle cookie (none when handle is empty)
// from the origin of the server.
func (a *mfaAPI) send(path, session, handle string, body map[string]any) *httptest.ResponseRecorder {
	a.t.Helper()
	r := httptest.NewRequest("POST", path, strings.NewReader(string(mustJSON(a.t, body))))
	r.Header.Set("Origin", a.srv.cfg.PublicURL)
	r.Header.Set("Content-Type", "application/json")
	authorize(r, session)
	if handle != "" {
		r.Header.Add("Cookie", handle)
	}
	return serve(a.t, a.handler, r)
}

// handleCleared checks that w deletes the handle cookie.
func handleCleared(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	for _, c := range w.Result().Cookies() {
		if c.Name == webauthnCookie && c.Value == "" && c.MaxAge < 0 {
			return
		}
	}
	t.Fatalf("handle cookie not cleared: %v", w.Header().Values("Set-Cookie"))
}

// begun checks the response of a begin request: the options alone in the
// body, and the handle in a cookie that scripts cannot read, sent only by
// this site, for the five minutes of the ceremony.
func (a *mfaAPI) begun(w *httptest.ResponseRecorder) webauthnBegin {
	a.t.Helper()
	expect(a.t, w, 200, "")
	if _, keys := jsonKeys(a.t, w.Body.Bytes()); !reflect.DeepEqual(keys, []string{"options"}) {
		a.t.Fatalf("begin keys: %v", keys)
	}
	out := decodeJSON[webauthnBegin](a.t, w)
	var o softOptions
	if err := json.Unmarshal(out.Options, &o); err != nil || len(o.Challenge) != 43 {
		a.t.Fatalf("begin response: %s", w.Body.String())
	}
	for _, c := range w.Result().Cookies() {
		if c.Name != webauthnCookie {
			continue
		}
		if c.Value == "" || len(c.Value) > maxWebAuthnHandleLength || strings.Contains(c.Value, o.Challenge) || !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteStrictMode || c.Path != "/" || c.Domain != "" || c.MaxAge != 300 {
			a.t.Fatalf("handle cookie: %+v", c)
		}
		out.Handle = c.Name + "=" + c.Value
	}
	if out.Handle == "" {
		a.t.Fatalf("no handle cookie: %v", w.Header().Values("Set-Cookie"))
	}
	return out
}

// beginKey starts the registration of a key from the account screen.
func (a *mfaAPI) beginKey(session string) webauthnBegin {
	a.t.Helper()
	return a.begun(a.send("/api/v1/mfa/webauthn/begin", session, "", map[string]any{"password": mfaPassword}))
}

func (a *mfaAPI) finishKey(session, handle string, credential json.RawMessage, name string) *httptest.ResponseRecorder {
	a.t.Helper()
	w := a.send("/api/v1/mfa/webauthn/finish", session, handle, map[string]any{"credential": credential, "name": name})
	if w.Code != 401 {
		handleCleared(a.t, w)
	}
	return w
}

// addKey registers k for session from the account screen and returns the
// credential and the recovery codes (nil when none were issued).
func (a *mfaAPI) addKey(session string, k *softKey, name string) (mfaCredentialResponse, []string) {
	a.t.Helper()
	begin := a.beginKey(session)
	w := a.finishKey(session, begin.Handle, k.create(begin.Options), name)
	expect(a.t, w, 201, "")
	body, keys := jsonKeys(a.t, w.Body.Bytes())
	if !reflect.DeepEqual(keys, []string{"credential", "recovery_codes"}) {
		a.t.Fatalf("finish keys: %v", keys)
	}
	if _, keys = jsonKeys(a.t, body["credential"]); !reflect.DeepEqual(keys, []string{"created_at", "id", "last_used_at", "name"}) {
		a.t.Fatalf("credential keys: %v", keys)
	}
	out := decodeJSON[webauthnCreatedResponse](a.t, w)
	return out.Credential, out.RecoveryCodes
}

// beginSignIn starts the sign-in with a key for an MFA token.
func (a *mfaAPI) beginSignIn(token string) webauthnBegin {
	a.t.Helper()
	return a.begun(a.send("/auth/mfa/webauthn/begin", "", "", map[string]any{"mfa_token": token}))
}

func (a *mfaAPI) finishSignIn(token, handle string, credential json.RawMessage) *httptest.ResponseRecorder {
	a.t.Helper()
	return a.send("/auth/mfa/webauthn/finish", "", handle, map[string]any{"mfa_token": token, "credential": credential})
}

// keySignIn signs in with the password and k.
func (a *mfaAPI) keySignIn(email string, k *softKey) string {
	a.t.Helper()
	ch := a.pending(email)
	begin := a.beginSignIn(ch.Token)
	cookie, _ := a.session(a.finishSignIn(ch.Token, begin.Handle, k.get(begin.Options)))
	return cookie
}

// beginEnroll and finishEnroll register a key before the first session.
func (a *mfaAPI) beginEnroll(token string) webauthnBegin {
	a.t.Helper()
	return a.begun(a.send("/auth/mfa/enroll/webauthn/begin", "", "", map[string]any{"mfa_token": token}))
}

func (a *mfaAPI) finishEnroll(token, handle string, credential json.RawMessage, name string) *httptest.ResponseRecorder {
	a.t.Helper()
	return a.send("/auth/mfa/enroll/webauthn/finish", "", handle, map[string]any{"mfa_token": token, "credential": credential, "name": name})
}

// keys returns the IDs of the WebAuthn credentials of userID.
func (a *mfaAPI) keys(userID string) []string {
	a.t.Helper()
	status, err := a.store.MFAStatus(context.Background(), userID)
	if err != nil {
		a.t.Fatal(err)
	}
	out := []string{}
	for _, c := range status.WebAuthn {
		out = append(out, c.ID)
	}
	return out
}

// TestWebAuthnRoundTrip registers keys from the account screen and signs in
// with them, through the verification of authlume.
func TestWebAuthnRoundTrip(t *testing.T) {
	mfaDrivers(t, func(t *testing.T, driver string) {
		api := newMFAAPI(t, driver, config.MFAOff)
		alice, _ := api.login("alice@example.com", mfaPassword)
		if got := api.status(alice); !got.WebAuthnAvailable || len(got.WebAuthn) != 0 {
			t.Fatalf("status: %+v", got)
		}

		// The first key is the first factor: it comes with recovery codes.
		key := newSoftKey(t)
		begin := api.beginKey(alice)
		var o struct {
			RP struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"rp"`
			User struct {
				Name string `json:"name"`
			} `json:"user"`
		}
		if err := json.Unmarshal(begin.Options, &o); err != nil || o.RP.ID != "astovia.test" || o.User.Name != "alice@example.com" {
			t.Fatalf("creation options: %s", begin.Options)
		}
		w := api.finishKey(alice, begin.Handle, key.create(begin.Options), "  YubiKey 5  ")
		expect(t, w, 201, "")
		created := decodeJSON[webauthnCreatedResponse](t, w)
		checkRecoveryCodes(t, created.RecoveryCodes)
		if created.Credential.ID == "" || created.Credential.Name != "YubiKey 5" || created.Credential.CreatedAt != formatTime(api.now.Unix()) || created.Credential.LastUsedAt != nil {
			t.Fatalf("created: %+v", created.Credential)
		}
		// A second key issues no codes.
		api.advance(time.Second)
		passkey := newSoftKey(t)
		passkey.counterless = true
		second, codes := api.addKey(alice, passkey, "Passkey")
		if codes != nil {
			t.Fatalf("codes for the second key: %v", codes)
		}
		if got := api.status(alice); len(got.WebAuthn) != 2 || got.WebAuthn[0] != created.Credential || got.WebAuthn[1] != second || got.RecoveryCodesRemaining != 10 {
			t.Fatalf("status: %+v", got)
		}

		// The password starts no session; either key signs in.
		ch := api.pending("alice@example.com")
		if !ch.Enrolled || ch.Methods != (mfaMethodsResponse{WebAuthn: true, Recovery: true}) {
			t.Fatalf("challenge: %+v", ch.Methods)
		}
		signIn := api.beginSignIn(ch.Token)
		var request struct {
			RPID             string `json:"rpId"`
			AllowCredentials []struct {
				ID string `json:"id"`
			} `json:"allowCredentials"`
		}
		if err := json.Unmarshal(signIn.Options, &request); err != nil || request.RPID != "astovia.test" || len(request.AllowCredentials) != 2 {
			t.Fatalf("request options: %s", signIn.Options)
		}
		w = api.finishSignIn(ch.Token, signIn.Handle, key.get(signIn.Options))
		handleCleared(t, w)
		cookie, _ := api.session(w)
		if me := decodeJSON[userResponse](t, api.do("GET", "/api/v1/me", cookie, "")); me.ID != "alice-id" {
			t.Fatalf("me: %+v", me)
		}
		api.keySignIn("alice@example.com", passkey)
		api.keySignIn("alice@example.com", key)
		if got := api.status(alice); got.WebAuthn[0].LastUsedAt == nil || got.WebAuthn[1].LastUsedAt == nil {
			t.Fatalf("last used: %+v", got.WebAuthn)
		}

		// Removing a key needs the password.
		remove := func(id, password string) *httptest.ResponseRecorder {
			return api.call("/api/v1/mfa/webauthn/"+id+"/delete", alice, map[string]string{"password": password})
		}
		expect(t, remove(second.ID, "wrong-password-123"), 400, "password_invalid")
		expect(t, remove(second.ID, mfaPassword), 204, "")
		expect(t, remove(second.ID, mfaPassword), 404, "not_found")
		ch = api.pending("alice@example.com")
		begin = api.beginSignIn(ch.Token)
		noSession(t, api.finishSignIn(ch.Token, begin.Handle, passkey.get(begin.Options)), 401, "mfa_code_invalid")
	})
}

// TestWebAuthnHandle checks the handle cookie of a ceremony: it is needed,
// spent by the first finish request whatever its outcome, and bound to the
// user and to the kind of ceremony.
func TestWebAuthnHandle(t *testing.T) {
	mfaDrivers(t, func(t *testing.T, driver string) {
		api := newMFAAPI(t, driver, config.MFAOff)
		alice, _ := api.login("alice@example.com", mfaPassword)
		carol, _ := api.login("carol@example.com", mfaPassword)
		key, carolKey := newSoftKey(t), newSoftKey(t)
		api.addKey(alice, key, "Key")
		api.addKey(carol, carolKey, "Key")
		ch, carolCh := api.pending("alice@example.com"), api.pending("carol@example.com")

		// No handle, or one too long to be read, is a lost ceremony.
		begin := api.beginSignIn(ch.Token)
		noSession(t, api.finishSignIn(ch.Token, "", key.get(begin.Options)), 400, "mfa_state_invalid")
		noSession(t, api.finishSignIn(ch.Token, webauthnCookie+"="+strings.Repeat("A", maxWebAuthnHandleLength+1), key.get(begin.Options)), 400, "mfa_state_invalid")
		// An unknown handle verifies nothing.
		w := api.finishSignIn(ch.Token, webauthnCookie+"=AAAA", key.get(begin.Options))
		noSession(t, w, 401, "mfa_code_invalid")
		handleCleared(t, w)

		// A failed finish spends the handle: the right response after it
		// fails as well.
		begin = api.beginSignIn(ch.Token)
		noSession(t, api.finishSignIn(ch.Token, begin.Handle, newSoftKey(t).get(begin.Options)), 401, "mfa_code_invalid")
		noSession(t, api.finishSignIn(ch.Token, begin.Handle, key.get(begin.Options)), 401, "mfa_code_invalid")
		// So does a successful one: the same response signs in once.
		begin = api.beginSignIn(ch.Token)
		response := key.get(begin.Options)
		api.session(api.finishSignIn(ch.Token, begin.Handle, response))
		noSession(t, api.finishSignIn(ch.Token, begin.Handle, response), 401, "mfa_code_invalid")
		later := api.beginSignIn(ch.Token)
		noSession(t, api.finishSignIn(ch.Token, later.Handle, response), 401, "mfa_code_invalid")

		// Carol's ceremony does not serve alice, nor the other way round.
		carolBegin := api.beginSignIn(carolCh.Token)
		noSession(t, api.finishSignIn(ch.Token, carolBegin.Handle, carolKey.get(carolBegin.Options)), 401, "mfa_code_invalid")
		begin = api.beginSignIn(ch.Token)
		noSession(t, api.finishSignIn(carolCh.Token, begin.Handle, key.get(begin.Options)), 401, "mfa_code_invalid")
		// A registration is not a sign-in.
		registration := api.beginKey(alice)
		noSession(t, api.finishSignIn(ch.Token, registration.Handle, key.get(registration.Options)), 401, "mfa_code_invalid")
		begin = api.beginSignIn(ch.Token)
		expect(t, api.finishKey(alice, begin.Handle, newSoftKey(t).create(begin.Options), "Key"), 400, "mfa_code_invalid")
		// The handle of another session's registration does not serve.
		aliceAgain := api.keySignIn("alice@example.com", key)
		registration = api.beginKey(aliceAgain)
		expect(t, api.finishKey(carol, registration.Handle, newSoftKey(t).create(registration.Options), "Key"), 400, "mfa_code_invalid")

		// The five minutes of a ceremony are kept by authlume on its own
		// clock; the store's side is tested in internal/store.
		if got := api.keys("alice-id"); len(got) != 1 {
			t.Fatalf("alice's keys: %v", got)
		}
	})
}

// TestWebAuthnEnrollBeforeSession registers the first key of a user that
// must have a factor, before any session.
func TestWebAuthnEnrollBeforeSession(t *testing.T) {
	mfaDrivers(t, func(t *testing.T, driver string) {
		api := newMFAAPI(t, driver, config.MFAAll)
		ch := api.pending("carol@example.com")
		if ch.Enrolled {
			t.Fatalf("challenge: %+v", ch)
		}
		key := newSoftKey(t)
		started := api.beginEnroll(ch.Token)
		noSession(t, api.finishEnroll(ch.Token, started.Handle, key.create(started.Options), ""), 400, "invalid_request")
		noSession(t, api.finishEnroll(ch.Token, "", key.create(started.Options), "Key"), 400, "mfa_state_invalid")
		w := api.finishEnroll(ch.Token, started.Handle, key.create(started.Options), "Key")
		if _, keys := jsonKeys(t, w.Body.Bytes()); !reflect.DeepEqual(keys, []string{"recovery_codes", "user"}) {
			t.Fatalf("finish keys: %v %s", keys, w.Body.String())
		}
		handleCleared(t, w)
		carol, _ := api.session(w)
		checkRecoveryCodes(t, decodeJSON[recoveryCodesResponse](t, w).RecoveryCodes)
		if got := api.status(carol); len(got.WebAuthn) != 1 || got.WebAuthn[0].Name != "Key" || got.RecoveryCodesRemaining != 10 {
			t.Fatalf("status: %+v", got)
		}
		// The key signs in; the enrollment is closed now that there is one.
		api.keySignIn("carol@example.com", key)
		ch = api.pending("carol@example.com")
		noSession(t, api.send("/auth/mfa/enroll/webauthn/begin", "", "", map[string]any{"mfa_token": ch.Token}), 409, "mfa_already_enrolled")

		// Of two enrollments begun together, one wins.
		if err := api.store.CreateUser(context.Background(), store.User{ID: "dave-id", Email: "dave@example.com", PasswordHash: mustHash(t), CreatedAt: 3, Role: store.RoleMember}); err != nil {
			t.Fatal(err)
		}
		dave := api.pending("dave@example.com")
		first, second := api.beginEnroll(dave.Token), api.beginEnroll(dave.Token)
		api.session(api.finishEnroll(dave.Token, first.Handle, newSoftKey(t).create(first.Options), "First"))
		noSession(t, api.finishEnroll(dave.Token, second.Handle, newSoftKey(t).create(second.Options), "Second"), 409, "mfa_already_enrolled")
		if got := api.keys("dave-id"); len(got) != 1 {
			t.Fatalf("dave's keys: %v", got)
		}
	})
}

// TestWebAuthnRevokedCeremony checks that a ceremony begun before the
// account was recovered enrolls nothing afterwards: reset-mfa and the
// revocation of the account (a password reset, disabling) remove the
// ceremonies, and the sessions and passwords they were begun with.
func TestWebAuthnRevokedCeremony(t *testing.T) {
	mfaDrivers(t, func(t *testing.T, driver string) {
		ctx := context.Background()
		api := newMFAAPI(t, driver, config.MFAAll)
		key := newSoftKey(t)

		// Before the first session: reset-mfa keeps the password, so the
		// MFA token still opens, but the ceremony is gone.
		ch := api.pending("carol@example.com")
		started := api.beginEnroll(ch.Token)
		if err := api.store.ResetMFA(ctx, "carol@example.com"); err != nil {
			t.Fatal(err)
		}
		noSession(t, api.finishEnroll(ch.Token, started.Handle, key.create(started.Options), "Key"), 401, "mfa_code_invalid")
		// A password reset ends the MFA token as well.
		ch = api.pending("carol@example.com")
		started = api.beginEnroll(ch.Token)
		if err := api.store.ChangeUser(ctx, "carol@example.com", mustHash(t), false, api.now.Unix()); err != nil {
			t.Fatal(err)
		}
		noSession(t, api.finishEnroll(ch.Token, started.Handle, key.create(started.Options), "Key"), 401, "mfa_token_invalid")
		if got := api.keys("carol-id"); len(got) != 0 {
			t.Fatalf("carol's keys: %v", got)
		}

		// From the account screen: the session that began the ceremony is
		// revoked by both.
		api.useURL(config.MFAOff, usersOrigin)
		for _, revoke := range []struct {
			name string
			fn   func() error
		}{
			{"reset-mfa", func() error { return api.store.ResetMFA(ctx, "alice@example.com") }},
			{"password reset", func() error {
				return api.store.ChangeUser(ctx, "alice@example.com", mustHash(t), false, api.now.Unix())
			}},
			{"disable", func() error { return api.store.ChangeUser(ctx, "alice@example.com", "", true, api.now.Unix()) }},
		} {
			alice, _ := api.login("alice@example.com", mfaPassword)
			begin := api.beginKey(alice)
			if err := revoke.fn(); err != nil {
				t.Fatal(err)
			}
			expect(t, api.finishKey(alice, begin.Handle, newSoftKey(t).create(begin.Options), "Key"), 401, "unauthorized")
			if got := api.keys("alice-id"); len(got) != 0 {
				t.Fatalf("%s: alice's keys: %v", revoke.name, got)
			}
			if err := api.store.EnableUser(ctx, "alice@example.com"); err != nil {
				t.Fatal(err)
			}
		}
		// The session of another browser of alice is revoked as well.
		alice, _ := api.login("alice@example.com", mfaPassword)
		begin := api.beginKey(alice)
		other, _ := api.login("alice@example.com", mfaPassword)
		if err := api.store.ResetMFA(ctx, "alice@example.com"); err != nil {
			t.Fatal(err)
		}
		expect(t, api.finishKey(other, begin.Handle, newSoftKey(t).create(begin.Options), "Key"), 401, "unauthorized")
		if got := api.keys("alice-id"); len(got) != 0 {
			t.Fatalf("alice's keys: %v", got)
		}
	})
}

// TestWebAuthnEnrollConditions checks the conditions of the registration
// from the account screen.
func TestWebAuthnEnrollConditions(t *testing.T) {
	mfaDrivers(t, func(t *testing.T, driver string) {
		api := newMFAAPI(t, driver, config.MFAOff)
		alice, _ := api.login("alice@example.com", mfaPassword)
		carol, _ := api.login("carol@example.com", mfaPassword)

		// The password is checked at the begin request.
		expect(t, api.call("/api/v1/mfa/webauthn/begin", alice, map[string]string{"password": "wrong-password-123"}), 400, "password_invalid")
		// The name: 1-64 characters, trimmed, without control characters.
		key := newSoftKey(t)
		begin := api.beginKey(alice)
		for _, name := range []string{"", " ", strings.Repeat("n", 65), "a\x00b"} {
			expect(t, api.send("/api/v1/mfa/webauthn/finish", alice, begin.Handle, map[string]any{"credential": key.create(begin.Options), "name": name}), 400, "invalid_request")
		}
		expect(t, api.send("/api/v1/mfa/webauthn/finish", alice, begin.Handle, map[string]any{"name": "Key"}), 400, "invalid_request")
		expect(t, api.finishKey(alice, "", key.create(begin.Options), "Key"), 400, "mfa_state_invalid")
		long := strings.Repeat("é", 64)
		w := api.finishKey(alice, begin.Handle, key.create(begin.Options), " "+long+"\t")
		expect(t, w, 201, "")
		if got := decodeJSON[webauthnCreatedResponse](t, w).Credential.Name; got != long {
			t.Fatalf("name: %q", got)
		}

		// The same key is not registered twice, for alice or for carol.
		begin = api.beginKey(alice)
		expect(t, api.finishKey(alice, begin.Handle, key.create(begin.Options), "Again"), 409, "mfa_credential_exists")
		begin = api.beginKey(carol)
		expect(t, api.finishKey(carol, begin.Handle, key.create(begin.Options), "Stolen"), 409, "mfa_credential_exists")

		// Ten keys at most: the begin request refuses the eleventh, and so
		// does the store when the limit is reached meanwhile.
		for i := 0; i < store.MaxWebAuthnCredentials-2; i++ {
			storeKey(t, api.store, "alice-id", "filler-"+string(rune('a'+i)))
		}
		begin = api.beginKey(alice)
		storeKey(t, api.store, "alice-id", "tenth")
		expect(t, api.finishKey(alice, begin.Handle, newSoftKey(t).create(begin.Options), "Eleventh"), 409, "mfa_limit")
		expect(t, api.call("/api/v1/mfa/webauthn/begin", alice, map[string]string{"password": mfaPassword}), 409, "mfa_limit")
	})
}

// TestWebAuthnAvailability checks a server whose address cannot use
// passkeys: http, where WebAuthn is off and a key can still be removed.
func TestWebAuthnAvailability(t *testing.T) {
	api := newMFAAPI(t, "sqlite", config.MFAOff)
	alice, _ := api.login("alice@example.com", mfaPassword)
	created, _ := api.addKey(alice, newSoftKey(t), "Key")
	api.useURL(config.MFAOff, "http://localhost:8080")
	if got := api.status(alice); got.WebAuthnAvailable || len(got.WebAuthn) != 1 {
		t.Fatalf("status: %+v", got)
	}
	ch := api.challenge(api.send("/auth/login", "", "", map[string]any{"email": "alice@example.com", "password": mfaPassword}))
	if ch.WebAuthnAvailable || ch.Methods.WebAuthn {
		t.Fatalf("challenge: %+v", ch)
	}
	noSession(t, api.send("/auth/mfa/webauthn/begin", "", "", map[string]any{"mfa_token": ch.Token}), 400, "webauthn_unavailable")
	noSession(t, api.send("/auth/mfa/webauthn/finish", "", "", map[string]any{"mfa_token": ch.Token, "credential": map[string]any{}}), 400, "webauthn_unavailable")
	expect(t, api.send("/api/v1/mfa/webauthn/begin", alice, "", map[string]any{"password": mfaPassword}), 400, "webauthn_unavailable")
	expect(t, api.send("/api/v1/mfa/webauthn/"+created.ID+"/delete", alice, "", map[string]any{"password": mfaPassword}), 204, "")
}
