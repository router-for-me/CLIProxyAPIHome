package managementhttp

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	cpasdkapi "github.com/router-for-me/CLIProxyAPI/v7/sdk/api"
	cpasdkauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/auth"
	cpacoreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cpaconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/buildinfo"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	clustermanagement "github.com/router-for-me/CLIProxyAPIHome/internal/cluster/management"
	appconfig "github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/home"
	"github.com/router-for-me/CLIProxyAPIHome/internal/managementasset"
	mgmthandlers "github.com/router-for-me/CLIProxyAPIHome/internal/managementhttp/hanlders"
	"github.com/router-for-me/CLIProxyAPIHome/internal/userapi"
	"github.com/router-for-me/CLIProxyAPIHome/internal/util"
	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
)

type RouteKey struct {
	Method string
	Path   string
}

type RouteRegistry struct {
	routes            map[RouteKey]gin.HandlerFunc
	clusterManagement *ClusterManagementOption
}

// newRouteRegistry creates a route registry.
func newRouteRegistry() *RouteRegistry {
	return &RouteRegistry{
		routes: make(map[RouteKey]gin.HandlerFunc),
	}
}

// Set updates set.
func (r *RouteRegistry) Set(method, path string, handler gin.HandlerFunc) {
	if r == nil || handler == nil {
		return
	}
	method = strings.ToUpper(strings.TrimSpace(method))
	path = strings.TrimSpace(path)
	if method == "" || path == "" {
		return
	}
	r.routes[RouteKey{Method: method, Path: path}] = handler
}

// Register wires package handlers into the provided registry.
func (r *RouteRegistry) Register(group *gin.RouterGroup) {
	if r == nil || group == nil || len(r.routes) == 0 {
		return
	}
	keys := make([]RouteKey, 0, len(r.routes))
	for k := range r.routes {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		iWildcard := routePathHasWildcard(keys[i].Path)
		jWildcard := routePathHasWildcard(keys[j].Path)
		if iWildcard != jWildcard {
			return !iWildcard
		}
		if keys[i].Path == keys[j].Path {
			return keys[i].Method < keys[j].Method
		}
		return keys[i].Path < keys[j].Path
	})
	for _, k := range keys {
		h := r.routes[k]
		if h == nil {
			continue
		}
		group.Handle(k.Method, k.Path, h)
	}
}

func routePathHasWildcard(path string) bool {
	for _, segment := range strings.Split(path, "/") {
		if strings.HasPrefix(segment, ":") || strings.HasPrefix(segment, "*") {
			return true
		}
	}
	return false
}

type RouteOption func(*RouteRegistry)

type ClusterManagementOption struct {
	Enabled          bool
	Repository       *cluster.Repository
	Runtime          *home.Runtime
	NodeIP           string
	NodePort         int
	HeartbeatTimeout time.Duration
	ForwardTLSConfig *tls.Config
	QuotaRecollect   clustermanagement.QuotaRecollectTrigger
}

type DatabaseManagementOption = ClusterManagementOption

var corsExposedResponseHeaders = []string{
	"X-CPA-VERSION",
	"X-CPA-COMMIT",
	"X-CPA-BUILD-DATE",
	"X-CPA-SUPPORT-PLUGIN",
	"X-CPA-HOME-VERSION",
	"X-CPA-HOME-COMMIT",
	"X-CPA-HOME-BUILD-DATE",
	"X-SERVER-VERSION",
	"X-SERVER-BUILD-DATE",
}

var corsExposedResponseHeadersJoined = strings.Join(corsExposedResponseHeaders, ", ")

// WithDatabaseManagement applies the database-backed management option.
func WithDatabaseManagement(opt DatabaseManagementOption) RouteOption {
	return func(r *RouteRegistry) {
		if r == nil || !opt.Enabled || opt.Repository == nil || opt.Runtime == nil {
			return
		}
		optCopy := opt
		r.clusterManagement = &optCopy
		handler := clustermanagement.NewHandler(opt.Repository, opt.Runtime, opt.NodeIP, opt.NodePort)
		handler.SetHeartbeatTimeout(opt.HeartbeatTimeout)
		handler.SetForwardTLSConfig(opt.ForwardTLSConfig)
		handler.SetQuotaRecollectTrigger(opt.QuotaRecollect)
		registerClusterManagementRoutes(r, handler)
	}
}

