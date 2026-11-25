package cluster

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestBillingModelPriceImportAppliesAtomicallyAndReplaysIdempotently(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	preview, errPreview := repo.CreateBillingModelPriceImportPreview(ctx, billingImportTestInput("missing"), billingImportTestCatalog())
	if errPreview != nil {
		t.Fatalf("CreateBillingModelPriceImportPreview() error = %v", errPreview)
	}
	if len(preview.Rows) != 1 || preview.Rows[0].Action != "create" || !preview.Rows[0].Applicable {
		t.Fatalf("preview rows = %#v", preview.Rows)
	}
	if preview.Rows[0].Final == nil || preview.Rows[0].Final.CacheWrite != 0 {
		t.Fatalf("preview cache-write price = %#v, want zero", preview.Rows[0].Final)
	}
	input := BillingModelPriceImportApplyInput{PreviewID: preview.PreviewID, PreviewRevision: preview.PreviewRevision, SelectedKeys: []string{preview.Rows[0].RowKey}, IdempotencyKey: "replay-key"}
	first, errApply := repo.ApplyBillingModelPriceImport(ctx, input)
	if errApply != nil {
		t.Fatalf("ApplyBillingModelPriceImport() error = %v", errApply)
	}
	second, errReplay := repo.ApplyBillingModelPriceImport(ctx, input)
	if errReplay != nil {
		t.Fatalf("replay ApplyBillingModelPriceImport() error = %v", errReplay)
	}
	if first.OperationID == "" || second.OperationID != first.OperationID || second.Status != "applied" || len(first.Rows) != 1 || first.Rows[0].ResourceID == "" {
		t.Fatalf("operations = %#v / %#v", first, second)
	}
	rules, errRules := repo.ListBillingModelPrices(ctx, BillingModelPriceQuery{Provider: "openai", Model: "gpt-import"})
	if errRules != nil {
		t.Fatalf("ListBillingModelPrices() error = %v", errRules)
	}
	if len(rules) != 1 || rules[0].Revision != 1 || first.Rows[0].ResourceID != rules[0].ID {
		t.Fatalf("stored rules = %#v", rules)
	}
	operation, errOperation := repo.GetBillingModelPriceImportOperation(ctx, first.OperationID)
	if errOperation != nil || operation.OperationID != first.OperationID {
		t.Fatalf("GetBillingModelPriceImportOperation() = %#v, %v", operation, errOperation)
	}
}

func TestDiscoverBillingImportTargetsCountsDistinctMergedTargets(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	if _, errCreate := repo.CreateBillingModelPrice(ctx, BillingModelPriceUpdate{Provider: "openai", Model: "model-0", Enabled: true}); errCreate != nil {
		t.Fatal(errCreate)
	}
	atLimit := func(yield func(string, string) bool) {
		for i := range billingImportMaxTargets {
			if !yield(" OPENAI ", fmt.Sprintf("model-%d", i)) {
				return
			}
		}
	}
	targets, errDiscover := repo.DiscoverBillingImportTargets(ctx, atLimit)
	if errDiscover != nil || len(targets) != billingImportMaxTargets {
		t.Fatalf("at limit: got %d targets, err=%v", len(targets), errDiscover)
	}
	targets, errDiscover = repo.DiscoverBillingImportTargets(ctx, func(yield func(string, string) bool) {
		for provider, model := range atLimit {
			if !yield(provider, model) {
				return
			}
		}
		if yield("openai", "additional-model") {
			t.Error("discovery kept consuming targets after overflow")
		}
	})
	if !errors.Is(errDiscover, ErrBillingImportTargetLimitExceeded) || targets != nil {
		t.Fatalf("above limit: got %d targets, err=%v", len(targets), errDiscover)
	}
}

func TestBillingModelPriceImportUsesContextBandRowMultiplier(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	catalog := billingImportTestCatalog()
	catalog.Models[0].ContextBands = []BillingModelPriceImportContextBand{{
		MinInputTokens: 272001,
		Cost:           BillingModelPriceImportCost{Input: 4, Output: 16},
	}}
	input := billingImportTestInput("missing")
	input.Policy.RowMultipliers = map[string]float64{
		"openai::gpt-import::*::272001": 2.5,
	}

	preview, errPreview := repo.CreateBillingModelPriceImportPreview(ctx, input, catalog)
	if errPreview != nil {
		t.Fatalf("CreateBillingModelPriceImportPreview() error = %v", errPreview)
	}
	if len(preview.Rows) != 2 {
		t.Fatalf("preview rows = %#v, want base and context-band rows", preview.Rows)
	}
	if preview.Rows[0].Multiplier != 1 || preview.Rows[1].RowKey != "openai::gpt-import::*::272001" || preview.Rows[1].Multiplier != 2.5 || preview.Rows[1].Final == nil || preview.Rows[1].Final.Input != 10 {
		t.Fatalf("preview rows = %#v, want per-context-band multiplier", preview.Rows)
	}
}

