package management

import (
	"context"
	"crypto/tls"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
)

func TestUsageRecordsRejectsInvalidRange(t *testing.T) {
	handler, closeRepo := newUsageObservabilityTestHandler(t)
	defer closeRepo()

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.GET("/usage/records", handler.ListUsageRecords)

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/usage/records?min_latency_ms=20&max_latency_ms=10", nil)
	engine.ServeHTTP(resp, req)

	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", resp.Code, http.StatusBadRequest)
	}
}

func TestParseUsageDateOnlyRangeUsesDSTSafeExclusiveEnd(t *testing.T) {
	t.Parallel()

	location, errLocation := time.LoadLocation("America/New_York")
	if errLocation != nil {
		t.Fatalf("LoadLocation() error = %v", errLocation)
	}
	tests := []struct {
		name     string
		date     string
		wantFrom time.Time
		wantTo   time.Time
	}{
		{
			name:     "spring forward",
			date:     "2026-03-08",
			wantFrom: time.Date(2026, time.March, 8, 5, 0, 0, 0, time.UTC),
			wantTo:   time.Date(2026, time.March, 9, 4, 0, 0, 0, time.UTC),
		},
		{
			name:     "fall back",
			date:     "2026-11-01",
			wantFrom: time.Date(2026, time.November, 1, 4, 0, 0, 0, time.UTC),
			wantTo:   time.Date(2026, time.November, 2, 5, 0, 0, 0, time.UTC),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			from, to, errRange := parseUsageTimeRange(test.date, test.date, location)
			if errRange != nil {
				t.Fatalf("parseUsageTimeRange() error = %v", errRange)
			}
			if from == nil || !from.Equal(test.wantFrom) {
				t.Fatalf("from = %v, want %v", from, test.wantFrom)
			}
			if to == nil || !to.Equal(test.wantTo) {
				t.Fatalf("to = %v, want %v", to, test.wantTo)
			}
		})
	}
}

func TestUsageAggregatesRejectsInvalidGroupBy(t *testing.T) {
	handler, closeRepo := newUsageObservabilityTestHandler(t)
	defer closeRepo()

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.GET("/usage/aggregates", handler.ListUsageAggregates)

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/usage/aggregates?group_by=payload", nil)
	engine.ServeHTTP(resp, req)

	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", resp.Code, http.StatusBadRequest)
	}
}

func TestGetCapabilitiesReturnsUsageObservabilityFlags(t *testing.T) {
	handler, closeRepo := newUsageObservabilityTestHandler(t)
	defer closeRepo()

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.GET("/capabilities", handler.GetCapabilities)

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/capabilities", nil)
	engine.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want %d", resp.Code, resp.Body.String(), http.StatusOK)
	}
	var payload map[string]any
	if errDecode := json.Unmarshal(resp.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	capabilities, ok := payload["capabilities"].(map[string]any)
	if !ok {
		t.Fatalf("capabilities = %T, want object", payload["capabilities"])
	}
	for _, key := range []string{"quota_snapshots", "quota_snapshot_details", "usage", "usage_overview", "usage_records", "usage_record_details", "usage_aggregates", "usage_export", "usage_provider_health", "usage_credential_health", "usage_realtime", "usage_token_breakdown_v2", "request_log_index", "request_events", "request_event_details", "request_event_export", "request_event_filters", "request_events_details", "request_events_export", "request_events_filters", "requestEvents", "requestEventDetails", "requestEventExport", "requestEventFilters", "requestEventsDetails", "requestEventsExport", "requestEventsFilters", "oauth_usage", "logs", "request_error_logs", "model_channel_bindings", "topology", "credential_cooldown_reset"} {
		if capabilities[key] != true {
			t.Fatalf("capabilities[%s] = %v, want true", key, capabilities[key])
		}
	}
	serverInfo, ok := payload["server_info"].(map[string]any)
	if !ok {
		t.Fatalf("server_info = %T, want object", payload["server_info"])
	}
	for _, key := range []string{"home_version", "home_commit", "home_build_date"} {
		if _, ok := serverInfo[key]; !ok {
			t.Fatalf("server_info missing %s: %#v", key, serverInfo)
		}
	}
}

func TestGetCapabilitiesKeepsTokenBreakdownDisabledUntilBackfill(t *testing.T) {
	db, errOpen := cluster.OpenSQLite(t.Context(), filepath.Join(t.TempDir(), "home.db"))
	if errOpen != nil {
		t.Fatalf("OpenSQLite() error = %v", errOpen)
	}
	sqlDB, errDB := db.DB()
	if errDB != nil {
		t.Fatalf("db.DB() error = %v", errDB)
	}
	defer func() {
		if errClose := sqlDB.Close(); errClose != nil {
			t.Errorf("close sqlite db: %v", errClose)
		}
	}()
	if errMigrate := cluster.AutoMigrate(db); errMigrate != nil {
		t.Fatalf("AutoMigrate() error = %v", errMigrate)
	}
	legacy := cluster.UsageRecord{
		Timestamp:   time.Date(2026, time.July, 23, 1, 0, 0, 0, time.UTC),
		Provider:    "openai",
		InputTokens: 10,
		TotalTokens: 10,
		PayloadJSON: cluster.JSONB(`{"timestamp":"2026-07-23T01:00:00Z"}`),
		CreatedAt:   time.Now().UTC(),
	}
	if errCreate := db.Create(&legacy).Error; errCreate != nil {
		t.Fatalf("create legacy usage row: %v", errCreate)
	}

	handler := NewHandler(cluster.NewRepository(db), nil, "192.0.2.10", 0)
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.GET("/capabilities", handler.GetCapabilities)

	resp := httptest.NewRecorder()
	engine.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/capabilities", nil))
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want %d", resp.Code, resp.Body.String(), http.StatusOK)
	}
	var payload map[string]any
	if errDecode := json.Unmarshal(resp.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	capabilities, ok := payload["capabilities"].(map[string]any)
	if !ok {
		t.Fatalf("capabilities = %T, want object", payload["capabilities"])
	}
	if capabilities["usage_token_breakdown_v2"] != false {
		t.Fatalf("usage_token_breakdown_v2 = %v, want false", capabilities["usage_token_breakdown_v2"])
	}
}

func TestListUsageRecordsReturnsJoinedItems(t *testing.T) {
	handler, closeRepo := newUsageObservabilityTestHandler(t)
	defer closeRepo()
	seedUsageObservabilityManagementRecord(t, handler)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.GET("/usage/records", handler.ListUsageRecords)

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/usage/records?limit=10", nil)
	engine.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want %d", resp.Code, resp.Body.String(), http.StatusOK)
	}
	if strings.Contains(resp.Body.String(), "client-key-secret") {
		t.Fatalf("response leaked raw client key: %s", resp.Body.String())
	}

	var payload map[string]any
	if errDecode := json.Unmarshal(resp.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if payload["total"] != float64(1) {
		t.Fatalf("total = %v, want 1", payload["total"])
	}
	items, ok := payload["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("items = %#v, want one item", payload["items"])
	}
	item, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("item = %T, want object", items[0])
	}
	assertUsageTokenBreakdown(t, item["token_breakdown"], 150, 100, 50)
	client, ok := item["client"].(map[string]any)
	if !ok {
		t.Fatalf("client = %T, want object", item["client"])
	}
	if client["api_key_masked"] != "clie...1234" {
		t.Fatalf("api_key_masked = %v, want clie...1234", client["api_key_masked"])
	}
	billing, ok := item["billing"].(map[string]any)
	if !ok {
		t.Fatalf("billing = %T, want object", item["billing"])
	}
	if billing["currency"] != cluster.UsageObservabilityCurrencyCredits {
		t.Fatalf("currency = %v, want %s", billing["currency"], cluster.UsageObservabilityCurrencyCredits)
	}
	if _, ok := payload["sortable_fields"].([]any); !ok {
		t.Fatalf("sortable_fields = %T, want array", payload["sortable_fields"])
	}
}

