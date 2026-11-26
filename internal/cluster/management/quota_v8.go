package management

import (
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginhost"
	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

type credentialQuotaRequest struct {
	AuthIndexSnake  *string `json:"auth_index"`
	AuthIndexCamel  *string `json:"authIndex"`
	AuthIndexPascal *string `json:"AuthIndex"`
	PluginID        string  `json:"plugin_id"`
	Provider        string  `json:"provider"`
}

func (r credentialQuotaRequest) resolveAuthIndex() string {
	if r.AuthIndexSnake != nil && strings.TrimSpace(*r.AuthIndexSnake) != "" {
		return strings.TrimSpace(*r.AuthIndexSnake)
	}
	if r.AuthIndexCamel != nil && strings.TrimSpace(*r.AuthIndexCamel) != "" {
		return strings.TrimSpace(*r.AuthIndexCamel)
	}
	if r.AuthIndexPascal != nil && strings.TrimSpace(*r.AuthIndexPascal) != "" {
		return strings.TrimSpace(*r.AuthIndexPascal)
	}
	return ""
}

// GetQuotaProviders returns the list of registered quota providers.
func (h *Handler) GetQuotaProviders(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusOK, gin.H{"providers": []any{}})
		return
	}
	host := h.runtime.PluginHost()
	providers := make([]pluginhost.RegisteredQuotaProviderInfo, 0)
	if host != nil {
		providers = append(providers, host.QuotaProviders(c.Request.Context())...)
	}
	if h.quotaRecollect != nil {
		for provider := range quotaRecollectProviders {
			providers = append(providers, pluginhost.RegisteredQuotaProviderInfo{Provider: provider, SupportedProviders: []string{provider}})
		}
	}
	sort.Slice(providers, func(i, j int) bool {
		if providers[i].Provider == providers[j].Provider {
			return providers[i].PluginID < providers[j].PluginID
		}
		return providers[i].Provider < providers[j].Provider
	})
	c.JSON(http.StatusOK, gin.H{"providers": providers})
}

// FetchCredentialQuota invokes a plugin quota provider or queues a Home collector.
func (h *Handler) FetchCredentialQuota(c *gin.Context) {
	var body credentialQuotaRequest
	if errBind := c.ShouldBindJSON(&body); errBind != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	authIndex := body.resolveAuthIndex()
	if authIndex == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "auth_index is required"})
		return
	}

	auth, errAuth := h.quotaAuthByIndex(c, authIndex)
	if errAuth != nil {
		respondError(c, http.StatusInternalServerError, "auth_load_failed", errAuth)
		return
	}
	if auth == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "auth not found"})
		return
	}

	host := h.runtime.PluginHost()

	pluginID := strings.TrimSpace(body.PluginID)
	provider := strings.TrimSpace(body.Provider)
	if provider == "" {
		provider = auth.Provider
	}

	if host != nil {
		req := pluginapi.QuotaFetchRequest{
			AuthIndex:  auth.Index,
			AuthID:     auth.ID,
			Provider:   provider,
			Metadata:   auth.Metadata,
			Attributes: auth.Attributes,
		}
		var quotaResp pluginapi.QuotaFetchResponse
		var handled bool
		var errFetch error
		if pluginID != "" {
			quotaResp, handled, errFetch = host.FetchQuotaByPlugin(c.Request.Context(), pluginID, req)
		} else {
			quotaResp, handled, errFetch = host.FetchQuota(c.Request.Context(), req)
		}
		if handled {
			if errFetch != nil {
				log.WithError(errFetch).Warnf("failed to fetch quota for credential %s", auth.Index)
				c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("failed to fetch quota: %v", errFetch)})
				return
			}
			c.JSON(http.StatusOK, quotaResp)
			return
		}
	}

	// Built-in Home collectors publish database snapshots asynchronously.
	if _, supported := quotaRecollectProviders[canonicalQuotaRecollectProvider(auth.Provider)]; supported && h.quotaRecollect != nil {
		accepted, errTrigger := h.quotaRecollect.TriggerCollection(c.Request.Context(), map[string]struct{}{auth.ID: {}}, nil)
		if errTrigger != nil {
			respondError(c, http.StatusInternalServerError, "quota_collect_failed", errTrigger)
			return
		}
		c.JSON(http.StatusAccepted, gin.H{"accepted": accepted, "running": accepted > 0, "credential_id": auth.ID})
		return
	}

	c.JSON(http.StatusNotImplemented, gin.H{"error": "no quota provider available for credential"})
}

