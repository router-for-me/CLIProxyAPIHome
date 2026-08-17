package synthesizer

import (
	"testing"
	"time"

	appconfig "github.com/router-for-me/CLIProxyAPIHome/internal/config"
)

func TestConfigSynthesizerBuildsXAIAPIKeyAuth(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	disableCooling := true
	cfg := &appconfig.Config{
		XAIKey: []appconfig.XAIKey{{
			APIKey:         "xai-key",
			Priority:       7,
			Prefix:         "grok",
			BaseURL:        "https://api.x.ai/v1",
			Websockets:     true,
			ProxyURL:       "socks5://proxy.example:1080",
			Headers:        map[string]string{"X-Test": "value"},
			ExcludedModels: []string{"grok-3-*"},
			DisableCooling: &disableCooling,
			Models: []appconfig.XAIModel{{
				Name:         "grok-4.5",
				Alias:        "grok-latest",
				DisplayName:  "Grok Latest",
				ForceMapping: true,
			}},
		}},
	}

	auths, errSynthesize := NewConfigSynthesizer().Synthesize(&SynthesisContext{
		Config:      cfg,
		Now:         now,
		IDGenerator: NewStableIDGenerator(),
	})
	if errSynthesize != nil {
		t.Fatalf("Synthesize() error = %v", errSynthesize)
	}
	if len(auths) != 1 {
		t.Fatalf("auth count = %d, want 1", len(auths))
	}
	auth := auths[0]
	if auth.Provider != "xai" || auth.Label != "xai-apikey" || auth.Prefix != "grok" {
		t.Fatalf("auth identity = %+v", auth)
	}
	if auth.Attributes["source"] == "" || auth.Attributes["api_key"] != "xai-key" || auth.Attributes["base_url"] != "https://api.x.ai/v1" {
		t.Fatalf("auth attributes = %v", auth.Attributes)
	}
	if auth.Attributes["priority"] != "7" || auth.Attributes["websockets"] != "true" || auth.Attributes["header:X-Test"] != "value" {
		t.Fatalf("auth routing attributes = %v", auth.Attributes)
	}
	if auth.Attributes["excluded_models"] != "grok-3-*" || auth.ProxyURL != "socks5://proxy.example:1080" {
		t.Fatalf("auth exclusion/proxy = %q/%q", auth.Attributes["excluded_models"], auth.ProxyURL)
	}
	if disabled := auth.DisableCoolingOverride(); disabled == nil || !*disabled {
		t.Fatalf("DisableCoolingOverride() = %#v, want true", disabled)
	}

	rawModels, okModels := auth.Metadata[homeConfigModelsMetadataKey].([]map[string]any)
	if !okModels || len(rawModels) != 1 {
		t.Fatalf("home_config_models = %#v", auth.Metadata[homeConfigModelsMetadataKey])
	}
	model := rawModels[0]
	if model["id"] != "grok-latest" || model["name"] != "grok-4.5" || model["config_display_name"] != "Grok Latest" || model["force_mapping"] != true {
		t.Fatalf("xAI model metadata = %#v", model)
	}
}
