package registry

import (
	"sync"
	"testing"
)

func TestUnregisteredClientCannotMutateModelState(t *testing.T) {
	const activeClientID = "active-client"
	const unregisteredClientID = "unregistered-client"
	const modelID = "shared-model"
	modelRegistry := &ModelRegistry{
		models:           make(map[string]*ModelRegistration),
		clientModels:     make(map[string][]string),
		clientModelInfos: make(map[string]map[string]*ModelInfo),
		clientProviders:  make(map[string]string),
		mutex:            &sync.RWMutex{},
	}
	modelRegistry.RegisterClient(activeClientID, "gemini", []*ModelInfo{{ID: modelID, Object: "model", Type: "gemini"}})

	modelRegistry.SetModelQuotaExceeded(unregisteredClientID, modelID)
	modelRegistry.SuspendClientModel(unregisteredClientID, modelID, "disabled")

	registration := modelRegistry.models[modelID]
	if registration == nil {
		t.Fatal("model registration missing")
	}
	if _, exists := registration.QuotaExceededClients[unregisteredClientID]; exists {
		t.Fatal("unregistered client added quota state")
	}
	if _, exists := registration.SuspendedClients[unregisteredClientID]; exists {
		t.Fatal("unregistered client added suspension state")
	}
	models := modelRegistry.GetAvailableModelDefinitions()
	if len(models) != 1 || models[0] == nil || models[0].ID != modelID {
		t.Fatalf("available models = %#v, want only %q", models, modelID)
	}
}
