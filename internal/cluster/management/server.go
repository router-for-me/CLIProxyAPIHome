package management

import (
	"context"
	"crypto/tls"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginstore"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/home"
	"github.com/router-for-me/CLIProxyAPIHome/internal/pluginauth"
)

type Handler struct {
	repo                  *cluster.Repository
	runtime               *home.Runtime
	nodeIP                string
	nodePort              int
	heartbeatTimeout      time.Duration
	forwardTLSConfig      *tls.Config
	pluginStoreHTTPClient pluginstore.HTTPDoer
	modelsDevHTTPClient   *http.Client
	pluginStoreAuth       *pluginauth.Service
	quotaRecollect        QuotaRecollectTrigger
}

// NewHandler creates a new handler.
func NewHandler(repo *cluster.Repository, runtime *home.Runtime, nodeIP string, nodePort int) *Handler {
	return &Handler{repo: repo, runtime: runtime, nodeIP: strings.TrimSpace(nodeIP), nodePort: nodePort, heartbeatTimeout: cluster.DefaultHeartbeatTimeout(), pluginStoreAuth: pluginauth.NewService(repo)}
}

// SetForwardTLSConfig sets the TLS config used for cluster HTTP forwarding.
func (h *Handler) SetForwardTLSConfig(tlsConfig *tls.Config) {
	if h == nil {
		return
	}
	h.forwardTLSConfig = tlsConfig
}

// SetHeartbeatTimeout sets the live cluster node timeout used by management reads.
func (h *Handler) SetHeartbeatTimeout(timeout time.Duration) {
	if h == nil || timeout <= 0 {
		return
	}
	h.heartbeatTimeout = timeout
}

func (h *Handler) SetPluginStoreHTTPClient(client pluginstore.HTTPDoer) {
	if h == nil {
		return
	}
	h.pluginStoreHTTPClient = client
}

// SetModelsDevHTTPClient sets the HTTP client used for server-side pricing imports.
func (h *Handler) SetModelsDevHTTPClient(client *http.Client) {
	if h == nil {
		return
	}
	h.modelsDevHTTPClient = client
}

func (h *Handler) replaceConfigSnapshot(ctx context.Context, root map[string]any) error {
	if h == nil || h.repo == nil {
		return context.Canceled
	}
	return h.repo.ReplaceConfigSnapshotWithLifecycleConfig(ctx, h.heartbeatTimeout, root)
}

// requestContext handles a request context.
func (h *Handler) requestContext(c *gin.Context) (context.Context, context.CancelFunc) {
	ctx := context.Background()
	if c != nil && c.Request != nil && c.Request.Context() != nil {
		ctx = c.Request.Context()
	}
	return context.WithTimeout(ctx, 10*time.Second)
}

// currentConfig handles a current config.
func (h *Handler) currentConfig(ctx context.Context) (*config.Config, map[string]any, error) {
	root, errSnapshot := h.configRoot(ctx)
	if errSnapshot != nil {
		return nil, nil, errSnapshot
	}
	cfg, errConfig := configFromRoot(root)
	if errConfig != nil {
		return nil, nil, errConfig
	}
	return cfg, root, nil
}

// refreshConfig refreshes a config.
func (h *Handler) refreshConfig(ctx context.Context) error {
	if h == nil || h.runtime == nil {
		return nil
	}
	cfg, payload, errConfig := h.repo.LoadConfigAsRuntimeConfig(ctx)
	if errConfig != nil {
		return errConfig
	}
	if errApply := h.runtime.ApplyConfigFromCluster(ctx, cfg); errApply != nil {
		return errApply
	}
	h.runtime.PublishConfigYAML(payload)
	return nil
}

func (h *Handler) publishCurrentConfig(ctx context.Context) error {
	if h == nil || h.runtime == nil {
		return nil
	}
	_, payload, errConfig := h.repo.LoadConfigAsRuntimeConfig(ctx)
	if errConfig != nil {
		return errConfig
	}
	h.runtime.PublishConfigYAML(payload)
	return nil
}

// refreshAuths refreshes an auths.
func (h *Handler) refreshAuths(ctx context.Context) error {
	if h == nil || h.runtime == nil {
		return nil
	}
	return h.runtime.ReloadAuths(ctx)
}

// respondError handles a respond error.
func respondError(c *gin.Context, status int, code string, err error) {
	message := strings.TrimSpace(code)
	if err != nil && strings.TrimSpace(err.Error()) != "" {
		message = err.Error()
	}
	c.JSON(status, gin.H{"error": code, "message": message})
}

// fieldErrorItem identifies one request field that failed validation together
// with a stable machine-readable code clients can localize.
type fieldErrorItem struct {
	Field string `json:"field"`
	Code  string `json:"code"`
}

// respondErrorWithFieldErrors writes the same envelope as respondError plus a
// field_errors array so clients can anchor and localize validation failures
// without parsing message strings.
func respondErrorWithFieldErrors(c *gin.Context, status int, code string, err error, fieldErrors []fieldErrorItem) {
	message := strings.TrimSpace(code)
	if err != nil && strings.TrimSpace(err.Error()) != "" {
		message = err.Error()
	}
	c.JSON(status, gin.H{"error": code, "message": message, "field_errors": fieldErrors})
}

// respondOK handles a respond ok.
func respondOK(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}
