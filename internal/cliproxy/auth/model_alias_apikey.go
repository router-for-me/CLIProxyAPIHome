package auth

import (
	"encoding/json"
	"strconv"
	"strings"

	internalconfig "github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
)

const homeConfigModelsMetadataKey = "home_config_models"

type dispatchModelResolution struct {
	Model           string
	Key             string
	ForceMapping    bool
	OriginalAlias   string
	PublishedSource bool
}

// rewriteModelForAuth returns a rewrite model for auth.
func rewriteModelForAuth(model string, auth *Auth) string {
	if auth == nil || model == "" {
		return model
	}
	prefix := strings.TrimSpace(auth.Prefix)
	if prefix == "" {
		return model
	}
	needle := prefix + "/"
	if !strings.HasPrefix(model, needle) {
		return model
	}
	return strings.TrimPrefix(model, needle)
}

// applyAPIKeyModelAlias applies an api key model alias.
func (m *Manager) applyAPIKeyModelAlias(auth *Auth, requestedModel string) string {
	// Normalize source data before building the derived payload.
	if m == nil || auth == nil {
		return requestedModel
	}

	kind, _ := auth.AccountInfo()
	if !strings.EqualFold(strings.TrimSpace(kind), "api_key") {
		return requestedModel
	}

	requestedModel = strings.TrimSpace(requestedModel)
	if requestedModel == "" {
		return requestedModel
	}

	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil {
		cfg = &internalconfig.Config{}
	}

	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	upstreamModel := ""
	switch provider {
	case "gemini":
		upstreamModel = resolveUpstreamModelForGeminiAPIKey(cfg, auth, requestedModel)
	case "gemini-interactions":
		upstreamModel = resolveUpstreamModelForInteractionsAPIKey(cfg, auth, requestedModel)
	case "claude":
		upstreamModel = resolveUpstreamModelForClaudeAPIKey(cfg, auth, requestedModel)
	case "codex":
		upstreamModel = resolveUpstreamModelForCodexAPIKey(cfg, auth, requestedModel)
	case "xai":
		upstreamModel = resolveUpstreamModelForXAIAPIKey(cfg, auth, requestedModel)
	case "meta":
		upstreamModel = resolveUpstreamModelForMetaAPIKey(cfg, auth, requestedModel)
	case "vertex":
		upstreamModel = resolveUpstreamModelForVertexAPIKey(cfg, auth, requestedModel)
	default:
		upstreamModel = resolveUpstreamModelForOpenAICompatAPIKey(cfg, auth, requestedModel)
	}

	if upstreamModel != "" {
		return upstreamModel
	}
	upstreamModel = resolveUpstreamModelFromAuthConfigModels(auth, requestedModel)
	if upstreamModel != "" {
		return upstreamModel
	}
	return requestedModel
}

// ResolveExecutionModel returns the auth-specific upstream model that dispatch
// would send for a public route model, or the trimmed route model when no
// alias applies.
func (m *Manager) ResolveExecutionModel(auth *Auth, routeModel string) string {
	if resolved := strings.TrimSpace(m.resolveDispatchModel(auth, routeModel).Model); resolved != "" {
		return resolved
	}
	return strings.TrimSpace(routeModel)
}

// ResolveBillingModel returns the canonical execution ID and whether dispatch
// resolved it from a source recorded on the published route.
func (m *Manager) ResolveBillingModel(auth *Auth, routeModel string) (model string, publishedSource bool) {
	resolved := m.resolveDispatchModel(auth, routeModel)
	return resolved.Key, resolved.PublishedSource
}

func isCopilotAuth(auth *Auth) bool {
	return auth != nil && strings.EqualFold(strings.TrimSpace(auth.Provider), "copilot")
}

func usesPublishedModelRoutes(auth *Auth) bool {
	if auth == nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(auth.Provider)) {
	case "copilot", "devin":
		return true
	default:
		return false
	}
}

