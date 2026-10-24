package httpserver

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/i2bskn/astovia/internal/auth"
	"github.com/i2bskn/astovia/internal/config"
	"github.com/i2bskn/astovia/internal/oauth"
	"github.com/i2bskn/astovia/internal/store"
)

// pushSubscriptionResponse is a registered device. The endpoint and keys are
// never returned.
type pushSubscriptionResponse struct {
	ID         string  `json:"id"`
	Label      string  `json:"label"`
	CreatedAt  string  `json:"created_at"`
	LastUsedAt *string `json:"last_used_at"`
}

type pushSubscriptionListResponse struct {
	Subscriptions []pushSubscriptionResponse `json:"subscriptions"`
}

func toPushSubscriptionResponse(p store.PushSubscription) pushSubscriptionResponse {
	out := pushSubscriptionResponse{ID: p.ID, Label: p.Label, CreatedAt: formatTime(p.CreatedAt)}
	if p.LastUsedAt.Valid {
		v := formatTime(p.LastUsedAt.Int64)
		out.LastUsedAt = &v
	}
	return out
}

// validPushEndpoint accepts an https URL of at most 2048 bytes of printable
// ASCII whose host is a DNS name (no IP literal), on the default port, with
// no user information or fragment. The server sends requests to it, so the
// rules follow internal/oauth's URL rules; the address it resolves to is
// checked again when sending.
func validPushEndpoint(raw string) bool {
	if len(raw) < 1 || len(raw) > 2048 || strings.Contains(raw, "#") {
		return false
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] <= 0x20 || raw[i] >= 0x7f {
			return false
		}
	}
	rest, ok := strings.CutPrefix(raw, "https://")
	if !ok {
		return false
	}
	// url.Parse decodes percent-encoded hosts, so look at the raw authority.
	if i := strings.IndexAny(rest, "/?"); i >= 0 {
		rest = rest[:i]
	}
	if strings.ContainsAny(rest, "%@[") {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" || u.User != nil || u.Host == "" || strings.HasSuffix(u.Host, ":") {
		return false
	}
	if p := u.Port(); p != "" && p != "443" {
		return false
	}
	host := u.Hostname()
	if _, err = netip.ParseAddr(host); err == nil {
		return false
	}
	return config.ValidDNSHost(host)
}

// decodePushKey decodes base64url with or without padding.
func decodePushKey(v string) ([]byte, bool) {
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(v, "="))
	return b, err == nil
}

// validTimezone reports whether v names an IANA time zone. "Local" and the
// empty name (UTC) are not accepted, so that the meaning does not depend on
// the server.
func validTimezone(v string) bool {
	if v == "" || v == "Local" || len(v) > 64 {
		return false
	}
	_, err := time.LoadLocation(v)
	return err == nil
}

func (s *Server) handlePushPublicKey(w http.ResponseWriter, r *http.Request) {
	u, ok := userFrom(r.Context())
	if !ok {
		s.authError(w, r, store.ErrUnauthorized)
		return
	}
	keys, err := s.vapid.Keys(r.Context())
	if err != nil {
		integrationError(w, "push key", u.ID, err)
		return
	}
	writeJSON(w, 200, map[string]string{"public_key": keys.Public})
}

func (s *Server) handleListPushSubscriptions(w http.ResponseWriter, r *http.Request) {
	u, ok := userFrom(r.Context())
	if !ok {
		s.authError(w, r, store.ErrUnauthorized)
		return
	}
	subs, err := s.auth.Store.PushSubscriptions(r.Context(), u.ID)
	if err != nil {
		integrationError(w, "push list", u.ID, err)
		return
	}
	out := pushSubscriptionListResponse{Subscriptions: make([]pushSubscriptionResponse, 0, len(subs))}
	for _, p := range subs {
		out.Subscriptions = append(out.Subscriptions, toPushSubscriptionResponse(p))
	}
	writeJSON(w, 200, out)
}

