package synthesizer

import (
	"testing"
	"time"

	appconfig "github.com/router-for-me/CLIProxyAPIHome/internal/config"
)

func TestConfigSynthesizerBuildsHeaderAuthenticatedInteractionsAuth(t *testing.T) {
	cfg := &appconfig.Config{InteractionsKey: []appconfig.GeminiKey{{
		BaseURL: "https://header-auth.example.test",
		Headers: map[string]string{"Authorization": "Bearer token"},
		Models: []appconfig.GeminiModel{{
			Name:         "gemini-upstream",
			Alias:        "gemini-alias",
			DisplayName:  "Gemini Catalog Name",
			ForceMapping: true,
		}},
	}}}

	auths, errSynthesize := NewConfigSynthesizer().Synthesize(&SynthesisContext{
		Config:      cfg,
		Now:         time.Unix(100, 0).UTC(),
		IDGenerator: NewStableIDGenerator(),
	})
	if errSynthesize != nil {
		t.Fatal(errSynthesize)
	}
	if len(auths) != 1 {
		t.Fatalf("auth count = %d, want 1", len(auths))
	}
	auth := auths[0]
	if auth.Provider != "gemini-interactions" || auth.Attributes["base_url"] != "https://header-auth.example.test" || auth.Attributes["config_index"] != "0" {
		t.Fatalf("auth = %#v", auth)
	}
	if _, exists := auth.Attributes["api_key"]; exists {
		t.Fatalf("api_key should be absent for header-authenticated entry: %#v", auth.Attributes)
	}
	if auth.Attributes["header:Authorization"] != "Bearer token" {
		t.Fatalf("headers = %#v", auth.Attributes)
	}
	rawModels, okModels := auth.Metadata[homeConfigModelsMetadataKey].([]map[string]any)
	if !okModels || len(rawModels) != 1 {
		t.Fatalf("home_config_models = %#v", auth.Metadata[homeConfigModelsMetadataKey])
	}
	model := rawModels[0]
	if model["config_display_name"] != "Gemini Catalog Name" || model["force_mapping"] != true {
		t.Fatalf("model metadata = %#v", model)
	}
}

func TestConfigSynthesizerPreservesOpenAICompatUpstreamModelInMetadata(t *testing.T) {
	cfg := &appconfig.Config{OpenAICompatibility: []appconfig.OpenAICompatibility{{
		Name:    "zai-coding",
		BaseURL: "https://api.example.test/v1",
		Models: []appconfig.OpenAICompatibilityModel{{
			Name:  "glm-5.3",
			Alias: "fractalops-coding",
		}},
	}}}
	auths, errSynthesize := NewConfigSynthesizer().Synthesize(&SynthesisContext{
		Config:      cfg,
		Now:         time.Unix(100, 0).UTC(),
		IDGenerator: NewStableIDGenerator(),
	})
	if errSynthesize != nil {
		t.Fatal(errSynthesize)
	}
	if len(auths) != 1 {
		t.Fatalf("auth count = %d, want 1", len(auths))
	}
	rawModels, okModels := auths[0].Metadata[homeConfigModelsMetadataKey].([]map[string]any)
	if !okModels || len(rawModels) != 1 {
		t.Fatalf("home_config_models = %#v", auths[0].Metadata[homeConfigModelsMetadataKey])
	}
	if rawModels[0]["id"] != "fractalops-coding" || rawModels[0]["name"] != "glm-5.3" {
		t.Fatalf("model metadata = %#v, want alias and upstream name", rawModels[0])
	}
}

func TestConfigSynthesizerUsesFullGeminiCredentialIdentity(t *testing.T) {
	base := appconfig.GeminiKey{
		APIKey:   "shared-key",
		BaseURL:  "https://api.example.test",
		Prefix:   "team",
		ProxyURL: "http://proxy-one.test",
		Headers:  map[string]string{"X-Auth": "one"},
	}
	tests := []struct {
		name    string
		variant appconfig.GeminiKey
	}{
		{
			name:    "prefix",
			variant: appconfig.GeminiKey{APIKey: "shared-key", BaseURL: "https://api.example.test", Prefix: "other-team", ProxyURL: "http://proxy-one.test", Headers: map[string]string{"X-Auth": "one"}},
		},
		{
			name:    "proxy",
			variant: appconfig.GeminiKey{APIKey: "shared-key", BaseURL: "https://api.example.test", Prefix: "team", ProxyURL: "http://proxy-two.test", Headers: map[string]string{"X-Auth": "one"}},
		},
		{
			name:    "headers",
			variant: appconfig.GeminiKey{APIKey: "shared-key", BaseURL: "https://api.example.test", Prefix: "team", ProxyURL: "http://proxy-one.test", Headers: map[string]string{"X-Auth": "two"}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			auths, errSynthesize := NewConfigSynthesizer().Synthesize(&SynthesisContext{
				Config:      &appconfig.Config{GeminiKey: []appconfig.GeminiKey{base, tc.variant}},
				Now:         time.Unix(100, 0).UTC(),
				IDGenerator: NewStableIDGenerator(),
			})
			if errSynthesize != nil {
				t.Fatal(errSynthesize)
			}
			if len(auths) != 2 {
				t.Fatalf("auth count = %d, want 2", len(auths))
			}
			if auths[0].ID == auths[1].ID || auths[0].Attributes["source"] == auths[1].Attributes["source"] {
				t.Fatalf("credential identities collided: %#v / %#v", auths[0], auths[1])
			}
			if auths[0].Attributes["config_index"] != "0" || auths[1].Attributes["config_index"] != "1" {
				t.Fatalf("config indexes = %q/%q", auths[0].Attributes["config_index"], auths[1].Attributes["config_index"])
			}
		})
	}
}
