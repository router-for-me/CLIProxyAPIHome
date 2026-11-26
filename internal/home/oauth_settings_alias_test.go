package home

import (
	"encoding/json"
	"strings"
	"testing"

	sdkpluginhost "github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginhost"
	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
)

func TestOAuthSettingsAliasCatalogAndDispatch(t *testing.T) {
	for _, source := range []string{"built-in", "plugin"} {
		for _, prefix := range []string{"", "team"} {
			t.Run(source+"/"+prefix, func(t *testing.T) {
				base := []*ModelInfo{{ID: "upstream", Name: "models/upstream", ContextLength: 1000}, {ID: "hidden", ContextLength: 2000}}
				if source == "plugin" {
					base = pluginModelsToHomeModels([]sdkpluginhost.ModelInfo{{ID: "upstream", Name: "models/upstream", ContextLength: 1000}, {ID: "hidden", ContextLength: 2000}})
				}
				aliases := []config.OAuthModelAlias{{Name: "upstream", Alias: "public-a", Fork: true}, {Name: "upstream", Alias: "public-b"}}
				cfg := &config.Config{OAuthModelAlias: map[string][]config.OAuthModelAlias{"codex": aliases}}
				manager := coreauth.NewManager(nil, &coreauth.FillFirstSelector{}, nil)
				manager.SetConfig(cfg)
				manager.SetOAuthModelAlias(cfg.OAuthModelAlias)
				auth := &coreauth.Auth{ID: "oauth-alias-context", Provider: "codex", Prefix: prefix, Status: coreauth.StatusActive}
				if _, errRegister := manager.Register(t.Context(), auth); errRegister != nil {
					t.Fatal(errRegister)
				}
				runtime := &Runtime{cfg: cfg, coreManager: manager}
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
				for _, phase := range []struct {
					name     string
					settings []config.OAuthModelSetting
					want     map[string]int
				}{
					{
						name: "alias-overrides-general",
						settings: []config.OAuthModelSetting{
							{Name: "upstream", Alias: "public-a", MaxContextLength: 524288},
							{Name: "upstream", Alias: "public-b", MaxContextLength: 262144},
							{Name: "upstream", MaxContextLength: 131072},
						},
						want: map[string]int{"upstream": 131072, "public-a": 524288, "public-b": 262144},
					},
					{
						name: "removed-one-alias-override",
						settings: []config.OAuthModelSetting{
							{Name: "upstream", Alias: "public-b", MaxContextLength: 262144},
							{Name: "upstream", MaxContextLength: 131072},
						},
						want: map[string]int{"upstream": 131072, "public-a": 131072, "public-b": 262144},
					},
					{name: "removed-all-settings", want: map[string]int{"upstream": 1000, "public-a": 1000, "public-b": 1000}},
					{
						name:     "alias-only-override",
						settings: []config.OAuthModelSetting{{Name: "upstream", Alias: "public-a", MaxContextLength: 524288}},
						want:     map[string]int{"upstream": 1000, "public-a": 524288, "public-b": 1000},
					},
				} {
					t.Run(phase.name, func(t *testing.T) {
						cfg.OAuthSettings = map[string][]config.OAuthModelSetting{"codex": phase.settings}
						models, candidates := applyOAuthModelConfig(cfg, "codex", "oauth", base, []string{"hidden"})
						runtime.registerResolvedModelsForAuth(auth, "codex", applyModelPrefixes(models, prefix, true), candidates)
						manager.RefreshSchedulerEntry(auth.ID)
						if len(models) != 3 || len(candidates) != 2 || candidates[0].ContextLength != phase.want["upstream"] {
							t.Fatalf("catalog or upstream candidate settings changed: models=%+v candidates=%+v", models, candidates)
						}
						for id, wantContext := range phase.want {
							route := id
							if prefix != "" {
								route = prefix + "/" + route
							}
							info := registry.GetGlobalRegistry().GetModelInfoForClient(auth.ID, route)
							if info == nil || info.ContextLength != wantContext {
								t.Fatalf("catalog %s = %+v, want context %d", route, info, wantContext)
							}
							decision, errDispatch := runtime.dispatchWithOptions(t.Context(), route, coreauth.Options{}, DispatchConcurrencyContext{})
							if errDispatch != nil || decision == nil || decision.Model != "upstream" || decision.ModelInfo == nil || decision.ModelInfo.ID != "upstream" || decision.ModelInfo.ContextLength != wantContext {
								t.Fatalf("dispatch %s = %+v, want upstream with context %d, error = %v", route, decision, wantContext, errDispatch)
							}
							thinking := dispatchModelInfoForAuth(auth.ID, "upstream(high)", route+"(high)")
							if thinking == nil || thinking.ContextLength != wantContext || thinking.ID != "upstream" {
								t.Fatalf("thinking suffix lost alias settings: %+v", thinking)
							}
							encoded, errMarshal := json.Marshal(info)
							if errMarshal != nil {
								t.Fatal(errMarshal)
							}
							if strings.Contains(string(encoded), "metadata_model_id") || strings.Contains(string(encoded), "MetadataModelID") {
								t.Fatal("internal upstream identity leaked into the model directory")
							}
						}
						if base[0].ID != "upstream" || base[0].Name != "models/upstream" || base[0].ContextLength != 1000 {
							t.Fatal("alias override mutated the reusable upstream catalog")
						}
					})
				}
			})
		}
	}
}

