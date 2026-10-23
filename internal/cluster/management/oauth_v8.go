package management

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// StartOAuthV8 dispatches login using the provider query parameter.
func (h *Handler) StartOAuthV8(c *gin.Context) {
	switch strings.ToLower(strings.TrimSpace(c.Query("provider"))) {
	case "":
		c.JSON(http.StatusBadRequest, gin.H{"error": "provider is required"})
	case "claude":
		h.RequestAnthropicToken(c)
	case "codex":
		switch strings.ToLower(strings.TrimSpace(c.Query("flow"))) {
		case "", "browser":
			h.RequestCodexToken(c)
		case "device":
			h.RequestCodexDeviceToken(c)
		default:
			c.JSON(http.StatusBadRequest, gin.H{"error": "unsupported oauth flow"})
		}
	case "antigravity":
		h.RequestAntigravityToken(c)
	case "kimi", "kimi-ai":
		h.RequestKimiToken(c)
	case "xai":
		h.RequestXAIToken(c)
	case "devin":
		h.RequestDevinToken(c)
	case "meta":
		h.RequestMetaToken(c)
	default:
		if !h.ServePluginAuthURL(c) {
			c.JSON(http.StatusNotFound, gin.H{"error": "provider_not_found"})
		}
	}
}

// ImportOAuthV8 dispatches credential import using the provider query parameter.
func (h *Handler) ImportOAuthV8(c *gin.Context) {
	switch strings.ToLower(strings.TrimSpace(c.Query("provider"))) {
	case "":
		c.JSON(http.StatusBadRequest, gin.H{"error": "provider is required"})
	case "vertex":
		h.ImportVertexCredential(c)
	default:
		c.JSON(http.StatusNotFound, gin.H{"error": "provider_not_found"})
	}
}

// CancelAuthSession invalidates a pending database-backed login session.
func (h *Handler) CancelAuthSession(c *gin.Context) {
	state := strings.TrimSpace(c.Query("state"))
	if state == "" {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "missing state"})
		return
	}
	if errState := validateOAuthState(state); errState != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "invalid state"})
		return
	}
	ctx, cancel := h.requestContext(c)
	defer cancel()
	cancelled, errCancel := h.repo.CancelOAuthSession(ctx, state)
	if errCancel != nil {
		respondError(c, http.StatusInternalServerError, "oauth_session_failed", errCancel)
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "cancelled": cancelled})
}