func publishedSourceModelID(sourceID, metadataID string, legacyCopilot bool) string {
	// Older Copilot registrations stored the source in the metadata field.
	if sourceID == "" && legacyCopilot {
		if metadataID = strings.TrimSpace(metadataID); metadataID != "" {
			return "copilot/" + strings.TrimPrefix(metadataID, "copilot/")
		}
	}
	return strings.TrimSpace(sourceID)
}

func publishedModelRouteForAuth(reg *registry.ModelRegistry, auth *Auth, routeModel string) (*registry.ModelInfo, suffixResult) {
	suffix := parseModelSuffix(routeModel)
	route := reg.GetModelInfoForClient(auth.ID, suffix.ModelName)
	if route == nil {
		if trimmed := strings.TrimSpace(routeModel); trimmed != "" && trimmed != suffix.ModelName {
			route = reg.GetModelInfoForClient(auth.ID, trimmed)
		}
	}
	return route, suffix
}

func isPublishedSourceModelKey(auth *Auth, resolvedModel string) bool {
	if !usesPublishedModelRoutes(auth) {
		return false
	}
	prefix := strings.ToLower(strings.TrimSpace(auth.Provider)) + "/"
	return strings.HasPrefix(canonicalModelKey(resolvedModel), prefix)
}

func publishedNativeSourceModelKey(auth *Auth, model string) (string, bool) {
	if !isPublishedSourceModelKey(auth, model) {
		return "", false
	}
	canonical := canonicalModelKey(model)
	prefix := strings.ToLower(strings.TrimSpace(auth.Provider)) + "/"
	native, qualified := strings.CutPrefix(canonical, prefix)
	if !qualified || native == "" {
		return "", false
	}
	return native, true
}

// publishedModelStateKey returns the active ModelStates key for a published
// source model, falling back to an unqualified legacy key when newer or
// alone. Unqualified public aliases must never be matched here.
func publishedModelStateKey(auth *Auth, model string) string {
	native, ok := publishedNativeSourceModelKey(auth, model)
	if !ok {
		return model
	}
	canonical := canonicalModelKey(model)
	legacy := auth.ModelStates[native]
	if legacy == nil {
		return model
	}
	current := auth.ModelStates[canonical]
	if current == nil {
		return native
	}
	if !legacy.LastObservedAt.IsZero() && !current.LastObservedAt.IsZero() {
		if legacy.LastObservedAt.After(current.LastObservedAt) {
			return native
		}
	} else if legacy.UpdatedAt.After(current.UpdatedAt) {
		return native
	}
	return model
}

// migratePublishedModelState consolidates a legacy unqualified source state
// into its canonical provider-qualified source key before applying feedback.
// The caller must own the auth snapshot or hold the manager's write lock.
func migratePublishedModelState(auth *Auth, model string) bool {
	native, ok := publishedNativeSourceModelKey(auth, model)
	if !ok {
		return false
	}
	canonical := canonicalModelKey(model)
	if _, exists := auth.ModelStates[native]; !exists {
		return false
	}
	if publishedModelStateKey(auth, canonical) == native {
		if auth.ModelStates == nil {
			auth.ModelStates = make(map[string]*ModelState)
		}
		auth.ModelStates[canonical] = auth.ModelStates[native]
	}
	delete(auth.ModelStates, native)
	return true
}

