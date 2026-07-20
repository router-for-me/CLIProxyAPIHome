package home

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	sdkpluginhost "github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginhost"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginstore"
	"github.com/router-for-me/CLIProxyAPIHome/internal/access"
	configaccess "github.com/router-for-me/CLIProxyAPIHome/internal/access/config_access"
	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	homeerrors "github.com/router-for-me/CLIProxyAPIHome/internal/errors"
	"github.com/router-for-me/CLIProxyAPIHome/internal/managementasset"
	"github.com/router-for-me/CLIProxyAPIHome/internal/node"
	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
	"github.com/router-for-me/CLIProxyAPIHome/internal/util"
	"github.com/router-for-me/CLIProxyAPIHome/internal/watcher/synthesizer"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type Runtime struct {
	cfgMu sync.RWMutex
	cfg   *config.Config

	authDir    string
	configPath string

	configSubsMu    sync.Mutex
	nextConfigSubID uint64
	configSubs      map[uint64]func(payload []byte) error

	accessManager           *access.Manager
	coreManager             *coreauth.Manager
	pluginHost              *sdkpluginhost.Host
	pluginSyncMu            sync.Mutex
	pluginStoreSync         map[string]pluginStoreSyncState
	pluginStoreAuthResolver func(context.Context) ([]pluginstore.ResolvedAuthConfig, error)
	pluginSyncConfigLoader  func(context.Context) (*config.Config, error)
	pluginSyncNodeActive    func(context.Context, string, string) (bool, error)
	pluginSyncHTTPClient    pluginstore.HTTPDoer
	clusterAdapter          ClusterAdapter
	clusterRefresh          func(context.Context, string) ([]byte, error)
	originalStore           coreauth.Store

	clusterUsageQueueMu sync.Mutex
	clusterUsageQueue   *usagePayloadQueue

	cancel context.CancelFunc

	fileWatcher interface{ Stop() error }
}

func (r *Runtime) SetPluginStoreAuthResolver(resolver func(context.Context) ([]pluginstore.ResolvedAuthConfig, error)) {
	if r == nil {
		return
	}
	r.pluginStoreAuthResolver = resolver
}

func (r *Runtime) resolvePluginStoreAuth(ctx context.Context) ([]pluginstore.ResolvedAuthConfig, error) {
	if r == nil || r.pluginStoreAuthResolver == nil {
		return nil, nil
	}
	return r.pluginStoreAuthResolver(ctx)
}

func (r *Runtime) SetPluginSyncConfigLoader(loader func(context.Context) (*config.Config, error)) {
	if r == nil {
		return
	}
	r.pluginSyncConfigLoader = loader
}

func (r *Runtime) pluginSyncConfig(ctx context.Context) (*config.Config, error) {
	if r == nil {
		return nil, nil
	}
	if r.pluginSyncConfigLoader != nil {
		return r.pluginSyncConfigLoader(ctx)
	}
	return r.Config(), nil
}

func (r *Runtime) SetPluginSyncNodeActive(check func(context.Context, string, string) (bool, error)) {
	if r == nil {
		return
	}
	r.pluginSyncNodeActive = check
}

func (r *Runtime) PluginSyncNodeActive(ctx context.Context, nodeID string, fingerprint string) (bool, error) {
	if r == nil || r.pluginSyncNodeActive == nil {
		return false, nil
	}
	return r.pluginSyncNodeActive(ctx, strings.TrimSpace(nodeID), strings.TrimSpace(fingerprint))
}

type ClusterAdapter interface {
	Enabled() bool
	LoadAuthIndex(ctx context.Context) error
	ListMinimalAuths() []*coreauth.Auth
	GetFullAuth(ctx context.Context, uuid string) (*coreauth.Auth, error)
	LoadConfigYAML(ctx context.Context) ([]byte, error)
}

type freshFullAuthResolver interface {
	GetFreshFullAuth(ctx context.Context, uuid string) (*coreauth.Auth, error)
}

type clusterUsageStore interface {
	StoreUsagePayload(ctx context.Context, payload string) error
}

type appLogStore interface {
	StoreAppLogPayload(ctx context.Context, clientIP string, payload string) error
}

type KVGetResult struct {
	Value []byte
	Found bool
}

type kvStore interface {
	KVGet(ctx context.Context, key string) ([]byte, bool, error)
	KVSet(ctx context.Context, key string, value []byte, ttl time.Duration, mode string) (bool, error)
	KVDel(ctx context.Context, keys []string) (int64, error)
	KVExpire(ctx context.Context, key string, ttl time.Duration) (bool, error)
	KVTTL(ctx context.Context, key string) (int64, error)
	KVIncrBy(ctx context.Context, key string, delta int64) (int64, error)
	KVMGet(ctx context.Context, keys []string) ([]KVGetResult, error)
	KVMSet(ctx context.Context, pairs map[string][]byte) error
	KVPurgeExpired(ctx context.Context, now time.Time, limit int) (int64, error)
}

type pluginStatusStore interface {
	ReplacePluginStatus(ctx context.Context, nodeType string, status node.PluginTaskStatus) error
	ListPendingPluginTasks(ctx context.Context, nodeType string, nodeID string) ([]node.PluginTask, error)
}

type channelScopedAuthStore interface {
	AllowedAuthIDsForAPIKey(ctx context.Context, apiKey string) ([]string, error)
}

type modelScopedAuthStore interface {
	AllowedModelIDsForAPIKey(ctx context.Context, apiKey string) ([]string, error)
}

