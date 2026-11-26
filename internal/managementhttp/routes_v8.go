package managementhttp

import (
	"net/http"

	cpasdkapi "github.com/router-for-me/CLIProxyAPI/v8/sdk/api"
	clustermanagement "github.com/router-for-me/CLIProxyAPIHome/internal/cluster/management"
)

// managementV8Routes adds V8 configuration and provider operations to the resource registry.
func managementV8Routes(routes *RouteRegistry, sdk *cpasdkapi.Handler, home *clustermanagement.Handler) *RouteRegistry {
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
