package home

import (
	"context"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

const clusterUsageWriterDrainTimeout = 10 * time.Second

// clusterUsageWriteRetryDelays are the waits between attempts for one usage payload.
// A failed write rolls back its transaction, so retrying does not duplicate usage or charges.
var clusterUsageWriteRetryDelays = []time.Duration{200 * time.Millisecond, time.Second}

type usagePayloadItem struct {
	payload    string
	receivedAt time.Time
}

type usagePayloadQueue struct {
	mu     sync.Mutex
	cond   *sync.Cond
	items  []usagePayloadItem
	head   int
	closed bool
}

func newUsagePayloadQueue() *usagePayloadQueue {
	q := &usagePayloadQueue{}
	q.cond = sync.NewCond(&q.mu)
	return q
}

func (q *usagePayloadQueue) Push(payload string, receivedAt time.Time) bool {
	if q == nil {
		return false
	}
	if receivedAt.IsZero() {
		receivedAt = time.Now().UTC()
	} else {
		receivedAt = receivedAt.UTC()
	}

	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return false
	}
	q.items = append(q.items, usagePayloadItem{payload: payload, receivedAt: receivedAt})
	q.cond.Signal()
	return true
}

func (q *usagePayloadQueue) Pop() (usagePayloadItem, bool) {
	if q == nil {
		return usagePayloadItem{}, false
	}

	q.mu.Lock()
	defer q.mu.Unlock()
	for q.head >= len(q.items) && !q.closed {
		q.cond.Wait()
	}
	// A closed queue still drains accepted items so shutdown does not drop usage.
	if q.head >= len(q.items) {
		return usagePayloadItem{}, false
	}

	item := q.items[q.head]
	q.items[q.head] = usagePayloadItem{}
	q.head++
	q.compactLocked()
	return item, true
}

func (q *usagePayloadQueue) Close() {
	if q == nil {
		return
	}

	q.mu.Lock()
	q.closed = true
	q.cond.Broadcast()
	q.mu.Unlock()
}

// Len returns the number of accepted items that have not been popped yet.
func (q *usagePayloadQueue) Len() int {
	if q == nil {
		return 0
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items) - q.head
}

func (q *usagePayloadQueue) compactLocked() {
	if q.head < 1024 || q.head*2 < len(q.items) {
		return
	}
	next := append([]usagePayloadItem(nil), q.items[q.head:]...)
	q.items = next
	q.head = 0
}

func (r *Runtime) startClusterUsageWriter(ctx context.Context) {
	if r == nil || r.clusterAdapter == nil || !r.clusterAdapter.Enabled() {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	store, ok := r.clusterAdapter.(clusterUsageStore)
	if !ok || store == nil {
		log.Errorf("cluster usage store is unavailable")
		return
	}

	queue := newUsagePayloadQueue()
	done := make(chan struct{})
	r.clusterUsageQueueMu.Lock()
	if r.clusterUsageQueue != nil {
		r.clusterUsageQueueMu.Unlock()
		return
	}
	r.clusterUsageQueue = queue
	r.clusterUsageWriterDone = done
	r.clusterUsageQueueMu.Unlock()

	go func() {
		<-ctx.Done()
		queue.Close()
	}()
	go func() {
		defer close(done)
		// Writes outlive runtime cancellation so queued usage can drain during shutdown.
		r.runClusterUsageWriter(context.WithoutCancel(ctx), store, queue)
	}()
}

func (r *Runtime) stopClusterUsageWriter() {
	if r == nil {
		return
	}

	r.clusterUsageQueueMu.Lock()
	queue := r.clusterUsageQueue
	done := r.clusterUsageWriterDone
	r.clusterUsageQueue = nil
	r.clusterUsageWriterDone = nil
	r.clusterUsageQueueMu.Unlock()
	if queue == nil {
		return
	}
	queue.Close()
	if done == nil {
		return
	}
	timer := time.NewTimer(clusterUsageWriterDrainTimeout)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		log.WithField("pending", queue.Len()).Warn("cluster usage writer drain timed out")
	}
}

func (r *Runtime) getClusterUsageQueue() *usagePayloadQueue {
	if r == nil {
		return nil
	}

	r.clusterUsageQueueMu.Lock()
	defer r.clusterUsageQueueMu.Unlock()
	return r.clusterUsageQueue
}

func (r *Runtime) runClusterUsageWriter(ctx context.Context, store clusterUsageStore, queue *usagePayloadQueue) {
	runClusterUsageWriterWithRetry(ctx, store, queue, clusterUsageWriteRetryDelays)
}

func runClusterUsageWriterWithRetry(ctx context.Context, store clusterUsageStore, queue *usagePayloadQueue, retryDelays []time.Duration) {
	if store == nil || queue == nil {
		return
	}

	for {
		item, ok := queue.Pop()
		if !ok {
			return
		}
		if strings.TrimSpace(item.payload) == "" {
			continue
		}
		storeClusterUsagePayloadWithRetry(ctx, store, item, retryDelays)
	}
}

func storeClusterUsagePayloadWithRetry(ctx context.Context, store clusterUsageStore, item usagePayloadItem, retryDelays []time.Duration) {
	attempts := len(retryDelays) + 1
	for attempt := 1; ; attempt++ {
		errStoreUsagePayload := store.StoreUsagePayload(ctx, item.payload, item.receivedAt)
		if errStoreUsagePayload == nil {
			return
		}
		entry := log.WithError(errStoreUsagePayload).WithFields(log.Fields{"attempt": attempt, "attempts": attempts})
		if attempt >= attempts || ctx.Err() != nil {
			entry.Error("usage database async write failed; usage dropped")
			return
		}
		entry.Warn("usage database async write failed; retrying")
		if delay := retryDelays[attempt-1]; delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
			case <-timer.C:
			}
		}
	}
}
