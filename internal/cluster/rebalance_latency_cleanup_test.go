package cluster

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"gorm.io/gorm"
)

func TestLatencySummaryCleanupBatchesAndCompositeIdentity(t *testing.T) {
	repo := newCredentialFoundationTestRepository(t)
	now, errNow := DatabaseNow(t.Context(), repo.db)
	if errNow != nil {
		t.Fatal(errNow)
	}
	records := make([]CPALatencyRecord, 2101)
	for index := range records {
		records[index] = CPALatencyRecord{Fingerprint: fmt.Sprintf("fp-%04d", index), Address: "10.0.0.1:8327", UpdatedAt: now.Add(-25 * time.Hour)}
	}
	records = append(records,
		CPALatencyRecord{Fingerprint: "fp-0000", Address: "10.0.0.2:8327", UpdatedAt: now},
		CPALatencyRecord{Fingerprint: "fresh", Address: "10.0.0.1:8327", UpdatedAt: now})
	if errCreate := repo.db.CreateInBatches(&records, 100).Error; errCreate != nil {
		t.Fatal(errCreate)
	}
	// A summary refreshed before deletion must survive without discarding its old neighbours.
	if errUpdate := repo.db.Model(&CPALatencyRecord{}).Where("fingerprint = ? AND address = ?", records[0].Fingerprint, records[0].Address).UpdateColumn("updated_at", now).Error; errUpdate != nil {
		t.Fatal(errUpdate)
	}
	for _, want := range []int64{1000, 1000, 100, 0} {
		// Even an oversized caller limit must not turn a batch into an unbounded delete.
		deleted, errPurge := repo.PurgeExpiredCPALatencySummaries(t.Context(), now.Add(72*time.Hour), 5000)
		if errPurge != nil || deleted != want {
			t.Fatalf("deleted=%d error=%v want=%d", deleted, errPurge, want)
		}
	}
	var remaining []CPALatencyRecord
	if errFind := repo.db.Find(&remaining).Error; errFind != nil {
		t.Fatal(errFind)
	}
	if len(remaining) != 3 {
		t.Fatalf("fresh summaries or renewed composite identity deleted: %+v", remaining)
	}
	// Both components must identify the selected row, including for expired rows sharing either key.
	pairs := []CPALatencyRecord{
		{Fingerprint: "pair-a", Address: "home-a", UpdatedAt: now.Add(-28 * time.Hour)},
		{Fingerprint: "pair-a", Address: "home-b", UpdatedAt: now.Add(-27 * time.Hour)},
		{Fingerprint: "pair-b", Address: "home-a", UpdatedAt: now.Add(-26 * time.Hour)},
	}
	if errCreate := repo.db.Create(&pairs).Error; errCreate != nil {
		t.Fatal(errCreate)
	}
	if deleted, errPurge := repo.PurgeExpiredCPALatencySummaries(t.Context(), now.Add(-72*time.Hour), 1); errPurge != nil || deleted != 1 {
		t.Fatalf("composite-key batch deleted=%d error=%v", deleted, errPurge)
	}
	for _, record := range pairs[1:] {
		var saved CPALatencyRecord
		if errFirst := repo.db.First(&saved, "fingerprint = ? AND address = ?", record.Fingerprint, record.Address).Error; errFirst != nil {
			t.Fatal(errFirst)
		}
	}
}

func TestLatencyConfirmationCleanupBatchesAndRetention(t *testing.T) {
	repo := newCredentialFoundationTestRepository(t)
	now, errNow := DatabaseNow(t.Context(), repo.db)
	if errNow != nil {
		t.Fatal(errNow)
	}
	records := make([]CPALatencyConfirmationRecord, 1101)
	for index := range records {
		records[index] = CPALatencyConfirmationRecord{ID: fmt.Sprintf("abandoned-%04d", index), UpdatedAt: now.Add(-11 * time.Minute)}
	}
	records = append(records,
		CPALatencyConfirmationRecord{ID: "active", UpdatedAt: now},
		CPALatencyConfirmationRecord{ID: "stale-evidence-but-not-abandoned", UpdatedAt: now.Add(-5 * time.Minute)})
	if errCreate := repo.db.CreateInBatches(&records, 100).Error; errCreate != nil {
		t.Fatal(errCreate)
	}
	for _, want := range []int64{1000, 101, 0} {
		deleted, errPurge := repo.PurgeExpiredCPALatencyConfirmations(t.Context(), now.Add(72*time.Hour), 5000)
		if errPurge != nil || deleted != want {
			t.Fatalf("deleted=%d error=%v want=%d", deleted, errPurge, want)
		}
	}
	var count int64
	if errCount := repo.db.Model(&CPALatencyConfirmationRecord{}).Count(&count).Error; errCount != nil || count != 2 {
		t.Fatalf("recent confirmations removed: count=%d error=%v", count, errCount)
	}
}

