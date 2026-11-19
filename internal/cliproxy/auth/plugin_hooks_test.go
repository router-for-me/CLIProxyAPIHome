package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type unhandledSchedulerPlugin struct{}

func (s *unhandledSchedulerPlugin) PickAuth(context.Context, pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, bool, error) {
	return pluginapi.SchedulerPickResponse{}, false, nil
}

type statefulSchedulerPlugin struct {
	unhandledSchedulerPlugin
	active bool
}

func (s *statefulSchedulerPlugin) HasScheduler() bool {
	return s.active
}

func TestDispatchWeightedPinnedAccountingUsesPluginState(t *testing.T) {
	for _, active := range []bool{false, true} {
		t.Run(fmt.Sprintf("active=%t", active), func(t *testing.T) {
			selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: &WeightedRoundRobinSelector{}})
			t.Cleanup(selector.Stop)
			manager := NewManager(nil, selector, nil)
			manager.SetPluginScheduler(&statefulSchedulerPlugin{active: active})
			const provider, model = "gemini", "plugin-pinned-model"
			aID, bID := t.Name()+"-a", t.Name()+"-b"
			registerDispatchTestAuth(t, manager, &Auth{
				ID: aID, Provider: provider, Status: StatusActive, Attributes: map[string]string{"weight": "3"},
			}, model)
			registerDispatchTestAuth(t, manager, &Auth{
				ID: bID, Provider: provider, Status: StatusActive, Attributes: map[string]string{"weight": "1"},
			}, model)
			for i := range 4 {
				decision, errDispatch := manager.Dispatch(t.Context(), []string{provider}, model, Options{
					Headers:  http.Header{"Session_id": {"pinned"}},
					Metadata: map[string]any{PinnedAuthIDMetadataKey: bID},
				})
				if errDispatch != nil || decision == nil || decision.Auth == nil || decision.Auth.ID != bID {
					t.Fatalf("pinned dispatch %d: decision=%v err=%v", i, decision, errDispatch)
				}
			}
			counts := map[string]int{}
			for i := range 12 {
				decision, errDispatch := manager.Dispatch(t.Context(), []string{provider}, model, Options{
					Headers: http.Header{"Session_id": {fmt.Sprintf("fresh-%d", i)}},
				})
				if errDispatch != nil || decision == nil || decision.Auth == nil {
					t.Fatalf("fresh dispatch %d: decision=%v err=%v", i, decision, errDispatch)
				}
				counts[decision.Auth.ID]++
			}
			wantA, wantB := 12, 0
			if active {
				// An active plugin declining a pick must not enable shared-pool accounting.
				wantA, wantB = 9, 3
			}
			if counts[aID] != wantA || counts[bID] != wantB {
				t.Fatalf("fresh distribution=%v, want a=%d b=%d", counts, wantA, wantB)
			}
		})
	}
}

type delegatingSchedulerPlugin struct{}

func (s *delegatingSchedulerPlugin) PickAuth(context.Context, pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, bool, error) {
	return pluginapi.SchedulerPickResponse{
		Handled:         true,
		DelegateBuiltin: pluginapi.SchedulerBuiltinFillFirst,
	}, true, nil
}

func TestDispatchPluginUnhandledPreservesBuiltinScheduler(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	first := geminiAPIKeyAuth("plugin-fallback-a", "first-key", "1")
	second := geminiAPIKeyAuth("plugin-fallback-b", "second-key", "1")
	registerDispatchTestAuth(t, manager, first, "plugin-fallback-model")
	registerDispatchTestAuth(t, manager, second, "plugin-fallback-model")

	decision, errDispatch := manager.Dispatch(context.Background(), []string{"gemini"}, "plugin-fallback-model", Options{})
	if errDispatch != nil {
		t.Fatalf("first Dispatch() error = %v", errDispatch)
	}
	if decision == nil || decision.Auth == nil || decision.Auth.ID != first.ID {
		t.Fatalf("first Dispatch() auth = %#v, want %s", decision, first.ID)
	}

	manager.SetPluginScheduler(&unhandledSchedulerPlugin{})
	decision, errDispatch = manager.Dispatch(context.Background(), []string{"gemini"}, "plugin-fallback-model", Options{})
	if errDispatch != nil {
		t.Fatalf("second Dispatch() error = %v", errDispatch)
	}
	if decision == nil || decision.Auth == nil || decision.Auth.ID != second.ID {
		t.Fatalf("second Dispatch() auth = %#v, want builtin scheduler to continue with %s", decision, second.ID)
	}
}

