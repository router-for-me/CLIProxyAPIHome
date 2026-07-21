package cluster

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func TestDatabaseSnapshotModelRegistryMatchesMigrationModels(t *testing.T) {
	t.Parallel()

	migrationModels := databaseMigrationModels()
	if len(migrationModels) != len(homeDatabaseModels) {
		t.Fatalf("migration model count = %d, snapshot model count = %d", len(migrationModels), len(homeDatabaseModels))
	}
	seenNames := make(map[string]struct{}, len(homeDatabaseModels))
	seenTypes := make(map[reflect.Type]struct{}, len(homeDatabaseModels))
	for index, model := range homeDatabaseModels {
		if model.name == "" || model.newRecord == nil || model.newBatch == nil || len(model.orderBy) == 0 {
			t.Fatalf("database model %d is incomplete: %#v", index, model)
		}
		if _, duplicate := seenNames[model.name]; duplicate {
			t.Fatalf("database model table %q is registered more than once", model.name)
		}
		seenNames[model.name] = struct{}{}

		recordType := reflect.TypeOf(model.newRecord())
		if recordType == nil || recordType.Kind() != reflect.Ptr {
			t.Fatalf("database model %s record type = %v", model.name, recordType)
		}
		if _, duplicate := seenTypes[recordType]; duplicate {
			t.Fatalf("database model type %v is registered more than once", recordType)
		}
		seenTypes[recordType] = struct{}{}
		if gotType := reflect.TypeOf(migrationModels[index]); gotType != recordType {
			t.Fatalf("migration model %d type = %v, snapshot type = %v", index, gotType, recordType)
		}
		batchType := reflect.TypeOf(model.newBatch())
		if batchType == nil || batchType.Kind() != reflect.Ptr || batchType.Elem().Kind() != reflect.Slice || batchType.Elem().Elem() != recordType.Elem() {
			t.Fatalf("database model %s batch type = %v, want pointer to slice of %v", model.name, batchType, recordType.Elem())
		}
		tableNamer, okTableNamer := model.newRecord().(interface{ TableName() string })
		if !okTableNamer {
			t.Fatalf("database model %s does not implement TableName()", model.name)
		}
		if tableNamer.TableName() != model.name {
			t.Fatalf("database model %s TableName() = %q", model.name, tableNamer.TableName())
		}
	}
	for _, required := range []any{
		&UserSecurityTokenRecord{},
		&UserMailJobRecord{},
		&UserSecurityThrottleRecord{},
	} {
		requiredType := reflect.TypeOf(required)
		if _, exists := seenTypes[requiredType]; !exists {
			t.Errorf("required migration model %v is not registered", requiredType)
		}
	}
}

func TestDatabaseSnapshotSQLiteRoundTrip(t *testing.T) {
	ctx := context.Background()
	source := openDatabaseSnapshotSQLiteTestDB(t, filepath.Join(t.TempDir(), "source.db"))
	want := seedDatabaseSnapshotTestData(t, source)
	snapshotPath := filepath.Join(t.TempDir(), "home.snapshot.zip")

	manifest, errExport := ExportDatabaseSnapshot(ctx, source, DatabaseSnapshotExportOptions{
		Path:        snapshotPath,
		HomeVersion: "test-version",
		HomeCommit:  "test-commit",
	})
	if errExport != nil {
		t.Fatalf("ExportDatabaseSnapshot() error = %v", errExport)
	}
	if manifest.SourceBackend != DatabaseBackendSQLite || manifest.HomeVersion != "test-version" || manifest.HomeCommit != "test-commit" {
		t.Fatalf("export manifest = %#v", manifest)
	}
	if len(manifest.Tables) != len(homeDatabaseModels) {
		t.Fatalf("export table count = %d, want %d", len(manifest.Tables), len(homeDatabaseModels))
	}

	snapshot, errOpen := OpenDatabaseSnapshot(ctx, snapshotPath)
	if errOpen != nil {
		t.Fatalf("OpenDatabaseSnapshot() error = %v", errOpen)
	}
	t.Cleanup(func() {
		if errClose := snapshot.Close(); errClose != nil {
			t.Errorf("close snapshot: %v", errClose)
		}
	})
	target := openDatabaseSnapshotSQLiteRawTestDB(t, filepath.Join(t.TempDir(), "target.db"))
	result, errImport := ImportDatabaseSnapshot(ctx, target, snapshot, nil)
	if errImport != nil {
		t.Fatalf("ImportDatabaseSnapshot() error = %v", errImport)
	}
	assertDatabaseSnapshotResultCounts(t, result, map[string]DatabaseSnapshotImportTableResult{
		"kv_store":               {Name: "kv_store", Imported: 1, Skipped: 2},
		"user_security_token":    {Name: "user_security_token", Skipped: 1},
		"user_mail_job":          {Name: "user_mail_job", Skipped: 1},
		"user_security_throttle": {Name: "user_security_throttle", Skipped: 1},
		"plugin_status":          {Name: "plugin_status", Skipped: 1},
		"plugin_tasks":           {Name: "plugin_tasks", Skipped: 1},
		"cluster":                {Name: "cluster", Skipped: 1},
		"cpa_node":               {Name: "cpa_node", Skipped: 1},
		"cluster_events":         {Name: "cluster_events", Skipped: 1},
		"oauth_sessions":         {Name: "oauth_sessions", Skipped: 1},
	})

	var auth AuthRecord
	if errFirst := target.Unscoped().First(&auth, "uuid = ?", want.authUUID).Error; errFirst != nil {
		t.Fatalf("load imported auth: %v", errFirst)
	}
	if string(auth.AuthJSON) != `{"token":"secret","nested":{"ok":true}}` || !auth.DeletedAt.Valid {
		t.Fatalf("imported auth = %#v", auth)
	}
	if auth.CreatedAt.Location() != time.UTC || auth.UpdatedAt.Location() != time.UTC {
		t.Fatalf("imported auth timestamps are not UTC: created=%v updated=%v", auth.CreatedAt, auth.UpdatedAt)
	}
	var pluginAuth PluginStoreAuthRecord
	if errFirst := target.First(&pluginAuth, want.pluginAuthID).Error; errFirst != nil {
		t.Fatalf("load imported plugin auth: %v", errFirst)
	}
	if pluginAuth.Enabled {
		t.Fatal("imported plugin auth is enabled, want disabled")
	}
	if !reflect.DeepEqual(pluginAuth.EncryptedCredentials, []byte{0, 1, 2, 253, 254, 255}) {
		t.Fatalf("imported plugin encrypted bytes = %v", pluginAuth.EncryptedCredentials)
	}
	var proxyPool ProxyPoolRecord
	if errFirst := target.First(&proxyPool, "id = ?", "proxy-1").Error; errFirst != nil {
		t.Fatalf("load imported proxy pool: %v", errFirst)
	}
	if proxyPool.Enabled {
		t.Fatal("imported proxy pool is enabled, want disabled")
	}
	var kvRecords []KVRecord
	if errFind := target.Order("key ASC").Find(&kvRecords).Error; errFind != nil {
		t.Fatalf("list imported kv records: %v", errFind)
	}
	if len(kvRecords) != 1 || kvRecords[0].Key != "ordinary:valid" || !reflect.DeepEqual(kvRecords[0].Value, []byte{0, 1, 2, 255}) {
		t.Fatalf("imported kv records = %#v", kvRecords)
	}
	var quota QuotaSnapshotRecord
	if errFirst := target.First(&quota, "credential_id = ?", want.authUUID).Error; errFirst != nil {
		t.Fatalf("load imported quota: %v", errFirst)
	}
	if quota.ProbeLeaseOwner != "old-home" || quota.ProbeLeaseExpiresAt == nil {
		t.Fatalf("imported quota lease fields = owner %q expires %v", quota.ProbeLeaseOwner, quota.ProbeLeaseExpiresAt)
	}
	assertDatabaseSnapshotRuntimeTablesEmpty(t, target)

	createdUser := UserRecord{Username: "next-user"}
	if errCreate := target.Create(&createdUser).Error; errCreate != nil {
		t.Fatalf("create user after import: %v", errCreate)
	}
	if createdUser.ID <= want.maximumUserID {
		t.Fatalf("user id after import = %d, want greater than %d", createdUser.ID, want.maximumUserID)
	}
	createdLog := AppLogRecord{Timestamp: time.Now().UTC(), Line: "after import", CreatedAt: time.Now().UTC()}
	if errCreate := target.Create(&createdLog).Error; errCreate != nil {
		t.Fatalf("create log after import: %v", errCreate)
	}
	if createdLog.ID <= want.maximumLogID {
		t.Fatalf("log id after import = %d, want greater than %d", createdLog.ID, want.maximumLogID)
	}
}