type apiKeyScopedDispatchStore interface {
	AllowedDispatchIDsForAPIKey(ctx context.Context, apiKey string) ([]string, []string, error)
}

// NewRuntime creates a new runtime.
func NewRuntime(cfg *config.Config) (*Runtime, error) {
	// Keep validation before state changes so failures leave existing data intact.
	if cfg == nil {
		return nil, fmt.Errorf("home runtime: config is nil")
	}

	resolvedAuthDir, errResolveAuthDir := util.ResolveAuthDir(cfg.AuthDir)
	if errResolveAuthDir != nil {
		return nil, errResolveAuthDir
	}
	if strings.TrimSpace(resolvedAuthDir) != "" {
		cfg.AuthDir = resolvedAuthDir
	}

	store := coreauth.GetTokenStore()
	if dirSetter, ok := store.(interface{ SetBaseDir(string) }); ok {
		dirSetter.SetBaseDir(cfg.AuthDir)
	}

	selector := selectorFromConfig(cfg)
	coreManager := coreauth.NewManager(store, selector, nil)
	coreManager.SetRoundTripperProvider(newDefaultRoundTripperProvider())
	coreManager.SetConfig(cfg)
	coreManager.SetOAuthModelAlias(cfg.OAuthModelAlias)
	pluginHost := newPluginHostForRuntime(cfg)

	accessManager := access.NewManager()
	configaccess.Register(&cfg.SDKConfig)

	runtime := &Runtime{
		cfg:           cfg,
		authDir:       cfg.AuthDir,
		accessManager: accessManager,
		coreManager:   coreManager,
		pluginHost:    pluginHost,
		originalStore: store,
	}
	coreManager.SetPluginAuthRefresher(runtime)
	coreManager.SetPluginScheduler(runtime)
	runtime.refreshAccessProviders()
	return runtime, nil
}

// SetClusterAdapter sets a cluster adapter.
func (r *Runtime) SetClusterAdapter(adapter ClusterAdapter) {
	if r == nil {
		return
	}
	r.clusterAdapter = adapter
	r.refreshAccessProviders()
	if r.coreManager != nil {
		if adapter != nil && adapter.Enabled() {
			r.coreManager.SetFullAuthResolver(adapter)
			if store, ok := adapter.(coreauth.Store); ok {
				r.coreManager.SetStore(store)
			}
		} else {
			r.coreManager.SetFullAuthResolver(nil)
			r.coreManager.SetStore(r.originalStore)
		}
	}
}

// SetClusterRefreshHandler sets a cluster refresh handler.
func (r *Runtime) SetClusterRefreshHandler(handler func(context.Context, string) ([]byte, error)) {
	if r == nil {
		return
	}
	r.clusterRefresh = handler
}

