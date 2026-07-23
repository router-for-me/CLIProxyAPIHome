package management

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	appconfig "github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"gopkg.in/yaml.v3"
)

// loadRuntimeConfig loads a runtime config.
func (h *Handler) loadRuntimeConfig(c *gin.Context) (context.Context, context.CancelFunc, *appconfig.Config, bool) {
	ctx, cancel := h.requestContext(c)
	cfg, _, errConfig := h.currentConfig(ctx)
	if errConfig != nil {
		cancel()
		respondError(c, http.StatusInternalServerError, "config_load_failed", errConfig)
		return nil, nil, nil, false
	}
	return ctx, cancel, cfg, true
}

// persistConfigRootKey persists a config root key.
func (h *Handler) persistConfigRootKey(c *gin.Context, ctx context.Context, cfg *appconfig.Config, key string) bool {
	if errPersist := h.saveRuntimeConfigRootKey(ctx, cfg, key); errPersist != nil {
		respondError(c, http.StatusInternalServerError, "write_failed", errPersist)
		return false
	}
	if errRefresh := h.refreshConfig(ctx); errRefresh != nil {
		respondError(c, http.StatusInternalServerError, "reload_failed", errRefresh)
		return false
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
	return true
}

// saveRuntimeConfigRootKey saves a runtime config root key.
func (h *Handler) saveRuntimeConfigRootKey(ctx context.Context, cfg *appconfig.Config, key string) error {
	root, errRoot := runtimeConfigRoot(cfg)
	if errRoot != nil {
		return errRoot
	}
	if value, ok := root[key]; ok {
		return h.repo.UpsertConfigValue(ctx, key, value)
	}

	currentRoot, errCurrentRoot := h.configRoot(ctx)
	if errCurrentRoot != nil {
		return errCurrentRoot
	}
	delete(currentRoot, key)
	return h.replaceConfigSnapshot(ctx, currentRoot)
}

// runtimeConfigRoot runs a time config root.
func runtimeConfigRoot(cfg *appconfig.Config) (map[string]any, error) {
	if cfg == nil {
		cfg = &appconfig.Config{}
	}
	data, errMarshal := yaml.Marshal(cfg)
	if errMarshal != nil {
		return nil, errMarshal
	}
	var root map[string]any
	if errUnmarshal := yaml.Unmarshal(data, &root); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	if root == nil {
		root = make(map[string]any)
	}
	for key := range root {
		if isCredentialConfigKey(key) {
			delete(root, key)
		}
	}
	return root, nil
}

// updateBoolConfigField updates a bool config field.
func (h *Handler) updateBoolConfigField(c *gin.Context, rootKey string, set func(*appconfig.Config, bool)) {
	var body struct {
		Value *bool `json:"value"`
	}
	if errBindJSON := c.ShouldBindJSON(&body); errBindJSON != nil || body.Value == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	ctx, cancel, cfg, ok := h.loadRuntimeConfig(c)
	if !ok {
		return
	}
	defer cancel()
	set(cfg, *body.Value)
	h.persistConfigRootKey(c, ctx, cfg, rootKey)
}

// updateIntConfigField updates an int config field.
func (h *Handler) updateIntConfigField(c *gin.Context, rootKey string, set func(*appconfig.Config, int)) {
	var body struct {
		Value *int `json:"value"`
	}
	if errBindJSON := c.ShouldBindJSON(&body); errBindJSON != nil || body.Value == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	ctx, cancel, cfg, ok := h.loadRuntimeConfig(c)
	if !ok {
		return
	}
	defer cancel()
	set(cfg, *body.Value)
	h.persistConfigRootKey(c, ctx, cfg, rootKey)
}

// updateStringConfigField updates a string config field.
func (h *Handler) updateStringConfigField(c *gin.Context, rootKey string, set func(*appconfig.Config, string)) {
	var body struct {
		Value *string `json:"value"`
	}
	if errBindJSON := c.ShouldBindJSON(&body); errBindJSON != nil || body.Value == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	ctx, cancel, cfg, ok := h.loadRuntimeConfig(c)
	if !ok {
		return
	}
	defer cancel()
	set(cfg, *body.Value)
	h.persistConfigRootKey(c, ctx, cfg, rootKey)
}

// GetDebug returns a debug.
func (h *Handler) GetDebug(c *gin.Context) {
	_, cancel, cfg, ok := h.loadRuntimeConfig(c)
	if !ok {
		return
	}
	defer cancel()
	c.JSON(http.StatusOK, gin.H{"debug": cfg.Debug})
}

// PutDebug replaces a debug.
func (h *Handler) PutDebug(c *gin.Context) {
	h.updateBoolConfigField(c, "debug", func(cfg *appconfig.Config, value bool) { cfg.Debug = value })
}

// GetUsageStatisticsEnabled returns an usage statistics enabled.
func (h *Handler) GetUsageStatisticsEnabled(c *gin.Context) {
	_, cancel, cfg, ok := h.loadRuntimeConfig(c)
	if !ok {
		return
	}
	defer cancel()
	c.JSON(http.StatusOK, gin.H{"usage-statistics-enabled": cfg.UsageStatisticsEnabled})
}

// PutUsageStatisticsEnabled replaces an usage statistics enabled.
func (h *Handler) PutUsageStatisticsEnabled(c *gin.Context) {
	var body struct {
		Value *bool `json:"value"`
	}
	if errBindJSON := c.ShouldBindJSON(&body); errBindJSON != nil || body.Value == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	if !*body.Value {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "usage statistics must remain enabled in home mode"})
		return
	}
	h.updateBoolConfigField(c, "usage-statistics-enabled", func(cfg *appconfig.Config, value bool) { cfg.UsageStatisticsEnabled = true })
}

