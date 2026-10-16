package get

import (
	"context"
	"sort"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
	"github.com/router-for-me/CLIProxyAPIHome/internal/respserver/dispatch"
	"github.com/tidwall/gjson"
)

func registerScopeTestModels(t *testing.T, rt interface {
	CoreManager() *coreauth.Manager
}) {
	t.Helper()
	register := func(id, provider string, attrs map[string]string, models ...*registry.ModelInfo) {
		t.Helper()
		auth := &coreauth.Auth{ID: id, Provider: provider, Status: coreauth.StatusActive, Attributes: attrs}
		if _, errRegister := rt.CoreManager().Register(context.Background(), auth); errRegister != nil {
			t.Fatalf("register %s: %v", id, errRegister)
		}
		registry.GetGlobalRegistry().RegisterClient(id, provider, models)
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	}
	register("scope-claude", "claude", map[string]string{"api_key": "a"},
		&registry.ModelInfo{ID: "scope-claude-allowed"}, &registry.ModelInfo{ID: "scope-claude-denied"})
	register("scope-codex", "codex", map[string]string{"api_key": "b", "plan_type": "pro"},
		&registry.ModelInfo{ID: "scope-codex-denied"})
}

func payloadModelIDs(t *testing.T, payload string) []string {
	t.Helper()
	var ids []string
	gjson.Parse(payload).ForEach(func(_, bucket gjson.Result) bool {
		bucket.ForEach(func(_, model gjson.Result) bool {
			ids = append(ids, model.Get("id").String())
			return true
		})
		return true
	})
	sort.Strings(ids)
	return ids
}

func TestBuildModelsJSONFiltersByAllowedModelIDs(t *testing.T) {
	rt := newGetTestRuntime(t)
	registerScopeTestModels(t, rt)
	env := dispatch.Env{Runtime: rt}

	cases := []struct {
		name    string
		allowed []string
		want    []string
	}{
		{"nil keeps every model", nil, []string{"scope-claude-allowed", "scope-claude-denied", "scope-codex-denied"}},
		{"allowlist keeps listed models only", []string{"scope-claude-allowed"}, []string{"scope-claude-allowed"}},
		{"allowlist matches case-insensitively and ignores suffixes", []string{"SCOPE-CLAUDE-ALLOWED(high)"}, []string{"scope-claude-allowed"}},
		{"empty allowlist lists nothing", []string{}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload, errBuild := buildModelsJSON(env, tc.allowed)
			if errBuild != nil {
				t.Fatalf("buildModelsJSON: %v", errBuild)
			}
			got := payloadModelIDs(t, payload)
			if len(got) != len(tc.want) {
				t.Fatalf("models = %v, want %v (payload %s)", got, tc.want, payload)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("models = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestHandleDefaultModelsRespectsAPIKeyModelGroups(t *testing.T) {
	rt, repo := newGetTestRuntimeWithRepository(t)
	registerScopeTestModels(t, rt)
	ctx := context.Background()

	group, errGroup := repo.CreateModelGroup(ctx, "scope-group", false)
	if errGroup != nil {
		t.Fatalf("CreateModelGroup: %v", errGroup)
	}
	if _, errDetail := repo.CreateModelGroupDetail(ctx, group.ID, "scope-claude-allowed", nil); errDetail != nil {
		t.Fatalf("CreateModelGroupDetail: %v", errDetail)
	}
	emptyGroup, errEmpty := repo.CreateModelGroup(ctx, "scope-empty-group", false)
	if errEmpty != nil {
		t.Fatalf("CreateModelGroup(empty): %v", errEmpty)
	}
	scoped := []uint{group.ID}
	empty := []uint{emptyGroup.ID}
	for _, key := range []cluster.APIKeyEntryUpdate{
		{APIKey: "scope-unrestricted-key"},
		{APIKey: "scope-restricted-key", ModelGroups: &scoped},
		{APIKey: "scope-empty-key", ModelGroups: &empty},
	} {
		if _, errKey := repo.CreateAPIKey(ctx, key); errKey != nil {
			t.Fatalf("CreateAPIKey(%s): %v", key.APIKey, errKey)
		}
	}

	cases := []struct {
		key  string
		want []string
	}{
		{"scope-unrestricted-key", []string{"scope-claude-allowed", "scope-claude-denied", "scope-codex-denied"}},
		{"scope-restricted-key", []string{"scope-claude-allowed"}},
		{"scope-empty-key", nil},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			reply := handleDefault(ctx, dispatch.Env{Runtime: rt}, []string{"GET", `{"type":"models","headers":{"authorization":"Bearer ` + tc.key + `"}}`})
			if reply.Kind != dispatch.ReplyKindBulkString {
				t.Fatalf("reply kind = %v, want bulk string", reply.Kind)
			}
			if errType := gjson.GetBytes(reply.BulkString, "error.type"); errType.Exists() {
				t.Fatalf("unexpected error: %s", string(reply.BulkString))
			}
			got := payloadModelIDs(t, string(reply.BulkString))
			if len(got) != len(tc.want) {
				t.Fatalf("models = %v, want %v (payload %s)", got, tc.want, string(reply.BulkString))
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("models = %v, want %v", got, tc.want)
				}
			}
		})
	}
}
