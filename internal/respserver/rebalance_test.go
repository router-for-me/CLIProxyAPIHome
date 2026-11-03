package respserver

import (
	"bytes"
	"context"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
)

func TestRebalanceCommandRequiresControlledMTLSMembership(t *testing.T) {
	for _, registered := range []bool{false, true} {
		t.Run(map[bool]string{false: "bootstrap", true: "controlled"}[registered], func(t *testing.T) {
			repo := newSubscriptionTestRepository(t)
			coordinator := cluster.NewCoordinator(repo, cluster.NodeIdentity{IP: "127.0.0.1", Port: 8327}, cluster.CoordinatorOptions{})
			if errInitialize := coordinator.Initialize(context.Background()); errInitialize != nil {
				t.Fatal(errInitialize)
			}
			t.Cleanup(func() {
				if errRetire := coordinator.RetireHomeIncarnation(context.Background()); errRetire != nil {
					t.Error(errRetire)
				}
			})
			conn := newSubscriptionTestConn("CLUSTER", "BALANCE", "ENABLE")
			if registered {
				lifecycle, errLifecycle := repo.LifecycleConfig(context.Background())
				if errLifecycle != nil {
					t.Fatal(errLifecycle)
				}
				home, _ := coordinator.HomeIncarnation()
				if _, errSubscribe := repo.SubscribeMembership(context.Background(), cluster.SubscribeMembershipRequest{Fingerprint: peerCertificateFingerprint(conn), NodeID: "subscription-test-node", Home: home, ProtocolVersion: 1, LifecycleConfigRevision: lifecycle.LifecycleConfigRevision}); errSubscribe != nil {
					t.Fatal(errSubscribe)
				}
			}
			server := New("", nil)
			server.SetClusterHandler(cluster.NewRESPHandler(coordinator, nil, repo))
			done := make(chan struct{})
			go func() { defer close(done); server.HandleConn(context.Background(), conn) }()
			t.Cleanup(func() {
				if errClose := conn.Close(); errClose != nil {
					t.Error(errClose)
				}
				waitSubscriptionTestSignal(t, done, "rebalance connection shutdown")
			})
			deadline := time.Now().Add(respPipeDeadline)
			for time.Now().Before(deadline) {
				output := conn.Output()
				if registered && strings.Contains(output, `"ok":true`) && strings.Contains(output, `"cpa_latency_swap_v1"`) {
					return
				}
				if !registered && strings.Contains(output, "credential concurrency node is unavailable") {
					return
				}
				runtime.Gosched()
			}
			t.Fatalf("unexpected rebalance response: %q", conn.Output())
		})
	}
}

type rebalanceForwardTestHandler struct {
	clusterHandler
	seen cluster.ConnectionLifetime
}

func (h *rebalanceForwardTestHandler) HandleRebalance(context.Context, cluster.ConnectionLifetime, []string) ([]byte, error) {
	return nil, nil
}
func (h *rebalanceForwardTestHandler) RebalanceInstruction(_ context.Context, lifetime cluster.ConnectionLifetime) ([]byte, error) {
	h.seen = lifetime
	return []byte(`{"id":"plan","term":1,"target":{"ip":"home-b","port":8327}}`), nil
}

func TestRebalanceForwardingUsesExactSubscriptionLifetime(t *testing.T) {
	handler := &rebalanceForwardTestHandler{}
	server := &Server{cluster: handler}
	lifetime := cluster.ConnectionLifetime{Fingerprint: "fp-a", ConnectedAt: time.Unix(123, 0), Home: cluster.HomeIncarnationID{IP: "home-a", Port: 8327, StartedAt: time.Unix(100, 0)}, Subscription: true}
	var output bytes.Buffer
	if errWrite := server.writeRebalanceInstruction(context.Background(), lifetime, newSafeWriter(&output)); errWrite != nil {
		t.Fatal(errWrite)
	}
	if handler.seen != lifetime || !strings.Contains(output.String(), "rebalance") || !strings.Contains(output.String(), `"id":"plan"`) {
		t.Fatalf("forwarded frame = %q", output.String())
	}
	output.Reset()
	if errWrite := server.writeRebalanceInstruction(context.Background(), cluster.ConnectionLifetime{}, newSafeWriter(&output)); errWrite != nil || output.Len() != 0 {
		t.Fatal("bootstrap connection received migration")
	}
}
