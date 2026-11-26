package management

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
)

type billingImportRoundTripper func(*http.Request) (*http.Response, error)

func (fn billingImportRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func TestPostBillingModelPriceCreatesRule(t *testing.T) {
	t.Parallel()

	handler, closeRepo := newBillingManagementTestHandler(t)
	defer closeRepo()

	body := []byte(`{"provider":"openai","model":"gpt-4.1-mini","service_tier":"standard","min_input_tokens":272001,"input_price_per_million":2,"output_price_per_million":8,"enabled":true}`)
	resp := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(resp)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/billing/model-prices", bytes.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")

	handler.CreateBillingModelPrice(ctx)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200", resp.Code, resp.Body.String())
	}
	var payload map[string]any
	if errDecode := json.Unmarshal(resp.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if payload["status"] != "ok" {
		t.Fatalf("status field = %v, want ok", payload["status"])
	}
	modelPrice, ok := payload["model_price"].(map[string]any)
	if !ok {
		t.Fatalf("model_price = %T, want object", payload["model_price"])
	}
	if modelPrice["provider"] != "openai" {
		t.Fatalf("model_price.provider = %v, want openai", modelPrice["provider"])
	}
	if modelPrice["model"] != "gpt-4.1-mini" {
		t.Fatalf("model_price.model = %v, want gpt-4.1-mini", modelPrice["model"])
	}
	if modelPrice["enabled"] != true {
		t.Fatalf("model_price.enabled = %v, want true", modelPrice["enabled"])
	}
	if modelPrice["input_price_per_million"] != float64(2) {
		t.Fatalf("model_price.input_price_per_million = %v, want 2", modelPrice["input_price_per_million"])
	}
	if modelPrice["service_tier"] != "standard" || modelPrice["min_input_tokens"] != float64(272001) {
		t.Fatalf("model_price tier/band = %v/%v", modelPrice["service_tier"], modelPrice["min_input_tokens"])
	}
}

func TestPostBillingModelPriceNormalizesAutoTierToStandard(t *testing.T) {
	t.Parallel()

	handler, closeRepo := newBillingManagementTestHandler(t)
	defer closeRepo()

	resp := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(resp)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/billing/model-prices", bytes.NewBufferString(`{"provider":"openai","model":"gpt-4.1-mini","service_tier":"auto","input_price_per_million":2,"enabled":true}`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	handler.CreateBillingModelPrice(ctx)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200", resp.Code, resp.Body.String())
	}
	var payload map[string]any
	if errDecode := json.Unmarshal(resp.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	modelPrice, ok := payload["model_price"].(map[string]any)
	if !ok || modelPrice["service_tier"] != "standard" {
		t.Fatalf("model_price = %#v, want standard tier", payload["model_price"])
	}
}

func TestBillingModelPriceRejectsInvalidServiceTierAsBadRequest(t *testing.T) {
	t.Parallel()

	handler, closeRepo := newBillingManagementTestHandler(t)
	defer closeRepo()

	postResp := httptest.NewRecorder()
	postCtx, _ := gin.CreateTestContext(postResp)
	postCtx.Request = httptest.NewRequest(http.MethodPost, "/billing/model-prices", bytes.NewBufferString(`{"provider":"openai","model":"gpt-test","service_tier":"bad tier"}`))
	postCtx.Request.Header.Set("Content-Type", "application/json")
	handler.CreateBillingModelPrice(postCtx)
	if postResp.Code != http.StatusBadRequest {
		t.Fatalf("POST status = %d body=%s, want 400", postResp.Code, postResp.Body.String())
	}

	price, errCreate := handler.repo.CreateBillingModelPrice(context.Background(), cluster.BillingModelPriceUpdate{Provider: "openai", Model: "gpt-test", Enabled: true})
	if errCreate != nil {
		t.Fatalf("CreateBillingModelPrice() error = %v", errCreate)
	}
	patchResp := httptest.NewRecorder()
	patchCtx, _ := gin.CreateTestContext(patchResp)
	patchCtx.Request = httptest.NewRequest(http.MethodPatch, "/billing/model-prices/"+price.ID, bytes.NewBufferString(`{"service_tier":"bad tier"}`))
	patchCtx.Request.Header.Set("Content-Type", "application/json")
	patchCtx.Params = gin.Params{{Key: "id", Value: price.ID}}
	handler.UpdateBillingModelPrice(patchCtx)
	if patchResp.Code != http.StatusBadRequest {
		t.Fatalf("PATCH status = %d body=%s, want 400", patchResp.Code, patchResp.Body.String())
	}
}

