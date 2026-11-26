package cluster

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"
)

func TestLifecycleCleanupRunsRecoveryAfterMembershipFailure(t *testing.T) {
	repo := newCredentialFoundationTestRepository(t)
	recoveries := 0
	coordinator := NewCoordinator(repo, NodeIdentity{IP: "home-a", Port: 8327}, CoordinatorOptions{
		HeartbeatTimeout: DefaultHeartbeatTimeout(),
		StartupRecovery: func(context.Context, HomeIncarnationID) error {
			recoveries++
			return nil
		},
	})
	if errInitialize := coordinator.Initialize(t.Context()); errInitialize != nil {
		t.Fatal(errInitialize)
	}
	recoveries = 0
	blocked := errors.New("membership is blocked")
	if errRegister := repo.db.Callback().Query().Before("gorm:query").Register("test:blocked_cleanup", func(tx *gorm.DB) {
		if tx.Statement.Table == "cpa_node_membership" {
			tx.AddError(blocked)
		}
	}); errRegister != nil {
		t.Fatal(errRegister)
	}
	if errCleanup := coordinator.runLifecycleCleanup(t.Context()); !errors.Is(errCleanup, blocked) {
		t.Fatalf("cleanup error = %v, want membership failure", errCleanup)
	}
	if recoveries != 1 {
		t.Fatalf("recovery calls = %d, want 1 despite membership failure", recoveries)
	}
}

func TestLifecycleOperationsBoundDatabaseContexts(t *testing.T) {
	for _, shorterParent := range []bool{false, true} {
		for _, operation := range []string{"subscribe", "participation", "heartbeat", "cancellation", "lifetime cancellation", "cleanup", "recovery"} {
			name := operation
			if shorterParent {
				name += " with shorter parent"
			}
			t.Run(name, func(t *testing.T) {
				repo, home, member := newQuiescenceMembership(t, t.Context(), "fp-budget")
				lifetime := ConnectionLifetime{Fingerprint: member.CertificateFingerprint, ConnectedAt: member.ConnectedAt, Home: home, Controlled: true}
				if operation == "cleanup" {
					if errUpdate := repo.db.Model(&CPANodeMembershipRecord{}).Where("certificate_fingerprint = ?", member.CertificateFingerprint).Update("last_seen_at", time.Unix(0, 0).UTC()).Error; errUpdate != nil {
						t.Fatal(errUpdate)
					}
				}
				ctx := t.Context()
				var parentDeadline time.Time
				if shorterParent {
					parentDeadline = time.Now().Add(2 * time.Second)
					var cancel context.CancelFunc
					ctx, cancel = context.WithDeadline(ctx, parentDeadline)
					defer cancel()
				}
				queries := 0
				if errRegister := repo.db.Callback().Query().Before("gorm:query").Register("test:lifecycle_deadline", func(tx *gorm.DB) {
					queries++
					deadline, bounded := tx.Statement.Context.Deadline()
					if !bounded || time.Until(deadline) > lifecycleTransactionBudget {
						t.Errorf("database query has no lifecycle deadline: %v", deadline)
					}
					if shorterParent && !deadline.Equal(parentDeadline) {
						t.Errorf("parent deadline was changed: got %v, want %v", deadline, parentDeadline)
					}
				}); errRegister != nil {
					t.Fatal(errRegister)
				}
				var errOperation error
				switch operation {
				case "subscribe":
					_, errOperation = repo.SubscribeMembership(ctx, SubscribeMembershipRequest{
						Fingerprint: member.CertificateFingerprint, NodeID: member.NodeID, Home: home,
						ProtocolVersion: 1, LifecycleConfigRevision: member.LifecycleConfigRevision, Takeover: true,
					})
				case "participation":
					errOperation = repo.RecordParticipation(ctx, lifetime)
				case "heartbeat":
					errOperation = repo.RefreshCPALiveness(ctx, lifetime)
				case "cancellation":
					_, errOperation = repo.BeginFingerprintCancellation(ctx, member.CertificateFingerprint)
				case "lifetime cancellation":
					_, errOperation = repo.BeginFingerprintCancellationForLifetime(ctx, lifetime)
				case "cleanup":
					errOperation = repo.CleanupStaleMemberships(ctx)
				case "recovery":
					errOperation = repo.RecoverStaleQuiescence(ctx)
				}
				if errOperation != nil {
					t.Fatal(errOperation)
				}
				if queries == 0 {
					t.Fatal("no database query was observed")
				}
			})
		}
	}
}

