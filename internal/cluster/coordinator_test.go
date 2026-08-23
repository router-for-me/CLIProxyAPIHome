package cluster

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPIHome/internal/node"
)

func TestHeartbeatElectionPersistsExactlyOneLiveMaster(t *testing.T) {
	ctx := context.Background()
	repo := newCredentialFoundationTestRepository(t)
	olderHome, errOlderHome := repo.RegisterHomeIncarnation(ctx, "10.0.0.1", 8317, nil)
	if errOlderHome != nil {
		t.Fatal(errOlderHome)
	}
	newerHome, errNewerHome := repo.RegisterHomeIncarnation(ctx, "10.0.0.2", 8317, nil)
	if errNewerHome != nil {
		t.Fatal(errNewerHome)
	}
	older := NewCoordinator(repo, NodeIdentity{IP: olderHome.IP, Port: olderHome.Port, StartedAt: olderHome.StartedAt}, CoordinatorOptions{HeartbeatTimeout: 20 * time.Second})
	newer := NewCoordinator(repo, NodeIdentity{IP: newerHome.IP, Port: newerHome.Port, StartedAt: newerHome.StartedAt}, CoordinatorOptions{HeartbeatTimeout: 20 * time.Second})

	if errBeat := older.heartbeatAndElect(ctx, olderHome); errBeat != nil {
		t.Fatal(errBeat)
	}
	if errBeat := newer.heartbeatAndElect(ctx, newerHome); errBeat != nil {
		t.Fatal(errBeat)
	}

	var masterCount int64
	if errCount := repo.db.Model(&ClusterNodeRecord{}).Where("is_master = ?", true).Count(&masterCount).Error; errCount != nil {
		t.Fatal(errCount)
	}
	if masterCount != 1 {
		t.Fatalf("persisted master count = %d, want 1", masterCount)
	}
	master, errMaster := newer.CurrentMaster(ctx)
	if errMaster != nil {
		t.Fatal(errMaster)
	}
	if master == nil || master.IP != older.node.IP || master.Port != older.node.Port {
		t.Fatalf("master = %#v, want older node", master)
	}
	if !older.IsMaster() || newer.IsMaster() {
		t.Fatalf("local master state older=%t newer=%t", older.IsMaster(), newer.IsMaster())
	}
}

func TestCoordinatorInitializeSyncsCPANodeSnapshot(t *testing.T) {
	ctx := context.Background()
	repo := newCredentialFoundationTestRepository(t)
	registry := node.GlobalRegistry()
	const (
		clientIP = "192.0.2.10"
		nodeID   = "cpa-initialize-snapshot"
	)
	registry.AddWithNodeID(clientIP, nodeID, time.Now().UTC())
	defer registry.RemoveWithNodeID(clientIP, nodeID)

	coordinator := NewCoordinator(repo, NodeIdentity{IP: "10.0.0.1", Port: 8317}, CoordinatorOptions{HeartbeatTimeout: 20 * time.Second})
	if errInitialize := coordinator.Initialize(ctx); errInitialize != nil {
		t.Fatal(errInitialize)
	}
	defer coordinator.setMaster(false)

	home, initialized := coordinator.HomeIncarnation()
	if !initialized {
		t.Fatal("coordinator is not initialized")
	}
	var snapshot CPANodeRecord
	if errFind := repo.db.First(&snapshot, "home_ip = ? AND home_port = ? AND home_started_at = ? AND node_key = ?", home.IP, home.Port, home.StartedAt, "node:"+nodeID).Error; errFind != nil {
		t.Fatal(errFind)
	}
	if snapshot.NodeID != nodeID || snapshot.ClientIP != clientIP || snapshot.ClientCount != 1 {
		t.Fatalf("snapshot = %#v", snapshot)
	}
}

func TestCurrentMasterRejectsExpiredPersistedMaster(t *testing.T) {
	ctx := context.Background()
	repo := newCredentialFoundationTestRepository(t)
	coordinator := NewCoordinator(repo, NodeIdentity{IP: "10.0.0.1", Port: 8317}, CoordinatorOptions{HeartbeatTimeout: time.Second})
	stale := time.Now().UTC().Add(-2 * time.Second)
	if errCreate := repo.db.Create(&ClusterNodeRecord{IP: coordinator.node.IP, Port: coordinator.node.Port, IsMaster: true, StartedAt: stale, LastSeenAt: stale}).Error; errCreate != nil {
		t.Fatal(errCreate)
	}
	master, errMaster := coordinator.CurrentMaster(ctx)
	if errMaster != nil {
		t.Fatal(errMaster)
	}
	if master != nil {
		t.Fatalf("CurrentMaster() = %#v, want nil", master)
	}
}

