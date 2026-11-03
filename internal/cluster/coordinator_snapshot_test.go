package cluster

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPIHome/internal/node"
	"gorm.io/gorm"
)

func newSnapshotTestCoordinator(t *testing.T, repo *Repository) *Coordinator {
	t.Helper()
	coordinator := NewCoordinator(repo, NodeIdentity{IP: "snapshot-home", Port: 8327}, CoordinatorOptions{})
	if errInitialize := coordinator.Initialize(context.Background()); errInitialize != nil {
		t.Fatal(errInitialize)
	}
	t.Cleanup(func() { coordinator.setMaster(false) })
	return coordinator
}

func waitSnapshotSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for snapshot synchronization")
	}
}

func snapshotDeleteCounter(t *testing.T, repo *Repository) *atomic.Int64 {
	t.Helper()
	var deletes atomic.Int64
	if errRegister := repo.db.Callback().Delete().Before("gorm:delete").Register("test:count-snapshot-deletes", func(tx *gorm.DB) {
		if tx.Statement.Table == (CPANodeRecord{}).TableName() {
			deletes.Add(1)
		}
	}); errRegister != nil {
		t.Fatal(errRegister)
	}
	return &deletes
}

func TestCoordinatorSnapshotCoalescesNotifications(t *testing.T) {
	repo := newCredentialFoundationTestRepository(t)
	coordinator := newSnapshotTestCoordinator(t, repo)
	deletes := snapshotDeleteCounter(t, repo)
	startedAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	scheduler := cpaNodeSnapshotScheduler{coordinator: coordinator, lastSuccess: startedAt}
	for range 100 {
		coordinator.requestCPANodeSnapshotRefresh()
	}
	if got := len(coordinator.snapshotRefresh); got != 1 {
		t.Fatalf("queued notifications = %d, want 1", got)
	}
	if deletes.Load() != 0 {
		t.Fatal("notification performed database work")
	}
	scheduler.refresh(context.Background(), startedAt.Add(time.Second))
	scheduler.refresh(context.Background(), startedAt.Add(2*time.Second))
	if got := deletes.Load(); got != 1 {
		t.Fatalf("snapshot replacements = %d, want 1", got)
	}
}

func TestCoordinatorSnapshotRefreshesPeriodicallyWithoutNotifications(t *testing.T) {
	repo := newCredentialFoundationTestRepository(t)
	coordinator := newSnapshotTestCoordinator(t, repo)
	deletes := snapshotDeleteCounter(t, repo)
	startedAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	scheduler := cpaNodeSnapshotScheduler{coordinator: coordinator, lastSuccess: startedAt}
	scheduler.refresh(context.Background(), startedAt.Add(time.Second))
	if deletes.Load() != 0 {
		t.Fatal("snapshot refreshed before the periodic deadline")
	}
	scheduler.refresh(context.Background(), startedAt.Add(coordinator.heartbeatInterval))
	scheduler.refresh(context.Background(), startedAt.Add(2*coordinator.heartbeatInterval))
	if got := deletes.Load(); got != 2 {
		t.Fatalf("periodic replacements = %d, want 2", got)
	}
}

func TestCoordinatorSnapshotReadsLatestNodesAndClearsLastNode(t *testing.T) {
	repo := newCredentialFoundationTestRepository(t)
	coordinator := newSnapshotTestCoordinator(t, repo)
	registry := node.GlobalRegistry()
	const oldIP, newIP = "192.0.2.221", "192.0.2.222"
	const oldID, newID = "snapshot-queued-old", "snapshot-queued-new"
	registry.AddWithNodeID(oldIP, oldID, time.Now().UTC())
	t.Cleanup(func() {
		registry.RemoveWithNodeID(oldIP, oldID)
		registry.RemoveWithNodeID(newIP, newID)
	})
	coordinator.requestCPANodeSnapshotRefresh()
	registry.RemoveWithNodeID(oldIP, oldID)
	registry.AddWithNodeID(newIP, newID, time.Now().UTC())
	startedAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	scheduler := cpaNodeSnapshotScheduler{coordinator: coordinator, lastSuccess: startedAt}
	scheduler.refresh(context.Background(), startedAt.Add(time.Second))
	var records []CPANodeRecord
	if errFind := repo.db.Find(&records).Error; errFind != nil {
		t.Fatal(errFind)
	}
	if len(records) != 1 || records[0].NodeID != newID {
		t.Fatalf("snapshot used queued node data: %#v", records)
	}
	registry.RemoveWithNodeID(newIP, newID)
	coordinator.requestCPANodeSnapshotRefresh()
	scheduler.refresh(context.Background(), startedAt.Add(2*time.Second))
	var count int64
	if errCount := repo.db.Model(&CPANodeRecord{}).Count(&count).Error; errCount != nil {
		t.Fatal(errCount)
	}
	if count != 0 {
		t.Fatalf("snapshots after last node left = %d, want 0", count)
	}
}

