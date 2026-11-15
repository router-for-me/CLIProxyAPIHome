package home

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
)

type tosPauseTestAdapter struct {
	runtimeKVTestAdapter
	pauseMu sync.Mutex
	value   []byte
}

func (a *tosPauseTestAdapter) KVGet(_ context.Context, key string) ([]byte, bool, error) {
	a.pauseMu.Lock()
	defer a.pauseMu.Unlock()
	return bytes.Clone(a.value), a.value != nil, nil
}

func (a *tosPauseTestAdapter) KVCompareAndSwap(_ context.Context, key string, expected []byte, exists bool, value []byte, ttl time.Duration) (bool, error) {
	a.pauseMu.Lock()
	defer a.pauseMu.Unlock()
	if (a.value != nil) != exists || !bytes.Equal(a.value, expected) {
		return false, nil
	}
	a.value = bytes.Clone(value)
	return true, nil
}

func TestAntigravityTOSPauseSharedDispatchAndReplay(t *testing.T) {
	ctx := t.Context()
	adapter := &tosPauseTestAdapter{runtimeKVTestAdapter: runtimeKVTestAdapter{enabled: true}}
	manager := coreauth.NewManager(nil, &coreauth.FillFirstSelector{}, nil)
	cfg := &config.Config{Antigravity: config.AntigravityConfig{TOSPauseWindow: "6h"}}
	manager.SetConfig(cfg)
	auth := &coreauth.Auth{ID: "tos-test-auth", Index: "tos-test-auth", Provider: "antigravity", Status: coreauth.StatusActive}
	if _, errRegister := manager.Register(ctx, auth); errRegister != nil {
		t.Fatal(errRegister)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "tos-test-model"}})
	manager.RefreshSchedulerEntry(auth.ID)
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	first := &Runtime{cfg: cfg, coreManager: manager, clusterAdapter: adapter}
	second := &Runtime{cfg: cfg, coreManager: manager, clusterAdapter: adapter}
	body := `{"error":{"code":403,"status":"PERMISSION_DENIED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"TOS_VIOLATION"}]}}`
	observed := time.Now().Add(-time.Minute).UTC()
	result := coreauth.NewUsageResult(auth.ID, auth.Provider, "tos-test-model", 403, body)
	result.ObservedAt = observed
	if errRecord := first.recordAntigravityTOSPause(ctx, result, false); errRecord != nil {
		t.Fatal(errRecord)
	}
	until, errDeadline := second.antigravityTOSPauseDeadline(ctx)
	if errDeadline != nil || !until.Equal(observed.Add(6*time.Hour)) {
		t.Fatalf("shared deadline = %v %v", until, errDeadline)
	}
	result.ObservedAt = time.Now()
	if errReplay := second.recordAntigravityTOSPause(ctx, result, true); errReplay != nil {
		t.Fatal(errReplay)
	}
	unchanged, errRead := first.antigravityTOSPauseDeadline(ctx)
	if errRead != nil || !unchanged.Equal(until) {
		t.Fatal("redelivery extended an active pause")
	}
	if decision, errDispatch := second.DispatchForAPIKey(ctx, "tos-test-model", nil, ""); errDispatch == nil || decision != nil {
		t.Fatalf("paused pool dispatched: %+v %v", decision, errDispatch)
	}
	other := &coreauth.Auth{ID: "tos-other-provider", Provider: "codex", Status: coreauth.StatusActive}
	if _, errRegister := manager.Register(ctx, other); errRegister != nil {
		t.Fatal(errRegister)
	}
	registry.GetGlobalRegistry().RegisterClient(other.ID, other.Provider, []*registry.ModelInfo{{ID: "tos-test-model"}})
	manager.RefreshSchedulerEntry(other.ID)
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(other.ID) })
	if decision, errDispatch := second.DispatchForAPIKey(ctx, "tos-test-model", nil, ""); errDispatch != nil || decision == nil || decision.Provider != "codex" {
		t.Fatalf("pause blocked another provider: %+v %v", decision, errDispatch)
	}
	cfg.Antigravity.TOSPauseWindow = "off"
	if deadline, errDisabled := second.antigravityTOSPauseDeadline(ctx); errDisabled != nil || !deadline.IsZero() {
		t.Fatalf("disabled pause = %v %v", deadline, errDisabled)
	}
	cfg.Antigravity.TOSPauseWindow = "6h"
	adapter.value = []byte(strconv.FormatInt(time.Now().Add(-time.Hour).UnixNano(), 10))
	result.ObservedAt = time.Now().Add(-7 * time.Hour)
	if errExpired := first.recordAntigravityTOSPause(ctx, result, true); errExpired != nil {
		t.Fatal(errExpired)
	}
	if deadline, errExpired := second.antigravityTOSPauseDeadline(ctx); errExpired != nil || deadline.After(time.Now()) {
		t.Fatal("old usage replay rearmed an expired pause")
	}
}

