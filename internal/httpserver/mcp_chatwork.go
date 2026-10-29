package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/i2bskn/astovia/internal/chatwork"
	"github.com/i2bskn/astovia/internal/chatworksync"
	"github.com/i2bskn/astovia/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	// chatworkToolTimeout bounds every Chatwork call of one tool call, so
	// that the response is written within the server's WriteTimeout.
	chatworkToolTimeout = 12 * time.Second
	// chatworkSearchWorkers is the number of rooms read at the same time by
	// chatwork_messages_search.
	chatworkSearchWorkers = 4
	// chatworkBodyRunes bounds the body of each message in lists.
	chatworkBodyRunes = 2000
	// chatworkQueryRunes bounds a query.
	chatworkQueryRunes = 100
)

const chatworkSearchNote = "Only the latest 100 messages of each room are searched."

// Failures of the Chatwork tools. The statuses are not used by REST.
var (
	errChatworkNotConnected = &opError{status: 409, code: "chatwork_not_connected", message: "Chatwork is not connected. Connect it in Astovia's settings."}
	errChatworkTokenInvalid = &opError{status: 409, code: "chatwork_token_invalid", message: "The Chatwork API token cannot be used. Reconnect Chatwork in Astovia's settings."}
	errChatworkMCPLimited   = &opError{status: 429, code: "rate_limited", message: "Too many Chatwork requests from MCP. Try again in a few minutes."}
	errChatworkNotFound     = &opError{status: 404, code: "not_found", message: "The room or message was not found, or you cannot access it."}
	errChatworkUnavailable  = &opError{status: 503, code: "chatwork_unavailable", message: "Chatwork could not be reached. Try again later."}
	errChatworkStore        = &opError{status: 503, code: "unavailable", message: "The service is temporarily unavailable. Try again."}
)

const (
	chatworkQueryMessage    = "query must be 1-100 characters with at least one word."
	chatworkTypeMessage     = "type must be my, direct or group."
	chatworkRoomIDsMessage  = "room_ids must be an array of 1-20 room IDs."
	chatworkCombinedMessage = "room_ids cannot be combined with type or rooms."
)

// chatworkMessageResponse is a Chatwork message. Body is Chatwork's markup
// as is. RoomName is set only by chatwork_messages_search.
type chatworkMessageResponse struct {
	MessageID     string  `json:"message_id"`
	RoomID        string  `json:"room_id"`
	RoomName      *string `json:"room_name,omitempty"`
	AccountID     string  `json:"account_id"`
	AccountName   string  `json:"account_name"`
	Body          string  `json:"body"`
	BodyTruncated bool    `json:"body_truncated,omitempty"`
	SentAt        string  `json:"sent_at"`
	URL           string  `json:"url"`
}

type chatworkRoomResponse struct {
	RoomID       string  `json:"room_id"`
	Name         string  `json:"name"`
	Type         string  `json:"type"`
	UnreadNum    int     `json:"unread_num"`
	MentionNum   int     `json:"mention_num"`
	LastUpdateAt *string `json:"last_update_at"`
	URL          string  `json:"url"`
}

type chatworkRoomListResponse struct {
	Rooms []chatworkRoomResponse `json:"rooms"`
	Total int                    `json:"total"`
}

type chatworkMessageListResponse struct {
	RoomID   string                    `json:"room_id"`
	RoomURL  string                    `json:"room_url"`
	Messages []chatworkMessageResponse `json:"messages"`
	Matched  int                       `json:"matched"`
	Fetched  int                       `json:"fetched"`
}

type chatworkSearchedRoom struct {
	RoomID   string `json:"room_id"`
	RoomName string `json:"room_name"`
}

type chatworkSkippedRoom struct {
	RoomID   string `json:"room_id"`
	RoomName string `json:"room_name"`
	Reason   string `json:"reason"`
}

