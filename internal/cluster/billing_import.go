package cluster

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"gorm.io/gorm"
)

const (
	BillingModelPriceImportSourceModelsDev  = "models.dev"
	BillingModelPriceImportSourceOpenRouter = "openrouter"
	BillingModelPriceImportSourceLiteLLM    = "litellm"
	BillingModelPriceImportSourceAuto       = "auto"
	billingImportPreviewTTL                 = 15 * time.Minute
	billingImportPreviewRetention           = 24 * time.Hour
	billingImportOperationRetention         = 30 * 24 * time.Hour
	billingTierDiagnosticsWindow            = 30 * 24 * time.Hour
	billingImportMaxTargets                 = 10000
	billingImportMaxPolicyEntries           = 10000
	billingImportMaxSelectedKeys            = 10000
	billingImportWriteBatchSize             = 50
)

var (
	ErrBillingImportPreviewNotFound     = errors.New("billing import preview not found")
	ErrBillingImportPreviewExpired      = errors.New("billing import preview expired")
	ErrBillingImportPreviewStale        = errors.New("billing import preview stale")
	ErrBillingImportRuleConflict        = errors.New("billing import rule conflict")
	ErrBillingImportInvalidSelection    = errors.New("billing import invalid selection")
	ErrBillingImportOverwriteRequired   = errors.New("billing import overwrite confirmation required")
	ErrBillingImportIdempotencyConflict = errors.New("billing import idempotency key was reused with a different request")
	ErrBillingImportOperationNotFound   = errors.New("billing import operation not found")
)

// BillingModelPriceImportPreviewRecord stores a server-generated import plan.
type BillingModelPriceImportPreviewRecord struct {
	ID               string    `gorm:"column:id;primaryKey"`
	Revision         string    `gorm:"column:revision;not null;uniqueIndex"`
	Source           string    `gorm:"column:source;not null"`
	SourceURL        string    `gorm:"column:source_url;not null"`
	SourceVersion    string    `gorm:"column:source_version;not null"`
	SourceFetchedAt  time.Time `gorm:"column:source_fetched_at;not null"`
	SourceModelCount int       `gorm:"column:source_model_count;not null"`
	Atomic           bool      `gorm:"column:atomic;not null;default:true"`
	GeneratedAt      time.Time `gorm:"column:generated_at;not null"`
	ExpiresAt        time.Time `gorm:"column:expires_at;not null;index"`
	Payload          JSONB     `gorm:"column:payload;not null"`
	CreatedAt        time.Time `gorm:"column:created_at;not null;index"`
}

func (BillingModelPriceImportPreviewRecord) TableName() string {
	return "billing_model_price_import_preview"
}

// BillingModelPriceImportOperationRecord persists terminal apply results for idempotent recovery.
type BillingModelPriceImportOperationRecord struct {
	ID              string    `gorm:"column:id;primaryKey"`
	PreviewID       string    `gorm:"column:preview_id;not null;index"`
	PreviewRevision string    `gorm:"column:preview_revision;not null"`
	IdempotencyKey  string    `gorm:"column:idempotency_key;not null;uniqueIndex"`
	RequestHash     string    `gorm:"column:request_hash;not null"`
	SelectionHash   string    `gorm:"column:selection_hash;not null;index"`
	Atomic          bool      `gorm:"column:atomic;not null"`
	Status          string    `gorm:"column:status;not null"`
	AppliedAt       time.Time `gorm:"column:applied_at;not null"`
	Result          JSONB     `gorm:"column:result;not null"`
	CreatedAt       time.Time `gorm:"column:created_at;not null;index"`
}

func (BillingModelPriceImportOperationRecord) TableName() string {
	return "billing_model_price_import_operation"
}

type BillingModelPriceImportTarget struct {
	Provider       string   `json:"provider"`
	Model          string   `json:"model"`
	ServiceTier    string   `json:"service_tier"`
	MinInputTokens int64    `json:"min_input_tokens"`
	Label          string   `json:"label"`
	Scopes         []string `json:"scopes"`
}

type BillingModelPriceImportMultiplierRule struct {
	ID         string  `json:"id"`
	Label      string  `json:"label"`
	MatchMode  string  `json:"match_mode"`
	Pattern    string  `json:"pattern"`
	Multiplier float64 `json:"multiplier"`
}

type BillingModelPriceImportAlias struct {
	TargetModel  string   `json:"target_model"`
	SourceModels []string `json:"source_models"`
}

type BillingModelPriceImportPolicy struct {
	OverwriteMode     string                                  `json:"overwrite_mode"`
	DefaultMultiplier float64                                 `json:"default_multiplier"`
	MultiplierRules   []BillingModelPriceImportMultiplierRule `json:"multiplier_rules"`
	Aliases           []BillingModelPriceImportAlias          `json:"aliases"`
	RowMultipliers    map[string]float64                      `json:"row_multipliers"`
	IncludeZeroCost   bool                                    `json:"include_zero_cost"`
}

type BillingModelPriceImportMatchOverride struct {
	TargetProvider string `json:"target_provider"`
	TargetModel    string `json:"target_model"`
	SourceProvider string `json:"source_provider"`
	SourceModel    string `json:"source_model"`
}

type BillingModelPriceImportPreviewInput struct {
	Source         string                                 `json:"source"`
	AutoDiscover   bool                                   `json:"auto_discover,omitempty"`
	CatalogJSON    json.RawMessage                        `json:"catalog_json,omitempty"`
	Targets        []BillingModelPriceImportTarget        `json:"targets"`
	Policy         BillingModelPriceImportPolicy          `json:"policy"`
	MatchOverrides []BillingModelPriceImportMatchOverride `json:"match_overrides"`
}

type BillingModelPriceImportCost struct {
	Input         float64  `json:"input"`
	Output        float64  `json:"output"`
	CacheRead     float64  `json:"cache_read"`
	CacheWrite    float64  `json:"cache_write"`
	Request       float64  `json:"request"`
	CacheWrite30m *float64 `json:"cache_write_30m,omitempty"`
	CacheWrite1h  *float64 `json:"cache_write_1h,omitempty"`
	CacheWrite12h *float64 `json:"cache_write_12h,omitempty"`
	Reasoning     *float64 `json:"reasoning,omitempty"`
	ImageInput    *float64 `json:"image_input,omitempty"`
	ImageOutput   *float64 `json:"image_output,omitempty"`
	AudioInput    *float64 `json:"audio_input,omitempty"`
	AudioOutput   *float64 `json:"audio_output,omitempty"`
	WebSearch     *float64 `json:"web_search,omitempty"`
	Image         *float64 `json:"image,omitempty"`
	PricingExpr   string   `json:"pricing_expr,omitempty"`
}

type BillingModelPriceImportContextBand struct {
	MinInputTokens        int64                       `json:"min_input_tokens"`
	Cost                  BillingModelPriceImportCost `json:"cost"`
	InvalidPriceFields    []string                    `json:"-"`
	UnsupportedDimensions []string                    `json:"-"`
	MissingPriceFields    []string                    `json:"-"`
}

type BillingModelPriceImportCatalogModel struct {
	Provider              string                               `json:"provider"`
	Model                 string                               `json:"model"`
	Name                  string                               `json:"name"`
	Cost                  *BillingModelPriceImportCost         `json:"cost"`
	ContextBands          []BillingModelPriceImportContextBand `json:"context_bands"`
	InvalidPriceFields    []string                             `json:"invalid_price_fields"`
	UnsupportedDimensions []string                             `json:"unsupported_dimensions"`
	ContextBandIssues     []string                             `json:"-"`
}

type BillingModelPriceImportCatalog struct {
	SourceURL string                                `json:"source_url"`
	Version   string                                `json:"version"`
	FetchedAt time.Time                             `json:"fetched_at"`
	Models    []BillingModelPriceImportCatalogModel `json:"models"`
}

// billingImportCatalogIndex keeps the catalog's original ordering while making
// repeated preview matching independent of the catalog size.
type billingImportCatalogIndex struct {
	models       []BillingModelPriceImportCatalogModel
	exact        map[string][]int
	providerBare map[string][]int
	bare         map[string][]int
}

// billingImportMatchPolicyIndex avoids rescanning aliases and overrides for
// every preview target. Slices keep their request order because that order is
// part of the matching policy.
type billingImportMatchPolicyIndex struct {
	aliases           []BillingModelPriceImportAlias
	aliasesByTarget   map[string][]int
	overrides         []BillingModelPriceImportMatchOverride
	overridesByTarget map[string][]int
}

type BillingModelPriceImportRuleSnapshot struct {
	ID                        string  `json:"id"`
	Provider                  string  `json:"provider"`
	Model                     string  `json:"model"`
	ServiceTier               string  `json:"service_tier"`
	MinInputTokens            int64   `json:"min_input_tokens"`
	InputPricePerMillion      float64 `json:"input_price_per_million"`
	OutputPricePerMillion     float64 `json:"output_price_per_million"`
	CacheReadPricePerMillion  float64 `json:"cache_read_price_per_million"`
	CacheWritePricePerMillion float64 `json:"cache_write_price_per_million"`
	RequestPrice              float64 `json:"request_price"`
	Source                    string  `json:"source"`
	Enabled                   bool    `json:"enabled"`
	Note                      string  `json:"note"`
	Revision                  string  `json:"revision"`
}

