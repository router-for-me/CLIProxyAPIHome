package synthesizer

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	appconfig "github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
	"github.com/router-for-me/CLIProxyAPIHome/internal/watcher/diff"
)

const homeConfigModelsMetadataKey = "home_config_models"

// ConfigSynthesizer generates Auth entries from configuration API keys.
// It handles Gemini, Interactions, Claude, Codex, xAI, OpenAI-compat, and Vertex-compat providers.
type ConfigSynthesizer struct{}

// NewConfigSynthesizer creates a new ConfigSynthesizer instance.
func NewConfigSynthesizer() *ConfigSynthesizer {
	return &ConfigSynthesizer{}
}

// Synthesize generates Auth entries from config API keys.
func (s *ConfigSynthesizer) Synthesize(ctx *SynthesisContext) ([]*coreauth.Auth, error) {
	out := make([]*coreauth.Auth, 0, 32)
	if ctx == nil || ctx.Config == nil {
		return out, nil
	}

	// Gemini API Keys
	out = append(out, s.synthesizeGeminiKeys(ctx)...)
	// Native Interactions API Keys
	out = append(out, s.synthesizeInteractionsKeys(ctx)...)
	// Claude API Keys
	out = append(out, s.synthesizeClaudeKeys(ctx)...)
	// Codex API Keys
	out = append(out, s.synthesizeCodexKeys(ctx)...)
	// xAI API Keys
	out = append(out, s.synthesizeXAIKeys(ctx)...)
	// OpenAI-compat
	out = append(out, s.synthesizeOpenAICompat(ctx)...)
	// Vertex-compat
	out = append(out, s.synthesizeVertexCompat(ctx)...)

	return out, nil
}

// synthesizeGeminiKeys creates Auth entries for Gemini API keys.
func (s *ConfigSynthesizer) synthesizeGeminiKeys(ctx *SynthesisContext) []*coreauth.Auth {
	return s.synthesizeGeminiKeyEntries(ctx, ctx.Config.GeminiKey, "gemini:apikey", "gemini", "gemini-apikey", "gemini")
}

// LegacyGeminiConfigSource returns the source used before Gemini routing fields became part of credential identity.
func LegacyGeminiConfigSource(auth *coreauth.Auth) string {
	if auth == nil || auth.Provider != "gemini" || auth.Attributes == nil {
		return ""
	}
	if !strings.HasPrefix(strings.TrimSpace(auth.Attributes["source"]), "config:gemini[") {
		return ""
	}
	key := strings.TrimSpace(auth.Attributes["api_key"])
	if key == "" {
		return ""
	}
	_, token := NewStableIDGenerator().Next("gemini:apikey", key, strings.TrimSpace(auth.Attributes["base_url"]))
	return fmt.Sprintf("config:gemini[%s]", token)
}

// GeminiConfigSource derives the current source from a stored Gemini credential identity.
func GeminiConfigSource(auth *coreauth.Auth) string {
	if auth == nil || auth.Provider != "gemini" || auth.Attributes == nil {
		return ""
	}
	key := strings.TrimSpace(auth.Attributes["api_key"])
	base := strings.TrimSpace(auth.Attributes["base_url"])
	if key == "" && base == "" {
		return ""
	}
	headers := make(map[string]string)
	for name, value := range auth.Attributes {
		if strings.HasPrefix(name, "header:") {
			headers[strings.TrimPrefix(name, "header:")] = value
		}
	}
	_, token := NewStableIDGenerator().Next(
		"gemini:apikey",
		key,
		base,
		strings.TrimSpace(auth.ProxyURL),
		strings.TrimSpace(auth.Prefix),
		appconfig.FormatSortedHeaders(headers),
	)
	return fmt.Sprintf("config:gemini[%s]", token)
}

// synthesizeInteractionsKeys creates Auth entries for native Interactions API keys.
func (s *ConfigSynthesizer) synthesizeInteractionsKeys(ctx *SynthesisContext) []*coreauth.Auth {
	return s.synthesizeGeminiKeyEntries(ctx, ctx.Config.InteractionsKey, "gemini-interactions:apikey", "interactions", "interactions-apikey", "gemini-interactions")
}

