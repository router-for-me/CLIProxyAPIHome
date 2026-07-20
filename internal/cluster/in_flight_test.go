package cluster

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/home"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func newInFlightTestRepository(t *testing.T) *Repository {
	t.Helper()
	db, errOpen := OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "home.db"))
	if errOpen != nil {
		t.Fatalf("OpenSQLite() error = %v", errOpen)
	}
	if errMigrate := AutoMigrate(db); errMigrate != nil {
		t.Fatalf("AutoMigrate() error = %v", errMigrate)
	}
	sqlDB, errDB := db.DB()
	if errDB != nil {
		t.Fatalf("db.DB() error = %v", errDB)
	}
	t.Cleanup(func() {
		if errClose := sqlDB.Close(); errClose != nil {
			t.Errorf("close database: %v", errClose)
		}
	})
	return NewRepository(db)
}

func openInFlightPostgresTestDB(t *testing.T, dsn string, label string) *gorm.DB {
	t.Helper()
	db, errOpen := gorm.Open(postgres.Open(dsn), databaseGORMConfig())
	if errOpen != nil {
		t.Fatalf("open postgres %s: %v", label, errOpen)
	}
	sqlDB, errDB := db.DB()
	if errDB != nil {
		t.Fatalf("postgres %s DB(): %v", label, errDB)
	}
	if errPing := sqlDB.PingContext(context.Background()); errPing != nil {
		t.Fatalf("ping postgres %s: %v", label, errPing)
	}
	t.Cleanup(func() {
		if errClose := sqlDB.Close(); errClose != nil {
			t.Errorf("close postgres %s: %v", label, errClose)
		}
	})
	return db
}

func upsertInFlightTestAuth(t *testing.T, repo *Repository, authID string, totalLimit int, modelLimits map[string]int) {
	t.Helper()
	now := time.Now().UTC()
	auth := &coreauth.Auth{
		ID:                 authID,
		Index:              authID,
		Provider:           "codex",
		Status:             coreauth.StatusActive,
		CreatedAt:          now,
		UpdatedAt:          now,
		MaxInFlight:        totalLimit,
		MaxInFlightByModel: modelLimits,
		Metadata:           map[string]any{"type": "codex"},
	}
	if _, errUpsert := repo.UpsertAuth(context.Background(), auth, "create"); errUpsert != nil {
		t.Fatalf("UpsertAuth() error = %v", errUpsert)
	}
}

func reserveInFlightTestLease(repo *Repository, dispatchID string, credentialID string, model string, ttl time.Duration) (*home.InFlightLease, error) {
	return repo.ReserveInFlightLease(context.Background(), home.InFlightReserveInput{
		DispatchID:     dispatchID,
		RequestID:      "request-" + dispatchID,
		CredentialID:   credentialID,
		Provider:       "codex",
		RequestedModel: model,
		Model:          model,
		CPANodeID:      "node-a",
		TTL:            ttl,
	})
}

func TestInFlightLeaseEnforcesTotalAndReleasesIdempotently(t *testing.T) {
	repo := newInFlightTestRepository(t)
	upsertInFlightTestAuth(t, repo, "auth-total", 1, nil)

	first, errFirst := reserveInFlightTestLease(repo, "dispatch-1", "auth-total", "gpt-5", time.Minute)
	if errFirst != nil || first == nil {
		t.Fatalf("first reserve = %#v, %v", first, errFirst)
	}
	_, errSecond := reserveInFlightTestLease(repo, "dispatch-2", "auth-total", "gpt-5", time.Minute)
	var concurrencyErr *home.ConcurrencyExceededError
	if !errors.As(errSecond, &concurrencyErr) || concurrencyErr.Scope != "credential" {
		t.Fatalf("second reserve error = %v, want credential concurrency error", errSecond)
	}

	released, errRelease := repo.ReleaseInFlightLease(context.Background(), first.LeaseID, "node-a", "completed")
	if errRelease != nil || !released {
		t.Fatalf("ReleaseInFlightLease() = %v, %v", released, errRelease)
	}
	releasedAgain, errReleaseAgain := repo.ReleaseInFlightLease(context.Background(), first.LeaseID, "node-a", "completed")
	if errReleaseAgain != nil || releasedAgain {
		t.Fatalf("second release = %v, %v, want false, nil", releasedAgain, errReleaseAgain)
	}
	if _, errReserve := reserveInFlightTestLease(repo, "dispatch-2", "auth-total", "gpt-5", time.Minute); errReserve != nil {
		t.Fatalf("reserve after release error = %v", errReserve)
	}
}

