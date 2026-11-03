package cluster

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// postgresLatencyReport validates the wire response as well as database errors in concurrent tests.
func postgresLatencyReport(ctx context.Context, handler *RESPHandler, lifetime ConnectionLifetime, sequence int64, observedAt time.Time, valid bool) error {
	report := latencyReport{ProcessID: "report-process", Samples: []latencySample{{
		IP: lifetime.Home.IP, Port: lifetime.Home.Port, Sequence: sequence,
		ObservedAt: observedAt, RTTUS: 100000, Valid: valid,
	}}}
	raw, errJSON := json.Marshal(report)
	if errJSON != nil {
		return errJSON
	}
	payload, errHandle := handler.HandleRebalance(ctx, lifetime, []string{"CLUSTER", "BALANCE", "LATENCY", string(raw)})
	if errHandle != nil {
		return errHandle
	}
	var response struct {
		OK bool `json:"ok"`
	}
	if errDecode := json.Unmarshal(payload, &response); errDecode != nil {
		return errDecode
	}
	if !response.OK {
		return fmt.Errorf("latency report rejected for %s", lifetime.Fingerprint)
	}
	return nil
}

func TestPostgresLatencyProbeDoesNotWaitForConfirmationWriter(t *testing.T) {
	f := seedRebalanceFixture(t, newPostgresQuiescenceRepository(t), 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	now, errNow := DatabaseNow(ctx, f.repo.db)
	if errNow != nil {
		t.Fatal(errNow)
	}
	peer := f.members[0]
	peer.CertificateFingerprint = "peer"
	peer.HomeIP, peer.HomePort, peer.HomeStartedAt = f.homes[2].IP, f.homes[2].Port, f.homes[2].StartedAt
	confirmation := CPALatencyConfirmationRecord{ID: "active", Signature: latencySwapSignature(f.members[0], peer), UpdatedAt: now}
	if errCreate := f.repo.db.Create(&confirmation).Error; errCreate != nil {
		t.Fatal(errCreate)
	}
	if errTransaction := f.repo.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if _, _, errGate := lockRebalanceGate(tx); errGate != nil {
			return errGate
		}
		if errUpdate := tx.Model(&confirmation).Update("updated_at", now.Add(time.Second)).Error; errUpdate != nil {
			return errUpdate
		}
		// A planner holds the gate and writes confirmation; telemetry must use an unlocked
		// snapshot rather than wait while holding the CPA row the planner may need next.
		reportCtx, cancelReport := context.WithTimeout(ctx, 3*time.Second)
		defer cancelReport()
		return postgresLatencyReport(reportCtx, f.handlers[1], f.lifetimes[0], 1, now.Add(-time.Second), true)
	}); errTransaction != nil {
		t.Fatalf("probe selection waited for the confirmation writer: %v", errTransaction)
	}
}

func TestPostgresLatencyReportDoesNotWaitForElectionGate(t *testing.T) {
	f := seedRebalanceFixture(t, newPostgresQuiescenceRepository(t), 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if errTransaction := f.repo.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		_, now, errGate := lockMasterGate(tx)
		if errGate != nil {
			return errGate
		}
		// This request uses a separate connection while the election gate stays locked.
		reportCtx, cancelReport := context.WithTimeout(ctx, 3*time.Second)
		defer cancelReport()
		return postgresLatencyReport(reportCtx, f.handlers[1], f.lifetimes[0], 1, now.Add(-time.Second), true)
	}); errTransaction != nil {
		t.Fatalf("telemetry waited for the election gate: %v", errTransaction)
	}
	var sample CPALatencyRecord
	if errFirst := f.repo.db.First(&sample, "fingerprint = ?", f.lifetimes[0].Fingerprint).Error; errFirst != nil {
		t.Fatal(errFirst)
	}
	if sample.Sequence != 1 || !sample.Valid {
		t.Fatalf("report did not commit while election gate was locked: %+v", sample)
	}
}

