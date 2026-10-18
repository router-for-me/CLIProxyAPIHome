package get

import (
	"context"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
	"github.com/router-for-me/CLIProxyAPIHome/internal/respserver/dispatch"
	"github.com/tidwall/gjson"
)

func TestBuildModelsJSONUltrafast(t *testing.T) {
	rt := newGetTestRuntime(t)
	register := func(auth *coreauth.Auth, models ...*registry.ModelInfo) {
		t.Helper()
		if _, errRegister := rt.CoreManager().Register(t.Context(), auth); errRegister != nil {
			t.Fatal(errRegister)
		}
		registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, models)
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	}
	shared := &registry.ModelInfo{ID: "gpt-6.1-sol"}
	register(&coreauth.Auth{
		ID: "ultrafast-pro", Provider: "codex", Status: coreauth.StatusActive,
		Attributes: map[string]string{"plan_type": "pro"},
	}, shared, &registry.ModelInfo{ID: "pro-only"}, &registry.ModelInfo{ID: "disabled-only"})
	proMax := &coreauth.Auth{
		ID: "ultrafast-promax", Provider: "codex", Status: coreauth.StatusActive,
		Attributes: map[string]string{"plan_type": " PROMAX "},
	}
	register(proMax, shared, &registry.ModelInfo{ID: "team/gpt-6-astra"})
	register(&coreauth.Auth{
		ID: "ultrafast-disabled", Provider: "codex", Status: coreauth.StatusDisabled, Disabled: true,
		Attributes: map[string]string{"plan_type": "promax"},
	}, &registry.ModelInfo{ID: "disabled-only"})
	register(&coreauth.Auth{
		ID: "ultrafast-other-provider", Provider: "openai", Status: coreauth.StatusActive,
		Attributes: map[string]string{"plan_type": "promax"},
	}, shared)
	assertAvailability := func(available bool) {
		t.Helper()
		payload, errBuild := buildModelsJSON(dispatch.Env{Runtime: rt})
		if errBuild != nil {
			t.Fatal(errBuild)
		}
		for _, id := range []string{"gpt-6.1-sol", "team/gpt-6-astra", "pro-only", "disabled-only"} {
			want := available && (id == "gpt-6.1-sol" || id == "team/gpt-6-astra")
			got := gjson.Get(payload, `codex-pro.#(id=="`+id+`").supports_ultrafast`)
			if got.Bool() != want || (!want && got.Exists()) {
				t.Errorf("%s supports_ultrafast = %s, want %v (omitted when false)", id, got.Raw, want)
			}
		}
		if got := gjson.Get(payload, `openai.0.supports_ultrafast`); got.Exists() {
			t.Errorf("non-Codex model carries supports_ultrafast: %s", got.Raw)
		}
	}
	assertAvailability(true)
	for _, tc := range []struct {
		name   string
		plan   string
		status coreauth.Status
	}{
		{"disabled status", "promax", coreauth.StatusDisabled},
		{"downgraded", "pro", coreauth.StatusActive},
	} {
		t.Run(tc.name, func(t *testing.T) {
			updated := proMax.Clone()
			updated.Attributes["plan_type"], updated.Status = tc.plan, tc.status
			if _, errUpdate := rt.CoreManager().Update(t.Context(), updated); errUpdate != nil {
				t.Fatal(errUpdate)
			}
			assertAvailability(false)
		})
	}
}

func TestBuildModelsJSONClaudeToolSearchCapability(t *testing.T) {
	rt := newGetTestRuntime(t)
	webSearch := true
	shared := &registry.ModelInfo{ID: "m-official", NativeCapabilities: &registry.NativeCapabilities{WebSearch: &webSearch}}
	register := func(id, provider string, attrs map[string]string, models ...*registry.ModelInfo) {
		t.Helper()
		auth := &coreauth.Auth{ID: id, Provider: provider, Status: coreauth.StatusActive, Attributes: attrs}
		if _, errRegister := rt.CoreManager().Register(context.Background(), auth); errRegister != nil {
			t.Fatalf("register %s: %v", id, errRegister)
		}
		registry.GetGlobalRegistry().RegisterClient(id, provider, models)
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	}
	register("ts-official", "claude", map[string]string{"api_key": "a"},
		shared, &registry.ModelInfo{ID: "m-mixed"}, &registry.ModelInfo{ID: "claude-opus-4-1-20250805"})
	register("ts-gateway", "claude", map[string]string{"api_key": "b", "base_url": "https://gateway.example.com"},
		&registry.ModelInfo{ID: "m-mixed"})
	register("ts-forced", "claude", map[string]string{"api_key": "c", "base_url": "https://gateway.example.com", "native_tool_search": "true"},
		&registry.ModelInfo{ID: "m-forced"})
	register("ts-codex", "codex", map[string]string{"api_key": "d", "plan_type": "pro"},
		&registry.ModelInfo{ID: "m-codex"})

	payload, errBuild := buildModelsJSON(dispatch.Env{Runtime: rt})
	if errBuild != nil {
		t.Fatalf("buildModelsJSON: %v", errBuild)
	}
	for model, want := range map[string]bool{
		"m-official":               true,
		"m-mixed":                  false,
		"m-forced":                 true,
		"claude-opus-4-1-20250805": false,
	} {
		capability := gjson.Get(payload, `claude.#(id=="`+model+`").native_capabilities.tool_search`)
		if !capability.Exists() || capability.Bool() != want {
			t.Errorf("%s tool_search = %s, want %v", model, capability.Raw, want)
		}
	}
	if got := gjson.Get(payload, `claude.#(id=="m-official").native_capabilities.web_search`); !got.Bool() {
		t.Errorf("m-official lost web_search: %s", gjson.Get(payload, `claude.#(id=="m-official")`).Raw)
	}
	if got := gjson.Get(payload, `codex-pro.#(id=="m-codex").native_capabilities.tool_search`); got.Exists() {
		t.Errorf("non-Claude route carries tool_search: %s", got.Raw)
	}
	if shared.NativeCapabilities.ToolSearch != nil {
		t.Fatal("buildModelsJSON mutated the registered model")
	}
	for _, info := range registry.GetGlobalRegistry().GetModelsForClient("ts-official") {
		if info.NativeCapabilities != nil && info.NativeCapabilities.ToolSearch != nil {
			t.Fatalf("registry model %s gained tool_search", info.ID)
		}
	}
}