// registerClusterManagementRoutes handles a register cluster management routes.
func registerClusterManagementRoutes(r *RouteRegistry, handler *clustermanagement.Handler) {
	// Validate request inputs before mutating persisted state.
	if r == nil || handler == nil {
		return
	}

	r.Set(http.MethodGet, "/nodes", handler.ListNodes)
	r.Set(http.MethodGet, "/topology", handler.GetTopology)
	r.Set(http.MethodGet, "/latest-version", handler.GetLatestVersion)
	r.Set(http.MethodPost, "/certificates/clients", handler.CreateClientCertificate)
	r.Set(http.MethodGet, "/config", handler.GetConfig)
	r.Set(http.MethodGet, "/config.yaml", handler.GetConfigYAML)
	r.Set(http.MethodPut, "/config.yaml", handler.PutConfigYAML)
	r.Set(http.MethodGet, "/plugins", handler.ListPlugins)
	r.Set(http.MethodGet, "/plugin-store", handler.ListPluginStore)
	r.Set(http.MethodPost, "/plugin-store/:id/install", handler.InstallPluginFromStore)
	r.Set(http.MethodPost, "/plugin-store/:id/uninstall", handler.UninstallPluginFromStore)
	r.Set(http.MethodGet, "/plugin-store-auth", handler.ListPluginStoreAuth)
	r.Set(http.MethodPost, "/plugin-store-auth", handler.CreatePluginStoreAuth)
	r.Set(http.MethodGet, "/plugin-store-auth/:id", handler.GetPluginStoreAuth)
	r.Set(http.MethodPatch, "/plugin-store-auth/:id", handler.UpdatePluginStoreAuth)
	r.Set(http.MethodDelete, "/plugin-store-auth/:id", handler.DeletePluginStoreAuth)

	r.Set(http.MethodGet, "/debug", handler.GetDebug)
	r.Set(http.MethodPut, "/debug", handler.PutDebug)
	r.Set(http.MethodPatch, "/debug", handler.PutDebug)
	r.Set(http.MethodGet, "/logging-to-file", handler.GetLoggingToFile)
	r.Set(http.MethodPut, "/logging-to-file", handler.PutLoggingToFile)
	r.Set(http.MethodPatch, "/logging-to-file", handler.PutLoggingToFile)
	r.Set(http.MethodGet, "/logs", handler.GetLogs)
	r.Set(http.MethodDelete, "/logs", handler.DeleteLogs)
	r.Set(http.MethodGet, "/logs-max-total-size-mb", handler.GetLogsMaxTotalSizeMB)
	r.Set(http.MethodPut, "/logs-max-total-size-mb", handler.PutLogsMaxTotalSizeMB)
	r.Set(http.MethodPatch, "/logs-max-total-size-mb", handler.PutLogsMaxTotalSizeMB)
	r.Set(http.MethodGet, "/error-logs-max-files", handler.GetErrorLogsMaxFiles)
	r.Set(http.MethodPut, "/error-logs-max-files", handler.PutErrorLogsMaxFiles)
	r.Set(http.MethodPatch, "/error-logs-max-files", handler.PutErrorLogsMaxFiles)
	r.Set(http.MethodGet, "/usage-statistics-enabled", handler.GetUsageStatisticsEnabled)
	r.Set(http.MethodPut, "/usage-statistics-enabled", handler.PutUsageStatisticsEnabled)
	r.Set(http.MethodPatch, "/usage-statistics-enabled", handler.PutUsageStatisticsEnabled)
	r.Set(http.MethodGet, "/proxy-url", handler.GetProxyURL)
	r.Set(http.MethodPut, "/proxy-url", handler.PutProxyURL)
	r.Set(http.MethodPatch, "/proxy-url", handler.PutProxyURL)
	r.Set(http.MethodDelete, "/proxy-url", handler.DeleteProxyURL)

	r.Set(http.MethodGet, "/quota-exceeded/switch-project", handler.GetSwitchProject)
	r.Set(http.MethodPut, "/quota-exceeded/switch-project", handler.PutSwitchProject)
	r.Set(http.MethodPatch, "/quota-exceeded/switch-project", handler.PutSwitchProject)
	r.Set(http.MethodGet, "/quota-exceeded/switch-preview-model", handler.GetSwitchPreviewModel)
	r.Set(http.MethodPut, "/quota-exceeded/switch-preview-model", handler.PutSwitchPreviewModel)
	r.Set(http.MethodPatch, "/quota-exceeded/switch-preview-model", handler.PutSwitchPreviewModel)

	r.Set(http.MethodGet, "/api-keys", handler.GetAPIKeys)
	r.Set(http.MethodPost, "/api-keys", handler.PostAPIKeys)
	r.Set(http.MethodPut, "/api-keys", handler.PutAPIKeys)
	r.Set(http.MethodPatch, "/api-keys", handler.PatchAPIKeys)
	r.Set(http.MethodDelete, "/api-keys", handler.DeleteAPIKeys)
	r.Set(http.MethodGet, "/api-key-usage", handler.GetAPIKeyUsage)
	r.Set(http.MethodGet, "/capabilities", handler.GetCapabilities)
	r.Set(http.MethodGet, "/credentials/in-flight", handler.GetInFlightDetails)
	r.Set(http.MethodGet, "/credentials/in-flight/summary", handler.GetInFlightSummary)
	r.Set(http.MethodGet, "/credentials/concurrency-policies", handler.ListCredentialConcurrencyPolicies)
	r.Set(http.MethodGet, "/credentials/concurrency", handler.GetCredentialConcurrency)
	r.Set(http.MethodGet, "/credentials/:credential_id/concurrency-policy", handler.GetCredentialConcurrencyPolicy)
	r.Set(http.MethodPatch, "/credentials/:credential_id/concurrency-policy", handler.PatchCredentialConcurrencyPolicy)
	r.Set(http.MethodDelete, "/credentials/:credential_id/cooldown", handler.ClearCredentialCooldown)
	r.Set(http.MethodGet, "/quota/credentials", handler.ListQuotaCredentials)
	r.Set(http.MethodGet, "/quota/credentials/:credential_id", handler.GetQuotaCredential)
	r.Set(http.MethodPost, "/quota/collect", handler.CollectQuota)
	r.Set(http.MethodGet, "/usage/overview", handler.GetUsageOverview)
	r.Set(http.MethodGet, "/usage/records", handler.ListUsageRecords)
	r.Set(http.MethodGet, "/usage/records/:id", handler.GetUsageRecord)
	r.Set(http.MethodGet, "/usage/aggregates", handler.ListUsageAggregates)
	r.Set(http.MethodGet, "/usage/export", handler.ExportUsageRecords)
	r.Set(http.MethodGet, "/usage/realtime", handler.GetUsageRealtime)
	r.Set(http.MethodGet, "/usage/health/providers", handler.GetUsageProviderHealth)
	r.Set(http.MethodGet, "/usage/health/credentials", handler.GetUsageCredentialHealth)
	r.Set(http.MethodGet, "/request-events", handler.ListRequestEvents)
	r.Set(http.MethodGet, "/request-events/export", handler.ExportRequestEvents)
	r.Set(http.MethodGet, "/request-events/filter-options", handler.GetRequestEventFilterOptions)
	r.Set(http.MethodGet, "/request-events/:id", handler.GetRequestEvent)
	r.Set(http.MethodGet, "/request-logs", handler.ListRequestLogs)
	r.Set(http.MethodGet, "/billing/overview", handler.GetBillingOverview)
	r.Set(http.MethodGet, "/billing/charges", handler.ListBillingCharges)
	r.Set(http.MethodGet, "/billing/balance-records", handler.ListBillingBalanceRecords)
	r.Set(http.MethodGet, "/billing/model-prices", handler.ListBillingModelPrices)
	r.Set(http.MethodPost, "/billing/model-prices/import/preview", handler.PreviewBillingModelPriceImport)
	r.Set(http.MethodPost, "/billing/model-prices/import/apply", handler.ApplyBillingModelPriceImport)
	r.Set(http.MethodGet, "/billing/model-prices/import/operations/:id", handler.GetBillingModelPriceImportOperation)
	r.Set(http.MethodGet, "/billing/settings", handler.GetBillingSettings)
	r.Set(http.MethodGet, "/billing/settings/diagnostics", handler.GetBillingTierDiagnostics)
	r.Set(http.MethodPatch, "/billing/settings", handler.UpdateBillingSettings)
	r.Set(http.MethodGet, "/proxy/proxy-pools", handler.ListProxyPoolItems)
	r.Set(http.MethodPost, "/billing/balance-records/recharge", handler.RechargeBillingBalance)
	r.Set(http.MethodPost, "/billing/balance-records/deduct", handler.DeductBillingBalance)
	r.Set(http.MethodPost, "/billing/model-prices", handler.CreateBillingModelPrice)
	r.Set(http.MethodPatch, "/billing/model-prices/:id", handler.UpdateBillingModelPrice)
	r.Set(http.MethodDelete, "/billing/model-prices/:id", handler.DeleteBillingModelPrice)
	r.Set(http.MethodPost, "/proxy/proxy-pools", handler.CreateProxyPoolItem)
	r.Set(http.MethodPatch, "/proxy/proxy-pools/:id", handler.UpdateProxyPoolItem)
	r.Set(http.MethodDelete, "/proxy/proxy-pools/:id", handler.DeleteProxyPoolItem)
	r.Set(http.MethodPost, "/proxy/proxy-pools/:id/test", handler.TestProxyPoolItem)
	r.Set(http.MethodGet, "/users", handler.ListUsers)
	r.Set(http.MethodPost, "/users", handler.CreateUser)
	r.Set(http.MethodGet, "/users/:id", handler.GetUser)
	r.Set(http.MethodPut, "/users/:id", handler.UpdateUser)
	r.Set(http.MethodPatch, "/users/:id", handler.UpdateUser)
	r.Set(http.MethodDelete, "/users/:id", handler.DeleteUser)
	r.Set(http.MethodGet, "/users/:id/period-limits", handler.GetUserPeriodLimits)
	r.Set(http.MethodPost, "/users/:id/period-limits/reset", handler.ResetUserPeriodLimits)
	r.Set(http.MethodGet, "/channel-groups", handler.ListChannelGroups)
	r.Set(http.MethodPost, "/channel-groups", handler.CreateChannelGroup)
	r.Set(http.MethodGet, "/channel-groups/:id", handler.GetChannelGroup)
	r.Set(http.MethodPut, "/channel-groups/:id", handler.UpdateChannelGroup)
	r.Set(http.MethodPatch, "/channel-groups/:id", handler.UpdateChannelGroup)
	r.Set(http.MethodDelete, "/channel-groups/:id", handler.DeleteChannelGroup)
	r.Set(http.MethodGet, "/channel-group-details", handler.ListChannelGroupDetails)
	r.Set(http.MethodPost, "/channel-group-details", handler.CreateChannelGroupDetail)
	r.Set(http.MethodGet, "/channel-group-details/:id", handler.GetChannelGroupDetail)
	r.Set(http.MethodPut, "/channel-group-details/:id", handler.UpdateChannelGroupDetail)
	r.Set(http.MethodPatch, "/channel-group-details/:id", handler.UpdateChannelGroupDetail)
	r.Set(http.MethodDelete, "/channel-group-details/:id", handler.DeleteChannelGroupDetail)
	r.Set(http.MethodGet, "/model-groups", handler.ListModelGroups)
	r.Set(http.MethodPost, "/model-groups", handler.CreateModelGroup)
	r.Set(http.MethodGet, "/model-groups/:id", handler.GetModelGroup)
	r.Set(http.MethodPut, "/model-groups/:id", handler.UpdateModelGroup)
	r.Set(http.MethodPatch, "/model-groups/:id", handler.UpdateModelGroup)
	r.Set(http.MethodDelete, "/model-groups/:id", handler.DeleteModelGroup)
	r.Set(http.MethodGet, "/model-group-details", handler.ListModelGroupDetails)
	r.Set(http.MethodPost, "/model-group-details", handler.CreateModelGroupDetail)
	r.Set(http.MethodGet, "/model-group-details/:id", handler.GetModelGroupDetail)
	r.Set(http.MethodPut, "/model-group-details/:id", handler.UpdateModelGroupDetail)
	r.Set(http.MethodPatch, "/model-group-details/:id", handler.UpdateModelGroupDetail)
	r.Set(http.MethodDelete, "/model-group-details/:id", handler.DeleteModelGroupDetail)

	r.Set(http.MethodGet, "/request-log", handler.GetRequestLog)
	r.Set(http.MethodPut, "/request-log", handler.PutRequestLog)
	r.Set(http.MethodPatch, "/request-log", handler.PutRequestLog)
	r.Set(http.MethodGet, "/request-log-by-id/:id", handler.DownloadRequestLogByID)
	r.Set(http.MethodGet, "/request-retry", handler.GetRequestRetry)
	r.Set(http.MethodPut, "/request-retry", handler.PutRequestRetry)
	r.Set(http.MethodPatch, "/request-retry", handler.PutRequestRetry)
	r.Set(http.MethodGet, "/max-retry-interval", handler.GetMaxRetryInterval)
	r.Set(http.MethodPut, "/max-retry-interval", handler.PutMaxRetryInterval)
	r.Set(http.MethodPatch, "/max-retry-interval", handler.PutMaxRetryInterval)
	r.Set(http.MethodGet, "/force-model-prefix", handler.GetForceModelPrefix)
	r.Set(http.MethodPut, "/force-model-prefix", handler.PutForceModelPrefix)
	r.Set(http.MethodPatch, "/force-model-prefix", handler.PutForceModelPrefix)
	r.Set(http.MethodGet, "/routing/strategy", handler.GetRoutingStrategy)
	r.Set(http.MethodPut, "/routing/strategy", handler.PutRoutingStrategy)
	r.Set(http.MethodPatch, "/routing/strategy", handler.PutRoutingStrategy)

	r.Set(http.MethodGet, "/gemini-api-key", handler.GetGeminiKeys)
	r.Set(http.MethodPut, "/gemini-api-key", handler.PutGeminiKeys)
	r.Set(http.MethodPatch, "/gemini-api-key", handler.PatchGeminiKey)
	r.Set(http.MethodDelete, "/gemini-api-key", handler.DeleteGeminiKey)
	r.Set(http.MethodGet, "/vertex-api-key", handler.GetVertexCompatKeys)
	r.Set(http.MethodPut, "/vertex-api-key", handler.PutVertexCompatKeys)
	r.Set(http.MethodPatch, "/vertex-api-key", handler.PatchVertexCompatKey)
	r.Set(http.MethodDelete, "/vertex-api-key", handler.DeleteVertexCompatKey)
	r.Set(http.MethodGet, "/codex-api-key", handler.GetCodexKeys)
	r.Set(http.MethodPut, "/codex-api-key", handler.PutCodexKeys)
	r.Set(http.MethodPatch, "/codex-api-key", handler.PatchCodexKey)
	r.Set(http.MethodDelete, "/codex-api-key", handler.DeleteCodexKey)
	r.Set(http.MethodGet, "/xai-api-key", handler.GetXAIKeys)
	r.Set(http.MethodPut, "/xai-api-key", handler.PutXAIKeys)
	r.Set(http.MethodPatch, "/xai-api-key", handler.PatchXAIKey)
	r.Set(http.MethodDelete, "/xai-api-key", handler.DeleteXAIKey)
	r.Set(http.MethodGet, "/claude-api-key", handler.GetClaudeKeys)
	r.Set(http.MethodPut, "/claude-api-key", handler.PutClaudeKeys)
	r.Set(http.MethodPatch, "/claude-api-key", handler.PatchClaudeKey)
	r.Set(http.MethodDelete, "/claude-api-key", handler.DeleteClaudeKey)
	r.Set(http.MethodGet, "/openai-compatibility", handler.GetOpenAICompat)
	r.Set(http.MethodPut, "/openai-compatibility", handler.PutOpenAICompat)
	r.Set(http.MethodPatch, "/openai-compatibility", handler.PatchOpenAICompat)
	r.Set(http.MethodDelete, "/openai-compatibility", handler.DeleteOpenAICompat)

	r.Set(http.MethodGet, "/oauth-excluded-models", handler.GetOAuthExcludedModels)
	r.Set(http.MethodPut, "/oauth-excluded-models", handler.PutOAuthExcludedModels)
	r.Set(http.MethodPatch, "/oauth-excluded-models", handler.PatchOAuthExcludedModels)
	r.Set(http.MethodDelete, "/oauth-excluded-models", handler.DeleteOAuthExcludedModels)
	r.Set(http.MethodGet, "/oauth-model-alias", handler.GetOAuthModelAlias)
	r.Set(http.MethodPut, "/oauth-model-alias", handler.PutOAuthModelAlias)
	r.Set(http.MethodPatch, "/oauth-model-alias", handler.PatchOAuthModelAlias)
	r.Set(http.MethodDelete, "/oauth-model-alias", handler.DeleteOAuthModelAlias)
	r.Set(http.MethodGet, "/payload", handler.GetConfigRoot("/payload"))
	r.Set(http.MethodPut, "/payload", handler.PutConfigRoot("/payload"))
	r.Set(http.MethodPatch, "/payload", handler.PatchConfigRoot("/payload"))
	r.Set(http.MethodDelete, "/payload", handler.DeleteConfigRoot("/payload"))

	r.Set(http.MethodGet, "/auth-files", handler.ListAuthFiles)
	r.Set(http.MethodGet, "/auth-files/models", handler.GetAuthFileModels)
	r.Set(http.MethodGet, "/auth-files/download", handler.DownloadAuthFile)
	r.Set(http.MethodPost, "/auth-files", handler.UploadAuthFile)
	r.Set(http.MethodDelete, "/auth-files", handler.DeleteAuthFile)
	r.Set(http.MethodPatch, "/auth-files/status", handler.PatchAuthFileStatus)
	r.Set(http.MethodPatch, "/auth-files/fields", handler.PatchAuthFileFields)
	r.Set(http.MethodGet, "/models", handler.GetModels)
	r.Set(http.MethodGet, "/model-definitions/:channel", handler.GetStaticModelDefinitions)
	r.Set(http.MethodGet, "/anthropic-auth-url", handler.RequestAnthropicToken)
	r.Set(http.MethodGet, "/antigravity-auth-url", handler.RequestAntigravityToken)
	r.Set(http.MethodGet, "/codex-auth-url", handler.RequestCodexToken)
	r.Set(http.MethodGet, "/kimi-auth-url", handler.RequestKimiToken)
	r.Set(http.MethodGet, "/xai-auth-url", handler.RequestXAIToken)
	r.Set(http.MethodGet, "/get-auth-status", handler.GetAuthStatus)
	r.Set(http.MethodPost, "/vertex/import", handler.ImportVertexCredential)
	r.Set(http.MethodPost, "/api-call", handler.APICall)
	r.Set(http.MethodPost, "/oauth-callback", handler.PostOAuthCallback)
}

