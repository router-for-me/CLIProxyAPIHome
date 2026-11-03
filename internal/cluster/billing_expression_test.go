package cluster

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func TestBillingPricingExpressionCachedTextCharges(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	user := newBillingCaseUser(t, repo, "cached-text")
	for _, tc := range []struct {
		model, expression string
		want              float64
	}{
		{"separate", "p * 10 + ct * 1 + c * 20", 0.0028},
		{"cache-parent", "p * 10 + cr * 2 + c * 20", 0.0036},
		{"input-parent", "p * 10 + c * 20", 0.01},
		{"free", "p * 10 + ct * 0 + c * 20", 0.002},
	} {
		if _, errCreate := repo.CreateBillingModelPrice(ctx, BillingModelPriceUpdate{Provider: "openai", Model: tc.model, Enabled: true, PricingExpr: tc.expression}); errCreate != nil {
			t.Fatal(errCreate)
		}
		payload := `{"timestamp":"2026-10-27T01:00:00Z","provider":"openai","model":"` + tc.model + `","api_key":"cached-text","request_id":"` + tc.model + `","accounting_version":2,"token_breakdown":{"schema_version":2,"quality":"complete","total_tokens":1000,"input":{"total_tokens":1000,"uncached_tokens":200,"cache_read_tokens":800,"cache_write_tokens":0},"output":{"total_tokens":0,"non_reasoning_tokens":0,"reasoning_tokens":0}},"tokens":{"input_tokens":1000,"cached_tokens":800,"cache_read_tokens":800,"cache_read_tokens_present":true,"cached_text_tokens":800,"total_tokens":1000}}`
		usage, errUsage := repo.AppendUsage(ctx, payload, "192.0.2.1")
		if errUsage != nil {
			t.Fatal(errUsage)
		}
		if usage.TokenAccountingQuality != UsageTokenAccountingQualityComplete {
			t.Fatal("expected complete token accounting")
		}
		charge, ok := billingCaseCharges(t, repo, user.ID)[tc.model]
		if !ok || charge.Amount != tc.want || math.Abs(charge.BalanceBefore-charge.BalanceAfter-tc.want) > 1e-10 {
			t.Errorf("%s: found=%v amount=%v balance=%v -> %v, want delta %v; breakdown=%s", tc.model, ok, charge.Amount, charge.BalanceBefore, charge.BalanceAfter, tc.want, charge.CostBreakdown)
		}
		if tc.model == "separate" {
			var cost BillingCostBreakdown
			if errDecode := json.Unmarshal(charge.CostBreakdown, &cost); errDecode != nil {
				t.Fatal(errDecode)
			}
			if cost.Items["expr:ct"].Tokens != 800 || cost.Items["expr:p"].Tokens != 200 {
				t.Errorf("cached text audit = %+v", cost.Items)
			}
		}
	}
}

func TestBillingExpressionCachedTextClampsToCacheBucket(t *testing.T) {
	t.Parallel()
	for _, complete := range []bool{false, true} {
		usage := &UsageRecord{Provider: "openai", InputTokens: 1000, CacheReadTokens: 800, CacheReadTokensPresent: true, TotalTokens: 1000, TokensJSON: JSONB(`{"cached_text_tokens":900,"cached_image_tokens":100,"cached_audio_tokens":200}`)}
		usage.TokenAccountingVersion = UsageTokenAccountingSchemaVersion
		usage.TokenAccountingQuality = UsageTokenAccountingQualityUnclassified
		usage.AccountingTotalTokens = 1000
		usage.UnclassifiedTokens = 1000
		wantText := float64(800)
		if complete {
			usage.TokenAccountingVersion = UsageTokenAccountingSchemaVersion
			usage.TokenAccountingQuality = UsageTokenAccountingQualityComplete
			usage.AccountingTotalTokens = 1000
			usage.AccountingInputTokens = 1000
			usage.UnclassifiedTokens = 0
			usage.UncachedInputTokens = 200
			usage.AccountingCacheReadTokens = 800
			wantText = 500
		}
		numbers := billingExpressionTokens(usage)
		if numbers["ct"] != wantText || numbers["cr"] != 0 {
			t.Errorf("complete=%v: buckets=%v", complete, numbers)
		}
		cost := billingChargeCost(usage, BillingPriceSnapshot{PricingExpr: "p * 10 + cr * 1"})
		if cost.Total != 0.0028 {
			t.Errorf("complete=%v: folded cost=%+v", complete, cost)
		}
	}
}

