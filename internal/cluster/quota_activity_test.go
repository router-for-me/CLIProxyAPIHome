package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"gorm.io/gorm"
)

func TestClaimEligibleQuotaProbeRequiresRecentUnconsumedUsage(t *testing.T) {
	now := time.Date(2026, 8, 23, 10, 0, 30, 0, time.UTC)
	recent := -29*time.Minute - 59*time.Second
	boundary := -30 * time.Minute
	old := -30*time.Minute - time.Second
	tests := []struct {
		name           string
		activityOffset *time.Duration
		wantClaim      bool
	}{
		{name: "no usage"},
		{name: "usage one second inside boundary", activityOffset: &recent, wantClaim: true},
		{name: "exact activity boundary", activityOffset: &boundary},
		{name: "old usage", activityOffset: &old},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			repo, closeRepo := newBillingTestRepository(t, ctx)
			defer closeRepo()
			credentialID := "activity-" + quotaSlug(test.name)
			seedQuotaSnapshotAuth(t, repo, credentialID, "kimi", test.name, map[string]any{"type": "kimi"})
			if test.activityOffset != nil {
				recordQuotaUsageActivityAt(t, repo, credentialID, "kimi", "oauth", now.Add(*test.activityOffset))
			}

			claimed, errClaim := repo.ClaimEligibleQuotaProbe(ctx, credentialID, "home-a", now, time.Minute)
			if errClaim != nil || claimed != test.wantClaim {
				t.Fatalf("ClaimEligibleQuotaProbe() = %v, %v, want %v, nil", claimed, errClaim, test.wantClaim)
			}
			if !claimed {
				return
			}
			db, errDB := repo.database()
			if errDB != nil {
				t.Fatalf("database() error = %v", errDB)
			}
			var snapshot QuotaSnapshotRecord
			if errFirst := db.First(&snapshot, "credential_id = ?", credentialID).Error; errFirst != nil {
				t.Fatalf("load claimed snapshot: %v", errFirst)
			}
			wantActivityAt := now.Add(*test.activityOffset)
			if snapshot.LastActiveProbeAt != nil || snapshot.ProbeActivityAt == nil || !snapshot.ProbeActivityAt.Equal(wantActivityAt) {
				t.Fatalf("activity watermarks after claim = active_probe %v claimed %v, want nil/%v", snapshot.LastActiveProbeAt, snapshot.ProbeActivityAt, wantActivityAt)
			}
		})
	}
}

func TestClaimEligibleQuotaProbeRequiresNewUsageAndHonorsRetrySchedule(t *testing.T) {
	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	credentialID := "activity-retry"
	seedQuotaSnapshotAuth(t, repo, credentialID, "kimi", "Activity Retry", map[string]any{"type": "kimi"})
	now := time.Date(2026, 8, 23, 11, 0, 0, 0, time.UTC)
	recordQuotaUsageActivityAt(t, repo, credentialID, "kimi", "oauth", now.Add(-time.Minute))

	claimed, errClaim := repo.ClaimEligibleQuotaProbe(ctx, credentialID, "home-a", now, time.Minute)
	if errClaim != nil || !claimed {
		t.Fatalf("initial claim = %v, %v, want true, nil", claimed, errClaim)
	}
	retryAt := now.Add(5 * time.Minute)
	failureAt := now.Add(time.Second)
	if errFail := repo.FailQuotaProbeAt(ctx, credentialID, "home-a", QuotaCollectionError{
		Code: "UPSTREAM_UNAVAILABLE", Message: "failed", Retryable: true, OccurredAt: &failureAt,
	}, retryAt, now); errFail != nil {
		t.Fatalf("FailQuotaProbeAt() error = %v", errFail)
	}

	claimed, errClaim = repo.ClaimEligibleQuotaProbe(ctx, credentialID, "home-b", retryAt, time.Minute)
	if errClaim != nil || claimed {
		t.Fatalf("claim without new usage = %v, %v, want false, nil", claimed, errClaim)
	}
	recordQuotaUsageActivityAt(t, repo, credentialID, "kimi", "oauth", now.Add(time.Minute))
	claimed, errClaim = repo.ClaimEligibleQuotaProbe(ctx, credentialID, "home-b", now.Add(2*time.Minute), time.Minute)
	if errClaim != nil || claimed {
		t.Fatalf("claim before retry schedule = %v, %v, want false, nil", claimed, errClaim)
	}
	claimed, errClaim = repo.ClaimEligibleQuotaProbe(ctx, credentialID, "home-b", retryAt, time.Minute)
	if errClaim != nil || !claimed {
		t.Fatalf("claim after new usage and retry schedule = %v, %v, want true, nil", claimed, errClaim)
	}
}

