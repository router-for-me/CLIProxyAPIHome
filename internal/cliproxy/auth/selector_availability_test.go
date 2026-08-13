package auth

import (
	"testing"
	"time"
)

// modelStateAuth builds a credential carrying a single model state.
func modelStateAuth(id string, priority string, model string, state *ModelState) *Auth {
	auth := &Auth{
		ID:          id,
		Provider:    "antigravity",
		Status:      StatusActive,
		ModelStates: map[string]*ModelState{model: state},
	}
	if priority != "" {
		auth.Attributes = map[string]string{"priority": priority}
	}
	return auth
}

// TestZeroDeadlineUnavailableModelStaysDispatchable pins the fail-open rule: a state
// with no recovery deadline is something nothing can expire, so it must read as
// recovered rather than park the credential. Legacy rows and cluster merge artifacts
// reach dispatch in exactly this shape.
func TestZeroDeadlineUnavailableModelStaysDispatchable(t *testing.T) {
	now := time.Now().UTC()
	auth := modelStateAuth("auth-zero-deadline", "", "gemini-3-flash-agent", &ModelState{
		Status:      StatusError,
		Unavailable: true,
		Quota:       QuotaState{Exceeded: true, Scope: quotaScopeModel, Reason: "quota"},
	})

	blocked, reason, next := isAuthBlockedForModel(auth, "gemini-3-flash-agent", now)
	if blocked {
		t.Fatal("a state with no recovery deadline parked the credential")
	}
	if reason != blockReasonNone || !next.IsZero() {
		t.Fatalf("reason/next = %v/%v, want an unblocked verdict", reason, next)
	}
}

// TestQuotaExceededWithOpenWindowBlocksDispatch covers state written by cluster merges
// or the Management API where the quota flag carries the window and the unavailable
// flag was never set.
func TestQuotaExceededWithOpenWindowBlocksDispatch(t *testing.T) {
	now := time.Now().UTC()
	recoverAt := now.Add(10 * time.Minute)
	auth := modelStateAuth("auth-quota-only", "", "gpt-5", &ModelState{
		Status: StatusError,
		Quota:  QuotaState{Exceeded: true, Scope: quotaScopeModel, Reason: "quota", NextRecoverAt: recoverAt},
	})

	blocked, reason, next := isAuthBlockedForModel(auth, "gpt-5", now)
	if !blocked {
		t.Fatal("quota-exceeded model with an open window was reported dispatchable")
	}
	if reason != blockReasonCooldown {
		t.Fatalf("reason = %v, want blockReasonCooldown", reason)
	}
	if !next.Equal(recoverAt) {
		t.Fatalf("next = %v, want %v", next, recoverAt)
	}
}

// TestQuotaExceededWithoutDeadlineStaysDispatchable keeps a quota flag that lost its
// window from becoming a state nothing can expire.
func TestQuotaExceededWithoutDeadlineStaysDispatchable(t *testing.T) {
	now := time.Now().UTC()
	auth := modelStateAuth("auth-quota-no-window", "", "gpt-5", &ModelState{
		Status: StatusError,
		Quota:  QuotaState{Exceeded: true, Scope: quotaScopeModel, Reason: "quota"},
	})

	if blocked, _, _ := isAuthBlockedForModel(auth, "gpt-5", now); blocked {
		t.Fatal("quota flag without a recovery window parked the credential")
	}
}

// TestOpenQuotaWindowBlocksAfterRetryDeadlinePasses covers a transient failure
// overwriting NextRetryAfter while the longer quota window is still open.
func TestOpenQuotaWindowBlocksAfterRetryDeadlinePasses(t *testing.T) {
	now := time.Now().UTC()
	recoverAt := now.Add(25 * time.Minute)
	auth := modelStateAuth("auth-open-quota", "", "gpt-5", &ModelState{
		Status:         StatusError,
		Unavailable:    true,
		NextRetryAfter: now.Add(-time.Minute),
		Quota:          QuotaState{Exceeded: true, Scope: quotaScopeModel, Reason: "quota", NextRecoverAt: recoverAt},
	})

	blocked, reason, next := isAuthBlockedForModel(auth, "gpt-5", now)
	if !blocked {
		t.Fatal("expired retry deadline released a credential whose quota window is still open")
	}
	if reason != blockReasonCooldown {
		t.Fatalf("reason = %v, want blockReasonCooldown", reason)
	}
	if !next.Equal(recoverAt) {
		t.Fatalf("next = %v, want quota recovery time %v", next, recoverAt)
	}
}

