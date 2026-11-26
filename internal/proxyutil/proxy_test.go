package proxyutil

import (
	"context"
	"errors"
	"io"
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
	dialCtx, cancelDial := context.WithCancel(context.Background())
	defer cancelDial()
	dialResult := make(chan error, 1)
	go func() {
		conn, errDial := transport.DialContext(dialCtx, "tcp", "example.com:443")
		if conn != nil {
			_ = conn.Close()
		}
		dialResult <- errDial
	}()

	var conn net.Conn
	select {
	case conn = <-accepted:
	case <-time.After(5 * time.Second):
		t.Fatal("SOCKS proxy did not accept the test connection")
	}
	defer func() {
		if errClose := conn.Close(); errClose != nil && !errors.Is(errClose, net.ErrClosed) {
			t.Errorf("proxy connection close error = %v", errClose)
		}
	}()
	if errDeadline := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); errDeadline != nil {
		t.Fatalf("SetReadDeadline() error = %v", errDeadline)
	}
	// Observe the SOCKS greeting before cancellation, then leave the handshake
	// unanswered so only the context can interrupt the client's blocked read.
	if _, errRead := io.ReadFull(conn, make([]byte, 3)); errRead != nil {
		t.Fatalf("read SOCKS greeting error = %v", errRead)
	}
	cancelDial()
	select {
	case errDial := <-dialResult:
		if errDial == nil || !errors.Is(dialCtx.Err(), context.Canceled) {
			t.Fatalf("DialContext() error/context = %v/%v, want context cancellation", errDial, dialCtx.Err())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SOCKS dial did not return after context cancellation")
	}
}