func TestDispatchPluginDelegateBuiltinUsesConcurrencyFilteredCandidates(t *testing.T) {
	tests := []struct {
		name     string
		excluded ExcludedConcurrencyCandidate
	}{
		{
			name:     "saturated credential",
			excluded: ExcludedConcurrencyCandidate{CredentialID: "plugin-delegate-a", Model: "plugin-delegate-model"},
		},
		{
			name:     "protocol required credential",
			excluded: ExcludedConcurrencyCandidate{CredentialID: "plugin-delegate-a"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manager := NewManager(nil, &FillFirstSelector{}, nil)
			first := geminiAPIKeyAuth("plugin-delegate-a", "first-key", "1")
			second := geminiAPIKeyAuth("plugin-delegate-b", "second-key", "1")
			registerDispatchTestAuth(t, manager, first, "plugin-delegate-model")
			registerDispatchTestAuth(t, manager, second, "plugin-delegate-model")
			manager.SetPluginScheduler(&delegatingSchedulerPlugin{})

			decision, errDispatch := manager.Dispatch(context.Background(), []string{"gemini"}, "plugin-delegate-model", Options{
				Metadata: map[string]any{
					ExcludedConcurrencyCandidatesMetadataKey: []ExcludedConcurrencyCandidate{test.excluded},
				},
			})
			if errDispatch != nil {
				t.Fatalf("Dispatch() error = %v", errDispatch)
			}
			if decision == nil || decision.Auth == nil || decision.Auth.ID != second.ID {
				t.Fatalf("Dispatch() auth = %#v, want %s", decision, second.ID)
			}
		})
	}
}

type schedulerPickResponseStub struct {
	response pluginapi.SchedulerPickResponse
}

func (s *schedulerPickResponseStub) PickAuth(context.Context, pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, bool, error) {
	return s.response, s.response.Handled, nil
}

func TestDispatchPluginExcludesCandidates(t *testing.T) {
	const model = "plugin-exclusions-model"
	const a, b, c = "plugin-exclusions-a", "plugin-exclusions-b", "plugin-exclusions-c"
	for _, tc := range []struct {
		name        string
		useWeighted bool
		delegate    bool
		direct      string
		excluded    []string
		metadata    map[string]any
		want        string
	}{
		{name: "builtin fallback", excluded: []string{" ", " " + a + " ", a, "unknown"}, want: b},
		{name: "weighted fallback", useWeighted: true, excluded: []string{a, c}, want: b},
		{name: "delegated builtin", delegate: true, excluded: []string{a}, want: b},
		{name: "delegation with model concurrency exclusion", delegate: true, excluded: []string{a}, metadata: map[string]any{
			ExcludedConcurrencyCandidatesMetadataKey: []ExcludedConcurrencyCandidate{{CredentialID: b, Model: model}},
		}, want: c},
		{name: "fallback with prior retry exclusion", excluded: []string{a}, metadata: map[string]any{
			ExcludedAuthIDsMetadataKey: []string{b},
		}, want: c},
		{name: "all excluded on fallback", excluded: []string{a, b, c}},
		{name: "all excluded on delegation", delegate: true, excluded: []string{a, b, c}},
		{name: "pinned credential excluded", excluded: []string{a}, metadata: map[string]any{PinnedAuthIDMetadataKey: a}},
		{name: "direct pick takes precedence", direct: a, excluded: []string{a}, want: a},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var selector Selector = &FillFirstSelector{}
			if tc.useWeighted {
				selector = &WeightedRoundRobinSelector{}
			}
			manager := NewManager(nil, selector, nil)
			for _, id := range []string{a, b, c} {
				registerDispatchTestAuth(t, manager, geminiAPIKeyAuth(id, id, "1"), model)
			}
			plugin := &schedulerPickResponseStub{response: pluginapi.SchedulerPickResponse{
				Handled: tc.delegate || tc.direct != "", AuthID: tc.direct, ExcludeCandidates: tc.excluded,
			}}
			if tc.delegate {
				plugin.response.DelegateBuiltin = pluginapi.SchedulerBuiltinFillFirst
			}
			manager.SetPluginScheduler(plugin)
			opts := Options{Metadata: tc.metadata}
			decision, errDispatch := manager.Dispatch(t.Context(), []string{"gemini"}, model, opts)
			if tc.want == "" {
				var authErr *Error
				if decision != nil || !errors.As(errDispatch, &authErr) || authErr.Code != "auth_not_found" {
					t.Fatalf("Dispatch() = %+v, %v, want no candidate", decision, errDispatch)
				}
			} else if errDispatch != nil || decision == nil || decision.Auth == nil || decision.Auth.ID != tc.want {
				t.Fatalf("Dispatch() = %+v, %v, want %s", decision, errDispatch, tc.want)
			}

			// Exclusions apply only to this selection pass, not the credential state or caller options.
			plugin.response = pluginapi.SchedulerPickResponse{Handled: true, AuthID: a}
			decision, errDispatch = manager.Dispatch(t.Context(), []string{"gemini"}, model, opts)
			if errDispatch != nil || decision == nil || decision.Auth == nil || decision.Auth.ID != a {
				t.Fatalf("subsequent Dispatch() = %+v, %v, want %s", decision, errDispatch, a)
			}
		})
	}
}

