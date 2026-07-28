package cluster

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"gorm.io/gorm"
)

const testMembershipInstanceID = "550e8400-e29b-41d4-a716-446655440000"

func TestSubscribeMembershipRejectsDifferentNodeAndAllowsSameNodeTakeover(t *testing.T) {
	ctx := context.Background()
	repo := newCredentialFoundationTestRepository(t)
	homeA, errHomeA := repo.RegisterHomeIncarnation(ctx, "10.0.0.1", 8317, []string{"credential_concurrency_foundation_v1"})
	if errHomeA != nil {
		t.Fatal(errHomeA)
	}
	homeB, errHomeB := repo.RegisterHomeIncarnation(ctx, "10.0.0.2", 8317, []string{"credential_concurrency_foundation_v1"})
	if errHomeB != nil {
		t.Fatal(errHomeB)
	}
	revision, errUpdate := repo.UpdateLifecycleConfig(ctx, 20*time.Second, config.DefaultCredentialConcurrencyConfig())
	if errUpdate != nil {
		t.Fatal(errUpdate)
	}
	first, errFirst := repo.SubscribeMembership(ctx, SubscribeMembershipRequest{Fingerprint: "fp-a", NodeID: "cpa-a", Home: homeA, ProtocolVersion: 1, LifecycleConfigRevision: revision})
	if errFirst != nil {
		t.Fatal(errFirst)
	}
	counter := CredentialConcurrencyCounterRecord{CredentialID: "cred-a", Model: "model-a", CertificateFingerprint: first.CertificateFingerprint, ActiveCount: 1, UpdatedAt: time.Now().UTC()}
	if errCreate := repo.db.Create(&counter).Error; errCreate != nil {
		t.Fatal(errCreate)
	}
	if _, errDuplicate := repo.SubscribeMembership(ctx, SubscribeMembershipRequest{Fingerprint: "fp-a", NodeID: "cpa-b", Home: homeB, ProtocolVersion: 1, LifecycleConfigRevision: revision}); !errors.Is(errDuplicate, ErrDuplicateCPACertificate) {
		t.Fatalf("duplicate error = %v", errDuplicate)
	}
	if _, errNoTakeover := repo.SubscribeMembership(ctx, SubscribeMembershipRequest{Fingerprint: "fp-a", NodeID: "cpa-a", Home: homeB, ProtocolVersion: 1, LifecycleConfigRevision: revision}); !errors.Is(errNoTakeover, ErrDuplicateCPACertificate) {
		t.Fatalf("same-node request without takeover error = %v", errNoTakeover)
	}
	if _, errDifferentNode := repo.SubscribeMembership(ctx, SubscribeMembershipRequest{Fingerprint: "fp-a", NodeID: "cpa-b", Home: homeB, ProtocolVersion: 1, LifecycleConfigRevision: revision, Takeover: true}); !errors.Is(errDifferentNode, ErrDuplicateCPACertificate) {
		t.Fatalf("different-node takeover error = %v", errDifferentNode)
	}
	persistedBeforeTakeover := CPANodeMembershipRecord{}
	if errPersisted := repo.db.First(&persistedBeforeTakeover, "certificate_fingerprint = ?", first.CertificateFingerprint).Error; errPersisted != nil {
		t.Fatal(errPersisted)
	}
	if !persistedBeforeTakeover.ConnectedAt.Equal(first.ConnectedAt) {
		t.Fatalf("failed takeover changed membership: %#v", persistedBeforeTakeover)
	}
	second, errSecond := repo.SubscribeMembership(ctx, SubscribeMembershipRequest{Fingerprint: "fp-a", NodeID: "cpa-a", Home: homeB, ProtocolVersion: 1, LifecycleConfigRevision: revision, Takeover: true})
	if errSecond != nil {
		t.Fatal(errSecond)
	}
	if !second.ConnectedAt.After(first.ConnectedAt) {
		t.Fatalf("connected_at did not increase: first=%s second=%s", first.ConnectedAt, second.ConnectedAt)
	}
	if second.HomeIP != homeB.IP || second.HomePort != homeB.Port || !second.HomeStartedAt.Equal(homeB.StartedAt) {
		t.Fatalf("takeover owner = %s:%d %s, want %#v", second.HomeIP, second.HomePort, second.HomeStartedAt, homeB)
	}
	oldLifetime := ConnectionLifetime{Fingerprint: first.CertificateFingerprint, ConnectedAt: first.ConnectedAt, Home: homeA, Controlled: true}
	newLifetime := ConnectionLifetime{Fingerprint: second.CertificateFingerprint, ConnectedAt: second.ConnectedAt, Home: homeB, Controlled: true}
	if _, errCancel := repo.BeginFingerprintCancellationForLifetime(ctx, oldLifetime); !errors.Is(errCancel, ErrMembershipNotActive) {
		t.Fatalf("stale lifetime cancellation error = %v, want %v", errCancel, ErrMembershipNotActive)
	}
	var counterCount int64
	if errCount := repo.db.Model(&CredentialConcurrencyCounterRecord{}).Where("certificate_fingerprint = ?", first.CertificateFingerprint).Count(&counterCount).Error; errCount != nil {
		t.Fatal(errCount)
	}
	if counterCount != 1 {
		t.Fatalf("counter count after takeover = %d, want 1", counterCount)
	}
	errOld := withConcurrencyTransaction(ctx, repo.db, func(tx *gorm.DB) error {
		return repo.LockActiveConcurrencyLifetimeTx(ctx, tx, oldLifetime)
	})
	if !errors.Is(errOld, ErrConcurrencyNodeUnavailable) {
		t.Fatalf("old lifetime error = %v, want %v", errOld, ErrConcurrencyNodeUnavailable)
	}
	if errNew := withConcurrencyTransaction(ctx, repo.db, func(tx *gorm.DB) error {
		return repo.LockActiveConcurrencyLifetimeTx(ctx, tx, newLifetime)
	}); errNew != nil {
		t.Fatalf("new lifetime error = %v", errNew)
	}
	oldClassified, errClassify := repo.ClassifyConnection(ctx, first.CertificateFingerprint, homeA)
	if errClassify != nil {
		t.Fatal(errClassify)
	}
	newClassified, errClassify := repo.ClassifyConnection(ctx, second.CertificateFingerprint, homeB)
	if errClassify != nil {
		t.Fatal(errClassify)
	}
	if oldClassified.Controlled || !newClassified.Controlled || !newClassified.ConnectedAt.Equal(second.ConnectedAt) {
		t.Fatalf("classified lifetimes old=%#v new=%#v", oldClassified, newClassified)
	}
	var activeCount int64
	if errCount := repo.db.Model(&CPANodeMembershipRecord{}).Where("certificate_fingerprint = ? AND state = ?", first.CertificateFingerprint, MembershipStateActive).Count(&activeCount).Error; errCount != nil {
		t.Fatal(errCount)
	}
	if activeCount != 1 {
		t.Fatalf("active membership count = %d, want 1", activeCount)
	}
	if _, errCancel := repo.BeginFingerprintCancellationForLifetime(ctx, newLifetime); errCancel != nil {
		t.Fatalf("current lifetime cancellation error = %v", errCancel)
	}
	assertMembershipState(t, repo, second.CertificateFingerprint, MembershipStateCanceling)
	if errClose := repo.db.Model(&CPANodeMembershipRecord{}).Where("certificate_fingerprint = ?", second.CertificateFingerprint).Update("state", MembershipStateClosed).Error; errClose != nil {
		t.Fatal(errClose)
	}
	if _, errTakeover := repo.SubscribeMembership(ctx, SubscribeMembershipRequest{Fingerprint: "fp-a", NodeID: "cpa-a", Home: homeB, ProtocolVersion: 1, LifecycleConfigRevision: revision, Takeover: true}); !errors.Is(errTakeover, ErrMembershipTakeoverUnavailable) {
		t.Fatalf("closed membership takeover error = %v, want %v", errTakeover, ErrMembershipTakeoverUnavailable)
	}
	if _, errTakeover := repo.SubscribeMembership(ctx, SubscribeMembershipRequest{Fingerprint: "fp-missing", NodeID: "cpa-a", Home: homeB, ProtocolVersion: 1, LifecycleConfigRevision: revision, Takeover: true}); !errors.Is(errTakeover, ErrMembershipTakeoverUnavailable) {
		t.Fatalf("missing membership takeover error = %v, want %v", errTakeover, ErrMembershipTakeoverUnavailable)
	}
	_, errReopen := repo.SubscribeMembership(ctx, SubscribeMembershipRequest{Fingerprint: "fp-a", NodeID: "cpa-a", Home: homeB, ProtocolVersion: 1, LifecycleConfigRevision: revision})
	if errReopen != nil {
		t.Fatalf("normal closed membership reopen error = %v", errReopen)
	}
}

