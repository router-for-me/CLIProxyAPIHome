package auth

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type markResultBlockingStore struct {
	block       atomic.Bool
	saveCalls   atomic.Int32
	startOnce   sync.Once
	saveStarted chan struct{}
	unblock     chan struct{}
}

func newMarkResultBlockingStore() *markResultBlockingStore {
	return &markResultBlockingStore{
		saveStarted: make(chan struct{}),
		unblock:     make(chan struct{}),
	}
}

func (s *markResultBlockingStore) List(context.Context) ([]*Auth, error) {
	return nil, nil
}

func (s *markResultBlockingStore) Save(ctx context.Context, auth *Auth) (string, error) {
	s.saveCalls.Add(1)
	if s.block.Load() {
		s.startOnce.Do(func() {
			close(s.saveStarted)
		})
		select {
		case <-s.unblock:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	if auth == nil {
		return "", nil
	}
	return auth.ID, nil
}

func (s *markResultBlockingStore) Delete(context.Context, string) error {
	return nil
}

func TestMarkResultDoesNotHoldManagerLockWhilePersisting(t *testing.T) {
	store := newMarkResultBlockingStore()
	manager := NewManager(store, nil, nil)

	auth := &Auth{
		ID:       "auth-1",
		Provider: "gemini",
		Status:   StatusActive,
		Metadata: map[string]any{
			"email": "user@example.com",
		},
	}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}

	store.block.Store(true)
	done := make(chan struct{})
	go func() {
		manager.MarkResult(context.Background(), Result{
			AuthID:  "auth-1",
			Model:   "gemini-3.1-pro-preview",
			Success: false,
			Error: &Error{
				Message:    "quota exhausted",
				HTTPStatus: http.StatusTooManyRequests,
			},
		})
		close(done)
	}()

	select {
	case <-store.saveStarted:
	case <-time.After(time.Second):
		close(store.unblock)
		<-done
		t.Fatal("MarkResult() did not reach store Save")
	}

	readDone := make(chan struct{})
	go func() {
		if got, ok := manager.GetByID("auth-1"); !ok || got == nil {
			t.Errorf("GetByID() = %#v, %v; want auth", got, ok)
		}
		close(readDone)
	}()

	select {
	case <-readDone:
	case <-time.After(100 * time.Millisecond):
		close(store.unblock)
		<-done
		t.Fatal("GetByID() blocked while MarkResult() was persisting")
	}

	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		close(store.unblock)
		<-done
		t.Fatal("MarkResult() blocked while persisting")
	}

	close(store.unblock)
}

func TestMarkResultUnauthorizedUsesRecoverableCooldown(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	auth := &Auth{
		ID:       "auth-401",
		Index:    "auth-401",
		Provider: "codex",
		Status:   StatusActive,
		Metadata: map[string]any{"access_token": "expired"},
	}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}

	before := time.Now()
	manager.MarkResult(context.Background(), Result{
		AuthID:  auth.ID,
		Model:   "gpt-5",
		Success: false,
		Error: &Error{
			Message:    "expired access token",
			HTTPStatus: http.StatusUnauthorized,
		},
	})

	updated, ok := manager.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatal("updated auth not found")
	}
	if updated.Disabled || updated.Status == StatusDisabled {
		t.Fatalf("execution 401 permanently disabled auth: %#v", updated)
	}
	state := updated.ModelStates["gpt-5"]
	if state == nil || !state.Unavailable || state.Status != StatusError {
		t.Fatalf("401 model state = %#v, want recoverable unavailable error", state)
	}
	if state.NextRetryAfter.Before(before.Add(unauthorizedRetryBackoff - time.Second)) {
		t.Fatalf("NextRetryAfter = %v, want about %v after failure", state.NextRetryAfter, unauthorizedRetryBackoff)
	}
	if blocked, reason, _ := isAuthBlockedForModel(updated, "gpt-5", time.Now()); !blocked || reason == blockReasonDisabled {
		t.Fatalf("isAuthBlockedForModel() = blocked %v reason %v, want non-disabled cooldown", blocked, reason)
	}
	if blocked, reason, next := isAuthBlockedForModel(updated, "gpt-5-mini", time.Now()); blocked || reason != blockReasonNone || !next.IsZero() {
		t.Fatalf("unrelated model blocked/reason/next = %v/%v/%v", blocked, reason, next)
	}
}

