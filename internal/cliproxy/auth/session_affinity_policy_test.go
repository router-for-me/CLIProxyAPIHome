package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
)

func TestSessionAffinityRebindPolicies(t *testing.T) {
	for _, strategy := range []string{"fill-first", "weighted-round-robin"} {
		for _, mode := range []string{"any", "quota", "never"} {
			for _, status := range []int{500, 429, 401, 403, 404, 408} {
				t.Run(strategy+"/"+mode+"/"+http.StatusText(status), func(t *testing.T) {
					store := NewSessionCache(time.Hour)
					t.Cleanup(store.Stop)
					var fallback Selector = &FillFirstSelector{}
					if strategy == "weighted-round-robin" {
						fallback = &WeightedRoundRobinSelector{}
					}
					selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Store: store, Fallback: fallback, RebindOn: mode})
					opts := Options{Headers: http.Header{"X-Session-Id": {"policy-session"}}}
					primary, _ := extractSessionIDs(opts.Headers, nil, nil)
					key := sessionBindingKey("codex", primary, "model")
					store.Set(key, "a")
					original, errLoad := store.LoadSessionBinding(t.Context(), key)
					if errLoad != nil {
						t.Fatal(errLoad)
					}
					blocked := &Auth{ID: "a", Provider: "codex", Status: StatusActive, ModelStates: map[string]*ModelState{"model": {Unavailable: true, NextRetryAfter: time.Now().Add(time.Hour), LastError: &Error{HTTPStatus: status}}}}
					other := &Auth{ID: "b", Provider: "codex", Status: StatusActive}
					picked, errPick := selector.Pick(t.Context(), "codex", "model", opts, []*Auth{blocked, other})
					if mode == "never" {
						if errPick == nil || picked != nil {
							t.Fatalf("never allowed failover: %v %v", picked, errPick)
						}
					} else if errPick != nil || picked == nil || picked.ID != "b" {
						t.Fatalf("failover = %v %v", picked, errPick)
					}
					binding, errRead := store.LoadSessionBinding(t.Context(), key)
					if errRead != nil {
						t.Fatal(errRead)
					}
					want := "b"
					if mode == "never" || (mode == "quota" && (status == 500 || status == 408)) {
						want = "a"
						if binding.Revision != original.Revision {
							t.Fatal("temporary failover changed the binding revision")
						}
					}
					if binding.AuthID != want {
						t.Fatalf("binding = %s, want %s", binding.AuthID, want)
					}
				})
			}
		}
	}
}

func TestManagerSessionAffinityTemporaryFailoverRecovery(t *testing.T) {
	for _, strategy := range []string{"fill-first", "weighted-round-robin"} {
		for _, failure := range []string{"refresh", "retry-round"} {
			t.Run(strategy+"/"+failure, func(t *testing.T) {
				ctx := t.Context()
				store := NewSessionCache(time.Hour)
				t.Cleanup(store.Stop)
				var fallback Selector = &FillFirstSelector{}
				if strategy == "weighted-round-robin" {
					fallback = &WeightedRoundRobinSelector{}
				}
				selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Store: store, Fallback: fallback, RebindOn: "quota"})
				manager := NewManager(nil, selector, nil)
				manager.SetConfig(&config.Config{})
				a := &Auth{ID: "recovery-a", Provider: "codex", Status: StatusActive, Metadata: map[string]any{
					"request_retry": 0, "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
				}}
				b := &Auth{ID: "recovery-b", Provider: "codex", Status: StatusActive, Metadata: map[string]any{"request_retry": 1}}
				for _, candidate := range []*Auth{a, b} {
					if _, errRegister := manager.Register(ctx, candidate); errRegister != nil {
						t.Fatal(errRegister)
					}
					registry.GetGlobalRegistry().RegisterClient(candidate.ID, "codex", []*registry.ModelInfo{{ID: "recovery-model"}})
					t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(candidate.ID) })
				}
				opts := Options{Headers: http.Header{"X-Session-Id": {"recovery-session"}}, SessionAffinity: &SessionAffinitySelection{}}
				primary, _ := extractSessionIDs(opts.Headers, nil, nil)
				key := sessionBindingKey("mixed", primary, "recovery-model")
				store.Set(key, a.ID)
				initial, errInitial := manager.Dispatch(ctx, []string{"codex"}, "recovery-model", opts)
				if errInitial != nil || initial == nil || initial.Auth.ID != a.ID {
					t.Fatalf("initial dispatch = %+v, error = %v", initial, errInitial)
				}
				opts.SessionAffinity.Finish(true)
				binding, errBinding := store.LoadSessionBinding(ctx, key)
				if errBinding != nil {
					t.Fatal(errBinding)
				}
				if failure == "refresh" {
					blocked := initial.Auth.Clone()
					ApplyRefreshFailureState(blocked, NewTransientRefreshError(), time.Now())
					if !RefreshBlocksDispatch(blocked) {
						t.Fatal("refresh failure did not block dispatch")
					}
					if _, errUpdate := manager.Update(ctx, blocked); errUpdate != nil {
						t.Fatal(errUpdate)
					}
				} else {
					if errResult := manager.MarkResult(ctx, Result{AuthID: a.ID, Provider: "codex", Model: "recovery-model", Error: &Error{HTTPStatus: http.StatusInternalServerError}}); errResult != nil {
						t.Fatal(errResult)
					}
					opts.Metadata = map[string]any{RequestRetryRoundMetadataKey: 1}
				}
				opts.SessionAffinity = &SessionAffinitySelection{}
				failover, errFailover := manager.Dispatch(ctx, []string{"codex"}, "recovery-model", opts)
				if errFailover != nil || failover == nil || failover.Auth.ID != b.ID {
					t.Fatalf("failover dispatch = %+v, error = %v", failover, errFailover)
				}
				if opts.SessionAffinity.Commit != nil {
					t.Fatal("temporary failover staged a replacement binding")
				}
				opts.SessionAffinity.Finish(true)
				after, errAfter := store.LoadSessionBinding(ctx, key)
				if errAfter != nil || after.AuthID != a.ID || after.Revision != binding.Revision {
					t.Fatalf("temporary failover changed binding: before=%+v after=%+v error=%v", binding, after, errAfter)
				}
				if _, errUpdate := manager.Update(ctx, initial.Auth); errUpdate != nil {
					t.Fatal(errUpdate)
				}
				opts.Metadata = nil
				opts.SessionAffinity = &SessionAffinitySelection{}
				recovered, errRecovered := manager.Dispatch(ctx, []string{"codex"}, "recovery-model", opts)
				if errRecovered != nil || recovered == nil || recovered.Auth.ID != a.ID {
					t.Fatalf("recovered dispatch = %+v, error = %v", recovered, errRecovered)
				}
			})
		}
	}
}