func TestCoordinatorSnapshotRetriesWriteFailureOnNextTick(t *testing.T) {
	repo := newCredentialFoundationTestRepository(t)
	coordinator := newSnapshotTestCoordinator(t, repo)
	var attempts int
	wantErr := errors.New("injected snapshot write failure")
	if errRegister := repo.db.Callback().Delete().Before("gorm:delete").Register("test:fail-first-snapshot", func(tx *gorm.DB) {
		if tx.Statement.Table == (CPANodeRecord{}).TableName() {
			attempts++
			if attempts == 1 {
				tx.AddError(wantErr)
			}
		}
	}); errRegister != nil {
		t.Fatal(errRegister)
	}
	startedAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	scheduler := cpaNodeSnapshotScheduler{coordinator: coordinator, lastSuccess: startedAt}
	coordinator.requestCPANodeSnapshotRefresh()
	scheduler.refresh(context.Background(), startedAt.Add(time.Second))
	if attempts != 1 || !scheduler.pending || !scheduler.lastSuccess.Equal(startedAt) {
		t.Fatalf("failure state: attempts=%d pending=%t lastSuccess=%v", attempts, scheduler.pending, scheduler.lastSuccess)
	}
	nextTick := startedAt.Add(2 * time.Second)
	scheduler.refresh(context.Background(), nextTick)
	if attempts != 2 || scheduler.pending || !scheduler.lastSuccess.Equal(nextTick) {
		t.Fatalf("recovery state: attempts=%d pending=%t lastSuccess=%v", attempts, scheduler.pending, scheduler.lastSuccess)
	}
}

func TestCoordinatorSnapshotDoesNotFallbackWhenDatabaseTimeFails(t *testing.T) {
	repo := newCredentialFoundationTestRepository(t)
	coordinator := newSnapshotTestCoordinator(t, repo)
	deletes := snapshotDeleteCounter(t, repo)
	var clockAttempts int
	if errRegister := repo.db.Callback().Row().Before("gorm:row").Register("test:fail-snapshot-clock", func(tx *gorm.DB) {
		if tx.Statement.SQL.String() == databaseNowQuery(tx) {
			clockAttempts++
			tx.AddError(errors.New("injected database clock failure"))
		}
	}); errRegister != nil {
		t.Fatal(errRegister)
	}
	startedAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	scheduler := cpaNodeSnapshotScheduler{coordinator: coordinator, lastSuccess: startedAt}
	coordinator.requestCPANodeSnapshotRefresh()
	scheduler.refresh(context.Background(), startedAt.Add(time.Second))
	if clockAttempts != 1 || deletes.Load() != 0 || !scheduler.pending || !scheduler.lastSuccess.Equal(startedAt) {
		t.Fatalf("clock failure: attempts=%d deletes=%d pending=%t lastSuccess=%v", clockAttempts, deletes.Load(), scheduler.pending, scheduler.lastSuccess)
	}
}

func TestCoordinatorSnapshotPreservesNotificationDuringRefresh(t *testing.T) {
	repo := newCredentialFoundationTestRepository(t)
	coordinator := newSnapshotTestCoordinator(t, repo)
	registry := node.GlobalRegistry()
	const clientIP, nodeID = "192.0.2.223", "snapshot-during-refresh"
	t.Cleanup(func() { registry.RemoveWithNodeID(clientIP, nodeID) })
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	finish := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(finish)
	var attempts atomic.Int64
	if errRegister := repo.db.Callback().Delete().After("gorm:delete").Register("test:block-first-snapshot", func(tx *gorm.DB) {
		if tx.Statement.Table == (CPANodeRecord{}).TableName() && attempts.Add(1) == 1 {
			close(entered)
			select {
			case <-release:
			case <-tx.Statement.Context.Done():
			}
		}
	}); errRegister != nil {
		t.Fatal(errRegister)
	}
	startedAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	scheduler := cpaNodeSnapshotScheduler{coordinator: coordinator, lastSuccess: startedAt}
	coordinator.requestCPANodeSnapshotRefresh()
	done := make(chan struct{})
	go func() {
		scheduler.refresh(context.Background(), startedAt.Add(time.Second))
		close(done)
	}()
	waitSnapshotSignal(t, entered)
	registry.AddWithNodeID(clientIP, nodeID, time.Now().UTC())
	coordinator.requestCPANodeSnapshotRefresh()
	finish()
	waitSnapshotSignal(t, done)
	scheduler.refresh(context.Background(), startedAt.Add(2*time.Second))
	var record CPANodeRecord
	if errFind := repo.db.First(&record, "node_id = ?", nodeID).Error; errFind != nil {
		t.Fatal(errFind)
	}
	if attempts.Load() != 2 {
		t.Fatalf("replacements = %d, want 2", attempts.Load())
	}
}

