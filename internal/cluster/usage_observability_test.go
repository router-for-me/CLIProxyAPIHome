package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"gorm.io/gorm"
)

func TestUsageObservabilityAutoMigrateCreatesDashboardIndexes(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()

	db, errDB := repo.database()
	if errDB != nil {
		t.Fatalf("database() error = %v", errDB)
	}
	for _, indexName := range []string{
		"idx_usage_provider_time",
		"idx_usage_provider_lower_time",
		"idx_usage_home_time",
		"idx_usage_auth_type_time",
		"idx_usage_auth_type_normalized_time",
		"idx_usage_failed_status_time",
	} {
		if !db.Migrator().HasIndex(&UsageRecord{}, indexName) {
			t.Fatalf("usage index %s was not created", indexName)
		}
	}
}

func TestUsageObservabilityRecordQueryDependencies(t *testing.T) {
	t.Parallel()

	userID := uint(7)
	clientKeyID := uint(8)
	zeroID := uint(0)
	amount := 1.5
	tests := []struct {
		name  string
		query UsageObservabilityRecordQuery
		want  usageObservabilityRecordQueryDependencies
	}{
		{
			name: "usage fields only",
			query: UsageObservabilityRecordQuery{
				Provider:         "openai",
				Model:            "gpt",
				Status:           "failed",
				RequestID:        "req-1",
				CredentialType:   "oauth",
				EventType:        "completion",
				CPANode:          "node-a",
				RequestLogSearch: "failed",
			},
		},
		{name: "inactive related fields", query: UsageObservabilityRecordQuery{User: "  ", UserID: &zeroID, ClientKeyID: &zeroID}},
		{
			name:  "user text",
			query: UsageObservabilityRecordQuery{User: "alice"},
			want:  usageObservabilityRecordQueryDependencies{Billing: true, APIKey: true, User: true},
		},
		{
			name:  "user id",
			query: UsageObservabilityRecordQuery{UserID: &userID},
			want:  usageObservabilityRecordQueryDependencies{Billing: true, APIKey: true},
		},
		{
			name:  "client key",
			query: UsageObservabilityRecordQuery{ClientKey: "client-key"},
			want:  usageObservabilityRecordQueryDependencies{Billing: true, APIKey: true},
		},
		{
			name:  "client key id",
			query: UsageObservabilityRecordQuery{ClientKeyID: &clientKeyID},
			want:  usageObservabilityRecordQueryDependencies{Billing: true, APIKey: true},
		},
		{
			name:  "credential id",
			query: UsageObservabilityRecordQuery{CredentialID: "credential-1"},
			want:  usageObservabilityRecordQueryDependencies{Auth: true},
		},
		{
			name:  "auth index",
			query: UsageObservabilityRecordQuery{AuthIndex: "auth-1"},
			want:  usageObservabilityRecordQueryDependencies{Auth: true},
		},
		{
			name:  "minimum amount",
			query: UsageObservabilityRecordQuery{MinAmount: &amount},
			want:  usageObservabilityRecordQueryDependencies{Billing: true},
		},
		{
			name:  "maximum amount",
			query: UsageObservabilityRecordQuery{MaxAmount: &amount},
			want:  usageObservabilityRecordQueryDependencies{Billing: true},
		},
		{
			name:  "global search",
			query: UsageObservabilityRecordQuery{Search: "needle"},
			want:  usageObservabilityRecordQueryDependencies{Billing: true, APIKey: true, User: true, Auth: true},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := usageObservabilityRecordQueryDependenciesFor(test.query); got != test.want {
				t.Fatalf("dependencies = %+v, want %+v", got, test.want)
			}
		})
	}
}

func TestUsageObservabilityExactScopesPruneOnlyUnneededJoins(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	db, errDB := repo.database()
	if errDB != nil {
		t.Fatalf("database() error = %v", errDB)
	}

	from := time.Date(2026, time.July, 25, 0, 0, 0, 0, time.UTC)
	to := from.Add(24 * time.Hour)
	statusCode := 500
	minLatency := int64(100)
	maxLatency := int64(5000)
	usageOnly := UsageObservabilityRecordQuery{
		From:             &from,
		To:               &to,
		Provider:         "openai",
		Model:            "gpt",
		HomeIP:           "192.0.2.1",
		Endpoint:         "/v1/responses",
		CredentialType:   "oauth",
		Status:           "failed",
		StatusCode:       &statusCode,
		RequestID:        "req-1",
		ExecutorType:     "CodexExecutor",
		EventType:        "completion",
		CPANode:          "node-a",
		MinLatencyMS:     &minLatency,
		MaxLatencyMS:     &maxLatency,
		RequestLogSearch: "failed",
	}
	countSQL := usageObservabilityDryRunCountSQL(t, db, usageOnly)
	assertUsageObservabilitySQLHasNoJoins(t, countSQL)
	if !strings.Contains(countSQL, "COUNT(*)") {
		t.Fatalf("usage-only count SQL = %q, want COUNT(*)", countSQL)
	}
	optionSQL := usageObservabilityDryRunOptionSQL(t, db, usageOnly)
	assertUsageObservabilitySQLHasNoJoins(t, optionSQL)

	userID := uint(7)
	clientKeyID := uint(8)
	amount := 1.5
	relatedQueries := []struct {
		name  string
		query UsageObservabilityRecordQuery
	}{
		{name: "user", query: UsageObservabilityRecordQuery{User: "alice"}},
		{name: "user id", query: UsageObservabilityRecordQuery{UserID: &userID}},
		{name: "client key", query: UsageObservabilityRecordQuery{ClientKey: "client-key"}},
		{name: "client key id", query: UsageObservabilityRecordQuery{ClientKeyID: &clientKeyID}},
		{name: "credential id", query: UsageObservabilityRecordQuery{CredentialID: "credential-1"}},
		{name: "auth index", query: UsageObservabilityRecordQuery{AuthIndex: "auth-1"}},
		{name: "minimum amount", query: UsageObservabilityRecordQuery{MinAmount: &amount}},
		{name: "maximum amount", query: UsageObservabilityRecordQuery{MaxAmount: &amount}},
		{name: "global search", query: UsageObservabilityRecordQuery{Search: "needle"}},
	}
	for _, test := range relatedQueries {
		t.Run(test.name, func(t *testing.T) {
			countSQL := usageObservabilityDryRunCountSQL(t, db, test.query)
			if !strings.Contains(countSQL, " JOIN ") || !strings.Contains(countSQL, `COUNT(DISTINCT "usage"."id")`) {
				t.Fatalf("related count SQL = %q, want full joins and distinct usage count", countSQL)
			}
			optionSQL := usageObservabilityDryRunOptionSQL(t, db, test.query)
			if !strings.Contains(optionSQL, " JOIN ") {
				t.Fatalf("related option SQL = %q, want full joins", optionSQL)
			}
		})
	}
}

func TestUsageObservabilityExactScopesPreserveResultsWithDuplicatingJoins(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	db, errDB := repo.database()
	if errDB != nil {
		t.Fatalf("database() error = %v", errDB)
	}
	now := time.Date(2026, time.July, 25, 1, 2, 3, 0, time.UTC)
	auths := []AuthRecord{
		{UUID: "uuid-one", AuthJSON: JSONB(`{}`), Version: 1, ID: "id-one", Index: "shared-index", Provider: "openai", CreatedAt: now, UpdatedAt: now},
		{UUID: "uuid-two", AuthJSON: JSONB(`{}`), Version: 1, ID: "id-two", Index: "shared-index", Provider: "openai", CreatedAt: now, UpdatedAt: now},
	}
	if errCreate := db.Create(&auths).Error; errCreate != nil {
		t.Fatalf("Create(auths) error = %v", errCreate)
	}
	usageRecords := []UsageRecord{
		{Timestamp: now, Provider: "openai", Model: "model-a", AuthIndex: "shared-index", EventType: "completion", UpstreamStatusCode: 200, PayloadJSON: JSONB(`{}`), CreatedAt: now},
		{Timestamp: now.Add(time.Second), Provider: "gemini", Model: "model-b", AuthIndex: "shared-index", EventType: "completion", UpstreamStatusCode: 200, PayloadJSON: JSONB(`{}`), CreatedAt: now},
	}
	if errCreate := db.Create(&usageRecords).Error; errCreate != nil {
		t.Fatalf("Create(usage) error = %v", errCreate)
	}

	query := UsageObservabilityRecordQuery{EventType: "completion"}
	var oldCount int64
	if errCount := usageObservabilityRecordScope(db.Table("usage"), query).
		Select(`COUNT(DISTINCT "usage"."id")`).Scan(&oldCount).Error; errCount != nil {
		t.Fatalf("old count error = %v", errCount)
	}
	var newCount int64
	if errCount := usageObservabilityRecordCountQuery(db.Table("usage"), query).Scan(&newCount).Error; errCount != nil {
		t.Fatalf("new count error = %v", errCount)
	}
	if oldCount != 2 || newCount != oldCount {
		t.Fatalf("counts old=%d new=%d, want both 2", oldCount, newCount)
	}

	type valueRow struct {
		Value string `gorm:"column:value"`
	}
	var oldRows []valueRow
	if errFind := usageObservabilityRecordScope(db.Table("usage"), query).
		Where(`"usage"."provider" IS NOT NULL`).
		Where(`TRIM("usage"."provider") <> ''`).
		Select(`DISTINCT "usage"."provider" AS value`).
		Order("value ASC").
		Limit(500).
		Scan(&oldRows).Error; errFind != nil {
		t.Fatalf("old provider options error = %v", errFind)
	}
	oldProviders := make([]string, 0, len(oldRows))
	for _, row := range oldRows {
		oldProviders = append(oldProviders, row.Value)
	}
	newOptions, errOptions := repo.UsageObservabilityFilterOptions(ctx, query)
	if errOptions != nil {
		t.Fatalf("UsageObservabilityFilterOptions() error = %v", errOptions)
	}
	if strings.Join(newOptions.Providers, "\x00") != strings.Join(oldProviders, "\x00") {
		t.Fatalf("provider options old=%v new=%v", oldProviders, newOptions.Providers)
	}

	relatedQuery := UsageObservabilityRecordQuery{CredentialID: "uuid-one"}
	var oldRelatedCount int64
	if errCount := usageObservabilityRecordScope(db.Table("usage"), relatedQuery).
		Select(`COUNT(DISTINCT "usage"."id")`).Scan(&oldRelatedCount).Error; errCount != nil {
		t.Fatalf("old related count error = %v", errCount)
	}
	var newRelatedCount int64
	if errCount := usageObservabilityRecordCountQuery(db.Table("usage"), relatedQuery).Scan(&newRelatedCount).Error; errCount != nil {
		t.Fatalf("new related count error = %v", errCount)
	}
	if newRelatedCount != oldRelatedCount {
		t.Fatalf("related counts old=%d new=%d", oldRelatedCount, newRelatedCount)
	}
}

