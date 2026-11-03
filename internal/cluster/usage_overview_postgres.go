package cluster

import (
	"fmt"
	"strings"

	"gorm.io/gorm"
)

func usageObservabilityIsPostgres(db *gorm.DB) bool {
	return db != nil && db.Dialector != nil && db.Dialector.Name() == "postgres"
}

// percentile_disc preserves the existing nearest-rank semantics, including ties
// and zero latency. Negative latencies remain excluded from the distribution.
func usageObservabilityPostgresPercentile(column string, percentile int) string {
	return fmt.Sprintf("percentile_disc(%d / 100.0) WITHIN GROUP (ORDER BY %s) FILTER (WHERE %s >= 0)", percentile, column, column)
}

// usageObservabilityOverviewGroupsPostgres computes percentiles in the same scan
// as each ranking, avoiding an additional window sort and join per section.
func usageObservabilityOverviewGroupsPostgres(db *gorm.DB, query UsageObservabilityRecordQuery) (UsageObservabilityTopGroups, []UsageObservabilityAggregateItem, error) {
	top := UsageObservabilityTopGroups{
		Users: []UsageObservabilityAggregateItem{}, ClientKeys: []UsageObservabilityAggregateItem{},
		Credentials: []UsageObservabilityAggregateItem{}, Providers: []UsageObservabilityAggregateItem{},
		Models: []UsageObservabilityAggregateItem{}, Endpoints: []UsageObservabilityAggregateItem{}, Errors: []UsageObservabilityAggregateItem{},
	}
	efficiency := []UsageObservabilityAggregateItem{}
	var clientRows []struct {
		Kind int                            `gorm:"column:kind"`
		Row  usageObservabilityAggregateRow `gorm:"embedded"`
	}
	if errScan := usageObservabilityOverviewClientGroupsPostgres(db, query).Scan(&clientRows).Error; errScan != nil {
		return UsageObservabilityTopGroups{}, nil, errScan
	}
	for _, row := range clientRows {
		if row.Kind == 0 {
			top.Users = append(top.Users, usageObservabilityAggregateItemFromRow(&row.Row, "user"))
		} else {
			top.ClientKeys = append(top.ClientKeys, usageObservabilityAggregateItemFromRow(&row.Row, "client_key"))
		}
	}
	for _, group := range []struct {
		name  string
		items *[]UsageObservabilityAggregateItem
	}{
		{"provider", &top.Providers}, {"endpoint", &top.Endpoints}, {"status_code", &top.Errors},
	} {
		metric := "request_count"
		groupQuery := query
		if group.name == "status_code" {
			metric, groupQuery.Status = "failed_count", "failed"
		}
		var rows []usageObservabilityAggregateRow
		if errScan := db.Table("(?) AS scoped", usageObservabilityOverviewGroupPostgres(db, groupQuery, group.name)).
			Order(usageObservabilityPreAggregatedSQLOrder(metric, "desc")).Limit(10).Scan(&rows).Error; errScan != nil {
			return UsageObservabilityTopGroups{}, nil, errScan
		}
		for index := range rows {
			*group.items = append(*group.items, usageObservabilityAggregateItemFromRow(&rows[index], group.name))
		}
	}
	// Preserve credential pre-aggregation: resolving auth identities before
	// grouping would join auth three times for every request instead of per index.
	var errCredentials error
	top.Credentials, errCredentials = usageObservabilityAggregateItemsSQL(db, query, "credential", "request_count", "desc", 10)
	if errCredentials != nil {
		return UsageObservabilityTopGroups{}, nil, errCredentials
	}
	// Share the small model aggregate between the two rankings. Materializing
	// only grouped rows avoids spooling the entire usage window to temporary disk.
	var modelRows []struct {
		Efficiency bool                           `gorm:"column:efficiency"`
		Row        usageObservabilityAggregateRow `gorm:"embedded"`
	}
	models := db.Table("model_aggregate AS scoped").Select("FALSE AS efficiency, scoped.*").
		Order(usageObservabilityPreAggregatedSQLOrder("request_count", "desc")).Limit(10)
	efficientModels := db.Table("model_aggregate AS scoped").Select("TRUE AS efficiency, scoped.*").
		Order(usageObservabilityPreAggregatedSQLOrder("total_tokens", "desc")).Limit(10)
	// The final ordering is explicit because UNION ALL does not guarantee that
	// the two inputs' order is preserved.
	statement := `WITH model_aggregate AS MATERIALIZED (?)
		SELECT * FROM ((?) UNION ALL (?)) AS ranked
		ORDER BY efficiency, CASE WHEN efficiency THEN accounting_total_tokens ELSE request_count END DESC,
		last_used_at DESC, aggregate_label ASC`
	if errScan := db.Raw(statement, usageObservabilityOverviewGroupPostgres(db, query, "model"), models, efficientModels).Scan(&modelRows).Error; errScan != nil {
		return UsageObservabilityTopGroups{}, nil, errScan
	}
	for _, row := range modelRows {
		item := usageObservabilityAggregateItemFromRow(&row.Row, "model")
		if row.Efficiency {
			efficiency = append(efficiency, item)
		} else {
			top.Models = append(top.Models, item)
		}
	}
	return top, efficiency, nil
}

