package home

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginstore"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"gopkg.in/yaml.v3"
)

func TestBuildPluginSyncPlanDirectUsesFirstRuleAndSkipsInstalledVersion(t *testing.T) {
	runtime := pluginSyncPlanRuntime(t, `
plugins:
  enabled: true
  configs:
    sample:
      enabled: true
      store:
        schema-version: 2
        id: sample
        version: 1.0.0
        install:
          type: direct
          artifacts:
            - goos: linux
              goarch: amd64
              url: https://downloads.example/private/sample.zip
              sha256: 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
`)
	runtime.SetPluginStoreAuthResolver(func(context.Context) ([]pluginstore.ResolvedAuthConfig, error) {
		return []pluginstore.ResolvedAuthConfig{
			{Match: "https://downloads.example/private/", ApplyTo: []string{pluginstore.RequestKindArtifact}, Type: pluginstore.AuthTypeBearer, Token: pluginstore.Secret("first-token")},
			{Match: "https://downloads.example/private/", ApplyTo: []string{pluginstore.RequestKindArtifact}, Type: pluginstore.AuthTypeBearer, Token: pluginstore.Secret("second-token")},
		}, nil
	})
	request := pluginstore.PluginSyncRequest{SchemaVersion: pluginstore.PluginSyncSchemaVersion, GOOS: "linux", GOARCH: "amd64"}
	response, errBuild := runtime.BuildPluginSyncPlan(context.Background(), request)
	if errBuild != nil {
		t.Fatalf("BuildPluginSyncPlan() error = %v", errBuild)
	}
	if len(response.Items) != 1 || len(response.Items[0].Auth) != 1 || string(response.Items[0].Auth[0].Token) != "first-token" {
		response.Clear()
		t.Fatalf("response = %#v, want first matching token", response)
	}
	response.Clear()
	request.InstalledVersions = map[string]string{"sample": "1.0.0"}
	skipped, errSkipped := runtime.BuildPluginSyncPlan(context.Background(), request)
	if errSkipped != nil {
		t.Fatalf("BuildPluginSyncPlan(skipped) error = %v", errSkipped)
	}
	defer skipped.Clear()
	if len(skipped.Items) != 0 {
		t.Fatalf("skipped items = %#v, want empty", skipped.Items)
	}
}

func TestBuildPluginSyncPlanResolvesLegacyDirectVersion(t *testing.T) {
	const sourceURL = "https://store.example/registry.json"
	const checksum = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	const registryJSON = `{"schema_version":2,"plugins":[{"id":"legacy","name":"Legacy","author":"test","description":"test",
		"version":"2.0.0","install":{"type":"direct","artifacts":[{"goos":"linux","goarch":"amd64","url":"https://files.example/v2.zip","sha256":"` + checksum + `"}]},
		"versions":[{"version":"1.0.0","install":{"type":"direct","artifacts":[{"goos":"linux","goarch":"amd64","url":"https://files.example/v1.zip","sha256":"` + checksum + `"}]}}]}]}`
	for _, tt := range []struct{ version, archive string }{
		{"v1.0.0", "v1.zip"}, {"2.0.0", "v2.zip"}, {"0.5.0", ""},
	} {
		t.Run(tt.version, func(t *testing.T) {
			rt := pluginSyncPlanRuntime(t, fmt.Sprintf(`
plugins:
  enabled: true
  configs:
    current:
      enabled: true
      store: {id: current, version: 1.0.0, install: {type: direct, artifacts: [{goos: linux, goarch: amd64, url: "https://public.example/new.zip", sha256: "%s"}]}}
    legacy:
      enabled: true
      store: {schema-version: 2, id: legacy, version: %s, source-url: "%s", install: {type: direct}}
`, checksum, tt.version, sourceURL))
			client := &pluginSyncPlanHTTPClient{responses: map[string]pluginSyncPlanHTTPResponse{
				sourceURL: {body: registryJSON, wantAuth: "Bearer registry-only"},
			}}
			rt.pluginSyncHTTPClient = client
			rt.SetPluginStoreAuthResolver(func(context.Context) ([]pluginstore.ResolvedAuthConfig, error) {
				return []pluginstore.ResolvedAuthConfig{
					{Match: sourceURL, ApplyTo: []string{pluginstore.RequestKindRegistry}, Type: pluginstore.AuthTypeBearer, Token: pluginstore.Secret("registry-only")},
					{Match: "https://files.example/", ApplyTo: []string{pluginstore.RequestKindArtifact}, Type: pluginstore.AuthTypeBearer, Token: pluginstore.Secret("artifact-only")},
				}, nil
			})
			req := pluginstore.PluginSyncRequest{SchemaVersion: pluginstore.PluginSyncSchemaVersion, GOOS: "linux", GOARCH: "amd64"}
			plan, errPlan := rt.BuildPluginSyncPlan(t.Context(), req)
			defer plan.Clear()
			if tt.archive == "" {
				if errPlan == nil || len(plan.Items) != 0 {
					t.Fatal("a missing pinned version must fail the plan")
				}
			} else {
				if errPlan != nil || len(plan.Items) != 2 || plan.Items[0].Manifest.ID != "current" {
					t.Fatalf("mixed plan has %d items, error %v", len(plan.Items), errPlan)
				}
				item := plan.Items[1]
				files := item.Manifest.Install.Artifacts
				if item.Manifest.ID != "legacy" || item.Manifest.Version != tt.version || len(files) != 1 ||
					files[0].URL != "https://files.example/"+tt.archive || files[0].SHA256 != checksum ||
					len(item.Auth) != 1 || string(item.Auth[0].Token) != "artifact-only" {
					t.Fatal("legacy plan did not pin the requested artifact and its own download credentials")
				}
			}
			clear(client.authorization)
			req.InstalledVersions = map[string]string{"legacy": tt.version}
			skipped, errSkip := rt.BuildPluginSyncPlan(t.Context(), req)
			defer skipped.Clear()
			if errSkip != nil || len(skipped.Items) != 1 || len(client.authorization) != 0 {
				t.Fatalf("installed legacy version: items=%d requests=%d error=%v", len(skipped.Items), len(client.authorization), errSkip)
			}
		})
	}
}

