package home

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	sdkpluginhost "github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginhost"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginstore"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
)

const LocalExecutorCatalogWorkerArg = "--plugin-catalog-worker"

const (
	localExecutorCatalogResult  = "catalog.json"
	localExecutorCatalogReceipt = "catalog-v1.ready"
	localExecutorCatalogTimeout = 20 * time.Second
	localExecutorCatalogRetry   = 30 * time.Second
)

type localExecutorCatalog struct {
	configDigest string
	configKey    [sha256.Size]byte
	artifactKey  string
	ready        chan struct{}
	models       []registry.ModelInfo
	err          error
	retryAt      time.Time
}

type localExecutorCatalogLoader func(context.Context, *config.Config, pluginstore.Manifest, []byte) ([]registry.ModelInfo, error)

// SetLocalExecutorCatalogLoader permits an alternate artifact reader before the runtime starts.
func (r *Runtime) SetLocalExecutorCatalogLoader(loader localExecutorCatalogLoader) {
	r.localExecutorCatalogLoader = loader
}

func isLocalExecutorStoreConfig(item config.PluginInstanceConfig) bool {
	store := yamlMappingValue(&item.Raw, "store")
	tags := yamlMappingValue(store, "tags")
	if tags == nil {
		return false
	}
	var values []string
	if errDecode := tags.Decode(&values); errDecode != nil {
		return false
	}
	for _, tag := range values {
		if strings.EqualFold(strings.TrimSpace(tag), "ACP") {
			return true
		}
	}
	return false
}

type installedLocalExecutor struct {
	manifest pluginstore.Manifest
	item     config.PluginInstanceConfig
	catalog  *localExecutorCatalog
}

func (r *Runtime) loadLocalExecutorCatalogs(ctx context.Context, cfg *config.Config) []installedLocalExecutor {
	var plugins []installedLocalExecutor
	for id, item := range cfg.Plugins.Configs {
		if !pluginConfigEnabled(item) || !isLocalExecutorStoreConfig(item) {
			continue
		}
		manifest, installed, errManifest := storeManifestFromPluginConfig(id, item)
		if errManifest != nil {
			log.WithError(errManifest).WithField("plugin_id", id).Warn("ACP catalog unavailable")
			continue
		}
		if !installed || manifest.ID != id || normalizeDesiredPluginFileVersion(manifest.Version) == "" {
			continue
		}
		plugins = append(plugins, installedLocalExecutor{manifest: manifest, item: item})
	}
	sort.Slice(plugins, func(i, j int) bool {
		left, right := plugins[i], plugins[j]
		if left.item.Priority != right.item.Priority {
			return left.item.Priority > right.item.Priority
		}
		return left.manifest.ID < right.manifest.ID
	})
	var workers sync.WaitGroup
	for i := range plugins {
		entry := &plugins[i]
		workers.Go(func() {
			entry.catalog = r.localExecutorModels(ctx, cfg, entry.manifest, entry.item)
		})
	}
	workers.Wait()
	return plugins
}

func (r *Runtime) localExecutorModels(ctx context.Context, cfg *config.Config, manifest pluginstore.Manifest, item config.PluginInstanceConfig) *localExecutorCatalog {
	values := map[string]any{}
	if errDecode := item.Raw.Decode(&values); errDecode != nil {
		log.WithField("plugin_id", manifest.ID).Warn("invalid ACP plugin configuration")
		return nil
	}
	// Match the effective YAML defaults inserted by the CPA plugin host.
	if yamlMappingValue(&item.Raw, "enabled") == nil {
		values["enabled"] = pluginConfigEnabled(item)
	}
	if yamlMappingValue(&item.Raw, "priority") == nil {
		values["priority"] = item.Priority
	}
	raw, errEncode := yaml.Marshal(values)
	if errEncode != nil {
		log.WithField("plugin_id", manifest.ID).Warn("cannot encode ACP plugin configuration")
		return nil
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(raw))
	// The CPA digest uses canonical YAML. Preserve scalar tags in the catalog cache key.
	original, errOriginal := yaml.Marshal(&item.Raw)
	if errOriginal != nil {
		log.WithField("plugin_id", manifest.ID).Warn("cannot encode ACP plugin configuration")
		return nil
	}
	configKey := sha256.Sum256(original)
	artifactKey, errKey := pluginStoreManifestKey(pluginRootFromConfigDir(cfg.Plugins.Dir), CurrentPluginPlatform(), manifest)
	if errKey != nil {
		log.WithError(errKey).WithField("plugin_id", manifest.ID).Warn("ACP catalog unavailable")
		return nil
	}
	r.localExecutorCatalogMu.Lock()
	if catalog := r.localExecutorCatalogs[manifest.ID]; catalog != nil && catalog.configDigest == digest &&
		catalog.configKey == configKey && catalog.artifactKey == artifactKey &&
		(catalog.retryAt.IsZero() || time.Now().Before(catalog.retryAt)) {
		r.localExecutorCatalogMu.Unlock()
		select {
		case <-ctx.Done():
			return nil
		case <-catalog.ready:
			if catalog.err != nil {
				return nil
			}
			return catalog
		}
	}
	catalog := &localExecutorCatalog{configDigest: digest, configKey: configKey, artifactKey: artifactKey, ready: make(chan struct{})}
	if r.localExecutorCatalogs == nil {
		r.localExecutorCatalogs = make(map[string]*localExecutorCatalog)
	}
	r.localExecutorCatalogs[manifest.ID] = catalog
	r.localExecutorCatalogMu.Unlock()

	loadCtx, cancelLoad := context.WithTimeout(ctx, localExecutorCatalogTimeout)
	defer cancelLoad()
	if loader := r.localExecutorCatalogLoader; loader != nil {
		catalog.models, catalog.err = loader(loadCtx, cfg, manifest, raw)
	} else {
		catalog.models, catalog.err = r.readLocalExecutorCatalog(loadCtx, cfg, manifest, item, artifactKey)
	}
	if catalog.err == nil {
		catalog.err = loadCtx.Err()
	}
	r.localExecutorCatalogMu.Lock()
	if ctx.Err() != nil {
		if r.localExecutorCatalogs[manifest.ID] == catalog {
			delete(r.localExecutorCatalogs, manifest.ID)
		}
	} else if catalog.err != nil {
		catalog.retryAt = time.Now().Add(localExecutorCatalogRetry)
	}
	close(catalog.ready)
	r.localExecutorCatalogMu.Unlock()
	if catalog.err != nil {
		if ctx.Err() == nil {
			log.WithError(catalog.err).WithFields(log.Fields{"plugin_id": manifest.ID, "version": manifest.Version}).Warn("ACP catalog unavailable")
		}
		return nil
	}
	return catalog
}

