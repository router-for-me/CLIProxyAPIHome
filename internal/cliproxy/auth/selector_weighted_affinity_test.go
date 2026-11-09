package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"
)

func TestWeightedSessionAffinityCompensatesStickyRequests(t *testing.T) {
	for _, inherit := range []bool{false, true} {
		t.Run(fmt.Sprintf("parent=%t", inherit), func(t *testing.T) {
			weighted := &WeightedRoundRobinSelector{}
			selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: weighted})
			t.Cleanup(selector.Stop)
			auths := []*Auth{
				{ID: "a", Attributes: map[string]string{"weight": "3"}},
				{ID: "b", Attributes: map[string]string{"weight": "1"}},
			}
			parent := Options{Headers: http.Header{"X-Session-ID": {"busy"}}}
			parentID, _ := extractSessionIDs(parent.Headers, nil, nil)
			key := sessionBindingKey("codex", parentID, "model")
			selector.cache.Set(key, "b")
			for i := range 4 {
				opts := parent
				if inherit {
					opts = Options{Headers: http.Header{
						"X-Session-ID": {fmt.Sprintf("child-%d", i)}, "X-Parent-Session-ID": {"busy"},
					}}
				}
				picked, errPick := selector.Pick(t.Context(), "codex", "model", opts, auths)
				if errPick != nil || picked == nil || picked.ID != "b" {
					t.Fatalf("sticky request %d: pick=%v err=%v", i, picked, errPick)
				}
			}
			state := weightedStateForTest(t, weighted, "codex:model", "a", "b")
			if state.currentForTest()["a"] != 12 || state.currentForTest()["b"] != -12 {
				t.Fatalf("sticky credits = %v, want a=12 b=-12", state.currentForTest())
			}
			for i := range 12 {
				// New sessions and requests without an identity share the same credits.
				opts := Options{}
				if i%2 == 0 {
					opts.Headers = http.Header{"X-Session-ID": {fmt.Sprintf("new-%d", i)}}
				}
				picked, errPick := selector.Pick(t.Context(), "codex", "model", opts, auths)
				if errPick != nil || picked == nil || picked.ID != "a" {
					t.Fatalf("compensating request %d: pick=%v err=%v", i, picked, errPick)
				}
			}
			if state.currentForTest()["a"] != 0 || state.currentForTest()["b"] != 0 {
				t.Fatalf("compensated credits = %v", state.currentForTest())
			}
			binding, errLoad := selector.store.LoadSessionBinding(t.Context(), key)
			if errLoad != nil || binding.AuthID != "b" {
				t.Fatalf("balancing moved existing binding: binding=%v err=%v", binding, errLoad)
			}
		})
	}
}

func TestWeightedSessionAffinityColdSelectionsCountOnce(t *testing.T) {
	weighted := &WeightedRoundRobinSelector{}
	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: weighted})
	t.Cleanup(selector.Stop)
	auths := []*Auth{{ID: "a", Attributes: map[string]string{"weight": "3"}}, {ID: "b"}}
	counts := map[string]int{}
	for i := range 40 {
		picked, errPick := selector.Pick(t.Context(), "codex", "model", Options{
			Headers: http.Header{"X-Session-ID": {fmt.Sprintf("session-%d", i)}},
		}, auths)
		if errPick != nil || picked == nil {
			t.Fatalf("pick=%v err=%v", picked, errPick)
		}
		state := weightedStateForTest(t, weighted, "codex:model", "a", "b")
		if i == 0 && (state.currentForTest()["a"] != -1 || state.currentForTest()["b"] != 1) {
			t.Fatalf("first selection charged more than once: %v", state.currentForTest())
		}
		counts[picked.ID]++
	}
	if counts["a"] != 30 || counts["b"] != 10 {
		t.Fatalf("cold distribution = %v", counts)
	}
}

