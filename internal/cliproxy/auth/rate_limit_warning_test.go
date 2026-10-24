package auth

import (
	"net/http"
	"testing"
	"time"
)

// TestParseClaudeRateLimitWarningsMarksWarnedWindow proves an
// allowed_warning status on the 5h window surfaces as a RateLimitWarning for
// that window on a successful response.
func TestParseClaudeRateLimitWarningsMarksWarnedWindow(t *testing.T) {
	headers := claudeHeaders(map[string]string{
		"Anthropic-Ratelimit-Unified-5h-Status": "allowed_warning",
	})
	result := NewUsageResultWithHeaders("auth-1", "claude", "claude-opus-4-1", http.StatusOK, "", headers)
	if !result.Success {
		t.Fatalf("Success = false, want true for a 200")
	}
	warning, ok := result.RateLimitWarnings["5h"]
	if !ok {
		t.Fatalf("RateLimitWarnings = %#v, want an entry for window 5h", result.RateLimitWarnings)
	}
	if warning.Window != "5h" {
		t.Fatalf("warning.Window = %q, want 5h", warning.Window)
	}
	if result.RateLimitAllClear {
		t.Fatalf("RateLimitAllClear = true, want false (no unsuffixed allowed header was sent)")
	}
}

// TestClaude7dOiHeaderCanonicalizesAndIsParsed proves the 7d_oi window
// survives http.Header's canonicalization round-trip: canonicalization only
// upper-cases the first letter after a hyphen, and "_" is not a hyphen, so
// the canonical key stays "Anthropic-Ratelimit-Unified-7d_oi-Status" (lower
// "d" and "oi" preserved). This is the literal canonical map key -- not just
// what Get() resolves -- since a bug here would make the whole feature
// silently inert for this window.
func TestClaude7dOiHeaderCanonicalizesAndIsParsed(t *testing.T) {
	headers := claudeHeaders(map[string]string{
		"Anthropic-Ratelimit-Unified-7d_oi-Status": "allowed_warning",
	})
	const canonicalKey = "Anthropic-Ratelimit-Unified-7d_oi-Status"
	values, ok := headers[canonicalKey]
	if !ok || len(values) != 1 || values[0] != "allowed_warning" {
		t.Fatalf("headers[%q] = %#v, ok=%v, want [\"allowed_warning\"], true (canonicalization changed the key)", canonicalKey, values, ok)
	}

	result := NewUsageResultWithHeaders("auth-1", "claude", "claude-opus-4-1", http.StatusOK, "", headers)
	if _, ok := result.RateLimitWarnings["7d_oi"]; !ok {
		t.Fatalf("RateLimitWarnings = %#v, want an entry for window 7d_oi", result.RateLimitWarnings)
	}
}

// TestGetAvailableAuthsPrefersNonWarnedCredential proves the selector
// de-prefers a warned credential in favour of a clean one at the same
// priority.
func TestGetAvailableAuthsPrefersNonWarnedCredential(t *testing.T) {
	now := time.Date(2026, time.September, 9, 0, 0, 0, 0, time.UTC)
	warned := &Auth{
		ID:     "a-warned",
		Status: StatusActive,
		RateLimitWarnings: map[string]RateLimitWarning{
			"5h": {Window: "5h", ResetAt: now.Add(20 * time.Minute)},
		},
	}
	clean := &Auth{ID: "b-clean", Status: StatusActive}

	available, err := getAvailableAuths([]*Auth{warned, clean}, "claude", "claude-opus-4-1", now)
	if err != nil {
		t.Fatalf("getAvailableAuths error = %v, want nil", err)
	}
	if len(available) != 1 || available[0].ID != "b-clean" {
		t.Fatalf("available = %#v, want only b-clean", available)
	}
}

// TestGetAvailableAuthsNeverStarvesSoleWarnedCredential proves the warned
// credential is still returned, never an error, when it is the only
// candidate.
func TestGetAvailableAuthsNeverStarvesSoleWarnedCredential(t *testing.T) {
	now := time.Date(2026, time.September, 9, 0, 0, 0, 0, time.UTC)
	warned := &Auth{
		ID:     "a-warned",
		Status: StatusActive,
		RateLimitWarnings: map[string]RateLimitWarning{
			"5h": {Window: "5h", ResetAt: now.Add(20 * time.Minute)},
		},
	}

	available, err := getAvailableAuths([]*Auth{warned}, "claude", "claude-opus-4-1", now)
	if err != nil {
		t.Fatalf("getAvailableAuths error = %v, want nil (never-starve)", err)
	}
	if len(available) != 1 || available[0].ID != "a-warned" {
		t.Fatalf("available = %#v, want the sole warned credential", available)
	}
}

