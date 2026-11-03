package cluster

import (
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"gorm.io/gorm"
)

type latencySwapSelectionFixture struct {
	now     time.Time
	homes   map[string]rebalanceHome
	members []CPANodeMembershipRecord
	clients map[string]CPARebalanceClientRecord
	samples map[string]CPALatencyRecord
	loads   map[string]int
}

func newLatencySwapSelectionFixture(homeCount, memberCount int) latencySwapSelectionFixture {
	f := latencySwapSelectionFixture{
		now:     time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC),
		homes:   make(map[string]rebalanceHome, homeCount),
		members: make([]CPANodeMembershipRecord, 0, memberCount),
		clients: make(map[string]CPARebalanceClientRecord, memberCount),
		loads:   make(map[string]int, homeCount),
	}
	homeIDs := make([]HomeIncarnationID, homeCount)
	for index := range homeIDs {
		home := HomeIncarnationID{IP: fmt.Sprintf("10.0.0.%d", index+1), Port: 8327, StartedAt: f.now.Add(-time.Hour)}
		homeIDs[index] = home
		f.homes[rebalanceHomeKey(home)] = rebalanceHome{id: home, capable: true, latencyCapable: true}
	}
	for index := range memberCount {
		home := homeIDs[index%homeCount]
		member := CPANodeMembershipRecord{CertificateFingerprint: fmt.Sprintf("fp-%08d", index), HomeIP: home.IP, HomePort: home.Port, HomeStartedAt: home.StartedAt, ConnectedAt: f.now.Add(-10 * time.Minute), ProtocolVersion: 1}
		f.members = append(f.members, member)
		f.clients[member.CertificateFingerprint] = CPARebalanceClientRecord{Fingerprint: member.CertificateFingerprint, ConnectedAt: member.ConnectedAt, LatencyConnectedAt: member.ConnectedAt}
		f.loads[rebalanceHomeKey(home)]++
	}
	f.samples = latencyTestSamples(f.members, f.homes, f.now, 100*time.Millisecond, 10*time.Millisecond)
	return f
}

// exhaustiveLatencySwap is the pre-optimization oracle: enumerate CPA pairs in fingerprint order.
func exhaustiveLatencySwap(f latencySwapSelectionFixture) (CPANodeMembershipRecord, CPANodeMembershipRecord, time.Duration) {
	ordered := slices.Clone(f.members)
	slices.SortFunc(ordered, func(a, b CPANodeMembershipRecord) int {
		if a.CertificateFingerprint < b.CertificateFingerprint {
			return -1
		}
		if a.CertificateFingerprint > b.CertificateFingerprint {
			return 1
		}
		return 0
	})
	var a, b CPANodeMembershipRecord
	var best time.Duration
	for index, left := range ordered {
		if !latencyClientEligible(f.clients[left.CertificateFingerprint], left, f.now) {
			continue
		}
		for _, right := range ordered[index+1:] {
			if !latencyClientEligible(f.clients[right.CertificateFingerprint], right, f.now) || (left.HomeIP == right.HomeIP && left.HomePort == right.HomePort) {
				continue
			}
			leftHome := HomeIncarnationID{IP: left.HomeIP, Port: left.HomePort, StartedAt: left.HomeStartedAt}
			rightHome := HomeIncarnationID{IP: right.HomeIP, Port: right.HomePort, StartedAt: right.HomeStartedAt}
			_, leftOK := latencyImproves(f.samples, left, rightHome)
			_, rightOK := latencyImproves(f.samples, right, leftHome)
			if !leftOK || !rightOK {
				continue
			}
			leftGain, _, _ := latencyBenefit(f.samples, left, rightHome)
			rightGain, _, _ := latencyBenefit(f.samples, right, leftHome)
			if leftGain+rightGain > best {
				a, b, best = left, right, leftGain+rightGain
			}
		}
	}
	return a, b, best
}

