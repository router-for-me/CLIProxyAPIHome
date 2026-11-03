package cluster

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"gorm.io/gorm"
)

func TestCoordinatorMasterCleansExpiredKV(t *testing.T) {
	repo := newCredentialFoundationTestRepository(t)
	ctx, cancelCtx := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancelCtx()
	coordinator := NewCoordinator(repo, NodeIdentity{IP: "127.0.0.1", Port: 8327}, CoordinatorOptions{HeartbeatInterval: time.Hour, HeartbeatTimeout: time.Hour})
	if errInitialize := coordinator.Initialize(ctx); errInitialize != nil {
		t.Fatal(errInitialize)
	}
	defer coordinator.setMaster(false)
	past := time.Now().UTC().Add(-time.Hour)
	if errCreate := repo.db.Create(&KVRecord{Key: "expired", Value: []byte("old"), ExpiresAt: &past}).Error; errCreate != nil {
		t.Fatal(errCreate)
	}
	purged := make(chan struct{}, 1)
	if errRegister := repo.db.Callback().Delete().After("gorm:commit_or_rollback_transaction").Register("test_kv_cleanup", func(tx *gorm.DB) {
		if tx.Statement.Table == "kv_store" && tx.Error == nil {
			select {
			case purged <- struct{}{}:
			default:
			}
		}
	}); errRegister != nil {
		t.Fatal(errRegister)
	}
	done := make(chan error, 1)
	go func() { done <- coordinator.Start(ctx) }()
	defer func() {
		cancelCtx()
		if errStart := <-done; errStart != nil {
			t.Errorf("coordinator stopped with error: %v", errStart)
		}
	}()
	select {
	case <-purged:
	case <-ctx.Done():
		t.Fatal("master did not start KV cleanup")
	}
	var count int64
	if errCount := repo.db.Model(&KVRecord{}).Where("key = ?", "expired").Count(&count).Error; errCount != nil || count != 0 {
		t.Fatalf("expired KV rows=%d err=%v", count, errCount)
	}
}

func TestSessionCleanupDrainsFullBatchesBeforeIdle(t *testing.T) {
	calls := 0
	runExpiredRecordsCleanup(t.Context(), "session affinity", func(ctx context.Context, _ time.Time, limit int) (int64, error) {
		if _, ok := ctx.Deadline(); !ok || limit != 1000 {
			t.Fatalf("unbounded batch: deadline=%t limit=%d", ok, limit)
		}
		calls++
		if calls <= 3 {
			return 1000, nil
		}
		return 7, nil
	}, func() bool { return true }, time.Now, func(delay time.Duration) bool {
		if calls != 4 || delay != expiredCleanupIdle {
			t.Fatalf("calls=%d delay=%s", calls, delay)
		}
		return false
	})
}

func TestSessionCleanupYieldsOnBudgetAndResumes(t *testing.T) {
	now := time.Unix(1000, 0)
	calls, waits := 0, 0
	runExpiredRecordsCleanup(t.Context(), "session affinity", func(context.Context, time.Time, int) (int64, error) {
		calls++
		now = now.Add(expiredCleanupBudget / 2)
		if calls <= 2 {
			return 1000, nil
		}
		return 0, nil
	}, func() bool { return true }, func() time.Time { return now }, func(delay time.Duration) bool {
		waits++
		if waits == 1 {
			if calls != 2 || delay != expiredCleanupYield {
				t.Fatalf("budget exceeded: calls=%d delay=%s", calls, delay)
			}
			return true
		}
		if calls != 3 || delay != expiredCleanupIdle {
			t.Fatalf("backlog not resumed: calls=%d delay=%s", calls, delay)
		}
		return false
	})
}

func TestSessionCleanupErrorBackoffIsBoundedAndResets(t *testing.T) {
	failure := errors.New("database unavailable")
	calls := 0
	var delays []time.Duration
	runExpiredRecordsCleanup(t.Context(), "session affinity", func(context.Context, time.Time, int) (int64, error) {
		calls++
		if calls == 9 {
			return 0, nil
		}
		return 0, failure
	}, func() bool { return true }, time.Now, func(delay time.Duration) bool {
		delays = append(delays, delay)
		return calls < 10
	})
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 32 * time.Second, time.Minute, time.Minute, expiredCleanupIdle, time.Second}
	if !reflect.DeepEqual(delays, want) {
		t.Fatalf("backoff=%v want=%v", delays, want)
	}
}

func TestSessionCleanupCancellation(t *testing.T) {
	ctx, cancelCtx := context.WithCancel(t.Context())
	calls := 0
	runExpiredRecordsCleanup(ctx, "session affinity", func(ctx context.Context, _ time.Time, _ int) (int64, error) {
		calls++
		cancelCtx()
		<-ctx.Done()
		return 0, ctx.Err()
	}, func() bool { return true }, time.Now, func(time.Duration) bool {
		t.Fatal("cleanup waited after cancellation")
		return false
	})
	if calls != 1 {
		t.Fatalf("calls=%d", calls)
	}
	// The real timer wait must also be interruptible during standby/backoff.
	coordinator := NewCoordinator(&Repository{}, NodeIdentity{}, CoordinatorOptions{})
	coordinator.runExpiredRecordsCleanupLoop(ctx, "session affinity", coordinator.repo.PurgeExpiredSessionBindings)
}

