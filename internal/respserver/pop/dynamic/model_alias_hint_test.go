package dynamic

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/home"
	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
	"github.com/router-for-me/CLIProxyAPIHome/internal/respserver/dispatch"
	"github.com/tidwall/gjson"
)

func newAliasHintRESPRuntime(t *testing.T) (*cluster.Repository, *home.Runtime) {
	t.Helper()
	db, errOpen := cluster.OpenSQLite(t.Context(), filepath.Join(t.TempDir(), "home.db"))
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
	runtime, errRuntime := home.NewRuntime(&config.Config{AuthDir: t.TempDir()})
	if errRuntime != nil {
		t.Fatal(errRuntime)
	}
	t.Cleanup(runtime.Stop)
	runtime.SetClusterAdapter(cluster.NewRuntimeAdapter(repo, ""))
	runtime.SetConcurrencyAdmitter(nil)
	return repo, runtime
}

func publishAliasHintRESPAuth(t *testing.T, repo *cluster.Repository, runtime *home.Runtime, upstream string, aliases ...string) string {
	t.Helper()
	models := make([]any, 0, len(aliases))
	for _, alias := range aliases {
		models = append(models, map[string]any{"id": alias, "name": upstream, "user_defined": true})
	}
	auth := &coreauth.Auth{
		ID: t.Name(), Index: t.Name(), Provider: "alias-hint-compat", Status: coreauth.StatusActive,
		Attributes: map[string]string{"api_key": "test-upstream-key"},
		Metadata:   map[string]any{"home_config_models": models},
	}
	record, errUpsert := repo.UpsertAuth(t.Context(), auth, "test")
	if errUpsert != nil {
		t.Fatal(errUpsert)
	}
	if errReload := runtime.ReloadAuths(t.Context()); errReload != nil {
		t.Fatal(errReload)
	}
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(record.UUID) })
	return record.UUID
}

func requestAliasHintRESP(t *testing.T, runtime *home.Runtime, key, model string) (*home.DispatchResult, *dispatch.Reply) {
	t.Helper()
	payload, errMarshal := json.Marshal(map[string]any{
		"type": "auth", "model": model, "headers": map[string]string{"authorization": "Bearer " + key},
	})
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	result, _, reply := dispatchRequest(t.Context(), dispatch.Env{Runtime: runtime}, []string{"RPOP", string(payload)})
	return result, reply
}

func TestRESPDispatchModelAliasHintsPreserveErrorCodes(t *testing.T) {
	for _, test := range []struct {
		name     string
		upstream string
		wantCode string
	}{
		{name: "static model", upstream: "gpt-6.1-sol", wantCode: "auth_not_found"},
		{name: "custom model", upstream: "upstream-custom-218", wantCode: "model_not_found"},
	} {
		t.Run(test.name, func(t *testing.T) {
			repo, runtime := newAliasHintRESPRuntime(t)
			publishAliasHintRESPAuth(t, repo, runtime, test.upstream, "friendly-218")
			const key = "alias-hint-client"
			if _, errKey := repo.CreateAPIKey(t.Context(), cluster.APIKeyEntryUpdate{APIKey: key}); errKey != nil {
				t.Fatal(errKey)
			}
			_, reply := requestAliasHintRESP(t, runtime, key, test.upstream)
			if reply == nil || gjson.GetBytes(reply.BulkString, "error.type").String() != test.wantCode ||
				!strings.Contains(gjson.GetBytes(reply.BulkString, "error.message").String(), `under the alias "friendly-218"`) {
				t.Fatalf("lookup error = %+v, want %s and alias hint", reply, test.wantCode)
			}
			if result, replyAlias := requestAliasHintRESP(t, runtime, key, "friendly-218"); result == nil || replyAlias != nil || result.Model != test.upstream {
				t.Fatalf("alias must keep routing to the upstream model: result=%+v reply=%+v", result, replyAlias)
			}
		})
	}
}