// Start starts the process.
func (r *Runtime) Start(ctx context.Context, configPath string) error {
	// Keep validation before state changes so failures leave existing data intact.
	if r == nil {
		return fmt.Errorf("home runtime: runtime is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	configPath = strings.TrimSpace(configPath)
	if configPath != "" {
		configPath = filepath.Clean(configPath)
		if !filepath.IsAbs(configPath) {
			if abs, errAbs := filepath.Abs(configPath); errAbs == nil {
				configPath = abs
			}
		}
	}

	r.cfgMu.Lock()
	r.configPath = configPath
	r.cfgMu.Unlock()

	runCtx, cancel := context.WithCancel(ctx)
	r.cancel = cancel
	r.startClusterUsageWriter(runCtx)
	r.startKVCleanupLoop(runCtx)
	r.startInFlightCleanupLoop(runCtx)

	if !r.clusterAutoRefreshGated() && strings.TrimSpace(r.authDir) != "" {
		if errEnsureAuthDir := os.MkdirAll(r.authDir, 0o755); errEnsureAuthDir != nil {
			return fmt.Errorf("home runtime: ensure auth dir: %w", errEnsureAuthDir)
		}
	}

	registry.StartModelsUpdater(runCtx)
	r.registerModelRefreshCallback()
	managementasset.SetCurrentConfig(r.cfg)
	managementasset.StartAutoUpdater(context.Background(), configPath)

	if errPluginSync := r.syncPluginStoreManifests(runCtx, r.Config()); errPluginSync != nil {
		return errPluginSync
	}
	r.applyPluginConfig(runCtx, r.Config())

	if errLoad := r.loadAuths(runCtx); errLoad != nil {
		return errLoad
	}

	clusterMode := r.clusterAutoRefreshGated()
	if clusterMode {
		log.Infof("core auth auto-refresh waiting for cluster master")
	} else {
		r.StartAutoRefresh(context.Background())
	}

	if clusterMode {
		log.Infof("hot reload file watcher disabled in cluster mode")
	} else {
		if errWatch := r.startFileWatcher(runCtx, configPath); errWatch != nil {
			return errWatch
		}
	}

	return nil
}

// Stop stops the process.
func (r *Runtime) Stop() {
	if r == nil {
		return
	}
	if r.cancel != nil {
		r.cancel()
		r.cancel = nil
	}
	r.stopClusterUsageWriter()
	if r.fileWatcher != nil {
		_ = r.fileWatcher.Stop()
		r.fileWatcher = nil
	}
	if r.pluginHost != nil {
		r.pluginHost.ShutdownAll()
	}
	r.StopAutoRefresh()
	if r.coreManager != nil {
		r.coreManager.Shutdown()
	}
}

// StartAutoRefresh starts an auto refresh.
func (r *Runtime) StartAutoRefresh(ctx context.Context) {
	if r == nil || r.coreManager == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	interval := 15 * time.Minute
	r.coreManager.StartAutoRefresh(ctx, interval)
	log.Infof("core auth auto-refresh started (interval=%s)", interval)
}

// StopAutoRefresh stops an auto refresh.
func (r *Runtime) StopAutoRefresh() {
	if r == nil || r.coreManager == nil {
		return
	}
	r.coreManager.StopAutoRefresh()
}

// clusterAutoRefreshGated handles a cluster auto refresh gated.
func (r *Runtime) clusterAutoRefreshGated() bool {
	return r != nil && r.clusterAdapter != nil && r.clusterAdapter.Enabled()
}

// Config handles a config.
func (r *Runtime) Config() *config.Config {
	if r == nil {
		return nil
	}
	r.cfgMu.RLock()
	defer r.cfgMu.RUnlock()
	return r.cfg
}

// CoreManager handles a core manager.
func (r *Runtime) CoreManager() *coreauth.Manager {
	if r == nil {
		return nil
	}
	return r.coreManager
}

// RefreshNow refreshes refresh now.
func (r *Runtime) RefreshNow(ctx context.Context, authIndex string) ([]byte, error) {
	if r == nil {
		return nil, fmt.Errorf("home runtime: runtime is nil")
	}
	if r.clusterRefresh != nil {
		return r.clusterRefresh(ctx, authIndex)
	}
	return r.RefreshNowLocal(ctx, authIndex)
}

// RefreshNowLocal refreshes refresh now local.
func (r *Runtime) RefreshNowLocal(ctx context.Context, authIndex string) ([]byte, error) {
	if r == nil || r.coreManager == nil {
		return nil, fmt.Errorf("home runtime: runtime not ready")
	}
	updated, errRefresh := r.coreManager.RefreshNow(ctx, authIndex)
	if errRefresh != nil {
		return nil, errRefresh
	}
	return BuildRefreshPayload(updated)
}

// UpdateAuthInMemory updates an auth in memory.
func (r *Runtime) UpdateAuthInMemory(ctx context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	if r == nil || r.coreManager == nil {
		return nil, fmt.Errorf("home runtime: runtime not ready")
	}
	return r.coreManager.Update(coreauth.WithSkipPersist(ctx), auth)
}

// RefreshClusterAuthIndex refreshes refresh cluster auth index.
func (r *Runtime) RefreshClusterAuthIndex(ctx context.Context, uuid string) error {
	if r == nil || r.clusterAdapter == nil {
		return nil
	}
	refresher, ok := r.clusterAdapter.(interface {
		RefreshAuthIndex(context.Context, string) error
	})
	if !ok || refresher == nil {
		return nil
	}
	return refresher.RefreshAuthIndex(ctx, uuid)
}

// PersistClusterUsagePayload stores persist cluster usage payload.
func (r *Runtime) PersistClusterUsagePayload(ctx context.Context, payload string) (bool, error) {
	if r == nil || r.clusterAdapter == nil || !r.clusterAdapter.Enabled() {
		return false, nil
	}
	queue := r.getClusterUsageQueue()
	if queue == nil {
		return true, nil
	}
	if ok := queue.Push(payload); !ok {
		log.Warnf("cluster usage queue is stopped; accepting usage without persistence")
	}
	return true, nil
}

// PersistAppLogPayload stores a CPA app log payload in the runtime database.
func (r *Runtime) PersistAppLogPayload(ctx context.Context, clientIP string, payload string) (bool, error) {
	if r == nil || r.clusterAdapter == nil || !r.clusterAdapter.Enabled() {
		return false, nil
	}
	store, ok := r.clusterAdapter.(appLogStore)
	if !ok || store == nil {
		return false, fmt.Errorf("app log store is unavailable")
	}
	return true, store.StoreAppLogPayload(ctx, clientIP, payload)
}

// KVGet returns an active KV value from the cluster store.
func (r *Runtime) KVGet(ctx context.Context, key string) ([]byte, bool, error) {
	store, errStore := r.kvStore()
	if errStore != nil {
		return nil, false, errStore
	}
	return store.KVGet(ctx, key)
}

// KVSet writes a KV value to the cluster store.
func (r *Runtime) KVSet(ctx context.Context, key string, value []byte, ttl time.Duration, mode string) (bool, error) {
	store, errStore := r.kvStore()
	if errStore != nil {
		return false, errStore
	}
	return store.KVSet(ctx, key, value, ttl, mode)
}

// KVDel deletes active KV values from the cluster store.
func (r *Runtime) KVDel(ctx context.Context, keys []string) (int64, error) {
	store, errStore := r.kvStore()
	if errStore != nil {
		return 0, errStore
	}
	return store.KVDel(ctx, keys)
}

// KVExpire updates a KV value TTL in the cluster store.
func (r *Runtime) KVExpire(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	store, errStore := r.kvStore()
	if errStore != nil {
		return false, errStore
	}
	return store.KVExpire(ctx, key, ttl)
}

// KVTTL returns a Redis-compatible KV TTL from the cluster store.
func (r *Runtime) KVTTL(ctx context.Context, key string) (int64, error) {
	store, errStore := r.kvStore()
	if errStore != nil {
		return 0, errStore
	}
	return store.KVTTL(ctx, key)
}

// KVIncrBy increments a KV integer in the cluster store.
func (r *Runtime) KVIncrBy(ctx context.Context, key string, delta int64) (int64, error) {
	store, errStore := r.kvStore()
	if errStore != nil {
		return 0, errStore
	}
	return store.KVIncrBy(ctx, key, delta)
}

// KVMGet returns KV values in request order from the cluster store.
func (r *Runtime) KVMGet(ctx context.Context, keys []string) ([]KVGetResult, error) {
	store, errStore := r.kvStore()
	if errStore != nil {
		return nil, errStore
	}
	return store.KVMGet(ctx, keys)
}

// KVMSet atomically writes KV values to the cluster store.
func (r *Runtime) KVMSet(ctx context.Context, pairs map[string][]byte) error {
	store, errStore := r.kvStore()
	if errStore != nil {
		return errStore
	}
	return store.KVMSet(ctx, pairs)
}

func (r *Runtime) kvStore() (kvStore, error) {
	if r == nil || r.clusterAdapter == nil || !r.clusterAdapter.Enabled() {
		return nil, fmt.Errorf("kv store unavailable")
	}
	store, ok := r.clusterAdapter.(kvStore)
	if !ok || store == nil {
		return nil, fmt.Errorf("kv store unavailable")
	}
	return store, nil
}

// ReplacePluginStatus stores the latest plugin status report in the cluster store.
func (r *Runtime) ReplacePluginStatus(ctx context.Context, nodeType string, status node.PluginTaskStatus) error {
	if r == nil || r.clusterAdapter == nil || !r.clusterAdapter.Enabled() {
		return fmt.Errorf("plugin status store unavailable")
	}
	store, ok := r.clusterAdapter.(pluginStatusStore)
	if !ok || store == nil {
		return fmt.Errorf("plugin status store unavailable")
	}
	return store.ReplacePluginStatus(ctx, nodeType, status)
}

// ListPendingPluginTasks returns pending plugin tasks for a node.
func (r *Runtime) ListPendingPluginTasks(ctx context.Context, nodeType string, nodeID string) ([]node.PluginTask, error) {
	if r == nil || r.clusterAdapter == nil || !r.clusterAdapter.Enabled() {
		return nil, fmt.Errorf("plugin task store unavailable")
	}
	store, ok := r.clusterAdapter.(pluginStatusStore)
	if !ok || store == nil {
		return nil, fmt.Errorf("plugin task store unavailable")
	}
	return store.ListPendingPluginTasks(ctx, nodeType, nodeID)
}

func (r *Runtime) startKVCleanupLoop(ctx context.Context) {
	store, errStore := r.kvStore()
	if errStore != nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	go r.runKVCleanupLoop(ctx, store)
}

func (r *Runtime) runKVCleanupLoop(ctx context.Context, store kvStore) {
	if store == nil {
		return
	}
	purge := func() {
		if _, errPurge := store.KVPurgeExpired(ctx, time.Now().UTC(), 1000); errPurge != nil {
			log.Errorf("kv store purge error: %v", errPurge)
		}
	}
	purge()
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			purge()
		}
	}
}

