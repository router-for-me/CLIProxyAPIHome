package management

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginstore"
	appconfig "github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/node"
	"github.com/router-for-me/CLIProxyAPIHome/internal/util"
	"gopkg.in/yaml.v3"
)

const pluginStoreRequestTimeout = 2 * time.Minute

type pluginStoreListResponse struct {
	PluginsEnabled bool                   `json:"plugins_enabled"`
	PluginsDir     string                 `json:"plugins_dir"`
	Sources        []pluginStoreSource    `json:"sources"`
	SourceErrors   []pluginStoreSourceErr `json:"source_errors,omitempty"`
	Plugins        []pluginStoreListEntry `json:"plugins"`
}

type pluginStoreSource struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	URL  string `json:"url"`
}

type pluginStoreSourceErr struct {
	SourceID   string `json:"source_id"`
	SourceName string `json:"source_name"`
	SourceURL  string `json:"source_url"`
	Message    string `json:"message"`
}

type pluginStoreListEntry struct {
	StoreID          string                `json:"store_id"`
	SourceID         string                `json:"source_id"`
	SourceName       string                `json:"source_name"`
	SourceURL        string                `json:"source_url"`
	ID               string                `json:"id"`
	Name             string                `json:"name"`
	Description      string                `json:"description"`
	Author           string                `json:"author"`
	Version          string                `json:"version"`
	Repository       string                `json:"repository"`
	InstallType      string                `json:"install_type"`
	AuthRequired     bool                  `json:"auth_required"`
	Platforms        []pluginStorePlatform `json:"platforms,omitempty"`
	Logo             string                `json:"logo,omitempty"`
	Homepage         string                `json:"homepage,omitempty"`
	License          string                `json:"license,omitempty"`
	Tags             []string              `json:"tags,omitempty"`
	Installed        bool                  `json:"installed"`
	InstalledVersion string                `json:"installed_version"`
	Path             string                `json:"path"`
	Configured       bool                  `json:"configured"`
	Registered       bool                  `json:"registered"`
	Enabled          bool                  `json:"enabled"`
	EffectiveEnabled bool                  `json:"effective_enabled"`
	UpdateAvailable  bool                  `json:"update_available"`
}

type pluginStorePlatform struct {
	GOOS   string `json:"goos"`
	GOARCH string `json:"goarch"`
}

type pluginInstallResponse struct {
	Status          string `json:"status"`
	SourceID        string `json:"source_id"`
	SourceName      string `json:"source_name"`
	SourceURL       string `json:"source_url"`
	ID              string `json:"id"`
	Version         string `json:"version"`
	InstallType     string `json:"install_type"`
	Path            string `json:"path"`
	PluginsEnabled  bool   `json:"plugins_enabled"`
	RestartRequired bool   `json:"restart_required"`
}

type pluginInstallRequest struct {
	Version string `json:"version"`
}

type pluginUninstallResponse struct {
	Status            string          `json:"status"`
	ID                string          `json:"id"`
	Task              node.PluginTask `json:"task"`
	ConfiguredRemoved bool            `json:"configured_removed"`
	TargetNodeType    string          `json:"target_node_type"`
	TargetNodeID      string          `json:"target_node_id,omitempty"`
	RestartRequired   bool            `json:"restart_required"`
}

type sourcedPlugin struct {
	source pluginstore.Source
	plugin pluginstore.Plugin
}

