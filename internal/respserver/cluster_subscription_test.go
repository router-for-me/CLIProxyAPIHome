package respserver

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	"github.com/router-for-me/CLIProxyAPIHome/internal/node"
)

type clusterBroadcastTestHandler struct {
	clusterHandler
	countUpdates atomic.Int64
	nodeQueries  atomic.Int64
	clientCount  atomic.Int64
	updateErr    error
	queryErr     error
	queryStarted chan struct{}
	rebalances   chan cluster.ConnectionLifetime
}

func (h *clusterBroadcastTestHandler) UpdateClientCount(_ context.Context, count int) error {
	h.countUpdates.Add(1)
	h.clientCount.Store(int64(count))
	return h.updateErr
}

func (h *clusterBroadcastTestHandler) RefreshCPALiveness(context.Context, cluster.ConnectionLifetime) error {
	return nil
}

func (h *clusterBroadcastTestHandler) HandleRebalance(context.Context, cluster.ConnectionLifetime, []string) ([]byte, error) {
	return nil, nil
}

func (h *clusterBroadcastTestHandler) RebalanceInstruction(_ context.Context, lifetime cluster.ConnectionLifetime) ([]byte, error) {
	if h.rebalances != nil {
		h.rebalances <- lifetime
	}
	return nil, nil
}

