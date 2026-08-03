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
	VertexKeys          int
	CodexKeys           int
	XAIKeys             int
	ClaudeKeys          int
	OpenAICompatibility int
}

// ApplyCredentialConfigToRoot restores auth-backed config keys into a config root.
func ApplyCredentialConfigToRoot(root map[string]any, auths []*coreauth.Auth) CredentialConfigCounts {
	if root == nil {
		return CredentialConfigCounts{}
	}
	result := &CredentialConfigCounts{}
	applyCredentialConfigToRoot(root, auths, result)
	return *result
}

type credentialModelPair struct {
	Name         string
	Alias        string
	DisplayName  string
	ForceMapping bool
	Thinking     *registry.ThinkingSupport
}

type credentialOpenAICompatGroup struct {
	Config      appconfig.OpenAICompatibility
	SeenEntry   map[string]struct{}
	SortKey     string
	FirstAuthID string
}

// applyCredentialConfigToRoot restores auth-backed credential config values.
func applyCredentialConfigToRoot(root map[string]any, auths []*coreauth.Auth, result *CredentialConfigCounts) {
	// Normalize source data before building the derived payload.
	geminiKeys := make([]appconfig.GeminiKey, 0)
	vertexKeys := make([]appconfig.VertexCompatKey, 0)
	codexKeys := make([]appconfig.CodexKey, 0)
	xaiKeys := make([]appconfig.XAIKey, 0)
	claudeKeys := make([]appconfig.ClaudeKey, 0)
	openAICompat := make(map[string]*credentialOpenAICompatGroup)

	for _, auth := range auths {
		switch credentialConfigAuthKind(auth) {
		case "gemini-api-key":
			geminiKeys = append(geminiKeys, credentialGeminiKey(auth))
		case "vertex-api-key":
			vertexKeys = append(vertexKeys, credentialVertexKey(auth))
		case "codex-api-key":
			codexKeys = append(codexKeys, credentialCodexKey(auth))
		case "xai-api-key":
			xaiKeys = append(xaiKeys, credentialXAIKey(auth))
		case "claude-api-key":
			claudeKeys = append(claudeKeys, credentialClaudeKey(auth))
		case "openai-compatibility":
			addOpenAICompatCredential(openAICompat, auth)
		}
	}

	if len(geminiKeys) > 0 {
		root["gemini-api-key"] = geminiKeys
		result.GeminiKeys = len(geminiKeys)
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
	case auth.Provider == "vertex" && strings.HasPrefix(source, "config:vertex-apikey["):
		return "vertex-api-key"
	case auth.Provider == "codex" && strings.HasPrefix(source, "config:codex["):
		return "codex-api-key"
	case auth.Provider == "xai" && strings.HasPrefix(source, "config:xai["):
		return "xai-api-key"
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
func credentialGeminiKey(auth *coreauth.Auth) appconfig.GeminiKey {
	return appconfig.GeminiKey{
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
	}
}

// credentialVertexKey builds a Vertex key config from an auth record.
func credentialVertexKey(auth *coreauth.Auth) appconfig.VertexCompatKey {
	return appconfig.VertexCompatKey{
		ID:             strings.TrimSpace(auth.ID),
		APIKey:         authAttribute(auth, "api_key"),
		Priority:       credentialPriority(auth),
		Prefix:         strings.TrimSpace(auth.Prefix),
		BaseURL:        authAttribute(auth, "base_url"),
		ProxyURL:       strings.TrimSpace(auth.ProxyURL),
		Models:         credentialVertexModels(auth),
		Headers:        credentialHeaders(auth),
		ExcludedModels: credentialExcludedModels(auth),
	}
}

// credentialCodexKey builds a Codex key config from an auth record.
func credentialCodexKey(auth *coreauth.Auth) appconfig.CodexKey {
	return appconfig.CodexKey{
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
	}
}

// credentialXAIKey builds an xAI key config from an auth record.
func credentialXAIKey(auth *coreauth.Auth) appconfig.XAIKey {
	return appconfig.XAIKey{
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
	}
}

// credentialClaudeKey builds a Claude key config from an auth record.
func credentialClaudeKey(auth *coreauth.Auth) appconfig.ClaudeKey {
	return appconfig.ClaudeKey{
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
	}
}

// addOpenAICompatCredential adds an OpenAI-compatible auth record to grouped config.
func addOpenAICompatCredential(groups map[string]*credentialOpenAICompatGroup, auth *coreauth.Auth) {
	// Keep validation before state changes so failures leave existing data intact.
	if auth == nil {
		return
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
	apiKey := authAttribute(auth, "api_key")
	proxyURL := strings.TrimSpace(auth.ProxyURL)
	representation := "nested"
	if apiKey == "" && proxyURL == "" {
		representation = "fallback:" + strings.TrimSpace(auth.ID)
	}
	groupKey := strings.Join([]string{
		strings.ToLower(name),
		baseURL,
		prefix,
		strconv.Itoa(priority),
		credentialHeadersKey(headers),
		strconv.FormatBool(disableCooling),
		representation,
	}, "\x00")

	group := groups[groupKey]
	if group == nil {
		group = &credentialOpenAICompatGroup{
			Config: appconfig.OpenAICompatibility{
				ID:             openAICompatFallbackID(auth),
				Name:           name,
				Priority:       priority,
				Prefix:         prefix,
				BaseURL:        baseURL,
				Models:         credentialOpenAIModels(auth),
				Headers:        headers,
				DisableCooling: disableCooling,
			},
			SeenEntry:   make(map[string]struct{}),
			SortKey:     groupKey,
			FirstAuthID: strings.TrimSpace(auth.ID),
		}
		groups[groupKey] = group
	}

	if apiKey == "" && proxyURL == "" {
		return
	}
	entryKey := strings.TrimSpace(auth.ID) + "\x00" + apiKey + "\x00" + proxyURL
	if _, ok := group.SeenEntry[entryKey]; ok {
		return
	}
	group.SeenEntry[entryKey] = struct{}{}
	group.Config.APIKeyEntries = append(group.Config.APIKeyEntries, appconfig.OpenAICompatibilityAPIKey{
		ID:       strings.TrimSpace(auth.ID),
		APIKey:   apiKey,
		ProxyURL: proxyURL,
	})
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
		out = append(out, appconfig.GeminiModel{Name: pair.Name, Alias: pair.Alias})
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
			Name:         pair.Name,
			Alias:        pair.Alias,
			DisplayName:  pair.DisplayName,
			ForceMapping: pair.ForceMapping,
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
		if isNonUserCodexBuiltin(modelMap, alias) {
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
		out = append(out, credentialModelPair{
			Name:         name,
			Alias:        alias,
			DisplayName:  strings.TrimSpace(stringFromAny(modelMap["config_display_name"])),
			ForceMapping: forceMapping,
			Thinking:     credentialThinking(modelMap["thinking"]),
		})
	}
	return out
}

// isNonUserCodexBuiltin reports whether non user codex builtin.
func isNonUserCodexBuiltin(modelMap map[string]any, alias string) bool {
	if !strings.EqualFold(alias, "gpt-image-2") {
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

// credentialDisableCooling restores the disable-cooling value from auth metadata.
func credentialDisableCooling(auth *coreauth.Auth) bool {
	if auth == nil || auth.Metadata == nil {
		return false
	}
	for _, key := range []string{"disable_cooling", "disable-cooling"} {
		if value, ok := boolFromAny(auth.Metadata[key]); ok {
			return value
		}
	}
	return false
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
