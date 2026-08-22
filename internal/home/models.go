package home

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
)

const (
	homeConfigModelsMetadataKey = "home_config_models"
)

type ModelInfo = registry.ModelInfo

// modelInfosFromAuthMetadata derives model infos from auth metadata.
func modelInfosFromAuthMetadata(a *coreauth.Auth, key string) []*ModelInfo {
	// Normalize source data before building the derived payload.
	if a == nil || len(a.Metadata) == 0 {
		return nil
	}
	raw, ok := a.Metadata[key]
	if !ok || raw == nil {
		return nil
	}
	data, errMarshal := json.Marshal(raw)
	if errMarshal != nil {
		return nil
	}
	var items []map[string]any
	if errUnmarshal := json.Unmarshal(data, &items); errUnmarshal != nil {
		return nil
	}
	out := make([]*ModelInfo, 0, len(items))
	for _, item := range items {
		if len(item) == 0 {
			continue
		}
		rawItem, errMarshalItem := json.Marshal(item)
		if errMarshalItem != nil {
			continue
		}
		var model ModelInfo
		if errUnmarshalItem := json.Unmarshal(rawItem, &model); errUnmarshalItem != nil {
			continue
		}
		if strings.TrimSpace(model.ID) == "" {
			continue
		}
		if model.Object == "" {
			model.Object = "model"
		}
		if model.Created == 0 {
			model.Created = time.Now().Unix()
		}
		model.UserDefined = boolFromMetadataValue(item["user_defined"])
		model.ConfigDisplayName = strings.TrimSpace(stringFromMetadataValue(item["config_display_name"]))
		model.ForceMapping = boolFromMetadataValue(item["force_mapping"])
		out = append(out, &model)
	}
	return out
}

// stringFromMetadataValue derives a string from metadata values.
func stringFromMetadataValue(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case []byte:
		return string(typed)
	default:
		return ""
	}
}

// boolFromMetadataValue derives bool from metadata value.
func boolFromMetadataValue(value any) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	case string:
		return strings.EqualFold(strings.TrimSpace(typed), "true") || strings.TrimSpace(typed) == "1"
	default:
		return false
	}
}

// openAICompatInfoFromAuth derives open ai compat info from auth.
func openAICompatInfoFromAuth(a *coreauth.Auth) (providerKey string, compatName string, ok bool) {
	if a == nil {
		return "", "", false
	}
	if len(a.Attributes) > 0 {
		providerKey = strings.TrimSpace(a.Attributes["provider_key"])
		compatName = strings.TrimSpace(a.Attributes["compat_name"])
		if compatName != "" {
			if providerKey == "" {
				providerKey = compatName
			}
			return strings.ToLower(providerKey), compatName, true
		}
	}
	if strings.EqualFold(strings.TrimSpace(a.Provider), "openai-compatibility") {
		return "openai-compatibility", strings.TrimSpace(a.Label), true
	}
	return "", "", false
}

// registerResolvedModelsForAuth returns a register resolved models for auth.
func (r *Runtime) registerResolvedModelsForAuth(a *coreauth.Auth, providerKey string, models []*ModelInfo) {
	if a == nil || a.ID == "" {
		return
	}
	if len(models) == 0 {
		registry.GetGlobalRegistry().UnregisterClient(a.ID)
		return
	}
	registry.GetGlobalRegistry().RegisterClient(a.ID, providerKey, models)
}

