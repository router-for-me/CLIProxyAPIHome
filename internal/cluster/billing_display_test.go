package cluster

import (
	"context"
	"fmt"
	"math"
	"reflect"
	"testing"
)

func int64Ptr(value int64) *int64 {
	return &value
}

func TestBillingDisplayTokensDoNotChangeLedger(t *testing.T) {
	t.Parallel()
	t.Run("sqlite", func(t *testing.T) {
		repo, closeRepo := newBillingTestRepository(t, t.Context())
		t.Cleanup(closeRepo)
		assertBillingDisplayTokensDoNotChangeLedger(t, repo)
	})
	t.Run("postgres", func(t *testing.T) {
		assertBillingDisplayTokensDoNotChangeLedger(t, newPostgresQuiescenceRepository(t))
	})
}

func assertBillingDisplayTokensDoNotChangeLedger(t *testing.T, repo *Repository) {
	t.Helper()
	ctx := context.Background()
	db, errDB := repo.database()
	if errDB != nil {
		t.Fatal(errDB)
	}
	username, credits := "display-user", 100.0
	user, errCreateUser := repo.CreateUser(ctx, UserUpdate{Username: &username, Credits: &credits})
	if errCreateUser != nil {
		t.Fatal(errCreateUser)
	}
	key := "display-key"
	if _, errKey := repo.CreateAPIKeyForUser(ctx, user.ID, APIKeyUserUpdate{APIKey: &key}); errKey != nil {
		t.Fatal(errKey)
	}
	for _, provider := range []string{"gemini", "claude", "openai", "unknown"} {
		if _, errPrice := repo.CreateBillingModelPrice(ctx, BillingModelPriceUpdate{
			Provider: provider, Model: "display-model", Enabled: true,
			InputPricePerMillion: 0.75, OutputPricePerMillion: 3.75,
		}); errPrice != nil {
			t.Fatal(errPrice)
		}
	}
	cases := []struct {
		name, provider, tokens, state string
		input, output                 int64
		cacheRead, cacheWrite         *int64
	}{
		{"gemini", "gemini", `"input_tokens":61070,"output_tokens":50,"reasoning_tokens":139,"total_tokens":61259`, "", 61070, 189, int64Ptr(0), int64Ptr(0)},
		{"claude", "claude", `"input_tokens":2,"output_tokens":2070,"cache_read_tokens":10000,"cache_creation_tokens":926,"total_tokens":12998`, "", 10928, 2070, int64Ptr(10000), int64Ptr(926)},
		{"openai", "openai", `"input_tokens":87384,"output_tokens":204,"reasoning_tokens":100,"cache_read_tokens":84992,"total_tokens":87588`, "", 87384, 204, int64Ptr(84992), int64Ptr(0)},
		{"inconsistent", "gemini", `"input_tokens":100,"output_tokens":20,"reasoning_tokens":10,"total_tokens":999`, "", 100, 20, nil, nil},
		{"unclassified", "unknown", `"input_tokens":100,"output_tokens":20,"total_tokens":120`, "", 100, 20, nil, nil},
		{"pending", "gemini", `"input_tokens":100,"output_tokens":20,"reasoning_tokens":10,"total_tokens":130`, "pending", 100, 20, nil, nil},
		{"missing", "gemini", `"input_tokens":100,"output_tokens":20,"reasoning_tokens":10,"total_tokens":130`, "missing", 100, 20, nil, nil},
		{"canonical-zero", "gemini", `"input_tokens":100,"output_tokens":20,"reasoning_tokens":10,"total_tokens":130`, "zero", 0, 0, int64Ptr(0), int64Ptr(0)},
	}
	var wantInput, wantOutput int64
	for _, tc := range cases {
		payload := fmt.Sprintf(`{"timestamp":"2026-06-10T12:00:00Z","provider":%q,"model":"display-model","api_key":"display-key","request_id":%q,"tokens":{%s}}`, tc.provider, tc.name, tc.tokens)
		usage, errAppend := repo.AppendUsage(ctx, payload, "192.0.2.10")
		if errAppend != nil {
			t.Fatalf("%s: %v", tc.name, errAppend)
		}
		switch tc.state {
		case "pending":
			if errUpdate := db.Model(&UsageRecord{}).Where("id = ?", usage.ID).Update("token_accounting_version", 0).Error; errUpdate != nil {
				t.Fatal(errUpdate)
			}
		case "missing":
			if errDelete := db.Delete(&UsageRecord{}, usage.ID).Error; errDelete != nil {
				t.Fatal(errDelete)
			}
		case "zero":
			if errUpdate := db.Model(&UsageRecord{}).Where("id = ?", usage.ID).Updates(usageTokenBreakdownUpdates(newUnclassifiedUsageTokenBreakdown(0))).Error; errUpdate != nil {
				t.Fatal(errUpdate)
			}
		}
		wantInput += tc.input
		wantOutput += tc.output
	}
	var before []BillingChargeRecord
	if errFind := db.Order("id").Find(&before).Error; errFind != nil {
		t.Fatal(errFind)
	}
	var userBefore UserRecord
	if errFind := db.First(&userBefore, user.ID).Error; errFind != nil {
		t.Fatal(errFind)
	}
	var wantAmount float64
	var wantCache int64
	stored := make(map[string]BillingChargeRecord, len(before))
	for _, row := range before {
		stored[row.RequestID] = row
		wantAmount += row.Amount
		wantCache += row.CacheTokens
	}
	if got := stored["gemini"]; got.OutputTokens != 50 || math.Abs(got.Amount-0.04651125) > 1e-12 {
		t.Fatalf("Gemini ledger = %+v", got)
	}
	// User-scoped reads and administrator username filters share this read model.
	result, errList := repo.ListBillingCharges(ctx, BillingChargeQuery{UserID: &user.ID, UserText: username, Limit: 100})
	if errList != nil {
		t.Fatal(errList)
	}
	if result.Total != int64(len(cases)) || len(result.Records) != len(cases) {
		t.Fatalf("list counts = %d/%d", result.Total, len(result.Records))
	}
	for _, tc := range cases {
		found := false
		for _, row := range result.Records {
			if row.RequestID != tc.name {
				continue
			}
			found = true
			if row.InputTokens != tc.input || row.OutputTokens != tc.output {
				t.Fatalf("%s display = %d/%d, want %d/%d", tc.name, row.InputTokens, row.OutputTokens, tc.input, tc.output)
			}
			if !reflect.DeepEqual(row.CacheReadTokens, tc.cacheRead) || !reflect.DeepEqual(row.CacheWriteTokens, tc.cacheWrite) {
				t.Fatalf("%s cache split = %v/%v, want %v/%v", tc.name, row.CacheReadTokens, row.CacheWriteTokens, tc.cacheRead, tc.cacheWrite)
			}
			original := stored[tc.name]
			row.InputTokens, row.OutputTokens = original.InputTokens, original.OutputTokens
			row.CacheReadTokens, row.CacheWriteTokens = original.CacheReadTokens, original.CacheWriteTokens
			if !reflect.DeepEqual(row, original) {
				t.Fatalf("%s changed non-display fields", tc.name)
			}
		}
		if !found {
			t.Fatalf("missing charge %s", tc.name)
		}
	}
	overview, errOverview := repo.BillingOverview(ctx, BillingOverviewQuery{UserID: &user.ID, UserText: username})
	if errOverview != nil {
		t.Fatal(errOverview)
	}
	if overview.InputTokens != wantInput || overview.OutputTokens != wantOutput || overview.CacheTokens != wantCache || math.Abs(overview.TotalChargeAmount-wantAmount) > 1e-12 || overview.RequestCount != int64(len(cases)) {
		t.Fatalf("overview = %+v", overview)
	}
	page, errPage := repo.ListBillingCharges(ctx, BillingChargeQuery{Limit: 2, Offset: 2})
	if errPage != nil || len(page.Records) != 2 || page.Total != int64(len(cases)) {
		t.Fatalf("page = %+v, error = %v", page, errPage)
	}
	if !reflect.DeepEqual(page.Records, result.Records[2:4]) {
		t.Fatal("pagination changed ordering or display tokens")
	}
	filtered, errFiltered := repo.BillingOverview(ctx, BillingOverviewQuery{Provider: "claude", Model: "display-model"})
	if errFiltered != nil || filtered.RequestCount != 1 || filtered.InputTokens != 10928 || filtered.OutputTokens != 2070 {
		t.Fatalf("filtered overview = %+v, error = %v", filtered, errFiltered)
	}
	empty, errEmpty := repo.BillingOverview(ctx, BillingOverviewQuery{Provider: "missing-provider"})
	if errEmpty != nil || empty.RequestCount != 0 || empty.InputTokens != 0 || empty.OutputTokens != 0 {
		t.Fatalf("empty overview = %+v, error = %v", empty, errEmpty)
	}
	var after []BillingChargeRecord
	if errFind := db.Order("id").Find(&after).Error; errFind != nil {
		t.Fatal(errFind)
	}
	var userAfter UserRecord
	if errFind := db.First(&userAfter, user.ID).Error; errFind != nil {
		t.Fatal(errFind)
	}
	if !reflect.DeepEqual(before, after) || !reflect.DeepEqual(userBefore, userAfter) {
		t.Fatal("display queries mutated the ledger or user balance")
	}
}
