package cluster

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"gorm.io/gorm"
)

func TestPostgresLatencyCleanupSkipsLockedRowsAndPreservesRefresh(t *testing.T) {
	for _, summaries := range []bool{true, false} {
		t.Run(fmt.Sprintf("summaries=%t", summaries), func(t *testing.T) {
			repo := newPostgresQuiescenceRepository(t)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			now, errNow := DatabaseNow(ctx, repo.db)
			if errNow != nil {
				t.Fatal(errNow)
			}
			purge := repo.PurgeExpiredCPALatencyConfirmations
			var held any
			if summaries {
				purge = repo.PurgeExpiredCPALatencySummaries
				records := []CPALatencyRecord{
					{Fingerprint: "held", Address: "home", UpdatedAt: now.Add(-25 * time.Hour)},
					{Fingerprint: "free", Address: "home", UpdatedAt: now.Add(-25 * time.Hour)},
				}
				if errCreate := repo.db.Create(&records).Error; errCreate != nil {
					t.Fatal(errCreate)
				}
				held = &records[0]
			} else {
				records := []CPALatencyConfirmationRecord{
					{ID: "held", UpdatedAt: now.Add(-11 * time.Minute)},
					{ID: "free", UpdatedAt: now.Add(-11 * time.Minute)},
				}
				if errCreate := repo.db.Create(&records).Error; errCreate != nil {
					t.Fatal(errCreate)
				}
				held = &records[0]
			}
			holder := repo.db.WithContext(ctx).Begin()
			if holder.Error != nil {
				t.Fatal(holder.Error)
			}
			defer func() {
				if errRollback := holder.Rollback().Error; errRollback != nil && !errors.Is(errRollback, sql.ErrTxDone) {
					t.Error(errRollback)
				}
			}()
			// The update is not committed: a cleaner sees the expired version but must
			// skip its writer's lock rather than wait and delete the refreshed row.
			if errUpdate := holder.Model(held).UpdateColumn("updated_at", now).Error; errUpdate != nil {
				t.Fatal(errUpdate)
			}
			cleanupCtx, cancelCleanup := context.WithTimeout(ctx, 3*time.Second)
			deleted, errPurge := purge(cleanupCtx, now.Add(72*time.Hour), 1000)
			cancelCleanup()
			if errPurge != nil || deleted != 1 {
				t.Fatalf("cleanup waited for a refreshed row: deleted=%d error=%v", deleted, errPurge)
			}
			if errCommit := holder.Commit().Error; errCommit != nil {
				t.Fatal(errCommit)
			}
			if deleted, errPurge := purge(ctx, now.Add(72*time.Hour), 1000); errPurge != nil || deleted != 0 {
				t.Fatalf("cleanup removed a committed refresh: deleted=%d error=%v", deleted, errPurge)
			}
		})
	}
}

func TestPostgresLatencyCleanupDoesNotWaitForElectionGate(t *testing.T) {
	f := seedRebalanceFixture(t, newPostgresQuiescenceRepository(t), 0)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	now, errNow := DatabaseNow(ctx, f.repo.db)
	if errNow != nil {
		t.Fatal(errNow)
	}
	if errCreate := f.repo.db.Create(&CPALatencyRecord{Fingerprint: "expired", Address: "home", UpdatedAt: now.Add(-25 * time.Hour)}).Error; errCreate != nil {
		t.Fatal(errCreate)
	}
	if errCreate := f.repo.db.Create(&CPALatencyConfirmationRecord{ID: "abandoned", UpdatedAt: now.Add(-time.Hour)}).Error; errCreate != nil {
		t.Fatal(errCreate)
	}
	if errTransaction := f.repo.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if _, _, errGate := lockMasterGate(tx); errGate != nil {
			return errGate
		}
		for _, purge := range []func(context.Context, time.Time, int) (int64, error){f.repo.PurgeExpiredCPALatencySummaries, f.repo.PurgeExpiredCPALatencyConfirmations} {
			cleanupCtx, cancelCleanup := context.WithTimeout(ctx, 3*time.Second)
			deleted, errPurge := purge(cleanupCtx, now, 1000)
			cancelCleanup()
			if errPurge != nil {
				return errPurge
			}
			if deleted != 1 {
				return fmt.Errorf("cleanup deleted %d rows, want 1", deleted)
			}
		}
		return nil
	}); errTransaction != nil {
		t.Fatal(errTransaction)
	}
}

