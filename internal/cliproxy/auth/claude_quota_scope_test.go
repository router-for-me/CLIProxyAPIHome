package auth

import (
	"strconv"
	"testing"
	"time"
)

// TestQuotaCooldownAfterFailureGateIsHintPresenceNotProviderAllowlist proves
// the A3 change: quotaCooldownAfterFailure honours a reset hint based on
// whether one was produced, not on a hardcoded provider allowlist. Since
// parseUsageRetryHints returns (nil, nil, false) for any provider without an
// explicit case, this cannot change behavior for antigravity/codex -- it
// only widens the set of providers that CAN reach this point once they have
// their own case in parseUsageRetryHints (today, just "claude").
func TestQuotaCooldownAfterFailureGateIsHintPresenceNotProviderAllowlist(t *testing.T) {
	now := time.Date(2026, time.September, 9, 0, 0, 0, 0, time.UTC)

	// An unrecognized provider with an explicit hint on the Result (bypassing
	// parseUsageRetryHints entirely, same as the antigravity/codex hand-built
	// literals in cooldown_backoff_test.go) is honoured under the new
	// hint-presence gate. Under the old provider-string gate this would have
	// been rejected.
	reset := now.Add(4 * time.Hour)
	result := Result{Provider: "some-future-provider", ResetAt: &reset}
	deadline, level := quotaCooldownAfterFailure(QuotaState{}, now, result)
	if !deadline.Equal(reset) {
		t.Fatalf("deadline = %v, want %v (provider-agnostic hint gate)", deadline, reset)
	}
	if level != 1 {
		t.Fatalf("level = %d, want 1 (the exponential ladder's first level, computed before the ResetAt override -- ResetAt only overrides the deadline, not the level)", level)
	}

	// An unrecognized provider with no hint at all still falls through to the
	// plain exponential floor, unchanged.
	deadline, level = quotaCooldownAfterFailure(QuotaState{}, now, Result{Provider: "some-future-provider"})
	if delay := deadline.Sub(now); delay != time.Second {
		t.Fatalf("delay = %v, want 1s exponential floor for a hint-less unknown provider", delay)
	}
	if level != 1 {
		t.Fatalf("level = %d, want 1", level)
	}
}

// TestApplyResultTransitionClaudeCredentialScopeBlocksSiblingModelStates
// proves A4 (i) and (ii): a Claude 429 whose Result carries CredentialScope
// blocks every ModelState on the auth, not just the one that failed, and (b)
// the aggregation (updateAggregatedAvailability, called at the end of the
// failure path) then derives auth.Unavailable=true/auth.NextRetryAfter from
// that fan-out -- proving the design works WITH the aggregation rather than
// being clobbered by it (the lead correction in
// .ws/lead-verified-home-facts.md).
func TestApplyResultTransitionClaudeCredentialScopeBlocksSiblingModelStates(t *testing.T) {
	now := time.Date(2026, time.September, 9, 0, 0, 0, 0, time.UTC)
	reset := now.Add(4 * time.Hour)

	auth := &Auth{
		ID:       "auth-claude-credential",
		Provider: "claude",
		Status:   StatusActive,
		ModelStates: map[string]*ModelState{
			// A sibling model that has never failed and must be blocked by
			// the fan-out even though the 429 landed on claude-opus.
			"claude-3-7-sonnet": {Status: StatusActive},
		},
	}

	result := Result{
		AuthID:          auth.ID,
		Provider:        "claude",
		Model:           "claude-opus-4-1",
		Success:         false,
		ResetAt:         &reset,
		CredentialScope: true,
		Error:           &Error{Message: "rate limited", HTTPStatus: 429},
	}

	NewManager(nil, nil, nil).applyResultTransition(auth, result, "claude-opus-4-1", now, false)

	failedState := auth.ModelStates["claude-opus-4-1"]
	if failedState == nil || !failedState.Unavailable || !failedState.NextRetryAfter.Equal(reset) {
		t.Fatalf("failed model state = %#v, want Unavailable with NextRetryAfter %v", failedState, reset)
	}

	siblingState := auth.ModelStates["claude-3-7-sonnet"]
	if siblingState == nil || !siblingState.Unavailable {
		t.Fatalf("sibling model state = %#v, want Unavailable (fan-out did not cover it)", siblingState)
	}
	if !siblingState.NextRetryAfter.Equal(reset) {
		t.Fatalf("sibling NextRetryAfter = %v, want %v", siblingState.NextRetryAfter, reset)
	}
	if !siblingState.Quota.Exceeded || !siblingState.Quota.NextRecoverAt.Equal(reset) {
		t.Fatalf("sibling Quota = %#v, want Exceeded with NextRecoverAt %v", siblingState.Quota, reset)
	}

	blocked, _, next := isAuthBlockedForModel(auth, "claude-3-7-sonnet", now)
	if !blocked || !next.Equal(reset) {
		t.Fatalf("isAuthBlockedForModel(sonnet) = blocked %v next %v, want blocked at %v", blocked, next, reset)
	}

	// The aggregation must, by itself (no direct write from the 429 branch),
	// derive auth.Unavailable / auth.NextRetryAfter now that every
	// ModelState is unavailable.
	if !auth.Unavailable {
		t.Fatalf("auth.Unavailable = false, want true once every ModelState is blocked")
	}
	if !auth.NextRetryAfter.Equal(reset) {
		t.Fatalf("auth.NextRetryAfter = %v, want %v", auth.NextRetryAfter, reset)
	}
}