// ResetCredentialQuota resets quota or usage for a credential.
func (h *Handler) ResetCredentialQuota(c *gin.Context) {
	var body credentialQuotaRequest
	if errBind := c.ShouldBindJSON(&body); errBind != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	authIndex := body.resolveAuthIndex()
	if authIndex == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "auth_index is required"})
		return
	}

	auth, errAuth := h.quotaAuthByIndex(c, authIndex)
	if errAuth != nil {
		respondError(c, http.StatusInternalServerError, "auth_load_failed", errAuth)
		return
	}
	if auth == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "auth not found"})
		return
	}

	host := h.runtime.PluginHost()

	if host == nil {
		c.JSON(http.StatusNotImplemented, gin.H{"error": "plugin host unavailable"})
		return
	}

	pluginID := strings.TrimSpace(body.PluginID)
	provider := strings.TrimSpace(body.Provider)
	if provider == "" {
		provider = auth.Provider
	}

	req := pluginapi.QuotaResetRequest{
		AuthIndex:  auth.Index,
		AuthID:     auth.ID,
		Provider:   provider,
		Metadata:   auth.Metadata,
		Attributes: auth.Attributes,
	}

	var resetResp pluginapi.QuotaResetResponse
	var handled bool
	var errReset error

	if pluginID != "" {
		if !host.HasQuotaProviderForPlugin(pluginID) {
			c.JSON(http.StatusNotFound, gin.H{"error": "quota provider not found for plugin"})
			return
		}
		resetResp, handled, errReset = host.ResetQuotaByPlugin(c.Request.Context(), pluginID, req)
		if !handled {
			c.JSON(http.StatusNotFound, gin.H{"error": "quota provider not found for plugin"})
			return
		}
	} else {
		if !host.HasQuotaProviderContext(c.Request.Context(), provider) {
			c.JSON(http.StatusNotImplemented, gin.H{"error": "no quota provider available for credential to reset"})
			return
		}
		resetResp, handled, errReset = host.ResetQuota(c.Request.Context(), req)
		if !handled {
			c.JSON(http.StatusBadGateway, gin.H{"error": "quota provider did not handle reset request"})
			return
		}
	}

	if errReset != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("plugin quota reset failed: %v", errReset)})
		return
	}
	if !resetResp.Success {
		msg := resetResp.Message
		if msg == "" {
			msg = "quota reset rejected by provider"
		}
		c.JSON(http.StatusBadGateway, gin.H{"error": msg})
		return
	}

	if h.runtime != nil && h.runtime.CoreManager() != nil {
		_, errResetCore := h.runtime.CoreManager().ClearQuotaCooldown(c.Request.Context(), auth.ID, "")
		if errResetCore != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to reset routing quota: %v", errResetCore)})
			return
		}
	}

	resp := gin.H{
		"status":     "ok",
		"auth_index": auth.Index,
	}
	if resetResp.Message != "" {
		resp["message"] = resetResp.Message
	}
	c.JSON(http.StatusOK, resp)
}

// GetPluginQuota handles GET /v8/management/plugins/:id/quota?auth_index=...
func (h *Handler) GetPluginQuota(c *gin.Context) {
	pluginID := strings.TrimSpace(c.Param("id"))
	authIndex := strings.TrimSpace(c.Query("auth_index"))
	if authIndex == "" {
		authIndex = strings.TrimSpace(c.Query("authIndex"))
	}
	if authIndex == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "auth_index is required"})
		return
	}
	h.fetchQuotaForPlugin(c, pluginID, authIndex)
}

// FetchPluginQuota handles POST /v8/management/plugins/:id/quota
func (h *Handler) FetchPluginQuota(c *gin.Context) {
	pluginID := strings.TrimSpace(c.Param("id"))
	var body credentialQuotaRequest
	if errBind := c.ShouldBindJSON(&body); errBind != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	authIndex := body.resolveAuthIndex()
	if authIndex == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "auth_index is required"})
		return
	}
	h.fetchQuotaForPlugin(c, pluginID, authIndex)
}

