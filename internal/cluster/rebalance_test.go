package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"testing"
	"time"

	"gorm.io/gorm"
)

type rebalanceFixture struct {
	repo         *Repository
	coordinators []*Coordinator
	homes        []HomeIncarnationID
	handlers     []*RESPHandler
	lifetimes    []ConnectionLifetime
	members      []CPANodeMembershipRecord
}

func newRebalanceFixture(t *testing.T, count int) *rebalanceFixture {
	t.Helper()
	return seedRebalanceFixture(t, newCredentialFoundationTestRepository(t), count)
}

func seedRebalanceFixture(t *testing.T, repo *Repository, count int) *rebalanceFixture {
	t.Helper()
	f := &rebalanceFixture{repo: repo}
	for index := range 3 {
		coordinator := NewCoordinator(f.repo, NodeIdentity{IP: fmt.Sprintf("10.0.0.%d", index+1), Port: 8327}, CoordinatorOptions{})
		if errInitialize := coordinator.Initialize(context.Background()); errInitialize != nil {
			t.Fatal(errInitialize)
		}
		t.Cleanup(func() { coordinator.setMaster(false) })
		home, _ := coordinator.HomeIncarnation()
		f.homes = append(f.homes, home)
		f.coordinators = append(f.coordinators, coordinator)
		f.handlers = append(f.handlers, NewRESPHandler(coordinator, nil, f.repo))
	}
	var lifecycle LifecycleConfigRecord
	if errFirst := f.repo.db.First(&lifecycle, "id = ?", 1).Error; errFirst != nil {
		t.Fatal(errFirst)
	}
	for index := range count {
		member, errSubscribe := f.repo.SubscribeMembership(context.Background(), SubscribeMembershipRequest{
			Fingerprint: fmt.Sprintf("fp-%02d", index), NodeID: fmt.Sprintf("cpa-%02d", index), Home: f.homes[1], ProtocolVersion: 1, LifecycleConfigRevision: lifecycle.Revision,
		})
		if errSubscribe != nil {
			t.Fatal(errSubscribe)
		}
		lifetime := ConnectionLifetime{Fingerprint: member.CertificateFingerprint, ConnectedAt: member.ConnectedAt, Home: f.homes[1], Controlled: true}
		f.members = append(f.members, member)
		f.lifetimes = append(f.lifetimes, lifetime)
		if !rebalanceTestCommand(t, f.handlers[1], lifetime, "ENABLE") {
			t.Fatal("capability registration rejected")
		}
	}
	return f
}

func rebalanceTestCommand(t *testing.T, handler *RESPHandler, lifetime ConnectionLifetime, args ...string) bool {
	t.Helper()
	payload, errHandle := handler.HandleRebalance(context.Background(), lifetime, append([]string{"CLUSTER", "BALANCE"}, args...))
	if errHandle != nil {
		t.Fatal(errHandle)
	}
	var response struct {
		OK bool `json:"ok"`
	}
	if errJSON := json.Unmarshal(payload, &response); errJSON != nil {
		t.Fatal(errJSON)
	}
	return response.OK
}

func (f *rebalanceFixture) reconcile(t *testing.T, index int) {
	t.Helper()
	if errReconcile := f.repo.ReconcileRebalances(context.Background(), f.homes[index], defaultHeartbeatTimeout); errReconcile != nil {
		t.Fatal(errReconcile)
	}
}

func (f *rebalanceFixture) plans(t *testing.T) []CPARebalancePlanRecord {
	t.Helper()
	var plans []CPARebalancePlanRecord
	if errFind := f.repo.db.Where("state IN ?", []string{rebalancePending, rebalanceExecuting}).Order("fingerprint").Find(&plans).Error; errFind != nil {
		t.Fatal(errFind)
	}
	return plans
}

func (f *rebalanceFixture) lifetime(plan CPARebalancePlanRecord) ConnectionLifetime {
	for _, lifetime := range f.lifetimes {
		if lifetime.Fingerprint == plan.Fingerprint {
			return lifetime
		}
	}
	return ConnectionLifetime{}
}

