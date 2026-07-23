package cluster

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestRuntimeConfigFromRootAppliesConcurrencyDefaults(t *testing.T) {
	cfg, _, errConfig := RuntimeConfigFromRoot(map[string]any{})
	if errConfig != nil {
		t.Fatalf("RuntimeConfigFromRoot() error = %v", errConfig)
	}
	if cfg.CredentialConcurrency.ReleaseFlushInterval != "250ms" {
		t.Fatalf("ReleaseFlushInterval = %q", cfg.CredentialConcurrency.ReleaseFlushInterval)
	}
	if cfg.CredentialConcurrency.ReleaseMaxBackoff != "2s" {
		t.Fatalf("ReleaseMaxBackoff = %q", cfg.CredentialConcurrency.ReleaseMaxBackoff)
	}
	if cfg.CredentialConcurrency.MaxLimit != 1_000_000 {
		t.Fatalf("MaxLimit = %d", cfg.CredentialConcurrency.MaxLimit)
	}
}

func TestRuntimeConfigFromRootPreservesConcurrencyLimiterConfig(t *testing.T) {
	root := map[string]any{
		"credential-concurrency": map[string]any{
			"release-flush-interval": "500ms",
			"release-max-backoff":    "3s",
			"busy-retry-min":         "300ms",
			"busy-retry-max":         "2s",
			"max-limit":              99,
		},
	}

	cfg, payload, errConfig := RuntimeConfigFromRoot(root)
	if errConfig != nil {
		t.Fatalf("RuntimeConfigFromRoot() error = %v", errConfig)
	}
	if cfg.CredentialConcurrency.ReleaseFlushInterval != "500ms" || cfg.CredentialConcurrency.ReleaseMaxBackoff != "3s" || cfg.CredentialConcurrency.BusyRetryMin != "300ms" || cfg.CredentialConcurrency.BusyRetryMax != "2s" || cfg.CredentialConcurrency.MaxLimit != 99 {
		t.Fatalf("CredentialConcurrency = %#v", cfg.CredentialConcurrency)
	}
	if !strings.Contains(string(payload), "max-limit: 99") {
		t.Fatalf("runtime payload omitted max-limit:\n%s", payload)
	}
}

func TestRuntimeConfigFromRootAppliesHomeModeScalarsAndPreservesRemoteManagement(t *testing.T) {
	root := map[string]any{
		"api-keys":                 []any{"local-key"},
		"usage-statistics-enabled": false,
		"disable-cooling":          false,
		"ws-auth":                  true,
		"remote-management": map[string]any{
			"allow-remote":          true,
			"disable-control-panel": false,
		},
	}

	cfg, _, errConfig := RuntimeConfigFromRoot(root)
	if errConfig != nil {
		t.Fatalf("RuntimeConfigFromRoot() error = %v", errConfig)
	}
	if len(cfg.APIKeys) != 0 {
		t.Fatalf("APIKeys = %#v, want nil/empty", cfg.APIKeys)
	}
	if !cfg.UsageStatisticsEnabled {
		t.Fatal("UsageStatisticsEnabled = false, want true")
	}
	if !cfg.DisableCooling {
		t.Fatal("DisableCooling = false, want true")
	}
	if cfg.WebsocketAuth {
		t.Fatal("WebsocketAuth = true, want false")
	}
	if !cfg.RemoteManagement.AllowRemote {
		t.Fatal("RemoteManagement.AllowRemote = false, want preserved true")
	}
	if cfg.RemoteManagement.DisableControlPanel {
		t.Fatal("RemoteManagement.DisableControlPanel = true, want preserved false")
	}
}

func TestLoadConfigAsRuntimeConfigProjectsPluginAuthRevisionWithoutStoreAuth(t *testing.T) {
	db, errOpen := OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "home.db"))
	if errOpen != nil {
		t.Fatalf("OpenSQLite() error = %v", errOpen)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	if errMigrate := AutoMigrate(db); errMigrate != nil {
		t.Fatalf("AutoMigrate() error = %v", errMigrate)
	}
	repo := NewRepository(db)
	root := map[string]any{
		"plugins": map[string]any{
			"enabled":       true,
			"sync-revision": int64(999),
			"store-auth": []any{map[string]any{
				"match": "https://downloads.example/", "token-env": "SHOULD_NOT_LEAK",
			}},
			"configs": map[string]any{},
		},
	}
	if errReplace := repo.ReplaceConfigSnapshot(context.Background(), root); errReplace != nil {
		t.Fatalf("ReplaceConfigSnapshot() error = %v", errReplace)
	}
	if errEvent := repo.AppendEvent(context.Background(), pluginStoreAuthEventScope, "update", "1", 1); errEvent != nil {
		t.Fatalf("AppendEvent() error = %v", errEvent)
	}
	_, payload, errLoad := repo.LoadConfigAsRuntimeConfig(context.Background())
	if errLoad != nil {
		t.Fatalf("LoadConfigAsRuntimeConfig() error = %v", errLoad)
	}
	var projected struct {
		Plugins struct {
			AuthRevision int64 `yaml:"auth-revision"`
		} `yaml:"plugins"`
	}
	if errUnmarshal := yaml.Unmarshal(payload, &projected); errUnmarshal != nil {
		t.Fatalf("unmarshal runtime payload: %v", errUnmarshal)
	}
	if projected.Plugins.AuthRevision == 0 {
		t.Fatal("runtime payload plugins.auth-revision = 0, want event revision")
	}
	if strings.Contains(string(payload), "sync-revision") {
		t.Fatalf("runtime config retained legacy plugins.sync-revision: %s", payload)
	}
	if strings.Contains(string(payload), "store-auth") || strings.Contains(string(payload), "SHOULD_NOT_LEAK") {
		t.Fatalf("runtime config leaked plugin store auth: %s", payload)
	}
}