func TestBillingModelPriceImportUpdatesCachePrices(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	rule, errCreate := repo.CreateBillingModelPrice(ctx, BillingModelPriceUpdate{
		Provider:                  "openai",
		Model:                     "gpt-import",
		InputPricePerMillion:      1,
		OutputPricePerMillion:     2,
		CacheReadPricePerMillion:  7,
		CacheWritePricePerMillion: 9,
		Source:                    BillingPriceSourceSync,
		Enabled:                   true,
	})
	if errCreate != nil {
		t.Fatalf("CreateBillingModelPrice() error = %v", errCreate)
	}
	input := billingImportTestInput("sync")
	catalog := billingImportTestCatalog()
	catalog.Models[0].Cost.CacheRead = 3
	catalog.Models[0].Cost.CacheWrite = 5
	preview, errPreview := repo.CreateBillingModelPriceImportPreview(ctx, input, catalog)
	if errPreview != nil {
		t.Fatalf("CreateBillingModelPriceImportPreview() error = %v", errPreview)
	}
	if len(preview.Rows) != 1 || preview.Rows[0].Final == nil || preview.Rows[0].Final.CacheRead != 3 || preview.Rows[0].Final.CacheWrite != 5 {
		t.Fatalf("preview row = %#v, want imported cache prices", preview.Rows)
	}
	_, errApply := repo.ApplyBillingModelPriceImport(ctx, BillingModelPriceImportApplyInput{
		PreviewID:       preview.PreviewID,
		PreviewRevision: preview.PreviewRevision,
		SelectedKeys:    []string{preview.Rows[0].RowKey},
		IdempotencyKey:  "update-cache-prices",
	})
	if errApply != nil {
		t.Fatalf("ApplyBillingModelPriceImport() error = %v", errApply)
	}
	stored, errStored := repo.GetBillingModelPrice(ctx, rule.ID)
	if errStored != nil {
		t.Fatalf("GetBillingModelPrice() error = %v", errStored)
	}
	if stored.InputPricePerMillion != 2 || stored.OutputPricePerMillion != 8 || stored.CacheReadPricePerMillion != 3 || stored.CacheWritePricePerMillion != 5 {
		t.Fatalf("stored rule = %#v, want imported token and cache prices", stored)
	}
}

func TestBillingModelPriceImportRejectsDuplicateRowKeys(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	catalog := BillingModelPriceImportCatalog{
		SourceURL: "https://models.dev/api.json",
		Version:   "fixture-v1",
		FetchedAt: time.Now().UTC(),
		Models: []BillingModelPriceImportCatalogModel{
			{Provider: "openai", Model: "foo", Cost: &BillingModelPriceImportCost{Input: 1, Output: 2}, ContextBands: []BillingModelPriceImportContextBand{{MinInputTokens: 100, Cost: BillingModelPriceImportCost{Input: 3, Output: 4}}}},
			{Provider: "openai", Model: "foo::*::100", Cost: &BillingModelPriceImportCost{Input: 5, Output: 6}},
		},
	}
	input := billingImportTestInput("missing")
	input.Targets = []BillingModelPriceImportTarget{{Provider: "openai", Model: "foo"}, {Provider: "openai", Model: "foo::*::100"}}
	preview, errPreview := repo.CreateBillingModelPriceImportPreview(ctx, input, catalog)
	if errPreview != nil {
		t.Fatalf("CreateBillingModelPriceImportPreview() error = %v", errPreview)
	}
	conflicts := 0
	for _, row := range preview.Rows {
		if row.RowKey == "openai::foo::*::100" {
			conflicts++
			if row.Status != "conflict" || row.Action != "review" || row.Applicable || row.Final != nil || row.WriteRule != nil {
				t.Fatalf("duplicate row = %#v, want non-applicable conflict", row)
			}
		}
	}
	if conflicts != 2 {
		t.Fatalf("duplicate row-key conflicts = %d, want 2; rows=%#v", conflicts, preview.Rows)
	}
}

func TestBillingModelPriceImportPreservesContextBandScopeForReviewRows(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	db, errDB := repo.database()
	if errDB != nil {
		t.Fatalf("database() error = %v", errDB)
	}
	for _, record := range []BillingModelPriceRecord{
		{ID: "disabled_context_rule_1", Provider: "openai", Model: "gpt-import", ServiceTier: BillingServiceTierWildcard, MinInputTokens: 272001, Enabled: false},
		{ID: "disabled_context_rule_2", Provider: "openai", Model: "gpt-import", ServiceTier: BillingServiceTierWildcard, MinInputTokens: 272001, Enabled: false},
	} {
		if errCreate := db.WithContext(ctx).Create(&record).Error; errCreate != nil {
			t.Fatalf("create duplicate disabled context rule: %v", errCreate)
		}
	}
	catalog := billingImportTestCatalog()
	catalog.Models[0].ContextBands = []BillingModelPriceImportContextBand{{
		MinInputTokens: 272001,
		Cost:           BillingModelPriceImportCost{Input: 4, Output: 16},
	}}

	preview, errPreview := repo.CreateBillingModelPriceImportPreview(ctx, billingImportTestInput("missing"), catalog)
	if errPreview != nil {
		t.Fatalf("CreateBillingModelPriceImportPreview() error = %v", errPreview)
	}
	if len(preview.Rows) != 2 || preview.Rows[0].MinInputTokens != 0 || preview.Rows[1].Status != "conflict" || preview.Rows[1].MinInputTokens != 272001 {
		t.Fatalf("preview rows = %#v, want a context-scoped conflict row", preview.Rows)
	}
}

