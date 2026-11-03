package cluster

import (
	"context"
	"database/sql"
	"time"

	log "github.com/sirupsen/logrus"
)

// observeDatabasePool only reads in-process counters: it never acquires a
// connection, queries the server, or changes pool settings. The first sample
// establishes a baseline; subsequent deltas cover the actual elapsed interval.
func observeDatabasePool(ctx context.Context, db *sql.DB, backend string, ticks <-chan time.Time) {
	var previous sql.DBStats
	var previousAt time.Time
	for ctx.Err() == nil {
		now := time.Now()
		current := db.Stats()
		initial := previousAt.IsZero()
		var interval time.Duration
		if initial {
			previous = current
		} else {
			interval = now.Sub(previousAt)
		}
		log.Infof("cluster database pool backend=%s initial=%t interval_ms=%.3f max_open=%d open=%d in_use=%d idle=%d wait_count=%d wait_count_delta=%d wait_ms=%.3f wait_ms_delta=%.3f max_idle_closed=%d max_idle_closed_delta=%d max_idle_time_closed=%d max_idle_time_closed_delta=%d max_lifetime_closed=%d max_lifetime_closed_delta=%d",
			backend, initial, float64(interval)/float64(time.Millisecond), current.MaxOpenConnections, current.OpenConnections, current.InUse, current.Idle,
			current.WaitCount, current.WaitCount-previous.WaitCount, float64(current.WaitDuration)/float64(time.Millisecond), float64(current.WaitDuration-previous.WaitDuration)/float64(time.Millisecond),
			current.MaxIdleClosed, current.MaxIdleClosed-previous.MaxIdleClosed, current.MaxIdleTimeClosed, current.MaxIdleTimeClosed-previous.MaxIdleTimeClosed, current.MaxLifetimeClosed, current.MaxLifetimeClosed-previous.MaxLifetimeClosed)
		previous, previousAt = current, now
		select {
		case <-ctx.Done():
			return
		case _, ok := <-ticks:
			if !ok {
				return
			}
		}
	}
}
