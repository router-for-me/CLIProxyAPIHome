package cluster

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"gorm.io/gorm"
)

func TestRebalancePlanCleanupBatchesAndRetention(t *testing.T) {
	for _, backend := range []struct {
		name string
		repo func(*testing.T) *Repository
	}{{"sqlite", newCredentialFoundationTestRepository}, {"postgres", newPostgresQuiescenceRepository}} {
		t.Run(backend.name, func(t *testing.T) {
			repo := backend.repo(t)
			now, errNow := DatabaseNow(t.Context(), repo.db)
			if errNow != nil {
				t.Fatal(errNow)
			}
			records := make([]CPARebalancePlanRecord, 1002)
			terminal := []string{rebalanceCompleted, rebalanceFailed, rebalanceCanceled}
			for index := range records {
				records[index] = CPARebalancePlanRecord{ID: fmt.Sprintf("old-%04d", index), State: terminal[index%len(terminal)], UpdatedAt: now.Add(-25 * time.Hour)}
			}
			records = append(records,
				CPARebalancePlanRecord{ID: "pending", State: rebalancePending, UpdatedAt: now.Add(-25 * time.Hour)},
				CPARebalancePlanRecord{ID: "executing", State: rebalanceExecuting, UpdatedAt: now.Add(-25 * time.Hour)},
				CPARebalancePlanRecord{ID: "recent", State: rebalanceCompleted, UpdatedAt: now.Add(-23 * time.Hour)},
				CPARebalancePlanRecord{ID: "unknown", State: "future-state", UpdatedAt: now.Add(-25 * time.Hour)})
			if errCreate := repo.db.CreateInBatches(&records, 100).Error; errCreate != nil {
				t.Fatal(errCreate)
			}
			for _, limit := range []int{0, -1} {
				if deleted, errPurge := repo.PurgeExpiredCPARebalancePlans(t.Context(), now, limit); errPurge != nil || deleted != 0 {
					t.Fatalf("non-positive limit: deleted=%d error=%v", deleted, errPurge)
				}
			}
			for index, want := range []int64{1, 1000, 1, 0} {
				limit := 5000
				if index == 0 {
					limit = 1
				}
				// Caller wall time must not change database-backed retention.
				deleted, errPurge := repo.PurgeExpiredCPARebalancePlans(t.Context(), now.Add(72*time.Hour), limit)
				if errPurge != nil || deleted != want {
					t.Fatalf("deleted=%d error=%v, want %d", deleted, errPurge, want)
				}
			}
			var saved []CPARebalancePlanRecord
			if errFind := repo.db.Find(&saved).Error; errFind != nil {
				t.Fatal(errFind)
			}
			if len(saved) != 4 {
				t.Fatalf("cleanup removed active, recent, or unknown states: %+v", saved)
			}
		})
	}
}

func TestRebalancePlannerLeavesHistoricalPlansForCleanup(t *testing.T) {
	f := newRebalanceFixture(t, 0)
	now, errNow := DatabaseNow(t.Context(), f.repo.db)
	if errNow != nil {
		t.Fatal(errNow)
	}
	old := CPARebalancePlanRecord{ID: "historical", State: rebalanceCompleted, UpdatedAt: now.Add(-25 * time.Hour)}
	if errCreate := f.repo.db.Create(&old).Error; errCreate != nil {
		t.Fatal(errCreate)
	}
	f.reconcile(t, 0)
	if errFirst := f.repo.db.First(&old, "id = ?", old.ID).Error; errFirst != nil {
		t.Fatal("planner still garbage collected history: ", errFirst)
	}
	if deleted, errPurge := f.repo.PurgeExpiredCPARebalancePlans(t.Context(), now, 1000); errPurge != nil || deleted != 1 {
		t.Fatalf("historical cleanup deleted=%d error=%v", deleted, errPurge)
	}
}

func TestRebalancePlanCleanupPropagatesClockFailure(t *testing.T) {
	repo := newCredentialFoundationTestRepository(t)
	failure := errors.New("test rebalance cleanup clock failure")
	if errRegister := repo.db.Callback().Row().Before("gorm:row").Register("test:rebalance-cleanup-clock", func(tx *gorm.DB) {
		if tx.Statement.SQL.String() == databaseNowQuery(tx) {
			tx.AddError(failure)
		}
	}); errRegister != nil {
		t.Fatal(errRegister)
	}
	if deleted, errPurge := repo.PurgeExpiredCPARebalancePlans(t.Context(), time.Time{}, 1); !errors.Is(errPurge, failure) || deleted != 0 {
		t.Fatalf("clock failure ignored: deleted=%d error=%v", deleted, errPurge)
	}
}

func TestCoordinatorMasterCleansExpiredRebalancePlans(t *testing.T) {
	repo := newCredentialFoundationTestRepository(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	coordinator := NewCoordinator(repo, NodeIdentity{IP: "127.0.0.1", Port: 8327}, CoordinatorOptions{HeartbeatInterval: time.Hour, HeartbeatTimeout: time.Hour})
	if errInitialize := coordinator.Initialize(ctx); errInitialize != nil {
		t.Fatal(errInitialize)
	}
	defer coordinator.setMaster(false)
	now, errNow := DatabaseNow(ctx, repo.db)
	if errNow != nil {
		t.Fatal(errNow)
	}
	if errCreate := repo.db.Create(&CPARebalancePlanRecord{ID: "old", State: rebalanceFailed, UpdatedAt: now.Add(-25 * time.Hour)}).Error; errCreate != nil {
		t.Fatal(errCreate)
	}
	purged := make(chan struct{}, 1)
	if errRegister := repo.db.Callback().Delete().After("gorm:commit_or_rollback_transaction").Register("test:rebalance-background-cleanup", func(tx *gorm.DB) {
		if tx.Statement.Table == "cpa_rebalance_plans" && tx.Error == nil && tx.RowsAffected == 1 {
			purged <- struct{}{}
		}
	}); errRegister != nil {
		t.Fatal(errRegister)
	}
	done := make(chan error, 1)
	go func() { done <- coordinator.Start(ctx) }()
	defer func() {
		cancel()
		if errStart := <-done; errStart != nil {
			t.Error(errStart)
		}
	}()
	select {
	case <-purged:
	case <-ctx.Done():
		t.Fatal("master did not start historical plan cleanup")
	}
}
