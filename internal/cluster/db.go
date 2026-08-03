package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/glebarez/sqlite"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	migrationAdvisoryLockKey                  int64 = 749327842680272315
	usageCacheReadBackfillAdvisoryLockKey     int64 = 749327842680272316
	usageDerivedColumnBackfillAdvisoryLockKey int64 = 749327842680272317
	usageCacheReadBackfillBatchSize                 = 500
	usageDerivedColumnBackfillBatchSize             = 100
	usageCacheReadBackfillStateKey                  = "internal:migration:usage-cache-read:v1"
	usageDerivedColumnBackfillStateKey              = "internal:migration:usage-derived-columns:v1"
)

// Open opens the resource.
func Open(ctx context.Context, cfg PGSQLConfig) (*gorm.DB, error) {
	// Keep validation before state changes so failures leave existing data intact.
	if ctx == nil {
		ctx = context.Background()
	}

	dsn, errDSN := cfg.DSN()
	if errDSN != nil {
		return nil, errDSN
	}

	db, errOpen := gorm.Open(postgres.Open(dsn), databaseGORMConfig())
	if errOpen != nil {
		return nil, errOpen
	}

	sqlDB, errDB := db.DB()
	if errDB != nil {
		return nil, errDB
	}
	if errPing := sqlDB.PingContext(ctx); errPing != nil {
		if errClose := sqlDB.Close(); errClose != nil {
			return nil, fmt.Errorf("ping postgres: %w; close sql db: %v", errPing, errClose)
		}
		return nil, errPing
	}

	return db, nil
}

// OpenSQLite opens a SQLite database.
func OpenSQLite(ctx context.Context, path string) (*gorm.DB, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	path = strings.TrimSpace(path)
	if path == "" {
		path = "home.db"
	}
	db, errOpen := gorm.Open(sqlite.Open(path), databaseGORMConfig())
	if errOpen != nil {
		return nil, errOpen
	}
	sqlDB, errDB := db.DB()
	if errDB != nil {
		return nil, errDB
	}
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)
	if errPragma := db.Exec("PRAGMA journal_mode=WAL").Error; errPragma != nil {
		if errClose := sqlDB.Close(); errClose != nil {
			return nil, fmt.Errorf("configure sqlite journal mode: %w; close sql db: %v", errPragma, errClose)
		}
		return nil, errPragma
	}
	if errPragma := db.Exec("PRAGMA busy_timeout=30000").Error; errPragma != nil {
		if errClose := sqlDB.Close(); errClose != nil {
			return nil, fmt.Errorf("configure sqlite busy timeout: %w; close sql db: %v", errPragma, errClose)
		}
		return nil, errPragma
	}
	if errPragma := db.Exec("PRAGMA synchronous=NORMAL").Error; errPragma != nil {
		if errClose := sqlDB.Close(); errClose != nil {
			return nil, fmt.Errorf("configure sqlite synchronous mode: %w; close sql db: %v", errPragma, errClose)
		}
		return nil, errPragma
	}
	if errPing := sqlDB.PingContext(ctx); errPing != nil {
		if errClose := sqlDB.Close(); errClose != nil {
			return nil, fmt.Errorf("ping sqlite: %w; close sql db: %v", errPing, errClose)
		}
		return nil, errPing
	}
	return db, nil
}

// ClientAddr handles a client addr.
func ClientAddr(ctx context.Context, db *gorm.DB) (string, error) {
	return clientAddr(ctx, db)
}

// AutoMigrate handles an auto migrate.
func AutoMigrate(db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("database connection is nil")
	}
	if db.Dialector != nil && db.Dialector.Name() == "postgres" {
		return db.Transaction(func(tx *gorm.DB) error {
			if errLock := tx.Exec("SELECT pg_advisory_xact_lock(?)", migrationAdvisoryLockKey).Error; errLock != nil {
				return errLock
			}
			return autoMigrate(tx)
		})
	}
	return autoMigrate(db)
}

