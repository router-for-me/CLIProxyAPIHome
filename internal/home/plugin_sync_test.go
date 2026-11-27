package home

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginstore"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"gopkg.in/yaml.v3"
)

func TestSyncPluginStoreManifestsInstallsCurrentPlatformArtifact(t *testing.T) {
	root := t.TempDir()
	platform := CurrentPluginPlatform()
	archiveData := makePluginStoreZip(t, map[string]string{"sample" + pluginExtension(platform.GOOS): "library-data"})
	archiveName := "sample_0.2.0_" + platform.GOOS + "_" + platform.GOARCH + ".zip"
	checksum := sha256.Sum256(archiveData)
	httpClient := pluginStoreHTTPDoer{
		"https://api.github.com/repos/owner/sample-plugin/releases/tags/v0.2.0": []byte(`{
			"tag_name": "v0.2.0",
			"assets": [
				{"name": "` + archiveName + `", "browser_download_url": "https://downloads.example/` + archiveName + `"},
				{"name": "checksums.txt", "browser_download_url": "https://downloads.example/checksums.txt"}
			]
		}`),
		"https://downloads.example/" + archiveName: archiveData,
		"https://downloads.example/checksums.txt":  []byte(hex.EncodeToString(checksum[:]) + "  " + archiveName + "\n"),
	}
	restore := replaceHomePluginStoreClientForTest(httpClient)
	defer restore()

	rt := &Runtime{}
	rt.SetPluginSyncConfigLoader(func(context.Context) (*config.Config, error) {
		return nil, errors.New("catalog configuration unavailable")
	})
	cfg := &config.Config{
		Plugins: config.PluginsConfig{
			Enabled: true,
			Dir:     root,
			Configs: map[string]config.PluginInstanceConfig{
				"sample": homePluginConfigFromYAML(t, `
enabled: true
load-in-home: true
store:
  id: sample
  name: Sample
  description: Adds sample support.
  author: owner
  version: 0.2.0
  release-tag: v0.2.0
  repository: https://github.com/owner/sample-plugin
`),
			},
		},
	}

	if errSync := rt.syncPluginStoreManifests(context.Background(), cfg); errSync != nil {
		t.Fatalf("syncPluginStoreManifests() error = %v", errSync)
	}
	target := filepath.Join(root, platform.GOOS, platform.GOARCH, "sample-v0.2.0"+pluginExtension(platform.GOOS))
	got, errRead := os.ReadFile(target)
	if errRead != nil {
		t.Fatalf("read target: %v", errRead)
	}
	if string(got) != "library-data" {
		t.Fatalf("target data = %q, want library-data", string(got))
	}
}

func TestSyncPluginStoreManifestsInstallsDirectArtifact(t *testing.T) {
	root := t.TempDir()
	platform := CurrentPluginPlatform()
	archiveData := makePluginStoreZip(t, map[string]string{"sample" + pluginExtension(platform.GOOS): "direct-library-data"})
	checksum := sha256.Sum256(archiveData)
	artifactURL := "https://downloads.example/sample_0.4.0.zip"
	sourceURL := "https://plugins.example/registry.json"
	restore := replaceHomePluginStoreClientForTest(pluginStoreHTTPDoer{
		sourceURL: []byte(`{
			"schema_version": 2,
			"plugins": [{
				"id": "sample",
				"name": "Sample",
				"description": "Adds sample support.",
				"author": "owner",
				"version": "0.4.0",
				"install": {
					"type": "direct",
					"artifacts": [{
						"goos": "` + platform.GOOS + `",
						"goarch": "` + platform.GOARCH + `",
						"url": "` + artifactURL + `",
						"sha256": "` + hex.EncodeToString(checksum[:]) + `"
					}]
				}
			}]
		}`),
		artifactURL: archiveData,
	})
	defer restore()

	rt := &Runtime{}
	cfg := &config.Config{
		Plugins: config.PluginsConfig{
			Enabled: true,
			Dir:     root,
			Configs: map[string]config.PluginInstanceConfig{
				"sample": homePluginConfigFromYAML(t, `
enabled: true
load-in-home: true
store:
  schema-version: 2
  id: sample
  name: Sample
  description: Adds sample support.
  author: owner
  version: 0.4.0
  source-url: `+sourceURL+`
  install:
    type: direct
`),
			},
		},
	}

	if errSync := rt.syncPluginStoreManifests(context.Background(), cfg); errSync != nil {
		t.Fatalf("syncPluginStoreManifests() error = %v", errSync)
	}
	target := filepath.Join(root, platform.GOOS, platform.GOARCH, "sample-v0.4.0"+pluginExtension(platform.GOOS))
	got, errRead := os.ReadFile(target)
	if errRead != nil {
		t.Fatalf("read target: %v", errRead)
	}
	if string(got) != "direct-library-data" {
		t.Fatalf("target data = %q, want direct-library-data", string(got))
	}
}

