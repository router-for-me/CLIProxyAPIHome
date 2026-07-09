package cluster

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
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

func TestMigrateUsageDerivedColumnsBackfillsStructuredMetadata(t *testing.T) {
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

	if errMigrate := migrateUsageDerivedColumns(db); errMigrate != nil {
		t.Fatalf("migrateUsageDerivedColumns() error = %v", errMigrate)
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
