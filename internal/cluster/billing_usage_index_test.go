package cluster

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"
)

func TestBillingUsageCoverIndexPostgres(t *testing.T) {
	repo := newPostgresQuiescenceRepository(t)
	db, errDB := repo.database()
	if errDB != nil {
		t.Fatal(errDB)
	}
	// A recorded v12 schema must run the new index migration on upgrade.
	if errDrop := db.Exec("DROP INDEX " + billingUsageCoverIndex).Error; errDrop != nil {
		t.Fatal(errDrop)
	}
	if errVersion := db.Model(&schemaMigrationRecord{}).Where("id = ?", schemaMigrationRecordID).Update("version", 12).Error; errVersion != nil {
		t.Fatal(errVersion)
	}
	if errMigrate := AutoMigrateContext(t.Context(), db); errMigrate != nil {
		t.Fatalf("upgrade v12 schema: %v", errMigrate)
	}
	assertPostgresSchemaMigrationVersion(t, db, currentDatabaseVersion)
	for attempt := 0; attempt < 2; attempt++ {
		if errMigrate := migrateBillingUsageCoverIndex(db); errMigrate != nil {
			t.Fatalf("repeat migration %d: %v", attempt, errMigrate)
		}
	}
	if !db.Migrator().HasIndex(&BillingChargeRecord{}, "idx_billing_charge_usage_id") {
		t.Fatal("original usage identity index was removed")
	}
	// Imported historical labels have no length bound. They must not become
	// indexed values or start rejecting otherwise valid billing records.
	longLabel := strings.Repeat("historical-label-", 4096)
	charge := BillingChargeRecord{ID: "cover-test", UsageID: 1, PayloadHash: "cover-test",
		APIKeyLabel: longLabel, APIKeyMasked: longLabel, PriceSnapshot: JSONB(`{}`), CreatedAt: time.Now().UTC()}
	if errCreate := db.Create(&charge).Error; errCreate != nil {
		t.Fatalf("create charge with unbounded historical labels: %v", errCreate)
	}
	duplicate := charge
	duplicate.ID, duplicate.PayloadHash = "cover-duplicate", "cover-duplicate"
	if errDuplicate := db.Create(&duplicate).Error; errDuplicate == nil {
		t.Fatal("duplicate usage_id was accepted")
	}
	var stored BillingChargeRecord
	if errFirst := db.First(&stored, "id = ?", charge.ID).Error; errFirst != nil {
		t.Fatal(errFirst)
	}
	if stored.APIKeyLabel != longLabel || stored.APIKeyMasked != longLabel {
		t.Fatal("cover migration changed historical labels")
	}
}

func TestBillingUsageCoverIndexRestoresBuildSettingsPostgres(t *testing.T) {
	repo := newPostgresQuiescenceRepository(t)
	db, errDB := repo.database()
	if errDB != nil {
		t.Fatal(errDB)
	}
	for _, memoryLimit := range []string{"1MB", "32MB"} {
		errTransaction := db.Transaction(func(tx *gorm.DB) error {
			if errDrop := tx.Exec("DROP INDEX " + billingUsageCoverIndex).Error; errDrop != nil {
				return errDrop
			}
			if errSettings := tx.Exec(`SELECT set_config('maintenance_work_mem', ?, true),
				set_config('max_parallel_maintenance_workers', '1', true)`, memoryLimit).Error; errSettings != nil {
				return errSettings
			}
			if errMigrate := migrateBillingUsageCoverIndex(tx); errMigrate != nil {
				return errMigrate
			}
			var settings struct {
				MemoryLimit string
				Workers     string
			}
			if errRead := tx.Raw(`SELECT current_setting('maintenance_work_mem') AS memory_limit,
				current_setting('max_parallel_maintenance_workers') AS workers`).Scan(&settings).Error; errRead != nil {
				return errRead
			}
			if settings.MemoryLimit != memoryLimit || settings.Workers != "1" {
				return fmt.Errorf("build changed caller settings: %+v", settings)
			}
			return nil
		})
		if errTransaction != nil {
			t.Fatalf("build with maintenance_work_mem=%s: %v", memoryLimit, errTransaction)
		}
	}
}

func TestBillingUsageCoverIndexRejectsUnexpectedDefinitionPostgres(t *testing.T) {
	repo := newPostgresQuiescenceRepository(t)
	db, errDB := repo.database()
	if errDB != nil {
		t.Fatal(errDB)
	}
	if errDrop := db.Exec("DROP INDEX " + billingUsageCoverIndex).Error; errDrop != nil {
		t.Fatal(errDrop)
	}
	if errCreate := db.Exec("CREATE INDEX " + billingUsageCoverIndex + " ON billing_charge (usage_id)").Error; errCreate != nil {
		t.Fatal(errCreate)
	}
	if errMigrate := migrateBillingUsageCoverIndex(db); errMigrate == nil || !strings.Contains(errMigrate.Error(), "unexpected definition") {
		t.Fatalf("migration accepted unexpected definition: %v", errMigrate)
	}
}

func TestBillingUsageCoverIndexSkipsSQLite(t *testing.T) {
	repo, closeRepo := newBillingTestRepository(t, t.Context())
	t.Cleanup(closeRepo)
	db, errDB := repo.database()
	if errDB != nil {
		t.Fatal(errDB)
	}
	if errMigrate := migrateBillingUsageCoverIndex(db); errMigrate != nil {
		t.Fatal(errMigrate)
	}
	if db.Migrator().HasIndex(&BillingChargeRecord{}, billingUsageCoverIndex) {
		t.Fatal("PostgreSQL-only covering index was created in SQLite")
	}
}
