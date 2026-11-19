package cluster

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	homeerrors "github.com/router-for-me/CLIProxyAPIHome/internal/errors"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func TestUsagePluginDeliverySQLiteBoundedAdmission(t *testing.T) {
	for _, failCallback := range []bool{false, true} {
		t.Run(fmt.Sprintf("callback_failure_%t", failCallback), func(t *testing.T) {
			repo, closeRepo := newBillingTestRepository(t, t.Context())
			defer closeRepo()
			limit := maxSQLiteUsageDeliveries
			payloads := make([]string, limit+32)
			for index := range payloads {
				payloads[index] = fmt.Sprintf(`{"timestamp":"2026-11-06T10:27:15Z","provider":"codex","execution_id":"sqlite-admission-%d"}`, index)
				if _, errAppend := repo.AppendUsage(t.Context(), payloads[index], ""); errAppend != nil {
					t.Fatal(errAppend)
				}
			}
			callbackErr := errors.New("callback failed")
			var wantErr error
			if failCallback {
				wantErr = callbackErr
			}
			ownerCtx, cancelOwner := context.WithCancel(t.Context())
			entered := make(chan struct{}, limit)
			release := make(chan struct{})
			var releaseOnce sync.Once
			var workers sync.WaitGroup
			var calls atomic.Int32
			results := make(chan error, limit)
			rejected := make(chan error, len(payloads)-limit)
			defer func() {
				releaseOnce.Do(func() { close(release) })
				cancelOwner()
				workers.Wait()
			}()
			for index, payload := range payloads[:limit] {
				workers.Go(func() {
					results <- repo.DeliverPluginUsage(ownerCtx, payload, func() error {
						calls.Add(1)
						entered <- struct{}{}
						<-release
						return wantErr
					})
				})
				select {
				case <-entered:
				case errDelivery := <-results:
					t.Fatalf("callback %d was not admitted: %v", index, errDelivery)
				case <-time.After(5 * time.Second):
					t.Fatal("callbacks did not fill SQLite admission capacity")
				}
				if index == 0 {
					// Duplicate rejection must release its provisional slot, or
					// distinct events could no longer fill the remaining capacity.
					for range 2 * limit {
						if errDelivery := repo.DeliverPluginUsage(t.Context(), payload, func() error { calls.Add(1); return nil }); !errors.Is(errDelivery, homeerrors.ErrUsagePluginDeliveryPending) {
							t.Fatalf("same-event retry error=%v", errDelivery)
						}
					}
				}
			}
			// A canceled sender must not release capacity while its callback is
			// still blocked. New distinct events must fail promptly, not queue.
			cancelOwner()
			for _, payload := range payloads[limit:] {
				workers.Go(func() {
					rejected <- repo.DeliverPluginUsage(t.Context(), payload, func() error {
						calls.Add(1)
						<-release
						return nil
					})
				})
			}
			for range len(payloads) - limit {
				select {
				case errDelivery := <-rejected:
					if !errors.Is(errDelivery, homeerrors.ErrUsagePluginDeliveryPending) {
						t.Fatalf("overloaded SQLite delivery error=%v", errDelivery)
					}
				case <-time.After(5 * time.Second):
					t.Fatalf("SQLite overload did not return pending: active callbacks=%d, limit=%d", calls.Load(), limit)
				}
			}
			pending := 0
			repo.usagePluginPending.Range(func(_, _ any) bool { pending++; return true })
			if calls.Load() != int32(limit) || pending != limit || len(repo.usagePluginAdmission) != limit {
				t.Fatalf("unbounded SQLite delivery: callbacks=%d pending=%d admitted=%d limit=%d", calls.Load(), pending, len(repo.usagePluginAdmission), limit)
			}
			var completed int64
			if errCount := repo.db.Model(&UsageRecord{}).Where("plugin_usage_delivered = ?", true).Count(&completed).Error; errCount != nil || completed != 0 {
				t.Fatalf("unfinished work was completed: count=%d error=%v", completed, errCount)
			}
			releaseOnce.Do(func() { close(release) })
			for range limit {
				if errDelivery := <-results; !errors.Is(errDelivery, wantErr) {
					t.Fatalf("callback result=%v, want %v", errDelivery, wantErr)
				}
			}
			workers.Wait()
			if len(repo.usagePluginAdmission) != 0 {
				t.Fatalf("callback completion leaked %d admission slots", len(repo.usagePluginAdmission))
			}
			wantCompleted := int64(limit)
			if failCallback {
				wantCompleted = 0
			}
			if errCount := repo.db.Model(&UsageRecord{}).Where("plugin_usage_delivered = ?", true).Count(&completed).Error; errCount != nil || completed != wantCompleted {
				t.Fatalf("unexpected completion after callback: count=%d error=%v", completed, errCount)
			}
			var retries int
			for range 2 {
				for _, payload := range payloads {
					if errDelivery := repo.DeliverPluginUsage(t.Context(), payload, func() error { retries++; return nil }); errDelivery != nil {
						t.Fatalf("capacity did not recover: %v", errDelivery)
					}
				}
			}
			wantRetries := len(payloads) - limit
			if failCallback {
				wantRetries += limit
			}
			if retries != wantRetries {
				t.Fatalf("retry callbacks=%d, want %d", retries, wantRetries)
			}
			repo.usagePluginPending.Range(func(_, _ any) bool { t.Error("completed delivery leaked event ownership"); return true })
		})
	}
}

