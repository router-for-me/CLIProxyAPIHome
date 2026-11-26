package registry

import (
	"reflect"
	"testing"
)

func TestLookupStaticModelInfoCoversAllChannels(t *testing.T) {
	// Bare IDs use the first matching source entry. Channel lookups also include built-ins.
	data := getModels()
	wantBare := make(map[string]*ModelInfo)
	for _, models := range [][]*ModelInfo{
		data.Claude, data.Gemini, data.Vertex, data.CodexPro, data.Kimi,
		data.Antigravity, data.XAI, data.Meta, data.Copilot, GetDevinModels(),
	} {
		for _, model := range models {
			if wantBare[model.ID] == nil {
				wantBare[model.ID] = model
			}
		}
	}

	for channel, models := range GetAllStaticModelDefinitions() {
		t.Run(channel, func(t *testing.T) {
			for _, model := range models {
				if got, want := LookupStaticModelInfo(model.ID), wantBare[model.ID]; !reflect.DeepEqual(got, want) {
					t.Errorf("bare lookup of %q = %+v, want %+v", model.ID, got, want)
				}
				qualified := channel + "/" + model.ID
				want := LookupStaticModelInfoByChannel(model.ID, channel)
				if want == nil {
					t.Fatalf("channel lookup missed %q", model.ID)
				}
				want.ID = qualified
				if got := LookupStaticModelInfo(qualified); !reflect.DeepEqual(got, want) {
					t.Errorf("static metadata for %q differs from its channel definition", qualified)
				}
			}
		})
	}
}

func TestLookupStaticModelInfoUsesActiveDevinCatalog(t *testing.T) {
	for _, model := range GetDevinModels() {
		want := *model
		want.ID = "devin/" + model.ID
		got := LookupStaticModelInfo(want.ID)
		if !reflect.DeepEqual(got, &want) {
			t.Errorf("qualified lookup metadata differs from the active Devin catalog for %q", model.ID)
		}
		if got := LookupStaticModelInfoByChannel(model.ID, "devin"); !reflect.DeepEqual(got, model) {
			t.Errorf("channel lookup of bare ID %q = %+v, want %+v", model.ID, got, model)
		}
	}
	for _, id := range []string{"swe-2", "swe-2-flash"} {
		want := LookupStaticModelInfoByChannel(id, "devin")
		if got := LookupStaticModelInfo(id); want == nil || !reflect.DeepEqual(got, want) {
			t.Errorf("global lookup of bare Devin model %q = %+v, want %+v", id, got, want)
		}
		if got := LookupModelInfo(id, "devin"); !reflect.DeepEqual(got, want) {
			t.Errorf("runtime lookup of %q scoped to Devin = %+v, want %+v", id, got, want)
		}
		if got := LookupModelInfo(id, "codex"); got != nil {
			t.Errorf("runtime lookup of %q scoped to Codex inherited Devin metadata: %+v", id, got)
		}
		if got := LookupModelInfo(id); got != nil {
			t.Errorf("runtime lookup of unregistered %q inherited Devin metadata: %+v", id, got)
		}
		wantQualified := *want
		wantQualified.ID = "devin/" + want.ID
		if got := LookupModelInfo(wantQualified.ID); !reflect.DeepEqual(got, &wantQualified) {
			t.Errorf("runtime lookup of qualified %q = %+v, want %+v", wantQualified.ID, got, &wantQualified)
		}
	}
}