// TestGetAvailableAuthsReturnsWarnedWhenAllCandidatesWarned proves that when
// every candidate at the best priority is warned, the selector still
// returns them (sorted by ID) rather than erroring.
func TestGetAvailableAuthsReturnsWarnedWhenAllCandidatesWarned(t *testing.T) {
	now := time.Date(2026, time.September, 9, 0, 0, 0, 0, time.UTC)
	warning := map[string]RateLimitWarning{"5h": {Window: "5h", ResetAt: now.Add(20 * time.Minute)}}
	a := &Auth{ID: "b-second", Status: StatusActive, RateLimitWarnings: warning}
	b := &Auth{ID: "a-first", Status: StatusActive, RateLimitWarnings: warning}

	available, err := getAvailableAuths([]*Auth{a, b}, "claude", "claude-opus-4-1", now)
	if err != nil {
		t.Fatalf("getAvailableAuths error = %v, want nil", err)
	}
	if len(available) != 2 {
		t.Fatalf("available = %#v, want both warned candidates", available)
	}
	if available[0].ID != "a-first" || available[1].ID != "b-second" {
		t.Fatalf("available IDs = [%s %s], want sorted [a-first b-second]", available[0].ID, available[1].ID)
	}
}

// TestApplyRateLimitWarningTransitionAllClearWipesEveryWindow proves an
// affirmative unsuffixed Unified-Status: allowed clears every previously
// recorded window, not just the ones present on the response.
func TestApplyRateLimitWarningTransitionAllClearWipesEveryWindow(t *testing.T) {
	now := time.Date(2026, time.September, 9, 0, 0, 0, 0, time.UTC)
	auth := &Auth{
		ID:     "auth-all-clear",
		Status: StatusActive,
		RateLimitWarnings: map[string]RateLimitWarning{
			"5h": {Window: "5h", ResetAt: now.Add(20 * time.Minute)},
			"7d": {Window: "7d", ResetAt: now.Add(2 * time.Hour)},
		},
	}
	result := Result{Provider: "claude", Success: true, RateLimitAllClear: true}

	NewManager(nil, nil, nil).applyResultTransition(auth, result, "claude-opus-4-1", now, false)

	if len(auth.RateLimitWarnings) != 0 {
		t.Fatalf("RateLimitWarnings = %#v, want empty after an all-clear", auth.RateLimitWarnings)
	}
}

// TestApplyRateLimitWarningTransitionDropsExpiredWindow proves a warning
// whose ResetAt has already passed is swept away on the next success, even
// when that success carries no incoming warning for the window at all.
func TestApplyRateLimitWarningTransitionDropsExpiredWindow(t *testing.T) {
	now := time.Date(2026, time.September, 9, 0, 0, 0, 0, time.UTC)
	auth := &Auth{
		ID:     "auth-expired",
		Status: StatusActive,
		RateLimitWarnings: map[string]RateLimitWarning{
			"5h": {Window: "5h", ResetAt: now.Add(-time.Minute)},
		},
	}
	result := Result{Provider: "claude", Success: true}

	NewManager(nil, nil, nil).applyResultTransition(auth, result, "claude-opus-4-1", now, false)

	if _, ok := auth.RateLimitWarnings["5h"]; ok {
		t.Fatalf("RateLimitWarnings[5h] still present, want swept once its ResetAt passed")
	}
}

// TestIsAuthBlockedForModelDisabledCredentialNeverSelected proves a
// Disabled credential is never a selection candidate regardless of warning
// state.
func TestIsAuthBlockedForModelDisabledCredentialNeverSelected(t *testing.T) {
	now := time.Date(2026, time.September, 9, 0, 0, 0, 0, time.UTC)
	disabled := &Auth{
		ID:       "a-disabled",
		Status:   StatusActive,
		Disabled: true,
		RateLimitWarnings: map[string]RateLimitWarning{
			"5h": {Window: "5h", ResetAt: now.Add(20 * time.Minute)},
		},
	}

	_, err := getAvailableAuths([]*Auth{disabled}, "claude", "claude-opus-4-1", now)
	if err == nil {
		t.Fatalf("getAvailableAuths error = nil, want an error (only candidate is Disabled)")
	}
}

