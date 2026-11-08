package codex

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
)

type retryRoundTripper func(*http.Request) (*http.Response, error)

func (f retryRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type failedRefreshBody struct{}

func (*failedRefreshBody) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func (*failedRefreshBody) Close() error             { return nil }

func TestRefreshTokensRetriesRecoverableFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"transport", 0, ""},
		{"read", http.StatusOK, ""},
		{"decode", http.StatusOK, `{"access_token":`},
		{"missing access token", http.StatusOK, `{"expires_in":3600}`},
		{"invalid access token", http.StatusOK, `{"access_token":123,"expires_in":3600}`},
		{"server error", http.StatusServiceUnavailable, `{"error":"server_error"}`},
		{"rate limit", http.StatusTooManyRequests, `{"error":"rate_limited"}`},
		{"generic unauthorized", http.StatusUnauthorized, `{"error":"gateway_unavailable"}`},
		{"redirect", http.StatusTemporaryRedirect, `{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				calls := 0
				auth := &CodexAuth{httpClient: &http.Client{Transport: retryRoundTripper(func(req *http.Request) (*http.Response, error) {
					calls++
					if req.URL.String() != TokenURL {
						t.Fatal("refresh credentials followed a redirect")
					}
					if errParse := req.ParseForm(); errParse != nil || req.Form.Get("refresh_token") != "original-refresh" {
						t.Fatal("retry did not reuse the current refresh credentials")
					}
					status, body := http.StatusOK, `{"access_token":"recovered","refresh_token":"rotated","expires_in":3600}`
					if calls == 1 {
						if tc.status == 0 {
							return nil, io.ErrUnexpectedEOF
						}
						status, body = tc.status, tc.body
					}
					var responseBody io.ReadCloser = io.NopCloser(strings.NewReader(body))
					if calls == 1 && tc.name == "read" {
						responseBody = &failedRefreshBody{}
					}
					return &http.Response{StatusCode: status, Request: req, Body: responseBody, Header: http.Header{"Location": {"https://example.invalid/replay"}}}, nil
				})}}
				data, errRefresh := auth.RefreshTokensWithRetry(context.Background(), "original-refresh", 3)
				if errRefresh != nil || data == nil || data.AccessToken != "recovered" || data.RefreshToken != "rotated" || calls != 2 {
					t.Fatalf("refresh did not recover: error=%v calls=%d", errRefresh, calls)
				}
			})
		})
	}
}

func TestRefreshTokensRetryExhaustionDoesNotFenceGrant(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		transport := retryRoundTripper(func(req *http.Request) (*http.Response, error) {
			calls++
			body := `{"expires_in":3600}`
			if calls > 3 {
				body = `{"access_token":"recovered","expires_in":3600}`
			}
			return &http.Response{StatusCode: http.StatusOK, Request: req, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
		})
		client := &CodexAuth{httpClient: &http.Client{Transport: transport}}
		if _, errRefresh := client.RefreshTokensWithRetry(context.Background(), "same-refresh", 3); errRefresh == nil || calls != 3 {
			t.Fatalf("retry limit not honored: error=%v calls=%d", errRefresh, calls)
		}
		client = &CodexAuth{httpClient: &http.Client{Transport: transport}}
		data, errRefresh := client.RefreshTokensWithRetry(context.Background(), "same-refresh", 3)
		if errRefresh != nil || data == nil || data.AccessToken != "recovered" || calls != 4 {
			t.Fatalf("later attempt remained fenced: error=%v calls=%d", errRefresh, calls)
		}
	})
}

func TestRefreshTokensStopsOnRejectedGrantOrCancellation(t *testing.T) {
	for _, code := range []string{"invalid_grant", "refresh_token_expired", "refresh_token_revoked", "refresh_token_reused"} {
		t.Run(code, func(t *testing.T) {
			calls := 0
			auth := &CodexAuth{httpClient: &http.Client{Transport: retryRoundTripper(func(req *http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: http.StatusUnauthorized, Request: req, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":"` + code + `"}`))}, nil
			})}}
			if _, errRefresh := auth.RefreshTokensWithRetry(context.Background(), "rejected", 3); errRefresh == nil || calls != 1 {
				t.Fatalf("grant rejection retried: error=%v calls=%d", errRefresh, calls)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	auth := &CodexAuth{httpClient: &http.Client{Transport: retryRoundTripper(func(*http.Request) (*http.Response, error) {
		t.Fatal("canceled refresh was submitted")
		return nil, nil
	})}}
	if _, errRefresh := auth.RefreshTokensWithRetry(ctx, "unused", 3); !errors.Is(errRefresh, context.Canceled) {
		t.Fatalf("cancellation lost: %v", errRefresh)
	}
}