func TestNewUsageResultScopesGoogleResetTimestampToAntigravity(t *testing.T) {
	resetAt := time.Now().UTC().Add(time.Hour).Truncate(time.Millisecond)
	body := `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","metadata":{"quotaResetTimeStamp":"` + resetAt.Format(time.RFC3339Nano) + `"}},{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"2s"}]}}`

	antigravityResult := NewUsageResult("antigravity-auth", "antigravity", "gemini-3.7-flash-high", http.StatusTooManyRequests, body)
	if antigravityResult.ResetAt == nil || !antigravityResult.ResetAt.Equal(resetAt) {
		t.Fatalf("ResetAt = %v, want %v", antigravityResult.ResetAt, resetAt)
	}
	if antigravityResult.RetryAfter == nil || *antigravityResult.RetryAfter != 2*time.Second {
		t.Fatalf("RetryAfter = %v, want 2s", antigravityResult.RetryAfter)
	}

	otherResult := NewUsageResult("other-auth", "openai", "gpt-5", http.StatusTooManyRequests, body)
	if otherResult.RetryAfter != nil || otherResult.ResetAt != nil {
		t.Fatalf("non-Antigravity result received retry hints: retryAfter=%v resetAt=%v", otherResult.RetryAfter, otherResult.ResetAt)
	}
}

func TestNewUsageResultParsesCodexUsageLimitHints(t *testing.T) {
	resetAt := time.Now().UTC().Add(72 * time.Hour).Truncate(time.Second)
	body := fmt.Sprintf(`{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached","resets_at":%d,"resets_in_seconds":326101}}`, resetAt.Unix())

	result := NewUsageResult("codex-auth", "codex", "gpt-5", http.StatusTooManyRequests, body)
	if result.ResetAt == nil || !result.ResetAt.Equal(resetAt) {
		t.Fatalf("ResetAt = %v, want %v", result.ResetAt, resetAt)
	}
	if result.RetryAfter == nil || *result.RetryAfter != 326101*time.Second {
		t.Fatalf("RetryAfter = %v, want 326101s", result.RetryAfter)
	}
}

func TestNewUsageResultScopesCodexUsageLimitHints(t *testing.T) {
	validBody := `{"error":{"type":"usage_limit_reached","resets_in_seconds":120}}`
	for _, tc := range []struct {
		name       string
		provider   string
		statusCode int
		body       string
	}{
		{name: "wrong provider", provider: "openai", statusCode: http.StatusTooManyRequests, body: validBody},
		{name: "wrong status code", provider: "codex", statusCode: http.StatusBadRequest, body: validBody},
		{name: "transient rate limit", provider: "codex", statusCode: http.StatusTooManyRequests, body: `{"error":{"type":"rate_limit_error","resets_in_seconds":120}}`},
		{name: "model capacity message", provider: "codex", statusCode: http.StatusTooManyRequests, body: `{"error":{"message":"selected model is at capacity","resets_in_seconds":120}}`},
		{name: "numeric string resets_in_seconds", provider: "codex", statusCode: http.StatusTooManyRequests, body: `{"error":{"type":"usage_limit_reached","resets_in_seconds":"120"}}`},
		{name: "fractional seconds", provider: "codex", statusCode: http.StatusTooManyRequests, body: `{"error":{"type":"usage_limit_reached","resets_in_seconds":1.5}}`},
		{name: "overflow seconds", provider: "codex", statusCode: http.StatusTooManyRequests, body: `{"error":{"type":"usage_limit_reached","resets_in_seconds":9223372037}}`},
		{name: "zero seconds", provider: "codex", statusCode: http.StatusTooManyRequests, body: `{"error":{"type":"usage_limit_reached","resets_in_seconds":0}}`},
		{name: "negative seconds", provider: "codex", statusCode: http.StatusTooManyRequests, body: `{"error":{"type":"usage_limit_reached","resets_in_seconds":-30}}`},
		{name: "malformed JSON", provider: "codex", statusCode: http.StatusTooManyRequests, body: `{"error":`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := NewUsageResult("auth", tc.provider, "gpt-5", tc.statusCode, tc.body)
			if result.RetryAfter != nil || result.ResetAt != nil {
				t.Fatalf("unexpected hints parsed: retryAfter=%v resetAt=%v", result.RetryAfter, result.ResetAt)
			}
		})
	}
}

