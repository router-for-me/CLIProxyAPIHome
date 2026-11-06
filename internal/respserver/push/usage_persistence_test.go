package push

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPIHome/internal/home"
	"github.com/router-for-me/CLIProxyAPIHome/internal/respserver/dispatch"
)

type usagePersistenceAdapter struct {
	home.ClusterAdapter
	err      error
	inserted bool
	seen     map[string]bool
}

func (a *usagePersistenceAdapter) Enabled() bool { return true }
func (a *usagePersistenceAdapter) StoreUsagePayload(ctx context.Context, payload string, receivedAt time.Time) error {
	_, errStore := a.StoreUsagePayloadOutcome(ctx, payload, receivedAt)
	return errStore
}

func (a *usagePersistenceAdapter) StoreUsagePayloadOutcome(_ context.Context, payload string, _ time.Time) (bool, error) {
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

func TestUsagePushRespondsWithErrorOnDatabaseFailure(t *testing.T) {
	adapter := &usagePersistenceAdapter{err: errors.New("unavailable")}
	runtime := &home.Runtime{}
	runtime.SetClusterAdapter(adapter)
	env := dispatch.Env{Runtime: runtime}
	args := []string{"LPUSH", "usage", `{"provider":"claude","execution_id":"exec-1"}`}
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
