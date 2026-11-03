package respserver

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
)

const respPipeDeadline = 2 * time.Second

// TestRESPAuthCommandDisabled verifies RESP AUTH is no longer accepted.
func TestRESPAuthCommandDisabled(t *testing.T) {
	server, client := net.Pipe()
	setRESPPipeDeadline(t, server, client)
	defer func() {
		if errClose := client.Close(); errClose != nil {
			t.Fatalf("client close error: %v", errClose)
		}
	}()

	srv := New("", nil)
	go srv.HandleConn(context.Background(), server)

	writeRESPCommand(t, client, "AUTH", "secret")
	line := readRESPLineFromConn(t, client)
	if line != "-ERR RESP AUTH disabled; use mTLS" {
		t.Fatalf("AUTH response = %q, want disabled error", line)
	}
}

// TestUnauthenticatedClusterCommandRequiresMTLS verifies CLUSTER commands do not reach the handler without mTLS.
func TestUnauthenticatedClusterCommandRequiresMTLS(t *testing.T) {
	server, client := net.Pipe()
	setRESPPipeDeadline(t, server, client)
	defer func() {
		if errClose := client.Close(); errClose != nil {
			t.Fatalf("client close error: %v", errClose)
		}
	}()

	srv := New("", nil)
	srv.SetClusterHandler(cluster.NewRESPHandler(nil, nil, nil))
	go srv.HandleConn(context.Background(), server)

	writeRESPCommand(t, client, "CLUSTER", "PING", "secret")
	line := readRESPLineFromConn(t, client)
	if line != "-NOAUTH Authentication required." {
		t.Fatalf("CLUSTER PING response = %q, want NOAUTH", line)
	}
}

func setRESPPipeDeadline(t *testing.T, conns ...net.Conn) {
	t.Helper()
	deadline := time.Now().Add(respPipeDeadline)
	for _, conn := range conns {
		if errSetDeadline := conn.SetDeadline(deadline); errSetDeadline != nil {
			t.Fatalf("set pipe deadline error: %v", errSetDeadline)
		}
	}
}

func writeRESPCommand(t *testing.T, conn net.Conn, args ...string) {
	t.Helper()
	if _, errWrite := fmt.Fprintf(conn, "*%d\r\n", len(args)); errWrite != nil {
		t.Fatalf("write array header error: %v", errWrite)
	}
	for _, arg := range args {
		if _, errWrite := fmt.Fprintf(conn, "$%d\r\n%s\r\n", len(arg), arg); errWrite != nil {
			t.Fatalf("write arg error: %v", errWrite)
		}
	}
}

func readRESPLineFromConn(t *testing.T, conn net.Conn) string {
	t.Helper()
	line, errRead := bufio.NewReader(conn).ReadString('\n')
	if errRead != nil {
		t.Fatalf("read response line error: %v", errRead)
	}
	line = strings.TrimSuffix(line, "\n")
	line = strings.TrimSuffix(line, "\r")
	return line
}

