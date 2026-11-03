package cluster

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func beginTimestampLockHolder(t *testing.T, ctx context.Context, repo *Repository) *gorm.DB {
	t.Helper()
	holder := repo.db.WithContext(ctx).Begin()
	if holder.Error != nil {
		t.Fatal(holder.Error)
	}
	t.Cleanup(func() {
		if errRollback := holder.Rollback().Error; errRollback != nil && !errors.Is(errRollback, sql.ErrTxDone) {
			t.Errorf("rollback timestamp lock holder: %v", errRollback)
		}
	})
	return holder
}

// commitTimestampLockAfterWait measures database time after proving the contender is blocked.
// Successful operations must persist a timestamp no earlier than this marker, without timer sleeps.
func commitTimestampLockAfterWait(t *testing.T, ctx context.Context, repo *Repository, holder *gorm.DB, contenderPID <-chan postgresBackendPIDResult) time.Time {
	t.Helper()
	holderPID := make(chan postgresBackendPIDResult, 1)
	if errPID := postgresContenderBackendPIDHook(holderPID)(holder); errPID != nil {
		t.Fatal(errPID)
	}
	waitForPostgresContendersBlockedByHolder(t, ctx, repo, holderPID, contenderPID)
	marker, errNow := DatabaseNow(ctx, holder)
	if errNow != nil {
		t.Fatal(errNow)
	}
	if errCommit := holder.Commit().Error; errCommit != nil {
		t.Fatal(errCommit)
	}
	return marker
}

