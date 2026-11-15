package cluster

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	appconfig "github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
)

// CredentialConfigCounts reports how many auth-backed config entries were restored.
type CredentialConfigCounts struct {
	GeminiKeys          int
	InteractionsKeys    int
	VertexKeys          int
	CodexKeys           int
	XAIKeys             int
	MetaKeys            int
	ClaudeKeys          int
	OpenAICompatibility int
}

// ApplyCredentialConfigToRoot restores auth-backed config keys into a config root.
func ApplyCredentialConfigToRoot(root map[string]any, auths []*coreauth.Auth) (CredentialConfigCounts, error) {
	if root == nil {
		return CredentialConfigCounts{}, nil
	}
	result := &CredentialConfigCounts{}
	if errApply := applyCredentialConfigToRoot(root, auths, result); errApply != nil {
		return CredentialConfigCounts{}, errApply
	}
	return *result, nil
}

type credentialModelPair struct {
	Name                       string
	Alias                      string
	DisplayName                string
	ForceMapping               bool
	SupportConfigurationUpdate bool
	Thinking                   *registry.ThinkingSupport
}

type credentialOpenAICompatGroup struct {
	Config      appconfig.OpenAICompatibility
	SeenEntry   map[string]struct{}
	SortKey     string
	FirstAuthID string
}

// applyCredentialConfigToRoot restores auth-backed credential config values.
func applyCredentialConfigToRoot(root map[string]any, auths []*coreauth.Auth, result *CredentialConfigCounts) error {
	// Normalize source data before building the derived payload.
	geminiKeys := make([]appconfig.GeminiKey, 0)
	interactionsKeys := make([]appconfig.GeminiKey, 0)
	vertexKeys := make([]appconfig.VertexCompatKey, 0)
	codexKeys := make([]appconfig.CodexKey, 0)
	xaiKeys := make([]appconfig.XAIKey, 0)
	metaKeys := make([]appconfig.MetaKey, 0)
	claudeKeys := make([]appconfig.ClaudeKey, 0)
	openAICompat := make(map[string]*credentialOpenAICompatGroup)

	for _, auth := range auths {
		switch credentialConfigAuthKind(auth) {
		case "gemini-api-key", "interactions-api-key":
			entry, errGemini := credentialGeminiKey(auth)
			if errGemini != nil {
				return errGemini
			}
			if auth.Provider == "gemini-interactions" {
				interactionsKeys = append(interactionsKeys, entry)
			} else {
				geminiKeys = append(geminiKeys, entry)
			}
		case "vertex-api-key":
			entry, errVertex := credentialVertexKey(auth)
			if errVertex != nil {
				return errVertex
			}
			vertexKeys = append(vertexKeys, entry)
		case "codex-api-key":
			entry, errCodex := credentialCodexKey(auth)
			if errCodex != nil {
				return errCodex
			}
			codexKeys = append(codexKeys, entry)
		case "xai-api-key":
			entry, errXAI := credentialXAIKey(auth)
			if errXAI != nil {
				return errXAI
			}
			xaiKeys = append(xaiKeys, entry)
		case "meta-api-key":
			entry, errMeta := credentialMetaKey(auth)
			if errMeta != nil {
				return errMeta
			}
			metaKeys = append(metaKeys, entry)
		case "claude-api-key":
			entry, errClaude := credentialClaudeKey(auth)
			if errClaude != nil {
				return errClaude
			}
			claudeKeys = append(claudeKeys, entry)
		case "openai-compatibility":
			if errAdd := addOpenAICompatCredential(openAICompat, auth); errAdd != nil {
				return errAdd
			}
		}
	}

	if len(geminiKeys) > 0 {
		root["gemini-api-key"] = geminiKeys
		result.GeminiKeys = len(geminiKeys)
	}
	if len(interactionsKeys) > 0 {
		root["interactions-api-key"] = interactionsKeys
		result.InteractionsKeys = len(interactionsKeys)
	}
	if len(vertexKeys) > 0 {
		root["vertex-api-key"] = vertexKeys
		result.VertexKeys = len(vertexKeys)
	}
	if len(codexKeys) > 0 {
		root["codex-api-key"] = codexKeys
		result.CodexKeys = len(codexKeys)
	}
	if len(xaiKeys) > 0 {
		root["xai-api-key"] = xaiKeys
		result.XAIKeys = len(xaiKeys)
	}
	if len(metaKeys) > 0 {
		root["meta-api-key"] = metaKeys
		result.MetaKeys = len(metaKeys)
	}
	if len(claudeKeys) > 0 {
		root["claude-api-key"] = claudeKeys
		result.ClaudeKeys = len(claudeKeys)
	}
	if len(openAICompat) > 0 {
		groups := make([]*credentialOpenAICompatGroup, 0, len(openAICompat))
		for _, group := range openAICompat {
			groups = append(groups, group)
		}
		sort.Slice(groups, func(i, j int) bool {
			if groups[i].SortKey == groups[j].SortKey {
				return groups[i].FirstAuthID < groups[j].FirstAuthID
			}
			return groups[i].SortKey < groups[j].SortKey
		})
		items := make([]appconfig.OpenAICompatibility, 0, len(groups))
		for _, group := range groups {
			items = append(items, group.Config)
		}
		root["openai-compatibility"] = items
		result.OpenAICompatibility = len(items)
	}
	return nil
}

