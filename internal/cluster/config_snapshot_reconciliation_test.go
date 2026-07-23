package cluster

import (
	"context"
	"errors"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/node"
)

func TestReplaceConfigSnapshotPreservesCredentialsWhenRootsAreOmitted(t *testing.T) {
	ctx := context.Background()
	repo := newCredentialFoundationTestRepository(t)
	geminiID := "12121212-1212-4212-8212-121212121212"
	codexID := "13131313-1313-4313-8313-131313131313"
	if errReplace := repo.ReplaceConfigSnapshot(ctx, providerCredentialSnapshotRoot(geminiID, "gemini-old", codexID, "codex-old")); errReplace != nil {
		t.Fatal(errReplace)
	}
	if errReplace := repo.ReplaceConfigSnapshot(ctx, map[string]any{"port": 8327}); errReplace != nil {
		t.Fatal(errReplace)
	}
	assertProviderCredentialSecrets(t, repo, ctx, map[string]string{geminiID: "gemini-old", codexID: "codex-old"})
}

func TestReplaceConfigSnapshotUpdatesOnlyExplicitProviderRoot(t *testing.T) {
	ctx := context.Background()
	repo := newCredentialFoundationTestRepository(t)
	geminiID := "14141414-1414-4414-8414-141414141414"
	codexID := "15151515-1515-4515-8515-151515151515"
	if errReplace := repo.ReplaceConfigSnapshot(ctx, providerCredentialSnapshotRoot(geminiID, "gemini-old", codexID, "codex-old")); errReplace != nil {
		t.Fatal(errReplace)
	}
	if errReplace := repo.ReplaceConfigSnapshot(ctx, map[string]any{
		"gemini-api-key": []any{map[string]any{"id": geminiID, "api-key": "gemini-new"}},
	}); errReplace != nil {
		t.Fatal(errReplace)
	}
	assertProviderCredentialSecrets(t, repo, ctx, map[string]string{geminiID: "gemini-new", codexID: "codex-old"})
}

func TestReplaceConfigSnapshotReplacesExplicitEmptyProviderRoot(t *testing.T) {
	ctx := context.Background()
	repo := newCredentialFoundationTestRepository(t)
	geminiID := "16161616-1616-4616-8616-161616161616"
	codexID := "17171717-1717-4717-8717-171717171717"
	if errReplace := repo.ReplaceConfigSnapshot(ctx, providerCredentialSnapshotRoot(geminiID, "gemini-old", codexID, "codex-old")); errReplace != nil {
		t.Fatal(errReplace)
	}
	if errReplace := repo.ReplaceConfigSnapshot(ctx, map[string]any{"gemini-api-key": []any{}}); errReplace != nil {
		t.Fatal(errReplace)
	}
	assertProviderCredentialSecrets(t, repo, ctx, map[string]string{codexID: "codex-old"})
}

func TestReplaceConfigSnapshotAndCreatePluginTaskPreservesCredentialsWhenRootsAreOmitted(t *testing.T) {
	ctx := context.Background()
	repo := newCredentialFoundationTestRepository(t)
	geminiID := "18181818-1818-4818-8818-181818181818"
	codexID := "19191919-1919-4919-8919-191919191919"
	if errReplace := repo.ReplaceConfigSnapshot(ctx, providerCredentialSnapshotRoot(geminiID, "gemini-old", codexID, "codex-old")); errReplace != nil {
		t.Fatal(errReplace)
	}
	if _, errReplace := repo.ReplaceConfigSnapshotAndCreatePluginTask(ctx, map[string]any{"port": 8327}, node.PluginTask{
		Operation: node.PluginTaskOperationDelete,
		PluginID:  "plugin-id",
	}); errReplace != nil {
		t.Fatal(errReplace)
	}
	assertProviderCredentialSecrets(t, repo, ctx, map[string]string{geminiID: "gemini-old", codexID: "codex-old"})
}