func TestSyncPluginStoreManifestsUsesResolvedArtifactAuth(t *testing.T) {
	root := t.TempDir()
	platform := CurrentPluginPlatform()
	archiveData := makePluginStoreZip(t, map[string]string{"sample" + pluginExtension(platform.GOOS): "private-library-data"})
	checksum := sha256.Sum256(archiveData)
	artifactURL := "https://downloads.example/private/sample_0.4.0.zip"
	sourceURL := "https://plugins.example/registry.json"
	restore := replaceHomePluginStoreClientForTest(authenticatedPluginStoreHTTPDoer{
		responses: pluginStoreHTTPDoer{
			sourceURL: []byte(`{
				"schema_version": 2,
				"plugins": [{
					"id": "sample",
					"name": "Sample",
					"description": "Adds sample support.",
					"author": "owner",
					"version": "0.4.0",
					"install": {
						"type": "direct",
						"artifacts": [{
							"goos": "` + platform.GOOS + `",
							"goarch": "` + platform.GOARCH + `",
							"url": "` + artifactURL + `",
							"sha256": "` + hex.EncodeToString(checksum[:]) + `"
						}]
					}
				}]
			}`),
			artifactURL: archiveData,
		},
		authorization: map[string]string{artifactURL: "Bearer artifact-token"},
	})
	defer restore()

	rt := &Runtime{}
	rt.SetPluginStoreAuthResolver(func(context.Context) ([]pluginstore.ResolvedAuthConfig, error) {
		return []pluginstore.ResolvedAuthConfig{{
			Match:   "https://downloads.example/private/",
			ApplyTo: []string{pluginstore.RequestKindArtifact},
			Type:    pluginstore.AuthTypeBearer,
			Token:   pluginstore.Secret("artifact-token"),
		}}, nil
	})
	cfg := &config.Config{
		Plugins: config.PluginsConfig{
			Enabled: true,
			Dir:     root,
			Configs: map[string]config.PluginInstanceConfig{
				"sample": homePluginConfigFromYAML(t, `
enabled: true
load-in-home: true
store:
  schema-version: 2
  id: sample
  name: Sample
  description: Adds sample support.
  author: owner
  version: 0.4.0
  source-url: `+sourceURL+`
  install:
    type: direct
`),
			},
		},
	}

	if errSync := rt.syncPluginStoreManifests(context.Background(), cfg); errSync != nil {
		t.Fatalf("syncPluginStoreManifests() error = %v", errSync)
	}
	target := filepath.Join(root, platform.GOOS, platform.GOARCH, "sample-v0.4.0"+pluginExtension(platform.GOOS))
	got, errRead := os.ReadFile(target)
	if errRead != nil {
		t.Fatalf("read target: %v", errRead)
	}
	if string(got) != "private-library-data" {
		t.Fatalf("target data = %q, want private-library-data", string(got))
	}
}

func TestSyncPluginStoreManifestsSkipsCPADistributedArtifact(t *testing.T) {
	root := t.TempDir()
	restore := replaceHomePluginStoreClientForTest(pluginStoreFatalHTTPDoer{t: t})
	defer restore()

	rt := &Runtime{}
	if errSync := rt.syncPluginStoreManifests(context.Background(), homePluginSyncTestConfig(t, root)); errSync != nil {
		t.Fatalf("syncPluginStoreManifests() error = %v", errSync)
	}
	platform := CurrentPluginPlatform()
	target := filepath.Join(root, platform.GOOS, platform.GOARCH, "sample-v0.2.0"+pluginExtension(platform.GOOS))
	if _, errStat := os.Stat(target); !os.IsNotExist(errStat) {
		t.Fatalf("target stat error = %v, want not exist", errStat)
	}
}