func (r *Runtime) readLocalExecutorCatalog(ctx context.Context, cfg *config.Config, manifest pluginstore.Manifest, item config.PluginInstanceConfig, artifactKey string) ([]registry.ModelInfo, error) {
	platform := CurrentPluginPlatform()
	root := pluginRootFromConfigDir(cfg.Plugins.Dir)
	// Each pinned artifact gets its own directory outside Home runtime discovery.
	root, errRoot := filepath.Abs(filepath.Join(root, ".acp-catalog", artifactKey))
	if errRoot != nil {
		return nil, errRoot
	}
	path, errPath := currentPluginFilePath(root, manifest.ID, manifest.Version)
	if errPath != nil {
		return nil, errPath
	}
	if path == "" {
		auth, errAuth := r.resolvePluginStoreAuth(ctx)
		if errAuth != nil {
			return nil, errAuth
		}
		defer pluginstore.ClearResolvedAuthConfigs(auth)
		client := newPluginStoreClient(cfg, auth)
		if _, errInstall := client.InstallManifest(ctx, manifest, pluginStoreInstallOptions(root, platform, nil, manifest.ID)); errInstall != nil {
			return nil, errInstall
		}
	}
	executable, errExe := os.Executable()
	if errExe != nil {
		return nil, errExe
	}
	version := normalizeDesiredPluginFileVersion(manifest.Version)
	runWorker := func(probe, requireReceipt bool, projectedConfig []byte) (localExecutorCatalogResponse, error) {
		var response localExecutorCatalogResponse
		request, errRequest := json.Marshal(localExecutorCatalogRequest{
			Directory: root, Plugin: manifest.ID, Version: version, Priority: item.Priority,
			Config: projectedConfig, Probe: probe, RequireReceipt: requireReceipt,
		})
		if errRequest != nil {
			return response, errRequest
		}
		workerDir, errDir := os.MkdirTemp("", "home-plugin-catalog-")
		if errDir != nil {
			return response, errDir
		}
		defer func() {
			if errRemove := os.RemoveAll(workerDir); errRemove != nil {
				log.WithError(errRemove).Debug("remove plugin catalog worker directory")
			}
		}()
		cmd := exec.CommandContext(ctx, executable, LocalExecutorCatalogWorkerArg)
		cmd.Dir = workerDir
		cmd.Stdin = bytes.NewReader(request)
		cmd.Env = []string{
			"HOME=" + workerDir, "USERPROFILE=" + workerDir, "XDG_CONFIG_HOME=" + workerDir, "XDG_CACHE_HOME=" + workerDir,
			"APPDATA=" + workerDir, "LOCALAPPDATA=" + workerDir, "TEMP=" + workerDir, "TMP=" + workerDir, "TMPDIR=" + workerDir,
		}
		cmd.Env = append(cmd.Env, "CLIPROXY_PLUGIN_MODE=catalog")
		if errRun := cmd.Run(); errRun != nil {
			if errContext := ctx.Err(); errContext != nil {
				return response, errContext
			}
			return response, errRun
		}
		result, errOpen := os.Open(filepath.Join(workerDir, localExecutorCatalogResult))
		if errOpen != nil {
			return response, errOpen
		}
		defer func() {
			if errClose := result.Close(); errClose != nil {
				log.WithError(errClose).Debug("close plugin catalog worker result")
			}
		}()
		errDecode := json.NewDecoder(io.LimitReader(result, 16<<20)).Decode(&response)
		return response, errDecode
	}
	probe, errProbe := runWorker(true, false, nil)
	if errProbe != nil {
		return nil, fmt.Errorf("probe plugin catalog: %w", errProbe)
	}
	projected, errProject := localExecutorWorkerConfig(&item.Raw, probe)
	if errProject != nil {
		return nil, errProject
	}
	response, errModels := runWorker(false, probe.Ready, projected)
	if errModels != nil {
		return nil, fmt.Errorf("read configured plugin model catalog: %w", errModels)
	}
	out := make([]registry.ModelInfo, 0, len(response.Models))
	for _, model := range response.Models {
		out = append(out, *pluginModelToHomeModel(model))
	}
	return out, nil
}

