package cluster

import (
	"bytes"
	"context"
	"errors"
	stdlog "log"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	homelogging "github.com/router-for-me/CLIProxyAPIHome/internal/logging"
	log "github.com/sirupsen/logrus"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

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

			logger := newParameterizedGORMLogger(newHomeGORMLogger(baseLogger))
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
			if bytes.Count(logs, []byte{'\n'}) != 1 {
				t.Fatalf("GORM log = %q, want one formatted line", logs)
			}
		})
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
