// Package registry provides model definitions and lookup helpers for various AI providers.
// Static model metadata is loaded from the embedded models.json file and can be refreshed from network.
package registry

import (
	"sort"
	"strings"
)

const (
	codexBuiltinImageModelID        = "gpt-image-2"
	xaiBuiltinImageModelID          = "grok-imagine-image"
	xaiBuiltinImageQualityModelID   = "grok-imagine-image-quality"
	xaiBuiltinVideoModelID          = "grok-imagine-video"
	xaiBuiltinVideo15PreviewModelID = "grok-imagine-video-1.5-preview"
)

// staticModelsJSON mirrors the top-level structure of models.json.
type staticModelsJSON struct {
	Claude      []*ModelInfo `json:"claude"`
	Gemini      []*ModelInfo `json:"gemini"`
	Vertex      []*ModelInfo `json:"vertex"`
	CodexFree   []*ModelInfo `json:"codex-free"`
	CodexTeam   []*ModelInfo `json:"codex-team"`
	CodexPlus   []*ModelInfo `json:"codex-plus"`
	CodexPro    []*ModelInfo `json:"codex-pro"`
	Kimi        []*ModelInfo `json:"kimi"`
	Antigravity []*ModelInfo `json:"antigravity"`
	XAI         []*ModelInfo `json:"xai"`
	Devin       []*ModelInfo `json:"devin"`
	Meta        []*ModelInfo `json:"meta"`
}

// GetClaudeModels returns the standard Claude model definitions.
func GetClaudeModels() []*ModelInfo {
	return cloneModelInfos(getModels().Claude)
}

// GetGeminiModels returns the standard Gemini model definitions.
func GetGeminiModels() []*ModelInfo {
	return cloneModelInfos(getModels().Gemini)
}

// GetGeminiVertexModels returns Gemini model definitions for Vertex AI.
func GetGeminiVertexModels() []*ModelInfo {
	return cloneModelInfos(getModels().Vertex)
}

// GetCodexFreeModels returns model definitions for the Codex free plan tier.
func GetCodexFreeModels() []*ModelInfo {
	return WithCodexBuiltins(cloneModelInfos(getModels().CodexFree))
}

// GetCodexTeamModels returns model definitions for the Codex team plan tier.
func GetCodexTeamModels() []*ModelInfo {
	return WithCodexBuiltins(cloneModelInfos(getModels().CodexTeam))
}

// GetCodexPlusModels returns model definitions for the Codex plus plan tier.
func GetCodexPlusModels() []*ModelInfo {
	return WithCodexBuiltins(cloneModelInfos(getModels().CodexPlus))
}

// GetCodexProModels returns model definitions for the Codex pro plan tier.
func GetCodexProModels() []*ModelInfo {
	return WithCodexBuiltins(cloneModelInfos(getModels().CodexPro))
}

// GetKimiModels returns the standard Kimi (Moonshot AI) model definitions.
func GetKimiModels() []*ModelInfo {
	return cloneModelInfos(getModels().Kimi)
}

// GetAntigravityModels returns the standard Antigravity model definitions.
func GetAntigravityModels() []*ModelInfo {
	return cloneModelInfos(getModels().Antigravity)
}

// GetXAIModels returns the standard xAI Grok model definitions.
func GetXAIModels() []*ModelInfo {
	return WithXAIBuiltins(cloneModelInfos(getModels().XAI))
}

// GetDevinModels returns the active Devin model catalog.
// It prefers the independent/embedded devin_models.json catalog, then models.json's
// devin section, and finally hardcoded staticDevinModels.
func GetDevinModels() []*ModelInfo {
	devinCatalogStore.mu.RLock()
	models := devinCatalogStore.models
	devinCatalogStore.mu.RUnlock()
	if len(models) > 0 {
		return cloneModelInfos(models)
	}
	if m := getModels(); m != nil && len(m.Devin) > 0 {
		return cloneModelInfos(m.Devin)
	}
	return cloneModelInfos(staticDevinModels)
}

// GetMetaModels returns the standard Meta Muse model definitions.
func GetMetaModels() []*ModelInfo {
	if m := getModels(); m != nil && len(m.Meta) > 0 {
		return cloneModelInfos(m.Meta)
	}
	return cloneModelInfos(staticMetaModels)
}

