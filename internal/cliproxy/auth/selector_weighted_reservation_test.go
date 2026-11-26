package auth

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

func TestWeightedReservationRollbackPreservesConcurrentCharges(t *testing.T) {
	weighted := &WeightedRoundRobinSelector{}
	auths := []*Auth{{ID: "a", Attributes: map[string]string{"weight": "3"}}, {ID: "b"}}
	key := "codex:model"
	// Reach the clamp before reserving: reversing a clamped update would not
	// restore the original credit, especially after another request commits.
	for range 1000 {
		_, reservation := weighted.reserve(key, auths, "b")
		reservation.finish(true)
	}
	state := weightedStateForTest(t, weighted, key, "a", "b")
	_, rejected := weighted.reserve(key, auths, "b")
	_, accepted := weighted.reserve(key, auths, "a")
	accepted.finish(true)
	rejected.finish(false)
	rejected.finish(true)
	accepted.finish(false)
	limit := maxSmoothWeightedCreditRounds * 4 / 3 * 3
	if state.currentForTest()["a"] != limit-1 || state.currentForTest()["b"] != -limit+1 || len(state.pendingForTest()) != 0 {
		t.Fatalf("rollback lost another charge: committed=%v pending=%v", state.currentForTest(), state.pendingForTest())
	}
}

func TestWeightedSessionAffinitySelectionReset(t *testing.T) {
	var absent *SessionAffinitySelection
	absent.Reset()
	var empty SessionAffinitySelection
	empty.Reset()
	empty.Reset()

	for _, committed := range []bool{false, true} {
		name := "pending"
		if committed {
			name = "committed"
		}
		t.Run(name, func(t *testing.T) {
			weighted := &WeightedRoundRobinSelector{}
			auths := []*Auth{{ID: "a", Attributes: map[string]string{"weight": "3"}}, {ID: "b"}}
			key := "codex:model"
			_, reservation := weighted.reserve(key, auths, "a")
			t.Cleanup(func() { reservation.finish(false) })
			store := NewSessionCache(time.Hour)
			t.Cleanup(store.Stop)
			selection := &SessionAffinitySelection{
				Store: store, Commit: &SessionAffinityCommit{Key: "binding", AuthID: "a"}, reservation: reservation,
			}
			if committed {
				selection.Finish(true)
			}
			// Reset must not release credit owned by another in-flight request.
			_, other := weighted.reserve(key, auths, "b")
			t.Cleanup(func() { other.finish(false) })
			selection.Reset()
			selection.Reset()
			selection.Finish(true)
			if selection.Store != nil || selection.Commit != nil || selection.reservation != nil {
				t.Fatalf("reset retained selection metadata: %#v", selection)
			}
			state := weightedStateForTest(t, weighted, key, "a", "b")
			wantA, wantB := int64(0), int64(0)
			if committed {
				wantA, wantB = -1, 1
			}
			if !reservation.finished || state.currentForTest()["a"] != wantA || state.currentForTest()["b"] != wantB {
				t.Fatalf("reset changed committed credits or lost reservation: finished=%t credits=%v", reservation.finished, state.currentForTest())
			}
			if len(state.pendingForTest()) != 2 || state.pendingForTest()["a"] != 3 || state.pendingForTest()["b"] != -3 {
				t.Fatalf("reset leaked credit or released another reservation: %v", state.pendingForTest())
			}
			other.finish(false)
			if len(state.pendingForTest()) != 0 {
				t.Fatalf("pending credits remain after releasing all reservations: %v", state.pendingForTest())
			}
		})
	}
}

func TestWeightedReservationResetInvalidatesOldGeneration(t *testing.T) {
	weighted := &WeightedRoundRobinSelector{}
	auths := []*Auth{{ID: "a", Attributes: map[string]string{"weight": "3"}}, {ID: "b"}}
	key := "codex:model"
	_, old := weighted.reserve(key, auths, "")
	auths[0].Attributes["weight"] = "1"
	_, latest := weighted.reserve(key, auths, "")
	old.finish(true)
	state := weightedStateForTest(t, weighted, key, "a", "b")
	if state.currentForTest()["a"] != 0 || state.currentForTest()["b"] != 0 || state.pendingForTest()["a"] != -1 || state.pendingForTest()["b"] != 1 {
		t.Fatalf("stale reservation changed new generation: committed=%v pending=%v", state.currentForTest(), state.pendingForTest())
	}
	latest.finish(true)
	if state.currentForTest()["a"] != -1 || state.currentForTest()["b"] != 1 || len(state.pendingForTest()) != 0 {
		t.Fatalf("new generation accounting: committed=%v pending=%v", state.currentForTest(), state.pendingForTest())
	}
}

type weightedReservationResolver struct {
	auths map[string]*Auth
	err   error
}

func (r *weightedReservationResolver) GetFullAuth(_ context.Context, id string) (*Auth, error) {
	return r.auths[id], r.err
}

func TestWeightedReservationManagerReleasesRejectedCandidates(t *testing.T) {
	for _, mode := range []string{"resolver-failure", "missing-auth", "revalidation"} {
		t.Run(mode, func(t *testing.T) {
			weighted := &WeightedRoundRobinSelector{}
			selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: weighted})
			t.Cleanup(selector.Stop)
			manager := NewManager(nil, selector, nil)
			a := &Auth{ID: "reservation-manager-a", Provider: "codex", Status: StatusActive}
			b := &Auth{ID: "reservation-manager-b", Provider: "codex", Status: StatusActive}
			registerDispatchTestAuth(t, manager, a, "model")
			registerDispatchTestAuth(t, manager, b, "model")
			resolver := &weightedReservationResolver{auths: map[string]*Auth{b.ID: b}}
			failure := errors.New("full auth unavailable")
			if mode == "resolver-failure" {
				resolver.err = failure
			} else if mode == "revalidation" {
				full := a.Clone()
				full.Disabled = true
				resolver.auths[a.ID] = full
			}
			manager.SetFullAuthResolver(resolver)
			selection := &SessionAffinitySelection{}
			t.Cleanup(func() { selection.Finish(false) })
			decision, errDispatch := manager.Dispatch(t.Context(), []string{"codex"}, "model", Options{
				Headers: http.Header{"X-Session-ID": {"manager"}}, SessionAffinity: selection,
			})
			state := weightedStateForTest(t, weighted, "codex:model", a.ID, b.ID)
			if mode == "resolver-failure" {
				if !errors.Is(errDispatch, failure) || decision != nil || len(state.pendingForTest()) != 0 {
					t.Fatalf("failed resolution: decision=%v err=%v pending=%v", decision, errDispatch, state.pendingForTest())
				}
			} else {
				if errDispatch != nil || decision == nil || decision.Auth.ID != b.ID || len(state.pendingForTest()) != 0 {
					// The surviving singleton's net reservation is zero. A leaked
					// reservation for a would still be visible in this map.
					t.Fatalf("reselection: decision=%v err=%v pending=%v", decision, errDispatch, state.pendingForTest())
				}
			}
			selection.Finish(false)
			if state.currentForTest()[a.ID] != 0 || state.currentForTest()[b.ID] != 0 || len(state.pendingForTest()) != 0 {
				t.Fatalf("rejected candidate consumed credits: committed=%v pending=%v", state.currentForTest(), state.pendingForTest())
			}
		})
	}
}
