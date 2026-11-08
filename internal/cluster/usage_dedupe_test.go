package cluster

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"
)

func TestAppendUsageDedupeKeyUniqueIndexCollapsesConcurrentRedeliveries(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	db, errDB := repo.database()
	if errDB != nil {
		t.Fatalf("database() error = %v", errDB)
	}
	if !db.Migrator().HasIndex(&UsageRecord{}, "idx_usage_dedupe_key") {
		t.Fatal("expected idx_usage_dedupe_key after migration")
	}

	payload := `{"timestamp":"2026-06-10T01:02:03Z","provider":"openai","model":"gpt-4.1-mini","request_id":"req-1","execution_id":"exec-concurrent","tokens":{"input_tokens":1,"total_tokens":1}}`
	const deliveries = 8
	var wg sync.WaitGroup
	results := make([]bool, deliveries)
	errs := make([]error, deliveries)
	for i := 0; i < deliveries; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			_, inserted, errAppend := repo.AppendUsageWithRuntimeOutcome(ctx, payload, UsageRuntimeMetadata{HomeIP: "192.0.2.10"})
			results[index], errs[index] = inserted, errAppend
		}(i)
	}
	wg.Wait()

	insertedCount := 0
	for i := 0; i < deliveries; i++ {
		if errs[i] != nil {
			t.Fatalf("AppendUsageWithRuntimeOutcome(%d) error = %v", i, errs[i])
		}
		if results[i] {
			insertedCount++
		}
	}
	if insertedCount != 1 {
		t.Fatalf("inserted deliveries = %d, want 1", insertedCount)
	}
	var rows []UsageRecord
	if errFind := db.Find(&rows).Error; errFind != nil {
		t.Fatalf("Find(usage) error = %v", errFind)
	}
	if len(rows) != 1 || rows[0].DedupeKey == "" {
		t.Fatalf("usage rows = %+v, want exactly one row with a dedupe key", rows)
	}

	// Rows written before the column existed carry an empty key and never collide.
	legacy := &UsageRecord{Timestamp: rows[0].Timestamp, PayloadJSON: JSONB("{}")}
	if errCreate := db.Create(legacy).Error; errCreate != nil {
		t.Fatalf("Create(legacy) error = %v", errCreate)
	}
	if errCreate := db.Create(&UsageRecord{Timestamp: rows[0].Timestamp, PayloadJSON: JSONB("{}")}).Error; errCreate != nil {
		t.Fatalf("Create(second legacy) error = %v", errCreate)
	}
}

func legacyUsagePayload(executionID string, at time.Time) string {
	return fmt.Sprintf(`{"timestamp":%q,"provider":"openai","model":"gpt-4.1-mini","request_id":"req-%s","execution_id":%q,"tokens":{"input_tokens":1,"total_tokens":1}}`,
		at.Format(time.RFC3339Nano), executionID, executionID)
}

// insertLegacyUsageRow mimics a row committed before dedupe keys existed.
func insertLegacyUsageRow(t *testing.T, repo *Repository, payload string) *UsageRecord {
	t.Helper()
	record, errRecord := UsageRecordFromPayload(payload, "192.0.2.10")
	if errRecord != nil {
		t.Fatalf("UsageRecordFromPayload() error = %v", errRecord)
	}
	db, errDB := repo.database()
	if errDB != nil {
		t.Fatalf("database() error = %v", errDB)
	}
	if errCreate := db.Create(record).Error; errCreate != nil {
		t.Fatalf("Create(legacy usage) error = %v", errCreate)
	}
	return record
}

