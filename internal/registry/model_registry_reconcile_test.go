package registry

import (
	"reflect"
	"sync"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

func TestModelRegistryStateLogsOnlyTransitions(t *testing.T) {
	const clientID = "state-client"
	const modelID = "state-model"
	modelRegistry := &ModelRegistry{
		models: make(map[string]*ModelRegistration), clientModels: make(map[string][]string),
		clientModelInfos: make(map[string]map[string]*ModelInfo), clientProviders: make(map[string]string),
		mutex: &sync.RWMutex{},
	}
	modelRegistry.RegisterClient(clientID, "gemini", []*ModelInfo{{ID: modelID}})

	savedLevel := log.GetLevel()
	savedHooks := log.StandardLogger().ReplaceHooks(make(log.LevelHooks))
	hook := logtest.NewGlobal()
	log.SetLevel(log.DebugLevel)
	t.Cleanup(func() {
		log.SetLevel(savedLevel)
		log.StandardLogger().ReplaceHooks(savedHooks)
	})

	modelRegistry.SetModelQuotaExceeded(clientID, modelID)
	registration := modelRegistry.models[modelID]
	oldQuotaTime := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	registration.QuotaExceededClients[clientID] = &oldQuotaTime
	beforeRefresh := time.Now()
	modelRegistry.SetModelQuotaExceeded(clientID, modelID)
	quotaTime := registration.QuotaExceededClients[clientID]
	if quotaTime == nil || quotaTime.Before(beforeRefresh) {
		t.Fatalf("quota timestamp = %v, want refreshed timestamp", quotaTime)
	}
	if !quotaTime.Add(modelQuotaExceededWindow).After(beforeRefresh) {
		t.Fatal("repeated quota marker did not renew the quota window")
	}

	modelRegistry.SuspendClientModel(clientID, modelID, "quota")
	registration.LastUpdated = oldQuotaTime
	modelRegistry.SuspendClientModel(clientID, modelID, "quota")
	if !registration.LastUpdated.Equal(oldQuotaTime) {
		t.Fatal("identical suspension changed LastUpdated")
	}
	modelRegistry.SuspendClientModel(clientID, modelID, "forbidden")
	if registration.SuspendedClients[clientID] != "forbidden" || !registration.LastUpdated.After(oldQuotaTime) {
		t.Fatal("suspension reason transition was not applied")
	}
	modelRegistry.SuspendClientModel(clientID, modelID, "forbidden")
	modelRegistry.ResumeClientModel(clientID, modelID)
	modelRegistry.ResumeClientModel(clientID, modelID)
	if _, exists := registration.SuspendedClients[clientID]; exists {
		t.Fatal("resume retained suspension state")
	}
	modelRegistry.ClearModelQuotaExceeded(clientID, modelID)
	modelRegistry.SetModelQuotaExceeded(clientID, modelID)

	var messages []string
	for _, entry := range hook.AllEntries() {
		messages = append(messages, entry.Message)
	}
	want := []string{
		"Marked model state-model as quota exceeded for client state-client",
		"Suspended client state-client for model state-model: quota",
		"Updated suspension reason for client state-client for model state-model: quota -> forbidden",
		"Resumed client state-client for model state-model",
		"Marked model state-model as quota exceeded for client state-client",
	}
	if !reflect.DeepEqual(messages, want) {
		t.Fatalf("state logs = %#v, want %#v", messages, want)
	}
}

func TestRegisterClientPreservesUnchangedBindingState(t *testing.T) {
	for _, tc := range []struct {
		name         string
		initialCount int
		nextCount    int
	}{
		{name: "metadata-only", initialCount: 1, nextCount: 1},
		{name: "duplicate-added", initialCount: 1, nextCount: 2},
		{name: "duplicate-removed", initialCount: 2, nextCount: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const clientID = "preserved-client"
			const modelID = "preserved-model"
			modelRegistry := &ModelRegistry{
				models: make(map[string]*ModelRegistration), clientModels: make(map[string][]string),
				clientModelInfos: make(map[string]map[string]*ModelInfo), clientProviders: make(map[string]string),
				mutex: &sync.RWMutex{},
			}
			models := make([]*ModelInfo, tc.initialCount)
			for i := range models {
				models[i] = &ModelInfo{ID: modelID, Name: "before"}
			}
			modelRegistry.RegisterClient(clientID, "gemini", models)
			modelRegistry.SetModelQuotaExceeded(clientID, modelID)
			modelRegistry.SuspendClientModel(clientID, modelID, "quota")
			quotaTime := modelRegistry.models[modelID].QuotaExceededClients[clientID]

			models = make([]*ModelInfo, tc.nextCount)
			for i := range models {
				models[i] = &ModelInfo{ID: modelID, Name: "after"}
			}
			modelRegistry.RegisterClient(clientID, "gemini", models)
			registration := modelRegistry.models[modelID]
			if registration.QuotaExceededClients[clientID] != quotaTime || registration.SuspendedClients[clientID] != "quota" {
				t.Fatal("re-registration cleared unchanged binding state")
			}
			if registration.Count != tc.nextCount || registration.Providers["gemini"] != tc.nextCount || registration.Info.Name != "after" {
				t.Fatalf("registration metadata/counts not updated: %#v", registration)
			}
		})
	}
}

func TestRegisterClientClearsRemovedBindingState(t *testing.T) {
	for _, operation := range []string{"provider-change", "provider-removed", "model-removed", "empty-models", "unregister"} {
		t.Run(operation, func(t *testing.T) {
			const clientID = "removed-client"
			const peerID = "retained-peer"
			const modelID = "shared-state-model"
			modelRegistry := &ModelRegistry{
				models: make(map[string]*ModelRegistration), clientModels: make(map[string][]string),
				clientModelInfos: make(map[string]map[string]*ModelInfo), clientProviders: make(map[string]string),
				mutex: &sync.RWMutex{},
			}
			models := []*ModelInfo{{ID: modelID}}
			for _, id := range []string{clientID, peerID} {
				modelRegistry.RegisterClient(id, "gemini", models)
				modelRegistry.SetModelQuotaExceeded(id, modelID)
				modelRegistry.SuspendClientModel(id, modelID, "quota")
			}
			peerQuotaTime := modelRegistry.models[modelID].QuotaExceededClients[peerID]
			switch operation {
			case "provider-change":
				modelRegistry.RegisterClient(clientID, "vertex", models)
			case "provider-removed":
				modelRegistry.RegisterClient(clientID, "", models)
			case "model-removed":
				modelRegistry.RegisterClient(clientID, "gemini", []*ModelInfo{{ID: "replacement-model"}})
			case "empty-models":
				modelRegistry.RegisterClient(clientID, "gemini", nil)
			case "unregister":
				modelRegistry.UnregisterClient(clientID)
			}
			registration := modelRegistry.models[modelID]
			if _, exists := registration.QuotaExceededClients[clientID]; exists {
				t.Fatal("removed binding retained quota state")
			}
			if _, exists := registration.SuspendedClients[clientID]; exists {
				t.Fatal("removed binding retained suspension state")
			}
			if registration.QuotaExceededClients[peerID] != peerQuotaTime || registration.SuspendedClients[peerID] != "quota" {
				t.Fatal("removed binding changed peer state")
			}
		})
	}
}
