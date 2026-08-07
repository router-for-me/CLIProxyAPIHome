package auth

import (
	"net/http"
	"testing"
	"time"
)

// noCoolingModelAuth builds a credential carrying the per-credential disable_cooling
// override, which is the only way Home can turn cooldowns off: the runtime forces the
// global flag to false because Home is the sole scheduler.
func noCoolingModelAuth(id, priority, model string) *Auth {
	auth := modelStateAuth(id, priority, model, &ModelState{Status: StatusActive})
	auth.Metadata = map[string]any{"disable_cooling": true}
	return auth
}

// TestDisableCoolingKeepsEveryFailureDispatchable covers the operator choice of running
// a credential without cooldowns: failures still record why they happened, but no branch
// may park the credential, because scheduling is expected to stay under the control of
// credential priority and session affinity alone.
func TestDisableCoolingKeepsEveryFailureDispatchable(t *testing.T) {
	failures := []struct {
		name   string
		status int
		body   string
	}{
		{name: "unauthorized", status: http.StatusUnauthorized, body: "expired access token"},
		{name: "payment required", status: http.StatusPaymentRequired, body: "payment required"},
		{name: "forbidden", status: http.StatusForbidden, body: "permission denied"},
		{name: "not found", status: http.StatusNotFound, body: "model not found"},
		{name: "quota", status: http.StatusTooManyRequests, body: "quota exceeded"},
		{name: "transient", status: http.StatusBadGateway, body: "bad gateway"},
		{name: "unmapped", status: http.StatusNotImplemented, body: "not implemented"},
		{name: "invalid grant", status: http.StatusBadRequest, body: "invalid_grant"},
		{name: "cloudflare challenge", status: http.StatusForbidden, body: "cf-mitigated"},
	}

	for _, tc := range failures {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now().UTC()
			auth := failingAuth("auth-nocool-"+tc.name, true)

			applyFailure(auth, "gpt-5", tc.status, tc.body, now)

			state := auth.ModelStates["gpt-5"]
			if state == nil {
				t.Fatal("model state was not recorded")
			}
			if !state.NextRetryAfter.IsZero() || !state.Quota.NextRecoverAt.IsZero() {
				t.Fatalf("disable_cooling produced deadlines: retry=%v recover=%v", state.NextRetryAfter, state.Quota.NextRecoverAt)
			}
			if state.Unavailable || state.Quota.Exceeded {
				t.Fatalf("state = %#v, want no availability flags left behind", state)
			}
			if state.LastError == nil {
				t.Fatal("failure detail was dropped")
			}
			if blocked, _, _ := isAuthBlockedForModel(auth, "gpt-5", now); blocked {
				t.Fatal("disable_cooling credential was parked")
			}
		})
	}
}

// TestDisableCoolingKeepsPrioritySchedulingInCharge pins the scheduling contract behind
// disable_cooling: repeated failures never remove a credential from the candidate set,
// so the highest priority tier keeps winning.
func TestDisableCoolingKeepsPrioritySchedulingInCharge(t *testing.T) {
	now := time.Now().UTC()
	model := "gemini-3-flash-agent"
	high := noCoolingModelAuth("auth-nocool-high", "4", model)
	low := noCoolingModelAuth("auth-nocool-low", "0", model)

	for range 3 {
		applyFailure(high, model, http.StatusTooManyRequests, "quota exceeded", now)
	}

	available, err := getAvailableAuths([]*Auth{low, high}, "antigravity", model, now)
	if err != nil {
		t.Fatalf("getAvailableAuths() error = %v", err)
	}
	if len(available) != 1 || available[0].ID != high.ID {
		t.Fatalf("available = %v, want only the highest priority credential %s", available, high.ID)
	}
}

// TestDisableCoolingKeepsSessionBindingStable keeps session affinity in charge for a
// credential that never cools down: a bound session must not drift to another
// credential just because the bound one keeps failing.
func TestDisableCoolingKeepsSessionBindingStable(t *testing.T) {
	now := time.Now().UTC()
	model := sessionPriorityModel
	high := noCoolingModelAuth("auth-nocool-session-high", "4", model)
	low := noCoolingModelAuth("auth-nocool-session-low", "0", model)

	selector := newSessionSelector()
	defer selector.Stop()
	opts := sessionOptions("sess-nocool")

	// Park the high tier with a real cooldown so the cold binding lands on the low tier.
	blockModel(high, now)
	bound, err := selector.Pick(t.Context(), "antigravity", model, opts, []*Auth{low, high})
	if err != nil {
		t.Fatalf("initial Pick() error = %v", err)
	}
	if bound.ID != low.ID {
		t.Fatalf("initial pick = %s, want %s", bound.ID, low.ID)
	}

	// The bound credential keeps failing, but disable_cooling means it stays available,
	// so the established binding must survive both the failures and the high tier
	// recovering.
	unblockModel(high)
	applyFailure(low, model, http.StatusTooManyRequests, "quota exceeded", now)
	applyFailure(low, model, http.StatusBadGateway, "bad gateway", now)

	reused, err := selector.Pick(t.Context(), "antigravity", model, opts, []*Auth{low, high})
	if err != nil {
		t.Fatalf("second Pick() error = %v", err)
	}
	if reused.ID != low.ID {
		t.Fatalf("second pick = %s, want the established binding %s", reused.ID, low.ID)
	}
}
