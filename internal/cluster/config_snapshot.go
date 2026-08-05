package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	appconfig "github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/watcher/synthesizer"
	"gopkg.in/yaml.v3"
	"gorm.io/gorm"
)

// LoadConfigAsRuntimeConfig loads a config as runtime config.
func (r *Repository) LoadConfigAsRuntimeConfig(ctx context.Context) (*appconfig.Config, []byte, error) {
	// Normalize source data before building the derived payload.
	snapshot, errSnapshot := r.LoadConfigSnapshot(ctx)
	if errSnapshot != nil {
		return nil, nil, errSnapshot
	}
	root, errRoot := ConfigRootFromSnapshot(snapshot)
	if errRoot != nil {
		return nil, nil, errRoot
	}
	lifecycleConfig, errLifecycle := r.LifecycleConfig(ctx)
	if errLifecycle != nil {
		return nil, nil, errLifecycle
	}
	observationBarrierRevision, errBarrier := r.concurrencyObservationBarrierRevision(ctx)
	if errBarrier != nil {
		return nil, nil, errBarrier
	}
	lifecycleConfig.ObservationBarrierRevision = observationBarrierRevision
	root["credential-concurrency"] = lifecycleConfig
	secretChanged, errSecret := normalizeConfigRootSecrets(root)
	if errSecret != nil {
		return nil, nil, errSecret
	}
	authRevision, errAuthRevision := r.PluginStoreAuthRevision(ctx)
	if errAuthRevision != nil {
		return nil, nil, errAuthRevision
	}
	if errProject := projectPluginAuthConfig(root, authRevision); errProject != nil {
		return nil, nil, errProject
	}
	cfg, payload, errConfig := RuntimeConfigFromRoot(root)
	if errConfig != nil {
		return nil, nil, errConfig
	}
	if secretChanged {
		if errUpsert := r.UpsertConfigValue(ctx, "remote-management", root["remote-management"]); errUpsert != nil {
			return nil, nil, errUpsert
		}
	}
	return cfg, payload, nil
}

func (r *Repository) reconcileConfigSnapshotProviderAuthsTx(ctx context.Context, tx *gorm.DB, values map[string]any) error {
	if tx == nil {
		return fmt.Errorf("database connection is nil")
	}
	if errLock := r.lockAllProviderAuthsForReconciliationTx(ctx, tx); errLock != nil {
		return errLock
	}
	explicitKeys := explicitProviderCredentialConfigKeys(values)
	if len(explicitKeys) == 0 {
		return nil
	}
	cfg, _, errConfig := RuntimeConfigFromRoot(values)
	if errConfig != nil {
		return errConfig
	}
	sctx := &synthesizer.SynthesisContext{
		Config:      cfg,
		Now:         time.Now().UTC(),
		IDGenerator: synthesizer.NewStableIDGenerator(),
	}
	auths, errSynthesize := synthesizer.NewConfigSynthesizer().Synthesize(sctx)
	if errSynthesize != nil {
		return errSynthesize
	}
	exported := make(map[string]any, len(explicitKeys))
	ApplyCredentialConfigToRoot(exported, auths)
	for key := range explicitKeys {
		if value, exists := exported[key]; exists {
			values[key] = value
		}
		next := providerAuthsForConfigKey(auths, key)
		if errReconcile := r.reconcileProviderAuthsWithLockedActivationGateTx(ctx, tx, key, next, ConcurrencyCredentialReferenceChecker{}); errReconcile != nil {
			return errReconcile
		}
	}
	return nil
}

func explicitProviderCredentialConfigKeys(values map[string]any) map[string]struct{} {
	explicit := make(map[string]struct{})
	for _, key := range providerCredentialConfigKeys {
		if _, exists := values[key]; exists {
			explicit[key] = struct{}{}
		}
	}
	return explicit
}

func providerAuthsForConfigKey(auths []*coreauth.Auth, key string) []*coreauth.Auth {
	next := make([]*coreauth.Auth, 0, len(auths))
	for _, auth := range auths {
		if isProviderAuthForConfigKey(auth, key) {
			next = append(next, auth)
		}
	}
	return next
}

// ConfigRootFromSnapshot derives config root from snapshot.
func ConfigRootFromSnapshot(snapshot map[string]json.RawMessage) (map[string]any, error) {
	root := make(map[string]any, len(snapshot))
	for key, raw := range snapshot {
		if isClusterCredentialConfigKey(key) || strings.TrimSpace(key) == credentialConcurrencyPoliciesRootKey {
			continue
		}
		var value any
		if len(raw) > 0 {
			if errUnmarshal := json.Unmarshal(raw, &value); errUnmarshal != nil {
				return nil, errUnmarshal
			}
		}
		root[key] = value
	}
	return root, nil
}

