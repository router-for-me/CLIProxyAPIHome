package get

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginstore"
	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
	"github.com/router-for-me/CLIProxyAPIHome/internal/respserver/dispatch"
	"github.com/tidwall/gjson"
	"gopkg.in/yaml.v3"
)

func TestLocalExecutorCatalogAndDispatch(t *testing.T) {
	rt, repo := newGetTestRuntime(t)
	ctx := t.Context()
	const native = "agy/gemini-4-carbon"
	const public = "acp/" + native
	const plugin = "third-party-acp"
	model := public
	var cfg config.Config
	if err := yaml.Unmarshal([]byte(`
plugins:
  enabled: true
  configs:
    third-party-acp:
      enabled: true
      private-account-path: account.json
      private-auth: {enabled: false, order: [a, b]}
      store:
        schema-version: 2
        id: third-party-acp
        version: 0.6.31
        tags: [" aCp "]
        install:
          type: direct
          artifacts:
            - goos: linux
              goarch: amd64
              url: https://downloads.example/acp.zip
              sha256: 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
`), &cfg); err != nil {
		t.Fatal(err)
	}
	cfg.NormalizePluginsConfig()
	var effective map[string]any
	item := cfg.Plugins.Configs[plugin]
	if errDecode := item.Raw.Decode(&effective); errDecode != nil {
		t.Fatal(errDecode)
	}
	effective["priority"] = 0
	fullConfig, errEncode := yaml.Marshal(effective)
	if errEncode != nil {
		t.Fatal(errEncode)
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(fullConfig))
	rt.SetLocalExecutorCatalogLoader(func(_ context.Context, _ *config.Config, manifest pluginstore.Manifest, _ []byte) ([]registry.ModelInfo, error) {
		if manifest.ID != plugin || manifest.Version != "0.6.31" {
			return nil, fmt.Errorf("read unexpected plugin artifact: %s %s", manifest.ID, manifest.Version)
		}
		return []registry.ModelInfo{{ID: native}, {ID: "gemini-4-carbon"}}, nil
	})
	rt.SetPluginSyncConfigLoader(func(context.Context) (*config.Config, error) { return &cfg, nil })
	rt.SetPluginSyncNodeActive(func(_ context.Context, id, fingerprint string) (bool, error) {
		return fingerprint == "cert-"+id && (id == "a" || id == "b"), nil
	})
	channel, errChannel := repo.CreateChannelGroup(ctx, "ACP test channel", false)
	if errChannel != nil {
		t.Fatal(errChannel)
	}
	addKey := func(key, model string, channelIDs []uint, perModel bool) {
		t.Helper()
		update := cluster.APIKeyEntryUpdate{APIKey: key}
		if !perModel && len(channelIDs) > 0 {
			baseChannels := channelIDs
			update.Channels = &baseChannels
			channelIDs = nil
		}
		if model != "" {
			group, errGroup := repo.CreateModelGroup(ctx, key, false)
			if errGroup != nil {
				t.Fatal(errGroup)
			}
			if _, errDetail := repo.CreateModelGroupDetail(ctx, group.ID, model, channelIDs); errDetail != nil {
				t.Fatal(errDetail)
			}
			update.ModelGroups = &[]uint{group.ID}
		}
		if _, errKey := repo.CreateAPIKey(ctx, update); errKey != nil {
			t.Fatal(errKey)
		}
	}
	addKey("acp-open", "", nil, false)
	addKey("acp-allowed", public, nil, false)
	addKey("acp-native-only", native, nil, false)
	addKey("acp-channel", "", []uint{channel.ID}, false)
	addKey("acp-model-channel", public, []uint{channel.ID}, true)
	if got := rt.CoreManager().List(); len(got) != 0 {
		t.Fatalf("fixture has upstream credentials: %d", len(got))
	}
	send := func(t *testing.T, kind, key, nodeID string) []byte {
		t.Helper()
		env := dispatch.Env{Runtime: rt, NodeID: nodeID, ClientCertificateFingerprint: "cert-" + nodeID}
		payload, errMarshal := json.Marshal(map[string]any{
			"type": kind, "model": model, "query": map[string]string{"key": key},
			"headers": map[string]string{"x-route-acp": "true"},
		})
		if errMarshal != nil {
			t.Fatal(errMarshal)
		}
		reply := handleDefault(ctx, env, []string{"GET", string(payload)})
		if reply.Kind != dispatch.ReplyKindBulkString {
			t.Fatalf("unexpected RESP response: %v", reply.Kind)
		}
		return reply.BulkString
	}
	tests := []struct {
		name, node, key, wantError string
		listed                     bool
	}{
		{"active node without a plugin report", "a", "acp-open", "", true},
		{"matching model group", "a", "acp-allowed", "", true},
		{"same catalog on a second node", "b", "acp-open", "", true},
		{"revoked node", "revoked", "acp-open", "local_executor_unavailable", true},
		{"no mTLS identity", "", "acp-open", "local_executor_unavailable", true},
		{"wrong model group", "a", "acp-native-only", "model_forbidden", false},
		{"channel restriction", "a", "acp-channel", "model_forbidden", false},
		{"model channel restriction", "a", "acp-model-channel", "model_forbidden", false},
		{"invalid key", "a", "unknown-key", "invalid_credential", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			models := send(t, "models", tt.key, tt.node)
			if listed := gjson.GetBytes(models, plugin+`.#(id=="`+public+`")`).Exists(); listed != tt.listed {
				t.Fatalf("listing = %s", models)
			}

			reply := send(t, "local-plugin-dispatch", tt.key, tt.node)
			if got := gjson.GetBytes(reply, "error.type").String(); got != tt.wantError {
				t.Fatalf("dispatch error = %q, want %q", got, tt.wantError)
			}
			if tt.wantError == "" {
				if gjson.GetBytes(reply, "kind").String() != "local-plugin" ||
					gjson.GetBytes(reply, "model").String() != native ||
					gjson.GetBytes(reply, "plugin").String() != plugin ||
					gjson.GetBytes(reply, "config_digest").String() != digest ||
					gjson.GetBytes(reply, "principal").String() != tt.key ||
					gjson.GetBytes(reply, "auth").Exists() || gjson.GetBytes(reply, "auth_index").Exists() {
					t.Fatal("unexpected local execution grant")
				}
			}
		})
	}
	t.Run("prefix variants and suffix admission", func(t *testing.T) {
		model = "acp/gemini-4-carbon"
		t.Cleanup(func() { model = public })
		if got := gjson.GetBytes(send(t, "local-plugin-dispatch", "acp-open", "a"), "model").String(); got != "gemini-4-carbon" {
			t.Fatalf("unprefixed target = %q", got)
		}
		if got := gjson.GetBytes(send(t, "local-plugin-dispatch", "acp-allowed", "a"), "error.type").String(); got != "model_forbidden" {
			t.Fatalf("prefixed permission leaked to unprefixed alias: %q", got)
		}
		model = public + "(high)"
		if got := gjson.GetBytes(send(t, "local-plugin-dispatch", "acp-allowed", "a"), "model").String(); got != native {
			t.Fatalf("thinking suffix target = %q", got)
		}
	})
	t.Run("explicit credential alias keeps precedence", func(t *testing.T) {
		credential := &coreauth.Auth{ID: "acp-alias-test-gemini-key", Provider: "gemini", Status: coreauth.StatusActive}
		if _, errRegister := rt.CoreManager().Register(ctx, credential); errRegister != nil {
			t.Fatal(errRegister)
		}
		reg := registry.GetGlobalRegistry()
		reg.RegisterClient(credential.ID, credential.Provider, []*registry.ModelInfo{{ID: native}, {ID: public}})
		t.Cleanup(func() { reg.UnregisterClient(credential.ID) })
		models := send(t, "models", "acp-open", "a")
		if gjson.GetBytes(models, "gemini.#").Int() != 2 || gjson.GetBytes(models, plugin+`.#(id=="`+public+`")`).Exists() {
			t.Fatalf("listing = %s", models)
		}
		if result := send(t, "local-plugin-dispatch", "acp-open", "a"); string(result) != `{"kind":"credential"}` {
			t.Fatal("explicit alias did not fall through to credential dispatch")
		}
		if got := gjson.GetBytes(send(t, "local-plugin-dispatch", "acp-native-only", "a"), "error.type").String(); got != "model_forbidden" {
			t.Fatalf("credential alias permission = %q", got)
		}
	})
	for _, action := range []string{"disable instance", "disable all plugins", "uninstall"} {
		t.Run(action, func(t *testing.T) {
			switch action {
			case "disable instance":
				disabled := item
				enabled := false
				disabled.Enabled = &enabled
				cfg.Plugins.Configs[plugin] = disabled
			case "disable all plugins":
				cfg.Plugins.Enabled = false
			case "uninstall":
				delete(cfg.Plugins.Configs, plugin)
			}
			t.Cleanup(func() {
				cfg.Plugins.Enabled = true
				cfg.Plugins.Configs[plugin] = item
			})
			models := send(t, "models", "acp-open", "a")
			if gjson.GetBytes(models, plugin).Exists() {
				t.Fatalf("withdrawn listing = %s", models)
			}
			reply := send(t, "local-plugin-dispatch", "acp-open", "a")
			if got := gjson.GetBytes(reply, "error.type").String(); got != "model_not_found" {
				t.Fatalf("withdrawn dispatch error = %q", got)
			}
		})
	}
}

