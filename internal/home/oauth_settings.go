package home

import (
	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
)

// applyOAuthModelConfig is shared by built-in, discovered and plugin catalogs.
// Upstream candidates retain general settings independently of alias overrides.
// Visible routes apply exclusions, aliases, then settings before prefixing.
func applyOAuthModelConfig(cfg *config.Config, provider, authKind string, models []*ModelInfo, excluded []string) ([]*ModelInfo, []*ModelInfo) {
	candidates := applyOAuthSettings(cfg, provider, authKind, append([]*ModelInfo{}, models...))
	models = applyExcludedModels(models, excluded)
	models = applyOAuthModelAlias(cfg, provider, authKind, models)
	models = applyOAuthSettings(cfg, provider, authKind, models)
	return models, candidates
}

// applyOAuthSettings updates Home's authoritative catalog before prefixes.
// API-key model capabilities remain credential-owned and are not changed here.
func applyOAuthSettings(cfg *config.Config, provider, authKind string, models []*ModelInfo) []*ModelInfo {
	if cfg == nil || len(models) == 0 {
		return models
	}
	channel := coreauth.OAuthModelAliasChannel(provider, authKind)
	settings := cfg.OAuthSettings[channel]
	if channel == "" || len(settings) == 0 {
		return models
	}
	out := make([]*ModelInfo, 0, len(models))
	for _, model := range models {
		if model == nil {
			continue
		}
		setting := config.ResolveOAuthModelSetting(settings, model.ID, model.MetadataModelID, model.Name, model.SourceModelID)
		if setting != nil && setting.MaxContextLength > 0 {
			clone := *model
			clone.ContextLength = setting.MaxContextLength
			out = append(out, &clone)
		} else {
			out = append(out, model)
		}
	}
	return out
}
