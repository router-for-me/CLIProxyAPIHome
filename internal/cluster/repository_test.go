package cluster

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/router-for-me/CLIProxyAPIHome/internal/node"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"gorm.io/gorm/schema"
)

func TestOpenSQLite_AutoMigrateAndConfigRoundTrip(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, errOpenSQLite := OpenSQLite(ctx, filepath.Join(t.TempDir(), "home.db"))
	if errOpenSQLite != nil {
		t.Fatalf("OpenSQLite failed: %v", errOpenSQLite)
	}
	sqlDB, errDB := db.DB()
	if errDB != nil {
		t.Fatalf("get sql db: %v", errDB)
	}
	defer func() {
		if errClose := sqlDB.Close(); errClose != nil {
			t.Errorf("close sql db: %v", errClose)
		}
	}()

	if errMigrate := AutoMigrate(db); errMigrate != nil {
		t.Fatalf("AutoMigrate failed: %v", errMigrate)
	}

	repo := NewRepository(db)
	if errUpsertConfigValue := repo.UpsertConfigValue(ctx, "debug", true); errUpsertConfigValue != nil {
		t.Fatalf("UpsertConfigValue failed: %v", errUpsertConfigValue)
	}
	snapshot, errLoadConfigSnapshot := repo.LoadConfigSnapshot(ctx)
	if errLoadConfigSnapshot != nil {
		t.Fatalf("LoadConfigSnapshot failed: %v", errLoadConfigSnapshot)
	}

	var debug bool
	if errUnmarshal := json.Unmarshal(snapshot["debug"], &debug); errUnmarshal != nil {
		t.Fatalf("unmarshal debug: %v", errUnmarshal)
	}
	if !debug {
		t.Fatalf("debug = false, want true")
	}
}

func TestCPANodeSnapshotRoundTrip(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, errOpenSQLite := OpenSQLite(ctx, filepath.Join(t.TempDir(), "home.db"))
	if errOpenSQLite != nil {
		t.Fatalf("OpenSQLite failed: %v", errOpenSQLite)
	}
	sqlDB, errDB := db.DB()
	if errDB != nil {
		t.Fatalf("get sql db: %v", errDB)
	}
	defer func() {
		if errClose := sqlDB.Close(); errClose != nil {
			t.Errorf("close sql db: %v", errClose)
		}
	}()
	if errMigrate := AutoMigrate(db); errMigrate != nil {
		t.Fatalf("AutoMigrate failed: %v", errMigrate)
	}

	repo := NewRepository(db)
	connectedAt := time.Now().UTC().Add(-time.Minute)
	seenAt := time.Now().UTC()
	if errSnapshot := repo.ReplaceCPANodeSnapshot(ctx, "home-a", 8327, []node.Node{
		{NodeID: "node-1", IP: "10.0.0.5", ClientCount: 1, Connected: connectedAt},
	}, seenAt); errSnapshot != nil {
		t.Fatalf("ReplaceCPANodeSnapshot(home-a) failed: %v", errSnapshot)
	}
	if errSnapshot := repo.ReplaceCPANodeSnapshot(ctx, "home-b", 8327, []node.Node{
		{NodeID: "node-2", IP: "10.0.0.6", ClientCount: 2, Connected: connectedAt},
	}, seenAt); errSnapshot != nil {
		t.Fatalf("ReplaceCPANodeSnapshot(home-b) failed: %v", errSnapshot)
	}

	records, errList := repo.ListLiveCPANodes(ctx, seenAt.Add(-time.Second))
	if errList != nil {
		t.Fatalf("ListLiveCPANodes failed: %v", errList)
	}
	if len(records) != 2 {
		t.Fatalf("cpa records = %d, want 2", len(records))
	}
	if records[0].HomeIP != "home-a" || records[0].NodeID != "node-1" || records[0].ClientCount != 1 || records[0].ConnectedAt.IsZero() || records[0].LastSeenAt.IsZero() {
		t.Fatalf("first cpa record = %+v, want home-a/node-1 snapshot", records[0])
	}

	if errSnapshot := repo.ReplaceCPANodeSnapshot(ctx, "home-a", 8327, nil, seenAt.Add(time.Second)); errSnapshot != nil {
		t.Fatalf("ReplaceCPANodeSnapshot(home-a empty) failed: %v", errSnapshot)
	}
	records, errList = repo.ListLiveCPANodes(ctx, seenAt.Add(-time.Second))
	if errList != nil {
		t.Fatalf("ListLiveCPANodes after replace failed: %v", errList)
	}
	if len(records) != 1 || records[0].NodeID != "node-2" {
		t.Fatalf("cpa records after replace = %+v, want only node-2", records)
	}
}

