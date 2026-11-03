package cluster

import (
	"context"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// PurgeExpiredCPARebalancePlans collects only old terminal history, independently
// of election and planning. Active-plan expiry and swap transitions stay in the planner.
func (r *Repository) PurgeExpiredCPARebalancePlans(ctx context.Context, _ time.Time, limit int) (int64, error) {
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
		terminal := []string{rebalanceCompleted, rebalanceFailed, rebalanceCanceled}
		keys := tx.Model(&CPARebalancePlanRecord{}).Select("id").
			Where("state IN ? AND updated_at < ?", terminal, cutoff).
			Order("updated_at, id").Limit(min(limit, expiredCleanupBatch))
		if tx.Dialector.Name() == "postgres" {
			keys = keys.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"})
		}
		result := tx.Where("id IN (?) AND state IN ? AND updated_at < ?", keys, terminal, cutoff).Delete(&CPARebalancePlanRecord{})
		deleted = result.RowsAffected
		return result.Error
	})
	if errTransaction != nil {
		return 0, errTransaction
	}
	return deleted, nil
}