func TestRESPDispatchModelAliasHintBeforeBillingRejection(t *testing.T) {
	repo, runtime := newAliasHintRESPRuntime(t)
	publishAliasHintRESPAuth(t, repo, runtime, "upstream-priced-218", "friendly-priced-218")
	username, unlimited := "alias-hint-user", true
	user, errUser := repo.CreateUser(t.Context(), cluster.UserUpdate{Username: &username, CreditsUnlimited: &unlimited})
	if errUser != nil {
		t.Fatal(errUser)
	}
	key := "alias-hint-user-key"
	if _, errKey := repo.CreateAPIKeyForUser(t.Context(), user.ID, cluster.APIKeyUserUpdate{APIKey: &key}); errKey != nil {
		t.Fatal(errKey)
	}
	if _, errPrice := repo.CreateBillingModelPrice(t.Context(), cluster.BillingModelPriceUpdate{Provider: "openai", Model: "friendly-priced-218", Enabled: true}); errPrice != nil {
		t.Fatal(errPrice)
	}
	_, reply := requestAliasHintRESP(t, runtime, key, "upstream-priced-218")
	if reply == nil || gjson.GetBytes(reply.BulkString, "error.type").String() != "model_not_found" {
		t.Fatalf("expected model_not_found: %+v", reply)
	}
	message := gjson.GetBytes(reply.BulkString, "error.message").String()
	if !strings.Contains(message, "has no configured price") || !strings.Contains(message, `under the alias "friendly-priced-218"`) {
		t.Fatalf("billing error must retain its reason and include the alias: %s", reply.BulkString)
	}
	if result, replyAlias := requestAliasHintRESP(t, runtime, key, "friendly-priced-218"); result == nil || replyAlias != nil {
		t.Fatalf("priced alias should route: result=%+v reply=%+v", result, replyAlias)
	}
}

func TestRESPDispatchModelAliasHintRespectsTargetPermissions(t *testing.T) {
	repo, runtime := newAliasHintRESPRuntime(t)
	const upstream = "upstream-scoped-218"
	authID := publishAliasHintRESPAuth(t, repo, runtime, upstream, "a-private", "b-private", "c-private", "d-visible")
	allowed, errAllowed := repo.CreateChannelGroup(t.Context(), "allowed", false)
	if errAllowed != nil {
		t.Fatal(errAllowed)
	}
	if _, errDetail := repo.CreateChannelGroupDetail(t.Context(), allowed.ID, authID); errDetail != nil {
		t.Fatal(errDetail)
	}
	empty, errEmpty := repo.CreateChannelGroup(t.Context(), "empty", false)
	if errEmpty != nil {
		t.Fatal(errEmpty)
	}
	modelGroup := func(name string, models map[string][]uint) uint {
		t.Helper()
		group, errGroup := repo.CreateModelGroup(t.Context(), name, false)
		if errGroup != nil {
			t.Fatal(errGroup)
		}
		for model, channels := range models {
			if _, errDetail := repo.CreateModelGroupDetail(t.Context(), group.ID, model, channels); errDetail != nil {
				t.Fatal(errDetail)
			}
		}
		return group.ID
	}
	visibleModels := modelGroup("visible", map[string][]uint{upstream: {empty.ID}, "d-visible": {allowed.ID}})
	deniedModels := modelGroup("denied", map[string][]uint{"unrelated-model": nil})
	deniedAliasChannels := modelGroup("denied-alias-channel", map[string][]uint{"d-visible": {empty.ID}})
	for _, test := range []struct {
		name     string
		channels []uint
		models   []uint
		wantHint bool
	}{
		{name: "credential denied", channels: []uint{empty.ID}},
		{name: "model denied", models: []uint{deniedModels}},
		{name: "target alias channel denied", models: []uint{deniedAliasChannels}},
		{name: "target alias channel allowed", models: []uint{visibleModels}, wantHint: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			key := "alias-hint-" + test.name
			if _, errKey := repo.CreateAPIKey(t.Context(), cluster.APIKeyEntryUpdate{APIKey: key, Channels: &test.channels, ModelGroups: &test.models}); errKey != nil {
				t.Fatal(errKey)
			}
			_, reply := requestAliasHintRESP(t, runtime, key, upstream)
			if reply == nil {
				t.Fatal("upstream request must fail")
			}
			message := gjson.GetBytes(reply.BulkString, "error.message").String()
			if strings.Contains(message, "private") || strings.Contains(message, `"d-visible"`) != test.wantHint {
				t.Fatalf("hint crosses target permissions: %s", reply.BulkString)
			}
			if test.wantHint {
				if result, replyAlias := requestAliasHintRESP(t, runtime, key, "d-visible"); result == nil || replyAlias != nil {
					t.Fatalf("suggested alias must be permitted: result=%+v reply=%+v", result, replyAlias)
				}
			}
		})
	}
	key := "alias-hint-scope-update"
	models := []uint{visibleModels}
	if _, errKey := repo.CreateAPIKey(t.Context(), cluster.APIKeyEntryUpdate{APIKey: key, ModelGroups: &models}); errKey != nil {
		t.Fatal(errKey)
	}
	disabled := true
	if _, errUpdate := repo.UpdateChannelGroup(t.Context(), allowed.ID, cluster.ChannelGroupUpdate{Disabled: &disabled}); errUpdate != nil {
		t.Fatal(errUpdate)
	}
	_, reply := requestAliasHintRESP(t, runtime, key, upstream)
	if reply == nil || strings.Contains(gjson.GetBytes(reply.BulkString, "error.message").String(), `"d-visible"`) {
		t.Fatalf("permission updates must take effect without rebuilding the alias index: %+v", reply)
	}
}