type chatworkSearchResponse struct {
	Query          string                    `json:"query"`
	Messages       []chatworkMessageResponse `json:"messages"`
	Matched        int                       `json:"matched"`
	SearchedRooms  []chatworkSearchedRoom    `json:"searched_rooms"`
	Skipped        []chatworkSkippedRoom     `json:"skipped"`
	MissingRoomIDs []string                  `json:"missing_room_ids"`
	Note           string                    `json:"note"`
}

// addChatworkTools adds the tools that read userID's Chatwork through the
// Chatwork integration. They never write to Chatwork.
func (s *Server) addChatworkTools(server *mcp.Server, userID string) {
	t := true
	annotations := &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: &t}
	typeProp := map[string]any{"type": []string{"string", "null"}, "enum": []any{"my", "direct", "group", nil}, "description": "my (the user's own chat), direct or group."}
	server.AddTool(&mcp.Tool{
		Name:        "chatwork_rooms_list",
		Description: "List the user's Chatwork rooms, most recently updated first, with unread and mention counts. Reads the user's Chatwork through Astovia's Chatwork integration. Use chatwork_messages_list to read a room; Chatwork returns only the latest 100 messages of each room.",
		InputSchema: objectSchema(nil, map[string]any{
			"query": nullableStringProp("Optional words that must all appear in the room name, ignoring case (1-100 characters)."),
			"type":  typeProp,
			"limit": map[string]any{"type": []string{"integer", "null"}, "minimum": 1, "maximum": 200, "description": "Maximum number of rooms (1-200, default 50)."},
		}),
		Annotations: annotations,
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var in struct {
			Query json.RawMessage `json:"query"`
			Type  json.RawMessage `json:"type"`
			Limit json.RawMessage `json:"limit"`
		}
		if !decodeArguments(req, &in) {
			return toolError(invalidArguments), nil
		}
		terms, e := chatworkOptionalQuery(in.Query)
		if e != nil {
			return toolError(e), nil
		}
		roomType, e := chatworkRoomType(in.Type)
		if e != nil {
			return toolError(e), nil
		}
		limit, e := chatworkInt(in.Limit, "limit", 50, 200)
		if e != nil {
			return toolError(e), nil
		}
		out, e := s.listChatworkRooms(ctx, userID, terms, roomType, limit)
		return toolResult(out, e), nil
	})
	server.AddTool(&mcp.Tool{
		Name:        "chatwork_messages_list",
		Description: "List the latest messages of one of the user's Chatwork rooms, newest first, optionally only those containing all the words of query. Reads the user's Chatwork through Astovia's Chatwork integration. The Chatwork API returns only the latest 100 messages of each room. Bodies are in Chatwork's markup ([To:...], [rp ...], [info] and so on).",
		InputSchema: objectSchema([]string{"room_id"}, map[string]any{
			"room_id": stringProp("Chatwork room ID, e.g. from chatwork_rooms_list or the conversation_id of an action item ref."),
			"query":   nullableStringProp("Optional words that must all appear in the body or the sender's name, ignoring case (1-100 characters)."),
			"limit":   map[string]any{"type": []string{"integer", "null"}, "minimum": 1, "maximum": 100, "description": "Maximum number of messages (1-100, default 30)."},
		}),
		Annotations: annotations,
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var in struct {
			RoomID json.RawMessage `json:"room_id"`
			Query  json.RawMessage `json:"query"`
			Limit  json.RawMessage `json:"limit"`
		}
		if !decodeArguments(req, &in) {
			return toolError(invalidArguments), nil
		}
		roomID, e := chatworkToolID(in.RoomID, "room_id", chatwork.ValidID)
		if e != nil {
			return toolError(e), nil
		}
		terms, e := chatworkOptionalQuery(in.Query)
		if e != nil {
			return toolError(e), nil
		}
		limit, e := chatworkInt(in.Limit, "limit", 30, 100)
		if e != nil {
			return toolError(e), nil
		}
		out, e := s.listChatworkMessages(ctx, userID, roomID, terms, limit)
		return toolResult(out, e), nil
	})
	server.AddTool(&mcp.Tool{
		Name:        "chatwork_message_get",
		Description: "Get one message of the user's Chatwork with its full body, e.g. the source of an action item from the conversation_id and message_id of its ref. Reads the user's Chatwork through Astovia's Chatwork integration. Use chatwork_messages_list for the latest 100 messages of a room.",
		InputSchema: objectSchema([]string{"room_id", "message_id"}, map[string]any{
			"room_id":    stringProp("Chatwork room ID."),
			"message_id": stringProp("Chatwork message ID."),
		}),
		Annotations: annotations,
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var in struct {
			RoomID    json.RawMessage `json:"room_id"`
			MessageID json.RawMessage `json:"message_id"`
		}
		if !decodeArguments(req, &in) {
			return toolError(invalidArguments), nil
		}
		roomID, e := chatworkToolID(in.RoomID, "room_id", chatwork.ValidID)
		if e != nil {
			return toolError(e), nil
		}
		messageID, e := chatworkToolID(in.MessageID, "message_id", chatwork.ValidMessageID)
		if e != nil {
			return toolError(e), nil
		}
		out, e := s.getChatworkMessage(ctx, userID, roomID, messageID)
		return toolResult(out, e), nil
	})
	server.AddTool(&mcp.Tool{
		Name:        "chatwork_messages_search",
		Description: "Search the user's Chatwork for messages containing all the words of query, ignoring case, newest first. Reads the user's Chatwork through Astovia's Chatwork integration. Chatwork has no search API: this searches only the latest 100 messages of each target room (at most 20 rooms): room_ids, or the most recently updated rooms.",
		InputSchema: objectSchema([]string{"query"}, map[string]any{
			"query":    stringProp("Words that must all appear in the body or the sender's name (1-100 characters)."),
			"room_ids": map[string]any{"type": []string{"array", "null"}, "minItems": 1, "maxItems": 20, "items": map[string]any{"type": "string"}, "description": "Optional room IDs to search (1-20). Cannot be combined with type or rooms."},
			"type":     typeProp,
			"rooms":    map[string]any{"type": []string{"integer", "null"}, "minimum": 1, "maximum": 20, "description": "Number of the most recently updated rooms to search (1-20, default 10)."},
			"limit":    map[string]any{"type": []string{"integer", "null"}, "minimum": 1, "maximum": 100, "description": "Maximum number of messages (1-100, default 30)."},
		}),
		Annotations: annotations,
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var in struct {
			Query   json.RawMessage `json:"query"`
			RoomIDs json.RawMessage `json:"room_ids"`
			Type    json.RawMessage `json:"type"`
			Rooms   json.RawMessage `json:"rooms"`
			Limit   json.RawMessage `json:"limit"`
		}
		if !decodeArguments(req, &in) {
			return toolError(invalidArguments), nil
		}
		if in.Query == nil || isNull(in.Query) {
			return toolError(invalidRequest("query is required.")), nil
		}
		query, terms, e := chatworkQuery(in.Query)
		if e != nil {
			return toolError(e), nil
		}
		var roomIDs []string
		if in.RoomIDs != nil && !isNull(in.RoomIDs) {
			if (in.Type != nil && !isNull(in.Type)) || (in.Rooms != nil && !isNull(in.Rooms)) {
				return toolError(invalidRequest(chatworkCombinedMessage)), nil
			}
			if roomIDs, e = chatworkRoomIDs(in.RoomIDs); e != nil {
				return toolError(e), nil
			}
		}
		roomType, e := chatworkRoomType(in.Type)
		if e != nil {
			return toolError(e), nil
		}
		rooms, e := chatworkInt(in.Rooms, "rooms", 10, 20)
		if e != nil {
			return toolError(e), nil
		}
		limit, e := chatworkInt(in.Limit, "limit", 30, 100)
		if e != nil {
			return toolError(e), nil
		}
		out, e := s.searchChatworkMessages(ctx, userID, query, terms, roomIDs, roomType, rooms, limit)
		return toolResult(out, e), nil
	})
}

