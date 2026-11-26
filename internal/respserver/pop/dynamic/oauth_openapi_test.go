package dynamic

import (
	"encoding/json"
	"fmt"
	"testing"

	cpaconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
	"github.com/tidwall/gjson"
)

func TestHandleAuthOAuthVertexOpenAPI(t *testing.T) {
	runtime, admitter := newConcurrencyDispatchRuntime(t, nil)
	t.Cleanup(runtime.Stop)
	enabled, disabled := true, false
	cfg := runtime.Config()
	cfg.OAuthModelAlias = map[string][]config.OAuthModelAlias{"vertex": {
		{Name: "gemini-2.5-flash", Alias: "force-openapi", OpenAPI: &enabled},
		{Name: "gemini-2.5-flash", Alias: "force-native", OpenAPI: &disabled},
		{Name: "gemini-2.5-flash", Alias: "automatic"},
	}}
	runtime.CoreManager().SetOAuthModelAlias(cfg.OAuthModelAlias)
	auth := &coreauth.Auth{ID: "vertex-oauth-dispatch", Index: "vertex-oauth-dispatch", Provider: "vertex", Prefix: "team", Status: coreauth.StatusActive, Metadata: map[string]any{"access_token": "fixture", "refresh_token": "private"}}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, "vertex", []*registry.ModelInfo{{ID: "team/force-openapi"}, {ID: "team/force-native"}, {ID: "team/automatic"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	if _, errRegister := runtime.CoreManager().Register(t.Context(), auth); errRegister != nil {
		t.Fatal(errRegister)
	}
	for _, model := range []string{"team/force-openapi", "team/force-native(high)", "team/automatic"} {
		request := fmt.Sprintf(`{"type":"auth","model":%q,"concurrency_protocol":1,"headers":{"x-api-key":"dispatch-client-key"}}`, model)
		reply := handleAuth(t.Context(), protocolOneDispatchEnv(runtime, "vertex-oauth-"+model), []string{"RPOP", "auth", request})
		if gjson.GetBytes(reply.BulkString, "error").Exists() || !reply.AccountedAdmission {
			t.Fatalf("dispatch failed: %s", reply.BulkString)
		}
		wantModel := "gemini-2.5-flash"
		if model == "team/force-native(high)" {
			wantModel += "(high)"
		}
		if got := gjson.GetBytes(reply.BulkString, "model").String(); got != wantModel {
			t.Fatalf("Home did not resolve alias %s: model=%s", model, got)
		}
		var rules []cpaconfig.VertexCompatModel
		if errDecode := json.Unmarshal([]byte(gjson.GetBytes(reply.BulkString, "auth.attributes.vertex_model_openapi").String()), &rules); errDecode != nil {
			t.Fatal(errDecode)
		}
		if len(rules) != 2 || rules[0].OpenAPI == nil || !*rules[0].OpenAPI || rules[1].OpenAPI == nil || *rules[1].OpenAPI {
			t.Fatalf("dispatch lost execution flags: %+v", rules)
		}
		if gjson.GetBytes(reply.BulkString, "auth.metadata.refresh_token").Exists() {
			t.Fatal("dispatch exposed the refresh token")
		}
	}
	stored, found := runtime.CoreManager().GetByID(auth.ID)
	if !found || stored.Attributes["vertex_model_openapi"] != "" {
		t.Fatal("dispatch wrote derived rules into the stored credential")
	}
	stored.Attributes = map[string]string{"vertex_model_openapi": "invalid-json"}
	if _, errUpdate := runtime.CoreManager().Update(t.Context(), stored); errUpdate != nil {
		t.Fatal(errUpdate)
	}
	calls := admitter.Calls()
	reply := handleAuth(t.Context(), protocolOneDispatchEnv(runtime, "vertex-invalid"), []string{"RPOP", "auth", `{"type":"auth","model":"team/force-openapi","concurrency_protocol":1,"headers":{"x-api-key":"dispatch-client-key"}}`})
	if admitter.Calls() != calls || reply.AccountedAdmission || !gjson.GetBytes(reply.BulkString, "error").Exists() {
		t.Fatal("invalid OAuth routing consumed a concurrency admission")
	}
}