func TestRESPHandlerKeepsMembershipInstanceIDRuntimeOnly(t *testing.T) {
	ctx := context.Background()
	repo := newCredentialFoundationTestRepository(t)
	home, errHome := repo.RegisterHomeIncarnation(ctx, "10.0.0.1", 8317, []string{"credential_concurrency_foundation_v1"})
	if errHome != nil {
		t.Fatal(errHome)
	}
	revision, errUpdate := repo.UpdateLifecycleConfig(ctx, 20*time.Second, config.DefaultCredentialConcurrencyConfig())
	if errUpdate != nil {
		t.Fatal(errUpdate)
	}
	coordinator := NewCoordinator(repo, NodeIdentity{IP: home.IP, Port: home.Port}, CoordinatorOptions{})
	coordinator.mu.Lock()
	coordinator.homeIncarnation = home
	coordinator.initialized = true
	coordinator.mu.Unlock()

	lifetime, errSubscribe := NewRESPHandler(coordinator, nil, repo).SubscribeMembership(ctx, "fp-runtime", "cpa-runtime", 1, revision, false, testMembershipInstanceID)
	if errSubscribe != nil {
		t.Fatal(errSubscribe)
	}
	if lifetime.InstanceID != testMembershipInstanceID {
		t.Fatalf("runtime instance ID = %q, want %q", lifetime.InstanceID, testMembershipInstanceID)
	}
	if repo.db.Migrator().HasColumn(&CPANodeMembershipRecord{}, "instance_id") {
		t.Fatal("membership instance ID was persisted as a database column")
	}
}

