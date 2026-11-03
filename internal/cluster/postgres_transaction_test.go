package cluster

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"gorm.io/gorm"
)

func TestPostgresTransactionTracerOnlyMeasuresStartupBegin(t *testing.T) {
	clock := time.Unix(0, 0)
	startup := postgresTransactionStartup{nowFunc: func() time.Time {
		clock = clock.Add(time.Millisecond)
		return clock
	}}
	ctx := context.WithValue(t.Context(), postgresTransactionStartupKey{}, &startup)
	tracer := postgresTransactionTracer{}
	for _, statement := range []string{"SELECT 1", "commit", "rollback", "begin", "begin isolation level serializable"} {
		queryCtx := tracer.TraceQueryStart(ctx, nil, pgx.TraceQueryStartData{SQL: statement})
		tracer.TraceQueryEnd(queryCtx, nil, pgx.TraceQueryEndData{Err: errors.New("begin failure")})
	}
	if !startup.observed || startup.beginDuration != 2*time.Millisecond {
		t.Fatalf("unexpected BEGIN measurement: %+v", startup)
	}
	if ctx.Value(postgresTransactionBeginKey{}) != nil {
		t.Fatal("query-local start time leaked into the transaction context")
	}
	// Ordinary transactions have no startup marker and must be left untouched.
	if got := tracer.TraceQueryStart(t.Context(), nil, pgx.TraceQueryStartData{SQL: "begin"}); got != t.Context() {
		t.Fatal("unobserved BEGIN context changed")
	}
	tracer.TraceQueryEnd(t.Context(), nil, pgx.TraceQueryEndData{})
}

func TestPostgresGateTransactionStartup(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("CLIPROXY_HOME_TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("CLIPROXY_HOME_TEST_POSTGRES_DSN is not configured")
	}
	db, errOpen := Open(t.Context(), PGSQLConfig{PostgresURI: dsn})
	if errOpen != nil {
		t.Fatal(errOpen)
	}
	pool, errDB := db.DB()
	if errDB != nil {
		t.Fatal(errDB)
	}
	t.Cleanup(func() {
		if errClose := pool.Close(); errClose != nil {
			t.Error(errClose)
		}
	})
	clock := time.Unix(0, 0)
	trace := gateTransactionTrace{Interface: db.Logger, startedAt: time.Now(), nowFunc: func() time.Time {
		clock = clock.Add(time.Millisecond)
		return clock
	}}
	var result int
	errTransaction := trace.transaction(t.Context(), db, func(tx *gorm.DB) error {
		if trace.connectionSource != "pool" || trace.beginDuration != time.Millisecond || trace.connectionAcquire != 2*time.Millisecond {
			t.Fatalf("native PostgreSQL BEGIN was not measured: %+v", trace)
		}
		// Advancing the test clock in the body must not affect startup timing.
		clock = clock.Add(time.Hour)
		return tx.Raw("SELECT 1").Scan(&result).Error
	})
	if errTransaction != nil {
		t.Fatal(errTransaction)
	}
	if result != 1 {
		t.Fatalf("transaction query result = %d, want 1", result)
	}
	if trace.beginDuration != time.Millisecond || trace.connectionAcquire != 2*time.Millisecond || trace.sqlCount != 1 {
		t.Fatalf("startup included the body/commit or added SQL: %+v", trace)
	}
	if pool.Stats().InUse != 0 {
		t.Fatalf("connection not released: %+v", pool.Stats())
	}
}