func TestInFlightLeaseEnforcesModelAndCombinedLimits(t *testing.T) {
	repo := newInFlightTestRepository(t)
	upsertInFlightTestAuth(t, repo, "auth-model", 2, map[string]int{"gpt-5": 1})

	if _, errReserve := reserveInFlightTestLease(repo, "dispatch-a", "auth-model", "gpt-5(high)", time.Minute); errReserve != nil {
		t.Fatalf("reserve gpt-5(high) error = %v", errReserve)
	}
	_, errModel := reserveInFlightTestLease(repo, "dispatch-b", "auth-model", "gpt-5(low)", time.Minute)
	var modelErr *home.ConcurrencyExceededError
	if !errors.As(errModel, &modelErr) || modelErr.Scope != "model" {
		t.Fatalf("second gpt-5 variant error = %v, want model concurrency error", errModel)
	}
	detail, errDetail := repo.GetInFlightCredentialDetail(context.Background(), "auth-model", 0, 50)
	if errDetail != nil || len(detail.Requests) != 1 || detail.Requests[0].Model != "gpt-5" {
		t.Fatalf("canonical model detail = %+v, %v", detail, errDetail)
	}
	if _, errReserve := reserveInFlightTestLease(repo, "dispatch-c", "auth-model", "gpt-4.1", time.Minute); errReserve != nil {
		t.Fatalf("reserve gpt-4.1 error = %v", errReserve)
	}
	_, errTotal := reserveInFlightTestLease(repo, "dispatch-d", "auth-model", "gpt-4o", time.Minute)
	var totalErr *home.ConcurrencyExceededError
	if !errors.As(errTotal, &totalErr) || totalErr.Scope != "credential" {
		t.Fatalf("third total reserve error = %v, want credential concurrency error", errTotal)
	}
}

func TestInFlightLeaseDispatchIDIsIdempotent(t *testing.T) {
	repo := newInFlightTestRepository(t)
	upsertInFlightTestAuth(t, repo, "auth-idempotent", 1, nil)

	first, errFirst := reserveInFlightTestLease(repo, "dispatch-same", "auth-idempotent", "gpt-5", time.Minute)
	if errFirst != nil {
		t.Fatalf("first reserve error = %v", errFirst)
	}
	second, errSecond := reserveInFlightTestLease(repo, "dispatch-same", "auth-idempotent", "gpt-5", time.Minute)
	if errSecond != nil || second == nil || !second.Reused || second.LeaseID != first.LeaseID {
		t.Fatalf("second reserve = %#v, %v, want reused lease", second, errSecond)
	}

	detail, errDetail := repo.GetInFlightCredentialDetail(context.Background(), "auth-idempotent", 0, 50)
	if errDetail != nil {
		t.Fatalf("GetInFlightCredentialDetail() error = %v", errDetail)
	}
	if detail.Summary.InFlight != 1 || len(detail.Requests) != 1 {
		t.Fatalf("detail = %+v, want one active lease", detail)
	}
}

