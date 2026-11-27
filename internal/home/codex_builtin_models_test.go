package home

import (
	"encoding/json"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
	"github.com/router-for-me/CLIProxyAPIHome/internal/watcher/synthesizer"
)

func TestCodexImageAliasSurvivesSynthesisAndRegistration(t *testing.T) {
	for _, id := range []string{
		"gpt-image-2.5", "gpt-image-2.5-flare", "gpt-image-2.5-sunburst",
		"gpt-image-2.6", "gpt-image-2.6-flare", "gpt-image-2.6-sunburst",
	} {
		t.Run(id, func(t *testing.T) {
			const upstream = "vendor/upstream-image-25"
			cfg := &config.Config{CodexKey: []config.CodexKey{{APIKey: "test-codex-image-key", Models: []config.CodexModel{{
				Name: upstream, Alias: id, DisplayName: "Custom Image", ForceMapping: true,
			}}}}}
			checkModel := func(models []*ModelInfo) {
				t.Helper()
				found := 0
				for _, model := range models {
					if model.ID != id {
						continue
					}
					found++
					if model.Name != upstream || model.DisplayName != "Custom Image" || model.ConfigDisplayName != "Custom Image" || !model.UserDefined || !model.ForceMapping {
						t.Errorf("configured image mapping changed: %+v", model)
					}
				}
				if found != 1 {
					t.Fatalf("got %d entries for %s, want 1", found, id)
				}
			}
			checkModel(buildCodexConfigModels(&cfg.CodexKey[0]))
			auths, errSynthesize := synthesizer.NewConfigSynthesizer().Synthesize(&synthesizer.SynthesisContext{
				Config: cfg, Now: time.Unix(1, 0), IDGenerator: synthesizer.NewStableIDGenerator(),
			})
			if errSynthesize != nil || len(auths) != 1 {
				t.Fatalf("Synthesize() = (%d auths, %v), want 1 auth", len(auths), errSynthesize)
			}
			// Match database JSON serialization, without retaining a runtime config fallback.
			data, errMarshal := json.Marshal(auths[0])
			if errMarshal != nil {
				t.Fatal(errMarshal)
			}
			var restored coreauth.Auth
			if errUnmarshal := json.Unmarshal(data, &restored); errUnmarshal != nil {
				t.Fatal(errUnmarshal)
			}
			checkModel(modelInfosFromAuthMetadata(&restored, homeConfigModelsMetadataKey))
			manager := coreauth.NewManager(nil, nil, nil)
			manager.SetConfig(&config.Config{})
			if _, errRegister := manager.Register(t.Context(), &restored); errRegister != nil {
				t.Fatal(errRegister)
			}
			runtime := &Runtime{coreManager: manager, cfg: &config.Config{}}
			t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(restored.ID) })
			runtime.registerModelsForAuth(&restored)
			manager.RefreshSchedulerEntry(restored.ID)
			checkModel(registry.GetGlobalRegistry().GetModelsForClient(restored.ID))
			if got := manager.ResolveExecutionModel(&restored, id); got != upstream {
				t.Errorf("ResolveExecutionModel() = %q, want %q", got, upstream)
			}
			decision, errDispatch := manager.Dispatch(t.Context(), []string{"codex"}, id, coreauth.Options{})
			if errDispatch != nil {
				t.Fatal(errDispatch)
			}
			if decision == nil || decision.UpstreamModel != upstream || !decision.ForceMapping || decision.OriginalAlias != id {
				t.Fatalf("Dispatch() lost configured mapping: %+v", decision)
			}
		})
	}
}

func TestRegisterCodexStoredModelsRefreshesBuiltins(t *testing.T) {
	for _, tc := range []struct {
		name     string
		override bool
		excluded string
		absent   map[string]bool
	}{
		{name: "legacy snapshot"},
		{name: "configured override", override: true},
		{name: "exact exclusion", excluded: "gpt-image-2.5", absent: map[string]bool{"gpt-image-2.5": true}},
		{name: "wildcard exclusion with override", override: true, excluded: "gpt-image-2.5*", absent: map[string]bool{
			"gpt-image-2.5": true, "gpt-image-2.5-flare": true, "gpt-image-2.5-sunburst": true,
		}},
		{name: "wildcard exclusion of 2.6", excluded: "gpt-image-2.6*", absent: map[string]bool{
			"gpt-image-2.6": true, "gpt-image-2.6-flare": true, "gpt-image-2.6-sunburst": true,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var auth coreauth.Auth
			// Old persisted credentials contain Image 2, not the new built-ins.
			if errUnmarshal := json.Unmarshal([]byte(`{
				"id":"codex-old-image-snapshot","provider":"codex","status":"active",
				"attributes":{"api_key":"test-codex-image-key","auth_kind":"apikey"},
				"metadata":{"home_config_models":[
					{"id":"gpt-image-2","version":"gpt-image-2","user_defined":false}
				]}
			}`), &auth); errUnmarshal != nil {
				t.Fatal(errUnmarshal)
			}
			if tc.override {
				stored := auth.Metadata[homeConfigModelsMetadataKey].([]any)
				auth.Metadata[homeConfigModelsMetadataKey] = append(stored, map[string]any{
					"id": "gpt-image-2.5", "name": "vendor/upstream-image-25", "user_defined": true,
					"display_name": "Custom Image", "config_display_name": "Custom Image", "force_mapping": true,
				})
			}
			auth.Attributes["excluded_models"] = tc.excluded
			before, errMarshal := json.Marshal(auth.Metadata)
			if errMarshal != nil {
				t.Fatal(errMarshal)
			}
			runtime := &Runtime{cfg: &config.Config{}}
			t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
			for range 2 {
				runtime.registerModelsForAuth(&auth)
				models := registry.GetGlobalRegistry().GetModelsForClient(auth.ID)
				if len(models) != 7-len(tc.absent) {
					t.Fatalf("got %d registered models, want %d", len(models), 7-len(tc.absent))
				}
				for _, id := range []string{
					"gpt-image-2", "gpt-image-2.5", "gpt-image-2.5-flare", "gpt-image-2.5-sunburst",
					"gpt-image-2.6", "gpt-image-2.6-flare", "gpt-image-2.6-sunburst",
				} {
					info := registry.GetGlobalRegistry().GetModelInfoForClient(auth.ID, id)
					if (info == nil) != tc.absent[id] {
						t.Errorf("%s registered = %t, want %t", id, info != nil, !tc.absent[id])
					}
					if info != nil && id == "gpt-image-2.5" && tc.override {
						if info.Name != "vendor/upstream-image-25" || info.DisplayName != "Custom Image" || !info.UserDefined || !info.ForceMapping {
							t.Errorf("stored override changed: %+v", info)
						}
					}
				}
			}
			after, errMarshalAfter := json.Marshal(auth.Metadata)
			if errMarshalAfter != nil {
				t.Fatal(errMarshalAfter)
			}
			if string(before) != string(after) {
				t.Fatal("model registration changed persisted metadata")
			}
		})
	}
}
