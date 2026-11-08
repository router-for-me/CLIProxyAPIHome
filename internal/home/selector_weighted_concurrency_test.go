package home

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
)

func TestWeightedSessionAffinityConcurrentNewSessions(t *testing.T) {
	for _, weightA := range []int{1, 3} {
		for _, admitted := range []bool{false, true} {
			t.Run(fmt.Sprintf("weight=%d/admitted=%t", weightA, admitted), func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				defer cancel()
				store := coreauth.NewSessionCache(time.Hour)
				t.Cleanup(store.Stop)
				selector := coreauth.NewSessionAffinitySelectorWithConfig(coreauth.SessionAffinityConfig{
					Fallback: &coreauth.WeightedRoundRobinSelector{}, Store: store,
				})
				manager := coreauth.NewManager(nil, selector, nil)
				for i, id := range []string{"concurrent-weighted-a", "concurrent-weighted-b"} {
					weight := 1
					if i == 0 {
						weight = weightA
					}
					auth := &coreauth.Auth{ID: id, Provider: "openai", Status: coreauth.StatusActive,
						Attributes: map[string]string{"weight": fmt.Sprint(weight)}}
					registry.GetGlobalRegistry().RegisterClient(id, auth.Provider, []*registry.ModelInfo{{ID: "gpt"}})
					t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
					if _, errRegister := manager.Register(ctx, auth); errRegister != nil {
						t.Fatal(errRegister)
					}
				}
				runtime := &Runtime{coreManager: manager, sessionAffinityStore: store}
				if admitted {
					runtime.SetConcurrencyAdmitter(ConcurrencyAdmitterFunc(func(ctx context.Context, req ConcurrencyAdmissionRequest) (ConcurrencyAdmissionResult, error) {
						swapped, errSwap := store.CompareAndSwapSessionBinding(ctx, *req.SessionAffinity)
						if errSwap != nil {
							return ConcurrencyAdmissionResult{}, errSwap
						}
						if !swapped {
							return ConcurrencyAdmissionResult{}, coreauth.ErrSessionAffinityConflict
						}
						return ConcurrencyAdmissionResult{CredentialID: req.CredentialID, Model: req.Model, Accounted: true}, nil
					}))
				}
				const requests = 20
				selected := make(chan string, requests)
				release := make(chan struct{})
				results := make(chan error, requests)
				for i := range requests {
					go func() {
						_, errDispatch := runtime.dispatchWithOptions(ctx, "gpt", coreauth.Options{
							Headers: http.Header{"X-Session-ID": {fmt.Sprintf("concurrent-%d", i)}},
						}, DispatchConcurrencyContext{PrepareResponse: func(result *DispatchResult) ([]byte, []byte, error) {
							selected <- result.AuthID
							select {
							case <-release:
								return nil, nil, nil
							case <-ctx.Done():
								return nil, nil, ctx.Err()
							}
						}})
						results <- errDispatch
					}()
				}
				counts := map[string]int{}
				// No binding/admission may finish until every initial selection has
				// occurred. All 20 requests must see outstanding reservations.
				for range requests {
					select {
					case id := <-selected:
						counts[id]++
					case <-ctx.Done():
						t.Fatal("requests did not reach the preparation barrier")
					}
				}
				close(release)
				for range requests {
					select {
					case errDispatch := <-results:
						if errDispatch != nil {
							t.Fatal(errDispatch)
						}
					case <-ctx.Done():
						t.Fatal("dispatch did not complete")
					}
				}
				wantA := requests * weightA / (weightA + 1)
				if counts["concurrent-weighted-a"] != wantA || counts["concurrent-weighted-b"] != requests-wantA {
					t.Fatalf("concurrent distribution=%v, want %d:%d", counts, wantA, requests-wantA)
				}
			})
		}
	}
}

type weightedDispatchIneligibleError struct{}

func (weightedDispatchIneligibleError) Error() string { return "credential concurrency exceeded" }
func (weightedDispatchIneligibleError) ConcurrencyType() string {
	return "credential_concurrency_exceeded"
}

type weightedDispatchBindingStore struct {
	coreauth.SessionAffinityStore
	failure  error
	conflict bool
}

func (s *weightedDispatchBindingStore) CompareAndSwapSessionBinding(ctx context.Context, commit coreauth.SessionAffinityCommit) (bool, error) {
	if s.failure != nil {
		failure := s.failure
		s.failure = nil
		return false, failure
	}
	if s.conflict {
		s.conflict = false
		return false, nil
	}
	return s.SessionAffinityStore.CompareAndSwapSessionBinding(ctx, commit)
}
