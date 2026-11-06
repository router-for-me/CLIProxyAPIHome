package cluster

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestBillingAutoMigrateCreatesTables(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()

	if repo == nil {
		t.Fatal("repository is nil")
	}

	db, errDB := repo.database()
	if errDB != nil {
		t.Fatalf("database() error = %v", errDB)
	}
	for _, table := range []string{
		"billing_model_price",
		"billing_balance_record",
		"billing_charge",
	} {
		if !db.Migrator().HasTable(table) {
			t.Fatalf("table %s was not migrated", table)
		}
	}
}

func TestCreateModelPriceRejectsNegativePrices(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()

	_, errCreate := repo.CreateBillingModelPrice(ctx, BillingModelPriceUpdate{
		Provider:             "openai",
		Model:                "gpt-4.1-mini",
		InputPricePerMillion: -1,
		Enabled:              true,
	})
	if errCreate == nil {
		t.Fatal("CreateBillingModelPrice() error = nil, want validation error")
	}
}

func TestCreateModelPriceRejectsDuplicateActiveRule(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()

	update := BillingModelPriceUpdate{
		Provider:              "openai",
		Model:                 "gpt-4.1-mini",
		InputPricePerMillion:  2,
		OutputPricePerMillion: 8,
		Enabled:               true,
	}
	if _, errCreateFirst := repo.CreateBillingModelPrice(ctx, update); errCreateFirst != nil {
		t.Fatalf("CreateBillingModelPrice(first) error = %v", errCreateFirst)
	}
	if _, errCreateSecond := repo.CreateBillingModelPrice(ctx, update); errCreateSecond == nil {
		t.Fatal("CreateBillingModelPrice(second) error = nil, want duplicate error")
	}
}

func TestCreateModelPriceAllowsDisabledDuplicate(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()

	disabled, errCreateDisabled := repo.CreateBillingModelPrice(ctx, BillingModelPriceUpdate{
		Provider:              "openai",
		Model:                 "gpt-4.1-mini",
		InputPricePerMillion:  2,
		OutputPricePerMillion: 8,
		Enabled:               false,
	})
	if errCreateDisabled != nil {
		t.Fatalf("CreateBillingModelPrice(disabled) error = %v", errCreateDisabled)
	}
	if disabled.Enabled {
		t.Fatal("CreateBillingModelPrice(disabled) persisted Enabled = true, want false")
	}

	db, errDB := repo.database()
	if errDB != nil {
		t.Fatalf("database() error = %v", errDB)
	}
	var stored BillingModelPriceRecord
	if errFirst := db.WithContext(ctx).First(&stored, "id = ?", disabled.ID).Error; errFirst != nil {
		t.Fatalf("find disabled billing model price: %v", errFirst)
	}
	if stored.Enabled {
		t.Fatal("stored disabled billing model price Enabled = true, want false")
	}

	enabled, errCreateEnabled := repo.CreateBillingModelPrice(ctx, BillingModelPriceUpdate{
		Provider:              "openai",
		Model:                 "gpt-4.1-mini",
		InputPricePerMillion:  2,
		OutputPricePerMillion: 8,
		Enabled:               true,
	})
	if errCreateEnabled != nil {
		t.Fatalf("CreateBillingModelPrice(enabled duplicate) error = %v", errCreateEnabled)
	}
	if !enabled.Enabled {
		t.Fatal("CreateBillingModelPrice(enabled duplicate) persisted Enabled = false, want true")
	}
}

func TestBillingModelPricePatchPreservesUnspecifiedFieldsAndWritesFalseAndZero(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()

	price, errCreate := repo.CreateBillingModelPrice(ctx, BillingModelPriceUpdate{
		Provider:                  "OpenAI",
		Model:                     "gpt-4.1-mini",
		InputPricePerMillion:      2,
		OutputPricePerMillion:     8,
		CacheReadPricePerMillion:  1,
		CacheWritePricePerMillion: 3,
		RequestPrice:              5,
		Source:                    BillingPriceSourceSync,
		Enabled:                   true,
		Note:                      "keep note",
	})
	if errCreate != nil {
		t.Fatalf("CreateBillingModelPrice() error = %v", errCreate)
	}

	updated, errUpdate := repo.UpdateBillingModelPrice(ctx, price.ID, BillingModelPricePatch{
		RequestPrice: float64Ptr(0),
		Enabled:      boolPtr(false),
	})
	if errUpdate != nil {
		t.Fatalf("UpdateBillingModelPrice() error = %v", errUpdate)
	}

	if updated.Provider != "openai" || updated.Model != "gpt-4.1-mini" {
		t.Fatalf("provider/model = %q/%q, want openai/gpt-4.1-mini", updated.Provider, updated.Model)
	}
	if updated.InputPricePerMillion != 2 ||
		updated.OutputPricePerMillion != 8 ||
		updated.CacheReadPricePerMillion != 1 ||
		updated.CacheWritePricePerMillion != 3 {
		t.Fatalf("token prices were not preserved: %#v", updated)
	}
	if updated.RequestPrice != 0 {
		t.Fatalf("request price = %v, want 0", updated.RequestPrice)
	}
	if updated.Enabled {
		t.Fatal("enabled = true, want false")
	}
	if updated.Source != BillingPriceSourceSync {
		t.Fatalf("source = %q, want %q", updated.Source, BillingPriceSourceSync)
	}
	if updated.Note != "keep note" {
		t.Fatalf("note = %q, want keep note", updated.Note)
	}
}

func TestBillingModelPriceDeleteSoftDeletesAndHidesRecord(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()

	price, errCreate := repo.CreateBillingModelPrice(ctx, BillingModelPriceUpdate{
		Provider:     "openai",
		Model:        "gpt-4.1-mini",
		RequestPrice: 1,
		Enabled:      true,
	})
	if errCreate != nil {
		t.Fatalf("CreateBillingModelPrice() error = %v", errCreate)
	}

	if errDelete := repo.DeleteBillingModelPrice(ctx, price.ID); errDelete != nil {
		t.Fatalf("DeleteBillingModelPrice() error = %v", errDelete)
	}

	records, errList := repo.ListBillingModelPrices(ctx, BillingModelPriceQuery{})
	if errList != nil {
		t.Fatalf("ListBillingModelPrices() error = %v", errList)
	}
	if len(records) != 0 {
		t.Fatalf("ListBillingModelPrices() count = %d, want 0", len(records))
	}
	if _, errGet := repo.GetBillingModelPrice(ctx, price.ID); !errors.Is(errGet, gorm.ErrRecordNotFound) {
		t.Fatalf("GetBillingModelPrice() error = %v, want record not found", errGet)
	}
	if _, errUpdate := repo.UpdateBillingModelPrice(ctx, price.ID, BillingModelPricePatch{RequestPrice: float64Ptr(2)}); !errors.Is(errUpdate, gorm.ErrRecordNotFound) {
		t.Fatalf("UpdateBillingModelPrice(deleted) error = %v, want record not found", errUpdate)
	}

	db, errDB := repo.database()
	if errDB != nil {
		t.Fatalf("database() error = %v", errDB)
	}
	var deleted BillingModelPriceRecord
	if errUnscoped := db.WithContext(ctx).Unscoped().First(&deleted, "id = ?", price.ID).Error; errUnscoped != nil {
		t.Fatalf("unscoped deleted price lookup error = %v", errUnscoped)
	}
	if deleted.DeletedAt.Valid == false {
		t.Fatal("deleted_at was not set")
	}
}

