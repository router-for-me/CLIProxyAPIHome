package auth

import (
	"net/http"
	"testing"
	"time"
)

// applyFailureWithCode records a failed execution result carrying an error code.
func applyFailureWithCode(auth *Auth, model string, status int, code, message string, now time.Time) {
	NewManager(nil, nil, nil).applyResultTransition(auth, Result{
		AuthID:  auth.ID,
		Model:   model,
		Success: false,
		Error:   &Error{Code: code, Message: message, HTTPStatus: status},
	}, model, now)
}

// TestInvalidGrantSuspendsModelForThirtyMinutes keeps a revoked grant off the
// dispatch path instead of retrying it every minute on the transient path.
func TestInvalidGrantSuspendsModelForThirtyMinutes(t *testing.T) {
	now := time.Now().UTC()
	auth := failingAuth("auth-invalid-grant", false)

	applyFailureWithCode(auth, "gpt-5", http.StatusBadRequest, "invalid_grant", "token has been revoked", now)

	state := auth.ModelStates["gpt-5"]
	if state == nil {
		t.Fatal("model state was not recorded")
	}
	if !state.NextRetryAfter.Equal(now.Add(30 * time.Minute)) {
		t.Fatalf("NextRetryAfter = %v, want %v", state.NextRetryAfter, now.Add(30*time.Minute))
	}
	if !state.Unavailable {
		t.Fatal("invalid_grant left the model available")
	}
	if blocked, reason, _ := isAuthBlockedForModel(auth, "gpt-5", now); !blocked || reason != blockReasonOther {
		t.Fatalf("blocked/reason = %v/%v, want a non-quota block", blocked, reason)
	}
}

// TestInvalidGrantMatchesMessageWithoutCode covers providers that only surface the
// reason in the message body.
func TestInvalidGrantMatchesMessageWithoutCode(t *testing.T) {
	now := time.Now().UTC()
	auth := failingAuth("auth-invalid-grant-msg", false)

	applyFailure(auth, "gpt-5", http.StatusUnauthorized, `{"error":"invalid_grant"}`, now)

	state := auth.ModelStates["gpt-5"]
	if state == nil || !state.NextRetryAfter.Equal(now.Add(30*time.Minute)) {
		t.Fatalf("state = %#v, want a 30m invalid_grant cooldown", state)
	}
}

// TestInvalidGrantIgnoredForUnrelatedStatus keeps the long cooldown scoped to the
// statuses that actually carry a grant failure.
func TestInvalidGrantIgnoredForUnrelatedStatus(t *testing.T) {
	now := time.Now().UTC()
	auth := failingAuth("auth-invalid-grant-5xx", false)

	applyFailureWithCode(auth, "gpt-5", http.StatusInternalServerError, "invalid_grant", "upstream blew up", now)

	state := auth.ModelStates["gpt-5"]
	if state == nil {
		t.Fatal("model state was not recorded")
	}
	if !state.NextRetryAfter.Equal(now.Add(transientErrorCooldown)) {
		t.Fatalf("NextRetryAfter = %v, want the transient cooldown %v", state.NextRetryAfter, now.Add(transientErrorCooldown))
	}
}

// TestInvalidGrantWithDisableCoolingStaysAvailable keeps disable_cooling retryable
// rather than leaving an unbounded unavailable flag.
func TestInvalidGrantWithDisableCoolingStaysAvailable(t *testing.T) {
	now := time.Now().UTC()
	auth := failingAuth("auth-invalid-grant-nocool", true)

	applyFailureWithCode(auth, "gpt-5", http.StatusBadRequest, "invalid_grant", "token has been revoked", now)

	state := auth.ModelStates["gpt-5"]
	if state == nil {
		t.Fatal("model state was not recorded")
	}
	if !state.NextRetryAfter.IsZero() || state.Unavailable {
		t.Fatalf("state = %#v, want no cooldown and available", state)
	}
}

