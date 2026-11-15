package config

import (
	"reflect"
	"testing"

	cpaconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"gopkg.in/yaml.v3"
)

func TestSDKExecutionOptionsWithoutHomeFields(t *testing.T) {
	for _, check := range []struct {
		owner reflect.Type
		field string
	}{
		{reflect.TypeOf(Config{}), "Client"},
		{reflect.TypeOf(Config{}), "ShutdownTimeoutSeconds"},
		{reflect.TypeOf(ClaudeConfig{}), "SessionDatePinDays"},
		{reflect.TypeOf(ClaudeHeaderDefaults{}), "NativeEntrypoints"},
		{reflect.TypeOf(CodexKey{}), "ConnectionPool"},
		{reflect.TypeOf(VertexCompatModel{}), "OpenAPI"},
	} {
		if _, exists := check.owner.FieldByName(check.field); exists {
			t.Fatalf("CPA-only field %s.%s was duplicated in Home", check.owner.Name(), check.field)
		}
	}
	source := []byte(`config-version: 8
server:
  response-compression: {enabled: false, min-bytes: 0}
  github-token: ''
  kv-store: {disabled: false, path: ''}
client:
  codex:
    web-search-result-cache: {enabled: false, ttl: 24h, limit: 0}
upstream:
  claude:
    session-date-pin-days: 0
    header-defaults:
      native-entrypoints: []
      inject-diagnostics: null
`)
	if errValidate := ValidateV8Config(source); errValidate != nil {
		t.Fatal(errValidate)
	}
	cfg := Config{CredentialConcurrency: DefaultCredentialConcurrencyConfig(), CredentialInFlight: DefaultCredentialInFlightConfig()}
	if errDecode := yaml.Unmarshal(source, &cfg); errDecode != nil {
		t.Fatal(errDecode)
	}
	var submitted map[string]any
	if errDecode := yaml.Unmarshal(source, &submitted); errDecode != nil {
		t.Fatal(errDecode)
	}
	if !reflect.DeepEqual(cfg.RawConfig, submitted) {
		t.Fatal("the submitted configuration tree was split or changed")
	}
	root, errRoot := NormalizeConfigRoot(cfg.RawConfig)
	if errRoot != nil {
		t.Fatal(errRoot)
	}
	if root["github-token"] != "" {
		t.Fatal("explicit empty string was lost")
	}
	compression := root["response-compression"].(map[string]any)
	if compression["enabled"] != false || compression["min-bytes"] != 0 {
		t.Fatal("explicit false or zero was lost")
	}
	headers := root["claude-header-defaults"].(map[string]any)
	if value, exists := headers["inject-diagnostics"]; !exists || value != nil {
		t.Fatal("explicit null was lost")
	}
	if value, exists := headers["native-entrypoints"]; !exists || len(value.([]any)) != 0 {
		t.Fatal("explicit empty list was lost")
	}
	// MarshalYAML and the legacy projection must both preserve opaque options.
	for _, legacy := range []bool{false, true} {
		var document any = cfg
		if legacy {
			root, errRoot := LegacyConfigRoot(&cfg)
			if errRoot != nil {
				t.Fatal(errRoot)
			}
			document = root
		}
		wire, errMarshal := yaml.Marshal(document)
		if errMarshal != nil {
			t.Fatal(errMarshal)
		}
		cpa, errParse := cpaconfig.ParseConfigBytes(wire)
		if errParse != nil {
			t.Fatal(errParse)
		}
		if cpa.Claude.SessionDatePinDays == nil || *cpa.Claude.SessionDatePinDays != 0 {
			t.Fatal("SDK-only setting did not reach CPA")
		}
		canonical, _, errNormalize := NormalizeConfigLayout(wire, true)
		if errNormalize != nil {
			t.Fatal(errNormalize)
		}
		if errValidate := ValidateV8Config(canonical); errValidate != nil {
			t.Fatal(errValidate)
		}
		var restored Config
		if errDecode := yaml.Unmarshal(canonical, &restored); errDecode != nil {
			t.Fatal(errDecode)
		}
		restoredRoot, errRoot := NormalizeConfigRoot(restored.RawConfig)
		if errRoot != nil {
			t.Fatal(errRoot)
		}
		if !legacy && !reflect.DeepEqual(root, restoredRoot) {
			t.Fatal("configuration tree changed across layout conversion")
		}
		restoredHeaders := restoredRoot["claude-header-defaults"].(map[string]any)
		if value, exists := restoredHeaders["inject-diagnostics"]; !exists || value != nil {
			t.Fatal("null SDK option was lost")
		}
		if value, exists := restoredHeaders["native-entrypoints"]; !exists || len(value.([]any)) != 0 {
			t.Fatal("empty SDK option was lost")
		}
	}
}

