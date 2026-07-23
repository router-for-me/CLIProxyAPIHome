package cluster

import (
	"context"
	"encoding/json"
	"math"
	"path/filepath"
	"testing"
	"time"
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
	if settings.ServiceTierSource != BillingServiceTierSourceRequest {
		t.Fatalf("service tier source = %q, want request", settings.ServiceTierSource)
	}
	if settings.ReportTimezone != BillingReportTimezoneUTC {
		t.Fatalf("report timezone = %q, want UTC", settings.ReportTimezone)
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
