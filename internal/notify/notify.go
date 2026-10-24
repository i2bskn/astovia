// Package notify decides when to notify the user (Engine), delivers queued
// notifications (Dispatcher) and sends them through a Channel. Web Push is
// the only channel for now; other channels implement the same interface.
package notify

import (
	"context"
	"unicode/utf8"
)

// Message is one notification as handed to a channel.
type Message struct {
	Kind, Title, Body string
	// URL is a path under /admin/ opened when the notification is clicked.
	URL string
	// Tag makes a newer notification replace an older one with the same
	// tag on the device.
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