func TestWeightedSessionAffinityBoundsCompensation(t *testing.T) {
	weighted := &WeightedRoundRobinSelector{}
	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: weighted})
	t.Cleanup(selector.Stop)
	auths := []*Auth{{ID: "a", Attributes: map[string]string{"weight": "3"}}, {ID: "b"}}
	opts := Options{Headers: http.Header{"X-Session-ID": {"long"}}}
	id, _ := extractSessionIDs(opts.Headers, nil, nil)
	selector.cache.Set(sessionBindingKey("codex", id, "model"), "b")
	for range 1000 {
		picked, errPick := selector.Pick(t.Context(), "codex", "model", opts, auths)
		if errPick != nil || picked == nil || picked.ID != "b" {
			t.Fatalf("long session: pick=%v err=%v", picked, errPick)
		}
	}
	state := weightedStateForTest(t, weighted, "codex:model", "a", "b")
	limit := maxSmoothWeightedCreditRounds * 4
	// Admit a whole sticky delta or none, stopping within one delta of the cap.
	wantCredit := limit / 3 * 3
	if state.currentForTest()["a"] != wantCredit || state.currentForTest()["b"] != -wantCredit {
		t.Fatalf("bounded credits = %v, want +/- %d", state.currentForTest(), wantCredit)
	}
	for range wantCredit {
		if _, errPick := weighted.Pick(t.Context(), "codex", "model", Options{}, auths); errPick != nil {
			t.Fatal(errPick)
		}
	}
	if state.currentForTest()["a"] != 0 || state.currentForTest()["b"] != 0 {
		t.Fatalf("compensation did not finish within bounded window: %v", state.currentForTest())
	}
}

type weightedAffinityRetryStore struct {
	SessionAffinityStore
	failures int
	winner   string
	err      error
}

func (s *weightedAffinityRetryStore) CompareAndSwapSessionBinding(ctx context.Context, commit SessionAffinityCommit) (bool, error) {
	if s.err != nil {
		return false, s.err
	}
	if s.failures > 0 {
		s.failures--
		if s.winner != "" {
			commit.AuthID = s.winner
			if _, errSwap := s.SessionAffinityStore.CompareAndSwapSessionBinding(ctx, commit); errSwap != nil {
				return false, errSwap
			}
		}
		return false, nil
	}
	return s.SessionAffinityStore.CompareAndSwapSessionBinding(ctx, commit)
}

func TestWeightedSessionAffinityCASAccounting(t *testing.T) {
	failure := errors.New("binding write failed")
	for _, tc := range []struct {
		name     string
		failures int
		winner   string
		err      error
		wantAuth string
		wantA    int64
		wantB    int64
	}{
		{name: "retry cold selection", failures: 3, wantAuth: "a", wantA: -1, wantB: 1},
		{name: "concurrent winner", failures: 1, winner: "b", wantAuth: "b", wantA: 3, wantB: -3},
		{name: "failed commit", err: failure},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cache := NewSessionCache(time.Hour)
			t.Cleanup(cache.Stop)
			store := &weightedAffinityRetryStore{SessionAffinityStore: cache, failures: tc.failures, winner: tc.winner, err: tc.err}
			weighted := &WeightedRoundRobinSelector{}
			selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: weighted, Store: store})
			picked, errPick := selector.Pick(t.Context(), "codex", "model", Options{Headers: http.Header{"X-Session-ID": {"cas"}}},
				[]*Auth{{ID: "a", Attributes: map[string]string{"weight": "3"}}, {ID: "b"}})
			if tc.err != nil {
				if !errors.Is(errPick, tc.err) || picked != nil {
					t.Fatalf("pick=%v err=%v, want commit failure", picked, errPick)
				}
			} else if errPick != nil || picked == nil || picked.ID != tc.wantAuth {
				t.Fatalf("pick=%v err=%v, want %s", picked, errPick, tc.wantAuth)
			}
			state := weightedStateForTest(t, weighted, "codex:model", "a", "b")
			if len(state.pendingForTest()) != 0 {
				t.Fatalf("CAS attempt leaked pending credits: %v", state.pendingForTest())
			}
			if state.currentForTest()["a"] != tc.wantA || state.currentForTest()["b"] != tc.wantB {
				t.Fatalf("credits = %v, want a=%d b=%d", state.currentForTest(), tc.wantA, tc.wantB)
			}
		})
	}
}

