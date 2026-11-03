package cluster

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func TestRebalanceFinalFenceRollsBackSpeculativeWrites(t *testing.T) {
	for _, backend := range []struct {
		name string
		repo func(*testing.T) *Repository
	}{{"sqlite", newCredentialFoundationTestRepository}, {"postgres", newPostgresQuiescenceRepository}} {
		t.Run(backend.name, func(t *testing.T) {
			for _, scenario := range []string{"lease", "member snapshot", "plan expiry", "retired master", "snapshot error"} {
				t.Run(scenario, func(t *testing.T) {
					f := seedRebalanceFixture(t, backend.repo(t), 8)
					f.reconcile(t, 0)
					plan := f.plans(t)[0]
					var gate ClusterMasterGateRecord
					if errFirst := f.repo.db.First(&gate, "id = ?", 1).Error; errFirst != nil {
						t.Fatal(errFirst)
					}
					now, errNow := DatabaseNow(t.Context(), f.repo.db)
					if errNow != nil {
						t.Fatal(errNow)
					}
					if scenario == "plan expiry" {
						plan.ExpiresAt = now.Add(time.Second)
						if errUpdate := f.repo.db.Model(&plan).Update("expires_at", plan.ExpiresAt).Error; errUpdate != nil {
							t.Fatal(errUpdate)
						}
					}
					failure := errors.New("test final snapshot failure")
					fenced := false
					if errRegister := f.repo.db.Callback().Query().After("gorm:query").Register("test:final-fence-boundary", func(tx *gorm.DB) {
						locking, ok := tx.Statement.Clauses["FOR"].Expression.(clause.Locking)
						if tx.Statement.Table != "cluster_master_gate" || !ok || locking.Strength != "NO KEY UPDATE" {
							return
						}
						fenced = true
						switch scenario {
						case "lease":
							now = gate.LeaseUntil
						case "member snapshot":
							now = f.members[0].LastSeenAt.Add(f.members[0].CPAHeartbeatTimeout)
						case "plan expiry":
							now = plan.ExpiresAt
						case "retired master":
							if errUpdate := tx.Session(&gorm.Session{NewDB: true}).Model(&HomeProcessIncarnationRecord{}).Where("home_ip = ?", f.homes[0].IP).Update("state", HomeIncarnationRetired).Error; errUpdate != nil {
								tx.AddError(errUpdate)
							}
						}
					}); errRegister != nil {
						t.Fatal(errRegister)
					}
					if errRegister := f.repo.db.Callback().Row().Before("gorm:row").Register("test:final-fence-clock", func(tx *gorm.DB) {
						if tx.Statement.SQL.String() == databaseNowQuery(tx) {
							tx.Statement.SQL.Reset()
							tx.Statement.Vars = nil
							clause.Expr{SQL: "SELECT ?", Vars: []any{now.UTC().Format(time.RFC3339Nano)}}.Build(tx.Statement)
						}
						if fenced && scenario == "snapshot error" && len(tx.Statement.Joins) > 0 {
							tx.AddError(failure)
						}
					}); errRegister != nil {
						t.Fatal(errRegister)
					}
					payload, errHandle := f.handlers[1].HandleRebalance(t.Context(), f.lifetime(plan), []string{"CLUSTER", "BALANCE", "CLAIM", plan.ID, strconv.FormatInt(plan.Term, 10), plan.TargetIP, strconv.Itoa(plan.TargetPort)})
					if scenario == "snapshot error" {
						if !errors.Is(errHandle, failure) {
							t.Fatalf("fence query error lost: %v", errHandle)
						}
					} else if errHandle != nil || !strings.Contains(string(payload), `"ok":false`) {
						t.Fatalf("invalid final fence authorized claim: payload=%s error=%v", payload, errHandle)
					}
					var saved CPARebalancePlanRecord
					if errFirst := f.repo.db.First(&saved, "id = ?", plan.ID).Error; errFirst != nil {
						t.Fatal(errFirst)
					}
					if !fenced || saved.State != rebalancePending || !saved.ExpiresAt.Equal(plan.ExpiresAt) {
						t.Fatalf("speculative claim survived failed fence: fenced=%t plan=%+v", fenced, saved)
					}
				})
			}
		})
	}
}

func TestRebalanceElectionFenceHasBoundedSQL(t *testing.T) {
	for _, backend := range []struct {
		name string
		repo func(*testing.T) *Repository
	}{{"sqlite", newCredentialFoundationTestRepository}, {"postgres", newPostgresQuiescenceRepository}} {
		t.Run(backend.name, func(t *testing.T) {
			f := seedRebalanceFixture(t, backend.repo(t), 40)
			recorder := &rebalanceSQLRecorder{Interface: f.repo.db.Logger}
			f.repo.db.Logger = recorder
			fenceIndex := -1
			if errRegister := f.repo.db.Callback().Query().After("gorm:query").Register("test:fence-sql-budget", func(tx *gorm.DB) {
				locking, ok := tx.Statement.Clauses["FOR"].Expression.(clause.Locking)
				if tx.Statement.Table == "cluster_master_gate" && ok && locking.Strength == "NO KEY UPDATE" {
					// Query callbacks precede the logger's Trace for this SELECT.
					fenceIndex = len(recorder.statements)
				}
			}); errRegister != nil {
				t.Fatal(errRegister)
			}
			f.reconcile(t, 0)
			if fenceIndex < 0 || len(recorder.statements)-fenceIndex != 4 {
				t.Fatalf("planning election fence SQL is not bounded: index=%d statements=%v", fenceIndex, recorder.statements)
			}
			for _, statement := range recorder.statements[fenceIndex:] {
				if strings.Contains(statement, "cpa_") {
					t.Fatalf("CPA work under the election fence: %s", statement)
				}
			}
			plan := f.plans(t)[0]
			recorder.statements, fenceIndex = nil, -1
			if !claimRebalanceTestPlan(t, f, plan) {
				t.Fatal("claim rejected")
			}
			if fenceIndex < 0 || len(recorder.statements)-fenceIndex != 3 {
				t.Fatalf("claim election fence SQL is not bounded: index=%d statements=%v", fenceIndex, recorder.statements)
			}
			for _, statement := range recorder.statements[fenceIndex:] {
				if strings.Contains(statement, "cpa_") {
					t.Fatalf("CPA work under claim fence: %s", statement)
				}
			}
			t.Log("election fence: planning=4 SQL, CLAIM=3 SQL (including lock SELECT, excluding transaction control)")
		})
	}
}
