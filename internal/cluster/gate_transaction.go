package cluster

import (
	"context"
	"database/sql"
	"time"

	log "github.com/sirupsen/logrus"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// gateTransactionTrace observes one transaction without changing SQL logging or
// recording parameters. Acquisition includes the database round trip; held time
// starts on receipt of the lock and includes commit or rollback completion.
type gateTransactionTrace struct {
	gormlogger.Interface
	operation         string
	startedAt         time.Time
	nowFunc           func() time.Time
	connectionAcquire time.Duration
	beginDuration     time.Duration
	connectionSource  string
	lockedAt          time.Time
	gateWait          time.Duration
	rebalanceLockedAt time.Time
	rebalanceWait     time.Duration
	sqlCount          int
	planCount         int
}

// transaction observes startup without taking over database/sql connection retries.
// PostgreSQL's query tracer measures BEGIN; the remaining startup time includes
// acquisition, dialing/authentication and framework overhead. GORM owns completion.
func (t *gateTransactionTrace) transaction(ctx context.Context, db *gorm.DB, fn func(*gorm.DB) error) error {
	if t.nowFunc == nil {
		t.nowFunc = time.Now
	}
	db = db.Session(&gorm.Session{Logger: t, Context: contextOrBackground(ctx)})
	if db.Error != nil {
		return db.Error
	}
	// Keep configured GORM deadlines on the existing uninstrumented path.
	if db.DefaultTransactionTimeout > 0 {
		t.connectionSource = "unavailable"
		return db.Transaction(fn)
	}
	_, pooled := db.Statement.ConnPool.(*sql.DB)
	if !pooled {
		// Never acquire a second connection when a caller already pinned one or
		// opened a transaction. In particular, SQLite may have only one connection.
		if _, nested := db.Statement.ConnPool.(gorm.TxCommitter); nested {
			t.connectionSource = "transaction"
			return db.Transaction(fn)
		}
		if _, pinned := db.Statement.ConnPool.(*sql.Conn); pinned {
			t.connectionSource = "pinned"
			return t.beginTransaction(db, fn)
		}
		// Custom/prepared pools may hide acquisition inside BeginTx; do not report
		// their combined startup latency as a measured BEGIN-only duration.
		t.connectionSource = "unavailable"
		return db.Transaction(fn)
	}
	// DB.BeginTx owns the entire retry budget, including its final always-new
	// acquisition. Repeated DB.Conn calls cannot preserve that guarantee.
	t.connectionSource = "unavailable"
	startup := postgresTransactionStartup{nowFunc: t.nowFunc}
	transactionCtx := db.Statement.Context
	db = db.WithContext(context.WithValue(transactionCtx, postgresTransactionStartupKey{}, &startup))
	started := t.nowFunc()
	recordStartup := func() {
		if startup.observed {
			t.connectionSource = "pool"
			t.beginDuration = startup.beginDuration
			t.connectionAcquire = t.nowFunc().Sub(started) - startup.beginDuration
		}
	}
	entered := false
	errTransaction := db.Transaction(func(tx *gorm.DB) error {
		recordStartup()
		entered = true
		// Do not observe statements from the body as transaction startup. Keep
		// the same GORM transaction object for callback and completion handling.
		tx.Statement.Context = transactionCtx
		return fn(tx)
	})
	if !entered {
		recordStartup()
	}
	return errTransaction
}

// beginTransaction records BEGIN failures too, but excludes all callback/commit work.
func (t *gateTransactionTrace) beginTransaction(db *gorm.DB, fn func(*gorm.DB) error) error {
	started := t.nowFunc()
	entered := false
	errTransaction := db.Transaction(func(tx *gorm.DB) error {
		t.beginDuration += t.nowFunc().Sub(started)
		entered = true
		return fn(tx)
	})
	if !entered {
		t.beginDuration += t.nowFunc().Sub(started)
	}
	return errTransaction
}

// ParamsFilter keeps the application's parameter-redaction boundary when GORM
// sees this observer instead of the underlying parameterized logger.
func (t *gateTransactionTrace) ParamsFilter(_ context.Context, sql string, _ ...interface{}) (string, []interface{}) {
	return sql, nil
}

func (t *gateTransactionTrace) Trace(ctx context.Context, begin time.Time, fc func() (string, int64), err error) {
	t.sqlCount++
	t.Interface.Trace(ctx, begin, fc, err)
}

func (t *gateTransactionTrace) logResult(err error) {
	finishedAt := time.Now()
	total := finishedAt.Sub(t.startedAt)
	var held time.Duration
	if !t.lockedAt.IsZero() {
		held = finishedAt.Sub(t.lockedAt)
	}
	var rebalanceHeld time.Duration
	if !t.rebalanceLockedAt.IsZero() {
		rebalanceHeld = finishedAt.Sub(t.rebalanceLockedAt)
	}
	slowThreshold := defaultDatabaseSlowQueryThreshold
	if logger, ok := t.Interface.(parameterizedGORMLogger); ok {
		if inner, okInner := logger.inner.(homeGORMLogger); okInner {
			slowThreshold = inner.slowThreshold
		}
	}
	// SQL count excludes driver-level transaction control; timings end after commit or rollback.
	const message = "cluster transaction operation=%s success=%t total_ms=%.3f sql_count=%d plan_count=%d connection_source=%s connection_acquire_ms=%.3f begin_ms=%.3f gate_acquire_ms=%.3f gate_held_ms=%.3f rebalance_acquire_ms=%.3f rebalance_held_ms=%.3f"
	if total >= slowThreshold {
		log.Warnf(message, t.operation, err == nil, float64(total)/float64(time.Millisecond), t.sqlCount, t.planCount, t.connectionSource, float64(t.connectionAcquire)/float64(time.Millisecond), float64(t.beginDuration)/float64(time.Millisecond), float64(t.gateWait)/float64(time.Millisecond), float64(held)/float64(time.Millisecond), float64(t.rebalanceWait)/float64(time.Millisecond), float64(rebalanceHeld)/float64(time.Millisecond))
		return
	}
	log.Debugf(message, t.operation, err == nil, float64(total)/float64(time.Millisecond), t.sqlCount, t.planCount, t.connectionSource, float64(t.connectionAcquire)/float64(time.Millisecond), float64(t.beginDuration)/float64(time.Millisecond), float64(t.gateWait)/float64(time.Millisecond), float64(held)/float64(time.Millisecond), float64(t.rebalanceWait)/float64(time.Millisecond), float64(rebalanceHeld)/float64(time.Millisecond))
}