// registerModelsForAuth (re)binds provider models in the global registry using the core auth ID as client identifier.
func (r *Runtime) registerModelsForAuth(a *coreauth.Auth) {
	if r == nil || a == nil || a.ID == "" {
		return
	}
	if a.Disabled || a.Status == coreauth.StatusDisabled {
		registry.GetGlobalRegistry().UnregisterClient(a.ID)
		return
	}

	r.cfgMu.RLock()
	cfg := r.cfg
	r.cfgMu.RUnlock()

	authKind := strings.ToLower(strings.TrimSpace(a.Attributes["auth_kind"]))
	if authKind == "" {
		if kind, _ := a.AccountInfo(); strings.EqualFold(kind, "api_key") {
			authKind = "apikey"
		}
	}
	// Unregister legacy client ID (if present) to avoid double counting
	if a.Runtime != nil {
		if idGetter, ok := a.Runtime.(interface{ GetClientID() string }); ok {
			if rid := idGetter.GetClientID(); rid != "" && rid != a.ID {
				registry.GetGlobalRegistry().UnregisterClient(rid)
			}
		}
	}

	provider := strings.ToLower(strings.TrimSpace(a.Provider))
	compatProviderKey, compatDisplayName, compatDetected := openAICompatInfoFromAuth(a)
	if compatDetected {
		provider = "openai-compatibility"
	}

	excluded := r.oauthExcludedModels(cfg, provider, authKind)
	// The synthesizer pre-merges per-account and global exclusions into the "excluded_models" attribute.
	// If this attribute is present, it represents the complete list of exclusions and overrides the global config.
	if a.Attributes != nil {
		if val, ok := a.Attributes["excluded_models"]; ok && strings.TrimSpace(val) != "" {
			excluded = strings.Split(val, ",")
		}
	}
	configModels := modelInfosFromAuthMetadata(a, homeConfigModelsMetadataKey)
	if r.tryRegisterPluginModelsForAuth(context.Background(), a, provider, authKind, excluded) {
		return
	}

	var models []*ModelInfo
	switch provider {
	case "gemini":
		models = registry.GetGeminiModels()
		if len(configModels) > 0 {
			models = configModels
		} else if entry := r.resolveConfigGeminiKey(cfg, a); entry != nil {
			if len(entry.Models) > 0 {
				models = buildGeminiConfigModels(entry)
			}
			if authKind == "apikey" {
				excluded = entry.ExcludedModels
			}
		}
		models = applyExcludedModels(models, excluded)
	case "gemini-interactions":
		models = registry.GetGeminiModels()
		if len(configModels) > 0 {
			models = configModels
		} else if entry := r.resolveConfigInteractionsKey(cfg, a); entry != nil {
			if len(entry.Models) > 0 {
				models = buildGeminiConfigModels(entry)
			}
			if authKind == "apikey" {
				excluded = entry.ExcludedModels
			}
		}
		models = applyExcludedModels(models, excluded)
	case "vertex":
		models = registry.GetGeminiVertexModels()
		if len(configModels) > 0 {
			models = configModels
		} else if entry := r.resolveConfigVertexCompatKey(cfg, a); entry != nil {
			if len(entry.Models) > 0 {
				models = buildVertexCompatConfigModels(entry)
			}
			if authKind == "apikey" {
				excluded = entry.ExcludedModels
			}
		}
		models = applyExcludedModels(models, excluded)
	case "antigravity":
		models = registry.GetAntigravityModels()
		models = applyExcludedModels(models, excluded)
	case "claude":
		models = registry.GetClaudeModels()
		if len(configModels) > 0 {
			models = configModels
		} else if entry := r.resolveConfigClaudeKey(cfg, a); entry != nil {
			if len(entry.Models) > 0 {
				models = buildClaudeConfigModels(entry)
			}
			if authKind == "apikey" {
				excluded = entry.ExcludedModels
			}
		}
		models = applyExcludedModels(models, excluded)
	case "codex":
		codexPlanType := ""
		if a.Attributes != nil {
			codexPlanType = strings.TrimSpace(a.Attributes["plan_type"])
		}
		switch strings.ToLower(codexPlanType) {
		case "pro":
			models = registry.GetCodexProModels()
		case "plus":
			models = registry.GetCodexPlusModels()
		case "team", "business", "go":
			models = registry.GetCodexTeamModels()
		case "free":
			models = registry.GetCodexFreeModels()
		default:
			models = registry.GetCodexProModels()
		}
		if len(configModels) > 0 {
			models = configModels
		} else if entry := r.resolveConfigCodexKey(cfg, a); entry != nil {
			if len(entry.Models) > 0 {
				models = buildCodexConfigModels(entry)
			}
			if authKind == "apikey" {
				excluded = entry.ExcludedModels
			}
		}
		models = applyExcludedModels(models, excluded)
	case "kimi":
		models = registry.GetKimiModels()
		models = applyExcludedModels(models, excluded)
	case "xai":
		models = registry.GetXAIModels()
		if len(configModels) > 0 {
			models = configModels
		} else if entry := r.resolveConfigXAIKey(cfg, a); entry != nil {
			if len(entry.Models) > 0 {
				models = buildXAIConfigModels(entry)
			}
			if authKind == "apikey" {
				excluded = entry.ExcludedModels
			}
		}
		models = applyExcludedModels(models, excluded)
	default:
		if len(configModels) > 0 {
			providerKey := provider
			if compatDetected {
				if compatProviderKey != "" {
					providerKey = compatProviderKey
				}
				if strings.EqualFold(providerKey, "openai-compatibility") && compatDisplayName != "" {
					providerKey = strings.ToLower(compatDisplayName)
				}
			}
			if providerKey == "" {
				providerKey = "openai-compatibility"
			}
			forcePrefix := cfg != nil && cfg.ForceModelPrefix
			r.registerResolvedModelsForAuth(a, providerKey, applyModelPrefixes(configModels, a.Prefix, forcePrefix))
			return
		}
		if cfg != nil {
			providerKey := provider
			compatName := strings.TrimSpace(a.Provider)
			isCompatAuth := false
			if compatDetected {
				if compatProviderKey != "" {
					providerKey = compatProviderKey
				}
				if compatDisplayName != "" {
					compatName = compatDisplayName
				}
				isCompatAuth = true
			}

			if strings.EqualFold(providerKey, "openai-compatibility") {
				isCompatAuth = true
				if a.Attributes != nil {
					if v := strings.TrimSpace(a.Attributes["compat_name"]); v != "" {
						compatName = v
					}
					if v := strings.TrimSpace(a.Attributes["provider_key"]); v != "" {
						providerKey = strings.ToLower(v)
						isCompatAuth = true
					}
				}
				if providerKey == "openai-compatibility" && compatName != "" {
					providerKey = strings.ToLower(compatName)
				}
			} else if a.Attributes != nil {
				if v := strings.TrimSpace(a.Attributes["compat_name"]); v != "" {
					compatName = v
					isCompatAuth = true
				}
				if v := strings.TrimSpace(a.Attributes["provider_key"]); v != "" {
					providerKey = strings.ToLower(v)
					isCompatAuth = true
				}
			}

			for i := range cfg.OpenAICompatibility {
				compat := &cfg.OpenAICompatibility[i]
				if compat.Disabled {
					continue
				}
				if strings.EqualFold(compat.Name, compatName) {
					isCompatAuth = true
					ms := make([]*ModelInfo, 0, len(compat.Models))
					for j := range compat.Models {
						m := compat.Models[j]
						modelID := m.Alias
						if modelID == "" {
							modelID = m.Name
						}
						thinking := m.Thinking
						if thinking == nil {
							thinking = &registry.ThinkingSupport{Levels: []string{"low", "medium", "high"}}
						}
						ms = append(ms, &ModelInfo{
							ID:          modelID,
							Object:      "model",
							Created:     time.Now().Unix(),
							OwnedBy:     compat.Name,
							Type:        "openai-compatibility",
							DisplayName: modelID,
							UserDefined: false,
							Thinking:    thinking,
						})
					}
					if len(ms) > 0 {
						if providerKey == "" {
							providerKey = "openai-compatibility"
						}
						r.registerResolvedModelsForAuth(a, providerKey, applyModelPrefixes(ms, a.Prefix, cfg.ForceModelPrefix))
					} else {
						registry.GetGlobalRegistry().UnregisterClient(a.ID)
					}
					return
				}
			}

			if isCompatAuth {
				registry.GetGlobalRegistry().UnregisterClient(a.ID)
				return
			}
		}
	}

	key := provider
	if key == "" {
		key = strings.ToLower(strings.TrimSpace(a.Provider))
	}
	models = r.appendPluginModels(key, models)
	models = applyExcludedModels(models, excluded)
	models = applyOAuthModelAlias(cfg, provider, authKind, models)
	if len(models) > 0 {
		forcePrefix := cfg != nil && cfg.ForceModelPrefix
		r.registerResolvedModelsForAuth(a, key, applyModelPrefixes(models, a.Prefix, forcePrefix))
		return
	}

	registry.GetGlobalRegistry().UnregisterClient(a.ID)
}

