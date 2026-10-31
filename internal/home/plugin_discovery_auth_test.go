package home

import (
	"context"
	"errors"
	"testing"

	homeauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
)

type discoveryAuthAdapter struct {
	ClusterAdapter
	full  *homeauth.Auth
	err   error
	calls int
}

func (a *discoveryAuthAdapter) ObservedAuthState(string) (int64, bool) {
	if a.full == nil {
		return 0, false
	}
	return a.full.StateVersion, true
}
func (a *discoveryAuthAdapter) Enabled() bool { return true }
func (a *discoveryAuthAdapter) GetFullAuth(_ context.Context, id string) (*homeauth.Auth, error) {
	a.calls++
	return a.full, a.err
}

func TestPluginDiscoveryUsesFullCredentialWithoutChangingSchedulingProjection(t *testing.T) {
	projection := &homeauth.Auth{ID: "credential-id", Index: "credential-id", Provider: "fixture", RuntimeProjection: true}
	adapter := &discoveryAuthAdapter{full: &homeauth.Auth{ID: projection.ID, Provider: "fixture", Metadata: map[string]any{"token": "synthetic-first"}}}
	runtime := &Runtime{clusterAdapter: adapter}
	first, errResolve := runtime.pluginDiscoveryAuth(context.Background(), projection)
	if errResolve != nil || first == nil || first.Metadata["token"] != "synthetic-first" {
		t.Fatal("discovery did not receive the full provider credential")
	}
	first.Metadata["token"] = "mutated-copy"
	if projection.Metadata != nil || adapter.full.Metadata["token"] != "synthetic-first" {
		t.Fatal("discovery changed the scheduler projection or authoritative credential")
	}
	// Reconnect updates the same identity. The next callback resolves it again.
	adapter.full.Metadata["token"] = "synthetic-reconnected"
	second, errReconnect := runtime.pluginDiscoveryAuth(context.Background(), projection)
	if errReconnect != nil || second.Metadata["token"] != "synthetic-reconnected" || second.ID != projection.ID || adapter.calls != 2 {
		t.Fatal("reconnect did not use the current credential with the same identity")
	}
	saved := adapter.full.Clone()
	for _, state := range []string{"deleted", "disabled", "refresh-blocked", "provider-changed", "resolver-failed"} {
		t.Run(state, func(t *testing.T) {
			full := saved.Clone()
			adapter.err = nil
			switch state {
			case "deleted":
				full = nil
			case "disabled":
				full.Disabled = true
			case "refresh-blocked":
				full.RuntimeRefreshBlocked = true
			case "provider-changed":
				full.Provider = "other"
			case "resolver-failed":
				adapter.err = errors.New("private resolver detail")
			}
			adapter.full = full
			resolved, errUnavailable := runtime.pluginDiscoveryAuth(context.Background(), projection)
			if resolved != nil || errUnavailable == nil || errUnavailable.Error() == "private resolver detail" {
				t.Fatal("unavailable credential reached discovery or leaked a resolver error")
			}
		})
	}
}

func TestPluginDiscoveryNonClusterAuthIsCopied(t *testing.T) {
	auth := &homeauth.Auth{ID: "local", Provider: "fixture", Metadata: map[string]any{"token": "synthetic-local"}}
	resolved, errResolve := (&Runtime{}).pluginDiscoveryAuth(context.Background(), auth)
	if errResolve != nil || resolved.Metadata["token"] != "synthetic-local" {
		t.Fatal("non-cluster discovery lost its credential")
	}
	resolved.Metadata["token"] = "changed"
	if auth.Metadata["token"] != "synthetic-local" {
		t.Fatal("discovery mutated local auth")
	}
}

func TestPluginDiscoveryDiscardsResultsAfterRevocationOrReconnect(t *testing.T) {
	projection := &homeauth.Auth{ID: "credential-id", Provider: "fixture", RuntimeProjection: true}
	adapter := &discoveryAuthAdapter{full: &homeauth.Auth{ID: projection.ID, Provider: "fixture", StateVersion: 1, Metadata: map[string]any{"token": "synthetic-first"}}}
	runtime := &Runtime{clusterAdapter: adapter}
	discovered, errResolve := runtime.pluginDiscoveryAuth(context.Background(), projection)
	if errResolve != nil || !runtime.pluginDiscoveryStillCurrent(projection, discovered) {
		t.Fatal("current discovery was rejected")
	}
	adapter.full.StateVersion = 2
	if runtime.pluginDiscoveryStillCurrent(projection, discovered) {
		t.Fatal("reconnected credential accepted stale discovery")
	}
	adapter.full = nil
	if runtime.pluginDiscoveryStillCurrent(projection, discovered) {
		t.Fatal("revoked credential accepted stale discovery")
	}
}

