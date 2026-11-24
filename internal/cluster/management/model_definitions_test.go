package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
)

func TestCredentialModelsV8ExclusionCandidates(t *testing.T) {
	auth := &coreauth.Auth{ID: t.Name(), Index: t.Name(), FileName: "candidate.json", Provider: "claude"}
	handler, _ := newCooldownManagementHandler(t, auth)
	modelRegistry := registry.GetGlobalRegistry()
	t.Cleanup(func() { modelRegistry.UnregisterClient(auth.ID) })
	modelRegistry.RegisterClientWithCandidates(auth.ID, "claude",
		[]*registry.ModelInfo{{ID: "team/alias"}},
		[]*registry.ModelInfo{{ID: "source-model"}, {ID: "excluded-model"}},
	)
	engine := gin.New()
	engine.GET("/v8/management/credentials/models", handler.GetCredentialModelsV8)
	engine.GET("/v0/management/auth-files/models", handler.GetAuthFileModels)
	read := func(path string) map[string]json.RawMessage {
		t.Helper()
		recorder := httptest.NewRecorder()
		engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path+"?name="+auth.ID, nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("status=%d, body=%s", recorder.Code, recorder.Body.String())
		}
		var body map[string]json.RawMessage
		if errDecode := json.Unmarshal(recorder.Body.Bytes(), &body); errDecode != nil {
			t.Fatal(errDecode)
		}
		return body
	}
	v8 := read("/v8/management/credentials/models")
	if string(v8["models"]) != `[{"id":"team/alias"}]` || string(v8["exclusion_models"]) != `[{"id":"source-model"},{"id":"excluded-model"}]` {
		t.Fatalf("unexpected v8 model catalog: %s", v8)
	}
	if _, exists := read("/v0/management/auth-files/models")["exclusion_models"]; exists {
		t.Fatal("v0 unexpectedly exposes exclusion candidates")
	}
	modelRegistry.RegisterClient(auth.ID, "claude", []*registry.ModelInfo{{ID: "plugin-model"}})
	if got := string(read("/v8/management/credentials/models")["exclusion_models"]); got != "null" {
		t.Fatalf("missing candidate snapshot must be null, got %s", got)
	}
	modelRegistry.RegisterClientWithCandidates(auth.ID, "claude", nil, []*registry.ModelInfo{})
	if got := string(read("/v8/management/credentials/models")["exclusion_models"]); got != "[]" {
		t.Fatalf("known empty catalog must be [], got %s", got)
	}
}

func TestGetStaticModelDefinitionsNamespacesCopilotAndDevin(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := NewHandler(nil, nil, "", 0)
	engine := gin.New()
	engine.GET("/v8/management/routing/model-definitions/:channel", handler.GetStaticModelDefinitions)
	engine.GET("/v8/management/models", handler.GetModelsV8)

	fetchDefs := func(path string) []registry.ModelInfo {
		t.Helper()
		recorder := httptest.NewRecorder()
		engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("GET %s status=%d body=%s", path, recorder.Code, recorder.Body.String())
		}
		var body struct {
			Models []registry.ModelInfo `json:"models"`
		}
		if errDecode := json.Unmarshal(recorder.Body.Bytes(), &body); errDecode != nil {
			t.Fatalf("decode %s: %v", path, errDecode)
		}
		if len(body.Models) == 0 {
			t.Fatalf("GET %s returned no models", path)
		}
		return body.Models
	}

	for _, tc := range []struct {
		query, channel string
	}{
		{"Copilot", "copilot"},
		{"Devin", "devin"},
	} {
		prefix := tc.channel + "/"
		routingDefs := fetchDefs("/v8/management/routing/model-definitions/" + tc.query)
		staticCatalog := fetchDefs("/v8/management/models?scope=static&channel=" + tc.channel)
		if len(routingDefs) != len(staticCatalog) {
			t.Fatalf("%s lengths: routing=%d static=%d", tc.channel, len(routingDefs), len(staticCatalog))
		}
		for i := range routingDefs {
			if strings.HasPrefix(staticCatalog[i].ID, prefix) {
				t.Fatalf("%s scope=static unexpectedly prefixed: %q", tc.channel, staticCatalog[i].ID)
			}
			if want := prefix + staticCatalog[i].ID; routingDefs[i].ID != want {
				t.Fatalf("%s routing model[%d] = %q, want %q", tc.channel, i, routingDefs[i].ID, want)
			}
		}
	}

	claudeDefs := fetchDefs("/v8/management/routing/model-definitions/claude")
	if strings.HasPrefix(claudeDefs[0].ID, "claude/") {
		t.Fatalf("claude model unexpectedly prefixed: %q", claudeDefs[0].ID)
	}
}
