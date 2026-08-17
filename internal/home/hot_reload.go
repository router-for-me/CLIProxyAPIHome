package home

import (
	"context"
	"fmt"
	"os"
	"strings"

	configaccess "github.com/router-for-me/CLIProxyAPIHome/internal/access/config_access"
	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/managementasset"
	"github.com/router-for-me/CLIProxyAPIHome/internal/watcher"
	"github.com/router-for-me/CLIProxyAPIHome/internal/watcher/diff"
	log "github.com/sirupsen/logrus"
)

// startFileWatcher starts a file watcher.
func (r *Runtime) startFileWatcher(ctx context.Context, configPath string) error {
	// Keep validation before state changes so failures leave existing data intact.
	if r == nil {
		return nil
	}
	configPath = strings.TrimSpace(configPath)
	if configPath == "" {
		return nil
	}

	r.cfgMu.RLock()
	authDir := r.authDir
	r.cfgMu.RUnlock()

	w, errNew := watcher.NewWatcher(configPath, authDir, watcher.Callbacks{
		OnConfigYAMLChange: func(ctx context.Context, data []byte) {
			r.PublishConfigYAML(data)
		},
		OnConfigReload: func(ctx context.Context, cfg *config.Config) {
			if errApply := r.applyConfigAndReloadAuths(ctx, cfg); errApply != nil {
				log.Errorf("config reload apply failed: %v", errApply)
			}
		},
		OnAuthChange: func(ctx context.Context) {
			if errReload := r.ReloadAuths(ctx); errReload != nil {
				log.Errorf("auth reload failed: %v", errReload)
			}
		},
	})
	if errNew != nil {
		return fmt.Errorf("home runtime: init file watcher: %w", errNew)
	}

	if errStart := w.Start(ctx); errStart != nil {
		_ = w.Stop()
		return fmt.Errorf("home runtime: start file watcher: %w", errStart)
	}

	r.fileWatcher = w
	log.Infof("hot reload enabled (config=%s auth-dir=%s)", configPath, strings.TrimSpace(authDir))
	return nil
}

// applyConfigAndReloadAuths applies a config and reload auths.
func (r *Runtime) applyConfigAndReloadAuths(ctx context.Context, cfg *config.Config) error {
	// Normalize source data before building the derived payload.
	if r == nil {
		return nil
	}
	r.stateApplyMu.Lock()
	defer r.stateApplyMu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	if cfg == nil {
		cfg = &config.Config{}
	}
	r.cfgMu.RLock()
	oldCfg := r.cfg
	oldAuthDir := r.authDir
	r.cfgMu.RUnlock()
	globalDisableCoolingActivated := oldCfg != nil && !oldCfg.DisableCooling && cfg.DisableCooling
	globalDisableCoolingDeactivated := oldCfg != nil && oldCfg.DisableCooling && !cfg.DisableCooling
	if globalDisableCoolingDeactivated && r.coreManager != nil {
		if errFence := r.coreManager.FenceDisabledCooldownStates(ctx); errFence != nil {
			return fmt.Errorf("home runtime: fence disabled cooldown state before re-enabling cooling: %w", errFence)
		}
	}

	if !r.clusterAutoRefreshGated() && strings.TrimSpace(cfg.AuthDir) != "" {
		if errEnsure := os.MkdirAll(cfg.AuthDir, 0o755); errEnsure != nil {
			return fmt.Errorf("home runtime: ensure auth dir: %w", errEnsure)
		}
	}
	if errPluginSync := r.syncPluginStoreManifests(ctx, cfg); errPluginSync != nil {
		return errPluginSync
	}

	store := coreauth.GetTokenStore()
	if dirSetter, ok := store.(interface{ SetBaseDir(string) }); ok {
		dirSetter.SetBaseDir(cfg.AuthDir)
	}

	if r.coreManager != nil {
		r.coreManager.SetConfig(cfg)
		r.coreManager.SetOAuthModelAlias(cfg.OAuthModelAlias)
		r.coreManager.SetSelector(selectorFromConfig(cfg))
	}
	r.applyPluginConfig(ctx, cfg)

	r.cfgMu.Lock()
	r.cfg = cfg
	r.authDir = cfg.AuthDir
	r.cfgMu.Unlock()

	if errReload := r.loadAuths(coreauth.WithSkipPersist(ctx)); errReload != nil {
		if r.coreManager != nil {
			r.coreManager.SetConfig(oldCfg)
			if oldCfg == nil {
				r.coreManager.SetOAuthModelAlias(nil)
			} else {
				r.coreManager.SetOAuthModelAlias(oldCfg.OAuthModelAlias)
			}
			r.coreManager.SetSelector(selectorFromConfig(oldCfg))
		}
		r.applyPluginConfig(ctx, oldCfg)
		if dirSetter, ok := store.(interface{ SetBaseDir(string) }); ok {
			dirSetter.SetBaseDir(oldAuthDir)
		}
		r.cfgMu.Lock()
		r.cfg = oldCfg
		r.authDir = oldAuthDir
		r.cfgMu.Unlock()
		return errReload
	}

	currentLevel := log.GetLevel()
	if cfg.Debug {
		log.SetLevel(log.DebugLevel)
	} else {
		log.SetLevel(log.InfoLevel)
	}
	if nextLevel := log.GetLevel(); nextLevel != currentLevel {
		log.Infof("log level changed from %s to %s (debug=%t)", currentLevel, nextLevel, cfg.Debug)
	}
	logConfigChanges(oldCfg, cfg)

	r.deleteRemovedPluginArtifacts(ctx, oldCfg, cfg)
	configaccess.Register(&cfg.SDKConfig)
	r.refreshAccessProviders()
	managementasset.SetCurrentConfig(cfg)

	if globalDisableCoolingActivated && r.coreManager != nil {
		if errFence := r.coreManager.FenceDisabledCooldownStates(ctx); errFence != nil {
			log.WithError(errFence).Warn("home runtime: failed to fence disabled cooldown state; dispatch policy remains active")
		}
	}
	return nil
}

// ApplyConfigFromCluster updates apply config from cluster.
func (r *Runtime) ApplyConfigFromCluster(ctx context.Context, cfg *config.Config) error {
	return r.applyConfigAndReloadAuths(ctx, cfg)
}

func logConfigChanges(oldCfg, newCfg *config.Config) {
	if oldCfg == nil || newCfg == nil {
		return
	}
	details := diff.BuildConfigChangeDetails(oldCfg, newCfg)
	if len(details) == 0 {
		return
	}
	log.Infof("config changes detected:")
	for _, detail := range details {
		log.Infof("  %s", detail)
	}
}
