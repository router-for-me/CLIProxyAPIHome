package home

import (
	"context"
	"fmt"
	"strings"

	homeauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
)

func (r *Runtime) pluginDiscoveryAuth(ctx context.Context, auth *homeauth.Auth) (*homeauth.Auth, error) {
	if auth == nil {
		return nil, fmt.Errorf("plugin discovery auth is missing")
	}
	if r == nil || r.clusterAdapter == nil || !r.clusterAdapter.Enabled() {
		return auth.Clone(), nil
	}
	// The scheduler holds a secret-free projection. Only this callback gets the
	// current credential from the same full-auth resolver used for dispatch.
	full, errResolve := r.clusterAdapter.GetFullAuth(ctx, auth.ID)
	if errResolve != nil {
		return nil, fmt.Errorf("plugin discovery auth could not be resolved")
	}
	if full == nil || full.ID != auth.ID || full.Disabled || full.Status == homeauth.StatusDisabled || full.RuntimeRefreshBlocked ||
		!strings.EqualFold(strings.TrimSpace(full.Provider), strings.TrimSpace(auth.Provider)) {
		return nil, fmt.Errorf("plugin discovery auth is unavailable")
	}
	resolved := full.Clone()
	resolved.ID, resolved.Index = auth.ID, auth.Index
	return resolved, nil
}

func (r *Runtime) pluginDiscoveryStillCurrent(auth *homeauth.Auth, discovered *homeauth.Auth) bool {
	if r == nil || r.clusterAdapter == nil || !r.clusterAdapter.Enabled() {
		return true
	}
	if observer, ok := r.clusterAdapter.(clusterAuthStateObserver); ok {
		version, active := observer.ObservedAuthState(auth.ID)
		return active && version == discovered.StateVersion
	}
	return false
}

func (r *Runtime) applyPluginDiscoveryAuthUpdate(ctx context.Context, update, discovered *homeauth.Auth) (*homeauth.Auth, error) {
	if r.clusterAdapter == nil || !r.clusterAdapter.Enabled() {
		return r.coreManager.Update(ctx, update)
	}
	saver, ok := r.clusterAdapter.(homeauth.StateVersionSaver)
	if !ok {
		return nil, fmt.Errorf("plugin discovery auth update cannot be persisted")
	}
	// The SDK fills partial AuthUpdates from the full callback auth. Persist that
	// record with its observed revision, then keep only the cluster projection.
	update.StateVersion = discovered.StateVersion
	_, version, errSave := saver.SaveWithStateVersion(ctx, update)
	if errSave != nil || version <= 0 {
		return nil, fmt.Errorf("plugin discovery auth update was not accepted")
	}
	for _, projection := range r.clusterAdapter.ListMinimalAuths() {
		if projection != nil && projection.ID == update.ID && projection.StateVersion == version {
			return r.coreManager.Update(homeauth.WithSkipPersist(ctx), projection)
		}
	}
	return nil, fmt.Errorf("plugin discovery auth projection is unavailable")
}

func (r *Runtime) publishPluginDiscoveryModels(auth, discovered *homeauth.Auth, publish func()) {
	// Removal uses the same lock so a completed revocation cannot be followed by
	// a stale callback's registry write. Rejected callbacks leave newer models alone.
	r.pluginModelRegistryMu.Lock()
	defer r.pluginModelRegistryMu.Unlock()
	if r.pluginDiscoveryStillCurrent(auth, discovered) {
		if discovered.Disabled || discovered.Status == homeauth.StatusDisabled || discovered.RuntimeRefreshBlocked {
			registry.GetGlobalRegistry().UnregisterClient(auth.ID)
		} else {
			publish()
		}
	}
}
