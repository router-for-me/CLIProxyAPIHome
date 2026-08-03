package kimi

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestKimiRefreshResponseErrorRequiresExplicitTerminalSignal(t *testing.T) {
	t.Parallel()

	generic := kimiRefreshResponseError(http.StatusUnauthorized, []byte(`{"error":"unauthorized","message":"proxy authentication required","access_token":"response-secret"}`))
	if errors.Is(generic, ErrRefreshTokenRejected) {
		t.Fatalf("generic HTTP 401 was classified as terminal: %v", generic)
	}
	if strings.Contains(generic.Error(), "response-secret") {
		t.Fatalf("generic refresh error leaked response body: %v", generic)
	}

	terminal := kimiRefreshResponseError(http.StatusBadRequest, []byte(`{"error":"invalid_grant","error_description":"refresh token revoked","refresh_token":"response-secret"}`))
	if !errors.Is(terminal, ErrRefreshTokenRejected) {
		t.Fatalf("invalid_grant was not classified as terminal: %v", terminal)
	}
	if strings.Contains(terminal.Error(), "response-secret") || strings.Contains(terminal.Error(), "invalid_grant") {
		t.Fatalf("terminal refresh error leaked response body: %v", terminal)
	}
}
