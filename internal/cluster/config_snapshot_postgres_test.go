package cluster

import (
	"context"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	cpaconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"gorm.io/gorm"
)

func TestOAuthConfigUpsertAndSnapshotMutationSerializePostgres(t *testing.T) {
	for _, first := range []string{"legacy", "v8"} {
		t.Run(first+" first", func(t *testing.T) {
			repo := newPostgresQuiescenceRepository(t)
			ctx, cancelCtx := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancelCtx()
			// Keep both mutations OAuth-scoped so they contend over the same stored root.
			if errSeed := repo.ReplaceConfigSnapshot(ctx, map[string]any{"oauth": map[string]any{"providers": map[string]any{
				"codex":       map[string]any{"header-defaults": map[string]any{"user-agent": "old-agent"}},
				"antigravity": map[string]any{"sensitive-words": []string{"old"}},
			}}}); errSeed != nil {
				t.Fatal(errSeed)
			}

			mutations := map[string]func(context.Context, *Repository) error{
				"legacy": func(updateCtx context.Context, updateRepo *Repository) error {
					return updateRepo.UpsertConfigValue(updateCtx, "antigravity", map[string]any{"sensitive-words": []string{"new"}})
				},
				"v8": func(updateCtx context.Context, updateRepo *Repository) error {
					return updateRepo.MutateConfigSnapshot(updateCtx, DefaultHeartbeatTimeout(), func(root map[string]any) (map[string]any, error) {
						providers := root["oauth"].(map[string]any)["providers"].(map[string]any)
						providers["codex"].(map[string]any)["header-defaults"].(map[string]any)["user-agent"] = "new-agent"
						return root, nil
					})
				},
			}
			second := "legacy"
			if first == "legacy" {
				second = "v8"
			}
			type mutationRoleKey struct{}
			readStarted, releaseRead := make(chan struct{}), make(chan struct{})
			var readOnce atomic.Bool
			var releaseOnce sync.Once
			var workers sync.WaitGroup
			defer func() {
				releaseOnce.Do(func() { close(releaseRead) })
				cancelCtx()
				workers.Wait()
			}()
			const callbackName = "test:config-scope-read"
			if errRegister := repo.db.Callback().Query().After("gorm:query").Register(callbackName, func(tx *gorm.DB) {
				if tx.Error != nil || tx.Statement.Context.Value(mutationRoleKey{}) != "holder" ||
					tx.Statement.Schema == nil || tx.Statement.Schema.Table != (ConfigRecord{}).TableName() || !readOnce.CompareAndSwap(false, true) {
					return
				}
				close(readStarted)
				select {
				case <-releaseRead:
				case <-ctx.Done():
					tx.AddError(ctx.Err())
				}
			}); errRegister != nil {
				t.Fatal(errRegister)
			}
			t.Cleanup(func() {
				if errRemove := repo.db.Callback().Query().Remove(callbackName); errRemove != nil {
					t.Error(errRemove)
				}
			})

			holderPID, contenderPID := make(chan postgresBackendPIDResult, 1), make(chan postgresBackendPIDResult, 1)
			results := make(chan error, 2)
			run := func(role, mutation string, pids chan<- postgresBackendPIDResult) {
				defer workers.Done()
				mutationCtx := context.WithValue(ctx, mutationRoleKey{}, role)
				results <- repo.db.WithContext(mutationCtx).Transaction(func(tx *gorm.DB) error {
					if errPID := postgresContenderBackendPIDHook(pids)(tx); errPID != nil {
						return errPID
					}
					return mutations[mutation](mutationCtx, NewRepository(tx))
				})
			}
			workers.Add(1)
			go run("holder", first, holderPID)
			select {
			case <-readStarted:
			case <-ctx.Done():
				t.Fatal("first mutation did not reach its config read")
			}
			workers.Add(1)
			go run("contender", second, contenderPID)
			// Verify the real database lock wait instead of relying on timer delays.
			waitForPostgresContendersBlockedByHolder(t, ctx, repo, holderPID, contenderPID)
			releaseOnce.Do(func() { close(releaseRead) })
			for range 2 {
				select {
				case errMutation := <-results:
					if errMutation != nil {
						t.Fatal(errMutation)
					}
				case <-ctx.Done():
					t.Fatal("concurrent config mutations did not complete")
				}
			}
			cfg, wire, errConfig := repo.LoadConfigAsRuntimeConfig(ctx)
			if errConfig != nil {
				t.Fatal(errConfig)
			}
			cpa, errCPA := cpaconfig.ParseConfigBytes(wire)
			if errCPA != nil {
				t.Fatal(errCPA)
			}
			if cpa.CodexHeaderDefaults.UserAgent != "new-agent" || !cfg.OAuthOnlyFields["codex-header-defaults.user-agent"] || !reflect.DeepEqual(cpa.Antigravity.SensitiveWords, []string{"new"}) {
				t.Fatal("a committed config mutation was overwritten by a stale OAuth scope")
			}
		})
	}
}
