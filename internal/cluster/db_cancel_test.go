package cluster

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"
)

func TestOpenPostgresCancelsDuringHandshake(t *testing.T) {
	listener, errListen := net.Listen("tcp", "127.0.0.1:0")
	if errListen != nil {
		t.Fatal(errListen)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, errAccept := listener.Accept()
		if errAccept == nil {
			accepted <- conn
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, errOpen := Open(ctx, PGSQLConfig{
			PostgresURI: fmt.Sprintf("postgres://test@%s/test?sslmode=disable&connect_timeout=10", listener.Addr()),
		})
		result <- errOpen
	}()
	var conn net.Conn
	select {
	case conn = <-accepted:
		defer conn.Close()
	case <-time.After(5 * time.Second):
		t.Fatal("PostgreSQL connection was not attempted")
	}
	// The peer accepts TCP but never answers the PostgreSQL startup packet.
	cancel()
	select {
	case errOpen := <-result:
		if !errors.Is(errOpen, context.Canceled) {
			t.Errorf("Open error = %v, want context cancellation", errOpen)
		}
	case <-time.After(2 * time.Second):
		t.Error("Open ignored cancellation during the PostgreSQL handshake")
		conn.Close()
		select {
		case <-result:
		case <-time.After(12 * time.Second):
			t.Error("Open did not terminate after the test peer closed")
		}
	}
}
