package management

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"gorm.io/gorm"
)

// GetAntigravityTOSPause reports whether Home is currently excluding the entire pool.
func (h *Handler) GetAntigravityTOSPause(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	if h == nil || h.runtime == nil {
		respondError(c, http.StatusServiceUnavailable, "antigravity_tos_pause_unavailable", nil)
		return
	}
	ctx, cancel := h.requestContext(c)
	defer cancel()
	deadline, errRead := h.runtime.AntigravityTOSPauseDeadline(ctx)
	if errRead != nil {
		respondError(c, http.StatusServiceUnavailable, "antigravity_tos_pause_unavailable", nil)
		return
	}
	observedAt := time.Now().UTC()
	var resumeAt *time.Time
	if deadline.After(observedAt) {
		deadline = deadline.UTC()
		resumeAt = &deadline
	}
	c.JSON(http.StatusOK, gin.H{"paused": resumeAt != nil, "resume_at": resumeAt, "observed_at": observedAt})
}

// ClearCredentialCooldown clears Home-owned quota cooldown state for one
// credential, optionally limited to one model.
func (h *Handler) ClearCredentialCooldown(c *gin.Context) {
	credentialID := strings.TrimSpace(c.Param("credential_id"))
	if credentialID == "" {
		respondError(c, http.StatusBadRequest, "credential_id_required", errors.New("credential_id is required"))
		return
	}

	rawModel, modelProvided := c.GetQuery("model")
	model := strings.TrimSpace(rawModel)
	if modelProvided && model == "" {
		respondError(c, http.StatusBadRequest, "model_required", errors.New("model must not be empty when provided"))
		return
	}
	if h == nil || h.runtime == nil || h.runtime.CoreManager() == nil {
		respondError(c, http.StatusServiceUnavailable, "cooldown_reset_unavailable", coreauth.ErrCooldownMutationUnsupported)
		return
	}

	ctx, cancel := h.requestContext(c)
	defer cancel()
	result, errClear := h.runtime.CoreManager().ClearQuotaCooldown(ctx, credentialID, model)
	if errClear != nil {
		status := quotaReadErrorStatus(errClear)
		switch {
		case errors.Is(errClear, gorm.ErrRecordNotFound):
			respondError(c, http.StatusNotFound, "credential_not_found", errClear)
		case errors.Is(errClear, coreauth.ErrCooldownMutationUnsupported), status == http.StatusServiceUnavailable:
			respondError(c, http.StatusServiceUnavailable, "cooldown_reset_unavailable", errClear)
		default:
			respondError(c, http.StatusInternalServerError, "cooldown_reset_failed", errClear)
		}
		return
	}

	scope := "all"
	response := gin.H{
		"status":         "ok",
		"credential_id":  result.CredentialID,
		"scope":          scope,
		"cleared":        result.Cleared,
		"cleared_models": append([]string{}, result.ClearedModels...),
	}
	if modelProvided {
		response["scope"] = "model"
		response["model"] = result.Model
	}
	c.JSON(http.StatusOK, response)
}
