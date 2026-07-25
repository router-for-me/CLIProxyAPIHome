package cluster

import (
	"bytes"
	"context"
	"errors"
	stdlog "log"
	"strings"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	homelogging "github.com/router-for-me/CLIProxyAPIHome/internal/logging"
	log "github.com/sirupsen/logrus"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

type parameterizedSQLCapture struct {
	templates []string
	queries   []string
}

func (c *parameterizedSQLCapture) LogMode(gormlogger.LogLevel) gormlogger.Interface {
	return c
}

func (c *parameterizedSQLCapture) Info(context.Context, string, ...interface{}) {}

func (c *parameterizedSQLCapture) Warn(context.Context, string, ...interface{}) {}

func (c *parameterizedSQLCapture) Error(context.Context, string, ...interface{}) {}

func (c *parameterizedSQLCapture) Trace(ctx context.Context, _ time.Time, fc func() (string, int64), _ error) {
	sql, _ := fc()
	c.templates = append(c.templates, sql)
	c.queries = append(c.queries, databaseQueryName(ctx))
}

func captureParameterizedSQL(t *testing.T, query func(*gorm.DB) error) string {
	t.Helper()
	capture := &parameterizedSQLCapture{}
	db, errOpen := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: newParameterizedGORMLogger(capture)})
	if errOpen != nil {
		t.Fatalf("open template-capture database: %v", errOpen)
	}
	configureDatabaseGORMClauseBuilders(db)
	sqlDB, errDB := db.DB()
	if errDB != nil {
		t.Fatalf("get template-capture database: %v", errDB)
	}
	t.Cleanup(func() {
		if errClose := sqlDB.Close(); errClose != nil {
			t.Errorf("close template-capture database: %v", errClose)
		}
	})
	capture.templates = nil
	capture.queries = nil
	errQuery := query(db)
	if len(capture.templates) != 1 {
		t.Fatalf("captured SQL templates = %d, want 1 (query error: %v)", len(capture.templates), errQuery)
	}
	return capture.templates[0]
}

func TestDatabaseQueryNamesReachGORMLogger(t *testing.T) {
	capture := &parameterizedSQLCapture{}
	db, errOpen := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: newParameterizedGORMLogger(capture)})
	if errOpen != nil {
		t.Fatalf("open query-name database: %v", errOpen)
	}
	configureDatabaseGORMClauseBuilders(db)
	sqlDB, errDB := db.DB()
	if errDB != nil {
		t.Fatalf("get query-name database: %v", errDB)
	}
	t.Cleanup(func() {
		if errClose := sqlDB.Close(); errClose != nil {
			t.Errorf("close query-name database: %v", errClose)
		}
	})
	if errMigrate := AutoMigrate(db); errMigrate != nil {
		t.Fatalf("migrate query-name database: %v", errMigrate)
	}
	capture.templates = nil
	capture.queries = nil

	repo := NewRepository(db)
	ctx := context.Background()
	if _, errList := repo.ListUsageObservabilityRecords(ctx, UsageObservabilityRecordQuery{}); errList != nil {
		t.Fatalf("list usage records: %v", errList)
	}
	if _, errList := repo.ListUsageObservabilityAggregates(ctx, UsageObservabilityAggregateQuery{GroupBy: "model"}); errList != nil {
		t.Fatalf("list usage aggregates: %v", errList)
	}
	if _, errP95 := usageObservabilityAggregateP95Values(db, UsageObservabilityRecordQuery{}, "model", []string{"test-model"}); errP95 != nil {
		t.Fatalf("query usage aggregate p95: %v", errP95)
	}
	if _, errTrend := usageObservabilityTrendSQL(db, UsageObservabilityRecordQuery{}, "hour", time.UTC, usageObservabilityOverviewBounds{}); errTrend != nil {
		t.Fatalf("query usage trend: %v", errTrend)
	}
	if _, _, errAuth := repo.GetAuth(ctx, "11111111-1111-4111-8111-111111111111"); !errors.Is(errAuth, gorm.ErrRecordNotFound) {
		t.Fatalf("get missing auth error = %v", errAuth)
	}
	if _, errIndex := repo.ListAuthIndex(ctx); errIndex != nil {
		t.Fatalf("list auth index: %v", errIndex)
	}
	watcher := NewEventWatcher(repo, time.Second, func(context.Context, ClusterEventRecord) error { return nil })
	if _, errEvents := watcher.eventsAfter(ctx, 0); errEvents != nil {
		t.Fatalf("poll cluster events: %v", errEvents)
	}
	if _, errMax := repo.MaxEventID(ctx); errMax != nil {
		t.Fatalf("read max cluster event: %v", errMax)
	}
	if _, errNow := DatabaseNow(ctx, db); errNow != nil {
		t.Fatalf("read cluster time: %v", errNow)
	}
	if _, errPending := firstPendingUsageTokenAccountingRecord(ctx, db); errPending != nil {
		t.Fatalf("read pending token accounting: %v", errPending)
	}
	if _, errClaim := repo.ClaimQuotaProbe(ctx, "11111111-1111-4111-8111-111111111111", "query-name-test", time.Now().UTC(), time.Minute); errClaim != nil {
		t.Fatalf("claim quota probe: %v", errClaim)
	}

	seen := make(map[string]bool, len(capture.queries))
	for _, query := range capture.queries {
		seen[query] = true
	}
	for _, query := range []string{
		"usage.aggregate.count",
		"usage.aggregate.items",
		"usage.aggregate.p95",
		"usage.records.list",
		"usage.records.count",
		"usage.trend.points",
		"auth.credential.get",
		"auth.index.list",
		"cluster.events.poll",
		"cluster.time.read",
		"quota.snapshot.lock",
		"usage.token_accounting.pending",
	} {
		if !seen[query] {
			t.Errorf("query name %q did not reach GORM logger; captured: %v", query, capture.queries)
		}
	}
}

