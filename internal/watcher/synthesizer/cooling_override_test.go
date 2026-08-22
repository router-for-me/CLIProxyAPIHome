package synthesizer

import (
	"testing"
	"time"

	appconfig "github.com/router-for-me/CLIProxyAPIHome/internal/config"
)

func TestConfigSynthesizerPreservesExplicitFalseCoolingOverrides(t *testing.T) {
	disableCooling := false
	tests := []struct {
		name string
		cfg  *appconfig.Config
	}{
		{
			name: "gemini",
			cfg: &appconfig.Config{GeminiKey: []appconfig.GeminiKey{{
				APIKey:         "gemini-key",
				DisableCooling: &disableCooling,
			}}},
		},
		{
			name: "interactions",
			cfg: &appconfig.Config{InteractionsKey: []appconfig.GeminiKey{{
				APIKey:         "interactions-key",
				DisableCooling: &disableCooling,
			}}},
		},
		{
			name: "claude",
			cfg: &appconfig.Config{ClaudeKey: []appconfig.ClaudeKey{{
				APIKey:         "claude-key",
				DisableCooling: &disableCooling,
			}}},
		},
		{
			name: "codex",
			cfg: &appconfig.Config{CodexKey: []appconfig.CodexKey{{
				APIKey:         "codex-key",
				BaseURL:        "https://codex.example.com",
				DisableCooling: &disableCooling,
			}}},
		},
		{
			name: "xai",
			cfg: &appconfig.Config{XAIKey: []appconfig.XAIKey{{
				APIKey:         "xai-key",
				BaseURL:        "https://api.x.ai/v1",
				DisableCooling: &disableCooling,
			}}},
		},
		{
			name: "openai compatibility",
			cfg: &appconfig.Config{OpenAICompatibility: []appconfig.OpenAICompatibility{{
				Name:           "compat",
				BaseURL:        "https://compat.example.com",
				DisableCooling: &disableCooling,
				APIKeyEntries:  []appconfig.OpenAICompatibilityAPIKey{{APIKey: "compat-key"}},
			}}},
		},
		{
			name: "vertex",
			cfg: &appconfig.Config{VertexCompatAPIKey: []appconfig.VertexCompatKey{{
				APIKey:         "vertex-key",
				BaseURL:        "https://vertex.example.com",
				DisableCooling: &disableCooling,
			}}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			auths, errSynthesize := NewConfigSynthesizer().Synthesize(&SynthesisContext{
				Config:      tc.cfg,
				Now:         time.Unix(100, 0).UTC(),
				IDGenerator: NewStableIDGenerator(),
			})
			if errSynthesize != nil {
				t.Fatalf("Synthesize() error = %v", errSynthesize)
			}
			if len(auths) != 1 {
				t.Fatalf("auth count = %d, want 1", len(auths))
			}
			if disabled := auths[0].DisableCoolingOverride(); disabled == nil || *disabled {
				t.Fatalf("DisableCoolingOverride() = %#v, want false", disabled)
			}
		})
	}
}