func usageObservabilityDryRunCountSQL(t *testing.T, db *gorm.DB, query UsageObservabilityRecordQuery) string {
	t.Helper()
	var total int64
	statement := usageObservabilityRecordCountQuery(db.Session(&gorm.Session{DryRun: true}).Table("usage"), query).Scan(&total).Statement
	if statement == nil || statement.SQL.Len() == 0 {
		t.Fatal("count dry-run SQL is empty")
	}
	return statement.SQL.String()
}

func usageObservabilityDryRunOptionSQL(t *testing.T, db *gorm.DB, query UsageObservabilityRecordQuery) string {
	t.Helper()
	type valueRow struct {
		Value string `gorm:"column:value"`
	}
	var rows []valueRow
	scope, _ := usageObservabilityRecordFilterScope(db.Session(&gorm.Session{DryRun: true}).Table("usage"), query)
	statement := scope.
		Where(`"usage"."provider" IS NOT NULL`).
		Where(`TRIM("usage"."provider") <> ''`).
		Select(`DISTINCT "usage"."provider" AS value`).
		Order("value ASC").
		Limit(500).
		Scan(&rows).Statement
	if statement == nil || statement.SQL.Len() == 0 {
		t.Fatal("filter option dry-run SQL is empty")
	}
	return statement.SQL.String()
}

func assertUsageObservabilitySQLHasNoJoins(t *testing.T, sql string) {
	t.Helper()
	if strings.Contains(sql, " JOIN ") {
		t.Fatalf("SQL = %q, want usage-only query without joins", sql)
	}
}

func TestMigrateAuthNextRetryAfterBackfillsJSON(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()

	db, errDB := repo.database()
	if errDB != nil {
		t.Fatalf("database() error = %v", errDB)
	}
	nextRetryAt := time.Date(2026, time.June, 10, 1, 30, 0, 0, time.UTC)
	auth := &coreauth.Auth{
		ID:             "legacy-retry-auth",
		Index:          "legacy-retry-auth",
		Provider:       "codex",
		NextRetryAfter: nextRetryAt,
		CreatedAt:      time.Date(2026, time.June, 10, 1, 0, 0, 0, time.UTC),
		UpdatedAt:      time.Date(2026, time.June, 10, 1, 0, 0, 0, time.UTC),
	}
	authJSON, errMarshal := json.Marshal(auth)
	if errMarshal != nil {
		t.Fatalf("marshal auth: %v", errMarshal)
	}
	if errCreate := db.Create(&AuthRecord{
		UUID:      auth.ID,
		AuthJSON:  JSONB(authJSON),
		Version:   1,
		ID:        auth.ID,
		Index:     auth.Index,
		Provider:  auth.Provider,
		CreatedAt: auth.CreatedAt,
		UpdatedAt: auth.UpdatedAt,
	}).Error; errCreate != nil {
		t.Fatalf("Create(auth) error = %v", errCreate)
	}
	if errMigrate := migrateAuthNextRetryAfter(db); errMigrate != nil {
		t.Fatalf("migrateAuthNextRetryAfter() error = %v", errMigrate)
	}
	var record AuthRecord
	if errFirst := db.First(&record, "uuid = ?", auth.ID).Error; errFirst != nil {
		t.Fatalf("First(auth) error = %v", errFirst)
	}
	if record.NextRetryAfter == nil || !record.NextRetryAfter.UTC().Equal(nextRetryAt) {
		t.Fatalf("next_retry_after = %v, want %v", record.NextRetryAfter, nextRetryAt)
	}
}

func TestUsageDerivedColumnBackfillBatchBackfillsStructuredMetadataIdempotently(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()

	db, errDB := repo.database()
	if errDB != nil {
		t.Fatalf("database() error = %v", errDB)
	}
	payload := `{"timestamp":"2026-07-09T01:02:03Z","event_type":"stream","request_id":"req-derived","upstream_request_id":"upstream-derived","upstream_status_code":202,"home_port":8328,"cpa_node_id":"node-derived","cpa_ip":"10.0.0.5","cpa_port":8317,"provider":"openai","model":"gpt-4.1-mini","endpoint":"/v1/chat/completions"}`
	record := UsageRecord{
		Timestamp:   time.Date(2026, time.July, 9, 1, 2, 3, 0, time.UTC),
		RequestID:   "req-derived",
		HomeIP:      "192.0.2.10",
		PayloadJSON: JSONB(payload),
		CreatedAt:   time.Now().UTC(),
	}
	if errCreate := db.Create(&record).Error; errCreate != nil {
		t.Fatalf("Create(usage) error = %v", errCreate)
	}

	first, errBackfill := repo.RunUsageDerivedColumnBackfillBatch(ctx)
	if errBackfill != nil {
		t.Fatalf("RunUsageDerivedColumnBackfillBatch() error = %v", errBackfill)
	}
	if first.Scanned != 1 || first.Updated != 1 || !first.Done || first.Skipped {
		t.Fatalf("first backfill result = %+v, want scanned=1 updated=1 done=true", first)
	}
	second, errBackfill := repo.RunUsageDerivedColumnBackfillBatch(ctx)
	if errBackfill != nil {
		t.Fatalf("RunUsageDerivedColumnBackfillBatch(second) error = %v", errBackfill)
	}
	if second.Scanned != 0 || second.Updated != 0 || !second.Done || second.Skipped {
		t.Fatalf("second backfill result = %+v, want completed no-op", second)
	}

	var updated UsageRecord
	if errFirst := db.First(&updated, "id = ?", record.ID).Error; errFirst != nil {
		t.Fatalf("First(usage) error = %v", errFirst)
	}
	if updated.EventType != "stream" {
		t.Fatalf("event_type = %q, want stream", updated.EventType)
	}
	if updated.UpstreamRequestID != "upstream-derived" || updated.UpstreamStatusCode != 202 {
		t.Fatalf("upstream = %q/%d, want upstream-derived/202", updated.UpstreamRequestID, updated.UpstreamStatusCode)
	}
	if updated.HomePort != 8328 {
		t.Fatalf("home_port = %d, want 8328", updated.HomePort)
	}
	if updated.CPANodeID != "node-derived" || updated.CPAIP != "10.0.0.5" || updated.CPAPort != 8317 || updated.CPALabel != "node-derived" {
		t.Fatalf("CPA ownership = node:%q ip:%q port:%d label:%q, want node-derived 10.0.0.5 8317 node-derived", updated.CPANodeID, updated.CPAIP, updated.CPAPort, updated.CPALabel)
	}
}

