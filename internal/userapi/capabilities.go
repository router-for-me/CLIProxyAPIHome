package userapi

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPIHome/internal/buildinfo"
)

// GetCapabilities reports optional User API features without exposing mail configuration.
//
// The manifest spans subsystems on purpose: a client reads it once at startup to
// decide which surfaces to render, so every optional feature belongs here rather
// than in whichever file happens to implement it.
func (h *Handler) GetCapabilities(c *gin.Context) {
	enabled := h.userEmailEnabled()
	c.JSON(http.StatusOK, gin.H{
		"capabilities": gin.H{
			"email_registration": enabled,
			"email_verification": enabled,
			"password_recovery":  enabled,
			// Advertised so a client can hide the catalog on an older Home
			// instead of discovering the route is missing by calling it and
			// handling a 404 as if it were an outage.
			"model_catalog": true,
		},
		"server_info": gin.H{
			"home_version":    buildinfo.Version,
			"home_commit":     buildinfo.Commit,
			"home_build_date": buildinfo.BuildDate,
		},
	})
}
