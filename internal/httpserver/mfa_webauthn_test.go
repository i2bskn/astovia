package httpserver

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"
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

// webauthnBegin is the response of the begin requests.
type webauthnBegin struct {
	Options json.RawMessage `json:"options"`
	State   string          `json:"state"`
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

// send posts the JSON object body to a public /auth endpoint from the
// origin of the server.
func (a *mfaAPI) send(path string, body map[string]any) *httptest.ResponseRecorder {
	a.t.Helper()
	r := httptest.NewRequest("POST", path, strings.NewReader(string(mustJSON(a.t, body))))
	r.Header.Set("Origin", a.srv.cfg.PublicURL)
	r.Header.Set("Content-Type", "application/json")
	return serve(a.t, a.handler, r)
}

// ask sends the JSON object body to an /api/v1 endpoint with bearer.
func (a *mfaAPI) ask(path, bearer string, body map[string]any) *httptest.ResponseRecorder {
	a.t.Helper()
	return a.do("POST", path, bearer, string(mustJSON(a.t, body)))
}

func (a *mfaAPI) begun(w *httptest.ResponseRecorder) webauthnBegin {
	a.t.Helper()
	expect(a.t, w, 200, "")
	if _, keys := jsonKeys(a.t, w.Body.Bytes()); !reflect.DeepEqual(keys, []string{"options", "state"}) {
		a.t.Fatalf("begin keys: %v", keys)
	}
	out := decodeJSON[webauthnBegin](a.t, w)
	var o softOptions
	if err := json.Unmarshal(out.Options, &o); err != nil || len(o.Challenge) != 43 || out.State == "" || strings.Contains(out.State, o.Challenge) {
		a.t.Fatalf("begin response: %s", w.Body.String())
	}
	return out
}

// beginKey starts the registration of a key from the account screen.
func (a *mfaAPI) beginKey(access string) webauthnBegin {
	a.t.Helper()
	return a.begun(a.call("/api/v1/mfa/webauthn/begin", access, map[string]string{"password": mfaPassword}))
}

func (a *mfaAPI) finishKey(access, state string, credential json.RawMessage, name string) *httptest.ResponseRecorder {
	a.t.Helper()
	return a.ask("/api/v1/mfa/webauthn/finish", access, map[string]any{"state": state, "credential": credential, "name": name})
}

// addKey registers k for the session access from the account screen and
// returns the credential and the recovery codes (nil when none were
// issued).
func (a *mfaAPI) addKey(access string, k *softKey, name string) (mfaCredentialResponse, []string) {
	a.t.Helper()
	begin := a.beginKey(access)
	w := a.finishKey(access, begin.State, k.create(begin.Options), name)
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
	return a.begun(a.send("/auth/mfa/webauthn/begin", map[string]any{"mfa_token": token}))
}

func (a *mfaAPI) finishSignIn(token, state string, credential json.RawMessage) *httptest.ResponseRecorder {
	a.t.Helper()
	return a.send("/auth/mfa/webauthn/finish", map[string]any{"mfa_token": token, "state": state, "credential": credential})
}

// keySignIn signs in with the password and k.
func (a *mfaAPI) keySignIn(email string, k *softKey) (string, string) {
	a.t.Helper()
	ch := a.pending(email)
	begin := a.beginSignIn(ch.Token)
	return a.session(a.finishSignIn(ch.Token, begin.State, k.get(begin.Options)))
}

// storedKey returns the stored row of a credential and the credential of
// the library in it.
func (a *mfaAPI) storedKey(userID, id string) (store.WebAuthnCredential, webauthn.Credential) {
	a.t.Helper()
	status, err := a.store.MFAStatus(context.Background(), userID)
	if err != nil {
		a.t.Fatal(err)
	}
	for _, row := range status.WebAuthn {
		if row.ID == id {
			var c webauthn.Credential
			if err = json.Unmarshal([]byte(row.Credential), &c); err != nil {
				a.t.Fatal(err)
			}
			return row, c
		}
	}
	a.t.Fatalf("credential %s of %s is not stored", id, userID)
	return store.WebAuthnCredential{}, webauthn.Credential{}
}

// TestWebAuthnRoundTrip registers keys from the account screen and signs in
// with them, through the verification of the library.
func TestWebAuthnRoundTrip(t *testing.T) {
	mfaDrivers(t, func(t *testing.T, driver string) {
		api := newMFAAPI(t, driver, config.MFAOff)
		alice, aliceCookie := api.login("alice@example.com", mfaPassword)
		w := api.do("POST", "/api/v1/api-tokens", alice, `{"name":"cli"}`)
		expect(t, w, 201, "")
		apiToken := decodeJSON[createdAPITokenResponse](t, w).Token
		if got := api.status(alice); !got.WebAuthnAvailable || len(got.WebAuthn) != 0 {
			t.Fatalf("status: %+v", got)
		}

		// The options to create a credential, in the JSON form of the
		// specification.
		begin := api.beginKey(alice)
		var creation struct {
			RP struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"rp"`
			User struct {
				ID          string `json:"id"`
				Name        string `json:"name"`
				DisplayName string `json:"displayName"`
			} `json:"user"`
			PubKeyCredParams []struct {
				Type string `json:"type"`
				Alg  int    `json:"alg"`
			} `json:"pubKeyCredParams"`
			Timeout                int `json:"timeout"`
			AuthenticatorSelection struct {
				RequireResidentKey *bool  `json:"requireResidentKey"`
				ResidentKey        string `json:"residentKey"`
				UserVerification   string `json:"userVerification"`
			} `json:"authenticatorSelection"`
			Attestation string `json:"attestation"`
		}
		if err := json.Unmarshal(begin.Options, &creation); err != nil {
			t.Fatal(err)
		}
		if creation.RP.ID != "astovia.test" || creation.RP.Name != "Astovia" || creation.User.ID != b64([]byte("alice-id")) || creation.User.Name != "alice@example.com" || creation.User.DisplayName != "alice@example.com" {
			t.Fatalf("creation options: %s", begin.Options)
		}
		if creation.Attestation != "none" || creation.AuthenticatorSelection.ResidentKey != "discouraged" || creation.AuthenticatorSelection.RequireResidentKey == nil || *creation.AuthenticatorSelection.RequireResidentKey || creation.AuthenticatorSelection.UserVerification != "preferred" || creation.Timeout <= 0 {
			t.Fatalf("creation options: %s", begin.Options)
		}
		if len(creation.PubKeyCredParams) == 0 || creation.PubKeyCredParams[0].Type != "public-key" || creation.PubKeyCredParams[0].Alg != -7 {
			t.Fatalf("pubKeyCredParams: %s", begin.Options)
		}
		if _, keys := jsonKeys(t, begin.Options); !reflect.DeepEqual(keys, []string{"attestation", "authenticatorSelection", "challenge", "pubKeyCredParams", "rp", "timeout", "user"}) {
			t.Fatalf("creation option keys: %v", keys)
		}
		// Nothing is stored before the authenticator answers.
		if got := api.status(alice); len(got.WebAuthn) != 0 || got.RecoveryCodesRemaining != 0 {
			t.Fatalf("after begin: %+v", got)
		}

		// The first factor returns the recovery codes; the name is trimmed.
		key := newSoftKey(t)
		w = api.finishKey(alice, begin.State, key.create(begin.Options), "  YubiKey 5  ")
		expect(t, w, 201, "")
		created := decodeJSON[webauthnCreatedResponse](t, w)
		checkRecoveryCodes(t, created.RecoveryCodes)
		registeredAt := api.now.Unix()
		if created.Credential.ID == "" || created.Credential.Name != "YubiKey 5" || created.Credential.CreatedAt != formatTime(registeredAt) || created.Credential.LastUsedAt != nil {
			t.Fatalf("created: %+v", created.Credential)
		}
		if got := api.status(alice); len(got.WebAuthn) != 1 || got.WebAuthn[0] != created.Credential || got.RecoveryCodesRemaining != 10 || got.TOTP != nil {
			t.Fatalf("status: %+v", got)
		}
		row, stored := api.storedKey("alice-id", created.Credential.ID)
		if row.CredentialID != b64(key.id) || !reflect.DeepEqual(stored.ID, key.id) || stored.Authenticator.SignCount != 0 || stored.AttestationFormat != "none" || row.LastUsedAt.Valid {
			t.Fatalf("stored: %+v", row)
		}
		// The session, its refresh and the API token are kept.
		expect(t, api.do("GET", "/api/v1/me", alice, ""), 200, "")
		_, aliceCookie = api.session(api.public("/auth/refresh", `{}`, aliceCookie))
		expect(t, api.do("GET", "/api/v1/action-items", apiToken, ""), 200, "")

		// A second key returns no codes, and its options exclude the first.
		begin = api.beginKey(alice)
		var excluding struct {
			ExcludeCredentials []struct {
				Type       string   `json:"type"`
				ID         string   `json:"id"`
				Transports []string `json:"transports"`
			} `json:"excludeCredentials"`
		}
		if err := json.Unmarshal(begin.Options, &excluding); err != nil {
			t.Fatal(err)
		}
		if len(excluding.ExcludeCredentials) != 1 || excluding.ExcludeCredentials[0].ID != b64(key.id) || excluding.ExcludeCredentials[0].Type != "public-key" || !reflect.DeepEqual(excluding.ExcludeCredentials[0].Transports, []string{"usb"}) {
			t.Fatalf("excludeCredentials: %s", begin.Options)
		}
		passkey := newSoftKey(t)
		passkey.counterless = true
		api.advance(time.Second)
		w = api.finishKey(alice, begin.State, passkey.create(begin.Options), "Passkey")
		expect(t, w, 201, "")
		if !strings.Contains(w.Body.String(), `"recovery_codes":null`) {
			t.Fatalf("second key: %s", w.Body.String())
		}
		second := decodeJSON[webauthnCreatedResponse](t, w).Credential
		if got := api.status(alice); len(got.WebAuthn) != 2 || got.WebAuthn[0].ID != created.Credential.ID || got.WebAuthn[1] != second || got.RecoveryCodesRemaining != 10 {
			t.Fatalf("status: %+v", got)
		}

		// The password alone starts no session now.
		ch := api.pending("alice@example.com")
		if !ch.Enrolled || !ch.WebAuthnAvailable || ch.Methods != (mfaMethodsResponse{WebAuthn: true, Recovery: true}) {
			t.Fatalf("challenge: %+v", ch)
		}
		noSession(t, api.post("/auth/mfa/totp", map[string]string{"mfa_token": ch.Token, "code": "000000"}), 401, "mfa_code_invalid")

		// The options to sign in allow the credentials of alice alone.
		signIn := api.beginSignIn(ch.Token)
		var request struct {
			RPID             string `json:"rpId"`
			Timeout          int    `json:"timeout"`
			UserVerification string `json:"userVerification"`
			AllowCredentials []struct {
				Type string `json:"type"`
				ID   string `json:"id"`
			} `json:"allowCredentials"`
		}
		if err := json.Unmarshal(signIn.Options, &request); err != nil {
			t.Fatal(err)
		}
		if request.RPID != "astovia.test" || request.UserVerification != "preferred" || request.Timeout <= 0 || len(request.AllowCredentials) != 2 || request.AllowCredentials[0].ID != b64(key.id) || request.AllowCredentials[1].ID != b64(passkey.id) || request.AllowCredentials[0].Type != "public-key" {
			t.Fatalf("request options: %s", signIn.Options)
		}
		if _, keys := jsonKeys(t, signIn.Options); !reflect.DeepEqual(keys, []string{"allowCredentials", "challenge", "rpId", "timeout", "userVerification"}) {
			t.Fatalf("request option keys: %v", keys)
		}

		// The signature starts the session; the counter and the time of use
		// are stored.
		api.advance(time.Minute)
		w = api.finishSignIn(ch.Token, signIn.State, key.get(signIn.Options))
		if _, keys := jsonKeys(t, w.Body.Bytes()); !reflect.DeepEqual(keys, []string{"access_token", "expires_in", "user"}) {
			t.Fatalf("finish keys: %v", keys)
		}
		access, cookie := api.session(w)
		if got := decodeJSON[userResponse](t, api.do("GET", "/api/v1/me", access, "")); got.ID != "alice-id" {
			t.Fatalf("me: %+v", got)
		}
		api.session(api.public("/auth/refresh", `{}`, cookie))
		usedAt := formatTime(api.now.Unix())
		row, stored = api.storedKey("alice-id", created.Credential.ID)
		if stored.Authenticator.SignCount != 1 || !row.LastUsedAt.Valid || row.LastUsedAt.Int64 != api.now.Unix() || row.CreatedAt != registeredAt || row.Name != "YubiKey 5" {
			t.Fatalf("stored after the sign-in: %+v", row)
		}
		if got := api.status(access); got.WebAuthn[0].LastUsedAt == nil || *got.WebAuthn[0].LastUsedAt != usedAt || got.WebAuthn[1].LastUsedAt != nil {
			t.Fatalf("status after the sign-in: %+v", got.WebAuthn)
		}
		// Again, with a user handle, and with the passkey that has no counter.
		key.handle = []byte("alice-id")
		api.keySignIn("alice@example.com", key)
		if _, stored = api.storedKey("alice-id", created.Credential.ID); stored.Authenticator.SignCount != 2 {
			t.Fatalf("counter: %d", stored.Authenticator.SignCount)
		}
		api.keySignIn("alice@example.com", passkey)
		api.keySignIn("alice@example.com", passkey)
		if row, stored = api.storedKey("alice-id", second.ID); stored.Authenticator.SignCount != 0 || !row.LastUsedAt.Valid {
			t.Fatalf("passkey: %+v", row)
		}
		// The recovery codes sign in as well.
		ch = api.pending("alice@example.com")
		api.session(api.post("/auth/mfa/recovery", map[string]string{"mfa_token": ch.Token, "code": created.RecoveryCodes[0]}))
		// The session from before the keys is still good.
		api.session(api.public("/auth/refresh", `{}`, aliceCookie))
	})
}

// TestWebAuthnSignInFailures checks what does not sign in.
func TestWebAuthnSignInFailures(t *testing.T) {
	api := newMFAAPI(t, "sqlite", config.MFAOff)
	alice, _ := api.login("alice@example.com", mfaPassword)
	carol, _ := api.login("carol@example.com", mfaPassword)
	key, carolKey := newSoftKey(t), newSoftKey(t)
	created, _ := api.addKey(alice, key, "Key")
	api.addKey(carol, carolKey, "Key")
	before, _ := api.storedKey("alice-id", created.ID)
	setup := api.setup(alice)
	registration := api.beginKey(alice)

	ch, carolCh := api.pending("alice@example.com"), api.pending("carol@example.com")
	begin, carolBegin := api.beginSignIn(ch.Token), api.beginSignIn(carolCh.Token)
	rejected := func(name string, w *httptest.ResponseRecorder, status int, code string) {
		t.Helper()
		if w.Code != status || errorCode(t, w) != code || len(w.Header().Values("Set-Cookie")) != 0 || strings.Contains(w.Body.String(), "access_token") {
			t.Fatalf("%s: %d %s", name, w.Code, w.Body.String())
		}
	}

	// Another private key behind the same credential ID, and an unknown
	// credential.
	forged := newSoftKey(t)
	forged.id = key.id
	rejected("another key", api.finishSignIn(ch.Token, begin.State, forged.get(begin.Options)), 401, "mfa_code_invalid")
	rejected("unknown credential", api.finishSignIn(ch.Token, begin.State, newSoftKey(t).get(begin.Options)), 401, "mfa_code_invalid")
	// Another challenge: the answer to an earlier begin request.
	later := api.beginSignIn(ch.Token)
	rejected("another challenge", api.finishSignIn(ch.Token, later.State, key.get(begin.Options)), 401, "mfa_code_invalid")
	rejected("another challenge", api.finishSignIn(ch.Token, begin.State, key.get(later.Options)), 401, "mfa_code_invalid")
	// Another origin and another relying party.
	for _, origin := range []string{"https://evil.test", "https://astovia.test:8443", "http://astovia.test", "https://astovia.test.evil.test"} {
		key.origin = origin
		rejected(origin, api.finishSignIn(ch.Token, begin.State, key.get(begin.Options)), 401, "mfa_code_invalid")
	}
	key.origin = usersOrigin
	key.rpID = "evil.test"
	rejected("another relying party", api.finishSignIn(ch.Token, begin.State, key.get(begin.Options)), 401, "mfa_code_invalid")
	key.rpID = ""
	// The user handle of another user.
	key.handle = []byte("carol-id")
	rejected("another user handle", api.finishSignIn(ch.Token, begin.State, key.get(begin.Options)), 401, "mfa_code_invalid")
	key.handle = nil

	// The credential of another user: carol's key answers alice's
	// challenge, and alice's key carol's.
	rejected("carol's key for alice", api.finishSignIn(ch.Token, begin.State, carolKey.get(begin.Options)), 401, "mfa_code_invalid")
	rejected("alice's key for carol", api.finishSignIn(carolCh.Token, carolBegin.State, key.get(carolBegin.Options)), 401, "mfa_code_invalid")
	// carol's own, valid sign-in does not serve alice's token, nor alice's
	// state carol's token.
	rejected("carol's state for alice", api.finishSignIn(ch.Token, carolBegin.State, carolKey.get(carolBegin.Options)), 400, "mfa_state_invalid")
	rejected("alice's state for carol", api.finishSignIn(carolCh.Token, begin.State, carolKey.get(begin.Options)), 400, "mfa_state_invalid")

	// States that are not the state of a sign-in of this user.
	for name, state := range map[string]string{
		"empty": "", "garbage": "v1.AAAA", "registration state": registration.State, "setup token": setup.SetupToken,
		"mfa token": ch.Token, "access token": alice, "too long": begin.State + strings.Repeat("A", maxWebAuthnStateLength),
	} {
		rejected(name, api.finishSignIn(ch.Token, state, key.get(begin.Options)), 400, "mfa_state_invalid")
	}
	tampered := []byte(begin.State)
	tampered[len(tampered)/2] ^= 1
	rejected("tampered state", api.finishSignIn(ch.Token, string(tampered), key.get(begin.Options)), 400, "mfa_state_invalid")

	// Responses that are not an assertion.
	for name, credential := range map[string]any{"empty object": map[string]any{}, "string": "x", "number": 1, "null": nil, "registration response": json.RawMessage(key.create(registration.Options))} {
		rejected(name, api.send("/auth/mfa/webauthn/finish", map[string]any{"mfa_token": ch.Token, "state": begin.State, "credential": credential}), 401, "mfa_code_invalid")
	}
	rejected("no credential", api.send("/auth/mfa/webauthn/finish", map[string]any{"mfa_token": ch.Token, "state": begin.State}), 400, "invalid_request")
	rejected("unknown field", api.send("/auth/mfa/webauthn/finish", map[string]any{"mfa_token": ch.Token, "state": begin.State, "credential": map[string]any{}, "user_id": "alice-id"}), 400, "invalid_request")

	// MFA tokens that are not one.
	for name, token := range map[string]string{"empty": "", "garbage": "v1.AAAA", "state": begin.State, "access token": alice} {
		rejected(name, api.send("/auth/mfa/webauthn/begin", map[string]any{"mfa_token": token}), 401, "mfa_token_invalid")
		rejected(name, api.finishSignIn(token, begin.State, key.get(begin.Options)), 401, "mfa_token_invalid")
		rejected(name, api.send("/auth/mfa/enroll/webauthn/begin", map[string]any{"mfa_token": token}), 401, "mfa_token_invalid")
		rejected(name, api.send("/auth/mfa/enroll/webauthn/finish", map[string]any{"mfa_token": token, "state": registration.State, "credential": map[string]any{}, "name": "Key"}), 401, "mfa_token_invalid")
	}

	// The state is good for five minutes: a second before, a wrong
	// signature is still what fails.
	api.advance(5*time.Minute - time.Second)
	rejected("another key before the expiry", api.finishSignIn(ch.Token, begin.State, forged.get(begin.Options)), 401, "mfa_code_invalid")
	api.advance(time.Second)
	rejected("expired state", api.finishSignIn(ch.Token, begin.State, key.get(begin.Options)), 400, "mfa_state_invalid")

	// None of it changed the credential, and the key still signs in.
	if after, _ := api.storedKey("alice-id", created.ID); !reflect.DeepEqual(after, before) {
		t.Fatalf("the credential changed: %+v", after)
	}
	begin = api.beginSignIn(ch.Token)
	api.session(api.finishSignIn(ch.Token, begin.State, key.get(begin.Options)))
	_, stored := api.storedKey("alice-id", created.ID)
	counter := stored.Authenticator.SignCount
	if counter != key.counter || counter < 2 {
		t.Fatalf("counter: %d, key %d", counter, key.counter)
	}

	// A counter that does not advance may be a clone: rejected, and not
	// stored.
	used, _ := api.storedKey("alice-id", created.ID)
	for _, back := range []uint32{counter - 1, 0} {
		key.counter = back
		begin = api.beginSignIn(ch.Token)
		rejected("counter", api.finishSignIn(ch.Token, begin.State, key.get(begin.Options)), 401, "mfa_code_invalid")
	}
	if after, _ := api.storedKey("alice-id", created.ID); !reflect.DeepEqual(after, used) {
		t.Fatalf("the credential changed: %+v", after)
	}
	key.counter = counter
	begin = api.beginSignIn(ch.Token)
	api.session(api.finishSignIn(ch.Token, begin.State, key.get(begin.Options)))

	// A user without a key has nothing to begin with.
	if err := api.store.ResetMFA(context.Background(), "carol@example.com"); err != nil {
		t.Fatal(err)
	}
	api.use(config.MFAAll, false)
	none := api.pending("carol@example.com")
	rejected("no key", api.send("/auth/mfa/webauthn/begin", map[string]any{"mfa_token": none.Token}), 401, "mfa_code_invalid")
	// A credential removed between begin and finish.
	api.use(config.MFAOff, false)
	begin = api.beginSignIn(ch.Token)
	if err := api.store.DeleteWebAuthnCredential(context.Background(), "alice-id", created.ID, config.MFAOff); err != nil {
		t.Fatal(err)
	}
	rejected("removed credential", api.finishSignIn(ch.Token, begin.State, key.get(begin.Options)), 401, "mfa_code_invalid")
}

// TestWebAuthnReplay checks that the response of a sign-in works once: the
// challenge is spent with it. The key has no counter, so nothing else
// stands in the way of the second use.
func TestWebAuthnReplay(t *testing.T) {
	mfaDrivers(t, func(t *testing.T, driver string) {
		api := newMFAAPI(t, driver, config.MFAOff)
		alice, _ := api.login("alice@example.com", mfaPassword)
		key := newSoftKey(t)
		key.counterless = true
		created, _ := api.addKey(alice, key, "Passkey")

		ch := api.pending("alice@example.com")
		begin := api.beginSignIn(ch.Token)
		response := key.get(begin.Options)
		api.session(api.finishSignIn(ch.Token, begin.State, response))
		used, _ := api.storedKey("alice-id", created.ID)
		// The same response again: with the same token, later, and with the
		// token of a new sign-in.
		noSession(t, api.finishSignIn(ch.Token, begin.State, response), 401, "mfa_code_invalid")
		api.advance(time.Minute)
		noSession(t, api.finishSignIn(ch.Token, begin.State, response), 401, "mfa_code_invalid")
		noSession(t, api.finishSignIn(api.pending("alice@example.com").Token, begin.State, response), 401, "mfa_code_invalid")
		// A new signature over the spent challenge is no better.
		noSession(t, api.finishSignIn(ch.Token, begin.State, key.get(begin.Options)), 401, "mfa_code_invalid")
		if after, _ := api.storedKey("alice-id", created.ID); !reflect.DeepEqual(after, used) {
			t.Fatalf("a replay changed the credential: %+v", after)
		}
		// A new challenge signs in.
		api.keySignIn("alice@example.com", key)

		// The same response twice at once: one session.
		for round := 0; round < 5; round++ {
			begin = api.beginSignIn(ch.Token)
			body := string(mustJSON(t, map[string]any{"mfa_token": ch.Token, "state": begin.State, "credential": key.get(begin.Options)}))
			sessions := 0
			for _, w := range api.parallelRaw("/auth/mfa/webauthn/finish", body, body) {
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
		}

		// A key with a counter cannot be replayed either.
		counting := newSoftKey(t)
		api.addKey(alice, counting, "Key")
		begin = api.beginSignIn(ch.Token)
		response = counting.get(begin.Options)
		api.session(api.finishSignIn(ch.Token, begin.State, response))
		noSession(t, api.finishSignIn(ch.Token, begin.State, response), 401, "mfa_code_invalid")

		// The challenge is spent even when the session could not be started.
		if driver != "sqlite" {
			return
		}
		raw, err := sql.Open("sqlite", api.dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = raw.Close() }()
		captureLog(t)
		begin = api.beginSignIn(ch.Token)
		response = key.get(begin.Options)
		if _, err = raw.ExecContext(context.Background(), "ALTER TABLE refresh_tokens RENAME TO refresh_tokens_away"); err != nil {
			t.Fatal(err)
		}
		noSession(t, api.finishSignIn(ch.Token, begin.State, response), 503, "signin_unavailable")
		if _, err = raw.ExecContext(context.Background(), "ALTER TABLE refresh_tokens_away RENAME TO refresh_tokens"); err != nil {
			t.Fatal(err)
		}
		noSession(t, api.finishSignIn(ch.Token, begin.State, response), 401, "mfa_code_invalid")
		api.keySignIn("alice@example.com", key)
	})
}

// TestWebAuthnEnrollBeforeSession checks the registration of a key by a
// user that must have a factor, and that nobody else can register one with
// the password alone.
func TestWebAuthnEnrollBeforeSession(t *testing.T) {
	mfaDrivers(t, func(t *testing.T, driver string) {
		ctx := context.Background()
		api := newMFAAPI(t, driver, config.MFAOwners)
		begin := func(token string) *httptest.ResponseRecorder {
			return api.send("/auth/mfa/enroll/webauthn/begin", map[string]any{"mfa_token": token})
		}
		finish := func(token, state string, credential json.RawMessage, name string) *httptest.ResponseRecorder {
			return api.send("/auth/mfa/enroll/webauthn/finish", map[string]any{"mfa_token": token, "state": state, "credential": credential, "name": name})
		}

		ch := api.pending("alice@example.com")
		if ch.Enrolled || !ch.WebAuthnAvailable {
			t.Fatalf("alice: %+v", ch)
		}
		// No key to sign in with yet.
		noSession(t, api.send("/auth/mfa/webauthn/begin", map[string]any{"mfa_token": ch.Token}), 401, "mfa_code_invalid")
		started := api.begun(begin(ch.Token))
		if strings.Contains(string(started.Options), "excludeCredentials") {
			t.Fatalf("options: %s", started.Options)
		}
		key := newSoftKey(t)
		// A bad name, a bad state and a bad response store nothing.
		noSession(t, finish(ch.Token, started.State, key.create(started.Options), ""), 400, "invalid_request")
		noSession(t, finish(ch.Token, started.State, key.create(started.Options), strings.Repeat("n", 65)), 400, "invalid_request")
		noSession(t, finish(ch.Token, "v1.AAAA", key.create(started.Options), "Key"), 400, "mfa_state_invalid")
		noSession(t, finish(ch.Token, started.State, json.RawMessage(`{}`), "Key"), 401, "mfa_code_invalid")
		other := api.begun(begin(ch.Token))
		noSession(t, finish(ch.Token, started.State, key.create(other.Options), "Key"), 401, "mfa_code_invalid")
		key.origin = "https://evil.test"
		noSession(t, finish(ch.Token, started.State, key.create(started.Options), "Key"), 401, "mfa_code_invalid")
		key.origin = usersOrigin
		noSession(t, api.send("/auth/mfa/enroll/webauthn/finish", map[string]any{"mfa_token": ch.Token, "state": started.State, "name": "Key"}), 400, "invalid_request")
		if status, err := api.store.MFAStatus(ctx, "alice-id"); err != nil || status.Enrolled() || status.RecoveryCodes != 0 {
			t.Fatalf("after the failures: %+v %v", status, err)
		}

		// The response registers the key and signs in.
		w := finish(ch.Token, started.State, key.create(started.Options), "Key")
		if _, keys := jsonKeys(t, w.Body.Bytes()); !reflect.DeepEqual(keys, []string{"access_token", "expires_in", "recovery_codes", "user"}) {
			t.Fatalf("finish keys: %v", keys)
		}
		alice, _ := api.session(w)
		checkRecoveryCodes(t, decodeJSON[mfaAuthResponse](t, w).RecoveryCodes)
		if got := api.status(alice); !got.Required || len(got.WebAuthn) != 1 || got.WebAuthn[0].Name != "Key" || got.TOTP != nil || got.RecoveryCodesRemaining != 10 {
			t.Fatalf("status: %+v", got)
		}

		// The same token registers nothing more: the account has a factor.
		second := newSoftKey(t)
		noSession(t, begin(ch.Token), 409, "mfa_already_enrolled")
		noSession(t, finish(ch.Token, other.State, second.create(other.Options), "Second"), 409, "mfa_already_enrolled")
		noSession(t, finish(ch.Token, started.State, key.create(started.Options), "Key"), 409, "mfa_already_enrolled")
		// Neither does the token of a user with a factor, issued by the
		// password alone, with a state from the account screen.
		enrolled := api.pending("alice@example.com")
		if !enrolled.Enrolled || enrolled.Methods != (mfaMethodsResponse{WebAuthn: true, Recovery: true}) {
			t.Fatalf("after the registration: %+v", enrolled)
		}
		fromAccount := api.beginKey(alice)
		noSession(t, begin(enrolled.Token), 409, "mfa_already_enrolled")
		noSession(t, finish(enrolled.Token, fromAccount.State, second.create(fromAccount.Options), "Second"), 409, "mfa_already_enrolled")
		noSession(t, api.post("/auth/mfa/enroll/totp/begin", map[string]string{"mfa_token": enrolled.Token}), 409, "mfa_already_enrolled")
		if status, err := api.store.MFAStatus(ctx, "alice-id"); err != nil || len(status.WebAuthn) != 1 || status.RecoveryCodes != 10 {
			t.Fatalf("alice: %+v %v", status, err)
		}
		api.keySignIn("alice@example.com", key)

		// A user that need not have a factor cannot register before a
		// session: carol's token was issued while everybody had to.
		api.use(config.MFAAll, false)
		carol := api.pending("carol@example.com")
		carolStarted := api.begun(begin(carol.Token))
		api.use(config.MFAOwners, false)
		noSession(t, begin(carol.Token), 403, "mfa_not_required")
		noSession(t, finish(carol.Token, carolStarted.State, second.create(carolStarted.Options), "Key"), 403, "mfa_not_required")
		if status, err := api.store.MFAStatus(ctx, "carol-id"); err != nil || status.Enrolled() || status.RecoveryCodes != 0 {
			t.Fatalf("carol: %+v %v", status, err)
		}

		// Two registrations of the same user at once, with a key and with an
		// authenticator app: one factor, one set of codes and one session.
		api.use(config.MFAAll, false)
		for round := 0; round < 4; round++ {
			carol = api.pending("carol@example.com")
			a, b := api.begun(begin(carol.Token)), api.begun(begin(carol.Token))
			bodies := []string{
				string(mustJSON(t, map[string]any{"mfa_token": carol.Token, "state": a.State, "credential": newSoftKey(t).create(a.Options), "name": "A"})),
				string(mustJSON(t, map[string]any{"mfa_token": carol.Token, "state": b.State, "credential": newSoftKey(t).create(b.Options), "name": "B"})),
			}
			sessions := 0
			for _, w := range api.parallelRaw("/auth/mfa/enroll/webauthn/finish", bodies...) {
				switch {
				case w.Code == 200 && len(w.Header().Values("Set-Cookie")) == 1:
					sessions++
				case w.Code == 409 && errorCode(t, w) == "mfa_already_enrolled" && len(w.Header().Values("Set-Cookie")) == 0:
				default:
					t.Fatalf("round %d: %d %s", round, w.Code, w.Body.String())
				}
			}
			status, err := api.store.MFAStatus(ctx, "carol-id")
			if err != nil || sessions != 1 || len(status.WebAuthn) != 1 || status.RecoveryCodes != 10 {
				t.Fatalf("round %d: %d sessions, %+v %v", round, sessions, status, err)
			}
			if err = api.store.ResetMFA(ctx, "carol@example.com"); err != nil {
				t.Fatal(err)
			}
		}
	})
}

// TestWebAuthnManagement checks the keys of the account screen.
func TestWebAuthnManagement(t *testing.T) {
	mfaDrivers(t, func(t *testing.T, driver string) {
		ctx := context.Background()
		api := newMFAAPI(t, driver, config.MFAOff)
		alice, aliceCookie := api.login("alice@example.com", mfaPassword)
		carol, _ := api.login("carol@example.com", mfaPassword)
		w := api.do("POST", "/api/v1/api-tokens", alice, `{"name":"cli"}`)
		expect(t, w, 201, "")
		apiToken := decodeJSON[createdAPITokenResponse](t, w).Token
		empty, err := api.store.MFAStatus(ctx, "alice-id")
		if err != nil {
			t.Fatal(err)
		}
		unchanged := func(want store.MFAStatus) {
			t.Helper()
			got, err := api.store.MFAStatus(ctx, "alice-id")
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("alice changed: %+v, want %+v (%v)", got, want, err)
			}
		}

		// The password is checked at the begin request, with 400s.
		for _, path := range []string{"/api/v1/mfa/webauthn/begin", "/api/v1/mfa/webauthn/x/delete"} {
			expect(t, api.call(path, alice, map[string]string{"password": "wrong-password-123"}), 400, "password_invalid")
			expect(t, api.call(path, alice, map[string]string{"password": strings.Repeat("p", 73)}), 400, "invalid_request")
			expect(t, api.call(path, alice, map[string]string{}), 400, "invalid_request")
			expect(t, api.do("POST", path, alice, `{"password":"`+mfaPassword+`","extra":1}`), 400, "invalid_request")
			expect(t, api.do("POST", path, alice, `{"password":"`+strings.Repeat("p", 4097)+`"}`), 413, "body_too_large")
		}
		expect(t, api.call("/api/v1/mfa/webauthn/x/delete", alice, map[string]string{"password": mfaPassword}), 404, "not_found")

		// The state and the response, with 400s as well: a 401 would make
		// the web UI refresh the session and send the request again.
		key := newSoftKey(t)
		begin, carolBegin := api.beginKey(alice), api.beginKey(carol)
		api.use(config.MFAAll, false)
		pending := api.pending("alice@example.com")
		api.use(config.MFAOff, false)
		setup := api.setup(alice)
		for name, state := range map[string]string{"empty": "", "garbage": "v1.AAAA", "carol's": carolBegin.State, "setup token": setup.SetupToken, "mfa token": pending.Token, "access token": alice, "too long": begin.State + strings.Repeat("A", maxWebAuthnStateLength)} {
			if w := api.finishKey(alice, state, key.create(begin.Options), "Key"); w.Code != 400 || errorCode(t, w) != "mfa_state_invalid" {
				t.Fatalf("%s state: %d %s", name, w.Code, w.Body.String())
			}
		}
		later := api.beginKey(alice)
		expect(t, api.finishKey(alice, later.State, key.create(begin.Options), "Key"), 400, "mfa_code_invalid")
		expect(t, api.finishKey(alice, begin.State, key.create(carolBegin.Options), "Key"), 400, "mfa_code_invalid")
		expect(t, api.finishKey(alice, begin.State, json.RawMessage(`{}`), "Key"), 400, "mfa_code_invalid")
		expect(t, api.finishKey(alice, begin.State, json.RawMessage(`"x"`), "Key"), 400, "mfa_code_invalid")
		key.origin = "https://evil.test"
		expect(t, api.finishKey(alice, begin.State, key.create(begin.Options), "Key"), 400, "mfa_code_invalid")
		key.origin = usersOrigin
		key.rpID = "evil.test"
		expect(t, api.finishKey(alice, begin.State, key.create(begin.Options), "Key"), 400, "mfa_code_invalid")
		key.rpID = ""
		// The names: 1-64 characters after trimming, no control characters.
		for _, name := range []string{"", "   ", strings.Repeat("n", 65), strings.Repeat("鍵", 65), "a\nb", "a\x00b", "a\u0085b"} {
			expect(t, api.finishKey(alice, begin.State, key.create(begin.Options), name), 400, "invalid_request")
		}
		expect(t, api.ask("/api/v1/mfa/webauthn/finish", alice, map[string]any{"state": begin.State, "name": "Key"}), 400, "invalid_request")
		expect(t, api.ask("/api/v1/mfa/webauthn/finish", alice, map[string]any{"state": begin.State, "credential": map[string]any{}, "name": "Key", "password": "x"}), 400, "invalid_request")
		expect(t, api.do("POST", "/api/v1/mfa/webauthn/finish", alice, `{"state":"`+strings.Repeat("s", webauthnBodyLimit)+`"}`), 413, "body_too_large")
		unchanged(empty)
		// The state expires five minutes after the begin request.
		api.advance(5*time.Minute - time.Second)
		expect(t, api.finishKey(alice, begin.State, json.RawMessage(`{}`), "Key"), 400, "mfa_code_invalid")
		api.advance(time.Second)
		expect(t, api.finishKey(alice, begin.State, key.create(begin.Options), "Key"), 400, "mfa_state_invalid")
		unchanged(empty)

		// A name of 64 characters, of any script.
		long := strings.Repeat("鍵", 64)
		begin = api.beginKey(alice)
		w = api.finishKey(alice, begin.State, key.create(begin.Options), " "+long+"\t")
		expect(t, w, 201, "")
		first := decodeJSON[webauthnCreatedResponse](t, w)
		checkRecoveryCodes(t, first.RecoveryCodes)
		if first.Credential.Name != long {
			t.Fatalf("name: %q", first.Credential.Name)
		}
		// The same response again, the same key again and the same key for
		// another user: the credential is registered already.
		expect(t, api.finishKey(alice, begin.State, key.create(begin.Options), "Again"), 409, "mfa_credential_exists")
		begin = api.beginKey(alice)
		expect(t, api.finishKey(alice, begin.State, key.create(begin.Options), "Again"), 409, "mfa_credential_exists")
		carolBegin = api.beginKey(carol)
		expect(t, api.finishKey(carol, carolBegin.State, key.create(carolBegin.Options), "Stolen"), 409, "mfa_credential_exists")
		if status, err := api.store.MFAStatus(ctx, "carol-id"); err != nil || status.Enrolled() || status.RecoveryCodes != 0 {
			t.Fatalf("carol: %+v %v", status, err)
		}
		if got := api.status(alice); len(got.WebAuthn) != 1 || got.RecoveryCodesRemaining != 10 {
			t.Fatalf("status: %+v", got)
		}

		// Access tokens only, JSON only, and the methods.
		paths := []string{"/api/v1/mfa/webauthn/begin", "/api/v1/mfa/webauthn/finish", "/api/v1/mfa/webauthn/" + first.Credential.ID + "/delete"}
		for _, path := range paths {
			for _, bearer := range []string{apiToken, pending.Token, ""} {
				expect(t, api.call(path, bearer, map[string]string{"password": mfaPassword}), 401, "unauthorized")
			}
			r := httptest.NewRequest("POST", path, strings.NewReader(`{"password":"`+mfaPassword+`"}`))
			r.Header.Set("Authorization", "Bearer "+alice)
			expect(t, serve(t, api.handler, r), 415, "unsupported_media_type")
			expect(t, api.do("GET", path, alice, ""), 405, "method_not_allowed")
			expect(t, api.do("DELETE", path, alice, ""), 405, "method_not_allowed")
		}
		expect(t, api.call("/api/v1/mfa/webauthn", alice, map[string]string{"password": mfaPassword}), 404, "not_found")
		expect(t, api.do("DELETE", "/api/v1/mfa/webauthn/"+first.Credential.ID, alice, ""), 404, "not_found")

		// Ten keys at most: the eleventh is refused at the begin request,
		// and at the finish request of a registration begun before.
		begin = api.beginKey(alice)
		for i := 1; i < store.MaxWebAuthnCredentials; i++ {
			c := store.WebAuthnCredential{ID: "filler-" + string(rune('a'+i)), UserID: "alice-id", CredentialID: "filler-" + string(rune('a'+i)), Credential: "{}", Name: "Filler", CreatedAt: api.now.Unix()}
			if _, err = api.store.EnrollWebAuthn(ctx, c, nil, false); err != nil {
				t.Fatal(err)
			}
		}
		full, err := api.store.MFAStatus(ctx, "alice-id")
		if err != nil || len(full.WebAuthn) != store.MaxWebAuthnCredentials {
			t.Fatalf("%d credentials (%v)", len(full.WebAuthn), err)
		}
		expect(t, api.call("/api/v1/mfa/webauthn/begin", alice, map[string]string{"password": mfaPassword}), 409, "mfa_limit")
		expect(t, api.finishKey(alice, begin.State, newSoftKey(t).create(begin.Options), "Eleventh"), 409, "mfa_limit")
		unchanged(full)
		for i := 1; i < store.MaxWebAuthnCredentials; i++ {
			if err = api.store.DeleteWebAuthnCredential(ctx, "alice-id", "filler-"+string(rune('a'+i)), config.MFAOff); err != nil {
				t.Fatal(err)
			}
		}

		// Removing: the password, and only the keys of the user.
		carolKey := newSoftKey(t)
		carolCreated, _ := api.addKey(carol, carolKey, "Carol's")
		before, err := api.store.MFAStatus(ctx, "alice-id")
		if err != nil {
			t.Fatal(err)
		}
		remove := func(access, id, password string) *httptest.ResponseRecorder {
			return api.call("/api/v1/mfa/webauthn/"+id+"/delete", access, map[string]string{"password": password})
		}
		expect(t, remove(alice, first.Credential.ID, "wrong-password-123"), 400, "password_invalid")
		expect(t, remove(alice, carolCreated.ID, mfaPassword), 404, "not_found")
		expect(t, remove(carol, first.Credential.ID, mfaPassword), 404, "not_found")
		expect(t, remove(alice, "missing", mfaPassword), 404, "not_found")
		unchanged(before)
		if status, err := api.store.MFAStatus(ctx, "carol-id"); err != nil || len(status.WebAuthn) != 1 {
			t.Fatalf("carol: %+v %v", status, err)
		}
		// A user that must have a factor cannot remove the last one.
		for _, policy := range []string{config.MFAOwners, config.MFAAll} {
			api.use(policy, false)
			expect(t, remove(alice, first.Credential.ID, mfaPassword), 409, "mfa_required")
			unchanged(before)
		}
		// With an authenticator app as well, the key goes and the recovery
		// codes stay.
		api.enroll(alice)
		w = remove(alice, first.Credential.ID, mfaPassword)
		if w.Code != 204 || w.Body.Len() != 0 {
			t.Fatalf("delete: %d %s", w.Code, w.Body.String())
		}
		if got := api.status(alice); len(got.WebAuthn) != 0 || got.TOTP == nil || got.RecoveryCodesRemaining != 10 || !got.Required {
			t.Fatalf("status after the removal: %+v", got)
		}
		expect(t, remove(alice, first.Credential.ID, mfaPassword), 404, "not_found")
		if ch := api.pending("alice@example.com"); ch.Methods != (mfaMethodsResponse{TOTP: true, Recovery: true}) {
			t.Fatalf("methods: %+v", ch.Methods)
		}
		// The removed key registers again; not required, the last factor
		// goes with the recovery codes.
		api.use(config.MFAOff, false)
		again, codes := api.addKey(alice, key, "Key")
		if codes != nil {
			t.Fatalf("codes with a second factor: %d", len(codes))
		}
		expect(t, api.call("/api/v1/mfa/totp/delete", alice, map[string]string{"password": mfaPassword}), 204, "")
		if got := api.status(alice); len(got.WebAuthn) != 1 || got.RecoveryCodesRemaining != 10 {
			t.Fatalf("status: %+v", got)
		}
		expect(t, remove(alice, again.ID, mfaPassword), 204, "")
		unchanged(empty)
		// The sessions and the API token were kept through all of it, and
		// the password alone signs in again.
		expect(t, api.do("GET", "/api/v1/me", alice, ""), 200, "")
		api.session(api.public("/auth/refresh", `{}`, aliceCookie))
		expect(t, api.do("GET", "/api/v1/action-items", apiToken, ""), 200, "")
		api.login("alice@example.com", mfaPassword)
		// carol's key was not touched.
		api.keySignIn("carol@example.com", carolKey)
	})
}

// TestWebAuthnAvailability checks where passkeys and security keys can be
// used: the relying party ID is the host name of ASTOVIA_PUBLIC_URL.
func TestWebAuthnAvailability(t *testing.T) {
	logs := captureLog(t)
	for _, tc := range []struct {
		url, rpID string
		logged    bool
	}{
		{"https://astovia.test", "astovia.test", false},
		{"https://astovia.test:8443", "astovia.test", false},
		{"https://localhost", "localhost", false},
		{"http://localhost:8080", "localhost", false},
		{"http://astovia.test", "", false},
		{"http://astovia.test:8080", "", false},
		{"https://192.0.2.10", "", false},
		{"https://192.0.2.10:8443", "", false},
		{"http://127.0.0.1:8080", "", false},
		{"https://[2001:db8::1]:8443", "", false},
		{"", "", false},
		{"https://", "", false},
		{"://", "", false},
		// Hosts the library rejects as a relying party ID.
		{"https://astovia.test.", "", true},
		{"https://intranet", "", true},
	} {
		logs.Reset()
		rp := newWebAuthn(tc.url)
		if (rp != nil) != (tc.rpID != "") || (logs.Len() != 0) != tc.logged {
			t.Fatalf("%q: available %v, log %q", tc.url, rp != nil, logs.String())
		}
		if rp != nil && (rp.Config.RPID != tc.rpID || !reflect.DeepEqual(rp.Config.RPOrigins, []string{tc.url}) || rp.Config.RPDisplayName != "Astovia") {
			t.Fatalf("%q: %+v", tc.url, rp.Config)
		}
		if tc.logged && (strings.Count(logs.String(), "\n") != 1 || !strings.Contains(logs.String(), "auth: mfa: passkeys and security keys are off: ")) {
			t.Fatalf("%q: log %q", tc.url, logs.String())
		}
	}

	// With a port, the relying party ID has none and the origin has it.
	const withPort = "https://astovia.test:8443"
	api := newMFAAPI(t, "sqlite", config.MFAOff)
	alice, _ := api.login("alice@example.com", mfaPassword)
	api.useURL(config.MFAOff, withPort)
	key := newSoftKey(t)
	begin := api.beginKey(alice)
	if o := key.options(begin.Options); o.RPID != "astovia.test" {
		t.Fatalf("relying party ID %q", o.RPID)
	}
	expect(t, api.finishKey(alice, begin.State, key.create(begin.Options), "Key"), 400, "mfa_code_invalid")
	key.origin = withPort
	expect(t, api.finishKey(alice, begin.State, key.create(begin.Options), "Key"), 201, "")
	challenge := func() mfaChallengeResponse {
		t.Helper()
		w := api.send("/auth/login", map[string]any{"email": "alice@example.com", "password": mfaPassword})
		expect(t, w, 200, "")
		return decodeJSON[mfaPendingResponse](t, w).MFA
	}
	ch := challenge()
	if !ch.WebAuthnAvailable || !ch.Methods.WebAuthn {
		t.Fatalf("challenge: %+v", ch)
	}
	signIn := api.beginSignIn(ch.Token)
	key.origin = usersOrigin
	noSession(t, api.finishSignIn(ch.Token, signIn.State, key.get(signIn.Options)), 401, "mfa_code_invalid")
	key.origin = withPort
	api.session(api.finishSignIn(ch.Token, signIn.State, key.get(signIn.Options)))

	// At an IP address WebAuthn is off: the key of before is listed and can
	// be removed, but not used; the authenticator app works.
	api.useURL(config.MFAOff, usersOrigin)
	secret, _ := api.enroll(alice)
	keyState := api.beginKey(alice)
	api.useURL(config.MFAAll, "https://192.0.2.10")
	ch = challenge()
	if ch.WebAuthnAvailable || ch.Methods != (mfaMethodsResponse{TOTP: true, Recovery: true}) {
		t.Fatalf("challenge at an IP address: %+v", ch)
	}
	w := api.send("/auth/login", map[string]any{"email": "carol@example.com", "password": mfaPassword})
	expect(t, w, 200, "")
	carol := decodeJSON[mfaPendingResponse](t, w).MFA
	if carol.Enrolled || carol.WebAuthnAvailable {
		t.Fatalf("carol at an IP address: %+v", carol)
	}
	credential := json.RawMessage(key.get(signIn.Options))
	noSession(t, api.send("/auth/mfa/webauthn/begin", map[string]any{"mfa_token": ch.Token}), 400, "webauthn_unavailable")
	noSession(t, api.send("/auth/mfa/webauthn/finish", map[string]any{"mfa_token": ch.Token, "state": signIn.State, "credential": credential}), 400, "webauthn_unavailable")
	noSession(t, api.send("/auth/mfa/enroll/webauthn/begin", map[string]any{"mfa_token": carol.Token}), 400, "webauthn_unavailable")
	noSession(t, api.send("/auth/mfa/enroll/webauthn/finish", map[string]any{"mfa_token": carol.Token, "state": keyState.State, "credential": credential, "name": "Key"}), 400, "webauthn_unavailable")
	expect(t, api.call("/api/v1/mfa/webauthn/begin", alice, map[string]string{"password": mfaPassword}), 400, "webauthn_unavailable")
	// The password is not even checked.
	expect(t, api.call("/api/v1/mfa/webauthn/begin", alice, map[string]string{"password": "wrong-password-123"}), 400, "webauthn_unavailable")
	expect(t, api.finishKey(alice, keyState.State, newSoftKey(t).create(keyState.Options), "Key"), 400, "webauthn_unavailable")
	got := api.status(alice)
	if got.WebAuthnAvailable || len(got.WebAuthn) != 1 || got.TOTP == nil {
		t.Fatalf("status at an IP address: %+v", got)
	}
	// The authenticator app signs in, and enrolls carol.
	api.session(api.send("/auth/mfa/totp", map[string]any{"mfa_token": ch.Token, "code": api.code(secret, 0)}))
	expect(t, api.send("/auth/mfa/enroll/totp/begin", map[string]any{"mfa_token": carol.Token}), 200, "")
	expect(t, api.call("/api/v1/mfa/webauthn/"+got.WebAuthn[0].ID+"/delete", alice, map[string]string{"password": mfaPassword}), 204, "")
	if got = api.status(alice); len(got.WebAuthn) != 0 {
		t.Fatalf("status after the removal: %+v", got)
	}
}

// TestWebAuthnUnsolicitedExtensions registers and signs in with responses
// that carry an extension output the server did not ask for, as password
// manager extensions return: the output is ignored.
func TestWebAuthnUnsolicitedExtensions(t *testing.T) {
	api := newMFAAPI(t, "sqlite", config.MFAOwners)
	credProps := map[string]any{"credProps": map[string]any{"rk": false}}

	// From the account screen, and the sign-in with the key.
	carol, _ := api.login("carol@example.com", mfaPassword)
	key := newSoftKey(t)
	key.extensions = credProps
	created, codes := api.addKey(carol, key, "Password manager")
	if created.Name != "Password manager" || len(codes) != 10 {
		t.Fatalf("created: %+v, %d codes", created, len(codes))
	}
	if access, _ := api.keySignIn("carol@example.com", key); access == "" {
		t.Fatal("no session after the sign-in with extension results")
	}

	// Before the first session.
	ch := api.pending("alice@example.com")
	if ch.Enrolled {
		t.Fatalf("challenge: %+v", ch)
	}
	first := newSoftKey(t)
	first.extensions = credProps
	begin := api.begun(api.send("/auth/mfa/enroll/webauthn/begin", map[string]any{"mfa_token": ch.Token}))
	w := api.send("/auth/mfa/enroll/webauthn/finish", map[string]any{"mfa_token": ch.Token, "state": begin.State, "credential": first.create(begin.Options), "name": "First"})
	expect(t, w, 200, "")
	if access, _ := api.keySignIn("alice@example.com", first); access == "" {
		t.Fatal("no session after the sign-in with the enrolled key")
	}
}

// TestWebAuthnLimitsAndLogs checks which requests count as attempts at a
// second factor, the lines logged, and that no secret reaches the log.
func TestWebAuthnLimitsAndLogs(t *testing.T) {
	api := newMFAAPI(t, "sqlite", config.MFAOff)
	alice, _ := api.login("alice@example.com", mfaPassword)
	logs := captureLog(t)
	key := newSoftKey(t)
	registration := api.beginKey(alice)
	expect(t, api.finishKey(alice, registration.State, json.RawMessage(`{}`), "Key"), 400, "mfa_code_invalid")
	w := api.finishKey(alice, registration.State, key.create(registration.Options), "Key")
	expect(t, w, 201, "")
	created := decodeJSON[webauthnCreatedResponse](t, w)
	ch := api.pending("alice@example.com")
	begin := api.beginSignIn(ch.Token)
	forged := newSoftKey(t)
	forged.id = key.id
	noSession(t, api.finishSignIn(ch.Token, begin.State, forged.get(begin.Options)), 401, "mfa_code_invalid")
	response := key.get(begin.Options)
	access, _ := api.session(api.finishSignIn(ch.Token, begin.State, response))
	noSession(t, api.finishSignIn(ch.Token, begin.State, response), 401, "mfa_code_invalid")
	noSession(t, api.finishSignIn(ch.Token, "v1.AAAA", response), 400, "mfa_state_invalid")
	expect(t, api.call("/api/v1/mfa/webauthn/"+created.Credential.ID+"/delete", access, map[string]string{"password": mfaPassword}), 204, "")

	got := logs.String()
	for line, n := range map[string]int{
		"auth: mfa: webauthn failed user=alice-id\n":   3,
		"auth: mfa: webauthn enrolled user=alice-id\n": 1,
		"auth: mfa: webauthn deleted user=alice-id\n":  1,
	} {
		if strings.Count(got, line) != n {
			t.Errorf("%q: %d times, want %d", strings.TrimSpace(line), strings.Count(got, line), n)
		}
	}
	secrets := []string{mfaPassword, "alice@example.com", ch.Token, access, alice, registration.State, begin.State, key.options(registration.Options).Challenge, key.options(begin.Options).Challenge, b64(key.id), string(response)}
	secrets = append(secrets, created.RecoveryCodes...)
	for _, secret := range secrets {
		if secret != "" && strings.Contains(got, secret) {
			t.Errorf("the log contains a secret of %d bytes", len(secret))
		}
	}
	if t.Failed() {
		t.Fatalf("%d lines logged", strings.Count(got, "\n"))
	}

	// Five attempts per user in 15 minutes: the sign-in with a key counts
	// with the codes, and the sixth is rejected even with the right key.
	api.addKey(alice, key, "Key")
	real := func(policy string) {
		api.use(policy, true)
		api.srv.limiter = newKeyLimiter(1000000, time.Minute)
		api.srv.mfaPeerLimiter = newKeyLimiter(1000000, time.Minute)
		api.handler = api.srv.handler()
	}
	real(config.MFAOff)
	ch = api.pending("alice@example.com")
	for i := 0; i < 4; i++ {
		begin = api.beginSignIn(ch.Token)
		noSession(t, api.finishSignIn(ch.Token, begin.State, forged.get(begin.Options)), 401, "mfa_code_invalid")
	}
	noSession(t, api.post("/auth/mfa/totp", map[string]string{"mfa_token": ch.Token, "code": "000000"}), 401, "mfa_code_invalid")
	begin = api.beginSignIn(ch.Token)
	w = api.finishSignIn(ch.Token, begin.State, key.get(begin.Options))
	noSession(t, w, 429, "rate_limited")
	if w.Header().Get("Retry-After") != "900" {
		t.Fatalf("Retry-After %q", w.Header().Get("Retry-After"))
	}
	// Beginning is not an attempt, and nothing was spent or stored.
	api.beginSignIn(ch.Token)
	if row, _ := api.storedKey("alice-id", api.status(alice).WebAuthn[0].ID); row.LastUsedAt.Valid {
		t.Fatal("a rejected attempt was stored")
	}

	// The registrations are not attempts: there is no secret to guess.
	real(config.MFAAll)
	carol := api.pending("carol@example.com")
	started := api.begun(api.send("/auth/mfa/enroll/webauthn/begin", map[string]any{"mfa_token": carol.Token}))
	for i := 0; i < 8; i++ {
		noSession(t, api.send("/auth/mfa/enroll/webauthn/finish", map[string]any{"mfa_token": carol.Token, "state": started.State, "credential": map[string]any{}, "name": "Key"}), 401, "mfa_code_invalid")
	}
	api.session(api.send("/auth/mfa/enroll/webauthn/finish", map[string]any{"mfa_token": carol.Token, "state": started.State, "credential": json.RawMessage(newSoftKey(t).create(started.Options)), "name": "Key"}))
	registration = api.beginKey(alice)
	for i := 0; i < 8; i++ {
		expect(t, api.finishKey(alice, registration.State, json.RawMessage(`{}`), "Key"), 400, "mfa_code_invalid")
	}
	expect(t, api.finishKey(alice, registration.State, newSoftKey(t).create(registration.Options), "Second"), 201, "")

	// The peer limit covers the WebAuthn endpoints of /auth/mfa/* too.
	api.use(config.MFAOff, true)
	bodies := []struct {
		path string
		body map[string]any
	}{
		{"/auth/mfa/webauthn/begin", map[string]any{"mfa_token": "x"}},
		{"/auth/mfa/webauthn/finish", map[string]any{"mfa_token": "x", "state": "s", "credential": map[string]any{}}},
		{"/auth/mfa/enroll/webauthn/begin", map[string]any{"mfa_token": "x"}},
		{"/auth/mfa/enroll/webauthn/finish", map[string]any{"mfa_token": "x", "state": "s", "credential": map[string]any{}, "name": "Key"}},
	}
	for i := 0; i < 10; i++ {
		noSession(t, api.send(bodies[i%len(bodies)].path, bodies[i%len(bodies)].body), 401, "mfa_token_invalid")
	}
	for _, b := range bodies {
		w = api.send(b.path, b.body)
		noSession(t, w, 429, "rate_limited")
		if w.Header().Get("Retry-After") != "60" {
			t.Fatalf("%s: Retry-After %q", b.path, w.Header().Get("Retry-After"))
		}
	}
}