func localExecutorWorkerConfig(raw *yaml.Node, probe localExecutorCatalogResponse) ([]byte, error) {
	var values map[string]yaml.Node
	if errDecode := raw.Decode(&values); errDecode != nil {
		return nil, errors.New("decode ACP catalog configuration")
	}
	for _, key := range []string{"enabled", "priority", "store", "load-in-home", "home"} {
		delete(values, key)
	}
	if !probe.Ready {
		if len(values) != 0 {
			return nil, errors.New("plugin catalog incompatible: configured plugin did not confirm catalog-v1 mode")
		}
		return nil, nil
	}
	projected := make(map[string]yaml.Node)
	for _, field := range probe.Fields {
		if value, present := values[field]; present {
			projected[field] = catalogConfigValue(&value)
		}
	}
	return yaml.Marshal(projected)
}

// Copy the value without comments or references to unprojected anchors.
func catalogConfigValue(node *yaml.Node) yaml.Node {
	if node.Kind == yaml.AliasNode {
		return catalogConfigValue(node.Alias)
	}
	value := *node
	value.HeadComment, value.LineComment, value.FootComment, value.Anchor = "", "", "", ""
	value.Content = nil
	for _, child := range node.Content {
		copyChild := catalogConfigValue(child)
		value.Content = append(value.Content, &copyChild)
	}
	return value
}

type localExecutorCatalogRequest struct {
	Directory      string
	Plugin         string
	Version        string
	Priority       int
	Config         []byte
	Probe          bool
	RequireReceipt bool
}

type localExecutorCatalogResponse struct {
	Ready  bool
	Fields []string                  `json:",omitempty"`
	Models []sdkpluginhost.ModelInfo `json:",omitempty"`
}

// RunLocalExecutorCatalogWorker loads just the selected artifact in a short-lived process.
func RunLocalExecutorCatalogWorker(ctx context.Context, input io.Reader) error {
	var request localExecutorCatalogRequest
	if errDecode := json.NewDecoder(input).Decode(&request); errDecode != nil {
		return errDecode
	}
	var raw yaml.Node
	if errEncode := raw.Encode(map[string]any{"store": map[string]string{"version": request.Version}}); errEncode != nil {
		return errEncode
	}
	if len(request.Config) != 0 {
		var projected yaml.Node
		if errDecode := yaml.Unmarshal(request.Config, &projected); errDecode != nil {
			return errDecode
		}
		if len(projected.Content) != 1 || projected.Content[0].Kind != yaml.MappingNode {
			return errors.New("invalid projected plugin catalog configuration")
		}
		raw.Content = append(raw.Content, projected.Content[0].Content...)
	}
	enabled := true
	host := sdkpluginhost.New()
	defer host.ShutdownAllContext(ctx)
	host.ApplyConfig(ctx, sdkpluginhost.RuntimeConfig{
		Enabled: true, Dir: request.Directory,
		Configs: map[string]sdkpluginhost.PluginInstanceConfig{
			request.Plugin: {Enabled: &enabled, Priority: request.Priority, Raw: raw},
		},
	})
	var response localExecutorCatalogResponse
	if request.Probe || request.RequireReceipt {
		info, errStat := os.Lstat(localExecutorCatalogReceipt)
		switch {
		case errStat == nil && info.Mode().IsRegular() && info.Size() == 1:
			receipt, errRead := os.ReadFile(localExecutorCatalogReceipt)
			if errRead != nil || string(receipt) != "1" {
				return errors.New("invalid plugin catalog-v1 receipt")
			}
			response.Ready = true
		case !errors.Is(errStat, os.ErrNotExist) || request.RequireReceipt:
			return errors.New("invalid or missing plugin catalog-v1 receipt")
		}
	}
	if request.Probe {
		plugins := host.RegisteredPlugins()
		if len(plugins) != 1 || plugins[0].ID != request.Plugin {
			return errors.New("catalog plugin registration unavailable")
		}
		if response.Ready {
			for _, field := range plugins[0].Metadata.ConfigFields {
				response.Fields = append(response.Fields, field.Name)
			}
		}
	} else {
		models, errModels := host.StaticExecutorModels(ctx, request.Plugin, request.Version)
		if errModels != nil {
			return errModels
		}
		response.Models = models
	}
	snapshot, errEncode := json.Marshal(response)
	if errEncode != nil {
		return errEncode
	}
	return os.WriteFile(localExecutorCatalogResult, snapshot, 0o600)
}
