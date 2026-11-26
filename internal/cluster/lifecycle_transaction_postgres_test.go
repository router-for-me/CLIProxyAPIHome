package cluster

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func TestPostgresLifecycleTransactionTimeoutsAreLocal(t *testing.T) {
	repo := newPostgresQuiescenceRepository(t)
	for _, commit := range []bool{false, true} {
		for _, shorter := range []bool{false, true} {
			errConnection := repo.db.Connection(func(db *gorm.DB) error {
				type timeouts struct{ Lock, Statement int64 }
				read := func(db *gorm.DB) (timeouts, error) {
					var value timeouts
					errRead := db.Raw(`SELECT
						(SELECT setting::bigint FROM pg_settings WHERE name = 'lock_timeout') AS lock,
						(SELECT setting::bigint FROM pg_settings WHERE name = 'statement_timeout') AS statement`).Scan(&value).Error
					return value, errRead
				}
				before, errBefore := read(db)
				if errBefore != nil {
					return errBefore
				}
				rollback := errors.New("test rollback")
				errTransaction := db.Transaction(func(tx *gorm.DB) error {
					initial := "0"
					if shorter {
						initial = "100ms"
					}
					if errSet := tx.Exec("SELECT set_config('lock_timeout', ?, true), set_config('statement_timeout', ?, true)", initial, initial).Error; errSet != nil {
						return errSet
					}
					if errConfigure := configureLifecycleTransaction(tx); errConfigure != nil {
						return errConfigure
					}
					configured, errRead := read(tx)
					if errRead != nil {
						return errRead
					}
					want := timeouts{lifecycleLockTimeout.Milliseconds(), lifecycleTransactionBudget.Milliseconds()}
					if shorter {
						want = timeouts{100, 100}
					}
					if configured != want {
						t.Errorf("timeouts = %+v, want %+v", configured, want)
					}
					if !commit {
						return rollback
					}
					return nil
				})
				if errTransaction != nil && !errors.Is(errTransaction, rollback) {
					return errTransaction
				}
				after, errAfter := read(db)
				if errAfter == nil && after != before {
					t.Errorf("transaction changed pooled session timeouts: before=%+v after=%+v", before, after)
				}
				return errAfter
			})
			if errConnection != nil {
				t.Fatal(errConnection)
			}
		}
	}
}

func TestPostgresSubscribeTimeoutReleasesSingletonLocks(t *testing.T) {
	repo, ctx, lifetime := newPostgresLifecycleFixture(t)
	var member CPANodeMembershipRecord
	if errFind := repo.db.First(&member, "certificate_fingerprint = ?", lifetime.Fingerprint).Error; errFind != nil {
		t.Fatal(errFind)
	}
	holder := repo.db.WithContext(ctx).Begin()
	if holder.Error != nil {
		t.Fatal(holder.Error)
	}
	t.Cleanup(func() {
		if errRollback := holder.Rollback().Error; errRollback != nil {
			t.Error(errRollback)
		}
	})
	if errLock := holder.Clauses(clause.Locking{Strength: "UPDATE"}).First(&CPANodeMembershipRecord{}, "certificate_fingerprint = ?", lifetime.Fingerprint).Error; errLock != nil {
		t.Fatal(errLock)
	}
	// No caller deadline: the repository must bound this lock wait itself.
	_, errSubscribe := repo.SubscribeMembership(context.Background(), SubscribeMembershipRequest{
		Fingerprint: lifetime.Fingerprint, NodeID: member.NodeID, Home: lifetime.Home,
		ProtocolVersion: 1, LifecycleConfigRevision: member.LifecycleConfigRevision, Takeover: true,
	})
	var pgError *pgconn.PgError
	if !errors.As(errSubscribe, &pgError) || pgError.Code != "55P03" {
		t.Fatalf("subscribe error = %v, want PostgreSQL lock timeout", errSubscribe)
	}
	// The original membership is still locked; the failed subscriber must already
	// have rolled back its global and lifecycle locks, rather than retaining either.
	if errProbe := repo.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if errGate := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "NOWAIT"}).First(&ConcurrencyActivationGateRecord{}, "id = ?", 1).Error; errGate != nil {
			return errGate
		}
		return tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "NOWAIT"}).First(&LifecycleConfigRecord{}, "id = ?", 1).Error
	}); errProbe != nil {
		t.Fatal("failed subscriber retained singleton locks:", errProbe)
	}
	if errFind := repo.db.First(&member, "certificate_fingerprint = ?", lifetime.Fingerprint).Error; errFind != nil {
		t.Fatal(errFind)
	}
	if !member.ConnectedAt.Equal(lifetime.ConnectedAt) {
		t.Fatal("failed takeover changed the membership lifetime")
	}
}

