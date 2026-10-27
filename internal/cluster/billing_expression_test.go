package cluster

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"testing"
)

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