func TestWeightedSessionAffinityDefersAccounting(t *testing.T) {
	for _, source := range []string{"cold", "sticky", "parent"} {
		t.Run(source, func(t *testing.T) {
			weighted := &WeightedRoundRobinSelector{}
			selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: weighted})
			t.Cleanup(selector.Stop)
			opts := Options{Headers: http.Header{"X-Session-ID": {"session"}}, SessionAffinity: &SessionAffinitySelection{}}
			wantA, wantB := int64(-1), int64(1)
			if source != "cold" {
				parent := "session"
				if source == "parent" {
					parent = "parent"
					opts.Headers.Set("X-Parent-Session-ID", parent)
				}
				id, _ := extractSessionIDs(http.Header{"X-Session-ID": {parent}}, nil, nil)
				selector.cache.Set(sessionBindingKey("codex", id, "model"), "b")
				wantA, wantB = 3, -3
			}
			auths := []*Auth{{ID: "a", Attributes: map[string]string{"weight": "3"}}, {ID: "b"}}
			// Release the first selection, as admission must do on failure.
			for attempt := range 2 {
				picked, errPick := selector.Pick(t.Context(), "codex", "model", opts, auths)
				if errPick != nil || picked == nil || opts.SessionAffinity.reservation == nil {
					t.Fatalf("pick=%v err=%v, missing deferred accounting", picked, errPick)
				}
				for _, state := range weighted.states {
					if state.currentForTest()["a"] != 0 || state.currentForTest()["b"] != 0 {
						t.Fatalf("uncommitted selection consumed credits: %v", state.currentForTest())
					}
					if state.pendingForTest()["a"] != wantA || state.pendingForTest()["b"] != wantB {
						t.Fatalf("missing reservation: %v", state.pendingForTest())
					}
				}
				if attempt == 0 {
					opts.SessionAffinity.Finish(false)
					if len(weightedStateForTest(t, weighted, "codex:model", "a", "b").pendingForTest()) != 0 {
						t.Fatal("failed attempt leaked pending credits")
					}
				}
			}
			if swapped, errSwap := selector.store.CompareAndSwapSessionBinding(t.Context(), *opts.SessionAffinity.Commit); errSwap != nil || !swapped {
				t.Fatalf("commit=%t err=%v", swapped, errSwap)
			}
			opts.SessionAffinity.Finish(true)
			opts.SessionAffinity.Finish(true)
			opts.SessionAffinity.Finish(false)
			state := weightedStateForTest(t, weighted, "codex:model", "a", "b")
			if len(state.pendingForTest()) != 0 {
				t.Fatalf("confirmed selection leaked pending credits: %v", state.pendingForTest())
			}
			if state.currentForTest()["a"] != wantA || state.currentForTest()["b"] != wantB {
				t.Fatalf("committed credits = %v, want a=%d b=%d", state.currentForTest(), wantA, wantB)
			}
		})
	}
}

func TestWeightedSessionAffinitySharesEligibleModelCredits(t *testing.T) {
	weighted := &WeightedRoundRobinSelector{}
	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: weighted})
	t.Cleanup(selector.Stop)
	auths := []*Auth{
		{ID: "a", Attributes: map[string]string{"weight": "3", "priority": "1"}},
		{ID: "b", Attributes: map[string]string{"priority": "1"}},
		{ID: "lower-priority", Attributes: map[string]string{"weight": "100"}},
		{ID: "zero", Attributes: map[string]string{"weight": "0", "priority": "2"}},
		{ID: "cooldown", Attributes: map[string]string{"priority": "2"}, ModelStates: map[string]*ModelState{
			"upstream": {Unavailable: true, NextRetryAfter: time.Now().Add(time.Hour)},
		}},
	}
	opts := Options{Headers: http.Header{"X-Session-ID": {"alias"}}, Metadata: map[string]any{RequestedModelMetadataKey: "requested"}}
	id, _ := extractSessionIDs(opts.Headers, nil, nil)
	selector.cache.Set(sessionBindingKey("codex", id, "upstream"), "b")
	picked, errPick := selector.Pick(t.Context(), "codex", "upstream", opts, auths)
	if errPick != nil || picked == nil || picked.ID != "b" {
		t.Fatalf("sticky pick=%v err=%v", picked, errPick)
	}
	state := weightedStateForTest(t, weighted, "codex:requested", "a", "b")
	if len(weighted.states) != 1 || len(state.currentForTest()) != 2 || state.currentForTest()["a"] != 3 || state.currentForTest()["b"] != -3 {
		t.Fatalf("wrong state key or eligible credits: %v", weighted.states)
	}
	picked, errPick = weighted.Pick(t.Context(), "codex", "", opts, auths[:2])
	if errPick != nil || picked == nil || picked.ID != "a" || state.currentForTest()["a"] != 2 || state.currentForTest()["b"] != -2 {
		t.Fatalf("fallback did not share credits: pick=%v err=%v credits=%v", picked, errPick, state.currentForTest())
	}
	if _, errOther := weighted.Pick(t.Context(), "other", "", opts, auths[:2]); errOther != nil {
		t.Fatal(errOther)
	}
	if state.currentForTest()["a"] != 2 || state.currentForTest()["b"] != -2 {
		t.Fatal("another provider changed credits")
	}
}