func TestBillingModelPriceImportMarksAmbiguousPreferredProviderMatchesForReview(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	catalog := BillingModelPriceImportCatalog{
		SourceURL: "https://models.dev/api.json",
		Version:   "fixture-v1",
		FetchedAt: time.Now().UTC(),
		Models: []BillingModelPriceImportCatalogModel{
			{Provider: "openai", Model: "gpt-duplicate", Cost: &BillingModelPriceImportCost{Input: 2, Output: 8}},
			{Provider: "openai", Model: "vendor/gpt-duplicate", Cost: &BillingModelPriceImportCost{Input: 3, Output: 12}},
		},
	}
	input := billingImportTestInput("missing")
	input.Targets[0].Model = "gpt-duplicate"

	preview, errPreview := repo.CreateBillingModelPriceImportPreview(ctx, input, catalog)
	if errPreview != nil {
		t.Fatalf("CreateBillingModelPriceImportPreview() error = %v", errPreview)
	}
	if len(preview.Rows) != 1 || preview.Rows[0].Status != "ambiguous" || preview.Rows[0].Action != "review" || preview.Rows[0].Applicable {
		t.Fatalf("preview rows = %#v, want an ambiguous review row", preview.Rows)
	}
}

func TestBillingModelPriceImportPrunesExpiredRecordsAndAllowsRepeatedSelectionHashes(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	db, errDB := repo.database()
	if errDB != nil {
		t.Fatalf("database() error = %v", errDB)
	}
	now := time.Now().UTC()
	oldPreview := BillingModelPriceImportPreviewRecord{ID: "preview_old", Revision: "previewrev_old", Source: BillingModelPriceImportSourceModelsDev, SourceURL: "https://models.dev/api.json", SourceVersion: "old", SourceFetchedAt: now.Add(-48 * time.Hour), Atomic: true, GeneratedAt: now.Add(-48 * time.Hour), ExpiresAt: now.Add(-48 * time.Hour), Payload: JSONB(`{}`), CreatedAt: now.Add(-48 * time.Hour)}
	oldOperation := BillingModelPriceImportOperationRecord{ID: "operation_old", PreviewID: oldPreview.ID, PreviewRevision: oldPreview.Revision, IdempotencyKey: "old-key", RequestHash: "old-request", SelectionHash: "same-selection", Atomic: true, Status: "applied", AppliedAt: now.Add(-31 * 24 * time.Hour), Result: JSONB(`{}`), CreatedAt: now.Add(-31 * 24 * time.Hour)}
	if errCreate := db.WithContext(ctx).Create(&oldPreview).Error; errCreate != nil {
		t.Fatalf("create old preview: %v", errCreate)
	}
	if errCreate := db.WithContext(ctx).Create(&oldOperation).Error; errCreate != nil {
		t.Fatalf("create old operation: %v", errCreate)
	}
	for _, record := range []BillingModelPriceImportOperationRecord{
		{ID: "operation_same_selection_1", PreviewID: "preview_a", PreviewRevision: "revision_a", IdempotencyKey: "same-selection-key-1", RequestHash: "request_a", SelectionHash: "reused-selection", Atomic: true, Status: "applied", AppliedAt: now, Result: JSONB(`{}`), CreatedAt: now},
		{ID: "operation_same_selection_2", PreviewID: "preview_b", PreviewRevision: "revision_b", IdempotencyKey: "same-selection-key-2", RequestHash: "request_b", SelectionHash: "reused-selection", Atomic: true, Status: "applied", AppliedAt: now, Result: JSONB(`{}`), CreatedAt: now},
	} {
		if errCreate := db.WithContext(ctx).Create(&record).Error; errCreate != nil {
			t.Fatalf("create operation with reused selection hash: %v", errCreate)
		}
	}
	if _, errPreview := repo.CreateBillingModelPriceImportPreview(ctx, billingImportTestInput("missing"), billingImportTestCatalog()); errPreview != nil {
		t.Fatalf("CreateBillingModelPriceImportPreview() error = %v", errPreview)
	}
	var previewCount, operationCount int64
	if errCount := db.WithContext(ctx).Model(&BillingModelPriceImportPreviewRecord{}).Where("id = ?", oldPreview.ID).Count(&previewCount).Error; errCount != nil {
		t.Fatalf("count old preview: %v", errCount)
	}
	if errCount := db.WithContext(ctx).Model(&BillingModelPriceImportOperationRecord{}).Where("id = ?", oldOperation.ID).Count(&operationCount).Error; errCount != nil {
		t.Fatalf("count old operation: %v", errCount)
	}
	if previewCount != 0 || operationCount != 0 {
		t.Fatalf("expired records remain: preview=%d operation=%d", previewCount, operationCount)
	}
}

