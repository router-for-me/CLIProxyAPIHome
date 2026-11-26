package cluster

import (
	"context"
	"testing"
)

func TestBillingPriceMatchingPrefersSpecificRules(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	for _, update := range []BillingModelPriceUpdate{
		{Provider: "*", Model: "*", InputPricePerMillion: 1, Enabled: true},
		{Provider: "*", Model: "gemini-3.5-flash", InputPricePerMillion: 2, Enabled: true},
		{Provider: "antigravity", Model: "gemini-*", InputPricePerMillion: 3, Enabled: true},
		{Provider: "antigravity", Model: "gemini-3.5-*", InputPricePerMillion: 4, Enabled: true},
		{Provider: "antigravity", Model: "Gemini-3.5-Flash-Low", InputPricePerMillion: 5, Enabled: true},
		{Provider: "antigravity", Model: "gemini-3.5-pro", InputPricePerMillion: 6, Enabled: true},
	} {
		if _, errCreate := repo.CreateBillingModelPrice(ctx, update); errCreate != nil {
			t.Fatalf("CreateBillingModelPrice(%+v) error = %v", update, errCreate)
		}
	}
	db, _ := repo.database()
	for _, tt := range []struct {
		name, provider, model, alias string
		wantPrice                    float64
		wantMatch, wantSource        string
	}{
		{"exact case-insensitive", "antigravity", "gemini-3.5-flash-low", "", 5, BillingPriceMatchExact, billingPriceModelSourceModel},
		{"longest pattern", "antigravity", "gemini-3.5-flash-unc", "", 4, BillingPriceMatchModelPattern, billingPriceModelSourceModel},
		{"shorter pattern", "antigravity", "gemini-4-argon", "", 3, BillingPriceMatchModelPattern, billingPriceModelSourceModel},
		{"exact alias beats model pattern", "antigravity", "gemini-3-flash-agent", "gemini-3.5-pro", 6, BillingPriceMatchExact, billingPriceModelSourceAlias},
		{"any provider exact", "vertex", "gemini-3.5-flash", "", 2, BillingPriceMatchAnyProvider, billingPriceModelSourceModel},
		{"any provider catch-all", "vertex", "glm-5.3", "", 1, BillingPriceMatchAnyProviderModelPattern, billingPriceModelSourceModel},
	} {
		t.Run(tt.name, func(t *testing.T) {
			usage := &UsageRecord{Provider: tt.provider, Model: tt.model, Alias: tt.alias, InputTokens: 10}
			_, snapshot, errSnapshot := billingPriceSnapshotForUsage(ctx, db, usage)
			if errSnapshot != nil {
				t.Fatalf("billingPriceSnapshotForUsage() error = %v", errSnapshot)
			}
			if snapshot.InputPricePerMillion != tt.wantPrice || snapshot.PriceMatch != tt.wantMatch || snapshot.PriceModelSource != tt.wantSource {
				t.Fatalf("snapshot = price %v match %q source %q, want %v %q %q", snapshot.InputPricePerMillion, snapshot.PriceMatch, snapshot.PriceModelSource, tt.wantPrice, tt.wantMatch, tt.wantSource)
			}
		})
	}
}

func TestBillingPricePatternValidationAndDispatchGate(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	for _, update := range []BillingModelPriceUpdate{
		{Provider: "openai", Model: "gpt-*-mini", Enabled: true},
		{Provider: "open*", Model: "gpt", Enabled: true},
	} {
		if _, errCreate := repo.CreateBillingModelPrice(ctx, update); errCreate == nil {
			t.Fatalf("CreateBillingModelPrice(%+v) accepted an invalid wildcard", update)
		}
	}
	if _, errCreate := repo.CreateBillingModelPrice(ctx, BillingModelPriceUpdate{Provider: "codex", Model: "codex-*", Enabled: true}); errCreate != nil {
		t.Fatalf("CreateBillingModelPrice(pattern) error = %v", errCreate)
	}
	db, _ := repo.database()
	if priced, errPriced := billingModelHasEnabledPrice(ctx, db, "codex-auto-review"); errPriced != nil || !priced {
		t.Fatalf("billingModelHasEnabledPrice(pattern) = %v, %v", priced, errPriced)
	}
	if priced, errPriced := billingModelHasEnabledPrice(ctx, db, "gpt-5.5"); errPriced != nil || priced {
		t.Fatalf("billingModelHasEnabledPrice(unpriced) = %v, %v", priced, errPriced)
	}
}
