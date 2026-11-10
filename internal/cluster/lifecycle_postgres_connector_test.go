package cluster

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
	"gorm.io/gorm"
)

func newLifecyclePostgresDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("HOME_TEST_POSTGRES_DSN"))
	if dsn == "" {
		dsn = strings.TrimSpace(os.Getenv("CLIPROXY_HOME_TEST_POSTGRES_DSN"))
	}
	if dsn == "" {
		t.Skip("HOME_TEST_POSTGRES_DSN is not configured")
	}
	db, errOpen := Open(t.Context(), PGSQLConfig{PostgresURI: dsn, MaxOpenConns: 1})
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
	return db
}

func TestPostgresLifecycleBeginRemovesSettingsRoundTrip(t *testing.T) {
	db := newLifecyclePostgresDB(t)
	if errConnection := db.Connection(func(pinned *gorm.DB) error {
		conn := pinned.Statement.ConnPool.(*sql.Conn)
		var wire bytes.Buffer
		if errRaw := conn.Raw(func(raw any) error {
			raw.(*lifecyclePostgresConn).Conn.Conn().PgConn().Frontend().Trace(&wire, pgproto3.TracerOptions{SuppressTimestamps: true})
			return nil
		}); errRaw != nil {
			return errRaw
		}
		defer func() {
			if errRaw := conn.Raw(func(raw any) error {
				raw.(*lifecyclePostgresConn).Conn.Conn().PgConn().Frontend().Untrace()
				return nil
			}); errRaw != nil {
				t.Error(errRaw)
			}
		}()
		for _, mode := range []string{"ordinary", "separate settings", "batched settings"} {
			ctx := t.Context()
			if mode == "batched settings" {
				var cancel context.CancelFunc
				ctx, cancel = lifecycleTransactionContext(ctx)
				defer cancel()
			}
			wire.Reset()
			if errTransaction := pinned.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
				if mode != "ordinary" {
					if errConfigure := configureLifecycleTransaction(tx); errConfigure != nil {
						return errConfigure
					}
				}
				return tx.Exec("SELECT 1").Error
			}); errTransaction != nil {
				return errTransaction
			}
			want := 3 // BEGIN, business query, COMMIT.
			if mode == "separate settings" {
				want++
			}
			if queries, ready := strings.Count(wire.String(), "F\tQuery\t"), strings.Count(wire.String(), "B\tReadyForQuery\t"); queries != want || ready != want {
				t.Fatalf("%s: queries=%d ready=%d, want %d complete protocol exchanges: %s", mode, queries, ready, want, wire.String())
			}
			if batched := strings.Contains(wire.String(), "begin; SELECT set_config"); batched != (mode == "batched settings") {
				t.Fatalf("%s: unexpected batched BEGIN: %s", mode, wire.String())
			}
			t.Logf("%s: %d protocol exchanges", mode, want)
		}
		return nil
	}); errConnection != nil {
		t.Fatal(errConnection)
	}
}

func TestPostgresBatchedLifecycleTimeoutsRestoreSession(t *testing.T) {
	db := newLifecyclePostgresDB(t)
	for _, shorter := range []bool{false, true} {
		for _, outcome := range []string{"commit", "rollback", "panic", "query error"} {
			t.Run(fmt.Sprintf("shorter=%t/%s", shorter, outcome), func(t *testing.T) {
				if errConnection := db.Connection(func(pinned *gorm.DB) error {
					// The same pinned session proves both commit and rollback restore defaults.
					initial := "0"
					if shorter {
						initial = "100ms"
					}
					if errSet := pinned.Exec("SELECT set_config('lock_timeout', ?, false), set_config('statement_timeout', ?, false)", initial, initial).Error; errSet != nil {
						return errSet
					}
					type settings struct{ Lock, Statement int64 }
					readSQL := `SELECT (SELECT setting::bigint FROM pg_settings WHERE name = 'lock_timeout') AS lock,
						(SELECT setting::bigint FROM pg_settings WHERE name = 'statement_timeout') AS statement`
					var before settings
					if errRead := pinned.Raw(readSQL).Scan(&before).Error; errRead != nil {
						return errRead
					}
					ctx, cancel := lifecycleTransactionContext(t.Context())
					defer cancel()
					rollback := errors.New("test rollback")
					var errTransaction error
					var recovered any
					func() {
						defer func() { recovered = recover() }()
						errTransaction = pinned.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
							if !ctx.Value(lifecycleTransactionKey{}).(*lifecycleTransactionState).configured {
								t.Error("BEGIN did not configure timeouts")
							}
							var configured settings
							if errRead := tx.Raw(readSQL).Scan(&configured).Error; errRead != nil {
								return errRead
							}
							want := settings{lifecycleLockTimeout.Milliseconds(), lifecycleTransactionBudget.Milliseconds()}
							if shorter {
								want = settings{100, 100}
							}
							if configured != want {
								t.Errorf("timeouts = %+v, want %+v", configured, want)
							}
							switch outcome {
							case "rollback":
								return rollback
							case "panic":
								panic(rollback)
							case "query error":
								return tx.Exec("SELECT 1/0").Error
							}
							return nil
						})
					}()
					switch outcome {
					case "commit":
						if errTransaction != nil {
							return errTransaction
						}
					case "rollback":
						if !errors.Is(errTransaction, rollback) {
							t.Fatalf("rollback error = %v", errTransaction)
						}
					case "panic":
						if recovered != rollback {
							t.Fatalf("panic = %v", recovered)
						}
					case "query error":
						var pgError *pgconn.PgError
						if !errors.As(errTransaction, &pgError) || pgError.Code != "22012" {
							t.Fatalf("query error = %v", errTransaction)
						}
					}
					var after settings
					if errRead := pinned.Raw(readSQL).Scan(&after).Error; errRead != nil {
						return errRead
					}
					if after != before {
						t.Errorf("limits leaked into session: before=%+v after=%+v", before, after)
					}
					return nil
				}); errConnection != nil {
					t.Fatal(errConnection)
				}
			})
		}
	}
}