func TestBillingModelPricePatchRejectsDuplicateActiveRule(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()

	if _, errCreate := repo.CreateBillingModelPrice(ctx, BillingModelPriceUpdate{
		Provider:     "openai",
		Model:        "gpt-4.1-mini",
		RequestPrice: 1,
		Enabled:      true,
	}); errCreate != nil {
		t.Fatalf("CreateBillingModelPrice(first) error = %v", errCreate)
	}
	price, errCreate := repo.CreateBillingModelPrice(ctx, BillingModelPriceUpdate{
		Provider:     "anthropic",
		Model:        "claude-sonnet-4",
		RequestPrice: 2,
		Enabled:      true,
	})
	if errCreate != nil {
		t.Fatalf("CreateBillingModelPrice(second) error = %v", errCreate)
	}

	_, errUpdate := repo.UpdateBillingModelPrice(ctx, price.ID, BillingModelPricePatch{
		Provider: stringPtr("OpenAI"),
		Model:    stringPtr("gpt-4.1-mini"),
	})
	if !errors.Is(errUpdate, ErrBillingDuplicateModelPrice) {
		t.Fatalf("UpdateBillingModelPrice() error = %v, want duplicate model price", errUpdate)
	}
}

func TestApplyBillingBalanceRecordUpdatesCreditsAndLedger(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()

	username := "alice@example.com"
	initial := 20.0
	user, errCreateUser := repo.CreateUser(ctx, UserUpdate{Username: &username, Credits: &initial})
	if errCreateUser != nil {
		t.Fatalf("CreateUser() error = %v", errCreateUser)
	}

	record, errRecharge := repo.ApplyBillingBalanceRecord(ctx, BillingBalanceUpdate{
		UserID:   user.ID,
		Type:     BillingBalanceTypeRecharge,
		Amount:   100,
		Operator: "admin",
		Note:     "offline recharge",
	})
	if errRecharge != nil {
		t.Fatalf("ApplyBillingBalanceRecord(recharge) error = %v", errRecharge)
	}
	if record.BalanceBefore != 20 || record.BalanceAfter != 120 {
		t.Fatalf("recharge balances = %v -> %v, want 20 -> 120", record.BalanceBefore, record.BalanceAfter)
	}

	updated, errUser := repo.GetUser(ctx, user.ID)
	if errUser != nil {
		t.Fatalf("GetUser() error = %v", errUser)
	}
	if updated.Credits != 120 {
		t.Fatalf("user credits = %v, want 120", updated.Credits)
	}
}

func TestApplyBillingBalanceRecordRequiresDeductNote(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()

	username := "alice@example.com"
	user, errCreateUser := repo.CreateUser(ctx, UserUpdate{Username: &username})
	if errCreateUser != nil {
		t.Fatalf("CreateUser() error = %v", errCreateUser)
	}

	_, errDeduct := repo.ApplyBillingBalanceRecord(ctx, BillingBalanceUpdate{
		UserID:   user.ID,
		Type:     BillingBalanceTypeDeduct,
		Amount:   1,
		Operator: "admin",
	})
	if errDeduct == nil {
		t.Fatal("ApplyBillingBalanceRecord(deduct without note) error = nil, want validation error")
	}
}

func TestAppendUsageCreatesBillingChargeAndDeductsCredits(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()

	username := "alice@example.com"
	credits := 10.0
	user, errCreateUser := repo.CreateUser(ctx, UserUpdate{Username: &username, Credits: &credits})
	if errCreateUser != nil {
		t.Fatalf("CreateUser() error = %v", errCreateUser)
	}
	clientKey := "client-key"
	if _, errCreateKey := repo.CreateAPIKeyForUser(ctx, user.ID, APIKeyUserUpdate{APIKey: &clientKey}); errCreateKey != nil {
		t.Fatalf("CreateAPIKeyForUser() error = %v", errCreateKey)
	}
	if _, errCreatePrice := repo.CreateBillingModelPrice(ctx, BillingModelPriceUpdate{
		Provider:              "openai",
		Model:                 "gpt-4.1-mini",
		InputPricePerMillion:  2,
		OutputPricePerMillion: 8,
		Enabled:               true,
	}); errCreatePrice != nil {
		t.Fatalf("CreateBillingModelPrice() error = %v", errCreatePrice)
	}

	payload := `{"timestamp":"2026-06-10T01:02:03Z","provider":"openai","model":"gpt-4.1-mini","alias":"client-model","endpoint":"/v1/chat/completions","api_key":"client-key","request_id":"req-1","tokens":{"input_tokens":1000000,"output_tokens":1000000,"total_tokens":2000000}}`
	usageRecord, errUsage := repo.AppendUsage(ctx, payload, "192.0.2.10")
	if errUsage != nil {
		t.Fatalf("AppendUsage() error = %v", errUsage)
	}
	if usageRecord == nil || usageRecord.ID == 0 {
		t.Fatalf("usage record was not persisted: %#v", usageRecord)
	}

	charges, errCharges := repo.ListBillingCharges(ctx, BillingChargeQuery{UserID: &user.ID, Limit: 10})
	if errCharges != nil {
		t.Fatalf("ListBillingCharges() error = %v", errCharges)
	}
	if len(charges.Records) != 1 {
		t.Fatalf("charge count = %d, want 1", len(charges.Records))
	}
	charge := charges.Records[0]
	if charge.Amount != 10 {
		t.Fatalf("charge amount = %v, want 10", charge.Amount)
	}
	if charge.BalanceBefore != 10 || charge.BalanceAfter != 0 {
		t.Fatalf("charge balances = %v -> %v, want 10 -> 0", charge.BalanceBefore, charge.BalanceAfter)
	}
	if charge.OriginalModel != "client-model" || charge.ActualModel != "gpt-4.1-mini" {
		t.Fatalf("models = original %q actual %q, want client-model/gpt-4.1-mini", charge.OriginalModel, charge.ActualModel)
	}
}

func TestAppendUsageUnlimitedCreditsKeepsBalanceAndRecordsCharge(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()

	username := "unlimited@example.com"
	unlimited := true
	user, errCreateUser := repo.CreateUser(ctx, UserUpdate{Username: &username, CreditsUnlimited: &unlimited})
	if errCreateUser != nil {
		t.Fatalf("CreateUser() error = %v", errCreateUser)
	}
	clientKey := "client-key-unlimited"
	if _, errCreateKey := repo.CreateAPIKeyForUser(ctx, user.ID, APIKeyUserUpdate{APIKey: &clientKey}); errCreateKey != nil {
		t.Fatalf("CreateAPIKeyForUser() error = %v", errCreateKey)
	}
	if _, errCreatePrice := repo.CreateBillingModelPrice(ctx, BillingModelPriceUpdate{
		Provider:     "openai",
		Model:        "gpt-4.1-mini",
		RequestPrice: 2,
		Enabled:      true,
	}); errCreatePrice != nil {
		t.Fatalf("CreateBillingModelPrice() error = %v", errCreatePrice)
	}

	payload := `{"timestamp":"2026-06-10T01:02:03Z","provider":"openai","model":"gpt-4.1-mini","api_key":"client-key-unlimited","request_id":"req-unlimited","tokens":{"total_tokens":1}}`
	if _, errUsage := repo.AppendUsage(ctx, payload, "192.0.2.10"); errUsage != nil {
		t.Fatalf("AppendUsage() error = %v", errUsage)
	}

	charges, errCharges := repo.ListBillingCharges(ctx, BillingChargeQuery{UserID: &user.ID, Limit: 10})
	if errCharges != nil {
		t.Fatalf("ListBillingCharges() error = %v", errCharges)
	}
	if len(charges.Records) != 1 {
		t.Fatalf("charge count = %d, want 1", len(charges.Records))
	}
	charge := charges.Records[0]
	if charge.Amount != 2 {
		t.Fatalf("charge amount = %v, want 2", charge.Amount)
	}
	if charge.BalanceBefore != 0 || charge.BalanceAfter != 0 {
		t.Fatalf("charge balances = %v -> %v, want 0 -> 0", charge.BalanceBefore, charge.BalanceAfter)
	}
	updated, errUser := repo.GetUser(ctx, user.ID)
	if errUser != nil {
		t.Fatalf("GetUser() error = %v", errUser)
	}
	if updated.Credits != 0 {
		t.Fatalf("credits = %v, want unchanged 0", updated.Credits)
	}
}

