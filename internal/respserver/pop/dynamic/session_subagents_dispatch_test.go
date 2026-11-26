package dynamic

import (
	"encoding/json"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/tidwall/gjson"
)

func TestRESPDispatchSubagentAffinity(t *testing.T) {
	enabled, disabled := true, false
	for _, setting := range []struct {
		name  string
		value *bool
	}{{"default", nil}, {"enabled", &enabled}, {"disabled", &disabled}} {
		for _, kind := range []string{"child", "guardian", "fork", "compaction", "header-fork"} {
			t.Run(setting.name+"/"+kind, func(t *testing.T) {
				rt, _ := newConcurrencyDispatchRuntime(t, []string{"cred-a", "cred-b"})
				rt.CoreManager().SetSelector(coreauth.NewSessionAffinitySelectorWithConfig(coreauth.SessionAffinityConfig{Fallback: &coreauth.FillFirstSelector{}, SubagentAffinity: setting.value}))
				env := protocolOneDispatchEnv(rt, "subagents-test")
				dispatch := func(request map[string]any) string {
					t.Helper()
					request["type"] = "auth"
					request["model"] = "gpt"
					request["concurrency_protocol"] = 1
					if request["headers"] == nil {
						request["headers"] = map[string]string{"x-api-key": "dispatch-client-key"}
					}
					body, errMarshal := json.Marshal(request)
					if errMarshal != nil {
						t.Fatal(errMarshal)
					}
					reply := handleAuth(t.Context(), env, []string{"RPOP", "auth", string(body)})
					authID := gjson.GetBytes(reply.BulkString, "auth.id").String()
					if authID == "" {
						t.Fatalf("dispatch error: %s", reply.BulkString)
					}
					return authID
				}
				if got := dispatch(map[string]any{"session_id": "slot:parent", "pinned_auth_id": "cred-b"}); got != "cred-b" {
					t.Fatalf("parent=%s", got)
				}
				request := map[string]any{"session_id": "slot:child", "parent_session_id": "slot:parent"}
				switch kind {
				case "fork", "compaction":
					request["node_kind"] = kind
				case "header-fork":
					request["headers"] = map[string]string{"x-api-key": "dispatch-client-key", "x-node-kind": "fork"}
				case "guardian":
					request["headers"] = map[string]string{"x-api-key": "dispatch-client-key", "x-openai-subagent": "guardian"}
				}
				want := "cred-b"
				if setting.value != nil && !*setting.value && (kind == "child" || kind == "guardian") {
					want = "cred-a"
				}
				if got := dispatch(request); got != want {
					t.Fatalf("child=%s, want %s", got, want)
				}
				if got := dispatch(map[string]any{"session_id": "slot:child"}); got != want {
					t.Fatalf("child binding=%s, want %s", got, want)
				}
				if got := dispatch(map[string]any{"session_id": "slot:parent"}); got != "cred-b" {
					t.Fatalf("parent binding changed: %s", got)
				}
			})
		}
	}
}
