package home

import (
	"context"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

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
	if q.closed {
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
	q.items = nil
	q.head = 0
	q.cond.Broadcast()
	q.mu.Unlock()
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
	r.clusterUsageQueueMu.Lock()
	if r.clusterUsageQueue != nil {
		r.clusterUsageQueueMu.Unlock()
		return
	}
	r.clusterUsageQueue = queue
	r.clusterUsageQueueMu.Unlock()

	go func() {
		<-ctx.Done()
		queue.Close()
	}()
	go r.runClusterUsageWriter(ctx, store, queue)
}

func (r *Runtime) stopClusterUsageWriter() {
	if r == nil {
		return
	}

	r.clusterUsageQueueMu.Lock()
	queue := r.clusterUsageQueue
	r.clusterUsageQueue = nil
	r.clusterUsageQueueMu.Unlock()
	if queue != nil {
		queue.Close()
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
		if errStoreUsagePayload := store.StoreUsagePayload(ctx, item.payload, item.receivedAt); errStoreUsagePayload != nil {
			log.Errorf("usage database async write error: %v", errStoreUsagePayload)
		}
	}
}
