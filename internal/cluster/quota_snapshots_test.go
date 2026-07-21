package cluster

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/tidwall/gjson"
	"gorm.io/gorm"
)

func TestQuotaAutoMigrateCreatesSnapshotTables(t *testing.T) {
	repo, closeRepo := newBillingTestRepository(t, context.Background())
	defer closeRepo()
	db, errDB := repo.database()
	if errDB != nil {
		t.Fatalf("database() error = %v", errDB)
	}
	for _, table := range []string{"quota_snapshot", "quota_window"} {
		if !db.Migrator().HasTable(table) {
			t.Fatalf("table %s was not migrated", table)
		}
	}
}

func TestAppendUsagePersistsCodexQuotaHeaderSnapshot(t *testing.T) {
	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	seedQuotaSnapshotAuth(t, repo, "codex-auth", "codex", "Codex Team", map[string]any{"type": "codex", "email": "ops@example.com"})

	payload := `{
        "timestamp":"2026-07-16T01:00:00Z",
        "provider":"codex",
        "auth_type":"oauth",
        "auth_index":"codex-auth",
        "request_id":"req-quota-1",
		"response_headers":{
		  "X-Codex-Primary-Used-Percent":["82"],
		  "X-Codex-Primary-Window-Minutes":["300"],
		  "X-Codex-Primary-Reset-After-Seconds":["600"],
		  "X-Upstream-Request-Id":["upstream-quota-1"],
		  "Authorization":["Bearer must-not-persist"]
        }
	      }`
	sanitized, errSanitize := SanitizeUsagePayloadSecrets(payload)
	if errSanitize != nil {
		t.Fatalf("SanitizeUsagePayloadSecrets() error = %v", errSanitize)
	}
	if !gjson.Get(sanitized, "quota_headers").IsObject() || gjson.Get(sanitized, "quota_headers.X-Codex-Primary-Used-Percent").String() != "82" {
		t.Fatalf("quota headers were not extracted: %s", sanitized)
	}
	_, errAppend := repo.AppendUsageWithRuntime(ctx, payload, UsageRuntimeMetadata{HomeIP: "192.0.2.10", HomePort: 8327, CPANodeID: "cpa-a", CPALabel: "CPA A"})
	if errAppend != nil {
		t.Fatalf("AppendUsageWithRuntime() error = %v", errAppend)
	}

	item, errGet := repo.GetQuotaCredential(ctx, "codex-auth", time.Date(2026, 7, 16, 1, 10, 0, 0, time.UTC))
	if errGet != nil {
		t.Fatalf("GetQuotaCredential() error = %v", errGet)
	}
	if item.QuotaStatus != "low" || item.Freshness != "fresh" || item.CollectionStatus != "partial" {
		t.Fatalf("quota state = %s/%s/%s, want low/fresh/partial", item.QuotaStatus, item.Freshness, item.CollectionStatus)
	}
	if item.Source == nil || *item.Source != "response_header" || len(item.Windows) != 1 {
		t.Fatalf("source/windows = %v/%d, want response_header/1", item.Source, len(item.Windows))
	}
	window := item.Windows[0]
	if window.PeriodUnit != "hour" || window.PeriodValue == nil || *window.PeriodValue != 5 || window.RemainingRatio == nil || math.Abs(*window.RemainingRatio-0.18) > 1e-9 {
		t.Fatalf("unexpected normalized window: %+v", window)
	}
	if item.Runtime == nil || item.Runtime.HomeID != "192.0.2.10:8327" || item.Runtime.CPANodeID != "cpa-a" {
		t.Fatalf("unexpected runtime ownership: %+v", item.Runtime)
	}

	db, errDB := repo.database()
	if errDB != nil {
		t.Fatalf("database() error = %v", errDB)
	}
	var usage UsageRecord
	if errFirst := db.First(&usage, "request_id = ?", "req-quota-1").Error; errFirst != nil {
		t.Fatalf("load usage: %v", errFirst)
	}
	stored := string(usage.PayloadJSON)
	if strings.Contains(stored, "must-not-persist") || gjson.Get(stored, "quota_headers.Authorization").Exists() || gjson.Get(stored, "response_headers").Exists() {
		t.Fatalf("usage payload leaked rejected header: %s", stored)
	}
	if gjson.Get(stored, "upstream_request_id").String() != "upstream-quota-1" {
		t.Fatalf("safe upstream request id was not preserved: %s", stored)
	}
}

func TestUpsertQuotaSnapshotRejectsLateObservation(t *testing.T) {
	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	seedQuotaSnapshotAuth(t, repo, "codex-late", "codex", "Codex Late", map[string]any{"type": "codex"})

	newer := time.Date(2026, 7, 16, 2, 0, 0, 0, time.UTC)
	older := newer.Add(-time.Hour)
	for _, observedAt := range []time.Time{newer, older} {
		remaining := 0.8
		status := "healthy"
		if observedAt.Equal(older) {
			remaining = 0
			status = "exhausted"
		}
		_, errUpsert := repo.UpsertQuotaSnapshot(ctx, QuotaSnapshotWrite{
			CredentialID: "codex-late", QuotaStatus: status, CollectionStatus: "success", Source: "response_header",
			ObservedAt: &observedAt, LastAttemptAt: &observedAt, LastSuccessAt: &observedAt, ReplaceWindows: true,
			Windows: []QuotaWindow{{ID: "codex-primary", Scope: "account", Mode: "rolling", Status: status, Unit: "percentage", RemainingRatio: &remaining, PeriodUnit: "hour", PeriodValue: float64Ptr(5), Source: "response_header", ObservedAt: observedAt}},
		})
		if errUpsert != nil {
			t.Fatalf("UpsertQuotaSnapshot(%s) error = %v", observedAt, errUpsert)
		}
	}
	item, errGet := repo.GetQuotaCredential(ctx, "codex-late", newer.Add(time.Minute))
	if errGet != nil {
		t.Fatalf("GetQuotaCredential() error = %v", errGet)
	}
	if item.QuotaStatus != "healthy" || item.ObservedAt == nil || !item.ObservedAt.Equal(newer) || item.Windows[0].Status != "healthy" {
		t.Fatalf("late observation replaced newer state: %+v", item)
	}
}

func TestPartialHeaderObservationMergesExistingProbeWindows(t *testing.T) {
	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	seedQuotaSnapshotAuth(t, repo, "codex-merge", "codex", "Codex Merge", map[string]any{"type": "codex"})
	probeAt := time.Date(2026, 7, 16, 2, 0, 0, 0, time.UTC)
	periodFive := float64(5)
	periodWeek := float64(1)
	remainingHealthy := 0.7
	_, errSeed := repo.UpsertQuotaSnapshot(ctx, QuotaSnapshotWrite{
		CredentialID: "codex-merge", QuotaStatus: "healthy", CollectionStatus: "success", Source: "active_probe",
		ObservedAt: &probeAt, LastSuccessAt: &probeAt, ReplaceWindows: true,
		Windows: []QuotaWindow{
			{ID: "codex-primary", Scope: "account", Mode: "rolling", Status: "healthy", Unit: "percentage", RemainingRatio: &remainingHealthy, PeriodUnit: "hour", PeriodValue: &periodFive, Source: "active_probe", ObservedAt: probeAt},
			{ID: "codex-secondary", Scope: "account", Mode: "rolling", Status: "healthy", Unit: "percentage", RemainingRatio: &remainingHealthy, PeriodUnit: "week", PeriodValue: &periodWeek, Source: "active_probe", ObservedAt: probeAt},
		},
	})
	if errSeed != nil {
		t.Fatalf("seed active probe snapshot: %v", errSeed)
	}
	headerAt := probeAt.Add(time.Minute)
	payload := `{"timestamp":"` + headerAt.Format(time.RFC3339) + `","provider":"codex","auth_type":"oauth","auth_index":"codex-merge","request_id":"req-merge","response_headers":{"X-Codex-Primary-Used-Percent":["95"],"X-Codex-Primary-Window-Minutes":["300"],"X-Codex-Primary-Reset-After-Seconds":["60"]}}`
	if _, errAppend := repo.AppendUsageWithRuntime(ctx, payload, UsageRuntimeMetadata{}); errAppend != nil {
		t.Fatalf("AppendUsageWithRuntime() error = %v", errAppend)
	}
	item, errGet := repo.GetQuotaCredential(ctx, "codex-merge", headerAt)
	if errGet != nil {
		t.Fatalf("GetQuotaCredential() error = %v", errGet)
	}
	if item.CollectionStatus != "partial" || item.QuotaStatus != "low" || item.Source == nil || *item.Source != "mixed" || len(item.Windows) != 2 {
		t.Fatalf("partial merge state = %+v", item)
	}
	var secondary *QuotaWindow
	for index := range item.Windows {
		if item.Windows[index].ID == "codex-secondary" {
			secondary = &item.Windows[index]
			break
		}
	}
	if secondary == nil || secondary.Source != "active_probe" || !secondary.ObservedAt.Equal(probeAt) {
		t.Fatalf("previous probe window was not preserved: %+v", item.Windows)
	}
}

