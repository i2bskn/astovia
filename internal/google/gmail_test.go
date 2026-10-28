package google

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"golang.org/x/text/encoding/japanese"
)

func TestGmailRequests(t *testing.T) {
	f := &fakeEndpoint{}
	c := newTestClient(t, f)
	ctx := context.Background()
	f.set(200, `{"emailAddress":"alice@example.com","messagesTotal":3}`)
	email, err := c.Profile(ctx, testAccess)
	r := f.last(t)
	if err != nil || email != "alice@example.com" || r.method != "GET" || r.path != "/gmail/v1/users/me/profile" || r.auth != "Bearer "+testAccess || r.rawQuery != "" {
		t.Fatalf("profile: %q %v %+v", email, err, r)
	}
	f.set(200, `{"messagesTotal":3}`)
	if _, err = c.Profile(ctx, testAccess); !errors.Is(err, ErrUnexpected) {
		t.Fatalf("profile without address: %v", err)
	}

	f.set(200, `{"messages":[{"id":"18a1","threadId":"18a0"},{"id":"XYZ","threadId":"18a0"},{"id":"18a2","threadId":"../x"},{"id":"18a3","threadId":"18a3"}],"nextPageToken":"p2","resultSizeEstimate":4}`)
	list, err := c.ListMessages(ctx, testAccess, "in:inbox after:100", "p1")
	r = f.last(t)
	if err != nil || !reflect.DeepEqual(list, MessageList{Messages: []MessageRef{{ID: "18a1", ThreadID: "18a0"}, {ID: "18a3", ThreadID: "18a3"}}, NextPageToken: "p2"}) ||
		r.path != "/gmail/v1/users/me/messages" || r.rawQuery != "maxResults=100&pageToken=p1&q=in%3Ainbox+after%3A100" {
		t.Fatalf("list: %+v %v %+v", list, err, r)
	}
	f.set(200, `{"resultSizeEstimate":0}`)
	if list, err = c.ListMessages(ctx, testAccess, "q", ""); err != nil || len(list.Messages) != 0 || list.NextPageToken != "" || strings.Contains(f.last(t).rawQuery, "pageToken") {
		t.Fatalf("empty list: %+v %v", list, err)
	}

	// An invalid thread ID is not requested.
	n := f.count()
	if _, err = c.Thread(ctx, testAccess, "../profile"); err == nil || f.count() != n {
		t.Fatalf("invalid thread ID: %v %d", err, f.count()-n)
	}
	var api *APIError
	if !errors.As(err, &api) {
		t.Fatalf("invalid thread ID error: %v", err)
	}
}