func TestListUsageRecordsTreatsToAsExclusive(t *testing.T) {
	handler, closeRepo := newUsageObservabilityTestHandler(t)
	defer closeRepo()
	seedUsageObservabilityManagementRecord(t, handler)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.GET("/usage/records", handler.ListUsageRecords)

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/usage/records?from=2026-06-10T01:00:00Z&to=2026-06-10T01:02:03Z", nil)
	engine.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want %d", resp.Code, resp.Body.String(), http.StatusOK)
	}
	var payload map[string]any
	if errDecode := json.Unmarshal(resp.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if payload["total"] != float64(0) {
		t.Fatalf("total = %v, want 0 for record exactly at exclusive to", payload["total"])
	}
}

func TestListRequestEventsReturnsFrontendContract(t *testing.T) {
	handler, closeRepo := newUsageObservabilityTestHandler(t)
	defer closeRepo()
	seedUsageObservabilityManagementRecord(t, handler)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.GET("/request-events", handler.ListRequestEvents)

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/request-events?limit=10&event_type=completion&cpa_node=cpa-a", nil)
	engine.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want %d", resp.Code, resp.Body.String(), http.StatusOK)
	}
	if strings.Contains(resp.Body.String(), "client-key-secret") {
		t.Fatalf("response leaked raw client key: %s", resp.Body.String())
	}

	var payload map[string]any
	if errDecode := json.Unmarshal(resp.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if payload["total"] != float64(1) || payload["sort"] != "timestamp_desc" {
		t.Fatalf("page metadata = %#v, want one timestamp-desc event", payload)
	}
	items, ok := payload["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("items = %#v, want one item", payload["items"])
	}
	item, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("item = %T, want object", items[0])
	}
	if item["id"] != "evt_1" || item["event_type"] != "completion" || item["status"] != "success" || item["status_code"] != float64(201) || item["upstream_status_code"] != float64(201) {
		t.Fatalf("item identity = %#v, want request event identity", item)
	}
	runtime, ok := item["runtime"].(map[string]any)
	if !ok {
		t.Fatalf("runtime = %T, want object", item["runtime"])
	}
	if runtime["home_ip"] != "192.0.2.10" || runtime["cpa_node_id"] != "cpa-a" || runtime["cpa_label"] != "cpa-a:8317" {
		t.Fatalf("runtime = %#v, want Home and CPA ownership", runtime)
	}
	if runtime["home_port"] != float64(8327) || runtime["home_id"] != "192.0.2.10:8327" {
		t.Fatalf("runtime Home identity = %#v, want 192.0.2.10:8327", runtime)
	}
	client, ok := item["client"].(map[string]any)
	if !ok {
		t.Fatalf("client = %T, want object", item["client"])
	}
	if client["client_key_masked"] != "clie...1234" {
		t.Fatalf("client_key_masked = %v, want clie...1234", client["client_key_masked"])
	}
	if _, ok := item["related"].(map[string]any); !ok {
		t.Fatalf("related = %T, want object", item["related"])
	}
}

func TestGetRequestEventFilterOptionsReturnsDistinctOptions(t *testing.T) {
	handler, closeRepo := newUsageObservabilityTestHandler(t)
	defer closeRepo()
	seedUsageObservabilityManagementRecord(t, handler)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.GET("/request-events/filter-options", handler.GetRequestEventFilterOptions)

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/request-events/filter-options?event_type=completion&cpa_node=cpa-a&limit=99999&offset=20", nil)
	engine.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want %d", resp.Code, resp.Body.String(), http.StatusOK)
	}
	var payload map[string][]string
	if errDecode := json.Unmarshal(resp.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	for key, want := range map[string]string{
		"event_types":  "completion",
		"providers":    "openai",
		"models":       "gpt-4.1-mini",
		"home_ips":     "192.0.2.10",
		"cpa_nodes":    "cpa-a:8317",
		"status_codes": "201",
	} {
		if !stringSliceContains(payload[key], want) {
			t.Fatalf("%s = %#v, want %q", key, payload[key], want)
		}
	}
}

func TestListRequestEventsFiltersEffectiveStatusCode(t *testing.T) {
	handler, closeRepo := newUsageObservabilityTestHandler(t)
	defer closeRepo()
	seedUsageObservabilityManagementRecord(t, handler)

	payload := `{"timestamp":"2026-06-10T01:02:04Z","event_type":"completion","provider":"openai","model":"gpt-4.1-mini","api_key":"client-key-secret-1234","request_id":"req-obs-200","cpa_node_id":"cpa-a","cpa_label":"cpa-a:8317","endpoint":"/v1/chat/completions","executor_type":"CodexWebsocketsExecutor","auth_index":"auth-observability","auth_type":"oauth","latency_ms":100,"tokens":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`
	if _, errUsage := handler.repo.AppendUsageWithRuntime(context.Background(), payload, cluster.UsageRuntimeMetadata{HomeIP: "192.0.2.10", HomePort: 8327}); errUsage != nil {
		t.Fatalf("AppendUsage(200) error = %v", errUsage)
	}

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.GET("/request-events", handler.ListRequestEvents)

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/request-events?status_code=201&limit=10", nil)
	engine.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want %d", resp.Code, resp.Body.String(), http.StatusOK)
	}
	var response map[string]any
	if errDecode := json.Unmarshal(resp.Body.Bytes(), &response); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if response["total"] != float64(1) {
		t.Fatalf("total = %v, want only upstream status 201", response["total"])
	}
	items, ok := response["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("items = %#v, want one item", response["items"])
	}
	item, ok := items[0].(map[string]any)
	if !ok || item["request_id"] != "req-obs-1" {
		t.Fatalf("item = %#v, want req-obs-1", items[0])
	}
}

func TestGetRequestEventReturnsDetailWithRedactedLogExcerpt(t *testing.T) {
	handler, closeRepo := newUsageObservabilityTestHandler(t)
	defer closeRepo()
	seedUsageObservabilityManagementRecord(t, handler)

	if errMkdir := os.MkdirAll(homeLogDirectory, 0o755); errMkdir != nil {
		t.Fatalf("MkdirAll(logs) error = %v", errMkdir)
	}
	logPath := filepath.Join(homeLogDirectory, "20260610010203-req-obs-1.log")
	logBody := "request line\nAuthorization: Bearer secret-token\nresponse line\n"
	if errWrite := os.WriteFile(logPath, []byte(logBody), 0o644); errWrite != nil {
		t.Fatalf("WriteFile(log) error = %v", errWrite)
	}
	defer func() {
		if errRemove := os.Remove(logPath); errRemove != nil && !os.IsNotExist(errRemove) {
			t.Errorf("remove log: %v", errRemove)
		}
	}()

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.GET("/request-events/:id", handler.GetRequestEvent)

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/request-events/evt_1?include_payload=true&include_logs=true&include_related=true", nil)
	engine.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want %d", resp.Code, resp.Body.String(), http.StatusOK)
	}
	if strings.Contains(resp.Body.String(), "secret-token") || strings.Contains(resp.Body.String(), "client-key-secret") {
		t.Fatalf("detail response leaked secret: %s", resp.Body.String())
	}
	var payload map[string]any
	if errDecode := json.Unmarshal(resp.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	event, ok := payload["event"].(map[string]any)
	if !ok {
		t.Fatalf("event = %T, want object", payload["event"])
	}
	if event["id"] != "evt_1" || event["request_id"] != "req-obs-1" {
		t.Fatalf("event identity = %#v, want evt_1 req-obs-1", event)
	}
	related, ok := event["related"].(map[string]any)
	if !ok {
		t.Fatalf("related = %T, want object", event["related"])
	}
	requestLog, ok := related["request_log"].(map[string]any)
	if !ok {
		t.Fatalf("related.request_log = %T, want object", related["request_log"])
	}
	if requestLog["home_port"] != float64(8327) {
		t.Fatalf("request_log.home_port = %v, want 8327", requestLog["home_port"])
	}
	wantDownloadURL := "/request-log-by-id/req-obs-1?home_ip=192.0.2.10&home_port=8327"
	if requestLog["download_url"] != wantDownloadURL {
		t.Fatalf("request_log.download_url = %v, want %s", requestLog["download_url"], wantDownloadURL)
	}
	payloadSummary, ok := payload["payload_summary"].(map[string]any)
	if !ok {
		t.Fatalf("payload_summary = %T, want object", payload["payload_summary"])
	}
	if payloadSummary["body_preview"] != nil {
		t.Fatalf("payload_summary.body_preview = %v, want nil", payloadSummary["body_preview"])
	}
	excerpt, ok := payload["log_excerpt"].([]any)
	if !ok || len(excerpt) == 0 {
		t.Fatalf("log_excerpt = %#v, want non-empty array", payload["log_excerpt"])
	}
}

