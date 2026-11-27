package home

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	goruntime "runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginstore"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/util"
	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
)

type PluginPlatform struct {
	GOOS   string `json:"goos"`
	GOARCH string `json:"goarch"`
}

type pluginStoreSyncState struct {
	Key  string
	Path string
	Hash string
	Size int64
}

type pluginStoreManifestSyncKey struct {
	ID         string                  `json:"id"`
	Version    string                  `json:"version"`
	ReleaseTag string                  `json:"release_tag"`
	Repository string                  `json:"repository"`
	Install    pluginstore.InstallPlan `json:"install"`
	SourceID   string                  `json:"source_id,omitempty"`
	SourceURL  string                  `json:"source_url,omitempty"`
	PluginsDir string                  `json:"plugins_dir"`
	Platform   PluginPlatform          `json:"platform"`
}

type pluginFileDigest struct {
	Hash string
	Size int64
}

type pluginFileInfo struct {
	ID      string
	Path    string
	Version string
}

var pluginFileVersionPattern = regexp.MustCompile(`^[0-9][0-9A-Za-z.+-]*$`)

type pluginArtifactRuntime interface {
	PluginBusy(id string) bool
	UnloadPlugin(id string) bool
}

// CurrentPluginPlatform reports the platform used by Home plugin discovery.
func CurrentPluginPlatform() PluginPlatform {
	return PluginPlatform{
		GOOS:   goruntime.GOOS,
		GOARCH: goruntime.GOARCH,
	}
}

func (r *Runtime) syncPluginStoreManifests(ctx context.Context, cfg *config.Config) error {
	if r == nil || cfg == nil || !cfg.Plugins.Enabled {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	auth, errAuth := r.resolvePluginStoreAuth(ctx)
	defer pluginstore.ClearResolvedAuthConfigs(auth)
	platform := CurrentPluginPlatform()
	client := newPluginStoreClient(cfg, auth)
	root := strings.TrimSpace(cfg.Plugins.Dir)
	if root == "" {
		root = "plugins"
	}

	r.pluginSyncMu.Lock()
	defer r.pluginSyncMu.Unlock()

	ids := make([]string, 0, len(cfg.Plugins.Configs))
	for id := range cfg.Plugins.Configs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		item := cfg.Plugins.Configs[id]
		catalog := isLocalExecutorStoreConfig(item)
		if !pluginConfigEnabled(item) || !pluginInstanceLoadsInHome(id, item) {
			continue
		}
		manifest, okManifest, errManifest := storeManifestFromPluginConfig(id, item)
		if errManifest != nil {
			if catalog {
				continue
			}
			return errManifest
		}
		if !okManifest {
			continue
		}
		if catalog && manifest.ID != id {
			continue
		}
		var errSync error
		if errAuth != nil {
			errSync = fmt.Errorf("home plugins: resolve store auth: %w", errAuth)
		} else {
			errSync = r.syncPluginStoreManifest(ctx, client, manifest, root, platform)
		}
		if errSync == nil {
			continue
		}
		if !catalog {
			return errSync
		}
		log.WithError(errSync).WithField("plugin_id", id).Warn("ACP plugin unavailable in Home runtime")
	}
	return nil
}

func (r *Runtime) syncPluginStoreManifest(ctx context.Context, client pluginstore.Client, manifest pluginstore.Manifest, root string, platform PluginPlatform) error {
	syncKey, errKey := pluginStoreManifestKey(root, platform, manifest)
	if errKey != nil {
		return errKey
	}
	if r.pluginStoreArtifactCurrent(root, manifest, syncKey) {
		return nil
	}
	result, errInstall := r.installPluginStoreManifest(ctx, client, manifest, root, platform)
	if errInstall != nil {
		return errInstall
	}
	return r.markPluginStoreSynced(root, manifest, syncKey, result)
}

func (r *Runtime) installPluginStoreManifest(ctx context.Context, client pluginstore.Client, manifest pluginstore.Manifest, root string, platform PluginPlatform) (pluginstore.InstallResult, error) {
	id := strings.TrimSpace(manifest.ID)
	if id == "" {
		return pluginstore.InstallResult{}, fmt.Errorf("home plugins: manifest plugin id is empty")
	}
	result, errInstall := client.InstallManifest(ctx, manifest, pluginStoreInstallOptions(root, platform, r.pluginRuntimeForArtifacts(), id))
	if errInstall != nil {
		return pluginstore.InstallResult{}, fmt.Errorf("home plugins: install %s: %w", id, errInstall)
	}
	return result, nil
}

