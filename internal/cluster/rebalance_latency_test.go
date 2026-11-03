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

func latencyTestState(t *testing.T, f *rebalanceFixture) (ClusterMasterGateRecord, time.Time, map[string]rebalanceHome, []CPANodeMembershipRecord, map[string]int, map[string]CPARebalanceClientRecord) {
	t.Helper()
	now, errNow := DatabaseNow(context.Background(), f.repo.db)
	if errNow != nil {
		t.Fatal(errNow)
	}
	var gate ClusterMasterGateRecord
	if errFirst := f.repo.db.First(&gate, "id = ?", 1).Error; errFirst != nil {
		t.Fatal(errFirst)
	}
	snapshot, errHomes := loadRebalanceHomes(f.repo.db, now, defaultHeartbeatTimeout)
	if errHomes != nil {
		t.Fatal(errHomes)
	}
	homes := snapshot.homes
	members, loads, errMembers := loadRebalanceMembers(f.repo.db, homes, now)
	if errMembers != nil {
		t.Fatal(errMembers)
	}
	var records []CPARebalanceClientRecord
	if errFind := f.repo.db.Find(&records).Error; errFind != nil {
		t.Fatal(errFind)
	}
	clients := make(map[string]CPARebalanceClientRecord)
	for _, record := range records {
		clients[record.Fingerprint] = record
	}
	return gate, now, homes, members, loads, clients
}

func newLatencySwapFixture(t *testing.T) *rebalanceFixture {
	t.Helper()
	return seedLatencySwapFixture(t, newCredentialFoundationTestRepository(t))
}

func seedLatencySwapFixture(t *testing.T, repo *Repository) *rebalanceFixture {
	t.Helper()
	f := seedRebalanceFixture(t, repo, 4)
	if errDelete := f.repo.db.Where("ip = ? AND port = ?", f.homes[2].IP, f.homes[2].Port).Delete(&ClusterNodeRecord{}).Error; errDelete != nil {
		t.Fatal(errDelete)
	}
	for index := range 2 {
		member, errSubscribe := f.repo.SubscribeMembership(context.Background(), SubscribeMembershipRequest{Fingerprint: f.members[index].CertificateFingerprint, NodeID: f.members[index].NodeID, Home: f.homes[0], ProtocolVersion: 1, LifecycleConfigRevision: f.members[index].LifecycleConfigRevision, Takeover: true})
		if errSubscribe != nil {
			t.Fatal(errSubscribe)
		}
		f.members[index] = member
		f.lifetimes[index] = ConnectionLifetime{Fingerprint: member.CertificateFingerprint, ConnectedAt: member.ConnectedAt, Home: f.homes[0], Controlled: true}
		if !rebalanceTestCommand(t, f.handlers[0], f.lifetimes[index], "ENABLE") {
			t.Fatal("enable rejected")
		}
	}
	for _, lifetime := range f.lifetimes {
		if errUpdate := f.repo.db.Model(&CPARebalanceClientRecord{}).Where("fingerprint = ?", lifetime.Fingerprint).Update("latency_connected_at", lifetime.ConnectedAt).Error; errUpdate != nil {
			t.Fatal(errUpdate)
		}
	}
	return f
}

func latencyTestSamples(members []CPANodeMembershipRecord, homes map[string]rebalanceHome, now time.Time, current, target time.Duration) map[string]CPALatencyRecord {
	samples := make(map[string]CPALatencyRecord)
	for _, member := range members {
		for _, home := range homes {
			rtt := target
			if member.HomeIP == home.id.IP && member.HomePort == home.id.Port {
				rtt = current
			}
			samples[latencyKey(member.CertificateFingerprint, home.id.IP, home.id.Port)] = CPALatencyRecord{Fingerprint: member.CertificateFingerprint, Address: latencyAddress(home.id.IP, home.id.Port), RTT: rtt, SampledAt: now, Valid: true, UpdatedAt: now}
		}
	}
	return samples
}