var staticDevinModels = []*ModelInfo{
	{ID: "devin/swe-2", Object: "model", Type: "devin", OwnedBy: "cognition", DisplayName: "SWE-2", ContextLength: 262000, MaxCompletionTokens: 128000, Thinking: &ThinkingSupport{Levels: []string{"medium", "high", "max"}}},
	{ID: "devin/claude-fable-5-1", Object: "model", Type: "devin", OwnedBy: "anthropic", DisplayName: "Claude Fable 5.1", ContextLength: 1000000, MaxCompletionTokens: 64000, Thinking: &ThinkingSupport{Levels: []string{"low", "medium", "high", "xhigh", "max"}}},
	{ID: "devin/gpt-6-astra", Object: "model", Type: "devin", OwnedBy: "openai", DisplayName: "GPT-6 Astra", ContextLength: 1000000, MaxCompletionTokens: 64000, Thinking: &ThinkingSupport{Levels: []string{"low", "medium", "high", "xhigh", "max"}}},
	{ID: "devin/glm-5-2", Object: "model", Type: "devin", OwnedBy: "zhipu", DisplayName: "GLM-5.2", ContextLength: 200000, MaxCompletionTokens: 64000, Thinking: &ThinkingSupport{Levels: []string{"none", "high"}}},
	{ID: "devin/glm-5-3", Object: "model", Type: "devin", OwnedBy: "zhipu", DisplayName: "GLM-5.3", ContextLength: 1048576, MaxCompletionTokens: 128000, Thinking: &ThinkingSupport{Levels: []string{"low", "high", "max"}}},
	{ID: "devin/glm-5-3-flash", Object: "model", Type: "devin", OwnedBy: "zhipu", DisplayName: "GLM-5.3 Flash", ContextLength: 1000000, MaxCompletionTokens: 128000, Thinking: &ThinkingSupport{Levels: []string{"low", "high", "max"}}},
	{ID: "devin/gpt-5-6-sol", Object: "model", Type: "devin", OwnedBy: "openai", DisplayName: "GPT-5.6 Sol", ContextLength: 1000000, MaxCompletionTokens: 128000, Thinking: &ThinkingSupport{Levels: []string{"none", "low", "medium", "high", "xhigh", "max"}}},
	{ID: "devin/gemini-3-8-flash", Object: "model", Type: "devin", OwnedBy: "google", DisplayName: "Gemini 3.8 Flash", ContextLength: 1048576, MaxCompletionTokens: 65536, Thinking: &ThinkingSupport{Levels: []string{"low", "medium", "high"}}},
	{ID: "devin/grok-4-6", Object: "model", Type: "devin", OwnedBy: "xai", DisplayName: "Grok 4.6", ContextLength: 500000, MaxCompletionTokens: 131072, Thinking: &ThinkingSupport{Levels: []string{"low", "medium", "high", "xhigh"}}},
	{ID: "devin/deepseek-v4-flash", Object: "model", Type: "devin", OwnedBy: "deepseek", DisplayName: "DeepSeek V4 Flash", ContextLength: 1048576, MaxCompletionTokens: 64000, Thinking: &ThinkingSupport{Levels: []string{"high", "max"}}},
	{ID: "devin/deepseek-v4-1-flash", Object: "model", Type: "devin", OwnedBy: "deepseek", DisplayName: "DeepSeek V4.1 Flash", ContextLength: 1048576, MaxCompletionTokens: 64000, Thinking: &ThinkingSupport{Levels: []string{"high", "max"}}},
}

var staticMetaModels = []*ModelInfo{
	{ID: "muse-spark-1.3", Object: "model", Created: 1787000000, OwnedBy: "meta", Type: "meta", DisplayName: "Muse Spark 1.3", Name: "muse-spark-1.3", Description: "Meta Muse Spark 1.3 flagship reasoning and agentic coding model", ContextLength: 1048576, MaxCompletionTokens: 65536, Thinking: &ThinkingSupport{Levels: []string{"minimal", "low", "medium", "high", "xhigh", "max"}}},
	{ID: "muse-spark-1.3-contributor", Object: "model", Created: 1787000000, OwnedBy: "meta", Type: "meta", DisplayName: "Muse Spark 1.3 Contributor", Name: "muse-spark-1.3-contributor", Description: "Meta Muse Spark 1.3 contributor model", ContextLength: 1048576, MaxCompletionTokens: 65536, Thinking: &ThinkingSupport{Levels: []string{"minimal", "low", "medium", "high", "xhigh", "max"}}},
	{ID: "muse-spark-1.2", Object: "model", Created: 1785000000, OwnedBy: "meta", Type: "meta", DisplayName: "Muse Spark 1.2", Name: "muse-spark-1.2", Description: "Meta Muse Spark 1.2 reasoning and agentic coding model", ContextLength: 1048576, MaxCompletionTokens: 65536, Thinking: &ThinkingSupport{Levels: []string{"minimal", "low", "medium", "high", "xhigh"}}},
	{ID: "muse-spark-1.2-contributor", Object: "model", Created: 1785000000, OwnedBy: "meta", Type: "meta", DisplayName: "Muse Spark 1.2 Contributor", Name: "muse-spark-1.2-contributor", Description: "Meta Muse Spark 1.2 contributor model", ContextLength: 1048576, MaxCompletionTokens: 65536, Thinking: &ThinkingSupport{Levels: []string{"minimal", "low", "medium", "high", "xhigh"}}},
	{ID: "muse-spark-1.1", Object: "model", Created: 1783000000, OwnedBy: "meta", Type: "meta", DisplayName: "Muse Spark 1.1", Name: "muse-spark-1.1", Description: "Meta Muse Spark 1.1 reasoning model", ContextLength: 1048576, MaxCompletionTokens: 65536, Thinking: &ThinkingSupport{Levels: []string{"low", "medium", "high", "xhigh"}}},
}

