package codex

import (
	"net/http"
	"testing"
)

func TestCodexRefreshResponseErrorClassifiesAndPreservesUpstreamBody(t *testing.T) {
	t.Parallel()

	terminalBody := `{"error":"invalid_grant","error_description":"refresh token revoked"}`
	terminal := newCodexRefreshResponseError(http.StatusBadRequest, []byte(terminalBody))
	if !isNonRetryableRefreshErr(terminal) {
		t.Fatalf("invalid_grant error was retryable: %v", terminal)
	}
	if got := terminal.Error(); got != terminalBody {
		t.Fatalf("terminal refresh error = %q, want exact body %q", got, terminalBody)
	}

	genericBody := `{"error":"unauthorized","message":"proxy authentication required"}`
	generic := newCodexRefreshResponseError(http.StatusUnauthorized, []byte(genericBody))
	if isNonRetryableRefreshErr(generic) {
		t.Fatalf("generic HTTP 401 was classified as terminal: %v", generic)
	}
	if got := generic.Error(); got != genericBody {
		t.Fatalf("generic refresh error = %q, want exact body %q", got, genericBody)
	}
}
