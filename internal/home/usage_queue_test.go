package home

import (
	"context"
	"errors"
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

type flakyUsageStore struct {
	failures int
	calls    int
	stored   []string
}

func (s *flakyUsageStore) StoreUsagePayload(_ context.Context, payload string, _ time.Time) error {
	s.calls++
	if s.calls <= s.failures {
		return errors.New("database is locked")
	}
	s.stored = append(s.stored, payload)
	return nil
}

func TestClusterUsageWriterRetriesFailedWrite(t *testing.T) {
	queue := newUsagePayloadQueue()
	store := &flakyUsageStore{failures: 2}
	if !queue.Push(`{"provider":"codex"}`, time.Time{}) {
		t.Fatal("usage queue rejected payload")
	}
	queue.Close()
	runClusterUsageWriterWithRetry(context.Background(), store, queue, []time.Duration{0, 0})
	if store.calls != 3 || len(store.stored) != 1 {
		t.Fatalf("calls = %d stored = %d, want 3 calls and 1 stored payload", store.calls, len(store.stored))
	}
}

func TestClusterUsageWriterStopsRetryingAfterLimit(t *testing.T) {
	queue := newUsagePayloadQueue()
	store := &flakyUsageStore{failures: 10}
	queue.Push(`{"provider":"codex"}`, time.Time{})
	queue.Push(`{"provider":"claude"}`, time.Time{})
	queue.Close()
	runClusterUsageWriterWithRetry(context.Background(), store, queue, []time.Duration{0})
	if store.calls != 4 || len(store.stored) != 0 {
		t.Fatalf("calls = %d stored = %d, want 2 attempts per payload and nothing stored", store.calls, len(store.stored))
	}
}

func TestUsagePayloadQueueDrainsAcceptedItemsAfterClose(t *testing.T) {
	queue := newUsagePayloadQueue()
	for _, payload := range []string{"a", "b"} {
		if !queue.Push(payload, time.Time{}) {
			t.Fatalf("Push(%s) rejected", payload)
		}
	}
	queue.Close()
	if queue.Push("c", time.Time{}) {
		t.Fatal("closed queue accepted a new payload")
	}
	for _, want := range []string{"a", "b"} {
		item, ok := queue.Pop()
		if !ok || item.payload != want {
			t.Fatalf("Pop() = %q, %v, want %q", item.payload, ok, want)
		}
	}
	if _, ok := queue.Pop(); ok {
		t.Fatal("Pop() on drained closed queue returned an item")
	}
}

func TestStopClusterUsageWriterDrainsPendingUsage(t *testing.T) {
	queue := newUsagePayloadQueue()
	store := &flakyUsageStore{}
	done := make(chan struct{})
	rt := &Runtime{clusterUsageQueue: queue, clusterUsageWriterDone: done}
	for _, payload := range []string{"a", "b", "c"} {
		queue.Push(payload, time.Time{})
	}
	go func() {
		defer close(done)
		runClusterUsageWriterWithRetry(context.Background(), store, queue, nil)
	}()
	rt.stopClusterUsageWriter()
	if len(store.stored) != 3 {
		t.Fatalf("stored = %v, want all queued payloads drained", store.stored)
	}
}
