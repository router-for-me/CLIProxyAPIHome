package cluster

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	gormlogger "gorm.io/gorm/logger"
)

type rebalanceSQLRecorder struct {
	gormlogger.Interface
	statements []string
	rows       []int64
}

func (r *rebalanceSQLRecorder) ParamsFilter(_ context.Context, sql string, _ ...interface{}) (string, []interface{}) {
	return sql, nil
}

func (r *rebalanceSQLRecorder) Trace(ctx context.Context, begin time.Time, fc func() (string, int64), err error) {
	sql, rows := fc()
	r.statements = append(r.statements, sql)
	r.rows = append(r.rows, rows)
	r.Interface.Trace(ctx, begin, fc, err)
}

func (r *rebalanceSQLRecorder) count(prefix, contains string) int {
	count := 0
	for _, sql := range r.statements {
		if strings.HasPrefix(sql, prefix) && strings.Contains(sql, contains) {
			count++
		}
	}
	return count
}

func TestRebalanceInvalidLeaseSkipsHomeSnapshot(t *testing.T) {
	for _, backend := range []struct {
		name string
		repo func(*testing.T) *Repository
	}{{"sqlite", newCredentialFoundationTestRepository}, {"postgres", newPostgresQuiescenceRepository}} {
		t.Run(backend.name, func(t *testing.T) {
			f := seedRebalanceFixture(t, backend.repo(t), 8)
			f.reconcile(t, 0)
			plan := f.plans(t)[0]
			lifetime := f.lifetime(plan)
			now, errNow := DatabaseNow(t.Context(), f.repo.db)
			if errNow != nil {
				t.Fatal(errNow)
			}
			snapshotFailure := errors.New("test Home snapshot query failure")
			if errRegister := f.repo.db.Callback().Row().Before("gorm:row").Register("test:invalid-lease-snapshot", func(tx *gorm.DB) {
				if tx.Statement.SQL.String() == databaseNowQuery(tx) {
					// Freeze database time to cover equality at the lease boundary.
					tx.Statement.SQL.Reset()
					tx.Statement.Vars = nil
					clause.Expr{SQL: "SELECT ?", Vars: []any{now.UTC().Format(time.RFC3339Nano)}}.Build(tx.Statement)
				}
				if len(tx.Statement.Joins) > 0 {
					tx.AddError(snapshotFailure)
				}
			}); errRegister != nil {
				t.Fatal(errRegister)
			}
			recorder := &rebalanceSQLRecorder{Interface: f.repo.db.Logger}
			f.repo.db.Logger = recorder
			for _, test := range []struct {
				name       string
				term       int64
				leaseUntil time.Time
			}{
				{"uninitialized term", 0, now.Add(time.Minute)},
				{"negative term", -1, now.Add(time.Minute)},
				{"expired lease", plan.Term, now.Add(-time.Second)},
				{"lease boundary", plan.Term, now},
			} {
				t.Run(test.name, func(t *testing.T) {
					if errUpdate := f.repo.db.Model(&ClusterMasterGateRecord{}).Where("id = ?", 1).
						Updates(map[string]any{"term": test.term, "lease_until": test.leaseUntil}).Error; errUpdate != nil {
						t.Fatal(errUpdate)
					}
					recorder.statements = nil
					payload, errInstruction := f.handlers[1].RebalanceInstruction(t.Context(), lifetime)
					if errInstruction != nil || len(payload) != 0 {
						t.Fatalf("invalid lease instruction: payload=%s error=%v", payload, errInstruction)
					}
					if len(recorder.statements) != 2 {
						t.Fatalf("instruction must only read gate and clock: %v", recorder.statements)
					}
					recorder.statements = nil
					if claimRebalanceTestPlan(t, f, plan) {
						t.Fatal("invalid lease authorized a claim")
					}
					if len(recorder.statements) != 5 {
						t.Fatalf("claim must only lock the lifetime and gates, then read the clock: %v", recorder.statements)
					}
				})
			}
			// A valid lease must still reach the snapshot query and propagate its error.
			if errUpdate := f.repo.db.Model(&ClusterMasterGateRecord{}).Where("id = ?", 1).
				Updates(map[string]any{"term": plan.Term, "lease_until": now.Add(time.Minute)}).Error; errUpdate != nil {
				t.Fatal(errUpdate)
			}
			if _, errInstruction := f.handlers[1].RebalanceInstruction(t.Context(), lifetime); !errors.Is(errInstruction, snapshotFailure) {
				t.Fatalf("valid lease skipped or suppressed snapshot failure: %v", errInstruction)
			}
			args := []string{"CLUSTER", "BALANCE", "CLAIM", plan.ID, strconv.FormatInt(plan.Term, 10), plan.TargetIP, strconv.Itoa(plan.TargetPort)}
			if _, errClaim := f.handlers[1].HandleRebalance(t.Context(), lifetime, args); !errors.Is(errClaim, snapshotFailure) {
				t.Fatalf("valid lease claim skipped or suppressed snapshot failure: %v", errClaim)
			}
		})
	}
}