func TestDispatchPluginExclusionsPreserveNextRoundAvailability(t *testing.T) {
	for _, delegate := range []bool{false, true} {
		for _, canRetry := range []bool{false, true} {
			t.Run(fmt.Sprintf("delegate=%t/reusable=%t", delegate, canRetry), func(t *testing.T) {
				manager := NewManager(nil, &FillFirstSelector{}, nil)
				const tried, excluded = "plugin-next-tried", "plugin-next-excluded"
				retry := 0
				if canRetry {
					retry = 1
				}
				registerDispatchTestAuth(t, manager, &Auth{ID: tried, Provider: "codex", Status: StatusActive, Metadata: map[string]any{"request_retry": retry}}, "gpt")
				registerDispatchTestAuth(t, manager, &Auth{ID: excluded, Provider: "codex", Status: StatusActive, Metadata: map[string]any{"request_retry": 3}}, "gpt")
				registerDispatchTestAuth(t, manager, dispatchRetryCooldownAuth("plugin-next-cooling", 2), "gpt")
				response := pluginapi.SchedulerPickResponse{Handled: delegate, ExcludeCandidates: []string{excluded}}
				if delegate {
					response.DelegateBuiltin = pluginapi.SchedulerBuiltinFillFirst
				}
				plugin := &schedulerPickResponseStub{response: response}
				manager.SetPluginScheduler(plugin)

				decision, errDispatch := manager.Dispatch(t.Context(), []string{"codex"}, "gpt", Options{Metadata: map[string]any{
					ExcludedAuthIDsMetadataKey: []string{tried}, RequestRetryRoundMetadataKey: 0,
				}})
				var authErr *Error
				if decision != nil || !errors.As(errDispatch, &authErr) || authErr.Code != "auth_unavailable" {
					t.Fatalf("Dispatch() = %+v, %v, want immediate retry", decision, errDispatch)
				}
				// At the next pick the plugin can admit a credential it previously excluded.
				plugin.response.ExcludeCandidates = []string{tried}
				decision, errDispatch = manager.Dispatch(t.Context(), []string{"codex"}, "gpt", Options{Metadata: map[string]any{RequestRetryRoundMetadataKey: 1}})
				if errDispatch != nil || decision == nil || decision.Auth.ID != excluded {
					t.Fatalf("next round = %+v, %v; want %s", decision, errDispatch, excluded)
				}
			})
		}
	}
}

func TestDispatchPluginDelegationHonorsNeverBindingWithOtherReadyCredential(t *testing.T) {
	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: &FillFirstSelector{}, RebindOn: "never"})
	t.Cleanup(selector.Stop)
	manager := NewManager(nil, selector, nil)
	const bound, other = "plugin-never-a", "plugin-never-b"
	for _, id := range []string{bound, other} {
		registerDispatchTestAuth(t, manager, &Auth{ID: id, Provider: "codex", Status: StatusActive}, "gpt")
	}
	providers := []string{"codex"}
	opts := Options{Headers: http.Header{"X-Session-Id": {"sticky"}}}
	decision, errDispatch := manager.Dispatch(t.Context(), providers, "gpt", opts)
	if errDispatch != nil || decision == nil || decision.Auth.ID != bound {
		t.Fatalf("initial dispatch = %+v, %v; want %s", decision, errDispatch, bound)
	}
	manager.SetPluginScheduler(&schedulerPickResponseStub{response: pluginapi.SchedulerPickResponse{
		Handled: true, DelegateBuiltin: pluginapi.SchedulerBuiltinFillFirst, ExcludeCandidates: []string{bound},
	}})
	decision, errDispatch = manager.Dispatch(t.Context(), providers, "gpt", opts)
	var rejected *Error
	if decision != nil || !errors.As(errDispatch, &rejected) || rejected.Code != "session_auth_unavailable" || rejected.BoundCredentialID() != bound {
		t.Fatalf("bound dispatch = %+v, %v; want session_auth_unavailable for %s", decision, errDispatch, bound)
	}
	decision, errDispatch = manager.Dispatch(t.Context(), providers, "gpt", Options{Headers: http.Header{"X-Session-Id": {"new"}}})
	if errDispatch != nil || decision == nil || decision.Auth.ID != other {
		t.Fatalf("unbound dispatch = %+v, %v; want %s", decision, errDispatch, other)
	}
}