type BuildResult struct {
	Engine      *gin.Engine
	Handler     *cpasdkapi.Handler
	AuthManager *cpacoreauth.Manager
}

// serveManagementControlPanel serves an embedded control panel asset.
func serveManagementControlPanel(cfg *cpaconfig.Config, configFilePath string, assetName string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c == nil {
			return
		}
		if cfg == nil || cfg.RemoteManagement.DisableControlPanel {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		file, errOpen := managementasset.OpenPanelAsset(configFilePath, assetName)
		if errOpen != nil {
			abortManagementAssetOpenError(c, errOpen, "failed to open management control panel asset")
			return
		}

		serveManagementAssetFile(c, file, "no-cache")
	}
}

// serveManagementControlPanelAsset serves hashed static assets referenced by the embedded panel HTML.
func serveManagementControlPanelAsset(cfg *cpaconfig.Config, configFilePath string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c == nil {
			return
		}
		if cfg == nil || cfg.RemoteManagement.DisableControlPanel {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}

		file, errOpen := managementasset.OpenStaticAsset(configFilePath, c.Param("filepath"))
		if errOpen != nil {
			abortManagementAssetOpenError(c, errOpen, "failed to open management control panel static asset")
			return
		}

		serveManagementAssetFile(c, file, "public, max-age=31536000, immutable")
	}
}