type BillingModelPriceImportWriteRule struct {
	Source         string `json:"source"`
	Enabled        bool   `json:"enabled"`
	Note           string `json:"note"`
	ServiceTier    string `json:"service_tier"`
	MinInputTokens int64  `json:"min_input_tokens"`
}

type BillingModelPriceImportReason struct {
	Code       string   `json:"code"`
	Message    string   `json:"message"`
	Candidates []string `json:"candidates,omitempty"`
}

type BillingModelPriceImportPreviewRow struct {
	RowKey          string                               `json:"row_key"`
	Provider        string                               `json:"provider"`
	Model           string                               `json:"model"`
	ServiceTier     string                               `json:"service_tier"`
	MinInputTokens  int64                                `json:"min_input_tokens"`
	Label           string                               `json:"label"`
	Status          string                               `json:"status"`
	Action          string                               `json:"action"`
	Applicable      bool                                 `json:"applicable"`
	SelectedDefault bool                                 `json:"selected_by_default"`
	MatchedProvider string                               `json:"matched_provider,omitempty"`
	MatchedModel    string                               `json:"matched_model,omitempty"`
	MatchedName     string                               `json:"matched_name,omitempty"`
	Official        *BillingModelPriceImportCost         `json:"official,omitempty"`
	Multiplier      float64                              `json:"multiplier"`
	Final           *BillingModelPriceImportCost         `json:"final,omitempty"`
	WriteRule       *BillingModelPriceImportWriteRule    `json:"write_rule,omitempty"`
	ExistingRule    *BillingModelPriceImportRuleSnapshot `json:"existing_rule,omitempty"`
	Reasons         []BillingModelPriceImportReason      `json:"reasons"`
}

type BillingModelPriceImportSummary struct {
	Total       int `json:"total"`
	Creates     int `json:"creates"`
	UpdatesSync int `json:"updates_sync"`
	Overwrites  int `json:"overwrites"`
	Skips       int `json:"skips"`
	Reviews     int `json:"reviews"`
	Conflicts   int `json:"conflicts"`
	Failed      int `json:"failed"`
}

type BillingModelPriceImportPreview struct {
	PreviewID        string                              `json:"preview_id"`
	PreviewRevision  string                              `json:"preview_revision"`
	Source           string                              `json:"source"`
	SourceURL        string                              `json:"source_url"`
	SourceVersion    string                              `json:"source_version"`
	SourceFetchedAt  time.Time                           `json:"source_fetched_at"`
	SourceModelCount int                                 `json:"source_model_count"`
	GeneratedAt      time.Time                           `json:"generated_at"`
	ExpiresAt        time.Time                           `json:"expires_at"`
	Atomic           bool                                `json:"atomic"`
	Rows             []BillingModelPriceImportPreviewRow `json:"rows"`
	Summary          BillingModelPriceImportSummary      `json:"summary"`
}

type BillingModelPriceImportApplyInput struct {
	PreviewID        string   `json:"preview_id"`
	PreviewRevision  string   `json:"preview_revision"`
	SelectedKeys     []string `json:"selected_keys"`
	ConfirmOverwrite bool     `json:"confirm_overwrite"`
	IdempotencyKey   string   `json:"idempotency_key"`
}

type BillingModelPriceImportOperationRowResult struct {
	Key          string `json:"key"`
	Provider     string `json:"provider"`
	Model        string `json:"model"`
	Action       string `json:"action"`
	Status       string `json:"status"`
	ResourceID   string `json:"resource_id,omitempty"`
	ErrorCode    string `json:"error_code,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`
}

type BillingModelPriceImportOperation struct {
	OperationID string                                      `json:"operation_id"`
	PreviewID   string                                      `json:"preview_id"`
	Status      string                                      `json:"status"`
	Atomic      bool                                        `json:"atomic"`
	AppliedAt   time.Time                                   `json:"applied_at"`
	Summary     BillingModelPriceImportSummary              `json:"summary"`
	Rows        []BillingModelPriceImportOperationRowResult `json:"rows"`
}

type BillingTierDiagnostics struct {
	Supported            bool       `json:"supported"`
	WindowStart          *time.Time `json:"window_start"`
	WindowEnd            *time.Time `json:"window_end"`
	EligibleRequests     int64      `json:"eligible_requests"`
	ResponseTierRequests int64      `json:"response_tier_requests"`
	FallbackRequests     int64      `json:"fallback_requests"`
	LastResponseTierAt   *time.Time `json:"last_response_tier_at,omitempty"`
}

func (r *Repository) CreateBillingModelPriceImportPreview(ctx context.Context, input BillingModelPriceImportPreviewInput, catalog BillingModelPriceImportCatalog) (*BillingModelPriceImportPreview, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	ctx = contextOrBackground(ctx)
	input = normalizeBillingModelPriceImportPreviewInput(input)
	if len(input.Targets) == 0 && input.AutoDiscover {
		discovered, errDiscover := r.discoverBillingImportTargets(ctx, db)
		if errDiscover != nil {
			return nil, errDiscover
		}
		input.Targets = discovered
		input = normalizeBillingModelPriceImportPreviewInput(input)
	}
	if errValidate := validateBillingModelPriceImportPreviewInput(input, catalog); errValidate != nil {
		return nil, errValidate
	}
	now := time.Now().UTC()
	if errCleanup := r.cleanupBillingModelPriceImports(ctx, db, now); errCleanup != nil {
		return nil, errCleanup
	}
	var existing []BillingModelPriceRecord
	if errFind := db.WithContext(ctx).Find(&existing).Error; errFind != nil {
		return nil, errFind
	}
	preview := buildBillingModelPriceImportPreview(input, catalog, existing, now)
	payload, errPayload := billingJSONB(preview)
	if errPayload != nil {
		return nil, errPayload
	}
	record := BillingModelPriceImportPreviewRecord{
		ID:               preview.PreviewID,
		Revision:         preview.PreviewRevision,
		Source:           preview.Source,
		SourceURL:        preview.SourceURL,
		SourceVersion:    preview.SourceVersion,
		SourceFetchedAt:  preview.SourceFetchedAt,
		SourceModelCount: preview.SourceModelCount,
		Atomic:           preview.Atomic,
		GeneratedAt:      preview.GeneratedAt,
		ExpiresAt:        preview.ExpiresAt,
		Payload:          payload,
		CreatedAt:        now,
	}
	if errCreate := db.WithContext(ctx).Create(&record).Error; errCreate != nil {
		return nil, errCreate
	}
	return &preview, nil
}

// ValidateBillingModelPriceImportPreviewInput validates request-controlled import data
// before the models.dev catalog is fetched.
func ValidateBillingModelPriceImportPreviewInput(input BillingModelPriceImportPreviewInput) error {
	return validateBillingModelPriceImportInput(normalizeBillingModelPriceImportPreviewInput(input))
}

func normalizeBillingModelPriceImportPreviewInput(input BillingModelPriceImportPreviewInput) BillingModelPriceImportPreviewInput {
	input.Source = strings.ToLower(strings.TrimSpace(input.Source))
	if input.Source == "" {
		input.Source = BillingModelPriceImportSourceModelsDev
	}
	input.Policy.OverwriteMode = strings.ToLower(strings.TrimSpace(input.Policy.OverwriteMode))
	if input.Policy.OverwriteMode == "" {
		input.Policy.OverwriteMode = "missing"
	}
	if input.Policy.DefaultMultiplier == 0 {
		input.Policy.DefaultMultiplier = 1
	}
	for index := range input.Targets {
		input.Targets[index].Provider = strings.ToLower(strings.TrimSpace(input.Targets[index].Provider))
		input.Targets[index].Model = strings.TrimSpace(input.Targets[index].Model)
		input.Targets[index].ServiceTier = normalizeBillingPriceServiceTier(input.Targets[index].ServiceTier)
		input.Targets[index].Label = strings.TrimSpace(input.Targets[index].Label)
		for scopeIndex := range input.Targets[index].Scopes {
			input.Targets[index].Scopes[scopeIndex] = strings.ToLower(strings.TrimSpace(input.Targets[index].Scopes[scopeIndex]))
		}
	}
	for index := range input.MatchOverrides {
		override := &input.MatchOverrides[index]
		override.TargetProvider = strings.ToLower(strings.TrimSpace(override.TargetProvider))
		override.TargetModel = strings.TrimSpace(override.TargetModel)
		override.SourceProvider = strings.ToLower(strings.TrimSpace(override.SourceProvider))
		override.SourceModel = strings.TrimSpace(override.SourceModel)
	}
	return input
}

