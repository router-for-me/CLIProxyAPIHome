package diff

import (
	"slices"
	"testing"

	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
)

func TestBuildConfigChangeDetailsIncludesCredentialRequestRetry(t *testing.T) {
	zero := 0
	two := 2
	oldCfg := &config.Config{
		GeminiKey: []config.GeminiKey{{APIKey: "secret", RequestRetry: &zero}},
		OpenAICompatibility: []config.OpenAICompatibility{{
			Name:          "compat",
			BaseURL:       "https://compat.example.com",
			RequestRetry:  &zero,
			APIKeyEntries: []config.OpenAICompatibilityAPIKey{{APIKey: "secret"}},
		}},
	}
	newCfg := &config.Config{
		GeminiKey: []config.GeminiKey{{APIKey: "secret", RequestRetry: &two}},
		OpenAICompatibility: []config.OpenAICompatibility{{
			Name:          "compat",
			BaseURL:       "https://compat.example.com",
			RequestRetry:  &two,
			APIKeyEntries: []config.OpenAICompatibilityAPIKey{{APIKey: "secret"}},
		}},
	}

	changes := BuildConfigChangeDetails(oldCfg, newCfg)
	if !slices.Contains(changes, "gemini[0].request-retry: 0 -> 2") {
		t.Fatalf("changes = %v, want Gemini request-retry detail", changes)
	}
	if !slices.Contains(changes, "  provider updated: compat (request-retry 0 -> 2)") {
		t.Fatalf("changes = %v, want OpenAI compatibility request-retry detail", changes)
	}
}