func TestSyncPluginStoreManifestsSkipsIdenticalArtifact(t *testing.T) {
	root := t.TempDir()
	platform := CurrentPluginPlatform()
	targetDir := filepath.Join(root, platform.GOOS, platform.GOARCH)
	if errMkdir := os.MkdirAll(targetDir, 0o755); errMkdir != nil {
		t.Fatalf("MkdirAll() error = %v", errMkdir)
	}
	target := filepath.Join(targetDir, "sample-v0.2.0"+pluginExtension(platform.GOOS))
	if errWrite := os.WriteFile(target, []byte("library-data"), 0o644); errWrite != nil {
		t.Fatalf("WriteFile() error = %v", errWrite)
	}
	oldTime := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	if errChtimes := os.Chtimes(target, oldTime, oldTime); errChtimes != nil {
		t.Fatalf("Chtimes() error = %v", errChtimes)
	}
	archiveData := makePluginStoreZip(t, map[string]string{"sample" + pluginExtension(platform.GOOS): "library-data"})
	archiveName := "sample_0.2.0_" + platform.GOOS + "_" + platform.GOARCH + ".zip"
	checksum := sha256.Sum256(archiveData)
	httpClient := pluginStoreHTTPDoer{
		"https://api.github.com/repos/owner/sample-plugin/releases/tags/v0.2.0": []byte(`{
			"tag_name": "v0.2.0",
			"assets": [
				{"name": "` + archiveName + `", "browser_download_url": "https://downloads.example/` + archiveName + `"},
				{"name": "checksums.txt", "browser_download_url": "https://downloads.example/checksums.txt"}
			]
		}`),
		"https://downloads.example/" + archiveName: archiveData,
		"https://downloads.example/checksums.txt":  []byte(hex.EncodeToString(checksum[:]) + "  " + archiveName + "\n"),
	}
	restore := replaceHomePluginStoreClientForTest(httpClient)
	defer restore()

	rt := &Runtime{}
	if errSync := rt.syncPluginStoreManifests(context.Background(), homeLoadedPluginSyncTestConfig(t, root)); errSync != nil {
		t.Fatalf("syncPluginStoreManifests() error = %v", errSync)
	}
	info, errStat := os.Stat(target)
	if errStat != nil {
		t.Fatalf("stat target: %v", errStat)
	}
	if !info.ModTime().Equal(oldTime) {
		t.Fatalf("target mod time = %s, want unchanged %s", info.ModTime(), oldTime)
	}
}

func TestSyncPluginStoreManifestsSkipsCachedArtifactWithoutHTTP(t *testing.T) {
	root := t.TempDir()
	platform := CurrentPluginPlatform()
	archiveData := makePluginStoreZip(t, map[string]string{"sample" + pluginExtension(platform.GOOS): "library-data"})
	archiveName := "sample_0.2.0_" + platform.GOOS + "_" + platform.GOARCH + ".zip"
	checksum := sha256.Sum256(archiveData)
	restore := replaceHomePluginStoreClientForTest(pluginStoreHTTPDoer{
		"https://api.github.com/repos/owner/sample-plugin/releases/tags/v0.2.0": []byte(`{
			"tag_name": "v0.2.0",
			"assets": [
				{"name": "` + archiveName + `", "browser_download_url": "https://downloads.example/` + archiveName + `"},
				{"name": "checksums.txt", "browser_download_url": "https://downloads.example/checksums.txt"}
			]
		}`),
		"https://downloads.example/" + archiveName: archiveData,
		"https://downloads.example/checksums.txt":  []byte(hex.EncodeToString(checksum[:]) + "  " + archiveName + "\n"),
	})

	rt := &Runtime{}
	cfg := homeLoadedPluginSyncTestConfig(t, root)
	if errSync := rt.syncPluginStoreManifests(context.Background(), cfg); errSync != nil {
		t.Fatalf("first syncPluginStoreManifests() error = %v", errSync)
	}
	restore()

	restore = replaceHomePluginStoreClientForTest(pluginStoreFatalHTTPDoer{t: t})
	defer restore()
	if errSync := rt.syncPluginStoreManifests(context.Background(), cfg); errSync != nil {
		t.Fatalf("second syncPluginStoreManifests() error = %v", errSync)
	}
}