func TestAntigravityTOSPausePreservesSessionBindings(t *testing.T) {
	for _, strategy := range []string{"fill-first", "weighted-round-robin"} {
		for _, mode := range []string{"any", "quota", "never"} {
			t.Run(strategy+"/"+mode, func(t *testing.T) {
				cfg := &config.Config{Routing: config.RoutingConfig{Strategy: strategy, SessionAffinity: true, SessionAffinityRebindOn: mode}, Antigravity: config.AntigravityConfig{TOSPauseWindow: "6h"}}
				store := coreauth.NewSessionCache(time.Hour)
				t.Cleanup(store.Stop)
				selector := selectorFromConfig(cfg, store)
				manager := coreauth.NewManager(nil, selector, nil)
				manager.SetConfig(cfg)
				model := "pause-binding-model"
				var bound, antigravity *coreauth.Auth
				for _, entry := range []struct{ id, provider string }{{"pause-z-bound", "codex"}, {"pause-a-other", "codex"}, {"pause-antigravity", "antigravity"}} {
					auth := &coreauth.Auth{ID: entry.id, Index: entry.id, Provider: entry.provider, Status: coreauth.StatusActive}
					if entry.provider == "antigravity" {
						auth.Attributes = map[string]string{"priority": "100"}
					}
					if _, errRegister := manager.Register(t.Context(), auth); errRegister != nil {
						t.Fatal(errRegister)
					}
					registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: model}})
					manager.RefreshSchedulerEntry(auth.ID)
					id := auth.ID
					t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
					if auth.ID == "pause-z-bound" {
						bound = auth
					}
					if auth.Provider == "antigravity" {
						antigravity = auth
					}
				}
				opts := coreauth.Options{Headers: http.Header{"X-Session-Id": {"pause-stable-session"}}}
				if _, errPick := selector.Pick(t.Context(), "mixed", model, opts, []*coreauth.Auth{bound}); errPick != nil {
					t.Fatal(errPick)
				}
				adapter := &tosPauseTestAdapter{runtimeKVTestAdapter: runtimeKVTestAdapter{enabled: true}}
				runtime := &Runtime{cfg: cfg, coreManager: manager, clusterAdapter: adapter}
				before, errBefore := runtime.dispatchWithOptions(t.Context(), model, opts, DispatchConcurrencyContext{})
				if errBefore != nil || before == nil || before.AuthID != bound.ID {
					t.Fatalf("initial binding not used: %v", errBefore)
				}
				adapter.value = []byte(strconv.FormatInt(time.Now().Add(time.Hour).UnixNano(), 10))
				after, errAfter := runtime.dispatchWithOptions(t.Context(), model, opts, DispatchConcurrencyContext{})
				if errAfter != nil || after == nil || after.AuthID != bound.ID {
					t.Fatalf("pause changed another provider's binding: %v", errAfter)
				}
				cold := coreauth.Options{Headers: http.Header{"X-Session-Id": {"pause-cold-session"}}}
				decision, errCold := runtime.dispatchWithOptions(t.Context(), model, cold, DispatchConcurrencyContext{})
				if errCold != nil || decision == nil || decision.Provider == "antigravity" {
					t.Fatalf("paused pool remained eligible: %v", errCold)
				}
				if mode == "never" {
					pinned := coreauth.Options{Headers: http.Header{"X-Session-Id": {"pause-antigravity-bound"}}}
					if _, errPick := selector.Pick(t.Context(), "mixed", model, pinned, []*coreauth.Auth{antigravity}); errPick != nil {
						t.Fatal(errPick)
					}
					if selected, errPaused := runtime.dispatchWithOptions(t.Context(), model, pinned, DispatchConcurrencyContext{}); errPaused == nil || selected != nil {
						t.Fatal("never failed over a binding to the paused pool")
					}
				}
				adapter.value = nil
				resumed, errResumed := runtime.dispatchWithOptions(t.Context(), model, opts, DispatchConcurrencyContext{})
				if errResumed != nil || resumed == nil || resumed.AuthID != bound.ID {
					t.Fatalf("resume changed another provider's binding: %v", errResumed)
				}
			})
		}
	}
}

