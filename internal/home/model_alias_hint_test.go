package home

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	homeerrors "github.com/router-for-me/CLIProxyAPIHome/internal/errors"
	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
)

// newAliasedCodexRuntime publishes upstream gpt-6.1-sol only as "GPT-6.1 Sol",
// which is what importing from a catalog with display names used to configure.
func newAliasedCodexRuntime(t *testing.T) *Runtime {
	t.Helper()
	manager := coreauth.NewManager(nil, &coreauth.RoundRobinSelector{}, nil)
	auth := &coreauth.Auth{
		ID:         "codex-aliased",
		Provider:   "codex",
		Status:     coreauth.StatusActive,
		Attributes: map[string]string{"api_key": "sk-test"},
		Metadata: map[string]any{"home_config_models": []any{
			map[string]any{"id": "GPT-6.1 Sol", "name": "gpt-6.1-sol", "user_defined": true},
		}},
	}
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("Register error = %v", errRegister)
	}
	runtime := &Runtime{coreManager: manager}
	runtime.registerResolvedModelsForAuth(auth, auth.Provider, []*registry.ModelInfo{{ID: "GPT-6.1 Sol"}}, nil)
	manager.RefreshSchedulerEntry(auth.ID)
	return runtime
}

func TestWithModelAliasHintPreservesErrorDetailsAndPolicyFailures(t *testing.T) {
	runtime := newAliasedCodexRuntime(t)
	input := &coreauth.Error{
		Code: homeerrors.TypeAuthNotFound, Message: "no auth available", Diagnostic: "diagnostic",
		Retryable: true, HTTPStatus: 503,
	}
	got := runtime.withModelAliasHint(context.Background(), input, "gpt-6.1-sol", "key")
	annotated, ok := got.(*coreauth.Error)
	if !ok || annotated == input || annotated.Code != input.Code || annotated.Diagnostic != input.Diagnostic ||
		annotated.HTTPStatus != input.HTTPStatus || annotated.Retryable != input.Retryable {
		t.Fatalf("error details changed: %#v", got)
	}
	for _, adapter := range []ClusterAdapter{
		&modelChannelDispatchTestAdapter{},
		&modelAccessTestAdapter{},
		&modelAccessTestAdapter{err: errors.New("private policy failure")},
		&modelAccessTestAdapter{policy: coreauth.NewModelAccess([]string{}, nil, nil)},
	} {
		runtime.clusterAdapter = adapter
		if out := runtime.withModelAliasHint(context.Background(), input, "gpt-6.1-sol", "key"); out != input {
			t.Fatalf("missing or denied access must preserve the input error: %v", out)
		}
	}
	runtime.clusterAdapter = nil
	priceError := fmt.Errorf("%s: model gpt-6.1-sol has no configured price", homeerrors.TypeModelNotFound)
	out := runtime.withModelAliasHint(context.Background(), priceError, "gpt-6.1-sol", "key")
	if !errors.Is(out, priceError) || !strings.HasPrefix(out.Error(), priceError.Error()+"; ") || !strings.Contains(out.Error(), `"GPT-6.1 Sol"`) {
		t.Fatalf("plain lookup error was not preserved and annotated: %v", out)
	}
}

func newPublishedAliasHintRuntime(t *testing.T, cfg *config.Config, auth *coreauth.Auth) *Runtime {
	t.Helper()
	manager := coreauth.NewManager(nil, nil, nil)
	manager.SetConfig(cfg)
	manager.SetOAuthModelAlias(cfg.OAuthModelAlias)
	runtime := &Runtime{cfg: cfg, coreManager: manager}
	runtime.applyCoreAuthAddOrUpdate(context.Background(), auth)
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	return runtime
}