func TestGetRequestEventKeepsRemoteLogRoutableWhenHomePortDiffers(t *testing.T) {
	handler, closeRepo := newUsageObservabilityTestHandler(t)
	defer closeRepo()
	handler.nodePort = 8327
	handler.forwardTLSConfig = &tls.Config{}

	requestID := "req-same-ip-port"
	payload := `{"timestamp":"2026-06-10T01:02:05Z","event_type":"completion","provider":"openai","model":"gpt-4.1-mini","request_id":"req-same-ip-port","endpoint":"/v1/chat/completions","latency_ms":100,"tokens":{"total_tokens":1}}`
	record, errUsage := handler.repo.AppendUsageWithRuntime(context.Background(), payload, cluster.UsageRuntimeMetadata{HomeIP: "192.0.2.10", HomePort: 8328})
	if errUsage != nil {
		t.Fatalf("AppendUsage(remote port) error = %v", errUsage)
	}

	if errMkdir := os.MkdirAll(homeLogDirectory, 0o755); errMkdir != nil {
		t.Fatalf("MkdirAll(logs) error = %v", errMkdir)
	}
	logPath := filepath.Join(homeLogDirectory, "20260610010205-"+requestID+".log")
	if errWrite := os.WriteFile(logPath, []byte("local log should not match remote port\n"), 0o644); errWrite != nil {
		t.Fatalf("WriteFile(log) error = %v", errWrite)
	}
	defer func() {
		if errRemove := os.Remove(logPath); errRemove != nil && !os.IsNotExist(errRemove) {
			t.Errorf("remove log: %v", errRemove)
		}
	}()

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.GET("/request-events/:id", handler.GetRequestEvent)

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/request-events/evt_%d?include_logs=true", record.ID), nil)
	engine.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want %d", resp.Code, resp.Body.String(), http.StatusOK)
	}
	var response map[string]any
	if errDecode := json.Unmarshal(resp.Body.Bytes(), &response); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	event, ok := response["event"].(map[string]any)
	if !ok {
		t.Fatalf("event = %T, want object", response["event"])
	}
	related, ok := event["related"].(map[string]any)
	if !ok {
		t.Fatalf("related = %T, want object", event["related"])
	}
	requestLog, ok := related["request_log"].(map[string]any)
	if !ok {
		t.Fatalf("request_log = %T, want object", related["request_log"])
	}
	if requestLog["available"] != true {
		t.Fatalf("request_log.available = %v, want true", requestLog["available"])
	}
	wantDownloadURL := "/request-log-by-id/req-same-ip-port?home_ip=192.0.2.10&home_port=8328"
	if requestLog["download_url"] != wantDownloadURL {
		t.Fatalf("request_log.download_url = %v, want %s", requestLog["download_url"], wantDownloadURL)
	}
	excerpt, ok := response["log_excerpt"].([]any)
	if !ok || len(excerpt) != 0 {
		t.Fatalf("log_excerpt = %#v, want empty for remote-port log", response["log_excerpt"])
	}
}

func TestGetUsageRecordMarksRemoteRequestLogRoutable(t *testing.T) {
	handler, closeRepo := newUsageObservabilityTestHandler(t)
	defer closeRepo()
	handler.nodePort = 8327
	handler.forwardTLSConfig = &tls.Config{}

	payload := `{"timestamp":"2026-06-10T01:02:05Z","event_type":"completion","provider":"openai","model":"gpt-4.1-mini","request_id":"req-remote-usage-detail","endpoint":"/v1/chat/completions","latency_ms":100,"tokens":{"total_tokens":1}}`
	record, errUsage := handler.repo.AppendUsageWithRuntime(context.Background(), payload, cluster.UsageRuntimeMetadata{HomeIP: "192.0.2.20", HomePort: 8328})
	if errUsage != nil {
		t.Fatalf("AppendUsage(remote) error = %v", errUsage)
	}

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.GET("/usage/records/:id", handler.GetUsageRecord)

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/usage/records/%d", record.ID), nil)
	engine.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want %d", resp.Code, resp.Body.String(), http.StatusOK)
	}
	var response map[string]any
	if errDecode := json.Unmarshal(resp.Body.Bytes(), &response); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	usageRecord, ok := response["record"].(map[string]any)
	if !ok {
		t.Fatalf("record = %T, want object", response["record"])
	}
	runtime, ok := usageRecord["runtime"].(map[string]any)
	if !ok || runtime["request_log_available"] != true {
		t.Fatalf("record.runtime = %#v, want request_log_available=true", usageRecord["runtime"])
	}
	related, ok := response["related"].(map[string]any)
	if !ok {
		t.Fatalf("related = %T, want object", response["related"])
	}
	requestLog, ok := related["request_log"].(map[string]any)
	if !ok || requestLog["available"] != true {
		t.Fatalf("related.request_log = %#v, want available=true", related["request_log"])
	}
	wantDownloadURL := "/request-log-by-id/req-remote-usage-detail?home_ip=192.0.2.20&home_port=8328"
	if requestLog["download_url"] != wantDownloadURL {
		t.Fatalf("related.request_log.download_url = %v, want %s", requestLog["download_url"], wantDownloadURL)
	}
}

func TestExportRequestEventsReturnsJSONLWithoutRawKey(t *testing.T) {
	handler, closeRepo := newUsageObservabilityTestHandler(t)
	defer closeRepo()
	seedUsageObservabilityManagementRecord(t, handler)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.GET("/request-events/export", handler.ExportRequestEvents)

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/request-events/export?format=jsonl&event_type=completion&cpa_node=cpa-a&limit=99999&offset=10", nil)
	engine.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want %d", resp.Code, resp.Body.String(), http.StatusOK)
	}
	if disposition := resp.Header().Get("Content-Disposition"); !strings.Contains(disposition, `request-events.jsonl`) {
		t.Fatalf("Content-Disposition = %q, want request-events.jsonl", disposition)
	}
	body := resp.Body.String()
	if strings.Contains(body, "client-key-secret") {
		t.Fatalf("export leaked raw client key: %s", body)
	}
	var row map[string]any
	line := strings.TrimSpace(body)
	if errDecode := json.Unmarshal([]byte(line), &row); errDecode != nil {
		t.Fatalf("decode jsonl row: %v body=%s", errDecode, body)
	}
	for _, key := range []string{"id", "event_type", "home_ip", "cpa_node_id", "client_key_masked", "usage_record_id", "request_log_available"} {
		if _, ok := row[key]; !ok {
			t.Fatalf("export row missing %s: %#v", key, row)
		}
	}
	if row["id"] != "evt_1" || row["event_type"] != "completion" || row["cpa_node_id"] != "cpa-a" {
		t.Fatalf("export row identity = %#v, want request event fields", row)
	}
}

