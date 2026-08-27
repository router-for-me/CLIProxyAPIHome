package auth

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

type refreshTestRoundTripper func(*http.Request) (*http.Response, error)

func (f refreshTestRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestAntigravityOAuthRefreshErrorClassification(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name         string
		body         string
		wantTerminal bool
	}{
		{
			name:         "invalid grant",
			body:         `{"error":"invalid_grant","error_description":"Bad Request"}`,
			wantTerminal: true,
		},
		{
			name:         "expired description",
			body:         `{"error":"bad_request","error_description":"Token has been expired or revoked."}`,
			wantTerminal: true,
		},
		{
			name:         "revoked refresh token description",
			body:         `{"error":"bad_request","error_description":"The refresh_token has been revoked."}`,
			wantTerminal: true,
		},
		{
			name:         "other bad request",
			body:         `{"error":"invalid_request","error_description":"Malformed request"}`,
			wantTerminal: false,
		},
		{
			name:         "terminal text in unrelated field",
			body:         `{"error":"invalid_request","error_description":"Malformed request","debug":"invalid_grant"}`,
			wantTerminal: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errRefresh := antigravityOAuthRefreshError(http.StatusBadRequest, []byte(tc.body))
			if got := isTerminalRefreshAuthError(errRefresh); got != tc.wantTerminal {
				t.Fatalf("isTerminalRefreshAuthError() = %v, want %v; err=%v", got, tc.wantTerminal, errRefresh)
			}
			if got := errRefresh.Error(); got != tc.body {
				t.Fatalf("refresh error = %q, want exact upstream body %q", got, tc.body)
			}
			if tc.wantTerminal {
				var authErr *Error
				if !errors.As(errRefresh, &authErr) {
					t.Fatalf("refresh error type = %T, want *Error", errRefresh)
				}
				if authErr.Code != refreshAuthErrorCode || authErr.HTTPStatus != http.StatusUnauthorized {
					t.Fatalf("auth error = %#v, want terminal authentication error", authErr)
				}
			} else {
				statusErr, ok := errRefresh.(interface{ StatusCode() int })
				if !ok || statusErr.StatusCode() != http.StatusServiceUnavailable {
					t.Fatalf("refresh error status = %T/%v, want 503", errRefresh, errRefresh)
				}
			}
		})
	}
}

func TestRefreshAntigravityPreservesTransportAndReadErrors(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		cause     error
		transport refreshTestRoundTripper
	}{
		{
			name:  "socks transport",
			cause: errors.New("socks connect tcp 10.0.0.8:1080: connection refused"),
			transport: func(_ *http.Request) (*http.Response, error) {
				return nil, errors.New("socks connect tcp 10.0.0.8:1080: connection refused")
			},
		},
		{
			name:  "response read",
			cause: errors.New("oauth response stream reset"),
			transport: func(_ *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       io.NopCloser(iotest.ErrReader(errors.New("oauth response stream reset"))),
				}, nil
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			auth := &Auth{Provider: "antigravity", Metadata: map[string]any{"refresh_token": "refresh-token"}}
			_, errRefresh := refreshAntigravity(context.Background(), nil, auth, tc.transport)
			if errRefresh == nil {
				t.Fatal("refreshAntigravity() error = nil")
			}
			if !strings.HasPrefix(errRefresh.Error(), "antigravity refresh: ") || !strings.Contains(errRefresh.Error(), tc.cause.Error()) {
				t.Fatalf("refreshAntigravity() error = %q, want provider prefix and %q", errRefresh, tc.cause)
			}
			var providerErr *providerRefreshError
			if !errors.As(errRefresh, &providerErr) || providerErr.StatusCode() != http.StatusServiceUnavailable {
				t.Fatalf("refreshAntigravity() error = %T/%v, want provider 503", errRefresh, errRefresh)
			}

			applyRefreshFailureState(auth, errRefresh, time.Now().UTC())
			if auth.LastError == nil || !strings.Contains(auth.LastError.Message, tc.cause.Error()) {
				t.Fatalf("persisted refresh error = %#v, want original cause", auth.LastError)
			}
		})
	}
}

func TestBuiltInRefreshErrorLeavesContextCancellationUnchanged(t *testing.T) {
	t.Parallel()

	errRefresh := builtInRefreshError("codex", context.Canceled)
	if !errors.Is(errRefresh, context.Canceled) {
		t.Fatalf("builtInRefreshError() = %v, want context cancellation", errRefresh)
	}
	var providerErr *providerRefreshError
	if errors.As(errRefresh, &providerErr) {
		t.Fatalf("context cancellation was exposed as provider failure: %v", errRefresh)
	}
}

