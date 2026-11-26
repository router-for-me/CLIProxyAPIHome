package home

import (
	"context"
	"errors"
	"testing"

	cpaauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	sdkpluginhost "github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginhost"
	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
)

type discoveryTestAdapter struct {
	ClusterAdapter
	full       *coreauth.Auth
	errFull    error
	fullCalls  int
	saved      *coreauth.Auth
	saveResult int64
	projection *coreauth.Auth
}

func (a *discoveryTestAdapter) Enabled() bool { return true }

func (a *discoveryTestAdapter) GetFullAuth(context.Context, string) (*coreauth.Auth, error) {
	a.fullCalls++
	if a.full == nil {
		return nil, a.errFull
	}
	return a.full.Clone(), a.errFull
}

func (a *discoveryTestAdapter) SaveWithStateVersion(_ context.Context, auth *coreauth.Auth) (string, int64, error) {
	a.saved = auth.Clone()
	return auth.ID, a.saveResult, nil
}

func (a *discoveryTestAdapter) ListMinimalAuths() []*coreauth.Auth {
	if a.projection == nil {
		return nil
	}
	return []*coreauth.Auth{a.projection.Clone()}
}

type discoveryTestPlugin struct {
	registered []sdkpluginhost.RegisteredPluginInfo
	result     sdkpluginhost.AuthModelResult
	seen       *cpaauth.Auth
}

func (p *discoveryTestPlugin) ModelsForAuth(_ context.Context, auth *cpaauth.Auth) sdkpluginhost.AuthModelResult {
	p.seen = auth
	if auth.Metadata["token"] != "synthetic-token" {
		return sdkpluginhost.AuthModelResult{Handled: true, Err: errors.New("missing token")}
	}
	return p.result
}

func (p *discoveryTestPlugin) ModelsForProvider(string) []sdkpluginhost.ModelInfo { return nil }

func (p *discoveryTestPlugin) RegisteredPlugins() []sdkpluginhost.RegisteredPluginInfo {
	return p.registered
}

func newDiscoveryTestRuntime(t *testing.T, adapter *discoveryTestAdapter) (*Runtime, *coreauth.Auth) {
	t.Helper()
	manager := coreauth.NewManager(nil, &coreauth.FillFirstSelector{}, nil)
	rt := &Runtime{cfg: &config.Config{}, coreManager: manager, clusterAdapter: adapter}
	projection := &coreauth.Auth{ID: t.Name(), Index: t.Name(), Provider: "fixture", StateVersion: 3,
		Status: coreauth.StatusActive, RuntimeProjection: true, Attributes: map[string]string{"auth_kind": "oauth"}}
	if _, errRegister := manager.Register(coreauth.WithSkipPersist(t.Context()), projection); errRegister != nil {
		t.Fatal(errRegister)
	}
	adapter.full = &coreauth.Auth{ID: projection.ID, Provider: "fixture", StateVersion: 3, Status: coreauth.StatusActive,
		Attributes: map[string]string{"auth_kind": "oauth"}, Metadata: map[string]any{"token": "synthetic-token"}}
	reg := registry.GetGlobalRegistry()
	t.Cleanup(func() { reg.UnregisterClient(projection.ID) })
	return rt, projection
}

func TestPluginDiscoveryUsesFullCredentialForProjection(t *testing.T) {
	adapter := &discoveryTestAdapter{}
	rt, projection := newDiscoveryTestRuntime(t, adapter)
	plugin := &discoveryTestPlugin{
		registered: []sdkpluginhost.RegisteredPluginInfo{{}},
		result:     sdkpluginhost.AuthModelResult{Handled: true, Models: []sdkpluginhost.ModelInfo{{ID: "fixture-model"}}},
	}
	rt.publishModelsForAuth(projection, plugin)
	if plugin.seen == nil || plugin.seen.Metadata["token"] != "synthetic-token" || plugin.seen.ID != projection.ID {
		t.Fatal("plugin discovery did not receive the full credential")
	}
	if !registry.GetGlobalRegistry().ClientSupportsModel(projection.ID, "fixture-model") {
		t.Fatal("discovered model was not registered")
	}
	if current, _ := rt.coreManager.GetByID(projection.ID); current == nil || !current.RuntimeProjection || current.Metadata["token"] != nil {
		t.Fatal("scheduler record stopped being a secret-free projection")
	}
}

func TestPluginDiscoveryKeepsProjectionWhenFullAuthIsUnavailable(t *testing.T) {
	for _, tc := range []string{"no-plugins", "resolver-error", "disabled", "provider-changed"} {
		t.Run(tc, func(t *testing.T) {
			adapter := &discoveryTestAdapter{}
			rt, projection := newDiscoveryTestRuntime(t, adapter)
			plugin := &discoveryTestPlugin{registered: []sdkpluginhost.RegisteredPluginInfo{{}}}
			switch tc {
			case "no-plugins":
				plugin.registered = nil
			case "resolver-error":
				adapter.errFull = errors.New("resolver failed")
			case "disabled":
				adapter.full.Disabled = true
			case "provider-changed":
				adapter.full.Provider = "other"
			}
			if got := rt.pluginDiscoveryAuth(t.Context(), plugin, projection); got != projection {
				t.Fatal("unusable full credential replaced the projection")
			}
			if tc == "no-plugins" && adapter.fullCalls != 0 {
				t.Fatal("full credential was loaded without registered plugins")
			}
		})
	}
}

func TestPluginDiscoveryAuthUpdateKeepsSchedulerProjection(t *testing.T) {
	for _, tc := range []struct {
		name     string
		accepted int64
	}{{"accepted", 4}, {"stale", 0}} {
		t.Run(tc.name, func(t *testing.T) {
			adapter := &discoveryTestAdapter{saveResult: tc.accepted}
			rt, projection := newDiscoveryTestRuntime(t, adapter)
			adapter.projection = projection.Clone()
			adapter.projection.StateVersion = 4
			adapter.projection.Prefix = "team"
			update := &cpaauth.Auth{ID: projection.ID, Prefix: "team", Status: cpaauth.StatusActive,
				Metadata: map[string]any{"token": "synthetic-token", "account_id": "updated"}}
			plugin := &discoveryTestPlugin{
				registered: []sdkpluginhost.RegisteredPluginInfo{{}},
				result:     sdkpluginhost.AuthModelResult{Handled: true, Auth: update, Models: []sdkpluginhost.ModelInfo{{ID: "fixture-model"}}},
			}
			rt.publishModelsForAuth(projection, plugin)
			if adapter.saved == nil || adapter.saved.StateVersion != 3 || adapter.saved.Metadata["account_id"] != "updated" {
				t.Fatal("plugin update was not persisted at the revision discovery read")
			}
			current, _ := rt.coreManager.GetByID(projection.ID)
			if current == nil || !current.RuntimeProjection || current.Metadata["token"] != nil {
				t.Fatal("full credential replaced the scheduler projection")
			}
			if wantVersion := map[int64]int64{4: 4, 0: 3}[tc.accepted]; current.StateVersion != wantVersion {
				t.Fatalf("scheduler revision = %d, want %d", current.StateVersion, wantVersion)
			}
		})
	}
}