func TestLookupStaticModelInfoWithChannelQualifier(t *testing.T) {
	for _, tc := range []struct {
		channel string
		modelID string
		query   string
	}{
		{channel: "devin", modelID: "swe-2", query: "Devin/SWE-2"},
		{channel: "copilot", modelID: "gpt-5.3-codex", query: "copilot/gpt-5.3-codex"},
	} {
		t.Run(tc.channel, func(t *testing.T) {
			native := LookupStaticModelInfoByChannel(tc.modelID, tc.channel)
			if native == nil || native.ID != tc.modelID {
				t.Fatalf("lookup of native model %q returned %+v", tc.modelID, native)
			}
			expected := *native
			expected.ID = tc.channel + "/" + native.ID
			if got := LookupStaticModelInfoByChannel(tc.query, tc.channel); !reflect.DeepEqual(got, &expected) {
				t.Errorf("scoped lookup of %q = %+v, want %+v", tc.query, got, expected)
			}
			if got := LookupStaticModelInfo(tc.query); !reflect.DeepEqual(got, &expected) {
				t.Errorf("global lookup of %q = %+v, want %+v", tc.query, got, expected)
			}
		})
	}
	if got := LookupStaticModelInfoByChannel("copilot/gpt-5.3-codex", "devin"); got != nil {
		t.Errorf("Devin lookup followed a Copilot qualifier: %+v", got)
	}
	if got := LookupStaticModelInfoByChannel("devin/swe-2", "copilot"); got != nil {
		t.Errorf("Copilot lookup followed a Devin qualifier: %+v", got)
	}
	if got := LookupStaticModelInfoByChannel("gpt-5.3-codex", "aistudio"); got != nil {
		t.Errorf("unexpected AI Studio catalog in Home: %+v", got)
	}
	wantCodex := findStaticModel(getModels().CodexPro, "gpt-6-sol")
	if got := LookupStaticModelInfo("gpt-6-sol"); wantCodex == nil || !reflect.DeepEqual(got, wantCodex) {
		t.Errorf("global lookup of shared model gpt-6-sol = %+v, want Codex definition %+v", got, wantCodex)
	}
	wantClaude := findStaticModel(getModels().Claude, "claude-sonnet-5")
	if got := LookupStaticModelInfo("claude-sonnet-5"); wantClaude == nil || !reflect.DeepEqual(got, wantClaude) {
		t.Errorf("global lookup of shared model claude-sonnet-5 = %+v, want Claude definition %+v", got, wantClaude)
	}
}

func TestLookupStaticModelInfoCopilotBeforeDevin(t *testing.T) {
	const id = "swe-2"
	modelsCatalogStore.mu.Lock()
	original := modelsCatalogStore.data
	modelsCatalogStore.data = &staticModelsJSON{Copilot: []*ModelInfo{{ID: id, Type: "copilot"}}}
	modelsCatalogStore.mu.Unlock()
	t.Cleanup(func() {
		modelsCatalogStore.mu.Lock()
		modelsCatalogStore.data = original
		modelsCatalogStore.mu.Unlock()
	})

	for _, tc := range []struct{ query, channel string }{
		{id, "copilot"},
		{"devin/" + id, "devin"},
	} {
		want := LookupStaticModelInfoByChannel(tc.query, tc.channel)
		if got := LookupStaticModelInfo(tc.query); want == nil || !reflect.DeepEqual(got, want) {
			t.Errorf("lookup of %q = %+v, want %s definition %+v", tc.query, got, tc.channel, want)
		}
	}
}