func createLatencyTestSwap(t *testing.T, f *rebalanceFixture) []CPARebalancePlanRecord {
	t.Helper()
	gate, now, homes, members, loads, clients := latencyTestState(t, f)
	for _, offset := range []time.Duration{-3 * time.Minute, -2 * time.Minute, -time.Minute, 0} {
		at := now.Add(offset)
		samples := latencyTestSamples(members, homes, at, 100*time.Millisecond, 10*time.Millisecond)
		if errPlan := f.repo.db.Transaction(func(tx *gorm.DB) error {
			return planLatencySwap(tx, gate, at, homes, members, loads, clients, samples, 0)
		}); errPlan != nil {
			t.Fatal(errPlan)
		}
	}
	plans := f.plans(t)
	if len(plans) != 2 || plans[0].SwapGroupID == "" || plans[0].SwapGroupID != plans[1].SwapGroupID {
		t.Fatalf("expected reciprocal swap, got %+v", plans)
	}
	return plans
}

func TestLatencyReportFreshnessIdentityAndFailure(t *testing.T) {
	f := newRebalanceFixture(t, 1)
	_, now, homes, _, _, clients := latencyTestState(t, f)
	lifetime := f.lifetimes[0]
	sample := latencySample{IP: lifetime.Home.IP, Port: lifetime.Home.Port, Sequence: 3, ObservedAt: now.Add(-time.Second), AgeMS: 1000, RTTUS: 100000, Valid: true}
	report := latencyReport{ProcessID: "process", Samples: []latencySample{sample}}
	accept := func(at time.Time) *RebalanceTarget {
		t.Helper()
		raw, errJSON := json.Marshal(report)
		if errJSON != nil {
			t.Fatal(errJSON)
		}
		var probe *RebalanceTarget
		if errAccept := f.repo.db.Transaction(func(tx *gorm.DB) error {
			var errReport error
			probe, errReport = acceptLatencyReport(tx, lifetime, string(raw), at, homes, clients[lifetime.Fingerprint])
			return errReport
		}); errAccept != nil {
			t.Fatal(errAccept)
		}
		return probe
	}
	if probe := accept(now); probe == nil || (probe.IP == lifetime.Home.IP && probe.Port == lifetime.Home.Port) {
		t.Fatal("high current latency did not request a communication-only probe")
	}
	var first CPALatencyRecord
	if errFirst := f.repo.db.First(&first).Error; errFirst != nil {
		t.Fatal(errFirst)
	}
	// A repeated sequence, even with a changed observation timestamp, must not renew freshness.
	report.Samples[0].ObservedAt = now.Add(time.Second)
	accept(now.Add(time.Minute))
	var duplicate CPALatencyRecord
	if errFirst := f.repo.db.First(&duplicate).Error; errFirst != nil {
		t.Fatal(errFirst)
	}
	if !duplicate.SampledAt.Equal(first.SampledAt) {
		t.Fatal("duplicate sample renewed TTL")
	}
	report.Samples[0] = sample
	report.Samples[0].Sequence--
	accept(now)
	report.Samples[0].Sequence = 4
	report.Samples[0].ObservedAt = now
	report.Samples[0].AgeMS = 0
	report.Samples[0].Valid = false
	accept(now)
	samples, errSamples := loadRebalanceLatencies(f.repo.db, now)
	if errSamples != nil || len(samples) != 0 {
		t.Fatalf("failed sample remained usable: %+v, %v", samples, errSamples)
	}
	report.Samples[0].Sequence = 5
	report.Samples[0].ObservedAt = now.Add(-rebalanceLatencyTTL - time.Second)
	report.Samples[0].Valid = true
	accept(now)
	if errFirst := f.repo.db.First(&duplicate).Error; errFirst != nil || duplicate.Sequence != 4 {
		t.Fatal("stale report replaced current failure")
	}
	raw, errJSON := json.Marshal(report)
	if errJSON != nil {
		t.Fatal(errJSON)
	}
	forged := lifetime
	forged.ConnectedAt = forged.ConnectedAt.Add(-time.Second)
	if _, errHandle := f.handlers[1].HandleRebalance(context.Background(), forged, []string{"CLUSTER", "BALANCE", "LATENCY", string(raw)}); errHandle == nil {
		t.Fatal("stale membership submitted telemetry")
	}
	if errUpdate := f.repo.db.Model(&CPALatencyRecord{}).Where("fingerprint = ?", lifetime.Fingerprint).UpdateColumn("updated_at", now.Add(-25*time.Hour)).Error; errUpdate != nil {
		t.Fatal(errUpdate)
	}
	if _, errPurge := f.repo.PurgeExpiredCPALatencySummaries(context.Background(), now, expiredCleanupBatch); errPurge != nil {
		t.Fatal(errPurge)
	}
	var count int64
	if errCount := f.repo.db.Model(&CPALatencyRecord{}).Count(&count).Error; errCount != nil || count != 0 {
		t.Fatalf("latency summaries were not purged: count=%d error=%v now=%v previous=%+v", count, errCount, now, duplicate)
	}
}

