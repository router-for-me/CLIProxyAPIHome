package registry

import (
	"reflect"
	"strings"
	"testing"
)

func TestCodexChannelsIncludeImageBuiltins(t *testing.T) {
	wantIDs := []string{
		"gpt-image-2", "gpt-image-2.5", "gpt-image-2.5-flare", "gpt-image-2.5-sunburst",
		"gpt-image-2.6", "gpt-image-2.6-flare", "gpt-image-2.6-sunburst",
	}
	for _, channel := range []string{"codex", "codex-free", "codex-plus", "codex-team", "codex-pro"} {
		found := make(map[string]bool)
		for _, model := range GetStaticModelDefinitionsByChannel(channel) {
			if model != nil {
				found[model.ID] = true
			}
		}
		for _, id := range wantIDs {
			if !found[id] {
				t.Errorf("%s: missing builtin model %q", channel, id)
			}
			if !IsCodexBuiltinModelID(id) {
				t.Errorf("IsCodexBuiltinModelID(%q) = false, want true", id)
			}
		}
	}
	if IsCodexBuiltinModelID("gpt-5") {
		t.Fatal("IsCodexBuiltinModelID(gpt-5) = true, want false")
	}
}

func TestWithCodexBuiltinsPreservesExistingModels(t *testing.T) {
	for _, userDefined := range []bool{true, false} {
		// Explicit thinking can clear UserDefined; it is not configuration provenance.
		configured := &ModelInfo{
			ID: " GPT-IMAGE-2.5 ", Name: "vendor/upstream-image-25",
			DisplayName: "Custom Image", ConfigDisplayName: "Custom Image",
			UserDefined: userDefined, ForceMapping: true,
			Thinking: &ThinkingSupport{Levels: []string{"low"}},
		}
		input := []*ModelInfo{configured, {ID: "custom"}}
		before := cloneModelInfos(input)
		models := WithCodexBuiltins(input)
		if !reflect.DeepEqual(input, before) {
			t.Fatal("WithCodexBuiltins mutated its input")
		}
		if len(models) != 8 {
			t.Fatalf("got %d models, want 8", len(models))
		}
		found := 0
		for _, model := range models {
			if strings.EqualFold(strings.TrimSpace(model.ID), "gpt-image-2.5") {
				found++
				if !reflect.DeepEqual(model, configured) {
					t.Errorf("configured model changed: got %+v, want %+v", model, configured)
				}
			}
		}
		if found != 1 {
			t.Errorf("got %d entries for gpt-image-2.5, want 1", found)
		}
		if again := WithCodexBuiltins(models); !reflect.DeepEqual(again, models) {
			t.Fatal("repeated injection changed the models")
		}
	}
}

func TestCodexCatalogBuiltinsOverrideRemoteDefinitions(t *testing.T) {
	original := getModels()
	catalog := *original
	stale := []*ModelInfo{{ID: "gpt-image-2.5", DisplayName: "Stale Remote Image"}}
	catalog.CodexFree, catalog.CodexPlus, catalog.CodexTeam, catalog.CodexPro = stale, stale, stale, stale
	modelsCatalogStore.mu.Lock()
	modelsCatalogStore.data = &catalog
	modelsCatalogStore.mu.Unlock()
	t.Cleanup(func() {
		modelsCatalogStore.mu.Lock()
		modelsCatalogStore.data = original
		modelsCatalogStore.mu.Unlock()
	})

	for _, channel := range []string{"codex-free", "codex-plus", "codex-team", "codex-pro"} {
		models := GetStaticModelDefinitionsByChannel(channel)
		if len(models) != 7 {
			t.Fatalf("%s has %d models, want 7", channel, len(models))
		}
		for _, model := range models {
			if model.ID == "gpt-image-2.5" && model.DisplayName != "GPT Image 2.5" {
				t.Errorf("%s retained stale built-in: %+v", channel, model)
			}
		}
	}
}