// oauthExcludedModels handles an oauth excluded models.
func (r *Runtime) oauthExcludedModels(cfg *config.Config, provider, authKind string) []string {
	if cfg == nil {
		return nil
	}
	authKindKey := strings.ToLower(strings.TrimSpace(authKind))
	providerKey := strings.ToLower(strings.TrimSpace(provider))
	if authKindKey == "apikey" {
		return nil
	}
	return cfg.OAuthExcludedModels[providerKey]
}

// resolveConfigClaudeKey resolves a config claude key.
func (r *Runtime) resolveConfigClaudeKey(cfg *config.Config, auth *coreauth.Auth) *config.ClaudeKey {
	// Normalize source data before building the derived payload.
	if auth == nil || cfg == nil {
		return nil
	}
	var attrKey, attrBase string
	if auth.Attributes != nil {
		attrKey = strings.TrimSpace(auth.Attributes["api_key"])
		attrBase = strings.TrimSpace(auth.Attributes["base_url"])
	}
	for i := range cfg.ClaudeKey {
		entry := &cfg.ClaudeKey[i]
		cfgKey := strings.TrimSpace(entry.APIKey)
		cfgBase := strings.TrimSpace(entry.BaseURL)
		if attrKey != "" && attrBase != "" {
			if strings.EqualFold(cfgKey, attrKey) && strings.EqualFold(cfgBase, attrBase) {
				return entry
			}
			continue
		}
		if attrKey != "" && strings.EqualFold(cfgKey, attrKey) {
			if cfgBase == "" || strings.EqualFold(cfgBase, attrBase) {
				return entry
			}
		}
		if attrKey == "" && attrBase != "" && strings.EqualFold(cfgBase, attrBase) {
			return entry
		}
	}
	if attrKey != "" {
		for i := range cfg.ClaudeKey {
			entry := &cfg.ClaudeKey[i]
			if strings.EqualFold(strings.TrimSpace(entry.APIKey), attrKey) {
				return entry
			}
		}
	}
	return nil
}

