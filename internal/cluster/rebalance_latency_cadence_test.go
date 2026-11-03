package cluster

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func TestLatencyConfirmationProbePrioritySafety(t *testing.T) {
	for _, side := range []int{0, 2} {
		for _, scenario := range []string{"active", "unknown target", "expired target", "expired confirmation", "new source lifetime", "new source incarnation", "new target incarnation", "not participant", "failure backoff", "lost improvement"} {
			t.Run(fmt.Sprintf("side=%d/%s", side, scenario), func(t *testing.T) {
				repo := newCredentialFoundationTestRepository(t)
				f := newLatencySwapSelectionFixture(3, 3)
				member, peer, alternative := f.members[side], f.members[2-side], f.members[1]
				lifetime := ConnectionLifetime{Fingerprint: member.CertificateFingerprint, ConnectedAt: member.ConnectedAt, Home: HomeIncarnationID{IP: member.HomeIP, Port: member.HomePort, StartedAt: member.HomeStartedAt}}
				client := f.clients[member.CertificateFingerprint]
				confirmation := CPALatencyConfirmationRecord{ID: "active", Signature: latencySwapSignature(f.members[0], f.members[2]), UpdatedAt: f.now}
				target := CPALatencyRecord{Fingerprint: member.CertificateFingerprint, Address: latencyAddress(peer.HomeIP, peer.HomePort), ProcessID: "p", Sequence: 1, ObservedAt: f.now.Add(-30 * time.Second), SampledAt: f.now.Add(-30 * time.Second), RTT: 10 * time.Millisecond, Valid: true, UpdatedAt: f.now}
				want := RebalanceTarget{IP: alternative.HomeIP, Port: alternative.HomePort}
				switch scenario {
				case "active", "unknown target", "expired target":
					want = RebalanceTarget{IP: peer.HomeIP, Port: peer.HomePort}
					if scenario == "expired target" {
						target.ObservedAt, target.SampledAt = f.now.Add(-rebalanceLatencyTTL-time.Second), f.now.Add(-rebalanceLatencyTTL-time.Second)
					}
				case "expired confirmation":
					confirmation.UpdatedAt = f.now.Add(-rebalanceLatencyTTL - time.Second)
				case "new source lifetime":
					lifetime.ConnectedAt = lifetime.ConnectedAt.Add(time.Second)
					client.ConnectedAt = lifetime.ConnectedAt
				case "new source incarnation":
					lifetime.Home.StartedAt = lifetime.Home.StartedAt.Add(time.Second)
				case "new target incarnation":
					oldKey := rebalanceHomeKey(HomeIncarnationID{IP: peer.HomeIP, Port: peer.HomePort, StartedAt: peer.HomeStartedAt})
					home := f.homes[oldKey]
					delete(f.homes, oldKey)
					home.id.StartedAt = home.id.StartedAt.Add(time.Second)
					f.homes[rebalanceHomeKey(home.id)] = home
				case "not participant":
					a, b := f.members[0], f.members[2]
					a.CertificateFingerprint, b.CertificateFingerprint = "different-a", "different-b"
					confirmation.Signature = latencySwapSignature(a, b)
				case "failure backoff":
					target.Valid = false
					target.ProbeRetryAt = f.now.Add(time.Minute)
				case "lost improvement":
					target.RTT = 95 * time.Millisecond
				}
				if errCreate := repo.db.Create(&client).Error; errCreate != nil {
					t.Fatal(errCreate)
				}
				if errCreate := repo.db.Create(&confirmation).Error; errCreate != nil {
					t.Fatal(errCreate)
				}
				if scenario != "unknown target" {
					if errCreate := repo.db.Create(&target).Error; errCreate != nil {
						t.Fatal(errCreate)
					}
				}
				report := latencyReport{ProcessID: "p", Samples: []latencySample{
					{IP: member.HomeIP, Port: member.HomePort, Sequence: 2, ObservedAt: f.now.Add(-10 * time.Second), AgeMS: 10000, RTTUS: 100000, Valid: true},
					{IP: alternative.HomeIP, Port: alternative.HomePort, Sequence: 3, ObservedAt: f.now.Add(-time.Minute), AgeMS: 60000, RTTUS: 10000, Valid: true},
				}}
				raw, errJSON := json.Marshal(report)
				if errJSON != nil {
					t.Fatal(errJSON)
				}
				var probe *RebalanceTarget
				if errReport := repo.db.Transaction(func(tx *gorm.DB) error {
					if errLock := tx.Clauses(clause.Locking{Strength: "NO KEY UPDATE"}).First(&client, "fingerprint = ?", lifetime.Fingerprint).Error; errLock != nil {
						return errLock
					}
					var errAccept error
					probe, errAccept = acceptLatencyReport(tx, lifetime, string(raw), f.now, f.homes, client)
					return errAccept
				}); errReport != nil {
					t.Fatal(errReport)
				}
				if probe == nil || *probe != want {
					t.Fatalf("probe = %+v, want %+v", probe, want)
				}
			})
		}
	}
}

