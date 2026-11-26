package home

import (
	"testing"

	sdkpluginhost "github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginhost"
	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
)

func TestOAuthModelConfigSourcesAndReload(t *testing.T) {
	for _, source := range []string{"built-in", "plugin"} {
		for _, authKind := range []string{"oauth", "apikey"} {
			t.Run(source+"/"+authKind, func(t *testing.T) {
				base := []*ModelInfo{{ID: "upstream", Name: "upstream", ContextLength: 1000}, {ID: "hidden", ContextLength: 2000}}
				if source == "plugin" {
					base = pluginModelsToHomeModels([]sdkpluginhost.ModelInfo{{ID: "upstream", Name: "upstream", ContextLength: 1000}, {ID: "hidden", ContextLength: 2000}})
				}
				cfg := &config.Config{
					OAuthSettings:   map[string][]config.OAuthModelSetting{"codex": {{Name: "upstream", MaxContextLength: 524288}, {Name: "hidden", MaxContextLength: 262144}}},
					OAuthModelAlias: map[string][]config.OAuthModelAlias{"codex": {{Name: "upstream", Alias: "public"}}},
				}
				for _, enabled := range []bool{true, false, true} {
					if enabled {
						cfg.OAuthSettings["codex"][0].MaxContextLength = 524288
					} else {
						cfg.OAuthSettings["codex"][0].MaxContextLength = 0
					}
					models, candidates := applyOAuthModelConfig(cfg, "codex", authKind, base, []string{"hidden"})
					wantID, wantContext := "upstream", 1000
					if authKind == "oauth" {
						wantID = "public"
						if enabled {
							wantContext = 524288
						}
					}
					if len(models) != 1 || models[0].ID != wantID || models[0].ContextLength != wantContext {
						t.Fatalf("catalog override/alias/exclusion order changed: %+v", models)
					}
					if len(candidates) != 2 || candidates[0].ID != "upstream" || candidates[0].ContextLength != wantContext {
						t.Fatal("upstream candidates do not share the catalog capability override")
					}
					runtime := &Runtime{cfg: cfg}
					auth := &coreauth.Auth{ID: "oauth-source-test", Provider: "codex", Prefix: "team"}
					runtime.registerResolvedModelsForAuth(auth, "codex", applyModelPrefixes(models, auth.Prefix, true), candidates)
					t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
					info := dispatchModelInfoForAuth(auth.ID, "upstream", "team/"+wantID)
					if info == nil || info.ContextLength != wantContext {
						t.Fatal("prefixed dispatch lost the configured model context")
					}
					if base[0].ContextLength != 1000 || base[0].ID != "upstream" {
						t.Fatal("config application mutated the reusable source catalog")
					}
				}
			})
		}
	}
}

func TestOAuthSettingsAntigravityDiscoveredCatalog(t *testing.T) {
	base := registry.GetAntigravityModels()
	if len(base) == 0 {
		t.Fatal("missing Antigravity catalog")
	}
	model := base[0].ID
	cfg := &config.Config{
		OAuthSettings:   map[string][]config.OAuthModelSetting{"antigravity": {{Name: model, MaxContextLength: 524288}}},
		OAuthModelAlias: map[string][]config.OAuthModelAlias{"antigravity": {{Name: model, Alias: "public"}}},
	}
	cfg.ForceModelPrefix = true
	runtime := &Runtime{cfg: cfg}
	auth := &coreauth.Auth{ID: "oauth-fetched-settings", Provider: "antigravity", Prefix: "team"}
	hints := antigravityModelCapabilityHints{ModelIDs: map[string]struct{}{normalizeAntigravityFetchedModelID(model): {}}}
	models, candidates := runtime.antigravityModelsAndCandidatesForHintsWithConfig(auth, hints, cfg)
	if len(models) != 1 || models[0].ID != "team/public" || models[0].ContextLength != 524288 || len(candidates) != 1 || candidates[0].ContextLength != 524288 {
		t.Fatal("discovered catalog bypassed OAuth settings before alias/prefix mapping")
	}
	delete(cfg.OAuthSettings, "antigravity")
	models, _ = runtime.antigravityModelsAndCandidatesForHintsWithConfig(auth, hints, cfg)
	if len(models) != 1 || models[0].ContextLength != base[0].ContextLength {
		t.Fatal("removing OAuth settings retained an old override")
	}
	hints.ModelIDs = map[string]struct{}{}
	models, candidates = runtime.antigravityModelsAndCandidatesForHintsWithConfig(auth, hints, cfg)
	if len(models) != 0 || len(candidates) != 0 {
		t.Fatal("OAuth settings recreated an authoritative empty discovered catalog")
	}
}

func TestOAuthModelConfigCopilotDisplayName(t *testing.T) {
	cfg := &config.Config{OAuthModelAlias: map[string][]config.OAuthModelAlias{
		"copilot": {{Name: "copilot/custom", Alias: "alias", DisplayName: "Custom Alpha"}},
	}}
	for _, tc := range []struct {
		name     string
		nativeID string
		model    ModelInfo
		want     string
	}{
		{name: "catalog name", nativeID: "alpha", model: ModelInfo{ID: "alpha", DisplayName: "Alpha"}, want: "Alpha (Copilot)"},
		{name: "existing suffix", nativeID: "beta", model: ModelInfo{ID: "beta", DisplayName: "Beta (Copilot)"}, want: "Beta (Copilot)"},
		{name: "native ID fallback", nativeID: "alpha", model: ModelInfo{ID: "alpha"}, want: "alpha (Copilot)"},
		{name: "alias display name", nativeID: "custom", model: ModelInfo{ID: "custom"}, want: "Custom Alpha"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := tc.model
			setCopilotModelDisplayName(&model, tc.nativeID)
			applyModelNamespace(&model, "copilot", tc.nativeID, model.ID)
			models, _ := applyOAuthModelConfig(cfg, "copilot", "oauth", []*ModelInfo{&model}, nil)
			if len(models) != 1 || models[0].DisplayName != tc.want {
				t.Fatalf("published models = %+v, want one model named %q", models, tc.want)
			}
		})
	}
}
