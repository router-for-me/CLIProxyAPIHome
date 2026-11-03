package cluster

import (
	"context"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	expiredCleanupBatch    = 1000
	expiredCleanupBudget   = time.Second
	expiredCleanupYield    = 10 * time.Millisecond
	expiredCleanupIdle     = time.Minute
	expiredCleanupRetry    = time.Second
	expiredCleanupMaxRetry = time.Minute
)

// runExpiredRecordsCleanupLoop is the background-task error boundary. Failed
// deletes are reported and retried with bounded backoff. Each store runs in its
// own goroutine, and only the elected master drains batches.
func (c *Coordinator) runExpiredRecordsCleanupLoop(ctx context.Context, name string, purge func(context.Context, time.Time, int) (int64, error)) {
	runExpiredRecordsCleanup(ctx, name, purge, c.IsMaster, time.Now, func(delay time.Duration) bool {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return false
		case <-timer.C:
			return true
		}
	})
}

func runExpiredRecordsCleanup(ctx context.Context, name string, purge func(context.Context, time.Time, int) (int64, error), isMaster func() bool, now func() time.Time, wait func(time.Duration) bool) {
	retry := expiredCleanupRetry
	for ctx.Err() == nil {
		// Standbys poll only local lease state, allowing takeover within a
		// second without adding database queries or replacing refresh callbacks.
		delay := expiredCleanupRetry
		if isMaster() {
			delay = expiredCleanupIdle
			started := now()
			workCtx, cancelWork := context.WithTimeout(ctx, expiredCleanupBudget)
			for ctx.Err() == nil && isMaster() {
				deleted, errPurge := purge(workCtx, now().UTC(), expiredCleanupBatch)
				if errPurge != nil {
					if ctx.Err() == nil {
						log.Warnf("%s cleanup failed: %v; retry after %s", name, errPurge, retry)
					}
					delay = retry
					retry = min(retry*2, expiredCleanupMaxRetry)
					break
				}
				retry = expiredCleanupRetry
				if deleted < expiredCleanupBatch {
					break
				}
				if now().Sub(started) >= expiredCleanupBudget || workCtx.Err() != nil {
					delay = expiredCleanupYield
					break
				}
			}
			cancelWork()
			if !isMaster() {
				delay = expiredCleanupRetry
			}
		}
		if ctx.Err() != nil || !wait(delay) {
			return
		}
	}
}
