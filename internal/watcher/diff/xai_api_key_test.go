package diff

import (
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
)

func TestBuildConfigChangeDetailsIncludesRedactedXAIChanges(t *testing.T) {
	disableCooling := true
	oldCfg := &config.Config{XAIKey: []config.XAIKey{{
		APIKey:  "old-secret",
		BaseURL: "https://api.x.ai/v1",
		Models:  []config.XAIModel{{Name: "grok-4.5", Alias: "grok-latest"}},
	}}}
	newCfg := &config.Config{XAIKey: []config.XAIKey{{
		APIKey:         "new-secret",
		Priority:       8,
		BaseURL:        "https://api.x.ai/v1",
		Websockets:     true,
		DisableCooling: &disableCooling,
		Models:         []config.XAIModel{{Name: "grok-4.5", Alias: "grok-latest", ForceMapping: true}},
	}}}

	changes := strings.Join(BuildConfigChangeDetails(oldCfg, newCfg), "\n")
	for _, want := range []string{
		"xai[0].priority: 0 -> 8",
		"xai[0].websockets: false -> true",
		"xai[0].disable-cooling: inherit -> true",
		"xai[0].api-key: updated",
		"xai[0].models: updated",
	} {
		if !strings.Contains(changes, want) {
			t.Fatalf("changes missing %q:\n%s", want, changes)
		}
	}
	if strings.Contains(changes, "old-secret") || strings.Contains(changes, "new-secret") {
		t.Fatalf("changes leaked xAI API key material:\n%s", changes)
	}
}
