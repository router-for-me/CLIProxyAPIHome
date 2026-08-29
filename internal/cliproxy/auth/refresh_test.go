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

	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

type refreshTestRoundTripper func(*http.Request) (*http.Response, error)

func (f refreshTestRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type refreshTestPluginRefresher func(context.Context, *Auth) (*Auth, bool, error)

func (f refreshTestPluginRefresher) RefreshAuth(ctx context.Context, auth *Auth) (*Auth, bool, error) {
	return f(ctx, auth)
}

type refreshTestRoundTripperProvider struct {
	transport http.RoundTripper
}

func (p refreshTestRoundTripperProvider) RoundTripperFor(*Auth) http.RoundTripper {
	return p.transport
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
		name        string
		stage       string
		wantSignals []string
		transport   refreshTestRoundTripper
	}{
		{
			name:        "socks transport",
			stage:       "transport",
			wantSignals: []string{"proxy=socks", "connection_refused"},
			transport: func(_ *http.Request) (*http.Response, error) {
				return nil, errors.New("socks connect tcp 10.0.0.8:1080: connection refused")
			},
		},
		{
			name:        "transport EOF",
			stage:       "transport",
			wantSignals: []string{"EOF"},
			transport: func(_ *http.Request) (*http.Response, error) {
				return nil, io.EOF
			},
		},
		{
			name:        "response read",
			stage:       "response_read",
			wantSignals: []string{"stream_reset"},
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
			if !strings.HasPrefix(errRefresh.Error(), "antigravity refresh failed: stage="+tc.stage) {
				t.Fatalf("refreshAntigravity() error = %q, want stage %q", errRefresh, tc.stage)
			}
			var providerErr *providerRefreshError
			if !errors.As(errRefresh, &providerErr) || providerErr.StatusCode() != http.StatusServiceUnavailable {
				t.Fatalf("refreshAntigravity() error = %T/%v, want provider 503", errRefresh, errRefresh)
			}

			ApplyRefreshFailureState(auth, errRefresh, time.Now().UTC())
			if auth.LastError == nil || auth.LastError.Message != refreshTransientErrorMsg || !strings.Contains(auth.LastError.Diagnostic, "stage="+tc.stage) || !strings.Contains(auth.LastError.Diagnostic, "retry_at=") {
				t.Fatalf("persisted refresh error = %#v, want generic message and safe stage diagnostic", auth.LastError)
			}
			for _, signal := range tc.wantSignals {
				if !strings.Contains(errRefresh.Error(), signal) || !strings.Contains(auth.LastError.Diagnostic, signal) {
					t.Fatalf("refresh diagnostics = error %q persisted %q, want %q", errRefresh, auth.LastError.Diagnostic, signal)
				}
			}
		})
	}
}

func TestApplyRefreshFailureStateRedactsPersistedTransportDiagnostic(t *testing.T) {
	t.Parallel()

	errRefresh := builtInRefreshError(
		"antigravity",
		"transport",
		errors.New(`Post "https://proxy-user:proxy-password@oauth.example/token?access_token=query-secret": access token: access-secret connection refused`),
	)
	auth := &Auth{ID: "auth-1", Provider: "antigravity", Status: StatusActive}
	ApplyRefreshFailureState(auth, errRefresh, time.Now().UTC())

	if auth.LastError == nil || !strings.Contains(auth.LastError.Diagnostic, "stage=transport") || !strings.Contains(auth.LastError.Diagnostic, "connection_refused") {
		t.Fatalf("persisted diagnostic = %#v, want transport failure detail", auth.LastError)
	}
	for _, secret := range []string{"proxy-user", "proxy-password", "query-secret", "access-secret"} {
		if strings.Contains(auth.LastError.Diagnostic, secret) {
			t.Fatalf("persisted diagnostic leaked %q: %q", secret, auth.LastError.Diagnostic)
		}
	}
	if auth.LastError.Message != refreshTransientErrorMsg || strings.Contains(auth.LastError.Error(), "connection_refused") {
		t.Fatalf("client-facing refresh error = %q, want generic message", auth.LastError.Error())
	}
}

func TestApplyRefreshFailureStateDoesNotPersistUnknownErrorText(t *testing.T) {
	t.Parallel()

	auth := &Auth{ID: "auth-1", Provider: "antigravity", Status: StatusActive}
	ApplyRefreshFailureState(auth, errors.New("provider failed with unlabeled-secret"), time.Now().UTC())

	if auth.LastError == nil || !strings.Contains(auth.LastError.Diagnostic, "error_type=") {
		t.Fatalf("persisted diagnostic = %#v, want safe error type", auth.LastError)
	}
	if strings.Contains(auth.LastError.Diagnostic, "unlabeled-secret") || strings.Contains(auth.LastError.Diagnostic, "provider failed") {
		t.Fatalf("persisted diagnostic retained arbitrary error text: %q", auth.LastError.Diagnostic)
	}
}

func TestBuiltInRefreshErrorLeavesContextCancellationUnchanged(t *testing.T) {
	t.Parallel()

	errRefresh := builtInRefreshError("codex", "provider_refresh", context.Canceled)
	if !errors.Is(errRefresh, context.Canceled) {
		t.Fatalf("builtInRefreshError() = %v, want context cancellation", errRefresh)
	}
	var providerErr *providerRefreshError
	if errors.As(errRefresh, &providerErr) {
		t.Fatalf("context cancellation was exposed as provider failure: %v", errRefresh)
	}
}

