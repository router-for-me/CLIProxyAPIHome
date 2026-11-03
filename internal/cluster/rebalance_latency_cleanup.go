package cluster

import (
	"context"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// PurgeExpiredCPALatencySummaries removes a bounded batch independently of the
// election gate. The callback's wall clock is deliberately ignored: retention
// uses database time, just like the reports that populate updated_at.
func (r *Repository) PurgeExpiredCPALatencySummaries(ctx context.Context, _ time.Time, limit int) (int64, error) {
	if limit <= 0 {
		return 0, nil
	}
	db, errDB := r.database()
	if errDB != nil {
		return 0, errDB
	}
	var deleted int64
	errTransaction := db.WithContext(contextOrBackground(ctx)).Transaction(func(tx *gorm.DB) error {
		now, errNow := DatabaseNow(tx.Statement.Context, tx)
		if errNow != nil {
			return errNow
		}
		cutoff := now.Add(-24 * time.Hour)
		keys := tx.Model(&CPALatencyRecord{}).Select("fingerprint", "address").
			Where("updated_at < ?", cutoff).Order("updated_at, fingerprint, address").Limit(min(limit, expiredCleanupBatch))
		if tx.Dialector.Name() == "postgres" {
			// An in-flight report owns its summary row; skip it rather than delay either task.
			keys = keys.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"})
		}
		// Keep the full composite identity and recheck expiry at physical deletion.
		result := tx.Where("(fingerprint, address) IN (?) AND updated_at < ?", keys, cutoff).Delete(&CPALatencyRecord{})
		deleted = result.RowsAffected
		return result.Error
	})
	if errTransaction != nil {
		return 0, errTransaction
	}
	return deleted, nil
}

// PurgeExpiredCPALatencyConfirmations only collects abandoned confirmations.
// Planning still owns all cancellation, evidence resets, and completion changes.
func (r *Repository) PurgeExpiredCPALatencyConfirmations(ctx context.Context, _ time.Time, limit int) (int64, error) {
	if limit <= 0 {
		return 0, nil
	}
	db, errDB := r.database()
	if errDB != nil {
		return 0, errDB
	}
	var deleted int64
	errTransaction := db.WithContext(contextOrBackground(ctx)).Transaction(func(tx *gorm.DB) error {
		now, errNow := DatabaseNow(tx.Statement.Context, tx)
		if errNow != nil {
			return errNow
		}
		cutoff := now.Add(-rebalanceLatencyCooldown)
		keys := tx.Model(&CPALatencyConfirmationRecord{}).Select("id").
			Where("updated_at < ?", cutoff).Order("updated_at, id").Limit(min(limit, expiredCleanupBatch))
		if tx.Dialector.Name() == "postgres" {
			keys = keys.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"})
		}
		result := tx.Where("id IN (?) AND updated_at < ?", keys, cutoff).Delete(&CPALatencyConfirmationRecord{})
		deleted = result.RowsAffected
		return result.Error
	})
	if errTransaction != nil {
		return 0, errTransaction
	}
	return deleted, nil
}
