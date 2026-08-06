package auth

import (
	"net/http"
	"testing"
	"time"
)

// failingAuth builds a credential ready to receive a failure transition.
func failingAuth(id string, disableCooling bool) *Auth {
	auth := &Auth{
		ID:       id,
		Provider: "antigravity",
		Status:   StatusActive,
	}
	if disableCooling {
		auth.Metadata = map[string]any{"disable_cooling": true}
	}
	return auth
}

// applyFailure records a failed execution result against the credential.
func applyFailure(auth *Auth, model string, status int, message string, now time.Time) {
	NewManager(nil, nil, nil).applyResultTransition(auth, Result{
		AuthID:  auth.ID,
		Model:   model,
		Success: false,
		Error:   &Error{Message: message, HTTPStatus: status},
	}, model, now)
}

// TestQuotaFailureWithDisableCoolingClearsUnboundedState reproduces the state
// observed on the credential behind the production 503: a quota failure on a
// credential with disable_cooling used to leave unavailable and quota-exceeded
// set with no recovery deadline at all.
func TestQuotaFailureWithDisableCoolingClearsUnboundedState(t *testing.T) {
	now := time.Now().UTC()
	auth := failingAuth("auth-quota-nocool", true)

	applyFailure(auth, "gemini-3-flash-agent", http.StatusTooManyRequests, "quota exceeded", now)

	state := auth.ModelStates["gemini-3-flash-agent"]
	if state == nil {
		t.Fatal("model state was not recorded")
	}
	if !state.NextRetryAfter.IsZero() || !state.Quota.NextRecoverAt.IsZero() {
		t.Fatalf("disable_cooling produced deadlines: retry=%v recover=%v", state.NextRetryAfter, state.Quota.NextRecoverAt)
	}
	if state.Unavailable || state.Quota.Exceeded {
		t.Fatalf("unbounded flags survived: unavailable=%v quotaExceeded=%v", state.Unavailable, state.Quota.Exceeded)
	}
	if state.LastError == nil || state.Status != StatusError {
		t.Fatalf("failure detail was dropped: status=%v lastError=%v", state.Status, state.LastError)
	}
	if blocked, _, _ := isAuthBlockedForModel(auth, "gemini-3-flash-agent", now); blocked {
		t.Fatal("disable_cooling credential was blocked despite having no cooldown")
	}
}

// TestUnmappedStatusParksModelBriefly covers status codes with no dedicated
// branch: they must cool down long enough to move dispatch onto another
// credential, then recover without operator action.
func TestUnmappedStatusParksModelBriefly(t *testing.T) {
	now := time.Now().UTC()
	auth := failingAuth("auth-unmapped", false)

	applyFailure(auth, "gpt-5", http.StatusConflict, "conflict", now)

	state := auth.ModelStates["gpt-5"]
	if state == nil {
		t.Fatal("model state was not recorded")
	}
	if !state.NextRetryAfter.Equal(now.Add(transientErrorCooldown)) {
		t.Fatalf("NextRetryAfter = %v, want %v", state.NextRetryAfter, now.Add(transientErrorCooldown))
	}
	if !state.Unavailable {
		t.Fatal("unmapped failure left the model available")
	}
	if blocked, reason, _ := isAuthBlockedForModel(auth, "gpt-5", now); !blocked || reason != blockReasonOther {
		t.Fatalf("blocked/reason = %v/%v, want a non-quota block during the cooldown", blocked, reason)
	}
	after := now.Add(transientErrorCooldown + time.Second)
	if blocked, _, _ := isAuthBlockedForModel(auth, "gpt-5", after); blocked {
		t.Fatal("model stayed blocked after the cooldown elapsed")
	}
}

// TestUnmappedStatusWithDisableCoolingStaysAvailable keeps disable_cooling from
// parking a credential on an unmapped status code.
func TestUnmappedStatusWithDisableCoolingStaysAvailable(t *testing.T) {
	now := time.Now().UTC()
	auth := failingAuth("auth-unmapped-nocool", true)

	applyFailure(auth, "gpt-5", http.StatusBadRequest, "bad request", now)

	state := auth.ModelStates["gpt-5"]
	if state == nil {
		t.Fatal("model state was not recorded")
	}
	if !state.NextRetryAfter.IsZero() || state.Unavailable {
		t.Fatalf("state = %#v, want no cooldown and available", state)
	}
	if blocked, _, _ := isAuthBlockedForModel(auth, "gpt-5", now); blocked {
		t.Fatal("disable_cooling credential was blocked on an unmapped status")
	}
}