func (s *Server) handleCreatePushSubscription(w http.ResponseWriter, r *http.Request) {
	u, ok := userFrom(r.Context())
	if !ok {
		s.authError(w, r, store.ErrUnauthorized)
		return
	}
	sid, ok := sessionFrom(r.Context())
	if !ok {
		s.authError(w, r, store.ErrUnauthorized)
		return
	}
	var in struct {
		Endpoint string `json:"endpoint"`
		Keys     struct {
			P256dh string `json:"p256dh"`
			Auth   string `json:"auth"`
		} `json:"keys"`
		Label    *string `json:"label"`
		Timezone *string `json:"timezone"`
	}
	if !decode(w, r, &in, integrationBodyLimit) {
		return
	}
	if !validPushEndpoint(in.Endpoint) {
		fail(w, 400, "invalid_request", "endpoint must be an https URL of a push service.")
		return
	}
	p256dh, ok1 := decodePushKey(in.Keys.P256dh)
	authKey, ok2 := decodePushKey(in.Keys.Auth)
	if !ok1 || !ok2 || len(p256dh) != 65 || p256dh[0] != 4 || len(authKey) != 16 {
		fail(w, 400, "invalid_request", "keys must hold a P-256 p256dh key and a 16-byte auth secret in base64url.")
		return
	}
	label := ""
	if in.Label != nil {
		label = oauth.ClientName(*in.Label, "")
		if utf8.RuneCountInString(label) > 64 {
			label = string([]rune(label)[:64])
		}
	}
	timezone := ""
	if in.Timezone != nil && validTimezone(*in.Timezone) {
		timezone = *in.Timezone
	}
	id, err := auth.Random()
	if err != nil {
		integrationError(w, "push create", u.ID, err)
		return
	}
	p := store.PushSubscription{ID: id, UserID: u.ID, Endpoint: in.Endpoint, P256dh: base64.RawURLEncoding.EncodeToString(p256dh), Auth: base64.RawURLEncoding.EncodeToString(authKey), Label: label}
	saved, created, err := s.auth.Store.SavePushSubscription(r.Context(), p, timezone, sid, s.auth.Now().Unix())
	switch {
	case err == nil:
	case errors.Is(err, store.ErrUnauthorized):
		s.authError(w, r, err)
		return
	case errors.Is(err, store.ErrTooManySubscriptions):
		fail(w, 409, "too_many_subscriptions", "You can register at most 20 devices. Remove one first.")
		return
	default:
		integrationError(w, "push create", u.ID, err)
		return
	}
	status := 200
	if created {
		status = 201
	}
	writeJSON(w, status, toPushSubscriptionResponse(saved))
}

func (s *Server) handleDeletePushSubscription(w http.ResponseWriter, r *http.Request) {
	u, ok := userFrom(r.Context())
	if !ok {
		s.authError(w, r, store.ErrUnauthorized)
		return
	}
	id := r.PathValue("id")
	if !itemIDPattern.MatchString(id) {
		fail(w, 404, "not_found", "Push subscription not found.")
		return
	}
	err := s.auth.Store.DeletePushSubscription(r.Context(), u.ID, id)
	if errors.Is(err, store.ErrSubscriptionNotFound) {
		fail(w, 404, "not_found", "Push subscription not found.")
		return
	}
	if err != nil {
		integrationError(w, "push delete", u.ID, err)
		return
	}
	w.WriteHeader(204)
}