// chatworkToolID returns an ID argument. A missing key is an invalid
// request; null or a value that is not an ID is reported as not found, as
// toolItemID does.
func chatworkToolID(raw json.RawMessage, name string, valid func(string) bool) (string, *opError) {
	if raw == nil {
		return "", invalidRequest(name + " is required.")
	}
	var v string
	if isNull(raw) || !decodeValue(raw, &v) || !valid(v) {
		return "", errChatworkNotFound
	}
	return v, nil
}

// chatworkQuery returns the trimmed query and its words in lower case.
func chatworkQuery(raw json.RawMessage) (string, []string, *opError) {
	v, ok := decodeString(raw)
	v = strings.TrimSpace(v)
	if !ok || v == "" || utf8.RuneCountInString(v) > chatworkQueryRunes {
		return "", nil, invalidRequest(chatworkQueryMessage)
	}
	terms := strings.Fields(strings.ToLower(v))
	if len(terms) == 0 {
		return "", nil, invalidRequest(chatworkQueryMessage)
	}
	return v, terms, nil
}

// chatworkOptionalQuery returns the words of an optional query; null is
// treated as omitted.
func chatworkOptionalQuery(raw json.RawMessage) ([]string, *opError) {
	if raw == nil || isNull(raw) {
		return nil, nil
	}
	_, terms, e := chatworkQuery(raw)
	return terms, e
}

