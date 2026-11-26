package management

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
)

func TestConfigV8SessionAffinitySubagentsRoundTrip(t *testing.T) {
	db, cleanup := openManagementLogTestDB(t)
	defer cleanup()
	repo := cluster.NewRepository(db)
	handler := NewHandler(repo, nil, "127.0.0.1", 0)
	router := gin.New()
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		router.Handle(method, "/v8/management/config/*path", handler.ConfigV8)
	}
	const path = "/v8/management/config/routing/session-affinity-subagents"
	for _, value := range []string{"false", "true"} {
		request := httptest.NewRequest(http.MethodPut, path, strings.NewReader(value))
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("PUT %s: status=%d body=%s", value, response.Code, response.Body.String())
		}
		response = httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusOK || strings.TrimSpace(response.Body.String()) != value {
			t.Fatalf("GET: status=%d body=%s, want %s", response.Code, response.Body.String(), value)
		}
		cfg, _, errLoad := repo.LoadConfigAsRuntimeConfig(t.Context())
		if errLoad != nil {
			t.Fatal(errLoad)
		}
		if cfg.Routing.SessionAffinitySubagents == nil || *cfg.Routing.SessionAffinitySubagents != (value == "true") {
			t.Fatalf("runtime lost explicit %s: %+v", value, cfg.Routing)
		}
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodDelete, path, nil))
	if response.Code != http.StatusOK {
		t.Fatalf("DELETE: status=%d body=%s", response.Code, response.Body.String())
	}
	cfg, _, errLoad := repo.LoadConfigAsRuntimeConfig(t.Context())
	if errLoad != nil {
		t.Fatal(errLoad)
	}
	if cfg.Routing.SessionAffinitySubagents != nil {
		t.Fatal("deleting the setting did not restore the default")
	}
}