func TestPostgresLatencyReportWaitDoesNotBlockElection(t *testing.T) {
	for _, blockedTable := range []string{"client", "summary"} {
		t.Run(blockedTable, func(t *testing.T) {
			f := seedRebalanceFixture(t, newPostgresQuiescenceRepository(t), 3)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			now, errNow := DatabaseNow(ctx, f.repo.db)
			if errNow != nil {
				t.Fatal(errNow)
			}
			for _, lifetime := range f.lifetimes[:2] {
				if errReport := postgresLatencyReport(ctx, f.handlers[1], lifetime, 1, now.Add(-2*time.Second), true); errReport != nil {
					t.Fatal(errReport)
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
			for _, lifetime := range f.lifetimes[:2] {
				if blockedTable == "client" {
					var client CPARebalanceClientRecord
					if errLock := holder.Clauses(clause.Locking{Strength: "NO KEY UPDATE"}).First(&client, "fingerprint = ?", lifetime.Fingerprint).Error; errLock != nil {
						t.Fatal(errLock)
					}
				} else {
					var sample CPALatencyRecord
					if errLock := holder.Clauses(clause.Locking{Strength: "UPDATE"}).First(&sample, "fingerprint = ?", lifetime.Fingerprint).Error; errLock != nil {
						t.Fatal(errLock)
					}
				}
			}
			holderPID := make(chan postgresBackendPIDResult, 1)
			if errPID := postgresContenderBackendPIDHook(holderPID)(holder); errPID != nil {
				t.Fatal(errPID)
			}
			results := make(chan error, 2)
			contenderPIDs := make([]<-chan postgresBackendPIDResult, 0, 2)
			for _, lifetime := range f.lifetimes[:2] {
				pid := make(chan postgresBackendPIDResult, 1)
				contenderPIDs = append(contenderPIDs, pid)
				done := make(chan struct{})
				go func() {
					defer close(done)
					results <- f.repo.db.WithContext(ctx).Connection(func(conn *gorm.DB) error {
						if errPID := postgresContenderBackendPIDHook(pid)(conn); errPID != nil {
							return errPID
						}
						handler := *f.handlers[1]
						handler.repo = NewRepository(conn)
						return postgresLatencyReport(ctx, &handler, lifetime, 2, now.Add(-time.Second), true)
					})
				}()
				t.Cleanup(func() { cancel(); <-done })
			}
			waitForPostgresContendersBlockedByHolder(t, ctx, f.repo, holderPID, contenderPIDs...)
			// Blocked telemetry must neither stop lease renewal nor serialize other CPAs.
			var before ClusterMasterGateRecord
			if errFirst := f.repo.db.First(&before, "id = ?", 1).Error; errFirst != nil {
				t.Fatal(errFirst)
			}
			heartbeatCtx, cancelHeartbeat := context.WithTimeout(ctx, 3*time.Second)
			errBeat := f.coordinators[0].heartbeatAndElect(heartbeatCtx, f.homes[0])
			cancelHeartbeat()
			if errBeat != nil {
				t.Fatalf("telemetry blocked election on %s: %v", blockedTable, errBeat)
			}
			var after ClusterMasterGateRecord
			if errFirst := f.repo.db.First(&after, "id = ?", 1).Error; errFirst != nil {
				t.Fatal(errFirst)
			}
			if after.Term != before.Term || after.MasterKey != before.MasterKey || after.LeaseUntil.Before(before.LeaseUntil) {
				t.Fatalf("lease renewal changed leadership: before=%+v after=%+v", before, after)
			}
			reportCtx, cancelReport := context.WithTimeout(ctx, 3*time.Second)
			errReport := postgresLatencyReport(reportCtx, f.handlers[1], f.lifetimes[2], 1, now.Add(-time.Second), true)
			cancelReport()
			if errReport != nil {
				t.Fatalf("unrelated CPA report blocked on %s: %v", blockedTable, errReport)
			}
			select {
			case errReport := <-results:
				t.Fatalf("report did not wait for %s lock: %v", blockedTable, errReport)
			default:
			}
			if errRollback := holder.Rollback().Error; errRollback != nil {
				t.Fatal(errRollback)
			}
			for range 2 {
				select {
				case errReport := <-results:
					if errReport != nil {
						t.Fatal(errReport)
					}
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
		})
	}
}

func TestPostgresLatencyReportAndResultPreserveClientState(t *testing.T) {
	f := seedRebalanceFixture(t, newPostgresQuiescenceRepository(t), 8)
	f.reconcile(t, 0)
	plan := f.plans(t)[0]
	if !claimRebalanceTestPlan(t, f, plan) {
		t.Fatal("claim rejected")
	}
	lifetime := f.lifetime(plan)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	now, errNow := DatabaseNow(ctx, f.repo.db)
	if errNow != nil {
		t.Fatal(errNow)
	}
	initial := CPALatencyRecord{Fingerprint: lifetime.Fingerprint, Address: latencyAddress(lifetime.Home.IP, lifetime.Home.Port), ProcessID: "report-process", Sequence: 1, ObservedAt: now.Add(-2 * time.Second), SampledAt: now.Add(-2 * time.Second), RTT: time.Millisecond, Valid: true, UpdatedAt: now}
	if errCreate := f.repo.db.Create(&initial).Error; errCreate != nil {
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
	if errLock := holder.Clauses(clause.Locking{Strength: "UPDATE"}).First(&initial, "fingerprint = ?", lifetime.Fingerprint).Error; errLock != nil {
		t.Fatal(errLock)
	}
	holderPID := make(chan postgresBackendPIDResult, 1)
	if errPID := postgresContenderBackendPIDHook(holderPID)(holder); errPID != nil {
		t.Fatal(errPID)
	}
	reportPID := make(chan postgresBackendPIDResult, 1)
	results := make(chan error, 2)
	for _, action := range []string{"LATENCY", "RESULT"} {
		pid := reportPID
		if action == "RESULT" {
			pid = make(chan postgresBackendPIDResult, 1)
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			results <- f.repo.db.WithContext(ctx).Connection(func(conn *gorm.DB) error {
				if errPID := postgresContenderBackendPIDHook(pid)(conn); errPID != nil {
					return errPID
				}
				handler := *f.handlers[1]
				handler.repo = NewRepository(conn)
				if action == "LATENCY" {
					return postgresLatencyReport(ctx, &handler, lifetime, 2, now.Add(-time.Second), true)
				}
				payload, errHandle := handler.HandleRebalance(ctx, lifetime, []string{"CLUSTER", "BALANCE", "RESULT", plan.ID, "failed"})
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
					return fmt.Errorf("rebalance failure report rejected")
				}
				return nil
			})
		}()
		t.Cleanup(func() { cancel(); <-done })
		if action == "LATENCY" {
			first := waitForPostgresBackendPID(t, ctx, reportPID, "latency report")
			reportPID <- postgresBackendPIDResult{pid: first}
			waitForPostgresContendersBlockedByHolder(t, ctx, f.repo, holderPID, reportPID)
			reportPID <- postgresBackendPIDResult{pid: first}
		} else {
			waitForPostgresContendersBlockedByHolder(t, ctx, f.repo, reportPID, pid)
		}
	}
	if errRollback := holder.Rollback().Error; errRollback != nil {
		t.Fatal(errRollback)
	}
	for range 2 {
		select {
		case errReport := <-results:
			if errReport != nil {
				t.Fatal(errReport)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	var client CPARebalanceClientRecord
	if errFirst := f.repo.db.First(&client, "fingerprint = ?", lifetime.Fingerprint).Error; errFirst != nil {
		t.Fatal(errFirst)
	}
	if !client.LatencyConnectedAt.Equal(lifetime.ConnectedAt) || client.FailureBackoff != time.Minute || !client.NextEligibleAt.After(now) {
		t.Fatalf("concurrent result overwrote telemetry or lost migration backoff: %+v", client)
	}
}

func TestPostgresLatencyReportsSerializePerCPA(t *testing.T) {
	for _, secondSequence := range []int64{2, 3, 4} {
		t.Run(fmt.Sprintf("second-sequence=%d", secondSequence), func(t *testing.T) {
			f := seedRebalanceFixture(t, newPostgresQuiescenceRepository(t), 1)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			lifetime := f.lifetimes[0]
			now, errNow := DatabaseNow(ctx, f.repo.db)
			if errNow != nil {
				t.Fatal(errNow)
			}
			if errReport := postgresLatencyReport(ctx, f.handlers[1], lifetime, 1, now.Add(-4*time.Second), false); errReport != nil {
				t.Fatal(errReport)
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
			var initial CPALatencyRecord
			if errLock := holder.Clauses(clause.Locking{Strength: "UPDATE"}).First(&initial, "fingerprint = ?", lifetime.Fingerprint).Error; errLock != nil {
				t.Fatal(errLock)
			}
			holderPID := make(chan postgresBackendPIDResult, 1)
			if errPID := postgresContenderBackendPIDHook(holderPID)(holder); errPID != nil {
				t.Fatal(errPID)
			}
			results := make(chan error, 2)
			firstPID := make(chan postgresBackendPIDResult, 1)
			for index, sequence := range []int64{3, secondSequence} {
				pid := firstPID
				if index == 1 {
					pid = make(chan postgresBackendPIDResult, 1)
				}
				done := make(chan struct{})
				go func() {
					defer close(done)
					results <- f.repo.db.WithContext(ctx).Connection(func(conn *gorm.DB) error {
						if errPID := postgresContenderBackendPIDHook(pid)(conn); errPID != nil {
							return errPID
						}
						handler := *f.handlers[1]
						handler.repo = NewRepository(conn)
						return postgresLatencyReport(ctx, &handler, lifetime, sequence, now.Add(time.Duration(sequence-5)*time.Second), false)
					})
				}()
				t.Cleanup(func() { cancel(); <-done })
				if index == 0 {
					// The first report has read history and is blocked writing its newer observation.
					first := waitForPostgresBackendPID(t, ctx, firstPID, "first report")
					firstPID <- postgresBackendPIDResult{pid: first}
					waitForPostgresContendersBlockedByHolder(t, ctx, f.repo, holderPID, firstPID)
					firstPID <- postgresBackendPIDResult{pid: first}
				} else {
					// The second report must wait for the first CPA client lock, not read old history.
					waitForPostgresContendersBlockedByHolder(t, ctx, f.repo, firstPID, pid)
				}
			}
			if errRollback := holder.Rollback().Error; errRollback != nil {
				t.Fatal(errRollback)
			}
			for range 2 {
				select {
				case errReport := <-results:
					if errReport != nil {
						t.Fatal(errReport)
					}
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			var sample CPALatencyRecord
			if errFirst := f.repo.db.First(&sample, "fingerprint = ?", lifetime.Fingerprint).Error; errFirst != nil {
				t.Fatal(errFirst)
			}
			wantBackoff := 2 * time.Minute
			if secondSequence == 4 {
				wantBackoff = 4 * time.Minute
			}
			wantSequence := max(3, secondSequence)
			if sample.Sequence != wantSequence || sample.Valid || sample.FailureBackoff != wantBackoff || !sample.ObservedAt.Equal(now.Add(time.Duration(wantSequence-5)*time.Second)) {
				t.Fatalf("concurrent reports lost ordering or failure backoff: %+v", sample)
			}
		})
	}
}
