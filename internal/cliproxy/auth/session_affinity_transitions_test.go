package auth

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
)

func TestSessionAffinityQuotaBlockingUsesCurrentState(t *testing.T) {
	now := time.Unix(1700000000, 0)
	for _, status := range []int{401, 402, 403, 404, 429, 408, 500, 503} {
		for _, scope := range []string{"model", "credential", "other-model"} {
			for _, active := range []bool{false, true} {
				t.Run(http.StatusText(status)+"/"+scope+"/"+map[bool]string{true: "active", false: "expired"}[active], func(t *testing.T) {
					deadline := now.Add(-time.Hour)
					if active {
						deadline = now.Add(time.Hour)
					}
					lastError := &Error{HTTPStatus: status}
					quota := QuotaState{}
					if status == 429 {
						quota = QuotaState{Exceeded: true, NextRecoverAt: deadline, Reason: "quota"}
					}
					auth := &Auth{Status: StatusError, Unavailable: true, LastError: lastError, NextRetryAfter: deadline, Quota: quota}
					if scope != "credential" {
						model := "model"
						if scope == "other-model" {
							model = "unrelated"
						}
						auth.ModelStates = map[string]*ModelState{model: {Status: StatusError, Unavailable: true, LastError: lastError, NextRetryAfter: deadline, Quota: quota}}
					}
					want := active && scope != "other-model" && status != 408 && status < 500
					if got := sessionAffinityQuotaBlocked(auth, "model", now); got != want {
						t.Fatalf("quota blocked=%t, want %t", got, want)
					}
				})
			}
		}
	}
	// Credential-wide quota still fences every model, even if this model is healthy.
	auth := &Auth{Status: StatusActive, Quota: QuotaState{Exceeded: true, Reason: "credential_quota", NextRecoverAt: now.Add(time.Hour)}, ModelStates: map[string]*ModelState{"model": {Status: StatusActive}}}
	if !sessionAffinityQuotaBlocked(auth, "model", now) {
		t.Fatal("credential-wide quota was ignored")
	}
	auth.Quota.NextRecoverAt = now.Add(-time.Hour)
	if sessionAffinityQuotaBlocked(auth, "model", now) {
		t.Fatal("expired credential-wide quota was retained")
	}
}

