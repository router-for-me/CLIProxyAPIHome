package management

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	cpaconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
)

func TestConfigV8HistoricalProviderSubtrees(t *testing.T) {
	for _, tc := range []struct {
		name, method, body            string
		steering, buffering, optimize bool
		agent                         string
	}{
		{"patch", http.MethodPatch, `{"response-steering":false,"header-defaults":{"user-agent":"updated"}}`, false, true, true, "updated"},
		{"replace", http.MethodPut, `{"response-steering":true}`, true, false, false, ""},
		{"delete", http.MethodDelete, "", false, false, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, cleanup := openManagementLogTestDB(t)
			defer cleanup()
			repo := cluster.NewRepository(db)
			h := NewHandler(repo, nil, "127.0.0.1", 0)
			router := gin.New()
			for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodPatch, http.MethodDelete} {
				router.Handle(method, "/v8/management/config/*path", h.ConfigV8)
			}
			router.GET("/v8/management/config.yaml", h.ConfigV8)
			request := func(method, route, body string) string {
				t.Helper()
				response := httptest.NewRecorder()
				router.ServeHTTP(response, httptest.NewRequest(method, "/v8/management/"+route, strings.NewReader(body)))
				if response.Code != http.StatusOK {
					t.Fatalf("%s %s: %s", method, route, response.Body.String())
				}
				return response.Body.String()
			}
			request(http.MethodPut, "config/upstream", `{"codex":{"response-steering":true,"stream-bootstrap-buffering":true},"xai":{"inject-x-search":true}}`)
			request(http.MethodPut, "config/client/codex", `{"optimize-multi-agent-v2":true}`)
			request(http.MethodPut, "config/oauth/auth-auto-refresh-workers", `3`)
			request(http.MethodPut, "config/oauth/providers/codex/header-defaults", `{"user-agent":"oauth-agent"}`)
			view := request(http.MethodGet, "config/oauth/providers/codex", "")
			if !strings.Contains(view, `"response-steering":true`) || !strings.Contains(view, `"optimize-multi-agent-v2":true`) || !strings.Contains(view, `"user-agent":"oauth-agent"`) {
				t.Fatalf("historical view omitted shared/client/OAuth values: %s", view)
			}
			request(tc.method, "config/oauth/providers/codex", tc.body)
			cfg, wire, err := repo.LoadConfigAsRuntimeConfig(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			cpa, errCPA := cpaconfig.ParseConfigBytes(wire)
			if errCPA != nil {
				t.Fatal(errCPA)
			}
			if cpa.Codex.ResponseSteering != tc.steering || cpa.Codex.StreamBootstrapBuffering != tc.buffering || cpa.Client.Codex.OptimizeMultiAgentV2 != tc.optimize || cpa.CodexHeaderDefaults.UserAgent != tc.agent {
				t.Fatal("historical provider update lost replacement/merge semantics")
			}
			if !cpa.XAI.InjectXSearch || cfg.AuthAutoRefreshWorkers != 3 || (tc.agent != "" && !cfg.OAuthOnlyFields["codex-header-defaults.user-agent"]) {
				t.Fatal("historical provider update changed unrelated values or OAuth scope")
			}
			if err = config.ValidateV8Config([]byte(request(http.MethodGet, "config.yaml", ""))); err != nil {
				t.Fatalf("YAML export retained historical aliases: %v", err)
			}
		})
	}
}
