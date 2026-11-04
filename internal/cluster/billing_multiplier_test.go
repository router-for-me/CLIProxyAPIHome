package cluster

import (
	"context"
	"encoding/json"
	"testing"
)

func TestBillingChargeAppliesEffortAndUserMultipliers(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	username := "multiplier@example.com"
	credits := 100.0
	multiplier := 0.8
	user, errCreateUser := repo.CreateUser(ctx, UserUpdate{Username: &username, Credits: &credits, BillingMultiplier: OptionalFloatUpdate{Set: true, Value: multiplier}})
	if errCreateUser != nil {
		t.Fatalf("CreateUser() error = %v", errCreateUser)
	}
	if user.BillingMultiplier == nil || *user.BillingMultiplier != 0.8 {
		t.Fatalf("billing_multiplier = %v, want 0.8", user.BillingMultiplier)
	}
	clientKey := "multiplier-key"
	if _, errCreateKey := repo.CreateAPIKeyForUser(ctx, user.ID, APIKeyUserUpdate{APIKey: &clientKey}); errCreateKey != nil {
		t.Fatalf("CreateAPIKeyForUser() error = %v", errCreateKey)
	}
	if _, errCreate := repo.CreateBillingModelPrice(ctx, BillingModelPriceUpdate{
		Provider: "codex", Model: "gpt-5.5", InputPricePerMillion: 10, RequestPrice: 1, Enabled: true,
		ReasoningEffortMultipliers: map[string]float64{"XHigh": 1.5},
	}); errCreate != nil {
		t.Fatalf("CreateBillingModelPrice() error = %v", errCreate)
	}
	for _, tc := range []struct{ effort, request string }{{"xhigh", "req-xhigh"}, {"low", "req-low"}} {
		payload := `{"timestamp":"2026-06-10T01:02:03Z","provider":"codex","model":"gpt-5.5","auth_index":"cred-1","reasoning_effort":"` + tc.effort + `","api_key":"multiplier-key","request_id":"` + tc.request + `","tokens":{"input_tokens":1000000,"total_tokens":1000000}}`
		if _, errUsage := repo.AppendUsage(ctx, payload, "192.0.2.10"); errUsage != nil {
			t.Fatalf("AppendUsage(%s) error = %v", tc.effort, errUsage)
		}
	}
	charges, errCharges := repo.ListBillingCharges(ctx, BillingChargeQuery{AuthIndex: "cred-1", Limit: 10})
	if errCharges != nil || len(charges.Records) != 2 {
		t.Fatalf("ListBillingCharges(auth_index) = %+v err = %v", charges, errCharges)
	}
	want := map[string][2]float64{
		"req-xhigh": {16, 12.8}, // (10 * 1.5 + 1) * 0.8
		"req-low":   {11, 8.8},  // (10 + 1) * 0.8
	}
	for _, charge := range charges.Records {
		expected := want[charge.RequestID]
		if charge.BaseAmount != expected[0] || charge.Amount != expected[1] || charge.BillingMultiplier != 0.8 || charge.AuthIndex != "cred-1" {
			t.Fatalf("%s base/amount/multiplier/auth = %v/%v/%v/%q, want %v/%v/0.8/cred-1", charge.RequestID, charge.BaseAmount, charge.Amount, charge.BillingMultiplier, charge.AuthIndex, expected[0], expected[1])
		}
		var cost BillingCostBreakdown
		if errUnmarshal := json.Unmarshal(charge.CostBreakdown, &cost); errUnmarshal != nil || cost.Total != charge.Amount || cost.BaseTotal != charge.BaseAmount {
			t.Fatalf("%s cost_breakdown = %s err = %v", charge.RequestID, charge.CostBreakdown, errUnmarshal)
		}
	}
	if none, errNone := repo.ListBillingCharges(ctx, BillingChargeQuery{AuthIndex: "cred-2", Limit: 10}); errNone != nil || none.Total != 0 {
		t.Fatalf("auth_index filter = %+v err = %v", none, errNone)
	}

	if _, errInvalid := repo.UpdateUser(ctx, user.ID, UserUpdate{BillingMultiplier: OptionalFloatUpdate{Set: true, Value: -1}}); errInvalid == nil {
		t.Fatal("negative billing_multiplier was accepted")
	}
	if _, errInvalid := repo.CreateBillingModelPrice(ctx, BillingModelPriceUpdate{Provider: "codex", Model: "bad", Enabled: true, ReasoningEffortMultipliers: map[string]float64{"high": -2}}); errInvalid == nil {
		t.Fatal("negative effort multiplier was accepted")
	}
}