func TestReplaceConfigSnapshotRejectsOrphanedConcurrencyCredential(t *testing.T) {
	ctx := context.Background()
	repo := newCredentialFoundationTestRepository(t)
	id := "23232323-2323-4232-8232-232323232323"
	if errReplace := repo.ReplaceConfigSnapshot(ctx, map[string]any{"gemini-api-key": []any{map[string]any{"id": id, "api-key": "gemini-old"}}}); errReplace != nil {
		t.Fatal(errReplace)
	}
	setExchangePolicy(t, repo, id, 2, nil)

	errReplace := repo.ReplaceConfigSnapshot(ctx, map[string]any{"gemini-api-key": []any{}})
	if !errors.Is(errReplace, ErrCredentialConcurrencyOrphan) {
		t.Fatalf("ReplaceConfigSnapshot() error = %v, want ErrCredentialConcurrencyOrphan", errReplace)
	}
	assertProviderCredentialSecrets(t, repo, ctx, map[string]string{id: "gemini-old"})
}

func TestReplaceConfigSnapshotImportsConcurrencyPoliciesWithoutSnapshotRecord(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		policies any
		present  bool
		want     int64
	}{
		{name: "missing preserves", want: 2},
		{name: "empty clears", policies: map[string]any{}, present: true, want: 0},
		{name: "replace imports", policies: map[string]any{"24242424-2424-4242-8242-242424242424": map[string]any{"max-in-flight": 4}}, present: true, want: 4},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			ctx := context.Background()
			repo := newCredentialFoundationTestRepository(t)
			id := "24242424-2424-4242-8242-242424242424"
			root := map[string]any{"gemini-api-key": []any{map[string]any{"id": id, "api-key": "gemini-key"}}}
			if errReplace := repo.ReplaceConfigSnapshot(ctx, root); errReplace != nil {
				t.Fatal(errReplace)
			}
			setExchangePolicy(t, repo, id, 2, nil)
			if testCase.present {
				root[credentialConcurrencyPoliciesRootKey] = testCase.policies
			}
			if errReplace := repo.ReplaceConfigSnapshot(ctx, root); errReplace != nil {
				t.Fatal(errReplace)
			}
			assertExchangePolicy(t, repo, id, testCase.want, nil)
			snapshot, errSnapshot := repo.LoadConfigSnapshot(ctx)
			if errSnapshot != nil {
				t.Fatal(errSnapshot)
			}
			if _, exists := snapshot[credentialConcurrencyPoliciesRootKey]; exists {
				t.Fatal("credential concurrency policies persisted as a config snapshot record")
			}
		})
	}
}

func TestReplaceConfigSnapshotAndCreatePluginTaskImportsConcurrencyPolicies(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		policies any
		present  bool
		want     int64
	}{
		{name: "missing preserves", want: 2},
		{name: "empty clears", policies: map[string]any{}, present: true, want: 0},
		{name: "replace imports", policies: map[string]any{"26262626-2626-4262-8262-262626262626": map[string]any{"max-in-flight": 4}}, present: true, want: 4},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			ctx := context.Background()
			repo := newCredentialFoundationTestRepository(t)
			id := "26262626-2626-4262-8262-262626262626"
			root := map[string]any{"gemini-api-key": []any{map[string]any{"id": id, "api-key": "gemini-key"}}}
			if errReplace := repo.ReplaceConfigSnapshot(ctx, root); errReplace != nil {
				t.Fatal(errReplace)
			}
			setExchangePolicy(t, repo, id, 2, nil)
			if testCase.present {
				root[credentialConcurrencyPoliciesRootKey] = testCase.policies
			}
			if _, errReplace := repo.ReplaceConfigSnapshotAndCreatePluginTask(ctx, root, node.PluginTask{Operation: node.PluginTaskOperationDelete, PluginID: "plugin-id"}); errReplace != nil {
				t.Fatal(errReplace)
			}
			assertExchangePolicy(t, repo, id, testCase.want, nil)
		})
	}
}

