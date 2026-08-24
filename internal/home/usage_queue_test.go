package home

import (
	"context"
	"testing"
	"time"
)

type capturedUsagePayload struct {
	payload    string
	receivedAt time.Time
}

type capturingUsageStore struct {
	items chan capturedUsagePayload
}

func (s *capturingUsageStore) StoreUsagePayload(_ context.Context, payload string, receivedAt time.Time) error {
	s.items <- capturedUsagePayload{payload: payload, receivedAt: receivedAt}
	return nil
}

func TestClusterUsageWriterPreservesEnqueueTime(t *testing.T) {
	queue := newUsagePayloadQueue()
	t.Cleanup(queue.Close)
	store := &capturingUsageStore{items: make(chan capturedUsagePayload, 1)}
	done := make(chan struct{})
	go func() {
		(&Runtime{}).runClusterUsageWriter(context.Background(), store, queue)
		close(done)
	}()

	receivedAt := time.Date(2026, 8, 24, 1, 2, 3, 456789000, time.FixedZone("test", 8*60*60))
	if !queue.Push(`{"provider":"codex"}`, receivedAt) {
		t.Fatal("usage queue rejected payload")
	}
	select {
	case captured := <-store.items:
		if captured.payload != `{"provider":"codex"}` || !captured.receivedAt.Equal(receivedAt) || captured.receivedAt.Location() != time.UTC {
			t.Fatalf("captured usage = %#v, want payload and UTC receive time %v", captured, receivedAt.UTC())
		}
	case <-time.After(time.Second):
		t.Fatal("usage writer did not store queued payload")
	}

	queue.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("usage writer did not stop after queue close")
	}
}
