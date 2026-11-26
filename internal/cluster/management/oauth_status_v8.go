package management

import (
	"net/http"

	"github.com/MoeForge/moego/json"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
)

// respondCompletedOAuthStatus includes the saved credential IDs in a completed login result.
func (h *Handler) respondCompletedOAuthStatus(c *gin.Context, session *cluster.OAuthSessionRecord) {
	payload := gin.H{"status": "ok"}
	if len(session.Data) > 0 {
		var result cluster.OAuthSessionResult
		if errUnmarshal := json.Unmarshal(session.Data, &result); errUnmarshal != nil {
			respondError(c, http.StatusInternalServerError, "oauth_session_failed", errUnmarshal)
			return
		}
		if len(result.AuthIDs) > 0 {
			payload["auth_ids"] = result.AuthIDs
			if len(result.AuthIDs) == 1 {
				payload["auth_id"] = result.AuthIDs[0]
			}
		}
	}
	c.JSON(http.StatusOK, payload)
}
