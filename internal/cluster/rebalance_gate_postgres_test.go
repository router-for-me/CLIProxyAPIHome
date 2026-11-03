package cluster

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"testing"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func TestPostgresRebalanceLocalWaitDoesNotBlockElection(t *testing.T) {
	for _, action := range []string{"planner", "RESULT", "CLAIM"} {
		t.Run(action, func(t *testing.T) {
			f := seedRebalanceFixture(t, newPostgresQuiescenceRepository(t), 8)
			f.reconcile(t, 0)
			plan := f.plans(t)[0]
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			if action == "planner" {
				if errUpdate := f.repo.db.Model(&plan).Update("expires_at", time.Unix(1, 0)).Error; errUpdate != nil {
					t.Fatal(errUpdate)
				}
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
			if action == "CLAIM" {
				if errLock := holder.Clauses(clause.Locking{Strength: "UPDATE"}).First(&CPARebalancePlanRecord{}, "id = ?", plan.ID).Error; errLock != nil {
					t.Fatal(errLock)
				}
			} else if errLock := holder.Clauses(clause.Locking{Strength: "NO KEY UPDATE"}).First(&CPARebalanceClientRecord{}, "fingerprint = ?", plan.Fingerprint).Error; errLock != nil {
				t.Fatal(errLock)
			}
			holderPID, contenderPID := make(chan postgresBackendPIDResult, 1), make(chan postgresBackendPIDResult, 1)
			if errPID := postgresContenderBackendPIDHook(holderPID)(holder); errPID != nil {
				t.Fatal(errPID)
			}
			done := make(chan error, 1)
			joined := make(chan struct{})
			go func() {
				defer close(joined)
				done <- f.repo.db.WithContext(ctx).Connection(func(conn *gorm.DB) error {
					if errPID := postgresContenderBackendPIDHook(contenderPID)(conn); errPID != nil {
						return errPID
					}
					repo := NewRepository(conn)
					if action == "planner" {
						return repo.ReconcileRebalances(ctx, f.homes[0], defaultHeartbeatTimeout)
					}
					handler := *f.handlers[1]
					handler.repo = repo
					args := []string{"CLUSTER", "BALANCE", "RESULT", plan.ID, "failed"}
					if action == "CLAIM" {
						args = []string{"CLUSTER", "BALANCE", "CLAIM", plan.ID, strconv.FormatInt(plan.Term, 10), plan.TargetIP, strconv.Itoa(plan.TargetPort)}
					}
					payload, errHandle := handler.HandleRebalance(ctx, f.lifetime(plan), args)
					if errHandle != nil {
						return errHandle
					}
					var response struct {
						OK bool `json:"ok"`
					}
					if errJSON := json.Unmarshal(payload, &response); errJSON != nil {
						return errJSON
					}
					if !response.OK {
						return errors.New("rebalance command rejected after local wait")
					}
					return nil
				})
			}()
			t.Cleanup(func() { cancel(); <-joined })
			waitForPostgresContendersBlockedByHolder(t, ctx, f.repo, holderPID, contenderPID)
			// The contender owns the independent mutex and legacy KEY SHARE guard,
			// but has not acquired the final election fence.
			heartbeatCtx, cancelHeartbeat := context.WithTimeout(ctx, 3*time.Second)
			errBeat := f.coordinators[0].heartbeatAndElect(heartbeatCtx, f.homes[0])
			cancelHeartbeat()
			if errBeat != nil {
				t.Fatalf("local %s wait blocked election: %v", action, errBeat)
			}
			if errRollback := holder.Rollback().Error; errRollback != nil {
				t.Fatal(errRollback)
			}
			if errDone := <-done; errDone != nil {
				t.Fatal(errDone)
			}
		})
	}
}

func TestPostgresRebalanceFinalFenceRejectsConcurrentElection(t *testing.T) {
	for _, action := range []string{"planner", "maintenance", "CLAIM"} {
		t.Run(action, func(t *testing.T) {
			f := seedRebalanceFixture(t, newPostgresQuiescenceRepository(t), 8)
			var plan CPARebalancePlanRecord
			if action != "planner" {
				f.reconcile(t, 0)
				plan = f.plans(t)[0]
			}
			if action == "maintenance" {
				if errUpdate := f.repo.db.Model(&plan).Update("expires_at", time.Unix(1, 0)).Error; errUpdate != nil {
					t.Fatal(errUpdate)
				}
			}
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			ready, resume := make(chan struct{}), make(chan struct{})
			operation := "rebalance_plan"
			if action == "CLAIM" {
				operation = "rebalance_claim"
			}
			if errRegister := f.repo.db.Callback().Query().Before("gorm:query").Register("test:pause-final-fence", func(tx *gorm.DB) {
				trace, ok := tx.Logger.(*gateTransactionTrace)
				locking, locked := tx.Statement.Clauses["FOR"].Expression.(clause.Locking)
				if ok && trace.operation == operation && tx.Statement.Table == "cluster_master_gate" && locked && locking.Strength == "NO KEY UPDATE" {
					close(ready)
					select {
					case <-resume:
					case <-ctx.Done():
						tx.AddError(ctx.Err())
					}
				}
			}); errRegister != nil {
				t.Fatal(errRegister)
			}
			done := make(chan error, 1)
			go func() {
				if action != "CLAIM" {
					done <- f.repo.ReconcileRebalances(ctx, f.homes[0], defaultHeartbeatTimeout)
					return
				}
				payload, errHandle := f.handlers[1].HandleRebalance(ctx, f.lifetime(plan), []string{"CLUSTER", "BALANCE", "CLAIM", plan.ID, strconv.FormatInt(plan.Term, 10), plan.TargetIP, strconv.Itoa(plan.TargetPort)})
				if errHandle == nil {
					var response struct {
						OK bool `json:"ok"`
					}
					errHandle = json.Unmarshal(payload, &response)
					if response.OK {
						errHandle = errors.New("old-term claim was authorized")
					}
				}
				done <- errHandle
			}()
			joined := false
			defer func() {
				cancel()
				if !joined {
					<-done
				}
			}()
			select {
			case <-ready:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if errUpdate := f.repo.db.Model(&ClusterNodeRecord{}).Where("ip = ?", f.homes[0].IP).Update("last_seen_at", time.Unix(1, 0)).Error; errUpdate != nil {
				t.Fatal(errUpdate)
			}
			if errBeat := f.coordinators[1].heartbeatAndElect(ctx, f.homes[1]); errBeat != nil {
				t.Fatal(errBeat)
			}
			close(resume)
			errDone := <-done
			joined = true
			if errDone != nil {
				t.Fatal(errDone)
			}
			plans := f.plans(t)
			if action == "planner" && len(plans) != 0 {
				t.Fatalf("old master committed speculative plans: %+v", plans)
			}
			for _, saved := range plans {
				if saved.State != rebalancePending {
					t.Fatalf("failed fence did not roll back %s: %+v", action, saved)
				}
			}
		})
	}
}

func TestPostgresRebalanceLegacyGuardAndQueuedWriter(t *testing.T) {
	f := seedRebalanceFixture(t, newPostgresQuiescenceRepository(t), 0)
	ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
	defer cancel()
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
	holderPID, legacyPID := make(chan postgresBackendPIDResult, 1), make(chan postgresBackendPIDResult, 1)
	if errPID := postgresContenderBackendPIDHook(holderPID)(holder); errPID != nil {
		t.Fatal(errPID)
	}
	done := make(chan error, 1)
	go func() {
		done <- f.repo.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			if errPID := postgresContenderBackendPIDHook(legacyPID)(tx); errPID != nil {
				return errPID
			}
			// This is the old Home's only serialization point. It must not enter
			// while the new planner is still computing or changing plan rows.
			return tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&ClusterMasterGateRecord{}, "id = ?", 1).Error
		})
	}()
	joined := false
	defer func() {
		cancel()
		if !joined {
			<-done
		}
	}()
	waitForPostgresContendersBlockedByHolder(t, ctx, f.repo, holderPID, legacyPID)
	// Upgrading our own KEY SHARE lock must not deadlock behind the queued
	// legacy writer that is waiting for this same transaction to finish.
	if _, _, errFence := lockMasterGate(holder); errFence != nil {
		t.Fatal(errFence)
	}
	if errCommit := holder.Commit().Error; errCommit != nil {
		t.Fatal(errCommit)
	}
	errDone := <-done
	joined = true
	if errDone != nil {
		t.Fatal(errDone)
	}
}