func TestLatencyCleanupNoopAndClockErrors(t *testing.T) {
	repo := newCredentialFoundationTestRepository(t)
	failure := errors.New("test database clock failure")
	if errRegister := repo.db.Callback().Row().Before("gorm:row").Register("test:latency-cleanup-clock-failure", func(tx *gorm.DB) {
		if tx.Statement.SQL.String() == databaseNowQuery(tx) {
			tx.AddError(failure)
		}
	}); errRegister != nil {
		t.Fatal(errRegister)
	}
	for _, purge := range []func(context.Context, time.Time, int) (int64, error){repo.PurgeExpiredCPALatencySummaries, repo.PurgeExpiredCPALatencyConfirmations} {
		for _, limit := range []int{0, -1} {
			if deleted, errPurge := purge(t.Context(), time.Time{}, limit); errPurge != nil || deleted != 0 {
				t.Fatalf("non-positive limit deleted=%d error=%v", deleted, errPurge)
			}
		}
		if deleted, errPurge := purge(t.Context(), time.Time{}, 1); !errors.Is(errPurge, failure) || deleted != 0 {
			t.Fatalf("clock failure was ignored: deleted=%d error=%v", deleted, errPurge)
		}
	}
}

func TestRebalancePlannerDoesNotGarbageCollectLatency(t *testing.T) {
	f := newLatencySwapFixture(t)
	now, errNow := DatabaseNow(t.Context(), f.repo.db)
	if errNow != nil {
		t.Fatal(errNow)
	}
	old := CPALatencyRecord{Fingerprint: "old", Address: "old", UpdatedAt: now.Add(-25 * time.Hour)}
	if errCreate := f.repo.db.Create(&old).Error; errCreate != nil {
		t.Fatal(errCreate)
	}
	abandoned := CPALatencyConfirmationRecord{ID: "abandoned", UpdatedAt: now.Add(-time.Hour)}
	if errCreate := f.repo.db.Create(&abandoned).Error; errCreate != nil {
		t.Fatal(errCreate)
	}
	f.reconcile(t, 0)
	if errFirst := f.repo.db.First(&old, "fingerprint = ?", "old").Error; errFirst != nil {
		t.Fatal("planner still collected expired summaries: ", errFirst)
	}
	if errFirst := f.repo.db.First(&abandoned, "id = ?", "abandoned").Error; errFirst != nil {
		t.Fatal("planner still collected unrelated abandoned confirmations: ", errFirst)
	}
}

func TestCoordinatorMasterCleansExpiredLatency(t *testing.T) {
	repo := newCredentialFoundationTestRepository(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	coordinator := NewCoordinator(repo, NodeIdentity{IP: "127.0.0.1", Port: 8327}, CoordinatorOptions{HeartbeatInterval: time.Hour, HeartbeatTimeout: time.Hour})
	if errInitialize := coordinator.Initialize(ctx); errInitialize != nil {
		t.Fatal(errInitialize)
	}
	defer coordinator.setMaster(false)
	now, errNow := DatabaseNow(ctx, repo.db)
	if errNow != nil {
		t.Fatal(errNow)
	}
	if errCreate := repo.db.Create(&CPALatencyRecord{Fingerprint: "expired", Address: "home", UpdatedAt: now.Add(-25 * time.Hour)}).Error; errCreate != nil {
		t.Fatal(errCreate)
	}
	if errCreate := repo.db.Create(&CPALatencyConfirmationRecord{ID: "abandoned", UpdatedAt: now.Add(-time.Hour)}).Error; errCreate != nil {
		t.Fatal(errCreate)
	}
	purged := make(chan string, 4)
	if errRegister := repo.db.Callback().Delete().After("gorm:commit_or_rollback_transaction").Register("test:latency-background-cleanup", func(tx *gorm.DB) {
		if tx.Error == nil && tx.RowsAffected > 0 && (tx.Statement.Table == "cpa_latency_summaries" || tx.Statement.Table == "cpa_latency_confirmations") {
			purged <- tx.Statement.Table
		}
	}); errRegister != nil {
		t.Fatal(errRegister)
	}
	done := make(chan error, 1)
	go func() { done <- coordinator.Start(ctx) }()
	defer func() {
		cancel()
		if errStart := <-done; errStart != nil {
			t.Error(errStart)
		}
	}()
	for seen := map[string]bool{}; len(seen) < 2; {
		select {
		case table := <-purged:
			seen[table] = true
		case <-ctx.Done():
			t.Fatal("master did not start both latency cleanup loops")
		}
	}
}