func TestBuildPluginSyncPlanUsesSemanticVersionEquality(t *testing.T) {
	runtime := pluginSyncPlanRuntime(t, `
plugins:
  enabled: true
  configs:
    sample:
      enabled: true
      store:
        schema-version: 2
        id: sample
        version: v1.0.0
        install:
          type: direct
          artifacts:
            - goos: linux
              goarch: amd64
              url: https://downloads.example/sample.zip
              sha256: 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
`)
	tests := []struct {
		name      string
		installed string
		wantItems int
	}{
		{name: "leading v is equivalent", installed: "1.0.0", wantItems: 0},
		{name: "missing numeric segments are equivalent", installed: "1", wantItems: 0},
		{name: "upgrade remains planned", installed: "0.9.0", wantItems: 1},
		{name: "downgrade remains planned", installed: "1.1.0", wantItems: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response, errBuild := runtime.BuildPluginSyncPlan(context.Background(), pluginstore.PluginSyncRequest{
				SchemaVersion:     pluginstore.PluginSyncSchemaVersion,
				GOOS:              "linux",
				GOARCH:            "amd64",
				InstalledVersions: map[string]string{"sample": test.installed},
			})
			if errBuild != nil {
				t.Fatalf("BuildPluginSyncPlan() error = %v", errBuild)
			}
			defer response.Clear()
			if len(response.Items) != test.wantItems {
				t.Fatalf("items = %d, want %d", len(response.Items), test.wantItems)
			}
		})
	}
}

func TestBuildPluginSyncPlanSkipsAuthResolutionWhenEverythingIsInstalled(t *testing.T) {
	runtime := pluginSyncPlanRuntime(t, `
plugins:
  enabled: true
  configs:
    sample:
      enabled: true
      store:
        schema-version: 2
        id: sample
        version: 1.0.0
        install:
          type: direct
          artifacts:
            - goos: linux
              goarch: amd64
              url: https://downloads.example/sample.zip
              sha256: 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
`)
	runtime.SetPluginStoreAuthResolver(func(context.Context) ([]pluginstore.ResolvedAuthConfig, error) {
		return nil, errors.New("auth resolver must not be called")
	})
	response, errBuild := runtime.BuildPluginSyncPlan(context.Background(), pluginstore.PluginSyncRequest{
		SchemaVersion:     pluginstore.PluginSyncSchemaVersion,
		GOOS:              "linux",
		GOARCH:            "amd64",
		InstalledVersions: map[string]string{"sample": "1.0.0"},
	})
	if errBuild != nil {
		t.Fatalf("BuildPluginSyncPlan() error = %v", errBuild)
	}
	defer response.Clear()
	if len(response.Items) != 0 {
		t.Fatalf("items = %#v, want empty", response.Items)
	}
}

