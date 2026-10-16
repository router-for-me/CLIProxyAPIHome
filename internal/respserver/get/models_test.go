package get

import (
	"context"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
	"github.com/router-for-me/CLIProxyAPIHome/internal/respserver/dispatch"
	"github.com/tidwall/gjson"
)

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

	payload, errBuild := buildModelsJSON(dispatch.Env{Runtime: rt}, nil)
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
