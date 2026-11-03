package dynamic

import (
	"path/filepath"
	"sync/atomic"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/home"
	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
	"github.com/router-for-me/CLIProxyAPIHome/internal/respserver/dispatch"
	"github.com/tidwall/gjson"
	"gorm.io/gorm"
)

func TestDispatchReusesAPIKeyOnlyWithinRequest(t *testing.T) {
	ctx := t.Context()
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
	key, errKey := repo.CreateAPIKey(ctx, cluster.APIKeyEntryUpdate{APIKey: "client-key"})
	if errKey != nil {
		t.Fatal(errKey)
	}
	rt, errRuntime := home.NewRuntime(&config.Config{})
	if errRuntime != nil {
		t.Fatal(errRuntime)
	}
	t.Cleanup(rt.Stop)
	rt.SetClusterAdapter(cluster.NewRuntimeAdapter(repo, ""))
	rt.SetConcurrencyAdmitter(&concurrencyDispatchAdmitter{})
	rt.CoreManager().SetFullAuthResolver(nil)
	rt.CoreManager().SetSelector(coreauth.NewSessionAffinitySelectorWithConfig(coreauth.SessionAffinityConfig{Fallback: &coreauth.FillFirstSelector{}}))
	const credentialID = "request-cache-credential"
	if _, errRegister := rt.CoreManager().Register(ctx, &coreauth.Auth{ID: credentialID, Provider: "openai"}); errRegister != nil {
		t.Fatal(errRegister)
	}
	registry.GetGlobalRegistry().RegisterClient(credentialID, "openai", []*registry.ModelInfo{{ID: "gpt"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(credentialID) })

	var keyQueries atomic.Int64
	const callback = "test:dispatch-api-key-queries"
	if errCallback := db.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == (cluster.APIKeyRecord{}).TableName() {
			keyQueries.Add(1)
		}
	}); errCallback != nil {
		t.Fatal(errCallback)
	}
	t.Cleanup(func() {
		if errRemove := db.Callback().Query().Remove(callback); errRemove != nil {
			t.Error(errRemove)
		}
	})
	args := []string{"RPOP", `{"type":"auth","model":"gpt","headers":{"authorization":"Bearer client-key"}}`}
	for range 2 {
		keyQueries.Store(0)
		result, _, errReply := dispatchRequest(ctx, dispatch.Env{Runtime: rt}, args)
		if result == nil || errReply != nil {
			t.Fatalf("dispatch result=%#v error=%#v", result, errReply)
		}
		if count := keyQueries.Load(); count != 1 {
			t.Fatalf("API key queried %d times, want 1 per request", count)
		}
	}
	if errDelete := repo.DeleteAPIKey(ctx, cluster.APIKeySelector{ID: key.ID}); errDelete != nil {
		t.Fatal(errDelete)
	}
	if _, _, errReply := dispatchRequest(ctx, dispatch.Env{Runtime: rt}, args); errReply == nil || gjson.GetBytes(errReply.BulkString, "error.type").String() != "invalid_credential" {
		t.Fatalf("deleted API key dispatch error=%#v", errReply)
	}
}