func TestUsagePluginDeliverySQLiteAdmissionErrorsReleaseCapacity(t *testing.T) {
	repo, closeRepo := newBillingTestRepository(t, t.Context())
	defer closeRepo()
	payload := `{"timestamp":"2026-11-06T10:27:15Z","provider":"codex","execution_id":"sqlite-admission-errors"}`
	if _, errAppend := repo.AppendUsage(t.Context(), payload, ""); errAppend != nil {
		t.Fatal(errAppend)
	}
	markerErr := errors.New("marker write failed")
	failMarker := false
	if errRegister := repo.db.Callback().Update().Before("gorm:update").Register("test:sqlite_admission_marker", func(tx *gorm.DB) {
		if failMarker {
			tx.AddError(markerErr)
		}
	}); errRegister != nil {
		t.Fatal(errRegister)
	}
	callbackErr := errors.New("callback failed")
	canceledCtx, cancel := context.WithCancel(t.Context())
	cancel()
	if errDelivery := repo.DeliverPluginUsage(canceledCtx, payload, nil); !errors.Is(errDelivery, context.Canceled) {
		t.Fatalf("canceled admission error=%v", errDelivery)
	}
	for range 2 * maxSQLiteUsageDeliveries {
		missing := `{"provider":"codex","execution_id":"missing-sqlite-admission"}`
		if errDelivery := repo.DeliverPluginUsage(t.Context(), missing, nil); !errors.Is(errDelivery, gorm.ErrRecordNotFound) {
			t.Fatalf("missing row error=%v", errDelivery)
		}
		if errDelivery := repo.DeliverPluginUsage(t.Context(), payload, func() error { return callbackErr }); !errors.Is(errDelivery, callbackErr) {
			t.Fatalf("callback error=%v", errDelivery)
		}
		failMarker = true
		if errDelivery := repo.DeliverPluginUsage(t.Context(), payload, nil); !errors.Is(errDelivery, markerErr) {
			t.Fatalf("marker error=%v", errDelivery)
		}
		failMarker = false
	}
	calls := 0
	for range 2 {
		if errDelivery := repo.DeliverPluginUsage(t.Context(), payload, func() error { calls++; return nil }); errDelivery != nil {
			t.Fatalf("admission did not recover after errors: %v", errDelivery)
		}
	}
	if calls != 1 || len(repo.usagePluginAdmission) != 0 {
		t.Fatalf("completed callbacks=%d admitted=%d, want one callback and no retained admission", calls, len(repo.usagePluginAdmission))
	}
	repo.usagePluginPending.Range(func(_, _ any) bool { t.Error("error path leaked event ownership"); return true })
}