// resolveConfigGeminiKey resolves a config gemini key.
func (r *Runtime) resolveConfigGeminiKey(cfg *config.Config, auth *coreauth.Auth) *config.GeminiKey {
	// Normalize source data before building the derived payload.
	if cfg == nil {
		return nil
	}
	return resolveConfigGeminiKeyEntry(auth, cfg.GeminiKey)
}

// resolveConfigInteractionsKey resolves a config native Interactions key.
func (r *Runtime) resolveConfigInteractionsKey(cfg *config.Config, auth *coreauth.Auth) *config.GeminiKey {
	if cfg == nil {
		return nil
	}
	return resolveConfigGeminiKeyEntry(auth, cfg.InteractionsKey)
}

func resolveConfigGeminiKeyEntry(auth *coreauth.Auth, entries []config.GeminiKey) *config.GeminiKey {
	if auth == nil {
		return nil
	}
	var attrKey, attrBase string
	if auth.Attributes != nil {
		attrKey = strings.TrimSpace(auth.Attributes["api_key"])
		attrBase = strings.TrimSpace(auth.Attributes["base_url"])
	}
	matchesCredentials := func(entry config.GeminiKey) bool {
		cfgKey := strings.TrimSpace(entry.APIKey)
		cfgBase := strings.TrimSpace(entry.BaseURL)
		if attrKey != "" && attrBase != "" {
			return strings.EqualFold(cfgKey, attrKey) && strings.EqualFold(cfgBase, attrBase)
		}
		if attrKey != "" {
			return strings.EqualFold(cfgKey, attrKey) && (cfgBase == "" || strings.EqualFold(cfgBase, attrBase))
		}
		return attrBase != "" && strings.EqualFold(cfgBase, attrBase)
	}
	if auth.Attributes != nil {
		index, errIndex := strconv.Atoi(strings.TrimSpace(auth.Attributes["config_index"]))
		if errIndex == nil && index >= 0 && index < len(entries) && matchesCredentials(entries[index]) {
			return &entries[index]
		}
	}
	for i := range entries {
		entry := entries[i]
		if matchesCredentials(entry) && strings.EqualFold(strings.TrimSpace(entry.Prefix), strings.TrimSpace(auth.Prefix)) && strings.EqualFold(strings.TrimSpace(entry.ProxyURL), strings.TrimSpace(auth.ProxyURL)) {
			return &entries[i]
		}
	}
	for i := range entries {
		if matchesCredentials(entries[i]) {
			return &entries[i]
		}
	}
	return nil
}

