package management

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	appconfig "github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/watcher/synthesizer"
)

// GetGeminiKeys returns a gemini keys.
func (h *Handler) GetGeminiKeys(c *gin.Context) { h.getAPIKeyList(c, "gemini-api-key") }

// PutGeminiKeys replaces a gemini keys.
func (h *Handler) PutGeminiKeys(c *gin.Context) { h.putAPIKeyList(c, "gemini-api-key") }

// PatchGeminiKey applies a partial update to a gemini key.
func (h *Handler) PatchGeminiKey(c *gin.Context) { h.patchAPIKey(c, "gemini-api-key") }

// DeleteGeminiKey deletes a gemini key.
func (h *Handler) DeleteGeminiKey(c *gin.Context) { h.deleteAPIKey(c, "gemini-api-key") }

// GetVertexCompatKeys returns a vertex compat keys.
func (h *Handler) GetVertexCompatKeys(c *gin.Context) { h.getAPIKeyList(c, "vertex-api-key") }

// PutVertexCompatKeys replaces a vertex compat keys.
func (h *Handler) PutVertexCompatKeys(c *gin.Context) { h.putAPIKeyList(c, "vertex-api-key") }

// PatchVertexCompatKey applies a partial update to a vertex compat key.
func (h *Handler) PatchVertexCompatKey(c *gin.Context) { h.patchAPIKey(c, "vertex-api-key") }

// DeleteVertexCompatKey deletes a vertex compat key.
func (h *Handler) DeleteVertexCompatKey(c *gin.Context) { h.deleteAPIKey(c, "vertex-api-key") }

// GetCodexKeys returns a codex keys.
func (h *Handler) GetCodexKeys(c *gin.Context) { h.getAPIKeyList(c, "codex-api-key") }

// PutCodexKeys replaces a codex keys.
func (h *Handler) PutCodexKeys(c *gin.Context) { h.putAPIKeyList(c, "codex-api-key") }

// PatchCodexKey applies a partial update to a codex key.
func (h *Handler) PatchCodexKey(c *gin.Context) { h.patchAPIKey(c, "codex-api-key") }

// DeleteCodexKey deletes a codex key.
func (h *Handler) DeleteCodexKey(c *gin.Context) { h.deleteAPIKey(c, "codex-api-key") }

// GetXAIKeys returns xAI keys.
func (h *Handler) GetXAIKeys(c *gin.Context) { h.getAPIKeyList(c, "xai-api-key") }

// PutXAIKeys replaces xAI keys.
func (h *Handler) PutXAIKeys(c *gin.Context) { h.putAPIKeyList(c, "xai-api-key") }

// PatchXAIKey applies a partial update to an xAI key.
func (h *Handler) PatchXAIKey(c *gin.Context) { h.patchAPIKey(c, "xai-api-key") }

// DeleteXAIKey deletes an xAI key.
func (h *Handler) DeleteXAIKey(c *gin.Context) { h.deleteAPIKey(c, "xai-api-key") }

// GetClaudeKeys returns a claude keys.
func (h *Handler) GetClaudeKeys(c *gin.Context) { h.getAPIKeyList(c, "claude-api-key") }

// PutClaudeKeys replaces a claude keys.
func (h *Handler) PutClaudeKeys(c *gin.Context) { h.putAPIKeyList(c, "claude-api-key") }

// PatchClaudeKey applies a partial update to a claude key.
func (h *Handler) PatchClaudeKey(c *gin.Context) { h.patchAPIKey(c, "claude-api-key") }

// DeleteClaudeKey deletes a claude key.
func (h *Handler) DeleteClaudeKey(c *gin.Context) { h.deleteAPIKey(c, "claude-api-key") }

// GetOpenAICompat returns an open ai compat.
func (h *Handler) GetOpenAICompat(c *gin.Context) { h.getAPIKeyList(c, "openai-compatibility") }

// PutOpenAICompat replaces an open ai compat.
func (h *Handler) PutOpenAICompat(c *gin.Context) { h.putAPIKeyList(c, "openai-compatibility") }