func TestDatabaseSnapshotExportRejectsExistingFile(t *testing.T) {
	t.Parallel()

	db := openDatabaseSnapshotSQLiteTestDB(t, filepath.Join(t.TempDir(), "source.db"))
	path := filepath.Join(t.TempDir(), "existing.zip")
	if errWrite := os.WriteFile(path, []byte("keep"), 0o600); errWrite != nil {
		t.Fatalf("write existing target: %v", errWrite)
	}
	_, errExport := ExportDatabaseSnapshot(context.Background(), db, DatabaseSnapshotExportOptions{Path: path})
	if errExport == nil || !strings.Contains(errExport.Error(), "already exists") {
		t.Fatalf("ExportDatabaseSnapshot() error = %v, want existing target error", errExport)
	}
	raw, errRead := os.ReadFile(path)
	if errRead != nil || string(raw) != "keep" {
		t.Fatalf("existing target changed: data=%q error=%v", raw, errRead)
	}
}

func TestDatabaseSnapshotExportRejectsInvalidUTF8(t *testing.T) {
	t.Parallel()

	db := openDatabaseSnapshotSQLiteTestDB(t, filepath.Join(t.TempDir(), "invalid-utf8.db"))
	now := time.Now().UTC()
	if errInsert := db.Exec(`INSERT INTO "log" ("timestamp", "level", "line", "created_at") VALUES (?, ?, CAST(X'6265666F7265FF6166746572' AS TEXT), ?)`, now, "info", now).Error; errInsert != nil {
		t.Fatalf("insert invalid UTF-8 log row: %v", errInsert)
	}
	stored := AppLogRecord{}
	if errFirst := db.First(&stored).Error; errFirst != nil {
		t.Fatalf("load invalid UTF-8 log row: %v", errFirst)
	}
	if utf8.ValidString(stored.Line) {
		t.Fatalf("stored log line %q is valid UTF-8, want invalid fixture", stored.Line)
	}

	path := filepath.Join(t.TempDir(), "invalid-utf8.zip")
	_, errExport := ExportDatabaseSnapshot(context.Background(), db, DatabaseSnapshotExportOptions{Path: path})
	if errExport == nil || !strings.Contains(errExport.Error(), "field line contains invalid UTF-8") {
		t.Fatalf("ExportDatabaseSnapshot() error = %v, want invalid UTF-8 field error", errExport)
	}
	if _, errStat := os.Stat(path); !os.IsNotExist(errStat) {
		t.Fatalf("invalid UTF-8 export target exists, stat error = %v", errStat)
	}
}

func TestDatabaseSnapshotExportRejectsUnpairedJSONSurrogate(t *testing.T) {
	t.Parallel()

	db := openDatabaseSnapshotSQLiteTestDB(t, filepath.Join(t.TempDir(), "unpaired-surrogate.db"))
	now := time.Now().UTC()
	record := ConfigRecord{Key: "invalid-surrogate", Value: JSONB(`{"value":"\uD800"}`), CreatedAt: now, UpdatedAt: now}
	if errCreate := db.Create(&record).Error; errCreate != nil {
		t.Fatalf("create config with unpaired JSON surrogate: %v", errCreate)
	}

	path := filepath.Join(t.TempDir(), "unpaired-surrogate.zip")
	_, errExport := ExportDatabaseSnapshot(context.Background(), db, DatabaseSnapshotExportOptions{Path: path})
	if errExport == nil || !strings.Contains(errExport.Error(), "field value contains invalid JSON encoding: JSON contains an unpaired high surrogate escape") {
		t.Fatalf("ExportDatabaseSnapshot() error = %v, want unpaired JSON surrogate field error", errExport)
	}
	if _, errStat := os.Stat(path); !os.IsNotExist(errStat) {
		t.Fatalf("unpaired JSON surrogate export target exists, stat error = %v", errStat)
	}
}

func TestDatabaseSnapshotLargeTableExportStreamsRows(t *testing.T) {
	db := openDatabaseSnapshotSQLiteTestDB(t, filepath.Join(t.TempDir(), "large.db"))
	now := time.Now().UTC()
	const rowCount = 5000
	for start := 0; start < rowCount; start += databaseSnapshotBatchSize {
		end := start + databaseSnapshotBatchSize
		if end > rowCount {
			end = rowCount
		}
		records := make([]AppLogRecord, 0, end-start)
		for index := start; index < end; index++ {
			records = append(records, AppLogRecord{Timestamp: now, Level: "info", Line: "streamed row", CreatedAt: now})
		}
		if errCreate := db.Create(&records).Error; errCreate != nil {
			t.Fatalf("seed large log batch %d: %v", start/databaseSnapshotBatchSize, errCreate)
		}
	}
	var logModel databaseModel
	for _, model := range homeDatabaseModels {
		if model.name == "log" {
			logModel = model
			break
		}
	}
	if logModel.newRecord == nil {
		t.Fatal("log database model is not registered")
	}
	tracker := &databaseSnapshotWriteTracker{}
	errTransaction := db.Transaction(func(tx *gorm.DB) error {
		rows, errExport := exportDatabaseSnapshotTable(context.Background(), tx, logModel, tracker)
		if errExport != nil {
			return errExport
		}
		if rows != rowCount {
			t.Fatalf("exported log rows = %d, want %d", rows, rowCount)
		}
		return nil
	})
	if errTransaction != nil {
		t.Fatalf("stream large log table: %v", errTransaction)
	}
	if tracker.writes != rowCount {
		t.Fatalf("writer calls = %d, want one per row (%d)", tracker.writes, rowCount)
	}
	if tracker.maximumWrite >= tracker.totalBytes || tracker.maximumWrite > 4096 {
		t.Fatalf("writer maximum chunk = %d total = %d, want bounded per-row writes", tracker.maximumWrite, tracker.totalBytes)
	}
}