func TestQuotaCooldownAfterFailureCodexPrecedenceAndFallback(t *testing.T) {
	now := time.Date(2026, 8, 4, 9, 2, 7, 0, time.UTC)
	futureReset := now.Add(72 * time.Hour)
	result := NewUsageResult("auth", "codex", "gpt-5", http.StatusTooManyRequests, `{"error":{"type":"usage_limit_reached","resets_at":`+strconv.FormatInt(futureReset.Unix(), 10)+`,"resets_in_seconds":30}}`)

	deadline, level := quotaCooldownAfterFailure(QuotaState{}, now, result)
	if !deadline.Equal(futureReset) {
		t.Fatalf("future resets_at deadline = %v, want %v", deadline, futureReset)
	}
	if level != 1 {
		t.Fatalf("level = %d, want 1", level)
	}

	staleReset := now.Add(-time.Hour)
	result = NewUsageResult("auth", "codex", "gpt-5", http.StatusTooManyRequests, `{"error":{"type":"usage_limit_reached","resets_at":`+strconv.FormatInt(staleReset.Unix(), 10)+`,"resets_in_seconds":77}}`)
	deadline, level = quotaCooldownAfterFailure(QuotaState{}, now, result)
	if !deadline.Equal(now.Add(77 * time.Second)) {
		t.Fatalf("stale resets_at fallback deadline = %v, want %v", deadline, now.Add(77*time.Second))
	}
	if level != 1 {
		t.Fatalf("level = %d, want 1", level)
	}

	nearFutureReset := now.Add(100 * time.Millisecond)
	result = Result{Provider: "codex", ResetAt: &nearFutureReset}
	deadline, level = quotaCooldownAfterFailure(QuotaState{}, now, result)
	if !deadline.Equal(now.Add(time.Second)) {
		t.Fatalf("near-future resets_at earlier than floor: deadline = %v, want 1s floor %v", deadline, now.Add(time.Second))
	}
	if level != 1 {
		t.Fatalf("level = %d, want 1", level)
	}
}

func TestQuotaCooldownAfterFailureEnforcesMaxHorizon(t *testing.T) {
	now := time.Date(2026, 8, 4, 9, 2, 7, 0, time.UTC)
	const maxHorizon = 60 * 24 * time.Hour

	// 1. ResetAt exactly at 60 days is accepted.
	exact60dReset := now.Add(maxHorizon)
	result := Result{Provider: "codex", ResetAt: &exact60dReset}
	deadline, level := quotaCooldownAfterFailure(QuotaState{}, now, result)
	if !deadline.Equal(exact60dReset) || level != 1 {
		t.Fatalf("exact 60d resetAt deadline = %v, want %v", deadline, exact60dReset)
	}

	// 2. ResetAt > 60 days (e.g. 61d or year 2262) is ignored and falls back to valid RetryAfter (3.8d).
	farFutureReset := now.Add(maxHorizon + 24*time.Hour)
	validRetry := 326101 * time.Second // ~3.8 days
	result = Result{Provider: "codex", ResetAt: &farFutureReset, RetryAfter: &validRetry}
	deadline, level = quotaCooldownAfterFailure(QuotaState{}, now, result)
	if !deadline.Equal(now.Add(validRetry)) || level != 1 {
		t.Fatalf("exceeded resetAt fallback deadline = %v, want %v", deadline, now.Add(validRetry))
	}

	// Year 2262 extreme timestamp ignored and falls back to valid RetryAfter.
	year2262 := time.Unix(9223372036, 0).UTC()
	result = Result{Provider: "codex", ResetAt: &year2262, RetryAfter: &validRetry}
	deadline, level = quotaCooldownAfterFailure(QuotaState{}, now, result)
	if !deadline.Equal(now.Add(validRetry)) || level != 1 {
		t.Fatalf("year 2262 resetAt fallback deadline = %v, want %v", deadline, now.Add(validRetry))
	}

	// 3. Both ResetAt and RetryAfter > 60 days are ignored -> falls back to exponential floor (1s).
	exceededRetry := maxHorizon + time.Second
	result = Result{Provider: "codex", ResetAt: &farFutureReset, RetryAfter: &exceededRetry}
	deadline, level = quotaCooldownAfterFailure(QuotaState{}, now, result)
	if !deadline.Equal(now.Add(time.Second)) || level != 1 {
		t.Fatalf("both exceeded deadline = %v, want 1s floor %v", deadline, now.Add(time.Second))
	}

	// 4. RetryAfter exactly at 60 days is accepted.
	exact60dRetry := maxHorizon
	result = Result{Provider: "codex", RetryAfter: &exact60dRetry}
	deadline, level = quotaCooldownAfterFailure(QuotaState{}, now, result)
	if !deadline.Equal(now.Add(exact60dRetry)) || level != 1 {
		t.Fatalf("exact 60d retryAfter deadline = %v, want %v", deadline, now.Add(exact60dRetry))
	}

	// 5. RetryAfter at 60 days + 1 second is rejected -> falls back to 1s exponential floor.
	result = Result{Provider: "codex", RetryAfter: &exceededRetry}
	deadline, level = quotaCooldownAfterFailure(QuotaState{}, now, result)
	if !deadline.Equal(now.Add(time.Second)) || level != 1 {
		t.Fatalf("exceeded retryAfter deadline = %v, want 1s floor %v", deadline, now.Add(time.Second))
	}

	// 6. Preexisting corrupted QuotaState with NextRecoverAt > 60 days is repaired on subsequent failure.
	corruptedState := QuotaState{
		Exceeded:      true,
		NextRecoverAt: now.Add(100 * 24 * time.Hour),
		BackoffLevel:  1,
	}
	validHint := 4 * 24 * time.Hour
	result = Result{Provider: "codex", RetryAfter: &validHint}
	deadline, level = quotaCooldownAfterFailure(corruptedState, now, result)
	if !deadline.Equal(now.Add(validHint)) || level != 2 {
		t.Fatalf("corrupted window repair deadline = %v level = %d, want %v level 2", deadline, level, now.Add(validHint))
	}

	// Preexisting corrupted QuotaState without hint is repaired by fallback exponential cooldown.
	deadline, level = quotaCooldownAfterFailure(corruptedState, now, Result{Provider: "codex"})
	if !deadline.Equal(now.Add(2*time.Second)) || level != 2 {
		t.Fatalf("corrupted window repair without hint deadline = %v level = %d, want %v level 2", deadline, level, now.Add(2*time.Second))
	}
}