func TestLatencySwapDirectionalSelectionMatchesExhaustive(t *testing.T) {
	for seed := range 120 {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			random := rand.New(rand.NewPCG(uint64(seed), 1))
			f := newLatencySwapSelectionFixture(2+random.IntN(5), 2+random.IntN(40))
			homeKeys := make([]string, 0, len(f.homes))
			for key := range f.homes {
				homeKeys = append(homeKeys, key)
			}
			slices.Sort(homeKeys)
			for index := range f.members {
				member := &f.members[index]
				client := f.clients[member.CertificateFingerprint]
				switch random.IntN(8) {
				case 0:
					client.NextEligibleAt = f.now.Add(time.Minute)
				case 1:
					client.OptimizationEligibleAt = f.now.Add(time.Minute)
				case 2:
					client.LatencyConnectedAt = member.ConnectedAt.Add(-time.Second)
				case 3:
					member.ProtocolVersion = 0
				}
				f.clients[member.CertificateFingerprint] = client
				for _, homeKey := range homeKeys {
					home := f.homes[homeKey].id
					key := latencyKey(member.CertificateFingerprint, home.IP, home.Port)
					if random.IntN(5) == 0 {
						delete(f.samples, key)
						continue
					}
					sample := f.samples[key]
					// Coarse RTTs deliberately create equal-gain directions and pairs.
					sample.RTT = time.Duration(1+random.IntN(20)) * 10 * time.Millisecond
					f.samples[key] = sample
				}
			}
			wantA, wantB, wantGain := exhaustiveLatencySwap(f)
			for range 4 {
				random.Shuffle(len(f.members), func(i, j int) { f.members[i], f.members[j] = f.members[j], f.members[i] })
				a, b := selectLatencySwap(f.now, f.homes, f.members, f.clients, f.samples, "")
				if latencySwapSignature(a, b) != latencySwapSignature(wantA, wantB) {
					t.Fatalf("directional pair = %s/%s, exhaustive = %s/%s (gain %v)", a.CertificateFingerprint, b.CertificateFingerprint, wantA.CertificateFingerprint, wantB.CertificateFingerprint, wantGain)
				}
			}
		})
	}
}

func TestLatencySwapDirectionalSelectionTiesAndThresholds(t *testing.T) {
	for _, test := range []struct {
		name            string
		current, target time.Duration
		wantPair        bool
	}{
		{"absolute boundary", 60 * time.Millisecond, 40 * time.Millisecond, true},
		{"relative boundary", 100 * time.Millisecond, 70 * time.Millisecond, true},
		{"below absolute", 60 * time.Millisecond, 41 * time.Millisecond, false},
		{"below relative", 100 * time.Millisecond, 71 * time.Millisecond, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newLatencySwapSelectionFixture(3, 12)
			f.samples = latencyTestSamples(f.members, f.homes, f.now, test.current, test.target)
			wantA, wantB := f.members[0], f.members[1]
			if !test.wantPair {
				wantA, wantB = CPANodeMembershipRecord{}, CPANodeMembershipRecord{}
			}
			for range 10 {
				slices.Reverse(f.members)
				a, b := selectLatencySwap(f.now, f.homes, f.members, f.clients, f.samples, "")
				if latencySwapSignature(a, b) != latencySwapSignature(wantA, wantB) {
					t.Fatalf("unexpected tied pair: %s/%s", a.CertificateFingerprint, b.CertificateFingerprint)
				}
			}
		})
	}
}

func TestLatencySwapDirectionalSelectionDoesNotTruncateCandidates(t *testing.T) {
	f := newLatencySwapSelectionFixture(2, 4000)
	wantA, wantB := f.members[len(f.members)-2], f.members[len(f.members)-1]
	for _, member := range []CPANodeMembershipRecord{wantA, wantB} {
		key := latencyKey(member.CertificateFingerprint, member.HomeIP, member.HomePort)
		sample := f.samples[key]
		sample.RTT = 200 * time.Millisecond
		f.samples[key] = sample
	}
	a, b := selectLatencySwap(f.now, f.homes, f.members, f.clients, f.samples, "")
	if latencySwapSignature(a, b) != latencySwapSignature(wantA, wantB) {
		t.Fatalf("later eligible CPAs were excluded: %s/%s", a.CertificateFingerprint, b.CertificateFingerprint)
	}
}