func TestForceClaimEligibleQuotaProbeBypassesActivityButNotLease(t *testing.T) {
	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	credentialID := "force-inactive"
	seedQuotaSnapshotAuth(t, repo, credentialID, "kimi", "Force Inactive", map[string]any{"type": "kimi"})
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)

	claimed, errClaim := repo.ForceClaimEligibleQuotaProbe(ctx, credentialID, "home-a", now, time.Minute)
	if errClaim != nil || !claimed {
		t.Fatalf("forced inactive claim = %v, %v, want true, nil", claimed, errClaim)
	}
	claimed, errClaim = repo.ForceClaimEligibleQuotaProbe(ctx, credentialID, "home-b", now.Add(30*time.Second), time.Minute)
	if errClaim != nil || claimed {
		t.Fatalf("forced leased claim = %v, %v, want false, nil", claimed, errClaim)
	}
}

func TestEligibleQuotaProbeIgnoresCooldownButRejectsDisabledAndRefreshBlocked(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 10, 0, 0, time.UTC)
	tests := []struct {
		name          string
		auth          *coreauth.Auth
		wantScheduled bool
		wantForced    bool
	}{
		{
			name: "cooldown",
			auth: &coreauth.Auth{
				Provider: "kimi", Status: coreauth.StatusError, Unavailable: true,
				NextRetryAfter: now.Add(time.Hour), Metadata: map[string]any{"type": "kimi"},
			},
			wantScheduled: true,
			wantForced:    true,
		},
		{
			name: "disabled",
			auth: &coreauth.Auth{
				Provider: "kimi", Status: coreauth.StatusDisabled, Disabled: true,
				Metadata: map[string]any{"type": "kimi"},
			},
		},
		{
			name: "refresh blocked",
			auth: &coreauth.Auth{
				Provider: "kimi", Status: coreauth.StatusError, Unavailable: true,
				LastError:      &coreauth.Error{Code: "refresh_temporarily_unavailable"},
				NextRetryAfter: now.Add(time.Hour), Metadata: map[string]any{"type": "kimi"},
			},
		},
		{
			name: "refresh unsupported",
			auth: &coreauth.Auth{
				Provider: "kimi", Status: coreauth.StatusActive,
				LastRefreshError: &coreauth.Error{Code: "refresh_unsupported"},
				Metadata:         map[string]any{"type": "kimi"},
			},
			wantScheduled: true,
			wantForced:    true,
		},
		{
			name: "safe refresh backoff",
			auth: &coreauth.Auth{
				Provider: "kimi", Status: coreauth.StatusActive,
				NextRefreshAfter: now.Add(time.Hour),
				Metadata:         map[string]any{"type": "kimi"},
			},
			wantScheduled: true,
			wantForced:    true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			repo, closeRepo := newBillingTestRepository(t, ctx)
			defer closeRepo()
			credentialID := "force-" + strings.ReplaceAll(test.name, " ", "-")
			test.auth.ID = credentialID
			test.auth.Index = credentialID
			test.auth.Label = credentialID
			test.auth.CreatedAt = now
			test.auth.UpdatedAt = now
			if _, errUpsert := repo.UpsertAuth(ctx, test.auth, "test"); errUpsert != nil {
				t.Fatalf("UpsertAuth() error = %v", errUpsert)
			}
			recordQuotaUsageActivityAt(t, repo, credentialID, "kimi", "oauth", now.Add(-time.Minute))

			claimed, errClaim := repo.ClaimEligibleQuotaProbe(ctx, credentialID, "home-a", now, time.Minute)
			if errClaim != nil || claimed != test.wantScheduled {
				t.Fatalf("scheduled claim = %v, %v, want %v, nil", claimed, errClaim, test.wantScheduled)
			}
			forceAt := now
			if claimed {
				forceAt = now.Add(2 * time.Minute)
			}
			claimed, errClaim = repo.ForceClaimEligibleQuotaProbe(ctx, credentialID, "home-b", forceAt, time.Minute)
			if errClaim != nil || claimed != test.wantForced {
				t.Fatalf("forced claim = %v, %v, want %v, nil", claimed, errClaim, test.wantForced)
			}
		})
	}
}