func TestListUsageRecordsFiltersStatusCodeAndReturnsDocumentFields(t *testing.T) {
	handler, closeRepo := newUsageObservabilityTestHandler(t)
	defer closeRepo()
	seedUsageObservabilityManagementRecord(t, handler)
	seedUsageObservabilityFailedManagementRecord(t, handler)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.GET("/usage/records", handler.ListUsageRecords)

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/usage/records?status_code=429&limit=10", nil)
	engine.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want %d", resp.Code, resp.Body.String(), http.StatusOK)
	}
	if strings.Contains(resp.Body.String(), "client-key-secret") {
		t.Fatalf("response leaked raw client key: %s", resp.Body.String())
	}

	var payload map[string]any
	if errDecode := json.Unmarshal(resp.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if payload["total"] != float64(1) {
		t.Fatalf("total = %v, want 1", payload["total"])
	}
	items, ok := payload["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("items = %#v, want one item", payload["items"])
	}
	item, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("item = %T, want object", items[0])
	}
	if item["status_code"] != float64(429) {
		t.Fatalf("status_code = %v, want 429", item["status_code"])
	}
	if item["upstream_request_id"] != "upstream-429" {
		t.Fatalf("upstream_request_id = %v, want upstream-429", item["upstream_request_id"])
	}
	if item["source"] != "responses" {
		t.Fatalf("source = %v, want responses", item["source"])
	}
	if item["service_tier"] != "priority" {
		t.Fatalf("service_tier = %v, want priority", item["service_tier"])
	}
	if item["reasoning_effort"] != "high" {
		t.Fatalf("reasoning_effort = %v, want high", item["reasoning_effort"])
	}
	client, ok := item["client"].(map[string]any)
	if !ok {
		t.Fatalf("client = %T, want object", item["client"])
	}
	if client["client_ip"] != "203.0.113.9" {
		t.Fatalf("client_ip = %v, want 203.0.113.9", client["client_ip"])
	}
	billing, ok := item["billing"].(map[string]any)
	if !ok {
		t.Fatalf("billing = %T, want object", item["billing"])
	}
	if _, ok := billing["balance_before"]; !ok {
		t.Fatalf("billing.balance_before missing: %#v", billing)
	}
	if _, ok := billing["balance_after"]; !ok {
		t.Fatalf("billing.balance_after missing: %#v", billing)
	}
}

func TestListUsageAggregatesReturnsItems(t *testing.T) {
	handler, closeRepo := newUsageObservabilityTestHandler(t)
	defer closeRepo()
	seedUsageObservabilityManagementRecord(t, handler)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.GET("/usage/aggregates", handler.ListUsageAggregates)

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/usage/aggregates?group_by=credential&metric=request_count&direction=desc&from=2026-06-10T01:00:00Z&to=2026-06-10T01:10:00Z", nil)
	engine.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want %d", resp.Code, resp.Body.String(), http.StatusOK)
	}

	var payload map[string]any
	if errDecode := json.Unmarshal(resp.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if payload["total"] != float64(1) {
		t.Fatalf("total = %v, want 1", payload["total"])
	}
	items, ok := payload["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("items = %#v, want one item", payload["items"])
	}
	item, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("item = %T, want object", items[0])
	}
	if item["id"] != "auth-observability" {
		t.Fatalf("id = %v, want auth-observability", item["id"])
	}
	if item["request_count"] != float64(1) {
		t.Fatalf("request_count = %v, want 1", item["request_count"])
	}
	assertUsageTokenBreakdown(t, item["token_breakdown"], 150, 100, 50)
	if _, ok := payload["sortable_metrics"].([]any); !ok {
		t.Fatalf("sortable_metrics = %T, want array", payload["sortable_metrics"])
	}
}

func TestListUsageAggregatesFiltersCredentialType(t *testing.T) {
	handler, closeRepo := newUsageObservabilityTestHandler(t)
	defer closeRepo()
	seedUsageObservabilityManagementRecord(t, handler)
	seedUsageObservabilityProviderAPIKeyManagementRecord(t, handler)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.GET("/usage/aggregates", handler.ListUsageAggregates)

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/usage/aggregates?group_by=credential&credential_type=oauth&metric=request_count&direction=desc&from=2026-06-10T01:00:00Z&to=2026-06-10T01:10:00Z", nil)
	engine.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want %d", resp.Code, resp.Body.String(), http.StatusOK)
	}
	var payload map[string]any
	if errDecode := json.Unmarshal(resp.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if payload["total"] != float64(1) {
		t.Fatalf("total = %v, want 1", payload["total"])
	}
	items, ok := payload["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("items = %#v, want one item", payload["items"])
	}
	item, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("item = %T, want object", items[0])
	}
	if item["id"] != "auth-observability" {
		t.Fatalf("id = %v, want auth-observability", item["id"])
	}
}

func TestGetUsageOverviewReturnsTotalsAndTopGroups(t *testing.T) {
	handler, closeRepo := newUsageObservabilityTestHandler(t)
	defer closeRepo()
	seedUsageObservabilityManagementRecord(t, handler)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.GET("/usage/overview", handler.GetUsageOverview)

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/usage/overview?from=2026-06-10T01:00:00Z&to=2026-06-10T01:10:00Z", nil)
	engine.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want %d", resp.Code, resp.Body.String(), http.StatusOK)
	}

	var payload map[string]any
	if errDecode := json.Unmarshal(resp.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	totals, ok := payload["totals"].(map[string]any)
	if !ok {
		t.Fatalf("totals = %T, want object", payload["totals"])
	}
	if totals["request_count"] != float64(1) {
		t.Fatalf("request_count = %v, want 1", totals["request_count"])
	}
	assertUsageTokenBreakdown(t, totals["token_breakdown"], 150, 100, 50)
	trend, ok := payload["trend"].([]any)
	if !ok || len(trend) == 0 {
		t.Fatalf("trend = %#v, want at least one point", payload["trend"])
	}
	trendPoint, ok := trend[0].(map[string]any)
	if !ok {
		t.Fatalf("trend[0] = %T, want object", trend[0])
	}
	assertUsageTokenBreakdown(t, trendPoint["token_breakdown"], 150, 100, 50)
	top, ok := payload["top"].(map[string]any)
	if !ok {
		t.Fatalf("top = %T, want object", payload["top"])
	}
	credentials, ok := top["credentials"].([]any)
	if !ok || len(credentials) != 1 {
		t.Fatalf("top.credentials = %#v, want one item", top["credentials"])
	}
}

func TestGetUsageOverviewUsesRequestedIntervalAndTimezone(t *testing.T) {
	handler, closeRepo := newUsageObservabilityTestHandler(t)
	defer closeRepo()
	seedUsageObservabilityManagementRecord(t, handler)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.GET("/usage/overview", handler.GetUsageOverview)

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/usage/overview?from=2026-06-10T00:00:00Z&to=2026-06-11T00:00:00Z&timezone=Asia/Shanghai&interval=day", nil)
	engine.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want %d", resp.Code, resp.Body.String(), http.StatusOK)
	}
	var payload map[string]any
	if errDecode := json.Unmarshal(resp.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	rangeValue, ok := payload["range"].(map[string]any)
	if !ok {
		t.Fatalf("range = %T, want object", payload["range"])
	}
	if rangeValue["interval"] != "day" {
		t.Fatalf("range.interval = %v, want day", rangeValue["interval"])
	}
	trend, ok := payload["trend"].([]any)
	if !ok || len(trend) != 2 {
		t.Fatalf("trend = %#v, want two contiguous points", payload["trend"])
	}
	point, ok := trend[0].(map[string]any)
	if !ok {
		t.Fatalf("trend[0] = %T, want object", trend[0])
	}
	if point["bucket_start"] != "2026-06-09T16:00:00Z" {
		t.Fatalf("bucket_start = %v, want 2026-06-09T16:00:00Z", point["bucket_start"])
	}
	activity, ok := payload["activity"].([]any)
	if !ok || len(activity) != 2 {
		t.Fatalf("activity = %#v, want two contiguous points", payload["activity"])
	}
	emptyPoint, ok := activity[1].(map[string]any)
	if !ok {
		t.Fatalf("activity[1] = %T, want object", activity[1])
	}
	if emptyPoint["request_count"] != float64(0) || emptyPoint["status"] != "empty" {
		t.Fatalf("activity[1] = %#v, want empty zero-request bucket", emptyPoint)
	}
}