func TestUsagePluginDeliveryPostgresBoundedAdmission(t *testing.T) {
	for _, configuredLimit := range []int{1, 2, 0, 64} {
		t.Run(fmt.Sprintf("max_open_%d", configuredLimit), func(t *testing.T) {
			fixture := newPostgresQuiescenceRepository(t)
			dsn := fixture.db.Dialector.(*postgres.Dialector).Config.DSN
			db, errOpen := Open(t.Context(), PGSQLConfig{PostgresURI: dsn, MaxOpenConns: configuredLimit})
			if errOpen != nil {
				t.Fatal(errOpen)
			}
			pool, errDB := db.DB()
			if errDB != nil {
				t.Fatal(errDB)
			}
			t.Cleanup(func() {
				if errClose := pool.Close(); errClose != nil {
					t.Error(errClose)
				}
			})
			configured, _ := db.Get(usageDeliveryStoreKey)
			store := configured.(*postgresUsageDeliveryStore)
			deliveryPool, errPool := store.db.DB()
			if errPool != nil {
				t.Fatal(errPool)
			}
			limit := maxUsageDeliveryConnections
			if configuredLimit > 0 && configuredLimit < limit {
				limit = configuredLimit
			}
			if cap(store.admission) != limit || deliveryPool.Stats().MaxOpenConnections != limit {
				t.Fatalf("admission capacity=%d, pool=%+v, want %d", cap(store.admission), deliveryPool.Stats(), limit)
			}
			repo := NewRepository(db)
			payloads := make([]string, limit+32)
			for index := range payloads {
				payloads[index] = fmt.Sprintf(`{"timestamp":"2026-11-06T10:27:15Z","provider":"codex","execution_id":"admission-%d"}`, index)
				if _, errAppend := repo.AppendUsage(t.Context(), payloads[index], ""); errAppend != nil {
					t.Fatal(errAppend)
				}
			}

			entered := make(chan struct{}, limit)
			release := make(chan struct{})
			var releaseOnce sync.Once
			var workers sync.WaitGroup
			var calls atomic.Int32
			results := make(chan error, limit)
			ownerCtx, cancelOwner := context.WithCancel(t.Context())
			defer func() {
				releaseOnce.Do(func() { close(release) })
				cancelOwner()
				workers.Wait()
			}()
			for _, payload := range payloads[:limit] {
				workers.Go(func() {
					results <- repo.DeliverPluginUsage(ownerCtx, payload, func() error {
						calls.Add(1)
						entered <- struct{}{}
						<-release
						return nil
					})
				})
			}
			for range limit {
				select {
				case <-entered:
				case errDelivery := <-results:
					t.Fatalf("delivery returned before callback: %v", errDelivery)
				case <-time.After(5 * time.Second):
					t.Fatal("callbacks did not fill admission capacity")
				}
			}
			// Even explicit request cancellation cannot release ownership during
			// an active callback. RESP disconnects provide no stronger guarantee.
			cancelOwner()
			before := deliveryPool.Stats()
			for _, payload := range payloads[limit:] {
				rejected := make(chan error, 1)
				workers.Go(func() {
					// Repository instances on the same database must share capacity.
					rejected <- NewRepository(db).DeliverPluginUsage(t.Context(), payload, func() error { calls.Add(1); return nil })
				})
				select {
				case errDelivery := <-rejected:
					if !errors.Is(errDelivery, homeerrors.ErrUsagePluginDeliveryPending) {
						t.Fatalf("overloaded admission error=%v", errDelivery)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("overloaded delivery queued instead of returning pending")
				}
			}
			pending := 0
			store.pending.Range(func(_, _ any) bool { pending++; return true })
			if pending != limit || len(store.admission) != limit || calls.Load() != int32(limit) {
				t.Fatalf("unbounded retained work: pending=%d admitted=%d calls=%d", pending, len(store.admission), calls.Load())
			}
			if after := deliveryPool.Stats(); after.WaitCount != before.WaitCount || after.InUse != limit {
				t.Fatalf("rejected work reached the pool wait queue: before=%+v after=%+v", before, after)
			}
			var completed int64
			if errCount := db.Model(&UsageRecord{}).Where("plugin_usage_delivered = ?", true).Count(&completed).Error; errCount != nil || completed != 0 {
				t.Fatalf("blocked or rejected work was completed: count=%d error=%v", completed, errCount)
			}
			releaseOnce.Do(func() { close(release) })
			for range limit {
				if errDelivery := <-results; errDelivery != nil {
					t.Fatal(errDelivery)
				}
			}
			workers.Wait()
			if len(store.admission) != 0 {
				t.Fatalf("completion leaked %d admission slots", len(store.admission))
			}
			for _, payload := range payloads {
				if errDelivery := repo.DeliverPluginUsage(t.Context(), payload, func() error { calls.Add(1); return nil }); errDelivery != nil {
					t.Fatal(errDelivery)
				}
			}
			if calls.Load() != int32(len(payloads)) {
				t.Fatalf("retried callbacks=%d, want %d", calls.Load(), len(payloads))
			}

			// Query, callback, marker-write and connection-acquisition failures
			// must all return admission capacity and remove per-event ownership.
			missing := `{"provider":"codex","execution_id":"missing-admission-row"}`
			if errDelivery := repo.DeliverPluginUsage(t.Context(), missing, nil); !errors.Is(errDelivery, gorm.ErrRecordNotFound) {
				t.Fatalf("missing row error=%v", errDelivery)
			}
			failure := `{"timestamp":"2026-11-06T10:27:15Z","provider":"codex","execution_id":"failed-admission-callback"}`
			if _, errAppend := repo.AppendUsage(t.Context(), failure, ""); errAppend != nil {
				t.Fatal(errAppend)
			}
			callbackErr := errors.New("callback failed")
			if errDelivery := repo.DeliverPluginUsage(t.Context(), failure, func() error { return callbackErr }); !errors.Is(errDelivery, callbackErr) {
				t.Fatalf("callback error=%v", errDelivery)
			}
			markerErr := errors.New("marker write failed")
			failMarker := true
			if errRegister := store.db.Callback().Update().Before("gorm:update").Register("test:admission_marker", func(tx *gorm.DB) {
				if failMarker {
					tx.AddError(markerErr)
				}
			}); errRegister != nil {
				t.Fatal(errRegister)
			}
			if errDelivery := repo.DeliverPluginUsage(t.Context(), failure, nil); !errors.Is(errDelivery, markerErr) {
				t.Fatalf("marker error=%v", errDelivery)
			}
			failMarker = false
			if errDelivery := repo.DeliverPluginUsage(t.Context(), failure, nil); errDelivery != nil {
				t.Fatalf("capacity did not recover after errors: %v", errDelivery)
			}
			if errClose := deliveryPool.Close(); errClose != nil {
				t.Fatal(errClose)
			}
			if errDelivery := repo.DeliverPluginUsage(t.Context(), failure, nil); errDelivery == nil {
				t.Fatal("closed delivery pool returned success")
			}
			if len(store.admission) != 0 {
				t.Fatalf("errors leaked %d admission slots", len(store.admission))
			}
			store.pending.Range(func(_, _ any) bool { t.Error("error path leaked event ownership"); return true })
		})
	}
}
