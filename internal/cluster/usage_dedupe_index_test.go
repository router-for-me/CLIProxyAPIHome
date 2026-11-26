package cluster

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"
)

func TestUsageDedupeQueriesUsePartialIndex(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	db, errDB := repo.database()
	if errDB != nil {
		t.Fatalf("database() error = %v", errDB)
	}

	// Exercise a full production-size update batch and a redelivery near the
	// table tail, using the SQL emitted by the production paths below.
	now := time.Now().UTC()
	records := make([]UsageRecord, usageDedupeKeyBackfillBatchSize+1)
	var payload string
	for i := range records {
		payload = legacyUsagePayload(fmt.Sprintf("exec-index-%d", i), now)
		record, errRecord := UsageRecordFromPayload(payload, "192.0.2.10")
		if errRecord != nil {
			t.Fatalf("UsageRecordFromPayload(%d) error = %v", i, errRecord)
		}
		records[i] = *record
	}
	if errCreate := db.CreateInBatches(&records, 100).Error; errCreate != nil {
		t.Fatalf("CreateInBatches(legacy usage) error = %v", errCreate)
	}

	var backfillSQL string
	var backfillArgs []any
	if errRegister := db.Callback().Raw().After("gorm:raw").Register("test:capture_usage_dedupe_backfill", func(tx *gorm.DB) {
		if backfillSQL == "" && strings.HasPrefix(tx.Statement.SQL.String(), `UPDATE "usage" SET "dedupe_key"`) {
			backfillSQL = tx.Statement.SQL.String()
			backfillArgs = append([]any(nil), tx.Statement.Vars...)
		}
	}); errRegister != nil {
		t.Fatalf("register backfill capture: %v", errRegister)
	}
	t.Cleanup(func() {
		if errRemove := db.Callback().Raw().Remove("test:capture_usage_dedupe_backfill"); errRemove != nil {
			t.Errorf("remove backfill capture: %v", errRemove)
		}
	})
	scanned, keyed, errBackfill := backfillUsageDedupeKeys(db, usageDedupeKeyBackfillBatchSize)
	if errBackfill != nil || scanned != len(records) || keyed != len(records) {
		t.Fatalf("backfill scanned=%d keyed=%d err=%v, want %d rows keyed", scanned, keyed, errBackfill, len(records))
	}

	var lookupSQL string
	var lookupArgs []any
	if errRegister := db.Callback().Query().After("gorm:query").Register("test:capture_usage_dedupe_lookup", func(tx *gorm.DB) {
		if tx.Statement.Table == "usage" {
			lookupSQL = tx.Statement.SQL.String()
			lookupArgs = append([]any(nil), tx.Statement.Vars...)
		}
	}); errRegister != nil {
		t.Fatalf("register lookup capture: %v", errRegister)
	}
	t.Cleanup(func() {
		if errRemove := db.Callback().Query().Remove("test:capture_usage_dedupe_lookup"); errRemove != nil {
			t.Errorf("remove lookup capture: %v", errRemove)
		}
	})
	existing, inserted, errAppend := repo.AppendUsageWithRuntimeOutcome(ctx, payload, UsageRuntimeMetadata{HomeIP: "192.0.2.11"})
	if errAppend != nil || inserted || existing == nil || existing.ID != records[len(records)-1].ID {
		t.Fatalf("redelivery inserted=%v record=%+v err=%v, want the last legacy row", inserted, existing, errAppend)
	}

	for _, tc := range []struct {
		name string
		sql  string
		args []any
	}{
		{name: "backfill", sql: backfillSQL, args: backfillArgs},
		{name: "redelivery", sql: lookupSQL, args: lookupArgs},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.sql == "" {
				t.Fatal("production query was not captured")
			}
			var plan []struct{ Detail string }
			if errPlan := db.Raw("EXPLAIN QUERY PLAN "+tc.sql, tc.args...).Scan(&plan).Error; errPlan != nil {
				t.Fatalf("EXPLAIN QUERY PLAN error = %v", errPlan)
			}
			var tableSteps []string
			for _, step := range plan {
				if strings.Contains(step.Detail, "SEARCH ") && strings.Contains(step.Detail, "INDEX idx_usage_dedupe_key ") {
					return
				}
				if strings.Contains(step.Detail, "usage") || strings.Contains(step.Detail, "keyed") {
					tableSteps = append(tableSteps, step.Detail)
				}
			}
			t.Fatalf("dedupe query does not search idx_usage_dedupe_key: %v", tableSteps)
		})
	}
}