func TestGetUsageOverviewTreatsToAsExclusiveBucketBoundary(t *testing.T) {
	handler, closeRepo := newUsageObservabilityTestHandler(t)
	defer closeRepo()
	seedUsageObservabilityManagementRecord(t, handler)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.GET("/usage/overview", handler.GetUsageOverview)

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/usage/overview?from=2026-06-09T16:00:00Z&to=2026-06-10T16:00:00Z&timezone=Asia/Shanghai&interval=day", nil)
	engine.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want %d", resp.Code, resp.Body.String(), http.StatusOK)
	}
	var payload map[string]any
	if errDecode := json.Unmarshal(resp.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	trend, ok := payload["trend"].([]any)
	if !ok || len(trend) != 1 {
		t.Fatalf("trend = %#v, want one day bucket", payload["trend"])
	}
	activity, ok := payload["activity"].([]any)
	if !ok || len(activity) != 1 {
		t.Fatalf("activity = %#v, want one day bucket", payload["activity"])
	}
}

func TestGetUsageOverviewRejectsExcessiveExplicitBucketCount(t *testing.T) {
	handler, closeRepo := newUsageObservabilityTestHandler(t)
	defer closeRepo()

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.GET("/usage/overview", handler.GetUsageOverview)

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/usage/overview?from=2026-06-01T00:00:00Z&to=2026-06-09T00:00:00Z&interval=minute", nil)
	engine.ServeHTTP(resp, req)

	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s, want %d", resp.Code, resp.Body.String(), http.StatusBadRequest)
	}
	if !strings.Contains(resp.Body.String(), "invalid_interval_range") {
		t.Fatalf("body = %s, want invalid_interval_range", resp.Body.String())
	}
}

func TestGetUsageOverviewUsesTimezoneForDateOnlyRange(t *testing.T) {
	handler, closeRepo := newUsageObservabilityTestHandler(t)
	defer closeRepo()
	seedUsageObservabilityManagementRecord(t, handler)
	seedUsageObservabilityTimezoneBoundaryRecord(t, handler)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.GET("/usage/overview", handler.GetUsageOverview)

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/usage/overview?from=2026-06-10&to=2026-06-10&timezone=Asia/Shanghai&interval=day", nil)
	engine.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want %d", resp.Code, resp.Body.String(), http.StatusOK)
	}
	var payload map[string]any
	if errDecode := json.Unmarshal(resp.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	totals, ok := payload["totals"].(map[string]any)
	if !ok {
		t.Fatalf("totals = %T, want object", payload["totals"])
	}
	if totals["request_count"] != float64(2) {
		t.Fatalf("request_count = %v, want 2", totals["request_count"])
	}
	trend, ok := payload["trend"].([]any)
	if !ok || len(trend) != 1 {
		t.Fatalf("trend = %#v, want one point", payload["trend"])
	}
	point, ok := trend[0].(map[string]any)
	if !ok {
		t.Fatalf("trend[0] = %T, want object", trend[0])
	}
	if point["bucket_start"] != "2026-06-09T16:00:00Z" {
		t.Fatalf("bucket_start = %v, want 2026-06-09T16:00:00Z", point["bucket_start"])
	}
}

func TestGetUsageRecordReturnsDetail(t *testing.T) {
	handler, closeRepo := newUsageObservabilityTestHandler(t)
	defer closeRepo()
	seedUsageObservabilityManagementRecord(t, handler)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.GET("/usage/records/:id", handler.GetUsageRecord)

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/usage/records/1?include_payload=true", nil)
	engine.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want %d", resp.Code, resp.Body.String(), http.StatusOK)
	}
	if strings.Contains(resp.Body.String(), "client-key-secret") {
		t.Fatalf("response leaked raw client key: %s", resp.Body.String())
	}

	var payload map[string]any
	if errDecode := json.Unmarshal(resp.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	record, ok := payload["record"].(map[string]any)
	if !ok {
		t.Fatalf("record = %T, want object", payload["record"])
	}
	if record["usage_id"] != float64(1) {
		t.Fatalf("usage_id = %v, want 1", record["usage_id"])
	}
	if _, ok := payload["payload_summary"].(map[string]any); !ok {
		t.Fatalf("payload_summary = %T, want object", payload["payload_summary"])
	}
	related, ok := payload["related"].(map[string]any)
	if !ok {
		t.Fatalf("related = %T, want object", payload["related"])
	}
	if _, ok := related["request_log"].(map[string]any); !ok {
		t.Fatalf("related.request_log = %T, want object", related["request_log"])
	}
}

func TestGetUsageRecordReturnsLogExcerptWhenRequested(t *testing.T) {
	handler, closeRepo := newUsageObservabilityTestHandler(t)
	defer closeRepo()
	seedUsageObservabilityManagementRecord(t, handler)

	if errMkdir := os.MkdirAll(homeLogDirectory, 0o755); errMkdir != nil {
		t.Fatalf("MkdirAll(logs) error = %v", errMkdir)
	}
	logPath := filepath.Join(homeLogDirectory, "20260610010203-req-obs-1.log")
	logBody := "request line\nAuthorization: Bearer secret-token\nresponse line\n"
	if errWrite := os.WriteFile(logPath, []byte(logBody), 0o644); errWrite != nil {
		t.Fatalf("WriteFile(log) error = %v", errWrite)
	}
	defer func() {
		if errRemove := os.Remove(logPath); errRemove != nil && !os.IsNotExist(errRemove) {
			t.Errorf("remove log: %v", errRemove)
		}
	}()

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.GET("/usage/records/:id", handler.GetUsageRecord)

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/usage/records/1?include_logs=true", nil)
	engine.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want %d", resp.Code, resp.Body.String(), http.StatusOK)
	}
	if strings.Contains(resp.Body.String(), "secret-token") {
		t.Fatalf("log excerpt leaked secret: %s", resp.Body.String())
	}
	var payload map[string]any
	if errDecode := json.Unmarshal(resp.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	excerpt, ok := payload["log_excerpt"].([]any)
	if !ok || len(excerpt) == 0 {
		t.Fatalf("log_excerpt = %#v, want non-empty array", payload["log_excerpt"])
	}
}

func TestExportUsageRecordsReturnsJSONLWithoutRawKey(t *testing.T) {
	handler, closeRepo := newUsageObservabilityTestHandler(t)
	defer closeRepo()
	seedUsageObservabilityManagementRecord(t, handler)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.GET("/usage/export", handler.ExportUsageRecords)

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/usage/export?format=jsonl", nil)
	engine.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want %d", resp.Code, resp.Body.String(), http.StatusOK)
	}
	body := resp.Body.String()
	if strings.Contains(body, "client-key-secret") {
		t.Fatalf("export leaked raw client key: %s", body)
	}
	if !strings.Contains(body, `"api_key_masked":"clie...1234"`) {
		t.Fatalf("export body = %s, want masked key", body)
	}
}

func TestExportUsageRecordsIncludesFlattenedSummaryFields(t *testing.T) {
	handler, closeRepo := newUsageObservabilityTestHandler(t)
	defer closeRepo()
	seedUsageObservabilityManagementRecord(t, handler)
	seedUsageObservabilityFailedManagementRecord(t, handler)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.GET("/usage/export", handler.ExportUsageRecords)

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/usage/export?format=jsonl&status_code=429", nil)
	engine.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want %d", resp.Code, resp.Body.String(), http.StatusOK)
	}
	var row map[string]any
	line := strings.TrimSpace(resp.Body.String())
	if errDecode := json.Unmarshal([]byte(line), &row); errDecode != nil {
		t.Fatalf("decode jsonl row: %v body=%s", errDecode, resp.Body.String())
	}
	for _, key := range []string{"error_status_code", "error_message", "error_body_preview", "request_log_available", "log_home_ip_required", "token_breakdown_schema_version", "token_breakdown_quality", "token_breakdown_total_tokens", "token_breakdown_input_uncached_tokens", "token_breakdown_output_non_reasoning_tokens"} {
		if _, ok := row[key]; !ok {
			t.Fatalf("export row missing %s: %#v", key, row)
		}
	}
	if row["error_status_code"] != float64(429) {
		t.Fatalf("error_status_code = %v, want 429", row["error_status_code"])
	}
	if !strings.Contains(fmt.Sprint(row["error_message"]), "rate limit exceeded") {
		t.Fatalf("error_message = %v, want rate limit text", row["error_message"])
	}
	if strings.Contains(fmt.Sprint(row["error_body_preview"]), "secret") {
		t.Fatalf("error_body_preview leaked secret: %v", row["error_body_preview"])
	}
}