func TestDatabaseGORMConfigRedactsParameters(t *testing.T) {
	config := databaseGORMConfig()
	filter, ok := config.Logger.(gorm.ParamsFilter)
	if !ok {
		t.Fatal("database GORM logger does not filter parameters")
	}
	query := "INSERT INTO plugin_store_auth_key (key) VALUES (?)"
	filteredQuery, params := filter.ParamsFilter(context.Background(), query, bytes.Repeat([]byte{'K'}, 32))
	if filteredQuery != query || len(params) != 0 {
		t.Fatal("database GORM logger retained SQL parameters")
	}
	if _, okInfo := config.Logger.LogMode(gormlogger.Info).(gorm.ParamsFilter); !okInfo {
		t.Fatal("database GORM logger lost parameter filtering after LogMode")
	}
}

func TestGORMParamsFilterReceivesStableParameterizedTemplates(t *testing.T) {
	authTemplate := func(uuid string) string {
		return captureParameterizedSQL(t, func(db *gorm.DB) error {
			return db.Where("uuid = ?", uuid).First(&AuthRecord{}).Error
		})
	}
	firstAuthSQL := authTemplate("11111111-1111-4111-8111-111111111111")
	secondAuthSQL := authTemplate("22222222-2222-4222-8222-222222222222")
	if firstAuthSQL != secondAuthSQL {
		t.Fatalf("auth SQL template changed with UUID:\nfirst:  %s\nsecond: %s", firstAuthSQL, secondAuthSQL)
	}
	if bytes.Contains([]byte(firstAuthSQL), []byte("11111111-1111-4111-8111-111111111111")) ||
		bytes.Contains([]byte(secondAuthSQL), []byte("22222222-2222-4222-8222-222222222222")) {
		t.Fatal("auth SQL template contains a bound UUID")
	}
	if !bytes.Contains([]byte(firstAuthSQL), []byte("uuid = ?")) {
		t.Fatalf("auth SQL template lost its UUID placeholder: %s", firstAuthSQL)
	}

	aggregateTemplate := func(from time.Time, to time.Time, limit int, groupBy string, metric string, direction string) string {
		return captureParameterizedSQL(t, func(db *gorm.DB) error {
			_, errQuery := usageObservabilityAggregateItemsSQL(db, UsageObservabilityRecordQuery{From: &from, To: &to}, groupBy, metric, direction, limit)
			return errQuery
		})
	}
	firstFrom := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	firstTo := firstFrom.Add(24 * time.Hour)
	secondFrom := time.Date(2026, time.June, 15, 0, 0, 0, 0, time.UTC)
	secondTo := secondFrom.Add(7 * 24 * time.Hour)
	firstAggregateSQL := aggregateTemplate(firstFrom, firstTo, 10, "model", "request_count", "desc")
	secondAggregateSQL := aggregateTemplate(secondFrom, secondTo, 10, "model", "request_count", "desc")
	if firstAggregateSQL != secondAggregateSQL {
		t.Fatalf("aggregate SQL template changed with time range:\nfirst:  %s\nsecond: %s", firstAggregateSQL, secondAggregateSQL)
	}
	if bytes.Contains([]byte(firstAggregateSQL), []byte(firstFrom.Format("2006-01-02"))) ||
		bytes.Contains([]byte(secondAggregateSQL), []byte(secondFrom.Format("2006-01-02"))) {
		t.Fatal("aggregate SQL template contains a bound time range")
	}

	limitedAggregateSQL := aggregateTemplate(firstFrom, firstTo, 50, "model", "request_count", "desc")
	if firstAggregateSQL != limitedAggregateSQL {
		t.Fatalf("aggregate SQL template changed with LIMIT:\nfirst:  %s\nsecond: %s", firstAggregateSQL, limitedAggregateSQL)
	}
	if databaseSQLID(firstAggregateSQL) != databaseSQLID(secondAggregateSQL) || databaseSQLID(firstAggregateSQL) != databaseSQLID(limitedAggregateSQL) {
		t.Fatal("aggregate parameter changes produced different SQL IDs")
	}

	credentialSQL := aggregateTemplate(firstFrom, firstTo, 10, "credential", "request_count", "desc")
	if firstAggregateSQL == credentialSQL || databaseSQLID(firstAggregateSQL) == databaseSQLID(credentialSQL) {
		t.Fatal("credential and model aggregate structures produced the same SQL template")
	}
	p95SQL := aggregateTemplate(firstFrom, firstTo, 10, "model", "p95_latency_ms", "desc")
	if firstAggregateSQL == p95SQL || databaseSQLID(firstAggregateSQL) == databaseSQLID(p95SQL) {
		t.Fatal("adding the aggregate p95 JOIN did not change the SQL template")
	}
	ascendingSQL := aggregateTemplate(firstFrom, firstTo, 10, "model", "request_count", "asc")
	if firstAggregateSQL == ascendingSQL || databaseSQLID(firstAggregateSQL) == databaseSQLID(ascendingSQL) {
		t.Fatal("changing aggregate ORDER BY did not change the SQL template")
	}
}