// resolveConfigVertexCompatKey resolves a config vertex compat key.
func (r *Runtime) resolveConfigVertexCompatKey(cfg *config.Config, auth *coreauth.Auth) *config.VertexCompatKey {
	// Normalize source data before building the derived payload.
	if auth == nil || cfg == nil {
		return nil
	}
	var attrKey, attrBase string
	if auth.Attributes != nil {
		attrKey = strings.TrimSpace(auth.Attributes["api_key"])
		attrBase = strings.TrimSpace(auth.Attributes["base_url"])
	}
	for i := range cfg.VertexCompatAPIKey {
		entry := &cfg.VertexCompatAPIKey[i]
		cfgKey := strings.TrimSpace(entry.APIKey)
		cfgBase := strings.TrimSpace(entry.BaseURL)
		if attrKey != "" && strings.EqualFold(cfgKey, attrKey) {
			if cfgBase == "" || strings.EqualFold(cfgBase, attrBase) {
				return entry
			}
			continue
		}
		if attrKey == "" && attrBase != "" && strings.EqualFold(cfgBase, attrBase) {
			return entry
		}
	}
	if attrKey != "" {
		for i := range cfg.VertexCompatAPIKey {
			entry := &cfg.VertexCompatAPIKey[i]
			if strings.EqualFold(strings.TrimSpace(entry.APIKey), attrKey) {
				return entry
			}
		}
	}
	return nil
}

// resolveConfigCodexKey resolves a config codex key.
func (r *Runtime) resolveConfigCodexKey(cfg *config.Config, auth *coreauth.Auth) *config.CodexKey {
	if cfg == nil {
		return nil
	}
	return resolveConfigCodexStyleKey(auth, cfg.CodexKey)
}

// resolveConfigXAIKey resolves a config xAI key.
func (r *Runtime) resolveConfigXAIKey(cfg *config.Config, auth *coreauth.Auth) *config.XAIKey {
	if cfg == nil {
		return nil
	}
	return resolveConfigCodexStyleKey(auth, cfg.XAIKey)
}

func resolveConfigCodexStyleKey(auth *coreauth.Auth, entries []config.CodexKey) *config.CodexKey {
	if auth == nil {
		return nil
	}
	var attrKey, attrBase string
	if auth.Attributes != nil {
		attrKey = strings.TrimSpace(auth.Attributes["api_key"])
		attrBase = strings.TrimSpace(auth.Attributes["base_url"])
	}
	for i := range entries {
		entry := &entries[i]
		cfgKey := strings.TrimSpace(entry.APIKey)
		cfgBase := strings.TrimSpace(entry.BaseURL)
		if attrKey != "" && strings.EqualFold(cfgKey, attrKey) {
			if cfgBase == "" || strings.EqualFold(cfgBase, attrBase) {
				return entry
			}
			continue
		}
		if attrKey == "" && attrBase != "" && strings.EqualFold(cfgBase, attrBase) {
			return entry
		}
	}
	return nil
}

// applyExcludedModels applies an excluded models.
func applyExcludedModels(models []*ModelInfo, excluded []string) []*ModelInfo {
	// Normalize source data before building the derived payload.
	if len(models) == 0 || len(excluded) == 0 {
		return models
	}

	patterns := make([]string, 0, len(excluded))
	for _, item := range excluded {
		if trimmed := strings.TrimSpace(item); trimmed != "" {
			patterns = append(patterns, strings.ToLower(trimmed))
		}
	}
	if len(patterns) == 0 {
		return models
	}

	filtered := make([]*ModelInfo, 0, len(models))
	for _, model := range models {
		if model == nil {
			continue
		}
		modelID := strings.ToLower(strings.TrimSpace(model.ID))
		blocked := false
		for _, pattern := range patterns {
			if matchWildcard(pattern, modelID) {
				blocked = true
				break
			}
		}
		if !blocked {
			filtered = append(filtered, model)
		}
	}
	return filtered
}

