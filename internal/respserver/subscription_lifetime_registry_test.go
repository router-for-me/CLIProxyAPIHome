package respserver

import (
	"context"
	"errors"
	"net"
	"testing"
	"testing/synctest"
	"time"

	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
)

func TestLifetimeLivenessRegistrationDoesNotResetDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		registry := NewFingerprintRegistry()
		lifetime := cluster.ConnectionLifetime{Fingerprint: "registration", ConnectedAt: time.Unix(90, 0), HeartbeatTimeout: 3 * time.Second}
		liveness, errWatch := registry.watchSubscriptionLiveness(lifetime)
		if errWatch != nil {
			t.Fatal(errWatch)
		}
		deadline := liveness.currentDeadline()
		time.Sleep(2 * time.Second)
		reused, errReuse := registry.watchSubscriptionLiveness(lifetime)
		if errReuse != nil || reused != liveness || reused.currentDeadline() != deadline {
			t.Fatalf("registration reset lifetime liveness: reused=%t error=%v", reused == liveness, errReuse)
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if _, errExpired := registry.watchSubscriptionLiveness(lifetime); !errors.Is(errExpired, ErrFingerprintFenced) {
			t.Fatalf("registration revived expired lifetime: %v", errExpired)
		}
		if errLate := liveness.persisted(); !errors.Is(errLate, errSubscriptionHeartbeatExpired) {
			t.Fatalf("late persistence revived expired lifetime: %v", errLate)
		}
	})
}

type initializingLivenessHandler struct {
	blockingClusterHandler
	updateCount func(context.Context) error
}

func (h *initializingLivenessHandler) ClassifyConnection(_ context.Context, fingerprint string) (cluster.ConnectionLifetime, error) {
	// The first subscription starts as an unowned bootstrap connection.
	return cluster.ConnectionLifetime{Fingerprint: fingerprint}, nil
}

func (h *initializingLivenessHandler) UpdateClientCount(ctx context.Context, _ int) error {
	return h.updateCount(ctx)
}

type failedSubscriptionAckConn struct {
	*subscriptionTestConn
}

func (c *failedSubscriptionAckConn) Write([]byte) (int, error) {
	return 0, errors.New("subscription ACK delivery failed")
}

func TestLifetimeExpiryDuringSubscriptionInitialization(t *testing.T) {
	for _, mode := range []string{"client count blocked", "initial config blocked", "initial config failed", "ACK failed"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				connection := newSubscriptionTestConn("SUBSCRIBE", "config")
				var subscriptionConn net.Conn = connection
				if mode == "ACK failed" {
					subscriptionConn = &failedSubscriptionAckConn{subscriptionTestConn: connection}
				}
				lifetime := cluster.ConnectionLifetime{
					Fingerprint: peerCertificateFingerprint(connection), ConnectedAt: time.Unix(100, 0),
					Subscription: true, HeartbeatTimeout: 3 * time.Second,
				}
				releaseCount := make(chan struct{})
				adapter := &blockingConfigAdapter{payload: []byte("host: \"\"\n"), loadStarted: make(chan struct{})}
				if mode == "initial config blocked" {
					adapter.releaseLoad = make(chan struct{})
				}
				if mode == "initial config failed" {
					adapter.err = errors.New("config unavailable")
				}
				server := New("", newSubscriptionTestRuntime(t, adapter))
				server.SetClusterHandler(&initializingLivenessHandler{
					blockingClusterHandler: blockingClusterHandler{lifetime: lifetime},
					updateCount: func(context.Context) error {
						if mode == "client count blocked" {
							<-releaseCount
						}
						return nil
					},
				})
				commandLifetime := lifetime
				commandLifetime.Subscription, commandLifetime.Controlled = false, true
				command, errAccept := server.fingerprints.Accept(context.Background(), newSubscriptionTestConn(), commandLifetime)
				if errAccept != nil {
					t.Fatal(errAccept)
				}
				defer func() {
					if errClose := command.Close(); errClose != nil {
						t.Error(errClose)
					}
					if errClose := connection.Close(); errClose != nil {
						t.Error(errClose)
					}
				}()
				done := make(chan struct{})
				go func() {
					server.HandleConn(context.Background(), subscriptionConn)
					close(done)
				}()
				synctest.Wait()
				time.Sleep(2 * time.Second)
				synctest.Wait()
				if command.Context().Err() != nil {
					t.Fatal("initialization failure fenced lifetime before its deadline")
				}
				time.Sleep(time.Second)
				synctest.Wait()
				if command.Context().Err() == nil {
					t.Fatal("initialization prevented lifetime expiry")
				}
				close(releaseCount)
				synctest.Wait()
				select {
				case <-done:
				default:
					t.Fatal("subscription initialization continued after lifetime expiry")
				}
			})
		})
	}
}