func TestSyncPluginStoreManifestsResyncsChangedLocalArtifact(t *testing.T) {
	root := t.TempDir()
	platform := CurrentPluginPlatform()
	archiveData := makePluginStoreZip(t, map[string]string{"sample" + pluginExtension(platform.GOOS): "library-data"})
	archiveName := "sample_0.2.0_" + platform.GOOS + "_" + platform.GOARCH + ".zip"
	checksum := sha256.Sum256(archiveData)
	restore := replaceHomePluginStoreClientForTest(pluginStoreHTTPDoer{
		"https://api.github.com/repos/owner/sample-plugin/releases/tags/v0.2.0": []byte(`{
			"tag_name": "v0.2.0",
			"assets": [
				{"name": "` + archiveName + `", "browser_download_url": "https://downloads.example/` + archiveName + `"},
				{"name": "checksums.txt", "browser_download_url": "https://downloads.example/checksums.txt"}
			]
		}`),
		"https://downloads.example/" + archiveName: archiveData,
		"https://downloads.example/checksums.txt":  []byte(hex.EncodeToString(checksum[:]) + "  " + archiveName + "\n"),
	})
	defer restore()

	rt := &Runtime{}
	cfg := homeLoadedPluginSyncTestConfig(t, root)
	if errSync := rt.syncPluginStoreManifests(context.Background(), cfg); errSync != nil {
		t.Fatalf("first syncPluginStoreManifests() error = %v", errSync)
	}
	target := filepath.Join(root, platform.GOOS, platform.GOARCH, "sample-v0.2.0"+pluginExtension(platform.GOOS))
	if errWrite := os.WriteFile(target, []byte("tampered-data"), 0o644); errWrite != nil {
		t.Fatalf("WriteFile() error = %v", errWrite)
	}

	if errSync := rt.syncPluginStoreManifests(context.Background(), cfg); errSync != nil {
		t.Fatalf("second syncPluginStoreManifests() error = %v", errSync)
	}
	got, errRead := os.ReadFile(target)
	if errRead != nil {
		t.Fatalf("read target: %v", errRead)
	}
	if string(got) != "library-data" {
		t.Fatalf("target data = %q, want library-data", string(got))
	}
}

func TestSyncPluginStoreManifestsRejectsInvalidManifest(t *testing.T) {
	cfg := &config.Config{
		Plugins: config.PluginsConfig{
			Enabled: true,
			Dir:     t.TempDir(),
			Configs: map[string]config.PluginInstanceConfig{
				"sample": homePluginConfigFromYAML(t, `
enabled: true
load-in-home: true
store:
  id: sample
`),
			},
		},
	}
	errSync := (&Runtime{}).syncPluginStoreManifests(context.Background(), cfg)
	if errSync == nil {
		t.Fatal("syncPluginStoreManifests() error = nil, want invalid manifest")
	}
}

func TestDeleteRemovedPluginArtifactsRemovesCurrentPlatformFile(t *testing.T) {
	root := t.TempDir()
	platform := CurrentPluginPlatform()
	targetDir := filepath.Join(root, platform.GOOS, platform.GOARCH)
	if errMkdir := os.MkdirAll(targetDir, 0o755); errMkdir != nil {
		t.Fatalf("MkdirAll() error = %v", errMkdir)
	}
	targets := []string{
		filepath.Join(targetDir, "sample"+pluginExtension(platform.GOOS)),
		filepath.Join(targetDir, "sample-v0.1.0"+pluginExtension(platform.GOOS)),
		filepath.Join(targetDir, "sample-v0.2.0"+pluginExtension(platform.GOOS)),
	}
	for _, target := range targets {
		if errWrite := os.WriteFile(target, []byte("library-data"), 0o644); errWrite != nil {
			t.Fatalf("WriteFile() error = %v", errWrite)
		}
	}

	rt := &Runtime{}
	rt.SetPluginSyncConfigLoader(func(context.Context) (*config.Config, error) {
		return nil, errors.New("catalog configuration unavailable")
	})
	rt.reconcilePluginArtifacts(context.Background(), homeLoadedPluginSyncTestConfig(t, root), &config.Config{
		Plugins: config.PluginsConfig{
			Enabled: true,
			Dir:     root,
			Configs: map[string]config.PluginInstanceConfig{},
		},
	})

	for _, target := range targets {
		if _, errStat := os.Stat(target); !os.IsNotExist(errStat) {
			t.Fatalf("target %s stat error = %v, want not exist", target, errStat)
		}
	}
}

