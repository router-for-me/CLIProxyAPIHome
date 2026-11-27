package home

import (
	"context"
	"strings"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
)

const LocalExecutorPrefix = "acp/"

// IsLocalExecutorUsage distinguishes a local account index from a Home credential.
func IsLocalExecutorUsage(provider, executorType, authID, alias string) bool {
	return provider != "" && executorType == "plugin:"+provider && strings.TrimSpace(authID) == "" &&
		strings.HasPrefix(alias, LocalExecutorPrefix)
}

// LocalExecutorRoute binds a public ModelInfo to the installed plugin and its exact registered ID.
type LocalExecutorRoute struct {
	Plugin       string
	Version      string
	ConfigDigest string
	Model        registry.ModelInfo
}

// LocalExecutorRoutes resolves the installed ACP catalogs before applying any caller policy.
// A non-nil second result reserves the requested ID for a credential model or explicit alias.
func (r *Runtime) LocalExecutorRoutes(ctx context.Context, requestedModel string) (map[string]LocalExecutorRoute, []string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	cfg, errConfig := r.pluginSyncConfig(ctx)
	if errConfig != nil {
		return nil, nil, errConfig
	}
	requestedModel = coreauth.CanonicalModelID(requestedModel)
	if requestedModel == "" && (cfg == nil || !cfg.Plugins.Enabled) {
		return nil, nil, nil
	}
	reserved := make(map[string][]string)
	if manager := r.CoreManager(); manager != nil {
		reg := registry.GetGlobalRegistry()
		for _, auth := range manager.List() {
			if auth == nil || auth.Disabled || auth.Status == coreauth.StatusDisabled {
				continue
			}
			_, models := reg.GetClientModelRoutes(auth.ID)
			for _, name := range models {
				if key := strings.ToLower(name); strings.HasPrefix(key, LocalExecutorPrefix) {
					reserved[key] = append(reserved[key], auth.ID)
				}
			}
		}
	}
	if cfg != nil {
		for _, aliases := range cfg.OAuthModelAlias {
			for _, alias := range aliases {
				name := strings.ToLower(coreauth.CanonicalModelID(alias.Alias))
				if strings.HasPrefix(name, LocalExecutorPrefix) {
					if _, registered := reserved[name]; !registered {
						reserved[name] = []string{}
					}
				}
			}
		}
	}

	if authIDs, exists := reserved[strings.ToLower(requestedModel)]; exists {
		return nil, authIDs, nil
	}
	if cfg == nil || !cfg.Plugins.Enabled {
		return nil, nil, nil
	}

	routes := make(map[string]LocalExecutorRoute)
	for _, plugin := range r.startLocalExecutorCatalogs(cfg) {
		if !plugin.catalog.available(ctx) {
			if errContext := ctx.Err(); errContext != nil {
				return nil, nil, errContext
			}
			continue
		}
		id := plugin.manifest.ID
		version := normalizeDesiredPluginFileVersion(plugin.manifest.Version)
		for _, model := range plugin.catalog.models {
			if model.ID == "" || strings.TrimSpace(model.ID) != model.ID {
				continue
			}
			public := LocalExecutorPrefix + model.ID
			if _, exists := reserved[strings.ToLower(public)]; exists || (requestedModel != "" && requestedModel != public) {
				continue
			}
			if _, exists := routes[public]; exists {
				continue
			}
			model.SourceModelID = model.ID
			model.ID = public
			model.Providers = []string{id}
			routes[public] = LocalExecutorRoute{Plugin: id, Version: version, Model: model, ConfigDigest: plugin.catalog.configDigest}
			if requestedModel != "" {
				return routes, nil, nil
			}
		}
	}
	return routes, nil, nil
}

// AdmitLocalExecutor applies the same model, channel, and billing checks as
// credential dispatch. An empty credential ID cannot satisfy channel scope.
func (r *Runtime) AdmitLocalExecutor(ctx context.Context, apiKey, model string) (bool, error) {
	authIDs, modelIDs, errAllowed := r.allowedDispatchIDsForAPIKey(ctx, apiKey, model)
	if errAllowed != nil {
		return false, errAllowed
	}
	return coreauth.NewModelAccess(authIDs, modelIDs, nil).Allows("", model), nil
}