func TestIsLifecycleTimeout(t *testing.T) {
	for name, testCase := range map[string]struct {
		err  error
		want bool
	}{
		"lock timeout":        {err: &pgconn.PgError{Code: "55P03"}, want: true},
		"statement timeout":   {err: fmt.Errorf("refresh liveness: %w", &pgconn.PgError{Code: "57014"}), want: true},
		"budget exhausted":    {err: fmt.Errorf("refresh liveness: %w", context.DeadlineExceeded), want: true},
		"caller canceled":     {err: context.Canceled},
		"deadlock":            {err: &pgconn.PgError{Code: "40P01"}},
		"inactive membership": {err: ErrMembershipNotActive},
		"fenced Home":         {err: ErrHomeIncarnationFenced},
		"no error":            {},
	} {
		t.Run(name, func(t *testing.T) {
			if got := IsLifecycleTimeout(testCase.err); got != testCase.want {
				t.Fatalf("IsLifecycleTimeout(%v) = %t, want %t", testCase.err, got, testCase.want)
			}
		})
	}
}

func TestClosedLifetimeCleanupDoesNotShareTransactionBudget(t *testing.T) {
	for _, operation := range []string{"complete", "recovery"} {
		t.Run(operation, func(t *testing.T) {
			ctx := t.Context()
			repo, home, member := newQuiescenceMembership(t, ctx, "fp-cleanup-budget")
			revision, errBegin := repo.BeginFingerprintCancellation(ctx, member.CertificateFingerprint)
			if errBegin != nil {
				t.Fatal(errBegin)
			}
			if errAck := repo.AcknowledgeQuiescence(ctx, member.CertificateFingerprint, member.ConnectedAt, revision, home); errAck != nil {
				t.Fatal(errAck)
			}
			deadlines := map[string]time.Time{}
			if errRegister := repo.db.Callback().Delete().Before("gorm:delete").Register("test:cleanup_budget", func(tx *gorm.DB) {
				deadline, bounded := tx.Statement.Context.Deadline()
				if !bounded || time.Until(deadline) > lifecycleTransactionBudget {
					t.Errorf("delete from %s has no lifecycle deadline: %v", tx.Statement.Table, deadline)
				}
				deadlines[tx.Statement.Table] = deadline
				if tx.Statement.Table == "cpa_node_participation" {
					// Coarse platform timers can report one instant before and after a fast
					// transaction; wait for a tick so a fresh cleanup budget is observably later.
					for observed := time.Now(); !time.Now().After(observed); {
						runtime.Gosched()
					}
				}
			}); errRegister != nil {
				t.Fatal(errRegister)
			}
			var errOperation error
			if operation == "complete" {
				errOperation = repo.CompleteFingerprintCancellation(ctx, member.CertificateFingerprint, member.ConnectedAt, revision)
			} else {
				errOperation = repo.RecoverStaleQuiescence(ctx)
			}
			if errOperation != nil {
				t.Fatal(errOperation)
			}
			transaction, cleanup := deadlines["cpa_node_participation"], deadlines["cpa_in_flight_snapshots"]
			if transaction.IsZero() || cleanup.IsZero() {
				t.Fatalf("observed delete deadlines = %v, want the transaction and the cleanup", deadlines)
			}
			// The cleanup budget starts after the commit, so its deadline is strictly later.
			if !cleanup.After(transaction) {
				t.Fatalf("cleanup deadline %v reuses the transaction deadline %v", cleanup, transaction)
			}
		})
	}
}