func TestGmailErrors(t *testing.T) {
	f := &fakeEndpoint{}
	c := newTestClient(t, f)
	ctx := context.Background()
	reason := func(r string) string {
		return `{"error":{"code":403,"message":"alice@example.com x","errors":[{"message":"x","domain":"global","reason":"` + r + `"}],"status":"PERMISSION_DENIED"}}`
	}
	for _, tc := range []struct {
		name   string
		status int
		body   string
		header map[string]string
		check  func(error) bool
	}{
		{"401", 401, `{"error":{"code":401,"status":"UNAUTHENTICATED"}}`, nil, func(e error) bool { return errors.Is(e, ErrUnauthorized) }},
		{"insufficientPermissions", 403, reason("insufficientPermissions"), nil, func(e error) bool { return errors.Is(e, ErrInsufficientScope) }},
		{"scope status", 403, `{"error":{"code":403,"status":"PERMISSION_DENIED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"ACCESS_TOKEN_SCOPE_INSUFFICIENT"}]}}`, nil, func(e error) bool { return errors.Is(e, ErrInsufficientScope) }},
		{"accessNotConfigured", 403, reason("accessNotConfigured"), nil, func(e error) bool { return errors.Is(e, ErrAPIDisabled) }},
		{"SERVICE_DISABLED", 403, `{"error":{"code":403,"status":"PERMISSION_DENIED","details":[{"reason":"SERVICE_DISABLED"}]}}`, nil, func(e error) bool { return errors.Is(e, ErrAPIDisabled) }},
		{"domainPolicy", 403, reason("domainPolicy"), nil, func(e error) bool { return errors.Is(e, ErrPolicyBlocked) }},
		{"rateLimitExceeded", 403, reason("rateLimitExceeded"), nil, func(e error) bool {
			var rl *RateLimitError
			return errors.As(e, &rl) && rl.RetryAt.IsZero() && errors.Is(e, ErrRateLimited)
		}},
		{"userRateLimitExceeded", 403, reason("userRateLimitExceeded"), map[string]string{"Retry-After": "120"}, func(e error) bool {
			var rl *RateLimitError
			return errors.As(e, &rl) && rl.RetryAt.Equal(testNow.Add(2*time.Minute))
		}},
		{"other 403", 403, reason("forbidden"), nil, func(e error) bool {
			var se *StatusError
			return errors.As(e, &se) && se.Code == 403
		}},
		{"429", 429, `{}`, map[string]string{"Retry-After": "30"}, func(e error) bool {
			var rl *RateLimitError
			return errors.As(e, &rl) && rl.RetryAt.Equal(testNow.Add(30*time.Second))
		}},
		{"429 far", 429, `{}`, map[string]string{"Retry-After": "86400"}, func(e error) bool {
			var rl *RateLimitError
			return errors.As(e, &rl) && rl.RetryAt.IsZero()
		}},
		{"404", 404, `{"error":{"code":404}}`, nil, func(e error) bool {
			var api *APIError
			return errors.As(e, &api) && api.Status == 404 && !api.TooLarge
		}},
		{"400", 400, `{"error":{"code":400,"status":"FAILED_PRECONDITION","errors":[{"reason":"failedPrecondition"}]}}`, nil, func(e error) bool {
			var api *APIError
			return errors.As(e, &api) && api.Status == 400
		}},
		{"500", 500, `x`, nil, func(e error) bool {
			var se *StatusError
			return errors.As(e, &se) && se.Code == 500
		}},
		{"302", 302, ``, map[string]string{"Location": "https://evil.example/"}, func(e error) bool {
			var se *StatusError
			return errors.As(e, &se) && se.Code == 302
		}},
		{"too large", 200, `{"id":"` + strings.Repeat("a", gmailBodyLimit) + `"}`, nil, func(e error) bool {
			var api *APIError
			return errors.As(e, &api) && api.TooLarge
		}},
		{"invalid JSON", 200, `{"messages":`, nil, func(e error) bool { return errors.Is(e, ErrUnexpected) }},
		{"wrong shape", 200, `{"messages":"x"}`, nil, func(e error) bool { return errors.Is(e, ErrUnexpected) }},
	} {
		f.mu.Lock()
		f.status, f.body, f.header = tc.status, tc.body, tc.header
		f.mu.Unlock()
		_, err := c.ListMessages(ctx, testAccess, "from:alice@example.com", "")
		if !tc.check(err) {
			t.Fatalf("%s: %v", tc.name, err)
		}
		noSecrets(t, err)
	}
}

func b64(v string) string { return base64.URLEncoding.EncodeToString([]byte(v)) }

func textPart(mimeType, charset, data string) map[string]any {
	ct := mimeType
	if charset != "" {
		ct += "; charset=" + charset
	}
	return map[string]any{"mimeType": mimeType, "filename": "", "headers": []map[string]string{{"name": "Content-Type", "value": ct}}, "body": map[string]any{"data": b64(data), "size": len(data)}}
}

func multipart(mimeType string, parts ...map[string]any) map[string]any {
	return map[string]any{"mimeType": mimeType, "filename": "", "headers": []map[string]string{{"name": "Content-Type", "value": mimeType + "; boundary=x"}}, "body": map[string]any{"size": 0}, "parts": parts}
}

// threadOf answers threads.get with one message of payload and headers.
func threadOf(t *testing.T, f *fakeEndpoint, payload map[string]any, headers map[string]string) {
	t.Helper()
	var hs []map[string]string
	if existing, ok := payload["headers"].([]map[string]string); ok {
		hs = append(hs, existing...)
	}
	for k, v := range headers {
		hs = append(hs, map[string]string{"name": k, "value": v})
	}
	payload["headers"] = hs
	body, err := json.Marshal(map[string]any{"id": "18a0", "historyId": "1", "messages": []map[string]any{
		{"id": "18a1", "threadId": "18a0", "labelIds": []string{"INBOX", "UNREAD"}, "internalDate": "1792800000123", "snippet": "x", "payload": payload},
	}})
	if err != nil {
		t.Fatal(err)
	}
	f.set(200, string(body))
}

func readThread(t *testing.T, c *Client) Message {
	t.Helper()
	th, err := c.Thread(context.Background(), testAccess, "18a0")
	if err != nil || len(th.Messages) != 1 {
		t.Fatalf("thread: %+v %v", th, err)
	}
	return th.Messages[0]
}

