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

	// 1. Short future reset timestamp (<= 30m) is respected exactly.
	shortReset := now.Add(10 * time.Minute)
	result := NewUsageResult("auth", "codex", "gpt-5", http.StatusTooManyRequests, `{"error":{"type":"usage_limit_reached","resets_at":`+strconv.FormatInt(shortReset.Unix(), 10)+`,"resets_in_seconds":30}}`)
	deadline, level := quotaCooldownAfterFailure(QuotaState{}, now, result)
	if !deadline.Equal(shortReset) {
		t.Fatalf("short resets_at deadline = %v, want %v", deadline, shortReset)
	}
	if level != 1 {
		t.Fatalf("level = %d, want 1", level)
	}

	// 2. Long future reset timestamp (> 30m) is clamped to the 30m horizon.
	longReset := now.Add(72 * time.Hour)
	result = NewUsageResult("auth", "codex", "gpt-5", http.StatusTooManyRequests, `{"error":{"type":"usage_limit_reached","resets_at":`+strconv.FormatInt(longReset.Unix(), 10)+`,"resets_in_seconds":30}}`)
	deadline, level = quotaCooldownAfterFailure(QuotaState{}, now, result)
	if !deadline.Equal(now.Add(30 * time.Minute)) {
		t.Fatalf("long resets_at deadline = %v, want clamped 30m %v", deadline, now.Add(30*time.Minute))
	}
	if level != 1 {
		t.Fatalf("level = %d, want 1", level)
	}

	// 3. Stale reset timestamp in the past falls back to resets_in_seconds.
	staleReset := now.Add(-time.Hour)
	result = NewUsageResult("auth", "codex", "gpt-5", http.StatusTooManyRequests, `{"error":{"type":"usage_limit_reached","resets_at":`+strconv.FormatInt(staleReset.Unix(), 10)+`,"resets_in_seconds":77}}`)
	deadline, level = quotaCooldownAfterFailure(QuotaState{}, now, result)
	if !deadline.Equal(now.Add(77 * time.Second)) {
		t.Fatalf("stale resets_at fallback deadline = %v, want %v", deadline, now.Add(77*time.Second))
	}
	if level != 1 {
		t.Fatalf("level = %d, want 1", level)
	}

	// 4. Near-future reset timestamp earlier than the 1s exponential floor preserves the 1s floor.
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
	const defaultHorizon = 60 * 24 * time.Hour
	const codexHorizon = 30 * time.Minute

	// --- Codex Horizon Tests (Clamped to 30m) ---

	// 1. Codex: Short hint (10m <= 30m) is accepted exactly.
	codexShortReset := now.Add(10 * time.Minute)
	result := Result{Provider: "codex", ResetAt: &codexShortReset}
	deadline, level := quotaCooldownAfterFailure(QuotaState{}, now, result)
	if !deadline.Equal(codexShortReset) || level != 1 {
		t.Fatalf("codex 10m resetAt deadline = %v, want %v", deadline, codexShortReset)
	}

	// 2. Codex: Long hint (72h > 30m) is clamped to now + 30m.
	codex72hReset := now.Add(72 * time.Hour)
	result = Result{Provider: "codex", ResetAt: &codex72hReset}
	deadline, level = quotaCooldownAfterFailure(QuotaState{}, now, result)
	if !deadline.Equal(now.Add(codexHorizon)) || level != 1 {
		t.Fatalf("codex 72h resetAt deadline = %v, want clamped 30m %v", deadline, now.Add(codexHorizon))
	}
	// 3. Codex: Extreme year 2262 timestamp clamped to now + 30m.
	year2262 := time.Unix(9223372036, 0).UTC()
	result = Result{Provider: "codex", ResetAt: &year2262}
	deadline, level = quotaCooldownAfterFailure(QuotaState{}, now, result)
	if !deadline.Equal(now.Add(codexHorizon)) || level != 1 {
		t.Fatalf("codex year 2262 deadline = %v, want clamped 30m %v", deadline, now.Add(codexHorizon))
	}

	// 4. Codex: RetryAfter at 45m is clamped to now + 30m.
	codex45mRetry := 45 * time.Minute
	result = Result{Provider: "codex", RetryAfter: &codex45mRetry}
	deadline, level = quotaCooldownAfterFailure(QuotaState{}, now, result)
	if !deadline.Equal(now.Add(codexHorizon)) || level != 1 {
		t.Fatalf("codex 45m retryAfter deadline = %v, want clamped 30m %v", deadline, now.Add(codexHorizon))
	}

	// 5. Codex: Corrupted QuotaState with NextRecoverAt > 30m is repaired on subsequent failure.
	corruptedCodexState := QuotaState{
		Exceeded:      true,
		NextRecoverAt: now.Add(5 * time.Hour),
		BackoffLevel:  1,
	}
	validCodexHint := 10 * time.Minute
	result = Result{Provider: "codex", RetryAfter: &validCodexHint}
	deadline, level = quotaCooldownAfterFailure(corruptedCodexState, now, result)
	if !deadline.Equal(now.Add(validCodexHint)) || level != 2 {
		t.Fatalf("corrupted codex window repair deadline = %v level = %d, want %v level 2", deadline, level, now.Add(validCodexHint))
	}

	// Corrupted QuotaState without hint is repaired by fallback exponential cooldown (level 1 -> 2: 2s).
	deadline, level = quotaCooldownAfterFailure(corruptedCodexState, now, Result{Provider: "codex"})
	if !deadline.Equal(now.Add(2*time.Second)) || level != 2 {
		t.Fatalf("corrupted codex window repair without hint deadline = %v level = %d, want %v level 2", deadline, level, now.Add(2*time.Second))
	}

	// --- Antigravity Horizon Tests (Clamped to 60 days) ---

	// 6. Antigravity: ResetAt exactly at 60 days is accepted.
	exact60dReset := now.Add(defaultHorizon)
	result = Result{Provider: "antigravity", ResetAt: &exact60dReset}
	deadline, level = quotaCooldownAfterFailure(QuotaState{}, now, result)
	if !deadline.Equal(exact60dReset) || level != 1 {
		t.Fatalf("antigravity exact 60d resetAt deadline = %v, want %v", deadline, exact60dReset)
	}

	// 7. Antigravity: ResetAt > 60 days (e.g. 90d or year 2262) is clamped to now + 60 days.
	farFutureReset := now.Add(90 * 24 * time.Hour)
	result = Result{Provider: "antigravity", ResetAt: &farFutureReset}
	deadline, level = quotaCooldownAfterFailure(QuotaState{}, now, result)
	if !deadline.Equal(now.Add(defaultHorizon)) || level != 1 {
		t.Fatalf("antigravity far future resetAt deadline = %v, want clamped 60d %v", deadline, now.Add(defaultHorizon))
	}

	// 8. Antigravity: RetryAfter at 90 days is clamped to now + 60 days.
	retry90d := 90 * 24 * time.Hour
	result = Result{Provider: "antigravity", RetryAfter: &retry90d}
	deadline, level = quotaCooldownAfterFailure(QuotaState{}, now, result)
	if !deadline.Equal(now.Add(defaultHorizon)) || level != 1 {
		t.Fatalf("antigravity 90d retryAfter deadline = %v, want clamped 60d %v", deadline, now.Add(defaultHorizon))
	}

	// 9. Antigravity: Corrupted QuotaState with NextRecoverAt > 60 days is repaired on subsequent failure.
	corruptedAntigravityState := QuotaState{
		Exceeded:      true,
		NextRecoverAt: now.Add(100 * 24 * time.Hour),
		BackoffLevel:  1,
	}
	validAntigravityHint := 4 * 24 * time.Hour
	result = Result{Provider: "antigravity", RetryAfter: &validAntigravityHint}
	deadline, level = quotaCooldownAfterFailure(corruptedAntigravityState, now, result)
	if !deadline.Equal(now.Add(validAntigravityHint)) || level != 2 {
		t.Fatalf("corrupted antigravity window repair deadline = %v level = %d, want %v level 2", deadline, level, now.Add(validAntigravityHint))
	}
}