func TestSessionAffinityQuotaRefreshClassification(t *testing.T) {
	now := time.Unix(1700000000, 0)
	for _, mode := range []string{"temporary", "expired", "unauthorized"} {
		t.Run(mode, func(t *testing.T) {
			candidate := &Auth{Status: StatusActive, Metadata: map[string]any{"expires_at": now.Add(time.Hour).Format(time.RFC3339)}}
			ApplyRefreshFailureState(candidate, NewTransientRefreshError(), now)
			switch mode {
			case "expired":
				candidate.Metadata["expires_at"] = now.Add(-time.Second).Format(time.RFC3339)
			case "unauthorized":
				candidate.LastError = newUnauthorizedRefreshError()
			}
			if got, want := sessionAffinityQuotaBlocked(candidate, "model", now), mode != "temporary"; got != want {
				t.Fatalf("quota blocked = %v, want %v", got, want)
			}
		})
	}
}

func TestManagerSessionAffinityPreservesTransientBinding(t *testing.T) {
	ctx := context.Background()
	store := NewSessionCache(time.Hour)
	t.Cleanup(store.Stop)
	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Store: store, Fallback: &FillFirstSelector{}, RebindOn: "quota"})
	manager := NewManager(nil, selector, nil)
	manager.SetConfig(&config.Config{})
	for _, id := range []string{"policy-a", "policy-b"} {
		auth := &Auth{ID: id, Provider: "codex", Status: StatusActive}
		if id == "policy-a" {
			auth.ModelStates = map[string]*ModelState{"policy-model": {Unavailable: true, NextRetryAfter: time.Now().Add(time.Hour), LastError: &Error{HTTPStatus: 500}}}
		}
		if _, errRegister := manager.Register(ctx, auth); errRegister != nil {
			t.Fatal(errRegister)
		}
		registry.GetGlobalRegistry().RegisterClient(id, "codex", []*registry.ModelInfo{{ID: "policy-model"}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	}
	opts := Options{Headers: http.Header{"X-Session-Id": {"manager-policy"}}, SessionAffinity: &SessionAffinitySelection{}}
	primary, _ := extractSessionIDs(opts.Headers, nil, nil)
	key := sessionBindingKey("mixed", primary, "policy-model")
	store.Set(key, "policy-a")
	decision, errDispatch := manager.Dispatch(ctx, []string{"codex"}, "policy-model", opts)
	if errDispatch != nil || decision == nil || decision.Auth.ID != "policy-b" {
		t.Fatalf("dispatch = %+v %v", decision, errDispatch)
	}
	if opts.SessionAffinity.Commit != nil {
		t.Fatal("temporary failover staged a replacement binding")
	}
	opts.SessionAffinity.Finish(true)
	binding, errLoad := store.LoadSessionBinding(ctx, key)
	if errLoad != nil || binding.AuthID != "policy-a" {
		t.Fatalf("binding = %+v %v", binding, errLoad)
	}
}
