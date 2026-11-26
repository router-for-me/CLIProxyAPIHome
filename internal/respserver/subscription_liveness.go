package respserver

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	log "github.com/sirupsen/logrus"
)

var errSubscriptionHeartbeatExpired = errors.New("subscription heartbeat persistence deadline exceeded")

// subscriptionLiveness tracks local monotonic time, not the database's wall clock.
// Only successful heartbeat persistence can renew this membership lifetime's deadline.
type subscriptionLiveness struct {
	mu       sync.Mutex
	timeout  time.Duration
	deadline time.Time
	expired  bool
	changed  chan struct{}
}

func newSubscriptionLiveness(timeout time.Duration) *subscriptionLiveness {
	if timeout <= 0 {
		timeout = config.DefaultCPAHeartbeatTimeout
	}
	return &subscriptionLiveness{
		timeout: timeout, deadline: time.Now().Add(timeout), changed: make(chan struct{}, 1),
	}
}

func (l *subscriptionLiveness) currentDeadline() time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.deadline
}

func (l *subscriptionLiveness) persisted() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if l.expired || !now.Before(l.deadline) {
		return errSubscriptionHeartbeatExpired
	}
	l.deadline = now.Add(l.timeout)
	select {
	case l.changed <- struct{}{}:
	default:
	}
	return nil
}

// remaining atomically latches expiry so a late database response cannot revive a lifetime.
func (l *subscriptionLiveness) remaining() time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	remaining := time.Until(l.deadline)
	if l.expired || remaining <= 0 {
		l.expired = true
		return 0
	}
	return remaining
}

// watchSubscriptionLiveness belongs to the exact lifetime entry, not a subscription socket.
// Disconnect, unsubscribe, and reattachment cannot cancel or reset its isolation deadline.
// Register it immediately after membership persistence, before any other blocking work.
func (r *FingerprintRegistry) watchSubscriptionLiveness(lifetime cluster.ConnectionLifetime) (*subscriptionLiveness, error) {
	r.mu.Lock()
	entry := r.entryLocked(lifetimeKey(lifetime))
	entry.mu.Lock()
	r.mu.Unlock()
	defer entry.mu.Unlock()
	if entry.fenced {
		return nil, ErrFingerprintFenced
	}
	if entry.liveness != nil {
		if entry.liveness.remaining() == 0 {
			return nil, errSubscriptionHeartbeatExpired
		}
		return entry.liveness, nil
	}
	liveness := newSubscriptionLiveness(lifetime.HeartbeatTimeout)
	entry.liveness = liveness
	go func() {
		timer := time.NewTimer(liveness.remaining())
		defer timer.Stop()
		for {
			select {
			case <-entry.fencedSignal:
				return
			case <-liveness.changed:
			case <-timer.C:
			}
			remaining := liveness.remaining()
			if remaining > 0 {
				timer.Reset(remaining)
				continue
			}
			log.Warn("subscription heartbeat persistence expired; fencing lifetime")
			if errFence := r.Fence(context.Background(), lifetime, r.LatestFenceRevision(lifetime)); errFence != nil {
				log.Warnf("failed to fence subscription after heartbeat expiry: %v", errFence)
			}
			return
		}
	}()
	return liveness, nil
}