func TestInFlightLeaseDispatchIDRejectsDifferentRequestIdentity(t *testing.T) {
	repo := newInFlightTestRepository(t)
	upsertInFlightTestAuth(t, repo, "auth-replay-identity", 2, nil)

	first, errFirst := reserveInFlightTestLease(repo, "dispatch-replay-identity", "auth-replay-identity", "gpt-5", time.Minute)
	if errFirst != nil || first == nil {
		t.Fatalf("first reserve = %#v, %v", first, errFirst)
	}
	for _, input := range []home.InFlightReserveInput{
		{
			DispatchID:     first.DispatchID,
			RequestID:      "different-request",
			CredentialID:   first.CredentialID,
			Provider:       first.Provider,
			RequestedModel: first.RequestedModel,
			Model:          first.Model,
			CPANodeID:      first.CPANodeID,
			TTL:            time.Minute,
		},
		{
			DispatchID:     first.DispatchID,
			RequestID:      first.RequestID,
			CredentialID:   first.CredentialID,
			Provider:       first.Provider,
			RequestedModel: "gpt-4.1",
			Model:          "gpt-4.1",
			CPANodeID:      first.CPANodeID,
			TTL:            time.Minute,
		},
	} {
		_, errReplay := repo.ReserveInFlightLease(context.Background(), input)
		var replayErr *home.DispatchReplayError
		if !errors.As(errReplay, &replayErr) {
			t.Fatalf("ReserveInFlightLease(%+v) error = %v, want replay error", input, errReplay)
		}
	}

	detail, errDetail := repo.GetInFlightCredentialDetail(context.Background(), first.CredentialID, 0, 50)
	if errDetail != nil || detail.Summary.InFlight != 1 || len(detail.Requests) != 1 {
		t.Fatalf("detail after rejected replay = %+v, %v", detail, errDetail)
	}
}

func TestInFlightLeaseConcurrentDispatchIDIsIdempotent(t *testing.T) {
	repo := newInFlightTestRepository(t)
	upsertInFlightTestAuth(t, repo, "auth-idempotent-concurrent", 2, nil)

	const workers = 6
	var wg sync.WaitGroup
	var mu sync.Mutex
	leases := make([]*home.InFlightLease, 0, workers)
	errorsSeen := make([]error, 0)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lease, errReserve := reserveInFlightTestLease(repo, "dispatch-shared", "auth-idempotent-concurrent", "gpt-5", time.Minute)
			mu.Lock()
			defer mu.Unlock()
			if errReserve != nil {
				errorsSeen = append(errorsSeen, errReserve)
				return
			}
			leases = append(leases, lease)
		}()
	}
	wg.Wait()

	if len(errorsSeen) != 0 || len(leases) != workers {
		t.Fatalf("leases=%d errors=%v, want %d successful idempotent reserves", len(leases), errorsSeen, workers)
	}
	for _, lease := range leases {
		if lease == nil || lease.LeaseID != "dispatch-shared" {
			t.Fatalf("lease = %#v, want dispatch-shared", lease)
		}
	}
	detail, errDetail := repo.GetInFlightCredentialDetail(context.Background(), "auth-idempotent-concurrent", 0, 50)
	if errDetail != nil {
		t.Fatalf("GetInFlightCredentialDetail() error = %v", errDetail)
	}
	if detail.Summary.InFlight != 1 || len(detail.Requests) != 1 {
		t.Fatalf("detail = %+v, want one active lease", detail)
	}
}

func TestInFlightLeaseRenewAndExpiry(t *testing.T) {
	repo := newInFlightTestRepository(t)
	upsertInFlightTestAuth(t, repo, "auth-renew", 1, nil)

	lease, errReserve := reserveInFlightTestLease(repo, "dispatch-renew", "auth-renew", "gpt-5", time.Minute)
	if errReserve != nil || lease == nil {
		t.Fatalf("reserve = %#v, %v", lease, errReserve)
	}
	renewed, errRenew := repo.RenewInFlightLease(context.Background(), lease.LeaseID, "node-a", 2*time.Minute)
	if errRenew != nil || !renewed {
		t.Fatalf("RenewInFlightLease() = %v, %v", renewed, errRenew)
	}
	detail, errDetail := repo.GetInFlightCredentialDetail(context.Background(), "auth-renew", 0, 50)
	if errDetail != nil || len(detail.Requests) != 1 {
		t.Fatalf("detail after renew = %+v, %v", detail, errDetail)
	}
	if !detail.Requests[0].ExpiresAt.After(lease.ExpiresAt) || !detail.Requests[0].LastRenewedAt.After(lease.LastRenewedAt) {
		t.Fatalf("renewed lease = %+v, original = %+v", detail.Requests[0], lease)
	}

	db, errDB := repo.database()
	if errDB != nil {
		t.Fatalf("database() error = %v", errDB)
	}
	if errExpire := db.Model(&InFlightLeaseRecord{}).
		Where("lease_id = ?", lease.LeaseID).
		Update("expires_at", time.Now().UTC().Add(-time.Second)).Error; errExpire != nil {
		t.Fatalf("expire lease: %v", errExpire)
	}
	if _, errNext := reserveInFlightTestLease(repo, "dispatch-after-expiry", "auth-renew", "gpt-5", time.Minute); errNext != nil {
		t.Fatalf("reserve after expiry error = %v", errNext)
	}
}