func TestPartialHeaderObservationPrunesExpiredWindowsOnly(t *testing.T) {
	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	seedQuotaSnapshotAuth(t, repo, "codex-expiring-merge", "codex", "Codex Expiring Merge", map[string]any{"type": "codex"})
	probeAt := time.Date(2026, 7, 16, 2, 0, 0, 0, time.UTC)
	headerAt := probeAt.Add(10 * time.Minute)
	oldExpiry := headerAt.Add(-time.Second)
	validExpiry := headerAt.Add(5 * time.Minute)
	periodFive, periodWeek := float64(5), float64(1)
	remaining := 0.7
	_, errSeed := repo.UpsertQuotaSnapshot(ctx, QuotaSnapshotWrite{
		CredentialID: "codex-expiring-merge", QuotaStatus: "healthy", CollectionStatus: "success", Source: "active_probe",
		ObservedAt: &probeAt, ExpiresAt: &validExpiry, LastSuccessAt: &probeAt, ReplaceWindows: true,
		Windows: []QuotaWindow{
			{ID: "codex-primary", Scope: "account", Mode: "rolling", Status: "healthy", Unit: "percentage", RemainingRatio: &remaining, PeriodUnit: "hour", PeriodValue: &periodFive, Source: "active_probe", ObservedAt: probeAt, ExpiresAt: &oldExpiry},
			{ID: "codex-secondary", Scope: "account", Mode: "rolling", Status: "healthy", Unit: "percentage", RemainingRatio: &remaining, PeriodUnit: "week", PeriodValue: &periodWeek, Source: "active_probe", ObservedAt: probeAt, ExpiresAt: &validExpiry},
			{ID: "codex-expired-extra", Scope: "account", Mode: "rolling", Status: "exhausted", Unit: "percentage", RemainingRatio: float64Ptr(0), PeriodUnit: "hour", PeriodValue: &periodFive, Source: "active_probe", ObservedAt: probeAt, ExpiresAt: &oldExpiry},
		},
	})
	if errSeed != nil {
		t.Fatalf("seed snapshot: %v", errSeed)
	}
	payload := `{"timestamp":"` + headerAt.Format(time.RFC3339) + `","provider":"codex","auth_type":"oauth","auth_index":"codex-expiring-merge","request_id":"req-expiring-merge","response_headers":{"X-Codex-Primary-Used-Percent":["95"],"X-Codex-Primary-Window-Minutes":["300"],"X-Codex-Primary-Reset-After-Seconds":["60"]}}`
	if _, errAppend := repo.AppendUsageWithRuntime(ctx, payload, UsageRuntimeMetadata{}); errAppend != nil {
		t.Fatalf("AppendUsageWithRuntime() error = %v", errAppend)
	}
	item, errGet := repo.GetQuotaCredential(ctx, "codex-expiring-merge", headerAt)
	if errGet != nil {
		t.Fatalf("GetQuotaCredential() error = %v", errGet)
	}
	windowsByID := make(map[string]QuotaWindow, len(item.Windows))
	for _, window := range item.Windows {
		windowsByID[window.ID] = window
	}
	primary, primaryOK := windowsByID["codex-primary"]
	_, secondaryOK := windowsByID["codex-secondary"]
	_, expiredOK := windowsByID["codex-expired-extra"]
	if len(item.Windows) != 2 || !primaryOK || !primary.ObservedAt.Equal(headerAt) || !secondaryOK || expiredOK {
		t.Fatalf("expired/still-valid merge = %+v", item.Windows)
	}
	itemAfterOldExpiry, errAfterExpiry := repo.GetQuotaCredential(ctx, "codex-expiring-merge", headerAt.Add(10*time.Minute))
	if errAfterExpiry != nil {
		t.Fatalf("GetQuotaCredential(after old expiry) error = %v", errAfterExpiry)
	}
	if len(itemAfterOldExpiry.Windows) != 1 || itemAfterOldExpiry.Windows[0].ID != "codex-primary" || itemAfterOldExpiry.Source == nil || *itemAfterOldExpiry.Source != "response_header" || itemAfterOldExpiry.QuotaStatus != "low" {
		t.Fatalf("fresh snapshot retained expired merged window: %+v", itemAfterOldExpiry)
	}
	itemStale, errStale := repo.GetQuotaCredential(ctx, "codex-expiring-merge", headerAt.Add(31*time.Minute))
	if errStale != nil {
		t.Fatalf("GetQuotaCredential(stale) error = %v", errStale)
	}
	if itemStale.Freshness != "stale" || len(itemStale.Windows) != 2 {
		t.Fatalf("stale snapshot did not retain last-known windows: %+v", itemStale)
	}
}

func TestListQuotaCredentialsReturnsFilteredAndGlobalSummaries(t *testing.T) {
	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	seedQuotaSnapshotAuth(t, repo, "codex-filter", "codex", "Codex Filter", map[string]any{"type": "codex"})
	seedQuotaSnapshotAuth(t, repo, "vertex-filter", "vertex", "Vertex Filter", nil)
	now := time.Date(2026, 7, 16, 3, 0, 0, 0, time.UTC)
	expiresAt := now.Add(time.Hour)
	_, errUpsert := repo.UpsertQuotaSnapshot(ctx, QuotaSnapshotWrite{CredentialID: "codex-filter", QuotaStatus: "healthy", CollectionStatus: "success", Source: "active_probe", ObservedAt: &now, ExpiresAt: &expiresAt, LastSuccessAt: &now})
	if errUpsert != nil {
		t.Fatalf("UpsertQuotaSnapshot() error = %v", errUpsert)
	}
	result, errList := repo.ListQuotaCredentials(ctx, QuotaListQuery{Limit: 50, Providers: map[string]struct{}{"codex": {}}, Sort: "risk_desc", Now: now})
	if errList != nil {
		t.Fatalf("ListQuotaCredentials() error = %v", errList)
	}
	if result.Total != 1 || result.Summary.TotalCredentials != 1 || result.Summary.Healthy != 1 {
		t.Fatalf("filtered summary = %+v total=%d", result.Summary, result.Total)
	}
	if result.GlobalSummary.TotalCredentials != 2 || result.GlobalSummary.Unsupported != 1 {
		t.Fatalf("global summary = %+v, want total=2 unsupported=1", result.GlobalSummary)
	}
}

func TestClaimQuotaProbeUsesExpiringLease(t *testing.T) {
	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	now := time.Date(2026, 7, 16, 6, 0, 0, 0, time.UTC)
	claimed, errClaim := repo.ClaimQuotaProbe(ctx, "lease-auth", "home-a", now, time.Minute)
	if errClaim != nil || !claimed {
		t.Fatalf("first claim = %v, %v, want true", claimed, errClaim)
	}
	claimed, errClaim = repo.ClaimQuotaProbe(ctx, "lease-auth", "home-b", now.Add(30*time.Second), time.Minute)
	if errClaim != nil || claimed {
		t.Fatalf("concurrent claim = %v, %v, want false", claimed, errClaim)
	}
	claimed, errClaim = repo.ClaimQuotaProbe(ctx, "lease-auth", "home-b", now.Add(2*time.Minute), time.Minute)
	if errClaim != nil || !claimed {
		t.Fatalf("expired claim = %v, %v, want true", claimed, errClaim)
	}
}

func TestForceClaimEligibleQuotaProbeBypassesFreshnessButNotLease(t *testing.T) {
	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	now := time.Date(2026, 7, 16, 6, 30, 0, 0, time.UTC)
	seedQuotaSnapshotAuth(t, repo, "force-fresh-auth", "codex", "Force Fresh", map[string]any{"type": "codex"})
	expiresAt := now.Add(30 * time.Minute)
	if _, errSeed := repo.UpsertQuotaSnapshot(ctx, QuotaSnapshotWrite{
		CredentialID: "force-fresh-auth", QuotaStatus: "healthy", CollectionStatus: "success", Source: "active_probe",
		ObservedAt: &now, ExpiresAt: &expiresAt, NextProbeAt: &expiresAt, LastSuccessAt: &now,
	}); errSeed != nil {
		t.Fatalf("UpsertQuotaSnapshot() error = %v", errSeed)
	}
	claimed, errClaim := repo.ClaimEligibleQuotaProbe(ctx, "force-fresh-auth", "home-a", now, time.Minute)
	if errClaim != nil || claimed {
		t.Fatalf("normal fresh claim = %v, %v, want false, nil", claimed, errClaim)
	}
	claimed, errClaim = repo.ForceClaimEligibleQuotaProbe(ctx, "force-fresh-auth", "home-a", now, time.Minute)
	if errClaim != nil || !claimed {
		t.Fatalf("forced fresh claim = %v, %v, want true, nil", claimed, errClaim)
	}
	claimed, errClaim = repo.ForceClaimEligibleQuotaProbe(ctx, "force-fresh-auth", "home-b", now.Add(30*time.Second), time.Minute)
	if errClaim != nil || claimed {
		t.Fatalf("forced leased claim = %v, %v, want false, nil", claimed, errClaim)
	}
}

