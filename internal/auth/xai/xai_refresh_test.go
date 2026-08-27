package xai

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
				"error":"temporarily_unavailable",
				"code":"rate_limited",
				"error_description":"Try again later"
			}`
	svc := &XAIAuth{httpClient: &http.Client{Transport: refreshRoundTripperFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(responseBody)),
		}, nil
	})}}

	_, errRefresh := svc.RefreshTokens(context.Background(), "test-refresh-token", "https://auth.x.ai/oauth/token")
	if errRefresh == nil {
		t.Fatal("RefreshTokens() error = nil, want provider response error")
	}
	if got := errRefresh.Error(); got != responseBody {
		t.Fatalf("RefreshTokens() error = %q, want exact body %q", got, responseBody)
	}
}
