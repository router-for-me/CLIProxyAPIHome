package auth

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestMixedRoundRobinCountsRetryEligibleCredentials(t *testing.T) {
	scheduler := newAuthScheduler(&RoundRobinSelector{}, nil, nil)
	for i := range 10 {
		// Only two credentials can join round 1. One of those has already been tried.
		retry := 0
		if i >= 8 {
			retry = 1
		}
		scheduler.upsertAuth(&Auth{
			ID: fmt.Sprintf("a-%02d", i), Provider: "gemini", Status: StatusActive,
			Metadata: map[string]any{"request_retry": retry},
		})
	}
	// This credential inherits the default retry limit.
	scheduler.upsertAuth(&Auth{ID: "b-00", Provider: "vertex", Status: StatusActive})
	providers := []string{"gemini", "vertex"}
	opts := withRequestRetryDispatchMetadata(Options{}, 1, 1)
	tried := map[string]struct{}{"a-08": {}}
	counts := map[string]int{}
	for range 20 {
		picked, provider, errPick := scheduler.pickMixed(t.Context(), providers, "", opts, tried)
		if errPick != nil || picked == nil || (picked.ID != "a-09" && picked.ID != "b-00") {
			t.Fatalf("pick = %v (%s), %v; want a-09 or b-00", picked, provider, errPick)
		}
		counts[provider]++
	}
	if counts["gemini"] != 10 || counts["vertex"] != 10 {
		t.Fatalf("provider picks = %v, want 10 each", counts)
	}
}

func TestMixedRoundRobinIsolatesAlternatingAPIKeyPools(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	const model = "mixed-api-key-pools"
	for id, provider := range map[string]string{"a-0": "gemini", "a-1": "gemini", "b-0": "vertex"} {
		registerDispatchTestAuth(t, manager, &Auth{ID: id, Provider: provider, Status: StatusActive}, model)
	}
	keys := [][]string{{"a-0", "a-1", "b-0"}, {"a-0", "b-0"}}
	var counts [2]int
	for range 60 {
		for i, allowed := range keys {
			decision, errDispatch := manager.Dispatch(t.Context(), []string{"gemini", "vertex"}, model, Options{Metadata: map[string]any{AllowedAuthIDsMetadataKey: allowed}})
			if errDispatch != nil || decision == nil {
				t.Fatalf("key %d: dispatch = %+v, %v", i, decision, errDispatch)
			}
			if decision.Provider == "gemini" {
				counts[i]++
			}
			if i == 1 && decision.Auth.ID == "a-1" {
				t.Fatal("narrower API key picked a disallowed credential")
			}
		}
	}
	if counts != [2]int{40, 30} {
		t.Fatalf("Gemini picks = %v, want [40 30] for the 2:1 and 1:1 pools", counts)
	}
}

func TestMixedRoundRobinStaggersNewAndRecreatedPools(t *testing.T) {
	scheduler := newAuthScheduler(&RoundRobinSelector{}, nil, nil)
	providers := []string{"gemini", "vertex"}
	scheduler.upsertAuth(&Auth{ID: "b", Provider: "vertex", Status: StatusActive})
	var pools []Options
	for i := range 7 {
		id := fmt.Sprintf("a-%d", i)
		scheduler.upsertAuth(&Auth{ID: id, Provider: "gemini", Status: StatusActive})
		pools = append(pools, Options{Metadata: map[string]any{AllowedAuthIDsMetadataKey: []string{id, "b"}}})
	}
	pick := func(i int) string {
		t.Helper()
		_, provider, errPick := scheduler.pickMixed(t.Context(), providers, "", pools[i], nil)
		if errPick != nil {
			t.Fatal(errPick)
		}
		return provider
	}
	for i := range pools {
		if got := pick(i); got != providers[i%2] {
			t.Fatalf("first pick for pool %d = %s, want %s", i, got, providers[i%2])
		}
	}
	for i := range pools {
		if got := pick(i); got != providers[(i+1)%2] {
			t.Fatalf("second pick for pool %d = %s, want %s", i, got, providers[(i+1)%2])
		}
	}
	// Simulate eviction of the first pool; recreating it must not start at slot zero.
	firstPool := mixedIDSetFingerprint(allowedAuthIDsFromOptions(pools[0]))
	for key := range scheduler.mixedCursors {
		if key.allowed == firstPool {
			delete(scheduler.mixedCursors, key)
			break
		}
	}
	if got := pick(0); got != "vertex" {
		t.Fatalf("recreated pool started at %s, want vertex", got)
	}
	if got := pick(0); got != "gemini" {
		t.Fatalf("recreated pool next pick = %s, want gemini", got)
	}
}

