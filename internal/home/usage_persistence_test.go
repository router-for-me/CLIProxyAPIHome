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
func (a *reviewUsageAdapter) StoreUsagePayload(_ context.Context, payload string, receivedAt time.Time) error {
	a.payload, a.receivedAt = payload, receivedAt
	return a.err
}

func TestPersistClusterUsageAcknowledgesOnlyCommittedWrites(t *testing.T) {
	adapter := &reviewUsageAdapter{err: errors.New("database unavailable")}
	runtime := &Runtime{clusterAdapter: adapter}
	receivedAt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	persisted, errPersist := runtime.PersistClusterUsagePayload(context.Background(), "payload", receivedAt)
	if persisted || errPersist == nil {
		t.Fatalf("failed write: persisted=%v err=%v", persisted, errPersist)
	}
	adapter.err = nil
	persisted, errPersist = runtime.PersistClusterUsagePayload(context.Background(), "payload", receivedAt)
	if !persisted || errPersist != nil || adapter.payload != "payload" || !adapter.receivedAt.Equal(receivedAt) {
		t.Fatalf("committed write: persisted=%v err=%v payload=%q time=%v", persisted, errPersist, adapter.payload, adapter.receivedAt)
	}
}
