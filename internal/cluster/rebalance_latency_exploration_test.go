package cluster

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func TestLatencyProbeExplorationSchedule(t *testing.T) {
	for phase := range 4 {
		for _, scenario := range []string{"unknown", "expired", "failed", "failure backoff", "pending confirmation", "recent promising"} {
			t.Run(fmt.Sprintf("phase=%d/%s", phase, scenario), func(t *testing.T) {
				repo := newCredentialFoundationTestRepository(t)
				f := newLatencySwapSelectionFixture(3, 3)
				at := f.now.Add(time.Duration(phase) * time.Minute)
				member, known, alternative := f.members[0], f.members[1], f.members[2]
				client := f.clients[member.CertificateFingerprint]
				if errCreate := repo.db.Create(&client).Error; errCreate != nil {
					t.Fatal(errCreate)
				}
				promising := CPALatencyRecord{Fingerprint: member.CertificateFingerprint, Address: latencyAddress(known.HomeIP, known.HomePort), ProcessID: "p", Sequence: 1, ObservedAt: at.Add(-time.Minute), SampledAt: at.Add(-time.Minute), RTT: 10 * time.Millisecond, Valid: true, UpdatedAt: at}
				if errCreate := repo.db.Create(&promising).Error; errCreate != nil {
					t.Fatal(errCreate)
				}
				other := CPALatencyRecord{Fingerprint: member.CertificateFingerprint, Address: latencyAddress(alternative.HomeIP, alternative.HomePort), ProcessID: "p", Sequence: 2, ObservedAt: at.Add(-3 * time.Minute), SampledAt: at.Add(-3 * time.Minute), RTT: 50 * time.Millisecond, Valid: true, UpdatedAt: at}
				want := RebalanceTarget{IP: known.HomeIP, Port: known.HomePort}
				switch scenario {
				case "unknown", "expired", "failed":
					if phase == 0 {
						want = RebalanceTarget{IP: alternative.HomeIP, Port: alternative.HomePort}
					}
					if scenario == "failed" {
						other.Valid = false
						other.ProbeRetryAt = at.Add(-time.Second)
					}
				case "failure backoff":
					other.Valid = false
					other.ProbeRetryAt = at.Add(time.Minute)
				case "pending confirmation":
					confirmation := CPALatencyConfirmationRecord{ID: "active", Signature: latencySwapSignature(member, known), UpdatedAt: at}
					if errCreate := repo.db.Create(&confirmation).Error; errCreate != nil {
						t.Fatal(errCreate)
					}
				case "recent promising":
					// Maintain the newly explored improvement, even when the older target
					// has a lower RTT but might lack a reciprocal partner.
					other.ObservedAt, other.SampledAt = at.Add(-45*time.Second), at.Add(-45*time.Second)
					if phase != 0 {
						want = RebalanceTarget{IP: alternative.HomeIP, Port: alternative.HomePort}
					}
				}
				if scenario != "unknown" && scenario != "pending confirmation" {
					if errCreate := repo.db.Create(&other).Error; errCreate != nil {
						t.Fatal(errCreate)
					}
				}
				raw, errJSON := json.Marshal(latencyReport{ProcessID: "p", Samples: []latencySample{{IP: member.HomeIP, Port: member.HomePort, Sequence: 3, ObservedAt: at.Add(-10 * time.Second), AgeMS: 10000, RTTUS: 100000, Valid: true}}})
				if errJSON != nil {
					t.Fatal(errJSON)
				}
				lifetime := ConnectionLifetime{Fingerprint: member.CertificateFingerprint, ConnectedAt: member.ConnectedAt, Home: HomeIncarnationID{IP: member.HomeIP, Port: member.HomePort, StartedAt: member.HomeStartedAt}}
				var probe *RebalanceTarget
				if errReport := repo.db.Transaction(func(tx *gorm.DB) error {
					if errLock := tx.Clauses(clause.Locking{Strength: "NO KEY UPDATE"}).First(&client, "fingerprint = ?", lifetime.Fingerprint).Error; errLock != nil {
						return errLock
					}
					var errAccept error
					probe, errAccept = acceptLatencyReport(tx, lifetime, string(raw), at, f.homes, client)
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

func TestLatencyProbeExploresReciprocalTarget(t *testing.T) {
	for phase := range 4 {
		for _, warmHistory := range []bool{false, true} {
			for _, reportOffsets := range [][3]time.Duration{
				{0, 0, 0},
				{0, 20 * time.Second, 59 * time.Second},
				{59 * time.Second, 0, 29 * time.Second},
			} {
				t.Run(fmt.Sprintf("phase=%d/warm=%t/offsets=%v", phase, warmHistory, reportOffsets), func(t *testing.T) {
					repo := newCredentialFoundationTestRepository(t)
					f := newLatencySwapSelectionFixture(3, 3)
					f.now = f.now.Add(time.Duration(phase) * time.Minute)
					// H1 -> H2 is promising but cannot be reciprocated. Only H1/H3
					// can swap, with both CPAs improving from 100ms to 10ms.
					rtts := [3][3]int64{{100000, 50000, 10000}, {100000, 100000, 100000}, {10000, 100000, 100000}}
					history := make([]map[string]latencySample, len(f.members))
					sequences := make([]int64, len(f.members))
					probeCounts := [3][3]int{}
					for index, member := range f.members {
						client := f.clients[member.CertificateFingerprint]
						if errCreate := repo.db.Create(&client).Error; errCreate != nil {
							t.Fatal(errCreate)
						}
						history[index] = make(map[string]latencySample)
					}
					if warmHistory {
						peer := f.members[1]
						sequences[0]++
						history[0][latencyAddress(peer.HomeIP, peer.HomePort)] = latencySample{
							IP: peer.HomeIP, Port: peer.HomePort, Sequence: sequences[0],
							ObservedAt: f.now.Add(-45 * time.Second), RTTUS: rtts[0][1], Valid: true,
						}
					}
					// Advance a simulated clock, retaining minute reports and 30-second
					// planning across every exploration phase and staggered report order.
					for elapsed := time.Duration(0); elapsed < 15*time.Minute; elapsed += time.Second {
						at := f.now.Add(elapsed)
						for index, member := range f.members {
							if elapsed < reportOffsets[index] || (elapsed-reportOffsets[index])%time.Minute != 0 {
								continue
							}
							sequences[index]++
							history[index][latencyAddress(member.HomeIP, member.HomePort)] = latencySample{
								IP: member.HomeIP, Port: member.HomePort, Sequence: sequences[index],
								ObservedAt: at.Add(-10 * time.Second), RTTUS: rtts[index][index], Valid: true,
							}
							report := latencyReport{ProcessID: "exploration-process"}
							for _, sample := range history[index] {
								sample.AgeMS = at.Sub(sample.ObservedAt).Milliseconds()
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
								if errLock := tx.Clauses(clause.Locking{Strength: "NO KEY UPDATE"}).First(&client, "fingerprint = ?", member.CertificateFingerprint).Error; errLock != nil {
									return errLock
								}
								lifetime := ConnectionLifetime{Fingerprint: member.CertificateFingerprint, ConnectedAt: member.ConnectedAt, Home: HomeIncarnationID{IP: member.HomeIP, Port: member.HomePort, StartedAt: member.HomeStartedAt}}
								var errAccept error
								probe, errAccept = acceptLatencyReport(tx, lifetime, string(raw), at, f.homes, client)
								return errAccept
							}); errReport != nil {
								t.Fatal(errReport)
							}
							if probe != nil {
								targetIndex := -1
								for candidate, peer := range f.members {
									if peer.HomeIP == probe.IP && peer.HomePort == probe.Port {
										targetIndex = candidate
										break
									}
								}
								if targetIndex < 0 || targetIndex == index {
									t.Fatalf("unexpected probe: %+v", probe)
								}
								probeCounts[index][targetIndex]++
								sequences[index]++
								// Probes happen after reporting and arrive in the next minute's report.
								history[index][latencyAddress(probe.IP, probe.Port)] = latencySample{
									IP: probe.IP, Port: probe.Port, Sequence: sequences[index],
									ObservedAt: at.Add(100 * time.Millisecond), RTTUS: rtts[index][targetIndex], Valid: true,
								}
							}
						}
						if elapsed%(30*time.Second) != 15*time.Second {
							continue
						}
						if errPlan := repo.db.Transaction(func(tx *gorm.DB) error {
							samples, errSamples := loadRebalanceLatencies(tx, at)
							if errSamples != nil {
								return errSamples
							}
							return planLatencySwap(tx, ClusterMasterGateRecord{Term: 1}, at, f.homes, f.members, f.loads, f.clients, samples, 0)
						}); errPlan != nil {
							t.Fatal(errPlan)
						}
						var plans []CPARebalancePlanRecord
						if errFind := repo.db.Order("fingerprint").Find(&plans).Error; errFind != nil {
							t.Fatal(errFind)
						}
						if len(plans) > 0 {
							if len(plans) != 2 || plans[0].SwapGroupID == "" || plans[0].SwapGroupID != plans[1].SwapGroupID ||
								plans[0].Fingerprint != f.members[0].CertificateFingerprint || plans[1].Fingerprint != f.members[2].CertificateFingerprint ||
								rebalanceSource(plans[0]) != rebalanceTarget(plans[1]) || rebalanceTarget(plans[0]) != rebalanceSource(plans[1]) {
								t.Fatalf("unexpected swap plans: %+v", plans)
							}
							t.Logf("H1/H3 swap confirmed after %s; probe counts=%v", elapsed, probeCounts)
							return
						}
					}
					t.Fatalf("reciprocal 100ms->10ms H1/H3 swap never found in 15 minutes; probe counts=%v", probeCounts)
				})
			}
		}
	}
}