func TestExportUsageRecordsAllowsLargeLimitAndIncludesTPSInCSV(t *testing.T) {
	handler, closeRepo := newUsageObservabilityTestHandler(t)
	defer closeRepo()
	seedUsageObservabilityManagementRecord(t, handler)
	seedUsageObservabilityAdditionalRecords(t, handler, 200)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.GET("/usage/export", handler.ExportUsageRecords)

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/usage/export?format=csv&limit=201", nil)
	engine.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want %d", resp.Code, resp.Body.String(), http.StatusOK)
	}
	rows, errRead := csv.NewReader(strings.NewReader(resp.Body.String())).ReadAll()
	if errRead != nil {
		t.Fatalf("read csv: %v body=%s", errRead, resp.Body.String())
	}
	if len(rows) != 202 {
		t.Fatalf("csv rows = %d, want 202 including header", len(rows))
	}
	if strings.Contains(resp.Body.String(), "<nil>") {
		t.Fatalf("csv export contains Go nil marker: %s", resp.Body.String())
	}
	header := rows[0]
	foundTPS := false
	foundTokenBreakdown := false
	for _, column := range header {
		if column == "tps" {
			foundTPS = true
		}
		if column == "token_breakdown_quality" {
			foundTokenBreakdown = true
		}
	}
	if !foundTPS {
		t.Fatalf("csv header missing tps: %v", header)
	}
	if !foundTokenBreakdown {
		t.Fatalf("csv header missing token breakdown fields: %v", header)
	}
}

func assertUsageTokenBreakdown(t *testing.T, value any, wantTotal float64, wantInput float64, wantOutput float64) {
	t.Helper()
	breakdown, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("token_breakdown = %T, want object", value)
	}
	if breakdown["schema_version"] != float64(cluster.UsageTokenAccountingSchemaVersion) || breakdown["quality"] != cluster.UsageTokenAccountingQualityComplete || breakdown["total_tokens"] != wantTotal {
		t.Fatalf("token_breakdown = %#v", breakdown)
	}
	input, ok := breakdown["input"].(map[string]any)
	if !ok || input["total_tokens"] != wantInput {
		t.Fatalf("token_breakdown.input = %#v, want total %.0f", breakdown["input"], wantInput)
	}
	output, ok := breakdown["output"].(map[string]any)
	if !ok || output["total_tokens"] != wantOutput {
		t.Fatalf("token_breakdown.output = %#v, want total %.0f", breakdown["output"], wantOutput)
	}
}

func TestGetUsageRealtimeReturnsSnapshot(t *testing.T) {
	handler, closeRepo := newUsageObservabilityTestHandler(t)
	defer closeRepo()
	seedUsageObservabilityManagementRecord(t, handler)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.GET("/usage/realtime", handler.GetUsageRealtime)

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/usage/realtime?from=2026-06-10T01:00:00Z&to=2026-06-10T01:10:00Z&bucket_seconds=60&group_by=model", nil)
	engine.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want %d", resp.Code, resp.Body.String(), http.StatusOK)
	}
	var payload map[string]any
	if errDecode := json.Unmarshal(resp.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if _, ok := payload["velocity"].([]any); !ok {
		t.Fatalf("velocity = %T, want array", payload["velocity"])
	}
	currentUsage, ok := payload["current_usage"].([]any)
	if !ok || len(currentUsage) != 1 {
		t.Fatalf("current_usage = %#v, want one item", payload["current_usage"])
	}
}

func TestGetUsageHealthReturnsProviderAndCredentialItems(t *testing.T) {
	handler, closeRepo := newUsageObservabilityTestHandler(t)
	defer closeRepo()
	seedUsageObservabilityManagementRecord(t, handler)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.GET("/usage/health/providers", handler.GetUsageProviderHealth)
	engine.GET("/usage/health/credentials", handler.GetUsageCredentialHealth)

	providerResp := httptest.NewRecorder()
	providerReq := httptest.NewRequest(http.MethodGet, "/usage/health/providers?from=2026-06-10T01:00:00Z&to=2026-06-10T01:10:00Z", nil)
	engine.ServeHTTP(providerResp, providerReq)
	if providerResp.Code != http.StatusOK {
		t.Fatalf("provider status = %d body=%s, want %d", providerResp.Code, providerResp.Body.String(), http.StatusOK)
	}

	credentialResp := httptest.NewRecorder()
	credentialReq := httptest.NewRequest(http.MethodGet, "/usage/health/credentials?from=2026-06-10T01:00:00Z&to=2026-06-10T01:10:00Z", nil)
	engine.ServeHTTP(credentialResp, credentialReq)
	if credentialResp.Code != http.StatusOK {
		t.Fatalf("credential status = %d body=%s, want %d", credentialResp.Code, credentialResp.Body.String(), http.StatusOK)
	}

	var credentialPayload map[string]any
	if errDecode := json.Unmarshal(credentialResp.Body.Bytes(), &credentialPayload); errDecode != nil {
		t.Fatalf("decode credential response: %v", errDecode)
	}
	items, ok := credentialPayload["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("credential items = %#v, want one item", credentialPayload["items"])
	}
}

func TestGetUsageHealthIncludesLastError(t *testing.T) {
	handler, closeRepo := newUsageObservabilityTestHandler(t)
	defer closeRepo()
	seedUsageObservabilityManagementRecord(t, handler)
	seedUsageObservabilityFailedManagementRecord(t, handler)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.GET("/usage/health/providers", handler.GetUsageProviderHealth)

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/usage/health/providers?from=2026-06-10T01:00:00Z&to=2026-06-10T01:10:00Z", nil)
	engine.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want %d", resp.Code, resp.Body.String(), http.StatusOK)
	}
	var payload map[string]any
	if errDecode := json.Unmarshal(resp.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	items, ok := payload["items"].([]any)
	if !ok || len(items) == 0 {
		t.Fatalf("items = %#v, want at least one item", payload["items"])
	}
	item, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("item = %T, want object", items[0])
	}
	if item["last_error_status"] != float64(429) {
		t.Fatalf("last_error_status = %v, want 429", item["last_error_status"])
	}
	if item["last_error_at"] != "2026-06-10T01:03:03Z" {
		t.Fatalf("last_error_at = %v, want 2026-06-10T01:03:03Z", item["last_error_at"])
	}
	if !strings.Contains(fmt.Sprint(item["last_error_message"]), "rate limit exceeded") {
		t.Fatalf("last_error_message = %v, want rate limit text", item["last_error_message"])
	}
}

func TestGetUsageProviderHealthIncludesNextRetryAt(t *testing.T) {
	handler, closeRepo := newUsageObservabilityTestHandler(t)
	defer closeRepo()
	seedUsageObservabilityManagementRecordWithRetry(t, handler)
	seedUsageObservabilityFailedManagementRecord(t, handler)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.GET("/usage/health/providers", handler.GetUsageProviderHealth)

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/usage/health/providers?from=2026-06-10T01:00:00Z&to=2026-06-10T01:10:00Z", nil)
	engine.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want %d", resp.Code, resp.Body.String(), http.StatusOK)
	}
	var payload map[string]any
	if errDecode := json.Unmarshal(resp.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	items, ok := payload["items"].([]any)
	if !ok || len(items) == 0 {
		t.Fatalf("items = %#v, want at least one item", payload["items"])
	}
	item, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("item = %T, want object", items[0])
	}
	if item["next_retry_at"] != "2026-06-10T01:30:00Z" {
		t.Fatalf("next_retry_at = %v, want 2026-06-10T01:30:00Z", item["next_retry_at"])
	}
	if item["provider"] != "openai" {
		t.Fatalf("provider = %v, want openai", item["provider"])
	}
}