func TestBuiltInRefreshErrorKeepsInternalDeadlineDiagnostic(t *testing.T) {
	t.Parallel()

	errRefresh := builtInRefreshError("antigravity", "transport", context.DeadlineExceeded)
	if !errors.Is(errRefresh, context.DeadlineExceeded) {
		t.Fatalf("builtInRefreshError() = %v, want wrapped context deadline", errRefresh)
	}
	var providerErr *providerRefreshError
	if !errors.As(errRefresh, &providerErr) || !strings.Contains(providerErr.Diagnostic(), "stage=transport") || !strings.Contains(providerErr.Diagnostic(), "timeout") {
		t.Fatalf("deadline diagnostic = %T/%v, want provider transport stage", errRefresh, errRefresh)
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

	ApplyRefreshFailureState(auth, providerErr, now)

	if !auth.Disabled || !auth.Unavailable || auth.Status != StatusDisabled {
		t.Fatalf("terminal auth state = %#v, want disabled and unavailable", auth)
	}
	if auth.LastError == nil || auth.LastError.Code != refreshAuthErrorCode || auth.LastError.HTTPStatus != http.StatusUnauthorized {
		t.Fatalf("LastError = %#v, want structured authentication error", auth.LastError)
	}
	if strings.Contains(auth.LastError.Error(), "provider-secret") || strings.Contains(auth.LastError.Error(), "invalid_grant") {
		t.Fatalf("LastError included unstructured provider detail: %v", auth.LastError)
	}
	if !strings.Contains(auth.LastError.Diagnostic, "invalid_grant") || strings.Contains(auth.LastError.Diagnostic, "provider-secret") {
		t.Fatalf("LastError diagnostic = %q, want redacted terminal signal", auth.LastError.Diagnostic)
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

	ApplyRefreshFailureState(auth, errRefresh, now)

	if !auth.Disabled || auth.LastError == nil || auth.LastError.Code != refreshAuthErrorCode {
		t.Fatalf("terminal refresh state = %#v, want disabled auth", auth)
	}
	if auth.LastError.Upstream == nil || auth.LastError.Upstream.Status != http.StatusBadRequest || string(auth.LastError.Upstream.Body) != string(body) {
		t.Fatalf("terminal upstream response = %#v, want 400/%q", auth.LastError.Upstream, body)
	}
	if got := auth.LastError.Error(); got != string(body) {
		t.Fatalf("terminal refresh error = %q, want exact upstream body %q", got, body)
	}
	if !strings.Contains(auth.LastError.Diagnostic, "stage=upstream_response") || !strings.Contains(auth.LastError.Diagnostic, `error="invalid_grant"`) {
		t.Fatalf("terminal refresh diagnostic = %q, want response stage and OAuth code", auth.LastError.Diagnostic)
	}
}

func TestRefreshAuthCredentialPreservesPluginTerminalUpstreamResponse(t *testing.T) {
	t.Parallel()

	const responseBody = `{"error":"invalid_grant","error_description":"Plugin refresh token expired"}`
	body := []byte(responseBody)
	pluginErr := &Error{
		Code:       refreshAuthErrorCode,
		Message:    refreshAuthErrorMsg,
		Diagnostic: "plugin refresh failed: stage=upstream_response status=400",
		HTTPStatus: http.StatusUnauthorized,
		Upstream: &UpstreamResponse{
			Status: http.StatusBadRequest,
			Body:   body,
		},
	}
	manager := NewManager(nil, nil, nil)
	manager.SetPluginAuthRefresher(refreshTestPluginRefresher(func(context.Context, *Auth) (*Auth, bool, error) {
		return nil, true, pluginErr
	}))
	auth := &Auth{ID: "plugin-terminal-upstream", Index: "plugin-terminal-upstream", Provider: "plugin-refresh-test", Status: StatusActive}

	refreshed, errRefresh := manager.RefreshAuthCredential(context.Background(), auth)
	var authErr *Error
	if !errors.As(errRefresh, &authErr) || authErr == nil || authErr.Upstream == nil {
		t.Fatalf("RefreshAuthCredential() error = %#v, want structured upstream response", errRefresh)
	}
	if authErr.Upstream.Status != http.StatusBadRequest || string(authErr.Upstream.Body) != responseBody || errRefresh.Error() != responseBody {
		t.Fatalf("RefreshAuthCredential() upstream = %#v, error=%q, want 400/%q", authErr.Upstream, errRefresh.Error(), body)
	}
	if refreshed == nil || !refreshed.Disabled || refreshed.LastError == nil || refreshed.LastError.Upstream == nil || string(refreshed.LastError.Upstream.Body) != responseBody {
		t.Fatalf("RefreshAuthCredential() state = %#v, want disabled auth with upstream response", refreshed)
	}
	pluginErr.Upstream.Body[0] = 'X'
	if string(authErr.Upstream.Body) != responseBody || string(refreshed.LastError.Upstream.Body) != responseBody {
		t.Fatal("RefreshAuthCredential() retained plugin-owned upstream response storage")
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

func TestRefreshNowObservedReturnsStoredDiagnosticDuringBackoff(t *testing.T) {
	t.Parallel()

	const diagnostic = "antigravity refresh failed: stage=transport err=EOF"
	auth := &Auth{
		ID:                    "auth-transient-backoff",
		Index:                 "auth-transient-backoff",
		Provider:              "antigravity",
		Status:                StatusError,
		Unavailable:           true,
		RuntimeRefreshBlocked: true,
		NextRefreshAfter:      time.Now().UTC().Add(time.Minute),
		LastError: &Error{
			Code:       refreshTransientErrorCode,
			Message:    refreshTransientErrorMsg,
			Diagnostic: diagnostic,
			Retryable:  true,
			HTTPStatus: http.StatusServiceUnavailable,
		},
	}
	manager := NewManager(nil, nil, nil)
	if _, errRegister := manager.Register(WithSkipPersist(context.Background()), auth); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}

	_, errRefresh := manager.RefreshNowObserved(context.Background(), auth.ID, "")
	var authErr *Error
	if !errors.As(errRefresh, &authErr) || authErr.Diagnostic != diagnostic || authErr.Message != refreshTransientErrorMsg {
		t.Fatalf("RefreshNowObserved() error = %#v, want stored safe diagnostic", errRefresh)
	}
}

func TestRefreshNowObservedReturnsSafeMatchingTokenAfterTransientFailure(t *testing.T) {
	t.Parallel()

	const responseBody = `{"error":"invalid_request","error_description":"Malformed request"}`
	providerCalls := 0
	transport := refreshTestRoundTripper(func(*http.Request) (*http.Response, error) {
		providerCalls++
		return &http.Response{
			StatusCode: http.StatusBadRequest,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(responseBody)),
		}, nil
	})
	manager := NewManager(nil, nil, nil)
	manager.SetRoundTripperProvider(refreshTestRoundTripperProvider{transport: transport})
	auth := &Auth{
		ID:       "auth-safe-observed",
		Index:    "auth-safe-observed",
		Provider: "antigravity",
		Status:   StatusActive,
		Metadata: map[string]any{
			"access_token":  "stored-access-token",
			"refresh_token": "stored-refresh-token",
			"expired":       time.Now().UTC().Add(30 * time.Minute).Format(time.RFC3339Nano),
		},
	}
	if _, errRegister := manager.Register(WithSkipPersist(context.Background()), auth); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	observedHash := AccessTokenSHA256(auth)

	for attempt := 1; attempt <= 2; attempt++ {
		updated, errRefresh := manager.RefreshNowObserved(context.Background(), auth.ID, observedHash)
		if errRefresh != nil {
			t.Fatalf("RefreshNowObserved() attempt %d error = %v", attempt, errRefresh)
		}
		if updated == nil || updated.Metadata["access_token"] != "stored-access-token" {
			t.Fatalf("RefreshNowObserved() attempt %d auth = %#v, want current token", attempt, updated)
		}
	}
	if providerCalls != 1 {
		t.Fatalf("provider refresh calls = %d, want 1 during refresh backoff", providerCalls)
	}

	_, errRefresh := manager.RefreshNow(context.Background(), auth.ID)
	var authErr *Error
	if !errors.As(errRefresh, &authErr) || authErr.Code != refreshTransientErrorCode || errRefresh.Error() != responseBody {
		t.Fatalf("RefreshNow() error = %#v, want persisted transient upstream response", errRefresh)
	}
	persisted, ok := manager.GetByID(auth.ID)
	if !ok || persisted == nil || persisted.LastError == nil || persisted.LastError.Code != refreshTransientErrorCode {
		t.Fatalf("persisted refresh diagnostic = %#v", persisted)
	}
}

func TestRefreshNowObservedReturnsConcurrentRotatedTokenAfterRefreshFailure(t *testing.T) {
	t.Parallel()

	const authID = "auth-concurrent-rotation"
	const responseBody = `{"error":"invalid_request","error_description":"Malformed request"}`
	manager := NewManager(nil, nil, nil)
	var concurrentUpdateErr error
	transport := refreshTestRoundTripper(func(*http.Request) (*http.Response, error) {
		current, ok := manager.GetByID(authID)
		if !ok || current == nil {
			return nil, errors.New("concurrent auth snapshot is unavailable")
		}
		rotated := current.Clone()
		rotated.StateVersion++
		rotated.Metadata["access_token"] = "concurrently-rotated-access-token"
		rotated.Metadata["refresh_token"] = "concurrently-rotated-refresh-token"
		rotated.Metadata["expired"] = time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)
		_, concurrentUpdateErr = manager.Update(WithSkipPersist(context.Background()), rotated)
		return &http.Response{
			StatusCode: http.StatusBadRequest,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(responseBody)),
		}, nil
	})
	manager.SetRoundTripperProvider(refreshTestRoundTripperProvider{transport: transport})
	auth := &Auth{
		ID:           authID,
		Index:        authID,
		Provider:     "antigravity",
		Status:       StatusActive,
		StateVersion: 10,
		Metadata: map[string]any{
			"access_token":  "observed-access-token",
			"refresh_token": "observed-refresh-token",
			"expired":       time.Now().UTC().Add(30 * time.Minute).Format(time.RFC3339Nano),
		},
	}
	if _, errRegister := manager.Register(WithSkipPersist(context.Background()), auth); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}

	updated, errRefresh := manager.RefreshNowObserved(context.Background(), authID, AccessTokenSHA256(auth))
	if errRefresh != nil {
		t.Fatalf("RefreshNowObserved() error = %v, want concurrent rotation", errRefresh)
	}
	if concurrentUpdateErr != nil {
		t.Fatalf("concurrent Update() error = %v", concurrentUpdateErr)
	}
	if updated == nil || updated.StateVersion != 11 || updated.Metadata["access_token"] != "concurrently-rotated-access-token" {
		t.Fatalf("RefreshNowObserved() auth = %#v, want concurrent version 11 token", updated)
	}
}

