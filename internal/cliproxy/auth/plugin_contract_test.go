package auth

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type contractScheduler struct {
	across   bool
	requests []pluginapi.SchedulerPickRequest
	pick     func(pluginapi.SchedulerPickRequest) pluginapi.SchedulerPickResponse
}

func (s *contractScheduler) SchedulerWantsAcrossPriorities() bool { return s.across }
func (s *contractScheduler) PickAuth(_ context.Context, req pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, bool, error) {
	s.requests = append(s.requests, req)
	resp := s.pick(req)
	return resp, resp.Handled, nil
}

func TestPluginSchedulerPriorityContract(t *testing.T) {
	for _, session := range []bool{false, true} {
		for _, tc := range []struct {
			name   string
			across bool
			resp   pluginapi.SchedulerPickResponse
			want   string
			count  int
		}{
			{"highest only", false, pluginapi.SchedulerPickResponse{Handled: true, AuthID: "low"}, "high", 1},
			{"opt in", true, pluginapi.SchedulerPickResponse{Handled: true, AuthID: "low"}, "low", 2},
			{"exclude fallback", false, pluginapi.SchedulerPickResponse{ExcludeCandidates: []string{"high"}}, "low", 1},
			{"exclude delegate", false, pluginapi.SchedulerPickResponse{Handled: true, DelegateBuiltin: "fill-first", ExcludeCandidates: []string{"high"}}, "low", 1},
		} {
			t.Run(fmt.Sprintf("%t/%s", session, tc.name), func(t *testing.T) {
				var selector Selector = &FillFirstSelector{}
				if session {
					affinity := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: selector})
					t.Cleanup(affinity.Stop)
					selector = affinity
				}
				manager := NewManager(nil, selector, nil)
				for id, priority := range map[string]string{"high": "10", "low": "1"} {
					registerDispatchTestAuth(t, manager, &Auth{ID: id, Provider: "codex", Status: StatusActive, Attributes: map[string]string{"priority": priority}}, "priority-model")
				}
				scheduler := &contractScheduler{across: tc.across, pick: func(pluginapi.SchedulerPickRequest) pluginapi.SchedulerPickResponse { return tc.resp }}
				manager.SetPluginScheduler(scheduler)
				decision, errDispatch := manager.Dispatch(t.Context(), []string{"codex"}, "priority-model", Options{})
				if errDispatch != nil || decision == nil || decision.Auth.ID != tc.want {
					t.Fatalf("decision=%+v error=%v, want %s", decision, errDispatch, tc.want)
				}
				if len(scheduler.requests) != 1 || len(scheduler.requests[0].Candidates) != tc.count {
					t.Fatalf("requests=%+v, want %d candidates", scheduler.requests, tc.count)
				}
			})
		}
	}
}

func TestPluginSchedulerStreamAndSelectionIdentity(t *testing.T) {
	manager := NewManager(nil, &FillFirstSelector{}, nil)
	registerDispatchTestAuth(t, manager, &Auth{ID: "identity", Provider: "codex", Status: StatusActive}, "identity-model")
	scheduler := &contractScheduler{pick: func(req pluginapi.SchedulerPickRequest) pluginapi.SchedulerPickResponse {
		return pluginapi.SchedulerPickResponse{Handled: true, AuthID: req.Candidates[0].ID}
	}}
	manager.SetPluginScheduler(scheduler)
	metadata := map[string]any{pluginapi.SelectionIDMetadataKey: "caller-stale"}
	seen := map[string]bool{}
	for _, stream := range []bool{false, true} {
		decision, errDispatch := manager.Dispatch(t.Context(), []string{"codex"}, "identity-model", Options{Stream: stream, Metadata: metadata})
		if errDispatch != nil || decision == nil {
			t.Fatal(errDispatch)
		}
		request := scheduler.requests[len(scheduler.requests)-1]
		id, _ := request.Options.Metadata[pluginapi.SelectionIDMetadataKey].(string)
		if id == "" || id == "caller-stale" || id != decision.SelectionID || seen[id] || request.Stream != stream {
			t.Fatalf("request=%+v decision=%+v", request, decision)
		}
		seen[id] = true
	}
	if metadata[pluginapi.SelectionIDMetadataKey] != "caller-stale" {
		t.Fatal("dispatch mutated caller metadata")
	}
	scheduler.pick = func(pluginapi.SchedulerPickRequest) pluginapi.SchedulerPickResponse {
		return pluginapi.SchedulerPickResponse{}
	}
	decision, errDispatch := manager.Dispatch(t.Context(), []string{"codex"}, "identity-model", Options{Metadata: metadata})
	if errDispatch != nil || decision.SelectionID != "" {
		t.Fatalf("unhandled pick acquired plugin identity: %+v %v", decision, errDispatch)
	}
}