func TestMigrateBillingImportIndexesRemovesLegacySelectionHashUniqueness(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	db, errDB := repo.database()
	if errDB != nil {
		t.Fatalf("database() error = %v", errDB)
	}
	if errDrop := db.Exec("DROP INDEX IF EXISTS idx_billing_model_price_import_operation_selection_hash").Error; errDrop != nil {
		t.Fatalf("drop selection-hash index: %v", errDrop)
	}
	if errCreate := db.Exec("CREATE UNIQUE INDEX idx_billing_model_price_import_operation_selection_hash ON billing_model_price_import_operation (selection_hash)").Error; errCreate != nil {
		t.Fatalf("create legacy unique selection-hash index: %v", errCreate)
	}
	if errMigrate := migrateBillingImportIndexes(db); errMigrate != nil {
		t.Fatalf("migrateBillingImportIndexes() error = %v", errMigrate)
	}
	now := time.Now().UTC()
	for _, record := range []BillingModelPriceImportOperationRecord{
		{ID: "operation_migrated_selection_1", PreviewID: "preview_a", PreviewRevision: "revision_a", IdempotencyKey: "migrated-selection-key-1", RequestHash: "request_a", SelectionHash: "migrated-selection", Atomic: true, Status: "applied", AppliedAt: now, Result: JSONB(`{}`), CreatedAt: now},
		{ID: "operation_migrated_selection_2", PreviewID: "preview_b", PreviewRevision: "revision_b", IdempotencyKey: "migrated-selection-key-2", RequestHash: "request_b", SelectionHash: "migrated-selection", Atomic: true, Status: "applied", AppliedAt: now, Result: JSONB(`{}`), CreatedAt: now},
	} {
		if errCreate := db.WithContext(ctx).Create(&record).Error; errCreate != nil {
			t.Fatalf("create operation after selection-hash migration: %v", errCreate)
		}
	}
}

func TestBillingModelPriceImportConvertsConcurrentCreateToConflict(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	preview, errPreview := repo.CreateBillingModelPriceImportPreview(ctx, billingImportTestInput("missing"), billingImportTestCatalog())
	if errPreview != nil {
		t.Fatalf("CreateBillingModelPriceImportPreview() error = %v", errPreview)
	}
	if _, errCreate := repo.CreateBillingModelPrice(ctx, BillingModelPriceUpdate{Provider: "openai", Model: "gpt-import", Enabled: true}); errCreate != nil {
		t.Fatalf("CreateBillingModelPrice() error = %v", errCreate)
	}
	_, errApply := repo.ApplyBillingModelPriceImport(ctx, BillingModelPriceImportApplyInput{PreviewID: preview.PreviewID, PreviewRevision: preview.PreviewRevision, SelectedKeys: []string{preview.Rows[0].RowKey}, IdempotencyKey: "create-conflict-key"})
	if !errors.Is(errApply, ErrBillingImportRuleConflict) {
		t.Fatalf("ApplyBillingModelPriceImport() error = %v, want rule conflict", errApply)
	}
}

func TestBillingModelPriceImportRollsBackEarlierRowsOnLaterConflict(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	catalog := BillingModelPriceImportCatalog{
		SourceURL: "https://models.dev/api.json",
		Version:   "fixture-v1",
		FetchedAt: time.Now().UTC(),
		Models: []BillingModelPriceImportCatalogModel{
			{Provider: "openai", Model: "a-model", Cost: &BillingModelPriceImportCost{Input: 1, Output: 2}},
			{Provider: "openai", Model: "z-model", Cost: &BillingModelPriceImportCost{Input: 3, Output: 4}},
		},
	}
	input := billingImportTestInput("missing")
	input.Targets = []BillingModelPriceImportTarget{{Provider: "openai", Model: "a-model"}, {Provider: "openai", Model: "z-model"}}
	preview, errPreview := repo.CreateBillingModelPriceImportPreview(ctx, input, catalog)
	if errPreview != nil {
		t.Fatalf("CreateBillingModelPriceImportPreview() error = %v", errPreview)
	}
	if len(preview.Rows) != 2 || preview.Rows[0].RowKey >= preview.Rows[1].RowKey {
		t.Fatalf("preview rows = %#v, want two ordered create rows", preview.Rows)
	}
	if _, errCreate := repo.CreateBillingModelPrice(ctx, BillingModelPriceUpdate{Provider: "openai", Model: "z-model", Enabled: true}); errCreate != nil {
		t.Fatalf("CreateBillingModelPrice(concurrent) error = %v", errCreate)
	}
	_, errApply := repo.ApplyBillingModelPriceImport(ctx, BillingModelPriceImportApplyInput{
		PreviewID:       preview.PreviewID,
		PreviewRevision: preview.PreviewRevision,
		SelectedKeys:    []string{preview.Rows[0].RowKey, preview.Rows[1].RowKey},
		IdempotencyKey:  "atomic-late-conflict",
	})
	if !errors.Is(errApply, ErrBillingImportRuleConflict) {
		t.Fatalf("ApplyBillingModelPriceImport() error = %v, want rule conflict", errApply)
	}
	rolledBack, errList := repo.ListBillingModelPrices(ctx, BillingModelPriceQuery{Provider: "openai", Model: "a-model"})
	if errList != nil {
		t.Fatalf("ListBillingModelPrices() error = %v", errList)
	}
	if len(rolledBack) != 0 {
		t.Fatalf("earlier imported rows were not rolled back: %#v", rolledBack)
	}
	db, errDB := repo.database()
	if errDB != nil {
		t.Fatalf("database() error = %v", errDB)
	}
	var operationCount int64
	if errCount := db.WithContext(ctx).Model(&BillingModelPriceImportOperationRecord{}).Where("idempotency_key = ?", "atomic-late-conflict").Count(&operationCount).Error; errCount != nil {
		t.Fatalf("count import operations: %v", errCount)
	}
	if operationCount != 0 {
		t.Fatalf("operation count = %d, want 0 after rollback", operationCount)
	}
}

