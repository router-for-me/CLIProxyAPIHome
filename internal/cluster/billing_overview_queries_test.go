package cluster

import (
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestBillingOverviewPostgresQueries(t *testing.T) {
	repo := newPostgresQuiescenceRepository(t)
	db, errDB := repo.database()
	if errDB != nil {
		t.Fatal(errDB)
	}
	// Exercise historical nullable counters without changing production constraints.
	if errNullable := db.Exec(`ALTER TABLE usage ALTER COLUMN accounting_input_tokens DROP NOT NULL, ALTER COLUMN accounting_output_tokens DROP NOT NULL`).Error; errNullable != nil {
		t.Fatal(errNullable)
	}
	from := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	to := from.Add(72 * time.Hour)
	users := make([]UserRecord, 16)
	for i := range users {
		users[i] = UserRecord{Username: fmt.Sprintf("finance-user-%02d", i), Credits: 100}
	}
	if errCreate := db.Create(&users).Error; errCreate != nil {
		t.Fatal(errCreate)
	}
	for i := 0; i < 96; i++ {
		stamp := from.Add(time.Duration(i-1) * time.Hour)
		usage := UsageRecord{Timestamp: stamp, CreatedAt: stamp, PayloadJSON: JSONB(`{}`),
			TokenAccountingVersion: 2, TokenAccountingQuality: UsageTokenAccountingQualityComplete,
			AccountingInputTokens: int64(1000 + i), AccountingOutputTokens: int64(100 + i)}
		switch i % 8 {
		case 1:
			usage.AccountingInputTokens, usage.AccountingOutputTokens = 0, 0
		case 2:
			usage.TokenAccountingQuality = UsageTokenAccountingQualityUnclassified
		case 3:
			usage.TokenAccountingQuality = UsageTokenAccountingQualityInconsistent
		case 4:
			usage.TokenAccountingVersion = 0
		case 5:
			usage.TokenAccountingVersion = 3
		}
		if errCreate := db.Create(&usage).Error; errCreate != nil {
			t.Fatal(errCreate)
		}
		charge := BillingChargeRecord{ID: fmt.Sprintf("finance-charge-%03d", i), PayloadHash: fmt.Sprintf("finance-hash-%03d", i),
			UsageID: usage.ID, UserID: &users[i%len(users)].ID, Provider: fmt.Sprintf("provider-%02d", i%12), Model: fmt.Sprintf("model-%02d", i%17),
			Amount: float64(i%4+1) / 8, InputTokens: int64(10 + i), OutputTokens: int64(3 + i), CacheTokens: int64(i % 7),
			PriceSnapshot: JSONB(`{}`), CreatedAt: stamp}
		if i%7 == 0 {
			charge.UserID = nil
		}
		if errCreate := db.Create(&charge).Error; errCreate != nil {
			t.Fatal(errCreate)
		}
		if i%8 == 7 {
			if errDelete := db.Delete(&usage).Error; errDelete != nil {
				t.Fatal(errDelete)
			}
		} else if i%8 == 6 {
			if errNull := db.Model(&usage).Updates(map[string]any{"accounting_input_tokens": nil, "accounting_output_tokens": nil}).Error; errNull != nil {
				t.Fatal(errNull)
			}
		}
		// Real NULL and empty model/provider names must not become subtotal rows.
		if i%11 == 0 {
			if errNull := db.Model(&charge).Updates(map[string]any{"provider": nil, "model": nil}).Error; errNull != nil {
				t.Fatal(errNull)
			}
		} else if i%13 == 0 {
			if errEmpty := db.Model(&charge).Updates(map[string]any{"provider": "", "model": ""}).Error; errEmpty != nil {
				t.Fatal(errEmpty)
			}
		}
	}
	if errDelete := db.Delete(&users[3]).Error; errDelete != nil {
		t.Fatal(errDelete)
	}
	for _, tc := range []struct {
		name  string
		query BillingOverviewQuery
	}{
		{"all", BillingOverviewQuery{}},
		{"user ID", BillingOverviewQuery{UserID: &users[0].ID}},
		{"deleted user", BillingOverviewQuery{UserID: &users[3].ID}},
		{"user text", BillingOverviewQuery{UserText: "finance-user-0"}},
		{"user and text", BillingOverviewQuery{UserID: &users[0].ID, UserText: users[0].Username}},
		{"provider", BillingOverviewQuery{Provider: "PROVIDER-01"}},
		{"model", BillingOverviewQuery{Model: "model-01"}},
		{"combined filters", BillingOverviewQuery{UserText: "finance-user", Provider: "provider-01", Model: "model"}},
		{"wildcard", BillingOverviewQuery{Model: "%"}},
		{"quoted filter", BillingOverviewQuery{Model: "model' OR '1'='1"}},
		{"empty", BillingOverviewQuery{Provider: "missing"}},
		{"timezone", BillingOverviewQuery{Timezone: "Asia/Shanghai"}},
		{"DST", BillingOverviewQuery{Timezone: "America/Los_Angeles"}},
		{"unbounded", BillingOverviewQuery{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			query := tc.query
			if tc.name != "unbounded" {
				query.From, query.ToExclusive = &from, &to
			}
			counter := &overviewQueryCounter{Interface: logger.Default.LogMode(logger.Silent)}
			got, errOverview := NewRepository(db.Session(&gorm.Session{Logger: counter})).BillingOverview(t.Context(), query)
			if errOverview != nil {
				t.Fatal(errOverview)
			}
			if counter.count > 7 {
				t.Fatalf("executed %d SELECTs, want at most 7", counter.count)
			}
			chargeQuery := billingChargeQueryFromOverview(query)
			var want struct {
				Amount                             float64
				Count, Input, Output, Cache, Users int64
			}
			// Keep the previous one-pass SQL as an independent aggregation oracle.
			if errOracle := billingChargeQueryScope(db.Model(&BillingChargeRecord{}), chargeQuery).
				Joins(`LEFT JOIN usage AS billing_usage ON billing_usage.id = billing_charge.usage_id`).
				Select(`COALESCE(SUM(billing_charge.amount),0) AS amount, COUNT(*) AS count,
					COALESCE(SUM(` + billingDisplayTokenSQL("input_tokens") + `),0) AS input,
					COALESCE(SUM(` + billingDisplayTokenSQL("output_tokens") + `),0) AS output,
					COALESCE(SUM(billing_charge.cache_tokens),0) AS cache,
					COUNT(DISTINCT billing_charge.user_id) AS users`).Scan(&want).Error; errOracle != nil {
				t.Fatal(errOracle)
			}
			if math.Abs(got.TotalChargeAmount-want.Amount) > 1e-10 || got.RequestCount != want.Count || got.InputTokens != want.Input || got.OutputTokens != want.Output || got.CacheTokens != want.Cache || got.ActiveUserCount != want.Users {
				t.Fatalf("totals mismatch: got %+v, want %+v", got, want)
			}
			wantModels, errModels := billingTopModels(t.Context(), db, chargeQuery)
			wantProviders, errProviders := billingTopProviders(t.Context(), db, chargeQuery)
			wantUsers, errUsers := billingTopUsers(t.Context(), db, chargeQuery)
			if errModels != nil || errProviders != nil || errUsers != nil {
				t.Fatalf("ranking oracle: %v / %v / %v", errModels, errProviders, errUsers)
			}
			if !reflect.DeepEqual(got.TopModels, wantModels) || !reflect.DeepEqual(got.TopProviders, wantProviders) || !reflect.DeepEqual(got.TopUsers, wantUsers) {
				t.Fatalf("ranking results/order changed: %+v", got)
			}
			var trendCount int64
			var trendAmount float64
			for _, point := range got.DailyTrend {
				trendCount += point.RequestCount
				trendAmount += point.ChargeAmount
			}
			if trendCount != got.RequestCount || math.Abs(trendAmount-got.TotalChargeAmount) > 1e-10 {
				t.Fatal("trend no longer reconciles with totals")
			}
		})
	}

	// Generic plans must see literal accounting predicates, not bind parameters.
	var rows []map[string]any
	statement := billingOverviewTotalsQuery(db.Session(&gorm.Session{DryRun: true}), BillingChargeQuery{From: &from, ToExclusive: &to}).Find(&rows).Statement
	text := statement.SQL.String()
	if !strings.Contains(text, `"token_accounting_version" = 2`) || !strings.Contains(text, `"token_accounting_quality" = 'complete'`) {
		t.Fatalf("partial-index predicate was parameterized: %s", text)
	}
	if strings.Contains(text, "COUNT(DISTINCT") {
		t.Fatal("PostgreSQL totals still sort wide rows for DISTINCT")
	}
}