func TestListBillingModelPricesReturnsSchemaVersion(t *testing.T) {
	t.Parallel()

	handler, closeRepo := newBillingManagementTestHandler(t)
	defer closeRepo()
	resp := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(resp)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/billing/model-prices", nil)
	handler.ListBillingModelPrices(ctx)

	var payload map[string]any
	if errDecode := json.Unmarshal(resp.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if payload["price_rule_schema_version"] != float64(2) {
		t.Fatalf("price_rule_schema_version = %v, want 2", payload["price_rule_schema_version"])
	}
}

func TestBillingSettingsGetAndPatch(t *testing.T) {
	t.Parallel()

	handler, closeRepo := newBillingManagementTestHandler(t)
	defer closeRepo()

	getResp := httptest.NewRecorder()
	getCtx, _ := gin.CreateTestContext(getResp)
	getCtx.Request = httptest.NewRequest(http.MethodGet, "/billing/settings", nil)
	handler.GetBillingSettings(getCtx)
	if getResp.Code != http.StatusOK ||
		!bytes.Contains(getResp.Body.Bytes(), []byte(`"model_source":"request"`)) ||
		!bytes.Contains(getResp.Body.Bytes(), []byte(`"service_tier_source":"request"`)) ||
		!bytes.Contains(getResp.Body.Bytes(), []byte(`"report_timezone":"UTC"`)) {
		t.Fatalf("GET status/body = %d %s", getResp.Code, getResp.Body.String())
	}

	patchResp := httptest.NewRecorder()
	patchCtx, _ := gin.CreateTestContext(patchResp)
	patchCtx.Request = httptest.NewRequest(http.MethodPatch, "/billing/settings", bytes.NewBufferString(`{"model_source":"response","service_tier_source":"response","report_timezone":"Asia/Shanghai"}`))
	patchCtx.Request.Header.Set("Content-Type", "application/json")
	handler.UpdateBillingSettings(patchCtx)
	if patchResp.Code != http.StatusOK ||
		!bytes.Contains(patchResp.Body.Bytes(), []byte(`"model_source":"response"`)) ||
		!bytes.Contains(patchResp.Body.Bytes(), []byte(`"service_tier_source":"response"`)) ||
		!bytes.Contains(patchResp.Body.Bytes(), []byte(`"report_timezone":"Asia/Shanghai"`)) {
		t.Fatalf("PATCH status/body = %d %s", patchResp.Code, patchResp.Body.String())
	}

	invalidResp := httptest.NewRecorder()
	invalidCtx, _ := gin.CreateTestContext(invalidResp)
	invalidCtx.Request = httptest.NewRequest(http.MethodPatch, "/billing/settings", bytes.NewBufferString(`{"report_timezone":"Invalid/Timezone"}`))
	invalidCtx.Request.Header.Set("Content-Type", "application/json")
	handler.UpdateBillingSettings(invalidCtx)
	if invalidResp.Code != http.StatusBadRequest || !bytes.Contains(invalidResp.Body.Bytes(), []byte(`"error":"invalid_body"`)) {
		t.Fatalf("invalid PATCH status/body = %d %s, want invalid_body", invalidResp.Code, invalidResp.Body.String())
	}
}

func TestParseBillingModelPriceImportCatalogDefaultsMissingCachePricesFromInput(t *testing.T) {
	t.Parallel()

	catalog, errCatalog := parseBillingModelPriceImportCatalog([]byte(`{
  "openai": {"models": {"gpt-import": {"id": "gpt-import", "cost": {
    "input": 2, "output": 8, "cache_write": 0,
    "context_over_200k": {"input": 3, "output": 12},
    "tiers": [{"input": 4, "output": 16, "cache_write": 0, "tier": {"type": "context", "size": 272000}}]
  }}}}
}`), modelsDevCatalogURL, time.Date(2026, time.July, 11, 0, 0, 0, 0, time.UTC))
	if errCatalog != nil {
		t.Fatalf("parseBillingModelPriceImportCatalog() error = %v", errCatalog)
	}
	if len(catalog.Models) != 1 || catalog.Models[0].Cost == nil || catalog.Models[0].Cost.CacheRead != 0.2 || catalog.Models[0].Cost.CacheWrite != 0 {
		t.Fatalf("catalog models = %#v", catalog.Models)
	}
	if len(catalog.Models[0].ContextBands) != 1 || catalog.Models[0].ContextBands[0].MinInputTokens != 272001 || catalog.Models[0].ContextBands[0].Cost.CacheRead != 0.4 || catalog.Models[0].ContextBands[0].Cost.CacheWrite != 0 || len(catalog.Models[0].ContextBands[0].MissingPriceFields) != 0 {
		t.Fatalf("context bands = %#v", catalog.Models[0].ContextBands)
	}
}

func TestParseBillingModelPriceImportCatalogDefaultsMissingTierCacheWriteFromInput(t *testing.T) {
	t.Parallel()

	catalog, errCatalog := parseBillingModelPriceImportCatalog([]byte(`{
  "requesty": {"models": {"google/gemini-tier": {"cost": {
    "input": 1.25, "output": 10, "cache_read": 0.125, "cache_write": 2.375,
    "tiers": [{"input": 2.5, "output": 15, "cache_read": 0.25, "tier": {"type": "context", "size": 200000}}]
  }}}}
}`), modelsDevCatalogURL, time.Date(2026, time.July, 11, 0, 0, 0, 0, time.UTC))
	if errCatalog != nil {
		t.Fatalf("parseBillingModelPriceImportCatalog() error = %v", errCatalog)
	}
	if len(catalog.Models) != 1 || len(catalog.Models[0].ContextBands) != 1 || catalog.Models[0].ContextBands[0].Cost.CacheRead != 0.25 || catalog.Models[0].ContextBands[0].Cost.CacheWrite != 3.125 || len(catalog.Models[0].ContextBands[0].MissingPriceFields) != 0 {
		t.Fatalf("catalog context bands = %#v", catalog.Models)
	}
}

func TestParseBillingModelPriceImportCatalogDefaultsBothCachePricesAndPreservesExplicitValues(t *testing.T) {
	t.Parallel()

	catalog, errCatalog := parseBillingModelPriceImportCatalog([]byte(`{
  "openai": {"models": {
    "gpt-derived": {"cost": {"input": 2, "output": 8}},
    "gpt-explicit": {"cost": {"input": 2, "output": 8, "cache_read": 0, "cache_write": 3}}
  }}
}`), modelsDevCatalogURL, time.Date(2026, time.July, 14, 0, 0, 0, 0, time.UTC))
	if errCatalog != nil {
		t.Fatalf("parseBillingModelPriceImportCatalog() error = %v", errCatalog)
	}
	if len(catalog.Models) != 2 || catalog.Models[0].Cost == nil || catalog.Models[1].Cost == nil {
		t.Fatalf("catalog models = %#v", catalog.Models)
	}
	if catalog.Models[0].Model != "gpt-derived" || catalog.Models[0].Cost.CacheRead != 0.2 || catalog.Models[0].Cost.CacheWrite != 2.5 {
		t.Fatalf("derived cache prices = %#v", catalog.Models[0])
	}
	if catalog.Models[1].Model != "gpt-explicit" || catalog.Models[1].Cost.CacheRead != 0 || catalog.Models[1].Cost.CacheWrite != 3 {
		t.Fatalf("explicit cache prices = %#v", catalog.Models[1])
	}
}

func TestParseBillingModelPriceImportCatalogFailsClosedForMalformedPriceAndTier(t *testing.T) {
	t.Parallel()

	catalog, errCatalog := parseBillingModelPriceImportCatalog([]byte(`{
  "openai": {"models": {
    "gpt-malformed-price": {"cost": {"input": "not-a-number", "output": 8}},
    "gpt-malformed-tier": {"cost": {"input": 2, "output": 8, "tiers": [{"input": 4, "output": 16, "tier": {"type": "context", "size": "not-a-number"}}]}}
  }}
}`), modelsDevCatalogURL, time.Date(2026, time.July, 11, 0, 0, 0, 0, time.UTC))
	if errCatalog != nil {
		t.Fatalf("parseBillingModelPriceImportCatalog() error = %v", errCatalog)
	}
	if len(catalog.Models) != 2 {
		t.Fatalf("catalog models = %#v", catalog.Models)
	}
	if len(catalog.Models[0].InvalidPriceFields) != 1 || catalog.Models[0].InvalidPriceFields[0] != "input" {
		t.Fatalf("malformed price fields = %#v", catalog.Models[0].InvalidPriceFields)
	}
	if len(catalog.Models[1].ContextBandIssues) != 1 || catalog.Models[1].ContextBandIssues[0] != "invalid_context_band_boundary" {
		t.Fatalf("malformed tier issues = %#v", catalog.Models[1].ContextBandIssues)
	}
}

func TestPreviewBillingModelPriceImportRejectsMalformedContextBand(t *testing.T) {
	t.Parallel()

	handler, closeRepo := newBillingManagementTestHandler(t)
	defer closeRepo()
	catalog, errCatalog := parseBillingModelPriceImportCatalog([]byte(`{
  "openai": {"models": {"gpt-malformed-tier": {"cost": {
    "input": 2, "output": 8,
    "tiers": [{"input": 4, "output": 16, "tier": {"type": "context", "size": "invalid"}}]
  }}}}
}`), modelsDevCatalogURL, time.Now().UTC())
	if errCatalog != nil {
		t.Fatalf("parseBillingModelPriceImportCatalog() error = %v", errCatalog)
	}
	preview, errPreview := handler.repo.CreateBillingModelPriceImportPreview(context.Background(), cluster.BillingModelPriceImportPreviewInput{
		Source:  cluster.BillingModelPriceImportSourceModelsDev,
		Targets: []cluster.BillingModelPriceImportTarget{{Provider: "openai", Model: "gpt-malformed-tier"}},
		Policy:  cluster.BillingModelPriceImportPolicy{OverwriteMode: "missing", DefaultMultiplier: 1},
	}, catalog)
	if errPreview != nil {
		t.Fatalf("CreateBillingModelPriceImportPreview() error = %v", errPreview)
	}
	if len(preview.Rows) != 1 || preview.Rows[0].Status != "invalid" || preview.Rows[0].Action != "review" || preview.Rows[0].Applicable || len(preview.Rows[0].Reasons) != 1 || preview.Rows[0].Reasons[0].Code != "invalid_context_bands" {
		t.Fatalf("preview rows = %#v", preview.Rows)
	}
}

func TestPreviewBillingModelPriceImportFetchesModelsDevServerSide(t *testing.T) {
	t.Parallel()

	handler, closeRepo := newBillingManagementTestHandler(t)
	defer closeRepo()
	handler.SetModelsDevHTTPClient(&http.Client{Transport: billingImportRoundTripper(func(request *http.Request) (*http.Response, error) {
		if request.URL.String() != modelsDevCatalogURL {
			return nil, fmt.Errorf("unexpected catalog URL %q", request.URL)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"openai":{"models":{"gpt-import":{"cost":{"input":2,"output":8}}}}}`))}, nil
	})})
	response := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(response)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/billing/model-prices/import/preview", strings.NewReader(`{"source":"models.dev","targets":[{"provider":"openai","model":"gpt-import"}],"policy":{"overwrite_mode":"missing","default_multiplier":1}}`))
	ctx.Request.Header.Set("Content-Type", "application/json")

	handler.PreviewBillingModelPriceImport(ctx)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200", response.Code, response.Body.String())
	}
	var payload map[string]any
	if errDecode := json.Unmarshal(response.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode preview response: %v", errDecode)
	}
	if payload["atomic"] != true || payload["preview_id"] == "" {
		t.Fatalf("preview response = %#v", payload)
	}
	rows, ok := payload["rows"].([]any)
	if !ok || len(rows) != 1 {
		t.Fatalf("preview rows = %#v", payload["rows"])
	}
	row, ok := rows[0].(map[string]any)
	if !ok || row["row_key"] == "" {
		t.Fatalf("preview row = %#v", rows[0])
	}
	applyResponse := httptest.NewRecorder()
	applyContext, _ := gin.CreateTestContext(applyResponse)
	applyContext.Request = httptest.NewRequest(http.MethodPost, "/billing/model-prices/import/apply", strings.NewReader(fmt.Sprintf(`{"preview_id":%q,"preview_revision":%q,"selected_keys":[%q],"idempotency_key":"preview-apply-key"}`, payload["preview_id"], payload["preview_revision"], row["row_key"])))
	applyContext.Request.Header.Set("Content-Type", "application/json")
	applyContext.Request.Header.Set("Idempotency-Key", "preview-apply-key")
	handler.ApplyBillingModelPriceImport(applyContext)
	if applyResponse.Code != http.StatusOK {
		t.Fatalf("apply status = %d body=%s, want 200", applyResponse.Code, applyResponse.Body.String())
	}
	var applyPayload map[string]any
	if errDecode := json.Unmarshal(applyResponse.Body.Bytes(), &applyPayload); errDecode != nil {
		t.Fatalf("decode apply response: %v", errDecode)
	}
	applyRows, ok := applyPayload["rows"].([]any)
	if !ok || len(applyRows) != 1 {
		t.Fatalf("apply rows = %#v", applyPayload["rows"])
	}
	applyRow, ok := applyRows[0].(map[string]any)
	if !ok || applyRow["key"] != row["row_key"] || applyRow["provider"] != "openai" || applyRow["model"] != "gpt-import" || applyRow["action"] != "create" || applyRow["status"] != "created" || applyRow["resource_id"] == "" {
		t.Fatalf("apply row = %#v", applyRows[0])
	}
}

func TestPreviewBillingModelPriceImportValidatesBeforeFetchingCatalog(t *testing.T) {
	t.Parallel()

	handler, closeRepo := newBillingManagementTestHandler(t)
	defer closeRepo()
	fetched := false
	handler.SetModelsDevHTTPClient(&http.Client{Transport: billingImportRoundTripper(func(_ *http.Request) (*http.Response, error) {
		fetched = true
		return nil, fmt.Errorf("catalog should not be fetched")
	})})
	response := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(response)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/billing/model-prices/import/preview", strings.NewReader(`{"source":"models.dev","targets":[{"provider":"openai","model":"gpt-import"}],"policy":{"overwrite_mode":"missing","default_multiplier":1,"multiplier_rules":[{"pattern":"gpt","multiplier":2}]}}`))
	ctx.Request.Header.Set("Content-Type", "application/json")

	handler.PreviewBillingModelPriceImport(ctx)

	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d body=%s, want 422", response.Code, response.Body.String())
	}
	if fetched {
		t.Fatal("invalid preview fetched the models.dev catalog")
	}
}

func TestPreviewBillingModelPriceImportReturnsInternalErrorForDatabaseFailure(t *testing.T) {
	t.Parallel()

	handler, closeRepo := newBillingManagementTestHandler(t)
	closeRepo()
	handler.SetModelsDevHTTPClient(&http.Client{Transport: billingImportRoundTripper(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"openai":{"models":{"gpt-import":{"cost":{"input":2,"output":8}}}}}`))}, nil
	})})
	response := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(response)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/billing/model-prices/import/preview", strings.NewReader(`{"source":"models.dev","targets":[{"provider":"openai","model":"gpt-import"}],"policy":{"overwrite_mode":"missing","default_multiplier":1}}`))
	ctx.Request.Header.Set("Content-Type", "application/json")

	handler.PreviewBillingModelPriceImport(ctx)

	if response.Code != http.StatusInternalServerError || !strings.Contains(response.Body.String(), "billing_import_preview_failed") {
		t.Fatalf("status = %d body=%s, want 500 billing_import_preview_failed", response.Code, response.Body.String())
	}
}