func TestMarkResultDerivesHorizonFromAuthoritativeAuthProvider(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	codexAuth := &Auth{ID: "auth-codex-mismatch", Index: "auth-codex-mismatch", Provider: "codex", Status: StatusActive}
	antigravityAuth := &Auth{ID: "auth-antigravity-mismatch", Index: "auth-antigravity-mismatch", Provider: "antigravity", Status: StatusActive}
	if _, errRegister := manager.Register(context.Background(), codexAuth); errRegister != nil {
		t.Fatalf("Register() codex error = %v", errRegister)
	}
	if _, errRegister := manager.Register(context.Background(), antigravityAuth); errRegister != nil {
		t.Fatalf("Register() antigravity error = %v", errRegister)
	}

	twoHours := 2 * time.Hour
	before := time.Now()

	// 1. Codex auth receiving a result labeled "antigravity" with 2h retry hint is clamped to 30m.
	manager.MarkResult(context.Background(), Result{
		AuthID:     codexAuth.ID,
		Provider:   "antigravity",
		Model:      "gpt-5",
		Success:    false,
		Error:      &Error{Message: "quota", HTTPStatus: http.StatusTooManyRequests},
		RetryAfter: &twoHours,
	})

	gotCodex, ok := manager.GetByID(codexAuth.ID)
	if !ok || gotCodex == nil || gotCodex.ModelStates["gpt-5"] == nil {
		t.Fatalf("codex auth state missing: %#v", gotCodex)
	}
	codexDelay := gotCodex.ModelStates["gpt-5"].NextRetryAfter.Sub(before)
	if codexDelay < 29*time.Minute || codexDelay > 31*time.Minute {
		t.Fatalf("codex NextRetryAfter = %v (delay %v), want clamped to ~30m", gotCodex.ModelStates["gpt-5"].NextRetryAfter, codexDelay)
	}

	// 2. Antigravity auth receiving a result labeled "codex" with 2h retry hint is NOT clamped to 30m.
	manager.MarkResult(context.Background(), Result{
		AuthID:     antigravityAuth.ID,
		Provider:   "codex",
		Model:      "gemini-3.7-flash",
		Success:    false,
		Error:      &Error{Message: "quota", HTTPStatus: http.StatusTooManyRequests},
		RetryAfter: &twoHours,
	})

	gotAntigravity, ok := manager.GetByID(antigravityAuth.ID)
	if !ok || gotAntigravity == nil || gotAntigravity.ModelStates["gemini-3.7-flash"] == nil {
		t.Fatalf("antigravity auth state missing: %#v", gotAntigravity)
	}
	antigravityDelay := gotAntigravity.ModelStates["gemini-3.7-flash"].NextRetryAfter.Sub(before)
	if antigravityDelay < 119*time.Minute || antigravityDelay > 121*time.Minute {
		t.Fatalf("antigravity NextRetryAfter = %v (delay %v), want ~2h", gotAntigravity.ModelStates["gemini-3.7-flash"].NextRetryAfter, antigravityDelay)
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