// credentialConfigAuthKind returns the config key represented by an auth record.
func credentialConfigAuthKind(auth *coreauth.Auth) string {
	if auth == nil || auth.Attributes == nil {
		return ""
	}
	source := strings.TrimSpace(auth.Attributes["source"])
	switch {
	case auth.Provider == "gemini" && strings.HasPrefix(source, "config:gemini["):
		return "gemini-api-key"
	case auth.Provider == "gemini-interactions" && strings.HasPrefix(source, "config:interactions["):
		return "interactions-api-key"
	case auth.Provider == "vertex" && strings.HasPrefix(source, "config:vertex-apikey["):
		return "vertex-api-key"
	case auth.Provider == "codex" && strings.HasPrefix(source, "config:codex["):
		return "codex-api-key"
	case auth.Provider == "xai" && strings.HasPrefix(source, "config:xai["):
		return "xai-api-key"
	case auth.Provider == "meta" && strings.HasPrefix(source, "config:meta["):
		return "meta-api-key"
	case auth.Provider == "claude" && strings.HasPrefix(source, "config:claude["):
		return "claude-api-key"
	case isOpenAICompatConfigAuth(auth):
		return "openai-compatibility"
	default:
		return ""
	}
}

// isOpenAICompatConfigAuth reports whether auth represents an OpenAI-compatible config entry.
func isOpenAICompatConfigAuth(auth *coreauth.Auth) bool {
	if auth == nil || auth.Attributes == nil || auth.Provider == "vertex" {
		return false
	}
	attrs := auth.Attributes
	if strings.TrimSpace(attrs["compat_name"]) != "" {
		return true
	}
	return strings.HasPrefix(strings.TrimSpace(attrs["source"]), "config:") && strings.TrimSpace(attrs["provider_key"]) != ""
}

// credentialGeminiKey builds a Gemini key config from an auth record.
func credentialGeminiKey(auth *coreauth.Auth) (appconfig.GeminiKey, error) {
	return applyCredentialConfigOptions(auth, appconfig.GeminiKey{
		ID:             strings.TrimSpace(auth.ID),
		APIKey:         authAttribute(auth, "api_key"),
		Priority:       credentialPriority(auth),
		Prefix:         strings.TrimSpace(auth.Prefix),
		BaseURL:        authAttribute(auth, "base_url"),
		ProxyURL:       strings.TrimSpace(auth.ProxyURL),
		Models:         credentialGeminiModels(auth),
		Headers:        credentialHeaders(auth),
		ExcludedModels: credentialExcludedModels(auth),
		DisableCooling: credentialDisableCooling(auth),
		RequestRetry:   credentialRequestRetry(auth),
	})
}

// credentialVertexKey builds a Vertex key config from an auth record.
func credentialVertexKey(auth *coreauth.Auth) (appconfig.VertexCompatKey, error) {
	return applyCredentialConfigOptions(auth, appconfig.VertexCompatKey{
		ID:             strings.TrimSpace(auth.ID),
		APIKey:         authAttribute(auth, "api_key"),
		Priority:       credentialPriority(auth),
		Prefix:         strings.TrimSpace(auth.Prefix),
		BaseURL:        authAttribute(auth, "base_url"),
		ProxyURL:       strings.TrimSpace(auth.ProxyURL),
		Models:         credentialVertexModels(auth),
		Headers:        credentialHeaders(auth),
		ExcludedModels: credentialExcludedModels(auth),
		DisableCooling: credentialDisableCooling(auth),
		RequestRetry:   credentialRequestRetry(auth),
	})
}

