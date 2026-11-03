package cluster

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func TestPostgresRebalanceLifetimeWaitDoesNotBlockElection(t *testing.T) {
	for _, blockedTable := range []string{"membership", "home"} {
		t.Run(blockedTable, func(t *testing.T) {
			f := seedRebalanceFixture(t, newPostgresQuiescenceRepository(t), 8)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			lifetime := f.lifetimes[0]
			holder := f.repo.db.WithContext(ctx).Begin()
			if holder.Error != nil {
				t.Fatal(holder.Error)
			}
			defer func() {
				if errRollback := holder.Rollback().Error; errRollback != nil && !errors.Is(errRollback, sql.ErrTxDone) {
					t.Error(errRollback)
				}
			}()
			if blockedTable == "membership" {
				var member CPANodeMembershipRecord
				if errLock := holder.Clauses(clause.Locking{Strength: "UPDATE"}).First(&member, "certificate_fingerprint = ?", lifetime.Fingerprint).Error; errLock != nil {
					t.Fatal(errLock)
				}
			} else {
				var home HomeProcessIncarnationRecord
				if errLock := holder.Clauses(clause.Locking{Strength: "UPDATE"}).First(&home, "home_ip = ? AND home_port = ? AND started_at = ?", lifetime.Home.IP, lifetime.Home.Port, lifetime.Home.StartedAt).Error; errLock != nil {
					t.Fatal(errLock)
				}
			}
			holderPID, contenderPID := make(chan postgresBackendPIDResult, 1), make(chan postgresBackendPIDResult, 1)
			if errPID := postgresContenderBackendPIDHook(holderPID)(holder); errPID != nil {
				t.Fatal(errPID)
			}
			result := make(chan error, 1)
			done := make(chan struct{})
			go func() {
				defer close(done)
				result <- f.repo.db.WithContext(ctx).Connection(func(conn *gorm.DB) error {
					if errPID := postgresContenderBackendPIDHook(contenderPID)(conn); errPID != nil {
						return errPID
					}
					handler := *f.handlers[1]
					handler.repo = NewRepository(conn)
					payload, errHandle := handler.HandleRebalance(ctx, lifetime, []string{"CLUSTER", "BALANCE", "ENABLE"})
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
						return ErrConcurrencyNodeUnavailable
					}
					return nil
				})
			}()
			t.Cleanup(func() { cancel(); <-done })
			waitForPostgresContendersBlockedByHolder(t, ctx, f.repo, holderPID, contenderPID)
			// A different Home must renew/elect while BALANCE remains blocked locally.
			heartbeatCtx, cancelHeartbeat := context.WithTimeout(ctx, 3*time.Second)
			errBeat := f.coordinators[0].heartbeatAndElect(heartbeatCtx, f.homes[0])
			cancelHeartbeat()
			if errBeat != nil {
				t.Fatalf("unrelated election blocked by %s lock: %v", blockedTable, errBeat)
			}
			select {
			case errResult := <-result:
				t.Fatalf("BALANCE did not wait for the lifetime lock: %v", errResult)
			default:
			}
			if errRollback := holder.Rollback().Error; errRollback != nil {
				t.Fatal(errRollback)
			}
			select {
			case errResult := <-result:
				if errResult != nil {
					t.Fatal(errResult)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		})
	}
}

func TestPostgresRebalanceInstructionDoesNotWaitForElectionGate(t *testing.T) {
	f := seedRebalanceFixture(t, newPostgresQuiescenceRepository(t), 8)
	f.reconcile(t, 0)
	plan := f.plans(t)[0]
	lifetime := f.lifetime(plan)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var payload []byte
	errTransaction := f.repo.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if _, _, errGate := lockMasterGate(tx); errGate != nil {
			return errGate
		}
		// Poll through a separate connection while the election gate remains locked.
		var errInstruction error
		payload, errInstruction = f.handlers[1].RebalanceInstruction(ctx, lifetime)
		return errInstruction
	})
	if errTransaction != nil {
		t.Fatalf("instruction polling while the election gate is locked: %v", errTransaction)
	}
	var instruction RebalanceInstruction
	if errJSON := json.Unmarshal(payload, &instruction); errJSON != nil {
		t.Fatal(errJSON)
	}
	if instruction.ID != plan.ID || instruction.Term != plan.Term {
		t.Fatalf("unexpected instruction: %+v", instruction)
	}
}

func TestPostgresRebalanceSerializesPlannersAndDuplicateClaims(t *testing.T) {
	f := seedRebalanceFixture(t, newPostgresQuiescenceRepository(t), 8)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	start := make(chan struct{})
	results := make(chan error, 4)
	var workers sync.WaitGroup
	for range 4 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			results <- f.repo.ReconcileRebalances(ctx, f.homes[0], defaultHeartbeatTimeout)
		}()
	}
	close(start)
	workers.Wait()
	for range 4 {
		if errResult := <-results; errResult != nil {
			t.Fatal(errResult)
		}
	}
	plans := f.plans(t)
	if len(plans) != 2 {
		t.Fatalf("concurrent planners created %d plans", len(plans))
	}
	plan := plans[0]
	args := []string{"CLUSTER", "BALANCE", "CLAIM", plan.ID, strconv.FormatInt(plan.Term, 10), plan.TargetIP, strconv.Itoa(plan.TargetPort)}
	type claimResult struct {
		accepted bool
		err      error
	}
	claims := make(chan claimResult, 4)
	start = make(chan struct{})
	for range 4 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			payload, errHandle := f.handlers[1].HandleRebalance(ctx, f.lifetime(plan), args)
			var response struct {
				OK bool `json:"ok"`
			}
			if errHandle == nil {
				errHandle = json.Unmarshal(payload, &response)
			}
			claims <- claimResult{accepted: response.OK, err: errHandle}
		}()
	}
	close(start)
	workers.Wait()
	accepted := 0
	for range 4 {
		result := <-claims
		if result.err != nil {
			t.Fatal(result.err)
		}
		if result.accepted {
			accepted++
		}
	}
	if accepted != 1 {
		t.Fatalf("accepted %d duplicate claims, want 1", accepted)
	}
}