func TestOpenSQLite_ConfiguresLocalConcurrency(t *testing.T) {
	t.Parallel()

	db, errOpenSQLite := OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "home.db"))
	if errOpenSQLite != nil {
		t.Fatalf("OpenSQLite failed: %v", errOpenSQLite)
	}
	sqlDB, errDB := db.DB()
	if errDB != nil {
		t.Fatalf("get sql db: %v", errDB)
	}
	defer func() {
		if errClose := sqlDB.Close(); errClose != nil {
			t.Errorf("close sql db: %v", errClose)
		}
	}()

	if got := sqlDB.Stats().MaxOpenConnections; got != 1 {
		t.Fatalf("MaxOpenConnections = %d, want 1", got)
	}

	var journalMode string
	if errRaw := db.Raw("PRAGMA journal_mode").Scan(&journalMode).Error; errRaw != nil {
		t.Fatalf("read journal_mode: %v", errRaw)
	}
	if got := strings.ToLower(strings.TrimSpace(journalMode)); got != "wal" {
		t.Fatalf("journal_mode = %q, want wal", journalMode)
	}

	var busyTimeout int
	if errRaw := db.Raw("PRAGMA busy_timeout").Scan(&busyTimeout).Error; errRaw != nil {
		t.Fatalf("read busy_timeout: %v", errRaw)
	}
	if busyTimeout < 5000 {
		t.Fatalf("busy_timeout = %d, want at least 5000", busyTimeout)
	}
}

func TestReplaceCPANodeSnapshotConcurrentSameHome(t *testing.T) {
	db, errOpenSQLite := OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "home.db"))
	if errOpenSQLite != nil {
		t.Fatalf("OpenSQLite failed: %v", errOpenSQLite)
	}
	sqlDB, errDB := db.DB()
	if errDB != nil {
		t.Fatalf("get sql db: %v", errDB)
	}
	defer func() {
		if errClose := sqlDB.Close(); errClose != nil {
			t.Errorf("close sql db: %v", errClose)
		}
	}()
	if errMigrate := AutoMigrate(db); errMigrate != nil {
		t.Fatalf("AutoMigrate failed: %v", errMigrate)
	}

	repo := NewRepository(db)
	start := make(chan struct{})
	errCh := make(chan error, 16)
	now := time.Now().UTC()
	var wg sync.WaitGroup
	for i := 0; i < cap(errCh); i++ {
		idx := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errCh <- repo.ReplaceCPANodeSnapshot(context.Background(), "home-a", 8327, []node.Node{
				{NodeID: "cpa-a", IP: "10.0.0.1", Connected: now, ClientCount: idx + 1},
			}, now.Add(time.Duration(idx)*time.Millisecond))
		}()
	}
	close(start)
	wg.Wait()
	close(errCh)
	for errSnapshot := range errCh {
		if errSnapshot != nil {
			t.Fatalf("ReplaceCPANodeSnapshot() concurrent error = %v", errSnapshot)
		}
	}

	records, errRecords := repo.ListLiveCPANodes(context.Background(), now.Add(-time.Minute))
	if errRecords != nil {
		t.Fatalf("ListLiveCPANodes() error = %v", errRecords)
	}
	if len(records) != 1 || records[0].NodeID != "cpa-a" || records[0].HomeIP != "home-a" || records[0].HomePort != 8327 {
		t.Fatalf("records = %+v, want one final CPA snapshot", records)
	}
}

