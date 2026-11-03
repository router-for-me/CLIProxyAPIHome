package cluster

import (
	"context"
	"testing"
)

func TestBillingReviewZeroMultipliers(t *testing.T) {
	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	user := newBillingCaseUser(t, repo, "zero-review")
	db, errDB := repo.database()
	if errDB != nil {
		t.Fatal(errDB)
	}
	if errUpdate := db.Model(&UserRecord{}).Where("id = ?", user.ID).Update("billing_multiplier", 0).Error; errUpdate != nil {
		t.Fatal(errUpdate)
	}
	if _, errPrice := repo.CreateBillingModelPrice(ctx, BillingModelPriceUpdate{Provider: "claude", Model: "m", InputPricePerMillion: 1, Enabled: true}); errPrice != nil {
		t.Fatal(errPrice)
	}
	payload := `{"timestamp":"2026-10-27T01:00:00Z","provider":"claude","model":"m","api_key":"zero-review","request_id":"zero-review","tokens":{"input_tokens":1000,"total_tokens":1000}}`
	if _, errUsage := repo.AppendUsage(ctx, payload, "192.0.2.1"); errUsage != nil {
		t.Fatal(errUsage)
	}
	charge := billingCaseCharges(t, repo, user.ID)["zero-review"]
	if charge.Amount != 0 || charge.BillingMultiplier != 0 {
		t.Fatalf("charge = %+v", charge)
	}
	cost := BillingCostBreakdown{Total: 1, ReasoningEffort: "high", ReasoningEffortMultiplier: 0}
	cost.finish(0)
	if cost.Total != 0 {
		t.Fatalf("zero effort multiplier charged %v", cost.Total)
	}
}

func TestBillingReviewFailedExpressionUnitFees(t *testing.T) {
	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	user := newBillingCaseUser(t, repo, "expr-review")
	_, errPrice := repo.CreateBillingModelPrice(ctx, BillingModelPriceUpdate{Provider: "claude", Model: "m", Enabled: true, PricingExpr: "p + req * usd(1) + ws * usd(1) + imgn * usd(1)"})
	if errPrice != nil {
		t.Fatal(errPrice)
	}
	payload := `{"timestamp":"2026-10-27T01:00:00Z","provider":"claude","model":"m","api_key":"expr-review","request_id":"f","failed":true,"web_search_requests":2,"image_count":2,"tokens":{"input_tokens":100,"total_tokens":100}}`
	if _, errUsage := repo.AppendUsage(ctx, payload, "192.0.2.1"); errUsage != nil {
		t.Fatal(errUsage)
	}
	charge := billingCaseCharges(t, repo, user.ID)["f"]
	if charge.Amount != 0.0001 {
		t.Fatalf("failed expression charged %v", charge.Amount)
	}
}
