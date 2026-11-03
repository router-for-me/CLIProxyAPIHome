package cluster

import (
	"context"
	"time"

	"github.com/router-for-me/CLIProxyAPIHome/internal/node"
	log "github.com/sirupsen/logrus"
)

const cpaNodeSnapshotRefreshBudget = 5 * time.Second

// requestCPANodeSnapshotRefresh coalesces requests without retaining stale node data.
func (c *Coordinator) requestCPANodeSnapshotRefresh() {
	select {
	case c.snapshotRefresh <- struct{}{}:
	default:
	}
}

type cpaNodeSnapshotScheduler struct {
	coordinator *Coordinator
	pending     bool
	lastSuccess time.Time
}

// runCPANodeSnapshotLoop owns all runtime refreshes. Ticks also bound failure retries.
func (c *Coordinator) runCPANodeSnapshotLoop(ctx context.Context, ticks <-chan time.Time, startedAt time.Time) {
	scheduler := cpaNodeSnapshotScheduler{coordinator: c, lastSuccess: startedAt}
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.snapshotRefresh:
			scheduler.pending = true
		case scheduledAt := <-ticks:
			scheduler.refresh(ctx, scheduledAt)
		}
	}
}

func (s *cpaNodeSnapshotScheduler) refresh(ctx context.Context, scheduledAt time.Time) {
	if ctx.Err() != nil {
		return
	}
	c := s.coordinator
	// Include notifications already queued even when select chose the tick first.
	select {
	case <-c.snapshotRefresh:
		s.pending = true
	default:
	}
	if !s.pending && scheduledAt.Sub(s.lastSuccess) < c.heartbeatInterval {
		return
	}
	home, initialized := c.HomeIncarnation()
	if !initialized {
		return
	}
	refreshCtx, cancelRefresh := context.WithTimeout(ctx, cpaNodeSnapshotRefreshBudget)
	errSnapshot := c.syncCPANodeSnapshot(refreshCtx, home)
	cancelRefresh()
	if ctx.Err() != nil {
		return
	}
	if errSnapshot != nil {
		s.pending = true
		log.Warnf("failed to sync CPA node snapshot: %v", errSnapshot)
		return
	}
	s.lastSuccess = scheduledAt
	s.pending = false
}

func (c *Coordinator) syncCPANodeSnapshot(ctx context.Context, home HomeIncarnationID) error {
	seenAt, errNow := c.repo.CurrentDatabaseTime(ctx)
	if errNow != nil {
		return errNow
	}
	return c.repo.ReplaceCPANodeSnapshotForIncarnation(ctx, home, node.GlobalRegistry().List(), seenAt)
}
