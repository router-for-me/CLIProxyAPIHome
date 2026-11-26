package dynamic

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestHandleAuthMalformedExecutionDoesNotAdmit(t *testing.T) {
	for _, options := range []any{"invalid-object", map[string]any{"connection-pool": map[string]any{"enabled": "invalid"}}} {
		runtime, admitter := newConcurrencyDispatchRuntime(t, []string{"malformed-credential"})
		auth, found := runtime.CoreManager().GetByID("malformed-credential")
		if !found {
			t.Fatal("missing test credential")
		}
		auth.Metadata = map[string]any{"credential_options": options}
		if _, errUpdate := runtime.CoreManager().Update(t.Context(), auth); errUpdate != nil {
			t.Fatal(errUpdate)
		}
		reply := handleAuth(t.Context(), protocolOneDispatchEnv(runtime, "malformed-fingerprint"), []string{"RPOP", "auth", `{"type":"auth","model":"gpt","concurrency_protocol":1,"headers":{"x-api-key":"dispatch-client-key"}}`})
		if admitter.Calls() != 0 || reply.AccountedAdmission {
			t.Fatal("invalid execution options consumed an admission slot")
		}
		if !gjson.Get(string(reply.BulkString), "error").Exists() {
			t.Fatalf("invalid execution options returned success: %s", reply.BulkString)
		}
	}
}
