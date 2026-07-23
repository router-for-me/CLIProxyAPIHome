package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
)

func TestQuotaManagementListFiltersByIDs(t *testing.T) {
	handler, closeRepo := newUsageObservabilityTestHandler(t)
	defer closeRepo()
	now := time.Now().UTC().Truncate(time.Second)
	mixedCaseID := "ABCDEF12-3456-4789-ABCD-EF1234567890"
	for _, id := range []string{mixedCaseID, "quota-id-b", "quota-id-c"} {
		auth := &coreauth.Auth{ID: id, Index: id, Provider: "codex", Label: id, Status: coreauth.StatusActive, Metadata: map[string]any{"type": "codex", "access_token": "token-" + id}, CreatedAt: now, UpdatedAt: now}
		if _, errUpsert := handler.repo.UpsertAuth(context.Background(), auth, "test"); errUpsert != nil {
			t.Fatalf("UpsertAuth(%s) error = %v", id, errUpsert)
		}
	}
	used, remaining, limit, periodValue := 25.0, 75.0, 100.0, 1.0
	if _, errUpsert := handler.repo.UpsertQuotaSnapshot(context.Background(), cluster.QuotaSnapshotWrite{
		CredentialID: mixedCaseID, QuotaStatus: "healthy", CollectionStatus: "success", Source: "active_probe", ObservedAt: &now, ReplaceWindows: true,
		Windows: []cluster.QuotaWindow{{ID: "mixed-case-window", Scope: "account", Mode: "fixed", Status: "healthy", Unit: "requests", Used: &used, Remaining: &remaining, Limit: &limit, PeriodUnit: "month", PeriodValue: &periodValue, Source: "active_probe", ObservedAt: now}},
	}); errUpsert != nil {
		t.Fatalf("UpsertQuotaSnapshot(%s) error = %v", mixedCaseID, errUpsert)
	}

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.GET("/quota/credentials", handler.ListQuotaCredentials)

	response := httptest.NewRecorder()
	engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/quota/credentials?ids="+mixedCaseID+",quota-id-c", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("list status = %d body=%s", response.Code, response.Body.String())
	}
	var payload map[string]any
	if errDecode := json.Unmarshal(response.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode list: %v", errDecode)
	}
	if payload["total"] != float64(2) {
		t.Fatalf("ids filter total = %v, want 2", payload["total"])
	}
	globalSummary, ok := payload["global_summary"].(map[string]any)
	if !ok || globalSummary["total_credentials"] != float64(3) {
		t.Fatalf("ids filter global_summary = %#v, want all three credentials", payload["global_summary"])
	}
	items, ok := payload["items"].([]any)
	if !ok || len(items) != 2 {
		t.Fatalf("ids filter items = %#v, want two", payload["items"])
	}
	seen := map[string]bool{}
	for _, raw := range items {
		if item, okItem := raw.(map[string]any); okItem {
			credentialID := item["credential_id"].(string)
			seen[credentialID] = true
			if credentialID == mixedCaseID && item["window_count"] != float64(1) {
				t.Fatalf("mixed-case credential window_count = %v, want 1", item["window_count"])
			}
		}
	}
	if !seen[mixedCaseID] || !seen["quota-id-c"] || seen["quota-id-b"] {
		t.Fatalf("ids filter returned wrong credentials: %v", seen)
	}

	wrongCaseResponse := httptest.NewRecorder()
	engine.ServeHTTP(wrongCaseResponse, httptest.NewRequest(http.MethodGet, "/quota/credentials?ids="+strings.ToLower(mixedCaseID), nil))
	if wrongCaseResponse.Code != http.StatusOK {
		t.Fatalf("wrong-case list status = %d body=%s", wrongCaseResponse.Code, wrongCaseResponse.Body.String())
	}
	var wrongCasePayload map[string]any
	if errDecode := json.Unmarshal(wrongCaseResponse.Body.Bytes(), &wrongCasePayload); errDecode != nil {
		t.Fatalf("decode wrong-case list: %v", errDecode)
	}
	if wrongCasePayload["total"] != float64(0) {
		t.Fatalf("wrong-case ids filter total = %v, want 0", wrongCasePayload["total"])
	}
}
