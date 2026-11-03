package cluster

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"gorm.io/gorm"
)

func TestLatencyReportOnlyUpdatesCapabilityForNewLifetime(t *testing.T) {
	f := newRebalanceFixture(t, 1)
	now, errNow := DatabaseNow(t.Context(), f.repo.db)
	if errNow != nil {
		t.Fatal(errNow)
	}
	clientUpdates := 0
	if errRegister := f.repo.db.Callback().Update().Before("gorm:update").Register("test:latency-capability-updates", func(tx *gorm.DB) {
		if tx.Statement.Table == "cpa_rebalance_clients" {
			clientUpdates++
		}
	}); errRegister != nil {
		t.Fatal(errRegister)
	}
	report := func(lifetime ConnectionLifetime, sequence int64, observedAt time.Time) {
		t.Helper()
		raw, errJSON := json.Marshal(latencyReport{ProcessID: "process", Samples: []latencySample{{IP: lifetime.Home.IP, Port: lifetime.Home.Port, Sequence: sequence, ObservedAt: observedAt, RTTUS: 10000, Valid: true}}})
		if errJSON != nil {
			t.Fatal(errJSON)
		}
		if !rebalanceTestCommand(t, f.handlers[1], lifetime, "LATENCY", string(raw)) {
			t.Fatal("report rejected")
		}
	}
	lifetime := f.lifetimes[0]
	report(lifetime, 1, now.Add(-2*time.Second))
	var initial CPARebalanceClientRecord
	if errFirst := f.repo.db.First(&initial, "fingerprint = ?", lifetime.Fingerprint).Error; errFirst != nil {
		t.Fatal(errFirst)
	}
	if clientUpdates != 1 || !initial.LatencyConnectedAt.Equal(lifetime.ConnectedAt) {
		t.Fatalf("first report did not bind capability: updates=%d client=%+v", clientUpdates, initial)
	}
	report(lifetime, 2, now.Add(-time.Second))
	report(lifetime, 2, now.Add(-time.Second))
	var after CPARebalanceClientRecord
	if errFirst := f.repo.db.First(&after, "fingerprint = ?", lifetime.Fingerprint).Error; errFirst != nil {
		t.Fatal(errFirst)
	}
	if clientUpdates != 1 || !after.UpdatedAt.Equal(initial.UpdatedAt) {
		t.Fatalf("same lifetime repeated capability writes: updates=%d client=%+v", clientUpdates, after)
	}
	var summary CPALatencyRecord
	if errFirst := f.repo.db.First(&summary, "fingerprint = ?", lifetime.Fingerprint).Error; errFirst != nil {
		t.Fatal(errFirst)
	}
	if summary.Sequence != 2 {
		t.Fatal("skipping capability write lost the new observation")
	}
	original := f.members[0]
	member, errSubscribe := f.repo.SubscribeMembership(context.Background(), SubscribeMembershipRequest{Fingerprint: original.CertificateFingerprint, NodeID: original.NodeID, Home: lifetime.Home, ProtocolVersion: 1, LifecycleConfigRevision: original.LifecycleConfigRevision, Takeover: true})
	if errSubscribe != nil {
		t.Fatal(errSubscribe)
	}
	lifetime.ConnectedAt = member.ConnectedAt
	if !rebalanceTestCommand(t, f.handlers[1], lifetime, "ENABLE") {
		t.Fatal("new lifetime registration rejected")
	}
	report(lifetime, 3, now)
	if errFirst := f.repo.db.First(&after, "fingerprint = ?", lifetime.Fingerprint).Error; errFirst != nil {
		t.Fatal(errFirst)
	}
	if clientUpdates != 2 || !after.LatencyConnectedAt.Equal(lifetime.ConnectedAt) {
		t.Fatalf("new lifetime did not renew capability: updates=%d client=%+v", clientUpdates, after)
	}
}
