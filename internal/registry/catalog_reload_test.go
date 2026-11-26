package registry

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestCatalogSourcesHotReload(t *testing.T) {
	oldGeneral, oldCodex, oldDevin := generalCatalogUpdater, codexCatalogUpdater, devinCatalogUpdater
	catalogRuntime.Lock()
	oldCtx := catalogRuntime.ctx
	catalogRuntime.ctx = nil
	catalogRuntime.Unlock()
	oldModels := getModels()
	codexClientCatalogStore.mu.RLock()
	oldCodexData, oldCodexRevision := codexClientCatalogStore.data, codexClientCatalogStore.revision
	codexClientCatalogStore.mu.RUnlock()
	devinCatalogStore.mu.RLock()
	oldDevinModels, oldDevinData, oldDevinRevision := devinCatalogStore.models, devinCatalogStore.rawJSON, devinCatalogStore.revision
	devinCatalogStore.mu.RUnlock()

	// Exercise real fetchers and publishers without touching callback state.
	isolated := func(original *catalogUpdater) *catalogUpdater {
		return &catalogUpdater{
			fetch: original.fetch,
			publish: func(data []byte) ([]string, error) {
				_, errPublish := original.publish(data)
				return nil, errPublish
			},
		}
	}
	generalCatalogUpdater, codexCatalogUpdater, devinCatalogUpdater = isolated(oldGeneral), isolated(oldCodex), isolated(oldDevin)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		// Fence any publication already in progress before restoring global stores.
		for _, updater := range []*catalogUpdater{generalCatalogUpdater, codexCatalogUpdater, devinCatalogUpdater} {
			updater.mu.Lock()
			updater.mu.Unlock()
		}
		modelsCatalogStore.mu.Lock()
		modelsCatalogStore.data = oldModels
		modelsCatalogStore.mu.Unlock()
		codexClientCatalogStore.mu.Lock()
		codexClientCatalogStore.data, codexClientCatalogStore.revision = oldCodexData, oldCodexRevision
		codexClientCatalogStore.mu.Unlock()
		devinCatalogStore.mu.Lock()
		devinCatalogStore.models, devinCatalogStore.rawJSON, devinCatalogStore.revision = oldDevinModels, oldDevinData, oldDevinRevision
		devinCatalogStore.mu.Unlock()
		generalCatalogUpdater, codexCatalogUpdater, devinCatalogUpdater = oldGeneral, oldCodex, oldDevin
		catalogRuntime.Lock()
		catalogRuntime.ctx = oldCtx
		catalogRuntime.Unlock()
	})
	writeSources := func(label string) CatalogSources {
		t.Helper()
		var general staticModelsJSON
		if errUnmarshal := json.Unmarshal(embeddedModelsJSON, &general); errUnmarshal != nil {
			t.Fatal(errUnmarshal)
		}
		general.Claude[0].ID = "claude-" + label
		var codex codexClientModelsPayload
		if errUnmarshal := json.Unmarshal(embeddedCodexClientModelsJSON, &codex); errUnmarshal != nil {
			t.Fatal(errUnmarshal)
		}
		codex.Models[0]["slug"] = "codex-" + label
		write := func(name string, value any) string {
			t.Helper()
			data, errMarshal := json.Marshal(value)
			if errMarshal != nil {
				t.Fatal(errMarshal)
			}
			path := filepath.Join(t.TempDir(), name+".json")
			if errWrite := os.WriteFile(path, data, 0600); errWrite != nil {
				t.Fatal(errWrite)
			}
			return path
		}
		return CatalogSources{
			Catalog:      write("general", general),
			CodexCatalog: write("codex", codex),
			DevinCatalog: write("devin", map[string]any{"devin": []map[string]string{{"id": "devin/" + label}}}),
		}
	}
	await := func(label string) {
		t.Helper()
		timer := time.NewTimer(5 * time.Second)
		defer timer.Stop()
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			var codex codexClientModelsPayload
			data, _ := GetCodexClientModelsSnapshot()
			if errUnmarshal := json.Unmarshal(data, &codex); errUnmarshal != nil {
				t.Fatal(errUnmarshal)
			}
			devin := GetDevinModels()
			if getModels().Claude[0].ID == "claude-"+label && len(codex.Models) > 0 && codex.Models[0]["slug"] == "codex-"+label && len(devin) == 1 && devin[0].ID == label {
				return
			}
			select {
			case <-timer.C:
				t.Fatalf("catalogs did not switch to %s", label)
			case <-ticker.C:
			}
		}
	}
	a, b := writeSources("a"), writeSources("b")
	UpdateModelCatalogSources(a)
	if generalCatalogUpdater.generation != 0 {
		t.Fatal("reload before startup started readers")
	}
	StartModelCatalogUpdaters(ctx, a)
	await("a")
	UpdateModelCatalogSources(b)
	await("b")

	devinCatalogUpdater.mu.Lock()
	generation := devinCatalogUpdater.generation
	devinCatalogUpdater.mu.Unlock()
	UpdateModelCatalogSources(b)
	devinCatalogUpdater.mu.Lock()
	unchangedGeneration := devinCatalogUpdater.generation
	devinCatalogUpdater.mu.Unlock()
	if generation != unchangedGeneration {
		t.Fatal("unchanged sources restarted readers")
	}
	if errWrite := os.WriteFile(b.DevinCatalog, []byte("invalid"), 0600); errWrite != nil {
		t.Fatal(errWrite)
	}
	devinCatalogUpdater.refresh(ctx, b.DevinCatalog, generation)
	if got := GetDevinModels(); len(got) != 1 || got[0].ID != "b" {
		t.Fatal("failed file refresh replaced last valid catalog")
	}
	if errWrite := os.WriteFile(b.DevinCatalog, []byte(`{"devin":[{"id":"devin/reread"}]}`), 0600); errWrite != nil {
		t.Fatal(errWrite)
	}
	devinCatalogUpdater.refresh(ctx, b.DevinCatalog, generation)
	if got := GetDevinModels(); len(got) != 1 || got[0].ID != "reread" {
		t.Fatal("file refresh did not reread source")
	}
	cancel()
	UpdateModelCatalogSources(a)
	devinCatalogUpdater.mu.Lock()
	stoppedGeneration := devinCatalogUpdater.generation
	devinCatalogUpdater.mu.Unlock()
	if stoppedGeneration != generation {
		t.Fatal("reload after shutdown restarted readers")
	}
}

