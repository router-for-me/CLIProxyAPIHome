package cluster

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestUsageRecordFromPayloadStoresRequestIDAndHomeIP(t *testing.T) {
	payload := `{"timestamp":"2026-05-29T01:02:03Z","request_id":"req-usage-1","executor_type":"CodexWebsocketsExecutor","tokens":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}`

	record, errRecord := UsageRecordFromPayload(payload, "192.0.2.10")
	if errRecord != nil {
		t.Fatalf("UsageRecordFromPayload: %v", errRecord)
	}

	if record.RequestID != "req-usage-1" {
		t.Fatalf("request id = %q, want req-usage-1", record.RequestID)
	}
	if record.HomeIP != "192.0.2.10" {
		t.Fatalf("home ip = %q, want 192.0.2.10", record.HomeIP)
	}
	if record.ExecutorType != "CodexWebsocketsExecutor" {
		t.Fatalf("executor type = %q, want CodexWebsocketsExecutor", record.ExecutorType)
	}
	if record.TotalTokens != 15 {
		t.Fatalf("total tokens = %d, want 15", record.TotalTokens)
	}
	if record.TokenAccountingVersion != UsageTokenAccountingSchemaVersion || record.AccountingTotalTokens != 15 {
		t.Fatalf("token accounting = version:%d total:%d", record.TokenAccountingVersion, record.AccountingTotalTokens)
	}
}

func TestUsageRecordFromPayloadStoresCanonicalTokenBreakdownV2(t *testing.T) {
	payload := `{"timestamp":"2026-07-23T01:02:03Z","provider":"openai","accounting_version":2,"tokens":{"input_tokens":100,"output_tokens":30,"reasoning_tokens":12,"cached_tokens":40,"cache_read_tokens":40,"cache_read_tokens_present":true,"total_tokens":130},"token_breakdown":{"schema_version":2,"quality":"complete","total_tokens":130,"input":{"total_tokens":100,"uncached_tokens":60,"cache_read_tokens":40,"cache_write_tokens":0},"output":{"total_tokens":30,"non_reasoning_tokens":18,"reasoning_tokens":12},"unclassified_tokens":0}}`

	record, errRecord := UsageRecordFromPayload(payload, "192.0.2.10")
	if errRecord != nil {
		t.Fatalf("UsageRecordFromPayload() error = %v", errRecord)
	}
	breakdown := usageTokenBreakdownFromRecord(record)
	if !breakdown.Valid() || breakdown.Quality != UsageTokenAccountingQualityComplete {
		t.Fatalf("token breakdown = %+v", breakdown)
	}
	if breakdown.Input.UncachedTokens != 60 || breakdown.Output.NonReasoningTokens != 18 {
		t.Fatalf("token breakdown = %+v", breakdown)
	}
}

func TestUsageRecordFromPayloadRejectsInvalidTokenBreakdownV2(t *testing.T) {
	payload := `{"timestamp":"2026-07-23T01:02:03Z","accounting_version":2,"tokens":{"total_tokens":130},"token_breakdown":{"schema_version":2,"quality":"complete","total_tokens":130,"input":{"total_tokens":100,"uncached_tokens":100,"cache_read_tokens":40,"cache_write_tokens":0},"output":{"total_tokens":30,"non_reasoning_tokens":30,"reasoning_tokens":0},"unclassified_tokens":0}}`

	if _, errRecord := UsageRecordFromPayload(payload, "192.0.2.10"); errRecord == nil || !strings.Contains(errRecord.Error(), "schema v2 invariants") {
		t.Fatalf("UsageRecordFromPayload() error = %v, want invariant failure", errRecord)
	}
}

