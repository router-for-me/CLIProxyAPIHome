package auth

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestAntigravityOAuthRefreshErrorClassification(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name         string
		body         string
		wantTerminal bool
	}{
		{
			name:         "invalid grant",
			body:         `{"error":"invalid_grant","error_description":"Bad Request","access_token":"response-secret"}`,
			wantTerminal: true,
		},
		{
			name:         "expired description",
			body:         `{"error":"bad_request","error_description":"Token has been expired or revoked.","refresh_token":"response-secret"}`,
			wantTerminal: true,
		},
		{
			name:         "revoked refresh token description",
			body:         `{"error":"bad_request","error_description":"The refresh_token has been revoked.","refresh_token":"response-secret"}`,
			wantTerminal: true,
		},
		{
			name:         "other bad request",
			body:         `{"error":"invalid_request","error_description":"Malformed request","access_token":"response-secret"}`,
			wantTerminal: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errRefresh := antigravityOAuthRefreshError(http.StatusBadRequest, []byte(tc.body))
			if got := isTerminalRefreshAuthError(errRefresh); got != tc.wantTerminal {
				t.Fatalf("isTerminalRefreshAuthError() = %v, want %v; err=%v", got, tc.wantTerminal, errRefresh)
			}
			if strings.Contains(errRefresh.Error(), "response-secret") {
				t.Fatalf("refresh error leaked response body: %v", errRefresh)
			}
			if tc.wantTerminal {
				var authErr *Error
				if !errors.As(errRefresh, &authErr) {
					t.Fatalf("refresh error type = %T, want *Error", errRefresh)
				}
				if authErr.Code != refreshAuthErrorCode || authErr.HTTPStatus != http.StatusUnauthorized {
					t.Fatalf("auth error = %#v, want terminal authentication error", authErr)
				}
			}
		})
	}
}

func TestApplyRefreshFailureStateRedactsTerminalProviderError(t *testing.T) {
	t.Parallel()

	auth := &Auth{
		ID:       "auth-1",
		Provider: "antigravity",
		Status:   StatusActive,
	}
	now := time.Now().UTC()
	providerErr := errors.New(`oauth refresh failed: invalid_grant refresh_token=provider-secret`)

	applyRefreshFailureState(auth, providerErr, now)

	if !auth.Disabled || !auth.Unavailable || auth.Status != StatusDisabled {
		t.Fatalf("terminal auth state = %#v, want disabled and unavailable", auth)
	}
	if auth.LastError == nil || auth.LastError.Code != refreshAuthErrorCode || auth.LastError.HTTPStatus != http.StatusUnauthorized {
		t.Fatalf("LastError = %#v, want structured authentication error", auth.LastError)
	}
	if strings.Contains(auth.LastError.Error(), "provider-secret") || strings.Contains(auth.LastError.Error(), "invalid_grant") {
		t.Fatalf("LastError leaked provider response: %v", auth.LastError)
	}
	if !auth.NextRefreshAfter.IsZero() {
		t.Fatalf("NextRefreshAfter = %v, want zero", auth.NextRefreshAfter)
	}
}

func TestApplyRefreshFailureStateKeepsOtherBadRequestsRetryable(t *testing.T) {
	t.Parallel()

	auth := &Auth{ID: "auth-1", Provider: "antigravity", Status: StatusActive}
	now := time.Now().UTC()
	applyRefreshFailureState(auth, errors.New("oauth refresh failed with status 400"), now)

	if auth.Disabled || auth.Status == StatusDisabled {
		t.Fatalf("ordinary HTTP 400 disabled auth: %#v", auth)
	}
	if want := now.Add(refreshFailureBackoff); !auth.NextRefreshAfter.Equal(want) {
		t.Fatalf("NextRefreshAfter = %v, want %v", auth.NextRefreshAfter, want)
	}
}
