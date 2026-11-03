package cluster

import (
	"context"
	"reflect"
	"strconv"
	"testing"
	"time"

	"gorm.io/gorm"
)

func TestRebalanceLocksLifetimeBeforeElectionGateAndClock(t *testing.T) {
	for _, action := range []string{"ENABLE", "CLAIM", "RESULT", "LATENCY"} {
		t.Run(action, func(t *testing.T) {
			f := newRebalanceFixture(t, 8)
			f.reconcile(t, 0)
			plan := f.plans(t)[0]
			var events []string
			if errRegister := f.repo.db.Callback().Query().After("gorm:query").Register("test:rebalance-lock-order", func(tx *gorm.DB) {
				if _, locked := tx.Statement.Clauses["FOR"]; locked {
					events = append(events, tx.Statement.Table)
				}
			}); errRegister != nil {
				t.Fatal(errRegister)
			}
			if errRegister := f.repo.db.Callback().Create().Before("gorm:create").Register("test:rebalance-gate-create-order", func(tx *gorm.DB) {
				if tx.Statement.Table == "cluster_master_gate" {
					events = append(events, "gate insert")
				}
			}); errRegister != nil {
				t.Fatal(errRegister)
			}
			if errRegister := f.repo.db.Callback().Row().Before("gorm:row").Register("test:rebalance-clock-order", func(tx *gorm.DB) {
				if tx.Statement.SQL.String() == databaseNowQuery(tx) {
					events = append(events, "database time")
				}
			}); errRegister != nil {
				t.Fatal(errRegister)
			}
			args := []string{action}
			switch action {
			case "CLAIM":
				args = append(args, plan.ID, strconv.FormatInt(plan.Term, 10), plan.TargetIP, strconv.Itoa(plan.TargetPort))
			case "RESULT":
				args = append(args, plan.ID, "failed")
			case "LATENCY":
				args = append(args, `{"process_id":"p","samples":[]}`)
			}
			if !rebalanceTestCommand(t, f.handlers[1], f.lifetime(plan), args...) {
				t.Fatal("command rejected")
			}
			want := []string{"cpa_node_membership", "home_process_incarnation", "cluster_rebalance_gate", "cluster_master_gate", "database time"}
			if action == "CLAIM" {
				want = append(want, "cluster_master_gate", "database time")
			} else if action == "RESULT" {
				want = append(want, "cpa_rebalance_clients", "database time")
			} else if action == "LATENCY" {
				want = []string{"cpa_node_membership", "home_process_incarnation", "cpa_rebalance_clients", "database time"}
			}
			if !reflect.DeepEqual(events, want) {
				t.Fatalf("lock/clock order = %v, want %v", events, want)
			}
		})
	}
}

func TestRebalanceClaimUsesTimeAfterLifetimeWait(t *testing.T) {
	f := newRebalanceFixture(t, 8)
	f.reconcile(t, 0)
	plan := f.plans(t)[0]
	now, errNow := DatabaseNow(context.Background(), f.repo.db)
	if errNow != nil {
		t.Fatal(errNow)
	}
	var gate ClusterMasterGateRecord
	if errFirst := f.repo.db.First(&gate, "id = ?", 1).Error; errFirst != nil {
		t.Fatal(errFirst)
	}
	if errRegister := f.repo.db.Callback().Row().Before("gorm:row").Register("test:rebalance-clock-after-wait", func(tx *gorm.DB) {
		if tx.Statement.SQL.String() == databaseNowQuery(tx) {
			tx.Statement.SQL.Reset()
			tx.Statement.SQL.WriteString("SELECT ?")
			tx.Statement.Vars = []any{now.UTC().Format(time.RFC3339Nano)}
		}
	}); errRegister != nil {
		t.Fatal(errRegister)
	}
	if errRegister := f.repo.db.Callback().Query().After("gorm:query").Register("test:rebalance-elapse-lifetime-wait", func(tx *gorm.DB) {
		if tx.Statement.Table == "home_process_incarnation" {
			if _, locked := tx.Statement.Clauses["FOR"]; locked {
				now = gate.LeaseUntil.Add(time.Second)
			}
		}
	}); errRegister != nil {
		t.Fatal(errRegister)
	}
	if claimRebalanceTestPlan(t, f, plan) {
		t.Fatal("claim used a timestamp from before the lifetime-lock wait")
	}
}