// BuildRefreshPayload builds a build refresh payload.
func BuildRefreshPayload(updated *coreauth.Auth) ([]byte, error) {
	// Resolve credential context before calling upstream OAuth services.
	if updated == nil {
		return nil, fmt.Errorf("auth manager: auth not found")
	}
	auth := SanitizeAuthForDownstream(updated)
	if auth == nil {
		return nil, fmt.Errorf("auth manager: auth not found")
	}
	authJSON, errMarshal := json.Marshal(auth)
	if errMarshal != nil {
		return nil, errMarshal
	}

	authIndex := strings.TrimSpace(auth.EnsureIndex())
	if authIndex == "" {
		return nil, fmt.Errorf("auth manager: auth not found")
	}
	authJSON, errSetAuthIndex := sjson.SetBytes(authJSON, "auth_index", authIndex)
	if errSetAuthIndex != nil {
		return nil, errSetAuthIndex
	}

	out := []byte("{}")
	out, _ = sjson.SetBytes(out, "auth_index", authIndex)
	out, _ = sjson.SetRawBytes(out, "auth", authJSON)
	return out, nil
}

// Authenticate validates request credentials and returns the access result.
func (r *Runtime) Authenticate(ctx context.Context, headers http.Header) (*access.Result, *access.AuthError) {
	return r.authenticateRequest(ctx, headers)
}

// AuthenticateHTTPRequest validates request credentials from a complete HTTP request.
func (r *Runtime) AuthenticateHTTPRequest(ctx context.Context, req *http.Request) (*access.Result, *access.AuthError) {
	return r.authenticateHTTPRequest(ctx, req)
}

// ReloadAuths handles a reload auths.
func (r *Runtime) ReloadAuths(ctx context.Context) error {
	return r.loadAuths(coreauth.WithSkipPersist(ctx))
}