func TestAntigravityTOSPauseBoundCredentialRecovery(t *testing.T) {
	for _, strategy := range []string{"fill-first", "weighted-round-robin"} {
		for _, mode := range []string{"any", "quota", "never"} {
			t.Run(strategy+"/"+mode, func(t *testing.T) {
				cfg := &config.Config{Routing: config.RoutingConfig{Strategy: strategy, SessionAffinity: true, SessionAffinityRebindOn: mode}, Antigravity: config.AntigravityConfig{TOSPauseWindow: "6h"}}
				store := coreauth.NewSessionCache(time.Hour)
				t.Cleanup(store.Stop)
				manager := coreauth.NewManager(nil, selectorFromConfig(cfg, store), nil)
				manager.SetConfig(cfg)
				bound := &coreauth.Auth{ID: "pause-recovery-a", Provider: "antigravity", Status: coreauth.StatusActive, Attributes: map[string]string{"priority": "100"}}
				fallback := &coreauth.Auth{ID: "pause-recovery-b", Provider: "codex", Status: coreauth.StatusActive}
				const model = "pause-recovery-model"
				for _, candidate := range []*coreauth.Auth{bound, fallback} {
					if _, errRegister := manager.Register(t.Context(), candidate); errRegister != nil {
						t.Fatal(errRegister)
					}
					registry.GetGlobalRegistry().RegisterClient(candidate.ID, candidate.Provider, []*registry.ModelInfo{{ID: model}})
					manager.RefreshSchedulerEntry(candidate.ID)
					t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(candidate.ID) })
				}
				adapter := &tosPauseTestAdapter{runtimeKVTestAdapter: runtimeKVTestAdapter{enabled: true}}
				runtime := &Runtime{cfg: cfg, coreManager: manager, clusterAdapter: adapter}
				opts := coreauth.Options{Headers: http.Header{"X-Session-Id": {"pause-recovery-session"}}}
				initial, errInitial := runtime.dispatchWithOptions(t.Context(), model, opts, DispatchConcurrencyContext{})
				if errInitial != nil || initial == nil || initial.AuthID != bound.ID {
					t.Fatalf("initial dispatch = %+v, error = %v", initial, errInitial)
				}
				// Another credential's TOS violation pauses this healthy bound credential.
				adapter.value = []byte(strconv.FormatInt(time.Now().Add(time.Hour).UnixNano(), 10))
				for range 2 {
					paused, errPaused := runtime.dispatchWithOptions(t.Context(), model, opts, DispatchConcurrencyContext{})
					if mode == "never" {
						var authErr *coreauth.Error
						if paused != nil || !errors.As(errPaused, &authErr) || authErr.Code != "session_auth_unavailable" {
							t.Fatalf("never dispatch = %+v, error = %v", paused, errPaused)
						}
					} else if errPaused != nil || paused == nil || paused.AuthID != fallback.ID {
						t.Fatalf("paused dispatch = %+v, error = %v", paused, errPaused)
					}
				}
				adapter.value = nil
				recovered, errRecovered := runtime.dispatchWithOptions(t.Context(), model, opts, DispatchConcurrencyContext{})
				wantID := bound.ID
				if mode == "any" {
					wantID = fallback.ID
				}
				if errRecovered != nil || recovered == nil || recovered.AuthID != wantID {
					t.Fatalf("recovered dispatch = %+v, want %s, error = %v", recovered, wantID, errRecovered)
				}
			})
		}
	}
}