func (s *ConfigSynthesizer) synthesizeGeminiKeyEntries(ctx *SynthesisContext, entries []appconfig.GeminiKey, idKind, sourceName, label, provider string) []*coreauth.Auth {
	// Normalize source data before building the derived payload.
	cfg := ctx.Config
	now := ctx.Now
	idGen := ctx.IDGenerator

	out := make([]*coreauth.Auth, 0, len(entries))
	for i := range entries {
		entry := entries[i]
		key := strings.TrimSpace(entry.APIKey)
		base := strings.TrimSpace(entry.BaseURL)
		if key == "" && base == "" {
			continue
		}
		prefix := strings.TrimSpace(entry.Prefix)
		proxyURL := strings.TrimSpace(entry.ProxyURL)
		id, token := idGen.Next(idKind, key, base, proxyURL, prefix, appconfig.FormatSortedHeaders(entry.Headers))
		attrs := map[string]string{
			"source":       fmt.Sprintf("config:%s[%s]", sourceName, token),
			"config_index": strconv.Itoa(i),
		}
		if key != "" {
			attrs["api_key"] = key
		}
		metadata := map[string]any{}
		if entry.DisableCooling != nil {
			metadata["disable_cooling"] = *entry.DisableCooling
		}
		addRequestRetryToMetadata(entry.RequestRetry, metadata)
		addConfigModelsToMetadata(metadata, buildConfigModels(entry.Models, "google", "gemini", now))
		if entry.Priority != 0 {
			attrs["priority"] = strconv.Itoa(entry.Priority)
		}
		if base != "" {
			attrs["base_url"] = base
		}
		if hash := diff.ComputeGeminiModelsHash(entry.Models); hash != "" {
			attrs["models_hash"] = hash
		}
		addConfigHeadersToAttrs(entry.Headers, attrs)
		a := &coreauth.Auth{
			ID:         id,
			Provider:   provider,
			Label:      label,
			Prefix:     prefix,
			Status:     coreauth.StatusActive,
			ProxyURL:   proxyURL,
			Attributes: attrs,
			Metadata:   metadata,
			CreatedAt:  now,
			UpdatedAt:  now,
		}
		ApplyAuthExcludedModelsMeta(a, cfg, entry.ExcludedModels, "apikey")
		if len(a.Metadata) == 0 {
			a.Metadata = nil
		}
		applyProviderCredentialID(ctx, a, entry.ID)
		out = append(out, a)
	}
	return out
}

// synthesizeClaudeKeys creates Auth entries for Claude API keys.
func (s *ConfigSynthesizer) synthesizeClaudeKeys(ctx *SynthesisContext) []*coreauth.Auth {
	// Normalize source data before building the derived payload.
	cfg := ctx.Config
	now := ctx.Now
	idGen := ctx.IDGenerator

	out := make([]*coreauth.Auth, 0, len(cfg.ClaudeKey))
	for i := range cfg.ClaudeKey {
		ck := cfg.ClaudeKey[i]
		key := strings.TrimSpace(ck.APIKey)
		if key == "" {
			continue
		}
		prefix := strings.TrimSpace(ck.Prefix)
		base := strings.TrimSpace(ck.BaseURL)
		id, token := idGen.Next("claude:apikey", key, base)
		attrs := map[string]string{
			"source":  fmt.Sprintf("config:claude[%s]", token),
			"api_key": key,
		}
		metadata := map[string]any{}
		if ck.DisableCooling != nil {
			metadata["disable_cooling"] = *ck.DisableCooling
		}
		addRequestRetryToMetadata(ck.RequestRetry, metadata)
		addConfigModelsToMetadata(metadata, buildConfigModels(ck.Models, "anthropic", "claude", now))
		if ck.Priority != 0 {
			attrs["priority"] = strconv.Itoa(ck.Priority)
		}
		if base != "" {
			attrs["base_url"] = base
		}
		if hash := diff.ComputeClaudeModelsHash(ck.Models); hash != "" {
			attrs["models_hash"] = hash
		}
		addConfigHeadersToAttrs(ck.Headers, attrs)
		proxyURL := strings.TrimSpace(ck.ProxyURL)
		a := &coreauth.Auth{
			ID:         id,
			Provider:   "claude",
			Label:      "claude-apikey",
			Prefix:     prefix,
			Status:     coreauth.StatusActive,
			ProxyURL:   proxyURL,
			Attributes: attrs,
			Metadata:   metadata,
			CreatedAt:  now,
			UpdatedAt:  now,
		}
		ApplyAuthExcludedModelsMeta(a, cfg, ck.ExcludedModels, "apikey")
		if len(a.Metadata) == 0 {
			a.Metadata = nil
		}
		applyProviderCredentialID(ctx, a, ck.ID)
		out = append(out, a)
	}
	return out
}

