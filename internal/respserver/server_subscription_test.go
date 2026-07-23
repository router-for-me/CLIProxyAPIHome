package respserver

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"net"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	appconfig "github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/home"
	"github.com/router-for-me/CLIProxyAPIHome/internal/respserver/dispatch"
)

func TestProtocolOneSubscribeCreatesMembershipWithCurrentHome(t *testing.T) {
	repo := newSubscriptionTestRepository(t)
	coordinator := cluster.NewCoordinator(repo, cluster.NodeIdentity{IP: "127.0.0.1", Port: 8317}, cluster.CoordinatorOptions{})
	if errInitialize := coordinator.Initialize(context.Background()); errInitialize != nil {
		t.Fatal(errInitialize)
	}
	defer func() {
		if errRetire := coordinator.RetireHomeIncarnation(context.Background()); errRetire != nil {
			t.Errorf("retire Home incarnation: %v", errRetire)
		}
	}()
	lifecycle, errLifecycle := repo.LifecycleConfig(context.Background())
	if errLifecycle != nil {
		t.Fatal(errLifecycle)
	}

	runtimeHome := newSubscriptionTestRuntime(t, &blockingConfigAdapter{payload: []byte("host: \"\"\nport: 8327\n"), loadStarted: make(chan struct{})})
	conn := newSubscriptionTestConn("SUBSCRIBE", "config", strconv.FormatInt(lifecycle.LifecycleConfigRevision, 10))
	server := New("", runtimeHome)
	server.SetClusterHandler(cluster.NewRESPHandler(coordinator, nil, repo))
	serverDone := make(chan struct{})
	go func() {
		server.HandleConn(context.Background(), conn)
		close(serverDone)
	}()
	defer func() {
		if errClose := conn.Close(); errClose != nil {
			t.Errorf("close test connection: %v", errClose)
		}
		waitSubscriptionTestSignal(t, serverDone, "RESP server shutdown")
	}()

	fingerprint := peerCertificateFingerprint(conn)
	commandHome, initialized := coordinator.HomeIncarnation()
	if !initialized {
		t.Fatal("coordinator Home incarnation is not initialized")
	}
	deadline := time.Now().Add(respPipeDeadline)
	for time.Now().Before(deadline) {
		lifetime, errClassify := repo.ClassifyConnection(context.Background(), fingerprint, commandHome)
		if errClassify == nil && lifetime.Controlled {
			return
		}
		goruntime.Gosched()
	}
	t.Fatalf("protocol 1 subscription did not create membership")
}

func TestBootstrapConnectionNeverBecomesControlled(t *testing.T) {
	repo := newSubscriptionTestRepository(t)
	coordinator := cluster.NewCoordinator(repo, cluster.NodeIdentity{IP: "127.0.0.1", Port: 8317}, cluster.CoordinatorOptions{})
	if errInitialize := coordinator.Initialize(context.Background()); errInitialize != nil {
		t.Fatal(errInitialize)
	}
	defer func() {
		if errRetire := coordinator.RetireHomeIncarnation(context.Background()); errRetire != nil {
			t.Errorf("retire Home incarnation: %v", errRetire)
		}
	}()
	lifecycle, errLifecycle := repo.LifecycleConfig(context.Background())
	if errLifecycle != nil {
		t.Fatal(errLifecycle)
	}

	runtimeHome := newSubscriptionTestRuntime(t, &blockingConfigAdapter{payload: []byte("host: \"\"\nport: 8327\n"), loadStarted: make(chan struct{})})
	conn := newSubscriptionTestConnCommands(
		[]string{"SUBSCRIBE", "config", strconv.FormatInt(lifecycle.LifecycleConfigRevision, 10)},
		[]string{"RPOP", "dispatch"},
	)
	server := New("", runtimeHome)
	server.SetClusterHandler(cluster.NewRESPHandler(coordinator, nil, repo))
	serverDone := make(chan struct{})
	go func() {
		server.HandleConn(context.Background(), conn)
		close(serverDone)
	}()
	defer func() {
		if errClose := conn.Close(); errClose != nil {
			t.Errorf("close test connection: %v", errClose)
		}
		waitSubscriptionTestSignal(t, serverDone, "RESP server shutdown")
	}()

	deadline := time.Now().Add(respPipeDeadline)
	for time.Now().Before(deadline) {
		if strings.Contains(conn.Output(), "controlled connection required") {
			return
		}
		goruntime.Gosched()
	}
	t.Fatalf("bootstrap subscriber executed controlled command: %q", conn.Output())
}