func TestManagerSessionAffinityExcludedProviderProtection(t *testing.T) {
	for _, strategy := range []string{"fill-first", "weighted-round-robin"} {
		for _, state := range []string{"healthy", "quota", "authentication", "disabled", "expired", "removed", "out-of-scope"} {
			t.Run(strategy+"/"+state, func(t *testing.T) {
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
				const model = "excluded-pool-model"
				a := &Auth{ID: "excluded-pool-a", Provider: "antigravity", Status: StatusActive}
				b := &Auth{ID: "excluded-pool-b", Provider: "codex", Status: StatusActive}
				switch state {
				case "quota", "authentication":
					status := http.StatusTooManyRequests
					if state == "authentication" {
						status = http.StatusUnauthorized
					}
					a.ModelStates = map[string]*ModelState{model: {Unavailable: true, Status: StatusError, NextRetryAfter: time.Now().Add(time.Hour), LastError: &Error{HTTPStatus: status}}}
				case "disabled":
					a.Disabled = true
				case "expired":
					a.Metadata = map[string]any{"expires_at": time.Now().Add(-time.Hour).Format(time.RFC3339)}
				}
				for _, candidate := range []*Auth{a, b} {
					if state == "removed" && candidate == a {
						continue
					}
					if _, errRegister := manager.Register(ctx, candidate); errRegister != nil {
						t.Fatal(errRegister)
					}
					registry.GetGlobalRegistry().RegisterClient(candidate.ID, candidate.Provider, []*registry.ModelInfo{{ID: model}})
					t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(candidate.ID) })
				}
				opts := Options{Headers: http.Header{"X-Session-Id": {"excluded-pool-session"}}, ExcludedProviders: []string{"antigravity"}, SessionAffinity: &SessionAffinitySelection{}}
				primary, _ := extractSessionIDs(opts.Headers, nil, nil)
				key := sessionBindingKey("mixed", primary, model)
				store.Set(key, a.ID)
				before, errBefore := store.LoadSessionBinding(ctx, key)
				if errBefore != nil {
					t.Fatal(errBefore)
				}
				providers := []string{"antigravity", "codex"}
				if state == "out-of-scope" {
					providers = []string{"codex", "claude"}
				}
				decision, errDispatch := manager.Dispatch(ctx, providers, model, opts)
				if errDispatch != nil || decision == nil || decision.Auth.ID != b.ID {
					t.Fatalf("dispatch = %+v, error = %v", decision, errDispatch)
				}
				if state == "healthy" {
					if opts.SessionAffinity.Commit != nil {
						t.Fatal("temporary provider exclusion staged a replacement binding")
					}
				} else {
					commit := opts.SessionAffinity.Commit
					if commit == nil || commit.AuthID != b.ID {
						t.Fatalf("ineligible original credential prevented rebinding: %+v", commit)
					}
					if swapped, errSwap := store.CompareAndSwapSessionBinding(ctx, *commit); errSwap != nil || !swapped {
						t.Fatalf("commit = %t, error = %v", swapped, errSwap)
					}
				}
				opts.SessionAffinity.Finish(true)
				after, errAfter := store.LoadSessionBinding(ctx, key)
				wantID := b.ID
				if state == "healthy" {
					wantID = a.ID
				}
				if errAfter != nil || after.AuthID != wantID || (state == "healthy" && after.Revision != before.Revision) {
					t.Fatalf("binding = %+v, want %s, error = %v", after, wantID, errAfter)
				}
			})
		}
	}
}

