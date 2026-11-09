package dynamic

import (
	"encoding/json"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
	"github.com/tidwall/gjson"
)

func TestRESPDispatchGuardianCrossModelInheritance(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		name := "enabled"
		if !enabled {
			name = "disabled"
		}
		t.Run(name, func(t *testing.T) {
			runtime, _ := newConcurrencyDispatchRuntime(t, []string{"cred-a", "cred-b"})
			for _, id := range []string{"cred-a", "cred-b"} {
				registry.GetGlobalRegistry().RegisterClient(id, "openai", []*registry.ModelInfo{{ID: "gpt"}, {ID: "custom-review"}})
			}
			runtime.CoreManager().SetSelector(coreauth.NewSessionAffinitySelectorWithConfig(coreauth.SessionAffinityConfig{Fallback: &coreauth.FillFirstSelector{}, SubagentAffinity: &enabled}))
			env := protocolOneDispatchEnv(runtime, "guardian-dispatch")
			dispatch := func(session, parent, model, pinned string) string {
				t.Helper()
				request := map[string]any{
					"type": "auth", "model": model, "session_id": "codex:" + session, "concurrency_protocol": 1,
					"headers": map[string]string{"x-api-key": "dispatch-client-key", "x-openai-subagent": "guardian"},
				}
				if parent != "" {
					request["parent_session_id"] = "codex:" + parent
				}
				if pinned != "" {
					request["pinned_auth_id"] = pinned
				}
				body, errMarshal := json.Marshal(request)
				if errMarshal != nil {
					t.Fatal(errMarshal)
				}
				reply := handleAuth(t.Context(), env, []string{"RPOP", "auth", string(body)})
				id := gjson.GetBytes(reply.BulkString, "auth.id").String()
				if id == "" {
					t.Fatalf("dispatch failed: %s", reply.BulkString)
				}
				return id
			}
			dispatch("parent", "", "gpt", "cred-b")
			want := "cred-a"
			if enabled {
				want = "cred-b"
			}
			if got := dispatch("child", "parent", "custom-review", ""); got != want {
				t.Fatalf("Guardian selection=%s, want %s", got, want)
			}
			if got := dispatch("child", "", "custom-review", ""); got != want {
				t.Fatalf("Guardian child binding=%s, want %s", got, want)
			}
		})
	}
}