func TestConfigSubscriptionPublishesInitialSnapshotBeforeConcurrentUpdate(t *testing.T) {
	previousMaxProcs := goruntime.GOMAXPROCS(1)
	defer goruntime.GOMAXPROCS(previousMaxProcs)

	adapter := &blockingConfigAdapter{
		payload:     []byte("host: \"\"\nport: 8327\ntest-marker: initial-snapshot\n"),
		loadStarted: make(chan struct{}),
		releaseLoad: make(chan struct{}),
	}
	runtimeHome := newSubscriptionTestRuntime(t, adapter)
	conn := newSubscriptionTestConn("SUBSCRIBE", "config")
	serverDone := make(chan struct{})
	go func() {
		New("", runtimeHome).HandleConn(context.Background(), conn)
		close(serverDone)
	}()

	waitSubscriptionTestSignal(t, adapter.loadStarted, "initial config load")
	publishDone := make(chan struct{})
	go func() {
		runtimeHome.PublishConfigYAML([]byte("host: \"\"\nport: 8327\ntest-marker: update-b\n"))
		close(publishDone)
	}()
	goruntime.Gosched()
	close(adapter.releaseLoad)
	waitSubscriptionTestSignal(t, publishDone, "config publish")

	output := conn.Output()
	ackIndex := strings.Index(output, "subscribe")
	initialIndex := strings.Index(output, "initial-snapshot")
	updateBIndex := strings.Index(output, "update-b")
	if ackIndex < 0 || initialIndex < 0 || updateBIndex < 0 {
		t.Fatalf("subscription output missing expected frames: %q", output)
	}
	if ackIndex > initialIndex || initialIndex > updateBIndex {
		t.Fatalf(
			"subscription frame order = ack:%d initial:%d update-b:%d, output %q",
			ackIndex,
			initialIndex,
			updateBIndex,
			output,
		)
	}

	if errClose := conn.Close(); errClose != nil {
		t.Fatalf("close test connection: %v", errClose)
	}
	waitSubscriptionTestSignal(t, serverDone, "RESP server shutdown")
}

func TestConfigSubscriptionDeliveryPreservesQueuedOrder(t *testing.T) {
	ready := make(chan struct{})
	aborted := make(chan struct{})
	var output bytes.Buffer
	delivery := newConfigSubscriptionDelivery(context.Background(), newSafeWriter(&output), ready, aborted)

	initialTail := configSubscriptionDeliveryTail(delivery)
	errB := make(chan error, 1)
	go func() { errB <- delivery.Write([]byte("update-b")) }()
	waitConfigSubscriptionDeliveryQueued(t, delivery, initialTail, "update B")

	tailB := configSubscriptionDeliveryTail(delivery)
	errC := make(chan error, 1)
	go func() { errC <- delivery.Write([]byte("update-c")) }()
	waitConfigSubscriptionDeliveryQueued(t, delivery, tailB, "update C")

	close(ready)
	if errWrite := <-errB; errWrite != nil {
		t.Fatalf("write update B: %v", errWrite)
	}
	if errWrite := <-errC; errWrite != nil {
		t.Fatalf("write update C: %v", errWrite)
	}
	updateBIndex := strings.Index(output.String(), "update-b")
	updateCIndex := strings.Index(output.String(), "update-c")
	if updateBIndex < 0 || updateCIndex < 0 || updateBIndex > updateCIndex {
		t.Fatalf("queued delivery output = %q", output.String())
	}
}