func TestLocalExecutorPriorityAndPinnedVersion(t *testing.T) {
	rt, repo := newGetTestRuntime(t)
	ctx := t.Context()
	if _, errKey := repo.CreateAPIKey(ctx, cluster.APIKeyEntryUpdate{APIKey: "priority-test"}); errKey != nil {
		t.Fatal(errKey)
	}
	credential := &coreauth.Auth{ID: "acp-priority-test-regular", Provider: "gemini", Status: coreauth.StatusActive}
	if _, errRegister := rt.CoreManager().Register(ctx, credential); errRegister != nil {
		t.Fatal(errRegister)
	}
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(credential.ID, credential.Provider, []*registry.ModelInfo{{ID: "regular-model"}})
	t.Cleanup(func() { reg.UnregisterClient(credential.ID) })
	rt.SetPluginSyncNodeActive(func(context.Context, string, string) (bool, error) { return true, nil })
	env := dispatch.Env{Runtime: rt, NodeID: "node", ClientCertificateFingerprint: "certificate"}
	const model = "acp/team/same"
	rt.SetLocalExecutorCatalogLoader(func(_ context.Context, _ *config.Config, manifest pluginstore.Manifest, _ []byte) ([]registry.ModelInfo, error) {
		switch manifest.ID {
		case "alpha":
			return []registry.ModelInfo{{ID: "team/same", DisplayName: "Alpha"}}, nil
		case "beta":
			if manifest.Version == "2.0.0" {
				return []registry.ModelInfo{{ID: "team/new", DisplayName: "Beta v2"}}, nil
			}
			if manifest.Version == "3.0.0" {
				return nil, context.DeadlineExceeded
			}
			return []registry.ModelInfo{{ID: "team/same", DisplayName: "Beta"}}, nil
		default:
			t.Errorf("ineligible plugin %s was queried", manifest.ID)
			return nil, fmt.Errorf("unexpected plugin")
		}
	})
	useConfig := func(priority int, version string, betaEnabled bool) {
		t.Helper()
		raw := fmt.Sprintf(`
plugins:
  enabled: true
  configs:
    cpa-acp:
      enabled: true
      priority: 1000
      store: {id: cpa-acp, version: 0.6.31, source-url: "https://downloads.example/registry.json", install: {type: direct}}
    regular:
      enabled: true
      priority: 1000
      store: {id: regular, version: 1.0.0, source-url: "https://downloads.example/registry.json", install: {type: direct}, tags: [Other]}
    invalid-manifest:
      enabled: true
      priority: 1000
      store: {id: invalid-manifest, tags: [ACP]}
    alpha:
      enabled: true
      priority: 100
      store: {id: alpha, version: 1.0.0, source-url: "https://downloads.example/registry.json", install: {type: direct}, tags: [ACP]}
    beta:
      enabled: %t
      priority: %d
      store: {id: beta, version: %s, source-url: "https://downloads.example/registry.json", install: {type: direct}, tags: [acp]}
`, betaEnabled, priority, version)
		var cfg config.Config
		if errDecode := yaml.Unmarshal([]byte(raw), &cfg); errDecode != nil {
			t.Fatal(errDecode)
		}
		rt.SetPluginSyncConfigLoader(func(context.Context) (*config.Config, error) { return &cfg, nil })
	}

	for _, tt := range []struct {
		name, version, winner, label string
		priority                     int
		enabled                      bool
	}{
		{"tied priorities", "1.0.0", "alpha", "Alpha", 100, true},
		{"higher priority", "1.0.0", "beta", "Beta", 101, true},
		{"disabled winner", "1.0.0", "alpha", "Alpha", 101, false},
		{"upgraded winner changes the model", "2.0.0", "alpha", "Alpha", 101, true},
		{"failed upgrade withdraws the old version", "3.0.0", "alpha", "Alpha", 101, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			useConfig(tt.priority, tt.version, tt.enabled)
			list := handleDefault(ctx, env, []string{"GET", `{"type":"models","query":{"key":"priority-test"}}`}).BulkString
			if gjson.GetBytes(list, "gemini.0.id").String() != "regular-model" {
				t.Fatalf("ordinary credential model missing: %s", list)
			}
			result := gjson.GetBytes(list, tt.winner+".0")
			if result.Get("id").String() != model || result.Get("display_name").String() != tt.label {
				t.Fatalf("listing = %s", list)
			}
			count := 0
			gjson.ParseBytes(list).ForEach(func(_, section gjson.Result) bool {
				for _, item := range section.Array() {
					if item.Get("id").String() == model {
						count++
					}
				}
				return true
			})
			if count != 1 {
				t.Fatalf("duplicate listing = %s", list)
			}
			grant := handleDefault(ctx, env, []string{"GET", `{"type":"local-plugin-dispatch","model":"` + model + `","query":{"key":"priority-test"}}`}).BulkString
			if gjson.GetBytes(grant, "plugin").String() != tt.winner || gjson.GetBytes(grant, "model").String() != "team/same" ||
				gjson.GetBytes(grant, "version").String() != "1.0.0" || len(gjson.GetBytes(grant, "config_digest").String()) != 64 {
				t.Fatal("unexpected authorization target or version")
			}
			if newModel := gjson.GetBytes(list, `beta.#(id=="acp/team/new")`).Exists(); newModel != (tt.version == "2.0.0") {
				t.Fatalf("versioned listing = %s", list)
			}
			if tt.version == "3.0.0" {
				withdrawn := handleDefault(ctx, env, []string{"GET", `{"type":"local-plugin-dispatch","model":"acp/team/new","query":{"key":"priority-test"}}`}).BulkString
				if got := gjson.GetBytes(withdrawn, "error.type").String(); got != "model_not_found" {
					t.Fatalf("old version still authorized: %s", withdrawn)
				}
			}
		})
	}
}