func TestDatabaseSnapshotValidationRejectsCorruption(t *testing.T) {
	t.Parallel()

	sourcePath := createDatabaseSnapshotValidationFixture(t)
	tests := []struct {
		name      string
		build     func(t *testing.T, source string, target string)
		wantError string
	}{
		{
			name: "broken zip",
			build: func(t *testing.T, _ string, target string) {
				t.Helper()
				if errWrite := os.WriteFile(target, []byte("not a zip"), 0o600); errWrite != nil {
					t.Fatalf("write broken zip: %v", errWrite)
				}
			},
			wantError: "open database snapshot archive",
		},
		{
			name: "checksum mismatch",
			build: func(t *testing.T, source string, target string) {
				t.Helper()
				rewriteDatabaseSnapshotZIP(t, source, target, func(name string, raw []byte) []byte {
					if name == databaseSnapshotTableEntryName("auth") {
						if len(raw) > 0 && raw[len(raw)-1] == '\n' {
							return append(append(append([]byte(nil), raw[:len(raw)-1]...), ' '), '\n')
						}
					}
					return raw
				}, "")
			},
			wantError: "checksum mismatch",
		},
		{
			name: "future format",
			build: func(t *testing.T, source string, target string) {
				t.Helper()
				rewriteDatabaseSnapshotZIP(t, source, target, func(name string, raw []byte) []byte {
					if name != databaseSnapshotManifestName {
						return raw
					}
					manifest := DatabaseSnapshotManifest{}
					if errDecode := json.Unmarshal(raw, &manifest); errDecode != nil {
						t.Fatalf("decode manifest: %v", errDecode)
					}
					manifest.FormatVersion = databaseSnapshotFormatVersion + 1
					updated, errMarshal := json.Marshal(manifest)
					if errMarshal != nil {
						t.Fatalf("encode future manifest: %v", errMarshal)
					}
					return updated
				}, "")
			},
			wantError: "newer than supported",
		},
		{
			name: "duplicate entry",
			build: func(t *testing.T, source string, target string) {
				t.Helper()
				rewriteDatabaseSnapshotZIP(t, source, target, func(_ string, raw []byte) []byte { return raw }, databaseSnapshotManifestName)
			},
			wantError: "duplicate zip entry",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			target := filepath.Join(t.TempDir(), "candidate.zip")
			test.build(t, sourcePath, target)
			snapshot, errOpen := OpenDatabaseSnapshot(context.Background(), target)
			if snapshot != nil {
				_ = snapshot.Close()
			}
			if errOpen == nil || !strings.Contains(errOpen.Error(), test.wantError) {
				t.Fatalf("OpenDatabaseSnapshot() error = %v, want %q", errOpen, test.wantError)
			}
		})
	}
}

func TestDatabaseSnapshotImportRejectsNonEmptyTarget(t *testing.T) {
	t.Parallel()

	snapshot := openDatabaseSnapshotValidationFixture(t)
	target := openDatabaseSnapshotSQLiteTestDB(t, filepath.Join(t.TempDir(), "target.db"))
	legacyAPIKeys := JSONB(`["existing-key"]`)
	if errCreate := target.Create(&ConfigRecord{Key: configAPIKeysRootKey, Value: legacyAPIKeys, Version: 1}).Error; errCreate != nil {
		t.Fatalf("seed non-empty target: %v", errCreate)
	}
	_, errImport := ImportDatabaseSnapshot(context.Background(), target, snapshot, nil)
	if errImport == nil || !strings.Contains(errImport.Error(), "business table config is not empty") {
		t.Fatalf("ImportDatabaseSnapshot() error = %v, want non-empty target error", errImport)
	}
	var configRecord ConfigRecord
	if errFirst := target.First(&configRecord, "key = ?", configAPIKeysRootKey).Error; errFirst != nil {
		t.Fatalf("load unchanged target config: %v", errFirst)
	}
	if !reflect.DeepEqual(configRecord.Value, legacyAPIKeys) {
		t.Fatalf("target config value = %s, want %s", configRecord.Value, legacyAPIKeys)
	}
	var apiKeyCount int64
	if errCount := target.Model(&APIKeyRecord{}).Count(&apiKeyCount).Error; errCount != nil || apiKeyCount != 0 {
		t.Fatalf("target API key count = %d, error = %v", apiKeyCount, errCount)
	}
	var eventCount int64
	if errCount := target.Model(&ClusterEventRecord{}).Count(&eventCount).Error; errCount != nil || eventCount != 0 {
		t.Fatalf("target cluster event count = %d, error = %v", eventCount, errCount)
	}
}

func TestDatabaseSnapshotPostgresPreflightRejectsOversizedField(t *testing.T) {
	t.Parallel()

	source := createDatabaseSnapshotValidationFixture(t)
	target := filepath.Join(t.TempDir(), "oversized.zip")
	var quotaRaw []byte
	rewriteDatabaseSnapshotZIP(t, source, target, func(name string, raw []byte) []byte {
		switch name {
		case databaseSnapshotTableEntryName("quota_snapshot"):
			record := QuotaSnapshotRecord{}
			if errDecode := json.Unmarshal([]byte(strings.TrimSpace(string(raw))), &record); errDecode != nil {
				t.Fatalf("decode quota snapshot row: %v", errDecode)
			}
			record.HomeID = strings.Repeat("h", quotaCredentialIDMaxLength+200)
			updated, errMarshal := json.Marshal(record)
			if errMarshal != nil {
				t.Fatalf("encode oversized quota snapshot row: %v", errMarshal)
			}
			quotaRaw = append(updated, '\n')
			return quotaRaw
		case databaseSnapshotManifestName:
			manifest := DatabaseSnapshotManifest{}
			if errDecode := json.Unmarshal(raw, &manifest); errDecode != nil {
				t.Fatalf("decode manifest: %v", errDecode)
			}
			updateDatabaseSnapshotManifestChecksum(t, &manifest, "quota_snapshot", quotaRaw)
			updated, errMarshal := json.Marshal(manifest)
			if errMarshal != nil {
				t.Fatalf("encode manifest: %v", errMarshal)
			}
			return updated
		default:
			return raw
		}
	}, "")
	snapshot, errOpen := OpenDatabaseSnapshot(context.Background(), target)
	if errOpen != nil {
		t.Fatalf("OpenDatabaseSnapshot() error = %v", errOpen)
	}
	defer func() { _ = snapshot.Close() }()
	if errSQLite := snapshot.ValidateForBackend(context.Background(), DatabaseBackendSQLite); errSQLite != nil {
		t.Fatalf("sqlite preflight error = %v", errSQLite)
	}
	errPostgres := snapshot.ValidateForBackend(context.Background(), DatabaseBackendPostgres)
	if errPostgres == nil || !strings.Contains(errPostgres.Error(), "field home_id exceeds postgres declared size 256") {
		t.Fatalf("postgres preflight error = %v, want home_id size error", errPostgres)
	}
}