func abortManagementAssetOpenError(c *gin.Context, err error, message string) {
	if c == nil {
		return
	}
	if errors.Is(err, fs.ErrNotExist) || os.IsNotExist(err) {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	log.WithError(err).Error(message)
	c.AbortWithStatus(http.StatusInternalServerError)
}

func serveManagementAssetFile(c *gin.Context, file fs.File, cacheControl string) {
	if c == nil || file == nil {
		return
	}
	defer func() {
		if errClose := file.Close(); errClose != nil {
			log.WithError(errClose).Warn("failed to close management control panel asset")
		}
	}()

	fileInfo, errStat := file.Stat()
	if errStat != nil {
		log.WithError(errStat).Error("failed to stat management control panel asset")
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}

	c.Header("Cache-Control", cacheControl)
	if readSeeker, ok := file.(io.ReadSeeker); ok {
		http.ServeContent(c.Writer, c.Request, fileInfo.Name(), fileInfo.ModTime(), readSeeker)
		return
	}

	data, errRead := io.ReadAll(file)
	if errRead != nil {
		log.WithError(errRead).Error("failed to read management control panel asset")
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}

	contentType := mime.TypeByExtension(path.Ext(fileInfo.Name()))
	if contentType == "" {
		contentType = http.DetectContentType(data)
	}
	c.Data(http.StatusOK, contentType, data)
}

// serveManagementControlPanelFromRuntime derives embedded control panel serving from runtime.
func serveManagementControlPanelFromRuntime(opt *ClusterManagementOption, configFilePath string, assetName string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if opt == nil || !opt.Enabled || opt.Runtime == nil {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		handler := serveManagementControlPanel(cpaConfigFromHomeConfig(opt.Runtime.Config()), configFilePath, assetName)
		handler(c)
	}
}

// serveManagementControlPanelAssetFromRuntime derives embedded asset serving from runtime.
func serveManagementControlPanelAssetFromRuntime(opt *ClusterManagementOption, configFilePath string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if opt == nil || !opt.Enabled || opt.Runtime == nil {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		handler := serveManagementControlPanelAsset(cpaConfigFromHomeConfig(opt.Runtime.Config()), configFilePath)
		handler(c)
	}
}

// registerManagementControlPanelRoutes registers the embedded control panel HTML assets.
func registerManagementControlPanelRoutes(engine *gin.Engine, handlerFor func(assetName string) gin.HandlerFunc, assetsHandler gin.HandlerFunc) {
	if engine == nil || handlerFor == nil || assetsHandler == nil {
		return
	}
	engine.GET("/", handlerFor(managementasset.IndexFileName))
	engine.GET("/index.html", handlerFor(managementasset.IndexFileName))
	engine.GET("/management.html", handlerFor(managementasset.ManagementFileName))
	engine.GET("/user.html", handlerFor(managementasset.UserFileName))
	engine.GET("/assets/*filepath", assetsHandler)
}

// Build builds a build.
func Build(configFilePath string, opts ...RouteOption) (*BuildResult, error) {
	// Validate request inputs before mutating persisted state.
	configFilePath = strings.TrimSpace(configFilePath)
	if configFilePath == "" {
		return nil, fmt.Errorf("management http: config file path is empty")
	}

	preReg := newRouteRegistry()
	for i := range opts {
		if opts[i] == nil {
			continue
		}
		opts[i](preReg)
	}
	clusterOpt := preReg.clusterManagement
	clusterEnabled := clusterOpt != nil && clusterOpt.Enabled && clusterOpt.Repository != nil && clusterOpt.Runtime != nil

	var cfg *cpaconfig.Config
	if clusterEnabled {
		cfg = cpaConfigFromHomeConfig(clusterOpt.Runtime.Config())
	} else {
		loadedConfig, errLoad := cpaconfig.LoadConfigOptional(configFilePath, false)
		if errLoad != nil {
			return nil, fmt.Errorf("management http: load config: %w", errLoad)
		}
		cfg = loadedConfig
	}
	if cfg == nil {
		cfg = &cpaconfig.Config{}
	}

	if !cfg.Debug {
		gin.SetMode(gin.ReleaseMode)
	}

	resolvedAuthDir, errResolveAuthDir := util.ResolveAuthDir(cfg.AuthDir)
	if errResolveAuthDir != nil {
		return nil, fmt.Errorf("management http: resolve auth dir: %w", errResolveAuthDir)
	}
	if strings.TrimSpace(resolvedAuthDir) != "" {
		cfg.AuthDir = resolvedAuthDir
	}

	tokenStore := newMetadataProxyStore(cpasdkauth.GetTokenStore())
	if dirSetter, ok := tokenStore.(interface{ SetBaseDir(string) }); ok && strings.TrimSpace(cfg.AuthDir) != "" {
		dirSetter.SetBaseDir(cfg.AuthDir)
	}

	authManager := cpacoreauth.NewManager(tokenStore, nil, nil)
	authManager.SetConfig(cfg)
	authManager.SetOAuthModelAlias(cfg.OAuthModelAlias)
	if !clusterEnabled {
		if errAuthLoad := authManager.Load(context.Background()); errAuthLoad != nil {
			return nil, fmt.Errorf("management http: load auths: %w", errAuthLoad)
		}
	}

	handler := cpasdkapi.NewHandler(cfg, configFilePath, authManager)
	handler.SetLogDirectory("logs")

	engine := gin.New()
	var trustedProxies []string
	if clusterEnabled {
		if current := clusterOpt.Runtime.Config(); current != nil {
			proxyConfig := &appconfig.Config{TrustedProxies: append([]string(nil), current.TrustedProxies...)}
			proxyConfig.NormalizeTrustedProxies()
			if errValidate := appconfig.ValidateTrustedProxies(proxyConfig.TrustedProxies); errValidate != nil {
				return nil, fmt.Errorf("management http: configure trusted proxies: %w", errValidate)
			}
			trustedProxies = proxyConfig.TrustedProxies
		}
	}
	if errTrustedProxies := engine.SetTrustedProxies(trustedProxies); errTrustedProxies != nil {
		return nil, fmt.Errorf("management http: configure trusted proxies: %w", errTrustedProxies)
	}
	engine.Use(gin.Recovery())
	engine.Use(corsMiddleware())

	if clusterEnabled {
		registerManagementControlPanelRoutes(engine, func(assetName string) gin.HandlerFunc {
			return serveManagementControlPanelFromRuntime(clusterOpt, configFilePath, assetName)
		}, serveManagementControlPanelAssetFromRuntime(clusterOpt, configFilePath))
	} else {
		registerManagementControlPanelRoutes(engine, func(assetName string) gin.HandlerFunc {
			return serveManagementControlPanel(cfg, configFilePath, assetName)
		}, serveManagementControlPanelAsset(cfg, configFilePath))
	}

	var clusterHandler *clustermanagement.Handler
	if clusterEnabled {
		clusterHandler = clustermanagement.NewHandler(clusterOpt.Repository, clusterOpt.Runtime, clusterOpt.NodeIP, clusterOpt.NodePort)
		clusterHandler.SetHeartbeatTimeout(clusterOpt.HeartbeatTimeout)
		clusterHandler.SetForwardTLSConfig(clusterOpt.ForwardTLSConfig)
		clusterGroup := engine.Group("/v0/cluster")
		clusterGroup.Use(withBuildInfoHeaders(), clusterMTLSMiddleware())
		registerClusterInternalRoutes(clusterGroup, clusterHandler)
	}

	if clusterEnabled {
		userGroup := engine.Group("/user")
		userGroup.Use(withBuildInfoHeaders())
		userapi.Register(userGroup, userapi.NewHandler(clusterOpt.Repository, clusterOpt.Runtime))
	}

	mgmt := engine.Group("/v0/management")

	reg := defaultRoutes(handler)
	for i := range opts {
		if opts[i] == nil {
			continue
		}
		opts[i](reg)
	}
	if clusterEnabled {
		mgmt.Use(
			withBuildInfoHeaders(),
			clusterAvailabilityMiddleware(clusterOpt, handler),
			handler.Middleware(),
		)
	} else {
		mgmt.Use(
			withBuildInfoHeaders(),
			refreshAndAvailabilityMiddleware(configFilePath, handler, authManager, tokenStore),
			handler.Middleware(),
		)
	}
	reg.Register(mgmt)
	if clusterEnabled && clusterHandler != nil {
		engine.NoRoute(clusterManagementNoRoute(clusterOpt, handler, clusterHandler))
	}

	return &BuildResult{
		Engine:      engine,
		Handler:     handler,
		AuthManager: authManager,
	}, nil
}

// registerClusterInternalRoutes wires mTLS-only cluster routes.
func registerClusterInternalRoutes(group *gin.RouterGroup, handler *clustermanagement.Handler) {
	if group == nil || handler == nil {
		return
	}
	group.GET("/request-log-by-id/:id", handler.DownloadLocalRequestLogByID)
}

// corsMiddleware returns a Gin middleware handler that adds CORS headers.
func corsMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Access-Control-Allow-Origin", "*")
		c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		c.Header("Access-Control-Allow-Headers", "*")
		c.Header("Access-Control-Expose-Headers", corsExposedResponseHeadersJoined)

		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}

		c.Next()
	}
}