func TestReplaceCPANodeSnapshotPersistsHandlerButNotRevisionOnlyState(t *testing.T) {
	db, errOpenSQLite := OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "home.db"))
	if errOpenSQLite != nil {
		t.Fatalf("OpenSQLite failed: %v", errOpenSQLite)
	}
	sqlDB, errDB := db.DB()
	if errDB != nil {
		t.Fatalf("get sql db: %v", errDB)
	}
	defer func() {
		if errClose := sqlDB.Close(); errClose != nil {
			t.Errorf("close sql db: %v", errClose)
		}
	}()
	if errMigrate := AutoMigrate(db); errMigrate != nil {
		t.Fatalf("AutoMigrate failed: %v", errMigrate)
	}

	seenAt := time.Now().UTC()
	repo := NewRepository(db)
	if errSnapshot := repo.ReplaceCPANodeSnapshot(context.Background(), "home-a", 8327, []node.Node{
		{NodeID: "handler-only", IP: "10.0.0.1", Connected: seenAt, ActiveHandlers: 1},
		{NodeID: "revision-only", IP: "10.0.0.2", Connected: seenAt, LatestCancelRevision: 1},
	}, seenAt); errSnapshot != nil {
		t.Fatalf("ReplaceCPANodeSnapshot() error = %v", errSnapshot)
	}

	records, errList := repo.ListLiveCPANodes(context.Background(), seenAt.Add(-time.Second))
	if errList != nil {
		t.Fatalf("ListLiveCPANodes() error = %v", errList)
	}
	if len(records) != 1 {
		t.Fatalf("snapshot records = %d, want only handler state", len(records))
	}
	if records[0].NodeID != "handler-only" || records[0].ActiveHandlers != 1 {
		t.Fatalf("handler-only record = %+v, want active handler state", records[0])
	}
}

func TestDeleteExpiredCPANodeSnapshotsUsesRetention(t *testing.T) {
	db, errOpenSQLite := OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "home.db"))
	if errOpenSQLite != nil {
		t.Fatalf("OpenSQLite failed: %v", errOpenSQLite)
	}
	sqlDB, errDB := db.DB()
	if errDB != nil {
		t.Fatalf("get sql db: %v", errDB)
	}
	defer func() {
		if errClose := sqlDB.Close(); errClose != nil {
			t.Errorf("close sql db: %v", errClose)
		}
	}()
	if errMigrate := AutoMigrate(db); errMigrate != nil {
		t.Fatalf("AutoMigrate failed: %v", errMigrate)
	}

	repo := NewRepository(db)
	databaseNow, errNow := DatabaseNow(context.Background(), db)
	if errNow != nil {
		t.Fatalf("DatabaseNow() error = %v", errNow)
	}
	records := []CPANodeRecord{
		{HomeIP: "home-old", HomePort: 8327, HomeStartedAt: databaseNow.Add(-time.Hour), NodeKey: "fingerprint:old", CertificateFingerprint: "old", ClientCount: 1, LastSeenAt: databaseNow.Add(-time.Minute)},
		{HomeIP: "home-new", HomePort: 8327, HomeStartedAt: databaseNow, NodeKey: "fingerprint:new", CertificateFingerprint: "new", ClientCount: 1, LastSeenAt: databaseNow},
	}
	if errCreate := db.Create(&records).Error; errCreate != nil {
		t.Fatalf("create snapshots: %v", errCreate)
	}
	if errDelete := repo.DeleteExpiredCPANodeSnapshots(context.Background(), 30*time.Second); errDelete != nil {
		t.Fatalf("DeleteExpiredCPANodeSnapshots() error = %v", errDelete)
	}
	var remaining []CPANodeRecord
	if errFind := db.Order("node_key").Find(&remaining).Error; errFind != nil {
		t.Fatalf("find remaining snapshots: %v", errFind)
	}
	if len(remaining) != 1 || remaining[0].CertificateFingerprint != "new" {
		t.Fatalf("remaining snapshots = %+v, want only new", remaining)
	}
}

