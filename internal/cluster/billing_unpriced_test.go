package cluster

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestDispatchRejectsUnpricedModelForUserKeyByDefault(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	username := "unpriced@example.com"
	unlimited := true
	user, errCreateUser := repo.CreateUser(ctx, UserUpdate{Username: &username, CreditsUnlimited: &unlimited})
	if errCreateUser != nil {
		t.Fatalf("CreateUser() error = %v", errCreateUser)
	}
	userKey := "user-key"
	if _, errCreateKey := repo.CreateAPIKeyForUser(ctx, user.ID, APIKeyUserUpdate{APIKey: &userKey}); errCreateKey != nil {
		t.Fatalf("CreateAPIKeyForUser() error = %v", errCreateKey)
	}
	if _, errCreateKey := repo.CreateAPIKey(ctx, APIKeyEntryUpdate{APIKey: "admin-key"}); errCreateKey != nil {
		t.Fatalf("CreateAPIKey() error = %v", errCreateKey)
	}

	_, _, errAllowed := repo.AllowedDispatchIDsForAPIKeyModel(ctx, userKey, "gpt-new")
	if errAllowed == nil || !strings.Contains(errAllowed.Error(), "model_not_found") || !strings.Contains(errAllowed.Error(), "no configured price") {
		t.Fatalf("unpriced dispatch error = %v, want model price not configured", errAllowed)
	}
	if _, _, errAdmin := repo.AllowedDispatchIDsForAPIKeyModel(ctx, "admin-key", "gpt-new"); errAdmin != nil {
		t.Fatalf("admin key dispatch error = %v, want no billing gate", errAdmin)
	}

	// An explicit zero price is a configured price.
	if _, errCreate := repo.CreateBillingModelPrice(ctx, BillingModelPriceUpdate{Provider: "openai", Model: "GPT-New", Enabled: true}); errCreate != nil {
		t.Fatalf("CreateBillingModelPrice() error = %v", errCreate)
	}
	if _, _, errPriced := repo.AllowedDispatchIDsForAPIKeyModel(ctx, userKey, "gpt-new"); errPriced != nil {
		t.Fatalf("zero-priced dispatch error = %v, want allowed", errPriced)
	}
	if _, _, errPrefixed := repo.AllowedDispatchIDsForAPIKeyModel(ctx, userKey, "aistudio/gpt-new"); errPrefixed != nil {
		t.Fatalf("prefixed model dispatch error = %v, want allowed via bare model price", errPrefixed)
	}

	// Disabled rules do not count as configured.
	if _, _, errDisabled := repo.AllowedDispatchIDsForAPIKeyModel(ctx, userKey, "gpt-other"); errDisabled == nil {
		t.Fatal("dispatch for another unpriced model succeeded")
	}
	allow := true
	if _, errSettings := repo.UpdateBillingSettings(ctx, BillingSettingsPatch{AllowUnpricedModels: &allow}); errSettings != nil {
		t.Fatalf("UpdateBillingSettings() error = %v", errSettings)
	}
	if _, _, errAllowedNow := repo.AllowedDispatchIDsForAPIKeyModel(ctx, userKey, "gpt-other"); errAllowedNow != nil {
		t.Fatalf("dispatch with allow_unpriced_models error = %v", errAllowedNow)
	}
}

func TestBillingChargeRecordsCostDiagnostics(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	username := "diag@example.com"
	credits := 10.0
	user, errCreateUser := repo.CreateUser(ctx, UserUpdate{Username: &username, Credits: &credits})
	if errCreateUser != nil {
		t.Fatalf("CreateUser() error = %v", errCreateUser)
	}
	clientKey := "diag-key"
	if _, errCreateKey := repo.CreateAPIKeyForUser(ctx, user.ID, APIKeyUserUpdate{APIKey: &clientKey}); errCreateKey != nil {
		t.Fatalf("CreateAPIKeyForUser() error = %v", errCreateKey)
	}
	if _, errCreate := repo.CreateBillingModelPrice(ctx, BillingModelPriceUpdate{Provider: "openai", Model: "free-model", Enabled: true}); errCreate != nil {
		t.Fatalf("CreateBillingModelPrice(free) error = %v", errCreate)
	}
	if _, errCreate := repo.CreateBillingModelPrice(ctx, BillingModelPriceUpdate{Provider: "openai", Model: "paid-model", InputPricePerMillion: 1, Enabled: true}); errCreate != nil {
		t.Fatalf("CreateBillingModelPrice(paid) error = %v", errCreate)
	}
	for index, model := range []string{"free-model", "unpriced-model", "paid-model"} {
		payload := `{"timestamp":"2026-06-10T01:02:0` + string(rune('0'+index)) + `Z","provider":"openai","model":"` + model + `","api_key":"diag-key","request_id":"req-` + model + `","tokens":{"input_tokens":1000,"output_tokens":10,"total_tokens":1010}}`
		if _, errUsage := repo.AppendUsage(ctx, payload, "192.0.2.10"); errUsage != nil {
			t.Fatalf("AppendUsage(%s) error = %v", model, errUsage)
		}
	}

	want := map[string]string{"free-model": BillingCostDiagnosticZeroPrice, "unpriced-model": BillingCostDiagnosticUnpricedModel, "paid-model": ""}
	all, errList := repo.ListBillingCharges(ctx, BillingChargeQuery{UserID: &user.ID, Limit: 10})
	if errList != nil {
		t.Fatalf("ListBillingCharges() error = %v", errList)
	}
	if len(all.Records) != 3 {
		t.Fatalf("charge count = %d, want 3", len(all.Records))
	}
	for _, record := range all.Records {
		if record.CostDiagnostic != want[record.Model] {
			t.Fatalf("%s cost_diagnostic = %q, want %q", record.Model, record.CostDiagnostic, want[record.Model])
		}
		if want[record.Model] != "" && !strings.Contains(string(record.PriceSnapshot), want[record.Model]) {
			t.Fatalf("%s snapshot = %s, want diagnostic", record.Model, record.PriceSnapshot)
		}
	}
	flagged, errFlagged := repo.ListBillingCharges(ctx, BillingChargeQuery{CostDiagnostic: "any", Limit: 10})
	if errFlagged != nil || flagged.Total != 2 {
		t.Fatalf("diagnostic filter total = %d err = %v, want 2", flagged.Total, errFlagged)
	}
	unpriced, errUnpriced := repo.ListBillingCharges(ctx, BillingChargeQuery{CostDiagnostic: BillingCostDiagnosticUnpricedModel, Limit: 10})
	if errUnpriced != nil || unpriced.Total != 1 || unpriced.Records[0].Model != "unpriced-model" {
		t.Fatalf("unpriced filter = %+v err = %v", unpriced, errUnpriced)
	}
}