func TestBuildPluginSyncPlanUsesAuthoritativeConfigLoader(t *testing.T) {
	runtime := pluginSyncPlanRuntime(t, "plugins:\n  enabled: false\n")
	var current config.Config
	if errYAML := yaml.Unmarshal([]byte(`
plugins:
  enabled: true
  configs:
    current:
      enabled: true
      store:
        schema-version: 2
        id: current
        version: 2.0.0
        install:
          type: direct
          artifacts:
            - goos: linux
              goarch: amd64
              url: https://downloads.example/current.zip
              sha256: 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
`), &current); errYAML != nil {
		t.Fatalf("yaml.Unmarshal() error = %v", errYAML)
	}
	current.NormalizePluginsConfig()
	runtime.SetPluginSyncConfigLoader(func(context.Context) (*config.Config, error) {
		return &current, nil
	})
	runtime.SetPluginStoreAuthResolver(func(context.Context) ([]pluginstore.ResolvedAuthConfig, error) { return nil, nil })
	response, errBuild := runtime.BuildPluginSyncPlan(context.Background(), pluginstore.PluginSyncRequest{
		SchemaVersion: pluginstore.PluginSyncSchemaVersion,
		GOOS:          "linux",
		GOARCH:        "amd64",
	})
	if errBuild != nil {
		t.Fatalf("BuildPluginSyncPlan() error = %v", errBuild)
	}
	defer response.Clear()
	if len(response.Items) != 1 || response.Items[0].Manifest.ID != "current" {
		t.Fatalf("response items = %#v, want authoritative current config", response.Items)
	}
}

func TestBuildPluginSyncPlanSortsByManifestID(t *testing.T) {
	runtime := pluginSyncPlanRuntime(t, `
plugins:
  enabled: true
  configs:
    a-config-key:
      enabled: true
      store:
        schema-version: 2
        id: z-plugin
        version: 1.0.0
        install:
          type: direct
          artifacts:
            - goos: linux
              goarch: amd64
              url: https://downloads.example/z-plugin.zip
              sha256: 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
    z-config-key:
      enabled: true
      store:
        schema-version: 2
        id: a-plugin
        version: 1.0.0
        install:
          type: direct
          artifacts:
            - goos: linux
              goarch: amd64
              url: https://downloads.example/a-plugin.zip
              sha256: 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
`)
	runtime.SetPluginStoreAuthResolver(func(context.Context) ([]pluginstore.ResolvedAuthConfig, error) { return nil, nil })
	response, errBuild := runtime.BuildPluginSyncPlan(context.Background(), pluginstore.PluginSyncRequest{
		SchemaVersion: pluginstore.PluginSyncSchemaVersion,
		GOOS:          "linux",
		GOARCH:        "amd64",
	})
	if errBuild != nil {
		t.Fatalf("BuildPluginSyncPlan() error = %v", errBuild)
	}
	defer response.Clear()
	if len(response.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(response.Items))
	}
	if got := []string{response.Items[0].Manifest.ID, response.Items[1].Manifest.ID}; got[0] != "a-plugin" || got[1] != "z-plugin" {
		t.Fatalf("manifest IDs = %v, want [a-plugin z-plugin]", got)
	}
}