func TestUsageRecordFromPayloadNormalizesLegacyProviderSemantics(t *testing.T) {
	tests := []struct {
		name          string
		payload       string
		wantInput     int64
		wantUncached  int64
		wantOutput    int64
		wantReasoning int64
	}{
		{
			name:          "openai subsets",
			payload:       `{"timestamp":"2026-07-23T01:02:03Z","provider":"openai","executor_type":"OpenAICompatExecutor","tokens":{"input_tokens":100,"output_tokens":30,"reasoning_tokens":12,"cached_tokens":40,"cache_read_tokens":40,"cache_read_tokens_present":true,"total_tokens":130}}`,
			wantInput:     100,
			wantUncached:  60,
			wantOutput:    30,
			wantReasoning: 12,
		},
		{
			name:          "claude independent cache",
			payload:       `{"timestamp":"2026-07-23T01:02:03Z","provider":"claude","executor_type":"ClaudeExecutor","tokens":{"input_tokens":30,"output_tokens":5,"cache_read_tokens":7,"cache_read_tokens_present":true,"cache_creation_tokens":13,"total_tokens":55}}`,
			wantInput:     50,
			wantUncached:  30,
			wantOutput:    5,
			wantReasoning: 0,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			record, errRecord := UsageRecordFromPayload(test.payload, "192.0.2.10")
			if errRecord != nil {
				t.Fatalf("UsageRecordFromPayload() error = %v", errRecord)
			}
			breakdown := usageTokenBreakdownFromRecord(record)
			if !breakdown.Valid() || breakdown.Quality != UsageTokenAccountingQualityComplete {
				t.Fatalf("token breakdown = %+v", breakdown)
			}
			if breakdown.Input.TotalTokens != test.wantInput || breakdown.Input.UncachedTokens != test.wantUncached || breakdown.Output.TotalTokens != test.wantOutput || breakdown.Output.ReasoningTokens != test.wantReasoning {
				t.Fatalf("token breakdown = %+v", breakdown)
			}
		})
	}
}

func TestUsageRecordFromPayloadStoresXAIAPIKeyStatistics(t *testing.T) {
	payload := `{"timestamp":"2026-07-14T01:02:03Z","source":"xai-upstream-secret","provider":"xai","executor_type":"XAIExecutor","model":"grok-4.5","alias":"grok-latest","auth_type":"api_key","auth_index":"xai-auth","tokens":{"input_tokens":12,"output_tokens":8,"reasoning_tokens":3,"cached_tokens":2,"total_tokens":23}}`

	record, errRecord := UsageRecordFromPayload(payload, "192.0.2.10")
	if errRecord != nil {
		t.Fatalf("UsageRecordFromPayload: %v", errRecord)
	}
	if record.Provider != "xai" || record.ExecutorType != "XAIExecutor" || record.AuthType != "api_key" || record.AuthIndex != "xai-auth" {
		t.Fatalf("xAI usage identity = %+v", record)
	}
	if record.Model != "grok-4.5" || record.Alias != "grok-latest" {
		t.Fatalf("xAI usage model/alias = %q/%q", record.Model, record.Alias)
	}
	if record.Source != "xai-auth" || strings.Contains(string(record.PayloadJSON), "xai-upstream-secret") {
		t.Fatalf("xAI usage source was not sanitized: source=%q payload=%s", record.Source, string(record.PayloadJSON))
	}
	if record.InputTokens != 12 || record.OutputTokens != 8 || record.ReasoningTokens != 3 || record.CachedTokens != 2 || record.TotalTokens != 23 {
		t.Fatalf("xAI usage tokens = %+v", record)
	}
}