func TestSubscribeMembershipKeepsTakeoverLivenessAtDatabaseTime(t *testing.T) {
	ctx := context.Background()
	repo := newCredentialFoundationTestRepository(t)
	home, errHome := repo.RegisterHomeIncarnation(ctx, "10.0.0.1", 8317, []string{"credential_concurrency_foundation_v1"})
	if errHome != nil {
		t.Fatal(errHome)
	}
	revision, errUpdate := repo.UpdateLifecycleConfig(ctx, 20*time.Second, config.DefaultCredentialConcurrencyConfig())
	if errUpdate != nil {
		t.Fatal(errUpdate)
	}
	first, errFirst := repo.SubscribeMembership(ctx, SubscribeMembershipRequest{Fingerprint: "fp-a", NodeID: "cpa-a", Home: home, ProtocolVersion: 1, LifecycleConfigRevision: revision})
	if errFirst != nil {
		t.Fatal(errFirst)
	}

	futureConnectedAt := first.ConnectedAt.Add(10 * time.Minute)
	if errFuture := repo.db.Model(&CPANodeMembershipRecord{}).
		Where("certificate_fingerprint = ?", first.CertificateFingerprint).
		Update("connected_at", futureConnectedAt).Error; errFuture != nil {
		t.Fatal(errFuture)
	}
	dbNowBefore, errNow := DatabaseNow(ctx, repo.db)
	if errNow != nil {
		t.Fatal(errNow)
	}

	takenOver, errTakeover := repo.SubscribeMembership(ctx, SubscribeMembershipRequest{Fingerprint: "fp-a", NodeID: "cpa-a", Home: home, ProtocolVersion: 1, LifecycleConfigRevision: revision, Takeover: true})
	if errTakeover != nil {
		t.Fatal(errTakeover)
	}
	dbNowAfter, errNow := DatabaseNow(ctx, repo.db)
	if errNow != nil {
		t.Fatal(errNow)
	}
	persisted := CPANodeMembershipRecord{}
	if errPersisted := repo.db.First(&persisted, "certificate_fingerprint = ?", takenOver.CertificateFingerprint).Error; errPersisted != nil {
		t.Fatal(errPersisted)
	}

	if !takenOver.ConnectedAt.After(futureConnectedAt) {
		t.Fatalf("takeover connected_at = %s, want after %s", takenOver.ConnectedAt, futureConnectedAt)
	}
	if !persisted.ConnectedAt.Equal(takenOver.ConnectedAt) {
		t.Fatalf("persisted connected_at = %s, want %s", persisted.ConnectedAt, takenOver.ConnectedAt)
	}
	if persisted.LastSeenAt.Before(dbNowBefore) || persisted.LastSeenAt.After(dbNowAfter) {
		t.Fatalf("persisted last_seen_at = %s, want database time between %s and %s", persisted.LastSeenAt, dbNowBefore, dbNowAfter)
	}
	if !persisted.LastSeenAt.Before(persisted.ConnectedAt) {
		t.Fatalf("takeover liveness time %s was advanced to lifetime identity %s", persisted.LastSeenAt, persisted.ConnectedAt)
	}
}

