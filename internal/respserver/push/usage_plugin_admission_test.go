package push

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	"github.com/router-for-me/CLIProxyAPIHome/internal/respserver/dispatch"
)

type blockingAdmissionUsageSink struct {
	entered chan struct{}
	release <-chan struct{}
	calls   atomic.Int32
}

func (s *blockingAdmissionUsageSink) HandleUsage(context.Context, pluginapi.UsageRecord) error {
	s.calls.Add(1)
	s.entered <- struct{}{}
	<-s.release
	return nil
}

func TestUsageIngestionSQLiteAdmissionReturnsRetryableErrors(t *testing.T) {
	db, rt := newUsageIngestionTestRuntime(t, true)
	const limit, excess = 8, 32
	release := make(chan struct{})
	sink := &blockingAdmissionUsageSink{entered: make(chan struct{}, limit+excess), release: release}
	runtime := &pluginUsageIngestionRuntime{Runtime: rt, sink: sink}
	var releaseOnce sync.Once
	var workers sync.WaitGroup
	defer func() {
		releaseOnce.Do(func() { close(release) })
		workers.Wait()
	}()
	payloads := make([]string, limit+excess)
	for index := range payloads {
		payloads[index] = fmt.Sprintf(`{"timestamp":"2026-11-06T10:27:15Z","provider":"codex","execution_id":"sqlite-ingestion-admission-%d"}`, index)
	}
	owners := make(chan dispatch.Reply, limit)
	for _, payload := range payloads[:limit] {
		workers.Go(func() {
			owners <- ingestUsagePayload(t.Context(), dispatch.Env{}, runtime, payload, time.Now().UTC())
		})
		select {
		case <-sink.entered:
		case reply := <-owners:
			t.Fatalf("owner returned before callback: %+v", reply)
		case <-time.After(5 * time.Second):
			t.Fatal("callbacks did not fill SQLite capacity")
		}
	}
	rejected := make(chan dispatch.Reply, excess)
	for _, payload := range payloads[limit:] {
		workers.Go(func() {
			rejected <- ingestUsagePayload(t.Context(), dispatch.Env{}, runtime, payload, time.Now().UTC())
		})
	}
	for range excess {
		select {
		case reply := <-rejected:
			if reply.Kind != dispatch.ReplyKindRedisError {
				t.Fatalf("excess ingestion was acknowledged: %+v", reply)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("excess ingestion blocked instead of returning a retryable error")
		}
	}
	if sink.calls.Load() != limit {
		t.Fatalf("active callbacks=%d, want %d", sink.calls.Load(), limit)
	}
	var rows []cluster.UsageRecord
	if errFind := db.Find(&rows).Error; errFind != nil || len(rows) != len(payloads) {
		t.Fatalf("usage persistence: rows=%d error=%v", len(rows), errFind)
	}
	for _, row := range rows {
		if row.PluginUsageDelivered {
			t.Fatal("blocked or rejected event marked complete")
		}
	}
	releaseOnce.Do(func() { close(release) })
	for range limit {
		if reply := <-owners; reply.Kind != dispatch.ReplyKindInteger {
			t.Fatalf("completed callback reply=%+v", reply)
		}
	}
	workers.Wait()
	for range 2 {
		for _, payload := range payloads {
			if reply := ingestUsagePayload(t.Context(), dispatch.Env{}, runtime, payload, time.Now().UTC()); reply.Kind != dispatch.ReplyKindInteger {
				t.Fatalf("capacity recovery retry=%+v", reply)
			}
		}
	}
	if sink.calls.Load() != int32(len(payloads)) {
		t.Fatalf("retry callback count=%d, want one per distinct event (%d)", sink.calls.Load(), len(payloads))
	}
	if errFind := db.Find(&rows).Error; errFind != nil || len(rows) != len(payloads) {
		t.Fatalf("retry changed usage row count: rows=%d error=%v", len(rows), errFind)
	}
	for _, row := range rows {
		if !row.PluginUsageDelivered {
			t.Fatal("successful retry did not complete its event")
		}
	}
}
