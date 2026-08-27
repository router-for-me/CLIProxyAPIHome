package claude

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type refreshRoundTripperFunc func(*http.Request) (*http.Response, error)

func (f refreshRoundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestRefreshTokensPreservesUpstreamBody(t *testing.T) {
	t.Parallel()

	const responseBody = `{
				"error":{"type":"invalid_request_error","message":"Refresh request was rejected"}
			}`
	svc := &ClaudeAuth{httpClient: &http.Client{Transport: refreshRoundTripperFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusBadRequest,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(responseBody)),
		}, nil
	})}}

	_, errRefresh := svc.RefreshTokens(context.Background(), "test-refresh-token")
	if errRefresh == nil {
		t.Fatal("RefreshTokens() error = nil, want provider response error")
	}
	if got := errRefresh.Error(); got != responseBody {
		t.Fatalf("RefreshTokens() error = %q, want exact body %q", got, responseBody)
	}
}