func TestDispatchPluginExclusionPreservesNeverBindingBeforeCooldown(t *testing.T) {
	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: &FillFirstSelector{}, RebindOn: "never"})
	t.Cleanup(selector.Stop)
	manager := NewManager(nil, selector, nil)
	const ready = "plugin-never-ready"
	registerDispatchTestAuth(t, manager, &Auth{ID: ready, Provider: "codex", Status: StatusActive}, "gpt")
	registerDispatchTestAuth(t, manager, dispatchRetryCooldownAuth("plugin-never-cooling", 2), "gpt")
	providers := []string{"codex"}
	opts := Options{Headers: http.Header{"X-Session-Id": {"bound"}}}
	if decision, errDispatch := manager.Dispatch(t.Context(), providers, "gpt", opts); errDispatch != nil || decision.Auth.ID != ready {
		t.Fatalf("initial dispatch = %+v, %v; want binding to %s", decision, errDispatch, ready)
	}
	manager.SetPluginScheduler(&schedulerPickResponseStub{response: pluginapi.SchedulerPickResponse{
		Handled: true, DelegateBuiltin: pluginapi.SchedulerBuiltinFillFirst, ExcludeCandidates: []string{ready},
	}})
	decision, errDispatch := manager.Dispatch(t.Context(), providers, "gpt", opts)
	var authErr *Error
	if decision != nil || !errors.As(errDispatch, &authErr) || authErr.Code != "session_auth_unavailable" {
		t.Fatalf("bound dispatch = %+v, %v; want session_auth_unavailable", decision, errDispatch)
	}
	decision, errDispatch = manager.Dispatch(t.Context(), providers, "gpt", Options{Headers: http.Header{"X-Session-Id": {"unbound"}}})
	var cooldown *modelCooldownError
	if decision != nil || !errors.As(errDispatch, &cooldown) || cooldown.RetryAfter() == nil {
		t.Fatalf("unbound dispatch = %+v, %v; want a timed cooldown", decision, errDispatch)
	}
}

func TestDispatchPluginWeightedFallbackIgnoresZeroWeightForCooldown(t *testing.T) {
	manager := NewManager(nil, &WeightedRoundRobinSelector{}, nil)
	const ready = "plugin-weighted-ready"
	registerDispatchTestAuth(t, manager, &Auth{ID: ready, Provider: "codex", Status: StatusActive}, "gpt")
	registerDispatchTestAuth(t, manager, &Auth{
		ID: "plugin-weighted-zero", Provider: "codex", Status: StatusActive,
		Attributes: map[string]string{"weight": "0"}, Metadata: map[string]any{"request_retry": 9},
	}, "gpt")
	registerDispatchTestAuth(t, manager, dispatchRetryCooldownAuth("plugin-weighted-cooling", 2), "gpt")
	manager.SetPluginScheduler(&schedulerPickResponseStub{response: pluginapi.SchedulerPickResponse{ExcludeCandidates: []string{ready}}})
	decision, errDispatch := manager.Dispatch(t.Context(), []string{"codex"}, "gpt", Options{})
	var cooldown *modelCooldownError
	if decision != nil || !errors.As(errDispatch, &cooldown) || cooldown.RetryAfter() == nil {
		t.Fatalf("Dispatch() = %+v, %v; want a timed cooldown", decision, errDispatch)
	}
	if retry, ok := cooldown.RequestRetryLimit(); !ok || retry != 2 {
		t.Fatalf("request retry = %d, %t; want the cooling credential's limit of 2", retry, ok)
	}
}