func (h *Handler) UninstallPluginFromStore(c *gin.Context) {
	id := strings.TrimSpace(c.Param("id"))
	if !validManagementPluginID(id) {
		respondError(c, http.StatusBadRequest, "invalid_plugin_id", fmt.Errorf("invalid plugin id"))
		return
	}
	ctx, cancel := h.pluginStoreRequestContext(c)
	defer cancel()

	configuredRemoved := false
	deleteTask := node.PluginTask{
		Operation:      node.PluginTaskOperationDelete,
		PluginID:       id,
		TargetNodeType: node.PluginTaskTargetAll,
	}
	_, root, errConfig := h.currentConfig(ctx)
	if errConfig != nil {
		respondError(c, http.StatusInternalServerError, "config_load_failed", errConfig)
		return
	}
	configuredRemoved = removePluginConfig(root, id)
	if _, errValidate := configFromRoot(root); errValidate != nil {
		respondError(c, http.StatusUnprocessableEntity, "invalid_config", errValidate)
		return
	}
	task, errTask := h.repo.ReplaceConfigSnapshotWithLifecycleConfigAndCreatePluginTask(ctx, h.heartbeatTimeout, root, deleteTask)
	if errTask != nil {
		respondError(c, http.StatusInternalServerError, "plugin_task_create_failed", errTask)
		return
	}
	if errRefresh := h.refreshConfig(ctx); errRefresh != nil {
		respondError(c, http.StatusInternalServerError, "reload_failed", errRefresh)
		return
	}

	c.JSON(http.StatusOK, pluginUninstallResponse{
		Status:            "uninstalled",
		ID:                trimString(id),
		Task:              task,
		ConfiguredRemoved: configuredRemoved,
		TargetNodeType:    trimString(task.TargetNodeType),
		TargetNodeID:      trimString(task.TargetNodeID),
		RestartRequired:   false,
	})
}

func (h *Handler) ListPluginStore(c *gin.Context) {
	ctx, cancel := h.pluginStoreRequestContext(c)
	defer cancel()
	auth, errAuth := h.pluginStoreAuth.Resolved(ctx)
	if errAuth != nil {
		respondError(c, http.StatusInternalServerError, "plugin_store_auth_resolve_failed", errAuth)
		return
	}
	defer pluginstore.ClearResolvedAuthConfigs(auth)

	cfg, _, errConfig := h.currentConfig(ctx)
	if errConfig != nil {
		respondError(c, http.StatusInternalServerError, "config_load_failed", errConfig)
		return
	}
	sources, errSources := pluginStoreSources(cfg)
	if errSources != nil {
		respondError(c, http.StatusInternalServerError, "plugin_store_source_invalid", errSources)
		return
	}
	plugins, sourceErrors := h.fetchSourcedPlugins(ctx, cfg, sources, auth)
	if len(plugins) == 0 && len(sourceErrors) > 0 {
		c.JSON(http.StatusBadGateway, gin.H{"error": "plugin_store_registry_failed", "message": sourceErrors[0].Message})
		return
	}

	entries := make([]pluginStoreListEntry, 0, len(plugins))
	for _, item := range plugins {
		manifest, configured := configuredPluginStoreManifest(item.plugin.ID, cfg)
		installedVersion := ""
		if configured {
			installedVersion = manifest.Version
		}
		enabled := false
		if cfg != nil {
			enabled = pluginInstanceEnabled(cfg.Plugins.Configs[item.plugin.ID])
		}
		storeVersion := item.plugin.Version
		if storeVersion == "" {
			storeVersion = installedVersion
		}
		entries = append(entries, pluginStoreListEntry{
			StoreID:          trimString(item.source.ID + "/" + item.plugin.ID),
			SourceID:         trimString(item.source.ID),
			SourceName:       trimString(item.source.Name),
			SourceURL:        trimString(item.source.URL),
			ID:               trimString(item.plugin.ID),
			Name:             trimString(item.plugin.Name),
			Description:      trimString(item.plugin.Description),
			Author:           trimString(item.plugin.Author),
			Version:          trimString(storeVersion),
			Repository:       trimString(item.plugin.Repository),
			InstallType:      trimString(pluginstore.PluginInstallType(item.plugin)),
			AuthRequired:     item.plugin.AuthRequired,
			Platforms:        sanitizePluginStorePlatforms(pluginstore.PluginPlatforms(item.plugin)),
			Logo:             trimString(item.plugin.Logo),
			Homepage:         trimString(item.plugin.Homepage),
			License:          trimString(item.plugin.License),
			Tags:             trimStrings(item.plugin.Tags),
			Installed:        configured,
			InstalledVersion: trimString(installedVersion),
			Configured:       configured,
			Enabled:          enabled,
			EffectiveEnabled: cfg != nil && cfg.Plugins.Enabled && enabled,
			UpdateAvailable:  pluginstore.UpdateAvailable(installedVersion, storeVersion),
		})
	}

	c.JSON(http.StatusOK, pluginStoreListResponse{
		PluginsEnabled: cfg != nil && cfg.Plugins.Enabled,
		PluginsDir:     trimString(pluginStoreDir(cfg)),
		Sources:        sanitizePluginStoreSources(sources),
		SourceErrors:   sanitizePluginStoreSourceErrors(sourceErrors),
		Plugins:        entries,
	})
}

