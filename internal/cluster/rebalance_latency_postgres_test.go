package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"testing"
	"time"
)

func TestPostgresLatencySchemaUpgradeFromV11(t *testing.T) {
	repo := newPostgresQuiescenceRepository(t)
	for _, model := range []any{&CPALatencyRecord{}, &CPALatencyConfirmationRecord{}} {
		if errDrop := repo.db.Migrator().DropTable(model); errDrop != nil {
			t.Fatal(errDrop)
		}
	}
	for _, column := range []struct {
		model any
		name  string
	}{
		{&CPARebalanceClientRecord{}, "latency_connected_at"},
		{&CPARebalanceClientRecord{}, "optimization_eligible_at"},
		{&CPARebalancePlanRecord{}, "swap_group_id"},
	} {
		if errDrop := repo.db.Migrator().DropColumn(column.model, column.name); errDrop != nil {
			t.Fatal(errDrop)
		}
	}
	if errUpdate := repo.db.Model(&schemaMigrationRecord{}).Where("id = ?", schemaMigrationRecordID).Update("version", 11).Error; errUpdate != nil {
		t.Fatal(errUpdate)
	}
	if errMigrate := AutoMigrate(repo.db); errMigrate != nil {
		t.Fatal(errMigrate)
	}
	assertPostgresSchemaMigrationVersion(t, repo.db, currentDatabaseVersion)
	if !repo.db.Migrator().HasTable(&CPALatencyRecord{}) || !repo.db.Migrator().HasTable(&CPALatencyConfirmationRecord{}) ||
		!repo.db.Migrator().HasColumn(&CPARebalanceClientRecord{}, "latency_connected_at") ||
		!repo.db.Migrator().HasColumn(&CPARebalanceClientRecord{}, "optimization_eligible_at") ||
		!repo.db.Migrator().HasColumn(&CPARebalancePlanRecord{}, "swap_group_id") {
		t.Fatal("v11 upgrade skipped latency runtime tables or columns")
	}
	if _, valid := databaseSnapshotModels(11); !valid {
		t.Fatal("previous portable snapshot version was lost")
	}
}

func TestPostgresLatencySwapConcurrentClaims(t *testing.T) {
	f := seedLatencySwapFixture(t, newPostgresQuiescenceRepository(t))
	plans := createLatencyTestSwap(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, plan := range plans {
		go func() {
			<-start
			payload, errHandle := f.handlers[1].HandleRebalance(ctx, f.lifetime(plan), []string{"CLUSTER", "BALANCE", "CLAIM", plan.ID, strconv.FormatInt(plan.Term, 10), plan.TargetIP, strconv.Itoa(plan.TargetPort)})
			if errHandle != nil {
				results <- errHandle
				return
			}
			var response struct {
				OK bool `json:"ok"`
			}
			if errJSON := json.Unmarshal(payload, &response); errJSON != nil {
				results <- errJSON
				return
			}
			if !response.OK {
				results <- fmt.Errorf("swap claim rejected: %s", plan.ID)
				return
			}
			results <- nil
		}()
	}
	close(start)
	for range 2 {
		select {
		case errClaim := <-results:
			if errClaim != nil {
				t.Error(errClaim)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	for _, plan := range f.plans(t) {
		if plan.State != rebalanceExecuting {
			t.Fatalf("concurrent swap did not reserve both legs: %+v", plan)
		}
	}
	f.reconcile(t, 0)
	if active := f.plans(t); len(active) != 2 {
		t.Fatalf("concurrent execution caused duplicate planning: %+v", active)
	}
}
