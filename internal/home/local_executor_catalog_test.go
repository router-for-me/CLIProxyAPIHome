package home

import (
	"bytes"
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginstore"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
	"gopkg.in/yaml.v3"
)

func TestLocalExecutorWorkerConfigProjectsDeclaredYAML(t *testing.T) {
	item := homePluginConfigFromYAML(t, `
enabled: true
priority: 10
store: {version: 1.0.0}
load-in-home: true
home: {load: true}
private: {secret: never-send-this}
template: &models ["3", 3]
prefix: "" # never-send-this
unprefixed: false
limit: 0
ratio: !!float 0
optional: null
models: *models
`)
	probe := localExecutorCatalogResponse{
		Ready:  true,
		Fields: []string{"prefix", "unprefixed", "limit", "ratio", "optional", "models", "absent", "store", "home"},
	}
	raw, errConfig := localExecutorWorkerConfig(&item.Raw, probe)
	if errConfig != nil {
		t.Fatal(errConfig)
	}
	if bytes.Contains(raw, []byte("never-send-this")) {
		t.Fatal("projection includes private data")
	}
	var values map[string]any
	if errDecode := yaml.Unmarshal(raw, &values); errDecode != nil {
		t.Fatal(errDecode)
	}
	want := map[string]any{
		"prefix": "", "unprefixed": false, "limit": 0, "ratio": float64(0),
		"optional": nil, "models": []any{"3", 3},
	}
	if !reflect.DeepEqual(values, want) {
		t.Fatalf("projection = %#v, want %#v", values, want)
	}
}

func TestLocalExecutorWorkerConfigWithoutReceipt(t *testing.T) {
	for _, tt := range []struct {
		name, private string
		wantError     bool
	}{
		{"host fields only", "", false},
		{"private empty setting", "prefix: ''\n", true},
		{"private false setting", "unprefixed: false\n", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			item := homePluginConfigFromYAML(t, "enabled: true\npriority: 2\nstore: {version: 1.0.0}\nload-in-home: true\n"+tt.private)
			cfg, errConfig := localExecutorWorkerConfig(&item.Raw, localExecutorCatalogResponse{Fields: []string{"prefix", "unprefixed"}})
			if errConfig != nil {
				if !tt.wantError || !strings.Contains(errConfig.Error(), "incompatible") {
					t.Fatal(errConfig)
				}
			} else if tt.wantError || len(cfg) != 0 {
				t.Fatalf("projection = %q, error = %v", cfg, errConfig)
			}
		})
	}
}

func TestSyncPluginStoreManifestsPreloadsIndependentACPCatalogs(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	cfg := &config.Config{Plugins: config.PluginsConfig{
		Enabled: true,
		Dir:     t.TempDir(),
		Configs: map[string]config.PluginInstanceConfig{
			"broken": homePluginConfigFromYAML(t, `
enabled: true
store: {id: broken, tags: [ACP], version: 1.0.0, source-url: "https://example.invalid/registry.json", install: {type: direct}}
`),
			"healthy": homePluginConfigFromYAML(t, `
enabled: true
store: {id: healthy, tags: [ACP], version: 1.0.0, source-url: "https://example.invalid/registry.json", install: {type: direct}}
`),
		},
	}}
	brokenStarted, healthyStarted := make(chan struct{}), make(chan struct{})
	release := make(chan struct{})
	rt := &Runtime{}
	rt.SetPluginSyncConfigLoader(func(context.Context) (*config.Config, error) { return cfg, nil })
	rt.SetLocalExecutorCatalogLoader(func(ctx context.Context, _ *config.Config, manifest pluginstore.Manifest, _ []byte) ([]registry.ModelInfo, error) {
		if manifest.ID == "healthy" {
			close(healthyStarted)
			return []registry.ModelInfo{{ID: "healthy-model"}}, nil
		}
		close(brokenStarted)
		select {
		case <-release:
			return nil, context.DeadlineExceeded
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	if errSync := rt.syncPluginStoreManifests(ctx, cfg); errSync != nil {
		t.Fatal(errSync)
	}
	for _, started := range []chan struct{}{brokenStarted, healthyStarted} {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("both catalog loads must start while the broken plugin is blocked")
		}
	}
	close(release)
	routes, _, errRoutes := rt.LocalExecutorRoutes(ctx, "")
	if route := routes["acp/healthy-model"]; errRoutes != nil || len(routes) != 1 || route.Plugin != "healthy" {
		t.Fatalf("routes = %#v, error = %v", routes, errRoutes)
	}
}
