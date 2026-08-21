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

func TestXAIAPIKeyManagementCRUD(t *testing.T) {
	db, cleanup := openManagementLogTestDB(t)
	defer cleanup()

	repo := cluster.NewRepository(db)
	handler := NewHandler(repo, nil, "127.0.0.1", 0)
	engine := gin.New()
	engine.GET("/xai-api-key", handler.GetXAIKeys)
	engine.PUT("/xai-api-key", handler.PutXAIKeys)
	engine.PATCH("/xai-api-key", handler.PatchXAIKey)
	engine.DELETE("/xai-api-key", handler.DeleteXAIKey)

	putBody := `[{
        "api-key":"xai-key",
        "priority":7,
        "prefix":"grok",
        "base-url":"https://api.x.ai/v1",
        "websockets":true,
        "proxy-url":"socks5://proxy.example:1080",
        "headers":{"X-Test":"value"},
        "models":[{
            "name":"grok-4.5",
            "alias":"grok-latest",
            "display-name":"Grok Latest",
            "force-mapping":true
        }],
        "excluded-models":["grok-3-*"],
        "disable-cooling":true,
        "request-retry":2
    }]`
	putResp := httptest.NewRecorder()
	putReq := httptest.NewRequest(http.MethodPut, "/xai-api-key", strings.NewReader(putBody))
	putReq.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(putResp, putReq)
	if putResp.Code != http.StatusOK {
		t.Fatalf("PUT status = %d body=%s", putResp.Code, putResp.Body.String())
	}

	auths, errAuths := repo.ListAuths(context.Background())
	if errAuths != nil {
		t.Fatalf("ListAuths() error = %v", errAuths)
	}
	if len(auths) != 1 || auths[0].Provider != "xai" || !strings.HasPrefix(auths[0].Attributes["source"], "config:xai[") {
		t.Fatalf("stored auths = %#v", auths)
	}
	staleActive := auths[0].Clone()
	auths[0].Disabled = true
	auths[0].Status = coreauth.StatusDisabled
	if _, errUpsert := repo.UpsertAuth(context.Background(), auths[0], "update"); errUpsert != nil {
		t.Fatalf("UpsertAuth(disabled) error = %v", errUpsert)
	}
	if _, errUpsert := repo.UpsertAuthPreservingDisabled(context.Background(), staleActive, "update"); errUpsert != nil {
		t.Fatalf("UpsertAuthPreservingDisabled(stale active) error = %v", errUpsert)
	}
	storedAuth, _, errGet := repo.GetAuth(context.Background(), auths[0].ID)
	if errGet != nil {
		t.Fatalf("GetAuth() error = %v", errGet)
	}
	if !storedAuth.Disabled || storedAuth.Status != coreauth.StatusDisabled {
		t.Fatalf("stale active update re-enabled auth: %#v", storedAuth)
	}
	staleDisabled := storedAuth.Clone()
	enabled := storedAuth.Clone()
	enabled.Disabled = false
	enabled.Status = coreauth.StatusActive
	enabled.StatusMessage = ""
	if _, errUpsert := repo.UpsertAuth(context.Background(), enabled, "update"); errUpsert != nil {
		t.Fatalf("UpsertAuth(enabled) error = %v", errUpsert)
	}
	if _, errUpsert := repo.UpsertAuthPreservingDisabled(context.Background(), staleDisabled, "update"); errUpsert != nil {
		t.Fatalf("UpsertAuthPreservingDisabled(stale disabled) error = %v", errUpsert)
	}
	storedAuth, _, errGet = repo.GetAuth(context.Background(), auths[0].ID)
	if errGet != nil {
		t.Fatalf("GetAuth(enabled) error = %v", errGet)
	}
	if storedAuth.Disabled || storedAuth.Status != coreauth.StatusActive {
		t.Fatalf("stale disabled update re-disabled auth: %#v", storedAuth)
	}
	storedAuth.Disabled = true
	storedAuth.Status = coreauth.StatusDisabled
	if _, errUpsert := repo.UpsertAuth(context.Background(), storedAuth, "update"); errUpsert != nil {
		t.Fatalf("UpsertAuth(re-disabled) error = %v", errUpsert)
	}

	item := getXAIAPIKeyItem(t, engine)
	if item["api-key"] != "xai-key" || item["base-url"] != "https://api.x.ai/v1" || item["websockets"] != true {
		t.Fatalf("GET xAI fields = %#v", item)
	}
	if item["disable-cooling"] != true || item["disabled"] != true {
		t.Fatalf("GET disable-cooling/disabled = %#v/%#v", item["disable-cooling"], item["disabled"])
	}
	if item["request-retry"] != float64(2) {
		t.Fatalf("GET request-retry = %#v, want 2", item["request-retry"])
	}
	models, okModels := item["models"].([]any)
	if !okModels || len(models) != 1 {
		t.Fatalf("GET models = %#v", item["models"])
	}
	model, _ := models[0].(map[string]any)
	if model["name"] != "grok-4.5" || model["alias"] != "grok-latest" || model["display-name"] != "Grok Latest" || model["force-mapping"] != true {
		t.Fatalf("GET model = %#v", model)
	}

	patchResp := httptest.NewRecorder()
	patchReq := httptest.NewRequest(http.MethodPatch, "/xai-api-key", strings.NewReader(`{"match":"xai-key","value":{"priority":11,"websockets":false,"disable-cooling":false}}`))
	patchReq.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(patchResp, patchReq)
	if patchResp.Code != http.StatusOK {
		t.Fatalf("PATCH status = %d body=%s", patchResp.Code, patchResp.Body.String())
	}
	item = getXAIAPIKeyItem(t, engine)
	if item["priority"] != float64(11) {
		t.Fatalf("PATCH priority = %#v, want 11", item["priority"])
	}
	if _, exists := item["websockets"]; exists {
		t.Fatalf("PATCH websockets still present: %#v", item)
	}
	if item["disabled"] != true {
		t.Fatalf("PATCH unexpectedly enabled credential: %#v", item)
	}
	if item["disable-cooling"] != false {
		t.Fatalf("PATCH disable-cooling = %#v, want false", item["disable-cooling"])
	}
	if item["request-retry"] != float64(2) {
		t.Fatalf("PATCH unrelated fields lost request-retry: %#v", item)
	}

	clearRetryResp := httptest.NewRecorder()
	clearRetryReq := httptest.NewRequest(http.MethodPatch, "/xai-api-key", strings.NewReader(`{"match":"xai-key","value":{"request-retry":null}}`))
	clearRetryReq.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(clearRetryResp, clearRetryReq)
	if clearRetryResp.Code != http.StatusOK {
		t.Fatalf("PATCH clear request-retry status = %d body=%s", clearRetryResp.Code, clearRetryResp.Body.String())
	}
	item = getXAIAPIKeyItem(t, engine)
	if _, exists := item["request-retry"]; exists {
		t.Fatalf("PATCH null request-retry still present: %#v", item)
	}
	storedAuth, _, errGet = repo.GetAuth(context.Background(), auths[0].ID)
	if errGet != nil {
		t.Fatalf("GetAuth(after request-retry clear) error = %v", errGet)
	}
	if got, ok := storedAuth.RequestRetryOverride(); ok || got != 0 {
		t.Fatalf("request retry override after clear = (%d, %t), want inherited", got, ok)
	}

	replaceResp := httptest.NewRecorder()
	replaceReq := httptest.NewRequest(http.MethodPut, "/xai-api-key", strings.NewReader(putBody))
	replaceReq.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(replaceResp, replaceReq)
	if replaceResp.Code != http.StatusOK {
		t.Fatalf("replacement PUT status = %d body=%s", replaceResp.Code, replaceResp.Body.String())
	}
	if replaced := getXAIAPIKeyItem(t, engine); replaced["disabled"] != true {
		t.Fatalf("replacement PUT unexpectedly enabled credential: %#v", replaced)
	}

	deleteResp := httptest.NewRecorder()
	deleteReq := httptest.NewRequest(http.MethodDelete, "/xai-api-key?api-key=xai-key", nil)
	engine.ServeHTTP(deleteResp, deleteReq)
	if deleteResp.Code != http.StatusOK {
		t.Fatalf("DELETE status = %d body=%s", deleteResp.Code, deleteResp.Body.String())
	}
	auths, errAuths = repo.ListAuths(context.Background())
	if errAuths != nil {
		t.Fatalf("ListAuths() after delete error = %v", errAuths)
	}
	if len(auths) != 0 {
		t.Fatalf("auth count after delete = %d, want 0", len(auths))
	}
}

