package home

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
	"github.com/router-for-me/CLIProxyAPIHome/internal/watcher/synthesizer"
)

func TestV8ModelCapabilitiesSurviveCredentialAndDispatch(t *testing.T) {
	thinking := &registry.ThinkingSupport{Levels: []string{" HIGH ", "none", "AUTO", "high"}}
	cfg := &config.Config{
		CodexKey:            []config.CodexKey{{APIKey: "codex", BaseURL: "https://example.test", Models: []config.CodexModel{{Name: "v8-upstream", Alias: "v8-alias", MaxContextLength: 32768, Thinking: thinking, IsCompat: true}}}},
		ClaudeKey:           []config.ClaudeKey{{APIKey: "claude", Models: []config.ClaudeModel{{Name: "v8-upstream", Alias: "v8-alias", MaxContextLength: 32768, Thinking: thinking, IsCompat: true}}}},
		GeminiKey:           []config.GeminiKey{{APIKey: "gemini", Models: []config.GeminiModel{{Name: "v8-upstream", Alias: "v8-alias", MaxContextLength: 32768, Thinking: thinking, IsCompat: true}}}},
		OpenAICompatibility: []config.OpenAICompatibility{{Name: "v8-compat", BaseURL: "https://example.test", APIKeyEntries: []config.OpenAICompatibilityAPIKey{{APIKey: "compat-key"}}, Models: []config.OpenAICompatibilityModel{{Name: "v8-upstream", Alias: "v8-alias", MaxContextLength: 32768, Thinking: thinking, IsCompat: true}}}},
	}
	auths, err := synthesizer.NewConfigSynthesizer().Synthesize(&synthesizer.SynthesisContext{Config: cfg, Now: time.Now(), IDGenerator: synthesizer.NewStableIDGenerator()})
	if err != nil || len(auths) != 4 {
		t.Fatalf("synthesis count=%d err=%v", len(auths), err)
	}
	runtime := &Runtime{cfg: &config.Config{}}
	for _, auth := range auths {
		t.Run(auth.Provider, func(t *testing.T) {
			raw, errMarshal := json.Marshal(auth)
			if errMarshal != nil {
				t.Fatal(errMarshal)
			}
			var restored coreauth.Auth
			if errDecode := json.Unmarshal(raw, &restored); errDecode != nil {
				t.Fatal(errDecode)
			}
			runtime.registerModelsForAuth(&restored)
			t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(restored.ID) })
			info := dispatchModelInfoForAuth(restored.ID, "v8-upstream(high)", "v8-alias")
			if info == nil || info.ContextLength != 32768 || info.UserDefined {
				t.Fatalf("dispatch capabilities = %+v", info)
			}
			if info.Thinking == nil || !reflect.DeepEqual(info.Thinking.Levels, []string{"high", "none", "auto"}) || !info.Thinking.ZeroAllowed || !info.Thinking.DynamicAllowed {
				t.Fatalf("thinking = %+v", info.Thinking)
			}
			encoded, errEncode := json.Marshal(info)
			if errEncode != nil {
				t.Fatal(errEncode)
			}
			var wire map[string]any
			if errDecode := json.Unmarshal(encoded, &wire); errDecode != nil {
				t.Fatal(errDecode)
			}
			if wire["context_length"] != float64(32768) {
				t.Fatalf("missing wire capabilities: %s", encoded)
			}
			if _, exists := wire["max_context_length"]; exists {
				t.Fatalf("unexpected dispatch max_context_length: %s", encoded)
			}
			if _, exists := wire["is_compat"]; exists {
				t.Fatalf("unexpected dispatch is_compat: %s", encoded)
			}
			sanitized, errSanitize := SanitizeAuthForDownstream(&restored, nil)
			if errSanitize != nil {
				t.Fatal(errSanitize)
			}
			downstream, errMarshalDownstream := json.Marshal(sanitized)
			if errMarshalDownstream != nil {
				t.Fatal(errMarshalDownstream)
			}
			var credential struct {
				Metadata struct {
					Options struct {
						Models []config.CodexModel `json:"models"`
					} `json:"credential_options"`
				} `json:"metadata"`
			}
			if errDecode := json.Unmarshal(downstream, &credential); errDecode != nil {
				t.Fatal(errDecode)
			}
			models := credential.Metadata.Options.Models
			if len(models) != 1 || models[0].Name != "v8-upstream" || models[0].Alias != "v8-alias" || models[0].MaxContextLength != 32768 || !models[0].IsCompat {
				t.Fatalf("downstream credential model options = %+v", models)
			}
		})
	}
	if !reflect.DeepEqual(thinking.Levels, []string{" HIGH ", "none", "AUTO", "high"}) {
		t.Fatal("normalization mutated caller's config")
	}
	direct := buildConfigModels(cfg.CodexKey[0].Models, "openai", "codex")
	if len(direct) != 1 || direct[0].ContextLength != 32768 || direct[0].Thinking == thinking || direct[0].UserDefined {
		t.Fatalf("direct config capabilities = %+v", direct)
	}
	legacy := buildConfigModels([]config.CodexModel{{Name: "legacy-custom"}}, "openai", "codex")
	if len(legacy) != 1 || !legacy[0].UserDefined || legacy[0].Thinking != nil || legacy[0].ContextLength != 0 {
		t.Fatalf("legacy defaults changed: %+v", legacy)
	}
}

