package dynamic

import (
	"context"
	"strings"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/home"
	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
	"github.com/router-for-me/CLIProxyAPIHome/internal/respserver/dispatch"
	"github.com/tidwall/gjson"
)

func TestDispatchCredentialPolicyJSONParsing(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		want    string
		wantErr bool
	}{
		{name: "missing", payload: `{"model":"gpt"}`},
		{name: "empty", payload: `{"model":"gpt","credential_policy":""}`},
		{name: "supported", payload: `{"model":"gpt","credential_policy":"codex_alpha_search_v1"}`, want: coreauth.CredentialPolicyCodexAlphaSearchV1},
		{name: "trimmed", payload: `{"model":"gpt","credential_policy":" codex_alpha_search_v1 "}`, want: coreauth.CredentialPolicyCodexAlphaSearchV1},
		{name: "unknown", payload: `{"model":"gpt","credential_policy":"future_policy"}`, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, errPolicy := dispatchCredentialPolicy(test.payload)
			if (errPolicy != nil) != test.wantErr {
				t.Fatalf("dispatchCredentialPolicy() error = %v, wantErr %t", errPolicy, test.wantErr)
			}
			if got != test.want {
				t.Fatalf("dispatchCredentialPolicy() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestHandleAuthAppliesCredentialPolicyFromRESPJSON(t *testing.T) {
	runtime := newAuthValidateRuntime(t, context.Background(), "dispatch-client-key", "deleted-dispatch-client-key")
	runtime.CoreManager().SetFullAuthResolver(nil)
	runtime.CoreManager().SetSelector(&coreauth.RoundRobinSelector{})
	auths := []*coreauth.Auth{
		{ID: "ordinary-resp", Provider: "codex", Status: coreauth.StatusActive, Attributes: map[string]string{"api_key": "ordinary"}},
		{ID: "oauth-resp", Provider: "codex", Status: coreauth.StatusActive, Attributes: map[string]string{"auth_kind": "oauth"}},
	}
	for _, auth := range auths {
		registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "gpt"}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
		if _, errRegister := runtime.CoreManager().Register(context.Background(), auth); errRegister != nil {
			t.Fatalf("Register(%s) error = %v", auth.ID, errRegister)
		}
	}

	reply := handleAuth(context.Background(), dispatch.Env{Runtime: runtime}, []string{"RPOP", `{"type":"auth","model":"gpt","credential_policy":"codex_alpha_search_v1","headers":{"x-api-key":"dispatch-client-key"}}`})
	payload := string(reply.BulkString)
	if got := gjson.Get(payload, "auth.id").String(); got != "oauth-resp" {
		t.Fatalf("selected auth = %q, body=%s", got, payload)
	}
	if gjson.Get(payload, "applied_credential_policy").Exists() {
		t.Fatalf("response unexpectedly includes applied_credential_policy: %s", payload)
	}
}

func TestHandleAuthRejectsUnknownCredentialPolicy(t *testing.T) {
	reply := handleAuth(context.Background(), dispatch.Env{Runtime: &home.Runtime{}}, []string{"RPOP", `{"type":"auth","model":"gpt","credential_policy":"future_policy"}`})
	if reply.Kind != dispatch.ReplyKindBulkString {
		t.Fatalf("reply kind = %v, want bulk string", reply.Kind)
	}
	payload := string(reply.BulkString)
	if got := gjson.Get(payload, "error.type").String(); got != "unsupported_credential_policy" {
		t.Fatalf("error.type = %q, body=%s", got, payload)
	}
	if message := gjson.Get(payload, "error.message").String(); !strings.Contains(message, `future_policy`) {
		t.Fatalf("error.message = %q, want policy name", message)
	}
}
