package home

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	cpaconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
)

func TestCPAExecutionMalformedDispatchFailsWithoutMutation(t *testing.T) {
	for _, options := range []any{
		"invalid-object",
		map[string]any{"connection-pool": map[string]any{"enabled": "invalid"}},
		map[string]any{"connection-pool": map[string]any{"max-idle-conns-per-host": "invalid"}},
		map[string]any{"models": "invalid-list"},
		map[string]any{"models": []any{map[string]any{"name": "model", "openapi": "invalid"}}},
	} {
		auth := &coreauth.Auth{ID: "malformed-dispatch", Provider: "vertex", Attributes: map[string]string{"vertex_model_openapi": "unchanged"}, Metadata: map[string]any{"credential_options": options, "home_config": map[string]any{"kept": true}}}
		before, errBefore := json.Marshal(auth)
		if errBefore != nil {
			t.Fatal(errBefore)
		}
		out, errSanitize := SanitizeAuthForDownstream(auth, nil)
		var typeError *json.UnmarshalTypeError
		if out != nil || !errors.As(errSanitize, &typeError) {
			t.Fatalf("malformed execution options produced auth=%+v error=%v", out, errSanitize)
		}
		payload, errRefresh := BuildRefreshPayload(auth, nil)
		if payload != nil || !errors.As(errRefresh, &typeError) {
			t.Fatalf("refresh hid projection error: %v", errRefresh)
		}
		after, errAfter := json.Marshal(auth)
		if errAfter != nil {
			t.Fatal(errAfter)
		}
		if !reflect.DeepEqual(before, after) {
			t.Fatal("failed projection mutated the authoritative credential")
		}
	}
}

func TestCPAExecutionTypedVertexOptionsAndNullPool(t *testing.T) {
	enabled := true
	auth := &coreauth.Auth{Provider: "vertex", Attributes: map[string]string{}, Metadata: map[string]any{"credential_options": map[string]any{
		"models":          []cpaconfig.VertexCompatModel{{Name: "upstream", Alias: "alias", OpenAPI: &enabled}},
		"connection-pool": map[string]any{},
	}}}
	downstream, errSanitize := SanitizeAuthForDownstream(auth, nil)
	if errSanitize != nil {
		t.Fatal(errSanitize)
	}
	if len(auth.Attributes) != 0 {
		t.Fatal("projection mutated an empty attribute map on the source credential")
	}
	if downstream.ProxyConnectionPool == nil || downstream.Attributes["vertex_model_openapi"] == "" {
		t.Fatal("typed options or empty pool opt-in were lost")
	}
	downstream.Metadata["credential_options"] = map[string]any{"models": nil, "connection-pool": nil}
	cleared, errClear := SanitizeAuthForDownstream(downstream, nil)
	if errClear != nil {
		t.Fatal(errClear)
	}
	if cleared.ProxyConnectionPool != nil || cleared.Attributes["vertex_model_openapi"] != "" {
		t.Fatal("explicit null retained stale execution projections")
	}
}
