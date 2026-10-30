package notify

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"syscall"
	"time"

	"github.com/SherClockHolmes/webpush-go"
	"github.com/i2bskn/astovia/internal/oauth"
	"github.com/i2bskn/astovia/internal/store"
)

const pushTimeout = 10 * time.Second

// errBlockedAddress is returned when a push endpoint resolves to an address
// that is not public.
var errBlockedAddress = errors.New("push endpoint address is not allowed")

// NewPushClient returns the HTTP client used to reach push services. Push
// endpoints are chosen by browsers (and so by whoever registers a
// subscription), so the client applies the same address check as Client ID
// Metadata Document fetching: only public addresses, no redirects, no proxy.
func NewPushClient() *http.Client {
	return newPushClient(oauth.AllowedCIMDAddr, nil)
}

// newPushClient checks the address of every connection after name
// resolution. Tests pass their own allow function and TLS config.
func newPushClient(allow func(netip.Addr) bool, tlsConfig *tls.Config) *http.Client {
	dialer := &net.Dialer{Timeout: pushTimeout, Control: func(_, address string, _ syscall.RawConn) error {
		ap, err := netip.ParseAddrPort(address)
		if err != nil || !allow(ap.Addr()) {
			return errBlockedAddress
		}
		return nil
	}}
	return &http.Client{
		Timeout: pushTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Transport: &http.Transport{
			Proxy:                 nil,
			DialContext:           dialer.DialContext,
			TLSClientConfig:       tlsConfig,
			TLSHandshakeTimeout:   pushTimeout,
			ResponseHeaderTimeout: pushTimeout,
			DisableKeepAlives:     true,
		},
	}
}

// WebPush sends messages to every Web Push subscription of the user.
type WebPush struct {
	Store *store.Store
	Keys  *VAPID
	// Subscriber is the VAPID "sub": an https URL, or an e-mail address to
	// which the library adds "mailto:".
	Subscriber string
	Client     *http.Client
	Now        func() time.Time
}

// NewWebPush returns the Web Push channel. The VAPID subscriber is publicURL
// when it is https, and mailto:webpush@localhost otherwise (development; the
// Apple push service may reject it).
func NewWebPush(s *store.Store, keys *VAPID, publicURL string, now func() time.Time) *WebPush {
	subscriber := "webpush@localhost"
	if strings.HasPrefix(publicURL, "https://") {
		subscriber = publicURL
	}
	return &WebPush{Store: s, Keys: keys, Subscriber: subscriber, Client: NewPushClient(), Now: now}
}

func (w *WebPush) Name() string { return "webpush" }

type pushPayload struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	URL   string `json:"url"`
	Tag   string `json:"tag"`
}

// ttl returns how long the push service keeps an undelivered message.
func ttl(kind string) int {
	switch kind {
	case store.NotificationStale, store.NotificationDueSoon, store.NotificationOverdue:
		return 21600
	case store.NotificationDigest:
		return 43200
	}
	return 3600
}

func urgency(kind string) webpush.Urgency {
	if kind == store.NotificationDetected {
		return webpush.UrgencyHigh
	}
	return webpush.UrgencyNormal
}

// Send encrypts the message and posts it to each subscription. A
// subscription the push service reports as gone (404, 410) or as not ours
// (403, for example after a VAPID key change) is deleted. It succeeds when at
// least one subscription accepted the message.
func (w *WebPush) Send(ctx context.Context, userID string, m Message) (int, error) {
	subs, err := w.Store.PushSubscriptions(ctx, userID)
	if err != nil {
		return 0, err
	}
	if len(subs) == 0 {
		return 0, store.ErrNoSubscription
	}
	keys, err := w.Keys.Keys(ctx)
	if err != nil {
		return 0, err
	}
	payload, err := json.Marshal(pushPayload{Title: truncate(m.Title, 80), Body: truncate(m.Body, 200), URL: clickURL(m.URL), Tag: m.Tag})
	if err != nil {
		return 0, err
	}
	opts := &webpush.Options{HTTPClient: w.Client, Subscriber: w.Subscriber, TTL: ttl(m.Kind), Urgency: urgency(m.Kind), VAPIDPublicKey: keys.Public, VAPIDPrivateKey: keys.Private}
	sent := 0
	var failure error
	for _, sub := range subs {
		resp, err := webpush.SendNotificationWithContext(ctx, payload, &webpush.Subscription{Endpoint: sub.Endpoint, Keys: webpush.Keys{Auth: sub.Auth, P256dh: sub.P256dh}}, opts)
		if err != nil {
			failure = err
			continue
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		switch resp.StatusCode {
		case http.StatusOK, http.StatusCreated, http.StatusAccepted:
			sent++
			if err = w.Store.TouchPushSubscription(ctx, sub.ID, w.Now().Unix()); err != nil {
				log.Printf("web push: user=%s: record delivery: %v", userID, err)
			}
		case http.StatusNotFound, http.StatusGone, http.StatusForbidden:
			if err = w.Store.RemovePushSubscription(ctx, sub.ID); err != nil {
				failure = err
			}
		case http.StatusRequestEntityTooLarge:
			// The payload is bounded above; this is a bug to fix here.
			log.Printf("web push: user=%s: payload too large", userID)
			failure = fmt.Errorf("push service status %d", resp.StatusCode)
		default:
			failure = fmt.Errorf("push service status %d", resp.StatusCode)
		}
	}
	if sent > 0 {
		return sent, nil
	}
	if failure == nil {
		// Every subscription was gone.
		return 0, store.ErrNoSubscription
	}
	return 0, failure
}

// clickURL returns the path opened when a notification is clicked. URLs saved
// before the web UI moved from /admin to / are rewritten, and anything that is
// not a plain same-origin path becomes /.
func clickURL(u string) string {
	if u == "/admin" {
		u = "/"
	} else if rest, ok := strings.CutPrefix(u, "/admin/"); ok {
		u = "/" + rest
	}
	if !strings.HasPrefix(u, "/") || strings.HasPrefix(u, "//") {
		return "/"
	}
	for _, c := range u {
		if c < 0x20 || c == 0x7f || c == '\\' {
			return "/"
		}
	}
	return u
}
