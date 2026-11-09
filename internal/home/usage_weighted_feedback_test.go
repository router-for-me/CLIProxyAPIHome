package home

import (
	"context"
	"errors"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
)

func newWeightedUsageFeedbackRuntime(t *testing.T) *Runtime {
	t.Helper()
	manager := coreauth.NewManager(nil, &coreauth.WeightedRoundRobinSelector{}, nil)
	for _, id := range []string{"feedback-usage-a", "feedback-usage-b"} {
		registry.GetGlobalRegistry().RegisterClient(id, "codex", []*registry.ModelInfo{{ID: "route"}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
		if _, errRegister := manager.Register(t.Context(), &coreauth.Auth{ID: id, Index: id, Provider: "codex", Status: coreauth.StatusActive}); errRegister != nil {
			t.Fatal(errRegister)
		}
	}
	return &Runtime{coreManager: manager}
}

const reusedFeedbackUsage = `{"execution_id":"unique-response","auth_index":"feedback-usage-b","provider":"codex","api_key":"client-key","model":"route","home_selection":{"reused":true,"model":"route"}}`

func TestReusedUsageIgnoresLegacyAndUnauthorizedMetadata(t *testing.T) {
	for _, mode := range []string{"legacy", "first", "missing-id", "invalid-policy", "denied-auth", "denied-model"} {
		t.Run(mode, func(t *testing.T) {
			runtime := newWeightedUsageFeedbackRuntime(t)
			doc := ojson.NewDocString(reusedFeedbackUsage)
			switch mode {
			case "legacy":
				_ = doc.Delete("home_selection")
			case "first":
				_ = doc.Set("home_selection.reused", false)
			case "missing-id":
				_ = doc.Delete("execution_id")
			case "invalid-policy":
				_ = doc.Set("home_selection.credential_policy", "unknown-policy")
			case "denied-auth":
				runtime.clusterAdapter = &weightedUsagePolicyErrorAdapter{modelChannelDispatchTestAdapter: modelChannelDispatchTestAdapter{allowedAuthIDs: []string{"feedback-usage-a"}, allowedModelIDs: []string{"route"}}}
			case "denied-model":
				runtime.clusterAdapter = &weightedUsagePolicyErrorAdapter{modelChannelDispatchTestAdapter: modelChannelDispatchTestAdapter{allowedModelIDs: []string{}}}
			}
			if errUsage := runtime.RecordUsagePayload(t.Context(), doc.String()); errUsage != nil {
				t.Fatal(errUsage)
			}
			for _, want := range []string{"feedback-usage-a", "feedback-usage-b"} {
				decision, errDispatch := runtime.coreManager.Dispatch(t.Context(), []string{"codex"}, "route", coreauth.Options{})
				if errDispatch != nil || decision.Auth.ID != want {
					t.Fatalf("ignored usage changed credit: decision=%v err=%v want=%s", decision, errDispatch, want)
				}
			}
		})
	}
}

type weightedUsagePolicyErrorAdapter struct {
	modelChannelDispatchTestAdapter
	err   error
	calls int
}

func (a *weightedUsagePolicyErrorAdapter) AllowedUsageIDsForAPIKeyModel(_ context.Context, _ string, modelID string) ([]string, []string, error) {
	a.calls++
	a.requestedModel = modelID
	return a.allowedAuthIDs, a.allowedModelIDs, a.err
}

func TestReusedUsageRetriesAfterPersistenceBeforeAccountingFailure(t *testing.T) {
	runtime := newWeightedUsageFeedbackRuntime(t)
	failure := errors.New("policy store unavailable")
	adapter := &weightedUsagePolicyErrorAdapter{err: failure}
	runtime.clusterAdapter = adapter
	if errUsage := runtime.RecordUsagePayload(t.Context(), reusedFeedbackUsage); !errors.Is(errUsage, failure) {
		t.Fatalf("feedback error=%v", errUsage)
	}
	adapter.err = nil
	for range 3 {
		if errUsage := runtime.ApplyUsagePayloadFeedback(t.Context(), reusedFeedbackUsage); errUsage != nil {
			t.Fatal(errUsage)
		}
	}
	if adapter.calls != 2 {
		t.Fatalf("permission lookups=%d, want 1 failed + 1 retry (replays must short-circuit in memory)", adapter.calls)
	}
	// One charge to b is repaid by one a, then ordinary a/b resumes. Neither
	// the failed first attempt nor the repeated successful feedback may charge.
	for _, want := range []string{"feedback-usage-a", "feedback-usage-a", "feedback-usage-b"} {
		decision, errDispatch := runtime.coreManager.Dispatch(t.Context(), []string{"codex"}, "route", coreauth.Options{})
		if errDispatch != nil || decision.Auth.ID != want {
			t.Fatalf("retry lost or duplicated accounting: decision=%v err=%v want=%s", decision, errDispatch, want)
		}
	}
}

type weightedUsageTestPluginScheduler struct{}

func (weightedUsageTestPluginScheduler) PickAuth(context.Context, pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, bool, error) {
	return pluginapi.SchedulerPickResponse{}, false, nil
}

func TestReusedUsageSkipsPreparationWithoutWeightedRouting(t *testing.T) {
	for _, mode := range []string{"round-robin", "fill-first", "affinity-round-robin", "plugin"} {
		t.Run(mode, func(t *testing.T) {
			runtime := newWeightedUsageFeedbackRuntime(t)
			switch mode {
			case "round-robin":
				runtime.coreManager.SetSelector(&coreauth.RoundRobinSelector{})
			case "fill-first":
				runtime.coreManager.SetSelector(&coreauth.FillFirstSelector{})
			case "affinity-round-robin":
				selector := coreauth.NewSessionAffinitySelectorWithConfig(coreauth.SessionAffinityConfig{Fallback: &coreauth.RoundRobinSelector{}})
				t.Cleanup(selector.Stop)
				runtime.coreManager.SetSelector(selector)
			case "plugin":
				runtime.coreManager.SetPluginScheduler(weightedUsageTestPluginScheduler{})
			}
			adapter := &weightedUsagePolicyErrorAdapter{err: errors.New("unexpected policy lookup")}
			runtime.clusterAdapter = adapter
			ctx := t.Context()
			// Parsing the payload, querying permissions, or cloning the nonempty
			// credential pool would allocate. Non-weighted accounting does none.
			if allocs := testing.AllocsPerRun(10, func() {
				if errAccount := runtime.accountReusedUsage(ctx, reusedFeedbackUsage); errAccount != nil {
					t.Fatal(errAccount)
				}
			}); allocs != 0 {
				t.Fatalf("non-weighted accounting allocated %v times", allocs)
			}
			if errUsage := runtime.RecordUsagePayload(ctx, reusedFeedbackUsage); errUsage != nil {
				t.Fatal(errUsage)
			}
			if errFeedback := runtime.ApplyUsagePayloadFeedback(ctx, reusedFeedbackUsage); errFeedback != nil {
				t.Fatal(errFeedback)
			}
			auth, ok := runtime.coreManager.GetByID("feedback-usage-b")
			if !ok || auth.Success != 1 || adapter.calls != 0 {
				t.Fatalf("ordinary feedback skipped or policy queried: auth=%+v calls=%d", auth, adapter.calls)
			}
		})
	}
}

func TestReusedUsageSkipsPermissionLookupWithoutCompetingPool(t *testing.T) {
	for _, mode := range []string{"single-credential", "different-priority", "unloaded-auth", "disabled-auth", "zero-weight-auth"} {
		t.Run(mode, func(t *testing.T) {
			manager := coreauth.NewManager(nil, &coreauth.WeightedRoundRobinSelector{}, nil)
			authB := &coreauth.Auth{ID: "feedback-usage-b", Index: "feedback-usage-b", Provider: "codex", Status: coreauth.StatusActive}
			switch mode {
			case "different-priority":
				peer := &coreauth.Auth{ID: "feedback-usage-a", Index: "feedback-usage-a", Provider: "codex", Status: coreauth.StatusActive, Attributes: map[string]string{"priority": "10"}}
				registry.GetGlobalRegistry().RegisterClient(peer.ID, "codex", []*registry.ModelInfo{{ID: "route"}})
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(peer.ID) })
				if _, errRegister := manager.Register(t.Context(), peer); errRegister != nil {
					t.Fatal(errRegister)
				}
			case "disabled-auth":
				authB.Disabled = true
			case "zero-weight-auth":
				authB.Attributes = map[string]string{"weight": "0"}
			}
			if mode != "unloaded-auth" {
				registry.GetGlobalRegistry().RegisterClient(authB.ID, "codex", []*registry.ModelInfo{{ID: "route"}})
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(authB.ID) })
				if _, errRegister := manager.Register(t.Context(), authB); errRegister != nil {
					t.Fatal(errRegister)
				}
			}
			adapter := &weightedUsagePolicyErrorAdapter{err: errors.New("unexpected permission query")}
			runtime := &Runtime{coreManager: manager, clusterAdapter: adapter}
			if errAccount := runtime.accountReusedUsage(t.Context(), reusedFeedbackUsage); errAccount != nil {
				t.Fatal(errAccount)
			}
			if adapter.calls != 0 {
				t.Fatalf("permission lookups=%d, want 0", adapter.calls)
			}
		})
	}
}

