package auth

import (
	"context"
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
	if isTerminalRefreshAuthError(errors.New("kimi: refresh failed with status 401")) {
		t.Fatal("generic Kimi refresh 401 was classified as terminal")
	}
}

func TestAuthIsNewerThanObservedPrefersTokenFingerprint(t *testing.T) {
	t.Parallel()

	refreshedAt := time.Now().UTC()
	auth := &Auth{
		LastRefreshedAt: refreshedAt,
		Metadata:        map[string]any{"access_token": "current-token"},
	}
	currentHash := AccessTokenSHA256(auth)
	staleHash := AccessTokenSHA256(&Auth{Metadata: map[string]any{"access_token": "stale-token"}})

	if AuthIsNewerThanObserved(auth, refreshedAt.Add(-time.Hour), currentHash) {
		t.Fatal("matching token fingerprint was treated as stale because of timestamp")
	}
	if !AuthIsNewerThanObserved(auth, refreshedAt.Add(time.Hour), staleHash) {
		t.Fatal("different token fingerprint was not treated as stale")
	}
	if AuthIsNewerThanObserved(auth, refreshedAt.Add(-time.Hour), "not-a-sha256") {
		t.Fatal("invalid token fingerprint fell back to timestamp")
	}
	if !AuthIsNewerThanObserved(auth, refreshedAt.Add(-time.Hour), "") {
		t.Fatal("missing fingerprint did not fall back to refresh timestamp")
	}
}

func TestAuthCloneDeepCopiesNestedMetadata(t *testing.T) {
	t.Parallel()

	original := &Auth{Metadata: map[string]any{
		"token": map[string]any{"access_token": "old-token"},
	}}
	cloned := original.Clone()
	cloned.Metadata["token"].(map[string]any)["access_token"] = "fresh-token"
	if got := original.Metadata["token"].(map[string]any)["access_token"]; got != "old-token" {
		t.Fatalf("nested metadata mutation changed original: %v", got)
	}
}

func TestAccessTokenSHA256SupportsKnownMetadataShapes(t *testing.T) {
	t.Parallel()

	want := AccessTokenSHA256(&Auth{Metadata: map[string]any{"access_token": "same-token"}})
	cases := map[string]*Auth{
		"camel case":        {Metadata: map[string]any{"accessToken": "same-token"}},
		"nested any map":    {Metadata: map[string]any{"token": map[string]any{"access_token": "same-token"}}},
		"nested string map": {Metadata: map[string]any{"Token": map[string]string{"accessToken": "same-token"}}},
	}
	for name, auth := range cases {
		t.Run(name, func(t *testing.T) {
			if got := AccessTokenSHA256(auth); got == "" || got != want {
				t.Fatalf("token hash = %q, want %q", got, want)
			}
		})
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

func TestRefreshPendingDoesNotShortenAuthCooldown(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	recoverAt := now.Add(time.Hour)
	auth := &Auth{
		Status:         StatusError,
		Unavailable:    true,
		NextRetryAfter: recoverAt,
		Quota:          QuotaState{Exceeded: true, NextRecoverAt: recoverAt},
	}
	setAuthCooldownScope(auth, cooldownScopeAuth)

	ApplyRefreshPendingState(auth, now, now.Add(refreshPendingBackoff))
	if !auth.NextRetryAfter.Equal(recoverAt) {
		t.Fatalf("NextRetryAfter = %v, want existing auth cooldown %v", auth.NextRetryAfter, recoverAt)
	}
}

func TestMarkRefreshPendingBlocksDispatchUntilRefreshCompletes(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, nil, nil)
	now := time.Now().UTC()
	auth := &Auth{
		ID:               "auth-refresh-pending",
		Index:            "auth-refresh-pending",
		Provider:         "codex",
		Status:           StatusError,
		Unavailable:      true,
		NextRefreshAfter: now.Add(-time.Second),
		NextRetryAfter:   now.Add(-time.Second),
	}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}

	if !manager.markRefreshPending(auth.ID, now) {
		t.Fatal("markRefreshPending() = false, want true")
	}
	updated, ok := manager.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatal("pending auth not found")
	}
	if blocked, _, next := isAuthBlockedForModel(updated, "gpt-5", now); !blocked || !next.After(now) {
		t.Fatalf("pending auth dispatch state = blocked %v next %v, want blocked with future retry", blocked, next)
	}
	decision, errDispatch := manager.Dispatch(context.Background(), []string{"codex"}, "gpt-5", Options{})
	if errDispatch == nil || decision != nil {
		t.Fatalf("Dispatch() = decision %#v error %v, want pending credential blocked", decision, errDispatch)
	}
}

func TestAutoRefreshUsesExternalLockHandler(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, nil, nil)
	auth := &Auth{
		ID:       "auth-auto-handler",
		Index:    "auth-auto-handler",
		Provider: "antigravity",
		Status:   StatusActive,
		Metadata: map[string]any{"access_token": "old", "refresh_token": "refresh"},
	}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	calls := 0
	manager.SetAutoRefreshHandler(func(_ context.Context, observed *Auth) error {
		calls++
		if observed == nil || observed.ID != auth.ID {
			t.Fatalf("auto refresh observed auth = %#v", observed)
		}
		return nil
	})

	manager.refreshAuth(context.Background(), auth.ID)

	if calls != 1 {
		t.Fatalf("auto refresh handler calls = %d, want 1", calls)
	}
}