func TestDatabaseSnapshotPostgresPreflightRejectsNUL(t *testing.T) {
	t.Parallel()

	source := createDatabaseSnapshotValidationFixture(t)
	tests := []struct {
		name      string
		table     string
		mutate    func(t *testing.T, raw []byte) []byte
		wantError string
	}{
		{
			name:  "text value",
			table: "log",
			mutate: func(t *testing.T, raw []byte) []byte {
				t.Helper()
				record := AppLogRecord{}
				if errDecode := json.Unmarshal(bytes.TrimSpace(raw), &record); errDecode != nil {
					t.Fatalf("decode log row: %v", errDecode)
				}
				record.Line = "before\x00after"
				updated, errMarshal := json.Marshal(record)
				if errMarshal != nil {
					t.Fatalf("encode log row with NUL: %v", errMarshal)
				}
				return append(updated, '\n')
			},
			wantError: "field line contains a NUL character unsupported by postgres",
		},
		{
			name:  "json value",
			table: "config",
			mutate: func(t *testing.T, raw []byte) []byte {
				t.Helper()
				record := ConfigRecord{}
				if errDecode := json.Unmarshal(bytes.TrimSpace(raw), &record); errDecode != nil {
					t.Fatalf("decode config row: %v", errDecode)
				}
				record.Value = JSONB(`{"nested":["before\u0000after"]}`)
				updated, errMarshal := json.Marshal(record)
				if errMarshal != nil {
					t.Fatalf("encode config row with JSON NUL: %v", errMarshal)
				}
				return append(updated, '\n')
			},
			wantError: "field value contains a NUL character unsupported by postgres",
		},
		{
			name:  "literal json escape and binary zeros",
			table: "config",
			mutate: func(t *testing.T, raw []byte) []byte {
				t.Helper()
				record := ConfigRecord{}
				if errDecode := json.Unmarshal(bytes.TrimSpace(raw), &record); errDecode != nil {
					t.Fatalf("decode config row: %v", errDecode)
				}
				record.Value = JSONB(`{"value":"\\u0000"}`)
				updated, errMarshal := json.Marshal(record)
				if errMarshal != nil {
					t.Fatalf("encode config row with literal JSON escape: %v", errMarshal)
				}
				return append(updated, '\n')
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			target := filepath.Join(t.TempDir(), "postgres-nul.zip")
			rewriteDatabaseSnapshotTable(t, source, target, test.table, func(raw []byte) []byte {
				return test.mutate(t, raw)
			})
			snapshot, errOpen := OpenDatabaseSnapshot(context.Background(), target)
			if errOpen != nil {
				t.Fatalf("OpenDatabaseSnapshot() error = %v", errOpen)
			}
			t.Cleanup(func() { _ = snapshot.Close() })
			if errSQLite := snapshot.ValidateForBackend(context.Background(), DatabaseBackendSQLite); errSQLite != nil {
				t.Fatalf("sqlite preflight error = %v", errSQLite)
			}
			errPostgres := snapshot.ValidateForBackend(context.Background(), DatabaseBackendPostgres)
			if test.wantError == "" {
				if errPostgres != nil {
					t.Fatalf("postgres preflight error = %v, want success", errPostgres)
				}
				return
			}
			if errPostgres == nil || !strings.Contains(errPostgres.Error(), test.wantError) {
				t.Fatalf("postgres preflight error = %v, want %q", errPostgres, test.wantError)
			}
		})
	}
}

func TestSnapshotJSONContainsNUL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		raw     JSONB
		wantNUL bool
		wantErr bool
	}{
		{name: "ordinary", raw: JSONB(`{"value":"ordinary"}`)},
		{name: "nested value", raw: JSONB(`{"items":[{"value":"\u0000"}]}`), wantNUL: true},
		{name: "object key", raw: JSONB(`{"\u0000":"value"}`), wantNUL: true},
		{name: "literal escape", raw: JSONB(`{"value":"\\u0000"}`)},
		{name: "valid surrogate pair", raw: JSONB(`{"value":"\uD83D\uDE00"}`)},
		{name: "literal surrogate escape", raw: JSONB(`{"value":"\\uD800"}`)},
		{name: "unpaired high surrogate", raw: JSONB(`{"value":"\uD800"}`), wantErr: true},
		{name: "unpaired low surrogate", raw: JSONB(`{"value":"\uDC00"}`), wantErr: true},
		{name: "high surrogate followed by ordinary escape", raw: JSONB(`{"value":"\uD800\u0041"}`), wantErr: true},
		{name: "binary-like base64", raw: JSONB(`{"value":"AAEC/w=="}`)},
		{name: "invalid UTF-8", raw: JSONB("{\"value\":\"\xff\"}"), wantErr: true},
		{name: "invalid", raw: JSONB(`{"value":`), wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			gotNUL, errNUL := snapshotJSONContainsNUL(test.raw)
			if (errNUL != nil) != test.wantErr {
				t.Fatalf("snapshotJSONContainsNUL() error = %v, wantErr %t", errNUL, test.wantErr)
			}
			if gotNUL != test.wantNUL {
				t.Fatalf("snapshotJSONContainsNUL() = %t, want %t", gotNUL, test.wantNUL)
			}
		})
	}
}

func TestDatabaseSnapshotValidationRejectsInvalidRecords(t *testing.T) {
	t.Parallel()

	source := createDatabaseSnapshotValidationFixture(t)
	tests := []struct {
		name      string
		table     string
		mutate    func(t *testing.T, raw []byte) []byte
		wantError string
	}{
		{
			name:  "invalid UTF-8",
			table: "log",
			mutate: func(t *testing.T, raw []byte) []byte {
				t.Helper()
				updated := bytes.Replace(raw, []byte("snapshot log"), []byte{'b', 0xff, 'd'}, 1)
				if bytes.Equal(updated, raw) {
					t.Fatal("log fixture text was not replaced")
				}
				return updated
			},
			wantError: "JSON contains invalid UTF-8",
		},
		{
			name:  "unpaired JSON surrogate",
			table: "config",
			mutate: func(t *testing.T, raw []byte) []byte {
				t.Helper()
				updated := bytes.Replace(raw, []byte(`{"debug":true,"items":[1,null,"x"]}`), []byte(`{"value":"\uD800"}`), 1)
				if bytes.Equal(updated, raw) {
					t.Fatal("config JSON fixture was not replaced")
				}
				return updated
			},
			wantError: "unpaired high surrogate escape",
		},
		{
			name:  "invalid base64",
			table: "plugin_store_auth_key",
			mutate: func(t *testing.T, raw []byte) []byte {
				t.Helper()
				row := make(map[string]any)
				if errDecode := json.Unmarshal(raw, &row); errDecode != nil {
					t.Fatalf("decode plugin key row: %v", errDecode)
				}
				row["Key"] = "***not-base64***"
				updated, errMarshal := json.Marshal(row)
				if errMarshal != nil {
					t.Fatalf("encode plugin key row: %v", errMarshal)
				}
				return append(updated, '\n')
			},
			wantError: "illegal base64 data",
		},
		{
			name:  "null not-null field",
			table: "config",
			mutate: func(t *testing.T, raw []byte) []byte {
				t.Helper()
				row := make(map[string]any)
				if errDecode := json.Unmarshal(raw, &row); errDecode != nil {
					t.Fatalf("decode config row: %v", errDecode)
				}
				row["Value"] = nil
				updated, errMarshal := json.Marshal(row)
				if errMarshal != nil {
					t.Fatalf("encode config row: %v", errMarshal)
				}
				return append(updated, '\n')
			},
			wantError: "field value must not be null",
		},
		{
			name:  "blank channel auth id",
			table: "channel_group_detail",
			mutate: func(t *testing.T, raw []byte) []byte {
				t.Helper()
				record := ChannelGroupDetailRecord{}
				if errDecode := json.Unmarshal(raw, &record); errDecode != nil {
					t.Fatalf("decode channel detail row: %v", errDecode)
				}
				record.AuthID = ""
				updated, errMarshal := json.Marshal(record)
				if errMarshal != nil {
					t.Fatalf("encode channel detail row: %v", errMarshal)
				}
				return append(updated, '\n')
			},
			wantError: "field auth_id must not be blank",
		},
		{
			name:  "whitespace quota credential id",
			table: "quota_snapshot",
			mutate: func(t *testing.T, raw []byte) []byte {
				t.Helper()
				record := QuotaSnapshotRecord{}
				if errDecode := json.Unmarshal(raw, &record); errDecode != nil {
					t.Fatalf("decode quota snapshot row: %v", errDecode)
				}
				record.CredentialID = " \t "
				updated, errMarshal := json.Marshal(record)
				if errMarshal != nil {
					t.Fatalf("encode quota snapshot row: %v", errMarshal)
				}
				return append(updated, '\n')
			},
			wantError: "field credential_id must not be blank",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			target := filepath.Join(t.TempDir(), "invalid-record.zip")
			rewriteDatabaseSnapshotTable(t, source, target, test.table, func(raw []byte) []byte {
				return test.mutate(t, raw)
			})
			snapshot, errOpen := OpenDatabaseSnapshot(context.Background(), target)
			if snapshot != nil {
				_ = snapshot.Close()
			}
			if errOpen == nil || !strings.Contains(errOpen.Error(), test.wantError) {
				t.Fatalf("OpenDatabaseSnapshot() error = %v, want %q", errOpen, test.wantError)
			}
		})
	}
}

