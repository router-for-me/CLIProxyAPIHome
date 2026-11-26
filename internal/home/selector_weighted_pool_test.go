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

func TestWeightedSessionAffinityDispatchPoolIsolationAndOverload(t *testing.T) {
	for _, admitted := range []bool{false, true} {
		for _, scenario := range []string{"priority", "allowlist", "retry-exclusion", "overload", "feasible"} {
			t.Run(fmt.Sprintf("%s/admitted=%t", scenario, admitted), func(t *testing.T) {
				store := coreauth.NewSessionCache(time.Hour)
				t.Cleanup(store.Stop)
				selector := coreauth.NewSessionAffinitySelectorWithConfig(coreauth.SessionAffinityConfig{
					Fallback: &coreauth.WeightedRoundRobinSelector{}, Store: store,
				})
				manager := coreauth.NewManager(nil, selector, nil)
				auths := []*coreauth.Auth{
					{ID: "weighted-pool-a", Provider: "openai", Status: coreauth.StatusActive},
					{ID: "weighted-pool-b", Provider: "openai", Status: coreauth.StatusActive, Attributes: map[string]string{"weight": "3"}},
					{ID: "weighted-pool-c", Provider: "openai", Status: coreauth.StatusActive, Attributes: map[string]string{}},
				}
				if scenario == "priority" {
					auths[1].Attributes["priority"] = "1"
					auths[2].Attributes["priority"] = "1"
				}
				for _, auth := range auths {
					registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "gpt"}})
					t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
					if _, errRegister := manager.Register(t.Context(), auth); errRegister != nil {
						t.Fatal(errRegister)
					}
				}
				runtime := &Runtime{coreManager: manager, sessionAffinityStore: store}
				manager.SetPluginScheduler(runtime)
				if admitted {
					runtime.SetConcurrencyAdmitter(ConcurrencyAdmitterFunc(func(ctx context.Context, req ConcurrencyAdmissionRequest) (ConcurrencyAdmissionResult, error) {
						if swapped, errSwap := store.CompareAndSwapSessionBinding(ctx, *req.SessionAffinity); errSwap != nil {
							return ConcurrencyAdmissionResult{}, errSwap
						} else if !swapped {
							return ConcurrencyAdmissionResult{}, coreauth.ErrSessionAffinityConflict
						}
						return ConcurrencyAdmissionResult{CredentialID: req.CredentialID, Model: req.Model, Accounted: true}, nil
					}))
				}
				sticky := coreauth.Options{Headers: http.Header{"X-Session-ID": {"busy"}}}
				seed := sticky
				seed.Metadata = map[string]any{coreauth.AllowedAuthIDsMetadataKey: []string{auths[0].ID}}
				first, errFirst := runtime.dispatchWithOptions(t.Context(), "gpt", seed, DispatchConcurrencyContext{})
				if errFirst != nil || first == nil || first.AuthID != auths[0].ID {
					t.Fatalf("seed binding: result=%v err=%v", first, errFirst)
				}
				freshPerSticky := 1
				if scenario == "feasible" {
					freshPerSticky = 4
				}
				counts := map[string]int{}
				for i := range 2000 {
					if i%freshPerSticky == 0 {
						bound, errBound := runtime.dispatchWithOptions(t.Context(), "gpt", sticky, DispatchConcurrencyContext{})
						if errBound != nil || bound == nil || bound.AuthID != auths[0].ID {
							t.Fatalf("sticky dispatch %d: result=%v err=%v", i, bound, errBound)
						}
					}
					opts := coreauth.Options{Headers: http.Header{"X-Session-ID": {fmt.Sprintf("fresh-%d", i)}}}
					switch scenario {
					case "allowlist":
						opts.Metadata = map[string]any{coreauth.AllowedAuthIDsMetadataKey: []string{auths[1].ID, auths[2].ID}}
					case "retry-exclusion":
						opts.Metadata = map[string]any{coreauth.ExcludedAuthIDsMetadataKey: []string{auths[0].ID}}
					}
					fresh, errFresh := runtime.dispatchWithOptions(t.Context(), "gpt", opts, DispatchConcurrencyContext{})
					if errFresh != nil || fresh == nil {
						t.Fatalf("fresh dispatch %d: result=%v err=%v", i, fresh, errFresh)
					}
					if i >= 1000 {
						counts[fresh.AuthID]++
					}
				}
				if counts[auths[0].ID] != 0 || counts[auths[1].ID] != 750 || counts[auths[2].ID] != 250 {
					t.Fatalf("fresh dispatch distribution=%v, want b=750 c=250", counts)
				}
			})
		}
	}
}