func TestPatchBillingModelPricePartialUpdatePreservesUnspecifiedFields(t *testing.T) {
	t.Parallel()

	handler, closeRepo := newBillingManagementTestHandler(t)
	defer closeRepo()

	price, errCreate := handler.repo.CreateBillingModelPrice(context.Background(), cluster.BillingModelPriceUpdate{
		Provider:                  "openai",
		Model:                     "gpt-4.1-mini",
		InputPricePerMillion:      2,
		OutputPricePerMillion:     8,
		CacheReadPricePerMillion:  1,
		CacheWritePricePerMillion: 3,
		RequestPrice:              5,
		Source:                    cluster.BillingPriceSourceSync,
		Enabled:                   true,
		Note:                      "keep note",
	})
	if errCreate != nil {
		t.Fatalf("CreateBillingModelPrice() error = %v", errCreate)
	}

	body := []byte(`{"request_price":0,"enabled":false}`)
	resp := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(resp)
	ctx.Request = httptest.NewRequest(http.MethodPatch, "/billing/model-prices/"+price.ID, bytes.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")
	ctx.Params = gin.Params{{Key: "id", Value: price.ID}}

	handler.UpdateBillingModelPrice(ctx)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200", resp.Code, resp.Body.String())
	}
	var payload map[string]any
	if errDecode := json.Unmarshal(resp.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if payload["status"] != "ok" {
		t.Fatalf("status field = %v, want ok", payload["status"])
	}
	modelPrice, ok := payload["model_price"].(map[string]any)
	if !ok {
		t.Fatalf("model_price = %T, want object", payload["model_price"])
	}
	if modelPrice["provider"] != "openai" || modelPrice["model"] != "gpt-4.1-mini" {
		t.Fatalf("model_price provider/model = %v/%v, want openai/gpt-4.1-mini", modelPrice["provider"], modelPrice["model"])
	}
	if modelPrice["input_price_per_million"] != float64(2) ||
		modelPrice["output_price_per_million"] != float64(8) ||
		modelPrice["cache_read_price_per_million"] != float64(1) ||
		modelPrice["cache_write_price_per_million"] != float64(3) {
		t.Fatalf("model_price token prices were not preserved: %#v", modelPrice)
	}
	if modelPrice["request_price"] != float64(0) {
		t.Fatalf("model_price.request_price = %v, want 0", modelPrice["request_price"])
	}
	if modelPrice["enabled"] != false {
		t.Fatalf("model_price.enabled = %v, want false", modelPrice["enabled"])
	}
	if modelPrice["source"] != cluster.BillingPriceSourceSync {
		t.Fatalf("model_price.source = %v, want %s", modelPrice["source"], cluster.BillingPriceSourceSync)
	}
	if modelPrice["note"] != "keep note" {
		t.Fatalf("model_price.note = %v, want keep note", modelPrice["note"])
	}
}

func TestDeleteBillingModelPriceDeletesRule(t *testing.T) {
	t.Parallel()

	handler, closeRepo := newBillingManagementTestHandler(t)
	defer closeRepo()

	price, errCreate := handler.repo.CreateBillingModelPrice(context.Background(), cluster.BillingModelPriceUpdate{
		Provider:     "openai",
		Model:        "gpt-4.1-mini",
		RequestPrice: 1,
		Enabled:      true,
	})
	if errCreate != nil {
		t.Fatalf("CreateBillingModelPrice() error = %v", errCreate)
	}

	resp := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(resp)
	ctx.Request = httptest.NewRequest(http.MethodDelete, "/billing/model-prices/"+price.ID, nil)
	ctx.Params = gin.Params{{Key: "id", Value: price.ID}}

	handler.DeleteBillingModelPrice(ctx)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200", resp.Code, resp.Body.String())
	}
	var payload map[string]any
	if errDecode := json.Unmarshal(resp.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if payload["status"] != "ok" {
		t.Fatalf("status field = %v, want ok", payload["status"])
	}
}

func TestDeleteBillingModelPriceMissingIDReturnsNotFound(t *testing.T) {
	t.Parallel()

	handler, closeRepo := newBillingManagementTestHandler(t)
	defer closeRepo()

	resp := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(resp)
	ctx.Request = httptest.NewRequest(http.MethodDelete, "/billing/model-prices/missing", nil)
	ctx.Params = gin.Params{{Key: "id", Value: "missing"}}

	handler.DeleteBillingModelPrice(ctx)

	if resp.Code != http.StatusNotFound {
		t.Fatalf("status = %d body=%s, want 404", resp.Code, resp.Body.String())
	}
	var payload map[string]any
	if errDecode := json.Unmarshal(resp.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if payload["error"] != "model_price_not_found" {
		t.Fatalf("error = %v, want model_price_not_found", payload["error"])
	}
}

func TestPostBillingBalanceDeductRequiresNote(t *testing.T) {
	t.Parallel()

	handler, closeRepo := newBillingManagementTestHandler(t)
	defer closeRepo()

	body := []byte(`{"user_id":1,"amount":1}`)
	resp := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(resp)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/billing/balance-records/deduct", bytes.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")

	handler.DeductBillingBalance(ctx)

	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s, want 400", resp.Code, resp.Body.String())
	}
}

