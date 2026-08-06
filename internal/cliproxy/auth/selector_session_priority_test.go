package auth

import (
	"context"
	"net/http"
	"testing"
	"time"
)

const sessionPriorityModel = "gemini-3-flash-agent"

// sessionPriorityAuth builds a credential at the given priority for the shared model.
func sessionPriorityAuth(id, priority string) *Auth {
	return modelStateAuth(id, priority, sessionPriorityModel, &ModelState{Status: StatusActive})
}

// blockModel parks the credential's model behind a future cooldown.
func blockModel(auth *Auth, now time.Time) {
	auth.ModelStates[sessionPriorityModel] = &ModelState{
		Status:         StatusError,
		Unavailable:    true,
		NextRetryAfter: now.Add(30 * time.Minute),
	}
}

// unblockModel restores the credential's model to a healthy state.
func unblockModel(auth *Auth) {
	auth.ModelStates[sessionPriorityModel] = &ModelState{Status: StatusActive}
}

// sessionOptions builds dispatch options carrying an explicit session header.
func sessionOptions(sessionID string) Options {
	headers := http.Header{}
	headers.Set("X-Session-ID", sessionID)
	return Options{Headers: headers}
}

func newSessionSelector() *SessionAffinitySelector {
	return NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
		Fallback: &FillFirstSelector{},
		TTL:      time.Hour,
	})
}

// TestSessionColdBindingPrefersHighestPriority keeps credential priority in charge
// of new bindings.
func TestSessionColdBindingPrefersHighestPriority(t *testing.T) {
	high := sessionPriorityAuth("auth-high", "4")
	low := sessionPriorityAuth("auth-low", "0")
	selector := newSessionSelector()
	defer selector.Stop()

	picked, err := selector.Pick(context.Background(), "antigravity", sessionPriorityModel, sessionOptions("sess-cold"), []*Auth{low, high})
	if err != nil {
		t.Fatalf("Pick() error = %v", err)
	}
	if picked.ID != high.ID {
		t.Fatalf("picked = %s, want the highest priority credential %s", picked.ID, high.ID)
	}
}

// TestSessionBindingSurvivesHigherPriorityRecovery is the regression test for
// mid-conversation credential swaps: once a session is bound to a still-available
// credential, a recovering higher-priority credential must not steal the session.
func TestSessionBindingSurvivesHigherPriorityRecovery(t *testing.T) {
	now := time.Now().UTC()
	high := sessionPriorityAuth("auth-high", "4")
	low := sessionPriorityAuth("auth-low", "0")
	blockModel(high, now)

	selector := newSessionSelector()
	defer selector.Stop()
	opts := sessionOptions("sess-sticky")

	bound, err := selector.Pick(context.Background(), "antigravity", sessionPriorityModel, opts, []*Auth{low, high})
	if err != nil {
		t.Fatalf("initial Pick() error = %v", err)
	}
	if bound.ID != low.ID {
		t.Fatalf("initial pick = %s, want %s while the high priority credential is cooling", bound.ID, low.ID)
	}

	unblockModel(high)

	reused, err := selector.Pick(context.Background(), "antigravity", sessionPriorityModel, opts, []*Auth{low, high})
	if err != nil {
		t.Fatalf("second Pick() error = %v", err)
	}
	if reused.ID != low.ID {
		t.Fatalf("second pick = %s, want the established binding %s to survive priority recovery", reused.ID, low.ID)
	}
}

// TestSessionRebindsWhenBoundAuthBlocked keeps genuine failover working: a bound
// credential that goes into cooldown must hand the session to the highest tier.
func TestSessionRebindsWhenBoundAuthBlocked(t *testing.T) {
	now := time.Now().UTC()
	high := sessionPriorityAuth("auth-high", "4")
	low := sessionPriorityAuth("auth-low", "0")
	blockModel(high, now)

	selector := newSessionSelector()
	defer selector.Stop()
	opts := sessionOptions("sess-failover")

	bound, err := selector.Pick(context.Background(), "antigravity", sessionPriorityModel, opts, []*Auth{low, high})
	if err != nil {
		t.Fatalf("initial Pick() error = %v", err)
	}
	if bound.ID != low.ID {
		t.Fatalf("initial pick = %s, want %s", bound.ID, low.ID)
	}

	unblockModel(high)
	blockModel(low, now)

	failedOver, err := selector.Pick(context.Background(), "antigravity", sessionPriorityModel, opts, []*Auth{low, high})
	if err != nil {
		t.Fatalf("failover Pick() error = %v", err)
	}
	if failedOver.ID != high.ID {
		t.Fatalf("failover pick = %s, want %s once the bound credential is cooling", failedOver.ID, high.ID)
	}
}

// TestSessionlessPickUsesHighestPriority keeps requests without a session signal on
// the priority path.
func TestSessionlessPickUsesHighestPriority(t *testing.T) {
	high := sessionPriorityAuth("auth-high", "4")
	low := sessionPriorityAuth("auth-low", "0")
	selector := newSessionSelector()
	defer selector.Stop()

	picked, err := selector.Pick(context.Background(), "antigravity", sessionPriorityModel, Options{}, []*Auth{low, high})
	if err != nil {
		t.Fatalf("Pick() error = %v", err)
	}
	if picked.ID != high.ID {
		t.Fatalf("picked = %s, want %s", picked.ID, high.ID)
	}
}

// TestHighestPriorityAuthsNarrowsTiers pins the helper used to keep the fallback
// selector on a single tier.
func TestHighestPriorityAuthsNarrowsTiers(t *testing.T) {
	high := sessionPriorityAuth("auth-high", "4")
	alsoHigh := sessionPriorityAuth("auth-high-2", "4")
	low := sessionPriorityAuth("auth-low", "0")

	narrowed := highestPriorityAuths([]*Auth{low, high, alsoHigh})
	if len(narrowed) != 2 {
		t.Fatalf("len(narrowed) = %d, want 2", len(narrowed))
	}
	for _, auth := range narrowed {
		if authPriority(auth) != 4 {
			t.Fatalf("narrowed contains %s at priority %d, want only priority 4", auth.ID, authPriority(auth))
		}
	}

	singleTier := []*Auth{high, alsoHigh}
	if got := highestPriorityAuths(singleTier); len(got) != len(singleTier) {
		t.Fatalf("single tier was reallocated: len=%d, want %d", len(got), len(singleTier))
	}
}

// TestAcrossPrioritiesReturnsEveryTier pins the membership view used to validate
// established bindings.
func TestAcrossPrioritiesReturnsEveryTier(t *testing.T) {
	now := time.Now().UTC()
	high := sessionPriorityAuth("auth-high", "4")
	low := sessionPriorityAuth("auth-low", "0")

	available, err := getAvailableAuthsAcrossPriorities([]*Auth{low, high}, "antigravity", sessionPriorityModel, now)
	if err != nil {
		t.Fatalf("getAvailableAuthsAcrossPriorities() error = %v", err)
	}
	if len(available) != 2 {
		t.Fatalf("len(available) = %d, want both tiers", len(available))
	}
	if available[0].ID != high.ID || available[1].ID != low.ID {
		t.Fatalf("available = [%s %s], want ID-sorted [%s %s]", available[0].ID, available[1].ID, high.ID, low.ID)
	}
}