func TestUsageDerivedColumnBackfillBatchResumesByCursor(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()

	db, errDB := repo.database()
	if errDB != nil {
		t.Fatalf("database() error = %v", errDB)
	}
	now := time.Date(2026, time.July, 9, 1, 2, 3, 0, time.UTC)
	for index := 0; index < 3; index++ {
		record := UsageRecord{
			Timestamp:   now,
			PayloadJSON: JSONB(`{"timestamp":"2026-07-09T01:02:03Z","endpoint":"/v1/chat/completions"}`),
			CreatedAt:   now,
		}
		if errCreate := db.Create(&record).Error; errCreate != nil {
			t.Fatalf("Create(usage %d) error = %v", index, errCreate)
		}
	}

	first, errBackfill := runUsageDerivedColumnBackfillBatch(ctx, db, 2)
	if errBackfill != nil {
		t.Fatalf("runUsageDerivedColumnBackfillBatch(first) error = %v", errBackfill)
	}
	if first.Scanned != 2 || first.Updated != 2 || first.Done || first.Skipped {
		t.Fatalf("first backfill result = %+v, want scanned=2 updated=2 done=false", first)
	}
	second, errBackfill := runUsageDerivedColumnBackfillBatch(ctx, db, 2)
	if errBackfill != nil {
		t.Fatalf("runUsageDerivedColumnBackfillBatch(second) error = %v", errBackfill)
	}
	if second.Scanned != 1 || second.Updated != 1 || !second.Done || second.Skipped {
		t.Fatalf("second backfill result = %+v, want scanned=1 updated=1 done=true", second)
	}

	var stateRecord KVRecord
	if errState := db.First(&stateRecord, "key = ?", usageDerivedColumnBackfillStateKey).Error; errState != nil {
		t.Fatalf("First(derived-column backfill state) error = %v", errState)
	}
	state := usageDerivedColumnBackfillState{}
	if errDecode := json.Unmarshal(stateRecord.Value, &state); errDecode != nil {
		t.Fatalf("decode derived-column backfill state: %v", errDecode)
	}
	if !state.Done || state.LastScannedID != state.HighWaterID {
		t.Fatalf("derived-column backfill state = %+v, want complete high-water cursor", state)
	}

	var records []UsageRecord
	if errFind := db.Order("id ASC").Find(&records).Error; errFind != nil {
		t.Fatalf("Find(usage) error = %v", errFind)
	}
	for index := range records {
		if records[index].EventType != "completion" {
			t.Fatalf("usage %d event_type = %q, want completion", index, records[index].EventType)
		}
	}
}

func TestUsageDerivedColumnBackfillBatchCatchesLateLegacyRowsBelowHighWater(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()

	db, errDB := repo.database()
	if errDB != nil {
		t.Fatalf("database() error = %v", errDB)
	}
	now := time.Date(2026, time.July, 9, 1, 2, 3, 0, time.UTC)
	initial := UsageRecord{
		ID:          20,
		Timestamp:   now,
		PayloadJSON: JSONB(`{"timestamp":"2026-07-09T01:02:03Z","endpoint":"/v1/chat/completions"}`),
		CreatedAt:   now,
	}
	if errCreate := db.Create(&initial).Error; errCreate != nil {
		t.Fatalf("Create(initial usage) error = %v", errCreate)
	}
	first, errBackfill := repo.RunUsageDerivedColumnBackfillBatch(ctx)
	if errBackfill != nil {
		t.Fatalf("RunUsageDerivedColumnBackfillBatch(initial) error = %v", errBackfill)
	}
	if first.Scanned != 1 || first.Updated != 1 || !first.Done {
		t.Fatalf("initial backfill result = %+v, want scanned=1 updated=1 done=true", first)
	}

	latePayload := `{"timestamp":"2026-07-09T01:03:03Z","event_type":"stream","upstream_request_id":"upstream-late","upstream_status_code":202,"home_port":8328,"cpa_node_id":"node-late","cpa_ip":"10.0.0.8","cpa_port":8317}`
	late := UsageRecord{
		ID:          10,
		Timestamp:   now.Add(time.Minute),
		PayloadJSON: JSONB(latePayload),
		CreatedAt:   now.Add(time.Minute),
	}
	if errCreate := db.Create(&late).Error; errCreate != nil {
		t.Fatalf("Create(late usage) error = %v", errCreate)
	}
	second, errBackfill := repo.RunUsageDerivedColumnBackfillBatch(ctx)
	if errBackfill != nil {
		t.Fatalf("RunUsageDerivedColumnBackfillBatch(late) error = %v", errBackfill)
	}
	if second.Scanned != 1 || second.Updated != 1 || !second.Done {
		t.Fatalf("late backfill result = %+v, want scanned=1 updated=1 done=true", second)
	}

	var updated UsageRecord
	if errFind := db.First(&updated, "id = ?", late.ID).Error; errFind != nil {
		t.Fatalf("First(late usage) error = %v", errFind)
	}
	if updated.EventType != "stream" || updated.UpstreamRequestID != "upstream-late" || updated.UpstreamStatusCode != 202 {
		t.Fatalf("late usage metadata = event:%q upstream:%q/%d", updated.EventType, updated.UpstreamRequestID, updated.UpstreamStatusCode)
	}
	if updated.HomePort != 8328 || updated.CPANodeID != "node-late" || updated.CPAIP != "10.0.0.8" || updated.CPAPort != 8317 || updated.CPALabel != "node-late" {
		t.Fatalf("late usage runtime = home_port:%d node:%q ip:%q port:%d label:%q", updated.HomePort, updated.CPANodeID, updated.CPAIP, updated.CPAPort, updated.CPALabel)
	}
}

func TestUsageCacheReadBackfillBatchBackfillsLegacyRowsIdempotently(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()

	db, errDB := repo.database()
	if errDB != nil {
		t.Fatalf("database() error = %v", errDB)
	}
	now := time.Date(2026, time.July, 12, 1, 2, 3, 0, time.UTC)
	records := []*UsageRecord{
		{
			Timestamp:           now,
			Provider:            "openai",
			ExecutorType:        "OpenAICompatExecutor",
			CachedTokens:        100,
			CacheCreationTokens: 25,
			PayloadJSON:         JSONB(`{}`),
			CreatedAt:           now,
		},
		{
			Timestamp:           now,
			Provider:            "claude",
			ExecutorType:        "ClaudeExecutor",
			CachedTokens:        40,
			CacheCreationTokens: 40,
			PayloadJSON:         JSONB(`{}`),
			CreatedAt:           now,
		},
		{
			Timestamp:       now,
			Provider:        "openai",
			ExecutorType:    "AnthropicExecutor",
			CachedTokens:    30,
			PayloadJSON:     JSONB(`{}`),
			CreatedAt:       now,
			CacheReadTokens: 0,
		},
		{
			Timestamp:       now,
			Provider:        "openai-compatible-anthropic",
			ExecutorType:    "OpenAICompatExecutor",
			CachedTokens:    20,
			PayloadJSON:     JSONB(`{}`),
			CreatedAt:       now,
			CacheReadTokens: 0,
		},
	}
	for _, record := range records {
		if errCreate := db.Create(record).Error; errCreate != nil {
			t.Fatalf("Create(usage) error = %v", errCreate)
		}
	}

	first, errBackfill := repo.RunUsageCacheReadBackfillBatch(ctx)
	if errBackfill != nil {
		t.Fatalf("RunUsageCacheReadBackfillBatch() error = %v", errBackfill)
	}
	if first.Scanned != len(records) || first.Updated != 2 || !first.Done || first.Skipped {
		t.Fatalf("first backfill result = %+v, want scanned=%d updated=2 done=true", first, len(records))
	}
	second, errBackfill := repo.RunUsageCacheReadBackfillBatch(ctx)
	if errBackfill != nil {
		t.Fatalf("RunUsageCacheReadBackfillBatch(second) error = %v", errBackfill)
	}
	if second.Scanned != 0 || second.Updated != 0 || !second.Done || second.Skipped {
		t.Fatalf("second backfill result = %+v, want completed no-op", second)
	}

	var updated []UsageRecord
	if errFind := db.Order("id ASC").Find(&updated).Error; errFind != nil {
		t.Fatalf("Find(usage) error = %v", errFind)
	}
	if len(updated) != len(records) {
		t.Fatalf("usage record count = %d, want %d", len(updated), len(records))
	}
	if updated[0].CacheReadTokens != 100 || updated[0].CachedTokens != 100 || updated[0].CacheCreationTokens != 25 {
		t.Fatalf("legacy OpenAI cache tokens = %+v, want cached/read/creation 100/100/25", updated[0])
	}
	if updated[1].CacheReadTokens != 0 || updated[1].CachedTokens != 40 || updated[1].CacheCreationTokens != 40 {
		t.Fatalf("Claude cache tokens = %+v, want cached/read/creation 40/0/40", updated[1])
	}
	if updated[2].CacheReadTokens != 0 || updated[2].CachedTokens != 30 {
		t.Fatalf("Anthropic executor cache tokens = %+v, want cached/read 30/0", updated[2])
	}
	if updated[3].CacheReadTokens != 20 || updated[3].CachedTokens != 20 {
		t.Fatalf("OpenAI-compatible cache tokens = %+v, want cached/read 20/20", updated[3])
	}
}