func TestBillingChargeAmountNormalizesOpenAICacheBuckets(t *testing.T) {
	t.Parallel()

	usage := &UsageRecord{
		Provider:            "openai",
		InputTokens:         1000000,
		CachedTokens:        250000,
		CacheReadTokens:     250000,
		CacheCreationTokens: 100000,
	}
	snapshot := BillingPriceSnapshot{
		InputPricePerMillion:      2,
		CacheReadPricePerMillion:  0.5,
		CacheWritePricePerMillion: 3,
	}
	if amount := billingChargeAmount(usage, snapshot); amount != 1.725 {
		t.Fatalf("billingChargeAmount() = %v, want 1.725", amount)
	}

	usage.CachedTokens = 0
	if amount := billingChargeAmount(usage, snapshot); amount != 1.725 {
		t.Fatalf("billingChargeAmount() with explicit cache read = %v, want 1.725", amount)
	}
}

func TestBillingChargeAmountDoesNotDoubleChargeOpenAIResponsesCacheBuckets(t *testing.T) {
	t.Parallel()

	usage := &UsageRecord{
		Provider:               "openai",
		InputTokens:            1000000,
		CachedTokens:           500000,
		CacheReadTokens:        500000,
		CacheReadTokensPresent: true,
		CacheCreationTokens:    500000,
	}
	snapshot := BillingPriceSnapshot{
		InputPricePerMillion:      5,
		CacheReadPricePerMillion:  2,
		CacheWritePricePerMillion: 4,
	}
	// Responses input_tokens is the total input: cache read and cache write are
	// both subsets, so this request has no non-cache input to bill at 5/M.
	if amount := billingChargeAmount(usage, snapshot); amount != 3 {
		t.Fatalf("billingChargeAmount() = %v, want 3", amount)
	}
}

func TestBillingChargeAmountPreservesCanonicalZeroCacheRead(t *testing.T) {
	t.Parallel()

	usage := &UsageRecord{
		Provider:               "openai",
		InputTokens:            100,
		CachedTokens:           30,
		CacheReadTokens:        0,
		CacheReadTokensPresent: true,
	}
	snapshot := BillingPriceSnapshot{InputPricePerMillion: 10, CacheReadPricePerMillion: 2}
	if amount := billingChargeAmount(usage, snapshot); amount != 0.001 {
		t.Fatalf("billingChargeAmount() = %.9f, want %.9f", amount, 0.001)
	}
}

func TestAppendUsageChargesCachedTokensWithCacheReadPrice(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()

	username := "alice@example.com"
	credits := 10.0
	user, errCreateUser := repo.CreateUser(ctx, UserUpdate{Username: &username, Credits: &credits})
	if errCreateUser != nil {
		t.Fatalf("CreateUser() error = %v", errCreateUser)
	}
	clientKey := "client-key"
	if _, errCreateKey := repo.CreateAPIKeyForUser(ctx, user.ID, APIKeyUserUpdate{APIKey: &clientKey}); errCreateKey != nil {
		t.Fatalf("CreateAPIKeyForUser() error = %v", errCreateKey)
	}
	if _, errCreatePrice := repo.CreateBillingModelPrice(ctx, BillingModelPriceUpdate{
		Provider:                 "openai",
		Model:                    "gpt-4.1-mini",
		InputPricePerMillion:     2,
		CacheReadPricePerMillion: 0.5,
		Enabled:                  true,
	}); errCreatePrice != nil {
		t.Fatalf("CreateBillingModelPrice() error = %v", errCreatePrice)
	}

	payload := `{"timestamp":"2026-06-10T01:02:03Z","provider":"openai","model":"gpt-4.1-mini","api_key":"client-key","request_id":"req-cached","tokens":{"input_tokens":1000000,"cached_tokens":250000,"total_tokens":1000000}}`
	if _, errUsage := repo.AppendUsage(ctx, payload, "192.0.2.10"); errUsage != nil {
		t.Fatalf("AppendUsage() error = %v", errUsage)
	}

	charges, errCharges := repo.ListBillingCharges(ctx, BillingChargeQuery{UserID: &user.ID, Limit: 10})
	if errCharges != nil {
		t.Fatalf("ListBillingCharges() error = %v", errCharges)
	}
	if len(charges.Records) != 1 {
		t.Fatalf("charge count = %d, want 1", len(charges.Records))
	}
	charge := charges.Records[0]
	if charge.Amount != 1.625 {
		t.Fatalf("charge amount = %v, want 1.625", charge.Amount)
	}
	if charge.CacheTokens != 250000 {
		t.Fatalf("cache tokens = %d, want 250000", charge.CacheTokens)
	}
	if charge.BalanceBefore != 10 || charge.BalanceAfter != 8.375 {
		t.Fatalf("charge balances = %v -> %v, want 10 -> 8.375", charge.BalanceBefore, charge.BalanceAfter)
	}
}

func TestAppendUsageChargesClaudeCacheBucketsIndependently(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()

	username := "alice@example.com"
	credits := 20.0
	user, errCreateUser := repo.CreateUser(ctx, UserUpdate{Username: &username, Credits: &credits})
	if errCreateUser != nil {
		t.Fatalf("CreateUser() error = %v", errCreateUser)
	}
	clientKey := "client-key"
	if _, errCreateKey := repo.CreateAPIKeyForUser(ctx, user.ID, APIKeyUserUpdate{APIKey: &clientKey}); errCreateKey != nil {
		t.Fatalf("CreateAPIKeyForUser() error = %v", errCreateKey)
	}
	if _, errCreatePrice := repo.CreateBillingModelPrice(ctx, BillingModelPriceUpdate{
		Provider:                  "claude",
		Model:                     "claude-sonnet-4",
		InputPricePerMillion:      2,
		OutputPricePerMillion:     8,
		CacheReadPricePerMillion:  0.5,
		CacheWritePricePerMillion: 3,
		Enabled:                   true,
	}); errCreatePrice != nil {
		t.Fatalf("CreateBillingModelPrice() error = %v", errCreatePrice)
	}

	payload := `{"timestamp":"2026-06-10T01:02:03Z","provider":"claude","model":"claude-sonnet-4","api_key":"client-key","request_id":"req-claude-cache","tokens":{"input_tokens":1000000,"output_tokens":1000000,"cached_tokens":250000,"cache_read_tokens":250000,"cache_creation_tokens":100000,"total_tokens":2350000}}`
	if _, errUsage := repo.AppendUsage(ctx, payload, "192.0.2.10"); errUsage != nil {
		t.Fatalf("AppendUsage() error = %v", errUsage)
	}

	charges, errCharges := repo.ListBillingCharges(ctx, BillingChargeQuery{UserID: &user.ID, Limit: 10})
	if errCharges != nil {
		t.Fatalf("ListBillingCharges() error = %v", errCharges)
	}
	if len(charges.Records) != 1 {
		t.Fatalf("charge count = %d, want 1", len(charges.Records))
	}
	charge := charges.Records[0]
	if charge.Amount != 10.425 {
		t.Fatalf("charge amount = %v, want 10.425", charge.Amount)
	}
	if charge.CacheTokens != 350000 {
		t.Fatalf("cache tokens = %d, want 350000", charge.CacheTokens)
	}
	if charge.BalanceBefore != 20 || charge.BalanceAfter != 9.575 {
		t.Fatalf("charge balances = %v -> %v, want 20 -> 9.575", charge.BalanceBefore, charge.BalanceAfter)
	}
}

