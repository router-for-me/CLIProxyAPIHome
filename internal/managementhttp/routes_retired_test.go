package managementhttp

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/home"
	"gopkg.in/yaml.v3"
)

func TestRetiredManagementRoutesUnavailable(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	for _, database := range []bool{false, true} {
		for _, enabled := range []bool{false, true} {
			t.Run(fmt.Sprintf("database=%t/enabled=%t", database, enabled), func(t *testing.T) {
				dir := t.TempDir()
				path := filepath.Join(dir, "config.yaml")
				secret := ""
				if enabled {
					normalized, _, errNormalize := config.NormalizeRemoteManagementSecret("retirement-fixture-password")
					if errNormalize != nil {
						t.Fatal(errNormalize)
					}
					secret = normalized
				}
				content := fmt.Sprintf("auth-dir: %s\nremote-management: {secret-key: '%s'}\n", filepath.ToSlash(dir), secret)
				var opts []RouteOption
				if database {
					cfg := &config.Config{}
					if errDecode := yaml.Unmarshal([]byte(content), cfg); errDecode != nil {
						t.Fatal(errDecode)
					}
					runtime, errRuntime := home.NewRuntime(cfg)
					if errRuntime != nil {
						t.Fatal(errRuntime)
					}
					// These requests only exercise routing, authentication, and input validation.
					opts = append(opts, WithDatabaseManagement(DatabaseManagementOption{
						Enabled: true, Repository: cluster.NewRepository(nil), Runtime: runtime,
					}))
				} else {
					if errWrite := os.WriteFile(path, []byte(content), 0600); errWrite != nil {
						t.Fatal(errWrite)
					}
				}
				built, errBuild := Build(path, opts...)
				if errBuild != nil {
					t.Fatal(errBuild)
				}
				for _, route := range built.Engine.Routes() {
					if route.Path == "/v0/management" || strings.HasPrefix(route.Path, "/v0/management/") {
						t.Fatalf("retired route remains registered: %s %s", route.Method, route.Path)
					}
				}
				for _, authorized := range []bool{false, true} {
					for _, suffix := range []string{"", "/", "/config", "/config.yaml", "/debug", "/codex-api-key", "/users", "/auth-files", "/codex-auth-url", "/plugin-fixture-auth-url", "/oauth-callback", "/missing"} {
						for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodHead} {
							request := httptest.NewRequest(method, "/v0/management"+suffix, nil)
							request.RemoteAddr = "127.0.0.1:1234"
							if authorized {
								request.Header.Set("Authorization", "Bearer retirement-fixture-password")
							}
							response := httptest.NewRecorder()
							built.Engine.ServeHTTP(response, request)
							if response.Code != http.StatusNotFound {
								t.Fatalf("%s %s authorized=%t: status=%d, want 404", method, request.URL.Path, authorized, response.Code)
							}
						}
					}
					request := httptest.NewRequest(http.MethodGet, "/v8/management/oauth/auth-url", nil)
					request.RemoteAddr = "127.0.0.1:1234"
					if authorized {
						request.Header.Set("Authorization", "Bearer retirement-fixture-password")
					}
					response := httptest.NewRecorder()
					built.Engine.ServeHTTP(response, request)
					want := http.StatusNotFound
					if enabled {
						want = http.StatusUnauthorized
						if authorized {
							want = http.StatusBadRequest
						}
					}
					if response.Code != want {
						t.Fatalf("v8 authorized=%t: status=%d, want %d: %s", authorized, response.Code, want, response.Body.String())
					}
				}
				for _, method := range []string{http.MethodGet, http.MethodPost} {
					response := httptest.NewRecorder()
					built.Engine.ServeHTTP(response, httptest.NewRequest(method, "/v8/management/oauth/callback", nil))
					want := http.StatusNotFound
					if enabled {
						want = http.StatusBadRequest
					}
					if response.Code != want {
						t.Fatalf("%s callback: status=%d, want %d: %s", method, response.Code, want, response.Body.String())
					}
				}
			})
		}
	}
}
