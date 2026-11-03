package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func TestBillingMigrationPreservesLegacyPriceAsWildcardBaseBand(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, errOpen := OpenSQLite(ctx, filepath.Join(t.TempDir(), "legacy.db"))
	if errOpen != nil {
		t.Fatalf("OpenSQLite() error = %v", errOpen)
	}
	sqlDB, _ := db.DB()
	defer func() { _ = sqlDB.Close() }()
	if errCreate := db.Exec(`CREATE TABLE billing_model_price (
		id text PRIMARY KEY, provider text NOT NULL, model text NOT NULL,
		input_price_per_million real NOT NULL DEFAULT 0, output_price_per_million real NOT NULL DEFAULT 0,
		cache_read_price_per_million real NOT NULL DEFAULT 0, cache_write_price_per_million real NOT NULL DEFAULT 0,
		request_price real NOT NULL DEFAULT 0, source text NOT NULL DEFAULT 'manual', enabled numeric NOT NULL,
		note text, created_at datetime, updated_at datetime, deleted_at datetime
	)`).Error; errCreate != nil {
		t.Fatalf("create legacy table: %v", errCreate)
	}
	if errInsert := db.Exec(`INSERT INTO billing_model_price (id, provider, model, input_price_per_million, enabled) VALUES ('legacy', 'openai', 'gpt-5.5', 2, TRUE)`).Error; errInsert != nil {
		t.Fatalf("insert legacy row: %v", errInsert)
	}
	if errMigrate := AutoMigrate(db); errMigrate != nil {
		t.Fatalf("AutoMigrate() error = %v", errMigrate)
	}
	var record BillingModelPriceRecord
	if errFirst := db.First(&record, "id = ?", "legacy").Error; errFirst != nil {
		t.Fatalf("load migrated row: %v", errFirst)
	}
	if record.ServiceTier != BillingServiceTierWildcard || record.MinInputTokens != 0 || record.InputPricePerMillion != 2 || record.Revision != 1 {
		t.Fatalf("migrated record = %+v", record)
	}
}

func TestUsageRecordFromPayloadAcceptsLegacyRequestTier(t *testing.T) {
	t.Parallel()

	record, errRecord := UsageRecordFromPayload(`{"timestamp":"2026-07-11T00:00:00Z","request_service_tier":"priority","response_service_tier":"default","tokens":{"input_tokens":1}}`, "")
	if errRecord != nil {
		t.Fatalf("UsageRecordFromPayload() error = %v", errRecord)
	}
	if record.ServiceTier != "priority" || record.ResponseServiceTier != "default" {
		t.Fatalf("tiers = request:%q response:%q", record.ServiceTier, record.ResponseServiceTier)
	}
}

func TestMigrateUsageServiceTiersPromotesLegacyRequestTier(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, errOpen := OpenSQLite(ctx, filepath.Join(t.TempDir(), "legacy-tier.db"))
	if errOpen != nil {
		t.Fatalf("OpenSQLite() error = %v", errOpen)
	}
	sqlDB, _ := db.DB()
	defer func() { _ = sqlDB.Close() }()
	if errMigrate := AutoMigrate(db); errMigrate != nil {
		t.Fatalf("AutoMigrate() error = %v", errMigrate)
	}
	if errAlter := db.Exec(`ALTER TABLE "usage" ADD COLUMN request_service_tier TEXT`).Error; errAlter != nil {
		t.Fatalf("add legacy request tier column: %v", errAlter)
	}
	now := time.Now().UTC()
	legacy := &UsageRecord{Timestamp: now, PayloadJSON: JSONB(`{}`), CreatedAt: now}
	canonical := &UsageRecord{Timestamp: now, ServiceTier: "default", PayloadJSON: JSONB(`{}`), CreatedAt: now}
	if errCreate := db.Create(legacy).Error; errCreate != nil {
		t.Fatalf("create legacy usage: %v", errCreate)
	}
	if errCreate := db.Create(canonical).Error; errCreate != nil {
		t.Fatalf("create canonical usage: %v", errCreate)
	}
	if errUpdate := db.Exec(`UPDATE "usage" SET request_service_tier = ? WHERE id IN (?, ?)`, "priority", legacy.ID, canonical.ID).Error; errUpdate != nil {
		t.Fatalf("set legacy request tiers: %v", errUpdate)
	}
	if errMigrate := migrateUsageServiceTiers(db); errMigrate != nil {
		t.Fatalf("migrateUsageServiceTiers() error = %v", errMigrate)
	}
	var records []UsageRecord
	if errFind := db.Order("id ASC").Find(&records).Error; errFind != nil {
		t.Fatalf("load migrated usage: %v", errFind)
	}
	if len(records) != 2 || records[0].ServiceTier != "priority" || records[1].ServiceTier != "default" {
		t.Fatalf("migrated records = %+v", records)
	}
}