func TestCodexContinuityWeightFilteringMatchesPluginDispatch(t *testing.T) {
	for _, session := range []bool{false, true} {
		for _, plugin := range []string{"none", "inactive", "active"} {
			t.Run(fmt.Sprintf("session=%t/plugin=%s", session, plugin), func(t *testing.T) {
				const ready, cooling, model = "weight-zero-ready", "weight-positive-cooling", "continuity-weight-model"
				var selector Selector = &WeightedRoundRobinSelector{}
				if session {
					affinity := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: selector})
					t.Cleanup(affinity.Stop)
					selector = affinity
				}
				manager := NewManager(nil, selector, nil)
				deadline := time.Now().Add(time.Hour)
				registerDispatchTestAuth(t, manager, &Auth{
					ID: ready, Provider: "codex", Status: StatusActive, Attributes: map[string]string{"weight": "0"},
				}, model)
				registerDispatchTestAuth(t, manager, &Auth{
					ID: cooling, Provider: "codex", Status: StatusActive, Attributes: map[string]string{"weight": "1"},
					ModelStates: map[string]*ModelState{model: {
						Unavailable: true, NextRetryAfter: deadline,
						Quota: QuotaState{Exceeded: true, NextRecoverAt: deadline},
					}},
				}, model)
				switch plugin {
				case "inactive":
					manager.SetPluginScheduler(&statefulSchedulerPlugin{active: false})
				case "active":
					manager.SetPluginScheduler(&contractScheduler{pick: func(req pluginapi.SchedulerPickRequest) pluginapi.SchedulerPickResponse {
						if len(req.Candidates) != 1 || req.Candidates[0].ID != ready {
							t.Fatalf("dispatch candidates=%+v, want available zero-weight credential", req.Candidates)
						}
						return pluginapi.SchedulerPickResponse{Handled: true, AuthID: ready}
					}})
				}
				decision, errDispatch := manager.Dispatch(t.Context(), []string{"codex"}, model, Options{})
				if plugin == "active" {
					if errDispatch != nil || decision == nil || decision.Auth == nil || decision.Auth.ID != ready {
						t.Fatalf("dispatch=%+v error=%v, want zero-weight credential", decision, errDispatch)
					}
				} else {
					var cooldown *modelCooldownError
					if decision != nil || !errors.As(errDispatch, &cooldown) {
						t.Fatalf("dispatch=%+v error=%v, want builtin weighted cooldown", decision, errDispatch)
					}
				}
				candidates, errCandidates := manager.CodexContinuityCandidates(t.Context(), model, Options{}, false)
				if errCandidates != nil {
					t.Fatal(errCandidates)
				}
				wantCount := 1
				if plugin == "active" {
					wantCount = 2
				}
				if len(candidates) != wantCount || candidates[0].ID != cooling || !candidates[0].InCooldown {
					t.Fatalf("continuity candidates=%+v, want %d candidates including cooled positive-weight credential", candidates, wantCount)
				}
				if plugin == "active" && (candidates[1].ID != ready || candidates[1].InCooldown) {
					t.Fatalf("continuity candidates=%+v, want available zero-weight credential", candidates)
				}
				candidates, errCandidates = manager.CodexContinuityCandidates(t.Context(), model, Options{Metadata: map[string]any{AllowedAuthIDsMetadataKey: []string{cooling}}}, false)
				if errCandidates != nil || len(candidates) != 1 || candidates[0].ID != cooling {
					t.Fatalf("permission-filtered candidates=%+v error=%v", candidates, errCandidates)
				}
			})
		}
	}
}

