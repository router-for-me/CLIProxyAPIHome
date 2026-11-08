package management

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
)

func TestCooldownResetRoutesPreserveNotFoundAndUnavailableResponses(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const authID = "deleted-cooldown-contract"
	handler, repo := newCooldownManagementHandler(t, &coreauth.Auth{ID: authID, Index: authID, Provider: "codex"})
	engine := gin.New()
	engine.DELETE("/credentials/:credential_id/cooldown", handler.ClearCredentialCooldown)
	engine.POST("/routing/cooldown/reset", handler.ResetQuota)
	if errDelete := repo.SoftDeleteAuth(context.Background(), authID); errDelete != nil {
		t.Fatal(errDelete)
	}
	for _, identifier := range []string{authID, "never-existed"} {
		for _, method := range []string{http.MethodDelete, http.MethodPost} {
			t.Run(method+"/"+identifier, func(t *testing.T) {
				path, body := "/credentials/"+identifier+"/cooldown", ""
				if method == http.MethodPost {
					path, body = "/routing/cooldown/reset", `{"auth_index":"`+identifier+`"}`
				}
				request := httptest.NewRequest(method, path, strings.NewReader(body))
				request.Header.Set("Content-Type", "application/json")
				response := httptest.NewRecorder()
				engine.ServeHTTP(response, request)
				if response.Code != http.StatusNotFound {
					t.Fatalf("missing credential status=%d body=%s, want 404", response.Code, response.Body.String())
				}

				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				canceled := httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(ctx)
				canceled.Header.Set("Content-Type", "application/json")
				unavailable := httptest.NewRecorder()
				engine.ServeHTTP(unavailable, canceled)
				if unavailable.Code != http.StatusServiceUnavailable {
					t.Fatalf("canceled query status=%d body=%s, want 503", unavailable.Code, unavailable.Body.String())
				}
			})
		}
	}
}
