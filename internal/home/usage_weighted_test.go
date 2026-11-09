package home

import (
	"context"
	"fmt"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
)

func TestReusedUsageCompensatesWithoutRedispatch(t *testing.T) {
	for _, scenario := range []struct{ affinity, images bool }{{false, false}, {false, true}, {true, false}, {true, true}} {
		t.Run(fmt.Sprintf("affinity=%t/images=%t", scenario.affinity, scenario.images), func(t *testing.T) {
			affinity := scenario.affinity
			weighted := &coreauth.WeightedRoundRobinSelector{}
			var selector coreauth.Selector = weighted
			if affinity {
				wrapped := coreauth.NewSessionAffinitySelectorWithConfig(coreauth.SessionAffinityConfig{Fallback: weighted})
				t.Cleanup(wrapped.Stop)
				selector = wrapped
			}
			manager := coreauth.NewManager(nil, selector, nil)
			for _, auth := range []*coreauth.Auth{
				{ID: "runtime-usage-a", Index: "runtime-index-a", Provider: "codex", Attributes: map[string]string{"weight": "3"}},
				{ID: "runtime-usage-b", Index: "runtime-index-b", Provider: "codex"},
			} {
				registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "route-model"}})
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
				if _, errRegister := manager.Register(t.Context(), auth); errRegister != nil {
					t.Fatal(errRegister)
				}
			}
			runtime := &Runtime{coreManager: manager}
			manager.SetPluginScheduler(runtime)
			admissions := 0
			runtime.SetConcurrencyAdmitter(ConcurrencyAdmitterFunc(func(_ context.Context, req ConcurrencyAdmissionRequest) (ConcurrencyAdmissionResult, error) {
				admissions++
				return ConcurrencyAdmissionResult{CredentialID: req.CredentialID, Model: req.Model, Accounted: true}, nil
			}))
			if _, errDispatch := runtime.dispatchWithOptions(t.Context(), "route-model", coreauth.Options{Metadata: map[string]any{coreauth.PinnedAuthIDMetadataKey: "runtime-usage-b"}}, DispatchConcurrencyContext{}); errDispatch != nil {
				t.Fatal(errDispatch)
			}
			payload := func(turn int, additional bool) string {
				model := "upstream-model"
				if additional {
					model = "image-tool-model"
				}
				// Additional-model usage has its own execution ID in CPA, even
				// though the inbound request ID and scheduling route are shared.
				return fmt.Sprintf(`{"request_id":"shared-websocket-request","execution_id":"response-%d-additional-%t","cpa_node_id":"node-a","auth_index":"runtime-index-b","provider":"codex","model":%q,"home_selection":{"reused":%t,"additional":%t,"model":"route-model"}}`, turn, additional, model, turn > 0, additional)
			}
			recordsPerTurn := 1
			if scenario.images {
				recordsPerTurn = 2
			}
			for turn := range 4 {
				for record := range recordsPerTurn {
					// Exercise both image-first and primary-first delivery. Split
					// billing records must never add a scheduling charge.
					data := payload(turn, scenario.images && record == turn%2)
					if errUsage := runtime.RecordUsagePayload(t.Context(), data); errUsage != nil {
						t.Fatal(errUsage)
					}
					// Retrying the same primary or image usage must never charge
					// it again, including an image generated on the initial turn.
					for range 3 {
						if errUsage := runtime.ApplyUsagePayloadFeedback(t.Context(), data); errUsage != nil {
							t.Fatal(errUsage)
						}
					}
				}
			}
			if admissions != 1 {
				t.Fatalf("usage acquired concurrency: admissions=%d", admissions)
			}
			// Four executions count four times, with or without split image
			// billing: one dispatch plus three primary reuse usage records.
			for i := range 12 {
				decision, errDispatch := manager.Dispatch(t.Context(), []string{"codex"}, "route-model", coreauth.Options{})
				if errDispatch != nil || decision.Auth.ID != "runtime-usage-a" {
					t.Fatalf("repayment %d: decision=%v err=%v", i, decision, errDispatch)
				}
			}
			counts := map[string]int{}
			for range 4 {
				decision, errDispatch := manager.Dispatch(t.Context(), []string{"codex"}, "route-model", coreauth.Options{})
				if errDispatch != nil {
					t.Fatal(errDispatch)
				}
				counts[decision.Auth.ID]++
			}
			if counts["runtime-usage-a"] != 3 || counts["runtime-usage-b"] != 1 {
				t.Fatalf("usage was overcharged: %v", counts)
			}
		})
	}
}
