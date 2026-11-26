package registry

import (
	"slices"
	"testing"
)

func TestLookupStaticModelInfoIncludesCopilot(t *testing.T) {
	for _, tt := range []struct{ id, wantType string }{
		{"mai-code-2-flash", "copilot"},
		{"claude-sonnet-5", "claude"},
	} {
		t.Run(tt.id, func(t *testing.T) {
			got := LookupStaticModelInfo(tt.id)
			if got == nil || got.ID != tt.id || got.Type != tt.wantType {
				t.Fatalf("lookup for %q = %+v, want type %q", tt.id, got, tt.wantType)
			}
		})
	}
}

func TestCopilotCatalogValidationAndChangeDetection(t *testing.T) {
	old := *getModels()
	if len(old.Copilot) == 0 || len(GetStaticModelDefinitionsByChannel("copilot")) != len(old.Copilot) {
		t.Fatal("missing built-in Copilot definitions")
	}
	next := old
	next.Copilot = []*ModelInfo{{ID: "fixture"}}
	if err := validateModelsCatalog(&next); err != nil {
		t.Fatal(err)
	}
	if changed := detectChangedProviders(&old, &next); !slices.Equal(changed, []string{"copilot"}) {
		t.Fatalf("changed providers = %v", changed)
	}
	next.Copilot = append(next.Copilot, &ModelInfo{ID: "fixture"})
	if err := validateModelsCatalog(&next); err == nil {
		t.Fatal("duplicate Copilot model ID was accepted")
	}
}
