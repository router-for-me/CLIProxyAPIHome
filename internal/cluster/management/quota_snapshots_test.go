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

func TestQuotaManagementListAndDetailReadDatabaseSnapshots(t *testing.T) {
	handler, closeRepo := newUsageObservabilityTestHandler(t)
	defer closeRepo()
	now := time.Now().UTC().Truncate(time.Second)
	auth := &coreauth.Auth{ID: "quota-management-auth", Index: "quota-management-auth", Provider: "codex", Label: "Management Codex", Status: coreauth.StatusActive, Metadata: map[string]any{"type": "codex", "email": "ops@example.com", "access_token": "management-token-must-not-leak"}, CreatedAt: now, UpdatedAt: now}
	if _, errUpsert := handler.repo.UpsertAuth(context.Background(), auth, "test"); errUpsert != nil {
		t.Fatalf("UpsertAuth() error = %v", errUpsert)
	}
	expiresAt := now.Add(time.Hour)
	resetAt := now.Add(30 * time.Minute)
	resetCreditGrantedAt := now.Add(-time.Hour)
	resetCreditExpiry := now.Add(72 * time.Hour)
	remaining := 0.15
	period := float64(5)
	availableResetCredits := 3
	_, errSnapshot := handler.repo.UpsertQuotaSnapshot(context.Background(), cluster.QuotaSnapshotWrite{
		CredentialID: "quota-management-auth", QuotaStatus: "low", CollectionStatus: "success", Source: "response_header",
		ObservedAt: &now, ExpiresAt: &expiresAt, LastAttemptAt: &now, LastSuccessAt: &now,
		ParserVersion: cluster.QuotaSnapshotVersion("codex"), CollectorVersion: cluster.QuotaSnapshotVersion("codex"), ReplaceWindows: true,
		Plan: &cluster.QuotaPlan{Name: "Pro 20x", Premium: true}, ReplacePlan: true,
		ResetCredits: &cluster.QuotaResetCredits{
			AvailableCount: &availableResetCredits,
			ObservedAt:     now,
			Credits: []cluster.QuotaResetCredit{{
				ID: "reset-credit-1", Status: "available", GrantedAt: resetCreditGrantedAt, ExpiresAt: &resetCreditExpiry,
			}},
		},
		ReplaceResetCredits: true,
		Windows:             []cluster.QuotaWindow{{ID: "codex-5-hour", Scope: "account", Mode: "rolling", Status: "low", Unit: "percentage", RemainingRatio: &remaining, ResetAt: &resetAt, PeriodUnit: "hour", PeriodValue: &period, Source: "response_header", ObservedAt: now}},
	})
	if errSnapshot != nil {
		t.Fatalf("UpsertQuotaSnapshot() error = %v", errSnapshot)
	}

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.GET("/quota/credentials", handler.ListQuotaCredentials)
	engine.GET("/quota/credentials/:credential_id", handler.GetQuotaCredential)

	listResponse := httptest.NewRecorder()
	engine.ServeHTTP(listResponse, httptest.NewRequest(http.MethodGet, "/quota/credentials?provider=codex&quota_status=low", nil))
	if listResponse.Code != http.StatusOK {
		t.Fatalf("list status = %d body=%s", listResponse.Code, listResponse.Body.String())
	}
	var listPayload map[string]any
	if errDecode := json.Unmarshal(listResponse.Body.Bytes(), &listPayload); errDecode != nil {
		t.Fatalf("decode list: %v", errDecode)
	}
	if listPayload["total"] != float64(1) {
		t.Fatalf("list total = %v, want 1", listPayload["total"])
	}
	items, ok := listPayload["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("list items = %#v, want one", listPayload["items"])
	}
	listItem, ok := items[0].(map[string]any)
	if !ok || listItem["earliest_reset_at"] != resetAt.Format(time.RFC3339) {
		t.Fatalf("list earliest_reset_at = %#v, want %s", listItem["earliest_reset_at"], resetAt.Format(time.RFC3339))
	}
	if plan, ok := listItem["plan"].(map[string]any); !ok || plan["name"] != "Pro 20x" {
		t.Fatalf("list plan = %#v, want Pro 20x", listItem["plan"])
	}
	if _, exists := listItem["reset_credits"]; exists {
		t.Fatalf("list unexpectedly exposed detail-only reset credits: %#v", listItem["reset_credits"])
	}
	if _, ok := listPayload["global_summary"].(map[string]any); !ok {
		t.Fatalf("global_summary = %T, want object", listPayload["global_summary"])
	}
	if strings.Contains(listResponse.Body.String(), "management-token-must-not-leak") {
		t.Fatalf("list response leaked credential token: %s", listResponse.Body.String())
	}
	if strings.Contains(listResponse.Body.String(), "ops@example.com") || !strings.Contains(listResponse.Body.String(), "op***@example.com") {
		t.Fatalf("list response did not mask account metadata: %s", listResponse.Body.String())
	}

	detailResponse := httptest.NewRecorder()
	engine.ServeHTTP(detailResponse, httptest.NewRequest(http.MethodGet, "/quota/credentials/quota-management-auth", nil))
	if detailResponse.Code != http.StatusOK {
		t.Fatalf("detail status = %d body=%s", detailResponse.Code, detailResponse.Body.String())
	}
	var detailPayload map[string]any
	if errDecode := json.Unmarshal(detailResponse.Body.Bytes(), &detailPayload); errDecode != nil {
		t.Fatalf("decode detail: %v", errDecode)
	}
	windows, ok := detailPayload["windows"].([]any)
	if !ok || len(windows) != 1 {
		t.Fatalf("detail windows = %#v, want one", detailPayload["windows"])
	}
	credential, ok := detailPayload["credential"].(map[string]any)
	if !ok || credential["earliest_reset_at"] != resetAt.Format(time.RFC3339) {
		t.Fatalf("detail earliest_reset_at = %#v, want %s", credential["earliest_reset_at"], resetAt.Format(time.RFC3339))
	}
	resetCredits, ok := detailPayload["reset_credits"].(map[string]any)
	if !ok || resetCredits["available_count"] != float64(3) {
		t.Fatalf("detail reset_credits = %#v, want available_count=3", detailPayload["reset_credits"])
	}
	credits, ok := resetCredits["credits"].([]any)
	if !ok || len(credits) != 1 {
		t.Fatalf("detail reset credit entries = %#v, want one", resetCredits["credits"])
	}
	credit, ok := credits[0].(map[string]any)
	if !ok || credit["status"] != "available" || credit["granted_at"] != resetCreditGrantedAt.Format(time.RFC3339) || credit["expires_at"] != resetCreditExpiry.Format(time.RFC3339) {
		t.Fatalf("detail reset credit = %#v, want read-only timing fields", credits[0])
	}
	if _, exists := credit["id"]; exists || strings.Contains(detailResponse.Body.String(), "reset-credit-1") {
		t.Fatalf("detail exposed provider reset-credit id: %s", detailResponse.Body.String())
	}
	if strings.Contains(detailResponse.Body.String(), "management-token-must-not-leak") {
		t.Fatalf("detail response leaked credential token: %s", detailResponse.Body.String())
	}
}

