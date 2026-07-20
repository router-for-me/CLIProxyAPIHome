package cluster

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
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
	writeFile(t, filepath.Join(authDir, "codex.json"), `{"type":"codex","email":"a@example.com","access_token":"token","max_in_flight":4,"max_in_flight_by_model":{"gpt-5.5":2}}`)

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
	auths, errAuths := repo.ListAuths(context.Background())
	if errAuths != nil {
		t.Fatalf("ListAuths() error = %v", errAuths)
	}
	var codexAuth *coreauth.Auth
	for _, auth := range auths {
		if auth != nil && auth.Provider == "codex" {
			codexAuth = auth
			break
		}
	}
	if codexAuth == nil || codexAuth.MaxInFlight != 4 || codexAuth.MaxInFlightByModel["gpt-5.5"] != 2 {
		t.Fatalf("imported codex concurrency limits = %#v", codexAuth)
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
	exportedAuthPath := filepath.Join(homeDir, ".cli-proxy-api", "codex.json")
	assertFileContains(t, exportedAuthPath, `"max_in_flight": 4`)
	assertFileContains(t, exportedAuthPath, `"gpt-5.5": 2`)
	assertFileNotContains(t, exportedAuthPath, `"in_flight"`)
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
	exportedAuthPath := filepath.Join(outputDir, "auths", "codex.json")
	assertFileContains(t, exportedAuthPath, `"max_in_flight": 4`)
	assertFileContains(t, exportedAuthPath, `"gpt-5.5": 2`)
	assertFileNotContains(t, exportedAuthPath, `"in_flight"`)
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
		ID:                 "codex-auth",
		Index:              "codex-auth",
		Provider:           "codex",
		Status:             coreauth.StatusActive,
		MaxInFlight:        4,
		MaxInFlightByModel: map[string]int{"gpt-5.5": 2},
		Metadata: map[string]any{
			"type":      "codex",
			"filename":  "codex.json",
			"token":     "test-token",
			"in_flight": 99,
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

func assertFileNotContains(t *testing.T, path string, unwanted string) {
	t.Helper()

	raw, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatalf("read %s: %v", path, errRead)
	}
	if strings.Contains(string(raw), unwanted) {
		t.Fatalf("%s unexpectedly contains %q:\n%s", path, unwanted, string(raw))
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
