package home

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginstore"
	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
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

func TestLocalExecutorConfigPreloadsIndependentCatalogs(t *testing.T) {
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
	defer close(release)
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
	rt.reconcilePluginArtifacts(ctx, nil, cfg)
	for _, started := range []chan struct{}{brokenStarted, healthyStarted} {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("both catalog loads must start while the broken plugin is blocked")
		}
	}
	for _, requested := range []string{"", "acp/healthy-model"} {
		routes, _, errRoutes := rt.LocalExecutorRoutes(ctx, requested)
		if route := routes["acp/healthy-model"]; ctx.Err() != nil || errRoutes != nil || len(routes) != 1 || route.Plugin != "healthy" {
			t.Fatalf("requested %q while broken plugin was blocked: routes = %#v, error = %v, context = %v", requested, routes, errRoutes, ctx.Err())
		}
	}
}

func TestLocalExecutorCatalogSurvivesCanceledReader(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := &config.Config{Plugins: config.PluginsConfig{Enabled: true, Configs: map[string]config.PluginInstanceConfig{
			"sample": homePluginConfigFromYAML(t, `enabled: true
store: {id: sample, tags: [ACP], version: 1.0.0, source-url: "https://example.invalid/registry.json", install: {type: direct}}`),
		}}}
		rt := &Runtime{}
		rt.SetPluginSyncConfigLoader(func(context.Context) (*config.Config, error) { return cfg, nil })
		started, release := make(chan struct{}, 2), make(chan struct{})
		rt.SetLocalExecutorCatalogLoader(func(ctx context.Context, _ *config.Config, _ pluginstore.Manifest, _ []byte) ([]registry.ModelInfo, error) {
			started <- struct{}{}
			select {
			case <-release:
				return []registry.ModelInfo{{ID: "shared"}}, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		})
		first, cancel := context.WithCancel(t.Context())
		result := make(chan error, 1)
		go func() {
			_, _, errRoutes := rt.LocalExecutorRoutes(first, "")
			result <- errRoutes
		}()
		<-started
		cancel()
		if errRoutes := <-result; !errors.Is(errRoutes, context.Canceled) {
			t.Errorf("first reader error = %v", errRoutes)
		}
		start := time.Now()
		for range 2 {
			pending, _, errPending := rt.LocalExecutorRoutes(t.Context(), "")
			if len(pending) != 0 || errPending != nil || time.Since(start) > localExecutorCatalogFirstRead {
				t.Fatalf("pending catalog held readers for %v: routes = %v, error = %v", time.Since(start), pending, errPending)
			}
		}
		close(release)
		synctest.Wait()
		routes, _, errRoutes := rt.LocalExecutorRoutes(t.Context(), "acp/shared")
		if errRoutes != nil || routes["acp/shared"].Plugin != "sample" || len(started) != 0 {
			t.Fatalf("shared load was discarded or restarted: routes = %#v, error = %v", routes, errRoutes)
		}
	})
}

