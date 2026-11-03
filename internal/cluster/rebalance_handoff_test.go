package cluster

import (
	"context"
	"reflect"
	"testing"
	"time"
)

func TestRebalanceExecutingHandoffRetainsCapacityUntilExpiry(t *testing.T) {
	f := newRebalanceFixture(t, 40)
	f.reconcile(t, 0)
	plans := f.plans(t)
	if len(plans) != rebalanceMaxActive {
		t.Fatalf("plans = %d, want %d", len(plans), rebalanceMaxActive)
	}
	for _, plan := range plans {
		if !claimRebalanceTestPlan(t, f, plan) {
			t.Fatal("claim rejected")
		}
	}
	now, errNow := DatabaseNow(context.Background(), f.repo.db)
	if errNow != nil {
		t.Fatal(errNow)
	}
	for _, plan := range plans {
		if errUpdate := f.repo.db.Model(&CPANodeMembershipRecord{}).Where("certificate_fingerprint = ?", plan.Fingerprint).
			Updates(map[string]any{"last_seen_at": now.Add(-4 * time.Second), "cpa_heartbeat_timeout": 3 * time.Second}).Error; errUpdate != nil {
			t.Fatal(errUpdate)
		}
	}
	if errUpdate := f.repo.db.Model(&ClusterMasterGateRecord{}).Where("id = ?", 1).Update("last_balance_at", time.Time{}).Error; errUpdate != nil {
		t.Fatal(errUpdate)
	}
	f.reconcile(t, 0)
	active := f.plans(t)
	if len(active) != len(plans) {
		t.Fatalf("active plans = %d, want %d", len(active), len(plans))
	}
	for index, plan := range active {
		if plan.ID != plans[index].ID || plan.State != rebalanceExecuting {
			t.Fatalf("executing plan was replaced during handoff: %+v", plan)
		}
	}
	// A missing target is not proof that a claimed operation can no longer complete.
	if errUpdate := f.repo.db.Model(&ClusterNodeRecord{}).Where("ip = ?", f.homes[2].IP).Update("last_seen_at", now.Add(-time.Minute)).Error; errUpdate != nil {
		t.Fatal(errUpdate)
	}
	f.reconcile(t, 0)
	if len(f.plans(t)) != len(plans) {
		t.Fatal("temporary target loss released execution capacity")
	}
	// Expiry still terminates stalled executions and applies the normal failure backoff.
	if errUpdate := f.repo.db.Model(&CPARebalancePlanRecord{}).Where("state = ?", rebalanceExecuting).Update("expires_at", now.Add(-time.Second)).Error; errUpdate != nil {
		t.Fatal(errUpdate)
	}
	f.reconcile(t, 0)
	afterExpiry, errAfterExpiry := DatabaseNow(context.Background(), f.repo.db)
	if errAfterExpiry != nil {
		t.Fatal(errAfterExpiry)
	}
	for _, plan := range plans {
		var saved CPARebalancePlanRecord
		if errFirst := f.repo.db.First(&saved, "id = ?", plan.ID).Error; errFirst != nil {
			t.Fatal(errFirst)
		}
		var client CPARebalanceClientRecord
		if errFirst := f.repo.db.First(&client, "fingerprint = ?", plan.Fingerprint).Error; errFirst != nil {
			t.Fatal(errFirst)
		}
		if saved.State != rebalanceFailed || client.FailureBackoff != time.Minute || client.TargetRetryAt.Before(now.Add(time.Minute)) || client.TargetRetryAt.After(afterExpiry.Add(time.Minute)) {
			t.Fatalf("expired execution did not fail with backoff: plan=%+v client=%+v", saved, client)
		}
	}
}

func TestRebalanceClaimReservesDisconnectedExecutingTarget(t *testing.T) {
	f := newRebalanceFixture(t, 3)
	f.reconcile(t, 0)
	plan := f.plans(t)[0]
	if !claimRebalanceTestPlan(t, f, plan) {
		t.Fatal("claim rejected")
	}
	now, errNow := DatabaseNow(context.Background(), f.repo.db)
	if errNow != nil {
		t.Fatal(errNow)
	}
	if errUpdate := f.repo.db.Model(&CPANodeMembershipRecord{}).Where("certificate_fingerprint = ?", plan.Fingerprint).
		Updates(map[string]any{"last_seen_at": now.Add(-4 * time.Second), "cpa_heartbeat_timeout": 3 * time.Second}).Error; errUpdate != nil {
		t.Fatal(errUpdate)
	}
	// Simulate another queued plan based on an earlier load snapshot. The two remaining
	// source members must account for the executing arrival at this same target.
	pending := plan
	pending.ID = "queued-plan"
	for _, member := range f.members {
		if member.CertificateFingerprint != plan.Fingerprint {
			pending.Fingerprint = member.CertificateFingerprint
			pending.SourceConnectedAt = member.ConnectedAt
			break
		}
	}
	if errCreate := f.repo.db.Create(&pending).Error; errCreate != nil {
		t.Fatal(errCreate)
	}
	if claimRebalanceTestPlan(t, f, pending) {
		t.Fatal("claim ignored an executing arrival during the heartbeat gap")
	}
	var saved CPARebalancePlanRecord
	if errFirst := f.repo.db.First(&saved, "id = ?", pending.ID).Error; errFirst != nil {
		t.Fatal(errFirst)
	}
	if saved.State != rebalanceCanceled {
		t.Fatalf("obsolete pending plan = %s, want canceled", saved.State)
	}
}