func TestUsageCacheReadBackfillBatchResumesByCursor(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()

	db, errDB := repo.database()
	if errDB != nil {
		t.Fatalf("database() error = %v", errDB)
	}
	now := time.Date(2026, time.July, 12, 1, 2, 3, 0, time.UTC)
	for _, record := range []*UsageRecord{
		{Timestamp: now, Provider: "openai", CachedTokens: 10, PayloadJSON: JSONB(`{}`), CreatedAt: now},
		{Timestamp: now, Provider: "claude", ExecutorType: "ClaudeExecutor", CachedTokens: 20, CacheCreationTokens: 20, PayloadJSON: JSONB(`{}`), CreatedAt: now},
		{Timestamp: now, Provider: "openai", CachedTokens: 30, CacheReadTokens: 30, PayloadJSON: JSONB(`{}`), CreatedAt: now},
		{Timestamp: now, Provider: "openai", CachedTokens: 40, PayloadJSON: JSONB(`{}`), CreatedAt: now},
		{Timestamp: now, Provider: "openai", ExecutorType: "AnthropicExecutor", CachedTokens: 50, PayloadJSON: JSONB(`{}`), CreatedAt: now},
		{Timestamp: now, Provider: "openai", CachedTokens: 60, PayloadJSON: JSONB(`{}`), CreatedAt: now},
	} {
		if errCreate := db.Create(record).Error; errCreate != nil {
			t.Fatalf("Create(usage) error = %v", errCreate)
		}
	}

	first, errBackfill := runUsageCacheReadBackfillBatch(ctx, db, 2)
	if errBackfill != nil {
		t.Fatalf("runUsageCacheReadBackfillBatch(first) error = %v", errBackfill)
	}
	if first.Scanned != 2 || first.Updated != 1 || first.Done || first.Skipped {
		t.Fatalf("first backfill result = %+v, want scanned=2 updated=1 done=false", first)
	}
	second, errBackfill := runUsageCacheReadBackfillBatch(ctx, db, 2)
	if errBackfill != nil {
		t.Fatalf("runUsageCacheReadBackfillBatch(second) error = %v", errBackfill)
	}
	if second.Scanned != 2 || second.Updated != 1 || second.Done || second.Skipped {
		t.Fatalf("second backfill result = %+v, want scanned=2 updated=1 done=false", second)
	}
	third, errBackfill := runUsageCacheReadBackfillBatch(ctx, db, 2)
	if errBackfill != nil {
		t.Fatalf("runUsageCacheReadBackfillBatch(third) error = %v", errBackfill)
	}
	if third.Scanned != 2 || third.Updated != 1 || !third.Done || third.Skipped {
		t.Fatalf("third backfill result = %+v, want scanned=2 updated=1 done=true", third)
	}

	var stateRecord KVRecord
	if errState := db.First(&stateRecord, "key = ?", usageCacheReadBackfillStateKey).Error; errState != nil {
		t.Fatalf("First(backfill state) error = %v", errState)
	}
	state := usageCacheReadBackfillState{}
	if errDecode := json.Unmarshal(stateRecord.Value, &state); errDecode != nil {
		t.Fatalf("decode backfill state: %v", errDecode)
	}
	if !state.Done || state.LastScannedID != state.HighWaterID {
		t.Fatalf("backfill state = %+v, want complete high-water cursor", state)
	}

	var records []UsageRecord
	if errFind := db.Order("id ASC").Find(&records).Error; errFind != nil {
		t.Fatalf("Find(usage) error = %v", errFind)
	}
	if len(records) != 6 {
		t.Fatalf("usage record count = %d, want 6", len(records))
	}
	if records[0].CacheReadTokens != 10 || records[1].CacheReadTokens != 0 || records[2].CacheReadTokens != 30 || records[3].CacheReadTokens != 40 || records[4].CacheReadTokens != 0 || records[5].CacheReadTokens != 60 {
		t.Fatalf("backfilled cache read tokens = %d,%d,%d,%d,%d,%d, want 10,0,30,40,0,60", records[0].CacheReadTokens, records[1].CacheReadTokens, records[2].CacheReadTokens, records[3].CacheReadTokens, records[4].CacheReadTokens, records[5].CacheReadTokens)
	}
}

func TestUsageCacheReadBackfillSkipsCanonicalZeroRead(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	db, errDB := repo.database()
	if errDB != nil {
		t.Fatalf("database() error = %v", errDB)
	}
	now := time.Date(2026, time.July, 12, 1, 2, 3, 0, time.UTC)
	record := &UsageRecord{
		Timestamp:              now,
		Provider:               "openai",
		CachedTokens:           10,
		CacheReadTokensPresent: true,
		PayloadJSON:            JSONB(`{}`),
		CreatedAt:              now,
	}
	if errCreate := db.Create(record).Error; errCreate != nil {
		t.Fatalf("Create(usage) error = %v", errCreate)
	}

	result, errBackfill := repo.RunUsageCacheReadBackfillBatch(ctx)
	if errBackfill != nil {
		t.Fatalf("RunUsageCacheReadBackfillBatch() error = %v", errBackfill)
	}
	if result.Updated != 0 || !result.Done {
		t.Fatalf("backfill result = %+v, want no update and done", result)
	}
	var stored UsageRecord
	if errFind := db.First(&stored, record.ID).Error; errFind != nil {
		t.Fatalf("First(usage) error = %v", errFind)
	}
	if stored.CacheReadTokens != 0 || !stored.CacheReadTokensPresent {
		t.Fatalf("stored cache read = %d present=%t, want 0/true", stored.CacheReadTokens, stored.CacheReadTokensPresent)
	}
}

func TestAutoMigrateDoesNotRunUsageBackfills(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	db, errDB := repo.database()
	if errDB != nil {
		t.Fatalf("database() error = %v", errDB)
	}
	now := time.Date(2026, time.July, 12, 1, 2, 3, 0, time.UTC)
	record := &UsageRecord{
		Timestamp:    now,
		Provider:     "openai",
		CachedTokens: 100,
		PayloadJSON:  JSONB(`{"timestamp":"2026-07-12T01:02:03Z","endpoint":"/v1/chat/completions"}`),
		CreatedAt:    now,
	}
	if errCreate := db.Create(record).Error; errCreate != nil {
		t.Fatalf("Create(usage) error = %v", errCreate)
	}
	if errMigrate := AutoMigrate(db); errMigrate != nil {
		t.Fatalf("AutoMigrate() error = %v", errMigrate)
	}
	var stored UsageRecord
	if errFind := db.First(&stored, record.ID).Error; errFind != nil {
		t.Fatalf("First(usage) error = %v", errFind)
	}
	if stored.CacheReadTokens != 0 {
		t.Fatalf("AutoMigrate cache read tokens = %d, want deferred backfill", stored.CacheReadTokens)
	}
	if stored.EventType != "" {
		t.Fatalf("AutoMigrate event type = %q, want deferred backfill", stored.EventType)
	}
	for _, stateKey := range []string{usageCacheReadBackfillStateKey, usageDerivedColumnBackfillStateKey} {
		if errState := db.First(&KVRecord{}, "key = ?", stateKey).Error; errState == nil {
			t.Fatalf("AutoMigrate unexpectedly created usage backfill state %q", stateKey)
		}
	}
}

func TestUsageObservabilityOverviewTopCredentialUsesModelStateNextRetry(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()

	username := "usage-retry-user"
	credits := 100.0
	user, errCreateUser := repo.CreateUser(ctx, UserUpdate{Username: &username, Credits: &credits})
	if errCreateUser != nil {
		t.Fatalf("CreateUser() error = %v", errCreateUser)
	}
	clientKey := "client-key-retry-secret-1234"
	if _, errCreateKey := repo.CreateAPIKeyForUser(ctx, user.ID, APIKeyUserUpdate{APIKey: &clientKey}); errCreateKey != nil {
		t.Fatalf("CreateAPIKeyForUser() error = %v", errCreateKey)
	}
	modelRetryAt := time.Date(2026, time.June, 10, 1, 30, 0, 0, time.UTC)
	auth := &coreauth.Auth{
		ID:        "auth-model-retry",
		Index:     "auth-model-retry",
		Provider:  "codex",
		Label:     "Model Retry OAuth",
		Status:    coreauth.StatusActive,
		CreatedAt: time.Date(2026, time.June, 10, 1, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, time.June, 10, 1, 0, 0, 0, time.UTC),
		ModelStates: map[string]*coreauth.ModelState{
			"gpt-4.1-mini": {
				Status:         coreauth.StatusError,
				Unavailable:    true,
				NextRetryAfter: modelRetryAt,
				UpdatedAt:      time.Date(2026, time.June, 10, 1, 10, 0, 0, time.UTC),
			},
		},
	}
	if _, errAuth := repo.UpsertAuth(ctx, auth, "test"); errAuth != nil {
		t.Fatalf("UpsertAuth() error = %v", errAuth)
	}
	db, errDB := repo.database()
	if errDB != nil {
		t.Fatalf("database() error = %v", errDB)
	}
	var authRecord AuthRecord
	if errFirst := db.First(&authRecord, "uuid = ?", auth.ID).Error; errFirst != nil {
		t.Fatalf("First(auth) error = %v", errFirst)
	}
	if authRecord.NextRetryAfter == nil || !authRecord.NextRetryAfter.UTC().Equal(modelRetryAt) {
		t.Fatalf("next_retry_after = %v, want %v", authRecord.NextRetryAfter, modelRetryAt)
	}
	if _, errCreatePrice := repo.CreateBillingModelPrice(ctx, BillingModelPriceUpdate{Provider: "openai", Model: "gpt-4.1-mini", RequestPrice: 2, Enabled: true}); errCreatePrice != nil {
		t.Fatalf("CreateBillingModelPrice() error = %v", errCreatePrice)
	}
	payload := `{"timestamp":"2026-06-10T01:15:00Z","provider":"openai","model":"gpt-4.1-mini","api_key":"client-key-retry-secret-1234","request_id":"req-obs-model-retry","endpoint":"/v1/chat/completions","executor_type":"CodexWebsocketsExecutor","auth_index":"auth-model-retry","auth_type":"oauth","latency_ms":500,"tokens":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}`
	if _, errUsage := repo.AppendUsage(ctx, payload, "192.0.2.10"); errUsage != nil {
		t.Fatalf("AppendUsage() error = %v", errUsage)
	}

	from := time.Date(2026, time.June, 10, 1, 0, 0, 0, time.UTC)
	to := time.Date(2026, time.June, 10, 2, 0, 0, 0, time.UTC)
	overview, errOverview := repo.UsageObservabilityOverview(ctx, UsageObservabilityOverviewQuery{From: &from, To: &to, Interval: "hour", Timezone: "UTC"})
	if errOverview != nil {
		t.Fatalf("UsageObservabilityOverview() error = %v", errOverview)
	}
	if len(overview.Top.Credentials) != 1 {
		t.Fatalf("credential count = %d, want 1", len(overview.Top.Credentials))
	}
	nextRetryAt, ok := overview.Top.Credentials[0].Metadata["next_retry_at"].(string)
	if !ok {
		t.Fatalf("next_retry_at = %T, want string", overview.Top.Credentials[0].Metadata["next_retry_at"])
	}
	if nextRetryAt != modelRetryAt.Format(time.RFC3339Nano) {
		t.Fatalf("next_retry_at = %q, want %q", nextRetryAt, modelRetryAt.Format(time.RFC3339Nano))
	}
}