// publishedResultModelKey normalizes bare native execution models reported by
// older nodes to the published canonical source key when known from the
// credential's published routes or existing canonical state.
func publishedResultModelKey(reg *registry.ModelRegistry, auth *Auth, model string) string {
	canonical := canonicalModelKey(model)
	if canonical == "" || !usesPublishedModelRoutes(auth) {
		return canonical
	}
	prefix := strings.ToLower(strings.TrimSpace(auth.Provider)) + "/"
	if strings.HasPrefix(canonical, prefix) {
		return canonical
	}
	if reg != nil && auth.ID != "" {
		legacyCopilot := isCopilotAuth(auth)
		for _, info := range reg.GetModelsForClient(auth.ID) {
			if info == nil {
				continue
			}
			sourceID := canonicalModelKey(publishedSourceModelID(info.SourceModelID, info.MetadataModelID, legacyCopilot))
			if sourceID == "" {
				continue
			}
			if native, qualified := strings.CutPrefix(sourceID, prefix); qualified && native == canonical {
				return sourceID
			}
			if info.SourceModelID != "" && canonicalModelKey(info.MetadataModelID) == canonical {
				return sourceID
			}
		}
	}
	if auth.ModelStates[prefix+canonical] != nil {
		return prefix + canonical
	}
	return canonical
}

// resolveDispatchModel resolves the auth-specific upstream model used for execution and runtime state.
func (m *Manager) resolveDispatchModel(auth *Auth, routeModel string) dispatchModelResolution {
	if usesPublishedModelRoutes(auth) {
		// Publication resolves collisions per account and keeps the response alias across prefixing.
		// Base-name lookup takes precedence over literal suffixed aliases.
		route, suffix := publishedModelRouteForAuth(registry.GetGlobalRegistry(), auth, routeModel)
		if route != nil {
			if sourceID := publishedSourceModelID(route.SourceModelID, route.MetadataModelID, isCopilotAuth(auth)); sourceID != "" {
				return dispatchModelResolution{
					Model:           preserveResolvedModelSuffix(sourceID, suffix),
					Key:             canonicalModelKey(sourceID),
					ForceMapping:    route.ResponseModelAlias != "",
					OriginalAlias:   route.ResponseModelAlias,
					PublishedSource: true,
				}
			}
		}
	}
	requestedModel := rewriteModelForAuth(routeModel, auth)
	oauthAlias := m.resolveOAuthModelAlias(auth, requestedModel)
	if strings.TrimSpace(oauthAlias.upstreamModel) != "" {
		requestedModel = oauthAlias.upstreamModel
	}
	resolved := m.applyAPIKeyModelAlias(auth, requestedModel)
	if strings.TrimSpace(resolved) == "" {
		resolved = requestedModel
	}
	if strings.TrimSpace(resolved) == "" {
		resolved = strings.TrimSpace(routeModel)
	}
	resolved = strings.TrimSpace(resolved)
	if resolved == "" {
		return dispatchModelResolution{}
	}
	forceMapping, originalAlias := oauthAlias.forceMapping, oauthAlias.originalAlias
	if !forceMapping {
		forceMapping, originalAlias = forceMappingFromAuthConfigModels(auth, requestedModel)
	}
	return dispatchModelResolution{
		Model:         resolved,
		Key:           canonicalModelKey(resolved),
		ForceMapping:  forceMapping,
		OriginalAlias: originalAlias,
	}
}

type metadataModelAliasEntry struct {
	name         string
	alias        string
	forceMapping bool
}

// GetName returns the upstream model name.
func (m metadataModelAliasEntry) GetName() string { return m.name }

// GetAlias returns the client-visible model alias.
func (m metadataModelAliasEntry) GetAlias() string { return m.alias }

// GetForceMapping returns whether response model fields should be rewritten.
func (m metadataModelAliasEntry) GetForceMapping() bool { return m.forceMapping }

// resolveUpstreamModelFromAuthConfigModels resolves alias metadata stored on cluster auth records.
func resolveUpstreamModelFromAuthConfigModels(auth *Auth, requestedModel string) string {
	if auth == nil || auth.Metadata == nil {
		return ""
	}
	raw := auth.Metadata[homeConfigModelsMetadataKey]
	entries := modelAliasEntriesFromMetadata(raw)
	if len(entries) == 0 {
		return ""
	}
	return resolveModelAliasFromConfigModels(requestedModel, entries)
}