func TestQuotaProbeCompletionRequiresCurrentLeaseOwner(t *testing.T) {
	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	now := time.Date(2026, 7, 16, 7, 0, 0, 0, time.UTC)
	seedQuotaSnapshotAuth(t, repo, "lease-complete-auth", "codex", "Lease Complete", map[string]any{"type": "codex"})
	claimed, errClaim := repo.ClaimQuotaProbe(ctx, "lease-complete-auth", "home-a", now, time.Minute)
	if errClaim != nil || !claimed {
		t.Fatalf("first claim = %v, %v", claimed, errClaim)
	}
	claimed, errClaim = repo.ClaimQuotaProbe(ctx, "lease-complete-auth", "home-b", now.Add(2*time.Minute), time.Minute)
	if errClaim != nil || !claimed {
		t.Fatalf("replacement claim = %v, %v", claimed, errClaim)
	}
	period := float64(5)
	updated, errUpsert := repo.UpsertQuotaSnapshot(ctx, QuotaSnapshotWrite{
		CredentialID: "lease-complete-auth", QuotaStatus: "healthy", CollectionStatus: "success", Source: "active_probe",
		ObservedAt: &now, ExpectedProbeOwner: "home-a", ClearProbeLease: true, ReplaceWindows: true,
		Windows: []QuotaWindow{{ID: "codex-primary", Scope: "account", Mode: "rolling", Status: "healthy", Unit: "percentage", PeriodUnit: "hour", PeriodValue: &period, Source: "active_probe", ObservedAt: now}},
	})
	if errUpsert != nil || updated {
		t.Fatalf("stale owner completion = %v, %v, want ignored", updated, errUpsert)
	}
	item, errGet := repo.GetQuotaCredential(ctx, "lease-complete-auth", now)
	if errGet != nil {
		t.Fatalf("GetQuotaCredential() error = %v", errGet)
	}
	if item.CollectionStatus != "collecting" || len(item.Windows) != 0 {
		t.Fatalf("stale owner mutated claimed snapshot: %+v", item)
	}
}

func TestCodexHeaderInvalidatesInFlightProbeCompletionAndFailure(t *testing.T) {
	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	now := time.Date(2026, 7, 16, 7, 30, 0, 0, time.UTC)
	seedQuotaSnapshotAuth(t, repo, "header-wins", "codex", "Header Wins", map[string]any{"type": "codex"})
	claimed, errClaim := repo.ClaimQuotaProbe(ctx, "header-wins", "home-a", now, time.Minute)
	if errClaim != nil || !claimed {
		t.Fatalf("ClaimQuotaProbe() = %v, %v", claimed, errClaim)
	}
	headerAt := now.Add(10 * time.Second)
	payload := `{"timestamp":"` + headerAt.Format(time.RFC3339) + `","provider":"codex","auth_type":"oauth","auth_index":"header-wins","request_id":"req-header-wins","response_headers":{"X-Codex-Primary-Used-Percent":["95"],"X-Codex-Primary-Window-Minutes":["300"],"X-Codex-Primary-Reset-After-Seconds":["60"]}}`
	if _, errAppend := repo.AppendUsageWithRuntime(ctx, payload, UsageRuntimeMetadata{CPANodeID: "cpa-new"}); errAppend != nil {
		t.Fatalf("AppendUsageWithRuntime() error = %v", errAppend)
	}
	probeAt := headerAt.Add(10 * time.Second)
	period := float64(5)
	updated, errComplete := repo.UpsertQuotaSnapshot(ctx, QuotaSnapshotWrite{
		CredentialID: "header-wins", QuotaStatus: "healthy", CollectionStatus: "success", Source: "active_probe",
		ObservedAt: &probeAt, ExpectedProbeOwner: "home-a", ClearProbeLease: true, ReplaceWindows: true,
		Windows: []QuotaWindow{{ID: "codex-primary", Scope: "account", Mode: "rolling", Status: "healthy", Unit: "percentage", PeriodUnit: "hour", PeriodValue: &period, Source: "active_probe", ObservedAt: probeAt}},
	})
	if errComplete != nil || updated {
		t.Fatalf("stale probe completion = %v, %v, want ignored", updated, errComplete)
	}
	occurredAt := probeAt
	errFail := repo.FailQuotaProbeAt(ctx, "header-wins", "home-a", QuotaCollectionError{Code: "UPSTREAM_UNAVAILABLE", Message: "stale probe", Retryable: true, OccurredAt: &occurredAt}, probeAt.Add(time.Minute), probeAt)
	if !errors.Is(errFail, ErrQuotaProbeLeaseLost) {
		t.Fatalf("stale probe failure error = %v, want ErrQuotaProbeLeaseLost", errFail)
	}
	item, errGet := repo.GetQuotaCredential(ctx, "header-wins", probeAt)
	if errGet != nil {
		t.Fatalf("GetQuotaCredential() error = %v", errGet)
	}
	if item.Source == nil || *item.Source != "response_header" || item.CollectionStatus != "partial" || item.QuotaStatus != "low" || item.Runtime == nil || item.Runtime.CPANodeID != "cpa-new" {
		t.Fatalf("stale probe overwrote header snapshot: %+v", item)
	}
}

func TestQuotaSnapshotWithoutExpiryIsNotFreshForever(t *testing.T) {
	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	seedQuotaSnapshotAuth(t, repo, "quota-no-expiry", "codex", "No Expiry", map[string]any{"type": "codex"})
	db, errDB := repo.database()
	if errDB != nil {
		t.Fatalf("database() error = %v", errDB)
	}
	observedAt := time.Date(2026, 7, 16, 8, 0, 0, 0, time.UTC)
	record := QuotaSnapshotRecord{CredentialID: "quota-no-expiry", QuotaStatus: "healthy", CollectionStatus: "success", Source: "active_probe", ObservedAt: &observedAt, ParserVersion: 1, CollectorVersion: 1, CreatedAt: observedAt, UpdatedAt: observedAt}
	if errCreate := db.Create(&record).Error; errCreate != nil {
		t.Fatalf("create legacy snapshot: %v", errCreate)
	}
	item, errGet := repo.GetQuotaCredential(ctx, "quota-no-expiry", observedAt.Add(time.Minute))
	if errGet != nil {
		t.Fatalf("GetQuotaCredential() error = %v", errGet)
	}
	if item.Freshness != "stale" {
		t.Fatalf("freshness = %s, want stale", item.Freshness)
	}
}

func TestQuotaSnapshotExpressesUnlimitedAndBalanceWindows(t *testing.T) {
	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	seedQuotaSnapshotAuth(t, repo, "quota-shapes", "kimi", "Quota Shapes", map[string]any{"type": "kimi"})
	now := time.Date(2026, 7, 16, 8, 0, 0, 0, time.UTC)
	remaining := 42.0
	_, errUpsert := repo.UpsertQuotaSnapshot(ctx, QuotaSnapshotWrite{
		CredentialID: "quota-shapes", QuotaStatus: "healthy", CollectionStatus: "success", Source: "active_probe", ObservedAt: &now, ReplaceWindows: true,
		Windows: []QuotaWindow{
			{ID: "unlimited", Scope: "account", Mode: "balance", Status: "healthy", Unit: "requests", IsUnlimited: true, PeriodUnit: "unknown", Source: "active_probe", ObservedAt: now},
			{ID: "balance", Scope: "account", Mode: "balance", Status: "unknown", Unit: "credits", Remaining: &remaining, PeriodUnit: "unknown", Source: "active_probe", ObservedAt: now},
		},
	})
	if errUpsert != nil {
		t.Fatalf("UpsertQuotaSnapshot() error = %v", errUpsert)
	}
	item, errGet := repo.GetQuotaCredential(ctx, "quota-shapes", now)
	if errGet != nil {
		t.Fatalf("GetQuotaCredential() error = %v", errGet)
	}
	if len(item.Windows) != 2 || !item.Windows[1].IsUnlimited && !item.Windows[0].IsUnlimited {
		t.Fatalf("unlimited/balance windows not preserved: %+v", item.Windows)
	}
}

func TestProviderAPIKeyQuotaIsExplicitlyUnsupported(t *testing.T) {
	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	now := time.Date(2026, 7, 16, 8, 0, 0, 0, time.UTC)
	auth := &coreauth.Auth{ID: "xai-api-key-quota", Index: "xai-api-key-quota", Provider: "xai", Label: "xAI API Key", Status: coreauth.StatusActive, Attributes: map[string]string{"source": "config:xai-api-key", "api_key": "must-not-leak"}, CreatedAt: now, UpdatedAt: now}
	if _, errUpsert := repo.UpsertAuth(ctx, auth, "test"); errUpsert != nil {
		t.Fatalf("UpsertAuth() error = %v", errUpsert)
	}
	item, errGet := repo.GetQuotaCredential(ctx, auth.ID, now)
	if errGet != nil {
		t.Fatalf("GetQuotaCredential() error = %v", errGet)
	}
	if item.CredentialType != "provider_api_key" || item.QuotaStatus != "unsupported" || item.CollectionStatus != "unsupported" {
		t.Fatalf("unexpected API-key quota support state: %+v", item)
	}
}