func TestDispatchPluginFillFirstForecastRespectsConfiguredWeightEligibility(t *testing.T) {
	for _, weighted := range []bool{false, true} {
		t.Run(fmt.Sprintf("weighted=%t", weighted), func(t *testing.T) {
			var selector Selector = &FillFirstSelector{}
			if weighted {
				selector = &WeightedRoundRobinSelector{}
			}
			manager := NewManager(nil, selector, nil)
			const ready = "plugin-delegated-ready"
			registerDispatchTestAuth(t, manager, &Auth{ID: ready, Provider: "codex", Status: StatusActive}, "gpt")
			cooling := dispatchRetryCooldownAuth("plugin-delegated-zero", 2)
			cooling.Attributes["weight"] = "0"
			registerDispatchTestAuth(t, manager, cooling, "gpt")
			manager.SetPluginScheduler(&schedulerPickResponseStub{response: pluginapi.SchedulerPickResponse{
				Handled: true, DelegateBuiltin: pluginapi.SchedulerBuiltinFillFirst, ExcludeCandidates: []string{ready},
			}})
			decision, errDispatch := manager.Dispatch(t.Context(), []string{"codex"}, "gpt", Options{})
			if decision != nil {
				t.Fatalf("Dispatch() = %+v, %v; want no credential", decision, errDispatch)
			}
			if weighted {
				var authErr *Error
				if !errors.As(errDispatch, &authErr) || authErr.Code != "auth_not_found" {
					t.Fatalf("Dispatch() error = %v; want auth_not_found, not zero-weight cooldown", errDispatch)
				}
			} else {
				var cooldown *modelCooldownError
				if !errors.As(errDispatch, &cooldown) || cooldown.RetryAfter() == nil {
					t.Fatalf("Dispatch() error = %v; want zero-weight cooldown under fill-first", errDispatch)
				}
			}
		})
	}
}

func TestDispatchPluginWeightedNextRoundIgnoresZeroWeight(t *testing.T) {
	for _, session := range []bool{false, true} {
		for _, zeroCooling := range []bool{false, true} {
			t.Run(fmt.Sprintf("session=%t/zeroCooling=%t", session, zeroCooling), func(t *testing.T) {
				var selector Selector = &WeightedRoundRobinSelector{}
				if session {
					affinity := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: selector})
					t.Cleanup(affinity.Stop)
					selector = affinity
				}
				manager := NewManager(nil, selector, nil)
				zero := dispatchRetryCooldownAuth("plugin-next-zero", 9)
				zero.Attributes["weight"] = "0"
				if !zeroCooling {
					zero.ModelStates = nil
				}
				positive := dispatchRetryCooldownAuth("plugin-next-positive", 2)
				deadline := time.Now().Add(time.Hour)
				positive.ModelStates["gpt"].NextRetryAfter = deadline
				positive.ModelStates["gpt"].Quota.NextRecoverAt = deadline
				registerDispatchTestAuth(t, manager, zero, "gpt")
				registerDispatchTestAuth(t, manager, positive, "gpt")
				manager.SetPluginScheduler(&schedulerPickResponseStub{})
				decision, errDispatch := manager.Dispatch(t.Context(), []string{"codex"}, "gpt", Options{Metadata: map[string]any{
					ExcludedAuthIDsMetadataKey: []string{zero.ID, positive.ID}, RequestRetryRoundMetadataKey: 0,
				}})
				var cooldown *modelCooldownError
				if decision != nil || !errors.As(errDispatch, &cooldown) {
					t.Fatalf("Dispatch() = %+v, %v; want positive-weight next-round cooldown", decision, errDispatch)
				}
				if delay := cooldown.RetryAfter(); delay == nil || *delay < 59*time.Minute || *delay > time.Hour {
					t.Fatalf("retry delay=%v, want approximately one hour", delay)
				}
				if retry, ok := cooldown.RequestRetryLimit(); !ok || retry != 2 {
					t.Fatalf("request retry=%d, %t, want 2", retry, ok)
				}
			})
		}
	}
}