// PatchOpenAICompat applies a partial update to an open ai compat.
func (h *Handler) PatchOpenAICompat(c *gin.Context) { h.patchAPIKey(c, "openai-compatibility") }

// DeleteOpenAICompat deletes an open ai compat.
func (h *Handler) DeleteOpenAICompat(c *gin.Context) { h.deleteAPIKey(c, "openai-compatibility") }

// getAPIKeyList returns an api key list.
func (h *Handler) getAPIKeyList(c *gin.Context, key string) {
	ctx, cancel := h.requestContext(c)
	defer cancel()
	auths, errAuths := h.repo.ListAuths(ctx)
	if errAuths != nil {
		respondError(c, http.StatusInternalServerError, "auth_load_failed", errAuths)
		return
	}
	items := make([]map[string]any, 0)
	for _, auth := range auths {
		if !isAPIKeyAuthForKey(auth, key) {
			continue
		}
		items = append(items, apiKeyAuthToMap(auth, key))
	}
	sort.Slice(items, func(i, j int) bool {
		return fmt.Sprint(items[i]["auth_index"]) < fmt.Sprint(items[j]["auth_index"])
	})
	c.JSON(http.StatusOK, gin.H{key: items})
}

// putAPIKeyList replaces an api key list.
func (h *Handler) putAPIKeyList(c *gin.Context, key string) {
	// Validate request inputs before mutating persisted state.
	body, errRead := io.ReadAll(c.Request.Body)
	if errRead != nil {
		respondError(c, http.StatusBadRequest, "invalid body", errRead)
		return
	}
	auths, errSynthesize := h.synthesizeAPIKeyBody(key, body)
	if errSynthesize != nil {
		respondError(c, http.StatusBadRequest, "invalid body", errSynthesize)
		return
	}

	ctx, cancel := h.requestContext(c)
	defer cancel()
	if errReplace := h.replaceAPIKeyAuths(ctx, key, auths); errReplace != nil {
		if errors.Is(errReplace, cluster.ErrCredentialValidation) {
			respondError(c, http.StatusBadRequest, "invalid body", errReplace)
			return
		}
		if errors.Is(errReplace, cluster.ErrCredentialIdentityConflict) {
			respondError(c, http.StatusConflict, "credential_identity_conflict", errReplace)
			return
		}
		respondError(c, http.StatusInternalServerError, "write_failed", errReplace)
		return
	}
	if errRefresh := h.refreshAuths(ctx); errRefresh != nil {
		respondError(c, http.StatusInternalServerError, "reload_failed", errRefresh)
		return
	}
	respondOK(c)
}

