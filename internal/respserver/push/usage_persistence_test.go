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
	err error
}

func (a *usagePersistenceAdapter) Enabled() bool { return true }
func (a *usagePersistenceAdapter) StoreUsagePayload(context.Context, string, time.Time) error {
	return a.err
}

func TestUsagePushRespondsWithErrorOnDatabaseFailure(t *testing.T) {
	adapter := &usagePersistenceAdapter{err: errors.New("unavailable")}
	runtime := &home.Runtime{}
	runtime.SetClusterAdapter(adapter)
	env := dispatch.Env{Runtime: runtime}
	args := []string{"LPUSH", "usage", `{"provider":"claude"}`}
	if reply := handleUsage(context.Background(), env, args); reply.Kind != dispatch.ReplyKindRedisError {
		t.Fatalf("database failure reply = %+v", reply)
	}
	adapter.err = nil
	if reply := handleUsage(context.Background(), env, args); reply.Kind != dispatch.ReplyKindInteger || reply.Integer != 1 {
		t.Fatalf("committed write reply = %+v", reply)
	}
}