func TestConfigReplacementRetiredConcurrencyPolicies(t *testing.T) {
	for _, replace := range []struct {
		name string
		call func(context.Context, *Repository, map[string]any) error
	}{
		{
			name: "config replace",
			call: func(ctx context.Context, repo *Repository, root map[string]any) error {
				return repo.ReplaceConfigSnapshot(ctx, root)
			},
		},
		{
			name: "plugin task config sync",
			call: func(ctx context.Context, repo *Repository, root map[string]any) error {
				_, errReplace := repo.ReplaceConfigSnapshotAndCreatePluginTask(ctx, root, node.PluginTask{Operation: node.PluginTaskOperationDelete, PluginID: "plugin-id"})
				return errReplace
			},
		},
	} {
		for _, testCase := range []struct {
			name      string
			policies  any
			present   bool
			wantClear bool
			wantError bool
		}{
			{name: "missing preserves"},
			{name: "empty clears", policies: map[string]any{}, present: true, wantClear: true},
			{name: "nonempty rejects", policies: map[string]any{"29292929-2929-4292-8292-292929292929": map[string]any{"max-in-flight": 4}}, present: true, wantError: true},
		} {
			t.Run(replace.name+"/"+testCase.name, func(t *testing.T) {
				ctx := context.Background()
				repo := newCredentialFoundationTestRepository(t)
				id := "29292929-2929-4292-8292-292929292929"
				before := seedRetiredProviderConcurrencyPolicy(t, repo, id)
				root := map[string]any{"port": 8327}
				if testCase.present {
					root[credentialConcurrencyPoliciesRootKey] = testCase.policies
				}

				errReplace := replace.call(ctx, repo, root)
				if testCase.wantError {
					if !errors.Is(errReplace, ErrConcurrencyCredentialNotFound) {
						t.Fatalf("replacement error = %v, want ErrConcurrencyCredentialNotFound", errReplace)
					}
					assertRetiredProviderConcurrencyPolicy(t, repo, id, before, false)
					return
				}
				if errReplace != nil {
					t.Fatal(errReplace)
				}
				assertRetiredProviderConcurrencyPolicy(t, repo, id, before, testCase.wantClear)
			})
		}
	}
}

func TestReplaceConfigSnapshotAllowsConcurrencyCredentialSecretRotation(t *testing.T) {
	ctx := context.Background()
	repo := newCredentialFoundationTestRepository(t)
	id := "24242424-2424-4242-8242-242424242424"
	if errReplace := repo.ReplaceConfigSnapshot(ctx, map[string]any{"gemini-api-key": []any{map[string]any{"id": id, "api-key": "gemini-old"}}}); errReplace != nil {
		t.Fatal(errReplace)
	}
	setExchangePolicy(t, repo, id, 2, nil)

	if errReplace := repo.ReplaceConfigSnapshot(ctx, map[string]any{"gemini-api-key": []any{map[string]any{"id": id, "api-key": "gemini-new"}}}); errReplace != nil {
		t.Fatal(errReplace)
	}
	assertProviderCredentialSecrets(t, repo, ctx, map[string]string{id: "gemini-new"})
	assertExchangePolicy(t, repo, id, 2, nil)
}

func TestReplaceConfigSnapshotAndCreatePluginTaskRejectsOrphanedConcurrencyCredential(t *testing.T) {
	ctx := context.Background()
	repo := newCredentialFoundationTestRepository(t)
	id := "25252525-2525-4252-8252-252525252525"
	if errReplace := repo.ReplaceConfigSnapshot(ctx, map[string]any{"gemini-api-key": []any{map[string]any{"id": id, "api-key": "gemini-old"}}}); errReplace != nil {
		t.Fatal(errReplace)
	}
	setExchangePolicy(t, repo, id, 2, nil)

	_, errReplace := repo.ReplaceConfigSnapshotAndCreatePluginTask(ctx, map[string]any{"gemini-api-key": []any{}}, node.PluginTask{
		Operation: node.PluginTaskOperationDelete,
		PluginID:  "plugin-id",
	})
	if !errors.Is(errReplace, ErrCredentialConcurrencyOrphan) {
		t.Fatalf("ReplaceConfigSnapshotAndCreatePluginTask() error = %v, want ErrCredentialConcurrencyOrphan", errReplace)
	}
	assertProviderCredentialSecrets(t, repo, ctx, map[string]string{id: "gemini-old"})
	var taskCount int64
	if errCount := repo.db.Model(&PluginTaskRecord{}).Count(&taskCount).Error; errCount != nil {
		t.Fatal(errCount)
	}
	if taskCount != 0 {
		t.Fatalf("plugin task count = %d, want 0", taskCount)
	}
}