// GetLoggingToFile returns a logging to file.
func (h *Handler) GetLoggingToFile(c *gin.Context) {
	_, cancel, cfg, ok := h.loadRuntimeConfig(c)
	if !ok {
		return
	}
	defer cancel()
	c.JSON(http.StatusOK, gin.H{"logging-to-file": cfg.LoggingToFile})
}

// PutLoggingToFile replaces a logging to file.
func (h *Handler) PutLoggingToFile(c *gin.Context) {
	h.updateBoolConfigField(c, "logging-to-file", func(cfg *appconfig.Config, value bool) { cfg.LoggingToFile = value })
}

// GetLogsMaxTotalSizeMB returns a logs max total size mb.
func (h *Handler) GetLogsMaxTotalSizeMB(c *gin.Context) {
	_, cancel, cfg, ok := h.loadRuntimeConfig(c)
	if !ok {
		return
	}
	defer cancel()
	c.JSON(http.StatusOK, gin.H{"logs-max-total-size-mb": cfg.LogsMaxTotalSizeMB})
}

// PutLogsMaxTotalSizeMB replaces a logs max total size mb.
func (h *Handler) PutLogsMaxTotalSizeMB(c *gin.Context) {
	var body struct {
		Value *int `json:"value"`
	}
	if errBindJSON := c.ShouldBindJSON(&body); errBindJSON != nil || body.Value == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	ctx, cancel, cfg, ok := h.loadRuntimeConfig(c)
	if !ok {
		return
	}
	defer cancel()
	value := *body.Value
	if value < 0 {
		value = 0
	}
	cfg.LogsMaxTotalSizeMB = value
	h.persistConfigRootKey(c, ctx, cfg, "logs-max-total-size-mb")
}

// GetErrorLogsMaxFiles returns an error logs max files.
func (h *Handler) GetErrorLogsMaxFiles(c *gin.Context) {
	_, cancel, cfg, ok := h.loadRuntimeConfig(c)
	if !ok {
		return
	}
	defer cancel()
	c.JSON(http.StatusOK, gin.H{"error-logs-max-files": cfg.ErrorLogsMaxFiles})
}

// PutErrorLogsMaxFiles replaces an error logs max files.
func (h *Handler) PutErrorLogsMaxFiles(c *gin.Context) {
	var body struct {
		Value *int `json:"value"`
	}
	if errBindJSON := c.ShouldBindJSON(&body); errBindJSON != nil || body.Value == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	ctx, cancel, cfg, ok := h.loadRuntimeConfig(c)
	if !ok {
		return
	}
	defer cancel()
	value := *body.Value
	if value < 0 {
		value = 10
	}
	cfg.ErrorLogsMaxFiles = value
	h.persistConfigRootKey(c, ctx, cfg, "error-logs-max-files")
}

// GetRequestLog returns a request log.
func (h *Handler) GetRequestLog(c *gin.Context) {
	_, cancel, cfg, ok := h.loadRuntimeConfig(c)
	if !ok {
		return
	}
	defer cancel()
	c.JSON(http.StatusOK, gin.H{"request-log": cfg.RequestLog})
}

