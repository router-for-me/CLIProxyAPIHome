package cluster

import (
	"fmt"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	"gorm.io/gorm"
)

// usageDedupeKeyBackfillBatchSize bounds one scan/update round of the startup
// dedupe key migration.
const usageDedupeKeyBackfillBatchSize = 500

// migrateUsageDedupeKeys keys every usage row written before dedupe_key
// existed. CPA requeues a rejected usage event with a fresh retention
// timestamp, so an already-billed event can be redelivered after an upgrade no
// matter how old its request timestamp is; only a complete backfill makes the
// unique index catch those redeliveries. The migration is idempotent and runs
// with the context carried by db, so startup cancellation stops it.
func migrateUsageDedupeKeys(db *gorm.DB) error {
	startedAt := time.Now()
	scanned, keyed, errBackfill := backfillUsageDedupeKeys(db, usageDedupeKeyBackfillBatchSize)
	if errBackfill != nil {
		return errBackfill
	}
	if scanned > 0 {
		log.WithFields(log.Fields{
			"usage_rows_scanned": scanned,
			"usage_rows_keyed":   keyed,
			"duration_ms":        time.Since(startedAt).Milliseconds(),
		}).Info("usage dedupe key backfill completed")
	}
	return nil
}

// backfillUsageDedupeKeys walks legacy rows in id order and keys them in
// batches. It reports the number of legacy rows scanned and keyed.
func backfillUsageDedupeKeys(db *gorm.DB, batchSize int) (int, int, error) {
	if db == nil {
		return 0, 0, fmt.Errorf("database connection is nil")
	}
	if batchSize <= 0 {
		return 0, 0, fmt.Errorf("usage dedupe key backfill batch size must be positive")
	}
	scanned, keyed := 0, 0
	var cursor uint
	for {
		var rows []UsageRecord
		if errRows := db.Select("id", "payload").Where("id > ? AND dedupe_key = ''", cursor).Order("id ASC").Limit(batchSize).Find(&rows).Error; errRows != nil {
			return scanned, keyed, errRows
		}
		if len(rows) == 0 {
			return scanned, keyed, nil
		}
		scanned += len(rows)
		cursor = rows[len(rows)-1].ID

		// Keep the first row of each key inside the batch: the update statement
		// cannot see its own writes, so a within-batch duplicate would violate
		// the unique index instead of being skipped by NOT EXISTS.
		seen := make(map[string]struct{}, len(rows))
		args := make([]any, 0, len(rows)*2)
		selects := make([]string, 0, len(rows))
		for _, row := range rows {
			key, errKey := usagePayloadDedupeKey(string(row.PayloadJSON))
			if errKey != nil {
				return scanned, keyed, fmt.Errorf("derive dedupe key for usage %d: %w", row.ID, errKey)
			}
			if _, duplicate := seen[key]; duplicate {
				continue
			}
			seen[key] = struct{}{}
			selects = append(selects, "SELECT CAST(? AS BIGINT) AS usage_id, CAST(? AS TEXT) AS usage_key")
			args = append(args, row.ID, key)
		}

		// A key that already exists keeps the current row unkeyed: the first keyed
		// row already deduplicates redeliveries of that event. The explicit nonempty
		// predicate lets the lookup use the partial index.
		update := db.Exec(
			`UPDATE "usage" SET "dedupe_key" = v.usage_key FROM (`+strings.Join(selects, " UNION ALL ")+`) AS v `+
				`WHERE "usage"."id" = v.usage_id AND "usage"."dedupe_key" = '' `+
				`AND NOT EXISTS (SELECT 1 FROM "usage" AS keyed WHERE keyed."dedupe_key" <> '' AND keyed."dedupe_key" = v.usage_key)`,
			args...,
		)
		if update.Error != nil {
			return scanned, keyed, update.Error
		}
		keyed += int(update.RowsAffected)
		if len(rows) < batchSize {
			return scanned, keyed, nil
		}
	}
}