func TestQuotaManagementRejectsInvalidFilter(t *testing.T) {
	handler, closeRepo := newUsageObservabilityTestHandler(t)
	defer closeRepo()
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.GET("/quota/credentials", handler.ListQuotaCredentials)
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/quota/credentials?quota_status=broken", nil))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s, want 400", response.Code, response.Body.String())
	}
}

func TestQuotaManagementRejectsInvalidSortAndPagination(t *testing.T) {
	handler, closeRepo := newUsageObservabilityTestHandler(t)
	defer closeRepo()
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.GET("/quota/credentials", handler.ListQuotaCredentials)
	for _, path := range []string{
		"/quota/credentials?sort=secret_desc",
		"/quota/credentials?limit=0",
		"/quota/credentials?limit=201",
		"/quota/credentials?offset=-1",
	} {
		response := httptest.NewRecorder()
		engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusBadRequest {
			t.Fatalf("%s status = %d body=%s, want 400", path, response.Code, response.Body.String())
		}
	}
}

func TestQuotaManagementMissingCredentialReturnsUniformNotFound(t *testing.T) {
	handler, closeRepo := newUsageObservabilityTestHandler(t)
	defer closeRepo()
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.GET("/quota/credentials/:credential_id", handler.GetQuotaCredential)
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/quota/credentials/deleted-or-missing", nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d body=%s, want 404", response.Code, response.Body.String())
	}
	var payload map[string]any
	if errDecode := json.Unmarshal(response.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	errorPayload, ok := payload["error"].(map[string]any)
	if !ok || errorPayload["code"] != "QUOTA_CREDENTIAL_NOT_FOUND" {
		t.Fatalf("error payload = %#v", payload["error"])
	}
}

