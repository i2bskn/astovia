package httpserver

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/i2bskn/astovia/internal/config"
	"github.com/i2bskn/astovia/internal/notify"
	"github.com/i2bskn/astovia/internal/store"
)

const channelsPath = "/api/v1/notification-channels"

var testMail = &config.MailConfig{Provider: config.MailResend, APIKey: "re_key", From: "notify@example.com"}

func routesBody(webpush, slack, email string) string {
	return `{"routes":{"webpush":` + webpush + `,"slack":` + slack + `,"email":` + email + `}}`
}

func TestNotificationChannelsAPI(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) { testNotificationChannelsAPI(t, driver) })
	}
}

func testNotificationChannelsAPI(t *testing.T, driver string) {
	api := newIntegrationAPI(t, driver)
	check := func(method, path, bearer, body, want string) {
		t.Helper()
		w := api.do(method, path, bearer, body)
		if w.Code != 200 || strings.TrimSpace(w.Body.String()) != want {
			t.Fatalf("%s %s %s: %d %s", method, path, body, w.Code, w.Body.String())
		}
	}
	// The defaults, without and with a mail service.
	check("GET", channelsPath, api.alice, "", `{"routes":{"webpush":["detected","stale","due_soon","overdue","digest"],"slack":[],"email":[]},"slack":{"configured":false,"last_error":null},"email":{"available":false,"provider":null,"address":null}}`)
	api.server.cfg.Mail = testMail
	check("GET", channelsPath, api.alice, "", `{"routes":{"webpush":["detected","stale","due_soon","overdue","digest"],"slack":[],"email":[]},"slack":{"configured":false,"last_error":null},"email":{"available":true,"provider":"resend","address":"alice@example.com"}}`)

	// Kinds are saved in the order of the table.
	check("PUT", channelsPath, api.alice, routesBody(`["digest","detected"]`, `["overdue","stale"]`, `["detected","overdue"]`), `{"routes":{"webpush":["detected","digest"],"slack":["stale","overdue"],"email":["detected","overdue"]},"slack":{"configured":false,"last_error":null},"email":{"available":true,"provider":"resend","address":"alice@example.com"}}`)
	for name, body := range map[string]string{
		"missing key":     `{"routes":{"webpush":[],"slack":[]}}`,
		"unknown kind":    routesBody(`["test"]`, `[]`, `[]`),
		"duplicate":       routesBody(`["stale","stale"]`, `[]`, `[]`),
		"null list":       routesBody(`null`, `[]`, `[]`),
		"null kind":       routesBody(`[null]`, `[]`, `[]`),
		"not a list":      routesBody(`"stale"`, `[]`, `[]`),
		"unknown channel": `{"routes":{"webpush":[],"slack":[],"email":[],"sms":[]}}`,
		"null routes":     `{"routes":null}`,
		"no routes":       `{}`,
	} {
		w := api.do("PUT", channelsPath, api.alice, body)
		if w.Code != 400 || errorCode(t, w) != "invalid_request" || !strings.Contains(w.Body.String(), "routes is invalid.") {
			t.Fatalf("%s: %d %s", name, w.Code, w.Body.String())
		}
	}

	// Without a mail service the e-mail kinds can only be reduced.
	api.server.cfg.Mail = nil
	expectStatus(t, api.do("PUT", channelsPath, api.alice, routesBody(`[]`, `[]`, `["detected","stale"]`)), 400, "mail_unavailable")
	check("PUT", channelsPath, api.alice, routesBody(`[]`, `[]`, `["overdue"]`), `{"routes":{"webpush":[],"slack":[],"email":["overdue"]},"slack":{"configured":false,"last_error":null},"email":{"available":false,"provider":null,"address":null}}`)
	api.server.cfg.Mail = testMail
	check("PUT", channelsPath, api.alice, routesBody(`[]`, `[]`, `["detected","overdue"]`), `{"routes":{"webpush":[],"slack":[],"email":["detected","overdue"]},"slack":{"configured":false,"last_error":null},"email":{"available":true,"provider":"resend","address":"alice@example.com"}}`)
	api.server.cfg.Mail = nil
	check("PUT", channelsPath, api.alice, routesBody(`[]`, `[]`, `["detected","overdue"]`), `{"routes":{"webpush":[],"slack":[],"email":["detected","overdue"]},"slack":{"configured":false,"last_error":null},"email":{"available":false,"provider":null,"address":null}}`)
	check("PUT", channelsPath, api.alice, routesBody(`[]`, `[]`, `[]`), `{"routes":{"webpush":[],"slack":[],"email":[]},"slack":{"configured":false,"last_error":null},"email":{"available":false,"provider":null,"address":null}}`)
	expectStatus(t, api.do("PUT", channelsPath, api.bob, routesBody(`[]`, `[]`, `["detected"]`)), 400, "mail_unavailable")

	// The Slack webhook is sealed and never returned.
	const webhookPath = channelsPath + "/slack-webhook"
	const webhook = "https://hooks.slack.com/services/T0001/B0001/abcDEF123"
	for name, url := range map[string]string{
		"http":        "http://hooks.slack.com/services/T0001/B0001/abcDEF123",
		"other host":  "https://hooks.slack.com.evil.example/services/T0001/B0001/abcDEF123",
		"other path":  "https://hooks.slack.com/workflows/T0001/B0001/abcDEF123",
		"short path":  "https://hooks.slack.com/services/T0001/B0001",
		"query":       webhook + "?x=1",
		"fragment":    webhook + "#x",
		"upper host":  "https://HOOKS.slack.com/services/T0001/B0001/abcDEF123",
		"too long":    "https://hooks.slack.com/services/T0001/B0001/" + strings.Repeat("a", 500),
		"empty":       "",
		"not webhook": "hooks.slack.com/services/T0001/B0001/abcDEF123",
	} {
		w := api.do("PUT", webhookPath, api.alice, `{"url":`+string(mustJSON(t, url))+`}`)
		if w.Code != 400 || errorCode(t, w) != "invalid_request" {
			t.Fatalf("%s: %d %s", name, w.Code, w.Body.String())
		}
	}
	for _, body := range []string{`{}`, `{"url":null}`, `{"url":1}`, `{"url":"` + webhook + `","x":1}`} {
		expectStatus(t, api.do("PUT", webhookPath, api.alice, body), 400, "invalid_request")
	}
	w := api.do("PUT", webhookPath, api.alice, `{"url":" `+webhook+`\n"}`)
	if w.Code != 200 || strings.Contains(w.Body.String(), "hooks.slack.com") || !strings.Contains(w.Body.String(), `"slack":{"configured":true,"last_error":null}`) {
		t.Fatalf("register: %d %s", w.Code, w.Body.String())
	}
	c, err := api.store.NotificationChannels(context.Background(), "alice-id")
	if err != nil {
		t.Fatal(err)
	}
	if opened, err := api.server.box.Open(notify.SlackWebhookPurpose("alice-id"), c.SlackWebhookSealed.String); err != nil || opened != webhook || strings.Contains(c.SlackWebhookSealed.String, "hooks") {
		t.Fatalf("sealed: %q %v", opened, err)
	}
	if _, err = api.store.MarkSlackWebhookInvalid(context.Background(), "alice-id", c.SlackWebhookSealed.String, notify.SlackWebhookInvalid); err != nil {
		t.Fatal(err)
	}
	if w = api.do("GET", channelsPath, api.alice, ""); !strings.Contains(w.Body.String(), `"slack":{"configured":true,"last_error":"webhook_invalid"}`) || strings.Contains(w.Body.String(), "hooks.slack.com") {
		t.Fatalf("last error: %s", w.Body.String())
	}
	for range 2 {
		if w = api.do("DELETE", webhookPath, api.alice, ""); w.Code != 204 {
			t.Fatalf("delete: %d %s", w.Code, w.Body.String())
		}
	}
	if w = api.do("GET", channelsPath, api.alice, ""); !strings.Contains(w.Body.String(), `"slack":{"configured":false,"last_error":null}`) {
		t.Fatalf("after delete: %s", w.Body.String())
	}

	// Only browser sessions manage the channels.
	tok := (&tokenAPI{t: t, handler: api.handler}).issue(api.alice, "x").Token
	for _, r := range [][2]string{{"GET", channelsPath}, {"PUT", channelsPath}, {"PUT", webhookPath}, {"DELETE", webhookPath}, {"POST", channelsPath + "/test"}} {
		expectStatus(t, api.do(r[0], r[1], tok, `{}`), 401, "unauthorized")
		expectStatus(t, api.do(r[0], r[1], "", `{}`), 401, "unauthorized")
	}
}