// RuntimeConfigFromRoot derives runtime config from root.
func RuntimeConfigFromRoot(root map[string]any) (*appconfig.Config, []byte, error) {
	// Normalize source data before building the derived payload.
	if _, errSecret := normalizeConfigRootSecrets(root); errSecret != nil {
		return nil, nil, errSecret
	}
	data, errMarshal := yaml.Marshal(root)
	if errMarshal != nil {
		return nil, nil, errMarshal
	}
	cfg := &appconfig.Config{}
	cfg.CredentialConcurrency = appconfig.DefaultCredentialConcurrencyConfig()
	cfg.CredentialInFlight = appconfig.DefaultCredentialInFlightConfig()
	cfg.Pprof.Addr = appconfig.DefaultPprofAddr
	cfg.RemoteManagement.PanelGitHubRepository = appconfig.DefaultPanelGitHubRepository
	cfg.ErrorLogsMaxFiles = 10
	cfg.RedisUsageQueueRetentionSeconds = 60
	if errUnmarshal := yaml.Unmarshal(data, cfg); errUnmarshal != nil {
		return nil, nil, errUnmarshal
	}
	if errValidate := cfg.CredentialInFlight.Validate(); errValidate != nil {
		return nil, nil, errValidate
	}
	if errValidate := appconfig.ValidateCredentialConcurrencyConfig(cfg.CredentialConcurrency); errValidate != nil {
		return nil, nil, errValidate
	}
	if errNormalizeIDs := cfg.NormalizeProviderCredentialIDs(); errNormalizeIDs != nil {
		return nil, nil, errNormalizeIDs
	}
	cfg.NormalizePluginsConfig()
	cfg.NormalizeUserEmailConfig()
	cfg.NormalizeTrustedProxies()
	if errTrustedProxies := appconfig.ValidateTrustedProxies(cfg.TrustedProxies); errTrustedProxies != nil {
		return nil, nil, errTrustedProxies
	}
	cfg.SanitizeGeminiKeys()
	cfg.SanitizeVertexCompatKeys()
	cfg.SanitizeCodexKeys()
	cfg.SanitizeXAIKeys()
	cfg.SanitizeCodexHeaderDefaults()
	cfg.SanitizeClaudeHeaderDefaults()
	cfg.SanitizeClaudeKeys()
	cfg.SanitizeOpenAICompatibility()
	cfg.SanitizeOAuthModelAlias()
	cfg.SanitizePayloadRules()
	if cfg.Pprof.Addr == "" {
		cfg.Pprof.Addr = appconfig.DefaultPprofAddr
	}
	if cfg.RemoteManagement.PanelGitHubRepository == "" {
		cfg.RemoteManagement.PanelGitHubRepository = appconfig.DefaultPanelGitHubRepository
	}
	appconfig.ForceHomeRuntimeConfig(cfg)
	return cfg, data, nil
}

func projectPluginAuthConfig(root map[string]any, authRevision int64) error {
	if root == nil {
		return nil
	}
	var plugins map[string]any
	if rawPlugins, exists := root["plugins"]; exists && rawPlugins != nil {
		current, okPlugins := rawPlugins.(map[string]any)
		if !okPlugins {
			return fmt.Errorf("plugins config must be a mapping")
		}
		plugins = make(map[string]any, len(current))
		for key, value := range current {
			plugins[key] = value
		}
	}
	if plugins == nil && authRevision <= 0 {
		return nil
	}
	if plugins == nil {
		plugins = map[string]any{}
	}
	delete(plugins, "store-auth")
	delete(plugins, "sync-revision")
	if authRevision > 0 {
		plugins["auth-revision"] = authRevision
	} else {
		delete(plugins, "auth-revision")
	}
	root["plugins"] = plugins
	return nil
}

// normalizeConfigRootSecrets normalizes a config root secrets.
func normalizeConfigRootSecrets(root map[string]any) (bool, error) {
	// Normalize source data before building the derived payload.
	if len(root) == 0 {
		return false, nil
	}
	rawRemoteManagement, ok := root["remote-management"]
	if !ok || rawRemoteManagement == nil {
		return false, nil
	}
	remoteManagement, ok := rawRemoteManagement.(map[string]any)
	if !ok {
		return false, nil
	}
	rawSecret, ok := remoteManagement["secret-key"]
	if !ok || rawSecret == nil {
		return false, nil
	}
	secret, ok := rawSecret.(string)
	if !ok {
		return false, nil
	}
	normalizedSecret, changed, errNormalizeSecret := appconfig.NormalizeRemoteManagementSecret(secret)
	if errNormalizeSecret != nil {
		return false, errNormalizeSecret
	}
	if !changed {
		return false, nil
	}
	remoteManagement["secret-key"] = normalizedSecret
	root["remote-management"] = remoteManagement
	return true, nil
}

// isClusterCredentialConfigKey reports whether cluster credential config key.
func isClusterCredentialConfigKey(key string) bool {
	switch strings.TrimSpace(key) {
	case "auth-dir", "credential-concurrency", "gemini-api-key", "vertex-api-key", "codex-api-key", "xai-api-key", "claude-api-key", "openai-compatibility":
		return true
	default:
		return false
	}
}
