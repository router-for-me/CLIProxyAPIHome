package cluster

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	cpaauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cpaconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	appconfig "github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/home"
	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
	"gopkg.in/yaml.v3"
)

func TestGeminiAPIKeyAliasDispatchFromSQLite(t *testing.T) {
	for _, tc := range []struct {
		name    string
		levels  []string
		options string
	}{
		{name: "default thinking"},
		{name: "configured thinking", levels: []string{"low", "medium", "high"}, options: ", thinking: {levels: [low, medium, high]}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			input := filepath.Join(t.TempDir(), "config.yaml")
			writeFile(t, input, strings.ReplaceAll(`config-version: 8
api-keys:
  gemini:
    - base-url: https://gemini.example.test
      models:
        - {name: agy-gemini-4-argon, alias: gemini-4-argon, display-name: Argon, force-mapping: trueTHINKING}
        - {name: agy-gemini-4-neon, alias: gemini-4-neon, display-name: Neon, force-mapping: trueTHINKING}
      keys: [{api-key: test-key}]
`, "THINKING", tc.options))
			repo := NewRepository(openImportTestSQLite(t))
			if _, errImport := ImportLocalState(ctx, ImportOptions{ConfigPath: input, Repository: repo}); errImport != nil {
				t.Fatal(errImport)
			}
			// Load models from the persisted credentials without API keys in runtime config.
			runtime, errRuntime := home.NewRuntime(&appconfig.Config{})
			if errRuntime != nil {
				t.Fatal(errRuntime)
			}
			t.Cleanup(runtime.Stop)
			runtime.SetClusterAdapter(NewRuntimeAdapter(repo, ""))
			if errReload := runtime.ReloadAuths(ctx); errReload != nil {
				t.Fatal(errReload)
			}
			for _, auth := range runtime.CoreManager().List() {
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
			}
			for _, alias := range []string{"gemini-4-argon", "gemini-4-neon"} {
				result, errDispatch := runtime.DispatchForAPIKey(ctx, alias, nil, "")
				if errDispatch != nil {
					t.Fatalf("dispatch %s: %v", alias, errDispatch)
				}
				if want := "agy-" + alias; result.Model != want || !result.ForceMapping || result.OriginalAlias != alias {
					t.Errorf("dispatch %s: model=%q force_mapping=%t original_alias=%q, want %q with forced mapping to %q", alias, result.Model, result.ForceMapping, result.OriginalAlias, want, alias)
				}
				info := result.ModelInfo
				if tc.levels != nil && (info == nil || info.UserDefined || info.Thinking == nil || !reflect.DeepEqual(info.Thinking.Levels, tc.levels)) {
					t.Errorf("dispatch %s: model info=%+v, want configured levels %v", alias, info, tc.levels)
				}
			}
		})
	}
}

func TestCPAExecutionExportRejectsMalformedCredentialMetadata(t *testing.T) {
	for _, tc := range []struct {
		name     string
		metadata map[string]any
	}{
		{name: "options-object", metadata: map[string]any{"credential_options": "not-an-object"}},
		{name: "options-models", metadata: map[string]any{"credential_options": map[string]any{"models": "not-an-array"}}},
		{name: "sdk-only-option", metadata: map[string]any{"credential_options": map[string]any{"connection-pool": map[string]any{"enabled": "not-a-bool"}}}},
		{name: "source-object", metadata: map[string]any{appconfig.HomeCredentialConfigMetadataKey: "not-an-object"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			auth := testConfigAPIKeyAuth("malformed-options", "codex", "config:codex[0]", "test-key")
			auth.Index = auth.ID
			auth.Metadata = tc.metadata
			root := map[string]any{"codex-api-key": "unchanged"}
			counts, errApply := ApplyCredentialConfigToRoot(root, []*coreauth.Auth{auth})
			var typeError *json.UnmarshalTypeError
			if !errors.As(errApply, &typeError) {
				t.Fatalf("apply error = %v, want wrapped JSON type error", errApply)
			}
			if root["codex-api-key"] != "unchanged" || counts != (CredentialConfigCounts{}) {
				t.Fatalf("failed apply changed root or counts: root=%+v counts=%+v", root, counts)
			}
			repo := NewRepository(openImportTestSQLite(t))
			if _, errUpsert := repo.UpsertAuth(ctx, auth, "test"); errUpsert != nil {
				t.Fatal(errUpsert)
			}
			output := t.TempDir()
			if _, errExport := ExportLocalState(ctx, ExportOptions{Repository: repo, OutputDir: output, AuthDirName: "auth"}); !errors.As(errExport, &typeError) {
				t.Fatalf("export error = %v, want wrapped JSON type error", errExport)
			}
			if _, errStat := os.Stat(filepath.Join(output, "config.yaml")); !errors.Is(errStat, os.ErrNotExist) {
				t.Fatalf("failed export wrote configuration: %v", errStat)
			}
		})
	}
}

