// Package registry provides model definitions and lookup helpers for various AI providers.
// Static model metadata is loaded from the embedded models.json file and can be refreshed from network.
package registry

import (
	"sort"
	"strings"
)

const (
	codexBuiltinImageModelID           = "gpt-image-2"
	codexBuiltinImage25FlareModelID    = "gpt-image-2.5-flare"
	codexBuiltinImage25SunburstModelID = "gpt-image-2.5-sunburst"
	codexBuiltinImage25ModelID         = "gpt-image-2.5"
	xaiBuiltinImageModelID             = "grok-imagine-image"
	xaiBuiltinImageQualityModelID      = "grok-imagine-image-quality"
	xaiBuiltinVideoModelID             = "grok-imagine-video"
	xaiBuiltinVideo15PreviewModelID    = "grok-imagine-video-1.5-preview"
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
	Copilot     []*ModelInfo `json:"copilot"`
}

// GetCopilotModels returns the Copilot presets from the active catalog.
func GetCopilotModels() []*ModelInfo {
	return cloneModelInfos(getModels().Copilot)
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
	return upsertModelInfos(cloneModelInfos(getModels().CodexFree), WithCodexBuiltins(nil)...)
}

// GetCodexTeamModels returns model definitions for the Codex team plan tier.
func GetCodexTeamModels() []*ModelInfo {
	return upsertModelInfos(cloneModelInfos(getModels().CodexTeam), WithCodexBuiltins(nil)...)
}

// GetCodexPlusModels returns model definitions for the Codex plus plan tier.
func GetCodexPlusModels() []*ModelInfo {
	return upsertModelInfos(cloneModelInfos(getModels().CodexPlus), WithCodexBuiltins(nil)...)
}