func TestCodexContinuityDistinguishesQuotaFromNonQuotaFailure(t *testing.T) {
	manager := NewManager(nil, &FillFirstSelector{}, nil)
	deadline := time.Now().Add(time.Hour)
	registerDispatchTestAuth(t, manager, &Auth{
		ID: "global-quota", Provider: "codex", Status: StatusActive, Unavailable: true, NextRetryAfter: deadline,
		Quota:     QuotaState{Exceeded: true, Reason: "credential_quota", NextRecoverAt: deadline},
		LastError: &Error{HTTPStatus: 429}, ModelStates: map[string]*ModelState{"quota-model": {
			Unavailable: true, NextRetryAfter: deadline,
			Quota: QuotaState{Exceeded: true, NextRecoverAt: deadline},
		}},
	}, "quota-model")
	registerDispatchTestAuth(t, manager, &Auth{
		ID: "quota-with-new-transport-error", Provider: "codex", Status: StatusActive,
		ModelStates: map[string]*ModelState{"quota-model": {
			Unavailable: true, NextRetryAfter: deadline, LastError: &Error{HTTPStatus: 503},
			Quota: QuotaState{Exceeded: true, NextRecoverAt: deadline},
		}},
	}, "quota-model")
	candidates, errCandidates := manager.CodexContinuityCandidates(t.Context(), "quota-model", Options{}, false)
	if errCandidates != nil || len(candidates) != 1 || candidates[0].ID != "global-quota" || !candidates[0].InCooldown {
		t.Fatalf("candidates=%+v error=%v, want only credential-wide quota", candidates, errCandidates)
	}
}

func TestCodexContinuityCandidatesRespectDispatchRestrictions(t *testing.T) {
	manager := NewManager(nil, &FillFirstSelector{}, nil)
	deadline := time.Now().Add(time.Hour)
	for _, auth := range []*Auth{
		{ID: "ready", Provider: "codex", Status: StatusActive},
		{ID: "quota", Provider: "codex", Status: StatusActive, ModelStates: map[string]*ModelState{"continuity-model": {Unavailable: true, NextRetryAfter: deadline, Quota: QuotaState{Exceeded: true, NextRecoverAt: deadline}}}},
		{ID: "transport", Provider: "codex", Status: StatusActive, ModelStates: map[string]*ModelState{"continuity-model": {Unavailable: true, NextRetryAfter: deadline, LastError: &Error{HTTPStatus: 503}}}},
		{ID: "disabled", Provider: "codex", Disabled: true},
	} {
		registerDispatchTestAuth(t, manager, auth, "continuity-model")
	}
	candidates, errCandidates := manager.CodexContinuityCandidates(t.Context(), "continuity-model", Options{}, false)
	if errCandidates != nil || len(candidates) != 2 || candidates[0].ID != "quota" || !candidates[0].InCooldown || candidates[1].ID != "ready" {
		t.Fatalf("candidates=%+v error=%v", candidates, errCandidates)
	}
	for _, metadata := range []map[string]any{
		{AllowedAuthIDsMetadataKey: []string{"ready"}, PinnedAuthIDMetadataKey: "quota"},
		{AllowedModelIDsMetadataKey: []string{}},
		{AllowedAuthIDsMetadataKey: []string{"quota"}, ExcludedAuthIDsMetadataKey: []string{"quota"}},
	} {
		candidates, errCandidates = manager.CodexContinuityCandidates(t.Context(), "continuity-model", Options{Metadata: metadata}, false)
		if errCandidates != nil || len(candidates) != 0 {
			t.Fatalf("metadata=%v candidates=%+v error=%v", metadata, candidates, errCandidates)
		}
	}
}