// patchAPIKey applies a partial update to an api key.
func (h *Handler) patchAPIKey(c *gin.Context, key string) {
	// Validate request inputs before mutating persisted state.
	body, errRead := io.ReadAll(c.Request.Body)
	if errRead != nil {
		respondError(c, http.StatusBadRequest, "invalid body", errRead)
		return
	}
	var patch apiKeyPatchRequest
	if errUnmarshal := json.Unmarshal(body, &patch); errUnmarshal != nil {
		respondError(c, http.StatusBadRequest, "invalid body", errUnmarshal)
		return
	}
	if patch.Value == nil {
		respondError(c, http.StatusBadRequest, "invalid body", fmt.Errorf("missing value"))
		return
	}

	ctx, cancel := h.requestContext(c)
	defer cancel()
	auths, errAuths := h.repo.ListAuths(ctx)
	if errAuths != nil {
		respondError(c, http.StatusInternalServerError, "auth_load_failed", errAuths)
		return
	}
	target, ambiguous := findAPIKeyAuth(auths, key, patch.Identifier(c))
	if ambiguous {
		respondError(c, http.StatusBadRequest, "multiple items match api-key; base-url is required", nil)
		return
	}
	if target == nil {
		respondError(c, http.StatusNotFound, "item not found", nil)
		return
	}
	entry := apiKeyAuthToMap(target, key)
	for patchKey, value := range patch.Value {
		entry[patchKey] = value
	}
	delete(entry, "auth_index")
	if key == "openai-compatibility" {
		setOpenAICompatibilityCredentialID(entry, target.ID)
	} else {
		entry["id"] = target.ID
		delete(entry, "uuid")
	}

	rawEntry, errMarshal := json.Marshal(entry)
	if errMarshal != nil {
		respondError(c, http.StatusBadRequest, "invalid body", errMarshal)
		return
	}
	nextAuths, errSynthesize := h.synthesizeAPIKeyBody(key, rawEntry)
	if errSynthesize != nil {
		respondError(c, http.StatusBadRequest, "invalid body", errSynthesize)
		return
	}
	if len(nextAuths) == 0 {
		if errDelete := h.repo.RetireProviderAuth(ctx, target.ID); errDelete != nil {
			respondError(c, http.StatusInternalServerError, "write_failed", errDelete)
			return
		}
	} else {
		next := nextAuths[0]
		preserveAPIKeyAuthDisabledState(next, target)
		if next.ID != target.ID {
			respondError(c, http.StatusBadRequest, "invalid body", fmt.Errorf("credential id cannot change"))
			return
		}
		if next.Provider != target.Provider {
			respondError(c, http.StatusBadRequest, "invalid body", fmt.Errorf("credential provider cannot change"))
			return
		}
		if _, errUpsert := h.repo.UpsertAuthPreservingDisabled(ctx, next, "update"); errUpsert != nil {
			respondError(c, http.StatusInternalServerError, "write_failed", errUpsert)
			return
		}
	}
	if errRefresh := h.refreshAuths(ctx); errRefresh != nil {
		respondError(c, http.StatusInternalServerError, "reload_failed", errRefresh)
		return
	}
	respondOK(c)
}

// deleteAPIKey deletes an api key.
func (h *Handler) deleteAPIKey(c *gin.Context, key string) {
	// Validate request inputs before mutating persisted state.
	ctx, cancel := h.requestContext(c)
	defer cancel()
	auths, errAuths := h.repo.ListAuths(ctx)
	if errAuths != nil {
		respondError(c, http.StatusInternalServerError, "auth_load_failed", errAuths)
		return
	}
	bodyID := apiKeyIdentifierFromRequest(c)
	target, ambiguous := findAPIKeyAuth(auths, key, bodyID)
	if ambiguous {
		respondError(c, http.StatusBadRequest, "multiple items match api-key; base-url is required", nil)
		return
	}
	if target == nil {
		respondError(c, http.StatusNotFound, "item not found", nil)
		return
	}
	if errDelete := h.repo.RetireProviderAuth(ctx, target.ID); errDelete != nil {
		respondError(c, http.StatusInternalServerError, "write_failed", errDelete)
		return
	}
	if errRefresh := h.refreshAuths(ctx); errRefresh != nil {
		respondError(c, http.StatusInternalServerError, "reload_failed", errRefresh)
		return
	}
	respondOK(c)
}