func autoMigrate(db *gorm.DB) error {
	if errMigrate := db.AutoMigrate(databaseMigrationModels()...); errMigrate != nil {
		return errMigrate
	}
	if errMigrate := migrateCPANodePrimaryKey(db); errMigrate != nil {
		return errMigrate
	}
	if errMigrate := migrateBillingIndexes(db); errMigrate != nil {
		return errMigrate
	}
	if errMigrate := migrateBillingImportIndexes(db); errMigrate != nil {
		return errMigrate
	}
	if errMigrate := migrateCertificateFingerprints(db); errMigrate != nil {
		return errMigrate
	}
	if errMigrate := migrateAPIKeyChannels(db); errMigrate != nil {
		return errMigrate
	}
	if errMigrate := migrateAPIKeyModelGroups(db); errMigrate != nil {
		return errMigrate
	}
	if errMigrate := migrateModelGroupDetailChannels(db); errMigrate != nil {
		return errMigrate
	}
	if errMigrate := migrateUserUniqueUsername(db); errMigrate != nil {
		return errMigrate
	}
	if errMigrate := migrateUserUniqueEmail(db); errMigrate != nil {
		return errMigrate
	}
	if errMigrate := migrateAuthNextRetryAfter(db); errMigrate != nil {
		return errMigrate
	}
	if errMigrate := migrateUsageObservabilityIndexes(db); errMigrate != nil {
		return errMigrate
	}
	if errMigrate := migrateUsageProviderAPIKeySources(db); errMigrate != nil {
		return errMigrate
	}
	if errMigrate := migrateUsageServiceTiers(db); errMigrate != nil {
		return errMigrate
	}
	return migrateLegacyAPIKeys(db)
}

func migrateCPANodePrimaryKey(db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("database connection is nil")
	}
	switch db.Dialector.Name() {
	case "sqlite":
		return migrateSQLiteCPANodePrimaryKey(db)
	case "postgres":
		return migratePostgresCPANodePrimaryKey(db)
	default:
		return fmt.Errorf("unsupported database dialect %q for cpa_node primary key migration", db.Dialector.Name())
	}
}

func migrateSQLiteCPANodePrimaryKey(db *gorm.DB) error {
	primaryKeyColumns, errColumns := sqliteTablePrimaryKeyColumns(db, "cpa_node")
	if errColumns != nil {
		return errColumns
	}
	if equalStringSlices(primaryKeyColumns, cpaNodePrimaryKeyColumns()) {
		return nil
	}
	if !equalStringSlices(primaryKeyColumns, legacyCPANodePrimaryKeyColumns()) {
		return fmt.Errorf("unsupported cpa_node primary key %q", primaryKeyColumns)
	}

	return db.Transaction(func(tx *gorm.DB) error {
		if errCreate := tx.Exec(`CREATE TABLE "cpa_node_migration" (
			"home_ip" TEXT NOT NULL,
			"home_port" INTEGER NOT NULL,
			"home_started_at" DATETIME NOT NULL,
			"node_key" TEXT NOT NULL,
			"node_id" TEXT,
			"client_ip" TEXT,
			"client_count" INTEGER,
			"certificate_fingerprint" TEXT,
			"open_connections" INTEGER,
			"active_handlers" INTEGER,
			"latest_cancel_revision" INTEGER,
			"connected_at" DATETIME,
			"last_seen_at" DATETIME,
			"created_at" DATETIME,
			"updated_at" DATETIME,
			PRIMARY KEY ("home_ip", "home_port", "home_started_at", "node_key")
		)`).Error; errCreate != nil {
			return errCreate
		}
		if errCopy := tx.Exec(`INSERT INTO "cpa_node_migration" (
			"home_ip", "home_port", "home_started_at", "node_key", "node_id", "client_ip", "client_count",
			"certificate_fingerprint", "open_connections", "active_handlers", "latest_cancel_revision",
			"connected_at", "last_seen_at", "created_at", "updated_at"
		)
		SELECT
			"home_ip", "home_port", COALESCE("home_started_at", "connected_at", "last_seen_at", "created_at", CURRENT_TIMESTAMP),
			"node_key", "node_id", "client_ip", "client_count", "certificate_fingerprint", "open_connections",
			"active_handlers", "latest_cancel_revision", "connected_at", "last_seen_at", "created_at", "updated_at"
		FROM "cpa_node"`).Error; errCopy != nil {
			return errCopy
		}
		if errDrop := tx.Migrator().DropTable("cpa_node"); errDrop != nil {
			return errDrop
		}
		if errRename := tx.Migrator().RenameTable("cpa_node_migration", "cpa_node"); errRename != nil {
			return errRename
		}
		return tx.AutoMigrate(&CPANodeRecord{})
	})
}

