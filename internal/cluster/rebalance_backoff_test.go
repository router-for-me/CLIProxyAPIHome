package cluster

import (
	"context"
	"testing"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func TestRebalanceAlternatingTargetFailuresRespectGlobalBackoff(t *testing.T) {
	f := newRebalanceFixture(t, 8)
	now, errNow := DatabaseNow(context.Background(), f.repo.db)
	if errNow != nil {
		t.Fatal(errNow)
	}
	base := now
	if errRegister := f.repo.db.Callback().Row().Before("gorm:row").Register("test:rebalance-backoff-clock", func(tx *gorm.DB) {
		if tx.Statement.SQL.String() == databaseNowQuery(tx) {
			tx.Statement.SQL.Reset()
			tx.Statement.SQL.WriteString("SELECT ?")
			tx.Statement.Vars = []any{now.UTC().Format(time.RFC3339Nano)}
		}
	}); errRegister != nil {
		t.Fatal(errRegister)
	}
	advance := func(elapsed time.Duration) {
		t.Helper()
		now = base.Add(elapsed)
		// Advance heartbeat snapshots with the controlled database clock; all Homes and
		// all eight registered CPAs remain eligible except for persisted backoff.
		for _, model := range []any{&ClusterNodeRecord{}, &HomeProcessIncarnationRecord{}, &CPANodeMembershipRecord{}} {
			if errUpdate := f.repo.db.Model(model).Where("1 = 1").Update("last_seen_at", now).Error; errUpdate != nil {
				t.Fatal(errUpdate)
			}
		}
		if errUpdate := f.repo.db.Model(&ClusterMasterGateRecord{}).Where("id = ?", 1).Updates(map[string]any{"lease_until": now.Add(defaultHeartbeatTimeout), "last_balance_at": time.Time{}}).Error; errUpdate != nil {
			t.Fatal(errUpdate)
		}
	}
	f.reconcile(t, 0)
	first := f.plans(t)[0]
	if !claimRebalanceTestPlan(t, f, first) || !rebalanceTestCommand(t, f.handlers[1], f.lifetime(first), "RESULT", first.ID, "failed") {
		t.Fatal("first failure rejected")
	}
	advance(30 * time.Second)
	f.reconcile(t, 0)
	for _, plan := range f.plans(t) {
		if plan.Fingerprint == first.Fingerprint {
			t.Fatal("alternate target bypassed the first one-minute failure backoff")
		}
	}
	// Once the first backoff expires, another target is allowed. Seed that exact
	// scheduler decision so the alternating-failure test is independent of tie order.
	advance(time.Minute)
	second := first
	second.ID = "alternate-target-plan"
	second.CreatedAt, second.UpdatedAt, second.ExpiresAt = now, now, now.Add(rebalancePlanTTL)
	for _, home := range f.homes {
		if home.IP != first.SourceIP && home.IP != first.TargetIP {
			second.TargetIP, second.TargetPort, second.TargetStartedAt = home.IP, home.Port, home.StartedAt
		}
	}
	if errCreate := f.repo.db.Create(&second).Error; errCreate != nil {
		t.Fatal(errCreate)
	}
	if !claimRebalanceTestPlan(t, f, second) || !rebalanceTestCommand(t, f.handlers[1], f.lifetime(second), "RESULT", second.ID, "failed") {
		t.Fatal("alternating target failure rejected")
	}
	var client CPARebalanceClientRecord
	if errFirst := f.repo.db.First(&client, "fingerprint = ?", first.Fingerprint).Error; errFirst != nil {
		t.Fatal(errFirst)
	}
	if client.FailureBackoff != 2*time.Minute || !client.NextEligibleAt.Equal(base.Add(3*time.Minute)) {
		t.Fatalf("second failure did not establish global backoff: %+v", client)
	}
	if !rebalanceTestCommand(t, f.handlers[1], f.lifetime(second), "ENABLE") {
		t.Fatal("capability re-registration failed")
	}
	for _, elapsed := range []time.Duration{90 * time.Second, 150 * time.Second, 179 * time.Second} {
		advance(elapsed)
		f.reconcile(t, 0)
		for _, plan := range f.plans(t) {
			if plan.Fingerprint == first.Fingerprint {
				t.Fatalf("retry to %s at %s bypassed deadline %s", plan.TargetIP, elapsed, client.NextEligibleAt)
			}
		}
	}
	advance(3 * time.Minute)
	f.reconcile(t, 0)
	found := false
	for _, plan := range f.plans(t) {
		if plan.Fingerprint == first.Fingerprint {
			found = true
		}
	}
	if !found {
		t.Fatal("CPA remained ineligible after its global failure backoff expired")
	}
	var registered int64
	if errCount := f.repo.db.Model(&CPARebalanceClientRecord{}).Count(&registered).Error; errCount != nil {
		t.Fatal(errCount)
	}
	if registered != 8 {
		t.Fatalf("registered CPAs = %d, want 8", registered)
	}
}

func TestRebalanceGlobalBackoffCapsAndCancellationCannotShortenIt(t *testing.T) {
	f := newRebalanceFixture(t, 8)
	f.reconcile(t, 0)
	plan := f.plans(t)[0]
	now, errNow := DatabaseNow(context.Background(), f.repo.db)
	if errNow != nil {
		t.Fatal(errNow)
	}
	if errRegister := f.repo.db.Callback().Row().Before("gorm:row").Register("test:backoff-clock", func(tx *gorm.DB) {
		if tx.Statement.SQL.String() == databaseNowQuery(tx) {
			tx.Statement.SQL.Reset()
			tx.Statement.Vars = nil
			clause.Expr{SQL: "SELECT ?", Vars: []any{now.UTC().Format(time.RFC3339Nano)}}.Build(tx.Statement)
		}
	}); errRegister != nil {
		t.Fatal(errRegister)
	}
	for _, wantBackoff := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 10 * time.Minute, 10 * time.Minute} {
		plan.State = rebalanceExecuting
		errTransaction := f.repo.db.Transaction(func(tx *gorm.DB) error { return finishRebalancePlan(tx, &plan, rebalanceFailed, nil) })
		if errTransaction != nil {
			t.Fatal(errTransaction)
		}
		var client CPARebalanceClientRecord
		if errFirst := f.repo.db.First(&client, "fingerprint = ?", plan.Fingerprint).Error; errFirst != nil {
			t.Fatal(errFirst)
		}
		if client.FailureBackoff != wantBackoff || !client.NextEligibleAt.Equal(now.Add(wantBackoff)) {
			t.Fatalf("backoff = %+v, want %s", client, wantBackoff)
		}
		plan.State = rebalancePending
		if errTransaction := f.repo.db.Transaction(func(tx *gorm.DB) error {
			return finishRebalancePlan(tx, &plan, rebalanceCanceled, nil)
		}); errTransaction != nil {
			t.Fatal(errTransaction)
		}
		var after CPARebalanceClientRecord
		if errFirst := f.repo.db.First(&after, "fingerprint = ?", plan.Fingerprint).Error; errFirst != nil {
			t.Fatal(errFirst)
		}
		if !after.NextEligibleAt.Equal(client.NextEligibleAt) {
			t.Fatal("cancellation shortened an unexpired global failure backoff")
		}
		now = now.Add(wantBackoff)
	}
	plan.State = rebalanceExecuting
	if errTransaction := f.repo.db.Transaction(func(tx *gorm.DB) error { return finishRebalancePlan(tx, &plan, rebalanceCompleted, nil) }); errTransaction != nil {
		t.Fatal(errTransaction)
	}
	var recovered CPARebalanceClientRecord
	if errFirst := f.repo.db.First(&recovered, "fingerprint = ?", plan.Fingerprint).Error; errFirst != nil {
		t.Fatal(errFirst)
	}
	if recovered.FailureBackoff != 0 || !recovered.NextEligibleAt.Equal(now.Add(rebalanceCooldown)) {
		t.Fatalf("success did not reset failures and start cooldown: %+v", recovered)
	}
}