func TestLocalMasterLeaseExpiresWithoutHeartbeatRenewal(t *testing.T) {
	coordinator := NewCoordinator(&Repository{}, NodeIdentity{IP: "10.0.0.1", Port: 8317}, CoordinatorOptions{HeartbeatTimeout: 20 * time.Millisecond})
	demoted := make(chan struct{})
	coordinator.SetOnMasterChanged(func(master bool) {
		if !master {
			close(demoted)
		}
	})
	coordinator.setMaster(true)
	select {
	case <-demoted:
	case <-time.After(time.Second):
		t.Fatal("local master lease did not expire")
	}
	if coordinator.IsMaster() {
		t.Fatal("coordinator remained master after its local lease expired")
	}
}

func TestExpiredMasterCallbackCannotOverrideRenewedLease(t *testing.T) {
	coordinator := NewCoordinator(&Repository{}, NodeIdentity{IP: "10.0.0.1", Port: 8317}, CoordinatorOptions{HeartbeatTimeout: time.Hour})
	expiredStarted := make(chan struct{})
	releaseExpired := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseExpired) }) }
	t.Cleanup(func() {
		release()
		coordinator.setMaster(false)
	})

	var firstExpired sync.Once
	var callbackMu sync.Mutex
	callbackState := false
	coordinator.SetOnMasterChanged(func(master bool) {
		if !master {
			firstExpired.Do(func() {
				close(expiredStarted)
				<-releaseExpired
			})
		}
		callbackMu.Lock()
		callbackState = master
		callbackMu.Unlock()
	})
	coordinator.setMaster(true)

	coordinator.mu.Lock()
	lease := coordinator.masterLease
	if coordinator.masterTimer != nil {
		coordinator.masterTimer.Stop()
		coordinator.masterTimer = nil
	}
	coordinator.mu.Unlock()

	expireDone := make(chan struct{})
	go func() {
		coordinator.expireMasterLease(lease)
		close(expireDone)
	}()
	select {
	case <-expiredStarted:
	case <-time.After(time.Second):
		t.Fatal("expired lease callback did not start")
	}

	renewDone := make(chan struct{})
	go func() {
		coordinator.setMaster(true)
		close(renewDone)
	}()
	deadline := time.After(time.Second)
	for !coordinator.IsMaster() {
		select {
		case <-deadline:
			t.Fatal("master lease was not renewed")
		case <-time.After(time.Millisecond):
		}
	}

	release()
	for name, done := range map[string]<-chan struct{}{"expiry": expireDone, "renewal": renewDone} {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatalf("%s callback did not complete", name)
		}
	}
	callbackMu.Lock()
	gotState := callbackState
	callbackMu.Unlock()
	if !gotState {
		t.Fatal("stale expiry callback overrode the renewed master callback")
	}
}

func TestRepeatedDemotionCannotSuppressLeaseExpiryCallback(t *testing.T) {
	coordinator := NewCoordinator(&Repository{}, NodeIdentity{IP: "10.0.0.1", Port: 8317}, CoordinatorOptions{HeartbeatTimeout: time.Hour})
	demoted := make(chan struct{}, 1)
	coordinator.SetOnMasterChanged(func(master bool) {
		if !master {
			demoted <- struct{}{}
		}
	})
	coordinator.setMaster(true)

	coordinator.masterCallbackMu.Lock()
	coordinator.mu.Lock()
	lease := coordinator.masterLease
	if coordinator.masterTimer != nil {
		coordinator.masterTimer.Stop()
		coordinator.masterTimer = nil
	}
	coordinator.mu.Unlock()

	expireDone := make(chan struct{})
	go func() {
		coordinator.expireMasterLease(lease)
		close(expireDone)
	}()
	deadline := time.After(time.Second)
	for coordinator.IsMaster() {
		select {
		case <-deadline:
			coordinator.masterCallbackMu.Unlock()
			t.Fatal("master lease did not expire")
		case <-time.After(time.Millisecond):
		}
	}

	coordinator.setMaster(false)
	coordinator.masterCallbackMu.Unlock()

	select {
	case <-demoted:
	case <-time.After(time.Second):
		t.Fatal("repeated demotion suppressed the lease expiry callback")
	}
	select {
	case <-expireDone:
	case <-time.After(time.Second):
		t.Fatal("lease expiry did not complete")
	}
}