// synthesizeCodexKeys creates Auth entries for Codex API keys.
func (s *ConfigSynthesizer) synthesizeCodexKeys(ctx *SynthesisContext) []*coreauth.Auth {
	return s.synthesizeCodexStyleKeys(ctx, ctx.Config.CodexKey, "codex")
}

// synthesizeXAIKeys creates Auth entries for xAI API keys.
func (s *ConfigSynthesizer) synthesizeXAIKeys(ctx *SynthesisContext) []*coreauth.Auth {
	return s.synthesizeCodexStyleKeys(ctx, ctx.Config.XAIKey, "xai")
}

func (s *ConfigSynthesizer) synthesizeCodexStyleKeys(ctx *SynthesisContext, entries []appconfig.CodexKey, provider string) []*coreauth.Auth {
	cfg := ctx.Config
	now := ctx.Now
	idGen := ctx.IDGenerator

	out := make([]*coreauth.Auth, 0, len(entries))
	for i := range entries {
		entry := entries[i]
		key := strings.TrimSpace(entry.APIKey)
		if key == "" {
			continue
		}
		prefix := strings.TrimSpace(entry.Prefix)
		baseURL := strings.TrimSpace(entry.BaseURL)
		id, token := idGen.Next(provider+":apikey", key, baseURL)
		attrs := map[string]string{
			"source":  fmt.Sprintf("config:%s[%s]", provider, token),
			"api_key": key,
		}
		metadata := map[string]any{}
		if entry.DisableCooling != nil {
			metadata["disable_cooling"] = *entry.DisableCooling
		}
		addRequestRetryToMetadata(entry.RequestRetry, metadata)
		modelOwner := provider
		modelType := provider
		if provider == "codex" {
			modelOwner = "openai"
			modelType = "openai"
		}
		models := buildConfigModels(entry.Models, modelOwner, modelType, now)
		if provider == "codex" {
			models = registry.WithCodexBuiltins(models)
		}
		addConfigModelsToMetadata(metadata, models)
		if entry.Priority != 0 {
			attrs["priority"] = strconv.Itoa(entry.Priority)
		}
		if baseURL != "" {
			attrs["base_url"] = baseURL
		}
		if entry.Websockets {
			attrs["websockets"] = "true"
		}
		if provider == "codex" && entry.AlphaSearch {
			attrs[coreauth.AttributeCodexAlphaSearch] = "true"
		}
		if hash := diff.ComputeCodexModelsHash(entry.Models); hash != "" {
			attrs["models_hash"] = hash
		}
		addConfigHeadersToAttrs(entry.Headers, attrs)
		a := &coreauth.Auth{
			ID:         id,
			Provider:   provider,
			Label:      provider + "-apikey",
			Prefix:     prefix,
			Status:     coreauth.StatusActive,
			ProxyURL:   strings.TrimSpace(entry.ProxyURL),
			Attributes: attrs,
			Metadata:   metadata,
			CreatedAt:  now,
			UpdatedAt:  now,
		}
		ApplyAuthExcludedModelsMeta(a, cfg, entry.ExcludedModels, "apikey")
		if len(a.Metadata) == 0 {
			a.Metadata = nil
		}
		applyProviderCredentialID(ctx, a, entry.ID)
		out = append(out, a)
	}
	return out
}