func claimRebalanceTestPlan(t *testing.T, f *rebalanceFixture, plan CPARebalancePlanRecord) bool {
	t.Helper()
	return rebalanceTestCommand(t, f.handlers[1], f.lifetime(plan), "CLAIM", plan.ID, strconv.FormatInt(plan.Term, 10), plan.TargetIP, strconv.Itoa(plan.TargetPort))
}

func TestRebalanceMasterPlansAndReservesAcrossFollowers(t *testing.T) {
	f := newRebalanceFixture(t, 8)
	f.reconcile(t, 1)
	if len(f.plans(t)) != 0 {
		t.Fatal("follower created plans")
	}
	f.reconcile(t, 0)
	plans := f.plans(t)
	if len(plans) != 2 {
		t.Fatalf("plans = %d, want 2", len(plans))
	}
	if plans[0].TargetIP == plans[1].TargetIP {
		t.Fatal("plans ignored target reservations")
	}
	for _, plan := range plans {
		if plan.SourceIP != f.homes[1].IP {
			t.Fatal("wrong source Home")
		}
		payload, errInstruction := f.handlers[1].RebalanceInstruction(context.Background(), f.lifetime(plan))
		if errInstruction != nil {
			t.Fatal(errInstruction)
		}
		var instruction RebalanceInstruction
		if errJSON := json.Unmarshal(payload, &instruction); errJSON != nil || instruction.ID != plan.ID {
			t.Fatalf("instruction = %s, %v", payload, errJSON)
		}
		stale := f.lifetime(plan)
		stale.ConnectedAt = stale.ConnectedAt.Add(-time.Second)
		if payload, errInstruction := f.handlers[1].RebalanceInstruction(context.Background(), stale); errInstruction != nil || len(payload) != 0 {
			t.Fatal("stale subscription received instruction")
		}
	}
	f.reconcile(t, 0)
	if len(f.plans(t)) != 2 {
		t.Fatal("duplicate round created more plans")
	}
	for range 5 {
		if errUpdate := f.repo.db.Model(&ClusterMasterGateRecord{}).Where("id = ?", 1).Update("last_balance_at", time.Time{}).Error; errUpdate != nil {
			t.Fatal(errUpdate)
		}
		f.reconcile(t, 0)
	}
	plans = f.plans(t)
	if len(plans) > rebalanceMaxActive {
		t.Fatal("global in-flight quota exceeded")
	}
	seen := map[string]bool{}
	for _, plan := range plans {
		if seen[plan.Fingerprint] {
			t.Fatal("duplicate CPA plan")
		}
		seen[plan.Fingerprint] = true
	}
}

func TestRebalanceInstructionIsReadOnlyAndDoesNotLock(t *testing.T) {
	f := newRebalanceFixture(t, 8)
	f.reconcile(t, 0)
	plan := f.plans(t)[0]
	if errRegister := f.repo.db.Callback().Query().Before("gorm:query").Register("rebalance:reject-instruction-locks", func(tx *gorm.DB) {
		if _, locked := tx.Statement.Clauses["FOR"]; locked {
			tx.AddError(fmt.Errorf("instruction polling must not acquire row locks"))
		}
	}); errRegister != nil {
		t.Fatal(errRegister)
	}
	if errReadOnly := f.repo.db.Exec("PRAGMA query_only = ON").Error; errReadOnly != nil {
		t.Fatal(errReadOnly)
	}
	payload, errInstruction := f.handlers[1].RebalanceInstruction(context.Background(), f.lifetime(plan))
	if errInstruction != nil {
		t.Fatal(errInstruction)
	}
	var instruction RebalanceInstruction
	if errJSON := json.Unmarshal(payload, &instruction); errJSON != nil {
		t.Fatal(errJSON)
	}
	if instruction.ID != plan.ID || instruction.Term != plan.Term || instruction.Target.IP != plan.TargetIP || instruction.Target.Port != plan.TargetPort {
		t.Fatalf("unexpected instruction: %+v", instruction)
	}
	if payload, errInstruction := f.handlers[1].RebalanceInstruction(context.Background(), f.lifetimes[len(f.lifetimes)-1]); errInstruction != nil || len(payload) != 0 {
		t.Fatalf("instruction without a plan = %s, error = %v", payload, errInstruction)
	}
}