func TestExpiredForcedQuotaProbeLeaseReturnsInactiveCredentialToIdle(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 15, 0, 0, time.UTC)
	oldActivity := -quotaProbeActivityWindow - time.Second
	tests := []struct {
		name           string
		activityOffset *time.Duration
	}{
		{name: "no usage"},
		{name: "old usage", activityOffset: &oldActivity},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			repo, closeRepo := newBillingTestRepository(t, ctx)
			defer closeRepo()
			credentialID := "force-inactive-takeover-" + quotaSlug(test.name)
			seedQuotaSnapshotAuth(t, repo, credentialID, "kimi", test.name, map[string]any{"type": "kimi"})
			if test.activityOffset != nil {
				recordQuotaUsageActivityAt(t, repo, credentialID, "kimi", "oauth", now.Add(*test.activityOffset))
			}

			claimed, errClaim := repo.ForceClaimEligibleQuotaProbe(ctx, credentialID, "home-a", now, time.Minute)
			if errClaim != nil || !claimed {
				t.Fatalf("forced inactive claim = %v, %v, want true, nil", claimed, errClaim)
			}

			type claimResult struct {
				claimed bool
				err     error
			}
			start := make(chan struct{})
			results := make(chan claimResult, 2)
			for _, owner := range []string{"home-b", "home-c"} {
				go func(owner string) {
					<-start
					claimedConcurrent, errConcurrent := repo.ClaimEligibleQuotaProbe(ctx, credentialID, owner, now.Add(2*time.Minute), time.Minute)
					results <- claimResult{claimed: claimedConcurrent, err: errConcurrent}
				}(owner)
			}
			close(start)
			claimResults := make([]claimResult, 0, 2)
			for range 2 {
				claimResults = append(claimResults, <-results)
			}
			for _, result := range claimResults {
				if result.err != nil || result.claimed {
					t.Fatalf("scheduled takeover of forced inactive probe = %v, %v, want false, nil", result.claimed, result.err)
				}
			}

			db, errDB := repo.database()
			if errDB != nil {
				t.Fatalf("database() error = %v", errDB)
			}
			var snapshot QuotaSnapshotRecord
			if errFirst := db.First(&snapshot, "credential_id = ?", credentialID).Error; errFirst != nil {
				t.Fatalf("load cleaned forced lease: %v", errFirst)
			}
			if snapshot.CollectionStatus != "idle" || snapshot.ProbeLeaseOwner != "" || snapshot.ProbeLeaseExpiresAt != nil || snapshot.ProbeActivityAt != nil {
				t.Fatalf("cleaned forced lease = status %q owner %q expires %v activity %v", snapshot.CollectionStatus, snapshot.ProbeLeaseOwner, snapshot.ProbeLeaseExpiresAt, snapshot.ProbeActivityAt)
			}
		})
	}
}

func TestExpiredQuotaProbeLeaseRetainsActivityForTakeover(t *testing.T) {
	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	credentialID := "activity-takeover"
	seedQuotaSnapshotAuth(t, repo, credentialID, "kimi", "Activity Takeover", map[string]any{"type": "kimi"})
	now := time.Date(2026, 8, 23, 12, 30, 0, 0, time.UTC)
	activityAt := now.Add(-quotaProbeActivityWindow).Add(time.Second)
	recordQuotaUsageActivityAt(t, repo, credentialID, "kimi", "oauth", activityAt)

	claimed, errClaim := repo.ClaimEligibleQuotaProbe(ctx, credentialID, "home-a", now, time.Minute)
	if errClaim != nil || !claimed {
		t.Fatalf("initial claim = %v, %v, want true, nil", claimed, errClaim)
	}
	claimed, errClaim = repo.ClaimEligibleQuotaProbe(ctx, credentialID, "home-b", now.Add(2*time.Minute), time.Minute)
	if errClaim != nil || !claimed {
		t.Fatalf("expired lease takeover = %v, %v, want true, nil", claimed, errClaim)
	}

	db, errDB := repo.database()
	if errDB != nil {
		t.Fatalf("database() error = %v", errDB)
	}
	var claimedSnapshot QuotaSnapshotRecord
	if errFirst := db.First(&claimedSnapshot, "credential_id = ?", credentialID).Error; errFirst != nil {
		t.Fatalf("load takeover claim: %v", errFirst)
	}
	if claimedSnapshot.ProbeLeaseOwner != "home-b" || claimedSnapshot.LastActiveProbeAt != nil || claimedSnapshot.ProbeActivityAt == nil || !claimedSnapshot.ProbeActivityAt.Equal(activityAt) {
		t.Fatalf("takeover watermarks = owner %q active %v claimed %v", claimedSnapshot.ProbeLeaseOwner, claimedSnapshot.LastActiveProbeAt, claimedSnapshot.ProbeActivityAt)
	}

	observedAt := now.Add(2 * time.Minute)
	expiresAt := observedAt.Add(30 * time.Minute)
	updated, errUpsert := repo.UpsertQuotaSnapshot(ctx, QuotaSnapshotWrite{
		CredentialID: credentialID, QuotaStatus: "healthy", CollectionStatus: "success", Source: "active_probe",
		ObservedAt: &observedAt, ExpiresAt: &expiresAt, NextProbeAt: &expiresAt,
		ExpectedProbeOwner: "home-b", ClearProbeLease: true,
	})
	if errUpsert != nil || !updated {
		t.Fatalf("takeover completion = %v, %v, want true, nil", updated, errUpsert)
	}
	var completed QuotaSnapshotRecord
	if errFirst := db.First(&completed, "credential_id = ?", credentialID).Error; errFirst != nil {
		t.Fatalf("load takeover completion: %v", errFirst)
	}
	if completed.LastActiveProbeAt == nil || !completed.LastActiveProbeAt.Equal(activityAt) || completed.ProbeActivityAt != nil || completed.ProbeLeaseOwner != "" {
		t.Fatalf("completed takeover watermarks = active %v claimed %v owner %q", completed.LastActiveProbeAt, completed.ProbeActivityAt, completed.ProbeLeaseOwner)
	}
}

