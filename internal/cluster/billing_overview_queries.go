package cluster

import (
	"context"
	"fmt"
	"strings"

	"gorm.io/gorm"
)

func billingOverviewTotalsQuery(db *gorm.DB, query BillingChargeQuery) *gorm.DB {
	scope := billingChargeQueryScope(db.Model(&BillingChargeRecord{}), query)
	inputTokens := billingDisplayTokenSQL("input_tokens")
	outputTokens := billingDisplayTokenSQL("output_tokens")
	users := `COUNT(DISTINCT "billing_charge"."user_id") AS active_user_count`
	isPostgres := usageObservabilityIsPostgres(db)
	if isPostgres {
		// Literal predicates let generic prepared plans use the partial index.
		scope = scope.Joins(fmt.Sprintf(`LEFT JOIN "usage" AS "billing_usage"
			ON "billing_usage"."id" = "billing_charge"."usage_id"
			AND "billing_usage"."token_accounting_version" = %d
			AND "billing_usage"."token_accounting_quality" = '%s'`,
			UsageTokenAccountingSchemaVersion, UsageTokenAccountingQualityComplete))
		// Row presence, rather than COALESCE(counter, fallback), preserves NULL counters.
		inputTokens = `CASE WHEN "billing_usage"."id" IS NOT NULL THEN "billing_usage"."accounting_input_tokens" ELSE "billing_charge"."input_tokens" END`
		outputTokens = `CASE WHEN "billing_usage"."id" IS NOT NULL THEN "billing_usage"."accounting_output_tokens" ELSE "billing_charge"."output_tokens" END`
		users = `"billing_charge"."user_id" AS user_id`
	} else {
		scope = scope.Joins(`LEFT JOIN "usage" AS "billing_usage" ON "billing_usage"."id" = "billing_charge"."usage_id"`)
	}
	totals := scope.Select(fmt.Sprintf(`
		COALESCE(SUM("billing_charge"."amount"), 0) AS total_charge_amount,
		COUNT(*) AS request_count,
		COALESCE(SUM(%s), 0) AS input_tokens,
		COALESCE(SUM(%s), 0) AS output_tokens,
		COALESCE(SUM("billing_charge"."cache_tokens"), 0) AS cache_tokens,
		%s,
		MIN("billing_charge"."created_at") AS min_created_at,
		MAX("billing_charge"."created_at") AS max_created_at`, inputTokens, outputTokens, users))
	if !isPostgres {
		return totals
	}
	// Grouping the user identity avoids a DISTINCT sort of all joined columns.
	totals = totals.Group(`"billing_charge"."user_id"`)
	return db.Table("(?) AS grouped_users", totals).Select(`
		COALESCE(SUM(total_charge_amount), 0) AS total_charge_amount,
		COALESCE(SUM(request_count), 0) AS request_count,
		COALESCE(SUM(input_tokens), 0) AS input_tokens,
		COALESCE(SUM(output_tokens), 0) AS output_tokens,
		COALESCE(SUM(cache_tokens), 0) AS cache_tokens,
		COUNT(user_id) AS active_user_count,
		MIN(min_created_at) AS min_created_at,
		MAX(max_created_at) AS max_created_at`)
}

func billingTopModelsAndProviders(ctx context.Context, db *gorm.DB, query BillingChargeQuery) ([]BillingTopItem, []BillingTopItem, error) {
	if !usageObservabilityIsPostgres(db) {
		models, errModels := billingTopModels(ctx, db, query)
		if errModels != nil {
			return nil, nil, errModels
		}
		providers, errProviders := billingTopProviders(ctx, db, query)
		return models, providers, errProviders
	}
	db = db.WithContext(contextOrBackground(ctx))
	grouped := billingChargeQueryScope(db.Model(&BillingChargeRecord{}), query).
		Select(`"billing_charge"."provider" AS provider, "billing_charge"."model" AS model,
			GROUPING("billing_charge"."model") AS kind,
			COALESCE(SUM("billing_charge"."amount"), 0) AS amount, COUNT(*) AS request_count`).
		Group(`GROUPING SETS (("billing_charge"."provider", "billing_charge"."model"), ("billing_charge"."provider"))`)
	// GROUPING distinguishes a real NULL model from the provider-only subtotal.
	ranked := db.Table("(?) AS grouped", grouped).
		Select(`grouped.*, ROW_NUMBER() OVER (
			PARTITION BY kind ORDER BY amount DESC, request_count DESC, provider ASC, model ASC
		) AS rank`)
	var rows []struct {
		Kind         int
		Provider     string
		Model        string
		Amount       float64
		RequestCount int64
	}
	if errScan := db.Table("(?) AS ranked", ranked).
		Select("kind, provider, model, amount, request_count").
		Where("rank <= ?", billingOverviewTopLimit).
		Order("kind ASC, rank ASC").Scan(&rows).Error; errScan != nil {
		return nil, nil, errScan
	}
	models := make([]BillingTopItem, 0, billingOverviewTopLimit)
	providers := make([]BillingTopItem, 0, billingOverviewTopLimit)
	for _, row := range rows {
		provider := strings.TrimSpace(row.Provider)
		if row.Kind == 1 {
			providers = append(providers, BillingTopItem{ID: provider, Label: provider, Amount: row.Amount, RequestCount: row.RequestCount})
			continue
		}
		model := strings.TrimSpace(row.Model)
		id := model
		if provider != "" && model != "" {
			id = provider + "/" + model
		}
		models = append(models, BillingTopItem{ID: id, Label: model, Amount: row.Amount, RequestCount: row.RequestCount})
	}
	return models, providers, nil
}