func TestSQLiteParameterizedLimitPreservesQuerySemantics(t *testing.T) {
	db, errOpen := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)})
	if errOpen != nil {
		t.Fatalf("open SQLite limit database: %v", errOpen)
	}
	configureDatabaseGORMClauseBuilders(db)
	sqlDB, errDB := db.DB()
	if errDB != nil {
		t.Fatalf("get SQLite limit database: %v", errDB)
	}
	t.Cleanup(func() {
		if errClose := sqlDB.Close(); errClose != nil {
			t.Errorf("close SQLite limit database: %v", errClose)
		}
	})
	if errCreate := db.Exec("CREATE TABLE limit_test (value INTEGER NOT NULL)").Error; errCreate != nil {
		t.Fatalf("create SQLite limit table: %v", errCreate)
	}
	for value := 1; value <= 4; value++ {
		if errInsert := db.Exec("INSERT INTO limit_test (value) VALUES (?)", value).Error; errInsert != nil {
			t.Fatalf("insert SQLite limit value %d: %v", value, errInsert)
		}
	}
	tests := []struct {
		name   string
		limit  int
		offset int
		want   []int
	}{
		{name: "limit only", limit: 2, want: []int{1, 2}},
		{name: "limit and offset", limit: 2, offset: 1, want: []int{2, 3}},
		{name: "offset only", offset: 2, want: []int{3, 4}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			query := db.Table("limit_test").Order("value ASC")
			if test.limit > 0 {
				query = query.Limit(test.limit)
			}
			if test.offset > 0 {
				query = query.Offset(test.offset)
			}
			var values []int
			if errFind := query.Pluck("value", &values).Error; errFind != nil {
				t.Fatalf("query SQLite LIMIT/OFFSET: %v", errFind)
			}
			if len(values) != len(test.want) {
				t.Fatalf("SQLite LIMIT/OFFSET values = %v, want %v", values, test.want)
			}
			for index := range values {
				if values[index] != test.want[index] {
					t.Fatalf("SQLite LIMIT/OFFSET values = %v, want %v", values, test.want)
				}
			}
		})
	}

	var rows []struct{ Value int }
	dryRun := db.Session(&gorm.Session{DryRun: true}).Table("limit_test").Order("value ASC").Offset(2).Find(&rows)
	if got, want := dryRun.Statement.SQL.String(), "SELECT * FROM `limit_test` ORDER BY value ASC LIMIT ? OFFSET ?"; got != want {
		t.Fatalf("SQLite offset-only SQL = %q, want %q", got, want)
	}
	if len(dryRun.Statement.Vars) != 2 || dryRun.Statement.Vars[0] != -1 || dryRun.Statement.Vars[1] != 2 {
		t.Fatalf("SQLite offset-only vars = %v, want [-1 2]", dryRun.Statement.Vars)
	}
}