func pluginRootFromConfigDir(root string) string {
	root = strings.TrimSpace(root)
	if root == "" {
		return "plugins"
	}
	root = filepath.Clean(root)
	return root
}

// Reconcile catalogs after publication so a failed config reload cannot prewarm
// or prune files for a configuration that never took effect.
func (r *Runtime) reconcilePluginArtifacts(ctx context.Context, oldCfg *config.Config, newCfg *config.Config) {
	if r == nil {
		return
	}
	if _, _, errCatalog := r.observeLocalExecutorCatalogs(ctx); errCatalog != nil {
		log.WithError(errCatalog).Warn("refresh ACP plugin catalogs")
	} else {
		if newCfg != nil {
			r.pruneLocalExecutorArtifacts(newCfg.Plugins.Dir)
		}
		if oldCfg != nil && (newCfg == nil || pluginRootFromConfigDir(oldCfg.Plugins.Dir) != pluginRootFromConfigDir(newCfg.Plugins.Dir)) {
			r.pruneLocalExecutorArtifacts(oldCfg.Plugins.Dir)
		}
	}
	removed := removedHomeLoadedStorePluginIDs(oldCfg, newCfg)
	if len(removed) == 0 {
		return
	}
	root := pluginRootFromConfigDir(oldCfg.Plugins.Dir)
	for _, id := range removed {
		path, deleted, errDelete := r.deletePluginArtifact(root, id)
		if errDelete != nil {
			log.Warnf("failed to delete removed home plugin %s: %v", id, errDelete)
			continue
		}
		r.clearPluginStoreSyncState(id)
		if deleted {
			log.Infof("deleted removed home plugin %s (%s)", id, path)
		}
	}
}

func (r *Runtime) deletePluginArtifact(root string, id string) (string, bool, error) {
	id = strings.TrimSpace(id)
	if !validPluginFileID(id) {
		return "", false, fmt.Errorf("invalid plugin id %q", id)
	}
	paths, errPath := pluginFilePaths(root, id)
	if errPath != nil {
		return "", false, errPath
	}
	if len(paths) == 0 {
		return "", false, nil
	}
	if errPrepare := preparePluginArtifactMutation(r.pluginRuntimeForArtifacts(), id); errPrepare != nil {
		return paths[0], false, errPrepare
	}
	deleted := false
	for _, path := range paths {
		if errRemove := os.Remove(path); errRemove != nil {
			if errors.Is(errRemove, os.ErrNotExist) {
				continue
			}
			return paths[0], deleted, errRemove
		}
		deleted = true
	}
	return paths[0], deleted, nil
}

func (r *Runtime) pluginRuntimeForArtifacts() pluginArtifactRuntime {
	if r == nil || r.pluginHost == nil {
		return nil
	}
	return r.pluginHost
}

func pluginStoreInstallOptions(root string, platform PluginPlatform, runtime pluginArtifactRuntime, id string) pluginstore.InstallOptions {
	return pluginstore.InstallOptions{
		PluginsDir: root,
		GOOS:       platform.GOOS,
		GOARCH:     platform.GOARCH,
		PluginLoaded: func() bool {
			return pluginRuntimeBusy(runtime, id)
		},
		BeforeWrite: func() error {
			return preparePluginArtifactMutation(runtime, id)
		},
	}
}

func preparePluginArtifactMutation(runtime pluginArtifactRuntime, id string) error {
	if !pluginRuntimeBusy(runtime, id) {
		return nil
	}
	if runtime == nil || (!runtime.UnloadPlugin(id) && pluginRuntimeBusy(runtime, id)) {
		return pluginstore.ErrLoadedPluginLocked
	}
	return nil
}

func pluginRuntimeBusy(runtime pluginArtifactRuntime, id string) bool {
	return runtime != nil && runtime.PluginBusy(id)
}

func removedHomeLoadedStorePluginIDs(oldCfg *config.Config, newCfg *config.Config) []string {
	oldIDs := homeLoadedStorePluginIDs(oldCfg)
	if len(oldIDs) == 0 {
		return nil
	}
	newIDs := homeLoadedStorePluginIDs(newCfg)
	removed := make([]string, 0)
	for id := range oldIDs {
		if _, ok := newIDs[id]; ok {
			continue
		}
		removed = append(removed, id)
	}
	sort.Strings(removed)
	return removed
}