func TestListUsageObservabilityRecordsJoinsBillingAndMasksClientKey(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()

	seedUsageObservabilityRecord(t, ctx, repo)

	result, errRecords := repo.ListUsageObservabilityRecords(ctx, UsageObservabilityRecordQuery{Limit: 10, Sort: "timestamp_desc"})
	if errRecords != nil {
		t.Fatalf("ListUsageObservabilityRecords() error = %v", errRecords)
	}
	if result.Total != 1 {
		t.Fatalf("total = %d, want 1", result.Total)
	}
	if len(result.Records) != 1 {
		t.Fatalf("record count = %d, want 1", len(result.Records))
	}

	record := result.Records[0]
	if record.UsageID == 0 {
		t.Fatal("usage id was not populated")
	}
	if record.RequestID != "req-obs-1" {
		t.Fatalf("request id = %q, want req-obs-1", record.RequestID)
	}
	if record.Client.APIKeyID == nil || *record.Client.APIKeyID == 0 {
		t.Fatalf("client api key id = %v, want populated", record.Client.APIKeyID)
	}
	if record.Client.UserID == nil || *record.Client.UserID == 0 {
		t.Fatalf("client user id = %v, want populated", record.Client.UserID)
	}
	if record.Client.Username != "usage-user" {
		t.Fatalf("username = %q, want usage-user", record.Client.Username)
	}
	if strings.Contains(record.Client.APIKeyMasked, "client-key-secret") {
		t.Fatalf("api key mask leaked raw key: %q", record.Client.APIKeyMasked)
	}
	if record.Client.APIKeyMasked != "clie...1234" {
		t.Fatalf("api key mask = %q, want clie...1234", record.Client.APIKeyMasked)
	}
	if record.Credential.CredentialID != "auth-observability" {
		t.Fatalf("credential id = %q, want auth-observability", record.Credential.CredentialID)
	}
	if record.Credential.Label != "Primary OAuth" {
		t.Fatalf("credential label = %q, want Primary OAuth", record.Credential.Label)
	}
	if record.Billing.Amount == nil || *record.Billing.Amount != 2 {
		t.Fatalf("billing amount = %v, want 2", record.Billing.Amount)
	}
	if record.Billing.Currency != UsageObservabilityCurrencyCredits {
		t.Fatalf("currency = %q, want %q", record.Billing.Currency, UsageObservabilityCurrencyCredits)
	}
}

func TestUsageObservabilityAggregateCacheRateUsesExplicitBuckets(t *testing.T) {
	t.Parallel()

	row := &usageObservabilityAggregateRow{
		CachedTokens:               20,
		CacheReadTokens:            20,
		CacheCreationTokens:        10,
		TotalTokens:                100,
		TokenAccountingQuality:     UsageTokenAccountingQualityComplete,
		AccountingTotalTokens:      100,
		AccountingInputTokens:      100,
		UncachedInputTokens:        70,
		AccountingCacheReadTokens:  20,
		AccountingCacheWriteTokens: 10,
	}
	item := usageObservabilityAggregateItemFromRow(row, "provider")
	if item.CacheRate != 0.3 {
		t.Fatalf("cache rate = %v, want 0.3", item.CacheRate)
	}

	accumulator := usageObservabilityAggregateAccumulator{Item: item}
	accumulator.Item.CacheRate = 0
	if result := accumulator.result(); result.CacheRate != 0.3 {
		t.Fatalf("accumulator cache rate = %v, want 0.3", result.CacheRate)
	}
}

func TestUsageObservabilityAggregateCacheRatePreservesCanonicalZeroRead(t *testing.T) {
	t.Parallel()

	row := &usageObservabilityAggregateRow{
		CachedTokens:           20,
		CacheReadTokens:        0,
		TotalTokens:            100,
		TokenAccountingQuality: UsageTokenAccountingQualityComplete,
		AccountingTotalTokens:  100,
		AccountingInputTokens:  100,
		UncachedInputTokens:    100,
	}
	item := usageObservabilityAggregateItemFromRow(row, "provider")
	if item.CacheRate != 0 {
		t.Fatalf("cache rate = %v, want 0", item.CacheRate)
	}

	accumulator := usageObservabilityAggregateAccumulator{Item: item}
	accumulator.Item.CacheRate = 1
	if result := accumulator.result(); result.CacheRate != 0 {
		t.Fatalf("accumulator cache rate = %v, want 0", result.CacheRate)
	}
}

func TestUsageObservabilityAggregateNormalizesMixedLegacyCacheHistory(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()

	db, errDB := repo.database()
	if errDB != nil {
		t.Fatalf("database() error = %v", errDB)
	}
	now := time.Date(2026, time.July, 12, 1, 2, 3, 0, time.UTC)
	legacy := &UsageRecord{
		Timestamp:    now,
		Provider:     "openai",
		Model:        "gpt-5",
		InputTokens:  900,
		OutputTokens: 100,
		CachedTokens: 100,
		TotalTokens:  1000,
		PayloadJSON:  JSONB(`{}`),
		CreatedAt:    now,
	}
	current := &UsageRecord{
		Timestamp:              now.Add(time.Second),
		Provider:               "openai",
		Model:                  "gpt-5",
		InputTokens:            900,
		OutputTokens:           100,
		CachedTokens:           100,
		CacheReadTokens:        100,
		CacheReadTokensPresent: true,
		CacheCreationTokens:    50,
		TotalTokens:            1000,
		PayloadJSON:            JSONB(`{}`),
		CreatedAt:              now,
	}
	for _, record := range []*UsageRecord{legacy, current} {
		if errCreate := db.Create(record).Error; errCreate != nil {
			t.Fatalf("Create(usage) error = %v", errCreate)
		}
	}
	backfill, errBackfill := repo.RunUsageTokenAccountingBackfillBatch(ctx)
	if errBackfill != nil {
		t.Fatalf("RunUsageTokenAccountingBackfillBatch() error = %v", errBackfill)
	}
	if !backfill.Done || backfill.Updated != 2 {
		t.Fatalf("usage token accounting backfill = %+v, want done with 2 updates", backfill)
	}

	result, errAggregates := repo.ListUsageObservabilityAggregates(ctx, UsageObservabilityAggregateQuery{
		GroupBy:   "provider",
		Metric:    "total_tokens",
		Direction: "desc",
		Limit:     10,
	})
	if errAggregates != nil {
		t.Fatalf("ListUsageObservabilityAggregates() error = %v", errAggregates)
	}
	if len(result.Items) != 1 {
		t.Fatalf("aggregate item count = %d, want 1", len(result.Items))
	}
	item := result.Items[0]
	if item.CachedTokens != 200 || item.CacheReadTokens != 200 || item.CacheCreationTokens != 50 {
		t.Fatalf("aggregate cache tokens = cached:%d read:%d creation:%d, want 200/200/50", item.CachedTokens, item.CacheReadTokens, item.CacheCreationTokens)
	}
	if item.CacheRate != 0.125 {
		t.Fatalf("cache rate = %v, want 0.125", item.CacheRate)
	}

	from := now.Add(-time.Second)
	to := now.Add(2 * time.Second)
	overview, errOverview := repo.UsageObservabilityOverview(ctx, UsageObservabilityOverviewQuery{
		From:     &from,
		To:       &to,
		Interval: "hour",
		Timezone: "UTC",
	})
	if errOverview != nil {
		t.Fatalf("UsageObservabilityOverview() error = %v", errOverview)
	}
	if overview.Totals.CacheReadTokens != 200 {
		t.Fatalf("overview cache read tokens = %d, want 200", overview.Totals.CacheReadTokens)
	}
	if len(overview.Trend) != 1 || overview.Trend[0].CacheReadTokens != 200 {
		t.Fatalf("overview trend = %+v, want one point with 200 cache read tokens", overview.Trend)
	}
}