func TestBillingModelPriceImportRejectsNonBaseTargetAndUnsafeContextBand(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	invalidTarget := billingImportTestInput("missing")
	invalidTarget.Targets[0].ServiceTier = "priority"
	if _, errPreview := repo.CreateBillingModelPriceImportPreview(ctx, invalidTarget, billingImportTestCatalog()); errPreview == nil {
		t.Fatal("CreateBillingModelPriceImportPreview() succeeded for non-base target")
	}
	unsafeCatalog := billingImportTestCatalog()
	unsafeCatalog.Models[0].ContextBands = []BillingModelPriceImportContextBand{{MinInputTokens: 272001, Cost: BillingModelPriceImportCost{Input: 4, Output: 16}, MissingPriceFields: []string{"cache_write"}}}
	preview, errPreview := repo.CreateBillingModelPriceImportPreview(ctx, billingImportTestInput("missing"), unsafeCatalog)
	if errPreview != nil {
		t.Fatalf("CreateBillingModelPriceImportPreview() error = %v", errPreview)
	}
	if len(preview.Rows) != 1 || preview.Rows[0].Status != "unsupported" || preview.Rows[0].Applicable || preview.Rows[0].Action != "review" {
		t.Fatalf("unsafe context preview = %#v", preview.Rows)
	}
	duplicateCatalog := billingImportTestCatalog()
	duplicateCatalog.Models[0].ContextBands = []BillingModelPriceImportContextBand{
		{MinInputTokens: 272001, Cost: BillingModelPriceImportCost{Input: 4, Output: 16}},
		{MinInputTokens: 272001, Cost: BillingModelPriceImportCost{Input: 5, Output: 20}},
	}
	duplicatePreview, errDuplicatePreview := repo.CreateBillingModelPriceImportPreview(ctx, billingImportTestInput("missing"), duplicateCatalog)
	if errDuplicatePreview != nil {
		t.Fatalf("CreateBillingModelPriceImportPreview() duplicate catalog error = %v", errDuplicatePreview)
	}
	if len(duplicatePreview.Rows) != 1 || duplicatePreview.Rows[0].Status != "invalid" || duplicatePreview.Rows[0].Applicable {
		t.Fatalf("duplicate context preview = %#v", duplicatePreview.Rows)
	}
}

func TestBillingModelPriceImportRejectsChangedRuleRevision(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	rule, errCreate := repo.CreateBillingModelPrice(ctx, BillingModelPriceUpdate{Provider: "openai", Model: "gpt-import", Source: BillingPriceSourceSync, Enabled: true})
	if errCreate != nil {
		t.Fatalf("CreateBillingModelPrice() error = %v", errCreate)
	}
	preview, errPreview := repo.CreateBillingModelPriceImportPreview(ctx, billingImportTestInput("sync"), billingImportTestCatalog())
	if errPreview != nil {
		t.Fatalf("CreateBillingModelPriceImportPreview() error = %v", errPreview)
	}
	note := "concurrent operator change"
	if _, errUpdate := repo.UpdateBillingModelPrice(ctx, rule.ID, BillingModelPricePatch{Note: &note}); errUpdate != nil {
		t.Fatalf("UpdateBillingModelPrice() error = %v", errUpdate)
	}
	_, errApply := repo.ApplyBillingModelPriceImport(ctx, BillingModelPriceImportApplyInput{PreviewID: preview.PreviewID, PreviewRevision: preview.PreviewRevision, SelectedKeys: []string{preview.Rows[0].RowKey}, IdempotencyKey: "conflict-key"})
	if !errors.Is(errApply, ErrBillingImportRuleConflict) {
		t.Fatalf("ApplyBillingModelPriceImport() error = %v, want revision conflict", errApply)
	}
}