func TestConfigSubscriptionReadFailureAbortsPendingUpdates(t *testing.T) {
	previousMaxProcs := goruntime.GOMAXPROCS(1)
	defer goruntime.GOMAXPROCS(previousMaxProcs)

	adapter := &blockingConfigAdapter{
		err:         errors.New("config read failed"),
		loadStarted: make(chan struct{}),
		releaseLoad: make(chan struct{}),
	}
	runtimeHome := newSubscriptionTestRuntime(t, adapter)
	conn := newSubscriptionTestConn("SUBSCRIBE", "config")
	serverDone := make(chan struct{})
	go func() {
		New("", runtimeHome).HandleConn(context.Background(), conn)
		close(serverDone)
	}()

	waitSubscriptionTestSignal(t, adapter.loadStarted, "initial config load")
	publishDone := make(chan struct{})
	go func() {
		runtimeHome.PublishConfigYAML([]byte("host: \"\"\nport: 8327\ntest-marker: must-not-publish\n"))
		close(publishDone)
	}()
	goruntime.Gosched()
	close(adapter.releaseLoad)
	waitSubscriptionTestSignal(t, publishDone, "aborted config publish")
	waitSubscriptionTestSignal(t, serverDone, "RESP server shutdown")

	output := conn.Output()
	if !strings.Contains(output, "subscribe") {
		t.Fatalf("subscription output missing ACK: %q", output)
	}
	if strings.Contains(output, "must-not-publish") || strings.Contains(output, "message") {
		t.Fatalf("failed subscription published a config message: %q", output)
	}
}

func TestAccountedReplyWriteFailureFencesFingerprint(t *testing.T) {
	var fenceCalls, closeCalls int
	connEnv := &dispatch.ConnEnv{
		FenceFingerprint: func(_ context.Context, reason string) error {
			fenceCalls++
			if reason != "accounted_dispatch_delivery_failed" {
				t.Fatalf("fence reason = %q", reason)
			}
			return nil
		},
		CloseLocalFingerprint: func() { closeCalls++ },
	}
	if errWrite := writeDispatchReplyWithFence(context.Background(), newSafeWriter(failingSubscriptionWriter{}), dispatch.Reply{Kind: dispatch.ReplyKindBulkString, BulkString: []byte("reply"), AccountedAdmission: true}, connEnv); errWrite == nil {
		t.Fatal("write unexpectedly succeeded")
	}
	if fenceCalls != 1 || closeCalls != 1 {
		t.Fatalf("fence calls = %d, close calls = %d", fenceCalls, closeCalls)
	}
}

func TestAccountedReplyPreparationFailureFencesFingerprint(t *testing.T) {
	var fenceCalls, closeCalls int
	connEnv := &dispatch.ConnEnv{
		FenceFingerprint:      func(context.Context, string) error { fenceCalls++; return errors.New("database unavailable") },
		CloseLocalFingerprint: func() { closeCalls++ },
	}
	reply := dispatch.Reply{Kind: dispatch.ReplyKindBulkString, BulkString: []byte("reply"), AccountedAdmission: true, PreWriteError: errors.New("tuple injection failed")}
	if errWrite := writeDispatchReplyWithFence(context.Background(), newSafeWriter(&bytes.Buffer{}), reply, connEnv); errWrite == nil {
		t.Fatal("write unexpectedly succeeded")
	}
	if fenceCalls != 1 || closeCalls != 1 {
		t.Fatalf("fence calls = %d, close calls = %d", fenceCalls, closeCalls)
	}
}