func TestXAIAPIKeyManagementIndexAndDuplicateSelection(t *testing.T) {
	db, cleanup := openManagementLogTestDB(t)
	defer cleanup()

	repo := cluster.NewRepository(db)
	handler := NewHandler(repo, nil, "127.0.0.1", 0)
	engine := gin.New()
	engine.GET("/xai-api-key", handler.GetXAIKeys)
	engine.PUT("/xai-api-key", handler.PutXAIKeys)
	engine.PATCH("/xai-api-key", handler.PatchXAIKey)
	engine.DELETE("/xai-api-key", handler.DeleteXAIKey)

	putResp := httptest.NewRecorder()
	putReq := httptest.NewRequest(http.MethodPut, "/xai-api-key", strings.NewReader(`[
		{"api-key":"shared-key","base-url":"https://one.example/v1"},
		{"api-key":"shared-key","base-url":"https://two.example/v1"}
	]`))
	putReq.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(putResp, putReq)
	if putResp.Code != http.StatusOK {
		t.Fatalf("PUT status = %d body=%s", putResp.Code, putResp.Body.String())
	}

	items := getXAIAPIKeyItems(t, engine)
	if len(items) != 2 {
		t.Fatalf("GET item count = %d, want 2", len(items))
	}
	firstID := items[0]["id"]
	firstBaseURL := items[0]["base-url"]

	patchResp := httptest.NewRecorder()
	patchReq := httptest.NewRequest(http.MethodPatch, "/xai-api-key", strings.NewReader(`{"index":0,"value":{"priority":13}}`))
	patchReq.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(patchResp, patchReq)
	if patchResp.Code != http.StatusOK {
		t.Fatalf("PATCH status = %d body=%s", patchResp.Code, patchResp.Body.String())
	}
	items = getXAIAPIKeyItems(t, engine)
	if items[0]["id"] != firstID || items[0]["base-url"] != firstBaseURL || items[0]["priority"] != float64(13) {
		t.Fatalf("PATCH index targeted a different item: before=%v/%v after=%#v", firstID, firstBaseURL, items[0])
	}

	deleteResp := httptest.NewRecorder()
	deleteReq := httptest.NewRequest(http.MethodDelete, "/xai-api-key?api-key=shared-key", nil)
	engine.ServeHTTP(deleteResp, deleteReq)
	if deleteResp.Code != http.StatusBadRequest {
		t.Fatalf("ambiguous DELETE status = %d, want 400 body=%s", deleteResp.Code, deleteResp.Body.String())
	}
	if itemsAfter := getXAIAPIKeyItems(t, engine); len(itemsAfter) != 2 {
		t.Fatalf("ambiguous DELETE changed item count to %d", len(itemsAfter))
	}
}

func getXAIAPIKeyItem(t *testing.T, engine http.Handler) map[string]any {
	t.Helper()
	items := getXAIAPIKeyItems(t, engine)
	if len(items) != 1 {
		t.Fatalf("GET item count = %d, want 1", len(items))
	}
	return items[0]
}

func getXAIAPIKeyItems(t *testing.T, engine http.Handler) []map[string]any {
	t.Helper()
	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/xai-api-key", nil)
	engine.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("GET status = %d body=%s", resp.Code, resp.Body.String())
	}
	var payload map[string][]map[string]any
	if errDecode := json.Unmarshal(resp.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode GET response: %v", errDecode)
	}
	return payload["xai-api-key"]
}