// PutRequestLog replaces a request log.
func (h *Handler) PutRequestLog(c *gin.Context) {
	h.updateBoolConfigField(c, "request-log", func(cfg *appconfig.Config, value bool) { cfg.RequestLog = value })
}

// GetRequestRetry returns a request retry.
func (h *Handler) GetRequestRetry(c *gin.Context) {
	_, cancel, cfg, ok := h.loadRuntimeConfig(c)
	if !ok {
		return
	}
	defer cancel()
	c.JSON(http.StatusOK, gin.H{"request-retry": cfg.RequestRetry})
}

// PutRequestRetry replaces a request retry.
func (h *Handler) PutRequestRetry(c *gin.Context) {
	h.updateIntConfigField(c, "request-retry", func(cfg *appconfig.Config, value int) { cfg.RequestRetry = value })
}

// GetMaxRetryInterval returns a max retry interval.
func (h *Handler) GetMaxRetryInterval(c *gin.Context) {
	_, cancel, cfg, ok := h.loadRuntimeConfig(c)
	if !ok {
		return
	}
	defer cancel()
	c.JSON(http.StatusOK, gin.H{"max-retry-interval": cfg.MaxRetryInterval})
}

// PutMaxRetryInterval replaces a max retry interval.
func (h *Handler) PutMaxRetryInterval(c *gin.Context) {
	h.updateIntConfigField(c, "max-retry-interval", func(cfg *appconfig.Config, value int) { cfg.MaxRetryInterval = value })
}

// GetForceModelPrefix returns a force model prefix.
func (h *Handler) GetForceModelPrefix(c *gin.Context) {
	_, cancel, cfg, ok := h.loadRuntimeConfig(c)
	if !ok {
		return
	}
	defer cancel()
	c.JSON(http.StatusOK, gin.H{"force-model-prefix": cfg.ForceModelPrefix})
}

// PutForceModelPrefix replaces a force model prefix.
func (h *Handler) PutForceModelPrefix(c *gin.Context) {
	h.updateBoolConfigField(c, "force-model-prefix", func(cfg *appconfig.Config, value bool) { cfg.ForceModelPrefix = value })
}

// normalizeClusterRoutingStrategy normalizes a cluster routing strategy.
func normalizeClusterRoutingStrategy(strategy string) (string, bool) {
	normalized := strings.ToLower(strings.TrimSpace(strategy))
	switch normalized {
	case "", "round-robin", "roundrobin", "rr":
		return "round-robin", true
	case "fill-first", "fillfirst", "ff":
		return "fill-first", true
	default:
		return "", false
	}
}

// GetRoutingStrategy returns a routing strategy.
func (h *Handler) GetRoutingStrategy(c *gin.Context) {
	_, cancel, cfg, ok := h.loadRuntimeConfig(c)
	if !ok {
		return
	}
	defer cancel()
	strategy, okNormalize := normalizeClusterRoutingStrategy(cfg.Routing.Strategy)
	if !okNormalize {
		c.JSON(http.StatusOK, gin.H{"strategy": strings.TrimSpace(cfg.Routing.Strategy)})
		return
	}
	c.JSON(http.StatusOK, gin.H{"strategy": strategy})
}

// PutRoutingStrategy replaces a routing strategy.
func (h *Handler) PutRoutingStrategy(c *gin.Context) {
	var body struct {
		Value *string `json:"value"`
	}
	if errBindJSON := c.ShouldBindJSON(&body); errBindJSON != nil || body.Value == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	normalized, okNormalize := normalizeClusterRoutingStrategy(*body.Value)
	if !okNormalize {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid strategy"})
		return
	}
	ctx, cancel, cfg, ok := h.loadRuntimeConfig(c)
	if !ok {
		return
	}
	defer cancel()
	cfg.Routing.Strategy = normalized
	h.persistConfigRootKey(c, ctx, cfg, "routing")
}

// GetProxyURL returns a proxy url.
func (h *Handler) GetProxyURL(c *gin.Context) {
	_, cancel, cfg, ok := h.loadRuntimeConfig(c)
	if !ok {
		return
	}
	defer cancel()
	c.JSON(http.StatusOK, gin.H{"proxy-url": cfg.ProxyURL})
}

// PutProxyURL replaces a proxy url.
func (h *Handler) PutProxyURL(c *gin.Context) {
	h.updateStringConfigField(c, "proxy-url", func(cfg *appconfig.Config, value string) { cfg.ProxyURL = value })
}

