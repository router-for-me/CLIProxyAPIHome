package synthesizer

import (
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
)

func TestConfigSynthesizerCodexAlphaSearchAttribute(t *testing.T) {
	cfg := &config.Config{
		CodexKey: []config.CodexKey{
			{APIKey: "codex-opted", AlphaSearch: true},
			{APIKey: "codex-default"},
		},
		XAIKey: []config.XAIKey{{APIKey: "xai-key", BaseURL: "https://api.x.ai/v1", AlphaSearch: true}},
	}
	auths, errSynthesize := NewConfigSynthesizer().Synthesize(&SynthesisContext{
		Config: cfg, Now: time.Now().UTC(), IDGenerator: NewStableIDGenerator(),
	})
	if errSynthesize != nil {
		t.Fatalf("Synthesize() error = %v", errSynthesize)
	}
	byKey := make(map[string]*coreauth.Auth, len(auths))
	for _, auth := range auths {
		if auth != nil && auth.Attributes != nil {
			byKey[auth.Attributes["api_key"]] = auth
		}
	}
	if got := byKey["codex-opted"].Attributes[coreauth.AttributeCodexAlphaSearch]; got != "true" {
		t.Fatalf("opted-in Codex attribute = %q, want true", got)
	}
	if _, exists := byKey["codex-default"].Attributes[coreauth.AttributeCodexAlphaSearch]; exists {
		t.Fatal("default Codex key unexpectedly has codex_alpha_search")
	}
	if _, exists := byKey["xai-key"].Attributes[coreauth.AttributeCodexAlphaSearch]; exists {
		t.Fatal("xAI key unexpectedly has codex_alpha_search")
	}
}
