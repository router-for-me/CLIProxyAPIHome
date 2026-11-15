package home

import (
	"encoding/json"
	"testing"
	"time"

	cpaauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cpaconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
	"github.com/router-for-me/CLIProxyAPIHome/internal/watcher/synthesizer"
	"gopkg.in/yaml.v3"
)

func TestCPAExecutionDownstreamProjection(t *testing.T) {
	source := []byte(`config-version: 8
server: {max-request-body-bytes: 4096, shutdown-timeout: 20, github-token: node-token, response-compression: {enabled: false, min-bytes: 512}}
client:
  codex: {optimize-multi-agent-v2: true, enable-apply-patch: true, web-search-result-cache: {enabled: true, ttl: 12h, limit: 42}}
  claude: {show-codex-commentary: true}
upstream:
  codex: {upstream-request-compression: true, upstream-request-compression-min-bytes: 2048, websocket-compression: false}
  claude: {server-side-fallback: true, header-defaults: {native-entrypoints: [t3-code], anthropic-beta-strip: [old], anthropic-beta-append: [new], inject-diagnostics: false, client-request-id: off}}
oauth:
  settings: {codex: [{name: test-model, max-context-length: 524288}]}
  providers: {codex: {signin-with-chatgpt-identifier: test-client}, antigravity: {tos-pause-window: 3h}, devin: {sensitive-patterns-replace: [{pattern: '^old', replacement: new}]}}
`)
	wire, errSanitize := sanitizeConfigYAMLForDownstream(source)
	if errSanitize != nil {
		t.Fatal(errSanitize)
	}
	cpa, errParse := cpaconfig.ParseConfigBytes(wire)
	if errParse != nil {
		t.Fatal(errParse)
	}
	if !cpa.Client.Codex.EnableApplyPatch || !cpa.Client.Codex.OptimizeMultiAgentV2 || !cpa.Client.Claude.ShowCodexCommentary || cpa.MaxRequestBodyBytes != 4096 {
		t.Fatalf("client/server configuration lost: %s", wire)
	}
	if !cpa.Codex.UpstreamRequestCompression || cpa.Codex.WebsocketCompression == nil || *cpa.Codex.WebsocketCompression {
		t.Fatal("shared provider options were lost")
	}
	// Some fields postdate the pinned SDK. Assert the compatible wire format
	// instead of requiring a dependency update for store-and-forward options.
	var root map[string]any
	if errDecode := yaml.Unmarshal(wire, &root); errDecode != nil {
		t.Fatal(errDecode)
	}
	if root["shutdown-timeout"] != 20 || root["github-token"] != "node-token" {
		t.Fatal("CPA node operation options were lost")
	}
	if root["claude"].(map[string]any)["server-side-fallback"] != true {
		t.Fatal("Claude server-side fallback option was lost")
	}
	claude := root["claude-header-defaults"].(map[string]any)
	if len(claude["native-entrypoints"].([]any)) != 1 || claude["inject-diagnostics"] != false || claude["client-request-id"] != "off" {
		t.Fatal("Claude header defaults were lost")
	}
}

func TestCPAExecutionEmptyCredentialPool(t *testing.T) {
	for _, present := range []bool{false, true} {
		auth := &coreauth.Auth{Metadata: map[string]any{"credential_options": map[string]any{}}}
		if present {
			auth.Metadata["credential_options"].(map[string]any)["connection-pool"] = map[string]any{}
		}
		sanitized, errSanitize := SanitizeAuthForDownstream(auth, nil)
		if errSanitize != nil {
			t.Fatal(errSanitize)
		}
		encoded, errMarshal := json.Marshal(sanitized)
		if errMarshal != nil {
			t.Fatal(errMarshal)
		}
		var downstream cpaauth.Auth
		if errDecode := json.Unmarshal(encoded, &downstream); errDecode != nil {
			t.Fatal(errDecode)
		}
		if (downstream.ProxyConnectionPool != nil) != present {
			t.Fatalf("empty pool opt-in lost: %s", encoded)
		}
	}
}

func TestOAuthSettingsRegisteredCatalogAndDispatch(t *testing.T) {
	base := registry.GetCodexProModels()
	if len(base) == 0 {
		t.Fatal("missing built-in Codex models")
	}
	model := base[0].ID
	cfg := &config.Config{
		OAuthSettings:   map[string][]config.OAuthModelSetting{"codex": {{Name: model, MaxContextLength: 524288}}},
		OAuthModelAlias: map[string][]config.OAuthModelAlias{"codex": {{Name: model, Alias: "settings-alias"}}},
	}
	auth := &coreauth.Auth{ID: "oauth-settings-catalog", Provider: "codex", Status: coreauth.StatusActive}
	runtime := &Runtime{cfg: cfg}
	runtime.registerModelsForAuth(auth)
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	info := dispatchModelInfoForAuth(auth.ID, model, "settings-alias")
	if info == nil || info.ContextLength != 524288 {
		t.Fatalf("dispatch did not inherit OAuth context settings: %+v", info)
	}
	registered := registry.GetGlobalRegistry().GetModelInfoForClient(auth.ID, "settings-alias")
	if registered == nil || registered.ContextLength != 524288 {
		t.Fatal("alias lost the configured OAuth context window")
	}
}

