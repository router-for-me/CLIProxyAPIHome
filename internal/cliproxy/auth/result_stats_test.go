package auth

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestUsageFeedbackStatsCountAfterCredentialLoads(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	result := Result{AuthID: "stats-auth", Model: "route", Success: true, RequestStatsEventID: "stats-execution", SkipRequestStats: true}
	if errMark := manager.MarkResult(t.Context(), result); !errors.Is(errMark, ErrAuthNotLoaded) {
		t.Fatalf("missing credential feedback=%v", errMark)
	}
	if _, errRegister := manager.Register(t.Context(), &Auth{ID: result.AuthID, Provider: "codex", Status: StatusActive}); errRegister != nil {
		t.Fatal(errRegister)
	}
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			if errMark := manager.MarkResult(t.Context(), result); errMark != nil {
				t.Error(errMark)
			}
		})
	}
	wg.Wait()
	auth, _ := manager.GetByID(result.AuthID)
	var recentSuccess int64
	for _, bucket := range auth.RecentRequestsSnapshot(time.Now()) {
		recentSuccess += bucket.Success
	}
	if auth.Success != 1 || recentSuccess != 1 {
		t.Fatalf("concurrent retry stats=%d recent=%d, want 1", auth.Success, recentSuccess)
	}
}

func TestUsageFeedbackStatsSurviveTransitionFailure(t *testing.T) {
	const authID = "stats-transition-auth"
	failure := errors.New("result database unavailable")
	store := &fakeMutatorStore{persisted: &Auth{ID: authID, Index: authID, Provider: "codex", Status: StatusActive}, mutateErr: failure}
	manager := newHomeNodeManager(t, store, authID)
	result := Result{AuthID: authID, Model: "route", Error: &Error{HTTPStatus: 429}, ObservedAt: time.Now().UTC(), RequestStatsEventID: "failed-transition-execution"}
	if errMark := manager.MarkResult(t.Context(), result); !errors.Is(errMark, failure) {
		t.Fatalf("transition failure=%v", errMark)
	}
	store.mutateErr = nil
	result.SkipRequestStats = true
	for range 3 {
		if errMark := manager.MarkResult(t.Context(), result); errMark != nil {
			t.Fatal(errMark)
		}
	}
	auth, _ := manager.GetByID(authID)
	var recentFailed int64
	for _, bucket := range auth.RecentRequestsSnapshot(time.Now()) {
		recentFailed += bucket.Failed
	}
	if auth.Failed != 1 || recentFailed != 1 || auth.ModelStates["route"] == nil || !auth.ModelStates["route"].Quota.Exceeded {
		t.Fatalf("retried transition lost stats or feedback: failed=%d recent=%d states=%+v", auth.Failed, recentFailed, auth.ModelStates)
	}
}

func TestUsageFeedbackStatsReplayHistoryIsBounded(t *testing.T) {
	manager := &Manager{}
	auth := &Auth{}
	now := time.Now()
	manager.mu.Lock()
	defer manager.mu.Unlock()
	for i := range maxUsageRequestStatsEvents + 3 {
		result := Result{Success: true, RequestStatsEventID: fmt.Sprintf("execution-%d", i), SkipRequestStats: true}
		manager.recordResultRequestStatsLocked(auth, result, now)
		manager.recordResultRequestStatsLocked(auth, result, now)
	}
	if auth.Success != maxUsageRequestStatsEvents+3 || len(manager.usageStats.seen) != maxUsageRequestStatsEvents || len(manager.usageStats.order) != maxUsageRequestStatsEvents {
		t.Fatalf("stats=%d replay cache=%d/%d", auth.Success, len(manager.usageStats.seen), len(manager.usageStats.order))
	}
	// Legacy duplicate deliveries keep their previous skip-statistics contract.
	manager.recordResultRequestStatsLocked(auth, Result{Success: true, SkipRequestStats: true}, now)
	if auth.Success != maxUsageRequestStatsEvents+3 {
		t.Fatal("legacy replay counted as a new request")
	}
}
