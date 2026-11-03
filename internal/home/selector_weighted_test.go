package home

import (
	"context"
	"testing"

	cpaauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cpaexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
)

func TestV8WeightedStrategyMatchesCPA(t *testing.T) {
	cfg := &config.Config{}
	cfg.Routing.Strategy = "weighted-round-robin"
	selected := selectorFromConfig(cfg, nil)
	homeAuths := []*coreauth.Auth{{ID: "a-zero", Provider: "codex", Status: coreauth.StatusActive, Attributes: map[string]string{"weight": "0"}}, {ID: "b-positive", Provider: "codex", Status: coreauth.StatusActive, Attributes: map[string]string{"weight": "1"}}}
	cpaAuths := []*cpaauth.Auth{{ID: "a-zero", Provider: "codex", Status: cpaauth.StatusActive, Attributes: map[string]string{"weight": "0"}}, {ID: "b-positive", Provider: "codex", Status: cpaauth.StatusActive, Attributes: map[string]string{"weight": "1"}}}
	cpaSelector := &cpaauth.WeightedRoundRobinSelector{}
	homeZero, cpaZero := 0, 0
	for i := 0; i < 6; i++ {
		a, err := selected.Pick(context.Background(), "codex", "fixture", coreauth.Options{}, homeAuths)
		if err != nil {
			t.Fatal(err)
		}
		if a.ID == "a-zero" {
			homeZero++
		}
		b, err := cpaSelector.Pick(context.Background(), "codex", "fixture", cpaexecutor.Options{}, cpaAuths)
		if err != nil {
			t.Fatal(err)
		}
		if b.ID == "a-zero" {
			cpaZero++
		}
	}
	t.Logf("weight=0 selections: Home=%d CPA=%d; Home selector=%T", homeZero, cpaZero, selected)
	if homeZero != cpaZero {
		t.Error("weighted routing differs from CPA V8")
	}
}