func TestOAuthSettingsHomeCatalogAndAPIKeyIsolation(t *testing.T) {
	cfg := &config.Config{OAuthSettings: map[string][]config.OAuthModelSetting{
		"codex":       {{Name: "upstream-model", MaxContextLength: 524288}},
		"antigravity": {{Name: "upstream-model", MaxContextLength: 262144}},
	}}
	base := []*ModelInfo{{ID: "upstream-model", ContextLength: 1000}}
	models := applyOAuthSettings(cfg, "codex", "oauth", base)
	if len(models) != 1 || models[0].ContextLength != 524288 || base[0].ContextLength != 1000 {
		t.Fatal("OAuth context override was not applied without mutating the base catalog")
	}
	apiKeyModels := applyOAuthSettings(cfg, "codex", "apikey", base)
	if apiKeyModels[0] != base[0] || apiKeyModels[0].ContextLength != 1000 {
		t.Fatal("OAuth settings changed API-key model capabilities")
	}
	antigravity := applyOAuthSettings(cfg, "antigravity", "oauth", base)
	if antigravity[0].ContextLength != 262144 {
		t.Fatal("provider-scoped settings leaked across channels")
	}
}

func TestCPAExecutionVertexOpenAPIProjection(t *testing.T) {
	var cfg config.Config
	if errDecode := yaml.Unmarshal([]byte(`api-keys:
  vertex:
    - prefix: team
      models:
        - {name: gemini-2.5-flash, alias: force-openapi, openapi: true}
        - {name: gemini-2.5-flash, alias: force-native, openapi: false}
        - {name: gemini-2.5-pro, alias: automatic}
      keys: [{api-key: fixture-key}]
`), &cfg); errDecode != nil {
		t.Fatal(errDecode)
	}
	auths, errSynthesize := synthesizer.NewConfigSynthesizer().Synthesize(&synthesizer.SynthesisContext{Config: &cfg, Now: time.Now(), IDGenerator: synthesizer.NewStableIDGenerator()})
	if errSynthesize != nil || len(auths) != 1 {
		t.Fatalf("synthesis failed: %v", errSynthesize)
	}
	// Emulate existing DB records: the stored credential has model options but
	// no routing attribute. Dispatch must project it without a config reimport.
	encoded, errMarshal := json.Marshal(auths[0])
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	var stored coreauth.Auth
	if errDecode := json.Unmarshal(encoded, &stored); errDecode != nil {
		t.Fatal(errDecode)
	}
	delete(stored.Attributes, "vertex_model_openapi")
	downstream, errSanitize := SanitizeAuthForDownstream(&stored, nil)
	if errSanitize != nil {
		t.Fatal(errSanitize)
	}
	var rules []cpaconfig.VertexCompatModel
	if errDecode := json.Unmarshal([]byte(downstream.Attributes["vertex_model_openapi"]), &rules); errDecode != nil {
		t.Fatal(errDecode)
	}
	if len(rules) != 2 || rules[0].Name != "gemini-2.5-flash" || rules[0].Alias != "force-openapi" || rules[0].OpenAPI == nil || !*rules[0].OpenAPI || rules[1].Alias != "force-native" || rules[1].OpenAPI == nil || *rules[1].OpenAPI {
		t.Fatal("explicit model routing flags or aliases were lost")
	}
	if _, exists := stored.Attributes["vertex_model_openapi"]; exists {
		t.Fatal("dispatch projection mutated the stored credential")
	}
	stored.Attributes["vertex_model_openapi"] = downstream.Attributes["vertex_model_openapi"]
	stored.Metadata["credential_options"].(map[string]any)["models"] = []any{}
	cleared, errClear := SanitizeAuthForDownstream(&stored, nil)
	if errClear != nil {
		t.Fatal(errClear)
	}
	if _, exists := cleared.Attributes["vertex_model_openapi"]; exists {
		t.Fatal("empty model configuration retained stale routing rules")
	}
	delete(stored.Metadata, "credential_options")
	legacy, errLegacy := SanitizeAuthForDownstream(&stored, nil)
	if errLegacy != nil {
		t.Fatal(errLegacy)
	}
	if legacy.Attributes["vertex_model_openapi"] == "" {
		t.Fatal("legacy routing attributes were removed without authoritative model options")
	}
}