func TestLookupStaticModelInfoDevinRefreshAndFallback(t *testing.T) {
	modelsCatalogStore.mu.RLock()
	originalGeneral := modelsCatalogStore.data
	modelsCatalogStore.mu.RUnlock()
	devinCatalogStore.mu.RLock()
	originalModels := devinCatalogStore.models
	originalJSON := devinCatalogStore.rawJSON
	originalRevision := devinCatalogStore.revision
	devinCatalogStore.mu.RUnlock()
	t.Cleanup(func() {
		modelsCatalogStore.mu.Lock()
		modelsCatalogStore.data = originalGeneral
		modelsCatalogStore.mu.Unlock()
		devinCatalogStore.mu.Lock()
		devinCatalogStore.models = originalModels
		devinCatalogStore.rawJSON = originalJSON
		devinCatalogStore.revision = originalRevision
		devinCatalogStore.mu.Unlock()
	})

	legacy := &ModelInfo{ID: "devin/swe-2", DisplayName: "Legacy", MaxCompletionTokens: 128000}
	general := *originalGeneral
	general.Devin = []*ModelInfo{legacy}
	modelsCatalogStore.mu.Lock()
	modelsCatalogStore.data = &general
	modelsCatalogStore.mu.Unlock()

	for _, payload := range []string{
		`{"devin":[{"id":"swe-2","max_completion_tokens":64000,"thinking":{"levels":["medium"]}}]}`,
		`{"devin":[{"id":"swe-2","max_completion_tokens":32000,"thinking":{"levels":["high"]}}]}`,
	} {
		if _, errLoad := loadDevinModelsFromBytes([]byte(payload), "test"); errLoad != nil {
			t.Fatal(errLoad)
		}
		for _, model := range GetDevinModels() {
			for _, id := range []string{model.ID, "devin/" + model.ID} {
				want := *model
				want.ID = id
				got := LookupStaticModelInfo(id)
				if !reflect.DeepEqual(got, &want) {
					t.Fatalf("lookup did not use refreshed metadata for %q: got %+v, want %+v", id, got, &want)
				}
				if got.Thinking != nil {
					got.Thinking.Levels[0] = "mutated"
					if !reflect.DeepEqual(LookupStaticModelInfo(id), &want) {
						t.Fatal("lookup result shares thinking metadata with the catalog")
					}
				}
			}
		}
		if got := LookupStaticModelInfo("devin/gpt-6-astra"); got != nil {
			t.Fatalf("lookup resurrected a model absent from the active catalog: %+v", got)
		}
	}

	// Preserve legacy models.json compatibility when the independent catalog is unavailable.
	devinCatalogStore.mu.Lock()
	devinCatalogStore.models = nil
	devinCatalogStore.mu.Unlock()
	if got := LookupStaticModelInfo(legacy.ID); !reflect.DeepEqual(got, legacy) {
		t.Fatalf("legacy fallback = %+v, want %+v", got, legacy)
	}
	bareLegacy := *legacy
	bareLegacy.ID = "swe-2"
	if got := LookupStaticModelInfo(bareLegacy.ID); !reflect.DeepEqual(got, &bareLegacy) {
		t.Fatalf("bare legacy fallback = %+v, want %+v", got, &bareLegacy)
	}

	// Return empty when both Devin catalog sources are absent.
	generalWithoutDevin := general
	generalWithoutDevin.Devin = nil
	modelsCatalogStore.mu.Lock()
	modelsCatalogStore.data = &generalWithoutDevin
	modelsCatalogStore.mu.Unlock()
	if got := GetDevinModels(); len(got) != 0 {
		t.Fatalf("GetDevinModels() = %+v, want empty when all catalog sources are absent", got)
	}
	if got := LookupStaticModelInfo(legacy.ID); got != nil {
		t.Fatalf("LookupStaticModelInfo(%q) = %+v, want nil", legacy.ID, got)
	}
}

func TestLookupStaticModelInfoUsesEffectiveMetaCatalog(t *testing.T) {
	modelsCatalogStore.mu.RLock()
	original := modelsCatalogStore.data
	modelsCatalogStore.mu.RUnlock()
	t.Cleanup(func() {
		modelsCatalogStore.mu.Lock()
		modelsCatalogStore.data = original
		modelsCatalogStore.mu.Unlock()
	})
	for _, model := range GetMetaModels() {
		if got := LookupStaticModelInfo(model.ID); !reflect.DeepEqual(got, model) {
			t.Errorf("lookup metadata differs from the active Meta catalog for %q", model.ID)
		}
	}

	partial := *original
	partial.Meta = []*ModelInfo{{ID: "muse-spark-1.3", ContextLength: 12345}}
	modelsCatalogStore.mu.Lock()
	modelsCatalogStore.data = &partial
	modelsCatalogStore.mu.Unlock()
	if got := LookupStaticModelInfo(partial.Meta[0].ID); !reflect.DeepEqual(got, partial.Meta[0]) {
		t.Fatalf("lookup did not use active Meta metadata: %+v", got)
	}
	if got := LookupStaticModelInfo("muse-spark-1.1"); got != nil {
		t.Fatalf("lookup resurrected a Meta model absent from the active catalog: %+v", got)
	}

	withoutMeta := partial
	withoutMeta.Meta = nil
	modelsCatalogStore.mu.Lock()
	modelsCatalogStore.data = &withoutMeta
	modelsCatalogStore.mu.Unlock()
	if got := GetMetaModels(); len(got) != 0 {
		t.Fatalf("GetMetaModels() = %+v, want empty when catalog meta section is absent", got)
	}
	if got := LookupStaticModelInfo("muse-spark-1.3"); got != nil {
		t.Fatalf("LookupStaticModelInfo(muse-spark-1.3) = %+v, want nil", got)
	}
}
