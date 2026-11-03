package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Use the existing aggregate queries as an independent oracle for the PostgreSQL
// overview. Each invocation gets an isolated schema in HOME_TEST_POSTGRES_DSN.
func TestUsageObservabilityOverviewPostgres(t *testing.T) {
	repo := newPostgresQuiescenceRepository(t)
	ctx := context.Background()
	db, errDB := repo.database()
	if errDB != nil {
		t.Fatal(errDB)
	}
	from := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	to := from.Add(48 * time.Hour)
	username, credits := "overview-user", 100.0
	user, errUser := repo.CreateUser(ctx, UserUpdate{Username: &username, Credits: &credits})
	if errUser != nil {
		t.Fatal(errUser)
	}
	clientKey := "overview-test-key"
	key, errKey := repo.CreateAPIKeyForUser(ctx, user.ID, APIKeyUserUpdate{APIKey: &clientKey})
	if errKey != nil {
		t.Fatal(errKey)
	}
	auth := &coreauth.Auth{ID: "overview-credential", Index: "overview-credential", Provider: "codex", Label: "Overview OAuth", Status: coreauth.StatusActive, NextRetryAfter: to}
	if _, errAuth := repo.UpsertAuth(ctx, auth, "test"); errAuth != nil {
		t.Fatal(errAuth)
	}
	identities := []string{auth.ID, auth.Index, "overview-credential", "deleted-credential", ""}
	providers := []string{"codex", "claude", "gemini", "openai-compatible-example"}
	var records []UsageRecord
	for i := 0; i < 144; i++ {
		record := UsageRecord{
			Timestamp: from.Add(time.Duration(i) * 10 * time.Minute), CreatedAt: from,
			Provider: providers[i%len(providers)], Model: fmt.Sprintf("model-%02d", i%12),
			AuthIndex: identities[i%len(identities)], AuthType: "oauth", APIKey: clientKey,
			Endpoint: "/v1/responses", HomeIP: "192.0.2.1", ExecutorType: "CodexExecutor",
			LatencyMS: int64(i%21-1) * 100, Failed: i%7 == 0, FailStatusCode: 429,
			InputTokens: int64(10 + i), OutputTokens: 5, ReasoningTokens: 2,
			CachedTokens: 3, CacheCreationTokens: 4, CacheReadTokensPresent: i%2 == 0,
			PayloadJSON: JSONB(`{}`),
		}
		if i%3 == 0 {
			record.TokenAccountingVersion = UsageTokenAccountingSchemaVersion
			record.TokenAccountingQuality = UsageTokenAccountingQualityComplete
			record.AccountingInputTokens = int64(1000 + i)
			record.AccountingOutputTokens = 20
			record.AccountingTotalTokens = record.AccountingInputTokens + record.AccountingOutputTokens
			record.AccountingCacheReadTokens = 10
			record.AccountingCacheWriteTokens = 5
			record.UncachedInputTokens = record.AccountingInputTokens - 15
			record.NonReasoningOutputTokens = 18
			record.AccountingReasoningTokens = 2
		}
		if i%11 == 0 {
			record.TokenAccountingQuality = UsageTokenAccountingQualityInconsistent
		}
		if i%13 == 0 {
			record.APIKey, record.AuthType, record.Endpoint, record.HomeIP = "missing-key", "api_key", "/v1/chat/completions", "192.0.2.2"
		}
		records = append(records, record)
	}
	// A record exactly at the end must be excluded, including from every CTE.
	records = append(records, UsageRecord{Timestamp: to, CreatedAt: from, Provider: "excluded", Model: "excluded", PayloadJSON: JSONB(`{}`)})
	if errCreate := db.Create(&records).Error; errCreate != nil {
		t.Fatal(errCreate)
	}
	for i, record := range records {
		if i%4 == 0 {
			continue // Exercise nullable amounts and the API key identity fallback.
		}
		charge := BillingChargeRecord{ID: fmt.Sprintf("charge-%d", i), PayloadHash: fmt.Sprintf("hash-%d", i), UsageID: record.ID,
			UserID: &user.ID, APIKeyID: &key.ID, APIKeyLabel: "Frozen label", APIKeyMasked: "over...key",
			Amount: float64(i%8) / 8, PriceSnapshot: JSONB(`{}`), CreatedAt: record.Timestamp}
		if errCreate := db.Create(&charge).Error; errCreate != nil {
			t.Fatal(errCreate)
		}
	}

	for _, filter := range []struct {
		name, provider, model, credentialType, homeIP, endpoint string
	}{
		{name: "all"},
		{name: "provider", provider: "claude"},
		{name: "model", model: "model-02"},
		{name: "credential type", credentialType: "api_key"},
		{name: "home", homeIP: "192.0.2.2"},
		{name: "endpoint", endpoint: "chat/completions"},
		{name: "combined", provider: "claude", credentialType: "api_key", homeIP: "192.0.2.2", endpoint: "chat/completions"},
		{name: "empty", provider: "missing"},
	} {
		t.Run(filter.name, func(t *testing.T) {
			query := UsageObservabilityOverviewQuery{From: &from, To: &to, Interval: "hour", Timezone: "Asia/Shanghai",
				Provider: filter.provider, Model: filter.model, CredentialType: filter.credentialType, HomeIP: filter.homeIP, Endpoint: filter.endpoint}
			counter := &overviewQueryCounter{Interface: logger.Default.LogMode(logger.Silent)}
			countedRepo := NewRepository(db.Session(&gorm.Session{Logger: counter}))
			got, errOverview := countedRepo.UsageObservabilityOverview(ctx, query)
			if errOverview != nil {
				t.Fatal(errOverview)
			}
			if counter.count > 11 {
				t.Fatalf("overview executed %d queries, want at most 11", counter.count)
			}
			if filter.name == "all" && counter.count != 11 {
				t.Fatalf("nonempty overview executed %d queries, want 11", counter.count)
			}
			recordQuery := usageObservabilityOverviewRecordQuery(query)
			wantTop, errTop := usageObservabilityTopSQL(db, recordQuery)
			if errTop != nil {
				t.Fatal(errTop)
			}
			wantEfficiency, errEfficiency := usageObservabilityAggregateItemsSQL(db, recordQuery, "model", "total_tokens", "desc", 10)
			if errEfficiency != nil {
				t.Fatal(errEfficiency)
			}
			assertOverviewJSONEqual(t, "top groups", got.Top, wantTop)
			assertOverviewJSONEqual(t, "model efficiency", got.ModelEfficiency, wantEfficiency)
			wantPercentiles, errPercentiles := usageObservabilityLatencyPercentilesSQL(db, recordQuery)
			if errPercentiles != nil {
				t.Fatal(errPercentiles)
			}
			for _, value := range []struct {
				got   *float64
				valid bool
				want  float64
			}{
				{got.Totals.P50LatencyMS, wantPercentiles.P50LatencyMS.Valid, wantPercentiles.P50LatencyMS.Float64},
				{got.Totals.P95LatencyMS, wantPercentiles.P95LatencyMS.Valid, wantPercentiles.P95LatencyMS.Float64},
			} {
				if (value.got != nil) != value.valid || value.got != nil && *value.got != value.want {
					t.Fatalf("latency percentile = %v, want valid=%v value=%v", value.got, value.valid, value.want)
				}
			}
			location := usageObservabilityLocation(query.Timezone)
			bucketExpr, bucketArgs := usageObservabilityTrendBucketUnixSQL(db, query.Interval, location, recordQuery, usageObservabilityOverviewBounds{})
			wantTrendP95, errTrend := usageObservabilityTrendP95SQL(db, recordQuery, bucketExpr, bucketArgs)
			if errTrend != nil {
				t.Fatal(errTrend)
			}
			for _, point := range got.Trend {
				want := wantTrendP95[point.BucketStart.Unix()]
				if (point.P95LatencyMS != nil) != want.Valid || point.P95LatencyMS != nil && *point.P95LatencyMS != want.Float64 {
					t.Fatalf("bucket %v percentile = %v, want %+v", point.BucketStart, point.P95LatencyMS, want)
				}
			}
			if filter.name == "all" && got.Totals.RequestCount != 144 {
				t.Fatalf("request count = %d, want 144", got.Totals.RequestCount)
			}
		})
	}
	ctxCancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, errOverview := repo.UsageObservabilityOverview(ctxCancelled, UsageObservabilityOverviewQuery{}); !errors.Is(errOverview, context.Canceled) {
		t.Fatalf("cancelled overview error = %v", errOverview)
	}
}