func TestDatabaseSnapshotImportRejectsCrossRecordViolations(t *testing.T) {
	t.Parallel()

	source := createDatabaseSnapshotValidationFixture(t)
	tests := []struct {
		name      string
		table     string
		mutate    func(t *testing.T, raw []byte) []byte
		wantError string
	}{
		{
			name:  "duplicate primary key",
			table: "auth",
			mutate: func(_ *testing.T, raw []byte) []byte {
				return append(append([]byte(nil), raw...), raw...)
			},
			wantError: "insert database snapshot table auth",
		},
		{
			name:  "missing auth relationship",
			table: "channel_group_detail",
			mutate: func(t *testing.T, raw []byte) []byte {
				t.Helper()
				record := ChannelGroupDetailRecord{}
				if errDecode := json.Unmarshal(raw, &record); errDecode != nil {
					t.Fatalf("decode channel detail row: %v", errDecode)
				}
				record.AuthID = "missing-auth"
				updated, errMarshal := json.Marshal(record)
				if errMarshal != nil {
					t.Fatalf("encode channel detail row: %v", errMarshal)
				}
				return append(updated, '\n')
			},
			wantError: "relationship channel_group_detail.auth_id",
		},
		{
			name:  "missing plugin auth key",
			table: "plugin_store_auth",
			mutate: func(t *testing.T, raw []byte) []byte {
				t.Helper()
				record := PluginStoreAuthRecord{}
				if errDecode := json.Unmarshal(raw, &record); errDecode != nil {
					t.Fatalf("decode plugin auth row: %v", errDecode)
				}
				record.KeyVersion++
				updated, errMarshal := json.Marshal(record)
				if errMarshal != nil {
					t.Fatalf("encode plugin auth row: %v", errMarshal)
				}
				return append(updated, '\n')
			},
			wantError: "relationship plugin_store_auth.key_version",
		},
		{
			name:  "missing api key channel group",
			table: "api_key",
			mutate: func(t *testing.T, raw []byte) []byte {
				t.Helper()
				record := APIKeyRecord{}
				if errDecode := json.Unmarshal(raw, &record); errDecode != nil {
					t.Fatalf("decode api key row: %v", errDecode)
				}
				record.Channels = JSONB(`[999999]`)
				updated, errMarshal := json.Marshal(record)
				if errMarshal != nil {
					t.Fatalf("encode api key row: %v", errMarshal)
				}
				return append(updated, '\n')
			},
			wantError: "field channels references missing channel group 999999",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "invalid-cross-record.zip")
			rewriteDatabaseSnapshotTable(t, source, path, test.table, func(raw []byte) []byte {
				return test.mutate(t, raw)
			})
			snapshot, errOpen := OpenDatabaseSnapshot(context.Background(), path)
			if errOpen != nil {
				t.Fatalf("OpenDatabaseSnapshot() error = %v, want transaction-time validation", errOpen)
			}
			t.Cleanup(func() { _ = snapshot.Close() })
			target := openDatabaseSnapshotSQLiteTestDB(t, filepath.Join(t.TempDir(), "target.db"))
			_, errImport := ImportDatabaseSnapshot(context.Background(), target, snapshot, nil)
			if errImport == nil || !strings.Contains(errImport.Error(), test.wantError) {
				t.Fatalf("ImportDatabaseSnapshot() error = %v, want %q", errImport, test.wantError)
			}
			assertDatabaseSnapshotBusinessTablesEmpty(t, target)
		})
	}
}

func TestDatabaseSnapshotImportRollsBackOnInsertFailure(t *testing.T) {
	t.Parallel()

	snapshot := openDatabaseSnapshotValidationFixture(t)
	target := openDatabaseSnapshotSQLiteTestDB(t, filepath.Join(t.TempDir(), "target.db"))
	if errTrigger := target.Exec(`CREATE TRIGGER fail_usage_snapshot_import BEFORE INSERT ON "usage" BEGIN SELECT RAISE(ABORT, 'forced snapshot import failure'); END`).Error; errTrigger != nil {
		t.Fatalf("create failure trigger: %v", errTrigger)
	}
	_, errImport := ImportDatabaseSnapshot(context.Background(), target, snapshot, nil)
	if errImport == nil || !strings.Contains(errImport.Error(), "forced snapshot import failure") {
		t.Fatalf("ImportDatabaseSnapshot() error = %v, want forced insert failure", errImport)
	}
	assertDatabaseSnapshotBusinessTablesEmpty(t, target)
}