// synthesizeAPIKeyBody handles a synthesize api key body.
func (h *Handler) synthesizeAPIKeyBody(key string, body []byte) ([]*coreauth.Auth, error) {
	// Normalize source data before building the derived payload.
	cfg := &appconfig.Config{}
	switch key {
	case "gemini-api-key":
		var entries []appconfig.GeminiKey
		if errDecode := decodeListBody(body, key, &entries); errDecode != nil {
			return nil, errDecode
		}
		cfg.GeminiKey = entries
	case "vertex-api-key":
		var entries []appconfig.VertexCompatKey
		if errDecode := decodeListBody(body, key, &entries); errDecode != nil {
			return nil, errDecode
		}
		cfg.VertexCompatAPIKey = entries
	case "codex-api-key":
		var entries []appconfig.CodexKey
		if errDecode := decodeListBody(body, key, &entries); errDecode != nil {
			return nil, errDecode
		}
		cfg.CodexKey = entries
	case "xai-api-key":
		var entries []appconfig.XAIKey
		if errDecode := decodeListBody(body, key, &entries); errDecode != nil {
			return nil, errDecode
		}
		cfg.XAIKey = entries
	case "claude-api-key":
		var entries []appconfig.ClaudeKey
		if errDecode := decodeListBody(body, key, &entries); errDecode != nil {
			return nil, errDecode
		}
		cfg.ClaudeKey = entries
	case "openai-compatibility":
		var entries []appconfig.OpenAICompatibility
		if errDecode := decodeListBody(body, key, &entries); errDecode != nil {
			return nil, errDecode
		}
		cfg.OpenAICompatibility = entries
	default:
		return nil, fmt.Errorf("unsupported api key route %s", key)
	}
	if errNormalizeIDs := cfg.NormalizeProviderCredentialIDs(); errNormalizeIDs != nil {
		return nil, errNormalizeIDs
	}
	cfg.SanitizeGeminiKeys()
	cfg.SanitizeVertexCompatKeys()
	cfg.SanitizeCodexKeys()
	cfg.SanitizeXAIKeys()
	cfg.SanitizeClaudeKeys()
	cfg.SanitizeOpenAICompatibility()
	return synthesizeConfigAuths(cfg), nil
}

// synthesizeConfigAuths handles a synthesize config auths.
func synthesizeConfigAuths(cfg *appconfig.Config) []*coreauth.Auth {
	// Normalize source data before building the derived payload.
	now := time.Now().UTC()
	sctx := &synthesizer.SynthesisContext{
		Config:      cfg,
		Now:         now,
		IDGenerator: synthesizer.NewStableIDGenerator(),
	}
	auths, errSynthesize := synthesizer.NewConfigSynthesizer().Synthesize(sctx)
	if errSynthesize != nil {
		return nil
	}
	return auths
}

// replaceAPIKeyAuths handles a replace api key auths.
func (h *Handler) replaceAPIKeyAuths(ctx context.Context, key string, next []*coreauth.Auth) error {
	existing, errExisting := h.repo.ListAuths(ctx)
	if errExisting != nil {
		return errExisting
	}
	existingBySource := make(map[string]*coreauth.Auth, len(existing))
	for _, auth := range existing {
		if !isAPIKeyAuthForKey(auth, key) || auth.Attributes == nil {
			continue
		}
		source := strings.TrimSpace(auth.Attributes["source"])
		if source != "" {
			existingBySource[auth.Provider+"\x00"+source] = auth
		}
	}
	for _, auth := range next {
		if auth == nil || auth.Attributes == nil {
			continue
		}
		source := strings.TrimSpace(auth.Attributes["source"])
		previous := existingBySource[auth.Provider+"\x00"+source]
		if previous == nil || auth.Attributes["provider_credential_id_generated"] != "true" {
			continue
		}
		auth.ID = previous.ID
		auth.Index = previous.Index
		auth.Attributes["cluster_uuid"] = previous.ID
	}
	return h.repo.ReconcileProviderAuths(ctx, key, next, nil)
}

func setOpenAICompatibilityCredentialID(entry map[string]any, credentialID string) {
	delete(entry, "id")
	delete(entry, "uuid")
	rawEntries, okEntries := entry["api-key-entries"]
	if !okEntries {
		entry["id"] = credentialID
		return
	}
	switch entries := rawEntries.(type) {
	case []map[string]any:
		if len(entries) == 0 {
			entry["id"] = credentialID
			return
		}
		entries[0]["id"] = credentialID
		delete(entries[0], "uuid")
	case []any:
		if len(entries) == 0 {
			entry["id"] = credentialID
			return
		}
		if item, okItem := entries[0].(map[string]any); okItem {
			item["id"] = credentialID
			delete(item, "uuid")
			return
		}
		entry["id"] = credentialID
	default:
		entry["id"] = credentialID
	}
}

func preserveAPIKeyAuthDisabledState(next, current *coreauth.Auth) {
	if next == nil || current == nil {
		return
	}
	next.Disabled = current.Disabled
	if current.Disabled || current.Status == coreauth.StatusDisabled {
		next.Disabled = true
		next.Status = coreauth.StatusDisabled
	}
}