func TestPostBillingBalanceRechargeUpdatesUserCredits(t *testing.T) {
	t.Parallel()

	handler, closeRepo := newBillingManagementTestHandler(t)
	defer closeRepo()

	username := "billing-user"
	credits := 5.0
	user, errCreate := handler.repo.CreateUser(context.Background(), cluster.UserUpdate{Username: &username, Credits: &credits})
	if errCreate != nil {
		t.Fatalf("CreateUser() error = %v", errCreate)
	}

	body := []byte(fmt.Sprintf(`{"user_id":%d,"amount":7,"note":"manual recharge"}`, user.ID))
	resp := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(resp)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/billing/balance-records/recharge", bytes.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")

	handler.RechargeBillingBalance(ctx)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200", resp.Code, resp.Body.String())
	}
	var payload map[string]any
	if errDecode := json.Unmarshal(resp.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	record, ok := payload["balance_record"].(map[string]any)
	if !ok {
		t.Fatalf("balance_record = %T, want object", payload["balance_record"])
	}
	if record["type"] != cluster.BillingBalanceTypeRecharge {
		t.Fatalf("balance_record.type = %v, want recharge", record["type"])
	}
	if record["balance_before"] != float64(5) {
		t.Fatalf("balance_record.balance_before = %v, want 5", record["balance_before"])
	}
	if record["balance_after"] != float64(12) {
		t.Fatalf("balance_record.balance_after = %v, want 12", record["balance_after"])
	}
	updated, errUser := handler.repo.GetUser(context.Background(), user.ID)
	if errUser != nil {
		t.Fatalf("GetUser() error = %v", errUser)
	}
	if updated.Credits != 12 {
		t.Fatalf("user credits = %v, want 12", updated.Credits)
	}
}

func TestPostBillingBalanceRechargeMissingUserReturnsNotFound(t *testing.T) {
	t.Parallel()

	handler, closeRepo := newBillingManagementTestHandler(t)
	defer closeRepo()

	body := []byte(`{"user_id":999,"amount":1,"note":"manual recharge"}`)
	resp := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(resp)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/billing/balance-records/recharge", bytes.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")

	handler.RechargeBillingBalance(ctx)

	if resp.Code != http.StatusNotFound {
		t.Fatalf("status = %d body=%s, want 404", resp.Code, resp.Body.String())
	}
	var payload map[string]any
	if errDecode := json.Unmarshal(resp.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if payload["error"] != "user_not_found" {
		t.Fatalf("error = %v, want user_not_found", payload["error"])
	}
}

func TestGetBillingOverviewReturnsTotals(t *testing.T) {
	t.Parallel()

	handler, closeRepo := newBillingManagementTestHandler(t)
	defer closeRepo()
	seedBillingManagementCharge(t, handler)

	resp := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(resp)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/billing/overview", nil)

	handler.GetBillingOverview(ctx)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200", resp.Code, resp.Body.String())
	}
	var payload map[string]any
	if errDecode := json.Unmarshal(resp.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	overview, ok := payload["overview"].(map[string]any)
	if !ok {
		t.Fatalf("overview = %T, want object", payload["overview"])
	}
	if overview["total_charge_amount"] != float64(2) {
		t.Fatalf("overview.total_charge_amount = %v, want 2", overview["total_charge_amount"])
	}
	if overview["total_recharge_amount"] != float64(50) {
		t.Fatalf("overview.total_recharge_amount = %v, want 50", overview["total_recharge_amount"])
	}
	rangeValue, ok := overview["range"].(map[string]any)
	if !ok {
		t.Fatalf("overview.range = %T, want object", overview["range"])
	}
	if rangeValue["timezone"] != "UTC" {
		t.Fatalf("overview.range.timezone = %v, want UTC", rangeValue["timezone"])
	}
	if rangeValue["from_at"] != nil || rangeValue["to_at_exclusive"] != nil {
		t.Fatalf("overview.range exact bounds = %#v, want null bounds", rangeValue)
	}
}

