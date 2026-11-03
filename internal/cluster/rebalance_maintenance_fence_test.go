package cluster

import (
	"context"
	"errors"
	"testing"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func TestRebalanceMaintenanceWritesRequireFence(t *testing.T) {
	for _, backend := range []struct {
		name string
		repo func(*testing.T) *Repository
	}{{"sqlite", newCredentialFoundationTestRepository}, {"postgres", newPostgresQuiescenceRepository}} {
		t.Run(backend.name, func(t *testing.T) {
			for _, rejectFence := range []bool{false, true} {
				name := "commit"
				if rejectFence {
					name = "rollback"
				}
				t.Run(name, func(t *testing.T) {
					f := seedRebalanceFixture(t, backend.repo(t), 8)
					f.reconcile(t, 0)
					plan := f.plans(t)[0]
					now, errNow := DatabaseNow(t.Context(), f.repo.db)
					if errNow != nil {
						t.Fatal(errNow)
					}
					if errUpdate := f.repo.db.Model(&ClusterMasterGateRecord{}).Where("id = ?", 1).Update("last_balance_at", now).Error; errUpdate != nil {
						t.Fatal(errUpdate)
					}
					if errUpdate := f.repo.db.Model(&plan).Update("expires_at", now.Add(-time.Second)).Error; errUpdate != nil {
						t.Fatal(errUpdate)
					}
					// Keep the run strictly inside the planning interval without relying on elapsed test time.
					if errRegister := f.repo.db.Callback().Row().Before("gorm:row").Register("test:maintenance-clock", func(tx *gorm.DB) {
						if tx.Statement.SQL.String() == databaseNowQuery(tx) {
							tx.Statement.SQL.Reset()
							tx.Statement.Vars = nil
							clause.Expr{SQL: "SELECT ?", Vars: []any{now.UTC().Format(time.RFC3339Nano)}}.Build(tx.Statement)
						}
					}); errRegister != nil {
						t.Fatal(errRegister)
					}
					failure := errors.New("test maintenance fence failure")
					fences := 0
					if errRegister := f.repo.db.Callback().Query().Before("gorm:query").Register("test:maintenance-fence", func(tx *gorm.DB) {
						locking, ok := tx.Statement.Clauses["FOR"].Expression.(clause.Locking)
						if tx.Statement.Table == "cluster_master_gate" && ok && locking.Strength == "NO KEY UPDATE" {
							fences++
							if rejectFence {
								tx.AddError(failure)
							}
						}
					}); errRegister != nil {
						t.Fatal(errRegister)
					}
					errReconcile := f.repo.ReconcileRebalances(t.Context(), f.homes[0], defaultHeartbeatTimeout)
					if (rejectFence && !errors.Is(errReconcile, failure)) || (!rejectFence && errReconcile != nil) || fences != 1 {
						t.Fatalf("maintenance fence: calls=%d error=%v reject=%t", fences, errReconcile, rejectFence)
					}
					var saved CPARebalancePlanRecord
					if errFirst := f.repo.db.First(&saved, "id = ?", plan.ID).Error; errFirst != nil {
						t.Fatal(errFirst)
					}
					var client CPARebalanceClientRecord
					if errFirst := f.repo.db.First(&client, "fingerprint = ?", plan.Fingerprint).Error; errFirst != nil {
						t.Fatal(errFirst)
					}
					wantState, wantBackoff := rebalanceFailed, time.Minute
					if rejectFence {
						wantState, wantBackoff = rebalancePending, 0
					}
					if saved.State != wantState || client.FailureBackoff != wantBackoff {
						t.Fatalf("plan/client writes were not atomic: state=%s backoff=%s", saved.State, client.FailureBackoff)
					}
					var gate ClusterMasterGateRecord
					if errFirst := f.repo.db.First(&gate, "id = ?", 1).Error; errFirst != nil {
						t.Fatal(errFirst)
					}
					if !gate.LastBalanceAt.Equal(now) {
						t.Fatal("maintenance advanced the planning cadence")
					}
				})
			}
		})
	}
}

func TestPostgresRebalanceReadOnlyMaintenanceDoesNotWaitForElection(t *testing.T) {
	for _, count := range []int{0, 8} {
		name := "empty"
		if count > 0 {
			name = "unchanged plans"
		}
		t.Run(name, func(t *testing.T) {
			f := seedRebalanceFixture(t, newPostgresQuiescenceRepository(t), count)
			f.reconcile(t, 0)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			// Keep a new election's writer lock held on a separate connection.
			// Read-only maintenance must finish using just the compatible KEY SHARE guard.
			if errTransaction := f.repo.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
				if _, _, errLock := lockMasterGate(tx); errLock != nil {
					return errLock
				}
				maintenanceCtx, cancelMaintenance := context.WithTimeout(ctx, 3*time.Second)
				defer cancelMaintenance()
				return f.repo.ReconcileRebalances(maintenanceCtx, f.homes[0], defaultHeartbeatTimeout)
			}); errTransaction != nil {
				t.Fatalf("read-only maintenance waited for election: %v", errTransaction)
			}
		})
	}
}