func TestRebalancePendingHeartbeatGapStillCancels(t *testing.T) {
	f := newRebalanceFixture(t, 8)
	f.reconcile(t, 0)
	plan := f.plans(t)[0]
	now, errNow := DatabaseNow(context.Background(), f.repo.db)
	if errNow != nil {
		t.Fatal(errNow)
	}
	if errUpdate := f.repo.db.Model(&CPANodeMembershipRecord{}).Where("certificate_fingerprint = ?", plan.Fingerprint).
		Updates(map[string]any{"last_seen_at": now.Add(-4 * time.Second), "cpa_heartbeat_timeout": 3 * time.Second}).Error; errUpdate != nil {
		t.Fatal(errUpdate)
	}
	f.reconcile(t, 0)
	var saved CPARebalancePlanRecord
	if errFirst := f.repo.db.First(&saved, "id = ?", plan.ID).Error; errFirst != nil {
		t.Fatal(errFirst)
	}
	if saved.State != rebalanceCanceled {
		t.Fatalf("unclaimed stale plan = %s, want canceled", saved.State)
	}
}

func TestReserveRebalanceLoadDuringHandoff(t *testing.T) {
	source := HomeIncarnationID{IP: "source", Port: 8327, StartedAt: time.Unix(1, 0)}
	target := HomeIncarnationID{IP: "target", Port: 8327, StartedAt: time.Unix(2, 0)}
	sourceKey, targetKey := rebalanceHomeKey(source), rebalanceHomeKey(target)
	connectedAt := time.Unix(3, 0)
	plan := CPARebalancePlanRecord{Fingerprint: "fp", State: rebalanceExecuting, SourceConnectedAt: connectedAt,
		SourceIP: source.IP, SourcePort: source.Port, SourceStartedAt: source.StartedAt,
		TargetIP: target.IP, TargetPort: target.Port, TargetStartedAt: target.StartedAt}
	member := CPANodeMembershipRecord{CertificateFingerprint: "fp", ConnectedAt: connectedAt, HomeIP: source.IP, HomePort: source.Port, HomeStartedAt: source.StartedAt}
	arrived := member
	arrived.ConnectedAt = connectedAt.Add(time.Second)
	arrived.HomeIP, arrived.HomePort, arrived.HomeStartedAt = target.IP, target.Port, target.StartedAt
	for _, testCase := range []struct {
		name   string
		state  string
		member *CPANodeMembershipRecord
		loads  map[string]int
		want   map[string]int
		added  int
	}{
		{"source live", rebalanceExecuting, &member, map[string]int{sourceKey: 3, targetKey: 0}, map[string]int{sourceKey: 2, targetKey: 1}, 0},
		{"source heartbeat missing", rebalanceExecuting, nil, map[string]int{sourceKey: 2, targetKey: 0}, map[string]int{sourceKey: 2, targetKey: 1}, 1},
		{"source Home missing", rebalanceExecuting, nil, map[string]int{targetKey: 0}, map[string]int{targetKey: 1}, 1},
		{"source empty", rebalanceExecuting, nil, map[string]int{sourceKey: 0, targetKey: 0}, map[string]int{sourceKey: 0, targetKey: 1}, 1},
		{"target missing", rebalanceExecuting, &member, map[string]int{sourceKey: 3}, map[string]int{sourceKey: 3}, 0},
		{"already arrived", rebalanceExecuting, &arrived, map[string]int{sourceKey: 2, targetKey: 1}, map[string]int{sourceKey: 2, targetKey: 1}, 0},
		{"pending live", rebalancePending, &member, map[string]int{sourceKey: 3, targetKey: 0}, map[string]int{sourceKey: 2, targetKey: 1}, 0},
		{"pending stale", rebalancePending, nil, map[string]int{sourceKey: 2, targetKey: 0}, map[string]int{sourceKey: 2, targetKey: 0}, 0},
		{"pending superseded", rebalancePending, &arrived, map[string]int{sourceKey: 2, targetKey: 1}, map[string]int{sourceKey: 2, targetKey: 1}, 0},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			plan.State = testCase.state
			members := make(map[string]CPANodeMembershipRecord)
			if testCase.member != nil {
				members["fp"] = *testCase.member
			}
			added := reserveRebalanceLoad(plan, members, testCase.loads)
			if added != testCase.added || !reflect.DeepEqual(testCase.loads, testCase.want) {
				t.Fatalf("reservation = %v, added=%d; want %v, added=%d", testCase.loads, added, testCase.want, testCase.added)
			}
		})
	}
}
