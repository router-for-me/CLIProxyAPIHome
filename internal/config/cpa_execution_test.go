package config

import (
	"os"
	"reflect"
	"strings"
	"testing"

	cpaconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"gopkg.in/yaml.v3"
)

func TestCPAExecutionConfigRoundTrip(t *testing.T) {
	source := []byte(`config-version: 8
server:
  max-request-body-bytes: 1048576
  response-compression: {enabled: false, min-bytes: 2048}
  shutdown-timeout: 30
  github-token: test-token
routing: {session-affinity-rebind-on: quota}
client:
  codex:
    optimize-multi-agent-v2: true
    enable-apply-patch: true
    web-search-result-cache: {enabled: true, ttl: 12h, limit: 42}
  claude: {show-codex-commentary: true}
upstream:
  codex:
    upstream-request-compression: true
    upstream-request-compression-min-bytes: 4096
    websocket-compression: false
  claude:
    server-side-fallback: true
    header-defaults:
      native-entrypoints: [t3-code]
      anthropic-beta-strip: [old-beta]
      anthropic-beta-append: [new-beta]
      inject-diagnostics: false
      client-request-id: off
      timezone: UTC
oauth:
  settings: {codex: [{name: test-model, max-context-length: 524288}]}
  providers:
    codex: {signin-with-chatgpt-identifier: test-client}
    antigravity:
      tos-pause-window: 3h
      sensitive-patterns-replace: [{pattern: '^old', replacement: new}]
    devin:
      sensitive-patterns-replace: [{pattern: '^old', replacement: ''}]
`)
	if errValidate := ValidateV8Config(source); errValidate != nil {
		t.Fatal(errValidate)
	}
	cfg := Config{CredentialConcurrency: DefaultCredentialConcurrencyConfig(), CredentialInFlight: DefaultCredentialInFlightConfig()}
	if errDecode := yaml.Unmarshal(source, &cfg); errDecode != nil {
		t.Fatal(errDecode)
	}
	root, errRoot := NormalizeConfigRoot(cfg.RawConfig)
	if errRoot != nil {
		t.Fatal(errRoot)
	}
	wire, errMarshal := yaml.Marshal(&cfg)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	cpa, errParse := cpaconfig.ParseConfigBytes(wire)
	if errParse != nil {
		t.Fatal(errParse)
	}
	if !cpa.Client.Codex.OptimizeMultiAgentV2 || !cpa.Client.Codex.EnableApplyPatch || !cpa.Client.Claude.ShowCodexCommentary || cpa.Client.Codex.WebSearchResultCache.Limit != 42 {
		t.Fatalf("client projection = %+v", cpa.Client)
	}
	if !cpa.Codex.UpstreamRequestCompression || cpa.Codex.UpstreamRequestCompressionMinBytes != 4096 || cpa.Codex.WebsocketCompression == nil || *cpa.Codex.WebsocketCompression {
		t.Fatalf("Codex projection = %+v", cpa.Codex)
	}
	if cpa.MaxRequestBodyBytes != 1048576 || cpa.ResponseCompression.Enabled == nil || *cpa.ResponseCompression.Enabled {
		t.Fatal("server or Claude settings were lost")
	}
	canonical, _, errMigrate := NormalizeConfigLayout(wire, true)
	if errMigrate != nil {
		t.Fatal(errMigrate)
	}
	if errValidate := ValidateV8Config(canonical); errValidate != nil {
		t.Fatal(errValidate)
	}
	var restored Config
	if errDecode := yaml.Unmarshal(canonical, &restored); errDecode != nil {
		t.Fatal(errDecode)
	}
	restoredRoot, errRestored := NormalizeConfigRoot(restored.RawConfig)
	if errRestored != nil {
		t.Fatal(errRestored)
	}
	if !reflect.DeepEqual(restoredRoot, root) || restored.Codex.OptimizeMultiAgentV2 != cfg.Codex.OptimizeMultiAgentV2 || !reflect.DeepEqual(restored.OAuthSettings, cfg.OAuthSettings) {
		t.Fatal("configuration changed across legacy/v8 round trip")
	}
}

