package auth

import (
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
)

func TestManagerSessionAffinityQuotaAliasState(t *testing.T) {
	for _, strategy := range []string{"fill-first", "weighted-round-robin"} {
		for _, scenario := range []string{"upstream-quota", "alias-quota", "alias-transient", "expired-alias-quota", "other-model-quota", "upstream-transient-alias-quota"} {
			t.Run(strategy+"/"+scenario, func(t *testing.T) {
				ctx := t.Context()
				store := NewSessionCache(time.Hour)
				t.Cleanup(store.Stop)
				var fallback Selector = &FillFirstSelector{}
				if strategy == "weighted-round-robin" {
					fallback = &WeightedRoundRobinSelector{}
				}
				selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Store: store, Fallback: fallback, RebindOn: "quota"})
				manager := NewManager(nil, selector, nil)
				cfg := &config.Config{OAuthModelAlias: map[string][]config.OAuthModelAlias{"codex": {{Name: "upstream", Alias: "public"}}}}
				manager.SetConfig(cfg)
				manager.SetOAuthModelAlias(cfg.OAuthModelAlias)
				a := &Auth{ID: "alias-state-a", Provider: "codex", Status: StatusActive}
				b := &Auth{ID: "alias-state-b", Provider: "codex", Status: StatusActive}
				clean := a.Clone()
				deadline := time.Now().Add(time.Hour)
				stateModel := "public"
				status := http.StatusTooManyRequests
				switch scenario {
				case "upstream-quota":
					stateModel = "upstream"
				case "alias-transient":
					status = http.StatusServiceUnavailable
				case "expired-alias-quota":
					deadline = time.Now().Add(-time.Hour)
				case "other-model-quota":
					stateModel = "other-model"
				}
				a.ModelStates = map[string]*ModelState{stateModel: {
					Unavailable: true, Status: StatusError, NextRetryAfter: deadline, LastError: &Error{HTTPStatus: status},
					Quota: QuotaState{Exceeded: status == http.StatusTooManyRequests, Reason: "quota", NextRecoverAt: deadline},
				}}
				if scenario == "upstream-transient-alias-quota" {
					a.ModelStates["upstream"] = &ModelState{Unavailable: true, Status: StatusError, NextRetryAfter: deadline, LastError: &Error{HTTPStatus: http.StatusServiceUnavailable}}
				}
				for _, candidate := range []*Auth{a, b} {
					if _, errRegister := manager.Register(ctx, candidate); errRegister != nil {
						t.Fatal(errRegister)
					}
					registry.GetGlobalRegistry().RegisterClient(candidate.ID, "codex", []*registry.ModelInfo{{ID: "public"}})
					t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(candidate.ID) })
				}
				opts := Options{Headers: http.Header{"X-Session-Id": {"alias-state-session"}}, SessionAffinity: &SessionAffinitySelection{}}
				if scenario == "expired-alias-quota" || scenario == "other-model-quota" {
					// A retry/admission exclusion must not revive expired or unrelated history.
					opts.Metadata = map[string]any{ExcludedAuthIDsMetadataKey: []string{a.ID}}
				}
				primary, _ := extractSessionIDs(opts.Headers, nil, nil)
				key := sessionBindingKey("mixed", primary, "public")
				store.Set(key, a.ID)
				before, errBefore := store.LoadSessionBinding(ctx, key)
				if errBefore != nil {
					t.Fatal(errBefore)
				}
				decision, errDispatch := manager.Dispatch(ctx, []string{"codex"}, "public", opts)
				if errDispatch != nil || decision == nil || decision.Auth.ID != b.ID || decision.UpstreamModel != "upstream" {
					t.Fatalf("failover = %+v, error = %v", decision, errDispatch)
				}
				rebind := scenario == "upstream-quota" || scenario == "alias-quota"
				if rebind {
					commit := opts.SessionAffinity.Commit
					if commit == nil || commit.AuthID != b.ID {
						t.Fatalf("active quota failed to stage rebinding: %+v", commit)
					}
					if swapped, errSwap := store.CompareAndSwapSessionBinding(ctx, *commit); errSwap != nil || !swapped {
						t.Fatalf("commit = %t, error = %v", swapped, errSwap)
					}
				} else if opts.SessionAffinity.Commit != nil {
					t.Fatal("transient, expired or unrelated failure staged rebinding")
				}
				opts.SessionAffinity.Finish(true)
				wantID := a.ID
				if rebind {
					wantID = b.ID
				}
				after, errAfter := store.LoadSessionBinding(ctx, key)
				if errAfter != nil || after.AuthID != wantID || (!rebind && after.Revision != before.Revision) {
					t.Fatalf("binding = %+v, want %s, error = %v", after, wantID, errAfter)
				}
				if _, errUpdate := manager.Update(ctx, clean); errUpdate != nil {
					t.Fatal(errUpdate)
				}
				opts.Metadata = nil
				opts.SessionAffinity = &SessionAffinitySelection{}
				recovered, errRecovered := manager.Dispatch(ctx, []string{"codex"}, "public", opts)
				if errRecovered != nil || recovered == nil || recovered.Auth.ID != wantID {
					t.Fatalf("recovery = %+v, want %s, error = %v", recovered, wantID, errRecovered)
				}
				opts.SessionAffinity.Finish(false)
			})
		}
	}
}