func TestAccountedFenceStartsDistributedFenceBeforeBlockingLocalClose(t *testing.T) {
	fenceStarted := make(chan struct{})
	localCloseStarted := make(chan struct{})
	releaseLocalClose := make(chan struct{})
	finished := make(chan struct{})
	connEnv := &dispatch.ConnEnv{
		FenceFingerprint: func(context.Context, string) error {
			close(fenceStarted)
			return nil
		},
		CloseLocalFingerprint: func() {
			close(localCloseStarted)
			<-releaseLocalClose
		},
	}
	go func() {
		fenceAccountedDispatch(context.Background(), connEnv)
		close(finished)
	}()

	select {
	case <-fenceStarted:
	case <-time.After(time.Second):
		t.Fatal("distributed fence did not start before blocking local close")
	}
	select {
	case <-localCloseStarted:
	case <-time.After(time.Second):
		t.Fatal("local fingerprint close did not start after distributed fence")
	}

	close(releaseLocalClose)
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("fence did not complete")
	}
}

func TestAccountedFenceUsesServerContext(t *testing.T) {
	type contextKey struct{}
	serverCtx, cancel := context.WithCancel(context.WithValue(context.Background(), contextKey{}, "server"))
	cancel()
	fenced := false
	connEnv := &dispatch.ConnEnv{
		FenceFingerprint: func(fenceCtx context.Context, _ string) error {
			fenced = fenceCtx.Value(contextKey{}) == "server" && fenceCtx.Err() == nil
			return nil
		},
	}
	fenceAccountedDispatch(serverCtx, connEnv)
	if !fenced {
		t.Fatal("fence did not receive a live context derived from the server context")
	}
}

type failingSubscriptionWriter struct{}

func (failingSubscriptionWriter) Write([]byte) (int, error) {
	return 0, errors.New("write failed")
}

func TestConfigSubscriptionDeliveryCanceledContextDoesNotWrite(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ready := make(chan struct{})
	close(ready)
	aborted := make(chan struct{})
	var output bytes.Buffer
	delivery := newConfigSubscriptionDelivery(ctx, newSafeWriter(&output), ready, aborted)
	for attempt := 0; attempt < 128; attempt++ {
		if errWrite := delivery.Write([]byte("must-not-publish")); !errors.Is(errWrite, context.Canceled) {
			t.Fatalf("Write(%d) error = %v, want context.Canceled", attempt, errWrite)
		}
	}
	if output.Len() != 0 {
		t.Fatalf("canceled delivery wrote %d bytes: %q", output.Len(), output.String())
	}
}

func newSubscriptionTestRepository(t *testing.T) *cluster.Repository {
	t.Helper()
	db, errOpen := cluster.OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "home.db"))
	if errOpen != nil {
		t.Fatal(errOpen)
	}
	if errMigrate := cluster.AutoMigrate(db); errMigrate != nil {
		t.Fatal(errMigrate)
	}
	t.Cleanup(func() {
		sqlDB, errDB := db.DB()
		if errDB == nil {
			if errClose := sqlDB.Close(); errClose != nil {
				t.Errorf("close database: %v", errClose)
			}
		}
	})
	return cluster.NewRepository(db)
}

func newSubscriptionTestRuntime(t *testing.T, adapter *blockingConfigAdapter) *home.Runtime {
	t.Helper()
	runtimeHome, errRuntime := home.NewRuntime(&appconfig.Config{})
	if errRuntime != nil {
		t.Fatalf("NewRuntime() error = %v", errRuntime)
	}
	runtimeHome.SetClusterAdapter(adapter)
	return runtimeHome
}

func waitSubscriptionTestSignal(t *testing.T, signal <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(respPipeDeadline):
		t.Fatalf("timed out waiting for %s", description)
	}
}

func configSubscriptionDeliveryTail(delivery *configSubscriptionDelivery) <-chan struct{} {
	delivery.queueMu.Lock()
	defer delivery.queueMu.Unlock()
	return delivery.tail
}

func waitConfigSubscriptionDeliveryQueued(t *testing.T, delivery *configSubscriptionDelivery, previous <-chan struct{}, description string) {
	t.Helper()
	deadline := time.Now().Add(respPipeDeadline)
	for time.Now().Before(deadline) {
		if configSubscriptionDeliveryTail(delivery) != previous {
			return
		}
		goruntime.Gosched()
	}
	t.Fatalf("timed out waiting for %s to queue", description)
}