func TestCPAExecutionCredentialGroupInheritance(t *testing.T) {
	source := []byte(`api-keys:
  codex:
    - base-url: https://example.test
      connection-pool: {enabled: true, idle-conn-timeout: 300s, max-idle-conns-per-host: 7}
      keys:
        - api-key: first
          connection-pool: {enabled: false}
        - api-key: second
          connection-pool: {}
        - api-key: third
          connection-pool: null
  vertex:
    - interactions: true
      keys: [{api-key: vertex-key}]
`)
	if errValidate := ValidateV8Config(source); errValidate != nil {
		t.Fatal(errValidate)
	}
	var cfg Config
	if errDecode := yaml.Unmarshal(source, &cfg); errDecode != nil {
		t.Fatal(errDecode)
	}
	for i, key := range cfg.CodexKey {
		pool, ok := key.RawConfig["connection-pool"].(map[string]any)
		if !ok || pool["enabled"] != (i != 0) || pool["idle-conn-timeout"] != "300s" || pool["max-idle-conns-per-host"] != 7 {
			t.Fatalf("key %d pool = %+v", i, pool)
		}
	}
	if cfg.VertexCompatAPIKey[0].RawConfig["interactions"] != true {
		t.Fatal("Vertex group capability was lost")
	}
}

func TestCPAExecutionCredentialDedupKeepsOptions(t *testing.T) {
	var cfg Config
	if errDecode := yaml.Unmarshal([]byte(`gemini-api-key:
  - {api-key: same-key, connection-pool: {enabled: true}}
  - {api-key: same-key, connection-pool: {enabled: false}}
vertex-api-key:
  - {api-key: same-key}
  - {api-key: same-key, interactions: true}
  - {api-key: same-key, connection-pool: {}}
`), &cfg); errDecode != nil {
		t.Fatal(errDecode)
	}
	cfg.SanitizeGeminiKeys()
	cfg.SanitizeVertexCompatKeys()
	if len(cfg.GeminiKey) != 2 || len(cfg.VertexCompatAPIKey) != 3 {
		t.Fatal("deduplication discarded distinct credential execution options")
	}
}

func TestCPAExecutionExampleConfig(t *testing.T) {
	data, errRead := os.ReadFile("../../config.example.yaml")
	if errRead != nil {
		t.Fatal(errRead)
	}
	if errValidate := ValidateV8Config(data); errValidate != nil {
		t.Fatal(errValidate)
	}
	cpa, errParse := cpaconfig.ParseConfigBytes(data)
	if errParse != nil {
		t.Fatal(errParse)
	}
	if cpa.Antigravity.TOSPauseWindow != "0" {
		t.Fatal("example must disable the TOS pool pause by default")
	}
	if cpa.Claude.SessionDatePinDays == nil || *cpa.Claude.SessionDatePinDays != 7 {
		t.Fatal("example must document the session date pin default")
	}
	if strings.Count(string(data), "      # sensitive-patterns-replace:") != 1 {
		t.Fatal("Antigravity must have a single regex replacement example")
	}
	var root yaml.Node
	if errDecode := yaml.Unmarshal(data, &root); errDecode != nil {
		t.Fatal(errDecode)
	}
	if wsAuth := yamlPath(root.Content[0], "oauth.providers.aistudio.ws-auth"); wsAuth == nil || wsAuth.Tag != "!!bool" || wsAuth.Value != "false" {
		t.Fatal("example must retain ws-auth: false for compatibility")
	}
}

func TestCPAExecutionPolicyValidation(t *testing.T) {
	for _, source := range []string{
		"routing: {session-affinity-rebind-on: invalid}",
		"oauth: {providers: {antigravity: {tos-pause-window: invalid}}}",
		"client: {codex: {web-search-result-cache: {ttl: invalid}}}",
	} {
		if errValidate := ValidateV8Config([]byte(source)); errValidate == nil {
			t.Fatalf("accepted invalid policy: %s", source)
		}
	}
}