func TestInFlightLeaseRenewDoesNotShortenExistingTTL(t *testing.T) {
	repo := newInFlightTestRepository(t)
	upsertInFlightTestAuth(t, repo, "auth-renew-ttl", 1, nil)

	lease, errReserve := reserveInFlightTestLease(repo, "dispatch-renew-ttl", "auth-renew-ttl", "gpt-5", 30*time.Minute)
	if errReserve != nil || lease == nil {
		t.Fatalf("reserve lease = %#v, %v", lease, errReserve)
	}
	renewed, errRenew := repo.RenewInFlightLease(context.Background(), lease.LeaseID, "node-a", time.Minute)
	if errRenew != nil || !renewed {
		t.Fatalf("RenewInFlightLease() = %v, %v", renewed, errRenew)
	}
	detail, errDetail := repo.GetInFlightCredentialDetail(context.Background(), lease.CredentialID, 0, 50)
	if errDetail != nil || len(detail.Requests) != 1 {
		t.Fatalf("detail after renew = %+v, %v", detail, errDetail)
	}
	renewedLease := detail.Requests[0]
	if remaining := renewedLease.ExpiresAt.Sub(renewedLease.LastRenewedAt); remaining < 29*time.Minute {
		t.Fatalf("renewed TTL = %v, want existing 30m cadence preserved", remaining)
	}
}

func TestInFlightLeaseRenewReportsFalseWhenExpiryWins(t *testing.T) {
	repo := newInFlightTestRepository(t)
	upsertInFlightTestAuth(t, repo, "auth-renew-expiry-race", 1, nil)

	lease, errReserve := reserveInFlightTestLease(repo, "dispatch-renew-expiry-race", "auth-renew-expiry-race", "gpt-5", time.Minute)
	if errReserve != nil || lease == nil {
		t.Fatalf("reserve lease = %#v, %v", lease, errReserve)
	}
	db, errDB := repo.database()
	if errDB != nil {
		t.Fatalf("database() error = %v", errDB)
	}
	var expireOnce sync.Once
	if errRegister := db.Callback().Update().Before("gorm:update").Register("test:expire_before_renew_update", func(tx *gorm.DB) {
		updates, ok := tx.Statement.Dest.(map[string]any)
		if !ok {
			return
		}
		if _, renewing := updates["last_renewed_at"]; !renewing {
			return
		}
		expireOnce.Do(func() {
			closedAt := time.Now().UTC()
			errExpire := tx.Exec(
				"UPDATE in_flight_lease SET status = ?, closed_at = ?, close_reason = ? WHERE id = ?",
				InFlightLeaseStatusExpired,
				closedAt,
				"ttl_expired",
				lease.ID,
			).Error
			if errExpire != nil {
				tx.AddError(errExpire)
			}
		})
	}); errRegister != nil {
		t.Fatalf("register expiry callback: %v", errRegister)
	}

	renewed, errRenew := repo.RenewInFlightLease(context.Background(), lease.LeaseID, "node-a", 2*time.Minute)
	if errRenew != nil || renewed {
		t.Fatalf("RenewInFlightLease() = %v, %v, want false, nil", renewed, errRenew)
	}
	record := InFlightLeaseRecord{}
	if errFirst := db.Where("lease_id = ?", lease.LeaseID).First(&record).Error; errFirst != nil {
		t.Fatalf("load lease after renewal race: %v", errFirst)
	}
	if record.Status != InFlightLeaseStatusExpired {
		t.Fatalf("lease status = %q, want %q", record.Status, InFlightLeaseStatusExpired)
	}
}