func TestFailedProbeWithoutLastKnownSnapshotReturnsErrorStatus(t *testing.T) {
	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	seedQuotaSnapshotAuth(t, repo, "quota-first-failure", "codex", "First Failure", map[string]any{"type": "codex"})
	now := time.Date(2026, 7, 16, 9, 0, 0, 0, time.UTC)
	claimed, errClaim := repo.ClaimQuotaProbe(ctx, "quota-first-failure", "home-a", now, time.Minute)
	if errClaim != nil || !claimed {
		t.Fatalf("ClaimQuotaProbe() = %v, %v, want claimed", claimed, errClaim)
	}
	occurredAt := now.Add(time.Second)
	statusCode := 429
	if errFail := repo.FailQuotaProbe(ctx, "quota-first-failure", "home-a", QuotaCollectionError{
		Code: "UPSTREAM_RATE_LIMITED", Message: "Authorization: Bearer secret-token Cookie=session-secret", Retryable: true,
		OccurredAt: &occurredAt, UpstreamStatusCode: &statusCode,
	}, now.Add(5*time.Minute)); errFail != nil {
		t.Fatalf("FailQuotaProbe() error = %v", errFail)
	}
	item, errGet := repo.GetQuotaCredential(ctx, "quota-first-failure", now.Add(time.Minute))
	if errGet != nil {
		t.Fatalf("GetQuotaCredential() error = %v", errGet)
	}
	if item.QuotaStatus != "error" || item.Freshness != "never" || item.CollectionStatus != "failed" {
		t.Fatalf("first failure state = %s/%s/%s, want error/never/failed", item.QuotaStatus, item.Freshness, item.CollectionStatus)
	}
	if item.Error == nil || strings.Contains(item.Error.Message, "secret-token") || strings.Contains(item.Error.Message, "session-secret") || len(item.Error.Message) > 500 {
		t.Fatalf("unsafe collection error = %+v", item.Error)
	}
}

func TestQuotaCredentialStatusRemainsIndependentFromQuotaStatus(t *testing.T) {
	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	now := time.Date(2026, 7, 16, 10, 0, 0, 0, time.UTC)
	expiresAt := now.Add(time.Hour)
	remaining := 0.1
	period := float64(5)
	for _, auth := range []*coreauth.Auth{
		{ID: "quota-disabled", Index: "quota-disabled", Provider: "codex", Label: "Disabled Low", Status: coreauth.StatusDisabled, Disabled: true, Metadata: map[string]any{"type": "codex"}, CreatedAt: now, UpdatedAt: now},
		{ID: "quota-cooldown", Index: "quota-cooldown", Provider: "codex", Label: "Cooldown Low", Status: coreauth.StatusError, Unavailable: true, NextRetryAfter: now.Add(time.Hour), Metadata: map[string]any{"type": "codex"}, CreatedAt: now, UpdatedAt: now},
	} {
		if _, errUpsert := repo.UpsertAuth(ctx, auth, "test"); errUpsert != nil {
			t.Fatalf("UpsertAuth(%s) error = %v", auth.ID, errUpsert)
		}
		_, errSnapshot := repo.UpsertQuotaSnapshot(ctx, QuotaSnapshotWrite{
			CredentialID: auth.ID, QuotaStatus: "low", CollectionStatus: "success", Source: "active_probe",
			ObservedAt: &now, ExpiresAt: &expiresAt, ReplaceWindows: true,
			Windows: []QuotaWindow{{ID: "primary", Scope: "account", Mode: "rolling", Status: "low", Unit: "percentage", RemainingRatio: &remaining, PeriodUnit: "hour", PeriodValue: &period, Source: "active_probe", ObservedAt: now}},
		})
		if errSnapshot != nil {
			t.Fatalf("UpsertQuotaSnapshot(%s) error = %v", auth.ID, errSnapshot)
		}
	}
	tests := map[string]string{"quota-disabled": "disabled", "quota-cooldown": "cooldown"}
	for credentialID, wantCredentialStatus := range tests {
		item, errGet := repo.GetQuotaCredential(ctx, credentialID, now)
		if errGet != nil {
			t.Fatalf("GetQuotaCredential(%s) error = %v", credentialID, errGet)
		}
		if item.CredentialStatus != wantCredentialStatus || item.QuotaStatus != "low" {
			t.Fatalf("credential/quota status for %s = %s/%s, want %s/low", credentialID, item.CredentialStatus, item.QuotaStatus, wantCredentialStatus)
		}
	}
}

func TestQuotaListCombinesFiltersAndUsesStableSorts(t *testing.T) {
	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	now := time.Date(2026, 7, 16, 11, 0, 0, 0, time.UTC)
	for _, seed := range []struct {
		id, provider, label, account, project, quotaStatus, source string
		observedAt, resetAt                                        time.Time
	}{
		{id: "quota-a", provider: "codex", label: "Bravo", account: "alpha@example.com", project: "project-a", quotaStatus: "low", source: "response_header", observedAt: now.Add(-time.Minute), resetAt: now.Add(2 * time.Hour)},
		{id: "quota-b", provider: "claude", label: "Alpha", account: "bravo@example.com", project: "project-b", quotaStatus: "healthy", source: "active_probe", observedAt: now.Add(-2 * time.Minute), resetAt: now.Add(time.Hour)},
		{id: "quota-c", provider: "codex", label: "Bravo", account: "charlie@example.com", project: "project-c", quotaStatus: "low", source: "active_probe", observedAt: now.Add(-time.Minute), resetAt: now.Add(3 * time.Hour)},
	} {
		seedQuotaSnapshotAuth(t, repo, seed.id, seed.provider, seed.label, map[string]any{"type": seed.provider, "email": seed.account, "project_id": seed.project})
		expiresAt := now.Add(time.Hour)
		remaining := 0.1
		if seed.quotaStatus == "healthy" {
			remaining = 0.8
		}
		period := float64(5)
		_, errUpsert := repo.UpsertQuotaSnapshot(ctx, QuotaSnapshotWrite{
			CredentialID: seed.id, QuotaStatus: seed.quotaStatus, CollectionStatus: "success", Source: seed.source,
			ObservedAt: &seed.observedAt, ExpiresAt: &expiresAt, ReplaceWindows: true,
			Windows: []QuotaWindow{{ID: "primary", Scope: "account", Mode: "rolling", Status: seed.quotaStatus, Unit: "percentage", RemainingRatio: &remaining, ResetAt: &seed.resetAt, PeriodUnit: "hour", PeriodValue: &period, Source: seed.source, ObservedAt: seed.observedAt}},
		})
		if errUpsert != nil {
			t.Fatalf("UpsertQuotaSnapshot(%s) error = %v", seed.id, errUpsert)
		}
	}
	filtered, errList := repo.ListQuotaCredentials(ctx, QuotaListQuery{
		Limit: 50, Search: "proj...ct-c", Providers: map[string]struct{}{"codex": {}}, QuotaStatuses: map[string]struct{}{"low": {}},
		Freshness: map[string]struct{}{"fresh": {}}, Sources: map[string]struct{}{"active_probe": {}},
		CredentialStatuses: map[string]struct{}{"enabled": {}}, CollectionStatuses: map[string]struct{}{"success": {}}, Sort: "risk_desc", Now: now,
	})
	if errList != nil {
		t.Fatalf("ListQuotaCredentials() error = %v", errList)
	}
	if filtered.Total != 1 || len(filtered.Items) != 1 || filtered.Items[0].CredentialID != "quota-c" || filtered.Summary.TotalCredentials != 1 || filtered.GlobalSummary.TotalCredentials != 3 {
		t.Fatalf("combined filter result = %+v", filtered)
	}
	if filtered.Items[0].Account == nil || *filtered.Items[0].Account != "ch***@example.com" || filtered.Items[0].Project == nil || *filtered.Items[0].Project != "proj...ct-c" {
		t.Fatalf("masked account/project = %v/%v", filtered.Items[0].Account, filtered.Items[0].Project)
	}

	for _, test := range []struct {
		sort string
		want []string
	}{
		{sort: "observed_at_desc", want: []string{"quota-a", "quota-c", "quota-b"}},
		{sort: "observed_at_asc", want: []string{"quota-b", "quota-a", "quota-c"}},
		{sort: "reset_at_asc", want: []string{"quota-b", "quota-a", "quota-c"}},
		{sort: "provider_asc", want: []string{"quota-b", "quota-a", "quota-c"}},
		{sort: "label_asc", want: []string{"quota-b", "quota-a", "quota-c"}},
		{sort: "risk_desc", want: []string{"quota-a", "quota-c", "quota-b"}},
	} {
		result, errSort := repo.ListQuotaCredentials(ctx, QuotaListQuery{Limit: 50, Sort: test.sort, Now: now})
		if errSort != nil {
			t.Fatalf("ListQuotaCredentials(sort=%s) error = %v", test.sort, errSort)
		}
		if len(result.Items) != len(test.want) {
			t.Fatalf("sort %s returned %d items, want %d", test.sort, len(result.Items), len(test.want))
		}
		for index, wantID := range test.want {
			if result.Items[index].CredentialID != wantID {
				t.Fatalf("sort %s item %d = %s, want %s", test.sort, index, result.Items[index].CredentialID, wantID)
			}
		}
	}
}