func TestAppendUsageDuplicatePayloadDoesNotDoubleCharge(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()

	username := "alice@example.com"
	credits := 10.0
	user, errCreateUser := repo.CreateUser(ctx, UserUpdate{Username: &username, Credits: &credits})
	if errCreateUser != nil {
		t.Fatalf("CreateUser() error = %v", errCreateUser)
	}
	clientKey := "client-key"
	if _, errCreateKey := repo.CreateAPIKeyForUser(ctx, user.ID, APIKeyUserUpdate{APIKey: &clientKey}); errCreateKey != nil {
		t.Fatalf("CreateAPIKeyForUser() error = %v", errCreateKey)
	}
	if _, errCreatePrice := repo.CreateBillingModelPrice(ctx, BillingModelPriceUpdate{
		Provider:     "openai",
		Model:        "gpt-4.1-mini",
		RequestPrice: 1,
		Enabled:      true,
	}); errCreatePrice != nil {
		t.Fatalf("CreateBillingModelPrice() error = %v", errCreatePrice)
	}

	payload := `{"timestamp":"2026-06-10T01:02:03Z","provider":"openai","model":"gpt-4.1-mini","api_key":"client-key","request_id":"req-1","tokens":{"input_tokens":1,"total_tokens":1}}`
	if _, errFirst := repo.AppendUsage(ctx, payload, "192.0.2.10"); errFirst != nil {
		t.Fatalf("AppendUsage(first) error = %v", errFirst)
	}
	if _, errSecond := repo.AppendUsage(ctx, payload, "192.0.2.10"); errSecond != nil {
		t.Fatalf("AppendUsage(second) error = %v", errSecond)
	}

	// Simulate CPA failover retry to another Home instance or connection IP (and reordered JSON keys as in PostgreSQL JSONB).
	failoverPayload := `{"tokens":{"total_tokens":1,"input_tokens":1},"request_id":"req-1","api_key":"client-key","model":"gpt-4.1-mini","provider":"openai","timestamp":"2026-06-10T01:02:03Z","cpa_node_id":"node-b","cpa_ip":"10.0.0.9","home":{"port":9327}}`
	if _, inserted, errThird := repo.AppendUsageWithRuntimeOutcome(ctx, failoverPayload, UsageRuntimeMetadata{HomeIP: "192.0.2.11", HomePort: 9327}); errThird != nil || inserted {
		t.Fatalf("AppendUsageWithRuntimeOutcome(failover duplicate) inserted=%v error = %v, want false/nil", inserted, errThird)
	}

	execPayloadA := `{"timestamp":"2026-06-10T01:02:04Z","provider":"openai","model":"gpt-4.1-mini","api_key":"client-key","request_id":"req-2","execution_id":"exec-retry-1","tokens":{"input_tokens":1,"total_tokens":1}}`
	execPayloadB := `{"timestamp":"2026-06-10T01:02:04Z","provider":"openai","model":"gpt-4.1-mini","api_key":"client-key","request_id":"req-2","execution_id":"exec-retry-1","cpa_node_id":"node-2","tokens":{"input_tokens":1,"total_tokens":1}}`
	firstExec, insertedFirst, errExecFirst := repo.AppendUsageWithRuntimeOutcome(ctx, execPayloadA, UsageRuntimeMetadata{HomeIP: "192.0.2.10", HomePort: 8327})
	if errExecFirst != nil || !insertedFirst {
		t.Fatalf("AppendUsageWithRuntimeOutcome(exec first) inserted=%v error = %v", insertedFirst, errExecFirst)
	}
	db, errDB := repo.database()
	if errDB != nil {
		t.Fatalf("database() error = %v", errDB)
	}
	// Simulate a pre-upgrade billing_charge row that used legacyBillingPayloadHash(rawPayload).
	if errLegacyHash := db.Model(&BillingChargeRecord{}).Where("usage_id = ?", firstExec.ID).Update("payload_hash", legacyBillingPayloadHash(execPayloadA)).Error; errLegacyHash != nil {
		t.Fatalf("Update(payload_hash legacy) error = %v", errLegacyHash)
	}
	secondExec, insertedSecond, errExecSecond := repo.AppendUsageWithRuntimeOutcome(ctx, execPayloadB, UsageRuntimeMetadata{HomeIP: "192.0.2.11", HomePort: 9327})
	if errExecSecond != nil || insertedSecond {
		t.Fatalf("AppendUsageWithRuntimeOutcome(exec duplicate) inserted=%v error = %v, want false/nil", insertedSecond, errExecSecond)
	}
	if secondExec == nil || secondExec.ID != firstExec.ID {
		t.Fatalf("deduplicated usage id = %+v, want %d", secondExec, firstExec.ID)
	}

	var usageCount int64
	if errCount := db.Model(&UsageRecord{}).Count(&usageCount).Error; errCount != nil {
		t.Fatalf("Count(usage) error = %v", errCount)
	}
	if usageCount != 2 {
		t.Fatalf("usage count = %d, want 2", usageCount)
	}

	charges, errCharges := repo.ListBillingCharges(ctx, BillingChargeQuery{UserID: &user.ID, Limit: 10})
	if errCharges != nil {
		t.Fatalf("ListBillingCharges() error = %v", errCharges)
	}
	if len(charges.Records) != 2 {
		t.Fatalf("charge count = %d, want 2", len(charges.Records))
	}
	updated, errUser := repo.GetUser(ctx, user.ID)
	if errUser != nil {
		t.Fatalf("GetUser() error = %v", errUser)
	}
	if updated.Credits != 8 {
		t.Fatalf("credits = %v, want 8", updated.Credits)
	}
}

func TestAppendUsageChargesSoftDeletedAPIKeyOwner(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()

	username := "alice@example.com"
	credits := 10.0
	user, errCreateUser := repo.CreateUser(ctx, UserUpdate{Username: &username, Credits: &credits})
	if errCreateUser != nil {
		t.Fatalf("CreateUser() error = %v", errCreateUser)
	}
	clientKey := "client-key"
	apiKey, errCreateKey := repo.CreateAPIKeyForUser(ctx, user.ID, APIKeyUserUpdate{APIKey: &clientKey})
	if errCreateKey != nil {
		t.Fatalf("CreateAPIKeyForUser() error = %v", errCreateKey)
	}
	if _, errCreatePrice := repo.CreateBillingModelPrice(ctx, BillingModelPriceUpdate{
		Provider:     "openai",
		Model:        "gpt-4.1-mini",
		RequestPrice: 1,
		Enabled:      true,
	}); errCreatePrice != nil {
		t.Fatalf("CreateBillingModelPrice() error = %v", errCreatePrice)
	}
	if errDeleteKey := repo.DeleteAPIKeyForUser(ctx, user.ID, apiKey.ID, ""); errDeleteKey != nil {
		t.Fatalf("DeleteAPIKeyForUser() error = %v", errDeleteKey)
	}

	payload := `{"timestamp":"2026-06-10T01:02:03Z","provider":"openai","model":"gpt-4.1-mini","api_key":"client-key","request_id":"req-soft-deleted-key","tokens":{"input_tokens":1,"total_tokens":1}}`
	if _, errUsage := repo.AppendUsage(ctx, payload, "192.0.2.10"); errUsage != nil {
		t.Fatalf("AppendUsage() error = %v", errUsage)
	}

	charges, errCharges := repo.ListBillingCharges(ctx, BillingChargeQuery{UserID: &user.ID, Limit: 10})
	if errCharges != nil {
		t.Fatalf("ListBillingCharges() error = %v", errCharges)
	}
	if len(charges.Records) != 1 {
		t.Fatalf("charge count = %d, want 1", len(charges.Records))
	}
	if charges.Records[0].UserID == nil || *charges.Records[0].UserID != user.ID {
		t.Fatalf("charge user id = %v, want %d", charges.Records[0].UserID, user.ID)
	}
	updated, errUser := repo.GetUser(ctx, user.ID)
	if errUser != nil {
		t.Fatalf("GetUser() error = %v", errUser)
	}
	if updated.Credits != 9 {
		t.Fatalf("credits = %v, want 9", updated.Credits)
	}
}

