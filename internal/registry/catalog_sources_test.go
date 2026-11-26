package registry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestCatalogFetcherSources(t *testing.T) {
	for _, tc := range []struct {
		name     string
		data     []byte
		validate func([]byte) error
	}{
		{"general", embeddedModelsJSON, validateCatalogBytes},
		{"codex", embeddedCodexClientModelsJSON, ValidateCodexClientModelsJSON},
		{"devin", embeddedDevinModelsJSON, validateDevinCatalogBytes},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var officialHits atomic.Int32
			official := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { officialHits.Add(1); _, _ = w.Write(tc.data) }))
			defer official.Close()
			custom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(tc.data) }))
			defer custom.Close()
			broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("invalid")) }))
			defer broken.Close()
			path := filepath.Join(t.TempDir(), "catalog.json")
			if errWrite := os.WriteFile(path, tc.data, 0600); errWrite != nil {
				t.Fatal(errWrite)
			}
			fetch := catalogFetcher(func() []string { return []string{official.URL} }, tc.validate)
			for _, source := range []string{custom.URL, path} {
				data, errFetch := fetch(context.Background(), source)
				if errFetch != nil || string(data) != string(tc.data) {
					t.Fatalf("source %s: %v", source, errFetch)
				}
			}
			for _, source := range []string{broken.URL, path + ".missing"} {
				if _, errFetch := fetch(context.Background(), source); errFetch == nil {
					t.Fatal("accepted invalid custom source")
				}
			}
			if officialHits.Load() != 0 {
				t.Fatal("custom sources fell back to official URL")
			}
			if _, errFetch := fetch(context.Background(), ""); errFetch != nil {
				t.Fatal(errFetch)
			}
			if officialHits.Load() != 1 {
				t.Fatal("default source was not fetched")
			}
			if errWrite := os.WriteFile(path, []byte("invalid"), 0600); errWrite != nil {
				t.Fatal(errWrite)
			}
			if _, errFetch := fetch(context.Background(), path); errFetch == nil {
				t.Fatal("file was not reread")
			}
		})
	}
}

func TestCatalogSourceSwitchRejectsStalePublication(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	published := make(chan string, 3)
	u := &catalogUpdater{
		generation: 1,
		fetch: func(ctx context.Context, source string) ([]byte, error) {
			if source == "old" {
				close(started)
				<-release
			}
			return []byte(source), nil
		},
		publish: func(data []byte) ([]string, error) { published <- string(data); return nil, nil },
	}
	go func() { u.refresh(ctx, "old", 1); close(done) }()
	<-started
	u.configure(ctx, "new")
	select {
	case value := <-published:
		if value != "new" {
			t.Fatalf("published %s", value)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("new source blocked behind old fetch")
	}
	close(release)
	<-done
	select {
	case value := <-published:
		t.Fatalf("stale publication: %s", value)
	default:
	}
	u.configure(ctx, "new")
	u.mu.Lock()
	generation := u.generation
	u.mu.Unlock()
	if generation != 2 {
		t.Fatal("unchanged source restarted")
	}
}

func TestCatalogRefreshKeepsLastValidData(t *testing.T) {
	last := "last valid"
	u := &catalogUpdater{
		fetch:   func(context.Context, string) ([]byte, error) { return nil, errors.New("unavailable") },
		publish: func(data []byte) ([]string, error) { last = string(data); return nil, nil },
	}
	u.refresh(context.Background(), "", 0)
	if last != "last valid" {
		t.Fatal("failed fetch changed catalog")
	}
	u.fetch = func(context.Context, string) ([]byte, error) { return []byte("invalid"), nil }
	u.publish = func([]byte) ([]string, error) { return nil, errors.New("invalid") }
	u.refresh(context.Background(), "", 0)
	if last != "last valid" {
		t.Fatal("invalid catalog changed data")
	}
}

func TestReadCatalogSource(t *testing.T) {
	data := []byte(`{"catalog":"test"}`)
	path := filepath.Join(t.TempDir(), "catalog.json")
	if errWrite := os.WriteFile(path, data, 0600); errWrite != nil {
		t.Fatal(errWrite)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(data)
	}))
	t.Cleanup(server.Close)
	for _, source := range []string{path, server.URL} {
		got, errRead := readCatalogSource(context.Background(), source)
		if errRead != nil || !bytes.Equal(got, data) {
			t.Fatalf("readCatalogSource(%q) = %q, %v", source, got, errRead)
		}
	}
	oversized := bytes.Repeat([]byte("x"), maxCodexClientModelsSize+1)
	if errWrite := os.WriteFile(path, oversized, 0600); errWrite != nil {
		t.Fatal(errWrite)
	}
	largeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(oversized)
	}))
	t.Cleanup(largeServer.Close)
	failedServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(failedServer.Close)
	for _, source := range []string{path, largeServer.URL, failedServer.URL, path + ".missing", t.TempDir()} {
		if _, errRead := readCatalogSource(context.Background(), source); errRead == nil {
			t.Fatalf("accepted invalid source %q", source)
		}
	}
}

