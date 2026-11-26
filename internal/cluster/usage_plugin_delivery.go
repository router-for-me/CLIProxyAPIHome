package cluster

import (
	"context"
	"errors"
	"fmt"
	"sync"

	homeerrors "github.com/router-for-me/CLIProxyAPIHome/internal/errors"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	usageDeliveryStoreKey       = "home:postgres-usage-delivery-store"
	maxUsageDeliveryConnections = 8
	maxSQLiteUsageDeliveries    = 8
)

type postgresUsageDeliveryStore struct {
	db        *gorm.DB
	admission chan struct{}
	pending   sync.Map
}

// DeliverPluginUsage records completion separately from usage insertion. A retry
// can therefore deliver an event whose insert committed with an uncertain result.
// Only a successful callback result authorizes the completion marker; pending or
// failed delivery leaves ownership with the sender. Completion is at-least-once
// across a crash between the callback and its marker; consumers can use the
// unchanged execution identity to deduplicate that replay.
func (r *Repository) DeliverPluginUsage(ctx context.Context, payload string, deliver func() error) error {
	db, errDB := r.database()
	if errDB != nil {
		return errDB
	}
	key, errKey := usagePayloadDedupeKey(payload)
	if errKey != nil {
		return errKey
	}
	// Cancellation must not release ownership while the detached plugin callback
	// is still running, or allow another Home to acknowledge its pending event.
	ctx = contextOrBackground(ctx)
	deliveryCtx := context.WithoutCancel(ctx)
	deliverAndMark := func(store *gorm.DB) error {
		query := store.WithContext(deliveryCtx).Select("id", "plugin_usage_delivered").Where("dedupe_key <> '' AND dedupe_key = ?", key)
		if db.Dialector.Name() == "postgres" {
			// A row lock fences concurrent Homes without locking plugin KV writes.
			query = query.Clauses(clause.Locking{Strength: "NO KEY UPDATE", Options: "NOWAIT"})
		}
		var record UsageRecord
		if errQuery := query.Take(&record).Error; errQuery != nil {
			var sqlState interface{ SQLState() string }
			if errors.As(errQuery, &sqlState) && sqlState.SQLState() == "55P03" {
				return homeerrors.ErrUsagePluginDeliveryPending
			}
			return errQuery
		}
		if record.PluginUsageDelivered {
			return nil
		}
		if deliver != nil {
			if errDeliver := deliver(); errDeliver != nil {
				return errDeliver
			}
		}
		update := store.WithContext(deliveryCtx).Model(&UsageRecord{}).Where("id = ?", record.ID).Update("plugin_usage_delivered", true)
		if update.Error != nil {
			return update.Error
		}
		if update.RowsAffected != 1 {
			return fmt.Errorf("usage event disappeared before plugin delivery completion")
		}
		return nil
	}

	switch db.Dialector.Name() {
	case "postgres":
		configured, _ := db.Get(usageDeliveryStoreKey)
		store, supported := configured.(*postgresUsageDeliveryStore)
		if !supported || store == nil || store.db == nil || store.admission == nil {
			return fmt.Errorf("PostgreSQL usage plugin delivery pool is unavailable")
		}
		if errContext := ctx.Err(); errContext != nil {
			return errContext
		}
		// Bound retained work as well as connections. RESP client timeouts do not
		// cancel synchronous handlers, so never queue behind blocked callbacks.
		select {
		case store.admission <- struct{}{}:
			defer func() { <-store.admission }()
		default:
			return homeerrors.ErrUsagePluginDeliveryPending
		}
		if _, pending := store.pending.LoadOrStore(key, struct{}{}); pending {
			return homeerrors.ErrUsagePluginDeliveryPending
		}
		defer store.pending.Delete(key)
		// Connection acquisition remains cancellable. Only an admitted transaction
		// and its callback detach cancellation to retain delivery ownership.
		return store.db.WithContext(ctx).Connection(func(connection *gorm.DB) error {
			return connection.WithContext(deliveryCtx).Transaction(deliverAndMark)
		})
	case "sqlite":
		if errContext := ctx.Err(); errContext != nil {
			return errContext
		}
		// Bound callbacks independently of SQLite's single SQL connection. Keep
		// ownership until completion even if the sender disconnects or cancels.
		r.usagePluginAdmissionOnce.Do(func() {
			r.usagePluginAdmission = make(chan struct{}, maxSQLiteUsageDeliveries)
		})
		select {
		case r.usagePluginAdmission <- struct{}{}:
			defer func() { <-r.usagePluginAdmission }()
		default:
			return homeerrors.ErrUsagePluginDeliveryPending
		}
		// SQLite supports one active Home per database. Do not hold a SQLite
		// transaction during callbacks: plugins may write to the same database.
		if _, pending := r.usagePluginPending.LoadOrStore(key, struct{}{}); pending {
			return homeerrors.ErrUsagePluginDeliveryPending
		}
		defer r.usagePluginPending.Delete(key)
		return deliverAndMark(db)
	default:
		return fmt.Errorf("unsupported usage plugin delivery database %q", db.Dialector.Name())
	}
}