// loadAuths loads an auths.
func (r *Runtime) loadAuths(ctx context.Context) error {
	// Normalize auth state before updating runtime indexes.
	if r == nil || r.coreManager == nil {
		return nil
	}
	if r.clusterAdapter != nil && r.clusterAdapter.Enabled() {
		return r.loadClusterAuths(ctx, r.clusterAdapter)
	}

	r.cfgMu.RLock()
	cfg := r.cfg
	authDir := r.authDir
	r.cfgMu.RUnlock()
	if cfg == nil {
		return fmt.Errorf("home runtime: config is nil")
	}

	now := time.Now()
	sctx := &synthesizer.SynthesisContext{
		Config:           cfg,
		AuthDir:          authDir,
		Now:              now,
		IDGenerator:      synthesizer.NewStableIDGenerator(),
		PluginAuthParser: r,
	}

	ctxSkipPersist := coreauth.WithSkipPersist(ctx)

	fileSynth := synthesizer.NewFileSynthesizer()
	fileAuths, errFile := fileSynth.Synthesize(sctx)
	if errFile != nil {
		return fmt.Errorf("home runtime: synthesize auth files: %w", errFile)
	}

	configSynth := synthesizer.NewConfigSynthesizer()
	configAuths, errCfg := configSynth.Synthesize(sctx)
	if errCfg != nil {
		return fmt.Errorf("home runtime: synthesize config auths: %w", errCfg)
	}

	desired := make(map[string]*coreauth.Auth, len(fileAuths)+len(configAuths))
	for _, a := range fileAuths {
		if a == nil || strings.TrimSpace(a.ID) == "" {
			continue
		}
		desired[a.ID] = a
		r.applyCoreAuthAddOrUpdate(ctxSkipPersist, a)
	}
	for _, a := range configAuths {
		if a == nil || strings.TrimSpace(a.ID) == "" {
			continue
		}
		desired[a.ID] = a
		r.applyCoreAuthAddOrUpdate(ctxSkipPersist, a)
	}

	removed := 0
	current := r.coreManager.List()
	for _, a := range current {
		if a == nil || strings.TrimSpace(a.ID) == "" {
			continue
		}
		if _, ok := desired[a.ID]; ok {
			continue
		}
		r.applyCoreAuthRemove(ctxSkipPersist, a.ID)
		removed++
	}

	log.Infof("loaded auths (files=%d config=%d removed=%d)", len(fileAuths), len(configAuths), removed)
	return nil
}

type DispatchResult struct {
	Model         string
	AccessToken   string
	BaseURL       string
	APIKey        string
	ForceMapping  bool
	OriginalAlias string

	AuthID         string
	Provider       string
	LeaseID        string
	LeaseTTL       time.Duration
	LeaseExpiresAt time.Time

	Auth *coreauth.Auth
}

type DispatchLeaseContext struct {
	RequestID  string
	DispatchID string
	CPANodeID  string
	CPAIP      string
	CPALabel   string
}

// DispatchForAPIKey processes dispatch with API-key channel restrictions.
func (r *Runtime) DispatchForAPIKey(ctx context.Context, reqModel string, headers http.Header, apiKey string) (*DispatchResult, error) {
	return r.DispatchForAPIKeyWithLease(ctx, reqModel, headers, apiKey, DispatchLeaseContext{})
}

// DispatchForAPIKeyWithLease selects a credential and atomically reserves its
// concurrency slot when the caller supplies request and dispatch identity.
func (r *Runtime) DispatchForAPIKeyWithLease(ctx context.Context, reqModel string, headers http.Header, apiKey string, leaseCtx DispatchLeaseContext) (*DispatchResult, error) {
	opts := coreauth.Options{}
	if headers != nil {
		opts.Headers = headers.Clone()
	}
	allowedAuthIDs, allowedModelIDs, errAllowed := r.allowedDispatchIDsForAPIKey(ctx, apiKey)
	if errAllowed != nil {
		return nil, errAllowed
	}
	metadata := make(map[string]any)
	if allowedAuthIDs != nil {
		metadata[coreauth.AllowedAuthIDsMetadataKey] = allowedAuthIDs
	}
	if allowedModelIDs != nil {
		metadata[coreauth.AllowedModelIDsMetadataKey] = allowedModelIDs
	}
	if len(metadata) > 0 {
		opts.Metadata = metadata
	}
	return r.dispatchWithLeaseOptions(ctx, reqModel, opts, leaseCtx)
}

