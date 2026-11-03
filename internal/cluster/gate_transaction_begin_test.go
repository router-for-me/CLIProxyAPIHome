package cluster

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// These driver hooks validate the connection/BEGIN boundary without sleeps or a network.
type gateTraceConnector struct {
	connects, begins, commits, rollbacks, closes int
	connectErr, beginErr, commitErr              error
	onBegin                                      func()
	badThrough                                   int
}

func (c *gateTraceConnector) Connect(ctx context.Context) (driver.Conn, error) {
	postgresTransactionTracer{}.TraceConnectStart(ctx, pgx.TraceConnectStartData{})
	c.connects++
	if c.connectErr != nil {
		return nil, c.connectErr
	}
	return &gateTraceConn{owner: c, id: c.connects}, nil
}
func (c *gateTraceConnector) Driver() driver.Driver { return gateTraceDriver{owner: c} }

type gateTraceDriver struct{ owner *gateTraceConnector }

func (d gateTraceDriver) Open(string) (driver.Conn, error) {
	return d.owner.Connect(context.Background())
}

type gateTraceConn struct {
	owner *gateTraceConnector
	id    int
}

func (c *gateTraceConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unexpected prepare")
}
func (c *gateTraceConn) Close() error { c.owner.closes++; return nil }
func (c *gateTraceConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}
func (c *gateTraceConn) BeginTx(ctx context.Context, _ driver.TxOptions) (driver.Tx, error) {
	tracer := postgresTransactionTracer{}
	ctx = tracer.TraceQueryStart(ctx, nil, pgx.TraceQueryStartData{SQL: "begin"})
	defer tracer.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{})
	c.owner.begins++
	if c.owner.onBegin != nil {
		c.owner.onBegin()
	}
	if c.owner.beginErr != nil {
		return nil, c.owner.beginErr
	}
	if c.id <= c.owner.badThrough {
		return nil, driver.ErrBadConn
	}
	return &gateTraceTx{owner: c.owner}, nil
}

type gateTraceTx struct{ owner *gateTraceConnector }

func (t *gateTraceTx) Commit() error   { t.owner.commits++; return t.owner.commitErr }
func (t *gateTraceTx) Rollback() error { t.owner.rollbacks++; return nil }