func TestClassifyConnectionOnlyControlsMembershipOwnerHome(t *testing.T) {
	ctx := context.Background()
	repo := newCredentialFoundationTestRepository(t)
	owner, errOwner := repo.RegisterHomeIncarnation(ctx, "10.0.0.1", 8317, []string{"credential_concurrency_foundation_v1"})
	if errOwner != nil {
		t.Fatal(errOwner)
	}
	commandHome, errCommandHome := repo.RegisterHomeIncarnation(ctx, "10.0.0.2", 8317, []string{"credential_concurrency_foundation_v1"})
	if errCommandHome != nil {
		t.Fatal(errCommandHome)
	}
	revision, errUpdate := repo.UpdateLifecycleConfig(ctx, 20*time.Second, config.DefaultCredentialConcurrencyConfig())
	if errUpdate != nil {
		t.Fatal(errUpdate)
	}
	member, errSubscribe := repo.SubscribeMembership(ctx, SubscribeMembershipRequest{Fingerprint: "fp-a", NodeID: "cpa-a", Home: owner, ProtocolVersion: 1, LifecycleConfigRevision: revision})
	if errSubscribe != nil {
		t.Fatal(errSubscribe)
	}

	lifetime, errClassify := repo.ClassifyConnection(ctx, "fp-a", commandHome)
	if errClassify != nil {
		t.Fatal(errClassify)
	}
	if lifetime.Controlled || lifetime.Home != commandHome || !lifetime.ConnectedAt.IsZero() {
		t.Fatalf("lifetime = %#v", lifetime)
	}
	var participationCount int64
	if errCount := repo.db.Model(&CPANodeParticipationRecord{}).Where("certificate_fingerprint = ? AND membership_connected_at = ? AND home_ip = ? AND home_port = ? AND home_started_at = ?", "fp-a", member.ConnectedAt, commandHome.IP, commandHome.Port, commandHome.StartedAt).Count(&participationCount).Error; errCount != nil {
		t.Fatal(errCount)
	}
	if participationCount != 0 {
		t.Fatalf("non-owner participation count = %d, want 0", participationCount)
	}

	lifetime, errClassify = repo.ClassifyConnection(ctx, "fp-a", owner)
	if errClassify != nil {
		t.Fatal(errClassify)
	}
	if !lifetime.Controlled || lifetime.Home != owner || !lifetime.ConnectedAt.Equal(member.ConnectedAt) {
		t.Fatalf("owner lifetime = %#v", lifetime)
	}
}