func homeLoadedStorePluginIDs(cfg *config.Config) map[string]struct{} {
	if cfg == nil || !cfg.Plugins.Enabled {
		return nil
	}
	out := make(map[string]struct{})
	for id, item := range cfg.Plugins.Configs {
		id = strings.TrimSpace(id)
		if id == "" || !pluginConfigEnabled(item) || !pluginInstanceLoadsInHome(id, item) {
			continue
		}
		if _, okManifest, _ := storeManifestFromPluginConfig(id, item); okManifest {
			out[id] = struct{}{}
		}
	}
	return out
}

func pluginStoreManifestKey(root string, platform PluginPlatform, manifest pluginstore.Manifest) (string, error) {
	key := pluginStoreManifestSyncKey{
		ID:         strings.TrimSpace(manifest.ID),
		Version:    strings.TrimSpace(manifest.Version),
		ReleaseTag: strings.TrimSpace(manifest.ReleaseTag),
		Repository: strings.TrimSpace(manifest.Repository),
		Install:    manifest.Install,
		SourceID:   strings.TrimSpace(manifest.SourceID),
		SourceURL:  strings.TrimSpace(manifest.SourceURL),
		PluginsDir: filepath.Clean(strings.TrimSpace(root)),
		Platform: PluginPlatform{
			GOOS:   strings.TrimSpace(platform.GOOS),
			GOARCH: strings.TrimSpace(platform.GOARCH),
		},
	}
	if key.PluginsDir == "." {
		key.PluginsDir = ""
	}
	data, errMarshal := json.Marshal(key)
	if errMarshal != nil {
		return "", fmt.Errorf("home plugins: marshal sync key for %s: %w", key.ID, errMarshal)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func (r *Runtime) pluginStoreArtifactCurrent(root string, manifest pluginstore.Manifest, key string) bool {
	if r == nil || r.pluginStoreSync == nil {
		return false
	}
	id := strings.TrimSpace(manifest.ID)
	state, ok := r.pluginStoreSync[id]
	if !ok || state.Key != key {
		return false
	}
	path, errPath := currentPluginFilePath(root, id, manifest.Version)
	if errPath != nil || strings.TrimSpace(path) == "" {
		return false
	}
	if filepath.Clean(path) != filepath.Clean(state.Path) {
		return false
	}
	digest, errDigest := pluginFileDigestForPath(path)
	if errDigest != nil {
		return false
	}
	return digest.Hash == state.Hash && digest.Size == state.Size
}

func (r *Runtime) markPluginStoreSynced(root string, manifest pluginstore.Manifest, key string, result pluginstore.InstallResult) error {
	if r == nil {
		return nil
	}
	id := strings.TrimSpace(manifest.ID)
	if id == "" {
		return fmt.Errorf("home plugins: sync state plugin id is empty")
	}
	path := strings.TrimSpace(result.Path)
	if path == "" {
		var errPath error
		path, errPath = currentPluginFilePath(root, id, manifest.Version)
		if errPath != nil {
			return fmt.Errorf("home plugins: locate synced artifact %s: %w", id, errPath)
		}
	}
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("home plugins: synced artifact %s path is empty", id)
	}
	digest, errDigest := pluginFileDigestForPath(path)
	if errDigest != nil {
		return fmt.Errorf("home plugins: hash synced artifact %s: %w", id, errDigest)
	}
	if r.pluginStoreSync == nil {
		r.pluginStoreSync = make(map[string]pluginStoreSyncState)
	}
	r.pluginStoreSync[id] = pluginStoreSyncState{
		Key:  key,
		Path: filepath.Clean(path),
		Hash: digest.Hash,
		Size: digest.Size,
	}
	return nil
}

func (r *Runtime) clearPluginStoreSyncState(id string) {
	if r == nil {
		return
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return
	}
	r.pluginSyncMu.Lock()
	defer r.pluginSyncMu.Unlock()
	if r.pluginStoreSync != nil {
		delete(r.pluginStoreSync, id)
	}
}

func pluginFileDigestForPath(path string) (pluginFileDigest, error) {
	file, errOpen := os.Open(path)
	if errOpen != nil {
		return pluginFileDigest{}, errOpen
	}
	defer func() {
		if errClose := file.Close(); errClose != nil {
			log.WithError(errClose).Debugf("home plugins: close digest file %s", path)
		}
	}()
	hasher := sha256.New()
	size, errCopy := io.Copy(hasher, file)
	if errCopy != nil {
		return pluginFileDigest{}, errCopy
	}
	return pluginFileDigest{
		Hash: hex.EncodeToString(hasher.Sum(nil)),
		Size: size,
	}, nil
}

func currentPluginFilePath(root string, id string, desiredVersions ...string) (string, error) {
	files, errFiles := pluginFileInfos(root, id)
	if errFiles != nil {
		return "", errFiles
	}
	if len(files) == 0 {
		return "", nil
	}
	desiredVersion := normalizeDesiredPluginFileVersion(desiredVersions...)
	if desiredVersion != "" {
		for _, file := range files {
			if file.Version == desiredVersion {
				return file.Path, nil
			}
		}
		return "", nil
	}
	return files[0].Path, nil
}

func pluginFilePaths(root string, id string) ([]string, error) {
	files, errFiles := pluginFileInfos(root, id)
	if errFiles != nil {
		return nil, errFiles
	}
	out := make([]string, 0, len(files))
	for _, file := range files {
		out = append(out, file.Path)
	}
	return out, nil
}

func pluginFileInfos(root string, id string) ([]pluginFileInfo, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		root = "plugins"
	}
	id = strings.TrimSpace(id)
	extension := pluginExtension(goruntime.GOOS)
	candidates := make([]pluginFileInfo, 0)
	for _, dir := range pluginCandidateDirs(root, goruntime.GOOS, goruntime.GOARCH) {
		entries, errReadDir := os.ReadDir(dir)
		if errReadDir != nil {
			if errors.Is(errReadDir, os.ErrNotExist) {
				continue
			}
			return nil, errReadDir
		}
		files := make([]string, 0, len(entries))
		for _, entry := range entries {
			if entry == nil || !entry.Type().IsRegular() {
				continue
			}
			if strings.HasSuffix(strings.ToLower(entry.Name()), extension) {
				files = append(files, filepath.Join(dir, entry.Name()))
			}
		}
		sort.Strings(files)
		for _, filePath := range files {
			file, okFile := pluginFileInfoFromPath(filePath, extension)
			if !okFile || file.ID != id {
				continue
			}
			candidates = append(candidates, file)
		}
	}
	if len(candidates) <= 1 {
		return candidates, nil
	}
	bestIndex := 0
	for index := 1; index < len(candidates); index++ {
		if pluginFilePreferred(candidates[index], candidates[bestIndex]) {
			bestIndex = index
		}
	}
	if bestIndex == 0 {
		return candidates, nil
	}
	out := make([]pluginFileInfo, 0, len(candidates))
	out = append(out, candidates[bestIndex])
	for index, candidate := range candidates {
		if index == bestIndex {
			continue
		}
		out = append(out, candidate)
	}
	return out, nil
}