func TestReusedUsageRequiresUsagePermissionStore(t *testing.T) {
	runtime := newWeightedUsageFeedbackRuntime(t)
	runtime.clusterAdapter = &modelChannelDispatchTestAdapter{}
	if errUsage := runtime.RecordUsagePayload(t.Context(), reusedFeedbackUsage); errUsage == nil {
		t.Fatal("missing usage permission store must fail closed")
	}
}

func TestReusedUsageChargesFailedResponsesBeforeCooldown(t *testing.T) {
	runtime := newWeightedUsageFeedbackRuntime(t)
	doc := ojson.NewDocString(reusedFeedbackUsage)
	_ = doc.Set("failed", true)
	_ = doc.Set("fail.status_code", 429)
	if errUsage := runtime.RecordUsagePayload(t.Context(), doc.String()); errUsage != nil {
		t.Fatal(errUsage)
	}
	// Recover availability without the reuse marker. The failure's charge must
	// survive both its cooldown feedback and this ordinary success feedback.
	if errUsage := runtime.RecordUsagePayload(t.Context(), `{"auth_index":"feedback-usage-b","provider":"codex","model":"route"}`); errUsage != nil {
		t.Fatal(errUsage)
	}
	for _, want := range []string{"feedback-usage-a", "feedback-usage-a", "feedback-usage-b"} {
		decision, errDispatch := runtime.coreManager.Dispatch(t.Context(), []string{"codex"}, "route", coreauth.Options{})
		if errDispatch != nil || decision.Auth.ID != want {
			t.Fatalf("failed response accounting: decision=%v err=%v want=%s", decision, errDispatch, want)
		}
	}
}
