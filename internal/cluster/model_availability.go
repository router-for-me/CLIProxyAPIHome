package cluster

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

// ModelAvailabilityQuery selects the observation window and the models to
// summarize.
type ModelAvailabilityQuery struct {
	// ModelIDs restricts the summary to a set of models, matched without regard
	// to case. An empty set returns nothing rather than scanning every model the
	// cluster has ever served, because every caller of this already knows which
	// models it is about to display.
	ModelIDs []string
	// From is the inclusive start of the observation window.
	From time.Time
	// ToExclusive is the exclusive end of the observation window.
	ToExclusive time.Time
}

// ModelAvailabilitySummary is what usage records say about one model over one
// window.
//
// Averages are pointers because "no request had a measurable latency" is a real
// outcome that must not be reported as a latency of zero. Counts are plain
// integers because zero requests is genuinely zero requests.
type ModelAvailabilitySummary struct {
	// ModelID is one of the spellings the usage records carry for this model.
	// Records are folded together case-insensitively, so when a model was
	// written more than one way this is the lowest such spelling rather than
	// the only one.
	ModelID string
	// SampleCount is how many requests were observed in the window. It is the
	// number a caller weighs before trusting any of the other fields.
	SampleCount int64
	// SuccessCount and FailedCount partition SampleCount.
	SuccessCount int64
	FailedCount  int64
	// AvgLatencyMS is the mean end-to-end latency of measured requests.
	AvgLatencyMS *float64
	// AvgTTFTMS is the mean time to first token. Only streaming responses
	// report it, so it can be absent while AvgLatencyMS is present.
	AvgTTFTMS *float64
	// ThroughputOutputTokens and ThroughputLatencyMS are the numerator and
	// denominator of output tokens per second, kept separate so the caller can
	// decide whether the sample is large enough to divide.
	ThroughputOutputTokens int64
	ThroughputLatencyMS    int64
	// FirstObservedAt and LastObservedAt bound the observations actually found,
	// which can be much narrower than the requested window.
	FirstObservedAt *time.Time
	LastObservedAt  *time.Time
}

type modelAvailabilityRow struct {
	// ModelKey is the grouping key the database itself computed. Keying the
	// result map by it rather than by re-folding Model in Go guarantees the map
	// has exactly one entry per group, whatever the two case-folding rules
	// would disagree about.
	ModelKey               string          `gorm:"column:model_key"`
	Model                  string          `gorm:"column:model"`
	SampleCount            int64           `gorm:"column:sample_count"`
	SuccessCount           sql.NullInt64   `gorm:"column:success_count"`
	FailedCount            sql.NullInt64   `gorm:"column:failed_count"`
	AvgLatencyMS           sql.NullFloat64 `gorm:"column:avg_latency_ms"`
	AvgTTFTMS              sql.NullFloat64 `gorm:"column:avg_ttft_ms"`
	ThroughputOutputTokens sql.NullInt64   `gorm:"column:throughput_output_tokens"`
	ThroughputLatencyMS    sql.NullInt64   `gorm:"column:throughput_latency_ms"`
	// MIN/MAX over a timestamp column comes back as a driver string on SQLite
	// and as a time on Postgres, so these are scanned as text and parsed with
	// the same layout set the observability aggregates already use.
	FirstObservedAt sql.NullString `gorm:"column:first_observed_at"`
	LastObservedAt  sql.NullString `gorm:"column:last_observed_at"`
}