func TestClassifyConnectionReturnsMembershipQueryError(t *testing.T) {
	ctx := context.Background()
	repo := newCredentialFoundationTestRepository(t)
	queryErr := errors.New("membership query failed")
	if errCallback := repo.db.Callback().Query().Before("gorm:query").Register("test:classify_membership_query_error", func(tx *gorm.DB) {
		if tx.Statement.Table == (CPANodeMembershipRecord{}).TableName() {
			tx.AddError(queryErr)
		}
	}); errCallback != nil {
		t.Fatal(errCallback)
	}

	_, errClassify := repo.ClassifyConnection(ctx, "fp-a", HomeIncarnationID{IP: "10.0.0.1", Port: 8317, StartedAt: time.Unix(1, 0).UTC()})
	if !errors.Is(errClassify, queryErr) {
		t.Fatalf("ClassifyConnection() error = %v, want %v", errClassify, queryErr)
	}
}

func TestRecordParticipationReturnsMembershipQueryError(t *testing.T) {
	ctx := context.Background()
	repo := newCredentialFoundationTestRepository(t)
	queryErr := errors.New("membership query failed")
	if errCallback := repo.db.Callback().Query().Before("gorm:query").Register("test:participation_membership_query_error", func(tx *gorm.DB) {
		if tx.Statement.Table == (CPANodeMembershipRecord{}).TableName() {
			tx.AddError(queryErr)
		}
	}); errCallback != nil {
		t.Fatal(errCallback)
	}

	errParticipation := repo.RecordParticipation(ctx, ConnectionLifetime{
		Fingerprint: "fp-a",
		ConnectedAt: time.Unix(1, 0).UTC(),
		Home:        HomeIncarnationID{IP: "10.0.0.1", Port: 8317, StartedAt: time.Unix(1, 0).UTC()},
	})
	if !errors.Is(errParticipation, queryErr) {
		t.Fatalf("RecordParticipation() error = %v, want %v", errParticipation, queryErr)
	}
}

func TestRecordParticipationRequiresActiveHomeIncarnation(t *testing.T) {
	ctx := context.Background()
	repo := newCredentialFoundationTestRepository(t)
	home, errHome := repo.RegisterHomeIncarnation(ctx, "10.0.0.1", 8317, []string{"credential_concurrency_foundation_v1"})
	if errHome != nil {
		t.Fatal(errHome)
	}
	revision, errUpdate := repo.UpdateLifecycleConfig(ctx, 20*time.Second, config.DefaultCredentialConcurrencyConfig())
	if errUpdate != nil {
		t.Fatal(errUpdate)
	}
	member, errSubscribe := repo.SubscribeMembership(ctx, SubscribeMembershipRequest{Fingerprint: "fp-a", NodeID: "cpa-a", Home: home, ProtocolVersion: 1, LifecycleConfigRevision: revision})
	if errSubscribe != nil {
		t.Fatal(errSubscribe)
	}
	if errRetire := repo.RetireHomeIncarnation(ctx, home); errRetire != nil {
		t.Fatal(errRetire)
	}

	errParticipation := repo.RecordParticipation(ctx, ConnectionLifetime{Fingerprint: member.CertificateFingerprint, ConnectedAt: member.ConnectedAt, Home: home})
	if !errors.Is(errParticipation, ErrHomeIncarnationInactive) {
		t.Fatalf("RecordParticipation() error = %v, want %v", errParticipation, ErrHomeIncarnationInactive)
	}
}