func TestPostgresBatchedLifecyclePreservesTransactionModes(t *testing.T) {
	db := newLifecyclePostgresDB(t)
	for isolation, want := range map[sql.IsolationLevel]string{
		sql.LevelReadUncommitted: "read uncommitted", sql.LevelReadCommitted: "read committed",
		sql.LevelRepeatableRead: "repeatable read", sql.LevelSnapshot: "repeatable read", sql.LevelSerializable: "serializable",
	} {
		ctx, cancel := lifecycleTransactionContext(t.Context())
		errTransaction := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			var actual struct{ Isolation, ReadOnly string }
			if errRead := tx.Raw("SELECT current_setting('transaction_isolation') AS isolation, current_setting('transaction_read_only') AS read_only").Scan(&actual).Error; errRead != nil {
				return errRead
			}
			if actual.Isolation != want || actual.ReadOnly != "on" {
				t.Errorf("transaction modes = %+v, want %s/read only", actual, want)
			}
			return nil
		}, &sql.TxOptions{Isolation: isolation, ReadOnly: true})
		cancel()
		if errTransaction != nil {
			t.Fatal(errTransaction)
		}
	}
}

func TestPostgresBatchedLifecycleNestedRollbackRestoresSettings(t *testing.T) {
	db := newLifecyclePostgresDB(t)
	if errTransaction := db.Transaction(func(parent *gorm.DB) error {
		if errSet := parent.Exec("SET LOCAL lock_timeout = '0'; SET LOCAL statement_timeout = '0'").Error; errSet != nil {
			return errSet
		}
		ctx, cancel := lifecycleTransactionContext(t.Context())
		defer cancel()
		rollback := errors.New("nested rollback")
		errNested := parent.WithContext(ctx).Transaction(func(child *gorm.DB) error {
			if ctx.Value(lifecycleTransactionKey{}).(*lifecycleTransactionState).configured {
				t.Error("savepoint was incorrectly treated as a batched BEGIN")
			}
			if errConfigure := configureLifecycleTransaction(child); errConfigure != nil {
				return errConfigure
			}
			var lock string
			if errRead := child.Raw("SELECT current_setting('lock_timeout')").Scan(&lock).Error; errRead != nil {
				return errRead
			}
			if lock != "1s" {
				t.Errorf("nested transaction lock timeout = %s, want 1s", lock)
			}
			return rollback
		})
		if !errors.Is(errNested, rollback) {
			return fmt.Errorf("nested rollback: %w", errNested)
		}
		var lock string
		if errRead := parent.Raw("SELECT current_setting('lock_timeout')").Scan(&lock).Error; errRead != nil {
			return errRead
		}
		if lock != "0" {
			t.Errorf("savepoint rollback leaked settings to parent: %s", lock)
		}
		return nil
	}); errTransaction != nil {
		t.Fatal(errTransaction)
	}
}

func TestPostgresBatchedLifecycleCancellationReleasesConnection(t *testing.T) {
	db := newLifecyclePostgresDB(t)
	ctx, cancel := lifecycleTransactionContext(t.Context())
	defer cancel()
	errTransaction := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if errConfigure := configureLifecycleTransaction(tx); errConfigure != nil {
			return errConfigure
		}
		cancel()
		return ctx.Err()
	})
	if !errors.Is(errTransaction, context.Canceled) {
		t.Fatalf("canceled transaction error = %v", errTransaction)
	}
	// The pool has one slot. A leaked transaction prevents this query completing.
	queryCtx, cancelQuery := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancelQuery()
	var result int
	if errRead := db.WithContext(queryCtx).Raw("SELECT 1").Scan(&result).Error; errRead != nil || result != 1 {
		t.Fatalf("pool was not usable after cancellation: result=%d error=%v", result, errRead)
	}
}

func TestLifecycleTransactionContextKeepsShorterParentAndFreshMarker(t *testing.T) {
	parent, cancelParent := context.WithDeadline(t.Context(), time.Now().Add(time.Second))
	defer cancelParent()
	ctx, cancel := lifecycleTransactionContext(parent)
	defer cancel()
	ctx.Value(lifecycleTransactionKey{}).(*lifecycleTransactionState).configured = true
	child, cancelChild := lifecycleTransactionContext(ctx)
	defer cancelChild()
	deadline, _ := parent.Deadline()
	if got, _ := child.Deadline(); !got.Equal(deadline) || child.Value(lifecycleTransactionKey{}).(*lifecycleTransactionState).configured {
		t.Fatal("nested operation lost its caller deadline or inherited a configured BEGIN marker")
	}
}
