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
)

func TestUsagePluginDeliverySQLite(t *testing.T) {
	repo, closeRepo := newBillingTestRepository(t, t.Context())
	defer closeRepo()
	assertUsagePluginDelivery(t, repo, repo)
}

func TestUsagePluginDeliveryPostgres(t *testing.T) {
	repo := newPostgresQuiescenceRepository(t)
	// Separate database handles model Homes with independent delivery pools and
	// pending maps, so this must exercise the PostgreSQL row-lock boundary.
	dsn := repo.db.Dialector.(*postgres.Dialector).Config.DSN
	retryDB, errOpen := Open(t.Context(), PGSQLConfig{PostgresURI: dsn, MaxOpenConns: 1})
	if errOpen != nil {
		t.Fatal(errOpen)
	}
	retryPool, errDB := retryDB.DB()
	if errDB != nil {
		t.Fatal(errDB)
	}
	t.Cleanup(func() {
		if errClose := retryPool.Close(); errClose != nil {
			t.Error(errClose)
		}
	})
	assertUsagePluginDelivery(t, repo, NewRepository(retryDB))
}

func TestUsagePluginDeliveryPostgresConstrainedPool(t *testing.T) {
	for _, limit := range []int{1, 2} {
		t.Run(fmt.Sprintf("max_open_%d", limit), func(t *testing.T) {
			fixture := newPostgresQuiescenceRepository(t)
			dsn := fixture.db.Dialector.(*postgres.Dialector).Config.DSN
			db, errOpen := Open(t.Context(), PGSQLConfig{PostgresURI: dsn, MaxOpenConns: limit})
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
			store, supported := configured.(*postgresUsageDeliveryStore)
			if !supported {
				t.Fatal("delivery store missing from database")
			}
			deliveryPool, errPool := store.db.DB()
			if errPool != nil {
				t.Fatal(errPool)
			}
			if stats := deliveryPool.Stats(); stats.MaxOpenConnections != limit || stats.OpenConnections != 0 {
				t.Fatalf("delivery pool must be bounded and lazy: %+v", stats)
			}
			repo := NewRepository(db)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			count := limit
			payloads := make([]string, count+1)
			for index := range payloads {
				payloads[index] = fmt.Sprintf(`{"timestamp":"2026-11-06T10:27:15Z","provider":"codex","execution_id":"pool-event-%d"}`, index)
				if _, errAppend := repo.AppendUsage(ctx, payloads[index], ""); errAppend != nil {
					t.Fatal(errAppend)
				}
			}
			entered := make(chan struct{}, limit)
			release := make(chan struct{})
			var releaseOnce sync.Once
			var workers sync.WaitGroup
			var calls atomic.Int32
			results := make(chan error, count)
			defer func() {
				releaseOnce.Do(func() { close(release) })
				cancel()
				workers.Wait()
			}()
			for index, payload := range payloads[:count] {
				workers.Add(1)
				go func() {
					defer workers.Done()
					errDelivery := repo.DeliverPluginUsage(ctx, payload, func() error {
						calls.Add(1)
						if index < limit {
							entered <- struct{}{}
							<-release
						}
						_, errSet := repo.KVSet(ctx, fmt.Sprintf("pool-callback-%d", index), []byte("value"), 0, KVSetModeAlways)
						return errSet
					})
					results <- errDelivery
				}()
				// Fill every transaction slot before testing canceled admission.
				if index == limit-1 {
					for range limit {
						select {
						case <-entered:
						case <-ctx.Done():
							t.Fatal("callbacks could not acquire delivery transactions")
						}
					}
					if stats := pool.Stats(); stats.InUse != 0 {
						t.Fatalf("delivery reserved callback pool connections: %+v", stats)
					}
					if stats := deliveryPool.Stats(); stats.InUse != limit {
						t.Fatalf("delivery pool slots were not filled: %+v", stats)
					}
					// Canceled requests must not consume isolated capacity.
					admissionCtx, cancelAdmission := context.WithCancel(ctx)
					cancelAdmission()
					admissionResult := make(chan error, 1)
					go func() {
						admissionResult <- NewRepository(db).DeliverPluginUsage(admissionCtx, payloads[count], func() error { calls.Add(1); return nil })
					}()
					select {
					case errAdmission := <-admissionResult:
						if !errors.Is(errAdmission, context.Canceled) {
							t.Fatalf("canceled pool admission error=%v", errAdmission)
						}
					case <-ctx.Done():
						t.Fatal("pool admission ignored cancellation")
					}
					releaseOnce.Do(func() { close(release) })
				}
			}
			for range count {
				select {
				case errResult := <-results:
					if errResult != nil {
						t.Fatalf("delivery or callback KV failed: %v", errResult)
					}
				case <-ctx.Done():
					t.Fatal("delivery transactions exhausted callback database connections")
				}
			}
			workers.Wait()
			if calls.Load() != int32(count) {
				t.Fatalf("callback count=%d, want %d", calls.Load(), count)
			}
			var completed int64
			if errCount := db.Model(&UsageRecord{}).Where("plugin_usage_delivered = ?", true).Count(&completed).Error; errCount != nil || completed != int64(count) {
				t.Fatalf("completed events=%d error=%v", completed, errCount)
			}
			if errClose := pool.Close(); errClose != nil {
				t.Fatal(errClose)
			}
			if stats := deliveryPool.Stats(); stats.OpenConnections != 0 {
				t.Fatalf("delivery pool leaked after main pool close: %+v", stats)
			}
			if errPing := deliveryPool.PingContext(ctx); errPing == nil {
				t.Fatal("delivery pool survived main pool closure")
			}
		})
	}
}