func TestApplyRefreshFailureStateUsesGenericTerminalErrorForUnstructuredFailure(t *testing.T) {
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
		t.Fatalf("LastError included unstructured provider detail: %v", auth.LastError)
	}
	if !auth.NextRefreshAfter.IsZero() {
		t.Fatalf("NextRefreshAfter = %v, want zero", auth.NextRefreshAfter)
	}
}

func TestApplyRefreshFailureStatePreservesTerminalUpstreamResponse(t *testing.T) {
	t.Parallel()

	auth := &Auth{ID: "auth-1", Provider: "antigravity", Status: StatusActive}
	now := time.Now().UTC()
	body := []byte(`{
		"error":"invalid_grant",
		"error_description":"Refresh token expired"
	}`)
	errRefresh := antigravityOAuthRefreshError(http.StatusBadRequest, body)

	applyRefreshFailureState(auth, errRefresh, now)

	if !auth.Disabled || auth.LastError == nil || auth.LastError.Code != refreshAuthErrorCode {
		t.Fatalf("terminal refresh state = %#v, want disabled auth", auth)
	}
	if auth.LastError.Upstream == nil || auth.LastError.Upstream.Status != http.StatusBadRequest || string(auth.LastError.Upstream.Body) != string(body) {
		t.Fatalf("terminal upstream response = %#v, want 400/%q", auth.LastError.Upstream, body)
	}
	if got := auth.LastError.Error(); got != string(body) {
		t.Fatalf("terminal refresh error = %q, want exact upstream body %q", got, body)
	}
}

func TestRefreshNowObservedReturnsStoredTerminalUpstreamResponse(t *testing.T) {
	t.Parallel()

	const responseBody = "first line\r\nsecond line\n"
	manager := NewManager(nil, nil, nil)
	auth := &Auth{
		ID:          "auth-terminal-upstream",
		Index:       "auth-terminal-upstream",
		Provider:    "antigravity",
		Status:      StatusDisabled,
		Disabled:    true,
		Unavailable: true,
		LastError: &Error{
			Code:       refreshAuthErrorCode,
			Message:    refreshAuthErrorMsg,
			HTTPStatus: http.StatusUnauthorized,
			Upstream: &UpstreamResponse{
				Status: http.StatusBadRequest,
				Body:   []byte(responseBody),
			},
		},
	}
	if _, errRegister := manager.Register(WithSkipPersist(context.Background()), auth); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}

	_, errRefresh := manager.RefreshNowObserved(context.Background(), auth.ID, "")
	var authErr *Error
	if !errors.As(errRefresh, &authErr) || authErr.Upstream == nil {
		t.Fatalf("RefreshNowObserved() error = %#v, want stored upstream response", errRefresh)
	}
	if authErr.Upstream.Status != http.StatusBadRequest || errRefresh.Error() != responseBody {
		t.Fatalf("stored upstream response = %#v, error=%q", authErr.Upstream, errRefresh.Error())
	}
}

func TestTerminalRefreshClassificationRequiresExplicitOAuthSignal(t *testing.T) {
	t.Parallel()

	if isTerminalRefreshAuthError(&Error{Code: "unauthorized", Message: "refresh endpoint returned 401", HTTPStatus: http.StatusUnauthorized}) {
		t.Fatal("generic refresh HTTP 401 was classified as terminal")
	}
	if isTerminalRefreshAuthError(&Error{Code: "authentication_error", Message: "proxy authentication failed", HTTPStatus: http.StatusUnauthorized}) {
		t.Fatal("generic proxy authentication error was classified as terminal")
	}
	if !isTerminalRefreshAuthError(&Error{Code: "invalid_grant", Message: "refresh token rejected"}) {
		t.Fatal("invalid_grant was not classified as terminal")
	}
}

func TestBackgroundRefreshUsesConfiguredHandler(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	auth := &Auth{
		ID:       "auth-auto-handler",
		Index:    "auth-auto-handler",
		Provider: "codex",
		Status:   StatusActive,
		Metadata: map[string]any{
			"access_token":  "access",
			"refresh_token": "refresh",
			"expires_at":    time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano),
		},
	}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	calls := 0
	manager.SetAutoRefreshHandler(func(_ context.Context, selected *Auth) error {
		calls++
		if selected == nil || selected.ID != auth.ID {
			t.Fatalf("selected auth = %#v, want %s", selected, auth.ID)
		}
		return nil
	})

	manager.refreshAuth(context.Background(), auth.ID)

	if calls != 1 {
		t.Fatalf("auto refresh handler calls = %d, want 1", calls)
	}
}