func normalizeDesiredPluginFileVersion(versions ...string) string {
	for _, version := range versions {
		version = strings.TrimSpace(version)
		if len(version) > 1 && (version[0] == 'v' || version[0] == 'V') {
			version = version[1:]
		}
		if validPluginFileVersion(version) {
			return version
		}
	}
	return ""
}

func pluginCandidateDirs(root string, goos string, goarch string) []string {
	dirs := make([]string, 0, 2)
	dirs = append(dirs, filepath.Join(root, goos, goarch))
	dirs = append(dirs, root)
	return dirs
}

func pluginIDFromPath(path string) string {
	file, okFile := pluginFileInfoFromPath(path, "")
	if okFile {
		return file.ID
	}
	base := filepath.Base(path)
	lowerBase := strings.ToLower(base)
	for _, extension := range []string{".so", ".dylib", ".dll"} {
		if strings.HasSuffix(lowerBase, extension) {
			return base[:len(base)-len(extension)]
		}
	}
	return base
}

func pluginFileInfoFromPath(filePath string, requiredExtension string) (pluginFileInfo, bool) {
	base := filepath.Base(filePath)
	lowerBase := strings.ToLower(base)
	extension := strings.TrimSpace(requiredExtension)
	if extension != "" {
		if !strings.HasSuffix(lowerBase, strings.ToLower(extension)) {
			return pluginFileInfo{}, false
		}
	} else {
		for _, candidateExtension := range []string{".so", ".dylib", ".dll"} {
			if strings.HasSuffix(lowerBase, candidateExtension) {
				extension = candidateExtension
				break
			}
		}
		if extension == "" {
			return pluginFileInfo{}, false
		}
	}
	name := base[:len(base)-len(extension)]
	id := name
	version := ""
	if versionIndex := strings.LastIndex(name, "-v"); versionIndex > 0 {
		candidateID := name[:versionIndex]
		candidateVersion := name[versionIndex+2:]
		if validPluginFileID(candidateID) && validPluginFileVersion(candidateVersion) {
			id = candidateID
			version = candidateVersion
		}
	}
	if !validPluginFileID(id) {
		return pluginFileInfo{}, false
	}
	return pluginFileInfo{ID: id, Path: filePath, Version: version}, true
}