// credentialCodexKey builds a Codex key config from an auth record.
func credentialCodexKey(auth *coreauth.Auth) (appconfig.CodexKey, error) {
	return applyCredentialConfigOptions(auth, appconfig.CodexKey{
		ID:             strings.TrimSpace(auth.ID),
		APIKey:         authAttribute(auth, "api_key"),
		Priority:       credentialPriority(auth),
		Prefix:         strings.TrimSpace(auth.Prefix),
		BaseURL:        authAttribute(auth, "base_url"),
		Websockets:     strings.EqualFold(authAttribute(auth, "websockets"), "true"),
		AlphaSearch:    strings.EqualFold(authAttribute(auth, coreauth.AttributeCodexAlphaSearch), "true"),
		ProxyURL:       strings.TrimSpace(auth.ProxyURL),
		Models:         credentialCodexModels(auth),
		Headers:        credentialHeaders(auth),
		ExcludedModels: credentialExcludedModels(auth),
		DisableCooling: credentialDisableCooling(auth),
		RequestRetry:   credentialRequestRetry(auth),
	})
}

// credentialXAIKey builds an xAI key config from an auth record.
func credentialXAIKey(auth *coreauth.Auth) (appconfig.XAIKey, error) {
	return applyCredentialConfigOptions(auth, appconfig.XAIKey{
		ID:             strings.TrimSpace(auth.ID),
		APIKey:         authAttribute(auth, "api_key"),
		Priority:       credentialPriority(auth),
		Prefix:         strings.TrimSpace(auth.Prefix),
		BaseURL:        authAttribute(auth, "base_url"),
		Websockets:     strings.EqualFold(authAttribute(auth, "websockets"), "true"),
		ProxyURL:       strings.TrimSpace(auth.ProxyURL),
		Models:         credentialXAIModels(auth),
		Headers:        credentialHeaders(auth),
		ExcludedModels: credentialExcludedModels(auth),
		DisableCooling: credentialDisableCooling(auth),
		RequestRetry:   credentialRequestRetry(auth),
	})
}

// credentialMetaKey builds a Meta key config from an auth record.
func credentialMetaKey(auth *coreauth.Auth) (appconfig.MetaKey, error) {
	return applyCredentialConfigOptions(auth, appconfig.MetaKey{
		ID:             strings.TrimSpace(auth.ID),
		APIKey:         authAttribute(auth, "api_key"),
		Priority:       credentialPriority(auth),
		Prefix:         strings.TrimSpace(auth.Prefix),
		BaseURL:        authAttribute(auth, "base_url"),
		Websockets:     strings.EqualFold(authAttribute(auth, "websockets"), "true"),
		ProxyURL:       strings.TrimSpace(auth.ProxyURL),
		Models:         credentialMetaModels(auth),
		Headers:        credentialHeaders(auth),
		ExcludedModels: credentialExcludedModels(auth),
		DisableCooling: credentialDisableCooling(auth),
		RequestRetry:   credentialRequestRetry(auth),
	})
}

// credentialClaudeKey builds a Claude key config from an auth record.
func credentialClaudeKey(auth *coreauth.Auth) (appconfig.ClaudeKey, error) {
	return applyCredentialConfigOptions(auth, appconfig.ClaudeKey{
		ID:             strings.TrimSpace(auth.ID),
		APIKey:         authAttribute(auth, "api_key"),
		Priority:       credentialPriority(auth),
		Prefix:         strings.TrimSpace(auth.Prefix),
		BaseURL:        authAttribute(auth, "base_url"),
		ProxyURL:       strings.TrimSpace(auth.ProxyURL),
		Models:         credentialClaudeModels(auth),
		Headers:        credentialHeaders(auth),
		ExcludedModels: credentialExcludedModels(auth),
		DisableCooling: credentialDisableCooling(auth),
		RequestRetry:   credentialRequestRetry(auth),
	})
}