type discoveryUpdateAdapter struct {
	discoveryAuthAdapter
}

func (a *discoveryUpdateAdapter) SaveWithStateVersion(_ context.Context, auth *homeauth.Auth) (string, int64, error) {
	if a.full == nil || auth.StateVersion != a.full.StateVersion {
		return auth.ID, 0, nil
	}
	a.full = auth.Clone()
	a.full.StateVersion++
	return auth.ID, a.full.StateVersion, nil
}

func (a *discoveryUpdateAdapter) ListMinimalAuths() []*homeauth.Auth {
	if a.full == nil {
		return nil
	}
	return []*homeauth.Auth{{ID: a.full.ID, Provider: a.full.Provider, Label: a.full.Label,
		StateVersion: a.full.StateVersion, RuntimeProjection: true}}
}

func TestPluginDiscoveryAuthUpdatePersistsFullRecordButKeepsSchedulerMinimal(t *testing.T) {
	ctx := context.Background()
	adapter := &discoveryUpdateAdapter{discoveryAuthAdapter: discoveryAuthAdapter{full: &homeauth.Auth{
		ID: "discovery-update", Provider: "fixture", StateVersion: 1,
		Metadata: map[string]any{"token": "synthetic-token"},
	}}}
	manager := homeauth.NewManager(nil, nil, nil)
	projection := adapter.ListMinimalAuths()[0]
	if _, errRegister := manager.Register(homeauth.WithSkipPersist(ctx), projection); errRegister != nil {
		t.Fatal(errRegister)
	}
	runtime := &Runtime{clusterAdapter: adapter, coreManager: manager}
	discovered := adapter.full.Clone()
	// The SDK merges a partial label update with the callback's full credential.
	update := pluginAuthToHomeAuth(homeAuthToPluginAuth(discovered))
	update.Label = "discovered label"
	applied, errUpdate := runtime.applyPluginDiscoveryAuthUpdate(ctx, update, discovered)
	if errUpdate != nil || applied == nil || applied.Label != update.Label || applied.StateVersion != 2 {
		t.Fatalf("discovery update was not applied: %v", errUpdate)
	}
	current, exists := manager.GetByID(projection.ID)
	if !exists || !current.RuntimeProjection || current.Metadata != nil || current.Storage != nil {
		t.Fatal("full discovery credential entered scheduler state")
	}
	if adapter.full.Metadata["token"] != "synthetic-token" || adapter.full.Label != update.Label {
		t.Fatal("partial plugin update damaged the authoritative credential")
	}
	if _, errStale := runtime.applyPluginDiscoveryAuthUpdate(ctx, update, discovered); errStale == nil {
		t.Fatal("stale discovery overwrote a newer credential")
	}
	adapter.full = nil
	if _, errRevoked := runtime.applyPluginDiscoveryAuthUpdate(ctx, update, applied); errRevoked == nil {
		t.Fatal("discovery update restored a revoked credential")
	}
}

func TestPluginDiscoveryPublicationPreservesNewerModelsAndCannotRestoreRevokedModels(t *testing.T) {
	const id = "discovery-publication"
	modelRegistry := registry.GetGlobalRegistry()
	t.Cleanup(func() { modelRegistry.UnregisterClient(id) })
	adapter := &discoveryAuthAdapter{full: &homeauth.Auth{ID: id, Provider: "fixture", StateVersion: 1}}
	runtime := &Runtime{clusterAdapter: adapter, coreManager: homeauth.NewManager(nil, nil, nil)}
	discovered := adapter.full.Clone()
	register := func(model string) {
		runtime.registerResolvedModelsForAuth(discovered, "fixture", []*ModelInfo{{ID: model}}, nil)
	}
	runtime.publishPluginDiscoveryModels(discovered, discovered, func() { register("old-model") })
	if modelRegistry.GetModelInfoForClient(id, "old-model") == nil {
		t.Fatal("current discovery did not publish its models")
	}
	adapter.full.StateVersion++
	register("new-model")
	runtime.publishPluginDiscoveryModels(discovered, discovered, func() { register("stale-model") })
	if modelRegistry.GetModelInfoForClient(id, "new-model") == nil || modelRegistry.GetModelInfoForClient(id, "stale-model") != nil {
		t.Fatal("stale discovery replaced or removed reconnected models")
	}
	adapter.full = nil
	runtime.applyCoreAuthRemove(homeauth.WithSkipPersist(context.Background()), id)
	runtime.publishPluginDiscoveryModels(discovered, discovered, func() { register("revoked-model") })
	if modelRegistry.GetModelInfoForClient(id, "revoked-model") != nil {
		t.Fatal("discovery restored models after revocation")
	}
}
