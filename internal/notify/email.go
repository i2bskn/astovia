package notify

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/i2bskn/astovia/internal/mail"
	"github.com/i2bskn/astovia/internal/store"
)

// Email sends messages to the login address of the user through the mail
// service the operator configured.
type Email struct {
	Store *store.Store
	// Sender is nil when the server has no mail service.
	Sender    mail.Sender
	PublicURL string
}

func (e *Email) Name() string { return store.ChannelEmail }

// Send mails the message with the notification ID as the idempotency key.
// A rate limit of the mail service returns ErrRateLimited.
func (e *Email) Send(ctx context.Context, userID string, m Message) (int, error) {
	if e.Sender == nil {
		return 0, store.ErrNoSubscription
	}
	to, err := e.Store.UserEmail(ctx, userID)
	if err != nil {
		return 0, err
	}
	err = e.Sender.Send(ctx, mail.Message{To: to, Subject: mailSubject(m), Text: mailText(m, e.PublicURL), IdempotencyKey: m.ID})
	if errors.Is(err, mail.ErrRateLimited) {
		return 0, fmt.Errorf("%w: %w", ErrRateLimited, err)
	}
	if err != nil {
		return 0, err
	}
	return 1, nil
}

// stripControl removes control characters; newlines are kept when keepNewline
// is set.
func stripControl(v string, keepNewline bool) string {
	return strings.Map(func(r rune) rune {
		if keepNewline && r == '\n' {
			return r
		}
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return -1
		}
		return r
	}, v)
}

// mailSubject returns the subject of m: its title without control
// characters, at most 150 characters.
func mailSubject(m Message) string {
	subject := strings.TrimSpace(truncate(stripControl(m.Title, false), 150))
	if subject == "" {
		return "Astovia の通知"
	}
	return subject
}

// mailText returns the plain text of m: the body, the link to the web UI and
// a footer naming where the channels are changed.
func mailText(m Message, publicURL string) string {
	base := strings.TrimRight(publicURL, "/")
	return stripControl(m.Body, true) + "\n\n" + absoluteURL(publicURL, m.URL) + "\n\n--\nこの通知は Astovia から送っています。送り先は " + base + "/settings/notifications で変えられます。\n"
}
