package cluster

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"gorm.io/gorm"
)

func TestPostgresRebalanceCleanupSkipsLocksAndElectionGate(t *testing.T) {
	f := seedRebalanceFixture(t, newPostgresQuiescenceRepository(t), 0)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	now, errNow := DatabaseNow(ctx, f.repo.db)
	if errNow != nil {
		t.Fatal(errNow)
	}
	records := []CPARebalancePlanRecord{
		{ID: "held", State: rebalanceFailed, UpdatedAt: now.Add(-25 * time.Hour)},
		{ID: "free", State: rebalanceCompleted, UpdatedAt: now.Add(-25 * time.Hour)},
	}
	if errCreate := f.repo.db.Create(&records).Error; errCreate != nil {
		t.Fatal(errCreate)
	}
	holder := f.repo.db.WithContext(ctx).Begin()
	if holder.Error != nil {
		t.Fatal(holder.Error)
	}
	defer func() {
		if errRollback := holder.Rollback().Error; errRollback != nil && !errors.Is(errRollback, sql.ErrTxDone) {
			t.Error(errRollback)
		}
	}()
	if _, _, errGate := lockRebalanceGate(holder); errGate != nil {
		t.Fatal(errGate)
	}
	if _, _, errGate := lockMasterGate(holder); errGate != nil {
		t.Fatal(errGate)
	}
	// The cleaner sees the old terminal version but must skip its uncommitted writer.
	if errUpdate := holder.Model(&records[0]).Updates(map[string]any{"state": rebalanceExecuting, "updated_at": now}).Error; errUpdate != nil {
		t.Fatal(errUpdate)
	}
	cleanupCtx, cancelCleanup := context.WithTimeout(ctx, 3*time.Second)
	deleted, errPurge := f.repo.PurgeExpiredCPARebalancePlans(cleanupCtx, now, 1000)
	cancelCleanup()
	if errPurge != nil || deleted != 1 {
		t.Fatalf("cleanup waited for election or plan locks: deleted=%d error=%v", deleted, errPurge)
	}
	if errCommit := holder.Commit().Error; errCommit != nil {
		t.Fatal(errCommit)
	}
	if deleted, errPurge := f.repo.PurgeExpiredCPARebalancePlans(ctx, now, 1000); errPurge != nil || deleted != 0 {
		t.Fatalf("cleanup removed a renewed active plan: deleted=%d error=%v", deleted, errPurge)
	}
}

func TestPostgresRebalancePlanCleanupRechecksStateAndRetention(t *testing.T) {
	for _, renewState := range []bool{false, true} {
		t.Run(fmt.Sprintf("renew-state=%t", renewState), func(t *testing.T) {
			repo := newPostgresQuiescenceRepository(t)
			now, errNow := DatabaseNow(t.Context(), repo.db)
			if errNow != nil {
				t.Fatal(errNow)
			}
			record := CPARebalancePlanRecord{ID: "renewed", State: rebalanceCompleted, UpdatedAt: now.Add(-25 * time.Hour)}
			if errCreate := repo.db.Create(&record).Error; errCreate != nil {
				t.Fatal(errCreate)
			}
			if errRegister := repo.db.Callback().Delete().Before("gorm:delete").Register("test:renew-plan-before-delete", func(tx *gorm.DB) {
				updates := map[string]any{"updated_at": now}
				if renewState {
					updates = map[string]any{"state": rebalancePending}
				}
				if errUpdate := tx.Session(&gorm.Session{NewDB: true}).Model(&record).Updates(updates).Error; errUpdate != nil {
					tx.AddError(errUpdate)
				}
			}); errRegister != nil {
				t.Fatal(errRegister)
			}
			if deleted, errPurge := repo.PurgeExpiredCPARebalancePlans(t.Context(), now, 1); errPurge != nil || deleted != 0 {
				t.Fatalf("cleanup discarded a renewed plan: deleted=%d error=%v", deleted, errPurge)
			}
		})
	}
}
