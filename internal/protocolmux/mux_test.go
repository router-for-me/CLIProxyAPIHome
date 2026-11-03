package protocolmux

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2"
)

type observedConn struct {
	net.Conn
	reading   chan struct{}
	once      sync.Once
	mu        sync.Mutex
	deadlines []time.Time
}

func (c *observedConn) Read(p []byte) (int, error) {
	c.once.Do(func() { close(c.reading) })
	return c.Conn.Read(p)
}

func (c *observedConn) SetDeadline(deadline time.Time) error {
	c.mu.Lock()
	c.deadlines = append(c.deadlines, deadline)
	c.mu.Unlock()
	return c.Conn.SetDeadline(deadline)
}

type observedListener struct {
	net.Listener
	accepted chan *observedConn
}

func (l *observedListener) Accept() (net.Conn, error) {
	conn, errAccept := l.Listener.Accept()
	if errAccept != nil {
		return nil, errAccept
	}
	observed := &observedConn{Conn: conn, reading: make(chan struct{})}
	l.accepted <- observed
	return observed, nil
}

func testListener(t *testing.T) *observedListener {
	t.Helper()
	listener, errListen := net.Listen("tcp", "127.0.0.1:0")
	if errListen != nil {
		t.Fatal(errListen)
	}
	t.Cleanup(func() { closeMuxTestListener(t, listener) })
	return &observedListener{Listener: listener, accepted: make(chan *observedConn, 256)}
}

func closeMuxTestListener(t *testing.T, listener net.Listener) {
	t.Helper()
	if errClose := listener.Close(); errClose != nil && !errors.Is(errClose, net.ErrClosed) {
		t.Errorf("close listener: %v", errClose)
	}
}

func testCloseConn(t *testing.T, conn net.Conn) {
	t.Helper()
	if errClose := conn.Close(); errClose != nil && !errors.Is(errClose, net.ErrClosed) {
		t.Errorf("close connection: %v", errClose)
	}
}

func testReceive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for mux progress")
		var zero T
		return zero
	}
}

func testDial(t *testing.T, listener *observedListener) (net.Conn, *observedConn) {
	t.Helper()
	conn, errDial := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
	if errDial != nil {
		t.Fatal(errDial)
	}
	t.Cleanup(func() { testCloseConn(t, conn) })
	return conn, testReceive(t, listener.accepted)
}

func testTLSConfig() *tls.Config {
	server := httptest.NewTLSServer(http.NotFoundHandler())
	config := server.TLS.Clone()
	config.NextProtos = []string{"h2", "http/1.1"}
	server.Close()
	return config
}

