package cluster

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func TestBillingCPAStructuredUsageDetails(t *testing.T) {
	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	user := newBillingCaseUser(t, repo, "cpa-structured")

	cw30m := 4.0
	cw12h := 8.0
	wsPrice := 0.01
	_, errPrice := repo.CreateBillingModelPrice(ctx, BillingModelPriceUpdate{
		Provider:                  "openai",
		Model:                     "gpt-5.6-codex",
		InputPricePerMillion:      2,
		OutputPricePerMillion:     10,
		CacheWritePricePerMillion: 2.5,
		Extra: BillingExtraPrices{
			CacheWrite30mPricePerMillion: &cw30m,
			CacheWrite12hPricePerMillion: &cw12h,
			WebSearchPricePerCall:        &wsPrice,
		},
		Enabled: true,
	})
	if errPrice != nil {
		t.Fatal(errPrice)
	}
	_, errImgPrice := repo.CreateBillingModelPrice(ctx, BillingModelPriceUpdate{
		Provider:    "openai",
		Model:       "gpt-image-2",
		Enabled:     true,
		PricingExpr: `(geo == "us" ? 1.1 : 1) * (p * 5 + imgn * usd(img_quality == "high" && img_size == "1024x1536" ? 0.2 : 0.05) + asec * usd(0.01))`,
	})
	if errImgPrice != nil {
		t.Fatal(errImgPrice)
	}

	// 1. OpenAI Responses explicit cache TTL ("30m") via cache_details.ttl + tool_usage_details.web_search_calls.
	payload30m := `{"timestamp":"2026-10-27T01:00:00Z","provider":"openai","model":"gpt-5.6-codex","api_key":"cpa-structured","request_id":"resp-30m","accounting_version":2,"token_breakdown":{"schema_version":2,"quality":"complete","total_tokens":1500,"input":{"total_tokens":1000,"uncached_tokens":0,"cache_read_tokens":0,"cache_write_tokens":1000},"output":{"total_tokens":500,"non_reasoning_tokens":500,"reasoning_tokens":0}},"tokens":{"input_tokens":1000,"cache_creation_tokens":1000,"output_tokens":500,"total_tokens":1500},"cache_details":{"mode":"explicit","ttl":"30m"},"tool_usage_details":{"web_search_calls":2}}`
	if _, errUsage := repo.AppendUsage(ctx, payload30m, "192.0.2.1"); errUsage != nil {
		t.Fatal(errUsage)
	}
	// 1000 cw30m @ $4/M = 0.004, 500 out @ $10/M = 0.005, 2 ws @ $0.01 = 0.02 -> 0.029
	charge30m := billingCaseCharges(t, repo, user.ID)["resp-30m"]
	if charge30m.Amount != 0.029 {
		t.Fatalf("resp-30m amount = %v, want 0.029 (breakdown=%s)", charge30m.Amount, string(charge30m.CostBreakdown))
	}

	// 2. Image/media structured fields (requested_image_details fallback, geo, media_counts.audio_seconds).
	payloadImg := `{"timestamp":"2026-10-27T01:01:00Z","provider":"openai","model":"gpt-image-2","api_key":"cpa-structured","request_id":"img-structured","inference_geo":"us","image_count":2,"requested_image_details":{"size":"1024x1536","quality":"high"},"media_counts":{"audio_seconds":3},"accounting_version":2,"token_breakdown":{"schema_version":2,"quality":"complete","total_tokens":1000,"input":{"total_tokens":1000,"uncached_tokens":1000,"cache_read_tokens":0,"cache_write_tokens":0},"output":{"total_tokens":0,"non_reasoning_tokens":0,"reasoning_tokens":0}},"tokens":{"input_tokens":1000,"total_tokens":1000}}`
	if _, errUsage := repo.AppendUsage(ctx, payloadImg, "192.0.2.1"); errUsage != nil {
		t.Fatal(errUsage)
	}
	// 1.1 * (1000 * 5/1M [0.005] + 2 * 0.2 [0.4] + 3 * 0.01 [0.03]) = 1.1 * 0.435 = 0.4785
	chargeImg := billingCaseCharges(t, repo, user.ID)["img-structured"]
	if chargeImg.Amount != 0.4785 {
		t.Fatalf("img-structured amount = %v, want 0.4785 (breakdown=%s)", chargeImg.Amount, string(chargeImg.CostBreakdown))
	}

	// 3. xAI server-side tools (xs, ce, fs, ds), cost_in_usd_ticks, and response_model fallback.
	_, errXAIPrice := repo.CreateBillingModelPrice(ctx, BillingModelPriceUpdate{
		Provider:    "xai",
		Model:       "grok-4-0709",
		Enabled:     true,
		PricingExpr: `usd(upcost) + xs * usd(0.005) + ce * usd(0.005) + fs * usd(0.0025) + ds * usd(0.005)`,
	})
	if errXAIPrice != nil {
		t.Fatal(errXAIPrice)
	}
	payloadXAI := `{"timestamp":"2026-10-27T01:02:00Z","provider":"xai","model":"grok-4-latest","response_model":"grok-4-0709","api_key":"cpa-structured","request_id":"xai-tools","cost_details":{"cost_in_usd_ticks":150000000},"tool_usage_details":{"x_search_calls":2,"code_execution_calls":1,"file_search_calls":2,"document_search_calls":1},"tokens":{"input_tokens":100,"output_tokens":50,"total_tokens":150}}`
	if _, errUsage := repo.AppendUsage(ctx, payloadXAI, "192.0.2.1"); errUsage != nil {
		t.Fatal(errUsage)
	}
	// upcost = 150000000 / 1e10 = 0.015; tools = 2*0.005 + 1*0.005 + 2*0.0025 + 1*0.005 = 0.025 -> total 0.04
	chargeXAI := billingCaseCharges(t, repo, user.ID)["xai-tools"]
	if chargeXAI.Amount != 0.04 || chargeXAI.ActualModel != "grok-4-0709" {
		t.Fatalf("xai-tools amount = %v actual_model = %q, want 0.04 / grok-4-0709 (breakdown=%s)", chargeXAI.Amount, chargeXAI.ActualModel, string(chargeXAI.CostBreakdown))
	}

	// 4. Anthropic iterations[] with advisor_message sub-model (billed at advisor model rate without double-counting message iteration).
	_, errSonnet := repo.CreateBillingModelPrice(ctx, BillingModelPriceUpdate{
		Provider:              "claude",
		Model:                 "claude-sonnet-4-6",
		InputPricePerMillion:  3,
		OutputPricePerMillion: 15,
		Enabled:               true,
	})
	if errSonnet != nil {
		t.Fatal(errSonnet)
	}
	_, errOpus := repo.CreateBillingModelPrice(ctx, BillingModelPriceUpdate{
		Provider:              "claude",
		Model:                 "claude-opus-4-7",
		InputPricePerMillion:  15,
		OutputPricePerMillion: 75,
		Enabled:               true,
	})
	if errOpus != nil {
		t.Fatal(errOpus)
	}
	payloadAdvisor := `{"timestamp":"2026-10-27T01:03:00Z","provider":"claude","model":"claude-sonnet-4-6","api_key":"cpa-structured","request_id":"claude-advisor","accounting_version":2,"token_breakdown":{"schema_version":2,"quality":"complete","total_tokens":2000,"input":{"total_tokens":1000,"uncached_tokens":1000,"cache_read_tokens":0,"cache_write_tokens":0},"output":{"total_tokens":1000,"non_reasoning_tokens":1000,"reasoning_tokens":0}},"tokens":{"input_tokens":1000,"output_tokens":1000,"total_tokens":2000},"iterations":[{"type":"message","model":"claude-sonnet-4-6","input_tokens":1000,"output_tokens":1000},{"type":"advisor_message","model":"claude-opus-4-7","input_tokens":2000,"output_tokens":400}]}`
	if _, errUsage := repo.AppendUsage(ctx, payloadAdvisor, "192.0.2.1"); errUsage != nil {
		t.Fatal(errUsage)
	}
	// Sonnet: 1000*3/1M + 1000*15/1M = 0.018; Opus advisor: 2000*15/1M + 400*75/1M = 0.03 + 0.03 = 0.06 -> total 0.078
	chargeAdvisor := billingCaseCharges(t, repo, user.ID)["claude-advisor"]
	if chargeAdvisor.Amount != 0.078 {
		t.Fatalf("claude-advisor amount = %v, want 0.078 (breakdown=%s)", chargeAdvisor.Amount, string(chargeAdvisor.CostBreakdown))
	}

	// 5. new-api / sub2api / LiteLLM / Gemini expression features: v1:, tier(), fixed(), image_count, img_tier, img_cr, maps, u().
	_, errGeminiImg := repo.CreateBillingModelPrice(ctx, BillingModelPriceUpdate{
		Provider:    "gemini",
		Model:       "gemini-3-pro-image",
		Enabled:     true,
		PricingExpr: `v1:(img_tier == "4k" ? tier("4k", fixed(0.24)) : tier("2k", fixed(0.12))) * image_count + maps * usd(0.025) + u("tool_usage_details.extra.custom_units") * usd(0.01)`,
	})
	if errGeminiImg != nil {
		t.Fatal(errGeminiImg)
	}
	payloadGeminiImg := `{"timestamp":"2026-10-27T01:04:00Z","provider":"gemini","model":"gemini-3-pro-image","api_key":"cpa-structured","request_id":"gemini-img-4k","image_count":2,"requested_image_details":{"size":"3840x2160"},"tool_usage_details":{"extra":{"google_maps_chunks":1,"custom_units":3}},"tokens":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}`
	if _, errUsage := repo.AppendUsage(ctx, payloadGeminiImg, "192.0.2.1"); errUsage != nil {
		t.Fatal(errUsage)
	}
	// 2 * 0.24 + 1 * 0.025 + 3 * 0.01 = 0.48 + 0.025 + 0.03 = 0.535, matched_tier = "4k"
	chargeGeminiImg := billingCaseCharges(t, repo, user.ID)["gemini-img-4k"]
	if chargeGeminiImg.Amount != 0.535 || !strings.Contains(string(chargeGeminiImg.CostBreakdown), `"matched_tier":"4k"`) {
		t.Fatalf("gemini-img-4k amount = %v breakdown = %s, want 0.535 / matched_tier=4k", chargeGeminiImg.Amount, string(chargeGeminiImg.CostBreakdown))
	}
}

