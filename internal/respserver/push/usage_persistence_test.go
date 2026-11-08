package push

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/home"
	"github.com/router-for-me/CLIProxyAPIHome/internal/respserver/dispatch"
	"gorm.io/gorm"
)

type usagePersistenceAdapter struct {
	home.ClusterAdapter
	err      error
	inserted bool
	seen     map[string]bool
}

func (a *usagePersistenceAdapter) Enabled() bool { return true }
func (a *usagePersistenceAdapter) StoreUsagePayload(_ context.Context, payload string, _ time.Time) (bool, error) {
	if a.err != nil {
		return false, a.err
	}
	if a.seen == nil {
		a.seen = make(map[string]bool)
	}
	if a.seen[payload] {
		a.inserted = false
		return false, nil
	}
	a.seen[payload] = true
	a.inserted = true
	return true, nil
}

func newUsageIngestionTestRuntime(t *testing.T, registerAuth bool) (*gorm.DB, *home.Runtime) {
	t.Helper()
	ctx := context.Background()
	db, errOpen := cluster.OpenSQLite(ctx, filepath.Join(t.TempDir(), "home.db"))
	if errOpen != nil {
		t.Fatal(errOpen)
	}
	sqlDB, errDB := db.DB()
	if errDB != nil {
		t.Fatal(errDB)
	}
	t.Cleanup(func() {
		if errClose := sqlDB.Close(); errClose != nil {
			t.Error(errClose)
		}
	})
	if errMigrate := cluster.AutoMigrate(db); errMigrate != nil {
		t.Fatal(errMigrate)
	}
	repo := cluster.NewRepository(db)
	rt, errRuntime := home.NewRuntime(&config.Config{})
	if errRuntime != nil {
		t.Fatal(errRuntime)
	}
	rt.SetClusterAdapter(cluster.NewRuntimeAdapter(repo, "192.0.2.10"))
	t.Cleanup(rt.Stop)

	const authID = "usage-ingestion-auth"
	auth := &coreauth.Auth{ID: authID, Index: authID, Provider: "codex", Status: coreauth.StatusActive}
	if _, errUpsert := repo.UpsertAuth(ctx, auth, "test"); errUpsert != nil {
		t.Fatal(errUpsert)
	}
	if registerAuth {
		if _, errRegister := rt.CoreManager().Register(coreauth.WithSkipPersist(ctx), auth); errRegister != nil {
			t.Fatal(errRegister)
		}
	}
	return db, rt
}

func TestUsagePushLoadsCredentialMissingFromLocalManager(t *testing.T) {
	_, rt := newUsageIngestionTestRuntime(t, false)
	env := dispatch.Env{Runtime: rt}
	if _, loaded := rt.CoreManager().GetByID("usage-ingestion-auth"); loaded {
		t.Fatal("credential must start unloaded")
	}

	// The credential exists in the database but this Home has not applied it yet:
	// feedback must be applied against the loaded credential, not dropped.
	quota := []string{"LPUSH", "usage", `{"timestamp":"2026-11-06T10:27:15Z","latency_ms":500,"auth_index":"usage-ingestion-auth","provider":"codex","model":"gpt-5","execution_id":"exec-unloaded","failed":true,"fail":{"status_code":429}}`}
	if reply := handleUsage(context.Background(), env, quota); reply.Kind != dispatch.ReplyKindInteger {
		t.Fatalf("unloaded credential reply = %+v", reply)
	}
	loaded, ok := rt.CoreManager().GetByID("usage-ingestion-auth")
	if !ok || loaded.ModelStates["gpt-5"] == nil || !loaded.ModelStates["gpt-5"].Quota.Exceeded {
		t.Fatalf("feedback was not applied to the loaded credential: ok=%v auth=%+v", ok, loaded)
	}

	// A credential that no longer exists anywhere keeps the delivery acknowledged.
	gone := []string{"LPUSH", "usage", `{"timestamp":"2026-11-06T10:27:16Z","latency_ms":500,"auth_index":"deleted-auth","provider":"codex","model":"gpt-5","execution_id":"exec-deleted","failed":true,"fail":{"status_code":429}}`}
	if reply := handleUsage(context.Background(), env, gone); reply.Kind != dispatch.ReplyKindInteger {
		t.Fatalf("deleted credential reply = %+v", reply)
	}
}