func (s *Server) handlePushTest(w http.ResponseWriter, r *http.Request) {
	u, ok := userFrom(r.Context())
	if !ok {
		s.authError(w, r, store.ErrUnauthorized)
		return
	}
	sid, ok := sessionFrom(r.Context())
	if !ok {
		s.authError(w, r, store.ErrUnauthorized)
		return
	}
	if !s.pushTestLimiter.allowKey(u.ID) {
		w.Header().Set("Retry-After", "60")
		fail(w, 429, "rate_limited", "Too many test notifications. Try again later.")
		return
	}
	id, err := auth.Random()
	if err != nil {
		integrationError(w, "push test", u.ID, err)
		return
	}
	now := s.auth.Now().Unix()
	n := store.Notification{ID: id, UserID: u.ID, Kind: store.NotificationTest, DedupeKey: "test:" + id, Title: "Astovia のテスト通知", Body: "この端末で通知を受け取れます。", URL: "/settings/notifications", ScheduledAt: now, CreatedAt: now}
	err = s.auth.Store.CreateTestNotification(r.Context(), n, sid, now)
	switch {
	case err == nil:
	case errors.Is(err, store.ErrUnauthorized):
		s.authError(w, r, err)
		return
	case errors.Is(err, store.ErrNoSubscription):
		fail(w, 409, "no_subscription", "No device is registered for notifications.")
		return
	default:
		integrationError(w, "push test", u.ID, err)
		return
	}
	writeJSON(w, 202, map[string]bool{"queued": true})
}

type notificationSettingsResponse struct {
	Timezone   string `json:"timezone"`
	DigestHour int64  `json:"digest_hour"`
	StaleDays  int64  `json:"stale_days"`
}

func (s *Server) handleGetNotificationSettings(w http.ResponseWriter, r *http.Request) {
	u, ok := userFrom(r.Context())
	if !ok {
		s.authError(w, r, store.ErrUnauthorized)
		return
	}
	n, err := s.auth.Store.NotificationSettings(r.Context(), u.ID)
	if err != nil {
		integrationError(w, "settings get", u.ID, err)
		return
	}
	writeJSON(w, 200, notificationSettingsResponse{Timezone: n.Timezone, DigestHour: n.DigestHour, StaleDays: n.StaleDays})
}

// decodeInt decodes a JSON integer in [lo, hi]; null, fractions and strings
// are rejected.
func decodeInt(raw json.RawMessage, lo, hi int64) (int64, bool) {
	var v int64
	if raw == nil || isNull(raw) || !decodeValue(raw, &v) || v < lo || v > hi {
		return 0, false
	}
	return v, true
}

func (s *Server) handlePutNotificationSettings(w http.ResponseWriter, r *http.Request) {
	u, ok := userFrom(r.Context())
	if !ok {
		s.authError(w, r, store.ErrUnauthorized)
		return
	}
	var in struct {
		Timezone   json.RawMessage `json:"timezone"`
		DigestHour json.RawMessage `json:"digest_hour"`
		StaleDays  json.RawMessage `json:"stale_days"`
	}
	if !decode(w, r, &in, integrationBodyLimit) {
		return
	}
	var timezone string
	if in.Timezone != nil {
		timezone, ok = decodeString(in.Timezone)
	}
	if in.Timezone == nil || !ok || !validTimezone(timezone) {
		fail(w, 400, "invalid_request", "timezone must be an IANA time zone name such as Asia/Tokyo.")
		return
	}
	hour, ok := decodeInt(in.DigestHour, 0, 23)
	if !ok {
		fail(w, 400, "invalid_request", "digest_hour must be an integer from 0 to 23.")
		return
	}
	days, ok := decodeInt(in.StaleDays, 1, 30)
	if !ok {
		fail(w, 400, "invalid_request", "stale_days must be an integer from 1 to 30.")
		return
	}
	n := store.NotificationSettings{UserID: u.ID, Timezone: timezone, DigestHour: hour, StaleDays: days, UpdatedAt: s.auth.Now().Unix()}
	if err := s.auth.Store.SaveNotificationSettings(r.Context(), n); err != nil {
		integrationError(w, "settings put", u.ID, err)
		return
	}
	writeJSON(w, 200, notificationSettingsResponse{Timezone: n.Timezone, DigestHour: n.DigestHour, StaleDays: n.StaleDays})
}
