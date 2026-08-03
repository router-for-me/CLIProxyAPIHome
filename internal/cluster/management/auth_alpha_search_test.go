package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
)

func TestCodexAPIKeyAlphaSearchManagementRoundTrip(t *testing.T) {
	db, cleanup := openManagementLogTestDB(t)
	defer cleanup()
	repo := cluster.NewRepository(db)
	handler := NewHandler(repo, nil, "127.0.0.1", 0)
	engine := gin.New()
	engine.PUT("/codex-api-key", handler.PutCodexKeys)
	engine.PATCH("/codex-api-key", handler.PatchCodexKey)
	engine.GET("/codex-api-key", handler.GetCodexKeys)

	putResp := httptest.NewRecorder()
	engine.ServeHTTP(putResp, httptest.NewRequest(http.MethodPut, "/codex-api-key", strings.NewReader(`[{"api-key":"codex-key","base-url":"https://codex.example/v1","alpha-search":true}]`)))
	if putResp.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, body=%s", putResp.Code, putResp.Body.String())
	}
	auths, errAuths := repo.ListAuths(context.Background())
	if errAuths != nil {
		t.Fatalf("ListAuths() error = %v", errAuths)
	}
	if len(auths) != 1 || auths[0].Attributes[coreauth.AttributeCodexAlphaSearch] != "true" {
		t.Fatalf("persisted auths = %#v, want codex_alpha_search=true", auths)
	}

	assertAlphaSearch := func(want bool) {
		t.Helper()
		getResp := httptest.NewRecorder()
		engine.ServeHTTP(getResp, httptest.NewRequest(http.MethodGet, "/codex-api-key", nil))
		if getResp.Code != http.StatusOK {
			t.Fatalf("GET status = %d, body=%s", getResp.Code, getResp.Body.String())
		}
		var payload map[string][]map[string]any
		if errDecode := json.Unmarshal(getResp.Body.Bytes(), &payload); errDecode != nil {
			t.Fatalf("decode GET response: %v", errDecode)
		}
		entries := payload["codex-api-key"]
		if len(entries) != 1 {
			t.Fatalf("GET entries = %#v", entries)
		}
		got, _ := entries[0]["alpha-search"].(bool)
		if got != want {
			t.Fatalf("GET alpha-search = %t, want %t; body=%s", got, want, getResp.Body.String())
		}
	}
	assertAlphaSearch(true)

	patchResp := httptest.NewRecorder()
	engine.ServeHTTP(patchResp, httptest.NewRequest(http.MethodPatch, "/codex-api-key", strings.NewReader(`{"match":"codex-key","value":{"alpha-search":false}}`)))
	if patchResp.Code != http.StatusOK {
		t.Fatalf("PATCH status = %d, body=%s", patchResp.Code, patchResp.Body.String())
	}
	assertAlphaSearch(false)
}