// DeleteProxyURL deletes a proxy url.
func (h *Handler) DeleteProxyURL(c *gin.Context) {
	ctx, cancel, cfg, ok := h.loadRuntimeConfig(c)
	if !ok {
		return
	}
	defer cancel()
	cfg.ProxyURL = ""
	h.persistConfigRootKey(c, ctx, cfg, "proxy-url")
}

// GetSwitchProject returns a switch project.
func (h *Handler) GetSwitchProject(c *gin.Context) {
	_, cancel, cfg, ok := h.loadRuntimeConfig(c)
	if !ok {
		return
	}
	defer cancel()
	c.JSON(http.StatusOK, gin.H{"switch-project": cfg.QuotaExceeded.SwitchProject})
}

// PutSwitchProject replaces a switch project.
func (h *Handler) PutSwitchProject(c *gin.Context) {
	h.updateBoolConfigField(c, "quota-exceeded", func(cfg *appconfig.Config, value bool) { cfg.QuotaExceeded.SwitchProject = value })
}

// GetSwitchPreviewModel returns a switch preview model.
func (h *Handler) GetSwitchPreviewModel(c *gin.Context) {
	_, cancel, cfg, ok := h.loadRuntimeConfig(c)
	if !ok {
		return
	}
	defer cancel()
	c.JSON(http.StatusOK, gin.H{"switch-preview-model": cfg.QuotaExceeded.SwitchPreviewModel})
}

// PutSwitchPreviewModel replaces a switch preview model.
func (h *Handler) PutSwitchPreviewModel(c *gin.Context) {
	h.updateBoolConfigField(c, "quota-exceeded", func(cfg *appconfig.Config, value bool) { cfg.QuotaExceeded.SwitchPreviewModel = value })
}

// GetAPIKeys returns an api keys.
func (h *Handler) GetAPIKeys(c *gin.Context) {
	ctx, cancel := h.requestContext(c)
	defer cancel()
	entries, errEntries := h.repo.ListAPIKeyEntries(ctx)
	if errEntries != nil {
		respondError(c, http.StatusInternalServerError, "api_keys_load_failed", errEntries)
		return
	}
	c.JSON(http.StatusOK, apiKeyEntriesResponse(entries))
}

// PostAPIKeys creates one API key.
func (h *Handler) PostAPIKeys(c *gin.Context) {
	h.createAPIKeyEntry(c)
}

// PutAPIKeys replaces an api keys.
func (h *Handler) PutAPIKeys(c *gin.Context) {
	data, errData := c.GetRawData()
	if errData != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read body"})
		return
	}
	entries, errEntries := decodeAPIKeyEntryUpdates(data)
	if errEntries != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	ctx, cancel := h.requestContext(c)
	defer cancel()
	if _, errReplace := h.repo.ReplaceAPIKeyEntries(ctx, entries); errReplace != nil {
		if cluster.IsAPIKeyConflictError(errReplace) {
			respondError(c, http.StatusConflict, "api_key_exists", errReplace)
			return
		}
		if errors.Is(errReplace, cluster.ErrUserNotFound) {
			respondError(c, http.StatusNotFound, "user_not_found", errReplace)
			return
		}
		respondError(c, http.StatusInternalServerError, "write_failed", errReplace)
		return
	}
	if errRefresh := h.refreshConfig(ctx); errRefresh != nil {
		respondError(c, http.StatusInternalServerError, "reload_failed", errRefresh)
		return
	}
	respondOK(c)
}

// PatchAPIKeys applies a partial update to an api keys.
func (h *Handler) PatchAPIKeys(c *gin.Context) {
	if errPatch := h.patchAPIKeyEntries(c); errPatch != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errPatch.Error()})
	}
}

// DeleteAPIKeys deletes an api keys.
func (h *Handler) DeleteAPIKeys(c *gin.Context) {
	if errDelete := h.deleteAPIKeyEntry(c); errDelete != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errDelete.Error()})
	}
}

// GetOAuthExcludedModels returns an o auth excluded models.
func (h *Handler) GetOAuthExcludedModels(c *gin.Context) {
	_, cancel, cfg, ok := h.loadRuntimeConfig(c)
	if !ok {
		return
	}
	defer cancel()
	c.JSON(http.StatusOK, gin.H{"oauth-excluded-models": appconfig.NormalizeOAuthExcludedModels(cfg.OAuthExcludedModels)})
}