// synthesizeOpenAICompat creates Auth entries for OpenAI-compatible providers.
func (s *ConfigSynthesizer) synthesizeOpenAICompat(ctx *SynthesisContext) []*coreauth.Auth {
	cfg := ctx.Config
	now := ctx.Now
	idGen := ctx.IDGenerator

	out := make([]*coreauth.Auth, 0)
	for i := range cfg.OpenAICompatibility {
		compat := &cfg.OpenAICompatibility[i]
		if compat.Disabled {
			continue
		}
		prefix := strings.TrimSpace(compat.Prefix)
		providerName := strings.ToLower(strings.TrimSpace(compat.Name))
		if providerName == "" {
			providerName = "openai-compatibility"
		}
		base := strings.TrimSpace(compat.BaseURL)
		disableCooling := compat.DisableCooling
		requestRetry := compat.RequestRetry

		// Handle new APIKeyEntries format (preferred)
		createdEntries := 0
		for j := range compat.APIKeyEntries {
			entry := &compat.APIKeyEntries[j]
			key := strings.TrimSpace(entry.APIKey)
			proxyURL := strings.TrimSpace(entry.ProxyURL)
			idKind := fmt.Sprintf("openai-compatibility:%s", providerName)
			id, token := idGen.Next(idKind, key, base, proxyURL)
			attrs := map[string]string{
				"source":       fmt.Sprintf("config:%s[%s]", providerName, token),
				"base_url":     base,
				"compat_name":  compat.Name,
				"provider_key": providerName,
			}
			metadata := map[string]any{}
			if disableCooling != nil {
				metadata["disable_cooling"] = *disableCooling
			}
			addRequestRetryToMetadata(requestRetry, metadata)
			addConfigModelsToMetadata(metadata, buildOpenAICompatibilityModels(compat.Models, compat.Name, now))
			if compat.Priority != 0 {
				attrs["priority"] = strconv.Itoa(compat.Priority)
			}
			if key != "" {
				attrs["api_key"] = key
			}
			if hash := diff.ComputeOpenAICompatModelsHash(compat.Models); hash != "" {
				attrs["models_hash"] = hash
			}
			addConfigHeadersToAttrs(compat.Headers, attrs)
			a := &coreauth.Auth{
				ID:         id,
				Provider:   providerName,
				Label:      compat.Name,
				Prefix:     prefix,
				Status:     coreauth.StatusActive,
				ProxyURL:   proxyURL,
				Attributes: attrs,
				Metadata:   metadata,
				CreatedAt:  now,
				UpdatedAt:  now,
			}
			if len(a.Metadata) == 0 {
				a.Metadata = nil
			}
			applyProviderCredentialID(ctx, a, entry.ID)
			out = append(out, a)
			createdEntries++
		}
		// Fallback: create entry without API key if no APIKeyEntries
		if createdEntries == 0 {
			idKind := fmt.Sprintf("openai-compatibility:%s", providerName)
			id, token := idGen.Next(idKind, base)
			attrs := map[string]string{
				"source":       fmt.Sprintf("config:%s[%s]", providerName, token),
				"base_url":     base,
				"compat_name":  compat.Name,
				"provider_key": providerName,
			}
			metadata := map[string]any{}
			if disableCooling != nil {
				metadata["disable_cooling"] = *disableCooling
			}
			addRequestRetryToMetadata(requestRetry, metadata)
			addConfigModelsToMetadata(metadata, buildOpenAICompatibilityModels(compat.Models, compat.Name, now))
			if compat.Priority != 0 {
				attrs["priority"] = strconv.Itoa(compat.Priority)
			}
			if hash := diff.ComputeOpenAICompatModelsHash(compat.Models); hash != "" {
				attrs["models_hash"] = hash
			}
			addConfigHeadersToAttrs(compat.Headers, attrs)
			a := &coreauth.Auth{
				ID:         id,
				Provider:   providerName,
				Label:      compat.Name,
				Prefix:     prefix,
				Status:     coreauth.StatusActive,
				Attributes: attrs,
				Metadata:   metadata,
				CreatedAt:  now,
				UpdatedAt:  now,
			}
			if len(a.Metadata) == 0 {
				a.Metadata = nil
			}
			applyProviderCredentialID(ctx, a, compat.ID)
			out = append(out, a)
		}
	}
	return out
}

