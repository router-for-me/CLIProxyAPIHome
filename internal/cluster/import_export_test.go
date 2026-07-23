package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"gorm.io/gorm"
)

func TestImportLocalState_IsIdempotent(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	authDir := filepath.Join(dir, "auth")
	if errMk := os.MkdirAll(authDir, 0o700); errMk != nil {
		t.Fatal(errMk)
	}
	writeFile(t, configPath, `
port: 8327
auth-dir: auth
api-keys:
  - user-key
gemini-api-key:
  - api-key: gemini-key
    models:
      - name: gemini-2.5-pro
xai-api-key:
  - api-key: xai-key
    base-url: https://api.x.ai/v1
    models:
      - name: grok-4.5
        alias: grok-latest
`)
	writeFile(t, filepath.Join(authDir, "codex.json"), `{"type":"codex","email":"a@example.com","access_token":"token"}`)

	db := openImportTestSQLite(t)
	repo := NewRepository(db)
	opts := ImportOptions{ConfigPath: configPath, AuthDir: authDir, Repository: repo, Now: time.Unix(100, 0)}

	first, errFirst := ImportLocalState(context.Background(), opts)
	if errFirst != nil {
		t.Fatalf("first import error = %v", errFirst)
	}
	firstEventCount := assertTableCount(t, db, &ClusterEventRecord{}, -1)
	second, errSecond := ImportLocalState(context.Background(), opts)
	if errSecond != nil {
		t.Fatalf("second import error = %v", errSecond)
	}
	secondEventCount := assertTableCount(t, db, &ClusterEventRecord{}, -1)

	if first.Created == 0 {
		t.Fatalf("first import Created = 0, want created rows")
	}
	if second.Created != 0 || second.Unchanged == 0 {
		t.Fatalf("second import stats = %+v, want no created and some unchanged", second)
	}
	if secondEventCount != firstEventCount {
		t.Fatalf("cluster_events count after second import = %d, want %d", secondEventCount, firstEventCount)
	}
	assertTableCount(t, db, &APIKeyRecord{}, 1)
	assertActiveAuthCount(t, db, 3)
}

func TestImportLocalStateEmitsOneConfigEventForHotOnlyCredentialConcurrencyChange(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	writeFile(t, configPath, "credential-concurrency:\n  release-flush-interval: 500ms\n")

	db := openImportTestSQLite(t)
	repo := NewRepository(db)
	if _, errEnsure := repo.EnsureLifecycleConfig(ctx, DefaultHeartbeatTimeout()); errEnsure != nil {
		t.Fatal(errEnsure)
	}
	before := assertTableCount(t, db, &ClusterEventRecord{}, -1)

	if _, errImport := ImportLocalState(ctx, ImportOptions{ConfigPath: configPath, Repository: repo}); errImport != nil {
		t.Fatal(errImport)
	}
	var events []ClusterEventRecord
	if errFind := db.Order("id ASC").Find(&events).Error; errFind != nil {
		t.Fatal(errFind)
	}
	if int64(len(events)) != before+1 {
		t.Fatalf("cluster events after hot-only import = %d, want %d", len(events), before+1)
	}
	event := events[len(events)-1]
	if event.Scope != "config" || event.Op != "update" || event.EntityUUID != "credential-concurrency" {
		t.Fatalf("hot-only import event = %#v, want credential-concurrency config update", event)
	}

	if _, errImport := ImportLocalState(ctx, ImportOptions{ConfigPath: configPath, Repository: repo}); errImport != nil {
		t.Fatal(errImport)
	}
	assertTableCount(t, db, &ClusterEventRecord{}, before+1)
}