func TestMigrateUsageDedupeKeysKeysEveryLegacyRowAndPreventsDoubleCharge(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	db, errDB := repo.database()
	if errDB != nil {
		t.Fatalf("database() error = %v", errDB)
	}
	username, credits, clientKey := "legacy@example.com", 10.0, "legacy-key"
	user, errUser := repo.CreateUser(ctx, UserUpdate{Username: &username, Credits: &credits})
	if errUser != nil {
		t.Fatalf("CreateUser() error = %v", errUser)
	}
	if _, errKey := repo.CreateAPIKeyForUser(ctx, user.ID, APIKeyUserUpdate{APIKey: &clientKey}); errKey != nil {
		t.Fatalf("CreateAPIKeyForUser() error = %v", errKey)
	}
	if _, errPrice := repo.CreateBillingModelPrice(ctx, BillingModelPriceUpdate{Provider: "openai", Model: "gpt-4.1-mini", RequestPrice: 1, Enabled: true}); errPrice != nil {
		t.Fatalf("CreateBillingModelPrice() error = %v", errPrice)
	}
	now := time.Now().UTC()

	// A billed event CPA kept retrying for hours before the upgrade: its request
	// timestamp is old, yet it is still redelivered once Home comes back.
	oldPayload := fmt.Sprintf(`{"timestamp":%q,"provider":"openai","model":"gpt-4.1-mini","api_key":"legacy-key","execution_id":"exec-old-billed","tokens":{"total_tokens":1}}`, now.Add(-5*time.Hour).Format(time.RFC3339Nano))
	old := insertLegacyUsageRow(t, repo, oldPayload)
	if errCharge := db.Transaction(func(tx *gorm.DB) error {
		return repo.createBillingChargeForUsageTx(ctx, tx, old, oldPayload)
	}); errCharge != nil {
		t.Fatalf("pre-upgrade charge error = %v", errCharge)
	}
	recent := insertLegacyUsageRow(t, repo, legacyUsagePayload("exec-recent", now.Add(-time.Minute)))
	duplicate := insertLegacyUsageRow(t, repo, legacyUsagePayload("exec-recent", now.Add(-time.Minute)))
	// The second copy lands in a later batch than the first with batch size 2.
	crossBatch := insertLegacyUsageRow(t, repo, legacyUsagePayload("exec-old-billed", now.Add(-5*time.Hour)))

	scanned, keyed, errBackfill := backfillUsageDedupeKeys(db, 2)
	if errBackfill != nil {
		t.Fatalf("backfillUsageDedupeKeys() error = %v", errBackfill)
	}
	if scanned != 4 || keyed != 2 {
		t.Fatalf("scanned=%d keyed=%d, want 4 scanned and 2 keyed", scanned, keyed)
	}
	keys := map[uint]string{}
	var rows []UsageRecord
	if errFind := db.Select("id", "dedupe_key").Find(&rows).Error; errFind != nil {
		t.Fatalf("Find(usage) error = %v", errFind)
	}
	for _, row := range rows {
		keys[row.ID] = row.DedupeKey
	}
	if keys[old.ID] == "" || keys[recent.ID] == "" {
		t.Fatalf("legacy rows must be keyed regardless of age: old=%q recent=%q", keys[old.ID], keys[recent.ID])
	}
	if keys[duplicate.ID] != "" || keys[crossBatch.ID] != "" {
		t.Fatalf("pre-existing duplicates must keep an empty key, got %q and %q", keys[duplicate.ID], keys[crossBatch.ID])
	}

	// Rerunning is a no-op; the migration is idempotent.
	if _, keyedAgain, errAgain := backfillUsageDedupeKeys(db, 2); errAgain != nil || keyedAgain != 0 {
		t.Fatalf("second backfill keyed=%d err=%v, want 0/nil", keyedAgain, errAgain)
	}

	// The redelivered old event collapses onto its row and is not charged again.
	existing, inserted, errAppend := repo.AppendUsageWithRuntimeOutcome(ctx, oldPayload, UsageRuntimeMetadata{HomeIP: "192.0.2.11"})
	if errAppend != nil || inserted || existing == nil || existing.ID != old.ID {
		t.Fatalf("redelivery inserted=%v id=%v err=%v, want dedupe onto %d", inserted, existing, errAppend, old.ID)
	}
	var charges int64
	if errCount := db.Model(&BillingChargeRecord{}).Count(&charges).Error; errCount != nil {
		t.Fatalf("Count(billing_charge) error = %v", errCount)
	}
	updated, errUpdated := repo.GetUser(ctx, user.ID)
	if errUpdated != nil {
		t.Fatalf("GetUser() error = %v", errUpdated)
	}
	if charges != 1 || updated.Credits != 9 {
		t.Fatalf("charges=%d credits=%v after redelivery, want 1 and 9", charges, updated.Credits)
	}
}

func TestMigrateUsageDedupeKeysHonorsCancelledContext(t *testing.T) {
	t.Parallel()

	repo, closeRepo := newBillingTestRepository(t, context.Background())
	defer closeRepo()
	db, errDB := repo.database()
	if errDB != nil {
		t.Fatalf("database() error = %v", errDB)
	}
	row := insertLegacyUsageRow(t, repo, legacyUsagePayload("exec-cancelled", time.Now().UTC()))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	errMigrate := migrateUsageDedupeKeys(db.WithContext(ctx))
	if !errors.Is(errMigrate, context.Canceled) {
		t.Fatalf("migrateUsageDedupeKeys() error = %v, want context.Canceled", errMigrate)
	}
	var stored UsageRecord
	if errFirst := db.Select("id", "dedupe_key").First(&stored, row.ID).Error; errFirst != nil {
		t.Fatalf("First(legacy) error = %v", errFirst)
	}
	if stored.DedupeKey != "" {
		t.Fatalf("cancelled migration keyed the row: %q", stored.DedupeKey)
	}
}