func TestUsageWithoutQuotaHeadersMakesSupportedProvidersEligible(t *testing.T) {
	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	receivedAt := time.Date(2026, 8, 23, 13, 0, 45, 0, time.UTC)
	tests := []struct {
		id               string
		authProvider     string
		reportedProvider string
		metadataType     string
	}{
		{id: "activity-codex", authProvider: "codex", reportedProvider: "codex", metadataType: "codex"},
		{id: "activity-claude", authProvider: "anthropic", reportedProvider: "claude", metadataType: "claude"},
		{id: "activity-antigravity", authProvider: "antigravity", reportedProvider: "antigravity", metadataType: "antigravity"},
		{id: "activity-kimi", authProvider: "kimi", reportedProvider: "kimi", metadataType: "kimi"},
		{id: "activity-kimi-via-claude", authProvider: "kimi", reportedProvider: "claude", metadataType: "kimi"},
		{id: "activity-xai", authProvider: "xai", reportedProvider: "grok", metadataType: "xai"},
	}
	for _, test := range tests {
		seedQuotaSnapshotAuth(t, repo, test.id, test.authProvider, test.id, map[string]any{"type": test.metadataType})
		recordQuotaUsageActivityAt(t, repo, test.id, test.reportedProvider, "oauth", receivedAt)
	}

	db, errDB := repo.database()
	if errDB != nil {
		t.Fatalf("database() error = %v", errDB)
	}
	wantActivityAt := receivedAt
	for _, test := range tests {
		claimed, errClaim := repo.ClaimEligibleQuotaProbe(ctx, test.id, "home-"+test.id, receivedAt.Add(time.Minute), time.Minute)
		if errClaim != nil || !claimed {
			t.Fatalf("claim %s = %v, %v, want true, nil", test.id, claimed, errClaim)
		}
		var snapshot QuotaSnapshotRecord
		if errFirst := db.First(&snapshot, "credential_id = ?", test.id).Error; errFirst != nil {
			t.Fatalf("load %s activity: %v", test.id, errFirst)
		}
		if snapshot.ProbeActivityAt == nil || !snapshot.ProbeActivityAt.Equal(wantActivityAt) || snapshot.ObservedAt != nil || snapshot.CollectionStatus != "collecting" {
			t.Fatalf("%s activity snapshot = %+v, want claimed watermark %v and no quota observation", test.id, snapshot, wantActivityAt)
		}
	}
}

func TestAppendUsageActivityUsesHomeReceivedTime(t *testing.T) {
	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	credentialID := "received-time-activity"
	seedQuotaSnapshotAuth(t, repo, credentialID, "kimi", "Received Time", map[string]any{"type": "kimi"})
	receivedAt := time.Date(2026, 8, 23, 13, 0, 45, 123456000, time.UTC)
	payload := `{"timestamp":"2099-01-01T00:00:00Z","provider":"kimi","auth_type":"oauth","auth_index":"received-time-activity"}`
	record, errAppend := repo.AppendUsageWithRuntime(ctx, payload, UsageRuntimeMetadata{ReceivedAt: receivedAt})
	if errAppend != nil {
		t.Fatalf("AppendUsageWithRuntime() error = %v", errAppend)
	}

	claimed, errClaim := repo.ClaimEligibleQuotaProbe(ctx, credentialID, "home-a", record.CreatedAt.Add(time.Second), time.Minute)
	if errClaim != nil || !claimed {
		t.Fatalf("ClaimEligibleQuotaProbe() = %v, %v, want true, nil", claimed, errClaim)
	}
	db, errDB := repo.database()
	if errDB != nil {
		t.Fatalf("database() error = %v", errDB)
	}
	var snapshot QuotaSnapshotRecord
	if errFirst := db.First(&snapshot, "credential_id = ?", credentialID).Error; errFirst != nil {
		t.Fatalf("load activity snapshot: %v", errFirst)
	}
	want := receivedAt.UTC()
	if !record.CreatedAt.Equal(want) {
		t.Fatalf("usage created_at = %v, want trusted Home receive time %v", record.CreatedAt, want)
	}
	if snapshot.ProbeActivityAt == nil || !snapshot.ProbeActivityAt.Equal(want) {
		t.Fatalf("probe_activity_at = %v, want Home receive time %v", snapshot.ProbeActivityAt, want)
	}
	if snapshot.ProbeActivityAt.Year() == 2099 {
		t.Fatalf("probe_activity_at trusted the CPA payload timestamp: %v", snapshot.ProbeActivityAt)
	}
}

