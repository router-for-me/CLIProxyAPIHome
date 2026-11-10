package respserver

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
)

type deadlineLivenessHandler struct {
	blockingClusterHandler
	refresh func(context.Context) error
}

func (h *deadlineLivenessHandler) RefreshCPALiveness(ctx context.Context, _ cluster.ConnectionLifetime) error {
	return h.refresh(ctx)
}

func TestSubscriptionLivenessExpiresDespiteOrdinaryTraffic(t *testing.T) {
	for _, mode := range []string{"periodic timeout", "PING timeout", "blocked database"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				commands := [][]string{{"SUBSCRIBE", "config"}}
				if mode == "PING timeout" {
					commands = append(commands, []string{"PING", "client-ping"}, []string{"PING", "home-rtt/probe"})
				}
				connection := newSubscriptionTestConnCommands(commands...)
				lifetime := cluster.ConnectionLifetime{
					Fingerprint: peerCertificateFingerprint(connection), ConnectedAt: time.Unix(60, 0),
					Subscription: true, HeartbeatTimeout: time.Minute,
				}
				var refreshes atomic.Int32
				handler := &deadlineLivenessHandler{
					blockingClusterHandler: blockingClusterHandler{lifetime: lifetime},
					refresh: func(ctx context.Context) error {
						refreshes.Add(1)
						if mode == "blocked database" {
							<-ctx.Done()
							return ctx.Err()
						}
						return &pgconn.PgError{Code: "55P03"}
					},
				}
				runtimeHome := newSubscriptionTestRuntime(t, &blockingConfigAdapter{
					payload: []byte("host: \"\"\n"), loadStarted: make(chan struct{}),
				})
				server := New("", runtimeHome)
				server.SetClusterHandler(handler)
				commandConnection := newSubscriptionTestConn()
				commandLifetime := lifetime
				commandLifetime.Subscription, commandLifetime.Controlled = false, true
				commandTracked, errAccept := server.fingerprints.Accept(context.Background(), commandConnection, commandLifetime)
				if errAccept != nil {
					t.Fatal(errAccept)
				}
				defer func() {
					if errClose := connection.Close(); errClose != nil {
						t.Error(errClose)
					}
					if errClose := commandTracked.Close(); errClose != nil {
						t.Error(errClose)
					}
					synctest.Wait()
				}()
				finish, errBegin := commandTracked.BeginHandler()
				if errBegin != nil {
					t.Fatal(errBegin)
				}
				taskCanceled := make(chan struct{})
				go func() {
					<-commandTracked.Context().Done()
					finish()
					close(taskCanceled)
				}()
				done := make(chan struct{})
				go func() {
					server.HandleConn(context.Background(), connection)
					close(done)
				}()
				stopTraffic := make(chan struct{})
				defer close(stopTraffic)
				go func() {
					ticker := time.NewTicker(10 * time.Second)
					defer ticker.Stop()
					for {
						select {
						case <-stopTraffic:
							return
						case <-connection.closed:
							return
						case <-ticker.C:
							runtimeHome.PublishConfigYAML([]byte("host: \"\"\ntest-marker: ordinary-config-update\n"))
						}
					}
				}()
				synctest.Wait()
				// All sleeps in this test advance synctest's virtual monotonic clock.
				time.Sleep(59 * time.Second)
				synctest.Wait()
				select {
				case <-connection.closed:
					t.Fatal("subscription fenced before its membership heartbeat timeout")
				default:
				}
				if refreshes.Load() == 0 {
					t.Fatal("heartbeat persistence was not attempted")
				}
				if output := connection.Output(); !strings.Contains(output, "ordinary-config-update") || !strings.Contains(output, clusterSubscriptionChannel) {
					t.Fatalf("ordinary messages did not continue during database failures: %q", output)
				}
				time.Sleep(time.Second)
				synctest.Wait()
				select {
				case <-done:
				default:
					t.Fatal("ordinary traffic or a blocked refresh extended the persistence deadline")
				}
				select {
				case <-taskCanceled:
				default:
					t.Fatal("expiry did not cancel an associated command/task")
				}
				if _, errAcceptFenced := server.fingerprints.Accept(context.Background(), newSubscriptionTestConn(), commandLifetime); !errors.Is(errAcceptFenced, ErrFingerprintFenced) {
					t.Fatalf("expired lifetime was not fenced: %v", errAcceptFenced)
				}
			})
		})
	}
}

func TestSubscriptionLivenessRecoversBeforePersistenceDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		connection := newSubscriptionTestConn()
		server := New("", nil)
		tracked, errAccept := server.fingerprints.Accept(context.Background(), connection, cluster.ConnectionLifetime{Fingerprint: "temporary", ConnectedAt: time.Unix(70, 0), HeartbeatTimeout: 10 * time.Second})
		if errAccept != nil {
			t.Fatal(errAccept)
		}
		defer func() {
			if errClose := tracked.Close(); errClose != nil {
				t.Error(errClose)
			}
		}()
		var refreshes atomic.Int32
		server.SetClusterHandler(&deadlineLivenessHandler{refresh: func(context.Context) error {
			if refreshes.Add(1) <= 3 {
				return &pgconn.PgError{Code: "55P03"}
			}
			return nil
		}})
		liveness, errWatch := server.fingerprints.watchSubscriptionLiveness(tracked.Lifetime())
		if errWatch != nil {
			t.Fatal(errWatch)
		}
		initial := liveness.currentDeadline()
		defer func() {
			if errFence := server.fingerprints.Fence(context.Background(), tracked.Lifetime(), 0); errFence != nil {
				t.Error(errFence)
			}
		}()
		cancelUpdates := server.startSubscriptionUpdates(tracked.Context(), tracked, newSafeWriter(connection), liveness)
		defer cancelUpdates()
		time.Sleep(3 * time.Second)
		synctest.Wait()
		if liveness.currentDeadline() != initial || connection.Output() != "" || tracked.Context().Err() != nil {
			t.Fatal("transient errors renewed liveness, emitted PONG, or prematurely fenced")
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if liveness.currentDeadline() != time.Now().Add(10*time.Second) || connection.Output() == "" {
			t.Fatal("successful persistence did not renew the deadline and emit PONG")
		}
		time.Sleep(10 * time.Second)
		synctest.Wait()
		if tracked.Context().Err() != nil {
			t.Fatal("recovered heartbeats did not preserve the subscription")
		}
	})
}

func TestSubscriptionLivenessRejectsLatePersistence(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		liveness := newSubscriptionLiveness(time.Second)
		deadline := liveness.currentDeadline()
		time.Sleep(time.Second)
		if errPersisted := liveness.persisted(); !errors.Is(errPersisted, errSubscriptionHeartbeatExpired) {
			t.Fatalf("late persistence error = %v", errPersisted)
		}
		if liveness.currentDeadline() != deadline {
			t.Fatal("late persistence revived an expired deadline")
		}
	})
}