// chatworkMatch reports whether text contains every term, ignoring case.
// No terms match everything.
func chatworkMatch(terms []string, text string) bool {
	if len(terms) == 0 {
		return true
	}
	text = strings.ToLower(text)
	for _, term := range terms {
		if !strings.Contains(text, term) {
			return false
		}
	}
	return true
}

// chatworkRoomType returns an optional room type; null is treated as
// omitted.
func chatworkRoomType(raw json.RawMessage) (string, *opError) {
	if raw == nil || isNull(raw) {
		return "", nil
	}
	v, ok := decodeString(raw)
	if !ok || (v != "my" && v != "direct" && v != "group") {
		return "", invalidRequest(chatworkTypeMessage)
	}
	return v, nil
}

// chatworkInt returns an optional JSON integer from 1 to upper, or def
// when it is omitted or null.
func chatworkInt(raw json.RawMessage, name string, def, upper int) (int, *opError) {
	if raw == nil || isNull(raw) {
		return def, nil
	}
	// Only JSON integers: 50.5, 5e1 and "50" are rejected.
	var n int
	if !decodeValue(raw, &n) || n < 1 || n > upper {
		return 0, invalidRequest(name + " must be an integer from 1 to " + strconv.Itoa(upper) + ".")
	}
	return n, nil
}

// chatworkRoomIDs returns the room IDs of room_ids without duplicates, in
// the given order.
func chatworkRoomIDs(raw json.RawMessage) ([]string, *opError) {
	var items []json.RawMessage
	if !decodeValue(raw, &items) || len(items) == 0 || len(items) > 20 {
		return nil, invalidRequest(chatworkRoomIDsMessage)
	}
	ids := make([]string, 0, len(items))
	for _, item := range items {
		v, ok := decodeString(item)
		if !ok || !chatwork.ValidID(v) {
			return nil, invalidRequest(chatworkRoomIDsMessage)
		}
		if !slices.Contains(ids, v) {
			ids = append(ids, v)
		}
	}
	return ids, nil
}

// chatworkToken returns userID's Chatwork API token.
func (s *Server) chatworkToken(ctx context.Context, op, userID string) (string, *opError) {
	c, err := s.auth.Store.ChatworkConnection(ctx, userID)
	if errors.Is(err, store.ErrConnectionNotFound) {
		return "", errChatworkNotConnected
	}
	if err != nil {
		log.Printf("chatwork mcp: %s user=%s: %v", op, userID, err)
		return "", errChatworkStore
	}
	// A connection in the error state is still tried: Chatwork's answer
	// tells whether the token works now.
	token, err := s.box.Open(chatworksync.TokenPurpose(userID), c.TokenSealed)
	if err != nil {
		log.Printf("chatwork mcp: %s user=%s: open token: %v", op, userID, err)
		return "", errChatworkTokenInvalid
	}
	return token, nil
}

