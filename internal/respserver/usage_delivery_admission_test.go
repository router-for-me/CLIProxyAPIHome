package respserver

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	homeerrors "github.com/router-for-me/CLIProxyAPIHome/internal/errors"
	"github.com/router-for-me/CLIProxyAPIHome/internal/respserver/dispatch"
)

type usageDeliveryTestConn struct {
	net.Conn
	state tls.ConnectionState
}

func (c usageDeliveryTestConn) ConnectionState() tls.ConnectionState { return c.state }

func TestUsageDeliveryAdmissionSurvivesRESPClientDisconnect(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("HOME_TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("HOME_TEST_POSTGRES_DSN is not configured")
	}
	admin, errOpen := cluster.Open(t.Context(), cluster.PGSQLConfig{PostgresURI: dsn})
	if errOpen != nil {
		t.Fatal(errOpen)
	}
	adminPool, errDB := admin.DB()
	if errDB != nil {
		t.Fatal(errDB)
	}
	t.Cleanup(func() {
		if errClose := adminPool.Close(); errClose != nil {
			t.Error(errClose)
		}
	})
	schema := fmt.Sprintf("usage_admission_%d", time.Now().UnixNano())
	if errCreate := admin.Exec("CREATE SCHEMA " + schema).Error; errCreate != nil {
		t.Fatal(errCreate)
	}
	t.Cleanup(func() {
		if errDrop := admin.Exec("DROP SCHEMA " + schema + " CASCADE").Error; errDrop != nil {
			t.Error(errDrop)
		}
	})
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		parsed, errParse := url.Parse(dsn)
		if errParse != nil {
			t.Fatal(errParse)
		}
		query := parsed.Query()
		query.Set("search_path", schema)
		parsed.RawQuery = query.Encode()
		dsn = parsed.String()
	} else {
		dsn += " search_path=" + schema
	}
	db, errSchemaOpen := cluster.Open(t.Context(), cluster.PGSQLConfig{PostgresURI: dsn, MaxOpenConns: 1})
	if errSchemaOpen != nil {
		t.Fatal(errSchemaOpen)
	}
	pool, errPool := db.DB()
	if errPool != nil {
		t.Fatal(errPool)
	}
	t.Cleanup(func() {
		if errClose := pool.Close(); errClose != nil {
			t.Error(errClose)
		}
	})
	if errMigrate := cluster.AutoMigrate(db); errMigrate != nil {
		t.Fatal(errMigrate)
	}
	repo := cluster.NewRepository(db)
	server := New("", nil)
	entered := make(chan context.Context, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	var workers sync.WaitGroup
	var calls atomic.Int32
	var clients []net.Conn
	defer func() {
		releaseOnce.Do(func() { close(release) })
		for _, client := range clients {
			if errClose := client.Close(); errClose != nil {
				t.Error(errClose)
			}
		}
		workers.Wait()
	}()
	// Inject a blocking callback at the repository boundary, while exercising
	// the production HandleConn loop with net.Pipe peers and PostgreSQL capacity.
	if errRegister := server.registry.RegisterDirect("LPUSH", "usage", func(ctx context.Context, _ dispatch.Env, args []string) dispatch.Reply {
		if _, errAppend := repo.AppendUsage(ctx, args[2], ""); errAppend != nil {
			return dispatch.Err("usage database write failed")
		}
		errDelivery := repo.DeliverPluginUsage(ctx, args[2], func() error {
			if calls.Add(1) == 1 {
				entered <- ctx
				<-release
			}
			return nil
		})
		if errors.Is(errDelivery, homeerrors.ErrUsagePluginDeliveryPending) {
			return dispatch.Err("usage plugin delivery pending")
		}
		if errDelivery != nil {
			return dispatch.Err("usage plugin delivery failed")
		}
		return dispatch.Integer(1)
	}); errRegister != nil {
		t.Fatal(errRegister)
	}
	authConn := newSubscriptionTestConn()
	state := authConn.state
	server.SetClusterHandler(&blockingClusterHandler{lifetime: cluster.ConnectionLifetime{
		Fingerprint: peerCertificateFingerprint(authConn), ConnectedAt: time.Now(), Controlled: true,
	}})
	connect := func() (net.Conn, <-chan struct{}) {
		serverConn, client := net.Pipe()
		clients = append(clients, client)
		if errDeadline := client.SetDeadline(time.Now().Add(5 * time.Second)); errDeadline != nil {
			t.Fatal(errDeadline)
		}
		done := make(chan struct{})
		workers.Go(func() {
			defer close(done)
			server.HandleConn(t.Context(), usageDeliveryTestConn{Conn: serverConn, state: state})
		})
		return client, done
	}
	payload := func(index int) string {
		return fmt.Sprintf(`{"timestamp":"2026-11-06T10:27:15Z","provider":"codex","execution_id":"disconnect-%d"}`, index)
	}
	owner, ownerDone := connect()
	writeRESPCommand(t, owner, "LPUSH", "usage", payload(0))
	var ownerCtx context.Context
	select {
	case ownerCtx = <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("owner callback did not start")
	}
	// An already-expired deadline deterministically simulates CPA's read
	// timeout without wall-clock sleeps. Closing the peer cannot cancel a
	// synchronous server handler that is still inside the plugin callback.
	if errDeadline := owner.SetReadDeadline(time.Unix(1, 0)); errDeadline != nil {
		t.Fatal(errDeadline)
	}
	var one [1]byte
	_, errRead := owner.Read(one[:])
	var timeout net.Error
	if !errors.As(errRead, &timeout) || !timeout.Timeout() {
		t.Fatalf("client read did not time out: %v", errRead)
	}
	if errClose := owner.Close(); errClose != nil {
		t.Fatal(errClose)
	}
	select {
	case <-ownerCtx.Done():
		t.Fatal("test must retain the uncanceled synchronous handler")
	default:
	}
	for index := 1; index <= 32; index++ {
		client, done := connect()
		writeRESPCommand(t, client, "LPUSH", "usage", payload(index))
		if index%2 == 0 {
			if line := readRESPLineFromConn(t, client); line != "-"+dispatch.Err("usage plugin delivery pending").RedisError {
				t.Fatalf("overload reply=%q", line)
			}
		}
		if errClose := client.Close(); errClose != nil {
			t.Fatal(errClose)
		}
		waitSubscriptionTestSignal(t, done, "overloaded handler shutdown before callback release")
	}
	if calls.Load() != 1 {
		t.Fatalf("overload started %d callbacks, want one", calls.Load())
	}
	select {
	case <-ownerDone:
		t.Fatal("disconnected owner released callback ownership early")
	default:
	}
	var completed int64
	if errCount := db.Model(&cluster.UsageRecord{}).Where("plugin_usage_delivered = ?", true).Count(&completed).Error; errCount != nil || completed != 0 {
		t.Fatalf("blocked/rejected usage completed: count=%d error=%v", completed, errCount)
	}
	releaseOnce.Do(func() { close(release) })
	waitSubscriptionTestSignal(t, ownerDone, "disconnected owner shutdown after callback release")
	client, done := connect()
	// Retry a previously rejected row, then replay it to verify completion fencing.
	for range 2 {
		writeRESPCommand(t, client, "LPUSH", "usage", payload(1))
		if line := readRESPLineFromConn(t, client); line != ":1" {
			t.Fatalf("recovered capacity reply=%q", line)
		}
	}
	if errClose := client.Close(); errClose != nil {
		t.Fatal(errClose)
	}
	waitSubscriptionTestSignal(t, done, "recovered handler shutdown")
	if calls.Load() != 2 {
		t.Fatalf("retry callback count=%d, want two distinct completed events", calls.Load())
	}
}
