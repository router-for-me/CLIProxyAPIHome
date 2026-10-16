package home

import (
	"context"
	"errors"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
)

type pluginDiscoveryTestAdapter struct {
	enabled bool
	full    *coreauth.Auth
	err     error
	asked   string
}

func (a *pluginDiscoveryTestAdapter) Enabled() bool                       { return a.enabled }
func (a *pluginDiscoveryTestAdapter) LoadAuthIndex(context.Context) error { return nil }
func (a *pluginDiscoveryTestAdapter) ListMinimalAuths() []*coreauth.Auth  { return nil }
func (a *pluginDiscoveryTestAdapter) LoadConfigYAML(context.Context) ([]byte, error) {
	return nil, errors.New("not implemented")
}
func (a *pluginDiscoveryTestAdapter) GetFullAuth(_ context.Context, uuid string) (*coreauth.Auth, error) {
	a.asked = uuid
	if a.err != nil {
		return nil, a.err
	}
	if a.full == nil {
		return nil, nil
	}
	return a.full.Clone(), nil
}

func minimalPluginAuth() *coreauth.Auth {
	return &coreauth.Auth{ID: "e3b5f1d8-fe50-54e5-a0e6-a1526863d84e", Provider: "copilot", Prefix: "copilot"}
}

func TestPluginDiscoveryAuthLoadsFullAuthInClusterMode(t *testing.T) {
	adapter := &pluginDiscoveryTestAdapter{enabled: true, full: &coreauth.Auth{
		ID:       "e3b5f1d8-fe50-54e5-a0e6-a1526863d84e",
		Provider: "copilot",
		Metadata: map[string]any{"type": "copilot", "github_access_token": "gho_fake_test_token"},
	}}
	r := &Runtime{clusterAdapter: adapter}
	minimal := minimalPluginAuth()

	got := r.pluginDiscoveryAuth(context.Background(), minimal)
	if adapter.asked != minimal.ID {
		t.Fatalf("GetFullAuth asked for %q, want %q", adapter.asked, minimal.ID)
	}
	if got == minimal || got.Metadata["github_access_token"] != "gho_fake_test_token" {
		t.Fatalf("expected the full auth with provider credentials, got metadata %v", got.Metadata)
	}
	if got.ID != minimal.ID {
		t.Fatalf("ID = %q, want %q", got.ID, minimal.ID)
	}
	if homeAuthToPluginAuth(got).Metadata["github_access_token"] != "gho_fake_test_token" {
		t.Fatal("plugin auth lost the provider credentials")
	}
}

func TestPluginDiscoveryAuthFallsBackToRuntimeAuth(t *testing.T) {
	cases := map[string]*Runtime{
		"no adapter":       {},
		"adapter disabled": {clusterAdapter: &pluginDiscoveryTestAdapter{enabled: false, full: &coreauth.Auth{ID: "x"}}},
		"lookup error":     {clusterAdapter: &pluginDiscoveryTestAdapter{enabled: true, err: errors.New("db down")}},
		"not found":        {clusterAdapter: &pluginDiscoveryTestAdapter{enabled: true}},
	}
	for name, r := range cases {
		minimal := minimalPluginAuth()
		if got := r.pluginDiscoveryAuth(context.Background(), minimal); got != minimal {
			t.Fatalf("%s: expected the runtime auth to be returned unchanged", name)
		}
	}
	var nilRuntime *Runtime
	if nilRuntime.pluginDiscoveryAuth(context.Background(), nil) != nil {
		t.Fatal("nil runtime and nil auth should return nil")
	}
}