func TestRebalanceClaimsFenceTermIdentityAndTarget(t *testing.T) {
	f := newRebalanceFixture(t, 8)
	f.reconcile(t, 0)
	plan := f.plans(t)[0]
	for _, args := range [][]string{
		{"CLAIM", plan.ID, strconv.FormatInt(plan.Term+1, 10), plan.TargetIP, strconv.Itoa(plan.TargetPort)},
		{"CLAIM", plan.ID, strconv.FormatInt(plan.Term, 10), "wrong-target", strconv.Itoa(plan.TargetPort)},
	} {
		if rebalanceTestCommand(t, f.handlers[1], f.lifetime(plan), args...) {
			t.Fatal("invalid plan claim accepted")
		}
	}
	other := f.lifetimes[len(f.lifetimes)-1]
	if rebalanceTestCommand(t, f.handlers[1], other, "CLAIM", plan.ID, strconv.FormatInt(plan.Term, 10), plan.TargetIP, strconv.Itoa(plan.TargetPort)) {
		t.Fatal("another certificate claimed plan")
	}
	if !claimRebalanceTestPlan(t, f, plan) {
		t.Fatal("valid claim rejected")
	}
	if claimRebalanceTestPlan(t, f, plan) {
		t.Fatal("duplicate claim accepted")
	}
	if payload, errInstruction := f.handlers[1].RebalanceInstruction(context.Background(), f.lifetime(plan)); errInstruction != nil || len(payload) != 0 {
		t.Fatal("claimed plan redelivered")
	}
	if _, errHandle := f.handlers[1].HandleRebalance(context.Background(), ConnectionLifetime{}, []string{"CLUSTER", "BALANCE", "ENABLE"}); errHandle == nil {
		t.Fatal("bootstrap identity enabled balancing")
	}
}

func takeOverRebalanceTestPlan(t *testing.T, f *rebalanceFixture, plan CPARebalancePlanRecord) (ConnectionLifetime, int) {
	t.Helper()
	var original CPANodeMembershipRecord
	if errFirst := f.repo.db.First(&original, "certificate_fingerprint = ?", plan.Fingerprint).Error; errFirst != nil {
		t.Fatal(errFirst)
	}
	targetIndex := -1
	for index, home := range f.homes {
		if rebalanceHomeKey(home) == rebalanceHomeKey(rebalanceTarget(plan)) {
			targetIndex = index
		}
	}
	member, errSubscribe := f.repo.SubscribeMembership(context.Background(), SubscribeMembershipRequest{Fingerprint: plan.Fingerprint, NodeID: original.NodeID, Home: f.homes[targetIndex], ProtocolVersion: 1, LifecycleConfigRevision: original.LifecycleConfigRevision, Takeover: true})
	if errSubscribe != nil {
		t.Fatal(errSubscribe)
	}
	lifetime := ConnectionLifetime{Fingerprint: member.CertificateFingerprint, ConnectedAt: member.ConnectedAt, Home: f.homes[targetIndex], Controlled: true}
	return lifetime, targetIndex
}