// modelAliasEntriesFromMetadata converts Home model metadata into alias entries.
func modelAliasEntriesFromMetadata(raw any) []modelAliasEntry {
	// Publication can reuse decoded aliases without changing stored metadata.
	if entries, ok := raw.([]modelAliasEntry); ok {
		return entries
	}
	if raw == nil {
		return nil
	}
	data, errMarshal := json.Marshal(raw)
	if errMarshal != nil || len(data) == 0 || string(data) == "null" {
		return nil
	}
	var items []map[string]any
	if errUnmarshal := json.Unmarshal(data, &items); errUnmarshal != nil {
		return nil
	}
	out := make([]modelAliasEntry, 0, len(items))
	for _, item := range items {
		alias := strings.TrimSpace(metadataString(item["id"]))
		name := strings.TrimSpace(metadataString(item["name"]))
		if name == "" && metadataBool(item["user_defined"]) {
			// Older configured models stored the upstream name in display_name.
			name = strings.TrimSpace(metadataString(item["display_name"]))
		}
		if alias == "" || name == "" {
			continue
		}
		out = append(out, metadataModelAliasEntry{
			name:         name,
			alias:        alias,
			forceMapping: metadataBool(item["force_mapping"]),
		})
	}
	return out
}

func forceMappingFromAuthConfigModels(auth *Auth, requestedModel string) (bool, string) {
	if auth == nil {
		return false, ""
	}
	kind, _ := auth.AccountInfo()
	if !strings.EqualFold(strings.TrimSpace(kind), "api_key") || auth.Metadata == nil {
		return false, ""
	}
	entries := modelAliasEntriesFromMetadata(auth.Metadata[homeConfigModelsMetadataKey])
	if len(entries) == 0 {
		return false, ""
	}
	_, candidates := modelAliasLookupCandidates(requestedModel)
	for _, entry := range entries {
		metadataEntry, okMetadata := entry.(metadataModelAliasEntry)
		if !okMetadata || !metadataEntry.forceMapping {
			continue
		}
		alias := strings.TrimSpace(metadataEntry.alias)
		for _, candidate := range candidates {
			if alias != "" && strings.EqualFold(alias, strings.TrimSpace(candidate)) {
				return true, alias
			}
		}
	}
	return false, ""
}

// metadataString derives a string from metadata values.
func metadataString(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case []byte:
		return string(typed)
	default:
		return ""
	}
}

// metadataBool derives a bool from metadata values.
func metadataBool(value any) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	case string:
		return strings.EqualFold(strings.TrimSpace(typed), "true") || strings.TrimSpace(typed) == "1"
	default:
		return false
	}
}

type apiKeyConfigEntry interface {
	GetAPIKey() string
	GetBaseURL() string
	GetPrefix() string
	GetProxyURL() string
}

// resolveAPIKeyConfig resolves an api key config.
func resolveAPIKeyConfig[T apiKeyConfigEntry](entries []T, auth *Auth) *T {
	// Normalize source data before building the derived payload.
	if auth == nil || len(entries) == 0 {
		return nil
	}
	attrKey, attrBase := "", ""
	if auth.Attributes != nil {
		attrKey = strings.TrimSpace(auth.Attributes["api_key"])
		attrBase = strings.TrimSpace(auth.Attributes["base_url"])
	}
	matchesCredentials := func(entry T) bool {
		cfgKey := strings.TrimSpace(entry.GetAPIKey())
		cfgBase := strings.TrimSpace(entry.GetBaseURL())
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
		if matchesCredentials(entry) && strings.EqualFold(strings.TrimSpace(entry.GetPrefix()), strings.TrimSpace(auth.Prefix)) && strings.EqualFold(strings.TrimSpace(entry.GetProxyURL()), strings.TrimSpace(auth.ProxyURL)) {
			return &entries[i]
		}
	}
	for i := range entries {
		if matchesCredentials(entries[i]) {
			return &entries[i]
		}
	}
	if attrKey != "" {
		for i := range entries {
			if strings.EqualFold(strings.TrimSpace(entries[i].GetAPIKey()), attrKey) {
				return &entries[i]
			}
		}
	}
	return nil
}