func TestLatencySwapRequiresSustainedFreshEvidence(t *testing.T) {
	for _, scenario := range []string{"fresh", "same sample", "one stale direction", "small gain", "spike", "one loses", "unknown", "mixed homes", "cooldown", "disconnected capability"} {
		t.Run(scenario, func(t *testing.T) {
			f := newLatencySwapFixture(t)
			gate, end, homes, members, loads, clients := latencyTestState(t, f)
			start := end.Add(-3 * time.Minute)
			if scenario == "mixed homes" {
				home := homes[rebalanceHomeKey(f.homes[1])]
				home.latencyCapable = false
				homes[rebalanceHomeKey(f.homes[1])] = home
			}
			if scenario == "cooldown" || scenario == "disconnected capability" {
				for key, client := range clients {
					if scenario == "cooldown" {
						client.OptimizationEligibleAt = end.Add(time.Minute)
					} else {
						client.LatencyConnectedAt = client.ConnectedAt.Add(-time.Second)
					}
					clients[key] = client
				}
			}
			for round := range 4 {
				at := start.Add(time.Duration(round) * time.Minute)
				sampledAt := at
				if scenario == "same sample" {
					sampledAt = start
				}
				current, target := 100*time.Millisecond, 10*time.Millisecond
				if scenario == "small gain" || (scenario == "spike" && round > 0) {
					current, target = 12*time.Millisecond, 10*time.Millisecond
				}
				samples := latencyTestSamples(members, homes, sampledAt, current, target)
				for key, sample := range samples {
					if scenario == "unknown" && sample.Address == latencyAddress(f.homes[0].IP, f.homes[0].Port) {
						delete(samples, key)
					}
					if scenario == "one loses" && sample.Address == latencyAddress(f.homes[0].IP, f.homes[0].Port) && sample.Fingerprint >= members[2].CertificateFingerprint {
						sample.RTT = 200 * time.Millisecond
						samples[key] = sample
					}
					if scenario == "one stale direction" && sample.Address == latencyAddress(f.homes[0].IP, f.homes[0].Port) && sample.Fingerprint >= members[2].CertificateFingerprint {
						sample.SampledAt = start
						samples[key] = sample
					}
				}
				// A new planner/term must retain valid confirmation, not restart it or skip fresh evidence.
				gate.Term = int64(round + 1)
				if errPlan := f.repo.db.Transaction(func(tx *gorm.DB) error {
					return planLatencySwap(tx, gate, at, homes, members, loads, clients, samples, 0)
				}); errPlan != nil {
					t.Fatal(errPlan)
				}
				want := 0
				if scenario == "fresh" && round == 3 {
					want = 2
				}
				if plans := f.plans(t); len(plans) != want {
					t.Fatalf("round %d plans = %d, want %d", round, len(plans), want)
				}
			}
		})
	}
}

func TestLatencySampleFreshnessGrace(t *testing.T) {
	f := newLatencySwapFixture(t)
	_, now, homes, members, _, _ := latencyTestState(t, f)
	samples := latencyTestSamples(members, homes, now, 100*time.Millisecond, 10*time.Millisecond)
	for _, test := range []struct {
		name   string
		age    time.Duration
		valid  bool
		usable bool
	}{
		{name: "report grace", age: 2*time.Minute + 5*time.Second, valid: true, usable: true},
		{name: "freshness boundary", age: 3 * time.Minute, valid: true, usable: true},
		{name: "expired", age: 3*time.Minute + time.Millisecond, valid: true},
		{name: "failed", age: time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, sample := range samples {
				sample.SampledAt, sample.Valid = now.Add(-test.age), test.valid
				if errSave := f.repo.db.Save(&sample).Error; errSave != nil {
					t.Fatal(errSave)
				}
			}
			loaded, errLoad := loadRebalanceLatencies(f.repo.db, now)
			if errLoad != nil {
				t.Fatal(errLoad)
			}
			want := 0
			if test.usable {
				want = len(samples)
			}
			if len(loaded) != want {
				t.Fatalf("usable samples = %d, want %d", len(loaded), want)
			}
		})
	}
}