func TestDispatchPluginMixedRoundRobinUsesEligibleProviderCounts(t *testing.T) {
	const model = "plugin-mixed-exclusions-model"
	for _, delegate := range []bool{false, true} {
		t.Run(fmt.Sprintf("delegate=%t", delegate), func(t *testing.T) {
			manager := NewManager(nil, &RoundRobinSelector{}, nil)
			var excluded []string
			for i := range 10 {
				id := fmt.Sprintf("plugin-mixed-gemini-%d", i)
				registerDispatchTestAuth(t, manager, geminiAPIKeyAuth(id, id, "1"), model)
				if i != 0 {
					excluded = append(excluded, id)
				}
			}
			vertex := geminiAPIKeyAuth("plugin-mixed-vertex", "vertex-key", "1")
			vertex.Provider = "vertex"
			registerDispatchTestAuth(t, manager, vertex, model)
			plugin := &schedulerPickResponseStub{response: pluginapi.SchedulerPickResponse{Handled: delegate}}
			if delegate {
				plugin.response.DelegateBuiltin = pluginapi.SchedulerBuiltinRoundRobin
			}
			manager.SetPluginScheduler(plugin)

			for _, phase := range []struct {
				exclude           []string
				calls, wantGemini int
			}{{nil, 22, 20}, {excluded, 22, 11}} {
				plugin.response.ExcludeCandidates = phase.exclude
				geminiPicks := 0
				for range phase.calls {
					decision, errDispatch := manager.Dispatch(t.Context(), []string{"gemini", "vertex"}, model, Options{})
					if errDispatch != nil || decision == nil || decision.Auth == nil {
						t.Fatalf("Dispatch() = %+v, %v", decision, errDispatch)
					}
					if decision.Provider == "gemini" {
						geminiPicks++
						if len(phase.exclude) > 0 && decision.Auth.ID != "plugin-mixed-gemini-0" {
							t.Fatalf("picked excluded credential %s", decision.Auth.ID)
						}
					} else if decision.Auth.ID != vertex.ID {
						t.Fatalf("unexpected pick: %+v", decision)
					}
				}
				if geminiPicks != phase.wantGemini {
					t.Fatalf("Gemini picks = %d/%d, want %d/%d", geminiPicks, phase.calls, phase.wantGemini, phase.calls)
				}
			}
		})
	}
}

func TestDispatchPluginRejectsWithoutFallback(t *testing.T) {
	const model, id = "plugin-reject-model", "plugin-reject-a"
	manager := NewManager(nil, &FillFirstSelector{}, nil)
	registerDispatchTestAuth(t, manager, geminiAPIKeyAuth(id, "key", "1"), model)
	manager.SetPluginScheduler(&schedulerPickResponseStub{response: pluginapi.SchedulerPickResponse{
		Handled: true, Reject: true, AuthID: id, DelegateBuiltin: pluginapi.SchedulerBuiltinFillFirst,
	}})
	decision, errDispatch := manager.Dispatch(t.Context(), []string{"gemini"}, model, Options{})
	var rejection *SchedulerRejectError
	var cause *Error
	if decision != nil || !errors.As(errDispatch, &rejection) || !errors.As(errDispatch, &cause) ||
		cause.Code != "auth_unavailable" || cause.Message != "scheduler rejected candidate selection" {
		t.Fatalf("Dispatch() = %+v, %v, want terminal rejection with default code and message", decision, errDispatch)
	}
}

func TestSchedulerAuthCandidatesAreSanitized(t *testing.T) {
	auth := &Auth{
		ID:       "auth-a",
		Provider: "Gemini",
		Status:   StatusActive,
		Attributes: map[string]string{
			"access_token":  "token-value",
			"api_key":       "api-key-value",
			"cookie":        "cookie-value",
			"priority":      "7",
			"team":          "alpha",
			"authorization": "bearer token",
		},
		Metadata: map[string]any{
			"tenant":        "one",
			"refresh_token": "refresh-token",
		},
	}

	candidates := schedulerAuthCandidates([]*Auth{auth})
	if len(candidates) != 1 {
		t.Fatalf("len(candidates) = %d, want 1", len(candidates))
	}

	candidate := candidates[0]
	if candidate.ID != "auth-a" || candidate.Provider != "gemini" || candidate.Priority != 7 || candidate.Status != string(StatusActive) {
		t.Fatalf("candidate = %#v, want sanitized auth-a", candidate)
	}
	for _, key := range []string{"access_token", "api_key", "cookie", "authorization"} {
		if _, ok := candidate.Attributes[key]; ok {
			t.Fatalf("candidate Attributes contains sensitive key %q", key)
		}
	}
	if candidate.Attributes["priority"] != "7" || candidate.Attributes["team"] != "alpha" {
		t.Fatalf("candidate Attributes = %#v, want priority/team", candidate.Attributes)
	}
	if len(candidate.Metadata) != 0 {
		t.Fatalf("candidate Metadata = %#v, want empty", candidate.Metadata)
	}

	candidate.Attributes["team"] = "mutated"
	if auth.Attributes["team"] != "alpha" {
		t.Fatalf("source attribute team = %q, want alpha", auth.Attributes["team"])
	}
}