func TestDatabaseSnapshotPostgresCrossBackendRoundTrips(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("CLIPROXY_HOME_TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("CLIPROXY_HOME_TEST_POSTGRES_DSN is not set")
	}
	ctx := context.Background()
	postgresDB, errOpen := gorm.Open(postgres.Open(dsn), databaseGORMConfig())
	if errOpen != nil {
		t.Fatalf("open postgres: %v", errOpen)
	}
	sqlDB, errSQL := postgresDB.DB()
	if errSQL != nil {
		t.Fatalf("postgres DB(): %v", errSQL)
	}
	t.Cleanup(func() {
		dropDatabaseSnapshotPostgresTables(t, postgresDB)
		if errClose := sqlDB.Close(); errClose != nil {
			t.Errorf("close postgres: %v", errClose)
		}
	})
	dropDatabaseSnapshotPostgresTables(t, postgresDB)

	sqliteSource := openDatabaseSnapshotSQLiteTestDB(t, filepath.Join(t.TempDir(), "sqlite-source.db"))
	want := seedDatabaseSnapshotTestData(t, sqliteSource)
	sqliteSnapshotPath := filepath.Join(t.TempDir(), "sqlite-source.zip")
	if _, errExport := ExportDatabaseSnapshot(ctx, sqliteSource, DatabaseSnapshotExportOptions{Path: sqliteSnapshotPath}); errExport != nil {
		t.Fatalf("export sqlite snapshot: %v", errExport)
	}
	sqliteSnapshot, errSnapshot := OpenDatabaseSnapshot(ctx, sqliteSnapshotPath)
	if errSnapshot != nil {
		t.Fatalf("open sqlite snapshot: %v", errSnapshot)
	}
	defer func() { _ = sqliteSnapshot.Close() }()
	if _, errImport := ImportDatabaseSnapshot(ctx, postgresDB, sqliteSnapshot, nil); errImport != nil {
		t.Fatalf("import sqlite snapshot into postgres: %v", errImport)
	}
	createdUser := UserRecord{Username: "postgres-sequence-user"}
	if errCreate := postgresDB.Create(&createdUser).Error; errCreate != nil {
		t.Fatalf("create postgres user after sqlite import: %v", errCreate)
	}
	if createdUser.ID <= want.maximumUserID {
		t.Fatalf("postgres user id after sqlite import = %d, want greater than %d", createdUser.ID, want.maximumUserID)
	}

	postgresSnapshotPath := filepath.Join(t.TempDir(), "postgres-source.zip")
	manifest, errExport := ExportDatabaseSnapshot(ctx, postgresDB, DatabaseSnapshotExportOptions{Path: postgresSnapshotPath})
	if errExport != nil {
		t.Fatalf("export postgres snapshot: %v", errExport)
	}
	if manifest.SourceBackend != DatabaseBackendPostgres {
		t.Fatalf("postgres snapshot source backend = %q", manifest.SourceBackend)
	}
	postgresSnapshot, errSnapshot := OpenDatabaseSnapshot(ctx, postgresSnapshotPath)
	if errSnapshot != nil {
		t.Fatalf("open postgres snapshot: %v", errSnapshot)
	}
	defer func() { _ = postgresSnapshot.Close() }()

	sqliteTarget := openDatabaseSnapshotSQLiteRawTestDB(t, filepath.Join(t.TempDir(), "sqlite-target.db"))
	if _, errImport := ImportDatabaseSnapshot(ctx, sqliteTarget, postgresSnapshot, nil); errImport != nil {
		t.Fatalf("import postgres snapshot into sqlite: %v", errImport)
	}
	var sqliteUsers int64
	if errCount := sqliteTarget.Unscoped().Model(&UserRecord{}).Count(&sqliteUsers).Error; errCount != nil || sqliteUsers != 2 {
		t.Fatalf("sqlite user count after postgres import = %d, error = %v", sqliteUsers, errCount)
	}

	dropDatabaseSnapshotPostgresTables(t, postgresDB)
	if _, errImport := ImportDatabaseSnapshot(ctx, postgresDB, postgresSnapshot, nil); errImport != nil {
		t.Fatalf("reimport postgres snapshot into postgres: %v", errImport)
	}
	var postgresUsers int64
	if errCount := postgresDB.Unscoped().Model(&UserRecord{}).Count(&postgresUsers).Error; errCount != nil || postgresUsers != 2 {
		t.Fatalf("postgres user count after postgres import = %d, error = %v", postgresUsers, errCount)
	}
}

type databaseSnapshotTestData struct {
	authUUID      string
	pluginAuthID  uint
	maximumUserID uint
	maximumLogID  uint
}

type databaseSnapshotWriteTracker struct {
	writes       int
	maximumWrite int
	totalBytes   int
}

func (w *databaseSnapshotWriteTracker) Write(data []byte) (int, error) {
	w.writes++
	if len(data) > w.maximumWrite {
		w.maximumWrite = len(data)
	}
	w.totalBytes += len(data)
	return len(data), nil
}

