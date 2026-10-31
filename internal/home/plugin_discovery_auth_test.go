package home

import (
	"context"
	"errors"
	"testing"

	homeauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
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
