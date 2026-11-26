package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	appconfig "github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"gopkg.in/yaml.v3"
)

func TestConfigV8ModelCatalogsFullDocumentRoundTrip(t *testing.T) {
	h := newModelCatalogConfigHarness(t)
	want := h.sources()
	h.seed(want)
	view := h.document()
	view["api-keys"] = map[string]any{"codex": []any{map[string]any{
		"name":     "catalog-test",
		"base-url": "https://example.test/v1",
		"models":   []any{map[string]string{"name": "upstream-model", "alias": "public-model"}},
		"keys":     []any{map[string]string{"api-key": "fixture-key"}},
	}}}
	h.putDocument(view)
	h.assertSources(want)
	var groups []struct {
		Models []appconfig.CodexModel `json:"models"`
	}
	if errUnmarshal := json.Unmarshal(h.request(http.MethodGet, "/v8/management/config/api-keys/codex", nil, http.StatusOK), &groups); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	if len(groups) != 1 || len(groups[0].Models) != 1 || groups[0].Models[0].Name != "upstream-model" || groups[0].Models[0].Alias != "public-model" {
		t.Fatalf("credential models were confused with the models config row: %+v", groups)
	}
	view = h.document()
	models := map[string]any{"catalog": want.Catalog, "codex-catalog": want.CodexCatalog, "devin-catalog": want.DevinCatalog}
	if !reflect.DeepEqual(view["models"], models) {
		t.Fatalf("GET config.yaml models = %#v, want %#v", view["models"], models)
	}

	// A full-document replacement intentionally deletes omitted config rows.
	delete(view, "models")
	h.putDocument(view)
	if _, exists := h.snapshot()["models"]; exists {
		t.Fatal("full replacement retained omitted models row")
	}
	if _, exists := h.document()["models"]; exists {
		t.Fatal("GET config.yaml restored omitted models")
	}
	cfg, _, errLoad := h.repo.LoadConfigAsRuntimeConfig(context.Background())
	if errLoad != nil {
		t.Fatal(errLoad)
	}
	if cfg.Models != (appconfig.ModelCatalogs{}) {
		t.Fatalf("runtime retained omitted models: %+v", cfg.Models)
	}
}

func TestConfigV8ModelCatalogsScopedWrites(t *testing.T) {
	for _, tc := range []struct {
		name, method, path string
	}{
		{"put-field", http.MethodPut, "/v8/management/config/models/catalog"},
		{"patch-field", http.MethodPatch, "/v8/management/config/models/catalog"},
		{"put-subtree", http.MethodPut, "/v8/management/config/models"},
		{"patch-subtree", http.MethodPatch, "/v8/management/config/models"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newModelCatalogConfigHarness(t)
			want := h.sources()
			h.seed(want)
			before := h.snapshot()
			want.Catalog = "https://example.test/updated.json?revision=2"
			var update any = want.Catalog
			if tc.path == "/v8/management/config/models" {
				if tc.method == http.MethodPut {
					update = want
				} else {
					update = map[string]string{"catalog": want.Catalog}
				}
			}
			body, errMarshal := json.Marshal(update)
			if errMarshal != nil {
				t.Fatal(errMarshal)
			}
			h.request(tc.method, tc.path, body, http.StatusOK)
			h.assertSources(want)
			after := h.snapshot()
			if string(after["port"]) != "8317" || string(after["request-retry"]) != "3" {
				t.Fatal("scoped write lost unrelated configuration")
			}
			delete(before, "models")
			delete(after, "models")
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("scoped write changed unrelated rows: before=%v after=%v", before, after)
			}
		})
	}
}

func TestConfigV8ModelCatalogsInvalidWritesPreserveSnapshot(t *testing.T) {
	for _, mode := range []string{"full-document", "put-field", "patch-field", "put-subtree", "patch-subtree"} {
		for _, field := range []string{"catalog", "codex-catalog", "devin-catalog"} {
			t.Run(mode+"/"+field, func(t *testing.T) {
				h := newModelCatalogConfigHarness(t)
				want := h.sources()
				h.seed(want)
				view := h.document()
				before := h.snapshot()
				method, path := http.MethodPut, "/v8/management/config/models/"+field
				var update any = "relative.json"
				if strings.HasPrefix(mode, "patch-") {
					method = http.MethodPatch
				}
				if strings.HasSuffix(mode, "subtree") {
					path = "/v8/management/config/models"
					update = map[string]string{field: "relative.json"}
				}
				var body []byte
				var errMarshal error
				if mode == "full-document" {
					path = "/v8/management/config.yaml"
					view["models"].(map[string]any)[field] = "relative.json"
					body, errMarshal = yaml.Marshal(view)
				} else {
					body, errMarshal = json.Marshal(update)
				}
				if errMarshal != nil {
					t.Fatal(errMarshal)
				}
				response := h.request(method, path, body, http.StatusBadRequest)
				if !strings.Contains(string(response), "invalid_config") || !strings.Contains(string(response), "models."+field) {
					t.Fatalf("write was not rejected for its invalid catalog source: %s", response)
				}
				if after := h.snapshot(); !reflect.DeepEqual(before, after) {
					t.Fatalf("rejected write changed database: before=%v after=%v", before, after)
				}
				h.assertSources(want)
			})
		}
	}
}