func TestUsageRecordFromPayloadDefaultsMissingTierToAuto(t *testing.T) {
	t.Parallel()

	record, errRecord := UsageRecordFromPayload(`{"timestamp":"2026-07-13T00:00:00Z","tokens":{"input_tokens":1}}`, "")
	if errRecord != nil {
		t.Fatalf("UsageRecordFromPayload() error = %v", errRecord)
	}
	if record.ServiceTier != "auto" {
		t.Fatalf("service tier = %q, want auto", record.ServiceTier)
	}
}

func TestUsageRecordFromPayloadKeepsServiceTierPrimaryOverLegacyRequestTier(t *testing.T) {
	t.Parallel()

	record, errRecord := UsageRecordFromPayload(`{"timestamp":"2026-07-13T00:00:00Z","service_tier":"default","request_service_tier":"priority","tokens":{"input_tokens":1}}`, "")
	if errRecord != nil {
		t.Fatalf("UsageRecordFromPayload() error = %v", errRecord)
	}
	if record.ServiceTier != "default" {
		t.Fatalf("service tier = %q, want default", record.ServiceTier)
	}
}

func TestBillingPriceMatchingDefaultsToServiceTier(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	for _, update := range []BillingModelPriceUpdate{
		{Provider: "openai", Model: "gpt-5.5", ServiceTier: "standard", RequestPrice: 1, Enabled: true},
		{Provider: "openai", Model: "gpt-5.5", ServiceTier: "priority", RequestPrice: 2, Enabled: true},
	} {
		if _, errCreate := repo.CreateBillingModelPrice(ctx, update); errCreate != nil {
			t.Fatalf("CreateBillingModelPrice() error = %v", errCreate)
		}
	}
	db, errDB := repo.database()
	if errDB != nil {
		t.Fatalf("database() error = %v", errDB)
	}
	// Default service_tier_source is request/service_tier, even when upstream
	// returns a different response_service_tier.
	_, snapshot, errSnapshot := billingPriceSnapshotForUsage(ctx, db, &UsageRecord{
		Provider:            "openai",
		Model:               "gpt-5.5",
		ServiceTier:         "priority",
		ResponseServiceTier: "default",
	})
	if errSnapshot != nil {
		t.Fatalf("billingPriceSnapshotForUsage() error = %v", errSnapshot)
	}
	if snapshot.RequestPrice != 2 || snapshot.EffectiveServiceTier != "priority" || snapshot.ServiceTierSource != BillingServiceTierSourceRequest {
		t.Fatalf("snapshot = %+v, want request service_tier priority rule", snapshot)
	}
}

func TestBillingPriceMatchingUsesContextBandAndExactTier(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()

	for _, update := range []BillingModelPriceUpdate{
		{Provider: "openai", Model: "gpt-5.5", ServiceTier: "*", MinInputTokens: 0, InputPricePerMillion: 1, Enabled: true},
		{Provider: "openai", Model: "gpt-5.5", ServiceTier: "standard", MinInputTokens: 0, InputPricePerMillion: 2, Enabled: true},
		{Provider: "openai", Model: "gpt-5.5", ServiceTier: "standard", MinInputTokens: 272001, InputPricePerMillion: 4, Enabled: true},
	} {
		if _, errCreate := repo.CreateBillingModelPrice(ctx, update); errCreate != nil {
			t.Fatalf("CreateBillingModelPrice(%+v) error = %v", update, errCreate)
		}
	}
	db, errDB := repo.database()
	if errDB != nil {
		t.Fatalf("database() error = %v", errDB)
	}

	for _, tt := range []struct {
		name      string
		input     int64
		tier      string
		wantPrice float64
		wantMin   int64
		wantTier  string
	}{
		{name: "default uses standard", input: 272000, tier: "default", wantPrice: 2, wantMin: 0, wantTier: "standard"},
		{name: "standard selects long band", input: 272001, tier: "standard", wantPrice: 4, wantMin: 272001, wantTier: "standard"},
		{name: "auto uses standard", input: 272001, tier: "auto", wantPrice: 4, wantMin: 272001, wantTier: "standard"},
		{name: "wildcard fallback", input: 272001, tier: "flex", wantPrice: 1, wantMin: 0, wantTier: "*"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			usage := &UsageRecord{Provider: "openai", Model: "gpt-5.5", InputTokens: tt.input, ServiceTier: tt.tier}
			_, snapshot, errSnapshot := billingPriceSnapshotForUsage(ctx, db, usage)
			if errSnapshot != nil {
				t.Fatalf("billingPriceSnapshotForUsage() error = %v", errSnapshot)
			}
			if snapshot.InputPricePerMillion != tt.wantPrice || snapshot.MinInputTokens != tt.wantMin || snapshot.MatchedServiceTier != tt.wantTier {
				t.Fatalf("snapshot = %+v", snapshot)
			}
		})
	}
}