// withBuildInfoHeaders applies the build info headers option.
func withBuildInfoHeaders() gin.HandlerFunc {
	return func(c *gin.Context) {
		setBuildInfoHeaders(c)
		c.Next()
	}
}

func setBuildInfoHeaders(c *gin.Context) {
	if c == nil {
		return
	}
	c.Writer.Header().Set("X-CPA-HOME-VERSION", buildinfo.Version)
	c.Writer.Header().Set("X-CPA-HOME-COMMIT", buildinfo.Commit)
	c.Writer.Header().Set("X-CPA-HOME-BUILD-DATE", buildinfo.BuildDate)
}

func clusterManagementNoRoute(opt *ClusterManagementOption, handler *cpasdkapi.Handler, clusterHandler *clustermanagement.Handler) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c == nil || c.Request == nil || c.Request.URL == nil {
			if c != nil {
				c.AbortWithStatus(http.StatusNotFound)
			}
			return
		}
		path := c.Request.URL.Path
		if path != "/v0/management" && !strings.HasPrefix(path, "/v0/management/") {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		if opt == nil || !opt.Enabled || opt.Repository == nil || opt.Runtime == nil || clusterHandler == nil {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}

		setBuildInfoHeaders(c)
		cfg := cpaConfigFromHomeConfig(opt.Runtime.Config())
		if cfg == nil {
			cfg = &cpaconfig.Config{}
		}
		envSecret, envSecretSet := os.LookupEnv("MANAGEMENT_PASSWORD")
		hasSecret := strings.TrimSpace(cfg.RemoteManagement.SecretKey) != "" || (envSecretSet && strings.TrimSpace(envSecret) != "")
		if !hasSecret {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		if handler != nil {
			handler.SetConfig(cfg)
			handler.Middleware()(c)
			if c.IsAborted() {
				return
			}
		}
		if c.Request.Method == http.MethodGet && clusterHandler.ServePluginAuthURL(c) {
			c.Abort()
			return
		}
		c.AbortWithStatus(http.StatusNotFound)
	}
}

