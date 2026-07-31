package home

import (
	"context"
	"strings"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
	log "github.com/sirupsen/logrus"
)

// applyCoreAuthAddOrUpdate applies a core auth add or update.
func (r *Runtime) applyCoreAuthAddOrUpdate(ctx context.Context, auth *coreauth.Auth) {
	// Normalize auth state before updating runtime indexes.
	if r == nil || r.coreManager == nil || auth == nil || auth.ID == "" {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}

	auth = auth.Clone()

	op := "register"
	var err error
	if existing, ok := r.coreManager.GetByID(auth.ID); ok && existing != nil {
		auth.CreatedAt = existing.CreatedAt
		if !existing.Disabled && existing.Status != coreauth.StatusDisabled && !auth.Disabled && auth.Status != coreauth.StatusDisabled {
			auth.LastRefreshedAt = existing.LastRefreshedAt
			auth.NextRefreshAfter = existing.NextRefreshAfter
			auth.ModelStates = mergeModelStates(auth.ModelStates, existing.ModelStates)
		}
		op = "update"
		_, err = r.coreManager.Update(ctx, auth)
	} else {
		_, err = r.coreManager.Register(ctx, auth)
	}
	if err != nil {
		log.Errorf("failed to %s auth %s: %v", op, auth.ID, err)
		current, ok := r.coreManager.GetByID(auth.ID)
		if !ok || current == nil || current.Disabled {
			registry.GetGlobalRegistry().UnregisterClient(auth.ID)
			return
		}
		auth = current
	}

	r.registerModelsForAuth(auth)
	r.coreManager.ReconcileRegistryModelStates(ctx, auth.ID)
	r.coreManager.RefreshSchedulerEntry(auth.ID)
}

// mergeModelStates merges the locally accumulated model states into the
// incoming cluster view, keeping whichever per-model state is fresher. Local
// states cover transitions that are not persisted to the cluster row (for
// example transient 5xx cooldowns), while incoming states carry cooldowns
// recorded by other Home nodes.
func mergeModelStates(incoming, local map[string]*coreauth.ModelState) map[string]*coreauth.ModelState {
	if len(local) == 0 {
		return incoming
	}
	if len(incoming) == 0 {
		return local
	}
	for model, state := range local {
		if state == nil {
			continue
		}
		current, ok := incoming[model]
		if !ok || current == nil || state.UpdatedAt.After(current.UpdatedAt) {
			incoming[model] = state
		}
	}
	return incoming
}

// loadClusterAuths loads a cluster auths.
func (r *Runtime) loadClusterAuths(ctx context.Context, adapter ClusterAdapter) error {
	// Normalize auth state before updating runtime indexes.
	if r == nil || r.coreManager == nil || adapter == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if errLoadIndex := adapter.LoadAuthIndex(ctx); errLoadIndex != nil {
		return errLoadIndex
	}

	auths := adapter.ListMinimalAuths()
	desired := make(map[string]*coreauth.Auth, len(auths))
	ctxSkipPersist := coreauth.WithSkipPersist(ctx)
	for _, auth := range auths {
		if auth == nil || strings.TrimSpace(auth.ID) == "" {
			continue
		}
		auth.ID = strings.TrimSpace(auth.ID)
		auth.Index = auth.ID
		desired[auth.ID] = auth
		r.applyCoreAuthAddOrUpdate(ctxSkipPersist, auth)
	}

	removed := 0
	current := r.coreManager.List()
	for _, auth := range current {
		if auth == nil || strings.TrimSpace(auth.ID) == "" {
			continue
		}
		if _, ok := desired[auth.ID]; ok {
			continue
		}
		r.applyCoreAuthRemove(ctxSkipPersist, auth.ID)
		removed++
	}

	log.Infof("loaded cluster auth index (auths=%d removed=%d)", len(desired), removed)
	return nil
}

// registerModelRefreshCallback handles a register model refresh callback.
func (r *Runtime) registerModelRefreshCallback() {
	// Resolve credential context before calling upstream OAuth services.
	registry.SetModelRefreshCallback(func(changedProviders []string) {
		if r == nil || r.coreManager == nil || len(changedProviders) == 0 {
			return
		}

		providerSet := make(map[string]bool, len(changedProviders))
		for _, p := range changedProviders {
			providerSet[strings.ToLower(strings.TrimSpace(p))] = true
		}

		auths := r.coreManager.List()
	refreshedLoop:
		for _, item := range auths {
			if item == nil || item.ID == "" {
				continue
			}
			auth, ok := r.coreManager.GetByID(item.ID)
			if !ok || auth == nil || auth.Disabled {
				continue
			}
			provider := strings.ToLower(strings.TrimSpace(auth.Provider))
			if !providerSet[provider] {
				continue
			}
			r.registerModelsForAuth(auth)
			r.coreManager.ReconcileRegistryModelStates(context.Background(), auth.ID)
			r.coreManager.RefreshSchedulerEntry(auth.ID)
			continue refreshedLoop
		}
	})
}

// applyCoreAuthRemove applies a core auth remove.
func (r *Runtime) applyCoreAuthRemove(ctx context.Context, authID string) {
	if r == nil || r.coreManager == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return
	}

	// Best-effort logging context before deletion.
	provider := ""
	label := ""
	if auth, ok := r.coreManager.GetByID(authID); ok && auth != nil {
		provider = strings.TrimSpace(auth.Provider)
		label = strings.TrimSpace(auth.Label)
	}

	if errDel := r.coreManager.Delete(ctx, authID); errDel != nil {
		log.Errorf("failed to delete auth %s: %v", authID, errDel)
	}
	registry.GetGlobalRegistry().UnregisterClient(authID)

	if provider != "" {
		log.Infof("auth removed (auth=%s provider=%s label=%s)", authID, provider, label)
	} else {
		log.Infof("auth removed (auth=%s)", authID)
	}
}

// availableProviderKeys handles an available provider keys.
func (r *Runtime) availableProviderKeys() []string {
	// Build the candidate view before applying availability rules.
	if r == nil || r.coreManager == nil {
		return nil
	}

	auths := r.coreManager.List()
	seen := make(map[string]struct{}, len(auths))
	out := make([]string, 0, len(auths))
	for _, auth := range auths {
		if auth == nil || auth.Disabled {
			continue
		}
		provider := strings.ToLower(strings.TrimSpace(auth.Provider))
		if provider == "" {
			continue
		}
		if _, ok := seen[provider]; ok {
			continue
		}
		seen[provider] = struct{}{}
		out = append(out, provider)
	}
	return out
}

// extractAccessToken extracts an access token.
func extractAccessToken(auth *coreauth.Auth) string {
	// Resolve credential context before calling upstream OAuth services.
	if auth == nil {
		return ""
	}

	if auth.Metadata == nil {
		return ""
	}

	if v, ok := auth.Metadata["access_token"].(string); ok {
		if trimmed := strings.TrimSpace(v); trimmed != "" {
			return trimmed
		}
	}

	for _, nestedKey := range []string{"token", "Token"} {
		raw, ok := auth.Metadata[nestedKey]
		if !ok || raw == nil {
			continue
		}
		switch tokenMap := raw.(type) {
		case map[string]any:
			if v, ok := tokenMap["access_token"].(string); ok {
				if trimmed := strings.TrimSpace(v); trimmed != "" {
					return trimmed
				}
			}
		case map[string]string:
			if v, ok := tokenMap["access_token"]; ok {
				if trimmed := strings.TrimSpace(v); trimmed != "" {
					return trimmed
				}
			}
		}
	}
	return ""
}