func TestLatencyCountBalanceIgnoresPerformanceThresholdAndRanksCandidates(t *testing.T) {
	f := newRebalanceFixture(t, 4)
	if errDelete := f.repo.db.Where("ip = ?", f.homes[2].IP).Delete(&ClusterNodeRecord{}).Error; errDelete != nil {
		t.Fatal(errDelete)
	}
	_, now, homes, members, _, _ := latencyTestState(t, f)
	samples := latencyTestSamples(members, homes, now, 10*time.Millisecond, 20*time.Millisecond)
	for key, record := range samples {
		// fp-03 is the least harmful move despite being last in the legacy order.
		if record.Fingerprint == "fp-03" && record.Address == latencyAddress(f.homes[0].IP, f.homes[0].Port) {
			record.RTT = 15 * time.Millisecond
			samples[key] = record
		}
		if errSave := f.repo.db.Save(&record).Error; errSave != nil {
			t.Fatal(errSave)
		}
	}
	for round := range 2 {
		if errUpdate := f.repo.db.Model(&ClusterMasterGateRecord{}).Where("id = ?", 1).Update("last_balance_at", time.Time{}).Error; errUpdate != nil {
			t.Fatal(errUpdate)
		}
		f.reconcile(t, 0)
		plans := f.plans(t)
		if len(plans) != 1 || plans[0].SwapGroupID != "" {
			t.Fatalf("count migration blocked by a slower target: %+v", plans)
		}
		if round == 0 && plans[0].Fingerprint != "fp-03" {
			t.Fatal("count migration did not prefer the least latency penalty")
		}
		if !claimRebalanceTestPlan(t, f, plans[0]) {
			t.Fatal("count claim rejected")
		}
		lifetime, target := takeOverRebalanceTestPlan(t, f, plans[0])
		if !rebalanceTestCommand(t, f.handlers[target], lifetime, "RESULT", plans[0].ID, "success") {
			t.Fatal("count success rejected")
		}
	}
	_, _, _, _, loads, _ := latencyTestState(t, f)
	for _, load := range loads {
		if load != 2 {
			t.Fatalf("did not converge to 2:2: %v", loads)
		}
	}
}

func TestLatencySwapClaimsReservationsAndCooldown(t *testing.T) {
	f := newLatencySwapFixture(t)
	plans := createLatencyTestSwap(t, f)
	for index, plan := range plans {
		if !claimRebalanceTestPlan(t, f, plan) {
			t.Fatalf("balanced-load swap claim %d rejected", index)
		}
		lifetime, target := takeOverRebalanceTestPlan(t, f, plan)
		if !rebalanceTestCommand(t, f.handlers[target], lifetime, "RESULT", plan.ID, "success") {
			t.Fatal("swap success rejected")
		}
		if index == 0 {
			if errUpdate := f.repo.db.Model(&ClusterMasterGateRecord{}).Where("id = ?", 1).Update("last_balance_at", time.Time{}).Error; errUpdate != nil {
				t.Fatal(errUpdate)
			}
			f.reconcile(t, 0)
			if remaining := f.plans(t); len(remaining) != 1 || remaining[0].SwapGroupID != plan.SwapGroupID {
				t.Fatalf("temporary 3:1 caused a reverse move: %+v", remaining)
			}
		}
	}
	_, now, _, _, loads, clients := latencyTestState(t, f)
	for _, load := range loads {
		if load != 2 {
			t.Fatalf("swap changed counts: %v", loads)
		}
	}
	for _, plan := range plans {
		if clients[plan.Fingerprint].OptimizationEligibleAt.Before(now.Add(rebalanceLatencyCooldown - time.Second)) {
			t.Fatal("successful optimization lost its durable cooldown")
		}
	}
}

