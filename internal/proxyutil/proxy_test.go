package proxyutil

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

func TestSOCKSTransportDialContextHonorsCancellation(t *testing.T) {
	listener, errListen := net.Listen("tcp", "127.0.0.1:0")
	if errListen != nil {
		t.Fatalf("Listen() error = %v", errListen)
	}
	defer func() {
		if errClose := listener.Close(); errClose != nil && !errors.Is(errClose, net.ErrClosed) {
			t.Errorf("listener close error = %v", errClose)
		}
	}()

	accepted := make(chan net.Conn, 1)
	go func() {
		conn, errAccept := listener.Accept()
		if errAccept == nil {
			accepted <- conn
		}
	}()

	transport, _, errTransport := BuildHTTPTransport("socks5://" + listener.Addr().String())
	if errTransport != nil {
		t.Fatalf("BuildHTTPTransport() error = %v", errTransport)
	}
	deadlineCtx, cancelDeadline := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancelDeadline()
	startedAt := time.Now()
	_, errDial := transport.DialContext(deadlineCtx, "tcp", "example.com:443")
	if errDial == nil || !errors.Is(deadlineCtx.Err(), context.DeadlineExceeded) {
		t.Fatalf("DialContext() error/context = %v/%v, want cancellation at deadline", errDial, deadlineCtx.Err())
	}
	if elapsed := time.Since(startedAt); elapsed > time.Second {
		t.Fatalf("SOCKS cancellation took %v, want under 1s", elapsed)
	}

	select {
	case conn := <-accepted:
		_ = conn.Close()
	case <-time.After(time.Second):
		t.Fatal("SOCKS proxy did not accept the test connection")
	}
}