func TestImportLocalStateCountsProviderCredentialReconciliation(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	writeFile(t, configPath, "gemini-api-key:\n  - id: 78787878-7878-4787-8787-787878787878\n    api-key: gemini\n")
	repo := NewRepository(openImportTestSQLite(t))
	opts := ImportOptions{ConfigPath: configPath, Repository: repo}

	first, errFirst := ImportLocalState(context.Background(), opts)
	if errFirst != nil {
		t.Fatal(errFirst)
	}
	if first.Created != 1 {
		t.Fatalf("first import stats = %+v, want one created provider credential", first)
	}
	second, errSecond := ImportLocalState(context.Background(), opts)
	if errSecond != nil {
		t.Fatal(errSecond)
	}
	if second.Unchanged != 1 {
		t.Fatalf("second import stats = %+v, want one unchanged provider credential", second)
	}
	writeFile(t, configPath, "gemini-api-key:\n  - id: 78787878-7878-4787-8787-787878787878\n    api-key: rotated\n")
	updated, errUpdated := ImportLocalState(context.Background(), opts)
	if errUpdated != nil {
		t.Fatal(errUpdated)
	}
	if updated.Updated != 1 {
		t.Fatalf("updated import stats = %+v, want one updated provider credential", updated)
	}
	if errRetire := repo.RetireProviderAuth(context.Background(), "78787878-7878-4787-8787-787878787878"); errRetire != nil {
		t.Fatal(errRetire)
	}
	restored, errRestored := ImportLocalState(context.Background(), opts)
	if errRestored != nil {
		t.Fatal(errRestored)
	}
	if restored.Restored != 1 {
		t.Fatalf("restored import stats = %+v, want one restored provider credential", restored)
	}
}

func TestImportLocalStateCountsUnchangedGeneratedProviderCredential(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	writeFile(t, configPath, "gemini-api-key:\n  - api-key: gemini\n")
	repo := NewRepository(openImportTestSQLite(t))
	opts := ImportOptions{ConfigPath: configPath, Repository: repo}

	if _, errImport := ImportLocalState(context.Background(), opts); errImport != nil {
		t.Fatal(errImport)
	}
	second, errImport := ImportLocalState(context.Background(), opts)
	if errImport != nil {
		t.Fatal(errImport)
	}
	if second.Unchanged != 1 || second.Updated != 0 {
		t.Fatalf("second import stats = %+v, want one unchanged generated provider credential", second)
	}
}

func TestImportLocalStateRejectsDuplicateProviderCredentialIDs(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	id := "77777777-7777-4777-8777-777777777777"
	writeFile(t, configPath, "gemini-api-key:\n  - id: "+id+"\n    api-key: gemini\ncodex-api-key:\n  - id: "+id+"\n    api-key: codex\n    base-url: https://example.test\n")

	_, errImport := ImportLocalState(context.Background(), ImportOptions{ConfigPath: configPath, Repository: NewRepository(openImportTestSQLite(t))})
	if !errors.Is(errImport, ErrCredentialIdentityConflict) {
		t.Fatalf("ImportLocalState() error = %v, want ErrCredentialIdentityConflict", errImport)
	}
}

func TestImportLocalStateRejectsExistingCredentialIdentityCollisions(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		config   string
		retire   bool
	}{
		{
			name:     "active oauth",
			provider: "codex",
			config:   "gemini-api-key:\n  - id: 88888888-8888-4888-8888-888888888888\n    api-key: gemini\n",
		},
		{
			name:     "retired provider from another lineage",
			provider: "gemini",
			retire:   true,
			config:   "codex-api-key:\n  - id: 88888888-8888-4888-8888-888888888888\n    api-key: codex\n    base-url: https://example.test\n",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			configPath := filepath.Join(dir, "config.yaml")
			writeFile(t, configPath, tc.config)
			repo := NewRepository(openImportTestSQLite(t))
			id := "88888888-8888-4888-8888-888888888888"
			existing := &coreauth.Auth{ID: id, Index: id, Provider: tc.provider, Attributes: map[string]string{"source": "oauth:existing", "api_key": "existing"}}
			if _, errUpsert := repo.UpsertAuth(context.Background(), existing, "create"); errUpsert != nil {
				t.Fatal(errUpsert)
			}
			if tc.retire {
				if errRetire := repo.RetireProviderAuth(context.Background(), id); errRetire != nil {
					t.Fatal(errRetire)
				}
			}

			_, errImport := ImportLocalState(context.Background(), ImportOptions{ConfigPath: configPath, Repository: repo})
			if !errors.Is(errImport, ErrCredentialIdentityConflict) {
				t.Fatalf("ImportLocalState() error = %v, want ErrCredentialIdentityConflict", errImport)
			}
		})
	}
}