func TestAntigravityTOSPauseExcludesSchedulerCandidates(t *testing.T) {
	for _, strategy := range []string{"round-robin", "fill-first", "weighted-round-robin"} {
		t.Run(strategy, func(t *testing.T) {
			cfg := &config.Config{Routing: config.RoutingConfig{Strategy: strategy}, Antigravity: config.AntigravityConfig{TOSPauseWindow: "6h"}}
			manager := coreauth.NewManager(nil, selectorFromConfig(cfg, nil), nil)
			manager.SetConfig(cfg)
			model := "pause-scheduler-model"
			for _, provider := range []string{"antigravity", "codex"} {
				auth := &coreauth.Auth{ID: "pause-scheduler-" + provider, Provider: provider, Status: coreauth.StatusActive}
				if provider == "antigravity" {
					auth.Attributes = map[string]string{"priority": "100"}
				}
				if _, errRegister := manager.Register(t.Context(), auth); errRegister != nil {
					t.Fatal(errRegister)
				}
				registry.GetGlobalRegistry().RegisterClient(auth.ID, provider, []*registry.ModelInfo{{ID: model}})
				manager.RefreshSchedulerEntry(auth.ID)
				id := auth.ID
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
			}
			adapter := &tosPauseTestAdapter{runtimeKVTestAdapter: runtimeKVTestAdapter{enabled: true}, value: []byte(strconv.FormatInt(time.Now().Add(time.Hour).UnixNano(), 10))}
			runtime := &Runtime{cfg: cfg, coreManager: manager, clusterAdapter: adapter}
			selected, errDispatch := runtime.dispatchWithOptions(t.Context(), model, coreauth.Options{}, DispatchConcurrencyContext{})
			if errDispatch != nil || selected == nil || selected.Provider != "codex" {
				t.Fatalf("scheduler dispatched paused provider: %v", errDispatch)
			}
		})
	}
}

func TestAntigravityTOSPauseUsageClassification(t *testing.T) {
	ctx := t.Context()
	for _, tc := range []struct {
		name, provider, body string
		status               int
		wantPause            bool
	}{
		{"TOS", "antigravity", `{"error":{"code":403,"status":"PERMISSION_DENIED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"TOS_VIOLATION"}]}}`, 403, true},
		{"quota", "antigravity", `{"error":{"code":429}}`, 429, false},
		{"other forbidden", "antigravity", `{"error":{"code":403,"status":"PERMISSION_DENIED"}}`, 403, false},
		{"other provider", "codex", `{"error":{"code":403,"status":"PERMISSION_DENIED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"TOS_VIOLATION"}]}}`, 403, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			adapter := &tosPauseTestAdapter{runtimeKVTestAdapter: runtimeKVTestAdapter{enabled: true}}
			manager := coreauth.NewManager(nil, &coreauth.FillFirstSelector{}, nil)
			manager.SetConfig(&config.Config{})
			auth := &coreauth.Auth{ID: "usage-tos-auth", Index: "usage-tos-auth", Provider: tc.provider, Status: coreauth.StatusActive}
			if _, errRegister := manager.Register(ctx, auth); errRegister != nil {
				t.Fatal(errRegister)
			}
			runtime := &Runtime{cfg: &config.Config{Antigravity: config.AntigravityConfig{TOSPauseWindow: "6h"}}, coreManager: manager, clusterAdapter: adapter}
			payload, errMarshal := json.Marshal(map[string]any{"auth_index": auth.ID, "provider": tc.provider, "model": "tos-model", "timestamp": time.Now().Add(-time.Minute).Format(time.RFC3339Nano), "failed": true, "fail": map[string]any{"status_code": tc.status, "body": tc.body}})
			if errMarshal != nil {
				t.Fatal(errMarshal)
			}
			if errRecord := runtime.RecordUsagePayload(ctx, string(payload)); errRecord != nil {
				t.Fatal(errRecord)
			}
			if (adapter.value != nil) != tc.wantPause {
				t.Fatalf("pause recorded = %v, want %v", adapter.value != nil, tc.wantPause)
			}
		})
	}
}