func TestReplaceConfigSnapshotAndCreatePluginTaskReplacesExplicitEmptyProviderRoot(t *testing.T) {
	ctx := context.Background()
	repo := newCredentialFoundationTestRepository(t)
	geminiID := "20202020-2020-4020-8020-202020202020"
	codexID := "21212121-2121-4121-8121-212121212121"
	if errReplace := repo.ReplaceConfigSnapshot(ctx, providerCredentialSnapshotRoot(geminiID, "gemini-old", codexID, "codex-old")); errReplace != nil {
		t.Fatal(errReplace)
	}
	if _, errReplace := repo.ReplaceConfigSnapshotAndCreatePluginTask(ctx, map[string]any{"gemini-api-key": []any{}}, node.PluginTask{
		Operation: node.PluginTaskOperationDelete,
		PluginID:  "plugin-id",
	}); errReplace != nil {
		t.Fatal(errReplace)
	}
	assertProviderCredentialSecrets(t, repo, ctx, map[string]string{codexID: "codex-old"})
}

func providerCredentialSnapshotRoot(geminiID string, geminiKey string, codexID string, codexKey string) map[string]any {
	return map[string]any{
		"gemini-api-key": []any{map[string]any{"id": geminiID, "api-key": geminiKey}},
		"codex-api-key":  []any{map[string]any{"id": codexID, "api-key": codexKey, "base-url": "https://example.test"}},
	}
}

func assertProviderCredentialSecrets(t *testing.T, repo *Repository, ctx context.Context, want map[string]string) {
	t.Helper()
	auths, errAuths := repo.ListAuths(ctx)
	if errAuths != nil {
		t.Fatal(errAuths)
	}
	if len(auths) != len(want) {
		t.Fatalf("active provider auth count = %d, want %d: %#v", len(auths), len(want), auths)
	}
	for _, auth := range auths {
		if auth.Attributes["api_key"] != want[auth.ID] {
			t.Fatalf("auth %s api_key = %q, want %q", auth.ID, auth.Attributes["api_key"], want[auth.ID])
		}
	}
}

func TestReplaceConfigSnapshotRollsBackLifecycleOnCredentialCollision(t *testing.T) {
	ctx := context.Background()
	repo := newCredentialFoundationTestRepository(t)
	if errReplace := repo.ReplaceConfigSnapshot(ctx, map[string]any{"port": 8327}); errReplace != nil {
		t.Fatal(errReplace)
	}
	seedSnapshotCredentialCollision(t, repo, ctx)

	next := config.DefaultCredentialConcurrencyConfig()
	next.CPAHeartbeatTimeout = 4 * time.Second
	errReplace := repo.ReplaceConfigSnapshotWithLifecycleConfig(ctx, 20*time.Second, map[string]any{
		"port":                   8328,
		"credential-concurrency": next,
		"gemini-api-key": []any{map[string]any{
			"id": credentialCollisionID, "api-key": "gemini-key",
		}},
	})
	if !errors.Is(errReplace, ErrCredentialIdentityConflict) {
		t.Fatalf("ReplaceConfigSnapshotWithLifecycleConfig() error = %v, want ErrCredentialIdentityConflict", errReplace)
	}
	assertLifecycleAndSnapshotUnchangedAfterCollision(t, repo, ctx)
}