func TestRefreshNowObservedReturnsConcurrentDisabledState(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name    string
		refresh func(*Manager, *Auth) (*Auth, bool, error)
	}{
		{
			name: "transient failure",
			refresh: func(_ *Manager, _ *Auth) (*Auth, bool, error) {
				return nil, true, errors.New("proxy connection refused")
			},
		},
		{
			name: "successful provider refresh",
			refresh: func(_ *Manager, target *Auth) (*Auth, bool, error) {
				updated := target.Clone()
				updated.Metadata["access_token"] = "provider-refreshed-access-token"
				return updated, true, nil
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			const authID = "auth-concurrent-disable"
			manager := NewManager(nil, nil, nil)
			auth := &Auth{
				ID:           authID,
				Index:        authID,
				Provider:     "plugin-refresh-test",
				Status:       StatusActive,
				StateVersion: 10,
				Metadata: map[string]any{
					"access_token": "observed-access-token",
				},
			}
			if _, errRegister := manager.Register(WithSkipPersist(context.Background()), auth); errRegister != nil {
				t.Fatalf("Register() error = %v", errRegister)
			}
			manager.SetPluginAuthRefresher(refreshTestPluginRefresher(func(ctx context.Context, target *Auth) (*Auth, bool, error) {
				current, ok := manager.GetByID(authID)
				if !ok || current == nil {
					return nil, true, errors.New("concurrent auth snapshot is unavailable")
				}
				disabled := current.Clone()
				disabled.StateVersion++
				disabled.Disabled = true
				disabled.Unavailable = true
				disabled.Status = StatusDisabled
				disabled.StatusMessage = "unauthorized"
				disabled.LastError = newUnauthorizedRefreshError()
				if _, errUpdate := manager.Update(WithSkipPersist(ctx), disabled); errUpdate != nil {
					return nil, true, errUpdate
				}
				return test.refresh(manager, target)
			}))

			updated, errRefresh := manager.RefreshNowObserved(context.Background(), authID, AccessTokenSHA256(auth))
			if updated != nil {
				t.Fatalf("RefreshNowObserved() auth = %#v, want no disabled credential payload", updated)
			}
			var authErr *Error
			if !errors.As(errRefresh, &authErr) || authErr == nil || authErr.Code != refreshAuthErrorCode || authErr.Message != refreshAuthErrorMsg {
				t.Fatalf("RefreshNowObserved() error = %#v, want concurrent unauthorized state", errRefresh)
			}
		})
	}
}

