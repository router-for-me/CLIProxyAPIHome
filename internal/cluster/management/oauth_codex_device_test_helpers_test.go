package management

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	"gorm.io/gorm"
)

type codexDeviceRoundTripper func(*http.Request) (*http.Response, error)

func (f codexDeviceRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type failingCodexDeviceWriter struct {
	gin.ResponseWriter
	err error
}

func (w failingCodexDeviceWriter) Write([]byte) (int, error) {
	return 0, w.err
}

type codexDeviceHTTPResponseWriter struct {
	status   int
	body     bytes.Buffer
	flushErr error
	header   http.Header
	onFlush  func() error
}

func (w *codexDeviceHTTPResponseWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *codexDeviceHTTPResponseWriter) Write(body []byte) (int, error) {
	return w.body.Write(body)
}

func (w *codexDeviceHTTPResponseWriter) WriteHeader(status int) {
	w.status = status
}

func (w *codexDeviceHTTPResponseWriter) FlushError() error {
	if w.onFlush != nil {
		return w.onFlush()
	}
	return w.flushErr
}

func (w *codexDeviceHTTPResponseWriter) Flush() {
	_ = w.FlushError()
}

type gatedCodexDeviceWriter struct {
	gin.ResponseWriter
	started   chan struct{}
	release   <-chan struct{}
	completed chan struct{}
}

func (w gatedCodexDeviceWriter) Write(body []byte) (int, error) {
	close(w.started)
	<-w.release
	n, errWrite := w.ResponseWriter.Write(body)
	close(w.completed)
	return n, errWrite
}

func onlyCodexOAuthSession(t *testing.T, db *gorm.DB) *cluster.OAuthSessionRecord {
	t.Helper()
	var sessions []cluster.OAuthSessionRecord
	if errFind := db.Find(&sessions).Error; errFind != nil {
		t.Fatal(errFind)
	}
	if len(sessions) != 1 {
		t.Fatalf("OAuth sessions = %d, want 1", len(sessions))
	}
	return &sessions[0]
}

func managementDeviceResponse(statusCode int, body string) *http.Response {
	return &http.Response{
		StatusCode: statusCode,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}
}