func (r *Repository) ApplyBillingModelPriceImport(ctx context.Context, input BillingModelPriceImportApplyInput) (*BillingModelPriceImportOperation, error) {
	input.PreviewID = strings.TrimSpace(input.PreviewID)
	input.PreviewRevision = strings.TrimSpace(input.PreviewRevision)
	input.IdempotencyKey = strings.TrimSpace(input.IdempotencyKey)
	if input.PreviewID == "" || input.PreviewRevision == "" || input.IdempotencyKey == "" || len(input.SelectedKeys) == 0 || len(input.SelectedKeys) > billingImportMaxSelectedKeys {
		return nil, ErrBillingImportInvalidSelection
	}
	selected := normalizedBillingImportSelectedKeys(input.SelectedKeys)
	if len(selected) != len(input.SelectedKeys) {
		return nil, ErrBillingImportInvalidSelection
	}
	input.SelectedKeys = selected
	requestHash := billingImportHash(input.PreviewID, input.PreviewRevision, strings.Join(selected, "\n"), fmt.Sprintf("%t", input.ConfirmOverwrite))
	selectionHash := billingImportHash(input.PreviewID, input.PreviewRevision, strings.Join(selected, "\n"))
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	ctx = contextOrBackground(ctx)
	var operation BillingModelPriceImportOperation
	errTransaction := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var existingOperation BillingModelPriceImportOperationRecord
		errExisting := tx.WithContext(ctx).Where("idempotency_key = ?", input.IdempotencyKey).First(&existingOperation).Error
		if errExisting == nil {
			if existingOperation.RequestHash != requestHash {
				return ErrBillingImportIdempotencyConflict
			}
			return unmarshalBillingImportOperation(existingOperation.Result, &operation)
		}
		if !errors.Is(errExisting, gorm.ErrRecordNotFound) {
			return errExisting
		}

		var previewRecord BillingModelPriceImportPreviewRecord
		if errPreview := tx.WithContext(ctx).First(&previewRecord, "id = ?", input.PreviewID).Error; errPreview != nil {
			if errors.Is(errPreview, gorm.ErrRecordNotFound) {
				return ErrBillingImportPreviewNotFound
			}
			return errPreview
		}
		if previewRecord.Revision != input.PreviewRevision {
			return ErrBillingImportPreviewStale
		}
		if time.Now().UTC().After(previewRecord.ExpiresAt) {
			return ErrBillingImportPreviewExpired
		}
		var preview BillingModelPriceImportPreview
		if errPreview := json.Unmarshal(previewRecord.Payload, &preview); errPreview != nil {
			return fmt.Errorf("decode billing import preview: %w", errPreview)
		}
		rowsByKey := make(map[string]BillingModelPriceImportPreviewRow, len(preview.Rows))
		for _, row := range preview.Rows {
			rowsByKey[row.RowKey] = row
		}
		selectedRows := make([]BillingModelPriceImportPreviewRow, 0, len(selected))
		expectedRevisions := make(map[string]int64, len(selected))
		for _, key := range selected {
			row, ok := rowsByKey[key]
			if !ok || !row.Applicable || row.Final == nil || row.WriteRule == nil {
				return ErrBillingImportInvalidSelection
			}
			if row.Action == "overwrite" && !input.ConfirmOverwrite {
				return ErrBillingImportOverwriteRequired
			}
			if row.ExistingRule != nil {
				expectedRevision, errRevision := strconv.ParseInt(row.ExistingRule.Revision, 10, 64)
				if errRevision != nil {
					return ErrBillingImportRuleConflict
				}
				if _, exists := expectedRevisions[row.ExistingRule.ID]; exists {
					return ErrBillingImportRuleConflict
				}
				expectedRevisions[row.ExistingRule.ID] = expectedRevision
			}
			selectedRows = append(selectedRows, row)
		}
		now := time.Now().UTC()
		operation = BillingModelPriceImportOperation{OperationID: billingID("import"), PreviewID: preview.PreviewID, Status: "applied", Atomic: preview.Atomic, AppliedAt: now}
		resourceIDs, errApply := applyBillingModelPriceImportRows(ctx, tx, selectedRows, expectedRevisions, now)
		if errApply != nil {
			if isBillingModelPriceConflict(errApply) {
				return ErrBillingImportRuleConflict
			}
			return errApply
		}
		for _, row := range selectedRows {
			resultStatus := map[string]string{"create": "created", "update_sync": "updated", "overwrite": "overwritten"}[row.Action]
			operation.Rows = append(operation.Rows, BillingModelPriceImportOperationRowResult{Key: row.RowKey, Provider: row.Provider, Model: row.Model, Action: row.Action, Status: resultStatus, ResourceID: resourceIDs[row.RowKey]})
			incrementBillingImportSummary(&operation.Summary, row.Action, row.Status)
		}
		result, errResult := billingJSONB(operation)
		if errResult != nil {
			return errResult
		}
		record := BillingModelPriceImportOperationRecord{ID: operation.OperationID, PreviewID: preview.PreviewID, PreviewRevision: preview.PreviewRevision, IdempotencyKey: input.IdempotencyKey, RequestHash: requestHash, SelectionHash: selectionHash, Atomic: preview.Atomic, Status: operation.Status, AppliedAt: now, Result: result, CreatedAt: now}
		if errCreate := tx.WithContext(ctx).Create(&record).Error; errCreate != nil {
			if isBillingModelPriceConflict(errCreate) {
				return ErrBillingImportRuleConflict
			}
			return errCreate
		}
		return nil
	})
	if errTransaction != nil {
		if replay, errReplay := r.replayBillingModelPriceImport(ctx, db, input.IdempotencyKey, requestHash); replay != nil || errReplay != nil {
			return replay, errReplay
		}
		return nil, errTransaction
	}
	return &operation, nil
}

