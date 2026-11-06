package cluster

import (
	"context"
	"testing"
	"time"
)

func TestQuotaWindowPreservationIsAtomicAndKeepsRetryFloor(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			var repo *Repository
			if backend == "postgres" {
				repo = newPostgresQuiescenceRepository(t)
			} else {
				var closeRepo func()
				repo, closeRepo = newBillingTestRepository(t, ctx)
				defer closeRepo()
			}
			id := "xai-preservation"
			seedQuotaSnapshotAuth(t, repo, id, "xai", "XAI", map[string]any{"type": "xai", "access_token": "synthetic"})
			base := time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)
			expiry := base.Add(30 * time.Minute)
			monthExpiry := base.Add(5 * time.Minute)
			windows := make([]QuotaWindow, 0, 4)
			for index, name := range []string{"xai-monthly-spend", "xai-on-demand", "xai-weekly-usage", "xai-product-grokbuild"} {
				remaining := float64(50)
				deadline := expiry
				if index == 0 {
					remaining = 0
					deadline = monthExpiry
				}
				windows = append(windows, QuotaWindow{ID: name, Scope: "account", Mode: "fixed", Status: "unknown", Unit: "percentage", Remaining: &remaining, Limit: float64Ptr(100), PeriodUnit: "week", PeriodValue: float64Ptr(1), ObservedAt: base, Source: "active_probe", ExpiresAt: &deadline, Priority: index})
			}
			_, errSeed := repo.UpsertQuotaSnapshot(ctx, QuotaSnapshotWrite{CredentialID: id, QuotaStatus: "exhausted", CollectionStatus: "success", Source: "active_probe", ObservedAt: &base, ExpiresAt: &expiry, NextProbeAt: &expiry, ReplaceWindows: true, Windows: windows, ParserVersion: QuotaSnapshotVersion("xai"), CollectorVersion: QuotaSnapshotVersion("xai")})
			if errSeed != nil {
				t.Fatal(errSeed)
			}
			db, errDB := repo.database()
			if errDB != nil {
				t.Fatal(errDB)
			}
			// Legacy rows inherit their original snapshot deadline exactly once.
			if errLegacy := db.Model(&QuotaWindowRecord{}).Where("credential_id = ? AND window_id = ?", id, "xai-on-demand").Update("expires_at", nil).Error; errLegacy != nil {
				t.Fatal(errLegacy)
			}
			now := base.Add(time.Minute)
			claimed, errClaim := repo.ForceClaimEligibleQuotaProbe(ctx, id, "owner", now, time.Minute)
			if errClaim != nil || !claimed {
				t.Fatalf("claim failed: %v", errClaim)
			}
			newExpiry := now.Add(30 * time.Minute)
			retryFloor := now.Add(10 * time.Minute)
			incoming := windows[2]
			incoming.ObservedAt = now
			incoming.ExpiresAt = &newExpiry
			incoming.Remaining = float64Ptr(99)
			incoming.Used = nil
			incoming.UsedRatio = nil
			incoming.RemainingRatio = nil
			incoming.Status = "unknown"
			unknown := windows[0]
			unknown.ObservedAt = now
			unknown.ExpiresAt = &newExpiry
			unknown.Remaining = nil
			unknown.Used = nil
			unknown.UsedRatio = nil
			unknown.RemainingRatio = nil
			unknown.Status = "unknown"
			write := QuotaSnapshotWrite{CredentialID: id, QuotaStatus: "healthy", CollectionStatus: "partial", Source: "active_probe", ObservedAt: &now, ExpiresAt: &newExpiry, LastAttemptAt: &now, NextProbeAt: &newExpiry, NextProbeNotBefore: &retryFloor, ExpectedProbeOwner: "other-owner", ClearProbeLease: true, ReplaceWindows: true, PreserveWindowPrefixes: []string{"xai-monthly-", "xai-on-demand"}, Windows: []QuotaWindow{incoming, unknown}, ParserVersion: QuotaSnapshotVersion("xai"), CollectorVersion: QuotaSnapshotVersion("xai")}
			updated, errWrongOwner := repo.UpsertQuotaSnapshot(ctx, write)
			if errWrongOwner != nil || updated {
				t.Fatalf("wrong lease owner modified quota: updated=%v error=%v", updated, errWrongOwner)
			}
			untouched, errUntouched := repo.GetQuotaCredential(ctx, id, now)
			if errUntouched != nil || len(untouched.Windows) != 4 || untouched.ObservedAt == nil || !untouched.ObservedAt.Equal(base) {
				t.Fatalf("fenced replacement mutated quota: item=%+v error=%v", untouched, errUntouched)
			}
			write.ExpectedProbeOwner = "owner"
			updated, errWrite := repo.UpsertQuotaSnapshot(ctx, write)
			if errWrite != nil || !updated {
				t.Fatalf("replacement failed: updated=%v error=%v", updated, errWrite)
			}
			item, errGet := repo.GetQuotaCredential(ctx, id, now)
			if errGet != nil || len(item.Windows) != 3 || item.QuotaStatus != "exhausted" || item.ExpiresAt == nil || !item.ExpiresAt.Equal(monthExpiry) || item.NextProbeAt == nil || !item.NextProbeAt.Equal(retryFloor) {
				t.Fatalf("merged quota lost risk or retry bounds: item=%+v error=%v", item, errGet)
			}
			for _, window := range item.Windows {
				switch window.ID {
				case "xai-monthly-spend":
					if window.Remaining == nil || *window.Remaining != 0 || !window.ObservedAt.Equal(base) || window.ExpiresAt == nil || !window.ExpiresAt.Equal(monthExpiry) {
						t.Fatal("unknown monthly amount overwrote a valid observation")
					}
				case "xai-on-demand":
					if !window.ObservedAt.Equal(base) || window.ExpiresAt == nil || !window.ExpiresAt.Equal(expiry) {
						t.Fatal("legacy window gained a new freshness deadline")
					}
				case "xai-weekly-usage":
					if window.Remaining == nil || *window.Remaining != 99 || !window.ObservedAt.Equal(now) {
						t.Fatal("successful weekly component was not updated")
					}
				default:
					t.Fatalf("successful component retained a removed product: %s", window.ID)
				}
			}
			now = base.Add(6 * time.Minute)
			claimed, errClaim = repo.ForceClaimEligibleQuotaProbe(ctx, id, "owner", now, time.Minute)
			if errClaim != nil || !claimed {
				t.Fatal("recovery claim failed")
			}
			newExpiry = now.Add(30 * time.Minute)
			incoming.ObservedAt = now
			write.ObservedAt = &now
			write.Windows = []QuotaWindow{incoming}
			write.NextProbeNotBefore = nil
			updated, errWrite = repo.UpsertQuotaSnapshot(ctx, write)
			if errWrite != nil || !updated {
				t.Fatalf("recovery failed: updated=%v error=%v", updated, errWrite)
			}
			recovered, errRecovered := repo.GetQuotaCredential(ctx, id, now)
			if errRecovered != nil || len(recovered.Windows) != 2 || recovered.QuotaStatus != "healthy" || recovered.ExpiresAt == nil || !recovered.ExpiresAt.Equal(expiry) || recovered.NextProbeAt == nil || !recovered.NextProbeAt.Equal(expiry) {
				t.Fatalf("expired preserved amount survived or retained data was refreshed: item=%+v error=%v", recovered, errRecovered)
			}
		})
	}
}

func TestQuotaWindowPreservationRejectsEmptyPrefix(t *testing.T) {
	now := time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)
	input := QuotaSnapshotWrite{CredentialID: "test", QuotaStatus: "unknown", CollectionStatus: "partial", Source: "active_probe", ObservedAt: &now, ReplaceWindows: true, PreserveWindowPrefixes: []string{""}}
	if errValidate := validateQuotaSnapshotWrite(&input); errValidate == nil {
		t.Fatal("an empty preservation prefix would match every window")
	}
}