// TestResultNeedsGlobalTransitionOnNewRateLimitWarning is the ANTI-INERT A
// regression: a success carrying a brand-new warning has no other
// "clearable availability state", so without rateLimitWarningsWouldChange
// this would return false, the StateMutator would never engage, and the
// warning would never persist.
func TestResultNeedsGlobalTransitionOnNewRateLimitWarning(t *testing.T) {
	now := time.Date(2026, time.September, 9, 0, 0, 0, 0, time.UTC)
	auth := &Auth{ID: "auth-anti-inert-a", Status: StatusActive}
	result := Result{
		Provider: "claude",
		Success:  true,
		RateLimitWarnings: map[string]RateLimitWarning{
			"5h": {Window: "5h", ResetAt: now.Add(20 * time.Minute)},
		},
	}

	if !NewManager(nil, nil, nil).resultNeedsGlobalTransition(auth, result, "claude-opus-4-1", now, false) {
		t.Fatalf("resultNeedsGlobalTransition = false, want true for a success carrying a new warning")
	}
}

// TestAvailabilityFingerprintChangesOnWarningNotOnObservedAt is the
// ANTI-INERT B regression: availabilityFingerprintValue must stay
// ==-comparable, so the warning state is folded into a scalar
// warningsDigest. The digest must differ once a warning is actually
// applied (proving the write is not silently discarded by MutateAuthState's
// unchanged-fingerprint guard) and must stay equal across two successes
// that only refresh ObservedAt (proving ObservedAt is excluded, so a
// healthy warned credential does not hammer the store on every request).
func TestAvailabilityFingerprintChangesOnWarningNotOnObservedAt(t *testing.T) {
	now := time.Date(2026, time.September, 9, 0, 0, 0, 0, time.UTC)
	auth := &Auth{ID: "auth-anti-inert-b", Status: StatusActive}

	before := availabilityFingerprint(auth, "claude-opus-4-1")

	firstResult := Result{
		Provider: "claude",
		Success:  true,
		RateLimitWarnings: map[string]RateLimitWarning{
			"5h": {Window: "5h", ResetAt: now.Add(20 * time.Minute)},
		},
	}
	NewManager(nil, nil, nil).applyResultTransition(auth, firstResult, "claude-opus-4-1", now, false)
	afterWarning := availabilityFingerprint(auth, "claude-opus-4-1")

	if before == afterWarning {
		t.Fatalf("fingerprint unchanged after applying a new warning, want it to differ")
	}

	later := now.Add(time.Minute)
	secondResult := Result{
		Provider: "claude",
		Success:  true,
		RateLimitWarnings: map[string]RateLimitWarning{
			"5h": {Window: "5h", ResetAt: now.Add(20 * time.Minute)},
		},
	}
	NewManager(nil, nil, nil).applyResultTransition(auth, secondResult, "claude-opus-4-1", later, false)
	afterRefresh := availabilityFingerprint(auth, "claude-opus-4-1")

	if afterWarning != afterRefresh {
		t.Fatalf("fingerprint changed across a same-window refresh (ObservedAt only), want it unchanged: %#v vs %#v", afterWarning, afterRefresh)
	}
}

// TestParseClaudeRateLimitWarningsIgnoresNonClaudeProvider proves the
// provider gate: an identical allowed_warning header set on any provider
// other than "claude" must not produce a warning, so no other provider's
// behavior can change.
func TestParseClaudeRateLimitWarningsIgnoresNonClaudeProvider(t *testing.T) {
	headers := claudeHeaders(map[string]string{
		"Anthropic-Ratelimit-Unified-5h-Status": "allowed_warning",
		"Anthropic-Ratelimit-Unified-Status":    "allowed",
	})
	result := NewUsageResultWithHeaders("auth-1", "openai", "gpt-5.5", http.StatusOK, "", headers)
	if result.RateLimitWarnings != nil {
		t.Fatalf("RateLimitWarnings = %#v, want nil for a non-claude provider", result.RateLimitWarnings)
	}
	if result.RateLimitAllClear {
		t.Fatalf("RateLimitAllClear = true, want false for a non-claude provider")
	}
}