// GetAllStaticModelDefinitions returns static model definitions grouped by channel.
func GetAllStaticModelDefinitions() map[string][]*ModelInfo {
	channels := []string{
		"claude",
		"gemini",
		"gemini-interactions",
		"vertex",
		"codex-free",
		"codex-team",
		"codex-plus",
		"codex-pro",
		"kimi",
		"antigravity",
		"xai",
		"devin",
		"meta",
	}
	definitions := make(map[string][]*ModelInfo, len(channels))
	for _, channel := range channels {
		definitions[channel] = GetStaticModelDefinitionsByChannel(channel)
	}
	return definitions
}

// withStaticModelProviders annotates management-facing static definitions with billing providers.
func withStaticModelProviders(models []*ModelInfo, providers ...string) []*ModelInfo {
	if len(models) == 0 {
		return models
	}

	normalizedProviders := make([]string, 0, len(providers))
	seen := make(map[string]struct{}, len(providers))
	for _, provider := range providers {
		provider = strings.ToLower(strings.TrimSpace(provider))
		if provider == "" {
			continue
		}
		if _, exists := seen[provider]; exists {
			continue
		}
		seen[provider] = struct{}{}
		normalizedProviders = append(normalizedProviders, provider)
	}
	sort.Strings(normalizedProviders)

	for _, model := range models {
		if model == nil {
			continue
		}
		model.Providers = append([]string(nil), normalizedProviders...)
	}
	return models
}

// WithCodexBuiltins injects hard-coded Codex-only model definitions that should
// not depend on remote models.json updates. Built-ins replace any matching IDs
// already present in the provided slice.
func WithCodexBuiltins(models []*ModelInfo) []*ModelInfo {
	return upsertModelInfos(models, codexBuiltinImageModelInfo())
}

// WithXAIBuiltins injects hard-coded xAI image/video model definitions that should
// not depend on remote models.json updates.
func WithXAIBuiltins(models []*ModelInfo) []*ModelInfo {
	return upsertModelInfos(models, xaiBuiltinImageModelInfo(), xaiBuiltinImageQualityModelInfo(), xaiBuiltinVideoModelInfo(), xaiBuiltinVideo15PreviewModelInfo())
}

// codexBuiltinImageModelInfo handles a codex builtin image model info.
func codexBuiltinImageModelInfo() *ModelInfo {
	return &ModelInfo{
		ID:          codexBuiltinImageModelID,
		Object:      "model",
		Created:     1704067200, // 2024-01-01
		OwnedBy:     "openai",
		Type:        "openai",
		DisplayName: "GPT Image 2",
		Version:     codexBuiltinImageModelID,
	}
}

// xaiBuiltinImageModelInfo returns the built-in xAI image model definition.
func xaiBuiltinImageModelInfo() *ModelInfo {
	return &ModelInfo{
		ID:          xaiBuiltinImageModelID,
		Object:      "model",
		Created:     1735689600, // 2025-01-01
		OwnedBy:     "xai",
		Type:        "xai",
		DisplayName: "Grok Imagine Image",
		Name:        xaiBuiltinImageModelID,
		Description: "xAI Grok image generation model.",
	}
}

// xaiBuiltinImageQualityModelInfo returns the built-in xAI quality image model definition.
func xaiBuiltinImageQualityModelInfo() *ModelInfo {
	return &ModelInfo{
		ID:          xaiBuiltinImageQualityModelID,
		Object:      "model",
		Created:     1735689600, // 2025-01-01
		OwnedBy:     "xai",
		Type:        "xai",
		DisplayName: "Grok Imagine Image Quality",
		Name:        xaiBuiltinImageQualityModelID,
		Description: "xAI Grok higher-fidelity image generation model.",
	}
}

// xaiBuiltinVideoModelInfo returns the built-in xAI video model definition.
func xaiBuiltinVideoModelInfo() *ModelInfo {
	return &ModelInfo{
		ID:          xaiBuiltinVideoModelID,
		Object:      "model",
		Created:     1735689600, // 2025-01-01
		OwnedBy:     "xai",
		Type:        "xai",
		DisplayName: "Grok Imagine Video",
		Name:        xaiBuiltinVideoModelID,
		Description: "xAI Grok video generation model.",
	}
}