func (h *Handler) fetchQuotaForPlugin(c *gin.Context, pluginID, authIndex string) {
	auth, errAuth := h.quotaAuthByIndex(c, authIndex)
	if errAuth != nil {
		respondError(c, http.StatusInternalServerError, "auth_load_failed", errAuth)
		return
	}
	if auth == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "auth not found"})
		return
	}

	host := h.runtime.PluginHost()

	if host == nil || !host.HasQuotaProviderForPlugin(pluginID) {
		c.JSON(http.StatusNotFound, gin.H{"error": "quota provider not found for plugin"})
		return
	}

	quotaResp, handled, errFetch := host.FetchQuotaByPlugin(c.Request.Context(), pluginID, pluginapi.QuotaFetchRequest{
		AuthIndex:  auth.Index,
		AuthID:     auth.ID,
		Provider:   auth.Provider,
		Metadata:   auth.Metadata,
		Attributes: auth.Attributes,
	})
	if !handled {
		c.JSON(http.StatusNotFound, gin.H{"error": "quota provider not found for plugin"})
		return
	}
	if errFetch != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("failed to fetch quota: %v", errFetch)})
		return
	}
	c.JSON(http.StatusOK, quotaResp)
}

// ResetPluginQuota handles DELETE /v8/management/plugins/:id/quota and POST /v8/management/plugins/:id/quota/reset
func (h *Handler) ResetPluginQuota(c *gin.Context) {
	pluginID := strings.TrimSpace(c.Param("id"))
	authIndex := strings.TrimSpace(c.Query("auth_index"))
	if authIndex == "" {
		authIndex = strings.TrimSpace(c.Query("authIndex"))
	}
	if authIndex == "" {
		var body credentialQuotaRequest
		_ = c.ShouldBindJSON(&body)
		authIndex = body.resolveAuthIndex()
	}
	if authIndex == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "auth_index is required"})
		return
	}

	auth, errAuth := h.quotaAuthByIndex(c, authIndex)
	if errAuth != nil {
		respondError(c, http.StatusInternalServerError, "auth_load_failed", errAuth)
		return
	}
	if auth == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "auth not found"})
		return
	}

	host := h.runtime.PluginHost()

	if host == nil || !host.HasQuotaProviderForPlugin(pluginID) {
		c.JSON(http.StatusNotFound, gin.H{"error": "quota provider not found for plugin"})
		return
	}

	resetResp, handled, errReset := host.ResetQuotaByPlugin(c.Request.Context(), pluginID, pluginapi.QuotaResetRequest{
		AuthIndex:  auth.Index,
		AuthID:     auth.ID,
		Provider:   auth.Provider,
		Metadata:   auth.Metadata,
		Attributes: auth.Attributes,
	})
	if !handled {
		c.JSON(http.StatusNotFound, gin.H{"error": "quota provider not found for plugin"})
		return
	}
	if errReset != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("failed to reset quota: %v", errReset)})
		return
	}
	if !resetResp.Success {
		msg := resetResp.Message
		if msg == "" {
			msg = "quota reset rejected by plugin"
		}
		c.JSON(http.StatusBadGateway, gin.H{"error": msg})
		return
	}

	if h.runtime != nil && h.runtime.CoreManager() != nil {
		_, errResetCore := h.runtime.CoreManager().ClearQuotaCooldown(c.Request.Context(), auth.ID, "")
		if errResetCore != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to reset routing quota: %v", errResetCore)})
			return
		}
	}

	resp := gin.H{
		"status":     "ok",
		"auth_index": auth.Index,
	}
	if resetResp.Message != "" {
		resp["message"] = resetResp.Message
	}
	c.JSON(http.StatusOK, resp)
}

func (h *Handler) quotaAuthByIndex(c *gin.Context, index string) (*coreauth.Auth, error) {
	auths, errAuths := h.repo.ListAuths(c.Request.Context())
	if errAuths != nil {
		return nil, errAuths
	}
	for _, auth := range auths {
		if auth != nil && (auth.ID == index || auth.Index == index) {
			return auth, nil
		}
	}
	return nil, nil
}