// TestBlockedNextUsesLatestRecoveryDeadline ensures the reported deadline never
// under-reports, so Retry-After cannot invite a premature client retry.
func TestBlockedNextUsesLatestRecoveryDeadline(t *testing.T) {
	now := time.Now().UTC()
	retryAt := now.Add(20 * time.Minute)
	recoverAt := now.Add(5 * time.Minute)
	auth := modelStateAuth("auth-latest-deadline", "", "gpt-5", &ModelState{
		Status:         StatusError,
		Unavailable:    true,
		NextRetryAfter: retryAt,
		Quota:          QuotaState{Exceeded: true, Scope: quotaScopeModel, Reason: "quota", NextRecoverAt: recoverAt},
	})

	blocked, _, next := isAuthBlockedForModel(auth, "gpt-5", now)
	if !blocked {
		t.Fatal("credential with future deadlines was reported dispatchable")
	}
	if !next.Equal(retryAt) {
		t.Fatalf("next = %v, want the later deadline %v", next, retryAt)
	}
}

// TestBlockAlwaysReportsDeadline pins the invariant the scheduler depends on: a block
// verdict always carries a future deadline, so promoteExpiredLocked can always bring the
// entry back on its own and no snapshot can strand a credential.
func TestBlockAlwaysReportsDeadline(t *testing.T) {
	now := time.Now().UTC()
	future := now.Add(time.Minute)
	past := now.Add(-time.Minute)

	snapshots := []struct {
		name          string
		unavailable   bool
		quotaExceeded bool
		retryAfter    time.Time
		recoverAt     time.Time
	}{
		{name: "no flags"},
		{name: "unavailable without deadline", unavailable: true},
		{name: "quota without deadline", quotaExceeded: true},
		{name: "both without deadline", unavailable: true, quotaExceeded: true},
		{name: "elapsed deadlines", unavailable: true, quotaExceeded: true, retryAfter: past, recoverAt: past},
		{name: "open retry window", unavailable: true, retryAfter: future},
		{name: "open quota window", quotaExceeded: true, recoverAt: future},
		{name: "mixed windows", unavailable: true, quotaExceeded: true, retryAfter: past, recoverAt: future},
	}

	for _, tc := range snapshots {
		t.Run(tc.name, func(t *testing.T) {
			blocked, _, next := availabilityBlock(tc.unavailable, tc.quotaExceeded, tc.retryAfter, tc.recoverAt, now)
			if !blocked {
				if !next.IsZero() {
					t.Fatalf("next = %v, want zero for an unblocked verdict", next)
				}
				return
			}
			if next.IsZero() || !next.After(now) {
				t.Fatalf("next = %v, want a future deadline for a blocked verdict", next)
			}
		})
	}
}

// TestExpiredDeadlinesRestoreAvailability keeps recovery automatic: once every
// deadline elapsed the credential returns without operator action.
func TestExpiredDeadlinesRestoreAvailability(t *testing.T) {
	now := time.Now().UTC()
	auth := modelStateAuth("auth-expired", "", "gpt-5", &ModelState{
		Status:         StatusError,
		Unavailable:    true,
		NextRetryAfter: now.Add(-time.Minute),
		Quota:          QuotaState{Exceeded: true, Scope: quotaScopeModel, Reason: "quota", NextRecoverAt: now.Add(-time.Second)},
	})

	blocked, reason, next := isAuthBlockedForModel(auth, "gpt-5", now)
	if blocked || reason != blockReasonNone || !next.IsZero() {
		t.Fatalf("blocked/reason/next = %v/%v/%v, want available after every deadline elapsed", blocked, reason, next)
	}
}