func TestWeightedSessionAffinityPinnedDispatchCompensation(t *testing.T) {
	for _, admitted := range []bool{false, true} {
		for _, channelScoped := range []bool{false, true} {
			for _, mode := range []string{"sticky", "cold", "no-session"} {
				t.Run(fmt.Sprintf("%s/channel=%t/admitted=%t", mode, channelScoped, admitted), func(t *testing.T) {
					store := coreauth.NewSessionCache(time.Hour)
					t.Cleanup(store.Stop)
					selector := coreauth.NewSessionAffinitySelectorWithConfig(coreauth.SessionAffinityConfig{
						Fallback: &coreauth.WeightedRoundRobinSelector{}, Store: store,
					})
					manager := coreauth.NewManager(nil, selector, nil)
					aID, bID, cID := t.Name()+"-a", t.Name()+"-b", t.Name()+"-c"
					lowerID := t.Name() + "-lower"
					auths := []*coreauth.Auth{
						{ID: aID, Provider: "openai", Status: coreauth.StatusActive, Attributes: map[string]string{"weight": "3", "priority": "1"}},
						{ID: bID, Provider: "openai", Status: coreauth.StatusActive, Attributes: map[string]string{"weight": "1", "priority": "1"}},
						{ID: lowerID, Provider: "openai", Status: coreauth.StatusActive, Attributes: map[string]string{"weight": "100", "priority": "0"}},
					}
					if channelScoped {
						auths = append(auths, &coreauth.Auth{ID: cID, Provider: "openai", Status: coreauth.StatusActive, Attributes: map[string]string{"weight": "100", "priority": "1"}})
					}
					for _, auth := range auths {
						registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "gpt"}})
						t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
						if _, errRegister := manager.Register(t.Context(), auth); errRegister != nil {
							t.Fatal(errRegister)
						}
					}
					runtime := &Runtime{coreManager: manager, sessionAffinityStore: store}
					manager.SetPluginScheduler(runtime)
					if channelScoped {
						runtime.clusterAdapter = &modelChannelDispatchTestAdapter{
							allowedAuthIDs:  []string{aID, bID, lowerID},
							allowedModelIDs: []string{"gpt"},
						}
					}
					if admitted {
						runtime.SetConcurrencyAdmitter(ConcurrencyAdmitterFunc(func(ctx context.Context, req ConcurrencyAdmissionRequest) (ConcurrencyAdmissionResult, error) {
							if req.SessionAffinity != nil {
								if swapped, errSwap := store.CompareAndSwapSessionBinding(ctx, *req.SessionAffinity); errSwap != nil {
									return ConcurrencyAdmissionResult{}, errSwap
								} else if !swapped {
									return ConcurrencyAdmissionResult{}, coreauth.ErrSessionAffinityConflict
								}
							}
							return ConcurrencyAdmissionResult{CredentialID: req.CredentialID, Model: req.Model, Accounted: true}, nil
						}))
					}
					stickyHeaders := http.Header{"X-Session-ID": {"pinned-session"}}
					if mode == "sticky" {
						seed, errSeed := runtime.DispatchForAPIKeyWithConcurrency(t.Context(), "gpt", stickyHeaders, "client-key", "", nil, bID, DispatchConcurrencyContext{})
						if errSeed != nil || seed == nil || seed.AuthID != bID {
							t.Fatalf("seed pinned binding: result=%v err=%v", seed, errSeed)
						}
					}
					pinnedCount := 4
					if mode == "sticky" {
						pinnedCount = 3
					}
					for i := range pinnedCount {
						var headers http.Header
						if mode != "no-session" {
							headers = stickyHeaders
						}
						res, errDispatch := runtime.DispatchForAPIKeyWithConcurrency(t.Context(), "gpt", headers, "client-key", "", nil, bID, DispatchConcurrencyContext{})
						if errDispatch != nil || res == nil || res.AuthID != bID {
							t.Fatalf("pinned dispatch %d: result=%v err=%v", i, res, errDispatch)
						}
					}
					for _, invalidPin := range []string{cID, "missing"} {
						if res, errInvalid := runtime.DispatchForAPIKeyWithConcurrency(t.Context(), "gpt", stickyHeaders, "client-key", "", nil, invalidPin, DispatchConcurrencyContext{}); errInvalid == nil || res != nil {
							t.Fatalf("invalid pin %q succeeded: result=%v err=%v", invalidPin, res, errInvalid)
						}
					}
					if res, errLower := runtime.DispatchForAPIKeyWithConcurrency(t.Context(), "gpt", http.Header{"X-Session-ID": {"lower-pinned"}}, "client-key", "", nil, lowerID, DispatchConcurrencyContext{}); errLower != nil || res == nil || res.AuthID != lowerID {
						t.Fatalf("lower-priority pin: result=%v err=%v", res, errLower)
					}
					counts := map[string]int{}
					for i := range 12 {
						res, errFresh := runtime.DispatchForAPIKeyWithConcurrency(t.Context(), "gpt", http.Header{"X-Session-ID": {fmt.Sprintf("fresh-%d", i)}}, "client-key", "", nil, "", DispatchConcurrencyContext{})
						if errFresh != nil || res == nil {
							t.Fatalf("fresh dispatch %d: result=%v err=%v", i, res, errFresh)
						}
						counts[res.AuthID]++
					}
					if counts[aID] != 12 || counts[bID] != 0 {
						t.Fatalf("fresh compensation counts=%v, want a=12 b=0", counts)
					}
					if mode != "no-session" {
						res, errSticky := runtime.DispatchForAPIKeyWithConcurrency(t.Context(), "gpt", stickyHeaders, "client-key", "", nil, "", DispatchConcurrencyContext{})
						if errSticky != nil || res == nil || res.AuthID != bID {
							t.Fatalf("pinned session binding was not preserved: result=%v err=%v", res, errSticky)
						}
					}
				})
			}
		}
	}
}