// TestCloudflareChallengeUsesQuotaBackoffWithFloor keeps a challenge page from being
// retried in a tight loop.
func TestCloudflareChallengeUsesQuotaBackoffWithFloor(t *testing.T) {
	now := time.Now().UTC()
	auth := failingAuth("auth-cloudflare", false)

	applyFailure(auth, "gpt-5", http.StatusForbidden, "<html>cf-mitigated challenge</html>", now)

	state := auth.ModelStates["gpt-5"]
	if state == nil {
		t.Fatal("model state was not recorded")
	}
	if !state.Quota.Exceeded || state.Quota.Reason != cloudflareChallengeReason {
		t.Fatalf("quota = %#v, want a cloudflare challenge cooldown", state.Quota)
	}
	if state.StatusMessage != cloudflareChallengeReason {
		t.Fatalf("StatusMessage = %q, want %q", state.StatusMessage, cloudflareChallengeReason)
	}
	minDeadline := now.Add(cloudflareChallengeMinCooldown)
	if state.NextRetryAfter.Before(minDeadline) {
		t.Fatalf("NextRetryAfter = %v, want at least %v", state.NextRetryAfter, minDeadline)
	}
	if !state.Quota.NextRecoverAt.Equal(state.NextRetryAfter) {
		t.Fatalf("NextRecoverAt = %v, want %v", state.Quota.NextRecoverAt, state.NextRetryAfter)
	}
	if blocked, reason, _ := isAuthBlockedForModel(auth, "gpt-5", now); !blocked || reason != blockReasonCooldown {
		t.Fatalf("blocked/reason = %v/%v, want blockReasonCooldown", blocked, reason)
	}
}

// TestCloudflareChallengeEscalatesBackoffLevel keeps repeated challenges backing off.
func TestCloudflareChallengeEscalatesBackoffLevel(t *testing.T) {
	now := time.Now().UTC()
	auth := failingAuth("auth-cloudflare-escalate", false)

	applyFailure(auth, "gpt-5", http.StatusForbidden, "cloudflare challenge", now)
	first := auth.ModelStates["gpt-5"].Quota
	if first.BackoffLevel == 0 {
		t.Fatalf("first BackoffLevel = %d, want the ladder to advance", first.BackoffLevel)
	}

	// The open window must be reused rather than restarted while it is still live.
	applyFailure(auth, "gpt-5", http.StatusForbidden, "cloudflare challenge", now.Add(time.Second))
	second := auth.ModelStates["gpt-5"].Quota
	if second.BackoffLevel < first.BackoffLevel {
		t.Fatalf("BackoffLevel regressed: %d -> %d", first.BackoffLevel, second.BackoffLevel)
	}
	if second.NextRecoverAt.Before(first.NextRecoverAt) {
		t.Fatalf("NextRecoverAt moved earlier: %v -> %v", first.NextRecoverAt, second.NextRecoverAt)
	}
}

// TestCloudflareChallengeWithDisableCoolingStaysAvailable keeps disable_cooling from
// leaving a quota flag nothing can expire.
func TestCloudflareChallengeWithDisableCoolingStaysAvailable(t *testing.T) {
	now := time.Now().UTC()
	auth := failingAuth("auth-cloudflare-nocool", true)

	applyFailure(auth, "gpt-5", http.StatusForbidden, "cf-mitigated", now)

	state := auth.ModelStates["gpt-5"]
	if state == nil {
		t.Fatal("model state was not recorded")
	}
	if !state.NextRetryAfter.IsZero() || state.Unavailable || state.Quota.Exceeded {
		t.Fatalf("state = %#v, want no cooldown and available", state)
	}
}

// TestCloudflareChallengeIgnoresUnrelatedMessage keeps ordinary 403 responses on the
// payment_required path.
func TestCloudflareChallengeIgnoresUnrelatedMessage(t *testing.T) {
	now := time.Now().UTC()
	auth := failingAuth("auth-plain-403", false)

	applyFailure(auth, "gpt-5", http.StatusForbidden, "permission denied", now)

	state := auth.ModelStates["gpt-5"]
	if state == nil {
		t.Fatal("model state was not recorded")
	}
	if state.Quota.Exceeded {
		t.Fatalf("quota = %#v, want no quota cooldown for a plain 403", state.Quota)
	}
	if !state.NextRetryAfter.Equal(now.Add(30 * time.Minute)) {
		t.Fatalf("NextRetryAfter = %v, want the 30m payment_required cooldown", state.NextRetryAfter)
	}
}