func TestRecordParticipationLocksMembershipBeforeInserting(t *testing.T) {
	ctx := context.Background()
	repo := newCredentialFoundationTestRepository(t)
	home, errHome := repo.RegisterHomeIncarnation(ctx, "10.0.0.1", 8317, []string{"credential_concurrency_foundation_v1"})
	if errHome != nil {
		t.Fatal(errHome)
	}
	revision, errUpdate := repo.UpdateLifecycleConfig(ctx, 20*time.Second, config.DefaultCredentialConcurrencyConfig())
	if errUpdate != nil {
		t.Fatal(errUpdate)
	}
	member, errSubscribe := repo.SubscribeMembership(ctx, SubscribeMembershipRequest{Fingerprint: "fp-a", NodeID: "cpa-a", Home: home, ProtocolVersion: 1, LifecycleConfigRevision: revision})
	if errSubscribe != nil {
		t.Fatal(errSubscribe)
	}

	membershipLocked := false
	lockErr := errors.New("participation must lock membership before insert")
	if errCallback := repo.db.Callback().Query().Before("gorm:query").Register("test:participation_membership_lock", func(tx *gorm.DB) {
		if tx.Statement.Table != (CPANodeMembershipRecord{}).TableName() {
			return
		}
		if _, locked := tx.Statement.Clauses["FOR"]; !locked {
			tx.AddError(lockErr)
			return
		}
		membershipLocked = true
	}); errCallback != nil {
		t.Fatal(errCallback)
	}
	if errCallback := repo.db.Callback().Create().Before("gorm:create").Register("test:participation_insert_after_membership_lock", func(tx *gorm.DB) {
		if tx.Statement.Table == (CPANodeParticipationRecord{}).TableName() && !membershipLocked {
			tx.AddError(lockErr)
		}
	}); errCallback != nil {
		t.Fatal(errCallback)
	}

	errParticipation := repo.RecordParticipation(ctx, ConnectionLifetime{Fingerprint: member.CertificateFingerprint, ConnectedAt: member.ConnectedAt, Home: home})
	if errParticipation != nil {
		t.Fatalf("RecordParticipation() error = %v", errParticipation)
	}
}

func TestProtocolZeroSubscribeIsRejectedAfterActivation(t *testing.T) {
	ctx := context.Background()
	repo := newCredentialFoundationTestRepository(t)
	home, errHome := repo.RegisterHomeIncarnation(ctx, "10.0.0.1", 8317, []string{"credential_concurrency_foundation_v1"})
	if errHome != nil {
		t.Fatal(errHome)
	}
	if errGate := repo.WithConcurrencyActivationGate(ctx, func(tx *gorm.DB, gate *ConcurrencyActivationGateRecord) error {
		gate.ActivePolicyCount = 1
		return tx.Save(gate).Error
	}); errGate != nil {
		t.Fatal(errGate)
	}
	_, errSubscribe := repo.SubscribeMembership(ctx, SubscribeMembershipRequest{Fingerprint: "fp-a", NodeID: "cpa-a", Home: home, ProtocolVersion: 0})
	if !errors.Is(errSubscribe, ErrConcurrencyProtocolRequired) {
		t.Fatalf("subscribe error = %v", errSubscribe)
	}
}

func TestProtocolOneSubscribeRequiresCurrentRevision(t *testing.T) {
	ctx := context.Background()
	repo := newCredentialFoundationTestRepository(t)
	home, errHome := repo.RegisterHomeIncarnation(ctx, "10.0.0.1", 8317, []string{"credential_concurrency_foundation_v1"})
	if errHome != nil {
		t.Fatal(errHome)
	}
	revision, errUpdate := repo.UpdateLifecycleConfig(ctx, 20*time.Second, config.DefaultCredentialConcurrencyConfig())
	if errUpdate != nil {
		t.Fatal(errUpdate)
	}
	_, errSubscribe := repo.SubscribeMembership(ctx, SubscribeMembershipRequest{Fingerprint: "fp-a", NodeID: "cpa-a", Home: home, ProtocolVersion: 1, LifecycleConfigRevision: revision - 1})
	if !errors.Is(errSubscribe, ErrLifecycleConfigRevisionMismatch) {
		t.Fatalf("subscribe error = %v", errSubscribe)
	}
}