// TestNormalizeHostIP verifies test normalize host ip behavior.
func TestNormalizeHostIP(t *testing.T) {
	// Validate request inputs before mutating persisted state.
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "ipv4",
			in:   "192.168.1.10",
			want: "192.168.1.10",
		},
		{
			name: "ipv4 host port",
			in:   "192.168.1.10:8327",
			want: "192.168.1.10",
		},
		{
			name: "ipv6",
			in:   "2001:db8::1",
			want: "2001:db8::1",
		},
		{
			name: "ipv6 host port",
			in:   "[2001:db8::1]:8327",
			want: "2001:db8::1",
		},
		{
			name: "ipv6 zone",
			in:   "fe80::1%lo0",
			want: "fe80::1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := normalizeHostIP(tt.in); got != tt.want {
				t.Fatalf("normalizeHostIP(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestIsClientHostAllowed verifies test is client host allowed behavior.
func TestIsClientHostAllowed(t *testing.T) {
	// Validate request inputs before mutating persisted state.
	tests := []struct {
		name       string
		clientIP   string
		allowHosts []string
		want       bool
	}{
		{
			name:       "empty allow list allows all clients",
			clientIP:   "203.0.113.10",
			allowHosts: nil,
			want:       true,
		},
		{
			name:       "listed ipv4 client is allowed",
			clientIP:   "203.0.113.10",
			allowHosts: []string{"203.0.113.10"},
			want:       true,
		},
		{
			name:       "unlisted ipv4 client is rejected",
			clientIP:   "203.0.113.11",
			allowHosts: []string{"203.0.113.10"},
			want:       false,
		},
		{
			name:       "ipv4 mapped client matches ipv4 allow host",
			clientIP:   "::ffff:203.0.113.10",
			allowHosts: []string{"203.0.113.10"},
			want:       true,
		},
		{
			name:       "blank client is rejected when allow list is configured",
			clientIP:   "",
			allowHosts: []string{"203.0.113.10"},
			want:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isClientHostAllowed(tt.clientIP, tt.allowHosts); got != tt.want {
				t.Fatalf("isClientHostAllowed(%q, %#v) = %t, want %t", tt.clientIP, tt.allowHosts, got, tt.want)
			}
		})
	}
}

// TestResolveRemoteIPNormalizesTCPAddr verifies test resolve remote ip normalizes tcp addr behavior.
func TestResolveRemoteIPNormalizesTCPAddr(t *testing.T) {
	ip, local := resolveRemoteIP(&net.TCPAddr{
		IP:   net.ParseIP("::ffff:127.0.0.1"),
		Port: 8327,
	})
	if ip != "127.0.0.1" {
		t.Fatalf("expected IPv4-mapped address to normalize to 127.0.0.1, got %q", ip)
	}
	if !local {
		t.Fatalf("expected IPv4 localhost to be local")
	}
}

func TestReadRESPBoundsAndOverflowProtection(t *testing.T) {
	t.Run("bulk string integer overflow and cap exceeded do not panic", func(t *testing.T) {
		for _, tc := range []struct {
			raw         string
			wantErr     string
			overflows32 bool
		}{
			{raw: "9223372036854775806\r\n", wantErr: "bulk string too large", overflows32: true},
			{raw: "9223372036854775807\r\n", wantErr: "bulk string too large", overflows32: true},
			{raw: "2147483646\r\n", wantErr: "bulk string too large"},
			{raw: "2147483647\r\n", wantErr: "bulk string too large"},
			{raw: "2147483648\r\n", wantErr: "bulk string too large", overflows32: true},
			{raw: "-2\r\n", wantErr: "protocol error"},
			{raw: "-9223372036854775808\r\n", wantErr: "protocol error", overflows32: true},
			{raw: strconv.Itoa(maxRESPBulkLen+1) + "\r\n", wantErr: "bulk string too large"},
			{raw: "1073741824\r\n", wantErr: "bulk string too large"},
			{raw: "2\r\nokXX", wantErr: "protocol error"},
		} {
			r := bufio.NewReader(strings.NewReader(tc.raw))
			_, errRead := readRESPBulkString(r)
			if errRead == nil {
				t.Fatalf("readRESPBulkString(%q) succeeded, want error %q", tc.raw, tc.wantErr)
			}
			if errors.Is(errRead, io.EOF) || errors.Is(errRead, io.ErrUnexpectedEOF) {
				t.Fatalf("readRESPBulkString(%q) returned EOF %v, want %q", tc.raw, errRead, tc.wantErr)
			}
			wantErr := tc.wantErr
			if strconv.IntSize == 32 && tc.overflows32 {
				wantErr = "protocol error"
			}
			if !strings.Contains(errRead.Error(), wantErr) {
				t.Fatalf("readRESPBulkString(%q) error = %v, want %q", tc.raw, errRead, wantErr)
			}
		}
	})

	t.Run("nil and empty bulk strings succeed", func(t *testing.T) {
		gotNil, errNil := readRESPBulkString(bufio.NewReader(strings.NewReader("-1\r\n")))
		if errNil != nil || gotNil != "" {
			t.Fatalf("readRESPBulkString(-1) = (%q, %v), want (\"\", nil)", gotNil, errNil)
		}
		gotEmpty, errEmpty := readRESPBulkString(bufio.NewReader(strings.NewReader("0\r\n\r\n")))
		if errEmpty != nil || gotEmpty != "" {
			t.Fatalf("readRESPBulkString(0) = (%q, %v), want (\"\", nil)", gotEmpty, errEmpty)
		}
	})

	t.Run("unauthenticated limits reject oversized bulk, array count, and cumulative frame budget", func(t *testing.T) {
		limits := unauthenticatedRESPReadLimits()

		oversizedBulk := "*1\r\n$" + strconv.Itoa(maxUnauthenticatedRESPBulkLen+1) + "\r\n" + strings.Repeat("x", maxUnauthenticatedRESPBulkLen+4)
		_, errBulk := readRESPArrayWithLimits(bufio.NewReader(strings.NewReader(oversizedBulk)), limits)
		if errBulk == nil || errors.Is(errBulk, io.EOF) || errors.Is(errBulk, io.ErrUnexpectedEOF) || !strings.Contains(errBulk.Error(), "bulk string too large") {
			t.Fatalf("readRESPArrayWithLimits(oversizedBulk) error = %v, want bulk string too large", errBulk)
		}

		oversizedCount := "*" + strconv.Itoa(maxUnauthenticatedRESPArrayLen+1) + "\r\n$1\r\na\r\n"
		_, errCount := readRESPArrayWithLimits(bufio.NewReader(strings.NewReader(oversizedCount)), limits)
		if errCount == nil || errors.Is(errCount, io.EOF) || errors.Is(errCount, io.ErrUnexpectedEOF) || !strings.Contains(errCount.Error(), "protocol error") {
			t.Fatalf("readRESPArrayWithLimits(oversizedCount) error = %v, want protocol error", errCount)
		}

		chunkLen := (maxUnauthenticatedRESPFrameBytes / 2) + 1
		chunk := strings.Repeat("Q", chunkLen)
		oversizedBudget := fmt.Sprintf("*2\r\n$%d\r\n%s\r\n$%d\r\n%s\r\n", chunkLen, chunk, chunkLen, chunk)
		_, errBudget := readRESPArrayWithLimits(bufio.NewReader(strings.NewReader(oversizedBudget)), limits)
		if errBudget == nil || errors.Is(errBudget, io.EOF) || errors.Is(errBudget, io.ErrUnexpectedEOF) || !strings.Contains(errBudget.Error(), "bulk string too large") {
			t.Fatalf("readRESPArrayWithLimits(oversizedBudget) error = %v, want bulk string too large", errBudget)
		}
	})

	t.Run("array count overflow and cap exceeded do not panic", func(t *testing.T) {
		for _, raw := range []string{
			"*9223372036854775807\r\n$1\r\na\r\n",
			"*9223372036854775806\r\n$1\r\na\r\n",
			"*2147483647\r\n$1\r\na\r\n",
			"*" + strconv.Itoa(maxRESPArrayLen+1) + "\r\n$1\r\na\r\n",
			"*-1\r\n$1\r\na\r\n",
		} {
			r := bufio.NewReader(strings.NewReader(raw))
			_, errRead := readRESPArray(r)
			if errRead == nil {
				t.Fatalf("readRESPArray(%q) succeeded, want error", raw)
			}
			if errors.Is(errRead, io.EOF) || errors.Is(errRead, io.ErrUnexpectedEOF) || !strings.Contains(errRead.Error(), "protocol error") {
				t.Fatalf("readRESPArray(%q) error = %v, want protocol error", raw, errRead)
			}
		}
	})

	t.Run("unbounded line without newline is rejected with protocol error before EOF", func(t *testing.T) {
		oversized := strings.Repeat("A", maxRESPLineLen+4096)
		r := bufio.NewReader(strings.NewReader(oversized))
		_, errRead := readRESPLine(r)
		if errRead == nil {
			t.Fatal("readRESPLine() succeeded on oversized line without newline, want error")
		}
		if errors.Is(errRead, io.EOF) || errors.Is(errRead, io.ErrUnexpectedEOF) || !strings.Contains(errRead.Error(), "protocol error") {
			t.Fatalf("readRESPLine() error = %v, want protocol error", errRead)
		}
	})

	t.Run("small reader buffer handles CRLF across ErrBufferFull fragments", func(t *testing.T) {
		want := strings.Repeat("D", 15)
		r := bufio.NewReaderSize(strings.NewReader(want+"\r\n"), 16)
		got, errRead := readRESPLine(r)
		if errRead != nil {
			t.Fatalf("readRESPLine(split CRLF) error = %v, want nil", errRead)
		}
		if got != want {
			t.Fatalf("readRESPLine(split CRLF) = %q, want %q", got, want)
		}
	})
}

func TestHandleConnRejectsOverflowAndOversizedFramesWithoutCrash(t *testing.T) {
	frames := []struct {
		name  string
		bytes []byte
	}{
		{
			name:  "bulk length int64 overflow",
			bytes: []byte("*1\r\n$9223372036854775806\r\n"),
		},
		{
			name:  "array count int64 overflow",
			bytes: []byte("*9223372036854775807\r\n"),
		},
		{
			name:  "huge bulk length exceeding unauthenticated cap",
			bytes: []byte("*1\r\n$1073741824\r\n"),
		},
		{
			name:  "unbounded line without CRLF",
			bytes: append([]byte("*1\r\n+"), bytes.Repeat([]byte("A"), maxRESPLineLen+8192)...),
		},
	}

	for _, tc := range frames {
		t.Run(tc.name, func(t *testing.T) {
			server, client := net.Pipe()
			setRESPPipeDeadline(t, server, client)
			defer func() {
				if errClose := client.Close(); errClose != nil && !errors.Is(errClose, net.ErrClosed) && !errors.Is(errClose, io.ErrClosedPipe) {
					t.Errorf("close client pipe: %v", errClose)
				}
			}()

			srv := New("", nil)
			go srv.HandleConn(context.Background(), server)

			go func() {
				if _, errWrite := client.Write(tc.bytes); errWrite != nil && !errors.Is(errWrite, net.ErrClosed) && !errors.Is(errWrite, io.ErrClosedPipe) {
					t.Logf("write stopped after server rejection: %v", errWrite)
				}
			}()

			line := readRESPLineFromConn(t, client)
			if !strings.HasPrefix(line, "-ERR ") {
				t.Fatalf("HandleConn(%s) response = %q, want -ERR prefix", tc.name, line)
			}
		})
	}
}

type panickingClusterHandler struct {
	lifetime cluster.ConnectionLifetime
}

func (h panickingClusterHandler) ClassifyConnection(_ context.Context, fingerprint string) (cluster.ConnectionLifetime, error) {
	if h.lifetime.Fingerprint != "" {
		return h.lifetime, nil
	}
	return cluster.ConnectionLifetime{Fingerprint: fingerprint}, nil
}

func (panickingClusterHandler) SubscribeMembership(context.Context, string, string, int, int64, bool, string) (cluster.ConnectionLifetime, error) {
	return cluster.ConnectionLifetime{}, nil
}

func (panickingClusterHandler) RefreshCPALiveness(context.Context, cluster.ConnectionLifetime) error {
	return nil
}

func (panickingClusterHandler) UpdateClientCount(context.Context, int) error {
	return nil
}

func (panickingClusterHandler) Handle(context.Context, []string, string) ([]byte, error) {
	panic("simulated panic in cluster Handle")
}

func (panickingClusterHandler) RequestClientCertificate(context.Context, string, string, []byte) ([]byte, error) {
	panic("simulated panic in RequestClientCertificate")
}

func TestHandleConnRecoversHandlerPanicAndReleasesAccounting(t *testing.T) {
	conn := newSubscriptionTestConn("CLUSTER", "PING")
	lifetime := cluster.ConnectionLifetime{
		Fingerprint: peerCertificateFingerprint(conn),
		ConnectedAt: time.Unix(1700000000, 0).UTC(),
		Controlled:  true,
	}

	srv := New("", nil)
	srv.SetClusterHandler(panickingClusterHandler{lifetime: lifetime})

	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.HandleConn(context.Background(), conn)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for HandleConn to recover and exit")
	}

	srv.fingerprints.mu.Lock()
	entry := srv.fingerprints.entries[lifetimeKey(lifetime)]
	var activeHandlers, activeConns int
	if entry != nil {
		entry.mu.Lock()
		activeHandlers = entry.handlers
		activeConns = len(entry.connections)
		entry.mu.Unlock()
	}
	srv.fingerprints.mu.Unlock()
	if entry == nil || activeHandlers != 0 || activeConns != 0 {
		t.Fatalf("fingerprint entry state after recovered handler panic: entry=%v handlers=%d conns=%d, want non-nil entry with handlers=0 conns=0", entry != nil, activeHandlers, activeConns)
	}

	// Verify that handler accounting was released on panic so Fencing does not hang.
	fenceCtx, cancelFence := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancelFence()
	if errFence := srv.FenceFingerprint(fenceCtx, lifetime, 1, func() error { return nil }); errFence != nil {
		t.Fatalf("FenceFingerprint() after recovered handler panic error = %v, want nil", errFence)
	}
}