func TestAutoMigrateUpgradesExistingUsageTableWithDedupeKeys(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := openDatabaseSnapshotSQLiteRawTestDB(t, filepath.Join(t.TempDir(), "legacy.db"))
	// Create the pre-upgrade usage shape and commit a row the way an older Home did.
	if errMigrate := db.AutoMigrate(&databaseSnapshotV17UsageRecord{}); errMigrate != nil {
		t.Fatalf("AutoMigrate(v17 usage) error = %v", errMigrate)
	}
	payload := legacyUsagePayload("exec-upgrade", time.Now().UTC().Add(-30*time.Second))
	current, errRecord := UsageRecordFromPayload(payload, "192.0.2.10")
	if errRecord != nil {
		t.Fatalf("UsageRecordFromPayload() error = %v", errRecord)
	}
	legacy := databaseSnapshotV17UsageRecord{Timestamp: current.Timestamp, Provider: current.Provider, Model: current.Model, RequestID: current.RequestID, PayloadJSON: current.PayloadJSON, CreatedAt: current.CreatedAt}
	if errCreate := db.Create(&legacy).Error; errCreate != nil {
		t.Fatalf("Create(legacy) error = %v", errCreate)
	}

	if errMigrate := AutoMigrate(db); errMigrate != nil {
		t.Fatalf("AutoMigrate() error = %v", errMigrate)
	}
	if !db.Migrator().HasColumn(&UsageRecord{}, "dedupe_key") || !db.Migrator().HasIndex(&UsageRecord{}, "idx_usage_dedupe_key") {
		t.Fatal("upgrade did not add dedupe_key column and index")
	}
	var upgraded UsageRecord
	if errFirst := db.First(&upgraded, legacy.ID).Error; errFirst != nil {
		t.Fatalf("First(legacy) error = %v", errFirst)
	}
	if upgraded.DedupeKey == "" {
		t.Fatal("startup migration did not key the recent legacy row")
	}

	repo := NewRepository(db)
	existing, inserted, errAppend := repo.AppendUsageWithRuntimeOutcome(ctx, payload, UsageRuntimeMetadata{HomeIP: "192.0.2.10"})
	if errAppend != nil || inserted || existing == nil || existing.ID != legacy.ID {
		t.Fatalf("post-upgrade redelivery inserted=%v id=%v err=%v, want dedupe onto %d", inserted, existing, errAppend, legacy.ID)
	}
}

func TestDatabaseSnapshotV17UsageImportsIntoCurrentFormat(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	source, closeSource := newBillingTestRepository(t, ctx)
	defer closeSource()
	payload := legacyUsagePayload("exec-snapshot", time.Now().UTC())
	if _, errAppend := source.AppendUsage(ctx, payload, "192.0.2.10"); errAppend != nil {
		t.Fatalf("AppendUsage() error = %v", errAppend)
	}
	sourceDB, errDB := source.database()
	if errDB != nil {
		t.Fatalf("database() error = %v", errDB)
	}
	path := filepath.Join(t.TempDir(), "snapshot-v17.zip")
	if _, errExport := exportDatabaseSnapshotVersion(ctx, sourceDB, path, 17); errExport != nil {
		t.Fatalf("exportDatabaseSnapshotVersion(17) error = %v", errExport)
	}
	snapshot, errOpen := OpenDatabaseSnapshot(ctx, path)
	if errOpen != nil {
		t.Fatalf("OpenDatabaseSnapshot() error = %v", errOpen)
	}
	defer func() {
		if errClose := snapshot.Close(); errClose != nil {
			t.Errorf("snapshot.Close() error = %v", errClose)
		}
	}()

	target := openDatabaseSnapshotSQLiteTestDB(t, filepath.Join(t.TempDir(), "target.db"))
	if _, errImport := ImportDatabaseSnapshot(ctx, target, snapshot, nil); errImport != nil {
		t.Fatalf("ImportDatabaseSnapshot(v17) error = %v", errImport)
	}
	var imported []UsageRecord
	if errFind := target.Find(&imported).Error; errFind != nil {
		t.Fatalf("Find(usage) error = %v", errFind)
	}
	// Import runs the custom migrations, so the recent v17 row is keyed on arrival.
	wantKey, errKey := usagePayloadDedupeKey(payload)
	if errKey != nil {
		t.Fatalf("usagePayloadDedupeKey() error = %v", errKey)
	}
	if len(imported) != 1 || imported[0].DedupeKey != wantKey {
		t.Fatalf("imported usage rows = %d key = %q, want one row keyed %q", len(imported), firstUsageDedupeKey(imported), wantKey)
	}
}

func firstUsageDedupeKey(rows []UsageRecord) string {
	if len(rows) == 0 {
		return ""
	}
	return rows[0].DedupeKey
}