func TestBillingModelPriceImportAppliesLargeBatchCreatesAndUpdates(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	const count = billingImportWriteBatchSize*2 + 7
	input := billingImportTestInput("missing")
	input.Targets = make([]BillingModelPriceImportTarget, 0, count)
	catalog := BillingModelPriceImportCatalog{SourceURL: "https://models.dev/api.json", Version: "fixture-v1", FetchedAt: time.Now().UTC(), Models: make([]BillingModelPriceImportCatalogModel, 0, count)}
	for index := 0; index < count; index++ {
		model := fmt.Sprintf("gpt-batch-%03d", index)
		input.Targets = append(input.Targets, BillingModelPriceImportTarget{Provider: "openai", Model: model})
		catalog.Models = append(catalog.Models, BillingModelPriceImportCatalogModel{Provider: "openai", Model: model, Cost: &BillingModelPriceImportCost{Input: 1, Output: 2, CacheRead: 0.25, CacheWrite: 1.5}})
	}
	preview, errPreview := repo.CreateBillingModelPriceImportPreview(ctx, input, catalog)
	if errPreview != nil {
		t.Fatalf("CreateBillingModelPriceImportPreview() error = %v", errPreview)
	}
	keys := billingImportPreviewRowKeys(preview.Rows)
	if _, errApply := repo.ApplyBillingModelPriceImport(ctx, BillingModelPriceImportApplyInput{PreviewID: preview.PreviewID, PreviewRevision: preview.PreviewRevision, SelectedKeys: keys, IdempotencyKey: "large-batch-create"}); errApply != nil {
		t.Fatalf("ApplyBillingModelPriceImport(create) error = %v", errApply)
	}
	rules, errRules := repo.ListBillingModelPrices(ctx, BillingModelPriceQuery{Provider: "openai"})
	if errRules != nil {
		t.Fatalf("ListBillingModelPrices() error = %v", errRules)
	}
	if len(rules) != count {
		t.Fatalf("created rule count = %d, want %d", len(rules), count)
	}

	input.Policy.OverwriteMode = "sync"
	for _, model := range catalog.Models {
		model.Cost.Input = 3
		model.Cost.Output = 6
		model.Cost.CacheRead = 0.75
		model.Cost.CacheWrite = 2.5
	}
	preview, errPreview = repo.CreateBillingModelPriceImportPreview(ctx, input, catalog)
	if errPreview != nil {
		t.Fatalf("CreateBillingModelPriceImportPreview(update) error = %v", errPreview)
	}
	keys = billingImportPreviewRowKeys(preview.Rows)
	if _, errApply := repo.ApplyBillingModelPriceImport(ctx, BillingModelPriceImportApplyInput{PreviewID: preview.PreviewID, PreviewRevision: preview.PreviewRevision, SelectedKeys: keys, IdempotencyKey: "large-batch-update"}); errApply != nil {
		t.Fatalf("ApplyBillingModelPriceImport(update) error = %v", errApply)
	}
	rules, errRules = repo.ListBillingModelPrices(ctx, BillingModelPriceQuery{Provider: "openai"})
	if errRules != nil {
		t.Fatalf("ListBillingModelPrices(after update) error = %v", errRules)
	}
	for _, rule := range rules {
		if rule.Revision != 2 || rule.InputPricePerMillion != 3 || rule.OutputPricePerMillion != 6 || rule.CacheReadPricePerMillion != 0.75 || rule.CacheWritePricePerMillion != 2.5 {
			t.Fatalf("updated rule = %#v, want batched prices and revision", rule)
		}
	}
}

func TestBillingChargeAmountKeepsZeroPricedOpenAICacheWritesInInput(t *testing.T) {
	t.Parallel()

	usage := &UsageRecord{Provider: "openai", ExecutorType: "OpenAICompatExecutor", InputTokens: 1000, CacheCreationTokens: 100}
	if amount := billingChargeAmount(usage, BillingPriceSnapshot{InputPricePerMillion: 1000}); amount != 1 {
		t.Fatalf("zero-price cache-write charge = %g, want 1", amount)
	}
}

func TestBillingTierDiagnosticsUsesOnlyEligibleRecentRequests(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	db, errDB := repo.database()
	if errDB != nil {
		t.Fatalf("database() error = %v", errDB)
	}
	now := time.Now().UTC()
	for _, record := range []UsageRecord{
		{Timestamp: now, ServiceTier: "standard", ResponseServiceTier: "priority", PayloadJSON: JSONB(`{}`), CreatedAt: now},
		{Timestamp: now, ServiceTier: "standard", PayloadJSON: JSONB(`{}`), CreatedAt: now},
		{Timestamp: now, PayloadJSON: JSONB(`{}`), CreatedAt: now},
		{Timestamp: now.Add(-31 * 24 * time.Hour), ServiceTier: "standard", PayloadJSON: JSONB(`{}`), CreatedAt: now},
	} {
		if errCreate := db.WithContext(ctx).Create(&record).Error; errCreate != nil {
			t.Fatalf("create usage record: %v", errCreate)
		}
	}
	diagnostics, errDiagnostics := repo.GetBillingTierDiagnostics(ctx)
	if errDiagnostics != nil {
		t.Fatalf("GetBillingTierDiagnostics() error = %v", errDiagnostics)
	}
	if !diagnostics.Supported || diagnostics.EligibleRequests != 2 || diagnostics.ResponseTierRequests != 1 || diagnostics.FallbackRequests != 1 || diagnostics.LastResponseTierAt == nil {
		t.Fatalf("diagnostics = %#v", diagnostics)
	}
}

func TestBillingTierDiagnosticsReturnsZeroCountsWithoutEligibleRequests(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	diagnostics, errDiagnostics := repo.GetBillingTierDiagnostics(ctx)
	if errDiagnostics != nil {
		t.Fatalf("GetBillingTierDiagnostics() error = %v", errDiagnostics)
	}
	if !diagnostics.Supported || diagnostics.EligibleRequests != 0 || diagnostics.ResponseTierRequests != 0 || diagnostics.FallbackRequests != 0 || diagnostics.LastResponseTierAt != nil {
		t.Fatalf("diagnostics = %#v, want supported zero-count result", diagnostics)
	}
}

func TestBillingModelPriceImportRejectsEmptyMultiplierMatchMode(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	input := billingImportTestInput("missing")
	input.Policy.MultiplierRules = []BillingModelPriceImportMultiplierRule{{Pattern: "gpt", Multiplier: 2}}
	if _, errPreview := repo.CreateBillingModelPriceImportPreview(ctx, input, billingImportTestCatalog()); errPreview == nil {
		t.Fatal("CreateBillingModelPriceImportPreview() succeeded with an empty multiplier match_mode")
	}
}