func migratePostgresCPANodePrimaryKey(db *gorm.DB) error {
	if errUpdate := db.Exec(`UPDATE "cpa_node"
		SET "home_started_at" = COALESCE("home_started_at", "connected_at", "last_seen_at", "created_at", CURRENT_TIMESTAMP)
		WHERE "home_started_at" IS NULL`).Error; errUpdate != nil {
		return errUpdate
	}
	return db.Exec(`DO $$
DECLARE
	legacy_constraint_name TEXT;
BEGIN
	SELECT constraint_record.conname INTO legacy_constraint_name
	FROM pg_constraint AS constraint_record
	WHERE constraint_record.conrelid = 'cpa_node'::regclass
		AND constraint_record.contype = 'p'
		AND (
			SELECT array_agg(attribute_record.attname::TEXT ORDER BY key_column.ordinality)
			FROM unnest(constraint_record.conkey) WITH ORDINALITY AS key_column(attnum, ordinality)
			JOIN pg_attribute AS attribute_record
				ON attribute_record.attrelid = constraint_record.conrelid
				AND attribute_record.attnum = key_column.attnum
		) = ARRAY['home_ip', 'home_port', 'node_key'];

	IF legacy_constraint_name IS NOT NULL THEN
		EXECUTE format('ALTER TABLE %I DROP CONSTRAINT %I', 'cpa_node', legacy_constraint_name);
		ALTER TABLE "cpa_node" ADD PRIMARY KEY ("home_ip", "home_port", "home_started_at", "node_key");
	END IF;
END $$`).Error
}

type sqliteTableInfo struct {
	Name string
	PK   int
}

func sqliteTablePrimaryKeyColumns(db *gorm.DB, table string) ([]string, error) {
	var columns []sqliteTableInfo
	if errQuery := db.Raw(`PRAGMA table_info("` + table + `")`).Scan(&columns).Error; errQuery != nil {
		return nil, errQuery
	}
	primaryKeyColumns := make([]string, 0, len(columns))
	for position := 1; position <= len(columns); position++ {
		for _, column := range columns {
			if column.PK == position {
				primaryKeyColumns = append(primaryKeyColumns, column.Name)
				break
			}
		}
	}
	return primaryKeyColumns, nil
}

func cpaNodePrimaryKeyColumns() []string {
	return []string{"home_ip", "home_port", "home_started_at", "node_key"}
}

func legacyCPANodePrimaryKeyColumns() []string {
	return []string{"home_ip", "home_port", "node_key"}
}

func equalStringSlices(left []string, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func migrateUsageObservabilityIndexes(db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("database connection is nil")
	}
	statements := []string{
		`CREATE INDEX IF NOT EXISTS idx_usage_provider_lower_time ON "usage" (LOWER("provider"), "timestamp" DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_usage_auth_type_normalized_time ON "usage" (LOWER(REPLACE("auth_type", '-', '_')), "timestamp" DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_usage_event_type_time ON "usage" ("event_type", "timestamp" DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_usage_cpa_node_time ON "usage" ("cpa_node_id", "timestamp" DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_usage_home_port_time ON "usage" ("home_ip", "home_port", "timestamp" DESC)`,
	}
	for _, statement := range statements {
		if errExec := db.Exec(statement).Error; errExec != nil {
			return errExec
		}
	}
	return nil
}

type usageDerivedColumnBackfillState struct {
	HighWaterID   uint `json:"high_water_id"`
	LastScannedID uint `json:"last_scanned_id"`
	Done          bool `json:"done"`
}

// UsageDerivedColumnBackfillResult describes one bounded historical usage metadata backfill batch.
type UsageDerivedColumnBackfillResult struct {
	Scanned int
	Updated int64
	Done    bool
	Skipped bool
}

// RunUsageDerivedColumnBackfillBatch advances the historical usage metadata
// migration without blocking startup on a full payload scan.
func (r *Repository) RunUsageDerivedColumnBackfillBatch(ctx context.Context) (UsageDerivedColumnBackfillResult, error) {
	db, errDB := r.database()
	if errDB != nil {
		return UsageDerivedColumnBackfillResult{}, errDB
	}
	return runUsageDerivedColumnBackfillBatch(contextOrBackground(ctx), db, usageDerivedColumnBackfillBatchSize)
}

