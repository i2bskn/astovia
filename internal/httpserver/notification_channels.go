package httpserver

import (
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/i2bskn/astovia/internal/auth"
	"github.com/i2bskn/astovia/internal/notify"
	"github.com/i2bskn/astovia/internal/store"
)

// slackWebhookPattern is the accepted form of a Slack Incoming Webhook URL.
var slackWebhookPattern = regexp.MustCompile(`^https://hooks\.slack\.com/services/[A-Za-z0-9]+/[A-Za-z0-9]+/[A-Za-z0-9]+$`)

// maxSlackWebhookLength bounds the webhook URL in bytes.
const maxSlackWebhookLength = 512

// notificationRoutes lists the kinds sent to each channel.
type notificationRoutes struct {
	WebPush []string `json:"webpush"`
	Slack   []string `json:"slack"`
	Email   []string `json:"email"`
}

// notificationChannelsResponse is the user's channels. The Slack webhook URL
// is never returned.
type notificationChannelsResponse struct {
	Routes notificationRoutes `json:"routes"`
	Slack  struct {
		Configured bool    `json:"configured"`
		LastError  *string `json:"last_error"`
	} `json:"slack"`
	Email struct {
		Available bool    `json:"available"`
		Provider  *string `json:"provider"`
		Address   *string `json:"address"`
	} `json:"email"`
}

func (s *Server) notificationChannelsResponse(u store.User, c store.NotificationChannels) notificationChannelsResponse {
	var out notificationChannelsResponse
	out.Routes = notificationRoutes{WebPush: c.WebPush, Slack: c.Slack, Email: c.Email}
	out.Slack.Configured = c.SlackWebhookSealed.Valid
	if c.SlackLastError.Valid {
		v := c.SlackLastError.String
		out.Slack.LastError = &v
	}
	if s.cfg.Mail != nil {
		provider, address := s.cfg.Mail.Provider, u.Email
		out.Email.Available, out.Email.Provider, out.Email.Address = true, &provider, &address
	}
	return out
}

// writeNotificationChannels reads the user's channels and writes them.
func (s *Server) writeNotificationChannels(w http.ResponseWriter, r *http.Request, u store.User, op string) {
	c, err := s.auth.Store.NotificationChannels(r.Context(), u.ID)
	if err != nil {
		integrationError(w, op, u.ID, err)
		return
	}
	writeJSON(w, 200, s.notificationChannelsResponse(u, c))
}

func (s *Server) handleGetNotificationChannels(w http.ResponseWriter, r *http.Request) {
	u, ok := userFrom(r.Context())
	if !ok {
		s.authError(w, r, store.ErrUnauthorized)
		return
	}
	s.writeNotificationChannels(w, r, u, "channels get")
}

// decodeKinds decodes an array of routable kinds without null, duplicates
// or unknown kinds, and returns it in the order of store.RoutableKinds.
func decodeKinds(raw json.RawMessage) ([]string, bool) {
	var list []json.RawMessage
	if raw == nil || isNull(raw) || !decodeValue(raw, &list) {
		return nil, false
	}
	seen := map[string]bool{}
	for _, item := range list {
		v, ok := decodeString(item)
		if !ok || seen[v] || !slices.Contains(store.RoutableKinds, v) {
			return nil, false
		}
		seen[v] = true
	}
	kinds := make([]string, 0, len(seen))
	for v := range seen {
		kinds = append(kinds, v)
	}
	return store.SortKinds(kinds), true
}

func (s *Server) handlePutNotificationChannels(w http.ResponseWriter, r *http.Request) {
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
		Routes json.RawMessage `json:"routes"`
	}
	if !decode(w, r, &in, integrationBodyLimit) {
		return
	}
	var routes struct {
		WebPush json.RawMessage `json:"webpush"`
		Slack   json.RawMessage `json:"slack"`
		Email   json.RawMessage `json:"email"`
	}
	invalid := in.Routes == nil || isNull(in.Routes) || !decodeValue(in.Routes, &routes)
	var webpush, slack, email []string
	ok1, ok2, ok3 := false, false, false
	if !invalid {
		webpush, ok1 = decodeKinds(routes.WebPush)
		slack, ok2 = decodeKinds(routes.Slack)
		email, ok3 = decodeKinds(routes.Email)
	}
	if invalid || !ok1 || !ok2 || !ok3 {
		fail(w, 400, "invalid_request", "routes is invalid.")
		return
	}
	// Without a mail service the e-mail kinds can only be reduced, so that a
	// user can still save the other channels after the operator removed it.
	if s.cfg.Mail == nil && len(email) > 0 {
		current, err := s.auth.Store.NotificationChannels(r.Context(), u.ID)
		if err != nil {
			integrationError(w, "channels put", u.ID, err)
			return
		}
		for _, k := range email {
			if !slices.Contains(current.Email, k) {
				fail(w, 400, "mail_unavailable", "Email notifications are not configured on this server.")
				return
			}
		}
	}
	err := s.auth.Store.SaveNotificationRoutes(r.Context(), u.ID, sid, webpush, slack, email, s.auth.Now().Unix())
	switch {
	case err == nil:
	case errors.Is(err, store.ErrUnauthorized):
		s.authError(w, r, err)
		return
	default:
		integrationError(w, "channels put", u.ID, err)
		return
	}
	s.writeNotificationChannels(w, r, u, "channels put")
}

