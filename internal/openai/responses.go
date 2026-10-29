package openai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
)

const (
	// eventLimit bounds one event of a stream and streamLimit the whole
	// stream.
	eventLimit  = 1 << 20
	streamLimit = 4 << 20
	// modelsLimit bounds the model list.
	modelsLimit = 1 << 20
	// maxModels is the most models returned.
	maxModels = 200
)

// ResponseRequest is one structured answer: the instructions (the system
// prompt), the user message and the JSON schema of the answer.
type ResponseRequest struct {
	Model        string
	Instructions string
	Input        string
	SchemaName   string
	Schema       map[string]any
}

// responseBody is everything sent to the Responses API. The ChatGPT plan
// usage rejects many of the usual parameters, so nothing else is sent.
type responseBody struct {
	Model        string          `json:"model"`
	Instructions string          `json:"instructions"`
	Input        []inputMessage  `json:"input"`
	Text         responseText    `json:"text"`
	Reasoning    reasoningConfig `json:"reasoning"`
	Store        bool            `json:"store"`
	Stream       bool            `json:"stream"`
}

type inputMessage struct {
	Role    string         `json:"role"`
	Content []inputContent `json:"content"`
}

type inputContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type responseText struct {
	Format responseFormat `json:"format"`
}

type responseFormat struct {
	Type   string         `json:"type"`
	Name   string         `json:"name"`
	Schema map[string]any `json:"schema"`
	Strict bool           `json:"strict"`
}

type reasoningConfig struct {
	Effort string `json:"effort"`
}

// Respond sends a request to the Responses API as a stream and returns the
// text of the completed response. A response that failed is an *APIError
// (Status 0 when reported in the stream); an incomplete response or a
// stream too large is ErrRejected; a refusal is ErrRefused; a stream that
// ended before the response completed is ErrTemporary.
func (c *Client) Respond(ctx context.Context, accessToken string, r ResponseRequest) (string, error) {
	const method = "responses"
	body, err := json.Marshal(responseBody{
		Model:        r.Model,
		Instructions: r.Instructions,
		Input:        []inputMessage{{Role: "user", Content: []inputContent{{Type: "input_text", Text: r.Input}}}},
		Text:         responseText{Format: responseFormat{Type: "json_schema", Name: r.SchemaName, Schema: r.Schema, Strict: true}},
		Reasoning:    reasoningConfig{Effort: "low"},
		Store:        false,
		Stream:       true,
	})
	if err != nil {
		return "", fmt.Errorf("%w (%s: invalid request)", ErrRejected, method)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.APIBaseURL+"/responses", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("%w (%s: invalid request)", ErrRejected, method)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("User-Agent", "astovia")
	resp, err := c.do(method, req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", readAPIError(resp)
	}
	return readStream(resp.Body)
}

// readAPIError reads the error of a response that is not 200.
func readAPIError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, bodyLimit))
	e := &APIError{Status: resp.StatusCode}
	var out struct {
		Error  json.RawMessage `json:"error"`
		Detail json.RawMessage `json:"detail"`
	}
	if json.Unmarshal(body, &out) != nil {
		return e
	}
	var obj struct {
		Code  string `json:"code"`
		Param string `json:"param"`
	}
	var text string
	switch {
	case out.Error != nil && json.Unmarshal(out.Error, &obj) == nil:
	case out.Detail != nil && json.Unmarshal(out.Detail, &obj) == nil:
	case out.Detail != nil && json.Unmarshal(out.Detail, &text) == nil:
		obj.Code = text
	}
	e.Code, e.Param = safe(obj.Code), safe(obj.Param)
	return e
}

// safe returns v when it is a plain code or parameter name, or "".
func safe(v string) string {
	if safePattern.MatchString(v) {
		return v
	}
	return ""
}

// errStreamTooLarge is a stream larger than streamLimit.
var errStreamTooLarge = errors.New("stream too large")

// limitedReader fails with errStreamTooLarge after streamLimit bytes.
type limitedReader struct {
	r io.Reader
	n int
}

func (l *limitedReader) Read(p []byte) (int, error) {
	if l.n > streamLimit {
		return 0, errStreamTooLarge
	}
	n, err := l.r.Read(p)
	l.n += n
	if l.n > streamLimit {
		return n, errStreamTooLarge
	}
	return n, err
}

// streamEvent is the part of a stream event that is read.
type streamEvent struct {
	Type  string `json:"type"`
	Code  string `json:"code"`
	Param string `json:"param"`
	Error *struct {
		Code  string `json:"code"`
		Param string `json:"param"`
	} `json:"error"`
	Response *struct {
		Error *struct {
			Code string `json:"code"`
		} `json:"error"`
		Output []struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	} `json:"response"`
}

