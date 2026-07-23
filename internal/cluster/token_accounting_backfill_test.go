package cluster

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestUsageTokenAccountingBackfillPreservesCanonicalPayload(t *testing.T) {
	ctx := context.Background()
	db, errOpen := OpenSQLite(ctx, filepath.Join(t.TempDir(), "home.db"))
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
	if errMigrate := AutoMigrate(db); errMigrate != nil {
		t.Fatalf("AutoMigrate() error = %v", errMigrate)
	}

	payload := `{"timestamp":"2026-07-23T01:00:00Z","provider":"plugin-provider","accounting_version":2,"token_breakdown":{"schema_version":2,"quality":"complete","total_tokens":150,"input":{"total_tokens":100,"uncached_tokens":60,"cache_read_tokens":30,"cache_write_tokens":10},"output":{"total_tokens":50,"non_reasoning_tokens":35,"reasoning_tokens":15},"unclassified_tokens":0}}`
	record := UsageRecord{
		Timestamp:    time.Date(2026, time.July, 23, 1, 0, 0, 0, time.UTC),
		Provider:     "plugin-provider",
		InputTokens:  100,
		OutputTokens: 50,
		TotalTokens:  150,
		PayloadJSON:  JSONB(payload),
		CreatedAt:    time.Now().UTC(),
	}
	if errCreate := db.Create(&record).Error; errCreate != nil {
		t.Fatalf("create mixed-version usage row: %v", errCreate)
	}
	result, errBackfill := runUsageTokenAccountingBackfillBatch(ctx, db, 10)
	if errBackfill != nil {
		t.Fatalf("backfill error = %v", errBackfill)
	}
	if result.Updated != 1 || !result.Done {
		t.Fatalf("backfill result = %+v", result)
	}

	var stored UsageRecord
	if errFirst := db.First(&stored, record.ID).Error; errFirst != nil {
		t.Fatalf("load backfilled row: %v", errFirst)
	}
	breakdown := usageTokenBreakdownFromRecord(&stored)
	want := UsageTokenBreakdown{
		SchemaVersion: UsageTokenAccountingSchemaVersion,
		Quality:       UsageTokenAccountingQualityComplete,
		TotalTokens:   150,
		Input: UsageTokenInputBreakdown{
			TotalTokens:      100,
			UncachedTokens:   60,
			CacheReadTokens:  30,
			CacheWriteTokens: 10,
		},
		Output: UsageTokenOutputBreakdown{
			TotalTokens:        50,
			NonReasoningTokens: 35,
			ReasoningTokens:    15,
		},
	}
	if breakdown != want {
		t.Fatalf("breakdown = %+v, want %+v", breakdown, want)
	}
}

func TestUsageTokenAccountingBackfillIsResumableAndCapabilityAware(t *testing.T) {
	ctx := context.Background()
	db, errOpen := OpenSQLite(ctx, filepath.Join(t.TempDir(), "home.db"))
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
	if errMigrate := AutoMigrate(db); errMigrate != nil {
		t.Fatalf("AutoMigrate() error = %v", errMigrate)
	}

	records := []UsageRecord{
		{
			Timestamp:              time.Date(2026, time.July, 23, 1, 0, 0, 0, time.UTC),
			Provider:               "openai",
			ExecutorType:           "OpenAICompatExecutor",
			InputTokens:            100,
			OutputTokens:           30,
			ReasoningTokens:        12,
			CachedTokens:           40,
			CacheReadTokens:        40,
			CacheReadTokensPresent: true,
			TotalTokens:            130,
			PayloadJSON:            JSONB(`{"timestamp":"2026-07-23T01:00:00Z"}`),
			CreatedAt:              time.Now().UTC(),
		},
		{
			Timestamp:    time.Date(2026, time.July, 23, 1, 1, 0, 0, time.UTC),
			Provider:     "unknown-provider",
			InputTokens:  9,
			OutputTokens: 1,
			TotalTokens:  10,
			PayloadJSON:  JSONB(`{"timestamp":"2026-07-23T01:01:00Z"}`),
			CreatedAt:    time.Now().UTC(),
		},
	}
	if errCreate := db.Create(&records).Error; errCreate != nil {
		t.Fatalf("create legacy usage rows: %v", errCreate)
	}
	repo := NewRepository(db)
	ready, errReady := repo.UsageTokenBreakdownV2Ready(ctx)
	if errReady != nil || ready {
		t.Fatalf("ready before backfill = %t, err=%v", ready, errReady)
	}

	first, errBackfill := runUsageTokenAccountingBackfillBatch(ctx, db, 1)
	if errBackfill != nil {
		t.Fatalf("first backfill error = %v", errBackfill)
	}
	if first.Scanned != 1 || first.Updated != 1 || first.Done {
		t.Fatalf("first backfill = %+v", first)
	}
	second, errBackfill := runUsageTokenAccountingBackfillBatch(ctx, db, 1)
	if errBackfill != nil {
		t.Fatalf("second backfill error = %v", errBackfill)
	}
	if second.Scanned != 1 || second.Updated != 1 || !second.Done {
		t.Fatalf("second backfill = %+v", second)
	}

	var stored []UsageRecord
	if errFind := db.Order("id ASC").Find(&stored).Error; errFind != nil {
		t.Fatalf("load backfilled rows: %v", errFind)
	}
	firstBreakdown := usageTokenBreakdownFromRecord(&stored[0])
	if !firstBreakdown.Valid() || firstBreakdown.Quality != UsageTokenAccountingQualityComplete || firstBreakdown.Input.UncachedTokens != 60 || firstBreakdown.Output.NonReasoningTokens != 18 {
		t.Fatalf("first breakdown = %+v", firstBreakdown)
	}
	secondBreakdown := usageTokenBreakdownFromRecord(&stored[1])
	if !secondBreakdown.Valid() || secondBreakdown.Quality != UsageTokenAccountingQualityUnclassified || secondBreakdown.UnclassifiedTokens != 10 {
		t.Fatalf("second breakdown = %+v", secondBreakdown)
	}
	ready, errReady = repo.UsageTokenBreakdownV2Ready(ctx)
	if errReady != nil || !ready {
		t.Fatalf("ready after backfill = %t, err=%v", ready, errReady)
	}

	lateRecord := UsageRecord{
		Timestamp:    time.Date(2026, time.July, 23, 1, 2, 0, 0, time.UTC),
		Provider:     "openai",
		InputTokens:  8,
		OutputTokens: 2,
		TotalTokens:  10,
		PayloadJSON:  JSONB(`{"timestamp":"2026-07-23T01:02:00Z"}`),
		CreatedAt:    time.Now().UTC(),
	}
	if errCreate := db.Create(&lateRecord).Error; errCreate != nil {
		t.Fatalf("create late legacy usage row: %v", errCreate)
	}
	ready, errReady = repo.UsageTokenBreakdownV2Ready(ctx)
	if errReady != nil || ready {
		t.Fatalf("ready after late legacy row = %t, err=%v", ready, errReady)
	}
	lateBackfill, errBackfill := runUsageTokenAccountingBackfillBatch(ctx, db, 10)
	if errBackfill != nil {
		t.Fatalf("late backfill error = %v", errBackfill)
	}
	if lateBackfill.Updated != 1 || !lateBackfill.Done {
		t.Fatalf("late backfill = %+v", lateBackfill)
	}
	ready, errReady = repo.UsageTokenBreakdownV2Ready(ctx)
	if errReady != nil || !ready {
		t.Fatalf("ready after late backfill = %t, err=%v", ready, errReady)
	}
}