func TestRuntimeConfigFromRootPreservesPluginConfig(t *testing.T) {
	root := map[string]any{
		"plugins": map[string]any{
			"enabled": true,
			"dir":     "plugins",
			"configs": map[string]any{
				"sample": map[string]any{
					"enabled":  true,
					"priority": 7,
					"mode":     "fast",
					"nested": map[string]any{
						"value": "keep",
					},
				},
			},
		},
	}

	cfg, payload, errConfig := RuntimeConfigFromRoot(root)
	if errConfig != nil {
		t.Fatalf("RuntimeConfigFromRoot() error = %v", errConfig)
	}
	if !cfg.Plugins.Enabled {
		t.Fatal("Plugins.Enabled = false, want true")
	}
	plugin := cfg.Plugins.Configs["sample"]
	if plugin.Enabled == nil || !*plugin.Enabled {
		t.Fatalf("plugin enabled = %#v, want true", plugin.Enabled)
	}
	if plugin.Priority != 7 {
		t.Fatalf("plugin priority = %d, want 7", plugin.Priority)
	}
	raw, errMarshal := yaml.Marshal(&plugin.Raw)
	if errMarshal != nil {
		t.Fatalf("marshal plugin raw: %v", errMarshal)
	}
	if !strings.Contains(string(raw), "mode: fast") || !strings.Contains(string(raw), "value: keep") {
		t.Fatalf("plugin raw config lost custom fields:\n%s", string(raw))
	}
	if !strings.Contains(string(payload), "plugins:") || !strings.Contains(string(payload), "mode: fast") {
		t.Fatalf("runtime payload lost plugin config:\n%s", string(payload))
	}
}

func TestRuntimeConfigFromRootPreservesAdvancedPayloadModelMatchers(t *testing.T) {
	root := map[string]any{
		"payload": map[string]any{
			"default": []any{
				map[string]any{
					"models": []any{
						map[string]any{
							"name":          "gemini-*",
							"protocol":      "gemini",
							"from-protocol": "responses",
							"headers": map[string]any{
								"X-Client-Tier": "tenant-*",
							},
							"match": []any{
								map[string]any{"metadata.client": "codex"},
							},
							"not-match": []any{
								map[string]any{"metadata.mode": "dev"},
							},
							"exist":     []any{"tools.#(type==\"web_search\").type"},
							"not-exist": []any{"metadata.disable_payload"},
						},
					},
					"params": map[string]any{
						"generationConfig.thinkingConfig.thinkingBudget": 32768,
					},
				},
			},
		},
	}

	cfg, payload, errConfig := RuntimeConfigFromRoot(root)
	if errConfig != nil {
		t.Fatalf("RuntimeConfigFromRoot() error = %v", errConfig)
	}
	if len(cfg.Payload.Default) != 1 || len(cfg.Payload.Default[0].Models) != 1 {
		t.Fatalf("payload default models = %#v, want one advanced matcher", cfg.Payload.Default)
	}
	model := cfg.Payload.Default[0].Models[0]
	if model.FromProtocol != "responses" {
		t.Fatalf("FromProtocol = %q, want responses", model.FromProtocol)
	}
	if model.Headers["X-Client-Tier"] != "tenant-*" {
		t.Fatalf("Headers = %#v, want X-Client-Tier matcher", model.Headers)
	}
	if len(model.Match) != 1 || model.Match[0]["metadata.client"] != "codex" {
		t.Fatalf("Match = %#v, want metadata.client matcher", model.Match)
	}
	if len(model.NotMatch) != 1 || model.NotMatch[0]["metadata.mode"] != "dev" {
		t.Fatalf("NotMatch = %#v, want metadata.mode matcher", model.NotMatch)
	}
	if len(model.Exist) != 1 || model.Exist[0] != "tools.#(type==\"web_search\").type" {
		t.Fatalf("Exist = %#v, want web_search path", model.Exist)
	}
	if len(model.NotExist) != 1 || model.NotExist[0] != "metadata.disable_payload" {
		t.Fatalf("NotExist = %#v, want disable payload path", model.NotExist)
	}
	for _, want := range []string{"from-protocol: responses", "X-Client-Tier: tenant-*", "not-exist:"} {
		if !strings.Contains(string(payload), want) {
			t.Fatalf("runtime payload missing %q:\n%s", want, string(payload))
		}
	}
}