func TestBillingResponseTierModeFallsBackAudibly(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	if _, errCreate := repo.CreateBillingModelPrice(ctx, BillingModelPriceUpdate{Provider: "openai", Model: "gpt-5.5", ServiceTier: "priority", Enabled: true}); errCreate != nil {
		t.Fatalf("CreateBillingModelPrice() error = %v", errCreate)
	}
	if _, errPatch := repo.UpdateBillingSettings(ctx, BillingSettingsPatch{ServiceTierSource: stringPtr(BillingServiceTierSourceResponse)}); errPatch != nil {
		t.Fatalf("UpdateBillingSettings() error = %v", errPatch)
	}
	db, _ := repo.database()
	_, snapshot, errSnapshot := billingPriceSnapshotForUsage(ctx, db, &UsageRecord{Provider: "openai", Model: "gpt-5.5", ServiceTier: "priority"})
	if errSnapshot != nil {
		t.Fatalf("billingPriceSnapshotForUsage() error = %v", errSnapshot)
	}
	if snapshot.ServiceTierSource != BillingServiceTierSourceResponse || snapshot.EffectiveServiceTier != "priority" || !snapshot.ResponseTierFallback {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	if snapshot.RequestedServiceTier != "priority" || snapshot.ResponseServiceTier != "" {
		t.Fatalf("snapshot audit tiers = %+v", snapshot)
	}
}

func TestBillingResponseTierModeFallsBackToStandardRuleForAuto(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	if _, errCreate := repo.CreateBillingModelPrice(ctx, BillingModelPriceUpdate{Provider: "openai", Model: "gpt-5.5", ServiceTier: "standard", InputPricePerMillion: 3, Enabled: true}); errCreate != nil {
		t.Fatalf("CreateBillingModelPrice() error = %v", errCreate)
	}
	if _, errPatch := repo.UpdateBillingSettings(ctx, BillingSettingsPatch{ServiceTierSource: stringPtr(BillingServiceTierSourceResponse)}); errPatch != nil {
		t.Fatalf("UpdateBillingSettings() error = %v", errPatch)
	}
	db, _ := repo.database()
	_, snapshot, errSnapshot := billingPriceSnapshotForUsage(ctx, db, &UsageRecord{Provider: "openai", Model: "gpt-5.5", ServiceTier: "auto"})
	if errSnapshot != nil {
		t.Fatalf("billingPriceSnapshotForUsage() error = %v", errSnapshot)
	}
	if snapshot.InputPricePerMillion != 3 || snapshot.EffectiveServiceTier != "standard" || !snapshot.ResponseTierFallback {
		t.Fatalf("snapshot = %+v", snapshot)
	}
}

func TestBillingResponseTierModeUsesReportedResponseTier(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	for _, tier := range []string{"priority", "standard"} {
		if _, errCreate := repo.CreateBillingModelPrice(ctx, BillingModelPriceUpdate{Provider: "openai", Model: "gpt-5.5", ServiceTier: tier, RequestPrice: map[string]float64{"priority": 2, "standard": 1}[tier], Enabled: true}); errCreate != nil {
			t.Fatalf("CreateBillingModelPrice(%s) error = %v", tier, errCreate)
		}
	}
	if _, errPatch := repo.UpdateBillingSettings(ctx, BillingSettingsPatch{ServiceTierSource: stringPtr(BillingServiceTierSourceResponse)}); errPatch != nil {
		t.Fatalf("UpdateBillingSettings() error = %v", errPatch)
	}
	db, _ := repo.database()
	_, snapshot, errSnapshot := billingPriceSnapshotForUsage(ctx, db, &UsageRecord{Provider: "openai", Model: "gpt-5.5", ServiceTier: "priority", ResponseServiceTier: "default"})
	if errSnapshot != nil {
		t.Fatalf("billingPriceSnapshotForUsage() error = %v", errSnapshot)
	}
	if snapshot.RequestPrice != 1 || snapshot.EffectiveServiceTier != "standard" || snapshot.ResponseTierFallback {
		t.Fatalf("snapshot = %+v", snapshot)
	}
}

func TestBillingChargeAmountFallsBackForNonCanonicalLegacyUsage(t *testing.T) {
	t.Parallel()

	snapshot := BillingPriceSnapshot{InputPricePerMillion: 10, OutputPricePerMillion: 20}
	for _, usage := range []*UsageRecord{
		{Provider: "plugin-provider", InputTokens: 100, OutputTokens: 50, TotalTokens: 150},
		{Provider: "openai", InputTokens: 100, OutputTokens: 50, TotalTokens: 200},
	} {
		if got, want := billingChargeAmount(usage, snapshot), 0.002; math.Abs(got-want) > 1e-12 {
			t.Fatalf("billingChargeAmount(%s) = %.9f, want %.9f", usage.Provider, got, want)
		}
	}
}

func TestBillingChargeAmountFallbackPreservesLegacyCacheBilling(t *testing.T) {
	t.Parallel()

	openAI := &UsageRecord{
		Provider:               "openai",
		InputTokens:            100,
		OutputTokens:           10,
		CacheReadTokens:        30,
		CacheReadTokensPresent: true,
		CacheCreationTokens:    20,
		TotalTokens:            999,
	}
	snapshot := BillingPriceSnapshot{
		InputPricePerMillion:      10,
		OutputPricePerMillion:     20,
		CacheReadPricePerMillion:  2,
		CacheWritePricePerMillion: 12.5,
	}
	if got, want := billingChargeAmount(openAI, snapshot), 0.00101; math.Abs(got-want) > 1e-12 {
		t.Fatalf("billingChargeAmount(OpenAI fallback) = %.9f, want %.9f", got, want)
	}
	snapshot.CacheWritePricePerMillion = 0
	if got, want := billingChargeAmount(openAI, snapshot), 0.00096; math.Abs(got-want) > 1e-12 {
		t.Fatalf("billingChargeAmount(OpenAI fallback without write price) = %.9f, want %.9f", got, want)
	}
	if got, want := billingCacheTokens(openAI), int64(50); got != want {
		t.Fatalf("billingCacheTokens(OpenAI fallback) = %d, want %d", got, want)
	}

	anthropic := *openAI
	anthropic.Provider = "anthropic"
	snapshot.CacheWritePricePerMillion = 12.5
	if got, want := billingChargeAmount(&anthropic, snapshot), 0.00151; math.Abs(got-want) > 1e-12 {
		t.Fatalf("billingChargeAmount(Anthropic fallback) = %.9f, want %.9f", got, want)
	}
	if got, want := billingCacheTokens(&anthropic), int64(50); got != want {
		t.Fatalf("billingCacheTokens(Anthropic fallback) = %d, want %d", got, want)
	}
}

func TestBillingChargeAmountSeparatesOpenAICacheReadAndWrite(t *testing.T) {
	t.Parallel()

	snapshot := BillingPriceSnapshot{InputPricePerMillion: 10, CacheReadPricePerMillion: 2, CacheWritePricePerMillion: 12.5}
	usage := &UsageRecord{Provider: "openai", InputTokens: 100, CachedTokens: 30, CacheCreationTokens: 20}
	if got, want := billingChargeAmount(usage, snapshot), 0.00081; math.Abs(got-want) > 1e-12 {
		t.Fatalf("billingChargeAmount() = %.9f, want %.9f", got, want)
	}

	snapshot.CacheWritePricePerMillion = 0
	if got, want := billingChargeAmount(usage, snapshot), 0.00076; math.Abs(got-want) > 1e-12 {
		t.Fatalf("billingChargeAmount(no cache-write fee) = %.9f, want %.9f", got, want)
	}
}

func TestBillingChargeAmountSeparatesOpenAICacheWriteWithoutCacheRead(t *testing.T) {
	t.Parallel()

	usage := &UsageRecord{Provider: "openai", InputTokens: 100, CacheCreationTokens: 20}
	snapshot := BillingPriceSnapshot{InputPricePerMillion: 10, CacheWritePricePerMillion: 12.5}
	if got, want := billingChargeAmount(usage, snapshot), 0.00105; math.Abs(got-want) > 1e-12 {
		t.Fatalf("billingChargeAmount() = %.9f, want %.9f", got, want)
	}
}

func TestBillingChargeAmountDoesNotTreatClaudeCreationAsCacheRead(t *testing.T) {
	t.Parallel()

	usage := &UsageRecord{Provider: "claude", InputTokens: 100, CachedTokens: 20, CacheCreationTokens: 20}
	snapshot := BillingPriceSnapshot{InputPricePerMillion: 10, CacheReadPricePerMillion: 2, CacheWritePricePerMillion: 12.5}
	if got, want := billingChargeAmount(usage, snapshot), 0.00125; math.Abs(got-want) > 1e-12 {
		t.Fatalf("billingChargeAmount() = %.9f, want %.9f", got, want)
	}
}

func TestBillingCacheTokensIncludesOpenAICacheReadAndWrite(t *testing.T) {
	t.Parallel()

	usage := &UsageRecord{
		Provider:            "openai",
		InputTokens:         50,
		CachedTokens:        30,
		CacheCreationTokens: 20,
		TotalTokens:         50,
	}
	if got, want := billingCacheTokens(usage), int64(50); got != want {
		t.Fatalf("billingCacheTokens() = %d, want %d", got, want)
	}
}

func TestBillingCacheTokensDoesNotDoubleCountAnthropicCreation(t *testing.T) {
	t.Parallel()

	usage := &UsageRecord{Provider: "openai", ExecutorType: "AnthropicExecutor", CachedTokens: 20, CacheCreationTokens: 20}
	if got, want := billingCacheTokens(usage), int64(20); got != want {
		t.Fatalf("billingCacheTokens() = %d, want %d", got, want)
	}
}

func TestBillingOpenAICompatibleProviderNameDoesNotSelectClaudeBuckets(t *testing.T) {
	t.Parallel()

	usage := &UsageRecord{
		Provider:            "openai-compatible-anthropic",
		ExecutorType:        "OpenAICompatExecutor",
		InputTokens:         100,
		CachedTokens:        30,
		CacheCreationTokens: 20,
	}
	snapshot := BillingPriceSnapshot{InputPricePerMillion: 10, CacheReadPricePerMillion: 2, CacheWritePricePerMillion: 12.5}
	if got, want := billingChargeAmount(usage, snapshot), 0.00081; math.Abs(got-want) > 1e-12 {
		t.Fatalf("billingChargeAmount() = %.9f, want %.9f", got, want)
	}
	if got, want := billingCacheTokens(usage), int64(50); got != want {
		t.Fatalf("billingCacheTokens() = %d, want %d", got, want)
	}
}

func TestBillingSettingsDefaultToRequest(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	settings, errSettings := repo.GetBillingSettings(ctx)
	if errSettings != nil {
		t.Fatalf("GetBillingSettings() error = %v", errSettings)
	}
	if settings.ModelSource != BillingModelSourceRequest {
		t.Fatalf("model source = %q, want request", settings.ModelSource)
	}
	if settings.ServiceTierSource != BillingServiceTierSourceRequest {
		t.Fatalf("service tier source = %q, want request", settings.ServiceTierSource)
	}
	if settings.ReportTimezone != BillingReportTimezoneUTC {
		t.Fatalf("report timezone = %q, want UTC", settings.ReportTimezone)
	}

	// Verify model_source="request" (default) vs model_source="response".
	if _, errCreate := repo.CreateBillingModelPrice(ctx, BillingModelPriceUpdate{Provider: "antigravity", Model: "gemini-4-argon-high", InputPricePerMillion: 5, Enabled: true}); errCreate != nil {
		t.Fatal(errCreate)
	}
	if _, errCreate := repo.CreateBillingModelPrice(ctx, BillingModelPriceUpdate{Provider: "antigravity", Model: "gemini-4-argon", InputPricePerMillion: 2, Enabled: true}); errCreate != nil {
		t.Fatal(errCreate)
	}
	db, _ := repo.database()
	usage := &UsageRecord{
		Provider:    "antigravity",
		Model:       "gemini-4-argon-high",
		InputTokens: 100,
		PayloadJSON: JSONB(`{"model":"gemini-4-argon-high","response_model":"gemini-4-argon"}`),
	}
	_, reqSnap, errReq := billingPriceSnapshotForUsage(ctx, db, usage)
	if errReq != nil {
		t.Fatal(errReq)
	}
	if reqSnap.Model != "gemini-4-argon-high" || reqSnap.InputPricePerMillion != 5 || reqSnap.ModelSource != BillingModelSourceRequest || reqSnap.PriceModelSource != billingPriceModelSourceModel {
		t.Fatalf("request mode snapshot = %+v, want gemini-4-argon-high @ 5", reqSnap)
	}
	if _, errPatch := repo.UpdateBillingSettings(ctx, BillingSettingsPatch{ModelSource: stringPtr(BillingModelSourceResponse)}); errPatch != nil {
		t.Fatal(errPatch)
	}
	_, respSnap, errResp := billingPriceSnapshotForUsage(ctx, db, usage)
	if errResp != nil {
		t.Fatal(errResp)
	}
	if respSnap.Model != "gemini-4-argon" || respSnap.InputPricePerMillion != 2 || respSnap.ModelSource != BillingModelSourceResponse || respSnap.PriceModelSource != billingPriceModelSourceResponseModel {
		t.Fatalf("response mode snapshot = %+v, want gemini-4-argon @ 2", respSnap)
	}
}

func TestBillingMalformedSettingsFallBackAndCanBeRepaired(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	if _, errSet := repo.KVSet(ctx, billingSettingsKVKey, []byte("not-json"), 0, KVSetModeAlways); errSet != nil {
		t.Fatalf("KVSet() error = %v", errSet)
	}
	settings, errSettings := repo.GetBillingSettings(ctx)
	if errSettings != nil {
		t.Fatalf("GetBillingSettings() error = %v", errSettings)
	}
	if settings.ServiceTierSource != BillingServiceTierSourceRequest {
		t.Fatalf("service tier source = %q, want request", settings.ServiceTierSource)
	}
	if settings.ReportTimezone != BillingReportTimezoneUTC {
		t.Fatalf("report timezone = %q, want UTC", settings.ReportTimezone)
	}
	payload := `{"timestamp":"2026-07-11T00:00:00Z","provider":"openai","model":"gpt-test","tokens":{"input_tokens":1}}`
	if _, errUsage := repo.AppendUsage(ctx, payload, ""); errUsage != nil {
		t.Fatalf("AppendUsage() error = %v", errUsage)
	}
	reportTimezone := "America/Los_Angeles"
	settings, errSettings = repo.UpdateBillingSettings(ctx, BillingSettingsPatch{
		ServiceTierSource: stringPtr(BillingServiceTierSourceResponse),
		ReportTimezone:    &reportTimezone,
	})
	if errSettings != nil {
		t.Fatalf("UpdateBillingSettings() error = %v", errSettings)
	}
	if settings.ServiceTierSource != BillingServiceTierSourceResponse {
		t.Fatalf("repaired service tier source = %q, want response", settings.ServiceTierSource)
	}
	if settings.ReportTimezone != reportTimezone {
		t.Fatalf("repaired report timezone = %q, want %q", settings.ReportTimezone, reportTimezone)
	}
}

func TestBillingSnapshotJSONIncludesTierAndBandAudit(t *testing.T) {
	t.Parallel()

	raw, errMarshal := json.Marshal(BillingPriceSnapshot{MatchedServiceTier: "standard", MinInputTokens: 272001, EffectiveServiceTier: "standard"})
	if errMarshal != nil {
		t.Fatalf("json.Marshal() error = %v", errMarshal)
	}
	for _, field := range []string{"matched_service_tier", "min_input_tokens", "effective_service_tier"} {
		if !json.Valid(raw) || !containsJSONField(raw, field) {
			t.Fatalf("snapshot JSON %s missing %q", raw, field)
		}
	}
}

func containsJSONField(raw []byte, field string) bool {
	var value map[string]any
	_ = json.Unmarshal(raw, &value)
	_, ok := value[field]
	return ok
}

func TestBillingPriceMatchingUsesFullClaudeContextForBand(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	for _, update := range []BillingModelPriceUpdate{
		{Provider: "claude", Model: "claude-opus-5", ServiceTier: "*", MinInputTokens: 0, InputPricePerMillion: 5, Enabled: true},
		{Provider: "claude", Model: "claude-opus-5", ServiceTier: "*", MinInputTokens: 200001, InputPricePerMillion: 10, Enabled: true},
	} {
		if _, errCreate := repo.CreateBillingModelPrice(ctx, update); errCreate != nil {
			t.Fatalf("CreateBillingModelPrice(%+v) error = %v", update, errCreate)
		}
	}
	db, _ := repo.database()

	// Claude reports cache reads outside input_tokens; the band must use the full prompt.
	usage := &UsageRecord{Provider: "claude", ExecutorType: "ClaudeExecutor", Model: "claude-opus-5", InputTokens: 10000, CacheReadTokens: 240000, CacheReadTokensPresent: true, OutputTokens: 100}
	_, snapshot, errSnapshot := billingPriceSnapshotForUsage(ctx, db, usage)
	if errSnapshot != nil {
		t.Fatalf("billingPriceSnapshotForUsage() error = %v", errSnapshot)
	}
	if snapshot.MinInputTokens != 200001 || snapshot.InputPricePerMillion != 10 || snapshot.ContextInputTokens != 250000 {
		t.Fatalf("snapshot = %+v, want long-context band with context 250000", snapshot)
	}

	// OpenAI-style input already includes cache reads, so it must not be double counted.
	if _, errCreate := repo.CreateBillingModelPrice(ctx, BillingModelPriceUpdate{Provider: "codex", Model: "gpt-5.5", ServiceTier: "*", MinInputTokens: 272001, InputPricePerMillion: 9, Enabled: true}); errCreate != nil {
		t.Fatalf("CreateBillingModelPrice(codex long) error = %v", errCreate)
	}
	if _, errCreate := repo.CreateBillingModelPrice(ctx, BillingModelPriceUpdate{Provider: "codex", Model: "gpt-5.5", ServiceTier: "*", MinInputTokens: 0, InputPricePerMillion: 3, Enabled: true}); errCreate != nil {
		t.Fatalf("CreateBillingModelPrice(codex base) error = %v", errCreate)
	}
	codexUsage := &UsageRecord{Provider: "codex", ExecutorType: "CodexExecutor", Model: "gpt-5.5", InputTokens: 200000, CacheReadTokens: 150000, CacheReadTokensPresent: true, OutputTokens: 10}
	_, codexSnapshot, errCodex := billingPriceSnapshotForUsage(ctx, db, codexUsage)
	if errCodex != nil {
		t.Fatalf("billingPriceSnapshotForUsage(codex) error = %v", errCodex)
	}
	if codexSnapshot.MinInputTokens != 0 || codexSnapshot.ContextInputTokens != 200000 {
		t.Fatalf("codex snapshot = %+v, want base band with context 200000", codexSnapshot)
	}
}

func TestBillingPriceMatchingFallsBackToAlias(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	if _, errCreate := repo.CreateBillingModelPrice(ctx, BillingModelPriceUpdate{Provider: "antigravity", Model: "gemini-3.5-flash", InputPricePerMillion: 1, Enabled: true}); errCreate != nil {
		t.Fatalf("CreateBillingModelPrice(alias) error = %v", errCreate)
	}
	db, _ := repo.database()

	usage := &UsageRecord{Provider: "antigravity", Model: "gemini-3-flash-agent", Alias: "gemini-3.5-flash", InputTokens: 10}
	price, snapshot, errSnapshot := billingPriceSnapshotForUsage(ctx, db, usage)
	if errSnapshot != nil {
		t.Fatalf("billingPriceSnapshotForUsage() error = %v", errSnapshot)
	}
	if price == nil || snapshot.Model != "gemini-3.5-flash" || snapshot.PriceModelSource != billingPriceModelSourceAlias || snapshot.InputPricePerMillion != 1 {
		t.Fatalf("snapshot = %+v, want alias price", snapshot)
	}

	if _, errCreate := repo.CreateBillingModelPrice(ctx, BillingModelPriceUpdate{Provider: "antigravity", Model: "gemini-3-flash-agent", InputPricePerMillion: 2, Enabled: true}); errCreate != nil {
		t.Fatalf("CreateBillingModelPrice(model) error = %v", errCreate)
	}
	_, modelSnapshot, errModel := billingPriceSnapshotForUsage(ctx, db, usage)
	if errModel != nil {
		t.Fatalf("billingPriceSnapshotForUsage(model) error = %v", errModel)
	}
	if modelSnapshot.Model != "gemini-3-flash-agent" || modelSnapshot.PriceModelSource != billingPriceModelSourceModel || modelSnapshot.InputPricePerMillion != 2 {
		t.Fatalf("snapshot = %+v, want executed model price first", modelSnapshot)
	}
}

func TestBillingApplyUserCreditDeltaUsesAtomicUpdate(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	username := "atomic@example.com"
	credits := 10.0
	user, errCreateUser := repo.CreateUser(ctx, UserUpdate{Username: &username, Credits: &credits})
	if errCreateUser != nil {
		t.Fatalf("CreateUser() error = %v", errCreateUser)
	}
	db, _ := repo.database()

	expected := credits
	for i := 0; i < 20; i++ {
		var before, after float64
		errTx := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			var errApply error
			before, after, errApply = billingApplyUserCreditDeltaTx(ctx, tx, user.ID, -0.125)
			return errApply
		})
		if errTx != nil {
			t.Fatalf("billingApplyUserCreditDeltaTx(#%d) error = %v", i, errTx)
		}
		if before != expected || after != expected-0.125 {
			t.Fatalf("charge #%d balances = %v -> %v, want %v -> %v", i, before, after, expected, expected-0.125)
		}
		expected -= 0.125
	}
	stored := &UserRecord{}
	if errFirst := db.WithContext(ctx).Where("id = ?", user.ID).First(stored).Error; errFirst != nil {
		t.Fatalf("load user error = %v", errFirst)
	}
	if stored.Credits != 7.5 {
		t.Fatalf("stored credits = %v, want 7.5", stored.Credits)
	}
}

