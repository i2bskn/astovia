package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/i2bskn/astovia/internal/slack"
	"github.com/i2bskn/astovia/internal/slacksync"
	"github.com/i2bskn/astovia/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	// slackToolTimeout bounds the database reads and the Slack calls of one
	// tool call, so that the response is written within the server's
	// WriteTimeout.
	slackToolTimeout = 12 * time.Second
	// slackNameLookups bounds the users.info calls of one tool call; other
	// members are shown by ID.
	slackNameLookups = 20
	// slackListTextRunes bounds the text of each message of searches and
	// lists, slackThreadTextRunes that of each message of a thread.
	slackListTextRunes   = 2000
	slackThreadTextRunes = 8000
	// slackQueryRunes bounds a search query and slackCursorRunes a cursor.
	slackQueryRunes  = 500
	slackCursorRunes = 500
	// slackMaxUnix is 9999-12-31T23:59:59Z.
	slackMaxUnix = 253402300799
)

// slackDefaultTypes are the conversation types listed by default.
var slackDefaultTypes = []string{"public_channel", "private_channel", "im", "mpim"}

// slackTSPattern matches a Slack message timestamp.
var slackTSPattern = regexp.MustCompile(`^[0-9]{1,12}\.[0-9]{1,8}$`)

// slackNotFoundCodes are the Slack error codes reported as not found.
var slackNotFoundCodes = map[string]bool{"channel_not_found": true, "thread_not_found": true, "message_not_found": true, "not_in_channel": true, "is_archived": true, "user_not_found": true}

const (
	slackQueryMessage  = "query must be 1-500 characters and not blank."
	slackTypesMessage  = "types must be 1-4 distinct values of public_channel, private_channel, im, mpim."
	slackCursorMessage = "cursor is invalid."
	slackTimeMessage   = "oldest and latest must be RFC 3339 date-times with an offset, from 1970 to 9999."
	slackOrderMessage  = "latest must not be before oldest."
)

// Failures of the Slack tools. The statuses are not used by REST.
var (
	errSlackNotConnected  = &opError{status: 409, code: "slack_not_connected", message: "Slack is not connected. Connect it in Astovia's settings."}
	errSlackTokenInvalid  = &opError{status: 409, code: "slack_token_invalid", message: "The Slack token cannot be used. Reconnect Slack in Astovia's settings."}
	errSlackScopesMissing = &opError{status: 409, code: "slack_scopes_missing", message: "The Slack token lacks a required scope. Reinstall the Slack app from the manifest and reconnect."}
	errSlackMCPLimited    = &opError{status: 429, code: "rate_limited", message: "Too many Slack requests from MCP. Try again in a minute."}
	errSlackNotFound      = &opError{status: 404, code: "not_found", message: "The conversation or message was not found, or you cannot access it."}
	errSlackUnavailable   = &opError{status: 503, code: "slack_unavailable", message: "Slack could not be reached. Try again later."}
	errSlackStore         = &opError{status: 503, code: "unavailable", message: "The service is temporarily unavailable. Try again."}
	errSlackCursor        = invalidRequest(slackCursorMessage)
	errSlackTime          = invalidRequest(slackTimeMessage)
)

// slackMessageResponse is a Slack message with its text in plain text.
// ChannelName is set only by slack_messages_search, which leaves out
// ReplyCount because search results may lack it.
type slackMessageResponse struct {
	ChannelID     string  `json:"channel_id"`
	ChannelName   *string `json:"channel_name,omitempty"`
	TS            string  `json:"ts"`
	UserID        string  `json:"user_id"`
	UserName      string  `json:"user_name"`
	Text          string  `json:"text"`
	TextTruncated bool    `json:"text_truncated,omitempty"`
	SentAt        *string `json:"sent_at"`
	ThreadTS      string  `json:"thread_ts"`
	ReplyCount    *int    `json:"reply_count,omitempty"`
	Subtype       string  `json:"subtype"`
	Bot           bool    `json:"bot"`
	URL           string  `json:"url"`
}

type slackSearchResponse struct {
	Query    string                 `json:"query"`
	Messages []slackMessageResponse `json:"messages"`
	Page     int                    `json:"page"`
	Pages    int                    `json:"pages"`
	Total    int                    `json:"total"`
}