func (r *Runtime) dispatchWithLeaseOptions(ctx context.Context, reqModel string, opts coreauth.Options, leaseCtx DispatchLeaseContext) (*DispatchResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	requestID := strings.TrimSpace(leaseCtx.RequestID)
	dispatchID := strings.TrimSpace(leaseCtx.DispatchID)
	excluded := make(map[string]struct{})
	var lastConcurrencyErr *ConcurrencyExceededError
	var lastIdentityErr error
	for {
		attemptOpts := opts
		attemptOpts.Metadata = cloneDispatchMetadata(opts.Metadata)
		if len(excluded) > 0 {
			allowed := dispatchAllowedAuthIDs(r.coreManager.List(), attemptOpts.Metadata, excluded)
			if len(allowed) == 0 {
				if lastIdentityErr != nil {
					return nil, lastIdentityErr
				}
				if lastConcurrencyErr != nil {
					return nil, lastConcurrencyErr
				}
				return nil, &coreauth.Error{Code: "auth_unavailable", Message: "no auth available"}
			}
			if attemptOpts.Metadata == nil {
				attemptOpts.Metadata = make(map[string]any)
			}
			attemptOpts.Metadata[coreauth.AllowedAuthIDsMetadataKey] = allowed
		}

		result, errDispatch := r.dispatchWithOptions(ctx, reqModel, attemptOpts)
		if errDispatch != nil {
			if lastIdentityErr != nil {
				return nil, lastIdentityErr
			}
			if lastConcurrencyErr != nil {
				return nil, lastConcurrencyErr
			}
			return nil, errDispatch
		}
		if result == nil || result.Auth == nil {
			return result, nil
		}

		limited := authConcurrencyLimitedForModel(result.Auth, result.Model)
		if requestID == "" || dispatchID == "" {
			if !limited {
				limited = r.persistedAuthConcurrencyLimitedForModel(ctx, result.Auth, result.Model)
			}
			if !limited {
				return result, nil
			}
			excluded[result.AuthID] = struct{}{}
			lastIdentityErr = &coreauth.Error{
				Code:    "concurrency_identity_required",
				Message: "request_id and dispatch_id are required for a concurrency-limited credential",
			}
			continue
		}

		lease, errReserve := r.ReserveInFlightLease(ctx, InFlightReserveInput{
			DispatchID:     dispatchID,
			RequestID:      requestID,
			CredentialID:   result.AuthID,
			Provider:       result.Provider,
			RequestedModel: reqModel,
			Model:          coreauth.CanonicalModelKey(result.Model),
			CPANodeID:      leaseCtx.CPANodeID,
			CPAIP:          leaseCtx.CPAIP,
			CPALabel:       leaseCtx.CPALabel,
			ForceMapping:   result.ForceMapping,
			OriginalAlias:  result.OriginalAlias,
		})
		if errReserve != nil {
			var concurrencyErr *ConcurrencyExceededError
			if errors.As(errReserve, &concurrencyErr) {
				excluded[result.AuthID] = struct{}{}
				lastConcurrencyErr = mergeConcurrencyError(lastConcurrencyErr, concurrencyErr)
				continue
			}
			var replayErr *DispatchReplayError
			if errors.As(errReserve, &replayErr) {
				return nil, replayErr
			}
			if errCtx := ctx.Err(); errCtx != nil {
				return nil, errCtx
			}
			if !limited {
				limited = r.persistedAuthConcurrencyLimitedForModel(ctx, result.Auth, result.Model)
			}
			if !limited {
				return result, nil
			}
			excluded[result.AuthID] = struct{}{}
			lastIdentityErr = &coreauth.Error{
				Code:      "concurrency_tracker_unavailable",
				Message:   "credential concurrency tracker unavailable",
				Retryable: true,
			}
			continue
		}
		if lease == nil {
			if !limited {
				return result, nil
			}
			return nil, &coreauth.Error{Code: "concurrency_tracker_unavailable", Message: "credential concurrency tracker unavailable", Retryable: true}
		}
		if lease.Reused && !dispatchMetadataAllowsAuthID(opts.Metadata, lease.CredentialID) {
			return nil, &DispatchReplayError{DispatchID: lease.DispatchID}
		}
		if lease.CredentialID != result.AuthID {
			reusedResult, errReused := r.dispatchResultForLease(ctx, lease)
			if errReused != nil {
				return nil, errReused
			}
			result = reusedResult
		}
		result.LeaseID = lease.LeaseID
		result.LeaseTTL = dispatchLeaseTTL(lease, r.inFlightLeaseTTL())
		result.LeaseExpiresAt = lease.ExpiresAt
		return result, nil
	}
}

