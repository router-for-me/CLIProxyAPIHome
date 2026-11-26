package management

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	cpaconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
)

func TestConfigV8PreservesLegacyCodexOptimizationSnapshot(t *testing.T) {
	db, cleanup := openManagementLogTestDB(t)
	defer cleanup()
	repo := cluster.NewRepository(db)
	if errInit := repo.ReplaceConfigSnapshot(context.Background(), map[string]any{}); errInit != nil {
		t.Fatal(errInit)
	}
	// Seed the historical database shape directly, bypassing current normalization.
	if errSeed := db.Create(&cluster.ConfigRecord{
		Key: "codex", Value: cluster.JSONB(`{"optimize-multi-agent-v2":true}`),
	}).Error; errSeed != nil {
		t.Fatal(errSeed)
	}
	handler := NewHandler(repo, nil, "127.0.0.1", 0)
	router := gin.New()
	router.GET("/v8/management/config.yaml", handler.ConfigV8)
	router.GET("/v8/management/config/*path", handler.ConfigV8)
	router.PATCH("/v8/management/config/*path", handler.ConfigV8)
	request := func(method, route, body string) []byte {
		t.Helper()
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(method, "/v8/management/"+route, strings.NewReader(body)))
		if response.Code != http.StatusOK {
			t.Fatalf("%s %s: status=%d body=%s", method, route, response.Code, response.Body.String())
		}
		return response.Body.Bytes()
	}

	for round := range 2 {
		_, wire, errLoad := repo.LoadConfigAsRuntimeConfig(context.Background())
		if errLoad != nil {
			t.Fatal(errLoad)
		}
		cpa, errParse := cpaconfig.ParseConfigBytes(wire)
		if errParse != nil {
			t.Fatal(errParse)
		}
		if !cpa.Client.Codex.OptimizeMultiAgentV2 {
			t.Fatalf("snapshot round %d lost enabled Codex optimization", round)
		}
		exported := request(http.MethodGet, "config.yaml", "")
		if errValidate := config.ValidateV8Config(exported); errValidate != nil {
			t.Fatal(errValidate)
		}
		cpaExport, errExport := cpaconfig.ParseConfigBytes(exported)
		if errExport != nil {
			t.Fatal(errExport)
		}
		if !cpaExport.Client.Codex.OptimizeMultiAgentV2 {
			t.Fatalf("V8 export round %d lost enabled Codex optimization", round)
		}
		if round == 0 {
			request(http.MethodPatch, "config/server", `{"port":8327}`)
		}
	}
	if value := strings.TrimSpace(string(request(http.MethodGet, "config/client/codex/optimize-multi-agent-v2", ""))); value != "true" {
		t.Fatalf("canonical V8 optimization value = %s, want true", value)
	}
}