// TestAuthCloneDoesNotAliasRateLimitWarnings proves Clone deep-copies the
// RateLimitWarnings map: mutating the clone must never mutate the original,
// which would otherwise bleed warning state across cloned auth snapshots on
// different nodes.
func TestAuthCloneDoesNotAliasRateLimitWarnings(t *testing.T) {
	now := time.Date(2026, time.September, 9, 0, 0, 0, 0, time.UTC)
	original := &Auth{
		ID:     "auth-clone",
		Status: StatusActive,
		RateLimitWarnings: map[string]RateLimitWarning{
			"5h": {Window: "5h", ResetAt: now.Add(20 * time.Minute)},
		},
	}

	clone := original.Clone()
	clone.RateLimitWarnings["5h"] = RateLimitWarning{Window: "5h", ResetAt: now.Add(999 * time.Hour)}
	clone.RateLimitWarnings["7d"] = RateLimitWarning{Window: "7d", ResetAt: now.Add(time.Hour)}

	if len(original.RateLimitWarnings) != 1 {
		t.Fatalf("original.RateLimitWarnings = %#v, want unchanged (len 1)", original.RateLimitWarnings)
	}
	originalWindow := original.RateLimitWarnings["5h"]
	if !originalWindow.ResetAt.Equal(now.Add(20 * time.Minute)) {
		t.Fatalf("original 5h ResetAt = %v, want unchanged at %v", originalWindow.ResetAt, now.Add(20*time.Minute))
	}
}

// TestApplyRateLimitWarningTransitionPerWindowAllowedClearsOnlyThatWindow is
// the Task 1 headline regression: a window whose header explicitly reports
// "allowed" clears ONLY that window's stale mark, even when a DIFFERENT
// window newly warns on the very same response and the unsuffixed status
// therefore still reads allowed_warning -- so the wholesale all-clear path
// (RateLimitAllClear) never fires and cannot be relied on to save this case.
func TestApplyRateLimitWarningTransitionPerWindowAllowedClearsOnlyThatWindow(t *testing.T) {
	now := time.Date(2026, time.September, 9, 0, 0, 0, 0, time.UTC)
	auth := &Auth{
		ID:     "auth-per-window-clear",
		Status: StatusActive,
		RateLimitWarnings: map[string]RateLimitWarning{
			"7d_oi": {Window: "7d_oi", ResetAt: now.Add(3 * 24 * time.Hour)},
		},
	}

	headers := claudeHeaders(map[string]string{
		"Anthropic-Ratelimit-Unified-Status":       "allowed_warning",
		"Anthropic-Ratelimit-Unified-5h-Status":    "allowed_warning",
		"Anthropic-Ratelimit-Unified-7d_oi-Status": "allowed",
	})
	result := NewUsageResultWithHeaders("auth-1", "claude", "claude-opus-4-1", http.StatusOK, "", headers)
	if result.RateLimitAllClear {
		t.Fatalf("RateLimitAllClear = true, want false (unsuffixed status is allowed_warning, not allowed)")
	}
	if _, ok := result.RateLimitWarnings["7d_oi"]; ok {
		t.Fatalf("RateLimitWarnings[7d_oi] = present, want absent -- 7d_oi's header is an explicit allowed, not allowed_warning")
	}

	applyRateLimitWarningTransition(auth, result, now)

	if _, ok := auth.RateLimitWarnings["7d_oi"]; ok {
		t.Fatalf("RateLimitWarnings[7d_oi] still present after transition, want cleared by its explicit allowed status")
	}
	warning5h, ok := auth.RateLimitWarnings["5h"]
	if !ok {
		t.Fatalf("RateLimitWarnings[5h] missing after transition, want it set from this response's allowed_warning")
	}
	if warning5h.Window != "5h" {
		t.Fatalf("warning5h.Window = %q, want 5h", warning5h.Window)
	}
}

// TestApplyRateLimitWarningTransitionRejectedDoesNotClear proves a window
// reporting "rejected" -- worse than warned, never better -- is excluded
// from RateLimitClearedWindows at the parse boundary and never clears an
// existing mark when applied.
func TestApplyRateLimitWarningTransitionRejectedDoesNotClear(t *testing.T) {
	now := time.Date(2026, time.September, 9, 0, 0, 0, 0, time.UTC)
	auth := &Auth{
		ID:     "auth-rejected-no-clear",
		Status: StatusActive,
		RateLimitWarnings: map[string]RateLimitWarning{
			"5h": {Window: "5h", ResetAt: now.Add(20 * time.Minute)},
		},
	}
	headers := claudeHeaders(map[string]string{
		"Anthropic-Ratelimit-Unified-5h-Status": "rejected",
	})
	result := NewUsageResultWithHeaders("auth-1", "claude", "claude-opus-4-1", http.StatusOK, "", headers)
	if len(result.RateLimitClearedWindows) != 0 {
		t.Fatalf("RateLimitClearedWindows = %#v, want empty -- a rejected window must never clear", result.RateLimitClearedWindows)
	}

	applyRateLimitWarningTransition(auth, result, now)
	if _, ok := auth.RateLimitWarnings["5h"]; !ok {
		t.Fatalf("RateLimitWarnings[5h] cleared, want it to remain -- a rejected window must never clear an existing mark")
	}
}