func runUsageDerivedColumnBackfillBatch(ctx context.Context, db *gorm.DB, batchSize int) (UsageDerivedColumnBackfillResult, error) {
	if db == nil {
		return UsageDerivedColumnBackfillResult{}, fmt.Errorf("database connection is nil")
	}
	if batchSize <= 0 {
		return UsageDerivedColumnBackfillResult{}, fmt.Errorf("usage derived-column backfill batch size must be positive")
	}
	result := UsageDerivedColumnBackfillResult{}
	errTransaction := db.WithContext(contextOrBackground(ctx)).Transaction(func(tx *gorm.DB) error {
		if tx.Dialector != nil && tx.Dialector.Name() == "postgres" {
			var acquired bool
			if errLock := tx.Raw("SELECT pg_try_advisory_xact_lock(?)", usageDerivedColumnBackfillAdvisoryLockKey).Scan(&acquired).Error; errLock != nil {
				return errLock
			}
			if !acquired {
				result.Skipped = true
				return nil
			}
		}
		return runUsageDerivedColumnBackfillBatchTx(ctx, tx, batchSize, &result)
	})
	if errTransaction != nil {
		return UsageDerivedColumnBackfillResult{}, errTransaction
	}
	return result, nil
}

func runUsageDerivedColumnBackfillBatchTx(ctx context.Context, tx *gorm.DB, batchSize int, result *UsageDerivedColumnBackfillResult) error {
	if tx == nil {
		return fmt.Errorf("database transaction is nil")
	}
	if result == nil {
		return fmt.Errorf("usage derived-column backfill result is nil")
	}

	state, stateRecord, exists, errState := loadUsageDerivedColumnBackfillState(ctx, tx)
	if errState != nil {
		return errState
	}
	if state.Done {
		return runUsageDerivedColumnCatchUpBatchTx(ctx, tx, batchSize, result)
	}
	if !exists {
		var latest UsageRecord
		latestResult := tx.WithContext(contextOrBackground(ctx)).Select("id").Order("id DESC").Limit(1).Find(&latest)
		if latestResult.Error != nil {
			return latestResult.Error
		}
		state.HighWaterID = latest.ID
		if state.HighWaterID == 0 {
			state.Done = true
		}
	}
	if state.LastScannedID >= state.HighWaterID {
		state.Done = true
	}
	if state.Done {
		result.Done = true
		return saveUsageDerivedColumnBackfillState(ctx, tx, stateRecord, exists, state)
	}

	var records []UsageRecord
	if errFind := tx.WithContext(contextOrBackground(ctx)).
		Select("id", "payload", "endpoint", "home_ip", "home_port", "event_type", "upstream_request_id", "upstream_status_code", "cpa_node_id", "cpa_ip", "cpa_port", "cpa_label").
		Where("id > ? AND id <= ?", state.LastScannedID, state.HighWaterID).
		Order("id ASC").
		Limit(batchSize).
		Find(&records).Error; errFind != nil {
		return errFind
	}
	if len(records) == 0 {
		state.Done = true
		result.Done = true
		return saveUsageDerivedColumnBackfillState(ctx, tx, stateRecord, exists, state)
	}

	result.Scanned = len(records)
	if errUpdate := updateUsageDerivedColumnRecords(ctx, tx, records, result); errUpdate != nil {
		return errUpdate
	}
	state.LastScannedID = records[len(records)-1].ID
	state.Done = state.LastScannedID >= state.HighWaterID
	result.Done = state.Done
	return saveUsageDerivedColumnBackfillState(ctx, tx, stateRecord, exists, state)
}

func runUsageDerivedColumnCatchUpBatchTx(ctx context.Context, tx *gorm.DB, batchSize int, result *UsageDerivedColumnBackfillResult) error {
	var records []UsageRecord
	if errFind := tx.WithContext(contextOrBackground(ctx)).
		Select("id", "payload", "endpoint", "home_ip", "home_port", "event_type", "upstream_request_id", "upstream_status_code", "cpa_node_id", "cpa_ip", "cpa_port", "cpa_label").
		Where("event_type = '' OR event_type IS NULL").
		Limit(batchSize).
		Find(&records).Error; errFind != nil {
		return errFind
	}
	result.Scanned = len(records)
	if len(records) == 0 {
		result.Done = true
		return nil
	}
	if errUpdate := updateUsageDerivedColumnRecords(ctx, tx, records, result); errUpdate != nil {
		return errUpdate
	}
	result.Done = len(records) < batchSize
	return nil
}

