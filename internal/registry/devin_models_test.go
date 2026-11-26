package registry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestValidateDevinModelsJSON(t *testing.T) {
	t.Run("legacy prefixed id is normalized to bare", func(t *testing.T) {
		models, err := ValidateDevinModelsJSON([]byte(`{"devin":[{"id":"devin/swe-2","display_name":"SWE-2"}]}`))
		if err != nil {
			t.Fatalf("ValidateDevinModelsJSON() error = %v", err)
		}
		if len(models) != 1 || models[0].ID != "swe-2" || models[0].Type != "devin" {
			t.Fatalf("models = %+v", models)
		}
	})
	t.Run("bare id stays bare", func(t *testing.T) {
		models, err := ValidateDevinModelsJSON([]byte(`{"devin":[{"id":"swe-2"}]}`))
		if err != nil {
			t.Fatalf("ValidateDevinModelsJSON() error = %v", err)
		}
		if len(models) != 1 || models[0].ID != "swe-2" {
			t.Fatalf("models = %+v", models)
		}
	})
	t.Run("duplicate id across bare and legacy prefixed", func(t *testing.T) {
		if _, err := ValidateDevinModelsJSON([]byte(`{"devin":[{"id":"devin/swe-2"},{"id":"SWE-2"}]}`)); err == nil {
			t.Fatal("expected duplicate id error")
		}
	})
}

func TestEmbeddedDevinModelsLoadedOnStartup(t *testing.T) {
	models := GetDevinModels()
	if len(models) < 30 {
		t.Fatalf("GetDevinModels() count = %d, want at least 30", len(models))
	}
	found := make(map[string]bool, len(models))
	for _, model := range models {
		if model != nil {
			found[model.ID] = true
		}
	}
	for _, id := range []string{"swe-2", "claude-fable-5-1", "gpt-6-astra"} {
		if !found[id] {
			t.Errorf("embedded catalog missing %q", id)
		}
	}
}

func TestDevinModelsRemoteFetchFallback(t *testing.T) {
	updater := newDevinCatalogTestUpdater(t)

	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "server error", http.StatusInternalServerError)
	}))
	t.Cleanup(failing.Close)
	devinModelsURLs = []string{failing.URL + "/devin_models.json"}

	initialModels := GetDevinModels()
	initialCount := len(initialModels)
	if initialCount == 0 {
		t.Fatal("expected non-empty initial Devin models")
	}
	updater.refresh(context.Background(), "", 0)
	if got := GetDevinModels(); !reflect.DeepEqual(got, initialModels) {
		t.Fatal("failed refresh changed catalog")
	}

	valid := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"devin":[{"id":"devin/custom-test-model","display_name":"Custom Test Model"}]}`))
	}))
	t.Cleanup(valid.Close)
	devinModelsURLs = []string{valid.URL + "/devin_models.json"}
	updater.refresh(context.Background(), "", 0)

	updated := GetDevinModels()
	if len(updated) != 1 || updated[0].ID != "custom-test-model" {
		t.Fatalf("updated catalog = %+v", updated)
	}
}

func TestDevinModelsRemoteSkipsInvalidPrimaryAndUsesBackup(t *testing.T) {
	updater := newDevinCatalogTestUpdater(t)

	invalid := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("<html>not a catalog</html>"))
	}))
	t.Cleanup(invalid.Close)
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"devin":[{"id":"devin/backup-model","display_name":"Backup Model"}]}`))
	}))
	t.Cleanup(backup.Close)

	devinModelsURLs = []string{invalid.URL + "/devin_models.json", backup.URL + "/devin_models.json"}
	updater.refresh(context.Background(), "", 0)

	updated := GetDevinModels()
	if len(updated) != 1 || updated[0].ID != "backup-model" {
		t.Fatalf("updated catalog = %+v, want backup-model from second source", updated)
	}
}

// newDevinCatalogTestUpdater exercises production fetch/publication synchronously
// without starting goroutines or changing process-wide updater generations.
func newDevinCatalogTestUpdater(t *testing.T) *catalogUpdater {
	t.Helper()
	oldURLs := devinModelsURLs
	devinCatalogStore.mu.RLock()
	oldModels, oldData, oldRevision := devinCatalogStore.models, devinCatalogStore.rawJSON, devinCatalogStore.revision
	devinCatalogStore.mu.RUnlock()
	refreshCallbackMu.Lock()
	oldCallback, oldPending := refreshCallback, pendingRefreshChanges
	refreshCallback, pendingRefreshChanges = nil, nil
	refreshCallbackMu.Unlock()
	t.Cleanup(func() {
		devinModelsURLs = oldURLs
		devinCatalogStore.mu.Lock()
		devinCatalogStore.models, devinCatalogStore.rawJSON, devinCatalogStore.revision = oldModels, oldData, oldRevision
		devinCatalogStore.mu.Unlock()
		refreshCallbackMu.Lock()
		refreshCallback, pendingRefreshChanges = oldCallback, oldPending
		refreshCallbackMu.Unlock()
	})
	return &catalogUpdater{fetch: devinCatalogUpdater.fetch, publish: devinCatalogUpdater.publish}
}