// TestApplyRateLimitWarningTransitionAbsentWindowDoesNotClear proves an
// ABSENT per-window header is never read as an all-clear for that window:
// only an explicit "allowed" status clears, absence stays a no-op.
func TestApplyRateLimitWarningTransitionAbsentWindowDoesNotClear(t *testing.T) {
	now := time.Date(2026, time.September, 9, 0, 0, 0, 0, time.UTC)
	auth := &Auth{
		ID:     "auth-absent-no-clear",
		Status: StatusActive,
		RateLimitWarnings: map[string]RateLimitWarning{
			"7d": {Window: "7d", ResetAt: now.Add(48 * time.Hour)},
		},
	}
	headers := claudeHeaders(map[string]string{
		"Anthropic-Ratelimit-Unified-5h-Status": "allowed_warning",
	})
	result := NewUsageResultWithHeaders("auth-1", "claude", "claude-opus-4-1", http.StatusOK, "", headers)
	if len(result.RateLimitClearedWindows) != 0 {
		t.Fatalf("RateLimitClearedWindows = %#v, want empty when 7d's header is absent from this response", result.RateLimitClearedWindows)
	}

	applyRateLimitWarningTransition(auth, result, now)
	if _, ok := auth.RateLimitWarnings["7d"]; !ok {
		t.Fatalf("RateLimitWarnings[7d] cleared, want it to remain -- an absent header must never clear a live mark")
	}
}

// TestResultNeedsGlobalTransitionOnClearedWindow is the Task 1.4 anti-inert
// guard: a response that ONLY clears a window (no new warnings, nothing
// expired) must still be reported as needing a global transition, or the
// clear is computed by applyRateLimitWarningTransition and then silently
// discarded by MutateAuthState's unchanged-fingerprint guard.
func TestResultNeedsGlobalTransitionOnClearedWindow(t *testing.T) {
	now := time.Date(2026, time.September, 9, 0, 0, 0, 0, time.UTC)
	auth := &Auth{
		ID:     "auth-clear-only",
		Status: StatusActive,
		RateLimitWarnings: map[string]RateLimitWarning{
			"7d_oi": {Window: "7d_oi", ResetAt: now.Add(3 * 24 * time.Hour)},
		},
	}
	result := Result{
		Provider:                "claude",
		Success:                 true,
		RateLimitClearedWindows: []string{"7d_oi"},
	}
	if !NewManager(nil, nil, nil).resultNeedsGlobalTransition(auth, result, "claude-opus-4-1", now, false) {
		t.Fatalf("resultNeedsGlobalTransition = false, want true for a response that only clears a live window")
	}
}

// TestAuthRateLimitWarnedKeyedLookupCases proves the Task 2 rewrite (a keyed
// lookup over the fixed claudeRateLimitWarningWindows array instead of a
// map range) still behaves identically across the warned / not-warned /
// expired-only cases.
func TestAuthRateLimitWarnedKeyedLookupCases(t *testing.T) {
	now := time.Date(2026, time.September, 9, 0, 0, 0, 0, time.UTC)

	notWarned := &Auth{ID: "not-warned", Status: StatusActive}
	if authRateLimitWarned(notWarned, now) {
		t.Fatalf("authRateLimitWarned(notWarned) = true, want false for a credential with no warnings")
	}

	warned := &Auth{
		ID:     "warned",
		Status: StatusActive,
		RateLimitWarnings: map[string]RateLimitWarning{
			"5h": {Window: "5h", ResetAt: now.Add(20 * time.Minute)},
		},
	}
	if !authRateLimitWarned(warned, now) {
		t.Fatalf("authRateLimitWarned(warned) = false, want true for a live 5h warning")
	}

	expiredOnly := &Auth{
		ID:     "expired-only",
		Status: StatusActive,
		RateLimitWarnings: map[string]RateLimitWarning{
			"7d": {Window: "7d", ResetAt: now.Add(-time.Minute)},
		},
	}
	if authRateLimitWarned(expiredOnly, now) {
		t.Fatalf("authRateLimitWarned(expiredOnly) = true, want false -- its only window's ResetAt has already passed")
	}
}