func TestInFlightLeaseExpiryDoesNotOverwriteConcurrentRenewal(t *testing.T) {
	repo := newInFlightTestRepository(t)
	upsertInFlightTestAuth(t, repo, "auth-expiry-renew-race", 1, nil)

	lease, errReserve := reserveInFlightTestLease(repo, "dispatch-expiry-renew-race", "auth-expiry-renew-race", "gpt-5", time.Minute)
	if errReserve != nil || lease == nil {
		t.Fatalf("reserve lease = %#v, %v", lease, errReserve)
	}
	db, errDB := repo.database()
	if errDB != nil {
		t.Fatalf("database() error = %v", errDB)
	}
	if errExpire := db.Model(&InFlightLeaseRecord{}).
		Where("id = ?", lease.ID).
		Update("expires_at", time.Now().UTC().Add(-time.Second)).Error; errExpire != nil {
		t.Fatalf("prepare expired lease: %v", errExpire)
	}
	var renewOnce sync.Once
	if errRegister := db.Callback().Update().Before("gorm:update").Register("test:renew_before_expiry_update", func(tx *gorm.DB) {
		updates, ok := tx.Statement.Dest.(map[string]any)
		if !ok {
			return
		}
		if _, expiring := updates["status"]; !expiring {
			return
		}
		renewOnce.Do(func() {
			lastRenewedAt := time.Now().UTC()
			errRenew := tx.Exec(
				"UPDATE in_flight_lease SET status = ?, last_renewed_at = ?, expires_at = ?, closed_at = NULL, close_reason = '' WHERE id = ?",
				InFlightLeaseStatusActive,
				lastRenewedAt,
				lastRenewedAt.Add(2*time.Minute),
				lease.ID,
			).Error
			if errRenew != nil {
				tx.AddError(errRenew)
			}
		})
	}); errRegister != nil {
		t.Fatalf("register renewal callback: %v", errRegister)
	}

	renewed, errRenew := repo.RenewInFlightLease(context.Background(), lease.LeaseID, "node-a", time.Minute)
	if errRenew != nil || !renewed {
		t.Fatalf("RenewInFlightLease() = %v, %v, want true, nil", renewed, errRenew)
	}
	record := InFlightLeaseRecord{}
	if errFirst := db.Where("lease_id = ?", lease.LeaseID).First(&record).Error; errFirst != nil {
		t.Fatalf("load lease after expiry race: %v", errFirst)
	}
	if record.Status != InFlightLeaseStatusActive || !record.ExpiresAt.After(time.Now().UTC()) {
		t.Fatalf("lease after expiry race = status:%q expires:%v, want active future lease", record.Status, record.ExpiresAt)
	}
}

func TestInFlightLeaseConcurrentRenewalDoesNotMoveExpiryBackward(t *testing.T) {
	repo := newInFlightTestRepository(t)
	upsertInFlightTestAuth(t, repo, "auth-concurrent-renew", 1, nil)

	lease, errReserve := reserveInFlightTestLease(repo, "dispatch-concurrent-renew", "auth-concurrent-renew", "gpt-5", time.Minute)
	if errReserve != nil || lease == nil {
		t.Fatalf("reserve lease = %#v, %v", lease, errReserve)
	}
	db, errDB := repo.database()
	if errDB != nil {
		t.Fatalf("database() error = %v", errDB)
	}
	futureRenewedAt := time.Now().UTC().Add(time.Minute)
	futureExpiresAt := futureRenewedAt.Add(2 * time.Minute)
	var renewOnce sync.Once
	if errRegister := db.Callback().Update().Before("gorm:update").Register("test:newer_concurrent_renewal", func(tx *gorm.DB) {
		updates, ok := tx.Statement.Dest.(map[string]any)
		if !ok {
			return
		}
		if _, renewing := updates["last_renewed_at"]; !renewing {
			return
		}
		renewOnce.Do(func() {
			errRenew := tx.Exec(
				"UPDATE in_flight_lease SET last_renewed_at = ?, expires_at = ? WHERE id = ?",
				futureRenewedAt,
				futureExpiresAt,
				lease.ID,
			).Error
			if errRenew != nil {
				tx.AddError(errRenew)
			}
		})
	}); errRegister != nil {
		t.Fatalf("register concurrent renewal callback: %v", errRegister)
	}

	renewed, errRenew := repo.RenewInFlightLease(context.Background(), lease.LeaseID, "node-a", time.Minute)
	if errRenew != nil || !renewed {
		t.Fatalf("RenewInFlightLease() = %v, %v, want true, nil", renewed, errRenew)
	}
	record := InFlightLeaseRecord{}
	if errFirst := db.Where("lease_id = ?", lease.LeaseID).First(&record).Error; errFirst != nil {
		t.Fatalf("load lease after concurrent renewal: %v", errFirst)
	}
	if !record.LastRenewedAt.Equal(futureRenewedAt) || !record.ExpiresAt.Equal(futureExpiresAt) {
		t.Fatalf(
			"lease renewal moved backward: last_renewed_at=%v expires_at=%v, want %v and %v",
			record.LastRenewedAt,
			record.ExpiresAt,
			futureRenewedAt,
			futureExpiresAt,
		)
	}
}