func TestChannelTestAPI(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) { testChannelTestAPI(t, driver) })
	}
}

func testChannelTestAPI(t *testing.T, driver string) {
	api := newIntegrationAPI(t, driver)
	const path = channelsPath + "/test"
	for _, body := range []string{`{}`, `{"channel":"sms"}`, `{"channel":null}`, `{"channel":"slack","x":1}`} {
		expectStatus(t, api.do("POST", path, api.alice, body), 400, "invalid_request")
	}
	// Nothing set up: each channel answers with its own code.
	expectStatus(t, api.do("POST", path, api.alice, `{"channel":"email"}`), 409, "mail_unavailable")
	expectStatus(t, api.do("POST", path, api.alice, `{"channel":"slack"}`), 409, "no_destination")
	expectStatus(t, api.do("POST", path, api.alice, `{"channel":"webpush"}`), 409, "no_subscription")
	// The limit of three per minute is shared by every channel and
	// /api/v1/push/test; mail_unavailable is answered before it.
	expectStatus(t, api.do("POST", "/api/v1/push/test", api.alice, ""), 409, "no_subscription")
	w := api.do("POST", path, api.alice, `{"channel":"slack"}`)
	if w.Code != 429 || errorCode(t, w) != "rate_limited" || w.Header().Get("Retry-After") != "60" {
		t.Fatalf("shared limit: %d %s", w.Code, w.Body.String())
	}
	expectStatus(t, api.do("POST", "/api/v1/push/test", api.alice, ""), 429, "rate_limited")
	// The limit is per user.
	expectStatus(t, api.do("POST", path, api.bob, `{"channel":"slack"}`), 409, "no_destination")

	api.server.pushTestLimiter = newKeyLimiter(3, time.Minute)
	api.server.cfg.Mail = testMail
	if w = api.do("PUT", channelsPath+"/slack-webhook", api.alice, `{"url":"https://hooks.slack.com/services/T1/B1/x1"}`); w.Code != 200 {
		t.Fatalf("register: %d %s", w.Code, w.Body.String())
	}
	for _, channel := range []string{"slack", "email"} {
		if w = api.do("POST", path, api.alice, `{"channel":"`+channel+`"}`); w.Code != 202 || strings.TrimSpace(w.Body.String()) != `{"queued":true}` {
			t.Fatalf("%s: %d %s", channel, w.Code, w.Body.String())
		}
	}
	due, err := api.store.DueNotifications(context.Background(), api.now.Unix(), 10)
	if err != nil || len(due) != 2 {
		t.Fatalf("queued: %+v %v", due, err)
	}
	channels := map[string]bool{}
	for _, n := range due {
		channels[n.Channel] = true
		if n.Kind != store.NotificationTest || n.Title != "Astovia のテスト通知" || n.Body != "この送り先で通知を受け取れます。" || n.URL != "/settings/notifications" || !strings.HasPrefix(n.DedupeKey, "test:") || n.UserID != "alice-id" {
			t.Fatalf("test notification: %+v", n)
		}
	}
	if !channels[store.ChannelSlack] || !channels[store.ChannelEmail] {
		t.Fatalf("channels: %v", channels)
	}
	// A webhook Slack rejected is not set up any more.
	c, err := api.store.NotificationChannels(context.Background(), "alice-id")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = api.store.MarkSlackWebhookInvalid(context.Background(), "alice-id", c.SlackWebhookSealed.String, notify.SlackWebhookInvalid); err != nil {
		t.Fatal(err)
	}
	expectStatus(t, api.do("POST", path, api.alice, `{"channel":"slack"}`), 409, "no_destination")

	// Web Push through the new endpoint.
	api.server.pushTestLimiter = newKeyLimiter(3, time.Minute)
	p256dh, authKey := pushKeys(t)
	if w = api.do("POST", "/api/v1/push/subscriptions", api.bob, subscriptionBody("https://push.example/b", p256dh, authKey, "")); w.Code != 201 {
		t.Fatalf("subscribe: %d %s", w.Code, w.Body.String())
	}
	if w = api.do("POST", path, api.bob, `{"channel":"webpush"}`); w.Code != 202 {
		t.Fatalf("webpush: %d %s", w.Code, w.Body.String())
	}
	due, err = api.store.DueNotifications(context.Background(), api.now.Unix(), 10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, n := range due {
		if n.UserID == "bob-id" {
			found = n.Channel == store.ChannelWebPush && n.Body == "この端末で通知を受け取れます。"
		}
	}
	if !found {
		t.Fatalf("webpush test: %+v", due)
	}
}

// A password reset and the disabling remove the Slack webhook and keep the
// chosen kinds; the old session cannot register it again.
func TestSlackWebhookRevoked(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			for _, disable := range []bool{false, true} {
				api := newIntegrationAPI(t, driver)
				body := `{"url":"https://hooks.slack.com/services/T1/B1/x1"}`
				if w := api.do("PUT", channelsPath+"/slack-webhook", api.alice, body); w.Code != 200 {
					t.Fatalf("PUT: %d %s", w.Code, w.Body.String())
				}
				if w := api.do("PUT", channelsPath, api.alice, routesBody(`[]`, `["stale"]`, `[]`)); w.Code != 200 {
					t.Fatalf("routes: %d %s", w.Code, w.Body.String())
				}
				if err := api.store.ChangeUser(context.Background(), "alice@example.com", "new-hash", disable, api.now.Unix()); err != nil {
					t.Fatal(err)
				}
				expectStatus(t, api.do("PUT", channelsPath+"/slack-webhook", api.alice, body), 401, "unauthorized")
				c, err := api.store.NotificationChannels(context.Background(), "alice-id")
				if err != nil || c.SlackWebhookSealed.Valid || len(c.Slack) != 1 {
					t.Fatalf("disable=%v: %+v %v", disable, c, err)
				}
			}
		})
	}
}