// PutOAuthExcludedModels replaces an o auth excluded models.
func (h *Handler) PutOAuthExcludedModels(c *gin.Context) {
	// Resolve credential context before calling upstream OAuth services.
	data, errData := c.GetRawData()
	if errData != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read body"})
		return
	}
	var entries map[string][]string
	if errUnmarshal := json.Unmarshal(data, &entries); errUnmarshal != nil {
		var wrapper struct {
			Items map[string][]string `json:"items"`
		}
		if errUnmarshalWrapper := json.Unmarshal(data, &wrapper); errUnmarshalWrapper != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
			return
		}
		entries = wrapper.Items
	}
	ctx, cancel, cfg, ok := h.loadRuntimeConfig(c)
	if !ok {
		return
	}
	defer cancel()
	cfg.OAuthExcludedModels = appconfig.NormalizeOAuthExcludedModels(entries)
	h.persistConfigRootKey(c, ctx, cfg, "oauth-excluded-models")
}

// PatchOAuthExcludedModels applies a partial update to an o auth excluded models.
func (h *Handler) PatchOAuthExcludedModels(c *gin.Context) {
	// Resolve credential context before calling upstream OAuth services.
	var body struct {
		Provider *string  `json:"provider"`
		Models   []string `json:"models"`
	}
	if errBindJSON := c.ShouldBindJSON(&body); errBindJSON != nil || body.Provider == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	provider := strings.ToLower(strings.TrimSpace(*body.Provider))
	if provider == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid provider"})
		return
	}
	ctx, cancel, cfg, ok := h.loadRuntimeConfig(c)
	if !ok {
		return
	}
	defer cancel()
	normalized := appconfig.NormalizeExcludedModels(body.Models)
	if len(normalized) == 0 {
		if cfg.OAuthExcludedModels == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "provider not found"})
			return
		}
		if _, exists := cfg.OAuthExcludedModels[provider]; !exists {
			c.JSON(http.StatusNotFound, gin.H{"error": "provider not found"})
			return
		}
		delete(cfg.OAuthExcludedModels, provider)
		if len(cfg.OAuthExcludedModels) == 0 {
			cfg.OAuthExcludedModels = nil
		}
		h.persistConfigRootKey(c, ctx, cfg, "oauth-excluded-models")
		return
	}
	if cfg.OAuthExcludedModels == nil {
		cfg.OAuthExcludedModels = make(map[string][]string)
	}
	cfg.OAuthExcludedModels[provider] = normalized
	h.persistConfigRootKey(c, ctx, cfg, "oauth-excluded-models")
}

// DeleteOAuthExcludedModels deletes an o auth excluded models.
func (h *Handler) DeleteOAuthExcludedModels(c *gin.Context) {
	// Resolve credential context before calling upstream OAuth services.
	provider := strings.ToLower(strings.TrimSpace(c.Query("provider")))
	if provider == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing provider"})
		return
	}
	ctx, cancel, cfg, ok := h.loadRuntimeConfig(c)
	if !ok {
		return
	}
	defer cancel()
	if cfg.OAuthExcludedModels == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "provider not found"})
		return
	}
	if _, exists := cfg.OAuthExcludedModels[provider]; !exists {
		c.JSON(http.StatusNotFound, gin.H{"error": "provider not found"})
		return
	}
	delete(cfg.OAuthExcludedModels, provider)
	if len(cfg.OAuthExcludedModels) == 0 {
		cfg.OAuthExcludedModels = nil
	}
	h.persistConfigRootKey(c, ctx, cfg, "oauth-excluded-models")
}

// GetOAuthModelAlias returns an o auth model alias.
func (h *Handler) GetOAuthModelAlias(c *gin.Context) {
	_, cancel, cfg, ok := h.loadRuntimeConfig(c)
	if !ok {
		return
	}
	defer cancel()
	c.JSON(http.StatusOK, gin.H{"oauth-model-alias": sanitizedOAuthModelAlias(cfg.OAuthModelAlias)})
}

