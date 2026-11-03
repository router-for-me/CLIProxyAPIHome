package get

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/home"
	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
	"github.com/router-for-me/CLIProxyAPIHome/internal/respserver/dispatch"
	"github.com/tidwall/gjson"
	"gorm.io/gorm"
)

func TestHandleDefaultModelsScopes(t *testing.T) {
	ctx := context.Background()
	db, errOpen := cluster.OpenSQLite(ctx, filepath.Join(t.TempDir(), "home.db"))
	if errOpen != nil {
		t.Fatal(errOpen)
	}
	sqlDB, errDB := db.DB()
	if errDB != nil {
		t.Fatal(errDB)
	}
	t.Cleanup(func() {
		if errClose := sqlDB.Close(); errClose != nil {
			t.Error(errClose)
		}
	})
	if errMigrate := cluster.AutoMigrate(db); errMigrate != nil {
		t.Fatal(errMigrate)
	}
	repo := cluster.NewRepository(db)
	rt, errRuntime := home.NewRuntime(&config.Config{})
	if errRuntime != nil {
		t.Fatal(errRuntime)
	}
	rt.SetClusterAdapter(cluster.NewRuntimeAdapter(repo, ""))
	t.Cleanup(rt.Stop)
	env := dispatch.Env{Runtime: rt}

	register := func(auth *coreauth.Auth, modelIDs ...string) {
		t.Helper()
		auth.Index = auth.ID
		if _, errUpsert := repo.UpsertAuth(ctx, auth, "test"); errUpsert != nil {
			t.Fatal(errUpsert)
		}
		if _, errRegister := rt.CoreManager().Register(coreauth.WithSkipPersist(ctx), auth); errRegister != nil {
			t.Fatal(errRegister)
		}
		models := make([]*registry.ModelInfo, 0, len(modelIDs))
		for _, id := range modelIDs {
			models = append(models, &registry.ModelInfo{ID: id})
		}
		registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, models)
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	}
	register(&coreauth.Auth{ID: "scope-a", Provider: "claude", Status: coreauth.StatusActive,
		Attributes:  map[string]string{"api_key": "upstream-a"},
		Unavailable: true, NextRetryAfter: time.Now().Add(time.Hour)}, "model-a", "shared", "tenant/public(high)")
	register(&coreauth.Auth{ID: "scope-b", Provider: "claude", Status: coreauth.StatusActive,
		Attributes: map[string]string{"api_key": "upstream-b", "base_url": "https://gateway.example.com"}}, "model-b", "shared")
	register(&coreauth.Auth{ID: "scope-c", Provider: "codex", Status: coreauth.StatusActive}, "model-c")
	register(&coreauth.Auth{ID: "scope-disabled", Provider: "claude", Disabled: true}, "disabled-model")
	register(&coreauth.Auth{ID: "scope-status-disabled", Provider: "claude", Status: coreauth.StatusDisabled}, "status-disabled-model")

	channel := func(name string, disabled bool, authIDs ...string) uint {
		t.Helper()
		group, errGroup := repo.CreateChannelGroup(ctx, name, disabled)
		if errGroup != nil {
			t.Fatal(errGroup)
		}
		for _, id := range authIDs {
			if _, errDetail := repo.CreateChannelGroupDetail(ctx, group.ID, id); errDetail != nil {
				t.Fatal(errDetail)
			}
		}
		return group.ID
	}
	channelA := channel("a", false, "scope-a")
	channelB := channel("b", false, "scope-b")
	channelC := channel("c", false, "scope-c")
	emptyChannel := channel("empty", false)
	disabledChannel := channel("disabled", true, "scope-c")
	disabledAuths := channel("disabled-auths", false, "scope-disabled", "scope-status-disabled")

	modelGroup := func(name string, disabled bool, details map[string][]uint) uint {
		t.Helper()
		group, errGroup := repo.CreateModelGroup(ctx, name, disabled)
		if errGroup != nil {
			t.Fatal(errGroup)
		}
		for model, channels := range details {
			if _, errDetail := repo.CreateModelGroupDetail(ctx, group.ID, model, channels); errDetail != nil {
				t.Fatal(errDetail)
			}
		}
		return group.ID
	}
	models := modelGroup("models", false, map[string][]uint{"SHARED": nil, "model-b": nil, "model-missing": nil, "tenant/public(low)": nil})
	explicitModels := modelGroup("explicit", false, map[string][]uint{
		"shared": {channelB}, "model-a": {channelB}, "model-b": {channelB},
		"model-c": {disabledChannel}, "tenant/public": {emptyChannel},
	})
	inheritedModels := modelGroup("inherited", false, map[string][]uint{"shared": nil})
	unionModels := modelGroup("union", false, map[string][]uint{"shared": {channelA}})
	emptyModels := modelGroup("empty", false, nil)
	disabledModels := modelGroup("disabled", true, map[string][]uint{"shared": nil})

	request := func(key, source string) []byte {
		t.Helper()
		payload := map[string]any{"type": "models"}
		switch source {
		case "query":
			payload["query"] = map[string]string{"key": key}
		case "fallback-header":
			payload["headers"] = map[string]string{"authorization": "Bearer invalid-key", "x-api-key": key}
		default:
			payload["headers"] = map[string]string{"authorization": "Bearer " + key}
		}
		raw, errMarshal := json.Marshal(payload)
		if errMarshal != nil {
			t.Fatal(errMarshal)
		}
		reply := handleDefault(ctx, env, []string{"GET", string(raw)})
		if reply.Kind != dispatch.ReplyKindBulkString {
			t.Fatalf("reply kind = %v", reply.Kind)
		}
		return reply.BulkString
	}
	tests := []struct {
		name       string
		channels   []uint
		models     []uint
		source     string
		want       []string
		toolSearch bool
	}{
		{name: "unrestricted", want: []string{"model-a", "model-b", "model-c", "shared", "tenant/public(high)"}},
		{name: "credentials only", channels: []uint{channelA}, want: []string{"model-a", "shared", "tenant/public(high)"}, toolSearch: true},
		{name: "credential union", channels: []uint{channelA, channelC}, want: []string{"model-a", "model-c", "shared", "tenant/public(high)"}, toolSearch: true},
		{name: "models only", models: []uint{models}, want: []string{"model-b", "shared", "tenant/public(high)"}},
		{name: "both", channels: []uint{channelA}, models: []uint{models}, want: []string{"shared", "tenant/public(high)"}, toolSearch: true},
		{name: "query key", channels: []uint{channelA}, models: []uint{models}, source: "query", want: []string{"shared", "tenant/public(high)"}, toolSearch: true},
		{name: "authenticated principal", channels: []uint{channelA}, models: []uint{models}, source: "fallback-header", want: []string{"shared", "tenant/public(high)"}, toolSearch: true},
		{name: "model credentials", models: []uint{explicitModels}, want: []string{"model-b", "shared"}},
		{name: "empty model intersection", channels: []uint{channelA}, models: []uint{explicitModels}},
		{name: "empty detail cannot cancel restriction", channels: []uint{channelA}, models: []uint{explicitModels, inheritedModels}},
		{name: "duplicate model channel union", channels: []uint{channelA}, models: []uint{explicitModels, inheritedModels, unionModels}, want: []string{"shared"}, toolSearch: true},
		{name: "empty credentials", channels: []uint{emptyChannel}},
		{name: "disabled channel", channels: []uint{disabledChannel}},
		{name: "disabled credentials", channels: []uint{disabledAuths}},
		{name: "empty models", models: []uint{emptyModels}},
		{name: "disabled model group", models: []uint{disabledModels}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			key := "key-" + test.name
			if _, errCreate := repo.CreateAPIKey(ctx, cluster.APIKeyEntryUpdate{APIKey: key, Channels: &test.channels, ModelGroups: &test.models}); errCreate != nil {
				t.Fatal(errCreate)
			}
			queries := make(map[string]int)
			callback := "test:model-access-query-count"
			if errCallback := db.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
				queries[tx.Statement.Table]++
			}); errCallback != nil {
				t.Fatal(errCallback)
			}
			raw := request(key, test.source)
			if errRemove := db.Callback().Query().Remove(callback); errRemove != nil {
				t.Fatal(errRemove)
			}
			wantKeyQueries := 1
			if test.source == "fallback-header" {
				wantKeyQueries++
			}
			if count := queries[(cluster.APIKeyRecord{}).TableName()]; count != wantKeyQueries {
				t.Errorf("API key queried %d times, want %d", count, wantKeyQueries)
			}
			for _, table := range []string{"model_group_detail", "channel_group_detail"} {
				if queries[table] > 1 {
					t.Errorf("%s queried %d times, want at most one batch", table, queries[table])
				}
			}
			var buckets map[string][]registry.ModelInfo
			if errUnmarshal := json.Unmarshal(raw, &buckets); errUnmarshal != nil {
				t.Fatalf("decode models: %v; body=%s", errUnmarshal, raw)
			}
			var got []string
			for _, entries := range buckets {
				for _, model := range entries {
					got = append(got, model.ID)
					if model.ID == "shared" && (model.NativeCapabilities == nil || model.NativeCapabilities.ToolSearch == nil || *model.NativeCapabilities.ToolSearch != test.toolSearch) {
						t.Errorf("shared tool_search must use only permitted routes; body=%s", raw)
					}
				}
			}
			sort.Strings(got)
			if !reflect.DeepEqual(got, test.want) {
				t.Errorf("models = %v, want %v; body=%s", got, test.want, raw)
			}

			// Keep listing policy aligned with dispatch, including canonical IDs.
			policy, errPolicy := repo.ModelAccessForAPIKey(ctx, key)
			if errPolicy != nil {
				t.Fatal(errPolicy)
			}
			for _, model := range []string{"model-a", "model-b", "model-c", "shared", "tenant/public(high)"} {
				authIDs, modelIDs, errAllowed := repo.AllowedDispatchIDsForAPIKeyModel(ctx, key, model)
				if errAllowed != nil {
					t.Fatal(errAllowed)
				}
				modelAllowed := modelIDs == nil
				for _, id := range modelIDs {
					modelAllowed = modelAllowed || strings.EqualFold(coreauth.CanonicalModelID(id), coreauth.CanonicalModelID(model))
				}
				for _, authID := range []string{"scope-a", "scope-b", "scope-c"} {
					authAllowed := authIDs == nil
					for _, id := range authIDs {
						authAllowed = authAllowed || id == authID
					}
					if policy.Allows(authID, model) != (authAllowed && modelAllowed) {
						t.Errorf("listing and dispatch disagree for %s / %s", authID, model)
					}
				}
			}
		})
	}

	t.Run("policy failure never returns global catalog", func(t *testing.T) {
		callback := "test:model-access-failure"
		if errCallback := db.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
			if tx.Statement.Table == "model_group_detail" {
				tx.AddError(errors.New("database unavailable: secret-detail"))
			}
		}); errCallback != nil {
			t.Fatal(errCallback)
		}
		t.Cleanup(func() {
			if errRemove := db.Callback().Query().Remove(callback); errRemove != nil {
				t.Error(errRemove)
			}
		})
		raw := request("key-both", "")
		if gjson.GetBytes(raw, "error.type").String() != "internal_error" || gjson.GetBytes(raw, "error.message").String() != "Model access service error" {
			t.Fatalf("expected safe policy error, got %s", raw)
		}
		if strings.Contains(string(raw), "secret-detail") || gjson.GetBytes(raw, "claude").Exists() {
			t.Fatalf("policy failure leaked details or models: %s", raw)
		}
	})

	t.Run("scope update takes effect on next listing", func(t *testing.T) {
		disabled := true
		if _, errUpdate := repo.UpdateChannelGroup(ctx, channelA, cluster.ChannelGroupUpdate{Disabled: &disabled}); errUpdate != nil {
			t.Fatal(errUpdate)
		}
		if raw := request("key-credentials only", ""); string(raw) != "{}" {
			t.Fatalf("disabled scope must return empty catalog, got %s", raw)
		}
	})
}