func TestMigrateUsageProviderAPIKeySourcesSanitizesHistoricalRows(t *testing.T) {
	db, errOpen := OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "home.db"))
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
	if errMigrate := db.AutoMigrate(&UsageRecord{}); errMigrate != nil {
		t.Fatalf("AutoMigrate() error = %v", errMigrate)
	}

	payload := `{"timestamp":"2026-07-14T01:02:03Z","source":"historical-upstream-secret","provider":"xai","auth_type":"apikey","auth_index":"xai-auth"}`
	record := &UsageRecord{
		Timestamp:   time.Date(2026, time.July, 14, 1, 2, 3, 0, time.UTC),
		Source:      "historical-upstream-secret",
		AuthIndex:   "xai-auth",
		AuthType:    "apikey",
		PayloadJSON: JSONB(payload),
		CreatedAt:   time.Now().UTC(),
	}
	if errCreate := db.Create(record).Error; errCreate != nil {
		t.Fatalf("create usage record: %v", errCreate)
	}

	if errMigrate := migrateUsageProviderAPIKeySources(db); errMigrate != nil {
		t.Fatalf("migrateUsageProviderAPIKeySources() error = %v", errMigrate)
	}
	var stored UsageRecord
	if errFirst := db.First(&stored, record.ID).Error; errFirst != nil {
		t.Fatalf("load migrated usage: %v", errFirst)
	}
	if stored.Source != "xai-auth" || strings.Contains(string(stored.PayloadJSON), "historical-upstream-secret") {
		t.Fatalf("historical usage source was not sanitized: source=%q payload=%s", stored.Source, string(stored.PayloadJSON))
	}
	latePayload := `{"timestamp":"2026-07-14T01:02:04Z","source":"late-upstream-secret","provider":"xai","auth_type":"provider_api_key","auth_index":"late-xai-auth"}`
	lateRecord := &UsageRecord{
		Timestamp:   time.Date(2026, time.July, 14, 1, 2, 4, 0, time.UTC),
		Source:      "late-upstream-secret",
		AuthIndex:   "late-xai-auth",
		AuthType:    "provider_api_key",
		PayloadJSON: JSONB(latePayload),
		CreatedAt:   time.Now().UTC(),
	}
	if errCreate := db.Create(lateRecord).Error; errCreate != nil {
		t.Fatalf("create late usage record: %v", errCreate)
	}
	if errMigrate := migrateUsageProviderAPIKeySources(db); errMigrate != nil {
		t.Fatalf("repeat migrateUsageProviderAPIKeySources() error = %v", errMigrate)
	}
	stored = UsageRecord{}
	if errFirst := db.First(&stored, lateRecord.ID).Error; errFirst != nil {
		t.Fatalf("load late migrated usage: %v", errFirst)
	}
	if stored.Source != "late-xai-auth" || strings.Contains(string(stored.PayloadJSON), "late-upstream-secret") {
		t.Fatalf("late usage source was not sanitized: source=%q payload=%s", stored.Source, string(stored.PayloadJSON))
	}
}

func TestUsageRecordFromPayloadUsesCanonicalCacheCreationField(t *testing.T) {
	payload := `{"timestamp":"2026-07-12T01:02:03Z","tokens":{"cache_creation_tokens":11,"cache_write_tokens":22}}`

	record, errRecord := UsageRecordFromPayload(payload, "192.0.2.10")
	if errRecord != nil {
		t.Fatalf("UsageRecordFromPayload: %v", errRecord)
	}
	if record.CacheCreationTokens != 11 {
		t.Fatalf("cache creation tokens = %d, want 11", record.CacheCreationTokens)
	}
}

func TestUsageRecordFromPayloadNormalizesLegacyCacheFields(t *testing.T) {
	tests := []struct {
		name                    string
		payload                 string
		wantCachedTokens        int64
		wantCacheReadTokens     int64
		wantCacheReadPresent    bool
		wantCacheCreationTokens int64
	}{
		{
			name:                    "legacy openai cache read",
			payload:                 `{"timestamp":"2026-07-12T01:02:03Z","provider":"openai","executor_type":"OpenAICompatExecutor","tokens":{"cached_tokens":13,"cache_read_tokens":0,"cache_creation_tokens":7}}`,
			wantCachedTokens:        13,
			wantCacheReadTokens:     13,
			wantCacheReadPresent:    false,
			wantCacheCreationTokens: 7,
		},
		{
			name:                    "current CPA preserves explicit zero read bucket",
			payload:                 `{"timestamp":"2026-07-12T01:02:03Z","provider":"openai","executor_type":"OpenAICompatExecutor","tokens":{"cached_tokens":13,"cache_read_tokens":0,"cache_read_tokens_present":true,"cache_creation_tokens":7}}`,
			wantCachedTokens:        13,
			wantCacheReadTokens:     0,
			wantCacheReadPresent:    true,
			wantCacheCreationTokens: 7,
		},
		{
			name:                    "claude keeps separate zero read bucket",
			payload:                 `{"timestamp":"2026-07-12T01:02:03Z","provider":"claude","executor_type":"ClaudeExecutor","tokens":{"cached_tokens":13,"cache_read_tokens":0,"cache_creation_tokens":13}}`,
			wantCachedTokens:        13,
			wantCacheReadTokens:     0,
			wantCacheReadPresent:    false,
			wantCacheCreationTokens: 13,
		},
		{
			name:                    "cache write fallback",
			payload:                 `{"timestamp":"2026-07-12T01:02:03Z","provider":"openai","tokens":{"cache_write_tokens":22}}`,
			wantCachedTokens:        0,
			wantCacheReadTokens:     0,
			wantCacheReadPresent:    false,
			wantCacheCreationTokens: 22,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			record, errRecord := UsageRecordFromPayload(test.payload, "192.0.2.10")
			if errRecord != nil {
				t.Fatalf("UsageRecordFromPayload() error = %v", errRecord)
			}
			if record.CachedTokens != test.wantCachedTokens ||
				record.CacheReadTokens != test.wantCacheReadTokens ||
				record.CacheReadTokensPresent != test.wantCacheReadPresent ||
				record.CacheCreationTokens != test.wantCacheCreationTokens {
				t.Fatalf("cache tokens = cached:%d read:%d present:%t creation:%d, want cached:%d read:%d present:%t creation:%d",
					record.CachedTokens,
					record.CacheReadTokens,
					record.CacheReadTokensPresent,
					record.CacheCreationTokens,
					test.wantCachedTokens,
					test.wantCacheReadTokens,
					test.wantCacheReadPresent,
					test.wantCacheCreationTokens)
			}
		})
	}
}