func TestBillingModelPriceImportPrefersFirstPartyProviderForKnownModelFamilies(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	catalog := BillingModelPriceImportCatalog{SourceURL: "https://models.dev/api.json", Version: "fixture-v1", FetchedAt: time.Now().UTC(), Models: []BillingModelPriceImportCatalogModel{
		{Provider: "openrouter", Model: "claude-sonnet-4-6", Cost: &BillingModelPriceImportCost{Input: 4, Output: 20}},
		{Provider: "anthropic", Model: "claude-sonnet-4-6", Cost: &BillingModelPriceImportCost{Input: 3, Output: 15}},
	}}
	input := billingImportTestInput("missing")
	input.Targets[0].Provider = "antigravity"
	input.Targets[0].Model = "claude-sonnet-4-6"
	preview, errPreview := repo.CreateBillingModelPriceImportPreview(ctx, input, catalog)
	if errPreview != nil {
		t.Fatalf("CreateBillingModelPriceImportPreview() error = %v", errPreview)
	}
	if len(preview.Rows) != 1 || preview.Rows[0].MatchedProvider != "anthropic" || !preview.Rows[0].Applicable {
		t.Fatalf("preview = %#v", preview.Rows)
	}
}

func TestBillingImportCatalogIndexMatchesReference(t *testing.T) {
	t.Parallel()

	models := []BillingModelPriceImportCatalogModel{
		{Provider: "catalog", Model: "override-first"},
		{Provider: "catalog", Model: "override-second"},
		{Provider: "openai", Model: "alias-full"},
		{Provider: "openai", Model: "alias-bare"},
		{Provider: "anthropic", Model: "claude-match"},
		{Provider: "other", Model: "claude-match"},
		{Provider: "vendor", Model: "nested/foo"},
		{Provider: " openai ", Model: "first-party-fallback"},
		{Provider: "openrouter", Model: "bare-fallback"},
		{Provider: "openai", Model: "duplicate"},
		{Provider: "openai", Model: "duplicate"},
		{Provider: "openai", Model: "ambiguous"},
		{Provider: "openai", Model: "tenant/ambiguous"},
		{Provider: "gateway", Model: "\u212a-model"},
	}
	aliases := []BillingModelPriceImportAlias{
		{TargetModel: "my/alias-target", SourceModels: []string{"alias-full"}},
		{TargetModel: "alias-target", SourceModels: []string{"alias-bare"}},
	}
	overrides := []BillingModelPriceImportMatchOverride{
		{TargetProvider: "gateway", TargetModel: "override", SourceProvider: "catalog", SourceModel: "override-first"},
		{TargetProvider: "GATEWAY", TargetModel: "OVERRIDE", SourceProvider: "catalog", SourceModel: "override-second"},
	}
	targets := []BillingModelPriceImportTarget{
		{Provider: "gateway", Model: "override"},
		{Provider: "gateway", Model: "my/alias-target"},
		{Provider: "gateway", Model: "claude-match"},
		{Provider: "vendor", Model: "nested/foo"},
		{Provider: "gateway", Model: "first-party-fallback"},
		{Provider: "gateway", Model: "bare-fallback"},
		{Provider: "gateway", Model: "duplicate"},
		{Provider: "gateway", Model: "ambiguous"},
		{Provider: "gateway", Model: "k-model"},
	}
	index := newBillingImportCatalogIndex(models)
	policy := newBillingImportMatchPolicyIndex(aliases, overrides)
	for _, target := range targets {
		t.Run(target.Provider+"/"+target.Model, func(t *testing.T) {
			want := billingImportCatalogMatchesReference(models, target, aliases, overrides)
			got := index.matches(target, policy)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("index.matches() = %#v, want %#v", got, want)
			}
			if gotWrapper := billingImportCatalogMatches(models, target, aliases, overrides); !reflect.DeepEqual(gotWrapper, want) {
				t.Fatalf("billingImportCatalogMatches() = %#v, want %#v", gotWrapper, want)
			}
		})
	}
}

func TestBuildBillingModelPriceImportPreviewHandlesMaximumIndexedCatalog(t *testing.T) {
	t.Parallel()

	const count = billingImportMaxTargets
	input := BillingModelPriceImportPreviewInput{
		Targets: make([]BillingModelPriceImportTarget, 0, count),
		Policy:  BillingModelPriceImportPolicy{OverwriteMode: "missing", DefaultMultiplier: 1},
	}
	catalog := BillingModelPriceImportCatalog{
		SourceURL: "https://models.dev/api.json",
		Version:   "large-fixture",
		FetchedAt: time.Date(2026, time.July, 13, 0, 0, 0, 0, time.UTC),
		Models:    make([]BillingModelPriceImportCatalogModel, 0, count),
	}
	for index := 0; index < count; index++ {
		model := fmt.Sprintf("model-%05d", index)
		input.Targets = append(input.Targets, BillingModelPriceImportTarget{Provider: "local", Model: model})
		catalog.Models = append(catalog.Models, BillingModelPriceImportCatalogModel{Provider: "local", Model: model, Cost: &BillingModelPriceImportCost{Input: 1, Output: 2}})
	}
	preview := buildBillingModelPriceImportPreview(input, catalog, nil, catalog.FetchedAt)
	if len(preview.Rows) != count || preview.Summary.Total != count || preview.Summary.Creates != count {
		t.Fatalf("preview summary = %#v rows=%d, want %d indexed creates", preview.Summary, len(preview.Rows), count)
	}
	for _, row := range []BillingModelPriceImportPreviewRow{preview.Rows[0], preview.Rows[len(preview.Rows)-1]} {
		if !row.Applicable || row.Action != "create" || row.MatchedProvider != "local" || row.Final == nil || row.Final.Input != 1 || row.Final.Output != 2 {
			t.Fatalf("preview row = %#v, want indexed catalog match", row)
		}
	}
}