type slackConversationResponse struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
	URL  string `json:"url"`
}

type slackConversationListResponse struct {
	Conversations []slackConversationResponse `json:"conversations"`
	NextCursor    string                      `json:"next_cursor"`
}

type slackMessageListResponse struct {
	ChannelID  string                 `json:"channel_id"`
	Messages   []slackMessageResponse `json:"messages"`
	HasMore    bool                   `json:"has_more"`
	NextCursor string                 `json:"next_cursor"`
}

type slackThreadResponse struct {
	ChannelID  string                 `json:"channel_id"`
	ThreadTS   string                 `json:"thread_ts"`
	Messages   []slackMessageResponse `json:"messages"`
	HasMore    bool                   `json:"has_more"`
	NextCursor string                 `json:"next_cursor"`
	Partial    bool                   `json:"partial"`
}

// addSlackTools adds the tools that read userID's Slack through the Slack
// integration. They never write to Slack.
func (s *Server) addSlackTools(server *mcp.Server, userID string) {
	t := true
	annotations := &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: &t}
	cursorProp := nullableStringProp("Optional next_cursor of the previous page (1-500 characters).")
	channelProp := stringProp("Slack conversation ID (e.g. C0123456789), e.g. from slack_conversations_list or the conversation_id of an action item ref.")
	server.AddTool(&mcp.Tool{
		Name:        "slack_messages_search",
		Description: "Search the user's Slack messages. Reads the user's Slack through Astovia's Slack integration (read only). Slack search syntax works: in:#channel, from:<@USERID>, in:<@USERID> for a DM. Results are newest first.",
		InputSchema: objectSchema([]string{"query"}, map[string]any{
			"query": stringProp("Slack search query (1-500 characters), passed to Slack as is."),
			"count": map[string]any{"type": []string{"integer", "null"}, "minimum": 1, "maximum": 100, "description": "Number of messages per page (1-100, default 20)."},
			"page":  map[string]any{"type": []string{"integer", "null"}, "minimum": 1, "maximum": 100, "description": "Page from 1 (1-100, default 1)."},
		}),
		Annotations: annotations,
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var in struct {
			Query json.RawMessage `json:"query"`
			Count json.RawMessage `json:"count"`
			Page  json.RawMessage `json:"page"`
		}
		if !decodeArguments(req, &in) {
			return toolError(invalidArguments), nil
		}
		if in.Query == nil || isNull(in.Query) {
			return toolError(invalidRequest("query is required.")), nil
		}
		query, ok := decodeString(in.Query)
		query = strings.TrimSpace(query)
		if !ok || query == "" || utf8.RuneCountInString(query) > slackQueryRunes {
			return toolError(invalidRequest(slackQueryMessage)), nil
		}
		count, e := chatworkInt(in.Count, "count", 20, 100)
		if e != nil {
			return toolError(e), nil
		}
		page, e := chatworkInt(in.Page, "page", 1, 100)
		if e != nil {
			return toolError(e), nil
		}
		out, e := s.searchSlackMessages(ctx, userID, query, count, page)
		return toolResult(out, e), nil
	})
	server.AddTool(&mcp.Tool{
		Name:        "slack_conversations_list",
		Description: "List the Slack conversations the user is a member of: public and private channels, direct messages (im) and group direct messages (mpim), archived ones excluded, in Slack's order. Reads the user's Slack through Astovia's Slack integration (read only). A page may have fewer conversations than limit, even none, while next_cursor is set; continue with cursor until it is empty.",
		InputSchema: objectSchema(nil, map[string]any{
			"types": map[string]any{"type": []string{"array", "null"}, "minItems": 1, "maxItems": 4, "uniqueItems": true,
				"items":       map[string]any{"type": "string", "enum": slackDefaultTypes},
				"description": "Optional conversation types (default all): public_channel, private_channel, im, mpim."},
			"limit":  map[string]any{"type": []string{"integer", "null"}, "minimum": 1, "maximum": 200, "description": "Maximum number of conversations asked from Slack (1-200, default 100)."},
			"cursor": cursorProp,
		}),
		Annotations: annotations,
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var in struct {
			Types  json.RawMessage `json:"types"`
			Limit  json.RawMessage `json:"limit"`
			Cursor json.RawMessage `json:"cursor"`
		}
		if !decodeArguments(req, &in) {
			return toolError(invalidArguments), nil
		}
		types, e := slackTypes(in.Types)
		if e != nil {
			return toolError(e), nil
		}
		limit, e := chatworkInt(in.Limit, "limit", 100, 200)
		if e != nil {
			return toolError(e), nil
		}
		cursor, e := slackCursor(in.Cursor)
		if e != nil {
			return toolError(e), nil
		}
		out, e := s.listSlackConversations(ctx, userID, types, limit, cursor)
		return toolResult(out, e), nil
	})
	server.AddTool(&mcp.Tool{
		Name:        "slack_messages_list",
		Description: "List the recent messages of one of the user's Slack conversations, newest first, optionally between oldest and latest. Reads the user's Slack through Astovia's Slack integration (read only). Thread replies are not included; use slack_thread_get with the parent's ts.",
		InputSchema: objectSchema([]string{"channel"}, map[string]any{
			"channel": channelProp,
			"limit":   map[string]any{"type": []string{"integer", "null"}, "minimum": 1, "maximum": 100, "description": "Maximum number of messages (1-100, default 30)."},
			"oldest":  map[string]any{"type": []string{"string", "null"}, "format": "date-time", "description": "Optional start in RFC 3339 with an offset, e.g. 2026-10-24T09:00:00+09:00."},
			"latest":  map[string]any{"type": []string{"string", "null"}, "format": "date-time", "description": "Optional end in RFC 3339 with an offset; not before oldest."},
			"cursor":  cursorProp,
		}),
		Annotations: annotations,
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var in struct {
			Channel json.RawMessage `json:"channel"`
			Limit   json.RawMessage `json:"limit"`
			Oldest  json.RawMessage `json:"oldest"`
			Latest  json.RawMessage `json:"latest"`
			Cursor  json.RawMessage `json:"cursor"`
		}
		if !decodeArguments(req, &in) {
			return toolError(invalidArguments), nil
		}
		channel, e := slackToolID(in.Channel, "channel", slackIDPattern)
		if e != nil {
			return toolError(e), nil
		}
		limit, e := chatworkInt(in.Limit, "limit", 30, 100)
		if e != nil {
			return toolError(e), nil
		}
		oldest, oldestOK, e := slackTime(in.Oldest)
		if e != nil {
			return toolError(e), nil
		}
		latest, latestOK, e := slackTime(in.Latest)
		if e != nil {
			return toolError(e), nil
		}
		if oldestOK && latestOK && latest.Before(oldest) {
			return toolError(invalidRequest(slackOrderMessage)), nil
		}
		cursor, e := slackCursor(in.Cursor)
		if e != nil {
			return toolError(e), nil
		}
		p := slack.HistoryParams{Channel: channel, Limit: limit, Cursor: cursor}
		if oldestOK {
			p.Oldest = slackSecondsTS(oldest)
		}
		if latestOK {
			p.Latest = slackSecondsTS(latest)
		}
		out, e := s.listSlackMessages(ctx, userID, p)
		return toolResult(out, e), nil
	})
	server.AddTool(&mcp.Tool{
		Name:        "slack_thread_get",
		Description: "Get a Slack thread, oldest first: the parent and its replies, from the ts of the parent or of a reply; a message outside threads is returned alone. Reads the user's Slack through Astovia's Slack integration (read only). Use it to read the source of an action item from the conversation_id and message_id of its ref.",
		InputSchema: objectSchema([]string{"channel", "ts"}, map[string]any{
			"channel": channelProp,
			"ts":      stringProp("Slack message timestamp (e.g. 1730000000.000100) of the parent or a reply."),
			"limit":   map[string]any{"type": []string{"integer", "null"}, "minimum": 1, "maximum": 100, "description": "Maximum number of messages (1-100, default 50)."},
			"cursor":  cursorProp,
		}),
		Annotations: annotations,
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var in struct {
			Channel json.RawMessage `json:"channel"`
			TS      json.RawMessage `json:"ts"`
			Limit   json.RawMessage `json:"limit"`
			Cursor  json.RawMessage `json:"cursor"`
		}
		if !decodeArguments(req, &in) {
			return toolError(invalidArguments), nil
		}
		channel, e := slackToolID(in.Channel, "channel", slackIDPattern)
		if e != nil {
			return toolError(e), nil
		}
		ts, e := slackToolID(in.TS, "ts", slackTSPattern)
		if e != nil {
			return toolError(e), nil
		}
		limit, e := chatworkInt(in.Limit, "limit", 50, 100)
		if e != nil {
			return toolError(e), nil
		}
		cursor, e := slackCursor(in.Cursor)
		if e != nil {
			return toolError(e), nil
		}
		out, e := s.getSlackThread(ctx, userID, channel, ts, limit, cursor)
		return toolResult(out, e), nil
	})
}