func TestUsageObservabilityLegacyTotalFallbackPreservesOpenAICompatibilityOverride(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()

	db, errDB := repo.database()
	if errDB != nil {
		t.Fatalf("database() error = %v", errDB)
	}
	now := time.Now().UTC()
	record := UsageRecord{
		Timestamp:           now,
		Provider:            "openai-compatible-anthropic",
		ExecutorType:        "OpenAICompatExecutor",
		InputTokens:         80,
		OutputTokens:        20,
		ReasoningTokens:     10,
		CacheReadTokens:     30,
		CacheCreationTokens: 10,
		PayloadJSON:         JSONB(`{}`),
		CreatedAt:           now,
	}
	if errCreate := db.Create(&record).Error; errCreate != nil {
		t.Fatalf("Create(usage) error = %v", errCreate)
	}

	totals, _, errTotals := usageObservabilityTotalsSQL(db, UsageObservabilityRecordQuery{Provider: record.Provider})
	if errTotals != nil {
		t.Fatalf("usageObservabilityTotalsSQL() error = %v", errTotals)
	}
	if totals.TokenBreakdown.TotalTokens != 100 {
		t.Fatalf("accounting total tokens = %d, want 100", totals.TokenBreakdown.TotalTokens)
	}
}

func TestUsageObservabilityMetricsFallbackWhileTokenBackfillIsPending(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()

	db, errDB := repo.database()
	if errDB != nil {
		t.Fatalf("database() error = %v", errDB)
	}
	now := time.Now().UTC().Truncate(time.Second)
	records := []UsageRecord{
		{
			Timestamp:           now,
			Provider:            "openai",
			Model:               "shared",
			InputTokens:         80,
			OutputTokens:        20,
			CachedTokens:        20,
			CacheCreationTokens: 10,
			PayloadJSON:         JSONB(`{}`),
			CreatedAt:           now,
		},
		{
			Timestamp:                 now,
			Provider:                  "codex",
			Model:                     "shared",
			InputTokens:               200,
			OutputTokens:              100,
			TotalTokens:               300,
			TokenAccountingVersion:    UsageTokenAccountingSchemaVersion,
			TokenAccountingQuality:    UsageTokenAccountingQualityComplete,
			AccountingTotalTokens:     300,
			AccountingInputTokens:     200,
			UncachedInputTokens:       140,
			AccountingCacheReadTokens: 60,
			AccountingOutputTokens:    100,
			NonReasoningOutputTokens:  100,
			PayloadJSON:               JSONB(`{}`),
			CreatedAt:                 now,
		},
	}
	if errCreate := db.Create(&records).Error; errCreate != nil {
		t.Fatalf("Create(usage) error = %v", errCreate)
	}
	for index := range records {
		charge := BillingChargeRecord{
			ID:            "charge-backfill-" + records[index].Provider,
			UsageID:       records[index].ID,
			PayloadHash:   "hash-backfill-" + records[index].Provider,
			Provider:      records[index].Provider,
			Model:         records[index].Model,
			Amount:        float64(index + 1),
			PriceSnapshot: JSONB(`{}`),
			CreatedAt:     now,
		}
		if errCreate := db.Create(&charge).Error; errCreate != nil {
			t.Fatalf("Create(billing charge) error = %v", errCreate)
		}
	}

	from := now.Add(-time.Second)
	to := now.Add(time.Minute)
	overview, errOverview := repo.UsageObservabilityOverview(ctx, UsageObservabilityOverviewQuery{
		From:     &from,
		To:       &to,
		Interval: "minute",
		Timezone: "UTC",
	})
	if errOverview != nil {
		t.Fatalf("UsageObservabilityOverview() error = %v", errOverview)
	}
	if overview.Totals.TokenBreakdown.TotalTokens != 400 || overview.Live.TPM != 80 {
		t.Fatalf("overview tokens = total:%d live_tpm:%v, want 400/80", overview.Totals.TokenBreakdown.TotalTokens, overview.Live.TPM)
	}
	if overview.Totals.BlendedCostPer1M == nil || *overview.Totals.BlendedCostPer1M != 7500 {
		t.Fatalf("blended cost = %v, want 7500", overview.Totals.BlendedCostPer1M)
	}

	list, errList := repo.ListUsageObservabilityRecords(ctx, UsageObservabilityRecordQuery{From: &from, To: &to, Sort: "tokens_desc", Limit: 10})
	if errList != nil {
		t.Fatalf("ListUsageObservabilityRecords() error = %v", errList)
	}
	if len(list.Records) != 2 || list.Records[0].Provider != "codex" {
		t.Fatalf("records = %+v, want codex first", list.Records)
	}

	aggregates, errAggregates := repo.ListUsageObservabilityAggregates(ctx, UsageObservabilityAggregateQuery{
		From:      &from,
		To:        &to,
		GroupBy:   "provider",
		Metric:    "total_tokens",
		Direction: "desc",
		Limit:     10,
	})
	if errAggregates != nil {
		t.Fatalf("ListUsageObservabilityAggregates() error = %v", errAggregates)
	}
	if len(aggregates.Items) != 2 || aggregates.Items[0].ID != "codex" || aggregates.Items[0].TokenBreakdown.TotalTokens != 300 {
		t.Fatalf("aggregates = %+v, want codex with 300 tokens first", aggregates.Items)
	}
	if aggregates.Items[1].CacheRate != 0.3 {
		t.Fatalf("openai cache rate = %v, want 0.3", aggregates.Items[1].CacheRate)
	}

	mixed, errMixed := repo.ListUsageObservabilityAggregates(ctx, UsageObservabilityAggregateQuery{
		From:      &from,
		To:        &to,
		GroupBy:   "model",
		Metric:    "total_tokens",
		Direction: "desc",
		Limit:     10,
	})
	if errMixed != nil {
		t.Fatalf("ListUsageObservabilityAggregates(mixed) error = %v", errMixed)
	}
	if len(mixed.Items) != 1 || mixed.Items[0].CacheRate != 0.225 {
		t.Fatalf("mixed aggregate = %+v, want cache rate 0.225", mixed.Items)
	}

	realtime, errRealtime := repo.UsageObservabilityRealtime(ctx, UsageObservabilityRealtimeQuery{
		From:          &from,
		To:            &to,
		GroupBy:       "provider",
		BucketSeconds: 60,
	})
	if errRealtime != nil {
		t.Fatalf("UsageObservabilityRealtime() error = %v", errRealtime)
	}
	if len(realtime.Velocity) != 1 || realtime.Velocity[0].TPM != 400 {
		t.Fatalf("realtime velocity = %+v, want one bucket with TPM 400", realtime.Velocity)
	}
}

func TestListUsageObservabilityAggregatesSortsBeforePagination(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()

	seedUsageObservabilityRecord(t, ctx, repo)
	if _, errCreatePrice := repo.CreateBillingModelPrice(ctx, BillingModelPriceUpdate{Provider: "openai", Model: "gpt-4.1", RequestPrice: 5, Enabled: true}); errCreatePrice != nil {
		t.Fatalf("CreateBillingModelPrice(second) error = %v", errCreatePrice)
	}
	payload := `{"timestamp":"2026-06-10T01:03:03Z","provider":"openai","model":"gpt-4.1","api_key":"client-key-secret-1234","request_id":"req-obs-2","endpoint":"/v1/chat/completions","executor_type":"CodexWebsocketsExecutor","auth_index":"auth-observability","auth_type":"oauth","latency_ms":2460,"tokens":{"input_tokens":200,"output_tokens":100,"total_tokens":300}}`
	if _, errUsage := repo.AppendUsage(ctx, payload, "192.0.2.10"); errUsage != nil {
		t.Fatalf("AppendUsage(second) error = %v", errUsage)
	}

	result, errAggregates := repo.ListUsageObservabilityAggregates(ctx, UsageObservabilityAggregateQuery{
		GroupBy:   "model",
		Metric:    "total_amount",
		Direction: "desc",
		Limit:     1,
	})
	if errAggregates != nil {
		t.Fatalf("ListUsageObservabilityAggregates() error = %v", errAggregates)
	}
	if result.Total != 2 {
		t.Fatalf("total = %d, want 2", result.Total)
	}
	if len(result.Items) != 1 {
		t.Fatalf("item count = %d, want 1", len(result.Items))
	}
	item := result.Items[0]
	if item.ID != "gpt-4.1" {
		t.Fatalf("top model = %q, want gpt-4.1", item.ID)
	}
	if item.TotalAmount == nil || *item.TotalAmount != 5 {
		t.Fatalf("top total amount = %v, want 5", item.TotalAmount)
	}
	if item.P95LatencyMS == nil || *item.P95LatencyMS != 2460 {
		t.Fatalf("top p95 latency = %v, want 2460", item.P95LatencyMS)
	}
}

