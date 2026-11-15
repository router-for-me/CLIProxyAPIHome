package synthesizer

import (
	"testing"
	"time"

	appconfig "github.com/router-for-me/CLIProxyAPIHome/internal/config"
)

func TestCPAExecutionSynthesisRejectsMalformedTrees(t *testing.T) {
	for _, family := range []string{"gemini", "interactions", "vertex", "codex", "xai", "meta", "claude", "compatibility-group", "compatibility-key"} {
		t.Run(family, func(t *testing.T) {
			bad := appconfig.CredentialConfig{RawConfig: map[string]any{"api-key": "fixture", "connection-pool": map[string]any{"enabled": "invalid"}}}
			cfg := &appconfig.Config{}
			switch family {
			case "gemini":
				cfg.GeminiKey = []appconfig.GeminiKey{{APIKey: "fixture", CredentialConfig: bad}}
			case "interactions":
				cfg.InteractionsKey = []appconfig.GeminiKey{{APIKey: "fixture", CredentialConfig: bad}}
			case "vertex":
				cfg.VertexCompatAPIKey = []appconfig.VertexCompatKey{{APIKey: "fixture", CredentialConfig: bad}}
			case "codex":
				cfg.CodexKey = []appconfig.CodexKey{{APIKey: "fixture", CredentialConfig: bad}}
			case "xai":
				cfg.XAIKey = []appconfig.XAIKey{{APIKey: "fixture", CredentialConfig: bad}}
			case "meta":
				cfg.MetaKey = []appconfig.MetaKey{{APIKey: "fixture", CredentialConfig: bad}}
			case "claude":
				cfg.GeminiKey = []appconfig.GeminiKey{{APIKey: "valid-before-invalid"}}
				cfg.ClaudeKey = []appconfig.ClaudeKey{{APIKey: "fixture", CredentialConfig: bad}}
			case "compatibility-group":
				cfg.OpenAICompatibility = []appconfig.OpenAICompatibility{{Name: "gateway", BaseURL: "https://example.test", CredentialConfig: bad}}
			case "compatibility-key":
				cfg.OpenAICompatibility = []appconfig.OpenAICompatibility{{Name: "gateway", BaseURL: "https://example.test", APIKeyEntries: []appconfig.OpenAICompatibilityAPIKey{{APIKey: "fixture", CredentialConfig: bad}}}}
			}
			auths, errSynthesize := NewConfigSynthesizer().Synthesize(&SynthesisContext{Config: cfg, Now: time.Now(), IDGenerator: NewStableIDGenerator()})
			if errSynthesize == nil || auths != nil {
				t.Fatalf("invalid tree returned partial credentials: count=%d err=%v", len(auths), errSynthesize)
			}
		})
	}
}
