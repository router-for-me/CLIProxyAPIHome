package auth

import (
	"fmt"
	"maps"
	"math/rand/v2"
	"net/http"
	"strconv"
	"testing"
	"time"
)

// weightedStateForTest checks that the requested accounting pool was created.
func weightedStateForTest(t *testing.T, selector *WeightedRoundRobinSelector, route string, ids ...string) *weightedPoolState {
	t.Helper()
	weights := make(map[string]int64, len(ids))
	for _, id := range ids {
		weights[id] = 1
	}
	state := selector.states[weightedPoolStateKey(route, weights)]
	ids = sortedWeightedIDs(weights)
	for state != nil && !state.matches(route, ids) {
		state = state.next
	}
	if state == nil {
		t.Fatalf("missing weighted state for %q and %v", route, ids)
	}
	return state
}

func TestWeightedPoolsResumeCreditsAndIsolatePendingReservations(t *testing.T) {
	weighted := &WeightedRoundRobinSelector{}
	auths := []*Auth{{ID: "a", Attributes: map[string]string{"weight": "3"}}, {ID: "b"}, {ID: "c"}}
	key := "codex:model"
	_, pendingFull := weighted.reserve(key, auths, "b")
	t.Cleanup(func() { pendingFull.finish(false) })
	_, pendingSubset := weighted.reserve(key, auths[:2], "b")
	t.Cleanup(func() { pendingSubset.finish(false) })
	pendingSubset.finish(true)
	subset := weightedStateForTest(t, weighted, key, "a", "b")
	full := weightedStateForTest(t, weighted, key, "a", "b", "c")
	if !maps.Equal(subset.currentForTest(), map[string]int64{"a": 3, "b": -3}) || full.hasCurrentForTest() {
		t.Fatalf("commit crossed pool boundary: subset=%v full=%v", subset.currentForTest(), full.currentForTest())
	}
	if !maps.Equal(full.pendingForTest(), map[string]int64{"a": 3, "b": -4, "c": 1}) {
		t.Fatalf("another pool consumed pending credit: %v", full.pendingForTest())
	}
	pendingFull.finish(false)
	for range 100 {
		_, reservation := weighted.reserve(key, auths, "b")
		reservation.finish(true)
	}
	// A returning pool resumes its debt, independent of full-pool traffic.
	for range 3 {
		picked, reservation := weighted.reserve(key, []*Auth{auths[1], auths[0]}, "")
		if picked == nil || picked.ID != "a" {
			t.Fatalf("returning subset lost credit: pick=%v credits=%v", picked, subset.currentForTest())
		}
		reservation.finish(true)
	}
	if !maps.Equal(subset.currentForTest(), map[string]int64{"a": 0, "b": 0}) || len(subset.pendingForTest()) != 0 || len(full.pendingForTest()) != 0 {
		t.Fatalf("returning subset failed to balance: %#v", subset)
	}
}

func TestWeightedPoolInterleavingMatchesIndependentSelectors(t *testing.T) {
	shared := &WeightedRoundRobinSelector{}
	auths := []*Auth{
		{ID: "a", Attributes: map[string]string{"weight": "1"}},
		{ID: "b", Attributes: map[string]string{"weight": "3"}},
		{ID: "c", Attributes: map[string]string{"weight": "2"}},
		{ID: "d", Attributes: map[string]string{"weight": "5"}},
	}
	var independent [16]WeightedRoundRobinSelector
	rng := rand.New(rand.NewPCG(17, 29))
	for step := range 5000 {
		mask := 1 + rng.IntN(15)
		var pool []*Auth
		for i, auth := range auths {
			if mask&(1<<i) != 0 {
				pool = append(pool, auth)
			}
		}
		// Membership and weights, not input order or other pools, define credit.
		rng.Shuffle(len(pool), func(i, j int) { pool[i], pool[j] = pool[j], pool[i] })
		boundID := ""
		if rng.IntN(3) == 0 {
			boundID = pool[rng.IntN(len(pool))].ID
		}
		picked, reservation := shared.reserve("route", pool, boundID)
		want, independentReservation := independent[mask].reserve("route", pool, boundID)
		if picked == nil || want == nil || picked.ID != want.ID {
			t.Fatalf("step %d pool %d: shared=%v independent=%v", step, mask, picked, want)
		}
		committed := rng.IntN(4) != 0
		reservation.finish(committed)
		independentReservation.finish(committed)
		poolKey := weightedPoolStateKey("route", authWeightVector(pool))
		state, independentState := shared.states[poolKey], independent[mask].states[poolKey]
		if !maps.Equal(state.currentForTest(), independentState.currentForTest()) || len(state.pendingForTest()) != 0 {
			t.Fatalf("step %d pool %d: credits diverged or pending leaked", step, mask)
		}
	}
}