func TestServeIdleConnectionDoesNotBlockHTTP(t *testing.T) {
	for _, wrappedTLS := range []bool{false, true} {
		mode := "raw_listener"
		if wrappedTLS {
			mode = "tls_listener"
		}
		for _, stage := range []string{"first_byte", "tls_handshake", "post_tls_byte"} {
			t.Run(mode+"/"+stage, func(t *testing.T) {
				base := testListener(t)
				tlsConfig := testTLSConfig()
				var listener net.Listener = base
				muxTLSConfig := tlsConfig
				if wrappedTLS {
					listener = tls.NewListener(base, tlsConfig)
					muxTLSConfig = nil
				}
				httpListener := NewListener(base.Addr(), 8)
				server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(http.StatusNoContent)
				}), TLSConfig: tlsConfig}
				if errConfigure := http2.ConfigureServer(server, &http2.Server{}); errConfigure != nil {
					t.Fatal(errConfigure)
				}
				httpDone := make(chan error, 1)
				go func() { httpDone <- server.Serve(httpListener) }()
				ctx, cancel := context.WithCancel(context.Background())
				done := make(chan error, 1)
				go func() { done <- Serve(ctx, listener, httpListener, nil, nil, muxTLSConfig) }()
				t.Cleanup(func() {
					cancel()
					if errServe := testReceive(t, done); NormalizeServeError(errServe) != nil {
						t.Errorf("serve: %v", errServe)
					}
					if errClose := server.Close(); errClose != nil {
						t.Errorf("close HTTP server: %v", errClose)
					}
					if errServe := testReceive(t, httpDone); NormalizeServeError(errServe) != nil {
						t.Errorf("HTTP serve: %v", errServe)
					}
				})
				idle, observed := testDial(t, base)
				testReceive(t, observed.reading)
				switch stage {
				case "tls_handshake":
					if _, errWrite := idle.Write([]byte{0x16}); errWrite != nil {
						t.Fatal(errWrite)
					}
				case "post_tls_byte":
					client := tls.Client(idle, &tls.Config{InsecureSkipVerify: true})
					handshakeCtx, cancelHandshake := context.WithTimeout(ctx, time.Second)
					defer cancelHandshake()
					if errHandshake := client.HandshakeContext(handshakeCtx); errHandshake != nil {
						t.Fatal(errHandshake)
					}
				}
				for _, http2Enabled := range []bool{false, true} {
					transport := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, ForceAttemptHTTP2: http2Enabled}
					defer transport.CloseIdleConnections()
					client := &http.Client{Transport: transport, Timeout: time.Second}
					response, errGet := client.Get("https://" + base.Addr().String() + "/")
					if errGet != nil {
						t.Fatalf("HTTP blocked by %s: %v", stage, errGet)
					}
					if errClose := response.Body.Close(); errClose != nil {
						t.Fatal(errClose)
					}
					if response.StatusCode != http.StatusNoContent || (http2Enabled && response.ProtoMajor != 2) {
						t.Fatalf("unexpected response: %s %s", response.Proto, response.Status)
					}
					active := testReceive(t, base.accepted)
					assertSetupDeadlines(t, active, true)
				}
			})
		}
	}
}

func assertSetupDeadlines(t *testing.T, conn *observedConn, handedOff bool) {
	t.Helper()
	conn.mu.Lock()
	defer conn.mu.Unlock()
	if len(conn.deadlines) == 0 || conn.deadlines[0].IsZero() {
		t.Fatal("missing initial setup deadline")
	}
	if remaining := time.Until(conn.deadlines[0]); remaining > 5*time.Second {
		t.Fatalf("setup budget too long: %v", remaining)
	}
	if handedOff && (len(conn.deadlines) < 2 || !conn.deadlines[1].IsZero()) {
		t.Fatalf("setup deadline not cleared before handoff: %v", conn.deadlines)
	}
}

func TestServeRESPHandoffClearsDeadlineAndPreservesBytes(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		name := "plaintext"
		if encrypted {
			name = "tls"
		}
		t.Run(name, func(t *testing.T) {
			base := testListener(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			routed := make(chan net.Conn, 1)
			done := make(chan error, 1)
			go func() { done <- Serve(ctx, base, nil, func(conn net.Conn) { routed <- conn }, nil, testTLSConfig()) }()
			client, observed := testDial(t, base)
			if encrypted {
				client = tls.Client(client, &tls.Config{InsecureSkipVerify: true})
			}
			if errDeadline := client.SetDeadline(time.Now().Add(2 * time.Second)); errDeadline != nil {
				t.Fatal(errDeadline)
			}
			payload := "*1\r\n$4\r\nPING\r\n"
			if _, errWrite := io.WriteString(client, payload); errWrite != nil {
				t.Fatal(errWrite)
			}
			server := testReceive(t, routed)
			t.Cleanup(func() { testCloseConn(t, server) })
			assertSetupDeadlines(t, observed, true)
			cancel()
			if errServe := testReceive(t, done); NormalizeServeError(errServe) != nil {
				t.Fatal(errServe)
			}
			data := make([]byte, len(payload))
			if _, errRead := io.ReadFull(server, data); errRead != nil || string(data) != payload {
				t.Fatalf("buffered RESP bytes = %q, error = %v", data, errRead)
			}
			// Serve only owns setup connections; the RESP server owns this session.
			if _, errWrite := io.WriteString(server, "+PONG\r\n"); errWrite != nil {
				t.Fatal(errWrite)
			}
			response := make([]byte, 7)
			if _, errRead := io.ReadFull(client, response); errRead != nil || string(response) != "+PONG\r\n" {
				t.Fatalf("established RESP session closed: %q, %v", response, errRead)
			}
		})
	}
}