func (h *Handler) InstallPluginFromStore(c *gin.Context) {
	id := strings.TrimSpace(c.Param("id"))
	if id == "" {
		respondError(c, http.StatusBadRequest, "invalid_plugin_id", fmt.Errorf("plugin id is required"))
		return
	}
	requestedVersion, errVersionRequest := pluginInstallRequestedVersion(c)
	if errVersionRequest != nil {
		respondError(c, http.StatusBadRequest, "invalid_request", errVersionRequest)
		return
	}
	ctx, cancel := h.pluginStoreRequestContext(c)
	defer cancel()
	auth, errAuth := h.pluginStoreAuth.Resolved(ctx)
	if errAuth != nil {
		respondError(c, http.StatusInternalServerError, "plugin_store_auth_resolve_failed", errAuth)
		return
	}
	defer pluginstore.ClearResolvedAuthConfigs(auth)

	cfg, root, errConfig := h.currentConfig(ctx)
	if errConfig != nil {
		respondError(c, http.StatusInternalServerError, "config_load_failed", errConfig)
		return
	}
	sources, errSources := pluginStoreSources(cfg)
	if errSources != nil {
		respondError(c, http.StatusInternalServerError, "plugin_store_source_invalid", errSources)
		return
	}
	source, plugin, client, okPlugin := h.findPluginStoreInstallTarget(ctx, cfg, sources, id, c.Query("source"), auth, c)
	if !okPlugin {
		return
	}
	manifest, errorCode, errManifest := pluginStoreInstallManifest(ctx, client, source, plugin, requestedVersion)
	if errManifest != nil {
		if errorCode == "" {
			errorCode = "plugin_manifest_invalid"
		}
		respondError(c, http.StatusBadGateway, errorCode, errManifest)
		return
	}

	upsertPluginStoreManifest(root, id, manifest)
	if _, errValidate := configFromRoot(root); errValidate != nil {
		respondError(c, http.StatusUnprocessableEntity, "invalid_config", errValidate)
		return
	}
	if errReplace := h.replaceConfigSnapshot(ctx, root); errReplace != nil {
		respondError(c, http.StatusInternalServerError, "write_failed", errReplace)
		return
	}
	if errRefresh := h.refreshConfig(ctx); errRefresh != nil {
		respondError(c, http.StatusInternalServerError, "reload_failed", errRefresh)
		return
	}

	c.JSON(http.StatusOK, pluginInstallResponse{
		Status:          "installed",
		SourceID:        trimString(source.ID),
		SourceName:      trimString(source.Name),
		SourceURL:       trimString(source.URL),
		ID:              trimString(manifest.ID),
		Version:         trimString(manifest.Version),
		InstallType:     trimString(manifest.InstallType()),
		Path:            "",
		PluginsEnabled:  cfg != nil && cfg.Plugins.Enabled,
		RestartRequired: false,
	})
}

func pluginStoreInstallManifest(ctx context.Context, client pluginstore.Client, source pluginstore.Source, plugin pluginstore.Plugin, requestedVersion string) (pluginstore.Manifest, string, error) {
	switch pluginstore.PluginInstallType(plugin) {
	case pluginstore.InstallTypeDirect:
		manifest, errManifest := pluginStoreDirectManifest(source, plugin, requestedVersion)
		if errManifest != nil {
			return pluginstore.Manifest{}, "plugin_manifest_invalid", errManifest
		}
		return manifest, "", nil
	case pluginstore.InstallTypeGitHubRelease:
		release, errRelease := fetchPluginStoreInstallRelease(ctx, client, plugin, requestedVersion)
		if errRelease != nil {
			return pluginstore.Manifest{}, "plugin_release_failed", errRelease
		}
		manifest, errManifest := pluginstore.ManifestFromRelease(source, plugin, release)
		if errManifest != nil {
			return pluginstore.Manifest{}, "plugin_release_invalid", errManifest
		}
		return manifest, "", nil
	default:
		return pluginstore.Manifest{}, "plugin_manifest_invalid", fmt.Errorf("unsupported install type %q", plugin.Install.Type)
	}
}