func TestImportLocalStateRollsBackLifecycleOnCredentialCollision(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	id := "99999999-9999-4999-8999-999999999999"
	writeFile(t, configPath, "credential-concurrency:\n  cpa-heartbeat-timeout: 4s\ngemini-api-key:\n  - id: "+id+"\n    api-key: gemini\n")
	repo := NewRepository(openImportTestSQLite(t))
	if _, errEnsure := repo.EnsureLifecycleConfig(ctx, 20*time.Second); errEnsure != nil {
		t.Fatal(errEnsure)
	}
	if _, errUpsert := repo.UpsertAuth(ctx, &coreauth.Auth{
		ID:       id,
		Index:    id,
		Provider: "codex",
		Attributes: map[string]string{
			"source":  "oauth:existing",
			"api_key": "codex",
		},
	}, "create"); errUpsert != nil {
		t.Fatal(errUpsert)
	}

	_, errImport := ImportLocalState(ctx, ImportOptions{ConfigPath: configPath, Repository: repo, NodeHeartbeatTimeout: 20 * time.Second})
	if !errors.Is(errImport, ErrCredentialIdentityConflict) {
		t.Fatalf("ImportLocalState() error = %v, want ErrCredentialIdentityConflict", errImport)
	}
	lifecycle, errLifecycle := repo.LifecycleConfig(ctx)
	if errLifecycle != nil {
		t.Fatal(errLifecycle)
	}
	if lifecycle.LifecycleConfigRevision != 1 || lifecycle.CPAHeartbeatTimeout != config.DefaultCPAHeartbeatTimeout {
		t.Fatalf("lifecycle config = %#v, want unchanged revision 1 defaults", lifecycle)
	}
}

func TestRepositoryUpsertResult_UsesSemanticJSONEquality(t *testing.T) {
	db := openImportTestSQLite(t)
	repo := NewRepository(db)
	ctx := context.Background()

	configRecord := ConfigRecord{
		Key:     "semantic-config",
		Value:   JSONB(`{"z":2,"a":{"b":true,"a":1}}`),
		Version: 1,
	}
	if errCreateConfig := db.Create(&configRecord).Error; errCreateConfig != nil {
		t.Fatalf("create config record: %v", errCreateConfig)
	}

	configEventCount := assertTableCount(t, db, &ClusterEventRecord{}, -1)
	configResult, errUpsertConfigValue := repo.UpsertConfigValueWithResult(ctx, "semantic-config", map[string]any{
		"a": map[string]any{
			"a": 1,
			"b": true,
		},
		"z": 2,
	})
	if errUpsertConfigValue != nil {
		t.Fatalf("UpsertConfigValueWithResult() error = %v", errUpsertConfigValue)
	}
	if configResult != UpsertResultUnchanged {
		t.Fatalf("config upsert result = %s, want %s", configResult, UpsertResultUnchanged)
	}
	assertTableCount(t, db, &ClusterEventRecord{}, configEventCount)

	auth := &coreauth.Auth{
		ID:       "semantic-auth",
		Index:    "semantic-auth",
		Provider: "gemini",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"a": "1",
			"b": "2",
		},
	}
	authRecord, errAuthRecord := AuthToRecord(auth)
	if errAuthRecord != nil {
		t.Fatalf("AuthToRecord() error = %v", errAuthRecord)
	}
	authRecord.AuthJSON = reorderObjectJSON(t, authRecord.AuthJSON)
	if errCreateAuth := db.Create(authRecord).Error; errCreateAuth != nil {
		t.Fatalf("create auth record: %v", errCreateAuth)
	}

	authEventCount := assertTableCount(t, db, &ClusterEventRecord{}, -1)
	_, authResult, errUpsertAuth := repo.UpsertAuthWithResult(ctx, auth, "upsert")
	if errUpsertAuth != nil {
		t.Fatalf("UpsertAuthWithResult() error = %v", errUpsertAuth)
	}
	if authResult != UpsertResultUnchanged {
		t.Fatalf("auth upsert result = %s, want %s", authResult, UpsertResultUnchanged)
	}
	assertTableCount(t, db, &ClusterEventRecord{}, authEventCount)
}