func TestUsageRecordFromPayloadWithRuntimeStoresOwnershipColumns(t *testing.T) {
	payload := `{"timestamp":"2026-07-09T01:02:03Z","request_id":"req-runtime-1","endpoint":"/v1/responses","upstream_status_code":"202","tokens":{"total_tokens":3}}`

	record, errRecord := UsageRecordFromPayloadWithRuntime(payload, UsageRuntimeMetadata{
		HomeIP:    "192.0.2.10",
		HomePort:  8327,
		CPANodeID: "node-1",
		CPAIP:     "10.0.0.5",
		CPAPort:   8317,
	})
	if errRecord != nil {
		t.Fatalf("UsageRecordFromPayloadWithRuntime: %v", errRecord)
	}

	if record.HomeIP != "192.0.2.10" || record.HomePort != 8327 {
		t.Fatalf("home ownership = %s:%d, want 192.0.2.10:8327", record.HomeIP, record.HomePort)
	}
	if record.CPANodeID != "node-1" || record.CPAIP != "10.0.0.5" || record.CPAPort != 8317 || record.CPALabel != "node-1" {
		t.Fatalf("CPA ownership = node=%q ip=%q port=%d label=%q, want node-1 10.0.0.5 8317 node-1", record.CPANodeID, record.CPAIP, record.CPAPort, record.CPALabel)
	}
	if record.EventType != "response" {
		t.Fatalf("event type = %q, want response", record.EventType)
	}
	if record.UpstreamStatusCode != 202 {
		t.Fatalf("upstream status code = %d, want 202", record.UpstreamStatusCode)
	}
}

func TestUsageRecordFromPayloadDoesNotTreatClientIPAsCPAIP(t *testing.T) {
	payload := `{"timestamp":"2026-07-09T01:02:03Z","request_id":"req-client-ip","client_ip":"203.0.113.8","endpoint":"/v1/chat/completions"}`

	record, errRecord := UsageRecordFromPayloadWithRuntime(payload, UsageRuntimeMetadata{HomeIP: "192.0.2.10"})
	if errRecord != nil {
		t.Fatalf("UsageRecordFromPayloadWithRuntime: %v", errRecord)
	}

	if record.CPAIP != "" {
		t.Fatalf("CPA IP = %q, want empty when only client_ip exists", record.CPAIP)
	}
}

func TestUsageRecordFromPayloadDerivesCPALabelFromPayloadOwnership(t *testing.T) {
	payload := `{"timestamp":"2026-07-09T01:02:03Z","request_id":"req-cpa-label","cpa_node_id":"node-from-payload","cpa_ip":"10.0.0.5","cpa_port":8317}`

	record, errRecord := UsageRecordFromPayloadWithRuntime(payload, UsageRuntimeMetadata{HomeIP: "192.0.2.10"})
	if errRecord != nil {
		t.Fatalf("UsageRecordFromPayloadWithRuntime: %v", errRecord)
	}

	if record.CPALabel != "node-from-payload" {
		t.Fatalf("CPA label = %q, want node-from-payload", record.CPALabel)
	}
}