func TestAutoMigrateUpgradesLegacyCPANodePrimaryKey(t *testing.T) {
	db, errOpenSQLite := OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "home.db"))
	if errOpenSQLite != nil {
		t.Fatalf("OpenSQLite failed: %v", errOpenSQLite)
	}
	sqlDB, errDB := db.DB()
	if errDB != nil {
		t.Fatalf("get sql db: %v", errDB)
	}
	defer func() {
		if errClose := sqlDB.Close(); errClose != nil {
			t.Errorf("close sql db: %v", errClose)
		}
	}()
	if errCreate := db.Exec(`CREATE TABLE cpa_node (
		home_ip TEXT NOT NULL,
		home_port INTEGER NOT NULL,
		node_key TEXT NOT NULL,
		node_id TEXT,
		client_ip TEXT,
		client_count INTEGER,
		certificate_fingerprint TEXT,
		open_connections INTEGER,
		active_handlers INTEGER,
		latest_cancel_revision INTEGER,
		connected_at DATETIME,
		last_seen_at DATETIME,
		created_at DATETIME,
		updated_at DATETIME,
		PRIMARY KEY (home_ip, home_port, node_key)
	)`).Error; errCreate != nil {
		t.Fatalf("create legacy cpa_node table: %v", errCreate)
	}
	legacySeenAt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if errCreate := db.Exec(`INSERT INTO cpa_node (home_ip, home_port, node_key, node_id, client_count, connected_at, last_seen_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, "home-a", 8327, "legacy", "legacy", 1, legacySeenAt, legacySeenAt).Error; errCreate != nil {
		t.Fatalf("insert legacy cpa_node row: %v", errCreate)
	}

	if errMigrate := AutoMigrate(db); errMigrate != nil {
		t.Fatalf("AutoMigrate() error = %v", errMigrate)
	}
	if errMigrate := AutoMigrate(db); errMigrate != nil {
		t.Fatalf("second AutoMigrate() error = %v", errMigrate)
	}

	var legacy CPANodeRecord
	if errFind := db.Where("home_ip = ? AND home_port = ? AND node_key = ?", "home-a", 8327, "legacy").First(&legacy).Error; errFind != nil {
		t.Fatalf("find preserved legacy row: %v", errFind)
	}
	if legacy.NodeID != "legacy" || legacy.ClientCount != 1 || legacy.HomeStartedAt.IsZero() {
		t.Fatalf("preserved legacy row = %+v", legacy)
	}

	startedAtA := time.Date(2026, 1, 2, 4, 0, 0, 0, time.UTC)
	startedAtB := startedAtA.Add(time.Hour)
	for _, startedAt := range []time.Time{startedAtA, startedAtB} {
		record := CPANodeRecord{HomeIP: "home-a", HomePort: 8327, HomeStartedAt: startedAt, NodeKey: "same-node", NodeID: "same-node", ClientCount: 1, ConnectedAt: startedAt, LastSeenAt: startedAt}
		if errCreate := db.Create(&record).Error; errCreate != nil {
			t.Fatalf("insert cpa node for started_at %s: %v", startedAt, errCreate)
		}
	}
	var count int64
	if errCount := db.Model(&CPANodeRecord{}).Where("home_ip = ? AND home_port = ? AND node_key = ?", "home-a", 8327, "same-node").Count(&count).Error; errCount != nil {
		t.Fatalf("count upgraded cpa nodes: %v", errCount)
	}
	if count != 2 {
		t.Fatalf("upgraded cpa node rows = %d, want 2", count)
	}
}

func TestAutoMigrateUpgradesLegacyCPANodePrimaryKeyPostgres(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("CLIPROXY_HOME_TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("CLIPROXY_HOME_TEST_POSTGRES_DSN is not configured")
	}
	db, errOpen := gorm.Open(postgres.Open(dsn), databaseGORMConfig())
	if errOpen != nil {
		t.Fatalf("open postgres: %v", errOpen)
	}
	sqlDB, errDB := db.DB()
	if errDB != nil {
		t.Fatalf("get sql db: %v", errDB)
	}
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)
	schemaName := "cpa_node_upgrade_" + strconv.FormatInt(time.Now().UnixNano(), 10)
	if errCreate := db.Exec("CREATE SCHEMA " + schemaName).Error; errCreate != nil {
		t.Fatalf("create test schema: %v", errCreate)
	}
	if errSearchPath := db.Exec("SET search_path TO " + schemaName).Error; errSearchPath != nil {
		t.Fatalf("set test schema search path: %v", errSearchPath)
	}
	defer func() {
		if errSearchPath := db.Exec("RESET search_path").Error; errSearchPath != nil {
			t.Errorf("reset search path: %v", errSearchPath)
		}
		if errDrop := db.Exec("DROP SCHEMA " + schemaName + " CASCADE").Error; errDrop != nil {
			t.Errorf("drop test schema: %v", errDrop)
		}
		if errClose := sqlDB.Close(); errClose != nil {
			t.Errorf("close sql db: %v", errClose)
		}
	}()
	if errCreate := db.Exec(`CREATE TABLE cpa_node (
		home_ip TEXT NOT NULL,
		home_port INTEGER NOT NULL,
		node_key TEXT NOT NULL,
		node_id TEXT,
		client_ip TEXT,
		client_count INTEGER,
		certificate_fingerprint TEXT,
		open_connections INTEGER,
		active_handlers INTEGER,
		latest_cancel_revision BIGINT,
		connected_at TIMESTAMPTZ,
		last_seen_at TIMESTAMPTZ,
		created_at TIMESTAMPTZ,
		updated_at TIMESTAMPTZ,
		PRIMARY KEY (home_ip, home_port, node_key)
	)`).Error; errCreate != nil {
		t.Fatalf("create legacy cpa_node table: %v", errCreate)
	}
	legacySeenAt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if errCreate := db.Exec(`INSERT INTO cpa_node (home_ip, home_port, node_key, node_id, client_count, connected_at, last_seen_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, "home-a", 8327, "legacy", "legacy", 1, legacySeenAt, legacySeenAt).Error; errCreate != nil {
		t.Fatalf("insert legacy cpa_node row: %v", errCreate)
	}
	if errMigrate := AutoMigrate(db); errMigrate != nil {
		t.Fatalf("AutoMigrate() error = %v", errMigrate)
	}
	if errMigrate := AutoMigrate(db); errMigrate != nil {
		t.Fatalf("second AutoMigrate() error = %v", errMigrate)
	}
	var legacy CPANodeRecord
	if errFind := db.Where("home_ip = ? AND home_port = ? AND node_key = ?", "home-a", 8327, "legacy").First(&legacy).Error; errFind != nil {
		t.Fatalf("find preserved legacy row: %v", errFind)
	}
	if legacy.NodeID != "legacy" || legacy.ClientCount != 1 || legacy.HomeStartedAt.IsZero() {
		t.Fatalf("preserved legacy row = %+v", legacy)
	}
	startedAtA := time.Date(2026, 1, 2, 4, 0, 0, 0, time.UTC)
	startedAtB := startedAtA.Add(time.Hour)
	for _, startedAt := range []time.Time{startedAtA, startedAtB} {
		record := CPANodeRecord{HomeIP: "home-a", HomePort: 8327, HomeStartedAt: startedAt, NodeKey: "same-node", NodeID: "same-node", ClientCount: 1, ConnectedAt: startedAt, LastSeenAt: startedAt}
		if errCreate := db.Create(&record).Error; errCreate != nil {
			t.Fatalf("insert cpa node for started_at %s: %v", startedAt, errCreate)
		}
	}
	var count int64
	if errCount := db.Model(&CPANodeRecord{}).Where("home_ip = ? AND home_port = ? AND node_key = ?", "home-a", 8327, "same-node").Count(&count).Error; errCount != nil {
		t.Fatalf("count upgraded cpa nodes: %v", errCount)
	}
	if count != 2 {
		t.Fatalf("upgraded cpa node rows = %d, want 2", count)
	}
}