func TestLatencySwapSingleFailureAndTermChange(t *testing.T) {
	for _, scenario := range []string{"pending failure", "after one completion", "term change", "mixed claim", "expiry"} {
		t.Run(scenario, func(t *testing.T) {
			f := newLatencySwapFixture(t)
			plans := createLatencyTestSwap(t, f)
			switch scenario {
			case "pending failure":
				rebalanceTestCommand(t, f.handlers[1], f.lifetime(plans[0]), "RESULT", plans[0].ID, "failed")
			case "after one completion":
				if !claimRebalanceTestPlan(t, f, plans[0]) {
					t.Fatal("first claim rejected")
				}
				lifetime, target := takeOverRebalanceTestPlan(t, f, plans[0])
				rebalanceTestCommand(t, f.handlers[target], lifetime, "RESULT", plans[0].ID, "success")
				rebalanceTestCommand(t, f.handlers[1], f.lifetime(plans[1]), "RESULT", plans[1].ID, "failed")
			case "term change":
				if errUpdate := f.repo.db.Model(&ClusterMasterGateRecord{}).Where("id = ?", 1).Update("term", plans[0].Term+1).Error; errUpdate != nil {
					t.Fatal(errUpdate)
				}
				if claimRebalanceTestPlan(t, f, plans[0]) {
					t.Fatal("stale-term swap claim accepted")
				}
				f.reconcile(t, 0)
			case "mixed claim":
				if errUpdate := f.repo.db.Model(&HomeProcessIncarnationRecord{}).Where("home_ip = ?", f.homes[1].IP).Update("capabilities", JSONB(`["cpa_rebalance_v1"]`)).Error; errUpdate != nil {
					t.Fatal(errUpdate)
				}
				if claimRebalanceTestPlan(t, f, plans[0]) {
					t.Fatal("mixed-version swap claim accepted")
				}
			case "expiry":
				now, errNow := DatabaseNow(context.Background(), f.repo.db)
				if errNow != nil {
					t.Fatal(errNow)
				}
				if errUpdate := f.repo.db.Model(&CPARebalancePlanRecord{}).Where("id = ?", plans[0].ID).Update("expires_at", now.Add(-time.Second)).Error; errUpdate != nil {
					t.Fatal(errUpdate)
				}
				f.reconcile(t, 0)
			}
			if active := f.plans(t); len(active) != 0 {
				t.Fatalf("failed group left unexecuted reservations: %+v", active)
			}
			if scenario == "after one completion" {
				// Performance cooldown never blocks necessary count repair.
				if errUpdate := f.repo.db.Model(&CPARebalanceClientRecord{}).Where("1 = 1").Updates(map[string]any{"next_eligible_at": time.Time{}, "optimization_eligible_at": time.Now().Add(time.Hour)}).Error; errUpdate != nil {
					t.Fatal(errUpdate)
				}
				if errUpdate := f.repo.db.Model(&ClusterMasterGateRecord{}).Where("id = ?", 1).Update("last_balance_at", time.Time{}).Error; errUpdate != nil {
					t.Fatal(errUpdate)
				}
				f.reconcile(t, 0)
				if repair := f.plans(t); len(repair) != 1 || repair[0].SwapGroupID != "" {
					t.Fatalf("actual post-failure counts were not repaired: %+v", repair)
				}
			}
		})
	}
}

func TestLatencySwapKeepsAuthorizedPeerOnFailureOrElection(t *testing.T) {
	for _, scenario := range []string{"failure", "election"} {
		t.Run(scenario, func(t *testing.T) {
			f := newLatencySwapFixture(t)
			plans := createLatencyTestSwap(t, f)
			if !claimRebalanceTestPlan(t, f, plans[0]) {
				t.Fatal("claim rejected")
			}
			if scenario == "failure" {
				rebalanceTestCommand(t, f.handlers[1], f.lifetime(plans[1]), "RESULT", plans[1].ID, "failed")
			} else {
				if errUpdate := f.repo.db.Model(&ClusterMasterGateRecord{}).Where("id = ?", 1).Update("term", plans[0].Term+1).Error; errUpdate != nil {
					t.Fatal(errUpdate)
				}
				f.reconcile(t, 0)
			}
			var authorized, peer CPARebalancePlanRecord
			if errFirst := f.repo.db.First(&authorized, "id = ?", plans[0].ID).Error; errFirst != nil {
				t.Fatal(errFirst)
			}
			if errFirst := f.repo.db.First(&peer, "id = ?", plans[1].ID).Error; errFirst != nil {
				t.Fatal(errFirst)
			}
			if authorized.State != rebalanceExecuting || (peer.State != rebalanceFailed && peer.State != rebalanceCanceled) {
				t.Fatalf("authorized work lost or peer not canceled: %s/%s", authorized.State, peer.State)
			}
			lifetime, target := takeOverRebalanceTestPlan(t, f, plans[0])
			if !rebalanceTestCommand(t, f.handlers[target], lifetime, "RESULT", plans[0].ID, "success") {
				t.Fatal("adopted executing work could not complete")
			}
		})
	}
}