// GetCodexProModels returns model definitions for the Codex pro plan tier.
func GetCodexProModels() []*ModelInfo {
	return upsertModelInfos(cloneModelInfos(getModels().CodexPro), WithCodexBuiltins(nil)...)
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

// GetMetaModels returns the standard Meta Muse model definitions.
func GetMetaModels() []*ModelInfo {
	return cloneModelInfos(getModels().Meta)
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
		"copilot",
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

// WithCodexBuiltins fills missing Codex built-ins without replacing configured models.
// Static catalog getters separately give built-ins precedence over remote definitions.
func WithCodexBuiltins(models []*ModelInfo) []*ModelInfo {
	return upsertModelInfos([]*ModelInfo{
		codexBuiltinImageModelInfo(codexBuiltinImageModelID, "GPT Image 2"),
		codexBuiltinImageModelInfo(codexBuiltinImage25FlareModelID, "GPT Image 2.5 Flare"),
		codexBuiltinImageModelInfo(codexBuiltinImage25SunburstModelID, "GPT Image 2.5 Sunburst"),
		codexBuiltinImageModelInfo(codexBuiltinImage25ModelID, "GPT Image 2.5"),
	}, models...)
}

// IsCodexBuiltinModelID reports whether id names a hard-coded Codex built-in model.
func IsCodexBuiltinModelID(id string) bool {
	switch strings.ToLower(strings.TrimSpace(id)) {
	case codexBuiltinImageModelID, codexBuiltinImage25FlareModelID, codexBuiltinImage25SunburstModelID, codexBuiltinImage25ModelID:
		return true
	default:
		return false
	}
}

// WithXAIBuiltins injects hard-coded xAI image/video model definitions that should
// not depend on remote models.json updates.
func WithXAIBuiltins(models []*ModelInfo) []*ModelInfo {
	return upsertModelInfos(models, xaiBuiltinImageModelInfo(), xaiBuiltinImageQualityModelInfo(), xaiBuiltinVideoModelInfo(), xaiBuiltinVideo15PreviewModelInfo())
}

// codexBuiltinImageModelInfo returns a built-in Codex image model definition.
func codexBuiltinImageModelInfo(id, displayName string) *ModelInfo {
	return &ModelInfo{
		ID:          id,
		Object:      "model",
		Created:     1704067200, // 2024-01-01
		OwnedBy:     "openai",
		Type:        "openai",
		DisplayName: displayName,
		Version:     id,
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
//   - copilot
//   - meta
func GetStaticModelDefinitionsByChannel(channel string) []*ModelInfo {
	models, provider := staticModelDefinitionsByChannel(strings.ToLower(strings.TrimSpace(channel)))
	if models == nil {
		return nil
	}
	return withStaticModelProviders(cloneModelInfos(models), provider)
}

// staticModelDefinitionsByChannel returns read-only catalog entries and the
// billing provider key for a normalized channel.
func staticModelDefinitionsByChannel(channel string) ([]*ModelInfo, string) {
	data := getModels()
	if data == nil {
		data = &staticModelsJSON{}
	}
	switch channel {
	case "claude":
		return data.Claude, "claude"
	case "gemini", "gemini-interactions":
		return data.Gemini, channel
	case "vertex":
		return data.Vertex, "vertex"
	case "codex", "codex-pro":
		return upsertModelInfos(data.CodexPro, WithCodexBuiltins(nil)...), "codex"
	case "codex-plus":
		return upsertModelInfos(data.CodexPlus, WithCodexBuiltins(nil)...), "codex"
	case "codex-team":
		return upsertModelInfos(data.CodexTeam, WithCodexBuiltins(nil)...), "codex"
	case "codex-free":
		return upsertModelInfos(data.CodexFree, WithCodexBuiltins(nil)...), "codex"
	case "kimi", "kimi-ai":
		return data.Kimi, channel
	case "antigravity":
		return data.Antigravity, "antigravity"
	case "xai", "x-ai", "grok":
		return WithXAIBuiltins(data.XAI), "xai"
	case "copilot":
		return data.Copilot, "copilot"
	case "devin":
		return devinModelDefinitions(), "devin"
	case "meta", "muse":
		return data.Meta, "meta"
	default:
		return nil, ""
	}
}

// LookupStaticModelInfoByChannel searches one provider-specific static section.
// A channel-qualified query can also match a bare catalog ID.
// It does not fall back across providers, so callers can preserve provenance.
func LookupStaticModelInfoByChannel(modelID, channel string) *ModelInfo {
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return nil
	}
	channel = strings.ToLower(strings.TrimSpace(channel))
	models, _ := staticModelDefinitionsByChannel(channel)
	if model := findStaticModel(models, modelID); model != nil {
		return cloneModelInfo(model)
	}
	if prefix, nativeID, qualified := strings.Cut(modelID, "/"); qualified && nativeID != "" && strings.EqualFold(prefix, channel) {
		model := findStaticModel(models, nativeID)
		if lowerID := strings.ToLower(nativeID); model == nil && lowerID != nativeID {
			model = findStaticModel(models, lowerID)
		}
		if model != nil {
			info := cloneModelInfo(model)
			info.ID = channel + "/" + info.ID
			return info
		}
	}
	return nil
}

// LookupStaticModelInfo searches all static model definitions for a model by ID.
// Returns nil if no matching model is found.
func LookupStaticModelInfo(modelID string) *ModelInfo {
	return lookupStaticModelInfo(modelID, devinModelDefinitions())
}

func lookupStaticModelInfo(modelID string, additionalCatalog []*ModelInfo) *ModelInfo {
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return nil
	}
	if channel, _, qualified := strings.Cut(modelID, "/"); qualified {
		if info := LookupStaticModelInfoByChannel(modelID, channel); info != nil {
			return info
		}
	}

	data := getModels()
	if data == nil {
		data = &staticModelsJSON{}
	}
	for _, models := range [][]*ModelInfo{
		data.Claude,
		data.Gemini,
		data.Vertex,
		data.CodexPro,
		data.Kimi,
		data.Antigravity,
		data.XAI,
		data.Meta,
		data.Copilot,
		// Keep the existing lookup order for bare IDs shared with independent catalogs.
		additionalCatalog,
	} {
		if model := findStaticModel(models, modelID); model != nil {
			return cloneModelInfo(model)
		}
	}

	return nil
}

func findStaticModel(models []*ModelInfo, modelID string) *ModelInfo {
	for _, model := range models {
		if model != nil && model.ID == modelID {
			return model
		}
	}
	return nil
}