func TestQuotaListEarliestResetUsesAllWindowsAndMatchesSort(t *testing.T) {
	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	now := time.Date(2026, 7, 16, 11, 30, 0, 0, time.UTC)
	expiresAt := now.Add(2 * time.Hour)
	period := float64(5)
	remaining := 0.8

	seedQuotaSnapshotAuth(t, repo, "quota-hidden-reset", "codex", "Hidden Reset", map[string]any{"type": "codex"})
	primaryReset := now.Add(2 * time.Hour)
	secondaryReset := now.Add(3 * time.Hour)
	hiddenReset := now.Add(30 * time.Minute)
	_, errHidden := repo.UpsertQuotaSnapshot(ctx, QuotaSnapshotWrite{
		CredentialID: "quota-hidden-reset", QuotaStatus: "healthy", CollectionStatus: "success", Source: "active_probe",
		ObservedAt: &now, ExpiresAt: &expiresAt, ReplaceWindows: true,
		Windows: []QuotaWindow{
			{ID: "primary", Priority: 0, Scope: "account", Mode: "rolling", Status: "healthy", Unit: "percentage", RemainingRatio: &remaining, ResetAt: &primaryReset, PeriodUnit: "hour", PeriodValue: &period, Source: "active_probe", ObservedAt: now},
			{ID: "secondary", Priority: 1, Scope: "account", Mode: "rolling", Status: "healthy", Unit: "percentage", RemainingRatio: &remaining, ResetAt: &secondaryReset, PeriodUnit: "hour", PeriodValue: &period, Source: "active_probe", ObservedAt: now},
			{ID: "hidden-earliest", Priority: 10, Scope: "model", Mode: "rolling", Status: "healthy", Unit: "percentage", RemainingRatio: &remaining, ResetAt: &hiddenReset, PeriodUnit: "hour", PeriodValue: &period, Source: "active_probe", ObservedAt: now},
		},
	})
	if errHidden != nil {
		t.Fatalf("UpsertQuotaSnapshot(hidden) error = %v", errHidden)
	}

	seedQuotaSnapshotAuth(t, repo, "quota-visible-reset", "claude", "Visible Reset", map[string]any{"type": "claude"})
	visibleReset := now.Add(time.Hour)
	_, errVisible := repo.UpsertQuotaSnapshot(ctx, QuotaSnapshotWrite{
		CredentialID: "quota-visible-reset", QuotaStatus: "healthy", CollectionStatus: "success", Source: "active_probe",
		ObservedAt: &now, ExpiresAt: &expiresAt, ReplaceWindows: true,
		Windows: []QuotaWindow{{ID: "primary", Scope: "account", Mode: "rolling", Status: "healthy", Unit: "percentage", RemainingRatio: &remaining, ResetAt: &visibleReset, PeriodUnit: "hour", PeriodValue: &period, Source: "active_probe", ObservedAt: now}},
	})
	if errVisible != nil {
		t.Fatalf("UpsertQuotaSnapshot(visible) error = %v", errVisible)
	}
	seedQuotaSnapshotAuth(t, repo, "quota-no-reset", "kimi", "No Reset", map[string]any{"type": "kimi"})
	_, errNoReset := repo.UpsertQuotaSnapshot(ctx, QuotaSnapshotWrite{
		CredentialID: "quota-no-reset", QuotaStatus: "healthy", CollectionStatus: "success", Source: "active_probe",
		ObservedAt: &now, ExpiresAt: &expiresAt, ReplaceWindows: true,
		Windows: []QuotaWindow{{ID: "primary", Scope: "account", Mode: "balance", Status: "healthy", Unit: "requests", Remaining: float64Ptr(10), PeriodUnit: "unknown", Source: "active_probe", ObservedAt: now}},
	})
	if errNoReset != nil {
		t.Fatalf("UpsertQuotaSnapshot(no reset) error = %v", errNoReset)
	}

	result, errList := repo.ListQuotaCredentials(ctx, QuotaListQuery{Limit: 50, Sort: "reset_at_asc", Now: now})
	if errList != nil {
		t.Fatalf("ListQuotaCredentials() error = %v", errList)
	}
	if len(result.Items) != 3 || result.Items[0].CredentialID != "quota-hidden-reset" || result.Items[1].CredentialID != "quota-visible-reset" || result.Items[2].CredentialID != "quota-no-reset" || result.Items[2].EarliestResetAt != nil {
		t.Fatalf("reset sort result = %+v", result.Items)
	}
	item := result.Items[0]
	if item.WindowCount != 3 || len(item.PrimaryWindows) != 2 || item.EarliestResetAt == nil || !item.EarliestResetAt.Equal(hiddenReset) {
		t.Fatalf("earliest reset/list compression = %+v", item)
	}
	for _, window := range item.PrimaryWindows {
		if window.ID == "hidden-earliest" {
			t.Fatalf("hidden earliest window unexpectedly included in primary windows: %+v", item.PrimaryWindows)
		}
	}

	staleItem, errStale := repo.GetQuotaCredential(ctx, "quota-hidden-reset", expiresAt.Add(time.Minute))
	if errStale != nil {
		t.Fatalf("GetQuotaCredential(stale) error = %v", errStale)
	}
	if staleItem.Freshness != "stale" || staleItem.EarliestResetAt == nil || !staleItem.EarliestResetAt.Equal(hiddenReset) {
		t.Fatalf("stale earliest reset = %+v", staleItem)
	}
}

func TestSoftDeleteAuthRemovesQuotaRowsAndVisibility(t *testing.T) {
	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	seedQuotaSnapshotAuth(t, repo, "quota-delete", "codex", "Delete Me", map[string]any{"type": "codex"})
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	period := float64(5)
	_, errSnapshot := repo.UpsertQuotaSnapshot(ctx, QuotaSnapshotWrite{
		CredentialID: "quota-delete", QuotaStatus: "healthy", CollectionStatus: "success", Source: "active_probe", ObservedAt: &now, ReplaceWindows: true,
		Windows: []QuotaWindow{{ID: "primary", Scope: "account", Mode: "rolling", Status: "healthy", Unit: "percentage", PeriodUnit: "hour", PeriodValue: &period, Source: "active_probe", ObservedAt: now}},
	})
	if errSnapshot != nil {
		t.Fatalf("UpsertQuotaSnapshot() error = %v", errSnapshot)
	}
	if errDelete := repo.SoftDeleteAuth(ctx, "quota-delete"); errDelete != nil {
		t.Fatalf("SoftDeleteAuth() error = %v", errDelete)
	}
	if _, errGet := repo.GetQuotaCredential(ctx, "quota-delete", now); !errors.Is(errGet, gorm.ErrRecordNotFound) {
		t.Fatalf("GetQuotaCredential(deleted) error = %v, want record not found", errGet)
	}
	db, errDB := repo.database()
	if errDB != nil {
		t.Fatalf("database() error = %v", errDB)
	}
	var snapshotCount, windowCount int64
	if errCount := db.Model(&QuotaSnapshotRecord{}).Where("credential_id = ?", "quota-delete").Count(&snapshotCount).Error; errCount != nil {
		t.Fatalf("count quota snapshots: %v", errCount)
	}
	if errCount := db.Model(&QuotaWindowRecord{}).Where("credential_id = ?", "quota-delete").Count(&windowCount).Error; errCount != nil {
		t.Fatalf("count quota windows: %v", errCount)
	}
	if snapshotCount != 0 || windowCount != 0 {
		t.Fatalf("deleted quota rows remain: snapshots=%d windows=%d", snapshotCount, windowCount)
	}
}

