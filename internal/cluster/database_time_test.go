package cluster

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func TestDatabaseNowQueryUsesClockTimestampForPostgres(t *testing.T) {
	db := &gorm.DB{Config: &gorm.Config{Dialector: postgres.New(postgres.Config{})}}
	if got := databaseNowQuery(db); got != "SELECT clock_timestamp()" {
		t.Fatalf("database now query = %q, want clock_timestamp", got)
	}
}

func TestDatabaseNowQueryKeepsCurrentTimestampForSQLite(t *testing.T) {
	db := &gorm.DB{Config: &gorm.Config{Dialector: sqlite.Open(":memory:")}}
	if got := databaseNowQuery(db); got != "SELECT CURRENT_TIMESTAMP" {
		t.Fatalf("database now query = %q, want CURRENT_TIMESTAMP", got)
	}
}

func TestRepositoryCurrentDatabaseTime(t *testing.T) {
	repo := newCredentialFoundationTestRepository(t)

	before, errBefore := DatabaseNow(context.Background(), repo.db)
	if errBefore != nil {
		t.Fatal(errBefore)
	}
	got, errNow := repo.CurrentDatabaseTime(context.Background())
	if errNow != nil {
		t.Fatal(errNow)
	}
	after, errAfter := DatabaseNow(context.Background(), repo.db)
	if errAfter != nil {
		t.Fatal(errAfter)
	}
	if got.Before(before) || got.After(after) {
		t.Fatalf("CurrentDatabaseTime() = %s, want between %s and %s", got, before, after)
	}
}

func TestDatabaseNowAdvancesWithinPostgresTransaction(t *testing.T) {
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
		t.Fatalf("get postgres database: %v", errDB)
	}
	defer func() {
		if errClose := sqlDB.Close(); errClose != nil {
			t.Errorf("close postgres database: %v", errClose)
		}
	}()

	ctx, cancelCtx := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelCtx()
	var first time.Time
	var second time.Time
	errTransaction := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var errNow error
		first, errNow = DatabaseNow(ctx, tx)
		if errNow != nil {
			return errNow
		}
		if errSleep := tx.Exec("SELECT pg_sleep(?)", 0.01).Error; errSleep != nil {
			return errSleep
		}
		second, errNow = DatabaseNow(ctx, tx)
		return errNow
	})
	if errTransaction != nil {
		t.Fatal(errTransaction)
	}
	if !second.After(first) {
		t.Fatalf("DatabaseNow() values = %s then %s, want advancing time in one transaction", first, second)
	}
}