func TestUsageDoesNotReleaseLeaseBeforeExplicitTerminalSignal(t *testing.T) {
	repo := newInFlightTestRepository(t)
	upsertInFlightTestAuth(t, repo, "auth-usage-release", 1, nil)

	lease, errReserve := reserveInFlightTestLease(repo, "dispatch-usage-lease", "auth-usage-release", "gpt-5", time.Minute)
	if errReserve != nil || lease == nil {
		t.Fatalf("reserve lease = %#v, %v", lease, errReserve)
	}
	payload := `{"timestamp":"` + time.Now().UTC().Format(time.RFC3339Nano) + `","provider":"codex","model":"gpt-5","auth_index":"auth-usage-release","request_id":"` + lease.RequestID + `","lease_id":"` + lease.LeaseID + `","cpa_node_id":"node-a","tokens":{"total_tokens":1}}`
	if _, errAppend := repo.AppendUsageWithRuntime(context.Background(), payload, UsageRuntimeMetadata{}); errAppend != nil {
		t.Fatalf("AppendUsageWithRuntime() error = %v", errAppend)
	}
	detail, errDetail := repo.GetInFlightCredentialDetail(context.Background(), "auth-usage-release", 0, 50)
	if errDetail != nil {
		t.Fatalf("GetInFlightCredentialDetail() error = %v", errDetail)
	}
	if detail.Summary.InFlight != 1 || len(detail.Requests) != 1 {
		t.Fatalf("detail after attempt usage = %+v, want lease to remain active", detail)
	}
	if _, errRelease := repo.ReleaseInFlightLease(context.Background(), lease.LeaseID, "node-a", "completed"); errRelease != nil {
		t.Fatalf("ReleaseInFlightLease() error = %v", errRelease)
	}
	detail, errDetail = repo.GetInFlightCredentialDetail(context.Background(), "auth-usage-release", 0, 50)
	if errDetail != nil || detail.Summary.InFlight != 0 || len(detail.Requests) != 0 {
		t.Fatalf("detail after explicit release = %+v, %v", detail, errDetail)
	}
}

func TestInFlightLeaseRejectsDifferentOrMissingNodeOwner(t *testing.T) {
	repo := newInFlightTestRepository(t)
	upsertInFlightTestAuth(t, repo, "auth-owner", 1, nil)

	lease, errReserve := reserveInFlightTestLease(repo, "dispatch-owner", "auth-owner", "gpt-5", time.Minute)
	if errReserve != nil || lease == nil {
		t.Fatalf("reserve = %#v, %v", lease, errReserve)
	}
	for _, nodeID := range []string{"", "node-b"} {
		_, errReplay := repo.ReserveInFlightLease(context.Background(), home.InFlightReserveInput{
			DispatchID:   lease.DispatchID,
			RequestID:    lease.RequestID,
			CredentialID: lease.CredentialID,
			Provider:     lease.Provider,
			Model:        lease.Model,
			CPANodeID:    nodeID,
			TTL:          time.Minute,
		})
		var replayErr *home.DispatchReplayError
		if !errors.As(errReplay, &replayErr) {
			t.Fatalf("ReserveInFlightLease(node=%q) error = %v, want replay error", nodeID, errReplay)
		}
		if _, errRenew := repo.RenewInFlightLease(context.Background(), lease.LeaseID, nodeID, time.Minute); errRenew == nil {
			t.Fatalf("RenewInFlightLease(node=%q) error = nil", nodeID)
		}
		if _, errRelease := repo.ReleaseInFlightLease(context.Background(), lease.LeaseID, nodeID, "completed"); errRelease == nil {
			t.Fatalf("ReleaseInFlightLease(node=%q) error = nil", nodeID)
		}
	}
}