// applyModelPrefixes applies a model prefixes.
func applyModelPrefixes(models []*ModelInfo, prefix string, forceModelPrefix bool) []*ModelInfo {
	// Normalize source data before building the derived payload.
	trimmedPrefix := strings.TrimSpace(prefix)
	if trimmedPrefix == "" || len(models) == 0 {
		return models
	}

	out := make([]*ModelInfo, 0, len(models)*2)
	seen := make(map[string]struct{}, len(models)*2)

	addModel := func(model *ModelInfo) {
		if model == nil {
			return
		}
		id := strings.TrimSpace(model.ID)
		if id == "" {
			return
		}
		if _, exists := seen[id]; exists {
			return
		}
		seen[id] = struct{}{}
		out = append(out, model)
	}

	for _, model := range models {
		if model == nil {
			continue
		}
		baseID := strings.TrimSpace(model.ID)
		if baseID == "" {
			continue
		}
		if !forceModelPrefix || trimmedPrefix == baseID {
			addModel(model)
		}
		clone := *model
		clone.ID = trimmedPrefix + "/" + baseID
		addModel(&clone)
	}
	return out
}

// matchWildcard handles a match wildcard.
func matchWildcard(pattern, value string) bool {
	// Keep validation before state changes so failures leave existing data intact.
	if pattern == "" {
		return false
	}

	if !strings.Contains(pattern, "*") {
		return pattern == value
	}

	parts := strings.Split(pattern, "*")
	if prefix := parts[0]; prefix != "" {
		if !strings.HasPrefix(value, prefix) {
			return false
		}
		value = value[len(prefix):]
	}

	if suffix := parts[len(parts)-1]; suffix != "" {
		if !strings.HasSuffix(value, suffix) {
			return false
		}
		value = value[:len(value)-len(suffix)]
	}

	for i := 1; i < len(parts)-1; i++ {
		segment := parts[i]
		if segment == "" {
			continue
		}
		idx := strings.Index(value, segment)
		if idx < 0 {
			return false
		}
		value = value[idx+len(segment):]
	}

	return true
}

type modelEntry interface {
	GetName() string
	GetAlias() string
}

type displayNameModelEntry interface {
	GetDisplayName() string
}

type forceMappingModelEntry interface {
	GetForceMapping() bool
}

// buildConfigModels builds a config models.
func buildConfigModels[T modelEntry](models []T, ownedBy, modelType string) []*ModelInfo {
	// Normalize source data before building the derived payload.
	if len(models) == 0 {
		return nil
	}
	now := time.Now().Unix()
	out := make([]*ModelInfo, 0, len(models))
	seen := make(map[string]struct{}, len(models))
	for i := range models {
		model := models[i]
		name := strings.TrimSpace(model.GetName())
		alias := strings.TrimSpace(model.GetAlias())
		if alias == "" {
			alias = name
		}
		if alias == "" {
			continue
		}
		key := strings.ToLower(alias)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		configuredDisplay := ""
		if displayEntry, okDisplay := any(model).(displayNameModelEntry); okDisplay {
			configuredDisplay = strings.TrimSpace(displayEntry.GetDisplayName())
		}
		display := configuredDisplay
		if display == "" {
			display = name
		}
		if display == "" {
			display = alias
		}
		forceMapping := false
		if forceEntry, okForce := any(model).(forceMappingModelEntry); okForce {
			forceMapping = forceEntry.GetForceMapping()
		}
		info := &ModelInfo{
			ID:                alias,
			Object:            "model",
			Created:           now,
			OwnedBy:           ownedBy,
			Type:              modelType,
			DisplayName:       display,
			Name:              name,
			UserDefined:       true,
			ConfigDisplayName: configuredDisplay,
			ForceMapping:      forceMapping,
		}
		if name != "" {
			if upstream := registry.LookupStaticModelInfo(name); upstream != nil && upstream.Thinking != nil {
				info.Thinking = upstream.Thinking
			}
		}
		out = append(out, info)
	}
	return out
}

// buildVertexCompatConfigModels builds a vertex compat config models.
func buildVertexCompatConfigModels(entry *config.VertexCompatKey) []*ModelInfo {
	if entry == nil {
		return nil
	}
	return buildConfigModels(entry.Models, "google", "vertex")
}