func TestGetUsageCredentialHealthIncludesNextRetryAt(t *testing.T) {
	handler, closeRepo := newUsageObservabilityTestHandler(t)
	defer closeRepo()
	seedUsageObservabilityManagementRecordWithRetry(t, handler)
	seedUsageObservabilityFailedManagementRecord(t, handler)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.GET("/usage/health/credentials", handler.GetUsageCredentialHealth)

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/usage/health/credentials?from=2026-06-10T01:00:00Z&to=2026-06-10T01:10:00Z", nil)
	engine.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want %d", resp.Code, resp.Body.String(), http.StatusOK)
	}
	var payload map[string]any
	if errDecode := json.Unmarshal(resp.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	items, ok := payload["items"].([]any)
	if !ok || len(items) == 0 {
		t.Fatalf("items = %#v, want at least one item", payload["items"])
	}
	item, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("item = %T, want object", items[0])
	}
	if item["next_retry_at"] != "2026-06-10T01:30:00Z" {
		t.Fatalf("next_retry_at = %v, want 2026-06-10T01:30:00Z", item["next_retry_at"])
	}
}

func TestListRequestLogsReturnsUsageBackedIndex(t *testing.T) {
	handler, closeRepo := newUsageObservabilityTestHandler(t)
	defer closeRepo()
	seedUsageObservabilityManagementRecord(t, handler)

	if errMkdir := os.MkdirAll(homeLogDirectory, 0o755); errMkdir != nil {
		t.Fatalf("MkdirAll(logs) error = %v", errMkdir)
	}
	logPath := filepath.Join(homeLogDirectory, "20260610010203-req-obs-1.log")
	if errWrite := os.WriteFile(logPath, []byte("request line\n"), 0o644); errWrite != nil {
		t.Fatalf("WriteFile(log) error = %v", errWrite)
	}
	defer func() {
		if errRemove := os.Remove(logPath); errRemove != nil && !os.IsNotExist(errRemove) {
			t.Errorf("remove log: %v", errRemove)
		}
	}()

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.GET("/request-logs", handler.ListRequestLogs)

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/request-logs?from=2026-06-10T01:00:00Z&to=2026-06-10T01:10:00Z", nil)
	engine.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want %d", resp.Code, resp.Body.String(), http.StatusOK)
	}
	var payload map[string]any
	if errDecode := json.Unmarshal(resp.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	items, ok := payload["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("items = %#v, want one item", payload["items"])
	}
	item, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("item = %T, want object", items[0])
	}
	if item["request_id"] != "req-obs-1" {
		t.Fatalf("request_id = %v, want req-obs-1", item["request_id"])
	}
	if item["available"] != true {
		t.Fatalf("available = %v, want true", item["available"])
	}
	if item["home_port"] != float64(8327) {
		t.Fatalf("home_port = %v, want 8327", item["home_port"])
	}
	wantDownloadURL := "/request-log-by-id/req-obs-1?home_ip=192.0.2.10&home_port=8327"
	if item["download_url"] != wantDownloadURL {
		t.Fatalf("download_url = %v, want %s", item["download_url"], wantDownloadURL)
	}
}

func TestListRequestLogsMarksUnavailableWhenFileMissing(t *testing.T) {
	handler, closeRepo := newUsageObservabilityTestHandler(t)
	defer closeRepo()
	seedUsageObservabilityManagementRecord(t, handler)

	_ = os.Remove(filepath.Join(homeLogDirectory, "20260610010203-req-obs-1.log"))

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.GET("/request-logs", handler.ListRequestLogs)

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/request-logs?from=2026-06-10T01:00:00Z&to=2026-06-10T01:10:00Z", nil)
	engine.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want %d", resp.Code, resp.Body.String(), http.StatusOK)
	}
	var payload map[string]any
	if errDecode := json.Unmarshal(resp.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	items, ok := payload["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("items = %#v, want one item", payload["items"])
	}
	item, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("item = %T, want object", items[0])
	}
	if item["available"] != false {
		t.Fatalf("available = %v, want false", item["available"])
	}
	if item["download_url"] != nil {
		t.Fatalf("download_url = %v, want nil", item["download_url"])
	}
}

func TestListRequestLogsMarksRemoteLogRoutable(t *testing.T) {
	handler, closeRepo := newUsageObservabilityTestHandler(t)
	defer closeRepo()
	handler.nodePort = 8327
	handler.forwardTLSConfig = &tls.Config{}

	payload := `{"timestamp":"2026-06-10T01:02:05Z","event_type":"completion","provider":"openai","model":"gpt-4.1-mini","request_id":"req-remote-index","endpoint":"/v1/chat/completions","latency_ms":100,"tokens":{"total_tokens":1}}`
	if _, errUsage := handler.repo.AppendUsageWithRuntime(context.Background(), payload, cluster.UsageRuntimeMetadata{HomeIP: "192.0.2.20", HomePort: 8328}); errUsage != nil {
		t.Fatalf("AppendUsage(remote) error = %v", errUsage)
	}

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.GET("/request-logs", handler.ListRequestLogs)

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/request-logs?request_id=req-remote-index", nil)
	engine.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want %d", resp.Code, resp.Body.String(), http.StatusOK)
	}
	var response map[string]any
	if errDecode := json.Unmarshal(resp.Body.Bytes(), &response); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	items, ok := response["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("items = %#v, want one item", response["items"])
	}
	item, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("item = %T, want object", items[0])
	}
	if item["available"] != true || item["file_name"] != nil || item["size_bytes"] != nil {
		t.Fatalf("item = %#v, want routable remote log without local metadata", item)
	}
	wantDownloadURL := "/request-log-by-id/req-remote-index?home_ip=192.0.2.20&home_port=8328"
	if item["download_url"] != wantDownloadURL {
		t.Fatalf("download_url = %v, want %s", item["download_url"], wantDownloadURL)
	}
}

func TestListRequestLogsSearchesFileName(t *testing.T) {
	handler, closeRepo := newUsageObservabilityTestHandler(t)
	defer closeRepo()
	seedUsageObservabilityManagementRecord(t, handler)

	if errMkdir := os.MkdirAll(homeLogDirectory, 0o755); errMkdir != nil {
		t.Fatalf("MkdirAll(logs) error = %v", errMkdir)
	}
	logPath := filepath.Join(homeLogDirectory, "20260610010203-req-obs-1.log")
	if errWrite := os.WriteFile(logPath, []byte("request line\n"), 0o644); errWrite != nil {
		t.Fatalf("WriteFile(log) error = %v", errWrite)
	}
	defer func() {
		if errRemove := os.Remove(logPath); errRemove != nil && !os.IsNotExist(errRemove) {
			t.Errorf("remove log: %v", errRemove)
		}
	}()

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.GET("/request-logs", handler.ListRequestLogs)

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/request-logs?from=2026-06-10T01:00:00Z&to=2026-06-10T01:10:00Z&search=20260610010203", nil)
	engine.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want %d", resp.Code, resp.Body.String(), http.StatusOK)
	}
	var payload map[string]any
	if errDecode := json.Unmarshal(resp.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if payload["total"] != float64(1) {
		t.Fatalf("total = %v, want 1", payload["total"])
	}
	items, ok := payload["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("items = %#v, want one item", payload["items"])
	}
	item, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("item = %T, want object", items[0])
	}
	if item["file_name"] != "20260610010203-req-obs-1.log" {
		t.Fatalf("file_name = %v, want 20260610010203-req-obs-1.log", item["file_name"])
	}
}

func TestListRequestLogsSearchesStatus(t *testing.T) {
	handler, closeRepo := newUsageObservabilityTestHandler(t)
	defer closeRepo()
	seedUsageObservabilityManagementRecord(t, handler)
	seedUsageObservabilityFailedManagementRecord(t, handler)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.GET("/request-logs", handler.ListRequestLogs)

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/request-logs?from=2026-06-10T01:00:00Z&to=2026-06-10T01:10:00Z&search=failed&limit=10", nil)
	engine.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want %d", resp.Code, resp.Body.String(), http.StatusOK)
	}
	var payload map[string]any
	if errDecode := json.Unmarshal(resp.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if payload["total"] != float64(1) {
		t.Fatalf("total = %v, want 1", payload["total"])
	}
	items, ok := payload["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("items = %#v, want one item", payload["items"])
	}
	item, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("item = %T, want object", items[0])
	}
	if item["request_id"] != "req-obs-429" {
		t.Fatalf("request_id = %v, want req-obs-429", item["request_id"])
	}
	if item["status"] != "failed" {
		t.Fatalf("status = %v, want failed", item["status"])
	}
}

