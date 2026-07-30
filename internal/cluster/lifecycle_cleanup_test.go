package cluster

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
)

func TestSubscribeMembershipRejectsInactiveRequestHome(t *testing.T) {
	ctx := context.Background()
	repo := newCredentialFoundationTestRepository(t)
	home, errHome := repo.RegisterHomeIncarnation(ctx, "10.0.0.1", 8317, []string{"credential_concurrency_foundation_v1"})
	if errHome != nil {
		t.Fatal(errHome)
	}
	if errRetire := repo.RetireHomeIncarnation(ctx, home); errRetire != nil {
		t.Fatal(errRetire)
	}
	revision, errUpdate := repo.UpdateLifecycleConfig(ctx, 20*time.Second, config.DefaultCredentialConcurrencyConfig())
	if errUpdate != nil {
		t.Fatal(errUpdate)
	}

	_, errSubscribe := repo.SubscribeMembership(ctx, SubscribeMembershipRequest{
		Fingerprint: "fp-inactive-home", NodeID: "cpa-a", Home: home, ProtocolVersion: 1, LifecycleConfigRevision: revision,
	})
	if !errors.Is(errSubscribe, ErrHomeIncarnationInactive) {
		t.Fatalf("SubscribeMembership() error = %v, want %v", errSubscribe, ErrHomeIncarnationInactive)
	}
}

func TestLifecycleConfigDoesNotCreateDefaultWithUnknownNodeTimeout(t *testing.T) {
	repo := newCredentialFoundationTestRepository(t)
	if _, errLifecycle := repo.LifecycleConfig(context.Background()); errLifecycle == nil {
		t.Fatal("LifecycleConfig() created a default lifecycle row without the configured node timeout")
	}
}

func TestCoordinatorRunsLifecycleCleanupContinuously(t *testing.T) {
	ctx := context.Background()
	repo := newCredentialFoundationTestRepository(t)
	cfg := config.DefaultCredentialConcurrencyConfig()
	cfg.CleanupInterval = 5 * time.Millisecond
	if _, errUpdate := repo.UpdateLifecycleConfig(ctx, 20*time.Second, cfg); errUpdate != nil {
		t.Fatal(errUpdate)
	}
	var recoveries atomic.Int32
	startCtx, cancelStart := context.WithTimeout(ctx, 2*time.Second)
	defer cancelStart()
	coordinator := NewCoordinator(repo, NodeIdentity{IP: "10.0.0.1", Port: 8317}, CoordinatorOptions{
		HeartbeatInterval: 5 * time.Millisecond,
		HeartbeatTimeout:  20 * time.Second,
		StartupRecovery: func(context.Context, HomeIncarnationID) error {
			if recoveries.Add(1) > 1 {
				cancelStart()
			}
			return nil
		},
	})
	if errInitialize := coordinator.Initialize(ctx); errInitialize != nil {
		t.Fatal(errInitialize)
	}
	if errStart := coordinator.Start(startCtx); errStart != nil {
		t.Fatal(errStart)
	}
	if recoveries.Load() < 2 {
		t.Fatalf("recovery runs = %d, want startup plus continuous cleanup", recoveries.Load())
	}
}

func TestCoordinatorLifecycleCleanupOwnsCPANodeSnapshotRetention(t *testing.T) {
	ctx := context.Background()
	repo := newCredentialFoundationTestRepository(t)
	coordinator := NewCoordinator(repo, NodeIdentity{IP: "10.0.0.1", Port: 8317}, CoordinatorOptions{
		HeartbeatTimeout: 20 * time.Second,
	})
	if errInitialize := coordinator.Initialize(ctx); errInitialize != nil {
		t.Fatal(errInitialize)
	}
	defer coordinator.setMaster(false)

	databaseNow, errNow := DatabaseNow(ctx, repo.db)
	if errNow != nil {
		t.Fatal(errNow)
	}
	expired := CPANodeRecord{
		HomeIP:                 "10.0.0.2",
		HomePort:               8317,
		HomeStartedAt:          databaseNow.Add(-time.Hour),
		NodeKey:                "fingerprint:expired",
		NodeID:                 "cpa-expired",
		CertificateFingerprint: "expired",
		ClientCount:            1,
		ConnectedAt:            databaseNow.Add(-time.Hour),
		LastSeenAt:             databaseNow.Add(-cpaNodeSnapshotRetention(coordinator.heartbeatTimeout) - time.Second),
	}
	if errCreate := repo.db.Create(&expired).Error; errCreate != nil {
		t.Fatal(errCreate)
	}

	if errUpdate := coordinator.UpdateClientCount(ctx, 0); errUpdate != nil {
		t.Fatal(errUpdate)
	}
	var countAfterUpdate int64
	if errCount := repo.db.Model(&CPANodeRecord{}).Where("node_key = ?", expired.NodeKey).Count(&countAfterUpdate).Error; errCount != nil {
		t.Fatal(errCount)
	}
	if countAfterUpdate != 1 {
		t.Fatalf("snapshot count after UpdateClientCount() = %d, want 1", countAfterUpdate)
	}

	if errCleanup := coordinator.runLifecycleCleanup(ctx); errCleanup != nil {
		t.Fatal(errCleanup)
	}
	var countAfterCleanup int64
	if errCount := repo.db.Model(&CPANodeRecord{}).Where("node_key = ?", expired.NodeKey).Count(&countAfterCleanup).Error; errCount != nil {
		t.Fatal(errCount)
	}
	if countAfterCleanup != 0 {
		t.Fatalf("snapshot count after runLifecycleCleanup() = %d, want 0", countAfterCleanup)
	}
}