type blockingConfigAdapter struct {
	payload     []byte
	err         error
	loadStarted chan struct{}
	releaseLoad chan struct{}
	startOnce   sync.Once
}

func (a *blockingConfigAdapter) Enabled() bool {
	return true
}

func (a *blockingConfigAdapter) LoadAuthIndex(context.Context) error {
	return nil
}

func (a *blockingConfigAdapter) ListMinimalAuths() []*coreauth.Auth {
	return nil
}

func (a *blockingConfigAdapter) GetFullAuth(context.Context, string) (*coreauth.Auth, error) {
	return nil, nil
}

func (a *blockingConfigAdapter) LoadConfigYAML(ctx context.Context) ([]byte, error) {
	a.startOnce.Do(func() {
		close(a.loadStarted)
	})
	if a.releaseLoad != nil {
		select {
		case <-a.releaseLoad:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if a.err != nil {
		return nil, a.err
	}
	return append([]byte(nil), a.payload...), nil
}

type subscriptionTestConn struct {
	inputMu   sync.Mutex
	input     *bytes.Reader
	outputMu  sync.Mutex
	output    bytes.Buffer
	closed    chan struct{}
	closeOnce sync.Once
	state     tls.ConnectionState
}

func newSubscriptionTestConn(args ...string) *subscriptionTestConn {
	return newSubscriptionTestConnCommands(args)
}

func newSubscriptionTestConnCommands(commands ...[]string) *subscriptionTestConn {
	var command bytes.Buffer
	for _, args := range commands {
		command.WriteString("*")
		command.WriteString(strconv.Itoa(len(args)))
		command.WriteString("\r\n")
		for _, arg := range args {
			command.WriteString("$")
			command.WriteString(strconv.Itoa(len(arg)))
			command.WriteString("\r\n")
			command.WriteString(arg)
			command.WriteString("\r\n")
		}
	}
	certificate := &x509.Certificate{
		Raw:     []byte("subscription-test-certificate"),
		Subject: pkix.Name{CommonName: "subscription-test-node"},
	}
	return &subscriptionTestConn{
		input:  bytes.NewReader(command.Bytes()),
		closed: make(chan struct{}),
		state: tls.ConnectionState{
			PeerCertificates: []*x509.Certificate{certificate},
			VerifiedChains:   [][]*x509.Certificate{{certificate}},
		},
	}
}

func (c *subscriptionTestConn) Read(payload []byte) (int, error) {
	c.inputMu.Lock()
	if c.input.Len() > 0 {
		count, errRead := c.input.Read(payload)
		c.inputMu.Unlock()
		return count, errRead
	}
	c.inputMu.Unlock()
	<-c.closed
	return 0, io.EOF
}

func (c *subscriptionTestConn) Write(payload []byte) (int, error) {
	select {
	case <-c.closed:
		return 0, net.ErrClosed
	default:
	}
	c.outputMu.Lock()
	defer c.outputMu.Unlock()
	return c.output.Write(payload)
}

func (c *subscriptionTestConn) Close() error {
	c.closeOnce.Do(func() {
		close(c.closed)
	})
	return nil
}

func (c *subscriptionTestConn) LocalAddr() net.Addr {
	return &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 8327}
}

func (c *subscriptionTestConn) RemoteAddr() net.Addr {
	return &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 48327}
}

func (c *subscriptionTestConn) SetDeadline(time.Time) error {
	return nil
}

func (c *subscriptionTestConn) SetReadDeadline(time.Time) error {
	return nil
}

func (c *subscriptionTestConn) SetWriteDeadline(time.Time) error {
	return nil
}

func (c *subscriptionTestConn) ConnectionState() tls.ConnectionState {
	return c.state
}

func (c *subscriptionTestConn) Output() string {
	c.outputMu.Lock()
	defer c.outputMu.Unlock()
	return c.output.String()
}