func TestRebalanceSuccessCooldownAndLostFeedbackRecovery(t *testing.T) {
	f := newRebalanceFixture(t, 8)
	f.reconcile(t, 0)
	plans := f.plans(t)
	for index, plan := range plans {
		if !claimRebalanceTestPlan(t, f, plan) {
			t.Fatal("claim rejected")
		}
		nowBeforeHandoff, errNow := DatabaseNow(context.Background(), f.repo.db)
		if errNow != nil {
			t.Fatal(errNow)
		}
		if errUpdate := f.repo.db.Model(&CPANodeMembershipRecord{}).Where("certificate_fingerprint = ?", plan.Fingerprint).
			Updates(map[string]any{"last_seen_at": nowBeforeHandoff.Add(-4 * time.Second), "cpa_heartbeat_timeout": 3 * time.Second}).Error; errUpdate != nil {
			t.Fatal(errUpdate)
		}
		if index == 1 {
			// Lifecycle cleanup can start cancellation while the CPA settles pending dispatches.
			if errCleanup := f.repo.CleanupStaleMemberships(context.Background()); errCleanup != nil {
				t.Fatal(errCleanup)
			}
		}
		f.reconcile(t, 0)
		var inProgress CPARebalancePlanRecord
		if errFirst := f.repo.db.First(&inProgress, "id = ?", plan.ID).Error; errFirst != nil {
			t.Fatal(errFirst)
		}
		if inProgress.State != rebalanceExecuting {
			t.Fatalf("plan during handoff = %s, want executing", inProgress.State)
		}
		lifetime, target := takeOverRebalanceTestPlan(t, f, plan)
		if !rebalanceTestCommand(t, f.handlers[target], lifetime, "ENABLE") {
			t.Fatal("target registration failed")
		}
		if index == 0 {
			if !rebalanceTestCommand(t, f.handlers[target], lifetime, "RESULT", plan.ID, "success") {
				t.Fatal("success rejected")
			}
		} else {
			f.reconcile(t, 0)
		}
		var saved CPARebalancePlanRecord
		if errFirst := f.repo.db.First(&saved, "id = ?", plan.ID).Error; errFirst != nil {
			t.Fatal(errFirst)
		}
		var client CPARebalanceClientRecord
		if errFirst := f.repo.db.First(&client, "fingerprint = ?", plan.Fingerprint).Error; errFirst != nil {
			t.Fatal(errFirst)
		}
		now, errNow := DatabaseNow(context.Background(), f.repo.db)
		if errNow != nil {
			t.Fatal(errNow)
		}
		if saved.State != rebalanceCompleted || client.NextEligibleAt.Before(nowBeforeHandoff.Add(rebalanceCooldown)) || client.NextEligibleAt.After(now.Add(rebalanceCooldown)) {
			t.Fatal("success did not start the full persisted cooldown")
		}
		if !rebalanceTestCommand(t, f.handlers[target], lifetime, "RESULT", plan.ID, "success") {
			t.Fatal("idempotent success rejected")
		}
		var after CPARebalanceClientRecord
		if errFirst := f.repo.db.First(&after, "fingerprint = ?", plan.Fingerprint).Error; errFirst != nil {
			t.Fatal(errFirst)
		}
		if !after.NextEligibleAt.Equal(client.NextEligibleAt) {
			t.Fatal("duplicate feedback extended cooldown")
		}
	}
}

func TestRebalanceFailureAndExpiryBackoff(t *testing.T) {
	f := newRebalanceFixture(t, 8)
	f.reconcile(t, 0)
	plans := f.plans(t)
	if !claimRebalanceTestPlan(t, f, plans[0]) {
		t.Fatal("claim rejected")
	}
	if !rebalanceTestCommand(t, f.handlers[1], f.lifetime(plans[0]), "RESULT", plans[0].ID, "failed") {
		t.Fatal("failure rejected")
	}
	now, errNow := DatabaseNow(context.Background(), f.repo.db)
	if errNow != nil {
		t.Fatal(errNow)
	}
	if errUpdate := f.repo.db.Model(&CPARebalancePlanRecord{}).Where("id = ?", plans[1].ID).Update("expires_at", now.Add(-time.Second)).Error; errUpdate != nil {
		t.Fatal(errUpdate)
	}
	f.reconcile(t, 0)
	for _, plan := range plans {
		var saved CPARebalancePlanRecord
		if errFirst := f.repo.db.First(&saved, "id = ?", plan.ID).Error; errFirst != nil {
			t.Fatal(errFirst)
		}
		var client CPARebalanceClientRecord
		if errFirst := f.repo.db.First(&client, "fingerprint = ?", plan.Fingerprint).Error; errFirst != nil {
			t.Fatal(errFirst)
		}
		if saved.State != rebalanceFailed || client.FailureBackoff != time.Minute || !client.TargetRetryAt.After(now) {
			t.Fatal("failure/expiry did not apply backoff")
		}
	}
}