func TestCoordinatorReloadsLifecycleCleanupInterval(t *testing.T) {
	ctx := context.Background()
	repo := newCredentialFoundationTestRepository(t)
	cfg := config.DefaultCredentialConcurrencyConfig()
	cfg.CleanupInterval = 5 * time.Millisecond
	if _, errUpdate := repo.UpdateLifecycleConfig(ctx, 20*time.Second, cfg); errUpdate != nil {
		t.Fatal(errUpdate)
	}

	var recoveries atomic.Int32
	intervalUpdated := make(chan error, 1)
	coordinator := NewCoordinator(repo, NodeIdentity{IP: "10.0.0.1", Port: 8317}, CoordinatorOptions{
		HeartbeatInterval: 5 * time.Millisecond,
		HeartbeatTimeout:  20 * time.Second,
		StartupRecovery: func(context.Context, HomeIncarnationID) error {
			if recoveries.Add(1) == 2 {
				next := cfg
				next.CleanupInterval = 100 * time.Millisecond
				_, errUpdate := repo.UpdateLifecycleConfig(ctx, 20*time.Second, next)
				intervalUpdated <- errUpdate
			}
			return nil
		},
	})
	if errInitialize := coordinator.Initialize(ctx); errInitialize != nil {
		t.Fatal(errInitialize)
	}

	startCtx, cancelStart := context.WithCancel(ctx)
	startDone := make(chan error, 1)
	go func() {
		startDone <- coordinator.Start(startCtx)
	}()
	select {
	case errUpdate := <-intervalUpdated:
		if errUpdate != nil {
			cancelStart()
			<-startDone
			t.Fatal(errUpdate)
		}
	case <-time.After(time.Second):
		cancelStart()
		<-startDone
		t.Fatal("cleanup did not run at the initial lifecycle interval")
	}

	time.Sleep(30 * time.Millisecond)
	if got := recoveries.Load(); got != 2 {
		cancelStart()
		<-startDone
		t.Fatalf("recovery runs = %d, want 2 after lifecycle interval update", got)
	}
	cancelStart()
	if errStart := <-startDone; errStart != nil {
		t.Fatal(errStart)
	}
}

func TestCoordinatorCleanupErrorDoesNotStopHeartbeat(t *testing.T) {
	ctx := context.Background()
	repo := newCredentialFoundationTestRepository(t)
	cfg := config.DefaultCredentialConcurrencyConfig()
	cfg.CleanupInterval = 5 * time.Millisecond
	if _, errUpdate := repo.UpdateLifecycleConfig(ctx, 20*time.Second, cfg); errUpdate != nil {
		t.Fatal(errUpdate)
	}
	coordinator := NewCoordinator(repo, NodeIdentity{IP: "10.0.0.1", Port: 8317}, CoordinatorOptions{
		HeartbeatInterval: 5 * time.Millisecond,
		HeartbeatTimeout:  20 * time.Second,
	})
	if errInitialize := coordinator.Initialize(ctx); errInitialize != nil {
		t.Fatal(errInitialize)
	}
	if errCreate := repo.db.Create(&CPANodeMembershipRecord{
		CertificateFingerprint: "fp-cleanup-error",
		NodeID:                 "cpa-a",
		HomeIP:                 "missing-home",
		HomePort:               8317,
		HomeStartedAt:          time.Unix(0, 0).UTC(),
		State:                  MembershipStateActive,
		CPAHeartbeatTimeout:    time.Nanosecond,
		LastSeenAt:             time.Unix(0, 0).UTC(),
	}).Error; errCreate != nil {
		t.Fatal(errCreate)
	}

	startCtx, cancelStart := context.WithCancel(ctx)
	defer cancelStart()
	startDone := make(chan error, 1)
	go func() {
		startDone <- coordinator.Start(startCtx)
	}()
	select {
	case errStart := <-startDone:
		t.Fatalf("Start() returned after cleanup error: %v", errStart)
	case <-time.After(50 * time.Millisecond):
	}

	home, initialized := coordinator.HomeIncarnation()
	if !initialized {
		t.Fatal("coordinator is not initialized")
	}
	if errFence := repo.FenceHomeIncarnation(ctx, home, "test heartbeat"); errFence != nil {
		t.Fatal(errFence)
	}
	if errStart := <-startDone; !errors.Is(errStart, ErrHomeIncarnationFenced) {
		t.Fatalf("Start() error = %v, want %v", errStart, ErrHomeIncarnationFenced)
	}
}