func TestAutoRefreshUnsupportedCredentialBacksOff(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, nil, nil)
	auth := &Auth{
		ID:       "auth-auto-unsupported",
		Index:    "auth-auto-unsupported",
		Provider: "codex",
		Status:   StatusActive,
		Metadata: map[string]any{"access_token": "access-only"},
	}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}

	manager.refreshAuth(context.Background(), auth.ID)

	updated, ok := manager.GetByID(auth.ID)
	if !ok || updated == nil || !updated.NextRefreshAfter.After(time.Now()) {
		t.Fatalf("unsupported auto-refresh state = %#v, want future NextRefreshAfter", updated)
	}
}

func TestRefreshAuthCredentialNoOpPreservesUnauthorizedState(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	auth := &Auth{
		ID:             "auth-no-refresh-token",
		Index:          "auth-no-refresh-token",
		Provider:       "codex",
		Status:         StatusError,
		StatusMessage:  "unauthorized",
		Unavailable:    true,
		NextRetryAfter: now.Add(time.Minute),
		LastError:      &Error{HTTPStatus: http.StatusUnauthorized, Message: "expired"},
		Metadata:       map[string]any{"access_token": "expired"},
	}
	manager := NewManager(nil, nil, nil)

	updated, errRefresh := manager.RefreshAuthCredential(context.Background(), auth)
	if !errors.Is(errRefresh, ErrRefreshUnsupported) {
		t.Fatalf("RefreshAuthCredential() error = %v, want ErrRefreshUnsupported", errRefresh)
	}
	if updated == nil || updated.Status != StatusError || !updated.Unavailable || updated.LastError == nil {
		t.Fatalf("no-op refresh changed unauthorized state: %#v", updated)
	}
	if !updated.LastRefreshedAt.IsZero() {
		t.Fatalf("LastRefreshedAt = %v, want zero after no-op", updated.LastRefreshedAt)
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

	resumed := ApplyRefreshSuccessState(auth, now)

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

func TestApplyRefreshSuccessStatePreservesQuotaOnUnauthorizedModel(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	quotaRetryAt := now.Add(10 * time.Minute)
	auth := &Auth{
		ID:       "auth-unauthorized-model-quota",
		Provider: "codex",
		Status:   StatusError,
		ModelStates: map[string]*ModelState{
			"gpt-5": {
				Status:         StatusError,
				StatusMessage:  "unauthorized",
				Unavailable:    true,
				NextRetryAfter: quotaRetryAt,
				LastError:      &Error{HTTPStatus: http.StatusUnauthorized, Message: "unauthorized"},
				Quota:          QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: quotaRetryAt, BackoffLevel: 2},
			},
		},
	}

	resumed := ApplyRefreshSuccessState(auth, now)

	state := auth.ModelStates["gpt-5"]
	if len(resumed) != 0 || state == nil || !state.Quota.Exceeded || !state.Unavailable || !state.NextRetryAfter.Equal(quotaRetryAt) {
		t.Fatalf("refreshed quota model = resumed %v state %#v", resumed, state)
	}
}

func TestApplyRefreshSuccessStateDoesNotPromoteModelQuota(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	quotaRetryAt := now.Add(10 * time.Minute)
	modelQuota := QuotaState{Exceeded: true, Reason: "model quota", NextRecoverAt: quotaRetryAt, BackoffLevel: 2}
	auth := &Auth{
		ID:          "auth-model-quota",
		Provider:    "codex",
		Status:      StatusError,
		Unavailable: true,
		Quota:       modelQuota,
		ModelStates: map[string]*ModelState{
			"gpt-5": {
				Status:         StatusError,
				StatusMessage:  "unauthorized",
				Unavailable:    true,
				NextRetryAfter: now.Add(time.Minute),
				LastError:      &Error{HTTPStatus: http.StatusUnauthorized, Message: "unauthorized"},
			},
			"gpt-5-mini": {
				Status:         StatusError,
				Unavailable:    true,
				NextRetryAfter: quotaRetryAt,
				Quota:          modelQuota,
			},
			"gpt-5-nano": {Status: StatusActive},
		},
	}

	ApplyRefreshSuccessState(auth, now)

	if auth.Unavailable {
		t.Fatalf("model quota was promoted to auth-level unavailable: %#v", auth)
	}
	if !auth.Quota.Exceeded || !auth.Quota.NextRecoverAt.Equal(quotaRetryAt) {
		t.Fatalf("aggregated model quota = %#v, want preserved for reporting", auth.Quota)
	}
}

func TestApplyRefreshSuccessStatePreservesQuotaCooldown(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	quotaRetryAt := now.Add(10 * time.Minute)
	auth := &Auth{
		ID:             "auth-refresh-quota",
		Provider:       "codex",
		Status:         StatusError,
		StatusMessage:  refreshTransientErrorMsg,
		Unavailable:    true,
		NextRetryAfter: now.Add(refreshFailureBackoff),
		LastError:      &Error{Code: refreshTransientErrorCode, Message: refreshTransientErrorMsg, Retryable: true},
		Quota: QuotaState{
			Exceeded:      true,
			Reason:        "quota",
			NextRecoverAt: quotaRetryAt,
			BackoffLevel:  3,
		},
	}

	ApplyRefreshSuccessState(auth, now)

	if !auth.Quota.Exceeded || !auth.Quota.NextRecoverAt.Equal(quotaRetryAt) {
		t.Fatalf("quota state = %#v, want preserved", auth.Quota)
	}
	if !auth.Unavailable || auth.Status != StatusError || !auth.NextRetryAfter.Equal(quotaRetryAt) {
		t.Fatalf("quota availability = unavailable %v status %v retry %v, want true/error/%v", auth.Unavailable, auth.Status, auth.NextRetryAfter, quotaRetryAt)
	}
}
