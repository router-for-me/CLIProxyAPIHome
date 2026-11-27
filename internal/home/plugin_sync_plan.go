package home

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginstore"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/util"
)

const (
	pluginSyncPlanLifetime    = 10 * time.Minute
	pluginSyncMaxRedirects    = 10
	pluginSyncMaxChecksumSize = 4 << 20
)

func (r *Runtime) BuildPluginSyncPlan(ctx context.Context, request pluginstore.PluginSyncRequest) (pluginstore.PluginSyncResponse, error) {
	return r.buildPluginSyncPlan(ctx, request, time.Now)
}

func (r *Runtime) buildPluginSyncPlan(ctx context.Context, request pluginstore.PluginSyncRequest, now func() time.Time) (pluginstore.PluginSyncResponse, error) {
	if request.SchemaVersion != pluginstore.PluginSyncSchemaVersion {
		return pluginstore.PluginSyncResponse{}, fmt.Errorf("unsupported plugin sync schema_version %d", request.SchemaVersion)
	}
	request.GOOS = strings.ToLower(strings.TrimSpace(request.GOOS))
	request.GOARCH = strings.ToLower(strings.TrimSpace(request.GOARCH))
	if request.GOOS == "" || request.GOARCH == "" {
		return pluginstore.PluginSyncResponse{}, fmt.Errorf("goos and goarch are required")
	}
	response := pluginstore.PluginSyncResponse{
		SchemaVersion: pluginstore.PluginSyncSchemaVersion,
		Items:         []pluginstore.PluginSyncItem{},
	}
	cfg, errConfig := r.pluginSyncConfig(ctx)
	if errConfig != nil {
		return pluginstore.PluginSyncResponse{}, fmt.Errorf("load plugin sync config: %w", errConfig)
	}
	if cfg == nil || !cfg.Plugins.Enabled {
		currentTime := now().UTC()
		response.ExpiresAt = currentTime.Add(pluginSyncPlanLifetime)
		if errValidate := response.Validate(currentTime); errValidate != nil {
			return pluginstore.PluginSyncResponse{}, errValidate
		}
		return response, nil
	}
	ids := make([]string, 0, len(cfg.Plugins.Configs))
	for id := range cfg.Plugins.Configs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	manifests := make([]pluginstore.Manifest, 0, len(ids))
	for _, id := range ids {
		item := cfg.Plugins.Configs[id]
		if item.Enabled == nil || !*item.Enabled {
			continue
		}
		manifest, okManifest, errManifest := storeManifestFromPluginConfig(id, item)
		if errManifest != nil {
			response.Clear()
			return pluginstore.PluginSyncResponse{}, errManifest
		}
		if !okManifest {
			continue
		}
		manifests = append(manifests, manifest)
	}
	sort.SliceStable(manifests, func(left, right int) bool {
		return strings.TrimSpace(manifests[left].ID) < strings.TrimSpace(manifests[right].ID)
	})
	for index := 1; index < len(manifests); index++ {
		id := strings.TrimSpace(manifests[index].ID)
		if id == strings.TrimSpace(manifests[index-1].ID) {
			return pluginstore.PluginSyncResponse{}, fmt.Errorf("plugin sync response contains duplicate plugin %q", id)
		}
	}
	pending := manifests[:0]
	for _, manifest := range manifests {
		if pluginSyncVersionsEqual(request.InstalledVersions[manifest.ID], manifest.Version) {
			continue
		}
		pending = append(pending, manifest)
	}
	manifests = pending
	if len(manifests) == 0 {
		currentTime := now().UTC()
		response.ExpiresAt = currentTime.Add(pluginSyncPlanLifetime)
		if errValidate := response.Validate(currentTime); errValidate != nil {
			return pluginstore.PluginSyncResponse{}, errValidate
		}
		return response, nil
	}
	rules, errRules := r.resolvePluginStoreAuth(ctx)
	if errRules != nil {
		return pluginstore.PluginSyncResponse{}, errRules
	}
	defer pluginstore.ClearResolvedAuthConfigs(rules)
	httpClient := r.pluginSyncHTTPDoer(cfg)
	for _, manifest := range manifests {
		planned, auth, errPlan := buildPluginSyncItem(ctx, httpClient, manifest, request.GOOS, request.GOARCH, rules)
		if errPlan != nil {
			response.Clear()
			return pluginstore.PluginSyncResponse{}, fmt.Errorf("plugin %s: %w", manifest.ID, errPlan)
		}
		response.Items = append(response.Items, pluginstore.PluginSyncItem{Manifest: planned, Auth: auth})
	}
	currentTime := now().UTC()
	response.ExpiresAt = currentTime.Add(pluginSyncPlanLifetime)
	if errValidate := response.Validate(currentTime); errValidate != nil {
		response.Clear()
		return pluginstore.PluginSyncResponse{}, fmt.Errorf("validate plugin sync response: %w", errValidate)
	}
	return response, nil
}