func pluginStoreDirectManifest(source pluginstore.Source, plugin pluginstore.Plugin, requestedVersion string) (pluginstore.Manifest, error) {
	version := normalizePluginStoreRequestedVersion(requestedVersion)
	if version == "" {
		version = normalizePluginStoreRequestedVersion(plugin.Version)
	}
	if normalizePluginStoreRequestedVersion(plugin.Version) == version {
		plugin.Version = version
		return pluginstore.ManifestFromPlugin(source, plugin)
	}
	for _, candidate := range plugin.Versions {
		if normalizePluginStoreRequestedVersion(candidate.Version) != version {
			continue
		}
		plugin.Version = version
		plugin.Install = candidate.Install
		if strings.TrimSpace(plugin.Install.Type) == "" {
			plugin.Install.Type = pluginstore.InstallTypeDirect
		}
		return pluginstore.ManifestFromPlugin(source, plugin)
	}
	return pluginstore.Manifest{}, fmt.Errorf("direct plugin version %q not found", version)
}

func pluginInstallRequestedVersion(c *gin.Context) (string, error) {
	requestedVersion := strings.TrimSpace(c.Query("version"))
	if c == nil || c.Request == nil || c.Request.Body == nil || c.Request.Body == http.NoBody {
		return requestedVersion, nil
	}
	body, errRead := io.ReadAll(c.Request.Body)
	if errRead != nil {
		return "", fmt.Errorf("read install request: %w", errRead)
	}
	if strings.TrimSpace(string(body)) == "" {
		return requestedVersion, nil
	}
	var req pluginInstallRequest
	if errDecode := json.Unmarshal(body, &req); errDecode != nil {
		return "", fmt.Errorf("decode install request: %w", errDecode)
	}
	bodyVersion := strings.TrimSpace(req.Version)
	if requestedVersion == "" {
		return bodyVersion, nil
	}
	if bodyVersion == "" || normalizePluginStoreRequestedVersion(bodyVersion) == normalizePluginStoreRequestedVersion(requestedVersion) {
		return requestedVersion, nil
	}
	return "", fmt.Errorf("version query %q does not match request body version %q", requestedVersion, bodyVersion)
}

