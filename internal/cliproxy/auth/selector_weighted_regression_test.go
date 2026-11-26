package auth

import (
	"fmt"
	"net/http"
	"testing"
)

func TestWeightedSessionAffinityPoolAndOverloadRegression(t *testing.T) {
	for _, deferred := range []bool{false, true} {
		for _, scenario := range []string{"lower-priority", "candidate-subset", "same-pool-overload", "same-pool-feasible"} {
			t.Run(fmt.Sprintf("%s/deferred=%t", scenario, deferred), func(t *testing.T) {
				weighted := &WeightedRoundRobinSelector{}
				selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: weighted})
				t.Cleanup(selector.Stop)
				auths := []*Auth{
					{ID: "a"}, {ID: "b", Attributes: map[string]string{"weight": "3"}}, {ID: "c"},
				}
				if scenario == "lower-priority" {
					auths[1].Attributes["priority"] = "1"
					auths[2].Attributes = map[string]string{"priority": "1"}
				}
				sticky := Options{Headers: http.Header{"X-Session-ID": {"bound-a"}}}
				id, _ := extractSessionIDs(sticky.Headers, nil, nil)
				key := sessionBindingKey("codex", id, "model")
				selector.cache.Set(key, "a")
				freshPerSticky := 1
				if scenario == "same-pool-feasible" {
					freshPerSticky = 4
				}
				counts := map[string]int{}
				for i := range 2000 {
					requests := []Options{}
					if i%freshPerSticky == 0 {
						requests = append(requests, sticky)
					}
					requests = append(requests, Options{Headers: http.Header{"X-Session-ID": {fmt.Sprintf("fresh-%d", i)}}})
					for j, opts := range requests {
						bound := len(requests) == 2 && j == 0
						pool := auths
						if !bound && scenario == "candidate-subset" {
							pool = auths[1:]
						}
						if deferred {
							opts.SessionAffinity = &SessionAffinitySelection{}
						}
						picked, errPick := selector.Pick(t.Context(), "codex", "model", opts, pool)
						if errPick != nil || picked == nil || (bound && picked.ID != "a") {
							t.Fatalf("request %d bound=%t: pick=%v err=%v", i, bound, picked, errPick)
						}
						if deferred {
							if swapped, errSwap := selector.store.CompareAndSwapSessionBinding(t.Context(), *opts.SessionAffinity.Commit); errSwap != nil || !swapped {
								t.Fatalf("binding commit=%t err=%v", swapped, errSwap)
							}
							opts.SessionAffinity.Finish(true)
						}
						if !bound && i >= 1000 {
							counts[picked.ID]++
						}
					}
				}
				t.Logf("fresh distribution: a=%d b=%d c=%d; want b=750 c=250", counts["a"], counts["b"], counts["c"])
				if counts["a"] != 0 || counts["b"] != 750 || counts["c"] != 250 {
					t.Errorf("fresh allocation = %v, want b=750 c=250", counts)
				}
				if bound, ok := selector.cache.Get(key); !ok || bound != "a" {
					t.Errorf("sticky binding moved: %q, found=%t", bound, ok)
				}
				for _, state := range weighted.states {
					if len(state.pendingForTest()) != 0 {
						t.Errorf("pending credit leaked: %v", state.pendingForTest())
					}
				}
			})
		}
	}
}
