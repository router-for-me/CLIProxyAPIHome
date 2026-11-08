package auth

import (
	"context"
	"testing"
	"time"
)

func TestMarkResultFencesStaleAndReplayedObservations(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	auth := &Auth{ID: "auth-observation-fence", Provider: "codex"}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("Register returned error: %v", errRegister)
	}
	base := time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)

	quota := quotaResult(auth.ID, "gpt-5")
	quota.ObservedAt = base.Add(10 * time.Second)
	if errMark := manager.MarkResult(context.Background(), quota); errMark != nil {
		t.Fatalf("MarkResult(quota) error = %v", errMark)
	}
	cooled, _ := manager.GetByID(auth.ID)
	state := cooled.ModelStates["gpt-5"]
	if state == nil || !state.Quota.Exceeded || state.Quota.BackoffLevel != 1 {
		t.Fatalf("expected quota cooldown after 429, got %+v", state)
	}
	if !state.LastObservedAt.Equal(quota.ObservedAt) {
		t.Fatalf("LastObservedAt = %v, want %v", state.LastObservedAt, quota.ObservedAt)
	}
	deadline := state.Quota.NextRecoverAt

	// An older success delivered late must not clear the newer cooldown.
	stale := Result{AuthID: auth.ID, Provider: "codex", Model: "gpt-5", Success: true, ObservedAt: base}
	if errMark := manager.MarkResult(context.Background(), stale); errMark != nil {
		t.Fatalf("MarkResult(stale success) error = %v", errMark)
	}
	afterStale, _ := manager.GetByID(auth.ID)
	if got := afterStale.ModelStates["gpt-5"]; !got.Quota.Exceeded || !got.Quota.NextRecoverAt.Equal(deadline) {
		t.Fatalf("stale success altered quota state: %+v", got)
	}
	if afterStale.Success != 1 {
		t.Fatalf("stale success must still count request stats, Success = %d", afterStale.Success)
	}

	// A redelivery of the same 429 must not escalate backoff or count again.
	replay := quota
	replay.SkipRequestStats = true
	if errMark := manager.MarkResult(context.Background(), replay); errMark != nil {
		t.Fatalf("MarkResult(replay) error = %v", errMark)
	}
	afterReplay, _ := manager.GetByID(auth.ID)
	if got := afterReplay.ModelStates["gpt-5"]; got.Quota.BackoffLevel != 1 || !got.Quota.NextRecoverAt.Equal(deadline) {
		t.Fatalf("replayed 429 altered quota state: %+v", got)
	}
	if afterReplay.Failed != 1 {
		t.Fatalf("replayed 429 must not count request stats, Failed = %d", afterReplay.Failed)
	}

	// A newer success clears the cooldown and advances the fence.
	newer := Result{AuthID: auth.ID, Provider: "codex", Model: "gpt-5", Success: true, ObservedAt: base.Add(20 * time.Second)}
	if errMark := manager.MarkResult(context.Background(), newer); errMark != nil {
		t.Fatalf("MarkResult(newer success) error = %v", errMark)
	}
	cleared, _ := manager.GetByID(auth.ID)
	if got := cleared.ModelStates["gpt-5"]; got.Quota.Exceeded || !got.LastObservedAt.Equal(newer.ObservedAt) {
		t.Fatalf("newer success did not clear quota or advance fence: %+v", got)
	}

	// Results without an event time are never fenced.
	untimed := quotaResult(auth.ID, "gpt-5")
	if errMark := manager.MarkResult(context.Background(), untimed); errMark != nil {
		t.Fatalf("MarkResult(untimed) error = %v", errMark)
	}
	final, _ := manager.GetByID(auth.ID)
	if got := final.ModelStates["gpt-5"]; !got.Quota.Exceeded {
		t.Fatalf("untimed 429 must apply, got %+v", got)
	}
}