func TestHomeGORMLoggerUsesApplicationFormat(t *testing.T) {
	tests := []struct {
		name        string
		elapsed     time.Duration
		err         error
		wantLevel   string
		wantMessage string
	}{
		{name: "slow query", elapsed: time.Second, wantLevel: "[warn ]", wantMessage: "SLOW SQL >= 200ms"},
		{name: "query error", err: errors.New("database\nunavailable"), wantLevel: "[error]", wantMessage: "error=database unavailable"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			baseLogger := log.New()
			baseLogger.SetOutput(&output)
			baseLogger.SetFormatter(&homelogging.LogFormatter{})

			queryLog := newDatabaseQueryLog(8, time.Minute)
			logger := newParameterizedGORMLogger(newHomeGORMLoggerWithQueryLog(baseLogger, queryLog))
			logger.Trace(context.Background(), time.Now().Add(-test.elapsed), func() (string, int64) {
				return "SELECT *\nFROM records", 0
			}, test.err)

			logs := output.Bytes()
			if !bytes.HasPrefix(logs, []byte(homelogging.FormatLogSourcePrefix("CLIProxyAPIHome"))) {
				t.Fatalf("GORM log = %q, want application source prefix", logs)
			}
			if !bytes.Contains(logs, []byte(test.wantLevel)) {
				t.Fatalf("GORM log = %q, want level %q", logs, test.wantLevel)
			}
			if !bytes.Contains(logs, []byte(test.wantMessage)) {
				t.Fatalf("GORM log = %q, want message %q", logs, test.wantMessage)
			}
			if !bytes.Contains(logs, []byte("SQL DEF query=database.unclassified sql_id=")) {
				t.Fatalf("GORM log = %q, want SQL definition", logs)
			}
			if bytes.Count(logs, []byte{'\n'}) != 2 {
				t.Fatalf("GORM log = %q, want two formatted lines", logs)
			}
		})
	}
}

func TestHomeGORMLoggerDoesNotRegisterFastQueries(t *testing.T) {
	var output bytes.Buffer
	baseLogger := log.New()
	baseLogger.SetOutput(&output)
	baseLogger.SetFormatter(&homelogging.LogFormatter{})
	queryLog := newDatabaseQueryLog(8, time.Minute)
	logger := newParameterizedGORMLogger(newHomeGORMLoggerWithQueryLog(baseLogger, queryLog))
	logger.Trace(context.Background(), time.Now(), func() (string, int64) {
		return "SELECT * FROM records WHERE id = ?", 1
	}, nil)
	if output.Len() != 0 {
		t.Fatalf("fast query log = %q, want empty", output.Bytes())
	}
	queryLog.mu.Lock()
	entryCount := len(queryLog.entries)
	queryLog.mu.Unlock()
	if entryCount != 0 {
		t.Fatalf("fast query catalog entries = %d, want 0", entryCount)
	}
}