func TestV8ClaudeNativeToolSearchOption(t *testing.T) {
	synthesize := func(enabled bool) *coreauth.Auth {
		t.Helper()
		cfg := &config.Config{ClaudeKey: []config.ClaudeKey{{APIKey: "k", BaseURL: "https://gateway.example.com", NativeToolSearch: enabled}}}
		auths, errSynthesize := synthesizer.NewConfigSynthesizer().Synthesize(&synthesizer.SynthesisContext{
			Config: cfg, IDGenerator: synthesizer.NewStableIDGenerator(),
		})
		if errSynthesize != nil || len(auths) != 1 {
			t.Fatalf("synthesis count=%d err=%v", len(auths), errSynthesize)
		}
		return auths[0]
	}

	enabled, errSanitize := SanitizeAuthForDownstream(synthesize(true), nil)
	if errSanitize != nil {
		t.Fatal(errSanitize)
	}
	if got := enabled.Attributes["native_tool_search"]; got != "true" {
		t.Fatalf("native_tool_search = %q, want true", got)
	}
	options, ok := enabled.Metadata["credential_options"].(map[string]any)
	if !ok || options["native-tool-search"] != true {
		t.Fatalf("credential_options = %#v, want native-tool-search=true", enabled.Metadata["credential_options"])
	}

	if value, exists := synthesize(false).Attributes["native_tool_search"]; exists {
		t.Fatalf("native_tool_search = %q, want absent by default", value)
	}
}

func TestV8CredentialOptionsPreserveEmptyModels(t *testing.T) {
	for _, models := range [][]config.CodexModel{nil, {}} {
		cfg := &config.Config{CodexKey: []config.CodexKey{{APIKey: "test-key", BaseURL: "https://example.test", Models: models}}}
		auths, errSynthesize := synthesizer.NewConfigSynthesizer().Synthesize(&synthesizer.SynthesisContext{
			Config: cfg, IDGenerator: synthesizer.NewStableIDGenerator(),
		})
		if errSynthesize != nil || len(auths) != 1 {
			t.Fatalf("synthesis count=%d err=%v", len(auths), errSynthesize)
		}
		downstream, errSanitize := SanitizeAuthForDownstream(auths[0], nil)
		if errSanitize != nil {
			t.Fatal(errSanitize)
		}
		options, ok := downstream.Metadata["credential_options"].(map[string]any)
		if !ok {
			t.Fatal("missing credential options")
		}
		if _, exists := options["models"]; !exists {
			t.Fatal("empty model configuration must remain distinguishable from an older payload")
		}
	}
}

func TestV8KimiAIModelsAndVertexThinking(t *testing.T) {
	runtime := &Runtime{cfg: &config.Config{}}
	auth := &coreauth.Auth{ID: "kimi-ai-model-test", Provider: "kimi-ai", Status: coreauth.StatusActive, Metadata: map[string]any{"type": "kimi-ai"}}
	runtime.registerModelsForAuth(auth)
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	models := registry.GetKimiModels()
	if len(models) == 0 || dispatchModelInfoForAuth(auth.ID, models[0].ID, models[0].ID) == nil {
		t.Fatal("Kimi-AI has no dispatchable models")
	}
	vertex := buildConfigModels([]config.VertexCompatModel{{Name: "vertex-custom", Thinking: &registry.ThinkingSupport{Min: 128, Max: 8192}}}, "google", "vertex")
	if len(vertex) != 1 || vertex[0].Thinking == nil || vertex[0].Thinking.Max != 8192 || vertex[0].UserDefined {
		t.Fatalf("vertex override = %+v", vertex)
	}
}