func TestAppendUsageCommitsCoreBeforeQuotaObservation(t *testing.T) {
	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	t.Cleanup(closeRepo)
	credentialID := "quota-commit-boundary"
	seedQuotaSnapshotAuth(t, repo, credentialID, "codex", "Commit Boundary", map[string]any{"type": "codex"})
	db, errDB := repo.database()
	if errDB != nil {
		t.Fatalf("database() error = %v", errDB)
	}
	sqlDB, errSQLDB := db.DB()
	if errSQLDB != nil {
		t.Fatalf("db.DB() error = %v", errSQLDB)
	}
	sqlDB.SetMaxOpenConns(2)
	sqlDB.SetMaxIdleConns(2)

	blocked := make(chan struct{})
	release := make(chan struct{})
	var blockOnce sync.Once
	var releaseOnce sync.Once
	releaseObservation := func() {
		releaseOnce.Do(func() { close(release) })
	}
	callbackName := "test:block-quota-observation-after-core-commit"
	if errRegister := db.Callback().Create().Before("gorm:create").Register(callbackName, func(tx *gorm.DB) {
		if tx.Statement.Table != (QuotaSnapshotRecord{}).TableName() {
			return
		}
		blockOnce.Do(func() {
			close(blocked)
			<-release
		})
	}); errRegister != nil {
		t.Fatalf("register quota observation callback: %v", errRegister)
	}
	t.Cleanup(func() {
		releaseObservation()
		if errRemove := db.Callback().Create().Remove(callbackName); errRemove != nil {
			t.Errorf("remove quota observation callback: %v", errRemove)
		}
	})

	payload := `{
		"timestamp":"2026-08-23T13:30:00Z",
		"provider":"codex",
		"auth_type":"oauth",
		"auth_index":"quota-commit-boundary",
		"request_id":"quota-commit-boundary",
		"response_headers":{
			"X-Codex-Active-Limit":"premium",
			"X-Codex-Primary-Used-Percent":"40",
			"X-Codex-Primary-Window-Minutes":"300",
			"X-Codex-Primary-Reset-After-Seconds":"600"
		}
	}`
	appendDone := make(chan error, 1)
	go func() {
		_, errAppend := repo.AppendUsageWithRuntime(ctx, payload, UsageRuntimeMetadata{})
		appendDone <- errAppend
	}()

	select {
	case <-blocked:
	case <-time.After(3 * time.Second):
		t.Fatal("quota observation did not reach the persistence barrier")
	}
	queryCtx, cancelQuery := context.WithTimeout(ctx, 2*time.Second)
	defer cancelQuery()
	var usageCount int64
	if errCount := db.WithContext(queryCtx).Model(&UsageRecord{}).Where("request_id = ?", "quota-commit-boundary").Count(&usageCount).Error; errCount != nil {
		t.Fatalf("count committed usage while quota observation is blocked: %v", errCount)
	}
	if usageCount != 1 {
		t.Fatalf("committed usage count while quota observation is blocked = %d, want 1", usageCount)
	}
	releaseObservation()
	select {
	case errAppend := <-appendDone:
		if errAppend != nil {
			t.Fatalf("AppendUsageWithRuntime() error = %v", errAppend)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("AppendUsageWithRuntime() did not finish after releasing quota observation")
	}
}

func TestQuotaActivityIdentityChangeRequiresNewUsage(t *testing.T) {
	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	credentialID := "activity-identity-change"
	now := time.Date(2026, 8, 23, 13, 45, 0, 0, time.UTC)
	seedQuotaSnapshotAuth(t, repo, credentialID, "kimi", "Kimi Identity", map[string]any{"type": "kimi"})
	recordQuotaUsageActivityAt(t, repo, credentialID, "kimi", "oauth", now)
	seedQuotaSnapshotAuth(t, repo, credentialID, "claude", "Claude Identity", map[string]any{"type": "claude"})
	_, mutatedRecord, changed, errMutate := repo.MutateAuth(ctx, credentialID, "test", func(auth *coreauth.Auth) bool {
		auth.Label = "Claude Identity Updated"
		return true
	})
	if errMutate != nil || !changed || mutatedRecord == nil || mutatedRecord.QuotaIdentityVersion <= 1 {
		t.Fatalf("non-identity mutation = changed %v record %#v error %v, want preserved generation above 1", changed, mutatedRecord, errMutate)
	}

	claimed, errClaim := repo.ClaimEligibleQuotaProbe(ctx, credentialID, "home-a", now.Add(time.Minute), time.Minute)
	if errClaim != nil || claimed {
		t.Fatalf("claim after identity change without new usage = %v, %v, want false, nil", claimed, errClaim)
	}
	newUsageAt := now.Add(2 * time.Minute)
	recordQuotaUsageActivityAt(t, repo, credentialID, "claude", "oauth", newUsageAt)
	claimed, errClaim = repo.ClaimEligibleQuotaProbe(ctx, credentialID, "home-a", newUsageAt.Add(time.Minute), time.Minute)
	if errClaim != nil || !claimed {
		t.Fatalf("claim after new identity usage = %v, %v, want true, nil", claimed, errClaim)
	}

	db, errDB := repo.database()
	if errDB != nil {
		t.Fatalf("database() error = %v", errDB)
	}
	var usages []UsageRecord
	if errFind := db.Where("auth_index = ?", credentialID).Order("id ASC").Find(&usages).Error; errFind != nil {
		t.Fatalf("load identity usage records: %v", errFind)
	}
	if len(usages) != 2 || usages[0].QuotaIdentityVersion <= 0 || usages[1].QuotaIdentityVersion <= usages[0].QuotaIdentityVersion {
		t.Fatalf("quota identity versions = %#v, want two increasing positive generations", usages)
	}
}

func TestQuotaActivityIdentityKeyFencesLegacyIdentityChanges(t *testing.T) {
	now := time.Date(2026, 8, 23, 13, 50, 0, 0, time.UTC)
	tests := []struct {
		name             string
		initialProvider  string
		initialMetadata  map[string]any
		nextProvider     string
		nextMetadata     map[string]any
		oldUsageProvider string
		newUsageProvider string
		oldAuthType      string
		newAuthType      string
	}{
		{
			name: "provider change", initialProvider: "codex", initialMetadata: map[string]any{"type": "codex"},
			nextProvider: "claude", nextMetadata: map[string]any{"type": "claude"},
			oldUsageProvider: "codex", newUsageProvider: "claude", oldAuthType: "oauth", newAuthType: "oauth",
		},
		{
			name: "credential type change", initialProvider: "codex", initialMetadata: map[string]any{"type": "codex"},
			nextProvider: "codex", nextMetadata: nil,
			oldUsageProvider: "codex", newUsageProvider: "codex", oldAuthType: "oauth", newAuthType: "file_auth",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			repo, closeRepo := newBillingTestRepository(t, ctx)
			defer closeRepo()
			credentialID := "legacy-identity-" + quotaSlug(test.name)
			seedQuotaSnapshotAuth(t, repo, credentialID, test.initialProvider, test.name, test.initialMetadata)
			recordQuotaUsageActivityAt(t, repo, credentialID, test.oldUsageProvider, test.oldAuthType, now)

			nextAuth := &coreauth.Auth{
				ID: credentialID, Index: credentialID, Provider: test.nextProvider, Label: test.name,
				Status: coreauth.StatusActive, Metadata: test.nextMetadata, CreatedAt: now, UpdatedAt: now.Add(time.Minute),
			}
			replaceQuotaIdentityWithoutGenerationForTest(t, repo, nextAuth)

			claimed, errClaim := repo.ClaimEligibleQuotaProbe(ctx, credentialID, "home-a", now.Add(time.Minute), time.Minute)
			if errClaim != nil || claimed {
				t.Fatalf("claim from legacy identity usage = %v, %v, want false, nil", claimed, errClaim)
			}
			newUsageAt := now.Add(2 * time.Minute)
			recordQuotaUsageActivityAt(t, repo, credentialID, test.newUsageProvider, test.newAuthType, newUsageAt)
			claimed, errClaim = repo.ClaimEligibleQuotaProbe(ctx, credentialID, "home-a", newUsageAt.Add(time.Minute), time.Minute)
			if errClaim != nil || !claimed {
				t.Fatalf("claim from current identity usage = %v, %v, want true, nil", claimed, errClaim)
			}
		})
	}
}

