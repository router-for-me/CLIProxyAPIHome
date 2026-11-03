package cluster

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"reflect"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestDashboardIndexesMigrateExistingDatabase(t *testing.T) {
	t.Parallel()

	repo, closeRepo := newBillingTestRepository(t, context.Background())
	defer closeRepo()
	db, errDB := repo.database()
	if errDB != nil {
		t.Fatalf("database() error = %v", errDB)
	}
	for _, indexName := range []string{"idx_usage_token_accounting_pending_v2", "idx_billing_charge_time_order"} {
		if errDrop := db.Exec("DROP INDEX " + indexName).Error; errDrop != nil {
			t.Fatalf("drop index %s: %v", indexName, errDrop)
		}
	}
	// Both upgrading a database without these indexes and rerunning migration are safe.
	for attempt := 0; attempt < 2; attempt++ {
		if errMigrate := AutoMigrate(db); errMigrate != nil {
			t.Fatalf("AutoMigrate() attempt %d: %v", attempt, errMigrate)
		}
	}
	var pendingIndexSQL string
	if errScan := db.Raw(`SELECT sql FROM sqlite_master WHERE type = 'index' AND name = ?`, "idx_usage_token_accounting_pending_v2").Scan(&pendingIndexSQL).Error; errScan != nil {
		t.Fatalf("read pending index definition: %v", errScan)
	}
	wantPredicate := fmt.Sprintf(`WHERE "token_accounting_version" <> %d`, UsageTokenAccountingSchemaVersion)
	if !strings.Contains(pendingIndexSQL, `ON "usage" ("id")`) || !strings.Contains(pendingIndexSQL, wantPredicate) {
		t.Fatalf("pending index definition = %q, want id index with %q", pendingIndexSQL, wantPredicate)
	}
	var columns []struct {
		Name string
		Desc int
		Key  int
	}
	if errScan := db.Raw(`PRAGMA index_xinfo('idx_billing_charge_time_order')`).Scan(&columns).Error; errScan != nil {
		t.Fatalf("read billing time index columns: %v", errScan)
	}
	var keyColumns []string
	for _, column := range columns {
		if column.Key == 0 {
			continue
		}
		if column.Desc != 1 {
			t.Fatalf("billing index column %q is not descending", column.Name)
		}
		keyColumns = append(keyColumns, column.Name)
	}
	if !reflect.DeepEqual(keyColumns, []string{"created_at", "id"}) {
		t.Fatalf("billing index columns = %v, want [created_at id]", keyColumns)
	}
}

func TestUsageTokenAccountingPendingQueriesUseLiteralVersion(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	db, errDB := repo.database()
	if errDB != nil {
		t.Fatalf("database() error = %v", errDB)
	}
	var logs bytes.Buffer
	queryDB := db.Session(&gorm.Session{Logger: logger.New(log.New(&logs, "", 0), logger.Config{
		LogLevel:             logger.Info,
		ParameterizedQueries: true,
	})})
	queryRepo := NewRepository(queryDB)
	ready, errReady := queryRepo.UsageTokenBreakdownV2Ready(ctx)
	if errReady != nil || !ready {
		t.Fatalf("empty database readiness = %t, err=%v", ready, errReady)
	}
	pending, errPending := firstPendingUsageTokenAccountingRecord(ctx, queryDB)
	if errPending != nil || pending != nil {
		t.Fatalf("empty database pending record = %+v, err=%v", pending, errPending)
	}
	predicate := fmt.Sprintf(`"token_accounting_version" <> %d`, UsageTokenAccountingSchemaVersion)
	if strings.Count(logs.String(), predicate) != 2 {
		t.Fatalf("both pending queries must use literal predicate %q; logs: %s", predicate, logs.String())
	}

	now := time.Date(2026, time.October, 12, 0, 0, 0, 0, time.UTC)
	records := []UsageRecord{
		{Timestamp: now, TokenAccountingVersion: UsageTokenAccountingSchemaVersion, PayloadJSON: JSONB(`{}`), CreatedAt: now},
		{Timestamp: now, TokenAccountingVersion: 0, PayloadJSON: JSONB(`{}`), CreatedAt: now},
		{Timestamp: now, TokenAccountingVersion: UsageTokenAccountingSchemaVersion + 1, PayloadJSON: JSONB(`{}`), CreatedAt: now},
	}
	if errCreate := db.Create(&records).Error; errCreate != nil {
		t.Fatalf("create mixed-version records: %v", errCreate)
	}
	ready, errReady = queryRepo.UsageTokenBreakdownV2Ready(ctx)
	if errReady != nil || ready {
		t.Fatalf("mixed-version readiness = %t, err=%v", ready, errReady)
	}
	pending, errPending = firstPendingUsageTokenAccountingRecord(ctx, queryDB)
	if errPending != nil || pending == nil || pending.ID != records[1].ID {
		t.Fatalf("first pending record = %+v, err=%v, want id %d", pending, errPending, records[1].ID)
	}
	if errUpdate := db.Model(&UsageRecord{}).Where("id = ?", records[1].ID).Update("token_accounting_version", UsageTokenAccountingSchemaVersion).Error; errUpdate != nil {
		t.Fatalf("mark legacy record canonical: %v", errUpdate)
	}
	pending, errPending = firstPendingUsageTokenAccountingRecord(ctx, queryDB)
	if errPending != nil || pending == nil || pending.ID != records[2].ID {
		t.Fatalf("higher-version pending record = %+v, err=%v, want id %d", pending, errPending, records[2].ID)
	}
}

func TestDatabaseSnapshotIndexOnlyVersionsRemainCompatible(t *testing.T) {
	t.Parallel()

	for _, version := range []int{6, 7, 8, 9, 10, 11, 12, 13, currentDatabaseVersion} {
		models, okModels := databaseSnapshotModels(version)
		if !okModels || len(models) != len(homeDatabaseModels) {
			t.Fatalf("snapshot version %d registry is incompatible", version)
		}
		for index, model := range models {
			current := homeDatabaseModels[index]
			if model.name != current.name || reflect.TypeOf(model.newRecord()) != reflect.TypeOf(current.newRecord()) {
				t.Fatalf("snapshot version %d model %d differs from current table shape", version, index)
			}
		}
	}
}
