package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/i2bskn/astovia/internal/openai"
)

// checkSchema is the structured output of a check request.
var checkSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"ok": map[string]any{"type": "boolean"},
	},
	"required":             []string{"ok"},
	"additionalProperties": false,
}

// ValidOpenAIModel accepts a model slug of 1-128 letters, digits and
// ".", "_", ":" or "-".
func ValidOpenAIModel(slug string) bool {
	if len(slug) < 1 || len(slug) > 128 {
		return false
	}
	for i := 0; i < len(slug); i++ {
		b := slug[i]
		if !(b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '.' || b == '_' || b == ':' || b == '-') {
			return false
		}
	}
	return true
}

// openAIClient returns the OpenAI client, or a default one for a Client
// built without it.
func (c *Client) openAIClient() *openai.Client {
	if c.OpenAI == nil {
		return openai.New("")
	}
	return c.OpenAI
}

// structuredOpenAI is structured with the user's ChatGPT plan. OpenAITimeout
// bounds everything: getting the token, the request, and the one retry with
// a new token after the token was refused.
func (c *Client) structuredOpenAI(ctx context.Context, cred Credential, model, system, user, schemaName string, schema map[string]any) (string, error) {
	timeout := c.OpenAITimeout
	if timeout <= 0 {
		timeout = openAITimeout
	}
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req := openai.ResponseRequest{Model: model, Instructions: system, Input: user, SchemaName: schemaName, Schema: schema}
	return withOpenAIToken(ctx, rctx, cred, func(token string) (string, error) {
		return c.openAIClient().Respond(rctx, token, req)
	})
}

// CheckOpenAI verifies with one small request that the ChatGPT plan of cred
// can answer with model, the JSON schema and the reasoning effort that the
// judgments use. ctx bounds it (OpenAITimeout does not apply). An answer
// other than {"ok": true} is ErrUnusable.
func (c *Client) CheckOpenAI(ctx context.Context, cred Credential, model string) error {
	req := openai.ResponseRequest{Model: model, Instructions: `Reply with {"ok": true}.`, Input: "ok?", SchemaName: checkSchemaName, Schema: checkSchema}
	text, err := withOpenAIToken(ctx, ctx, cred, func(token string) (string, error) {
		return c.openAIClient().Respond(ctx, token, req)
	})
	if err != nil {
		return err
	}
	var out struct {
		OK *bool `json:"ok"`
	}
	d := json.NewDecoder(strings.NewReader(text))
	d.DisallowUnknownFields()
	if err := d.Decode(&out); err != nil || out.OK == nil || !*out.OK {
		return fmt.Errorf("%w (invalid output)", ErrUnusable)
	}
	return nil
}

// OpenAIModels lists the models the ChatGPT plan of cred can use, keeping
// only those whose slug ValidOpenAIModel accepts. ctx bounds it.
func (c *Client) OpenAIModels(ctx context.Context, cred Credential) ([]openai.Model, error) {
	list, err := withOpenAIToken(ctx, ctx, cred, func(token string) ([]openai.Model, error) {
		return c.openAIClient().Models(ctx, token)
	})
	if err != nil {
		return nil, err
	}
	out := []openai.Model{}
	for _, m := range list {
		if ValidOpenAIModel(m.Slug) {
			out = append(out, m)
		}
	}
	return out, nil
}

// withOpenAIToken runs call with an access token of cred, and once more
// with a new token when the token was refused. parent is the caller's
// context and ctx the one bounding the call. Errors are classified: when
// parent is done its error is returned, and the other context errors are
// ErrTemporary.
func withOpenAIToken[T any](parent, ctx context.Context, cred Credential, call func(token string) (T, error)) (T, error) {
	var zero T
	if cred.Token == nil {
		return zero, fmt.Errorf("%w (no token source)", ErrKeyInvalid)
	}
	token, err := cred.Token(ctx, "")
	if err != nil {
		return zero, tokenError(parent, err)
	}
	v, err := call(token)
	if err != nil && errors.Is(err, openai.ErrUnauthorized) && parent.Err() == nil {
		if token, err = cred.Token(ctx, token); err != nil {
			return zero, tokenError(parent, err)
		}
		v, err = call(token)
	}
	if err != nil {
		return zero, classifyOpenAI(parent, err)
	}
	return v, nil
}

// classes are the error classes, in the order they are tested.
var classes = []error{ErrKeyInvalid, ErrModelUnavailable, ErrBilling, ErrUsageLimit, ErrUnusable, ErrTemporary}

// tokenError returns an error of a token source: the caller's context error
// when it is done, ErrTemporary for another context error or an error that
// is not classified, and the error as it is otherwise (so that errors.Is
// still finds what the source wrapped).
func tokenError(parent context.Context, err error) error {
	if e := parent.Err(); e != nil {
		return e
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%w (token: timeout)", ErrTemporary)
	}
	for _, class := range classes {
		if errors.Is(err, class) {
			return err
		}
	}
	return fmt.Errorf("%w (token)", ErrTemporary)
}

// classifyOpenAI maps an error of the openai package to one of the error
// classes. parent is the caller's context: when it is done, its error is
// returned. The result never includes response bodies or tokens.
func classifyOpenAI(parent context.Context, err error) error {
	if e := parent.Err(); e != nil {
		return e
	}
	var api *openai.APIError
	switch {
	case errors.As(err, &api):
		return fmt.Errorf("%w (status %d, code %q, param %q)", apiErrorClass(api), api.Status, api.Code, api.Param)
	case errors.Is(err, openai.ErrRefused), errors.Is(err, openai.ErrRejected):
		return fmt.Errorf("%w (%v)", ErrUnusable, err)
	}
	// Connection errors, timeouts and streams that ended early. The
	// errors of package openai carry no token, body or URL.
	return fmt.Errorf("%w (%v)", ErrTemporary, err)
}

// apiErrorClass is the class of a refusal of the ChatGPT plan usage.
func apiErrorClass(e *openai.APIError) error {
	switch e.Code {
	case "subscription_sharing_invalid_user":
		return ErrKeyInvalid
	case "subscription_sharing_user_not_eligible":
		return ErrBilling
	case "subscription_sharing_usage_limit_exceeded":
		return ErrUsageLimit
	case "subscription_sharing_usage_unavailable", "subscription_sharing_user_unavailable":
		return ErrTemporary
	case "subscription_sharing_unsupported_capability", "subscription_sharing_route_not_supported":
		return ErrUnusable
	case "chatpass_v2_scope_not_authorized", "chatpass_v2_invalid_authorization_context":
		return ErrKeyInvalid
	case "model_not_found":
		return ErrModelUnavailable
	}
	code := e.Status
	switch {
	case code == 0:
		// A failure reported in the stream without a known code.
		return ErrTemporary
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		return ErrKeyInvalid
	case code == http.StatusNotFound:
		return ErrModelUnavailable
	case code == http.StatusRequestTimeout || code == http.StatusTooManyRequests || code >= 500:
		return ErrTemporary
	case code >= 400 && code < 500:
		return ErrUnusable
	}
	return ErrTemporary
}
