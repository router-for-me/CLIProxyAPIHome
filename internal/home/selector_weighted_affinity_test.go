package home

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
)

func TestWeightedSessionAffinityDispatchCommitAccounting(t *testing.T) {
	for _, mode := range []string{"standalone", "admitted", "custom-admitted", "sticky", "conflict", "binding-conflict", "binding-failure", "admission-failure", "custom-admission-failure", "ineligible", "prepare-failure", "cancelled", "accounted-mismatch", "unaccounted-mismatch"} {
		t.Run(mode, func(t *testing.T) {
			cache := coreauth.NewSessionCache(time.Hour)
			t.Cleanup(cache.Stop)
			failure := errors.New("dispatch preparation/admission failed")
			var store coreauth.SessionAffinityStore = cache
			if mode == "binding-failure" || mode == "binding-conflict" {
				store = &weightedDispatchBindingStore{SessionAffinityStore: cache, conflict: mode == "binding-conflict"}
				if mode == "binding-failure" {
					store.(*weightedDispatchBindingStore).failure = failure
				}
			}
			selector := coreauth.NewSessionAffinitySelectorWithConfig(coreauth.SessionAffinityConfig{
				Fallback: &coreauth.WeightedRoundRobinSelector{}, Store: store,
			})
			manager := coreauth.NewManager(nil, selector, nil)
			for _, id := range []string{"weighted-commit-a", "weighted-commit-b"} {
				auth := &coreauth.Auth{ID: id, Provider: "openai", Status: coreauth.StatusActive}
				registry.GetGlobalRegistry().RegisterClient(id, auth.Provider, []*registry.ModelInfo{{ID: "gpt"}})
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
				if _, errRegister := manager.Register(t.Context(), auth); errRegister != nil {
					t.Fatal(errRegister)
				}
			}
			runtime := &Runtime{coreManager: manager, sessionAffinityStore: store}
			if mode == "custom-admitted" || mode == "custom-admission-failure" {
				runtime.sessionAffinityStore = nil
			}
			calls := 0
			withAdmitter := mode != "standalone" && mode != "prepare-failure" && mode != "cancelled" && mode != "binding-failure" && mode != "binding-conflict"
			if withAdmitter {
				runtime.SetConcurrencyAdmitter(ConcurrencyAdmitterFunc(func(ctx context.Context, req ConcurrencyAdmissionRequest) (ConcurrencyAdmissionResult, error) {
					calls++
					if (mode == "admission-failure" || mode == "custom-admission-failure") && calls == 1 {
						return ConcurrencyAdmissionResult{}, failure
					}
					if mode == "ineligible" && calls == 1 {
						return ConcurrencyAdmissionResult{}, weightedDispatchIneligibleError{}
					}
					if mode == "conflict" && calls == 1 {
						return ConcurrencyAdmissionResult{}, coreauth.ErrSessionAffinityConflict
					}
					if mode == "custom-admitted" || mode == "custom-admission-failure" {
						if req.SessionAffinity != nil {
							t.Fatal("local binding must commit before custom admission")
						}
					} else {
						if req.SessionAffinity == nil {
							t.Fatal("missing deferred binding")
						}
						if swapped, errSwap := store.CompareAndSwapSessionBinding(ctx, *req.SessionAffinity); errSwap != nil || !swapped {
							t.Fatalf("binding commit=%t err=%v", swapped, errSwap)
						}
					}
					result := ConcurrencyAdmissionResult{CredentialID: req.CredentialID, Model: req.Model, Accounted: true}
					if (mode == "accounted-mismatch" || mode == "unaccounted-mismatch") && calls == 1 {
						result.CredentialID = "unexpected-credential"
						result.Accounted = mode == "accounted-mismatch"
					}
					return result, nil
				}))
			}
			concurrencyCtx := DispatchConcurrencyContext{}
			if mode == "prepare-failure" {
				concurrencyCtx.PrepareResponse = func(*DispatchResult) ([]byte, []byte, error) {
					return nil, nil, failure
				}
			}
			ctx := t.Context()
			if mode == "cancelled" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				defer cancel()
				failure = context.Canceled
				concurrencyCtx.PrepareResponse = func(*DispatchResult) ([]byte, []byte, error) {
					cancel()
					return nil, nil, ctx.Err()
				}
			}
			first, errFirst := runtime.dispatchWithOptions(ctx, "gpt", coreauth.Options{
				Headers: http.Header{"X-Session-ID": {"first"}},
			}, concurrencyCtx)
			wantNext := "weighted-commit-b"
			if mode == "unaccounted-mismatch" {
				if errFirst == nil || first != nil {
					t.Fatalf("first dispatch=%v err=%v, want admission mismatch", first, errFirst)
				}
				wantNext = "weighted-commit-a"
			} else if mode == "admission-failure" || mode == "custom-admission-failure" || mode == "prepare-failure" || mode == "binding-failure" || mode == "cancelled" {
				if !errors.Is(errFirst, failure) || first != nil {
					t.Fatalf("first dispatch=%v err=%v, want failure", first, errFirst)
				}
				wantNext = "weighted-commit-a"
			} else {
				wantFirst := "weighted-commit-a"
				if mode == "ineligible" {
					wantFirst, wantNext = "weighted-commit-b", "weighted-commit-a"
				}
				if errFirst != nil || first == nil || first.AuthID != wantFirst {
					t.Fatalf("first dispatch=%v err=%v", first, errFirst)
				}
				if mode == "accounted-mismatch" && first.AdmissionFenceFailure == nil {
					t.Fatal("missing admission fence failure")
				}
			}
			if mode == "conflict" && calls != 2 {
				t.Fatalf("admission attempts=%d, want 2", calls)
			}
			newSessions := 1
			if mode == "sticky" {
				for range 3 {
					sticky, errSticky := runtime.dispatchWithOptions(t.Context(), "gpt", coreauth.Options{
						Headers: http.Header{"X-Session-ID": {"first"}},
					}, DispatchConcurrencyContext{})
					if errSticky != nil || sticky == nil || sticky.AuthID != "weighted-commit-a" {
						t.Fatalf("sticky dispatch=%v err=%v", sticky, errSticky)
					}
				}
				newSessions = 4
			}
			for i := range newSessions {
				next, errNext := runtime.dispatchWithOptions(t.Context(), "gpt", coreauth.Options{
					Headers: http.Header{"X-Session-ID": {fmt.Sprintf("next-%d", i)}},
				}, DispatchConcurrencyContext{})
				if errNext != nil || next == nil || next.AuthID != wantNext {
					t.Fatalf("next dispatch=%v err=%v, want %s", next, errNext, wantNext)
				}
			}
		})
	}
}