// chatworkToolError converts a Chatwork error to an opError. Unexpected
// errors are logged; they never carry the URL or the token. A 401 does not
// change the connection: the synchronization reports it.
func chatworkToolError(op, userID string, err error) *opError {
	var rateLimited *chatwork.RateLimitError
	var apiErr *chatwork.APIError
	switch {
	case errors.Is(err, chatwork.ErrUnauthorized):
		return errChatworkTokenInvalid
	case errors.As(err, &rateLimited):
		message := "Chatwork rate limit reached. Try again in a few minutes."
		if !rateLimited.RetryAt.IsZero() {
			message = "Chatwork rate limit reached. Try again after " + rateLimited.RetryAt.UTC().Format(time.RFC3339) + "."
		}
		return &opError{status: 429, code: "rate_limited", message: message}
	case errors.As(err, &apiErr) && !apiErr.TooLarge && (apiErr.Status == http.StatusForbidden || apiErr.Status == http.StatusNotFound):
		return errChatworkNotFound
	default:
		log.Printf("chatwork mcp: %s user=%s: %v", op, userID, err)
		return errChatworkUnavailable
	}
}

// toChatworkMessage converts m of roomID. With truncate, the body is cut to
// chatworkBodyRunes.
func toChatworkMessage(roomID string, m chatwork.Message, truncate bool) chatworkMessageResponse {
	out := chatworkMessageResponse{MessageID: m.ID, RoomID: roomID, AccountID: m.AccountID, AccountName: m.AccountName, Body: m.Body,
		SentAt: formatTime(m.SendTime), URL: chatwork.MessageURL(roomID, m.ID)}
	if truncate && utf8.RuneCountInString(m.Body) > chatworkBodyRunes {
		out.Body = string([]rune(m.Body)[:chatworkBodyRunes])
		out.BodyTruncated = true
	}
	return out
}

// newestMessageFirst orders messages by send time, newest first, then by
// message ID.
func newestMessageFirst(a, b chatwork.Message) int {
	if a.SendTime != b.SendTime {
		if a.SendTime > b.SendTime {
			return -1
		}
		return 1
	}
	return -chatwork.CompareIDs(a.ID, b.ID)
}

// sortRooms orders rooms by last update, newest first, then by room ID.
func sortRooms(rooms []chatwork.Room) {
	slices.SortStableFunc(rooms, func(a, b chatwork.Room) int {
		if a.LastUpdate != b.LastUpdate {
			if a.LastUpdate > b.LastUpdate {
				return -1
			}
			return 1
		}
		return -chatwork.CompareIDs(a.ID, b.ID)
	})
}

func (s *Server) listChatworkRooms(ctx context.Context, userID string, terms []string, roomType string, limit int) (chatworkRoomListResponse, *opError) {
	ctx, cancel := context.WithTimeout(ctx, chatworkToolTimeout)
	defer cancel()
	token, e := s.chatworkToken(ctx, "rooms", userID)
	if e != nil {
		return chatworkRoomListResponse{}, e
	}
	if !s.chatworkMCPLimiter.allowKey(userID) {
		return chatworkRoomListResponse{}, errChatworkMCPLimited
	}
	rooms, err := s.chatwork.Rooms(ctx, token)
	if err != nil {
		return chatworkRoomListResponse{}, chatworkToolError("rooms", userID, err)
	}
	var matched []chatwork.Room
	for _, r := range rooms {
		if (roomType == "" || r.Type == roomType) && chatworkMatch(terms, r.Name) {
			matched = append(matched, r)
		}
	}
	sortRooms(matched)
	out := chatworkRoomListResponse{Rooms: []chatworkRoomResponse{}, Total: len(matched)}
	for _, r := range matched[:min(limit, len(matched))] {
		room := chatworkRoomResponse{RoomID: r.ID, Name: r.Name, Type: r.Type, UnreadNum: r.UnreadNum, MentionNum: r.MentionNum, URL: chatwork.RoomURL(r.ID)}
		if r.LastUpdate != 0 {
			v := formatTime(r.LastUpdate)
			room.LastUpdateAt = &v
		}
		out.Rooms = append(out.Rooms, room)
	}
	return out, nil
}

