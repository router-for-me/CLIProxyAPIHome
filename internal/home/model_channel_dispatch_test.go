package home

import (
	"context"
	"errors"
	"reflect"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
)

type modelChannelDispatchTestAdapter struct {
	allowedAuthIDs  []string
	allowedModelIDs []string
	requestedModel  string
}

func (a *modelChannelDispatchTestAdapter) Enabled() bool { return true }

func (a *modelChannelDispatchTestAdapter) LoadAuthIndex(context.Context) error { return nil }

func (a *modelChannelDispatchTestAdapter) ListMinimalAuths() []*coreauth.Auth { return nil }

func (a *modelChannelDispatchTestAdapter) GetFullAuth(context.Context, string) (*coreauth.Auth, error) {
	return nil, errors.New("not implemented")
}

func (a *modelChannelDispatchTestAdapter) LoadConfigYAML(context.Context) ([]byte, error) {
	return nil, errors.New("not implemented")
}

func (a *modelChannelDispatchTestAdapter) AllowedDispatchIDsForAPIKeyModel(_ context.Context, _ string, modelID string) ([]string, []string, error) {
	a.requestedModel = modelID
	return a.allowedAuthIDs, a.allowedModelIDs, nil
}

func TestDispatchForAPIKeyAppliesModelSpecificAuthIDs(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	modelID := "gpt-test-model"
	for _, authID := range []string{"auth-a", "auth-b"} {
		registry.GetGlobalRegistry().RegisterClient(authID, "codex", []*registry.ModelInfo{{ID: modelID, Object: "model", Type: "openai"}})
		auth := &coreauth.Auth{ID: authID, Provider: "codex", Status: coreauth.StatusActive}
		if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
			t.Fatalf("Register(%s) error = %v", authID, errRegister)
		}
		t.Cleanup(func() {
			registry.GetGlobalRegistry().UnregisterClient(authID)
		})
	}

	adapter := &modelChannelDispatchTestAdapter{
		allowedAuthIDs:  []string{"auth-b"},
		allowedModelIDs: []string{modelID},
	}
	runtime := &Runtime{coreManager: manager, clusterAdapter: adapter}
	result, errDispatch := runtime.DispatchForAPIKey(context.Background(), modelID+"(high)", nil, "client-key")
	if errDispatch != nil {
		t.Fatalf("DispatchForAPIKey() error = %v", errDispatch)
	}
	if result == nil || result.AuthID != "auth-b" {
		t.Fatalf("DispatchForAPIKey() auth = %#v, want auth-b", result)
	}
	if adapter.requestedModel != modelID {
		t.Fatalf("policy model = %q, want %q", adapter.requestedModel, modelID)
	}
	if !reflect.DeepEqual(adapter.allowedAuthIDs, []string{"auth-b"}) {
		t.Fatalf("adapter allowed auth IDs mutated: %v", adapter.allowedAuthIDs)
	}
}

func TestDispatchForAPIKeyFailsClosedForEmptyModelChannel(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	modelID := "gpt-empty-model"
	authID := "auth-empty"
	registry.GetGlobalRegistry().RegisterClient(authID, "codex", []*registry.ModelInfo{{ID: modelID, Object: "model", Type: "openai"}})
	if _, errRegister := manager.Register(context.Background(), &coreauth.Auth{ID: authID, Provider: "codex", Status: coreauth.StatusActive}); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(authID)
	})

	adapter := &modelChannelDispatchTestAdapter{
		allowedAuthIDs:  []string{},
		allowedModelIDs: []string{modelID},
	}
	runtime := &Runtime{coreManager: manager, clusterAdapter: adapter}
	result, errDispatch := runtime.DispatchForAPIKey(context.Background(), modelID, nil, "client-key")
	if errDispatch == nil {
		t.Fatalf("DispatchForAPIKey() result = %#v, want error", result)
	}
	if result != nil {
		t.Fatalf("DispatchForAPIKey() result = %#v, want nil", result)
	}
}