func seedDatabaseSnapshotTestData(t *testing.T, db *gorm.DB) databaseSnapshotTestData {
	t.Helper()
	now := time.Now().UTC().Add(-time.Hour).In(time.FixedZone("snapshot-test", 8*60*60))
	deletedAt := now.Add(time.Minute)
	expiresAt := now.Add(24 * time.Hour)
	expiredAt := now.Add(-24 * time.Hour)
	userID := uint(101)
	channelGroupID := uint(201)
	modelGroupID := uint(301)
	apiKeyID := uint(401)
	usageID := uint(501)
	pluginAuthID := uint(601)
	authUUID := "auth-snapshot-uuid"
	records := []any{
		&AuthRecord{UUID: authUUID, AuthJSON: JSONB(`{"token":"secret","nested":{"ok":true}}`), Version: 3, ID: "auth-logical-id", Index: "auth-index", Provider: "codex", CreatedAt: now, UpdatedAt: now, DeletedAt: gorm.DeletedAt{Time: deletedAt, Valid: true}},
		&ConfigRecord{Key: "server", Value: JSONB(`{"debug":true,"items":[1,null,"x"]}`), Version: 2, CreatedAt: now, UpdatedAt: now},
		&KVRecord{Key: "ordinary:valid", Value: []byte{0, 1, 2, 255}, Version: 1, ExpiresAt: &expiresAt, CreatedAt: now, UpdatedAt: now},
		&KVRecord{Key: "ordinary:expired", Value: []byte("expired"), Version: 1, ExpiresAt: &expiredAt, CreatedAt: now, UpdatedAt: now},
		&KVRecord{Key: "internal:migration:test", Value: []byte("internal"), Version: 1, CreatedAt: now, UpdatedAt: now},
		&PluginStoreAuthKeyRecord{ID: 1, Key: []byte{9, 8, 7, 0, 255}, KeyVersion: 7},
		&PluginStoreAuthRecord{ID: pluginAuthID, Name: "store", Match: "github.com/example", ApplyTo: JSONB(`["manifest"]`), AuthType: "bearer", HeaderName: "Authorization", EncryptedCredentials: []byte{0, 1, 2, 253, 254, 255}, KeyVersion: 7, Enabled: true, Version: 4},
		&UserRecord{ID: userID, Username: "snapshot-user", Password: "hash", Credits: 42.5, Timezone: "Asia/Shanghai", MFA: JSONB(`{"enabled":true}`), Passkey: JSONB(`[]`), CreatedAt: now, UpdatedAt: now},
		&UserSecurityTokenRecord{ID: 111, UserID: userID, Purpose: UserSecurityTokenPurposeEmailVerification, TokenHash: HashUserSecurityValue("snapshot-token"), ExpiresAt: expiresAt, CreatedAt: now},
		&UserMailJobRecord{ID: 121, UserID: userID, Purpose: UserSecurityTokenPurposeEmailVerification, Status: UserMailJobStatusPending, NextAttemptAt: now, CreatedAt: now, UpdatedAt: now},
		&UserSecurityThrottleRecord{Key: "snapshot-throttle", Count: 1, ExpiresAt: expiresAt, UpdatedAt: now},
		&ChannelGroupRecord{ID: channelGroupID, ChannelName: "snapshot-channel", CreatedAt: now, UpdatedAt: now},
		&ModelGroupRecord{ID: modelGroupID, GroupName: "snapshot-models", CreatedAt: now, UpdatedAt: now},
		&APIKeyRecord{ID: apiKeyID, APIKey: "snapshot-api-key", UserID: &userID, Channels: JSONB(`[201]`), ModelGroups: JSONB(`[301]`), CreatedAt: now, UpdatedAt: now},
		&ChannelGroupDetailRecord{ID: 211, ChannelGroupID: channelGroupID, AuthID: "auth-logical-id", CreatedAt: now, UpdatedAt: now},
		&ModelGroupDetailRecord{ID: 311, ModelGroupID: modelGroupID, ModelID: "gpt-test", CreatedAt: now, UpdatedAt: now},
		&UsageRecord{ID: usageID, Timestamp: now, Source: "snapshot", AuthIndex: "auth-index", InputTokens: 1, OutputTokens: 2, TotalTokens: 3, Provider: "codex", Model: "gpt-test", TokensJSON: JSONB(`{"input":1}`), FailJSON: JSONB(`null`), PayloadJSON: JSONB(`{"model":"gpt-test"}`), CreatedAt: now},
		&QuotaSnapshotRecord{CredentialID: authUUID, QuotaStatus: "healthy", CollectionStatus: "success", Source: "active_probe", ObservedAt: &now, ExpiresAt: &expiresAt, ProbeLeaseOwner: "old-home", ProbeLeaseExpiresAt: &expiresAt, ParserVersion: 1, CollectorVersion: 1, CreatedAt: now, UpdatedAt: now},
		&QuotaWindowRecord{CredentialID: authUUID, WindowID: "primary", Label: "Primary", Scope: "credential", Mode: "rolling", Status: "available", Unit: "requests", Used: float64Pointer(2), Remaining: float64Pointer(8), Limit: float64Pointer(10), PeriodUnit: "hour", Source: "active_probe", ObservedAt: now, ExpiresAt: &expiresAt, CreatedAt: now, UpdatedAt: now},
		&BillingModelPriceRecord{ID: "price-1", Provider: "codex", Model: "gpt-test", ServiceTier: "*", InputPricePerMillion: 1.25, OutputPricePerMillion: 2.5, Source: "manual", Enabled: true, CreatedAt: now, UpdatedAt: now, Revision: 1},
		&BillingModelPriceImportPreviewRecord{ID: "preview-1", Revision: "revision-1", Source: "models.dev", SourceURL: "https://example.invalid/models", SourceVersion: "1", SourceFetchedAt: now, SourceModelCount: 1, Atomic: true, GeneratedAt: now, ExpiresAt: expiresAt, Payload: JSONB(`{"rows":[]}`), CreatedAt: now},
		&BillingModelPriceImportOperationRecord{ID: "operation-1", PreviewID: "preview-1", PreviewRevision: "revision-1", IdempotencyKey: "idempotency-1", RequestHash: "request-hash", SelectionHash: "selection-hash", Atomic: true, Status: "applied", AppliedAt: now, Result: JSONB(`{"status":"ok"}`), CreatedAt: now},
		&BillingBalanceRecord{ID: "balance-1", UserID: userID, Type: "recharge", Amount: 10, BalanceBefore: 32.5, BalanceAfter: 42.5, Operator: "test", CreatedAt: now},
		&BillingChargeRecord{ID: "charge-1", UsageID: usageID, PayloadHash: "payload-hash", UserID: &userID, APIKeyID: &apiKeyID, Provider: "codex", Model: "gpt-test", InputTokens: 1, OutputTokens: 2, Amount: 0.01, BalanceBefore: 42.5, BalanceAfter: 42.49, PriceSnapshot: JSONB(`{"input":1.25}`), CreatedAt: now},
		&ProxyPoolRecord{ID: "proxy-1", Name: "Proxy", ProxyURL: "http://127.0.0.1:8080", Enabled: true, Scope: "global", LastTestResult: "untested", CreatedAt: now, UpdatedAt: now},
		&AppLogRecord{ID: 701, Timestamp: now, ClientIP: "127.0.0.1", RequestID: "request-1", HomeIP: "127.0.0.1", Level: "info", Line: "snapshot log", CreatedAt: now},
		&CertificateRecord{ID: "certificate-1", ClusterID: "cluster-1", CertificatePEM: "certificate", PrivateKeyPEM: "private-key", IsCA: true, SerialNumber: "1", NotBefore: now, NotAfter: expiresAt, CreatedAt: now, UpdatedAt: now},
		&PluginStatusRecord{NodeType: "cpa", NodeID: "node-1", PluginID: "plugin-1", ReportedAt: now, CreatedAt: now, UpdatedAt: now},
		&PluginTaskRecord{ID: 901, Operation: "install", PluginID: "plugin-1", CreatedAt: now, UpdatedAt: now},
		&ClusterNodeRecord{IP: "127.0.0.1", Port: 18327, StartedAt: now, LastSeenAt: now},
		&CPANodeRecord{HomeIP: "127.0.0.1", HomePort: 18327, NodeKey: "node-key", NodeID: "node-1", ConnectedAt: now, LastSeenAt: now, CreatedAt: now, UpdatedAt: now},
		&ClusterEventRecord{ID: 1001, Scope: "config", Op: "update", EntityUUID: "server", Version: 1, CreatedAt: now},
		&OAuthSessionRecord{State: "oauth-state", Provider: "codex", Status: "pending", Data: JSONB(`{"safe":true}`), CreatedAt: now, UpdatedAt: now, ExpiresAt: expiresAt},
	}
	for _, record := range records {
		if errCreate := db.Unscoped().Create(record).Error; errCreate != nil {
			t.Fatalf("seed database snapshot record %T: %v", record, errCreate)
		}
	}
	if errDisable := db.Model(&PluginStoreAuthRecord{}).Where("id = ?", pluginAuthID).Update("enabled", false).Error; errDisable != nil {
		t.Fatalf("disable database snapshot plugin auth: %v", errDisable)
	}
	if errDisable := db.Model(&ProxyPoolRecord{}).Where("id = ?", "proxy-1").Update("enabled", false).Error; errDisable != nil {
		t.Fatalf("disable database snapshot proxy pool: %v", errDisable)
	}
	return databaseSnapshotTestData{authUUID: authUUID, pluginAuthID: pluginAuthID, maximumUserID: userID, maximumLogID: 701}
}

func openDatabaseSnapshotSQLiteTestDB(t *testing.T, path string) *gorm.DB {
	t.Helper()
	db := openDatabaseSnapshotSQLiteRawTestDB(t, path)
	if errMigrate := AutoMigrate(db); errMigrate != nil {
		t.Fatalf("AutoMigrate() error = %v", errMigrate)
	}
	return db
}

func openDatabaseSnapshotSQLiteRawTestDB(t *testing.T, path string) *gorm.DB {
	t.Helper()
	db, errOpen := OpenSQLite(context.Background(), path)
	if errOpen != nil {
		t.Fatalf("OpenSQLite() error = %v", errOpen)
	}
	sqlDB, errSQL := db.DB()
	if errSQL != nil {
		t.Fatalf("DB() error = %v", errSQL)
	}
	t.Cleanup(func() {
		if errClose := sqlDB.Close(); errClose != nil {
			t.Errorf("close sqlite: %v", errClose)
		}
	})
	return db
}

