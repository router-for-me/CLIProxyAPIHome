package synthesizer

import (
	"testing"
	"time"

	appconfig "github.com/router-for-me/CLIProxyAPIHome/internal/config"
)

func TestConfigSynthesizerPreservesRequestRetryOverrides(t *testing.T) {
	requestRetry := 0
	tests := []struct {
		name string
		cfg  *appconfig.Config
	}{
		{
			name: "gemini",
			cfg:  &appconfig.Config{GeminiKey: []appconfig.GeminiKey{{APIKey: "gemini-key", RequestRetry: &requestRetry}}},
		},
		{
			name: "claude",
			cfg:  &appconfig.Config{ClaudeKey: []appconfig.ClaudeKey{{APIKey: "claude-key", RequestRetry: &requestRetry}}},
		},
		{
			name: "codex",
			cfg:  &appconfig.Config{CodexKey: []appconfig.CodexKey{{APIKey: "codex-key", RequestRetry: &requestRetry}}},
		},
		{
			name: "xai",
			cfg:  &appconfig.Config{XAIKey: []appconfig.XAIKey{{APIKey: "xai-key", RequestRetry: &requestRetry}}},
		},
		{
			name: "openai compatibility",
			cfg: &appconfig.Config{OpenAICompatibility: []appconfig.OpenAICompatibility{{
				Name:          "compat",
				BaseURL:       "https://compat.example.com",
				RequestRetry:  &requestRetry,
				APIKeyEntries: []appconfig.OpenAICompatibilityAPIKey{{APIKey: "compat-key"}},
			}}},
		},
		{
			name: "vertex",
			cfg:  &appconfig.Config{VertexCompatAPIKey: []appconfig.VertexCompatKey{{APIKey: "vertex-key", RequestRetry: &requestRetry}}},
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
			if got, ok := auths[0].RequestRetryOverride(); !ok || got != 0 {
				t.Fatalf("RequestRetryOverride() = (%d, %t), want (0, true)", got, ok)
			}
		})
	}
}

func TestConfigSynthesizerTreatsNegativeRequestRetryAsInherited(t *testing.T) {
	requestRetry := -1
	auths, errSynthesize := NewConfigSynthesizer().Synthesize(&SynthesisContext{
		Config: &appconfig.Config{GeminiKey: []appconfig.GeminiKey{{
			APIKey:       "gemini-key",
			RequestRetry: &requestRetry,
		}}},
		Now:         time.Unix(100, 0).UTC(),
		IDGenerator: NewStableIDGenerator(),
	})
	if errSynthesize != nil {
		t.Fatalf("Synthesize() error = %v", errSynthesize)
	}
	if len(auths) != 1 {
		t.Fatalf("auth count = %d, want 1", len(auths))
	}
	if got, ok := auths[0].RequestRetryOverride(); ok || got != 0 {
		t.Fatalf("RequestRetryOverride() = (%d, %t), want inherited", got, ok)
	}
}