func fetchPluginStoreInstallRelease(ctx context.Context, client pluginstore.Client, plugin pluginstore.Plugin, version string) (pluginstore.Release, error) {
	version = strings.TrimSpace(version)
	if version == "" {
		return client.FetchLatestRelease(ctx, plugin)
	}
	tags := pluginStoreReleaseTagCandidates(version)
	errs := make([]error, 0, len(tags))
	for _, tag := range tags {
		release, errRelease := client.FetchReleaseByTag(ctx, plugin, tag)
		if errRelease == nil {
			return release, nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", tag, errRelease))
	}
	return pluginstore.Release{}, fmt.Errorf("fetch release by tag: %w", errors.Join(errs...))
}

func pluginStoreReleaseTagCandidates(version string) []string {
	version = strings.TrimSpace(version)
	if version == "" {
		return nil
	}
	if strings.HasPrefix(strings.ToLower(version), "v") {
		return []string{version, strings.TrimSpace(version[1:])}
	}
	return []string{version, "v" + version}
}

func normalizePluginStoreRequestedVersion(version string) string {
	version = strings.TrimSpace(version)
	if strings.HasPrefix(strings.ToLower(version), "v") {
		return strings.TrimSpace(version[1:])
	}
	return version
}

func (h *Handler) pluginStoreRequestContext(c *gin.Context) (context.Context, context.CancelFunc) {
	ctx := context.Background()
	if c != nil && c.Request != nil && c.Request.Context() != nil {
		ctx = c.Request.Context()
	}
	return context.WithTimeout(ctx, pluginStoreRequestTimeout)
}

func (h *Handler) findPluginStoreInstallTarget(ctx context.Context, cfg *appconfig.Config, sources []pluginstore.Source, id string, requestedSourceID string, auth []pluginstore.ResolvedAuthConfig, c *gin.Context) (pluginstore.Source, pluginstore.Plugin, pluginstore.Client, bool) {
	requestedSourceID = strings.TrimSpace(requestedSourceID)
	if requestedSourceID != "" {
		for _, source := range sources {
			if source.ID != requestedSourceID {
				continue
			}
			client := h.newPluginStoreClient(cfg, source.URL, auth)
			registry, errRegistry := client.FetchRegistry(ctx)
			if errRegistry != nil {
				c.JSON(http.StatusBadGateway, gin.H{"error": "plugin_store_registry_failed", "message": errRegistry.Error()})
				return pluginstore.Source{}, pluginstore.Plugin{}, pluginstore.Client{}, false
			}
			plugin, okPlugin := registry.PluginByID(id)
			if !okPlugin {
				c.JSON(http.StatusNotFound, gin.H{"error": "plugin_not_found", "message": "plugin not found in registry source"})
				return pluginstore.Source{}, pluginstore.Plugin{}, pluginstore.Client{}, false
			}
			return source, plugin, client, true
		}
		c.JSON(http.StatusNotFound, gin.H{"error": "plugin_store_source_not_found", "message": "plugin store source not found"})
		return pluginstore.Source{}, pluginstore.Plugin{}, pluginstore.Client{}, false
	}

	plugins, sourceErrors := h.fetchSourcedPlugins(ctx, cfg, sources, auth)
	matches := make([]sourcedPlugin, 0)
	for _, item := range plugins {
		if item.plugin.ID == id {
			matches = append(matches, item)
		}
	}
	if len(matches) == 0 {
		if len(plugins) == 0 && len(sourceErrors) > 0 {
			c.JSON(http.StatusBadGateway, gin.H{"error": "plugin_store_registry_failed", "message": sourceErrors[0].Message})
			return pluginstore.Source{}, pluginstore.Plugin{}, pluginstore.Client{}, false
		}
		c.JSON(http.StatusNotFound, gin.H{"error": "plugin_not_found", "message": "plugin not found in registry"})
		return pluginstore.Source{}, pluginstore.Plugin{}, pluginstore.Client{}, false
	}
	if len(matches) > 1 {
		c.JSON(http.StatusConflict, gin.H{
			"error":   "plugin_store_source_required",
			"message": "multiple plugin store sources contain this plugin id; specify source",
			"sources": sanitizePluginStoreSources(sourcedPluginSources(matches)),
		})
		return pluginstore.Source{}, pluginstore.Plugin{}, pluginstore.Client{}, false
	}
	match := matches[0]
	return match.source, match.plugin, h.newPluginStoreClient(cfg, match.source.URL, auth), true
}

func pluginStoreSources(cfg *appconfig.Config) ([]pluginstore.Source, error) {
	if cfg == nil {
		return pluginstore.NormalizeSources(nil)
	}
	return pluginstore.NormalizeSources(cfg.Plugins.StoreSources)
}

func (h *Handler) newPluginStoreClient(cfg *appconfig.Config, registryURL string, auth []pluginstore.ResolvedAuthConfig) pluginstore.Client {
	var httpClient pluginstore.HTTPDoer
	if h != nil {
		httpClient = h.pluginStoreHTTPClient
	}
	if httpClient == nil {
		client := &http.Client{}
		if cfg != nil && strings.TrimSpace(cfg.ProxyURL) != "" {
			util.SetProxy(&appconfig.SDKConfig{ProxyURL: strings.TrimSpace(cfg.ProxyURL)}, client)
		}
		httpClient = client
	}
	return pluginstore.NewClientWithResolvedAuth(httpClient, registryURL, auth)
}

func (h *Handler) fetchSourcedPlugins(ctx context.Context, cfg *appconfig.Config, sources []pluginstore.Source, auth []pluginstore.ResolvedAuthConfig) ([]sourcedPlugin, []pluginStoreSourceErr) {
	plugins := make([]sourcedPlugin, 0)
	sourceErrors := make([]pluginStoreSourceErr, 0)
	for _, source := range sources {
		client := h.newPluginStoreClient(cfg, source.URL, auth)
		registry, errRegistry := client.FetchRegistry(ctx)
		if errRegistry != nil {
			sourceErrors = append(sourceErrors, pluginStoreSourceErr{
				SourceID:   source.ID,
				SourceName: source.Name,
				SourceURL:  source.URL,
				Message:    errRegistry.Error(),
			})
			continue
		}
		for _, plugin := range registry.Plugins {
			plugins = append(plugins, sourcedPlugin{source: source, plugin: plugin})
		}
	}
	return plugins, sourceErrors
}

func upsertPluginStoreManifest(root map[string]any, id string, manifest pluginstore.Manifest) {
	plugins := stringAnyMap(root["plugins"])
	if plugins == nil {
		plugins = map[string]any{}
	}
	configs := stringAnyMap(plugins["configs"])
	if configs == nil {
		configs = map[string]any{}
	}
	entry := stringAnyMap(configs[id])
	if entry == nil {
		entry = map[string]any{}
	}
	entry["enabled"] = true
	entry["store"] = pluginStoreManifestMap(manifest)
	configs[id] = entry
	plugins["configs"] = configs
	root["plugins"] = plugins
}

func removePluginConfig(root map[string]any, id string) bool {
	plugins := stringAnyMap(root["plugins"])
	if plugins == nil {
		return false
	}
	configs := stringAnyMap(plugins["configs"])
	if configs == nil {
		return false
	}
	if _, ok := configs[id]; !ok {
		return false
	}
	delete(configs, id)
	plugins["configs"] = configs
	root["plugins"] = plugins
	return true
}

func pluginStoreManifestMap(manifest pluginstore.Manifest) map[string]any {
	out := map[string]any{
		"id":      strings.TrimSpace(manifest.ID),
		"version": strings.TrimSpace(manifest.Version),
	}
	if manifest.SchemaVersion != 0 {
		out["schema-version"] = manifest.SchemaVersion
	}
	setTrimmedString(out, "name", manifest.Name)
	setTrimmedString(out, "description", manifest.Description)
	setTrimmedString(out, "author", manifest.Author)
	if strings.TrimSpace(manifest.ReleaseTag) != "" {
		out["release-tag"] = strings.TrimSpace(manifest.ReleaseTag)
	}
	if strings.TrimSpace(manifest.Repository) != "" {
		out["repository"] = strings.TrimSpace(manifest.Repository)
	}
	setTrimmedString(out, "source-id", manifest.SourceID)
	setTrimmedString(out, "source-name", manifest.SourceName)
	setTrimmedString(out, "source-url", manifest.SourceURL)
	if strings.TrimSpace(manifest.Install.Type) != "" {
		out["install"] = pluginStoreInstallPlanMap(manifest.Install)
	}
	if strings.TrimSpace(manifest.Logo) != "" {
		out["logo"] = strings.TrimSpace(manifest.Logo)
	}
	if strings.TrimSpace(manifest.Homepage) != "" {
		out["homepage"] = strings.TrimSpace(manifest.Homepage)
	}
	if strings.TrimSpace(manifest.License) != "" {
		out["license"] = strings.TrimSpace(manifest.License)
	}
	if len(manifest.Tags) > 0 {
		out["tags"] = append([]string(nil), manifest.Tags...)
	}
	return out
}

func setTrimmedString(out map[string]any, key string, value string) {
	if strings.TrimSpace(value) != "" {
		out[key] = strings.TrimSpace(value)
	}
}

func pluginStoreInstallPlanMap(plan pluginstore.InstallPlan) map[string]any {
	out := map[string]any{
		"type": strings.TrimSpace(plan.Type),
	}
	if len(plan.Artifacts) == 0 {
		return out
	}
	artifacts := make([]map[string]any, 0, len(plan.Artifacts))
	for _, artifact := range plan.Artifacts {
		item := map[string]any{
			"goos":   strings.TrimSpace(artifact.GOOS),
			"goarch": strings.TrimSpace(artifact.GOARCH),
			"url":    strings.TrimSpace(artifact.URL),
			"sha256": strings.TrimSpace(artifact.SHA256),
		}
		if artifact.Size > 0 {
			item["size"] = artifact.Size
		}
		artifacts = append(artifacts, item)
	}
	out["artifacts"] = artifacts
	return out
}

func configuredPluginStoreManifest(id string, cfg *appconfig.Config) (pluginstore.Manifest, bool) {
	if cfg == nil {
		return pluginstore.Manifest{}, false
	}
	item, okItem := cfg.Plugins.Configs[id]
	if !okItem || item.Raw.Kind == 0 {
		return pluginstore.Manifest{}, false
	}
	storeNode := yamlMappingValue(&item.Raw, "store")
	if storeNode == nil || storeNode.Kind == 0 {
		return pluginstore.Manifest{}, false
	}
	var manifest pluginstore.Manifest
	if errDecode := storeNode.Decode(&manifest); errDecode != nil {
		return pluginstore.Manifest{}, false
	}
	if strings.TrimSpace(manifest.ID) == "" {
		manifest.ID = id
	}
	return manifest, true
}

func yamlMappingValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		keyNode := node.Content[i]
		if keyNode == nil || keyNode.Value != key {
			continue
		}
		return node.Content[i+1]
	}
	return nil
}