func TestExportLocalState_RefusesExistingTargets(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(t *testing.T, dir string)
		options func(dir string, repo *Repository) ExportOptions
		want    string
	}{
		{
			name: "existing config",
			setup: func(t *testing.T, dir string) {
				writeFile(t, filepath.Join(dir, "config.yaml"), "port: 1\n")
			},
			options: func(dir string, repo *Repository) ExportOptions {
				return ExportOptions{
					OutputDir:   dir,
					Repository:  repo,
					ConfigName:  "config.yaml",
					AuthDirName: "auth",
				}
			},
			want: "config.yaml already exists",
		},
		{
			name: "non-empty auth dir",
			setup: func(t *testing.T, dir string) {
				authDir := filepath.Join(dir, "auth")
				if errMkdirAll := os.MkdirAll(authDir, 0o700); errMkdirAll != nil {
					t.Fatal(errMkdirAll)
				}
				writeFile(t, filepath.Join(authDir, "codex.json"), `{"type":"codex"}`)
			},
			options: func(dir string, repo *Repository) ExportOptions {
				return ExportOptions{
					OutputDir:   dir,
					Repository:  repo,
					ConfigName:  "config.yaml",
					AuthDirName: "auth",
				}
			},
			want: "already exists and is not empty",
		},
		{
			name: "escaping config name",
			options: func(dir string, repo *Repository) ExportOptions {
				return ExportOptions{
					OutputDir:   dir,
					Repository:  repo,
					ConfigName:  "../config.yaml",
					AuthDirName: "auth",
				}
			},
			want: "ConfigName",
		},
		{
			name: "escaping auth dir name",
			options: func(dir string, repo *Repository) ExportOptions {
				return ExportOptions{
					OutputDir:   dir,
					Repository:  repo,
					ConfigName:  "config.yaml",
					AuthDirName: "../auth",
				}
			},
			want: "AuthDirName",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			db := openImportTestSQLite(t)
			repo := NewRepository(db)
			if errUpsert := repo.UpsertConfigValue(context.Background(), "port", 8327); errUpsert != nil {
				t.Fatal(errUpsert)
			}
			if tc.setup != nil {
				tc.setup(t, dir)
			}

			_, errExport := ExportLocalState(context.Background(), tc.options(dir, repo))
			if errExport == nil || !strings.Contains(errExport.Error(), tc.want) {
				t.Fatalf("ExportLocalState() error = %v, want error containing %q", errExport, tc.want)
			}
		})
	}
}

func TestExportLocalState_DefaultWritesConfigToCurrentDirAndAuthToHome(t *testing.T) {
	rootDir := t.TempDir()
	homeDir := filepath.Join(rootDir, "home")
	workDir := filepath.Join(rootDir, "work")
	if errMkdirAll := os.MkdirAll(workDir, 0o700); errMkdirAll != nil {
		t.Fatal(errMkdirAll)
	}
	t.Setenv("HOME", homeDir)
	t.Setenv("USERPROFILE", homeDir)
	t.Chdir(workDir)

	db := openImportTestSQLite(t)
	repo := NewRepository(db)
	seedExportState(t, repo)

	stats, errExport := ExportLocalState(context.Background(), ExportOptions{Repository: repo})
	if errExport != nil {
		t.Fatalf("ExportLocalState() error = %v", errExport)
	}
	if stats.AuthFiles != 1 {
		t.Fatalf("ExportLocalState() AuthFiles = %d, want 1", stats.AuthFiles)
	}
	assertFileContains(t, filepath.Join(workDir, "config.yaml"), "auth-dir: ~/.cli-proxy-api")
	assertFileExists(t, filepath.Join(homeDir, ".cli-proxy-api", "codex.json"))
}