func buildPluginSyncItem(ctx context.Context, httpClient pluginstore.HTTPDoer, manifest pluginstore.Manifest, goos, goarch string, rules []pluginstore.ResolvedAuthConfig) (pluginstore.Manifest, []pluginstore.ResolvedAuthConfig, error) {
	switch manifest.InstallType() {
	case pluginstore.InstallTypeDirect:
		if len(manifest.Install.Artifacts) == 0 {
			// Older store installations pinned the registry URL and version only.
			source := pluginstore.Source{URL: manifest.SourceURL}
			client := pluginstore.NewClientWithResolvedAuth(httpClient, source.URL, rules)
			store, errFetch := client.FetchRegistry(ctx)
			if errFetch != nil {
				return pluginstore.Manifest{}, nil, fmt.Errorf("fetch direct install source: %w", errFetch)
			}
			plugin, found := store.PluginByID(manifest.ID)
			if !found || pluginstore.PluginInstallType(plugin) != pluginstore.InstallTypeDirect {
				return pluginstore.Manifest{}, nil, fmt.Errorf("direct plugin %q not found in source", manifest.ID)
			}
			pinned, errPin := DirectPluginStoreManifest(source, plugin, manifest.Version)
			if errPin != nil {
				return pluginstore.Manifest{}, nil, errPin
			}
			manifest.Install = pinned.Install
		}
		artifact, errArtifact := pluginstore.SelectArtifact(manifest.Install, goos, goarch)
		if errArtifact != nil {
			return pluginstore.Manifest{}, nil, errArtifact
		}
		if errURL := validatePluginSyncHTTPSURL(artifact.URL); errURL != nil {
			return pluginstore.Manifest{}, nil, fmt.Errorf("direct artifact: %w", errURL)
		}
		manifest.Install = pluginstore.InstallPlan{Type: pluginstore.InstallTypeDirect, Artifacts: []pluginstore.Artifact{artifact}}
		auth := pluginSyncAuthForTarget(rules, artifact.URL, pluginstore.RequestKindArtifact)
		return manifest, auth, nil
	case pluginstore.InstallTypeGitHubRelease:
		return buildGitHubPluginSyncItem(ctx, httpClient, manifest, goos, goarch, rules)
	default:
		return pluginstore.Manifest{}, nil, fmt.Errorf("unsupported install type %q", manifest.Install.Type)
	}
}