// addOpenAICompatCredential adds an OpenAI-compatible auth record to grouped config.
func addOpenAICompatCredential(groups map[string]*credentialOpenAICompatGroup, auth *coreauth.Auth) error {
	// Keep validation before state changes so failures leave existing data intact.
	if auth == nil {
		return nil
	}
	name := authAttribute(auth, "compat_name")
	if name == "" {
		name = strings.TrimSpace(auth.Label)
	}
	if name == "" {
		name = authAttribute(auth, "provider_key")
	}
	baseURL := authAttribute(auth, "base_url")
	prefix := strings.TrimSpace(auth.Prefix)
	priority := credentialPriority(auth)
	headers := credentialHeaders(auth)
	disableCooling := credentialDisableCooling(auth)
	requestRetry := credentialRequestRetry(auth)
	apiKey := authAttribute(auth, "api_key")
	proxyURL := strings.TrimSpace(auth.ProxyURL)
	representation := "nested"
	if apiKey == "" && proxyURL == "" {
		representation = "fallback:" + strings.TrimSpace(auth.ID)
	}
	groupConfig, errGroup := applyCredentialConfigOptions(auth, appconfig.OpenAICompatibility{
		ID:             openAICompatFallbackID(auth),
		Name:           name,
		Priority:       priority,
		Prefix:         prefix,
		BaseURL:        baseURL,
		Models:         credentialOpenAIModels(auth),
		Headers:        headers,
		DisableCooling: disableCooling,
		RequestRetry:   requestRetry,
	})
	if errGroup != nil {
		return errGroup
	}
	// Equal effective options can come from different group defaults and key
	// overrides. Only merge groups whose original defaults also match, or a
	// key with an omitted override can change behavior on the next import.
	groupSource := make(map[string]any, len(groupConfig.RawConfig))
	for key, value := range groupConfig.RawConfig {
		if key != "api-key-entries" {
			groupSource[key] = value
		}
	}
	source, errSource := json.Marshal(groupSource)
	if errSource != nil {
		return fmt.Errorf("encode compatibility source for credential %q: %w", auth.ID, errSource)
	}
	options, errMarshal := json.Marshal(auth.Metadata["credential_options"])
	if errMarshal != nil {
		return fmt.Errorf("encode compatibility options for credential %q: %w", auth.ID, errMarshal)
	}
	groupKey := strings.Join([]string{
		strings.ToLower(name),
		baseURL,
		prefix,
		strconv.Itoa(priority),
		credentialHeadersKey(headers),
		optionalBoolKey(disableCooling),
		optionalIntKey(requestRetry),
		representation,
		string(options),
		string(source),
	}, "\x00")

	group := groups[groupKey]
	if group == nil {
		group = &credentialOpenAICompatGroup{
			Config:      groupConfig,
			SeenEntry:   make(map[string]struct{}),
			SortKey:     groupKey,
			FirstAuthID: strings.TrimSpace(auth.ID),
		}
		groups[groupKey] = group
	}

	if apiKey == "" && proxyURL == "" {
		return nil
	}
	entryKey := strings.TrimSpace(auth.ID) + "\x00" + apiKey + "\x00" + proxyURL
	if _, ok := group.SeenEntry[entryKey]; ok {
		return nil
	}
	entry, errApply := applyCredentialConfigOptions(auth, appconfig.OpenAICompatibilityAPIKey{
		ID:       strings.TrimSpace(auth.ID),
		APIKey:   apiKey,
		ProxyURL: proxyURL,
	})
	if errApply != nil {
		return errApply
	}
	group.SeenEntry[entryKey] = struct{}{}
	group.Config.APIKeyEntries = append(group.Config.APIKeyEntries, entry)
	return nil
}

func openAICompatFallbackID(auth *coreauth.Auth) string {
	if authAttribute(auth, "api_key") != "" || strings.TrimSpace(auth.ProxyURL) != "" {
		return ""
	}
	return strings.TrimSpace(auth.ID)
}

// credentialGeminiModels builds Gemini model config from stored model metadata.
func credentialGeminiModels(auth *coreauth.Auth) []appconfig.GeminiModel {
	pairs := credentialModelPairs(auth)
	out := make([]appconfig.GeminiModel, 0, len(pairs))
	for _, pair := range pairs {
		out = append(out, appconfig.GeminiModel{
			Name:         pair.Name,
			Alias:        pair.Alias,
			DisplayName:  pair.DisplayName,
			ForceMapping: pair.ForceMapping,
		})
	}
	return out
}