func TestCoordinatorSnapshotLoopCancelsInFlightRefresh(t *testing.T) {
	repo := newCredentialFoundationTestRepository(t)
	coordinator := newSnapshotTestCoordinator(t, repo)
	entered, exited := make(chan struct{}), make(chan struct{})
	var attempts atomic.Int64
	if errRegister := repo.db.Callback().Delete().Before("gorm:delete").Register("test:cancel-snapshot", func(tx *gorm.DB) {
		if tx.Statement.Table != (CPANodeRecord{}).TableName() {
			return
		}
		attempts.Add(1)
		deadline, ok := tx.Statement.Context.Deadline()
		if !ok || time.Until(deadline) > cpaNodeSnapshotRefreshBudget {
			t.Error("snapshot is missing its refresh budget")
		}
		close(entered)
		<-tx.Statement.Context.Done()
		tx.AddError(tx.Statement.Context.Err())
	}); errRegister != nil {
		t.Fatal(errRegister)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ticks := make(chan time.Time, 1)
	coordinator.requestCPANodeSnapshotRefresh()
	ticks <- time.Now()
	go func() {
		coordinator.runCPANodeSnapshotLoop(ctx, ticks, time.Now())
		close(exited)
	}()
	waitSnapshotSignal(t, entered)
	coordinator.requestCPANodeSnapshotRefresh()
	cancel()
	waitSnapshotSignal(t, exited)
	if attempts.Load() != 1 {
		t.Fatalf("refresh attempts on exit = %d, want 1", attempts.Load())
	}
}

func TestCoordinatorSnapshotRejectsOldIncarnation(t *testing.T) {
	for _, state := range []string{HomeIncarnationFenced, HomeIncarnationRetired} {
		t.Run(state, func(t *testing.T) {
			repo := newCredentialFoundationTestRepository(t)
			coordinator := newSnapshotTestCoordinator(t, repo)
			home, _ := coordinator.HomeIncarnation()
			if errUpdate := repo.db.Model(&HomeProcessIncarnationRecord{}).
				Where("home_ip = ? AND home_port = ? AND started_at = ?", home.IP, home.Port, home.StartedAt).
				Update("state", state).Error; errUpdate != nil {
				t.Fatal(errUpdate)
			}
			if _, errRegister := repo.RegisterHomeIncarnation(context.Background(), home.IP, home.Port, nil); errRegister != nil {
				t.Fatal(errRegister)
			}
			deletes := snapshotDeleteCounter(t, repo)
			startedAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			scheduler := cpaNodeSnapshotScheduler{coordinator: coordinator, lastSuccess: startedAt}
			coordinator.requestCPANodeSnapshotRefresh()
			scheduler.refresh(context.Background(), startedAt.Add(time.Second))
			if deletes.Load() != 0 || !scheduler.pending || !scheduler.lastSuccess.Equal(startedAt) {
				t.Fatal("old incarnation was allowed to replace a snapshot")
			}
		})
	}
}

func TestCoordinatorClientCountDoesNotReadClockOrWriteSnapshot(t *testing.T) {
	repo := newCredentialFoundationTestRepository(t)
	coordinator := newSnapshotTestCoordinator(t, repo)
	deletes := snapshotDeleteCounter(t, repo)
	if errRegister := repo.db.Callback().Row().Before("gorm:row").Register("test:reject-client-count-clock", func(tx *gorm.DB) {
		t.Error("client count update queried the database clock")
		tx.AddError(errors.New("unexpected clock query"))
	}); errRegister != nil {
		t.Fatal(errRegister)
	}
	if errUpdate := coordinator.UpdateClientCount(context.Background(), 7); errUpdate != nil {
		t.Fatal(errUpdate)
	}
	var record ClusterNodeRecord
	if errFind := repo.db.First(&record, "ip = ? AND port = ?", coordinator.node.IP, coordinator.node.Port).Error; errFind != nil {
		t.Fatal(errFind)
	}
	if record.ClientCount != 7 || deletes.Load() != 0 || len(coordinator.snapshotRefresh) != 1 {
		t.Fatalf("client count=%d snapshot writes=%d notifications=%d", record.ClientCount, deletes.Load(), len(coordinator.snapshotRefresh))
	}
	<-coordinator.snapshotRefresh
	wantErr := errors.New("injected count update failure")
	if errRegister := repo.db.Callback().Update().Before("gorm:update").Register("test:fail-client-count", func(tx *gorm.DB) {
		if tx.Statement.Table == (ClusterNodeRecord{}).TableName() {
			tx.AddError(wantErr)
		}
	}); errRegister != nil {
		t.Fatal(errRegister)
	}
	if errUpdate := coordinator.UpdateClientCount(context.Background(), 8); !errors.Is(errUpdate, wantErr) {
		t.Fatalf("UpdateClientCount() error = %v, want %v", errUpdate, wantErr)
	}
	if len(coordinator.snapshotRefresh) != 0 {
		t.Fatal("failed count update queued a snapshot refresh")
	}
}

func TestCoordinatorStartWaitsForSnapshotOnEveryExit(t *testing.T) {
	for _, reason := range []string{"canceled", "heartbeat-failed", "election-failed"} {
		t.Run(reason, func(t *testing.T) {
			repo := newCredentialFoundationTestRepository(t)
			coordinator := newSnapshotTestCoordinator(t, repo)
			coordinator.heartbeatInterval = 10 * time.Millisecond
			entered, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			finish := func() { releaseOnce.Do(func() { close(release) }) }
			t.Cleanup(finish)
			var snapshotCalls atomic.Int64
			if errRegister := repo.db.Callback().Row().Before("gorm:row").Register("test:hold-runtime-snapshot-clock", func(tx *gorm.DB) {
				deadline, ok := tx.Statement.Context.Deadline()
				// Snapshot clock reads have a deadline and run outside transactions.
				// Cleanup clock reads run in transactions; heartbeat queries have no deadline.
				_, inTransaction := tx.Statement.ConnPool.(gorm.TxCommitter)
				if tx.Statement.SQL.String() != databaseNowQuery(tx) || !ok || inTransaction {
					return
				}
				if time.Until(deadline) > cpaNodeSnapshotRefreshBudget {
					t.Error("snapshot exceeded its refresh budget")
				}
				snapshotCalls.Add(1)
				close(entered)
				<-tx.Statement.Context.Done()
				close(canceled)
				<-release
				tx.AddError(tx.Statement.Context.Err())
			}); errRegister != nil {
				t.Fatal(errRegister)
			}
			wantErr := errors.New("injected coordinator failure")
			var fail atomic.Bool
			if errRegister := repo.db.Callback().Update().Before("gorm:update").Register("test:fail-runtime-heartbeat", func(tx *gorm.DB) {
				if reason == "heartbeat-failed" && fail.Load() && tx.Statement.Table == (HomeProcessIncarnationRecord{}).TableName() {
					tx.AddError(wantErr)
				}
			}); errRegister != nil {
				t.Fatal(errRegister)
			}
			if errRegister := repo.db.Callback().Create().Before("gorm:create").Register("test:fail-runtime-election", func(tx *gorm.DB) {
				if reason == "election-failed" && fail.Load() && tx.Statement.Table == (ClusterNodeRecord{}).TableName() {
					tx.AddError(wantErr)
				}
			}); errRegister != nil {
				t.Fatal(errRegister)
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			exited := make(chan struct{})
			go func() {
				done <- coordinator.Start(ctx)
				close(exited)
			}()
			t.Cleanup(func() {
				cancel()
				finish()
				waitSnapshotSignal(t, exited)
			})
			waitSnapshotSignal(t, entered)
			coordinator.requestCPANodeSnapshotRefresh()
			if reason == "canceled" {
				cancel()
			} else {
				fail.Store(true)
			}
			waitSnapshotSignal(t, canceled)
			select {
			case errStart := <-done:
				t.Fatalf("Start returned before snapshot worker exited: %v", errStart)
			default:
			}
			finish()
			waitSnapshotSignal(t, exited)
			errStart := <-done
			if reason == "canceled" && errStart != nil {
				t.Fatal(errStart)
			}
			if reason != "canceled" && !errors.Is(errStart, wantErr) {
				t.Fatalf("Start() error = %v, want %v", errStart, wantErr)
			}
			if snapshotCalls.Load() != 1 {
				t.Fatalf("snapshot calls = %d, want one worker and no exit refresh", snapshotCalls.Load())
			}
		})
	}
}

func TestCoordinatorInitializeToleratesSnapshotFailure(t *testing.T) {
	repo := newCredentialFoundationTestRepository(t)
	var attempts int
	if errRegister := repo.db.Callback().Delete().Before("gorm:delete").Register("test:fail-initial-snapshot", func(tx *gorm.DB) {
		if tx.Statement.Table == (CPANodeRecord{}).TableName() {
			attempts++
			tx.AddError(errors.New("injected initial snapshot failure"))
		}
	}); errRegister != nil {
		t.Fatal(errRegister)
	}
	coordinator := newSnapshotTestCoordinator(t, repo)
	if _, initialized := coordinator.HomeIncarnation(); !initialized || attempts != 1 {
		t.Fatalf("initialized=%t snapshot attempts=%d", initialized, attempts)
	}
}