// refreshAndAvailabilityMiddleware refreshes an and availability middleware.
func refreshAndAvailabilityMiddleware(configFilePath string, handler *cpasdkapi.Handler, authManager *cpacoreauth.Manager, tokenStore any) gin.HandlerFunc {
	// Resolve credential context before calling upstream OAuth services.
	envSecret, envSecretSet := os.LookupEnv("MANAGEMENT_PASSWORD")
	envSecret = strings.TrimSpace(envSecret)
	envManagementSecret := envSecretSet && envSecret != ""

	return func(c *gin.Context) {
		if c == nil {
			return
		}

		cfg, errLoad := cpaconfig.LoadConfigOptional(configFilePath, false)
		if errLoad != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "config_reload_failed", "message": errLoad.Error()})
			return
		}
		if cfg == nil {
			cfg = &cpaconfig.Config{}
		}

		resolvedAuthDir, errResolveAuthDir := util.ResolveAuthDir(cfg.AuthDir)
		if errResolveAuthDir != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "resolve_auth_dir_failed", "message": errResolveAuthDir.Error()})
			return
		}
		if strings.TrimSpace(resolvedAuthDir) != "" {
			cfg.AuthDir = resolvedAuthDir
		}

		hasSecret := strings.TrimSpace(cfg.RemoteManagement.SecretKey) != "" || envManagementSecret
		if !hasSecret {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}

		if handler != nil {
			handler.SetConfig(cfg)
		}
		if authManager != nil {
			authManager.SetConfig(cfg)
			authManager.SetOAuthModelAlias(cfg.OAuthModelAlias)
		}
		if tokenStore != nil {
			if dirSetter, ok := tokenStore.(interface{ SetBaseDir(string) }); ok && strings.TrimSpace(cfg.AuthDir) != "" {
				dirSetter.SetBaseDir(cfg.AuthDir)
			}
		}
		if authManager != nil {
			ctx := c.Request.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			if errAuthLoad := authManager.Load(ctx); errAuthLoad != nil {
				log.WithError(errAuthLoad).Warn("management http: auth manager reload failed")
			}
		}

		c.Next()
	}
}