func TestRebalanceStageTwoSQLBudget(t *testing.T) {
	for _, backend := range []struct {
		name string
		repo func(*testing.T) *Repository
	}{{"sqlite", newCredentialFoundationTestRepository}, {"postgres", newPostgresQuiescenceRepository}} {
		t.Run(backend.name, func(t *testing.T) {
			for _, test := range []struct {
				name      string
				members   int
				wantSQL   int
				wantPlans int
			}{{"idle", 0, 13, 0}, {"batch", 8, 14, 2}} {
				t.Run(test.name, func(t *testing.T) {
					f := seedRebalanceFixture(t, backend.repo(t), test.members)
					recorder := &rebalanceSQLRecorder{Interface: f.repo.db.Logger}
					f.repo.db.Logger = recorder
					f.reconcile(t, 0)
					if len(recorder.statements) != test.wantSQL {
						t.Fatalf("planner SQL = %d, want %d: %v", len(recorder.statements), test.wantSQL, recorder.statements)
					}
					if recorder.count("SELECT", "cpa_rebalance_plans") != 1 || recorder.count("INSERT", "cluster_master_gate") != 0 || recorder.count("DELETE", "") != 0 {
						t.Fatalf("redundant planner statements: %v", recorder.statements)
					}
					wantInserts := 0
					if test.wantPlans > 0 {
						wantInserts = 1
					}
					if recorder.count("INSERT", "cpa_rebalance_plans") != wantInserts {
						t.Fatalf("ordinary plans were not batched: %v", recorder.statements)
					}
					t.Logf("planner SQL=%d, plan INSERTs=%d", len(recorder.statements), wantInserts)
					plans := f.plans(t)
					if len(plans) != test.wantPlans || (len(plans) == 2 && plans[0].TargetIP == plans[1].TargetIP) {
						t.Fatalf("batch lost load reservations: %+v", plans)
					}
					recorder.statements = nil
					f.reconcile(t, 0)
					if len(recorder.statements) != 6 || recorder.count("SELECT", "cpa_rebalance_plans") != 1 {
						t.Fatalf("maintenance SQL = %d, want 6: %v", len(recorder.statements), recorder.statements)
					}
					if recorder.count("SELECT", "cluster_master_gate") != 1 || recorder.count("UPDATE", "") != 0 || recorder.count("INSERT", "") != 0 || recorder.count("DELETE", "") != 0 {
						t.Fatalf("read-only maintenance fenced or wrote state: %v", recorder.statements)
					}
				})
			}
		})
	}
}

func TestRebalanceStageOneSwapWorkingSet(t *testing.T) {
	for _, backend := range []struct {
		name string
		repo func(*testing.T) *Repository
	}{{"sqlite", newCredentialFoundationTestRepository}, {"postgres", newPostgresQuiescenceRepository}} {
		t.Run(backend.name, func(t *testing.T) {
			f := seedLatencySwapFixture(t, backend.repo(t))
			gate, now, _, _, _, _ := latencyTestState(t, f)
			plans := []CPARebalancePlanRecord{newRebalancePlan(f.members[0], f.homes[1], gate.Term, now), newRebalancePlan(f.members[2], f.homes[0], gate.Term, now)}
			plans[0].ID, plans[1].ID = "a-first", "z-expired"
			plans[0].SwapGroupID, plans[1].SwapGroupID = "swap", "swap"
			plans[1].ExpiresAt = now.Add(-time.Second)
			if errCreate := f.repo.db.Create(&plans).Error; errCreate != nil {
				t.Fatal(errCreate)
			}
			// Maintenance must still finish the expired leg before the next planning round.
			if errUpdate := f.repo.db.Model(&gate).Update("last_balance_at", now).Error; errUpdate != nil {
				t.Fatal(errUpdate)
			}
			if errRegister := f.repo.db.Callback().Query().Before("gorm:query").Register("test:ordered-working-set", func(tx *gorm.DB) {
				if tx.Statement.Table == "cpa_rebalance_plans" {
					tx.Statement.AddClause(clause.OrderBy{Columns: []clause.OrderByColumn{{Column: clause.Column{Name: "id"}}}})
				}
			}); errRegister != nil {
				t.Fatal(errRegister)
			}
			recorder := &rebalanceSQLRecorder{Interface: f.repo.db.Logger}
			f.repo.db.Logger = recorder
			f.reconcile(t, 0)
			if recorder.count("SELECT", "cpa_rebalance_plans") != 1 {
				t.Fatalf("swap maintenance reloaded plans: %v", recorder.statements)
			}
			var saved []CPARebalancePlanRecord
			if errFind := f.repo.db.Order("id").Find(&saved).Error; errFind != nil {
				t.Fatal(errFind)
			}
			if len(saved) != 2 || saved[0].State != rebalanceCanceled || saved[1].State != rebalanceFailed {
				t.Fatalf("earlier peer was not canceled in the working set: %+v", saved)
			}
			for index, plan := range saved {
				var client CPARebalanceClientRecord
				if errFirst := f.repo.db.First(&client, "fingerprint = ?", plan.Fingerprint).Error; errFirst != nil {
					t.Fatal(errFirst)
				}
				wantBackoff := time.Duration(0)
				if index == 1 {
					wantBackoff = time.Minute
				}
				if client.FailureBackoff != wantBackoff || !client.NextEligibleAt.After(now) {
					t.Fatalf("peer transition duplicated or lost backoff: %+v", client)
				}
			}
		})
	}
}