func (r *Runtime) dispatchWithOptions(ctx context.Context, reqModel string, opts coreauth.Options) (*DispatchResult, error) {
	// Build the candidate view before applying availability rules.
	if r == nil || r.coreManager == nil {
		return nil, fmt.Errorf("home runtime: core manager is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	providers := r.availableProviderKeys()
	if len(providers) == 0 {
		return nil, fmt.Errorf("home runtime: no providers available")
	}

	if !r.supportsRequestedModel(reqModel) {
		trimmedModel := strings.TrimSpace(reqModel)
		if trimmedModel == "" {
			trimmedModel = "requested model"
		}
		return nil, &coreauth.Error{
			Code:    homeerrors.TypeModelNotFound,
			Message: fmt.Sprintf(homeerrors.MessageModelDoesNotExistFmt, trimmedModel),
		}
	}

	decision, errDispatch := r.coreManager.Dispatch(ctx, providers, reqModel, opts)
	if errDispatch != nil {
		return nil, errDispatch
	}
	if decision == nil || decision.Auth == nil {
		return nil, fmt.Errorf("home runtime: dispatch returned nil auth")
	}

	auth := decision.Auth
	upstreamModel := strings.TrimSpace(decision.UpstreamModel)
	if upstreamModel == "" {
		upstreamModel = strings.TrimSpace(reqModel)
	}
	return dispatchResultFromAuth(auth, upstreamModel, decision.Provider, decision.ForceMapping, decision.OriginalAlias), nil
}

func dispatchResultFromAuth(auth *coreauth.Auth, model string, provider string, forceMapping bool, originalAlias string) *DispatchResult {
	if auth == nil {
		return nil
	}
	accessToken := extractAccessToken(auth)
	baseURL := ""
	apiKey := ""
	if auth.Attributes != nil {
		baseURL = strings.TrimSpace(auth.Attributes["base_url"])
		apiKey = strings.TrimSpace(auth.Attributes["api_key"])
	}

	return &DispatchResult{
		Model:         strings.TrimSpace(model),
		AccessToken:   accessToken,
		BaseURL:       baseURL,
		APIKey:        apiKey,
		ForceMapping:  forceMapping,
		OriginalAlias: strings.TrimSpace(originalAlias),
		AuthID:        auth.ID,
		Provider:      strings.TrimSpace(provider),
		Auth:          auth.Clone(),
	}
}

func (r *Runtime) dispatchResultForLease(ctx context.Context, lease *InFlightLease) (*DispatchResult, error) {
	if r == nil || lease == nil || strings.TrimSpace(lease.CredentialID) == "" {
		return nil, fmt.Errorf("home runtime: invalid reused lease")
	}
	if r.clusterAdapter == nil {
		return nil, fmt.Errorf("home runtime: cluster adapter unavailable")
	}
	auth, errAuth := r.clusterAdapter.GetFullAuth(ctx, lease.CredentialID)
	if errAuth != nil {
		return nil, errAuth
	}
	if auth == nil {
		return nil, fmt.Errorf("home runtime: leased auth not found")
	}
	return dispatchResultFromAuth(auth, lease.Model, lease.Provider, lease.ForceMapping, lease.OriginalAlias), nil
}

func cloneDispatchMetadata(metadata map[string]any) map[string]any {
	if len(metadata) == 0 {
		return nil
	}
	out := make(map[string]any, len(metadata))
	for key, value := range metadata {
		out[key] = value
	}
	return out
}

func dispatchLeaseTTL(lease *InFlightLease, fallback time.Duration) time.Duration {
	if lease != nil {
		if ttl := lease.ExpiresAt.Sub(lease.LastRenewedAt); ttl > 0 {
			return ttl
		}
	}
	return fallback
}

func dispatchAllowedAuthIDSet(metadata map[string]any) (map[string]struct{}, bool) {
	if metadata == nil {
		return nil, false
	}
	raw, configured := metadata[coreauth.AllowedAuthIDsMetadataKey]
	if !configured {
		return nil, false
	}
	allowed := make(map[string]struct{})
	add := func(value string) {
		if id := strings.TrimSpace(value); id != "" {
			allowed[id] = struct{}{}
		}
	}
	switch values := raw.(type) {
	case []string:
		for _, value := range values {
			add(value)
		}
	case []any:
		for _, value := range values {
			add(fmt.Sprint(value))
		}
	case map[string]struct{}:
		for value := range values {
			add(value)
		}
	case map[string]bool:
		for value, enabled := range values {
			if enabled {
				add(value)
			}
		}
	}
	return allowed, true
}

func dispatchMetadataAllowsAuthID(metadata map[string]any, authID string) bool {
	allowed, configured := dispatchAllowedAuthIDSet(metadata)
	if !configured {
		return true
	}
	_, ok := allowed[strings.TrimSpace(authID)]
	return ok
}

func dispatchAllowedAuthIDs(auths []*coreauth.Auth, metadata map[string]any, excluded map[string]struct{}) []string {
	configured, restricted := dispatchAllowedAuthIDSet(metadata)
	out := make([]string, 0, len(auths))
	for _, auth := range auths {
		if auth == nil {
			continue
		}
		id := strings.TrimSpace(auth.ID)
		if id == "" {
			continue
		}
		if restricted {
			if _, ok := configured[id]; !ok {
				continue
			}
		}
		if _, skip := excluded[id]; skip {
			continue
		}
		out = append(out, id)
	}
	return out
}

func authConcurrencyLimitedForModel(auth *coreauth.Auth, model string) bool {
	if auth == nil {
		return false
	}
	if auth.MaxInFlight > 0 {
		return true
	}
	model = coreauth.CanonicalModelKey(model)
	for candidate, limit := range auth.MaxInFlightByModel {
		if coreauth.CanonicalModelKey(candidate) == model && limit > 0 {
			return true
		}
	}
	return false
}

func (r *Runtime) persistedAuthConcurrencyLimitedForModel(ctx context.Context, auth *coreauth.Auth, model string) bool {
	if r == nil || r.clusterAdapter == nil || auth == nil || strings.TrimSpace(auth.ID) == "" {
		return false
	}
	var (
		persisted *coreauth.Auth
		errAuth   error
	)
	if resolver, ok := r.clusterAdapter.(freshFullAuthResolver); ok && resolver != nil {
		persisted, errAuth = resolver.GetFreshFullAuth(ctx, auth.ID)
	} else {
		persisted, errAuth = r.clusterAdapter.GetFullAuth(ctx, auth.ID)
	}
	if errAuth != nil || persisted == nil {
		return false
	}
	return authConcurrencyLimitedForModel(persisted, model)
}

func mergeConcurrencyError(previous *ConcurrencyExceededError, current *ConcurrencyExceededError) *ConcurrencyExceededError {
	if previous == nil {
		return current
	}
	if current == nil || previous.Scope == current.Scope {
		return previous
	}
	return &ConcurrencyExceededError{Scope: "mixed"}
}

func (r *Runtime) allowedAuthIDsForAPIKey(ctx context.Context, apiKey string) ([]string, error) {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" || r == nil || r.clusterAdapter == nil {
		return nil, nil
	}
	store, ok := r.clusterAdapter.(channelScopedAuthStore)
	if !ok || store == nil {
		return nil, nil
	}
	return store.AllowedAuthIDsForAPIKey(ctx, apiKey)
}

func (r *Runtime) allowedDispatchIDsForAPIKey(ctx context.Context, apiKey string) ([]string, []string, error) {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" || r == nil || r.clusterAdapter == nil {
		return nil, nil, nil
	}
	if store, ok := r.clusterAdapter.(apiKeyScopedDispatchStore); ok && store != nil {
		return store.AllowedDispatchIDsForAPIKey(ctx, apiKey)
	}

	allowedAuthIDs, errAuthIDs := r.allowedAuthIDsForAPIKey(ctx, apiKey)
	if errAuthIDs != nil {
		return nil, nil, errAuthIDs
	}
	var allowedModelIDs []string
	if store, ok := r.clusterAdapter.(modelScopedAuthStore); ok && store != nil {
		modelIDs, errModelIDs := store.AllowedModelIDsForAPIKey(ctx, apiKey)
		if errModelIDs != nil {
			return nil, nil, errModelIDs
		}
		allowedModelIDs = modelIDs
	}
	return allowedAuthIDs, allowedModelIDs, nil
}

