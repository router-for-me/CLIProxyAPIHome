package managementhttp

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	cpasdkapi "github.com/router-for-me/CLIProxyAPI/v8/sdk/api"
	cpaconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	clustermanagement "github.com/router-for-me/CLIProxyAPIHome/internal/cluster/management"
)

func TestManagementV8Routes(t *testing.T) {
	sdk := cpasdkapi.NewHandlerWithoutConfigFilePath(&cpaconfig.Config{}, nil)
	home := clustermanagement.NewHandler(nil, nil, "", 0)
	legacy := defaultRoutes(sdk)
	registerClusterManagementRoutes(legacy, home)
	engine := gin.New()
	managementV8Routes(legacy, sdk, home).Register(engine.Group("/v8/management"))
	registered := make(map[string]bool)
	for _, route := range engine.Routes() {
		registered[route.Method+" "+route.Path] = true
		if route.Method == http.MethodGet && route.Path == "/v8/management/oauth/status" && !strings.Contains(route.Handler, "GetAuthStatusV8") {
			t.Fatalf("v8 OAuth status must include saved credential IDs, got %s", route.Handler)
		}
	}
	for _, route := range []string{
		"GET /config", "PUT /config", "PATCH /config", "GET /config.yaml", "PUT /config.yaml", "DELETE /config/*path",
		"GET /credentials", "POST /credentials/refresh", "GET /credentials/in-flight", "PATCH /credentials/:credential_id/concurrency-policy",
		"GET /credentials/model-states", "GET /credentials/:credential_id/model-states",
		"GET /oauth/auth-url", "POST /oauth/import", "GET /oauth/status", "DELETE /oauth/session",
		"GET /server/latest-version", "POST /requests/api-call", "POST /routing/cooldown/reset",
		"GET /observability/logs", "GET /observability/usage/api-keys", "GET /plugins/store", "DELETE /plugins/:id",
		"GET /credentials/quota/providers", "POST /credentials/quota/fetch", "POST /credentials/quota/reset",
		"GET /plugins/:id/quota", "POST /plugins/:id/quota", "DELETE /plugins/:id/quota",
		"GET /quota/credentials", "GET /quota/credentials/:credential_id", "POST /quota/collect",
		"GET /users", "GET /nodes", "GET /billing/overview", "GET /access/api-keys",
	} {
		method, path, _ := strings.Cut(route, " ")
		if !registered[method+" /v8/management"+path] {
			t.Errorf("missing %s", route)
		}
	}
	for _, path := range []string{"/debug", "/api-keys", "/codex-api-key", "/auth-files", "/codex-auth-url"} {
		if registered["GET /v8/management"+path] {
			t.Errorf("legacy setting registered in v8: %s", path)
		}
	}
	resp := httptest.NewRecorder()
	engine.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/v8/management/observability/logs?anchor_id=invalid", nil))
	if resp.Code != http.StatusBadRequest || !strings.Contains(resp.Body.String(), "invalid_anchor_id") {
		t.Fatalf("v8 logs did not use the cursor-aware handler: status %d, body %s", resp.Code, resp.Body.String())
	}
}

func TestManagementV8QuotaRoutesRemainAvailable(t *testing.T) {
	for _, database := range []bool{false, true} {
		t.Run(map[bool]string{false: "SDK", true: "database"}[database], func(t *testing.T) {
			sdk := cpasdkapi.NewHandlerWithoutConfigFilePath(&cpaconfig.Config{}, nil)
			legacy := defaultRoutes(sdk)
			var home *clustermanagement.Handler
			if database {
				home = clustermanagement.NewHandler(nil, nil, "", 0)
				registerClusterManagementRoutes(legacy, home)
			}
			engine := gin.New()
			managementV8Routes(legacy, sdk, home).Register(engine.Group("/v8/management"))
			registered := make(map[string]bool)
			for _, route := range engine.Routes() {
				registered[route.Method+" "+route.Path] = true
				if route.Method == http.MethodGet && route.Path == "/v8/management/credentials" {
					wantHandler := "ListAuthFiles"
					if database {
						wantHandler = "ListCredentialsV8"
					}
					if !strings.Contains(route.Handler, wantHandler) {
						t.Fatalf("credential handler = %s, want %s", route.Handler, wantHandler)
					}
				}
				if database && route.Method == http.MethodGet && route.Path == "/v8/management/credentials/models" && !strings.Contains(route.Handler, "GetCredentialModelsV8") {
					t.Fatalf("v8 models route must include exclusion candidates, got %s", route.Handler)
				}
			}
			for _, route := range []string{
				"GET /credentials",
				"GET /credentials/quota/providers", "POST /credentials/quota/fetch", "POST /credentials/quota/reset",
				"GET /plugins/:id/quota", "POST /plugins/:id/quota", "DELETE /plugins/:id/quota",
			} {
				method, path, _ := strings.Cut(route, " ")
				if !registered[method+" /v8/management"+path] {
					t.Fatalf("missing quota route: %s", route)
				}
			}
		})
	}
}

func TestManagementV8AccessControlAndOAuthCallbacks(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	for _, enabled := range []bool{true, false} {
		t.Run(map[bool]string{true: "enabled", false: "disabled"}[enabled], func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.yaml")
			secret := ""
			if enabled {
				secret = "v8-fixture-password"
			}
			content := "port: 8317\nauth-dir: " + filepath.ToSlash(dir) + "\nremote-management: {secret-key: '" + secret + "'}\n"
			if err := os.WriteFile(path, []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			built, err := Build(path)
			if err != nil {
				t.Fatal(err)
			}
			for _, authorized := range []bool{true, false} {
				for _, route := range []string{"/v0/management/config", "/v8/management/config", "/v8/management/config/server/port"} {
					req := httptest.NewRequest("GET", route, nil)
					req.RemoteAddr = "127.0.0.1:1234"
					if authorized {
						req.Header.Set("Authorization", "Bearer v8-fixture-password")
					}
					rec := httptest.NewRecorder()
					built.Engine.ServeHTTP(rec, req)
					want := http.StatusNotFound
					if enabled {
						want = http.StatusUnauthorized
						if authorized {
							want = http.StatusOK
						}
					}
					if rec.Code != want {
						t.Fatalf("%s authorized=%v: %d want %d: %s", route, authorized, rec.Code, want, rec.Body.String())
					}
				}
			}
			rec := httptest.NewRecorder()
			built.Engine.ServeHTTP(rec, httptest.NewRequest("GET", "/v8/management/oauth/callback", nil))
			want := http.StatusNotFound
			if enabled {
				want = http.StatusBadRequest
			}
			if rec.Code != want {
				t.Fatalf("callback should validate state without bearer: %d, want %d", rec.Code, want)
			}
		})
	}
}