func TestExportLocalState_CustomOutputDirWritesAuthsUnderOutputDir(t *testing.T) {
	outputDir := t.TempDir()
	db := openImportTestSQLite(t)
	repo := NewRepository(db)
	seedExportState(t, repo)

	stats, errExport := ExportLocalState(context.Background(), ExportOptions{
		OutputDir:   outputDir,
		Repository:  repo,
		AuthDirName: "auths",
	})
	if errExport != nil {
		t.Fatalf("ExportLocalState() error = %v", errExport)
	}
	if stats.AuthFiles != 1 {
		t.Fatalf("ExportLocalState() AuthFiles = %d, want 1", stats.AuthFiles)
	}
	assertFileContains(t, filepath.Join(outputDir, "config.yaml"), "auth-dir: auths")
	assertFileExists(t, filepath.Join(outputDir, "auths", "codex.json"))
}

func openImportTestSQLite(t *testing.T) *gorm.DB {
	t.Helper()

	db, errOpenSQLite := OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "home.db"))
	if errOpenSQLite != nil {
		t.Fatalf("OpenSQLite() error = %v", errOpenSQLite)
	}
	sqlDB, errDB := db.DB()
	if errDB != nil {
		t.Fatalf("DB() error = %v", errDB)
	}
	t.Cleanup(func() {
		if errClose := sqlDB.Close(); errClose != nil {
			t.Errorf("close sqlite db: %v", errClose)
		}
	})
	if errMigrate := AutoMigrate(db); errMigrate != nil {
		t.Fatalf("AutoMigrate() error = %v", errMigrate)
	}
	return db
}

func writeFile(t *testing.T, path string, payload string) {
	t.Helper()

	if errWrite := os.WriteFile(path, []byte(payload), 0o600); errWrite != nil {
		t.Fatalf("write %s: %v", path, errWrite)
	}
}

func seedExportState(t *testing.T, repo *Repository) {
	t.Helper()

	ctx := context.Background()
	if errUpsert := repo.UpsertConfigValue(ctx, "port", 8327); errUpsert != nil {
		t.Fatal(errUpsert)
	}
	auth := &coreauth.Auth{
		ID:       "codex-auth",
		Index:    "codex-auth",
		Provider: "codex",
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{
			"type":     "codex",
			"filename": "codex.json",
			"token":    "test-token",
		},
	}
	if _, _, errUpsertAuth := repo.UpsertAuthWithResult(ctx, auth, "upsert"); errUpsertAuth != nil {
		t.Fatal(errUpsertAuth)
	}
}

func assertFileContains(t *testing.T, path string, want string) {
	t.Helper()

	raw, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatalf("read %s: %v", path, errRead)
	}
	if !strings.Contains(string(raw), want) {
		t.Fatalf("%s does not contain %q:\n%s", path, want, string(raw))
	}
}

func assertFileExists(t *testing.T, path string) {
	t.Helper()

	info, errStat := os.Stat(path)
	if errStat != nil {
		t.Fatalf("stat %s: %v", path, errStat)
	}
	if info.IsDir() {
		t.Fatalf("%s is a directory, want file", path)
	}
}

func assertTableCount(t *testing.T, db *gorm.DB, model any, want int64) int64 {
	t.Helper()

	var count int64
	if errCount := db.Model(model).Count(&count).Error; errCount != nil {
		t.Fatalf("count table %T: %v", model, errCount)
	}
	if want >= 0 && count != want {
		t.Fatalf("table %T count = %d, want %d", model, count, want)
	}
	return count
}

func assertActiveAuthCount(t *testing.T, db *gorm.DB, want int64) {
	t.Helper()

	assertTableCount(t, db, &AuthRecord{}, want)
}

func reorderObjectJSON(t *testing.T, raw []byte) JSONB {
	t.Helper()

	var object map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(raw, &object); errUnmarshal != nil {
		t.Fatalf("unmarshal object json: %v", errUnmarshal)
	}
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(keys)))

	var builder strings.Builder
	builder.WriteByte('{')
	for i, key := range keys {
		if i > 0 {
			builder.WriteByte(',')
		}
		keyJSON, errMarshalKey := json.Marshal(key)
		if errMarshalKey != nil {
			t.Fatalf("marshal json key: %v", errMarshalKey)
		}
		builder.Write(keyJSON)
		builder.WriteByte(':')
		builder.Write(object[key])
	}
	builder.WriteByte('}')
	return JSONB(builder.String())
}