// resolveGeminiAPIKeyConfig resolves a gemini api key config.
func resolveGeminiAPIKeyConfig(cfg *internalconfig.Config, auth *Auth) *internalconfig.GeminiKey {
	if cfg == nil {
		return nil
	}
	return resolveAPIKeyConfig(cfg.GeminiKey, auth)
}

// resolveInteractionsAPIKeyConfig resolves a native Interactions api key config.
func resolveInteractionsAPIKeyConfig(cfg *internalconfig.Config, auth *Auth) *internalconfig.GeminiKey {
	if cfg == nil {
		return nil
	}
	return resolveAPIKeyConfig(cfg.InteractionsKey, auth)
}

// resolveClaudeAPIKeyConfig resolves a claude api key config.
func resolveClaudeAPIKeyConfig(cfg *internalconfig.Config, auth *Auth) *internalconfig.ClaudeKey {
	if cfg == nil {
		return nil
	}
	return resolveAPIKeyConfig(cfg.ClaudeKey, auth)
}

// resolveCodexAPIKeyConfig resolves a codex api key config.
func resolveCodexAPIKeyConfig(cfg *internalconfig.Config, auth *Auth) *internalconfig.CodexKey {
	if cfg == nil {
		return nil
	}
	return resolveAPIKeyConfig(cfg.CodexKey, auth)
}

// resolveXAIAPIKeyConfig resolves an xAI api key config.
func resolveXAIAPIKeyConfig(cfg *internalconfig.Config, auth *Auth) *internalconfig.XAIKey {
	if cfg == nil {
		return nil
	}
	return resolveAPIKeyConfig(cfg.XAIKey, auth)
}

// resolveMetaAPIKeyConfig resolves a Meta api key config.
func resolveMetaAPIKeyConfig(cfg *internalconfig.Config, auth *Auth) *internalconfig.MetaKey {
	if cfg == nil {
		return nil
	}
	return resolveAPIKeyConfig(cfg.MetaKey, auth)
}

// resolveVertexAPIKeyConfig resolves a vertex api key config.
func resolveVertexAPIKeyConfig(cfg *internalconfig.Config, auth *Auth) *internalconfig.VertexCompatKey {
	if cfg == nil {
		return nil
	}
	return resolveAPIKeyConfig(cfg.VertexCompatAPIKey, auth)
}

// resolveUpstreamModelForGeminiAPIKey resolves an upstream model for gemini api key.
func resolveUpstreamModelForGeminiAPIKey(cfg *internalconfig.Config, auth *Auth, requestedModel string) string {
	entry := resolveGeminiAPIKeyConfig(cfg, auth)
	if entry == nil {
		return ""
	}
	return resolveModelAliasFromConfigModels(requestedModel, asModelAliasEntries(entry.Models))
}

// resolveUpstreamModelForInteractionsAPIKey resolves an upstream model for a native Interactions api key.
func resolveUpstreamModelForInteractionsAPIKey(cfg *internalconfig.Config, auth *Auth, requestedModel string) string {
	entry := resolveInteractionsAPIKeyConfig(cfg, auth)
	if entry == nil {
		return ""
	}
	return resolveModelAliasFromConfigModels(requestedModel, asModelAliasEntries(entry.Models))
}

// resolveUpstreamModelForClaudeAPIKey resolves an upstream model for claude api key.
func resolveUpstreamModelForClaudeAPIKey(cfg *internalconfig.Config, auth *Auth, requestedModel string) string {
	entry := resolveClaudeAPIKeyConfig(cfg, auth)
	if entry == nil {
		return ""
	}
	return resolveModelAliasFromConfigModels(requestedModel, asModelAliasEntries(entry.Models))
}

