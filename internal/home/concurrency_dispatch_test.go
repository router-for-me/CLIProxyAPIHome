package home

import (
	"context"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
)

type dispatchSpySelector struct {
	calls int
}

func (s *dispatchSpySelector) Pick(_ context.Context, _ string, _ string, _ coreauth.Options, auths []*coreauth.Auth) (*coreauth.Auth, error) {
	s.calls++
	if len(auths) == 0 {
		return nil, nil
	}
	return auths[0], nil
}

func TestCredentialPolicyFiltersBeforeConcurrencyAdmission(t *testing.T) {
	manager := coreauth.NewManager(nil, &coreauth.RoundRobinSelector{}, nil)
	ordinary := &coreauth.Auth{ID: "ordinary-codex", Provider: "codex", Status: coreauth.StatusActive, Attributes: map[string]string{"api_key": "ordinary"}}
	oauth := &coreauth.Auth{ID: "oauth-codex", Provider: "codex", Status: coreauth.StatusActive, Attributes: map[string]string{"auth_kind": "oauth"}}
	for _, auth := range []*coreauth.Auth{ordinary, oauth} {
		registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "gpt"}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
		if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
			t.Fatalf("Register(%s) error = %v", auth.ID, errRegister)
		}
	}

	admittedIDs := make([]string, 0, 1)
	runtime := &Runtime{coreManager: manager}
	runtime.SetConcurrencyAdmitter(ConcurrencyAdmitterFunc(func(_ context.Context, req ConcurrencyAdmissionRequest) (ConcurrencyAdmissionResult, error) {
		admittedIDs = append(admittedIDs, req.CredentialID)
		return ConcurrencyAdmissionResult{CredentialID: req.CredentialID, Model: req.Model}, nil
	}))

	result, errDispatch := runtime.dispatchWithOptions(context.Background(), "gpt", coreauth.Options{Metadata: map[string]any{
		coreauth.CredentialPolicyMetadataKey: coreauth.CredentialPolicyCodexAlphaSearchV1,
	}}, DispatchConcurrencyContext{})
	if errDispatch != nil {
		t.Fatalf("dispatchWithOptions() error = %v", errDispatch)
	}
	if result == nil || result.AuthID != oauth.ID {
		t.Fatalf("dispatch result = %#v, want %s", result, oauth.ID)
	}
	if len(admittedIDs) != 1 || admittedIDs[0] != oauth.ID {
		t.Fatalf("admitted credentials = %#v, ordinary key must not consume concurrency", admittedIDs)
	}
}

func TestDispatchRejectsMalformedUTF8BeforeSchedulingOrAdmitting(t *testing.T) {
	requestedModel := "mapped-model(HIGH\xff)"
	routeModel := "mapped-model"
	selector := &dispatchSpySelector{}
	manager := coreauth.NewManager(nil, selector, nil)
	auth := &coreauth.Auth{
		ID:       "malformed-model-auth",
		Provider: "openai",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"api_key": "test-key",
		},
		Metadata: map[string]any{
			"home_config_models": []map[string]any{{
				"id":           routeModel,
				"name":         "valid-upstream-model",
				"user_defined": true,
			}},
		},
	}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: routeModel}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })

	mapped, errMapped := manager.Dispatch(context.Background(), []string{auth.Provider}, routeModel, coreauth.Options{})
	if errMapped != nil {
		t.Fatalf("Dispatch() mapping error = %v", errMapped)
	}
	if mapped == nil || mapped.UpstreamModel != "valid-upstream-model" {
		t.Fatalf("Dispatch() mapping result = %#v", mapped)
	}
	selectorCallsBeforeRequest := selector.calls

	admitted := false
	runtime := &Runtime{coreManager: manager}
	runtime.SetConcurrencyAdmitter(ConcurrencyAdmitterFunc(func(_ context.Context, _ ConcurrencyAdmissionRequest) (ConcurrencyAdmissionResult, error) {
		admitted = true
		return ConcurrencyAdmissionResult{}, nil
	}))

	result, errDispatch := runtime.dispatchWithOptions(context.Background(), requestedModel, coreauth.Options{}, DispatchConcurrencyContext{})
	if errDispatch == nil || result != nil {
		t.Fatalf("dispatch result = %#v, error = %v", result, errDispatch)
	}
	if selector.calls != selectorCallsBeforeRequest || admitted {
		t.Fatalf("malformed model scheduling/admission calls = %d/%t, want %d/false", selector.calls, admitted, selectorCallsBeforeRequest)
	}
}
