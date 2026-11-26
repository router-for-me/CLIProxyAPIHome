package config

import (
	"reflect"
	"testing"

	cpaconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"gopkg.in/yaml.v3"
)

func TestSharedUpstreamConfigProjectsForBundledCPA(t *testing.T) {
	source := []byte(`client: {codex: {optimize-multi-agent-v2: true}}
upstream:
  codex: {disable-codex-cloaking: true, stream-bootstrap-buffering: true, stream-bootstrap-timeout: 10s, orphan-delegation-compatibility: true, model-level-cooling: true, response-steering: true}
  claude:
    model-level-cooling: true
    disable-cloaking-model-list: true
    disable-claude-cloak-mode: true
    header-defaults: {user-agent: upstream-agent, package-version: 1.2.3, runtime-version: v22.1.0, os: Linux, arch: arm64, timeout: '123', timezone: Asia/Shanghai, stabilize-device-profile: true}
  xai: {inject-x-search: true}
oauth: {auth-dir: upstream-auth, auth-auto-refresh-workers: 3, providers: {codex: {header-defaults: {user-agent: oauth-agent, beta-features: oauth-beta}}}}
`)
	if errValidate := ValidateV8Config(source); errValidate != nil {
		t.Fatal(errValidate)
	}
	var cfg Config
	if errDecode := yaml.Unmarshal(source, &cfg); errDecode != nil {
		t.Fatal(errDecode)
	}
	if cfg.OAuthOnlyFields["codex.disable-codex-cloaking"] || !cfg.OAuthOnlyFields["codex-header-defaults.user-agent"] {
		t.Fatal("client or shared provider fields acquired OAuth scope")
	}
	var root map[string]any
	if errDecode := yaml.Unmarshal(source, &root); errDecode != nil {
		t.Fatal(errDecode)
	}
	projected, errNormalize := NormalizeConfigRoot(root)
	if errNormalize != nil {
		t.Fatal(errNormalize)
	}
	wire, errMarshal := yaml.Marshal(projected)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	cpa, errParse := cpaconfig.ParseConfigBytes(wire)
	if errParse != nil {
		t.Fatal(errParse)
	}
	expected := []any{"upstream-auth", 3, true, true, "10s", true, true, true, true, true, true, "upstream-agent", "1.2.3", "v22.1.0", "Linux", "arm64", "123", "Asia/Shanghai", true, true}
	for _, view := range []*cpaconfig.Config{cpa, cpa.ForAPIKey()} {
		actual := []any{
			view.AuthDir, view.AuthAutoRefreshWorkers,
			view.Codex.DisableCodexCloaking, view.Codex.StreamBootstrapBuffering, view.Codex.StreamBootstrapTimeout,
			view.Codex.OrphanDelegationCompatibility, view.Codex.ModelLevelCooling, view.Codex.ResponseSteering,
			view.Claude.ModelLevelCooling, view.ClaudeCode.DisableCloakingModelList, view.DisableClaudeCloakMode,
			view.ClaudeHeaderDefaults.UserAgent, view.ClaudeHeaderDefaults.PackageVersion, view.ClaudeHeaderDefaults.RuntimeVersion,
			view.ClaudeHeaderDefaults.OS, view.ClaudeHeaderDefaults.Arch, view.ClaudeHeaderDefaults.Timeout, view.ClaudeHeaderDefaults.Timezone,
			view.ClaudeHeaderDefaults.StabilizeDeviceProfile != nil && *view.ClaudeHeaderDefaults.StabilizeDeviceProfile,
			view.XAI.InjectXSearch,
		}
		if !reflect.DeepEqual(actual, expected) {
			t.Fatalf("compatible projection changed shared settings: got %#v, want %#v", actual, expected)
		}
	}
	if cpa.CodexHeaderDefaults.UserAgent != "oauth-agent" || cpa.ForAPIKey().CodexHeaderDefaults.UserAgent != "" {
		t.Fatal("compatible projection lost OAuth-only header scope")
	}
	remigrated, _, errMigrate := NormalizeConfigLayout(wire, true)
	if errMigrate != nil {
		t.Fatal(errMigrate)
	}
	if errValidate := ValidateV8Config(remigrated); errValidate != nil {
		t.Fatal(errValidate)
	}
	var restored Config
	if errDecode := yaml.Unmarshal(remigrated, &restored); errDecode != nil {
		t.Fatal(errDecode)
	}
	restoredWire, errRestoredMarshal := yaml.Marshal(restored)
	if errRestoredMarshal != nil {
		t.Fatal(errRestoredMarshal)
	}
	restoredCPA, errRestoredParse := cpaconfig.ParseConfigBytes(restoredWire)
	if errRestoredParse != nil {
		t.Fatal(errRestoredParse)
	}
	if !reflect.DeepEqual(restoredCPA.Codex, cpa.Codex) || !reflect.DeepEqual(restoredCPA.ClaudeHeaderDefaults, cpa.ClaudeHeaderDefaults) || !reflect.DeepEqual(restored.OAuthOnlyFields, cfg.OAuthOnlyFields) || restored.AuthDir != cfg.AuthDir {
		t.Fatal("database projection round trip changed runtime provider settings")
	}
}

func TestSharedUpstreamHistoricalPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name, source string
		want         bool
	}{
		{"historical OAuth wins flat", "codex: {response-steering: false}\noauth: {providers: {codex: {response-steering: true}}}\n", true},
		{"canonical false wins", "codex: {response-steering: true}\noauth: {providers: {codex: {response-steering: true}}}\nupstream: {codex: {response-steering: false}}\n", false},
		{"canonical null wins", "oauth: {providers: {codex: {response-steering: true}}}\nupstream: {codex: {response-steering: null}}\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, migrate := range []bool{false, true} {
				data, _, errNormalize := NormalizeConfigLayout([]byte(tc.source), migrate)
				if errNormalize != nil {
					t.Fatal(errNormalize)
				}
				var cfg Config
				if errDecode := yaml.Unmarshal(data, &cfg); errDecode != nil {
					t.Fatal(errDecode)
				}
				cpaView, errParse := cpaconfig.ParseConfigBytes(data)
				if errParse != nil {
					t.Fatal(errParse)
				}
				if cpaView.Codex.ResponseSteering != tc.want || cfg.OAuthOnlyFields["codex.response-steering"] {
					t.Fatal("shared upstream precedence or scope changed")
				}
				if migrate {
					if errValidate := ValidateV8Config(data); errValidate != nil {
						t.Fatal(errValidate)
					}
				}
			}
		})
	}
}
