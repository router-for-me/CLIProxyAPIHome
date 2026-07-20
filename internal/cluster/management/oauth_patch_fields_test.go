package management

import (
	"encoding/json"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
)

func TestApplyOAuthFieldPatchArbitraryFields(t *testing.T) {
	auth := &coreauth.Auth{
		ID:         "codex-auth",
		Provider:   "codex",
		Attributes: map[string]string{"websockets": "true"},
		Metadata: map[string]any{
			"type":       "codex",
			"websockets": true,
		},
	}
	fields := mustRawFields(t, `{"abc":true,"nested.cde":true,"fgh":{"ijk":true},"websockets":false}`)

	changed, errPatch := applyOAuthFieldPatch(auth, fields)
	if errPatch != nil {
		t.Fatalf("applyOAuthFieldPatch returned error: %v", errPatch)
	}
	if !changed {
		t.Fatalf("applyOAuthFieldPatch changed = false, want true")
	}
	if got := auth.Metadata["abc"]; got != true {
		t.Fatalf("metadata.abc = %#v, want true", got)
	}
	nested, ok := auth.Metadata["nested"].(map[string]any)
	if !ok {
		t.Fatalf("metadata.nested = %#v, want object", auth.Metadata["nested"])
	}
	if got := nested["cde"]; got != true {
		t.Fatalf("metadata.nested.cde = %#v, want true", got)
	}
	fgh, ok := auth.Metadata["fgh"].(map[string]any)
	if !ok {
		t.Fatalf("metadata.fgh = %#v, want object", auth.Metadata["fgh"])
	}
	if got := fgh["ijk"]; got != true {
		t.Fatalf("metadata.fgh.ijk = %#v, want true", got)
	}
	if got, ok := auth.Metadata["websockets"].(bool); !ok || got {
		t.Fatalf("metadata.websockets = %#v, want false", auth.Metadata["websockets"])
	}
	if got := auth.Attributes["websockets"]; got != "false" {
		t.Fatalf("attributes.websockets = %q, want false", got)
	}
}

func TestApplyOAuthFieldPatchConcurrencyLimits(t *testing.T) {
	auth := &coreauth.Auth{ID: "codex-auth", Provider: "codex"}
	fields := mustRawFields(t, `{"max_in_flight":3,"max_in_flight_by_model":{"gpt-5(high)":2,"gpt-4.1":0}}`)

	changed, errPatch := applyOAuthFieldPatch(auth, fields)
	if errPatch != nil || !changed {
		t.Fatalf("applyOAuthFieldPatch() = %v, %v", changed, errPatch)
	}
	if auth.MaxInFlight != 3 {
		t.Fatalf("MaxInFlight = %d, want 3", auth.MaxInFlight)
	}
	if len(auth.MaxInFlightByModel) != 1 || auth.MaxInFlightByModel["gpt-5"] != 2 {
		t.Fatalf("MaxInFlightByModel = %#v, want gpt-5=2", auth.MaxInFlightByModel)
	}
	if auth.Metadata["max_in_flight"] != 3 {
		t.Fatalf("metadata max_in_flight = %#v, want 3", auth.Metadata["max_in_flight"])
	}
	metadataLimits, ok := auth.Metadata["max_in_flight_by_model"].(map[string]int)
	if !ok || len(metadataLimits) != 1 || metadataLimits["gpt-5"] != 2 {
		t.Fatalf("metadata model limits = %#v, want gpt-5=2", auth.Metadata["max_in_flight_by_model"])
	}
	auth.Metadata["in_flight"] = 99
	payload := authFilePayload(auth)
	if payload["max_in_flight"] != 3 {
		t.Fatalf("auth file max_in_flight = %#v, want 3", payload["max_in_flight"])
	}
	modelLimits, ok := payload["max_in_flight_by_model"].(map[string]int)
	if !ok || len(modelLimits) != 1 || modelLimits["gpt-5"] != 2 {
		t.Fatalf("auth file model limits = %#v, want gpt-5=2", payload["max_in_flight_by_model"])
	}
	if _, exists := payload["in_flight"]; exists {
		t.Fatalf("auth file payload retained derived count: %#v", payload)
	}

	clearFields := mustRawFields(t, `{"max_in_flight":null,"max_in_flight_by_model":{}}`)
	if _, errClear := applyOAuthFieldPatch(auth, clearFields); errClear != nil {
		t.Fatalf("clear concurrency fields: %v", errClear)
	}
	if auth.MaxInFlight != 0 || auth.MaxInFlightByModel != nil {
		t.Fatalf("cleared limits = %d, %#v", auth.MaxInFlight, auth.MaxInFlightByModel)
	}
	if _, exists := auth.Metadata["max_in_flight"]; exists {
		t.Fatalf("cleared auth retained metadata max_in_flight: %#v", auth.Metadata)
	}
	if _, exists := auth.Metadata["max_in_flight_by_model"]; exists {
		t.Fatalf("cleared auth retained metadata model limits: %#v", auth.Metadata)
	}
	clearedPayload := authFilePayload(auth)
	if _, exists := clearedPayload["max_in_flight"]; exists {
		t.Fatalf("cleared auth file retained max_in_flight: %#v", clearedPayload)
	}
	if _, exists := clearedPayload["max_in_flight_by_model"]; exists {
		t.Fatalf("cleared auth file retained max_in_flight_by_model: %#v", clearedPayload)
	}
}

func TestApplyOAuthFieldPatchRejectsInvalidConcurrencyLimits(t *testing.T) {
	tests := []struct {
		name    string
		payload string
	}{
		{name: "negative total", payload: `{"max_in_flight":-1}`},
		{name: "fractional total", payload: `{"max_in_flight":1.5}`},
		{name: "numeric string total", payload: `{"max_in_flight":"2"}`},
		{name: "model limits must be object", payload: `{"max_in_flight_by_model":[]}`},
		{name: "blank model", payload: `{"max_in_flight_by_model":{" ":1}}`},
		{name: "null model", payload: `{"max_in_flight_by_model":{"gpt-5":null}}`},
		{name: "negative model", payload: `{"max_in_flight_by_model":{"gpt-5":-1}}`},
		{name: "duplicate trimmed model", payload: `{"max_in_flight_by_model":{"gpt-5":1," gpt-5 ":2}}`},
		{name: "read-only in-flight count", payload: `{"in_flight":1}`},
		{name: "read-only remaining count", payload: `{"remaining":1}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			auth := &coreauth.Auth{ID: "codex-auth", Provider: "codex"}
			if _, errPatch := applyOAuthFieldPatch(auth, mustRawFields(t, test.payload)); errPatch == nil {
				t.Fatalf("applyOAuthFieldPatch(%s) error = nil", test.payload)
			}
		})
	}
}

func mustRawFields(t *testing.T, payload string) map[string]json.RawMessage {
	t.Helper()
	var fields map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal([]byte(payload), &fields); errUnmarshal != nil {
		t.Fatalf("failed to unmarshal raw fields: %v", errUnmarshal)
	}
	return fields
}