func TestDispatchBroadAllowlistPreservesContextErrorsWithTriedCredentials(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	const model = "mixed-canceled"
	for id, provider := range map[string]string{"cancel-tried": "gemini", "cancel-ready": "vertex"} {
		registerDispatchTestAuth(t, manager, &Auth{ID: id, Provider: provider, Status: StatusActive}, model)
	}
	allowed := []string{"cancel-ready", "cancel-tried"}
	for i := range 256 {
		allowed = append(allowed, fmt.Sprint("other-", i))
	}
	opts := Options{Metadata: map[string]any{
		AllowedAuthIDsMetadataKey: allowed, ExcludedAuthIDsMetadataKey: []string{"cancel-tried"}, RequestRetryRoundMetadataKey: 0,
	}}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	expired, closeDeadline := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer closeDeadline()
	for _, ctx := range []context.Context{canceled, expired} {
		decision, errDispatch := manager.Dispatch(ctx, []string{"gemini", "vertex"}, model, opts)
		if decision != nil || errDispatch != ctx.Err() {
			t.Fatalf("Dispatch() = %+v, %v; want %v unchanged", decision, errDispatch, ctx.Err())
		}
	}
}

func TestMixedBroadAllowlistSelectsWithExpiredRefreshBlockedCredential(t *testing.T) {
	scheduler := newAuthScheduler(&RoundRobinSelector{}, nil, nil)
	providers := []string{"gemini", "vertex"}
	allowed := make(map[string]struct{}, 129)
	for i := range 129 {
		id := fmt.Sprintf("broad-healthy-%03d", i)
		scheduler.upsertAuth(&Auth{ID: id, Provider: providers[i%2], Status: StatusActive})
		allowed[id] = struct{}{}
	}
	const blockedID = "broad-refresh-blocked"
	scheduler.upsertAuth(&Auth{
		ID: blockedID, Provider: "gemini", Status: StatusError, Unavailable: true,
		RuntimeRefreshBlocked: true, NextRetryAfter: time.Now().Add(-time.Hour),
	})

	// Inspect the version before selecting, so a regression fails deterministically
	// instead of hanging in the broad-allowlist snapshot-validation loop.
	snapshots := scheduler.mixedReadySnapshots(providers, "")
	for range 3 {
		checked := scheduler.mixedReadySnapshots(providers, "")
		for i, snapshot := range snapshots {
			if !snapshot.matches(checked[i].shard) {
				t.Fatal("no-op retry promotion invalidated an unchanged ready snapshot")
			}
		}
	}
	blocked := snapshots[0].shard.entries[blockedID]
	if blocked.state != scheduledStateBlocked || !RefreshBlocksDispatch(blocked.auth) {
		t.Fatal("expired retry time must not clear the refresh acquisition block")
	}

	for range 4 {
		picked, _, errPick := scheduler.pickMixedWithStrategy(t.Context(), providers, "", Options{}, nil, schedulerStrategyRoundRobin, allowed)
		if errPick != nil || picked == nil {
			t.Fatalf("broad pick = %v, %v; want a healthy credential", picked, errPick)
		}
		if _, ok := allowed[picked.ID]; !ok {
			t.Fatalf("selected credential outside the allowlist: %s", picked.ID)
		}
		if !snapshots[0].matches(scheduler.providers["gemini"].modelShards[""]) {
			t.Fatal("selection invalidated the unchanged refresh-blocked shard")
		}
	}
}