func TestBackgroundRefreshLogsProviderError(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	auth := &Auth{
		ID:       "auth-auto-provider-error",
		Index:    "auth-auto-provider-error",
		Provider: "antigravity",
		Status:   StatusActive,
		Metadata: map[string]any{
			"access_token":  "access",
			"refresh_token": "refresh",
			"expires_at":    time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano),
		},
	}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	const providerMessage = `antigravity refresh: upstream request failed with status 400 error="invalid_request" request_id="req-123"`
	manager.SetAutoRefreshHandler(func(context.Context, *Auth) error {
		return errors.New(providerMessage)
	})

	savedHooks := make(log.LevelHooks)
	for level, hooks := range log.StandardLogger().Hooks {
		savedHooks[level] = append([]log.Hook(nil), hooks...)
	}
	hook := logtest.NewGlobal()
	t.Cleanup(func() {
		log.StandardLogger().ReplaceHooks(savedHooks)
	})

	manager.refreshAuth(context.Background(), auth.ID)

	for _, entry := range hook.AllEntries() {
		if entry.Level == log.WarnLevel && strings.Contains(entry.Message, providerMessage) {
			return
		}
	}
	t.Fatalf("provider refresh error was not logged: %#v", hook.AllEntries())
}

func TestApplyRefreshPendingStateDoesNotBlockDispatch(t *testing.T) {
	now := time.Now().UTC()
	auth := &Auth{ID: "auth-1", Provider: "codex", Status: StatusActive}

	ApplyRefreshPendingState(auth, now)

	if auth.Disabled || auth.Unavailable || auth.Status != StatusActive || !auth.NextRetryAfter.IsZero() {
		t.Fatalf("refresh pending blocked credential dispatch: %#v", auth)
	}
	if !RefreshRetryBackoffOpen(auth, now) {
		t.Fatal("refresh pending did not open refresh-only backoff")
	}
	if blocked, _, _ := isAuthBlockedForModel(auth, "gpt-5", now); blocked {
		t.Fatal("refresh pending blocked a model using the still-valid access token")
	}
}

func TestUnsupportedRefreshBackoffDoesNotBlockDispatch(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	retryAt := now.Add(5 * time.Minute)
	auth := &Auth{
		ID:                    "auth-refresh-unsupported",
		Provider:              "custom",
		Status:                StatusError,
		Unavailable:           true,
		RuntimeRefreshBlocked: true,
		NextRetryAfter:        retryAt,
		LastError:             newTransientRefreshError(),
		StatusMessage:         refreshTransientErrorMsg,
		ModelStates: map[string]*ModelState{
			"blocked-model": {
				Status:         StatusError,
				Unavailable:    true,
				NextRetryAfter: retryAt,
				LastError:      &Error{Message: "model cooldown", HTTPStatus: http.StatusServiceUnavailable},
			},
		},
	}
	ApplyUnsupportedRefreshBackoff(auth, now)

	if !RefreshRetryBackoffOpen(auth, now) {
		t.Fatal("unsupported refresh did not open refresh-only backoff")
	}
	if RefreshBlocksDispatch(auth) {
		t.Fatal("unsupported refresh backoff was promoted to a credential-level dispatch block")
	}
	if auth.LastError == nil || auth.LastError.Code != refreshUnsupportedCode || auth.StatusMessage != "" {
		t.Fatalf("unsupported refresh state = last error %#v status message %q", auth.LastError, auth.StatusMessage)
	}
	if blocked, _, _ := isAuthBlockedForModel(auth, "blocked-model", now); !blocked {
		t.Fatal("existing model cooldown was cleared unexpectedly")
	}
	if blocked, _, _ := isAuthBlockedForModel(auth, "new-model", now); blocked {
		t.Fatal("unsupported refresh backoff blocked an otherwise dispatchable model")
	}

	NewManager(nil, nil, nil).applyResultTransition(auth, Result{AuthID: auth.ID, Model: "new-model", Success: true}, "new-model", now, false)
	if auth.Unavailable || auth.RuntimeRefreshBlocked || RefreshBlocksDispatch(auth) {
		t.Fatalf("successful result preserved an unsupported refresh as a dispatch block: %#v", auth)
	}
}

func TestApplyRefreshFailureStateBlocksTransientFailuresFromDispatch(t *testing.T) {
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
				t.Fatalf("transient refresh failure did not block auth: %#v", auth)
			}
			want := now.Add(refreshFailureBackoff)
			if !auth.NextRefreshAfter.Equal(want) || !auth.NextRetryAfter.Equal(want) {
				t.Fatalf("refresh/retry deadlines = %v/%v, want %v", auth.NextRefreshAfter, auth.NextRetryAfter, want)
			}
			if auth.LastError == nil || auth.LastError.Code != refreshTransientErrorCode || !auth.LastError.Retryable {
				t.Fatalf("LastError = %#v, want retryable transient refresh error", auth.LastError)
			}
			if auth.LastError.Message != refreshTransientErrorMsg || auth.StatusMessage != refreshTransientErrorMsg {
				t.Fatalf("refresh error messages = %q/%q, want generic default", auth.LastError.Message, auth.StatusMessage)
			}
			for _, checkAt := range []time.Time{now, want.Add(time.Second)} {
				if blocked, _, _ := isAuthBlockedForModel(auth, "gemini-3.7-flash-high", checkAt); !blocked {
					t.Fatalf("refresh-failed auth became dispatchable at %v", checkAt)
				}
			}
		})
	}
}