// credentialVertexModels builds Vertex model config from stored model metadata.
func credentialVertexModels(auth *coreauth.Auth) []appconfig.VertexCompatModel {
	pairs := credentialModelPairs(auth)
	out := make([]appconfig.VertexCompatModel, 0, len(pairs))
	for _, pair := range pairs {
		out = append(out, appconfig.VertexCompatModel{Name: pair.Name, Alias: pair.Alias})
	}
	return out
}

// credentialCodexModels builds Codex model config from stored model metadata.
func credentialCodexModels(auth *coreauth.Auth) []appconfig.CodexModel {
	pairs := credentialModelPairs(auth)
	out := make([]appconfig.CodexModel, 0, len(pairs))
	for _, pair := range pairs {
		out = append(out, appconfig.CodexModel{
			Name:                       pair.Name,
			Alias:                      pair.Alias,
			DisplayName:                pair.DisplayName,
			ForceMapping:               pair.ForceMapping,
			SupportConfigurationUpdate: pair.SupportConfigurationUpdate,
		})
	}
	return out
}

// credentialXAIModels builds xAI model config from stored model metadata.
func credentialXAIModels(auth *coreauth.Auth) []appconfig.XAIModel {
	pairs := credentialModelPairs(auth)
	out := make([]appconfig.XAIModel, 0, len(pairs))
	for _, pair := range pairs {
		out = append(out, appconfig.XAIModel{
			Name:         pair.Name,
			Alias:        pair.Alias,
			DisplayName:  pair.DisplayName,
			ForceMapping: pair.ForceMapping,
		})
	}
	return out
}

// credentialMetaModels builds Meta model config from stored model metadata.
func credentialMetaModels(auth *coreauth.Auth) []appconfig.MetaModel {
	pairs := credentialModelPairs(auth)
	out := make([]appconfig.MetaModel, 0, len(pairs))
	for _, pair := range pairs {
		out = append(out, appconfig.MetaModel{
			Name:         pair.Name,
			Alias:        pair.Alias,
			DisplayName:  pair.DisplayName,
			ForceMapping: pair.ForceMapping,
		})
	}
	return out
}

// credentialClaudeModels builds Claude model config from stored model metadata.
func credentialClaudeModels(auth *coreauth.Auth) []appconfig.ClaudeModel {
	pairs := credentialModelPairs(auth)
	out := make([]appconfig.ClaudeModel, 0, len(pairs))
	for _, pair := range pairs {
		out = append(out, appconfig.ClaudeModel{Name: pair.Name, Alias: pair.Alias})
	}
	return out
}

// credentialOpenAIModels builds OpenAI-compatible model config from stored model metadata.
func credentialOpenAIModels(auth *coreauth.Auth) []appconfig.OpenAICompatibilityModel {
	pairs := credentialModelPairs(auth)
	out := make([]appconfig.OpenAICompatibilityModel, 0, len(pairs))
	for _, pair := range pairs {
		out = append(out, appconfig.OpenAICompatibilityModel{Name: pair.Name, Alias: pair.Alias, Thinking: pair.Thinking})
	}
	return out
}

// credentialModelPairs returns unique model name/alias pairs from auth metadata.
func credentialModelPairs(auth *coreauth.Auth) []credentialModelPair {
	// Normalize source data before building the derived payload.
	if auth == nil || auth.Metadata == nil {
		return nil
	}
	raw := auth.Metadata["home_config_models"]
	models, ok := raw.([]any)
	if !ok || len(models) == 0 {
		return nil
	}

	out := make([]credentialModelPair, 0, len(models))
	seen := make(map[string]struct{}, len(models))
	for _, rawModel := range models {
		modelMap, okMap := rawModel.(map[string]any)
		if !okMap {
			continue
		}
		alias := strings.TrimSpace(stringFromAny(modelMap["id"]))
		if alias == "" {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") && isNonUserCodexBuiltin(modelMap, alias) {
			continue
		}
		name := strings.TrimSpace(stringFromAny(modelMap["name"]))
		if name == "" {
			name = strings.TrimSpace(stringFromAny(modelMap["display_name"]))
		}
		if name == "" {
			name = alias
		}
		key := strings.ToLower(alias)
		if _, okSeen := seen[key]; okSeen {
			continue
		}
		seen[key] = struct{}{}
		forceMapping, _ := boolFromAny(modelMap["force_mapping"])
		supportConfigurationUpdate, _ := boolFromAny(modelMap["support_configuration_update"])
		out = append(out, credentialModelPair{
			Name:                       name,
			Alias:                      alias,
			DisplayName:                strings.TrimSpace(stringFromAny(modelMap["config_display_name"])),
			ForceMapping:               forceMapping,
			SupportConfigurationUpdate: supportConfigurationUpdate,
			Thinking:                   credentialThinking(modelMap["thinking"]),
		})
	}
	return out
}

