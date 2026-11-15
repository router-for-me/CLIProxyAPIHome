package management

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	cpaconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
)

func TestConfigV8CPAExecutionDatabaseMutations(t *testing.T) {
	db, cleanup := openManagementLogTestDB(t)
	defer cleanup()
	repo := cluster.NewRepository(db)
	handler := NewHandler(repo, nil, "127.0.0.1", 0)
	router := gin.New()
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		router.Handle(method, "/v8/management/config/*path", handler.ConfigV8)
	}
	cases := []struct{ path, value string }{
		{"server/max-request-body-bytes", "4096"},
		{"server/response-compression", `{"enabled":false,"min-bytes":512}`},
		{"server/shutdown-timeout", "20"},
		{"server/github-token", `"test-token"`},
		{"routing/session-affinity-rebind-on", `"quota"`},
		{"client/codex", `{"enable-apply-patch":true,"optimize-multi-agent-v2":true,"web-search-result-cache":{"enabled":true,"ttl":"12h","limit":42}}`},
		{"client/claude/show-codex-commentary", "true"},
		{"upstream/codex", `{"upstream-request-compression":true,"upstream-request-compression-min-bytes":2048,"websocket-compression":false}`},
		{"upstream/claude", `{"server-side-fallback":true,"header-defaults":{"native-entrypoints":["t3-code"],"anthropic-beta-strip":["old"],"anthropic-beta-append":["new"],"inject-diagnostics":false,"client-request-id":"off"}}`},
		{"oauth/settings", `{"codex":[{"name":"test-model","max-context-length":524288}]}`},
		{"oauth/providers/codex/signin-with-chatgpt-identifier", `"test-client"`},
		{"oauth/providers/antigravity", `{"tos-pause-window":"3h","sensitive-patterns-replace":[{"pattern":"^old","replacement":"new"}]}`},
		{"oauth/providers/devin/sensitive-patterns-replace", `[{"pattern":"^old","replacement":""}]`},
		{"api-keys/vertex", `[{"name":"vertex","interactions":true,"keys":[{"api-key":"vertex-key"}]}]`},
		{"api-keys/codex", `[{"name":"codex","base-url":"https://example.test","connection-pool":{"enabled":true,"idle-conn-timeout":"123s"},"keys":[{"api-key":"codex-key","connection-pool":{"enabled":false}}]}]`},
	}
	for _, tc := range cases {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodPut, "/v8/management/config/"+tc.path, strings.NewReader(tc.value)))
		if response.Code != http.StatusOK {
			t.Fatalf("PUT %s: %d %s", tc.path, response.Code, response.Body.String())
		}
		response = httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v8/management/config/"+tc.path, nil))
		if response.Code != http.StatusOK {
			t.Fatalf("GET %s: %d %s", tc.path, response.Code, response.Body.String())
		}
	}
	cfg, wire, errLoad := repo.LoadConfigAsRuntimeConfig(t.Context())
	if errLoad != nil {
		t.Fatal(errLoad)
	}
	cpa, errCPA := cpaconfig.ParseConfigBytes(wire)
	if errCPA != nil {
		t.Fatal(errCPA)
	}
	if cfg.Routing.SessionAffinityRebindOn != "quota" || !cpa.Client.Codex.EnableApplyPatch || !cfg.Codex.OptimizeMultiAgentV2 || cfg.Antigravity.TOSPauseWindow != "3h" || cfg.OAuthSettings["codex"][0].MaxContextLength != 524288 {
		t.Fatal("database runtime projection lost Home or CPA configuration")
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodDelete, "/v8/management/config/client/codex/web-search-result-cache", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("DELETE: %d %s", response.Code, response.Body.String())
	}
	cfg, wire, errLoad = repo.LoadConfigAsRuntimeConfig(t.Context())
	if errLoad != nil {
		t.Fatal(errLoad)
	}
	cpa, errCPA = cpaconfig.ParseConfigBytes(wire)
	if errCPA != nil {
		t.Fatal(errCPA)
	}
	if cpa.Client.Codex.WebSearchResultCache.Enabled || !cfg.Codex.OptimizeMultiAgentV2 {
		t.Fatalf("deleting cache settings changed other client options: %v", errLoad)
	}
}

func TestConfigV8CredentialTreeOnlyEditsArePersisted(t *testing.T) {
	db, cleanup := openManagementLogTestDB(t)
	defer cleanup()
	repo := cluster.NewRepository(db)
	handler := NewHandler(repo, nil, "127.0.0.1", 0)
	router := gin.New()
	router.PUT("/config/*path", handler.ConfigV8)
	router.GET("/config/*path", handler.ConfigV8)
	var version int64
	var credentialID string
	for _, option := range []string{`,"openapi":false`, `,"openapi":true`, ``} {
		body := `[{"name":"vertex","models":[{"name":"model","alias":"alias"` + option + `}],"keys":[{"api-key":"vertex-key"}]}]`
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodPut, "/config/api-keys/vertex", strings.NewReader(body)))
		if response.Code != http.StatusOK {
			t.Fatalf("PUT: %d %s", response.Code, response.Body.String())
		}
		auths, errList := repo.ListAuths(t.Context())
		if errList != nil || len(auths) != 1 {
			t.Fatalf("credentials=%d err=%v", len(auths), errList)
		}
		if credentialID != "" && credentialID != auths[0].ID {
			t.Fatal("execution-only edit replaced the credential identity")
		}
		credentialID = auths[0].ID
		_, record, errGet := repo.GetAuth(t.Context(), credentialID)
		if errGet != nil {
			t.Fatal(errGet)
		}
		if record.Version <= version {
			t.Fatal("tree-only change was ignored by credential reconciliation")
		}
		version = record.Version
		response = httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/config/api-keys/vertex", nil))
		if response.Code != http.StatusOK {
			t.Fatal("could not read saved credential tree")
		}
		if option == "" && strings.Contains(response.Body.String(), `"openapi"`) {
			t.Fatal("removed execution field was recreated from defaults")
		}
		if option != "" && !strings.Contains(response.Body.String(), strings.TrimPrefix(option, ",")) {
			t.Fatal("execution-only change was not exported")
		}
	}
}
