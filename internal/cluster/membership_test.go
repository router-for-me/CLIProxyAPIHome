package cluster

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"gorm.io/gorm"
)

func TestSubscribeMembershipRejectsDuplicateOwnerAndReopensOnlyClosed(t *testing.T) {
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
	if _, errDuplicate := repo.SubscribeMembership(ctx, SubscribeMembershipRequest{Fingerprint: "fp-a", NodeID: "cpa-a", Home: homeB, ProtocolVersion: 1, LifecycleConfigRevision: revision}); !errors.Is(errDuplicate, ErrDuplicateCPACertificate) {
		t.Fatalf("duplicate error = %v", errDuplicate)
	}
	if errClose := repo.db.WithContext(ctx).Model(&CPANodeMembershipRecord{}).Where("certificate_fingerprint = ?", "fp-a").Update("state", MembershipStateClosed).Error; errClose != nil {
		t.Fatal(errClose)
	}
	second, errSecond := repo.SubscribeMembership(ctx, SubscribeMembershipRequest{Fingerprint: "fp-a", NodeID: "cpa-a", Home: homeB, ProtocolVersion: 1, LifecycleConfigRevision: revision})
	if errSecond != nil {
		t.Fatal(errSecond)
	}
	if !second.ConnectedAt.After(first.ConnectedAt) {
		t.Fatalf("connected_at did not increase: first=%s second=%s", first.ConnectedAt, second.ConnectedAt)
	}
}

func TestClassifyConnectionRecordsParticipationForCurrentCommandHome(t *testing.T) {
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
	if !lifetime.Controlled || lifetime.Home != commandHome || !lifetime.ConnectedAt.Equal(member.ConnectedAt) {
		t.Fatalf("lifetime = %#v", lifetime)
	}
	var participation CPANodeParticipationRecord
	if errFind := repo.db.Where("certificate_fingerprint = ? AND membership_connected_at = ? AND home_ip = ? AND home_port = ? AND home_started_at = ?", "fp-a", member.ConnectedAt, commandHome.IP, commandHome.Port, commandHome.StartedAt).First(&participation).Error; errFind != nil {
		t.Fatal(errFind)
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