func TestAppendUsageRequestPriceChargesZeroTokenUsage(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()

	username := "alice@example.com"
	credits := 10.0
	user, errCreateUser := repo.CreateUser(ctx, UserUpdate{Username: &username, Credits: &credits})
	if errCreateUser != nil {
		t.Fatalf("CreateUser() error = %v", errCreateUser)
	}
	clientKey := "client-key"
	if _, errCreateKey := repo.CreateAPIKeyForUser(ctx, user.ID, APIKeyUserUpdate{APIKey: &clientKey}); errCreateKey != nil {
		t.Fatalf("CreateAPIKeyForUser() error = %v", errCreateKey)
	}
	if _, errCreatePrice := repo.CreateBillingModelPrice(ctx, BillingModelPriceUpdate{
		Provider:     "openai",
		Model:        "gpt-4.1-mini",
		RequestPrice: 1,
		Enabled:      true,
	}); errCreatePrice != nil {
		t.Fatalf("CreateBillingModelPrice() error = %v", errCreatePrice)
	}

	payload := `{"timestamp":"2026-06-10T01:02:03Z","provider":"openai","model":"gpt-4.1-mini","api_key":"client-key","request_id":"req-zero-token","tokens":{"total_tokens":0}}`
	if _, errUsage := repo.AppendUsage(ctx, payload, "192.0.2.10"); errUsage != nil {
		t.Fatalf("AppendUsage() error = %v", errUsage)
	}

	charges, errCharges := repo.ListBillingCharges(ctx, BillingChargeQuery{UserID: &user.ID, Limit: 10})
	if errCharges != nil {
		t.Fatalf("ListBillingCharges() error = %v", errCharges)
	}
	if len(charges.Records) != 1 {
		t.Fatalf("charge count = %d, want 1", len(charges.Records))
	}
	if charges.Records[0].Amount != 1 {
		t.Fatalf("charge amount = %v, want 1", charges.Records[0].Amount)
	}
	updated, errUser := repo.GetUser(ctx, user.ID)
	if errUser != nil {
		t.Fatalf("GetUser() error = %v", errUser)
	}
	if updated.Credits != 9 {
		t.Fatalf("credits = %v, want 9", updated.Credits)
	}
}

func TestAppendUsageWithoutModelPriceCreatesZeroCharge(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()

	username := "alice@example.com"
	credits := 10.0
	user, errCreateUser := repo.CreateUser(ctx, UserUpdate{Username: &username, Credits: &credits})
	if errCreateUser != nil {
		t.Fatalf("CreateUser() error = %v", errCreateUser)
	}
	clientKey := "client-key"
	if _, errCreateKey := repo.CreateAPIKeyForUser(ctx, user.ID, APIKeyUserUpdate{APIKey: &clientKey}); errCreateKey != nil {
		t.Fatalf("CreateAPIKeyForUser() error = %v", errCreateKey)
	}

	payload := `{"timestamp":"2026-06-10T01:02:03Z","provider":"openai","model":"gpt-4.1-mini","api_key":"client-key","request_id":"req-no-price","tokens":{"input_tokens":100,"output_tokens":200,"total_tokens":300}}`
	if _, errUsage := repo.AppendUsage(ctx, payload, "192.0.2.10"); errUsage != nil {
		t.Fatalf("AppendUsage() error = %v", errUsage)
	}

	charges, errCharges := repo.ListBillingCharges(ctx, BillingChargeQuery{UserID: &user.ID, Limit: 10})
	if errCharges != nil {
		t.Fatalf("ListBillingCharges() error = %v", errCharges)
	}
	if len(charges.Records) != 1 {
		t.Fatalf("charge count = %d, want 1", len(charges.Records))
	}
	charge := charges.Records[0]
	if charge.Amount != 0 {
		t.Fatalf("charge amount = %v, want 0", charge.Amount)
	}
	if charge.MatchedPriceRule != "default:zero" {
		t.Fatalf("matched price rule = %q, want default:zero", charge.MatchedPriceRule)
	}
	var snapshot BillingPriceSnapshot
	if errUnmarshal := json.Unmarshal(charge.PriceSnapshot, &snapshot); errUnmarshal != nil {
		t.Fatalf("unmarshal price snapshot: %v", errUnmarshal)
	}
	if snapshot.Provider != "openai" || snapshot.Model != "gpt-4.1-mini" {
		t.Fatalf("price snapshot provider/model = %q/%q, want openai/gpt-4.1-mini", snapshot.Provider, snapshot.Model)
	}
	if snapshot.InputPricePerMillion != 0 ||
		snapshot.OutputPricePerMillion != 0 ||
		snapshot.CacheReadPricePerMillion != 0 ||
		snapshot.CacheWritePricePerMillion != 0 ||
		snapshot.RequestPrice != 0 {
		t.Fatalf("price snapshot contains non-zero prices: %#v", snapshot)
	}
	updated, errUser := repo.GetUser(ctx, user.ID)
	if errUser != nil {
		t.Fatalf("GetUser() error = %v", errUser)
	}
	if updated.Credits != 10 {
		t.Fatalf("credits = %v, want 10", updated.Credits)
	}
}

func TestAppendUsageWithoutModelPriceDoesNotLogRecordNotFound(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()

	db, errDB := repo.database()
	if errDB != nil {
		t.Fatalf("database() error = %v", errDB)
	}
	var logs bytes.Buffer
	repo = NewRepository(db.Session(&gorm.Session{
		Logger: logger.New(log.New(&logs, "", 0), logger.Config{LogLevel: logger.Info}),
	}))

	username := "alice@example.com"
	credits := 10.0
	user, errCreateUser := repo.CreateUser(ctx, UserUpdate{Username: &username, Credits: &credits})
	if errCreateUser != nil {
		t.Fatalf("CreateUser() error = %v", errCreateUser)
	}
	clientKey := "client-key"
	if _, errCreateKey := repo.CreateAPIKeyForUser(ctx, user.ID, APIKeyUserUpdate{APIKey: &clientKey}); errCreateKey != nil {
		t.Fatalf("CreateAPIKeyForUser() error = %v", errCreateKey)
	}

	logs.Reset()
	payload := `{"timestamp":"2026-06-10T01:02:03Z","provider":"openai","model":"gpt-4.1-mini","api_key":"client-key","request_id":"req-no-price-log","tokens":{"input_tokens":100,"output_tokens":200,"total_tokens":300}}`
	if _, errUsage := repo.AppendUsage(ctx, payload, "192.0.2.10"); errUsage != nil {
		t.Fatalf("AppendUsage() error = %v", errUsage)
	}
	if strings.Contains(strings.ToLower(logs.String()), "record not found") {
		t.Fatalf("AppendUsage() logged record not found for missing model price: %s", logs.String())
	}
}