// isNonUserCodexBuiltin reports whether alias is a Codex built-in that was not configured by the user.
func isNonUserCodexBuiltin(modelMap map[string]any, alias string) bool {
	if !registry.IsCodexBuiltinModelID(alias) {
		return false
	}
	userDefined, ok := boolFromAny(modelMap["user_defined"])
	return ok && !userDefined
}

// credentialThinking converts stored thinking metadata into config shape.
func credentialThinking(value any) *registry.ThinkingSupport {
	if value == nil {
		return nil
	}
	raw, errMarshal := json.Marshal(value)
	if errMarshal != nil || len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var thinking registry.ThinkingSupport
	if errUnmarshal := json.Unmarshal(raw, &thinking); errUnmarshal != nil {
		return nil
	}
	if thinking.Min == 0 && thinking.Max == 0 && !thinking.ZeroAllowed && !thinking.DynamicAllowed && len(thinking.Levels) == 0 {
		return nil
	}
	return &thinking
}

// credentialHeaders restores custom headers from auth attributes.
func credentialHeaders(auth *coreauth.Auth) map[string]string {
	if auth == nil || len(auth.Attributes) == 0 {
		return nil
	}
	headers := make(map[string]string)
	for key, value := range auth.Attributes {
		if !strings.HasPrefix(key, "header:") {
			continue
		}
		name := strings.TrimSpace(strings.TrimPrefix(key, "header:"))
		value = strings.TrimSpace(value)
		if name == "" || value == "" {
			continue
		}
		headers[name] = value
	}
	if len(headers) == 0 {
		return nil
	}
	return headers
}

// credentialHeadersKey returns a stable grouping key for custom headers.
func credentialHeadersKey(headers map[string]string) string {
	if len(headers) == 0 {
		return ""
	}
	keys := make([]string, 0, len(headers))
	for key := range headers {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+headers[key])
	}
	return strings.Join(parts, "\n")
}

// credentialPriority restores the priority value from auth attributes.
func credentialPriority(auth *coreauth.Auth) int {
	priority := strings.TrimSpace(authAttribute(auth, "priority"))
	if priority == "" {
		return 0
	}
	value, errAtoi := strconv.Atoi(priority)
	if errAtoi != nil {
		return 0
	}
	return value
}

// credentialDisableCooling restores the optional disable-cooling override.
func credentialDisableCooling(auth *coreauth.Auth) *bool {
	return cloneOptionalBool(auth.DisableCoolingOverride())
}

func optionalBoolKey(value *bool) string {
	if value == nil {
		return "inherit"
	}
	return strconv.FormatBool(*value)
}

// credentialRequestRetry restores the optional additional-round override.
func credentialRequestRetry(auth *coreauth.Auth) *int {
	value, ok := auth.RequestRetryOverride()
	if !ok {
		return nil
	}
	return &value
}

func optionalIntKey(value *int) string {
	if value == nil {
		return "inherit"
	}
	return strconv.Itoa(*value)
}

// credentialExcludedModels restores the excluded model list from auth attributes.
func credentialExcludedModels(auth *coreauth.Auth) []string {
	raw := strings.TrimSpace(authAttribute(auth, "excluded_models"))
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, part := range parts {
		model := strings.TrimSpace(part)
		if model == "" {
			continue
		}
		key := strings.ToLower(model)
		if _, okSeen := seen[key]; okSeen {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, model)
	}
	return out
}