func (s *Server) handlePutSlackWebhook(w http.ResponseWriter, r *http.Request) {
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
		URL json.RawMessage `json:"url"`
	}
	if !decode(w, r, &in, integrationBodyLimit) {
		return
	}
	var webhook string
	if in.URL != nil {
		webhook, ok = decodeString(in.URL)
		webhook = strings.TrimSpace(webhook)
	}
	if in.URL == nil || !ok || len(webhook) > maxSlackWebhookLength || !slackWebhookPattern.MatchString(webhook) {
		fail(w, 400, "invalid_request", "url must be a Slack Incoming Webhook URL (https://hooks.slack.com/services/...).")
		return
	}
	sealed, err := s.box.Seal(notify.SlackWebhookPurpose(u.ID), webhook)
	if err != nil {
		integrationError(w, "slack webhook seal", u.ID, err)
		return
	}
	err = s.auth.Store.SetSlackWebhook(r.Context(), u.ID, sid, sealed, s.auth.Now().Unix())
	switch {
	case err == nil:
	case errors.Is(err, store.ErrUnauthorized):
		s.authError(w, r, err)
		return
	default:
		integrationError(w, "slack webhook put", u.ID, err)
		return
	}
	s.writeNotificationChannels(w, r, u, "slack webhook put")
}

func (s *Server) handleDeleteSlackWebhook(w http.ResponseWriter, r *http.Request) {
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
	err := s.auth.Store.ClearSlackWebhook(r.Context(), u.ID, sid, s.auth.Now().Unix())
	switch {
	case err == nil:
	case errors.Is(err, store.ErrUnauthorized):
		s.authError(w, r, err)
		return
	default:
		integrationError(w, "slack webhook delete", u.ID, err)
		return
	}
	w.WriteHeader(204)
}

// handleChannelTest queues a test notification on one channel. The limit of
// three per minute is shared with /api/v1/push/test and every channel.
func (s *Server) handleChannelTest(w http.ResponseWriter, r *http.Request) {
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
		Channel json.RawMessage `json:"channel"`
	}
	if !decode(w, r, &in, integrationBodyLimit) {
		return
	}
	var channel string
	if in.Channel != nil {
		channel, ok = decodeString(in.Channel)
	}
	if in.Channel == nil || !ok || (channel != store.ChannelWebPush && channel != store.ChannelSlack && channel != store.ChannelEmail) {
		fail(w, 400, "invalid_request", "channel must be webpush, slack or email.")
		return
	}
	if channel == store.ChannelEmail && s.cfg.Mail == nil {
		fail(w, 409, "mail_unavailable", "Email notifications are not configured on this server.")
		return
	}
	if !s.pushTestLimiter.allowKey(u.ID) {
		w.Header().Set("Retry-After", "60")
		fail(w, 429, "rate_limited", "Too many test notifications. Try again later.")
		return
	}
	id, err := auth.Random()
	if err != nil {
		integrationError(w, "channel test", u.ID, err)
		return
	}
	now := s.auth.Now().Unix()
	n := store.Notification{ID: id, UserID: u.ID, Kind: store.NotificationTest, DedupeKey: "test:" + id, Title: "Astovia のテスト通知", Body: "この送り先で通知を受け取れます。", URL: "/settings/notifications", ScheduledAt: now, CreatedAt: now, Channel: channel}
	if channel == store.ChannelWebPush {
		n.Body = pushTestBody
	}
	err = s.auth.Store.CreateTestNotification(r.Context(), n, sid, now)
	switch {
	case err == nil:
	case errors.Is(err, store.ErrUnauthorized):
		s.authError(w, r, err)
		return
	case errors.Is(err, store.ErrNoSubscription) && channel == store.ChannelWebPush:
		fail(w, 409, "no_subscription", "No device is registered for notifications.")
		return
	case errors.Is(err, store.ErrNoSubscription):
		fail(w, 409, "no_destination", "Nothing is set up for this channel.")
		return
	default:
		integrationError(w, "channel test", u.ID, err)
		return
	}
	writeJSON(w, 202, map[string]bool{"queued": true})
}