func TestAppendUsageListBillingChargesFiltersUserText(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()

	aliceName := "alice@example.com"
	aliceCredits := 10.0
	alice, errCreateAlice := repo.CreateUser(ctx, UserUpdate{Username: &aliceName, Credits: &aliceCredits})
	if errCreateAlice != nil {
		t.Fatalf("CreateUser(alice) error = %v", errCreateAlice)
	}
	bobName := "bob@example.com"
	bobCredits := 10.0
	bob, errCreateBob := repo.CreateUser(ctx, UserUpdate{Username: &bobName, Credits: &bobCredits})
	if errCreateBob != nil {
		t.Fatalf("CreateUser(bob) error = %v", errCreateBob)
	}

	aliceKey := "alice-client-key"
	if _, errCreateAliceKey := repo.CreateAPIKeyForUser(ctx, alice.ID, APIKeyUserUpdate{APIKey: &aliceKey}); errCreateAliceKey != nil {
		t.Fatalf("CreateAPIKeyForUser(alice) error = %v", errCreateAliceKey)
	}
	bobKey := "bob-client-key"
	if _, errCreateBobKey := repo.CreateAPIKeyForUser(ctx, bob.ID, APIKeyUserUpdate{APIKey: &bobKey}); errCreateBobKey != nil {
		t.Fatalf("CreateAPIKeyForUser(bob) error = %v", errCreateBobKey)
	}
	if _, errCreatePrice := repo.CreateBillingModelPrice(ctx, BillingModelPriceUpdate{
		Provider:     "openai",
		Model:        "gpt-4.1-mini",
		RequestPrice: 1,
		Enabled:      true,
	}); errCreatePrice != nil {
		t.Fatalf("CreateBillingModelPrice() error = %v", errCreatePrice)
	}

	alicePayload := `{"timestamp":"2026-06-10T01:02:03Z","provider":"openai","model":"gpt-4.1-mini","api_key":"alice-client-key","request_id":"req-alice","tokens":{"input_tokens":1,"total_tokens":1}}`
	if _, errAppendAlice := repo.AppendUsage(ctx, alicePayload, "192.0.2.10"); errAppendAlice != nil {
		t.Fatalf("AppendUsage(alice) error = %v", errAppendAlice)
	}
	bobPayload := `{"timestamp":"2026-06-10T01:03:03Z","provider":"openai","model":"gpt-4.1-mini","api_key":"bob-client-key","request_id":"req-bob","tokens":{"input_tokens":1,"total_tokens":1}}`
	if _, errAppendBob := repo.AppendUsage(ctx, bobPayload, "192.0.2.10"); errAppendBob != nil {
		t.Fatalf("AppendUsage(bob) error = %v", errAppendBob)
	}

	byUsername, errByUsername := repo.ListBillingCharges(ctx, BillingChargeQuery{UserText: "bob", Limit: 10})
	if errByUsername != nil {
		t.Fatalf("ListBillingCharges(user text username) error = %v", errByUsername)
	}
	if len(byUsername.Records) != 1 || byUsername.Records[0].UserID == nil || *byUsername.Records[0].UserID != bob.ID {
		t.Fatalf("username filter records = %#v, want one bob charge", byUsername.Records)
	}

	byUserID, errByUserID := repo.ListBillingCharges(ctx, BillingChargeQuery{UserText: strconv.FormatUint(uint64(alice.ID), 10), Limit: 10})
	if errByUserID != nil {
		t.Fatalf("ListBillingCharges(user text id) error = %v", errByUserID)
	}
	if len(byUserID.Records) != 1 || byUserID.Records[0].UserID == nil || *byUserID.Records[0].UserID != alice.ID {
		t.Fatalf("user id filter records = %#v, want one alice charge", byUserID.Records)
	}
}

func TestListBillingChargesUsesExclusiveToBoundary(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()

	username := "alice@example.com"
	credits := 10.0
	user, errCreateUser := repo.CreateUser(ctx, UserUpdate{Username: &username, Credits: &credits})
	if errCreateUser != nil {
		t.Fatalf("CreateUser() error = %v", errCreateUser)
	}
	key := "client-key"
	if _, errCreateKey := repo.CreateAPIKeyForUser(ctx, user.ID, APIKeyUserUpdate{APIKey: &key}); errCreateKey != nil {
		t.Fatalf("CreateAPIKeyForUser() error = %v", errCreateKey)
	}
	if _, errCreatePrice := repo.CreateBillingModelPrice(ctx, BillingModelPriceUpdate{
		Provider:     "openai",
		Model:        "gpt-4.1-mini",
		RequestPrice: 1,
		Enabled:      true,
	}); errCreatePrice != nil {
		t.Fatalf("CreateBillingModelPrice() error = %v", errCreatePrice)
	}

	beforePayload := `{"timestamp":"2026-06-10T11:59:59Z","provider":"openai","model":"gpt-4.1-mini","api_key":"client-key","request_id":"req-before-to","tokens":{"input_tokens":1,"total_tokens":1}}`
	if _, errBefore := repo.AppendUsage(ctx, beforePayload, "192.0.2.10"); errBefore != nil {
		t.Fatalf("AppendUsage(before) error = %v", errBefore)
	}
	afterPayload := `{"timestamp":"2026-06-10T12:00:01Z","provider":"openai","model":"gpt-4.1-mini","api_key":"client-key","request_id":"req-after-to","tokens":{"input_tokens":1,"total_tokens":1}}`
	if _, errAfter := repo.AppendUsage(ctx, afterPayload, "192.0.2.10"); errAfter != nil {
		t.Fatalf("AppendUsage(after) error = %v", errAfter)
	}
	atBoundaryPayload := `{"timestamp":"2026-06-10T12:00:00Z","provider":"openai","model":"gpt-4.1-mini","api_key":"client-key","request_id":"req-at-to-exclusive","tokens":{"input_tokens":1,"total_tokens":1}}`
	if _, errAtBoundary := repo.AppendUsage(ctx, atBoundaryPayload, "192.0.2.10"); errAtBoundary != nil {
		t.Fatalf("AppendUsage(at boundary) error = %v", errAtBoundary)
	}

	to := time.Date(2026, time.June, 10, 12, 0, 0, 0, time.UTC)
	result, errCharges := repo.ListBillingCharges(ctx, BillingChargeQuery{ToExclusive: &to, Limit: 10})
	if errCharges != nil {
		t.Fatalf("ListBillingCharges() error = %v", errCharges)
	}
	if len(result.Records) != 1 {
		t.Fatalf("charge count = %d, want 1", len(result.Records))
	}
	if result.Records[0].RequestID != "req-before-to" {
		t.Fatalf("charge request id = %q, want req-before-to", result.Records[0].RequestID)
	}
}

func TestListBillingBalanceRecordsUsesExclusiveToBoundary(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()

	username := "alice@example.com"
	user, errCreateUser := repo.CreateUser(ctx, UserUpdate{Username: &username})
	if errCreateUser != nil {
		t.Fatalf("CreateUser() error = %v", errCreateUser)
	}
	db, errDB := repo.database()
	if errDB != nil {
		t.Fatalf("database() error = %v", errDB)
	}
	records := []BillingBalanceRecord{
		{
			ID:            billingID("balance"),
			UserID:        user.ID,
			Type:          BillingBalanceTypeRecharge,
			Amount:        10,
			BalanceBefore: 0,
			BalanceAfter:  10,
			Operator:      "admin",
			CreatedAt:     time.Date(2026, time.June, 10, 11, 59, 59, 0, time.UTC),
		},
		{
			ID:            billingID("balance"),
			UserID:        user.ID,
			Type:          BillingBalanceTypeRecharge,
			Amount:        15,
			BalanceBefore: 10,
			BalanceAfter:  25,
			Operator:      "admin",
			CreatedAt:     time.Date(2026, time.June, 10, 12, 0, 0, 0, time.UTC),
		},
		{
			ID:            billingID("balance"),
			UserID:        user.ID,
			Type:          BillingBalanceTypeRecharge,
			Amount:        20,
			BalanceBefore: 25,
			BalanceAfter:  45,
			Operator:      "admin",
			CreatedAt:     time.Date(2026, time.June, 10, 12, 0, 1, 0, time.UTC),
		},
	}
	if errCreate := db.WithContext(ctx).Create(&records).Error; errCreate != nil {
		t.Fatalf("create balance records: %v", errCreate)
	}

	to := time.Date(2026, time.June, 10, 12, 0, 0, 0, time.UTC)
	result, errRecords := repo.ListBillingBalanceRecords(ctx, BillingBalanceQuery{ToExclusive: &to, Limit: 10})
	if errRecords != nil {
		t.Fatalf("ListBillingBalanceRecords() error = %v", errRecords)
	}
	if len(result.Records) != 1 {
		t.Fatalf("balance record count = %d, want 1", len(result.Records))
	}
	if result.Records[0].Amount != 10 {
		t.Fatalf("balance record amount = %v, want 10", result.Records[0].Amount)
	}
}

