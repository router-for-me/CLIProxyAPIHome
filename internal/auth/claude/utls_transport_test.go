package claude

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/http2"
)

type blockingClaudeDialer struct {
	started chan struct{}
	once    sync.Once
}

func (*blockingClaudeDialer) Dial(string, string) (net.Conn, error) {
	return nil, errors.New("context-free dial must not be used")
}

func (d *blockingClaudeDialer) DialContext(ctx context.Context, _, _ string) (net.Conn, error) {
	d.once.Do(func() { close(d.started) })
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestUTLSConnectionCreationAndWaitHonorContext(t *testing.T) {
	dialer := &blockingClaudeDialer{started: make(chan struct{})}
	transport := &utlsRoundTripper{
		connections: make(map[string]*http2.ClientConn),
		pending:     make(map[string]chan struct{}),
		dialer:      dialer,
	}
	creatorCtx, cancelCreator := context.WithCancel(context.Background())
	creatorDone := make(chan error, 1)
	go func() {
		_, errConnection := transport.getOrCreateConnection(creatorCtx, "example.com", "example.com:443")
		creatorDone <- errConnection
	}()
	select {
	case <-dialer.started:
	case <-time.After(time.Second):
		t.Fatal("connection creation did not start")
	}

	waiterCtx, cancelWaiter := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancelWaiter()
	_, errWaiter := transport.getOrCreateConnection(waiterCtx, "example.com", "example.com:443")
	if !errors.Is(errWaiter, context.DeadlineExceeded) {
		t.Fatalf("waiting connection error = %v, want context deadline exceeded", errWaiter)
	}

	cancelCreator()
	select {
	case errCreator := <-creatorDone:
		if !errors.Is(errCreator, context.Canceled) {
			t.Fatalf("creating connection error = %v, want context canceled", errCreator)
		}
	case <-time.After(time.Second):
		t.Fatal("creating connection ignored context cancellation")
	}
}
