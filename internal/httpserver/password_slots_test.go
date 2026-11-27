package httpserver

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/i2bskn/astovia/internal/config"
)

// TestPasswordSlots checks that the key derivations wait for a slot, that
// a request refused for want of one is a 503 with Retry-After, and that
// such a refusal counts as no attempt of the sign-in or the password
// confirmation and spends no link.
func TestPasswordSlots(t *testing.T) {
	mfaDrivers(t, func(t *testing.T, driver string) {
		// The limits of the sign-ins per address and of the password
		// confirmations are on; those per client address are lifted.
		api := newMFAAPI(t, driver, config.MFAOff)
		api.use(config.MFAOff, true)
		api.srv.limiter = newKeyLimiter(10000, time.Minute)
		api.srv.accountLimiter = newKeyLimiter(10000, time.Minute)
		alice, _ := api.login("alice@example.com", "correct-password-123")
		w := api.do("POST", "/api/v1/invitations", alice, `{"email":"dave@example.com","role":"member"}`)
		expect(t, w, 201, "")
		daveToken := fragmentToken(t, decodeJSON[createdInvitationResponse](t, w).URL, usersOrigin+"/invite#")

		slots := &passwordSlots{slots: make(chan struct{}, 1), wait: time.Millisecond}
		api.srv.passwordSlots = slots
		slots.slots <- struct{}{}
		busy := func(w *httptest.ResponseRecorder) {
			t.Helper()
			expect(t, w, 503, "unavailable")
			if got := w.Header().Values("Retry-After"); len(got) != 1 || got[0] != "1" {
				t.Fatalf("Retry-After: %v", got)
			}
		}
		// More attempts than the limits allow are all refused uncounted.
		for range 12 {
			w = api.public("/auth/login", `{"email":"alice@example.com","password":"wrong-password-123"}`, "")
			busy(w)
			w = api.call("/api/v1/mfa/recovery-codes", alice, map[string]string{"password": "wrong-password-123"})
			busy(w)
		}
		w = api.public("/auth/invitations/accept", `{"token":"`+daveToken+`","password":"dave-password-123"}`, "")
		busy(w)
		// A password the derivation would refuse is still refused first.
		expect(t, api.public("/auth/invitations/accept", `{"token":"`+daveToken+`","password":"short"}`, ""), 400, "invalid_request")

		<-slots.slots
		api.login("alice@example.com", "correct-password-123")
		expect(t, api.call("/api/v1/mfa/recovery-codes", alice, map[string]string{"password": "correct-password-123"}), 409, "mfa_not_enrolled")
		expect(t, api.public("/auth/invitations/accept", `{"token":"`+daveToken+`","password":"dave-password-123"}`, ""), 200, "")
		if len(slots.slots) != 0 {
			t.Fatalf("%d slots held after the requests", len(slots.slots))
		}
	})
}

// TestPasswordSlotLimit checks the default number of slots and that a
// waiting request gets a slot as soon as one is released.
func TestPasswordSlotLimit(t *testing.T) {
	p := newPasswordSlots()
	if cap(p.slots) < 2 || p.wait != passwordSlotWait {
		t.Fatalf("slots: %d, wait %v", cap(p.slots), p.wait)
	}
	for range cap(p.slots) {
		if !p.acquire(t.Context()) {
			t.Fatal("a free slot was refused")
		}
	}
	p.wait = 10 * time.Millisecond
	if p.acquire(t.Context()) {
		t.Fatal("a slot over the limit")
	}
	p.wait = time.Minute
	go func() {
		time.Sleep(10 * time.Millisecond)
		p.release()
	}()
	if !p.acquire(t.Context()) {
		t.Fatal("a released slot was not handed over")
	}
}

// TestPasswordBodyLimits checks that a password of the longest length
// whose every byte is escaped six times in JSON fits the bodies that carry
// one.
func TestPasswordBodyLimits(t *testing.T) {
	mfaDrivers(t, func(t *testing.T, driver string) {
		api := newMFAAPI(t, driver, config.MFAOff)
		alice, _ := api.login("alice@example.com", "correct-password-123")
		w := api.do("POST", "/api/v1/invitations", alice, `{"email":"dave@example.com","role":"member"}`)
		expect(t, w, 201, "")
		daveToken := fragmentToken(t, decodeJSON[createdInvitationResponse](t, w).URL, usersOrigin+"/invite#")

		password := strings.Repeat("\x01", 1024)
		body := mustJSON(t, map[string]string{"token": daveToken, "password": password})
		if len(body) < 6*1024 {
			t.Fatalf("the password is not escaped: %d bytes", len(body))
		}
		expect(t, api.public("/auth/invitations/accept", string(body), ""), 200, "")
		dave, _ := api.session(api.post("/auth/login", map[string]string{"email": "dave@example.com", "password": password}))
		expect(t, api.call("/api/v1/mfa/recovery-codes", dave, map[string]string{"password": password}), 409, "mfa_not_enrolled")
	})
}
