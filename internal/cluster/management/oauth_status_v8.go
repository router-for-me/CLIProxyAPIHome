package management

import (
	"encoding/json"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
)

const oauthStatusV8Key = "home.oauth-status-v8"

// GetAuthStatusV8 includes the saved credential IDs in a completed login result.
func (h *Handler) GetAuthStatusV8(c *gin.Context) {
	c.Set(oauthStatusV8Key, true)
	h.GetAuthStatus(c)
}

func (h *Handler) respondCompletedOAuthStatus(c *gin.Context, session *cluster.OAuthSessionRecord) {
	payload := gin.H{"status": "ok"}
	if c.GetBool(oauthStatusV8Key) && len(session.Data) > 0 {
		var result struct {
			AuthIDs []string `json:"auth_ids"`
		}
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
