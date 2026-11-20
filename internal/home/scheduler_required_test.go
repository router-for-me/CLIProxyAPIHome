package home

import (
	"context"
	"errors"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	sdkpluginhost "github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginhost"
	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
	"gopkg.in/yaml.v3"
)

// vetoSchedulerHost behaves like the plugin host around a veto-only scheduler: it refuses a pick
// whose candidates leave the allowed set and otherwise lets the built-in selector choose. Once
// fused it reports no scheduler and every pick unhandled, as the host does after a panic.
type vetoSchedulerHost struct {
	allowed      map[string]bool
	active       bool
	fuseOnNext   bool
	picksHandled int
}

func (h *vetoSchedulerHost) HasScheduler() bool { return h.active }

func (h *vetoSchedulerHost) PickAuth(_ context.Context, req pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, bool, error) {
	if !h.active {
		return pluginapi.SchedulerPickResponse{}, false, nil
	}
	if h.fuseOnNext {
		h.active = false
		return pluginapi.SchedulerPickResponse{}, false, nil
	}
	h.picksHandled++
	for _, candidate := range req.Candidates {
		if !h.allowed[candidate.ID] {
			return pluginapi.SchedulerPickResponse{}, true, errors.New("native_policy_denied")
		}
	}
	return pluginapi.SchedulerPickResponse{Handled: false}, true, nil
}

func schedulerTestConfig(t *testing.T, required bool) *config.Config {
	t.Helper()
	text := "plugins:\n  enabled: true\n  configs:\n    policy:\n      enabled: true\n      load-in-home: true\n"
	if required {
		text += "      scheduler-required: true\n"
	}
	cfg := &config.Config{}
	if errParse := yaml.Unmarshal([]byte(text), cfg); errParse != nil {
		t.Fatal(errParse)
	}
	return cfg
}

func schedulerTestRuntime(t *testing.T, required bool, host *vetoSchedulerHost, ids ...string) (*Runtime, *coreauth.Manager) {
	t.Helper()
	manager := coreauth.NewManager(nil, &coreauth.FillFirstSelector{}, nil)
	for _, id := range ids {
		auth := &coreauth.Auth{ID: id, Provider: "gemini", Status: coreauth.StatusActive, Attributes: map[string]string{"api_key": id + "-key"}}
		registry.GetGlobalRegistry().RegisterClient(id, "gemini", []*registry.ModelInfo{{ID: "scheduler-test-model"}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
		if _, errRegister := manager.Register(t.Context(), auth); errRegister != nil {
			t.Fatal(errRegister)
		}
	}
	runtime := &Runtime{cfg: schedulerTestConfig(t, required), coreManager: manager}
	if host != nil {
		runtime.schedulerHost = host
	}
	manager.SetPluginScheduler(runtime)
	return runtime, manager
}

func dispatchSchedulerTest(t *testing.T, manager *coreauth.Manager) (*coreauth.Auth, error) {
	t.Helper()
	decision, errDispatch := manager.Dispatch(t.Context(), []string{"gemini"}, "scheduler-test-model", coreauth.Options{})
	if errDispatch != nil {
		return nil, errDispatch
	}
	if decision == nil {
		return nil, nil
	}
	return decision.Auth, nil
}

func requireSchedulerUnavailable(t *testing.T, auth *coreauth.Auth, errDispatch error) {
	t.Helper()
	var authErr *coreauth.Error
	if auth != nil || !errors.As(errDispatch, &authErr) || authErr.Code != "scheduler_unavailable" {
		t.Fatalf("dispatch = %v, %v; want the closed scheduler_unavailable refusal", auth, errDispatch)
	}
}

func TestSchedulerRequiredRefusesPicksOutsidePolicyAfterAFuse(t *testing.T) {
	host := &vetoSchedulerHost{allowed: map[string]bool{"policy-allowed": true}, active: true}
	_, manager := schedulerTestRuntime(t, true, host, "policy-allowed", "policy-outside")

	if auth, errDispatch := dispatchSchedulerTest(t, manager); auth != nil || errDispatch == nil {
		t.Fatalf("active veto dispatch = %v, %v; want the policy denial", auth, errDispatch)
	}

	host.active = false // fused: no scheduler, every later host pick unhandled
	auth, errDispatch := dispatchSchedulerTest(t, manager)
	requireSchedulerUnavailable(t, auth, errDispatch)
}

func TestSchedulerRequiredRefusesWhenTheHostFusesDuringThePick(t *testing.T) {
	host := &vetoSchedulerHost{allowed: map[string]bool{"policy-allowed": true}, active: true, fuseOnNext: true}
	_, manager := schedulerTestRuntime(t, true, host, "policy-allowed", "policy-outside")

	auth, errDispatch := dispatchSchedulerTest(t, manager)
	requireSchedulerUnavailable(t, auth, errDispatch)
}

func TestSchedulerRequiredRefusesWhenTheSchedulerNeverLoaded(t *testing.T) {
	_, manager := schedulerTestRuntime(t, true, &vetoSchedulerHost{}, "policy-allowed")

	auth, errDispatch := dispatchSchedulerTest(t, manager)
	requireSchedulerUnavailable(t, auth, errDispatch)
}

func TestSchedulerRequiredKeepsTheVetoedPathUnchanged(t *testing.T) {
	host := &vetoSchedulerHost{allowed: map[string]bool{"policy-allowed": true}, active: true}
	_, manager := schedulerTestRuntime(t, true, host, "policy-allowed")

	auth, errDispatch := dispatchSchedulerTest(t, manager)
	if errDispatch != nil || auth == nil || auth.ID != "policy-allowed" || host.picksHandled != 1 {
		t.Fatalf("dispatch = %v, %v after %d vetoed picks; want policy-allowed through one veto", auth, errDispatch, host.picksHandled)
	}
}

func TestSchedulerNotRequiredKeepsTheBuiltInFallback(t *testing.T) {
	_, manager := schedulerTestRuntime(t, false, &vetoSchedulerHost{}, "fallback-a")

	auth, errDispatch := dispatchSchedulerTest(t, manager)
	if errDispatch != nil || auth == nil || auth.ID != "fallback-a" {
		t.Fatalf("dispatch = %v, %v; want the built-in selector to pick fallback-a", auth, errDispatch)
	}
}

func TestSchedulerRequiredRefusesThroughTheRealHostWithNoSchedulerLoaded(t *testing.T) {
	_, manager := schedulerTestRuntime(t, true, nil, "policy-allowed")
	runtime := &Runtime{cfg: schedulerTestConfig(t, true), coreManager: manager, pluginHost: sdkpluginhost.New()}
	manager.SetPluginScheduler(runtime)

	auth, errDispatch := dispatchSchedulerTest(t, manager)
	requireSchedulerUnavailable(t, auth, errDispatch)
}

func TestSchedulerRequiredRefusesWithoutAPluginHost(t *testing.T) {
	_, manager := schedulerTestRuntime(t, true, nil, "policy-allowed")
	runtime := &Runtime{cfg: schedulerTestConfig(t, true), coreManager: manager}
	manager.SetPluginScheduler(runtime)

	auth, errDispatch := dispatchSchedulerTest(t, manager)
	requireSchedulerUnavailable(t, auth, errDispatch)
}
