package codex

import (
	"net/http"
	"strings"
	"testing"
)

func TestCodexRefreshResponseErrorClassifiesAndRedactsTerminalSignals(t *testing.T) {
	t.Parallel()

	terminal := newCodexRefreshResponseError(http.StatusBadRequest, []byte(`{"error":"invalid_grant","error_description":"refresh token revoked","refresh_token":"provider-secret"}`))
	if !isNonRetryableRefreshErr(terminal) {
		t.Fatalf("invalid_grant error was retryable: %v", terminal)
	}
	if strings.Contains(terminal.Error(), "provider-secret") {
		t.Fatalf("terminal refresh error leaked provider body: %v", terminal)
	}

	generic := newCodexRefreshResponseError(http.StatusUnauthorized, []byte(`{"error":"unauthorized","message":"proxy authentication required","access_token":"provider-secret"}`))
	if isNonRetryableRefreshErr(generic) {
		t.Fatalf("generic HTTP 401 was classified as terminal: %v", generic)
	}
	if strings.Contains(generic.Error(), "provider-secret") {
		t.Fatalf("generic refresh error leaked provider body: %v", generic)
	}
}
