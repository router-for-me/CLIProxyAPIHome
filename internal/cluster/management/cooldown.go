package management

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"gorm.io/gorm"
)

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