func TestUsagePushRedeliveryIsIdempotentForUsageAndFeedback(t *testing.T) {
	db, rt := newUsageIngestionTestRuntime(t, true)
	env := dispatch.Env{Runtime: rt}
	startedAt := time.Date(2026, 11, 6, 10, 27, 15, 0, time.UTC)
	payload := func(executionID string, offset time.Duration, status int) string {
		return fmt.Sprintf(`{"timestamp":%q,"latency_ms":500,"auth_index":"usage-ingestion-auth","provider":"codex","model":"gpt-5","execution_id":%q,"failed":%v,"fail":{"status_code":%d}}`,
			startedAt.Add(offset).Format(time.RFC3339Nano), executionID, status != 200, status)
	}

	quota := []string{"LPUSH", "usage", payload("exec-429", 10*time.Second, 429)}
	if reply := handleUsage(context.Background(), env, quota); reply.Kind != dispatch.ReplyKindInteger {
		t.Fatalf("first delivery reply = %+v", reply)
	}
	cooled, _ := rt.CoreManager().GetByID("usage-ingestion-auth")
	state := cooled.ModelStates["gpt-5"]
	if state == nil || !state.Quota.Exceeded || state.Quota.BackoffLevel != 1 || cooled.Failed != 1 {
		t.Fatalf("expected one counted 429 cooldown, got failed=%d state=%+v", cooled.Failed, state)
	}
	deadline := state.Quota.NextRecoverAt

	// CPA redelivers the same event: no new usage row, no new request count, no backoff escalation.
	if reply := handleUsage(context.Background(), env, quota); reply.Kind != dispatch.ReplyKindInteger {
		t.Fatalf("redelivery reply = %+v", reply)
	}
	replayed, _ := rt.CoreManager().GetByID("usage-ingestion-auth")
	if got := replayed.ModelStates["gpt-5"]; replayed.Failed != 1 || got.Quota.BackoffLevel != 1 || !got.Quota.NextRecoverAt.Equal(deadline) {
		t.Fatalf("redelivery changed state: failed=%d state=%+v", replayed.Failed, got)
	}

	// An older success arriving late is counted but does not clear the newer cooldown.
	stale := []string{"LPUSH", "usage", payload("exec-200-old", 0, 200)}
	if reply := handleUsage(context.Background(), env, stale); reply.Kind != dispatch.ReplyKindInteger {
		t.Fatalf("stale success reply = %+v", reply)
	}
	afterStale, _ := rt.CoreManager().GetByID("usage-ingestion-auth")
	if got := afterStale.ModelStates["gpt-5"]; afterStale.Success != 1 || !got.Quota.Exceeded {
		t.Fatalf("stale success altered cooldown: success=%d state=%+v", afterStale.Success, got)
	}

	var usageCount int64
	if errCount := db.Model(&cluster.UsageRecord{}).Count(&usageCount).Error; errCount != nil {
		t.Fatalf("Count(usage) error = %v", errCount)
	}
	if usageCount != 2 {
		t.Fatalf("usage rows = %d, want 2", usageCount)
	}
}

func TestUsagePushRespondsWithErrorOnDatabaseFailure(t *testing.T) {
	adapter := &usagePersistenceAdapter{err: errors.New("unavailable")}
	runtime := &home.Runtime{}
	runtime.SetClusterAdapter(adapter)
	env := dispatch.Env{Runtime: runtime}
	args := []string{"LPUSH", "usage", `{"timestamp":"2026-11-06T10:27:15Z","provider":"claude","execution_id":"exec-1"}`}
	if reply := handleUsage(context.Background(), env, args); reply.Kind != dispatch.ReplyKindRedisError {
		t.Fatalf("database failure reply = %+v", reply)
	}
	adapter.err = nil
	if reply := handleUsage(context.Background(), env, args); reply.Kind != dispatch.ReplyKindInteger || reply.Integer != 1 || !adapter.inserted {
		t.Fatalf("committed write reply = %+v inserted=%v", reply, adapter.inserted)
	}
	if reply := handleUsage(context.Background(), env, args); reply.Kind != dispatch.ReplyKindInteger || reply.Integer != 1 || adapter.inserted {
		t.Fatalf("duplicate retry reply = %+v inserted=%v, want integer 1 and inserted=false", reply, adapter.inserted)
	}
}
