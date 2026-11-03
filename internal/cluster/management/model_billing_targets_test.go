package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
)

func TestModelsV8BillingTargetsResolvePerCredential(t *testing.T) {
	const alias = "MY-MODEL"
	makeAuth := func(id, provider, upstream string) *coreauth.Auth {
		return &coreauth.Auth{
			ID: t.Name() + id, Index: t.Name() + id, Provider: provider, Prefix: "team", Status: coreauth.StatusActive,
			Attributes: map[string]string{"auth_kind": "apikey"},
			Metadata:   map[string]any{"home_config_models": []map[string]any{{"id": alias, "name": upstream, "user_defined": true}}},
		}
	}
	first := makeAuth("first", "z-ai", "glm-5.3-flash")
	handler, _ := newCooldownManagementHandler(t, first)
	manager := handler.runtime.CoreManager()
	modelRegistry := registry.GetGlobalRegistry()
	auths := []*coreauth.Auth{
		first,
		makeAuth("same-provider", "z-ai", "glm-next"),
		makeAuth("other-provider", "another-provider", "other-upstream"),
		makeAuth("duplicate", "z-ai", "glm-5.3-flash"),
		makeAuth("disabled", "z-ai", "disabled-upstream"),
		{ID: t.Name() + "oauth", Provider: "codex", Prefix: "team", Status: coreauth.StatusActive, Attributes: map[string]string{"auth_kind": "oauth"}},
	}
	auths[4].Disabled = true
	for i, auth := range auths {
		if i > 0 {
			if _, errRegister := manager.Register(coreauth.WithSkipPersist(context.Background()), auth); errRegister != nil {
				t.Fatal(errRegister)
			}
		}
		modelRegistry.RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "team/" + alias}})
		t.Cleanup(func() { modelRegistry.UnregisterClient(auth.ID) })
	}
	manager.SetOAuthModelAlias(map[string][]config.OAuthModelAlias{"codex": {{Name: "gpt-upstream", Alias: alias}}})
	engine := gin.New()
	engine.GET("/v8/management/models", handler.GetModelsV8)
	engine.GET("/v0/management/models", handler.GetModels)
	read := func(path string) []billingModelDefinition {
		t.Helper()
		recorder := httptest.NewRecorder()
		engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
		}
		var payload struct {
			Models []billingModelDefinition `json:"models"`
		}
		if errDecode := json.Unmarshal(recorder.Body.Bytes(), &payload); errDecode != nil {
			t.Fatal(errDecode)
		}
		return payload.Models
	}
	find := func(models []billingModelDefinition) billingModelDefinition {
		t.Helper()
		for _, model := range models {
			if model.ID == "team/"+alias {
				return model
			}
		}
		t.Fatal("alias missing from model catalog")
		return billingModelDefinition{}
	}
	want := []billingModelTarget{
		{Provider: "another-provider", Model: "other-upstream"},
		{Provider: "codex", Model: "gpt-upstream"},
		{Provider: "z-ai", Model: "glm-5.3-flash"},
		{Provider: "z-ai", Model: "glm-next"},
	}
	if got := find(read("/v8/management/models?scope=available")).BillingModels; !reflect.DeepEqual(got, want) {
		t.Fatalf("billing targets = %#v, want %#v", got, want)
	}
	if got := find(read("/v0/management/models?scope=available")).BillingModels; got != nil {
		t.Fatalf("legacy response changed: %#v", got)
	}
	modelRegistry.UnregisterClient(auths[1].ID)
	if got := find(read("/v8/management/models?scope=available")).BillingModels; !reflect.DeepEqual(got, want[:3]) {
		t.Fatalf("removed credential still contributes a target: %#v", got)
	}
}

func TestModelsV8DoesNotInventUnresolvedBillingTargets(t *testing.T) {
	modelRegistry := registry.GetGlobalRegistry()
	id := t.Name()
	modelRegistry.RegisterClient(id, "z-ai", []*registry.ModelInfo{{ID: id, Name: "unverified-name"}})
	t.Cleanup(func() { modelRegistry.UnregisterClient(id) })
	handler := &Handler{}
	for _, model := range handler.availableBillingModelDefinitions() {
		if model.ID == id {
			if model.BillingModels == nil || len(model.BillingModels) != 0 {
				t.Fatalf("unresolved catalog must expose an empty target list: %#v", model.BillingModels)
			}
			return
		}
	}
	t.Fatal("missing registered model")
}
