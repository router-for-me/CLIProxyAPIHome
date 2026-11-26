package home

import (
	"errors"
	"net/http"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
)

func TestSessionAffinityProviderAvailabilityChanges(t *testing.T) {
	for _, strategy := range []string{"round-robin", "fill-first", "weighted-round-robin"} {
		for _, mode := range []string{"any", "quota", "never"} {
			for _, change := range []string{"disable-bound-provider", "enable-other-provider"} {
				t.Run(strategy+"/"+mode+"/"+change, func(t *testing.T) {
					cfg := &config.Config{Routing: config.RoutingConfig{
						Strategy: strategy, SessionAffinity: true, SessionAffinityRebindOn: mode,
					}}
					store := coreauth.NewSessionCache(time.Hour)
					t.Cleanup(store.Stop)
					manager := coreauth.NewManager(nil, selectorFromConfig(cfg, store), nil)
					manager.SetConfig(cfg)
					bound := &coreauth.Auth{ID: "provider-binding-a", Provider: "codex", Status: coreauth.StatusActive, Attributes: map[string]string{"priority": "100"}}
					other := &coreauth.Auth{ID: "provider-binding-b", Provider: "claude", Status: coreauth.StatusActive}
					if change == "enable-other-provider" {
						other.Disabled = true
						other.Attributes = map[string]string{"priority": "200"}
					}
					const model = "provider-binding-model"
					for _, candidate := range []*coreauth.Auth{bound, other} {
						if _, errRegister := manager.Register(t.Context(), candidate); errRegister != nil {
							t.Fatal(errRegister)
						}
						registry.GetGlobalRegistry().RegisterClient(candidate.ID, candidate.Provider, []*registry.ModelInfo{{ID: model}})
						manager.RefreshSchedulerEntry(candidate.ID)
						t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(candidate.ID) })
					}
					runtime := &Runtime{cfg: cfg, coreManager: manager, sessionAffinityStore: store}
					headers := http.Header{"X-Session-Id": {"provider-binding-session"}}
					initial, errInitial := runtime.DispatchForAPIKey(t.Context(), model, headers, "")
					if errInitial != nil || initial == nil || initial.AuthID != bound.ID {
						t.Fatalf("initial dispatch = %+v, error = %v", initial, errInitial)
					}

					updated := bound.Clone()
					updated.Disabled = true
					wantID := other.ID
					wantProviders := 1
					if change == "enable-other-provider" {
						updated = other.Clone()
						updated.Disabled = false
						wantID = bound.ID
						wantProviders = 2
					}
					if _, errUpdate := manager.Update(t.Context(), updated); errUpdate != nil {
						t.Fatal(errUpdate)
					}
					if providers := runtime.availableProviderKeys(); len(providers) != wantProviders {
						t.Fatalf("available providers = %v, want %d", providers, wantProviders)
					}
					for range 2 {
						selected, errDispatch := runtime.DispatchForAPIKey(t.Context(), model, headers, "")
						if change == "disable-bound-provider" && mode == "never" {
							var authErr *coreauth.Error
							if selected != nil || !errors.As(errDispatch, &authErr) || authErr.Code != "session_auth_unavailable" {
								t.Fatalf("never dispatch = %+v, error = %v", selected, errDispatch)
							}
						} else if errDispatch != nil || selected == nil || selected.AuthID != wantID {
							t.Fatalf("dispatch = %+v, want %s, error = %v", selected, wantID, errDispatch)
						}
					}
					// Provider recovery must not resurrect an old binding after a
					// permitted rebind, or replace a never binding after rejection.
					if change == "disable-bound-provider" {
						if _, errRestore := manager.Update(t.Context(), bound); errRestore != nil {
							t.Fatal(errRestore)
						}
						if mode == "never" {
							wantID = bound.ID
						}
						recovered, errRecovered := runtime.DispatchForAPIKey(t.Context(), model, headers, "")
						if errRecovered != nil || recovered == nil || recovered.AuthID != wantID {
							t.Fatalf("recovered dispatch = %+v, want %s, error = %v", recovered, wantID, errRecovered)
						}
					}
				})
			}
		}
	}
}