func TestGetBillingOverviewGroupsRequestedTimezone(t *testing.T) {
	t.Parallel()

	handler, closeRepo := newBillingManagementTestHandler(t)
	defer closeRepo()
	seedBillingManagementCharge(t, handler)
	payload := `{"timestamp":"2026-06-09T17:00:00Z","provider":"openai","model":"gpt-4.1-mini","api_key":"client-key","request_id":"req-2","tokens":{"input_tokens":1,"total_tokens":1}}`
	if _, errUsage := handler.repo.AppendUsage(context.Background(), payload, "192.0.2.10"); errUsage != nil {
		t.Fatalf("AppendUsage() error = %v", errUsage)
	}
	boundaryPayload := `{"timestamp":"2026-06-10T16:00:00Z","provider":"openai","model":"gpt-4.1-mini","api_key":"client-key","request_id":"req-next-midnight","tokens":{"input_tokens":1,"total_tokens":1}}`
	if _, errUsage := handler.repo.AppendUsage(context.Background(), boundaryPayload, "192.0.2.10"); errUsage != nil {
		t.Fatalf("AppendUsage(boundary) error = %v", errUsage)
	}

	resp := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(resp)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/billing/overview?from=2026-06-09T16%3A00%3A00Z&to=2026-06-10T16%3A00%3A00Z&timezone=Asia%2FShanghai", nil)
	handler.GetBillingOverview(ctx)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200", resp.Code, resp.Body.String())
	}
	var response map[string]any
	if errDecode := json.Unmarshal(resp.Body.Bytes(), &response); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	overview, ok := response["overview"].(map[string]any)
	if !ok {
		t.Fatalf("overview = %T, want object", response["overview"])
	}
	if overview["request_count"] != float64(2) || overview["total_charge_amount"] != float64(4) {
		t.Fatalf("overview totals = requests %v amount %v, want 2 and 4", overview["request_count"], overview["total_charge_amount"])
	}
	trend, ok := overview["daily_trend"].([]any)
	if !ok || len(trend) != 1 {
		t.Fatalf("daily_trend = %#v, want one point", overview["daily_trend"])
	}
	point, ok := trend[0].(map[string]any)
	if !ok || point["date"] != "2026-06-10" || point["request_count"] != float64(2) {
		t.Fatalf("daily_trend[0] = %#v, want June 10 with 2 requests", trend[0])
	}
	rangeValue, ok := overview["range"].(map[string]any)
	if !ok ||
		rangeValue["from"] != "2026-06-10" ||
		rangeValue["to"] != "2026-06-10" ||
		rangeValue["from_at"] != "2026-06-09T16:00:00Z" ||
		rangeValue["to_at_exclusive"] != "2026-06-10T16:00:00Z" ||
		rangeValue["timezone"] != "Asia/Shanghai" {
		t.Fatalf("range = %#v, want June 10 in Asia/Shanghai", overview["range"])
	}
}

func TestGetBillingOverviewUsesConfiguredReportTimezoneAndAllowsOverride(t *testing.T) {
	t.Parallel()

	handler, closeRepo := newBillingManagementTestHandler(t)
	defer closeRepo()
	seedBillingManagementCharge(t, handler)
	payload := `{"timestamp":"2026-06-09T17:00:00Z","provider":"openai","model":"gpt-4.1-mini","api_key":"client-key","request_id":"req-configured-timezone","tokens":{"input_tokens":1,"total_tokens":1}}`
	if _, errUsage := handler.repo.AppendUsage(context.Background(), payload, "192.0.2.10"); errUsage != nil {
		t.Fatalf("AppendUsage() error = %v", errUsage)
	}
	reportTimezone := "Asia/Shanghai"
	if _, errSettings := handler.repo.UpdateBillingSettings(context.Background(), cluster.BillingSettingsPatch{ReportTimezone: &reportTimezone}); errSettings != nil {
		t.Fatalf("UpdateBillingSettings() error = %v", errSettings)
	}

	assertOverview := func(path string, wantTimezone string, wantRequests float64) {
		t.Helper()
		resp := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(resp)
		ctx.Request = httptest.NewRequest(http.MethodGet, path, nil)
		handler.GetBillingOverview(ctx)
		if resp.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d body=%s, want 200", path, resp.Code, resp.Body.String())
		}
		var response map[string]any
		if errDecode := json.Unmarshal(resp.Body.Bytes(), &response); errDecode != nil {
			t.Fatalf("decode response: %v", errDecode)
		}
		overview, ok := response["overview"].(map[string]any)
		if !ok {
			t.Fatalf("overview = %T, want object", response["overview"])
		}
		rangeValue, ok := overview["range"].(map[string]any)
		if !ok || rangeValue["timezone"] != wantTimezone || overview["request_count"] != wantRequests {
			t.Fatalf("overview = %#v, want timezone %s and requests %v", overview, wantTimezone, wantRequests)
		}
	}

	assertOverview("/billing/overview?from=2026-06-10&to=2026-06-10", "Asia/Shanghai", 2)
	assertOverview("/billing/overview?from=2026-06-10&to=2026-06-10&timezone=UTC", "UTC", 1)
}

func TestListBillingChargesReturnsItems(t *testing.T) {
	t.Parallel()

	handler, closeRepo := newBillingManagementTestHandler(t)
	defer closeRepo()
	seedBillingManagementCharge(t, handler)

	resp := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(resp)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/billing/charges?limit=10", nil)

	handler.ListBillingCharges(ctx)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200", resp.Code, resp.Body.String())
	}
	var payload map[string]any
	if errDecode := json.Unmarshal(resp.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if payload["total"] != float64(1) {
		t.Fatalf("total = %v, want 1", payload["total"])
	}
	items, ok := payload["items"].([]any)
	if !ok {
		t.Fatalf("items = %T, want array", payload["items"])
	}
	if len(items) != 1 {
		t.Fatalf("item count = %d, want 1", len(items))
	}
	item, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("item = %T, want object", items[0])
	}
	if item["amount"] != float64(2) {
		t.Fatalf("item.amount = %v, want 2", item["amount"])
	}
}

func TestBillingChargeQueryDateOnlyToNormalizesToNextMidnight(t *testing.T) {
	t.Parallel()

	resp := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(resp)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/billing/charges?to=2026-06-10", nil)

	query, ok := billingChargeQueryFromRequest(ctx, cluster.BillingReportTimezoneUTC)
	if !ok {
		t.Fatalf("billingChargeQueryFromRequest() ok = false, status=%d body=%s", resp.Code, resp.Body.String())
	}
	if query.ToExclusive == nil {
		t.Fatal("query.ToExclusive is nil")
	}
	want := time.Date(2026, time.June, 11, 0, 0, 0, 0, time.UTC)
	if !query.ToExclusive.Equal(want) {
		t.Fatalf("query.ToExclusive = %s, want %s", query.ToExclusive.Format(time.RFC3339Nano), want.Format(time.RFC3339Nano))
	}
}

