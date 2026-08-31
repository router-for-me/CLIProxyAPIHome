package auth

import (
	"context"
	"math"
	"net/http"
	"testing"
	"time"
)

func quotaResult(authID, model string) Result {
	return Result{
		AuthID:   authID,
		Provider: "codex",
		Model:    model,
		Success:  false,
		Error: &Error{
			Message:    "quota",
			Retryable:  true,
			HTTPStatus: http.StatusTooManyRequests,
		},
	}
}

func TestMarkResultQuotaBackoffEscalatesOncePerWindow(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	auth := &Auth{
		ID:       "auth-quota-window",
		Provider: "codex",
	}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("Register returned error: %v", errRegister)
	}

	manager.MarkResult(context.Background(), quotaResult(auth.ID, "gpt-5"))
	first, ok := manager.GetByID(auth.ID)
	if !ok || first == nil || first.ModelStates["gpt-5"] == nil {
		t.Fatalf("expected model state after first failure")
	}
	firstState := first.ModelStates["gpt-5"]
	if firstState.Quota.BackoffLevel != 1 {
		t.Fatalf("expected BackoffLevel 1 after first failure, got %d", firstState.Quota.BackoffLevel)
	}
	if !firstState.Quota.NextRecoverAt.After(time.Now()) {
		t.Fatalf("expected open cooldown window after first failure, got %v", firstState.Quota.NextRecoverAt)
	}

	// A second in-flight failure lands while the first window is still open.
	manager.MarkResult(context.Background(), quotaResult(auth.ID, "gpt-5"))
	second, ok := manager.GetByID(auth.ID)
	if !ok || second == nil || second.ModelStates["gpt-5"] == nil {
		t.Fatalf("expected model state after second failure")
	}
	secondState := second.ModelStates["gpt-5"]
	if secondState.Quota.BackoffLevel != 1 {
		t.Fatalf("expected BackoffLevel to stay 1 for in-window failure, got %d", secondState.Quota.BackoffLevel)
	}
	if !secondState.Quota.NextRecoverAt.Equal(firstState.Quota.NextRecoverAt) {
		t.Fatalf("expected NextRecoverAt to stay %v for in-window failure, got %v", firstState.Quota.NextRecoverAt, secondState.Quota.NextRecoverAt)
	}
	if !secondState.NextRetryAfter.Equal(firstState.NextRetryAfter) {
		t.Fatalf("expected NextRetryAfter to stay %v for in-window failure, got %v", firstState.NextRetryAfter, secondState.NextRetryAfter)
	}
}

func TestMarkResultQuotaBackoffEscalatesAfterWindowExpiry(t *testing.T) {
	expired := time.Now().Add(-time.Second)
	manager := NewManager(nil, nil, nil)
	auth := &Auth{
		ID:       "auth-quota-expired",
		Provider: "codex",
		ModelStates: map[string]*ModelState{
			"gpt-5": {
				Status:         StatusError,
				Unavailable:    true,
				NextRetryAfter: expired,
				Quota:          QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: expired, BackoffLevel: 3},
			},
		},
	}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("Register returned error: %v", errRegister)
	}

	manager.MarkResult(context.Background(), quotaResult(auth.ID, "gpt-5"))
	updated, ok := manager.GetByID(auth.ID)
	if !ok || updated == nil || updated.ModelStates["gpt-5"] == nil {
		t.Fatalf("expected model state after failure")
	}
	state := updated.ModelStates["gpt-5"]
	if state.Quota.BackoffLevel != 4 {
		t.Fatalf("expected BackoffLevel 4 after post-window failure, got %d", state.Quota.BackoffLevel)
	}
	if !state.Quota.NextRecoverAt.After(time.Now()) {
		t.Fatalf("expected a fresh cooldown window, got %v", state.Quota.NextRecoverAt)
	}
}

func TestQuotaCooldownAfterFailureKeepsBackoffFloorForShortRetryDelay(t *testing.T) {
	now := time.Date(2026, time.August, 29, 0, 0, 0, 0, time.UTC)
	retryAfter := time.Second
	for _, provider := range []string{"antigravity", "codex"} {
		result := Result{
			Provider:   provider,
			RetryAfter: &retryAfter,
		}
		quota := QuotaState{}
		for index, wantDelay := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second} {
			deadline, level := quotaCooldownAfterFailure(quota, now, result)
			if delay := deadline.Sub(now); delay != wantDelay {
				t.Fatalf("%s failure %d delay = %v, want %v", provider, index+1, delay, wantDelay)
			}
			if wantLevel := index + 1; level != wantLevel {
				t.Fatalf("%s failure %d BackoffLevel = %d, want %d", provider, index+1, level, wantLevel)
			}
			quota.NextRecoverAt = deadline
			quota.BackoffLevel = level
			now = deadline.Add(time.Nanosecond)
		}
	}
}

func TestQuotaCooldownAfterFailureCodexOpenWindowExtendsAndNeverShortens(t *testing.T) {
	now := time.Date(2026, 8, 4, 9, 2, 7, 0, time.UTC)
	open := QuotaState{
		Exceeded:      true,
		NextRecoverAt: now.Add(time.Hour),
		BackoffLevel:  3,
	}

	short := 5 * time.Second
	deadline, level := quotaCooldownAfterFailure(open, now, Result{Provider: "codex", RetryAfter: &short})
	if !deadline.Equal(open.NextRecoverAt) || level != 3 {
		t.Fatalf("shorter delay changed open window: deadline = %v, level = %d", deadline, level)
	}

	long := 72 * time.Hour
	deadline, level = quotaCooldownAfterFailure(open, now, Result{Provider: "codex", RetryAfter: &long})
	if !deadline.Equal(now.Add(long)) || level != 3 {
		t.Fatalf("longer delay failed to extend open window: deadline = %v, level = %d", deadline, level)
	}

	futureReset := now.Add(96 * time.Hour)
	deadline, level = quotaCooldownAfterFailure(open, now, Result{Provider: "codex", ResetAt: &futureReset})
	if !deadline.Equal(futureReset) || level != 3 {
		t.Fatalf("longer reset timestamp failed to extend open window: deadline = %v, level = %d", deadline, level)
	}
}

func TestNextQuotaCooldownLadder(t *testing.T) {
	cases := []struct {
		prevLevel    int
		wantCooldown time.Duration
		wantLevel    int
	}{
		{-3, time.Second, 1},
		{0, time.Second, 1},
		{1, 2 * time.Second, 2},
		{5, 32 * time.Second, 6},
		{10, 1024 * time.Second, 11},
		{11, quotaBackoffMax, 11},
		{20, quotaBackoffMax, 20},
		{62, quotaBackoffMax, 62},
		{63, quotaBackoffMax, 63},
		{64, quotaBackoffMax, 64},
		{100, quotaBackoffMax, 100},
		{math.MaxInt, quotaBackoffMax, math.MaxInt},
	}
	for _, tc := range cases {
		cooldown, level := nextQuotaCooldown(tc.prevLevel, false)
		if cooldown != tc.wantCooldown || level != tc.wantLevel {
			t.Fatalf("nextQuotaCooldown(%d) = (%v, %d), want (%v, %d)", tc.prevLevel, cooldown, level, tc.wantCooldown, tc.wantLevel)
		}
	}

	if cooldown, level := nextQuotaCooldown(4, true); cooldown != 0 || level != 4 {
		t.Fatalf("nextQuotaCooldown with cooling disabled = (%v, %d), want (0, 4)", cooldown, level)
	}
}