// readStream reads server-sent events until response.completed.
func readStream(body io.Reader) (string, error) {
	const method = "responses"
	sc := bufio.NewScanner(&limitedReader{r: body})
	sc.Buffer(make([]byte, 0, 64<<10), eventLimit+1)
	var name string
	var data strings.Builder
	hasData := false
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			if hasData {
				text, done, err := handleEvent(name, data.String())
				if done || err != nil {
					return text, err
				}
			}
			name, hasData = "", false
			data.Reset()
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			name = value
		case "data":
			if hasData {
				data.WriteByte('\n')
			}
			data.WriteString(value)
			hasData = true
			if data.Len() > eventLimit {
				return "", fmt.Errorf("%w (%s: event too large)", ErrRejected, method)
			}
		}
	}
	err := sc.Err()
	switch {
	case errors.Is(err, errStreamTooLarge):
		return "", fmt.Errorf("%w (%s: stream too large)", ErrRejected, method)
	case errors.Is(err, bufio.ErrTooLong):
		return "", fmt.Errorf("%w (%s: event too large)", ErrRejected, method)
	case err != nil:
		return "", fmt.Errorf("%w (%s): %w", ErrTemporary, method, err)
	}
	return "", fmt.Errorf("%w (%s: stream ended early)", ErrTemporary, method)
}

// handleEvent handles one event. done is true when the stream is over.
func handleEvent(name, data string) (string, bool, error) {
	const method = "responses"
	if data == "[DONE]" {
		return "", false, nil
	}
	var ev streamEvent
	if err := json.Unmarshal([]byte(data), &ev); err != nil {
		return "", true, fmt.Errorf("%w (%s: unexpected event)", ErrRejected, method)
	}
	kind := ev.Type
	if kind == "" {
		kind = name
	}
	switch kind {
	case "response.completed":
		if ev.Response == nil {
			return "", true, fmt.Errorf("%w (%s: unexpected event)", ErrRejected, method)
		}
		var text strings.Builder
		for _, item := range ev.Response.Output {
			if item.Type != "message" {
				continue
			}
			for _, part := range item.Content {
				switch part.Type {
				case "output_text":
					text.WriteString(part.Text)
				case "refusal":
					return "", true, fmt.Errorf("%w (%s)", ErrRefused, method)
				}
			}
		}
		return text.String(), true, nil
	case "response.failed":
		e := &APIError{}
		if ev.Response != nil && ev.Response.Error != nil {
			e.Code = safe(ev.Response.Error.Code)
		}
		return "", true, e
	case "response.incomplete":
		return "", true, fmt.Errorf("%w (%s: incomplete)", ErrRejected, method)
	case "error":
		e := &APIError{Code: safe(ev.Code), Param: safe(ev.Param)}
		if ev.Error != nil {
			e.Code, e.Param = safe(ev.Error.Code), safe(ev.Error.Param)
		}
		return "", true, e
	}
	return "", false, nil
}

// Model is a model the user's plan can use: Slug is sent as the model and
// Name is shown.
type Model struct {
	Slug string
	Name string
}

// Models lists the models of the user's plan whose visibility is "list"
// (or not given), in the order of the API, without duplicates and at most
// maxModels.
func (c *Client) Models(ctx context.Context, accessToken string) ([]Model, error) {
	const method = "models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.APIBaseURL+"/models", nil)
	if err != nil {
		return nil, fmt.Errorf("%w (%s: invalid request)", ErrRejected, method)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "astovia")
	resp, err := c.do(method, req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, readAPIError(resp)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, modelsLimit+1))
	if err != nil {
		return nil, fmt.Errorf("%w (%s): %w", ErrTemporary, method, err)
	}
	if len(body) > modelsLimit {
		return nil, fmt.Errorf("%w (%s: response body is too large)", ErrRejected, method)
	}
	type entry struct {
		Slug        string `json:"slug"`
		ID          string `json:"id"`
		DisplayName string `json:"display_name"`
		Visibility  string `json:"visibility"`
	}
	// The plan route lists the models under "models"; the API under "data".
	var out struct {
		Data   *[]entry `json:"data"`
		Models *[]entry `json:"models"`
	}
	if err = json.Unmarshal(body, &out); err != nil || (out.Data == nil && out.Models == nil) {
		return nil, fmt.Errorf("%w (%s: unexpected response, keys %s)", ErrTemporary, method, topKeys(body))
	}
	list := out.Models
	if list == nil {
		list = out.Data
	}
	models := []Model{}
	seen := map[string]bool{}
	named := false
	for _, m := range *list {
		named = named || m.Slug != "" || m.ID != ""
		if m.Visibility != "" && m.Visibility != "list" {
			continue
		}
		slug := m.Slug
		if slug == "" {
			slug = m.ID
		}
		if slug == "" || seen[slug] {
			continue
		}
		seen[slug] = true
		name := m.DisplayName
		if name == "" {
			name = slug
		}
		models = append(models, Model{Slug: slug, Name: name})
		if len(models) == maxModels {
			break
		}
	}
	// Entries that name no model mean the response is not understood.
	if len(*list) > 0 && !named {
		return nil, fmt.Errorf("%w (%s: entries without slug or id)", ErrTemporary, method)
	}
	return models, nil
}

// topKeys names the top-level keys of a JSON object for a log, without any
// value: "[]" when body is not an object.
func topKeys(body []byte) string {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return "[]"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, safe(k))
	}
	sort.Strings(keys)
	return "[" + strings.Join(keys, " ") + "]"
}