func TestUsageObservabilityAggregateSQLOrderUsesTotalAmountExpression(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		direction string
		want      string
	}{
		{name: "descending", direction: "desc", want: "COALESCE(SUM(scoped.amount), 0) DESC, last_used_at DESC, aggregate_label ASC"},
		{name: "ascending", direction: "asc", want: "COALESCE(SUM(scoped.amount), 0) ASC, last_used_at DESC, aggregate_label ASC"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := usageObservabilityAggregateSQLOrder("total_amount", test.direction); got != test.want {
				t.Fatalf("usageObservabilityAggregateSQLOrder() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestGetUsageObservabilityRecordReturnsRecord(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()

	seedUsageObservabilityRecord(t, ctx, repo)

	record, errRecord := repo.GetUsageObservabilityRecord(ctx, "1")
	if errRecord != nil {
		t.Fatalf("GetUsageObservabilityRecord() error = %v", errRecord)
	}
	if record.UsageID != 1 {
		t.Fatalf("usage id = %d, want 1", record.UsageID)
	}
	if record.Client.APIKeyMasked != "clie...1234" {
		t.Fatalf("api key mask = %q, want clie...1234", record.Client.APIKeyMasked)
	}
}

func TestUsageObservabilityOverviewBuildsTrendWithSQLBuckets(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()

	seedUsageObservabilityRecord(t, ctx, repo)
	payloads := []string{
		`{"timestamp":"2026-06-10T01:45:00Z","provider":"openai","model":"gpt-4.1-mini","api_key":"client-key-secret-1234","request_id":"req-obs-trend-2","endpoint":"/v1/chat/completions","executor_type":"CodexWebsocketsExecutor","auth_index":"auth-observability","auth_type":"oauth","latency_ms":2460,"tokens":{"input_tokens":200,"output_tokens":100,"total_tokens":300}}`,
		`{"timestamp":"2026-06-10T02:05:00Z","provider":"openai","model":"gpt-4.1-mini","api_key":"client-key-secret-1234","request_id":"req-obs-trend-3","endpoint":"/v1/chat/completions","executor_type":"CodexWebsocketsExecutor","auth_index":"auth-observability","auth_type":"oauth","latency_ms":500,"failed":true,"fail":{"status_code":429},"tokens":{"input_tokens":20,"output_tokens":10,"total_tokens":30}}`,
	}
	for index, payload := range payloads {
		if _, errUsage := repo.AppendUsage(ctx, payload, "192.0.2.10"); errUsage != nil {
			t.Fatalf("AppendUsage(%d) error = %v", index, errUsage)
		}
	}

	from := time.Date(2026, time.June, 10, 1, 0, 0, 0, time.UTC)
	to := time.Date(2026, time.June, 10, 2, 59, 59, 0, time.UTC)
	overview, errOverview := repo.UsageObservabilityOverview(ctx, UsageObservabilityOverviewQuery{
		From:     &from,
		To:       &to,
		Interval: "hour",
		Timezone: "UTC",
	})
	if errOverview != nil {
		t.Fatalf("UsageObservabilityOverview() error = %v", errOverview)
	}
	if len(overview.Trend) != 2 {
		t.Fatalf("trend point count = %d, want 2", len(overview.Trend))
	}

	first := overview.Trend[0]
	if !first.BucketStart.Equal(from) {
		t.Fatalf("first bucket start = %v, want %v", first.BucketStart, from)
	}
	if first.RequestCount != 2 || first.SuccessCount != 2 || first.FailedCount != 0 {
		t.Fatalf("first counts = requests:%d success:%d failed:%d, want 2/2/0", first.RequestCount, first.SuccessCount, first.FailedCount)
	}
	if first.TotalTokens != 450 {
		t.Fatalf("first total tokens = %d, want 450", first.TotalTokens)
	}
	if first.AvgLatencyMS == nil || *first.AvgLatencyMS != 1960 {
		t.Fatalf("first avg latency = %v, want 1960", first.AvgLatencyMS)
	}
	if first.P95LatencyMS == nil || *first.P95LatencyMS != 2460 {
		t.Fatalf("first p95 latency = %v, want 2460", first.P95LatencyMS)
	}

	second := overview.Trend[1]
	if second.RequestCount != 1 || second.SuccessCount != 0 || second.FailedCount != 1 {
		t.Fatalf("second counts = requests:%d success:%d failed:%d, want 1/0/1", second.RequestCount, second.SuccessCount, second.FailedCount)
	}
}

func TestUsageObservabilityOverviewFillsEmptyTrendBuckets(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()

	seedUsageObservabilityRecord(t, ctx, repo)
	from := time.Date(2026, time.June, 10, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, time.June, 12, 23, 59, 59, 0, time.UTC)
	overview, errOverview := repo.UsageObservabilityOverview(ctx, UsageObservabilityOverviewQuery{
		From:     &from,
		To:       &to,
		Interval: "day",
		Timezone: "UTC",
	})
	if errOverview != nil {
		t.Fatalf("UsageObservabilityOverview() error = %v", errOverview)
	}
	if len(overview.Trend) != 3 {
		t.Fatalf("trend point count = %d, want 3", len(overview.Trend))
	}
	if overview.Trend[0].RequestCount != 1 {
		t.Fatalf("first request count = %d, want 1", overview.Trend[0].RequestCount)
	}
	for index := 1; index < len(overview.Trend); index++ {
		if overview.Trend[index].RequestCount != 0 {
			t.Fatalf("trend[%d] request count = %d, want 0", index, overview.Trend[index].RequestCount)
		}
		if overview.Activity[index].Status != "empty" {
			t.Fatalf("activity[%d] status = %q, want empty", index, overview.Activity[index].Status)
		}
	}
}

func TestUsageObservabilityOverviewUsesHalfOpenEndBoundary(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()

	seedUsageObservabilityRecord(t, ctx, repo)
	from := time.Date(2026, time.June, 10, 1, 0, 0, 0, time.UTC)
	to := time.Date(2026, time.June, 10, 1, 2, 3, 0, time.UTC)
	overview, errOverview := repo.UsageObservabilityOverview(ctx, UsageObservabilityOverviewQuery{
		From:     &from,
		To:       &to,
		Interval: "hour",
		Timezone: "UTC",
	})
	if errOverview != nil {
		t.Fatalf("UsageObservabilityOverview() error = %v", errOverview)
	}
	if overview.Totals.RequestCount != 0 {
		t.Fatalf("request count = %d, want 0 for record exactly at exclusive to", overview.Totals.RequestCount)
	}
	if len(overview.Trend) != 1 || overview.Trend[0].RequestCount != 0 {
		t.Fatalf("trend = %+v, want one empty partial bucket", overview.Trend)
	}
	if len(overview.Activity) != 1 || overview.Activity[0].Status != "empty" {
		t.Fatalf("activity = %+v, want one empty bucket", overview.Activity)
	}
}

func TestUsageObservabilityOverviewRejectsTooManyExplicitTrendBuckets(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()

	from := time.Date(2026, time.June, 1, 0, 0, 0, 0, time.UTC)
	to := from.Add(8 * 24 * time.Hour)
	_, errOverview := repo.UsageObservabilityOverview(ctx, UsageObservabilityOverviewQuery{
		From:     &from,
		To:       &to,
		Interval: "minute",
		Timezone: "UTC",
	})
	if !errors.Is(errOverview, ErrUsageObservabilityTooManyTrendBuckets) {
		t.Fatalf("UsageObservabilityOverview() error = %v, want bucket limit error", errOverview)
	}
}

func TestUsageObservabilityOverviewAutoPromotesIntervalForBucketLimit(t *testing.T) {
	t.Parallel()

	from := time.Date(2000, time.January, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2040, time.January, 1, 0, 0, 0, 0, time.UTC)
	interval, errInterval := usageObservabilityOverviewIntervalForBounds("auto", time.UTC, &from, &to, usageObservabilityOverviewBounds{})
	if errInterval != nil {
		t.Fatalf("usageObservabilityOverviewIntervalForBounds() error = %v", errInterval)
	}
	if interval != "week" {
		t.Fatalf("interval = %q, want week", interval)
	}
}

func TestUsageObservabilityOverviewFillsEmptyRangeAcrossDST(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()

	from := time.Date(2026, time.March, 7, 12, 0, 0, 0, time.UTC)
	to := time.Date(2026, time.March, 9, 12, 0, 0, 0, time.UTC)
	overview, errOverview := repo.UsageObservabilityOverview(ctx, UsageObservabilityOverviewQuery{
		From:     &from,
		To:       &to,
		Interval: "day",
		Timezone: "America/New_York",
	})
	if errOverview != nil {
		t.Fatalf("UsageObservabilityOverview() error = %v", errOverview)
	}
	wantStarts := []time.Time{
		time.Date(2026, time.March, 7, 5, 0, 0, 0, time.UTC),
		time.Date(2026, time.March, 8, 5, 0, 0, 0, time.UTC),
		time.Date(2026, time.March, 9, 4, 0, 0, 0, time.UTC),
	}
	if len(overview.Trend) != len(wantStarts) {
		t.Fatalf("trend point count = %d, want %d", len(overview.Trend), len(wantStarts))
	}
	for index, wantStart := range wantStarts {
		if !overview.Trend[index].BucketStart.Equal(wantStart) {
			t.Fatalf("trend[%d] bucket start = %v, want %v", index, overview.Trend[index].BucketStart, wantStart)
		}
		if overview.Activity[index].RequestCount != 0 || overview.Activity[index].Status != "empty" {
			t.Fatalf("activity[%d] = %+v, want empty zero-request bucket", index, overview.Activity[index])
		}
	}
}

func TestUsageObservabilityHealthStatusContract(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		errorRate    float64
		requestCount int64
		want         string
	}{
		{name: "empty", errorRate: 0, requestCount: 0, want: "empty"},
		{name: "healthy", errorRate: 0.049, requestCount: 10, want: "healthy"},
		{name: "degraded", errorRate: 0.05, requestCount: 10, want: "degraded"},
		{name: "unavailable", errorRate: 0.50, requestCount: 10, want: "unavailable"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := usageObservabilityHealthStatus(test.errorRate, test.requestCount); got != test.want {
				t.Fatalf("usageObservabilityHealthStatus() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestUsageObservabilityOverviewSQLiteTrendNamedTimezoneDST(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()

	seedUsageObservabilityRecord(t, ctx, repo)
	payload := `{"timestamp":"2026-03-08T07:30:00Z","provider":"openai","model":"gpt-4.1-mini","api_key":"client-key-secret-1234","request_id":"req-obs-dst-day","endpoint":"/v1/chat/completions","executor_type":"CodexWebsocketsExecutor","auth_index":"auth-observability","auth_type":"oauth","latency_ms":500,"tokens":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}`
	if _, errUsage := repo.AppendUsage(ctx, payload, "192.0.2.10"); errUsage != nil {
		t.Fatalf("AppendUsage(dst) error = %v", errUsage)
	}

	from := time.Date(2026, time.March, 8, 7, 0, 0, 0, time.UTC)
	to := time.Date(2026, time.March, 8, 8, 0, 0, 0, time.UTC)
	overview, errOverview := repo.UsageObservabilityOverview(ctx, UsageObservabilityOverviewQuery{
		From:     &from,
		To:       &to,
		Interval: "day",
		Timezone: "America/New_York",
	})
	if errOverview != nil {
		t.Fatalf("UsageObservabilityOverview(dst) error = %v", errOverview)
	}
	if len(overview.Trend) != 1 {
		t.Fatalf("trend point count = %d, want 1", len(overview.Trend))
	}
	wantBucketStart := time.Date(2026, time.March, 8, 5, 0, 0, 0, time.UTC)
	if !overview.Trend[0].BucketStart.Equal(wantBucketStart) {
		t.Fatalf("bucket start = %v, want %v", overview.Trend[0].BucketStart, wantBucketStart)
	}
	if overview.Trend[0].RequestCount != 1 {
		t.Fatalf("request count = %d, want 1", overview.Trend[0].RequestCount)
	}
}

func TestListUsageObservabilityRecordsResolvesCredentialByRuntimeIndex(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()

	username := "usage-user"
	credits := 100.0
	user, errCreateUser := repo.CreateUser(ctx, UserUpdate{Username: &username, Credits: &credits})
	if errCreateUser != nil {
		t.Fatalf("CreateUser() error = %v", errCreateUser)
	}
	clientKey := "client-key-secret-1234"
	if _, errCreateKey := repo.CreateAPIKeyForUser(ctx, user.ID, APIKeyUserUpdate{APIKey: &clientKey}); errCreateKey != nil {
		t.Fatalf("CreateAPIKeyForUser() error = %v", errCreateKey)
	}
	auth := &coreauth.Auth{
		ID:        "auth-runtime-uuid",
		Index:     "auth-runtime-uuid",
		Provider:  "codex",
		Label:     "Runtime Index OAuth",
		Status:    coreauth.StatusActive,
		CreatedAt: time.Date(2026, time.June, 10, 1, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, time.June, 10, 1, 0, 0, 0, time.UTC),
	}
	authJSON, errMarshal := json.Marshal(auth)
	if errMarshal != nil {
		t.Fatalf("marshal auth: %v", errMarshal)
	}
	db, errDB := repo.database()
	if errDB != nil {
		t.Fatalf("database() error = %v", errDB)
	}
	authRecord := &AuthRecord{
		UUID:      "auth-runtime-uuid",
		AuthJSON:  JSONB(authJSON),
		Version:   1,
		ID:        "auth-runtime-uuid",
		Index:     "runtime-index-1",
		Provider:  "codex",
		Label:     "Runtime Index OAuth",
		Status:    coreauth.StatusActive,
		CreatedAt: time.Date(2026, time.June, 10, 1, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, time.June, 10, 1, 0, 0, 0, time.UTC),
	}
	if errCreateAuth := db.WithContext(ctx).Create(authRecord).Error; errCreateAuth != nil {
		t.Fatalf("Create(auth) error = %v", errCreateAuth)
	}
	if _, errCreatePrice := repo.CreateBillingModelPrice(ctx, BillingModelPriceUpdate{Provider: "openai", Model: "gpt-4.1-mini", RequestPrice: 2, Enabled: true}); errCreatePrice != nil {
		t.Fatalf("CreateBillingModelPrice() error = %v", errCreatePrice)
	}
	payload := `{"timestamp":"2026-06-10T01:02:03Z","provider":"openai","model":"gpt-4.1-mini","api_key":"client-key-secret-1234","request_id":"req-runtime-index","endpoint":"/v1/chat/completions","executor_type":"CodexWebsocketsExecutor","auth_index":"runtime-index-1","auth_type":"oauth","latency_ms":1460,"tokens":{"input_tokens":100,"output_tokens":50,"total_tokens":150}}`
	if _, errUsage := repo.AppendUsage(ctx, payload, "192.0.2.10"); errUsage != nil {
		t.Fatalf("AppendUsage() error = %v", errUsage)
	}

	result, errRecords := repo.ListUsageObservabilityRecords(ctx, UsageObservabilityRecordQuery{Limit: 10, Sort: "timestamp_desc"})
	if errRecords != nil {
		t.Fatalf("ListUsageObservabilityRecords() error = %v", errRecords)
	}
	if len(result.Records) != 1 {
		t.Fatalf("record count = %d, want 1", len(result.Records))
	}
	record := result.Records[0]
	if record.Credential.CredentialID != "auth-runtime-uuid" {
		t.Fatalf("credential id = %q, want auth-runtime-uuid", record.Credential.CredentialID)
	}
	if record.Credential.AuthIndex != "runtime-index-1" {
		t.Fatalf("credential auth index = %q, want runtime-index-1", record.Credential.AuthIndex)
	}
	if record.Credential.Label != "Runtime Index OAuth" {
		t.Fatalf("credential label = %q, want Runtime Index OAuth", record.Credential.Label)
	}
}

func seedUsageObservabilityRecord(t *testing.T, ctx context.Context, repo *Repository) {
	t.Helper()

	username := "usage-user"
	credits := 100.0
	user, errCreateUser := repo.CreateUser(ctx, UserUpdate{Username: &username, Credits: &credits})
	if errCreateUser != nil {
		t.Fatalf("CreateUser() error = %v", errCreateUser)
	}
	clientKey := "client-key-secret-1234"
	if _, errCreateKey := repo.CreateAPIKeyForUser(ctx, user.ID, APIKeyUserUpdate{APIKey: &clientKey}); errCreateKey != nil {
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
	if _, errAuth := repo.UpsertAuth(ctx, auth, "test"); errAuth != nil {
		t.Fatalf("UpsertAuth() error = %v", errAuth)
	}
	if _, errCreatePrice := repo.CreateBillingModelPrice(ctx, BillingModelPriceUpdate{Provider: "openai", Model: "gpt-4.1-mini", RequestPrice: 2, Enabled: true}); errCreatePrice != nil {
		t.Fatalf("CreateBillingModelPrice() error = %v", errCreatePrice)
	}
	payload := `{"timestamp":"2026-06-10T01:02:03Z","provider":"openai","model":"gpt-4.1-mini","api_key":"client-key-secret-1234","request_id":"req-obs-1","endpoint":"/v1/chat/completions","executor_type":"CodexWebsocketsExecutor","auth_index":"auth-observability","auth_type":"oauth","latency_ms":1460,"ttft_ms":333,"tokens":{"input_tokens":100,"output_tokens":50,"total_tokens":150}}`
	if _, errUsage := repo.AppendUsage(ctx, payload, "192.0.2.10"); errUsage != nil {
		t.Fatalf("AppendUsage() error = %v", errUsage)
	}
}