func TestQuotaManagementEmptyCollectionsUseJSONArrays(t *testing.T) {
	handler, closeRepo := newUsageObservabilityTestHandler(t)
	defer closeRepo()
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.GET("/quota/credentials", handler.ListQuotaCredentials)
	engine.GET("/quota/credentials/:credential_id", handler.GetQuotaCredential)

	listResponse := httptest.NewRecorder()
	engine.ServeHTTP(listResponse, httptest.NewRequest(http.MethodGet, "/quota/credentials", nil))
	if listResponse.Code != http.StatusOK {
		t.Fatalf("list status = %d body=%s", listResponse.Code, listResponse.Body.String())
	}
	var listPayload map[string]any
	if errDecode := json.Unmarshal(listResponse.Body.Bytes(), &listPayload); errDecode != nil {
		t.Fatalf("decode list response: %v", errDecode)
	}
	items, ok := listPayload["items"].([]any)
	if !ok || len(items) != 0 {
		t.Fatalf("items = %#v, want empty array", listPayload["items"])
	}

	now := time.Now().UTC().Truncate(time.Second)
	auth := &coreauth.Auth{ID: "quota-empty-windows", Index: "quota-empty-windows", Provider: "codex", Status: coreauth.StatusActive, Metadata: map[string]any{"type": "codex"}, CreatedAt: now, UpdatedAt: now}
	if _, errUpsert := handler.repo.UpsertAuth(context.Background(), auth, "test"); errUpsert != nil {
		t.Fatalf("UpsertAuth() error = %v", errUpsert)
	}
	if claimed, errClaim := handler.repo.ClaimQuotaProbe(context.Background(), auth.ID, "home-a", now, time.Minute); errClaim != nil || !claimed {
		t.Fatalf("ClaimQuotaProbe() = %v, %v", claimed, errClaim)
	}

	detailResponse := httptest.NewRecorder()
	engine.ServeHTTP(detailResponse, httptest.NewRequest(http.MethodGet, "/quota/credentials/"+auth.ID, nil))
	if detailResponse.Code != http.StatusOK {
		t.Fatalf("detail status = %d body=%s", detailResponse.Code, detailResponse.Body.String())
	}
	var detailPayload map[string]any
	if errDecode := json.Unmarshal(detailResponse.Body.Bytes(), &detailPayload); errDecode != nil {
		t.Fatalf("decode detail response: %v", errDecode)
	}
	windows, ok := detailPayload["windows"].([]any)
	if !ok || len(windows) != 0 {
		t.Fatalf("windows = %#v, want empty array", detailPayload["windows"])
	}
	credential, ok := detailPayload["credential"].(map[string]any)
	if !ok {
		t.Fatalf("credential = %T, want object", detailPayload["credential"])
	}
	if _, exists := credential["next_probe_at"]; !exists {
		t.Fatalf("credential omitted next_probe_at: %#v", credential)
	}
}

func TestQuotaManagementReturnsServiceUnavailableForDatabaseOutage(t *testing.T) {
	handler := NewHandler(cluster.NewRepository(nil), nil, "", 0)
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.GET("/quota/credentials", handler.ListQuotaCredentials)
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/quota/credentials", nil)
	request.Header.Set("X-Request-ID", strings.Repeat("x", 256))
	engine.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d body=%s, want 503", response.Code, response.Body.String())
	}
	var payload map[string]any
	if errDecode := json.Unmarshal(response.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	errorPayload, ok := payload["error"].(map[string]any)
	if !ok || errorPayload["retryable"] != true || errorPayload["request_id"] != "" {
		t.Fatalf("error payload = %#v", payload["error"])
	}
}