func TestQuotaSnapshotWritesPreserveActivityWatermarks(t *testing.T) {
	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	credentialID := "preserve-activity"
	seedQuotaSnapshotAuth(t, repo, credentialID, "codex", "Preserve Activity", map[string]any{"type": "codex"})
	now := time.Date(2026, 8, 23, 14, 0, 0, 0, time.UTC)
	recordQuotaUsageActivityAt(t, repo, credentialID, "codex", "oauth", now)
	claimed, errClaim := repo.ClaimEligibleQuotaProbe(ctx, credentialID, "home-a", now, time.Minute)
	if errClaim != nil || !claimed {
		t.Fatalf("ClaimEligibleQuotaProbe() = %v, %v", claimed, errClaim)
	}
	newUsageAt := now.Add(30 * time.Second)
	recordQuotaUsageActivityAt(t, repo, credentialID, "codex", "oauth", newUsageAt)
	activeObservedAt := now.Add(45 * time.Second)
	activeExpiresAt := now.Add(2 * time.Minute)
	updated, errActive := repo.UpsertQuotaSnapshot(ctx, QuotaSnapshotWrite{
		CredentialID: credentialID, QuotaStatus: "healthy", CollectionStatus: "success", Source: "active_probe",
		ObservedAt: &activeObservedAt, ExpiresAt: &activeExpiresAt, NextProbeAt: &activeExpiresAt,
		ParserVersion: codexQuotaSnapshotVersion, CollectorVersion: codexQuotaSnapshotVersion,
		ExpectedProbeOwner: "home-a", ClearProbeLease: true,
	})
	if errActive != nil || !updated {
		t.Fatalf("active snapshot write = %v, %v", updated, errActive)
	}
	passiveObservedAt := now.Add(time.Minute)
	passiveExpiresAt := passiveObservedAt.Add(30 * time.Minute)
	if _, errPassive := repo.UpsertQuotaSnapshot(ctx, QuotaSnapshotWrite{
		CredentialID: credentialID, QuotaStatus: "healthy", CollectionStatus: "partial", Source: "response_header",
		ObservedAt: &passiveObservedAt, ReceivedAt: &passiveObservedAt, ExpiresAt: &passiveExpiresAt,
		NextProbeAt: &passiveObservedAt, ParserVersion: quotaSnapshotSchemaVersion, CollectorVersion: quotaSnapshotSchemaVersion,
	}); errPassive != nil {
		t.Fatalf("passive snapshot write: %v", errPassive)
	}

	db, errDB := repo.database()
	if errDB != nil {
		t.Fatalf("database() error = %v", errDB)
	}
	var snapshot QuotaSnapshotRecord
	if errFirst := db.First(&snapshot, "credential_id = ?", credentialID).Error; errFirst != nil {
		t.Fatalf("load preserved activity: %v", errFirst)
	}
	if snapshot.LastActiveProbeAt == nil || !snapshot.LastActiveProbeAt.Equal(now) || snapshot.ProbeActivityAt != nil {
		t.Fatalf("preserved activity watermarks = active_probe %v claimed %v, want %v/nil", snapshot.LastActiveProbeAt, snapshot.ProbeActivityAt, now)
	}
	claimed, errClaim = repo.ClaimEligibleQuotaProbe(ctx, credentialID, "home-b", activeExpiresAt, time.Minute)
	if errClaim != nil || !claimed {
		t.Fatalf("claim usage received during probe = %v, %v, want true, nil", claimed, errClaim)
	}
	if errFirst := db.First(&snapshot, "credential_id = ?", credentialID).Error; errFirst != nil {
		t.Fatalf("load pending activity claim: %v", errFirst)
	}
	if snapshot.LastActiveProbeAt == nil || !snapshot.LastActiveProbeAt.Equal(now) || snapshot.ProbeActivityAt == nil || !snapshot.ProbeActivityAt.Equal(newUsageAt) {
		t.Fatalf("pending activity watermarks = active_probe %v claimed %v, want %v/%v", snapshot.LastActiveProbeAt, snapshot.ProbeActivityAt, now, newUsageAt)
	}
}

