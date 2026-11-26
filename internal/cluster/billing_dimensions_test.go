package cluster

import (
	"context"
	"math"
	"testing"
)

func billingDimensionsTestUsage(tokensJSON string) *UsageRecord {
	// uncached 1000 (+ cache read 100 + cache write 300) input, 200 output of which 50 reasoning.
	return &UsageRecord{
		Provider:                   "claude",
		TokenAccountingVersion:     UsageTokenAccountingSchemaVersion,
		TokenAccountingQuality:     UsageTokenAccountingQualityComplete,
		AccountingTotalTokens:      1600,
		AccountingInputTokens:      1400,
		UncachedInputTokens:        1000,
		AccountingCacheReadTokens:  100,
		AccountingCacheWriteTokens: 300,
		AccountingOutputTokens:     200,
		NonReasoningOutputTokens:   150,
		AccountingReasoningTokens:  50,
		TokensJSON:                 JSONB(tokensJSON),
	}
}

func TestBillingCachedMediaNotChargedAsUncachedInput(t *testing.T) {
	t.Parallel()
	imgPrice := 20.0
	audioPrice := 30.0
	snapshot := BillingPriceSnapshot{
		InputPricePerMillion:     1,
		OutputPricePerMillion:    10,
		CacheReadPricePerMillion: 0.1,
		BillingExtraPrices: BillingExtraPrices{
			ImageInputPricePerMillion: &imgPrice,
			AudioInputPricePerMillion: &audioPrice,
		},
	}
	// 1. Explicit cached_image_tokens / cached_audio_tokens are subtracted from total prompt image/audio tokens.
	explicitUsage := &UsageRecord{
		Provider:                  "gemini",
		TokenAccountingVersion:    UsageTokenAccountingSchemaVersion,
		TokenAccountingQuality:    UsageTokenAccountingQualityComplete,
		AccountingTotalTokens:     1000,
		AccountingInputTokens:     1000,
		UncachedInputTokens:       600,
		AccountingCacheReadTokens: 400,
		TokensJSON:                JSONB(`{"image_input_tokens":150,"cached_image_tokens":100,"audio_input_tokens":80,"cached_audio_tokens":80}`),
	}
	explicitCost := billingChargeCost(explicitUsage, snapshot)
	if explicitCost.Items["image_input"].Tokens != 50 || explicitCost.Items["input"].Tokens != 550 {
		t.Fatalf("explicit cached media items = %+v", explicitCost.Items)
	}
	// 2. Real chat.completion.chunk shape (77dcea63.log): prompt=196409, cached=196096 (uncached=313), total prompt image_tokens=5141.
	chatCachedImage := &UsageRecord{
		Provider:                  "openai",
		TokenAccountingVersion:    UsageTokenAccountingSchemaVersion,
		TokenAccountingQuality:    UsageTokenAccountingQualityComplete,
		AccountingTotalTokens:     196752,
		AccountingInputTokens:     196409,
		UncachedInputTokens:       313,
		AccountingCacheReadTokens: 196096,
		AccountingOutputTokens:    343,
		NonReasoningOutputTokens:  342,
		AccountingReasoningTokens: 1,
		TokensJSON:                JSONB(`{"input_tokens":196409,"cached_tokens":196096,"cache_read_tokens":196096,"cache_read_tokens_present":true,"image_input_tokens":5141,"output_tokens":343,"reasoning_tokens":1,"total_tokens":196752}`),
	}
	chatCost := billingChargeCost(chatCachedImage, snapshot)
	if _, hasImg := chatCost.Items["image_input"]; hasImg || chatCost.Items["input"].Tokens != 313 {
		t.Fatalf("cached prompt image leaked into uncached input: %+v", chatCost.Items)
	}
}