func TestPutConfigYAMLModelCatalogsDatabaseRoundTrip(t *testing.T) {
	h := newModelCatalogConfigHarness(t)
	want := h.sources()
	root := map[string]any{"port": 8317, "models": want}
	body, errMarshal := yaml.Marshal(root)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	h.request(http.MethodPut, "/v0/management/config.yaml", body, http.StatusOK)
	h.assertSources(want)
	before := h.snapshot()
	root["models"] = map[string]string{"catalog": "relative.json"}
	body, errMarshal = yaml.Marshal(root)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	h.request(http.MethodPut, "/v0/management/config.yaml", body, http.StatusUnprocessableEntity)
	if after := h.snapshot(); !reflect.DeepEqual(before, after) {
		t.Fatal("rejected legacy PUT changed database")
	}
}

type modelCatalogConfigHarness struct {
	t      *testing.T
	repo   *cluster.Repository
	router *gin.Engine
}

func newModelCatalogConfigHarness(t *testing.T) *modelCatalogConfigHarness {
	t.Helper()
	db, cleanup := openManagementLogTestDB(t)
	t.Cleanup(cleanup)
	repo := cluster.NewRepository(db)
	if _, errEnsure := repo.EnsureLifecycleConfig(t.Context(), cluster.DefaultHeartbeatTimeout()); errEnsure != nil {
		t.Fatal(errEnsure)
	}
	handler := NewHandler(repo, nil, "127.0.0.1", 0)
	router := gin.New()
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodPatch} {
		router.Handle(method, "/v8/management/config", handler.ConfigV8)
		router.Handle(method, "/v8/management/config/*path", handler.ConfigV8)
	}
	router.GET("/v8/management/config.yaml", handler.ConfigV8)
	router.PUT("/v8/management/config.yaml", handler.ConfigV8)
	router.PUT("/v0/management/config.yaml", handler.PutConfigYAML)
	return &modelCatalogConfigHarness{t: t, repo: repo, router: router}
}

func (h *modelCatalogConfigHarness) request(method, path string, body []byte, status int) []byte {
	h.t.Helper()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(method, path, strings.NewReader(string(body)))
	if strings.HasSuffix(path, ".yaml") {
		request.Header.Set("Content-Type", "application/yaml")
	} else {
		request.Header.Set("Content-Type", "application/json")
	}
	h.router.ServeHTTP(recorder, request)
	if recorder.Code != status {
		h.t.Fatalf("%s %s: status=%d, want=%d, body=%s", method, path, recorder.Code, status, recorder.Body.String())
	}
	return recorder.Body.Bytes()
}

func (h *modelCatalogConfigHarness) sources() appconfig.ModelCatalogs {
	return appconfig.ModelCatalogs{
		Catalog:      "https://example.test/models.json?revision=1&format=json",
		CodexCatalog: "http://localhost/codex.json",
		DevinCatalog: filepath.Join(h.t.TempDir(), "devin models.json"),
	}
}

func (h *modelCatalogConfigHarness) snapshot() map[string]json.RawMessage {
	h.t.Helper()
	snapshot, errSnapshot := h.repo.LoadConfigSnapshot(context.Background())
	if errSnapshot != nil {
		h.t.Fatal(errSnapshot)
	}
	return snapshot
}

func (h *modelCatalogConfigHarness) document() map[string]any {
	h.t.Helper()
	var root map[string]any
	if errUnmarshal := yaml.Unmarshal(h.request(http.MethodGet, "/v8/management/config.yaml", nil, http.StatusOK), &root); errUnmarshal != nil {
		h.t.Fatal(errUnmarshal)
	}
	return root
}

func (h *modelCatalogConfigHarness) putDocument(root map[string]any) {
	h.t.Helper()
	body, errMarshal := yaml.Marshal(root)
	if errMarshal != nil {
		h.t.Fatal(errMarshal)
	}
	h.request(http.MethodPut, "/v8/management/config.yaml", body, http.StatusOK)
}

func (h *modelCatalogConfigHarness) seed(sources appconfig.ModelCatalogs) {
	h.t.Helper()
	// Preserve Home-owned revisions when replacing the complete document.
	root := h.document()
	root["models"] = sources
	root["server"] = map[string]any{"port": 8317}
	root["routing"] = map[string]any{"retry": map[string]any{"request-retry": 3}}
	h.putDocument(root)
}

func (h *modelCatalogConfigHarness) assertSources(want appconfig.ModelCatalogs) {
	h.t.Helper()
	var persisted map[string]string
	if errUnmarshal := json.Unmarshal(h.snapshot()["models"], &persisted); errUnmarshal != nil {
		h.t.Fatalf("decode models config row: %v", errUnmarshal)
	}
	models := map[string]string{"catalog": want.Catalog, "codex-catalog": want.CodexCatalog, "devin-catalog": want.DevinCatalog}
	if !reflect.DeepEqual(persisted, models) {
		h.t.Fatalf("models config row = %#v, want %#v", persisted, models)
	}
	cfg, _, errLoad := h.repo.LoadConfigAsRuntimeConfig(context.Background())
	if errLoad != nil {
		h.t.Fatal(errLoad)
	}
	if cfg.Models != want {
		h.t.Fatalf("runtime models = %+v, want %+v", cfg.Models, want)
	}
}