func TestBuildPluginSyncPlanRejectsDuplicateManifestIDsWithoutPartialItems(t *testing.T) {
	runtime := pluginSyncPlanRuntime(t, `
plugins:
  enabled: true
  configs:
    first:
      enabled: true
      store:
        schema-version: 2
        id: duplicate
        name: Duplicate Plugin
        description: Duplicate plugin
        author: example
        version: 1.0.0
        release-tag: v1.0.0
        repository: https://github.com/example/duplicate
        install:
          type: github-release
    second:
      enabled: true
      store:
        schema-version: 2
        id: duplicate
        name: Duplicate Plugin
        description: Duplicate plugin
        author: example
        version: 2.0.0
        release-tag: v2.0.0
        repository: https://github.com/example/duplicate
        install:
          type: github-release
`)
	client := &pluginSyncPlanHTTPClient{responses: map[string]pluginSyncPlanHTTPResponse{}}
	runtime.pluginSyncHTTPClient = client
	resolverCalls := 0
	runtime.SetPluginStoreAuthResolver(func(context.Context) ([]pluginstore.ResolvedAuthConfig, error) {
		resolverCalls++
		return nil, nil
	})
	response, errBuild := runtime.BuildPluginSyncPlan(context.Background(), pluginstore.PluginSyncRequest{
		SchemaVersion: pluginstore.PluginSyncSchemaVersion,
		GOOS:          "linux",
		GOARCH:        "amd64",
	})
	if errBuild == nil {
		response.Clear()
		t.Fatal("BuildPluginSyncPlan() error = nil")
	}
	if len(response.Items) != 0 {
		response.Clear()
		t.Fatalf("partial response items = %#v", response.Items)
	}
	if resolverCalls != 0 {
		t.Fatalf("auth resolver calls = %d, want 0", resolverCalls)
	}
	if len(client.authorization) != 0 {
		t.Fatalf("network requests = %#v, want none", client.authorization)
	}
}

func TestPluginSyncRequestErrorRedactsURLErrorAndPreservesCause(t *testing.T) {
	cause := errors.New("connection failed")
	errRequest := &url.Error{
		Op:  http.MethodGet,
		URL: "https://objects.example/plugin.zip?signature=secret-token",
		Err: cause,
	}
	errResult := pluginSyncRequestError(errRequest.URL, errRequest)
	if strings.Contains(errResult.Error(), "secret-token") || strings.Contains(errResult.Error(), "signature=") {
		t.Fatalf("error leaked signed url: %v", errResult)
	}
	if !errors.Is(errResult, cause) {
		t.Fatalf("errors.Is(error, cause) = false: %v", errResult)
	}
}

func TestBuildPluginSyncPlanRejectsSchemaPlatformAndHTTPDirectArtifact(t *testing.T) {
	runtime := pluginSyncPlanRuntime(t, `
plugins:
  enabled: true
  configs:
    sample:
      enabled: true
      store:
        schema-version: 2
        id: sample
        version: 1.0.0
        install:
          type: direct
          artifacts:
            - goos: linux
              goarch: amd64
              url: http://downloads.example/sample.zip
              sha256: 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
`)
	tests := []pluginstore.PluginSyncRequest{
		{SchemaVersion: 2, GOOS: "linux", GOARCH: "amd64"},
		{SchemaVersion: pluginstore.PluginSyncSchemaVersion, GOOS: "", GOARCH: "amd64"},
		{SchemaVersion: pluginstore.PluginSyncSchemaVersion, GOOS: "linux", GOARCH: "amd64"},
	}
	for index, request := range tests {
		response, errBuild := runtime.BuildPluginSyncPlan(context.Background(), request)
		response.Clear()
		if errBuild == nil {
			t.Fatalf("case %d error = nil", index)
		}
	}
}