// slackToolID returns a channel or ts argument. A missing key is an invalid
// request; null or a value that does not match pattern is reported as not
// found, as chatworkToolID does.
func slackToolID(raw json.RawMessage, name string, pattern *regexp.Regexp) (string, *opError) {
	if raw == nil {
		return "", invalidRequest(name + " is required.")
	}
	var v string
	if isNull(raw) || !decodeValue(raw, &v) || !pattern.MatchString(v) {
		return "", errSlackNotFound
	}
	return v, nil
}

// slackCursor returns an optional cursor; null is treated as omitted.
func slackCursor(raw json.RawMessage) (string, *opError) {
	if raw == nil || isNull(raw) {
		return "", nil
	}
	v, ok := decodeString(raw)
	if !ok || v == "" || utf8.RuneCountInString(v) > slackCursorRunes {
		return "", errSlackCursor
	}
	return v, nil
}

// slackTypes returns the comma separated conversation types of types, or
// all of them when it is omitted or null.
func slackTypes(raw json.RawMessage) (string, *opError) {
	if raw == nil || isNull(raw) {
		return strings.Join(slackDefaultTypes, ","), nil
	}
	var items []json.RawMessage
	if !decodeValue(raw, &items) || len(items) == 0 || len(items) > len(slackDefaultTypes) {
		return "", invalidRequest(slackTypesMessage)
	}
	types := make([]string, 0, len(items))
	for _, item := range items {
		v, ok := decodeString(item)
		if !ok || !slices.Contains(slackDefaultTypes, v) || slices.Contains(types, v) {
			return "", invalidRequest(slackTypesMessage)
		}
		types = append(types, v)
	}
	return strings.Join(types, ","), nil
}

