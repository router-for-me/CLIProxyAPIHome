package home

import (
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
)

func TestRegisterModelsCredentialExclusionsOverrideGlobal(t *testing.T) {
	catalog := registry.GetKimiModels()
	if len(catalog) < 2 {
		t.Fatalf("kimi catalog has %d models, want at least 2", len(catalog))
	}
	globalModel := catalog[0].ID
	credentialModel := catalog[1].ID
	runtime := &Runtime{cfg: &config.Config{OAuthExcludedModels: map[string][]string{"kimi-ai": {globalModel}}}}

	registered := func(auth *coreauth.Auth) map[string]bool {
		t.Helper()
		runtime.registerModelsForAuth(auth)
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
		out := make(map[string]bool)
		for _, model := range registry.GetGlobalRegistry().GetModelsForClient(auth.ID) {
			out[model.ID] = true
		}
		return out
	}

	inheritAuth := &coreauth.Auth{
		ID: "kimi-exclusion-inherit", Provider: "kimi-ai", Status: coreauth.StatusActive,
		Attributes: map[string]string{"auth_kind": "oauth"},
	}
	inherited := registered(inheritAuth)
	if inherited[globalModel] || !inherited[credentialModel] {
		t.Fatalf("inherited models = %v, want %q excluded by global config and %q kept", inherited, globalModel, credentialModel)
	}

	// A later global config change must apply to the same credential on the next registration.
	runtime.cfg = &config.Config{OAuthExcludedModels: map[string][]string{"kimi-ai": {credentialModel}}}
	reloaded := registered(inheritAuth)
	if !reloaded[globalModel] || reloaded[credentialModel] {
		t.Fatalf("reloaded models = %v, want %q kept and %q excluded by the updated global config", reloaded, globalModel, credentialModel)
	}
	runtime.cfg = &config.Config{OAuthExcludedModels: map[string][]string{"kimi-ai": {globalModel}}}

	overridden := registered(&coreauth.Auth{
		ID: "kimi-exclusion-override", Provider: "kimi-ai", Status: coreauth.StatusActive,
		Attributes: map[string]string{"auth_kind": "oauth", "excluded_models": credentialModel},
	})
	if !overridden[globalModel] || overridden[credentialModel] {
		t.Fatalf("overridden models = %v, want %q kept and %q excluded by credential config", overridden, globalModel, credentialModel)
	}
}