func TestLatencySwapRetainedPairRevalidatesBothDirections(t *testing.T) {
	for _, scenario := range []string{"valid", "missing member", "new lifetime", "new home incarnation", "count cooldown", "optimization cooldown", "stale capability", "missing source sample", "missing target sample", "lost improvement"} {
		for _, side := range []int{4, 5} {
			t.Run(fmt.Sprintf("%s/side=%d", scenario, side), func(t *testing.T) {
				f := newLatencySwapSelectionFixture(2, 8)
				retainedA, retainedB := f.members[4], f.members[5]
				signature := latencySwapSignature(retainedA, retainedB)
				for _, member := range f.members[:2] {
					key := latencyKey(member.CertificateFingerprint, member.HomeIP, member.HomePort)
					sample := f.samples[key]
					sample.RTT = 200 * time.Millisecond
					f.samples[key] = sample
				}
				member := &f.members[side]
				client := f.clients[member.CertificateFingerprint]
				other := f.members[side^1]
				targetKey := latencyKey(member.CertificateFingerprint, other.HomeIP, other.HomePort)
				switch scenario {
				case "missing member":
					f.members = slices.Delete(f.members, side, side+1)
				case "new lifetime":
					member.ConnectedAt = member.ConnectedAt.Add(time.Second)
					client.ConnectedAt, client.LatencyConnectedAt = member.ConnectedAt, member.ConnectedAt
				case "new home incarnation":
					member.HomeStartedAt = member.HomeStartedAt.Add(time.Second)
				case "count cooldown":
					client.NextEligibleAt = f.now.Add(time.Minute)
				case "optimization cooldown":
					client.OptimizationEligibleAt = f.now.Add(time.Minute)
				case "stale capability":
					client.LatencyConnectedAt = member.ConnectedAt.Add(-time.Second)
				case "missing source sample":
					delete(f.samples, latencyKey(member.CertificateFingerprint, member.HomeIP, member.HomePort))
				case "missing target sample":
					delete(f.samples, targetKey)
				case "lost improvement":
					sample := f.samples[targetKey]
					sample.RTT = 90 * time.Millisecond
					f.samples[targetKey] = sample
				}
				f.clients[client.Fingerprint] = client
				slices.Reverse(f.members)
				a, b := selectLatencySwap(f.now, f.homes, f.members, f.clients, f.samples, signature)
				wantA, wantB, _ := exhaustiveLatencySwap(f)
				if scenario == "valid" {
					wantA, wantB = retainedA, retainedB
				}
				if latencySwapSignature(a, b) != latencySwapSignature(wantA, wantB) {
					t.Fatalf("retained pair validation = %s/%s, want %s/%s", a.CertificateFingerprint, b.CertificateFingerprint, wantA.CertificateFingerprint, wantB.CertificateFingerprint)
				}
			})
		}
	}
}

func TestLatencySwapConfirmationNotDisplacedByBetterCandidates(t *testing.T) {
	f := newLatencySwapFixture(t)
	gate, now, homes, members, loads, clients := latencyTestState(t, f)
	firstSeen := now.Add(-rebalanceLatencyTTL)
	wantA, wantB := members[0], members[2]
	for round := range 4 {
		at := firstSeen.Add(time.Duration(round) * time.Minute)
		samples := latencyTestSamples(members, homes, at, 100*time.Millisecond, 10*time.Millisecond)
		if round > 0 {
			for _, member := range []CPANodeMembershipRecord{members[1], members[3]} {
				key := latencyKey(member.CertificateFingerprint, member.HomeIP, member.HomePort)
				sample := samples[key]
				sample.RTT = 200 * time.Millisecond
				samples[key] = sample
			}
		}
		if errPlan := f.repo.db.Transaction(func(tx *gorm.DB) error {
			return planLatencySwap(tx, gate, at, homes, members, loads, clients, samples, 0)
		}); errPlan != nil {
			t.Fatal(errPlan)
		}
		if round < 3 {
			var confirmation CPALatencyConfirmationRecord
			if errFirst := f.repo.db.First(&confirmation, "id = ?", "active").Error; errFirst != nil {
				t.Fatal(errFirst)
			}
			if confirmation.Signature != latencySwapSignature(wantA, wantB) || !confirmation.FirstSeenAt.Equal(firstSeen) || confirmation.EvidenceCount != round+1 {
				t.Fatalf("better candidate reset confirmation: %+v", confirmation)
			}
		}
	}
	plans := f.plans(t)
	if len(plans) != 2 || plans[0].Fingerprint != wantA.CertificateFingerprint || plans[1].Fingerprint != wantB.CertificateFingerprint {
		t.Fatalf("confirmed pair was displaced: %+v", plans)
	}
}

