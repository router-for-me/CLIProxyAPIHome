package cluster

import (
	"context"
	"reflect"
	"testing"
	"time"
)

func TestQuotaPlanExpirySurvivesPassiveObservationAndProbeFailure(t *testing.T) {
	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	seedQuotaSnapshotAuth(t, repo, "devin-expiry", "devin", "Devin", map[string]any{"type": "devin", "auth_kind": "oauth"})
	now := time.Now().UTC()
	expiresAt := now.Add(5 * 24 * time.Hour)
	period := float64(1)
	window := QuotaWindow{ID: "devin-daily", Scope: "account", Mode: "fixed", Status: "healthy", Unit: "percentage", PeriodUnit: "day", PeriodValue: &period, ObservedAt: now, Source: "active_probe"}
	_, errWrite := repo.UpsertQuotaSnapshot(ctx, QuotaSnapshotWrite{
		CredentialID: "devin-expiry", QuotaStatus: "healthy", CollectionStatus: "success", Source: "active_probe", ObservedAt: &now,
		Plan: &QuotaPlan{Name: "Pro", ExpiresAt: &expiresAt}, ReplacePlan: true, ReplaceWindows: true, Windows: []QuotaWindow{window},
	})
	if errWrite != nil {
		t.Fatal(errWrite)
	}
	later := now.Add(time.Minute)
	window.ObservedAt = later
	window.Source = "response_header"
	_, errPassive := repo.UpsertQuotaSnapshot(ctx, QuotaSnapshotWrite{
		CredentialID: "devin-expiry", QuotaStatus: "healthy", CollectionStatus: "success", Source: "response_header", ObservedAt: &later, Windows: []QuotaWindow{window},
	})
	if errPassive != nil {
		t.Fatal(errPassive)
	}
	claimed, errClaim := repo.ForceClaimEligibleQuotaProbe(ctx, "devin-expiry", "test-home", later, time.Minute)
	if errClaim != nil || !claimed {
		t.Fatalf("probe claim failed: claimed=%v error=%v", claimed, errClaim)
	}
	errFail := repo.FailQuotaProbeAt(ctx, "devin-expiry", "test-home", QuotaCollectionError{Code: "UPSTREAM_UNAVAILABLE", Message: "Quota endpoint unavailable."}, later.Add(time.Minute), later)
	if errFail != nil {
		t.Fatal(errFail)
	}
	item, errGet := repo.GetQuotaCredential(ctx, "devin-expiry", later)
	if errGet != nil || item.Plan == nil || item.Plan.ExpiresAt == nil || !item.Plan.ExpiresAt.Equal(expiresAt) || item.CollectionStatus != "failed" {
		t.Fatalf("last-known plan expiry was not preserved: item=%+v error=%v", item, errGet)
	}
}

func TestQuotaPlanExpirySnapshotFormatCompatibility(t *testing.T) {
	for _, version := range []int{1, 14, 15, currentDatabaseVersion} {
		models, ok := databaseSnapshotModels(version)
		if !ok {
			t.Fatalf("snapshot format %d is unsupported", version)
		}
		found := false
		for _, model := range models {
			if model.name != "quota_snapshot" {
				continue
			}
			found = true
			_, hasExpiry := reflect.TypeOf(model.newRecord()).Elem().FieldByName("PlanExpiresAt")
			if hasExpiry != (version == currentDatabaseVersion) {
				t.Fatalf("snapshot format %d has incompatible plan expiry shape", version)
			}
		}
		if !found {
			t.Fatalf("snapshot format %d lost the quota table", version)
		}
	}
}