func TestHomeGORMLoggerAggregatesRepeatedSlowQueries(t *testing.T) {
	var output bytes.Buffer
	baseLogger := log.New()
	baseLogger.SetOutput(&output)
	baseLogger.SetFormatter(&homelogging.LogFormatter{})
	now := time.Date(2026, time.July, 25, 0, 0, 0, 0, time.UTC)
	queryLog := newDatabaseQueryLog(8, time.Minute)
	queryLog.now = func() time.Time { return now }
	logger := newParameterizedGORMLogger(newHomeGORMLoggerWithQueryLog(baseLogger, queryLog))
	ctx := WithDatabaseQueryName(context.Background(), "usage.aggregate.items")
	trace := func(elapsed time.Duration) {
		logger.Trace(ctx, time.Now().Add(-elapsed), func() (string, int64) {
			return "SELECT * FROM usage WHERE timestamp >= ?", 8
		}, nil)
	}

	trace(250 * time.Millisecond)
	firstLogs := output.String()
	if strings.Count(firstLogs, "SQL DEF ") != 1 || strings.Count(firstLogs, "SLOW SQL") != 1 {
		t.Fatalf("first slow logs = %q", firstLogs)
	}
	now = now.Add(10 * time.Second)
	trace(300 * time.Millisecond)
	if got := output.String(); got != firstLogs {
		t.Fatalf("suppressed slow query changed logs: %q", got)
	}
	now = now.Add(51 * time.Second)
	trace(600 * time.Millisecond)
	logs := output.String()
	if strings.Count(logs, "SQL DEF ") != 1 || strings.Count(logs, "SLOW SQL") != 2 {
		t.Fatalf("aggregated slow logs = %q", logs)
	}
	if strings.Count(logs, "sql=SELECT * FROM usage WHERE timestamp >= ?") != 1 {
		t.Fatalf("slow SQL definition repeated in short logs: %q", logs)
	}
	if !strings.Contains(logs, "suppressed=1 avg=300ms max=300ms") {
		t.Fatalf("aggregated slow log missing summary: %q", logs)
	}
}

func TestHomeGORMLoggerEmitsEverySQLError(t *testing.T) {
	var output bytes.Buffer
	baseLogger := log.New()
	baseLogger.SetOutput(&output)
	baseLogger.SetFormatter(&homelogging.LogFormatter{})
	queryLog := newDatabaseQueryLog(8, time.Minute)
	logger := newParameterizedGORMLogger(newHomeGORMLoggerWithQueryLog(baseLogger, queryLog))
	ctx := WithDatabaseQueryName(context.Background(), "usage.aggregate.items")
	for range 2 {
		logger.Trace(ctx, time.Now(), func() (string, int64) {
			return "SELECT missing FROM usage WHERE timestamp >= ?", -1
		}, errors.New("column \"missing\" does not exist"))
	}
	logs := output.String()
	if strings.Count(logs, "SQL DEF ") != 1 {
		t.Fatalf("SQL definition count in %q", logs)
	}
	if strings.Count(logs, "SQL ERROR ") != 2 {
		t.Fatalf("SQL error count in %q", logs)
	}
	if strings.Count(logs, "sql=SELECT missing FROM usage WHERE timestamp >= ?") != 1 || !strings.Contains(logs, "[rows:-]") {
		t.Fatalf("SQL error definition or rows format changed: %q", logs)
	}
	if strings.Count(logs, "column \"missing\" does not exist") != 2 {
		t.Fatalf("SQL errors lost error details: %q", logs)
	}
}

