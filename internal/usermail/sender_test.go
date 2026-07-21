package usermail

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestSMTPSenderRenderMessageNormalizesBodyLineEndings(t *testing.T) {
	sender := &SMTPSender{
		fromAddress: "no-reply@example.com",
		fromName:    "CLIProxyAPIHome",
	}
	rendered := sender.renderMessage("alice@example.com", Message{
		Subject: "Line endings",
		Text:    "first\nsecond\r\nthird\rfourth",
	})
	_, body, found := strings.Cut(rendered, "\r\n\r\n")
	if !found {
		t.Fatalf("rendered message has no header/body separator: %q", rendered)
	}
	if want := "first\r\nsecond\r\nthird\r\nfourth\r\n"; body != want {
		t.Fatalf("rendered body = %q, want %q", body, want)
	}
	for index, current := range []byte(rendered) {
		switch current {
		case '\n':
			if index == 0 || rendered[index-1] != '\r' {
				t.Fatalf("rendered message contains bare LF at byte %d: %q", index, rendered)
			}
		case '\r':
			if index+1 >= len(rendered) || rendered[index+1] != '\n' {
				t.Fatalf("rendered message contains bare CR at byte %d: %q", index, rendered)
			}
		}
	}
}

func TestSMTPSenderTreatsQuitFailureAfterAcceptedDataAsSuccess(t *testing.T) {
	listener, errListen := net.Listen("tcp", "127.0.0.1:0")
	if errListen != nil {
		t.Fatalf("listen SMTP test server: %v", errListen)
	}
	t.Cleanup(func() { _ = listener.Close() })
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- serveSMTPAcceptThenDropQuit(listener)
	}()

	host, portRaw, errSplit := net.SplitHostPort(listener.Addr().String())
	if errSplit != nil {
		t.Fatalf("split SMTP test address: %v", errSplit)
	}
	port, errPort := strconv.Atoi(portRaw)
	if errPort != nil {
		t.Fatalf("parse SMTP test port: %v", errPort)
	}
	sender := &SMTPSender{
		config: SMTPConfig{
			Host:    host,
			Port:    port,
			Timeout: time.Second,
		},
		fromAddress: "no-reply@example.com",
		fromName:    "CLIProxyAPIHome",
	}
	if errSend := sender.Send(nil, Message{
		To:      "alice@example.com",
		Subject: "Accepted message",
		Text:    "hello",
	}); errSend != nil {
		t.Fatalf("Send() error after accepted DATA = %v", errSend)
	}
	select {
	case errServer := <-serverDone:
		if errServer != nil {
			t.Fatalf("SMTP test server: %v", errServer)
		}
	case <-time.After(time.Second):
		t.Fatal("SMTP test server did not finish")
	}
}

func TestSMTPSenderDeliversThroughSTARTTLS(t *testing.T) {
	certificate, roots := newSMTPTestCertificate(t)
	listener, errListen := net.Listen("tcp", "127.0.0.1:0")
	if errListen != nil {
		t.Fatalf("listen SMTP test server: %v", errListen)
	}
	t.Cleanup(func() { _ = listener.Close() })
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- serveSMTPSTARTTLS(listener, certificate)
	}()

	host, portRaw, errSplit := net.SplitHostPort(listener.Addr().String())
	if errSplit != nil {
		t.Fatalf("split SMTP test address: %v", errSplit)
	}
	port, errPort := strconv.Atoi(portRaw)
	if errPort != nil {
		t.Fatalf("parse SMTP test port: %v", errPort)
	}
	sender := &SMTPSender{
		config: SMTPConfig{
			Host:     host,
			Port:     port,
			StartTLS: true,
			Timeout:  2 * time.Second,
		},
		fromAddress: "no-reply@example.com",
		fromName:    "CLIProxyAPIHome",
		tlsConfig: &tls.Config{
			RootCAs:    roots,
			ServerName: host,
		},
	}
	if errSend := sender.Send(context.Background(), Message{
		To:      "alice@example.com",
		Subject: "STARTTLS message",
		Text:    "hello over TLS",
	}); errSend != nil {
		t.Fatalf("Send(STARTTLS) error = %v", errSend)
	}
	select {
	case errServer := <-serverDone:
		if errServer != nil {
			t.Fatalf("STARTTLS test server: %v", errServer)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("STARTTLS test server did not finish")
	}
}