// slackTime returns an optional RFC 3339 date-time with an offset; null is
// treated as omitted.
func slackTime(raw json.RawMessage) (time.Time, bool, *opError) {
	if raw == nil || isNull(raw) {
		return time.Time{}, false, nil
	}
	v, ok := decodeString(raw)
	if !ok {
		return time.Time{}, false, errSlackTime
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil || t.Unix() < 0 || t.Unix() > slackMaxUnix {
		return time.Time{}, false, errSlackTime
	}
	return t, true, nil
}

// slackSecondsTS returns the Slack timestamp of t's whole second.
func slackSecondsTS(t time.Time) string { return strconv.FormatInt(t.Unix(), 10) + ".000000" }

// slackConn returns userID's Slack connection and its token.
func (s *Server) slackConn(ctx context.Context, op, userID string) (store.SlackConnection, string, *opError) {
	c, err := s.auth.Store.SlackConnection(ctx, userID)
	if errors.Is(err, store.ErrConnectionNotFound) {
		return store.SlackConnection{}, "", errSlackNotConnected
	}
	if err != nil {
		log.Printf("slack mcp: %s user=%s: %v", op, userID, err)
		return store.SlackConnection{}, "", errSlackStore
	}
	// A connection in the error state is still tried: Slack's answer tells
	// whether the token works now.
	token, err := s.box.Open(slacksync.TokenPurpose(userID), c.TokenSealed)
	if err != nil {
		log.Printf("slack mcp: %s user=%s: open token: %v", op, userID, err)
		return store.SlackConnection{}, "", errSlackTokenInvalid
	}
	return c, token, nil
}

// slackToolError converts a Slack error to an opError. Unexpected errors are
// logged; they never carry the URL or the token. An unusable token does not
// change the connection: the synchronization reports it.
func slackToolError(op, userID string, err error) *opError {
	var rateLimited *slack.RateLimitError
	var apiErr *slack.APIError
	switch {
	case errors.Is(err, slack.ErrUnauthorized):
		return errSlackTokenInvalid
	case errors.Is(err, slack.ErrMissingScope):
		return errSlackScopesMissing
	case errors.As(err, &rateLimited):
		message := "Slack rate limit reached. Try again in a minute."
		if !rateLimited.RetryAt.IsZero() {
			message = "Slack rate limit reached. Try again after " + rateLimited.RetryAt.UTC().Format(time.RFC3339) + "."
		}
		return &opError{status: 429, code: "rate_limited", message: message}
	case errors.As(err, &apiErr) && slackNotFoundCodes[apiErr.Code]:
		return errSlackNotFound
	case errors.As(err, &apiErr) && apiErr.Code == "invalid_cursor":
		return errSlackCursor
	case errors.As(err, &apiErr) && (apiErr.Code == "invalid_ts_oldest" || apiErr.Code == "invalid_ts_latest"):
		return errSlackTime
	default:
		log.Printf("slack mcp: %s user=%s: %v", op, userID, err)
		return errSlackUnavailable
	}
}

// slackNames resolves member IDs to display names for one tool call. At
// most slackNameLookups members are asked from Slack; the others, and those
// Slack does not tell, are shown by ID.
type slackNames struct {
	s       *Server
	ctx     context.Context
	conn    store.SlackConnection
	token   string
	cache   map[string]string
	lookups int
}

func (s *Server) newSlackNames(ctx context.Context, conn store.SlackConnection, token string) *slackNames {
	return &slackNames{s: s, ctx: ctx, conn: conn, token: token, cache: map[string]string{}}
}

// name returns the display name of a member: the profile's display name,
// the real name or the user name, as slacksync does, or the ID. It never
// fails the tool call.
func (n *slackNames) name(id string) string {
	if id == "" {
		return ""
	}
	if id == n.conn.SlackUserID {
		if v := slack.CleanText(n.conn.SlackUserName); v != "" {
			return v
		}
	}
	if v, ok := n.cache[id]; ok {
		return v
	}
	name := id
	n.cache[id] = name
	if !slackIDPattern.MatchString(id) || n.lookups >= slackNameLookups || n.ctx.Err() != nil {
		return name
	}
	n.lookups++
	if !n.s.slackUsersLimiter.allowKey(n.conn.UserID) {
		return name
	}
	u, err := n.s.slack.UserInfo(n.ctx, n.token, id)
	if err != nil {
		return name
	}
	for _, v := range []string{u.DisplayName, u.RealName, u.Name} {
		if v = slack.CleanText(v); v != "" {
			name = v
			break
		}
	}
	n.cache[id] = name
	return name
}

// formatText is slack.FormatText with these names.
func (n *slackNames) formatText(text string) string {
	out, _ := slack.FormatText(text, func(id string) (string, error) { return n.name(id), nil })
	return out
}

// slackArchiveURL returns the workspace URL followed by /archives/ and path
// when the result is a Slack URL, and "" otherwise.
func slackArchiveURL(teamURL, path string) string {
	v := strings.TrimSuffix(teamURL, "/") + "/archives/" + path
	if !slack.ValidURL(v) {
		return ""
	}
	return v
}

// slackConversationURL links to a conversation.
func slackConversationURL(teamURL, channel string) string {
	if !slackIDPattern.MatchString(channel) {
		return ""
	}
	return slackArchiveURL(teamURL, channel)
}

// slackMessageURL links to a message; a reply links within its thread.
func slackMessageURL(teamURL, channel, ts, threadTS string) string {
	if !slackIDPattern.MatchString(channel) || !slackTSPattern.MatchString(ts) {
		return ""
	}
	path := channel + "/p" + strings.ReplaceAll(ts, ".", "")
	if threadTS != "" && threadTS != ts {
		if !slackTSPattern.MatchString(threadTS) {
			return ""
		}
		path += "?thread_ts=" + threadTS + "&cid=" + channel
	}
	return slackArchiveURL(teamURL, path)
}

// toSlackMessage converts m of channel, resolving names and formatting the
// text, which is cut to limit characters.
func toSlackMessage(names *slackNames, channel string, m slack.Message, limit int) slackMessageResponse {
	out := slackMessageResponse{ChannelID: channel, TS: m.TS, UserID: m.User, UserName: names.name(m.User), ThreadTS: m.ThreadTS, Subtype: m.Subtype, Bot: m.BotID != ""}
	out.Text = names.formatText(m.Text)
	if utf8.RuneCountInString(out.Text) > limit {
		out.Text = string([]rune(out.Text)[:limit])
		out.TextTruncated = true
	}
	if slackTSPattern.MatchString(m.TS) {
		sec, _, _ := strings.Cut(m.TS, ".")
		if n, err := strconv.ParseInt(sec, 10, 64); err == nil {
			v := formatTime(n)
			out.SentAt = &v
		}
	}
	return out
}

// withReplyCount sets the reply count of a message from history or replies.
func withReplyCount(out slackMessageResponse, m slack.Message) slackMessageResponse {
	n := m.ReplyCount
	out.ReplyCount = &n
	return out
}

// slackSearchChannelName names the conversation of a search result: #name,
// DM or グループDM, as slacksync does. The other member of a DM is not in
// search results.
func slackSearchChannelName(ch slack.Channel) string {
	switch {
	case ch.IsIM:
		return "DM"
	case ch.IsMPIM:
		return "グループDM"
	}
	if name := slack.CleanText(ch.Name); name != "" {
		return "#" + name
	}
	return ""
}

func (s *Server) searchSlackMessages(ctx context.Context, userID, query string, count, page int) (slackSearchResponse, *opError) {
	ctx, cancel := context.WithTimeout(ctx, slackToolTimeout)
	defer cancel()
	conn, token, e := s.slackConn(ctx, "search", userID)
	if e != nil {
		return slackSearchResponse{}, e
	}
	if !s.slackSearchLimiter.allowKey(userID) {
		return slackSearchResponse{}, errSlackMCPLimited
	}
	res, err := s.slack.SearchMessages(ctx, token, query, count, page)
	if err != nil {
		return slackSearchResponse{}, slackToolError("search", userID, err)
	}
	out := slackSearchResponse{Query: query, Messages: []slackMessageResponse{}, Page: res.Page, Pages: res.Pages, Total: res.Total}
	if out.Page == 0 {
		out.Page = page
	}
	names := s.newSlackNames(ctx, conn, token)
	for _, m := range res.Matches {
		channel := m.Channel.ID
		if !slackIDPattern.MatchString(channel) {
			channel = ""
		}
		// A reply found by search may lack thread_ts; its permalink has it,
		// as slacksync's searchThread reads it.
		if m.ThreadTS == "" {
			if u, err := url.Parse(m.Permalink); err == nil && slackTSPattern.MatchString(u.Query().Get("thread_ts")) {
				m.ThreadTS = u.Query().Get("thread_ts")
			}
		}
		msg := toSlackMessage(names, channel, m, slackListTextRunes)
		name := slackSearchChannelName(m.Channel)
		msg.ChannelName = &name
		if slack.ValidURL(m.Permalink) {
			msg.URL = m.Permalink
		} else if channel != "" {
			msg.URL = slackMessageURL(conn.TeamURL, channel, m.TS, m.ThreadTS)
		}
		out.Messages = append(out.Messages, msg)
	}
	return out, nil
}

func (s *Server) listSlackConversations(ctx context.Context, userID, types string, limit int, cursor string) (slackConversationListResponse, *opError) {
	ctx, cancel := context.WithTimeout(ctx, slackToolTimeout)
	defer cancel()
	conn, token, e := s.slackConn(ctx, "conversations", userID)
	if e != nil {
		return slackConversationListResponse{}, e
	}
	if !s.slackConversationsLimiter.allowKey(userID) {
		return slackConversationListResponse{}, errSlackMCPLimited
	}
	list, next, err := s.slack.UserConversationsPage(ctx, token, types, limit, cursor)
	if err != nil {
		return slackConversationListResponse{}, slackToolError("conversations", userID, err)
	}
	out := slackConversationListResponse{Conversations: []slackConversationResponse{}, NextCursor: next}
	names := s.newSlackNames(ctx, conn, token)
	for _, ch := range list {
		c := slackConversationResponse{ID: ch.ID, URL: slackConversationURL(conn.TeamURL, ch.ID)}
		switch {
		case ch.IsIM:
			c.Type, c.Name = "im", "DM"
			if name := names.name(ch.User); name != "" && name != ch.User {
				c.Name = "DM（" + name + "）"
			}
		case ch.IsMPIM:
			c.Type, c.Name = "mpim", "グループDM"
		default:
			c.Type = "public_channel"
			if ch.IsPrivate {
				c.Type = "private_channel"
			}
			if name := slack.CleanText(ch.Name); name != "" {
				c.Name = "#" + name
			}
		}
		out.Conversations = append(out.Conversations, c)
	}
	return out, nil
}

func (s *Server) listSlackMessages(ctx context.Context, userID string, p slack.HistoryParams) (slackMessageListResponse, *opError) {
	ctx, cancel := context.WithTimeout(ctx, slackToolTimeout)
	defer cancel()
	conn, token, e := s.slackConn(ctx, "messages", userID)
	if e != nil {
		return slackMessageListResponse{}, e
	}
	if !s.slackHistoryLimiter.allowKey(userID) {
		return slackMessageListResponse{}, errSlackMCPLimited
	}
	page, err := s.slack.History(ctx, token, p)
	if err != nil {
		return slackMessageListResponse{}, slackToolError("messages", userID, err)
	}
	out := slackMessageListResponse{ChannelID: p.Channel, Messages: []slackMessageResponse{}, HasMore: page.HasMore, NextCursor: page.NextCursor}
	names := s.newSlackNames(ctx, conn, token)
	for _, m := range page.Messages {
		msg := withReplyCount(toSlackMessage(names, p.Channel, m, slackListTextRunes), m)
		msg.URL = slackMessageURL(conn.TeamURL, p.Channel, m.TS, m.ThreadTS)
		out.Messages = append(out.Messages, msg)
	}
	return out, nil
}

// getSlackThread reads the thread of ts. When ts is a reply, the thread is
// read again from its parent; if that fails for a reason other than the
// token, the first page is returned as partial.
func (s *Server) getSlackThread(ctx context.Context, userID, channel, ts string, limit int, cursor string) (slackThreadResponse, *opError) {
	ctx, cancel := context.WithTimeout(ctx, slackToolTimeout)
	defer cancel()
	conn, token, e := s.slackConn(ctx, "thread", userID)
	if e != nil {
		return slackThreadResponse{}, e
	}
	if !s.slackHistoryLimiter.allowKey(userID) {
		return slackThreadResponse{}, errSlackMCPLimited
	}
	page, err := s.slack.Replies(ctx, token, slack.HistoryParams{Channel: channel, TS: ts, Limit: limit, Cursor: cursor})
	if err != nil {
		return slackThreadResponse{}, slackToolError("thread", userID, err)
	}
	out := slackThreadResponse{ChannelID: channel, ThreadTS: ts, Messages: []slackMessageResponse{}}
	if len(page.Messages) > 0 {
		if parent := page.Messages[0].ThreadTS; parent != "" && parent != ts && slackTSPattern.MatchString(parent) {
			out.ThreadTS = parent
			switch {
			case !s.slackHistoryLimiter.allowKey(userID):
				out.Partial = true
			default:
				whole, err := s.slack.Replies(ctx, token, slack.HistoryParams{Channel: channel, TS: parent, Limit: limit})
				if err != nil {
					if e := slackToolError("thread parent", userID, err); e == errSlackTokenInvalid || e == errSlackScopesMissing {
						return slackThreadResponse{}, e
					}
					out.Partial = true
				} else {
					page = whole
				}
			}
		}
	}
	if len(page.Messages) == 0 {
		return slackThreadResponse{}, errSlackNotFound
	}
	out.HasMore, out.NextCursor = page.HasMore, page.NextCursor
	names := s.newSlackNames(ctx, conn, token)
	for _, m := range page.Messages {
		msg := withReplyCount(toSlackMessage(names, channel, m, slackThreadTextRunes), m)
		msg.URL = slackMessageURL(conn.TeamURL, channel, m.TS, m.ThreadTS)
		out.Messages = append(out.Messages, msg)
	}
	return out, nil
}