func TestBillingOverviewAggregatesChargesAndBalances(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()

	username := "alice@example.com"
	credits := 100.0
	user, errCreateUser := repo.CreateUser(ctx, UserUpdate{Username: &username, Credits: &credits})
	if errCreateUser != nil {
		t.Fatalf("CreateUser() error = %v", errCreateUser)
	}
	key := "client-key"
	if _, errKey := repo.CreateAPIKeyForUser(ctx, user.ID, APIKeyUserUpdate{APIKey: &key}); errKey != nil {
		t.Fatalf("CreateAPIKeyForUser() error = %v", errKey)
	}
	if _, errPrice := repo.CreateBillingModelPrice(ctx, BillingModelPriceUpdate{Provider: "openai", Model: "gpt-4.1-mini", RequestPrice: 2, Enabled: true}); errPrice != nil {
		t.Fatalf("CreateBillingModelPrice() error = %v", errPrice)
	}
	balanceRecord, errRecharge := repo.ApplyBillingBalanceRecord(ctx, BillingBalanceUpdate{UserID: user.ID, Type: BillingBalanceTypeRecharge, Amount: 50, Operator: "admin"})
	if errRecharge != nil {
		t.Fatalf("ApplyBillingBalanceRecord() error = %v", errRecharge)
	}
	db, errDB := repo.database()
	if errDB != nil {
		t.Fatalf("database() error = %v", errDB)
	}
	if errUpdate := db.WithContext(ctx).Model(&BillingBalanceRecord{}).Where("id = ?", balanceRecord.ID).Update("created_at", time.Date(2026, time.June, 10, 1, 2, 3, 0, time.UTC)).Error; errUpdate != nil {
		t.Fatalf("update balance record timestamp: %v", errUpdate)
	}
	payload := `{"timestamp":"2026-06-10T01:02:03Z","provider":"openai","model":"gpt-4.1-mini","api_key":"client-key","request_id":"req-1","tokens":{"input_tokens":1,"total_tokens":1}}`
	if _, errUsage := repo.AppendUsage(ctx, payload, "192.0.2.10"); errUsage != nil {
		t.Fatalf("AppendUsage() error = %v", errUsage)
	}

	from := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2027, time.January, 1, 0, 0, 0, 0, time.UTC)
	overview, errOverview := repo.BillingOverview(ctx, BillingOverviewQuery{From: &from, ToExclusive: &to})
	if errOverview != nil {
		t.Fatalf("BillingOverview() error = %v", errOverview)
	}
	if overview.Range.From != "2026-01-01" {
		t.Fatalf("range from = %q, want 2026-01-01", overview.Range.From)
	}
	if overview.Range.To != "2026-12-31" {
		t.Fatalf("range to = %q, want 2026-12-31", overview.Range.To)
	}
	if overview.Range.FromAt == nil || !overview.Range.FromAt.Equal(from) {
		t.Fatalf("range from_at = %v, want %s", overview.Range.FromAt, from.Format(time.RFC3339Nano))
	}
	if overview.Range.ToAtExclusive == nil || !overview.Range.ToAtExclusive.Equal(to) {
		t.Fatalf("range to_at_exclusive = %v, want %s", overview.Range.ToAtExclusive, to.Format(time.RFC3339Nano))
	}
	if overview.Range.Timezone != "UTC" {
		t.Fatalf("range timezone = %q, want UTC", overview.Range.Timezone)
	}
	if overview.TotalRechargeAmount != 50 {
		t.Fatalf("total recharge = %v, want 50", overview.TotalRechargeAmount)
	}
	if overview.TotalChargeAmount != 2 {
		t.Fatalf("total charge = %v, want 2", overview.TotalChargeAmount)
	}
	if overview.RequestCount != 1 {
		t.Fatalf("request count = %d, want 1", overview.RequestCount)
	}
	if len(overview.DailyTrend) != 1 {
		t.Fatalf("daily trend count = %d, want 1", len(overview.DailyTrend))
	}
	if overview.DailyTrend[0].Date != "2026-06-10" || overview.DailyTrend[0].ChargeAmount != 2 || overview.DailyTrend[0].RequestCount != 1 {
		t.Fatalf("daily trend = %#v, want 2026-06-10 amount 2 request count 1", overview.DailyTrend[0])
	}
	if len(overview.TopUsers) != 1 {
		t.Fatalf("top users count = %d, want 1", len(overview.TopUsers))
	}
	if overview.TopUsers[0].ID != strconv.FormatUint(uint64(user.ID), 10) || overview.TopUsers[0].Label != username || overview.TopUsers[0].Amount != 2 || overview.TopUsers[0].RequestCount != 1 {
		t.Fatalf("top user = %#v, want user %d/%s amount 2 request count 1", overview.TopUsers[0], user.ID, username)
	}
	if len(overview.TopModels) != 1 {
		t.Fatalf("top models count = %d, want 1", len(overview.TopModels))
	}
	if overview.TopModels[0].ID != "openai/gpt-4.1-mini" || overview.TopModels[0].Label != "gpt-4.1-mini" || overview.TopModels[0].Amount != 2 || overview.TopModels[0].RequestCount != 1 {
		t.Fatalf("top model = %#v, want openai/gpt-4.1-mini amount 2 request count 1", overview.TopModels[0])
	}
	if len(overview.TopProviders) != 1 {
		t.Fatalf("top providers count = %d, want 1", len(overview.TopProviders))
	}
	if overview.TopProviders[0].ID != "openai" || overview.TopProviders[0].Label != "openai" || overview.TopProviders[0].Amount != 2 || overview.TopProviders[0].RequestCount != 1 {
		t.Fatalf("top provider = %#v, want openai amount 2 request count 1", overview.TopProviders[0])
	}
}