func TestExpiredCleanupMasterTakeoverAndDemotion(t *testing.T) {
	for _, name := range []string{"session affinity", "kv store", "cpa latency summaries", "cpa latency confirmations"} {
		t.Run(name, func(t *testing.T) {
			coordinator := NewCoordinator(&Repository{}, NodeIdentity{}, CoordinatorOptions{HeartbeatTimeout: time.Hour})
			defer coordinator.setMaster(false)
			calls, waits := 0, 0
			runExpiredRecordsCleanup(t.Context(), name, func(context.Context, time.Time, int) (int64, error) {
				calls++
				if calls == 1 {
					coordinator.setMaster(false)
					return 1000, nil
				}
				return 0, nil
			}, coordinator.IsMaster, time.Now, func(delay time.Duration) bool {
				waits++
				switch waits {
				case 1:
					if calls != 0 || delay != expiredCleanupRetry {
						t.Fatalf("standby purged records: calls=%d delay=%s", calls, delay)
					}
					coordinator.setMaster(true)
				case 2:
					if calls != 1 {
						t.Fatalf("cleanup continued after demotion: calls=%d", calls)
					}
					coordinator.setMaster(true)
				case 3:
					if calls != 2 || delay != expiredCleanupIdle {
						t.Fatalf("takeover did not drain backlog: calls=%d delay=%s", calls, delay)
					}
					return false
				}
				return true
			})
		})
	}
}

func TestSessionCleanupConcurrentRenewal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "home.db")
	reader := openRefreshTestRepositoryAt(t, path, true)
	writer := openRefreshTestRepositoryAt(t, path, false)
	testSessionCleanupConcurrentRenewal(t, reader, writer)
}

func TestPostgresSessionCleanupConcurrentRenewal(t *testing.T) {
	repo := newPostgresQuiescenceRepository(t)
	testSessionCleanupConcurrentRenewal(t, repo, repo)
}

func testSessionCleanupConcurrentRenewal(t *testing.T, reader, writer *Repository) {
	t.Helper()
	ctx, cancelCtx := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancelCtx()
	now := time.Now().UTC()
	record := SessionAffinityRecord{Key: fmt.Sprintf("%064x", 1), CredentialID: "a", Revision: "r", ExpiresAt: now.Add(-time.Hour)}
	if errCreate := reader.db.Create(&record).Error; errCreate != nil {
		t.Fatal(errCreate)
	}
	started, release := make(chan struct{}), make(chan struct{})
	if errRegister := reader.db.Callback().Delete().Before("gorm:delete").Register("test_cleanup_renewal", func(tx *gorm.DB) {
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
			tx.AddError(ctx.Err())
		}
	}); errRegister != nil {
		t.Fatal(errRegister)
	}
	defer func() {
		if errRemove := reader.db.Callback().Delete().Remove("test_cleanup_renewal"); errRemove != nil {
			t.Errorf("remove cleanup barrier: %v", errRemove)
		}
	}()
	done := make(chan error, 1)
	go func() {
		deleted, errPurge := reader.PurgeExpiredSessionBindings(ctx, now, 1000)
		if errPurge == nil && deleted != 0 {
			errPurge = fmt.Errorf("deleted %d renewed bindings", deleted)
		}
		done <- errPurge
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("cleanup did not reach deletion barrier")
	}
	if errRenew := writer.db.WithContext(ctx).Model(&SessionAffinityRecord{}).Where("key = ?", record.Key).Update("expires_at", now.Add(time.Hour)).Error; errRenew != nil {
		close(release)
		<-done
		t.Fatal(errRenew)
	}
	close(release)
	select {
	case errPurge := <-done:
		if errPurge != nil {
			t.Fatal(errPurge)
		}
	case <-ctx.Done():
		t.Fatal("cleanup did not finish")
	}
}

func TestSessionCleanupDatabaseBacklogAndRenewal(t *testing.T) {
	testSessionCleanupDatabaseBacklogAndRenewal(t, newCredentialFoundationTestRepository(t))
}

func TestPostgresSessionCleanupDatabaseBacklogAndRenewal(t *testing.T) {
	testSessionCleanupDatabaseBacklogAndRenewal(t, newPostgresQuiescenceRepository(t))
}

func testSessionCleanupDatabaseBacklogAndRenewal(t *testing.T, repo *Repository) {
	t.Helper()
	now := time.Now().UTC()
	records := make([]SessionAffinityRecord, 2101)
	for index := range records {
		records[index] = SessionAffinityRecord{Key: fmt.Sprintf("%064x", index), CredentialID: "a", Revision: "revision", ExpiresAt: now.Add(-time.Hour)}
	}
	if errCreate := repo.db.CreateInBatches(&records, 100).Error; errCreate != nil {
		t.Fatal(errCreate)
	}
	// A concurrent renewal before physical deletion must remain valid even if
	// the cleaner's cutoff was captured before the renewal.
	if errRenew := repo.db.Model(&SessionAffinityRecord{}).Where("key = ?", records[0].Key).Update("expires_at", now.Add(time.Hour)).Error; errRenew != nil {
		t.Fatal(errRenew)
	}
	for _, want := range []int64{1000, 1000, 100, 0} {
		deleted, errPurge := repo.PurgeExpiredSessionBindings(t.Context(), now, 1000)
		if errPurge != nil || deleted != want {
			t.Fatalf("deleted=%d err=%v want=%d", deleted, errPurge, want)
		}
	}
	binding, errLoad := repo.LoadSessionBinding(t.Context(), records[0].Key)
	if errLoad != nil || binding.AuthID != "a" || !binding.ExpiresAt.After(now) {
		t.Fatalf("renewed binding deleted: %#v err=%v", binding, errLoad)
	}
}
