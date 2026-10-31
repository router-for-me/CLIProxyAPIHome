package home

import (
	"context"
	"fmt"
	"strings"

	homeauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
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
	return true
}