func TestBillingOverviewGroupsDailyTrendByTimezone(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()

	username := "timezone@example.com"
	credits := 100.0
	user, errCreateUser := repo.CreateUser(ctx, UserUpdate{Username: &username, Credits: &credits})
	if errCreateUser != nil {
		t.Fatalf("CreateUser() error = %v", errCreateUser)
	}
	key := "timezone-client-key"
	if _, errKey := repo.CreateAPIKeyForUser(ctx, user.ID, APIKeyUserUpdate{APIKey: &key}); errKey != nil {
		t.Fatalf("CreateAPIKeyForUser() error = %v", errKey)
	}
	if _, errPrice := repo.CreateBillingModelPrice(ctx, BillingModelPriceUpdate{Provider: "openai", Model: "gpt-4.1-mini", RequestPrice: 1, Enabled: true}); errPrice != nil {
		t.Fatalf("CreateBillingModelPrice() error = %v", errPrice)
	}

	payloads := []string{
		`{"timestamp":"2026-06-09T17:00:00Z","provider":"openai","model":"gpt-4.1-mini","api_key":"timezone-client-key","request_id":"req-local-morning","tokens":{"input_tokens":1,"total_tokens":1}}`,
		`{"timestamp":"2026-06-10T09:00:00Z","provider":"openai","model":"gpt-4.1-mini","api_key":"timezone-client-key","request_id":"req-local-evening","tokens":{"input_tokens":1,"total_tokens":1}}`,
		`{"timestamp":"2026-03-08T09:00:00Z","provider":"openai","model":"gpt-4.1-mini","api_key":"timezone-client-key","request_id":"req-dst-before","tokens":{"input_tokens":1,"total_tokens":1}}`,
		`{"timestamp":"2026-03-08T10:30:00Z","provider":"openai","model":"gpt-4.1-mini","api_key":"timezone-client-key","request_id":"req-dst-after","tokens":{"input_tokens":1,"total_tokens":1}}`,
	}
	for _, payload := range payloads {
		if _, errUsage := repo.AppendUsage(ctx, payload, "192.0.2.10"); errUsage != nil {
			t.Fatalf("AppendUsage() error = %v", errUsage)
		}
	}

	from := time.Date(2026, time.June, 9, 16, 0, 0, 0, time.UTC)
	to := time.Date(2026, time.June, 10, 16, 0, 0, 0, time.UTC)
	overview, errOverview := repo.BillingOverview(ctx, BillingOverviewQuery{From: &from, ToExclusive: &to, Timezone: "Asia/Shanghai"})
	if errOverview != nil {
		t.Fatalf("BillingOverview(Asia/Shanghai) error = %v", errOverview)
	}
	if overview.RequestCount != 2 || overview.TotalChargeAmount != 2 {
		t.Fatalf("overview totals = requests %d amount %v, want 2 and 2", overview.RequestCount, overview.TotalChargeAmount)
	}
	if overview.Range.From != "2026-06-10" || overview.Range.To != "2026-06-10" || overview.Range.Timezone != "Asia/Shanghai" {
		t.Fatalf("overview range = %#v, want 2026-06-10 in Asia/Shanghai", overview.Range)
	}
	if len(overview.DailyTrend) != 1 {
		t.Fatalf("daily trend count = %d, want 1; trend = %#v", len(overview.DailyTrend), overview.DailyTrend)
	}
	if overview.DailyTrend[0].Date != "2026-06-10" || overview.DailyTrend[0].ChargeAmount != 2 || overview.DailyTrend[0].RequestCount != 2 {
		t.Fatalf("daily trend = %#v, want 2026-06-10 amount 2 request count 2", overview.DailyTrend[0])
	}

	utcOverview, errUTCOverview := repo.BillingOverview(ctx, BillingOverviewQuery{From: &from, ToExclusive: &to})
	if errUTCOverview != nil {
		t.Fatalf("BillingOverview(UTC) error = %v", errUTCOverview)
	}
	if len(utcOverview.DailyTrend) != 2 {
		t.Fatalf("UTC daily trend count = %d, want 2; trend = %#v", len(utcOverview.DailyTrend), utcOverview.DailyTrend)
	}
	if utcOverview.DailyTrend[0].Date != "2026-06-09" || utcOverview.DailyTrend[1].Date != "2026-06-10" {
		t.Fatalf("UTC daily trend = %#v, want June 9 and June 10", utcOverview.DailyTrend)
	}

	dstFrom := time.Date(2026, time.March, 8, 8, 0, 0, 0, time.UTC)
	dstTo := time.Date(2026, time.March, 9, 7, 0, 0, 0, time.UTC)
	dstOverview, errDSTOverview := repo.BillingOverview(ctx, BillingOverviewQuery{From: &dstFrom, ToExclusive: &dstTo, Timezone: "America/Los_Angeles"})
	if errDSTOverview != nil {
		t.Fatalf("BillingOverview(America/Los_Angeles) error = %v", errDSTOverview)
	}
	if dstOverview.RequestCount != 2 || len(dstOverview.DailyTrend) != 1 {
		t.Fatalf("DST overview = requests %d trend %#v, want 2 requests in one day", dstOverview.RequestCount, dstOverview.DailyTrend)
	}
	if dstOverview.Range.From != "2026-03-08" || dstOverview.Range.To != "2026-03-08" || dstOverview.DailyTrend[0].Date != "2026-03-08" {
		t.Fatalf("DST range/trend = range %#v trend %#v, want March 8", dstOverview.Range, dstOverview.DailyTrend)
	}
}

func TestBillingDateExpressionUsesPostgresTimezone(t *testing.T) {
	t.Parallel()

	db, errOpen := gorm.Open(postgres.New(postgres.Config{
		DSN:                  "host=127.0.0.1 user=cliproxy dbname=cliproxy_home sslmode=disable",
		PreferSimpleProtocol: true,
	}), &gorm.Config{DisableAutomaticPing: true})
	if errOpen != nil {
		t.Fatalf("open postgres dry-run db: %v", errOpen)
	}
	expression, args := billingDateExpression(db, `"billing_charge"."created_at"`, "Asia/Shanghai", billingTrendBounds{})
	if expression != `TO_CHAR(timezone(?, "billing_charge"."created_at"), 'YYYY-MM-DD')` {
		t.Fatalf("expression = %q", expression)
	}
	if len(args) != 1 || args[0] != "Asia/Shanghai" {
		t.Fatalf("args = %#v, want Asia/Shanghai", args)
	}
}

func TestBillingOverviewTotalBalanceUsesUserFilter(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()

	aliceName := "alice@example.com"
	aliceCredits := 100.0
	alice, errCreateAlice := repo.CreateUser(ctx, UserUpdate{Username: &aliceName, Credits: &aliceCredits})
	if errCreateAlice != nil {
		t.Fatalf("CreateUser(alice) error = %v", errCreateAlice)
	}
	bobName := "bob@example.com"
	bobCredits := 50.0
	bob, errCreateBob := repo.CreateUser(ctx, UserUpdate{Username: &bobName, Credits: &bobCredits})
	if errCreateBob != nil {
		t.Fatalf("CreateUser(bob) error = %v", errCreateBob)
	}

	aliceOverview, errAliceOverview := repo.BillingOverview(ctx, BillingOverviewQuery{UserID: &alice.ID})
	if errAliceOverview != nil {
		t.Fatalf("BillingOverview(alice) error = %v", errAliceOverview)
	}
	if aliceOverview.TotalBalance != 100 {
		t.Fatalf("alice total balance = %v, want 100", aliceOverview.TotalBalance)
	}

	bobOverview, errBobOverview := repo.BillingOverview(ctx, BillingOverviewQuery{UserText: "bob"})
	if errBobOverview != nil {
		t.Fatalf("BillingOverview(bob) error = %v", errBobOverview)
	}
	if bobOverview.TotalBalance != 50 {
		t.Fatalf("bob total balance = %v, want 50", bobOverview.TotalBalance)
	}
	if bob.ID == alice.ID {
		t.Fatal("test users unexpectedly share the same id")
	}
}

func newBillingTestRepository(t *testing.T, ctx context.Context) (*Repository, func()) {
	t.Helper()

	db, errOpenSQLite := OpenSQLite(ctx, filepath.Join(t.TempDir(), "home.db"))
	if errOpenSQLite != nil {
		t.Fatalf("OpenSQLite() error = %v", errOpenSQLite)
	}
	sqlDB, errDB := db.DB()
	if errDB != nil {
		t.Fatalf("db.DB() error = %v", errDB)
	}
	closeRepo := func() {
		if errClose := sqlDB.Close(); errClose != nil {
			t.Errorf("close sqlite db: %v", errClose)
		}
	}
	if errMigrate := AutoMigrate(db); errMigrate != nil {
		closeRepo()
		t.Fatalf("AutoMigrate() error = %v", errMigrate)
	}
	return NewRepository(db), closeRepo
}

func float64Ptr(value float64) *float64 {
	return &value
}

func stringPtr(value string) *string {
	return &value
}