func TestRefreshNowObservedRejectsStaleSuccessAndDoesNotResumeConcurrentUnauthorizedModel(t *testing.T) {
	const (
		authID = "auth-concurrent-model-state"
		model  = "concurrent-unauthorized-model"
	)
	now := time.Now().UTC()
	modelRegistry := registry.GetGlobalRegistry()
	modelRegistry.RegisterClient(authID, "plugin-refresh-test", []*registry.ModelInfo{{ID: model, Object: "model", Type: "openai"}})
	modelRegistry.SuspendClientModel(authID, model, "unauthorized")
	t.Cleanup(func() { modelRegistry.UnregisterClient(authID) })
	if registryHasAvailableModel(modelRegistry, model) {
		t.Fatal("test setup did not suspend model")
	}

	manager := NewManager(nil, nil, nil)
	auth := &Auth{
		ID:           authID,
		Index:        authID,
		Provider:     "plugin-refresh-test",
		Status:       StatusError,
		Unavailable:  true,
		StateVersion: 10,
		Metadata:     map[string]any{"access_token": "observed-access-token"},
		ModelStates: map[string]*ModelState{
			model: {
				Status:        StatusError,
				StatusMessage: "unauthorized",
				Unavailable:   true,
				LastError:     &Error{Message: "access token expired", HTTPStatus: http.StatusUnauthorized},
				UpdatedAt:     now,
			},
		},
	}
	if _, errRegister := manager.Register(WithSkipPersist(context.Background()), auth); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	manager.SetPluginAuthRefresher(refreshTestPluginRefresher(func(ctx context.Context, target *Auth) (*Auth, bool, error) {
		current, ok := manager.GetByID(authID)
		if !ok || current == nil {
			return nil, true, errors.New("concurrent auth snapshot is unavailable")
		}
		concurrent := current.Clone()
		concurrent.StateVersion++
		concurrent.Label = "concurrent update"
		if _, errUpdate := manager.Update(WithSkipPersist(ctx), concurrent); errUpdate != nil {
			return nil, true, errUpdate
		}
		updated := target.Clone()
		updated.Metadata["access_token"] = "provider-refreshed-access-token"
		return updated, true, nil
	}))

	updated, errRefresh := manager.RefreshNowObserved(context.Background(), authID, AccessTokenSHA256(auth))
	if updated != nil {
		t.Fatalf("RefreshNowObserved() auth = %#v, want no stale credential payload", updated)
	}
	var authErr *Error
	if !errors.As(errRefresh, &authErr) || authErr.Code != refreshTransientErrorCode {
		t.Fatalf("RefreshNowObserved() error = %#v, want transient refresh error", errRefresh)
	}
	if registryHasAvailableModel(modelRegistry, model) {
		t.Fatal("stale refresh success resumed a concurrently unauthorized model")
	}
}