// DirectPluginStoreManifest pins the requested registry version and its platform artifacts.
func DirectPluginStoreManifest(source pluginstore.Source, plugin pluginstore.Plugin, requestedVersion string) (pluginstore.Manifest, error) {
	version := NormalizePluginStoreVersion(requestedVersion)
	if version == "" {
		version = NormalizePluginStoreVersion(plugin.Version)
	}
	if NormalizePluginStoreVersion(plugin.Version) == version {
		plugin.Version = version
		return pluginstore.ManifestFromPlugin(source, plugin)
	}
	for _, candidate := range plugin.Versions {
		if NormalizePluginStoreVersion(candidate.Version) != version {
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

// NormalizePluginStoreVersion accepts either spelling of a version tag.
func NormalizePluginStoreVersion(version string) string {
	version = strings.TrimSpace(version)
	if strings.HasPrefix(strings.ToLower(version), "v") {
		return strings.TrimSpace(version[1:])
	}
	return version
}

func buildGitHubPluginSyncItem(ctx context.Context, httpClient pluginstore.HTTPDoer, manifest pluginstore.Manifest, goos, goarch string, rules []pluginstore.ResolvedAuthConfig) (pluginstore.Manifest, []pluginstore.ResolvedAuthConfig, error) {
	owner, repo, errRepository := pluginstore.GitHubRepositoryParts(manifest.Repository)
	if errRepository != nil {
		return pluginstore.Manifest{}, nil, errRepository
	}
	tag := strings.TrimSpace(manifest.ReleaseTag)
	metadataURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/tags/%s", url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(tag))
	metadataAuth := pluginSyncAuthForTarget(rules, metadataURL, pluginstore.RequestKindMetadata)
	client := pluginstore.NewClientWithResolvedAuth(httpClient, "", metadataAuth)
	release, errRelease := client.FetchReleaseByTag(ctx, manifest.Plugin(), tag)
	client.ClearAuth()
	if errRelease != nil {
		return pluginstore.Manifest{}, nil, errRelease
	}
	releaseVersion, errVersion := pluginstore.ReleaseVersion(release)
	if errVersion != nil {
		return pluginstore.Manifest{}, nil, errVersion
	}
	if !pluginSyncVersionsEqual(releaseVersion, manifest.Version) {
		return pluginstore.Manifest{}, nil, fmt.Errorf("release tag %q resolved version %q, want %q", tag, releaseVersion, strings.TrimSpace(manifest.Version))
	}
	archiveName := fmt.Sprintf("%s_%s_%s_%s.zip", strings.TrimSpace(manifest.ID), releaseVersion, goos, goarch)
	archive, checksums, errAssets := selectPluginSyncReleaseAssets(release, archiveName)
	if errAssets != nil {
		return pluginstore.Manifest{}, nil, errAssets
	}
	checksumsURL, errChecksumsURL := pluginSyncAssetURL(checksums, rules)
	if errChecksumsURL != nil {
		return pluginstore.Manifest{}, nil, fmt.Errorf("checksums asset: %w", errChecksumsURL)
	}
	checksumData, errChecksums := fetchPluginSyncURL(ctx, httpClient, checksumsURL, pluginstore.RequestKindArtifact, rules, pluginSyncMaxChecksumSize)
	if errChecksums != nil {
		return pluginstore.Manifest{}, nil, fmt.Errorf("download checksums: %w", errChecksums)
	}
	defer clearPluginSyncBytes(checksumData)
	checksum, errChecksum := pluginSyncChecksum(checksumData, archiveName)
	if errChecksum != nil {
		return pluginstore.Manifest{}, nil, errChecksum
	}
	archiveURL, errArchiveURL := pluginSyncAssetURL(archive, rules)
	if errArchiveURL != nil {
		return pluginstore.Manifest{}, nil, fmt.Errorf("archive asset: %w", errArchiveURL)
	}
	if errURL := validatePluginSyncHTTPSURL(archiveURL); errURL != nil {
		return pluginstore.Manifest{}, nil, errURL
	}
	manifest.Install = pluginstore.InstallPlan{Type: pluginstore.InstallTypeDirect, Artifacts: []pluginstore.Artifact{{
		GOOS: goos, GOARCH: goarch, URL: archiveURL, SHA256: checksum,
	}}}
	return manifest, pluginSyncAuthForTarget(rules, archiveURL, pluginstore.RequestKindArtifact), nil
}

func selectPluginSyncReleaseAssets(release pluginstore.Release, archiveName string) (pluginstore.ReleaseAsset, pluginstore.ReleaseAsset, error) {
	var archive pluginstore.ReleaseAsset
	var checksums pluginstore.ReleaseAsset
	for _, asset := range release.Assets {
		switch strings.TrimSpace(asset.Name) {
		case archiveName:
			archive = asset
		case "checksums.txt":
			checksums = asset
		}
	}
	if strings.TrimSpace(archive.Name) == "" {
		return pluginstore.ReleaseAsset{}, pluginstore.ReleaseAsset{}, fmt.Errorf("release asset %s not found", archiveName)
	}
	if strings.TrimSpace(checksums.Name) == "" {
		return pluginstore.ReleaseAsset{}, pluginstore.ReleaseAsset{}, fmt.Errorf("release asset checksums.txt not found")
	}
	return archive, checksums, nil
}

func pluginSyncAssetURL(asset pluginstore.ReleaseAsset, rules []pluginstore.ResolvedAuthConfig) (string, error) {
	apiURL := strings.TrimSpace(asset.APIURL)
	downloadURL := strings.TrimSpace(asset.BrowserDownloadURL)
	if apiURL != "" {
		if auth, okAuth := pluginstore.ResolvedAuthForRequest(rules, apiURL, pluginstore.RequestKindArtifact); okAuth {
			configured := strings.TrimSpace(auth.Type) != "" && !strings.EqualFold(auth.Type, pluginstore.AuthTypeNone)
			auth.Clear()
			if configured {
				downloadURL = apiURL
			}
		}
	}
	if downloadURL == "" {
		downloadURL = apiURL
	}
	if downloadURL == "" {
		return "", fmt.Errorf("asset %q missing download url", asset.Name)
	}
	if errURL := validatePluginSyncHTTPSURL(downloadURL); errURL != nil {
		return "", errURL
	}
	return downloadURL, nil
}

func fetchPluginSyncURL(ctx context.Context, httpClient pluginstore.HTTPDoer, requestURL string, kind string, rules []pluginstore.ResolvedAuthConfig, maxSize int64) ([]byte, error) {
	currentURL := strings.TrimSpace(requestURL)
	for redirects := 0; ; redirects++ {
		if redirects > pluginSyncMaxRedirects {
			return nil, fmt.Errorf("too many redirects")
		}
		if errURL := validatePluginSyncNetworkURL(currentURL); errURL != nil {
			return nil, errURL
		}
		req, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, currentURL, nil)
		if errRequest != nil {
			return nil, fmt.Errorf("create plugin store request: %w", errRequest)
		}
		req.Header.Set("Accept", "application/octet-stream")
		auth, okAuth := pluginstore.ResolvedAuthForRequest(rules, currentURL, kind)
		if okAuth {
			if errAuth := applyPluginSyncAuth(req.Header, auth); errAuth != nil {
				auth.Clear()
				return nil, errAuth
			}
		}
		resp, errDo := httpClient.Do(req)
		for name := range req.Header {
			req.Header.Del(name)
		}
		auth.Clear()
		if errDo != nil {
			return nil, pluginSyncRequestError(currentURL, errDo)
		}
		if resp == nil || resp.Body == nil {
			return nil, fmt.Errorf("plugin store request returned empty response")
		}
		if resp.StatusCode >= 300 && resp.StatusCode <= 399 {
			location := strings.TrimSpace(resp.Header.Get("Location"))
			if errClose := resp.Body.Close(); errClose != nil {
				return nil, fmt.Errorf("close plugin store redirect response: %w", errClose)
			}
			base, errBase := url.Parse(currentURL)
			next, errNext := url.Parse(location)
			if errBase != nil || errNext != nil || location == "" {
				return nil, fmt.Errorf("plugin store redirect location is invalid")
			}
			currentURL = base.ResolveReference(next).String()
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			if errClose := resp.Body.Close(); errClose != nil {
				return nil, fmt.Errorf("plugin store request returned status %d; close response: %w", resp.StatusCode, errClose)
			}
			return nil, fmt.Errorf("plugin store request returned status %d", resp.StatusCode)
		}
		limited := io.LimitReader(resp.Body, maxSize+1)
		data, errRead := io.ReadAll(limited)
		errClose := resp.Body.Close()
		if errRead != nil {
			clearPluginSyncBytes(data)
			return nil, fmt.Errorf("read plugin store response: %w", errRead)
		}
		if errClose != nil {
			clearPluginSyncBytes(data)
			return nil, fmt.Errorf("close plugin store response: %w", errClose)
		}
		if int64(len(data)) > maxSize {
			clearPluginSyncBytes(data)
			return nil, fmt.Errorf("plugin store response exceeds size limit")
		}
		return data, nil
	}
}

func applyPluginSyncAuth(headers http.Header, auth pluginstore.ResolvedAuthConfig) error {
	switch strings.ToLower(strings.TrimSpace(auth.Type)) {
	case "", pluginstore.AuthTypeNone:
		return nil
	case pluginstore.AuthTypeBearer, pluginstore.AuthTypeGitHubToken:
		headers.Set("Authorization", "Bearer "+string(auth.Token))
	case pluginstore.AuthTypeBasic:
		credential := make([]byte, 0, len(auth.Username)+1+len(auth.Password))
		credential = append(credential, auth.Username...)
		credential = append(credential, ':')
		credential = append(credential, auth.Password...)
		headers.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString(credential))
		clearPluginSyncBytes(credential)
	case pluginstore.AuthTypeHeader:
		headers.Set(auth.HeaderName, string(auth.HeaderValue))
	default:
		return fmt.Errorf("unsupported plugin store auth type %q", auth.Type)
	}
	return nil
}

func pluginSyncAuthForTarget(rules []pluginstore.ResolvedAuthConfig, targetURL string, kind string) []pluginstore.ResolvedAuthConfig {
	auth, okAuth := pluginstore.ResolvedAuthForRequest(rules, targetURL, kind)
	if !okAuth {
		return nil
	}
	return []pluginstore.ResolvedAuthConfig{auth}
}

func pluginSyncVersionsEqual(installed string, target string) bool {
	installed = strings.TrimSpace(installed)
	target = strings.TrimSpace(target)
	if installed == "" || target == "" {
		return false
	}
	return !pluginstore.UpdateAvailable(installed, target) && !pluginstore.UpdateAvailable(target, installed)
}

func pluginSyncChecksum(data []byte, archiveName string) (string, error) {
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 2 || strings.TrimPrefix(fields[len(fields)-1], "*") != archiveName {
			continue
		}
		checksum := strings.ToLower(strings.TrimSpace(fields[0]))
		decoded, errDecode := hex.DecodeString(checksum)
		if errDecode != nil || len(decoded) != 32 {
			clearPluginSyncBytes(decoded)
			return "", fmt.Errorf("checksum for %s is invalid", archiveName)
		}
		clearPluginSyncBytes(decoded)
		return checksum, nil
	}
	return "", fmt.Errorf("checksum for %s not found", archiveName)
}