func TestClaudeCloakPatternsTypedSDKProjection(t *testing.T) {
	entry := ClaudeKey{Cloak: &CloakConfig{SensitivePatternsReplace: []SensitivePatternRule{
		{Pattern: "^private", Replacement: ""},
		{Pattern: "[invalid", Replacement: "ignored"},
		{Pattern: "Claude[ ]+Code", Replacement: "Claude"},
	}}}
	entry.Cloak = NormalizeCloakConfig(entry.Cloak)
	want := []SensitivePatternRule{
		{Pattern: "^private", Replacement: ""},
		{Pattern: "Claude[ ]+Code", Replacement: "Claude"},
	}
	if !reflect.DeepEqual(entry.Cloak.SensitivePatternsReplace, want) {
		t.Fatalf("normalized cloak patterns = %#v, want %#v", entry.Cloak.SensitivePatternsReplace, want)
	}
	options, errProject := CredentialExecutionOptions(entry)
	if errProject != nil {
		t.Fatal(errProject)
	}
	wantWire := []any{
		map[string]any{"pattern": "^private", "replacement": ""},
		map[string]any{"pattern": "Claude[ ]+Code", "replacement": "Claude"},
	}
	cloak, ok := options["cloak"].(map[string]any)
	if !ok || !reflect.DeepEqual(cloak["sensitive-patterns-replace"], wantWire) {
		t.Fatalf("SDK cloak projection = %#v, want patterns %#v", options["cloak"], wantWire)
	}
}

func TestSDKExecutionSchemaRejectsUnknownAndMalformedOptions(t *testing.T) {
	for _, source := range []string{
		"client: {codex: {future-typo: true}}",
		"upstream: {claude: {header-defaults: {native-entrypointz: []}}}",
		"server: {response-compression: {enabled: invalid}}",
		"api-keys: {codex: [{keys: [{api-key: test, unknown-option: true}]}]}",
		"api-keys: {claude: [{keys: [{api-key: test, cloak: {unknown-option: true}}]}]}",
		"api-keys: {vertex: [{keys: [{api-key: test, models: [{name: test, alias: test, unknown-option: true}]}]}]}",
	} {
		if errValidate := ValidateV8Config([]byte(source)); errValidate == nil {
			t.Fatalf("accepted invalid execution option: %s", source)
		}
	}
}

func TestSDKCredentialOptionsRespectGroupAndKeyOwnership(t *testing.T) {
	options := map[string]any{
		"models":          []any{map[string]any{"name": "model", "alias": "alias"}},
		"weight":          2,
		"connection-pool": map[string]any{"enabled": false},
		"refresh_token":   "must-not-be-forwarded",
	}
	group, errGroup := CredentialConfigFromOptions(OpenAICompatibility{}, options)
	if errGroup != nil {
		t.Fatal(errGroup)
	}
	if _, exists := group["weight"]; exists {
		t.Fatal("key weight leaked into its provider group")
	}
	key, errKey := CredentialConfigFromOptions(OpenAICompatibilityAPIKey{}, options)
	if errKey != nil {
		t.Fatal(errKey)
	}
	if _, exists := key["models"]; exists {
		t.Fatal("group models leaked into its key entry")
	}
	key["refresh_token"] = "must-not-be-forwarded"
	entry := OpenAICompatibilityAPIKey{CredentialConfig: CredentialConfig{RawConfig: key}}
	projected, errProject := CredentialExecutionOptions(entry)
	if errProject != nil {
		t.Fatal(errProject)
	}
	if _, exists := projected["refresh_token"]; exists {
		t.Fatal("unknown refresh material was forwarded")
	}
	if pool, exists := projected["connection-pool"]; !exists || pool.(map[string]any)["enabled"] != false {
		t.Fatal("valid SDK execution option was lost")
	}

}

