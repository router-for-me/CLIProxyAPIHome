package respserver

import (
	"context"
	"errors"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
)

func TestLatencyPingDoesNotRefreshSharedLiveness(t *testing.T) {
	conn := newSubscriptionTestConnCommands([]string{"SUBSCRIBE", "config"}, []string{"PING", "home-rtt/unique"})
	handler := &blockingClusterHandler{
		lifetime:    cluster.ConnectionLifetime{Fingerprint: peerCertificateFingerprint(conn), ConnectedAt: time.Unix(30, 0), Subscription: true},
		livenessErr: errors.New("telemetry must not run the shared liveness writer"),
	}
	server := New("", newSubscriptionTestRuntime(t, &blockingConfigAdapter{payload: []byte("host: \"\"\n"), loadStarted: make(chan struct{})}))
	server.SetClusterHandler(handler)
	done := make(chan struct{})
	go func() { defer close(done); server.HandleConn(context.Background(), conn) }()
	t.Cleanup(func() {
		if errClose := conn.Close(); errClose != nil {
			t.Error(errClose)
		}
		waitSubscriptionTestSignal(t, done, "telemetry echo shutdown")
	})
	deadline := time.Now().Add(respPipeDeadline)
	for time.Now().Before(deadline) {
		if strings.Contains(conn.Output(), "home-rtt/unique") {
			return
		}
		select {
		case <-done:
			t.Fatalf("telemetry invoked liveness writer before echo: %q", conn.Output())
		default:
			runtime.Gosched()
		}
	}
	t.Fatalf("telemetry echo missing: %q", conn.Output())
}

func TestLatencyPingEchoAndProbePreserveMembership(t *testing.T) {
	for _, subscription := range []bool{false, true} {
		t.Run(map[bool]string{false: "foreign-home-probe", true: "current-subscription"}[subscription], func(t *testing.T) {
			repo := newSubscriptionTestRepository(t)
			origin := cluster.NewCoordinator(repo, cluster.NodeIdentity{IP: "127.0.0.1", Port: 8327}, cluster.CoordinatorOptions{})
			target := cluster.NewCoordinator(repo, cluster.NodeIdentity{IP: "127.0.0.2", Port: 8327}, cluster.CoordinatorOptions{})
			for _, coordinator := range []*cluster.Coordinator{origin, target} {
				if errInitialize := coordinator.Initialize(context.Background()); errInitialize != nil {
					t.Fatal(errInitialize)
				}
				t.Cleanup(func() {
					if errRetire := coordinator.RetireHomeIncarnation(context.Background()); errRetire != nil {
						t.Error(errRetire)
					}
				})
			}
			lifecycle, errLifecycle := repo.LifecycleConfig(context.Background())
			if errLifecycle != nil {
				t.Fatal(errLifecycle)
			}
			commands := [][]string{{"PING", "home-rtt/unique"}, {"PING"}}
			if subscription {
				commands = append([][]string{{"SUBSCRIBE", "config", strconv.FormatInt(lifecycle.LifecycleConfigRevision, 10)}}, commands...)
			}
			conn := newSubscriptionTestConnCommands(commands...)
			fingerprint := peerCertificateFingerprint(conn)
			originHome, _ := origin.HomeIncarnation()
			targetHome, _ := target.HomeIncarnation()
			var original cluster.CPANodeMembershipRecord
			if !subscription {
				var errSubscribe error
				original, errSubscribe = repo.SubscribeMembership(context.Background(), cluster.SubscribeMembershipRequest{Fingerprint: fingerprint, NodeID: "latency-test", Home: originHome, ProtocolVersion: 1, LifecycleConfigRevision: lifecycle.LifecycleConfigRevision})
				if errSubscribe != nil {
					t.Fatal(errSubscribe)
				}
			}
			server := New("", newSubscriptionTestRuntime(t, &blockingConfigAdapter{payload: []byte("host: \"\"\n"), loadStarted: make(chan struct{})}))
			server.SetClusterHandler(cluster.NewRESPHandler(target, nil, repo))
			done := make(chan struct{})
			go func() { defer close(done); server.HandleConn(context.Background(), conn) }()
			t.Cleanup(func() {
				if errClose := conn.Close(); errClose != nil {
					t.Error(errClose)
				}
				waitSubscriptionTestSignal(t, done, "latency connection shutdown")
			})
			want := "+PONG\r\n"
			if subscription {
				want = "*2\r\n$4\r\npong\r\n$0\r\n\r\n"
			}
			deadline := time.Now().Add(respPipeDeadline)
			for time.Now().Before(deadline) {
				output := conn.Output()
				if strings.Contains(output, "home-rtt/unique") && strings.Contains(output, want) {
					ownerHome := originHome
					if subscription {
						ownerHome = targetHome
					}
					owner, errClassify := repo.ClassifyConnection(context.Background(), fingerprint, ownerHome)
					if errClassify != nil || !owner.Controlled || (!subscription && !owner.ConnectedAt.Equal(original.ConnectedAt)) {
						t.Fatalf("PING changed membership: %+v, %v", owner, errClassify)
					}
					if !subscription && strings.Contains(output, "subscribe") {
						t.Fatal("probe registered a subscription")
					}
					return
				}
				runtime.Gosched()
			}
			t.Fatalf("numbered PONG or original empty heartbeat missing: %q", conn.Output())
		})
	}
}
