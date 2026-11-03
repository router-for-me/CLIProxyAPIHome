package protocolmux

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

// IsRESPPrefix reports whether is resp prefix.
func IsRESPPrefix(prefix byte) bool {
	switch prefix {
	case '*', '$', '+', '-', ':':
		return true
	default:
		return false
	}
}

const (
	connectionSetupTimeout = 5 * time.Second
	maxPendingSetups       = 128
)

// Serve classifies connections concurrently with a bounded setup budget.
// Callbacks take ownership of their connections and must dispatch promptly.
func Serve(ctx context.Context, listener net.Listener, httpListener *Listener, onRESPConn func(net.Conn), onHTTPConn func(net.Conn), tlsConfig *tls.Config) error {
	if listener == nil {
		return net.ErrClosed
	}
	if ctx == nil {
		ctx = context.Background()
	}
	serveCtx, cancel := context.WithCancel(ctx)
	stopListener := context.AfterFunc(serveCtx, func() {
		closeMuxListener(listener)
	})
	var pending sync.WaitGroup
	defer func() {
		cancel()
		stopListener()
		pending.Wait()
	}()
	setups := make(chan struct{}, maxPendingSetups)
	for {
		conn, errAccept := listener.Accept()
		if errAccept != nil {
			return errAccept
		}
		if conn == nil {
			continue
		}
		if serveCtx.Err() != nil {
			closeMuxConn(conn)
			return net.ErrClosed
		}
		select {
		case setups <- struct{}{}:
			pending.Add(1)
			go func() {
				defer pending.Done()
				defer func() { <-setups }()
				setupConn(serveCtx, conn, httpListener, onRESPConn, onHTTPConn, tlsConfig)
			}()
		default:
			// Keep accepting when the setup budget is full, without accumulating
			// unbounded sockets or goroutines behind slow clients.
			closeMuxConn(conn)
		}
	}
}

func setupConn(ctx context.Context, conn net.Conn, httpListener *Listener, onRESPConn func(net.Conn), onHTTPConn func(net.Conn), tlsConfig *tls.Config) {
	setupCtx, cancel := context.WithTimeout(ctx, connectionSetupTimeout)
	defer cancel()
	stopClose := context.AfterFunc(setupCtx, func() { closeMuxConn(conn) })
	handedOff := false
	routedConn := conn
	defer func() {
		if recovered := recover(); recovered != nil {
			var remoteAddr net.Addr
			if conn != nil {
				remoteAddr = conn.RemoteAddr()
			}
			log.Errorf("protocol mux: panic while routing connection from %v: %v", remoteAddr, recovered)
			handedOff = false
		}
		stopClose()
		if !handedOff {
			if routedConn != nil && routedConn != conn {
				closeMuxConn(routedConn)
			}
			closeMuxConn(conn)
		}
	}()
	deadline, _ := setupCtx.Deadline()
	if errDeadline := conn.SetDeadline(deadline); errDeadline != nil {
		return
	}
	routed, resp, errClassify := classifyConn(setupCtx, conn, tlsConfig)
	if errClassify != nil || !stopClose() || setupCtx.Err() != nil {
		return
	}
	routedConn = routed
	// Setup deadlines must not leak into established HTTP/2 or RESP sessions.
	if errDeadline := routed.SetDeadline(time.Time{}); errDeadline != nil {
		return
	}
	if resp {
		if onRESPConn != nil {
			handedOff = true
			onRESPConn(routed)
		}
		return
	}
	if onHTTPConn != nil {
		handedOff = true
		onHTTPConn(routed)
		return
	}
	if httpListener != nil {
		handedOff = httpListener.putContext(setupCtx, routed) == nil
	}
}

func classifyConn(ctx context.Context, conn net.Conn, tlsConfig *tls.Config) (net.Conn, bool, error) {
	if tlsConn, ok := conn.(*tls.Conn); ok {
		if errHandshake := tlsConn.HandshakeContext(ctx); errHandshake != nil {
			return nil, false, errHandshake
		}
		if isHTTPProtocol(tlsConn) {
			return tlsConn, false, nil
		}
	}
	reader := bufio.NewReader(conn)
	prefix, errPeek := reader.Peek(1)
	if errPeek != nil {
		return nil, false, errPeek
	}
	if tlsConfig != nil && isTLSClientHello(prefix[0]) {
		tlsConn := tls.Server(&BufferedConn{Conn: conn, reader: reader}, tlsConfig)
		if errHandshake := tlsConn.HandshakeContext(ctx); errHandshake != nil {
			return nil, false, errHandshake
		}
		if isHTTPProtocol(tlsConn) {
			return tlsConn, false, nil
		}
		reader = bufio.NewReader(tlsConn)
		prefix, errPeek = reader.Peek(1)
		if errPeek != nil {
			return nil, false, errPeek
		}
		conn = tlsConn
	}
	return &BufferedConn{Conn: conn, reader: reader}, IsRESPPrefix(prefix[0]), nil
}

func isHTTPProtocol(conn *tls.Conn) bool {
	proto := strings.TrimSpace(conn.ConnectionState().NegotiatedProtocol)
	return proto == "h2" || proto == "http/1.1"
}

func closeMuxConn(conn net.Conn) {
	if errClose := conn.Close(); errClose != nil && !errors.Is(errClose, net.ErrClosed) {
		log.WithError(errClose).Debug("protocol mux: close connection")
	}
}

func closeMuxListener(listener net.Listener) {
	if errClose := listener.Close(); errClose != nil && !errors.Is(errClose, net.ErrClosed) {
		log.WithError(errClose).Debug("protocol mux: close listener")
	}
}

func isTLSClientHello(prefix byte) bool {
	return prefix == 0x16
}

// NormalizeServeError normalizes a serve error.
func NormalizeServeError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, net.ErrClosed) {
		return nil
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