func TestDevinCatalogRefreshNotifies(t *testing.T) {
	devinCatalogStore.mu.RLock()
	oldModels, oldData, oldRevision := devinCatalogStore.models, devinCatalogStore.rawJSON, devinCatalogStore.revision
	devinCatalogStore.mu.RUnlock()
	refreshCallbackMu.Lock()
	oldCallback, oldPending := refreshCallback, pendingRefreshChanges
	var notifications [][]string
	refreshCallback = func(changed []string) { notifications = append(notifications, changed) }
	pendingRefreshChanges = nil
	refreshCallbackMu.Unlock()
	t.Cleanup(func() {
		devinCatalogStore.mu.Lock()
		devinCatalogStore.models, devinCatalogStore.rawJSON, devinCatalogStore.revision = oldModels, oldData, oldRevision
		devinCatalogStore.mu.Unlock()
		refreshCallbackMu.Lock()
		refreshCallback, pendingRefreshChanges = oldCallback, oldPending
		refreshCallbackMu.Unlock()
	})
	u := &catalogUpdater{
		fetch: func(context.Context, string) ([]byte, error) {
			return []byte(`{"devin":[{"id":"devin/notify"}]}`), nil
		},
		publish: devinCatalogUpdater.publish,
	}
	u.refresh(context.Background(), "", 0)
	u.refresh(context.Background(), "", 0)
	if !reflect.DeepEqual(notifications, [][]string{{"devin"}}) {
		t.Fatalf("notifications = %v, want exactly one Devin notification", notifications)
	}
}