func TestManagerSessionAffinityStateTransitions(t *testing.T) {
	for _, strategy := range []string{"fill-first", "weighted-round-robin"} {
		for _, mode := range []string{"any", "quota", "never"} {
			for _, scenario := range []string{"transient", "quota", "expired-quota-admission", "expired-forbidden-retry", "other-model-quota-admission", "refresh", "disabled", "expired-binding"} {
				t.Run(strategy+"/"+mode+"/"+scenario, func(t *testing.T) {
					ctx := t.Context()
					store := NewSessionCache(time.Hour)
					defer store.Stop()
					var fallback Selector = &FillFirstSelector{}
					if strategy == "weighted-round-robin" {
						fallback = &WeightedRoundRobinSelector{}
					}
					selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Store: store, Fallback: fallback, RebindOn: mode})
					manager := NewManager(nil, selector, nil)
					manager.SetConfig(&config.Config{})
					a := &Auth{ID: "transition-a", Provider: "codex", Status: StatusActive, Metadata: map[string]any{"expires_at": time.Now().Add(time.Hour).Format(time.RFC3339)}}
					b := &Auth{ID: "transition-b", Provider: "codex", Status: StatusActive}
					for _, candidate := range []*Auth{a, b} {
						if _, errRegister := manager.Register(ctx, candidate); errRegister != nil {
							t.Fatal(errRegister)
						}
						registry.GetGlobalRegistry().RegisterClient(candidate.ID, "codex", []*registry.ModelInfo{{ID: "transition-model"}})
						t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(candidate.ID) })
					}
					opts := Options{Headers: http.Header{"X-Session-Id": {"transition-session"}}, SessionAffinity: &SessionAffinitySelection{}}
					primary, _ := extractSessionIDs(opts.Headers, nil, nil)
					key := sessionBindingKey("mixed", primary, "transition-model")
					store.Set(key, a.ID)
					before, errBefore := store.LoadSessionBinding(ctx, key)
					if errBefore != nil {
						t.Fatal(errBefore)
					}
					blocked := a.Clone()
					now := time.Now()
					state := &ModelState{Unavailable: true, Status: StatusError, NextRetryAfter: now.Add(time.Hour), LastError: &Error{HTTPStatus: 503}}
					blocked.ModelStates = map[string]*ModelState{"transition-model": state}
					permanent := false
					switch scenario {
					case "quota":
						permanent = true
						state.LastError.HTTPStatus = 429
						state.Quota = QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: state.NextRetryAfter}
					case "expired-quota-admission", "other-model-quota-admission":
						state.LastError.HTTPStatus = 429
						if scenario == "expired-quota-admission" {
							state.NextRetryAfter = now.Add(-time.Hour)
						} else {
							blocked.ModelStates = map[string]*ModelState{"other-model": state}
						}
						state.Quota = QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: state.NextRetryAfter}
						blocked.LastError, blocked.Quota = state.LastError, state.Quota
						opts.Metadata = map[string]any{ExcludedConcurrencyCandidatesMetadataKey: []ExcludedConcurrencyCandidate{{CredentialID: a.ID}}}
					case "expired-forbidden-retry":
						state.LastError.HTTPStatus = 403
						state.NextRetryAfter = now.Add(-time.Hour)
						opts.Metadata = map[string]any{ExcludedAuthIDsMetadataKey: []string{a.ID}}
					case "refresh":
						blocked.ModelStates = nil
						ApplyRefreshFailureState(blocked, NewTransientRefreshError(), now)
					case "disabled":
						blocked.Disabled = true
						permanent = true
					case "expired-binding":
						store.mu.Lock()
						entry := store.entries[key]
						entry.expiresAt = now.Add(-time.Hour)
						store.entries[key] = entry
						store.mu.Unlock()
					}
					if _, errUpdate := manager.Update(ctx, blocked); errUpdate != nil {
						t.Fatal(errUpdate)
					}
					decision, errDispatch := manager.Dispatch(ctx, []string{"codex"}, "transition-model", opts)
					if mode == "never" && scenario != "expired-binding" {
						var authErr *Error
						if !errors.As(errDispatch, &authErr) || authErr.Code != "session_auth_unavailable" || decision != nil {
							t.Fatalf("never dispatch=%+v error=%v", decision, errDispatch)
						}
					} else {
						if errDispatch != nil || decision == nil || decision.Auth.ID != b.ID {
							t.Fatalf("failover=%+v error=%v", decision, errDispatch)
						}
						if commit := opts.SessionAffinity.Commit; commit != nil {
							if swapped, errSwap := store.CompareAndSwapSessionBinding(ctx, *commit); errSwap != nil || !swapped {
								t.Fatalf("commit=%t error=%v", swapped, errSwap)
							}
						}
						opts.SessionAffinity.Finish(true)
					}
					wantBound := a.ID
					if mode == "any" || (mode == "quota" && permanent) || scenario == "expired-binding" {
						wantBound = b.ID
					}
					after, errAfter := store.LoadSessionBinding(ctx, key)
					if errAfter != nil || after.AuthID != wantBound || (wantBound == a.ID && after.Revision != before.Revision) {
						t.Fatalf("binding=%+v want=%s error=%v", after, wantBound, errAfter)
					}
					// Recovery must return to the preserved credential, but never undo
					// a deliberate rebind. Use a new selector to emulate another Home.
					if _, errUpdate := manager.Update(ctx, a); errUpdate != nil {
						t.Fatal(errUpdate)
					}
					manager.SetSelector(NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Store: store, Fallback: fallback, RebindOn: mode}))
					opts.Metadata = nil
					opts.SessionAffinity = &SessionAffinitySelection{}
					recovered, errRecovered := manager.Dispatch(ctx, []string{"codex"}, "transition-model", opts)
					if errRecovered != nil || recovered == nil || recovered.Auth.ID != wantBound {
						t.Fatalf("recovery=%+v want=%s error=%v", recovered, wantBound, errRecovered)
					}
					opts.SessionAffinity.Finish(false)
				})
			}
		}
	}
}