func TestBillingOverviewQueryUsesTimezoneForDateOnlyRange(t *testing.T) {
	t.Parallel()

	resp := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(resp)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/billing/overview?from=2026-07-19&to=2026-07-19&timezone=Asia%2FShanghai", nil)

	query, ok := billingOverviewQueryFromRequest(ctx, cluster.BillingReportTimezoneUTC)
	if !ok {
		t.Fatalf("billingOverviewQueryFromRequest() ok = false, status=%d body=%s", resp.Code, resp.Body.String())
	}
	if query.Timezone != "Asia/Shanghai" {
		t.Fatalf("query.Timezone = %q, want Asia/Shanghai", query.Timezone)
	}
	if query.From == nil || query.ToExclusive == nil {
		t.Fatalf("query range = %v to %v, want both boundaries", query.From, query.ToExclusive)
	}
	wantFrom := time.Date(2026, time.July, 18, 16, 0, 0, 0, time.UTC)
	wantTo := time.Date(2026, time.July, 19, 16, 0, 0, 0, time.UTC)
	if !query.From.Equal(wantFrom) || !query.ToExclusive.Equal(wantTo) {
		t.Fatalf("query range = %s to %s, want %s to %s", query.From.Format(time.RFC3339Nano), query.ToExclusive.Format(time.RFC3339Nano), wantFrom.Format(time.RFC3339Nano), wantTo.Format(time.RFC3339Nano))
	}

	dstResp := httptest.NewRecorder()
	dstCtx, _ := gin.CreateTestContext(dstResp)
	dstCtx.Request = httptest.NewRequest(http.MethodGet, "/billing/overview?from=2026-03-08&to=2026-03-08&timezone=America%2FLos_Angeles", nil)
	dstQuery, ok := billingOverviewQueryFromRequest(dstCtx, cluster.BillingReportTimezoneUTC)
	if !ok || dstQuery.From == nil || dstQuery.ToExclusive == nil {
		t.Fatalf("DST query = %#v ok=%v, status=%d body=%s", dstQuery, ok, dstResp.Code, dstResp.Body.String())
	}
	wantDSTFrom := time.Date(2026, time.March, 8, 8, 0, 0, 0, time.UTC)
	wantDSTTo := time.Date(2026, time.March, 9, 7, 0, 0, 0, time.UTC)
	if !dstQuery.From.Equal(wantDSTFrom) || !dstQuery.ToExclusive.Equal(wantDSTTo) {
		t.Fatalf("DST query range = %s to %s, want %s to %s", dstQuery.From.Format(time.RFC3339Nano), dstQuery.ToExclusive.Format(time.RFC3339Nano), wantDSTFrom.Format(time.RFC3339Nano), wantDSTTo.Format(time.RFC3339Nano))
	}
	if got := dstQuery.ToExclusive.Sub(*dstQuery.From); got != 23*time.Hour {
		t.Fatalf("spring DST range duration = %s, want 23h", got)
	}

	fallResp := httptest.NewRecorder()
	fallCtx, _ := gin.CreateTestContext(fallResp)
	fallCtx.Request = httptest.NewRequest(http.MethodGet, "/billing/overview?from=2026-11-01&to=2026-11-01&timezone=America%2FLos_Angeles", nil)
	fallQuery, ok := billingOverviewQueryFromRequest(fallCtx, cluster.BillingReportTimezoneUTC)
	if !ok || fallQuery.From == nil || fallQuery.ToExclusive == nil {
		t.Fatalf("fall DST query = %#v ok=%v, status=%d body=%s", fallQuery, ok, fallResp.Code, fallResp.Body.String())
	}
	if got := fallQuery.ToExclusive.Sub(*fallQuery.From); got != 25*time.Hour {
		t.Fatalf("fall DST range duration = %s, want 25h", got)
	}
}

func TestBillingRangeQueryRejectsInvalidTimezoneAndInvertedRange(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		path      string
		wantError string
	}{
		{name: "invalid timezone", path: "/billing/overview?timezone=Invalid%2FTimezone", wantError: "invalid_timezone"},
		{name: "inverted range", path: "/billing/overview?from=2026-07-21&to=2026-07-19", wantError: "invalid_time_range"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(resp)
			ctx.Request = httptest.NewRequest(http.MethodGet, tt.path, nil)

			if _, ok := billingOverviewQueryFromRequest(ctx, cluster.BillingReportTimezoneUTC); ok {
				t.Fatal("billingOverviewQueryFromRequest() ok = true, want false")
			}
			if resp.Code != http.StatusBadRequest {
				t.Fatalf("status = %d body=%s, want 400", resp.Code, resp.Body.String())
			}
			var payload map[string]any
			if errDecode := json.Unmarshal(resp.Body.Bytes(), &payload); errDecode != nil {
				t.Fatalf("decode response: %v", errDecode)
			}
			if payload["error"] != tt.wantError {
				t.Fatalf("error = %v, want %s", payload["error"], tt.wantError)
			}
		})
	}
}

func seedBillingManagementCharge(t *testing.T, handler *Handler) {
	t.Helper()

	ctx := context.Background()
	username := "billing-user"
	credits := 100.0
	user, errCreateUser := handler.repo.CreateUser(ctx, cluster.UserUpdate{Username: &username, Credits: &credits})
	if errCreateUser != nil {
		t.Fatalf("CreateUser() error = %v", errCreateUser)
	}
	key := "client-key"
	if _, errCreateKey := handler.repo.CreateAPIKeyForUser(ctx, user.ID, cluster.APIKeyUserUpdate{APIKey: &key}); errCreateKey != nil {
		t.Fatalf("CreateAPIKeyForUser() error = %v", errCreateKey)
	}
	if _, errCreatePrice := handler.repo.CreateBillingModelPrice(ctx, cluster.BillingModelPriceUpdate{Provider: "openai", Model: "gpt-4.1-mini", RequestPrice: 2, Enabled: true}); errCreatePrice != nil {
		t.Fatalf("CreateBillingModelPrice() error = %v", errCreatePrice)
	}
	if _, errRecharge := handler.repo.ApplyBillingBalanceRecord(ctx, cluster.BillingBalanceUpdate{UserID: user.ID, Type: cluster.BillingBalanceTypeRecharge, Amount: 50, Operator: "admin"}); errRecharge != nil {
		t.Fatalf("ApplyBillingBalanceRecord() error = %v", errRecharge)
	}
	payload := `{"timestamp":"2026-06-10T01:02:03Z","provider":"openai","model":"gpt-4.1-mini","api_key":"client-key","request_id":"req-1","tokens":{"input_tokens":1,"total_tokens":1}}`
	if _, errUsage := handler.repo.AppendUsage(ctx, payload, "192.0.2.10"); errUsage != nil {
		t.Fatalf("AppendUsage() error = %v", errUsage)
	}
}

func newBillingManagementTestHandler(t *testing.T) (*Handler, func()) {
	t.Helper()

	ctx := context.Background()
	db, errOpen := cluster.OpenSQLite(ctx, filepath.Join(t.TempDir(), "home.db"))
	if errOpen != nil {
		t.Fatalf("OpenSQLite() error = %v", errOpen)
	}
	sqlDB, errDB := db.DB()
	if errDB != nil {
		t.Fatalf("db.DB() error = %v", errDB)
	}
	closeRepo := func() {
		if errClose := sqlDB.Close(); errClose != nil {
			t.Errorf("close sqlite db: %v", errClose)
		}
	}
	if errMigrate := cluster.AutoMigrate(db); errMigrate != nil {
		closeRepo()
		t.Fatalf("AutoMigrate() error = %v", errMigrate)
	}
	return NewHandler(cluster.NewRepository(db), nil, "192.0.2.10", 0), closeRepo
}