func TestBillingApplyUserCreditDeltaConcurrentPostgres(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("CLIPROXY_HOME_TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("CLIPROXY_HOME_TEST_POSTGRES_DSN is not configured")
	}
	ctx := context.Background()
	db, errOpen := gorm.Open(postgres.Open(dsn), databaseGORMConfig(defaultDatabaseSlowQueryThreshold))
	if errOpen != nil {
		t.Fatalf("open postgres: %v", errOpen)
	}
	sqlDB, errDB := db.DB()
	if errDB != nil {
		t.Fatalf("get postgres database: %v", errDB)
	}
	defer func() {
		if errClose := sqlDB.Close(); errClose != nil {
			t.Errorf("close postgres database: %v", errClose)
		}
	}()
	if errMigrate := AutoMigrateContext(ctx, db); errMigrate != nil {
		t.Fatalf("AutoMigrateContext() error = %v", errMigrate)
	}
	repo := NewRepository(db)
	username := fmt.Sprintf("atomic-%d@example.com", time.Now().UnixNano())
	credits := 100.0
	user, errCreateUser := repo.CreateUser(ctx, UserUpdate{Username: &username, Credits: &credits})
	if errCreateUser != nil {
		t.Fatalf("CreateUser() error = %v", errCreateUser)
	}
	defer func() {
		if errDelete := db.WithContext(ctx).Unscoped().Delete(&UserRecord{}, user.ID).Error; errDelete != nil {
			t.Errorf("delete test user: %v", errDelete)
		}
	}()

	const workers = 16
	const chargesPerWorker = 5
	errs := make(chan error, workers*chargesPerWorker)
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < chargesPerWorker; i++ {
				errs <- db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
					before, after, errApply := billingApplyUserCreditDeltaTx(ctx, tx, user.ID, -0.25)
					if errApply == nil && after != before-0.25 {
						return fmt.Errorf("balances = %v -> %v, want delta -0.25", before, after)
					}
					return errApply
				})
			}
		}()
	}
	wg.Wait()
	close(errs)
	for errCharge := range errs {
		if errCharge != nil {
			t.Fatalf("concurrent charge error = %v", errCharge)
		}
	}
	stored := &UserRecord{}
	if errFirst := db.WithContext(ctx).Where("id = ?", user.ID).First(stored).Error; errFirst != nil {
		t.Fatalf("load user error = %v", errFirst)
	}
	if want := credits - 0.25*workers*chargesPerWorker; stored.Credits != want {
		t.Fatalf("stored credits = %v, want %v", stored.Credits, want)
	}
}