func TestCPAExecutionLegacyAuthAttributesRoundTrip(t *testing.T) {
	for _, provider := range []string{"codex", "xai"} {
		for _, enabled := range []bool{true, false} {
			t.Run(provider+"/"+strconv.FormatBool(enabled), func(t *testing.T) {
				ctx := t.Context()
				repo := NewRepository(openImportTestSQLite(t))
				auth := &coreauth.Auth{
					ID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Provider: provider, Status: coreauth.StatusActive,
					Attributes: map[string]string{
						"source": "config:" + provider + "[0]", "api_key": "legacy-key",
						"base_url": "https://legacy.example.test", "websockets": strconv.FormatBool(enabled),
					},
					Metadata: map[string]any{"credential_options": map[string]any{
						"models":          []any{map[string]any{"name": "test-model", "alias": "test-alias"}},
						"connection-pool": map[string]any{"enabled": false, "max-idle-conns-per-host": 7},
					}},
				}
				if provider == "codex" {
					auth.Attributes[coreauth.AttributeCodexAlphaSearch] = strconv.FormatBool(enabled)
				}
				auth.Index = auth.ID
				if _, errUpsert := repo.UpsertAuth(ctx, auth, "test"); errUpsert != nil {
					t.Fatal(errUpsert)
				}
				output := t.TempDir()
				if _, errExport := ExportLocalState(ctx, ExportOptions{Repository: repo, OutputDir: output, AuthDirName: "auth"}); errExport != nil {
					t.Fatal(errExport)
				}
				path := filepath.Join(output, "config.yaml")
				data, errRead := os.ReadFile(path)
				if errRead != nil {
					t.Fatal(errRead)
				}
				var cfg appconfig.Config
				if errDecode := yaml.Unmarshal(data, &cfg); errDecode != nil {
					t.Fatal(errDecode)
				}
				keys := cfg.CodexKey
				if provider == "xai" {
					keys = cfg.XAIKey
				}
				if len(keys) != 1 || keys[0].Websockets != enabled || keys[0].AlphaSearch != (provider == "codex" && enabled) {
					t.Fatalf("legacy attributes lost on export: %+v", keys)
				}
				pool, ok := keys[0].RawConfig["connection-pool"].(map[string]any)
				if !ok || pool["enabled"] != false || pool["max-idle-conns-per-host"] != 7 || len(keys[0].Models) != 1 {
					t.Fatalf("legacy execution options lost on export: %+v", keys[0])
				}
				second := NewRepository(openImportTestSQLite(t))
				if _, errImport := ImportLocalState(ctx, ImportOptions{ConfigPath: path, Repository: second}); errImport != nil {
					t.Fatal(errImport)
				}
				auths, errAuths := second.ListAuths(ctx)
				if errAuths != nil || len(auths) != 1 {
					t.Fatalf("reimported auths = %d, error = %v", len(auths), errAuths)
				}
				if strings.EqualFold(auths[0].Attributes["websockets"], "true") != enabled ||
					strings.EqualFold(auths[0].Attributes[coreauth.AttributeCodexAlphaSearch], "true") != (provider == "codex" && enabled) {
					t.Fatalf("legacy attributes lost on reimport: %+v", auths[0].Attributes)
				}
			})
		}
	}
}