func TestLocalExecutorCatalogCachedInput(t *testing.T) {
	const settings = `enabled: true
store: {id: sample, tags: [ACP], version: 1.0.0, source-url: https://store.example/registry.json, install: {type: direct}}
ratio: !!float 0
`
	item := homePluginConfigFromYAML(t, settings)
	cfg := &config.Config{Plugins: config.PluginsConfig{Enabled: true, Configs: map[string]config.PluginInstanceConfig{"sample": item}}}
	rt := &Runtime{cfg: cfg}
	rt.SetLocalExecutorCatalogLoader(func(context.Context, *config.Config, pluginstore.Manifest, []byte) ([]registry.ModelInfo, error) {
		return []registry.ModelInfo{{ID: "model"}}, nil
	})
	read := func() *localExecutorCatalog {
		t.Helper()
		_, catalogs, errRead := rt.observeLocalExecutorCatalogs(t.Context())
		if errRead != nil || len(catalogs) != 1 {
			t.Fatalf("read catalogs: count=%d error=%v", len(catalogs), errRead)
		}
		<-catalogs[0].ready
		return catalogs[0]
	}
	first := read()
	item = homePluginConfigFromYAML(t, "# different source location\n"+settings)
	cfg.Plugins.Configs["sample"] = item
	if read() != first {
		t.Fatal("equivalent configuration restarted the catalog")
	}
	// The CPA digest encodes both as 0. The catalog worker also receives the YAML tag.
	yamlMappingValue(&item.Raw, "ratio").Tag = "!!int"
	changed := read()
	if changed == first || changed.configDigest != first.configDigest {
		t.Fatal("a changed scalar tag must reload the catalog even when the CPA digest is identical")
	}
	const manifest = "{id: sample, tags: [ACP], version: 1.0.0, source-url: https://store.example/registry.json, install: {type: direct}}"
	cfg.Plugins.Configs["sample"] = homePluginConfigFromYAML(t, "enabled: true\nother: &manifest "+manifest+"\nstore: "+manifest)
	read()
	cfg.Plugins.Configs["sample"] = homePluginConfigFromYAML(t, "enabled: true\nother: &manifest "+manifest+"\nstore: *manifest")
	if routes, _, errRoutes := rt.LocalExecutorRoutes(t.Context(), ""); errRoutes != nil || len(routes) != 0 {
		t.Fatal("a warm cache accepted a store declaration that cold discovery would skip")
	}
}

func TestLocalExecutorCatalogIgnoresSupersededRead(t *testing.T) {
	for _, version := range []string{"2.0.0", "uninstalled"} {
		t.Run(version, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				configuration := func(v string) *config.Config {
					cfg := &config.Config{Plugins: config.PluginsConfig{Enabled: true, Configs: map[string]config.PluginInstanceConfig{}}}
					if v != "uninstalled" {
						cfg.Plugins.Configs["sample"] = homePluginConfigFromYAML(t, fmt.Sprintf(`enabled: true
store: {id: sample, version: %s, tags: [ACP], source-url: https://store.example/registry.json, install: {type: direct}}`, v))
					}
					return cfg
				}
				var latest atomic.Pointer[config.Config]
				latest.Store(configuration("1.0.0"))
				oldRead, resume := make(chan struct{}), make(chan struct{})
				slow, cancel := context.WithCancel(t.Context())
				defer cancel()
				var pauseOnce atomic.Bool
				var loads atomic.Int32
				rt := &Runtime{}
				rt.SetPluginSyncConfigLoader(func(ctx context.Context) (*config.Config, error) {
					cfg := latest.Load()
					if ctx == slow && pauseOnce.CompareAndSwap(false, true) {
						close(oldRead)
						<-resume
					}
					return cfg, nil
				})
				rt.SetLocalExecutorCatalogLoader(func(_ context.Context, _ *config.Config, m pluginstore.Manifest, _ []byte) ([]registry.ModelInfo, error) {
					loads.Add(1)
					return []registry.ModelInfo{{ID: m.Version}}, nil
				})
				if routes, _, err := rt.LocalExecutorRoutes(t.Context(), ""); err != nil || len(routes) != 1 {
					t.Fatalf("initial routes = %v, error %v", routes, err)
				}
				late := make(chan map[string]LocalExecutorRoute, 1)
				go func() {
					routes, _, err := rt.LocalExecutorRoutes(slow, "")
					if err != nil {
						t.Error(err)
					}
					late <- routes
				}()
				<-oldRead
				latest.Store(configuration(version))
				current, _, err := rt.LocalExecutorRoutes(t.Context(), "")
				if err != nil || (version == "uninstalled" && len(current) != 0) ||
					(version != "uninstalled" && current["acp/"+version].Version != version) {
					t.Fatalf("updated routes = %v, error %v", current, err)
				}
				before := loads.Load()
				close(resume)
				if got := <-late; !reflect.DeepEqual(got, current) {
					t.Fatalf("late reader returned %v, want current routes %v", got, current)
				}
				final, _, err := rt.LocalExecutorRoutes(t.Context(), "")
				if err != nil || !reflect.DeepEqual(final, current) || loads.Load() != before {
					t.Fatalf("late reader changed publication: routes=%v loads=%d, error=%v", final, loads.Load(), err)
				}
			})
		})
	}
}