func TestRebalanceStageOneSnapshotEligibility(t *testing.T) {
	for _, backend := range []struct {
		name string
		repo func(*testing.T) *Repository
	}{{"sqlite", newCredentialFoundationTestRepository}, {"postgres", newPostgresQuiescenceRepository}} {
		t.Run(backend.name, func(t *testing.T) {
			f := seedRebalanceFixture(t, backend.repo(t), 0)
			var gate ClusterMasterGateRecord
			if errFirst := f.repo.db.First(&gate, "id = ?", 1).Error; errFirst != nil {
				t.Fatal(errFirst)
			}
			now, errNow := DatabaseNow(t.Context(), f.repo.db)
			if errNow != nil {
				t.Fatal(errNow)
			}
			rollback := errors.New("test snapshot rollback")
			for _, test := range []struct {
				name     string
				mutate   func(*gorm.DB) error
				valid    bool
				wantHome int
			}{
				{"healthy", func(tx *gorm.DB) error { return nil }, true, 3},
				{"retired master", func(tx *gorm.DB) error {
					return tx.Model(&HomeProcessIncarnationRecord{}).Where("home_ip = ?", f.homes[0].IP).Update("state", HomeIncarnationRetired).Error
				}, false, 2},
				{"stale master process", func(tx *gorm.DB) error {
					return tx.Model(&HomeProcessIncarnationRecord{}).Where("home_ip = ?", f.homes[0].IP).Update("last_seen_at", now.Add(-time.Minute)).Error
				}, false, 2},
				{"different incarnation", func(tx *gorm.DB) error {
					return tx.Model(&ClusterNodeRecord{}).Where("ip = ?", f.homes[0].IP).Update("started_at", now.Add(-time.Hour)).Error
				}, false, 2},
				{"duplicate master with inactive process", func(tx *gorm.DB) error {
					if errUpdate := tx.Model(&HomeProcessIncarnationRecord{}).Where("home_ip = ?", f.homes[1].IP).Update("state", HomeIncarnationRetired).Error; errUpdate != nil {
						return errUpdate
					}
					return tx.Model(&ClusterNodeRecord{}).Where("ip = ?", f.homes[1].IP).Update("is_master", true).Error
				}, false, 2},
				{"duplicate active master", func(tx *gorm.DB) error {
					return tx.Model(&ClusterNodeRecord{}).Where("ip = ?", f.homes[1].IP).Update("is_master", true).Error
				}, false, 3},
			} {
				t.Run(test.name, func(t *testing.T) {
					errTransaction := f.repo.db.Transaction(func(tx *gorm.DB) error {
						if errMutate := test.mutate(tx); errMutate != nil {
							return errMutate
						}
						snapshot, errHomes := loadRebalanceHomes(tx, now, defaultHeartbeatTimeout)
						if errHomes != nil {
							return errHomes
						}
						if snapshot.leaderValid(gate, now) != test.valid || len(snapshot.homes) != test.wantHome {
							t.Errorf("snapshot valid=%t homes=%d, want valid=%t homes=%d", snapshot.leaderValid(gate, now), len(snapshot.homes), test.valid, test.wantHome)
						}
						if snapshot.leaderValid(gate, gate.LeaseUntil) {
							t.Error("expired lease accepted")
						}
						return rollback
					})
					if !errors.Is(errTransaction, rollback) {
						t.Fatal(errTransaction)
					}
				})
			}
		})
	}
}

