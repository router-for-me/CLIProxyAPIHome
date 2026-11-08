package push

import (
	"context"
	"fmt"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/home"
	"github.com/router-for-me/CLIProxyAPIHome/internal/respserver/dispatch"
)

func TestUsagePushFenceOnlyFeedbackSurvivesRedeliveryAfterExpiration(t *testing.T) {
	for _, test := range []struct {
		name       string
		secondNode int
	}{
		{name: "same node", secondNode: 0},
		{name: "peer node", secondNode: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			testUsagePushFenceOnlyFeedbackSurvivesRedelivery(t, test.secondNode)
		})
	}
}

func testUsagePushFenceOnlyFeedbackSurvivesRedelivery(t *testing.T, secondNode int) {
	t.Helper()
	ctx := context.Background()
	db, firstNode := newUsageIngestionTestRuntime(t, true)
	repo := cluster.NewRepository(db)
	nodes := []*home.Runtime{firstNode}
	for i := 0; i < 2; i++ {
		rt, errRuntime := home.NewRuntime(&config.Config{})
		if errRuntime != nil {
			t.Fatal(errRuntime)
		}
		rt.SetClusterAdapter(cluster.NewRuntimeAdapter(repo, "192.0.2.11"))
		t.Cleanup(rt.Stop)
		if _, errRegister := rt.CoreManager().Register(coreauth.WithSkipPersist(ctx), &coreauth.Auth{ID: "usage-ingestion-auth", Index: "usage-ingestion-auth", Provider: "codex", Status: coreauth.StatusActive}); errRegister != nil {
			t.Fatal(errRegister)
		}
		nodes = append(nodes, rt)
	}
	at := time.Now().UTC().Add(-2 * time.Minute)
	first := []string{"LPUSH", "usage", fmt.Sprintf(`{"timestamp":%q,"auth_index":"usage-ingestion-auth","provider":"codex","model":"gpt-5","execution_id":"quota-first","failed":true,"fail":{"status_code":429,"body":"{\"error\":{\"type\":\"usage_limit_reached\",\"resets_in_seconds\":3600}}"}}`, at.Format(time.RFC3339Nano))}
	second := []string{"LPUSH", "usage", fmt.Sprintf(`{"timestamp":%q,"auth_index":"usage-ingestion-auth","provider":"codex","model":"gpt-5","execution_id":"quota-second","failed":true,"fail":{"status_code":429}}`, at.Add(time.Minute).Format(time.RFC3339Nano))}
	if reply := handleUsage(ctx, dispatch.Env{Runtime: nodes[0]}, first); reply.Kind != dispatch.ReplyKindInteger {
		t.Fatal(reply)
	}
	if reply := handleUsage(ctx, dispatch.Env{Runtime: nodes[secondNode]}, second); reply.Kind != dispatch.ReplyKindInteger {
		t.Fatal(reply)
	}
	auth, _, errGet := repo.GetAuth(ctx, "usage-ingestion-auth")
	if errGet != nil {
		t.Fatal(errGet)
	}
	if !auth.ModelStates["gpt-5"].LastObservedAt.Equal(at.Add(time.Minute)) {
		t.Fatal("in-window feedback did not save its shared observation fence")
	}
	// Expire the window deterministically, without waiting on platform timers.
	expiredAt := at
	if _, _, _, errExpire := repo.MutateAuth(ctx, "usage-ingestion-auth", "test", func(auth *coreauth.Auth) bool {
		auth.ModelStates["gpt-5"].NextRetryAfter = expiredAt
		auth.ModelStates["gpt-5"].Quota.NextRecoverAt = expiredAt
		auth.NextRetryAfter = expiredAt
		auth.Quota.NextRecoverAt = expiredAt
		return true
	}); errExpire != nil {
		t.Fatal(errExpire)
	}
	if reply := handleUsage(ctx, dispatch.Env{Runtime: nodes[2]}, second); reply.Kind != dispatch.ReplyKindInteger {
		t.Fatal(reply)
	}
	after, _, errAfter := repo.GetAuth(ctx, "usage-ingestion-auth")
	if errAfter != nil {
		t.Fatal(errAfter)
	}
	if state := after.ModelStates["gpt-5"]; state.Quota.BackoffLevel != 1 || !state.Quota.NextRecoverAt.Equal(expiredAt) {
		t.Fatalf("deduplicated feedback restarted cooldown: %+v", state)
	}
	var count int64
	if errCount := db.Model(&cluster.UsageRecord{}).Count(&count).Error; errCount != nil || count != 2 {
		t.Fatalf("usage count = %d, error = %v; want 2, nil", count, errCount)
	}
}