// supportsRequestedModel handles a supports requested model.
func (r *Runtime) supportsRequestedModel(model string) bool {
	if r == nil {
		return false
	}
	trimmedModel := strings.TrimSpace(model)
	if trimmedModel == "" {
		return false
	}
	modelKey := coreauth.CanonicalModelKey(trimmedModel)
	if modelKey == "" {
		modelKey = trimmedModel
	}
	return registry.LookupModelInfo(modelKey) != nil
}

// AddToken stores a credential JSON blob into the auth directory and schedules it for use.
// It returns the created (or existing) auth file name under auth-dir.
func (r *Runtime) AddToken(ctx context.Context, rawJSON string) (string, error) {
	// Resolve credential context before calling upstream OAuth services.
	if r == nil {
		return "", fmt.Errorf("home runtime: runtime is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	rawJSON = strings.TrimSpace(rawJSON)
	if rawJSON == "" {
		return "", fmt.Errorf("home runtime: empty token json")
	}
	if !gjson.Valid(rawJSON) {
		return "", fmt.Errorf("home runtime: invalid token json")
	}

	r.cfgMu.RLock()
	cfg := r.cfg
	authDir := r.authDir
	r.cfgMu.RUnlock()
	if cfg == nil {
		return "", fmt.Errorf("home runtime: config is nil")
	}
	if strings.TrimSpace(authDir) == "" {
		return "", fmt.Errorf("home runtime: auth-dir is empty")
	}

	tokenType := strings.TrimSpace(gjson.Get(rawJSON, "type").String())
	if tokenType == "" {
		return "", fmt.Errorf("home runtime: token json missing type")
	}

	sum := sha256.Sum256([]byte(rawJSON))
	token := hex.EncodeToString(sum[:8])
	baseName := strings.ToLower(tokenType) + "-" + token + ".json"
	fullPath := filepath.Join(authDir, baseName)

	if errMk := os.MkdirAll(authDir, 0o755); errMk != nil {
		return "", fmt.Errorf("home runtime: create auth dir: %w", errMk)
	}

	if _, errStat := os.Stat(fullPath); errStat == nil {
		r.applyAuthFile(ctx, fullPath, []byte(rawJSON))
		return baseName, nil
	} else if !os.IsNotExist(errStat) {
		return "", fmt.Errorf("home runtime: stat auth file: %w", errStat)
	}

	if errWrite := os.WriteFile(fullPath, []byte(rawJSON), 0o600); errWrite != nil {
		return "", fmt.Errorf("home runtime: write auth file: %w", errWrite)
	}

	r.applyAuthFile(ctx, fullPath, []byte(rawJSON))
	return baseName, nil
}

// applyAuthFile applies an auth file.
func (r *Runtime) applyAuthFile(ctx context.Context, fullPath string, data []byte) {
	// Normalize auth state before updating runtime indexes.
	if r == nil || r.coreManager == nil {
		return
	}
	r.cfgMu.RLock()
	cfg := r.cfg
	authDir := r.authDir
	r.cfgMu.RUnlock()
	if cfg == nil {
		return
	}

	sctx := &synthesizer.SynthesisContext{
		Config:           cfg,
		AuthDir:          authDir,
		Now:              time.Now(),
		IDGenerator:      synthesizer.NewStableIDGenerator(),
		PluginAuthParser: r,
	}

	auths := synthesizer.SynthesizeAuthFile(sctx, fullPath, data)
	if len(auths) == 0 {
		return
	}

	ctxSkipPersist := coreauth.WithSkipPersist(ctx)
	for _, a := range auths {
		r.applyCoreAuthAddOrUpdate(ctxSkipPersist, a)
	}
}

func (r *Runtime) refreshAccessProviders() {
	if r == nil || r.accessManager == nil {
		return
	}
	providers := access.RegisteredProviders()
	if provider := r.clusterAccessProvider(); provider != nil {
		providers = append(providers, provider)
	}
	r.accessManager.SetProviders(providers)
}

func (r *Runtime) clusterAccessProvider() access.Provider {
	if r == nil || r.clusterAdapter == nil || !r.clusterAdapter.Enabled() {
		return nil
	}
	validator, ok := r.clusterAdapter.(apiKeyValidator)
	if !ok || validator == nil {
		return nil
	}
	return newClusterAPIKeyAccessProvider(validator)
}

// authenticateRequest handles an authenticate request.
func (r *Runtime) authenticateRequest(ctx context.Context, headers http.Header) (*access.Result, *access.AuthError) {
	if r == nil || r.accessManager == nil {
		return nil, access.NewNoCredentialsError()
	}
	if ctx == nil {
		ctx = context.Background()
	}

	req, errReq := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost/", nil)
	if errReq != nil {
		return nil, access.NewNoCredentialsError()
	}
	if headers != nil {
		req.Header = headers.Clone()
	}

	return r.authenticateHTTPRequest(ctx, req)
}

func (r *Runtime) authenticateHTTPRequest(ctx context.Context, req *http.Request) (*access.Result, *access.AuthError) {
	if r == nil || r.accessManager == nil {
		return nil, access.NewNoCredentialsError()
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if req == nil {
		return nil, access.NewNoCredentialsError()
	}
	return r.accessManager.Authenticate(ctx, req)
}