// resolveUpstreamModelForCodexAPIKey resolves an upstream model for codex api key.
func resolveUpstreamModelForCodexAPIKey(cfg *internalconfig.Config, auth *Auth, requestedModel string) string {
	entry := resolveCodexAPIKeyConfig(cfg, auth)
	if entry == nil {
		return ""
	}
	return resolveModelAliasFromConfigModels(requestedModel, asModelAliasEntries(entry.Models))
}

// resolveUpstreamModelForXAIAPIKey resolves an upstream model for xAI api key.
func resolveUpstreamModelForXAIAPIKey(cfg *internalconfig.Config, auth *Auth, requestedModel string) string {
	entry := resolveXAIAPIKeyConfig(cfg, auth)
	if entry == nil {
		return ""
	}
	return resolveModelAliasFromConfigModels(requestedModel, asModelAliasEntries(entry.Models))
}

// resolveUpstreamModelForMetaAPIKey resolves an upstream model for Meta api key.
func resolveUpstreamModelForMetaAPIKey(cfg *internalconfig.Config, auth *Auth, requestedModel string) string {
	entry := resolveMetaAPIKeyConfig(cfg, auth)
	if entry == nil {
		return ""
	}
	return resolveModelAliasFromConfigModels(requestedModel, asModelAliasEntries(entry.Models))
}

// resolveUpstreamModelForVertexAPIKey resolves an upstream model for vertex api key.
func resolveUpstreamModelForVertexAPIKey(cfg *internalconfig.Config, auth *Auth, requestedModel string) string {
	entry := resolveVertexAPIKeyConfig(cfg, auth)
	if entry == nil {
		return ""
	}
	return resolveModelAliasFromConfigModels(requestedModel, asModelAliasEntries(entry.Models))
}

// resolveUpstreamModelForOpenAICompatAPIKey resolves an upstream model for open ai compat api key.
func resolveUpstreamModelForOpenAICompatAPIKey(cfg *internalconfig.Config, auth *Auth, requestedModel string) string {
	providerKey := ""
	compatName := ""
	if auth != nil && len(auth.Attributes) > 0 {
		providerKey = strings.TrimSpace(auth.Attributes["provider_key"])
		compatName = strings.TrimSpace(auth.Attributes["compat_name"])
	}
	if compatName == "" && !strings.EqualFold(strings.TrimSpace(auth.Provider), "openai-compatibility") {
		return ""
	}
	entry := resolveOpenAICompatConfig(cfg, providerKey, compatName, auth.Provider)
	if entry == nil {
		return ""
	}
	return resolveModelAliasFromConfigModels(requestedModel, asModelAliasEntries(entry.Models))
}

// resolveOpenAICompatConfig resolves an open ai compat config.
func resolveOpenAICompatConfig(cfg *internalconfig.Config, providerKey, compatName, authProvider string) *internalconfig.OpenAICompatibility {
	// Normalize source data before building the derived payload.
	if cfg == nil {
		return nil
	}
	candidates := make([]string, 0, 3)
	if v := strings.TrimSpace(compatName); v != "" {
		candidates = append(candidates, v)
	}
	if v := strings.TrimSpace(providerKey); v != "" {
		candidates = append(candidates, v)
	}
	if v := strings.TrimSpace(authProvider); v != "" {
		candidates = append(candidates, v)
	}
	for i := range cfg.OpenAICompatibility {
		compat := &cfg.OpenAICompatibility[i]
		if compat.Disabled {
			continue
		}
		for _, candidate := range candidates {
			if candidate != "" && strings.EqualFold(strings.TrimSpace(candidate), compat.Name) {
				return compat
			}
		}
	}
	return nil
}

// asModelAliasEntries handles an as model alias entries.
func asModelAliasEntries[T interface {
	GetName() string
	GetAlias() string
}](models []T) []modelAliasEntry {
	if len(models) == 0 {
		return nil
	}
	out := make([]modelAliasEntry, 0, len(models))
	for i := range models {
		out = append(out, models[i])
	}
	return out
}