func TestServeBoundsPendingSetupsAndClosesThemOnCancel(t *testing.T) {
	base := testListener(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, base, nil, nil, nil, nil) }()
	clients := make([]net.Conn, 0, 128)
	for range 128 {
		client, observed := testDial(t, base)
		testReceive(t, observed.reading)
		assertSetupDeadlines(t, observed, false)
		clients = append(clients, client)
	}
	overflow, _ := testDial(t, base)
	assertClosed(t, overflow)
	cancel()
	if errServe := testReceive(t, done); NormalizeServeError(errServe) != nil {
		t.Fatal(errServe)
	}
	for _, client := range clients {
		assertClosed(t, client)
	}
}

func assertClosed(t *testing.T, conn net.Conn) {
	t.Helper()
	if errDeadline := conn.SetReadDeadline(time.Now().Add(time.Second)); errDeadline != nil {
		t.Fatal(errDeadline)
	}
	// A rejected TLS connection may send an alert before closing.
	_, errRead := io.Copy(io.Discard, conn)
	if netError, ok := errRead.(net.Error); ok && netError.Timeout() {
		t.Fatal("connection remained open")
	}
}

func TestServeSetupDeadlineAndFullHTTPQueue(t *testing.T) {
	for _, fullQueue := range []bool{false, true} {
		name := "read_timeout"
		if fullQueue {
			name = "full_http_queue"
		}
		t.Run(name, func(t *testing.T) {
			base := testListener(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			httpListener := NewListener(base.Addr(), 1)
			queued, peer := net.Pipe()
			t.Cleanup(func() { testCloseConn(t, queued); testCloseConn(t, peer) })
			if errPut := httpListener.Put(queued); errPut != nil {
				t.Fatal(errPut)
			}
			done := make(chan error, 1)
			go func() { done <- Serve(ctx, base, httpListener, nil, nil, nil) }()
			client, observed := testDial(t, base)
			testReceive(t, observed.reading)
			assertSetupDeadlines(t, observed, false)
			if fullQueue {
				if _, errWrite := io.WriteString(client, "GET / HTTP/1.1\r\n\r\n"); errWrite != nil {
					t.Fatal(errWrite)
				}
			} else {
				// Expire the armed deadline deterministically without a wall-clock sleep.
				if errDeadline := observed.Conn.SetDeadline(time.Now().Add(-time.Second)); errDeadline != nil {
					t.Fatal(errDeadline)
				}
				assertClosed(t, client)
			}
			cancel()
			if errServe := testReceive(t, done); NormalizeServeError(errServe) != nil {
				t.Fatal(errServe)
			}
			assertClosed(t, client)
		})
	}
}

func TestServeMalformedTLSAndListenerClose(t *testing.T) {
	base := testListener(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	routed := make(chan net.Conn, 1)
	tlsConfig := testTLSConfig()
	go func() {
		done <- Serve(ctx, base, nil, func(conn net.Conn) { routed <- conn }, nil, tlsConfig)
	}()
	malformed, _ := testDial(t, base)
	if _, errWrite := malformed.Write([]byte{0x16, 0, 0, 0, 0}); errWrite != nil {
		t.Fatal(errWrite)
	}
	assertClosed(t, malformed)
	early, _ := testDial(t, base)
	testCloseConn(t, early)
	active, _ := testDial(t, base)
	if _, errWrite := io.WriteString(active, "*1\r\n"); errWrite != nil {
		t.Fatal(errWrite)
	}
	server := testReceive(t, routed)
	t.Cleanup(func() { testCloseConn(t, server) })
	idle, observed := testDial(t, base)
	testReceive(t, observed.reading)
	closeMuxTestListener(t, base)
	if errServe := testReceive(t, done); NormalizeServeError(errServe) != nil {
		t.Fatal(errServe)
	}
	assertClosed(t, idle)
}

func TestServeNotBlockedByIdleConnectionAndRecoversPanic(t *testing.T) {
	listener, errListen := net.Listen("tcp", "127.0.0.1:0")
	if errListen != nil {
		t.Fatalf("failed to listen: %v", errListen)
	}
	defer func() {
		if errClose := listener.Close(); errClose != nil && !errors.Is(errClose, net.ErrClosed) {
			t.Errorf("close listener: %v", errClose)
		}
	}()

	var routed atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		routed.Add(1)
		w.WriteHeader(http.StatusOK)
	})

	httpLn := NewListener(listener.Addr(), 1024)
	defer func() {
		if errClose := httpLn.Close(); errClose != nil && !errors.Is(errClose, net.ErrClosed) {
			t.Errorf("close http mux listener: %v", errClose)
		}
	}()

	httpSrv := &http.Server{Handler: handler}
	httpErrCh := make(chan error, 1)
	go func() {
		httpErrCh <- NormalizeServeError(httpSrv.Serve(httpLn))
	}()
	defer func() {
		if errClose := httpSrv.Close(); errClose != nil && !errors.Is(errClose, net.ErrClosed) {
			t.Errorf("close http server: %v", errClose)
		}
	}()

	errCh := make(chan error, 1)
	go func() {
		errCh <- NormalizeServeError(Serve(context.Background(), listener, httpLn, func(conn net.Conn) {
			panic("simulated synchronous RESP callback panic")
		}, nil, nil))
	}()

	// Open an idle TCP connection that never sends any bytes.
	idleConn, errDialIdle := net.DialTimeout("tcp", listener.Addr().String(), 2*time.Second)
	if errDialIdle != nil {
		t.Fatalf("failed to dial idle connection: %v", errDialIdle)
	}
	defer func() {
		if errClose := idleConn.Close(); errClose != nil && !errors.Is(errClose, net.ErrClosed) {
			t.Errorf("close idle connection: %v", errClose)
		}
	}()

	// Trigger a RESP connection whose callback panics synchronously inside setupConn.
	respConn, errDialRESP := net.DialTimeout("tcp", listener.Addr().String(), 2*time.Second)
	if errDialRESP != nil {
		t.Fatalf("failed to dial resp connection: %v", errDialRESP)
	}
	defer func() {
		if errClose := respConn.Close(); errClose != nil && !errors.Is(errClose, net.ErrClosed) {
			t.Errorf("close resp connection: %v", errClose)
		}
	}()
	if errDeadline := respConn.SetDeadline(time.Now().Add(2 * time.Second)); errDeadline != nil {
		t.Fatalf("set resp connection deadline: %v", errDeadline)
	}
	if _, errWrite := respConn.Write([]byte("*1\r\n")); errWrite != nil {
		t.Fatalf("write resp prefix: %v", errWrite)
	}
	var buf [1]byte
	_, errRead := respConn.Read(buf[:])
	if errRead == nil {
		t.Fatal("expected panicking RESP connection to be closed by recovery")
	}
	if netErr, ok := errRead.(net.Error); ok && netErr.Timeout() {
		t.Fatalf("expected panicking RESP connection to be closed immediately, got read timeout: %v", errRead)
	}

	// Send a real HTTP request and verify the accept loop is still alive.
	client := &http.Client{Timeout: 3 * time.Second}
	resp, errGet := client.Get("http://" + listener.Addr().String() + "/")
	if errGet != nil {
		t.Fatalf("HTTP request failed: %v", errGet)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			t.Errorf("close response body: %v", errClose)
		}
	}()

	if routed.Load() == 0 {
		t.Fatal("expected HTTP request to be routed")
	}

	if errClose := listener.Close(); errClose != nil && !errors.Is(errClose, net.ErrClosed) {
		t.Fatalf("close listener: %v", errClose)
	}
	select {
	case errServe := <-errCh:
		if errServe != nil {
			t.Fatalf("Serve() returned error: %v", errServe)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Serve() to exit")
	}

	if errClose := httpLn.Close(); errClose != nil && !errors.Is(errClose, net.ErrClosed) {
		t.Fatalf("close http mux listener: %v", errClose)
	}
	select {
	case errHTTP := <-httpErrCh:
		if errHTTP != nil {
			t.Fatalf("httpSrv.Serve() returned error: %v", errHTTP)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for httpSrv.Serve() to exit")
	}
}