func updateUsageDerivedColumnRecords(ctx context.Context, tx *gorm.DB, records []UsageRecord, result *UsageDerivedColumnBackfillResult) error {
	for _, record := range records {
		derived, errRecord := UsageRecordFromPayloadWithRuntime(string(record.PayloadJSON), UsageRuntimeMetadata{
			HomeIP:   record.HomeIP,
			HomePort: record.HomePort,
		})
		updates := map[string]any{}
		if errRecord != nil {
			if strings.TrimSpace(record.EventType) == "" {
				updates["event_type"] = usageEventTypeFromPayload(string(record.PayloadJSON), record.Endpoint)
			}
		} else {
			if strings.TrimSpace(record.EventType) == "" && strings.TrimSpace(derived.EventType) != "" {
				updates["event_type"] = derived.EventType
			}
			if strings.TrimSpace(record.UpstreamRequestID) == "" && strings.TrimSpace(derived.UpstreamRequestID) != "" {
				updates["upstream_request_id"] = derived.UpstreamRequestID
			}
			if record.UpstreamStatusCode == 0 && derived.UpstreamStatusCode > 0 {
				updates["upstream_status_code"] = derived.UpstreamStatusCode
			}
			if record.HomePort == 0 && derived.HomePort > 0 {
				updates["home_port"] = derived.HomePort
			}
			if strings.TrimSpace(record.CPANodeID) == "" && strings.TrimSpace(derived.CPANodeID) != "" {
				updates["cpa_node_id"] = derived.CPANodeID
			}
			if strings.TrimSpace(record.CPAIP) == "" && strings.TrimSpace(derived.CPAIP) != "" {
				updates["cpa_ip"] = derived.CPAIP
			}
			if record.CPAPort == 0 && derived.CPAPort > 0 {
				updates["cpa_port"] = derived.CPAPort
			}
			if strings.TrimSpace(record.CPALabel) == "" && strings.TrimSpace(derived.CPALabel) != "" {
				updates["cpa_label"] = derived.CPALabel
			}
		}
		if len(updates) == 0 {
			continue
		}
		update := tx.WithContext(contextOrBackground(ctx)).Model(&UsageRecord{}).Where("id = ?", record.ID).Updates(updates)
		if update.Error != nil {
			return update.Error
		}
		result.Updated += update.RowsAffected
	}
	return nil
}

func loadUsageDerivedColumnBackfillState(ctx context.Context, tx *gorm.DB) (usageDerivedColumnBackfillState, KVRecord, bool, error) {
	record := KVRecord{}
	errFind := tx.WithContext(contextOrBackground(ctx)).Clauses(clause.Locking{Strength: "UPDATE"}).Where("key = ?", usageDerivedColumnBackfillStateKey).First(&record).Error
	if errors.Is(errFind, gorm.ErrRecordNotFound) {
		return usageDerivedColumnBackfillState{}, KVRecord{}, false, nil
	}
	if errFind != nil {
		return usageDerivedColumnBackfillState{}, KVRecord{}, false, errFind
	}
	state := usageDerivedColumnBackfillState{}
	if errDecode := json.Unmarshal(record.Value, &state); errDecode != nil {
		return usageDerivedColumnBackfillState{}, KVRecord{}, false, fmt.Errorf("decode usage derived-column backfill state: %w", errDecode)
	}
	return state, record, true, nil
}

func saveUsageDerivedColumnBackfillState(ctx context.Context, tx *gorm.DB, record KVRecord, exists bool, state usageDerivedColumnBackfillState) error {
	value, errEncode := json.Marshal(state)
	if errEncode != nil {
		return fmt.Errorf("encode usage derived-column backfill state: %w", errEncode)
	}
	if !exists {
		return tx.WithContext(contextOrBackground(ctx)).Create(&KVRecord{Key: usageDerivedColumnBackfillStateKey, Value: value, Version: 1}).Error
	}
	record.Value = value
	record.Version++
	if record.Version <= 1 {
		record.Version = 1
	}
	return tx.WithContext(contextOrBackground(ctx)).Save(&record).Error
}

