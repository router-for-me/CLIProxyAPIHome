package cluster

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPIHome/internal/node"
	"gorm.io/gorm"
)

type snapshotReplacementSourceContextKey struct{}

func TestPostgresCoordinatorBlockedSnapshotAllowsHeartbeatAndClientCount(t *testing.T) {
	repo := newPostgresQuiescenceRepository(t)
	coordinator := newSnapshotTestCoordinator(t, repo)
	home, _ := coordinator.HomeIncarnation()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	deleted, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	finish := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(finish)
	var deletes atomic.Int64
	if errRegister := repo.db.Callback().Delete().After("gorm:delete").Register("test:hold-background-snapshot", func(tx *gorm.DB) {
		if tx.Statement.Table != (CPANodeRecord{}).TableName() || deletes.Add(1) != 1 {
			return
		}
		close(deleted)
		select {
		case <-release:
		case <-tx.Statement.Context.Done():
		}
	}); errRegister != nil {
		t.Fatal(errRegister)
	}
	registry := node.GlobalRegistry()
	const oldIP, newIP = "192.0.2.211", "192.0.2.212"
	const oldID, newID = "snapshot-replacement-old", "snapshot-replacement-new"
	registry.AddWithNodeID(oldIP, oldID, home.StartedAt)
	t.Cleanup(func() {
		registry.RemoveWithNodeID(oldIP, oldID)
		registry.RemoveWithNodeID(newIP, newID)
	})
	startedAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	scheduler := cpaNodeSnapshotScheduler{coordinator: coordinator, lastSuccess: startedAt}
	coordinator.requestCPANodeSnapshotRefresh()
	firstDone := make(chan struct{})
	go func() {
		scheduler.refresh(ctx, startedAt.Add(time.Second))
		close(firstDone)
	}()
	waitSnapshotSignal(t, deleted)

	// These calls must finish while the snapshot DELETE-to-CREATE transaction is still held.
	heartbeatCtx, cancelHeartbeat := context.WithTimeout(ctx, 2*time.Second)
	defer cancelHeartbeat()
	incarnation, errHeartbeat := coordinator.heartbeatHomeIncarnation(heartbeatCtx)
	if errHeartbeat != nil {
		t.Fatalf("Home heartbeat blocked by snapshot: %v", errHeartbeat)
	}
	if errBeat := coordinator.heartbeatAndElect(heartbeatCtx, incarnation); errBeat != nil {
		t.Fatalf("heartbeat/election blocked by snapshot: %v", errBeat)
	}
	coordinator.requestCPANodeSnapshotRefresh()
	registry.RemoveWithNodeID(oldIP, oldID)
	registry.AddWithNodeID(newIP, newID, home.StartedAt)
	if errUpdate := coordinator.UpdateClientCount(heartbeatCtx, registry.TotalCount()); errUpdate != nil {
		t.Fatalf("client count blocked by snapshot: %v", errUpdate)
	}
	if repo.cpaSnapshotMu.TryLock() {
		repo.cpaSnapshotMu.Unlock()
		t.Fatal("snapshot did not retain its independent replacement lock")
	}
	if got := deletes.Load(); got != 1 {
		t.Fatalf("snapshot DELETEs while blocked = %d, want 1", got)
	}
	select {
	case <-firstDone:
		t.Fatal("held snapshot finished before release")
	default:
	}
	finish()
	waitSnapshotSignal(t, firstDone)
	scheduler.refresh(ctx, startedAt.Add(2*time.Second))
	var records []CPANodeRecord
	if errFind := repo.db.Where("home_ip = ? AND home_port = ? AND home_started_at = ?", home.IP, home.Port, home.StartedAt).Find(&records).Error; errFind != nil {
		t.Fatal(errFind)
	}
	if len(records) != 1 || records[0].NodeID != newID || deletes.Load() != 2 {
		t.Fatalf("latest snapshot lost or merged: records=%#v deletes=%d", records, deletes.Load())
	}
}

func TestPostgresCPANodeSnapshotReplacementSerializesCompleteTransaction(t *testing.T) {
	repo := newPostgresQuiescenceRepository(t)
	coordinator := newSnapshotTestCoordinator(t, repo)
	home, _ := coordinator.HomeIncarnation()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	seenAt, errNow := repo.CurrentDatabaseTime(ctx)
	if errNow != nil {
		t.Fatal(errNow)
	}
	deleted, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	finish := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(finish)
	var deletes atomic.Int64
	if errRegister := repo.db.Callback().Delete().After("gorm:delete").Register("test:hold-complete-replacement", func(tx *gorm.DB) {
		if tx.Statement.Table != (CPANodeRecord{}).TableName() {
			return
		}
		deletes.Add(1)
		if tx.Statement.Context.Value(snapshotReplacementSourceContextKey{}) != "first" {
			return
		}
		close(deleted)
		select {
		case <-release:
		case <-ctx.Done():
		}
	}); errRegister != nil {
		t.Fatal(errRegister)
	}
	firstDone, secondDone := make(chan error, 1), make(chan error, 1)
	firstCtx := context.WithValue(ctx, snapshotReplacementSourceContextKey{}, "first")
	go func() {
		firstDone <- repo.ReplaceCPANodeSnapshotForIncarnation(firstCtx, home, []node.Node{{NodeID: "old", IP: "192.0.2.213", ClientCount: 1}}, seenAt)
	}()
	waitSnapshotSignal(t, deleted)
	secondStarted := make(chan struct{})
	go func() {
		close(secondStarted)
		secondDone <- repo.ReplaceCPANodeSnapshotForIncarnation(ctx, home, []node.Node{{NodeID: "new", IP: "192.0.2.214", ClientCount: 1}}, seenAt)
	}()
	waitSnapshotSignal(t, secondStarted)
	// The independent mutex must span both DELETE and CREATE, not only either statement.
	if repo.cpaSnapshotMu.TryLock() {
		repo.cpaSnapshotMu.Unlock()
		t.Fatal("complete replacement lock was released between DELETE and CREATE")
	}
	if got := deletes.Load(); got != 1 {
		t.Fatalf("overlapping DELETEs = %d, want 1", got)
	}
	finish()
	for _, done := range []<-chan error{firstDone, secondDone} {
		select {
		case errReplace := <-done:
			if errReplace != nil {
				t.Fatal(errReplace)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	var records []CPANodeRecord
	if errFind := repo.db.Where("home_ip = ? AND home_port = ? AND home_started_at = ?", home.IP, home.Port, home.StartedAt).Find(&records).Error; errFind != nil {
		t.Fatal(errFind)
	}
	if len(records) != 1 || records[0].NodeID != "new" || deletes.Load() != 2 {
		t.Fatalf("replacement merged or lost snapshots: records=%#v deletes=%d", records, deletes.Load())
	}
}
