package auth

import (
	"container/heap"
	"context"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

type authAutoRefreshLoop struct {
	manager     *Manager
	interval    time.Duration
	concurrency int

	mu    sync.Mutex
	queue refreshMinHeap
	index map[string]*refreshHeapItem
	dirty map[string]struct{}

	wakeCh chan struct{}
	jobs   chan string
}

// newAuthAutoRefreshLoop creates an auth auto refresh loop.
func newAuthAutoRefreshLoop(manager *Manager, interval time.Duration, concurrency int) *authAutoRefreshLoop {
	if interval <= 0 {
		interval = refreshCheckInterval
	}
	if concurrency <= 0 {
		concurrency = refreshMaxConcurrency
	}
	jobBuffer := concurrency * 4
	if jobBuffer < 64 {
		jobBuffer = 64
	}
	return &authAutoRefreshLoop{
		manager:     manager,
		interval:    interval,
		concurrency: concurrency,
		index:       make(map[string]*refreshHeapItem),
		dirty:       make(map[string]struct{}),
		wakeCh:      make(chan struct{}, 1),
		jobs:        make(chan string, jobBuffer),
	}
}

// queueReschedule queues a reschedule.
func (l *authAutoRefreshLoop) queueReschedule(authID string) {
	if l == nil || authID == "" {
		return
	}
	l.mu.Lock()
	l.dirty[authID] = struct{}{}
	l.mu.Unlock()
	select {
	case l.wakeCh <- struct{}{}:
	default:
	}
}

// run drives the background loop until it is stopped.
func (l *authAutoRefreshLoop) run(ctx context.Context) {
	if l == nil || l.manager == nil {
		return
	}

	workers := l.concurrency
	if workers <= 0 {
		workers = refreshMaxConcurrency
	}
	for i := 0; i < workers; i++ {
		go l.worker(ctx)
	}

	l.loop(ctx)
}

// worker processes queued auto-refresh work.
func (l *authAutoRefreshLoop) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case authID := <-l.jobs:
			if authID == "" {
				continue
			}
			resolved := l.manager.refreshAuth(ctx, authID)
			l.rescheduleAfterRefresh(time.Now().UTC(), authID, resolved)
		}
	}
}

// rebuild rebuilds the state.
func (l *authAutoRefreshLoop) rebuild(now time.Time) {
	// Keep validation before state changes so failures leave existing data intact.
	type entry struct {
		id   string
		next time.Time
	}

	entries := make([]entry, 0)

	l.manager.mu.RLock()
	for id, auth := range l.manager.auths {
		next, ok := nextRefreshCheckAt(now, auth, l.interval)
		if !ok {
			continue
		}
		entries = append(entries, entry{id: id, next: next})
	}
	l.manager.mu.RUnlock()

	l.mu.Lock()
	l.queue = l.queue[:0]
	l.index = make(map[string]*refreshHeapItem, len(entries))
	for _, e := range entries {
		item := &refreshHeapItem{id: e.id, next: e.next}
		heap.Push(&l.queue, item)
		l.index[e.id] = item
	}
	l.mu.Unlock()
}

// loop runs the scheduling loop until the context is canceled.
func (l *authAutoRefreshLoop) loop(ctx context.Context) {
	// Keep validation before state changes so failures leave existing data intact.
	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	defer timer.Stop()

	var timerCh <-chan time.Time
	l.resetTimer(timer, &timerCh, time.Now())

	for {
		select {
		case <-ctx.Done():
			return
		case <-l.wakeCh:
			now := time.Now()
			l.applyDirty(now)
			l.resetTimer(timer, &timerCh, now)
		case <-timerCh:
			now := time.Now()
			l.handleDue(ctx, now)
			l.applyDirty(now)
			l.resetTimer(timer, &timerCh, now)
		}
	}
}

// resetTimer resets a timer.
func (l *authAutoRefreshLoop) resetTimer(timer *time.Timer, timerCh *<-chan time.Time, now time.Time) {
	// Keep validation before state changes so failures leave existing data intact.
	next, ok := l.peek()
	if !ok {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		*timerCh = nil
		return
	}

	wait := next.Sub(now)
	if wait < 0 {
		wait = 0
	}
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(wait)
	*timerCh = timer.C
}

// peek returns the next queued item without removing it.
func (l *authAutoRefreshLoop) peek() (time.Time, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.queue) == 0 {
		return time.Time{}, false
	}
	return l.queue[0].next, true
}

// handleDue handles a due.
func (l *authAutoRefreshLoop) handleDue(ctx context.Context, now time.Time) {
	due := l.popDue(now)
	if len(due) == 0 {
		return
	}
	if log.IsLevelEnabled(log.DebugLevel) {
		log.Debugf("auto-refresh scheduler due auths: %d", len(due))
	}
	for _, authID := range due {
		l.handleDueAuth(ctx, now, authID)
	}
}