func TestPostgresLatencyCleanupDelayDoesNotBlockHeartbeatOrRenewal(t *testing.T) {
	for _, summaries := range []bool{true, false} {
		t.Run(fmt.Sprintf("summaries=%t", summaries), func(t *testing.T) {
			f := seedRebalanceFixture(t, newPostgresQuiescenceRepository(t), 1)
			ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
			defer cancel()
			now, errNow := DatabaseNow(ctx, f.repo.db)
			if errNow != nil {
				t.Fatal(errNow)
			}
			purge := f.repo.PurgeExpiredCPALatencyConfirmations
			table := "cpa_latency_confirmations"
			if summaries {
				purge, table = f.repo.PurgeExpiredCPALatencySummaries, "cpa_latency_summaries"
				lifetime := f.lifetimes[0]
				record := CPALatencyRecord{Fingerprint: lifetime.Fingerprint, Address: latencyAddress(lifetime.Home.IP, lifetime.Home.Port), UpdatedAt: now.Add(-25 * time.Hour)}
				if errCreate := f.repo.db.Create(&record).Error; errCreate != nil {
					t.Fatal(errCreate)
				}
			} else {
				if errCreate := f.repo.db.Create(&CPALatencyConfirmationRecord{ID: "active", UpdatedAt: now.Add(-time.Hour)}).Error; errCreate != nil {
					t.Fatal(errCreate)
				}
			}
			started, release := make(chan struct{}), make(chan struct{})
			if errRegister := f.repo.db.Callback().Delete().Before("gorm:delete").Register("test:pause-latency-cleanup", func(tx *gorm.DB) {
				if tx.Statement.Table == table {
					close(started)
					select {
					case <-release:
					case <-ctx.Done():
						tx.AddError(ctx.Err())
					}
				}
			}); errRegister != nil {
				t.Fatal(errRegister)
			}
			done := make(chan error, 1)
			go func() {
				deleted, errPurge := purge(ctx, now, 1000)
				if errPurge == nil && deleted != 0 {
					errPurge = fmt.Errorf("cleanup deleted %d renewed rows", deleted)
				}
				done <- errPurge
			}()
			joined := false
			defer func() {
				cancel()
				if !joined {
					<-done
				}
			}()
			select {
			case <-started:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			heartbeatCtx, cancelHeartbeat := context.WithTimeout(ctx, 3*time.Second)
			errBeat := f.coordinators[0].heartbeatAndElect(heartbeatCtx, f.homes[0])
			cancelHeartbeat()
			if errBeat != nil {
				t.Fatal("cleanup blocked heartbeat: ", errBeat)
			}
			if summaries {
				if errReport := postgresLatencyReport(ctx, f.handlers[1], f.lifetimes[0], 1, now.Add(-time.Second), true); errReport != nil {
					t.Fatal(errReport)
				}
			} else {
				if errUpdate := f.repo.db.Model(&CPALatencyConfirmationRecord{}).Where("id = ?", "active").UpdateColumn("updated_at", now).Error; errUpdate != nil {
					t.Fatal(errUpdate)
				}
			}
			close(release)
			select {
			case errPurge := <-done:
				joined = true
				if errPurge != nil {
					t.Fatal(errPurge)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		})
	}
}

func TestPostgresLatencyCleanupKeepsBatchBound(t *testing.T) {
	repo := newPostgresQuiescenceRepository(t)
	now, errNow := DatabaseNow(t.Context(), repo.db)
	if errNow != nil {
		t.Fatal(errNow)
	}
	records := make([]CPALatencyRecord, 1001)
	for index := range records {
		records[index] = CPALatencyRecord{Fingerprint: "shared", Address: fmt.Sprintf("home-%04d", index), UpdatedAt: now.Add(-25 * time.Hour)}
	}
	if errCreate := repo.db.CreateInBatches(&records, 100).Error; errCreate != nil {
		t.Fatal(errCreate)
	}
	for _, want := range []int64{1000, 1, 0} {
		if deleted, errPurge := repo.PurgeExpiredCPALatencySummaries(t.Context(), now, 2000); errPurge != nil || deleted != want {
			t.Fatalf("deleted=%d error=%v want=%d", deleted, errPurge, want)
		}
	}
}