func serveSMTPAcceptThenDropQuit(listener net.Listener) error {
	conn, errAccept := listener.Accept()
	if errAccept != nil {
		return errAccept
	}
	defer func() { _ = conn.Close() }()
	reader := bufio.NewReader(conn)
	writer := bufio.NewWriter(conn)
	writeResponse := func(response string) error {
		if _, errWrite := writer.WriteString(response); errWrite != nil {
			return errWrite
		}
		return writer.Flush()
	}
	if errWrite := writeResponse("220 localhost ESMTP ready\r\n"); errWrite != nil {
		return errWrite
	}
	for {
		line, errRead := reader.ReadString('\n')
		if errRead != nil {
			return errRead
		}
		command := strings.ToUpper(strings.TrimSpace(line))
		switch {
		case strings.HasPrefix(command, "EHLO"):
			if errWrite := writeResponse("250-localhost\r\n250 OK\r\n"); errWrite != nil {
				return errWrite
			}
		case strings.HasPrefix(command, "HELO"), strings.HasPrefix(command, "MAIL FROM:"), strings.HasPrefix(command, "RCPT TO:"):
			if errWrite := writeResponse("250 OK\r\n"); errWrite != nil {
				return errWrite
			}
		case command == "DATA":
			if errWrite := writeResponse("354 End data with <CR><LF>.<CR><LF>\r\n"); errWrite != nil {
				return errWrite
			}
			for {
				dataLine, errData := reader.ReadString('\n')
				if errData != nil {
					return errData
				}
				if dataLine == ".\r\n" {
					break
				}
			}
			if errWrite := writeResponse("250 queued\r\n"); errWrite != nil {
				return errWrite
			}
		case command == "QUIT":
			return conn.Close()
		default:
			return fmt.Errorf("unexpected SMTP command %q", command)
		}
	}
}

func serveSMTPSTARTTLS(listener net.Listener, certificate tls.Certificate) error {
	conn, errAccept := listener.Accept()
	if errAccept != nil {
		return errAccept
	}
	defer func() { _ = conn.Close() }()
	reader := bufio.NewReader(conn)
	writer := bufio.NewWriter(conn)
	writeResponse := func(response string) error {
		if _, errWrite := writer.WriteString(response); errWrite != nil {
			return errWrite
		}
		return writer.Flush()
	}
	if errWrite := writeResponse("220 localhost ESMTP ready\r\n"); errWrite != nil {
		return errWrite
	}
	line, errRead := reader.ReadString('\n')
	if errRead != nil || !strings.HasPrefix(strings.ToUpper(strings.TrimSpace(line)), "EHLO") {
		return fmt.Errorf("initial EHLO = %q, %v", strings.TrimSpace(line), errRead)
	}
	if errWrite := writeResponse("250-localhost\r\n250-STARTTLS\r\n250 OK\r\n"); errWrite != nil {
		return errWrite
	}
	line, errRead = reader.ReadString('\n')
	if errRead != nil || strings.ToUpper(strings.TrimSpace(line)) != "STARTTLS" {
		return fmt.Errorf("STARTTLS command = %q, %v", strings.TrimSpace(line), errRead)
	}
	if errWrite := writeResponse("220 Ready to start TLS\r\n"); errWrite != nil {
		return errWrite
	}
	tlsConn := tls.Server(conn, &tls.Config{
		Certificates: []tls.Certificate{certificate},
		MinVersion:   tls.VersionTLS12,
	})
	if errHandshake := tlsConn.Handshake(); errHandshake != nil {
		return errHandshake
	}
	conn = tlsConn
	reader = bufio.NewReader(conn)
	writer = bufio.NewWriter(conn)
	line, errRead = reader.ReadString('\n')
	if errRead != nil || !strings.HasPrefix(strings.ToUpper(strings.TrimSpace(line)), "EHLO") {
		return fmt.Errorf("TLS EHLO = %q, %v", strings.TrimSpace(line), errRead)
	}
	if errWrite := writeResponse("250 localhost\r\n"); errWrite != nil {
		return errWrite
	}
	for {
		line, errRead = reader.ReadString('\n')
		if errRead != nil {
			return errRead
		}
		command := strings.ToUpper(strings.TrimSpace(line))
		switch {
		case strings.HasPrefix(command, "MAIL FROM:"), strings.HasPrefix(command, "RCPT TO:"):
			if errWrite := writeResponse("250 OK\r\n"); errWrite != nil {
				return errWrite
			}
		case command == "DATA":
			if errWrite := writeResponse("354 End data with <CR><LF>.<CR><LF>\r\n"); errWrite != nil {
				return errWrite
			}
			for {
				dataLine, errData := reader.ReadString('\n')
				if errData != nil {
					return errData
				}
				if dataLine == ".\r\n" {
					break
				}
			}
			if errWrite := writeResponse("250 queued\r\n"); errWrite != nil {
				return errWrite
			}
		case command == "QUIT":
			return writeResponse("221 bye\r\n")
		default:
			return fmt.Errorf("unexpected TLS SMTP command %q", command)
		}
	}
}

func newSMTPTestCertificate(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	privateKey, errKey := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if errKey != nil {
		t.Fatalf("generate SMTP test key: %v", errKey)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:         true,
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, errCertificate := x509.CreateCertificate(rand.Reader, template, template, &privateKey.PublicKey, privateKey)
	if errCertificate != nil {
		t.Fatalf("create SMTP test certificate: %v", errCertificate)
	}
	certificate := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: privateKey}
	roots := x509.NewCertPool()
	parsed, errParse := x509.ParseCertificate(der)
	if errParse != nil {
		t.Fatalf("parse SMTP test certificate: %v", errParse)
	}
	roots.AddCert(parsed)
	return certificate, roots
}