func TestThreadMessage(t *testing.T) {
	f := &fakeEndpoint{}
	c := newTestClient(t, f)
	iso, err := japanese.ISO2022JP.NewEncoder().String("お世話になっております。\n資料をご確認ください。")
	if err != nil {
		t.Fatal(err)
	}
	subject, err := japanese.ISO2022JP.NewEncoder().String("見積もりの件")
	if err != nil {
		t.Fatal(err)
	}
	name, err := japanese.ISO2022JP.NewEncoder().String("山田 太郎")
	if err != nil {
		t.Fatal(err)
	}
	enc := func(v string) string { return "=?ISO-2022-JP?B?" + base64.StdEncoding.EncodeToString([]byte(v)) + "?=" }
	threadOf(t, f, textPart("text/plain", "ISO-2022-JP", iso), map[string]string{
		"From": enc(name) + " <Taro@Example.COM>", "To": "Alice <alice@example.com>, bob@example.com", "Cc": "", "Subject": enc(subject), "Date": "Fri, 24 Oct 2026 10:00:00 +0900",
		"List-Unsubscribe": "<mailto:x@example.com>", "Precedence": "bulk", "auto-submitted": "auto-generated",
	})
	m := readThread(t, c)
	if m.ID != "18a1" || m.ThreadID != "18a0" || !reflect.DeepEqual(m.Labels, []string{"INBOX", "UNREAD"}) || !m.Internal.Equal(time.UnixMilli(1792800000123)) || !m.HasLabel("INBOX") || m.HasLabel("SENT") {
		t.Fatalf("message: %+v", m)
	}
	if m.Subject != "見積もりの件" || m.FromName != "山田 太郎" || m.FromEmail != "taro@example.com" || !m.ToParsed || !reflect.DeepEqual(m.ToList, []Address{{Name: "Alice", Email: "alice@example.com"}, {Email: "bob@example.com"}}) ||
		!m.CcParsed || len(m.CcList) != 0 || m.Precedence != "bulk" || m.AutoSubmitted != "auto-generated" || !m.HasAutoSubmitted || m.ListUnsubscribe == "" || m.Date == "" {
		t.Fatalf("headers: %+v", m)
	}
	if m.Text != "お世話になっております。\n資料をご確認ください。" {
		t.Fatalf("text: %q", m.Text)
	}

	// Addresses that cannot be parsed.
	threadOf(t, f, textPart("text/plain", "utf-8", "hi"), map[string]string{"From": "Broken <<\x01 sender", "To": "undisclosed-recipients:;, <<", "Cc": "a@"})
	m = readThread(t, c)
	if m.FromEmail != "" || m.FromName != "Broken << sender" || m.ToParsed || m.ToList != nil || m.CcParsed || m.HasAutoSubmitted {
		t.Fatalf("unparsed: %+v", m)
	}
	threadOf(t, f, textPart("text/plain", "utf-8", "hi"), map[string]string{"From": strings.Repeat("x", 300) + " <<"})
	if m = readThread(t, c); len([]rune(m.FromName)) != 200 {
		t.Fatalf("long From: %d", len(m.FromName))
	}

	// Messages with an invalid ID or date are left out.
	f.set(200, `{"id":"18a0","messages":[{"id":"zz","internalDate":"1"},{"id":"18a2","internalDate":"x"},{"id":"18a3","internalDate":"5","payload":{"mimeType":"text/plain","body":{"data":"`+b64("ok")+`"}}}]}`)
	th, err := c.Thread(context.Background(), testAccess, "18a0")
	if err != nil || len(th.Messages) != 1 || th.Messages[0].ID != "18a3" || th.Messages[0].Text != "ok" {
		t.Fatalf("invalid messages: %+v %v", th, err)
	}
}

