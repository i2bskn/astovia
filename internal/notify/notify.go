// Package notify decides when to notify the user (Engine), delivers queued
// notifications (Dispatcher) and sends them through a Channel: Web Push,
// Slack (an Incoming Webhook of the user) or e-mail.
package notify

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"
)

// ErrDestinationInvalid is returned by a channel whose destination became
// unusable (a Slack webhook that was removed or cannot be decrypted). The
// notification is not retried.
var ErrDestinationInvalid = errors.New("notification destination is invalid")

// ErrRateLimited is returned by a channel when the service limits the rate
// of sending. The dispatcher postpones the notification without counting an
// attempt.
var ErrRateLimited = errors.New("notification rate limited")

// Errors recorded in notification_channels.slack_last_error.
const (
	// SlackWebhookInvalid: Slack answered 403, 404 or 410.
	SlackWebhookInvalid = "webhook_invalid"
	// SlackWebhookUndecryptable: the stored webhook cannot be decrypted, for
	// example after ASTOVIA_JWT_SECRET changed.
	SlackWebhookUndecryptable = "webhook_undecryptable"
)

// SlackWebhookPurpose is the secretbox purpose of the user's Slack webhook.
func SlackWebhookPurpose(userID string) string {
	return "slack-webhook:" + userID
}

// Message is one notification as handed to a channel.
type Message struct {
	// ID is the ID of the notification row; e-mail uses it as the
	// idempotency key.
	ID                string
	Kind, Title, Body string
	// URL is a same-origin path of the web UI (such as / or /auto) opened when
	// the notification is clicked.
	URL string
	// Tag makes a newer notification replace an older one with the same
	// tag on the device.
	// Tag is used by Web Push only.
	Tag string
}

// Channel delivers messages to a user. Send returns the number of devices
// (or accounts) that accepted the message. When the user has nowhere to
// deliver to, it returns store.ErrNoSubscription.
type Channel interface {
	Name() string
	Send(ctx context.Context, userID string, m Message) (int, error)
}

func truncate(v string, n int) string {
	if utf8.RuneCountInString(v) <= n {
		return v
	}
	return string([]rune(v)[:n])
}

// absoluteURL returns the URL opened from a notification outside the
// browser: the public URL followed by the same-origin path of m.URL.
func absoluteURL(publicURL, path string) string {
	return strings.TrimRight(publicURL, "/") + clickURL(path)
}