func migrateUsageProviderAPIKeySources(db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("database connection is nil")
	}
	var records []UsageRecord
	return db.
		Select("id", "payload", "source", "auth_index", "auth_type").
		Where(`LOWER(REPLACE("auth_type", '-', '_')) IN (?, ?, ?)`, "provider_api_key", "api_key", "apikey").
		Where(`COALESCE("source", '') <> CASE WHEN COALESCE("auth_index", '') <> '' THEN "auth_index" ELSE ? END`, "provider-api-key").
		FindInBatches(&records, 500, func(tx *gorm.DB, _ int) error {
			for _, record := range records {
				sanitized, errSanitize := sanitizeProviderAPIKeyUsageSource(string(record.PayloadJSON), record.AuthIndex)
				if errSanitize != nil {
					return errSanitize
				}
				source := usagePayloadString(sanitized, "source")
				if errUpdate := tx.Model(&UsageRecord{}).Where("id = ?", record.ID).Updates(map[string]any{
					"source":  source,
					"payload": JSONB(sanitized),
				}).Error; errUpdate != nil {
					return errUpdate
				}
			}
			return nil
		}).Error
}

// migrateUsageServiceTiers collapses the redundant legacy request tier into
// service_tier. The old column is intentionally retained in existing databases
// so upgrades remain non-destructive, but new records only use service_tier and
// response_service_tier.
func migrateUsageServiceTiers(db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("database connection is nil")
	}
	if db.Migrator().HasColumn(&UsageRecord{}, "request_service_tier") {
		return db.Exec(`UPDATE "usage"
			SET "service_tier" = CASE
				WHEN TRIM(COALESCE("request_service_tier", '')) <> '' THEN "request_service_tier"
				ELSE ?
			END
			WHERE "service_tier" IS NULL OR TRIM("service_tier") = ''`, defaultUsageServiceTier).Error
	}
	return db.Model(&UsageRecord{}).
		Where("service_tier IS NULL OR TRIM(service_tier) = ''").
		Update("service_tier", defaultUsageServiceTier).Error
}

type usageCacheReadBackfillState struct {
	HighWaterID   uint `json:"high_water_id"`
	LastScannedID uint `json:"last_scanned_id"`
	Done          bool `json:"done"`
}

// UsageCacheReadBackfillResult describes one bounded historical usage backfill batch.
type UsageCacheReadBackfillResult struct {
	Scanned int
	Updated int64
	Done    bool
	Skipped bool
}

// RunUsageCacheReadBackfillBatch advances the historical cache-read migration
// without holding the schema migration transaction open. CPA versions before
// v7.2.67 wrote cache reads to cached_tokens; new ingress and read paths remain
// compatible while this resumable maintenance task catches up. It updates usage
// dimensions only and never rewrites immutable billing charges or balances.
func (r *Repository) RunUsageCacheReadBackfillBatch(ctx context.Context) (UsageCacheReadBackfillResult, error) {
	db, errDB := r.database()
	if errDB != nil {
		return UsageCacheReadBackfillResult{}, errDB
	}
	return runUsageCacheReadBackfillBatch(contextOrBackground(ctx), db, usageCacheReadBackfillBatchSize)
}

func runUsageCacheReadBackfillBatch(ctx context.Context, db *gorm.DB, batchSize int) (UsageCacheReadBackfillResult, error) {
	if db == nil {
		return UsageCacheReadBackfillResult{}, fmt.Errorf("database connection is nil")
	}
	if batchSize <= 0 {
		return UsageCacheReadBackfillResult{}, fmt.Errorf("usage cache-read backfill batch size must be positive")
	}
	result := UsageCacheReadBackfillResult{}
	errTransaction := db.WithContext(contextOrBackground(ctx)).Transaction(func(tx *gorm.DB) error {
		if tx.Dialector != nil && tx.Dialector.Name() == "postgres" {
			var acquired bool
			if errLock := tx.Raw("SELECT pg_try_advisory_xact_lock(?)", usageCacheReadBackfillAdvisoryLockKey).Scan(&acquired).Error; errLock != nil {
				return errLock
			}
			if !acquired {
				result.Skipped = true
				return nil
			}
		}
		return runUsageCacheReadBackfillBatchTx(ctx, tx, batchSize, &result)
	})
	if errTransaction != nil {
		return UsageCacheReadBackfillResult{}, errTransaction
	}
	return result, nil
}