func TestRebalanceNewMasterInvalidatesPendingButAdoptsExecuting(t *testing.T) {
	f := newRebalanceFixture(t, 8)
	f.reconcile(t, 0)
	plans := f.plans(t)
	var executing, pending CPARebalancePlanRecord
	for _, plan := range plans {
		if plan.TargetIP == f.homes[2].IP {
			executing = plan
		} else {
			pending = plan
		}
	}
	if !claimRebalanceTestPlan(t, f, executing) {
		t.Fatal("claim rejected")
	}
	now, errNow := DatabaseNow(context.Background(), f.repo.db)
	if errNow != nil {
		t.Fatal(errNow)
	}
	if errUpdate := f.repo.db.Model(&CPANodeMembershipRecord{}).Where("certificate_fingerprint = ?", executing.Fingerprint).
		Updates(map[string]any{"last_seen_at": now.Add(-4 * time.Second), "cpa_heartbeat_timeout": 3 * time.Second}).Error; errUpdate != nil {
		t.Fatal(errUpdate)
	}
	if errUpdate := f.repo.db.Model(&ClusterNodeRecord{}).Where("ip = ?", f.homes[0].IP).Update("last_seen_at", now.Add(-time.Minute)).Error; errUpdate != nil {
		t.Fatal(errUpdate)
	}
	if errBeat := f.coordinators[1].heartbeatAndElect(context.Background(), f.homes[1]); errBeat != nil {
		t.Fatal(errBeat)
	}
	if claimRebalanceTestPlan(t, f, pending) {
		t.Fatal("old-term pending plan claimed")
	}
	f.reconcile(t, 1)
	var savedPending, savedExecuting CPARebalancePlanRecord
	if errFirst := f.repo.db.First(&savedPending, "id = ?", pending.ID).Error; errFirst != nil {
		t.Fatal(errFirst)
	}
	if errFirst := f.repo.db.First(&savedExecuting, "id = ?", executing.ID).Error; errFirst != nil {
		t.Fatal(errFirst)
	}
	if savedPending.State != rebalanceCanceled || savedExecuting.State != rebalanceExecuting {
		t.Fatal("new master did not invalidate/adopt plans correctly")
	}
	lifetime, target := takeOverRebalanceTestPlan(t, f, executing)
	if !rebalanceTestCommand(t, f.handlers[target], lifetime, "RESULT", executing.ID, "success") {
		t.Fatal("new term rejected committed work completion")
	}
	var gate ClusterMasterGateRecord
	if errFirst := f.repo.db.First(&gate, "id = ?", 1).Error; errFirst != nil {
		t.Fatal(errFirst)
	}
	if gate.Term <= pending.Term {
		t.Fatal("leadership term did not advance")
	}
	before := len(f.plans(t))
	f.reconcile(t, 0)
	if len(f.plans(t)) != before {
		t.Fatal("old master created plans")
	}
}

func TestRebalanceClaimRechecksLoadsAndReservations(t *testing.T) {
	f := newRebalanceFixture(t, 8)
	f.reconcile(t, 0)
	var plan CPARebalancePlanRecord
	for _, candidate := range f.plans(t) {
		if candidate.TargetIP == f.homes[0].IP {
			plan = candidate
		}
	}
	for index := 4; index < 8; index++ {
		member := f.members[index]
		if _, errSubscribe := f.repo.SubscribeMembership(context.Background(), SubscribeMembershipRequest{Fingerprint: member.CertificateFingerprint, NodeID: member.NodeID, Home: f.homes[0], ProtocolVersion: 1, LifecycleConfigRevision: member.LifecycleConfigRevision, Takeover: true}); errSubscribe != nil {
			t.Fatal(errSubscribe)
		}
	}
	if claimRebalanceTestPlan(t, f, plan) {
		t.Fatal("obsolete load decision was executed")
	}
	var saved CPARebalancePlanRecord
	if errFirst := f.repo.db.First(&saved, "id = ?", plan.ID).Error; errFirst != nil {
		t.Fatal(errFirst)
	}
	if saved.State != rebalanceCanceled {
		t.Fatal("obsolete plan did not release its reservation")
	}
}