type overviewQueryCounter struct {
	logger.Interface
	count int
}

func TestUsageObservabilityLiveSummary(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			var repo *Repository
			if backend == "postgres" {
				repo = newPostgresQuiescenceRepository(t)
			} else {
				var closeRepo func()
				repo, closeRepo = newBillingTestRepository(t, ctx)
				t.Cleanup(closeRepo)
			}
			db, errDB := repo.database()
			if errDB != nil {
				t.Fatal(errDB)
			}
			now := time.Now().UTC()
			var records []UsageRecord
			for i, latency := range []int64{-1, 0, 100, 400} {
				record := UsageRecord{Timestamp: now.Add(-time.Minute), CreatedAt: now, Provider: "codex", LatencyMS: latency,
					InputTokens: 10, OutputTokens: 5, Failed: i == 0, PayloadJSON: JSONB(`{}`)}
				if i == 3 {
					record.TokenAccountingVersion = UsageTokenAccountingSchemaVersion
					record.TokenAccountingQuality = UsageTokenAccountingQualityComplete
					record.AccountingTotalTokens = 200
				}
				records = append(records, record)
			}
			records = append(records,
				UsageRecord{Timestamp: now.Add(-10 * time.Minute), CreatedAt: now, Provider: "codex", PayloadJSON: JSONB(`{}`)},
				UsageRecord{Timestamp: now.Add(-time.Minute), CreatedAt: now, Provider: "excluded", PayloadJSON: JSONB(`{}`)})
			if errCreate := db.Create(&records).Error; errCreate != nil {
				t.Fatal(errCreate)
			}
			from := now.Add(-time.Hour)
			query := UsageObservabilityRecordQuery{From: &from, To: &now, Provider: "codex"}
			got, errLive := usageObservabilityLiveSQL(db, query, 300)
			if errLive != nil {
				t.Fatal(errLive)
			}
			p50, p95 := 100.0, 400.0
			want := UsageObservabilityLiveSummary{WindowSeconds: 300, RPM: 0.8, TPM: 49, ErrorRate: 0.25, SuccessRate: 0.75, P50LatencyMS: &p50, P95LatencyMS: &p95}
			assertOverviewJSONEqual(t, "live summary", got, want)
			query.From = &now
			empty, errEmpty := usageObservabilityLiveSQL(db, query, 0)
			if errEmpty != nil {
				t.Fatal(errEmpty)
			}
			assertOverviewJSONEqual(t, "empty live summary", empty, UsageObservabilityLiveSummary{WindowSeconds: 300})
		})
	}
}

func (c *overviewQueryCounter) Trace(_ context.Context, _ time.Time, _ func() (string, int64), _ error) {
	c.count++
}

func assertOverviewJSONEqual(t *testing.T, name string, got, want any) {
	t.Helper()
	gotJSON, errGot := json.Marshal(got)
	if errGot != nil {
		t.Fatal(errGot)
	}
	wantJSON, errWant := json.Marshal(want)
	if errWant != nil {
		t.Fatal(errWant)
	}
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("%s mismatch\ngot: %s\nwant: %s", name, gotJSON, wantJSON)
	}
}
