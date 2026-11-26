package main

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/respserver"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func TestRecoverCPAFencesContinuesAfterBlockedMembership(t *testing.T) {
	ctx := context.Background()
	db, errOpen := cluster.OpenSQLite(ctx, filepath.Join(t.TempDir(), "home.db"))
	if errOpen != nil {
		t.Fatal(errOpen)
	}
	sqlDB, errDB := db.DB()
	if errDB != nil {
		t.Fatal(errDB)
	}
	defer func() {
		if errClose := sqlDB.Close(); errClose != nil {
			t.Errorf("close database: %v", errClose)
		}
	}()
	if errMigrate := cluster.AutoMigrate(db); errMigrate != nil {
		t.Fatal(errMigrate)
	}
	repo := cluster.NewRepository(db)
	home, errHome := repo.RegisterHomeIncarnation(ctx, "127.0.0.1", 8317, []string{"credential_concurrency_foundation_v1"})
	if errHome != nil {
		t.Fatal(errHome)
	}
	revision, errLifecycle := repo.UpdateLifecycleConfig(ctx, 20*time.Second, config.DefaultCredentialConcurrencyConfig())
	if errLifecycle != nil {
		t.Fatal(errLifecycle)
	}
	const blockedFingerprint, pendingFingerprint = "fp-blocked", "fp-pending"
	for _, fingerprint := range []string{blockedFingerprint, pendingFingerprint} {
		member, errMember := repo.SubscribeMembership(ctx, cluster.SubscribeMembershipRequest{Fingerprint: fingerprint, NodeID: "cpa-" + fingerprint, Home: home, ProtocolVersion: 1, LifecycleConfigRevision: revision})
		if errMember != nil {
			t.Fatal(errMember)
		}
		if _, errBegin := repo.BeginFingerprintCancellation(ctx, member.CertificateFingerprint); errBegin != nil {
			t.Fatal(errBegin)
		}
	}

	// Fail every membership read for one fingerprint, as a row lock held elsewhere would.
	blocked := errors.New("membership is blocked")
	if errRegister := db.Callback().Query().Before("gorm:query").Register("test:blocked_membership", func(tx *gorm.DB) {
		if tx.Statement.Table != "cpa_node_membership" {
			return
		}
		where, okWhere := tx.Statement.Clauses["WHERE"].Expression.(clause.Where)
		if !okWhere {
			return
		}
		for _, expression := range where.Exprs {
			condition, okCondition := expression.(clause.Expr)
			if okCondition && len(condition.Vars) > 0 && condition.Vars[0] == blockedFingerprint {
				tx.AddError(blocked)
			}
		}
	}); errRegister != nil {
		t.Fatal(errRegister)
	}

	errRecover := recoverCPAFences(ctx, repo, home, respserver.New("", nil))
	if !errors.Is(errRecover, blocked) {
		t.Fatalf("recovery error = %v, want the blocked membership failure", errRecover)
	}
	var members []cluster.CPANodeMembershipRecord
	if errFind := db.Order("certificate_fingerprint").Find(&members).Error; errFind != nil {
		t.Fatal(errFind)
	}
	if len(members) != 2 {
		t.Fatalf("memberships = %d, want 2", len(members))
	}
	if members[0].CertificateFingerprint != blockedFingerprint || members[0].State != cluster.MembershipStateCanceling {
		t.Fatalf("blocked membership = %s in state %q, want %s still canceling", members[0].CertificateFingerprint, members[0].State, blockedFingerprint)
	}
	if members[1].CertificateFingerprint != pendingFingerprint || members[1].State != cluster.MembershipStateClosed {
		t.Fatalf("unrelated membership = %s in state %q, want %s closed", members[1].CertificateFingerprint, members[1].State, pendingFingerprint)
	}
}