func validatePluginSyncHTTPSURL(rawURL string) error {
	parsed, errParse := url.Parse(strings.TrimSpace(rawURL))
	if errParse != nil || !strings.EqualFold(parsed.Scheme, "https") || strings.TrimSpace(parsed.Host) == "" {
		return fmt.Errorf("plugin sync url must use https")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("plugin sync url must not contain credentials, query, or fragment")
	}
	return nil
}

func validatePluginSyncNetworkURL(rawURL string) error {
	parsed, errParse := url.Parse(strings.TrimSpace(rawURL))
	if errParse != nil || !strings.EqualFold(parsed.Scheme, "https") || strings.TrimSpace(parsed.Host) == "" {
		return fmt.Errorf("plugin sync url must use https")
	}
	if parsed.User != nil || parsed.Fragment != "" {
		return fmt.Errorf("plugin sync url must not contain credentials or fragment")
	}
	return nil
}

func pluginSyncRequestError(rawURL string, err error) error {
	var urlError *url.Error
	if errors.As(err, &urlError) && urlError.Err != nil {
		err = urlError.Err
	}
	parsed, errParse := url.Parse(strings.TrimSpace(rawURL))
	safeURL := "plugin store url"
	if errParse == nil && parsed.Scheme != "" && parsed.Host != "" {
		parsed.User = nil
		parsed.RawQuery = ""
		parsed.ForceQuery = false
		parsed.Fragment = ""
		safeURL = parsed.String()
	}
	return fmt.Errorf("request %s failed: %w", safeURL, err)
}

func (r *Runtime) pluginSyncHTTPDoer(cfg *config.Config) pluginstore.HTTPDoer {
	if r != nil && r.pluginSyncHTTPClient != nil {
		return r.pluginSyncHTTPClient
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if cfg != nil && strings.TrimSpace(cfg.ProxyURL) != "" {
		util.SetProxy(&config.SDKConfig{ProxyURL: strings.TrimSpace(cfg.ProxyURL)}, client)
	}
	return client
}

func clearPluginSyncBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}
