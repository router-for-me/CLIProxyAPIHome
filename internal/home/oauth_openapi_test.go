package home

import (
	"encoding/json"
	"reflect"
	"testing"

	cpaconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"gopkg.in/yaml.v3"
)

func TestOAuthVertexOpenAPIProjectionAndRefresh(t *testing.T) {
	var cfg config.Config
	if errDecode := yaml.Unmarshal([]byte(`config-version: 8
oauth:
  model-alias:
    vertex:
      - {name: gemini-2.5-flash, alias: force-openapi, openapi: true, fork: true, force-mapping: true, display-name: Public}
      - {name: gemini-2.5-flash, alias: force-native, openapi: false}
      - {name: gemini-2.5-flash, alias: automatic}
      - {name: gemini-2.5-flash, alias: null-value, openapi: null}
`), &cfg); errDecode != nil {
		t.Fatal(errDecode)
	}
	cfg.SanitizeOAuthModelAlias()
	for _, attrs := range []map[string]string{nil, {}} {
		auth := &coreauth.Auth{ID: "oauth-vertex", Index: "oauth-vertex", Provider: "vertex", Prefix: "team", Attributes: attrs, Metadata: map[string]any{
			"access_token": "fixture-access", "refresh_token": "fixture-refresh", "home_config": map[string]any{"private": true},
		}}
		before, errBefore := json.Marshal(auth)
		if errBefore != nil {
			t.Fatal(errBefore)
		}
		out, errSanitize := SanitizeAuthForDownstream(auth, &cfg)
		if errSanitize != nil {
			t.Fatal(errSanitize)
		}
		var rules []cpaconfig.VertexCompatModel
		if errDecode := json.Unmarshal([]byte(out.Attributes["vertex_model_openapi"]), &rules); errDecode != nil {
			t.Fatal(errDecode)
		}
		if len(rules) != 2 || rules[0].Name != "gemini-2.5-flash" || rules[0].Alias != "force-openapi" || rules[0].OpenAPI == nil || !*rules[0].OpenAPI || rules[1].Alias != "force-native" || rules[1].OpenAPI == nil || *rules[1].OpenAPI {
			t.Fatalf("wrong OAuth execution rules: %s", out.Attributes["vertex_model_openapi"])
		}
		var minimal []map[string]any
		if errDecode := json.Unmarshal([]byte(out.Attributes["vertex_model_openapi"]), &minimal); errDecode != nil {
			t.Fatal(errDecode)
		}
		for _, rule := range minimal {
			if len(rule) != 3 {
				t.Fatalf("Home-only alias settings reached CPA: %+v", rule)
			}
		}
		payload, errRefresh := BuildRefreshPayload(auth, &cfg)
		if errRefresh != nil {
			t.Fatal(errRefresh)
		}
		var refresh struct {
			Auth coreauth.Auth `json:"auth"`
		}
		if errDecode := json.Unmarshal(payload, &refresh); errDecode != nil {
			t.Fatal(errDecode)
		}
		if refresh.Auth.Attributes["vertex_model_openapi"] != out.Attributes["vertex_model_openapi"] {
			t.Fatal("refresh lost the OAuth execution rules")
		}
		for _, private := range []string{"refresh_token", "home_config"} {
			if _, exists := refresh.Auth.Metadata[private]; exists {
				t.Fatalf("refresh exposed %s", private)
			}
		}
		after, errAfter := json.Marshal(auth)
		if errAfter != nil {
			t.Fatal(errAfter)
		}
		if !reflect.DeepEqual(before, after) {
			t.Fatal("projection changed the authoritative credential")
		}
		cleared, errClear := SanitizeAuthForDownstream(auth, &config.Config{})
		if errClear != nil {
			t.Fatal(errClear)
		}
		if _, exists := cleared.Attributes["vertex_model_openapi"]; exists {
			t.Fatal("removing OAuth settings retained their execution projection")
		}
	}
}

func TestOAuthVertexOpenAPIPrecedenceAndIsolation(t *testing.T) {
	enabled, disabled := true, false
	cfg := &config.Config{OAuthModelAlias: map[string][]config.OAuthModelAlias{"vertex": {{Name: "upstream", Alias: "public", OpenAPI: &enabled}}}}
	for _, tc := range []struct {
		name      string
		auth      *coreauth.Auth
		wantRules int
	}{
		{"credential-model", &coreauth.Auth{Provider: "vertex", Metadata: map[string]any{"credential_options": map[string]any{"models": []cpaconfig.VertexCompatModel{{Name: "upstream", Alias: "public", OpenAPI: &disabled}}}}}, 2},
		{"legacy-routing", &coreauth.Auth{Provider: "vertex", Attributes: map[string]string{"vertex_model_openapi": `[{"name":"upstream","alias":"public","openapi":false}]`}}, 2},
		{"api-key", &coreauth.Auth{Provider: "vertex", Attributes: map[string]string{"api_key": "fixture"}}, 0},
		{"api-key-kind", &coreauth.Auth{Provider: "vertex", Attributes: map[string]string{"auth_kind": "api_key"}}, 0},
		{"other-provider", &coreauth.Auth{Provider: "codex"}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, errSanitize := SanitizeAuthForDownstream(tc.auth, cfg)
			if errSanitize != nil {
				t.Fatal(errSanitize)
			}
			raw := out.Attributes["vertex_model_openapi"]
			if tc.wantRules == 0 {
				if raw != "" {
					t.Fatalf("OAuth settings affected another credential: %s", raw)
				}
				return
			}
			var rules []cpaconfig.VertexCompatModel
			if errDecode := json.Unmarshal([]byte(raw), &rules); errDecode != nil {
				t.Fatal(errDecode)
			}
			if len(rules) != tc.wantRules || rules[0].OpenAPI == nil || *rules[0].OpenAPI || rules[1].OpenAPI == nil || !*rules[1].OpenAPI {
				t.Fatalf("credential rules lost precedence: %s", raw)
			}
		})
	}
	if out, errSanitize := SanitizeAuthForDownstream(&coreauth.Auth{Provider: "vertex", Attributes: map[string]string{"vertex_model_openapi": "malformed"}}, cfg); errSanitize == nil || out != nil {
		t.Fatalf("invalid credential routing was silently replaced: auth=%+v err=%v", out, errSanitize)
	}
}