// PutOAuthModelAlias replaces an o auth model alias.
func (h *Handler) PutOAuthModelAlias(c *gin.Context) {
	// Resolve credential context before calling upstream OAuth services.
	data, errData := c.GetRawData()
	if errData != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read body"})
		return
	}
	var entries map[string][]appconfig.OAuthModelAlias
	if errUnmarshal := json.Unmarshal(data, &entries); errUnmarshal != nil {
		var wrapper struct {
			Items map[string][]appconfig.OAuthModelAlias `json:"items"`
		}
		if errUnmarshalWrapper := json.Unmarshal(data, &wrapper); errUnmarshalWrapper != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
			return
		}
		entries = wrapper.Items
	}
	ctx, cancel, cfg, ok := h.loadRuntimeConfig(c)
	if !ok {
		return
	}
	defer cancel()
	cfg.OAuthModelAlias = sanitizedOAuthModelAlias(entries)
	h.persistConfigRootKey(c, ctx, cfg, "oauth-model-alias")
}

// PatchOAuthModelAlias applies a partial update to an o auth model alias.
func (h *Handler) PatchOAuthModelAlias(c *gin.Context) {
	// Resolve credential context before calling upstream OAuth services.
	var body struct {
		Provider *string                     `json:"provider"`
		Channel  *string                     `json:"channel"`
		Aliases  []appconfig.OAuthModelAlias `json:"aliases"`
	}
	if errBindJSON := c.ShouldBindJSON(&body); errBindJSON != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	channelRaw := ""
	if body.Channel != nil {
		channelRaw = *body.Channel
	} else if body.Provider != nil {
		channelRaw = *body.Provider
	}
	channel := strings.ToLower(strings.TrimSpace(channelRaw))
	if channel == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid channel"})
		return
	}
	ctx, cancel, cfg, ok := h.loadRuntimeConfig(c)
	if !ok {
		return
	}
	defer cancel()
	normalizedMap := sanitizedOAuthModelAlias(map[string][]appconfig.OAuthModelAlias{channel: body.Aliases})
	normalized := normalizedMap[channel]
	if len(normalized) == 0 {
		if cfg.OAuthModelAlias == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "channel not found"})
			return
		}
		if _, exists := cfg.OAuthModelAlias[channel]; !exists {
			c.JSON(http.StatusNotFound, gin.H{"error": "channel not found"})
			return
		}
		delete(cfg.OAuthModelAlias, channel)
		if len(cfg.OAuthModelAlias) == 0 {
			cfg.OAuthModelAlias = nil
		}
		h.persistConfigRootKey(c, ctx, cfg, "oauth-model-alias")
		return
	}
	if cfg.OAuthModelAlias == nil {
		cfg.OAuthModelAlias = make(map[string][]appconfig.OAuthModelAlias)
	}
	cfg.OAuthModelAlias[channel] = normalized
	h.persistConfigRootKey(c, ctx, cfg, "oauth-model-alias")
}

// DeleteOAuthModelAlias deletes an o auth model alias.
func (h *Handler) DeleteOAuthModelAlias(c *gin.Context) {
	// Resolve credential context before calling upstream OAuth services.
	channel := strings.ToLower(strings.TrimSpace(c.Query("channel")))
	if channel == "" {
		channel = strings.ToLower(strings.TrimSpace(c.Query("provider")))
	}
	if channel == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing channel"})
		return
	}
	ctx, cancel, cfg, ok := h.loadRuntimeConfig(c)
	if !ok {
		return
	}
	defer cancel()
	if cfg.OAuthModelAlias == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "channel not found"})
		return
	}
	if _, exists := cfg.OAuthModelAlias[channel]; !exists {
		c.JSON(http.StatusNotFound, gin.H{"error": "channel not found"})
		return
	}
	delete(cfg.OAuthModelAlias, channel)
	if len(cfg.OAuthModelAlias) == 0 {
		cfg.OAuthModelAlias = nil
	}
	h.persistConfigRootKey(c, ctx, cfg, "oauth-model-alias")
}

// sanitizedOAuthModelAlias sanitizes a d o auth model alias.
func sanitizedOAuthModelAlias(entries map[string][]appconfig.OAuthModelAlias) map[string][]appconfig.OAuthModelAlias {
	if len(entries) == 0 {
		return nil
	}
	copied := make(map[string][]appconfig.OAuthModelAlias, len(entries))
	for channel, aliases := range entries {
		if len(aliases) == 0 {
			continue
		}
		copied[channel] = append([]appconfig.OAuthModelAlias(nil), aliases...)
	}
	if len(copied) == 0 {
		return nil
	}
	cfg := appconfig.Config{OAuthModelAlias: copied}
	cfg.SanitizeOAuthModelAlias()
	if len(cfg.OAuthModelAlias) == 0 {
		return nil
	}
	return cfg.OAuthModelAlias
}