func TestCanUseObservedTokenAfterRefreshFailure(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	base := &Auth{
		ID:       "auth-safe-observed-policy",
		Provider: "antigravity",
		Status:   StatusActive,
		Metadata: map[string]any{
			"access_token": "stored-access-token",
			"expired":      now.Add(30 * time.Minute).Format(time.RFC3339Nano),
		},
	}
	matchingHash := AccessTokenSHA256(base)
	mismatchedHash := AccessTokenSHA256(&Auth{Metadata: map[string]any{"access_token": "other-token"}})

	for _, test := range []struct {
		name         string
		observedHash string
		errRefresh   error
		mutate       func(*Auth)
		want         bool
	}{
		{name: "matching transient", observedHash: matchingHash, errRefresh: NewTransientRefreshError(), want: true},
		{name: "matching unsupported", observedHash: matchingHash, errRefresh: ErrRefreshUnsupported, want: true},
		{name: "missing hash", errRefresh: NewTransientRefreshError()},
		{name: "invalid hash", observedHash: "not-a-sha256", errRefresh: NewTransientRefreshError()},
		{name: "mismatched hash", observedHash: mismatchedHash, errRefresh: NewTransientRefreshError()},
		{name: "terminal error", observedHash: matchingHash, errRefresh: newUnauthorizedRefreshError()},
		{name: "canceled", observedHash: matchingHash, errRefresh: context.Canceled},
		{name: "deadline", observedHash: matchingHash, errRefresh: context.DeadlineExceeded},
		{
			name:         "inside safety window",
			observedHash: matchingHash,
			errRefresh:   NewTransientRefreshError(),
			mutate: func(auth *Auth) {
				auth.Metadata["expired"] = now.Add(refreshFailureBackoff).Format(time.RFC3339Nano)
			},
		},
		{
			name:         "disabled credential",
			observedHash: matchingHash,
			errRefresh:   NewTransientRefreshError(),
			mutate: func(auth *Auth) {
				auth.Disabled = true
				auth.Status = StatusDisabled
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			auth := base.Clone()
			if test.mutate != nil {
				test.mutate(auth)
			}
			if got := CanUseObservedTokenAfterRefreshFailure(auth, test.observedHash, test.errRefresh, now); got != test.want {
				t.Fatalf("CanUseObservedTokenAfterRefreshFailure() = %v, want %v", got, test.want)
			}
		})
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

func TestBackgroundRefreshLogsSafeProviderError(t *testing.T) {
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
		if entry.Level == log.WarnLevel && entry.Message == "auth refresh failed" && entry.Data["diagnostic"] == "status=400" {
			return
		}
	}
	t.Fatalf("provider refresh error was not logged: %#v", hook.AllEntries())
}

func TestLogCredentialRefreshFailureIncludesSafeDiagnosticFields(t *testing.T) {
	savedHooks := make(log.LevelHooks)
	for level, hooks := range log.StandardLogger().Hooks {
		savedHooks[level] = append([]log.Hook(nil), hooks...)
	}
	hook := logtest.NewGlobal()
	t.Cleanup(func() {
		log.StandardLogger().ReplaceHooks(savedHooks)
	})

	retryAt := time.Now().UTC().Add(refreshFailureBackoff)
	expiresAt := retryAt.Add(25 * time.Minute)
	auth := &Auth{
		ID:               "auth-diagnostic",
		Provider:         "antigravity",
		NextRefreshAfter: retryAt,
		Metadata:         map[string]any{"expired": expiresAt.Format(time.RFC3339Nano)},
		LastError: &Error{
			Code:       refreshTransientErrorCode,
			Message:    refreshTransientErrorMsg,
			Diagnostic: "antigravity refresh failed: stage=transport err=EOF",
		},
	}
	logCredentialRefreshFailure(context.Background(), auth)

	for _, entry := range hook.AllEntries() {
		if entry.Level != log.WarnLevel || entry.Message != "credential refresh failed" {
			continue
		}
		if entry.Data["auth"] != auth.ID || entry.Data["provider"] != auth.Provider || entry.Data["proxy_configured"] != false || entry.Data["stage"] != "credential_refresh" || entry.Data["code"] != refreshTransientErrorCode {
			t.Fatalf("refresh log fields = %#v", entry.Data)
		}
		if entry.Data["diagnostic"] != auth.LastError.Diagnostic || entry.Data["retry_at"] != retryAt.Format(time.RFC3339Nano) || entry.Data["token_expires_at"] != expiresAt.Format(time.RFC3339Nano) || entry.Data["dispatch_blocked"] != false {
			t.Fatalf("refresh diagnostic log fields = %#v", entry.Data)
		}
		return
	}
	t.Fatalf("credential refresh failure was not logged: %#v", hook.AllEntries())
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
	if !strings.Contains(auth.LastError.Diagnostic, "provider=custom") || !strings.Contains(auth.LastError.Diagnostic, "reason=no_refresh_handler") {
		t.Fatalf("unsupported refresh diagnostic = %q, want provider and capability reason", auth.LastError.Diagnostic)
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

func TestRefreshAuthCredentialReturnsUnsupportedDiagnostic(t *testing.T) {
	t.Parallel()

	auth := &Auth{
		ID:       "antigravity-missing-refresh-token",
		Provider: "antigravity",
		Metadata: map[string]any{"access_token": "expired-access-token"},
	}
	refreshed, errRefresh := NewManager(nil, nil, nil).RefreshAuthCredential(context.Background(), auth)
	var authErr *Error
	if !errors.As(errRefresh, &authErr) || authErr == nil || authErr.Code != refreshUnsupportedCode {
		t.Fatalf("RefreshAuthCredential() error = %#v, want structured unsupported refresh error", errRefresh)
	}
	for _, signal := range []string{"provider=antigravity", "reason=missing_refresh_token"} {
		if !strings.Contains(authErr.Diagnostic, signal) {
			t.Fatalf("RefreshAuthCredential() diagnostic = %q, want %q", authErr.Diagnostic, signal)
		}
	}
	if refreshed == nil || refreshed.LastError == nil || refreshed.LastError.Diagnostic != authErr.Diagnostic {
		t.Fatalf("RefreshAuthCredential() state = %#v, want returned diagnostic %q", refreshed, authErr.Diagnostic)
	}
	if !errors.Is(errRefresh, ErrRefreshUnsupported) {
		t.Fatalf("RefreshAuthCredential() error = %#v, want ErrRefreshUnsupported compatibility", errRefresh)
	}
}

func TestApplyRefreshFailureStateKeepsSafeAntigravityTokenDispatchable(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name         string
		expiresAfter time.Duration
		retryAfter   time.Duration
	}{
		{name: "retry before safety window", expiresAfter: 6 * time.Minute, retryAfter: time.Minute},
		{name: "keep normal backoff", expiresAfter: 20 * time.Minute, retryAfter: refreshFailureBackoff},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Now().UTC()
			auth := &Auth{
				ID:       "auth-safe-antigravity",
				Provider: "antigravity",
				Status:   StatusActive,
				Metadata: map[string]any{
					"access_token": "still-valid-access",
					"expired":      now.Add(test.expiresAfter).Format(time.RFC3339Nano),
				},
			}

			ApplyRefreshFailureState(auth, errors.New("proxy connection refused"), now)

			wantRefreshAt := now.Add(test.retryAfter)
			if auth.Disabled || auth.Unavailable || auth.RuntimeRefreshBlocked || auth.Status != StatusActive {
				t.Fatalf("safe Antigravity token was blocked after transient refresh failure: %#v", auth)
			}
			if !auth.NextRefreshAfter.Equal(wantRefreshAt) || !auth.NextRetryAfter.IsZero() {
				t.Fatalf("refresh/retry deadlines = %v/%v, want %v/zero", auth.NextRefreshAfter, auth.NextRetryAfter, wantRefreshAt)
			}
			if auth.LastError == nil || auth.LastError.Code != refreshTransientErrorCode {
				t.Fatalf("LastError = %#v, want transient refresh diagnostic", auth.LastError)
			}
			if blocked, _, _ := isAuthBlockedForModel(auth, "gemini-3.7-flash-high", now); blocked {
				t.Fatal("safe Antigravity token became undispatchable")
			}
		})
	}
}

