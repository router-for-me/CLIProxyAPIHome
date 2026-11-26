package management

import (
	"encoding/json"
	"net/http"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/home"
	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
)

func TestConfigV8OAuthAliasContextReload(t *testing.T) {
	repo, request := newConfigV8AuthIndexRouter(t)
	var source *registry.ModelInfo
	for _, model := range registry.GetCodexProModels() {
		if model != nil && model.ContextLength > 0 {
			source = model
			break
		}
	}
	if source == nil {
		t.Fatal("missing Codex model with a context window")
	}
	auth := &coreauth.Auth{ID: "oauth-context-lifecycle", Index: "oauth-context-lifecycle", Provider: "codex", Prefix: "team", Status: coreauth.StatusActive, Attributes: map[string]string{"auth_kind": "oauth", "plan_type": "pro"}}
	if _, errUpsert := repo.UpsertAuth(t.Context(), auth, "test"); errUpsert != nil {
		t.Fatal(errUpsert)
	}
	aliases, errMarshal := json.Marshal([]config.OAuthModelAlias{{Name: source.ID, Alias: "context-a", Fork: true}, {Name: source.ID, Alias: "context-b"}})
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	request("PUT", "/v8/management/config/oauth/model-alias/codex", string(aliases), http.StatusOK)
	cfg, _, errConfig := repo.LoadConfigAsRuntimeConfig(t.Context())
	if errConfig != nil {
		t.Fatal(errConfig)
	}
	runtime, errRuntime := home.NewRuntime(cfg)
	if errRuntime != nil {
		t.Fatal(errRuntime)
	}
	t.Cleanup(runtime.Stop)
	runtime.SetClusterAdapter(cluster.NewRuntimeAdapter(repo, "oauth-context-test"))
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	for _, configured := range []bool{true, false, true} {
		if configured {
			settings, errSettings := json.Marshal([]config.OAuthModelSetting{
				{Name: source.ID, Alias: "context-a", MaxContextLength: 524288},
				{Name: source.ID, Alias: "context-b", MaxContextLength: 262144},
				{Name: source.ID, MaxContextLength: 131072},
			})
			if errSettings != nil {
				t.Fatal(errSettings)
			}
			request("PUT", "/v8/management/config/oauth/settings/codex", string(settings), http.StatusOK)
		} else {
			request("DELETE", "/v8/management/config/oauth/settings/codex", "", http.StatusOK)
		}
		next, _, errNext := repo.LoadConfigAsRuntimeConfig(t.Context())
		if errNext != nil {
			t.Fatal(errNext)
		}
		if errApply := runtime.ApplyConfigFromCluster(t.Context(), next); errApply != nil {
			t.Fatal(errApply)
		}
		for _, route := range []string{"context-a", "context-b"} {
			want := source.ContextLength
			if configured {
				want = 524288
				if route == "context-b" {
					want = 262144
				}
			}
			route = "team/" + route
			info := registry.GetGlobalRegistry().GetModelInfoForClient(auth.ID, route)
			if info == nil || info.ContextLength != want {
				t.Fatalf("reloaded directory %s = %+v, want context %d", route, info, want)
			}
			decision, errDispatch := runtime.DispatchForAPIKey(t.Context(), route, nil, "")
			if errDispatch != nil || decision == nil || decision.Model != source.ID || decision.ModelInfo == nil || decision.ModelInfo.ID != source.ID || decision.ModelInfo.ContextLength != want {
				t.Fatalf("reloaded dispatch %s = %+v, want %s with context %d, error = %v", route, decision, source.ID, want, errDispatch)
			}
		}
	}
}