func stringAnyMap(value any) map[string]any {
	switch typed := value.(type) {
	case map[string]any:
		return typed
	case map[any]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			keyString, okKey := key.(string)
			if !okKey {
				continue
			}
			out[keyString] = item
		}
		return out
	default:
		return nil
	}
}

func pluginInstanceEnabled(item appconfig.PluginInstanceConfig) bool {
	return item.Enabled != nil && *item.Enabled
}

func pluginStoreDir(cfg *appconfig.Config) string {
	if cfg == nil || strings.TrimSpace(cfg.Plugins.Dir) == "" {
		return "plugins"
	}
	return strings.TrimSpace(cfg.Plugins.Dir)
}

func validManagementPluginID(id string) bool {
	id = strings.TrimSpace(id)
	if id == "" || id == "." || id == ".." || strings.ContainsAny(id, `/\`) {
		return false
	}
	for _, char := range id {
		switch {
		case char >= 'a' && char <= 'z':
		case char >= 'A' && char <= 'Z':
		case char >= '0' && char <= '9':
		case char == '-', char == '_', char == '.':
		default:
			return false
		}
	}
	return true
}

func sourcedPluginSources(plugins []sourcedPlugin) []pluginstore.Source {
	sources := make([]pluginstore.Source, 0, len(plugins))
	for _, item := range plugins {
		sources = append(sources, item.source)
	}
	return sources
}

func sanitizePluginStoreSources(sources []pluginstore.Source) []pluginStoreSource {
	out := make([]pluginStoreSource, 0, len(sources))
	for _, source := range sources {
		out = append(out, pluginStoreSource{
			ID:   trimString(source.ID),
			Name: trimString(source.Name),
			URL:  trimString(source.URL),
		})
	}
	return out
}

func sanitizePluginStoreSourceErrors(sourceErrors []pluginStoreSourceErr) []pluginStoreSourceErr {
	if len(sourceErrors) == 0 {
		return nil
	}
	out := make([]pluginStoreSourceErr, 0, len(sourceErrors))
	for _, sourceError := range sourceErrors {
		out = append(out, pluginStoreSourceErr{
			SourceID:   trimString(sourceError.SourceID),
			SourceName: trimString(sourceError.SourceName),
			SourceURL:  trimString(sourceError.SourceURL),
			Message:    trimString(sourceError.Message),
		})
	}
	return out
}

func sanitizePluginStorePlatforms(platforms []pluginstore.Platform) []pluginStorePlatform {
	if len(platforms) == 0 {
		return nil
	}
	out := make([]pluginStorePlatform, 0, len(platforms))
	for _, platform := range platforms {
		out = append(out, pluginStorePlatform{
			GOOS:   trimString(platform.GOOS),
			GOARCH: trimString(platform.GOARCH),
		})
	}
	return out
}

func trimString(value string) string {
	return strings.TrimSpace(value)
}

func trimStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		out = append(out, trimString(value))
	}
	return out
}