func TestUsagePluginDeliveryMigration(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			var repo *Repository
			if backend == "postgres" {
				repo = newPostgresQuiescenceRepository(t)
			} else {
				var closeRepo func()
				repo, closeRepo = newBillingTestRepository(t, t.Context())
				defer closeRepo()
			}
			payload := `{"timestamp":"2026-11-06T10:27:15Z","provider":"codex","execution_id":"before-delivery-migration"}`
			if _, errAppend := repo.AppendUsage(t.Context(), payload, ""); errAppend != nil {
				t.Fatal(errAppend)
			}
			if errDrop := repo.db.Migrator().DropColumn(&UsageRecord{}, "plugin_usage_delivered"); errDrop != nil {
				t.Fatal(errDrop)
			}
			if backend == "postgres" {
				// Recreate a v19 fixture; the production version writer is monotonic.
				if errVersion := repo.db.Model(&schemaMigrationRecord{}).Where("id = ?", schemaMigrationRecordID).Update("version", 19).Error; errVersion != nil {
					t.Fatal(errVersion)
				}
			}
			if errMigrate := AutoMigrate(repo.db); errMigrate != nil {
				t.Fatal(errMigrate)
			}
			if !repo.db.Migrator().HasColumn(&UsageRecord{}, "plugin_usage_delivered") {
				t.Fatal("migration did not add the delivery completion marker")
			}
			if backend == "postgres" {
				assertPostgresSchemaMigrationVersion(t, repo.db, currentDatabaseVersion)
			}
			var record UsageRecord
			if errFirst := repo.db.First(&record).Error; errFirst != nil || record.PluginUsageDelivered {
				t.Fatalf("migrated event=%+v error=%v", record, errFirst)
			}
			called := false
			if errDelivery := repo.DeliverPluginUsage(t.Context(), payload, func() error { called = true; return nil }); errDelivery != nil || !called {
				t.Fatalf("migrated event delivery called=%v error=%v", called, errDelivery)
			}
		})
	}
}

