package management

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
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

func TestBillingImportDiscoversExecutionModelBehindAliases(t *testing.T) {
	devinAuth := &coreauth.Auth{
		ID: t.Name() + "-devin", Index: t.Name() + "-devin", Provider: "devin", Prefix: "team", Status: coreauth.StatusActive,
		Attributes: map[string]string{"auth_kind": "oauth"},
	}
	handler, repo := newCooldownManagementHandler(t, devinAuth)
	manager := handler.runtime.CoreManager()
	manager.SetOAuthModelAlias(map[string][]config.OAuthModelAlias{
		"devin": {{Name: "devin/plugin-upstream", Alias: "plugin-chat"}},
	})
	copilotAuth := &coreauth.Auth{ID: t.Name() + "-copilot", Index: t.Name() + "-copilot", Provider: "copilot", Status: coreauth.StatusActive}
	customAuth := &coreauth.Auth{
		ID: t.Name() + "-custom", Index: t.Name() + "-custom", Provider: "z-ai", Prefix: "team", Status: coreauth.StatusActive,
		Attributes: map[string]string{"auth_kind": "apikey"},
		Metadata:   map[string]any{"home_config_models": []map[string]any{{"id": "MY-MODEL", "name": "glm-5.3-flash", "user_defined": true}}},
	}
	for _, auth := range []*coreauth.Auth{copilotAuth, customAuth} {
		if _, errRegister := manager.Register(coreauth.WithSkipPersist(t.Context()), auth); errRegister != nil {
			t.Fatal(errRegister)
		}
	}
	modelRegistry := registry.GetGlobalRegistry()
	for _, auth := range []*coreauth.Auth{devinAuth, copilotAuth, customAuth} {
		authID := auth.ID
		t.Cleanup(func() { modelRegistry.UnregisterClient(authID) })
	}
	modelRegistry.RegisterClient(devinAuth.ID, devinAuth.Provider, []*registry.ModelInfo{
		{ID: "team/chat", SourceModelID: "devin/swe-2"},
		{ID: "fast", SourceModelID: "devin/swe-2"},
		{ID: "team/plugin-direct"},
		{ID: "team/plugin-chat"},
		{ID: "retired", SourceModelID: "devin/retired-native-model"},
	})
	modelRegistry.RegisterClientWithCandidates(copilotAuth.ID, copilotAuth.Provider,
		[]*registry.ModelInfo{{ID: "stale-copilot", SourceModelID: "copilot/stale-model"}},
		[]*registry.ModelInfo{{ID: "copilot/stale-model"}},
	)
	modelRegistry.RegisterClient(customAuth.ID, customAuth.Provider, []*registry.ModelInfo{{ID: "team/MY-MODEL"}})
	for _, payload := range []string{
		`{"timestamp":"2026-10-27T01:00:00Z","provider":"devin","model":"legacy-model","request_id":"legacy","tokens":{"input_tokens":1}}`,
		`{"timestamp":"2026-10-27T01:00:01Z","provider":"devin","model":"devin/swe-2","request_id":"static-overlap","tokens":{"input_tokens":1}}`,
	} {
		if _, errUsage := repo.AppendUsage(t.Context(), payload, "192.0.2.1"); errUsage != nil {
			t.Fatal(errUsage)
		}
	}
	targets, errDiscover := repo.DiscoverBillingImportTargets(t.Context(), handler.billingImportRegistryTargets)
	if errDiscover != nil {
		t.Fatal(errDiscover)
	}
	got := make(map[billingModelTarget]int, len(targets))
	for _, target := range targets {
		got[billingModelTarget{Provider: target.Provider, Model: target.Model}]++
	}
	want := map[billingModelTarget]int{
		{Provider: "devin", Model: "legacy-model"}:          1,
		{Provider: "devin", Model: "plugin-direct"}:         1,
		{Provider: "devin", Model: "devin/plugin-upstream"}: 1,
		{Provider: "z-ai", Model: "glm-5.3-flash"}:          1,
	}
	for _, channel := range []string{"copilot", "devin"} {
		for _, model := range registry.GetStaticModelDefinitionsByChannel(channel) {
			if model == nil {
				continue
			}
			want[billingModelTarget{Provider: channel, Model: catalogRouteModelID(channel, model.ID)}] = 1
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("discovered billing targets = %v, want %v", got, want)
	}
	if len(modelRegistry.GetModelsForClient(copilotAuth.ID)) != 1 {
		t.Fatal("billing target discovery unexpectedly pruned Copilot runtime models")
	}

	engine := gin.New()
	engine.GET("/v8/management/models", handler.GetModelsV8)
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v8/management/models?scope=available", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /v8/management/models?scope=available status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if len(modelRegistry.GetModelsForClient(copilotAuth.ID)) != 0 {
		t.Fatal("available models endpoint did not prune stale Copilot runtime models")
	}
}

func TestBillingImportAutoDiscoverRejectsOverflowBeforeCatalogFetch(t *testing.T) {
	db, cleanup := openManagementLogTestDB(t)
	t.Cleanup(cleanup)
	prices := make([]cluster.BillingModelPriceRecord, 10000)
	for i := range prices {
		id := fmt.Sprintf("model-%d", i)
		prices[i] = cluster.BillingModelPriceRecord{ID: id, Provider: "openai", Model: id, Enabled: true}
	}
	if errSeed := db.CreateInBatches(prices, 100).Error; errSeed != nil {
		t.Fatal(errSeed)
	}
	handler := NewHandler(cluster.NewRepository(db), nil, "192.0.2.1", 0)
	handler.SetModelsDevHTTPClient(&http.Client{Transport: billingImportRoundTripper(func(_ *http.Request) (*http.Response, error) {
		t.Fatal("target overflow must be rejected before fetching the catalog")
		return nil, nil
	})})
	engine := gin.New()
	const path = "/v8/management/billing/model-prices/import/preview"
	engine.POST(path, handler.PreviewBillingModelPriceImport)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"auto_discover":true}`))
	request.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(recorder, request)
	var response struct {
		Error string `json:"error"`
	}
	if errDecode := json.Unmarshal(recorder.Body.Bytes(), &response); errDecode != nil {
		t.Fatal(errDecode)
	}
	if recorder.Code != http.StatusUnprocessableEntity || response.Error != "import_target_limit_exceeded" {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}