func TestBodyText(t *testing.T) {
	f := &fakeEndpoint{}
	c := newTestClient(t, f)
	html := `<!DOCTYPE html><html><head><title>Title text</title><style>.x{}</style><script>alert(1)</script></head><body>
<p>Hello&nbsp;Alice &amp; team,</p>
<div>Please   review<br>the draft.</div>
<!-- a comment -->
<template><p>template text</p></template><noscript>noscript text</noscript><svg><text>svg text</text></svg>
<div hidden>hidden attr</div><span aria-hidden="true">aria hidden</span><span aria-hidden="false">aria shown</span>
<div style="display: none !important">display none</div><div style="DISPLAY:NONE">upper none</div><p style="visibility:hidden">visibility</p>
<p style="opacity:0">opacity zero</p><p style="opacity: 0.8">opacity shown</p><p style="font-size:0px">font zero</p><p style="font-size:0.9em">font shown</p>
<p style="max-height:0">max zero</p><p style="min-height:0">min shown</p><p style="height:0;overflow:hidden">height zero</p><p style="width:0">width only shown</p>
<p style="color:red; background:url(display:none)">decl shown</p>
<ul><li>one</li><li>two</li></ul>
<div class="gmail_quote gmail_quote_container">On Mon wrote:<blockquote>old text</blockquote></div>
</body></html>`
	threadOf(t, f, multipart("multipart/mixed",
		multipart("multipart/alternative", textPart("text/plain", "utf-8", "plain only text"), textPart("text/html", "utf-8", html)),
		map[string]any{"mimeType": "text/plain", "filename": "notes.txt", "headers": []map[string]string{}, "body": map[string]any{"attachmentId": "att1", "size": 10}},
	), nil)
	m := readThread(t, c)
	want := "Hello Alice & team,\nPlease review\nthe draft.\naria shown\nopacity shown\nfont shown\nmin shown\nwidth only shown\ndecl shown\none\ntwo"
	if m.Text != want {
		t.Fatalf("html text:\n%q\nwant\n%q", m.Text, want)
	}
	for _, s := range []string{"plain only", "Title", "alert", "template", "noscript", "svg", "hidden attr", "aria hidden", "display none", "upper none", "visibility", "opacity zero", "font zero", "max zero", "height zero", "old text", "comment"} {
		if strings.Contains(m.Text, s) {
			t.Fatalf("%q left in %q", s, m.Text)
		}
	}

	// An attachment is never read, even as the only text part.
	threadOf(t, f, multipart("multipart/mixed", textPart("text/plain", "utf-8", "body text"),
		map[string]any{"mimeType": "text/html", "filename": "page.html", "headers": []map[string]string{}, "body": map[string]any{"data": b64("<p>attached</p>"), "size": 15}}), nil)
	if m = readThread(t, c); m.Text != "body text" {
		t.Fatalf("attachment: %q", m.Text)
	}

	// Quotes and the lines that introduce them are removed.
	for _, tc := range []struct{ body, want string }{
		{"Thanks!\r\n\r\nOn Mon, Oct 20, 2026 at 9:00 AM Bob <bob@example.com> wrote:\r\n> earlier\r\n> text", "Thanks!"},
		{"了解です。\n\n山田さんは書きました:\n> 以前の本文", "了解です。"},
		{"承知しました。\n\n2026年10月28日(水) 9:30 山田 太郎 <taro@example.com>:\n\n> 以前の本文", "承知しました。"},
		{"top\n> quoted\n  > also quoted\nbottom", "top\nbottom"},
		{"On the other hand, this line stays.\nwrote: too", "On the other hand, this line stays.\nwrote: too"},
	} {
		threadOf(t, f, textPart("text/plain", "", tc.body), nil)
		if m = readThread(t, c); m.Text != tc.want {
			t.Fatalf("quote %q: %q", tc.body, m.Text)
		}
	}

	// Unknown charsets are read as UTF-8 with invalid bytes replaced, and
	// the text is cut at 4,000 characters.
	threadOf(t, f, textPart("text/plain", "x-unknown", "ok\xff"), nil)
	if m = readThread(t, c); m.Text != "ok�" {
		t.Fatalf("unknown charset: %q", m.Text)
	}
	threadOf(t, f, textPart("text/plain", "utf-8", strings.Repeat("あ", 5000)), nil)
	if m = readThread(t, c); len([]rune(m.Text)) != TextLimit {
		t.Fatalf("limit: %d", len([]rune(m.Text)))
	}
	// Shift_JIS through htmlindex.
	sjis, err := japanese.ShiftJIS.NewEncoder().String("こんにちは")
	if err != nil {
		t.Fatal(err)
	}
	threadOf(t, f, textPart("text/plain", "Shift_JIS", sjis), nil)
	if m = readThread(t, c); m.Text != "こんにちは" {
		t.Fatalf("Shift_JIS: %q", m.Text)
	}
}

func TestHiddenStyle(t *testing.T) {
	for style, want := range map[string]bool{
		"display:none": true, "display: none !important": true, " Display : None ; color:red": true, "visibility:hidden": true, "opacity:0": true, "opacity:0.0": true, "opacity:.0": true,
		"font-size:0": true, "font-size:0px": true, "font-size:0em": true, "font-size:0%": true, "max-height:0px": true, "height:0;overflow:hidden": true, "width:0px; overflow: hidden": true,
		"opacity:0.8": false, "font-size:0.9em": false, "min-height:0": false, "height:0": false, "overflow:hidden": false, "display:block": false, "background:url(display:none)": false,
		"x-display:none": false, "display:none-ish": false, "font-size:10px": false, "": false,
	} {
		if hiddenStyle(style) != want {
			t.Fatalf("hiddenStyle(%q) != %v", style, want)
		}
	}
}
