package registry

import (
	"reflect"
	"sync"
	"testing"
)

func TestGetAvailableModelDefinitionsIncludesBillingProviders(t *testing.T) {
	modelRegistry := &ModelRegistry{
		models:           make(map[string]*ModelRegistration),
		clientModels:     make(map[string][]string),
		clientModelInfos: make(map[string]map[string]*ModelInfo),
		clientProviders:  make(map[string]string),
		mutex:            &sync.RWMutex{},
	}
	model := &ModelInfo{ID: "gpt-5.5", Type: "openai", OwnedBy: "openai"}

	modelRegistry.RegisterClient("codex-client", "codex", []*ModelInfo{model})
	modelRegistry.RegisterClient("compat-client", "OpenRouter", []*ModelInfo{model})

	models := modelRegistry.GetAvailableModelDefinitions()
	if len(models) != 1 {
		t.Fatalf("GetAvailableModelDefinitions() returned %d models, want 1", len(models))
	}
	wantProviders := []string{"codex", "openrouter"}
	if !reflect.DeepEqual(models[0].Providers, wantProviders) {
		t.Fatalf("providers = %#v, want %#v", models[0].Providers, wantProviders)
	}
	if models[0].Type != "openai" {
		t.Fatalf("type = %q, want openai", models[0].Type)
	}

	modelRegistry.UnregisterClient("compat-client")
	models = modelRegistry.GetAvailableModelDefinitions()
	if len(models) != 1 {
		t.Fatalf("GetAvailableModelDefinitions() after unregister returned %d models, want 1", len(models))
	}
	wantProviders = []string{"codex"}
	if !reflect.DeepEqual(models[0].Providers, wantProviders) {
		t.Fatalf("providers after unregister = %#v, want %#v", models[0].Providers, wantProviders)
	}
}

func TestStaticModelDefinitionsExposeCanonicalBillingProviders(t *testing.T) {
	tests := []struct {
		channel  string
		provider string
	}{
		{channel: "claude", provider: "claude"},
		{channel: "gemini", provider: "gemini"},
		{channel: "gemini-interactions", provider: "gemini-interactions"},
		{channel: "vertex", provider: "vertex"},
		{channel: "aistudio", provider: "aistudio"},
		{channel: "codex-free", provider: "codex"},
		{channel: "codex-pro", provider: "codex"},
		{channel: "kimi", provider: "kimi"},
		{channel: "antigravity", provider: "antigravity"},
		{channel: "xai", provider: "xai"},
		{channel: "x-ai", provider: "xai"},
		{channel: "grok", provider: "xai"},
	}

	for _, test := range tests {
		t.Run(test.channel, func(t *testing.T) {
			models := GetStaticModelDefinitionsByChannel(test.channel)
			if len(models) == 0 {
				t.Fatalf("GetStaticModelDefinitionsByChannel(%q) returned no models", test.channel)
			}
			for _, model := range models {
				if model == nil {
					continue
				}
				wantProviders := []string{test.provider}
				if !reflect.DeepEqual(model.Providers, wantProviders) {
					t.Fatalf("model %q providers = %#v, want %#v", model.ID, model.Providers, wantProviders)
				}
			}
		})
	}
}

func TestDetectChangedProvidersIncludesCPAProviderAliases(t *testing.T) {
	oldData := &staticModelsJSON{
		Gemini:   []*ModelInfo{{ID: "gemini-old"}},
		AIStudio: []*ModelInfo{{ID: "aistudio-old"}},
	}
	newData := &staticModelsJSON{
		Gemini:   []*ModelInfo{{ID: "gemini-new"}},
		AIStudio: []*ModelInfo{{ID: "aistudio-new"}},
	}

	wantProviders := []string{"gemini", "gemini-interactions", "aistudio"}
	if providers := detectChangedProviders(oldData, newData); !reflect.DeepEqual(providers, wantProviders) {
		t.Fatalf("detectChangedProviders() = %#v, want %#v", providers, wantProviders)
	}
}

func TestModelRegistrationProvidersNormalizesAndFiltersCounts(t *testing.T) {
	registration := &ModelRegistration{Providers: map[string]int{
		"Codex":        2,
		" codex ":      1,
		" OpenRouter ": 1,
		"zero":         0,
		"negative":     -1,
		"":             1,
	}}

	wantProviders := []string{"codex", "openrouter"}
	if providers := modelRegistrationProviders(registration); !reflect.DeepEqual(providers, wantProviders) {
		t.Fatalf("modelRegistrationProviders() = %#v, want %#v", providers, wantProviders)
	}
}

func TestAllStaticModelDefinitionsUseCanonicalProviders(t *testing.T) {
	modelsByChannel := GetAllStaticModelDefinitions()
	tests := []struct {
		channel  string
		provider string
	}{
		{channel: "gemini-interactions", provider: "gemini-interactions"},
		{channel: "aistudio", provider: "aistudio"},
		{channel: "codex-free", provider: "codex"},
		{channel: "codex-team", provider: "codex"},
		{channel: "codex-plus", provider: "codex"},
		{channel: "codex-pro", provider: "codex"},
	}

	for _, test := range tests {
		models := modelsByChannel[test.channel]
		if len(models) == 0 {
			t.Fatalf("channel %q returned no models", test.channel)
		}
		for _, model := range models {
			if model == nil {
				continue
			}
			wantProviders := []string{test.provider}
			if !reflect.DeepEqual(model.Providers, wantProviders) {
				t.Fatalf("channel %q model %q providers = %#v, want %#v", test.channel, model.ID, model.Providers, wantProviders)
			}
		}
	}
}