func createDatabaseSnapshotValidationFixture(t *testing.T) string {
	t.Helper()
	db := openDatabaseSnapshotSQLiteTestDB(t, filepath.Join(t.TempDir(), "fixture.db"))
	seedDatabaseSnapshotTestData(t, db)
	path := filepath.Join(t.TempDir(), "fixture.zip")
	if _, errExport := ExportDatabaseSnapshot(context.Background(), db, DatabaseSnapshotExportOptions{Path: path}); errExport != nil {
		t.Fatalf("ExportDatabaseSnapshot() fixture error = %v", errExport)
	}
	return path
}

func openDatabaseSnapshotValidationFixture(t *testing.T) *ValidatedDatabaseSnapshot {
	t.Helper()
	path := createDatabaseSnapshotValidationFixture(t)
	snapshot, errOpen := OpenDatabaseSnapshot(context.Background(), path)
	if errOpen != nil {
		t.Fatalf("OpenDatabaseSnapshot() fixture error = %v", errOpen)
	}
	t.Cleanup(func() {
		if errClose := snapshot.Close(); errClose != nil {
			t.Errorf("close snapshot fixture: %v", errClose)
		}
	})
	return snapshot
}

func assertDatabaseSnapshotResultCounts(t *testing.T, result DatabaseSnapshotImportResult, want map[string]DatabaseSnapshotImportTableResult) {
	t.Helper()
	got := make(map[string]DatabaseSnapshotImportTableResult, len(result.Tables))
	for _, table := range result.Tables {
		got[table.Name] = table
	}
	for name, wantTable := range want {
		if got[name] != wantTable {
			t.Errorf("table result %s = %#v, want %#v", name, got[name], wantTable)
		}
	}
}

func assertDatabaseSnapshotRuntimeTablesEmpty(t *testing.T, db *gorm.DB) {
	t.Helper()
	for _, model := range homeDatabaseModels {
		if model.restore {
			continue
		}
		var count int64
		if errCount := db.Unscoped().Model(model.newRecord()).Count(&count).Error; errCount != nil {
			t.Fatalf("count runtime table %s: %v", model.name, errCount)
		}
		if count != 0 {
			t.Errorf("runtime table %s count = %d, want 0", model.name, count)
		}
	}
}

func assertDatabaseSnapshotBusinessTablesEmpty(t *testing.T, db *gorm.DB) {
	t.Helper()
	for _, model := range homeDatabaseModels {
		if !model.restore {
			continue
		}
		var count int64
		if errCount := db.Unscoped().Model(model.newRecord()).Count(&count).Error; errCount != nil {
			t.Fatalf("count business table %s after rollback: %v", model.name, errCount)
		}
		if count != 0 {
			t.Fatalf("business table %s count after rollback = %d, want 0", model.name, count)
		}
	}
}

func rewriteDatabaseSnapshotZIP(t *testing.T, source string, target string, mutate func(name string, raw []byte) []byte, duplicateName string) {
	t.Helper()
	reader, errOpen := zip.OpenReader(source)
	if errOpen != nil {
		t.Fatalf("open source zip: %v", errOpen)
	}
	defer func() { _ = reader.Close() }()
	targetFile, errCreate := os.Create(target)
	if errCreate != nil {
		t.Fatalf("create target zip: %v", errCreate)
	}
	writer := zip.NewWriter(targetFile)
	for _, entry := range reader.File {
		entryReader, errEntry := entry.Open()
		if errEntry != nil {
			t.Fatalf("open zip entry %s: %v", entry.Name, errEntry)
		}
		raw, errRead := io.ReadAll(entryReader)
		_ = entryReader.Close()
		if errRead != nil {
			t.Fatalf("read zip entry %s: %v", entry.Name, errRead)
		}
		raw = mutate(entry.Name, raw)
		entryWriter, errWriter := writer.Create(entry.Name)
		if errWriter != nil {
			t.Fatalf("create zip entry %s: %v", entry.Name, errWriter)
		}
		if _, errWrite := entryWriter.Write(raw); errWrite != nil {
			t.Fatalf("write zip entry %s: %v", entry.Name, errWrite)
		}
		if entry.Name == duplicateName {
			duplicateWriter, errDuplicate := writer.Create(entry.Name)
			if errDuplicate != nil {
				t.Fatalf("create duplicate zip entry %s: %v", entry.Name, errDuplicate)
			}
			if _, errWrite := duplicateWriter.Write(raw); errWrite != nil {
				t.Fatalf("write duplicate zip entry %s: %v", entry.Name, errWrite)
			}
		}
	}
	if errClose := writer.Close(); errClose != nil {
		t.Fatalf("close target zip: %v", errClose)
	}
	if errClose := targetFile.Close(); errClose != nil {
		t.Fatalf("close target file: %v", errClose)
	}
}

func rewriteDatabaseSnapshotTable(t *testing.T, source string, target string, table string, mutate func(raw []byte) []byte) {
	t.Helper()
	var updatedTable []byte
	rewriteDatabaseSnapshotZIP(t, source, target, func(name string, raw []byte) []byte {
		switch name {
		case databaseSnapshotTableEntryName(table):
			updatedTable = mutate(raw)
			return updatedTable
		case databaseSnapshotManifestName:
			if updatedTable == nil {
				t.Fatalf("snapshot table %s was not encountered before manifest", table)
			}
			manifest := DatabaseSnapshotManifest{}
			if errDecode := json.Unmarshal(raw, &manifest); errDecode != nil {
				t.Fatalf("decode manifest: %v", errDecode)
			}
			updateDatabaseSnapshotManifestChecksum(t, &manifest, table, updatedTable)
			for index := range manifest.Tables {
				if manifest.Tables[index].Name == table {
					manifest.Tables[index].Rows = int64(strings.Count(string(updatedTable), "\n"))
				}
			}
			updated, errMarshal := json.Marshal(manifest)
			if errMarshal != nil {
				t.Fatalf("encode manifest: %v", errMarshal)
			}
			return updated
		default:
			return raw
		}
	}, "")
}

func float64Pointer(value float64) *float64 {
	return &value
}

func dropDatabaseSnapshotPostgresTables(t *testing.T, db *gorm.DB) {
	t.Helper()
	for index := len(homeDatabaseModels) - 1; index >= 0; index-- {
		if errDrop := db.Migrator().DropTable(homeDatabaseModels[index].newRecord()); errDrop != nil {
			t.Fatalf("drop postgres snapshot table %s: %v", homeDatabaseModels[index].name, errDrop)
		}
	}
}

func updateDatabaseSnapshotManifestChecksum(t *testing.T, manifest *DatabaseSnapshotManifest, table string, raw []byte) {
	t.Helper()
	sum := sha256.Sum256(raw)
	for index := range manifest.Tables {
		if manifest.Tables[index].Name == table {
			manifest.Tables[index].SHA256 = hex.EncodeToString(sum[:])
			return
		}
	}
	t.Fatalf("manifest table %s not found", table)
}
