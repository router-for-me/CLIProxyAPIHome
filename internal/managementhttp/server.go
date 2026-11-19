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
	cpasdkapi "github.com/router-for-me/CLIProxyAPI/v8/sdk/api"
	cpasdkauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	cpacoreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cpaconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/buildinfo"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	clustermanagement "github.com/router-for-me/CLIProxyAPIHome/internal/cluster/management"
	appconfig "github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/home"
	"github.com/router-for-me/CLIProxyAPIHome/internal/managementasset"
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
	"ETag",
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

// registerClusterManagementRoutes registers database-backed V8 resource handlers.
func registerClusterManagementRoutes(r *RouteRegistry, handler *clustermanagement.Handler) {
	// Validate request inputs before mutating persisted state.
	if r == nil || handler == nil {
		return
	}

	r.Set(http.MethodGet, "/nodes", handler.ListNodes)
	r.Set(http.MethodPatch, "/nodes/:node_id", handler.UpdateCPANodeName)
	r.Set(http.MethodGet, "/topology", handler.GetTopology)
	r.Set(http.MethodGet, "/server/latest-version", handler.GetLatestVersion)
	r.Set(http.MethodPost, "/certificates/clients", handler.CreateClientCertificate)
	r.Set(http.MethodGet, "/plugins", handler.ListPlugins)
	r.Set(http.MethodGet, "/plugins/store", handler.ListPluginStore)
	r.Set(http.MethodPost, "/plugins/store/:id/install", handler.InstallPluginFromStore)
	r.Set(http.MethodPost, "/plugins/store/:id/uninstall", handler.UninstallPluginFromStore)
	r.Set(http.MethodGet, "/plugin-store-auth", handler.ListPluginStoreAuth)
	r.Set(http.MethodPost, "/plugin-store-auth", handler.CreatePluginStoreAuth)
	r.Set(http.MethodGet, "/plugin-store-auth/:id", handler.GetPluginStoreAuth)
	r.Set(http.MethodPatch, "/plugin-store-auth/:id", handler.UpdatePluginStoreAuth)
	r.Set(http.MethodDelete, "/plugin-store-auth/:id", handler.DeletePluginStoreAuth)

	r.Set(http.MethodGet, "/observability/logs", handler.GetLogsV8)
	r.Set(http.MethodDelete, "/observability/logs", handler.DeleteLogs)

	r.Set(http.MethodGet, "/access/api-keys", handler.GetAPIKeys)
	r.Set(http.MethodPost, "/access/api-keys", handler.PostAPIKeys)
	r.Set(http.MethodPut, "/access/api-keys", handler.PutAPIKeys)
	r.Set(http.MethodPatch, "/access/api-keys", handler.PatchAPIKeys)
	r.Set(http.MethodDelete, "/access/api-keys", handler.DeleteAPIKeys)
	r.Set(http.MethodGet, "/observability/usage/api-keys", handler.GetAPIKeyUsage)
	r.Set(http.MethodGet, "/capabilities", handler.GetCapabilities)
	r.Set(http.MethodGet, "/credentials/in-flight", handler.GetInFlightDetails)
	r.Set(http.MethodGet, "/credentials/in-flight/summary", handler.GetInFlightSummary)
	r.Set(http.MethodGet, "/credentials/concurrency-policies", handler.ListCredentialConcurrencyPolicies)
	r.Set(http.MethodGet, "/credentials/concurrency", handler.GetCredentialConcurrency)
	r.Set(http.MethodGet, "/credentials/:credential_id/concurrency-policy", handler.GetCredentialConcurrencyPolicy)
	r.Set(http.MethodPatch, "/credentials/:credential_id/concurrency-policy", handler.PatchCredentialConcurrencyPolicy)
	r.Set(http.MethodDelete, "/credentials/:credential_id/cooldown", handler.ClearCredentialCooldown)
	r.Set(http.MethodGet, "/credentials/:credential_id/model-states", handler.GetCredentialModelStates)
	r.Set(http.MethodGet, "/credentials/model-states", handler.GetCredentialModelStates)
	r.Set(http.MethodGet, "/quota/credentials", handler.ListQuotaCredentials)
	r.Set(http.MethodGet, "/quota/credentials/:credential_id", handler.GetQuotaCredential)
	r.Set(http.MethodPost, "/quota/collect", handler.CollectQuota)
	r.Set(http.MethodGet, "/usage/overview", handler.GetUsageOverview)
	r.Set(http.MethodGet, "/usage/records", handler.ListUsageRecords)
	r.Set(http.MethodGet, "/usage/session-tree", handler.GetSessionTree)
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
	r.Set(http.MethodGet, "/channel-group-credential-candidates", handler.ListChannelGroupCredentialCandidates)
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

	r.Set(http.MethodGet, "/observability/logs/requests/:id", handler.DownloadRequestLogByID)

	r.Set(http.MethodGet, "/credentials", handler.ListCredentialsV8)
	r.Set(http.MethodGet, "/credentials/models", handler.GetCredentialModelsV8)
	r.Set(http.MethodGet, "/credentials/download", handler.DownloadAuthFile)
	r.Set(http.MethodPost, "/credentials", handler.UploadAuthFile)
	r.Set(http.MethodDelete, "/credentials", handler.DeleteAuthFile)
	r.Set(http.MethodPatch, "/credentials/status", handler.PatchAuthFileStatus)
	r.Set(http.MethodPatch, "/credentials/fields", handler.PatchAuthFileFields)
	r.Set(http.MethodGet, "/models", handler.GetModelsV8)
	r.Set(http.MethodGet, "/routing/model-definitions/:channel", handler.GetStaticModelDefinitions)
	r.Set(http.MethodGet, "/oauth/status", handler.GetAuthStatus)
	r.Set(http.MethodPost, "/requests/api-call", handler.APICall)
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

	// Both the identity and the gzip representation are cacheable, so shared
	// caches have to key on the negotiated encoding even when this particular
	// response goes out uncompressed.
	c.Header("Vary", "Accept-Encoding")

	assetName := fileInfo.Name()
	contentType := mime.TypeByExtension(path.Ext(assetName))

	if shouldCompressAsset(c.Request, assetName, fileInfo) {
		compressed, raw, compressedOK := gzipAsset(assetName, fileInfo, file)
		if compressedOK {
			c.Header("Content-Encoding", "gzip")
			// ServeContent would otherwise sniff the gzip bytes; the extension
			// already tells us the real type.
			if contentType != "" {
				c.Header("Content-Type", contentType)
			}
			http.ServeContent(c.Writer, c.Request, assetName, fileInfo.ModTime(), compressedAssetReader(compressed))
			return
		}
		// Compression was attempted, so the reader is spent. Serve the bytes it
		// consumed instead of re-reading an exhausted file.
		if raw == nil {
			log.Error("failed to read management control panel asset for compression")
			c.AbortWithStatus(http.StatusInternalServerError)
			return
		}
		if contentType == "" {
			contentType = http.DetectContentType(raw)
		}
		c.Data(http.StatusOK, contentType, raw)
		return
	}

	if readSeeker, ok := file.(io.ReadSeeker); ok {
		http.ServeContent(c.Writer, c.Request, assetName, fileInfo.ModTime(), readSeeker)
		return
	}

	data, errRead := io.ReadAll(file)
	if errRead != nil {
		log.WithError(errRead).Error("failed to read management control panel asset")
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}

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
		clusterHandler.SetQuotaRecollectTrigger(clusterOpt.QuotaRecollect)
		clusterGroup := engine.Group("/v0/cluster")
		clusterGroup.Use(withBuildInfoHeaders(), clusterMTLSMiddleware())
		registerClusterInternalRoutes(clusterGroup, clusterHandler)
	}

	if clusterEnabled {
		userGroup := engine.Group("/user")
		userGroup.Use(withBuildInfoHeaders())
		userapi.Register(userGroup, userapi.NewHandler(clusterOpt.Repository, clusterOpt.Runtime))
	}

	reg := defaultRoutes(handler)
	for i := range opts {
		if opts[i] == nil {
			continue
		}
		opts[i](reg)
	}
	v8 := engine.Group("/v8/management")
	v8.Use(withBuildInfoHeaders())
	if clusterEnabled {
		v8.Use(clusterAvailabilityMiddleware(clusterOpt, handler), handler.Middleware())
	} else {
		v8.Use(
			refreshAndAvailabilityMiddleware(configFilePath, handler, authManager, tokenStore),
			handler.Middleware(),
			func(c *gin.Context) { c.Set("management.config-v8", true) },
		)
	}
	managementV8Routes(reg, handler, clusterHandler).Register(v8)
	// OAuth callbacks authenticate with their pending state, as in CPA V8.
	callback := engine.Group("/v8/management/oauth")
	callback.Use(withBuildInfoHeaders())
	if clusterEnabled {
		callback.Use(clusterAvailabilityMiddleware(clusterOpt, handler))
		callback.GET("/callback", clusterHandler.PostOAuthCallback)
		callback.POST("/callback", clusterHandler.PostOAuthCallback)
	} else {
		callback.Use(refreshAndAvailabilityMiddleware(configFilePath, handler, authManager, tokenStore))
		callback.GET("/callback", handler.GetOAuthCallback)
		callback.POST("/callback", handler.PostOAuthCallback)
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

// defaultRoutes registers SDK-backed V8 resource handlers.
func defaultRoutes(handler *cpasdkapi.Handler) *RouteRegistry {
	// Validate request inputs before mutating persisted state.
	r := newRouteRegistry()
	if handler == nil {
		return r
	}

	r.Set(http.MethodGet, "/server/latest-version", handler.GetLatestVersion)

	r.Set(http.MethodPost, "/requests/api-call", handler.APICall)

	r.Set(http.MethodGet, "/observability/usage/api-keys", handler.GetAPIKeyUsage)
	r.Set(http.MethodGet, "/observability/usage/queue", handler.GetUsageQueue)

	r.Set(http.MethodGet, "/observability/logs", handler.GetLogs)
	r.Set(http.MethodDelete, "/observability/logs", handler.DeleteLogs)
	r.Set(http.MethodGet, "/observability/logs/errors", handler.GetRequestErrorLogs)
	r.Set(http.MethodGet, "/observability/logs/errors/:name", handler.DownloadRequestErrorLog)
	r.Set(http.MethodGet, "/observability/logs/requests/:id", handler.GetRequestLogByID)

	r.Set(http.MethodGet, "/credentials", handler.ListAuthFiles)
	r.Set(http.MethodGet, "/credentials/models", handler.GetAuthFileModels)
	r.Set(http.MethodGet, "/routing/model-definitions/:channel", handler.GetStaticModelDefinitions)
	r.Set(http.MethodGet, "/credentials/download", handler.DownloadAuthFile)
	r.Set(http.MethodPost, "/credentials", handler.UploadAuthFile)
	r.Set(http.MethodDelete, "/credentials", handler.DeleteAuthFile)
	r.Set(http.MethodPatch, "/credentials/status", handler.PatchAuthFileStatus)
	r.Set(http.MethodPatch, "/credentials/fields", handler.PatchAuthFileFields)

	r.Set(http.MethodGet, "/oauth/status", handler.GetAuthStatus)

	return r
}
