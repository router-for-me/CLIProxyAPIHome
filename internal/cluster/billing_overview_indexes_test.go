package cluster

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"
)

func TestBillingOverviewIndexesPostgres(t *testing.T) {
	repo := newPostgresQuiescenceRepository(t)
	db, errDB := repo.database()
	if errDB != nil {
		t.Fatal(errDB)
	}
	// The unreleased usage and financial indexes share the v12-to-v13 migration.
	for _, name := range []string{billingUsageCoverIndex, billingOverviewTimeCoverIndex, billingOverviewUsageCoverIndex} {
		if errDrop := db.Exec("DROP INDEX " + name).Error; errDrop != nil {
			t.Fatal(errDrop)
		}
	}
	if errVersion := db.Model(&schemaMigrationRecord{}).Where("id = ?", schemaMigrationRecordID).Update("version", 12).Error; errVersion != nil {
		t.Fatal(errVersion)
	}
	if errMigrate := AutoMigrateContext(t.Context(), db); errMigrate != nil {
		t.Fatalf("upgrade v12 schema: %v", errMigrate)
	}
	assertPostgresSchemaMigrationVersion(t, db, currentDatabaseVersion)
	for attempt := 0; attempt < 2; attempt++ {
		if errRepeat := migrateBillingOverviewIndexes(db); errRepeat != nil {
			t.Fatalf("repeat migration %d: %v", attempt, errRepeat)
		}
	}
	for _, name := range []string{"idx_billing_charge_usage_id", "idx_billing_charge_time_order", billingUsageCoverIndex} {
		if !db.Migrator().HasIndex(&BillingChargeRecord{}, name) {
			t.Fatalf("existing index %s was removed", name)
		}
	}
	longLabel := strings.Repeat("historical-label-", 4096)
	charge := BillingChargeRecord{ID: "finance-index", UsageID: 1, PayloadHash: "finance-index",
		APIKeyLabel: longLabel, APIKeyMasked: longLabel, PriceSnapshot: JSONB(`{}`), CreatedAt: time.Now().UTC()}
	if errCreate := db.Create(&charge).Error; errCreate != nil {
		t.Fatalf("unbounded label rejected: %v", errCreate)
	}
	var stored BillingChargeRecord
	if errFirst := db.First(&stored, "id = ?", charge.ID).Error; errFirst != nil {
		t.Fatal(errFirst)
	}
	if stored.APIKeyLabel != longLabel || stored.APIKeyMasked != longLabel {
		t.Fatal("historical labels changed")
	}
}

func TestBillingOverviewIndexesRestoreSettingsPostgres(t *testing.T) {
	repo := newPostgresQuiescenceRepository(t)
	db, errDB := repo.database()
	if errDB != nil {
		t.Fatal(errDB)
	}
	for _, memory := range []string{"1MB", "32MB"} {
		if errTransaction := db.Transaction(func(tx *gorm.DB) error {
			for _, name := range []string{billingOverviewTimeCoverIndex, billingOverviewUsageCoverIndex} {
				if errDrop := tx.Exec("DROP INDEX " + name).Error; errDrop != nil {
					return errDrop
				}
			}
			if errSettings := tx.Exec(`SELECT set_config('maintenance_work_mem', ?, true), set_config('max_parallel_maintenance_workers', '1', true)`, memory).Error; errSettings != nil {
				return errSettings
			}
			if errMigrate := migrateBillingOverviewIndexes(tx); errMigrate != nil {
				return errMigrate
			}
			var settings struct{ Memory, Workers string }
			if errRead := tx.Raw(`SELECT current_setting('maintenance_work_mem') AS memory, current_setting('max_parallel_maintenance_workers') AS workers`).Scan(&settings).Error; errRead != nil {
				return errRead
			}
			if settings.Memory != memory || settings.Workers != "1" {
				return fmt.Errorf("settings not restored: %+v", settings)
			}
			return nil
		}); errTransaction != nil {
			t.Fatal(errTransaction)
		}
	}
}

func TestBillingOverviewIndexesRejectDefinitionsPostgres(t *testing.T) {
	repo := newPostgresQuiescenceRepository(t)
	db, errDB := repo.database()
	if errDB != nil {
		t.Fatal(errDB)
	}
	for _, tc := range []struct{ name, index, definition string }{
		{"missing columns", billingOverviewTimeCoverIndex, "ON billing_charge(created_at)"},
		{"wrong relation", billingOverviewTimeCoverIndex, "ON usage(created_at)"},
		{"wrong predicate", billingOverviewUsageCoverIndex, "ON usage(id) INCLUDE(accounting_input_tokens,accounting_output_tokens) WHERE token_accounting_version=1 AND token_accounting_quality='complete'"},
		{"missing predicate", billingOverviewUsageCoverIndex, "ON usage(id) INCLUDE(accounting_input_tokens,accounting_output_tokens)"},
		{"wrong include order", billingOverviewUsageCoverIndex, "ON usage(id) INCLUDE(accounting_output_tokens,accounting_input_tokens) WHERE token_accounting_version=2 AND token_accounting_quality='complete'"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rollback := errors.New("rollback index fixture")
			errTransaction := db.Transaction(func(tx *gorm.DB) error {
				if errDrop := tx.Exec("DROP INDEX " + tc.index).Error; errDrop != nil {
					return errDrop
				}
				if errCreate := tx.Exec("CREATE INDEX " + tc.index + " " + tc.definition).Error; errCreate != nil {
					return errCreate
				}
				if errMigrate := migrateBillingOverviewIndexes(tx); errMigrate == nil || !strings.Contains(errMigrate.Error(), "unexpected definition") {
					t.Errorf("accepted unexpected definition: %v", errMigrate)
				}
				return rollback
			})
			if !errors.Is(errTransaction, rollback) {
				t.Fatal(errTransaction)
			}
		})
	}
}

func TestBillingOverviewIndexesSkipSQLite(t *testing.T) {
	repo, closeRepo := newBillingTestRepository(t, t.Context())
	t.Cleanup(closeRepo)
	db, errDB := repo.database()
	if errDB != nil {
		t.Fatal(errDB)
	}
	if errMigrate := migrateBillingOverviewIndexes(db); errMigrate != nil {
		t.Fatal(errMigrate)
	}
	if db.Migrator().HasIndex(&BillingChargeRecord{}, billingOverviewTimeCoverIndex) || db.Migrator().HasIndex(&UsageRecord{}, billingOverviewUsageCoverIndex) {
		t.Fatal("PostgreSQL covering indexes were created in SQLite")
	}
}