func TestInFlightLeaseConcurrentReserveDoesNotOverIssue(t *testing.T) {
	repo := newInFlightTestRepository(t)
	upsertInFlightTestAuth(t, repo, "auth-concurrent", 1, nil)

	const workers = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	successes := 0
	concurrencyFailures := 0
	otherErrors := make([]error, 0)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			_, errReserve := reserveInFlightTestLease(repo, "dispatch-concurrent-"+string(rune('a'+index)), "auth-concurrent", "gpt-5", time.Minute)
			mu.Lock()
			defer mu.Unlock()
			if errReserve == nil {
				successes++
				return
			}
			var concurrencyErr *home.ConcurrencyExceededError
			if errors.As(errReserve, &concurrencyErr) {
				concurrencyFailures++
				return
			}
			otherErrors = append(otherErrors, errReserve)
		}(i)
	}
	wg.Wait()
	if successes != 1 || concurrencyFailures != workers-1 || len(otherErrors) != 0 {
		t.Fatalf("successes=%d concurrency_failures=%d other_errors=%v", successes, concurrencyFailures, otherErrors)
	}
}

func TestInFlightLeaseConcurrentReserveAcrossRepositoriesDoesNotOverIssue(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "home.db")
	dbA, errOpen := OpenSQLite(ctx, dbPath)
	if errOpen != nil {
		t.Fatalf("OpenSQLite(A) error = %v", errOpen)
	}
	if errMigrate := AutoMigrate(dbA); errMigrate != nil {
		t.Fatalf("AutoMigrate() error = %v", errMigrate)
	}
	dbB, errOpen := OpenSQLite(ctx, dbPath)
	if errOpen != nil {
		t.Fatalf("OpenSQLite(B) error = %v", errOpen)
	}
	for label, db := range map[string]interface{ DB() (*sql.DB, error) }{"A": dbA, "B": dbB} {
		sqlDB, errDB := db.DB()
		if errDB != nil {
			t.Fatalf("db.%s.DB() error = %v", label, errDB)
		}
		t.Cleanup(func() {
			if errClose := sqlDB.Close(); errClose != nil {
				t.Errorf("close database %s: %v", label, errClose)
			}
		})
	}
	repoA := NewRepository(dbA)
	repoB := NewRepository(dbB)
	upsertInFlightTestAuth(t, repoA, "auth-multi-home", 1, nil)

	start := make(chan struct{})
	results := make(chan error, 2)
	for index, repo := range []*Repository{repoA, repoB} {
		go func(index int, repo *Repository) {
			<-start
			_, errReserve := reserveInFlightTestLease(repo, fmt.Sprintf("dispatch-multi-home-%d", index), "auth-multi-home", "gpt-5", time.Minute)
			results <- errReserve
		}(index, repo)
	}
	close(start)

	successes := 0
	concurrencyFailures := 0
	otherErrors := make([]error, 0)
	for range 2 {
		errReserve := <-results
		if errReserve == nil {
			successes++
			continue
		}
		var concurrencyErr *home.ConcurrencyExceededError
		if errors.As(errReserve, &concurrencyErr) {
			concurrencyFailures++
			continue
		}
		otherErrors = append(otherErrors, errReserve)
	}
	if successes != 1 || concurrencyFailures != 1 || len(otherErrors) != 0 {
		t.Fatalf("successes=%d concurrency_failures=%d other_errors=%v", successes, concurrencyFailures, otherErrors)
	}
}