// popDue removes and returns a due.
func (l *authAutoRefreshLoop) popDue(now time.Time) []string {
	l.mu.Lock()
	defer l.mu.Unlock()

	var due []string
	for len(l.queue) > 0 {
		item := l.queue[0]
		if item == nil || item.next.After(now) {
			break
		}
		popped := heap.Pop(&l.queue).(*refreshHeapItem)
		if popped == nil {
			continue
		}
		delete(l.index, popped.id)
		due = append(due, popped.id)
	}
	return due
}

// handleDueAuth handles a due auth.
func (l *authAutoRefreshLoop) handleDueAuth(ctx context.Context, now time.Time, authID string) {
	// Validate request inputs before mutating persisted state.
	if authID == "" {
		return
	}

	manager := l.manager

	manager.mu.RLock()
	auth := manager.auths[authID]
	if auth == nil {
		manager.mu.RUnlock()
		return
	}
	next, shouldSchedule := nextRefreshCheckAt(now, auth, l.interval)
	shouldRefresh := manager.shouldRefresh(auth, now)
	manager.mu.RUnlock()

	if !shouldSchedule {
		l.remove(authID)
		return
	}

	if !shouldRefresh {
		l.upsert(authID, next)
		return
	}

	if !manager.markRefreshPending(authID, now) {
		manager.mu.RLock()
		auth = manager.auths[authID]
		next, shouldSchedule = nextRefreshCheckAt(now, auth, l.interval)
		manager.mu.RUnlock()
		if shouldSchedule {
			l.upsert(authID, next)
		} else {
			l.remove(authID)
		}
		return
	}

	manager.mu.RLock()
	auth = manager.auths[authID]
	next, shouldSchedule = nextRefreshCheckAt(now, auth, l.interval)
	manager.mu.RUnlock()
	if shouldSchedule {
		l.upsert(authID, next)
	} else {
		l.remove(authID)
	}

	select {
	case <-ctx.Done():
		return
	case l.jobs <- authID:
	}
}

// rescheduleAfterRefresh commits a resolved schedule only while the current
// index still matches the resolved storage revision.
func (l *authAutoRefreshLoop) rescheduleAfterRefresh(now time.Time, authID string, resolved *Auth) {
	if l == nil || l.manager == nil || authID == "" {
		return
	}

	l.manager.mu.RLock()
	current := l.manager.auths[authID]
	scheduled := current
	if current != nil && resolved != nil && current.StateVersion == resolved.StateVersion {
		scheduled = resolved
	}
	stateVersion := int64(0)
	if current != nil {
		stateVersion = current.StateVersion
	}
	next, ok := nextRefreshCheckAt(now, scheduled, l.interval)
	if ok {
		l.upsertAfterRefresh(authID, next, stateVersion)
	} else {
		l.remove(authID)
	}
	l.manager.mu.RUnlock()
	select {
	case l.wakeCh <- struct{}{}:
	default:
	}
}

// applyDirty applies a dirty.
func (l *authAutoRefreshLoop) applyDirty(now time.Time) {
	dirty := l.drainDirty()
	if len(dirty) == 0 {
		return
	}

	for _, authID := range dirty {
		l.manager.mu.RLock()
		auth := l.manager.auths[authID]
		next, ok := nextRefreshCheckAt(now, auth, l.interval)
		stateVersion := int64(0)
		if auth != nil {
			stateVersion = auth.StateVersion
		}
		l.manager.mu.RUnlock()

		if !ok {
			l.remove(authID)
			continue
		}
		l.upsertDirty(authID, next, stateVersion)
	}
}

// drainDirty drains a dirty.
func (l *authAutoRefreshLoop) drainDirty() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.dirty) == 0 {
		return nil
	}
	out := make([]string, 0, len(l.dirty))
	for authID := range l.dirty {
		out = append(out, authID)
		delete(l.dirty, authID)
	}
	return out
}

