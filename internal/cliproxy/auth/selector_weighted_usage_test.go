package auth

import (
	"fmt"
	"maps"
	"sync"
	"testing"
)

func TestWeightedUsageSharesDispatchCredits(t *testing.T) {
	for _, affinity := range []bool{false, true} {
		t.Run(fmt.Sprintf("affinity=%t", affinity), func(t *testing.T) {
			weighted := &WeightedRoundRobinSelector{}
			var selector Selector = weighted
			if affinity {
				wrapped := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: weighted})
				t.Cleanup(wrapped.Stop)
				selector = wrapped
			}
			manager := NewManager(nil, selector, nil)
			for _, auth := range []*Auth{
				{ID: "usage-a", Index: "usage-index-a", Provider: "codex", Attributes: map[string]string{"weight": "3"}},
				{ID: "usage-b", Index: "usage-index-b", Provider: "codex"},
			} {
				registerDispatchTestAuth(t, manager, auth, "model")
			}
			// The first turn is accounted by dispatch, not by its usage.
			decision, errDispatch := manager.Dispatch(t.Context(), []string{"codex"}, "model", Options{Metadata: map[string]any{PinnedAuthIDMetadataKey: "usage-b"}})
			if errDispatch != nil || decision.Auth.ID != "usage-b" {
				t.Fatalf("pinned dispatch: decision=%v err=%v", decision, errDispatch)
			}
			for turn := range 3 {
				var wg sync.WaitGroup
				for range 8 {
					wg.Go(func() {
						manager.AccountReusedSelection(t.Context(), []string{"codex"}, "model", "usage-index-b", fmt.Sprintf("turn-%d", turn), Options{})
					})
				}
				wg.Wait()
			}
			state := weightedStateForTest(t, weighted, "codex:model", "usage-a", "usage-b")
			if !maps.Equal(state.currentForTest(), map[string]int64{"usage-a": 12, "usage-b": -12}) {
				t.Fatalf("usage missing, duplicated, or on another ledger: %v", state.currentForTest())
			}
			for i := range 12 {
				decision, errDispatch = manager.Dispatch(t.Context(), []string{"codex"}, "model", Options{})
				if errDispatch != nil || decision.Auth.ID != "usage-a" {
					t.Fatalf("repayment %d: decision=%v err=%v", i, decision, errDispatch)
				}
			}
			if state.hasCurrentForTest() || state.inFlight != 0 {
				t.Fatalf("debt did not settle: %v", state.currentForTest())
			}
		})
	}
}

func TestWeightedUsageRespectsPoolRestrictions(t *testing.T) {
	weighted := &WeightedRoundRobinSelector{}
	manager := NewManager(nil, weighted, nil)
	for _, auth := range []*Auth{
		{ID: "restricted-a", Provider: "codex", Attributes: map[string]string{"weight": "3"}},
		{ID: "restricted-b", Index: "restricted-index-b", Provider: "codex"},
		{ID: "restricted-high", Provider: "codex", Attributes: map[string]string{"priority": "10"}},
		{ID: "restricted-hidden", Provider: "codex"},
		{ID: "restricted-zero", Provider: "codex", Attributes: map[string]string{"weight": "0"}},
	} {
		registerDispatchTestAuth(t, manager, auth, "model")
	}
	opts := Options{Metadata: map[string]any{
		AllowedAuthIDsMetadataKey:  []string{"restricted-a", "restricted-b", "restricted-high", "restricted-zero"},
		AllowedModelIDsMetadataKey: []string{"model"},
	}}
	manager.AccountReusedSelection(t.Context(), []string{"codex"}, "model", "restricted-index-b", "turn", opts)
	state := weightedStateForTest(t, weighted, "codex:model", "restricted-a", "restricted-b")
	if !maps.Equal(state.currentForTest(), map[string]int64{"restricted-a": 3, "restricted-b": -3}) || weighted.stateCount != 1 {
		t.Fatalf("crossed a priority/permission/weight boundary: %v", state.currentForTest())
	}
	opts.Metadata[AllowedModelIDsMetadataKey] = []string{}
	manager.AccountReusedSelection(t.Context(), []string{"codex"}, "model", "restricted-index-b", "denied-model", opts)
	opts.Metadata[AllowedModelIDsMetadataKey] = []string{"model"}
	opts.Metadata[AllowedAuthIDsMetadataKey] = []string{"restricted-a"}
	manager.AccountReusedSelection(t.Context(), []string{"codex"}, "model", "restricted-index-b", "denied-auth", opts)
	if state.currentForTest()["restricted-a"] != 3 {
		t.Fatal("denied usage changed credits")
	}
}

func TestWeightedUsageReplayCacheIsBounded(t *testing.T) {
	weighted := &WeightedRoundRobinSelector{}
	auths := []*Auth{{ID: "a"}, {ID: "b"}}
	for i := range maxWeightedUsageEvents + 10 {
		weighted.accountUsage("route", auths, "b", fmt.Sprint(i))
	}
	if len(weighted.usageSeen) != maxWeightedUsageEvents || len(weighted.usageOrder) != maxWeightedUsageEvents {
		t.Fatalf("unbounded replay cache: seen=%d order=%d", len(weighted.usageSeen), len(weighted.usageOrder))
	}
}