func TestInFlightLeaseConcurrentReservePostgresDoesNotOverIssue(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("CLIPROXY_HOME_TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("CLIPROXY_HOME_TEST_POSTGRES_DSN is not set")
	}

	dbA := openInFlightPostgresTestDB(t, dsn, "A")
	if errMigrate := AutoMigrate(dbA); errMigrate != nil {
		t.Fatalf("AutoMigrate() error = %v", errMigrate)
	}
	if errTruncate := dbA.Exec("TRUNCATE TABLE in_flight_lease, auth RESTART IDENTITY CASCADE").Error; errTruncate != nil {
		t.Fatalf("truncate postgres test tables: %v", errTruncate)
	}
	dbB := openInFlightPostgresTestDB(t, dsn, "B")
	repoA := NewRepository(dbA)
	repoB := NewRepository(dbB)
	upsertInFlightTestAuth(t, repoA, "auth-postgres-multi-home", 1, nil)

	start := make(chan struct{})
	results := make(chan error, 2)
	for index, repo := range []*Repository{repoA, repoB} {
		go func(index int, repo *Repository) {
			<-start
			_, errReserve := reserveInFlightTestLease(repo, fmt.Sprintf("dispatch-postgres-multi-home-%d", index), "auth-postgres-multi-home", "gpt-5", time.Minute)
			results <- errReserve
		}(index, repo)
	}
	close(start)

	successes := 0
	concurrencyFailures := 0
	for range 2 {
		errReserve := <-results
		if errReserve == nil {
			successes++
			continue
		}
		var concurrencyErr *home.ConcurrencyExceededError
		if errors.As(errReserve, &concurrencyErr) {
			concurrencyFailures++
			continue
		}
		t.Fatalf("unexpected reserve error: %v", errReserve)
	}
	if successes != 1 || concurrencyFailures != 1 {
		t.Fatalf("successes=%d concurrency_failures=%d, want 1 and 1", successes, concurrencyFailures)
	}
}

func TestInFlightLeaseUnlimitedPostgresReservationsDoNotSerialize(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("CLIPROXY_HOME_TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("CLIPROXY_HOME_TEST_POSTGRES_DSN is not set")
	}

	dbA := openInFlightPostgresTestDB(t, dsn, "unlimited-A")
	if errMigrate := AutoMigrate(dbA); errMigrate != nil {
		t.Fatalf("AutoMigrate() error = %v", errMigrate)
	}
	if errTruncate := dbA.Exec("TRUNCATE TABLE in_flight_lease, auth RESTART IDENTITY CASCADE").Error; errTruncate != nil {
		t.Fatalf("truncate postgres test tables: %v", errTruncate)
	}
	dbB := openInFlightPostgresTestDB(t, dsn, "unlimited-B")
	repoA := NewRepository(dbA)
	repoB := NewRepository(dbB)
	upsertInFlightTestAuth(t, repoA, "auth-postgres-unlimited", 0, nil)

	createReached := make(chan struct{}, 2)
	releaseCreate := make(chan struct{})
	registerBarrier := func(db *gorm.DB, name string) {
		t.Helper()
		errRegister := db.Callback().Create().Before("gorm:create").Register(name, func(tx *gorm.DB) {
			if tx.Statement == nil || tx.Statement.Schema == nil || tx.Statement.Schema.Table != (InFlightLeaseRecord{}).TableName() {
				return
			}
			createReached <- struct{}{}
			<-releaseCreate
		})
		if errRegister != nil {
			t.Fatalf("register create barrier %s: %v", name, errRegister)
		}
	}
	registerBarrier(dbA, "test:in_flight_unlimited_barrier_a")
	registerBarrier(dbB, "test:in_flight_unlimited_barrier_b")

	start := make(chan struct{})
	results := make(chan error, 2)
	for index, repo := range []*Repository{repoA, repoB} {
		go func(index int, repo *Repository) {
			<-start
			_, errReserve := reserveInFlightTestLease(repo, fmt.Sprintf("dispatch-postgres-unlimited-%d", index), "auth-postgres-unlimited", "gpt-5", time.Minute)
			results <- errReserve
		}(index, repo)
	}
	close(start)

	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	reached := 0
	timedOut := false
	for reached < 2 {
		select {
		case <-createReached:
			reached++
		case <-timer.C:
			timedOut = true
		}
		if timedOut {
			break
		}
	}
	close(releaseCreate)

	for range 2 {
		if errReserve := <-results; errReserve != nil {
			t.Fatalf("unlimited reserve error: %v", errReserve)
		}
	}
	if timedOut {
		t.Fatalf("only %d unlimited reservation(s) reached create concurrently; credential dispatches were serialized", reached)
	}
}