func (r *Repository) replayBillingModelPriceImport(ctx context.Context, db *gorm.DB, idempotencyKey, requestHash string) (*BillingModelPriceImportOperation, error) {
	var record BillingModelPriceImportOperationRecord
	if errFind := db.WithContext(ctx).Where("idempotency_key = ?", idempotencyKey).First(&record).Error; errFind != nil {
		if errors.Is(errFind, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, errFind
	}
	if record.RequestHash != requestHash {
		return nil, ErrBillingImportIdempotencyConflict
	}
	operation := &BillingModelPriceImportOperation{}
	if errDecode := unmarshalBillingImportOperation(record.Result, operation); errDecode != nil {
		return nil, errDecode
	}
	return operation, nil
}

func (r *Repository) GetBillingModelPriceImportOperation(ctx context.Context, id string) (*BillingModelPriceImportOperation, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	var record BillingModelPriceImportOperationRecord
	if errFind := db.WithContext(contextOrBackground(ctx)).First(&record, "id = ?", strings.TrimSpace(id)).Error; errFind != nil {
		if errors.Is(errFind, gorm.ErrRecordNotFound) {
			return nil, ErrBillingImportOperationNotFound
		}
		return nil, errFind
	}
	operation := &BillingModelPriceImportOperation{}
	if errDecode := unmarshalBillingImportOperation(record.Result, operation); errDecode != nil {
		return nil, errDecode
	}
	return operation, nil
}

func (r *Repository) GetBillingTierDiagnostics(ctx context.Context) (BillingTierDiagnostics, error) {
	db, errDB := r.database()
	if errDB != nil {
		return BillingTierDiagnostics{}, errDB
	}
	ctx = contextOrBackground(ctx)
	var result struct {
		Eligible int64
		Present  int64
		Fallback int64
	}
	windowEnd := time.Now().UTC()
	windowStart := windowEnd.Add(-billingTierDiagnosticsWindow)
	tierPresent := "service_tier IS NOT NULL AND service_tier <> ''"
	errQuery := db.WithContext(ctx).Model(&UsageRecord{}).Where("timestamp >= ? AND "+tierPresent, windowStart).Select(`
		COUNT(*) AS eligible,
		COALESCE(SUM(CASE WHEN response_service_tier IS NOT NULL AND response_service_tier <> '' THEN 1 ELSE 0 END), 0) AS present,
		COALESCE(SUM(CASE WHEN response_service_tier IS NULL OR response_service_tier = '' THEN 1 ELSE 0 END), 0) AS fallback`).Scan(&result).Error
	if errQuery != nil {
		return BillingTierDiagnostics{}, errQuery
	}
	var lastRecord UsageRecord
	lastResult := db.WithContext(ctx).Where("timestamp >= ? AND "+tierPresent+" AND response_service_tier IS NOT NULL AND response_service_tier <> ''", windowStart).Order("timestamp DESC, id DESC").Limit(1).Find(&lastRecord)
	if lastResult.Error != nil {
		return BillingTierDiagnostics{}, lastResult.Error
	}
	var lastResponseTierAt *time.Time
	if lastResult.RowsAffected > 0 {
		value := lastRecord.Timestamp.UTC()
		lastResponseTierAt = &value
	}
	return BillingTierDiagnostics{Supported: true, WindowStart: &windowStart, WindowEnd: &windowEnd, EligibleRequests: result.Eligible, ResponseTierRequests: result.Present, FallbackRequests: result.Fallback, LastResponseTierAt: lastResponseTierAt}, nil
}

func buildBillingModelPriceImportPreview(input BillingModelPriceImportPreviewInput, catalog BillingModelPriceImportCatalog, existing []BillingModelPriceRecord, now time.Time) BillingModelPriceImportPreview {
	source := input.Source
	if source == "" {
		source = BillingModelPriceImportSourceModelsDev
	}
	preview := BillingModelPriceImportPreview{PreviewID: billingID("preview"), PreviewRevision: billingID("previewrev"), Source: source, SourceURL: catalog.SourceURL, SourceVersion: catalog.Version, SourceFetchedAt: catalog.FetchedAt.UTC(), SourceModelCount: len(catalog.Models), GeneratedAt: now, ExpiresAt: now.Add(billingImportPreviewTTL), Atomic: true}
	compiledMultiplierRules := billingImportCompileMultiplierRules(input.Policy.MultiplierRules)
	catalogIndex := newBillingImportCatalogIndex(catalog.Models)
	policyIndex := newBillingImportMatchPolicyIndex(input.Policy.Aliases, input.MatchOverrides)
	existingByIdentity := make(map[string][]BillingModelPriceRecord)
	for _, rule := range existing {
		key := billingImportRuleIdentity(rule.Provider, rule.Model, rule.ServiceTier, rule.MinInputTokens)
		existingByIdentity[key] = append(existingByIdentity[key], rule)
	}
	for _, target := range input.Targets {
		baseKey := billingImportTargetKey(target.Provider, target.Model)
		multiplier := billingImportMultiplier(input.Policy, compiledMultiplierRules, target.Model, baseKey)
		matches := catalogIndex.matches(target, policyIndex)
		if len(matches) == 0 {
			preview.Rows = append(preview.Rows, billingImportReviewRow(baseKey, target, multiplier, "unmatched", "no_models_dev_match", nil))
			continue
		}
		if len(matches) > 1 {
			candidates := make([]string, 0, len(matches))
			for _, match := range matches {
				candidates = append(candidates, match.Provider+"/"+match.Model)
			}
			preview.Rows = append(preview.Rows, billingImportReviewRow(baseKey, target, multiplier, "ambiguous", "multiple_matches", candidates))
			continue
		}
		match := matches[0]
		if len(match.InvalidPriceFields) > 0 {
			preview.Rows = append(preview.Rows, billingImportReviewRow(baseKey, target, multiplier, "invalid", "invalid_price_values", nil))
			continue
		}
		if len(match.UnsupportedDimensions) > 0 {
			preview.Rows = append(preview.Rows, billingImportReviewRow(baseKey, target, multiplier, "unsupported", "unsupported_price_dimensions", match.UnsupportedDimensions))
			continue
		}
		if match.Cost == nil {
			preview.Rows = append(preview.Rows, billingImportReviewRow(baseKey, target, multiplier, "unmatched", "missing_cost", nil))
			continue
		}
		if len(match.ContextBandIssues) > 0 {
			preview.Rows = append(preview.Rows, billingImportReviewRow(baseKey, target, multiplier, "invalid", "invalid_context_bands", match.ContextBandIssues))
			continue
		}
		if status, code, candidates := billingImportContextBandValidation(match.ContextBands); status != "" {
			preview.Rows = append(preview.Rows, billingImportReviewRow(baseKey, target, multiplier, status, code, candidates))
			continue
		}
		bands := append([]BillingModelPriceImportContextBand{{MinInputTokens: 0, Cost: *match.Cost}}, match.ContextBands...)
		sort.Slice(bands, func(i, j int) bool { return bands[i].MinInputTokens < bands[j].MinInputTokens })
		for index, band := range bands {
			rowKey := baseKey
			if index > 0 {
				rowKey = fmt.Sprintf("%s::*::%d", baseKey, band.MinInputTokens)
			}
			rowMultiplier := billingImportMultiplier(input.Policy, compiledMultiplierRules, target.Model, rowKey)
			identity := billingImportRuleIdentity(target.Provider, target.Model, BillingServiceTierWildcard, band.MinInputTokens)
			rules := existingByIdentity[identity]
			if len(rules) > 1 {
				preview.Rows = append(preview.Rows, billingImportScopedReviewRow(rowKey, target, rowMultiplier, "conflict", "duplicate_existing_identity", nil, band.MinInputTokens))
				continue
			}
			var existingRule *BillingModelPriceRecord
			if len(rules) == 1 {
				existingRule = &rules[0]
			}
			preview.Rows = append(preview.Rows, billingImportMatchedRow(rowKey, target, match, band, rowMultiplier, input.Policy, existingRule, catalog.FetchedAt))
		}
	}
	billingImportRejectDuplicateRowKeys(preview.Rows)
	preview.Summary = billingImportPreviewSummary(preview.Rows)
	return preview
}

func billingImportContextBandValidation(bands []BillingModelPriceImportContextBand) (string, string, []string) {
	seen := make(map[int64]struct{}, len(bands))
	for _, band := range bands {
		if band.MinInputTokens <= 0 {
			return "invalid", "invalid_context_band_boundary", nil
		}
		if _, ok := seen[band.MinInputTokens]; ok {
			return "invalid", "duplicate_context_band_identity", nil
		}
		seen[band.MinInputTokens] = struct{}{}
		if len(band.InvalidPriceFields) > 0 {
			return "invalid", "invalid_context_band_prices", band.InvalidPriceFields
		}
		if len(band.UnsupportedDimensions) > 0 {
			return "unsupported", "unsupported_context_band_dimensions", band.UnsupportedDimensions
		}
		if len(band.MissingPriceFields) > 0 {
			return "unsupported", "incomplete_context_band_prices", band.MissingPriceFields
		}
	}
	return "", "", nil
}

func billingImportMatchedRow(rowKey string, target BillingModelPriceImportTarget, match BillingModelPriceImportCatalogModel, band BillingModelPriceImportContextBand, multiplier float64, policy BillingModelPriceImportPolicy, existing *BillingModelPriceRecord, fetchedAt time.Time) BillingModelPriceImportPreviewRow {
	final := billingImportMultiplyCost(band.Cost, multiplier)
	status := "matched"
	if billingImportZeroCost(final) {
		status = "zero_cost"
	}
	row := BillingModelPriceImportPreviewRow{RowKey: rowKey, Provider: strings.ToLower(strings.TrimSpace(target.Provider)), Model: strings.TrimSpace(target.Model), ServiceTier: BillingServiceTierWildcard, MinInputTokens: band.MinInputTokens, Label: strings.TrimSpace(target.Label), Status: status, MatchedProvider: match.Provider, MatchedModel: match.Model, MatchedName: match.Name, Official: &band.Cost, Multiplier: multiplier, Reasons: []BillingModelPriceImportReason{{Code: "matched", Message: "Exact models.dev match."}}}
	if row.Label == "" {
		row.Label = row.Model
	}
	if status == "zero_cost" && !policy.IncludeZeroCost {
		row.Action = "skip"
		return row
	}
	if existing != nil {
		snapshot := billingImportRuleSnapshot(*existing)
		row.ExistingRule = &snapshot
	}
	if existing == nil {
		row.Action = "create"
	} else if policy.OverwriteMode == "sync" && existing.Source == BillingPriceSourceSync {
		row.Action = "update_sync"
	} else if policy.OverwriteMode == "all" {
		if existing.Source == BillingPriceSourceSync {
			row.Action = "update_sync"
		} else {
			row.Action = "overwrite"
		}
	} else {
		row.Action = "skip"
	}
	if row.Action == "skip" {
		return row
	}
	row.Applicable, row.SelectedDefault, row.Final = true, true, &final
	enabled := true
	if existing != nil {
		enabled = existing.Enabled
	}
	row.WriteRule = &BillingModelPriceImportWriteRule{Source: BillingPriceSourceSync, Enabled: enabled, Note: fmt.Sprintf("synced from models.dev (%s/%s) x%g @ %s", match.Provider, match.Model, multiplier, fetchedAt.UTC().Format("2006-01-02")), ServiceTier: row.ServiceTier, MinInputTokens: row.MinInputTokens}
	return row
}

func billingImportReviewRow(rowKey string, target BillingModelPriceImportTarget, multiplier float64, status, code string, candidates []string) BillingModelPriceImportPreviewRow {
	return billingImportScopedReviewRow(rowKey, target, multiplier, status, code, candidates, 0)
}

func billingImportScopedReviewRow(rowKey string, target BillingModelPriceImportTarget, multiplier float64, status, code string, candidates []string, minInputTokens int64) BillingModelPriceImportPreviewRow {
	return BillingModelPriceImportPreviewRow{RowKey: rowKey, Provider: strings.ToLower(strings.TrimSpace(target.Provider)), Model: strings.TrimSpace(target.Model), ServiceTier: BillingServiceTierWildcard, MinInputTokens: minInputTokens, Label: strings.TrimSpace(target.Label), Status: status, Action: "review", Multiplier: multiplier, Reasons: []BillingModelPriceImportReason{{Code: code, Message: strings.ReplaceAll(code, "_", " "), Candidates: candidates}}}
}

func billingImportCatalogMatches(models []BillingModelPriceImportCatalogModel, target BillingModelPriceImportTarget, aliases []BillingModelPriceImportAlias, overrides []BillingModelPriceImportMatchOverride) []BillingModelPriceImportCatalogModel {
	return newBillingImportCatalogIndex(models).matches(target, newBillingImportMatchPolicyIndex(aliases, overrides))
}

func newBillingImportCatalogIndex(models []BillingModelPriceImportCatalogModel) billingImportCatalogIndex {
	index := billingImportCatalogIndex{
		models:       models,
		exact:        make(map[string][]int, len(models)),
		providerBare: make(map[string][]int, len(models)),
		bare:         make(map[string][]int, len(models)),
	}
	for position, model := range models {
		exactKey := billingImportCatalogExactKey(model.Provider, model.Model)
		bareKey := billingImportCatalogBareKey(model.Model)
		index.exact[exactKey] = append(index.exact[exactKey], position)
		providerBareKey := billingImportCatalogProviderBareKey(model.Provider, model.Model)
		index.providerBare[providerBareKey] = append(index.providerBare[providerBareKey], position)
		index.bare[bareKey] = append(index.bare[bareKey], position)
	}
	return index
}

var defaultBillingImportAliases = []BillingModelPriceImportAlias{
	{TargetModel: "gemini-3-flash", SourceModels: []string{"gemini-3-flash-preview", "gemini-3-flash"}},
	{TargetModel: "gemini-3-flash-agent", SourceModels: []string{"gemini-3.5-flash", "gemini-3.5-flash-preview", "gemini-3-flash-preview"}},
	{TargetModel: "gemini-pro-agent", SourceModels: []string{"gemini-3.1-pro-preview", "gemini-3.1-pro", "gemini-3-pro-preview"}},
	{TargetModel: "gemini-3.5-flash-low", SourceModels: []string{"gemini-3.5-flash"}},
	{TargetModel: "gemini-3.5-flash-extra-low", SourceModels: []string{"gemini-3.5-flash"}},
	{TargetModel: "gemini-3.1-pro-low", SourceModels: []string{"gemini-3.1-pro-preview", "gemini-3.1-pro"}},
	{TargetModel: "claude-opus-4-6-thinking", SourceModels: []string{"claude-opus-4-6", "claude-opus-4.6"}},
	{TargetModel: "claude-sonnet-4-6-thinking", SourceModels: []string{"claude-sonnet-4-6", "claude-sonnet-4.6"}},
	{TargetModel: "claude-opus-5-5-high", SourceModels: []string{"claude-opus-4-6", "claude-opus-4.6"}},
	{TargetModel: "claude-sonnet-5-5-high", SourceModels: []string{"claude-sonnet-4-6", "claude-sonnet-4.6"}},
}

func newBillingImportMatchPolicyIndex(aliases []BillingModelPriceImportAlias, overrides []BillingModelPriceImportMatchOverride) billingImportMatchPolicyIndex {
	combinedAliases := make([]BillingModelPriceImportAlias, 0, len(aliases)+len(defaultBillingImportAliases))
	combinedAliases = append(combinedAliases, aliases...)
	combinedAliases = append(combinedAliases, defaultBillingImportAliases...)
	index := billingImportMatchPolicyIndex{
		aliases:           combinedAliases,
		aliasesByTarget:   make(map[string][]int, len(combinedAliases)),
		overrides:         overrides,
		overridesByTarget: make(map[string][]int, len(overrides)),
	}
	for position, alias := range aliases {
		key := billingImportCatalogFoldedValue(strings.TrimSpace(alias.TargetModel))
		index.aliasesByTarget[key] = append(index.aliasesByTarget[key], position)
	}
	for position, override := range overrides {
		key := billingImportOverrideTargetKey(override.TargetProvider, override.TargetModel)
		index.overridesByTarget[key] = append(index.overridesByTarget[key], position)
	}
	return index
}

func (index billingImportCatalogIndex) matches(target BillingModelPriceImportTarget, policy billingImportMatchPolicyIndex) []BillingModelPriceImportCatalogModel {
	if override, ok := policy.firstOverride(target); ok {
		matches := index.exact[billingImportCatalogExactKey(override.SourceProvider, override.SourceModel)]
		if len(matches) == 0 {
			return nil
		}
		return []BillingModelPriceImportCatalogModel{index.models[matches[0]]}
	}
	candidates := policy.modelCandidates(target.Model)
	preferredProviders := billingImportPreferredProviders(target.Provider, target.Model)
	for _, candidate := range candidates {
		for _, provider := range preferredProviders {
			matches := index.uniqueModels(index.providerBare[billingImportCatalogProviderBareKey(provider, candidate)])
			if len(matches) > 0 {
				return matches
			}
		}
	}
	for _, candidate := range candidates {
		firstParty := index.firstPartyBareModels(candidate)
		if len(firstParty) == 1 {
			return firstParty
		}
	}

	exact := index.sortedIndexesForExact(target.Provider, candidates)
	if len(exact) > 0 {
		return index.uniqueModels(exact)
	}
	return index.uniqueModels(index.sortedIndexesForBare(candidates))
}

func (index billingImportCatalogIndex) uniqueModels(positions []int) []BillingModelPriceImportCatalogModel {
	if len(positions) == 0 {
		return nil
	}
	if len(positions) == 1 {
		return []BillingModelPriceImportCatalogModel{index.models[positions[0]]}
	}
	models := make([]BillingModelPriceImportCatalogModel, 0, len(positions))
	for _, position := range positions {
		models = append(models, index.models[position])
	}
	return billingImportUniqueCatalogMatches(models)
}

func (index billingImportCatalogIndex) firstPartyBareModels(candidate string) []BillingModelPriceImportCatalogModel {
	positions := index.bare[billingImportCatalogBareKey(candidate)]
	models := make([]BillingModelPriceImportCatalogModel, 0, len(positions))
	for _, position := range positions {
		model := index.models[position]
		if billingImportFirstPartyProvider(model.Provider) {
			models = append(models, model)
		}
	}
	return models
}

func (index billingImportCatalogIndex) sortedIndexesForExact(provider string, candidates []string) []int {
	positions := make([]int, 0, len(candidates))
	for _, candidate := range candidates {
		positions = append(positions, index.exact[billingImportCatalogExactKey(provider, candidate)]...)
	}
	sort.Ints(positions)
	return positions
}

func (index billingImportCatalogIndex) sortedIndexesForBare(candidates []string) []int {
	positions := make([]int, 0, len(candidates))
	for _, candidate := range candidates {
		positions = append(positions, index.bare[billingImportCatalogBareKey(candidate)]...)
	}
	sort.Ints(positions)
	return positions
}

func (index billingImportMatchPolicyIndex) firstOverride(target BillingModelPriceImportTarget) (BillingModelPriceImportMatchOverride, bool) {
	positions := index.overridesByTarget[billingImportOverrideTargetKey(target.Provider, target.Model)]
	if len(positions) == 0 {
		return BillingModelPriceImportMatchOverride{}, false
	}
	return index.overrides[positions[0]], true
}

func (index billingImportMatchPolicyIndex) modelCandidates(model string) []string {
	model = strings.TrimSpace(model)
	bare := billingImportBareModel(model)
	values := []string{bare, model}
	aliasPositions := index.aliasPositionsForModel(bare, model)
	for _, position := range aliasPositions {
		values = append(values, index.aliases[position].SourceModels...)
	}
	for _, suffix := range []string{"-thinking", "-extra-low", "-high", "-medium", "-low", "-preview", "-latest", ":free"} {
		if strings.HasSuffix(strings.ToLower(bare), suffix) {
			stripped := bare[:len(bare)-len(suffix)]
			values = append(values, stripped)
			for _, aliasPos := range index.aliasPositionsForModel(stripped, stripped) {
				values = append(values, index.aliases[aliasPos].SourceModels...)
			}
		}
	}
	for _, candidate := range append([]string(nil), values...) {
		for _, variant := range billingImportVersionDelimiterVariants(candidate) {
			values = append(values, variant)
		}
	}
	for _, candidate := range append([]string(nil), values...) {
		lower := strings.ToLower(candidate)
		if !strings.HasSuffix(lower, "-preview") {
			values = append(values, candidate+"-preview")
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

func (index billingImportMatchPolicyIndex) aliasPositionsForModel(bare string, model string) []int {
	if len(index.aliases) == 0 {
		return nil
	}
	keys := []string{billingImportCatalogFoldedValue(bare)}
	if fullKey := billingImportCatalogFoldedValue(model); fullKey != keys[0] {
		keys = append(keys, fullKey)
	}
	positions := make([]int, 0)
	seen := make(map[int]struct{})
	for _, key := range keys {
		for _, position := range index.aliasesByTarget[key] {
			if _, exists := seen[position]; exists {
				continue
			}
			seen[position] = struct{}{}
			positions = append(positions, position)
		}
	}
	sort.Ints(positions)
	return positions
}

func billingImportCatalogExactKey(provider string, model string) string {
	return billingImportCatalogFoldedValue(provider) + "\x00" + billingImportCatalogFoldedValue(model)
}

func billingImportCatalogProviderBareKey(provider string, model string) string {
	return billingImportCatalogFoldedValue(provider) + "\x00" + billingImportCatalogBareKey(model)
}

func billingImportCatalogBareKey(model string) string {
	return billingImportCatalogFoldedValue(billingImportBareModel(model))
}

func billingImportOverrideTargetKey(provider string, model string) string {
	return billingImportCatalogFoldedValue(strings.TrimSpace(provider)) + "\x00" + billingImportCatalogFoldedValue(strings.TrimSpace(model))
}

func billingImportCatalogFoldedValue(value string) string {
	var builder strings.Builder
	builder.Grow(len(value))
	for _, valueRune := range value {
		foldedRune := valueRune
		for nextRune := unicode.SimpleFold(valueRune); nextRune != valueRune; nextRune = unicode.SimpleFold(nextRune) {
			if nextRune < foldedRune {
				foldedRune = nextRune
			}
		}
		builder.WriteRune(foldedRune)
	}
	return builder.String()
}

func billingImportPreferredProviders(provider, model string) []string {
	model = strings.ToLower(strings.TrimSpace(model))
	values := make([]string, 0, 6)
	switch {
	case strings.HasPrefix(model, "claude"):
		values = append(values, "anthropic")
	case strings.HasPrefix(model, "gemini"), strings.HasPrefix(model, "gemma"):
		values = append(values, "google")
	case strings.HasPrefix(model, "grok"):
		values = append(values, "xai")
	case strings.HasPrefix(model, "gpt"), strings.HasPrefix(model, "o1"), strings.HasPrefix(model, "o3"), strings.HasPrefix(model, "o4"), strings.HasPrefix(model, "codex"):
		values = append(values, "openai")
	}
	values = append(values, strings.ToLower(strings.TrimSpace(provider)), "anthropic", "google", "xai", "openai")
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func billingImportFirstPartyProvider(provider string) bool {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "anthropic", "google", "xai", "openai":
		return true
	default:
		return false
	}
}

func billingImportUniqueCatalogMatches(models []BillingModelPriceImportCatalogModel) []BillingModelPriceImportCatalogModel {
	seen := make(map[string]struct{}, len(models))
	result := make([]BillingModelPriceImportCatalogModel, 0, len(models))
	for _, model := range models {
		key := strings.ToLower(strings.TrimSpace(model.Provider)) + "\x00" + strings.ToLower(strings.TrimSpace(model.Model))
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, model)
	}
	return result
}

func billingImportRejectDuplicateRowKeys(rows []BillingModelPriceImportPreviewRow) {
	counts := make(map[string]int, len(rows))
	for _, row := range rows {
		counts[row.RowKey]++
	}
	for index := range rows {
		if counts[rows[index].RowKey] < 2 {
			continue
		}
		rows[index].Status = "conflict"
		rows[index].Action = "review"
		rows[index].Applicable = false
		rows[index].SelectedDefault = false
		rows[index].Final = nil
		rows[index].WriteRule = nil
		rows[index].Reasons = []BillingModelPriceImportReason{{Code: "duplicate_row_key", Message: "duplicate row key"}}
	}
}

func applyBillingModelPriceImportRows(ctx context.Context, tx *gorm.DB, rows []BillingModelPriceImportPreviewRow, expectedRevisions map[string]int64, now time.Time) (map[string]string, error) {
	resourceIDs := make(map[string]string, len(rows))
	creates := make([]BillingModelPriceRecord, 0, len(rows))
	updates := make([]BillingModelPriceImportPreviewRow, 0, len(rows))
	for _, row := range rows {
		if row.Final == nil || row.WriteRule == nil {
			return nil, ErrBillingImportInvalidSelection
		}
		if row.ExistingRule == nil {
			record := BillingModelPriceRecord{
				ID:                        billingID("price"),
				Provider:                  strings.ToLower(strings.TrimSpace(row.Provider)),
				Model:                     strings.TrimSpace(row.Model),
				ServiceTier:               row.ServiceTier,
				MinInputTokens:            row.MinInputTokens,
				InputPricePerMillion:      row.Final.Input,
				OutputPricePerMillion:     row.Final.Output,
				CacheReadPricePerMillion:  row.Final.CacheRead,
				CacheWritePricePerMillion: row.Final.CacheWrite,
				RequestPrice:              row.Final.Request,
				BillingExtraPrices:        row.Final.extraPrices(),
				PricingExpr:               strings.TrimSpace(row.Final.PricingExpr),
				Source:                    BillingPriceSourceSync,
				Enabled:                   row.WriteRule.Enabled,
				Note:                      row.WriteRule.Note,
				Revision:                  1,
				CreatedAt:                 now,
				UpdatedAt:                 now,
			}
			creates = append(creates, record)
			resourceIDs[row.RowKey] = record.ID
			continue
		}
		if _, ok := expectedRevisions[row.ExistingRule.ID]; !ok {
			return nil, ErrBillingImportRuleConflict
		}
		updates = append(updates, row)
		resourceIDs[row.RowKey] = row.ExistingRule.ID
	}
	for start := 0; start < len(creates); start += billingImportWriteBatchSize {
		end := min(start+billingImportWriteBatchSize, len(creates))
		if errCreate := tx.WithContext(ctx).CreateInBatches(creates[start:end], billingImportWriteBatchSize).Error; errCreate != nil {
			return nil, errCreate
		}
	}
	for start := 0; start < len(updates); start += billingImportWriteBatchSize {
		end := min(start+billingImportWriteBatchSize, len(updates))
		if errUpdate := applyBillingModelPriceImportUpdateBatch(ctx, tx, updates[start:end], expectedRevisions, now); errUpdate != nil {
			return nil, errUpdate
		}
	}
	return resourceIDs, nil
}

func applyBillingModelPriceImportUpdateBatch(ctx context.Context, tx *gorm.DB, rows []BillingModelPriceImportPreviewRow, expectedRevisions map[string]int64, now time.Time) error {
	if len(rows) == 0 {
		return nil
	}
	columns := []struct {
		name  string
		value func(BillingModelPriceImportPreviewRow) any
	}{
		{name: "input_price_per_million", value: func(row BillingModelPriceImportPreviewRow) any { return row.Final.Input }},
		{name: "output_price_per_million", value: func(row BillingModelPriceImportPreviewRow) any { return row.Final.Output }},
		{name: "cache_read_price_per_million", value: func(row BillingModelPriceImportPreviewRow) any { return row.Final.CacheRead }},
		{name: "cache_write_price_per_million", value: func(row BillingModelPriceImportPreviewRow) any { return row.Final.CacheWrite }},
		{name: "request_price", value: func(row BillingModelPriceImportPreviewRow) any { return row.Final.Request }},
		{name: "cache_write_30m_price_per_million", value: func(row BillingModelPriceImportPreviewRow) any { return row.Final.CacheWrite30m }},
		{name: "cache_write_1h_price_per_million", value: func(row BillingModelPriceImportPreviewRow) any { return row.Final.CacheWrite1h }},
		{name: "cache_write_12h_price_per_million", value: func(row BillingModelPriceImportPreviewRow) any { return row.Final.CacheWrite12h }},
		{name: "reasoning_price_per_million", value: func(row BillingModelPriceImportPreviewRow) any { return row.Final.Reasoning }},
		{name: "image_input_price_per_million", value: func(row BillingModelPriceImportPreviewRow) any { return row.Final.ImageInput }},
		{name: "image_output_price_per_million", value: func(row BillingModelPriceImportPreviewRow) any { return row.Final.ImageOutput }},
		{name: "audio_input_price_per_million", value: func(row BillingModelPriceImportPreviewRow) any { return row.Final.AudioInput }},
		{name: "audio_output_price_per_million", value: func(row BillingModelPriceImportPreviewRow) any { return row.Final.AudioOutput }},
		{name: "web_search_price_per_call", value: func(row BillingModelPriceImportPreviewRow) any { return row.Final.WebSearch }},
		{name: "image_price_per_image", value: func(row BillingModelPriceImportPreviewRow) any { return row.Final.Image }},
		{name: "pricing_expr", value: func(row BillingModelPriceImportPreviewRow) any { return strings.TrimSpace(row.Final.PricingExpr) }},
		{name: "source", value: func(BillingModelPriceImportPreviewRow) any { return BillingPriceSourceSync }},
		{name: "enabled", value: func(row BillingModelPriceImportPreviewRow) any { return row.WriteRule.Enabled }},
		{name: "note", value: func(row BillingModelPriceImportPreviewRow) any { return row.WriteRule.Note }},
	}
	var statement strings.Builder
	arguments := make([]any, 0, len(rows)*(len(columns)*2+2)+1)
	statement.WriteString("UPDATE billing_model_price SET ")
	for columnIndex, column := range columns {
		if columnIndex > 0 {
			statement.WriteString(", ")
		}
		statement.WriteString(column.name)
		statement.WriteString(" = CASE id")
		for _, row := range rows {
			statement.WriteString(" WHEN ? THEN ?")
			arguments = append(arguments, row.ExistingRule.ID, column.value(row))
		}
		statement.WriteString(" ELSE ")
		statement.WriteString(column.name)
		statement.WriteString(" END")
	}
	statement.WriteString(", revision = revision + 1, updated_at = ? WHERE deleted_at IS NULL AND (")
	arguments = append(arguments, now)
	for index, row := range rows {
		if index > 0 {
			statement.WriteString(" OR ")
		}
		expectedRevision, ok := expectedRevisions[row.ExistingRule.ID]
		if !ok {
			return ErrBillingImportRuleConflict
		}
		statement.WriteString("(id = ? AND revision = ?)")
		arguments = append(arguments, row.ExistingRule.ID, expectedRevision)
	}
	statement.WriteString(")")
	result := tx.WithContext(ctx).Exec(statement.String(), arguments...)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != int64(len(rows)) {
		return ErrBillingImportRuleConflict
	}
	return nil
}

func validateBillingModelPriceImportPreviewInput(input BillingModelPriceImportPreviewInput, catalog BillingModelPriceImportCatalog) error {
	if errInput := validateBillingModelPriceImportInput(input); errInput != nil {
		return errInput
	}
	if strings.TrimSpace(catalog.SourceURL) == "" || catalog.FetchedAt.IsZero() {
		return fmt.Errorf("a catalog snapshot is required")
	}
	return nil
}

func IsSupportedBillingModelPriceImportSource(source string) bool {
	switch strings.ToLower(strings.TrimSpace(source)) {
	case "", BillingModelPriceImportSourceModelsDev, BillingModelPriceImportSourceOpenRouter, BillingModelPriceImportSourceLiteLLM, BillingModelPriceImportSourceAuto:
		return true
	default:
		return false
	}
}

func validateBillingModelPriceImportInput(input BillingModelPriceImportPreviewInput) error {
	if !IsSupportedBillingModelPriceImportSource(input.Source) {
		return fmt.Errorf("unsupported import source")
	}
	if (!input.AutoDiscover && len(input.Targets) == 0) || len(input.Targets) > billingImportMaxTargets {
		return fmt.Errorf("import targets are required and must not exceed %d", billingImportMaxTargets)
	}
	if len(input.Policy.MultiplierRules) > billingImportMaxPolicyEntries || len(input.Policy.Aliases) > billingImportMaxPolicyEntries || len(input.Policy.RowMultipliers) > billingImportMaxPolicyEntries || len(input.MatchOverrides) > billingImportMaxPolicyEntries {
		return fmt.Errorf("import policy entries must not exceed %d", billingImportMaxPolicyEntries)
	}
	if input.Policy.OverwriteMode != "missing" && input.Policy.OverwriteMode != "sync" && input.Policy.OverwriteMode != "all" {
		return fmt.Errorf("invalid overwrite_mode")
	}
	if !billingImportValidNumber(input.Policy.DefaultMultiplier) || input.Policy.DefaultMultiplier <= 0 {
		return fmt.Errorf("default_multiplier must be positive and finite")
	}
	seen := make(map[string]struct{})
	for _, target := range input.Targets {
		if target.Provider == "" || target.Model == "" || target.ServiceTier != BillingServiceTierWildcard || target.MinInputTokens != 0 {
			return fmt.Errorf("invalid import target")
		}
		for _, scope := range target.Scopes {
			if scope != "available" && scope != "static" {
				return fmt.Errorf("invalid import target scope")
			}
		}
		key := billingImportTargetKey(target.Provider, target.Model)
		if _, ok := seen[key]; ok {
			return fmt.Errorf("duplicate import target")
		}
		seen[key] = struct{}{}
	}
	overrides := make(map[string]struct{}, len(input.MatchOverrides))
	for _, override := range input.MatchOverrides {
		key := billingImportTargetKey(override.TargetProvider, override.TargetModel)
		if override.TargetProvider == "" || override.TargetModel == "" || override.SourceProvider == "" || override.SourceModel == "" {
			return fmt.Errorf("invalid match override")
		}
		if _, ok := seen[key]; !ok {
			return fmt.Errorf("match override target is not requested")
		}
		if _, ok := overrides[key]; ok {
			return fmt.Errorf("duplicate match override")
		}
		overrides[key] = struct{}{}
	}
	for _, alias := range input.Policy.Aliases {
		if strings.TrimSpace(alias.TargetModel) == "" || len(alias.SourceModels) == 0 {
			return fmt.Errorf("invalid alias rule")
		}
		for _, sourceModel := range alias.SourceModels {
			if strings.TrimSpace(sourceModel) == "" {
				return fmt.Errorf("invalid alias rule")
			}
		}
	}
	for _, rule := range input.Policy.MultiplierRules {
		if !billingImportValidNumber(rule.Multiplier) || rule.Multiplier <= 0 || strings.TrimSpace(rule.Pattern) == "" || (!strings.EqualFold(rule.MatchMode, "prefix") && !strings.EqualFold(rule.MatchMode, "regex")) {
			return fmt.Errorf("invalid multiplier rule")
		}
		if strings.EqualFold(rule.MatchMode, "regex") {
			if _, errRegex := regexp.Compile("(?i)" + rule.Pattern); errRegex != nil {
				return fmt.Errorf("invalid multiplier rule")
			}
		}
	}
	for _, multiplier := range input.Policy.RowMultipliers {
		if !billingImportValidNumber(multiplier) || multiplier <= 0 {
			return fmt.Errorf("invalid row multiplier")
		}
	}
	return nil
}

func billingImportCompileMultiplierRules(rules []BillingModelPriceImportMultiplierRule) map[int]*regexp.Regexp {
	compiled := make(map[int]*regexp.Regexp)
	for index, rule := range rules {
		if !strings.EqualFold(strings.TrimSpace(rule.MatchMode), "regex") {
			continue
		}
		if matcher, errCompile := regexp.Compile("(?i)" + rule.Pattern); errCompile == nil {
			compiled[index] = matcher
		}
	}
	return compiled
}

func billingImportMultiplier(policy BillingModelPriceImportPolicy, compiledRules map[int]*regexp.Regexp, model, rowKey string) float64 {
	if value, ok := policy.RowMultipliers[rowKey]; ok {
		return value
	}
	for index, rule := range policy.MultiplierRules {
		if strings.EqualFold(strings.TrimSpace(rule.MatchMode), "prefix") && strings.HasPrefix(strings.ToLower(model), strings.ToLower(strings.TrimSpace(rule.Pattern))) {
			return rule.Multiplier
		}
		if strings.EqualFold(strings.TrimSpace(rule.MatchMode), "regex") {
			matcher := compiledRules[index]
			if matcher != nil && matcher.MatchString(model) {
				return rule.Multiplier
			}
		}
	}
	if policy.DefaultMultiplier == 0 {
		return 1
	}
	return policy.DefaultMultiplier
}

func (c BillingModelPriceImportCost) extraPrices() BillingExtraPrices {
	return BillingExtraPrices{
		CacheWrite30mPricePerMillion: billingImportCloneFloatPtr(c.CacheWrite30m),
		CacheWrite1hPricePerMillion:  billingImportCloneFloatPtr(c.CacheWrite1h),
		CacheWrite12hPricePerMillion: billingImportCloneFloatPtr(c.CacheWrite12h),
		ReasoningPricePerMillion:     billingImportCloneFloatPtr(c.Reasoning),
		ImageInputPricePerMillion:    billingImportCloneFloatPtr(c.ImageInput),
		ImageOutputPricePerMillion:   billingImportCloneFloatPtr(c.ImageOutput),
		AudioInputPricePerMillion:    billingImportCloneFloatPtr(c.AudioInput),
		AudioOutputPricePerMillion:   billingImportCloneFloatPtr(c.AudioOutput),
		WebSearchPricePerCall:        billingImportCloneFloatPtr(c.WebSearch),
		ImagePricePerImage:           billingImportCloneFloatPtr(c.Image),
	}
}

func billingImportCloneFloatPtr(v *float64) *float64 {
	if v == nil {
		return nil
	}
	copied := *v
	return &copied
}

func billingImportScaleFloatPtr(v *float64, multiplier float64) *float64 {
	if v == nil {
		return nil
	}
	scaled := billingRoundAmount(*v * multiplier)
	return &scaled
}

func billingImportMultiplyCost(cost BillingModelPriceImportCost, multiplier float64) BillingModelPriceImportCost {
	expr := strings.TrimSpace(cost.PricingExpr)
	if expr != "" && multiplier != 1 {
		body := strings.TrimSpace(strings.TrimPrefix(expr, "v1:"))
		expr = fmt.Sprintf("(%s) * %g", body, multiplier)
	}
	return BillingModelPriceImportCost{
		Input:         cost.Input * multiplier,
		Output:        cost.Output * multiplier,
		CacheRead:     cost.CacheRead * multiplier,
		CacheWrite:    cost.CacheWrite * multiplier,
		Request:       cost.Request * multiplier,
		CacheWrite30m: billingImportScaleFloatPtr(cost.CacheWrite30m, multiplier),
		CacheWrite1h:  billingImportScaleFloatPtr(cost.CacheWrite1h, multiplier),
		CacheWrite12h: billingImportScaleFloatPtr(cost.CacheWrite12h, multiplier),
		Reasoning:     billingImportScaleFloatPtr(cost.Reasoning, multiplier),
		ImageInput:    billingImportScaleFloatPtr(cost.ImageInput, multiplier),
		ImageOutput:   billingImportScaleFloatPtr(cost.ImageOutput, multiplier),
		AudioInput:    billingImportScaleFloatPtr(cost.AudioInput, multiplier),
		AudioOutput:   billingImportScaleFloatPtr(cost.AudioOutput, multiplier),
		WebSearch:     billingImportScaleFloatPtr(cost.WebSearch, multiplier),
		Image:         billingImportScaleFloatPtr(cost.Image, multiplier),
		PricingExpr:   expr,
	}
}

func billingImportZeroCost(cost BillingModelPriceImportCost) bool {
	if cost.Input != 0 || cost.Output != 0 || cost.CacheRead != 0 || cost.CacheWrite != 0 || cost.Request != 0 || strings.TrimSpace(cost.PricingExpr) != "" {
		return false
	}
	for _, ptr := range []*float64{cost.CacheWrite30m, cost.CacheWrite1h, cost.CacheWrite12h, cost.Reasoning, cost.ImageInput, cost.ImageOutput, cost.AudioInput, cost.AudioOutput, cost.WebSearch, cost.Image} {
		if ptr != nil && *ptr != 0 {
			return false
		}
	}
	return true
}

func billingImportVersionDelimiterVariants(model string) []string {
	var out []string
	runes := []rune(model)
	// Convert digit.digit <-> digit-digit for version numbers like claude-opus-4.6 <-> claude-opus-4-6.
	dotToDash := make([]rune, len(runes))
	dashToDot := make([]rune, len(runes))
	changedDot, changedDash := false, false
	for i, r := range runes {
		dotToDash[i] = r
		dashToDot[i] = r
		if i > 0 && i+1 < len(runes) && unicode.IsDigit(runes[i-1]) && unicode.IsDigit(runes[i+1]) {
			if r == '.' {
				dotToDash[i] = '-'
				changedDot = true
			} else if r == '-' && (i+2 == len(runes) || !unicode.IsDigit(runes[i+2])) {
				dashToDot[i] = '.'
				changedDash = true
			}
		}
	}
	if changedDot {
		out = append(out, string(dotToDash))
	}
	if changedDash {
		out = append(out, string(dashToDot))
	}
	return out
}

func (r *Repository) DiscoverBillingImportTargets(ctx context.Context) ([]BillingModelPriceImportTarget, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	return r.discoverBillingImportTargets(contextOrBackground(ctx), db)
}

func (r *Repository) discoverBillingImportTargets(ctx context.Context, db *gorm.DB) ([]BillingModelPriceImportTarget, error) {
	type pair struct {
		Provider string
		Model    string
	}
	var usagePairs []pair
	if errUsage := db.WithContext(ctx).Model(&UsageRecord{}).
		Select(" DISTINCT LOWER(TRIM(provider)) AS provider, TRIM(model) AS model").
		Where("provider <> '' AND model <> ''").
		Limit(billingImportMaxTargets).
		Scan(&usagePairs).Error; errUsage != nil {
		return nil, errUsage
	}
	var pricePairs []pair
	if errPrices := db.WithContext(ctx).Model(&BillingModelPriceRecord{}).
		Select("DISTINCT LOWER(TRIM(provider)) AS provider, TRIM(model) AS model").
		Where("provider <> '' AND provider <> '*' AND model <> '' AND model NOT LIKE '%*'").
		Limit(billingImportMaxTargets).
		Scan(&pricePairs).Error; errPrices != nil {
		return nil, errPrices
	}
	seen := make(map[string]struct{})
	out := make([]BillingModelPriceImportTarget, 0, len(usagePairs)+len(pricePairs))
	for _, item := range append(usagePairs, pricePairs...) {
		p := strings.ToLower(strings.TrimSpace(item.Provider))
		m := strings.TrimSpace(item.Model)
		if p == "" || m == "" || strings.Contains(p, "*") || strings.Contains(m, "*") {
			continue
		}
		key := billingImportTargetKey(p, m)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, BillingModelPriceImportTarget{
			Provider:       p,
			Model:          m,
			ServiceTier:    BillingServiceTierWildcard,
			MinInputTokens: 0,
			Label:          m,
		})
		if len(out) >= billingImportMaxTargets {
			break
		}
	}
	return out, nil
}
func billingImportRuleIdentity(provider, model, tier string, min int64) string {
	return strings.ToLower(strings.TrimSpace(provider)) + "\x00" + strings.TrimSpace(model) + "\x00" + normalizeBillingPriceServiceTier(tier) + "\x00" + fmt.Sprintf("%d", min)
}
func billingImportTargetKey(provider, model string) string {
	return strings.ToLower(strings.TrimSpace(provider)) + "::" + strings.TrimSpace(model)
}
func billingImportBareModel(model string) string {
	trimmed := strings.TrimSpace(model)
	if index := strings.LastIndex(trimmed, "/"); index >= 0 {
		return trimmed[index+1:]
	}
	return trimmed
}
func billingImportRuleSnapshot(rule BillingModelPriceRecord) BillingModelPriceImportRuleSnapshot {
	return BillingModelPriceImportRuleSnapshot{ID: rule.ID, Provider: rule.Provider, Model: rule.Model, ServiceTier: rule.ServiceTier, MinInputTokens: rule.MinInputTokens, InputPricePerMillion: rule.InputPricePerMillion, OutputPricePerMillion: rule.OutputPricePerMillion, CacheReadPricePerMillion: rule.CacheReadPricePerMillion, CacheWritePricePerMillion: rule.CacheWritePricePerMillion, RequestPrice: rule.RequestPrice, Source: rule.Source, Enabled: rule.Enabled, Note: rule.Note, Revision: strconv.FormatInt(rule.Revision, 10)}
}
func billingImportPreviewSummary(rows []BillingModelPriceImportPreviewRow) BillingModelPriceImportSummary {
	var summary BillingModelPriceImportSummary
	for _, row := range rows {
		incrementBillingImportSummary(&summary, row.Action, row.Status)
	}
	return summary
}
func incrementBillingImportSummary(summary *BillingModelPriceImportSummary, action, status string) {
	summary.Total++
	switch action {
	case "create":
		summary.Creates++
	case "update_sync":
		summary.UpdatesSync++
	case "overwrite":
		summary.Overwrites++
	case "skip":
		summary.Skips++
	default:
		summary.Reviews++
	}
	if status == "conflict" {
		summary.Conflicts++
	}
}
func normalizedBillingImportSelectedKeys(keys []string) []string {
	values := append([]string(nil), keys...)
	for index := range values {
		values[index] = strings.TrimSpace(values[index])
	}
	sort.Strings(values)
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" || (len(result) > 0 && result[len(result)-1] == value) {
			return nil
		}
		result = append(result, value)
	}
	return result
}
func billingImportHash(values ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(values, "\x00")))
	return hex.EncodeToString(sum[:])
}

