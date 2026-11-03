package cluster

import (
	"fmt"

	"gorm.io/gorm"
)

const billingUsageCoverIndex = "idx_billing_charge_usage_cover_v1"

func migrateBillingUsageCoverIndex(db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("database connection is nil")
	}
	if !usageObservabilityIsPostgres(db) {
		return nil
	}
	// Cover fixed-width statistics only. Including unbounded historical labels
	// could exceed PostgreSQL's index tuple limit and reject otherwise valid writes.
	// Keep the original usage_id unique index for GORM and older Home processes.
	// Bound startup build memory as well as steady-state query memory. Restore
	// the caller's settings even when this runs inside the shared migration transaction.
	if errCreate := db.Exec(`DO $migration$
		DECLARE
			memory_limit text := current_setting('maintenance_work_mem');
			parallel_workers text := current_setting('max_parallel_maintenance_workers');
		BEGIN
			PERFORM set_config('maintenance_work_mem',
				LEAST(pg_size_bytes(memory_limit) / 1024, 16384)::text || 'kB', true);
			PERFORM set_config('max_parallel_maintenance_workers', '0', true);
			CREATE UNIQUE INDEX IF NOT EXISTS idx_billing_charge_usage_cover_v1
				ON "billing_charge" ("usage_id") INCLUDE ("amount", "user_id", "api_key_id");
			PERFORM set_config('maintenance_work_mem', memory_limit, true);
			PERFORM set_config('max_parallel_maintenance_workers', parallel_workers, true);
		END; $migration$`).Error; errCreate != nil {
		return fmt.Errorf("create billing usage covering index: %w", errCreate)
	}
	// IF NOT EXISTS also accepts an invalid or differently defined prebuilt index.
	// Refuse to mark the migration complete unless the live definition matches.
	var valid bool
	if errValidate := db.Raw(`SELECT i.indisvalid AND i.indisready AND i.indisunique
		AND i.indpred IS NULL AND i.indexprs IS NULL
		AND i.indnkeyatts = 1 AND i.indnatts = 4 AND am.amname = 'btree'
		AND ARRAY(
			SELECT a.attname::text
			FROM unnest(i.indkey) WITH ORDINALITY AS k(attnum, position)
			JOIN pg_attribute AS a ON a.attrelid = i.indrelid AND a.attnum = k.attnum
			ORDER BY k.position
		) = ARRAY['usage_id', 'amount', 'user_id', 'api_key_id']::text[]
		FROM pg_index AS i
		JOIN pg_class AS idx ON idx.oid = i.indexrelid
		JOIN pg_am AS am ON am.oid = idx.relam
		WHERE i.indexrelid = to_regclass(?) AND i.indrelid = 'billing_charge'::regclass`, billingUsageCoverIndex).
		Scan(&valid).Error; errValidate != nil {
		return fmt.Errorf("validate billing usage covering index: %w", errValidate)
	}
	if !valid {
		return fmt.Errorf("billing usage covering index %s is invalid or has an unexpected definition", billingUsageCoverIndex)
	}
	return nil
}