func TestOAuthSettingsDiscoveredAliasIdentity(t *testing.T) {
	base := registry.GetAntigravityModels()
	if len(base) == 0 {
		t.Fatal("missing Antigravity catalog")
	}
	upstream := base[0].ID
	cfg := &config.Config{
		OAuthModelAlias: map[string][]config.OAuthModelAlias{"antigravity": {{Name: upstream, Alias: "discovered-a", Fork: true}, {Name: upstream, Alias: "discovered-b"}}},
		OAuthSettings: map[string][]config.OAuthModelSetting{"antigravity": {
			{Name: upstream, Alias: "discovered-a", MaxContextLength: 524288},
			{Name: upstream, MaxContextLength: 131072},
		}},
	}
	cfg.ForceModelPrefix = true
	runtime := &Runtime{cfg: cfg}
	auth := &coreauth.Auth{ID: "oauth-discovered-alias-context", Provider: "antigravity", Prefix: "team"}
	hints := antigravityModelCapabilityHints{ModelIDs: map[string]struct{}{normalizeAntigravityFetchedModelID(upstream): {}}}
	models, candidates := runtime.antigravityModelsAndCandidatesForHintsWithConfig(auth, hints, cfg)
	runtime.registerResolvedModelsForAuth(auth, "antigravity", models, candidates)
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	for route, want := range map[string]int{"team/discovered-a": 524288, "team/discovered-b": 131072} {
		info := registry.GetGlobalRegistry().GetModelInfoForClient(auth.ID, route)
		dispatch := dispatchModelInfoForAuth(auth.ID, upstream, route)
		if info == nil || info.ContextLength != want || dispatch == nil || dispatch.ContextLength != want {
			t.Fatalf("discovered route %s lost alias settings: catalog=%+v dispatch=%+v", route, info, dispatch)
		}
	}
	if len(candidates) != 1 || candidates[0].ContextLength != 131072 {
		t.Fatalf("alias settings changed upstream candidates: %+v", candidates)
	}
}

func TestOAuthSettingsAliasAPIKeyIsolation(t *testing.T) {
	cfg := &config.Config{
		OAuthSettings:   map[string][]config.OAuthModelSetting{"codex": {{Name: "upstream", Alias: "public", MaxContextLength: 524288}}},
		OAuthModelAlias: map[string][]config.OAuthModelAlias{"codex": {{Name: "upstream", Alias: "public"}}},
	}
	base := []*ModelInfo{{ID: "upstream", ContextLength: 1000}, {ID: "public", ContextLength: 2000}}
	models, candidates := applyOAuthModelConfig(cfg, "codex", "apikey", base, nil)
	if len(models) != 2 || models[0].ID != "upstream" || models[0].ContextLength != 1000 || models[1].ContextLength != 2000 || candidates[1].ContextLength != 2000 {
		t.Fatalf("OAuth aliases or settings changed API-key models: %+v", models)
	}
	registry.GetGlobalRegistry().RegisterClient("apikey-pool-context", "codex", models)
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient("apikey-pool-context") })
	info := dispatchModelInfoForAuth("apikey-pool-context", "upstream", "public")
	if info == nil || info.ContextLength != 1000 || info.ID != "upstream" {
		t.Fatalf("API-key dispatch stopped preferring selected upstream capabilities: %+v", info)
	}
}
