package cluster

import (
	"fmt"

	"gorm.io/gorm"
)

const (
	billingOverviewTimeCoverIndex  = "idx_billing_charge_overview_time_v1"
	billingOverviewUsageCoverIndex = "idx_usage_billing_complete_v2"
)

func migrateBillingOverviewIndexes(db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("database is nil")
	}
	if !usageObservabilityIsPostgres(db) {
		return nil
	}
	// Only bounded-width columns are stored. Model names and historical labels
	// must not enlarge index tuples or make otherwise valid charges unwritable.
	// Preserve lower memory settings and restore the caller's maintenance settings.
	if errCreate := db.Exec(fmt.Sprintf(`DO $migration$
		DECLARE
			memory_limit text := current_setting('maintenance_work_mem');
			parallel_workers text := current_setting('max_parallel_maintenance_workers');
		BEGIN
			PERFORM set_config('maintenance_work_mem',
				LEAST(pg_size_bytes(memory_limit) / 1024, 16384)::text || 'kB', true);
			PERFORM set_config('max_parallel_maintenance_workers', '0', true);
			CREATE INDEX IF NOT EXISTS idx_billing_charge_overview_time_v1
				ON billing_charge (created_at)
				INCLUDE (usage_id, user_id, amount, input_tokens, output_tokens, cache_tokens);
			CREATE INDEX IF NOT EXISTS idx_usage_billing_complete_v2
				ON usage (id) INCLUDE (accounting_input_tokens, accounting_output_tokens)
				WHERE token_accounting_version = %d AND token_accounting_quality = '%s';
			PERFORM set_config('maintenance_work_mem', memory_limit, true);
			PERFORM set_config('max_parallel_maintenance_workers', parallel_workers, true);
		END; $migration$`, UsageTokenAccountingSchemaVersion, UsageTokenAccountingQualityComplete)).Error; errCreate != nil {
		return fmt.Errorf("create billing overview indexes: %w", errCreate)
	}
	// A failed concurrent prebuild can leave an invalid index with the expected name.
	// Validate the complete definition before advancing the schema version.
	predicate := fmt.Sprintf("((token_accounting_version = %d) AND (token_accounting_quality = '%s'::text))",
		UsageTokenAccountingSchemaVersion, UsageTokenAccountingQualityComplete)
	var valid bool
	if errValidate := db.Raw(`WITH expected(index_name, table_name, column_names, predicate) AS (
		VALUES
			(?, 'billing_charge', ARRAY['created_at', 'usage_id', 'user_id', 'amount', 'input_tokens', 'output_tokens', 'cache_tokens'], NULL::text),
			(?, 'usage', ARRAY['id', 'accounting_input_tokens', 'accounting_output_tokens'], ?::text)
	)
	SELECT bool_and(COALESCE(
		i.indisvalid AND i.indisready AND NOT i.indisunique
		AND i.indrelid = to_regclass(expected.table_name)
		AND i.indnkeyatts = 1 AND i.indnatts = cardinality(expected.column_names)
		AND i.indexprs IS NULL AND am.amname = 'btree'
		AND pg_get_expr(i.indpred, i.indrelid) IS NOT DISTINCT FROM expected.predicate
		AND ARRAY(
			SELECT a.attname::text
			FROM unnest(i.indkey) WITH ORDINALITY AS k(attnum, position)
			JOIN pg_attribute AS a ON a.attrelid = i.indrelid AND a.attnum = k.attnum
			ORDER BY k.position
		) = expected.column_names, FALSE))
	FROM expected
	LEFT JOIN pg_index AS i ON i.indexrelid = to_regclass(expected.index_name)
	LEFT JOIN pg_class AS idx ON idx.oid = i.indexrelid
	LEFT JOIN pg_am AS am ON am.oid = idx.relam`,
		billingOverviewTimeCoverIndex, billingOverviewUsageCoverIndex, predicate).Scan(&valid).Error; errValidate != nil {
		return fmt.Errorf("validate billing overview indexes: %w", errValidate)
	}
	if !valid {
		return fmt.Errorf("billing overview indexes are invalid or have an unexpected definition")
	}
	return nil
}