func TestGateTransactionStartupTimingAndOutcomes(t *testing.T) {
	for _, scenario := range []string{"success", "connect error", "begin error", "body error", "commit error", "bad begin retry", "bad body no retry", "bad commit no retry", "panic"} {
		t.Run(scenario, func(t *testing.T) {
			failure := errors.New("test transaction failure")
			connector := &gateTraceConnector{}
			pool := sql.OpenDB(connector)
			t.Cleanup(func() {
				if errClose := pool.Close(); errClose != nil {
					t.Error(errClose)
				}
			})
			db, errOpen := gorm.Open(postgres.New(postgres.Config{Conn: pool}), &gorm.Config{DisableAutomaticPing: true})
			if errOpen != nil {
				t.Fatal(errOpen)
			}
			clock := time.Unix(0, 0)
			trace := gateTransactionTrace{Interface: db.Logger, startedAt: time.Now(), nowFunc: func() time.Time {
				clock = clock.Add(time.Millisecond)
				return clock
			}}
			switch scenario {
			case "connect error":
				connector.connectErr = failure
			case "begin error":
				connector.beginErr = failure
			case "commit error":
				connector.commitErr = failure
			case "bad commit no retry":
				connector.commitErr = driver.ErrBadConn
			case "bad begin retry":
				connector.beginErr = driver.ErrBadConn
			}
			bodyCalls := 0
			var measuredBegin time.Duration
			var result error
			var recovered any
			func() {
				defer func() { recovered = recover() }()
				result = trace.transaction(t.Context(), db, func(tx *gorm.DB) error {
					bodyCalls++
					if tx.Statement.Context.Value(postgresTransactionStartupKey{}) != nil {
						t.Error("startup observer leaked into transaction body")
					}
					measuredBegin = trace.beginDuration
					if measuredBegin <= 0 {
						t.Error("transaction body started before BEGIN was recorded")
					}
					if _, ok := tx.Statement.ConnPool.(*sql.Tx); !ok {
						t.Error("GORM no longer owns the original transaction")
					}
					if scenario == "body error" {
						return failure
					}
					if scenario == "bad body no retry" {
						return driver.ErrBadConn
					}
					if scenario == "panic" {
						panic("test panic")
					}
					return nil
				})
			}()
			if trace.connectionSource != "pool" || trace.connectionAcquire <= 0 || pool.Stats().InUse != 0 {
				t.Fatalf("acquisition observation or connection release failed: source=%s acquire=%s stats=%+v", trace.connectionSource, trace.connectionAcquire, pool.Stats())
			}
			if bodyCalls > 0 && trace.beginDuration != measuredBegin {
				t.Fatal("BEGIN timing included body or commit")
			}
			switch scenario {
			case "success":
				if result != nil || bodyCalls != 1 || connector.commits != 1 {
					t.Fatalf("success changed: %v %+v", result, connector)
				}
			case "connect error":
				if !errors.Is(result, failure) || trace.beginDuration != 0 || connector.begins != 0 || bodyCalls != 0 {
					t.Fatalf("connect error lost: %v", result)
				}
			case "begin error":
				if !errors.Is(result, failure) || trace.beginDuration <= 0 || bodyCalls != 0 {
					t.Fatalf("begin error lost: %v", result)
				}
			case "bad begin retry":
				if !errors.Is(result, driver.ErrBadConn) || connector.begins != 3 || bodyCalls != 0 {
					t.Fatalf("bad-connection BEGIN retry changed: %v %+v", result, connector)
				}
			case "panic":
				if recovered != "test panic" || connector.rollbacks != 1 {
					t.Fatalf("panic did not roll back: %v %+v", recovered, connector)
				}
			case "commit error", "bad commit no retry":
				want := failure
				if scenario == "bad commit no retry" {
					want = driver.ErrBadConn
				}
				if !errors.Is(result, want) || bodyCalls != 1 || connector.begins != 1 || connector.commits != 1 {
					t.Fatalf("commit error changed or retried: %v %+v", result, connector)
				}
			default:
				want := failure
				if scenario == "bad body no retry" {
					want = driver.ErrBadConn
				}
				if !errors.Is(result, want) || bodyCalls != 1 || connector.rollbacks != 1 {
					t.Fatalf("body error was retried or not rolled back: %v %+v", result, connector)
				}
			}
		})
	}
}

func TestGateTransactionFinalRetryUsesFreshConnection(t *testing.T) {
	for _, observed := range []bool{false, true} {
		name := "native"
		if observed {
			name = "observed"
		}
		t.Run(name, func(t *testing.T) {
			connector := &gateTraceConnector{badThrough: 3}
			pool := sql.OpenDB(connector)
			pool.SetMaxIdleConns(2)
			t.Cleanup(func() {
				if errClose := pool.Close(); errClose != nil {
					t.Error(errClose)
				}
			})
			// Keep two stale connections idle and return a third during the first
			// BEGIN. A cached-only third attempt would select another stale one.
			var connections []*sql.Conn
			for range 3 {
				conn, errConn := pool.Conn(t.Context())
				if errConn != nil {
					t.Fatal(errConn)
				}
				connections = append(connections, conn)
				t.Cleanup(func() {
					if errClose := conn.Close(); errClose != nil && !errors.Is(errClose, sql.ErrConnDone) {
						t.Error(errClose)
					}
				})
			}
			for _, conn := range connections[:2] {
				if errClose := conn.Close(); errClose != nil {
					t.Fatal(errClose)
				}
			}
			connector.onBegin = func() {
				if connector.begins == 1 {
					if errClose := connections[2].Close(); errClose != nil {
						t.Error(errClose)
					}
				}
			}
			db, errOpen := gorm.Open(postgres.New(postgres.Config{Conn: pool}), &gorm.Config{DisableAutomaticPing: true})
			if errOpen != nil {
				t.Fatal(errOpen)
			}
			bodyCalls := 0
			body := func(*gorm.DB) error { bodyCalls++; return nil }
			var errTransaction error
			if observed {
				trace := gateTransactionTrace{Interface: db.Logger, startedAt: time.Now()}
				errTransaction = trace.transaction(t.Context(), db, body)
			} else {
				errTransaction = db.WithContext(t.Context()).Transaction(body)
			}
			if errTransaction != nil {
				t.Fatalf("fresh connection should succeed after two stale BEGIN failures: %v", errTransaction)
			}
			if connector.begins != 3 || connector.connects != 4 || bodyCalls != 1 || connector.commits != 1 {
				t.Fatalf("retry budget or transaction body changed: body_calls=%d driver=%+v", bodyCalls, connector)
			}
			if stats := pool.Stats(); stats.InUse != 0 || stats.Idle != 2 {
				t.Fatalf("connections were leaked or idle pool was drained: %+v", stats)
			}
		})
	}
}

