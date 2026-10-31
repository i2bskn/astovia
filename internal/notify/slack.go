package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/i2bskn/astovia/internal/secretbox"
	"github.com/i2bskn/astovia/internal/store"
)

// Slack sends messages to the Incoming Webhook the user registered. The
// webhook URL is a secret: it is never put in an error or a log.
type Slack struct {
	Store     *store.Store
	Box       *secretbox.Box
	PublicURL string
	Client    *http.Client
}

// NewSlack returns the Slack channel. Its client reaches public addresses
// only, without redirects or a proxy, like the Web Push client.
func NewSlack(s *store.Store, box *secretbox.Box, publicURL string) *Slack {
	return &Slack{Store: s, Box: box, PublicURL: publicURL, Client: NewPushClient()}
}

func (s *Slack) Name() string { return store.ChannelSlack }

// Send posts the message to the user's webhook. A webhook that Slack
// reports as gone or not allowed (403, 404, 410), or that cannot be
// decrypted, is marked invalid and ErrDestinationInvalid is returned. A 429
// returns ErrRateLimited; other failures are returned for a retry.
func (s *Slack) Send(ctx context.Context, userID string, m Message) (int, error) {
	c, err := s.Store.NotificationChannels(ctx, userID)
	if err != nil {
		return 0, err
	}
	if !c.SlackReady() {
		return 0, store.ErrNoSubscription
	}
	sealed := c.SlackWebhookSealed.String
	webhook, err := s.Box.Open(SlackWebhookPurpose(userID), sealed)
	if err != nil {
		s.markInvalid(ctx, userID, sealed, SlackWebhookUndecryptable)
		return 0, fmt.Errorf("slack webhook cannot be decrypted: %w", ErrDestinationInvalid)
	}
	body, err := json.Marshal(map[string]string{"text": slackText(m, s.PublicURL)})
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, pushTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhook, bytes.NewReader(body))
	if err != nil {
		// The error would contain the URL.
		return 0, errors.New("slack webhook: invalid request")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.Client.Do(req)
	if err != nil {
		// A *url.Error contains the URL; only the fact is returned.
		return 0, errors.New("slack webhook: request failed")
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	_ = resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return 1, nil
	case http.StatusForbidden, http.StatusNotFound, http.StatusGone:
		s.markInvalid(ctx, userID, sealed, SlackWebhookInvalid)
		return 0, fmt.Errorf("slack webhook status %d: %w", resp.StatusCode, ErrDestinationInvalid)
	case http.StatusTooManyRequests:
		return 0, fmt.Errorf("slack webhook status %d: %w", resp.StatusCode, ErrRateLimited)
	}
	// Redirects are not followed: hooks.slack.com does not redirect.
	return 0, fmt.Errorf("slack webhook status %d", resp.StatusCode)
}

// markInvalid records code unless the user registered another webhook in the
// meantime. A failure is logged; the notification ends either way.
func (s *Slack) markInvalid(ctx context.Context, userID, sealed, code string) {
	if _, err := s.Store.MarkSlackWebhookInvalid(context.WithoutCancel(ctx), userID, sealed, code); err != nil {
		log.Printf("slack notification: user=%s: record %s: %v", userID, code, err)
	}
}

// slackEscaper escapes the characters with a meaning in Slack mrkdwn.
var slackEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

// slackText returns the mrkdwn text of m: the title in bold, the body and a
// link to the web UI.
func slackText(m Message, publicURL string) string {
	link := strings.ReplaceAll(slackEscaper.Replace(absoluteURL(publicURL, m.URL)), "|", "%7C")
	return "*" + slackEscaper.Replace(truncate(m.Title, 150)) + "*\n" + slackEscaper.Replace(truncate(m.Body, 1000)) + "\n<" + link + "|Astovia で開く>"
}