func TestUsagePushAcknowledgesFeedbackForDeletedLoadedCredential(t *testing.T) {
	for _, status := range []int{200, 401, 429} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			ctx := context.Background()
			db, rt := newUsageIngestionTestRuntime(t, true)
			repo := cluster.NewRepository(db)
			if errDelete := repo.SoftDeleteAuth(ctx, "usage-ingestion-auth"); errDelete != nil {
				t.Fatal(errDelete)
			}
			payload := fmt.Sprintf(`{"timestamp":%q,"auth_index":"usage-ingestion-auth","provider":"codex","model":"gpt-5","execution_id":"deleted-feedback","failed":%v,"fail":{"status_code":%d}}`, time.Now().UTC().Format(time.RFC3339Nano), status != 200, status)
			args := []string{"LPUSH", "usage", payload}
			for i := 0; i < 2; i++ {
				if reply := handleUsage(ctx, dispatch.Env{Runtime: rt}, args); reply.Kind != dispatch.ReplyKindInteger {
					t.Fatalf("delivery %d was not acknowledged: %+v", i, reply)
				}
			}
			var count int64
			if errCount := db.Model(&cluster.UsageRecord{}).Count(&count).Error; errCount != nil || count != 1 {
				t.Fatalf("usage count = %d, error = %v; want 1, nil", count, errCount)
			}
		})
	}
}

func TestUsagePushAcknowledgesInvalidTimingWithoutPoisoningFeedback(t *testing.T) {
	for _, test := range []struct {
		name    string
		offset  time.Duration
		latency string
	}{
		{name: "future clock", offset: 24 * time.Hour, latency: "0"},
		{name: "excessive latency", latency: "86400000"},
		{name: "overflow latency", latency: "9223372036854775807"},
		{name: "negative latency", latency: "-1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			db, rt := newUsageIngestionTestRuntime(t, true)
			at := time.Now().UTC()
			invalid := []string{"LPUSH", "usage", fmt.Sprintf(`{"timestamp":%q,"latency_ms":%s,"auth_index":"usage-ingestion-auth","provider":"codex","model":"gpt-5","execution_id":"invalid-time","failed":true,"fail":{"status_code":429}}`, at.Add(test.offset).Format(time.RFC3339Nano), test.latency)}
			if reply := handleUsage(ctx, dispatch.Env{Runtime: rt}, invalid); reply.Kind != dispatch.ReplyKindInteger {
				t.Fatalf("invalid timing was not acknowledged: %+v", reply)
			}
			local, _ := rt.CoreManager().GetByID("usage-ingestion-auth")
			if local.ModelStates["gpt-5"] != nil {
				t.Fatal("invalid timing changed scheduler state")
			}
			// Both a healthy failure and a subsequent success must still apply.
			for i, status := range []int{429, 200} {
				observedAt := at.Add(time.Duration(i)*time.Second - time.Minute)
				payload := fmt.Sprintf(`{"timestamp":%q,"auth_index":"usage-ingestion-auth","provider":"codex","model":"gpt-5","execution_id":"healthy-%d","failed":%v,"fail":{"status_code":%d}}`, observedAt.Format(time.RFC3339Nano), status, status != 200, status)
				if reply := handleUsage(ctx, dispatch.Env{Runtime: rt}, []string{"LPUSH", "usage", payload}); reply.Kind != dispatch.ReplyKindInteger {
					t.Fatal(reply)
				}
				current, _ := rt.CoreManager().GetByID("usage-ingestion-auth")
				if state := current.ModelStates["gpt-5"]; state == nil || state.Quota.Exceeded != (status == 429) {
					t.Fatalf("healthy status %d was not applied: %+v", status, state)
				}
			}
			var count int64
			if errCount := db.Model(&cluster.UsageRecord{}).Count(&count).Error; errCount != nil || count != 3 {
				t.Fatalf("usage count = %d, error = %v; want 3, nil", count, errCount)
			}
		})
	}
}
