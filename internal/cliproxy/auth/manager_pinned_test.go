package auth

import (
	"errors"
	"fmt"
	"maps"
	"net/http"
	"testing"
	"time"
)

func TestManagerNonWeightedPinnedSelection(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		for _, affinity := range []bool{false, true} {
			for _, strategy := range []string{"round-robin", "fill-first"} {
				t.Run(fmt.Sprintf("mixed=%t/affinity=%t/%s", mixed, affinity, strategy), func(t *testing.T) {
					var selector Selector = &RoundRobinSelector{}
					if strategy == "fill-first" {
						selector = &FillFirstSelector{}
					}
					if affinity {
						sticky := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: selector})
						t.Cleanup(sticky.Stop)
						selector = sticky
					}
					manager := NewManager(nil, selector, nil)
					const provider, model = "pinned-non-weighted", "pinned-model"
					pinnedID := t.Name() + "-pinned"
					for i := range 32 {
						auth := &Auth{
							ID: fmt.Sprintf("%s-other-%02d", t.Name(), i), Provider: provider, Status: StatusActive,
							Attributes: map[string]string{"priority": "10"},
						}
						if i == 0 {
							auth.ID = pinnedID
							auth.Attributes["priority"] = "0"
						}
						registerDispatchTestAuth(t, manager, auth, model)
					}
					otherModelID := t.Name() + "-other-model"
					registerDispatchTestAuth(t, manager, &Auth{
						ID: otherModelID, Provider: provider, Status: StatusActive,
						Attributes: map[string]string{"priority": "10"},
					}, "other-model")
					providers := []string{provider}
					if mixed {
						providers = []string{provider, "other-provider"}
					}
					pick := func(id string, excluded []string) (*DispatchDecision, error) {
						meta := map[string]any{PinnedAuthIDMetadataKey: id}
						if len(excluded) > 0 {
							meta[ExcludedAuthIDsMetadataKey] = excluded
						}
						return manager.Dispatch(t.Context(), providers, model, Options{
							Headers:  http.Header{"Session_id": {"non-weighted-pinned"}},
							Metadata: meta,
						})
					}
					for range 3 {
						if decision, errPick := pick(pinnedID, nil); errPick != nil || decision == nil || decision.Auth.ID != pinnedID {
							t.Fatalf("lower-priority pin not enforced: decision=%v err=%v", decision, errPick)
						}
					}
					for _, tc := range []struct {
						name     string
						id       string
						excluded []string
						wantCode string
					}{
						{name: "missing pin", id: "missing", wantCode: "auth_not_found"},
						{name: "already tried", id: pinnedID, excluded: []string{pinnedID}, wantCode: "auth_unavailable"},
						{name: "ineligible model", id: otherModelID, wantCode: "auth_not_found"},
					} {
						decision, errPick := pick(tc.id, tc.excluded)
						var authErr *Error
						if decision != nil || !errors.As(errPick, &authErr) || authErr.Code != tc.wantCode {
							t.Fatalf("%s: decision=%v err=%v, want %s", tc.name, decision, errPick, tc.wantCode)
						}
					}
					if sticky, ok := selector.(*SessionAffinitySelector); ok {
						namespace := provider
						if mixed {
							namespace = "mixed"
						}
						id, _ := extractSessionIDs(http.Header{"Session_id": {"non-weighted-pinned"}}, nil, nil)
						if binding, found := sticky.cache.Get(sessionBindingKey(namespace, id, model)); !found || binding != pinnedID {
							t.Fatalf("pinning checks changed the sticky binding: %q, found=%t", binding, found)
						}
					}
				})
			}
		}
	}
}

