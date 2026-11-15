package cluster

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	cpaauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cpaconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	appconfig "github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/home"
	"gopkg.in/yaml.v3"
)

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
      keys:
        - api-key: codex-key
          connection-pool: {enabled: false}
  vertex:
    - interactions: true
      models: [{name: gemini-2.5-flash, alias: vertex-model, openapi: false}]
      keys: [{api-key: vertex-key}]
  claude:
    - keys:
        - api-key: claude-key
          cloak:
            sensitive-patterns-replace: [{pattern: '^old', replacement: new}]
  openai-compatibility:
    - name: gateway
      base-url: https://gateway.example.test
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
	if errAuths != nil || len(auths) != 4 {
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
		switch auth.Provider {
		case "codex", "gateway":
			pool := downstream.ProxyConnectionPool
			if pool == nil || pool.Enabled == nil || *pool.Enabled || pool.MaxIdleConnsPerHost == nil {
				t.Fatalf("%s dispatch pool = %+v", auth.Provider, pool)
			}
			wantIdle, wantCount := "300s", 7
			if auth.Provider == "gateway" {
				wantIdle, wantCount = "123s", 9
			}
			if pool.IdleConnTimeout != wantIdle || *pool.MaxIdleConnsPerHost != wantCount {
				t.Fatalf("%s lost inherited pool settings: %+v", auth.Provider, pool)
			}
		case "vertex":
			options := downstream.Metadata["credential_options"].(map[string]any)
			model := options["models"].([]any)[0].(map[string]any)
			if model["openapi"] != false {
				t.Fatal("nested SDK-only model option was lost")
			}
			if downstream.Attributes["interactions"] != "true" {
				t.Fatal("Vertex dispatch lost Interactions capability")
			}
		case "claude":
			if downstream.Metadata["cloak_sensitive_patterns_replace"] == nil {
				t.Fatal("Claude dispatch lost regex cloak rules")
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
	if !cpa.Client.Codex.EnableApplyPatch || !cfg.Codex.OptimizeMultiAgentV2 || cpa.ShutdownTimeoutSeconds != 20 || cpa.ResponseCompression.Enabled == nil || *cpa.ResponseCompression.Enabled {
		t.Fatalf("export lost execution options:\n%s", data)
	}
	if len(cfg.OAuthSettings["codex"]) != 1 || cfg.VertexCompatAPIKey[0].RawConfig["interactions"] != true || len(cfg.ClaudeKey[0].RawConfig["cloak"].(map[string]any)["sensitive-patterns-replace"].([]any)) != 1 {
		t.Fatal("export lost credential or OAuth options")
	}
	if pool := cfg.CodexKey[0].RawConfig["connection-pool"].(map[string]any); pool["enabled"] != false || pool["idle-conn-timeout"] != "300s" {
		t.Fatalf("export pool = %+v", pool)
	}
	if cfg.VertexCompatAPIKey[0].RawConfig["models"].([]any)[0].(map[string]any)["openapi"] != false {
		t.Fatal("nested SDK-only model option was lost on export")
	}
	second := NewRepository(openImportTestSQLite(t))
	if _, errImport := ImportLocalState(ctx, ImportOptions{ConfigPath: filepath.Join(output, "config.yaml"), Repository: second}); errImport != nil {
		t.Fatal(errImport)
	}
}
