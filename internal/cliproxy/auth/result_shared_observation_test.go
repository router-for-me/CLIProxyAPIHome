package auth

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestMarkResultCleanTimedSuccessStaysDatabaseFree(t *testing.T) {
	ctx := context.Background()
	const authID = "auth-clean-observation"
	seed := &Auth{ID: authID, Index: authID, Provider: "codex", Status: StatusActive, Metadata: map[string]any{"access_token": "test-access-token"}}
	store := &fakeMutatorStore{persisted: seed.Clone()}
	nodes := []*Manager{newHomeNodeManager(t, store, authID), newHomeNodeManager(t, store, authID)}
	at := time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 100; i++ {
		success := Result{AuthID: authID, Provider: "codex", Model: "gpt-5", Success: true, ObservedAt: at.Add(time.Duration(i) * time.Second)}
		if i%2 == 0 {
			success.AccessTokenSHA256 = AccessTokenSHA256(seed)
		}
		if errMark := nodes[i%len(nodes)].MarkResult(ctx, success); errMark != nil {
			t.Fatal(errMark)
		}
	}
	if attempts := store.mutationAttemptCount(); attempts != 0 {
		t.Fatalf("clean successes opened %d state transactions, want 0", attempts)
	}
}

func TestMarkResultInWindowObservationPersistsWithoutAvailabilityChange(t *testing.T) {
	ctx := context.Background()
	const authID = "auth-in-window-observation"
	store := &fakeMutatorStore{persisted: &Auth{ID: authID, Index: authID, Provider: "codex", Status: StatusActive}}
	nodeA := newHomeNodeManager(t, store, authID)
	nodeB := newHomeNodeManager(t, store, authID)
	at := time.Now().UTC().Add(-time.Minute)
	first := quotaResult(authID, "gpt-5")
	retry := time.Hour
	first.RetryAfter = &retry
	first.ObservedAt = at
	if errMark := nodeA.MarkResult(ctx, first); errMark != nil {
		t.Fatal(errMark)
	}
	deadline := store.persistedSnapshot().ModelStates["gpt-5"].Quota.NextRecoverAt
	// The same node already sees the open window locally; only the fence advances.
	sameNode := quotaResult(authID, "gpt-5")
	sameNode.ObservedAt = at.Add(time.Second)
	if errMark := nodeA.MarkResult(ctx, sameNode); errMark != nil {
		t.Fatal(errMark)
	}
	if state := store.persistedSnapshot().ModelStates["gpt-5"]; !state.LastObservedAt.Equal(sameNode.ObservedAt) || !state.Quota.NextRecoverAt.Equal(deadline) || state.Quota.BackoffLevel != 1 {
		t.Fatalf("same-node fence-only mutation changed availability or lost observation: %+v", state)
	}
	newer := quotaResult(authID, "gpt-5")
	newer.ObservedAt = at.Add(2 * time.Second)
	if errMark := nodeB.MarkResult(ctx, newer); errMark != nil {
		t.Fatal(errMark)
	}
	if state := store.persistedSnapshot().ModelStates["gpt-5"]; !state.LastObservedAt.Equal(newer.ObservedAt) || !state.Quota.NextRecoverAt.Equal(deadline) || state.Quota.BackoffLevel != 1 {
		t.Fatalf("fence-only mutation changed availability or lost observation: %+v", state)
	}
	older := Result{AuthID: authID, Provider: "codex", Model: "gpt-5", Success: true, ObservedAt: at.Add(1500 * time.Millisecond)}
	if errMark := nodeA.MarkResult(ctx, older); errMark != nil {
		t.Fatal(errMark)
	}
	if !store.persistedSnapshot().ModelStates["gpt-5"].Quota.Exceeded {
		t.Fatal("older success cleared newer cooldown")
	}
	mutations := store.mutationCount()
	newer.SkipRequestStats = true
	if errMark := nodeB.MarkResult(ctx, newer); errMark != nil {
		t.Fatal(errMark)
	}
	if store.mutationCount() != mutations {
		t.Fatal("replayed observation caused another write")
	}
}

func TestMarkResultTimedSuccessPreservesTokenVersionFence(t *testing.T) {
	ctx := context.Background()
	const authID = "auth-observation-token-version"
	seed := &Auth{ID: authID, Index: authID, Provider: "codex", Status: StatusActive, Metadata: map[string]any{"access_token": "new-token"}}
	store := &fakeMutatorStore{persisted: seed.Clone()}
	manager := newHomeNodeManager(t, store, authID)
	result := Result{AuthID: authID, Provider: "codex", Model: "gpt-5", Success: true, ObservedAt: time.Now().UTC(), AccessTokenSHA256: AccessTokenSHA256(&Auth{Metadata: map[string]any{"access_token": "old-token"}})}
	if errMark := manager.MarkResult(ctx, result); errMark != nil {
		t.Fatal(errMark)
	}
	if store.mutationCount() != 0 || store.persistedSnapshot().ModelStates["gpt-5"] != nil {
		t.Fatal("old token success advanced the current credential's observation fence")
	}
}

func TestMarkResultTimedObservationReturnsPersistenceFailure(t *testing.T) {
	ctx := context.Background()
	const authID = "auth-observation-write-error"
	failure := errors.New("database unavailable")
	store := &fakeMutatorStore{persisted: &Auth{ID: authID, Index: authID, Provider: "codex", Status: StatusActive}}
	manager := newHomeNodeManager(t, store, authID)
	at := time.Now().UTC().Add(-time.Minute)
	first := quotaResult(authID, "gpt-5")
	retry := time.Hour
	first.RetryAfter = &retry
	first.ObservedAt = at
	if errMark := manager.MarkResult(ctx, first); errMark != nil {
		t.Fatal(errMark)
	}
	store.mu.Lock()
	store.mutateErr = failure
	store.mu.Unlock()
	inWindow := quotaResult(authID, "gpt-5")
	inWindow.ObservedAt = at.Add(time.Second)
	if errMark := manager.MarkResult(ctx, inWindow); !errors.Is(errMark, failure) {
		t.Fatalf("MarkResult error = %v, want transient persistence failure", errMark)
	}
	local, _ := manager.GetByID(authID)
	if state := local.ModelStates["gpt-5"]; state == nil || !state.LastObservedAt.Equal(at) {
		t.Fatalf("failed persistence advanced the local observation fence: %+v", state)
	}
}