func (r *Repository) cleanupBillingModelPriceImports(ctx context.Context, db *gorm.DB, now time.Time) error {
	if db == nil {
		return fmt.Errorf("database connection is nil")
	}
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if errDelete := tx.Where("expires_at < ?", now.Add(-billingImportPreviewRetention)).Delete(&BillingModelPriceImportPreviewRecord{}).Error; errDelete != nil {
			return errDelete
		}
		if errDelete := tx.Where("created_at < ?", now.Add(-billingImportOperationRetention)).Delete(&BillingModelPriceImportOperationRecord{}).Error; errDelete != nil {
			return errDelete
		}
		return nil
	})
}
func unmarshalBillingImportOperation(raw JSONB, target *BillingModelPriceImportOperation) error {
	if err := json.Unmarshal(raw, target); err != nil {
		return fmt.Errorf("decode billing import operation: %w", err)
	}
	return nil
}
func billingImportValidNumber(value float64) bool { return !math.IsNaN(value) && !math.IsInf(value, 0) }

func migrateBillingImportIndexes(db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("database connection is nil")
	}
	const currentIndex = "idx_billing_model_price_import_operation_selection_hash"
	const legacyIndex = "idx_billing_model_price_import_operation_records_selection_hash"
	indexes, errIndexes := db.Migrator().GetIndexes(&BillingModelPriceImportOperationRecord{})
	if errIndexes != nil {
		return errIndexes
	}
	createCurrent := true
	for _, index := range indexes {
		switch index.Name() {
		case currentIndex:
			unique, known := index.Unique()
			if known && !unique {
				createCurrent = false
				continue
			}
			if errDrop := db.Migrator().DropIndex(&BillingModelPriceImportOperationRecord{}, currentIndex); errDrop != nil {
				return errDrop
			}
		case legacyIndex:
			if errDrop := db.Migrator().DropIndex(&BillingModelPriceImportOperationRecord{}, legacyIndex); errDrop != nil {
				return errDrop
			}
		}
	}
	if !createCurrent {
		return nil
	}
	return db.Migrator().CreateIndex(&BillingModelPriceImportOperationRecord{}, currentIndex)
}