func TestPassiveQuotaSnapshotsPreservePendingProbeScheduleAcrossRepeatedWrites(t *testing.T) {
	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	credentialID := "preserve-probe-schedule"
	seedQuotaSnapshotAuth(t, repo, credentialID, "codex", "Preserve Probe Schedule", map[string]any{"type": "codex"})
	now := time.Date(2026, 8, 23, 14, 0, 0, 0, time.UTC)
	recordQuotaUsageActivityAt(t, repo, credentialID, "codex", "oauth", now)
	claimed, errClaim := repo.ClaimEligibleQuotaProbe(ctx, credentialID, "home-a", now, time.Minute)
	if errClaim != nil || !claimed {
		t.Fatalf("ClaimEligibleQuotaProbe() = %v, %v", claimed, errClaim)
	}

	activeObservedAt := now.Add(time.Second)
	nextProbeAt := now.Add(30 * time.Minute)
	windowSeconds := int64(5 * time.Hour / time.Second)
	period := float64(5)
	updated, errActive := repo.UpsertQuotaSnapshot(ctx, QuotaSnapshotWrite{
		CredentialID: credentialID, QuotaStatus: "healthy", CollectionStatus: "success", Source: "active_probe",
		ObservedAt: &activeObservedAt, ExpiresAt: &nextProbeAt, NextProbeAt: &nextProbeAt,
		ParserVersion: codexQuotaSnapshotVersion, CollectorVersion: codexQuotaSnapshotVersion,
		ExpectedProbeOwner: "home-a", ClearProbeLease: true, ReplaceWindows: true,
		Windows: []QuotaWindow{{
			ID: "codex-5-hour", Scope: "account", Mode: "rolling", Status: "healthy", Unit: "percentage",
			WindowSeconds: &windowSeconds, PeriodUnit: "hour", PeriodValue: &period, Source: "active_probe", ObservedAt: activeObservedAt,
		}},
	})
	if errActive != nil || !updated {
		t.Fatalf("active snapshot write = %v, %v", updated, errActive)
	}

	for index, passiveAt := range []time.Time{now.Add(time.Minute), now.Add(2 * time.Minute)} {
		payload := fmt.Sprintf(`{"timestamp":%q,"provider":"codex","auth_type":"oauth","auth_index":%q,"response_headers":{"X-Codex-Active-Limit":["premium"],"X-Codex-Primary-Used-Percent":["10"],"X-Codex-Primary-Window-Minutes":["300"],"X-Codex-Primary-Reset-After-Seconds":["600"]}}`, passiveAt.Format(time.RFC3339Nano), credentialID)
		if _, errAppend := repo.AppendUsageWithRuntime(ctx, payload, UsageRuntimeMetadata{ReceivedAt: passiveAt}); errAppend != nil {
			t.Fatalf("AppendUsageWithRuntime(passive %d) error = %v", index+1, errAppend)
		}
	}

	earlyClaimAt := now.Add(3 * time.Minute)
	claimed, errClaim = repo.ClaimEligibleQuotaProbe(ctx, credentialID, "home-b", earlyClaimAt, time.Minute)
	if errClaim != nil || claimed {
		t.Fatalf("early claim after repeated passive writes = %v, %v, want false, nil", claimed, errClaim)
	}

	db, errDB := repo.database()
	if errDB != nil {
		t.Fatalf("database() error = %v", errDB)
	}
	var snapshot QuotaSnapshotRecord
	if errFirst := db.First(&snapshot, "credential_id = ?", credentialID).Error; errFirst != nil {
		t.Fatalf("load preserved schedule: %v", errFirst)
	}
	if snapshot.NextProbeAt == nil || !snapshot.NextProbeAt.Equal(nextProbeAt) {
		t.Fatalf("next probe after repeated passive writes = %v, want %v", snapshot.NextProbeAt, nextProbeAt)
	}

	claimed, errClaim = repo.ClaimEligibleQuotaProbe(ctx, credentialID, "home-b", nextProbeAt, time.Minute)
	if errClaim != nil || !claimed {
		t.Fatalf("scheduled claim = %v, %v, want true, nil", claimed, errClaim)
	}
}