// authAttribute handles an auth attribute.
func authAttribute(auth *coreauth.Auth, key string) string {
	if auth == nil || auth.Attributes == nil {
		return ""
	}
	return strings.TrimSpace(auth.Attributes[key])
}

// writeExportAuthFilesExclusive writes auth files without overwriting existing files.
func writeExportAuthFilesExclusive(auths []*coreauth.Auth, authDir string) (int, error) {
	// Normalize auth state before updating runtime indexes.
	authDir = strings.TrimSpace(authDir)
	if authDir == "" {
		return 0, nil
	}
	if errMkdir := os.MkdirAll(authDir, 0o700); errMkdir != nil {
		return 0, errMkdir
	}

	count := 0
	seen := make(map[string]struct{}, len(auths))
	for _, auth := range auths {
		if !isExportOAuthAuth(auth) {
			continue
		}
		relName := exportAuthFileName(auth)
		if relName == "" {
			continue
		}
		fullPath, errPath := exportAuthFilePath(authDir, relName)
		if errPath != nil {
			return count, errPath
		}
		if _, okSeen := seen[fullPath]; okSeen {
			continue
		}
		seen[fullPath] = struct{}{}

		payload := cloneAnyMap(auth.Metadata)
		if strings.TrimSpace(stringFromAny(payload["uuid"])) == "" && strings.TrimSpace(auth.ID) != "" {
			payload["uuid"] = strings.TrimSpace(auth.ID)
		}
		if auth.Disabled || auth.Status == coreauth.StatusDisabled {
			payload["disabled"] = true
		}
		raw, errMarshal := json.MarshalIndent(payload, "", "  ")
		if errMarshal != nil {
			return count, errMarshal
		}
		raw = append(raw, '\n')
		if errWrite := writeExportFileExclusive(fullPath, relName, raw, 0o600); errWrite != nil {
			return count, errWrite
		}
		count++
	}
	return count, nil
}

// isExportOAuthAuth reports whether an auth record should be exported as an auth file.
func isExportOAuthAuth(auth *coreauth.Auth) bool {
	if auth == nil || auth.Metadata == nil {
		return false
	}
	if strings.TrimSpace(stringFromAny(auth.Metadata["type"])) == "" {
		return false
	}
	if auth.Attributes != nil {
		source := strings.TrimSpace(auth.Attributes["source"])
		if strings.HasPrefix(source, "config:") {
			return false
		}
		if strings.EqualFold(strings.TrimSpace(auth.Attributes["runtime_only"]), "true") {
			return false
		}
	}
	if virtual, ok := boolFromAny(auth.Metadata["virtual"]); ok && virtual {
		return false
	}
	return true
}

// exportAuthFileName returns the relative auth file name used for export.
func exportAuthFileName(auth *coreauth.Auth) string {
	// Normalize auth state before updating runtime indexes.
	if auth == nil {
		return ""
	}
	if auth.Metadata != nil {
		if name := strings.TrimSpace(stringFromAny(auth.Metadata["filename"])); name != "" {
			return sanitizeExportRelativePath(name)
		}
	}
	for _, pathValue := range []string{authAttribute(auth, "path"), authAttribute(auth, "source")} {
		if pathValue == "" || strings.HasPrefix(pathValue, "config:") {
			continue
		}
		if baseName := filepath.Base(pathValue); baseName != "." && baseName != string(os.PathSeparator) {
			return sanitizeExportRelativePath(baseName)
		}
	}
	if auth.Metadata != nil {
		if uuidValue := strings.TrimSpace(stringFromAny(auth.Metadata["uuid"])); uuidValue != "" {
			return sanitizeExportRelativePath(uuidValue + ".json")
		}
	}
	if authID := strings.TrimSpace(auth.ID); authID != "" {
		if strings.HasSuffix(strings.ToLower(authID), ".json") {
			return sanitizeExportRelativePath(filepath.Base(authID))
		}
		return sanitizeExportRelativePath(authID + ".json")
	}
	return ""
}

// sanitizeExportRelativePath sanitizes a relative export path.
func sanitizeExportRelativePath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	path = filepath.Clean(path)
	if filepath.IsAbs(path) {
		path = filepath.Base(path)
	}
	for strings.HasPrefix(path, ".."+string(os.PathSeparator)) || path == ".." {
		path = filepath.Base(path)
	}
	if path == "." || path == string(os.PathSeparator) {
		return ""
	}
	return path
}