func TestBillingAdvisorCacheTTLCharges(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	user := newBillingCaseUser(t, repo, "advisor-cache")
	ttlPrice := 20.0
	for _, rule := range []BillingModelPriceUpdate{
		{Provider: "claude", Model: "parent", InputPricePerMillion: 1, Enabled: true},
		{Provider: "claude", Model: "advisor", CacheWritePricePerMillion: 10, Extra: BillingExtraPrices{CacheWrite1hPricePerMillion: &ttlPrice}, Enabled: true},
		{Provider: "claude", Model: "advisor-expr", PricingExpr: "cc * 10 + cc1h * 20", Enabled: true},
	} {
		if _, errCreate := repo.CreateBillingModelPrice(ctx, rule); errCreate != nil {
			t.Fatal(errCreate)
		}
	}
	for _, model := range []string{"advisor", "advisor-expr"} {
		payload := `{"timestamp":"2026-10-27T01:00:00Z","provider":"claude","model":"parent","api_key":"advisor-cache","request_id":"` + model + `","tokens":{"input_tokens":1000,"total_tokens":1000},"iterations":[{"type":"advisor_message","model":"` + model + `","input_tokens":0,"output_tokens":0,"cache_creation_input_tokens":1500,"cache_creation":{"ephemeral_5m_input_tokens":500,"ephemeral_1h_input_tokens":1000}}]}`
		if _, errUsage := repo.AppendUsage(ctx, payload, "192.0.2.1"); errUsage != nil {
			t.Fatal(errUsage)
		}
		charge, ok := billingCaseCharges(t, repo, user.ID)[model]
		if !ok || charge.Amount != 0.026 || math.Abs(charge.BalanceBefore-charge.BalanceAfter-0.026) > 1e-10 {
			t.Errorf("%s: found=%v amount=%v balance=%v -> %v, want delta 0.026; breakdown=%s", model, ok, charge.Amount, charge.BalanceBefore, charge.BalanceAfter, charge.CostBreakdown)
		}
	}
}

func TestBillingExtraDimensionsFallBackToParentPrices(t *testing.T) {
	t.Parallel()

	snapshot := BillingPriceSnapshot{InputPricePerMillion: 10, OutputPricePerMillion: 20, CacheReadPricePerMillion: 1, CacheWritePricePerMillion: 12.5}
	usage := billingDimensionsTestUsage(`{"cache_creation_1h_tokens":100,"image_input_tokens":400,"image_output_tokens":30}`)
	cost := billingChargeCost(usage, snapshot)
	// Unset detail prices bill exactly like the parent buckets.
	want := (1000*10 + 100*1 + 300*12.5 + 200*20) / 1e6
	if math.Abs(cost.Total-billingRoundAmount(want)) > 1e-12 {
		t.Fatalf("total = %.10f, want %.10f (%+v)", cost.Total, want, cost.Items)
	}
	if cost.Items["cache_write_1h"].Tokens != 100 || cost.Items["cache_write"].Tokens != 200 || cost.Items["image_input"].Tokens != 400 || cost.Items["input"].Tokens != 600 {
		t.Fatalf("items = %+v", cost.Items)
	}
	if _, ok := cost.Items["reasoning"]; ok || cost.Items["output"].Tokens != 170 || cost.Items["image_output"].Tokens != 30 {
		t.Fatalf("output items = %+v, want reasoning folded into output", cost.Items)
	}
}

func TestBillingExtraDimensionsUseConfiguredPrices(t *testing.T) {
	t.Parallel()

	snapshot := BillingPriceSnapshot{InputPricePerMillion: 10, OutputPricePerMillion: 20, CacheReadPricePerMillion: 1, CacheWritePricePerMillion: 12.5}
	for name, value := range map[string]float64{
		BillingPriceCacheWrite1h:  20,
		BillingPriceCacheWrite12h: 30,
		BillingPriceReasoning:     40,
		BillingPriceImageInput:    5,
		BillingPriceImageOutput:   0,
		BillingPriceAudioInput:    7,
		BillingPriceAudioOutput:   9,
	} {
		price := value
		if errSet := snapshot.Set(name, &price); errSet != nil {
			t.Fatalf("Set(%s) error = %v", name, errSet)
		}
	}
	// Detail counts larger than their parent bucket are clamped.
	usage := billingDimensionsTestUsage(`{"cache_creation_1h_tokens":100,"cache_creation_12h_tokens":500,"image_input_tokens":400,"audio_input_tokens":100,"image_output_tokens":30,"audio_output_tokens":20}`)
	cost := billingChargeCost(usage, snapshot)
	want := (500*10 + 400*5 + 100*7 + 100*1 + 0*12.5 + 100*20 + 200*30 + 100*20 + 50*40 + 30*0 + 20*9) / 1e6
	if math.Abs(cost.Total-billingRoundAmount(want)) > 1e-12 {
		t.Fatalf("total = %.10f, want %.10f (%+v)", cost.Total, want, cost.Items)
	}
	if cost.Items["cache_write_12h"].Tokens != 200 || cost.Items["reasoning"].Tokens != 50 || cost.Items["output"].Tokens != 100 {
		t.Fatalf("items = %+v", cost.Items)
	}
}

