package home

import (
	"context"
	"errors"
	"testing"
	"time"
)

type reviewUsageAdapter struct {
	ClusterAdapter
	err        error
	payload    string
	receivedAt time.Time
}

func (a *reviewUsageAdapter) Enabled() bool { return true }
func (a *reviewUsageAdapter) StoreUsagePayload(_ context.Context, payload string, receivedAt time.Time) (bool, error) {
	a.payload, a.receivedAt = payload, receivedAt
	return a.err == nil, a.err
}

func TestPersistClusterUsageAcknowledgesOnlyCommittedWrites(t *testing.T) {
	adapter := &reviewUsageAdapter{err: errors.New("database unavailable")}
	runtime := &Runtime{clusterAdapter: adapter}
	receivedAt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	persisted, inserted, errPersist := runtime.PersistClusterUsagePayload(context.Background(), "payload", receivedAt)
	if persisted || inserted || errPersist == nil {
		t.Fatalf("failed write: persisted=%v inserted=%v err=%v", persisted, inserted, errPersist)
	}
	adapter.err = nil
	persisted, inserted, errPersist = runtime.PersistClusterUsagePayload(context.Background(), "payload", receivedAt)
	if !persisted || !inserted || errPersist != nil || adapter.payload != "payload" || !adapter.receivedAt.Equal(receivedAt) {
		t.Fatalf("committed write: persisted=%v inserted=%v err=%v payload=%q time=%v", persisted, inserted, errPersist, adapter.payload, adapter.receivedAt)
	}
}