func runUsageCacheReadBackfillBatchTx(ctx context.Context, tx *gorm.DB, batchSize int, result *UsageCacheReadBackfillResult) error {
	if tx == nil {
		return fmt.Errorf("database transaction is nil")
	}
	if result == nil {
		return fmt.Errorf("usage cache-read backfill result is nil")
	}

	state, stateRecord, exists, errState := loadUsageCacheReadBackfillState(ctx, tx)
	if errState != nil {
		return errState
	}
	if state.Done {
		result.Done = true
		return nil
	}
	if !exists {
		var latest UsageRecord
		latestResult := tx.WithContext(contextOrBackground(ctx)).Order("id DESC").Limit(1).Find(&latest)
		if latestResult.Error != nil {
			return latestResult.Error
		}
		state.HighWaterID = latest.ID
		if state.HighWaterID == 0 {
			state.Done = true
		}
	}
	if state.LastScannedID >= state.HighWaterID {
		state.Done = true
	}
	if state.Done {
		result.Done = true
		return saveUsageCacheReadBackfillState(ctx, tx, stateRecord, exists, state)
	}

	ids := make([]uint, 0, batchSize)
	if errFind := tx.WithContext(contextOrBackground(ctx)).Model(&UsageRecord{}).
		Where("id > ? AND id <= ?", state.LastScannedID, state.HighWaterID).
		Order("id ASC").
		Limit(batchSize).
		Pluck("id", &ids).Error; errFind != nil {
		return errFind
	}
	if len(ids) == 0 {
		state.Done = true
		result.Done = true
		return saveUsageCacheReadBackfillState(ctx, tx, stateRecord, exists, state)
	}
	result.Scanned = len(ids)
	update := tx.WithContext(contextOrBackground(ctx)).Model(&UsageRecord{}).
		Where("id IN ? AND cache_read_tokens_present = ? AND cache_read_tokens = 0 AND cached_tokens > 0 AND "+usageCacheReadFallbackSQLCondition("provider", "executor_type"), ids, false).
		UpdateColumn("cache_read_tokens", gorm.Expr("cached_tokens"))
	if update.Error != nil {
		return update.Error
	}
	result.Updated = update.RowsAffected
	state.LastScannedID = ids[len(ids)-1]
	state.Done = state.LastScannedID >= state.HighWaterID
	result.Done = state.Done
	return saveUsageCacheReadBackfillState(ctx, tx, stateRecord, exists, state)
}

func loadUsageCacheReadBackfillState(ctx context.Context, tx *gorm.DB) (usageCacheReadBackfillState, KVRecord, bool, error) {
	record := KVRecord{}
	errFind := tx.WithContext(contextOrBackground(ctx)).Clauses(clause.Locking{Strength: "UPDATE"}).Where("key = ?", usageCacheReadBackfillStateKey).First(&record).Error
	if errors.Is(errFind, gorm.ErrRecordNotFound) {
		return usageCacheReadBackfillState{}, KVRecord{}, false, nil
	}
	if errFind != nil {
		return usageCacheReadBackfillState{}, KVRecord{}, false, errFind
	}
	state := usageCacheReadBackfillState{}
	if errDecode := json.Unmarshal(record.Value, &state); errDecode != nil {
		return usageCacheReadBackfillState{}, KVRecord{}, false, fmt.Errorf("decode usage cache-read backfill state: %w", errDecode)
	}
	return state, record, true, nil
}

func saveUsageCacheReadBackfillState(ctx context.Context, tx *gorm.DB, record KVRecord, exists bool, state usageCacheReadBackfillState) error {
	value, errEncode := json.Marshal(state)
	if errEncode != nil {
		return fmt.Errorf("encode usage cache-read backfill state: %w", errEncode)
	}
	if !exists {
		return tx.WithContext(contextOrBackground(ctx)).Create(&KVRecord{Key: usageCacheReadBackfillStateKey, Value: value, Version: 1}).Error
	}
	record.Value = value
	record.Version++
	if record.Version <= 1 {
		record.Version = 1
	}
	return tx.WithContext(contextOrBackground(ctx)).Save(&record).Error
}

func migrateAuthNextRetryAfter(db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("database connection is nil")
	}
	var records []AuthRecord
	if errFind := db.
		Where("next_retry_after IS NULL").
		Find(&records).Error; errFind != nil {
		return errFind
	}
	for _, record := range records {
		nextRetryAt := usageObservabilityAuthJSONNextRetryAt(string(record.AuthJSON))
		if nextRetryAt == nil || nextRetryAt.IsZero() {
			continue
		}
		nextRetryValue := nextRetryAt.UTC()
		if errUpdate := db.Model(&AuthRecord{}).
			Where("uuid = ?", record.UUID).
			Update("next_retry_after", nextRetryValue).Error; errUpdate != nil {
			return errUpdate
		}
	}
	return nil
}