func TestOpenRouterTimeOverrideDimensionCharges(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	handler, closeRepo := newBillingManagementTestHandler(t)
	defer closeRepo()
	username, key, credits := "time-dimensions@example.com", "time-dimensions", 100.0
	user, errUser := handler.repo.CreateUser(ctx, cluster.UserUpdate{Username: &username, Credits: &credits})
	if errUser != nil {
		t.Fatal(errUser)
	}
	if _, errKey := handler.repo.CreateAPIKeyForUser(ctx, user.ID, cluster.APIKeyUserUpdate{APIKey: &key}); errKey != nil {
		t.Fatal(errKey)
	}
	raw := []byte(`{"data":[{"id":"openai/media-model","pricing":{"prompt":"0.000001","completion":"0.000002","image":"0.000003","image_output":"0.000010","audio":"0.000004","audio_output":"0.000005","internal_reasoning":"0.000006","input_cache_write_1h":"0.000010","overrides":[{"utc_start":900,"utc_end":1700,"prompt":"0.000001","completion":"0.000002","image_output":"0.000020","audio":"0","input_cache_read":"0"}]}}]}`)
	catalog, errParse := parseOpenRouterBillingImportCatalog(raw, "inline://openrouter", time.Now())
	if errParse != nil {
		t.Fatal(errParse)
	}
	preview, errPreview := handler.repo.CreateBillingModelPriceImportPreview(ctx, cluster.BillingModelPriceImportPreviewInput{
		Source: "openrouter", Targets: []cluster.BillingModelPriceImportTarget{{Provider: "openai", Model: "media-model"}},
		Policy: cluster.BillingModelPriceImportPolicy{OverwriteMode: "missing", DefaultMultiplier: 1},
	}, catalog)
	if errPreview != nil {
		t.Fatal(errPreview)
	}
	if len(preview.Rows) != 1 || !preview.Rows[0].Applicable {
		t.Fatalf("preview=%+v", preview)
	}
	if _, errApply := handler.repo.ApplyBillingModelPriceImport(ctx, cluster.BillingModelPriceImportApplyInput{
		PreviewID: preview.PreviewID, PreviewRevision: preview.PreviewRevision, SelectedKeys: []string{preview.Rows[0].RowKey}, IdempotencyKey: "time-dimensions",
	}); errApply != nil {
		t.Fatal(errApply)
	}
	expected := make(map[string]float64)
	for _, tc := range []struct {
		field          string
		output         bool
		base, override float64
	}{
		{"image_input_tokens", false, .003, .001},
		{"image_output_tokens", true, .01, .02},
		{"audio_input_tokens", false, .004, 0},
		{"audio_output_tokens", true, .005, .002},
		{"reasoning_tokens", true, .006, .002},
		{"cache_read_tokens", false, .0001, 0},
		{"cache_creation_1h_tokens", false, .01, .001},
	} {
		for _, hour := range []int{8, 10} {
			requestID := fmt.Sprintf("%s-%d", tc.field, hour)
			expected[requestID] = tc.base
			if hour == 10 {
				expected[requestID] = tc.override
			}
			input, output, uncached, cacheRead, cacheWrite, nonReasoning, reasoning := 1000, 0, 1000, 0, 0, 0, 0
			if tc.output {
				input, output, uncached, nonReasoning = 0, 1000, 0, 1000
			}
			switch tc.field {
			case "reasoning_tokens":
				nonReasoning, reasoning = 0, 1000
			case "cache_read_tokens":
				uncached, cacheRead = 0, 1000
			case "cache_creation_1h_tokens":
				uncached, cacheWrite = 0, 1000
			}
			tokens := map[string]any{"input_tokens": input, "output_tokens": output, "total_tokens": 1000, "cache_read_tokens": cacheRead, "cache_read_tokens_present": true, "cache_creation_tokens": cacheWrite}
			tokens[tc.field] = 1000
			payload, errMarshal := json.Marshal(map[string]any{
				"timestamp": fmt.Sprintf("2026-10-27T%02d:00:00Z", hour), "provider": "openai", "model": "media-model", "api_key": key, "request_id": requestID,
				"accounting_version": 2, "tokens": tokens,
				"token_breakdown": map[string]any{"schema_version": 2, "quality": "complete", "total_tokens": 1000,
					"input":  map[string]any{"total_tokens": input, "uncached_tokens": uncached, "cache_read_tokens": cacheRead, "cache_write_tokens": cacheWrite},
					"output": map[string]any{"total_tokens": output, "non_reasoning_tokens": nonReasoning, "reasoning_tokens": reasoning}},
			})
			if errMarshal != nil {
				t.Fatal(errMarshal)
			}
			usage, errUsage := handler.repo.AppendUsage(ctx, string(payload), "192.0.2.1")
			if errUsage != nil {
				t.Fatal(errUsage)
			}
			if usage.TokenAccountingQuality != cluster.UsageTokenAccountingQualityComplete {
				t.Fatalf("%s: incomplete accounting", requestID)
			}
		}
	}
	charges, errCharges := handler.repo.ListBillingCharges(ctx, cluster.BillingChargeQuery{UserID: &user.ID, Limit: 100})
	if errCharges != nil {
		t.Fatal(errCharges)
	}
	if len(charges.Records) != len(expected) {
		t.Fatalf("charges=%d, want %d", len(charges.Records), len(expected))
	}
	for _, charge := range charges.Records {
		want := expected[charge.RequestID]
		delta := charge.BalanceBefore - charge.BalanceAfter - want
		if charge.Amount != want || delta > 1e-10 || delta < -1e-10 || charge.CostDiagnostic == cluster.BillingCostDiagnosticPricingExprError {
			t.Errorf("%s: amount=%v delta=%v want=%v breakdown=%s", charge.RequestID, charge.Amount, charge.BalanceBefore-charge.BalanceAfter, want, charge.CostBreakdown)
		}
	}
}