func TestDeleteRemovedPluginArtifactsRemovesHomeArtifactWhenLoadInHomeDisabled(t *testing.T) {
	root := t.TempDir()
	platform := CurrentPluginPlatform()
	targetDir := filepath.Join(root, platform.GOOS, platform.GOARCH)
	if errMkdir := os.MkdirAll(targetDir, 0o755); errMkdir != nil {
		t.Fatalf("MkdirAll() error = %v", errMkdir)
	}
	targets := []string{
		filepath.Join(targetDir, "sample"+pluginExtension(platform.GOOS)),
		filepath.Join(targetDir, "sample-v0.2.0"+pluginExtension(platform.GOOS)),
	}
	for _, target := range targets {
		if errWrite := os.WriteFile(target, []byte("library-data"), 0o644); errWrite != nil {
			t.Fatalf("WriteFile() error = %v", errWrite)
		}
	}

	rt := &Runtime{}
	rt.reconcilePluginArtifacts(context.Background(), homeLoadedPluginSyncTestConfig(t, root), homePluginSyncTestConfig(t, root))

	for _, target := range targets {
		if _, errStat := os.Stat(target); !os.IsNotExist(errStat) {
			t.Fatalf("target %s stat error = %v, want not exist", target, errStat)
		}
	}
}

func TestCurrentPluginFilePathPrefersHighestVersionedFile(t *testing.T) {
	root := t.TempDir()
	platform := CurrentPluginPlatform()
	targetDir := filepath.Join(root, platform.GOOS, platform.GOARCH)
	if errMkdir := os.MkdirAll(targetDir, 0o755); errMkdir != nil {
		t.Fatalf("MkdirAll() error = %v", errMkdir)
	}
	extension := pluginExtension(platform.GOOS)
	paths := []string{
		filepath.Join(targetDir, "sample"+extension),
		filepath.Join(targetDir, "sample-v0.1.0"+extension),
		filepath.Join(targetDir, "sample-v0.2.0"+extension),
	}
	for _, path := range paths {
		if errWrite := os.WriteFile(path, []byte("library-data"), 0o644); errWrite != nil {
			t.Fatalf("WriteFile(%s) error = %v", path, errWrite)
		}
	}

	got, errPath := currentPluginFilePath(root, "sample")
	if errPath != nil {
		t.Fatalf("currentPluginFilePath() error = %v", errPath)
	}
	want := filepath.Join(targetDir, "sample-v0.2.0"+extension)
	if got != want {
		t.Fatalf("currentPluginFilePath() = %q, want %q", got, want)
	}
}

func TestCurrentPluginFilePathPrefersDesiredVersionOverHigherVersion(t *testing.T) {
	root := t.TempDir()
	platform := CurrentPluginPlatform()
	targetDir := filepath.Join(root, platform.GOOS, platform.GOARCH)
	if errMkdir := os.MkdirAll(targetDir, 0o755); errMkdir != nil {
		t.Fatalf("MkdirAll() error = %v", errMkdir)
	}
	extension := pluginExtension(platform.GOOS)
	desired := filepath.Join(targetDir, "sample-v0.1.0"+extension)
	higher := filepath.Join(targetDir, "sample-v0.2.0"+extension)
	for _, path := range []string{desired, higher} {
		if errWrite := os.WriteFile(path, []byte("library-data"), 0o644); errWrite != nil {
			t.Fatalf("WriteFile(%s) error = %v", path, errWrite)
		}
	}

	got, errPath := currentPluginFilePath(root, "sample", "0.1.0")
	if errPath != nil {
		t.Fatalf("currentPluginFilePath() error = %v", errPath)
	}
	if got != desired {
		t.Fatalf("currentPluginFilePath() = %q, want %q", got, desired)
	}
}