func TestReadCatalogSourceTimeout(t *testing.T) {
	oldTimeout := modelsFetchTimeout
	modelsFetchTimeout = 100 * time.Millisecond
	t.Cleanup(func() { modelsFetchTimeout = oldTimeout })
	for _, phase := range []string{"headers", "body"} {
		t.Run(phase, func(t *testing.T) {
			started := make(chan struct{})
			release := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if phase == "body" {
					w.WriteHeader(http.StatusOK)
					w.(http.Flusher).Flush()
				}
				close(started)
				select {
				case <-r.Context().Done():
				case <-release:
				}
			}))
			t.Cleanup(func() {
				close(release)
				server.Close()
			})
			done := make(chan error, 1)
			go func() {
				_, errRead := readCatalogSource(context.Background(), server.URL)
				done <- errRead
			}()
			select {
			case <-started:
			case <-time.After(2 * time.Second):
				t.Fatal("catalog request did not reach server")
			}
			select {
			case errRead := <-done:
				if !errors.Is(errRead, context.DeadlineExceeded) {
					t.Fatalf("read error = %v, want internal deadline exceeded", errRead)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("catalog download exceeded internal timeout")
			}
		})
	}
}

func TestCatalogFetcherTimeoutUsesBackup(t *testing.T) {
	oldTimeout := modelsFetchTimeout
	modelsFetchTimeout = 100 * time.Millisecond
	t.Cleanup(func() { modelsFetchTimeout = oldTimeout })
	for _, phase := range []string{"headers", "body"} {
		t.Run(phase, func(t *testing.T) {
			release := make(chan struct{})
			hanging := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if phase == "body" {
					w.WriteHeader(http.StatusOK)
					w.(http.Flusher).Flush()
				}
				select {
				case <-r.Context().Done():
				case <-release:
				}
			}))
			t.Cleanup(func() {
				close(release)
				hanging.Close()
			})
			backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write(embeddedDevinModelsJSON)
			}))
			t.Cleanup(backup.Close)
			fetch := catalogFetcher(func() []string { return []string{hanging.URL, backup.URL} }, validateDevinCatalogBytes)
			done := make(chan error, 1)
			go func() {
				data, errFetch := fetch(context.Background(), "")
				if errFetch == nil && !bytes.Equal(data, embeddedDevinModelsJSON) {
					errFetch = errors.New("backup catalog was not returned")
				}
				done <- errFetch
			}()
			select {
			case errFetch := <-done:
				if errFetch != nil {
					t.Fatal(errFetch)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("timed-out primary blocked fallback")
			}
		})
	}
}

func TestCatalogSourcesValidate(t *testing.T) {
	for _, source := range []string{"", filepath.Join(t.TempDir(), "models.json"), "https://example.com/models.json", "http://localhost/models.json", "relative.json", "./models.json", "~/models.json", "ftp://example.com/models", "file:///tmp/models.json", "https:///models"} {
		valid := source == "" || filepath.IsAbs(source) || source == "https://example.com/models.json" || source == "http://localhost/models.json"
		for _, sources := range []CatalogSources{{Catalog: source}, {CodexCatalog: source}, {DevinCatalog: source}} {
			if errValidate := sources.Validate(); (errValidate == nil) != valid {
				t.Errorf("Validate(%+v) = %v, want valid=%t", sources, errValidate, valid)
			}
		}
	}
}

func TestPublishCatalogPreservesOptionalSections(t *testing.T) {
	original := getModels()
	t.Cleanup(func() {
		modelsCatalogStore.mu.Lock()
		modelsCatalogStore.data = original
		modelsCatalogStore.mu.Unlock()
	})
	old := *original
	old.Devin = []*ModelInfo{{ID: "devin/preserved"}}
	old.Meta = []*ModelInfo{{ID: "meta/preserved"}}
	old.Copilot = []*ModelInfo{{ID: "copilot-preserved"}}
	modelsCatalogStore.mu.Lock()
	modelsCatalogStore.data = &old
	modelsCatalogStore.mu.Unlock()
	incoming := old
	incoming.Devin, incoming.Meta, incoming.Copilot = nil, nil, nil
	data, errMarshal := json.Marshal(&incoming)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	changed, errPublish := publishCatalogBytes(data)
	if errPublish != nil {
		t.Fatal(errPublish)
	}
	got := getModels()
	if len(changed) != 0 ||
		len(got.Devin) != 1 || got.Devin[0].ID != "devin/preserved" ||
		len(got.Meta) != 1 || got.Meta[0].ID != "meta/preserved" ||
		len(got.Copilot) != 1 || got.Copilot[0].ID != "copilot-preserved" {
		t.Fatalf("optional sections not preserved: Devin=%+v Meta=%+v Copilot=%+v changed=%v", got.Devin, got.Meta, got.Copilot, changed)
	}
	if _, errPublish := publishCatalogBytes([]byte(`{}`)); errPublish == nil || getModels() != got {
		t.Fatal("invalid catalog replaced last valid catalog")
	}
}