// xaiBuiltinVideo15PreviewModelInfo returns the built-in xAI preview video model definition.
func xaiBuiltinVideo15PreviewModelInfo() *ModelInfo {
	return &ModelInfo{
		ID:          xaiBuiltinVideo15PreviewModelID,
		Object:      "model",
		Created:     1735689600, // 2025-01-01
		OwnedBy:     "xai",
		Type:        "xai",
		DisplayName: "Grok Imagine Video 1.5 Preview",
		Name:        xaiBuiltinVideo15PreviewModelID,
		Description: "xAI Grok preview video generation model.",
	}
}

// upsertModelInfos inserts or updates a model infos.
func upsertModelInfos(models []*ModelInfo, extras ...*ModelInfo) []*ModelInfo {
	// Normalize source data before building the derived payload.
	if len(extras) == 0 {
		return models
	}

	extraIDs := make(map[string]struct{}, len(extras))
	extraList := make([]*ModelInfo, 0, len(extras))
	for _, extra := range extras {
		if extra == nil {
			continue
		}
		id := strings.TrimSpace(extra.ID)
		if id == "" {
			continue
		}
		key := strings.ToLower(id)
		if _, exists := extraIDs[key]; exists {
			continue
		}
		extraIDs[key] = struct{}{}
		extraList = append(extraList, cloneModelInfo(extra))
	}

	if len(extraList) == 0 {
		return models
	}

	filtered := make([]*ModelInfo, 0, len(models)+len(extraList))
	for _, model := range models {
		if model == nil {
			continue
		}
		id := strings.TrimSpace(model.ID)
		if id == "" {
			continue
		}
		if _, exists := extraIDs[strings.ToLower(id)]; exists {
			continue
		}
		filtered = append(filtered, model)
	}

	filtered = append(filtered, extraList...)
	return filtered
}

// cloneModelInfos returns a shallow copy of the slice with each element deep-cloned.
func cloneModelInfos(models []*ModelInfo) []*ModelInfo {
	if len(models) == 0 {
		return nil
	}
	out := make([]*ModelInfo, len(models))
	for i, m := range models {
		out[i] = cloneModelInfo(m)
	}
	return out
}

// GetStaticModelDefinitionsByChannel returns static model definitions for a given channel/provider.
// It returns nil when the channel is unknown.
//
// Supported channels:
//   - claude
//   - gemini
//   - gemini-interactions
//   - vertex
//   - codex
//   - kimi
//   - antigravity
//   - xai
//   - devin
//   - meta
func GetStaticModelDefinitionsByChannel(channel string) []*ModelInfo {
	// Normalize source data before building the derived payload.
	key := strings.ToLower(strings.TrimSpace(channel))
	provider := key
	var models []*ModelInfo
	switch key {
	case "claude":
		models = GetClaudeModels()
	case "gemini":
		models = GetGeminiModels()
	case "gemini-interactions":
		models = GetGeminiModels()
	case "vertex":
		models = GetGeminiVertexModels()
	case "codex", "codex-pro":
		models = GetCodexProModels()
		provider = "codex"
	case "codex-plus":
		models = GetCodexPlusModels()
		provider = "codex"
	case "codex-team":
		models = GetCodexTeamModels()
		provider = "codex"
	case "codex-free":
		models = GetCodexFreeModels()
		provider = "codex"
	case "kimi", "kimi-ai":
		models = GetKimiModels()
	case "antigravity":
		models = GetAntigravityModels()
	case "xai", "x-ai", "grok":
		models = GetXAIModels()
		provider = "xai"
	case "devin":
		models = GetDevinModels()
	case "meta", "muse":
		models = GetMetaModels()
		provider = "meta"
	default:
		return nil
	}
	return withStaticModelProviders(models, provider)
}

// LookupStaticModelInfo searches all static model definitions for a model by ID.
// Returns nil if no matching model is found.
func LookupStaticModelInfo(modelID string) *ModelInfo {
	// Normalize source data before building the derived payload.
	if modelID == "" {
		return nil
	}

	data := getModels()
	allModels := [][]*ModelInfo{
		data.Claude,
		data.Gemini,
		data.Vertex,
		data.CodexPro,
		data.Kimi,
		data.Antigravity,
		data.XAI,
		GetDevinModels(),
		GetMetaModels(),
	}
	for _, models := range allModels {
		for _, m := range models {
			if m != nil && m.ID == modelID {
				return cloneModelInfo(m)
			}
		}
	}

	return nil
}