func TestQuotaActivityWatermarksAreExcludedFromPortableSnapshots(t *testing.T) {
	now := time.Date(2026, 8, 23, 15, 0, 0, 0, time.UTC)
	encoded, errMarshal := json.Marshal(QuotaSnapshotRecord{
		CredentialID: "runtime-activity", QuotaStatus: "unknown", CollectionStatus: "idle",
		LastActiveProbeAt: &now, ProbeActivityAt: &now,
	})
	if errMarshal != nil {
		t.Fatalf("json.Marshal() error = %v", errMarshal)
	}
	for _, field := range []string{"LastActiveProbeAt", "ProbeActivityAt", "last_active_probe_at", "probe_activity_at"} {
		if strings.Contains(string(encoded), field) {
			t.Fatalf("portable quota snapshot exposed runtime field %q: %s", field, encoded)
		}
	}
}

func recordQuotaUsageActivityAt(t testing.TB, repo *Repository, authIndex string, provider string, authType string, receivedAt time.Time) {
	t.Helper()
	db, errDB := repo.database()
	if errDB != nil {
		t.Fatalf("database() error = %v", errDB)
	}
	payload := fmt.Sprintf(`{"timestamp":%q,"provider":%q,"auth_type":%q,"auth_index":%q}`, receivedAt.UTC().Format(time.RFC3339Nano), provider, authType, authIndex)
	resolved, identityKey, found, errResolve := resolveQuotaUsageCredential(context.Background(), db, authIndex, authType)
	if errResolve != nil {
		t.Fatalf("resolve quota usage credential: %v", errResolve)
	}
	record := UsageRecord{
		Timestamp: receivedAt.UTC(), Provider: provider, AuthType: authType, AuthIndex: authIndex,
		PayloadJSON: JSONB(payload), CreatedAt: receivedAt.UTC(),
	}
	if found {
		record.QuotaCredentialID = resolved.UUID
		record.QuotaIdentityVersion = quotaCredentialIdentityVersion(resolved)
		record.QuotaIdentityKey = identityKey
	}
	if errCreate := db.Create(&record).Error; errCreate != nil {
		t.Fatalf("record quota usage activity: %v", errCreate)
	}
}

func replaceQuotaIdentityWithoutGenerationForTest(t testing.TB, repo *Repository, nextAuth *coreauth.Auth) {
	t.Helper()
	db, errDB := repo.database()
	if errDB != nil {
		t.Fatalf("database() error = %v", errDB)
	}
	var existing AuthRecord
	if errFirst := db.First(&existing, "uuid = ?", nextAuth.ID).Error; errFirst != nil {
		t.Fatalf("load existing auth: %v", errFirst)
	}
	next, errRecord := AuthToRecord(nextAuth)
	if errRecord != nil {
		t.Fatalf("AuthToRecord() error = %v", errRecord)
	}
	next.Version = existing.Version + 1
	next.QuotaIdentityVersion = existing.QuotaIdentityVersion
	next.CreatedAt = existing.CreatedAt
	if errUpdate := db.Select("*").Where("uuid = ?", existing.UUID).Updates(next).Error; errUpdate != nil {
		t.Fatalf("simulate legacy identity update: %v", errUpdate)
	}
}