func newUsageObservabilityTestHandler(t *testing.T) (*Handler, func()) {
	t.Helper()

	db, errOpen := cluster.OpenSQLite(t.Context(), filepath.Join(t.TempDir(), "home.db"))
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

func seedUsageObservabilityManagementRecord(t *testing.T, handler *Handler) {
	t.Helper()

	ctx := context.Background()
	username := "usage-user"
	credits := 100.0
	user, errCreateUser := handler.repo.CreateUser(ctx, cluster.UserUpdate{Username: &username, Credits: &credits})
	if errCreateUser != nil {
		t.Fatalf("CreateUser() error = %v", errCreateUser)
	}
	clientKey := "client-key-secret-1234"
	if _, errCreateKey := handler.repo.CreateAPIKeyForUser(ctx, user.ID, cluster.APIKeyUserUpdate{APIKey: &clientKey}); errCreateKey != nil {
		t.Fatalf("CreateAPIKeyForUser() error = %v", errCreateKey)
	}
	auth := &coreauth.Auth{
		ID:        "auth-observability",
		Index:     "auth-observability",
		Provider:  "codex",
		Label:     "Primary OAuth",
		Status:    coreauth.StatusActive,
		CreatedAt: time.Date(2026, time.June, 10, 1, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, time.June, 10, 1, 0, 0, 0, time.UTC),
	}
	if _, errAuth := handler.repo.UpsertAuth(ctx, auth, "test"); errAuth != nil {
		t.Fatalf("UpsertAuth() error = %v", errAuth)
	}
	if _, errCreatePrice := handler.repo.CreateBillingModelPrice(ctx, cluster.BillingModelPriceUpdate{Provider: "openai", Model: "gpt-4.1-mini", RequestPrice: 2, Enabled: true}); errCreatePrice != nil {
		t.Fatalf("CreateBillingModelPrice() error = %v", errCreatePrice)
	}
	payload := `{"timestamp":"2026-06-10T01:02:03Z","event_type":"completion","provider":"openai","model":"gpt-4.1-mini","api_key":"client-key-secret-1234","request_id":"req-obs-1","upstream_status_code":201,"client_ip":"203.0.113.8","cpa_node_id":"cpa-a","cpa_ip":"10.0.0.5","cpa_port":8317,"cpa_label":"cpa-a:8317","method":"POST","stream":true,"messages":[{"role":"user","content":"hello"}],"tools":[{"type":"function"}],"endpoint":"/v1/chat/completions","executor_type":"CodexWebsocketsExecutor","auth_index":"auth-observability","auth_type":"oauth","latency_ms":1460,"ttft_ms":333,"tokens":{"input_tokens":100,"output_tokens":50,"total_tokens":150}}`
	if _, errUsage := handler.repo.AppendUsageWithRuntime(ctx, payload, cluster.UsageRuntimeMetadata{HomeIP: "192.0.2.10", HomePort: 8327}); errUsage != nil {
		t.Fatalf("AppendUsage() error = %v", errUsage)
	}
}

func seedUsageObservabilityManagementRecordWithRetry(t *testing.T, handler *Handler) {
	t.Helper()

	seedUsageObservabilityManagementRecord(t, handler)
	nextRetry := time.Date(2026, time.June, 10, 1, 30, 0, 0, time.UTC)
	auth := &coreauth.Auth{
		ID:             "auth-observability",
		Index:          "auth-observability",
		Provider:       "codex",
		Label:          "Primary OAuth",
		Status:         coreauth.StatusActive,
		Unavailable:    true,
		NextRetryAfter: nextRetry,
		CreatedAt:      time.Date(2026, time.June, 10, 1, 0, 0, 0, time.UTC),
		UpdatedAt:      time.Date(2026, time.June, 10, 1, 0, 0, 0, time.UTC),
	}
	if _, errAuth := handler.repo.UpsertAuth(context.Background(), auth, "test"); errAuth != nil {
		t.Fatalf("UpsertAuth(retry) error = %v", errAuth)
	}
}

func seedUsageObservabilityTimezoneBoundaryRecord(t *testing.T, handler *Handler) {
	t.Helper()

	payload := `{"timestamp":"2026-06-09T17:02:03Z","provider":"openai","model":"gpt-4.1-mini","api_key":"client-key-secret-1234","request_id":"req-obs-shanghai-day","endpoint":"/v1/chat/completions","executor_type":"CodexWebsocketsExecutor","auth_index":"auth-observability","auth_type":"oauth","latency_ms":1000,"tokens":{"input_tokens":100,"output_tokens":50,"total_tokens":150}}`
	if _, errUsage := handler.repo.AppendUsage(context.Background(), payload, "192.0.2.10"); errUsage != nil {
		t.Fatalf("AppendUsage(timezone boundary) error = %v", errUsage)
	}
}

func seedUsageObservabilityAdditionalRecords(t *testing.T, handler *Handler, count int) {
	t.Helper()

	base := time.Date(2026, time.June, 10, 2, 0, 0, 0, time.UTC)
	for index := 0; index < count; index++ {
		timestamp := base.Add(time.Duration(index) * time.Second).Format(time.RFC3339)
		payload := fmt.Sprintf(`{"timestamp":%q,"provider":"openai","model":"gpt-4.1-mini","api_key":"client-key-secret-1234","request_id":"req-export-%03d","endpoint":"/v1/chat/completions","executor_type":"CodexWebsocketsExecutor","auth_index":"auth-observability","auth_type":"oauth","latency_ms":1000,"tokens":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`, timestamp, index)
		if _, errUsage := handler.repo.AppendUsage(context.Background(), payload, "192.0.2.10"); errUsage != nil {
			t.Fatalf("AppendUsage(additional %d) error = %v", index, errUsage)
		}
	}
}

func seedUsageObservabilityFailedManagementRecord(t *testing.T, handler *Handler) {
	t.Helper()

	payload := `{"timestamp":"2026-06-10T01:03:03Z","source":"responses","provider":"openai","model":"gpt-4.1-mini","api_key":"client-key-secret-1234","request_id":"req-obs-429","upstream_request_id":"upstream-429","client_ip":"203.0.113.9","endpoint":"/v1/responses","executor_type":"CodexWebsocketsExecutor","auth_index":"auth-observability","auth_type":"oauth","failed":true,"fail":{"status_code":429,"body":"rate limit exceeded: access_token secret"},"latency_ms":2460,"ttft_ms":444,"reasoning_effort":"high","service_tier":"priority","tokens":{"input_tokens":120,"output_tokens":0,"total_tokens":120}}`
	if _, errUsage := handler.repo.AppendUsage(context.Background(), payload, "192.0.2.10"); errUsage != nil {
		t.Fatalf("AppendUsage(failed) error = %v", errUsage)
	}
}

func seedUsageObservabilityProviderAPIKeyManagementRecord(t *testing.T, handler *Handler) {
	t.Helper()

	payload := `{"timestamp":"2026-06-10T01:04:03Z","provider":"openai","model":"gpt-4.1-mini","api_key":"client-key-secret-1234","request_id":"req-obs-provider-key","endpoint":"/v1/chat/completions","executor_type":"OpenAICompatibleExecutor","auth_index":"provider-key-1","auth_type":"provider_api_key","latency_ms":1600,"tokens":{"input_tokens":80,"output_tokens":40,"total_tokens":120}}`
	if _, errUsage := handler.repo.AppendUsage(context.Background(), payload, "192.0.2.10"); errUsage != nil {
		t.Fatalf("AppendUsage(provider api key) error = %v", errUsage)
	}
}