// ListModelAvailability summarizes observed reliability and speed per model.
//
// This is deliberately not built on the operator observability aggregate. That
// aggregate answers a much wider question, joins billing and credential tables
// to do it, and is shaped by what an operator console needs on screen. Reusing
// it here would drag per-user amounts and credential identities into a query
// whose result is shown to buyers, so the narrower question gets its own query
// over the usage table alone.
func (r *Repository) ListModelAvailability(ctx context.Context, query ModelAvailabilityQuery) (map[string]ModelAvailabilitySummary, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}

	// Match, group and key on the folded identifier throughout. A usage record
	// carries whatever spelling the request that produced it used, which is not
	// always the spelling the catalog registers, and the returned map is keyed
	// lower-cased. Folding only at the end would both drop the records whose
	// case differs from the catalog's and, where two spellings did survive,
	// collapse their two groups onto one key and keep whichever arrived last.
	wanted := make([]string, 0, len(query.ModelIDs))
	seen := make(map[string]struct{}, len(query.ModelIDs))
	for _, modelID := range query.ModelIDs {
		key := strings.ToLower(strings.TrimSpace(modelID))
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		wanted = append(wanted, key)
	}
	if len(wanted) == 0 {
		return map[string]ModelAvailabilitySummary{}, nil
	}

	// LOWER() on the column rules out an index on model alone, but no such
	// index exists and none would be used here anyway: the window predicate is
	// what the planner drives this query from.
	scope := db.WithContext(contextOrBackground(ctx)).
		Table("usage").
		Where(`LOWER("usage"."model") IN ?`, wanted)
	if !query.From.IsZero() {
		scope = scope.Where(`"usage"."timestamp" >= ?`, query.From)
	}
	if !query.ToExclusive.IsZero() {
		scope = scope.Where(`"usage"."timestamp" < ?`, query.ToExclusive)
	}

	var rows []modelAvailabilityRow
	if errScan := scope.Select(`
		LOWER("usage"."model") AS model_key,
		MIN("usage"."model") AS model,
		COUNT("usage"."id") AS sample_count,
		SUM(CASE WHEN "usage"."failed" THEN 0 ELSE 1 END) AS success_count,
		SUM(CASE WHEN "usage"."failed" THEN 1 ELSE 0 END) AS failed_count,
		AVG(CASE WHEN "usage"."failed" THEN NULL WHEN "usage"."latency_ms" > 0 THEN "usage"."latency_ms" END) AS avg_latency_ms,
		AVG(CASE WHEN "usage"."failed" THEN NULL WHEN "usage"."ttft_ms" > 0 THEN "usage"."ttft_ms" END) AS avg_ttft_ms,
		SUM(CASE WHEN "usage"."failed" THEN 0 WHEN "usage"."latency_ms" > 0 AND "usage"."output_tokens" > 0 THEN "usage"."output_tokens" ELSE 0 END) AS throughput_output_tokens,
		SUM(CASE WHEN "usage"."failed" THEN 0 WHEN "usage"."latency_ms" > 0 AND "usage"."output_tokens" > 0 THEN "usage"."latency_ms" ELSE 0 END) AS throughput_latency_ms,
		MIN("usage"."timestamp") AS first_observed_at,
		MAX("usage"."timestamp") AS last_observed_at`).
		Group(`LOWER("usage"."model")`).
		Scan(&rows).Error; errScan != nil {
		return nil, errScan
	}

	summaries := make(map[string]ModelAvailabilitySummary, len(rows))
	for i := range rows {
		modelID := strings.TrimSpace(rows[i].Model)
		key := strings.TrimSpace(rows[i].ModelKey)
		if modelID == "" || key == "" {
			continue
		}
		summary := ModelAvailabilitySummary{
			ModelID:                modelID,
			SampleCount:            rows[i].SampleCount,
			SuccessCount:           optionalSQLInt64Value(rows[i].SuccessCount),
			FailedCount:            optionalSQLInt64Value(rows[i].FailedCount),
			ThroughputOutputTokens: optionalSQLInt64Value(rows[i].ThroughputOutputTokens),
			ThroughputLatencyMS:    optionalSQLInt64Value(rows[i].ThroughputLatencyMS),
		}
		if rows[i].AvgLatencyMS.Valid {
			value := rows[i].AvgLatencyMS.Float64
			summary.AvgLatencyMS = &value
		}
		if rows[i].AvgTTFTMS.Valid {
			value := rows[i].AvgTTFTMS.Float64
			summary.AvgTTFTMS = &value
		}
		if rows[i].FirstObservedAt.Valid {
			if value, ok := usageObservabilityAggregateTime(rows[i].FirstObservedAt.String); ok {
				summary.FirstObservedAt = &value
			}
		}
		if rows[i].LastObservedAt.Valid {
			if value, ok := usageObservabilityAggregateTime(rows[i].LastObservedAt.String); ok {
				summary.LastObservedAt = &value
			}
		}
		summaries[key] = summary
	}
	return summaries, nil
}