// decodeListBody decodes a list body.
func decodeListBody[T any](body []byte, key string, out *[]T) error {
	// Validate request inputs before mutating persisted state.
	if len(body) == 0 {
		*out = nil
		return nil
	}
	if errUnmarshal := json.Unmarshal(body, out); errUnmarshal == nil {
		return nil
	}
	var object map[string]json.RawMessage
	if errObject := json.Unmarshal(body, &object); errObject == nil && object != nil {
		for _, listKey := range []string{key, "items", "list", "data"} {
			if raw, ok := object[listKey]; ok {
				if errList := json.Unmarshal(raw, out); errList != nil {
					return errList
				}
				return nil
			}
		}
		var single T
		if errSingle := json.Unmarshal(body, &single); errSingle != nil {
			return errSingle
		}
		*out = []T{single}
		return nil
	}
	var single T
	if errSingle := json.Unmarshal(body, &single); errSingle != nil {
		return errSingle
	}
	*out = []T{single}
	return nil
}

type apiKeyPatchRequest struct {
	Index *int           `json:"index"`
	Match *string        `json:"match"`
	Name  *string        `json:"name"`
	ID    *string        `json:"id"`
	UUID  *string        `json:"uuid"`
	Value map[string]any `json:"value"`
}

type apiKeyIdentifier struct {
	ID      string
	Index   *int
	APIKey  string
	BaseURL string
	Name    string
}

// Identifier returns the provider identifier.
func (p apiKeyPatchRequest) Identifier(c *gin.Context) apiKeyIdentifier {
	identifier := apiKeyIdentifier{Index: p.Index}
	if p.ID != nil {
		identifier.ID = strings.TrimSpace(*p.ID)
	}
	if identifier.ID == "" && p.UUID != nil {
		identifier.ID = strings.TrimSpace(*p.UUID)
	}
	if p.Match != nil {
		identifier.APIKey = strings.TrimSpace(*p.Match)
	}
	if p.Name != nil {
		identifier.Name = strings.TrimSpace(*p.Name)
	}
	if c != nil {
		identifier.BaseURL = strings.TrimSpace(c.Query("base-url"))
	}
	return identifier
}

// apiKeyIdentifierFromRequest derives api key identifier from request.
func apiKeyIdentifierFromRequest(c *gin.Context) apiKeyIdentifier {
	identifier := apiKeyIdentifier{
		ID:      firstNonEmptyQuery(c, "id", "uuid", "auth_index", "index"),
		APIKey:  firstNonEmptyQuery(c, "api-key", "api_key", "match"),
		BaseURL: firstNonEmptyQuery(c, "base-url", "base_url"),
		Name:    firstNonEmptyQuery(c, "name"),
	}
	if idxRaw := strings.TrimSpace(c.Query("index")); idxRaw != "" {
		if idx, errAtoi := strconv.Atoi(idxRaw); errAtoi == nil {
			identifier.Index = &idx
		}
	}
	return identifier
}

// findAPIKeyAuth handles a find api key auth.
func findAPIKeyAuth(auths []*coreauth.Auth, key string, identifier apiKeyIdentifier) (*coreauth.Auth, bool) {
	// Validate request inputs before mutating persisted state.
	filtered := make([]*coreauth.Auth, 0, len(auths))
	for _, auth := range auths {
		if isAPIKeyAuthForKey(auth, key) {
			filtered = append(filtered, auth)
		}
	}
	sort.Slice(filtered, func(i, j int) bool {
		return strings.TrimSpace(filtered[i].ID) < strings.TrimSpace(filtered[j].ID)
	})
	if identifier.Index != nil && *identifier.Index >= 0 && *identifier.Index < len(filtered) {
		return filtered[*identifier.Index], false
	}
	for _, auth := range filtered {
		if identifier.ID != "" && (auth.ID == identifier.ID || auth.Index == identifier.ID) {
			return auth, false
		}
	}
	matches := make([]*coreauth.Auth, 0, 1)
	for _, auth := range filtered {
		attrs := auth.Attributes
		if attrs == nil {
			continue
		}
		if identifier.APIKey != "" && strings.TrimSpace(attrs["api_key"]) != identifier.APIKey {
			continue
		}
		if identifier.BaseURL != "" && strings.TrimSpace(attrs["base_url"]) != identifier.BaseURL {
			continue
		}
		if identifier.Name != "" && strings.TrimSpace(attrs["compat_name"]) != identifier.Name && strings.TrimSpace(auth.Label) != identifier.Name {
			continue
		}
		if identifier.APIKey != "" || identifier.Name != "" {
			matches = append(matches, auth)
		}
	}
	if identifier.APIKey != "" && identifier.BaseURL == "" && len(matches) > 1 {
		return nil, true
	}
	if len(matches) > 0 {
		return matches[0], false
	}
	return nil, false
}