func TestHomeGORMLoggerDoesNotTruncateLongSQLDefinition(t *testing.T) {
	from := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	to := from.Add(24 * time.Hour)
	sql := captureParameterizedSQL(t, func(db *gorm.DB) error {
		_, errQuery := usageObservabilityAggregateItemsSQL(db, UsageObservabilityRecordQuery{From: &from, To: &to}, "model", "p95_latency_ms", "desc", 10)
		return errQuery
	})
	canonicalSQL := canonicalDatabaseSQL(sql)
	if len(canonicalSQL) < 14318 {
		t.Fatalf("production aggregate SQL length = %d, want at least 14318", len(canonicalSQL))
	}
	if !strings.Contains(canonicalSQL, "?") || strings.Contains(canonicalSQL, from.Format("2006-01-02")) {
		t.Fatal("production aggregate SQL did not retain parameter placeholders")
	}

	var output bytes.Buffer
	baseLogger := log.New()
	baseLogger.SetOutput(&output)
	baseLogger.SetFormatter(&homelogging.LogFormatter{})
	queryLog := newDatabaseQueryLog(8, time.Minute)
	logger := newParameterizedGORMLogger(newHomeGORMLoggerWithQueryLog(baseLogger, queryLog))
	logger.Trace(WithDatabaseQueryName(context.Background(), "usage.aggregate.items"), time.Now().Add(-time.Second), func() (string, int64) {
		return sql, 8
	}, nil)
	logs := output.String()
	if !strings.Contains(logs, "sql="+canonicalSQL) {
		t.Fatal("SQL definition was truncated or split")
	}
	if strings.Count(logs, "\n") != 2 {
		t.Fatalf("long SQL logs contain %d lines, want 2", strings.Count(logs, "\n"))
	}
}

func TestParameterizedGORMLoggerIgnoresRecordNotFoundAsError(t *testing.T) {
	tests := []struct {
		name          string
		slowThreshold time.Duration
		elapsed       time.Duration
		wantSlow      bool
	}{
		{name: "fast query", slowThreshold: time.Hour, wantSlow: false},
		{name: "slow query", slowThreshold: time.Millisecond, elapsed: time.Second, wantSlow: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			inner := gormlogger.New(stdlog.New(&output, "", 0), gormlogger.Config{
				SlowThreshold: test.slowThreshold,
				LogLevel:      gormlogger.Warn,
				Colorful:      false,
			})
			redacted := newParameterizedGORMLogger(inner)
			redacted.Trace(context.Background(), time.Now().Add(-test.elapsed), func() (string, int64) {
				return "SELECT * FROM records WHERE id = ?", 0
			}, gorm.ErrRecordNotFound)

			logs := output.Bytes()
			if bytes.Contains(logs, []byte("record not found")) {
				t.Fatal("record-not-found error was emitted")
			}
			if gotSlow := bytes.Contains(logs, []byte("SLOW SQL")); gotSlow != test.wantSlow {
				t.Fatalf("slow SQL emitted = %t, want %t", gotSlow, test.wantSlow)
			}
		})
	}
}

