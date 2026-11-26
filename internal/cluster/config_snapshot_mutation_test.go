package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	cpaconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"gorm.io/gorm"
)

func TestMutateConfigSnapshotConcurrentRepositories(t *testing.T) {
	repo := newCredentialFoundationTestRepository(t)
	ctx := context.Background()
	if errSeed := repo.ReplaceConfigSnapshot(ctx, map[string]any{"logs-max-total-size-mb": 0}); errSeed != nil {
		t.Fatal(errSeed)
	}
	var database struct{ File string }
	if errFile := repo.db.Raw("SELECT file FROM pragma_database_list WHERE name = 'main'").Scan(&database).Error; errFile != nil {
		t.Fatal(errFile)
	}
	otherDB, errOpen := OpenSQLite(ctx, database.File)
	if errOpen != nil {
		t.Fatal(errOpen)
	}
	otherSQLDB, errDB := otherDB.DB()
	if errDB != nil {
		t.Fatal(errDB)
	}
	t.Cleanup(func() {
		if errClose := otherSQLDB.Close(); errClose != nil {
			t.Error(errClose)
		}
	})
	repos := []*Repository{repo, NewRepository(otherDB)}
	const count = 12
	start := make(chan struct{})
	results := make(chan error, count)
	var workers sync.WaitGroup
	for index := range count {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			results <- repos[index%len(repos)].MutateConfigSnapshot(ctx, DefaultHeartbeatTimeout(), func(root map[string]any) (map[string]any, error) {
				root["logs-max-total-size-mb"] = int(root["logs-max-total-size-mb"].(float64)) + 1
				return root, nil
			})
		}()
	}
	close(start)
	workers.Wait()
	close(results)
	for errMutation := range results {
		if errMutation != nil {
			t.Error(errMutation)
		}
	}
	snapshot, errSnapshot := repo.LoadConfigSnapshot(ctx)
	if errSnapshot != nil {
		t.Fatal(errSnapshot)
	}
	var got int
	if errDecode := json.Unmarshal(snapshot["logs-max-total-size-mb"], &got); errDecode != nil {
		t.Fatal(errDecode)
	}
	if got != count {
		t.Fatalf("committed edits=%d, want %d", got, count)
	}
}

func TestMutateConfigSnapshotRollsBackRejectedEdit(t *testing.T) {
	repo := newCredentialFoundationTestRepository(t)
	ctx := context.Background()
	if errSeed := repo.ReplaceConfigSnapshot(ctx, map[string]any{"debug": false}); errSeed != nil {
		t.Fatal(errSeed)
	}
	before, errBefore := repo.LoadConfigSnapshot(ctx)
	if errBefore != nil {
		t.Fatal(errBefore)
	}
	rejected := errors.New("rejected fixture edit")
	err := repo.MutateConfigSnapshot(ctx, DefaultHeartbeatTimeout(), func(root map[string]any) (map[string]any, error) {
		root["debug"] = true
		return root, rejected
	})
	if !errors.Is(err, rejected) {
		t.Fatalf("rejected mutation returned %v", err)
	}
	after, errAfter := repo.LoadConfigSnapshot(ctx)
	if errAfter != nil {
		t.Fatal(errAfter)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("rejected edit changed persisted configuration")
	}
}