func TestDispatchAliasHintUsesPublishedPrefix(t *testing.T) {
	cfg := &config.Config{}
	cfg.ForceModelPrefix = true
	auth := &coreauth.Auth{
		ID: t.Name(), Provider: "claude", Prefix: "team", Status: coreauth.StatusActive,
		Attributes: map[string]string{"api_key": "test"},
		Metadata: map[string]any{homeConfigModelsMetadataKey: []any{
			map[string]any{"id": "friendly-218", "name": "upstream-218", "user_defined": true},
		}},
	}
	runtime := newPublishedAliasHintRuntime(t, cfg, auth)
	if result, errAlias := runtime.DispatchForAPIKey(context.Background(), "team/friendly-218", nil, ""); errAlias != nil || result == nil || result.Model != "upstream-218" {
		t.Fatalf("prefixed alias should route: result=%v error=%v", result, errAlias)
	}
	if result, errBare := runtime.DispatchForAPIKey(context.Background(), "friendly-218", nil, ""); result != nil || errBare == nil {
		t.Fatalf("bare alias must not route: result=%v error=%v", result, errBare)
	}
	for _, requested := range []string{"upstream-218", "team/upstream-218", "team/upstream-218(high)"} {
		_, errDispatch := runtime.DispatchForAPIKey(context.Background(), requested, nil, "")
		if errDispatch == nil || !strings.Contains(errDispatch.Error(), `under the alias "team/friendly-218"`) {
			t.Fatalf("expected published alias for %q: %v", requested, errDispatch)
		}
	}
	auth.Metadata[homeConfigModelsMetadataKey] = []any{
		map[string]any{"id": "renamed-218", "name": "upstream-218", "user_defined": true},
	}
	runtime.applyCoreAuthAddOrUpdate(context.Background(), auth)
	_, errRenamed := runtime.DispatchForAPIKey(context.Background(), "upstream-218", nil, "")
	if errRenamed == nil || !strings.Contains(errRenamed.Error(), `"team/renamed-218"`) || strings.Contains(errRenamed.Error(), `"team/friendly-218"`) {
		t.Fatalf("credential update left stale alias hints: %v", errRenamed)
	}
}

func TestDispatchAliasHintTracksOAuthConfig(t *testing.T) {
	cfg := &config.Config{OAuthModelAlias: map[string][]config.OAuthModelAlias{
		"claude": {{Name: "oauth-upstream-218", Alias: "oauth-friendly-218"}},
	}}
	auth := &coreauth.Auth{
		ID: t.Name(), Provider: "claude", Status: coreauth.StatusActive,
		Attributes: map[string]string{"auth_kind": "oauth"},
		Metadata: map[string]any{homeConfigModelsMetadataKey: []any{
			map[string]any{"id": "oauth-upstream-218"},
		}},
	}
	runtime := newPublishedAliasHintRuntime(t, cfg, auth)
	if result, errAlias := runtime.DispatchForAPIKey(context.Background(), "oauth-friendly-218", nil, ""); errAlias != nil || result == nil || result.Model != "oauth-upstream-218" {
		t.Fatalf("OAuth alias should route: result=%v error=%v", result, errAlias)
	}
	_, errDispatch := runtime.DispatchForAPIKey(context.Background(), "oauth-upstream-218", nil, "")
	if errDispatch == nil || !strings.Contains(errDispatch.Error(), `"oauth-friendly-218"`) {
		t.Fatalf("expected OAuth alias hint: %v", errDispatch)
	}
	cfg.OAuthModelAlias["claude"][0].Alias = "oauth-renamed-218"
	runtime.coreManager.SetOAuthModelAlias(cfg.OAuthModelAlias)
	runtime.applyCoreAuthAddOrUpdate(context.Background(), auth)
	_, errReloaded := runtime.DispatchForAPIKey(context.Background(), "oauth-upstream-218", nil, "")
	if errReloaded == nil || !strings.Contains(errReloaded.Error(), `"oauth-renamed-218"`) || strings.Contains(errReloaded.Error(), `"oauth-friendly-218"`) {
		t.Fatalf("OAuth config update left stale hints: %v", errReloaded)
	}
	cfg.OAuthModelAlias = nil
	runtime.coreManager.SetOAuthModelAlias(nil)
	runtime.applyCoreAuthAddOrUpdate(context.Background(), auth)
	if got := runtime.coreManager.AliasesForUpstreamModel("oauth-upstream-218", nil); len(got) != 0 {
		t.Fatalf("removed OAuth aliases remain indexed: %v", got)
	}
	if result, errOriginal := runtime.DispatchForAPIKey(context.Background(), "oauth-upstream-218", nil, ""); result == nil || errOriginal != nil {
		t.Fatalf("clearing the alias must restore the original route: result=%+v error=%v", result, errOriginal)
	}
}

