package managementhttp

import (
	"net/http"
	"strings"

	cpasdkapi "github.com/router-for-me/CLIProxyAPI/v8/sdk/api"
	clustermanagement "github.com/router-for-me/CLIProxyAPIHome/internal/cluster/management"
)

// managementV8Routes maps operational routes to the same authoritative handlers
// as v0, while configuration uses the separate v8 tree contract.
func managementV8Routes(legacy *RouteRegistry, sdk *cpasdkapi.Handler, home *clustermanagement.Handler) *RouteRegistry {
	routes := newRouteRegistry()
	paths := map[string]string{
		"/latest-version":             "/server/latest-version",
		"/api-call":                   "/requests/api-call",
		"/model-definitions/:channel": "/routing/model-definitions/:channel",
		"/logs":                       "/observability/logs",
		"/request-error-logs":         "/observability/logs/errors",
		"/request-error-logs/:name":   "/observability/logs/errors/:name",
		"/request-log-by-id/:id":      "/observability/logs/requests/:id",
		"/api-key-usage":              "/observability/usage/api-keys",
		"/usage-queue":                "/observability/usage/queue",
		"/auth-files":                 "/credentials",
		"/auth-files/models":          "/credentials/models",
		"/auth-files/model-states":    "/credentials/model-states",
		"/auth-files/download":        "/credentials/download",
		"/auth-files/status":          "/credentials/status",
		"/auth-files/fields":          "/credentials/fields",
		"/get-auth-status":            "/oauth/status",
		"/plugins":                    "/plugins",
		"/plugin-store":               "/plugins/store",
		"/plugin-store/:id/install":   "/plugins/store/:id/install",
		"/plugin-store/:id/uninstall": "/plugins/store/:id/uninstall",
	}
	for key, handler := range legacy.routes {
		if path, exists := paths[key.Path]; exists {
			routes.Set(key.Method, path, handler)
		}
		if home != nil {
			// Home resources keep their existing names under the new version.
			for _, prefix := range []string{"/nodes", "/topology", "/certificates", "/capabilities", "/credentials/", "/quota/", "/usage/", "/request-events", "/request-logs", "/billing/", "/proxy/", "/users", "/channel-groups", "/channel-group-details", "/model-groups", "/model-group-details", "/plugin-store-auth", "/models"} {
				if key.Path == prefix || strings.HasPrefix(key.Path, strings.TrimSuffix(prefix, "/")+"/") {
					routes.Set(key.Method, key.Path, handler)
					break
				}
			}
			if key.Path == "/api-keys" {
				routes.Set(key.Method, "/access/api-keys", handler)
			}
		}
	}
	configHandler := sdk.ConfigV8
	if home != nil {
		configHandler = home.ConfigV8
	}
	routes.Set(http.MethodGet, "/config", configHandler)
	routes.Set(http.MethodPut, "/config", configHandler)
	routes.Set(http.MethodPatch, "/config", configHandler)
	routes.Set(http.MethodGet, "/config.yaml", configHandler)
	routes.Set(http.MethodPut, "/config.yaml", configHandler)
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		routes.Set(method, "/config/*path", configHandler)
	}
	if home != nil {
		routes.Set(http.MethodGet, "/oauth/auth-url", home.StartOAuthV8)
		routes.Set(http.MethodPost, "/oauth/import", home.ImportOAuthV8)
		routes.Set(http.MethodDelete, "/oauth/session", home.CancelAuthSession)
		routes.Set(http.MethodPost, "/credentials/refresh", home.RefreshAuthFiles)
		routes.Set(http.MethodPost, "/routing/cooldown/reset", home.ResetQuota)
		routes.Set(http.MethodGet, "/credentials/quota/providers", home.GetQuotaProviders)
		routes.Set(http.MethodPost, "/credentials/quota/fetch", home.FetchCredentialQuota)
		routes.Set(http.MethodPost, "/credentials/quota/reset", home.ResetCredentialQuota)
		routes.Set(http.MethodGet, "/plugins/:id/quota", home.GetPluginQuota)
		routes.Set(http.MethodPost, "/plugins/:id/quota", home.FetchPluginQuota)
		routes.Set(http.MethodDelete, "/plugins/:id/quota", home.ResetPluginQuota)
		routes.Set(http.MethodDelete, "/plugins/:id", home.UninstallPluginFromStore)
	} else {
		routes.Set(http.MethodGet, "/oauth/auth-url", sdk.StartOAuthV8)
		routes.Set(http.MethodPost, "/oauth/import", sdk.ImportOAuthV8)
		routes.Set(http.MethodDelete, "/oauth/session", sdk.CancelAuthSession)
		routes.Set(http.MethodPost, "/credentials/refresh", sdk.RefreshAuthFiles)
		routes.Set(http.MethodPost, "/routing/cooldown/reset", sdk.ResetQuota)
		routes.Set(http.MethodGet, "/credentials/quota/providers", sdk.GetQuotaProviders)
		routes.Set(http.MethodPost, "/credentials/quota/fetch", sdk.FetchCredentialQuota)
		routes.Set(http.MethodPost, "/credentials/quota/reset", sdk.ResetCredentialQuota)
		routes.Set(http.MethodGet, "/plugins/:id/quota", sdk.GetPluginQuota)
		routes.Set(http.MethodPost, "/plugins/:id/quota", sdk.FetchPluginQuota)
		routes.Set(http.MethodDelete, "/plugins/:id/quota", sdk.ResetPluginQuota)
		routes.Set(http.MethodDelete, "/plugins/:id", sdk.DeletePlugin)
	}
	return routes
}
