package cluster

import (
	"context"
	"testing"
	"time"
)

func TestQuotaSharedPoolBreakdownDoesNotDetermineAvailability(t *testing.T) {
	for _, independent := range []string{"unknown", "healthy", "low", "exhausted", "error"} {
		for _, breakdown := range []string{"unknown", "healthy", "low", "exhausted", "error"} {
			t.Run(independent+"/"+breakdown, func(t *testing.T) {
				windows := []QuotaWindow{
					{Status: independent},
					{Status: breakdown, ScopeID: quotaOptionalString("xai-product-usage")},
				}
				if got := AggregateQuotaWindowStatus(windows); got != independent {
					t.Fatalf("shared usage overrode its pool: got %s, want %s", got, independent)
				}
				records := []QuotaWindowRecord{{Status: independent}, {Status: breakdown, ScopeID: "xai-product-usage"}}
				if got := quotaWindowRecordAggregateStatus(records); got != independent {
					t.Fatalf("stored shared usage overrode its pool: got %s, want %s", got, independent)
				}
				if got := AggregateQuotaWindowStatus(windows[1:]); got != "unknown" {
					t.Fatalf("product detail alone established availability: %s", got)
				}
			})
		}
	}
	for _, windows := range [][]QuotaWindow{
		{{Status: "healthy"}, {Status: "exhausted"}},
		{{Status: "exhausted"}, {Status: "healthy"}},
	} {
		if got := AggregateQuotaWindowStatus(windows); got != "exhausted" {
			t.Fatalf("an independent exhausted limit was ignored: %s", got)
		}
	}
}

func TestQuotaFallbackPlanKeepsLockedLastKnownMetadata(t *testing.T) {
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
			id := "xai-fallback-plan"
			seedQuotaSnapshotAuth(t, repo, id, "xai", "XAI", map[string]any{"type": "xai", "access_token": "synthetic"})
			now := time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)
			planExpiry := now.Add(24 * time.Hour)
			for _, stage := range []struct {
				name                 string
				plan, fallback, want *QuotaPlan
				replace              bool
			}{
				{name: "populate absent", fallback: &QuotaPlan{Name: "SuperGrok"}, want: &QuotaPlan{Name: "SuperGrok"}},
				{name: "replace heuristic", plan: &QuotaPlan{Name: "SuperGrok Plus", Premium: true, ExpiresAt: &planExpiry}, replace: true, want: &QuotaPlan{Name: "SuperGrok Plus", Premium: true, ExpiresAt: &planExpiry}},
				{name: "preserve all metadata", fallback: &QuotaPlan{Name: "SuperGrok Heavy", Premium: true}, want: &QuotaPlan{Name: "SuperGrok Plus", Premium: true, ExpiresAt: &planExpiry}},
				{name: "confirmed free", plan: &QuotaPlan{Name: "Free"}, fallback: &QuotaPlan{Name: "SuperGrok Heavy", Premium: true}, replace: true, want: &QuotaPlan{Name: "Free"}},
				{name: "preserve free", fallback: &QuotaPlan{Name: "SuperGrok Heavy", Premium: true}, want: &QuotaPlan{Name: "Free"}},
				{name: "explicit clear", fallback: &QuotaPlan{Name: "SuperGrok"}, replace: true},
				{name: "populate after clear", fallback: &QuotaPlan{Name: "SuperGrok"}, want: &QuotaPlan{Name: "SuperGrok"}},
			} {
				t.Run(stage.name, func(t *testing.T) {
					now = now.Add(time.Minute)
					claimed, errClaim := repo.ForceClaimEligibleQuotaProbe(ctx, id, "owner", now, time.Minute)
					if errClaim != nil || !claimed {
						t.Fatalf("claim failed: claimed=%v error=%v", claimed, errClaim)
					}
					expiry := now.Add(30 * time.Minute)
					write := QuotaSnapshotWrite{CredentialID: id, QuotaStatus: "healthy", CollectionStatus: "partial", Source: "active_probe", ObservedAt: &now, ExpiresAt: &expiry, NextProbeAt: &expiry, ExpectedProbeOwner: "owner", ClearProbeLease: true, Plan: stage.plan, FallbackPlan: stage.fallback, ReplacePlan: stage.replace}
					updated, errWrite := repo.UpsertQuotaSnapshot(ctx, write)
					if errWrite != nil || !updated {
						t.Fatalf("write failed: updated=%v error=%v", updated, errWrite)
					}
					item, errGet := repo.GetQuotaCredential(ctx, id, now)
					if errGet != nil {
						t.Fatal(errGet)
					}
					if stage.want == nil {
						if item.Plan != nil {
							t.Fatalf("plan was not cleared: %+v", item.Plan)
						}
						return
					}
					if item.Plan == nil || item.Plan.Name != stage.want.Name || item.Plan.Premium != stage.want.Premium || (item.Plan.ExpiresAt == nil) != (stage.want.ExpiresAt == nil) || stage.want.ExpiresAt != nil && !item.Plan.ExpiresAt.Equal(*stage.want.ExpiresAt) {
						t.Fatalf("incorrect plan metadata: got %+v, want %+v", item.Plan, stage.want)
					}
					write.ExpectedProbeOwner = "lost-owner"
					write.Plan, write.FallbackPlan = &QuotaPlan{Name: "incorrect"}, &QuotaPlan{Name: "incorrect"}
					if changed, errFenced := repo.UpsertQuotaSnapshot(ctx, write); errFenced != nil || changed {
						t.Fatalf("lost probe owner modified a plan: changed=%v error=%v", changed, errFenced)
					}
				})
			}
		})
	}
}

func TestQuotaFallbackPlanRequiresLease(t *testing.T) {
	input := QuotaSnapshotWrite{CredentialID: "xai", QuotaStatus: "unknown", CollectionStatus: "partial", Source: "active_probe", FallbackPlan: &QuotaPlan{Name: "SuperGrok"}}
	if errValidate := validateQuotaSnapshotWrite(&input); errValidate == nil {
		t.Fatal("a fallback plan could bypass the active snapshot lock")
	}
}

func TestQuotaWindowPreservationRejectsElapsedReset(t *testing.T) {
	base := time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)
	expiry := base.Add(time.Hour)
	for _, delta := range []time.Duration{-time.Second, 0, time.Second} {
		reset := base.Add(delta)
		window := QuotaWindowRecord{ResetAt: &reset, ExpiresAt: &expiry}
		if got := quotaWindowRecordIsCurrent(window, QuotaSnapshotRecord{}, &base); got != (delta > 0) {
			t.Fatalf("incorrect merge eligibility at reset %v: %v", delta, got)
		}
	}
}