func TestManagerWeightedPinnedSessionCompensation(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		for _, mode := range []string{"sticky", "cold", "no-session"} {
			t.Run(fmt.Sprintf("mixed=%t/%s", mixed, mode), func(t *testing.T) {
				weighted := &WeightedRoundRobinSelector{}
				selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: weighted})
				t.Cleanup(selector.Stop)
				manager := NewManager(nil, selector, nil)
				const provider, model = "weighted-pinned", "weighted-pinned-model"
				aID, bID := t.Name()+"-a", t.Name()+"-b"
				triedID := t.Name() + "-tried"
				disallowedID := t.Name() + "-disallowed"
				excluded := []*Auth{
					{ID: t.Name() + "-disabled", Disabled: true},
					{ID: t.Name() + "-zero", Attributes: map[string]string{"weight": "0"}},
					{ID: t.Name() + "-cooldown", ModelStates: map[string]*ModelState{
						model: {Unavailable: true, NextRetryAfter: time.Now().Add(time.Hour)},
					}},
					{ID: t.Name() + "-wrong-model"},
					{ID: t.Name() + "-wrong-provider", Provider: "other-provider"},
					{ID: t.Name() + "-lower-priority", Attributes: map[string]string{"priority": "0"}},
					{ID: triedID},
					{ID: disallowedID},
				}
				auths := append([]*Auth{
					{ID: aID, Attributes: map[string]string{"weight": "3", "priority": "1"}},
					{ID: bID, Attributes: map[string]string{"weight": "1", "priority": "1"}},
				}, excluded...)
				for _, auth := range auths {
					if auth.Provider == "" {
						auth.Provider = provider
					}
					auth.Status = StatusActive
					if auth.Attributes == nil {
						auth.Attributes = make(map[string]string)
					}
					if _, ok := auth.Attributes["weight"]; !ok {
						auth.Attributes["weight"] = "100"
					}
					if _, ok := auth.Attributes["priority"]; !ok {
						auth.Attributes["priority"] = "2"
					}
					registeredModel := model
					if auth.ID == t.Name()+"-wrong-model" {
						registeredModel = "other-model"
					}
					registerDispatchTestAuth(t, manager, auth, registeredModel)
				}
				namespace := provider
				providers := []string{provider}
				if mixed {
					namespace = "mixed"
					providers = []string{provider, "no-credentials"}
				}
				stickyHeaders := http.Header{"Session_id": {"pinned-session"}}
				id, _ := extractSessionIDs(stickyHeaders, nil, nil)
				bindingKey := sessionBindingKey(namespace, id, model)
				if mode == "sticky" {
					selector.cache.Set(bindingKey, bID)
				}
				lowerID := t.Name() + "-lower-priority"
				allowedIDs := []string{
					aID, bID, lowerID,
					excluded[0].ID, excluded[1].ID, excluded[2].ID, excluded[3].ID, excluded[4].ID, triedID,
				}
				pick := func(opts Options) (*Auth, error) {
					meta := make(map[string]any, len(opts.Metadata)+2)
					for k, v := range opts.Metadata {
						meta[k] = v
					}
					meta[ExcludedAuthIDsMetadataKey] = []string{triedID}
					meta[AllowedAuthIDsMetadataKey] = allowedIDs
					opts.Metadata = meta
					decision, errDispatch := manager.Dispatch(t.Context(), providers, model, opts)
					if errDispatch != nil || decision == nil {
						return nil, errDispatch
					}
					return decision.Auth, nil
				}
				for i := range 4 {
					opts := Options{Metadata: map[string]any{PinnedAuthIDMetadataKey: bID}}
					if mode != "no-session" {
						opts.Headers = stickyHeaders
					}
					if auth, errPick := pick(opts); errPick != nil || auth == nil || auth.ID != bID {
						t.Fatalf("pinned request %d: auth=%v err=%v, want %q", i, auth, errPick, bID)
					}
				}
				state := weightedStateForTest(t, weighted, namespace+":"+model, aID, bID)
				if state == nil || !maps.Equal(state.currentForTest(), map[string]int64{aID: 12, bID: -12}) || len(state.pendingForTest()) != 0 {
					t.Fatalf("pinned traffic did not consume the eligible pool's credit: %#v", state)
				}
				counts := map[string]int{}
				for i := range 12 {
					auth, errPick := pick(Options{Headers: http.Header{"Session_id": {fmt.Sprintf("fresh-%d", i)}}})
					if errPick != nil || auth == nil {
						t.Fatalf("fresh request %d: auth=%v err=%v", i, auth, errPick)
					}
					counts[auth.ID]++
				}
				if counts[aID] != 12 || counts[bID] != 0 {
					t.Fatalf("fresh distribution = %v, want a=12 b=0", counts)
				}
				if !maps.Equal(state.currentForTest(), map[string]int64{aID: 0, bID: 0}) || len(state.pendingForTest()) != 0 {
					t.Fatalf("compensation did not balance: current=%v pending=%v", state.currentForTest(), state.pendingForTest())
				}
				if mode != "no-session" {
					if binding, ok := selector.cache.Get(bindingKey); !ok || binding != bID {
						t.Fatalf("compensation changed pinned binding: %q, found=%t", binding, ok)
					}
				}
				// A broader accounting pool must never make an invalid pin selectable.
				for _, pinnedID := range []string{excluded[0].ID, excluded[1].ID, excluded[2].ID, excluded[3].ID, excluded[4].ID, triedID, disallowedID, "missing"} {
					opts := Options{
						Headers:  stickyHeaders,
						Metadata: map[string]any{PinnedAuthIDMetadataKey: pinnedID},
					}
					if auth, errPick := pick(opts); errPick == nil || auth != nil {
						t.Fatalf("invalid pin %q escaped pinning checks: auth=%v err=%v", pinnedID, auth, errPick)
					}
					if !maps.Equal(state.currentForTest(), map[string]int64{aID: 0, bID: 0}) || len(state.pendingForTest()) != 0 {
						t.Fatalf("invalid pin %q changed accounting: current=%v pending=%v", pinnedID, state.currentForTest(), state.pendingForTest())
					}
				}
				// A valid lower-priority pin still wins without charging another tier.
				lowerOpts := Options{
					Headers:  http.Header{"Session_id": {"lower-pinned-session"}},
					Metadata: map[string]any{PinnedAuthIDMetadataKey: lowerID},
				}
				if auth, errPick := pick(lowerOpts); errPick != nil || auth == nil || auth.ID != lowerID {
					t.Fatalf("lower-priority pin was not enforced: auth=%v err=%v", auth, errPick)
				}
				if !maps.Equal(state.currentForTest(), map[string]int64{aID: 0, bID: 0}) || len(state.pendingForTest()) != 0 {
					t.Fatalf("lower-priority pin changed higher-tier credits: current=%v pending=%v", state.currentForTest(), state.pendingForTest())
				}
				lowerState := weightedStateForTest(t, weighted, namespace+":"+model, lowerID)
				if !maps.Equal(lowerState.currentForTest(), map[string]int64{lowerID: 0}) || len(lowerState.pendingForTest()) != 0 {
					t.Fatalf("lower-priority pin accounted the wrong pool: %#v", lowerState)
				}
			})
		}
	}
}