func TestConfigUpsertLocksSQLiteBeforeReading(t *testing.T) {
	tests := []struct {
		name    string
		key     string
		initial any
		next    any
		want    UpsertResult
	}{
		{name: "OAuth provider", key: "antigravity", initial: map[string]any{"sensitive-words": []string{"old"}}, next: map[string]any{"sensitive-words": []string{"new"}}, want: UpsertResultUpdated},
		{name: "upstream provider", key: "xai", initial: map[string]any{"inject-x-search": false}, next: map[string]any{"inject-x-search": true}, want: UpsertResultUpdated},
		{name: "ordinary root", key: "debug", initial: false, next: true, want: UpsertResultUpdated},
		{name: "direct OAuth root", key: "oauth", initial: map[string]any{}, next: map[string]any{"providers": map[string]any{}}, want: UpsertResultUpdated},
		{name: "new root", key: "request-retry", next: 3, want: UpsertResultCreated},
		{name: "unchanged root", key: "debug", initial: false, next: false, want: UpsertResultUnchanged},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repo := newCredentialFoundationTestRepository(t)
			ctx, cancelCtx := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancelCtx()
			if errSeed := repo.UpsertConfigValue(ctx, "write-lock-probe", false); errSeed != nil {
				t.Fatal(errSeed)
			}
			if test.initial != nil {
				if errSeed := repo.UpsertConfigValue(ctx, test.key, test.initial); errSeed != nil {
					t.Fatal(errSeed)
				}
			}
			var database struct{ File string }
			if errFile := repo.db.Raw("SELECT file FROM pragma_database_list WHERE name = 'main'").Scan(&database).Error; errFile != nil {
				t.Fatal(errFile)
			}
			otherDB, errOpen := OpenSQLite(ctx, database.File)
			if errOpen != nil {
				t.Fatal(errOpen)
			}
			otherSQLDB, errDB := otherDB.DB()
			if errDB != nil {
				t.Fatal(errDB)
			}
			t.Cleanup(func() {
				if errClose := otherSQLDB.Close(); errClose != nil {
					t.Error(errClose)
				}
			})
			// Probe lock ownership immediately instead of waiting for a busy timeout.
			if errPragma := otherDB.Exec("PRAGMA busy_timeout=0").Error; errPragma != nil {
				t.Fatal(errPragma)
			}
			checked := false
			const callbackName = "test:config-upsert-write-lock"
			if errRegister := repo.db.Callback().Query().After("gorm:query").Register(callbackName, func(tx *gorm.DB) {
				if checked || tx.Statement.Schema == nil || tx.Statement.Schema.Table != (ConfigRecord{}).TableName() {
					return
				}
				checked = true
				// A different connection must not commit after the config read, even
				// when the target root is absent or the upsert will be a no-op.
				errWrite := otherDB.WithContext(ctx).Exec("UPDATE config SET version = version + 1 WHERE key = ?", "write-lock-probe").Error
				var sqliteError interface{ Code() int }
				if !errors.As(errWrite, &sqliteError) || sqliteError.Code()&0xff != 5 {
					t.Errorf("competing write after config read = %v, want SQLITE_BUSY", errWrite)
				}
			}); errRegister != nil {
				t.Fatal(errRegister)
			}
			t.Cleanup(func() {
				if errRemove := repo.db.Callback().Query().Remove(callbackName); errRemove != nil {
					t.Error(errRemove)
				}
			})
			result, errUpsert := repo.UpsertConfigValueWithResult(ctx, test.key, test.next)
			if errUpsert != nil {
				t.Fatal(errUpsert)
			}
			if !checked {
				t.Fatal("upsert did not reach its config read")
			}
			if result != test.want {
				t.Fatalf("upsert result = %q, want %q", result, test.want)
			}
			var record ConfigRecord
			if errLoad := repo.db.First(&record, "key = ?", test.key).Error; errLoad != nil {
				t.Fatal(errLoad)
			}
			wantJSON, errMarshal := json.Marshal(test.next)
			if errMarshal != nil {
				t.Fatal(errMarshal)
			}
			equal, errEqual := semanticJSONEqual(record.Value, wantJSON)
			if errEqual != nil || !equal {
				t.Fatalf("persisted config = %s, want %s (error: %v)", record.Value, wantJSON, errEqual)
			}
		})
	}
}

func TestOAuthConfigUpsertsPreserveConcurrentScopes(t *testing.T) {
	repo := newCredentialFoundationTestRepository(t)
	ctx, cancelCtx := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelCtx()
	if errSeed := repo.ReplaceConfigSnapshot(ctx, map[string]any{"oauth": map[string]any{"providers": map[string]any{
		"codex":       map[string]any{"disable-codex-cloaking": false},
		"antigravity": map[string]any{"sensitive-words": []string{"old"}},
		"xai":         map[string]any{"inject-x-search": false},
		"devin":       map[string]any{"sensitive-words": []string{"old"}},
	}}}); errSeed != nil {
		t.Fatal(errSeed)
	}
	var database struct{ File string }
	if errFile := repo.db.Raw("SELECT file FROM pragma_database_list WHERE name = 'main'").Scan(&database).Error; errFile != nil {
		t.Fatal(errFile)
	}
	otherDB, errOpen := OpenSQLite(ctx, database.File)
	if errOpen != nil {
		t.Fatal(errOpen)
	}
	otherSQLDB, errDB := otherDB.DB()
	if errDB != nil {
		t.Fatal(errDB)
	}
	t.Cleanup(func() {
		if errClose := otherSQLDB.Close(); errClose != nil {
			t.Error(errClose)
		}
	})
	repos := []*Repository{repo, NewRepository(otherDB)}
	edits := []struct {
		key   string
		value map[string]any
	}{
		{key: "codex", value: map[string]any{"disable-codex-cloaking": true}},
		{key: "antigravity", value: map[string]any{"sensitive-words": []string{"new"}}},
		{key: "xai", value: map[string]any{"inject-x-search": true}},
		{key: "devin", value: map[string]any{"sensitive-words": []string{"new"}}},
	}
	start := make(chan struct{})
	results := make(chan error, len(edits))
	for index, edit := range edits {
		go func() {
			<-start
			results <- repos[index%len(repos)].UpsertConfigValue(ctx, edit.key, edit.value)
		}()
	}
	close(start)
	for range edits {
		if errUpdate := <-results; errUpdate != nil {
			t.Error(errUpdate)
		}
	}
	_, wire, errConfig := repo.LoadConfigAsRuntimeConfig(ctx)
	if errConfig != nil {
		t.Fatal(errConfig)
	}
	cpa, errCPA := cpaconfig.ParseConfigBytes(wire)
	if errCPA != nil {
		t.Fatal(errCPA)
	}
	if !cpa.Codex.DisableCodexCloaking || !cpa.XAI.InjectXSearch ||
		!reflect.DeepEqual(cpa.Antigravity.SensitiveWords, []string{"new"}) || !reflect.DeepEqual(cpa.Devin.SensitiveWords, []string{"new"}) {
		t.Fatal("a concurrent edit replaced another provider's OAuth settings")
	}
}