func TestGateTransactionKeepsPinnedAndNestedConnections(t *testing.T) {
	repo := newCredentialFoundationTestRepository(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	failure := errors.New("nested rollback")
	if errConnection := repo.db.WithContext(ctx).Connection(func(conn *gorm.DB) error {
		clock := time.Unix(0, 0)
		trace := gateTransactionTrace{Interface: conn.Logger, startedAt: time.Now(), nowFunc: func() time.Time {
			clock = clock.Add(time.Millisecond)
			return clock
		}}
		if errTransaction := trace.transaction(ctx, conn, func(tx *gorm.DB) error {
			if errCreate := tx.Create(&ClusterRebalanceGateRecord{ID: 1}).Error; errCreate != nil {
				return errCreate
			}
			nested := gateTransactionTrace{Interface: repo.db.Logger, startedAt: time.Now()}
			errNested := nested.transaction(ctx, tx, func(child *gorm.DB) error {
				if errCreate := child.Create(&ClusterRebalanceGateRecord{ID: 2}).Error; errCreate != nil {
					return errCreate
				}
				return failure
			})
			if !errors.Is(errNested, failure) || nested.connectionSource != "transaction" || nested.connectionAcquire != 0 || nested.beginDuration != 0 {
				t.Fatalf("nested savepoint/acquisition behavior changed: %v %+v", errNested, nested)
			}
			return nil
		}); errTransaction != nil {
			return errTransaction
		}
		if trace.connectionSource != "pinned" || trace.connectionAcquire != 0 || trace.beginDuration <= 0 {
			t.Fatalf("pinned timing is misleading: %+v", trace)
		}
		var count int64
		if errCount := conn.Model(&ClusterRebalanceGateRecord{}).Count(&count).Error; errCount != nil {
			return errCount
		}
		if count != 1 {
			t.Fatalf("nested rollback affected parent: count=%d", count)
		}
		return nil
	}); errConnection != nil {
		t.Fatal(errConnection)
	}
}

func TestGateTransactionUnobservedPoolKeepsGORMPath(t *testing.T) {
	repo := newCredentialFoundationTestRepository(t)
	trace := gateTransactionTrace{Interface: repo.db.Logger, startedAt: time.Now()}
	if errTransaction := trace.transaction(t.Context(), repo.db, func(tx *gorm.DB) error {
		return tx.Create(&ClusterRebalanceGateRecord{ID: 1}).Error
	}); errTransaction != nil {
		t.Fatal(errTransaction)
	}
	if trace.connectionSource != "unavailable" || trace.connectionAcquire != 0 || trace.beginDuration != 0 {
		t.Fatalf("unmeasured startup was reported as split timing: %+v", trace)
	}
}

func TestGateTransactionConfiguredTimeoutKeepsGORMPath(t *testing.T) {
	repo := newCredentialFoundationTestRepository(t)
	db := repo.db.Session(&gorm.Session{})
	db.DefaultTransactionTimeout = time.Second
	trace := gateTransactionTrace{Interface: db.Logger, startedAt: time.Now()}
	if errTransaction := trace.transaction(t.Context(), db, func(tx *gorm.DB) error {
		return tx.Create(&ClusterRebalanceGateRecord{ID: 1}).Error
	}); errTransaction != nil {
		t.Fatal(errTransaction)
	}
	if trace.connectionSource != "unavailable" || trace.connectionAcquire != 0 || trace.beginDuration != 0 {
		t.Fatalf("unmeasured startup was reported as split timing: %+v", trace)
	}
}

func TestGateTransactionCanceledAcquisitionDoesNotBegin(t *testing.T) {
	repo := newCredentialFoundationTestRepository(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	trace := gateTransactionTrace{Interface: repo.db.Logger, startedAt: time.Now()}
	errTransaction := trace.transaction(ctx, repo.db, func(*gorm.DB) error { t.Fatal("canceled transaction entered its body"); return nil })
	if !errors.Is(errTransaction, context.Canceled) || trace.beginDuration != 0 {
		t.Fatalf("cancellation lost: %v %+v", errTransaction, trace)
	}
}