func TestSDKExecutionValidationAlsoCoversImports(t *testing.T) {
	for _, source := range []string{
		"client: {codex: {web-search-result-cache: {enabled: invalid}}}",
		"vertex-api-key: [{api-key: test, models: [{name: model, alias: alias, openapi: invalid}]}]",
		"claude-api-key: [{api-key: test, cloak: {sensitive-patterns-replace: invalid}}]",
	} {
		var cfg Config
		if errDecode := yaml.Unmarshal([]byte(source), &cfg); errDecode == nil {
			t.Fatalf("import accepted malformed SDK option: %s", source)
		}
	}
}

func TestSDKExecutionCredentialDedupRetainsNestedOptions(t *testing.T) {
	var cfg Config
	if errDecode := yaml.Unmarshal([]byte(`vertex-api-key:
  - api-key: shared
    base-url: https://example.com
    models: [{name: model, alias: alias, openapi: false}]
  - api-key: shared
    base-url: https://example.com
    models: [{name: model, alias: alias, openapi: true}]
`), &cfg); errDecode != nil {
		t.Fatal(errDecode)
	}
	cfg.SanitizeVertexCompatKeys()
	if len(cfg.VertexCompatAPIKey) != 2 {
		t.Fatal("deduplication discarded a nested SDK option override")
	}
}

func TestConfigTreeIsAuthoritative(t *testing.T) {
	source := []byte(`config-version: 8
routing: {session-affinity-rebind-on: quota}
client:
  codex:
    web-search-result-cache: {enabled: false, ttl: 24h, limit: 0}
`)
	var cfg Config
	if errDecode := yaml.Unmarshal(source, &cfg); errDecode != nil {
		t.Fatal(errDecode)
	}
	var original map[string]any
	if errDecode := yaml.Unmarshal(source, &original); errDecode != nil {
		t.Fatal(errDecode)
	}
	// Changing a runtime projection must not rewrite the saved configuration.
	cfg.Port = 9999
	cfg.Routing.SessionAffinityRebindOn = "never"
	wire, errMarshal := yaml.Marshal(&cfg)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	var saved map[string]any
	if errDecode := yaml.Unmarshal(wire, &saved); errDecode != nil {
		t.Fatal(errDecode)
	}
	if !reflect.DeepEqual(original, saved) {
		t.Fatal("runtime fields or defaults rewrote the complete configuration tree")
	}
	// A tree edit takes effect when the read projection is reloaded.
	cfg.RawConfig["routing"].(map[string]any)["session-affinity-rebind-on"] = "never"
	cache := cfg.RawConfig["client"].(map[string]any)["codex"].(map[string]any)["web-search-result-cache"].(map[string]any)
	cache["ttl"] = "12h"
	wire, errMarshal = yaml.Marshal(&cfg)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	var reloaded Config
	if errDecode := yaml.Unmarshal(wire, &reloaded); errDecode != nil {
		t.Fatal(errDecode)
	}
	if reloaded.Routing.SessionAffinityRebindOn != "never" {
		t.Fatal("Home policy did not reload from the edited tree")
	}
	if ttl := reloaded.RawConfig["client"].(map[string]any)["codex"].(map[string]any)["web-search-result-cache"].(map[string]any)["ttl"]; ttl != "12h" {
		t.Fatal("CPA option was not retained")
	}
	if errDecode := yaml.Unmarshal([]byte("routing: {session-affinity-rebind-on: any}"), &reloaded); errDecode != nil {
		t.Fatal(errDecode)
	}
	if _, exists := reloaded.RawConfig["client"]; exists {
		t.Fatal("replacement retained an old configuration subtree")
	}
}