func TestLatencySwapExpiredConfirmationStartsNewBestPair(t *testing.T) {
	f := newLatencySwapFixture(t)
	gate, now, homes, members, loads, clients := latencyTestState(t, f)
	confirmation := CPALatencyConfirmationRecord{ID: "active", Signature: latencySwapSignature(members[1], members[3]), FirstSeenAt: now.Add(-10 * time.Minute), UpdatedAt: now.Add(-rebalanceLatencyTTL - time.Second), EvidenceCount: 10}
	if errCreate := f.repo.db.Create(&confirmation).Error; errCreate != nil {
		t.Fatal(errCreate)
	}
	samples := latencyTestSamples(members, homes, now, 100*time.Millisecond, 10*time.Millisecond)
	if errPlan := f.repo.db.Transaction(func(tx *gorm.DB) error {
		return planLatencySwap(tx, gate, now, homes, members, loads, clients, samples, 0)
	}); errPlan != nil {
		t.Fatal(errPlan)
	}
	if errFirst := f.repo.db.First(&confirmation, "id = ?", "active").Error; errFirst != nil {
		t.Fatal(errFirst)
	}
	if confirmation.Signature != latencySwapSignature(members[0], members[2]) || !confirmation.FirstSeenAt.Equal(now) || confirmation.EvidenceCount != 1 || len(f.plans(t)) != 0 {
		t.Fatalf("expired confirmation reused stale evidence or pinned a candidate: %+v", confirmation)
	}
}

func BenchmarkLatencySwapSelection(b *testing.B) {
	for _, homeCount := range []int{2, 8} {
		for _, memberCount := range []int{1000, 4000, 16000} {
			for _, retained := range []bool{false, true} {
				b.Run(fmt.Sprintf("homes=%d/cpas=%d/retained=%t", homeCount, memberCount, retained), func(b *testing.B) {
					f := newLatencySwapSelectionFixture(homeCount, memberCount)
					signature := ""
					if retained {
						signature = latencySwapSignature(f.members[0], f.members[1])
					}
					b.ReportAllocs()
					for b.Loop() {
						a, c := selectLatencySwap(f.now, f.homes, f.members, f.clients, f.samples, signature)
						if a.CertificateFingerprint != f.members[0].CertificateFingerprint || c.CertificateFingerprint != f.members[1].CertificateFingerprint {
							b.Fatal("unexpected candidate pair")
						}
					}
				})
			}
		}
	}
}

// BenchmarkPlanLatencySwapUnderGate measures the real planner transaction after
// inputs are loaded; it is not an end-to-end reconciliation or PostgreSQL benchmark.
func BenchmarkPlanLatencySwapUnderGate(b *testing.B) {
	for _, memberCount := range []int{1000, 4000, 16000} {
		for _, retained := range []bool{false, true} {
			b.Run(fmt.Sprintf("cpas=%d/retained=%t", memberCount, retained), func(b *testing.B) {
				f := newLatencySwapSelectionFixture(2, memberCount)
				db, errOpen := OpenSQLite(b.Context(), filepath.Join(b.TempDir(), "balance.db"))
				if errOpen != nil {
					b.Fatal(errOpen)
				}
				sqlDB, errDB := db.DB()
				if errDB != nil {
					b.Fatal(errDB)
				}
				b.Cleanup(func() {
					if errClose := sqlDB.Close(); errClose != nil {
						b.Error(errClose)
					}
				})
				if errMigrate := db.AutoMigrate(&ClusterMasterGateRecord{}, &ClusterRebalanceGateRecord{}, &CPALatencyConfirmationRecord{}); errMigrate != nil {
					b.Fatal(errMigrate)
				}
				if errCreate := db.Create(&ClusterMasterGateRecord{ID: 1}).Error; errCreate != nil {
					b.Fatal(errCreate)
				}
				if retained {
					confirmation := CPALatencyConfirmationRecord{ID: "active", Signature: latencySwapSignature(f.members[0], f.members[1]), FirstSeenAt: f.now, UpdatedAt: f.now}
					if errCreate := db.Create(&confirmation).Error; errCreate != nil {
						b.Fatal(errCreate)
					}
				}
				var held time.Duration
				b.ReportAllocs()
				for b.Loop() {
					if errPlan := db.Transaction(func(tx *gorm.DB) error {
						gate, _, errGate := lockRebalanceGate(tx)
						if errGate != nil {
							return errGate
						}
						started := time.Now()
						errPlan := planLatencySwap(tx, gate, f.now, f.homes, f.members, f.loads, f.clients, f.samples, 0)
						held += time.Since(started)
						if errPlan != nil {
							return errPlan
						}
						if !retained {
							return tx.Where("id = ?", "active").Delete(&CPALatencyConfirmationRecord{}).Error
						}
						return nil
					}); errPlan != nil {
						b.Fatal(errPlan)
					}
				}
				b.ReportMetric(float64(held.Nanoseconds())/float64(b.N), "planning-ns/op")
			})
		}
	}
}