func TestCoordinatorHeartbeatAdvancesWhileCleanupRecoveryIsBlocked(t *testing.T) {
	ctx := context.Background()
	repo := newCredentialFoundationTestRepository(t)
	cfg := config.DefaultCredentialConcurrencyConfig()
	cfg.CleanupInterval = 5 * time.Millisecond
	if _, errUpdate := repo.UpdateLifecycleConfig(ctx, 20*time.Second, cfg); errUpdate != nil {
		t.Fatal(errUpdate)
	}

	cleanupEntered := make(chan struct{})
	releaseCleanup := make(chan struct{})
	var recoveries atomic.Int32
	coordinator := NewCoordinator(repo, NodeIdentity{IP: "10.0.0.1", Port: 8317}, CoordinatorOptions{
		HeartbeatInterval: 5 * time.Millisecond,
		HeartbeatTimeout:  20 * time.Second,
		StartupRecovery: func(context.Context, HomeIncarnationID) error {
			if recoveries.Add(1) == 2 {
				close(cleanupEntered)
				<-releaseCleanup
			}
			return nil
		},
	})
	if errInitialize := coordinator.Initialize(ctx); errInitialize != nil {
		t.Fatal(errInitialize)
	}
	home, initialized := coordinator.HomeIncarnation()
	if !initialized {
		t.Fatal("coordinator is not initialized")
	}

	startCtx, cancelStart := context.WithCancel(ctx)
	startDone := make(chan error, 1)
	go func() {
		startDone <- coordinator.Start(startCtx)
	}()
	select {
	case <-cleanupEntered:
	case <-time.After(time.Second):
		cancelStart()
		<-startDone
		t.Fatal("cleanup recovery did not block")
	}

	var before HomeProcessIncarnationRecord
	if errFind := repo.db.First(&before, "home_ip = ? AND home_port = ? AND started_at = ?", home.IP, home.Port, home.StartedAt).Error; errFind != nil {
		close(releaseCleanup)
		cancelStart()
		<-startDone
		t.Fatal(errFind)
	}
	time.Sleep(time.Second + 4*coordinator.heartbeatInterval)
	var after HomeProcessIncarnationRecord
	if errFind := repo.db.First(&after, "home_ip = ? AND home_port = ? AND started_at = ?", home.IP, home.Port, home.StartedAt).Error; errFind != nil {
		close(releaseCleanup)
		cancelStart()
		<-startDone
		t.Fatal(errFind)
	}
	if !after.LastSeenAt.After(before.LastSeenAt) {
		close(releaseCleanup)
		cancelStart()
		<-startDone
		t.Fatalf("Home last_seen_at = %s, want after %s while cleanup is blocked", after.LastSeenAt, before.LastSeenAt)
	}

	close(releaseCleanup)
	cancelStart()
	if errStart := <-startDone; errStart != nil {
		t.Fatal(errStart)
	}
}

