package management

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"

	cpaconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	"github.com/router-for-me/CLIProxyAPIHome/internal/home"
	"gopkg.in/yaml.v3"
)

func TestConfigV8OAuthVertexOpenAPILifecycle(t *testing.T) {
	repo, request := newConfigV8AuthIndexRouter(t)
	path := `/v8/management/config/oauth/model-alias/vertex`
	body := `[{"name":"gemini-2.5-flash","alias":"force-openapi","openapi":true},{"name":"gemini-2.5-flash","alias":"force-native","openapi":false},{"name":"gemini-2.5-flash","alias":"automatic","openapi":null}]`
	request("PUT", path, body, http.StatusOK)
	request("PUT", path, request("GET", path, "", http.StatusOK), http.StatusOK)
	yamlConfig := request("GET", "/v8/management/config.yaml", "", http.StatusOK)
	request("PUT", "/v8/management/config.yaml", yamlConfig, http.StatusOK)
	auth := &coreauth.Auth{ID: "vertex-oauth-lifecycle", Index: "vertex-oauth-lifecycle", Provider: "vertex", Status: coreauth.StatusActive, Metadata: map[string]any{"access_token": "fixture"}}
	for cycle := range 2 {
		cfg, _, errConfig := repo.LoadConfigAsRuntimeConfig(t.Context())
		if errConfig != nil {
			t.Fatal(errConfig)
		}
		aliases := cfg.OAuthModelAlias["vertex"]
		if len(aliases) != 3 || aliases[0].OpenAPI == nil || !*aliases[0].OpenAPI || aliases[1].OpenAPI == nil || *aliases[1].OpenAPI || aliases[2].OpenAPI != nil {
			t.Fatalf("cycle %d lost persisted alias execution settings: %+v", cycle, aliases)
		}
		out, errSanitize := home.SanitizeAuthForDownstream(auth, cfg)
		if errSanitize != nil {
			t.Fatal(errSanitize)
		}
		var rules []cpaconfig.VertexCompatModel
		if errDecode := json.Unmarshal([]byte(out.Attributes["vertex_model_openapi"]), &rules); errDecode != nil {
			t.Fatal(errDecode)
		}
		if len(rules) != 2 || rules[0].OpenAPI == nil || !*rules[0].OpenAPI || rules[1].OpenAPI == nil || *rules[1].OpenAPI {
			t.Fatalf("cycle %d lost execution projection", cycle)
		}
		runtime, errRuntime := home.NewRuntime(cfg)
		if errRuntime != nil {
			t.Fatal(errRuntime)
		}
		runtime.SetClusterAdapter(cluster.NewRuntimeAdapter(repo, "oauth-openapi-test"))
		wire, errWire := runtime.ReadConfigYAMLContext(t.Context())
		runtime.Stop()
		if errWire != nil {
			t.Fatal(errWire)
		}
		var root map[string]any
		if errDecode := yaml.Unmarshal(wire, &root); errDecode != nil {
			t.Fatal(errDecode)
		}
		if _, exists := root["oauth-model-alias"]; exists {
			t.Fatal("Home forwarded its alias resolution table")
		}
		output := t.TempDir()
		if _, errExport := cluster.ExportLocalState(t.Context(), cluster.ExportOptions{Repository: repo, OutputDir: output, AuthDirName: "auth"}); errExport != nil {
			t.Fatal(errExport)
		}
		next, nextRequest := newConfigV8AuthIndexRouter(t)
		if _, errImport := cluster.ImportLocalState(t.Context(), cluster.ImportOptions{Repository: next, ConfigPath: filepath.Join(output, "config.yaml")}); errImport != nil {
			t.Fatal(errImport)
		}
		repo, request = next, nextRequest
	}
	request("DELETE", path, "", http.StatusOK)
	cfg, _, errConfig := repo.LoadConfigAsRuntimeConfig(t.Context())
	if errConfig != nil {
		t.Fatal(errConfig)
	}
	out, errSanitize := home.SanitizeAuthForDownstream(auth, cfg)
	if errSanitize != nil {
		t.Fatal(errSanitize)
	}
	if out.Attributes["vertex_model_openapi"] != "" {
		t.Fatal("deleted aliases left stale execution rules")
	}
}
