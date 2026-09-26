package synthesizer

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
)

func TestCodexKeySynthesisPreservesPerModelConfigurationUpdateCapability(t *testing.T) {
	cfg := &config.Config{CodexKey: []config.CodexKey{{APIKey: "test-key", Models: []config.CodexModel{
		{Name: "model-on", SupportConfigurationUpdate: true},
		{Name: "model-off"},
	}}}}
	auths, errSynthesize := NewConfigSynthesizer().Synthesize(&SynthesisContext{Config: cfg, Now: time.Unix(1, 0), IDGenerator: NewStableIDGenerator()})
	if errSynthesize != nil {
		t.Fatal(errSynthesize)
	}
	if len(auths) != 1 {
		t.Fatalf("auths = %d, want one", len(auths))
	}
	models, ok := auths[0].Metadata[homeConfigModelsMetadataKey].([]map[string]any)
	if !ok {
		t.Fatalf("config models metadata = %#v", auths[0].Metadata)
	}
	got := make(map[string]bool)
	for _, model := range models {
		if model["id"] == "model-on" || model["id"] == "model-off" {
			got[model["id"].(string)], _ = model["support_configuration_update"].(bool)
		}
	}
	if !got["model-on"] || got["model-off"] {
		t.Fatalf("synthesized model capabilities = %v, want on=true/off=false", got)
	}
}