func TestBillingChargeQuantizesAndItemizesCost(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	username := "itemized@example.com"
	credits := 1.0
	user, errCreateUser := repo.CreateUser(ctx, UserUpdate{Username: &username, Credits: &credits})
	if errCreateUser != nil {
		t.Fatalf("CreateUser() error = %v", errCreateUser)
	}
	clientKey := "itemized-key"
	if _, errCreateKey := repo.CreateAPIKeyForUser(ctx, user.ID, APIKeyUserUpdate{APIKey: &clientKey}); errCreateKey != nil {
		t.Fatalf("CreateAPIKeyForUser() error = %v", errCreateKey)
	}
	if _, errCreate := repo.CreateBillingModelPrice(ctx, BillingModelPriceUpdate{Provider: "codex", Model: "gpt-5.5", InputPricePerMillion: 1.25, OutputPricePerMillion: 10, CacheReadPricePerMillion: 0.125, Enabled: true}); errCreate != nil {
		t.Fatalf("CreateBillingModelPrice() error = %v", errCreate)
	}
	// 1 uncached input token at $1.25/M costs 0.00000125; 1 cache read at $0.125/M costs 0.000000125.
	payload := `{"timestamp":"2026-06-10T01:02:03Z","provider":"codex","executor_type":"CodexExecutor","model":"gpt-5.5","api_key":"itemized-key","request_id":"req-itemized","tokens":{"input_tokens":2,"output_tokens":3,"reasoning_tokens":1,"cache_read_tokens":1,"cache_read_tokens_present":true,"total_tokens":5}}`
	if _, errUsage := repo.AppendUsage(ctx, payload, "192.0.2.10"); errUsage != nil {
		t.Fatalf("AppendUsage() error = %v", errUsage)
	}
	charges, errCharges := repo.ListBillingCharges(ctx, BillingChargeQuery{UserID: &user.ID, Limit: 10})
	if errCharges != nil || len(charges.Records) != 1 {
		t.Fatalf("ListBillingCharges() = %+v err = %v", charges, errCharges)
	}
	charge := charges.Records[0]
	// Raw total 0.000031375 rounds to 8 decimals.
	if charge.Amount != 0.00003138 {
		t.Fatalf("amount = %.12f, want 0.00003138", charge.Amount)
	}
	if charge.BalanceAfter != 0.99996862 {
		t.Fatalf("balance_after = %.12f, want 0.99996862", charge.BalanceAfter)
	}
	if charge.BilledCacheReadTokens == nil || *charge.BilledCacheReadTokens != 1 || charge.BilledReasoningTokens == nil || *charge.BilledReasoningTokens != 1 || charge.BilledCacheWriteTokens == nil || *charge.BilledCacheWriteTokens != 0 {
		t.Fatalf("billed tokens = %v/%v/%v", charge.BilledCacheReadTokens, charge.BilledCacheWriteTokens, charge.BilledReasoningTokens)
	}
	var cost BillingCostBreakdown
	if errUnmarshal := json.Unmarshal(charge.CostBreakdown, &cost); errUnmarshal != nil {
		t.Fatalf("cost_breakdown = %s err = %v", charge.CostBreakdown, errUnmarshal)
	}
	if cost.Total != charge.Amount || cost.Items["input"].Tokens != 1 || cost.Items["cache_read"].Tokens != 1 || cost.Items["output"].Tokens != 3 {
		t.Fatalf("cost_breakdown = %+v", cost)
	}
}