func (s *Server) listChatworkMessages(ctx context.Context, userID, roomID string, terms []string, limit int) (chatworkMessageListResponse, *opError) {
	ctx, cancel := context.WithTimeout(ctx, chatworkToolTimeout)
	defer cancel()
	token, e := s.chatworkToken(ctx, "messages", userID)
	if e != nil {
		return chatworkMessageListResponse{}, e
	}
	if !s.chatworkMCPLimiter.allowKey(userID) {
		return chatworkMessageListResponse{}, errChatworkMCPLimited
	}
	page, err := s.chatwork.Messages(ctx, token, roomID)
	if err != nil {
		return chatworkMessageListResponse{}, chatworkToolError("messages", userID, err)
	}
	var matched []chatwork.Message
	for _, m := range page.Messages {
		if chatworkMatch(terms, m.Body+"\n"+m.AccountName) {
			matched = append(matched, m)
		}
	}
	slices.SortStableFunc(matched, newestMessageFirst)
	out := chatworkMessageListResponse{RoomID: roomID, RoomURL: chatwork.RoomURL(roomID), Messages: []chatworkMessageResponse{}, Matched: len(matched), Fetched: len(page.Messages)}
	for _, m := range matched[:min(limit, len(matched))] {
		out.Messages = append(out.Messages, toChatworkMessage(roomID, m, true))
	}
	return out, nil
}

func (s *Server) getChatworkMessage(ctx context.Context, userID, roomID, messageID string) (chatworkMessageResponse, *opError) {
	ctx, cancel := context.WithTimeout(ctx, chatworkToolTimeout)
	defer cancel()
	token, e := s.chatworkToken(ctx, "message", userID)
	if e != nil {
		return chatworkMessageResponse{}, e
	}
	if !s.chatworkMCPLimiter.allowKey(userID) {
		return chatworkMessageResponse{}, errChatworkMCPLimited
	}
	m, err := s.chatwork.Message(ctx, token, roomID, messageID)
	if err != nil {
		return chatworkMessageResponse{}, chatworkToolError("message", userID, err)
	}
	return toChatworkMessage(roomID, m, false), nil
}

// chatworkRoomSlot is the result of reading one target room of a search.
type chatworkRoomSlot struct {
	page   chatwork.MessagePage
	ok     bool
	reason string
}