// TestTransientFailureBlocksForOneMinute pins the 408/5xx cooldown window.
func TestTransientFailureBlocksForOneMinute(t *testing.T) {
	now := time.Now().UTC()
	auth := failingAuth("auth-transient", false)

	applyFailure(auth, "gpt-5", http.StatusBadGateway, "bad gateway", now)

	state := auth.ModelStates["gpt-5"]
	if state == nil {
		t.Fatal("model state was not recorded")
	}
	if !state.NextRetryAfter.Equal(now.Add(transientErrorCooldown)) {
		t.Fatalf("NextRetryAfter = %v, want %v", state.NextRetryAfter, now.Add(transientErrorCooldown))
	}
	if !state.Unavailable {
		t.Fatal("transient failure left the model available")
	}
	if blocked, _, next := isAuthBlockedForModel(auth, "gpt-5", now); !blocked || !next.Equal(state.NextRetryAfter) {
		t.Fatalf("blocked/next = %v/%v, want a block until %v", blocked, next, state.NextRetryAfter)
	}
}

// TestTransientFailureWithDisableCoolingStaysAvailable keeps disable_cooling
// retryable on 408/5xx instead of leaving an unbounded unavailable flag.
func TestTransientFailureWithDisableCoolingStaysAvailable(t *testing.T) {
	now := time.Now().UTC()
	auth := failingAuth("auth-transient-nocool", true)

	applyFailure(auth, "gpt-5", http.StatusServiceUnavailable, "service unavailable", now)

	state := auth.ModelStates["gpt-5"]
	if state == nil {
		t.Fatal("model state was not recorded")
	}
	if !state.NextRetryAfter.IsZero() || state.Unavailable {
		t.Fatalf("state = %#v, want no cooldown and available", state)
	}
	if blocked, _, _ := isAuthBlockedForModel(auth, "gpt-5", now); blocked {
		t.Fatal("disable_cooling credential was blocked on a transient failure")
	}
}

// TestQuotaFailureWithCoolingKeepsQuotaWindow guards the normal quota path: the
// disable_cooling cleanup must not weaken a real cooldown.
func TestQuotaFailureWithCoolingKeepsQuotaWindow(t *testing.T) {
	now := time.Now().UTC()
	auth := failingAuth("auth-quota-cool", false)

	applyFailure(auth, "gpt-5", http.StatusTooManyRequests, "quota exceeded", now)

	state := auth.ModelStates["gpt-5"]
	if state == nil {
		t.Fatal("model state was not recorded")
	}
	if !state.Quota.Exceeded || !state.Unavailable {
		t.Fatalf("state = %#v, want a quota cooldown", state)
	}
	if !state.Quota.NextRecoverAt.After(now) {
		t.Fatalf("NextRecoverAt = %v, want a future quota window", state.Quota.NextRecoverAt)
	}
	if blocked, reason, _ := isAuthBlockedForModel(auth, "gpt-5", now); !blocked || reason != blockReasonCooldown {
		t.Fatalf("blocked/reason = %v/%v, want blockReasonCooldown", blocked, reason)
	}
}

// TestModelNotSupportedKeepsLongCooldown ensures the disable_cooling cleanup runs
// after the model-support branch and cannot erase its 12h deadline.
func TestModelNotSupportedKeepsLongCooldown(t *testing.T) {
	now := time.Now().UTC()
	auth := failingAuth("auth-unsupported-nocool", true)

	applyFailure(auth, "gpt-5", http.StatusBadRequest, "requested model is not supported", now)

	state := auth.ModelStates["gpt-5"]
	if state == nil {
		t.Fatal("model state was not recorded")
	}
	if !state.NextRetryAfter.Equal(now.Add(12 * time.Hour)) {
		t.Fatalf("NextRetryAfter = %v, want a 12h cooldown", state.NextRetryAfter)
	}
	if !state.Unavailable {
		t.Fatal("unsupported model stayed available")
	}
}