func TestExecutionResultsPreserveNonBlockingRefreshDiagnostic(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	for _, test := range []struct {
		name   string
		result Result
	}{
		{name: "success", result: Result{AuthID: "auth-safe-refresh-result", Model: "gemini-3.7-flash-high", Success: true}},
		{name: "failure", result: Result{AuthID: "auth-safe-refresh-result", Model: "gemini-3.7-flash-high", Error: &Error{Message: "upstream unavailable", HTTPStatus: http.StatusServiceUnavailable}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			auth := &Auth{
				ID:       test.result.AuthID,
				Index:    test.result.AuthID,
				Provider: "antigravity",
				Status:   StatusActive,
				Metadata: map[string]any{
					"access_token": "still-valid-access",
					"expired":      now.Add(20 * time.Minute).Format(time.RFC3339Nano),
				},
			}
			body := []byte(`{"error":"temporarily_unavailable"}`)
			errRefresh := &Error{
				Code:       refreshTransientErrorCode,
				Message:    refreshTransientErrorMsg,
				Diagnostic: "antigravity refresh failed: stage=upstream_response status=503",
				Retryable:  true,
				HTTPStatus: http.StatusServiceUnavailable,
				Upstream:   &UpstreamResponse{Status: http.StatusServiceUnavailable, Body: body},
			}
			ApplyRefreshFailureState(auth, errRefresh, now)
			NewManager(nil, nil, nil).applyResultTransition(auth, test.result, test.result.Model, now.Add(time.Second), false)

			if auth.LastRefreshError == nil || auth.LastRefreshError.Code != refreshTransientErrorCode || auth.LastRefreshError.Diagnostic == "" || auth.LastRefreshError.Upstream == nil || string(auth.LastRefreshError.Upstream.Body) != string(body) {
				t.Fatalf("LastRefreshError = %#v, want retained structured refresh diagnostic", auth.LastRefreshError)
			}
			manager := NewManager(nil, nil, nil)
			if _, errRegister := manager.Register(WithSkipPersist(context.Background()), auth); errRegister != nil {
				t.Fatalf("Register() error = %v", errRegister)
			}
			_, errObserved := manager.RefreshNowObserved(context.Background(), auth.ID, "")
			var observed *Error
			if !errors.As(errObserved, &observed) || observed == nil || observed.Diagnostic != auth.LastRefreshError.Diagnostic || observed.Upstream == nil || string(observed.Upstream.Body) != string(body) {
				t.Fatalf("RefreshNowObserved() error = %#v, want retained refresh diagnostic", errObserved)
			}
		})
	}
}

