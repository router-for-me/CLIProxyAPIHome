package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

func TestReconcileRegistryModelStatesLogsOnlyTransitions(t *testing.T) {
	const authID = "registry-idempotent-auth"
	const model = "registry-idempotent-model"
	ctx := context.Background()
	modelRegistry := registry.GetGlobalRegistry()
	models := []*registry.ModelInfo{{ID: model, Object: "model", Type: "gemini"}}
	modelRegistry.RegisterClient(authID, "gemini", models)
	t.Cleanup(func() { modelRegistry.UnregisterClient(authID) })
	manager := NewManager(nil, nil, nil)
	if _, errRegister := manager.Register(ctx, &Auth{
		ID: authID, Index: authID, Provider: "gemini", Status: StatusActive,
		ModelStates: map[string]*ModelState{model: quotaCooldownModelState(time.Now().UTC(), time.Hour)},
	}); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}

	savedLevel := log.GetLevel()
	savedHooks := log.StandardLogger().ReplaceHooks(make(log.LevelHooks))
	hook := logtest.NewGlobal()
	log.SetLevel(log.DebugLevel)
	t.Cleanup(func() {
		log.SetLevel(savedLevel)
		log.StandardLogger().ReplaceHooks(savedHooks)
	})

	manager.ReconcileRegistryModelStates(ctx, authID)
	if entries := hook.AllEntries(); len(entries) != 2 {
		t.Fatalf("initial quota logs = %#v, want quota and suspension logs", entries)
	}
	hook.Reset()
	for range 3 {
		modelRegistry.RegisterClient(authID, "gemini", models)
		manager.ReconcileRegistryModelStates(ctx, authID)
		manager.ReconcileRegistryModelStates(ctx, authID)
	}
	if entries := hook.AllEntries(); len(entries) != 0 {
		t.Fatalf("unchanged reconciliation emitted logs: %#v", entries)
	}

	auth, ok := manager.GetByID(authID)
	if !ok || auth == nil {
		t.Fatal("registered auth missing")
	}
	auth.ModelStates[model] = &ModelState{
		Status: StatusError, StatusMessage: "forbidden", Unavailable: true,
		NextRetryAfter: time.Now().UTC().Add(time.Hour),
		LastError:      &Error{Message: "forbidden", HTTPStatus: http.StatusForbidden},
	}
	if _, errUpdate := manager.Update(ctx, auth); errUpdate != nil {
		t.Fatalf("Update() error = %v", errUpdate)
	}
	manager.ReconcileRegistryModelStates(ctx, authID)
	manager.ReconcileRegistryModelStates(ctx, authID)
	if entries := hook.AllEntries(); len(entries) != 1 || entries[0].Message != "Updated suspension reason for client "+authID+" for model "+model+": quota -> forbidden" {
		t.Fatalf("suspension transition logs = %#v, want one reason update without resume", entries)
	}
	if registryHasAvailableModel(modelRegistry, model) {
		t.Fatal("forbidden model remained available after reason transition")
	}

	hook.Reset()
	auth.ModelStates = nil
	if _, errUpdate := manager.Update(ctx, auth); errUpdate != nil {
		t.Fatalf("Update() error = %v", errUpdate)
	}
	manager.ReconcileRegistryModelStates(ctx, authID)
	manager.ReconcileRegistryModelStates(ctx, authID)
	if entries := hook.AllEntries(); len(entries) != 1 || entries[0].Message != "Resumed client "+authID+" for model "+model {
		t.Fatalf("recovery logs = %#v, want one resume", entries)
	}
	if !registryHasAvailableModel(modelRegistry, model) {
		t.Fatal("recovered model remained unavailable")
	}
}