func TestBillingV14UpgradeAndPopulatedChargeMigration(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	// 1. Verify schema migration coordinator upgrades v14 -> v15 (currentDatabaseVersion) and retains v14 snapshot models.
	if currentDatabaseVersion < 15 {
		t.Fatalf("currentDatabaseVersion = %d, want >= 15", currentDatabaseVersion)
	}
	if _, ok := databaseSnapshotModels(14); !ok {
		t.Fatal("v14 snapshot models must remain supported")
	}
	store := newFakeSchemaMigrationStore(schemaMigrationState{Version: 14, Initialized: true})
	migResult, errCoord := coordinateSchemaMigration(ctx, currentDatabaseVersion, time.Millisecond, time.Second, false, store.operations())
	if errCoord != nil {
		t.Fatalf("coordinateSchemaMigration(v14->v%d) error = %v", currentDatabaseVersion, errCoord)
	}
	if !migResult.MigrationExecutor || !migResult.MigrationCommitted || migResult.CurrentState.Version != currentDatabaseVersion {
		t.Fatalf("v14 upgrade result = %+v, want executed and committed to %d", migResult, currentDatabaseVersion)
	}

	// 2. Verify populated pre-v15 billing_charge table migrates safely on SQLite and preserves 1 for historical charges + 0 for new zero-multiplier charges.
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	db, errDB := repo.database()
	if errDB != nil {
		t.Fatal(errDB)
	}
	username := "pop-migrate@example.com"
	credits := 50.0
	user, errUser := repo.CreateUser(ctx, UserUpdate{Username: &username, Credits: &credits})
	if errUser != nil {
		t.Fatal(errUser)
	}
	keyVal := "pop-migrate-key"
	if _, errKey := repo.CreateAPIKeyForUser(ctx, user.ID, APIKeyUserUpdate{APIKey: &keyVal}); errKey != nil {
		t.Fatal(errKey)
	}
	if _, errPrice := repo.CreateBillingModelPrice(ctx, BillingModelPriceUpdate{
		Provider:             "openai",
		Model:                "gpt-pop",
		InputPricePerMillion: 10,
		Enabled:              true,
	}); errPrice != nil {
		t.Fatal(errPrice)
	}
	if _, errU1 := repo.AppendUsage(ctx, `{"timestamp":"2026-10-27T02:00:00Z","provider":"openai","model":"gpt-pop","api_key":"pop-migrate-key","request_id":"hist-1","tokens":{"input_tokens":100000,"total_tokens":100000}}`, "192.0.2.1"); errU1 != nil {
		t.Fatal(errU1)
	}
	if errReset := db.Model(&BillingChargeRecord{}).Where("request_id = ?", "hist-1").Updates(map[string]any{"base_amount": 0}).Error; errReset != nil {
		t.Fatal(errReset)
	}
	for _, col := range []string{"billing_multiplier", "base_amount", "auth_index", "cost_diagnostic", "cost_breakdown"} {
		if errDrop := db.Migrator().DropColumn(&BillingChargeRecord{}, col); errDrop != nil {
			t.Fatalf("DropColumn(billing_charge.%s) error = %v", col, errDrop)
		}
	}
	if errDrop := db.Migrator().DropColumn(&BillingModelPriceRecord{}, "pricing_expr"); errDrop != nil {
		t.Fatalf("DropColumn(billing_model_price.pricing_expr) error = %v", errDrop)
	}
	if errDrop := db.Migrator().DropColumn(&UserRecord{}, "billing_multiplier"); errDrop != nil {
		t.Fatalf("DropColumn(user.billing_multiplier) error = %v", errDrop)
	}
	if errMigrate := AutoMigrateContext(ctx, db); errMigrate != nil {
		t.Fatalf("AutoMigrateContext on populated pre-v15 tables error = %v", errMigrate)
	}
	histCharge := billingCaseCharges(t, repo, user.ID)["hist-1"]
	if histCharge.BillingMultiplier != 1 || histCharge.BaseAmount != 1 || histCharge.Amount != 1 {
		t.Fatalf("historical charge after upgrade = %+v, want multiplier=1 base=1 amount=1", histCharge)
	}

	// 3. Verify exporting/importing a v14 snapshot (with frozen legacy user, billing_model_price, and billing_charge shapes) succeeds on v15.
	snapshotPath := filepath.Join(t.TempDir(), "legacy-v14.snapshot.zip")
	if _, errExport := exportDatabaseSnapshotVersion(ctx, db, snapshotPath, 14); errExport != nil {
		t.Fatalf("exportDatabaseSnapshotVersion(v14) error = %v", errExport)
	}
	snapshot, errOpenSnap := OpenDatabaseSnapshot(ctx, snapshotPath)
	if errOpenSnap != nil {
		t.Fatalf("OpenDatabaseSnapshot(v14) error = %v", errOpenSnap)
	}
	defer func() { _ = snapshot.Close() }()

	targetDB := openDatabaseSnapshotSQLiteRawTestDB(t, filepath.Join(t.TempDir(), "target-v15.db"))
	if _, errImport := ImportDatabaseSnapshot(ctx, targetDB, snapshot, nil); errImport != nil {
		t.Fatalf("ImportDatabaseSnapshot(v14 -> v15) error = %v", errImport)
	}
	var importedCharge BillingChargeRecord
	if errFirst := targetDB.First(&importedCharge, "request_id = ?", "hist-1").Error; errFirst != nil {
		t.Fatalf("load imported v14 charge: %v", errFirst)
	}
	if importedCharge.BillingMultiplier != 1 || importedCharge.BaseAmount != 1 || importedCharge.Amount != 1 {
		t.Fatalf("imported v14 charge = %+v, want multiplier=1 base_amount=1 amount=1", importedCharge)
	}
}