func (h *clusterBroadcastTestHandler) Handle(ctx context.Context, _ []string, _ string) ([]byte, error) {
	query := h.nodeQueries.Add(1)
	if h.queryStarted != nil {
		close(h.queryStarted)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return []byte(fmt.Sprintf(`{"snapshot":%d}`, query)), h.queryErr
}

// Inject ticks to exercise the production loop without wall-clock ordering.
func newClusterBroadcastTestServer(t *testing.T, handler *clusterBroadcastTestHandler) (*Server, chan<- time.Time, context.Context, <-chan struct{}) {
	t.Helper()
	server := New("", nil)
	server.SetClusterHandler(handler)
	ctx, cancel := context.WithCancel(context.Background())
	broadcast := &clusterSubscriptionBroadcast{
		cancel:      cancel,
		subscribers: make(map[chan []byte]struct{}),
	}
	server.clusterBroadcast = broadcast
	ticks := make(chan time.Time)
	done := make(chan struct{})
	go func() {
		defer close(done)
		server.runClusterSubscriptionUpdates(ctx, broadcast, ticks)
	}()
	t.Cleanup(func() {
		cancel()
		waitSubscriptionTestSignal(t, done, "cluster broadcast shutdown")
	})
	return server, ticks, ctx, done
}

func tickClusterBroadcast(t *testing.T, ticks chan<- time.Time) {
	t.Helper()
	select {
	case ticks <- time.Now():
	case <-time.After(respPipeDeadline):
		t.Fatal("cluster broadcast did not accept tick")
	}
}

func readClusterBroadcast(t *testing.T, updates <-chan []byte) []byte {
	t.Helper()
	select {
	case payload := <-updates:
		return payload
	case <-time.After(respPipeDeadline):
		t.Fatal("cluster broadcast did not publish update")
		return nil
	}
}

func TestClusterBroadcastSharesClientCountAndNodeRefresh(t *testing.T) {
	handler := &clusterBroadcastTestHandler{}
	server, ticks, _, _ := newClusterBroadcastTestServer(t, handler)
	updates := make([]<-chan []byte, 32)
	for i := range updates {
		var unsubscribe func()
		updates[i], unsubscribe = server.subscribeClusterUpdates()
		t.Cleanup(unsubscribe)
	}

	for cycle := 1; cycle <= 2; cycle++ {
		tickClusterBroadcast(t, ticks)
		want := fmt.Sprintf(`{"snapshot":%d}`, cycle)
		for i, subscriber := range updates {
			if payload := string(readClusterBroadcast(t, subscriber)); payload != want {
				t.Fatalf("subscriber %d payload = %q, want %q", i, payload, want)
			}
		}
		if handler.countUpdates.Load() != int64(cycle) || handler.nodeQueries.Load() != int64(cycle) {
			t.Fatalf("cycle %d: count updates = %d, node queries = %d", cycle, handler.countUpdates.Load(), handler.nodeQueries.Load())
		}
		if handler.clientCount.Load() != int64(node.GlobalRegistry().TotalCount()) {
			t.Fatal("broadcast did not sync the Home client count")
		}
	}
}

func TestClusterBroadcastSlowSubscriberKeepsLatestSnapshot(t *testing.T) {
	handler := &clusterBroadcastTestHandler{}
	server, ticks, _, _ := newClusterBroadcastTestServer(t, handler)
	slow, unsubscribeSlow := server.subscribeClusterUpdates()
	defer unsubscribeSlow()
	fast, unsubscribeFast := server.subscribeClusterUpdates()
	defer unsubscribeFast()

	for cycle := 1; cycle <= 3; cycle++ {
		tickClusterBroadcast(t, ticks)
		if payload := string(readClusterBroadcast(t, fast)); payload != fmt.Sprintf(`{"snapshot":%d}`, cycle) {
			t.Fatalf("fast subscriber payload = %q", payload)
		}
	}
	// The fast subscriber may receive before fan-out reaches the slow one.
	server.clusterBroadcastMu.Lock()
	server.clusterBroadcastMu.Unlock()
	if payload := string(readClusterBroadcast(t, slow)); payload != `{"snapshot":3}` {
		t.Fatalf("slow subscriber payload = %q", payload)
	}
}

func TestClusterBroadcastFailuresRetryAndStillNotifySubscribers(t *testing.T) {
	for _, failure := range []string{"count", "nodes"} {
		t.Run(failure, func(t *testing.T) {
			handler := &clusterBroadcastTestHandler{}
			if failure == "count" {
				handler.updateErr = errors.New("count update failed")
			} else {
				handler.queryErr = errors.New("node query failed")
			}
			server, ticks, _, _ := newClusterBroadcastTestServer(t, handler)
			updates, unsubscribe := server.subscribeClusterUpdates()
			defer unsubscribe()
			tickClusterBroadcast(t, ticks)
			payload := readClusterBroadcast(t, updates)
			if failure == "nodes" && len(payload) != 0 {
				t.Fatalf("failed query published payload %q", payload)
			}
			if failure == "count" && len(payload) == 0 {
				t.Fatal("count update failure suppressed the node list")
			}

			handler.updateErr = nil
			handler.queryErr = nil
			tickClusterBroadcast(t, ticks)
			if payload := string(readClusterBroadcast(t, updates)); payload != `{"snapshot":2}` {
				t.Fatalf("retry payload = %q", payload)
			}
			if handler.countUpdates.Load() != 2 || handler.nodeQueries.Load() != 2 {
				t.Fatal("failed database work was not retried on the next tick")
			}
		})
	}
}

func TestClusterBroadcastLastSubscriberCancelsPendingRefresh(t *testing.T) {
	handler := &clusterBroadcastTestHandler{queryStarted: make(chan struct{})}
	server, ticks, ctx, done := newClusterBroadcastTestServer(t, handler)
	_, unsubscribeFirst := server.subscribeClusterUpdates()
	defer unsubscribeFirst()
	last, unsubscribeLast := server.subscribeClusterUpdates()
	defer unsubscribeLast()
	unsubscribeFirst()
	if ctx.Err() != nil {
		t.Fatal("first subscriber stopped the shared refresh")
	}

	tickClusterBroadcast(t, ticks)
	waitSubscriptionTestSignal(t, handler.queryStarted, "pending cluster query")
	unsubscribeLast()
	waitSubscriptionTestSignal(t, done, "canceled cluster query")
	if ctx.Err() == nil || server.clusterBroadcast != nil {
		t.Fatal("last subscriber did not stop the shared refresh")
	}
	select {
	case payload := <-last:
		t.Fatalf("canceled query published payload %q", payload)
	default:
	}
}

func TestClusterBroadcastDeliversToConnectionsAndPreservesRebalance(t *testing.T) {
	handler := &clusterBroadcastTestHandler{rebalances: make(chan cluster.ConnectionLifetime, 2)}
	server, ticks, _, done := newClusterBroadcastTestServer(t, handler)
	connections := make([]*subscriptionTestConn, 2)
	lifetimes := make(map[cluster.ConnectionLifetime]bool)
	var cancels []context.CancelFunc
	for i := range connections {
		connections[i] = newSubscriptionTestConn()
		lifetime := cluster.ConnectionLifetime{
			Fingerprint: fmt.Sprintf("fp-%d", i), ConnectedAt: time.Unix(int64(i+1), 0), Subscription: true,
		}
		lifetimes[lifetime] = true
		tracked, errAccept := server.fingerprints.Accept(context.Background(), connections[i], lifetime)
		if errAccept != nil {
			t.Fatal(errAccept)
		}
		t.Cleanup(func() {
			if errClose := tracked.Close(); errClose != nil {
				t.Error(errClose)
			}
		})
		cancel := server.startSubscriptionUpdates(tracked.Context(), tracked, newSafeWriter(connections[i]))
		cancels = append(cancels, cancel)
		t.Cleanup(cancel)
	}

	for cycle := 1; cycle <= 2; cycle++ {
		if cycle == 2 {
			handler.queryErr = errors.New("node query failed")
		}
		tickClusterBroadcast(t, ticks)
		seen := make(map[cluster.ConnectionLifetime]bool)
		for range connections {
			select {
			case lifetime := <-handler.rebalances:
				if !lifetimes[lifetime] || seen[lifetime] {
					t.Fatalf("unexpected rebalance lifetime: %#v", lifetime)
				}
				seen[lifetime] = true
			case <-time.After(respPipeDeadline):
				t.Fatal("subscriber did not check its rebalance instruction")
			}
		}
	}
	for i, connection := range connections {
		output := connection.Output()
		if !strings.Contains(output, clusterSubscriptionChannel) || !strings.Contains(output, `{"snapshot":1}`) || strings.Contains(output, `{"snapshot":2}`) {
			t.Fatalf("subscriber %d output = %q", i, output)
		}
	}
	if handler.countUpdates.Load() != 2 || handler.nodeQueries.Load() != 2 {
		t.Fatal("connection delivery repeated the database work")
	}
	for _, cancel := range cancels {
		cancel()
	}
	waitSubscriptionTestSignal(t, done, "connection broadcast shutdown")
}

func TestClusterBroadcastRestartsAfterLastSubscriber(t *testing.T) {
	server := New("", nil)
	server.SetClusterHandler(&clusterBroadcastTestHandler{})
	_, unsubscribeFirst := server.subscribeClusterUpdates()
	first := server.clusterBroadcast
	unsubscribeFirst()
	_, unsubscribeSecond := server.subscribeClusterUpdates()
	defer unsubscribeSecond()
	second := server.clusterBroadcast
	unsubscribeFirst()
	if first == second || second == nil || server.clusterBroadcast != second {
		t.Fatal("stale unsubscribe stopped the new broadcast")
	}
}
