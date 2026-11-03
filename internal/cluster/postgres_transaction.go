package cluster

import (
	"context"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type postgresTransactionStartupKey struct{}
type postgresTransactionBeginKey struct{}

type postgresTransactionStartup struct {
	nowFunc       func() time.Time
	beginDuration time.Duration
	observed      bool
}

// postgresTransactionTracer observes connections and driver BEGIN statements
// carrying our startup context, without recording SQL or changing driver behavior.
type postgresTransactionTracer struct{}

func (postgresTransactionTracer) TraceConnectStart(ctx context.Context, _ pgx.TraceConnectStartData) context.Context {
	if startup, ok := ctx.Value(postgresTransactionStartupKey{}).(*postgresTransactionStartup); ok {
		startup.observed = true
	}
	return ctx
}

func (postgresTransactionTracer) TraceConnectEnd(context.Context, pgx.TraceConnectEndData) {}

func (postgresTransactionTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	startup, ok := ctx.Value(postgresTransactionStartupKey{}).(*postgresTransactionStartup)
	if !ok || (data.SQL != "begin" && !strings.HasPrefix(data.SQL, "begin ")) {
		return ctx
	}
	return context.WithValue(ctx, postgresTransactionBeginKey{}, startup.nowFunc())
}

func (postgresTransactionTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	started, ok := ctx.Value(postgresTransactionBeginKey{}).(time.Time)
	if !ok {
		return
	}
	startup := ctx.Value(postgresTransactionStartupKey{}).(*postgresTransactionStartup)
	startup.beginDuration += startup.nowFunc().Sub(started)
	startup.observed = true
}
