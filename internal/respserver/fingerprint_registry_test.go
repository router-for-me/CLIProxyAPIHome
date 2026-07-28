package respserver

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
)

func TestFingerprintFenceWaitsForConnectionAndHandlerQuiescence(t *testing.T) {
	registry := NewFingerprintRegistry()
	serverConn, clientConn := net.Pipe()
	defer func() {
		if errClose := clientConn.Close(); errClose != nil {
			t.Errorf("close client connection: %v", errClose)
		}
	}()
	lifetime := cluster.ConnectionLifetime{Fingerprint: "fp-a", ConnectedAt: time.Unix(10, 0), Controlled: true}
	tracked, errAccept := registry.Accept(context.Background(), serverConn, lifetime)
	if errAccept != nil {
		t.Fatal(errAccept)
	}
	finish, errBegin := tracked.BeginHandler()
	if errBegin != nil {
		t.Fatal(errBegin)
	}
	done := make(chan error, 1)
	go func() { done <- registry.Fence(context.Background(), lifetime, 7) }()
	select {
	case <-done:
		t.Fatal("fence returned before handler drained")
	case <-time.After(20 * time.Millisecond):
	}
	finish()
	if errClose := tracked.Close(); errClose != nil {
		t.Fatal(errClose)
	}
	if errFence := <-done; errFence != nil {
		t.Fatal(errFence)
	}
	if revision := registry.LatestFenceRevision(lifetime); revision != 7 {
		t.Fatalf("revision = %d", revision)
	}
}

func TestSubscriptionAttachFinishesBootstrapHandlerBeforeMove(t *testing.T) {
	registry := NewFingerprintRegistry()
	serverConn, clientConn := net.Pipe()
	defer func() { _ = clientConn.Close() }()
	tracked, errAccept := registry.Accept(context.Background(), serverConn, cluster.ConnectionLifetime{Fingerprint: "fp-a"})
	if errAccept != nil {
		t.Fatal(errAccept)
	}
	finish, errBegin := tracked.BeginHandler()
	if errBegin != nil {
		t.Fatal(errBegin)
	}
	attachDone := make(chan error, 1)
	go func() {
		attachDone <- tracked.AttachSubscriptionLifetime(cluster.ConnectionLifetime{Fingerprint: "fp-a", ConnectedAt: time.Unix(10, 0), Subscription: true})
	}()
	select {
	case errAttach := <-attachDone:
		t.Fatalf("attach completed before bootstrap handler finished: %v", errAttach)
	case <-time.After(20 * time.Millisecond):
	}
	finish()
	if errAttach := <-attachDone; errAttach != nil {
		t.Fatal(errAttach)
	}
}

func TestSubscriptionConnectionAttachesLifetimeWithoutBecomingControlled(t *testing.T) {
	registry := NewFingerprintRegistry()
	serverConn, clientConn := net.Pipe()
	defer func() { _ = clientConn.Close() }()
	tracked, errAccept := registry.Accept(context.Background(), serverConn, cluster.ConnectionLifetime{Fingerprint: "fp-a"})
	if errAccept != nil {
		t.Fatal(errAccept)
	}
	lifetime := cluster.ConnectionLifetime{Fingerprint: "fp-a", ConnectedAt: time.Unix(10, 0), Subscription: true}
	if errAttach := tracked.AttachSubscriptionLifetime(lifetime); errAttach != nil {
		t.Fatal(errAttach)
	}
	attached := tracked.Lifetime()
	if attached.Controlled || !attached.Subscription || !attached.ConnectedAt.Equal(lifetime.ConnectedAt) {
		t.Fatalf("attached lifetime = %#v", attached)
	}
}