// upsert inserts or updates the record.
func (l *authAutoRefreshLoop) upsert(authID string, next time.Time) {
	if authID == "" || next.IsZero() {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.upsertLocked(authID, next, 0)
}

// upsertAfterRefresh records the state revision used by a completed worker.
func (l *authAutoRefreshLoop) upsertAfterRefresh(authID string, next time.Time, stateVersion int64) {
	if authID == "" || next.IsZero() {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.upsertLocked(authID, next, stateVersion)
}

// upsertDirty keeps an already committed schedule for the same or newer state revision.
func (l *authAutoRefreshLoop) upsertDirty(authID string, next time.Time, stateVersion int64) {
	if authID == "" || next.IsZero() {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if stateVersion > 0 {
		if item := l.index[authID]; item != nil && item.stateVersion >= stateVersion {
			return
		}
	}
	l.upsertLocked(authID, next, 0)
}

func (l *authAutoRefreshLoop) upsertLocked(authID string, next time.Time, stateVersion int64) {
	if item, ok := l.index[authID]; ok && item != nil {
		item.next = next
		item.stateVersion = stateVersion
		heap.Fix(&l.queue, item.index)
		return
	}
	item := &refreshHeapItem{id: authID, next: next, stateVersion: stateVersion}
	heap.Push(&l.queue, item)
	l.index[authID] = item
}

// remove removes the value.
func (l *authAutoRefreshLoop) remove(authID string) {
	if authID == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	item, ok := l.index[authID]
	if !ok || item == nil {
		return
	}
	heap.Remove(&l.queue, item.index)
	delete(l.index, authID)
}

// nextRefreshCheckAt returns a next refresh check at.
func nextRefreshCheckAt(now time.Time, auth *Auth, interval time.Duration) (time.Time, bool) {
	// Resolve credential context before calling upstream OAuth services.
	if authRefreshDisabled(auth) {
		return time.Time{}, false
	}

	accountType, _ := auth.AccountInfo()
	if accountType == "api_key" {
		return time.Time{}, false
	}

	if !auth.NextRefreshAfter.IsZero() && now.Before(auth.NextRefreshAfter) {
		return auth.NextRefreshAfter, true
	}

	if evaluator, ok := auth.Runtime.(RefreshEvaluator); ok && evaluator != nil {
		if interval <= 0 {
			interval = refreshCheckInterval
		}
		return now.Add(interval), true
	}

	builtInRefresh := authSupportsBuiltInRefresh(auth)
	if builtInRefresh && accessTokenForFingerprint(auth) == "" {
		return now, true
	}

	lastRefresh := auth.LastRefreshedAt
	if lastRefresh.IsZero() {
		if ts, ok := authLastRefreshTimestamp(auth); ok {
			lastRefresh = ts
		}
	}

	expiry, hasExpiry := auth.ExpirationTime()

	if pref := authPreferredInterval(auth); pref > 0 {
		candidates := make([]time.Time, 0, 2)
		if hasExpiry && !expiry.IsZero() {
			if !expiry.After(now) || expiry.Sub(now) <= pref {
				return now, true
			}
			candidates = append(candidates, expiry.Add(-pref))
		}
		if lastRefresh.IsZero() {
			return now, true
		}
		candidates = append(candidates, lastRefresh.Add(pref))
		next := candidates[0]
		for _, candidate := range candidates[1:] {
			if candidate.Before(next) {
				next = candidate
			}
		}
		if !next.After(now) {
			return now, true
		}
		return next, true
	}

	provider := strings.ToLower(auth.Provider)
	lead := ProviderRefreshLead(provider, auth.Runtime)
	if lead == nil {
		return time.Time{}, false
	}
	if hasExpiry && !expiry.IsZero() {
		dueAt := expiry.Add(-*lead)
		if !dueAt.After(now) {
			return now, true
		}
		return dueAt, true
	}
	if builtInRefresh {
		// A refresh lead is not a token lifetime. Recheck unknown expiry later
		// without calling the provider until the credential becomes explicitly due.
		if interval <= 0 {
			interval = refreshCheckInterval
		}
		return now.Add(interval), true
	}
	if !lastRefresh.IsZero() {
		dueAt := lastRefresh.Add(*lead)
		if !dueAt.After(now) {
			return now, true
		}
		return dueAt, true
	}
	return now, true
}

type refreshHeapItem struct {
	id           string
	next         time.Time
	index        int
	stateVersion int64
}

type refreshMinHeap []*refreshHeapItem

// Len returns the number of items.
func (h refreshMinHeap) Len() int { return len(h) }

// Less reports whether one item should sort before another.
func (h refreshMinHeap) Less(i, j int) bool {
	return h[i].next.Before(h[j].next)
}

// Swap exchanges two items.
func (h refreshMinHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].index = i
	h[j].index = j
}

// Push adds an item.
func (h *refreshMinHeap) Push(x any) {
	item, ok := x.(*refreshHeapItem)
	if !ok || item == nil {
		return
	}
	item.index = len(*h)
	*h = append(*h, item)
}

// Pop removes and returns the next item.
func (h *refreshMinHeap) Pop() any {
	old := *h
	n := len(old)
	if n == 0 {
		return (*refreshHeapItem)(nil)
	}
	item := old[n-1]
	item.index = -1
	*h = old[:n-1]
	return item
}
