package diff

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
)

func TestGeminiModelHashesIncludeCatalogFields(t *testing.T) {
	base := []config.GeminiModel{{Name: "gemini-upstream", Alias: "gemini-alias"}}
	baseHash := ComputeGeminiModelsHash(base)
	baseSummary := SummarizeGeminiModels(base)
	tests := []struct {
		name   string
		models []config.GeminiModel
	}{
		{name: "display name", models: []config.GeminiModel{{Name: "gemini-upstream", Alias: "gemini-alias", DisplayName: "Catalog Name"}}},
		{name: "force mapping", models: []config.GeminiModel{{Name: "gemini-upstream", Alias: "gemini-alias", ForceMapping: true}}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ComputeGeminiModelsHash(tc.models); got == baseHash {
				t.Fatalf("ComputeGeminiModelsHash() did not change for %s", tc.name)
			}
			if got := SummarizeGeminiModels(tc.models); got.hash == baseSummary.hash {
				t.Fatalf("SummarizeGeminiModels() did not change for %s", tc.name)
			}
		})
	}
}