func TestDispatchAliasHintOmitsDisabledAndExcludedModels(t *testing.T) {
	auth := &coreauth.Auth{
		ID: t.Name(), Provider: "claude", Status: coreauth.StatusActive,
		Attributes: map[string]string{"api_key": "test"},
		Metadata: map[string]any{homeConfigModelsMetadataKey: []any{
			map[string]any{"id": "friendly-218", "name": "upstream-218", "user_defined": true},
		}},
	}
	runtime := newPublishedAliasHintRuntime(t, &config.Config{}, auth)
	for _, change := range []string{"excluded", "disabled", "removed"} {
		t.Run(change, func(t *testing.T) {
			auth.Disabled = false
			delete(auth.Attributes, "excluded_models")
			runtime.applyCoreAuthAddOrUpdate(context.Background(), auth)
			if got := runtime.coreManager.AliasesForUpstreamModel("upstream-218", nil); len(got) != 1 {
				t.Fatalf("expected a published alias before %s: %v", change, got)
			}
			switch change {
			case "excluded":
				auth.Attributes["excluded_models"] = "friendly-218"
			case "disabled":
				auth.Disabled = true
			case "removed":
				runtime.applyCoreAuthRemove(context.Background(), auth.ID)
			}
			if change != "removed" {
				runtime.applyCoreAuthAddOrUpdate(context.Background(), auth)
			}
			if got := runtime.coreManager.AliasesForUpstreamModel("upstream-218", nil); len(got) != 0 {
				t.Fatalf("unpublished aliases remain: %v", got)
			}
		})
	}
}

func TestDispatchByUpstreamNameNamesTheAlias(t *testing.T) {
	runtime := newAliasedCodexRuntime(t)

	_, errDispatch := runtime.DispatchForAPIKey(context.Background(), "gpt-6.1-sol", nil, "")
	var authErr *coreauth.Error
	if !errors.As(errDispatch, &authErr) {
		t.Fatalf("dispatch error = %v, want *coreauth.Error", errDispatch)
	}
	if authErr.Code != homeerrors.TypeModelNotFound && authErr.Code != homeerrors.TypeAuthNotFound {
		t.Fatalf("code = %q, want model_not_found or auth_not_found", authErr.Code)
	}
	if !strings.Contains(authErr.Message, `is configured under the alias "GPT-6.1 Sol"`) {
		t.Fatalf("message = %q, want it to name the alias", authErr.Message)
	}

	// The alias itself still routes.
	decision, errAlias := runtime.DispatchForAPIKey(context.Background(), "GPT-6.1 Sol", nil, "")
	if errAlias != nil || decision == nil || decision.Auth == nil || decision.Auth.ID != "codex-aliased" {
		t.Fatalf("dispatch by alias = %+v, %v; want codex-aliased", decision, errAlias)
	}
}

func TestWithModelAliasHintLeavesOtherErrorsAlone(t *testing.T) {
	runtime := newAliasedCodexRuntime(t)

	authNotFound := &coreauth.Error{Code: homeerrors.TypeAuthNotFound, Message: "no auth available"}
	got := runtime.withModelAliasHint(context.Background(), authNotFound, "gpt-6.1-sol", "")
	var annotated *coreauth.Error
	if !errors.As(got, &annotated) || annotated.Code != homeerrors.TypeAuthNotFound ||
		annotated.Message != `no auth available; model "gpt-6.1-sol" is configured under the alias "GPT-6.1 Sol"; request that model ID or clear the alias` {
		t.Fatalf("annotated = %#v", got)
	}
	if authNotFound.Message != "no auth available" {
		t.Fatalf("original error mutated: %q", authNotFound.Message)
	}

	cases := map[string]error{
		"no alias for model": &coreauth.Error{Code: homeerrors.TypeAuthNotFound, Message: "no auth available"},
		"other code":         &coreauth.Error{Code: "auth_unavailable", Message: "no auth available"},
		"wrapped auth error": fmt.Errorf("wrapped: %w", &coreauth.Error{Code: homeerrors.TypeAuthNotFound, Message: "x"}),
		"non auth error":     errors.New("boom"),
	}
	models := map[string]string{"no alias for model": "gpt-other"}
	for name, input := range cases {
		model := models[name]
		if model == "" {
			model = "gpt-6.1-sol"
		}
		if out := runtime.withModelAliasHint(context.Background(), input, model, ""); out != input {
			t.Fatalf("%s: got %v, want the input error unchanged", name, out)
		}
	}
	if out := runtime.withModelAliasHint(context.Background(), nil, "gpt-6.1-sol", ""); out != nil {
		t.Fatalf("nil error = %v, want nil", out)
	}
}