func TestBillingModelPriceExtraPricesRoundTrip(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	reasoning := 15.0
	update := BillingModelPriceUpdate{Provider: "openai", Model: "o9", InputPricePerMillion: 1, Enabled: true}
	if errSet := update.Extra.Set(BillingPriceReasoning, &reasoning); errSet != nil {
		t.Fatalf("Set() error = %v", errSet)
	}
	record, errCreate := repo.CreateBillingModelPrice(ctx, update)
	if errCreate != nil {
		t.Fatalf("CreateBillingModelPrice() error = %v", errCreate)
	}
	if record.ReasoningPricePerMillion == nil || *record.ReasoningPricePerMillion != 15 || record.ImageInputPricePerMillion != nil {
		t.Fatalf("created extras = %+v", record.BillingExtraPrices)
	}
	image := 2.0
	updated, errUpdate := repo.UpdateBillingModelPrice(ctx, record.ID, BillingModelPricePatch{Extra: map[string]*float64{BillingPriceReasoning: nil, BillingPriceImageInput: &image}})
	if errUpdate != nil {
		t.Fatalf("UpdateBillingModelPrice() error = %v", errUpdate)
	}
	if updated.ReasoningPricePerMillion != nil || updated.ImageInputPricePerMillion == nil || *updated.ImageInputPricePerMillion != 2 {
		t.Fatalf("patched extras = %+v", updated.BillingExtraPrices)
	}
	negative := -1.0
	if _, errInvalid := repo.UpdateBillingModelPrice(ctx, record.ID, BillingModelPricePatch{Extra: map[string]*float64{BillingPriceAudioInput: &negative}}); errInvalid == nil {
		t.Fatal("negative extra price was accepted")
	}
	if _, errUnknown := repo.UpdateBillingModelPrice(ctx, record.ID, BillingModelPricePatch{Extra: map[string]*float64{"bogus": &image}}); errUnknown == nil {
		t.Fatal("unknown extra price was accepted")
	}
}

func TestBillingPerUnitSurcharges(t *testing.T) {
	t.Parallel()

	webSearch, image := 0.01, 0.04
	snapshot := BillingPriceSnapshot{InputPricePerMillion: 10, RequestPrice: 0.5}
	snapshot.WebSearchPricePerCall = &webSearch
	snapshot.ImagePricePerImage = &image
	snapshot.ReasoningEffortMultipliers = map[string]float64{"high": 2}
	usage := &UsageRecord{Provider: "openai", InputTokens: 1000000, TotalTokens: 1000000, Effort: "high",
		TokensJSON: JSONB(`{"input_tokens":1000000,"total_tokens":1000000,"web_search_requests":3}`), PayloadJSON: JSONB(`{"image_count":2}`)}
	cost := billingChargeCost(usage, snapshot)
	// Surcharges and the request price are not scaled by the effort multiplier.
	if want := billingRoundAmount(10*2 + 0.5 + 3*0.01 + 2*0.04); cost.Total != want {
		t.Fatalf("total = %v, want %v (%+v)", cost.Total, want, cost)
	}
	if cost.Items["web_search_calls"].Tokens != 3 || cost.Items["images"].Tokens != 2 {
		t.Fatalf("items = %+v", cost.Items)
	}

	// An expression that prices ws itself is not charged twice; ignored imgn still bills.
	snapshot.PricingExpr = "p * 10 + ws * usd(0.02)"
	exprCost := billingChargeCost(usage, snapshot)
	if want := billingRoundAmount(10*2 + 3*0.02*2 + 2*0.04); exprCost.Total != want {
		t.Fatalf("expression total = %v, want %v (%+v)", exprCost.Total, want, exprCost)
	}
	if _, ok := exprCost.Items["web_search_calls"]; ok {
		t.Fatalf("expression charged web search twice: %+v", exprCost.Items)
	}
}
