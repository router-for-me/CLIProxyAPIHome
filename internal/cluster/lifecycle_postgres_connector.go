package cluster

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

// lifecyclePostgresConnector only changes BEGIN for explicitly marked operations.
// Embedding pgx's stdlib connection preserves its pooling, preparation, query,
// cancellation, and bad-connection behavior for every other database/sql method.
type lifecyclePostgresConnector struct{ driver.Connector }

func (c lifecyclePostgresConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, errConnect := c.Connector.Connect(ctx)
	if errConnect != nil {
		return nil, errConnect
	}
	pgxConn, ok := conn.(*stdlib.Conn)
	if !ok {
		errClose := conn.Close()
		return nil, fmt.Errorf("lifecycle connector requires pgx stdlib connection, got %T (close error: %v)", conn, errClose)
	}
	return &lifecyclePostgresConn{Conn: pgxConn}, nil
}

type lifecyclePostgresConn struct{ *stdlib.Conn }

func (c *lifecyclePostgresConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	state, ok := ctx.Value(lifecycleTransactionKey{}).(*lifecycleTransactionState)
	if !ok {
		return c.Conn.BeginTx(ctx, opts)
	}
	if c.Conn.Conn().IsClosed() {
		return nil, driver.ErrBadConn
	}
	// Keep pgx stdlib's isolation/read-only mapping. BeginQuery overrides all
	// pgx TxOptions, so transaction modes must be part of the SQL itself.
	begin := "begin"
	switch sql.IsolationLevel(opts.Isolation) {
	case sql.LevelDefault:
	case sql.LevelReadUncommitted:
		begin += " isolation level read uncommitted"
	case sql.LevelReadCommitted:
		begin += " isolation level read committed"
	case sql.LevelRepeatableRead, sql.LevelSnapshot:
		begin += " isolation level repeatable read"
	case sql.LevelSerializable:
		begin += " isolation level serializable"
	default:
		return nil, fmt.Errorf("unsupported isolation: %v", opts.Isolation)
	}
	if opts.ReadOnly {
		begin += " read only"
	}
	// No parameters: pgx sends one simple-protocol Query containing both
	// statements. Limits apply before any business SQL, not midway through it.
	// pgx closes the connection if any part of this BEGIN fails.
	tx, errBegin := c.Conn.Conn().BeginTx(ctx, pgx.TxOptions{BeginQuery: begin + "; " + lifecycleTimeoutSQL})
	if errBegin != nil {
		return nil, errBegin
	}
	state.configured = true
	return lifecyclePostgresTx{ctx: ctx, tx: tx}, nil
}

// lifecyclePostgresTx adapts pgx completion to database/sql, which still owns
// cancellation rollback, connection release, and the transaction's lifetime.
type lifecyclePostgresTx struct {
	ctx context.Context
	tx  pgx.Tx
}

func (tx lifecyclePostgresTx) Commit() error   { return tx.tx.Commit(tx.ctx) }
func (tx lifecyclePostgresTx) Rollback() error { return tx.tx.Rollback(tx.ctx) }
