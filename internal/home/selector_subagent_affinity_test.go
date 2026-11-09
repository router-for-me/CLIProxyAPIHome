package home

import (
	"net/http"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
)

func TestSelectorFromConfigSubagentAffinity(t *testing.T) {
	store := coreauth.NewSessionCache(time.Hour)
	t.Cleanup(store.Stop)
	cfg := &config.Config{Routing: config.RoutingConfig{Strategy: "fill-first", SessionAffinity: true}}
	selector := selectorFromConfig(cfg, store)
	auths := []*coreauth.Auth{{ID: "a", Status: coreauth.StatusActive}, {ID: "b", Status: coreauth.StatusActive}}
	parent := coreauth.Options{Headers: http.Header{"X-Session-Id": {"parent"}}}
	picked, errPick := selector.Pick(t.Context(), "openai", "model", parent, auths[1:])
	if errPick != nil || picked == nil || picked.ID != "b" {
		t.Fatalf("parent=%v err=%v", picked, errPick)
	}
	disabled, enabled := false, true
	steps := []struct {
		setting *bool
		session string
		want    string
	}{
		{nil, "old-child", "b"},
		{&disabled, "new-child", "a"},
		{&disabled, "old-child", "b"},
		{&enabled, "new-child", "a"},
		{&enabled, "another-child", "b"},
	}
	for _, step := range steps {
		cfg.Routing.SessionAffinitySubagents = step.setting
		selector = selectorFromConfig(cfg, store)
		opts := coreauth.Options{Headers: http.Header{"X-Session-Id": {step.session}, "X-Parent-Session-Id": {"parent"}}}
		picked, errPick = selector.Pick(t.Context(), "openai", "model", opts, auths)
		if errPick != nil || picked == nil || picked.ID != step.want {
			t.Fatalf("session=%s pick=%v err=%v, want %s", step.session, picked, errPick, step.want)
		}
	}
	cfg.Routing.SessionAffinity = false
	if _, ok := selectorFromConfig(cfg, store).(*coreauth.SessionAffinitySelector); ok {
		t.Fatal("subagent setting enabled affinity with master switch off")
	}
}