func TestRebalanceExpiredLeadershipAndRetiredMasterStopInstructions(t *testing.T) {
	f := newRebalanceFixture(t, 8)
	f.reconcile(t, 0)
	plan := f.plans(t)[0]
	now, errNow := DatabaseNow(context.Background(), f.repo.db)
	if errNow != nil {
		t.Fatal(errNow)
	}
	if errUpdate := f.repo.db.Model(&ClusterMasterGateRecord{}).Where("id = ?", 1).Update("lease_until", now.Add(-time.Second)).Error; errUpdate != nil {
		t.Fatal(errUpdate)
	}
	if payload, errInstruction := f.handlers[1].RebalanceInstruction(context.Background(), f.lifetime(plan)); errInstruction != nil || len(payload) != 0 {
		t.Fatal("expired master delivered instruction")
	}
	if claimRebalanceTestPlan(t, f, plan) {
		t.Fatal("expired master authorized claim")
	}
	if errBeat := f.coordinators[0].heartbeatAndElect(context.Background(), f.homes[0]); errBeat != nil {
		t.Fatal(errBeat)
	}
	var gate ClusterMasterGateRecord
	if errFirst := f.repo.db.First(&gate, "id = ?", 1).Error; errFirst != nil {
		t.Fatal(errFirst)
	}
	if gate.Term <= plan.Term {
		t.Fatal("reacquiring an expired lease reused its term")
	}
	if errRetire := f.repo.RetireHomeIncarnation(context.Background(), f.homes[0]); errRetire != nil {
		t.Fatal(errRetire)
	}
	if payload, errInstruction := f.handlers[1].RebalanceInstruction(context.Background(), f.lifetime(plan)); errInstruction != nil || len(payload) != 0 {
		t.Fatal("retired master delivered instruction")
	}
}

func TestRebalanceConvergesWithBoundedPlans(t *testing.T) {
	for _, count := range []int{5, 8, 40} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			f := newRebalanceFixture(t, count)
			for round := range 30 {
				if errUpdate := f.repo.db.Model(&ClusterMasterGateRecord{}).Where("id = ?", 1).Update("last_balance_at", time.Time{}).Error; errUpdate != nil {
					t.Fatal(errUpdate)
				}
				f.reconcile(t, 0)
				plans := f.plans(t)
				if len(plans) > rebalanceMaxActive {
					t.Fatal("migration quota exceeded")
				}
				for _, plan := range plans {
					if !claimRebalanceTestPlan(t, f, plan) {
						t.Fatal("reserved migration unexpectedly rejected")
					}
					lifetime, target := takeOverRebalanceTestPlan(t, f, plan)
					if !rebalanceTestCommand(t, f.handlers[target], lifetime, "ENABLE") || !rebalanceTestCommand(t, f.handlers[target], lifetime, "RESULT", plan.ID, "success") {
						t.Fatal("migration feedback rejected")
					}
				}
				var members []CPANodeMembershipRecord
				if errFind := f.repo.db.Find(&members).Error; errFind != nil {
					t.Fatal(errFind)
				}
				counts := make([]int, len(f.homes))
				for _, member := range members {
					for index, home := range f.homes {
						if membershipOwnedByHome(member, home) {
							counts[index]++
						}
					}
				}
				minimum, maximum := counts[0], counts[0]
				for _, load := range counts {
					minimum, maximum = min(minimum, load), max(maximum, load)
				}
				if maximum-minimum <= 1 {
					t.Logf("%d CPAs converged to %v in %d rounds", count, counts, round+1)
					return
				}
				if len(plans) == 0 {
					t.Fatalf("planner stalled at %v", counts)
				}
			}
			t.Fatal("planner did not converge")
		})
	}
}

func TestRebalanceSkipsUnregisteredAndStaleCapabilities(t *testing.T) {
	f := newRebalanceFixture(t, 8)
	if errUpdate := f.repo.db.Model(&CPARebalanceClientRecord{}).Where("1 = 1").Update("connected_at", time.Unix(1, 0)).Error; errUpdate != nil {
		t.Fatal(errUpdate)
	}
	f.reconcile(t, 0)
	if len(f.plans(t)) != 0 {
		t.Fatal("old connection capability selected")
	}
	if errDelete := f.repo.db.Where("1 = 1").Delete(&CPARebalanceClientRecord{}).Error; errDelete != nil {
		t.Fatal(errDelete)
	}
	if errUpdate := f.repo.db.Model(&ClusterMasterGateRecord{}).Where("id = ?", 1).Update("last_balance_at", time.Time{}).Error; errUpdate != nil {
		t.Fatal(errUpdate)
	}
	f.reconcile(t, 0)
	if len(f.plans(t)) != 0 {
		t.Fatal("old CPA selected")
	}
}
