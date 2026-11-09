package cluster

import (
	"fmt"
	"net/http"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/home"
	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
)

func TestSessionAffinitySubagentsSharedDatabaseAndReload(t *testing.T) {
	repo := newCredentialFoundationTestRepository(t)
	ids := []string{"22222222-2222-4222-8222-000000000001", "22222222-2222-4222-8222-000000000002"}
	for _, id := range ids {
		auth := &coreauth.Auth{ID: id, Index: id, Provider: "openai", Status: coreauth.StatusActive, Attributes: map[string]string{"api_key": "fixture-key"}, Metadata: map[string]any{"home_config_models": []map[string]any{{"id": "subagent-model", "name": "subagent-model", "user_defined": true}}}}
		if _, errUpsert := repo.UpsertAuth(t.Context(), auth, "upsert"); errUpsert != nil {
			t.Fatal(errUpsert)
		}
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	}
	nodeA := newSessionAffinityRuntime(t, repo, "subagent-a")
	nodeB := newSessionAffinityRuntime(t, NewRepository(repo.db), "subagent-b")
	dispatch := func(node *home.Runtime, session, parent, pinned string) string {
		t.Helper()
		headers := http.Header{"X-Session-Id": {session}, "X-Parent-Session-Id": {parent}}
		result, errDispatch := node.DispatchForAPIKeyWithConcurrency(t.Context(), "subagent-model", headers, "", "", nil, pinned, home.DispatchConcurrencyContext{})
		if errDispatch != nil || result == nil {
			t.Fatalf("session=%s result=%v err=%v", session, result, errDispatch)
		}
		return result.AuthID
	}
	if got := dispatch(nodeA, "parent", "", ids[1]); got != ids[1] {
		t.Fatalf("parent=%s", got)
	}
	if got := dispatch(nodeB, "existing-child", "parent", ""); got != ids[1] {
		t.Fatalf("initial inheritance=%s", got)
	}
	for _, node := range []*home.Runtime{nodeA, nodeB} {
		cfg := *node.Config()
		disabled := false
		cfg.Routing.Strategy = "fill-first"
		cfg.Routing.SessionAffinitySubagents = &disabled
		if errApply := node.ApplyConfigFromCluster(t.Context(), &cfg); errApply != nil {
			t.Fatal(errApply)
		}
	}
	for index, node := range []*home.Runtime{nodeA, nodeB} {
		for _, step := range []struct{ session, parent, want string }{
			{fmt.Sprintf("new-child-%d", index), "parent", ids[0]},
			{"existing-child", "parent", ids[1]},
			{"parent", "", ids[1]},
		} {
			if got := dispatch(node, step.session, step.parent, ""); got != step.want {
				t.Fatalf("session=%s got=%s want=%s", step.session, got, step.want)
			}
		}
	}
	if got := dispatch(nodeB, "new-child-0", "parent", ""); got != ids[0] {
		t.Fatalf("shared child binding=%s", got)
	}
	cfg := *nodeA.Config()
	enabled := true
	cfg.Routing.SessionAffinitySubagents = &enabled
	if errApply := nodeA.ApplyConfigFromCluster(t.Context(), &cfg); errApply != nil {
		t.Fatal(errApply)
	}
	if got := dispatch(nodeA, "new-child-0", "parent", ""); got != ids[0] {
		t.Fatalf("enabling replaced existing binding=%s", got)
	}
	if got := dispatch(nodeA, "enabled-child", "parent", ""); got != ids[1] {
		t.Fatalf("enabled inheritance=%s", got)
	}
	if got := dispatch(nodeB, "enabled-child", "parent", ""); got != ids[1] {
		t.Fatalf("disabled node discarded shared child binding=%s", got)
	}
}