func TestPreviewAndApplyBillingModelPriceImportOpenRouterAndLiteLLM(t *testing.T) {
	t.Parallel()

	handler, closeRepo := newBillingManagementTestHandler(t)
	defer closeRepo()

	// Seed usage for auto_discover test.
	_, errUsage := handler.repo.AppendUsage(context.Background(), `{"timestamp":"2026-10-27T01:00:00Z","provider":"anthropic","model":"claude-opus-4-6","request_id":"seed-1","tokens":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}`, "192.0.2.10")
	if errUsage != nil {
		t.Fatalf("AppendUsage() error = %v", errUsage)
	}

	// 1. OpenRouter catalog import with version delimiter matching (claude-opus-4.6 -> claude-opus-4-6),
	// 1h cache write, web_search, and time-of-day overrides synthesized into pricing_expr.
	openRouterJSON := `{
  "data": [
    {
      "id": "anthropic/claude-opus-4.6",
      "name": "Anthropic: Claude Opus 4.6",
      "pricing": {
        "prompt": "0.000005",
        "completion": "0.000025",
        "input_cache_read": "0.0000005",
        "input_cache_write": "0.00000625",
        "input_cache_write_1h": "0.00001",
        "web_search": "0.01"
      }
    },
    {
      "id": "deepseek/deepseek-v4.1-flash",
      "name": "DeepSeek V4.1 Flash",
      "pricing": {
        "prompt": "0.0000003",
        "completion": "0.0000012",
        "input_cache_read": "0.00000003",
        "overrides": [
          {
            "utc_days": ["saturday", "sunday"],
            "prompt": "0.00000015",
            "completion": "0.0000006",
            "input_cache_read": "0.000000015"
          }
        ]
      }
    },
    {
      "id": "openai/gpt-time-req",
      "name": "GPT Time Request Fee",
      "pricing": {
        "prompt": "0.000001",
        "completion": "0.000002",
        "request": "0.01",
        "overrides": [
          {
            "utc_days": ["sunday"],
            "prompt": "0.0000005",
            "completion": "0.000001",
            "request": "0.01"
          }
        ]
      }
    }
  ]
}`
	previewResp := httptest.NewRecorder()
	previewCtx, _ := gin.CreateTestContext(previewResp)
	previewBody := fmt.Sprintf(`{"source":"openrouter","catalog_json":%s,"targets":[{"provider":"anthropic","model":"claude-opus-4-6"},{"provider":"deepseek","model":"deepseek-v4.1-flash"},{"provider":"openai","model":"gpt-time-req"}],"policy":{"overwrite_mode":"all","default_multiplier":1}}`, openRouterJSON)
	previewCtx.Request = httptest.NewRequest(http.MethodPost, "/billing/model-prices/import/preview", strings.NewReader(previewBody))
	previewCtx.Request.Header.Set("Content-Type", "application/json")
	handler.PreviewBillingModelPriceImport(previewCtx)
	if previewResp.Code != http.StatusOK {
		t.Fatalf("OpenRouter preview status = %d body=%s", previewResp.Code, previewResp.Body.String())
	}
	var preview cluster.BillingModelPriceImportPreview
	if errDecode := json.Unmarshal(previewResp.Body.Bytes(), &preview); errDecode != nil {
		t.Fatalf("decode OpenRouter preview: %v", errDecode)
	}
	if len(preview.Rows) != 3 || !preview.Rows[0].Applicable || !preview.Rows[1].Applicable || !preview.Rows[2].Applicable {
		t.Fatalf("OpenRouter preview rows = %#v", preview.Rows)
	}
	if preview.Rows[0].Final.CacheWrite1h == nil || *preview.Rows[0].Final.CacheWrite1h != 10 || preview.Rows[0].Final.WebSearch == nil || *preview.Rows[0].Final.WebSearch != 0.01 {
		t.Fatalf("OpenRouter claude extra prices = %#v", preview.Rows[0].Final)
	}
	if !strings.Contains(preview.Rows[1].Final.PricingExpr, "weekday('UTC')") {
		t.Fatalf("OpenRouter deepseek pricing_expr = %q, want weekday('UTC')", preview.Rows[1].Final.PricingExpr)
	}
	if !strings.Contains(preview.Rows[2].Final.PricingExpr, "req * usd(0.01)") {
		t.Fatalf("OpenRouter gpt-time-req pricing_expr = %q, want req * usd(0.01)", preview.Rows[2].Final.PricingExpr)
	}

	applyResp := httptest.NewRecorder()
	applyCtx, _ := gin.CreateTestContext(applyResp)
	applyBody := fmt.Sprintf(`{"preview_id":%q,"preview_revision":%q,"selected_keys":[%q,%q,%q],"confirm_overwrite":true,"idempotency_key":"or-apply-1"}`, preview.PreviewID, preview.PreviewRevision, preview.Rows[0].RowKey, preview.Rows[1].RowKey, preview.Rows[2].RowKey)
	applyCtx.Request = httptest.NewRequest(http.MethodPost, "/billing/model-prices/import/apply", strings.NewReader(applyBody))
	applyCtx.Request.Header.Set("Content-Type", "application/json")
	handler.ApplyBillingModelPriceImport(applyCtx)
	if applyResp.Code != http.StatusOK {
		t.Fatalf("OpenRouter apply status = %d body=%s", applyResp.Code, applyResp.Body.String())
	}
	prices, errList := handler.repo.ListBillingModelPrices(context.Background(), cluster.BillingModelPriceQuery{Provider: "anthropic", Model: "claude-opus-4-6"})
	if errList != nil || len(prices) != 1 || prices[0].CacheWrite1hPricePerMillion == nil || *prices[0].CacheWrite1hPricePerMillion != 10 || prices[0].WebSearchPricePerCall == nil || *prices[0].WebSearchPricePerCall != 0.01 {
		t.Fatalf("persisted claude price = %#v err=%v", prices, errList)
	}
	// Verify charging 100 input tokens on a Tuesday (base branch) preserves the $0.01 request fee ($0.0001 + $0.01 = $0.0101).
	if _, errUsageReq := handler.repo.AppendUsage(context.Background(), `{"timestamp":"2026-06-09T12:00:00Z","provider":"openai","model":"gpt-time-req","api_key":"client-key-import","request_id":"req-or-time","tokens":{"input_tokens":100,"total_tokens":100}}`, "192.0.2.15"); errUsageReq != nil {
		t.Fatalf("AppendUsage(gpt-time-req) error = %v", errUsageReq)
	}
	chargesReq, errChargesReq := handler.repo.ListBillingCharges(context.Background(), cluster.BillingChargeQuery{Model: "gpt-time-req", Limit: 10})
	if errChargesReq != nil || len(chargesReq.Records) == 0 || chargesReq.Records[0].Amount != 0.0101 {
		t.Fatalf("gpt-time-req charges = %+v err=%v, want 0.0101", chargesReq.Records, errChargesReq)
	}

	// 2. LiteLLM catalog import with 1h/12h cache write, reasoning, image price, and >200k context band.
	liteLLMJSON := `{
  "sample_spec": {"input_cost_per_token": 0},
  "openai/gpt-5.6-pro": {
    "litellm_provider": "openai",
    "input_cost_per_token": 0.0000025,
    "output_cost_per_token": 0.00001,
    "cache_read_input_token_cost": 0.00000025,
    "cache_creation_input_token_cost": 0.000003125,
    "cache_creation_input_token_cost_above_12hr": 0.000005,
    "output_cost_per_reasoning_token": 0.000015,
    "output_cost_per_image": 0.04,
    "search_context_cost_per_query": 0.01,
    "input_cost_per_token_above_200k_tokens": 0.000005,
    "output_cost_per_token_above_200k_tokens": 0.00002
  }
}`
	llResp := httptest.NewRecorder()
	llCtx, _ := gin.CreateTestContext(llResp)
	llBody := fmt.Sprintf(`{"source":"litellm","catalog_json":%s,"targets":[{"provider":"openai","model":"gpt-5.6-pro"}],"policy":{"overwrite_mode":"missing","default_multiplier":1}}`, liteLLMJSON)
	llCtx.Request = httptest.NewRequest(http.MethodPost, "/billing/model-prices/import/preview", strings.NewReader(llBody))
	llCtx.Request.Header.Set("Content-Type", "application/json")
	handler.PreviewBillingModelPriceImport(llCtx)
	if llResp.Code != http.StatusOK {
		t.Fatalf("LiteLLM preview status = %d body=%s", llResp.Code, llResp.Body.String())
	}
	var llPreview cluster.BillingModelPriceImportPreview
	if errDecode := json.Unmarshal(llResp.Body.Bytes(), &llPreview); errDecode != nil {
		t.Fatalf("decode LiteLLM preview: %v", errDecode)
	}
	if len(llPreview.Rows) != 2 || llPreview.Rows[0].Final.CacheWrite12h == nil || *llPreview.Rows[0].Final.CacheWrite12h != 5 || llPreview.Rows[0].Final.Reasoning == nil || *llPreview.Rows[0].Final.Reasoning != 15 || llPreview.Rows[0].Final.Image == nil || *llPreview.Rows[0].Final.Image != 0.04 || llPreview.Rows[1].MinInputTokens != 200001 {
		t.Fatalf("LiteLLM preview rows = %#v", llPreview.Rows)
	}
}
