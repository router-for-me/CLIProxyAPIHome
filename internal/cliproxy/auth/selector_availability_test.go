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

// TestZeroDeadlineUnavailableModelBlocksDispatch locks the regression behind the
// production 503: a model marked unavailable without any recovery deadline is an
// unbounded bad state and must fail closed instead of being dispatched.
func TestZeroDeadlineUnavailableModelBlocksDispatch(t *testing.T) {
	now := time.Now().UTC()
	auth := modelStateAuth("auth-zero-deadline", "", "gemini-3-flash-agent", &ModelState{
		Status:      StatusError,
		Unavailable: true,
		Quota:       QuotaState{Exceeded: true, Scope: quotaScopeModel, Reason: "quota"},
	})

	blocked, reason, next := isAuthBlockedForModel(auth, "gemini-3-flash-agent", now)
	if !blocked {
		t.Fatal("unavailable model without a recovery deadline was reported dispatchable")
	}
	if reason != blockReasonOther {
		t.Fatalf("reason = %v, want blockReasonOther for an unbounded bad state", reason)
	}
	if !next.IsZero() {
		t.Fatalf("next = %v, want zero: there is no deadline to report", next)
	}
}

// TestQuotaExceededWithoutUnavailableBlocksDispatch covers state written by cluster
// merges or the Management API where only the quota flag is set.
func TestQuotaExceededWithoutUnavailableBlocksDispatch(t *testing.T) {
	now := time.Now().UTC()
	auth := modelStateAuth("auth-quota-only", "", "gpt-5", &ModelState{
		Status: StatusError,
		Quota:  QuotaState{Exceeded: true, Scope: quotaScopeModel, Reason: "quota"},
	})

	blocked, reason, next := isAuthBlockedForModel(auth, "gpt-5", now)
	if !blocked {
		t.Fatal("quota-exceeded model was reported dispatchable")
	}
	if reason != blockReasonOther {
		t.Fatalf("reason = %v, want blockReasonOther without a recovery deadline", reason)
	}
	if !next.IsZero() {
		t.Fatalf("next = %v, want zero", next)
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

// TestHighPriorityBadCredentialFallsBackToHealthy is the scheduling-level
// regression test for the production 503: a priority=4 credential stuck in an
// unbounded bad state must not starve a healthy priority=0 credential.
func TestHighPriorityBadCredentialFallsBackToHealthy(t *testing.T) {
	now := time.Now().UTC()
	model := "gemini-3-flash-agent"
	broken := modelStateAuth("auth-broken-high-priority", "4", model, &ModelState{
		Status:      StatusError,
		Unavailable: true,
		Quota:       QuotaState{Exceeded: true, Scope: quotaScopeModel, Reason: "quota"},
	})
	healthy := modelStateAuth("auth-healthy-low-priority", "0", model, &ModelState{Status: StatusActive})

	available, err := getAvailableAuths([]*Auth{broken, healthy}, "antigravity", model, now)
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