// clusterAvailabilityMiddleware handles a cluster availability middleware.
func clusterAvailabilityMiddleware(opt *ClusterManagementOption, handler *cpasdkapi.Handler) gin.HandlerFunc {
	// Validate request inputs before mutating persisted state.
	envSecret, envSecretSet := os.LookupEnv("MANAGEMENT_PASSWORD")
	envSecret = strings.TrimSpace(envSecret)
	envManagementSecret := envSecretSet && envSecret != ""

	return func(c *gin.Context) {
		if c == nil {
			return
		}
		if opt == nil || !opt.Enabled || opt.Repository == nil || opt.Runtime == nil {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		cfg := cpaConfigFromHomeConfig(opt.Runtime.Config())
		if cfg == nil {
			cfg = &cpaconfig.Config{}
		}
		hasSecret := strings.TrimSpace(cfg.RemoteManagement.SecretKey) != "" || envManagementSecret
		if !hasSecret {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		if handler != nil {
			handler.SetConfig(cfg)
		}
		c.Next()
	}
}

// clusterMTLSMiddleware allows only verified cluster mTLS peers.
func clusterMTLSMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c == nil || c.Request == nil || c.Request.TLS == nil ||
			len(c.Request.TLS.PeerCertificates) == 0 || len(c.Request.TLS.VerifiedChains) == 0 {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "cluster_mtls_required"})
			return
		}
		c.Next()
	}
}

// cpaConfigFromHomeConfig derives cpa config from home config.
func cpaConfigFromHomeConfig(homeCfg *appconfig.Config) *cpaconfig.Config {
	if homeCfg == nil {
		return nil
	}
	data, errMarshal := yaml.Marshal(homeCfg)
	if errMarshal != nil {
		return &cpaconfig.Config{}
	}
	cfg := &cpaconfig.Config{}
	if errUnmarshal := yaml.Unmarshal(data, cfg); errUnmarshal != nil {
		return &cpaconfig.Config{}
	}
	return cfg
}