// isAPIKeyAuthForKey reports whether api key auth for key.
func isAPIKeyAuthForKey(auth *coreauth.Auth, key string) bool {
	if auth == nil || auth.Attributes == nil || strings.TrimSpace(auth.Attributes["api_key"]) == "" && key != "openai-compatibility" {
		return false
	}
	source := strings.TrimSpace(auth.Attributes["source"])
	switch key {
	case "gemini-api-key":
		return auth.Provider == "gemini" && strings.HasPrefix(source, "config:gemini[")
	case "claude-api-key":
		return auth.Provider == "claude" && strings.HasPrefix(source, "config:claude[")
	case "codex-api-key":
		return auth.Provider == "codex" && strings.HasPrefix(source, "config:codex[")
	case "xai-api-key":
		return auth.Provider == "xai" && strings.HasPrefix(source, "config:xai[")
	case "vertex-api-key":
		return auth.Provider == "vertex" && strings.HasPrefix(source, "config:vertex-apikey[")
	case "openai-compatibility":
		return strings.TrimSpace(auth.Attributes["compat_name"]) != "" || (strings.HasPrefix(source, "config:") && strings.TrimSpace(auth.Attributes["provider_key"]) != "" && auth.Provider != "vertex")
	default:
		return false
	}
}

// apiKeyAuthToMap converts api key auth to map.
func apiKeyAuthToMap(auth *coreauth.Auth, key string) map[string]any {
	// Validate request inputs before mutating persisted state.
	item := make(map[string]any)
	if auth == nil {
		return item
	}
	attrs := auth.Attributes
	if attrs == nil {
		attrs = map[string]string{}
	}
	item["auth_index"] = auth.ID
	item["id"] = auth.ID
	item["api-key"] = attrs["api_key"]
	item["base-url"] = attrs["base_url"]
	item["prefix"] = auth.Prefix
	item["proxy-url"] = auth.ProxyURL
	item["disabled"] = auth.Disabled || auth.Status == coreauth.StatusDisabled
	if priority := strings.TrimSpace(attrs["priority"]); priority != "" {
		if priorityValue, errAtoi := strconv.Atoi(priority); errAtoi == nil {
			item["priority"] = priorityValue
		}
	}
	headers := make(map[string]string)
	for name, value := range attrs {
		if strings.HasPrefix(name, "header:") {
			headers[strings.TrimPrefix(name, "header:")] = value
		}
	}
	if len(headers) > 0 {
		item["headers"] = headers
	}
	if key == "openai-compatibility" {
		name := strings.TrimSpace(attrs["compat_name"])
		if name == "" {
			name = strings.TrimSpace(auth.Label)
		}
		item["name"] = name
		if attrs["api_key"] != "" || auth.ProxyURL != "" {
			delete(item, "id")
			item["api-key-entries"] = []map[string]any{{
				"id":        auth.ID,
				"api-key":   attrs["api_key"],
				"proxy-url": auth.ProxyURL,
			}}
		}
	}
	if (key == "codex-api-key" || key == "xai-api-key") && strings.EqualFold(attrs["websockets"], "true") {
		item["websockets"] = true
	}
	if key == "codex-api-key" && strings.EqualFold(attrs[coreauth.AttributeCodexAlphaSearch], "true") {
		item["alpha-search"] = true
	}
	if excluded := apiKeyExcludedModels(auth); len(excluded) > 0 {
		item["excluded-models"] = excluded
	}
	if disabledCooling, okDisableCooling := auth.DisableCoolingOverride(); okDisableCooling && disabledCooling {
		item["disable-cooling"] = true
	}
	switch key {
	case "codex-api-key", "xai-api-key", "gemini-api-key", "vertex-api-key", "claude-api-key":
		models := credentialAPIKeyModels(auth)
		if len(models) > 0 {
			item["models"] = models
		}
	}
	return item
}

