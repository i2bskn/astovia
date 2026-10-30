package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/i2bskn/astovia/internal/store"
)

// instructionsBody is a PUT body with every scope set from values ("" for
// the others).
func instructionsBody(t *testing.T, values map[string]string) string {
	t.Helper()
	in := map[string]string{}
	for _, scope := range store.InstructionScopes {
		in[scope] = values[scope]
	}
	data, err := json.Marshal(map[string]any{"instructions": in})
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// instructionsOf decodes a response of the instructions API and checks that
// it has every scope.
func instructionsOf(t *testing.T, raw []byte) map[string]string {
	t.Helper()
	var out struct {
		Instructions map[string]string `json:"instructions"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("%v: %s", err, raw)
	}
	if len(out.Instructions) != len(store.InstructionScopes) {
		t.Fatalf("scopes: %s", raw)
	}
	for _, scope := range store.InstructionScopes {
		if _, ok := out.Instructions[scope]; !ok {
			t.Fatalf("missing %s: %s", scope, raw)
		}
	}
	return out.Instructions
}

func TestAIInstructionsAPI(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) { testAIInstructionsAPI(t, driver) })
	}
}

func testAIInstructionsAPI(t *testing.T, driver string) {
	api := newIntegrationAPI(t, driver)
	const path = "/api/v1/ai/instructions"
	empty := map[string]string{}
	for _, scope := range store.InstructionScopes {
		empty[scope] = ""
	}

	w := api.do("GET", path, api.alice, "")
	if w.Code != 200 || !reflect.DeepEqual(instructionsOf(t, w.Body.Bytes()), empty) {
		t.Fatalf("default: %d %s", w.Code, w.Body.String())
	}

	// Saved with the surrounding white space trimmed; an empty one has no row.
	w = api.do("PUT", path, api.alice, instructionsBody(t, map[string]string{store.InstructionCommon: "  bot は対応不要  \n", store.InstructionSlackDetect: "cc は none", store.InstructionGmailReply: " \t "}))
	want := map[string]string{}
	for k, v := range empty {
		want[k] = v
	}
	want[store.InstructionCommon], want[store.InstructionSlackDetect] = "bot は対応不要", "cc は none"
	if w.Code != 200 || !reflect.DeepEqual(instructionsOf(t, w.Body.Bytes()), want) {
		t.Fatalf("PUT: %d %s", w.Code, w.Body.String())
	}
	if w = api.do("GET", path, api.alice, ""); !reflect.DeepEqual(instructionsOf(t, w.Body.Bytes()), want) {
		t.Fatalf("GET after PUT: %s", w.Body.String())
	}
	m, err := api.store.AIInstructions(context.Background(), "alice-id")
	if err != nil || len(m) != 2 || m[store.InstructionCommon] != "bot は対応不要" {
		t.Fatalf("stored: %v %v", m, err)
	}

	// Another user's instructions are not visible.
	if w = api.do("GET", path, api.bob, ""); !reflect.DeepEqual(instructionsOf(t, w.Body.Bytes()), empty) {
		t.Fatalf("bob: %s", w.Body.String())
	}

	// Emptying removes the instruction.
	w = api.do("PUT", path, api.alice, instructionsBody(t, map[string]string{store.InstructionSlackDetect: "cc は none"}))
	if w.Code != 200 {
		t.Fatalf("PUT empty: %d %s", w.Code, w.Body.String())
	}
	if m, err = api.store.AIInstructions(context.Background(), "alice-id"); err != nil || len(m) != 1 || m[store.InstructionSlackDetect] != "cc は none" {
		t.Fatalf("after emptying: %v %v", m, err)
	}

	// Every instruction at the maximum length of four-byte characters fits
	// in the body limit.
	long := map[string]string{}
	for _, scope := range store.InstructionScopes {
		long[scope] = strings.Repeat("𠮷", store.MaxInstructionLength)
	}
	if w = api.do("PUT", path, api.alice, instructionsBody(t, long)); w.Code != 200 || !reflect.DeepEqual(instructionsOf(t, w.Body.Bytes()), long) {
		t.Fatalf("maximum length: %d", w.Code)
	}

	full := `"common":"","github.detect":"","slack.detect":"","chatwork.detect":"","gmail.detect":"","slack.reply":"","chatwork.reply":""`
	for _, body := range []string{
		`{}`,
		`{"instructions":null}`,
		`{"instructions":[]}`,
		`{"instructions":"x"}`,
		`{"instructions":{` + full + `}}`,
		`{"instructions":{` + full + `,"gmail.reply":null}}`,
		`{"instructions":{` + full + `,"gmail.reply":1}}`,
		`{"instructions":{` + full + `,"gmail.reply":""," x":""}}`,
		`{"instructions":{` + full + `,"gmail.reply":""},"other":1}`,
		`{"instructions":{` + full + `,"gmail.reply":"\u0000"}}`,
	} {
		expectStatus(t, api.do("PUT", path, api.alice, body), 400, "invalid_request")
	}
	over := map[string]string{store.InstructionChatworkReply: strings.Repeat("あ", store.MaxInstructionLength+1)}
	w = api.do("PUT", path, api.alice, instructionsBody(t, over))
	expectStatus(t, w, 400, "invalid_request")
	if !strings.Contains(w.Body.String(), "instruction for chatwork.reply must be at most 2000 characters.") {
		t.Fatalf("message: %s", w.Body.String())
	}
	if m, err = api.store.AIInstructions(context.Background(), "alice-id"); err != nil || len(m) != len(store.InstructionScopes) {
		t.Fatalf("changed by a rejected PUT: %d %v", len(m), err)
	}

	// Unauthenticated requests and API tokens are rejected.
	for _, m := range []string{"GET", "PUT"} {
		expectStatus(t, api.do(m, path, "", `{}`), 401, "unauthorized")
	}
	tok := (&tokenAPI{t: t, handler: api.handler}).issue(api.alice, "x").Token
	for _, m := range []string{"GET", "PUT"} {
		expectStatus(t, api.do(m, path, tok, instructionsBody(t, nil)), 401, "unauthorized")
	}
}

// A password reset and the disabling remove the instructions, and the old
// session cannot write them again.
func TestAIInstructionsRevoked(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			for _, disable := range []bool{false, true} {
				api := newIntegrationAPI(t, driver)
				const path = "/api/v1/ai/instructions"
				body := instructionsBody(t, map[string]string{store.InstructionCommon: "指示"})
				if w := api.do("PUT", path, api.alice, body); w.Code != 200 {
					t.Fatalf("PUT: %d %s", w.Code, w.Body.String())
				}
				if err := api.store.ChangeUser(context.Background(), "alice@example.com", "new-hash", disable, api.now.Unix()); err != nil {
					t.Fatal(err)
				}
				expectStatus(t, api.do("PUT", path, api.alice, body), 401, "unauthorized")
				if m, err := api.store.AIInstructions(context.Background(), "alice-id"); err != nil || len(m) != 0 {
					t.Fatalf("disable=%v: kept %v %v", disable, m, err)
				}
			}
		})
	}
}

// SaveAIInstructions checks the session itself, so that a request
// authenticated before the session was revoked cannot write.
func TestSaveAIInstructionsInactiveSession(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			api := newIntegrationAPI(t, driver)
			err := api.store.SaveAIInstructions(context.Background(), "alice-id", "no-such-session", map[string]string{store.InstructionCommon: "x"}, api.now.Unix())
			if !errors.Is(err, store.ErrUnauthorized) {
				t.Fatalf("inactive session: %v", err)
			}
			if m, err := api.store.AIInstructions(context.Background(), "alice-id"); err != nil || len(m) != 0 {
				t.Fatalf("written: %v %v", m, err)
			}
		})
	}
}