func TestPostgresHomeHeartbeatTimestampAfterLockWait(t *testing.T) {
	for _, strength := range []string{"UPDATE", "NO KEY UPDATE"} {
		t.Run(strength, func(t *testing.T) {
			repo, ctx, lifetime := newPostgresLifecycleFixture(t)
			holder := beginTimestampLockHolder(t, ctx, repo)
			var home HomeProcessIncarnationRecord
			if errLock := holder.Clauses(clause.Locking{Strength: strength}).First(&home, "home_ip = ?", lifetime.Home.IP).Error; errLock != nil {
				t.Fatal(errLock)
			}
			observeLifecycleTransitionPID(t, repo)
			observer := &lifecycleTransitionPIDObserver{results: make(chan postgresBackendPIDResult, 1)}
			heartbeatCtx := context.WithValue(ctx, lifecycleTransitionPIDContextKey{}, observer)
			done := make(chan error, 1)
			go func() { done <- repo.HeartbeatHomeIncarnation(heartbeatCtx, lifetime.Home) }()
			marker := commitTimestampLockAfterWait(t, ctx, repo, holder, observer.results)
			select {
			case errHeartbeat := <-done:
				if errHeartbeat != nil {
					t.Fatal(errHeartbeat)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if errFind := repo.db.First(&home, "home_ip = ?", lifetime.Home.IP).Error; errFind != nil {
				t.Fatal(errFind)
			}
			if home.LastSeenAt.Before(marker) {
				t.Fatalf("Home heartbeat timestamp %s predates lock release marker %s", home.LastSeenAt, marker)
			}
		})
	}
}

func TestPostgresCPAHeartbeatTimestampAfterLockWait(t *testing.T) {
	repo, ctx, lifetime := newPostgresLifecycleFixture(t)
	holder := beginTimestampLockHolder(t, ctx, repo)
	var member CPANodeMembershipRecord
	if errLock := holder.Clauses(clause.Locking{Strength: "NO KEY UPDATE"}).First(&member, "certificate_fingerprint = ?", lifetime.Fingerprint).Error; errLock != nil {
		t.Fatal(errLock)
	}
	observeLifecycleTransitionPID(t, repo)
	observer := &lifecycleTransitionPIDObserver{results: make(chan postgresBackendPIDResult, 1)}
	heartbeatCtx := context.WithValue(ctx, lifecycleTransitionPIDContextKey{}, observer)
	done := make(chan error, 1)
	go func() { done <- repo.RefreshCPALiveness(heartbeatCtx, lifetime) }()
	marker := commitTimestampLockAfterWait(t, ctx, repo, holder, observer.results)
	select {
	case errHeartbeat := <-done:
		if errHeartbeat != nil {
			t.Fatal(errHeartbeat)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if errFind := repo.db.First(&member, "certificate_fingerprint = ?", lifetime.Fingerprint).Error; errFind != nil {
		t.Fatal(errFind)
	}
	if member.LastSeenAt.Before(marker) || member.UpdatedAt.Before(marker) {
		t.Fatalf("CPA heartbeat timestamps %s/%s predate lock release marker %s", member.LastSeenAt, member.UpdatedAt, marker)
	}
}

func TestPostgresInFlightTimestampAfterAttemptLockWait(t *testing.T) {
	for _, scenario := range []string{"complete", "staging", "first-insert-conflict", "rejected", "overflow"} {
		t.Run(scenario, func(t *testing.T) {
			repo, ctx, lifetime := newPostgresLifecycleFixture(t)
			identity := InFlightIngestIdentity{
				CertificateFingerprint: lifetime.Fingerprint, NodeID: "node-a", MembershipConnectedAt: lifetime.ConnectedAt,
			}
			frame := inFlightPart(2, 0, 1, nil, nil)
			if scenario == "staging" {
				if _, errStage := repo.IngestInFlightFrame(ctx, identity, mustInFlightJSON(t, inFlightPart(2, 0, 2, nil, nil)), DefaultInFlightLimits()); errStage != nil {
					t.Fatal(errStage)
				}
				frame = inFlightPart(2, 1, 2, nil, nil)
			} else if scenario != "first-insert-conflict" {
				publishInFlightSnapshot(t, repo, identity, 1, nil)
			}
			if scenario == "rejected" {
				frame.Kind = "invalid"
			}
			if scenario == "overflow" {
				frame.Kind = InFlightFrameOverflow
				frame.PartIndex, frame.PartCount = nil, nil
				frame.AggregateGroupCount = DefaultInFlightLimits().MaxAggregateGroups + 1
			}
			raw := mustInFlightJSON(t, frame)
			holder := beginTimestampLockHolder(t, ctx, repo)
			if _, errMember := repo.RequireActiveCPAMembership(ctx, holder, identity); errMember != nil {
				t.Fatal(errMember)
			}
			if scenario == "first-insert-conflict" {
				initial := newInFlightAttempt(identity, inFlightPart(1, 0, 1, nil, nil), databaseTestTime)
				if errCreate := holder.Create(&initial).Error; errCreate != nil {
					t.Fatal(errCreate)
				}
			} else {
				var attempt CPAInFlightSnapshotAttemptRecord
				if errLock := holder.Clauses(clause.Locking{Strength: "UPDATE"}).First(&attempt).Error; errLock != nil {
					t.Fatal(errLock)
				}
			}
			observeLifecycleTransitionPID(t, repo)
			observer := &lifecycleTransitionPIDObserver{results: make(chan postgresBackendPIDResult, 1)}
			ingestCtx := context.WithValue(ctx, lifecycleTransitionPIDContextKey{}, observer)
			done := make(chan error, 1)
			go func() {
				_, errIngest := repo.IngestInFlightFrame(ingestCtx, identity, raw, DefaultInFlightLimits())
				done <- errIngest
			}()
			marker := commitTimestampLockAfterWait(t, ctx, repo, holder, observer.results)
			select {
			case errIngest := <-done:
				if scenario == "rejected" {
					if !errors.Is(errIngest, ErrInFlightFrameInvalid) {
						t.Fatalf("invalid frame error = %v", errIngest)
					}
				} else if errIngest != nil {
					t.Fatal(errIngest)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			attempt := loadInFlightAttempt(t, repo)
			if attempt.HighestSeenRevision != 2 || attempt.UpdatedAt.Before(marker) {
				t.Fatalf("attempt revision %d timestamp %s predates lock release marker %s", attempt.HighestSeenRevision, attempt.UpdatedAt, marker)
			}
			if scenario == "rejected" || scenario == "overflow" {
				return
			}
			var snapshot CPAInFlightSnapshotRecord
			if errFind := repo.db.First(&snapshot).Error; errFind != nil {
				t.Fatal(errFind)
			}
			if snapshot.Revision != 2 || snapshot.UpdatedAt.Before(marker) {
				t.Fatalf("snapshot revision %d timestamp %s predates lock release marker %s", snapshot.Revision, snapshot.UpdatedAt, marker)
			}
		})
	}
}