func TestSubscriptionStopsPongWhenSharedLivenessFails(t *testing.T) {
	repo := newSubscriptionTestRepository(t)
	coordinator := cluster.NewCoordinator(repo, cluster.NodeIdentity{IP: "127.0.0.1", Port: 8317}, cluster.CoordinatorOptions{})
	if errInitialize := coordinator.Initialize(context.Background()); errInitialize != nil {
		t.Fatal(errInitialize)
	}
	home, initialized := coordinator.HomeIncarnation()
	if !initialized {
		t.Fatal("Home incarnation is not initialized")
	}
	lifecycle, errLifecycle := repo.LifecycleConfig(context.Background())
	if errLifecycle != nil {
		t.Fatal(errLifecycle)
	}
	member, errSubscribe := repo.SubscribeMembership(context.Background(), cluster.SubscribeMembershipRequest{
		Fingerprint: "fp-liveness", NodeID: "cpa-liveness", Home: home, ProtocolVersion: 1, LifecycleConfigRevision: lifecycle.LifecycleConfigRevision,
	})
	if errSubscribe != nil {
		t.Fatal(errSubscribe)
	}
	if errFence := repo.FenceHomeIncarnation(context.Background(), home, "test"); errFence != nil {
		t.Fatal(errFence)
	}

	connection := newSubscriptionTestConn()
	registry := NewFingerprintRegistry()
	tracked, errAccept := registry.Accept(context.Background(), connection, cluster.ConnectionLifetime{Fingerprint: member.CertificateFingerprint, ConnectedAt: member.ConnectedAt, Home: home, Subscription: true})
	if errAccept != nil {
		t.Fatal(errAccept)
	}
	defer func() { _ = tracked.Close() }()
	server := New("", nil)
	server.fingerprints = registry
	server.SetClusterHandler(cluster.NewRESPHandler(coordinator, nil, repo))
	cancel := server.startSubscriptionUpdates(tracked.Context(), tracked, newSafeWriter(connection))
	defer cancel()
	time.Sleep(subscriptionHeartbeatInterval + 50*time.Millisecond)
	if output := connection.Output(); strings.Contains(output, "pong") {
		t.Fatalf("liveness failure wrote pong: %q", output)
	}
}

func TestActiveLifetimeFenceClosesBootstrapOriginatedSubscription(t *testing.T) {
	registry := NewFingerprintRegistry()
	serverConn, clientConn := net.Pipe()
	defer func() { _ = clientConn.Close() }()
	tracked, errAccept := registry.Accept(context.Background(), serverConn, cluster.ConnectionLifetime{Fingerprint: "fp-a"})
	if errAccept != nil {
		t.Fatal(errAccept)
	}
	lifetime := cluster.ConnectionLifetime{Fingerprint: "fp-a", ConnectedAt: time.Unix(10, 0), Subscription: true}
	if errAttach := tracked.AttachSubscriptionLifetime(lifetime); errAttach != nil {
		t.Fatal(errAttach)
	}
	if errFence := registry.Fence(context.Background(), lifetime, 3); errFence != nil {
		t.Fatal(errFence)
	}
	select {
	case <-tracked.Context().Done():
	case <-time.After(time.Second):
		t.Fatal("fence did not cancel bootstrap-originated subscription")
	}
}

func TestFenceWaitsForBlockingClusterHandler(t *testing.T) {
	connection := newSubscriptionTestConn("CLUSTER", "NODES")
	fingerprint := peerCertificateFingerprint(connection)
	lifetime := cluster.ConnectionLifetime{Fingerprint: fingerprint, ConnectedAt: time.Unix(20, 0), Controlled: true}
	handler := &blockingClusterHandler{lifetime: lifetime, started: make(chan struct{}), release: make(chan struct{}), contextCanceled: make(chan struct{})}
	server := New("", nil)
	server.SetClusterHandler(handler)
	serverDone := make(chan struct{})
	go func() {
		server.HandleConn(context.Background(), connection)
		close(serverDone)
	}()
	waitSubscriptionTestSignal(t, handler.started, "blocking CLUSTER handler")

	fenceDone := make(chan error, 1)
	go func() { fenceDone <- server.FenceFingerprint(context.Background(), lifetime, 4, nil) }()
	select {
	case <-fenceDone:
		t.Fatal("fence returned before blocking CLUSTER handler finished")
	case <-time.After(20 * time.Millisecond):
	}
	waitSubscriptionTestSignal(t, handler.contextCanceled, "CLUSTER handler context cancellation")
	close(handler.release)
	if errFence := <-fenceDone; errFence != nil {
		t.Fatal(errFence)
	}
	waitSubscriptionTestSignal(t, serverDone, "RESP server shutdown")
}