// synthesizeVertexCompat creates Auth entries for Vertex-compatible providers.
func (s *ConfigSynthesizer) synthesizeVertexCompat(ctx *SynthesisContext) []*coreauth.Auth {
	// Normalize source data before building the derived payload.
	cfg := ctx.Config
	now := ctx.Now
	idGen := ctx.IDGenerator

	out := make([]*coreauth.Auth, 0, len(cfg.VertexCompatAPIKey))
	for i := range cfg.VertexCompatAPIKey {
		compat := &cfg.VertexCompatAPIKey[i]
		providerName := "vertex"
		base := strings.TrimSpace(compat.BaseURL)

		key := strings.TrimSpace(compat.APIKey)
		prefix := strings.TrimSpace(compat.Prefix)
		proxyURL := strings.TrimSpace(compat.ProxyURL)
		idKind := "vertex:apikey"
		id, token := idGen.Next(idKind, key, base, proxyURL)
		attrs := map[string]string{
			"source":       fmt.Sprintf("config:vertex-apikey[%s]", token),
			"base_url":     base,
			"provider_key": providerName,
		}
		metadata := map[string]any{}
		if compat.DisableCooling != nil {
			metadata["disable_cooling"] = *compat.DisableCooling
		}
		addRequestRetryToMetadata(compat.RequestRetry, metadata)
		addConfigModelsToMetadata(metadata, buildConfigModels(compat.Models, "google", "vertex", now))
		if compat.Priority != 0 {
			attrs["priority"] = strconv.Itoa(compat.Priority)
		}
		if key != "" {
			attrs["api_key"] = key
		}
		if hash := diff.ComputeVertexCompatModelsHash(compat.Models); hash != "" {
			attrs["models_hash"] = hash
		}
		addConfigHeadersToAttrs(compat.Headers, attrs)
		a := &coreauth.Auth{
			ID:         id,
			Provider:   providerName,
			Label:      "vertex-apikey",
			Prefix:     prefix,
			Status:     coreauth.StatusActive,
			ProxyURL:   proxyURL,
			Attributes: attrs,
			Metadata:   metadata,
			CreatedAt:  now,
			UpdatedAt:  now,
		}
		ApplyAuthExcludedModelsMeta(a, cfg, compat.ExcludedModels, "apikey")
		if len(a.Metadata) == 0 {
			a.Metadata = nil
		}
		applyProviderCredentialID(ctx, a, compat.ID)
		out = append(out, a)
	}
	return out
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
func buildConfigModels[T modelEntry](models []T, ownedBy, modelType string, now time.Time) []*registry.ModelInfo {
	// Normalize source data before building the derived payload.
	if len(models) == 0 {
		return nil
	}
	created := now.Unix()
	if created == 0 {
		created = time.Now().Unix()
	}
	out := make([]*registry.ModelInfo, 0, len(models))
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
		info := &registry.ModelInfo{
			ID:                alias,
			Object:            "model",
			Created:           created,
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

// buildOpenAICompatibilityModels builds an open ai compatibility models.
func buildOpenAICompatibilityModels(models []appconfig.OpenAICompatibilityModel, compatName string, now time.Time) []*registry.ModelInfo {
	// Normalize source data before building the derived payload.
	if len(models) == 0 {
		return nil
	}
	created := now.Unix()
	if created == 0 {
		created = time.Now().Unix()
	}
	out := make([]*registry.ModelInfo, 0, len(models))
	seen := make(map[string]struct{}, len(models))
	for i := range models {
		model := models[i]
		modelID := strings.TrimSpace(model.Alias)
		if modelID == "" {
			modelID = strings.TrimSpace(model.Name)
		}
		if modelID == "" {
			continue
		}
		key := strings.ToLower(modelID)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		thinking := model.Thinking
		if thinking == nil {
			thinking = &registry.ThinkingSupport{Levels: []string{"low", "medium", "high"}}
		}
		out = append(out, &registry.ModelInfo{
			ID:          modelID,
			Name:        strings.TrimSpace(model.Name),
			Object:      "model",
			Created:     created,
			OwnedBy:     compatName,
			Type:        "openai-compatibility",
			DisplayName: modelID,
			UserDefined: true,
			Thinking:    thinking,
		})
	}
	return out
}

// addConfigModelsToMetadata converts add config models to metadata.
func addConfigModelsToMetadata(metadata map[string]any, models []*registry.ModelInfo) {
	if metadata == nil || len(models) == 0 {
		return
	}
	payload := modelInfoMetadataPayload(models)
	if len(payload) == 0 {
		return
	}
	metadata[homeConfigModelsMetadataKey] = payload
}

// addRequestRetryToMetadata copies a non-negative credential retry override.
func addRequestRetryToMetadata(requestRetry *int, metadata map[string]any) {
	if requestRetry == nil || *requestRetry < 0 || metadata == nil {
		return
	}
	metadata["request_retry"] = *requestRetry
}

// modelInfoMetadataPayload handles a model info metadata payload.
func modelInfoMetadataPayload(models []*registry.ModelInfo) []map[string]any {
	if len(models) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(models))
	for _, model := range models {
		if model == nil || strings.TrimSpace(model.ID) == "" {
			continue
		}
		raw, errMarshal := json.Marshal(model)
		if errMarshal != nil {
			continue
		}
		var item map[string]any
		if errUnmarshal := json.Unmarshal(raw, &item); errUnmarshal != nil {
			continue
		}
		item["user_defined"] = model.UserDefined
		if displayName := strings.TrimSpace(model.ConfigDisplayName); displayName != "" {
			item["config_display_name"] = displayName
		}
		if model.ForceMapping {
			item["force_mapping"] = true
		}
		out = append(out, item)
	}
	return out
}