func TestWeightedPoolKeysAndCapacity(t *testing.T) {
	seen := make(map[[32]byte]struct{})
	for _, tc := range []struct {
		route string
		ids   []string
	}{
		{route: "route", ids: []string{"a", "bc"}},
		{route: "route", ids: []string{"ab", "c"}},
		{route: "route1:a", ids: []string{"bc"}},
		{route: "route", ids: []string{"a1:b", "c"}},
	} {
		t.Run(fmt.Sprint(tc), func(t *testing.T) {
			weights := make(map[string]int64)
			for _, id := range tc.ids {
				weights[id] = 1
			}
			key := weightedPoolStateKey(tc.route, weights)
			if _, duplicate := seen[key]; duplicate {
				t.Fatal("different pool identities collided")
			}
			seen[key] = struct{}{}
			for _, id := range tc.ids {
				weights[id] = 3
			}
			if key != weightedPoolStateKey(tc.route, weights) {
				t.Fatal("weight change changed pool identity instead of resetting credit")
			}
		})
	}
	weighted := &WeightedRoundRobinSelector{maxKeys: 2}
	auths := []*Auth{{ID: "a"}, {ID: "b"}}
	_, old := weighted.reserve("route", auths, "b")
	t.Cleanup(func() { old.finish(false) })
	for _, auth := range auths {
		_, reservation := weighted.reserve("route", []*Auth{auth}, auth.ID)
		reservation.finish(true)
	}
	_, current := weighted.reserve("route", auths, "a")
	t.Cleanup(func() { current.finish(false) })
	if len(weighted.states) > weighted.maxKeys || old.state == current.state {
		t.Fatal("pool eviction failed to bound or replace state")
	}
	old.finish(true)
	if current.state.hasCurrentForTest() || !maps.Equal(current.state.pendingForTest(), map[string]int64{"a": -1, "b": 1}) {
		t.Fatal("finishing an evicted reservation affected the new pool")
	}
	current.finish(true)
}

func TestWeightedSessionAffinityChangingPoolsDoNotStarveEqualWeights(t *testing.T) {
	for _, newSession := range []bool{false, true} {
		t.Run(fmt.Sprintf("new-session=%t", newSession), func(t *testing.T) {
			weighted := &WeightedRoundRobinSelector{}
			selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: weighted})
			t.Cleanup(selector.Stop)
			auths := []*Auth{
				{ID: "a", Attributes: map[string]string{"priority": "0"}},
				{ID: "b", Attributes: map[string]string{"priority": "1"}},
				{ID: "c", Attributes: map[string]string{"priority": "1"}},
			}
			sticky := Options{Headers: http.Header{"X-Session-ID": {"lower-priority"}}}
			id, _ := extractSessionIDs(sticky.Headers, nil, nil)
			key := sessionBindingKey("codex", id, "model")
			selector.cache.Set(key, "a")
			counts := map[string]int{}
			const warmup, requests = 100, 1000
			for i := range requests {
				picked, errPick := selector.Pick(t.Context(), "codex", "model", sticky, auths)
				if errPick != nil || picked == nil || picked.ID != "a" {
					t.Fatalf("sticky request %d: pick=%v err=%v", i, picked, errPick)
				}
				opts := Options{}
				if newSession {
					opts.Headers = http.Header{"X-Session-ID": {fmt.Sprintf("fresh-%d", i)}}
				}
				picked, errPick = selector.Pick(t.Context(), "codex", "model", opts, auths)
				if errPick != nil || picked == nil || (picked.ID != "b" && picked.ID != "c") {
					t.Fatalf("fresh request %d: pick=%v err=%v", i, picked, errPick)
				}
				if i >= warmup {
					counts[picked.ID]++
				}
			}
			if counts["b"] != (requests-warmup)/2 || counts["c"] != (requests-warmup)/2 {
				t.Fatalf("changing pools starved equal-weight candidates after warmup: %v, want b=450 c=450", counts)
			}
			if binding, ok := selector.cache.Get(key); !ok || binding != "a" {
				t.Fatalf("balancing moved the sticky binding: %q, found=%t", binding, ok)
			}
			if state := weightedStateForTest(t, weighted, "codex:model", "b", "c"); len(state.pendingForTest()) != 0 {
				t.Fatalf("pending credits leaked: %v", state.pendingForTest())
			}
		})
	}
}

