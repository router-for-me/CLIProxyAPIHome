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

	db, errOpen := gorm.Open(postgres.Open(dsn), &gorm.Config{})
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
	db, errOpen := gorm.Open(sqlite.Open(path), &gorm.Config{})
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
	if errPragma := db.Exec("PRAGMA busy_timeout=5000").Error; errPragma != nil {
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
	if errMigrate := db.AutoMigrate(&AuthRecord{}, &ConfigRecord{}, &KVRecord{}, &PluginStatusRecord{}, &PluginTaskRecord{}, &UserRecord{}, &APIKeyRecord{}, &ChannelGroupRecord{}, &ChannelGroupDetailRecord{}, &ModelGroupRecord{}, &ModelGroupDetailRecord{}, &ClusterNodeRecord{}, &CPANodeRecord{}, &ClusterEventRecord{}, &UsageRecord{}, &BillingModelPriceRecord{}, &BillingBalanceRecord{}, &BillingChargeRecord{}, &ProxyPoolRecord{}, &AppLogRecord{}, &OAuthSessionRecord{}, &CertificateRecord{}); errMigrate != nil {
		return errMigrate
	}
	if errMigrate := migrateBillingIndexes(db); errMigrate != nil {
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
	if errMigrate := migrateUserUniqueUsername(db); errMigrate != nil {
		return errMigrate
	}
	if errMigrate := migrateAuthNextRetryAfter(db); errMigrate != nil {
		return errMigrate
	}
	if errMigrate := migrateUsageObservabilityIndexes(db); errMigrate != nil {
		return errMigrate
	}
	if errMigrate := migrateUsageDerivedColumns(db); errMigrate != nil {
		return errMigrate
	}
	return migrateLegacyAPIKeys(db)
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

func migrateUsageDerivedColumns(db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("database connection is nil")
	}
	var records []UsageRecord
	where, args := usageDerivedColumnBackfillWhere()
	return db.
		Select("id", "payload", "home_ip", "home_port", "event_type", "upstream_request_id", "upstream_status_code", "cpa_node_id", "cpa_ip", "cpa_port", "cpa_label").
		Where(where, args...).
		FindInBatches(&records, 500, func(tx *gorm.DB, _ int) error {
			for _, record := range records {
				derived, errRecord := UsageRecordFromPayloadWithRuntime(string(record.PayloadJSON), UsageRuntimeMetadata{
					HomeIP:   record.HomeIP,
					HomePort: record.HomePort,
				})
				if errRecord != nil {
					continue
				}
				updates := map[string]any{}
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
				if len(updates) == 0 {
					continue
				}
				if errUpdate := tx.Model(&UsageRecord{}).Where("id = ?", record.ID).Updates(updates).Error; errUpdate != nil {
					return errUpdate
				}
			}
			return nil
		}).Error
}

func usageDerivedColumnBackfillWhere() (string, []any) {
	upstreamRequestID := usagePayloadTextAnyCondition(
		usagePayloadTextContainsAny(`"upstream_request_id"`),
		usagePayloadTextContainsAll(`"upstream"`, `"request_id"`),
		usagePayloadTextContainsAll(`"response"`, `"request_id"`),
		usagePayloadTextContainsAll(`"response"`, `"id"`),
	)
	upstreamStatusCode := usagePayloadTextAnyCondition(
		usagePayloadTextContainsAny(`"upstream_status_code"`),
		usagePayloadTextContainsAll(`"upstream"`, `"status_code"`),
		usagePayloadTextContainsAll(`"response"`, `"status_code"`),
	)
	homePort := usagePayloadTextAnyCondition(
		usagePayloadTextContainsAny(`"home_port"`),
		usagePayloadTextContainsAll(`"home"`, `"port"`),
	)
	cpaNodeID := usagePayloadTextAnyCondition(
		usagePayloadTextContainsAny(`"cpa_node_id"`, `"node_id"`),
		usagePayloadTextContainsAll(`"cpa"`, `"node_id"`),
	)
	cpaIP := usagePayloadTextAnyCondition(
		usagePayloadTextContainsAny(`"cpa_ip"`),
		usagePayloadTextContainsAll(`"cpa"`, `"ip"`),
	)
	cpaPort := usagePayloadTextAnyCondition(
		usagePayloadTextContainsAny(`"cpa_port"`),
		usagePayloadTextContainsAll(`"cpa"`, `"port"`),
	)
	cpaLabel := usagePayloadTextAnyCondition(
		usagePayloadTextContainsAny(`"cpa_label"`, `"cpa_node_id"`, `"node_id"`, `"cpa_ip"`),
		usagePayloadTextContainsAll(`"cpa"`, `"label"`),
		usagePayloadTextContainsAll(`"cpa"`, `"node_id"`),
		usagePayloadTextContainsAll(`"cpa"`, `"ip"`),
	)

	clauses := []string{
		`event_type = '' OR event_type IS NULL`,
		`((upstream_request_id = '' OR upstream_request_id IS NULL) AND ` + upstreamRequestID.clause + `)`,
		`(upstream_status_code = 0 AND ` + upstreamStatusCode.clause + `)`,
		`(home_port = 0 AND ` + homePort.clause + `)`,
		`((cpa_node_id = '' OR cpa_node_id IS NULL) AND ` + cpaNodeID.clause + `)`,
		`((cpa_ip = '' OR cpa_ip IS NULL) AND ` + cpaIP.clause + `)`,
		`(cpa_port = 0 AND ` + cpaPort.clause + `)`,
		`((cpa_label = '' OR cpa_label IS NULL) AND ` + cpaLabel.clause + `)`,
	}
	args := make([]any, 0, upstreamRequestID.argCount()+upstreamStatusCode.argCount()+homePort.argCount()+cpaNodeID.argCount()+cpaIP.argCount()+cpaPort.argCount()+cpaLabel.argCount())
	for _, condition := range []usagePayloadTextCondition{upstreamRequestID, upstreamStatusCode, homePort, cpaNodeID, cpaIP, cpaPort, cpaLabel} {
		args = append(args, condition.args...)
	}
	return strings.Join(clauses, " OR "), args
}

type usagePayloadTextCondition struct {
	clause string
	args   []any
}

func (c usagePayloadTextCondition) argCount() int {
	return len(c.args)
}

func usagePayloadTextContainsAny(markers ...string) usagePayloadTextCondition {
	conditions := make([]string, 0, len(markers))
	args := make([]any, 0, len(markers))
	for _, marker := range markers {
		marker = strings.TrimSpace(marker)
		if marker == "" {
			continue
		}
		conditions = append(conditions, `CAST("payload" AS TEXT) LIKE ?`)
		args = append(args, "%"+marker+"%")
	}
	if len(conditions) == 0 {
		return usagePayloadTextCondition{clause: "1 = 0"}
	}
	return usagePayloadTextCondition{clause: "(" + strings.Join(conditions, " OR ") + ")", args: args}
}

func usagePayloadTextContainsAll(markers ...string) usagePayloadTextCondition {
	conditions := make([]string, 0, len(markers))
	args := make([]any, 0, len(markers))
	for _, marker := range markers {
		marker = strings.TrimSpace(marker)
		if marker == "" {
			continue
		}
		conditions = append(conditions, `CAST("payload" AS TEXT) LIKE ?`)
		args = append(args, "%"+marker+"%")
	}
	if len(conditions) == 0 {
		return usagePayloadTextCondition{clause: "1 = 0"}
	}
	return usagePayloadTextCondition{clause: "(" + strings.Join(conditions, " AND ") + ")", args: args}
}

func usagePayloadTextAnyCondition(conditions ...usagePayloadTextCondition) usagePayloadTextCondition {
	clauses := make([]string, 0, len(conditions))
	args := []any{}
	for _, condition := range conditions {
		if strings.TrimSpace(condition.clause) == "" {
			continue
		}
		clauses = append(clauses, condition.clause)
		args = append(args, condition.args...)
	}
	if len(clauses) == 0 {
		return usagePayloadTextCondition{clause: "1 = 0"}
	}
	return usagePayloadTextCondition{clause: "(" + strings.Join(clauses, " OR ") + ")", args: args}
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
