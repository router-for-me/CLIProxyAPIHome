package auth

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

func TestDispatchExpiryGateBeforeModelState(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		delta   time.Duration
		kind    string
		blocked bool
	}{
		{"expired", -time.Second, AuthKindOAuth, true},
		{"expires now", 0, AuthKindOAuth, true},
		{"near expiry", 30 * time.Second, AuthKindOAuth, true},
		{"safety boundary", 5 * time.Minute, AuthKindOAuth, true},
		{"outside safety boundary", 5*time.Minute + time.Nanosecond, AuthKindOAuth, false},
		{"fresh", time.Hour, AuthKindOAuth, false},
		{"API key unchanged", -time.Hour, AuthKindAPIKey, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &Auth{ID: "expiry", Provider: "gemini", Status: StatusActive, Attributes: map[string]string{"auth_kind": tc.kind}, Metadata: map[string]any{"expired": now.Add(tc.delta).Format(time.RFC3339Nano)}, ModelStates: map[string]*ModelState{"model": {Status: StatusActive}}}
			for _, model := range []string{"", "model", "other"} {
				blocked, reason, _ := isAuthBlockedForModel(a, model, now)
				if blocked != tc.blocked {
					t.Fatalf("model=%q blocked=%v want %v", model, blocked, tc.blocked)
				}
				if blocked && reason != blockReasonOther {
					t.Fatalf("expiry became quota cooldown: %v", reason)
				}
			}
		})
	}
	unknown := &Auth{Provider: "gemini", Attributes: map[string]string{"auth_kind": AuthKindOAuth}}
	if blocked, _, _ := isAuthBlockedForModel(unknown, "model", now); blocked {
		t.Fatal("unknown expiry changed existing eligibility")
	}
}

func TestDispatchExpirySkipsCredentialAndAcceptsSameUUIDReplacement(t *testing.T) {
	for _, tc := range []struct {
		name     string
		selector Selector
	}{
		{"scheduler", &RoundRobinSelector{}}, {"custom", firstCandidateSelector{}}, {"fill first", &FillFirstSelector{}}, {"weighted", &WeightedRoundRobinSelector{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager := NewManager(nil, tc.selector, nil)
			makeAuth := func(id string, expiry time.Time, priority string) *Auth {
				return &Auth{ID: id, Provider: "gemini", Status: StatusActive, Attributes: map[string]string{"auth_kind": AuthKindOAuth, "priority": priority}, Metadata: map[string]any{"expired": expiry.Format(time.RFC3339Nano)}}
			}
			stale := makeAuth("stale-"+tc.name, time.Now().Add(-time.Hour), "100")
			fresh := makeAuth("fresh-"+tc.name, time.Now().Add(time.Hour), "0")
			registerDispatchTestAuth(t, manager, stale, "model")
			registerDispatchTestAuth(t, manager, fresh, "model")
			decision, err := manager.Dispatch(context.Background(), []string{"gemini"}, "model", Options{})
			if err != nil || decision == nil || decision.Auth.ID != fresh.ID {
				t.Fatalf("expired credential was not skipped: decision=%v err=%v", decision, err)
			}
			fresh.Metadata["expired"] = time.Now().Add(time.Minute).Format(time.RFC3339Nano)
			if _, errUpdate := manager.Update(context.Background(), fresh); errUpdate != nil {
				t.Fatal(errUpdate)
			}
			decision, err = manager.Dispatch(context.Background(), []string{"gemini"}, "model", Options{})
			var unavailable *Error
			if decision != nil || !errors.As(err, &unavailable) || unavailable.Code != "auth_unavailable" {
				t.Fatalf("unsafe-only pool did not refuse: decision=%v err=%v", decision, err)
			}
			stale.Metadata["expired"] = time.Now().Add(time.Hour).Format(time.RFC3339Nano)
			if _, errUpdate := manager.Update(context.Background(), stale); errUpdate != nil {
				t.Fatal(errUpdate)
			}
			decision, err = manager.Dispatch(context.Background(), []string{"gemini"}, "model", Options{})
			if err != nil || decision == nil || decision.Auth.ID != stale.ID {
				t.Fatalf("same-UUID fresh replacement stayed blocked: decision=%v err=%v", decision, err)
			}
		})
	}
}

func TestDispatchExpiryDropsExistingAffinity(t *testing.T) {
	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: &FillFirstSelector{}, TTL: time.Hour})
	defer selector.Stop()
	first := &Auth{ID: "a", Provider: "gemini", Status: StatusActive, Attributes: map[string]string{"auth_kind": AuthKindOAuth}, Metadata: map[string]any{"expired": time.Now().Add(time.Hour).Format(time.RFC3339Nano)}}
	second := first.Clone()
	second.ID = "b"
	opts := Options{Headers: http.Header{"X-Session-ID": []string{"expiry-session"}}}
	picked, err := selector.Pick(context.Background(), "gemini", "model", opts, []*Auth{first, second})
	if err != nil || picked == nil || picked.ID != first.ID {
		t.Fatalf("initial affinity: %v %v", picked, err)
	}
	first.Metadata["expired"] = time.Now().Add(-time.Hour).Format(time.RFC3339Nano)
	picked, err = selector.Pick(context.Background(), "gemini", "model", opts, []*Auth{first, second})
	if err != nil || picked == nil || picked.ID != second.ID {
		t.Fatalf("expired affinity remained dispatchable: %v %v", picked, err)
	}
}

func TestDispatchExpiryRechecksFullCredential(t *testing.T) {
	for _, tc := range []struct {
		name     string
		selector Selector
	}{
		{"scheduler", &RoundRobinSelector{}}, {"custom", firstCandidateSelector{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager := NewManager(nil, tc.selector, nil)
			stale := &Auth{ID: "full-stale-" + tc.name, Provider: "gemini", Status: StatusActive, Attributes: map[string]string{"auth_kind": AuthKindOAuth, "priority": "100"}, Metadata: map[string]any{"expired": time.Now().Add(time.Hour).Format(time.RFC3339Nano)}}
			fresh := stale.Clone()
			fresh.ID = "full-fresh-" + tc.name
			fresh.Attributes["priority"] = "0"
			registerDispatchTestAuth(t, manager, stale, "model")
			registerDispatchTestAuth(t, manager, fresh, "model")
			full := stale.Clone()
			full.Metadata["expired"] = time.Now().Add(-time.Hour).Format(time.RFC3339Nano)
			manager.SetFullAuthResolver(dispatchTestFullAuthResolver{auths: map[string]*Auth{stale.ID: full, fresh.ID: fresh}})
			decision, err := manager.Dispatch(context.Background(), []string{"gemini"}, "model", Options{})
			if err != nil || decision == nil || decision.Auth.ID != fresh.ID {
				t.Fatalf("fresh index hid expired full credential: %v %v", decision, err)
			}
		})
	}
}