func TestLatencySwapRejectsPeerThatReturnedToANewSourceLifetime(t *testing.T) {
	f := newLatencySwapFixture(t)
	plans := createLatencyTestSwap(t, f)
	if !claimRebalanceTestPlan(t, f, plans[0]) {
		t.Fatal("claim rejected")
	}
	var original CPANodeMembershipRecord
	if errFirst := f.repo.db.First(&original, "certificate_fingerprint = ?", plans[0].Fingerprint).Error; errFirst != nil {
		t.Fatal(errFirst)
	}
	if _, errSubscribe := f.repo.SubscribeMembership(context.Background(), SubscribeMembershipRequest{Fingerprint: original.CertificateFingerprint, NodeID: original.NodeID, Home: rebalanceSource(plans[0]), ProtocolVersion: 1, LifecycleConfigRevision: original.LifecycleConfigRevision, Takeover: true}); errSubscribe != nil {
		t.Fatal(errSubscribe)
	}
	if claimRebalanceTestPlan(t, f, plans[1]) {
		t.Fatal("peer rollback lifetime was mistaken for an in-progress swap")
	}
	f.reconcile(t, 0)
	if active := f.plans(t); len(active) != 0 {
		t.Fatalf("failed execution retained reservations after rollback: %+v", active)
	}
}

func TestLatencyReportRejectsMalformedPayloadWithoutMutation(t *testing.T) {
	f := newRebalanceFixture(t, 1)
	for _, args := range [][]string{
		{"CLUSTER", "BALANCE", "LATENCY"},
		{"CLUSTER", "BALANCE", "LATENCY", "{"},
		{"CLUSTER", "BALANCE", "LATENCY", `{"process_id":7,"samples":[]}`},
		{"CLUSTER", "BALANCE", "LATENCY", `{"process_id":"p","samples":"invalid"}`},
	} {
		if _, errHandle := f.handlers[1].HandleRebalance(context.Background(), f.lifetimes[0], args); errHandle == nil {
			t.Fatalf("invalid latency report was accepted: %v", args)
		}
	}
	var count int64
	if errCount := f.repo.db.Model(&CPALatencyRecord{}).Count(&count).Error; errCount != nil {
		t.Fatal(errCount)
	}
	if count != 0 {
		t.Fatal("invalid latency report wrote summaries")
	}
	var client CPARebalanceClientRecord
	if errFirst := f.repo.db.First(&client, "fingerprint = ?", f.lifetimes[0].Fingerprint).Error; errFirst != nil {
		t.Fatal(errFirst)
	}
	if !client.LatencyConnectedAt.IsZero() {
		t.Fatal("invalid latency report enabled telemetry")
	}
}

func TestLatencyReportRESPNegotiation(t *testing.T) {
	f := newRebalanceFixture(t, 1)
	_, now, _, _, _, _ := latencyTestState(t, f)
	payload := fmt.Sprintf(`{"process_id":"p","samples":[{"ip":%q,"port":%d,"sequence":1,"observed_at":%q,"age_ms":0,"rtt_us":100000,"valid":true}]}`, f.homes[1].IP, f.homes[1].Port, now.Format(time.RFC3339Nano))
	if !rebalanceTestCommand(t, f.handlers[1], f.lifetimes[0], "LATENCY", payload) {
		t.Fatal("latency report rejected")
	}
	var client CPARebalanceClientRecord
	if errFirst := f.repo.db.First(&client).Error; errFirst != nil || !client.LatencyConnectedAt.Equal(f.lifetimes[0].ConnectedAt) {
		t.Fatalf("latency capability not bound to membership: %+v, %v", client, errFirst)
	}
	if rebalanceTestCommand(t, f.handlers[1], f.lifetimes[0], "CLAIM", "missing", strconv.Itoa(1), f.homes[0].IP, strconv.Itoa(f.homes[0].Port)) {
		t.Fatal("nonexistent plan accepted")
	}
}
