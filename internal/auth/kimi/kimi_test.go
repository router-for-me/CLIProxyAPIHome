package kimi

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestKimiRefreshResponseErrorClassification(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name         string
		status       int
		body         string
		wantTerminal bool
	}{
		{name: "generic unauthorized", status: http.StatusUnauthorized, body: `{}`},
		{name: "generic forbidden", status: http.StatusForbidden, body: `{}`},
		{name: "invalid grant", status: http.StatusBadRequest, body: `{"error":"invalid_grant","refresh_token":"secret"}`, wantTerminal: true},
		{name: "explicit unauthorized rejection", status: http.StatusUnauthorized, body: `{"error":"unauthorized","error_description":"refresh token rejected"}`, wantTerminal: true},
		{name: "other bad request", status: http.StatusBadRequest, body: `{"error":"invalid_request","refresh_token":"secret"}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errRefresh := kimiRefreshResponseError(tc.status, []byte(tc.body))
			if got := errors.Is(errRefresh, ErrRefreshTokenRejected); got != tc.wantTerminal {
				t.Fatalf("errors.Is(ErrRefreshTokenRejected) = %v, want %v; err=%v", got, tc.wantTerminal, errRefresh)
			}
			if strings.Contains(errRefresh.Error(), "secret") {
				t.Fatalf("refresh error leaked response body: %v", errRefresh)
			}
		})
	}
}
