package cluster

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
)

func TestPostgresConcurrencyLifetimeSharesHomeLockButBlocksFencing(t *testing.T) {
	repo := newPostgresQuiescenceRepository(t)
	home, errHome := repo.RegisterHomeIncarnation(context.Background(), "10.0.0.1", 8317, nil)
	if errHome != nil {
		t.Fatal(errHome)
	}
	revision, errConfig := repo.UpdateLifecycleConfig(context.Background(), 20*time.Second, config.DefaultCredentialConcurrencyConfig())
	if errConfig != nil {
		t.Fatal(errConfig)
	}
	member, errMember := repo.SubscribeMembership(context.Background(), SubscribeMembershipRequest{Fingerprint: "heartbeat-reader", NodeID: "cpa-reader", Home: home, ProtocolVersion: 1, LifecycleConfigRevision: revision})
	if errMember != nil {
		t.Fatal(errMember)
	}
	holder := repo.db.Begin()
	if holder.Error != nil {
		t.Fatal(holder.Error)
	}
	defer func() {
		if errRollback := holder.Rollback().Error; errRollback != nil && !errors.Is(errRollback, sql.ErrTxDone) {
			t.Errorf("rollback holder: %v", errRollback)
		}
	}()
	if errVerify := holder.Clauses(clause.Locking{Strength: "SHARE"}).First(&HomeProcessIncarnationRecord{}, "home_ip = ? AND home_port = ? AND started_at = ?", home.IP, home.Port, home.StartedAt).Error; errVerify != nil {
		t.Fatal(errVerify)
	}

	// A second read must complete while the first transaction retains its lock.
	errReader := repo.db.Transaction(func(tx *gorm.DB) error {
		if errTimeout := tx.Exec("SET LOCAL lock_timeout = '200ms'").Error; errTimeout != nil {
			return errTimeout
		}
		return NewRepository(tx).LockActiveConcurrencyLifetimeTx(context.Background(), tx, ConnectionLifetime{Controlled: true, Fingerprint: member.CertificateFingerprint, ConnectedAt: member.ConnectedAt, Home: home})
	})
	if errReader != nil {
		t.Fatalf("concurrent Home verification blocked: %v", errReader)
	}

	// Fencing still needs to wait for all validating transactions to finish.
	errFence := repo.db.Transaction(func(tx *gorm.DB) error {
		if errTimeout := tx.Exec("SET LOCAL lock_timeout = '200ms'").Error; errTimeout != nil {
			return errTimeout
		}
		return tx.Model(&HomeProcessIncarnationRecord{}).
			Where("home_ip = ? AND home_port = ? AND started_at = ?", home.IP, home.Port, home.StartedAt).
			Update("state", HomeIncarnationFenced).Error
	})
	var pgError *pgconn.PgError
	if !errors.As(errFence, &pgError) || pgError.Code != "55P03" {
		t.Fatalf("fencing while verification is open = %v, want lock_not_available", errFence)
	}
	if errCommit := holder.Commit().Error; errCommit != nil {
		t.Fatal(errCommit)
	}
	if errFence := repo.FenceHomeIncarnation(context.Background(), home, "test"); errFence != nil {
		t.Fatal(errFence)
	}
	if errVerify := repo.db.Transaction(func(tx *gorm.DB) error {
		return verifyActiveHomeIncarnation(tx, home)
	}); !errors.Is(errVerify, ErrHomeIncarnationFenced) {
		t.Fatalf("verification after fencing = %v, want fenced", errVerify)
	}
}
