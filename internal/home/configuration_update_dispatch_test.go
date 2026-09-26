package home

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
	"github.com/router-for-me/CLIProxyAPIHome/internal/watcher/synthesizer"
	"github.com/tidwall/gjson"
)

func TestCodexConfigurationUpdateCapabilityFollowsSelectedHomeCredential(t *testing.T) {
	const model = "opaque-home-model"
	cfg := &config.Config{CodexKey: []config.CodexKey{
		{APIKey: "enabled-key", Models: []config.CodexModel{{Name: model, SupportConfigurationUpdate: true}}},
		{APIKey: "disabled-key", Models: []config.CodexModel{{Name: model}}},
	}}
	auths, errSynthesize := synthesizer.NewConfigSynthesizer().Synthesize(&synthesizer.SynthesisContext{
		Config: cfg, Now: time.Unix(1, 0), IDGenerator: synthesizer.NewStableIDGenerator(),
	})
	if errSynthesize != nil {
		t.Fatal(errSynthesize)
	}
	manager := coreauth.NewManager(nil, nil, nil)
	adapter := &modelChannelDispatchTestAdapter{allowedModelIDs: []string{model}}
	runtime := &Runtime{coreManager: manager, clusterAdapter: adapter, cfg: cfg}
	byKey := make(map[string]string)
	for _, auth := range auths {
		if auth == nil || auth.Provider != "codex" {
			continue
		}
		byKey[auth.Attributes["api_key"]] = auth.ID
		if _, errRegister := manager.Register(t.Context(), auth); errRegister != nil {
			t.Fatalf("Register() error = %v", errRegister)
		}
		runtime.registerModelsForAuth(auth)
		manager.RefreshSchedulerEntry(auth.ID)
		if info := registry.GetGlobalRegistry().GetModelInfoForClient(auth.ID, model); info == nil {
			t.Fatalf("registered credential %s lost model %s; metadata=%+v", auth.ID, model, auth.Metadata)
		}
		authID := auth.ID
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(authID) })
	}
	if len(byKey) != 2 {
		t.Fatalf("synthesized credentials = %v", byKey)
	}
	for _, tc := range []struct {
		key     string
		support bool
	}{
		{key: "enabled-key", support: true},
		{key: "disabled-key"},
	} {
		t.Run(tc.key, func(t *testing.T) {
			adapter.allowedAuthIDs = []string{byKey[tc.key]}
			result, errDispatch := runtime.DispatchForAPIKey(context.Background(), model, nil, "client-key")
			if errDispatch != nil || result == nil || result.AuthID != byKey[tc.key] || result.ModelInfo == nil {
				t.Fatalf("DispatchForAPIKey() = (%+v, %v), want %s", result, errDispatch, byKey[tc.key])
			}
			if result.ModelInfo.SupportConfigurationUpdate != tc.support {
				t.Fatalf("selected model capability = %+v, want %t", result.ModelInfo, tc.support)
			}
			payload, errMarshal := json.Marshal(result.ModelInfo)
			if errMarshal != nil {
				t.Fatal(errMarshal)
			}
			if field := gjson.GetBytes(payload, "support_configuration_update"); !field.Exists() || field.Bool() != tc.support {
				t.Fatalf("dispatch wire missing explicit capability: %s", payload)
			}
		})
	}
}