func TestBillingPricingExpressionCharges(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	username := "expr@example.com"
	credits := 100.0
	user, errCreateUser := repo.CreateUser(ctx, UserUpdate{Username: &username, Credits: &credits})
	if errCreateUser != nil {
		t.Fatalf("CreateUser() error = %v", errCreateUser)
	}
	clientKey := "expr-key"
	if _, errCreateKey := repo.CreateAPIKeyForUser(ctx, user.ID, APIKeyUserUpdate{APIKey: &clientKey}); errCreateKey != nil {
		t.Fatalf("CreateAPIKeyForUser() error = %v", errCreateKey)
	}
	if _, errInvalid := repo.CreateBillingModelPrice(ctx, BillingModelPriceUpdate{Provider: "claude", Model: "bad", Enabled: true, PricingExpr: "p * nope"}); errInvalid == nil || !strings.Contains(errInvalid.Error(), "pricing_expr") {
		t.Fatalf("invalid pricing_expr error = %v", errInvalid)
	}
	// Columns are ignored when an expression is set; cache writes are unpriced so they fold into p.
	expression := "len > 200000 ? p * 6 + c * 22.5 + cr * 0.6 : p * 3 + c * 15 + cr * 0.3"
	if _, errCreate := repo.CreateBillingModelPrice(ctx, BillingModelPriceUpdate{Provider: "claude", Model: "claude-opus-5", InputPricePerMillion: 999, RequestPrice: 5, Enabled: true, PricingExpr: expression}); errCreate != nil {
		t.Fatalf("CreateBillingModelPrice(expr) error = %v", errCreate)
	}
	// p / c fails at runtime when c is 0 and must fall back to the columns.
	if _, errCreate := repo.CreateBillingModelPrice(ctx, BillingModelPriceUpdate{Provider: "claude", Model: "claude-fallback", InputPricePerMillion: 2, Enabled: true, PricingExpr: "p / c"}); errCreate != nil {
		t.Fatalf("CreateBillingModelPrice(fallback) error = %v", errCreate)
	}
	payloads := map[string]string{
		"req-long":     `{"timestamp":"2026-06-10T01:02:03Z","provider":"claude","executor_type":"ClaudeExecutor","model":"claude-opus-5","api_key":"expr-key","request_id":"req-long","tokens":{"input_tokens":10000,"output_tokens":1000,"cache_read_tokens":240000,"cache_read_tokens_present":true,"cache_creation_tokens":5000,"total_tokens":256000}}`,
		"req-short":    `{"timestamp":"2026-06-10T01:02:04Z","provider":"claude","executor_type":"ClaudeExecutor","model":"claude-opus-5","api_key":"expr-key","request_id":"req-short","tokens":{"input_tokens":10000,"output_tokens":1000,"total_tokens":11000}}`,
		"req-fallback": `{"timestamp":"2026-06-10T01:02:05Z","provider":"claude","executor_type":"ClaudeExecutor","model":"claude-fallback","api_key":"expr-key","request_id":"req-fallback","tokens":{"input_tokens":1000000,"output_tokens":0,"total_tokens":1000000}}`,
	}
	for name, payload := range payloads {
		if _, errUsage := repo.AppendUsage(ctx, payload, "192.0.2.10"); errUsage != nil {
			t.Fatalf("AppendUsage(%s) error = %v", name, errUsage)
		}
	}
	charges, errCharges := repo.ListBillingCharges(ctx, BillingChargeQuery{UserID: &user.ID, Limit: 10})
	if errCharges != nil || len(charges.Records) != 3 {
		t.Fatalf("ListBillingCharges() = %+v err = %v", charges, errCharges)
	}
	want := map[string]float64{
		"req-long":     billingRoundAmount((15000*6 + 1000*22.5 + 240000*0.6) / 1e6), // cache write folds into p
		"req-short":    billingRoundAmount((10000*3 + 1000*15) / 1e6),
		"req-fallback": 2,
	}
	for _, charge := range charges.Records {
		if math.Abs(charge.Amount-want[charge.RequestID]) > 1e-12 {
			t.Fatalf("%s amount = %.10f, want %.10f", charge.RequestID, charge.Amount, want[charge.RequestID])
		}
		var cost BillingCostBreakdown
		if errUnmarshal := json.Unmarshal(charge.CostBreakdown, &cost); errUnmarshal != nil {
			t.Fatalf("%s cost_breakdown error = %v", charge.RequestID, errUnmarshal)
		}
		switch charge.RequestID {
		case "req-fallback":
			if charge.CostDiagnostic != BillingCostDiagnosticPricingExprError || !strings.Contains(cost.ExpressionError, "division by zero") || cost.PricingExpr != "" {
				t.Fatalf("fallback charge diag %q cost %+v", charge.CostDiagnostic, cost)
			}
		default:
			if charge.CostDiagnostic != "" || cost.PricingExpr != expression || cost.Items["expr:p"].Tokens == 0 {
				t.Fatalf("%s diag %q cost %+v", charge.RequestID, charge.CostDiagnostic, cost)
			}
		}
	}
}