func TestReplaceConfigSnapshotAndCreatePluginTaskRollsBackLifecycleOnCredentialCollision(t *testing.T) {
	ctx := context.Background()
	repo := newCredentialFoundationTestRepository(t)
	if errReplace := repo.ReplaceConfigSnapshot(ctx, map[string]any{"port": 8327}); errReplace != nil {
		t.Fatal(errReplace)
	}
	seedSnapshotCredentialCollision(t, repo, ctx)

	next := config.DefaultCredentialConcurrencyConfig()
	next.CPAHeartbeatTimeout = 4 * time.Second
	_, errReplace := repo.ReplaceConfigSnapshotWithLifecycleConfigAndCreatePluginTask(ctx, 20*time.Second, map[string]any{
		"port":                   8328,
		"credential-concurrency": next,
		"gemini-api-key": []any{map[string]any{
			"id": credentialCollisionID, "api-key": "gemini-key",
		}},
	}, node.PluginTask{Operation: node.PluginTaskOperationDelete, PluginID: "plugin-id"})
	if !errors.Is(errReplace, ErrCredentialIdentityConflict) {
		t.Fatalf("ReplaceConfigSnapshotWithLifecycleConfigAndCreatePluginTask() error = %v, want ErrCredentialIdentityConflict", errReplace)
	}
	assertLifecycleAndSnapshotUnchangedAfterCollision(t, repo, ctx)
	var taskCount int64
	if errCount := repo.db.Model(&PluginTaskRecord{}).Count(&taskCount).Error; errCount != nil {
		t.Fatal(errCount)
	}
	if taskCount != 0 {
		t.Fatalf("plugin task count = %d, want 0", taskCount)
	}
}

const credentialCollisionID = "77777777-7777-4777-8777-777777777777"

func seedSnapshotCredentialCollision(t *testing.T, repo *Repository, ctx context.Context) {
	t.Helper()
	auth := &coreauth.Auth{
		ID:       credentialCollisionID,
		Index:    credentialCollisionID,
		Provider: "codex",
		Attributes: map[string]string{
			"source":  "config:codex[existing]",
			"api_key": "codex-key",
		},
	}
	if _, errUpsert := repo.UpsertAuth(ctx, auth, "create"); errUpsert != nil {
		t.Fatal(errUpsert)
	}
}

func assertLifecycleAndSnapshotUnchangedAfterCollision(t *testing.T, repo *Repository, ctx context.Context) {
	t.Helper()
	lifecycle, errLifecycle := repo.LifecycleConfig(ctx)
	if errLifecycle != nil {
		t.Fatal(errLifecycle)
	}
	defaults := config.DefaultCredentialConcurrencyConfig()
	if lifecycle.LifecycleConfigRevision != 1 || lifecycle.CPAHeartbeatTimeout != defaults.CPAHeartbeatTimeout {
		t.Fatalf("lifecycle config = %#v, want unchanged revision 1 defaults", lifecycle)
	}
	snapshot, errSnapshot := repo.LoadConfigSnapshot(ctx)
	if errSnapshot != nil {
		t.Fatal(errSnapshot)
	}
	if got := string(snapshot["port"]); got != "8327" {
		t.Fatalf("snapshot port = %s, want 8327", got)
	}
}

func TestReplaceConfigSnapshotReconcilesProviderAuthsWithCredentialID(t *testing.T) {
	ctx := context.Background()
	repo := newCredentialFoundationTestRepository(t)
	id := "55555555-5555-4555-8555-555555555555"
	root := map[string]any{
		"gemini-api-key": []any{map[string]any{
			"id": id, "api-key": "old", "base-url": "https://example.test",
		}},
	}
	if errReplace := repo.ReplaceConfigSnapshot(ctx, root); errReplace != nil {
		t.Fatal(errReplace)
	}
	root["gemini-api-key"] = []any{map[string]any{
		"id": id, "api-key": "new", "base-url": "https://example.test",
	}}
	if errReplace := repo.ReplaceConfigSnapshot(ctx, root); errReplace != nil {
		t.Fatal(errReplace)
	}
	auths, errAuths := repo.ListAuths(ctx)
	if errAuths != nil {
		t.Fatal(errAuths)
	}
	if len(auths) != 1 || auths[0].ID != id || auths[0].Attributes["api_key"] != "new" {
		t.Fatalf("reconciled auths = %#v", auths)
	}
	snapshot, errSnapshot := repo.LoadConfigSnapshot(ctx)
	if errSnapshot != nil {
		t.Fatal(errSnapshot)
	}
	if len(snapshot["gemini-api-key"]) == 0 {
		t.Fatal("credential config was not persisted")
	}
}
