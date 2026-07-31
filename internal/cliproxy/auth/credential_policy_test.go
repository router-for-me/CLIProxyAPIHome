package auth

import (
	"context"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func codexPolicyOptions() Options {
	return Options{Metadata: map[string]any{CredentialPolicyMetadataKey: CredentialPolicyCodexAlphaSearchV1}}
}

func TestCredentialPolicyAllowsCodexAlphaSearch(t *testing.T) {
	tests := []struct {
		name string
		auth *Auth
		want bool
	}{
		{name: "OAuth", auth: &Auth{Provider: "codex", Attributes: map[string]string{"auth_kind": "oauth"}}, want: true},
		{name: "inferred OAuth", auth: &Auth{Provider: "codex", Metadata: map[string]any{"access_token": "token"}}, want: true},
		{name: "opted-in API key", auth: &Auth{Provider: "codex", Attributes: map[string]string{"api_key": "key", AttributeCodexAlphaSearch: "true"}}, want: true},
		{name: "API key wins OAuth shape", auth: &Auth{Provider: "codex", Attributes: map[string]string{"api_key": "key"}, Metadata: map[string]any{"access_token": "token"}}},
		{name: "non-canonical opt-in", auth: &Auth{Provider: "codex", Attributes: map[string]string{"api_key": "key", AttributeCodexAlphaSearch: "1"}}},
		{name: "ordinary API key", auth: &Auth{Provider: "codex", Attributes: map[string]string{"api_key": "key"}}},
		{name: "invalid opt-in", auth: &Auth{Provider: "codex", Attributes: map[string]string{"api_key": "key", AttributeCodexAlphaSearch: "enabled"}}},
		{name: "xAI OAuth", auth: &Auth{Provider: "xai", Attributes: map[string]string{"auth_kind": "oauth", AttributeCodexAlphaSearch: "true"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := credentialPolicyAllows(CredentialPolicyCodexAlphaSearchV1, test.auth); got != test.want {
				t.Fatalf("credentialPolicyAllows() = %t, want %t", got, test.want)
			}
			if !credentialPolicyAllows("", test.auth) {
				t.Fatal("empty policy changed credential eligibility")
			}
		})
	}
}

func TestDispatchCredentialPolicyFiltersRoundRobinFastPath(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	auths := []*Auth{
		{ID: "ordinary", Provider: "codex", Status: StatusActive, Attributes: map[string]string{"api_key": "ordinary"}},
		{ID: "oauth", Provider: "codex", Status: StatusActive, Attributes: map[string]string{"auth_kind": "oauth"}},
		{ID: "opted-in", Provider: "codex", Status: StatusActive, Attributes: map[string]string{"api_key": "opted-in", AttributeCodexAlphaSearch: "true"}},
	}
	for _, auth := range auths {
		registerDispatchTestAuth(t, manager, auth, "gpt")
	}

	seen := map[string]bool{}
	for range 4 {
		decision, errDispatch := manager.Dispatch(context.Background(), []string{"codex"}, "gpt", codexPolicyOptions())
		if errDispatch != nil {
			t.Fatalf("Dispatch() error = %v", errDispatch)
		}
		if decision == nil || decision.Auth == nil || decision.Auth.ID == "ordinary" {
			t.Fatalf("Dispatch() = %#v, ordinary API key must be filtered", decision)
		}
		seen[decision.Auth.ID] = true
	}
	if !seen["oauth"] || !seen["opted-in"] {
		t.Fatalf("round-robin selected = %#v, want both eligible credentials", seen)
	}
}

type credentialPolicyCandidateSelector struct {
	candidateIDs []string
}

func (s *credentialPolicyCandidateSelector) Pick(_ context.Context, _ string, _ string, _ Options, auths []*Auth) (*Auth, error) {
	s.candidateIDs = s.candidateIDs[:0]
	for _, auth := range auths {
		if auth != nil {
			s.candidateIDs = append(s.candidateIDs, auth.ID)
		}
	}
	if len(auths) == 0 {
		return nil, nil
	}
	return auths[0], nil
}

func TestDispatchCredentialPolicyFiltersCustomSelectorSlowPath(t *testing.T) {
	selector := &credentialPolicyCandidateSelector{}
	manager := NewManager(nil, selector, nil)
	registerDispatchTestAuth(t, manager, &Auth{ID: "ordinary-slow", Provider: "codex", Status: StatusActive, Attributes: map[string]string{"api_key": "ordinary"}}, "gpt")
	registerDispatchTestAuth(t, manager, &Auth{ID: "oauth-slow", Provider: "codex", Status: StatusActive, Attributes: map[string]string{"auth_kind": "oauth"}}, "gpt")

	decision, errDispatch := manager.Dispatch(context.Background(), []string{"codex"}, "gpt", codexPolicyOptions())
	if errDispatch != nil {
		t.Fatalf("Dispatch() error = %v", errDispatch)
	}
	if decision == nil || decision.Auth == nil || decision.Auth.ID != "oauth-slow" {
		t.Fatalf("Dispatch() = %#v, want oauth-slow", decision)
	}
	if len(selector.candidateIDs) != 1 || selector.candidateIDs[0] != "oauth-slow" {
		t.Fatalf("custom selector candidates = %#v, want only oauth-slow", selector.candidateIDs)
	}
}

type credentialPolicyPluginScheduler struct {
	request pluginapi.SchedulerPickRequest
}

func (s *credentialPolicyPluginScheduler) PickAuth(_ context.Context, req pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, bool, error) {
	s.request = req
	return pluginapi.SchedulerPickResponse{Handled: true, AuthID: "opted-plugin"}, true, nil
}

func TestDispatchCredentialPolicyFiltersPluginCandidates(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	registerDispatchTestAuth(t, manager, &Auth{ID: "ordinary-plugin", Provider: "codex", Status: StatusActive, Attributes: map[string]string{"api_key": "ordinary"}}, "gpt")
	registerDispatchTestAuth(t, manager, &Auth{ID: "opted-plugin", Provider: "codex", Status: StatusActive, Attributes: map[string]string{"api_key": "opted", AttributeCodexAlphaSearch: "true"}}, "gpt")
	scheduler := &credentialPolicyPluginScheduler{}
	manager.SetPluginScheduler(scheduler)

	decision, errDispatch := manager.Dispatch(context.Background(), []string{"codex"}, "gpt", codexPolicyOptions())
	if errDispatch != nil {
		t.Fatalf("Dispatch() error = %v", errDispatch)
	}
	if decision == nil || decision.Auth == nil || decision.Auth.ID != "opted-plugin" {
		t.Fatalf("Dispatch() = %#v, want opted-plugin", decision)
	}
	if len(scheduler.request.Candidates) != 1 || scheduler.request.Candidates[0].ID != "opted-plugin" {
		t.Fatalf("plugin candidates = %#v, want only opted-plugin", scheduler.request.Candidates)
	}
}

func TestDispatchCredentialPolicyRechecksResolvedFullAuth(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	minimalMismatch := &Auth{ID: "minimal-opted", Provider: "codex", Status: StatusActive, Attributes: map[string]string{"api_key": "minimal", AttributeCodexAlphaSearch: "true"}}
	eligible := &Auth{ID: "eligible-oauth", Provider: "codex", Status: StatusActive, Attributes: map[string]string{"auth_kind": "oauth"}}
	registerDispatchTestAuth(t, manager, minimalMismatch, "gpt")
	registerDispatchTestAuth(t, manager, eligible, "gpt")
	fullMismatch := minimalMismatch.Clone()
	delete(fullMismatch.Attributes, AttributeCodexAlphaSearch)
	manager.SetFullAuthResolver(dispatchTestFullAuthResolver{auths: map[string]*Auth{
		minimalMismatch.ID: fullMismatch,
		eligible.ID:        eligible,
	}})

	decision, errDispatch := manager.Dispatch(context.Background(), []string{"codex"}, "gpt", codexPolicyOptions())
	if errDispatch != nil {
		t.Fatalf("Dispatch() error = %v", errDispatch)
	}
	if decision == nil || decision.Auth == nil || decision.Auth.ID != eligible.ID {
		t.Fatalf("Dispatch() = %#v, want defensive full-auth recheck to select %s", decision, eligible.ID)
	}
}

func TestDispatchWithoutCredentialPolicyPreservesEligibility(t *testing.T) {
	manager := NewManager(nil, &FillFirstSelector{}, nil)
	ordinary := &Auth{ID: "ordinary-regression", Provider: "codex", Status: StatusActive, Attributes: map[string]string{"api_key": "ordinary"}}
	registerDispatchTestAuth(t, manager, ordinary, "gpt")
	decision, errDispatch := manager.Dispatch(context.Background(), []string{"codex"}, "gpt", Options{})
	if errDispatch != nil {
		t.Fatalf("Dispatch() error = %v", errDispatch)
	}
	if decision == nil || decision.Auth == nil || decision.Auth.ID != ordinary.ID {
		t.Fatalf("Dispatch() = %#v, want ordinary credential without policy", decision)
	}
}