// defaultRoutes handles a default routes.
func defaultRoutes(handler *cpasdkapi.Handler) *RouteRegistry {
	// Validate request inputs before mutating persisted state.
	r := newRouteRegistry()
	if handler == nil {
		return r
	}

	r.Set(http.MethodGet, "/nodes", mgmthandlers.ListNodes)
	r.Set(http.MethodGet, "/config", handler.GetConfig)
	r.Set(http.MethodGet, "/config.yaml", handler.GetConfigYAML)
	r.Set(http.MethodPut, "/config.yaml", handler.PutConfigYAML)
	r.Set(http.MethodGet, "/latest-version", handler.GetLatestVersion)

	r.Set(http.MethodGet, "/debug", handler.GetDebug)
	r.Set(http.MethodPut, "/debug", handler.PutDebug)
	r.Set(http.MethodPatch, "/debug", handler.PutDebug)

	r.Set(http.MethodGet, "/logging-to-file", handler.GetLoggingToFile)
	r.Set(http.MethodPut, "/logging-to-file", handler.PutLoggingToFile)
	r.Set(http.MethodPatch, "/logging-to-file", handler.PutLoggingToFile)

	r.Set(http.MethodGet, "/logs-max-total-size-mb", handler.GetLogsMaxTotalSizeMB)
	r.Set(http.MethodPut, "/logs-max-total-size-mb", handler.PutLogsMaxTotalSizeMB)
	r.Set(http.MethodPatch, "/logs-max-total-size-mb", handler.PutLogsMaxTotalSizeMB)

	r.Set(http.MethodGet, "/error-logs-max-files", handler.GetErrorLogsMaxFiles)
	r.Set(http.MethodPut, "/error-logs-max-files", handler.PutErrorLogsMaxFiles)
	r.Set(http.MethodPatch, "/error-logs-max-files", handler.PutErrorLogsMaxFiles)

	r.Set(http.MethodGet, "/usage-statistics-enabled", handler.GetUsageStatisticsEnabled)
	r.Set(http.MethodPut, "/usage-statistics-enabled", handler.PutUsageStatisticsEnabled)
	r.Set(http.MethodPatch, "/usage-statistics-enabled", handler.PutUsageStatisticsEnabled)

	r.Set(http.MethodGet, "/proxy-url", handler.GetProxyURL)
	r.Set(http.MethodPut, "/proxy-url", handler.PutProxyURL)
	r.Set(http.MethodPatch, "/proxy-url", handler.PutProxyURL)
	r.Set(http.MethodDelete, "/proxy-url", handler.DeleteProxyURL)

	r.Set(http.MethodPost, "/api-call", handler.APICall)

	r.Set(http.MethodGet, "/quota-exceeded/switch-project", handler.GetSwitchProject)
	r.Set(http.MethodPut, "/quota-exceeded/switch-project", handler.PutSwitchProject)
	r.Set(http.MethodPatch, "/quota-exceeded/switch-project", handler.PutSwitchProject)

	r.Set(http.MethodGet, "/quota-exceeded/switch-preview-model", handler.GetSwitchPreviewModel)
	r.Set(http.MethodPut, "/quota-exceeded/switch-preview-model", handler.PutSwitchPreviewModel)
	r.Set(http.MethodPatch, "/quota-exceeded/switch-preview-model", handler.PutSwitchPreviewModel)

	r.Set(http.MethodGet, "/api-keys", handler.GetAPIKeys)
	r.Set(http.MethodPut, "/api-keys", handler.PutAPIKeys)
	r.Set(http.MethodPatch, "/api-keys", handler.PatchAPIKeys)
	r.Set(http.MethodDelete, "/api-keys", handler.DeleteAPIKeys)
	r.Set(http.MethodGet, "/api-key-usage", handler.GetAPIKeyUsage)
	r.Set(http.MethodGet, "/usage-queue", handler.GetUsageQueue)

	r.Set(http.MethodGet, "/gemini-api-key", handler.GetGeminiKeys)
	r.Set(http.MethodPut, "/gemini-api-key", handler.PutGeminiKeys)
	r.Set(http.MethodPatch, "/gemini-api-key", handler.PatchGeminiKey)
	r.Set(http.MethodDelete, "/gemini-api-key", handler.DeleteGeminiKey)

	r.Set(http.MethodGet, "/logs", handler.GetLogs)
	r.Set(http.MethodDelete, "/logs", handler.DeleteLogs)
	r.Set(http.MethodGet, "/request-error-logs", handler.GetRequestErrorLogs)
	r.Set(http.MethodGet, "/request-error-logs/:name", handler.DownloadRequestErrorLog)
	r.Set(http.MethodGet, "/request-log-by-id/:id", handler.GetRequestLogByID)
	r.Set(http.MethodGet, "/request-log", handler.GetRequestLog)
	r.Set(http.MethodPut, "/request-log", handler.PutRequestLog)
	r.Set(http.MethodPatch, "/request-log", handler.PutRequestLog)

	r.Set(http.MethodGet, "/request-retry", handler.GetRequestRetry)
	r.Set(http.MethodPut, "/request-retry", handler.PutRequestRetry)
	r.Set(http.MethodPatch, "/request-retry", handler.PutRequestRetry)
	r.Set(http.MethodGet, "/max-retry-interval", handler.GetMaxRetryInterval)
	r.Set(http.MethodPut, "/max-retry-interval", handler.PutMaxRetryInterval)
	r.Set(http.MethodPatch, "/max-retry-interval", handler.PutMaxRetryInterval)

	r.Set(http.MethodGet, "/force-model-prefix", handler.GetForceModelPrefix)
	r.Set(http.MethodPut, "/force-model-prefix", handler.PutForceModelPrefix)
	r.Set(http.MethodPatch, "/force-model-prefix", handler.PutForceModelPrefix)

	r.Set(http.MethodGet, "/routing/strategy", handler.GetRoutingStrategy)
	r.Set(http.MethodPut, "/routing/strategy", handler.PutRoutingStrategy)
	r.Set(http.MethodPatch, "/routing/strategy", handler.PutRoutingStrategy)

	r.Set(http.MethodGet, "/claude-api-key", handler.GetClaudeKeys)
	r.Set(http.MethodPut, "/claude-api-key", handler.PutClaudeKeys)
	r.Set(http.MethodPatch, "/claude-api-key", handler.PatchClaudeKey)
	r.Set(http.MethodDelete, "/claude-api-key", handler.DeleteClaudeKey)

	r.Set(http.MethodGet, "/codex-api-key", handler.GetCodexKeys)
	r.Set(http.MethodPut, "/codex-api-key", handler.PutCodexKeys)
	r.Set(http.MethodPatch, "/codex-api-key", handler.PatchCodexKey)
	r.Set(http.MethodDelete, "/codex-api-key", handler.DeleteCodexKey)

	r.Set(http.MethodGet, "/openai-compatibility", handler.GetOpenAICompat)
	r.Set(http.MethodPut, "/openai-compatibility", handler.PutOpenAICompat)
	r.Set(http.MethodPatch, "/openai-compatibility", handler.PatchOpenAICompat)
	r.Set(http.MethodDelete, "/openai-compatibility", handler.DeleteOpenAICompat)

	r.Set(http.MethodGet, "/vertex-api-key", handler.GetVertexCompatKeys)
	r.Set(http.MethodPut, "/vertex-api-key", handler.PutVertexCompatKeys)
	r.Set(http.MethodPatch, "/vertex-api-key", handler.PatchVertexCompatKey)
	r.Set(http.MethodDelete, "/vertex-api-key", handler.DeleteVertexCompatKey)

	r.Set(http.MethodGet, "/oauth-excluded-models", handler.GetOAuthExcludedModels)
	r.Set(http.MethodPut, "/oauth-excluded-models", handler.PutOAuthExcludedModels)
	r.Set(http.MethodPatch, "/oauth-excluded-models", handler.PatchOAuthExcludedModels)
	r.Set(http.MethodDelete, "/oauth-excluded-models", handler.DeleteOAuthExcludedModels)

	r.Set(http.MethodGet, "/oauth-model-alias", handler.GetOAuthModelAlias)
	r.Set(http.MethodPut, "/oauth-model-alias", handler.PutOAuthModelAlias)
	r.Set(http.MethodPatch, "/oauth-model-alias", handler.PatchOAuthModelAlias)
	r.Set(http.MethodDelete, "/oauth-model-alias", handler.DeleteOAuthModelAlias)

	r.Set(http.MethodGet, "/auth-files", handler.ListAuthFiles)
	r.Set(http.MethodGet, "/auth-files/models", handler.GetAuthFileModels)
	r.Set(http.MethodGet, "/model-definitions/:channel", handler.GetStaticModelDefinitions)
	r.Set(http.MethodGet, "/auth-files/download", handler.DownloadAuthFile)
	r.Set(http.MethodPost, "/auth-files", handler.UploadAuthFile)
	r.Set(http.MethodDelete, "/auth-files", handler.DeleteAuthFile)
	r.Set(http.MethodPatch, "/auth-files/status", handler.PatchAuthFileStatus)
	r.Set(http.MethodPatch, "/auth-files/fields", handler.PatchAuthFileFields)
	r.Set(http.MethodPost, "/vertex/import", handler.ImportVertexCredential)

	r.Set(http.MethodGet, "/anthropic-auth-url", handler.RequestAnthropicToken)
	r.Set(http.MethodGet, "/codex-auth-url", handler.RequestCodexToken)
	r.Set(http.MethodGet, "/antigravity-auth-url", handler.RequestAntigravityToken)
	r.Set(http.MethodGet, "/kimi-auth-url", handler.RequestKimiToken)
	r.Set(http.MethodPost, "/oauth-callback", handler.PostOAuthCallback)
	r.Set(http.MethodGet, "/get-auth-status", handler.GetAuthStatus)

	return r
}
