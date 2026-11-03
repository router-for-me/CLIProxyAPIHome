package respserver

import (
	"context"
	"time"

	log "github.com/sirupsen/logrus"
)

// clusterSubscriptionBroadcast shares database work, not per-connection liveness.
// Its subscribers are protected by Server.clusterBroadcastMu.
type clusterSubscriptionBroadcast struct {
	cancel      context.CancelFunc
	subscribers map[chan []byte]struct{}
}

func (s *Server) subscribeClusterUpdates() (<-chan []byte, func()) {
	s.clusterBroadcastMu.Lock()
	defer s.clusterBroadcastMu.Unlock()

	broadcast := s.clusterBroadcast
	if broadcast == nil {
		// No individual subscriber owns the shared refresh context.
		ctx, cancel := context.WithCancel(context.Background())
		broadcast = &clusterSubscriptionBroadcast{
			cancel:      cancel,
			subscribers: make(map[chan []byte]struct{}),
		}
		s.clusterBroadcast = broadcast
		go func() {
			ticker := time.NewTicker(clusterSubscriptionUpdateInterval)
			defer ticker.Stop()
			s.runClusterSubscriptionUpdates(ctx, broadcast, ticker.C)
		}()
	}
	updates := make(chan []byte, 1)
	broadcast.subscribers[updates] = struct{}{}
	return updates, func() {
		s.clusterBroadcastMu.Lock()
		defer s.clusterBroadcastMu.Unlock()
		delete(broadcast.subscribers, updates)
		if len(broadcast.subscribers) == 0 && s.clusterBroadcast == broadcast {
			s.clusterBroadcast = nil
			broadcast.cancel()
		}
	}
}

func (s *Server) runClusterSubscriptionUpdates(ctx context.Context, broadcast *clusterSubscriptionBroadcast, ticks <-chan time.Time) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			if ctx.Err() != nil {
				return
			}
			s.broadcastClusterSubscriptionUpdate(ctx, broadcast)
		}
	}
}

func (s *Server) broadcastClusterSubscriptionUpdate(ctx context.Context, broadcast *clusterSubscriptionBroadcast) {
	s.syncClusterClientCount(ctx)
	payload, errPayload := s.cluster.Handle(ctx, []string{clusterCommand, "NODES"}, "")
	if ctx.Err() != nil {
		return
	}
	if errPayload != nil {
		log.Warnf("failed to build cluster update for subscribers: %v", errPayload)
		payload = nil
	}

	s.clusterBroadcastMu.Lock()
	defer s.clusterBroadcastMu.Unlock()
	for updates := range broadcast.subscribers {
		// Keep only the latest snapshot so a slow writer cannot stall refreshes.
		select {
		case <-updates:
		default:
		}
		updates <- payload
	}
}