func TestParameterizedGORMLoggerRedactsSensitiveTraceValues(t *testing.T) {
	var output bytes.Buffer
	inner := gormlogger.New(stdlog.New(&output, "", 0), gormlogger.Config{
		SlowThreshold:        -time.Nanosecond,
		LogLevel:             gormlogger.Warn,
		Colorful:             false,
		ParameterizedQueries: false,
	})
	redacted := newParameterizedGORMLogger(inner).LogMode(gormlogger.Warn)
	db, errOpen := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: redacted})
	if errOpen != nil {
		t.Fatalf("open test database: %v", errOpen)
	}
	sqlDB, errDB := db.DB()
	if errDB != nil {
		t.Fatalf("get sql database: %v", errDB)
	}
	t.Cleanup(func() {
		if errClose := sqlDB.Close(); errClose != nil {
			t.Errorf("close sql database: %v", errClose)
		}
	})
	output.Reset()

	printableKey := bytes.Repeat([]byte{'K'}, 32)
	printableCiphertext := bytes.Repeat([]byte{'C'}, 64)
	if errExec := db.Exec("SELECT ?", printableKey).Error; errExec != nil {
		t.Fatalf("execute slow query: %v", errExec)
	}
	if errExec := db.Exec("SELECT * FROM missing_plugin_store_auth WHERE encrypted_credentials = ?", printableCiphertext).Error; errExec == nil {
		t.Fatal("sensitive SQL error query unexpectedly succeeded")
	}

	logs := output.Bytes()
	if !bytes.Contains(logs, []byte("SLOW SQL")) {
		t.Fatal("slow SQL trace was not emitted")
	}
	if !bytes.Contains(logs, []byte("missing_plugin_store_auth")) {
		t.Fatal("SQL error trace was not emitted")
	}
	if !bytes.Contains(logs, []byte("SELECT ?")) || !bytes.Contains(logs, []byte("encrypted_credentials = ?")) {
		t.Fatal("SQL traces did not retain parameter placeholders")
	}
	if bytes.Contains(logs, printableKey) {
		t.Fatal("slow SQL trace leaked the printable encryption key")
	}
	if bytes.Contains(logs, printableCiphertext) {
		t.Fatal("SQL error trace leaked printable encrypted credentials")
	}
}

func TestHomeGORMLoggerCatalogRedactsSensitiveScanValues(t *testing.T) {
	var output bytes.Buffer
	baseLogger := log.New()
	baseLogger.SetOutput(&output)
	baseLogger.SetFormatter(&homelogging.LogFormatter{})
	queryLog := newDatabaseQueryLog(8, time.Minute)
	homeLogger := homeGORMLogger{
		logger:        baseLogger,
		level:         gormlogger.Warn,
		slowThreshold: -time.Nanosecond,
		queryLog:      queryLog,
	}
	db, errOpen := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: newParameterizedGORMLogger(homeLogger)})
	if errOpen != nil {
		t.Fatalf("open test database: %v", errOpen)
	}
	configureDatabaseGORMClauseBuilders(db)
	sqlDB, errDB := db.DB()
	if errDB != nil {
		t.Fatalf("get sql database: %v", errDB)
	}
	t.Cleanup(func() {
		if errClose := sqlDB.Close(); errClose != nil {
			t.Errorf("close sql database: %v", errClose)
		}
	})
	output.Reset()

	printableKey := bytes.Repeat([]byte{'K'}, 32)
	printableCiphertext := bytes.Repeat([]byte{'C'}, 64)
	var value struct {
		Data []byte `gorm:"column:value"`
	}
	if errScan := db.Raw("SELECT ? AS value", printableKey).Scan(&value).Error; errScan != nil {
		t.Fatalf("scan slow query: %v", errScan)
	}
	if errScan := db.Raw("SELECT encrypted_credentials FROM missing_plugin_store_auth WHERE encrypted_credentials = ?", printableCiphertext).Scan(&value).Error; errScan == nil {
		t.Fatal("sensitive SQL error query unexpectedly succeeded")
	}

	logs := output.Bytes()
	if bytes.Contains(logs, printableKey) || bytes.Contains(logs, printableCiphertext) {
		t.Fatal("Home SQL logs leaked a sensitive bound parameter")
	}
	if !bytes.Contains(logs, []byte("SELECT ?")) || !bytes.Contains(logs, []byte("encrypted_credentials = ?")) {
		t.Fatalf("Home SQL definitions lost placeholders: %q", logs)
	}
	queryLog.mu.Lock()
	defer queryLog.mu.Unlock()
	if len(queryLog.entries) != 2 {
		t.Fatalf("catalog entries = %d, want 2", len(queryLog.entries))
	}
	for sqlID, entry := range queryLog.entries {
		if strings.Contains(entry.SQL, string(printableKey)) || strings.Contains(entry.SQL, string(printableCiphertext)) {
			t.Fatalf("catalog entry %s leaked a sensitive bound parameter", sqlID)
		}
		if !strings.Contains(entry.SQL, "?") {
			t.Fatalf("catalog entry %s lost parameter placeholders: %q", sqlID, entry.SQL)
		}
	}
}