func TestNewUsageResultIgnoresAntigravityQuotaResetDelayAndMessage(t *testing.T) {
	body := `{"error":{"code":429,"message":"Individual quota reached. Resets in 45m.","status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","metadata":{"quotaResetDelay":"45m"}}]}}`

	result := NewUsageResult("antigravity-auth", "antigravity", "gemini-3.7-flash-high", http.StatusTooManyRequests, body)
	if result.RetryAfter != nil || result.ResetAt != nil {
		t.Fatalf("ignored retry fields produced hints: retryAfter=%v resetAt=%v", result.RetryAfter, result.ResetAt)
	}
}

func TestNewUsageResultRequiresMatchingGoogleRetryDetailTypes(t *testing.T) {
	resetAt := time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)
	body := `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","retryDelay":"2s"},{"@type":"type.googleapis.com/google.rpc.RetryInfo","metadata":{"quotaResetTimeStamp":"` + resetAt + `"}}]}}`

	result := NewUsageResult("antigravity-auth", "antigravity", "gemini-3.7-flash-high", http.StatusTooManyRequests, body)
	if result.RetryAfter != nil || result.ResetAt != nil {
		t.Fatalf("mismatched detail types produced hints: retryAfter=%v resetAt=%v", result.RetryAfter, result.ResetAt)
	}
}

func TestMarkResultIgnoresMissingCanonicalModel(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	auth := &Auth{ID: "auth-missing-model", Index: "auth-missing-model", Provider: "codex", Status: StatusActive}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}

	manager.MarkResult(context.Background(), Result{
		AuthID:  auth.ID,
		Model:   " ",
		Success: false,
		Error:   &Error{Message: "quota", HTTPStatus: http.StatusTooManyRequests},
	})
	manager.MarkResult(context.Background(), Result{AuthID: auth.ID, Success: true})

	updated, ok := manager.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatal("updated auth not found")
	}
	if updated.Success != 0 || updated.Failed != 0 || updated.Status != StatusActive || updated.Unavailable || updated.Quota.Exceeded || len(updated.ModelStates) != 0 {
		t.Fatalf("model-less result changed auth: %#v", updated)
	}
}

func TestAuthRefreshBackoffDoesNotBlockModels(t *testing.T) {
	now := time.Now().UTC()
	auth := &Auth{
		ID:               "auth-refresh-backoff",
		Provider:         "codex",
		Status:           StatusActive,
		NextRefreshAfter: now.Add(refreshFailureBackoff),
	}

	refreshRetryAt := auth.NextRefreshAfter
	NewManager(nil, nil, nil).applyResultTransition(auth, Result{AuthID: auth.ID, Model: "gpt-5", Success: true}, "gpt-5", now, false)
	if !auth.NextRefreshAfter.Equal(refreshRetryAt) || !RefreshRetryBackoffOpen(auth, now) {
		t.Fatalf("successful usage shortened refresh backoff: got %v, want %v", auth.NextRefreshAfter, refreshRetryAt)
	}
	blocked, reason, next := isAuthBlockedForModel(auth, "gpt-5", now)
	if blocked || reason != blockReasonNone || !next.IsZero() {
		t.Fatalf("isAuthBlockedForModel() = %v, %v, %v; want dispatchable", blocked, reason, next)
	}
}
