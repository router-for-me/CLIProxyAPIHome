package respserver

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	"github.com/router-for-me/CLIProxyAPIHome/internal/respserver/dispatch"
)

func TestSubscriptionTimeoutThenTeardownRetainsLifetimeExpiry(t *testing.T) {
	for _, mode := range []string{"disconnect", "unsubscribe", "parent canceled"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				commands := [][]string{{"SUBSCRIBE", "config"}, {"PING", "heartbeat"}}
				if mode == "unsubscribe" {
					commands = append(commands, []string{"UNSUBSCRIBE", "config"})
				}
				connection := newSubscriptionTestConnCommands(commands...)
				lifetime := cluster.ConnectionLifetime{
					Fingerprint: peerCertificateFingerprint(connection), ConnectedAt: time.Unix(80, 0),
					Subscription: true, HeartbeatTimeout: 3 * time.Second,
				}
				var refreshes atomic.Int32
				server := New("", newSubscriptionTestRuntime(t, &blockingConfigAdapter{
					payload: []byte("host: \"\"\n"), loadStarted: make(chan struct{}),
				}))
				if mode == "unsubscribe" {
					// Exercise the existing callback without adding a production RESP command.
					if errRegister := server.registry.RegisterDirect("UNSUBSCRIBE", "config", func(_ context.Context, env dispatch.Env, _ []string) dispatch.Reply {
						_, errUnsubscribe := env.Conn.UnsubscribeConfigYAML()
						if errUnsubscribe != nil {
							return dispatch.Err(errUnsubscribe.Error())
						}
						return dispatch.Integer(0)
					}); errRegister != nil {
						t.Fatal(errRegister)
					}
				}
				server.SetClusterHandler(&deadlineLivenessHandler{
					blockingClusterHandler: blockingClusterHandler{lifetime: lifetime},
					refresh: func(context.Context) error {
						refreshes.Add(1)
						return &pgconn.PgError{Code: "55P03"}
					},
				})
				commandLifetime := lifetime
				commandLifetime.Subscription, commandLifetime.Controlled = false, true
				command, errAccept := server.fingerprints.Accept(context.Background(), newSubscriptionTestConn(), commandLifetime)
				if errAccept != nil {
					t.Fatal(errAccept)
				}
				newLifetime := commandLifetime
				newLifetime.ConnectedAt = newLifetime.ConnectedAt.Add(time.Second)
				newCommand, errAcceptNew := server.fingerprints.Accept(context.Background(), newSubscriptionTestConn(), newLifetime)
				if errAcceptNew != nil {
					t.Fatal(errAcceptNew)
				}
				finish, errBegin := command.BeginHandler()
				if errBegin != nil {
					t.Fatal(errBegin)
				}
				defer finish()
				defer func() {
					for _, conn := range []interface{ Close() error }{connection, command, newCommand} {
						if errClose := conn.Close(); errClose != nil {
							t.Error(errClose)
						}
					}
				}()
				taskCanceled := make(chan struct{})
				go func() {
					<-command.Context().Done()
					finish()
					close(taskCanceled)
				}()
				parentCtx, cancelParent := context.WithCancel(context.Background())
				defer cancelParent()
				done := make(chan struct{})
				go func() {
					server.HandleConn(parentCtx, connection)
					close(done)
				}()
				synctest.Wait()
				if refreshes.Load() == 0 {
					t.Fatal("heartbeat timeout was not exercised before teardown")
				}
				time.Sleep(time.Second)
				if mode == "parent canceled" {
					cancelParent()
				}
				if mode == "disconnect" {
					if errClose := connection.Close(); errClose != nil {
						t.Fatal(errClose)
					}
					synctest.Wait()
					select {
					case <-done:
					default:
						t.Fatal("subscription handler did not exit before expiry")
					}
				}
				if command.Context().Err() != nil {
					t.Fatal("transient timeout fenced the command before its deadline")
				}
				time.Sleep(2 * time.Second)
				synctest.Wait()
				select {
				case <-taskCanceled:
				default:
					t.Fatal("subscription teardown discarded the lifetime isolation deadline")
				}
				if newCommand.Context().Err() != nil {
					t.Fatal("old lifetime expiry canceled a newer lifetime")
				}
				if _, errAcceptOld := server.fingerprints.Accept(context.Background(), newSubscriptionTestConn(), commandLifetime); !errors.Is(errAcceptOld, ErrFingerprintFenced) {
					t.Fatalf("expired lifetime accepted a new command: %v", errAcceptOld)
				}
			})
		})
	}
}