func TestCleanupStaleMembershipBeginsCancellationAndRecoveryHonorsReclaimGrace(t *testing.T) {
	ctx := context.Background()
	repo, owner, member := newQuiescenceMembership(t, ctx, "fp-cleanup-grace")
	participant, errParticipant := repo.RegisterHomeIncarnation(ctx, "10.0.0.2", 8317, []string{"credential_concurrency_foundation_v1"})
	if errParticipant != nil {
		t.Fatal(errParticipant)
	}
	if errParticipation := repo.RecordParticipation(ctx, ConnectionLifetime{Fingerprint: member.CertificateFingerprint, ConnectedAt: member.ConnectedAt, Home: participant}); errParticipation != nil {
		t.Fatal(errParticipation)
	}
	if errUpdate := repo.db.Model(&CPANodeMembershipRecord{}).Where("certificate_fingerprint = ?", member.CertificateFingerprint).Update("last_seen_at", time.Unix(0, 0).UTC()).Error; errUpdate != nil {
		t.Fatal(errUpdate)
	}
	if errUpdate := repo.db.Model(&HomeProcessIncarnationRecord{}).Where("home_ip = ? AND home_port = ? AND started_at = ?", participant.IP, participant.Port, participant.StartedAt).Updates(map[string]any{
		"state": HomeIncarnationExpired, "last_seen_at": time.Now().UTC(),
	}).Error; errUpdate != nil {
		t.Fatal(errUpdate)
	}

	if errCleanup := repo.CleanupStaleMemberships(ctx); errCleanup != nil {
		t.Fatal(errCleanup)
	}
	var canceling CPANodeMembershipRecord
	if errFind := repo.db.First(&canceling, "certificate_fingerprint = ?", member.CertificateFingerprint).Error; errFind != nil {
		t.Fatal(errFind)
	}
	if canceling.State != MembershipStateCanceling {
		t.Fatalf("membership state = %q, want %q", canceling.State, MembershipStateCanceling)
	}
	if errRecover := repo.RecoverStaleQuiescence(ctx); errRecover != nil {
		t.Fatal(errRecover)
	}
	var pending CPANodeQuiescenceRecord
	if errFind := repo.db.Where("certificate_fingerprint = ? AND home_ip = ? AND home_port = ? AND home_started_at = ?", member.CertificateFingerprint, participant.IP, participant.Port, participant.StartedAt).First(&pending).Error; errFind != nil {
		t.Fatal(errFind)
	}
	if pending.Status != QuiescenceStatusPending {
		t.Fatalf("grace-period quiescence status = %q, want %q (owner=%#v)", pending.Status, QuiescenceStatusPending, owner)
	}
}

func TestRecoveryFencesPostGraceAndAllowsExactLifetimeToReopen(t *testing.T) {
	ctx := context.Background()
	repo, owner, member := newQuiescenceMembership(t, ctx, "fp-cleanup-complete")
	participant, errParticipant := repo.RegisterHomeIncarnation(ctx, "10.0.0.2", 8317, []string{"credential_concurrency_foundation_v1"})
	if errParticipant != nil {
		t.Fatal(errParticipant)
	}
	if errParticipation := repo.RecordParticipation(ctx, ConnectionLifetime{Fingerprint: member.CertificateFingerprint, ConnectedAt: member.ConnectedAt, Home: participant}); errParticipation != nil {
		t.Fatal(errParticipation)
	}
	if errUpdate := repo.db.Model(&HomeProcessIncarnationRecord{}).Where("home_ip = ? AND home_port = ? AND started_at = ?", participant.IP, participant.Port, participant.StartedAt).Updates(map[string]any{
		"state": HomeIncarnationExpired, "last_seen_at": time.Unix(0, 0).UTC(),
	}).Error; errUpdate != nil {
		t.Fatal(errUpdate)
	}
	revision, errBegin := repo.BeginFingerprintCancellation(ctx, member.CertificateFingerprint)
	if errBegin != nil {
		t.Fatal(errBegin)
	}
	if errAck := repo.AcknowledgeQuiescence(ctx, member.CertificateFingerprint, member.ConnectedAt, revision, owner); errAck != nil {
		t.Fatal(errAck)
	}
	if errRecover := repo.RecoverStaleQuiescence(ctx); errRecover != nil {
		t.Fatal(errRecover)
	}
	var closed CPANodeMembershipRecord
	if errFind := repo.db.First(&closed, "certificate_fingerprint = ?", member.CertificateFingerprint).Error; errFind != nil {
		t.Fatal(errFind)
	}
	if closed.State != MembershipStateClosed {
		t.Fatalf("membership state = %q, want %q", closed.State, MembershipStateClosed)
	}
	var participations int64
	if errCount := repo.db.Model(&CPANodeParticipationRecord{}).Where("certificate_fingerprint = ? AND membership_connected_at = ?", member.CertificateFingerprint, member.ConnectedAt).Count(&participations).Error; errCount != nil {
		t.Fatal(errCount)
	}
	if participations != 0 {
		t.Fatalf("participations = %d, want 0", participations)
	}
	lifecycle, errLifecycle := repo.LifecycleConfig(ctx)
	if errLifecycle != nil {
		t.Fatal(errLifecycle)
	}
	reopened, errSubscribe := repo.SubscribeMembership(ctx, SubscribeMembershipRequest{Fingerprint: member.CertificateFingerprint, NodeID: "cpa-b", Home: owner, ProtocolVersion: 1, LifecycleConfigRevision: lifecycle.LifecycleConfigRevision})
	if errSubscribe != nil {
		t.Fatal(errSubscribe)
	}
	if !reopened.ConnectedAt.After(member.ConnectedAt) {
		t.Fatalf("reopened lifetime = %s, want after %s", reopened.ConnectedAt, member.ConnectedAt)
	}
}