func TestV8ModelAliasOptionsSurviveCredential(t *testing.T) {
	cfg := &config.Config{
		ClaudeKey: []config.ClaudeKey{{APIKey: "fixture-claude", Models: []config.ClaudeModel{
			{Name: "upstream", Alias: "friendly", DisplayName: " Friendly Model ", ForceMapping: true},
			{Name: "legacy-upstream", Alias: "legacy-alias"},
		}}},
		VertexCompatAPIKey: []config.VertexCompatKey{{APIKey: "fixture-vertex", BaseURL: "https://example.test", Models: []config.VertexCompatModel{
			{Name: "upstream", Alias: "friendly", DisplayName: " Friendly Model ", ForceMapping: true},
			{Name: "legacy-upstream", Alias: "legacy-alias"},
		}}},
	}
	checkModels := func(t *testing.T, models []*ModelInfo) {
		t.Helper()
		if len(models) != 2 {
			t.Fatalf("model count = %d, want 2", len(models))
		}
		for _, model := range models {
			switch model.ID {
			case "friendly":
				if model.Name != "upstream" || model.DisplayName != "Friendly Model" || model.ConfigDisplayName != "Friendly Model" || !model.ForceMapping {
					t.Errorf("configured alias options lost: %+v", model)
				}
			case "legacy-alias":
				if model.Name != "legacy-upstream" || model.DisplayName != "legacy-upstream" || model.ConfigDisplayName != "" || model.ForceMapping {
					t.Errorf("legacy alias defaults changed: %+v", model)
				}
			default:
				t.Errorf("unexpected model: %+v", model)
			}
		}
	}
	for _, direct := range []struct {
		provider string
		models   []*ModelInfo
	}{
		{"claude", buildConfigModels(cfg.ClaudeKey[0].Models, "anthropic", "claude")},
		{"vertex", buildConfigModels(cfg.VertexCompatAPIKey[0].Models, "google", "vertex")},
	} {
		t.Run(direct.provider+"-config", func(t *testing.T) { checkModels(t, direct.models) })
	}
	auths, errSynthesize := synthesizer.NewConfigSynthesizer().Synthesize(&synthesizer.SynthesisContext{
		Config: cfg, Now: time.Now(), IDGenerator: synthesizer.NewStableIDGenerator(),
	})
	if errSynthesize != nil || len(auths) != 2 {
		t.Fatalf("synthesis count=%d err=%v", len(auths), errSynthesize)
	}
	runtime := &Runtime{cfg: &config.Config{}}
	for _, auth := range auths {
		t.Run(auth.Provider+"-credential", func(t *testing.T) {
			data, errMarshal := json.Marshal(auth)
			if errMarshal != nil {
				t.Fatal(errMarshal)
			}
			var restored coreauth.Auth
			if errDecode := json.Unmarshal(data, &restored); errDecode != nil {
				t.Fatal(errDecode)
			}
			checkModels(t, modelInfosFromAuthMetadata(&restored, homeConfigModelsMetadataKey))
			runtime.registerModelsForAuth(&restored)
			t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(restored.ID) })
			checkModels(t, registry.GetGlobalRegistry().GetModelsForClient(restored.ID))
		})
	}
}

func TestV8OAuthModelAliasDisplayName(t *testing.T) {
	for _, test := range []struct {
		name        string
		fork        bool
		displayName string
		wantDisplay string
	}{
		{name: "rename", displayName: "Configured GPT Five", wantDisplay: "Configured GPT Five"},
		{name: "fork", fork: true, displayName: "Configured GPT Five", wantDisplay: "Configured GPT Five"},
		{name: "trimmed", displayName: " Configured GPT Five ", wantDisplay: "Configured GPT Five"},
		{name: "unset", wantDisplay: "Upstream GPT Five"},
		{name: "blank", fork: true, displayName: " \t ", wantDisplay: "Upstream GPT Five"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := &config.Config{OAuthModelAlias: map[string][]config.OAuthModelAlias{
				"codex": {{Name: "gpt-5", Alias: "g5", Fork: test.fork, DisplayName: test.displayName}},
			}}
			original := &ModelInfo{ID: "gpt-5", Name: "models/gpt-5", DisplayName: "Upstream GPT Five"}
			models := applyOAuthModelAlias(cfg, "codex", "oauth", []*ModelInfo{original})
			wantCount := 1
			if test.fork {
				wantCount++
			}
			if len(models) != wantCount {
				t.Fatalf("model count = %d, want %d", len(models), wantCount)
			}
			alias := models[len(models)-1]
			if alias.ID != "g5" || alias.Name != "models/g5" || alias.DisplayName != test.wantDisplay {
				t.Fatalf("alias = %+v, want g5 with display name %q", alias, test.wantDisplay)
			}
			if original.ID != "gpt-5" || original.Name != "models/gpt-5" || original.DisplayName != "Upstream GPT Five" {
				t.Fatalf("alias modified the upstream model: %+v", original)
			}
			if test.fork && models[0] != original {
				t.Fatal("fork did not preserve the original catalog entry")
			}
		})
	}
}