func TestLatencyConfirmationLookupFailureRollsBackReport(t *testing.T) {
	repo := newCredentialFoundationTestRepository(t)
	f := newLatencySwapSelectionFixture(3, 3)
	member := f.members[0]
	client := f.clients[member.CertificateFingerprint]
	client.LatencyConnectedAt = time.Time{}
	if errCreate := repo.db.Create(&client).Error; errCreate != nil {
		t.Fatal(errCreate)
	}
	if errDrop := repo.db.Migrator().DropTable(&CPALatencyConfirmationRecord{}); errDrop != nil {
		t.Fatal(errDrop)
	}
	report := latencyReport{ProcessID: "p", Samples: []latencySample{{IP: member.HomeIP, Port: member.HomePort, Sequence: 1, ObservedAt: f.now, RTTUS: 100000, Valid: true}}}
	raw, errJSON := json.Marshal(report)
	if errJSON != nil {
		t.Fatal(errJSON)
	}
	lifetime := ConnectionLifetime{Fingerprint: member.CertificateFingerprint, ConnectedAt: member.ConnectedAt, Home: HomeIncarnationID{IP: member.HomeIP, Port: member.HomePort, StartedAt: member.HomeStartedAt}}
	errReport := repo.db.Transaction(func(tx *gorm.DB) error {
		if errLock := tx.Clauses(clause.Locking{Strength: "NO KEY UPDATE"}).First(&client, "fingerprint = ?", lifetime.Fingerprint).Error; errLock != nil {
			return errLock
		}
		_, errAccept := acceptLatencyReport(tx, lifetime, string(raw), f.now, f.homes, client)
		return errAccept
	})
	if errReport == nil {
		t.Fatal("confirmation query failure was silently ignored")
	}
	var count int64
	if errCount := repo.db.Model(&CPALatencyRecord{}).Count(&count).Error; errCount != nil {
		t.Fatal(errCount)
	}
	if errFirst := repo.db.First(&client, "fingerprint = ?", lifetime.Fingerprint).Error; errFirst != nil {
		t.Fatal(errFirst)
	}
	if count != 0 || !client.LatencyConnectedAt.IsZero() {
		t.Fatal("failed report partially committed")
	}
}