// exportAuthFilePath resolves an auth file path under the export auth directory.
func exportAuthFilePath(authDir, relName string) (string, error) {
	baseDir := filepath.Clean(authDir)
	fullPath := filepath.Clean(filepath.Join(baseDir, relName))
	relPath, errRel := filepath.Rel(baseDir, fullPath)
	if errRel != nil {
		return "", errRel
	}
	if relPath == ".." || strings.HasPrefix(relPath, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("auth file path escapes auth-dir: %s", relName)
	}
	return fullPath, nil
}

// cloneAnyMap clones an any map.
func cloneAnyMap(in map[string]any) map[string]any {
	if len(in) == 0 {
		return make(map[string]any)
	}
	out := make(map[string]any, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

// stringFromAny derives string from any.
func stringFromAny(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case fmt.Stringer:
		return typed.String()
	case []byte:
		return string(typed)
	default:
		if value == nil {
			return ""
		}
		return fmt.Sprint(value)
	}
}

// boolFromAny derives bool from any.
func boolFromAny(value any) (bool, bool) {
	switch typed := value.(type) {
	case bool:
		return typed, true
	case string:
		parsed, errParseBool := strconv.ParseBool(strings.TrimSpace(typed))
		if errParseBool != nil {
			return false, false
		}
		return parsed, true
	default:
		return false, false
	}
}

// applyCredentialConfigOptions restores options retained with the authoritative
// credential rather than reconstructing them from lossy scheduling projections.
func applyCredentialConfigOptions[T any](auth *coreauth.Auth, value T) (T, error) {
	if auth == nil || auth.Metadata == nil {
		return value, nil
	}
	options := auth.Metadata["credential_options"]
	data, errMarshal := json.Marshal(options)
	if errMarshal != nil {
		return value, fmt.Errorf("encode options for credential %q: %w", auth.ID, errMarshal)
	}
	if errDecode := json.Unmarshal(data, &value); errDecode != nil {
		return value, fmt.Errorf("decode options for credential %q: %w", auth.ID, errDecode)
	}
	var source map[string]any
	if raw := auth.Metadata[appconfig.HomeCredentialConfigMetadataKey]; raw != nil {
		sourceData, errSource := json.Marshal(raw)
		if errSource != nil {
			return value, fmt.Errorf("encode source for credential %q: %w", auth.ID, errSource)
		}
		if errDecode := json.Unmarshal(sourceData, &source); errDecode != nil {
			return value, fmt.Errorf("decode source for credential %q: %w", auth.ID, errDecode)
		}
		if _, keyEntry := any(&value).(*appconfig.OpenAICompatibilityAPIKey); keyEntry {
			keys, ok := source["api-key-entries"].([]any)
			if !ok || len(keys) != 1 {
				return value, fmt.Errorf("source for credential %q must contain exactly one compatibility key entry", auth.ID)
			}
			source, ok = keys[0].(map[string]any)
			if !ok || source == nil {
				return value, fmt.Errorf("source key entry for credential %q must be an object", auth.ID)
			}
		}
	} else if options != nil {
		// Compatibility for records written before raw credential trees existed.
		var errRestore error
		source, errRestore = appconfig.CredentialConfigFromOptions(value, options)
		if errRestore != nil {
			return value, fmt.Errorf("restore legacy options for credential %q: %w", auth.ID, errRestore)
		}
	}
	if source == nil {
		return value, nil
	}
	// Identity, secrets and scheduling remain authoritative on the auth record.
	// Execution fields (including nested models/cloak) stay in the original tree.
	data, errMarshal = json.Marshal(value)
	if errMarshal != nil {
		return value, fmt.Errorf("encode fields for credential %q: %w", auth.ID, errMarshal)
	}
	var owned map[string]any
	if errDecode := json.Unmarshal(data, &owned); errDecode != nil {
		return value, fmt.Errorf("decode fields for credential %q: %w", auth.ID, errDecode)
	}
	for _, key := range appconfig.HomeCredentialFields {
		if field, exists := owned[key]; exists {
			source[key] = field
		} else {
			delete(source, key)
		}
	}
	if holder, ok := any(&value).(interface{ SetConfigTree(map[string]any) }); ok {
		holder.SetConfigTree(source)
	}
	return value, nil
}