// searchChatworkMessages reads the latest messages of the target rooms and
// returns those matching terms. A room that cannot be read is skipped,
// except that a rejected token fails the whole search.
func (s *Server) searchChatworkMessages(ctx context.Context, userID, query string, terms, roomIDs []string, roomType string, rooms, limit int) (chatworkSearchResponse, *opError) {
	ctx, cancel := context.WithTimeout(ctx, chatworkToolTimeout)
	defer cancel()
	token, e := s.chatworkToken(ctx, "search", userID)
	if e != nil {
		return chatworkSearchResponse{}, e
	}
	if !s.chatworkMCPLimiter.allowKey(userID) {
		return chatworkSearchResponse{}, errChatworkMCPLimited
	}
	all, err := s.chatwork.Rooms(ctx, token)
	if err != nil {
		return chatworkSearchResponse{}, chatworkToolError("search rooms", userID, err)
	}
	out := chatworkSearchResponse{Query: query, Messages: []chatworkMessageResponse{}, SearchedRooms: []chatworkSearchedRoom{}, Skipped: []chatworkSkippedRoom{}, MissingRoomIDs: []string{}, Note: chatworkSearchNote}

	// The targets, in the order they are read and reported.
	var targets []chatwork.Room
	if roomIDs != nil {
		byID := make(map[string]chatwork.Room, len(all))
		for _, r := range all {
			byID[r.ID] = r
		}
		for _, id := range roomIDs {
			if r, ok := byID[id]; ok {
				targets = append(targets, r)
			} else {
				out.MissingRoomIDs = append(out.MissingRoomIDs, id)
			}
		}
	} else {
		for _, r := range all {
			if roomType == "" || r.Type == roomType {
				targets = append(targets, r)
			}
		}
		sortRooms(targets)
		targets = targets[:min(rooms, len(targets))]
	}

	searchCtx, cancelSearch := context.WithCancel(ctx)
	defer cancelSearch()
	slots := make([]chatworkRoomSlot, len(targets))
	var mu sync.Mutex
	rateLimited, unauthorized := false, false
	isRateLimited := func() bool {
		mu.Lock()
		defer mu.Unlock()
		return rateLimited
	}
	skip := func(i int, reason string) {
		mu.Lock()
		slots[i].reason = reason
		mu.Unlock()
	}
	sem := make(chan struct{}, chatworkSearchWorkers)
	var wg sync.WaitGroup
	// Rooms are dispatched in order by this goroutine alone, so that the
	// limiter is consumed in order. Once stop is set, the remaining rooms
	// are skipped with it.
	stop := ""
	for i, room := range targets {
		if stop == "" {
			switch {
			case searchCtx.Err() != nil:
				stop = "timeout"
			case isRateLimited():
				stop = "rate_limited"
			case !s.chatworkMCPLimiter.allowKey(userID):
				stop = "rate_limited"
			}
		}
		if stop == "" {
			select {
			case sem <- struct{}{}:
				// A 429 received while waiting stops this room as well.
				if isRateLimited() {
					<-sem
					stop = "rate_limited"
				}
			case <-searchCtx.Done():
				stop = "timeout"
			}
		}
		if stop != "" {
			skip(i, stop)
			continue
		}
		wg.Add(1)
		go func(i int, roomID string) {
			defer wg.Done()
			defer func() { <-sem }()
			page, err := s.chatwork.Messages(searchCtx, token, roomID)
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				slots[i].page, slots[i].ok = page, true
				return
			}
			var rateLimitErr *chatwork.RateLimitError
			var apiErr *chatwork.APIError
			switch {
			case errors.Is(err, chatwork.ErrUnauthorized):
				unauthorized = true
				cancelSearch()
			case errors.As(err, &rateLimitErr):
				rateLimited = true
				slots[i].reason = "rate_limited"
			case errors.As(err, &apiErr) && !apiErr.TooLarge && (apiErr.Status == http.StatusForbidden || apiErr.Status == http.StatusNotFound):
				slots[i].reason = "not_found"
			case searchCtx.Err() != nil:
				slots[i].reason = "timeout"
			default:
				log.Printf("chatwork mcp: search user=%s room=%s: %v", userID, roomID, err)
				slots[i].reason = "unavailable"
			}
		}(i, room.ID)
	}
	wg.Wait()
	if unauthorized {
		return chatworkSearchResponse{}, errChatworkTokenInvalid
	}

	type found struct {
		room chatwork.Room
		msg  chatwork.Message
	}
	var matched []found
	for i, room := range targets {
		slot := slots[i]
		if !slot.ok {
			out.Skipped = append(out.Skipped, chatworkSkippedRoom{RoomID: room.ID, RoomName: room.Name, Reason: slot.reason})
			continue
		}
		out.SearchedRooms = append(out.SearchedRooms, chatworkSearchedRoom{RoomID: room.ID, RoomName: room.Name})
		for _, m := range slot.page.Messages {
			if chatworkMatch(terms, m.Body+"\n"+m.AccountName) {
				matched = append(matched, found{room: room, msg: m})
			}
		}
	}
	slices.SortStableFunc(matched, func(a, b found) int { return newestMessageFirst(a.msg, b.msg) })
	out.Matched = len(matched)
	for _, f := range matched[:min(limit, len(matched))] {
		m := toChatworkMessage(f.room.ID, f.msg, true)
		name := f.room.Name
		m.RoomName = &name
		out.Messages = append(out.Messages, m)
	}
	return out, nil
}