// usageObservabilityOverviewClientGroupsPostgres shares the detail scan between
// both client rankings, retaining exact percentiles in the same aggregation pass.
func usageObservabilityOverviewClientGroupsPostgres(db *gorm.DB, query UsageObservabilityRecordQuery) *gorm.DB {
	_, keyLabel := usageObservabilityAggregateSQLIdentity("client_key")
	base := usageObservabilityUsageBillingScope(db.Table("usage"), query).
		Joins(`LEFT JOIN "api_key" ON "api_key"."api_key" = "usage"."api_key"`).
		Select(fmt.Sprintf(`
			COALESCE("billing_charge"."user_id", "api_key"."user_id") AS user_id,
			COALESCE("billing_charge"."api_key_id", "api_key"."id") AS api_key_id,
			%s AS client_key_label,
			COALESCE("billing_charge"."api_key_label", CASE WHEN "api_key"."id" IS NULL THEN '' ELSE 'api-key-' || CAST("api_key"."id" AS TEXT) END) AS api_key_label,
			"billing_charge"."api_key_masked" AS api_key_masked,
			%s`, keyLabel, usageObservabilityAggregateMetricsBaseSelect()))
	// GROUPING distinguishes a genuine unknown identity from the other subtotal.
	// Keep numeric identities until after grouping and preserve billed ownership
	// independently of the current API key's user or identity.
	grouped := db.Table("(?) AS scoped", base).
		Select(`
			GROUPING(scoped.user_id) AS kind,
			CASE WHEN GROUPING(scoped.user_id) = 0 THEN scoped.user_id ELSE scoped.api_key_id END AS numeric_id,
			MAX(scoped.user_id) AS metadata_user_id,
			MAX(scoped.api_key_id) AS metadata_api_key_id,
			MAX(scoped.client_key_label) AS client_key_label,
			MAX(scoped.api_key_label) AS metadata_api_key_label,
			MAX(scoped.api_key_masked) AS metadata_api_key_masked,
			` + usageObservabilityAggregateMetricsSQLSelect(usageObservabilityPostgresPercentile("scoped.latency_ms", 95)+" AS p95_latency_ms")).
		Group("GROUPING SETS ((scoped.user_id), (scoped.api_key_id))")
	// Usernames are needed only for grouped users, but must be resolved before
	// Top N because the existing order uses the display label to break ties.
	labeled := db.Table("(?) AS grouped", grouped).
		Joins(`LEFT JOIN "user" ON grouped.kind = 0 AND "user"."id" = grouped.numeric_id`).
		Select(`grouped.*,
			COALESCE(CAST(grouped.numeric_id AS TEXT), 'unknown') AS aggregate_id,
			CASE WHEN grouped.kind = 0
				THEN COALESCE(NULLIF("user"."username", ''), COALESCE(CAST(grouped.numeric_id AS TEXT), 'unknown'))
				ELSE grouped.client_key_label END AS aggregate_label,
			CASE WHEN grouped.kind = 0 THEN "user"."username" ELSE '' END AS metadata_username`)
	ranked := db.Table("(?) AS labeled", labeled).
		Select(`labeled.*, ROW_NUMBER() OVER (
			PARTITION BY kind ORDER BY request_count DESC, last_used_at DESC, aggregate_label ASC
		) AS rank`)
	return db.Table("(?) AS ranked", ranked).
		Where("rank <= ?", 10).
		Order("kind ASC, rank ASC")
}

func usageObservabilityOverviewGroupPostgres(db *gorm.DB, query UsageObservabilityRecordQuery, group string) *gorm.DB {
	base := usageObservabilityAggregateBaseQuery(db, query, group)
	selectSQL := strings.Replace(usageObservabilityAggregateSQLSelect(false), "NULL AS p95_latency_ms", usageObservabilityPostgresPercentile("scoped.latency_ms", 95)+" AS p95_latency_ms", 1)
	return db.Table("(?) AS scoped", base).Select(selectSQL).Group("scoped.aggregate_id")
}