// TestApplyResultTransitionClaudeModelScopedRejectionDoesNotFanOut is the
// negative case: a Claude 429 without CredentialScope (e.g. a plain
// model-specific rate limit, or a Fable-only 7d_oi rejection) must behave
// exactly like today -- only the failing model is affected.
func TestApplyResultTransitionClaudeModelScopedRejectionDoesNotFanOut(t *testing.T) {
	now := time.Date(2026, time.September, 9, 0, 0, 0, 0, time.UTC)

	auth := &Auth{
		ID:       "auth-claude-model-scoped",
		Provider: "claude",
		Status:   StatusActive,
		ModelStates: map[string]*ModelState{
			"claude-3-7-sonnet": {Status: StatusActive},
		},
	}

	result := Result{
		AuthID:          auth.ID,
		Provider:        "claude",
		Model:           "claude-opus-4-1",
		Success:         false,
		CredentialScope: false,
		Error:           &Error{Message: "rate limited", HTTPStatus: 429},
	}

	NewManager(nil, nil, nil).applyResultTransition(auth, result, "claude-opus-4-1", now, false)

	siblingState := auth.ModelStates["claude-3-7-sonnet"]
	if siblingState == nil || siblingState.Unavailable {
		t.Fatalf("sibling model state = %#v, want untouched (still available)", siblingState)
	}
	if auth.Unavailable {
		t.Fatalf("auth.Unavailable = true, want false since not every model is blocked")
	}
}

// TestApplyResultTransitionRespectsDisableCoolingForCredentialScope proves
// the fan-out never fires under disableCooling -- it lives inside the same
// `else` branch as the rest of the 429-with-cooling-enabled logic, but this
// is asserted explicitly per the brief's instruction to respect
// disableCooling identically to existing code.
func TestApplyResultTransitionRespectsDisableCoolingForCredentialScope(t *testing.T) {
	now := time.Date(2026, time.September, 9, 0, 0, 0, 0, time.UTC)
	reset := now.Add(4 * time.Hour)

	auth := &Auth{
		ID:       "auth-claude-disable-cooling",
		Provider: "claude",
		Status:   StatusActive,
		ModelStates: map[string]*ModelState{
			"claude-3-7-sonnet": {Status: StatusActive},
		},
	}

	result := Result{
		AuthID:          auth.ID,
		Provider:        "claude",
		Model:           "claude-opus-4-1",
		Success:         false,
		ResetAt:         &reset,
		CredentialScope: true,
		Error:           &Error{Message: "rate limited", HTTPStatus: 429},
	}

	NewManager(nil, nil, nil).applyResultTransition(auth, result, "claude-opus-4-1", now, true /* disableCooling */)

	siblingState := auth.ModelStates["claude-3-7-sonnet"]
	if siblingState == nil || siblingState.Unavailable {
		t.Fatalf("sibling model state = %#v, want untouched under disableCooling", siblingState)
	}
	failedState := auth.ModelStates["claude-opus-4-1"]
	if failedState == nil || failedState.Unavailable {
		t.Fatalf("failed model state = %#v, want Unavailable=false under disableCooling", failedState)
	}
}

// TestQuotaCooldownAfterFailureClaudeResetHintDoesNotReArmTheOneSecondLadder is the
// charter-mandated regression test (A5-iii): a Claude 429 with a reset hint
// ~4h out must NOT re-arm the model-scoped 1s->30m exponential ladder --
// confirmed by directly comparing against the exponential floor value that
// the existing (pre-fix) codepath would have produced.
func TestQuotaCooldownAfterFailureClaudeResetHintDoesNotReArmTheOneSecondLadder(t *testing.T) {
	now := time.Date(2026, time.September, 9, 0, 0, 0, 0, time.UTC)
	reset := now.Add(4 * time.Hour)
	result := Result{Provider: "claude", ResetAt: &reset, CredentialScope: true}

	deadline, level := quotaCooldownAfterFailure(QuotaState{}, now, result)
	if !deadline.Equal(reset) {
		t.Fatalf("deadline = %v, want the 4h reset hint %v (not the exponential ladder)", deadline, reset)
	}
	if delay := deadline.Sub(now); delay == time.Second {
		t.Fatalf("deadline landed exactly on the 1s exponential floor -- the reset hint was NOT honoured")
	}
	// level is unaffected by an honoured ResetAt (matches antigravity/codex
	// behavior in TestQuotaCooldownAfterFailureCodexOpenWindowExtendsAndNeverShortens).
	_ = level
}

// TestParseUsageRetryHintsClaudeCredentialScopeEndToEnd exercises the full
// pipeline (parseUsageRetryHints -> NewUsageResultWithHeaders) with a
// realistic 5h-window rejection, asserting the resulting Result both carries
// the reset and is marked credential-scoped -- the exact shape the fix must
// deliver for a live Claude 429.
func TestParseUsageRetryHintsClaudeCredentialScopeEndToEnd(t *testing.T) {
	now := time.Now().UTC()
	reset := now.Add(4 * time.Hour)
	headers := claudeHeaders(map[string]string{
		"Anthropic-Ratelimit-Unified-5h-Status": "rejected",
		"Anthropic-Ratelimit-Unified-5h-Reset":  strconv.FormatInt(reset.Unix(), 10),
	})

	result := NewUsageResultWithHeaders("claude-auth", "claude", "claude-opus-4-1", 429, "", headers)
	if !result.CredentialScope {
		t.Fatalf("result.CredentialScope = false, want true")
	}
	if result.ResetAt == nil || result.ResetAt.Unix() != reset.Unix() {
		t.Fatalf("result.ResetAt = %v, want %v", result.ResetAt, reset)
	}
}
