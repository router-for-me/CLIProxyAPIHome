package kimi

import (
	"errors"
	"net/http"
	"testing"
)

func TestKimiRefreshResponseErrorRequiresExplicitTerminalSignal(t *testing.T) {
	t.Parallel()

	genericBody := `{"error":"unauthorized","message":"proxy authentication required"}`
	generic := kimiRefreshResponseError(http.StatusUnauthorized, []byte(genericBody))
	if errors.Is(generic, ErrRefreshTokenRejected) {
		t.Fatalf("generic HTTP 401 was classified as terminal: %v", generic)
	}
	if got := generic.Error(); got != genericBody {
		t.Fatalf("generic refresh error = %q, want exact body %q", got, genericBody)
	}

	terminalBody := `{"error":"invalid_grant","error_description":"refresh token revoked"}`
	terminal := kimiRefreshResponseError(http.StatusBadRequest, []byte(terminalBody))
	if !errors.Is(terminal, ErrRefreshTokenRejected) {
		t.Fatalf("invalid_grant was not classified as terminal: %v", terminal)
	}
	if got := terminal.Error(); got != terminalBody {
		t.Fatalf("terminal refresh error = %q, want exact body %q", got, terminalBody)
	}
}