func TestMigrateCPANodePrimaryKeyPostgresUsesActualConstraintName(t *testing.T) {
	var logs bytes.Buffer
	db, errOpen := gorm.Open(postgres.New(postgres.Config{
		DSN:                  "host=127.0.0.1 user=cliproxy dbname=cliproxy_home sslmode=disable",
		PreferSimpleProtocol: true,
	}), &gorm.Config{
		DisableAutomaticPing: true,
		DryRun:               true,
		Logger: logger.New(log.New(&logs, "", 0), logger.Config{
			LogLevel: logger.Info,
		}),
	})
	if errOpen != nil {
		t.Fatalf("open postgres dry-run db: %v", errOpen)
	}

	if errMigrate := migrateCPANodePrimaryKey(db); errMigrate != nil {
		t.Fatalf("migrateCPANodePrimaryKey() error = %v", errMigrate)
	}
	output := logs.String()
	if !strings.Contains(output, "pg_constraint") || !strings.Contains(output, "conname") || !strings.Contains(output, "DROP CONSTRAINT") {
		t.Fatalf("postgres migration SQL = %q, want dynamic primary constraint lookup", output)
	}
}

func TestJSONBGormDBDataTypeForMigrators(t *testing.T) {
	t.Parallel()

	pgDB, errOpenPostgres := gorm.Open(postgres.New(postgres.Config{
		DSN:                  "host=127.0.0.1 user=cliproxy dbname=cliproxy_home sslmode=disable",
		PreferSimpleProtocol: true,
	}), &gorm.Config{DisableAutomaticPing: true})
	if errOpenPostgres != nil {
		t.Fatalf("open postgres dry-run db: %v", errOpenPostgres)
	}
	sqliteDB, errOpenSQLite := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if errOpenSQLite != nil {
		t.Fatalf("open sqlite db: %v", errOpenSQLite)
	}
	sqlDB, errDB := sqliteDB.DB()
	if errDB != nil {
		t.Fatalf("get sqlite sql db: %v", errDB)
	}
	defer func() {
		if errClose := sqlDB.Close(); errClose != nil {
			t.Errorf("close sqlite sql db: %v", errClose)
		}
	}()

	jsonField := configRecordJSONBField(t)
	assertJSONBFullDataType(t, pgDB, jsonField, "jsonb")
	assertJSONBFullDataType(t, sqliteDB, jsonField, "text")
}

func configRecordJSONBField(t *testing.T) *schema.Field {
	t.Helper()

	parsedSchema, errParse := schema.Parse(&ConfigRecord{}, &sync.Map{}, schema.NamingStrategy{})
	if errParse != nil {
		t.Fatalf("parse ConfigRecord schema: %v", errParse)
	}
	jsonField := parsedSchema.LookUpField("Value")
	if jsonField == nil {
		t.Fatalf("ConfigRecord.Value field not found")
	}
	return jsonField
}

func assertJSONBFullDataType(t *testing.T, db *gorm.DB, field *schema.Field, want string) {
	t.Helper()

	expr := db.Migrator().FullDataTypeOf(field)
	got := strings.ToLower(strings.TrimSpace(expr.SQL))
	if !strings.Contains(got, want) {
		t.Fatalf("%s JSONB data type = %q, want %q", db.Dialector.Name(), got, want)
	}
}