func TestSubscriptionPingDoesNotReplyWhenSharedLivenessFails(t *testing.T) {
	connection := newSubscriptionTestConnCommands([]string{"SUBSCRIBE", "config"}, []string{"PING", "client-ping"})
	fingerprint := peerCertificateFingerprint(connection)
	handler := &blockingClusterHandler{
		lifetime:    cluster.ConnectionLifetime{Fingerprint: fingerprint, ConnectedAt: time.Unix(30, 0), Subscription: true},
		livenessErr: errors.New("shared liveness failed"),
	}
	server := New("", newSubscriptionTestRuntime(t, &blockingConfigAdapter{payload: []byte("host: \"\"\n"), loadStarted: make(chan struct{})}))
	server.SetClusterHandler(handler)
	serverDone := make(chan struct{})
	go func() {
		server.HandleConn(context.Background(), connection)
		close(serverDone)
	}()
	waitSubscriptionTestSignal(t, serverDone, "subscription PING liveness failure")
	output := connection.Output()
	if !strings.Contains(output, "subscribe") {
		t.Fatalf("subscription setup did not complete: %q", output)
	}
	if strings.Contains(output, "pong") || strings.Contains(output, "client-ping") {
		t.Fatalf("liveness failure wrote subscription pong: %q", output)
	}
}

func TestClosedLifetimeFenceDoesNotBlockNewLifetime(t *testing.T) {
	registry := NewFingerprintRegistry()
	oldLifetime := cluster.ConnectionLifetime{Fingerprint: "fp-a", ConnectedAt: time.Unix(10, 0), Controlled: true}
	oldServer, oldClient := net.Pipe()
	defer func() { _ = oldClient.Close() }()
	oldTracked, errAccept := registry.Accept(context.Background(), oldServer, oldLifetime)
	if errAccept != nil {
		t.Fatal(errAccept)
	}
	if errClose := oldTracked.Close(); errClose != nil {
		t.Fatal(errClose)
	}
	if errFence := registry.Fence(context.Background(), oldLifetime, 1); errFence != nil {
		t.Fatal(errFence)
	}

	newLifetime := cluster.ConnectionLifetime{Fingerprint: "fp-a", ConnectedAt: time.Unix(11, 0), Controlled: true}
	newServer, newClient := net.Pipe()
	defer func() { _ = newClient.Close() }()
	newTracked, errAccept := registry.Accept(context.Background(), newServer, newLifetime)
	if errAccept != nil {
		t.Fatalf("new lifetime accept: %v", errAccept)
	}
	if errClose := newTracked.Close(); errClose != nil {
		t.Fatal(errClose)
	}
}

type blockingClusterHandler struct {
	lifetime        cluster.ConnectionLifetime
	livenessErr     error
	started         chan struct{}
	release         chan struct{}
	contextCanceled chan struct{}
}

func (h *blockingClusterHandler) ClassifyConnection(context.Context, string) (cluster.ConnectionLifetime, error) {
	return h.lifetime, nil
}

func (h *blockingClusterHandler) SubscribeMembership(context.Context, string, string, int, int64, bool, string) (cluster.ConnectionLifetime, error) {
	return h.lifetime, nil
}

func (h *blockingClusterHandler) RefreshCPALiveness(context.Context, cluster.ConnectionLifetime) error {
	return h.livenessErr
}

func (h *blockingClusterHandler) UpdateClientCount(context.Context, int) error {
	return nil
}

func (h *blockingClusterHandler) RequestClientCertificate(context.Context, string, string, []byte) ([]byte, error) {
	return nil, errors.New("not implemented")
}

func (h *blockingClusterHandler) Handle(ctx context.Context, _ []string, _ string) ([]byte, error) {
	if h.started == nil {
		return []byte("{}"), nil
	}
	close(h.started)
	go func() {
		<-ctx.Done()
		close(h.contextCanceled)
	}()
	<-h.release
	return []byte("{}"), nil
}