func TestRebalanceStageOneMalformedCommandsDoNotQuery(t *testing.T) {
	f := newRebalanceFixture(t, 1)
	recorder := &rebalanceSQLRecorder{Interface: f.repo.db.Logger}
	f.repo.db.Logger = recorder
	for _, args := range [][]string{
		{"CLUSTER", "BALANCE"}, {"CLUSTER", "BALANCE", "UNKNOWN"},
		{"CLUSTER", "BALANCE", "ENABLE", "extra"}, {"CLUSTER", "BALANCE", "CLAIM"},
		{"CLUSTER", "BALANCE", "CLAIM", "id", "bad-term", "home", "8327"},
		{"CLUSTER", "BALANCE", "RESULT", "id", "invalid"}, {"CLUSTER", "BALANCE", "LATENCY"},
	} {
		if _, errHandle := f.handlers[1].HandleRebalance(t.Context(), f.lifetimes[0], args); errHandle == nil {
			t.Fatalf("malformed command accepted: %v", args)
		}
	}
	if len(recorder.statements) != 0 {
		t.Fatalf("malformed commands touched the database: %v", recorder.statements)
	}
}

func TestRebalanceStageOneElectionSQLAndFlags(t *testing.T) {
	for _, backend := range []struct {
		name string
		repo func(*testing.T) *Repository
	}{{"sqlite", newCredentialFoundationTestRepository}, {"postgres", newPostgresQuiescenceRepository}} {
		t.Run(backend.name, func(t *testing.T) {
			f := seedRebalanceFixture(t, backend.repo(t), 0)
			var gate ClusterMasterGateRecord
			if errFirst := f.repo.db.First(&gate, "id = ?", 1).Error; errFirst != nil {
				t.Fatal(errFirst)
			}
			// Force a lease refresh even when SQLite heartbeats share one clock tick.
			if errUpdate := f.repo.db.Model(&gate).Update("lease_until", gate.LeaseUntil.Add(-time.Second)).Error; errUpdate != nil {
				t.Fatal(errUpdate)
			}
			recorder := &rebalanceSQLRecorder{Interface: f.repo.db.Logger}
			f.repo.db.Logger = recorder
			for index, wantSQL := range []int{6, 5} {
				recorder.statements, recorder.rows = nil, nil
				if errBeat := f.coordinators[index].heartbeatAndElect(t.Context(), f.homes[index]); errBeat != nil {
					t.Fatal(errBeat)
				}
				if len(recorder.statements) != wantSQL {
					t.Fatalf("election SQL=%d, want %d: %v", len(recorder.statements), wantSQL, recorder.statements)
				}
				for queryIndex, sql := range recorder.statements {
					if strings.HasPrefix(sql, "UPDATE") && strings.Contains(sql, "is_master") && recorder.rows[queryIndex] != 0 {
						t.Fatalf("incumbent flags rewritten: %s rows=%d", sql, recorder.rows[queryIndex])
					}
				}
			}
			// Legacy rows may have NULL flags; a selected node must still become master.
			if errUpdate := f.repo.db.Model(&ClusterNodeRecord{}).Where("ip = ?", f.homes[0].IP).Update("is_master", nil).Error; errUpdate != nil {
				t.Fatal(errUpdate)
			}
			if errUpdate := f.repo.db.Model(&ClusterNodeRecord{}).Where("ip = ?", f.homes[1].IP).Update("is_master", true).Error; errUpdate != nil {
				t.Fatal(errUpdate)
			}
			if errBeat := f.coordinators[0].heartbeatAndElect(t.Context(), f.homes[0]); errBeat != nil {
				t.Fatal(errBeat)
			}
			var masters []ClusterNodeRecord
			if errFind := f.repo.db.Where("is_master = ?", true).Find(&masters).Error; errFind != nil {
				t.Fatal(errFind)
			}
			if len(masters) != 1 || masters[0].IP != f.homes[0].IP {
				t.Fatalf("election failed to repair master flags: %+v", masters)
			}
		})
	}
}

func TestRebalanceStageOneBatchFailureRollsBackRound(t *testing.T) {
	f := newRebalanceFixture(t, 40)
	failure := errors.New("test rebalance batch insert failure")
	var batchSize int
	if errRegister := f.repo.db.Callback().Create().Before("gorm:create").Register("test:fail-plan-batch", func(tx *gorm.DB) {
		if tx.Statement.Table == "cpa_rebalance_plans" {
			batchSize = tx.Statement.ReflectValue.Len()
			tx.AddError(failure)
		}
	}); errRegister != nil {
		t.Fatal(errRegister)
	}
	if errReconcile := f.repo.ReconcileRebalances(t.Context(), f.homes[0], defaultHeartbeatTimeout); !errors.Is(errReconcile, failure) {
		t.Fatalf("batch insert error was lost: %v", errReconcile)
	}
	if batchSize != rebalanceMaxActive || len(f.plans(t)) != 0 {
		t.Fatalf("failed batch left partial reservations: batch=%d", batchSize)
	}
	var gate ClusterMasterGateRecord
	if errFirst := f.repo.db.First(&gate, "id = ?", 1).Error; errFirst != nil {
		t.Fatal(errFirst)
	}
	if !gate.LastBalanceAt.IsZero() {
		t.Fatal("failed batch advanced the planning cadence")
	}
}
