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

func TestTerminalRefreshClassificationRequiresExplicitOAuthSignal(t *testing.T) {
	t.Parallel()

	if isTerminalRefreshAuthError(&Error{Code: "unauthorized", Message: "refresh endpoint returned 401", HTTPStatus: http.StatusUnauthorized}) {
		t.Fatal("generic refresh HTTP 401 was classified as terminal")
	}
	if !isTerminalRefreshAuthError(&Error{Code: "invalid_grant", Message: "refresh token rejected"}) {
		t.Fatal("invalid_grant was not classified as terminal")
	}
}

func TestApplyRefreshFailureStateKeepsTransientFailuresRetryable(t *testing.T) {
	t.Parallel()

	for _, message := range []string{
		"oauth refresh failed with status 400",
		"oauth refresh failed with status 401",
		"proxy connection refused",
	} {
		t.Run(message, func(t *testing.T) {
			auth := &Auth{ID: "auth-1", Provider: "antigravity", Status: StatusActive}
			now := time.Now().UTC()
			applyRefreshFailureState(auth, errors.New(message), now)

			if auth.Disabled || auth.Status == StatusDisabled {
				t.Fatalf("transient refresh failure disabled auth: %#v", auth)
			}
			if !auth.Unavailable || auth.Status != StatusError {
				t.Fatalf("transient refresh state = %#v, want unavailable error", auth)
			}
			want := now.Add(refreshFailureBackoff)
			if !auth.NextRefreshAfter.Equal(want) || !auth.NextRetryAfter.Equal(want) {
				t.Fatalf("refresh/retry deadlines = %v/%v, want %v", auth.NextRefreshAfter, auth.NextRetryAfter, want)
			}
			if auth.LastError == nil || auth.LastError.Code != refreshTransientErrorCode || !auth.LastError.Retryable {
				t.Fatalf("LastError = %#v, want retryable transient refresh error", auth.LastError)
			}
		})
	}
}

func TestApplyRefreshSuccessStateClearsUnauthorizedCooldown(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	retryAt := now.Add(time.Minute)
	auth := &Auth{
		ID:               "auth-1",
		Provider:         "codex",
		Status:           StatusError,
		StatusMessage:    refreshTransientErrorMsg,
		Unavailable:      true,
		NextRetryAfter:   retryAt,
		NextRefreshAfter: retryAt,
		LastError:        &Error{Code: refreshTransientErrorCode, Message: refreshTransientErrorMsg, Retryable: true},
		ModelStates: map[string]*ModelState{
			"gpt-5": {
				Status:         StatusError,
				StatusMessage:  "unauthorized",
				Unavailable:    true,
				NextRetryAfter: retryAt,
				LastError:      &Error{HTTPStatus: http.StatusUnauthorized, Message: "expired"},
			},
		},
	}

	resumed := applyRefreshSuccessState(auth, now)

	if len(resumed) != 1 || resumed[0] != "gpt-5" {
		t.Fatalf("resumed models = %v, want [gpt-5]", resumed)
	}
	if auth.Disabled || auth.Unavailable || auth.Status != StatusActive || auth.LastError != nil {
		t.Fatalf("refreshed auth state = %#v, want active", auth)
	}
	if !auth.NextRefreshAfter.IsZero() || !auth.NextRetryAfter.IsZero() {
		t.Fatalf("refresh/retry deadlines = %v/%v, want zero", auth.NextRefreshAfter, auth.NextRetryAfter)
	}
	state := auth.ModelStates["gpt-5"]
	if state == nil || state.Unavailable || state.Status != StatusActive || state.LastError != nil {
		t.Fatalf("refreshed model state = %#v, want active", state)
	}
}