func TestApplyRefreshFailureStatePreservesTransientUpstreamResponse(t *testing.T) {
	t.Parallel()

	auth := &Auth{ID: "auth-1", Provider: "antigravity", Status: StatusActive}
	now := time.Now().UTC()
	body := []byte("first line\r\nsecond line\n")
	errRefresh := antigravityOAuthRefreshError(http.StatusBadRequest, body)

	applyRefreshFailureState(auth, errRefresh, now)

	if auth.LastError == nil || auth.LastError.Message != refreshTransientErrorMsg || auth.StatusMessage != refreshTransientErrorMsg {
		t.Fatalf("refresh failure state = %#v, want generic scheduling message", auth)
	}
	if auth.LastError.Upstream == nil || auth.LastError.Upstream.Status != http.StatusBadRequest || string(auth.LastError.Upstream.Body) != string(body) {
		t.Fatalf("transient upstream response = %#v, want 400/%q", auth.LastError.Upstream, body)
	}
	if got := auth.LastError.Error(); got != string(body) {
		t.Fatalf("refresh error = %q, want exact upstream body %q", got, body)
	}
}

func TestApplyResultTransitionPreservesRefreshAcquisitionState(t *testing.T) {
	t.Parallel()

	const providerMessage = `antigravity refresh: upstream request failed with status 400 error="invalid_request" request_id="req-123"`
	for _, test := range []struct {
		name   string
		result Result
	}{
		{name: "success", result: Result{AuthID: "auth-1", Model: "gemini-3.7-flash-high", Success: true}},
		{name: "failure", result: Result{AuthID: "auth-1", Model: "gemini-3.7-flash-high", Error: &Error{Message: "upstream unavailable", HTTPStatus: http.StatusServiceUnavailable}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Now().UTC()
			retryAt := now.Add(refreshFailureBackoff)
			auth := &Auth{
				ID:               "auth-1",
				Provider:         "antigravity",
				Status:           StatusError,
				StatusMessage:    providerMessage,
				Unavailable:      true,
				NextRefreshAfter: retryAt,
				NextRetryAfter:   retryAt,
				LastError: &Error{
					Code:       refreshTransientErrorCode,
					Message:    providerMessage,
					Retryable:  true,
					HTTPStatus: http.StatusServiceUnavailable,
				},
			}

			NewManager(nil, nil, nil).applyResultTransition(auth, test.result, test.result.Model, now, false)

			if auth.Status != StatusError || !auth.Unavailable || auth.StatusMessage != providerMessage || auth.LastError == nil || auth.LastError.Code != refreshTransientErrorCode || auth.LastError.Message != providerMessage || !auth.NextRetryAfter.Equal(retryAt) {
				t.Fatalf("result replaced refresh acquisition state: %#v", auth)
			}
			if blocked, _, _ := isAuthBlockedForModel(auth, test.result.Model, retryAt.Add(time.Hour)); !blocked {
				t.Fatal("refresh-failed auth became dispatchable before a successful refresh")
			}

			applyRefreshSuccessState(auth, retryAt)
			if blocked, _, _ := isAuthBlockedForModel(auth, test.result.Model, retryAt); blocked {
				t.Fatalf("successful refresh did not restore dispatch: %#v", auth)
			}
		})
	}
}

func TestRuntimeRefreshBlockPreventsDispatchAndClearsAfterRefresh(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	auth := &Auth{
		ID:                    "auth-minimal-refresh-block",
		Provider:              "antigravity",
		Status:                StatusError,
		Unavailable:           true,
		RuntimeRefreshBlocked: true,
		NextRetryAfter:        now.Add(5 * time.Minute),
	}
	if blocked, _, _ := isAuthBlockedForModel(auth, "gemini-3.7-flash-high", now); !blocked {
		t.Fatal("minimal refresh-block projection was dispatchable")
	}

	applyRefreshSuccessState(auth, now)
	if auth.Unavailable || auth.RuntimeRefreshBlocked || auth.Status != StatusActive {
		t.Fatalf("successful refresh left runtime block behind: %#v", auth)
	}
}

func TestApplyRefreshSuccessStateClearsUnauthorizedModelCooldown(t *testing.T) {
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
