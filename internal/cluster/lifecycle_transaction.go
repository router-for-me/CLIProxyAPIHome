package cluster

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"
)

const (
	lifecycleTransactionBudget = 5 * time.Second
	lifecycleLockTimeout       = time.Second

	// PostgreSQL SQLSTATE codes raised when a bounded lifecycle wait expires.
	postgresLockNotAvailable = "55P03"
	postgresQueryCanceled    = "57014"
)

// current_setting includes units (for example, 100ms or 1min). Convert to
// milliseconds directly instead of enumerating PostgreSQL's configuration catalog.
// Zero disables a timeout, so treat it as unbounded before applying our limit.
var lifecycleTimeoutSQL = fmt.Sprintf(`SELECT set_config(name,
	LEAST(NULLIF(EXTRACT(EPOCH FROM current_setting(name)::interval) * 1000, 0), budget)::bigint::text, true)
	FROM (VALUES ('lock_timeout', %d), ('statement_timeout', %d)) AS limits(name, budget)`,
	lifecycleLockTimeout.Milliseconds(), lifecycleTransactionBudget.Milliseconds())

type lifecycleTransactionKey struct{}
type lifecycleTransactionState struct{ configured bool }

// lifecycleTransactionContext gives each operation its own budget and BEGIN marker.
// A fresh marker also lets nested transactions configure their own savepoint scope.
func lifecycleTransactionContext(ctx context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(contextOrBackground(ctx), lifecycleTransactionBudget)
	return context.WithValue(ctx, lifecycleTransactionKey{}, &lifecycleTransactionState{}), cancel
}

// configureLifecycleTransaction bounds server-side waits without changing pooled session defaults.
// Home's PostgreSQL connector sends these settings with BEGIN. Nested transactions and
// externally supplied pools retain the separate statement when no batched BEGIN ran.
// Shorter caller/session limits are preserved; idle transaction recovery is separate.
func configureLifecycleTransaction(tx *gorm.DB) error {
	if tx.Dialector.Name() != "postgres" {
		return nil
	}
	if state, ok := tx.Statement.Context.Value(lifecycleTransactionKey{}).(*lifecycleTransactionState); ok && state.configured {
		return nil
	}
	return tx.Exec(lifecycleTimeoutSQL).Error
}

// IsLifecycleTimeout reports whether a lifecycle operation exhausted its bounded database
// wait instead of reaching a verdict about the membership or Home. It is meant for the
// error of a single operation: a joined error matches as soon as any branch timed out.
func IsLifecycleTimeout(err error) bool {
	var pgError *pgconn.PgError
	if errors.As(err, &pgError) && (pgError.Code == postgresLockNotAvailable || pgError.Code == postgresQueryCanceled) {
		return true
	}
	return errors.Is(err, context.DeadlineExceeded)
}