func migrateUserUniqueUsername(db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("database connection is nil")
	}
	return db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_user_username_active_unique ON "user" ("username") WHERE "deleted_at" IS NULL`).Error
}

func migrateUserUniqueEmail(db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("database connection is nil")
	}
	if errCreate := db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_user_email_verified_active_unique ON "user" ("email") WHERE "deleted_at" IS NULL AND "email" IS NOT NULL AND "email_verified_at" IS NOT NULL`).Error; errCreate != nil {
		return errCreate
	}
	return db.Exec(`DROP INDEX IF EXISTS idx_user_email_active_unique`).Error
}

func migrateCertificateFingerprints(db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("database connection is nil")
	}
	var records []CertificateRecord
	if errFind := db.
		Where("certificate_pem <> ? AND COALESCE(certificate_fingerprint, '') = ?", "", "").
		Find(&records).Error; errFind != nil {
		return errFind
	}
	for _, record := range records {
		fingerprint, errFingerprint := certificateFingerprintPEM([]byte(record.CertificatePEM))
		if errFingerprint != nil {
			return errFingerprint
		}
		if errUpdate := db.Model(&CertificateRecord{}).
			Where("id = ?", record.ID).
			Update("certificate_fingerprint", fingerprint).Error; errUpdate != nil {
			return errUpdate
		}
	}
	return nil
}

func migrateLegacyAPIKeys(db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("database connection is nil")
	}

	record := ConfigRecord{}
	errFirst := db.Where("key = ?", configAPIKeysRootKey).First(&record).Error
	switch {
	case errors.Is(errFirst, gorm.ErrRecordNotFound):
		return nil
	case errFirst != nil:
		return errFirst
	}

	var apiKeys []string
	if len(record.Value) > 0 {
		if errUnmarshal := json.Unmarshal([]byte(record.Value), &apiKeys); errUnmarshal != nil {
			var rawList []any
			if errUnmarshalList := json.Unmarshal([]byte(record.Value), &rawList); errUnmarshalList != nil {
				return errUnmarshal
			}
			apiKeys = make([]string, 0, len(rawList))
			for _, item := range rawList {
				str, ok := item.(string)
				if !ok {
					continue
				}
				apiKeys = append(apiKeys, str)
			}
		}
	}
	apiKeys = normalizeAPIKeys(apiKeys)

	ctx := context.Background()
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if errReplace := replaceAPIKeysTx(ctx, tx, apiKeys); errReplace != nil {
			return errReplace
		}
		if errDelete := tx.Delete(&ConfigRecord{}, "key = ?", configAPIKeysRootKey).Error; errDelete != nil {
			return errDelete
		}
		return appendEvent(tx, "config", "migrate", configAPIKeysRootKey, 1)
	})
}

// DSN returns the PostgreSQL connection string.
func (c PGSQLConfig) DSN() (string, error) {
	if c.Password == "" && c.Passowrd != "" {
		c.Password = c.Passowrd
	}
	if c.SSLMode == "" {
		c.SSLMode = "disable"
	}
	if errValidate := c.Validate(); errValidate != nil {
		return "", errValidate
	}

	parts := []string{
		"host=" + quoteDSNValue(c.Host),
		"port=" + strconv.Itoa(c.Port),
		"user=" + quoteDSNValue(c.User),
		"password=" + quoteDSNValue(c.Password),
		"dbname=" + quoteDSNValue(c.Database),
		"sslmode=" + quoteDSNValue(c.SSLMode),
	}
	return strings.Join(parts, " "), nil
}

// clientAddr handles a client addr.
func clientAddr(ctx context.Context, db *gorm.DB) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if db == nil {
		return "", fmt.Errorf("database connection is nil")
	}

	var addr string
	if errScan := db.WithContext(ctx).Raw("SELECT inet_client_addr()").Scan(&addr).Error; errScan != nil {
		return "", errScan
	}
	if strings.TrimSpace(addr) == "" {
		return "", fmt.Errorf("postgres inet_client_addr() returned empty client address")
	}
	return addr, nil
}

// quoteDSNValue handles a quote dsn value.
func quoteDSNValue(value string) string {
	if value == "" {
		return "''"
	}
	if !strings.ContainsAny(value, " \t\r\n\\'") {
		return value
	}

	replacer := strings.NewReplacer(`\`, `\\`, `'`, `\'`)
	return "'" + replacer.Replace(value) + "'"
}
