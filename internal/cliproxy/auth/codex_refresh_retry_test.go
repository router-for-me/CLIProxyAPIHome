package auth

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	codexauth "github.com/router-for-me/CLIProxyAPIHome/internal/auth/codex"
)

func TestCodexRefreshFailureRemainsRetryable(t *testing.T) {
	now := time.Now()
	auth := &Auth{Provider: "codex", Status: StatusActive, Metadata: map[string]any{"refresh_token": "same-refresh"}}
	failure := ApplyRefreshFailureState(auth, io.ErrUnexpectedEOF, now)
	if failure == nil || failure.Code != refreshTransientErrorCode || !failure.Retryable || auth.Disabled || !auth.NextRefreshAfter.After(now) {
		t.Fatal("temporary refresh failure became permanent")
	}
	if _, exists := auth.Metadata[codexauth.RefreshOutcomeMetadataKey]; exists {
		t.Fatal("temporary failure created a persistent grant fence")
	}
}

func TestCodexRefreshIgnoresLegacyOutcomeAndRetainsCurrentGrant(t *testing.T) {
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	calls := 0
	http.DefaultTransport = refreshTestRoundTripper(func(req *http.Request) (*http.Response, error) {
		calls++
		if errParse := req.ParseForm(); errParse != nil || req.Form.Get("refresh_token") != "same-refresh" {
			t.Fatal("legacy credential was replaced before retrying")
		}
		return &http.Response{StatusCode: http.StatusOK, Request: req, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"access_token":"recovered","expires_in":3600}`))}, nil
	})
	auth := &Auth{Provider: "codex", Status: StatusActive, Metadata: map[string]any{
		"access_token": "previous", "refresh_token": "same-refresh",
		codexauth.RefreshOutcomeMetadataKey: map[string]any{"outer": map[string]any{"grant_sha256": "legacy", "stage": "response_read"}},
	}}
	updated, errRefresh := refreshCredential(context.Background(), nil, auth, nil)
	if errRefresh != nil || calls != 1 || updated == nil || updated.Metadata["access_token"] != "recovered" || updated.Metadata["refresh_token"] != "same-refresh" {
		t.Fatalf("legacy outcome blocked recovery: error=%v calls=%d", errRefresh, calls)
	}
	if _, exists := updated.Metadata[codexauth.RefreshOutcomeMetadataKey]; exists {
		t.Fatal("successful refresh retained obsolete grant diagnostics")
	}
}