func pluginFilePreferred(candidate pluginFileInfo, current pluginFileInfo) bool {
	if candidate.Version == "" {
		return false
	}
	if current.Version == "" {
		return true
	}
	comparison, comparable := comparePluginFileVersions(candidate.Version, current.Version)
	if !comparable {
		return candidate.Version > current.Version
	}
	return comparison > 0
}

func comparePluginFileVersions(a string, b string) (int, bool) {
	segmentsA := strings.Split(a, ".")
	segmentsB := strings.Split(b, ".")
	length := len(segmentsA)
	if len(segmentsB) > length {
		length = len(segmentsB)
	}
	for index := 0; index < length; index++ {
		numberA, okA := pluginFileVersionSegment(segmentsA, index)
		numberB, okB := pluginFileVersionSegment(segmentsB, index)
		if !okA || !okB {
			return 0, false
		}
		if numberA != numberB {
			if numberA < numberB {
				return -1, true
			}
			return 1, true
		}
	}
	return 0, true
}

func pluginFileVersionSegment(segments []string, index int) (int64, bool) {
	if index >= len(segments) {
		return 0, true
	}
	number, errParse := strconv.ParseInt(segments[index], 10, 64)
	if errParse != nil || number < 0 {
		return 0, false
	}
	return number, true
}

func validPluginFileID(id string) bool {
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

func validPluginFileVersion(version string) bool {
	version = strings.TrimSpace(version)
	return version != "" && !strings.HasPrefix(version, "v") && pluginFileVersionPattern.MatchString(version)
}

func storeManifestFromPluginConfig(id string, item config.PluginInstanceConfig) (pluginstore.Manifest, bool, error) {
	if item.Raw.Kind == 0 {
		return pluginstore.Manifest{}, false, nil
	}
	storeNode := yamlMappingValue(&item.Raw, "store")
	if storeNode == nil || storeNode.Kind == 0 {
		return pluginstore.Manifest{}, false, nil
	}
	var manifest pluginstore.Manifest
	if errDecode := storeNode.Decode(&manifest); errDecode != nil {
		return pluginstore.Manifest{}, false, fmt.Errorf("home plugins: decode store manifest for %s: %w", id, errDecode)
	}
	if strings.TrimSpace(manifest.ID) == "" {
		manifest.ID = strings.TrimSpace(id)
	}
	if errValidate := manifest.Validate(); errValidate != nil {
		return pluginstore.Manifest{}, false, fmt.Errorf("home plugins: invalid store manifest for %s: %w", id, errValidate)
	}
	return manifest, true, nil
}

func pluginConfigEnabled(item config.PluginInstanceConfig) bool {
	return item.Enabled != nil && *item.Enabled
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

var newPluginStoreClient = func(cfg *config.Config, auth []pluginstore.ResolvedAuthConfig) pluginstore.Client {
	client := &http.Client{}
	if cfg != nil && strings.TrimSpace(cfg.ProxyURL) != "" {
		util.SetProxy(&config.SDKConfig{ProxyURL: strings.TrimSpace(cfg.ProxyURL)}, client)
	}
	return pluginstore.NewClientWithResolvedAuth(client, "", auth)
}

func pluginExtension(goos string) string {
	switch strings.ToLower(strings.TrimSpace(goos)) {
	case "darwin", "mac", "macos", "osx":
		return ".dylib"
	case "windows":
		return ".dll"
	default:
		return ".so"
	}
}
