package home

import (
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
)

func TestRegisterModelsForAuthSupportsCPAProviderIdentifiers(t *testing.T) {
	runtime := &Runtime{cfg: &config.Config{}}
	tests := []string{"aistudio", "gemini-interactions"}

	for _, provider := range tests {
		t.Run(provider, func(t *testing.T) {
			authID := "provider-registration-" + provider
			defer registry.GetGlobalRegistry().UnregisterClient(authID)

			runtime.registerModelsForAuth(&coreauth.Auth{ID: authID, Provider: provider})
			models := registry.GetGlobalRegistry().GetModelsForClient(authID)
			if len(models) == 0 {
				t.Fatalf("provider %q registered no models", provider)
			}

			modelID := models[0].ID
			foundProvider := false
			for _, model := range registry.GetGlobalRegistry().GetAvailableModelDefinitions() {
				if model == nil || model.ID != modelID {
					continue
				}
				for _, registeredProvider := range model.Providers {
					if registeredProvider == provider {
						foundProvider = true
						break
					}
				}
			}
			if !foundProvider {
				t.Fatalf("model %q did not expose provider %q", modelID, provider)
			}
		})
	}
}