func assertUsagePluginDelivery(t *testing.T, owner, retry *Repository) {
	t.Helper()
	ctx := t.Context()
	payload := `{"timestamp":"2026-11-06T10:27:15Z","provider":"codex","execution_id":"delivery-state","selection_id":"retained-selection"}`
	if _, inserted, errAppend := owner.AppendUsageWithRuntimeOutcome(ctx, payload, UsageRuntimeMetadata{}); errAppend != nil || !inserted {
		t.Fatalf("append inserted=%v error=%v", inserted, errAppend)
	}
	var calls atomic.Int32
	entered := make(chan error, 1)
	release := make(chan struct{})
	done := make(chan error, 1)
	finished := make(chan struct{})
	ownerCtx, cancelOwner := context.WithCancel(ctx)
	defer cancelOwner()
	defer func() {
		close(release)
		<-finished
	}()
	go func() {
		defer close(finished)
		done <- owner.DeliverPluginUsage(ownerCtx, payload, func() error {
			calls.Add(1)
			// Plugin storage must remain writable while delivery owns the event.
			_, errSet := owner.KVSet(context.Background(), "delivery-test", []byte("value"), 0, KVSetModeAlways)
			entered <- errSet
			<-release
			return errSet
		})
	}()
	select {
	case errSet := <-entered:
		if errSet != nil {
			t.Fatalf("plugin storage write: %v", errSet)
		}
	case errDelivery := <-done:
		t.Fatalf("delivery returned before callback: %v", errDelivery)
	case <-ctx.Done():
		t.Fatal("plugin callback did not start")
	}
	cancelOwner()
	// Same-store PostgreSQL duplicates must release any provisional admission
	// slot even when the pool has spare capacity and the pending-key fence rejects them.
	if owner.db.Dialector.Name() == "postgres" {
		if errDelivery := NewRepository(owner.db).DeliverPluginUsage(ctx, payload, func() error { calls.Add(1); return nil }); !errors.Is(errDelivery, homeerrors.ErrUsagePluginDeliveryPending) {
			t.Fatalf("same-store pending retry error=%v", errDelivery)
		}
		configured, _ := owner.db.Get(usageDeliveryStoreKey)
		if admitted := len(configured.(*postgresUsageDeliveryStore).admission); admitted != 1 {
			t.Fatalf("duplicate leaked admission capacity: admitted=%d, want one active owner", admitted)
		}
	}
	for attempt := 0; attempt < 2; attempt++ {
		if _, inserted, errAppend := retry.AppendUsageWithRuntimeOutcome(ctx, payload, UsageRuntimeMetadata{}); errAppend != nil || inserted {
			t.Fatalf("pending retry inserted=%v error=%v", inserted, errAppend)
		}
		// Neither a duplicate callback nor a new snapshot with no sink may
		// acknowledge an event whose original callback is still blocked.
		var deliver func() error
		if attempt == 0 {
			deliver = func() error { calls.Add(1); return nil }
		}
		if errDelivery := retry.DeliverPluginUsage(ctx, payload, deliver); !errors.Is(errDelivery, homeerrors.ErrUsagePluginDeliveryPending) {
			t.Fatalf("pending retry %d error=%v", attempt, errDelivery)
		}
	}
	release <- struct{}{}
	if errDelivery := <-done; errDelivery != nil {
		t.Fatal(errDelivery)
	}
	if errDelivery := NewRepository(owner.db).DeliverPluginUsage(ctx, payload, func() error { calls.Add(1); return nil }); errDelivery != nil {
		t.Fatal(errDelivery)
	}
	if calls.Load() != 1 {
		t.Fatalf("callback count=%d, want one completed delivery", calls.Load())
	}
	var record UsageRecord
	if errFirst := owner.db.First(&record).Error; errFirst != nil || !record.PluginUsageDelivered {
		t.Fatalf("delivery completion was not persisted: record=%+v error=%v", record, errFirst)
	}
	// Selection identity is not event identity: a retained selection can produce
	// another execution, which must still reach the plugin.
	second := `{"timestamp":"2026-11-06T10:27:16Z","provider":"codex","execution_id":"another-execution","selection_id":"retained-selection"}`
	if _, _, errAppend := retry.AppendUsageWithRuntimeOutcome(ctx, second, UsageRuntimeMetadata{ReceivedAt: time.Now()}); errAppend != nil {
		t.Fatal(errAppend)
	}
	if errDelivery := retry.DeliverPluginUsage(ctx, second, func() error { calls.Add(1); return nil }); errDelivery != nil || calls.Load() != 2 {
		t.Fatalf("retained selection delivery count=%d error=%v", calls.Load(), errDelivery)
	}
}