func TestLatencyThreeHomeConfirmationSurvivesDelayedProbeReports(t *testing.T) {
	for _, warmHistory := range []bool{false, true} {
		for _, test := range []struct {
			plannerOffset time.Duration
			clockSkew     time.Duration
			reportJitter  time.Duration
		}{
			{plannerOffset: time.Second},
			{plannerOffset: 15 * time.Second},
			{plannerOffset: 29 * time.Second},
			{plannerOffset: 29 * time.Second, clockSkew: time.Second},
			{plannerOffset: 29 * time.Second, clockSkew: 3 * time.Second, reportJitter: 200 * time.Millisecond},
		} {
			t.Run(fmt.Sprintf("warm=%t/planner-offset=%s/skew=%s/jitter=%s", warmHistory, test.plannerOffset, test.clockSkew, test.reportJitter), func(t *testing.T) {
				repo := newCredentialFoundationTestRepository(t)
				f := newLatencySwapSelectionFixture(3, 3)
				history := make(map[string]map[string]latencySample)
				sequences := make(map[string]int64)
				for _, member := range f.members {
					client := f.clients[member.CertificateFingerprint]
					if errCreate := repo.db.Create(&client).Error; errCreate != nil {
						t.Fatal(errCreate)
					}
					history[member.CertificateFingerprint] = make(map[string]latencySample)
					if warmHistory {
						for _, home := range f.homes {
							if home.id.IP == member.HomeIP {
								continue
							}
							sequences[member.CertificateFingerprint]++
							history[member.CertificateFingerprint][latencyAddress(home.id.IP, home.id.Port)] = latencySample{
								IP: home.id.IP, Port: home.id.Port, Sequence: sequences[member.CertificateFingerprint],
								ObservedAt: f.now.Add(-45*time.Second - test.clockSkew), RTTUS: 10000, Valid: true,
							}
						}
					}
				}
				gate := ClusterMasterGateRecord{Term: 1}
				var firstSeenAt time.Time
				for minute := range 15 {
					reportAt := f.now.Add(time.Duration(minute)*time.Minute + time.Duration(minute%3)*test.reportJitter)
					for _, member := range f.members {
						fingerprint := member.CertificateFingerprint
						sequences[fingerprint]++
						history[fingerprint][latencyAddress(member.HomeIP, member.HomePort)] = latencySample{
							IP: member.HomeIP, Port: member.HomePort, Sequence: sequences[fingerprint],
							ObservedAt: reportAt.Add(-10*time.Second - test.clockSkew), RTTUS: 100000, Valid: true,
						}
						report := latencyReport{ProcessID: "cadence-process"}
						for _, sample := range history[fingerprint] {
							// CPA age is measured locally and is unaffected by its wall-clock offset.
							sample.AgeMS = reportAt.Sub(sample.ObservedAt.Add(test.clockSkew)).Milliseconds()
							if sample.AgeMS <= (2 * time.Minute).Milliseconds() {
								report.Samples = append(report.Samples, sample)
							}
						}
						raw, errJSON := json.Marshal(report)
						if errJSON != nil {
							t.Fatal(errJSON)
						}
						var probe *RebalanceTarget
						if errReport := repo.db.Transaction(func(tx *gorm.DB) error {
							var client CPARebalanceClientRecord
							if errLock := tx.Clauses(clause.Locking{Strength: "NO KEY UPDATE"}).First(&client, "fingerprint = ?", fingerprint).Error; errLock != nil {
								return errLock
							}
							lifetime := ConnectionLifetime{Fingerprint: fingerprint, ConnectedAt: member.ConnectedAt, Home: HomeIncarnationID{IP: member.HomeIP, Port: member.HomePort, StartedAt: member.HomeStartedAt}}
							var errAccept error
							probe, errAccept = acceptLatencyReport(tx, lifetime, string(raw), reportAt, f.homes, client)
							return errAccept
						}); errReport != nil {
							t.Fatal(errReport)
						}
						if probe != nil {
							// CPA probes at most one target per minute AFTER uploading its report.
							// A result becomes visible to Home only in the next minute's report.
							sequences[fingerprint]++
							history[fingerprint][latencyAddress(probe.IP, probe.Port)] = latencySample{
								IP: probe.IP, Port: probe.Port, Sequence: sequences[fingerprint],
								ObservedAt: reportAt.Add(100*time.Millisecond - test.clockSkew), RTTUS: 10000, Valid: true,
							}
						}
					}
					// Home plans every 30 seconds, including between report deliveries when old evidence expires.
					for _, offset := range []time.Duration{test.plannerOffset, test.plannerOffset + 30*time.Second} {
						at := reportAt.Add(offset)
						if errPlan := repo.db.Transaction(func(tx *gorm.DB) error {
							samples, errSamples := loadRebalanceLatencies(tx, at)
							if errSamples != nil {
								return errSamples
							}
							return planLatencySwap(tx, gate, at, f.homes, f.members, f.loads, f.clients, samples, 0)
						}); errPlan != nil {
							t.Fatal(errPlan)
						}
						var plans []CPARebalancePlanRecord
						if errFind := repo.db.Find(&plans).Error; errFind != nil {
							t.Fatal(errFind)
						}
						if len(plans) != 0 {
							if len(plans) != 2 || plans[0].SwapGroupID == "" || plans[0].SwapGroupID != plans[1].SwapGroupID ||
								rebalanceSource(plans[0]) != rebalanceTarget(plans[1]) || rebalanceTarget(plans[0]) != rebalanceSource(plans[1]) {
								t.Fatalf("unexpected swap plans: %+v", plans)
							}
							t.Logf("swap confirmed after %s with one-minute delayed probe reports", at.Sub(f.now))
							return
						}
						var confirmation CPALatencyConfirmationRecord
						errFirst := repo.db.First(&confirmation, "id = ?", "active").Error
						if errFirst == gorm.ErrRecordNotFound && firstSeenAt.IsZero() {
							continue
						}
						if errFirst != nil {
							t.Fatalf("confirmation disappeared at %s: %v", at.Sub(f.now), errFirst)
						}
						if !firstSeenAt.IsZero() && !confirmation.FirstSeenAt.Equal(firstSeenAt) {
							t.Fatalf("confirmation restarted at %s: %+v", at.Sub(f.now), confirmation)
						}
						firstSeenAt = confirmation.FirstSeenAt
					}
				}
				t.Fatal("no swap after 15 minutes of stable 100ms -> 10ms improvements to both alternative Homes")
			})
		}
	}
}