func TestCurrentPluginFilePathReturnsEmptyWhenDesiredVersionIsMissing(t *testing.T) {
	root := t.TempDir()
	platform := CurrentPluginPlatform()
	targetDir := filepath.Join(root, platform.GOOS, platform.GOARCH)
	if errMkdir := os.MkdirAll(targetDir, 0o755); errMkdir != nil {
		t.Fatalf("MkdirAll() error = %v", errMkdir)
	}
	target := filepath.Join(targetDir, "sample-v0.2.0"+pluginExtension(platform.GOOS))
	if errWrite := os.WriteFile(target, []byte("library-data"), 0o644); errWrite != nil {
		t.Fatalf("WriteFile() error = %v", errWrite)
	}

	got, errPath := currentPluginFilePath(root, "sample", "0.1.0")
	if errPath != nil {
		t.Fatalf("currentPluginFilePath() error = %v", errPath)
	}
	if got != "" {
		t.Fatalf("currentPluginFilePath() = %q, want empty path", got)
	}
}

func TestCurrentPluginFilePathFallsBackToUnversionedFile(t *testing.T) {
	root := t.TempDir()
	platform := CurrentPluginPlatform()
	targetDir := filepath.Join(root, platform.GOOS, platform.GOARCH)
	if errMkdir := os.MkdirAll(targetDir, 0o755); errMkdir != nil {
		t.Fatalf("MkdirAll() error = %v", errMkdir)
	}
	target := filepath.Join(targetDir, "sample"+pluginExtension(platform.GOOS))
	if errWrite := os.WriteFile(target, []byte("library-data"), 0o644); errWrite != nil {
		t.Fatalf("WriteFile() error = %v", errWrite)
	}

	got, errPath := currentPluginFilePath(root, "sample")
	if errPath != nil {
		t.Fatalf("currentPluginFilePath() error = %v", errPath)
	}
	if got != target {
		t.Fatalf("currentPluginFilePath() = %q, want %q", got, target)
	}
}

func TestPluginStoreInstallOptionsUnloadsBusyRuntimeBeforeWrite(t *testing.T) {
	runtime := &pluginArtifactRuntimeStub{busy: true, unloadOK: true}
	options := pluginStoreInstallOptions("plugins", PluginPlatform{GOOS: "windows", GOARCH: "amd64"}, runtime, "sample")
	if options.PluginLoaded == nil || !options.PluginLoaded() {
		t.Fatal("PluginLoaded() = false, want busy plugin before unload")
	}
	if options.BeforeWrite == nil {
		t.Fatal("BeforeWrite is nil")
	}
	if errPrepare := options.BeforeWrite(); errPrepare != nil {
		t.Fatalf("BeforeWrite() error = %v", errPrepare)
	}
	if runtime.unloadCalls != 1 {
		t.Fatalf("UnloadPlugin calls = %d, want 1", runtime.unloadCalls)
	}
	if options.PluginLoaded() {
		t.Fatal("PluginLoaded() = true, want false after unload")
	}
}

func TestPluginStoreInstallOptionsRejectsStillBusyRuntime(t *testing.T) {
	runtime := &pluginArtifactRuntimeStub{busy: true, unloadOK: false}
	options := pluginStoreInstallOptions("plugins", PluginPlatform{GOOS: "windows", GOARCH: "amd64"}, runtime, "sample")
	errPrepare := options.BeforeWrite()
	if !errors.Is(errPrepare, pluginstore.ErrLoadedPluginLocked) {
		t.Fatalf("BeforeWrite() error = %v, want ErrLoadedPluginLocked", errPrepare)
	}
	if runtime.unloadCalls != 1 {
		t.Fatalf("UnloadPlugin calls = %d, want 1", runtime.unloadCalls)
	}
}