func TestModelSchedulerPromotionInvalidatesOnlyOnStateChange(t *testing.T) {
	now := time.Now()
	deadline := now.Add(time.Hour)
	auth := &Auth{ID: "promotion-cooling", Provider: "gemini", Status: StatusActive,
		ModelStates: map[string]*ModelState{"gpt": {
			Status: StatusError, Unavailable: true, NextRetryAfter: deadline,
			Quota: QuotaState{Exceeded: true, NextRecoverAt: deadline},
		}},
	}
	provider := &providerScheduler{auths: map[string]*scheduledAuthMeta{
		auth.ID: {auth: auth, supportedModelSet: map[string]struct{}{"gpt": {}}, registeredModelSet: map[string]struct{}{"gpt": {}}},
	}, modelShards: make(map[string]*modelScheduler)}
	shard := provider.ensureModelLocked("gpt", now)
	version := shard.readyVersion
	shard.promoteExpiredLocked(now)
	if shard.readyVersion != version {
		t.Fatal("unexpired cooldown changed the ready version")
	}
	shard.promoteExpiredLocked(deadline)
	if shard.readyVersion != version+1 || shard.entries[auth.ID].state != scheduledStateReady {
		t.Fatal("expired cooldown did not promote the credential and invalidate the snapshot")
	}
	shard.promoteExpiredLocked(deadline.Add(time.Second))
	if shard.readyVersion != version+1 {
		t.Fatal("already-promoted credential invalidated the snapshot again")
	}
}

func TestMixedBroadAllowlistHonorsCandidateAndRetryFilters(t *testing.T) {
	scheduler := newAuthScheduler(&RoundRobinSelector{}, nil, nil)
	providers := []string{"gemini", "vertex"}
	for _, auth := range []*Auth{
		{ID: "a-0", Provider: "gemini", Status: StatusActive, Metadata: map[string]any{"request_retry": 0}},
		{ID: "a-1", Provider: "gemini", Status: StatusActive},
		{ID: "b-1", Provider: "vertex", Status: StatusActive},
	} {
		scheduler.upsertAuth(auth)
	}
	snapshots := scheduler.mixedReadySnapshots(providers, "")
	full := newSchedulerPickFilter(Options{}, nil, map[string]struct{}{"a-0": {}, "a-1": {}, "b-1": {}})
	if pool := snapshots[0].filteredPool(full); pool.count != 2 || pool.positions != nil {
		t.Fatalf("complete ready bucket = %+v; want the original ready view", pool)
	}
	allowed := map[string]struct{}{"a-0": {}, "b-1": {}}
	for i := range 256 {
		allowed[fmt.Sprint("unrelated-", i)] = struct{}{}
	}
	opts := withRequestRetryDispatchMetadata(Options{}, 1, 1)
	for range 4 {
		picked, _, errPick := scheduler.pickMixedWithStrategy(t.Context(), providers, "", opts, nil, schedulerStrategyRoundRobin, allowed)
		if errPick != nil || picked == nil || picked.ID != "b-1" {
			t.Fatalf("pick = %v, %v; want b-1 after both candidate and retry filtering", picked, errPick)
		}
	}
	scheduler.removeAuth("a-0")
	if snapshots[0].matches(scheduler.providers["gemini"].modelShards[""]) {
		t.Fatal("ready snapshot should be invalidated by a credential removal")
	}
}

func BenchmarkMixedRoundRobinAllowlist(b *testing.B) {
	for _, tc := range []struct{ size, allowedPerProvider int }{{100, 1}, {10_000, 1}, {10_000, 4_500}, {10_000, 5_000}} {
		b.Run(fmt.Sprintf("pool=%d/allowed=%d", tc.size, tc.allowedPerProvider*2), func(b *testing.B) {
			scheduler := newAuthScheduler(&RoundRobinSelector{}, nil, nil)
			providers := []string{"gemini", "vertex"}
			var allowed []string
			for _, provider := range providers {
				for i := range tc.size / len(providers) {
					id := fmt.Sprintf("%s-%05d", provider, i)
					scheduler.upsertAuth(&Auth{ID: id, Provider: provider, Status: StatusActive})
					if i >= tc.size/len(providers)-tc.allowedPerProvider {
						allowed = append(allowed, id)
					}
				}
			}
			opts := Options{Metadata: map[string]any{AllowedAuthIDsMetadataKey: allowed}}
			if _, _, errWarmup := scheduler.pickMixed(b.Context(), providers, "", opts, nil); errWarmup != nil {
				b.Fatal(errWarmup)
			}
			b.ReportAllocs()
			for b.Loop() {
				if _, _, errPick := scheduler.pickMixed(b.Context(), providers, "", opts, nil); errPick != nil {
					b.Fatal(errPick)
				}
			}
		})
	}
}