func TestLocalExecutorCatalogLateReadAfterStop(t *testing.T) {
	cfg := &config.Config{Plugins: config.PluginsConfig{Enabled: true, Configs: map[string]config.PluginInstanceConfig{
		"sample": homePluginConfigFromYAML(t, `enabled: true
store: {id: sample, version: 1.0.0, tags: [ACP], source-url: https://store.example/registry.json, install: {type: direct}}`),
	}}}
	rt := &Runtime{}
	started, resume := make(chan struct{}), make(chan struct{})
	rt.SetPluginSyncConfigLoader(func(context.Context) (*config.Config, error) {
		close(started)
		<-resume
		return cfg, nil
	})
	rt.SetLocalExecutorCatalogLoader(func(context.Context, *config.Config, pluginstore.Manifest, []byte) ([]registry.ModelInfo, error) {
		t.Error("stopped runtime started a catalog worker")
		return nil, nil
	})
	done := make(chan error, 1)
	go func() {
		_, _, errRoutes := rt.LocalExecutorRoutes(t.Context(), "")
		done <- errRoutes
	}()
	<-started
	rt.Stop()
	close(resume)
	if errRoutes := <-done; !errors.Is(errRoutes, context.Canceled) || len(rt.localExecutorCatalogs) != 0 {
		t.Fatalf("catalogs after stop = %d, error = %v", len(rt.localExecutorCatalogs), errRoutes)
	}
}

func BenchmarkLocalExecutorRoutesWithoutACP(b *testing.B) {
	for _, count := range []int{0, 1000} {
		b.Run(fmt.Sprintf("credentials_%d", count), func(b *testing.B) {
			m := coreauth.NewManager(nil, nil, nil)
			cfg := &config.Config{Plugins: config.PluginsConfig{Enabled: true, Configs: map[string]config.PluginInstanceConfig{"ordinary": {}}}}
			rt := &Runtime{cfg: cfg, coreManager: m}
			ctx := b.Context()
			for i := range count {
				if _, errRegister := m.Register(ctx, &coreauth.Auth{ID: fmt.Sprintf("account-%d", i), Provider: "gemini"}); errRegister != nil {
					b.Fatal(errRegister)
				}
			}
			b.ReportAllocs()
			for b.Loop() {
				if routes, _, errRoutes := rt.LocalExecutorRoutes(ctx, ""); errRoutes != nil || len(routes) != 0 {
					b.Fatal("unexpected ACP routes", errRoutes)
				}
			}
		})
	}
}

func BenchmarkLocalExecutorRoutesWithACP(b *testing.B) {
	const native = "agy/gemini-3.8-flash"
	const public = "acp/" + native
	var item config.PluginInstanceConfig
	if errParse := yaml.Unmarshal([]byte(`enabled: true
model_prefix: agy/
route_unprefixed_models: true
store:
  id: cpa-acp
  version: 0.6.39
  tags: [Provider, Executor, Management, ACP]
  source-url: https://store.example/registry.json
  install:
    type: direct
    artifacts:
      - {goos: windows, goarch: amd64, url: https://store.example/plugin.zip, sha256: fbb39821c2f7a561f453935c044dd71ce32cf53c87da9fdeaae308e48e81c69e}
`), &item); errParse != nil {
		b.Fatal(errParse)
	}
	rt := &Runtime{cfg: &config.Config{Plugins: config.PluginsConfig{Enabled: true, Configs: map[string]config.PluginInstanceConfig{"cpa-acp": item}}}}
	rt.SetLocalExecutorCatalogLoader(func(context.Context, *config.Config, pluginstore.Manifest, []byte) ([]registry.ModelInfo, error) {
		return []registry.ModelInfo{{ID: native}}, nil
	})
	ctx := b.Context()
	if routes, _, errRoutes := rt.LocalExecutorRoutes(ctx, public); errRoutes != nil || routes[public].Plugin != "cpa-acp" {
		b.Fatalf("warm catalog: %v", errRoutes)
	}
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, _, errRoutes := rt.LocalExecutorRoutes(ctx, public); errRoutes != nil {
				b.Error(errRoutes)
				return
			}
		}
	})
}