func billingImportCatalogMatchesReference(models []BillingModelPriceImportCatalogModel, target BillingModelPriceImportTarget, aliases []BillingModelPriceImportAlias, overrides []BillingModelPriceImportMatchOverride) []BillingModelPriceImportCatalogModel {
	for _, override := range overrides {
		if strings.EqualFold(strings.TrimSpace(override.TargetProvider), strings.TrimSpace(target.Provider)) && strings.EqualFold(strings.TrimSpace(override.TargetModel), strings.TrimSpace(target.Model)) {
			for _, model := range models {
				if strings.EqualFold(model.Provider, override.SourceProvider) && strings.EqualFold(model.Model, override.SourceModel) {
					return []BillingModelPriceImportCatalogModel{model}
				}
			}
			return nil
		}
	}
	candidates := billingImportModelCandidatesReference(target.Model, aliases)
	preferredProviders := billingImportPreferredProviders(target.Provider, target.Model)
	for _, candidate := range candidates {
		for _, provider := range preferredProviders {
			matches := billingImportModelsForProviderCandidateReference(models, provider, candidate)
			if len(matches) > 0 {
				return matches
			}
		}
	}
	for _, candidate := range candidates {
		var firstParty []BillingModelPriceImportCatalogModel
		for _, model := range models {
			if !strings.EqualFold(billingImportBareModel(model.Model), billingImportBareModel(candidate)) || !billingImportFirstPartyProvider(model.Provider) {
				continue
			}
			firstParty = append(firstParty, model)
		}
		if len(firstParty) == 1 {
			return firstParty
		}
	}

	var exact, bare []BillingModelPriceImportCatalogModel
	for _, model := range models {
		for _, candidate := range candidates {
			if strings.EqualFold(model.Provider, target.Provider) && strings.EqualFold(model.Model, candidate) {
				exact = append(exact, model)
			}
			if strings.EqualFold(billingImportBareModel(model.Model), billingImportBareModel(candidate)) {
				bare = append(bare, model)
			}
		}
	}
	if len(exact) > 0 {
		return billingImportUniqueCatalogMatches(exact)
	}
	return billingImportUniqueCatalogMatches(bare)
}

func billingImportModelsForProviderCandidateReference(models []BillingModelPriceImportCatalogModel, provider string, candidate string) []BillingModelPriceImportCatalogModel {
	var matches []BillingModelPriceImportCatalogModel
	for _, model := range models {
		if strings.EqualFold(model.Provider, provider) && (strings.EqualFold(model.Model, candidate) || strings.EqualFold(billingImportBareModel(model.Model), billingImportBareModel(candidate))) {
			matches = append(matches, model)
		}
	}
	return billingImportUniqueCatalogMatches(matches)
}

func billingImportModelCandidatesReference(model string, aliases []BillingModelPriceImportAlias) []string {
	model = strings.TrimSpace(model)
	bare := billingImportBareModel(model)
	values := []string{bare, model}
	for _, alias := range aliases {
		if strings.EqualFold(strings.TrimSpace(alias.TargetModel), bare) || strings.EqualFold(strings.TrimSpace(alias.TargetModel), model) {
			values = append(values, alias.SourceModels...)
		}
	}
	for _, suffix := range []string{"-thinking", "-preview", "-latest"} {
		if strings.HasSuffix(strings.ToLower(bare), suffix) {
			values = append(values, bare[:len(bare)-len(suffix)])
		}
	}
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		key := strings.ToLower(value)
		if value == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, value)
	}
	return result
}

func billingImportTestInput(overwriteMode string) BillingModelPriceImportPreviewInput {
	return BillingModelPriceImportPreviewInput{Source: BillingModelPriceImportSourceModelsDev, Targets: []BillingModelPriceImportTarget{{Provider: "openai", Model: "gpt-import", Label: "GPT Import"}}, Policy: BillingModelPriceImportPolicy{OverwriteMode: overwriteMode, DefaultMultiplier: 1}}
}

func billingImportPreviewRowKeys(rows []BillingModelPriceImportPreviewRow) []string {
	keys := make([]string, 0, len(rows))
	for _, row := range rows {
		if row.Applicable {
			keys = append(keys, row.RowKey)
		}
	}
	return keys
}

func billingImportTestCatalog() BillingModelPriceImportCatalog {
	return BillingModelPriceImportCatalog{SourceURL: "https://models.dev/api.json", Version: "fixture-v1", FetchedAt: time.Date(2026, time.July, 11, 0, 0, 0, 0, time.UTC), Models: []BillingModelPriceImportCatalogModel{{Provider: "openai", Model: "gpt-import", Name: "GPT Import", Cost: &BillingModelPriceImportCost{Input: 2, Output: 8, CacheWrite: 0}}}}
}