func TestPostgresLifecycleCleanupContinuesAfterLockedMembership(t *testing.T) {
	repo, ctx, lifetime := newPostgresLifecycleFixture(t)
	var second CPANodeMembershipRecord
	if errFind := repo.db.First(&second, "certificate_fingerprint = ?", lifetime.Fingerprint).Error; errFind != nil {
		t.Fatal(errFind)
	}
	second.CertificateFingerprint = "fp-b"
	second.NodeID = "node-b"
	if errCreate := repo.db.Create(&second).Error; errCreate != nil {
		t.Fatal(errCreate)
	}
	if errExpire := repo.db.Model(&CPANodeMembershipRecord{}).Where("certificate_fingerprint IN ?", []string{lifetime.Fingerprint, second.CertificateFingerprint}).Update("last_seen_at", time.Unix(0, 0).UTC()).Error; errExpire != nil {
		t.Fatal(errExpire)
	}
	holder := repo.db.WithContext(ctx).Begin()
	if holder.Error != nil {
		t.Fatal(holder.Error)
	}
	t.Cleanup(func() {
		if errRollback := holder.Rollback().Error; errRollback != nil {
			t.Error(errRollback)
		}
	})
	if errLock := holder.Clauses(clause.Locking{Strength: "UPDATE"}).First(&CPANodeMembershipRecord{}, "certificate_fingerprint = ?", lifetime.Fingerprint).Error; errLock != nil {
		t.Fatal(errLock)
	}
	errCleanup := repo.CleanupStaleMemberships(context.Background())
	var pgError *pgconn.PgError
	if !errors.As(errCleanup, &pgError) || pgError.Code != "55P03" {
		t.Fatalf("cleanup error = %v, want PostgreSQL lock timeout", errCleanup)
	}
	if errFind := repo.db.First(&second, "certificate_fingerprint = ?", second.CertificateFingerprint).Error; errFind != nil {
		t.Fatal(errFind)
	}
	if second.State != MembershipStateCanceling {
		t.Fatalf("unrelated membership state = %s, want canceling", second.State)
	}
	if errHeartbeat := repo.RefreshCPALiveness(context.Background(), lifetime); !errors.As(errHeartbeat, &pgError) || pgError.Code != "55P03" {
		t.Fatalf("heartbeat error = %v, want PostgreSQL lock timeout", errHeartbeat)
	}
}

func TestPostgresLifecycleConfigReadBypassesRowLock(t *testing.T) {
	repo, ctx, _ := newPostgresLifecycleFixture(t)
	holder := repo.db.WithContext(ctx).Begin()
	if holder.Error != nil {
		t.Fatal(holder.Error)
	}
	t.Cleanup(func() {
		if errRollback := holder.Rollback().Error; errRollback != nil {
			t.Error(errRollback)
		}
	})
	var record LifecycleConfigRecord
	if errLock := holder.Clauses(clause.Locking{Strength: "UPDATE"}).First(&record, "id = ?", 1).Error; errLock != nil {
		t.Fatal(errLock)
	}
	committedRevision := record.Revision
	if errUpdate := holder.Model(&record).Update("revision", committedRevision+1).Error; errUpdate != nil {
		t.Fatal(errUpdate)
	}
	readCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	cfg, errRead := repo.LifecycleConfig(readCtx)
	if errRead != nil {
		t.Fatal("read waited for an uncommitted lifecycle update:", errRead)
	}
	if cfg.LifecycleConfigRevision != committedRevision {
		t.Fatalf("read revision = %d, want committed revision %d", cfg.LifecycleConfigRevision, committedRevision)
	}
}
