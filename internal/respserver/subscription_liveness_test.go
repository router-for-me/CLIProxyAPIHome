package respserver

import (
	"context"
	"fmt"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
)

// gatedLivenessHandler lets a test decide the outcome of each liveness refresh in order.
type gatedLivenessHandler struct {
	blockingClusterHandler
	entered chan struct{}
	results chan error
}

func (h *gatedLivenessHandler) RefreshCPALiveness(ctx context.Context, _ cluster.ConnectionLifetime) error {
	select {
	case h.entered <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case errLiveness := <-h.results:
		return errLiveness
	case <-ctx.Done():
		return ctx.Err()
	}
}

func waitSubscriptionTestOutput(t *testing.T, connection *subscriptionTestConn, want string) {
	t.Helper()
	deadline := time.Now().Add(respPipeDeadline)
	for !strings.Contains(connection.Output(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %q in %q", want, connection.Output())
		}
		goruntime.Gosched()
	}
}

func TestSubscriptionHeartbeatSkipsPongOnLivenessTimeout(t *testing.T) {
	connection := newSubscriptionTestConn()
	lifetime := cluster.ConnectionLifetime{Fingerprint: "fp-liveness-timeout", ConnectedAt: time.Unix(40, 0), Subscription: true}
	registry := NewFingerprintRegistry()
	tracked, errAccept := registry.Accept(context.Background(), connection, lifetime)
	if errAccept != nil {
		t.Fatal(errAccept)
	}
	defer func() { _ = tracked.Close() }()
	handler := &gatedLivenessHandler{entered: make(chan struct{}), results: make(chan error, 1)}
	server := New("", nil)
	server.fingerprints = registry
	server.SetClusterHandler(handler)
	cancel := server.startSubscriptionUpdates(tracked.Context(), tracked, newSafeWriter(connection), newSubscriptionLiveness(0))
	defer cancel()

	waitSubscriptionTestSignal(t, handler.entered, "first heartbeat")
	handler.results <- &pgconn.PgError{Code: "55P03"}
	// The second refresh starts only after the first heartbeat has been fully handled.
	waitSubscriptionTestSignal(t, handler.entered, "second heartbeat")
	if output := connection.Output(); strings.Contains(output, "pong") {
		t.Fatalf("timed-out heartbeat wrote pong: %q", output)
	}
	if errFenced := tracked.Context().Err(); errFenced != nil {
		t.Fatalf("timed-out heartbeat fenced the subscription: %v", errFenced)
	}
	handler.results <- nil
	waitSubscriptionTestOutput(t, connection, "pong")
}

func TestSubscriptionPingSkipsReplyOnLivenessTimeout(t *testing.T) {
	connection := newSubscriptionTestConnCommands([]string{"SUBSCRIBE", "config"}, []string{"PING", "client-ping"}, []string{"PING", "home-rtt/probe"})
	fingerprint := peerCertificateFingerprint(connection)
	handler := &blockingClusterHandler{
		lifetime:    cluster.ConnectionLifetime{Fingerprint: fingerprint, ConnectedAt: time.Unix(50, 0), Subscription: true},
		livenessErr: fmt.Errorf("refresh liveness: %w", context.DeadlineExceeded),
	}
	server := New("", newSubscriptionTestRuntime(t, &blockingConfigAdapter{payload: []byte("host: \"\"\n"), loadStarted: make(chan struct{})}))
	server.SetClusterHandler(handler)
	serverDone := make(chan struct{})
	go func() {
		server.HandleConn(context.Background(), connection)
		close(serverDone)
	}()
	// The telemetry echo bypasses liveness, so its reply proves the connection outlived the skipped PING.
	waitSubscriptionTestOutput(t, connection, "home-rtt/probe")
	if output := connection.Output(); strings.Contains(output, "client-ping") {
		t.Fatalf("timed-out PING wrote a subscription pong: %q", output)
	}
	if errClose := connection.Close(); errClose != nil {
		t.Fatal(errClose)
	}
	waitSubscriptionTestSignal(t, serverDone, "RESP server shutdown")
}