// TestHighPriorityCoolingCredentialFallsBackToHealthy is the scheduling-level
// regression test for the production 503: a priority=4 credential inside its cooldown
// must not starve a healthy priority=0 credential.
func TestHighPriorityCoolingCredentialFallsBackToHealthy(t *testing.T) {
	now := time.Now().UTC()
	model := "gemini-3-flash-agent"
	recoverAt := now.Add(5 * time.Minute)
	cooling := modelStateAuth("auth-cooling-high-priority", "4", model, &ModelState{
		Status:         StatusError,
		Unavailable:    true,
		NextRetryAfter: recoverAt,
		Quota:          QuotaState{Exceeded: true, Scope: quotaScopeModel, Reason: "quota", NextRecoverAt: recoverAt},
	})
	healthy := modelStateAuth("auth-healthy-low-priority", "0", model, &ModelState{Status: StatusActive})

	available, err := getAvailableAuths([]*Auth{cooling, healthy}, "antigravity", model, now)
	if err != nil {
		t.Fatalf("getAvailableAuths() error = %v, want the healthy credential", err)
	}
	if len(available) != 1 || available[0].ID != healthy.ID {
		ids := make([]string, 0, len(available))
		for _, auth := range available {
			ids = append(ids, auth.ID)
		}
		t.Fatalf("available = %v, want only %s", ids, healthy.ID)
	}
}

// TestLegacyUnboundedStateKeepsCredentialSchedulable is the upgrade guard: a snapshot
// written by an older Home version carries an unavailable flag with no deadline, and it
// must never survive as a credential nothing can revive.
func TestLegacyUnboundedStateKeepsCredentialSchedulable(t *testing.T) {
	now := time.Now().UTC()
	model := "gemini-3-flash-agent"
	legacy := modelStateAuth("auth-legacy-unbounded", "4", model, &ModelState{
		Status:      StatusError,
		Unavailable: true,
		LastError:   &Error{Message: "conflict", HTTPStatus: 409},
	})

	available, err := getAvailableAuths([]*Auth{legacy}, "antigravity", model, now)
	if err != nil {
		t.Fatalf("getAvailableAuths() error = %v, want the legacy credential to stay schedulable", err)
	}
	if len(available) != 1 || available[0].ID != legacy.ID {
		t.Fatalf("available = %v, want the legacy credential", available)
	}

	// The next aggregate pass normalizes the stored snapshot so nothing keeps
	// reporting a cooldown dispatch already ignores.
	updateAggregatedAvailability(legacy, now)
	if state := legacy.ModelStates[model]; state == nil || state.Unavailable {
		t.Fatalf("state = %#v, want the unbounded flag cleared", state)
	}
	if legacy.Unavailable {
		t.Fatal("credential aggregate still reports unavailable")
	}
}

// TestAllCredentialsInQuotaCooldownReportsRetryAfter keeps the 429 cooldown error
// intact: a fleet-wide quota window still yields an actionable reset hint.
func TestAllCredentialsInQuotaCooldownReportsRetryAfter(t *testing.T) {
	now := time.Now().UTC()
	model := "gpt-5"
	earliest := now.Add(3 * time.Minute)
	first := modelStateAuth("auth-cooldown-early", "", model, &ModelState{
		Status:         StatusError,
		Unavailable:    true,
		NextRetryAfter: earliest,
		Quota:          QuotaState{Exceeded: true, Scope: quotaScopeModel, Reason: "quota", NextRecoverAt: earliest},
	})
	later := now.Add(9 * time.Minute)
	second := modelStateAuth("auth-cooldown-late", "", model, &ModelState{
		Status:         StatusError,
		Unavailable:    true,
		NextRetryAfter: later,
		Quota:          QuotaState{Exceeded: true, Scope: quotaScopeModel, Reason: "quota", NextRecoverAt: later},
	})

	_, err := getAvailableAuths([]*Auth{first, second}, "codex", model, now)
	cooldownErr, ok := err.(*modelCooldownError)
	if !ok {
		t.Fatalf("error = %T (%v), want *modelCooldownError", err, err)
	}
	if cooldownErr.resetIn > earliest.Sub(now) {
		t.Fatalf("resetIn = %v, want at most the earliest recovery %v", cooldownErr.resetIn, earliest.Sub(now))
	}
}