func TestCPAExecutionImportDatabaseDispatchExport(t *testing.T) {
	ctx := t.Context()
	input := filepath.Join(t.TempDir(), "config.yaml")
	writeFile(t, input, `config-version: 8
client: {codex: {enable-apply-patch: true, optimize-multi-agent-v2: true}}
server: {shutdown-timeout: 20, response-compression: {enabled: false}}
upstream: {claude: {header-defaults: {native-entrypoints: [t3-code], inject-diagnostics: false}}}
oauth: {settings: {codex: [{name: test-model, max-context-length: 524288}]}}
api-keys:
  codex:
    - base-url: https://codex.example.test
      connection-pool: {enabled: true, idle-conn-timeout: 300s, max-idle-conns-per-host: 7}
      models: [{name: codex-model, alias: codex-alias, is-compat: true}]
      keys:
        - api-key: codex-key
          disable-codex-cloaking: false
          connection-pool: {enabled: false}
  gemini:
    - models: [{name: gemini-model, alias: gemini-alias, is-compat: true}]
      keys: [{api-key: gemini-key}]
  vertex:
    - interactions: true
      models: [{name: gemini-2.5-flash, alias: vertex-model, openapi: false}]
      keys: [{api-key: vertex-key}]
  claude:
    - models: [{name: claude-model, alias: claude-alias, is-compat: true}]
      keys:
        - api-key: claude-key
          experimental-cch-signing: true
          rebuild-mid-system-message: true
          fingerprint-profile: claude-code-cli
          cloak:
            mode: always
            strict-mode: true
            cache-user-id: false
            sensitive-patterns-replace: [{pattern: '^old', replacement: new}]
  openai-compatibility:
    - name: gateway
      base-url: https://gateway.example.test
      support-prompt-cache-key: true
      models: [{name: gateway-model, alias: gateway-alias, is-compat: true, use-max-completion-tokens: true}]
      connection-pool: {enabled: true, idle-conn-timeout: 123s, max-idle-conns-per-host: 9}
      keys:
        - api-key: gateway-key
          connection-pool: {enabled: false}
`)
	repo := NewRepository(openImportTestSQLite(t))
	if _, errImport := ImportLocalState(ctx, ImportOptions{ConfigPath: input, Repository: repo}); errImport != nil {
		t.Fatal(errImport)
	}
	auths, errAuths := repo.ListAuths(ctx)
	if errAuths != nil || len(auths) != 5 {
		t.Fatalf("auths = %d, error = %v", len(auths), errAuths)
	}
	for _, auth := range auths {
		sanitized, errSanitize := home.SanitizeAuthForDownstream(auth, nil)
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
		if _, exists := downstream.Metadata[appconfig.HomeCredentialConfigMetadataKey]; exists {
			t.Fatal("private credential configuration leaked to CPA")
		}
		options := downstream.Metadata["credential_options"].(map[string]any)
		model := options["models"].([]any)[0].(map[string]any)
		if auth.Provider != "vertex" && model["is-compat"] != true {
			t.Fatalf("%s dispatch lost model compatibility", auth.Provider)
		}
		switch auth.Provider {
		case "codex", "openai-compatible-gateway":
			pool := downstream.ProxyConnectionPool
			if pool == nil || pool.Enabled == nil || *pool.Enabled || pool.MaxIdleConnsPerHost == nil {
				t.Fatalf("%s dispatch pool = %+v", auth.Provider, pool)
			}
			wantIdle, wantCount := "300s", 7
			if auth.Provider == "openai-compatible-gateway" {
				wantIdle, wantCount = "123s", 9
			}
			if pool.IdleConnTimeout != wantIdle || *pool.MaxIdleConnsPerHost != wantCount {
				t.Fatalf("%s lost inherited pool settings: %+v", auth.Provider, pool)
			}
			if auth.Provider == "codex" {
				if options["disable-codex-cloaking"] != false || downstream.Attributes["codex_disable_cloaking"] != "false" {
					t.Fatal("Codex dispatch lost explicit false cloaking override")
				}
			} else if options["support-prompt-cache-key"] != true || model["use-max-completion-tokens"] != true {
				t.Fatal("compatibility dispatch lost CPA execution options")
			}
		case "vertex":
			if model["openapi"] != false {
				t.Fatal("nested model option was lost")
			}
			if downstream.Attributes["interactions"] != "true" {
				t.Fatal("Vertex dispatch lost Interactions capability")
			}
		case "claude":
			if downstream.Metadata["cloak_sensitive_patterns_replace"] == nil {
				t.Fatal("Claude dispatch lost regex cloak rules")
			}
			for key, want := range map[string]string{
				"experimental_cch_signing": "true", "rebuild_mid_system_message": "true",
				"fingerprint_profile": "claude-code-cli", "cloak_mode": "always",
				"cloak_strict_mode": "true", "cloak_cache_user_id": "false",
			} {
				if downstream.Attributes[key] != want {
					t.Fatalf("Claude dispatch attribute %s = %q, want %q", key, downstream.Attributes[key], want)
				}
			}
		}
	}
	output := t.TempDir()
	if _, errExport := ExportLocalState(ctx, ExportOptions{Repository: repo, OutputDir: output, AuthDirName: "auth"}); errExport != nil {
		t.Fatal(errExport)
	}
	data, errRead := os.ReadFile(filepath.Join(output, "config.yaml"))
	if errRead != nil {
		t.Fatal(errRead)
	}
	cfg := appconfig.Config{CredentialConcurrency: appconfig.DefaultCredentialConcurrencyConfig(), CredentialInFlight: appconfig.DefaultCredentialInFlightConfig()}
	if errDecode := yaml.Unmarshal(data, &cfg); errDecode != nil {
		t.Fatal(errDecode)
	}
	root, errRoot := appconfig.LegacyConfigRoot(&cfg)
	if errRoot != nil {
		t.Fatal(errRoot)
	}
	wire, errWire := yaml.Marshal(root)
	if errWire != nil {
		t.Fatal(errWire)
	}
	cpa, errCPA := cpaconfig.ParseConfigBytes(wire)
	if errCPA != nil {
		t.Fatal(errCPA)
	}
	if !cpa.Client.Codex.EnableApplyPatch || !cpa.Client.Codex.OptimizeMultiAgentV2 || cpa.ShutdownTimeoutSeconds != 20 || cpa.ResponseCompression.Enabled == nil || *cpa.ResponseCompression.Enabled {
		t.Fatalf("export lost execution options:\n%s", data)
	}
	if override := cpa.CodexKey[0].DisableCodexCloaking; override == nil || *override {
		t.Fatal("export lost explicit false Codex cloaking override")
	}
	claude := cpa.ClaudeKey[0]
	if !claude.ExperimentalCCHSigning || !claude.RebuildMidSystemMessage || claude.FingerprintProfile != "claude-code-cli" ||
		claude.Cloak == nil || claude.Cloak.Mode != "always" || !claude.Cloak.StrictMode || claude.Cloak.CacheUserID == nil || *claude.Cloak.CacheUserID {
		t.Fatal("export lost Claude execution options")
	}
	compat := cpa.OpenAICompatibility[0]
	if !cpa.CodexKey[0].Models[0].IsCompat || !cpa.GeminiKey[0].Models[0].IsCompat || !claude.Models[0].IsCompat ||
		!compat.SupportPromptCacheKey || !compat.Models[0].IsCompat || !compat.Models[0].UseMaxCompletionTokens {
		t.Fatal("export lost model or compatibility execution options")
	}
	if len(cfg.OAuthSettings["codex"]) != 1 || cfg.VertexCompatAPIKey[0].RawConfig["interactions"] != true || len(cfg.ClaudeKey[0].RawConfig["cloak"].(map[string]any)["sensitive-patterns-replace"].([]any)) != 1 {
		t.Fatal("export lost credential or OAuth options")
	}
	if pool := cfg.CodexKey[0].RawConfig["connection-pool"].(map[string]any); pool["enabled"] != false || pool["idle-conn-timeout"] != "300s" {
		t.Fatalf("export pool = %+v", pool)
	}
	if !cfg.CodexKey[0].Models[0].IsCompat || !cfg.GeminiKey[0].Models[0].IsCompat || !cfg.ClaudeKey[0].Models[0].IsCompat ||
		!cfg.OpenAICompatibility[0].Models[0].IsCompat || !cfg.OpenAICompatibility[0].Models[0].UseMaxCompletionTokens {
		t.Fatal("export lost typed model compatibility options")
	}
	if openAPI := cfg.VertexCompatAPIKey[0].Models[0].OpenAPI; openAPI == nil || *openAPI {
		t.Fatal("export lost explicit false model endpoint override")
	}
	second := NewRepository(openImportTestSQLite(t))
	if _, errImport := ImportLocalState(ctx, ImportOptions{ConfigPath: filepath.Join(output, "config.yaml"), Repository: second}); errImport != nil {
		t.Fatal(errImport)
	}
	restored, errRestored := second.ListAuths(ctx)
	if errRestored != nil || len(restored) != len(auths) {
		t.Fatalf("reimported auths = %d, error = %v", len(restored), errRestored)
	}
	for _, auth := range auths {
		var match *coreauth.Auth
		for _, candidate := range restored {
			if candidate.ID == auth.ID {
				match = candidate
				break
			}
		}
		if match == nil || !reflect.DeepEqual(auth.Metadata["credential_options"], match.Metadata["credential_options"]) {
			t.Fatalf("%s execution options changed after export and reimport", auth.Provider)
		}
	}
}