func TestApplyRefreshFailureStateBlocksKnownUnauthorizedAntigravityToken(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	for _, test := range []struct {
		name  string
		apply func(*Auth)
	}{
		{
			name: "credential unauthorized",
			apply: func(auth *Auth) {
				auth.Status = StatusError
				auth.StatusMessage = "unauthorized"
				auth.Unavailable = true
				auth.LastError = &Error{Code: "unauthorized", Message: "access token expired", HTTPStatus: http.StatusUnauthorized}
			},
		},
		{
			name: "model unauthorized",
			apply: func(auth *Auth) {
				auth.ModelStates = map[string]*ModelState{
					"gemini-3.7-flash-high": {
						Status:        StatusError,
						StatusMessage: "unauthorized",
						Unavailable:   true,
						LastError:     &Error{Message: "access token expired", HTTPStatus: http.StatusUnauthorized},
					},
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			auth := &Auth{
				ID:       "auth-known-unauthorized",
				Provider: "antigravity",
				Status:   StatusActive,
				Metadata: map[string]any{
					"access_token": "known-invalid-access",
					"expired":      now.Add(20 * time.Minute).Format(time.RFC3339Nano),
				},
			}
			test.apply(auth)
			observedHash := AccessTokenSHA256(auth)
			refreshErr := errors.New("proxy connection refused")

			if CanUseObservedTokenAfterRefreshFailure(auth, observedHash, refreshErr, now) {
				t.Fatal("known unauthorized token was accepted after refresh failure")
			}
			ApplyRefreshFailureState(auth, refreshErr, now)

			if !auth.Unavailable || !auth.RuntimeRefreshBlocked || !RefreshBlocksDispatch(auth) {
				t.Fatalf("known unauthorized token remained dispatchable after refresh failure: %#v", auth)
			}
		})
	}
}

func TestApplyRefreshFailureStateClearsOnlyRefreshOwnedCredentialState(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	quotaRecoverAt := now.Add(12 * time.Minute)
	newBlockedAuth := func() *Auth {
		return &Auth{
			ID:                    "auth-safe-credential-quota",
			Provider:              "antigravity",
			Status:                StatusError,
			StatusMessage:         refreshTransientErrorMsg,
			Unavailable:           true,
			RuntimeRefreshBlocked: true,
			NextRetryAfter:        now.Add(-time.Second),
			LastError:             newTransientRefreshError(),
			Quota: QuotaState{
				Exceeded:      true,
				Scope:         "credential",
				Reason:        "credential quota exceeded",
				NextRecoverAt: quotaRecoverAt,
				BackoffLevel:  3,
			},
			Metadata: map[string]any{
				"access_token": "still-valid-access",
				"expired":      now.Add(20 * time.Minute).Format(time.RFC3339Nano),
			},
		}
	}

	t.Run("restores credential quota after refresh error overlay", func(t *testing.T) {
		auth := newBlockedAuth()

		ApplyRefreshFailureState(auth, errors.New("proxy connection refused"), now)

		if auth.RuntimeRefreshBlocked || RefreshBlocksDispatch(auth) {
			t.Fatalf("refresh-owned block was not cleared: %#v", auth)
		}
		if !auth.Quota.Exceeded || auth.Quota.Scope != "credential" || !auth.Quota.NextRecoverAt.Equal(quotaRecoverAt) {
			t.Fatalf("credential quota was lost: %#v", auth.Quota)
		}
		if auth.Status != StatusError || !auth.Unavailable || !auth.NextRetryAfter.Equal(quotaRecoverAt) {
			t.Fatalf("credential quota availability = %#v, want retry at %v", auth, quotaRecoverAt)
		}
		if auth.LastError == nil || auth.LastError.Code == refreshTransientErrorCode || auth.LastError.HTTPStatus != http.StatusTooManyRequests || auth.StatusMessage != auth.Quota.Reason {
			t.Fatalf("credential quota error = %#v/%q, want non-refresh quota error", auth.LastError, auth.StatusMessage)
		}
	})

	t.Run("preserves existing non-refresh credential error", func(t *testing.T) {
		auth := newBlockedAuth()
		auth.StatusMessage = "provider quota exhausted"
		auth.LastError = &Error{
			Code:       "quota_exhausted",
			Message:    "provider quota exhausted",
			Diagnostic: "provider quota window is open",
			Retryable:  true,
			HTTPStatus: http.StatusTooManyRequests,
			Upstream: &UpstreamResponse{
				Status: http.StatusTooManyRequests,
				Body:   []byte(`{"error":"quota_exhausted"}`),
			},
		}

		ApplyRefreshFailureState(auth, errors.New("proxy connection refused"), now)

		if auth.LastError == nil || auth.LastError.Code != "quota_exhausted" || auth.LastError.Diagnostic != "provider quota window is open" || auth.LastError.Upstream == nil || string(auth.LastError.Upstream.Body) != `{"error":"quota_exhausted"}` {
			t.Fatalf("non-refresh credential error was not preserved: %#v", auth.LastError)
		}
		if auth.StatusMessage != "provider quota exhausted" || !auth.Unavailable || !auth.NextRetryAfter.Equal(quotaRecoverAt) {
			t.Fatalf("non-refresh availability was not preserved: %#v", auth)
		}
	})
}

func TestApplyRefreshFailureStateKeepsModelCooldownFromBecomingRefreshBlock(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	retryAt := now.Add(time.Minute)
	auth := &Auth{
		ID:             "auth-safe-model-cooldown",
		Provider:       "antigravity",
		Status:         StatusError,
		Unavailable:    true,
		NextRetryAfter: retryAt,
		LastError:      &Error{Message: "MODEL_CAPACITY_EXHAUSTED", HTTPStatus: http.StatusServiceUnavailable},
		Metadata: map[string]any{
			"access_token": "still-valid-access",
			"expired":      now.Add(refreshFailureBackoff + time.Minute).Format(time.RFC3339Nano),
		},
		ModelStates: map[string]*ModelState{
			"model-a": {
				Status:         StatusError,
				StatusMessage:  "MODEL_CAPACITY_EXHAUSTED",
				Unavailable:    true,
				NextRetryAfter: retryAt,
				LastError:      &Error{Message: "MODEL_CAPACITY_EXHAUSTED", HTTPStatus: http.StatusServiceUnavailable},
				UpdatedAt:      now,
			},
		},
	}

	refreshFailure := ApplyRefreshFailureState(auth, errors.New("proxy connection refused"), now)

	if refreshFailure == nil || refreshFailure.Code != refreshTransientErrorCode {
		t.Fatalf("refresh failure = %#v, want transient refresh error", refreshFailure)
	}
	if RefreshBlocksDispatch(auth) {
		t.Fatalf("safe refresh failure became a credential-wide block: %#v", auth)
	}
	if auth.LastError == nil || auth.LastError.Message != "MODEL_CAPACITY_EXHAUSTED" {
		t.Fatalf("auth LastError = %#v, want model error preserved", auth.LastError)
	}
	if blocked, _, _ := isAuthBlockedForModel(auth, "model-b", now); blocked {
		t.Fatal("safe refresh failure blocked an unrelated model")
	}
}

func TestApplyRefreshFailureStateBlocksAntigravityWithoutAccessToken(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	auth := &Auth{
		ID:       "auth-missing-antigravity-access",
		Provider: "antigravity",
		Status:   StatusActive,
		Metadata: map[string]any{
			"refresh_token": "refresh-token",
			"expired":       now.Add(refreshFailureBackoff + time.Minute).Format(time.RFC3339Nano),
		},
	}

	ApplyRefreshFailureState(auth, errors.New("proxy connection refused"), now)

	if !auth.Unavailable || !auth.RuntimeRefreshBlocked || !RefreshBlocksDispatch(auth) {
		t.Fatalf("Antigravity credential without an access token remained dispatchable: %#v", auth)
	}
}

func TestApplyRefreshFailureStateBlocksAntigravityInsideSafetyWindow(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	auth := &Auth{
		ID:       "auth-expiring-antigravity",
		Provider: "antigravity",
		Status:   StatusActive,
		Metadata: map[string]any{
			"access_token": "expiring-access",
			"expired":      now.Add(refreshFailureBackoff - time.Minute).Format(time.RFC3339Nano),
		},
	}

	ApplyRefreshFailureState(auth, errors.New("proxy connection refused"), now)

	if !auth.Unavailable || !auth.RuntimeRefreshBlocked || auth.Status != StatusError {
		t.Fatalf("Antigravity token inside the safety window remained dispatchable: %#v", auth)
	}
	if blocked, _, _ := isAuthBlockedForModel(auth, "gemini-3.7-flash-high", now); !blocked {
		t.Fatal("Antigravity token inside the safety window was not blocked")
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
			ApplyRefreshFailureState(auth, errors.New(message), now)

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

	ApplyRefreshFailureState(auth, errRefresh, now)

	if auth.LastError == nil || auth.LastError.Message != refreshTransientErrorMsg || auth.StatusMessage != refreshTransientErrorMsg {
		t.Fatalf("refresh failure state = %#v, want generic scheduling message", auth)
	}
	if auth.LastError.Upstream == nil || auth.LastError.Upstream.Status != http.StatusBadRequest || string(auth.LastError.Upstream.Body) != string(body) {
		t.Fatalf("transient upstream response = %#v, want 400/%q", auth.LastError.Upstream, body)
	}
	if got := auth.LastError.Error(); got != string(body) {
		t.Fatalf("refresh error = %q, want exact upstream body %q", got, body)
	}
	if !strings.Contains(auth.LastError.Diagnostic, "stage=upstream_response") || !strings.Contains(auth.LastError.Diagnostic, "status 400") {
		t.Fatalf("refresh diagnostic = %q, want upstream response stage and status", auth.LastError.Diagnostic)
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
