package home

import (
	"context"
	"strings"

	sdkpluginhost "github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginhost"
	homeauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/logging"
	log "github.com/sirupsen/logrus"
)

type registeredPluginLister interface {
	RegisteredPlugins() []sdkpluginhost.RegisteredPluginInfo
}

// pluginDiscoveryAuth returns the credential passed to a plugin's per-auth
// model discovery. Cluster schedulers hold a secret-free projection, so the
// full record is loaded for the callback only. Any lookup problem keeps the
// projection, which is the previous behavior.
func (r *Runtime) pluginDiscoveryAuth(ctx context.Context, plugins modelPluginSource, auth *homeauth.Auth) *homeauth.Auth {
	if !auth.RuntimeProjection || r.clusterAdapter == nil || !r.clusterAdapter.Enabled() {
		return auth
	}
	if lister, ok := plugins.(registeredPluginLister); ok && len(lister.RegisteredPlugins()) == 0 {
		return auth
	}
	full, errFull := r.clusterAdapter.GetFullAuth(ctx, auth.ID)
	if errFull != nil {
		log.WithFields(log.Fields{"auth_id": auth.ID, "reason": logging.SafeErrorDiagnostic(errFull)}).Warn("plugin model discovery could not load the full credential")
		return auth
	}
	if full == nil || full.Disabled || full.Status == homeauth.StatusDisabled ||
		!strings.EqualFold(strings.TrimSpace(full.Provider), strings.TrimSpace(auth.Provider)) {
		return auth
	}
	full.ID, full.Index = auth.ID, auth.Index
	return full
}

// applyProjectedPluginAuthUpdate persists a plugin update built from the full
// credential at the revision discovery read, then keeps only the scheduler
// projection in memory. A stale or rejected update leaves auth unchanged.
func (r *Runtime) applyProjectedPluginAuthUpdate(ctx context.Context, auth, discovery, update *homeauth.Auth) *homeauth.Auth {
	saver, ok := r.clusterAdapter.(homeauth.StateVersionSaver)
	if !ok {
		return auth
	}
	update.StateVersion = discovery.StateVersion
	_, version, errSave := saver.SaveWithStateVersion(ctx, update)
	if errSave != nil || version <= 0 {
		log.WithField("auth_id", auth.ID).Warn("plugin model discovery auth update was not accepted")
		return auth
	}
	for _, projection := range r.clusterAdapter.ListMinimalAuths() {
		if projection == nil || projection.ID != auth.ID || projection.StateVersion != version {
			continue
		}
		if applied, errApply := r.coreManager.Update(homeauth.WithSkipPersist(ctx), projection); errApply == nil && applied != nil {
			return applied
		}
		break
	}
	return auth
}