func TestAppendUsageMapsRuntimeAuthIndexToCredentialUUID(t *testing.T) {
	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	now := time.Date(2026, 7, 16, 13, 0, 0, 0, time.UTC)
	auth := &coreauth.Auth{ID: "quota-runtime-uuid", Index: "quota-runtime-uuid", Provider: "codex", Label: "Runtime Index", Status: coreauth.StatusActive, Metadata: map[string]any{"type": "codex"}, CreatedAt: now, UpdatedAt: now}
	record, errRecord := AuthToRecord(auth)
	if errRecord != nil {
		t.Fatalf("AuthToRecord() error = %v", errRecord)
	}
	record.Index = "runtime-index-1"
	db, errDB := repo.database()
	if errDB != nil {
		t.Fatalf("database() error = %v", errDB)
	}
	if errCreate := db.Create(record).Error; errCreate != nil {
		t.Fatalf("create compatibility auth: %v", errCreate)
	}

	payload := `{"timestamp":"2026-07-16T13:00:00Z","provider":"codex","auth_type":"oauth","auth_index":"runtime-index-1","response_headers":{"X-Codex-Primary-Used-Percent":["10"],"X-Codex-Primary-Window-Minutes":["300"],"X-Codex-Primary-Reset-After-Seconds":["60"]}}`
	if _, errAppend := repo.AppendUsageWithRuntime(ctx, payload, UsageRuntimeMetadata{}); errAppend != nil {
		t.Fatalf("AppendUsageWithRuntime() error = %v", errAppend)
	}

	item, errGet := repo.GetQuotaCredential(ctx, auth.ID, now)
	if errGet != nil {
		t.Fatalf("GetQuotaCredential() error = %v", errGet)
	}
	if item.AuthIndex == nil || *item.AuthIndex != "runtime-index-1" || item.QuotaStatus != "healthy" || len(item.Windows) != 1 {
		t.Fatalf("runtime-index quota item = %+v", item)
	}
	result, errList := repo.ListQuotaCredentials(ctx, QuotaListQuery{Limit: 50, Now: now})
	if errList != nil {
		t.Fatalf("ListQuotaCredentials() error = %v", errList)
	}
	if len(result.Items) != 1 || result.Items[0].CredentialID != auth.ID {
		t.Fatalf("quota list = %+v", result.Items)
	}
	var uuidCount, runtimeIndexCount int64
	if errCount := db.Model(&QuotaSnapshotRecord{}).Where("credential_id = ?", auth.ID).Count(&uuidCount).Error; errCount != nil {
		t.Fatalf("count UUID snapshots: %v", errCount)
	}
	if errCount := db.Model(&QuotaSnapshotRecord{}).Where("credential_id = ?", "runtime-index-1").Count(&runtimeIndexCount).Error; errCount != nil {
		t.Fatalf("count runtime-index snapshots: %v", errCount)
	}
	if uuidCount != 1 || runtimeIndexCount != 0 {
		t.Fatalf("snapshot counts uuid=%d runtime-index=%d", uuidCount, runtimeIndexCount)
	}
}

func TestAppendUsageBoundsLongQuotaWindowMetadata(t *testing.T) {
	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	now := time.Date(2026, 7, 16, 13, 15, 0, 0, time.UTC)
	seedQuotaSnapshotAuth(t, repo, "quota-long-window", "codex", "Long Window", map[string]any{"type": "codex"})
	group := strings.Repeat("A", 400)
	label := strings.Repeat("L", 400)
	payload := `{"timestamp":"2026-07-16T13:15:00Z","provider":"codex","auth_type":"oauth","auth_index":"quota-long-window","response_headers":{"X-Codex-` + group + `-Limit-Name":["` + label + `"],"X-Codex-` + group + `-Primary-Used-Percent":["10"],"X-Codex-` + group + `-Primary-Window-Minutes":["300"],"X-Codex-` + group + `-Primary-Reset-After-Seconds":["60"]}}`
	if _, errAppend := repo.AppendUsageWithRuntime(ctx, payload, UsageRuntimeMetadata{}); errAppend != nil {
		t.Fatalf("AppendUsageWithRuntime() error = %v", errAppend)
	}
	item, errGet := repo.GetQuotaCredential(ctx, "quota-long-window", now)
	if errGet != nil {
		t.Fatalf("GetQuotaCredential() error = %v", errGet)
	}
	if len(item.Windows) != 1 || len(item.Windows[0].ID) > quotaWindowTextMaxLength || item.Windows[0].Label == nil || len(*item.Windows[0].Label) > quotaWindowTextMaxLength {
		t.Fatalf("bounded quota window = %+v", item.Windows)
	}
}

func TestInvalidQuotaHeadersDoNotRollbackUsage(t *testing.T) {
	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	seedQuotaSnapshotAuth(t, repo, "quota-invalid-headers", "codex", "Invalid Headers", map[string]any{"type": "codex"})
	username := "quota-billing-user"
	credits := 10.0
	user, errCreateUser := repo.CreateUser(ctx, UserUpdate{Username: &username, Credits: &credits})
	if errCreateUser != nil {
		t.Fatalf("CreateUser() error = %v", errCreateUser)
	}
	clientKey := "quota-client-key"
	if _, errCreateKey := repo.CreateAPIKeyForUser(ctx, user.ID, APIKeyUserUpdate{APIKey: &clientKey}); errCreateKey != nil {
		t.Fatalf("CreateAPIKeyForUser() error = %v", errCreateKey)
	}
	if _, errCreatePrice := repo.CreateBillingModelPrice(ctx, BillingModelPriceUpdate{Provider: "codex", Model: "gpt-quota", RequestPrice: 2, Enabled: true}); errCreatePrice != nil {
		t.Fatalf("CreateBillingModelPrice() error = %v", errCreatePrice)
	}
	payload := `{"timestamp":"2026-07-16T13:20:00Z","provider":"codex","model":"gpt-quota","api_key":"quota-client-key","auth_type":"oauth","auth_index":"quota-invalid-headers","request_id":"req-invalid-quota","tokens":{"total_tokens":1},"response_headers":{"X-Codex-Foo-Bar-Limit-Name":["First"],"X-Codex-Foo-Bar-Primary-Used-Percent":["10"],"X-Codex-Foo-Bar-Primary-Window-Minutes":["300"],"X-Codex-Foo-Bar-Primary-Reset-After-Seconds":["60"],"X-Codex-Foo--Bar-Limit-Name":["Second"],"X-Codex-Foo--Bar-Primary-Used-Percent":["20"],"X-Codex-Foo--Bar-Primary-Window-Minutes":["300"],"X-Codex-Foo--Bar-Primary-Reset-After-Seconds":["60"]}}`
	if _, errAppend := repo.AppendUsageWithRuntime(ctx, payload, UsageRuntimeMetadata{}); errAppend != nil {
		t.Fatalf("AppendUsageWithRuntime() error = %v", errAppend)
	}
	db, errDB := repo.database()
	if errDB != nil {
		t.Fatalf("database() error = %v", errDB)
	}
	var usageCount, snapshotCount int64
	if errCount := db.Model(&UsageRecord{}).Where("request_id = ?", "req-invalid-quota").Count(&usageCount).Error; errCount != nil {
		t.Fatalf("count usage: %v", errCount)
	}
	if errCount := db.Model(&QuotaSnapshotRecord{}).Where("credential_id = ?", "quota-invalid-headers").Count(&snapshotCount).Error; errCount != nil {
		t.Fatalf("count snapshots: %v", errCount)
	}
	if usageCount != 1 || snapshotCount != 0 {
		t.Fatalf("usage/snapshot counts = %d/%d, want 1/0", usageCount, snapshotCount)
	}
	charges, errCharges := repo.ListBillingCharges(ctx, BillingChargeQuery{UserID: &user.ID, Limit: 10})
	if errCharges != nil {
		t.Fatalf("ListBillingCharges() error = %v", errCharges)
	}
	if len(charges.Records) != 1 || charges.Records[0].Amount != 2 {
		t.Fatalf("billing charges = %+v, want one amount=2 charge", charges.Records)
	}
}

func TestAppendUsageRejectsQuotaForMismatchedOrDeletedCredential(t *testing.T) {
	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	now := time.Date(2026, 7, 16, 13, 30, 0, 0, time.UTC)
	seedQuotaSnapshotAuth(t, repo, "quota-provider-mismatch", "claude", "Claude", map[string]any{"type": "claude"})
	seedQuotaSnapshotAuth(t, repo, "quota-deleted-observation", "codex", "Deleted", map[string]any{"type": "codex"})
	if errDelete := repo.SoftDeleteAuth(ctx, "quota-deleted-observation"); errDelete != nil {
		t.Fatalf("SoftDeleteAuth() error = %v", errDelete)
	}

	for _, credentialID := range []string{"quota-provider-mismatch", "quota-deleted-observation"} {
		payload := `{"timestamp":"2026-07-16T13:30:00Z","provider":"codex","auth_type":"oauth","auth_index":"` + credentialID + `","response_headers":{"X-Codex-Primary-Used-Percent":["10"],"X-Codex-Primary-Window-Minutes":["300"],"X-Codex-Primary-Reset-After-Seconds":["60"]}}`
		if _, errAppend := repo.AppendUsageWithRuntime(ctx, payload, UsageRuntimeMetadata{}); errAppend != nil {
			t.Fatalf("AppendUsageWithRuntime(%s) error = %v", credentialID, errAppend)
		}
	}

	db, errDB := repo.database()
	if errDB != nil {
		t.Fatalf("database() error = %v", errDB)
	}
	var count int64
	if errCount := db.Model(&QuotaSnapshotRecord{}).Where("credential_id IN ?", []string{"quota-provider-mismatch", "quota-deleted-observation"}).Count(&count).Error; errCount != nil {
		t.Fatalf("count quota snapshots: %v", errCount)
	}
	if count != 0 {
		t.Fatalf("mismatched/deleted credential snapshot count = %d, want 0", count)
	}
	item, errGet := repo.GetQuotaCredential(ctx, "quota-provider-mismatch", now)
	if errGet != nil {
		t.Fatalf("GetQuotaCredential() error = %v", errGet)
	}
	if item.Freshness != "never" || item.CollectionStatus != "idle" {
		t.Fatalf("mismatched credential quota item = %+v", item)
	}
}