func apiKeyExcludedModels(auth *coreauth.Auth) []string {
	if auth == nil || auth.Attributes == nil {
		return nil
	}
	raw := strings.TrimSpace(auth.Attributes["excluded_models"])
	if raw == "" {
		return nil
	}
	return appconfig.NormalizeExcludedModels(strings.Split(raw, ","))
}

// credentialAPIKeyModels extracts config-like model aliases from stored model metadata.
func credentialAPIKeyModels(auth *coreauth.Auth) []map[string]any {
	pairs := credentialModelPairs(auth)
	out := make([]map[string]any, 0, len(pairs))
	for _, pair := range pairs {
		item := map[string]any{"name": pair.Name, "alias": pair.Alias}
		if pair.DisplayName != "" {
			item["display-name"] = pair.DisplayName
		}
		if pair.ForceMapping {
			item["force-mapping"] = true
		}
		out = append(out, item)
	}
	return out
}

type credentialAPIKeyModelPair struct {
	Name         string
	Alias        string
	DisplayName  string
	ForceMapping bool
}

// credentialModelPairs returns unique model name/alias pairs from auth metadata.
func credentialModelPairs(auth *coreauth.Auth) []credentialAPIKeyModelPair {
	if auth == nil || auth.Metadata == nil {
		return nil
	}
	raw := auth.Metadata["home_config_models"]
	models, ok := raw.([]any)
	if !ok || len(models) == 0 {
		return nil
	}
	out := make([]credentialAPIKeyModelPair, 0, len(models))
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
		if provider := strings.TrimSpace(strings.ToLower(auth.Provider)); provider == "codex" {
			if isNonUserCodexBuiltin(modelMap, alias) {
				continue
			}
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
		forceMapping, _ := parseBoolAny(modelMap["force_mapping"])
		out = append(out, credentialAPIKeyModelPair{
			Name:         name,
			Alias:        alias,
			DisplayName:  strings.TrimSpace(stringFromAny(modelMap["config_display_name"])),
			ForceMapping: forceMapping,
		})
	}
	return out
}

// isNonUserCodexBuiltin reports whether a non-user Codex built-in should be hidden.
func isNonUserCodexBuiltin(modelMap map[string]any, alias string) bool {
	if !strings.EqualFold(alias, "gpt-image-2") {
		return false
	}
	if userDefined, ok := modelMap["user_defined"]; ok {
		if parsed, okParse := parseBoolAny(userDefined); okParse && !parsed {
			return true
		}
	}
	return false
}

// parseBoolAny parses a bool any.
func parseBoolAny(val any) (bool, bool) {
	switch typed := val.(type) {
	case bool:
		return typed, true
	case string:
		trimmed := strings.TrimSpace(typed)
		if trimmed == "" {
			return false, false
		}
		parsed, err := strconv.ParseBool(trimmed)
		if err != nil {
			return false, false
		}
		return parsed, true
	case float64:
		return typed != 0, true
	default:
		return false, false
	}
}

// firstNonEmptyQuery handles a first non empty query.
func firstNonEmptyQuery(c *gin.Context, keys ...string) string {
	if c == nil {
		return ""
	}
	for _, key := range keys {
		if value := strings.TrimSpace(c.Query(key)); value != "" {
			return value
		}
	}
	return ""
}