func TestWeightedSessionAffinityPoolIsolation(t *testing.T) {
	for _, reason := range []string{"priority", "retry-exclusion", "cooldown"} {
		for _, weights := range [][2]int{{1, 1}, {3, 1}, {1, 3}, {5, 2}} {
			t.Run(fmt.Sprintf("%s/%d:%d", reason, weights[0], weights[1]), func(t *testing.T) {
				weighted := &WeightedRoundRobinSelector{}
				selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: weighted})
				t.Cleanup(selector.Stop)
				auths := []*Auth{
					{ID: "a", Attributes: map[string]string{"priority": "0"}},
					{ID: "b", Attributes: map[string]string{"weight": strconv.Itoa(weights[0])}},
					{ID: "c", Attributes: map[string]string{"weight": strconv.Itoa(weights[1])}},
				}
				ctx := t.Context()
				if reason == "priority" {
					auths[1].Attributes["priority"] = "1"
					auths[2].Attributes["priority"] = "1"
				}
				sticky := Options{Headers: http.Header{"X-Session-ID": {"bound-a"}}}
				id, _ := extractSessionIDs(sticky.Headers, nil, nil)
				key := sessionBindingKey("codex", id, "model")
				selector.cache.Set(key, "a")
				const warmup, measured = 1000, 1400
				counts := map[string]int{}
				for i := range warmup + measured {
					picked, errPick := selector.Pick(ctx, "codex", "model", sticky, auths)
					if errPick != nil || picked == nil || picked.ID != "a" {
						t.Fatalf("sticky request %d: pick=%v err=%v", i, picked, errPick)
					}
					freshAuths := auths
					if reason == "retry-exclusion" {
						freshAuths = auths[1:]
					} else if reason == "cooldown" {
						auths[0].ModelStates = map[string]*ModelState{"model": {Unavailable: true, NextRetryAfter: time.Now().Add(time.Hour)}}
					}
					opts := Options{Headers: http.Header{"X-Session-ID": {fmt.Sprintf("fresh-%d", i)}}}
					picked, errPick = selector.Pick(ctx, "codex", "model", opts, freshAuths)
					if errPick != nil || picked == nil || (picked.ID != "b" && picked.ID != "c") {
						t.Fatalf("fresh request %d: pick=%v err=%v", i, picked, errPick)
					}
					if i >= warmup {
						counts[picked.ID]++
					}
					auths[0].ModelStates = nil
				}
				wantB := measured * weights[0] / (weights[0] + weights[1])
				if counts["b"] != wantB || counts["c"] != measured-wantB {
					t.Fatalf("cross-pool traffic distorted cold selections: %v, want b=%d c=%d", counts, wantB, measured-wantB)
				}
				if bound, ok := selector.cache.Get(key); !ok || bound != "a" {
					t.Fatalf("binding moved: %q, found=%t", bound, ok)
				}
				for _, state := range weighted.states {
					if len(state.pendingForTest()) != 0 {
						t.Fatalf("pending credits leaked: %v", state.pendingForTest())
					}
				}
			})
		}
	}
}