func TestSanitizeUsagePayloadRejectsSecretLikeRequestIDs(t *testing.T) {
	payload := `{"timestamp":"2026-07-16T13:45:00Z","provider":"codex","upstream_request_id":"Bearer token","upstream":{"request_id":"sk-secret"},"response":{"request_id":"Authorization: token","id":"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.signaturevalue"}}`
	sanitized, errSanitize := SanitizeUsagePayloadSecrets(payload)
	if errSanitize != nil {
		t.Fatalf("SanitizeUsagePayloadSecrets() error = %v", errSanitize)
	}
	for _, path := range []string{"upstream_request_id", "upstream.request_id", "response.request_id", "response.id"} {
		if gjson.Get(sanitized, path).Exists() {
			t.Fatalf("sanitized payload retained %s: %s", path, sanitized)
		}
	}
	row := &usageObservabilityRecordRow{UpstreamRequestID: "Bearer historical-token"}
	if record := usageObservabilityRecordFromRow(row); record.UpstreamRequestID != "" {
		t.Fatalf("historical upstream request ID = %q, want empty", record.UpstreamRequestID)
	}
}

func TestQuotaUsageObservationClampsExcessiveFutureTimestamp(t *testing.T) {
	now := time.Date(2026, 7, 16, 14, 0, 0, 0, time.UTC)
	payload := `{"timestamp":"2099-01-01T00:00:00Z","provider":"codex","auth_index":"future-auth","quota_headers":{"X-Codex-Primary-Used-Percent":"10","X-Codex-Primary-Window-Minutes":"300","X-Codex-Primary-Reset-After-Seconds":"60"}}`
	input, ok := quotaSnapshotWriteFromUsagePayload(payload, UsageRuntimeMetadata{}, now)
	if !ok {
		t.Fatal("quotaSnapshotWriteFromUsagePayload() did not return a snapshot")
	}
	if input.ObservedAt == nil || !input.ObservedAt.Equal(now) {
		t.Fatalf("observed_at = %v, want %v", input.ObservedAt, now)
	}
	wantExpiry := now.Add(quotaHeaderSnapshotFreshness)
	if input.ExpiresAt == nil || !input.ExpiresAt.Equal(wantExpiry) || input.NextProbeAt == nil || !input.NextProbeAt.Equal(wantExpiry) {
		t.Fatalf("expiry/next probe = %v/%v, want %v", input.ExpiresAt, input.NextProbeAt, wantExpiry)
	}
}

func TestAppendUsageRecoversFromExistingFutureQuotaSnapshot(t *testing.T) {
	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	seedQuotaSnapshotAuth(t, repo, "future-recovery", "codex", "Future Recovery", map[string]any{"type": "codex"})
	now := time.Now().UTC().Truncate(time.Second)
	future := now.Add(365 * 24 * time.Hour)
	period := float64(5)
	remainingFuture := 0.01
	_, errFuture := repo.UpsertQuotaSnapshot(ctx, QuotaSnapshotWrite{
		CredentialID: "future-recovery", QuotaStatus: "exhausted", CollectionStatus: "success", Source: "response_header", ObservedAt: &future, ReplaceWindows: true,
		Windows: []QuotaWindow{{ID: "codex-primary", Scope: "account", Mode: "rolling", Status: "exhausted", Unit: "percentage", RemainingRatio: &remainingFuture, PeriodUnit: "hour", PeriodValue: &period, Source: "response_header", ObservedAt: future}},
	})
	if errFuture != nil {
		t.Fatalf("UpsertQuotaSnapshot(future) error = %v", errFuture)
	}
	payload := `{"timestamp":"` + now.Format(time.RFC3339) + `","provider":"codex","auth_type":"oauth","auth_index":"future-recovery","response_headers":{"X-Codex-Primary-Used-Percent":["10"],"X-Codex-Primary-Window-Minutes":["300"],"X-Codex-Primary-Reset-After-Seconds":["60"]}}`
	if _, errAppend := repo.AppendUsageWithRuntime(ctx, payload, UsageRuntimeMetadata{}); errAppend != nil {
		t.Fatalf("AppendUsageWithRuntime() error = %v", errAppend)
	}
	item, errGet := repo.GetQuotaCredential(ctx, "future-recovery", now)
	if errGet != nil {
		t.Fatalf("GetQuotaCredential() error = %v", errGet)
	}
	if item.ObservedAt == nil || item.ObservedAt.After(now.Add(quotaMaxFutureObservationSkew)) || item.ObservedAt.Equal(future) || item.NextProbeAt == nil || item.NextProbeAt.After(now.Add(time.Hour)) {
		t.Fatalf("future snapshot was not recovered: %+v", item)
	}
	if len(item.Windows) != 1 || item.Windows[0].ObservedAt.After(now.Add(quotaMaxFutureObservationSkew)) || item.Windows[0].Status != "healthy" {
		t.Fatalf("future windows were not replaced: %+v", item.Windows)
	}
}

func TestClaimQuotaProbeResetsExistingFutureSnapshot(t *testing.T) {
	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	seedQuotaSnapshotAuth(t, repo, "future-probe-recovery", "codex", "Future Probe", map[string]any{"type": "codex"})
	now := time.Date(2026, 7, 16, 14, 15, 0, 0, time.UTC)
	future := now.Add(365 * 24 * time.Hour)
	period := float64(5)
	_, errFuture := repo.UpsertQuotaSnapshot(ctx, QuotaSnapshotWrite{
		CredentialID: "future-probe-recovery", QuotaStatus: "healthy", CollectionStatus: "success", Source: "response_header", ObservedAt: &future, ReplaceWindows: true,
		Windows: []QuotaWindow{{ID: "future", Scope: "account", Mode: "rolling", Status: "healthy", Unit: "percentage", PeriodUnit: "hour", PeriodValue: &period, Source: "response_header", ObservedAt: future}},
	})
	if errFuture != nil {
		t.Fatalf("UpsertQuotaSnapshot(future) error = %v", errFuture)
	}
	claimed, errClaim := repo.ClaimQuotaProbe(ctx, "future-probe-recovery", "home-a", now, time.Minute)
	if errClaim != nil || !claimed {
		t.Fatalf("ClaimQuotaProbe() = %v, %v", claimed, errClaim)
	}
	item, errGet := repo.GetQuotaCredential(ctx, "future-probe-recovery", now)
	if errGet != nil {
		t.Fatalf("GetQuotaCredential() error = %v", errGet)
	}
	if item.ObservedAt != nil || item.Freshness != "never" || item.QuotaStatus != "unknown" || len(item.Windows) != 0 {
		t.Fatalf("future snapshot was not reset: %+v", item)
	}
	occurredAt := now.Add(time.Second)
	if errFail := repo.FailQuotaProbeAt(ctx, "future-probe-recovery", "home-a", QuotaCollectionError{Code: "UPSTREAM_UNAVAILABLE", Message: "failed", Retryable: true, OccurredAt: &occurredAt}, now.Add(5*time.Minute), now); errFail != nil {
		t.Fatalf("FailQuotaProbeAt() error = %v", errFail)
	}
	claimed, errClaim = repo.ClaimQuotaProbe(ctx, "future-probe-recovery", "home-b", now.Add(time.Minute), time.Minute)
	if errClaim != nil || claimed {
		t.Fatalf("backoff claim = %v, %v, want false", claimed, errClaim)
	}
}

func TestCreateQuotaSnapshotRecordRejectsOlderConflict(t *testing.T) {
	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	db, errDB := repo.database()
	if errDB != nil {
		t.Fatalf("database() error = %v", errDB)
	}
	newer := time.Date(2026, 7, 16, 14, 30, 0, 0, time.UTC)
	older := newer.Add(-time.Hour)
	newerInput := QuotaSnapshotWrite{CredentialID: "conflict-auth", QuotaStatus: "healthy", CollectionStatus: "success", Source: "response_header", ObservedAt: &newer, ParserVersion: 1, CollectorVersion: 1}
	newerRecord := quotaSnapshotRecordFromWrite(newerInput)
	if errCreate := db.Create(&newerRecord).Error; errCreate != nil {
		t.Fatalf("create newer snapshot: %v", errCreate)
	}
	olderInput := QuotaSnapshotWrite{CredentialID: "conflict-auth", QuotaStatus: "exhausted", CollectionStatus: "success", Source: "response_header", ObservedAt: &older, ParserVersion: 1, CollectorVersion: 1}
	olderRecord := quotaSnapshotRecordFromWrite(olderInput)
	accepted, errCreate := createQuotaSnapshotRecord(db, &olderRecord, olderInput)
	if errCreate != nil {
		t.Fatalf("createQuotaSnapshotRecord() error = %v", errCreate)
	}
	if accepted {
		t.Fatal("older conflicting snapshot was accepted")
	}
	var stored QuotaSnapshotRecord
	if errFirst := db.First(&stored, "credential_id = ?", "conflict-auth").Error; errFirst != nil {
		t.Fatalf("load stored snapshot: %v", errFirst)
	}
	if stored.ObservedAt == nil || !stored.ObservedAt.Equal(newer) || stored.QuotaStatus != "healthy" {
		t.Fatalf("stored snapshot = %+v", stored)
	}
}

