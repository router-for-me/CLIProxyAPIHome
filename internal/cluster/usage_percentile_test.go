package cluster

import (
	"bytes"
	"database/sql"
	"log"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestUsageObservabilityAggregateP95Backends(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			var repo *Repository
			if backend == "postgres" {
				repo = newPostgresQuiescenceRepository(t)
			} else {
				var closeRepo func()
				repo, closeRepo = newBillingTestRepository(t, t.Context())
				t.Cleanup(closeRepo)
			}
			db, errDB := repo.database()
			if errDB != nil {
				t.Fatal(errDB)
			}
			if errCreate := db.Create(&AuthRecord{UUID: "p95-uuid", ID: "p95-id", Index: "p95-index", Provider: "codex", AuthJSON: JSONB(`{}`)}).Error; errCreate != nil {
				t.Fatal(errCreate)
			}
			from := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
			to := from.Add(time.Hour)
			identities := []string{"p95-uuid", "p95-id", "p95-index"}
			var records []UsageRecord
			var latencies []int64
			for index := 0; index < 60; index++ {
				// Include zero, ties, failures and a non-maximum nearest-rank P95.
				latency := int64(index/2) * 100
				latencies = append(latencies, latency)
				records = append(records, UsageRecord{Timestamp: from, AuthIndex: identities[index%len(identities)], Provider: "codex",
					Model: "p95-model", AuthType: "oauth", Failed: index%3 == 0, LatencyMS: latency, PayloadJSON: JSONB(`{}`)})
			}
			for _, record := range []UsageRecord{
				{Timestamp: from, AuthIndex: "p95-index", Provider: "codex", LatencyMS: -1},
				{Timestamp: from, AuthIndex: "raw-only", Provider: "codex", LatencyMS: 20},
				{Timestamp: from, AuthIndex: "raw-only", Provider: "codex", LatencyMS: 0},
				{Timestamp: from, AuthIndex: "negative-only", Provider: "codex", LatencyMS: -1},
				{Timestamp: from, AuthIndex: "", Provider: "codex", LatencyMS: 50},
				{Timestamp: from.Add(-time.Second), AuthIndex: "p95-id", Provider: "codex", LatencyMS: 999999},
				{Timestamp: to, AuthIndex: "p95-id", Provider: "codex", LatencyMS: 999999},
				{Timestamp: from, AuthIndex: "p95-id", Provider: "excluded", LatencyMS: 999999},
			} {
				record.PayloadJSON = JSONB(`{}`)
				records = append(records, record)
			}
			if errCreate := db.Create(&records).Error; errCreate != nil {
				t.Fatal(errCreate)
			}
			query := UsageObservabilityRecordQuery{From: &from, To: &to, Provider: "codex"}
			var rows []struct {
				AggregateID  string          `gorm:"column:aggregate_id"`
				P95LatencyMS sql.NullFloat64 `gorm:"column:p95_latency_ms"`
			}
			if errScan := usageObservabilityAggregateP95Query(db, query, "credential").Scan(&rows).Error; errScan != nil {
				t.Fatal(errScan)
			}
			want := map[string]float64{"p95-uuid": usageObservabilityPercentile(latencies, 0.95), "raw-only": 20, "unknown": 50}
			if len(rows) != len(want) {
				t.Fatalf("P95 groups = %d, want %d", len(rows), len(want))
			}
			for _, row := range rows {
				value, exists := want[row.AggregateID]
				if !exists || !row.P95LatencyMS.Valid || row.P95LatencyMS.Float64 != value {
					t.Fatalf("unexpected P95 group: %+v, want %v", row, want)
				}
			}
			selected, errSelected := usageObservabilityAggregateP95Values(db, query, "credential", []string{"p95-uuid", "negative-only", "unknown"})
			if errSelected != nil {
				t.Fatal(errSelected)
			}
			if len(selected) != 2 || selected["p95-uuid"].Float64 != want["p95-uuid"] || selected["unknown"].Float64 != 50 {
				t.Fatalf("selected percentiles = %v", selected)
			}
			empty, errEmpty := usageObservabilityAggregateP95Values(db, query, "credential", nil)
			if errEmpty != nil || len(empty) != 0 {
				t.Fatalf("empty selection = %v, error=%v", empty, errEmpty)
			}
			for _, metric := range []string{"request_count", "p95_latency_ms"} {
				page, errPage := repo.ListUsageObservabilityAggregates(t.Context(), UsageObservabilityAggregateQuery{
					From: &from, To: &to, Provider: "codex", GroupBy: "credential", Metric: metric, Direction: "desc", Limit: 1})
				if errPage != nil {
					t.Fatal(errPage)
				}
				if page.Total != 4 || len(page.Items) != 1 || page.Items[0].ID != "p95-uuid" ||
					page.Items[0].P95LatencyMS == nil || *page.Items[0].P95LatencyMS != want["p95-uuid"] {
					t.Fatalf("%s page = %+v", metric, page)
				}
			}
			var queryLog bytes.Buffer
			queryDB := db.Session(&gorm.Session{Logger: logger.New(log.New(&queryLog, "", 0), logger.Config{
				LogLevel: logger.Info, ParameterizedQueries: true,
			})})
			if errScan := usageObservabilityAggregateP95Query(queryDB, query, "credential", "p95-uuid").Scan(&rows).Error; errScan != nil {
				t.Fatal(errScan)
			}
			statement := queryLog.String()
			if backend == "postgres" {
				if !strings.Contains(statement, "percentile_disc(") || strings.Contains(statement, "ROW_NUMBER()") {
					t.Fatalf("PostgreSQL did not use native percentiles: %s", statement)
				}
			} else if !strings.Contains(statement, "ROW_NUMBER()") {
				t.Fatalf("SQLite lost its window percentile fallback: %s", statement)
			}
		})
	}
}