// buildGeminiConfigModels builds a gemini config models.
func buildGeminiConfigModels(entry *config.GeminiKey) []*ModelInfo {
	if entry == nil {
		return nil
	}
	return buildConfigModels(entry.Models, "google", "gemini")
}

// buildClaudeConfigModels builds a claude config models.
func buildClaudeConfigModels(entry *config.ClaudeKey) []*ModelInfo {
	if entry == nil {
		return nil
	}
	return buildConfigModels(entry.Models, "anthropic", "claude")
}

// buildXAIConfigModels builds xAI config models.
func buildXAIConfigModels(entry *config.XAIKey) []*ModelInfo {
	if entry == nil {
		return nil
	}
	return buildConfigModels(entry.Models, "xai", "xai")
}

// buildCodexConfigModels builds a codex config models.
func buildCodexConfigModels(entry *config.CodexKey) []*ModelInfo {
	if entry == nil {
		return nil
	}
	return registry.WithCodexBuiltins(buildConfigModels(entry.Models, "openai", "openai"))
}

// rewriteModelInfoName rewrites a model info name.
func rewriteModelInfoName(name, oldID, newID string) string {
	// Normalize source data before building the derived payload.
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return name
	}
	oldID = strings.TrimSpace(oldID)
	newID = strings.TrimSpace(newID)
	if oldID == "" || newID == "" {
		return name
	}
	if strings.EqualFold(oldID, newID) {
		return name
	}
	if strings.EqualFold(trimmed, oldID) {
		return newID
	}
	if strings.HasSuffix(trimmed, "/"+oldID) {
		prefix := strings.TrimSuffix(trimmed, oldID)
		return prefix + newID
	}
	if trimmed == "models/"+oldID {
		return "models/" + newID
	}
	return name
}

// applyOAuthModelAlias applies an o auth model alias.
func applyOAuthModelAlias(cfg *config.Config, provider, authKind string, models []*ModelInfo) []*ModelInfo {
	// Resolve credential context before calling upstream OAuth services.
	if cfg == nil || len(models) == 0 {
		return models
	}
	channel := coreauth.OAuthModelAliasChannel(provider, authKind)
	if channel == "" || len(cfg.OAuthModelAlias) == 0 {
		return models
	}
	aliases := cfg.OAuthModelAlias[channel]
	if len(aliases) == 0 {
		return models
	}

	type aliasEntry struct {
		alias string
		fork  bool
	}

	forward := make(map[string][]aliasEntry, len(aliases))
	for i := range aliases {
		name := strings.TrimSpace(aliases[i].Name)
		alias := strings.TrimSpace(aliases[i].Alias)
		if name == "" || alias == "" {
			continue
		}
		if strings.EqualFold(name, alias) {
			continue
		}
		key := strings.ToLower(name)
		forward[key] = append(forward[key], aliasEntry{alias: alias, fork: aliases[i].Fork})
	}
	if len(forward) == 0 {
		return models
	}

	out := make([]*ModelInfo, 0, len(models))
	seen := make(map[string]struct{}, len(models))
	for _, model := range models {
		if model == nil {
			continue
		}
		id := strings.TrimSpace(model.ID)
		if id == "" {
			continue
		}
		key := strings.ToLower(id)
		entries := forward[key]
		if len(entries) == 0 {
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, model)
			continue
		}

		keepOriginal := false
		for _, entry := range entries {
			if entry.fork {
				keepOriginal = true
				break
			}
		}
		if keepOriginal {
			if _, exists := seen[key]; !exists {
				seen[key] = struct{}{}
				out = append(out, model)
			}
		}

		addedAlias := false
		for _, entry := range entries {
			mappedID := strings.TrimSpace(entry.alias)
			if mappedID == "" {
				continue
			}
			if strings.EqualFold(mappedID, id) {
				continue
			}
			aliasKey := strings.ToLower(mappedID)
			if _, exists := seen[aliasKey]; exists {
				continue
			}
			seen[aliasKey] = struct{}{}
			clone := *model
			clone.ID = mappedID
			if clone.Name != "" {
				clone.Name = rewriteModelInfoName(clone.Name, id, mappedID)
			}
			out = append(out, &clone)
			addedAlias = true
		}

		if !keepOriginal && !addedAlias {
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, model)
		}
	}
	return out
}
