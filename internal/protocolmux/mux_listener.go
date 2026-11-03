package protocolmux

import (
	"context"
	"net"
	"sync"
)

type Listener struct {
	addr    net.Addr
	connCh  chan net.Conn
	closeCh chan struct{}
	once    sync.Once
}

// NewListener creates a new listener.
func NewListener(addr net.Addr, buffer int) *Listener {
	if buffer <= 0 {
		buffer = 1
	}
	return &Listener{
		addr:    addr,
		connCh:  make(chan net.Conn, buffer),
		closeCh: make(chan struct{}),
	}
}

// Put handles put.
func (l *Listener) Put(conn net.Conn) error {
	return l.putContext(context.Background(), conn)
}

func (l *Listener) putContext(ctx context.Context, conn net.Conn) error {
	if l == nil || conn == nil {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-l.closeCh:
		return net.ErrClosed
	case l.connCh <- conn:
		return nil
	}
}

// Accept manages accept.
func (l *Listener) Accept() (net.Conn, error) {
	if l == nil {
		return nil, net.ErrClosed
	}
	select {
	case <-l.closeCh:
		return nil, net.ErrClosed
	case conn := <-l.connCh:
		if conn == nil {
			return nil, net.ErrClosed
		}
		return conn, nil
	}
}

// Close manages close.
func (l *Listener) Close() error {
	if l == nil {
		return nil
	}
	l.once.Do(func() {
		close(l.closeCh)
	})
	return nil
}

// Addr adds a r.
func (l *Listener) Addr() net.Addr {
	if l == nil {
		return &net.TCPAddr{}
	}
	if l.addr == nil {
		return &net.TCPAddr{}
	}
	return l.addr
}