func TestSafeQuotaRequestIDRejectsSecretLikeValues(t *testing.T) {
	for _, valid := range []string{"req-1234", "550e8400-e29b-41d4-a716-446655440000", "request_abc.def:123"} {
		if got := SafeQuotaRequestID(valid); got != valid {
			t.Fatalf("SafeQuotaRequestID(%q) = %q", valid, got)
		}
	}
	for _, invalid := range []string{
		"Bearer access-token-value",
		"Authorization: secret",
		"sk-proj-secret-value",
		"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.signaturevalue",
		strings.Repeat("x", 129),
		"request id with spaces",
	} {
		if got := SafeQuotaRequestID(invalid); got != "" {
			t.Fatalf("SafeQuotaRequestID(%q) = %q, want empty", invalid, got)
		}
	}
}

func TestQuotaCollectionErrorFiltersHistoricalSecretRequestID(t *testing.T) {
	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	now := time.Date(2026, 7, 16, 14, 40, 0, 0, time.UTC)
	seedQuotaSnapshotAuth(t, repo, "historical-error-request-id", "codex", "Historical Error", map[string]any{"type": "codex"})
	failure := QuotaCollectionError{Code: "UPSTREAM_UNAVAILABLE", Message: "failed", Retryable: true, OccurredAt: &now}
	_, errSnapshot := repo.UpsertQuotaSnapshot(ctx, QuotaSnapshotWrite{CredentialID: "historical-error-request-id", QuotaStatus: "error", CollectionStatus: "failed", Error: &failure})
	if errSnapshot != nil {
		t.Fatalf("UpsertQuotaSnapshot() error = %v", errSnapshot)
	}
	db, errDB := repo.database()
	if errDB != nil {
		t.Fatalf("database() error = %v", errDB)
	}
	if errUpdate := db.Model(&QuotaSnapshotRecord{}).Where("credential_id = ?", "historical-error-request-id").Update("error_request_id", "Bearer historical-token").Error; errUpdate != nil {
		t.Fatalf("update historical request ID: %v", errUpdate)
	}
	item, errGet := repo.GetQuotaCredential(ctx, "historical-error-request-id", now)
	if errGet != nil {
		t.Fatalf("GetQuotaCredential() error = %v", errGet)
	}
	if item.Error == nil || item.Error.RequestID != nil {
		t.Fatalf("historical collection error = %+v", item.Error)
	}
}

func TestQuotaCredentialTypeHonorsExplicitAuthKind(t *testing.T) {
	for _, test := range []struct {
		name string
		auth *coreauth.Auth
		want string
	}{
		{name: "attribute api key wins over oauth metadata", auth: &coreauth.Auth{Provider: "codex", Attributes: map[string]string{"auth_kind": "apikey", "api_key": "secret"}, Metadata: map[string]any{"type": "codex"}}, want: "provider_api_key"},
		{name: "metadata oauth wins over api key shape", auth: &coreauth.Auth{Provider: "codex", Attributes: map[string]string{"api_key": "secret"}, Metadata: map[string]any{"auth_kind": "oauth"}}, want: "oauth"},
		{name: "vertex remains explicit", auth: &coreauth.Auth{Provider: "vertex", Metadata: map[string]any{"auth_kind": "oauth"}}, want: "vertex"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := quotaCredentialType(test.auth); got != test.want {
				t.Fatalf("quotaCredentialType() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestQuotaSafeErrorMessagePreservesValidUTF8(t *testing.T) {
	message := quotaSafeErrorMessage(strings.Repeat("界", 200))
	if len(message) > 500 || !utf8.ValidString(message) {
		t.Fatalf("quotaSafeErrorMessage() produced %d bytes of invalid UTF-8", len(message))
	}
}

func TestAuthTypeChangeClearsAndRejectsOldQuotaObservation(t *testing.T) {
	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	now := time.Date(2026, 7, 16, 14, 45, 0, 0, time.UTC)
	seedQuotaSnapshotAuth(t, repo, "quota-type-change", "codex", "OAuth", map[string]any{"type": "codex"})
	_, errSnapshot := repo.UpsertQuotaSnapshot(ctx, QuotaSnapshotWrite{CredentialID: "quota-type-change", QuotaStatus: "healthy", CollectionStatus: "success", Source: "active_probe", ObservedAt: &now})
	if errSnapshot != nil {
		t.Fatalf("UpsertQuotaSnapshot() error = %v", errSnapshot)
	}
	apiKeyAuth := &coreauth.Auth{ID: "quota-type-change", Index: "quota-type-change", Provider: "codex", Label: "API Key", Status: coreauth.StatusActive, Attributes: map[string]string{"auth_kind": "apikey", "source": "config:codex", "api_key": "must-not-leak"}, Metadata: map[string]any{"type": "codex"}, CreatedAt: now, UpdatedAt: now.Add(time.Minute)}
	if _, errUpsert := repo.UpsertAuth(ctx, apiKeyAuth, "test"); errUpsert != nil {
		t.Fatalf("UpsertAuth(API key) error = %v", errUpsert)
	}
	payload := `{"timestamp":"2026-07-16T14:46:00Z","provider":"codex","auth_type":"oauth","auth_index":"quota-type-change","response_headers":{"X-Codex-Primary-Used-Percent":["10"],"X-Codex-Primary-Window-Minutes":["300"],"X-Codex-Primary-Reset-After-Seconds":["60"]}}`
	if _, errAppend := repo.AppendUsageWithRuntime(ctx, payload, UsageRuntimeMetadata{}); errAppend != nil {
		t.Fatalf("AppendUsageWithRuntime() error = %v", errAppend)
	}
	item, errGet := repo.GetQuotaCredential(ctx, apiKeyAuth.ID, now.Add(2*time.Minute))
	if errGet != nil {
		t.Fatalf("GetQuotaCredential() error = %v", errGet)
	}
	if item.CredentialType != "provider_api_key" || item.QuotaStatus != "unsupported" || item.Freshness != "never" || len(item.Windows) != 0 {
		t.Fatalf("type-change quota item = %+v", item)
	}
}

func TestAuthProviderChangeClearsQuotaSnapshot(t *testing.T) {
	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	now := time.Date(2026, 7, 16, 15, 0, 0, 0, time.UTC)
	seedQuotaSnapshotAuth(t, repo, "quota-provider-change", "codex", "Provider Change", map[string]any{"type": "codex"})
	period := float64(5)
	_, errSnapshot := repo.UpsertQuotaSnapshot(ctx, QuotaSnapshotWrite{
		CredentialID: "quota-provider-change", QuotaStatus: "healthy", CollectionStatus: "success", Source: "active_probe", ObservedAt: &now, ReplaceWindows: true,
		Windows: []QuotaWindow{{ID: "primary", Scope: "account", Mode: "rolling", Status: "healthy", Unit: "percentage", PeriodUnit: "hour", PeriodValue: &period, Source: "active_probe", ObservedAt: now}},
	})
	if errSnapshot != nil {
		t.Fatalf("UpsertQuotaSnapshot() error = %v", errSnapshot)
	}
	vertex := &coreauth.Auth{ID: "quota-provider-change", Index: "quota-provider-change", Provider: "vertex", Label: "Vertex", Status: coreauth.StatusActive, Metadata: map[string]any{"type": "vertex"}, CreatedAt: now, UpdatedAt: now.Add(time.Minute)}
	if _, errUpsert := repo.UpsertAuth(ctx, vertex, "test"); errUpsert != nil {
		t.Fatalf("UpsertAuth(vertex) error = %v", errUpsert)
	}
	item, errGet := repo.GetQuotaCredential(ctx, vertex.ID, now.Add(time.Minute))
	if errGet != nil {
		t.Fatalf("GetQuotaCredential() error = %v", errGet)
	}
	if item.Provider != "vertex" || item.CredentialType != "vertex" || item.QuotaStatus != "unsupported" || item.Freshness != "never" || len(item.Windows) != 0 {
		t.Fatalf("provider-change quota item = %+v", item)
	}
}

func seedQuotaSnapshotAuth(t *testing.T, repo *Repository, id string, provider string, label string, metadata map[string]any) {
	t.Helper()
	now := time.Date(2026, 7, 16, 0, 0, 0, 0, time.UTC)
	auth := &coreauth.Auth{ID: id, Index: id, Provider: provider, Label: label, Status: coreauth.StatusActive, Metadata: metadata, CreatedAt: now, UpdatedAt: now}
	if _, errUpsert := repo.UpsertAuth(context.Background(), auth, "test"); errUpsert != nil {
		t.Fatalf("UpsertAuth(%s) error = %v", id, errUpsert)
	}
}