func TestBuildPluginSyncPlanGitHubPinsArchiveAndSeparatesAuthAcrossRedirect(t *testing.T) {
	runtime := pluginSyncPlanRuntime(t, `
plugins:
  enabled: true
  configs:
    private-plugin:
      enabled: true
      store:
        schema-version: 2
        id: private-plugin
        name: Private Plugin
        description: Private plugin
        author: example
        version: 1.0.0
        release-tag: v1.0.0
        repository: https://github.com/example/private-plugin
        install:
          type: github-release
`)
	metadataURL := "https://api.github.com/repos/example/private-plugin/releases/tags/v1.0.0"
	archiveAPI := "https://api.github.com/assets/1"
	checksumsAPI := "https://api.github.com/assets/2"
	redirectURL := "https://objects.example/checksums.txt?signature=temporary"
	client := &pluginSyncPlanHTTPClient{responses: map[string]pluginSyncPlanHTTPResponse{
		metadataURL:  {body: fmt.Sprintf(`{"tag_name":"v1.0.0","assets":[{"url":%q,"name":"private-plugin_1.0.0_linux_amd64.zip","browser_download_url":"https://github.com/example/private-plugin/releases/download/v1.0.0/private-plugin.zip"},{"url":%q,"name":"checksums.txt","browser_download_url":"https://github.com/example/private-plugin/releases/download/v1.0.0/checksums.txt"}]}`, archiveAPI, checksumsAPI), wantAuth: "Bearer metadata-token"},
		checksumsAPI: {status: http.StatusFound, location: redirectURL, wantAuth: "Bearer artifact-token"},
		redirectURL:  {body: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef  private-plugin_1.0.0_linux_amd64.zip\n"},
	}}
	runtime.pluginSyncHTTPClient = client
	runtime.SetPluginStoreAuthResolver(func(context.Context) ([]pluginstore.ResolvedAuthConfig, error) {
		return []pluginstore.ResolvedAuthConfig{
			{Match: "https://api.github.com/repos/example/private-plugin/releases/", ApplyTo: []string{pluginstore.RequestKindMetadata}, Type: pluginstore.AuthTypeGitHubToken, Token: pluginstore.Secret("metadata-token")},
			{Match: "https://api.github.com/assets/", ApplyTo: []string{pluginstore.RequestKindArtifact}, Type: pluginstore.AuthTypeGitHubToken, Token: pluginstore.Secret("artifact-token")},
		}, nil
	})
	response, errBuild := runtime.BuildPluginSyncPlan(context.Background(), pluginstore.PluginSyncRequest{
		SchemaVersion: pluginstore.PluginSyncSchemaVersion, GOOS: "linux", GOARCH: "amd64",
	})
	if errBuild != nil {
		t.Fatalf("BuildPluginSyncPlan() error = %v", errBuild)
	}
	defer response.Clear()
	if len(response.Items) != 1 || len(response.Items[0].Manifest.Install.Artifacts) != 1 {
		t.Fatalf("response = %#v, want one pinned artifact", response)
	}
	artifact := response.Items[0].Manifest.Install.Artifacts[0]
	if artifact.URL != archiveAPI || artifact.SHA256 == "" || response.Items[0].Manifest.InstallType() != pluginstore.InstallTypeDirect {
		t.Fatalf("artifact = %#v, want pinned API archive", artifact)
	}
	if len(response.Items[0].Auth) != 1 || string(response.Items[0].Auth[0].Token) != "artifact-token" {
		t.Fatalf("auth = %#v, want only final archive artifact rule", response.Items[0].Auth)
	}
	if got := client.authorization[redirectURL]; got != "" {
		t.Fatalf("redirect authorization = %q, want empty", got)
	}
}

func TestBuildPluginSyncPlanExpirationStartsAfterPlanning(t *testing.T) {
	runtime := pluginSyncPlanRuntime(t, `
plugins:
  enabled: true
  configs:
    sample:
      enabled: true
      store:
        schema-version: 2
        id: sample
        name: Sample Plugin
        description: Sample plugin
        author: example
        version: 1.0.0
        release-tag: v1.0.0
        repository: https://github.com/example/sample
        install:
          type: github-release
`)
	metadataURL := "https://api.github.com/repos/example/sample/releases/tags/v1.0.0"
	checksumsURL := "https://downloads.example/checksums.txt"
	startedAt := time.Date(2026, time.July, 17, 10, 0, 0, 0, time.UTC)
	finishedAt := startedAt.Add(3 * time.Minute)
	currentTime := startedAt
	runtime.pluginSyncHTTPClient = &pluginSyncPlanHTTPClient{responses: map[string]pluginSyncPlanHTTPResponse{
		metadataURL: {
			body:         fmt.Sprintf(`{"tag_name":"v1.0.0","assets":[{"name":"sample_1.0.0_linux_amd64.zip","browser_download_url":"https://downloads.example/sample.zip"},{"name":"checksums.txt","browser_download_url":%q}]}`, checksumsURL),
			beforeReturn: func() { currentTime = finishedAt },
		},
		checksumsURL: {body: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef  sample_1.0.0_linux_amd64.zip\n"},
	}}
	runtime.SetPluginStoreAuthResolver(func(context.Context) ([]pluginstore.ResolvedAuthConfig, error) { return nil, nil })
	response, errBuild := runtime.buildPluginSyncPlan(context.Background(), pluginstore.PluginSyncRequest{
		SchemaVersion: pluginstore.PluginSyncSchemaVersion,
		GOOS:          "linux",
		GOARCH:        "amd64",
	}, func() time.Time { return currentTime })
	if errBuild != nil {
		response.Clear()
		t.Fatalf("buildPluginSyncPlan() error = %v", errBuild)
	}
	defer response.Clear()
	wantExpiresAt := finishedAt.Add(pluginSyncPlanLifetime)
	if !response.ExpiresAt.Equal(wantExpiresAt) {
		t.Fatalf("expires_at = %s, want %s", response.ExpiresAt, wantExpiresAt)
	}

	disabled := pluginSyncPlanRuntime(t, "plugins:\n  enabled: false\n")
	empty, errEmpty := disabled.buildPluginSyncPlan(context.Background(), pluginstore.PluginSyncRequest{
		SchemaVersion: pluginstore.PluginSyncSchemaVersion,
		GOOS:          "linux",
		GOARCH:        "amd64",
	}, func() time.Time { return finishedAt })
	if errEmpty != nil {
		t.Fatalf("BuildPluginSyncPlan(disabled) error = %v", errEmpty)
	}
	defer empty.Clear()
	if len(empty.Items) != 0 {
		t.Fatalf("disabled items = %#v, want empty", empty.Items)
	}
	if errValidate := empty.Validate(finishedAt); errValidate != nil {
		t.Fatalf("disabled response Validate() error = %v", errValidate)
	}
}

func TestBuildPluginSyncPlanFailsWholeRequestWithoutPartialItems(t *testing.T) {
	runtime := pluginSyncPlanRuntime(t, `
plugins:
  enabled: true
  configs:
    a-valid:
      enabled: true
      store:
        schema-version: 2
        id: a-valid
        version: 1.0.0
        install:
          type: direct
          artifacts:
            - goos: linux
              goarch: amd64
              url: https://downloads.example/a-valid.zip
              sha256: 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
    z-bad:
      enabled: true
      store:
        schema-version: 2
        id: z-bad
        name: Bad Plugin
        description: Bad plugin
        author: example
        version: 1.0.0
        release-tag: v1.0.0
        repository: https://github.com/example/z-bad
        install:
          type: github-release
`)
	runtime.pluginSyncHTTPClient = &pluginSyncPlanHTTPClient{responses: map[string]pluginSyncPlanHTTPResponse{}}
	response, errBuild := runtime.BuildPluginSyncPlan(context.Background(), pluginstore.PluginSyncRequest{
		SchemaVersion: pluginstore.PluginSyncSchemaVersion, GOOS: "linux", GOARCH: "amd64",
	})
	if errBuild == nil {
		response.Clear()
		t.Fatal("BuildPluginSyncPlan() error = nil")
	}
	if len(response.Items) != 0 {
		response.Clear()
		t.Fatalf("partial response items = %#v", response.Items)
	}
}

type pluginSyncPlanHTTPResponse struct {
	status       int
	body         string
	location     string
	wantAuth     string
	beforeReturn func()
}

type pluginSyncPlanHTTPClient struct {
	responses     map[string]pluginSyncPlanHTTPResponse
	authorization map[string]string
}

func (c *pluginSyncPlanHTTPClient) Do(req *http.Request) (*http.Response, error) {
	if c.authorization == nil {
		c.authorization = map[string]string{}
	}
	c.authorization[req.URL.String()] = req.Header.Get("Authorization")
	item, ok := c.responses[req.URL.String()]
	if !ok {
		return nil, fmt.Errorf("unexpected request %s", req.URL.String())
	}
	if item.beforeReturn != nil {
		item.beforeReturn()
	}
	if item.wantAuth != "" && req.Header.Get("Authorization") != item.wantAuth {
		return nil, fmt.Errorf("authorization for %s = %q, want %q", req.URL.String(), req.Header.Get("Authorization"), item.wantAuth)
	}
	status := item.status
	if status == 0 {
		status = http.StatusOK
	}
	header := make(http.Header)
	if item.location != "" {
		header.Set("Location", item.location)
	}
	return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(item.body)), Request: req}, nil
}

func pluginSyncPlanRuntime(t *testing.T, raw string) *Runtime {
	t.Helper()
	var cfg config.Config
	if errYAML := yaml.Unmarshal([]byte(raw), &cfg); errYAML != nil {
		t.Fatalf("yaml.Unmarshal() error = %v", errYAML)
	}
	cfg.NormalizePluginsConfig()
	runtime, errRuntime := NewRuntime(&cfg)
	if errRuntime != nil {
		t.Fatalf("NewRuntime() error = %v", errRuntime)
	}
	t.Cleanup(runtime.Stop)
	return runtime
}
