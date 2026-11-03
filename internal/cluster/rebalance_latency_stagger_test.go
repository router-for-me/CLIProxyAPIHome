package cluster

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func TestLatencyStaggeredReportsKeepThreeHomeEvidenceFresh(t *testing.T) {
	for _, delays := range [][3]time.Duration{
		{60 * time.Second, 80 * time.Second, 119 * time.Second},
		{119 * time.Second, 60 * time.Second, 89 * time.Second},
	} {
		t.Run(fmt.Sprint(delays), func(t *testing.T) {
			repo := newCredentialFoundationTestRepository(t)
			f := newLatencySwapSelectionFixture(3, 3)
			history := make(map[string]map[string]latencySample)
			sequence := make(map[string]int64)
			for _, member := range f.members {
				client := f.clients[member.CertificateFingerprint]
				if errCreate := repo.db.Create(&client).Error; errCreate != nil {
					t.Fatal(errCreate)
				}
				history[member.CertificateFingerprint] = make(map[string]latencySample)
			}
			// Process report and planner events chronologically; each CPA retains its
			// own initial offset, a fixed minute cadence, and next-report probe delivery.
			for elapsed := time.Duration(0); elapsed < 15*time.Minute; elapsed += time.Second {
				at := f.now.Add(elapsed)
				for index, member := range f.members {
					if elapsed < delays[index] || (elapsed-delays[index])%time.Minute != 0 {
						continue
					}
					fingerprint := member.CertificateFingerprint
					sequence[fingerprint]++
					history[fingerprint][latencyAddress(member.HomeIP, member.HomePort)] = latencySample{
						IP: member.HomeIP, Port: member.HomePort, Sequence: sequence[fingerprint],
						ObservedAt: at.Add(-10 * time.Second), RTTUS: 100000, Valid: true,
					}
					report := latencyReport{ProcessID: "staggered-process"}
					for _, sample := range history[fingerprint] {
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
						if errLock := tx.Clauses(clause.Locking{Strength: "NO KEY UPDATE"}).First(&client, "fingerprint = ?", fingerprint).Error; errLock != nil {
							return errLock
						}
						lifetime := ConnectionLifetime{Fingerprint: fingerprint, ConnectedAt: member.ConnectedAt, Home: HomeIncarnationID{IP: member.HomeIP, Port: member.HomePort, StartedAt: member.HomeStartedAt}}
						var errAccept error
						probe, errAccept = acceptLatencyReport(tx, lifetime, string(raw), at, f.homes, client)
						return errAccept
					}); errReport != nil {
						t.Fatal(errReport)
					}
					if probe != nil {
						sequence[fingerprint]++
						history[fingerprint][latencyAddress(probe.IP, probe.Port)] = latencySample{
							IP: probe.IP, Port: probe.Port, Sequence: sequence[fingerprint],
							ObservedAt: at.Add(100 * time.Millisecond), RTTUS: 10000, Valid: true,
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
				if errFind := repo.db.Find(&plans).Error; errFind != nil {
					t.Fatal(errFind)
				}
				if len(plans) > 0 {
					if len(plans) != 2 || plans[0].SwapGroupID == "" || plans[0].SwapGroupID != plans[1].SwapGroupID {
						t.Fatalf("unexpected plans: %+v", plans)
					}
					t.Logf("staggered reports produced a swap after %s", elapsed)
					return
				}
			}
			t.Fatal("staggered reports prevented sustained swap confirmation")
		})
	}
}