func homePluginSyncTestConfig(t *testing.T, root string) *config.Config {
	t.Helper()
	return &config.Config{
		Plugins: config.PluginsConfig{
			Enabled: true,
			Dir:     root,
			Configs: map[string]config.PluginInstanceConfig{
				"sample": homePluginConfigFromYAML(t, `
enabled: true
store:
  id: sample
  name: Sample
  description: Adds sample support.
  author: owner
  version: 0.2.0
  release-tag: v0.2.0
  repository: https://github.com/owner/sample-plugin
`),
			},
		},
	}
}

func homeLoadedPluginSyncTestConfig(t *testing.T, root string) *config.Config {
	t.Helper()
	cfg := homePluginSyncTestConfig(t, root)
	cfg.Plugins.Configs["sample"] = homePluginConfigFromYAML(t, `
enabled: true
load-in-home: true
store:
  id: sample
  name: Sample
  description: Adds sample support.
  author: owner
  version: 0.2.0
  release-tag: v0.2.0
  repository: https://github.com/owner/sample-plugin
`)
	return cfg
}

func homePluginConfigFromYAML(t *testing.T, text string) config.PluginInstanceConfig {
	t.Helper()
	var item config.PluginInstanceConfig
	if errUnmarshal := yaml.Unmarshal([]byte(text), &item); errUnmarshal != nil {
		t.Fatalf("unmarshal plugin config: %v", errUnmarshal)
	}
	return item
}

func replaceHomePluginStoreClientForTest(httpClient pluginstore.HTTPDoer) func() {
	previous := newPluginStoreClient
	newPluginStoreClient = func(cfg *config.Config, auth []pluginstore.ResolvedAuthConfig) pluginstore.Client {
		return pluginstore.NewClientWithResolvedAuth(httpClient, "", auth)
	}
	return func() {
		newPluginStoreClient = previous
	}
}

func makePluginStoreZip(t *testing.T, files map[string]string) []byte {
	t.Helper()

	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for name, content := range files {
		file, errCreate := writer.Create(name)
		if errCreate != nil {
			t.Fatalf("Create(%s) error = %v", name, errCreate)
		}
		if _, errWrite := file.Write([]byte(content)); errWrite != nil {
			t.Fatalf("Write(%s) error = %v", name, errWrite)
		}
	}
	if errClose := writer.Close(); errClose != nil {
		t.Fatalf("Close() error = %v", errClose)
	}
	return buffer.Bytes()
}

type pluginStoreFatalHTTPDoer struct {
	t *testing.T
}

func (c pluginStoreFatalHTTPDoer) Do(req *http.Request) (*http.Response, error) {
	c.t.Helper()
	c.t.Fatalf("unexpected plugin store HTTP request: %s", req.URL.String())
	return nil, fmt.Errorf("unexpected plugin store HTTP request: %s", req.URL.String())
}

type pluginStoreHTTPDoer map[string][]byte

func (c pluginStoreHTTPDoer) Do(req *http.Request) (*http.Response, error) {
	body, ok := c[req.URL.String()]
	if !ok {
		return &http.Response{
			StatusCode: http.StatusNotFound,
			Body:       io.NopCloser(strings.NewReader("not found")),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(bytes.NewReader(body)),
		Header:     make(http.Header),
		Request:    req,
	}, nil
}

type authenticatedPluginStoreHTTPDoer struct {
	responses     pluginStoreHTTPDoer
	authorization map[string]string
}

func (c authenticatedPluginStoreHTTPDoer) Do(req *http.Request) (*http.Response, error) {
	if want := c.authorization[req.URL.String()]; want != "" && req.Header.Get("Authorization") != want {
		return &http.Response{
			StatusCode: http.StatusUnauthorized,
			Body:       io.NopCloser(strings.NewReader("unauthorized")),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	}
	return c.responses.Do(req)
}

type pluginArtifactRuntimeStub struct {
	busy        bool
	unloadOK    bool
	unloadCalls int
}

func (r *pluginArtifactRuntimeStub) PluginBusy(id string) bool {
	_ = id
	return r.busy
}

func (r *pluginArtifactRuntimeStub) UnloadPlugin(id string) bool {
	_ = id
	r.unloadCalls++
	if r.unloadOK {
		r.busy = false
		return true
	}
	return false
}
